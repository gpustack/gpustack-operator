package dcmi

import "C"
import (
	"fmt"
	"strconv"
	"strings"
	"unsafe"
)

// GetCardList retrieves the list of available DCMI cards and their count.
func (l *DCMI) GetCardList() (int32, []int32, Return) {
	var cardNum int32
	cardList := make([]int32, MAX_CARD_NUM)
	ret := Return(dcmiGetCardList(&cardNum, &cardList[0], MAX_CARD_NUM))
	return cardNum, cardList[:cardNum], ret
}

// GetDeviceNumInCard retrieves the number of devices available in the specified DCMI card.
func (l *DCMI) GetDeviceNumInCard(cardId int32) (int32, Return) {
	var deviceNum int32
	ret := Return(dcmiGetDeviceNumInCard(cardId, &deviceNum))
	return deviceNum, ret
}

// GetDeviceHandleByCardAndIndex retrieves a handle for the device at the specified index in the specified DCMI card.
func (l *DCMI) GetDeviceHandleByCardAndIndex(cardId, deviceId int32) Device {
	return Device{cardId: cardId, deviceId: deviceId}
}

type Device struct {
	cardId   int32
	deviceId int32
}

// GetType retrieves the type of the device, such as NPU, MCU, or CPU.
func (l Device) GetType() (UnitType, Return) {
	var deviceType UnitType
	ret := Return(dcmiGetDeviceType(l.cardId, l.deviceId, &deviceType))
	return deviceType, ret
}

// GetChipInfoV retrieves the chip information of the device,
// returning a ChipInfoHandler for further queries.
func (l Device) GetChipInfoV() ChipInfoHandler {
	return ChipInfoHandler(l)
}

type ChipInfoHandler Device

func (l ChipInfoHandler) V1() (ChipInfoV2, Return) {
	var info ChipInfo
	ret := Return(dcmiGetDeviceChipInfo(l.cardId, l.deviceId, &info))
	return ChipInfoV2{
		Chip_type:  info.Chip_type,
		Chip_name:  info.Chip_name,
		Chip_ver:   info.Chip_ver,
		Aicore_cnt: info.Aicore_cnt,
	}, ret
}

func (info ChipInfoV2) GetChipType() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Chip_type[0])))
}

func (info ChipInfoV2) GetChipName() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Chip_name[0])))
}

func (info ChipInfoV2) GetChipVersion() string {
	return C.GoString((*C.char)(unsafe.Pointer(&info.Chip_ver[0])))
}

func (l ChipInfoHandler) V2() (ChipInfoV2, Return) {
	var info ChipInfoV2
	ret := Return(dcmiGetDeviceChipInfoV2(l.cardId, l.deviceId, &info))
	return info, ret
}

// GetVDieV retrieves the die information of the device,
// returning a VDieHandler for further queries.
func (l Device) GetVDieV() VDieHandler {
	return VDieHandler(l)
}

type VDieHandler Device

func (dieId DieId) String() string {
	var parts []string
	for i := 0; i < len(dieId.Die); i++ {
		parts = append(parts, strings.ToUpper(strconv.FormatUint(uint64(dieId.Die[i]), 16)))
	}
	return strings.Join(parts, " ")
}

func (l VDieHandler) V1() (DieId, Return) {
	var socDie SocDie
	ret := Return(dcmiGetDeviceDie(l.cardId, l.deviceId, &socDie))
	return DieId(socDie), ret
}

func (l VDieHandler) V2() (DieId, Return) {
	var dieId DieId
	ret := Return(dcmiGetDeviceDieV2(l.cardId, l.deviceId, VDIE, &dieId))
	return dieId, ret
}

// GetUtilizationRateV retrieves the utilization rates of various components of the device,
// returning a UtilizationRateHandler for further queries.
func (l Device) GetUtilizationRateV() UtilizationRateHandler {
	return UtilizationRateHandler(l)
}

type UtilizationRateHandler Device

func (l UtilizationRateHandler) V1() (MultiUtilizationInfo, Return) {
	var info MultiUtilizationInfo
	ret := Return(dcmiGetDeviceUtilizationRate(l.cardId, l.deviceId, UTILIZATION_RATE_AICPU, &info.Aic_util))
	if !ret.IsSuccess() {
		return info, ret
	}
	ret = Return(dcmiGetDeviceUtilizationRate(l.cardId, l.deviceId, UTILIZATION_RATE_VECTORCORE, &info.Aiv_util))
	if !ret.IsSuccess() {
		return info, ret
	}
	ret = Return(dcmiGetDeviceUtilizationRate(l.cardId, l.deviceId, UTILIZATION_RATE_AICORE, &info.Aicore_util))
	if !ret.IsSuccess() {
		return info, ret
	}
	ret = Return(dcmiGetDeviceUtilizationRate(l.cardId, l.deviceId, UTILIZATION_RATE_NPU, &info.Npu_util))
	return info, ret
}

