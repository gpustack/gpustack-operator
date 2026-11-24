package nvidia

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerNVIDIA

// hbmMemoryBusWidthBits is the memory bus width (bits) at or above which a GPU is HBM rather
// than GDDR. Data-center HBM stacks are >=1024-bit (e.g. A30 3072, A100/H100 5120) while GDDR
// parts top out at 512-bit (GDDR7 included), so the threshold separates them with a wide margin.
const hbmMemoryBusWidthBits = 1024

// gddr7ComputeCapabilityMajor is the first generation whose GDDR memory keeps ECC inside the
// DRAM die (GDDR7 on RTX Blackwell) instead of trading ~1/16 of the framebuffer for ECC-mode
// parity, which the restore in DetectAccelerator compensates on earlier GDDR generations.
const gddr7ComputeCapabilityMajor = 12

var _PciVendor string

func init() {
	pciID := nodefeature.GetPciVendorID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type cachedAccelerator struct {
	group device.DevicesGroup
	accel device.Accelerator
}

type cachedMetrics struct {
	id     string
	memory uint64
}

type nvidia struct {
	initMu      sync.Mutex
	initialized bool
	nvml        *nvml.NVML
	logger      klog.Logger

	// fabricsMu guards fabrics. Nothing calls the detect pass concurrently today -- the preflight
	// subcommand builds its own detector in its own process, and the daemon's loop runs one pass at
	// a time -- so this lock is insurance against a second caller rather than a race being fixed.
	// It is held here instead of being left out because the map outlives a pass, which makes "one
	// caller" a property of the callers rather than of this type.
	fabricsMu sync.Mutex
	// fabrics remembers the registration each accelerator last answered the fabric query with, keyed
	// by its UUID. See rememberFabric for why a failed read reuses it instead of reporting none.
	fabrics map[string]fabricInfo

	cacheMu         sync.Mutex
	cachedDetected  map[int]cachedAccelerator
	cachedMonitored map[int]cachedMetrics
}

// New creates a new nvidia device interface and initializes the NVML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &nvidia{
		nvml:            nvml.New(binding.WithLogger(logger)),
		logger:          logger,
		fabrics:         make(map[string]fabricInfo),
		cachedDetected:  make(map[int]cachedAccelerator),
		cachedMonitored: make(map[int]cachedMetrics),
	}
}

// The three methods below implement device.Detector, in its declaration order.

func (in *nvidia) Name() string {
	return Manufacturer
}

