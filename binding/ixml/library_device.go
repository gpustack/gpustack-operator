package ixml

import (
	"fmt"

	"gpustack.ai/gpustack/binding"
)

// DeviceGetCount retrieves the number of NVIDIA devices in the system.
func (l *IXML) DeviceGetCount() (int, Return) {
	if l.so.Lookup("nvmlDeviceGetCount_v2") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var deviceCount uint32
	ret := nvmlDeviceGetCount_v2(&deviceCount)
	return int(deviceCount), ret
}

// DeviceGetHandleByIndex retrieves a handle for the device at the specified index.
func (l *IXML) DeviceGetHandleByIndex(index int) (Device, Return) {
	if l.so.Lookup("nvmlDeviceGetHandleByIndex_v2") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle nvmlDevice
	ret := nvmlDeviceGetHandleByIndex_v2(uint32(index), &handle)
	return Device{handle: handle, so: l.so}, ret
}

// DeviceGetHandleByPciBusId retrieves a handle for the device with the specified PCI bus ID.
func (l *IXML) DeviceGetHandleByPciBusId(pciBusId string) (Device, Return) {
	if l.so.Lookup("nvmlDeviceGetHandleByPciBusId_v2") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle nvmlDevice
	ret := nvmlDeviceGetHandleByPciBusId_v2(pciBusId+string(rune(0)), &handle)
	return Device{handle: handle, so: l.so}, ret
}

// DeviceGetHandleByUUID retrieves a handle for the device with the specified UUID.
func (l *IXML) DeviceGetHandleByUUID(uuid string) (Device, Return) {
	if l.so.Lookup("nvmlDeviceGetHandleByUUID") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle nvmlDevice
	ret := nvmlDeviceGetHandleByUUID(uuid+string(rune(0)), &handle)
	return Device{handle: handle, so: l.so}, ret
}

type Device struct {
	handle nvmlDevice
	so     binding.Library
}

// GetCudaComputeCapability retrieves the CUDA compute capability of the device, returning the major and minor version numbers.
func (l Device) GetCudaComputeCapability() (int32, int32, Return) {
	if l.so.Lookup("nvmlDeviceGetCudaComputeCapability") != nil {
		return 0, 0, ERROR_FUNCTION_NOT_FOUND
	}

	var major, minor int32
	ret := nvmlDeviceGetCudaComputeCapability(l.handle, &major, &minor)
	return major, minor, ret
}

func (info PciInfo) GetBusId() string {
	return fmt.Sprintf("%04x:%02x:%02x.0", info.Domain, info.Bus, info.Device)
}

