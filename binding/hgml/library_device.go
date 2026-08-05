package hgml

import "C"
import (
	"fmt"
	"strings"
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
	// GetGpuInstanceId returns the id of the GPU instance that owns this MIG device handle.
	// Unlike GetGpuInstance it does not resolve the full instance (which needs the parent
	// device handle), so it is valid to call directly on a MIG device handle.
	GetGpuInstanceId() (uint32, Return)
	// GetComputeInstanceId returns the id of the compute instance this MIG device handle is,
	// within its owning GPU instance. Read together with GetGpuInstanceId and GetUUID off the
	// same handle, it identifies which compute instance the identity string addresses.
	GetComputeInstanceId() (uint32, Return)
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

// GetGpuInstanceId retrieves the id of the GPU instance that owns this device handle,
// returning the id and a Return value indicating the success or failure of the operation.
// It is valid on a MIG device handle, where it identifies the owning GPU instance.
func (l Device) GetGpuInstanceId() (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuInstanceId") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}
	var gpuInstanceId uint32
	ret := hgmlDeviceGetGpuInstanceId(l.handle, &gpuInstanceId)
	return gpuInstanceId, ret
}

// GetComputeInstanceId retrieves the id of the compute instance this device handle is, within
// its owning GPU instance, returning the id and a Return value indicating the success or failure
// of the operation. It is valid on a MIG device handle, which is a compute instance: a MIG device
// handle carries both the identity string a container is given and the ids of the GPU and compute
// instances that identity addresses, so reading them off the one handle keeps them describing the
// same partition. Ids are unique per GPU instance and are reassigned once the instance is destroyed.
func (l Device) GetComputeInstanceId() (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetComputeInstanceId") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}
	var computeInstanceId uint32
	ret := hgmlDeviceGetComputeInstanceId(l.handle, &computeInstanceId)
	return computeInstanceId, ret
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

// GetGpuInstanceProfileInfo probes the GPU instance profile identified by profileId
// directly from the device, without requiring an existing GPU instance. An unsupported
// profile id surfaces as a non-success Return, which the caller skips.
//
// The versioned read is taken as V2, because it is the only one whose layout the library
// cannot mislay. Both versioned reads go through ONE symbol and are told apart only by the
// struct version the caller writes into the buffer it passes — and v2 and v3 are exactly
// the SAME SIZE (152 bytes) while differing from their third field onward, v2 carrying an
// IsP2pSupported that v3 dropped and v3 a Capabilities that v2 lacks. A library that
// dispatches on the size encoded in that version word instead of on the version itself
// therefore fills a v3-typed buffer with the v2 layout and reports success, and every
// field from the third on is then read from the wrong offset: the name comes out of
// MemorySizeMB's bytes, which reads as empty only for the memory sizes whose low byte
// happens to be zero. V2 reads into a v2-typed buffer and copies field by field, so it has
// no offset to get wrong.
//
// v3 adds exactly one field any caller here consumes — Capabilities, which marks a media
// or graphics variant — so it is read separately and kept only when that read describes
// the same profile V2 just described. Where the two disagree the capability is left unset
// and the callers testing it fall back to the '+' in the profile name, which is the tell
// they already treat as primary.
//
// A library too old for the versioned symbol falls back to V1, which carries no name. So
// does one that rejects the v2 struct while accepting the v3 one: there the v3 read is the
// only one carrying a name, and a library refusing v2 is by construction not the size-
// dispatching kind this guards against.
func (l Device) GetGpuInstanceProfileInfo(profileId uint32) (GpuInstanceProfileInfo_v3, Return) {
	h := GpuInstanceProfileInfoHandler{
		device:               l.handle,
		gpuInstanceProfileId: profileId,
		so:                   l.so,
	}
	if info, ret := h.V2(); ret.IsSuccess() {
		if v3, v3ret := h.V3(); v3ret.IsSuccess() && describesSameProfile(info, v3) {
			info.Capabilities = v3.Capabilities
		}
		return info, ret
	}
	if info, ret := h.V3(); ret.IsSuccess() {
		return info, ret
	}
	return h.V1()
}

// describesSameProfile reports whether two reads of one profile agree on everything both struct
// versions carry. It is what makes a v3 read usable: a v3 buffer the library filled with the v2
// layout reads its slice count out of IsP2pSupported, its memory size out of the field before it and
// its name out of the memory size, so a read agreeing on all of those was laid out as asked for.
//
// The name is compared as a name rather than as the raw array, so trailing bytes the library did or
// did not write past the terminator cannot decide the answer.
func describesSameProfile(a, b GpuInstanceProfileInfo_v3) bool {
	return a.Id == b.Id &&
		a.SliceCount == b.SliceCount &&
		a.InstanceCount == b.InstanceCount &&
		a.MemorySizeMB == b.MemorySizeMB &&
		a.GetName() == b.GetName()
}

// GetName returns the canonical profile name (e.g. "1g.5gb") from the C char array, read
// no further than the array itself: a library that fills every byte without terminating
// must not be able to make this read past it. It is empty on the legacy (V1) path, which
// carries no name.
//
// The "MIG " prefix strip mirrors the NVML binding, where the name is reported as
// "MIG 1g.5gb" and the bare form is what resource keys and user requests use. Whether
// this vendor's library carries that prefix is UNCONFIRMED — it awaits verification on
// real hardware. The strip is a no-op when the prefix is absent, so the bare form is
// returned either way.
func (info *GpuInstanceProfileInfo_v3) GetName() string {
	return strings.TrimPrefix(binding.GoStringN(info.Name[:]), "MIG ")
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

// Destroy destroys the GPU instance. It returns ERROR_IN_USE while the instance
// still holds compute instances or active processes, so the caller destroys the
// compute instances first (GetComputeInstances then ComputeInstance.Destroy) and
// retries with bounds on IN_USE.
func (l GpuInstance) Destroy() Return {
	if l.so.Lookup("hgmlGpuInstanceDestroy") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}
	return hgmlGpuInstanceDestroy(l.gpuInstance)
}

// Destroy destroys the compute instance. It returns ERROR_IN_USE while the instance
// still has active processes, so the caller retries with bounds on IN_USE.
func (l ComputeInstance) Destroy() Return {
	if l.so.Lookup("hgmlComputeInstanceDestroy") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}
	return hgmlComputeInstanceDestroy(l.computeInstance)
}

// gpuInstancePossiblePlacementsCall matches the base and _v2 possible-placements
// symbols, which share a signature, so GetGpuInstancePossiblePlacements can prefer
// _v2 and fall back to the base with a single count-then-fill implementation.
type gpuInstancePossiblePlacementsCall func(hgmlDevice, uint32, *GpuInstancePlacement, *uint32) Return

func (l Device) gpuInstancePossiblePlacements(symbol string, call gpuInstancePossiblePlacementsCall, profileId uint32) ([]GpuInstancePlacement, Return) {
	if l.so.Lookup(symbol) != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}
	var count uint32
	if ret := call(l.handle, profileId, nil, &count); !ret.IsSuccess() {
		return nil, ret
	}
	if count == 0 {
		return nil, SUCCESS
	}
	placements := make([]GpuInstancePlacement, count)
	if ret := call(l.handle, profileId, &placements[0], &count); !ret.IsSuccess() {
		return nil, ret
	}
	// The out-count is the library's, and the buffer's length is ours: bound one by the other
	// before slicing, as every other enumeration here does. A library reporting more than it was
	// given must not be able to index past the buffer, and a failed fill must not be sliced at all.
	count = min(count, uint32(len(placements)))
	return placements[:count], SUCCESS
}

