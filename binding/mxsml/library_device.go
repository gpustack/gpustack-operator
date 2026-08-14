package mxsml

import "C"
import (
	"unsafe"

	"gpustack.ai/gpustack/binding"
)

// GetDeviceCount retrieves the number of devices available.
func (l *MXSML) GetDeviceCount() int {
	if l.so.Lookup("mxSmlGetDeviceCount") != nil {
		return 0
	}

	return int(mxSmlGetDeviceCount())
}

// GetDeviceHandleByIndex retrieves a handle for the device at the specified index.
func (l *MXSML) GetDeviceHandleByIndex(index int) Device {
	return Device{index: uint32(index), so: l.so}
}

type Device struct {
	index uint32
	so    binding.Library
}

// GetMemoryInfo retrieves the memory information of the device.
func (l Device) GetMemoryInfo() (MemoryInfo, Return) {
	if l.so.Lookup("mxSmlGetMemoryInfo") != nil {
		return MemoryInfo{}, FunctionNotFound
	}

	var info MemoryInfo
	ret := mxSmlGetMemoryInfo(l.index, &info)
	return info, ret
}

// GetPcieInfo retrieves the PCIe information of the device.
func (l Device) GetPcieInfo() (PcieInfo, Return) {
	if l.so.Lookup("mxSmlGetPcieInfo") != nil {
		return PcieInfo{}, FunctionNotFound
	}

	var info PcieInfo
	ret := mxSmlGetPcieInfo(l.index, &info)
	return info, ret
}

// GetNodeAffinity retrieves the NUMA node affinity of the device.
func (l Device) GetNodeAffinity() ([]uint32, Return) {
	if l.so.Lookup("mxSmlGetNodeAffinity") != nil {
		return nil, FunctionNotFound
	}

	nodeSetSize := uint32(binding.GetNumaNodeSetSize())
	nodeSet := make([]uint32, nodeSetSize)
	ret := mxSmlGetNodeAffinity(l.index, nodeSetSize, &nodeSet[0])
	return nodeSet, ret
}

// GetTemperatureInfo retrieves the temperature information of the device for the specified sensors.
func (l Device) GetTemperatureInfo(sensors TemperatureSensors) (int32, Return) {
	if l.so.Lookup("mxSmlGetTemperatureInfo") != nil {
		return 0, FunctionNotFound
	}

	var temp int32
	ret := mxSmlGetTemperatureInfo(l.index, sensors, &temp)
	return temp, ret
}

// GetBoardPowerLimit retrieves the current power limit of the device.
func (l Device) GetBoardPowerLimit() (uint32, Return) {
	if l.so.Lookup("mxSmlGetBoardPowerLimit") != nil {
		return 0, FunctionNotFound
	}

	var powerLimit uint32
	ret := mxSmlGetBoardPowerLimit(l.index, &powerLimit)
	return powerLimit, ret
}

// GetBoardPowerInfo retrieves the power information of the device for each power way.
func (l Device) GetBoardPowerInfo() ([]BoardWayElectricInfo, Return) {
	if l.so.Lookup("mxSmlGetBoardPowerInfo") != nil {
		return nil, FunctionNotFound
	}

	var count uint32
	info := make([]BoardWayElectricInfo, 1)
	ret := mxSmlGetBoardPowerInfo(l.index, &count, &info[0])
	if ret == InsufficientSize {
		info = make([]BoardWayElectricInfo, count)
		ret = mxSmlGetBoardPowerInfo(l.index, &count, &info[0])
		return info[:count], ret
	}
	return nil, ret
}

func (info DeviceInfo) GetBusId() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.BdfId[0])))
}

func (info DeviceInfo) GetDeviceName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.DeviceName[0])))
}

func (info DeviceInfo) GetUUID() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Uuid[0])))
}

// GetInfo retrieves the general information of the device.
func (l Device) GetInfo() (DeviceInfo, Return) {
	if l.so.Lookup("mxSmlGetDeviceInfo") != nil {
		return DeviceInfo{}, FunctionNotFound
	}

	var info DeviceInfo
	ret := mxSmlGetDeviceInfo(l.index, &info)
	return info, ret
}

