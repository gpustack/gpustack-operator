package cndev

import "C"
import (
	"fmt"
	"runtime"
	"strconv"
	"unsafe"

	"gpustack.ai/gpustack/binding"
)

// GetDeviceCount returns the number of devices available in the system.
func (l *CNDev) GetDeviceCount() (int, Return) {
	if l.so.Lookup("cndevGetDeviceCount") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	var info CardInfo
	info.Version = VERSION_6
	ret := cndevGetDeviceCount(&info)
	return int(info.Number), ret
}

// GetDeviceHandleByIndex returns a handle for the device at the specified index.
func (l *CNDev) GetDeviceHandleByIndex(index int) (Device, Return) {
	if l.so.Lookup("cndevGetDeviceHandleByIndex") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle cndevDevice
	ret := cndevGetDeviceHandleByIndex(int32(index), &handle)
	return Device{handle: handle, so: l.so}, ret
}

// GetDeviceHandleByPciBusId returns a handle for the device with the specified PCI bus ID.
func (l *CNDev) GetDeviceHandleByPciBusId(pciBusId string) (Device, Return) {
	if l.so.Lookup("cndevGetDeviceHandleByPciBusId") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle cndevDevice
	ret := cndevGetDeviceHandleByPciBusId(pciBusId, &handle)
	return Device{handle: handle, so: l.so}, ret
}

// GetDeviceHandleByUUID returns a handle for the device with the specified UUID.
func (l *CNDev) GetDeviceHandleByUUID(uuid string) (Device, Return) {
	if l.so.Lookup("cndevGetDeviceHandleByUUID") != nil {
		return Device{}, ERROR_FUNCTION_NOT_FOUND
	}

	var handle cndevDevice
	ret := cndevGetDeviceHandleByUUID(uuid, &handle)
	return Device{handle: handle, so: l.so}, ret
}

type Device struct {
	handle cndevDevice
	so     binding.Library
}

func (uuid UUID) String() string {
	return "MLU-" + C.GoString((*C.char)(unsafe.Pointer(&uuid.Uuid[0])))
}

// GetCardName returns the name of the device.
func (l Device) GetCardName() (string, Return) {
	if l.so.Lookup("cndevGetCardNameStringByDevId") == nil {
		return cndevGetCardNameStringByDevId(l.handle), SUCCESS
	}

	if l.so.Lookup("cndevGetCardName") == nil {
		var cardName CardName
		cardName.Version = VERSION_6
		ret := cndevGetCardName(&cardName, l.handle)
		if !ret.IsSuccess() {
			return "", ret
		}

		cardNameId := NameEnum(cardName.Id)
		if l.so.Lookup("cndevGetCardNameString") == nil {
			return cndevGetCardNameString(cardNameId), SUCCESS
		}

		switch cardNameId {
		case DEVICE_TYPE_MLU100:
			return "MLU100", ret
		case DEVICE_TYPE_MLU270:
			return "MLU270", ret
		case DEVICE_TYPE_MLU220_M2, DEVICE_TYPE_MLU220_EDGE, DEVICE_TYPE_MLU220_EVB, DEVICE_TYPE_MLU220_M2i:
			return "MLU220", ret
		case DEVICE_TYPE_MLU290:
			return "MLU290", ret
		case DEVICE_TYPE_MLU370:
			return "MLU370", ret
		case DEVICE_TYPE_MLU365:
			return "MLU365", ret
		case DEVICE_TYPE_CE3226:
			return "CE3226", ret
		case DEVICE_TYPE_MLU590:
			return "MLU590", ret
		case DEVICE_TYPE_MLU585:
			return "MLU585", ret
		case DEVICE_TYPE_MLU580:
			return "MLU580", ret
		case DEVICE_TYPE_MLU570:
			return "MLU570", ret
		}
		return "MLU", ret
	}

	return "", ERROR_FUNCTION_NOT_FOUND
}

func (sn CardSN) String() string {
	return strconv.FormatUint(sn.Sn, 16)
}

// GetCardSN returns the serial number of the device.
func (l Device) GetCardSN() (CardSN, Return) {
	if l.so.Lookup("cndevGetCardSN") != nil {
		return CardSN{}, ERROR_FUNCTION_NOT_FOUND
	}

	var cardSN CardSN
	cardSN.Version = VERSION_6
	ret := cndevGetCardSN(&cardSN, l.handle)
	return cardSN, ret
}

