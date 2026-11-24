package thead

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/hgml"
	"gpustack.ai/gpustack/pkg/device"
)

const hgmlFixtureSource = `
#include "hgml.h"
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int init_calls = 0;
static int is_initialized = 0;

static const char *fixture_env(const char *name) {
    const char *value = getenv(name);
    return value == NULL ? "" : value;
}

static unsigned int device_index(hgmlDevice_t device) {
    return (unsigned int)((uintptr_t)device.handle - 1U);
}

static hgmlReturn_t maybe_fail_device(const char *stage, unsigned int idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_HGML_FAIL_STAGE"), stage) != 0)
        return HGML_SUCCESS;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_HGML_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return HGML_SUCCESS;
    if (strcmp(fail_idx, "all") != 0 && (unsigned int)strtoul(fail_idx, NULL, 10) != idx)
        return HGML_SUCCESS;
    return (hgmlReturn_t)atoi(fixture_env("GPUSTACK_TEST_HGML_FAIL_ERR"));
}

hgmlReturn_t hgmlInit_v2(void) {
    const char *err_env = fixture_env("GPUSTACK_TEST_HGML_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_HGML_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (hgmlReturn_t)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetCount_v2(unsigned int *count) {
    if (strcmp(fixture_env("GPUSTACK_TEST_HGML_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return HGML_ERROR_UNINITIALIZED;
    const char *err_env = fixture_env("GPUSTACK_TEST_HGML_COUNT_ERR");
    if (err_env[0] != '\0')
        return (hgmlReturn_t)atoi(err_env);
    const char *cnt_env = fixture_env("GPUSTACK_TEST_HGML_COUNT");
    *count = cnt_env[0] == '\0' ? 1U : (unsigned int)strtoul(cnt_env, NULL, 10);
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetHandleByIndex_v2(unsigned int index, hgmlDevice_t *device) {
    hgmlReturn_t ret = maybe_fail_device("handle", index);
    if (ret != HGML_SUCCESS)
        return ret;
    device->handle = (struct hgmlDevice_st *)(uintptr_t)(index + 1U);
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetUUID(hgmlDevice_t device, char *uuid, unsigned int length) {
    unsigned int idx = device_index(device);
    hgmlReturn_t ret = maybe_fail_device("uuid", idx);
    if (ret != HGML_SUCCESS)
        return ret;
    snprintf(uuid, length, "PPU-%u", idx);
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetPciInfo_v2(hgmlDevice_t device, hgmlPciInfo_t *pci) {
    unsigned int idx = device_index(device);
    hgmlReturn_t ret = maybe_fail_device("pci", idx);
    if (ret != HGML_SUCCESS)
        return ret;
    memset(pci, 0, sizeof(*pci));
    pci->domain = 0;
    pci->bus = idx + 1U;
    pci->device = 0;
    snprintf(pci->busId, sizeof(pci->busId), "0000:%02x:00.0", idx + 1U);
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetName(hgmlDevice_t device, char *name, unsigned int length) {
    unsigned int idx = device_index(device);
    hgmlReturn_t ret = maybe_fail_device("name", idx);
    if (ret != HGML_SUCCESS)
        return ret;
    snprintf(name, length, "T-Head PPU fixture");
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetMinorNumber(hgmlDevice_t device, unsigned int *minorNumber) {
    *minorNumber = device_index(device);
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetMemoryInfo_v2(hgmlDevice_t device, hgmlMemory_v2_t *memory) {
    unsigned int idx = device_index(device);
    hgmlReturn_t ret = maybe_fail_device("memory", idx);
    if (ret != HGML_SUCCESS)
        return ret;
    memory->total = 65536ULL << 20;
    memory->free = memory->total;
    memory->used = 0;
    return HGML_SUCCESS;
}

hgmlReturn_t hgmlDeviceGetMemoryInfo(hgmlDevice_t device, hgmlMemory_t *memory) {
    unsigned int idx = device_index(device);
    hgmlReturn_t ret = maybe_fail_device("memory", idx);
    if (ret != HGML_SUCCESS)
        return ret;
    memory->total = 65536ULL << 20;
    memory->free = memory->total;
    memory->used = 0;
    return HGML_SUCCESS;
}
`

func compileHGMLFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "fixture_hgml.c")
	require.NoError(t, os.WriteFile(src, []byte(hgmlFixtureSource), 0o600))

	flags := make([]string, 0, 6)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-I../../../../binding/hgml", src, "-o", filepath.Join(dir, "libhgml.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

// TestUnqueryableCardAndErrorHandling verifies T-Head unqueryable-card skipping, enumeration index
// preservation, error propagation, all-cards-failed handling, and retryable initialization.
func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_HGML_MODE"); mode != "" {
		d := New(device.DetectorOptions{})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "PPU-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "PPU-2", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "PPU-0", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "PPU-2", metrics[0].Accelerators[1].ID)
		case "fatal_per_card":
			groups, err := d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)
		case "all_unqueryable":
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
			assert.Equal(t, "PPU-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
		default:
			t.Fatalf("unknown mode %q", mode)
		}
		return
	}

	dir := compileHGMLFixture(t)

	runCase := func(t *testing.T, extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnqueryableCardAndErrorHandling$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"LD_LIBRARY_PATH="+dir,
			"DYLD_LIBRARY_PATH="+dir,
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	t.Run("skip PPU-1 with GPU_NOT_FOUND and preserve index 2", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_HGML_MODE=skip_single",
			"GPUSTACK_TEST_HGML_COUNT=3",
			"GPUSTACK_TEST_HGML_FAIL_STAGE=handle",
			"GPUSTACK_TEST_HGML_FAIL_INDEX=1",
			"GPUSTACK_TEST_HGML_FAIL_ERR="+strconv.Itoa(int(hgml.ERROR_GPU_NOT_FOUND)),
		)
	})

	t.Run("NO_PERMISSION on handle fails detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_HGML_MODE=fatal_per_card",
			"GPUSTACK_TEST_HGML_COUNT=2",
			"GPUSTACK_TEST_HGML_FAIL_STAGE=handle",
			"GPUSTACK_TEST_HGML_FAIL_INDEX=1",
			"GPUSTACK_TEST_HGML_FAIL_ERR="+strconv.Itoa(int(hgml.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("all PPUs unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_HGML_MODE=all_unqueryable",
			"GPUSTACK_TEST_HGML_COUNT=2",
			"GPUSTACK_TEST_HGML_FAIL_STAGE=handle",
			"GPUSTACK_TEST_HGML_FAIL_INDEX=all",
			"GPUSTACK_TEST_HGML_FAIL_ERR="+strconv.Itoa(int(hgml.ERROR_GPU_IS_LOST)),
		)
	})

	t.Run("transient Init UNINITIALIZED recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_HGML_MODE=transient_init_error",
			"GPUSTACK_TEST_HGML_COUNT=1",
			"GPUSTACK_TEST_HGML_REQUIRE_INIT=true",
			"GPUSTACK_TEST_HGML_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_HGML_INIT_ERR="+strconv.Itoa(int(hgml.ERROR_UNINITIALIZED)),
		)
	})
}

