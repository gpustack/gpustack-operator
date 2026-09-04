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
	"gpustack.ai/gpustack/pkg/utils/strconvx"
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

// device.Detector implementation, in the interface's declaration order.

func (in *ascend) Name() string {
	return Manufacturer
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

			uuid, ret := deviceUUID(dev)
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device vdie info")
				continue
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
			// dcmi's own addressing for this accelerator, in the slot order every consumer reads
			// it by:
			//
			//   0 -- the physical id, which vcann-rt keys its quota config by and which a vendor
			//        runtime resolves on every generation but A5;
			//   1 -- the dcmi card id, which on the V2 API is also the device (logic) id: that
			//        generation has no card level and enumerates devices flat, so
			//        cardId == devId == logicId (see binding/dcmi GetCardList). The allocator
			//        names ASCEND_VISIBLE_DEVICES by this slot on A5 (family "950");
			//   2 -- the device's index within the card, always 0 on V2.
			//
			// The allocator's container-share seam addresses a device by slots 1 and 2 together.
			// All three are distinct numbers on real hardware, so a consumer reading the wrong
			// slot names another accelerator rather than failing.
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

			topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

			// RoCE networking belongs to the dcmi card, a hardware level above the
			// accelerator that carries several NPUs; it is recorded on the topology of
			// each accelerator in it.
			if ip, snm, ret := dev.GetIp(dcmi.ROCE_PORT, 0); ret.IsSuccess() {
				if gw, ret := dev.GetGateway(dcmi.ROCE_PORT, 0); ret.IsSuccess() {
					topo.RoCE = &device.Ethernet{
						IP:         ip.String(),
						SubnetMask: snm.String(),
						Gateway:    gw.String(),
					}
				}
			}

			var status device.AcceleratorStatus
			{
				status.Unhealthy = memoryUnhealthy
				status.LogicalSliced = getLogicalSliced(
					grpList[grpIndex].Family,
					grpList[grpIndex].RuntimeVersion,
				)
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
	}

	device.SetGroupSlicedDetails(grpList)

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

			uuid, ret := deviceUUID(dev)
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device vdie info")
				continue
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

func (in *ascend) init() {
	in.once.Do(func() {
		if ret := in.dcmi.Init(in.logger); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize DCMI library")
		}
	})
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
	"Ascend910_9363":    256,
	"Ascend910_9579":    260,
	"Ascend910_95":      260,
	"Ascend950":         260,
}

// deviceUUID reads the die id identifying a device, which is what the Devices API carries as the
// accelerator's id.
//
// Both driver generations are covered without a branch here, because the binding orders the two
// entry points: on a V2 driver the first is that generation's die query — which asks for the virtual
// die and then the DDie, the type the vendor names as the A5 chip's uuid — and the second is a V1
// entry point a V2 driver refuses. On a V1 driver the DDie is never asked for.
//
// A device whose die cannot be read is dropped by the caller rather than identified some other way.
// Accelerator.ID is universally unique by contract, and the only other per-device number to hand is
// the PCI address, which repeats on every node of a fleet — substituting it would make two nodes'
// accelerators collide on identity, which is worse than a missing device.
func deviceUUID(dev dcmi.Device) (string, dcmi.Return) {
	dieHandler := dev.GetVDieV()

	die, ret := dieHandler.V2()
	if ret.IsSuccess() {
		return die.String(), ret
	}

	die, v1Ret := dieHandler.V1()
	if v1Ret.IsSuccess() {
		return die.String(), v1Ret
	}

	// Which of the two failures is worth carrying out depends on whether the second call was
	// served at all, which is a question its own return code answers -- so neither generation has
	// to be named here. A V2 driver refuses the V1 entry point outright, and letting that refusal
	// out would bury what the die queries above reported: a permission error or a timeout would
	// reach the log as "this driver does not serve that call". A V1 driver does serve it, and
	// there the second failure is the specific one, the first being an older driver turning down
	// the newer die query.
	if v1Ret == dcmi.ERROR_NOT_SUPPORT || v1Ret == dcmi.ERROR_FUNCTION_NOT_FOUND {
		return "", ret
	}

	return "", v1Ret
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
	// The 950 generation is matched by prefix, not by name. Its suffixes are an open set -- 950PR
	// and 950DT ship today -- and every vendor reader treats them as one soc: ascend-common's
	// Is910A5Chip, ascend-docker-runtime's own type mapper and torch_npu's SetSocVersion all test
	// HasPrefix("Ascend950") and collapse whatever follows onto the single Ascend950 soc version.
	// Listing the suffixes instead would leave the next one resolving to no family at all, since
	// none of the fallbacks below match a name starting with 950.
	if strings.HasPrefix(devName, "950") {
		return "Ascend950"
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

// slicedRuntimeMajors lists, per family, the CANN runtime majors the operator image actually
// builds a vcann-rt for -- one entry per xbuild-ascend-cann-<major>-<family> stage. Nothing
// downstream re-checks the pairing: the allocator composes the library path as
// "cann-<major>-<family>" and mounts it, so a family/major this map does not cover would be
// offered slicing and then fail to start the container on a directory that was never built.
// Adding a build stage means adding its major here.
var slicedRuntimeMajors = map[string][]int{
	"910B": {8, 9},
	"910C": {8, 9},
	"950":  {9},
	"310P": {9}, // upstream added the 310P dcmi adapter in CANN 9.1.0.
}

// getLogicalSliced reports the logical (software) slicing a family supports on a host running
// the given CANN runtime version: temporal compute sharing plus software VRAM partitioning
// through vcann-rt's preloaded runtime, with the per-accelerator slice count capped at the maximum
// user processes a device serves (63). A family/major pair the image ships no runtime for yields
// the zero value, so slicing is simply not offered rather than offered and then unstartable.
//
// The major is compared as a number because it arrives as a string, in which "10" sorts below
// "9".
func getLogicalSliced(family, runtimeVersion string) device.AcceleratorLogicalSliced {
	majors, supported := slicedRuntimeMajors[family]
	if !supported {
		return device.AcceleratorLogicalSliced{}
	}
	major, err := strconvx.Atoi[int](device.RuntimeMajor(runtimeVersion, "8"))
	if err != nil || !slices.Contains(majors, major) {
		return device.AcceleratorLogicalSliced{}
	}

	return device.AcceleratorLogicalSliced{
		Count:                     63,
		CoresPercentageOvercommit: true,
	}
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
