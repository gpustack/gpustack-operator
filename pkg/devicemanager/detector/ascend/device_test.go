package ascend

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/device"
)

// A device whose die cannot be read yields no uuid and carries the failure out, which is what makes
// all three call sites drop it.
//
// That is the observable form of "no PCI address as a substitute": Accelerator.ID is universally
// unique by contract while a BDF repeats on every node of a fleet, so a device that cannot identify
// itself has to be dropped rather than identified some other way. Returning an empty uuid with a
// success code instead would have every unidentifiable device on a node collapse onto one id.
//
// On a host with no Ascend library to load, every die entry point is unresolved, which is exactly
// this case. A host whose driver does answer for device 0 cannot observe it, hence the skip rather
// than a failure — the same host-dependence the process tests in this package carry.
func TestDeviceUUIDUnreadable(t *testing.T) {
	uuid, ret := deviceUUID(dcmi.Device{})
	if ret.IsSuccess() {
		t.Skip("an Ascend driver answered for device 0 on this host, so an unreadable die cannot be observed")
	}

	assert.Empty(t, uuid, "the uuid of a device whose die cannot be read")
	assert.False(t, ret.IsSuccess(), "the failure must be carried out, not swallowed")
}

// The detector names a group's family by chaining these two, so the chain is what the cases below
// drive. Both ends are asserted: a soc name and a family can agree on the family while disagreeing
// on how it was reached, and reaching it by fallback rather than by an exact name is the failure
// this guards. "Ascend910_9363" is the case in point -- absent from the version map it still ends
// up with a family, just the wrong one, because "^910" catches it.
func TestGuessSocNameAndFamily(t *testing.T) {
	// The 950 generation is the one Ascend recognizes by prefix rather than by name: its suffixes
	// are an open set (PR, DT, ...), and every upstream that reads a chip name -- ascend-common's
	// Is910A5Chip, ascend-docker-runtime's own copy, torch_npu's SetSocVersion -- tests
	// HasPrefix("Ascend950") instead of listing them. These cases pin that same rule here.
	testCases := []struct {
		name        string
		devName     string
		wantSocName string
		wantFamily  string
	}{
		{"a 950 names itself in full", "Ascend950", "Ascend950", "950"},
		{"a 950PR resolves to the 950 family", "Ascend950PR", "Ascend950", "950"},
		{"a 950DT resolves to the 950 family", "Ascend950DT", "Ascend950", "950"},
		{"a 950 suffix nobody has shipped yet still resolves", "Ascend950XX", "Ascend950", "950"},
		{"the Ascend prefix is optional", "950PR", "Ascend950", "950"},
		// 910C parts carry an underscore, so they are named exactly and must never reach "^910",
		// which would resolve them to Ascend910A and hand back the "910" family instead.
		{"a 910C part is named exactly", "Ascend910_9362", "Ascend910_9362", "910C"},
		{"the newest 910C part is named exactly", "Ascend910_9363", "Ascend910_9363", "910C"},
		{"a 910B part is named exactly", "910B2", "Ascend910B2", "910B"},
		{"an unlisted 910B suffix falls back within its family", "910B9", "Ascend910B1", "910B"},
		{"a 310P part is named exactly", "310P3", "Ascend310P3", "310P"},
		{"an unlisted 310P suffix falls back within its family", "310P9", "Ascend310P1", "310P"},
		{"a 310B part falls back within its family", "310B9", "Ascend310B1", "310B"},
		{"a bare 910 is the oldest family", "910A", "Ascend910A", "910"},
		{"an empty name yields no family", "", "", ""},
		{"an unknown name yields no family", "Ascend404", "", ""},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			socName := guessSocNameFromDeviceName(tc.devName)
			assert.Equalf(t, tc.wantSocName, socName, "guessSocNameFromDeviceName(%q)", tc.devName)
			assert.Equalf(t, tc.wantFamily, getFamilyFromSocName(socName),
				"getFamilyFromSocName(guessSocNameFromDeviceName(%q))", tc.devName)
		})
	}
}

