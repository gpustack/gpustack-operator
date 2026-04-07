package mtml

import (
	"fmt"

	"gpustack.ai/gpustack/binding"
)

// CountDevice retrieves the number of devices in the system.
func (l *MTML) CountDevice() (int, Return) {
	if l.so.Lookup("mtmlLibraryCountDevice") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var deviceCount uint32
	ret := mtmlLibraryCountDevice(l.lib, &deviceCount)
	return int(deviceCount), ret
}

// InitDeviceByIndex initializes a device handle for the device at the specified index.
func (l *MTML) InitDeviceByIndex(index int) (Device, Return) {
	if l.so.Lookup("mtmlLibraryInitDeviceByIndex") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle *mtmlDevice
	ret := mtmlLibraryInitDeviceByIndex(l.lib, uint32(index), &handle)
	return Device{handle: handle, so: l.so}, ret
}

// InitDeviceByPciSbdf initializes a device handle for the device with the specified PCI bus ID.
func (l *MTML) InitDeviceByPciSbdf(pciBusId string) (Device, Return) {
	if l.so.Lookup("mtmlLibraryInitDeviceByPciSbdf") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle *mtmlDevice
	ret := mtmlLibraryInitDeviceByPciSbdf(l.lib, pciBusId+string(rune(0)), &handle)
	return Device{handle: handle, so: l.so}, ret
}

// InitDeviceByUuid initializes a device handle for the device with the specified UUID.
func (l *MTML) InitDeviceByUuid(uuid string) (Device, Return) {
	if l.so.Lookup("mtmlLibraryInitDeviceByUuid") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle *mtmlDevice
	ret := mtmlLibraryInitDeviceByUuid(l.lib, uuid+string(rune(0)), &handle)
	return Device{handle: handle, so: l.so}, ret
}

type Device struct {
	handle *mtmlDevice
	so     binding.Library
}

// Free releases the device handle and any resources it holds.
func (l Device) Free() Return {
	if l.so.Lookup("mtmlLibraryFreeDevice") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	return mtmlLibraryFreeDevice(l.handle)
}

// GetName retrieves the name of the device.
func (l Device) GetName() (string, Return) {
	if l.so.Lookup("mtmlDeviceGetName") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	name := make([]byte, DEVICE_NAME_BUFFER_SIZE)
	ret := mtmlDeviceGetName(l.handle, &name[0], DEVICE_NAME_BUFFER_SIZE)
	return string(name[:clen(name)]), ret
}

// GetUUID retrieves the UUID of the device.
func (l Device) GetUUID() (string, Return) {
	if l.so.Lookup("mtmlDeviceGetUUID") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	uuid := make([]byte, DEVICE_UUID_BUFFER_SIZE)
	ret := mtmlDeviceGetUUID(l.handle, &uuid[0], DEVICE_UUID_BUFFER_SIZE)
	return string(uuid[:clen(uuid)]), ret
}

func (info PciInfo) GetBusId() string {
	return fmt.Sprintf("%04x:%02x:%02x.0", info.Segment, info.Bus, info.Device)
}

