package mthreads

import (
	"fmt"
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
	initMu      sync.Mutex
	initialized bool
	mtml        *mtml.MTML
	logger      klog.Logger
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
		in.logger.Error(e, "failed to detect mthreads devices")
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

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize MTML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("mthreads driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.mtml.CountDevice()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get mthreads device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get mthreads device count")
			return nil, err
		}
		in.logger.Info("no mthreads devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.logger.Info("no mthreads devices found")
		return nil, nil
	}

	drVer, _ := in.mtml.SystemGetDriverVersion()

	grpList := device.DevicesGroupList{}
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.mtml.InitDeviceByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			continue
		}

		if err := func() error {
			defer func() {
				_ = dev.Free()
			}()

			prop, ret := dev.GetProperty()
			if !ret.IsSuccess() {
				return handleCardQueryError(logger, i, "get device property", ret, &lastUnqueryableErr)
			}
			if mtml.VirtRole(prop.VirtCap) == mtml.VIRT_ROLE_GUEST_VIRTDEVICE {
				logger.Info("skipping virtual device")
				return nil
			}

			uuid, ret := dev.GetUUID()
			if !ret.IsSuccess() {
				return handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr)
			}

			var pciBusId string
			{
				pciInfo, ret := dev.GetPciInfo()
				if !ret.IsSuccess() {
					return handleCardQueryError(logger, i, "get device PCI info", ret, &lastUnqueryableErr)
				}
				pciBusId = pciInfo.GetBusId()
			}
			pciDev := pciDevs[pciBusId]

			name, ret := dev.GetName()
			if !ret.IsSuccess() {
				return handleCardQueryError(logger, i, "get device name", ret, &lastUnqueryableErr)
			}

			var (
				memory          uint64
				memoryUnhealthy bool
			)
			{
				memHandler, ret := dev.InitMemory()
				if !ret.IsSuccess() {
					return handleCardQueryError(logger, i, "initialize memory handler", ret, &lastUnqueryableErr)
				}
				defer func() {
					_ = memHandler.Free()
				}()

				memTotal, ret := memHandler.GetTotal()
				if !ret.IsSuccess() {
					return handleCardQueryError(logger, i, "get device memory total", ret, &lastUnqueryableErr)
				}
				memory = device.ConvertBytesToMiB(memTotal)
				memoryUnhealthy = isDeviceUnhealthy(memHandler)
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

			topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

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
					Index:           uint32(i),
					PhysicalIndexes: physicalIndexes,
					Topology:        topo,
					Status:          status,
				},
			)
			return nil
		}(); err != nil {
			return nil, err
		}
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

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize MTML library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("mthreads driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.mtml.CountDevice()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get mthreads device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get mthreads device count")
			return nil, err
		}
		in.logger.Info("no mthreads devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.logger.Info("no mthreads devices found")
		return nil, nil
	}

	metricsGrp := device.MetricsGroup{
		Manufacturer: Manufacturer,
		Timestamp:    time.Now(),
		Accelerators: make([]device.AcceleratorMetrics, 0, cnt),
	}
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.mtml.InitDeviceByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			continue
		}

		if err := func() error {
			defer func() {
				_ = dev.Free()
			}()

			prop, ret := dev.GetProperty()
			if !ret.IsSuccess() {
				return handleCardQueryError(logger, i, "get device property", ret, &lastUnqueryableErr)
			}
			if mtml.VirtRole(prop.VirtCap) == mtml.VIRT_ROLE_GUEST_VIRTDEVICE {
				logger.Info("skipping virtual device")
				return nil
			}

			uuid, ret := dev.GetUUID()
			if !ret.IsSuccess() {
				return handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr)
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
					return handleCardQueryError(logger, i, "initialize memory handler", ret, &lastUnqueryableErr)
				}
				defer func() {
					_ = memHandler.Free()
				}()

				memTotal, ret := memHandler.GetTotal()
				if !ret.IsSuccess() {
					return handleCardQueryError(logger, i, "get device memory total", ret, &lastUnqueryableErr)
				}
				memory = device.ConvertBytesToMiB(memTotal)

				memUsed, ret := memHandler.GetUsed()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device memory used")
				} else {
					memoryUsage = device.ConvertBytesToMiB(memUsed)
				}

				memoryUtilization = device.CalculateUtilization(memoryUsage, memory)
				memoryUnhealthy = isDeviceUnhealthy(memHandler)
			}

			var (
				coresUtilization uint32
				temperature      uint32
				powerUsage       uint32
			)
			{
				gpuHandler, ret := dev.InitGpu()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to initialize gpu handler")
				} else {
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
				}

				powerUsage, ret = dev.GetPowerUsage()
				if !ret.IsSuccess() {
					logger.V(3).Error(ret, "failed to get device power usage")
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
			return nil
		}(); err != nil {
			return nil, err
		}
	}

	if len(metricsGrp.Accelerators) == 0 {
		if lastUnqueryableErr != nil {
			return nil, lastUnqueryableErr
		}
		return nil, nil
	}

	return device.MetricsGroupList{metricsGrp}, nil
}

func isDeviceUnhealthy(memHandler mtml.MemoryHandler) bool {
	memEccDramUE, ret := memHandler.GetEccErrorCounter(
		mtml.MEMORY_ERROR_TYPE_UNCORRECTED,
		mtml.VOLATILE_ECC,
		mtml.MEMORY_LOCATION_DRAM,
	)
	return evaluateDeviceUnhealthy(memEccDramUE, ret)
}

// evaluateDeviceUnhealthy reports whether the vendor's uncorrectable ECC count makes the
// accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(eccErrors uint64, ret mtml.Return) bool {
	return ret.IsSuccess() && eccErrors > 0
}

func (in *mthreads) init() {
	_ = in.ensureInit()
}

func (in *mthreads) ensureInit() mtml.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.initialized {
		return mtml.SUCCESS
	}

	ret := in.mtml.Init()
	if ret.IsSuccess() {
		in.initialized = true
		return mtml.SUCCESS
	}

	in.logger.Error(ret, "failed to initialize MTML library")
	return ret
}

// isUnqueryableDeviceError reports whether a non-success MTML return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret mtml.Return) bool {
	switch ret {
	case mtml.ERROR_DRIVER_FAILURE,
		mtml.ERROR_NOT_FOUND,
		mtml.ERROR_RESOURCE_IS_BUSY,
		mtml.ERROR_UNKNOWN:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library on a node without detected PCI
// devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret mtml.Return, noPciCheck bool, pciDevCount int) error {
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
	logger klog.Logger, index int, op string, ret mtml.Return, lastUnqueryableErr *error,
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
