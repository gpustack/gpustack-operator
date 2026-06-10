package cambricon

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/cndev"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerCambricon

var _PciVendor string

func init() {
	pciID := nodefeature.GetPciID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type cambricon struct {
	once   sync.Once
	cndev  *cndev.CNDev
	logger klog.Logger
}

// New creates a new cambricon device interface and initializes the RSMI library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &cambricon{
		cndev:  cndev.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

func (in *cambricon) Name() string {
	return Manufacturer
}

func (in *cambricon) init() {
	in.once.Do(func() {
		if ret := in.cndev.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize CNDev library")
		}
	})
}

func (in *cambricon) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect cambricon devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no cambricon PCI devices found")
		return nil, nil
	}

	in.init()

	cnt, ret := in.cndev.GetDeviceCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no cambricon devices found")
		} else {
			in.logger.Error(ret, "no cambricon devices found")
		}
		return nil, nil
	}

	rtVer := getRuntimeVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.cndev.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device handle by index")
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device UUID")
			continue
		}

		var pciBusId string
		{
			pciHandler := dev.GetPCIeInfoV()
			pciInfo, ret := pciHandler.V2()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device PCI info")
				continue
			}
			pciBusId = pciInfo.GetBusId()
		}
		pciDev := pciDevs[pciBusId]

		name, ret := dev.GetCardName()
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
				logger.Error(ret, "failed to get device memory total")
				continue
			}
			memory = uint64(memInfo.PhysicalMemoryTotal)

			healthHandler := dev.GetCardHealthStateV()
			healthInfo, ret := healthHandler.V2()
			if ret.IsSuccess() {
				memoryUnhealthy = healthInfo.Health == 0
			}
		}

		grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
			return grp.Name == name && grp.Memory == memory
		})
		if grpIndex == -1 {
			var drVer string
			if verInfo, ret := dev.GetVersionInfo(); ret.IsSuccess() {
				drVer = fmt.Sprintf("%d.%d", verInfo.DriverMajorVersion, verInfo.DriverMinorVersion)
			}

			// New group.
			grpList = append(grpList, device.DevicesGroup{
				ID:             device.ConstructGroupID(Manufacturer, name, memory),
				Manufacturer:   Manufacturer,
				Name:           name,
				Memory:         memory,
				DriverVersion:  drVer,
				RuntimeVersion: device.NormalizeVersion(rtVer),
			})
			grpIndex = len(grpList) - 1
		}

		physicalIndexes := []uint32{uint32(i)}

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class)

		// var features device.AcceleratorFeatures

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
				// Features: features,
				Status: status,
			},
		)
		index++
	}

	return grpList, nil
}

func (in *cambricon) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor cambricon devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no cambricon PCI devices found")
		return nil, nil
	}

	in.init()

	cnt, ret := in.cndev.GetDeviceCount()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no cambricon devices found")
		} else {
			in.logger.Error(ret, "no cambricon devices found")
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

		dev, ret := in.cndev.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device handle by index")
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
				logger.Error(ret, "failed to get device memory total")
				continue
			}

			memory = uint64(memInfo.PhysicalMemoryTotal)
			memoryUsage = uint64(memInfo.PhysicalMemoryUsed)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)

			healthHandler := dev.GetCardHealthStateV()
			healthInfo, ret := healthHandler.V2()
			if ret.IsSuccess() {
				memoryUnhealthy = healthInfo.Health == 0
			}
		}

		var (
			coresUtilization uint32
			temperature      uint32
			powerUsage       uint32
		)
		{
			utilInfo, ret := dev.GetUtilizationInfo()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device cores utilization")
			} else {
				coresUtilization = uint32(utilInfo.AverageCoreUtilization)
			}

			tempInfo, ret := dev.GetTemperatureInfo()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device temperature")
			} else {
				temperature = uint32(tempInfo.Chip)
			}

			powerInfo, ret := dev.GetPowerInfo()
			if !ret.IsSuccess() {
				logger.V(3).Error(ret, "failed to get device power usage")
			} else {
				powerUsage = uint32(powerInfo.Usage)
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

var versionRegexp = regexp.MustCompile(`\d+\.\d+\.\d+`)

func getRuntimeVersion() string {
	paths := []string{
		"/usr/local/neuware/version.txt",
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				match := versionRegexp.FindString(strings.TrimSpace(string(data)))
				if match != "" {
					return match
				}
			}
			break
		}
	}
	return ""
}