// GetIpUsage retrieves the usage information of the specified IP block of the device.
func (l Device) GetIpUsage(ip UsageIp) (int32, Return) {
	if l.so.Lookup("mxSmlGetDeviceIpUsage") != nil {
		return 0, FunctionNotFound
	}

	var usage int32
	ret := mxSmlGetDeviceIpUsage(l.index, ip, &usage)
	return usage, ret
}

// GetTotalEccErrors retrieves the ECC error information of the device for the specified error type and location.
func (l Device) GetTotalEccErrors() (EccErrorCount, Return) {
	if l.so.Lookup("mxSmlGetTotalEccErrors") != nil {
		return EccErrorCount{}, FunctionNotFound
	}

	var count EccErrorCount
	ret := mxSmlGetTotalEccErrors(l.index, &count)
	return count, ret
}

// GetDeviceTopology retrieves the topology level between this device and another device.
func (l Device) GetDeviceTopology(device2 Device) (GpuTopologyLevel, Return) {
	if l.so.Lookup("mxSmlGetDeviceTopology") != nil {
		return 0, FunctionNotFound
	}

	var topology GpuTopologyLevel
	ret := mxSmlGetDeviceTopology(l.index, device2.index, &topology)
	return topology, ret
}

// maxProcessQueryAttempts bounds a count-then-fill read. The number of processes can grow between
// the count and the fill, so a fill that comes back full is retried at a larger size. The bound is
// what makes the retry terminate, and exhausting it reports the insufficiency instead of the buffer
// the last attempt filled: a truncated list read as a complete one turns processes that exist into
// processes that do not.
const maxProcessQueryAttempts = 3

// maxProcessRows caps how many rows a per-process read will allocate for. The count comes from the
// library, and a library whose ABI does not match the header it was generated against — or one that
// is simply broken — can report a count that has nothing to do with any process list; allocating it
// would take the whole process down before the attempt bound ever came into play. No real node comes
// near this many rows: it is thousands of processes on a single accelerator, well past what a node's
// own process table holds. A count past the ceiling is treated as a read that cannot be completed,
// exactly like a buffer the library keeps outgrowing.
const maxProcessRows uint32 = 4096

// sgpuDisabled is the sgpu id the library writes when a device is not in sgpu mode (MxSml.h). The
// older layouts of the process query carry no sgpu id at all, so their rows report this rather than
// sgpu 0, which is a real slice.
const sgpuDisabled int32 = -1

// processFiller fills rows, which is never empty, with the processes of one device and reports
// through count how many rows it wrote.
type processFiller func(count *uint32, rows []ProcessInfo_v3) Return

// GetProcessInfo returns one row per process running on this device, each carrying the per-GPU
// entries the library reports for that process — GpuMemoryUsage in the library's own units, and
// GpuNumber as the count of entries it filled. Nothing is converted or collapsed here.
//
// Pids are host pids: a containerized process is named by the pid the host sees, not the pid it
// sees itself, and no translation happens here.
//
// This uses the per-device form of the query rather than the whole-host one. The host form takes
// its buffer size as an input only and reports nothing back, so there is no way to tell a full
// buffer from a truncated read; the per-device form reports how many rows it wrote, which is what
// makes a truncated read detectable and therefore reportable.
//
// A buffer the library outgrows is never returned as a list. A fill that writes as many rows as it
// was given may have had more to write, so it is retried larger, and a list that keeps filling
// every buffer through the retries is reported as insufficient-size rather than shortened.
func (l Device) GetProcessInfo() ([]ProcessInfo_v3, Return) {
	if l.so.Lookup("mxSmlGetNumberOfProcess") != nil {
		return nil, FunctionNotFound
	}
	fill := l.processFiller()
	if fill == nil {
		return nil, FunctionNotFound
	}

	// The library counts the processes using any GPU, which is an upper bound for this one.
	var count uint32
	if ret := mxSmlGetNumberOfProcess(&count); !ret.IsSuccess() {
		return nil, ret
	}

	for range maxProcessQueryAttempts {
		if count == 0 {
			return nil, Success
		}
		// Refuse a count no process list can have rather than allocating it.
		if count > maxProcessRows {
			return nil, InsufficientSize
		}

		// One row of slack past the count asked for: if the library fills every row it was given,
		// it may have had more to write, and that is the only truncation signal this query has.
		size := count + 1
		rows := make([]ProcessInfo_v3, size)
		written := size
		ret := fill(&written, rows)
		switch {
		case ret == InsufficientSize || written >= size:
			// Retry larger. Growth has to be strict for the retry to make progress, so a library
			// that reports no more than the size we just tried is stepped past.
			if written > count {
				count = written
			} else {
				count++
			}
		case !ret.IsSuccess():
			return nil, ret
		default:
			return rows[:written], ret
		}
	}

	return nil, InsufficientSize
}

