package iluvatar

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/ixml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerIluvatar

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

type iluvatar struct {
	initMu          sync.Mutex
	initialized     bool
	cacheMu         sync.Mutex
	cachedDetected  map[int]cachedAccelerator
	cachedMonitored map[int]cachedMetrics
	ixml            *ixml.IXML
	logger          klog.Logger
}

// New creates a new iluvatar device interface and initializes the IXML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &iluvatar{
		ixml:   ixml.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

// Name, DetectAccelerator and MonitorAccelerator implement device.Detector.
func (in *iluvatar) Name() string {
	return Manufacturer
}

func (in *iluvatar) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect iluvatar devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.clearCache()
		in.logger.Info("no iluvatar pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize IXML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("iluvatar driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.ixml.DeviceGetCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get iluvatar device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get iluvatar device count")
			return nil, err
		}
		in.logger.Info("no iluvatar devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no iluvatar devices found")
		return nil, nil
	}

	drVer, _ := in.ixml.SystemGetDriverVersion()
	rtVer, _ := in.ixml.SystemGetCudaDriverVersion()

	grpList := device.DevicesGroupList{}
	nextDetected := make(map[int]cachedAccelerator, cnt)
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	queriedCount := 0
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.ixml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainCachedDetected(i, unqueryableSlots, &grpList)
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainCachedDetected(i, unqueryableSlots, &grpList)
			continue
		}

		var pciBusId string
		{
			pciInfo, ret := dev.GetPciInfo()
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device PCI info", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.retainCachedDetected(i, unqueryableSlots, &grpList)
				continue
			}
			pciBusId = pciInfo.GetBusId()
		}
		pciDev := pciDevs[pciBusId]

		name, ret := dev.GetName()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device name", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainCachedDetected(i, unqueryableSlots, &grpList)
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
				var v1Ret ixml.Return
				memInfo, v1Ret = memHandler.V1()
				ret = selectVersionedReturn(ret, v1Ret)
				if !ret.IsSuccess() {
					if err := handleCardQueryError(logger, i, "get device memory info", ret, &lastUnqueryableErr); err != nil {
						return nil, err
					}
					in.retainCachedDetected(i, unqueryableSlots, &grpList)
					continue
				}
			}
			memory = device.ConvertBytesToMiB(memInfo.Total)
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		ccMajor, ccMinor, _ := dev.GetCudaComputeCapability()

		grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
			return grp.Name == name && grp.Memory == memory
		})
		if grpIndex == -1 {
			// New group.
			grpList = append(grpList, device.DevicesGroup{
				ID:                device.ConstructGroupID(Manufacturer, name, memory),
				Manufacturer:      Manufacturer,
				Name:              name,
				Memory:            memory,
				DriverVersion:     drVer,
				RuntimeVersion:    stringifyRuntimeVersion(rtVer),
				ComputeCapability: stringifyComputeCapability(ccMajor, ccMinor),
			})
			grpIndex = len(grpList) - 1
		}

		// The recorded minor number is what an accelerator's device node is NAMED after on the
		// manufacturers that build one from it. "card" survives here only as the Linux DRM node name
		// /dev/dri/card<minor>, which this repository does not own; the number is left ABSENT when the
		// driver cannot answer for it rather than substituted by the enumeration index: a substituted
		// value is indistinguishable from a real minor at every later consumer, which would then build
		// a device path out of a guess. No allocator of this manufacturer reads the field today;
		// publishing a number the driver never gave is what the next consumer would inherit.
		var physicalIndexes []uint32
		if minorNum, ret := dev.GetMinorNumber(); ret.IsSuccess() {
			physicalIndexes = []uint32{minorNum}
		} else {
			logger.V(3).Info("recorded no minor number for a card whose driver could not answer for it; "+
				"a consumer that names a device node after that number cannot address this card",
				"card", uuid, "reason", ret.Error())
		}

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy
			// Logical (software) slicing via HAMi-core ld.preload; Iluvatar's native vcuda-core model
			// is 1%-granular with 100 == a whole accelerator, so the per-accelerator slice ceiling
			// is 100.
			status.LogicalSliced = device.AcceleratorLogicalSliced{
				Count:                     100,
				CoresPercentageOvercommit: true,
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

		grpCopy := *grpList[grpIndex].DeepCopy()
		grpCopy.Accelerators = nil
		nextDetected[i] = cachedAccelerator{
			group: grpCopy,
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

	in.updateDetectedCache(nextDetected, nextMonitored, unqueryableSlots)
	device.SetGroupSlicedDetails(grpList)

	return grpList, nil
}

func (in *iluvatar) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor iluvatar devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.clearCache()
		in.logger.Info("no iluvatar pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize IXML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("iluvatar driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.ixml.DeviceGetCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get iluvatar device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get iluvatar device count")
			return nil, err
		}
		in.logger.Info("no iluvatar devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no iluvatar devices found")
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

		dev, ret := in.ixml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainCachedMonitored(i, unqueryableSlots, &metricsGrp)
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainCachedMonitored(i, unqueryableSlots, &metricsGrp)
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
				var v1Ret ixml.Return
				memInfo, v1Ret = memHandler.V1()
				ret = selectVersionedReturn(ret, v1Ret)
				if !ret.IsSuccess() {
					if err := handleCardQueryError(logger, i, "get device memory info", ret, &lastUnqueryableErr); err != nil {
						return nil, err
					}
					in.retainCachedMonitored(i, unqueryableSlots, &metricsGrp)
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
				gpmMetrics, ret := gpmMetricsHandler.V1(100*time.Millisecond, ixml.GPM_METRIC_SM_UTIL)
				if !ret.IsSuccess() {
					logger.V(4).Error(ret, "failed to get device cores utilization with GPM, fallback")
					utilInfo, ret := dev.GetUtilizationRates()
					if !ret.IsSuccess() {
						logger.V(3).Error(ret, "failed to get device cores utilization")
					} else {
						coresUtilization = utilInfo.Gpu
					}
				} else {
					coresUtilization = uint32(gpmMetrics[0].Value)
				}
			} else {
				logger.Info("cannot get device cores utilization with GPM, fallback")
				utilInfo, ret := dev.GetUtilizationRates()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device cores utilization")
				} else {
					coresUtilization = utilInfo.Gpu
				}
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

	in.updateMonitoredCache(nextMonitored, unqueryableSlots)

	return device.MetricsGroupList{metricsGrp}, nil
}