// GetGpuInstancePossiblePlacements returns the legal placements (start:size in
// memory-slice units) for the given GPU instance profile on this device. It queries
// the count first, then fills the slice, and prefers the _v2 symbol with a base
// fallback, mirroring the V1/V2/V3 profile-info handlers.
func (l Device) GetGpuInstancePossiblePlacements(profileId uint32) ([]GpuInstancePlacement, Return) {
	placements, ret := l.gpuInstancePossiblePlacements(
		"hgmlDeviceGetGpuInstancePossiblePlacements_v2", hgmlDeviceGetGpuInstancePossiblePlacements_v2, profileId)
	if ret == ERROR_FUNCTION_NOT_FOUND {
		return l.gpuInstancePossiblePlacements(
			"hgmlDeviceGetGpuInstancePossiblePlacements", hgmlDeviceGetGpuInstancePossiblePlacements, profileId)
	}
	return placements, ret
}

// GetGpuInstances returns the live GPU instances of the given profile on this
// device. Each handle's info is read so the returned GpuInstance carries its occupied
// placement — GetInfo().Placement, which the ledger uses to build the occupied intervals.
//
// The result buffer is sized from the profile's legal-placement count (its per-card
// instance ceiling), obtained via GetGpuInstancePossiblePlacements, which takes this
// creation profileId. It must NOT be sized via GetGpuInstanceProfileInfo(profileId):
// that call takes a profile ENUM INDEX (0..GPU_INSTANCE_PROFILE_COUNT-1), not a creation
// Id, and the two differ on real hardware, so passing the creation Id there fails and
// drops every live instance — silently emptying the occupancy ledger, which double-books
// placement slots on allocation and hides instances from reclaim.
//
// That sizing is a ceiling, not a guess: the possible-placements query reports every legal
// placement of the profile, not the currently free ones, and two live instances of one profile
// cannot share a memory-slice placement — so the live count can never exceed the placement
// count. An out-count equal to the buffer is therefore a fully partitioned card, not a
// truncated read, which is why it is not treated as an error.
//
// No legal placement means no instance can exist, so an empty placement list reports an empty
// live set rather than probing further. A failed placement query is propagated as a failure
// and never reaches that conclusion.
func (l Device) GetGpuInstances(profileId uint32) ([]GpuInstance, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuInstances") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}
	placements, ret := l.GetGpuInstancePossiblePlacements(profileId)
	if !ret.IsSuccess() {
		return nil, ret
	}
	if len(placements) == 0 {
		return nil, SUCCESS
	}

	if l.so.Lookup("hgmlGpuInstanceGetInfo") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}
	handles := make([]hgmlGpuInstance, len(placements))
	// count is the in/out buffer-capacity argument (in: array size, out: number written), so
	// initialize it to the allocated handle count — never leave it 0, which asks the library to
	// fill a zero-capacity buffer and can return an error / empty set where the in size is enforced.
	count := uint32(len(handles))
	if ret := hgmlDeviceGetGpuInstances(l.handle, profileId, &handles[0], &count); !ret.IsSuccess() {
		return nil, ret
	}
	// Bound the library's out-count by the buffer it was given: a library reporting success with a
	// larger count would otherwise index past the allocated handles and abort the process.
	count = min(count, uint32(len(handles)))
	instances := make([]GpuInstance, 0, count)
	for i := uint32(0); i < count; i++ {
		var info hgmlGpuInstanceInfo
		if ret := hgmlGpuInstanceGetInfo(handles[i], &info); !ret.IsSuccess() {
			return nil, ret
		}
		instances = append(instances, GpuInstance{
			device:               l.handle,
			gpuInstanceId:        info.Id,
			gpuInstanceProfileId: info.ProfileId,
			gpuInstancePlacement: info.Placement,
			gpuInstance:          handles[i],
			so:                   l.so,
		})
	}
	return instances, SUCCESS
}

