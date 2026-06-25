package ascend

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerAscend

var (
	_ToolkitHome string
	_PciVendor   string
)

func init() {
	_ToolkitHome = os.Getenv("ASCEND_TOOLKIT_HOME")
	if _ToolkitHome == "" {
		_ToolkitHome = "/usr/local/Ascend/cann"
		if s, err := os.Stat(_ToolkitHome); err != nil || !s.IsDir() {
			_ToolkitHome = "/usr/local/Ascend/ascend-toolkit/latest/runtime"
		}
	}

	pciID := nodefeature.GetPciVendorID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type ascend struct {
	once   sync.Once
	dcmi   *dcmi.DCMI
	logger klog.Logger
}

// New creates a new ascend device interface and initializes the DCMI library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &ascend{
		dcmi:   dcmi.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

func (in *ascend) Name() string {
	return Manufacturer
}

func (in *ascend) init() {
	in.once.Do(func() {
		if ret := in.dcmi.Init(in.logger); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize DCMI library")
		}
	})
}

func (in *ascend) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect ascend devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no ascend pci devices found")
		return nil, nil
	}

	in.init()

	_, cardList, ret := in.dcmi.GetCardList()
	if !ret.IsSuccess() || len(cardList) == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no ascend devices found")
		} else {
			in.logger.Error(ret, "no ascend devices found")
		}
		return nil, nil
	}

	drVer, _ := in.dcmi.GetDriverVersion()
	rtVer := getToolkitVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for _, card := range cardList {
		logger := in.logger.WithValues("card", card)

		cnt, ret := in.dcmi.GetDeviceNumInCard(card)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device count in card")
			continue
		}

		for i := int32(0); i < cnt; i++ {
			logger := logger.WithValues("index", i)

			dev := in.dcmi.GetDeviceHandleByCardAndIndex(card, i)

			typ, ret := dev.GetType()
			if ret.IsSuccess() && typ != dcmi.NPU_TYPE {
				logger.Info("skipping non-NPU device")
				continue
			}

			var uuid string
			{
				dieHandler := dev.GetVDieV()
				die, ret := dieHandler.V2()
				if !ret.IsSuccess() {
					die, ret = dieHandler.V1()
					if !ret.IsSuccess() {
						logger.Error(ret, "failed to get device vdie info")
						continue
					}
				}
				uuid = die.String()
			}

			var pciBusId string
			{
				pciHandler := dev.GetPcieInfoV()
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

			var chipInfo dcmi.ChipInfoV2
			{
				infoHandler := dev.GetChipInfoV()
				chipInfo, ret = infoHandler.V2()
				if !ret.IsSuccess() {
					chipInfo, ret = infoHandler.V1()
					if !ret.IsSuccess() {
						logger.Error(ret, "failed to get device chip info")
						continue
					}
				}
			}

			name := chipInfo.GetChipName()

			var (
				memory          uint64
				memoryUnhealthy bool
			)
			{
				hbmInfo, ret := dev.GetHbmInfo()
				if !ret.IsSuccess() {
					memHandler := dev.GetMemoryInfoV()
					memInfo, ret := memHandler.V2()
					if !ret.IsSuccess() {
						logger.Error(ret, "failed to get device memory info")
						continue
					}
					memory = memInfo.Memory_size

					eccInfo, ret := dev.GetEccInfo(dcmi.DEVICE_TYPE_DDR)
					if ret.IsSuccess() && eccInfo.Enable_flag > 0 {
						memoryUnhealthy = eccInfo.Single_bit_error_cnt > 0 || eccInfo.Double_bit_error_cnt > 0
					}
				} else {
					memory = hbmInfo.Memory_size

					eccInfo, ret := dev.GetEccInfo(dcmi.DEVICE_TYPE_HBM)
					if ret.IsSuccess() && eccInfo.Enable_flag > 0 {
						memoryUnhealthy = eccInfo.Single_bit_error_cnt > 0 || eccInfo.Double_bit_error_cnt > 0
					}
				}
			}

			phyId, ret := dev.GetPhysicalID()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device physical ID")
				continue
			}
			physicalIndexes := []uint32{phyId, uint32(card), uint32(i)}

			grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
				return grp.Name == name && grp.Memory == memory
			})
			if grpIndex == -1 {
				// New group.
				cores := chipInfo.Aicore_cnt
				family := getFamilyFromSocName(guessSocNameFromDeviceName(name))
				grpList = append(grpList, device.DevicesGroup{
					ID:             device.ConstructGroupID(Manufacturer, name, memory),
					Manufacturer:   Manufacturer,
					Name:           name,
					Memory:         memory,
					Cores:          cores,
					DriverVersion:  drVer,
					RuntimeVersion: device.NormalizeVersion(rtVer),
					Family:         family,
				})
				grpIndex = len(grpList) - 1
			}

			topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class)

			var features device.AcceleratorFeatures
			{
				ip, snm, ret := dev.GetIp(dcmi.ROCE_PORT, 0)
				if ret.IsSuccess() {
					gw, ret := dev.GetGateway(dcmi.ROCE_PORT, 0)
					if ret.IsSuccess() {
						features.RoCE = &device.Ethernet{
							IP:         ip.String(),
							SubnetMask: snm.String(),
							Gateway:    gw.String(),
						}
					}
				}
			}

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
					Features:        features,
					Status:          status,
				},
			)
			index++
		}
	}

	return grpList, nil
}

