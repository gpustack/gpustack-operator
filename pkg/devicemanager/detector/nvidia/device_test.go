// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package nvidia

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// compileNVMLFixture builds the simulated driver every subprocess test in this file loads, one
// library per test directory.
func compileNVMLFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	flags := make([]string, 0, 7)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-DNVML_NO_UNVERSIONED_FUNC_DEFS=1", "-I../../../../binding/nvml",
		"testdata/memory_nvml.c", "-o", filepath.Join(dir, "libnvidia-ml.so.1"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

// TestDetectAcceleratorMemory drives the real DetectAccelerator and Node-label construction
// against a simulated NVML driver in a subprocess, one fixture library per case. The capacity
// cases encode the ECC memory rule: NVML reports the total with the current ECC mode applied,
// and the nominal size is restored only on pre-GDDR7 GDDR parts with ECC enabled -- measured on
// an ECC-enabled RTX PRO 6000 Blackwell Server Edition (driver 580.173.02): 97887 MiB total,
// 512-bit bus, compute capability 12.0, no ECC carve.
func TestDetectAcceleratorMemory(t *testing.T) {
	// Each subprocess loads its own NVML fixture without affecting other tests.
	if os.Getenv("GPUSTACK_TEST_NVML_MEMORY") == "true" {
		groups, err := New(device.DetectorOptions{}).DetectAccelerator(true)
		require.NoError(t, err)
		require.Len(t, groups, 1)
		want, err := strconv.ParseUint(os.Getenv("GPUSTACK_TEST_NVML_WANT_MIB"), 10, 64)
		require.NoError(t, err)
		assert.Equal(t, want, groups[0].Memory)
		require.Len(t, groups[0].Accelerators, 1)
		labels := nodefeature.ConstructAcceleratableNodeLabels(groups)
		key := nodefeature.AcceleratableFeatureLabelPrefix + Manufacturer + "-" + groups[0].ID + ".memory"
		assert.Equal(t, os.Getenv("GPUSTACK_TEST_NVML_DISPLAY_MEMORY"), labels[key])
		return
	}

	dir := compileNVMLFixture(t)

	cases := []struct {
		name     string
		memory   uint64 // fixture's MemoryInfo total, in MiB
		want     uint64 // expected DevicesGroup.Memory after the ECC rule, in MiB
		display  string // expected Node memory label
		busWidth uint32
		busErr   bool
		ecc      bool
		eccErr   bool
		ccMajor  int32
		ccMinor  int32
		ccErr    bool
		v1       bool
	}{
		// The measured RTX PRO 6000 readings. GDDR7 keeps ECC inside the DRAM die, so the
		// driver-reported total is already nominal and must not be restored (baseline bug: 102Gi).
		{
			name: "RTX PRO 6000 GDDR7 ECC", memory: 97887, want: 97887, display: "96Gi",
			busWidth: 512, ecc: true, ccMajor: 12, ccMinor: 0,
		},
		// L40S-class GDDR6: enabling ECC mode takes ~1/16 of the framebuffer, which the restore
		// gives back (broad-deletion regression: 45Gi).
		{
			name: "L40S GDDR6 ECC", memory: 46080, want: 49152, display: "48Gi",
			busWidth: 384, ecc: true, ccMajor: 8, ccMinor: 9,
		},
		// Another pre-GDDR7 generation on a narrower bus: the restore must not be tuned to 512-bit.
		{
			name: "Turing T4 GDDR6 ECC", memory: 15360, want: 16384, display: "16Gi",
			busWidth: 256, ecc: true, ccMajor: 7, ccMinor: 5,
		},
		// A narrow GDDR7 part: the generation, not the bus width, ends the restore.
		{
			name: "narrow GDDR7", memory: 8192, want: 8192, display: "8Gi",
			busWidth: 128, ecc: true, ccMajor: 12, ccMinor: 0,
		},
		// ECC mode off: nothing was taken, nothing to restore.
		{
			name: "GDDR ECC disabled", memory: 49152, want: 49152, display: "48Gi",
			busWidth: 384, ccMajor: 8, ccMinor: 9,
		},
		// HBM: ECC lives in hardware-reserved regions; NVML always reports full capacity.
		{
			name: "HBM ECC", memory: 81559, want: 81559, display: "80Gi",
			busWidth: 5120, ecc: true, ccMajor: 9, ccMinor: 0,
		},
		// Classifications the detector cannot make keep the driver-reported total.
		{
			name: "zero memory bus", memory: 46080, want: 46080, display: "45Gi",
			busWidth: 0, ecc: true, ccMajor: 8, ccMinor: 9,
		},
		{
			name: "memory bus unreadable", memory: 46080, want: 46080, display: "45Gi",
			busErr: true, ecc: true, ccMajor: 8, ccMinor: 9,
		},
		{
			name: "ECC mode unreadable", memory: 46080, want: 46080, display: "45Gi",
			busWidth: 384, eccErr: true, ccMajor: 8, ccMinor: 9,
		},
		{
			name: "compute capability unreadable", memory: 46080, want: 46080, display: "45Gi",
			busWidth: 384, ecc: true, ccErr: true,
		},
		// The V1 memory fallback rides the same rule.
		{
			name: "memory V1 fallback restores GDDR6", memory: 46080, want: 49152, display: "48Gi",
			busWidth: 384, ecc: true, ccMajor: 8, ccMinor: 9, v1: true,
		},
		{
			name: "memory V1 fallback keeps GDDR7", memory: 97887, want: 97887, display: "96Gi",
			busWidth: 512, ecc: true, ccMajor: 12, ccMinor: 0, v1: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDetectAcceleratorMemory$", "-test.count=1")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GPUSTACK_TEST_NVML_MEMORY=true",
				"GPUSTACK_TEST_NVML_TOTAL_MIB="+strconv.FormatUint(tc.memory, 10),
				"GPUSTACK_TEST_NVML_WANT_MIB="+strconv.FormatUint(tc.want, 10),
				"GPUSTACK_TEST_NVML_DISPLAY_MEMORY="+tc.display,
				"GPUSTACK_TEST_NVML_BUS_WIDTH="+strconv.FormatUint(uint64(tc.busWidth), 10),
				"GPUSTACK_TEST_NVML_BUS_WIDTH_ERROR="+strconv.FormatBool(tc.busErr),
				"GPUSTACK_TEST_NVML_ECC="+strconv.FormatBool(tc.ecc),
				"GPUSTACK_TEST_NVML_ECC_ERROR="+strconv.FormatBool(tc.eccErr),
				"GPUSTACK_TEST_NVML_CC_MAJOR="+strconv.FormatInt(int64(tc.ccMajor), 10),
				"GPUSTACK_TEST_NVML_CC_MINOR="+strconv.FormatInt(int64(tc.ccMinor), 10),
				"GPUSTACK_TEST_NVML_CC_ERROR="+strconv.FormatBool(tc.ccErr),
				"GPUSTACK_TEST_NVML_V1="+strconv.FormatBool(tc.v1),
				"LD_LIBRARY_PATH="+dir,
				"DYLD_LIBRARY_PATH="+dir,
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

// TestEvaluateDeviceUnhealthy pins the NVIDIA health rule: an uncorrectable DRAM ECC error makes
// the card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name      string
		eccErrors uint64
		ret       nvml.Return
		want      bool
	}{
		{
			name:      "no uncorrectable ECC error is healthy",
			eccErrors: 0,
			ret:       nvml.SUCCESS,
			want:      false,
		},
		{
			name:      "an uncorrectable ECC error is unhealthy",
			eccErrors: 3,
			ret:       nvml.SUCCESS,
			want:      true,
		},
		{
			name:      "an unsupported ECC query is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       nvml.ERROR_NOT_SUPPORTED,
			want:      false,
		},
		{
			name:      "an ECC query whose driver is not loaded is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       nvml.ERROR_DRIVER_NOT_LOADED,
			want:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccErrors, tc.ret))
		})
	}
}