// GetPciInfo retrieves the PCI information of the device, including bus ID, domain ID, and device ID.
func (l Device) GetPciInfo() (PciInfo, Return) {
	if l.so.Lookup("nvmlDeviceGetPciInfo_v3") != nil {
		return PciInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var pci PciInfo
	ret := nvmlDeviceGetPciInfo_v3(l.handle, &pci)
	return pci, ret
}

// GetTemperature retrieves the temperature information of the device.
func (l Device) GetTemperature() (uint32, Return) {
	if l.so.Lookup("nvmlDeviceGetTemperature") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var temp uint32
	ret := nvmlDeviceGetTemperature(l.handle, TEMPERATURE_GPU, &temp)
	return temp, ret
}

// GetPowerManagementDefaultLimit retrieves the default power management limit of the device in milliwatts.
func (l Device) GetPowerManagementDefaultLimit() (uint32, Return) {
	if l.so.Lookup("nvmlDeviceGetPowerManagementDefaultLimit") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var defaultLimit uint32
	ret := nvmlDeviceGetPowerManagementDefaultLimit(l.handle, &defaultLimit)
	return defaultLimit, ret
}

// GetPowerUsage retrieves the current power usage of the device in milliwatts.
func (l Device) GetPowerUsage() (uint32, Return) {
	if l.so.Lookup("nvmlDeviceGetPowerUsage") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var power uint32
	ret := nvmlDeviceGetPowerUsage(l.handle, &power)
	return power, ret
}

// GetMinorNumber retrieves the minor number of the device, which is used to identify the device in the system.
func (l Device) GetMinorNumber() (uint32, Return) {
	if l.so.Lookup("nvmlDeviceGetMinorNumber") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var minorNumber uint32
	ret := nvmlDeviceGetMinorNumber(l.handle, &minorNumber)
	return minorNumber, ret
}

// GetName retrieves the name of the device, which typically includes the GPU model and other identifying information.
func (l Device) GetName() (string, Return) {
	if l.so.Lookup("nvmlDeviceGetName") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	name := make([]byte, DEVICE_NAME_BUFFER_SIZE)
	ret := nvmlDeviceGetName(l.handle, &name[0], DEVICE_NAME_BUFFER_SIZE)
	return string(name[:clen(name)]), ret
}

// GetUUID retrieves the universally unique identifier (UUID) of the device,
func (l Device) GetUUID() (string, Return) {
	if l.so.Lookup("nvmlDeviceGetUUID") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	uuid := make([]byte, DEVICE_UUID_BUFFER_SIZE)
	ret := nvmlDeviceGetUUID(l.handle, &uuid[0], DEVICE_UUID_BUFFER_SIZE)
	return string(uuid[:clen(uuid)]), ret
}

// GetUtilizationRates retrieves the current utilization rates of the device, including GPU and memory utilization,
func (l Device) GetUtilizationRates() (Utilization, Return) {
	if l.so.Lookup("nvmlDeviceGetUtilizationRates") != nil {
		return Utilization{}, ERROR_FUNCTION_NOT_FOUND
	}

	var utilization Utilization
	ret := nvmlDeviceGetUtilizationRates(l.handle, &utilization)
	return utilization, ret
}

// GetMemoryInfoV retrieves the memory information of the device, including total, used, and free memory,
// returning a handler that can be used to access different versions of the memory information.
func (l Device) GetMemoryInfoV() MemoryInfoHandler {
	return MemoryInfoHandler(l)
}

type MemoryInfoHandler Device

func (l MemoryInfoHandler) V1() (Memory_v2, Return) {
	if l.so.Lookup("nvmlDeviceGetMemoryInfo") != nil {
		return Memory_v2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var memory Memory
	ret := nvmlDeviceGetMemoryInfo(l.handle, &memory)
	return Memory_v2{
		Total: memory.Total,
		Used:  memory.Used,
		Free:  memory.Free,
	}, ret
}

func (l MemoryInfoHandler) V2() (Memory_v2, Return) {
	if l.so.Lookup("nvmlDeviceGetMemoryInfo_v2") != nil {
		return Memory_v2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var memory Memory_v2
	memory.Version = STRUCT_VERSION(memory, 2)
	ret := nvmlDeviceGetMemoryInfo_v2(l.handle, &memory)
	return memory, ret
}

// GetHealth retrieves the health status of the device, returning a bitmask that indicates any health issues detected on the device.
func (l Device) GetHealth() (uint64, Return) {
	if l.so.Lookup("ixmlDeviceGetHealth") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var health uint64
	ret := ixmlDeviceGetHealth(l.handle, &health)
	return health, ret
}

// GetTopologyCommonAncestor retrieves the common ancestor in the GPU topology between the current device and another specified device,
func (l Device) GetTopologyCommonAncestor(device2 Device) (GpuTopologyLevel, Return) {
	if l.so.Lookup("nvmlDeviceGetTopologyCommonAncestor") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var pathInfo GpuTopologyLevel
	ret := nvmlDeviceGetTopologyCommonAncestor(l.handle, device2.handle, &pathInfo)
	return pathInfo, ret
}

// maxProcessQueryAttempts bounds a count-then-fill read. The row count a driver reports can grow
// between the probe and the fill as processes come and go, so a fill that comes back short is
// retried at the size the driver then asks for. The bound is what makes the retry terminate, and
// exhausting it reports the insufficiency instead of the buffer the last attempt filled: a
// truncated list read as a complete one turns processes that exist into processes that do not.
const maxProcessQueryAttempts = 3

// maxProcessRows caps how many rows a per-process read will allocate for. The count comes from the
// driver, and a driver whose ABI does not match the header it was generated against — or one that
// is simply broken — can report a count that has nothing to do with any process list; allocating it
// would take the whole process down before the attempt bound ever came into play. No real node
// comes near this many rows: it is thousands of processes on a single accelerator, well past what a
// node's own process table holds. A count past the ceiling is treated as a read that cannot be
// completed, exactly like a buffer the driver keeps outgrowing.
const maxProcessRows uint32 = 4096

// readProcessRows runs the count-then-fill protocol of a per-process query to completion and
// returns only a complete list. fill is called with no rows to probe the count, then with a sized
// buffer to fill it, and reports through the count how many rows the driver wrote or now needs.
//
// The returned rows are exactly what the driver wrote, in the driver's own units.
func readProcessRows[T any](fill func(count *uint32, rows []T) Return) ([]T, Return) {
	// A probe with no buffer reports how many rows the driver holds. The driver answers a
	// zero-sized buffer with the short-buffer code, which is the probe working, not failing.
	var count uint32
	if ret := fill(&count, nil); !ret.IsSuccess() && ret != ERROR_INSUFFICIENT_SIZE {
		return nil, ret
	}

	for range maxProcessQueryAttempts {
		if count == 0 {
			return nil, SUCCESS
		}
		// Refuse a count no process list can have rather than allocating it.
		if count > maxProcessRows {
			return nil, ERROR_INSUFFICIENT_SIZE
		}

		rows := make([]T, count)
		written := count
		ret := fill(&written, rows)
		switch {
		case ret == ERROR_INSUFFICIENT_SIZE:
			// The list grew between the probe and the fill. Retry at the size the driver now
			// asks for; growth has to be strict for the retry to make progress, so a driver
			// that repeats the size we just tried is stepped past rather than tried again.
			if written > count {
				count = written
			} else {
				count++
			}
		case !ret.IsSuccess():
			return nil, ret
		default:
			// Clamp to the buffer we sized, so a driver reporting more rows written than it was
			// given cannot drive a slice-out-of-range panic.
			if written > count {
				written = count
			}
			return rows[:written], ret
		}
	}

	return nil, ERROR_INSUFFICIENT_SIZE
}

// GetComputeRunningProcesses returns one row per compute process running on this device, with
// UsedGpuMemory in bytes as the library reports it.
//
// Pids are host pids: a containerized process is named by the pid the host sees, not the pid it
// sees itself, and no translation happens here.
//
// The vendored header carries a single, unversioned form of this query, so the rows hold the pid
// and its memory and nothing else. There is no per-process utilization query in this library at
// all, which is why per-process compute is not merely zero on this hardware but unavailable.
func (l Device) GetComputeRunningProcesses() ([]ProcessInfo_v1, Return) {
	if l.so.Lookup("nvmlDeviceGetComputeRunningProcesses") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	return readProcessRows(func(count *uint32, rows []ProcessInfo_v1) Return {
		if len(rows) == 0 {
			return nvmlDeviceGetComputeRunningProcesses(l.handle, count, nil)
		}
		return nvmlDeviceGetComputeRunningProcesses(l.handle, count, &rows[0])
	})
}
