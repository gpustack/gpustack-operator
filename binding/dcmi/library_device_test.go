// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dcmi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The V2 address translation, which is the one piece of the adaptation that is pure logic and can
// therefore be pinned without a driver answering.
//
// GetCardList presents V2 device id N as card N holding exactly one device, so (N, 0) is the only
// address that translation produces. The rest of this table is what must NOT be accepted: a handle
// carrying any other second coordinate came from a stale or corrupt index, and serving it device N
// anyway would attribute a whole card's readings to a device that was never asked about -- wrong in
// the way that reads as plausible.
func TestDevice_devID(t *testing.T) {
	testCases := []struct {
		name   string
		device Device
		want   int32
		ret    Return
	}{
		{"the first device of the first card", Device{cardId: 0, deviceId: 0}, 0, SUCCESS},
		{"the only device of card 7", Device{cardId: 7, deviceId: 0}, 7, SUCCESS},
		{
			"the only device of the last card a read accepts",
			Device{cardId: MAX_CARD_NUM - 1, deviceId: 0},
			MAX_CARD_NUM - 1, SUCCESS,
		},

		{"a second device on a card that has one", Device{cardId: 7, deviceId: 1}, 0, ERROR_INVALID_DEVICE_ID},
		{"an index far past the one device", Device{cardId: 7, deviceId: 99}, 0, ERROR_INVALID_DEVICE_ID},
		{"a negative device index", Device{cardId: 7, deviceId: -1}, 0, ERROR_INVALID_DEVICE_ID},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			devId, ret := tc.device.devID()

			assert.Equal(t, tc.ret, ret, "the return of devID()")
			assert.Equal(t, tc.want, devId, "the device id devID() resolved")
		})
	}
}

// The three die types have to stay three distinct values.
//
// VDIE and NDIE come from the generated constants, DDIE is declared by hand because the public V2
// header does not enumerate it, and the V2 die read asks for VDIE and then DDIE. If a regenerated
// header ever moved VDIE onto 2, that read would ask the same question twice and a device whose
// virtual die is unreadable would be dropped with no sign of why.
func TestDieTypes_AreDistinct(t *testing.T) {
	assert.EqualValues(t, 2, DDIE, "DDIE, per the vendor's own constant")
	assert.NotEqualValues(t, DDIE, VDIE, "DDIE and VDIE")
	assert.NotEqualValues(t, DDIE, NDIE, "DDIE and NDIE")
	assert.NotEqualValues(t, VDIE, NDIE, "VDIE and NDIE")
}

// The chip-info command pair the super-pod read is issued with, pinned at the vendor's own values.
//
// Neither is in the vendored header -- its dcmi_main_cmd enum has no entry for 12 -- so both are
// declared by hand and nothing but this pins them. They select which question the generic
// device-info entry point answers, and a driver asked the wrong pair fills the caller's buffer from
// a different struct: the answer comes back successful and means something else.
//
// MAIN_CMD_CHIP_INF must also stay clear of the command groups the header does enumerate, or the
// same call would reach a group the driver already serves.
func TestChipInfoCommands_MatchTheVendor(t *testing.T) {
	assert.EqualValues(t, 12, MAIN_CMD_CHIP_INF, "MainCmdChipInf, per the vendor's own constant")
	assert.EqualValues(t, 1, CINF_SUB_CMD_GET_SPOD_INFO, "CinfSubCmdGetSPodInfo, per the vendor's own constant")

	for _, declared := range []int{
		MAIN_CMD_DVPP, MAIN_CMD_ISP, MAIN_CMD_TS_GROUP_NUM, MAIN_CMD_CAN, MAIN_CMD_UART,
		MAIN_CMD_UPGRADE, MAIN_CMD_HCCS, MAIN_CMD_TEMP, MAIN_CMD_SVM, MAIN_CMD_VDEV_MNG,
		MAIN_CMD_SIO, MAIN_CMD_DEVICE_SHARE,
	} {
		assert.NotEqual(t, declared, MAIN_CMD_CHIP_INF, "a command group the header already enumerates")
	}
}