// TestEvaluateRecoveryActionUnhealthy pins the recovery-action reading itself: the overall call,
// the field's own return, and the value's type each gate the verdict, and every action but
// "none" is a fault. The type gate doubles as the guard against an unanswered field, whose zero
// value carries DOUBLE.
func TestEvaluateRecoveryActionUnhealthy(t *testing.T) {
	fieldValue := func(action nvml.DeviceGpuRecoveryAction) [8]byte {
		var value [8]byte
		binary.LittleEndian.PutUint32(value[:4], uint32(action))
		return value
	}

	testCases := []struct {
		name      string
		fieldRet  nvml.Return
		nvmlRet   uint32
		valueType uint32
		value     [8]byte
		want      bool
	}{
		{
			name:      "a gpu reset action is unhealthy",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_GPU_RESET),
			want:      true,
		},
		{
			name:      "a node reboot action is unhealthy",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_NODE_REBOOT),
			want:      true,
		},
		{
			name:      "a drain p2p action is unhealthy",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_DRAIN_P2P),
			want:      true,
		},
		{
			name:      "a drain and reset action is unhealthy",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_DRAIN_AND_RESET),
			want:      true,
		},
		{
			name:      "an unlisted action is a fault too",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(99),
			want:      true,
		},
		{
			name:      "a none action is healthy",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_NONE),
			want:      false,
		},
		{
			name:      "a failed field-values call reads no verdict",
			fieldRet:  nvml.ERROR_NOT_SUPPORTED,
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_GPU_RESET),
			want:      false,
		},
		{
			name:      "a failed field reads no verdict",
			nvmlRet:   uint32(nvml.ERROR_NOT_SUPPORTED),
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_GPU_RESET),
			want:      false,
		},
		{
			name:      "a double value reads no verdict",
			valueType: uint32(nvml.VALUE_TYPE_DOUBLE),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_GPU_RESET),
			want:      false,
		},
		{
			name:      "a signed int value reads no verdict",
			valueType: uint32(nvml.VALUE_TYPE_SIGNED_INT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_GPU_RESET),
			want:      false,
		},
		{
			name:      "an unsigned short value reads no verdict",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_SHORT),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_GPU_RESET),
			want:      false,
		},
		{
			name:      "an unsigned long value decodes like an unsigned int",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_LONG),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_GPU_RESET),
			want:      true,
		},
		{
			name:      "an unsigned long long value decodes like an unsigned int",
			valueType: uint32(nvml.VALUE_TYPE_UNSIGNED_LONG_LONG),
			value:     fieldValue(nvml.GPU_RECOVERY_ACTION_DRAIN_AND_RESET),
			want:      true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			value := nvml.FieldValue{
				ValueType:  tc.valueType,
				NvmlReturn: tc.nvmlRet,
				Value:      tc.value,
			}
			assert.Equal(t, tc.want, evaluateRecoveryActionUnhealthy(tc.fieldRet, value))
		})
	}
}