// TestRetainDroppedKnownCards verifies that a previously detected T-Head card that later
// becomes unqueryable is retained in place as unhealthy across repeated passes, recovers when
// live queries succeed again, returns an error when all cards become unqueryable, and is pruned
// after a clean device count shrink.
func TestRetainDroppedKnownCards(t *testing.T) {
	if scenario := os.Getenv("GPUSTACK_TEST_HGML_RETAIN_SCENARIO"); scenario != "" {
		d := New(device.DetectorOptions{})
		switch scenario {
		case "drop_repeat_recover":
			// Pass 1: 3 healthy cards detected live (also seeds monitor cache).
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			for i := 0; i < 3; i++ {
				assert.Equal(t, "PPU-"+strconv.Itoa(i), groups[0].Accelerators[i].ID)
				assert.Equal(t, uint32(i), groups[0].Accelerators[i].Index)
				assert.Equal(t, []uint32{uint32(i)}, groups[0].Accelerators[i].PhysicalIndexes)
				assert.False(t, groups[0].Accelerators[i].Status.Unhealthy)
			}

			// Pass 2 & 3: card 1 becomes unqueryable; Monitor runs first before ever succeeding live.
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_INDEX", "1")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_ERR", strconv.Itoa(int(hgml.ERROR_GPU_IS_LOST)))

			for pass := 2; pass <= 3; pass++ {
				metrics, err := d.MonitorAccelerator(true)
				require.NoError(t, err, "pass %d monitor", pass)
				require.Len(t, metrics, 1)
				require.Len(t, metrics[0].Accelerators, 3)
				assert.Equal(t, "PPU-0", metrics[0].Accelerators[0].ID)
				assert.False(t, metrics[0].Accelerators[0].Unhealthy)
				assert.Equal(t, "PPU-1", metrics[0].Accelerators[1].ID)
				assert.Equal(t, uint64(65536), metrics[0].Accelerators[1].Memory)
				assert.True(t, metrics[0].Accelerators[1].Unhealthy)
				assert.Equal(t, "PPU-2", metrics[0].Accelerators[2].ID)
				assert.False(t, metrics[0].Accelerators[2].Unhealthy)

				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err, "pass %d detect", pass)
				require.Len(t, groups, 1)
				require.Len(t, groups[0].Accelerators, 3)
				assert.Equal(t, "PPU-0", groups[0].Accelerators[0].ID)
				assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
				assert.Equal(t, []uint32{0}, groups[0].Accelerators[0].PhysicalIndexes)
				assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
				assert.Equal(t, "PPU-1", groups[0].Accelerators[1].ID)
				assert.Equal(t, uint32(1), groups[0].Accelerators[1].Index)
				assert.Equal(t, []uint32{1}, groups[0].Accelerators[1].PhysicalIndexes)
				assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
				assert.Equal(t, "PPU-2", groups[0].Accelerators[2].ID)
				assert.Equal(t, uint32(2), groups[0].Accelerators[2].Index)
				assert.Equal(t, []uint32{2}, groups[0].Accelerators[2].PhysicalIndexes)
				assert.False(t, groups[0].Accelerators[2].Status.Unhealthy)
			}

			// Pass 4: card 1 recovers.
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_STAGE", "")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_INDEX", "")

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

			t.Setenv("GPUSTACK_TEST_HGML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_INDEX", "all")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_ERR", strconv.Itoa(int(hgml.ERROR_GPU_IS_LOST)))

			groups, err = d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err = d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)

			// Partial recovery confirms the all-unqueryable pass did not mutate cache.
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_INDEX", "1")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
			assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)

		case "clean_shrink_prunes_cache":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)

			// Clean shrink to 2 cards with no errors.
			t.Setenv("GPUSTACK_TEST_HGML_COUNT", "2")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)

			// Count returns to 3 with slot 2 unqueryable; pruned slot 2 is not resurrected.
			t.Setenv("GPUSTACK_TEST_HGML_COUNT", "3")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_INDEX", "2")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_ERR", strconv.Itoa(int(hgml.ERROR_GPU_NOT_FOUND)))

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "PPU-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, "PPU-1", groups[0].Accelerators[1].ID)

			metrics, err = d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)

			// Clean shrink to 0 clears cache.
			t.Setenv("GPUSTACK_TEST_HGML_COUNT", "0")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_STAGE", "")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, groups)

			t.Setenv("GPUSTACK_TEST_HGML_COUNT", "2")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_STAGE", "handle")
			t.Setenv("GPUSTACK_TEST_HGML_FAIL_INDEX", "1")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 1)
			assert.Equal(t, "PPU-0", groups[0].Accelerators[0].ID)

		default:
			t.Fatalf("unknown scenario %q", scenario)
		}
		return
	}

	dir := compileHGMLFixture(t)

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
				"GPUSTACK_TEST_HGML_RETAIN_SCENARIO="+tc.scenario,
				"GPUSTACK_TEST_HGML_COUNT="+tc.count,
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
		ret  hgml.Return
		want bool
	}{
		{name: "GPU_IS_LOST", ret: hgml.ERROR_GPU_IS_LOST, want: true},
		{name: "UNKNOWN", ret: hgml.ERROR_UNKNOWN, want: true},
		{name: "RESET_REQUIRED", ret: hgml.ERROR_RESET_REQUIRED, want: true},
		{name: "INSUFFICIENT_POWER", ret: hgml.ERROR_INSUFFICIENT_POWER, want: true},
		{name: "IRQ_ISSUE", ret: hgml.ERROR_IRQ_ISSUE, want: true},
		{name: "GPU_NOT_FOUND", ret: hgml.ERROR_GPU_NOT_FOUND, want: true},
		{name: "SUCCESS", ret: hgml.SUCCESS, want: false},
		{name: "NO_PERMISSION", ret: hgml.ERROR_NO_PERMISSION, want: false},
		{name: "UNINITIALIZED", ret: hgml.ERROR_UNINITIALIZED, want: false},
		{name: "TIMEOUT", ret: hgml.ERROR_TIMEOUT, want: false},
		{name: "NOT_SUPPORTED", ret: hgml.ERROR_NOT_SUPPORTED, want: false},
		{name: "NOT_FOUND", ret: hgml.ERROR_NOT_FOUND, want: false},
		{name: "DRIVER_NOT_LOADED", ret: hgml.ERROR_DRIVER_NOT_LOADED, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: hgml.ERROR_LIBRARY_NOT_FOUND, want: false},
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
		ret         hgml.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: hgml.SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: hgml.ERROR_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "driver not loaded with no pci devices", ret: hgml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "driver not loaded with pci devices present", ret: hgml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "library not found with pci devices present", ret: hgml.ERROR_LIBRARY_NOT_FOUND, noPciCheck: false, pciDevCount: 1, wantErr: true},
		{name: "no permission with noPciCheck", ret: hgml.ERROR_NO_PERMISSION, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "uninitialized with noPciCheck", ret: hgml.ERROR_UNINITIALIZED, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "timeout with pci devices", ret: hgml.ERROR_TIMEOUT, noPciCheck: false, pciDevCount: 1, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize HGML", tc.ret, tc.noPciCheck, tc.pciDevCount)
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
		v2Ret hgml.Return
		v1Ret hgml.Return
		want  hgml.Return
	}{
		{name: "v1 succeeds", v2Ret: hgml.ERROR_NOT_SUPPORTED, v1Ret: hgml.SUCCESS, want: hgml.SUCCESS},
		{name: "v2 unsupported uses v1 error", v2Ret: hgml.ERROR_NOT_SUPPORTED, v1Ret: hgml.ERROR_GPU_NOT_FOUND, want: hgml.ERROR_GPU_NOT_FOUND},
		{name: "v2 function missing uses v1 error", v2Ret: hgml.ERROR_FUNCTION_NOT_FOUND, v1Ret: hgml.ERROR_NO_PERMISSION, want: hgml.ERROR_NO_PERMISSION},
		{name: "v2 lost preserved when v1 missing", v2Ret: hgml.ERROR_GPU_IS_LOST, v1Ret: hgml.ERROR_FUNCTION_NOT_FOUND, want: hgml.ERROR_GPU_IS_LOST},
		{name: "v2 no permission preserved when v1 missing", v2Ret: hgml.ERROR_NO_PERMISSION, v1Ret: hgml.ERROR_FUNCTION_NOT_FOUND, want: hgml.ERROR_NO_PERMISSION},
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
		ret          hgml.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "GPU_NOT_FOUND skips and records last error", ret: hgml.ERROR_GPU_NOT_FOUND, wantFatalErr: false, wantLastErr: true},
		{name: "GPU_IS_LOST skips and records last error", ret: hgml.ERROR_GPU_IS_LOST, wantFatalErr: false, wantLastErr: true},
		{name: "NO_PERMISSION fails pass immediately", ret: hgml.ERROR_NO_PERMISSION, wantFatalErr: true, wantLastErr: false},
		{name: "TIMEOUT fails pass immediately", ret: hgml.ERROR_TIMEOUT, wantFatalErr: true, wantLastErr: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var lastErr error
			err := handleCardQueryError(klog.Background(), 1, "get device handle", tc.ret, &lastErr)
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

// TestEvaluateDeviceUnhealthy pins the THead health rule: an uncorrectable ECC error makes the
// card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name      string
		eccErrors uint64
		ret       hgml.Return
		want      bool
	}{
		{
			name:      "no uncorrectable ECC error is healthy",
			eccErrors: 0,
			ret:       hgml.SUCCESS,
			want:      false,
		},
		{
			name:      "an uncorrectable ECC error is unhealthy",
			eccErrors: 3,
			ret:       hgml.SUCCESS,
			want:      true,
		},
		{
			name:      "an unsupported ECC query is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       hgml.ERROR_NOT_SUPPORTED,
			want:      false,
		},
		{
			name:      "an ECC query whose driver is not loaded is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       hgml.ERROR_DRIVER_NOT_LOADED,
			want:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccErrors, tc.ret))
		})
	}
}