func (l UtilizationRateHandler) V2() (MultiUtilizationInfo, Return) {
	var info MultiUtilizationInfo
	ret := Return(dcmiGetDeviceUtilizationRateV2(l.cardId, l.deviceId, &info))
	return info, ret
}

// GetTemperature retrieves the current temperature of the device.
func (l Device) GetTemperature() (int32, Return) {
	var temp int32
	ret := Return(dcmiGetDeviceTemperature(l.cardId, l.deviceId, &temp))
	return temp, ret
}

// GetPowerInfo retrieves the current power information of the device.
func (l Device) GetPowerInfo() (int32, Return) {
	var power int32
	ret := Return(dcmiGetDevicePowerInfo(l.cardId, l.deviceId, &power))
	return power, ret
}

// GetPhysicalID retrieves the physical ID of the device,
// which is used for identifying the device in a physical topology.
func (l Device) GetPhysicalID() (uint32, Return) {
	var (
		logId int32
		phyId uint32
	)
	ret := Return(dcmiGetDeviceLogicId(&logId, l.cardId, l.deviceId))
	if ret.IsSuccess() {
		ret = Return(dcmiGetDevicePhyidFromLogicid(uint32(logId), &phyId))
	}
	return phyId, ret
}

// GetPcieInfoV retrieves the PCIe information of the device,
// returning a PcieInfoHandler for further queries.
func (l Device) GetPcieInfoV() PcieInfoHandler {
	return PcieInfoHandler(l)
}

type PcieInfoHandler Device

func (info PcieInfoAll) GetBusId() string {
	return fmt.Sprintf("%04x:%02x:%02x.%01x", info.Domain, info.Bdf_busid, info.Bdf_deviceid, info.Bdf_funcid)
}

func (l PcieInfoHandler) V1() (PcieInfoAll, Return) {
	var info PcieInfo
	ret := Return(dcmiGetDevicePcieInfo(l.cardId, l.deviceId, &info))
	return PcieInfoAll{
		Venderid:     info.Venderid,
		Subvenderid:  info.Subvenderid,
		Deviceid:     info.Deviceid,
		Subdeviceid:  info.Subdeviceid,
		Bdf_busid:    info.Bdf_busid,
		Bdf_deviceid: info.Bdf_deviceid,
		Bdf_funcid:   info.Bdf_funcid,
	}, ret
}

func (l PcieInfoHandler) V2() (PcieInfoAll, Return) {
	var info PcieInfoAll
	ret := Return(dcmiGetDevicePcieInfoV2(l.cardId, l.deviceId, &info))
	return info, ret
}

// GetHbmInfo retrieves the current HBM (High Bandwidth Memory) information of the device.
func (l Device) GetHbmInfo() (HbmInfo, Return) {
	var info HbmInfo
	ret := Return(dcmiGetDeviceHbmInfo(l.cardId, l.deviceId, &info))
	return info, ret
}

// GetMemoryInfoV retrieves the current memory information of the device,
// returning a MemoryHandler for further queries.
func (l Device) GetMemoryInfoV() MemoryHandler {
	return MemoryHandler(l)
}

type MemoryHandler Device

func (l MemoryHandler) V2() (GetMemoryInfo, Return) {
	var info MemoryInfo
	ret := Return(dcmiGetDeviceMemoryInfoV2(l.cardId, l.deviceId, &info))
	return GetMemoryInfo{
		Memory_size: info.Size,
		Freq:        info.Freq,
		Utiliza:     info.Utiliza,
	}, ret
}

func (l MemoryHandler) V3() (GetMemoryInfo, Return) {
	var info GetMemoryInfo
	ret := Return(dcmiGetDeviceMemoryInfoV3(l.cardId, l.deviceId, &info))
	return info, ret
}

// GetEccInfo retrieves the current ECC (Error-Correcting Code) information of the device for the specified device type.
func (l Device) GetEccInfo(deviceType DeviceType) (EccInfo, Return) {
	var info EccInfo
	ret := Return(dcmiGetDeviceEccInfo(l.cardId, l.deviceId, deviceType, &info))
	return info, ret
}