func (in *nvidia) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect nvidia devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no nvidia pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize NVML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("nvidia driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.nvml.DeviceGetCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get nvidia device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get nvidia device count")
			return nil, err
		}
		in.logger.Info("no nvidia devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no nvidia devices found")
		return nil, nil
	}

	drVer, _ := in.nvml.SystemGetDriverVersion()
	rtVer, _ := in.nvml.SystemGetCudaDriverVersion()

	grpList := device.DevicesGroupList{}
	nextDetected := make(map[int]cachedAccelerator, cnt)
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	queriedCount := 0
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.nvml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.restoreCachedDetected(i, unqueryableSlots, &grpList)
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.restoreCachedDetected(i, unqueryableSlots, &grpList)
			continue
		}

		var pciBusId string
		{
			pciHandler := dev.GetPciInfoV()
			pciInfo, ret := pciHandler.V2()
			if !ret.IsSuccess() {
				var v1Ret nvml.Return
				pciInfo, v1Ret = pciHandler.V1()
				ret = selectVersionedReturn(ret, v1Ret)
				if !ret.IsSuccess() {
					if err := handleCardQueryError(logger, i, "get device PCI info", ret, &lastUnqueryableErr); err != nil {
						return nil, err
					}
					in.restoreCachedDetected(i, unqueryableSlots, &grpList)
					continue
				}
			}
			pciBusId = pciInfo.GetBusId()
		}
		pciDev := pciDevs[pciBusId]

		name, ret := dev.GetName()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device name", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.restoreCachedDetected(i, unqueryableSlots, &grpList)
			continue
		}

		// Read before the memory block: the ECC capacity rule below gates on the generation, and
		// the group fields after the block reuse the same reading.
		ccMajor, ccMinor, ccRet := dev.GetCudaComputeCapability()

		var (
			memory          uint64
			memoryUnhealthy bool
		)
		{
			memHandler := dev.GetMemoryInfoV()
			memInfo, ret := memHandler.V2()
			if !ret.IsSuccess() {
				var v1Ret nvml.Return
				memInfo, v1Ret = memHandler.V1()
				ret = selectVersionedReturn(ret, v1Ret)
				if !ret.IsSuccess() {
					if err := handleCardQueryError(logger, i, "get device memory info", ret, &lastUnqueryableErr); err != nil {
						return nil, err
					}
					in.restoreCachedDetected(i, unqueryableSlots, &grpList)
					continue
				}
			}
			// NVML reports the total with the current ECC mode already applied.
			memory = device.ConvertBytesToMiB(memInfo.Total)

			// Enabling ECC mode costs user-visible capacity on GDDR parts that keep ECC parity
			// outside the framebuffer: since Turing, professional GDDR cards give ~1/16 of memory
			// to parity when the mode is on, so restore the nominal size (x16/15) there. Two
			// boundaries keep the rule honest. HBM never restores: its ECC lives in
			// hardware-reserved regions and NVML always reports full capacity, and data-center HBM
			// stacks are >=1024-bit (A30 3072, A100/H100 5120) while GDDR parts top out at 512-bit.
			// And GDDR7 never restores: its ECC is built into the DRAM die (NVIDIA RTX Blackwell
			// GPU Architecture whitepaper v1.1), so an ECC-enabled RTX PRO 6000 Blackwell Server
			// Edition reports its 96 GB nominal as 97887 MiB total -- measured, driver 580.173.02
			// -- which the restore would inflate to 104412 MiB. RTX Blackwell is compute capability
			// 12.0, so the restore applies only below that major. A reading the detector cannot
			// classify -- bus width, ECC mode, or compute capability unreadable -- keeps the
			// driver-reported total rather than a capacity the driver did not report.
			if bw, ret := dev.GetMemoryBusWidth(); ret.IsSuccess() && bw > 0 && bw < hbmMemoryBusWidthBits {
				if cur, _, ret := dev.GetEccMode(); ret.IsSuccess() && cur == nvml.FEATURE_ENABLED {
					if ccRet.IsSuccess() && ccMajor < gddr7ComputeCapabilityMajor {
						memory = memory * 16 / 15
					}
				}
			}
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
			return grp.Name == name && grp.Memory == memory
		})
		if grpIndex == -1 {
			// New group.
			cores, _ := dev.GetNumGpuCores()
			family := getFamilyFromComputeCapability(ccMajor, ccMinor)
			grpList = append(grpList, device.DevicesGroup{
				ID:                device.ConstructGroupID(Manufacturer, name, memory),
				Manufacturer:      Manufacturer,
				Name:              name,
				Memory:            memory,
				Cores:             cores,
				DriverVersion:     drVer,
				RuntimeVersion:    stringifyRuntimeVersion(rtVer),
				Family:            family,
				ComputeCapability: stringifyComputeCapability(ccMajor, ccMinor),
			})
			grpIndex = len(grpList) - 1
		}

		// The recorded minor number is what an accelerator's device node is NAMED after on the
		// manufacturers that build one from it (/dev/dri/card<minor> and friends), so it is left ABSENT
		// when the driver
		// cannot answer for it rather than substituted by the enumeration index: a substituted value is
		// indistinguishable from a real minor at every later consumer, which would then build a device
		// path out of a guess. No allocator of this vendor reads the field today; publishing a number
		// the driver never gave is what the next consumer would inherit.
		var physicalIndexes []uint32
		if minorNum, ret := dev.GetMinorNumber(); ret.IsSuccess() {
			physicalIndexes = []uint32{minorNum}
		} else {
			logger.V(3).Info("recorded no minor number for a card whose driver could not answer for it; "+
				"a consumer that names a device node after that number cannot address this card",
				"card", uuid, "reason", ret.Error())
		}

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)
		topo.Fabric = in.readFabric(dev, uuid, logger)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy

			// Logical (software) and physical (MIG) slicing are mutually exclusive per accelerator,
			// keyed on the current MIG mode: an accelerator that is currently MIG-enabled is
			// hard-partitioned and reports only its physical MIG profiles. Every other accelerator —
			// MIG off, MIG unsupported (GetMigMode returns not-supported on non-MIG accelerators), or
			// the mode unreadable — reports the group's logical-slice capability.
			//
			// A pending-mode transition is not partitioned yet and is re-detected after the
			// administrator's reset + DeviceManager restart. This runs per accelerator, fixing the
			// old placeholder's first-accelerator-only-seed defect.
			//
			// A mode the driver could not read is treated as MIG off, as it always has been, but
			// it is no longer treated in silence: since an accelerator in the mode reports ONLY its MIG
			// profiles, an administrator who enabled MIG would otherwise see an accelerator quietly
			// advertising the logical-slice capability it cannot serve. A driver answering that
			// the mode is unsupported is not a failure — that is an accelerator which does not do MIG.
			migCurrent, _, migRet := dev.GetMigMode()
			if !migRet.IsSuccess() && !driverReportsAbsent(migRet) {
				logger.Error(migRet, "could not read a card's MIG mode, so it is reported as "+
					"MIG off; if MIG is in fact enabled, the card advertises logical slicing it cannot serve",
					"card", uuid)
			}
			if migCurrent == nvml.DEVICE_MIG_ENABLE {
				profiles := detectMigProfiles(dev)
				status.PhysicalSliced = device.AcceleratorPhysicalSliced{
					Profiles: profiles,
					Count:    maxProfileCount(profiles),
				}
			} else {
				// Logical (software) slicing via HAMi-core ld.preload; the per-accelerator slice count is
				// capped at the max CUDA user processes a GPU serves (128, Volta+).
				status.LogicalSliced = device.AcceleratorLogicalSliced{
					Count:                     128,
					CoresPercentageOvercommit: true,
				}
			}
		}

		accel := device.Accelerator{
			ID:              uuid,
			Index:           uint32(i),
			PhysicalIndexes: physicalIndexes,
			Topology:        topo,
			Status:          status,
		}
		grpList[grpIndex].Accelerators = append(grpList[grpIndex].Accelerators, accel)
		nextDetected[i] = cachedAccelerator{
			group: cloneGroupMetadata(grpList[grpIndex]),
			accel: *accel.DeepCopy(),
		}
		nextMonitored[i] = cachedMetrics{
			id:     uuid,
			memory: memory,
		}
		queriedCount++
	}

	if queriedCount == 0 {
		if lastUnqueryableErr != nil {
			return nil, lastUnqueryableErr
		}
		in.clearCache()
		return nil, nil
	}

	in.commitDetectedCache(nextDetected, nextMonitored, unqueryableSlots)
	device.SetGroupSlicedDetails(grpList)

	return grpList, nil
}