func (in *iluvatar) getCachedDetected(slot int) (cachedAccelerator, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedDetected[slot]
	if !ok {
		return cachedAccelerator{}, false
	}
	grpCopy := *cached.group.DeepCopy()
	grpCopy.Accelerators = nil
	return cachedAccelerator{
		group: grpCopy,
		accel: *cached.accel.DeepCopy(),
	}, true
}

func (in *iluvatar) getCachedMonitored(slot int) (cachedMetrics, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedMonitored[slot]
	return cached, ok
}

func (in *iluvatar) retainCachedDetected(
	slot int, unqueryableSlots map[int]struct{}, grpList *device.DevicesGroupList,
) {
	unqueryableSlots[slot] = struct{}{}
	cached, ok := in.getCachedDetected(slot)
	if !ok {
		return
	}

	cached.accel.Status.Unhealthy = true

	grpIndex := slices.IndexFunc(*grpList, func(grp device.DevicesGroup) bool {
		return grp.Name == cached.group.Name && grp.Memory == cached.group.Memory
	})
	if grpIndex == -1 {
		*grpList = append(*grpList, cached.group)
		grpIndex = len(*grpList) - 1
	}
	(*grpList)[grpIndex].Accelerators = append((*grpList)[grpIndex].Accelerators, cached.accel)
}