// GetAffinityCPUInfo retrieves the affinity CPU information of the device as a string.
func (l Device) GetAffinityCPUInfo() (string, Return) {
	info := make([]byte, TOPO_INFO_MAX_LENGTH)
	var infoLength int32
	ret := Return(dcmiGetAffinityCpuInfoByDeviceId(l.cardId, l.deviceId, &info[0], &infoLength))
	return string(info[:infoLength]), ret
}

// GetTopoInfo retrieves the topology information between two devices, returning it as an integer.
func (l Device) GetTopoInfo(device2 Device) (int32, Return) {
	var topoInfo int32
	ret := Return(dcmiGetTopoInfoByDeviceId(l.cardId, l.deviceId, device2.cardId, device2.deviceId, &topoInfo))
	return topoInfo, ret
}

func (ipAddr IpAddr) String() string {
	if ipAddr.Ip_type == IPADDR_TYPE_V4 {
		return fmt.Sprintf("%d.%d.%d.%d", ipAddr.U_addr[0], ipAddr.U_addr[1], ipAddr.U_addr[2], ipAddr.U_addr[3])
	}
	if ipAddr.Ip_type == IPADDR_TYPE_V6 {
		var parts []string
		for i := 0; i < 16; i += 2 {
			part := uint16(ipAddr.U_addr[i])<<8 | uint16(ipAddr.U_addr[i+1])
			parts = append(parts, fmt.Sprintf("%x", part))
		}
		return strings.Join(parts, ":")
	}
	return ""
}

// GetIp retrieves the IP address and subnet mask of the device for the specified port type.
func (l Device) GetIp(portType PortType, portId int32) (IpAddr, IpAddr, Return) {
	var ipAddr IpAddr
	var subnetMask IpAddr
	ret := Return(dcmiGetDeviceIp(l.cardId, l.deviceId, portType, portId, &ipAddr, &subnetMask))
	return ipAddr, subnetMask, ret
}

// GetGateway retrieves the gateway IP address of the device for the specified port type.
func (l Device) GetGateway(portType PortType, portId int32) (IpAddr, Return) {
	var gateway IpAddr
	ret := Return(dcmiGetDeviceGateway(l.cardId, l.deviceId, portType, portId, &gateway))
	return gateway, ret
}

type VDeviceInfo struct {
	TotalResource SocTotalResource
	FreeResource  SocFreeResource
	Items         []VdevQuery
}

// GetVDeviceInfo retrieves the virtual device information of the device,
// including total resources, free resources, and individual virtual device queries.
func (l Device) GetVDeviceInfo() (VDeviceInfo, Return) {
	var info VDeviceInfo

	totalResourcePtr := unsafe.Pointer(&info.TotalResource)
	totalResourceSize := uint32(unsafe.Sizeof(info.TotalResource))
	ret := Return(dcmiGetDeviceInfo(
		l.cardId, l.deviceId,
		MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_TOTAL_RESOURCE,
		totalResourcePtr, &totalResourceSize,
	))
	if !ret.IsSuccess() {
		return VDeviceInfo{}, ret
	}

	freeResourcePtr := unsafe.Pointer(&info.FreeResource)
	freeResourceSize := uint32(unsafe.Sizeof(info.FreeResource))
	ret = Return(dcmiGetDeviceInfo(
		l.cardId, l.deviceId,
		MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_FREE_RESOURCE,
		freeResourcePtr, &freeResourceSize,
	))
	if !ret.IsSuccess() {
		return VDeviceInfo{}, ret
	}

	for i := uint32(0); i < info.TotalResource.Vdev_num; i++ {
		var vDevQuery VdevQuery
		vDevQuery.Vdev_id = info.TotalResource.Vdev_id[i]
		vDevQueryPtr := unsafe.Pointer(&vDevQuery)
		vDevQuerySize := uint32(unsafe.Sizeof(vDevQuery))
		ret = Return(dcmiGetDeviceInfo(
			l.cardId, l.deviceId,
			MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_VDEV_RESOURCE,
			vDevQueryPtr, &vDevQuerySize,
		))
		if !ret.IsSuccess() {
			return VDeviceInfo{}, ret
		}

		var vDevActivity VdevQuery
		vDevActivity.Vdev_id = vDevQuery.Vdev_id
		vDevActivityPtr := unsafe.Pointer(&vDevActivity)
		vDevActivitySize := uint32(unsafe.Sizeof(vDevActivity))
		ret = Return(dcmiGetDeviceInfo(
			l.cardId, l.deviceId,
			MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_VDEV_ACTIVITY,
			vDevActivityPtr, &vDevActivitySize,
		))
		if ret.IsSuccess() {
			cmp := vDevActivity.Query_info.Computing
			vDevQuery.Query_info.Computing.Vdev_aicore_utilization = cmp.Vdev_aicore_utilization
			vDevQuery.Query_info.Computing.Vdev_memory_total = cmp.Vdev_memory_total
			vDevQuery.Query_info.Computing.Vdev_memory_free = cmp.Vdev_memory_free
		}

		info.Items = append(info.Items, vDevQuery)
	}

	return info, ret
}

