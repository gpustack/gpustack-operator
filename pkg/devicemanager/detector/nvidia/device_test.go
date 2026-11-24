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

// TestUnqueryableCardAndErrorHandling drives DetectAccelerator and MonitorAccelerator against
// the simulated NVML driver in subprocesses to verify unqueryable-card skipping, enumeration
// index preservation, non-card-specific error propagation, all-cards-failed errors, and
// retryable library initialization.
func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_NVML_MODE"); mode != "" {
		d := New(device.DetectorOptions{})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "GPU-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "GPU-2", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			wantUnhealthy0 := os.Getenv("GPUSTACK_TEST_NVML_UNHEALTHY_INDEX") == "0"
			assert.Equal(t, wantUnhealthy0, groups[0].Accelerators[0].Status.Unhealthy)
			assert.False(t, groups[0].Accelerators[1].Status.Unhealthy)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			if os.Getenv("GPUSTACK_TEST_NVML_FAIL_STAGE") == "name" || os.Getenv("GPUSTACK_TEST_NVML_FAIL_STAGE") == "pci" {
				require.Len(t, metrics[0].Accelerators, 3)
			} else {
				require.Len(t, metrics[0].Accelerators, 2)
				assert.Equal(t, "GPU-0", metrics[0].Accelerators[0].ID)
				assert.Equal(t, "GPU-2", metrics[0].Accelerators[1].ID)
			}
		case "fatal_per_card":
			groups, err := d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			if os.Getenv("GPUSTACK_TEST_NVML_CHECK_MONITOR") == "true" {
				metrics, err := d.MonitorAccelerator(true)
				require.Error(t, err)
				assert.Nil(t, metrics)
			}
		case "all_unqueryable":
			groups, err := d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)
		case "count_error":
			groups, err := d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)
		case "transient_init_error":
			groups, err := d.DetectAccelerator(true)
			if os.Getenv("GPUSTACK_TEST_NVML_EXPECT_FIRST_ERR") == "true" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Nil(t, groups)

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 1)
			assert.Equal(t, "GPU-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
		case "init_library_not_found":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, metrics)
		default:
			t.Fatalf("unknown mode %q", mode)
		}
		return
	}

	dir := compileNVMLFixture(t)

	runCase := func(t *testing.T, extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnqueryableCardAndErrorHandling$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GPUSTACK_TEST_NVML_TOTAL_MIB=81920",
			"LD_LIBRARY_PATH="+dir,
			"DYLD_LIBRARY_PATH="+dir,
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	unqueryableCodes := []struct {
		name string
		code nvml.Return
	}{
		{name: "GPU_IS_LOST", code: nvml.ERROR_GPU_IS_LOST},
		{name: "UNKNOWN", code: nvml.ERROR_UNKNOWN},
		{name: "RESET_REQUIRED", code: nvml.ERROR_RESET_REQUIRED},
		{name: "INSUFFICIENT_POWER", code: nvml.ERROR_INSUFFICIENT_POWER},
		{name: "IRQ_ISSUE", code: nvml.ERROR_IRQ_ISSUE},
	}

	for _, stage := range []string{"handle", "uuid", "pci", "name", "memory"} {
		for _, ec := range unqueryableCodes {
			t.Run("skip GPU-1 on "+stage+" with "+ec.name, func(t *testing.T) {
				runCase(t,
					"GPUSTACK_TEST_NVML_MODE=skip_single",
					"GPUSTACK_TEST_NVML_COUNT=3",
					"GPUSTACK_TEST_NVML_FAIL_STAGE="+stage,
					"GPUSTACK_TEST_NVML_FAIL_INDEX=1",
					"GPUSTACK_TEST_NVML_FAIL_ERR="+strconv.Itoa(int(ec.code)),
				)
			})
		}
	}

	t.Run("preserve unhealthy card retention while skipping lost sibling", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_NVML_MODE=skip_single",
			"GPUSTACK_TEST_NVML_COUNT=3",
			"GPUSTACK_TEST_NVML_FAIL_STAGE=handle",
			"GPUSTACK_TEST_NVML_FAIL_INDEX=1",
			"GPUSTACK_TEST_NVML_FAIL_ERR="+strconv.Itoa(int(nvml.ERROR_GPU_IS_LOST)),
			"GPUSTACK_TEST_NVML_UNHEALTHY_INDEX=0",
		)
	})

	for _, tc := range []struct {
		stage        string
		checkMonitor bool
	}{
		{stage: "handle", checkMonitor: true},
		{stage: "uuid", checkMonitor: true},
		{stage: "pci", checkMonitor: false},
		{stage: "name", checkMonitor: false},
		{stage: "memory", checkMonitor: true},
	} {
		t.Run("NO_PERMISSION on "+tc.stage+" fails pass", func(t *testing.T) {
			runCase(t,
				"GPUSTACK_TEST_NVML_MODE=fatal_per_card",
				"GPUSTACK_TEST_NVML_COUNT=2",
				"GPUSTACK_TEST_NVML_FAIL_STAGE="+tc.stage,
				"GPUSTACK_TEST_NVML_FAIL_INDEX=1",
				"GPUSTACK_TEST_NVML_FAIL_ERR="+strconv.Itoa(int(nvml.ERROR_NO_PERMISSION)),
				"GPUSTACK_TEST_NVML_CHECK_MONITOR="+strconv.FormatBool(tc.checkMonitor),
			)
		})
	}

	t.Run("all GPUs returning GPU_IS_LOST returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_NVML_MODE=all_unqueryable",
			"GPUSTACK_TEST_NVML_COUNT=2",
			"GPUSTACK_TEST_NVML_FAIL_STAGE=handle",
			"GPUSTACK_TEST_NVML_FAIL_INDEX=all",
			"GPUSTACK_TEST_NVML_FAIL_ERR="+strconv.Itoa(int(nvml.ERROR_GPU_IS_LOST)),
		)
	})

	t.Run("DeviceGetCount returning NO_PERMISSION returns error", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_NVML_MODE=count_error",
			"GPUSTACK_TEST_NVML_COUNT_ERR="+strconv.Itoa(int(nvml.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("transient Init UNINITIALIZED recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_NVML_MODE=transient_init_error",
			"GPUSTACK_TEST_NVML_COUNT=1",
			"GPUSTACK_TEST_NVML_REQUIRE_INIT=true",
			"GPUSTACK_TEST_NVML_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_NVML_INIT_ERR="+strconv.Itoa(int(nvml.ERROR_UNINITIALIZED)),
			"GPUSTACK_TEST_NVML_EXPECT_FIRST_ERR=true",
		)
	})

	t.Run("transient Init DRIVER_NOT_LOADED recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_NVML_MODE=transient_init_error",
			"GPUSTACK_TEST_NVML_COUNT=1",
			"GPUSTACK_TEST_NVML_REQUIRE_INIT=true",
			"GPUSTACK_TEST_NVML_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_NVML_INIT_ERR="+strconv.Itoa(int(nvml.ERROR_DRIVER_NOT_LOADED)),
			"GPUSTACK_TEST_NVML_EXPECT_FIRST_ERR=false",
		)
	})

	t.Run("transient Init followed by ALREADY_INITIALIZED succeeds", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_NVML_MODE=transient_init_error",
			"GPUSTACK_TEST_NVML_COUNT=1",
			"GPUSTACK_TEST_NVML_REQUIRE_INIT=true",
			"GPUSTACK_TEST_NVML_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_NVML_INIT_ERR="+strconv.Itoa(int(nvml.ERROR_UNINITIALIZED)),
			"GPUSTACK_TEST_NVML_INIT_OK_RET="+strconv.Itoa(int(nvml.ERROR_ALREADY_INITIALIZED)),
			"GPUSTACK_TEST_NVML_EXPECT_FIRST_ERR=true",
		)
	})

	t.Run("Init returning LIBRARY_NOT_FOUND with noPciCheck returns nil nil", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_NVML_MODE=init_library_not_found",
			"GPUSTACK_TEST_NVML_INIT_ERR="+strconv.Itoa(int(nvml.ERROR_LIBRARY_NOT_FOUND)),
		)
	})
}