// GetUUID returns the UUID of the device.
func (l Device) GetUUID() (string, Return) {
	if l.so.Lookup("cndevGetUUID") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	var uuid UUID
	uuid.Version = VERSION_6
	ret := cndevGetUUID(&uuid, l.handle)
	return uuid.String(), ret
}

// GetMemoryInfoV returns the memory information for the device.
func (l Device) GetMemoryInfoV() MemoryHandler {
	return MemoryHandler(l)
}

type MemoryHandler Device

func (l MemoryHandler) V2() (MemoryInfoV2, Return) {
	if l.so.Lookup("cndevGetMemoryUsageV2") != nil {
		return MemoryInfoV2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var memInfo MemoryInfoV2
	ret := cndevGetMemoryUsageV2(&memInfo, l.handle)
	return memInfo, ret
}

// GetVersionInfo returns the version information for the device.
func (l Device) GetVersionInfo() (VersionInfo, Return) {
	if l.so.Lookup("cndevGetVersionInfo") != nil {
		return VersionInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var versionInfo VersionInfo
	// Version is an input: cnDev rejects a versioned struct that does not declare which layout the
	// caller speaks, and every other wrapper in this file sets it. Leaving it zero made a conforming
	// driver answer ERROR_UNSUPPORTED_API_VERSION instead of the version.
	versionInfo.Version = VERSION_6
	ret := cndevGetVersionInfo(&versionInfo, l.handle)
	return versionInfo, ret
}

// GetCardHealthStateV returns the health state handler.
func (l Device) GetCardHealthStateV() CardHealthStateHandler {
	return CardHealthStateHandler(l)
}

type CardHealthStateHandler Device

func (l CardHealthStateHandler) V1() (CardHealthStateV2, Return) {
	if l.so.Lookup("cndevGetCardHealthState") != nil {
		return CardHealthStateV2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var healthState CardHealthState
	healthState.Version = VERSION_6
	ret := cndevGetCardHealthState(&healthState, l.handle)
	return CardHealthStateV2{
		Health:      healthState.Health,
		DeviceState: healthState.DeviceState,
		DriverState: healthState.DriverState,
	}, ret
}

func (l CardHealthStateHandler) V2() (CardHealthStateV2, Return) {
	if l.so.Lookup("cndevGetCardHealthStateV2") != nil {
		return CardHealthStateV2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var healthStateV2 CardHealthStateV2
	ret := cndevGetCardHealthStateV2(&healthStateV2, l.handle)
	return healthStateV2, ret
}

// GetPCIeInfoV returns the PCIe information handler.
func (l Device) GetPCIeInfoV() PCIeInfoHandler {
	return PCIeInfoHandler(l)
}

type PCIeInfoHandler Device

func (info PCIeInfoV2) GetBusId() string {
	return fmt.Sprintf("%04x:%02x:%02x.%d", info.Domain, info.Bus, info.Device, info.Function)
}

func (l PCIeInfoHandler) V2() (PCIeInfoV2, Return) {
	if l.so.Lookup("cndevGetPCIeInfoV2") != nil {
		return PCIeInfoV2{}, ERROR_FUNCTION_NOT_FOUND
	}

	var pcieInfo PCIeInfoV2
	ret := cndevGetPCIeInfoV2(&pcieInfo, l.handle)
	return pcieInfo, ret
}

// GetNumaNodeId returns the NUMA node ID associated with the device.
func (l Device) GetNumaNodeId() (NUMANodeId, Return) {
	if l.so.Lookup("cndevGetNUMANodeIdByDevId") != nil {
		return NUMANodeId{}, ERROR_FUNCTION_NOT_FOUND
	}

	var numaNodeId NUMANodeId
	numaNodeId.Version = VERSION_6
	ret := cndevGetNUMANodeIdByDevId(&numaNodeId, l.handle)
	return numaNodeId, ret
}

func (l Device) GetPowerInfo() (DevicePowerInfo, Return) {
	if l.so.Lookup("cndevGetDevicePowerInfo") != nil {
		return DevicePowerInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var powerInfo DevicePowerInfo
	ret := cndevGetDevicePowerInfo(&powerInfo, l.handle)
	return powerInfo, ret
}

func (l Device) GetECCInfo() (ECCInfo, Return) {
	if l.so.Lookup("cndevGetECCInfo") != nil {
		return ECCInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var eccInfo ECCInfo
	eccInfo.Version = VERSION_6
	ret := cndevGetECCInfo(&eccInfo, l.handle)
	return eccInfo, ret
}

func (l Device) GetTemperatureInfo() (TemperatureInfo, Return) {
	if l.so.Lookup("cndevGetTemperatureInfo") != nil {
		return TemperatureInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var tempInfo TemperatureInfo
	ret := cndevGetTemperatureInfo(&tempInfo, l.handle)
	return tempInfo, ret
}

func (l Device) GetUtilizationInfo() (UtilizationInfo, Return) {
	if l.so.Lookup("cndevGetDeviceUtilizationInfo") != nil {
		return UtilizationInfo{}, ERROR_FUNCTION_NOT_FOUND
	}

	var utilInfo UtilizationInfo
	utilInfo.Version = VERSION_6
	ret := cndevGetDeviceUtilizationInfo(&utilInfo, l.handle)
	return utilInfo, ret
}

// GetSMLUMode reports whether sMLU mode is enabled on the device.
func (l Device) GetSMLUMode() (SMLUMode, Return) {
	if l.so.Lookup("cndevGetSMLUMode") != nil {
		return SMLUMode{}, ERROR_FUNCTION_NOT_FOUND
	}

	var mode SMLUMode
	mode.Version = VERSION_6
	ret := cndevGetSMLUMode(&mode, l.handle)
	return mode, ret
}

// SetSMLUMode enables or disables sMLU mode on the device.
func (l Device) SetSMLUMode(enabled bool) Return {
	if l.so.Lookup("cndevSetSMLUMode") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	var mode SMLUMode
	mode.Version = VERSION_6
	mode.SmluMode = uint32(FEATURE_DISABLED)
	if enabled {
		mode.SmluMode = uint32(FEATURE_ENABLED)
	}
	return cndevSetSMLUMode(&mode, l.handle)
}

// CreateSMluProfile creates an sMLU profile from the given quota and returns its profile ID.
func (l Device) CreateSMluProfile(profile SMluSet) (int32, Return) {
	if l.so.Lookup("cndevCreateSMluProfileInfo") != nil {
		return 0, ERROR_FUNCTION_NOT_FOUND
	}

	profile.Version = VERSION_6
	var profileID int32
	ret := cndevCreateSMluProfileInfo(&profile, &profileID, l.handle)
	return profileID, ret
}

// DestroySMluProfile destroys the sMLU profile with the given profile ID.
func (l Device) DestroySMluProfile(profileID int32) Return {
	if l.so.Lookup("cndevDestroySMluProfileInfo") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	return cndevDestroySMluProfileInfo(profileID, l.handle)
}

// CreateSMluInstance creates an sMLU instance from the given profile ID, naming it name. The
// instance is addressed by name for every subsequent operation, so the handle is not returned.
func (l Device) CreateSMluInstance(profileID uint32, name string) Return {
	if l.so.Lookup("cndevCreateSMluInstanceByProfileId") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	cName, cNameAllocMap := unpackPCharString(name)
	var handle cndevMluInstance
	ret := cndevCreateSMluInstanceByProfileId(&handle, profileID, l.handle, (*byte)(unsafe.Pointer(cName)))
	runtime.KeepAlive(cNameAllocMap)
	return ret
}

// DestroySMluInstanceByName destroys the sMLU instance with the given name on the device.
func (l Device) DestroySMluInstanceByName(name string) Return {
	if l.so.Lookup("cndevDestroySMluInstanceByInstanceName") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	cName, cNameAllocMap := unpackPCharString(name)
	ret := cndevDestroySMluInstanceByInstanceName(l.handle, (*byte)(unsafe.Pointer(cName)))
	runtime.KeepAlive(cNameAllocMap)
	return ret
}

// GetAllSMluInstanceInfo returns the information of every sMLU instance on the device.
func (l Device) GetAllSMluInstanceInfo() ([]SMluInfo, Return) {
	if l.so.Lookup("cndevGetAllSMluInstanceInfo") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	// The count parameter is in/out: a probe with no buffer reports how many instances exist, so
	// INSUFFICIENT_SPACE from the probe is expected and is not treated as a hard failure.
	var count int32
	if ret := cndevGetAllSMluInstanceInfo(&count, nil, l.handle); !ret.IsSuccess() && ret != ERROR_INSUFFICIENT_SPACE {
		return nil, ret
	}
	if count <= 0 {
		return nil, SUCCESS
	}

	infos := make([]SMluInfo, count)
	for i := range infos {
		infos[i].Version = VERSION_6
	}
	ret := cndevGetAllSMluInstanceInfo(&count, &infos[0], l.handle)
	if !ret.IsSuccess() {
		return nil, ret
	}
	// Clamp to the buffer we sized, so a driver that reports a larger out-count than the
	// in-count cannot drive a slice-out-of-range panic.
	if int(count) > len(infos) {
		count = int32(len(infos))
	}
	return infos[:count], ret
}

// GetInstanceName returns the sMLU instance name from the NUL-terminated C char array.
func (info *SMluInfo) GetInstanceName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.InstanceName[0])))
}

// GetDevNodeName returns the sMLU instance device node path from the NUL-terminated C char array.
func (info *SMluInfo) GetDevNodeName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.DevNodeName[0])))
}

