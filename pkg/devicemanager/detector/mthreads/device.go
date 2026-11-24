package mthreads

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/mtml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerMThreads

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

type mthreads struct {
	initMu          sync.Mutex
	initialized     bool
	cacheMu         sync.Mutex
	cachedDetected  map[int]cachedAccelerator
	cachedMonitored map[int]cachedMetrics
	mtml            *mtml.MTML
	logger          klog.Logger
}

// New creates a new mthreads device interface and initializes the MTML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &mthreads{
		mtml:   mtml.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

// Name, DetectAccelerator and MonitorAccelerator implement device.Detector.
func (in *mthreads) Name() string {
	return Manufacturer
}

func (in *mthreads) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect mthreads devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.clearCache()
		in.logger.Info("no mthreads pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize MTML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("mthreads driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.mtml.CountDevice()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get mthreads device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get mthreads device count")
			return nil, err
		}
		in.logger.Info("no mthreads devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no mthreads devices found")
		return nil, nil
	}

	drVer, _ := in.mtml.SystemGetDriverVersion()

	grpList := device.DevicesGroupList{}
	nextDetected := make(map[int]cachedAccelerator, cnt)
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	queriedCount := 0
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		onQueryErr := func(op string, ret mtml.Return) error {
			if err := handleCardQueryError(logger, i, op, ret, &lastUnqueryableErr); err != nil {
				return err
			}
			in.retainCachedDetected(i, unqueryableSlots, &grpList)
			return nil
		}

		dev, ret := in.mtml.InitDeviceByIndex(i)
		if !ret.IsSuccess() {
			if err := onQueryErr("get device handle", ret); err != nil {
				return nil, err
			}
			continue
		}

		if err := func() error {
			defer func() {
				_ = dev.Free()
			}()

			prop, ret := dev.GetProperty()
			if !ret.IsSuccess() {
				return onQueryErr("get device property", ret)
			}
			if mtml.VirtRole(prop.VirtCap) == mtml.VIRT_ROLE_GUEST_VIRTDEVICE {
				logger.Info("skipping virtual device")
				return nil
			}

			uuid, ret := dev.GetUUID()
			if !ret.IsSuccess() {
				return onQueryErr("get device UUID", ret)
			}

			var pciBusId string
			{
				pciInfo, ret := dev.GetPciInfo()
				if !ret.IsSuccess() {
					return onQueryErr("get device PCI info", ret)
				}
				pciBusId = pciInfo.GetBusId()
			}
			pciDev := pciDevs[pciBusId]

			name, ret := dev.GetName()
			if !ret.IsSuccess() {
				return onQueryErr("get device name", ret)
			}

			var (
				memory          uint64
				memoryUnhealthy bool
			)
			{
				memHandler, ret := dev.InitMemory()
				if !ret.IsSuccess() {
					return onQueryErr("initialize memory handler", ret)
				}
				defer func() {
					_ = memHandler.Free()
				}()

				memTotal, ret := memHandler.GetTotal()
				if !ret.IsSuccess() {
					return onQueryErr("get device memory total", ret)
				}
				memory = device.ConvertBytesToMiB(memTotal)
				memoryUnhealthy = isDeviceUnhealthy(memHandler)
			}

			grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
				return grp.Name == name && grp.Memory == memory
			})
			if grpIndex == -1 {
				// New group.
				cores, _ := dev.CountGpuCores()
				grpList = append(grpList, device.DevicesGroup{
					ID:            device.ConstructGroupID(Manufacturer, name, memory),
					Manufacturer:  Manufacturer,
					Name:          name,
					Memory:        memory,
					Cores:         cores,
					DriverVersion: drVer,
				})
				grpIndex = len(grpList) - 1
			}

			physicalIndexes := []uint32{uint32(i)}

			topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

			var status device.AcceleratorStatus
			{
				status.Unhealthy = memoryUnhealthy
				// GPU logical slicing via the sGPU kmod + MTHREADS_QOS_* env; the per-accelerator
				// slice count is capped at 16. Compute is a relative weight (not a hard cap), so it is
				// not overcommitted.
				status.LogicalSliced = device.AcceleratorLogicalSliced{
					Count: 16,
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
			return nil
		}(); err != nil {
			return nil, err
		}
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

func (in *mthreads) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor mthreads devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.clearCache()
		in.logger.Info("no mthreads pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize MTML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("mthreads driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.mtml.CountDevice()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get mthreads device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get mthreads device count")
			return nil, err
		}
		in.logger.Info("no mthreads devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no mthreads devices found")
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

		onQueryErr := func(op string, ret mtml.Return) error {
			if err := handleCardQueryError(logger, i, op, ret, &lastUnqueryableErr); err != nil {
				return err
			}
			in.retainCachedMonitored(i, unqueryableSlots, &metricsGrp)
			return nil
		}

		dev, ret := in.mtml.InitDeviceByIndex(i)
		if !ret.IsSuccess() {
			if err := onQueryErr("get device handle", ret); err != nil {
				return nil, err
			}
			continue
		}

		if err := func() error {
			defer func() {
				_ = dev.Free()
			}()

			prop, ret := dev.GetProperty()
			if !ret.IsSuccess() {
				return onQueryErr("get device property", ret)
			}
			if mtml.VirtRole(prop.VirtCap) == mtml.VIRT_ROLE_GUEST_VIRTDEVICE {
				logger.Info("skipping virtual device")
				return nil
			}

			uuid, ret := dev.GetUUID()
			if !ret.IsSuccess() {
				return onQueryErr("get device UUID", ret)
			}

			var (
				memory            uint64
				memoryUsage       uint64
				memoryUtilization uint32
				memoryUnhealthy   bool
			)
			{
				memHandler, ret := dev.InitMemory()
				if !ret.IsSuccess() {
					return onQueryErr("initialize memory handler", ret)
				}
				defer func() {
					_ = memHandler.Free()
				}()

				memTotal, ret := memHandler.GetTotal()
				if !ret.IsSuccess() {
					return onQueryErr("get device memory total", ret)
				}
				memory = device.ConvertBytesToMiB(memTotal)

				memUsed, ret := memHandler.GetUsed()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device memory used")
				} else {
					memoryUsage = device.ConvertBytesToMiB(memUsed)
				}

				memoryUtilization = device.CalculateUtilization(memoryUsage, memory)
				memoryUnhealthy = isDeviceUnhealthy(memHandler)
			}

			var (
				coresUtilization uint32
				temperature      uint32
				powerUsage       uint32
			)
			{
				gpuHandler, ret := dev.InitGpu()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to initialize gpu handler")
				} else {
					defer func() {
						_ = gpuHandler.Free()
					}()

					coresUtilization, ret = gpuHandler.GetUtilization()
					if !ret.IsSuccess() {
						logger.V(3).Error(ret, "failed to get device cores utilization")
					}

					temperature, ret = gpuHandler.GetTemperature()
					if !ret.IsSuccess() {
						logger.V(3).Error(ret, "failed to get device temperature")
					}
				}

				powerUsage, ret = dev.GetPowerUsage()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device power usage")
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
			return nil
		}(); err != nil {
			return nil, err
		}
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

