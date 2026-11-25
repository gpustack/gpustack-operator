package thead

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/hgml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerTHead

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

type thead struct {
	initMu      sync.Mutex
	initialized bool
	hgml        *hgml.HGML
	logger      klog.Logger

	cacheMu         sync.Mutex
	cachedDetected  map[int]cachedAccelerator
	cachedMonitored map[int]cachedMetrics
}

// New creates a new thead device interface and initializes the HGML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &thead{
		hgml:            hgml.New(binding.WithLogger(logger)),
		logger:          logger,
		cachedDetected:  make(map[int]cachedAccelerator),
		cachedMonitored: make(map[int]cachedMetrics),
	}
}

// The three methods below implement device.Detector, in its declaration order.

func (in *thead) Name() string {
	return Manufacturer
}

func (in *thead) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect thead devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no thead pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize HGML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("thead driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.hgml.DeviceGetCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get thead device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get thead device count")
			return nil, err
		}
		in.logger.Info("no thead devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no thead devices found")
		return nil, nil
	}

	drVer, _ := in.hgml.SystemGetDriverVersion()
	rtVer, _ := in.hgml.SystemGetHggcDriverVersion()

	grpList := device.DevicesGroupList{}
	liveSlots := make(map[int]struct{}, cnt)
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	queriedCount := 0
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.hgml.DeviceGetHandleByIndex(i)
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
				var v1Ret hgml.Return
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

		var (
			memory          uint64
			memoryUnhealthy bool
		)
		{
			memHandler := dev.GetMemoryInfoV()
			memInfo, ret := memHandler.V2()
			if !ret.IsSuccess() {
				var v1Ret hgml.Return
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
			memory = device.ConvertBytesToMiB(memInfo.Total)
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy

			// Logical (software) and physical (partition) slicing are mutually exclusive per
			// accelerator. Unqueryable partitioning mode or profile query errors isolate the
			// affected card and preserve cached unhealthy state, while other non-absent errors
			// fail the pass rather than misclassifying a partitioned card.
			migCurrent, _, migRet := dev.GetMigMode()
			if !migRet.IsSuccess() && !isMigModeAbsent(migRet) {
				if err := handleCardQueryError(logger, i, "get device MIG mode", migRet, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.restoreCachedDetected(i, unqueryableSlots, &grpList)
				continue
			}
			if migRet.IsSuccess() && migCurrent == hgml.DEVICE_MIG_ENABLE {
				profiles, profileRet := detectMigProfiles(dev, logger)
				if !profileRet.IsSuccess() {
					if err := handleCardQueryError(logger, i, "detect device MIG profiles", profileRet, &lastUnqueryableErr); err != nil {
						return nil, err
					}
					in.restoreCachedDetected(i, unqueryableSlots, &grpList)
					continue
				}
				status.PhysicalSliced = physicalSliced(profiles)
			} else {
				// Logical (software) slicing via the preload pair this image stages. Unlike
				// CUDA, hgml.h documents no per-accelerator user-process ceiling, so the count is a
				// deliberately loose device-plugin token pool — the binding constraint on a
				// slice request is its memory budget — set to the same 128 the NVIDIA detector
				// publishes. Overcommit is true because the compute cap is a duty-cycle window
				// over wall time; memory is the exact dimension and the flag does not touch it.
				status.LogicalSliced = device.AcceleratorLogicalSliced{
					Count:                     128,
					CoresPercentageOvercommit: true,
				}
			}
		}

		ccMajor, ccMinor, _ := dev.GetHggcComputeCapability()

		grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
			return grp.Name == name && grp.Memory == memory
		})
		if grpIndex == -1 {
			// New group.
			cores, _ := dev.GetNumGpuCores()
			grpList = append(grpList, device.DevicesGroup{
				ID:                device.ConstructGroupID(Manufacturer, name, memory),
				Manufacturer:      Manufacturer,
				Name:              name,
				Memory:            memory,
				Cores:             cores,
				DriverVersion:     drVer,
				RuntimeVersion:    stringifyRuntimeVersion(rtVer),
				ComputeCapability: stringifyComputeCapability(ccMajor, ccMinor),
			})
			grpIndex = len(grpList) - 1
		}

		// The recorded minor number is what PROVES that a device node addresses the accelerator this
		// record describes. The node is named after the card ordinal, not after this number, and the
		// allocator stats the node the ordinal names and compares its kernel character-device minor
		// against this value. So it is left ABSENT when the driver cannot answer for it rather than
		// substituted by the enumeration index: a substituted value would make a wrong ordinal look
		// proven, and a container would be handed whichever accelerator happened to answer to it. An
		// absent record is refusable; a plausible wrong one is not.
		var physicalIndexes []uint32
		if minorNum, ret := dev.GetMinorNumber(); ret.IsSuccess() {
			physicalIndexes = []uint32{minorNum}
		} else {
			// Behind a verbosity level, like every other per-accelerator driver read that fails in
			// this loop: the condition is static and the loop is periodic, so at default verbosity
			// this would repeat for the life of the node without telling an operator anything the
			// moment it matters. What it costs is reported where it bites instead — an accelerator
			// without this number is refused by EVERY allocation path, whole-accelerator and
			// partition alike, and that refusal carries the same reason to the Pod that asked for it.
			logger.V(3).Info("recorded no minor number for a card whose driver could not answer for it; "+
				"every allocation on it will be refused rather than addressed by an ordinal nothing "+
				"can prove",
				"card", uuid, "reason", ret.Error())
		}

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

		accel := device.Accelerator{
			ID:              uuid,
			Index:           uint32(i),
			PhysicalIndexes: physicalIndexes,
			Topology:        topo,
			Status:          status,
		}
		grpList[grpIndex].Accelerators = append(grpList[grpIndex].Accelerators, accel)
		liveSlots[i] = struct{}{}
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

	// Accelerators of one group must agree on what a profile name means before the aggregation
	// below merges them by name, so any name they disagree on is withheld from the whole group.
	for i := range grpList {
		for _, reason := range rejectDivergentGroupProfiles(&grpList[i]) {
			in.logger.Info("withheld a partition profile the group's cards disagree on",
				"group", grpList[i].ID, "reason", reason)
		}
	}

	in.commitDetectedCache(snapshotLiveDetected(grpList, liveSlots), nextMonitored, unqueryableSlots)

	device.SetGroupSlicedDetails(grpList)

	return grpList, nil
}

