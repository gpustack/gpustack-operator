package rsmi

import (
	"fmt"
	"slices"
	"strconv"

	"gpustack.ai/gpustack/binding"
)

// GetDeviceCount retrieves the number of devices available.
func (l *RSMI) GetDeviceCount() (int, Return) {
	if l.so.Lookup("rsmi_num_monitor_devices") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	var count uint32
	ret := rsmiNumMonitorDevices(&count)
	return int(count), ret
}

// GetDeviceHandleByIndex retrieves a handle for the device at the specified index.
func (l *RSMI) GetDeviceHandleByIndex(index int) Device {
	return Device{index: uint32(index), so: l.so}
}

type Device struct {
	index uint32
	so    binding.Library
}

// GetUniqueId retrieves the unique identifier of the device.
func (l Device) GetUniqueId() (string, Return) {
	if l.so.Lookup("rsmi_dev_unique_id_get") != nil {
		return "", STATUS_FUNCTION_NOT_FOUND
	}

	var id uint64
	ret := rsmiDevUniqueIdGet(l.index, &id)
	if !ret.IsSuccess() {
		return "", ret
	}
	return "GPU-" + strconv.FormatUint(id, 16), ret
}

const MAX_NAME_LENGTH = 256

// GetName retrieves the name of the device.
func (l Device) GetName() (string, Return) {
	if l.so.Lookup("rsmi_dev_name_get") != nil {
		return "", STATUS_FUNCTION_NOT_FOUND
	}

	name := make([]byte, MAX_NAME_LENGTH)
	ret := rsmiDevNameGet(l.index, &name[0], MAX_NAME_LENGTH)
	return string(name[:clen(name)]), ret
}

// GetTargetGraphicsVersion retrieves the target graphics version of the device.
func (l Device) GetTargetGraphicsVersion() (string, Return) {
	if l.so.Lookup("rsmi_dev_target_graphics_version_get") != nil {
		return "", STATUS_FUNCTION_NOT_FOUND
	}

	var version uint64
	ret := rsmiDevTargetGraphicsVersionGet(l.index, &version)
	if !ret.IsSuccess() {
		return "", ret
	}
	if version < 2000 {
		return fmt.Sprintf("gfx%d", version), ret
	}
	return fmt.Sprintf("gfx%x", version), ret
}

// GetPciId retrieves the PCI ID of the device and formats it as a string in the format "domain:bus:device.function".
func (l Device) GetPciId() (string, Return) {
	if l.so.Lookup("rsmi_dev_pci_id_get") != nil {
		return "", STATUS_FUNCTION_NOT_FOUND
	}

	var id uint64
	ret := rsmiDevPciIdGet(l.index, &id)
	if !ret.IsSuccess() {
		return "", ret
	}
	//  # BDFID = ((DOMAIN & 0xFFFFFFFF) << 32) | ((Partition & 0xF) << 28)
	//    #         | ((BUS & 0xFF) << 8) | ((DEVICE & 0x1F) <<3 )
	//    #         | (FUNCTION & 0x7)
	domain := (id >> 32) & 0xFFFFFFFF
	bus := (id >> 8) & 0xFF
	deviceId := (id >> 3) & 0x1F
	function := id & 0x7
	return fmt.Sprintf("%04x:%02x:%02x.%x", domain, bus, deviceId, function), ret
}

// GetBusyPercent retrieves the busy percentage of the device.
func (l Device) GetBusyPercent() (uint32, Return) {
	if l.so.Lookup("rsmi_dev_busy_percent_get") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	var busy uint32
	ret := rsmiDevBusyPercentGet(l.index, &busy)
	return busy, ret
}

const MAX_TEMP_SENSORS = 8

// GetTempMetric retrieves the specified temperature metric for the device.
// It iterates through all available temperature sensors and returns the first successful reading of the specified metric.
// If no sensor provides a successful reading, it returns a not found status.
func (l Device) GetTempMetric(metric TemperatureMetric) (int64, Return) {
	if l.so.Lookup("rsmi_dev_temp_metric_get") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	for i := uint32(0); i < MAX_TEMP_SENSORS; i++ {
		var temp int64
		ret := rsmiDevTempMetricGet(l.index, i, metric, &temp)
		if ret.IsSuccess() {
			return temp, ret
		}
	}
	return 0, STATUS_NOT_FOUND
}

// GetMemoryTotal retrieves the total memory of the device for the specified memory type.
func (l Device) GetMemoryTotal(memType MemoryType) (uint64, Return) {
	if l.so.Lookup("rsmi_dev_memory_total_get") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	var total uint64
	ret := rsmiDevMemoryTotalGet(l.index, memType, &total)
	return total, ret
}

