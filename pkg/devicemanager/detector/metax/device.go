package metax

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/mxsml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

const Manufacturer = nodefeature.ManufacturerMetaX

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

type metax struct {
	initMu          sync.Mutex
	initialized     bool
	cacheMu         sync.Mutex
	cachedDetected  map[int]cachedAccelerator
	cachedMonitored map[int]cachedMetrics
	mxsml           *mxsml.MXSML
	logger          klog.Logger
}

// New creates a new metax device interface and initializes the MXSML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &metax{
		mxsml:  mxsml.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

// Name, DetectAccelerator and MonitorAccelerator implement device.Detector.
func (in *metax) Name() string {
	return Manufacturer
}

func (in *metax) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect metax devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.clearCache()
		in.logger.Info("no metax PCI devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize MXSML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("metax driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt := in.mxsml.GetDeviceCount()
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no metax devices found")
		return nil, nil
	}

	drVer, _ := in.mxsml.GetDriverVersion()
	rtVer, _ := in.mxsml.GetMacaVersion()

	grpList := device.DevicesGroupList{}
	nextDetected := make(map[int]cachedAccelerator, cnt)
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	queriedCount := 0
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.mxsml.GetDeviceHandleByIndex(i)

		info, ret := dev.GetInfo()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device info", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainCachedDetected(i, unqueryableSlots, &grpList)
			continue
		}
		if mxsml.DeviceVirtualizationMode(info.Mode) == mxsml.Virtualization_Mode_Vf {
			logger.Info("skipping virtual function device")
			continue
		}

		uuid := info.GetUUID()

		pciBusId := info.GetBusId()
		pciDev := pciDevs[pciBusId]

		name := info.GetDeviceName()

		var (
			memory          uint64
			memoryUnhealthy bool
		)
		{
			memInfo, ret := dev.GetMemoryInfo()
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device memory info", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.retainCachedDetected(i, unqueryableSlots, &grpList)
				continue
			}
			memory = device.ConvertKiBToMiB(memInfo.VramTotal)
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
			return grp.Name == name && grp.Memory == memory
		})
		if grpIndex == -1 {
			// New group.
			family := guessFamilyFromDeviceName(name)
			grpList = append(grpList, device.DevicesGroup{
				ID:             device.ConstructGroupID(Manufacturer, name, memory),
				Manufacturer:   Manufacturer,
				Name:           name,
				Memory:         memory,
				DriverVersion:  drVer,
				RuntimeVersion: device.NormalizeVersion(rtVer),
				Family:         family,
			})
			grpIndex = len(grpList) - 1
		}

		physicalIndexes := getPhysicalIndexes(pciBusId)

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy
			// GPU logical slicing via sysfs sgpu/create (+ METAX_SGPUS); the per-accelerator slice
			// count is capped at 16 (DefaultDevCnt, operationally confirmed). Compute is spatially
			// partitioned, not overcommitted.
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

