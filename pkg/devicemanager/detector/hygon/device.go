package hygon

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/amdgpu"
	"gpustack.ai/gpustack/binding/dmi"
	"gpustack.ai/gpustack/binding/hsa"
	"gpustack.ai/gpustack/binding/rsmi"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerHygon

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

type hygon struct {
	initMu     sync.Mutex
	amdgpuInit bool
	rsmiInit   bool
	hsaInit    bool
	dmiInit    bool
	amdgpu     *amdgpu.AMDGPU
	rsmi       *rsmi.RSMI
	hsa        *hsa.HSA
	dmi        *dmi.DMI
	logger     klog.Logger

	cacheMu         sync.Mutex
	cachedDetected  map[int]cachedAccelerator
	cachedMonitored map[int]cachedMetrics
}

// New creates a new hygon device interface and initializes the RSMI library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &hygon{
		amdgpu: amdgpu.New(binding.WithLogger(logger)),
		rsmi:   rsmi.New(binding.WithLogger(logger)),
		hsa:    hsa.New(binding.WithLogger(logger)),
		dmi:    dmi.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

// Name, DetectAccelerator and MonitorAccelerator implement device.Detector.
func (in *hygon) Name() string {
	return Manufacturer
}

func (in *hygon) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect hygon devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no hygon pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize RSMI library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("hygon driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.rsmi.GetDeviceCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get hygon device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get hygon device count")
			return nil, err
		}
		in.logger.Info("no hygon devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no hygon devices found")
		return nil, nil
	}

	pciDevNames := binding.GetPCIDeviceNames([]string{_PciVendor})
	hsaAgts := in.hsa.GetAgents()

	// Read once, not per card: the mode belongs to the node, so asking each card would be asking the
	// same question eight times and inviting a caller to believe the answers could differ.
	migEnabled, err := in.migModeEnabled()
	if err != nil {
		return nil, err
	}

	modelNames := make(modelNameCache)

	drVer := getDriverVersion()
	rtVer, _ := in.rsmi.GetROCMVersion()

	grpList := device.DevicesGroupList{}
	queriedCount := 0
	liveSlots := make(map[int]struct{}, cnt)
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.rsmi.GetDeviceHandleByIndex(i)

		uuid, ret := dev.GetUniqueId()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainUnqueryableDetected(&grpList, unqueryableSlots, i)
			continue
		}

		var pciBusId string
		{
			pciBusId, ret = dev.GetPciId()
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device PCI ID", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.retainUnqueryableDetected(&grpList, unqueryableSlots, i)
				continue
			}
		}
		pciDev := pciDevs[pciBusId]
		hsaAgt, ok := hsaAgts[pciBusId]
		if !ok {
			hsaAgt = hsaAgts[uuid]
		}

		var (
			memory          uint64
			memoryUnhealthy bool
		)
		{
			memTotal, ret := dev.GetMemoryTotal(rsmi.MEM_TYPE_VRAM)
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device memory total", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.retainUnqueryableDetected(&grpList, unqueryableSlots, i)
				continue
			}
			memory = device.ConvertBytesToMiB(memTotal)
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		physicalIndexes := getPhysicalIndexes(pciBusId)

		// Resolved before the group is formed, because on a node in Multi-Instance mode the card's own
		// compute-unit count can be learned nowhere else; see migCardCores.
		var migCard migCardCapability
		if migEnabled {
			var migRet dmi.Return
			migCard, migRet = in.detectCardMigProfiles(pciBusId, logger)
			if !migRet.IsSuccess() {
				if err := handleDMICardQueryError(logger, i, "detect device MIG profiles", migRet, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.retainUnqueryableDetected(&grpList, unqueryableSlots, i)
				continue
			}
		}

		name := modelNames.resolve(pciDev, func() string {
			n := pciDevNames.GetName(pciDev.Vendor, pciDev.Device, pciDev.SubVendor, pciDev.SubDevice)
			// HSA is deliberately skipped on a partitioned node. Its runtime cannot initialize inside
			// this container unless a partition happens to be visible to it, so whether it answers is
			// a function of what the node is currently carved into rather than of the hardware -- and
			// a name that changes with that is worse than a plain one. It was seen to flip between
			// device-manager restarts on the same node, which renames the group, and a renamed group
			// is a new group: a second InstanceType, a second ClusterQueue, and a Pod admitted against
			// the one the node no longer publishes.
			if n == "" && !migEnabled {
				n = hsaAgt.ProductName
			}
			if n == "" && len(physicalIndexes) != 0 {
				n = in.amdgpu.GetMarketingName(physicalIndexes[0])
			}
			if n == "" {
				// RSMI last, and it is the link that keeps a partitioned node nameable at all: it is
				// unaffected by the mode and answers for every card, with the PCI device id itself
				// when it can name nothing better. A card with no name yields an invalid label key and
				// its group never syncs, so a plain model id beats nothing. Getting a product name
				// there instead is a matter of the image's pci.ids knowing the device, which upstream
				// does not.
				n, _ = dev.GetName()
			}
			return n
		})

		tgVer := hsaAgt.Name
		if tgVer == "" {
			tgVer, _ = dev.GetTargetGraphicsVersion()
		}

		grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
			return grp.Name == name && grp.Memory == memory
		})
		if grpIndex == -1 {
			// New group.
			var (
				cores  = hsaAgt.ComputeUnitCount
				family = hsaAgt.Family()
			)
			if migEnabled {
				// HSA describes an instance rather than the card here, so its count is the partition's
				// and would understate the card's compute; the partition profiles carry the real one.
				cores = migCard.cores
			}
			if cores == 0 {
				if len(physicalIndexes) != 0 {
					gpuInfo, ret2 := in.amdgpu.QueryGPUInfo(physicalIndexes[0])
					if ret2.IsSuccess() {
						cores = gpuInfo.Cu_active_number
						family = gpuInfo.Family()
					}
				}
			}
			grpList = append(grpList, device.DevicesGroup{
				ID:                device.ConstructGroupID(Manufacturer, name, memory),
				Manufacturer:      Manufacturer,
				Name:              name,
				Memory:            memory,
				Cores:             cores,
				DriverVersion:     drVer,
				RuntimeVersion:    device.NormalizeVersion(rtVer),
				Family:            family,
				ComputeCapability: tgVer,
			})
			grpIndex = len(grpList) - 1
		}

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy

			// The two capabilities are mutually exclusive on this hardware, and the exclusivity is the
			// node's rather than the card's. With Multi-Instance mode on, a container given the device
			// nodes but no compute-instance configuration finds NO device at all, so a card in MIG
			// mode can serve neither a whole-card nor a logically sliced request.
			if migEnabled {
				status.PhysicalSliced = migCard.sliced
			} else {
				// DCU logical slicing via hy-virtual (vdev.conf + DTK/hyhal); the per-accelerator slice
				// count is capped at 4 (product default). The vdev.conf assigns each slice a disjoint CU
				// bitmask, so compute is spatially partitioned (the sum stays within one accelerator),
				// not overcommitted.
				status.LogicalSliced = device.AcceleratorLogicalSliced{
					Count: 4,
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
		liveSlots[i] = struct{}{}
		queriedCount++

		nextMonitored[i] = cachedMetrics{
			id:     uuid,
			memory: memory,
		}
	}

	if queriedCount == 0 {
		if lastUnqueryableErr != nil {
			return nil, lastUnqueryableErr
		}
		return nil, nil
	}

	// Withhold any profile the cards of one group disagree about, before the aggregation below merges
	// profiles by name and would publish a reading that describes neither card.
	for i := range grpList {
		for _, reason := range rejectDivergentGroupProfiles(&grpList[i]) {
			in.logger.Info("withheld a partition profile from a group", "reason", reason)
		}
	}

	in.updateDetectedCache(snapshotLiveDetected(grpList, liveSlots), nextMonitored, unqueryableSlots)

	device.SetGroupSlicedDetails(grpList)

	return grpList, nil
}