// Every exported signature of this package, pinned at compile time.
//
// `go build ./...` already catches a changed signature that some package in this repo calls. This
// block covers the rest: a binding is meant to be a stable surface, and several of these methods
// have no in-tree caller to break, so a quiet change to one of them would go unnoticed here and
// surface only in whatever consumes the binding next.
//
// Each line is a method expression assigned to an explicitly typed blank, so nothing is allocated
// and a mismatch is a compile error at the contract rather than at a caller.
var (
	_ func(*DCMI) (int32, []int32, Return)                        = (*DCMI).GetCardList
	_ func(*DCMI, int32) (int32, Return)                          = (*DCMI).GetDeviceNumInCard
	_ func(*DCMI, int32, int32) Device                            = (*DCMI).GetDeviceHandleByCardAndIndex
	_ func(*DCMI) (string, Return)                                = (*DCMI).GetDriverVersion
	_ func(*DCMI) (MultiDiePolicy, Return)                        = (*DCMI).GetMultiDiePolicy
	_ func(*DCMI, MultiDiePolicy) Return                          = (*DCMI).SetMultiDiePolicy
	_ func(Device) (UnitType, Return)                             = Device.GetType
	_ func(Device) ChipInfoHandler                                = Device.GetChipInfoV
	_ func(Device) VDieHandler                                    = Device.GetVDieV
	_ func(Device) UtilizationRateHandler                         = Device.GetUtilizationRateV
	_ func(Device) (int32, Return)                                = Device.GetTemperature
	_ func(Device) (int32, Return)                                = Device.GetPowerInfo
	_ func(Device) (uint32, Return)                               = Device.GetPhysicalID
	_ func(Device) PcieInfoHandler                                = Device.GetPcieInfoV
	_ func(Device) (HbmInfo, Return)                              = Device.GetHbmInfo
	_ func(Device) MemoryHandler                                  = Device.GetMemoryInfoV
	_ func(Device, DeviceType) (EccInfo, Return)                  = Device.GetEccInfo
	_ func(Device) (string, Return)                               = Device.GetAffinityCPUInfo
	_ func(Device, Device) (int32, Return)                        = Device.GetTopoInfo
	_ func(Device) (SpodInfo, Return)                             = Device.GetSuperPodInfo
	_ func(Device) (uint32, Return)                               = Device.GetMainboardId
	_ func(Device) (uint32, Return)                               = Device.GetUrmaDeviceCount
	_ func(Device, uint32) ([]UrmaEidInfo, Return)                = Device.GetEidList
	_ func(Device, PortType, int32) (IpAddr, IpAddr, Return)      = Device.GetIp
	_ func(Device, PortType, int32) (IpAddr, Return)              = Device.GetGateway
	_ func(Device) (VDeviceInfo, Return)                          = Device.GetVDeviceInfo
	_ func(Device) (bool, Return)                                 = Device.GetShareEnabled
	_ func(Device, bool) Return                                   = Device.SetShareEnabled
	_ func(Device) ([]ProcMemInfo, Return)                        = Device.GetProcessMemoryUsage
	_ func(ChipInfoHandler) (ChipInfoV2, Return)                  = ChipInfoHandler.V1
	_ func(ChipInfoHandler) (ChipInfoV2, Return)                  = ChipInfoHandler.V2
	_ func(VDieHandler) (DieId, Return)                           = VDieHandler.V1
	_ func(VDieHandler) (DieId, Return)                           = VDieHandler.V2
	_ func(UtilizationRateHandler) (MultiUtilizationInfo, Return) = UtilizationRateHandler.V1
	_ func(UtilizationRateHandler) (MultiUtilizationInfo, Return) = UtilizationRateHandler.V2
	_ func(PcieInfoHandler) (PcieInfoAll, Return)                 = PcieInfoHandler.V1
	_ func(PcieInfoHandler) (PcieInfoAll, Return)                 = PcieInfoHandler.V2
	_ func(MemoryHandler) (GetMemoryInfo, Return)                 = MemoryHandler.V2
	_ func(MemoryHandler) (GetMemoryInfo, Return)                 = MemoryHandler.V3
)
