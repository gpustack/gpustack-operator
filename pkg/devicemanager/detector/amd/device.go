package amd

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
	initMu     sync.Mutex
	amdgpuInit bool
	rsmiInit   bool
	amdsmiInit bool
	hsaInit    bool
	amdgpu     *amdgpu.AMDGPU
	amdsmi     *amdsmi.AMDSMI
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

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize AMDSMI library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("amd driver api unavailable", "reason", ret)
		return nil, nil
	}

	devs, ret := in.amdsmi.GetProcessorHandles()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get amd devices", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get amd devices")
			return nil, err
		}
		in.logger.Info("no amd devices found", "reason", ret)
		return nil, nil
	}
	if len(devs) == 0 {
		in.logger.Info("no amd devices found")
		return nil, nil
	}

	pciDevNames := binding.GetPCIDeviceNames([]string{_PciVendor})
	hsaAgts := in.hsa.GetAgents()

	rtVer, _ := in.amdsmi.GetROCMVersion()

	grpList := device.DevicesGroupList{}
	var lastUnqueryableErr error
	for i := 0; i < len(devs); i++ {
		logger := in.logger.WithValues("index", i)

		dev := devs[i]

		asicInfo, ret := dev.GetGpuAsicInfo()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device ASIC info", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			continue
		}

		uuid := asicInfo.GetUniqueId()

		var pciBusId string
		{
			bdf, ret := dev.GetGpuDeviceBdf()
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device PCI ID", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
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
				if err := handleCardQueryError(logger, i, "get device VRAM usage", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				continue
			}
			memory = uint64(mem.Total)
			memoryUnhealthy = isDeviceUnhealthy(dev)
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

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

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
				Index:           uint32(i),
				PhysicalIndexes: physicalIndexes,
				Topology:        topo,
				Status:          status,
			},
		)
	}

	if len(grpList) == 0 {
		if lastUnqueryableErr != nil {
			return nil, lastUnqueryableErr
		}
		return nil, nil
	}

	device.SetGroupSlicedDetails(grpList)

	return grpList, nil
}

// unavailableReading is the value AMD's libraries use to say a reading is unavailable rather than to
// report one. It is the maximum of uint16, which is the width GpuMetrics reports utilization and
// temperature in — and it is also what the driver writes into the wider uint32 power fields, rather
// than their own maximum, which is why one constant serves both.
//
// Not widened to the uint32 maximum for those power fields: AMD's own binding compares the whole
// power block against this value, and widening it would stop catching the reading that is actually
// reported. The ROCm SMI side of this backend carries a different sentinel at the wider value —
// kfdStatsInvalid in binding/rsmi/library_device.go — and the two are not interchangeable.
const unavailableReading uint32 = 0xFFFF

// isUnavailableReading reports whether a driver reading is that sentinel rather than a measurement.
//
// It is needed because the call that produced the value succeeded. An SR-IOV virtual function
// exposes no host-side power telemetry to its guest and answers this instead of failing, as does a
// metrics field the driver did not fill, so a caller that checks only the return code publishes
// 65535 as a utilization, a temperature or a wattage. Nothing crashes, which is what makes it worth
// checking for: the figure travels into monitoring and scheduling as though it had been measured.
func isUnavailableReading(v uint32) bool {
	return v == unavailableReading
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

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize AMDSMI library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("amd driver api unavailable", "reason", ret)
		return nil, nil
	}

	devs, ret := in.amdsmi.GetProcessorHandles()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get amd devices", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get amd devices")
			return nil, err
		}
		in.logger.Info("no amd devices found", "reason", ret)
		return nil, nil
	}
	if len(devs) == 0 {
		in.logger.Info("no amd devices found")
		return nil, nil
	}

	metricsGrp := device.MetricsGroup{
		Manufacturer: Manufacturer,
		Timestamp:    time.Now(),
		Accelerators: make([]device.AcceleratorMetrics, 0, len(devs)),
	}
	var lastUnqueryableErr error
	for i := 0; i < len(devs); i++ {
		logger := in.logger.WithValues("index", i)

		dev := devs[i]

		asicInfo, ret := dev.GetGpuAsicInfo()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device ASIC info", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
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
				if err := handleCardQueryError(logger, i, "get device VRAM usage", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				continue
			}

			memory = uint64(mem.Total)
			memoryUsage = uint64(mem.Used)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)
			memoryUnhealthy = isDeviceUnhealthy(dev)
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
				// Assigned only where the reading is one: an unavailable reading keeps the zero the
				// variable already holds, rather than being assigned and then repaired.
				if v := uint32(metricsInfo.Average_gfx_activity); !isUnavailableReading(v) {
					coresUtilization = v
				}
				if v := uint32(metricsInfo.Temperature_hotspot); !isUnavailableReading(v) {
					temperature = v
				}
			}

			powerInfo, ret := dev.GetPowerInfo()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device power usage")
			} else {
				// The current socket power, the average as its fallback, and neither where both are
				// the sentinel -- which is what a virtual function reports for every one of them.
				switch {
				case !isUnavailableReading(powerInfo.Current_socket_power):
					powerUsage = powerInfo.Current_socket_power
				case !isUnavailableReading(powerInfo.Average_socket_power):
					powerUsage = powerInfo.Average_socket_power
				}
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
	}

	if len(metricsGrp.Accelerators) == 0 {
		if lastUnqueryableErr != nil {
			return nil, lastUnqueryableErr
		}
		return nil, nil
	}

	return device.MetricsGroupList{metricsGrp}, nil
}

