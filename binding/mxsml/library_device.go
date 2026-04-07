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