// GetMemoryUsage retrieves the used memory of the device for the specified memory type.
func (l Device) GetMemoryUsage(memType MemoryType) (uint64, Return) {
	if l.so.Lookup("rsmi_dev_memory_usage_get") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	var used uint64
	ret := rsmiDevMemoryUsageGet(l.index, memType, &used)
	return used, ret
}

// GetEccCount retrieves the ECC error count of the device for the specified GPU block.
func (l Device) GetEccCount(block GpuBlock) (ErrorCount, Return) {
	if l.so.Lookup("rsmi_dev_ecc_count_get") != nil {
		return ErrorCount{}, STATUS_FUNCTION_NOT_FOUND
	}

	var count ErrorCount
	ret := rsmiDevEccCountGet(l.index, block, &count)
	return count, ret
}

// GetPowerCap retrieves the power cap of the device.
func (l Device) GetPowerCap() (uint64, Return) {
	if l.so.Lookup("rsmi_dev_power_cap_get") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	var powerCap uint64
	ret := rsmiDevPowerCapGet(l.index, 0, &powerCap)
	return powerCap, ret
}

// GetPower retrieves the current power consumption of the device for the average power type.
func (l Device) GetPower() (uint64, Return) {
	if l.so.Lookup("rsmi_dev_power_get") == nil {
		var (
			power     uint64
			powerType = AVERAGE_POWER
		)
		ret := rsmiDevPowerGet(l.index, &power, &powerType)
		if ret.IsSuccess() {
			return power, ret
		}
	}
	if l.so.Lookup("rsmi_dev_power_ave_get") == nil {
		var power uint64
		ret := rsmiDevPowerAveGet(l.index, 0, &power)
		return power, ret
	}
	return 0, STATUS_FUNCTION_NOT_FOUND
}

// GetNumaNodeNumber retrieves the NUMA node number associated with the device.
func (l Device) GetNumaNodeNumber() (uint32, Return) {
	if l.so.Lookup("rsmi_topo_get_numa_node_number") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	var node uint32
	ret := rsmiTopoGetNumaNodeNumber(l.index, &node)
	return node, ret
}

// GetLinkType retrieves the link type and number of hops between two GPU devices.
func (l Device) GetLinkType(device2 Device) (uint64, IoLinkType, Return) {
	if l.so.Lookup("rsmi_topo_get_link_type") != nil {
		return 0, 0, STATUS_FUNCTION_NOT_FOUND
	}

	var (
		linkHops uint64
		linkType IoLinkType
	)
	ret := rsmiTopoGetLinkType(l.index, device2.index, &linkHops, &linkType)
	return linkHops, linkType, ret
}

// maxProcessQueryAttempts bounds a count-then-fill read. The row count the library reports can grow
// between the probe and the fill as processes come and go, so a fill that comes back short is
// retried at the size the library then asks for. The bound is what makes the retry terminate, and
// exhausting it reports the insufficiency instead of the buffer the last attempt filled: a
// truncated list read as a complete one turns processes that exist into processes that do not.
const maxProcessQueryAttempts = 3

// maxProcessRows caps how many rows a per-process read will allocate for. The count comes from the
// library, and a library whose ABI does not match the header it was generated against — or one that
// is simply broken — can report a count that has nothing to do with any process list; allocating it
// would take the whole process down before the attempt bound ever came into play. No real node
// comes near this many rows: it is thousands of processes on a single accelerator, well past what a
// node's own process table holds. A count past the ceiling is treated as a read that cannot be
// completed, exactly like a buffer the library keeps outgrowing.
const maxProcessRows uint32 = 4096

// readProcessRows runs the count-then-fill protocol of a per-process query to completion and
// returns only a complete list. fill is called with no rows to probe the count, then with a sized
// buffer to fill it, and reports through the count how many rows the library wrote or now needs.
//
// The returned rows are exactly what the library wrote, in the library's own units.
func readProcessRows[T any](fill func(count *uint32, rows []T) Return) ([]T, Return) {
	// A probe with no buffer reports how many rows the library holds.
	var count uint32
	if ret := fill(&count, nil); !ret.IsSuccess() && ret != STATUS_INSUFFICIENT_SIZE {
		return nil, ret
	}

	for range maxProcessQueryAttempts {
		if count == 0 {
			return nil, STATUS_SUCCESS
		}
		// Refuse a count no process list can have rather than allocating it.
		if count > maxProcessRows {
			return nil, STATUS_INSUFFICIENT_SIZE
		}

		rows := make([]T, count)
		written := count
		ret := fill(&written, rows)
		switch {
		case ret == STATUS_INSUFFICIENT_SIZE:
			// The list grew between the probe and the fill. Retry at the size the library now
			// asks for; growth has to be strict for the retry to make progress, so a library
			// that repeats the size we just tried is stepped past rather than tried again.
			if written > count {
				count = written
			} else {
				count++
			}
		case !ret.IsSuccess():
			return nil, ret
		default:
			// Clamp to the buffer we sized, so a library reporting more rows written than it was
			// given cannot drive a slice-out-of-range panic.
			if written > count {
				written = count
			}
			return rows[:written], ret
		}
	}

	return nil, STATUS_INSUFFICIENT_SIZE
}

