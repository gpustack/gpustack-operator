package ascend

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
