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

type thead struct {
	once   sync.Once
	hgml   *hgml.HGML
	logger klog.Logger
}

// New creates a new thead device interface and initializes the HGML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &thead{
		hgml:   hgml.New(binding.WithLogger(logger)),
		logger: logger,
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

	in.init()

	cnt, ret := in.hgml.DeviceGetCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no thead devices found")
		} else {
			in.logger.Error(ret, "no thead devices found")
		}
		return nil, nil
	}

	drVer, _ := in.hgml.SystemGetDriverVersion()
	rtVer, _ := in.hgml.SystemGetHggcDriverVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.hgml.DeviceGetHandleByIndex(i)
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

			memEccDramUE, ret := dev.GetMemoryErrorCounter(
				hgml.MEMORY_ERROR_TYPE_UNCORRECTED,
				hgml.VOLATILE_ECC,
				hgml.MEMORY_LOCATION_DRAM,
			)
			if ret.IsSuccess() && memEccDramUE > 0 {
				memoryUnhealthy = true
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

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy

			// Logical (software) and physical (partition) slicing are mutually exclusive per
			// accelerator, which is what keeps the two capacity-key families from both counting
			// one accelerator. An accelerator currently in the partitioning mode is
			// hard-partitioned and reports only its physical partition profiles; the capability
			// is set solely when the driver actually offers one, so a mode-enabled accelerator
			// whose driver offers nothing reports no capability rather than an empty one. Every
			// other accelerator — mode off, mode unsupported, or the mode unreadable — offers
			// logical slicing instead. A pending-mode transition is not partitioned yet and is
			// re-detected after the administrator's DeviceManager restart, because the re-detect
			// trigger does not include the partitioning mode.
			//
			// A mode the driver could not read is treated as not-partitioned, as it always has
			// been, but it is not treated in silence: since an accelerator in the mode reports
			// ONLY its partition profiles, an administrator who enabled the mode would otherwise
			// see an accelerator quietly advertising the logical slicing it cannot serve. A
			// driver answering that the mode is unsupported is not a failure — that is an
			// accelerator which does not partition.
			migCurrent, _, migRet := dev.GetMigMode()
			if !migRet.IsSuccess() && !driverReportsAbsent(migRet) {
				logger.Error(migRet, "could not read a card's partitioning mode, so it is reported as "+
					"not partitioned; if the mode is in fact enabled, the card advertises logical "+
					"slicing it cannot serve",
					"card", uuid)
			}
			if migCurrent == hgml.DEVICE_MIG_ENABLE {
				status.PhysicalSliced = physicalSliced(detectMigProfiles(dev, logger))
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

	// Accelerators of one group must agree on what a profile name means before the aggregation
	// below merges them by name, so any name they disagree on is withheld from the whole group.
	for i := range grpList {
		for _, reason := range rejectDivergentGroupProfiles(&grpList[i]) {
			in.logger.Info("withheld a partition profile the group's cards disagree on",
				"group", grpList[i].ID, "reason", reason)
		}
	}

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

	in.init()

	cnt, ret := in.hgml.DeviceGetCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no thead devices found")
		} else {
			in.logger.Error(ret, "no thead devices found")
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

		dev, ret := in.hgml.DeviceGetHandleByIndex(i)
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
				hgml.MEMORY_ERROR_TYPE_UNCORRECTED,
				hgml.VOLATILE_ECC,
				hgml.MEMORY_LOCATION_DRAM,
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
	in.once.Do(func() {
		if ret := in.hgml.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize HGML library")
		}
	})
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
