package metax

import (
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
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

const Manufacturer = devicefeature.ManufacturerMetaX

var _PciVendor string

func init() {
	pciID := devicefeature.GetPciID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type metax struct {
	once   sync.Once
	mxsml  *mxsml.MXSML
	logger klog.Logger
}

// New creates a new metax device interface and initializes the MXSML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &metax{
		mxsml:  mxsml.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

func (in *metax) Name() string {
	return Manufacturer
}

func (in *metax) init() {
	in.once.Do(func() {
		if ret := in.mxsml.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize MXSML library")
		}
	})
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
		in.logger.Info("no metax PCI devices found")
		return nil, nil
	}

	in.init()

	cnt := in.mxsml.GetDeviceCount()
	if cnt == 0 {
		in.logger.Info("no metax devices found")
		return nil, nil
	}

	drVer, _ := in.mxsml.GetDriverVersion()
	rtVer, _ := in.mxsml.GetMacaVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.mxsml.GetDeviceHandleByIndex(i)

		info, ret := dev.GetInfo()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device info")
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
				logger.Error(ret, "failed to get device memory info")
				continue
			}
			memory = device.ConvertKiBToMiB(memInfo.VramTotal)

			memEcc, ret := dev.GetTotalEccErrors()
			if ret.IsSuccess() && memEcc.DramUE > 0 {
				memoryUnhealthy = true
			}
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
		in.logger.Info("no iluvatar pci devices found")
		return nil, nil
	}

	in.init()

	cnt := in.mxsml.GetDeviceCount()
	if cnt == 0 {
		in.logger.Info("no metax devices found")
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

		dev := in.mxsml.GetDeviceHandleByIndex(i)

		info, ret := dev.GetInfo()
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device info")
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
				logger.Error(ret, "failed to get device memory info")
				continue
			}

			memory = device.ConvertKiBToMiB(memInfo.VramTotal)
			memoryUsage = device.ConvertKiBToMiB(memInfo.VramUse)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)

			memEcc, ret := dev.GetTotalEccErrors()
			if ret.IsSuccess() && memEcc.DramUE > 0 {
				memoryUnhealthy = true
			}
		}

		var (
			coresUtilization uint32
			temperature      uint32
			powerUsage       uint32
		)
		{
			ipUsage, ret := dev.GetIpUsage(mxsml.Usage_Xcore)
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device core utilization")
			} else {
				coresUtilization = uint32(ipUsage)
			}

			tempInfo, ret := dev.GetTemperatureInfo(mxsml.Temperature_Hotspot)
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device temperature")
			} else {
				temperature = uint32(tempInfo)
			}

			powerInfo, ret := dev.GetBoardPowerInfo()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device power usage")
			} else {
				powerUsage = slicex.Sum(powerInfo, func(info mxsml.BoardWayElectricInfo) uint32 {
					return info.Power / 1_000 // Convert mW to W.
				})
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