func (in *thead) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor thead devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no thead pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize HGML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("thead driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.hgml.DeviceGetCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get thead device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get thead device count")
			return nil, err
		}
		in.logger.Info("no thead devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no thead devices found")
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

		dev, ret := in.hgml.DeviceGetHandleByIndex(i)
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
				var v1Ret hgml.Return
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
				gpmMetrics, ret := gpmMetricsHandler.V1(100*time.Millisecond, hgml.GPM_METRIC_SM_UTIL)
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

func snapshotLiveDetected(
	grpList device.DevicesGroupList, liveSlots map[int]struct{},
) map[int]cachedAccelerator {
	nextDetected := make(map[int]cachedAccelerator, len(liveSlots))
	for _, grp := range grpList {
		for _, accel := range grp.Accelerators {
			slot := int(accel.Index)
			if _, isLive := liveSlots[slot]; !isLive {
				continue
			}
			nextDetected[slot] = cachedAccelerator{
				group: cloneGroupMetadata(grp),
				accel: *accel.DeepCopy(),
			}
		}
	}
	return nextDetected
}

func (in *thead) getCachedDetected(slot int) (cachedAccelerator, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedDetected[slot]
	return cached, ok
}

func (in *thead) getCachedMonitored(slot int) (cachedMetrics, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedMonitored[slot]
	return cached, ok
}

func (in *thead) restoreCachedDetected(
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

func (in *thead) restoreCachedMonitored(
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

func (in *thead) commitDetectedCache(
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

func (in *thead) commitMonitoredCache(
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

func (in *thead) clearCache() {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	clear(in.cachedDetected)
	clear(in.cachedMonitored)
}

func isDeviceUnhealthy(dev hgml.Device) bool {
	memEccDramUE, ret := dev.GetMemoryErrorCounter(
		hgml.MEMORY_ERROR_TYPE_UNCORRECTED,
		hgml.VOLATILE_ECC,
		hgml.MEMORY_LOCATION_DRAM,
	)
	return evaluateDeviceUnhealthy(memEccDramUE, ret)
}

// evaluateDeviceUnhealthy reports whether the vendor's uncorrectable ECC count makes the
// accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(eccErrors uint64, ret hgml.Return) bool {
	return ret.IsSuccess() && eccErrors > 0
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
func coresUtilizationOf(logger klog.Logger, dev hgml.Device) uint32 {
	utilInfo, ret := dev.GetUtilizationRates()
	switch {
	case ret.IsSuccess():
		return utilInfo.Gpu
	case ret == hgml.ERROR_NOT_SUPPORTED:
		logger.Info("device-level cores utilization is unsupported")
	default:
		logger.Error(ret, "failed to get device cores utilization")
	}
	return 0
}

func (in *thead) init() {
	_ = in.ensureInit()
}

func (in *thead) ensureInit() hgml.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.initialized {
		return hgml.SUCCESS
	}

	ret := in.hgml.Init()
	if ret.IsSuccess() || ret == hgml.ERROR_ALREADY_INITIALIZED {
		in.initialized = true
		return hgml.SUCCESS
	}

	in.logger.Error(ret, "failed to initialize HGML library")
	return ret
}

// isUnqueryableDeviceError reports whether a non-success HGML return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret hgml.Return) bool {
	switch ret {
	case hgml.ERROR_GPU_IS_LOST,
		hgml.ERROR_UNKNOWN,
		hgml.ERROR_RESET_REQUIRED,
		hgml.ERROR_INSUFFICIENT_POWER,
		hgml.ERROR_IRQ_ISSUE,
		hgml.ERROR_GPU_NOT_FOUND:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library on a node without detected PCI
// devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret hgml.Return, noPciCheck bool, pciDevCount int) error {
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
func selectVersionedReturn(v2Ret, v1Ret hgml.Return) hgml.Return {
	if v1Ret.IsSuccess() {
		return v1Ret
	}
	if v2Ret == hgml.ERROR_FUNCTION_NOT_FOUND ||
		v2Ret == hgml.ERROR_NOT_SUPPORTED ||
		v2Ret == hgml.ERROR_ARGUMENT_VERSION_MISMATCH {
		return v1Ret
	}
	return v2Ret
}

// handleCardQueryError classifies a fatal per-card query error. Card-specific unqueryable errors
// are logged, recorded in lastUnqueryableErr, and skipped; other errors fail the pass immediately.
func handleCardQueryError(
	logger klog.Logger, index int, op string, ret hgml.Return, lastUnqueryableErr *error,
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