func (in *hygon) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor hygon devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no hygon pci devices found")
		return nil, nil
	}

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize RSMI library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("hygon driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.rsmi.GetDeviceCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get hygon device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get hygon device count")
			return nil, err
		}
		in.logger.Info("no hygon devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.clearCache()
		in.logger.Info("no hygon devices found")
		return nil, nil
	}

	metricsGrp := device.MetricsGroup{
		Manufacturer: Manufacturer,
		Timestamp:    time.Now(),
		Accelerators: make([]device.AcceleratorMetrics, 0, cnt),
	}
	queriedCount := 0
	nextMonitored := make(map[int]cachedMetrics, cnt)
	unqueryableSlots := make(map[int]struct{})
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.rsmi.GetDeviceHandleByIndex(i)

		uuid, ret := dev.GetUniqueId()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			in.retainUnqueryableMonitored(&metricsGrp, unqueryableSlots, i)
			continue
		}

		var (
			memory            uint64
			memoryUsage       uint64
			memoryUtilization uint32
			memoryUnhealthy   bool
		)
		{
			memTotal, ret := dev.GetMemoryTotal(rsmi.MEM_TYPE_VRAM)
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device memory total", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				in.retainUnqueryableMonitored(&metricsGrp, unqueryableSlots, i)
				continue
			}
			memory = device.ConvertBytesToMiB(memTotal)

			memUsage, ret := dev.GetMemoryUsage(rsmi.MEM_TYPE_VRAM)
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device memory usage")
			} else {
				memoryUsage = device.ConvertBytesToMiB(memUsage)
			}

			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)
			memoryUnhealthy = isDeviceUnhealthy(dev)
		}

		var (
			coresUtilization uint32
			temperature      uint32
			powerUsage       uint32
		)
		{
			coresUtilization, ret = dev.GetBusyPercent()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device cores utilization")
			}

			temp, ret := dev.GetTempMetric(rsmi.TEMP_CURRENT)
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device temperature")
			} else if temp > 0 {
				temperature = uint32(temp) / 1_000 // Convert from millidegree Celsius to degree Celsius.
			}

			power, ret := dev.GetPower()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device power usage")
			} else if power > 0 {
				powerUsage = uint32(power) / 1_000_000 // Convert from microwatt to watt.
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
		queriedCount++
		nextMonitored[i] = cachedMetrics{
			id:     uuid,
			memory: memory,
		}
	}

	if queriedCount == 0 {
		if lastUnqueryableErr != nil {
			return nil, lastUnqueryableErr
		}
		return nil, nil
	}

	in.updateMonitoredCache(nextMonitored, unqueryableSlots)

	return device.MetricsGroupList{metricsGrp}, nil
}

