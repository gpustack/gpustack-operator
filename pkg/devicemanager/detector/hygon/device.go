package hygon

import (
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

type hygon struct {
	once   sync.Once
	amdgpu *amdgpu.AMDGPU
	rsmi   *rsmi.RSMI
	hsa    *hsa.HSA
	dmi    *dmi.DMI
	logger klog.Logger
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

	in.init()

	cnt, ret := in.rsmi.GetDeviceCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no hygon devices found")
		} else {
			in.logger.Error(ret, "no hygon devices found")
		}
		return nil, nil
	}

	pciDevNames := binding.GetPCIDeviceNames([]string{_PciVendor})
	hsaAgts := in.hsa.GetAgents()

	// Read once, not per card: the mode belongs to the node, so asking each card would be asking the
	// same question eight times and inviting a caller to believe the answers could differ.
	migEnabled, migKnown := in.migModeEnabled()

	drVer := getDriverVersion()
	rtVer, _ := in.rsmi.GetROCMVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.rsmi.GetDeviceHandleByIndex(i)

		uuid, ret := dev.GetUniqueId()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device UUID")
			continue
		}

		var pciBusId string
		{
			pciBusId, ret = dev.GetPciId()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device PCI ID")
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
				logger.Error(ret, "failed to get device memory total")
				continue
			}
			memory = device.ConvertBytesToMiB(memTotal)

			memEcc, ret := dev.GetEccCount(rsmi.GPU_BLOCK_UMC)
			if ret.IsSuccess() && memEcc.Uncorrectable_err > 0 {
				memoryUnhealthy = true
			}
		}

		physicalIndexes := getPhysicalIndexes(pciBusId)

		// Resolved before the group is formed, because on a node in Multi-Instance mode the card's own
		// compute-unit count can be learned nowhere else; see migCardCores.
		var migCard migCardCapability
		if migEnabled {
			migCard = in.detectCardMigProfiles(pciBusId, logger)
		}

		name := pciDevNames.GetName(pciDev.Vendor, pciDev.Device, pciDev.SubVendor, pciDev.SubDevice)
		if name == "" {
			name = hsaAgt.ProductName
			if name == "" && len(physicalIndexes) != 0 {
				name = in.amdgpu.GetMarketingName(physicalIndexes[0])
			}
			if name == "" {
				// RSMI last, and it is the link that carries a partitioned node: pci.ids inside the
				// device-manager image does not know this device, and HSA exposes a single MIG instance
				// per process once the node is in Multi-Instance mode, so seven of eight cards get no
				// agent at all and the three links above all come back empty. A card with no name yields
				// an invalid label key and its group never syncs, so the chain must not end above this.
				name, _ = dev.GetName()
			}
		}

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

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy

			// The two capabilities are mutually exclusive on this hardware, and the exclusivity is the
			// node's rather than the card's. With Multi-Instance mode on, a container given the device
			// nodes but no compute-instance configuration finds NO device at all -- measured on a node
			// where five of eight cards held no instance whatsoever -- so a card in MIG mode can serve
			// neither a whole-card nor a logically sliced request, and publishing either capability
			// would advertise capacity that cannot be allocated.
			//
			// A MODE THAT COULD NOT BE READ PUBLISHES NEITHER. Which of the two is right is exactly
			// what the unread answer decides, and both wrong guesses cost a workload: guessing
			// unpartitioned on a partitioned node admits whole-card work the node cannot serve, and
			// guessing partitioned on a normal node withdraws capacity that works. So the card keeps
			// its whole-card identity and offers no share of itself until the mode reads again, which
			// leaves a pending Pod rather than a failing one.
			switch {
			case !migKnown:
			case migEnabled:
				status.PhysicalSliced = migCard.sliced
			default:
				// DCU logical slicing via hy-virtual (vdev.conf + DTK/hyhal); the per-accelerator slice
				// count is capped at 4 (product default). The vdev.conf assigns each slice a disjoint CU
				// bitmask, so compute is spatially partitioned (the sum stays within one accelerator),
				// not overcommitted.
				status.LogicalSliced = device.AcceleratorLogicalSliced{
					Count: 4,
				}
			}
		}

		grpList[grpIndex].Accelerators = append(
			grpList[grpIndex].Accelerators,
			device.Accelerator{
				ID:              uuid,
				Index:           index,
				PhysicalIndexes: physicalIndexes,
				Topology:        topo,
				Status:          status,
			},
		)
		index++
	}

	// Withhold any profile the cards of one group disagree about, before the aggregation below merges
	// profiles by name and would publish a reading that describes neither card.
	for i := range grpList {
		for _, reason := range rejectDivergentGroupProfiles(&grpList[i]) {
			in.logger.Info("withheld a partition profile from a group", "reason", reason)
		}
	}

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

	in.init()

	cnt, ret := in.rsmi.GetDeviceCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no hygon devices found")
		} else {
			in.logger.Error(ret, "no hygon devices found")
		}
		return nil, nil
	}

	grpList := device.MetricsGroupList{
		{
			Manufacturer: Manufacturer,
			Timestamp:    time.Now(),
			Accelerators: make([]device.AcceleratorMetrics, 0, cnt),
		},
	}
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.rsmi.GetDeviceHandleByIndex(i)

		uuid, ret := dev.GetUniqueId()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device UUID")
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
				logger.Error(ret, "failed to get device memory total")
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

			memEcc, ret := dev.GetEccCount(rsmi.GPU_BLOCK_UMC)
			if ret.IsSuccess() && memEcc.Uncorrectable_err > 0 {
				memoryUnhealthy = true
			}
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

		grpList[0].Accelerators = append(
			grpList[0].Accelerators,
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
	}

	return grpList, nil
}

func (in *hygon) init() {
	in.once.Do(func() {
		if ret := in.amdgpu.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize AMDGPU library")
		}
		if ret := in.rsmi.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize RSMI library")
		}
		if ret := in.hsa.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize HSA library")
		}
		// The Multi-Instance library ships only with a driver that supports partitioning, so a host
		// without one is the ordinary case rather than a fault: the failure is logged at a debug
		// level and every later mode read answers "not partitioned". Init logs its own reason.
		if ret := in.dmi.Init(in.logger.V(3)); !ret.IsSuccess() {
			in.logger.V(3).Info("hygon multi-instance library unavailable; the node cannot be partitioned",
				"return", ret)
		}
	})
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
