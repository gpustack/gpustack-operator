// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package nvidia

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

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