func (in *nvidia) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor nvidia devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no nvidia pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize NVML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("nvidia driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.nvml.DeviceGetCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get nvidia device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get nvidia device count")
			return nil, err
		}
		in.logger.Info("no nvidia devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no nvidia devices found")
		return nil, nil
	}

	metricsGrp := device.MetricsGroup{
		Manufacturer: Manufacturer,
		Timestamp:    time.Now(),
		Accelerators: make([]device.AcceleratorMetrics, 0, cnt),
	}
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	queriedCount := 0
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.nvml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.restoreCachedMonitored(i, unqueryableSlots, &metricsGrp.Accelerators)
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.restoreCachedMonitored(i, unqueryableSlots, &metricsGrp.Accelerators)
			continue
		}

		var (
			memory            uint64
			memoryUsage       uint64
			memoryUtilization uint32
			memoryUnhealthy   bool
		)
		{
			memHandler := dev.GetMemoryInfoV()
			memInfo, ret := memHandler.V2()
			if !ret.IsSuccess() {
				var v1Ret nvml.Return
				memInfo, v1Ret = memHandler.V1()
				ret = selectVersionedReturn(ret, v1Ret)
				if !ret.IsSuccess() {
					if err := handleCardQueryError(logger, i, "get device memory info", ret, &lastUnqueryableErr); err != nil {
						return nil, err
					}
					in.restoreCachedMonitored(i, unqueryableSlots, &metricsGrp.Accelerators)
					continue
				}
			}

			memory = device.ConvertBytesToMiB(memInfo.Total)
			memoryUsage = device.ConvertBytesToMiB(memInfo.Used)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		var (
			coresUtilization uint32
			temperature      uint32
			powerUsage       uint32
		)
		{
			gpmSupportHandler := dev.GpmQueryDeviceSupportV()
			gpmSupport, ret := gpmSupportHandler.V1()
			if ret.IsSuccess() && gpmSupport.IsSupportedDevice != 0 {
				gpmMetricsHandler := dev.GpmMetricsGetV()
				gpmMetrics, ret := gpmMetricsHandler.V1(100*time.Millisecond, nvml.GPM_METRIC_SM_UTIL)
				if !ret.IsSuccess() {
					logger.V(4).Error(ret, "failed to get device cores utilization with GPM, fallback")
					coresUtilization = coresUtilizationOf(logger, dev)
				} else {
					coresUtilization = uint32(gpmMetrics[0].Value)
				}
			} else {
				logger.Info("cannot get device cores utilization with GPM, fallback")
				coresUtilization = coresUtilizationOf(logger, dev)
			}

			temperature, ret = dev.GetTemperature()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device temperature")
			}

			powerUsage, ret = dev.GetPowerUsage()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device power usage")
			} else {
				powerUsage /= 1_000 // Convert from milliwatt to watt.
			}
		}

		metricsGrp.Accelerators = append(
			metricsGrp.Accelerators,
			device.AcceleratorMetrics{
				ID:                uuid,
				Memory:            memory,
				MemoryUsage:       memoryUsage,
				MemoryUtilization: memoryUtilization,
				CoresUtilization:  coresUtilization,
				Temperature:       temperature,
				PowerUsage:        powerUsage,
				Unhealthy:         memoryUnhealthy,
			},
		)
		nextMonitored[i] = cachedMetrics{
			id:     uuid,
			memory: memory,
		}
		queriedCount++
	}

	if queriedCount == 0 {
		if lastUnqueryableErr != nil {
			return nil, lastUnqueryableErr
		}
		in.clearCache()
		return nil, nil
	}

	in.commitMonitoredCache(nextMonitored, unqueryableSlots)

	return device.MetricsGroupList{metricsGrp}, nil
}