func TestGetLogicalSliced(t *testing.T) {
	sliced := device.AcceleratorLogicalSliced{
		Count:                     63,
		CoresPercentageOvercommit: true,
	}

	// Slicing is offered exactly where the image builds a vcann-rt, so every case below mirrors
	// one xbuild-ascend-cann-<major>-<family> stage or the absence of one. The "10.0" rows matter
	// twice over: an unbuilt major must be refused, and the major has to be compared as a number
	// because it arrives as a string, in which "10" sorts below "9" and would slip through a
	// lexicographic ">= 9".
	testCases := []struct {
		name           string
		family         string
		runtimeVersion string
		want           device.AcceleratorLogicalSliced
	}{
		{"310P on CANN 9 slices", "310P", "9.1", sliced},
		{"310P on CANN 8 does not", "310P", "8.5", device.AcceleratorLogicalSliced{}},
		{"310P without a runtime falls back to 8", "310P", "", device.AcceleratorLogicalSliced{}},
		{"910B slices on CANN 8", "910B", "8.5", sliced},
		{"910B slices on CANN 9", "910B", "9.1", sliced},
		{"910C slices on CANN 8", "910C", "8.5", sliced},
		{"910C slices on CANN 9", "910C", "9.1", sliced},
		{"950 slices on CANN 9", "950", "9.1", sliced},
		// No cann-8-950 stage exists, so a 950 on a CANN 8 host must not be offered slicing it
		// cannot start.
		{"950 does not slice on CANN 8", "950", "8.5", device.AcceleratorLogicalSliced{}},
		// An unbuilt major is refused for every family, which is also the numeric-compare guard.
		{"310P does not slice on CANN 10", "310P", "10.0", device.AcceleratorLogicalSliced{}},
		{"910B does not slice on CANN 10", "910B", "10.0", device.AcceleratorLogicalSliced{}},
		{"910 does not slice on CANN 8", "910", "8.5", device.AcceleratorLogicalSliced{}},
		{"910 does not slice on CANN 9", "910", "9.1", device.AcceleratorLogicalSliced{}},
		{"310B does not slice on CANN 9", "310B", "9.1", device.AcceleratorLogicalSliced{}},
		{"an unknown family does not slice", "", "9.1", device.AcceleratorLogicalSliced{}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, getLogicalSliced(tc.family, tc.runtimeVersion),
				"getLogicalSliced(%q, %q)", tc.family, tc.runtimeVersion)
		})
	}
}