// TestRetainDroppedKnownCards verifies that a previously detected NVIDIA card that later
// becomes unqueryable is retained in place as unhealthy across repeated passes, recovers when
// live queries succeed again, returns an error when all cards become unqueryable, and is pruned
// after a clean device count shrink.
func TestRetainDroppedKnownCards(t *testing.T) {
	if scenario := os.Getenv("GPUSTACK_TEST_NVML_RETAIN_SCENARIO"); scenario != "" {
		d := New(device.DetectorOptions{})
		switch scenario {
		case "drop_repeat_recover":
			// Pass 1: 3 healthy cards detected live (also seeds monitor cache).
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			for i := 0; i < 3; i++ {
				assert.Equal(t, "GPU-"+strconv.Itoa(i), groups[0].Accelerators[i].ID)
				assert.Equal(t, uint32(i), groups[0].Accelerators[i].Index)
				assert.Equal(t, []uint32{uint32(i)}, groups[0].Accelerators[i].PhysicalIndexes)
				assert.False(t, groups[0].Accelerators[i].Status.Unhealthy)
			}

			// Pass 2: card 1 becomes unqueryable; Monitor runs first before ever succeeding live.
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_INDEX", "1")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_ERR", strconv.Itoa(int(nvml.ERROR_GPU_IS_LOST)))

			for pass := 2; pass <= 3; pass++ {
				metrics, err := d.MonitorAccelerator(true)
				require.NoError(t, err, "pass %d monitor", pass)
				require.Len(t, metrics, 1)
				require.Len(t, metrics[0].Accelerators, 3)
				assert.Equal(t, "GPU-0", metrics[0].Accelerators[0].ID)
				assert.False(t, metrics[0].Accelerators[0].Unhealthy)
				assert.Equal(t, "GPU-1", metrics[0].Accelerators[1].ID)
				assert.Equal(t, uint64(81920), metrics[0].Accelerators[1].Memory)
				assert.True(t, metrics[0].Accelerators[1].Unhealthy)
				assert.Equal(t, "GPU-2", metrics[0].Accelerators[2].ID)
				assert.False(t, metrics[0].Accelerators[2].Unhealthy)

				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err, "pass %d detect", pass)
				require.Len(t, groups, 1)
				require.Len(t, groups[0].Accelerators, 3)
				assert.Equal(t, "GPU-0", groups[0].Accelerators[0].ID)
				assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
				assert.Equal(t, []uint32{0}, groups[0].Accelerators[0].PhysicalIndexes)
				assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
				assert.Equal(t, "GPU-1", groups[0].Accelerators[1].ID)
				assert.Equal(t, uint32(1), groups[0].Accelerators[1].Index)
				assert.Equal(t, []uint32{1}, groups[0].Accelerators[1].PhysicalIndexes)
				assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
				assert.Equal(t, "GPU-2", groups[0].Accelerators[2].ID)
				assert.Equal(t, uint32(2), groups[0].Accelerators[2].Index)
				assert.Equal(t, []uint32{2}, groups[0].Accelerators[2].PhysicalIndexes)
				assert.False(t, groups[0].Accelerators[2].Status.Unhealthy)
			}

			// Pass 4: card 1 recovers.
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_STAGE", "")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_INDEX", "")

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			for i := 0; i < 3; i++ {
				assert.False(t, groups[0].Accelerators[i].Status.Unhealthy)
			}

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 3)
			for i := 0; i < 3; i++ {
				assert.False(t, metrics[0].Accelerators[i].Unhealthy)
			}

		case "all_unqueryable_after_cache":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)

			// All cards become unqueryable: must return error without mutating cache.
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_INDEX", "all")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_ERR", strconv.Itoa(int(nvml.ERROR_GPU_IS_LOST)))

			groups, err = d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err = d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)

			// Partial recovery: card 0 recovers while card 1 stays lost; cache is intact.
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_INDEX", "1")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
			assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)

		case "clean_shrink_prunes_cache":
			// Pass 1: 3 healthy cards.
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)

			// Pass 2: clean shrink to 2 cards with no errors.
			t.Setenv("GPUSTACK_TEST_NVML_COUNT", "2")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)

			// Pass 3: count returns to 3 with slot 2 immediately unqueryable; slot 2 was pruned.
			t.Setenv("GPUSTACK_TEST_NVML_COUNT", "3")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_INDEX", "2")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_ERR", strconv.Itoa(int(nvml.ERROR_GPU_IS_LOST)))

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "GPU-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, "GPU-1", groups[0].Accelerators[1].ID)

			metrics, err = d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)

			// Pass 4: clean shrink to 0 clears cache.
			t.Setenv("GPUSTACK_TEST_NVML_COUNT", "0")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_STAGE", "")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, groups)

			t.Setenv("GPUSTACK_TEST_NVML_COUNT", "2")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_NVML_FAIL_INDEX", "1")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 1)
			assert.Equal(t, "GPU-0", groups[0].Accelerators[0].ID)

		default:
			t.Fatalf("unknown scenario %q", scenario)
		}
		return
	}

	dir := compileNVMLFixture(t)

	testCases := []struct {
		name     string
		scenario string
		count    string
	}{
		{name: "drop repeat and recover", scenario: "drop_repeat_recover", count: "3"},
		{name: "all cards unqueryable after cache", scenario: "all_unqueryable_after_cache", count: "2"},
		{name: "clean shrink prunes cache", scenario: "clean_shrink_prunes_cache", count: "3"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRetainDroppedKnownCards$", "-test.count=1")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GPUSTACK_TEST_NVML_RETAIN_SCENARIO="+tc.scenario,
				"GPUSTACK_TEST_NVML_COUNT="+tc.count,
				"GPUSTACK_TEST_NVML_TOTAL_MIB=81920",
				"LD_LIBRARY_PATH="+dir,
				"DYLD_LIBRARY_PATH="+dir,
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

func TestIsUnqueryableDeviceError(t *testing.T) {
	testCases := []struct {
		name string
		ret  nvml.Return
		want bool
	}{
		{name: "GPU_IS_LOST", ret: nvml.ERROR_GPU_IS_LOST, want: true},
		{name: "UNKNOWN", ret: nvml.ERROR_UNKNOWN, want: true},
		{name: "RESET_REQUIRED", ret: nvml.ERROR_RESET_REQUIRED, want: true},
		{name: "INSUFFICIENT_POWER", ret: nvml.ERROR_INSUFFICIENT_POWER, want: true},
		{name: "IRQ_ISSUE", ret: nvml.ERROR_IRQ_ISSUE, want: true},
		{name: "SUCCESS", ret: nvml.SUCCESS, want: false},
		{name: "NO_PERMISSION", ret: nvml.ERROR_NO_PERMISSION, want: false},
		{name: "UNINITIALIZED", ret: nvml.ERROR_UNINITIALIZED, want: false},
		{name: "TIMEOUT", ret: nvml.ERROR_TIMEOUT, want: false},
		{name: "NOT_SUPPORTED", ret: nvml.ERROR_NOT_SUPPORTED, want: false},
		{name: "DRIVER_NOT_LOADED", ret: nvml.ERROR_DRIVER_NOT_LOADED, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: nvml.ERROR_LIBRARY_NOT_FOUND, want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isUnqueryableDeviceError(tc.ret))
		})
	}
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         nvml.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: nvml.SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: nvml.ERROR_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "driver not loaded with no pci devices", ret: nvml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "driver not loaded with pci devices present", ret: nvml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "library not found with pci devices present", ret: nvml.ERROR_LIBRARY_NOT_FOUND, noPciCheck: false, pciDevCount: 1, wantErr: true},
		{name: "no permission with noPciCheck", ret: nvml.ERROR_NO_PERMISSION, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "uninitialized with noPciCheck", ret: nvml.ERROR_UNINITIALIZED, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "timeout with pci devices", ret: nvml.ERROR_TIMEOUT, noPciCheck: false, pciDevCount: 1, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize NVML", tc.ret, tc.noPciCheck, tc.pciDevCount)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.ret)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSelectVersionedReturn(t *testing.T) {
	testCases := []struct {
		name  string
		v2Ret nvml.Return
		v1Ret nvml.Return
		want  nvml.Return
	}{
		{name: "v1 succeeds", v2Ret: nvml.ERROR_NOT_SUPPORTED, v1Ret: nvml.SUCCESS, want: nvml.SUCCESS},
		{name: "v2 unsupported uses v1 error", v2Ret: nvml.ERROR_NOT_SUPPORTED, v1Ret: nvml.ERROR_GPU_IS_LOST, want: nvml.ERROR_GPU_IS_LOST},
		{name: "v2 function missing uses v1 error", v2Ret: nvml.ERROR_FUNCTION_NOT_FOUND, v1Ret: nvml.ERROR_NO_PERMISSION, want: nvml.ERROR_NO_PERMISSION},
		{name: "v2 lost preserved when v1 missing", v2Ret: nvml.ERROR_GPU_IS_LOST, v1Ret: nvml.ERROR_FUNCTION_NOT_FOUND, want: nvml.ERROR_GPU_IS_LOST},
		{name: "v2 no permission preserved when v1 missing", v2Ret: nvml.ERROR_NO_PERMISSION, v1Ret: nvml.ERROR_FUNCTION_NOT_FOUND, want: nvml.ERROR_NO_PERMISSION},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, selectVersionedReturn(tc.v2Ret, tc.v1Ret))
		})
	}
}
