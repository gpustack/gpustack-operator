package nvidia

import (
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

var _PciVendor string

func init() {
	pciID := nodefeature.GetPciVendorID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type nvidia struct {
	once   sync.Once
	nvml   *nvml.NVML
	logger klog.Logger
}

// New creates a new nvidia device interface and initializes the NVML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &nvidia{
		nvml:   nvml.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

func (in *nvidia) Name() string {
	return Manufacturer
}

func (in *nvidia) init() {
	in.once.Do(func() {
		if ret := in.nvml.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize NVML library")
		}
	})
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

	in.init()

	cnt, ret := in.nvml.DeviceGetCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no nvidia devices found")
		} else {
			in.logger.Error(ret, "no nvidia devices found")
		}
		return nil, nil
	}

	drVer, _ := in.nvml.SystemGetDriverVersion()
	rtVer, _ := in.nvml.SystemGetCudaDriverVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.nvml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device handle")
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device UUID")
			continue
		}

		var pciBusId string
		{
			pciHandler := dev.GetPciInfoV()
			pciInfo, ret := pciHandler.V2()
			if !ret.IsSuccess() {
				pciInfo, ret = pciHandler.V1()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to get device PCI info")
					continue
				}
			}
			pciBusId = pciInfo.GetBusId()
		}
		pciDev := pciDevs[pciBusId]

		name, ret := dev.GetName()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device name")
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
				memInfo, ret = memHandler.V1()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to get device memory info")
					continue
				}
			}
			memory = device.ConvertBytesToMiB(memInfo.Total)

			// When ECC is enabled the GPU reserves ~6.25% of physical
			// memory for parity, so NVML reports the reduced usable
			// capacity. Restore the loss so the advertised Memory
			// matches the physical (marketing) size.
			if cur, _, ret := dev.GetEccMode(); ret.IsSuccess() && cur == nvml.FEATURE_ENABLED {
				memory = memory * 16 / 15
			}

			memEccDramUE, ret := dev.GetMemoryErrorCounter(
				nvml.MEMORY_ERROR_TYPE_UNCORRECTED,
				nvml.VOLATILE_ECC,
				nvml.MEMORY_LOCATION_DRAM,
			)
			if ret.IsSuccess() && memEccDramUE > 0 {
				memoryUnhealthy = true
			}
		}

		ccMajor, ccMinor, _ := dev.GetCudaComputeCapability()

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

			// Soft (logical) slicing via HAMi-core ld.preload; the per-device slice count is
			// capped at the max CUDA user processes a GPU serves (128, Volta+).
			grpList[grpIndex].AcceleratorsFeature.LogicalSliced = device.AcceleratorSliced{
				MaxSize:                   128,
				CoresPercentageOvercommit: true,
				MemoryPercentageStep:      1,
			}
			// MIG (physical) hard partitioning is seeded as a placeholder when the card has
			// MIG mode enabled; the real max size / memory step come from the MIG profile in
			// a follow-up.
			migModeCurrent, migModePending, _ := dev.GetMigMode()
			if migModeCurrent == nvml.DEVICE_MIG_ENABLE || migModePending == nvml.DEVICE_MIG_ENABLE {
				grpList[grpIndex].AcceleratorsFeature.PhysicalSliced = device.AcceleratorSliced{
					MaxSize:              7,  // TODO gain max size from MIG profile later
					MemoryPercentageStep: 25, // TODO complete step from MIG profile later
				}
			}
		}

		var physicalIndexes []uint32
		{
			minorNum, ret := dev.GetMinorNumber()
			if ret.IsSuccess() {
				physicalIndexes = []uint32{minorNum}
			} else {
				physicalIndexes = []uint32{uint32(i)}
			}
		}

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy
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

	return grpList, nil
}

func (in *nvidia) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor mthreads devices")
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

	in.init()

	cnt, ret := in.nvml.DeviceGetCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no nvidia devices found")
		} else {
			in.logger.Error(ret, "no nvidia devices found")
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

		dev, ret := in.nvml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device handle")
			continue
		}

		uuid, ret := dev.GetUUID()
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
			memHandler := dev.GetMemoryInfoV()
			memInfo, ret := memHandler.V2()
			if !ret.IsSuccess() {
				memInfo, ret = memHandler.V1()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to get device memory info")
					continue
				}
			}

			memory = device.ConvertBytesToMiB(memInfo.Total)
			memoryUsage = device.ConvertBytesToMiB(memInfo.Used)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)

			memEccDramUE, ret := dev.GetMemoryErrorCounter(
				nvml.MEMORY_ERROR_TYPE_UNCORRECTED,
				nvml.VOLATILE_ECC,
				nvml.MEMORY_LOCATION_DRAM,
			)
			if ret.IsSuccess() && memEccDramUE > 0 {
				memoryUnhealthy = true
			}
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