func isDeviceUnhealthy(dev amdsmi.Device) bool {
	memEcc, ret := dev.GetGpuEccCount(amdsmi.GPU_BLOCK_UMC)
	return evaluateDeviceUnhealthy(memEcc, ret)
}

// evaluateDeviceUnhealthy reports whether the vendor's uncorrectable ECC count makes the
// accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(eccCount amdsmi.ErrorCount, ret amdsmi.Return) bool {
	return ret.IsSuccess() && eccCount.Uncorrectable_count > 0
}

func (in *amd) init() {
	_ = in.ensureInit()
}

func (in *amd) ensureInit() amdsmi.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.amdsmiInit {
		return amdsmi.STATUS_SUCCESS
	}

	if !in.amdgpuInit {
		if ret := in.amdgpu.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize AMDGPU library")
		} else {
			in.amdgpuInit = true
		}
	}
	// ROCm SMI IS LOADED BEFORE AMD SMI, AND THE ORDER IS NOT INTERCHANGEABLE. Measured on
	// ROCm 7.2.0: with AMD SMI loaded first, the dlopen of ROCm SMI aborts the process with
	// SIGBUS inside dlopen itself, while this order leaves both libraries working and agreeing
	// on every accelerator's identity. Whoever reorders these will not find out from a test.
	//
	// The order is not free on a newer stack, and amdsmi.New is where it is paid for: ROCm SMI
	// in the global scope zeroes AMD SMI's socket enumeration, so that library is loaded
	// RTLD_DEEPBIND.
	if !in.rsmiInit {
		if ret := in.rsmi.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize ROCm SMI library")
		} else {
			in.rsmiInit = true
		}
	}
	ret := in.amdsmi.Init()
	if !ret.IsSuccess() {
		in.logger.Error(ret, "failed to initialize AMDSMI library")
		return ret
	}
	in.amdsmiInit = true

	if !in.hsaInit {
		if hsaRet := in.hsa.Init(); !hsaRet.IsSuccess() {
			in.logger.Error(hsaRet, "failed to initialize HSA library")
		} else {
			in.hsaInit = true
		}
	}
	return amdsmi.STATUS_SUCCESS
}

// isUnqueryableDeviceError reports whether a non-success AMDSMI return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret amdsmi.Return) bool {
	switch ret {
	case amdsmi.STATUS_DRM_ERROR,
		amdsmi.STATUS_API_FAILED,
		amdsmi.STATUS_IO,
		amdsmi.STATUS_FILE_ERROR,
		amdsmi.STATUS_FILE_NOT_FOUND,
		amdsmi.STATUS_DIRECTORY_NOT_FOUND,
		amdsmi.STATUS_BUSY,
		amdsmi.STATUS_NOT_FOUND,
		amdsmi.STATUS_NO_DATA,
		amdsmi.STATUS_AMDGPU_RESTART_ERR,
		amdsmi.STATUS_CORRUPTED_EEPROM,
		amdsmi.STATUS_INTERNAL_EXCEPTION,
		amdsmi.STATUS_IPC_ERROR:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library on a node without detected PCI
// devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret amdsmi.Return, noPciCheck bool, pciDevCount int) error {
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
	logger klog.Logger, index int, op string, ret amdsmi.Return, lastUnqueryableErr *error,
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