func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name      string
		health    uint32
		healthRet dcmi.Return
		eccInfo   dcmi.EccInfo
		eccRet    dcmi.Return
		want      bool
	}{
		{
			name:      "normal health status is healthy",
			health:    0,
			healthRet: dcmi.SUCCESS,
			want:      false,
		},
		{
			name:      "minor alarm health status remains healthy even with single-bit ECC",
			health:    1,
			healthRet: dcmi.SUCCESS,
			eccInfo:   dcmi.EccInfo{Enable_flag: 1, Single_bit_error_cnt: 12},
			eccRet:    dcmi.SUCCESS,
			want:      false,
		},
		{
			name:      "major alarm health status is unhealthy",
			health:    2,
			healthRet: dcmi.SUCCESS,
			want:      true,
		},
		{
			name:      "critical alarm health status is unhealthy",
			health:    3,
			healthRet: dcmi.SUCCESS,
			want:      true,
		},
		{
			name:      "not-found sentinel health status is unhealthy",
			health:    0xFFFFFFFF,
			healthRet: dcmi.SUCCESS,
			want:      true,
		},
		{
			name:      "health query unavailable falls back to ECC and ignores single-bit error",
			healthRet: dcmi.ERROR_FUNCTION_NOT_FOUND,
			eccInfo:   dcmi.EccInfo{Enable_flag: 1, Single_bit_error_cnt: 5, Double_bit_error_cnt: 0},
			eccRet:    dcmi.SUCCESS,
			want:      false,
		},
		{
			name:      "health query unavailable falls back to ECC and marks double-bit error unhealthy",
			healthRet: dcmi.ERROR_FUNCTION_NOT_FOUND,
			eccInfo:   dcmi.EccInfo{Enable_flag: 1, Single_bit_error_cnt: 5, Double_bit_error_cnt: 1},
			eccRet:    dcmi.SUCCESS,
			want:      true,
		},
		{
			name:      "health query unavailable with ECC disabled is healthy",
			healthRet: dcmi.ERROR_FUNCTION_NOT_FOUND,
			eccInfo:   dcmi.EccInfo{Enable_flag: 0, Double_bit_error_cnt: 1},
			eccRet:    dcmi.SUCCESS,
			want:      false,
		},
		{
			name:      "both health and ECC queries unavailable is healthy",
			healthRet: dcmi.ERROR_FUNCTION_NOT_FOUND,
			eccRet:    dcmi.ERROR_NOT_SUPPORT,
			want:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateDeviceUnhealthy(tc.health, tc.healthRet, func() (dcmi.EccInfo, dcmi.Return) {
				return tc.eccInfo, tc.eccRet
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSelectMemoryType pins which memory query a device's readings come from. A successful HBM
// answer is trusted only when it reports capacity: trusting a zero-size answer reports zero
// capacity and checks the wrong ECC counters, which is the misalignment with the vendor runtime
// this guards against. The V2 API declares no DDR query at all -- the V1 call passes through and
// a V2 driver refuses it -- so its HBM answer is used as reported, whatever size it carries, and
// a device is never sent to a query its driver cannot serve.
func TestSelectMemoryType(t *testing.T) {
	testCases := []struct {
		name       string
		hbmInfo    dcmi.HbmInfo
		hbmRet     dcmi.Return
		apiVersion dcmi.APIVersion
		want       dcmi.DeviceType
	}{
		{
			name:       "an hbm answer with capacity is hbm",
			hbmInfo:    dcmi.HbmInfo{Memory_size: 32768},
			hbmRet:     dcmi.SUCCESS,
			apiVersion: dcmi.APIVersionV1,
			want:       dcmi.DEVICE_TYPE_HBM,
		},
		{
			name:       "a successful zero hbm answer is ddr",
			hbmInfo:    dcmi.HbmInfo{},
			hbmRet:     dcmi.SUCCESS,
			apiVersion: dcmi.APIVersionV1,
			want:       dcmi.DEVICE_TYPE_DDR,
		},
		{
			name:       "a failed hbm query is ddr",
			hbmRet:     dcmi.ERROR_NOT_SUPPORT,
			apiVersion: dcmi.APIVersionV1,
			want:       dcmi.DEVICE_TYPE_DDR,
		},
		{
			name:       "the v2 api keeps a zero hbm answer",
			hbmInfo:    dcmi.HbmInfo{},
			hbmRet:     dcmi.SUCCESS,
			apiVersion: dcmi.APIVersionV2,
			want:       dcmi.DEVICE_TYPE_HBM,
		},
		{
			name:       "the v2 api keeps hbm capacity",
			hbmInfo:    dcmi.HbmInfo{Memory_size: 131072},
			hbmRet:     dcmi.SUCCESS,
			apiVersion: dcmi.APIVersionV2,
			want:       dcmi.DEVICE_TYPE_HBM,
		},
		{
			name:       "a failed hbm query is ddr on the v2 api too",
			hbmRet:     dcmi.ERROR_NOT_SUPPORT,
			apiVersion: dcmi.APIVersionV2,
			want:       dcmi.DEVICE_TYPE_DDR,
		},
		{
			name:       "an unknown api generation is not read as v2",
			hbmRet:     dcmi.SUCCESS,
			apiVersion: dcmi.APIVersionUnknown,
			want:       dcmi.DEVICE_TYPE_DDR,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, selectMemoryType(tc.hbmInfo, tc.hbmRet, tc.apiVersion))
		})
	}
}

// TestKeepOnMissingDDR pins which unreadable DDR queries keep a device in the output. A query
// the library cannot even make -- the entry point is missing, or the library and driver are an
// incompatible pair -- is not the driver's answer about the device, so a device the HBM query
// did answer keeps that answer instead of vanishing from detection and monitoring. A refusal
// the driver itself returned is a different case: it has always dropped the device, and still
// does.
func TestKeepOnMissingDDR(t *testing.T) {
	testCases := []struct {
		name   string
		hbmRet dcmi.Return
		ddrRet dcmi.Return
		want   bool
	}{
		{
			name:   "an hbm answer stands when the ddr entry point is missing",
			hbmRet: dcmi.SUCCESS,
			ddrRet: dcmi.ERROR_FUNCTION_NOT_FOUND,
			want:   true,
		},
		{
			name:   "an hbm answer stands when the library cannot serve the ddr query",
			hbmRet: dcmi.SUCCESS,
			ddrRet: dcmi.ERROR_LIBRARY_NOT_FOUND,
			want:   true,
		},
		{
			name:   "the driver refusing the ddr query still drops the device",
			hbmRet: dcmi.SUCCESS,
			ddrRet: dcmi.ERROR_NOT_SUPPORT,
			want:   false,
		},
		{
			name:   "a device the hbm query never answered still drops",
			hbmRet: dcmi.ERROR_NOT_SUPPORT,
			ddrRet: dcmi.ERROR_FUNCTION_NOT_FOUND,
			want:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, keepOnMissingDDR(tc.hbmRet, tc.ddrRet))
		})
	}
}

