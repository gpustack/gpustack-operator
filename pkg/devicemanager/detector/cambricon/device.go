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
	pciID := nodefeature.GetPciVendorID(Manufacturer)
	p := strings.Split(pciID, "_")
	_PciVendor = p[len(p)-1]
}

type cambricon struct {
	initMu      sync.Mutex
	initialized bool
	cndev       *cndev.CNDev
	logger      klog.Logger
}

// New creates a new cambricon device interface and initializes the RSMI library.
func New(opts device.DetectorOptions) device.Detector {
	logger := opts.Logger.WithName(Manufacturer)
	return &cambricon{
		cndev:  cndev.New(binding.WithLogger(logger)),
		logger: logger,
	}
}

// Name, DetectAccelerator and MonitorAccelerator implement device.Detector.
func (in *cambricon) Name() string {
	return Manufacturer
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

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize CNDev library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("cambricon driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.cndev.GetDeviceCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get cambricon device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get cambricon device count")
			return nil, err
		}
		in.logger.Info("no cambricon devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.logger.Info("no cambricon devices found")
		return nil, nil
	}

	rtVer := getRuntimeVersion()

	grpList := device.DevicesGroupList{}
	var lastUnqueryableErr error
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.cndev.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle by index", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			continue
		}

		var pciBusId string
		{
			pciHandler := dev.GetPCIeInfoV()
			pciInfo, ret := pciHandler.V2()
			if !ret.IsSuccess() {
				if err := handleCardQueryError(logger, i, "get device PCI info", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				continue
			}
			pciBusId = pciInfo.GetBusId()
		}
		pciDev := pciDevs[pciBusId]

		name, ret := dev.GetCardName()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device name", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
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
				if err := handleCardQueryError(logger, i, "get device memory total", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				continue
			}
			memory = uint64(memInfo.PhysicalMemoryTotal)
			memoryUnhealthy = isDeviceUnhealthy(dev)
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

		topo := device.ConstructTopology(pciBusId, pciDev.Root, pciDev.Class, pciDev.Switches)

		var status device.AcceleratorStatus
		{
			status.Unhealthy = memoryUnhealthy
			// MLU logical slicing via the cnDev sMLU profile; the per-accelerator slice count is
			// capped at 16 (operationally confirmed). Compute is spatially partitioned, not overcommitted.
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

	if ret := in.ensureInit(); !ret.IsSuccess() {
		if err := classifyInitOrCountError("initialize CNDev library", ret, noPciCheck, len(pciDevs)); err != nil {
			return nil, err
		}
		in.logger.Info("cambricon driver api unavailable", "reason", ret)
		return nil, nil
	}

	cnt, ret := in.cndev.GetDeviceCount()
	if !ret.IsSuccess() {
		if err := classifyInitOrCountError("get cambricon device count", ret, noPciCheck, len(pciDevs)); err != nil {
			in.logger.Error(ret, "failed to get cambricon device count")
			return nil, err
		}
		in.logger.Info("no cambricon devices found", "reason", ret)
		return nil, nil
	}
	if cnt == 0 {
		in.logger.Info("no cambricon devices found")
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

		dev, ret := in.cndev.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device handle by index", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
			continue
		}

		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			if err := handleCardQueryError(logger, i, "get device UUID", ret, &lastUnqueryableErr); err != nil {
				return nil, err
			}
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
				if err := handleCardQueryError(logger, i, "get device memory total", ret, &lastUnqueryableErr); err != nil {
					return nil, err
				}
				continue
			}

			memory = uint64(memInfo.PhysicalMemoryTotal)
			memoryUsage = uint64(memInfo.PhysicalMemoryUsed)
			memoryUtilization = device.CalculateUtilization(memoryUsage, memory)
			memoryUnhealthy = isDeviceUnhealthy(dev)
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

func (in *cambricon) init() {
	_ = in.ensureInit()
}

func (in *cambricon) ensureInit() cndev.Return {
	in.initMu.Lock()
	defer in.initMu.Unlock()

	if in.initialized {
		return cndev.SUCCESS
	}

	ret := in.cndev.Init()
	if ret.IsSuccess() {
		in.initialized = true
		return cndev.SUCCESS
	}

	in.logger.Error(ret, "failed to initialize CNDev library")
	return ret
}

// isUnqueryableDeviceError reports whether a non-success CNDev return indicates a card-specific
// hardware or bus fault where skipping the affected card allows healthy sibling cards to remain
// queryable.
func isUnqueryableDeviceError(ret cndev.Return) bool {
	switch ret {
	case cndev.ERROR_INVALID_DEVICE_ID,
		cndev.ERROR_UNKNOWN,
		cndev.ERROR_INVALID_LINK,
		cndev.ERROR_NOT_FOUND,
		cndev.ERROR_IN_USE,
		cndev.ERROR_IN_PROBLEM:
		return true
	default:
		return false
	}
}

// classifyInitOrCountError distinguishes an absent driver or library (or zero-card Init return) on
// a node without detected PCI devices from an operational failure that must fail the pass.
func classifyInitOrCountError(op string, ret cndev.Return, noPciCheck bool, pciDevCount int) error {
	if ret.IsSuccess() {
		return nil
	}
	if (ret.IsAPIUnavailable() || ret == cndev.ERROR_NO_DEVICES) && (noPciCheck || pciDevCount == 0) {
		return nil
	}
	return fmt.Errorf("failed to %s: %w", op, ret)
}

// handleCardQueryError classifies a fatal per-card query error. Card-specific unqueryable errors
// are logged, recorded in lastUnqueryableErr, and skipped; other errors fail the pass immediately.
func handleCardQueryError(
	logger klog.Logger, index int, op string, ret cndev.Return, lastUnqueryableErr *error,
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

func isDeviceUnhealthy(dev cndev.Device) bool {
	healthHandler := dev.GetCardHealthStateV()
	return evaluateDeviceUnhealthy(healthHandler.V2, healthHandler.V1)
}

// evaluateDeviceUnhealthy reports whether the vendor's card health state (V2 with V1 fallback)
// makes the accelerator unhealthy. A query the driver could not answer is not a fault, so it reads healthy.
func evaluateDeviceUnhealthy(
	readV2 func() (cndev.CardHealthStateV2, cndev.Return),
	readV1 func() (cndev.CardHealthStateV2, cndev.Return),
) bool {
	if healthInfo, ret := readV2(); ret.IsSuccess() {
		return healthInfo.Health == 0
	}
	if healthInfo, ret := readV1(); ret.IsSuccess() {
		return healthInfo.Health == 0
	}
	return false
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
