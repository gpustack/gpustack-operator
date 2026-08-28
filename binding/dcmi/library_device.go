package dcmi

import "C"
import (
	"fmt"
	"strconv"
	"strings"
	"unsafe"
)

// GetCardList retrieves the list of available DCMI cards and their count.
//
// The two API generations disagree about what a card is, and this is the one place that difference
// is resolved. V1 enumerates cards, each holding one or more devices. V2 has no card level at all:
// it enumerates devices flat, indexed by the number V1 calls the logic id. So on V2 this returns
// the device list, and every entry is presented as a card holding exactly one device --
// cardId == devId == logicId -- which makes (cardId, 0) an address this binding genuinely accepts.
// No other second coordinate is; see devID.
//
// The count the library reports is validated before it is used to slice. Both generations are told
// how long the buffer is, so a count within it is a complete read, but a negative count or one past
// the end is a corrupt or truncated answer and is reported as ERROR_LIST_TRUNCATED rather than
// panicking or handing back a partial list as though it were whole.
func (l *DCMI) GetCardList() (int32, []int32, Return) {
	var (
		num  int32
		ret  Return
		list = make([]int32, MAX_CARD_NUM)
	)

	if apiVersion() == APIVersionV2 {
		ret = Return(dcmiv2GetDeviceList(&list[0], &num, int32(len(list))))
	} else {
		ret = Return(dcmiGetCardList(&num, &list[0], int32(len(list))))
	}
	if !ret.IsSuccess() {
		return 0, nil, ret
	}
	if num < 0 || num > int32(len(list)) {
		return 0, nil, ERROR_LIST_TRUNCATED
	}

	return num, list[:num], SUCCESS
}