// processFiller returns a filler over the newest layout of the per-device process query the
// installed library carries, or nil when it carries none. The older layouts are read into their own
// buffers and copied field by field, so a driver missing the newest symbol degrades rather than
// fails, and a field an older layout does not carry is reported as not applying rather than as zero.
func (l Device) processFiller() processFiller {
	switch {
	case l.so.Lookup("mxSmlGetSingleGpuProcess_v3") == nil:
		return func(count *uint32, rows []ProcessInfo_v3) Return {
			return mxSmlGetSingleGpuProcess_v3(l.index, count, &rows[0])
		}
	case l.so.Lookup("mxSmlGetSingleGpuProcess_v2") == nil:
		return func(count *uint32, rows []ProcessInfo_v3) Return {
			buf := make([]ProcessInfo_v2, len(rows))
			ret := mxSmlGetSingleGpuProcess_v2(l.index, count, &buf[0])
			if !ret.IsSuccess() {
				return ret
			}
			for i := range buf[:min(int(*count), len(buf))] {
				rows[i] = ProcessInfo_v3{
					ProcessId:   buf[i].ProcessId,
					ProcessName: buf[i].ProcessName,
					GpuNumber:   buf[i].GpuNumber,
				}
				for j := range buf[i].ProcessGpuInfo {
					rows[i].ProcessGpuInfo[j] = ProcessGpuInfo_v3{
						BdfId:          buf[i].ProcessGpuInfo[j].BdfId,
						GpuId:          buf[i].ProcessGpuInfo[j].GpuId,
						GpuMemoryUsage: buf[i].ProcessGpuInfo[j].GpuMemoryUsage,
						DieId:          buf[i].ProcessGpuInfo[j].DieId,
						SgpuId:         sgpuDisabled,
					}
				}
			}
			return ret
		}
	case l.so.Lookup("mxSmlGetSingleGpuProcess") == nil:
		return func(count *uint32, rows []ProcessInfo_v3) Return {
			buf := make([]ProcessInfo, len(rows))
			ret := mxSmlGetSingleGpuProcess(l.index, count, &buf[0])
			if !ret.IsSuccess() {
				return ret
			}
			for i := range buf[:min(int(*count), len(buf))] {
				rows[i] = ProcessInfo_v3{
					ProcessId:   buf[i].ProcessId,
					ProcessName: buf[i].ProcessName,
					GpuNumber:   buf[i].GpuNumber,
				}
				for j := range buf[i].ProcessGpuInfo {
					// This layout carries no die id either, and the library has no value for a
					// die that does not apply, so it stays zero on a driver this old.
					rows[i].ProcessGpuInfo[j] = ProcessGpuInfo_v3{
						BdfId:          buf[i].ProcessGpuInfo[j].BdfId,
						GpuId:          buf[i].ProcessGpuInfo[j].GpuId,
						GpuMemoryUsage: buf[i].ProcessGpuInfo[j].GpuMemoryUsage,
						SgpuId:         sgpuDisabled,
					}
				}
			}
			return ret
		}
	}
	return nil
}