func (in *ascend) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor ascend devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no ascend pci devices found")
		return nil, nil
	}

	in.init()

	_, cardList, ret := in.dcmi.GetCardList()
	if !ret.IsSuccess() || len(cardList) == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no ascend devices found")
		} else {
			in.logger.Error(ret, "no ascend devices found")
		}
		return nil, nil
	}

	grpList := device.MetricsGroupList{
		{
			Manufacturer: Manufacturer,
			Timestamp:    time.Now(),
			Accelerators: make([]device.AcceleratorMetrics, 0, len(cardList)*2),
		},
	}
	for _, card := range cardList {
		logger := in.logger.WithValues("card", card)

		cnt, ret := in.dcmi.GetDeviceNumInCard(card)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device count in card")
			continue
		}

		for i := int32(0); i < cnt; i++ {
			logger := logger.WithValues("index", i)

			dev := in.dcmi.GetDeviceHandleByCardAndIndex(card, i)

			typ, ret := dev.GetType()
			if ret.IsSuccess() && typ != dcmi.NPU_TYPE {
				logger.Info("skipping non-NPU device")
				continue
			}

			var uuid string
			{
				dieHandler := dev.GetVDieV()
				die, ret := dieHandler.V2()
				if !ret.IsSuccess() {
					die, ret = dieHandler.V1()
					if !ret.IsSuccess() {
						logger.Error(ret, "failed to get device vdie info")
						continue
					}
				}
				uuid = die.String()
			}

			var (
				memory            uint64
				memoryUsage       uint64
				memoryUtilization uint32
				memoryUnhealthy   bool
			)
			{
				hbmInfo, ret := dev.GetHbmInfo()
				if !ret.IsSuccess() {
					memHandler := dev.GetMemoryInfoV()
					memInfo, ret := memHandler.V2()
					if !ret.IsSuccess() {
						logger.Error(ret, "failed to get device memory info")
						continue
					}

					memory = memInfo.Memory_size
					memoryUsage = memInfo.Memory_size - memInfo.Memory_available
					memoryUtilization = device.CalculateUtilization(memoryUsage, memory)

					eccInfo, ret := dev.GetEccInfo(dcmi.DEVICE_TYPE_DDR)
					if ret.IsSuccess() && eccInfo.Enable_flag > 0 {
						memoryUnhealthy = eccInfo.Single_bit_error_cnt > 0 || eccInfo.Double_bit_error_cnt > 0
					}
				} else {
					memory = hbmInfo.Memory_size
					memoryUsage = hbmInfo.Memory_usage
					memoryUtilization = device.CalculateUtilization(memoryUsage, memory)

					eccInfo, ret := dev.GetEccInfo(dcmi.DEVICE_TYPE_HBM)
					if ret.IsSuccess() && eccInfo.Enable_flag > 0 {
						memoryUnhealthy = eccInfo.Single_bit_error_cnt > 0 || eccInfo.Double_bit_error_cnt > 0
					}
				}
			}

			var (
				coresUtilization uint32
				temperature      uint32
				powerUsage       uint32
			)
			{
				utilHandler := dev.GetUtilizationRateV()
				utilInfo, ret := utilHandler.V2()
				if !ret.IsSuccess() {
					utilInfo, ret = utilHandler.V1()
					if !ret.IsSuccess() {
						logger.V(3).Error(ret, "failed to get device cores utilization")
					} else {
						coresUtilization = utilInfo.Aicore_util
					}
				} else {
					coresUtilization = utilInfo.Aicore_util
				}

				temp, ret := dev.GetTemperature()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device temperature")
				} else if temp > 0 {
					temperature = uint32(temp)
				}

				power, ret := dev.GetPowerInfo()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device power usage")
				} else if power > 0 {
					powerUsage = uint32(power) / 10 // Convert from deciwatts to watts.
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
	}

	return grpList, nil
}