// kfdStatsInvalid is KFD_STATS_INVALID as defined in rocm_smi.h: the value rsmi_process_info_t's
// cu_occupancy field carries on a GFX revision that does not provide the debugfs method the
// library would need to measure it. It is not generated into const.go because the header
// documents it beside the struct field rather than among the library's numbered constants.
const kfdStatsInvalid = 0xFFFFFFFF

// CuOccupancyAvailable reports whether Cu_occupancy holds a percentage at all.
//
// RSMI writes kfdStatsInvalid into the field when the GFX revision does not provide the debugfs
// method it would need to measure compute-unit occupancy. That is a sentinel, not a number: the
// field is unsigned, so read as a number it is the largest percentage representable, and a caller
// that compares or averages it reports an unmeasurable process as a fully-occupied one.
func (p ProcessInfo) CuOccupancyAvailable() bool {
	return p.Cu_occupancy != kfdStatsInvalid
}

// GetComputeProcessInfo returns one row per compute process running on this device, in the
// library's own units: Vram_usage and Sdma_usage as the library reports them, and Cu_occupancy as
// compute-unit usage in percent, which is the header's own wording — a share, not a count of units
// — see CuOccupancyAvailable for the sentinel it carries when a GFX revision cannot measure it,
// which is never rewritten to zero here. Nothing is converted here.
//
// Pids are host pids: a containerized process is named by the pid the host sees, not the pid it
// sees itself, and no translation happens here.
//
// This takes three queries because the library has no per-device process list. The first enumerates
// every compute process on the host. Each of those pids is then asked which device indices it is
// using, and only a pid whose answer contains this device's index is asked for its figures on this
// device. Membership has to be established that way round: the per-device figures query documents no
// return code for a pid that is not on the device at all, so its answer cannot be used to decide
// whether the pid belongs here, and treating it as if it could would attribute every compute process
// on the host to every device on it.
//
// The result is either complete or an error, never partial: an empty list with success means this
// device has no compute processes, and any pid whose membership or figures the library will not
// answer for fails the whole call rather than being dropped from the list. A pid that exits between
// the enumeration and the questions about it therefore fails the call too, which the caller's next
// sample repairs — a list with an unknown number of holes cannot be told from a complete one.
func (l Device) GetComputeProcessInfo() ([]ProcessInfo, Return) {
	if l.so.Lookup("rsmi_compute_process_info_get") != nil ||
		l.so.Lookup("rsmi_compute_process_gpus_get") != nil ||
		l.so.Lookup("rsmi_compute_process_info_by_device_get") != nil {
		return nil, STATUS_FUNCTION_NOT_FOUND
	}

	hostProcs, ret := readProcessRows(func(count *uint32, rows []ProcessInfo) Return {
		if len(rows) == 0 {
			return rsmiComputeProcessInfoGet(nil, count)
		}
		return rsmiComputeProcessInfoGet(&rows[0], count)
	})
	if !ret.IsSuccess() {
		return nil, ret
	}

	var rows []ProcessInfo
	for i := range hostProcs {
		pid := hostProcs[i].Process_id

		// Which devices this pid is using. The same count-then-fill protocol as the process list,
		// so a list that grows underneath the read is retried rather than half-read.
		indices, ret := readProcessRows(func(count *uint32, rows []uint32) Return {
			if len(rows) == 0 {
				return rsmiComputeProcessGpusGet(pid, nil, count)
			}
			return rsmiComputeProcessGpusGet(pid, &rows[0], count)
		})
		if !ret.IsSuccess() {
			return nil, ret
		}
		if !slices.Contains(indices, l.index) {
			continue
		}

		var proc ProcessInfo
		if ret := rsmiComputeProcessInfoByDeviceGet(pid, l.index, &proc); !ret.IsSuccess() {
			return nil, ret
		}
		rows = append(rows, proc)
	}
	return rows, STATUS_SUCCESS
}