func (in *hygon) getCachedDetected(slot int) (cachedAccelerator, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedDetected[slot]
	if !ok {
		return cachedAccelerator{}, false
	}
	grp := *cached.group.DeepCopy()
	grp.Accelerators = nil
	return cachedAccelerator{
		group: grp,
		accel: *cached.accel.DeepCopy(),
	}, true
}

func (in *hygon) getCachedMonitored(slot int) (cachedMetrics, bool) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	cached, ok := in.cachedMonitored[slot]
	return cached, ok
}

func (in *hygon) clearCache() {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	in.cachedDetected = nil
	in.cachedMonitored = nil
}

func snapshotLiveDetected(
	grpList device.DevicesGroupList, liveSlots map[int]struct{},
) map[int]cachedAccelerator {
	nextDetected := make(map[int]cachedAccelerator, len(liveSlots))
	for _, grp := range grpList {
		grpHeader := grp
		grpHeader.Accelerators = nil
		for _, accel := range grp.Accelerators {
			slot := int(accel.Index)
			if _, isLive := liveSlots[slot]; !isLive {
				continue
			}
			nextDetected[slot] = cachedAccelerator{
				group: *grpHeader.DeepCopy(),
				accel: *accel.DeepCopy(),
			}
		}
	}
	return nextDetected
}

func (in *hygon) updateDetectedCache(
	nextDetected map[int]cachedAccelerator,
	nextMonitored map[int]cachedMetrics,
	unqueryableSlots map[int]struct{},
) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	for slot := range unqueryableSlots {
		if _, exists := nextDetected[slot]; !exists {
			if prev, ok := in.cachedDetected[slot]; ok {
				nextDetected[slot] = prev
			}
		}
		if _, exists := nextMonitored[slot]; !exists {
			if prev, ok := in.cachedMonitored[slot]; ok {
				nextMonitored[slot] = prev
			}
		}
	}
	in.cachedDetected = nextDetected
	in.cachedMonitored = nextMonitored
}