// Borrowed from https://gitcode.com/Ascend/pytorch/blob/master/torch_npu/csrc/core/npu/NpuVariables.cpp#L13-L40 and
// https://gitcode.com/Ascend/pytorch/blob/master/torch_npu/csrc/core/npu/NpuVariables.h#L5-L34.
// Ascend product category, please refer to:
// https://www.hiascend.com/document/detail/zh/AscendFAQ/ProduTech/productform/hardwaredesc_0001.html and
// https://blog.csdn.net/fuhanghang/article/details/146411242.
var socNameVersionMap = map[string]int{
	"Ascend910PremiumA": 100,
	"Ascend910ProA":     101,
	"Ascend910A":        102,
	"Ascend910ProB":     103,
	"Ascend910B":        104,
	"Ascend310P1":       200,
	"Ascend310P2":       201,
	"Ascend310P3":       202,
	"Ascend310P4":       203,
	"Ascend310P5":       204,
	"Ascend310P7":       205,
	"Ascend910B1":       220,
	"Ascend910B2":       221,
	"Ascend910B2C":      222,
	"Ascend910B3":       223,
	"Ascend910B4":       224,
	"Ascend910B4-1":     225,
	"Ascend310B1":       240,
	"Ascend310B2":       241,
	"Ascend310B3":       242,
	"Ascend310B4":       243,
	"Ascend910_9391":    250,
	"Ascend910":         250,
	"Ascend910_9392":    251,
	"Ascend910_9381":    252,
	"Ascend910_9382":    253,
	"Ascend910_9372":    254,
	"Ascend910_9362":    255,
	"Ascend910_9579":    260,
	"Ascend910_95":      260,
	"Ascend950":         260,
}

var (
	_910ARegex = regexp.MustCompile(`^910`)
	_910BRegex = regexp.MustCompile(`^(910B\d|A2G\d)`)
	_310PRegex = regexp.MustCompile(`^(310P\d?|I2\d?)`)
)

func guessSocNameFromDeviceName(devName string) string {
	devName = strings.TrimPrefix(strings.TrimSpace(devName), "Ascend")
	socName := "Ascend" + strings.TrimSpace(devName)
	if _, ok := socNameVersionMap[socName]; ok {
		return socName
	}
	// https://gitcode.com/Ascend/mind-cluster/blob/master/component/ascend-common/devmanager/common/utils.go#L159-L176
	if _310PRegex.MatchString(devName) {
		return "Ascend310P1"
	}
	if strings.Contains(devName, "310B") {
		return "Ascend310B1"
	}
	// if strings.Contains(devName, "310") {
	// 	return "Ascend310"
	// }
	if _910BRegex.MatchString(devName) {
		return "Ascend910B1"
	}
	if _910ARegex.MatchString(devName) {
		return "Ascend910A"
	}
	return ""
}

func getFamilyFromSocName(socName string) string {
	if socName != "" {
		socVersion, ok := socNameVersionMap[socName]
		if ok {
			switch {
			case socVersion < 200:
				return "910"
			case socVersion < 220:
				return "310P"
			case socVersion < 240:
				return "910B" // 910B/A2
			case socVersion < 250:
				return "310B"
			case socVersion < 260:
				return "910C" // 910C/A3
			case socVersion < 270:
				return "950" // 950/A5
			}
		}
	}

	return ""
}

func getToolkitVersion() string {
	const prefix = "Version="

	for _, path := range []string{
		filepath.Join(_ToolkitHome, "version.info"),
		filepath.Join(_ToolkitHome, "share", "info", "runtime", "version.info"),
	} {
		if s, err := os.Stat(path); err == nil && s.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				continue
			}

			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, prefix) {
					_ = f.Close()
					return strings.TrimPrefix(line, prefix)
				}
			}
			_ = f.Close()
		}
	}
	return ""
}