func (in *mthreads) getCachedDetected(slot int) (cachedAccelerator, bool) {
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

func (in *mthreads) getCachedMonitored(slot int) (cachedMetrics, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedMonitored[slot]
	return cached, ok
}

func (in *mthreads) retainCachedDetected(
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

func (in *mthreads) retainCachedMonitored(
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

func (in *mthreads) clearCache() {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	in.cachedDetected = nil
	in.cachedMonitored = nil
}

func (in *mthreads) updateDetectedCache(
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

func (in *mthreads) updateMonitoredCache(
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

func isDeviceUnhealthy(memHandler mtml.MemoryHandler) bool {
	memEccDramUE, ret := memHandler.GetEccErrorCounter(
		mtml.MEMORY_ERROR_TYPE_UNCORRECTED,
		mtml.VOLATILE_ECC,
		mtml.MEMORY_LOCATION_DRAM,
	)
	return evaluateDeviceUnhealthy(memEccDramUE, ret)
}

// evaluateDeviceUnhealthy reports whether the vendor's uncorrectable ECC count makes the
// accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(eccErrors uint64, ret mtml.Return) bool {
	return ret.IsSuccess() && eccErrors > 0
}

func (in *mthreads) init() {
	_ = in.ensureInit()
}

func (in *mthreads) ensureInit() mtml.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.initialized {
		return mtml.SUCCESS
	}

	ret := in.mtml.Init()
	if ret.IsSuccess() {
		in.initialized = true
		return mtml.SUCCESS
	}

	in.logger.Error(ret, "failed to initialize MTML library")
	return ret
}

// isUnqueryableDeviceError reports whether a non-success MTML return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret mtml.Return) bool {
	switch ret {
	case mtml.ERROR_DRIVER_FAILURE,
		mtml.ERROR_NOT_FOUND,
		mtml.ERROR_RESOURCE_IS_BUSY,
		mtml.ERROR_UNKNOWN:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library on a node without detected PCI
// devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret mtml.Return, noPciCheck bool, pciDevCount int) error {
	if ret.IsSuccess() {
		return nil
	}
	if ret.IsAPIUnavailable() && (noPciCheck || pciDevCount == 0) {
		return nil
	}
	return fmt.Errorf("failed to %s: %w", op, ret)
}

// handleCardQueryError classifies a fatal per-card query error. Card-specific unqueryable errors
// are logged, recorded in lastUnqueryableErr, and skipped; other errors fail the pass immediately.
func handleCardQueryError(
	logger klog.Logger, index int, op string, ret mtml.Return, lastUnqueryableErr *error,
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