// GetPciInfo retrieves the PCI information for the device, including segment, bus, device, and function numbers.
func (l Device) GetPciInfo() (PciInfo, Return) {
	if l.so.Lookup("mtmlDeviceGetPciInfo") != nil {
		return PciInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var pciInfo PciInfo
	ret := mtmlDeviceGetPciInfo(l.handle, &pciInfo)
	return pciInfo, ret
}

// GetMemoryAffinityWithNode retrieves the memory affinity of the device with respect to NUMA nodes,
func (l Device) GetMemoryAffinityWithNode() ([]uint32, Return) {
	if l.so.Lookup("mtmlDeviceGetMemoryAffinityWithinNode") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	nodeSetSize := uint32(binding.GetNumaNodeSetSize())
	nodeSet := make([]uint32, nodeSetSize)
	ret := mtmlDeviceGetMemoryAffinityWithinNode(l.handle, nodeSetSize, &nodeSet[0])
	return nodeSet, ret
}

// GetTopologyLevel retrieves the topology level between this device and another specified device,
// indicating how closely they are connected in terms of system topology.
func (l Device) GetTopologyLevel(device2 Device) (DeviceTopologyLevel, Return) {
	if l.so.Lookup("mtmlDeviceGetTopologyLevel") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var level DeviceTopologyLevel
	ret := mtmlDeviceGetTopologyLevel(l.handle, device2.handle, &level)
	return level, ret
}

// GetMtLinkSpec retrieves the specifications of the MTLink connections for the device,
// including the number of links and their capabilities.
func (l Device) GetMtLinkSpec() (MtLinkSpec, Return) {
	if l.so.Lookup("mtmlDeviceGetMtLinkSpec") != nil {
		return MtLinkSpec{}, ERROR_FUNCTION_NOT_FOUND
	}

	var spec MtLinkSpec
	ret := mtmlDeviceGetMtLinkSpec(l.handle, &spec)
	return spec, ret
}

// GetMtLinkState retrieves the current state of the specified MTLink connection for the device,
// including the link's current speed and width.
func (l Device) GetMtLinkState(linkId uint32) (MtLinkState, Return) {
	if l.so.Lookup("mtmlDeviceGetMtLinkState") != nil {
		return MTLINK_STATE_DOWN, ERROR_FUNCTION_NOT_FOUND
	}

	var state MtLinkState
	ret := mtmlDeviceGetMtLinkState(l.handle, linkId, &state)
	return state, ret
}

// CountGpuCores retrieves the number of GPU cores in the device,
// which can be used to understand the computational capabilities of the GPU.
func (l Device) CountGpuCores() (uint32, Return) {
	if l.so.Lookup("mtmlDeviceCountGpuCores") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var coreCount uint32
	ret := mtmlDeviceCountGpuCores(l.handle, &coreCount)
	return coreCount, ret
}

// GetPowerUsage retrieves the power usage of the device in milliwatts,
// which can be useful for monitoring and managing power consumption.
func (l Device) GetPowerUsage() (uint32, Return) {
	if l.so.Lookup("mtmlDeviceGetPowerUsage") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var powerUsage uint32
	ret := mtmlDeviceGetPowerUsage(l.handle, &powerUsage)
	return powerUsage, ret
}

// InitGpu initializes a GPU handler for the device and retrieves the GPU temperature and utilization,
// allowing for monitoring of GPU performance and thermal conditions.
func (l Device) InitGpu() (GpuHandler, Return) {
	if l.so.Lookup("mtmlDeviceInitGpu") != nil {
		return GpuHandler{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle *mtmlGpu
	ret := mtmlDeviceInitGpu(l.handle, &handle)
	return GpuHandler{handle: handle, so: l.so}, ret
}

type GpuHandler struct {
	handle *mtmlGpu
	so     binding.Library
}

// Free releases the GPU handler and any resources it holds.
func (l GpuHandler) Free() Return {
	if l.so.Lookup("mtmlDeviceFreeGpu") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	return mtmlDeviceFreeGpu(l.handle)
}

// GetTemperature retrieves the GPU temperature in degrees Celsius.
func (l GpuHandler) GetTemperature() (uint32, Return) {
	if l.so.Lookup("mtmlGpuGetTemperature") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var temperature int32
	ret := mtmlGpuGetTemperature(l.handle, &temperature)
	return uint32(temperature), ret
}

// GetUtilization retrieves the GPU utilization as a percentage.
func (l GpuHandler) GetUtilization() (uint32, Return) {
	if l.so.Lookup("mtmlGpuGetUtilization") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var utilization uint32
	ret := mtmlGpuGetUtilization(l.handle, &utilization)
	return utilization, ret
}

// GetEngineUtilization retrieves the GPU engine utilization for the specified engine as a percentage.
func (l GpuHandler) GetEngineUtilization(engine GpuEngine) (uint32, Return) {
	if l.so.Lookup("mtmlGpuGetEngineUtilization") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var utilization uint32
	ret := mtmlGpuGetEngineUtilization(l.handle, engine, &utilization)
	return utilization, ret
}

func (l Device) InitMemory() (MemoryHandler, Return) {
	if l.so.Lookup("mtmlDeviceInitMemory") != nil {
		return MemoryHandler{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle *mtmlMemory
	ret := mtmlDeviceInitMemory(l.handle, &handle)
	return MemoryHandler{handle: handle, so: l.so}, ret
}

type MemoryHandler struct {
	handle *mtmlMemory
	so     binding.Library
}

// Free releases the memory handler and any resources it holds.
func (l MemoryHandler) Free() Return {
	if l.so.Lookup("mtmlDeviceFreeMemory") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	return mtmlDeviceFreeMemory(l.handle)
}

// GetTotal retrieves the total memory size in bytes.
func (l MemoryHandler) GetTotal() (uint64, Return) {
	if l.so.Lookup("mtmlMemoryGetTotal") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var total uint64
	ret := mtmlMemoryGetTotal(l.handle, &total)
	return total, ret
}

// GetUsed retrieves the used memory size in bytes.
func (l MemoryHandler) GetUsed() (uint64, Return) {
	if l.so.Lookup("mtmlMemoryGetUsed") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var used uint64
	ret := mtmlMemoryGetUsed(l.handle, &used)
	return used, ret
}

// GetEccErrorCounter retrieves the ECC error counter for the specified error type, counter type, and location.
func (l MemoryHandler) GetEccErrorCounter(errorType MemoryErrorType, counterType EccCounterType, location MemoryLocation) (uint64, Return) {
	if l.so.Lookup("mtmlMemoryGetEccErrorCounter") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var count uint64
	ret := mtmlMemoryGetEccErrorCounter(l.handle, errorType, counterType, location, &count)
	return count, ret
}

// GetProperty retrieves the properties of the device.
func (l Device) GetProperty() (DeviceProperty, Return) {
	if l.so.Lookup("mtmlDeviceGetProperty") != nil {
		return DeviceProperty{}, ERROR_FUNCTION_NOT_FOUND
	}

	var property DeviceProperty
	ret := mtmlDeviceGetProperty(l.handle, &property)
	return property, ret
}