func (in *iluvatar) retainCachedMonitored(
	slot int, unqueryableSlots map[int]struct{}, metricsGrp *device.MetricsGroup,
) {
	unqueryableSlots[slot] = struct{}{}
	cached, ok := in.getCachedMonitored(slot)
	if !ok {
		return
	}

	metricsGrp.Accelerators = append(
		metricsGrp.Accelerators,
		device.AcceleratorMetrics{
			ID:        cached.id,
			Memory:    cached.memory,
			Unhealthy: true,
		},
	)
}

func (in *iluvatar) clearCache() {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	in.cachedDetected = nil
	in.cachedMonitored = nil
}

func (in *iluvatar) updateDetectedCache(
	nextDetected map[int]cachedAccelerator,
	nextMonitored map[int]cachedMetrics,
	unqueryableSlots map[int]struct{},
) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	for slot := range unqueryableSlots {
		if _, ok := nextDetected[slot]; !ok {
			if prev, ok := in.cachedDetected[slot]; ok {
				nextDetected[slot] = prev
			}
		}
		if _, ok := nextMonitored[slot]; !ok {
			if prev, ok := in.cachedMonitored[slot]; ok {
				nextMonitored[slot] = prev
			}
		}
	}
	in.cachedDetected = nextDetected
	in.cachedMonitored = nextMonitored
}

func (in *iluvatar) updateMonitoredCache(
	nextMonitored map[int]cachedMetrics,
	unqueryableSlots map[int]struct{},
) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	for slot := range unqueryableSlots {
		if _, ok := nextMonitored[slot]; !ok {
			if prev, ok := in.cachedMonitored[slot]; ok {
				nextMonitored[slot] = prev
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

func isDeviceUnhealthy(dev ixml.Device) bool {
	memHealth, ret := dev.GetHealth()
	return evaluateDeviceUnhealthy(memHealth, ret)
}

// evaluateDeviceUnhealthy reports whether the vendor's health state makes the accelerator
// unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(health uint64, ret ixml.Return) bool {
	return ret.IsSuccess() && health != ixml.HealthOK
}

func (in *iluvatar) init() {
	_ = in.ensureInit()
}

func (in *iluvatar) ensureInit() ixml.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.initialized {
		return ixml.SUCCESS
	}

	ret := in.ixml.Init()
	if ret.IsSuccess() || ret == ixml.ERROR_ALREADY_INITIALIZED {
		in.initialized = true
		return ixml.SUCCESS
	}

	in.logger.Error(ret, "failed to initialize IXML library")
	return ret
}

// isUnqueryableDeviceError reports whether a non-success IXML return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret ixml.Return) bool {
	switch ret {
	case ixml.ERROR_GPU_IS_LOST,
		ixml.ERROR_UNKNOWN,
		ixml.ERROR_RESET_REQUIRED,
		ixml.ERROR_INSUFFICIENT_POWER,
		ixml.ERROR_IRQ_ISSUE:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library on a node without detected PCI
// devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret ixml.Return, noPciCheck bool, pciDevCount int) error {
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
func selectVersionedReturn(v2Ret, v1Ret ixml.Return) ixml.Return {
	if v1Ret.IsSuccess() {
		return v1Ret
	}
	if v2Ret == ixml.ERROR_FUNCTION_NOT_FOUND ||
		v2Ret == ixml.ERROR_NOT_SUPPORTED ||
		v2Ret == ixml.ERROR_ARGUMENT_VERSION_MISMATCH {
		return v1Ret
	}
	return v2Ret
}

// handleCardQueryError classifies a fatal per-card query error. Card-specific unqueryable errors
// are logged, recorded in lastUnqueryableErr, and skipped; other errors fail the pass immediately.
func handleCardQueryError(
	logger klog.Logger, index int, op string, ret ixml.Return, lastUnqueryableErr *error,
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