// TestAcceleratorUnhealthy drives the real DetectAccelerator and MonitorAccelerator against a
// simulated NVML driver in a subprocess, one fixture library per case. The health it pins has
// two sources: an uncorrectable DRAM ECC count, and the driver's GPU recovery action -- the
// field a zero ECC count cannot replace, since a card whose driver asks for a reset, a reboot,
// or a drain is not healthy no matter what its counters read.
func TestAcceleratorUnhealthy(t *testing.T) {
	// Each subprocess loads its own NVML fixture without affecting other tests.
	if os.Getenv("GPUSTACK_TEST_NVML_HEALTH") == "true" {
		wantUnhealthy := os.Getenv("GPUSTACK_TEST_NVML_WANT_UNHEALTHY") == "true"
		if os.Getenv("GPUSTACK_TEST_NVML_MONITOR") == "true" {
			groups, err := New(device.DetectorOptions{}).MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 1)
			assert.Equal(t, wantUnhealthy, groups[0].Accelerators[0].Unhealthy)
			return
		}
		groups, err := New(device.DetectorOptions{}).DetectAccelerator(true)
		require.NoError(t, err)
		require.Len(t, groups, 1)
		require.Len(t, groups[0].Accelerators, 1)
		assert.Equal(t, wantUnhealthy, groups[0].Accelerators[0].Status.Unhealthy)
		return
	}

	dir := compileNVMLFixture(t)

	// value type 1 is VALUE_TYPE_UNSIGNED_INT, the type the field answers in; type 0 is
	// VALUE_TYPE_DOUBLE, what an unanswered field's zero value carries.
	cases := []struct {
		name           string
		eccUECounter   string // fixture's volatile uncorrected DRAM ECC count
		eccUEError     bool   // make the ECC query fail
		recoveryAction string // fixture's FI_DEV_GET_GPU_RECOVERY_ACTION value
		raType         string // fixture's value type for that field
		raError        bool   // fail the recovery action field alone
		fvError        bool   // fail the whole field-values call
		monitor        bool   // drive MonitorAccelerator instead of DetectAccelerator
		wantUnhealthy  bool
	}{
		{
			name:           "a gpu reset action with an unreadable ecc count is unhealthy",
			eccUEError:     true,
			recoveryAction: "1", raType: "1",
			wantUnhealthy: true,
		},
		{
			name:           "a node reboot action is unhealthy",
			recoveryAction: "2", raType: "1",
			wantUnhealthy: true,
		},
		{
			name:           "a drain p2p action is unhealthy",
			recoveryAction: "3", raType: "1",
			wantUnhealthy: true,
		},
		{
			name:           "a drain and reset action is unhealthy",
			recoveryAction: "4", raType: "1",
			wantUnhealthy: true,
		},
		{
			name:           "a none action with a clean ecc count is healthy",
			recoveryAction: "0", raType: "1",
			wantUnhealthy: false,
		},
		{
			name:          "a failed recovery action query is no verdict when ecc is clean",
			fvError:       true,
			wantUnhealthy: false,
		},
		{
			name:          "a failed recovery action field is no verdict when ecc is clean",
			raError:       true,
			wantUnhealthy: false,
		},
		{
			// DOUBLE is what an unanswered field's zero value carries; decoding it as an
			// integer would read garbage as a verdict.
			name:           "a value in the wrong type is no verdict",
			recoveryAction: "1", raType: "0",
			wantUnhealthy: false,
		},
		{
			name:         "an ecc fault stands when the recovery query fails",
			eccUECounter: "3", fvError: true,
			wantUnhealthy: true,
		},
		{
			name:         "an ecc fault stands when the recovery action is none",
			eccUECounter: "3", recoveryAction: "0", raType: "1",
			wantUnhealthy: true,
		},
		{
			name:           "monitoring reports the recovery action fault",
			eccUEError:     true,
			recoveryAction: "1", raType: "1",
			monitor:       true,
			wantUnhealthy: true,
		},
		{
			name:          "monitoring reports healthy with no verdict",
			fvError:       true,
			monitor:       true,
			wantUnhealthy: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAcceleratorUnhealthy$", "-test.count=1")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GPUSTACK_TEST_NVML_HEALTH=true",
				"GPUSTACK_TEST_NVML_ECC_UE_COUNTER="+tc.eccUECounter,
				"GPUSTACK_TEST_NVML_ECC_UE_ERROR="+strconv.FormatBool(tc.eccUEError),
				"GPUSTACK_TEST_NVML_RECOVERY_ACTION="+tc.recoveryAction,
				"GPUSTACK_TEST_NVML_RECOVERY_ACTION_TYPE="+tc.raType,
				"GPUSTACK_TEST_NVML_RECOVERY_ACTION_ERROR="+strconv.FormatBool(tc.raError),
				"GPUSTACK_TEST_NVML_FIELD_VALUES_ERROR="+strconv.FormatBool(tc.fvError),
				"GPUSTACK_TEST_NVML_MONITOR="+strconv.FormatBool(tc.monitor),
				"GPUSTACK_TEST_NVML_WANT_UNHEALTHY="+strconv.FormatBool(tc.wantUnhealthy),
				"LD_LIBRARY_PATH="+dir,
				"DYLD_LIBRARY_PATH="+dir,
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}
