package amdsmi

import "C"
import (
	"fmt"
	"strings"
	"unsafe"

	"gpustack.ai/gpustack/binding"
)

// GetProcessorHandles retrieves the device handles for all AMD GPUs in the system.
func (l *AMDSMI) GetProcessorHandles() ([]Device, Return) {
	if l.so.Lookup("amdsmi_get_socket_handles") != nil {
		return nil, STATUS_FUNCTION_NOT_FOUND
	}

	var numSockets uint32
	ret := amdsmiGetSocketHandles(&numSockets, nil)
	if !ret.IsSuccess() {
		return nil, ret
	}
	socketHandles := make([]*SocketHandle, numSockets)
	ret = amdsmiGetSocketHandles(&numSockets, &socketHandles[0])
	if !ret.IsSuccess() {
		return nil, ret
	}

	var handles []Device
	for i := uint32(0); i < numSockets; i++ {
		var numProcessors uint32
		ret = amdsmiGetProcessorHandles(socketHandles[i], &numProcessors, nil)
		if !ret.IsSuccess() {
			return nil, ret
		}
		processorHandles := make([]*ProcessorHandle, numProcessors)
		ret = amdsmiGetProcessorHandles(socketHandles[i], &numProcessors, &processorHandles[0])
		if !ret.IsSuccess() {
			return nil, ret
		}

		for j := uint32(0); j < numProcessors; j++ {
			handles = append(handles, Device{handle: processorHandles[j], so: l.so})
		}
	}

	return handles, ret
}

func (l *AMDSMI) GetProcessorHandleByBdf(pciBusId string) (Device, Return) {
	if l.so.Lookup("amdsmi_get_processor_handle_from_bdf") != nil {
		return Device{}, STATUS_FUNCTION_NOT_FOUND
	}

	bdf, err := convertStringToBdf(pciBusId)
	if err != nil {
		return Device{}, STATUS_INVAL
	}

	var handle *ProcessorHandle
	ret := amdsmiGetProcessorHandleFromBdf(bdf, &handle)
	return Device{handle: handle, so: l.so}, ret
}

type Device struct {
	handle *ProcessorHandle
	so     binding.Library
}

func (info AsicInfo) GetVendorName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Vendor_name[0])))
}

func (info AsicInfo) GetMarketName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Market_name[0])))
}

// isUnsupportedSerial reports whether a normalized serial is the vendor's "cannot report this"
// sentinel rather than an identity. The header documents `0xFFFFFFFF if not supported` for the ASIC
// serial and a wider all-ones form for sibling fields, so every width counts: no real serial is all
// ones, and a sentinel that survives becomes an identity — the same one on every accelerator whose
// serial the library cannot read.
func isUnsupportedSerial(serial string) bool {
	return serial != "" && strings.Trim(serial, "f") == ""
}

// GetAsicSerial returns the accelerator's ASIC serial in lower case, without the "0x" prefix the
// vendor library only sometimes writes, and empty when the library has no serial to report.
//
// Trimming the prefix conditionally is load-bearing rather than defensive: the serial is the
// accelerator's identity — the detector publishes it as the accelerator ID and the ROCm runtime
// matches an agent against exactly that string — so cutting two characters unconditionally renames
// every accelerator, and the container is then handed a filter that selects nothing.
//
// Two spellings mean the library has nothing to report: the literal "N/A", and the all-ones sentinel.
// Both must come back empty, because callers treat an empty serial as "no identity" and refuse the
// claim, whereas a surviving sentinel is silently accepted as one.
func (info AsicInfo) GetAsicSerial() string {
	ret := strings.TrimPrefix(
		strings.ToLower(C.GoString((*C.char)(unsafe.Pointer(&info.Asic_serial[0])))), "0x")
	if ret == "n/a" || isUnsupportedSerial(ret) {
		return ""
	}
	return ret
}

func (info AsicInfo) GetUniqueId() string {
	if as := info.GetAsicSerial(); as != "" {
		return "GPU-" + as
	}
	return ""
}

func (info AsicInfo) GetTargetGraphicsVersion() string {
	return fmt.Sprintf("gfx%x", info.Target_graphics_version)
}

// GetGpuAsicInfo retrieves the ASIC information for the specified GPU device.
func (l Device) GetGpuAsicInfo() (AsicInfo, Return) {
	if l.so.Lookup("amdsmi_get_processor_handle_from_bdf") != nil {
		return AsicInfo{}, STATUS_FUNCTION_NOT_FOUND
	}

	var info AsicInfo
	ret := amdsmiGetGpuAsicInfo(l.handle, &info)
	return info, ret
}

func (info DriverInfo) GetVersion() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Version[0])))
}

func (info DriverInfo) GetDate() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Date[0])))
}

func (info DriverInfo) GetName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Name[0])))
}

