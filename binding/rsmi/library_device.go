package rsmi

import (
	"fmt"
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