func cloneGroupMetadata(grp device.DevicesGroup) device.DevicesGroup {
	grp.Accelerators = nil
	return *grp.DeepCopy()
}

func (in *nvidia) getCachedDetected(slot int) (cachedAccelerator, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedDetected[slot]
	return cached, ok
}

func (in *nvidia) getCachedMonitored(slot int) (cachedMetrics, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedMonitored[slot]
	return cached, ok
}

func (in *nvidia) restoreCachedDetected(
	slot int, unqueryableSlots map[int]struct{}, grpList *device.DevicesGroupList,
) {
	unqueryableSlots[slot] = struct{}{}
	cached, ok := in.getCachedDetected(slot)
	if !ok {
		return
	}
	grpIndex := slices.IndexFunc(*grpList, func(grp device.DevicesGroup) bool {
		return grp.Name == cached.group.Name && grp.Memory == cached.group.Memory
	})
	if grpIndex == -1 {
		*grpList = append(*grpList, cloneGroupMetadata(cached.group))
		grpIndex = len(*grpList) - 1
	}
	accel := *cached.accel.DeepCopy()
	accel.Status.Unhealthy = true
	(*grpList)[grpIndex].Accelerators = append((*grpList)[grpIndex].Accelerators, accel)
}

func (in *nvidia) restoreCachedMonitored(
	slot int, unqueryableSlots map[int]struct{}, accels *[]device.AcceleratorMetrics,
) {
	unqueryableSlots[slot] = struct{}{}
	cached, ok := in.getCachedMonitored(slot)
	if !ok {
		return
	}
	*accels = append(*accels, device.AcceleratorMetrics{
		ID:        cached.id,
		Memory:    cached.memory,
		Unhealthy: true,
	})
}