// GetDeviceNumInCard retrieves the number of devices available in the specified DCMI card.
//
// V2 has no card level, so GetCardList presents each device id as a card holding exactly one device
// (see there). The count is therefore 1 for a device id that generation recognizes, and the call
// refuses one it does not: a caller walking a stale list must not have its indices accepted in
// silence. The recognition check is the cheapest V2 query that takes a device id.
func (l *DCMI) GetDeviceNumInCard(cardId int32) (int32, Return) {
	if apiVersion() == APIVersionV2 {
		var deviceType UnitType
		if ret := Return(dcmiv2GetDeviceType(cardId, &deviceType)); !ret.IsSuccess() {
			return 0, ret
		}

		return 1, SUCCESS
	}

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

// devID resolves a handle to the single device id the V2 API addresses, and refuses any address the
// V2 translation does not actually produce.
//
// GetCardList presents device id N as card N holding one device, so (N, 0) is the only honest pair.
// Accepting (N, 7) would quietly serve device N to a caller working from a stale or corrupt index --
// a whole-card reading attributed to the wrong device reads as plausible, which is the worst kind of
// wrong for a capacity figure.
func (l Device) devID() (int32, Return) {
	if l.deviceId != 0 {
		return 0, ERROR_INVALID_DEVICE_ID
	}

	return l.cardId, SUCCESS
}

// deviceInfo issues the generic device-info query on whichever generation is loaded, so that the
// callers composing several of these queries in a row do not each carry the branch.
func (l Device) deviceInfo(mainCmd MainCmd, subCmd uint32, buf unsafe.Pointer, size *uint32) Return {
	if apiVersion() == APIVersionV2 {
		devId, ret := l.devID()
		if !ret.IsSuccess() {
			return ret
		}

		return Return(dcmiv2GetDeviceInfo(devId, mainCmd, subCmd, buf, size))
	}

	return Return(dcmiGetDeviceInfo(l.cardId, l.deviceId, mainCmd, subCmd, buf, size))
}

// GetType retrieves the type of the device, such as NPU, MCU, or CPU.
func (l Device) GetType() (UnitType, Return) {
	var deviceType UnitType

	if apiVersion() == APIVersionV2 {
		devId, ret := l.devID()
		if !ret.IsSuccess() {
			return deviceType, ret
		}
		ret = Return(dcmiv2GetDeviceType(devId, &deviceType))

		return deviceType, ret
	}

	ret := Return(dcmiGetDeviceType(l.cardId, l.deviceId, &deviceType))

	return deviceType, ret
}

// GetChipInfoV retrieves the chip information of the device,
// returning a ChipInfoHandler for further queries.
func (l Device) GetChipInfoV() ChipInfoHandler {
	return ChipInfoHandler(l)
}

type ChipInfoHandler Device

// V1 reads the older, narrower chip-info struct.
//
// It does not adapt to V2, and the reason is not that V2 lacks a counterpart -- it is that V2's
// only chip-info entry point is the one V2() below already reaches, and every caller tries V2()
// first. Adapting here would issue the same call twice and never run.
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

	if apiVersion() == APIVersionV2 {
		devId, ret := Device(l).devID()
		if !ret.IsSuccess() {
			return info, ret
		}
		ret = Return(dcmiv2GetDeviceChipInfo(devId, &info))

		return info, ret
	}

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

// V1 reads the SoC die through the older entry point.
//
// It does not adapt to V2: that generation's only die query is the one V2() below reaches, and
// callers try V2() first, so adapting here would repeat a call that has already been made.
func (l VDieHandler) V1() (DieId, Return) {
	var socDie SocDie
	ret := Return(dcmiGetDeviceDie(l.cardId, l.deviceId, &socDie))
	return DieId(socDie), ret
}

// V2 reads the die whose id identifies the chip.
//
// On a V2 driver it asks for the virtual die first and then, only if that is refused, the DDie --
// which the vendor names as the A5 chip's uuid. Both attempts are V2 calls, so the DDie one cannot
// reach a V1 driver, and a caller falling back to V1() afterwards is unaffected: on a V2 driver
// that entry point is refused anyway, so this ordering costs a V1 host nothing and saves a V2 host
// one refused call.
func (l VDieHandler) V2() (DieId, Return) {
	var dieId DieId

	if apiVersion() == APIVersionV2 {
		devId, ret := Device(l).devID()
		if !ret.IsSuccess() {
			return dieId, ret
		}

		ret = Return(dcmiv2GetDeviceDieId(devId, VDIE, &dieId))
		if !ret.IsSuccess() {
			ret = Return(dcmiv2GetDeviceDieId(devId, DDIE, &dieId))
		}

		return dieId, ret
	}

	ret := Return(dcmiGetDeviceDieV2(l.cardId, l.deviceId, VDIE, &dieId))

	return dieId, ret
}

// GetUtilizationRateV retrieves the utilization rates of various components of the device,
// returning a UtilizationRateHandler for further queries.
func (l Device) GetUtilizationRateV() UtilizationRateHandler {
	return UtilizationRateHandler(l)
}

type UtilizationRateHandler Device

// V1 assembles the multi-component rate from four per-type reads.
//
// Both generations offer the per-type entry point, so this adapts: on V2 the four reads go to
// dcmiv2_get_device_utilization_rate. A caller that tries V2() first therefore gets multi-rate
// then per-type on either generation, which is the same shape it already expected.
func (l UtilizationRateHandler) V1() (MultiUtilizationInfo, Return) {
	var (
		info  MultiUtilizationInfo
		ret   Return
		devId int32
		onV2  = apiVersion() == APIVersionV2
	)

	if onV2 {
		if devId, ret = Device(l).devID(); !ret.IsSuccess() {
			return info, ret
		}
	}

	rate := func(inputType int32, out *uint32) Return {
		if onV2 {
			return Return(dcmiv2GetDeviceUtilizationRate(devId, inputType, out))
		}
		return Return(dcmiGetDeviceUtilizationRate(l.cardId, l.deviceId, inputType, out))
	}

	// Read into a local per component, for the same reason GetVDeviceInfo does: no address inside
	// the returned struct reaches C, so the safety does not depend on what that struct happens to
	// hold today. MultiUtilizationInfo is a pure C layout and carries no pointer slot to find, so
	// this costs nothing now -- it keeps the file to one rule rather than two.
	//
	// Each local is assigned across as soon as its own read succeeds, not in one batch at the end.
	// A caller that stops on the return code still receives the components read before the failure,
	// which is what this returned before the locals were introduced.
	var aic, aiv, aicore, npu uint32
	if ret = rate(UTILIZATION_RATE_AICPU, &aic); !ret.IsSuccess() {
		return info, ret
	}
	info.Aic_util = aic
	if ret = rate(UTILIZATION_RATE_VECTORCORE, &aiv); !ret.IsSuccess() {
		return info, ret
	}
	info.Aiv_util = aiv
	if ret = rate(UTILIZATION_RATE_AICORE, &aicore); !ret.IsSuccess() {
		return info, ret
	}
	info.Aicore_util = aicore
	ret = rate(UTILIZATION_RATE_NPU, &npu)
	info.Npu_util = npu

	return info, ret
}

func (l UtilizationRateHandler) V2() (MultiUtilizationInfo, Return) {
	var info MultiUtilizationInfo

	if apiVersion() == APIVersionV2 {
		devId, ret := Device(l).devID()
		if !ret.IsSuccess() {
			return info, ret
		}
		ret = Return(dcmiv2GetDeviceMultiUtilizationRate(devId, &info))

		return info, ret
	}

	ret := Return(dcmiGetDeviceUtilizationRateV2(l.cardId, l.deviceId, &info))

	return info, ret
}

// GetTemperature retrieves the current temperature of the device.
func (l Device) GetTemperature() (int32, Return) {
	var temp int32

	if apiVersion() == APIVersionV2 {
		devId, ret := l.devID()
		if !ret.IsSuccess() {
			return 0, ret
		}
		ret = Return(dcmiv2GetDeviceTemperature(devId, &temp))

		return temp, ret
	}

	ret := Return(dcmiGetDeviceTemperature(l.cardId, l.deviceId, &temp))

	return temp, ret
}

// GetPowerInfo retrieves the current power information of the device.
func (l Device) GetPowerInfo() (int32, Return) {
	var power int32

	if apiVersion() == APIVersionV2 {
		devId, ret := l.devID()
		if !ret.IsSuccess() {
			return 0, ret
		}
		ret = Return(dcmiv2GetDevicePowerInfo(devId, &power))

		return power, ret
	}

	ret := Return(dcmiGetDevicePowerInfo(l.cardId, l.deviceId, &power))

	return power, ret
}

// GetPhysicalID retrieves the physical ID of the device,
// which is used for identifying the device in a physical topology.
//
// V1 needs two calls: the handle resolves to a logic id, and the logic id to a physical one. V2
// declares no logic-id query because its device index already is that number, so the translation
// GetCardList performs hands this the logic id directly and one call remains.
func (l Device) GetPhysicalID() (uint32, Return) {
	var (
		logId int32
		phyId uint32
	)

	if apiVersion() == APIVersionV2 {
		devId, ret := l.devID()
		if !ret.IsSuccess() {
			return 0, ret
		}
		ret = Return(dcmiv2GetChipPhyIdByDevId(uint32(devId), &phyId))

		return phyId, ret
	}

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

// V1 reads the narrower PCIe struct.
//
// It does not adapt to V2: that generation's only PCIe entry point is the one V2() below reaches,
// and callers try V2() first, so adapting here would repeat a call already made.
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

	if apiVersion() == APIVersionV2 {
		devId, ret := Device(l).devID()
		if !ret.IsSuccess() {
			return info, ret
		}
		ret = Return(dcmiv2GetDevicePcieInfo(devId, &info))

		return info, ret
	}

	ret := Return(dcmiGetDevicePcieInfoV2(l.cardId, l.deviceId, &info))

	return info, ret
}

// GetHbmInfo retrieves the current HBM (High Bandwidth Memory) information of the device.
func (l Device) GetHbmInfo() (HbmInfo, Return) {
	var info HbmInfo

	if apiVersion() == APIVersionV2 {
		devId, ret := l.devID()
		if !ret.IsSuccess() {
			return info, ret
		}
		ret = Return(dcmiv2GetDeviceHbmInfo(devId, &info))

		return info, ret
	}

	ret := Return(dcmiGetDeviceHbmInfo(l.cardId, l.deviceId, &info))

	return info, ret
}

// GetMemoryInfoV retrieves the current memory information of the device,
// returning a MemoryHandler for further queries.
func (l Device) GetMemoryInfoV() MemoryHandler {
	return MemoryHandler(l)
}

type MemoryHandler Device

// V2 reads the memory_info_v2 struct. V2 of the API declares no counterpart for it -- memory there
// comes from the HBM query alone -- so this passes through and a V2 driver refuses it.
func (l MemoryHandler) V2() (GetMemoryInfo, Return) {
	var info MemoryInfo
	ret := Return(dcmiGetDeviceMemoryInfoV2(l.cardId, l.deviceId, &info))
	return GetMemoryInfo{
		Memory_size: info.Size,
		Freq:        info.Freq,
		Utiliza:     info.Utiliza,
	}, ret
}

// V3 reads the memory_info_v3 struct. As with V2 above, the V2 API declares no counterpart, so this
// passes through and a V2 driver refuses it.
func (l MemoryHandler) V3() (GetMemoryInfo, Return) {
	var info GetMemoryInfo
	ret := Return(dcmiGetDeviceMemoryInfoV3(l.cardId, l.deviceId, &info))
	return info, ret
}

// GetEccInfo retrieves the current ECC (Error-Correcting Code) information of the device for the specified device type.
func (l Device) GetEccInfo(deviceType DeviceType) (EccInfo, Return) {
	var info EccInfo

	if apiVersion() == APIVersionV2 {
		devId, ret := l.devID()
		if !ret.IsSuccess() {
			return info, ret
		}
		ret = Return(dcmiv2GetDeviceEccInfo(devId, deviceType, &info))

		return info, ret
	}

	ret := Return(dcmiGetDeviceEccInfo(l.cardId, l.deviceId, deviceType, &info))

	return info, ret
}

// GetAffinityCPUInfo retrieves the affinity CPU information of the device as a string.
//
// The length is an output-only parameter on both generations -- there is no way to tell the library
// how large the buffer is -- so the length it reports is checked against the buffer before it is
// used to slice. A length past the end means the library wrote more than a read accepts, which is a
// truncated answer rather than a short one.
func (l Device) GetAffinityCPUInfo() (string, Return) {
	var (
		infoLength int32
		ret        Return
		info       = make([]byte, TOPO_INFO_MAX_LENGTH)
	)

	if apiVersion() == APIVersionV2 {
		devId, devRet := l.devID()
		if !devRet.IsSuccess() {
			return "", devRet
		}
		ret = Return(dcmiv2GetAffinityCpuInfoByDevId(devId, &info[0], &infoLength))
	} else {
		ret = Return(dcmiGetAffinityCpuInfoByDeviceId(l.cardId, l.deviceId, &info[0], &infoLength))
	}
	if !ret.IsSuccess() {
		return "", ret
	}
	if infoLength < 0 || infoLength > int32(len(info)) {
		return "", ERROR_LIST_TRUNCATED
	}

	return string(info[:infoLength]), SUCCESS
}

// GetTopoInfo retrieves the topology information between two devices, returning it as an integer.
//
// V2 declares no topology query, so this passes through and a V2 driver refuses it. There is no
// pairwise distance to report on that generation.
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
//
// V2 declares no device-IP query, so this passes through and a V2 driver refuses it.
func (l Device) GetIp(portType PortType, portId int32) (IpAddr, IpAddr, Return) {
	var ipAddr IpAddr
	var subnetMask IpAddr
	ret := Return(dcmiGetDeviceIp(l.cardId, l.deviceId, portType, portId, &ipAddr, &subnetMask))
	return ipAddr, subnetMask, ret
}

// GetGateway retrieves the gateway IP address of the device for the specified port type.
//
// V2 declares no device-gateway query, so this passes through and a V2 driver refuses it.
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
//
// Every read goes through deviceInfo, so the generation is handled once rather than at each of the
// four queries below. The reported virtual-device count is checked against the array it indexes: a
// driver reporting more than the array holds would otherwise read past its end.
//
// Each read lands in a local and is assigned afterwards, never straight into a field of the returned
// struct. That struct holds Items, a Go slice, and the runtime's pointer check scans every pointer
// slot of the object an argument points into -- so an address taken inside it hands C an object that
// carries a Go pointer as soon as Items is non-nil, and the call is killed before the driver sees it.
// Reading through locals is what makes that independent of statement order here: today the two reads
// below happen while Items is still nil, and nothing about the function says they have to.
func (l Device) GetVDeviceInfo() (VDeviceInfo, Return) {
	var info VDeviceInfo

	var totalResource SocTotalResource
	totalResourcePtr := unsafe.Pointer(&totalResource)
	totalResourceSize := uint32(unsafe.Sizeof(totalResource))
	ret := l.deviceInfo(MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_TOTAL_RESOURCE, totalResourcePtr, &totalResourceSize)
	if !ret.IsSuccess() {
		return VDeviceInfo{}, ret
	}
	if totalResource.Vdev_num > uint32(len(totalResource.Vdev_id)) {
		return VDeviceInfo{}, ERROR_LIST_TRUNCATED
	}
	info.TotalResource = totalResource

	var freeResource SocFreeResource
	freeResourcePtr := unsafe.Pointer(&freeResource)
	freeResourceSize := uint32(unsafe.Sizeof(freeResource))
	ret = l.deviceInfo(MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_FREE_RESOURCE, freeResourcePtr, &freeResourceSize)
	if !ret.IsSuccess() {
		return VDeviceInfo{}, ret
	}
	info.FreeResource = freeResource

	for i := uint32(0); i < info.TotalResource.Vdev_num; i++ {
		var vDevQuery VdevQuery
		vDevQuery.Vdev_id = info.TotalResource.Vdev_id[i]
		vDevQueryPtr := unsafe.Pointer(&vDevQuery)
		vDevQuerySize := uint32(unsafe.Sizeof(vDevQuery))
		ret = l.deviceInfo(MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_VDEV_RESOURCE, vDevQueryPtr, &vDevQuerySize)
		if !ret.IsSuccess() {
			return VDeviceInfo{}, ret
		}

		var vDevActivity VdevQuery
		vDevActivity.Vdev_id = vDevQuery.Vdev_id
		vDevActivityPtr := unsafe.Pointer(&vDevActivity)
		vDevActivitySize := uint32(unsafe.Sizeof(vDevActivity))
		ret = l.deviceInfo(MAIN_CMD_VDEV_MNG, VMNG_SUB_CMD_GET_VDEV_ACTIVITY, vDevActivityPtr, &vDevActivitySize)
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
//
// V2 declares no container-share entry point at all, so this passes through and a V2 driver refuses
// it. That generation has no such flag to read, which is a different thing from having one that is
// off, and the allocator distinguishes the two.
func (l Device) GetShareEnabled() (bool, Return) {
	var enableFlag int32
	ret := Return(dcmiGetDeviceShareEnable(l.cardId, l.deviceId, &enableFlag))
	return enableFlag != 0, ret
}

// SetShareEnabled enables or disables container-share mode for the device.
// The flag lives in the driver rather than in this process, so it outlives the caller,
// and it is reset by a reboot unless the driver's share-config recover mode is also on.
//
// As with GetShareEnabled above, V2 declares no counterpart: there is no flag to write there.
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
//
// The V2 entry point takes its count the same way — output only — so the guard tail and the count
// check apply to it unchanged.
func (l Device) GetProcessMemoryUsage() ([]ProcMemInfo, Return) {
	rows := make([]ProcMemInfo, maxProcMemInfoRows+procMemInfoGuardRows)
	for i := maxProcMemInfoRows; i < len(rows); i++ {
		rows[i].Id = procMemInfoGuard
	}

	var (
		procNum int32
		ret     Return
	)

	if apiVersion() == APIVersionV2 {
		devId, devRet := l.devID()
		if !devRet.IsSuccess() {
			return nil, devRet
		}
		ret = Return(dcmiv2GetDeviceProcMemInfo(devId, &rows[0], &procNum))
	} else {
		ret = Return(dcmiGetDeviceResourceInfo(l.cardId, l.deviceId, &rows[0], &procNum))
	}
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