func TestSelectPCIBusID(t *testing.T) {
	testCases := []struct {
		name       string
		v2Info     dcmi.PcieInfoAll
		v2Ret      dcmi.Return
		v1Info     dcmi.PcieInfoAll
		v1Ret      dcmi.Return
		wantV1Call bool
		wantBusID  string
		wantRet    dcmi.Return
	}{
		{
			name: "v2 success returns formatted bus id without calling v1",
			v2Info: dcmi.PcieInfoAll{
				Domain:       0,
				Bdf_busid:    0xc1,
				Bdf_deviceid: 0,
				Bdf_funcid:   0,
			},
			v2Ret:      dcmi.SUCCESS,
			wantV1Call: false,
			wantBusID:  "0000:c1:00.0",
			wantRet:    dcmi.SUCCESS,
		},
		{
			name:  "v2 unsupported falls back to v1 on older drivers",
			v2Ret: dcmi.ERROR_NOT_SUPPORT,
			v1Info: dcmi.PcieInfoAll{
				Domain:       0,
				Bdf_busid:    0x21,
				Bdf_deviceid: 0,
				Bdf_funcid:   0,
			},
			v1Ret:      dcmi.SUCCESS,
			wantV1Call: true,
			wantBusID:  "0000:21:00.0",
			wantRet:    dcmi.SUCCESS,
		},
		{
			name:       "ub-attached v2 device keeps empty bus id when both generations return not supported",
			v2Ret:      dcmi.ERROR_NOT_SUPPORT,
			v1Ret:      dcmi.ERROR_NOT_SUPPORT,
			wantV1Call: true,
			wantBusID:  "",
			wantRet:    dcmi.ERROR_NOT_SUPPORT,
		},
		{
			name:       "v2 failure reason is preserved when v1 entry point is absent",
			v2Ret:      dcmi.ERROR_TIME_OUT,
			v1Ret:      dcmi.ERROR_FUNCTION_NOT_FOUND,
			wantV1Call: true,
			wantBusID:  "",
			wantRet:    dcmi.ERROR_TIME_OUT,
		},
		{
			name:       "v1 failure reason is returned when v1 driver served and failed",
			v2Ret:      dcmi.ERROR_FUNCTION_NOT_FOUND,
			v1Ret:      dcmi.ERROR_IOCTL_FAIL,
			wantV1Call: true,
			wantBusID:  "",
			wantRet:    dcmi.ERROR_IOCTL_FAIL,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			v1Called := false
			busID, ret := selectPCIBusID(tc.v2Info, tc.v2Ret, func() (dcmi.PcieInfoAll, dcmi.Return) {
				v1Called = true
				return tc.v1Info, tc.v1Ret
			})
			assert.Equal(t, tc.wantV1Call, v1Called)
			assert.Equal(t, tc.wantBusID, busID)
			assert.Equal(t, tc.wantRet, ret)
		})
	}
}

func TestIsPCIBusIDUnsupported(t *testing.T) {
	testCases := []struct {
		name string
		ret  dcmi.Return
		want bool
	}{
		{"not supported on ub-attached device", dcmi.ERROR_NOT_SUPPORT, true},
		{"not supported inside container", dcmi.ERROR_NOT_SUPPORT_IN_CONTAINER, true},
		{"symbol absent from library", dcmi.ERROR_FUNCTION_NOT_FOUND, true},
		{"library absent", dcmi.ERROR_LIBRARY_NOT_FOUND, true},
		{"config info missing", dcmi.ERROR_CONFIG_INFO_NOT_EXIST, false},
		{"driver timeout", dcmi.ERROR_TIME_OUT, false},
		{"success", dcmi.SUCCESS, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isPCIBusIDUnsupported(tc.ret))
		})
	}
}

