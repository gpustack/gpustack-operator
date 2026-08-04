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

// hbmMemoryBusWidthBits is the memory bus width (bits) at or above which a GPU is HBM rather
// than GDDR. Data-center HBM stacks are >=1024-bit (e.g. A30 3072, A100/H100 5120) while GDDR
// data-center parts top out at 384-bit, so the threshold separates them with a wide margin.
const hbmMemoryBusWidthBits = 1024

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

			// ECC parity bits are carved out of user-visible memory only on GDDR
			// GPUs; HBM keeps ECC in a hardware-reserved region and already reports
			// full capacity. The memory bus width is the NVML-native discriminator —
			// data-center HBM stacks are >=1024-bit (H100 5120, A100 5120, A30 3072)
			// while GDDR parts are <=384-bit — so restore the ~1/16 ECC loss (to the
			// physical/marketing size) only on a narrow (GDDR) bus with ECC enabled.
			// When the bus width is unreadable (older driver) no restore is applied.
			if bw, ret := dev.GetMemoryBusWidth(); ret.IsSuccess() && bw > 0 && bw < hbmMemoryBusWidthBits {
				if cur, _, ret := dev.GetEccMode(); ret.IsSuccess() && cur == nvml.FEATURE_ENABLED {
					memory = memory * 16 / 15
				}
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
		}

		// The recorded minor number is what a card's device node is NAMED after on the vendors that
		// build one from it (/dev/dri/card<minor> and friends), so it is left ABSENT when the driver
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

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy

			// Logical (software) and physical (MIG) slicing are mutually exclusive per card, keyed
			// on the current MIG mode: a card that is currently MIG-enabled is hard-partitioned
			// and reports only its physical MIG profiles. Every other card — MIG off, MIG
			// unsupported (GetMigMode returns not-supported on non-MIG cards), or the mode
			// unreadable — reports the group's logical-slice capability. A pending-mode
			// transition is not partitioned yet and is re-detected after the administrator's
			// reset + DeviceManager restart. This runs per card, fixing the old placeholder's
			// first-card-only-seed defect.
			if migCurrent, _, _ := dev.GetMigMode(); migCurrent == nvml.DEVICE_MIG_ENABLE {
				profiles := detectMigProfiles(dev, memory)
				status.PhysicalSliced = device.AcceleratorPhysicalSliced{
					Profiles: profiles,
					Count:    maxProfileCount(profiles),
				}
			} else {
				// Logical (software) slicing via HAMi-core ld.preload; the per-card slice count is
				// capped at the max CUDA user processes a GPU serves (128, Volta+).
				status.LogicalSliced = device.AcceleratorLogicalSliced{
					Count:                     128,
					CoresPercentageOvercommit: true,
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

	device.SetGroupSlicedDetails(grpList)

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
