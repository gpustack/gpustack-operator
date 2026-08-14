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

// maxProcessRows caps how many rows a per-process read will allocate for. The capacity a caller
// passes came from the library's own count, and a library whose ABI does not match the header it was
// generated against — or one that is simply broken — can report a count that has nothing to do with
// any process list; allocating it would take the whole process down. No real node comes near this
// many rows: it is thousands of processes on a single accelerator, well past what a node's own
// process table holds. A capacity past the ceiling is refused without a call, the same outcome as a
// list too long to read.
const maxProcessRows uint32 = 4096

// GetGpuProcessList returns the processes running on this device, in the library's own units: Mem
// and the Memory_usage family in bytes, Engine_usage in nanoseconds, Sdma_usage in microseconds,
// Cu_occupancy as a count of compute units. Nothing is converted here.
//
// Pids are host pids: a containerized process is named by the pid the host sees, not the pid it
// sees itself, and no translation happens here.
//
// The library requires at least one second to pass before the first call of this query and before
// every call after that, or the values it reports are not valid. Sizing a buffer and filling it are
// two calls to that same query, so they cannot both happen inside one invocation without breaking
// that rule. This method therefore makes exactly one library call, and the caller carries the
// capacity from one invocation to the next — its sampling period is what spaces the calls:
//
//   - maxProcesses of zero asks only how many processes there are. It returns no rows, that count,
//     and success; the caller keeps the count for its next sample.
//   - maxProcesses above zero fills a buffer of that size. It returns the rows written and how many
//     that was.
//
// A buffer the library outgrows is never returned as a list: out-of-resources means the real list is
// longer than the capacity given, so it comes back with no rows, the count the library now reports,
// and that code, for the caller to retry with on its next sample.
//
// A caller that invokes this twice in quick succession gets values the vendor itself calls invalid.
func (l Device) GetGpuProcessList(maxProcesses uint32) ([]ProcInfo, uint32, Return) {
	if l.so.Lookup("amdsmi_get_gpu_process_list") != nil {
		return nil, 0, STATUS_FUNCTION_NOT_FOUND
	}

	// A zero size is how the library is asked for the count instead of the list.
	if maxProcesses == 0 {
		var count uint32
		ret := amdsmiGetGpuProcessList(l.handle, &count, nil)
		if !ret.IsSuccess() && ret != STATUS_OUT_OF_RESOURCES {
			return nil, 0, ret
		}
		return nil, count, STATUS_SUCCESS
	}

	if maxProcesses > maxProcessRows {
		return nil, 0, STATUS_OUT_OF_RESOURCES
	}

	rows := make([]ProcInfo, maxProcesses)
	written := maxProcesses
	ret := amdsmiGetGpuProcessList(l.handle, &written, &rows[0])
	if ret == STATUS_OUT_OF_RESOURCES {
		// The list is longer than the capacity given: report the size it now needs, and no rows.
		return nil, written, STATUS_OUT_OF_RESOURCES
	}
	if !ret.IsSuccess() {
		return nil, 0, ret
	}
	// Clamp to the buffer we sized, so a library reporting more rows written than it was given
	// cannot drive a slice-out-of-range panic.
	if written > maxProcesses {
		written = maxProcesses
	}
	return rows[:written], written, ret
}

// maxProcessGpuQueryAttempts bounds the count-then-fill retries of the membership query below. The
// device set of one process can only grow while it is being read, and it is bounded by the machine's
// accelerator count, so a query that has not settled in this many attempts is answering something
// other than a device set and is reported as unreadable rather than retried forever.
const maxProcessGpuQueryAttempts = 4

// GetComputeProcessGpus returns the indices of the devices a process is using, in the library's own
// device enumeration — the same order GetProcessorHandles yields — and never a partial set.
//
// This exists because GetGpuProcessList's own answer cannot say which device a row belongs to:
// measured on ROCm 7.2.0, that query returns every compute process the driver knows regardless of the
// processor handle it is given, so a caller that trusts the handle attributes one card's processes to
// every card in the machine. Membership has to be established the other way round, by asking each pid
// which devices it holds, which is what this query answers and what rocm-smi itself goes through.
//
// The caller has to map an index back to whatever identity it keys on, and the library offers no
// index-from-handle call to do it with, so that mapping is the caller's to justify — see
// GetProcessorHandles for the enumeration the indices belong to. An index at or past the number of
// devices that enumeration held means the two index spaces are not the same one, which no caller can
// repair by guessing.
//
// The set is either complete or an error: a probe reporting a count is followed by a fill of exactly
// that size, and a library that keeps asking for more than the read accepts is reported as
// insufficient rather than as the part that fit. An empty set with success means the process holds no
// device at all, which is a measurement rather than a refusal.
//
// NO CALLER USES THIS TODAY, AND THE REASON IS NOT AN OVERSIGHT. Measured on ROCm 7.2.0 / AMD SMI
// 26.2.1: the symbol resolves, the signature matches both that release's own header and AMD's
// published documentation, and the call answers INVAL for a process id that is holding a device at
// that moment — with the documented count-probe shape and with a pre-sized buffer alike. The device
// manager reads the same relation through ROCm SMI instead, which answers it per device; see
// pkg/devicemanager/detector/amd/process.go. This wrapper is kept because it is a faithful binding of
// the vendor's API and a later release may implement it.
func (l *AMDSMI) GetComputeProcessGpus(pid uint32) ([]uint32, Return) {
	if l.so.Lookup("amdsmi_get_gpu_compute_process_gpus") != nil {
		return nil, STATUS_FUNCTION_NOT_FOUND
	}

	// A nil array is how the library is asked for the count instead of the set.
	var count uint32
	ret := amdsmiGetGpuComputeProcessGpus(pid, nil, &count)
	if !ret.IsSuccess() && ret != STATUS_INSUFFICIENT_SIZE && ret != STATUS_OUT_OF_RESOURCES {
		return nil, ret
	}

	for range maxProcessGpuQueryAttempts {
		if count == 0 {
			return nil, STATUS_SUCCESS
		}
		// Refuse a count no machine's device set can have rather than allocating it.
		if count > maxProcessRows {
			return nil, STATUS_INSUFFICIENT_SIZE
		}

		indices := make([]uint32, count)
		written := count
		ret := amdsmiGetGpuComputeProcessGpus(pid, &indices[0], &written)
		switch {
		case ret == STATUS_INSUFFICIENT_SIZE, ret == STATUS_OUT_OF_RESOURCES:
			// The set grew between the probe and the fill. Retry at the size the library now asks
			// for; growth has to be strict for the retry to make progress, so a library that
			// repeats the size we just tried is stepped past rather than tried again.
			if written > count {
				count = written
			} else {
				count++
			}
		case !ret.IsSuccess():
			return nil, ret
		default:
			// Clamp to the buffer we sized, so a library reporting more indices written than it was
			// given cannot drive a slice-out-of-range panic.
			if written > count {
				written = count
			}
			return indices[:written], ret
		}
	}

	return nil, STATUS_INSUFFICIENT_SIZE
}