// GetGpuDriverInfo retrieves the driver information for the specified GPU device.
func (l Device) GetGpuDriverInfo() (DriverInfo, Return) {
	if l.so.Lookup("amdsmi_get_gpu_driver_info") != nil {
		return DriverInfo{}, STATUS_FUNCTION_NOT_FOUND
	}

	var info DriverInfo
	ret := amdsmiGetGpuDriverInfo(l.handle, &info)
	return info, ret
}

func (bdf Bdf) String() string {
	v := *(*uint64)(unsafe.Pointer(&bdf[0]))
	function := v & 0x7
	device := (v >> 3) & 0x1F
	bus := (v >> 8) & 0xFF
	domain := (v >> 16) & 0xFFFFFFFFFFFF
	return fmt.Sprintf("%04x:%02x:%02x.%x", domain, bus, device, function)
}

func convertStringToBdf(pciBusId string) (Bdf, error) {
	var domain uint64
	var bus uint64
	var device uint64
	var function uint64

	_, err := fmt.Sscanf(pciBusId, "%04x:%02x:%02x.%x",
		&domain, &bus, &device, &function)
	if err != nil {
		return Bdf{}, fmt.Errorf("invalid PCI Bus ID format: %s", pciBusId)
	}

	v := (function & 0x7) |
		((device & 0x1F) << 3) |
		((bus & 0xFF) << 8) |
		((domain & 0xFFFFFFFFFFFF) << 16)

	var bdf Bdf
	*(*uint64)(unsafe.Pointer(&bdf[0])) = v

	return bdf, nil
}

// GetGpuDeviceBdf retrieves the Bus-Device-Function (BDF) information for the specified GPU device.
func (l Device) GetGpuDeviceBdf() (Bdf, Return) {
	if l.so.Lookup("amdsmi_get_gpu_device_bdf") != nil {
		return Bdf{}, STATUS_FUNCTION_NOT_FOUND
	}

	var bdf Bdf
	ret := amdsmiGetGpuDeviceBdf(l.handle, &bdf)
	return bdf, ret
}

// GetGpuMetricsInfo retrieves the performance metrics for the specified GPU device.
func (l Device) GetGpuMetricsInfo() (GpuMetrics, Return) {
	if l.so.Lookup("amdsmi_get_gpu_metrics_info") != nil {
		return GpuMetrics{}, STATUS_FUNCTION_NOT_FOUND
	}

	var metrics GpuMetrics
	ret := amdsmiGetGpuMetricsInfo(l.handle, &metrics)
	return metrics, ret
}

// GetGpuVramUsage retrieves the VRAM usage information for the specified GPU device.
func (l Device) GetGpuVramUsage() (VramUsage, Return) {
	if l.so.Lookup("amdsmi_get_gpu_vram_usage") != nil {
		return VramUsage{}, STATUS_FUNCTION_NOT_FOUND
	}

	var vramUsage VramUsage
	ret := amdsmiGetGpuVramUsage(l.handle, &vramUsage)
	return vramUsage, ret
}

// GetGpuEccCount retrieves the ECC error count for the specified GPU device and block.
func (l Device) GetGpuEccCount(block GpuBlock) (ErrorCount, Return) {
	if l.so.Lookup("amdsmi_get_gpu_ecc_count") != nil {
		return ErrorCount{}, STATUS_FUNCTION_NOT_FOUND
	}

	var eccCount ErrorCount
	ret := amdsmiGetGpuEccCount(l.handle, block, &eccCount)
	return eccCount, ret
}

// GetPowerInfo retrieves the power information for the specified GPU device.
func (l Device) GetPowerInfo() (PowerInfo, Return) {
	if l.so.Lookup("amdsmi_get_power_info") != nil {
		return PowerInfo{}, STATUS_FUNCTION_NOT_FOUND
	}

	var powerInfo PowerInfo
	ret := amdsmiGetPowerInfo(l.handle, &powerInfo)
	return powerInfo, ret
}

// GetNumaNodeNumber retrieves the NUMA node number for the specified GPU device.
func (l Device) GetNumaNodeNumber() (uint32, Return) {
	if l.so.Lookup("amdsmi_topo_get_numa_node_number") != nil {
		return 0, STATUS_FUNCTION_NOT_FOUND
	}

	var numaNode uint32
	ret := amdsmiTopoGetNumaNodeNumber(l.handle, &numaNode)
	return numaNode, ret
}

// GetLinkType retrieves the link type and number of hops between two GPU devices.
func (l Device) GetLinkType(device2 Device) (uint64, LinkType, Return) {
	if l.so.Lookup("amdsmi_topo_get_link_type") != nil {
		return 0, 0, STATUS_FUNCTION_NOT_FOUND
	}

	var (
		linkHops uint64
		linkType LinkType
	)
	ret := amdsmiTopoGetLinkType(l.handle, device2.handle, &linkHops, &linkType)
	return linkHops, linkType, ret
}