func TestApplyFallbackAffinity(t *testing.T) {
	testCases := []struct {
		name       string
		initial    device.Topology
		cpuRaw     string
		cpuRet     dcmi.Return
		mappedNuma string
		wantRead   bool
		want       device.Topology
	}{
		{
			name: "existing sysfs numa affinity skips dcmi cpu query",
			initial: device.Topology{
				PciBusID:     "0000:c1:00.0",
				NumaAffinity: "1",
				CpuAffinity:  "32-63",
			},
			wantRead: false,
			want: device.Topology{
				PciBusID:     "0000:c1:00.0",
				NumaAffinity: "1",
				CpuAffinity:  "32-63",
			},
		},
		{
			name:       "empty bus id populates canonical cpu and numa affinity from dcmi",
			initial:    device.Topology{},
			cpuRaw:     "0-15, 32-47",
			cpuRet:     dcmi.SUCCESS,
			mappedNuma: "0",
			wantRead:   true,
			want: device.Topology{
				NumaAffinity: "0",
				CpuAffinity:  "0-15,32-47",
			},
		},
		{
			name:       "valid dcmi cpu list stays when host exposes no numa mapping",
			initial:    device.Topology{},
			cpuRaw:     "0-15",
			cpuRet:     dcmi.SUCCESS,
			mappedNuma: "",
			wantRead:   true,
			want: device.Topology{
				NumaAffinity: "",
				CpuAffinity:  "0-15",
			},
		},
		{
			name:     "unsupported dcmi cpu query leaves affinity empty",
			initial:  device.Topology{},
			cpuRet:   dcmi.ERROR_NOT_SUPPORT,
			wantRead: true,
			want:     device.Topology{},
		},
		{
			name:     "non-numeric dcmi cpu text leaves affinity empty",
			initial:  device.Topology{},
			cpuRaw:   "N/A",
			cpuRet:   dcmi.SUCCESS,
			wantRead: true,
			want:     device.Topology{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			topo := tc.initial
			readCalled := false
			applyFallbackAffinity(
				&topo,
				func() (string, dcmi.Return) {
					readCalled = true
					return tc.cpuRaw, tc.cpuRet
				},
				func(string) string {
					return tc.mappedNuma
				},
			)
			assert.Equal(t, tc.wantRead, readCalled)
			assert.Equal(t, tc.want, topo)
		})
	}
}

const dcmiFixtureSource = `
#include "dcmi_interface_api.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int init_calls = 0;
static int is_initialized = 0;

static const char *fixture_env(const char *name) {
    const char *value = getenv(name);
    return value == NULL ? "" : value;
}

static int device_phy_id(int card_id, int device_id) {
    int per_card = atoi(fixture_env("GPUSTACK_TEST_DCMI_DEVS_PER_CARD"));
    if (per_card <= 0)
        per_card = 1;
    return card_id * per_card + device_id;
}

static int maybe_fail_device(const char *stage, int phy_id) {
    if (strcmp(fixture_env("GPUSTACK_TEST_DCMI_FAIL_STAGE"), stage) != 0)
        return 0;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_DCMI_FAIL_PHY");
    if (fail_idx[0] == '\0')
        return 0;
    if (strcmp(fail_idx, "all") != 0 && atoi(fail_idx) != phy_id)
        return 0;
    return atoi(fixture_env("GPUSTACK_TEST_DCMI_FAIL_ERR"));
}

int dcmi_init(void) {
    const char *err_env = fixture_env("GPUSTACK_TEST_DCMI_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_DCMI_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    return 0;
}

int dcmi_get_card_list(int *card_num, int *card_list, int list_len) {
    if (strcmp(fixture_env("GPUSTACK_TEST_DCMI_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return -8005;
    const char *err_env = fixture_env("GPUSTACK_TEST_DCMI_COUNT_ERR");
    if (err_env[0] != '\0')
        return atoi(err_env);
    int cards = atoi(fixture_env("GPUSTACK_TEST_DCMI_CARDS"));
    if (cards <= 0)
        cards = 1;
    if (cards > list_len)
        cards = list_len;
    *card_num = cards;
    for (int i = 0; i < cards; i++) {
        card_list[i] = i;
    }
    return 0;
}

int dcmi_get_device_num_in_card(int card_id, int *device_num) {
    (void)card_id;
    int per_card = atoi(fixture_env("GPUSTACK_TEST_DCMI_DEVS_PER_CARD"));
    if (per_card <= 0)
        per_card = 1;
    *device_num = per_card;
    return 0;
}

int dcmi_get_device_type(int card_id, int device_id, enum dcmi_unit_type *device_type) {
    (void)card_id;
    (void)device_id;
    if (strcmp(fixture_env("GPUSTACK_TEST_DCMI_NON_NPU"), "true") == 0) {
        *device_type = MCU_TYPE;
        return 0;
    }
    *device_type = NPU_TYPE;
    return 0;
}

int dcmi_get_device_die_v2(int card_id, int device_id, enum dcmi_die_type input_type, struct dcmi_die_id *die_id) {
    (void)input_type;
    int phy = device_phy_id(card_id, device_id);
    int ret = maybe_fail_device("die", phy);
    if (ret != 0)
        return ret;
    memset(die_id, 0, sizeof(*die_id));
    die_id->soc_die[0] = (unsigned int)(phy + 1);
    return 0;
}

int dcmi_get_device_die(int card_id, int device_id, struct dcmi_soc_die_stru *device_die) {
    (void)card_id;
    (void)device_id;
    (void)device_die;
    return -8255;
}

int dcmi_get_device_chip_info_v2(int card_id, int device_id, struct dcmi_chip_info_v2 *device_info) {
    int phy = device_phy_id(card_id, device_id);
    int ret = maybe_fail_device("chip", phy);
    if (ret != 0)
        return ret;
    memset(device_info, 0, sizeof(*device_info));
    snprintf((char *)device_info->chip_name, sizeof(device_info->chip_name), "Ascend910B2");
    device_info->aicore_cnt = 24;
    return 0;
}

int dcmi_get_device_chip_info(int card_id, int device_id, struct dcmi_chip_info *chip_info) {
    (void)card_id;
    (void)device_id;
    (void)chip_info;
    return -8255;
}

int dcmi_get_device_hbm_info(int card_id, int device_id, struct dcmi_hbm_info *hbm_info) {
    int phy = device_phy_id(card_id, device_id);
    int ret = maybe_fail_device("memory", phy);
    if (ret != 0)
        return ret;
    memset(hbm_info, 0, sizeof(*hbm_info));
    hbm_info->memory_size = 65536;
    hbm_info->memory_usage = 1024;
    return 0;
}

int dcmi_get_device_logic_id(int *device_logic_id, int card_id, int device_id) {
    *device_logic_id = device_phy_id(card_id, device_id);
    return 0;
}

int dcmi_get_device_phyid_from_logicid(unsigned int logicid, unsigned int *phyid) {
    int ret = maybe_fail_device("phyid", (int)logicid);
    if (ret != 0)
        return ret;
    *phyid = logicid;
    return 0;
}
`

func compileDCMIFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	libDir := filepath.Join(dir, "driver", "lib64", "driver")
	require.NoError(t, os.MkdirAll(libDir, 0o755))
	// The vendored header defines DCMIDLLEXPORT as static; rewrite a local copy so the
	// fixture exports external symbols while keeping the exact vendored ABI.
	hdr, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "binding", "dcmi", "dcmi_interface_api.h"))
	require.NoError(t, err)
	patchedHdr := strings.Replace(string(hdr), "#define DCMIDLLEXPORT static", "#define DCMIDLLEXPORT", 1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dcmi_interface_api.h"), []byte(patchedHdr), 0o600))

	src := filepath.Join(dir, "fixture_dcmi.c")
	require.NoError(t, os.WriteFile(src, []byte(dcmiFixtureSource), 0o600))

	flags := make([]string, 0, 7)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-I"+dir, "-I../../../../binding/dcmi", src, "-o", filepath.Join(libDir, "libdcmi.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_DCMI_MODE"); mode != "" {
		d := New(device.DetectorOptions{Logger: klog.Background()})
		switch mode {
		case "multi_card_skip_one":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)
			assert.Equal(t, uint32(3), groups[0].Accelerators[2].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 3)
		case "intentional_skip_non_npu":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, metrics)
		case "fatal_per_card", "all_unqueryable", "count_error":
			groups, err := d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)
		case "transient_init_error":
			groups, err := d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 1)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
		default:
			t.Fatalf("unknown mode %q", mode)
		}
		return
	}

	dir := compileDCMIFixture(t)

	runCase := func(t *testing.T, extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnqueryableCardAndErrorHandling$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"CANN_HOME="+dir,
			"LD_LIBRARY_PATH="+filepath.Join(dir, "driver", "lib64", "driver"),
			"DYLD_LIBRARY_PATH="+filepath.Join(dir, "driver", "lib64", "driver"),
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	t.Run("multi-card skips phyId 1 and keeps unique non-compacting phyId indexes", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_DCMI_MODE=multi_card_skip_one",
			"GPUSTACK_TEST_DCMI_CARDS=2",
			"GPUSTACK_TEST_DCMI_DEVS_PER_CARD=2",
			"GPUSTACK_TEST_DCMI_FAIL_STAGE=die",
			"GPUSTACK_TEST_DCMI_FAIL_PHY=1",
			"GPUSTACK_TEST_DCMI_FAIL_ERR="+strconv.Itoa(int(dcmi.ERROR_DEVICE_NOT_EXIST)),
		)
	})

	t.Run("all non-NPU devices skipped intentionally returns nil nil", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_DCMI_MODE=intentional_skip_non_npu",
			"GPUSTACK_TEST_DCMI_CARDS=2",
			"GPUSTACK_TEST_DCMI_DEVS_PER_CARD=1",
			"GPUSTACK_TEST_DCMI_NON_NPU=true",
		)
	})

	t.Run("OPER_NOT_PERMITTED fails detect and monitor immediately", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_DCMI_MODE=fatal_per_card",
			"GPUSTACK_TEST_DCMI_CARDS=2",
			"GPUSTACK_TEST_DCMI_DEVS_PER_CARD=1",
			"GPUSTACK_TEST_DCMI_FAIL_STAGE=die",
			"GPUSTACK_TEST_DCMI_FAIL_PHY=1",
			"GPUSTACK_TEST_DCMI_FAIL_ERR="+strconv.Itoa(int(dcmi.ERROR_OPER_NOT_PERMITTED)),
		)
	})

	t.Run("all devices unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_DCMI_MODE=all_unqueryable",
			"GPUSTACK_TEST_DCMI_CARDS=2",
			"GPUSTACK_TEST_DCMI_DEVS_PER_CARD=1",
			"GPUSTACK_TEST_DCMI_FAIL_STAGE=die",
			"GPUSTACK_TEST_DCMI_FAIL_PHY=all",
			"GPUSTACK_TEST_DCMI_FAIL_ERR="+strconv.Itoa(int(dcmi.ERROR_DEVICE_NOT_EXIST)),
		)
	})

	t.Run("GetCardList OPER_NOT_PERMITTED returns error", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_DCMI_MODE=count_error",
			"GPUSTACK_TEST_DCMI_COUNT_ERR="+strconv.Itoa(int(dcmi.ERROR_OPER_NOT_PERMITTED)),
		)
	})

	t.Run("transient Init failure recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_DCMI_MODE=transient_init_error",
			"GPUSTACK_TEST_DCMI_CARDS=1",
			"GPUSTACK_TEST_DCMI_DEVS_PER_CARD=1",
			"GPUSTACK_TEST_DCMI_REQUIRE_INIT=true",
			"GPUSTACK_TEST_DCMI_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_DCMI_INIT_ERR="+strconv.Itoa(int(dcmi.ERROR_INNER_ERR)),
		)
	})
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         dcmi.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: dcmi.SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: dcmi.ERROR_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "function not found with no pci devices", ret: dcmi.ERROR_FUNCTION_NOT_FOUND, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "library not found with pci devices present", ret: dcmi.ERROR_LIBRARY_NOT_FOUND, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "oper not permitted with noPciCheck", ret: dcmi.ERROR_OPER_NOT_PERMITTED, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "inner err with noPciCheck", ret: dcmi.ERROR_INNER_ERR, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "timeout with pci devices", ret: dcmi.ERROR_TIME_OUT, noPciCheck: false, pciDevCount: 1, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize DCMI", tc.ret, tc.noPciCheck, tc.pciDevCount)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.ret)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestIsUnqueryableDeviceError(t *testing.T) {
	testCases := []struct {
		name string
		ret  dcmi.Return
		want bool
	}{
		{name: "DEVICE_NOT_EXIST", ret: dcmi.ERROR_DEVICE_NOT_EXIST, want: true},
		{name: "INVALID_DEVICE_ID", ret: dcmi.ERROR_INVALID_DEVICE_ID, want: true},
		{name: "IOCTL_FAIL", ret: dcmi.ERROR_IOCTL_FAIL, want: true},
		{name: "SEND_MSG_FAIL", ret: dcmi.ERROR_SEND_MSG_FAIL, want: true},
		{name: "RECV_MSG_FAIL", ret: dcmi.ERROR_RECV_MSG_FAIL, want: true},
		{name: "NOT_REDAY", ret: dcmi.ERROR_NOT_REDAY, want: true},
		{name: "RESET_FAIL", ret: dcmi.ERROR_RESET_FAIL, want: true},
		{name: "IS_UPGRADING", ret: dcmi.ERROR_IS_UPGRADING, want: true},
		{name: "INNER_ERR", ret: dcmi.ERROR_INNER_ERR, want: true},
		{name: "MEM_OPERATE_FAIL", ret: dcmi.ERROR_MEM_OPERATE_FAIL, want: true},
		{name: "SECURE_FUN_FAIL", ret: dcmi.ERROR_SECURE_FUN_FAIL, want: true},
		{name: "ABORT_OPERATE", ret: dcmi.ERROR_ABORT_OPERATE, want: true},
		{name: "RESOURCE_OCCUPIED", ret: dcmi.ERROR_RESOURCE_OCCUPIED, want: true},
		{name: "UNKNOWN", ret: dcmi.ERROR_UNKNOWN, want: true},
		{name: "SUCCESS", ret: dcmi.SUCCESS, want: false},
		{name: "OPER_NOT_PERMITTED", ret: dcmi.ERROR_OPER_NOT_PERMITTED, want: false},
		{name: "INVALID_PARAMETER", ret: dcmi.ERROR_INVALID_PARAMETER, want: false},
		{name: "TIME_OUT", ret: dcmi.ERROR_TIME_OUT, want: false},
		{name: "NOT_SUPPORT", ret: dcmi.ERROR_NOT_SUPPORT, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: dcmi.ERROR_LIBRARY_NOT_FOUND, want: false},
		{name: "FUNCTION_NOT_FOUND", ret: dcmi.ERROR_FUNCTION_NOT_FOUND, want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isUnqueryableDeviceError(tc.ret))
		})
	}
}

