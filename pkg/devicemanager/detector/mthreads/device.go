package mthreads

import (
	"slices"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/mtml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

const Manufacturer = nodefeature.ManufacturerMThreads

var _PciVendor string

func init() {
	pciID := nodefeature.GetPciVendorID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type mthreads struct {
	once   sync.Once
	mtml   *mtml.MTML
	logger klog.Logger
}

// New creates a new mthreads device interface and initializes the MTML library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &mthreads{
		mtml:   mtml.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

// Name, DetectAccelerator and MonitorAccelerator implement device.Detector.
func (in *mthreads) Name() string {
	return Manufacturer
}

func (in *mthreads) DetectAccelerator(noPciCheck bool) (_ device.DevicesGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to detect metax devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no mthreads pci devices found")
		return nil, nil
	}

	in.init()

	cnt, ret := in.mtml.CountDevice()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no mthreads devices found")
		} else {
			in.logger.Error(ret, "no mthreads devices found")
		}
		return nil, nil
	}

	drVer, _ := in.mtml.SystemGetDriverVersion()

	var index uint32
	grpList := device.DevicesGroupList{}
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.mtml.InitDeviceByIndex(i)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device handle")
			continue
		}

		func() {
			defer func() {
				_ = dev.Free()
			}()

			prop, ret := dev.GetProperty()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device property")
				return
			}
			if mtml.VirtRole(prop.VirtCap) == mtml.VIRT_ROLE_GUEST_VIRTDEVICE {
				logger.Info("skipping virtual device")
				return
			}

			uuid, ret := dev.GetUUID()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device UUID")
				return
			}

			var pciBusId string
			{
				pciInfo, ret := dev.GetPciInfo()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to get device PCI Info")
					return
				}
				pciBusId = pciInfo.GetBusId()
			}
			pciDev := pciDevs[pciBusId]

			name, ret := dev.GetName()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device name")
				return
			}

			var (
				memory          uint64
				memoryUnhealthy bool
			)
			{
				memHandler, ret := dev.InitMemory()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to initialize memory handler")
					return
				}
				defer func() {
					_ = memHandler.Free()
				}()

				memTotal, ret := memHandler.GetTotal()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to get device memory total")
					return
				}
				memory = device.ConvertBytesToMiB(memTotal)

				memEccDramUE, ret := memHandler.GetEccErrorCounter(
					mtml.MEMORY_ERROR_TYPE_UNCORRECTED,
					mtml.VOLATILE_ECC,
					mtml.MEMORY_LOCATION_DRAM,
				)
				if ret.IsSuccess() && memEccDramUE > 0 {
					memoryUnhealthy = true
				}
			}

			grpIndex := slices.IndexFunc(grpList, func(grp device.DevicesGroup) bool {
				return grp.Name == name && grp.Memory == memory
			})
			if grpIndex == -1 {
				// New group.
				cores, _ := dev.CountGpuCores()
				grpList = append(grpList, device.DevicesGroup{
					ID:            device.ConstructGroupID(Manufacturer, name, memory),
					Manufacturer:  Manufacturer,
					Name:          name,
					Memory:        memory,
					Cores:         cores,
					DriverVersion: drVer,
				})
				grpIndex = len(grpList) - 1
			}

			physicalIndexes := []uint32{uint32(i)}

			topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class)

			var status device.AcceleratorStatus
			{
				status.Unhealthy = memoryUnhealthy
				// GPU logical slicing via the sGPU kmod + MTHREADS_QOS_* env; the per-accelerator
				// slice count is capped at 16. Compute is a relative weight (not a hard cap), so it is
				// not overcommitted.
				status.LogicalSliced = device.AcceleratorLogicalSliced{
					Count: 16,
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
		}()
	}

	device.SetGroupSlicedDetails(grpList)

	return grpList, nil
}

func (in *mthreads) MonitorAccelerator(noPciCheck bool) (_ device.MetricsGroupList, err error) {
	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor mthreads devices")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		in.logger.Info("no mthreads pci devices found")
		return nil, nil
	}

	in.init()

	cnt, ret := in.mtml.CountDevice()
	if !ret.IsSuccess() || cnt == 0 {
		if ret.IsSuccess() {
			in.logger.Info("no mthreads devices found")
		} else {
			in.logger.Error(ret, "no mthreads devices found")
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

		dev, ret := in.mtml.InitDeviceByIndex(i)
		if !ret.IsSuccess() {
			logger.Error(ret, "failed to get device handle")
			continue
		}

		func() {
			defer func() {
				_ = dev.Free()
			}()

			prop, ret := dev.GetProperty()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device property")
				return
			}
			if mtml.VirtRole(prop.VirtCap) == mtml.VIRT_ROLE_GUEST_VIRTDEVICE {
				logger.Info("skipping virtual device")
				return
			}

			uuid, ret := dev.GetUUID()
			if !ret.IsSuccess() {
				logger.Error(ret, "failed to get device UUID")
				return
			}

			var (
				memory            uint64
				memoryUsage       uint64
				memoryUtilization uint32
				memoryUnhealthy   bool
			)
			{
				memHandler, ret := dev.InitMemory()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to initialize memory handler")
					return
				}
				defer func() {
					_ = memHandler.Free()
				}()

				memTotal, ret := memHandler.GetTotal()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to get device memory total")
					return
				}
				memory = device.ConvertBytesToMiB(memTotal)

				memUsed, ret := memHandler.GetUsed()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device memory used")
				} else {
					memoryUsage = device.ConvertBytesToMiB(memUsed)
				}

				memoryUtilization = device.CalculateUtilization(memoryUsage, memory)

				memEccDramUE, ret := memHandler.GetEccErrorCounter(
					mtml.MEMORY_ERROR_TYPE_UNCORRECTED,
					mtml.VOLATILE_ECC,
					mtml.MEMORY_LOCATION_DRAM,
				)
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device memory ECC error counter")
				} else if memEccDramUE > 0 {
					memoryUnhealthy = true
				}
			}

			var (
				coresUtilization uint32
				temperature      uint32
				powerUsage       uint32
			)
			{
				gpuHandler, ret := dev.InitGpu()
				if !ret.IsSuccess() {
					logger.Error(ret, "failed to initialize gpu handler")
					return
				}
				defer func() {
					_ = gpuHandler.Free()
				}()

				coresUtilization, ret = gpuHandler.GetUtilization()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device cores utilization")
				}

				temperature, ret = gpuHandler.GetTemperature()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device temperature")
				}

				powerUsage, ret = dev.GetPowerUsage()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device power usage")
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
		}()
	}

	return grpList, nil
}

func (in *mthreads) init() {
	in.once.Do(func() {
		if ret := in.mtml.Init(); !ret.IsSuccess() {
			in.logger.Error(ret, "failed to initialize MTML library")
		}
	})
}
