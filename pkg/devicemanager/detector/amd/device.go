package amd

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
	"gpustack.ai/gpustack/binding/amdsmi"
	"gpustack.ai/gpustack/binding/hsa"
	"gpustack.ai/gpustack/binding/rsmi"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerAMD

var _PciVendor string

func init() {
	pciID := nodefeature.GetPciVendorID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type amd struct {
	once   sync.Once
	amdgpu *amdgpu.AMDGPU
	amdsmi *amdsmi.AMDSMI
	// rsmi serves the per-process query alone. AMD SMI is this backend's library for everything
	// else, but its per-process device membership entry point answers INVAL on a live process id
	// (measured on ROCm 7.2.0 / AMD SMI 26.2.1), and without membership a row cannot be told from
	// another card's — while ROCm SMI answers the same question per device, which is the shape the
	// figure needs. It is the route rocm-smi itself takes on this stack.
	rsmi   *rsmi.RSMI
	hsa    *hsa.HSA
	logger klog.Logger
}

// New creates a new amd device interface and initializes the AMDSMI library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &amd{
		amdgpu: amdgpu.New(binding.WithLogger(logger)),
		amdsmi: amdsmi.New(binding.WithLogger(logger)),
		rsmi:   rsmi.New(binding.WithLogger(logger)),
		hsa:    hsa.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

// Name, DetectAccelerator and MonitorAccelerator implement device.Detector.
func (in *amd) Name() string {
	return Manufacturer
}

func (in *amd) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect amd devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no amd pci devices found")
		return nil, nil
	}

	in.init()

	devs, ret := in.amdsmi.GetProcessorHandles()
	if !ret.IsSuccess() || len(devs) == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no amd devices found")
		} else {
			in.logger.Error(ret, "no amd devices found")
		}
		return nil, nil
	}

	pciDevNames := binding.GetPCIDeviceNames([]string{_PciVendor})
	hsaAgts := in.hsa.GetAgents()

	rtVer, _ := in.amdsmi.GetROCMVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for i := 0; i < len(devs); i++ {
		logger := in.logger.WithValues("index", i)

		dev := devs[i]

		asicInfo, ret := dev.GetGpuAsicInfo()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device ASIC info")
			continue
		}

		uuid := asicInfo.GetUniqueId()

		var pciBusId string
		{
			bdf, ret := dev.GetGpuDeviceBdf()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device PCI ID")
				continue
			}
			pciBusId = bdf.String()
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
			mem, ret := dev.GetGpuVramUsage()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device VRAM usage")
				continue
			}
			memory = uint64(mem.Total)

			memEcc, ret := dev.GetGpuEccCount(amdsmi.GPU_BLOCK_UMC)
			if ret.IsSuccess() && memEcc.Uncorrectable_count > 0 {
				memoryUnhealthy = true
			}
		}

		physicalIndexes := getPhysicalIndexes(pciBusId)

		name := pciDevNames.GetName(pciDev.Vendor, pciDev.Device, pciDev.SubVendor, pciDev.SubDevice)
		if name == "" {
			name = hsaAgt.ProductName
			if name == "" && len(physicalIndexes) != 0 {
				name = in.amdgpu.GetMarketingName(physicalIndexes[0])
			}
			if name == "" {
				name = asicInfo.GetMarketName()
			}
		}

		tgVer := hsaAgt.Name
		if tgVer == "" {
			tgVer = asicInfo.GetTargetGraphicsVersion()
		}

		grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
			return grp.Name == name && grp.Memory == memory
		})
		if grpIndex == -1 {
			// New group.
			var drVer string
			if drInfo, ret := dev.GetGpuDriverInfo(); ret.IsSuccess() {
				drVer = drInfo.GetVersion()
			}
			var (
				cores  = hsaAgt.ComputeUnitCount
				family = hsaAgt.Family()
			)
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

			// AMD has no partitioning mode, so logical (software) slicing via the
			// ld.so.preload shim is unconditional. The count is a deliberately loose device-plugin token
			// pool — the binding constraint on a slice request is its memory budget — set to
			// the same 128 the NVIDIA and THead detectors publish. Overcommit is true because
			// CU masks may overlap and tenants sharing an overlap divide it fairly, measured on
			// both RDNA and CDNA; memory is the exact dimension the flag does not touch.
			status.LogicalSliced = device.AcceleratorLogicalSliced{
				Count:                     128,
				CoresPercentageOvercommit: true,
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

	device.SetGroupSlicedDetails(grpList)

	return grpList, nil
}

func (in *amd) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor amd devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no amd pci devices found")
		return nil, nil
	}

	in.init()

	devs, ret := in.amdsmi.GetProcessorHandles()
	if !ret.IsSuccess() || len(devs) == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no amd devices found")
		} else {
			in.logger.Error(ret, "no amd devices found")
		}
		return nil, nil
	}

	grpList := device.MetricsGroupList{
		{
			Manufacturer: Manufacturer,
			Timestamp:    time.Now(),
			Accelerators: make([]device.AcceleratorMetrics, 0, len(devs)),
		},
	}
	for i := 0; i < len(devs); i++ {
		logger := in.logger.WithValues("index", i)

		dev := devs[i]

		asicInfo, ret := dev.GetGpuAsicInfo()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device ASIC info")
			continue
		}

		uuid := asicInfo.GetUniqueId()

		var (
			memory            uint64
			memoryUsage       uint64
			memoryUtilization uint32
			memoryUnhealthy   bool
		)
		{
			mem, ret := dev.GetGpuVramUsage()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device VRAM usage")
				continue
			}

			memory = uint64(mem.Total)
			memoryUsage = uint64(mem.Used)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)

			memEcc, ret := dev.GetGpuEccCount(amdsmi.GPU_BLOCK_UMC)
			if ret.IsSuccess() && memEcc.Uncorrectable_count > 0 {
				memoryUnhealthy = true
			}
		}

		var (
			coresUtilization uint32
			temperature      uint32
			powerUsage       uint32
		)
		{
			metricsInfo, ret := dev.GetGpuMetricsInfo()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device cores utilization and temperature")
			} else {
				coresUtilization = uint32(metricsInfo.Average_gfx_activity)
				temperature = uint32(metricsInfo.Temperature_hotspot)
			}

			powerInfo, ret := dev.GetPowerInfo()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device power usage")
			} else {
				powerUsage = powerInfo.Current_socket_power
				if powerUsage == 0xFFFF {
					powerUsage = powerInfo.Average_socket_power
				}
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

func (in *amd) init() {
	in.once.Do(func() {
		if ret := in.amdgpu.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize AMDGPU library")
		}
		// ROCm SMI IS LOADED BEFORE AMD SMI, AND THE ORDER IS NOT INTERCHANGEABLE. Measured on
		// ROCm 7.2.0: with AMD SMI loaded first, the dlopen of ROCm SMI aborts the process with
		// SIGBUS inside dlopen itself, while this order leaves both libraries working and agreeing
		// on every accelerator's identity. Whoever reorders these will not find out from a test.
		//
		// The order is not free on a newer stack, and amdsmi.New is where it is paid for: ROCm SMI
		// in the global scope zeroes AMD SMI's socket enumeration, so that library is loaded
		// RTLD_DEEPBIND.
		if ret := in.rsmi.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize ROCm SMI library")
		}
		if ret := in.amdsmi.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize AMDSMI library")
		}
		if ret := in.hsa.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize HSA library")
		}
	})
}

func getPhysicalIndexes(bdf string) []uint32 {
	var (
		cardID    *int
		renderDID *int
	)

	paths := []string{
		filepath.Join("/sys/module/amdgpu/drivers/pci:amdgpu", bdf, "drm"),
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