func TestSelectVersionedReturn(t *testing.T) {
	testCases := []struct {
		name  string
		v2Ret dcmi.Return
		v1Ret dcmi.Return
		want  dcmi.Return
	}{
		{name: "v1 succeeds", v2Ret: dcmi.ERROR_NOT_SUPPORT, v1Ret: dcmi.SUCCESS, want: dcmi.SUCCESS},
		{name: "v1 unsupported preserves v2 error", v2Ret: dcmi.ERROR_DEVICE_NOT_EXIST, v1Ret: dcmi.ERROR_NOT_SUPPORT, want: dcmi.ERROR_DEVICE_NOT_EXIST},
		{name: "v1 missing preserves v2 permission error", v2Ret: dcmi.ERROR_OPER_NOT_PERMITTED, v1Ret: dcmi.ERROR_FUNCTION_NOT_FOUND, want: dcmi.ERROR_OPER_NOT_PERMITTED},
		{name: "v2 unsupported uses v1 error", v2Ret: dcmi.ERROR_NOT_SUPPORT, v1Ret: dcmi.ERROR_IOCTL_FAIL, want: dcmi.ERROR_IOCTL_FAIL},
		{name: "v2 missing uses v1 error", v2Ret: dcmi.ERROR_FUNCTION_NOT_FOUND, v1Ret: dcmi.ERROR_OPER_NOT_PERMITTED, want: dcmi.ERROR_OPER_NOT_PERMITTED},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, selectVersionedReturn(tc.v2Ret, tc.v1Ret))
		})
	}
}

func TestHandleCardQueryError(t *testing.T) {
	testCases := []struct {
		name         string
		ret          dcmi.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "DEVICE_NOT_EXIST skips and records last error", ret: dcmi.ERROR_DEVICE_NOT_EXIST, wantFatalErr: false, wantLastErr: true},
		{name: "IOCTL_FAIL skips and records last error", ret: dcmi.ERROR_IOCTL_FAIL, wantFatalErr: false, wantLastErr: true},
		{name: "OPER_NOT_PERMITTED fails pass immediately", ret: dcmi.ERROR_OPER_NOT_PERMITTED, wantFatalErr: true, wantLastErr: false},
		{name: "TIME_OUT fails pass immediately", ret: dcmi.ERROR_TIME_OUT, wantFatalErr: true, wantLastErr: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var lastErr error
			err := handleCardQueryError(klog.Background(), 1, "get device vdie info", tc.ret, &lastErr)
			if tc.wantFatalErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.ret)
				assert.NoError(t, lastErr)
			} else {
				assert.NoError(t, err)
				require.Error(t, lastErr)
				assert.ErrorIs(t, lastErr, tc.ret)
			}
		})
	}
}