func (in *nvidia) commitDetectedCache(
	nextDetected map[int]cachedAccelerator,
	nextMonitored map[int]cachedMetrics,
	unqueryableSlots map[int]struct{},
) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	for slot := range unqueryableSlots {
		if _, exists := nextDetected[slot]; !exists {
			if cached, ok := in.cachedDetected[slot]; ok {
				nextDetected[slot] = cached
			}
		}
		if _, exists := nextMonitored[slot]; !exists {
			if cached, ok := in.cachedMonitored[slot]; ok {
				nextMonitored[slot] = cached
			}
		}
	}
	in.cachedDetected = nextDetected
	in.cachedMonitored = nextMonitored
}

func (in *nvidia) commitMonitoredCache(
	nextMonitored map[int]cachedMetrics,
	unqueryableSlots map[int]struct{},
) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	for slot := range unqueryableSlots {
		if _, exists := nextMonitored[slot]; !exists {
			if cached, ok := in.cachedMonitored[slot]; ok {
				nextMonitored[slot] = cached
			}
		}
	}
	in.cachedMonitored = nextMonitored

	for slot := range in.cachedDetected {
		if _, ok := nextMonitored[slot]; ok {
			continue
		}
		delete(in.cachedDetected, slot)
	}
}

func (in *nvidia) clearCache() {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	clear(in.cachedDetected)
	clear(in.cachedMonitored)
}

func isDeviceUnhealthy(dev nvml.Device) bool {
	memEccDramUE, ret := dev.GetMemoryErrorCounter(
		nvml.MEMORY_ERROR_TYPE_UNCORRECTED,
		nvml.VOLATILE_ECC,
		nvml.MEMORY_LOCATION_DRAM,
	)
	if evaluateDeviceUnhealthy(memEccDramUE, ret) {
		return true
	}
	action, actionRet := readRecoveryAction(dev)
	return evaluateRecoveryActionUnhealthy(actionRet, action)
}

// evaluateDeviceUnhealthy reports whether the vendor's uncorrectable ECC count makes the
// accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(eccErrors uint64, ret nvml.Return) bool {
	return ret.IsSuccess() && eccErrors > 0
}

// readRecoveryAction reads the driver's GPU recovery action for a device through the field-values
// interface. The deprecated reset-status fields answer the same question and are deliberately
// left unread.
func readRecoveryAction(dev nvml.Device) (nvml.FieldValue, nvml.Return) {
	values := []nvml.FieldValue{{FieldId: nvml.FI_DEV_GET_GPU_RECOVERY_ACTION}}
	ret := dev.GetFieldValues(values)
	return values[0], ret
}

// evaluateRecoveryActionUnhealthy reports whether the driver's recovery action makes the
// accelerator unhealthy. Every action but "none" asks for an intervention -- a GPU reset, a
// node reboot, or draining the card -- so any of them is a fault.
//
// A query the driver could not answer, in whole or for this one field, is not a fault of the
// card and reads no verdict; nor does a value that arrived in a type this reader does not
// decode. A field the driver never answered carries the zero value, whose type is DOUBLE, so
// the type gate is what keeps that reading from decoding as a "none" verdict. None of these
// can erase a verdict the caller already returned: the ECC fault is checked first, and this
// runs only when it read healthy.
func evaluateRecoveryActionUnhealthy(fieldRet nvml.Return, value nvml.FieldValue) bool {
	if !fieldRet.IsSuccess() || nvml.Return(value.NvmlReturn) != nvml.SUCCESS {
		return false
	}
	switch nvml.ValueType(value.ValueType) {
	case nvml.VALUE_TYPE_UNSIGNED_INT, nvml.VALUE_TYPE_UNSIGNED_LONG, nvml.VALUE_TYPE_UNSIGNED_LONG_LONG:
	default:
		return false
	}
	// The union was written in host byte order, and every platform an NVIDIA driver serves is
	// little-endian, so the low word carries the action for all three accepted types.
	action := nvml.DeviceGpuRecoveryAction(binary.LittleEndian.Uint32(value.Value[:4]))
	return action != nvml.GPU_RECOVERY_ACTION_NONE
}