func (in *metax) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor metax devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.clearCache()
		in.logger.Info("no metax pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize MXSML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("metax driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt := in.mxsml.GetDeviceCount()
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no metax devices found")
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

		dev := in.mxsml.GetDeviceHandleByIndex(i)

		info, ret := dev.GetInfo()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device info", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainCachedMonitored(i, unqueryableSlots, &metricsGrp)
			continue
		}
		if mxsml.DeviceVirtualizationMode(info.Mode) == mxsml.Virtualization_Mode_Vf {
			logger.Info("skipping virtual function device")
			continue
		}

		uuid := info.GetUUID()

		var (
			memory            uint64
			memoryUsage       uint64
			memoryUtilization uint32
			memoryUnhealthy   bool
		)
		{
			memInfo, ret := dev.GetMemoryInfo()
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device memory info", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.retainCachedMonitored(i, unqueryableSlots, &metricsGrp)
				continue
			}

			memory = device.ConvertKiBToMiB(memInfo.VramTotal)
			memoryUsage = device.ConvertKiBToMiB(memInfo.VramUse)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		var (
			coresUtilization uint32
			temperature      uint32
			powerUsage       uint32
		)
		{
			ipUsage, ret := dev.GetIpUsage(mxsml.Usage_Xcore)
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device core utilization")
			} else {
				coresUtilization = uint32(ipUsage)
			}

			tempInfo, ret := dev.GetTemperatureInfo(mxsml.Temperature_Hotspot)
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device temperature")
			} else {
				temperature = uint32(tempInfo)
			}

			powerInfo, ret := dev.GetBoardPowerInfo()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device power usage")
			} else {
				powerUsage = slicex.Sum(powerInfo, func(info mxsml.BoardWayElectricInfo) uint32 {
					return info.Power / 1_000 // Convert mW to W.
				})
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

func (in *metax) getCachedDetected(slot int) (cachedAccelerator, bool) {
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

func (in *metax) getCachedMonitored(slot int) (cachedMetrics, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedMonitored[slot]
	return cached, ok
}

func (in *metax) retainCachedDetected(
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

func (in *metax) retainCachedMonitored(
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

func (in *metax) clearCache() {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	in.cachedDetected = nil
	in.cachedMonitored = nil
}

func (in *metax) updateDetectedCache(
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

func (in *metax) updateMonitoredCache(
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

func isDeviceUnhealthy(dev mxsml.Device) bool {
	memEcc, ret := dev.GetTotalEccErrors()
	return evaluateDeviceUnhealthy(memEcc, ret)
}

// evaluateDeviceUnhealthy reports whether the vendor's uncorrectable DRAM ECC count makes the
// accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(eccErrors mxsml.EccErrorCount, ret mxsml.Return) bool {
	return ret.IsSuccess() && eccErrors.DramUE > 0
}

func (in *metax) init() {
	_ = in.ensureInit()
}

func (in *metax) ensureInit() mxsml.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.initialized {
		return mxsml.Success
	}

	ret := in.mxsml.Init()
	if ret.IsSuccess() {
		in.initialized = true
		return mxsml.Success
	}

	in.logger.Error(ret, "failed to initialize MXSML library")
	return ret
}

// isUnqueryableDeviceError reports whether a non-success MXSML return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret mxsml.Return) bool {
	switch ret {
	case mxsml.Failure,
		mxsml.NoDevice,
		mxsml.SysfsError,
		mxsml.InvalidDeviceId,
		mxsml.InvalidDieId,
		mxsml.IOControlFailure,
		mxsml.FlrNotReady,
		mxsml.OpenDeviceFileFailure,
		mxsml.CloseDeviceFileFailure,
		mxsml.BusyDevice,
		mxsml.GetPciBridgeFailure:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library (or zero-card Init return) on
// a node without detected PCI devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret mxsml.Return, noPciCheck bool, pciDevCount int) error {
	if ret.IsSuccess() {
		return nil
	}
	if (ret.IsAPIUnavailable() || ret == mxsml.NoDevice) && (noPciCheck || pciDevCount == 0) {
		return nil
	}
	return fmt.Errorf("failed to %s: %w", op, ret)
}

// handleCardQueryError classifies a fatal per-card query error. Card-specific unqueryable errors
// are logged, recorded in lastUnqueryableErr, and skipped; other errors fail the pass immediately.
func handleCardQueryError(
	logger klog.Logger, index int, op string, ret mxsml.Return, lastUnqueryableErr *error,
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

func getPhysicalIndexes(bdf string) []uint32 {
	var (
		cardID    *int
		renderDID *int
	)

	paths := []string{
		filepath.Join("/sys/module/metax/drivers/pci:METAX", bdf, "drm"),
		filepath.Join("/sys/module/metax/drivers/pci:metax", bdf, "drm"),
	}
	for _, drmPath := range paths {
		info, err := os.Stat(drmPath)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(drmPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, "card") {
				if id, err := strconv.Atoi(name[4:]); err == nil {
					cardID = &id
				}
			} else if strings.HasPrefix(name, "renderD") {
				if id, err := strconv.Atoi(name[7:]); err == nil {
					renderDID = &id
				}
			}
		}
		break
	}

	if cardID != nil {
		if renderDID != nil {
			return []uint32{uint32(*cardID), uint32(*renderDID)}
		}
		return []uint32{uint32(*cardID)}
	}
	return nil
}

var (
	_MXCRegex = regexp.MustCompile(`c\d{2,4}`)
	_MXNRegex = regexp.MustCompile(`n\d{2,4}`)
	_MXGRegex = regexp.MustCompile(`g\d{2,4}`)
)

func guessFamilyFromDeviceName(name string) string {
	if _MXCRegex.MatchString(name) {
		return "MXC"
	}
	if _MXNRegex.MatchString(name) {
		return "MXN"
	}
	if _MXGRegex.MatchString(name) {
		return "MXG"
	}
	return ""
}