func (in *hygon) updateMonitoredCache(
	nextMonitored map[int]cachedMetrics,
	unqueryableSlots map[int]struct{},
) {
	in.cacheMu.Lock()
	defer in.cacheMu.Unlock()

	for slot := range unqueryableSlots {
		if _, exists := nextMonitored[slot]; !exists {
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

func (in *hygon) retainUnqueryableDetected(
	grpList *device.DevicesGroupList,
	unqueryableSlots map[int]struct{},
	slot int,
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

func (in *hygon) retainUnqueryableMonitored(
	metricsGrp *device.MetricsGroup,
	unqueryableSlots map[int]struct{},
	slot int,
) {
	unqueryableSlots[slot] = struct{}{}
	cached, ok := in.getCachedMonitored(slot)
	if !ok {
		return
	}
	metricsGrp.Accelerators = append(metricsGrp.Accelerators, device.AcceleratorMetrics{
		ID:        cached.id,
		Memory:    cached.memory,
		Unhealthy: true,
	})
}

func isDeviceUnhealthy(dev rsmi.Device) bool {
	memEcc, ret := dev.GetEccCount(rsmi.GPU_BLOCK_UMC)
	return evaluateDeviceUnhealthy(memEcc, ret)
}

// evaluateDeviceUnhealthy reports whether the vendor's uncorrectable ECC count makes the
// accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(eccCount rsmi.ErrorCount, ret rsmi.Return) bool {
	return ret.IsSuccess() && eccCount.Uncorrectable_err > 0
}

// modelNameCache resolves a card's name ONCE PER MODEL rather than once per card.
//
// Cards sharing a PCI identity are the same product, so they must carry the same name -- and the
// name is what groups them. Resolving it per card lets two cards of one model disagree whenever the
// sources disagree, which is not hypothetical on this hardware: with Multi-Instance mode on, the HSA
// runtime answers for at most one card and pci.ids inside the device-manager image knows none of
// them, so one card can be named from a source the others cannot reach. The node's identical
// hardware then splits into two groups, publishes two InstanceTypes, and divides its own capacity
// between them.
type modelNameCache map[string]string

// resolve returns the model's name, computing it with resolveOne only for the first card of a model
// that asks. An empty answer is not cached: a later card may reach a source this one could not, and
// caching nothing would fix the whole model at nameless.
func (c modelNameCache) resolve(pciDev binding.PCIDevice, resolveOne func() string) string {
	key := strings.Join([]string{pciDev.Vendor, pciDev.Device, pciDev.SubVendor, pciDev.SubDevice}, ":")
	if name, ok := c[key]; ok && name != "" {
		return name
	}
	name := resolveOne()
	if name != "" {
		c[key] = name
	}
	return name
}

func (in *hygon) init() {
	_ = in.ensureInit()
}

func (in *hygon) ensureInit() rsmi.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if !in.amdgpuInit {
		if ret := in.amdgpu.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize AMDGPU library")
		} else {
			in.amdgpuInit = true
		}
	}
	if !in.rsmiInit {
		ret := in.rsmi.Init()
		if !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize RSMI library")
			return ret
		}
		in.rsmiInit = true
	}

	if !in.hsaInit {
		if hsaRet := in.hsa.Init(); !hsaRet.IsSuccess() {
			in.logger.Error(hsaRet, "failed to initialize HSA library")
		} else {
			in.hsaInit = true
		}
	}
	// The Multi-Instance library ships only with a driver that supports partitioning, so a host
	// without one is the ordinary case rather than a fault: the failure is logged at a debug
	// level and every later mode read answers "not partitioned". Init logs its own reason.
	if !in.dmiInit {
		if dmiRet := in.dmi.Init(in.logger.V(3)); !dmiRet.IsSuccess() && dmiRet != dmi.ERROR_ALREADY_INITIALIZED {
			in.logger.V(3).Info("hygon multi-instance library unavailable; the node cannot be partitioned",
				"return", dmiRet)
		} else {
			in.dmiInit = true
		}
	}
	return rsmi.STATUS_SUCCESS
}

// isUnqueryableDeviceError reports whether a non-success RSMI return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret rsmi.Return) bool {
	switch ret {
	case rsmi.STATUS_FILE_ERROR,
		rsmi.STATUS_INTERNAL_EXCEPTION,
		rsmi.STATUS_NOT_FOUND,
		rsmi.STATUS_NO_DATA,
		rsmi.STATUS_BUSY,
		rsmi.STATUS_DIRECTORY_NOT_FOUND,
		rsmi.STATUS_AMDGPU_RESTART_ERR,
		rsmi.STATUS_IPC_ERROR:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library on a node without detected PCI
// devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret rsmi.Return, noPciCheck bool, pciDevCount int) error {
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
	logger klog.Logger, index int, op string, ret rsmi.Return, lastUnqueryableErr *error,
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

func getDriverVersion() string {
	paths := []string{
		"/sys/module/hycu/version",
		"/sys/module/hydcu/version",
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				return strings.TrimSpace(string(data))
			}
			break
		}
	}
	return ""
}

func getPhysicalIndexes(bdf string) []uint32 {
	var (
		cardID    *int
		renderDID *int
	)

	paths := []string{
		filepath.Join("/sys/module/hycu/drivers/pci:hycu", bdf, "drm"),
		filepath.Join("/sys/module/hydcu/drivers/pci:hydcu", bdf, "drm"),
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