// coresUtilizationOf reads an accelerator's whole-device cores utilization.
//
// NOT_SUPPORTED is the driver's own answer about the device rather than a failure, and a
// partitioning accelerator is the case in point: with MIG mode on, compute is accounted per
// partition and no whole-device aggregate exists. That is a property of the device, so it holds for
// every round the detector runs, and reporting it as an error would raise a failure, once a round
// forever, over a device that is working as designed. It is reported at the detector's own verbosity
// instead.
//
// Either way the reading is zero: this figure has no absent form, and a consumer after a
// partitioning accelerator's real usage reads each partition's own entry rather than the card's.
func coresUtilizationOf(logger klog.Logger, dev nvml.Device) uint32 {
	utilInfo, ret := dev.GetUtilizationRates()
	switch {
	case ret.IsSuccess():
		return utilInfo.Gpu
	case ret == nvml.ERROR_NOT_SUPPORTED:
		logger.Info("device-level cores utilization is unsupported")
	default:
		logger.Error(ret, "failed to get device cores utilization")
	}
	return 0
}

func (in *nvidia) init() {
	_ = in.ensureInit()
}

func (in *nvidia) ensureInit() nvml.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.initialized {
		return nvml.SUCCESS
	}

	ret := in.nvml.Init()
	if ret.IsSuccess() || ret == nvml.ERROR_ALREADY_INITIALIZED {
		in.initialized = true
		return nvml.SUCCESS
	}

	in.logger.Error(ret, "failed to initialize NVML library")
	return ret
}

// isUnqueryableDeviceError reports whether a non-success NVML return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret nvml.Return) bool {
	switch ret {
	case nvml.ERROR_GPU_IS_LOST,
		nvml.ERROR_UNKNOWN,
		nvml.ERROR_RESET_REQUIRED,
		nvml.ERROR_INSUFFICIENT_POWER,
		nvml.ERROR_IRQ_ISSUE:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library on a node without detected PCI
// devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret nvml.Return, noPciCheck bool, pciDevCount int) error {
	if ret.IsSuccess() {
		return nil
	}
	if ret.IsAPIUnavailable() && (noPciCheck || pciDevCount == 0) {
		return nil
	}
	return fmt.Errorf("failed to %s: %w", op, ret)
}

// selectVersionedReturn resolves the return code after a V2 query fails and falls back to V1.
// When V1 also fails, V2's return is kept unless V2 itself was unavailable or unsupported.
func selectVersionedReturn(v2Ret, v1Ret nvml.Return) nvml.Return {
	if v1Ret.IsSuccess() {
		return v1Ret
	}
	if v2Ret == nvml.ERROR_FUNCTION_NOT_FOUND ||
		v2Ret == nvml.ERROR_NOT_SUPPORTED ||
		v2Ret == nvml.ERROR_ARGUMENT_VERSION_MISMATCH {
		return v1Ret
	}
	return v2Ret
}

// handleCardQueryError classifies a fatal per-card query error. Card-specific unqueryable errors
// are logged, recorded in lastUnqueryableErr, and skipped; other errors fail the pass immediately.
func handleCardQueryError(
	logger klog.Logger, index int, op string, ret nvml.Return, lastUnqueryableErr *error,
) error {
	err := fmt.Errorf("failed to %s at index %d: %w", op, index, ret)
	if isUnqueryableDeviceError(ret) {
		logger.Error(ret, "skipping unqueryable device", "operation", op)
		*lastUnqueryableErr = err
		return nil
	}
	logger.Error(ret, "failed to "+op)
	return err
}

func stringifyRuntimeVersion(rtVer int32) string {
	if rtVer <= 0 {
		return ""
	}
	major := rtVer / 1000
	minor := (rtVer % 1000) / 10
	return strconv.Itoa(int(major)) + "." + strconv.Itoa(int(minor))
}

func stringifyComputeCapability(ccMajor, ccMinor int32) string {
	if ccMajor == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d", ccMajor, ccMinor)
}

func getFamilyFromComputeCapability(ccMajor, ccMinor int32) string {
	switch ccMajor {
	case 1:
		return "Tesla"
	case 2:
		return "Fermi"
	case 3:
		return "Kepler"
	case 5:
		return "Maxwell"
	case 6:
		return "Pascal"
	case 7:
		if ccMinor < 5 {
			return "Volta"
		}
		return "Turing"
	case 8:
		if ccMinor < 9 {
			return "Ampere"
		}
		return "Ada-Lovelace"
	case 9:
		return "Hopper"
	case 10, 12:
		return "Blackwell"
	}
	return ""
}