// GetComputeInstanceProfileInfo probes the compute-instance profile identified by the profile
// ENUM INDEX ciProfileIndex (0..COMPUTE_INSTANCE_PROFILE_COUNT-1) and ciEngProfileId on this GPU
// instance, without requiring an existing compute instance. It is called before
// CreateComputeInstance to obtain the profile info a compute instance is built from — whose Id
// is the creation id every other compute-instance call takes.
func (l GpuInstance) GetComputeInstanceProfileInfo(ciProfileIndex, ciEngProfileId uint32) (ComputeInstanceProfileInfo, Return) {
	if l.so.Lookup("hgmlGpuInstanceGetComputeInstanceProfileInfo") != nil {
		return ComputeInstanceProfileInfo{}, ERROR_FUNCTION_NOT_FOUND
	}
	var info ComputeInstanceProfileInfo
	ret := hgmlGpuInstanceGetComputeInstanceProfileInfo(l.gpuInstance, ciProfileIndex, ciEngProfileId, &info)
	return info, ret
}

// GetComputeInstances returns the live compute instances of one compute-instance profile on this
// GPU instance, and each handle's info is read to populate the returned ComputeInstance values.
// Reclaim uses this to destroy a GPU instance's compute instances before the instance itself.
//
// ciProfileIndex is a profile ENUM INDEX (0..COMPUTE_INSTANCE_PROFILE_COUNT-1), the space the
// profile-info probe takes. The library's own enumeration call takes the creation Id the probe
// reports in the profile record, which is a different space — the header names the two parameters
// apart — so the looked-up Id is forwarded rather than the index. Passing the index straight
// through matches only where the two spaces coincide, which leaves a live compute instance
// unfound wherever they do not: its GPU instance's teardown is then rejected as busy forever,
// stranding that partition and its placement for the node's lifetime.
//
// The buffer is sized from the profile's InstanceCount, queried with the SHARED engine profile —
// the only engine profile that exists — COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT is 1 — so this
// enumeration misses no compute instance, whoever created it. An out-count equal to that buffer is a GPU instance
// filled to its profile's ceiling, not a truncated read.
func (l GpuInstance) GetComputeInstances(ciProfileIndex uint32) ([]ComputeInstance, Return) {
	if l.so.Lookup("hgmlGpuInstanceGetComputeInstances") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}
	profileInfo, ret := l.GetComputeInstanceProfileInfo(ciProfileIndex, COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
	if !ret.IsSuccess() {
		return nil, ret
	}
	if profileInfo.InstanceCount == 0 {
		return nil, SUCCESS
	}

	handles := make([]hgmlComputeInstance, profileInfo.InstanceCount)
	// count is the in/out buffer-capacity argument; initialize it to the allocated handle count
	// (see GetGpuInstances) so the library is never handed a zero-capacity buffer.
	count := uint32(len(handles))
	if ret := hgmlGpuInstanceGetComputeInstances(
		l.gpuInstance, profileInfo.Id, &handles[0], &count); !ret.IsSuccess() {
		return nil, ret
	}
	// Bound the library's out-count by the buffer it was given (see GetGpuInstances).
	count = min(count, uint32(len(handles)))
	instances := make([]ComputeInstance, 0, count)
	for i := uint32(0); i < count; i++ {
		var info hgmlComputeInstanceInfo
		if l.so.Lookup("hgmlComputeInstanceGetInfo_v2") == nil {
			ret = hgmlComputeInstanceGetInfo_v2(handles[i], &info)
		} else {
			ret = hgmlComputeInstanceGetInfo(handles[i], &info)
		}
		if !ret.IsSuccess() {
			return nil, ret
		}
		instances = append(instances, ComputeInstance{
			GpuInstance:              l,
			computeInstanceId:        info.Id,
			computeInstanceProfileId: info.ProfileId,
			computeInstancePlacement: info.Placement,
			computeInstance:          handles[i],
		})
	}
	return instances, SUCCESS
}

// GetGpuInstanceRemainingCapacity returns how many more GPU instances of the given
// profile can still be created on this device, as the library reports it directly — a
// cross-check against the placement-derived free count the ledger computes.
func (l Device) GetGpuInstanceRemainingCapacity(profileId uint32) (uint32, Return) {
	if l.so.Lookup("hgmlDeviceGetGpuInstanceRemainingCapacity") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}
	var count uint32
	ret := hgmlDeviceGetGpuInstanceRemainingCapacity(l.handle, profileId, &count)
	return count, ret
}