// GetSMluProfileIds returns the IDs of every sMLU profile defined on the device.
func (l Device) GetSMluProfileIds() ([]int32, Return) {
	if l.so.Lookup("cndevGetSMluProfileIdInfo") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	var info SMluProfileIdInfo
	info.Version = VERSION_6
	ret := cndevGetSMluProfileIdInfo(&info, l.handle)
	if !ret.IsSuccess() {
		return nil, ret
	}
	n := int(info.Count)
	if n > len(info.ProfileId) {
		n = len(info.ProfileId)
	}
	ids := make([]int32, n)
	copy(ids, info.ProfileId[:n])
	return ids, ret
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
// Because every cnDev struct declares its own layout, stamping the version of each row is part of
// fill rather than of this loop, which re-runs fill on every attempt.
//
// The returned rows are exactly what the library wrote, in the library's own units.
func readProcessRows[T any](fill func(count *uint32, rows []T) Return) ([]T, Return) {
	// A probe with no buffer reports how many rows the library holds. It answers a zero-sized
	// buffer with the short-buffer code, which is the probe working, not failing.
	var count uint32
	if ret := fill(&count, nil); !ret.IsSuccess() && ret != ERROR_INSUFFICIENT_SPACE {
		return nil, ret
	}

	for range maxProcessQueryAttempts {
		if count == 0 {
			return nil, SUCCESS
		}
		// Refuse a count no process list can have rather than allocating it.
		if count > maxProcessRows {
			return nil, ERROR_INSUFFICIENT_SPACE
		}

		rows := make([]T, count)
		written := count
		ret := fill(&written, rows)
		switch {
		case ret == ERROR_INSUFFICIENT_SPACE:
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

	return nil, ERROR_INSUFFICIENT_SPACE
}

// GetProcessInfo returns one row per process running on this device, with PhysicalMemoryUsed and
// VirtualMemoryUsed in the KiB the library reports them in — no unit is converted here.
//
// Pids are host pids: a containerized process is named by the pid the host sees, not the pid it
// sees itself, and no translation happens here.
func (l Device) GetProcessInfo() ([]ProcessInfo, Return) {
	if l.so.Lookup("cndevGetProcessInfo") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	return readProcessRows(func(count *uint32, rows []ProcessInfo) Return {
		if len(rows) == 0 {
			return cndevGetProcessInfo(count, nil, l.handle)
		}
		// Version is an input: cnDev rejects a versioned struct that does not declare which
		// layout the caller expects, and every row in the buffer carries its own version word.
		for i := range rows {
			rows[i].Version = VERSION_6
		}
		return cndevGetProcessInfo(count, &rows[0], l.handle)
	})
}

// GetProcessUtilization returns one row per process running on this device, with IpuUtil, JpuUtil,
// VpuDecUtil, VpuEncUtil and MemUtil as the percentages the library reports.
//
// A process the library does not report is absent from the result rather than present at zero, and
// ERROR_NOT_SUPPORTED — the device cannot account utilization per process at all — is returned as
// itself rather than as an empty list.
func (l Device) GetProcessUtilization() ([]ProcessUtilization, Return) {
	if l.so.Lookup("cndevGetProcessUtilization") != nil {
		return nil, ERROR_FUNCTION_NOT_FOUND
	}

	return readProcessRows(func(count *uint32, rows []ProcessUtilization) Return {
		if len(rows) == 0 {
			return cndevGetProcessUtilization(count, nil, l.handle)
		}
		for i := range rows {
			rows[i].Version = VERSION_6
		}
		return cndevGetProcessUtilization(count, &rows[0], l.handle)
	})
}
