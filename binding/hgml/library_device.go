package hgml

import (
	"fmt"
	"unsafe"

	"gpustack.ai/gpustack/binding"
)

// DeviceGetCount retrieves the number of NVIDIA devices in the system.
func (l *HGML) DeviceGetCount() (int, Return) {
	var deviceCount uint32
	if l.so.Lookup("hgmlDeviceGetCount_v2") == nil {
		ret := hgmlDeviceGetCount_v2(&deviceCount)
		return int(deviceCount), ret
	}
	if l.so.Lookup("hgmlDeviceGetCount") == nil {
		ret := hgmlDeviceGetCount(&deviceCount)
		return int(deviceCount), ret
	}
	return 0, ERROR_FUNCTION_NOT_FOUND
}

// DeviceGetHandleByIndex retrieves a handle for the device at the specified index.
func (l *HGML) DeviceGetHandleByIndex(index int) (Device, Return) {
	var handle hgmlDevice
	if l.so.Lookup("hgmlDeviceGetHandleByIndex_v2") == nil {
		ret := hgmlDeviceGetHandleByIndex_v2(uint32(index), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	if l.so.Lookup("hgmlDeviceGetHandleByIndex") == nil {
		ret := hgmlDeviceGetHandleByIndex(uint32(index), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	return Device{}, ERROR_FUNCTION_NOT_FOUND
}

// DeviceGetHandleByPciBusId retrieves a handle for the device with the specified PCI bus ID.
func (l *HGML) DeviceGetHandleByPciBusId(pciBusId string) (Device, Return) {
	var handle hgmlDevice
	if l.so.Lookup("hgmlDeviceGetHandleByPciBusId_v2") == nil {
		ret := hgmlDeviceGetHandleByPciBusId_v2(pciBusId+string(rune(0)), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	if l.so.Lookup("hgmlDeviceGetHandleByPciBusId") == nil {
		ret := hgmlDeviceGetHandleByPciBusId(pciBusId+string(rune(0)), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	return Device{}, ERROR_FUNCTION_NOT_FOUND
}

// DeviceGetHandleByUUID retrieves a handle for the device with the specified UUID.
func (l *HGML) DeviceGetHandleByUUID(uuid string) (Device, Return) {
	if l.so.Lookup("hgmlDeviceGetHandleByUUID") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle hgmlDevice
	ret := hgmlDeviceGetHandleByUUID(uuid+string(rune(0)), &handle)
	return Device{handle: handle, so: l.so}, ret
}

type Device struct {
	handle hgmlDevice
	so     binding.Library
}

// GetHggcComputeCapability retrieves the HGGC compute capability of the device,
// returning the major and minor version numbers.
func (l Device) GetHggcComputeCapability() (int32, int32, Return) {
	if l.so.Lookup("hgmlDeviceGetHggcComputeCapability") != nil {
		return 0, 0, ERROR_FUNCTION_NOT_FOUND
	}

	var major, minor int32
	ret := hgmlDeviceGetHggcComputeCapability(l.handle, &major, &minor)
	return major, minor, ret
}

// GetPciInfoV returns a handler that can be used to access different versions of the PCI information of the device.
func (l Device) GetPciInfoV() PciInfoHandler {
	return PciInfoHandler(l)
}

type PciInfoHandler Device

func (info PciInfo) GetBusId() string {
	return fmt.Sprintf("%04x:%02x:%02x.0", info.Domain, info.Bus, info.Device)
}

func (l PciInfoHandler) V2() (PciInfo, Return) {
	if l.so.Lookup("hgmlDeviceGetPciInfo_v2") != nil {
		return PciInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var pciInfo PciInfo
	ret := hgmlDeviceGetPciInfo_v2(l.handle, &pciInfo)
	return pciInfo, ret
}

func (l PciInfoHandler) V1() (PciInfo, Return) {
	if l.so.Lookup("hgmlDeviceGetPciInfo") != nil {
		return PciInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var pciInfo PciInfo
	ret := hgmlDeviceGetPciInfo(l.handle, &pciInfo)
	return pciInfo, ret
}

// GetMemoryAffinity retrieves the memory affinity of the device for a specified affinity scope,
// returning a list of NUMA node IDs.
func (l Device) GetMemoryAffinity(scope AffinityScope) ([]uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetMemoryAffinity") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	nodeSetSize := uint32(binding.GetNumaNodeSetSize())
	nodeSet := make([]uint32, nodeSetSize)
	ret := hgmlDeviceGetMemoryAffinity(l.handle, nodeSetSize, &nodeSet[0], scope)
	return nodeSet, ret
}

// GetTemperature retrieves the temperature information of the device.
func (l Device) GetTemperature() (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetTemperature") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var temp uint32
	ret := hgmlDeviceGetTemperature(l.handle, TEMPERATURE_GPU, &temp)
	return temp, ret
}

// GetPowerManagementDefaultLimit retrieves the default power management limit of the device in milliwatts.
func (l Device) GetPowerManagementDefaultLimit() (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetPowerManagementDefaultLimit") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var defaultLimit uint32
	ret := hgmlDeviceGetPowerManagementDefaultLimit(l.handle, &defaultLimit)
	return defaultLimit, ret
}

// GetPowerUsage retrieves the current power usage of the device in milliwatts.
func (l Device) GetPowerUsage() (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetPowerUsage") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var power uint32
	ret := hgmlDeviceGetPowerUsage(l.handle, &power)
	return power, ret
}

// GetMinorNumber retrieves the minor number of the device, which is used to identify the device in the system.
func (l Device) GetMinorNumber() (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetMinorNumber") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var minorNumber uint32
	ret := hgmlDeviceGetMinorNumber(l.handle, &minorNumber)
	return minorNumber, ret
}

// GetName retrieves the name of the device, which typically includes the GPU model and other identifying information.
func (l Device) GetName() (string, Return) {
	if l.so.Lookup("hgmlDeviceGetMinorNumber") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	name := make([]byte, DEVICE_NAME_V2_BUFFER_SIZE)
	ret := hgmlDeviceGetName(l.handle, &name[0], DEVICE_NAME_V2_BUFFER_SIZE)
	return string(name[:clen(name)]), ret
}

// GetUUID retrieves the universally unique identifier (UUID) of the device,
// which is a string that uniquely identifies the device across all systems.
func (l Device) GetUUID() (string, Return) {
	if l.so.Lookup("hgmlDeviceGetUUID") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	uuid := make([]byte, DEVICE_UUID_V2_BUFFER_SIZE)
	ret := hgmlDeviceGetUUID(l.handle, &uuid[0], DEVICE_UUID_V2_BUFFER_SIZE)
	return string(uuid[:clen(uuid)]), ret
}

// GetUtilizationRates retrieves the current utilization rates of the device, including GPU and memory utilization,
// returning a Utilization struct with the relevant information.
func (l Device) GetUtilizationRates() (Utilization, Return) {
	if l.so.Lookup("hgmlDeviceGetUtilizationRates") != nil {
		return Utilization{}, ERROR_FUNCTION_NOT_FOUND
	}

	var utilization Utilization
	ret := hgmlDeviceGetUtilizationRates(l.handle, &utilization)
	return utilization, ret
}

// GetNumGpuCores retrieves the number of GPU cores available on the device.
func (l Device) GetNumGpuCores() (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetNumGpuCores") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var numCores uint32
	ret := hgmlDeviceGetNumGpuCores(l.handle, &numCores)
	return numCores, ret
}

// GetMemoryInfoV retrieves the memory information of the device, including total, used, and free memory,
// returning a handler that can be used to access different versions of the memory information.
func (l Device) GetMemoryInfoV() MemoryInfoHandler {
	return MemoryInfoHandler(l)
}

type MemoryInfoHandler Device

func (l MemoryInfoHandler) V1() (Memory_v2, Return) {
	if l.so.Lookup("hgmlDeviceGetMemoryInfo") != nil {
		return Memory_v2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var memory Memory
	ret := hgmlDeviceGetMemoryInfo(l.handle, &memory)
	return Memory_v2{
		Total: memory.Total,
		Used:  memory.Used,
		Free:  memory.Free,
	}, ret
}

func (l MemoryInfoHandler) V2() (Memory_v2, Return) {
	if l.so.Lookup("hgmlDeviceGetMemoryInfo_v2") != nil {
		return Memory_v2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var memory Memory_v2
	memory.Version = STRUCT_VERSION(memory, 2)
	ret := hgmlDeviceGetMemoryInfo_v2(l.handle, &memory)
	return memory, ret
}

// GetMemoryErrorCounter retrieves the count of memory errors for the device based on the specified error type,
// ECC counter type, and memory location,
// returning the error count as a uint64 value.
func (l Device) GetMemoryErrorCounter(errorType MemoryErrorType, counterType EccCounterType, locationType MemoryLocation) (uint64, Return) {
	if l.so.Lookup("hgmlDeviceGetMemoryErrorCounter") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var count uint64
	ret := hgmlDeviceGetMemoryErrorCounter(l.handle, errorType, counterType, locationType, &count)
	return count, ret
}

// GetTopologyCommonAncestor retrieves the common ancestor in the GPU topology between the current device and another specified device,
// returning the topology level of the common ancestor.
func (l Device) GetTopologyCommonAncestor(device2 Device) (GpuTopologyLevel, Return) {
	if l.so.Lookup("hgmlDeviceGetTopologyCommonAncestor") != nil {
		return TOPOLOGY_SYSTEM, ERROR_FUNCTION_NOT_FOUND
	}

	var pathInfo GpuTopologyLevel
	ret := hgmlDeviceGetTopologyCommonAncestor(l.handle, device2.handle, &pathInfo)
	return pathInfo, ret
}

// GetGpuFabricInfoV retrieves the GPU fabric information of the device,
// returning a handler that can be used to access different versions of the GPU fabric information.
func (l Device) GetGpuFabricInfoV() GpuFabricInfoHandler {
	return GpuFabricInfoHandler(l)
}

type GpuFabricInfoHandler Device

func (l GpuFabricInfoHandler) V1() (GpuFabricInfo, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuFabricInfo") != nil {
		return GpuFabricInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var gpuFabricInfo GpuFabricInfo
	ret := hgmlDeviceGetGpuFabricInfo(l.handle, &gpuFabricInfo)
	return gpuFabricInfo, ret
}

// GetFieldValues retrieves the values of specified fields for the device,
// returning a list of FieldValue structs with the corresponding values.
func (l Device) GetFieldValues(values []FieldValue) Return {
	if l.so.Lookup("hgmlDeviceGetFieldValues") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	valuesCount := len(values)
	return hgmlDeviceGetFieldValues(l.handle, int32(valuesCount), &values[0])
}

// GetIcnLinkState retrieves the state of the ICNLink connection for a specified link index,
// returning whether the link is active or not.
func (l Device) GetIcnLinkState(link int) (EnableState, Return) {
	if l.so.Lookup("hgmlDeviceGetIcnLinkState") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var isActive EnableState
	ret := hgmlDeviceGetIcnLinkState(l.handle, uint32(link), &isActive)
	return isActive, ret
}

// GetMigMode retrieves the current and pending MIG mode of the device,
// returning the current mode, pending mode, and a Return value indicating the success or failure of the operation.
func (l Device) GetMigMode() (uint32, uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetMigMode") != nil {
		return 0, 0, ERROR_FUNCTION_NOT_FOUND
	}

	var currentMode, pendingMode uint32
	ret := hgmlDeviceGetMigMode(l.handle, &currentMode, &pendingMode)
	return currentMode, pendingMode, ret
}

// GetMaxMigDeviceCount retrieves the maximum number of MIG devices that can be created on the device,
// returning the count and a Return value indicating the success or failure of the operation.
func (l Device) GetMaxMigDeviceCount() (int, Return) {
	if l.so.Lookup("hgmlDeviceGetMaxMigDeviceCount") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var count uint32
	ret := hgmlDeviceGetMaxMigDeviceCount(l.handle, &count)
	return int(count), ret
}

type MigDevice interface {
	// GetUUID retrieves the universally unique identifier (UUID) of the device,
	// which is a string that uniquely identifies the device across all systems.
	GetUUID() (string, Return)
	// GetMemoryInfoV retrieves the memory information of the device, including total, used, and free memory,
	// returning a handler that can be used to access different versions of the memory information.
	GetMemoryInfoV() MemoryInfoHandler
	// GetMemoryErrorCounter retrieves the count of memory errors for the device based on the specified error type,
	// ECC counter type, and memory location,
	// returning the error count as a uint64 value.
	GetMemoryErrorCounter(MemoryErrorType, EccCounterType, MemoryLocation) (uint64, Return)
	// GetTopologyCommonAncestor retrieves the common ancestor in the GPU topology between the current device and another specified device,
	// returning the topology level of the common ancestor.
	GetTopologyCommonAncestor(Device) (GpuTopologyLevel, Return)
	// GetFieldValues retrieves the values of specified fields for the device,
	// returning a list of FieldValue structs with the corresponding values.
	GetFieldValues([]FieldValue) Return
	// GetGpuInstance retrieves the GPU instance associated with the device,
	// returning the GPU instance and a Return value indicating the success or failure of the operation.
	GetGpuInstance() (GpuInstance, Return)
	// GpmQueryDeviceSupportV retrieves the support information for GPU performance metrics (GPM) on the device,
	// returning a handler that can be used to access different versions of the GPM support information.
	GpmQueryDeviceSupportV() GpmSupportHandler
	// GpmMetricsGetV retrieves the performance metrics of the device for GPU performance monitoring (GPM),
	// returning a handler that can be used to access different versions of the GPM metrics information.
	GpmMetricsGetV() GpmMetricsGetHandler
}

type _MigDevice struct {
	Device
}

// GetMigDeviceHandleByIndex retrieves the handle of a MIG device based on its index,
// returning the MIG device handle and a Return value indicating the success or failure of the operation.
func (l Device) GetMigDeviceHandleByIndex(index int) (MigDevice, Return) {
	if l.so.Lookup("hgmlDeviceGetMigDeviceHandleByIndex") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	var migDevice hgmlDevice
	ret := hgmlDeviceGetMigDeviceHandleByIndex(l.handle, uint32(index), &migDevice)
	return _MigDevice{Device{handle: migDevice, so: l.so}}, ret
}

// CreateGpuInstance creates a GPU instance on the device using the specified GPU instance profile information,
// returning the created GPU instance and a Return value indicating the success or failure of the operation.
func (l Device) CreateGpuInstance(info *GpuInstanceProfileInfo) (GpuInstance, Return) {
	if info == nil {
		return GpuInstance{}, ERROR_INVALID_ARGUMENT
	}

	if l.so.Lookup("hgmlDeviceCreateGpuInstance") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstance hgmlGpuInstance
	ret := hgmlDeviceCreateGpuInstance(l.handle, info.Id, &gpuInstance)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("hgmlGpuInstanceGetInfo") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceInfo hgmlGpuInstanceInfo
	ret = hgmlGpuInstanceGetInfo(gpuInstance, &gpuInstanceInfo)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	return GpuInstance{
		device:               l.handle,
		gpuInstanceId:        gpuInstanceInfo.Id,
		gpuInstanceProfileId: gpuInstanceInfo.ProfileId,
		gpuInstancePlacement: gpuInstanceInfo.Placement,
		gpuInstance:          gpuInstance,
		so:                   l.so,
	}, ret
}

// CreateGpuInstanceWithPlacement creates a GPU instance on the device using the specified GPU instance profile information and placement,
// returning the created GPU instance and a Return value indicating the success or failure of the operation.
func (l Device) CreateGpuInstanceWithPlacement(info *GpuInstanceProfileInfo, placement *GpuInstancePlacement) (GpuInstance, Return) {
	if info == nil {
		return GpuInstance{}, ERROR_INVALID_ARGUMENT
	}

	if l.so.Lookup("hgmlDeviceCreateGpuInstanceWithPlacement") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstance hgmlGpuInstance
	ret := hgmlDeviceCreateGpuInstanceWithPlacement(l.handle, info.Id, placement, &gpuInstance)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("hgmlGpuInstanceGetInfo") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceInfo hgmlGpuInstanceInfo
	ret = hgmlGpuInstanceGetInfo(gpuInstance, &gpuInstanceInfo)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	return GpuInstance{
		device:               l.handle,
		gpuInstanceId:        gpuInstanceInfo.Id,
		gpuInstanceProfileId: gpuInstanceInfo.ProfileId,
		gpuInstancePlacement: gpuInstanceInfo.Placement,
		gpuInstance:          gpuInstance,
		so:                   l.so,
	}, ret
}

// GetGpuInstance retrieves the GPU instance associated with the device,
// returning the GPU instance and a Return value indicating the success or failure of the operation.
func (l Device) GetGpuInstance() (GpuInstance, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuInstanceId") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceId uint32
	ret := hgmlDeviceGetGpuInstanceId(l.handle, &gpuInstanceId)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("hgmlDeviceGetGpuInstanceById") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstance hgmlGpuInstance
	ret = hgmlDeviceGetGpuInstanceById(l.handle, gpuInstanceId, &gpuInstance)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("hgmlGpuInstanceGetInfo") == nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceInfo hgmlGpuInstanceInfo
	ret = hgmlGpuInstanceGetInfo(gpuInstance, &gpuInstanceInfo)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	return GpuInstance{
		device:               l.handle,
		gpuInstanceId:        gpuInstanceId,
		gpuInstanceProfileId: gpuInstanceInfo.ProfileId,
		gpuInstancePlacement: gpuInstanceInfo.Placement,
		gpuInstance:          gpuInstance,
		so:                   l.so,
	}, ret
}

type (
	GpuInstance struct {
		device               hgmlDevice
		gpuInstanceId        uint32
		gpuInstanceProfileId uint32
		gpuInstancePlacement GpuInstancePlacement
		gpuInstance          hgmlGpuInstance
		so                   binding.Library
	}

	GpuInstanceInfo struct {
		Id        uint32
		ProfileId uint32
		Placement GpuInstancePlacement
	}
)

// GetInfo retrieves the information about the GPU instance, including its ID, profile ID, and placement.
func (l GpuInstance) GetInfo() GpuInstanceInfo {
	return GpuInstanceInfo{
		Id:        l.gpuInstanceId,
		ProfileId: l.gpuInstanceProfileId,
		Placement: l.gpuInstancePlacement,
	}
}

// GetProfileInfoV retrieves the profile information of the GPU instance, including the profile ID and placement.
func (l GpuInstance) GetProfileInfoV() GpuInstanceProfileInfoHandler {
	return GpuInstanceProfileInfoHandler(l)
}

type GpuInstanceProfileInfoHandler GpuInstance

func (l GpuInstanceProfileInfoHandler) V1() (GpuInstanceProfileInfo_v3, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuInstanceProfileInfo") != nil {
		return GpuInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuInstanceProfileInfo
	ret := hgmlDeviceGetGpuInstanceProfileInfo(l.device, l.gpuInstanceProfileId, &info)
	return GpuInstanceProfileInfo_v3{
		Id:                  info.Id,
		SliceCount:          info.SliceCount,
		InstanceCount:       info.InstanceCount,
		MultiprocessorCount: info.MultiprocessorCount,
		CopyEngineCount:     info.CopyEngineCount,
		DecoderCount:        info.DecoderCount,
		EncoderCount:        info.EncoderCount,
		JpegCount:           info.JpegCount,
		OfaCount:            info.OfaCount,
		MemorySizeMB:        info.MemorySizeMB,
	}, ret
}

func (l GpuInstanceProfileInfoHandler) V2() (GpuInstanceProfileInfo_v3, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuInstanceProfileInfoV") != nil {
		return GpuInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuInstanceProfileInfo_v2
	info.Version = STRUCT_VERSION(info, 2)
	ret := hgmlDeviceGetGpuInstanceProfileInfoV(l.device, l.gpuInstanceProfileId, &info)
	return GpuInstanceProfileInfo_v3{
		Version:             info.Version,
		Id:                  info.Id,
		SliceCount:          info.SliceCount,
		InstanceCount:       info.InstanceCount,
		MultiprocessorCount: info.MultiprocessorCount,
		CopyEngineCount:     info.CopyEngineCount,
		DecoderCount:        info.DecoderCount,
		EncoderCount:        info.EncoderCount,
		JpegCount:           info.JpegCount,
		OfaCount:            info.OfaCount,
		MemorySizeMB:        info.MemorySizeMB,
		Name:                info.Name,
	}, ret
}

func (l GpuInstanceProfileInfoHandler) V3() (GpuInstanceProfileInfo_v3, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuInstanceProfileInfoV") != nil {
		return GpuInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuInstanceProfileInfo_v3
	info.Version = STRUCT_VERSION(info, 3)
	ret := hgmlDeviceGetGpuInstanceProfileInfoV(l.device, l.gpuInstanceProfileId, (*GpuInstanceProfileInfo_v2)(unsafe.Pointer(&info)))
	return info, ret
}

// CreateComputeInstance creates a compute instance associated with the GPU instance using the specified profile information.
func (l GpuInstance) CreateComputeInstance(info *ComputeInstanceProfileInfo) (ComputeInstance, Return) {
	if info == nil {
		return ComputeInstance{}, ERROR_INVALID_ARGUMENT
	}

	if l.so.Lookup("hgmlGpuInstanceCreateComputeInstance") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstance hgmlComputeInstance
	ret := hgmlGpuInstanceCreateComputeInstance(l.gpuInstance, info.Id, &computeInstance)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	var computeInstanceInfo hgmlComputeInstanceInfo
	if l.so.Lookup("hgmlComputeInstanceGetInfo_v2") == nil {
		ret = hgmlComputeInstanceGetInfo_v2(computeInstance, &computeInstanceInfo)
	} else {
		ret = hgmlComputeInstanceGetInfo(computeInstance, &computeInstanceInfo)
	}
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	return ComputeInstance{
		GpuInstance:              l,
		computeInstanceId:        computeInstanceInfo.Id,
		computeInstanceProfileId: computeInstanceInfo.ProfileId,
		computeInstancePlacement: computeInstanceInfo.Placement,
		computeInstance:          computeInstance,
	}, ret
}

// CreateComputeInstanceWithPlacement creates a compute instance associated with the GPU instance using the specified profile information and placement.
func (l GpuInstance) CreateComputeInstanceWithPlacement(info *ComputeInstanceProfileInfo, placement *ComputeInstancePlacement) (ComputeInstance, Return) {
	if info == nil {
		return ComputeInstance{}, ERROR_INVALID_ARGUMENT
	}

	if l.so.Lookup("hgmlGpuInstanceCreateComputeInstanceWithPlacement") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstance hgmlComputeInstance
	ret := hgmlGpuInstanceCreateComputeInstanceWithPlacement(l.gpuInstance, info.Id, placement, &computeInstance)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	var computeInstanceInfo hgmlComputeInstanceInfo
	if l.so.Lookup("hgmlComputeInstanceGetInfo_v2") == nil {
		ret = hgmlComputeInstanceGetInfo_v2(computeInstance, &computeInstanceInfo)
	} else {
		ret = hgmlComputeInstanceGetInfo(computeInstance, &computeInstanceInfo)
	}
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	return ComputeInstance{
		GpuInstance:              l,
		computeInstanceId:        computeInstanceInfo.Id,
		computeInstanceProfileId: computeInstanceInfo.ProfileId,
		computeInstancePlacement: computeInstanceInfo.Placement,
		computeInstance:          computeInstance,
	}, ret
}

// GetComputeInstance retrieves the compute instance associated with the GPU instance, if any.
func (l GpuInstance) GetComputeInstance() (ComputeInstance, Return) {
	if l.so.Lookup("hgmlDeviceGetComputeInstanceId") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var id uint32
	ret := hgmlDeviceGetComputeInstanceId(l.device, &id)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	if l.so.Lookup("hgmlGpuInstanceGetComputeInstanceById") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstance hgmlComputeInstance
	ret = hgmlGpuInstanceGetComputeInstanceById(l.gpuInstance, id, &computeInstance)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	var computeInstanceInfo hgmlComputeInstanceInfo
	if l.so.Lookup("hgmlComputeInstanceGetInfo_v2") == nil {
		ret = hgmlComputeInstanceGetInfo_v2(computeInstance, &computeInstanceInfo)
	} else {
		ret = hgmlComputeInstanceGetInfo(computeInstance, &computeInstanceInfo)
	}
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	return ComputeInstance{
		GpuInstance:              l,
		computeInstanceId:        id,
		computeInstanceProfileId: computeInstanceInfo.ProfileId,
		computeInstancePlacement: computeInstanceInfo.Placement,
		computeInstance:          computeInstance,
	}, ret
}

type (
	ComputeInstance struct {
		GpuInstance
		computeInstanceId        uint32
		computeInstanceProfileId uint32
		computeInstancePlacement ComputeInstancePlacement
		computeInstance          hgmlComputeInstance
	}

	ComputeInstanceInfo struct {
		Id        uint32
		ProfileId uint32
		Placement ComputeInstancePlacement
	}
)

// GetInfo retrieves the information about the compute instance, including its ID, profile ID, and placement.
func (l ComputeInstance) GetInfo() ComputeInstanceInfo {
	return ComputeInstanceInfo{
		Id:        l.computeInstanceId,
		ProfileId: l.computeInstanceProfileId,
		Placement: l.computeInstancePlacement,
	}
}

// GetProfileInfoV retrieves the profile information of the compute instance, including the profile ID and placement.
func (l ComputeInstance) GetProfileInfoV() ComputeInstanceProfileInfoHandler {
	return ComputeInstanceProfileInfoHandler(l)
}

type ComputeInstanceProfileInfoHandler ComputeInstance

func (l ComputeInstanceProfileInfoHandler) V1() (ComputeInstanceProfileInfo_v3, Return) {
	if l.so.Lookup("hgmlGpuInstanceGetComputeInstanceProfileInfo") != nil {
		return ComputeInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info ComputeInstanceProfileInfo
	ret := hgmlGpuInstanceGetComputeInstanceProfileInfo(
		l.gpuInstance,
		l.gpuInstanceProfileId,
		l.computeInstanceProfileId,
		&info)
	return ComputeInstanceProfileInfo_v3{
		Id:                    info.Id,
		SliceCount:            info.SliceCount,
		InstanceCount:         info.InstanceCount,
		MultiprocessorCount:   info.MultiprocessorCount,
		SharedCopyEngineCount: info.SharedCopyEngineCount,
		SharedDecoderCount:    info.SharedDecoderCount,
		SharedEncoderCount:    info.SharedEncoderCount,
		SharedJpegCount:       info.SharedJpegCount,
		SharedOfaCount:        info.SharedOfaCount,
	}, ret
}

func (l ComputeInstanceProfileInfoHandler) V2() (ComputeInstanceProfileInfo_v3, Return) {
	if l.so.Lookup("hgmlGpuInstanceGetComputeInstanceProfileInfoV") != nil {
		return ComputeInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info ComputeInstanceProfileInfo_v2
	info.Version = STRUCT_VERSION(info, 2)
	ret := hgmlGpuInstanceGetComputeInstanceProfileInfoV(
		l.gpuInstance,
		l.gpuInstanceProfileId,
		l.computeInstanceProfileId,
		&info)
	return ComputeInstanceProfileInfo_v3{
		Version:               info.Version,
		Id:                    info.Id,
		SliceCount:            info.SliceCount,
		InstanceCount:         info.InstanceCount,
		MultiprocessorCount:   info.MultiprocessorCount,
		SharedCopyEngineCount: info.SharedCopyEngineCount,
		SharedDecoderCount:    info.SharedDecoderCount,
		SharedEncoderCount:    info.SharedEncoderCount,
		SharedJpegCount:       info.SharedJpegCount,
		SharedOfaCount:        info.SharedOfaCount,
		Name:                  info.Name,
	}, ret
}

func (l ComputeInstanceProfileInfoHandler) V3() (ComputeInstanceProfileInfo_v3, Return) {
	if l.so.Lookup("hgmlGpuInstanceGetComputeInstanceProfileInfoV") != nil {
		return ComputeInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info ComputeInstanceProfileInfo_v3
	info.Version = STRUCT_VERSION(info, 3)
	ret := hgmlGpuInstanceGetComputeInstanceProfileInfoV(
		l.gpuInstance,
		l.gpuInstanceProfileId,
		l.computeInstanceProfileId,
		(*ComputeInstanceProfileInfo_v2)(unsafe.Pointer(&info)),
	)
	return info, ret
}
