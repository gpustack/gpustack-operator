package nvml

import "C"
import (
	"fmt"
	"unsafe"

	"gpustack.ai/gpustack/binding"
)

// DeviceGetCount retrieves the number of NVIDIA devices in the system.
func (l *NVML) DeviceGetCount() (int, Return) {
	var deviceCount uint32
	if l.so.Lookup("nvmlDeviceGetCount_v2") == nil {
		ret := nvmlDeviceGetCount_v2(&deviceCount)
		return int(deviceCount), ret
	}
	if l.so.Lookup("nvmlDeviceGetCount") == nil {
		ret := nvmlDeviceGetCount(&deviceCount)
		return int(deviceCount), ret
	}
	return 0, ERROR_FUNCTION_NOT_FOUND
}

// DeviceGetHandleByIndex retrieves a handle for the device at the specified index.
func (l *NVML) DeviceGetHandleByIndex(index int) (Device, Return) {
	var handle nvmlDevice
	if l.so.Lookup("nvmlDeviceGetHandleByIndex_v2") == nil {
		ret := nvmlDeviceGetHandleByIndex_v2(uint32(index), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	if l.so.Lookup("nvmlDeviceGetHandleByIndex") == nil {
		ret := nvmlDeviceGetHandleByIndex(uint32(index), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	return Device{}, ERROR_FUNCTION_NOT_FOUND
}

// DeviceGetHandleByPciBusId retrieves a handle for the device with the specified PCI bus ID.
func (l *NVML) DeviceGetHandleByPciBusId(pciBusId string) (Device, Return) {
	var handle nvmlDevice
	if l.so.Lookup("nvmlDeviceGetHandleByPciBusId_v2") == nil {
		ret := nvmlDeviceGetHandleByPciBusId_v2(pciBusId+string(rune(0)), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	if l.so.Lookup("nvmlDeviceGetHandleByPciBusId") == nil {
		ret := nvmlDeviceGetHandleByPciBusId(pciBusId+string(rune(0)), &handle)
		return Device{handle: handle, so: l.so}, ret
	}
	return Device{}, ERROR_FUNCTION_NOT_FOUND
}

// DeviceGetHandleByUUID retrieves a handle for the device with the specified UUID.
func (l *NVML) DeviceGetHandleByUUID(uuid string) (Device, Return) {
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

// GetCudaComputeCapability retrieves the CUDA compute capability of the device,
// returning the major and minor version numbers.
func (l Device) GetCudaComputeCapability() (int32, int32, Return) {
	if l.so.Lookup("nvmlDeviceGetCudaComputeCapability") != nil {
		return 0, 0, ERROR_FUNCTION_NOT_FOUND
	}

	var major, minor int32
	ret := nvmlDeviceGetCudaComputeCapability(l.handle, &major, &minor)
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
	if l.so.Lookup("nvmlDeviceGetPciInfo_v2") != nil {
		return PciInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var pciInfo PciInfo
	ret := nvmlDeviceGetPciInfo_v2(l.handle, &pciInfo)
	return pciInfo, ret
}

func (l PciInfoHandler) V1() (PciInfo, Return) {
	if l.so.Lookup("nvmlDeviceGetPciInfo") != nil {
		return PciInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var pciInfo PciInfo
	ret := nvmlDeviceGetPciInfo(l.handle, &pciInfo)
	return pciInfo, ret
}

// GetMemoryAffinity retrieves the memory affinity of the device for a specified affinity scope,
// returning a list of NUMA node IDs.
func (l Device) GetMemoryAffinity(scope AffinityScope) ([]uint32, Return) {
	if l.so.Lookup("nvmlDeviceGetMemoryAffinity") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	nodeSetSize := uint32(binding.GetNumaNodeSetSize())
	nodeSet := make([]uint32, nodeSetSize)
	ret := nvmlDeviceGetMemoryAffinity(l.handle, nodeSetSize, &nodeSet[0], scope)
	return nodeSet, ret
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

// GetTemperatureV retrieves the temperature information of the device,
// returning a handler that can be used to access different versions of the temperature information.
func (l Device) GetTemperatureV() TemperatureHandler {
	return TemperatureHandler(l)
}

type TemperatureHandler Device

func (l TemperatureHandler) V1() (Temperature, Return) {
	if l.so.Lookup("nvmlDeviceGetTemperature") != nil {
		return Temperature{}, ERROR_FUNCTION_NOT_FOUND
	}

	var temperature Temperature
	temperature.Version = STRUCT_VERSION(temperature, 1)
	ret := nvmlDeviceGetTemperatureV(l.handle, &temperature)
	return temperature, ret
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

	name := make([]byte, DEVICE_NAME_V2_BUFFER_SIZE)
	ret := nvmlDeviceGetName(l.handle, &name[0], DEVICE_NAME_V2_BUFFER_SIZE)
	return string(name[:clen(name)]), ret
}

// GetUUID retrieves the universally unique identifier (UUID) of the device,
// which is a string that uniquely identifies the device across all systems.
func (l Device) GetUUID() (string, Return) {
	if l.so.Lookup("nvmlDeviceGetUUID") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	uuid := make([]byte, DEVICE_UUID_V2_BUFFER_SIZE)
	ret := nvmlDeviceGetUUID(l.handle, &uuid[0], DEVICE_UUID_V2_BUFFER_SIZE)
	return string(uuid[:clen(uuid)]), ret
}

// GetUtilizationRates retrieves the current utilization rates of the device, including GPU and memory utilization,
// returning a Utilization struct with the relevant information.
func (l Device) GetUtilizationRates() (Utilization, Return) {
	if l.so.Lookup("nvmlDeviceGetUtilizationRates") != nil {
		return Utilization{}, ERROR_FUNCTION_NOT_FOUND
	}

	var utilization Utilization
	ret := nvmlDeviceGetUtilizationRates(l.handle, &utilization)
	return utilization, ret
}

// GetNumGpuCores retrieves the number of GPU cores available on the device.
func (l Device) GetNumGpuCores() (uint32, Return) {
	if l.so.Lookup("nvmlDeviceGetNumGpuCores") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var numCores uint32
	ret := nvmlDeviceGetNumGpuCores(l.handle, &numCores)
	return numCores, ret
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
		Free:  memory.Free,
		Used:  memory.Used,
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

// GetEccMode retrieves the current and pending ECC mode of the device.
func (l Device) GetEccMode() (EnableState, EnableState, Return) {
	if l.so.Lookup("nvmlDeviceGetEccMode") != nil {
		return FEATURE_DISABLED, FEATURE_DISABLED, ERROR_FUNCTION_NOT_FOUND
	}

	var current, pending EnableState
	ret := nvmlDeviceGetEccMode(l.handle, &current, &pending)
	return current, pending, ret
}

// GetMemoryErrorCounter retrieves the count of memory errors for the device based on the specified error type,
// ECC counter type, and memory location,
// returning the error count as a uint64 value.
func (l Device) GetMemoryErrorCounter(errorType MemoryErrorType, counterType EccCounterType, locationType MemoryLocation) (uint64, Return) {
	if l.so.Lookup("nvmlDeviceGetMemoryErrorCounter") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var count uint64
	ret := nvmlDeviceGetMemoryErrorCounter(l.handle, errorType, counterType, locationType, &count)
	return count, ret
}

// GetTopologyCommonAncestor retrieves the common ancestor in the GPU topology between the current device and another specified device,
// returning the topology level of the common ancestor.
func (l Device) GetTopologyCommonAncestor(device2 Device) (GpuTopologyLevel, Return) {
	if l.so.Lookup("nvmlDeviceGetTopologyCommonAncestor") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var pathInfo GpuTopologyLevel
	ret := nvmlDeviceGetTopologyCommonAncestor(l.handle, device2.handle, &pathInfo)
	return pathInfo, ret
}

// GetGpuFabricInfoV retrieves the GPU fabric information of the device,
// returning a handler that can be used to access different versions of the GPU fabric information.
func (l Device) GetGpuFabricInfoV() GpuFabricInfoHandler {
	return GpuFabricInfoHandler(l)
}

type GpuFabricInfoHandler Device

func (l GpuFabricInfoHandler) V1() (GpuFabricInfo, Return) {
	if l.so.Lookup("nvmlDeviceGetGpuFabricInfo") != nil {
		return GpuFabricInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var gpuFabricInfo GpuFabricInfo
	ret := nvmlDeviceGetGpuFabricInfo(l.handle, &gpuFabricInfo)
	return gpuFabricInfo, ret
}

func (l GpuFabricInfoHandler) V2() (GpuFabricInfo_v2, Return) {
	if l.so.Lookup("nvmlDeviceGetGpuFabricInfo_v2") != nil {
		return GpuFabricInfo_v2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuFabricInfo_v2
	info.Version = STRUCT_VERSION(info, 2)
	ret := nvmlDeviceGetGpuFabricInfoV(l.handle, (*GpuFabricInfoV)(unsafe.Pointer(&info)))
	return info, ret
}

// GetFieldValues retrieves the values of specified fields for the device,
// returning a list of FieldValue structs with the corresponding values.
func (l Device) GetFieldValues(values []FieldValue) Return {
	if l.so.Lookup("nvmlDeviceGetFieldValues") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	valuesCount := len(values)
	return nvmlDeviceGetFieldValues(l.handle, int32(valuesCount), &values[0])
}

// GetNvLinkState retrieves the state of the NVLink connection for a specified link index,
// returning whether the link is active or not.
func (l Device) GetNvLinkState(link int) (EnableState, Return) {
	if l.so.Lookup("nvmlDeviceGetNvLinkState") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var isActive EnableState
	ret := nvmlDeviceGetNvLinkState(l.handle, uint32(link), &isActive)
	return isActive, ret
}

// GetMigMode retrieves the current and pending MIG mode of the device,
// returning the current mode, pending mode, and a Return value indicating the success or failure of the operation.
func (l Device) GetMigMode() (uint32, uint32, Return) {
	if l.so.Lookup("nvmlDeviceGetMigMode") != nil {
		return 0, 0, ERROR_FUNCTION_NOT_FOUND
	}

	var currentMode, pendingMode uint32
	ret := nvmlDeviceGetMigMode(l.handle, &currentMode, &pendingMode)
	return currentMode, pendingMode, ret
}

// GetMaxMigDeviceCount retrieves the maximum number of MIG devices that can be created on the device,
// returning the count and a Return value indicating the success or failure of the operation.
func (l Device) GetMaxMigDeviceCount() (int, Return) {
	if l.so.Lookup("nvmlDeviceGetMaxMigDeviceCount") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var count uint32
	ret := nvmlDeviceGetMaxMigDeviceCount(l.handle, &count)
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
	if l.so.Lookup("nvmlDeviceGetMigDeviceHandleByIndex") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	var migDevice nvmlDevice
	ret := nvmlDeviceGetMigDeviceHandleByIndex(l.handle, uint32(index), &migDevice)
	return _MigDevice{Device{handle: migDevice, so: l.so}}, ret
}

// CreateGpuInstance creates a GPU instance on the device using the specified GPU instance profile information,
// returning the created GPU instance and a Return value indicating the success or failure of the operation.
func (l Device) CreateGpuInstance(info *GpuInstanceProfileInfo) (GpuInstance, Return) {
	if info == nil {
		return GpuInstance{}, ERROR_INVALID_ARGUMENT
	}

	if l.so.Lookup("nvmlDeviceCreateGpuInstance") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstance nvmlGpuInstance
	ret := nvmlDeviceCreateGpuInstance(l.handle, info.Id, &gpuInstance)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("nvmlGpuInstanceGetInfo") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceInfo nvmlGpuInstanceInfo
	ret = nvmlGpuInstanceGetInfo(gpuInstance, &gpuInstanceInfo)
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

	if l.so.Lookup("nvmlDeviceCreateGpuInstanceWithPlacement") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstance nvmlGpuInstance
	ret := nvmlDeviceCreateGpuInstanceWithPlacement(l.handle, info.Id, placement, &gpuInstance)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("nvmlGpuInstanceGetInfo") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceInfo nvmlGpuInstanceInfo
	ret = nvmlGpuInstanceGetInfo(gpuInstance, &gpuInstanceInfo)
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
	var gpuInstanceId uint32
	ret := nvmlDeviceGetGpuInstanceId(l.handle, &gpuInstanceId)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("nvmlGpuInstanceGetInfo") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstance nvmlGpuInstance
	ret = nvmlDeviceGetGpuInstanceById(l.handle, gpuInstanceId, &gpuInstance)
	if !ret.IsSuccess() {
		return GpuInstance{}, ret
	}

	if l.so.Lookup("nvmlGpuInstanceGetInfo") != nil {
		return GpuInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceInfo nvmlGpuInstanceInfo
	ret = nvmlGpuInstanceGetInfo(gpuInstance, &gpuInstanceInfo)
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
		device               nvmlDevice
		gpuInstanceId        uint32
		gpuInstanceProfileId uint32
		gpuInstancePlacement GpuInstancePlacement
		gpuInstance          nvmlGpuInstance
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
	if l.so.Lookup("nvmlDeviceGetGpuInstanceProfileInfo") != nil {
		return GpuInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuInstanceProfileInfo
	ret := nvmlDeviceGetGpuInstanceProfileInfo(l.device, l.gpuInstanceProfileId, &info)
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
	if l.so.Lookup("nvmlDeviceGetGpuInstanceProfileInfoV") != nil {
		return GpuInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuInstanceProfileInfo_v2
	info.Version = STRUCT_VERSION(info, 2)
	ret := nvmlDeviceGetGpuInstanceProfileInfoV(l.device, l.gpuInstanceProfileId, &info)
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
	if l.so.Lookup("nvmlDeviceGetGpuInstanceProfileInfoV") != nil {
		return GpuInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info GpuInstanceProfileInfo_v3
	info.Version = STRUCT_VERSION(info, 3)
	ret := nvmlDeviceGetGpuInstanceProfileInfoV(l.device, l.gpuInstanceProfileId, (*GpuInstanceProfileInfo_v2)(unsafe.Pointer(&info)))
	return info, ret
}

// GetGpuInstanceProfileInfo probes the GPU instance profile identified by profileId
// directly from the device, without requiring an existing GPU instance. It prefers the
// versioned NVML calls (which carry the profile Name) and falls back to the legacy call
// (no Name) on older drivers, returning the first successful result or the last error.
// An unsupported profile id surfaces as a non-success Return, which the caller skips.
func (l Device) GetGpuInstanceProfileInfo(profileId uint32) (GpuInstanceProfileInfo_v3, Return) {
	h := GpuInstanceProfileInfoHandler{
		device:               l.handle,
		gpuInstanceProfileId: profileId,
		so:                   l.so,
	}
	if info, ret := h.V3(); ret.IsSuccess() {
		return info, ret
	}
	if info, ret := h.V2(); ret.IsSuccess() {
		return info, ret
	}
	return h.V1()
}

// GetName returns the profile name (e.g. "1g.5gb") from the NUL-terminated C char array.
// It is empty on the legacy (V1) path, which carries no name.
func (info *GpuInstanceProfileInfo_v3) GetName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Name[0])))
}

// CreateComputeInstance creates a compute instance associated with the GPU instance using the specified profile information.
func (l GpuInstance) CreateComputeInstance(info *ComputeInstanceProfileInfo) (ComputeInstance, Return) {
	if info == nil {
		return ComputeInstance{}, ERROR_INVALID_ARGUMENT
	}

	if l.so.Lookup("nvmlGpuInstanceCreateComputeInstance") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstance nvmlComputeInstance
	ret := nvmlGpuInstanceCreateComputeInstance(l.gpuInstance, info.Id, &computeInstance)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	if l.so.Lookup("nvmlComputeInstanceGetInfo") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstanceInfo nvmlComputeInstanceInfo
	ret = nvmlComputeInstanceGetInfo(computeInstance, &computeInstanceInfo)
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

	if l.so.Lookup("nvmlGpuInstanceCreateComputeInstanceWithPlacement") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstance nvmlComputeInstance
	ret := nvmlGpuInstanceCreateComputeInstanceWithPlacement(l.gpuInstance, info.Id, placement, &computeInstance)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	if l.so.Lookup("nvmlComputeInstanceGetInfo") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstanceInfo nvmlComputeInstanceInfo
	ret = nvmlComputeInstanceGetInfo(computeInstance, &computeInstanceInfo)
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
	if l.so.Lookup("nvmlDeviceGetComputeInstanceId") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var id uint32
	ret := nvmlDeviceGetComputeInstanceId(l.device, &id)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	if l.so.Lookup("nvmlGpuInstanceGetComputeInstanceById") != nil {
		return ComputeInstance{}, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstance nvmlComputeInstance
	ret = nvmlGpuInstanceGetComputeInstanceById(l.gpuInstance, id, &computeInstance)
	if !ret.IsSuccess() {
		return ComputeInstance{}, ret
	}

	var computeInstanceInfo nvmlComputeInstanceInfo
	if l.so.Lookup("nvmlComputeInstanceGetInfo_v2") == nil {
		ret = nvmlComputeInstanceGetInfo_v2(computeInstance, &computeInstanceInfo)
	} else {
		ret = nvmlComputeInstanceGetInfo(computeInstance, &computeInstanceInfo)
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
		computeInstance          nvmlComputeInstance
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
	if l.so.Lookup("nvmlGpuInstanceGetComputeInstanceProfileInfo") != nil {
		return ComputeInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info ComputeInstanceProfileInfo
	ret := nvmlGpuInstanceGetComputeInstanceProfileInfo(
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
	if l.so.Lookup("nvmlGpuInstanceGetComputeInstanceProfileInfoV") != nil {
		return ComputeInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info ComputeInstanceProfileInfo_v2
	info.Version = STRUCT_VERSION(info, 2)
	ret := nvmlGpuInstanceGetComputeInstanceProfileInfoV(
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
	if l.so.Lookup("nvmlGpuInstanceGetComputeInstanceProfileInfoV") != nil {
		return ComputeInstanceProfileInfo_v3{}, ERROR_FUNCTION_NOT_FOUND
	}

	var info ComputeInstanceProfileInfo_v3
	info.Version = STRUCT_VERSION(info, 3)
	ret := nvmlGpuInstanceGetComputeInstanceProfileInfoV(
		l.gpuInstance,
		l.gpuInstanceProfileId,
		l.computeInstanceProfileId,
		(*ComputeInstanceProfileInfo_v2)(unsafe.Pointer(&info)),
	)
	return info, ret
}