// GetShareEnabled reports whether container-share mode is enabled for the device.
// Measured on a 910B2 at driver 25.5.1: with the mode disabled, a second container that opens
// the same device fails, while the first one and any whole-card user are unaffected. Whether
// every co-tenancy arrangement depends on the mode has not been established, so treat this as
// the driver's own single-container guard rather than as a complete rule.
// A driver that does not implement the query returns ERROR_FUNCTION_NOT_FOUND.
func (l Device) GetShareEnabled() (bool, Return) {
	var enableFlag int32
	ret := Return(dcmiGetDeviceShareEnable(l.cardId, l.deviceId, &enableFlag))
	return enableFlag != 0, ret
}

// SetShareEnabled enables or disables container-share mode for the device.
// The flag lives in the driver rather than in this process, so it outlives the caller,
// and it is reset by a reboot unless the driver's share-config recover mode is also on.
func (l Device) SetShareEnabled(enabled bool) Return {
	var enableFlag int32
	if enabled {
		enableFlag = 1
	}
	return Return(dcmiSetDeviceShareEnable(l.cardId, l.deviceId, enableFlag))
}

const (
	// maxProcMemInfoRows is how many process rows one read of a device accepts. It is far above
	// any real workload — a handful of processes hold an accelerator, not hundreds — and the
	// margin is deliberate, because the library cannot be told where the buffer ends.
	maxProcMemInfoRows = 256
	// procMemInfoGuardRows is the tail allocated past maxProcMemInfoRows and left for the library
	// to overrun into. Its whole purpose is that the overrun lands in memory this package owns.
	//
	// It is far larger than the list this read accepts, and that asymmetry is the point: the tail
	// cannot PREVENT an overrun — nothing can, while the count is output-only — it can only be wide
	// enough that a plausible one stays inside this allocation. A row is a handful of bytes, so the
	// whole buffer costs tens of kilobytes per read and buys the difference between a refused read
	// and corrupted memory elsewhere in the process. The count the driver writes is still checked
	// against the accepted rows, so widening this never widens what a caller is handed.
	procMemInfoGuardRows = 4096
	// procMemInfoGuard marks an unwritten guard row. Process ids are positive, so the library
	// writing anything at all over a guard row changes it.
	procMemInfoGuard int32 = -1
)

// GetProcessMemoryUsage returns one row per process holding memory on the device, with
// Mem_usage in bytes as the library reports it.
//
// The count is an OUTPUT ONLY parameter — measured against driver 25.5.1, a count of 0 passed in
// still had the first row written and the count overwritten with the real one — so there is no way
// to tell the library how large the buffer is, and a device with more processes than the buffer
// holds would have it write past the end. The buffer is therefore over-allocated with a guard tail
// the library is not expected to reach: a written guard row means the device held more processes
// than a read accepts, which returns ERROR_LIST_TRUNCATED rather than a shorter list, and the
// overrun is confined to memory this function owns.
//
// A truncated read is an incomplete read, not a small one. Returning the rows that did fit would
// present part of a device's processes as all of them, which is how a per-tenant figure computed
// from these rows would come out plausible and wrong.
func (l Device) GetProcessMemoryUsage() ([]ProcMemInfo, Return) {
	rows := make([]ProcMemInfo, maxProcMemInfoRows+procMemInfoGuardRows)
	for i := maxProcMemInfoRows; i < len(rows); i++ {
		rows[i].Id = procMemInfoGuard
	}

	var procNum int32
	ret := Return(dcmiGetDeviceResourceInfo(l.cardId, l.deviceId, &rows[0], &procNum))
	if !ret.IsSuccess() {
		return nil, ret
	}

	for i := maxProcMemInfoRows; i < len(rows); i++ {
		if rows[i].Id != procMemInfoGuard {
			return nil, ERROR_LIST_TRUNCATED
		}
	}
	// The count is checked as well as the guard: a library that reports more than it wrote would
	// otherwise have the rows beyond what it filled read as processes holding no memory.
	if procNum < 0 || procNum > maxProcMemInfoRows {
		return nil, ERROR_LIST_TRUNCATED
	}
	return rows[:procNum], ret
}
