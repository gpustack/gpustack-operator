package hygon

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

	"gpustack.ai/gpustack/binding/rsmi"
	"gpustack.ai/gpustack/pkg/device"
)

// TestEvaluateDeviceUnhealthy pins the Hygon health rule: an uncorrectable UMC ECC error makes
// the card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name     string
		eccCount rsmi.ErrorCount
		ret      rsmi.Return
		want     bool
	}{
		{
			name:     "no uncorrectable ECC error is healthy",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 0},
			ret:      rsmi.STATUS_SUCCESS,
			want:     false,
		},
		{
			name:     "an uncorrectable ECC error is unhealthy",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 3},
			ret:      rsmi.STATUS_SUCCESS,
			want:     true,
		},
		{
			name:     "an unsupported ECC query is healthy even with a nonzero count",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 3},
			ret:      rsmi.STATUS_NOT_SUPPORTED,
			want:     false,
		},
		{
			name:     "an ECC query whose driver could not be initialized is healthy even with a nonzero count",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 3},
			ret:      rsmi.STATUS_INIT_ERROR,
			want:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccCount, tc.ret))
		})
	}
}

const rsmiFixtureSource = `
#include "rocm_smi.h"
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

static rsmi_status_t maybe_fail_device(const char *stage, uint32_t idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_RSMI_FAIL_STAGE"), stage) != 0)
        return RSMI_STATUS_SUCCESS;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_RSMI_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return RSMI_STATUS_SUCCESS;
    if (strcmp(fail_idx, "all") != 0 && (uint32_t)strtoul(fail_idx, NULL, 10) != idx)
        return RSMI_STATUS_SUCCESS;
    return (rsmi_status_t)atoi(fixture_env("GPUSTACK_TEST_RSMI_FAIL_ERR"));
}

rsmi_status_t rsmi_init(uint64_t init_flags) {
    (void)init_flags;
    const char *err_env = fixture_env("GPUSTACK_TEST_RSMI_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_RSMI_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (rsmi_status_t)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    return RSMI_STATUS_SUCCESS;
}

rsmi_status_t rsmi_num_monitor_devices(uint32_t *num_devices) {
    if (strcmp(fixture_env("GPUSTACK_TEST_RSMI_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return RSMI_STATUS_INIT_ERROR;
    const char *err_env = fixture_env("GPUSTACK_TEST_RSMI_COUNT_ERR");
    if (err_env[0] != '\0')
        return (rsmi_status_t)atoi(err_env);
    const char *cnt_env = fixture_env("GPUSTACK_TEST_RSMI_COUNT");
    *num_devices = cnt_env[0] == '\0' ? 1U : (uint32_t)strtoul(cnt_env, NULL, 10);
    return RSMI_STATUS_SUCCESS;
}

rsmi_status_t rsmi_dev_unique_id_get(uint32_t dv_ind, uint64_t *id) {
    rsmi_status_t ret = maybe_fail_device("uuid", dv_ind);
    if (ret != RSMI_STATUS_SUCCESS)
        return ret;
    *id = (uint64_t)(dv_ind + 1U);
    return RSMI_STATUS_SUCCESS;
}

rsmi_status_t rsmi_dev_pci_id_get(uint32_t dv_ind, uint64_t *bdfid) {
    rsmi_status_t ret = maybe_fail_device("pci", dv_ind);
    if (ret != RSMI_STATUS_SUCCESS)
        return ret;
    *bdfid = ((uint64_t)(dv_ind + 1U) << 8);
    return RSMI_STATUS_SUCCESS;
}

rsmi_status_t rsmi_dev_memory_total_get(uint32_t dv_ind, rsmi_memory_type_t mem_type, uint64_t *total) {
    (void)mem_type;
    rsmi_status_t ret = maybe_fail_device("memory", dv_ind);
    if (ret != RSMI_STATUS_SUCCESS)
        return ret;
    *total = 65536ULL << 20;
    return RSMI_STATUS_SUCCESS;
}

rsmi_status_t rsmi_dev_name_get(uint32_t dv_ind, char *name, size_t len) {
    (void)dv_ind;
    snprintf(name, len, "Hygon DCU Fixture");
    return RSMI_STATUS_SUCCESS;
}
`

func compileRSMIFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "fixture_rsmi.c")
	require.NoError(t, os.WriteFile(src, []byte(rsmiFixtureSource), 0o600))

	flags := make([]string, 0, 6)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-I../../../../binding/rsmi", src, "-o", filepath.Join(dir, "librocm_smi64.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_RSMI_MODE"); mode != "" {
		d := New(device.DetectorOptions{})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "GPU-1", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "GPU-3", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "GPU-1", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "GPU-3", metrics[0].Accelerators[1].ID)
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
		case "retain_unhealthy_lifecycle":
			setFail := func(stage, index string, errCode rsmi.Return) {
				t.Setenv("GPUSTACK_TEST_RSMI_FAIL_STAGE", stage)
				t.Setenv("GPUSTACK_TEST_RSMI_FAIL_INDEX", index)
				if errCode == rsmi.STATUS_SUCCESS {
					t.Setenv("GPUSTACK_TEST_RSMI_FAIL_ERR", "")
				} else {
					t.Setenv("GPUSTACK_TEST_RSMI_FAIL_ERR", strconv.Itoa(int(errCode)))
				}
			}

			// Pass 1: all 3 cards healthy. Only DetectAccelerator runs first to confirm it seeds
			// the monitor cache as well.
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			for i, wantID := range []string{"GPU-1", "GPU-2", "GPU-3"} {
				assert.Equal(t, wantID, groups[0].Accelerators[i].ID)
				assert.Equal(t, uint32(i), groups[0].Accelerators[i].Index)
				assert.False(t, groups[0].Accelerators[i].Status.Unhealthy)
			}

			// Pass 2: card 1 becomes unqueryable; MonitorAccelerator runs before DetectAccelerator.
			setFail("uuid", "1", rsmi.STATUS_FILE_ERROR)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 3)
			assert.Equal(t, "GPU-1", metrics[0].Accelerators[0].ID)
			assert.False(t, metrics[0].Accelerators[0].Unhealthy)
			assert.Equal(t, "GPU-2", metrics[0].Accelerators[1].ID)
			assert.Equal(t, uint64(65536), metrics[0].Accelerators[1].Memory)
			assert.True(t, metrics[0].Accelerators[1].Unhealthy)
			assert.Equal(t, "GPU-3", metrics[0].Accelerators[2].ID)
			assert.False(t, metrics[0].Accelerators[2].Unhealthy)

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			assert.Equal(t, "GPU-1", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
			assert.Equal(t, "GPU-2", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(1), groups[0].Accelerators[1].Index)
			assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
			assert.Equal(t, "GPU-3", groups[0].Accelerators[2].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[2].Index)
			assert.False(t, groups[0].Accelerators[2].Status.Unhealthy)

			// Pass 3: card 1 remains unqueryable and is still retained as unhealthy.
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			assert.Equal(t, "GPU-2", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(1), groups[0].Accelerators[1].Index)
			assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)

			metrics, err = d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 3)
			assert.Equal(t, "GPU-2", metrics[0].Accelerators[1].ID)
			assert.Equal(t, uint64(65536), metrics[0].Accelerators[1].Memory)
			assert.True(t, metrics[0].Accelerators[1].Unhealthy)

			// Pass 4: card 1 recovers and returns healthy again.
			setFail("", "", rsmi.STATUS_SUCCESS)

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			for i := range groups[0].Accelerators {
				assert.False(t, groups[0].Accelerators[i].Status.Unhealthy)
			}

			metrics, err = d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 3)
			for i := range metrics[0].Accelerators {
				assert.False(t, metrics[0].Accelerators[i].Unhealthy)
			}
		case "cached_then_all_unqueryable":
			// Pass 1: populate cache with 3 healthy cards.
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)

			// Pass 2: all cards become unqueryable; must return error and not emit cached cards.
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_STAGE", "memory")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_INDEX", "all")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_ERR", strconv.Itoa(int(rsmi.STATUS_AMDGPU_RESTART_ERR)))

			groups, err = d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)

			// Pass 3: card 0 and 2 recover while card 1 stays unqueryable; cache was preserved.
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_INDEX", "1")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)
			assert.Equal(t, "GPU-2", groups[0].Accelerators[1].ID)
			assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
		case "clean_shrink_prunes_cache":
			// Pass 1: 3 healthy cards.
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 3)

			// Pass 2: clean shrink to 2 cards with zero errors.
			t.Setenv("GPUSTACK_TEST_RSMI_COUNT", "2")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)

			// Pass 3: count returns to 3 with slot 2 unqueryable; pruned slot 2 must not resurrect.
			t.Setenv("GPUSTACK_TEST_RSMI_COUNT", "3")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_STAGE", "uuid")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_INDEX", "2")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_ERR", strconv.Itoa(int(rsmi.STATUS_FILE_ERROR)))

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)

			metrics, err = d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)

			// Pass 4: clean shrink to 0 clears cache completely.
			t.Setenv("GPUSTACK_TEST_RSMI_COUNT", "0")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_STAGE", "")
			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, groups)

			t.Setenv("GPUSTACK_TEST_RSMI_COUNT", "2")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_STAGE", "uuid")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_INDEX", "0")
			t.Setenv("GPUSTACK_TEST_RSMI_FAIL_ERR", strconv.Itoa(int(rsmi.STATUS_FILE_ERROR)))

			groups, err = d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 1)
			assert.Equal(t, uint32(1), groups[0].Accelerators[0].Index)
		default:
			t.Fatalf("unknown mode %q", mode)
		}
		return
	}

	dir := compileRSMIFixture(t)

	runCase := func(t *testing.T, extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnqueryableCardAndErrorHandling$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"ROCM_SMI_LIB_PATH="+dir,
			"LD_LIBRARY_PATH="+dir,
			"DYLD_LIBRARY_PATH="+dir,
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	t.Run("skip DCU-1 on FILE_ERROR and preserve index 2", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=skip_single",
			"GPUSTACK_TEST_RSMI_COUNT=3",
			"GPUSTACK_TEST_RSMI_FAIL_STAGE=uuid",
			"GPUSTACK_TEST_RSMI_FAIL_INDEX=1",
			"GPUSTACK_TEST_RSMI_FAIL_ERR="+strconv.Itoa(int(rsmi.STATUS_FILE_ERROR)),
		)
	})

	t.Run("PERMISSION on card fails detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=fatal_per_card",
			"GPUSTACK_TEST_RSMI_COUNT=2",
			"GPUSTACK_TEST_RSMI_FAIL_STAGE=uuid",
			"GPUSTACK_TEST_RSMI_FAIL_INDEX=1",
			"GPUSTACK_TEST_RSMI_FAIL_ERR="+strconv.Itoa(int(rsmi.STATUS_PERMISSION)),
		)
	})

	t.Run("all DCUs unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=all_unqueryable",
			"GPUSTACK_TEST_RSMI_COUNT=2",
			"GPUSTACK_TEST_RSMI_FAIL_STAGE=memory",
			"GPUSTACK_TEST_RSMI_FAIL_INDEX=all",
			"GPUSTACK_TEST_RSMI_FAIL_ERR="+strconv.Itoa(int(rsmi.STATUS_AMDGPU_RESTART_ERR)),
		)
	})

	t.Run("GetDeviceCount PERMISSION returns error", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=count_error",
			"GPUSTACK_TEST_RSMI_COUNT_ERR="+strconv.Itoa(int(rsmi.STATUS_PERMISSION)),
		)
	})

	t.Run("transient Init INIT_ERROR recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=transient_init_error",
			"GPUSTACK_TEST_RSMI_COUNT=1",
			"GPUSTACK_TEST_RSMI_REQUIRE_INIT=true",
			"GPUSTACK_TEST_RSMI_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_RSMI_INIT_ERR="+strconv.Itoa(int(rsmi.STATUS_INIT_ERROR)),
		)
	})

	t.Run("retain dropped known card as unhealthy across passes and recover", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=retain_unhealthy_lifecycle",
			"GPUSTACK_TEST_RSMI_COUNT=3",
		)
	})

	t.Run("all DCUs unqueryable after caching returns error without mutating cache", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=cached_then_all_unqueryable",
			"GPUSTACK_TEST_RSMI_COUNT=3",
		)
	})

	t.Run("clean shrink prunes cache so dropped slot is not resurrected", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_RSMI_MODE=clean_shrink_prunes_cache",
			"GPUSTACK_TEST_RSMI_COUNT=3",
		)
	})
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         rsmi.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: rsmi.STATUS_SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: rsmi.STATUS_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "driver not loaded with no pci devices", ret: rsmi.STATUS_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "driver not loaded with pci devices present", ret: rsmi.STATUS_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "permission with noPciCheck", ret: rsmi.STATUS_PERMISSION, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "init error with noPciCheck", ret: rsmi.STATUS_INIT_ERROR, noPciCheck: true, pciDevCount: 0, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize RSMI", tc.ret, tc.noPciCheck, tc.pciDevCount)
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
		ret  rsmi.Return
		want bool
	}{
		{name: "FILE_ERROR", ret: rsmi.STATUS_FILE_ERROR, want: true},
		{name: "DIRECTORY_NOT_FOUND", ret: rsmi.STATUS_DIRECTORY_NOT_FOUND, want: true},
		{name: "NOT_FOUND", ret: rsmi.STATUS_NOT_FOUND, want: true},
		{name: "NO_DATA", ret: rsmi.STATUS_NO_DATA, want: true},
		{name: "BUSY", ret: rsmi.STATUS_BUSY, want: true},
		{name: "AMDGPU_RESTART_ERR", ret: rsmi.STATUS_AMDGPU_RESTART_ERR, want: true},
		{name: "INTERNAL_EXCEPTION", ret: rsmi.STATUS_INTERNAL_EXCEPTION, want: true},
		{name: "IPC_ERROR", ret: rsmi.STATUS_IPC_ERROR, want: true},
		{name: "SUCCESS", ret: rsmi.STATUS_SUCCESS, want: false},
		{name: "PERMISSION", ret: rsmi.STATUS_PERMISSION, want: false},
		{name: "INIT_ERROR", ret: rsmi.STATUS_INIT_ERROR, want: false},
		{name: "INVALID_ARGS", ret: rsmi.STATUS_INVALID_ARGS, want: false},
		{name: "NOT_SUPPORTED", ret: rsmi.STATUS_NOT_SUPPORTED, want: false},
		{name: "DRIVER_NOT_LOADED", ret: rsmi.STATUS_DRIVER_NOT_LOADED, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: rsmi.STATUS_LIBRARY_NOT_FOUND, want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isUnqueryableDeviceError(tc.ret))
		})
	}
}

func TestHandleCardQueryError(t *testing.T) {
	testCases := []struct {
		name         string
		ret          rsmi.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "FILE_ERROR skips and records last error", ret: rsmi.STATUS_FILE_ERROR, wantFatalErr: false, wantLastErr: true},
		{name: "AMDGPU_RESTART_ERR skips and records last error", ret: rsmi.STATUS_AMDGPU_RESTART_ERR, wantFatalErr: false, wantLastErr: true},
		{name: "PERMISSION fails pass immediately", ret: rsmi.STATUS_PERMISSION, wantFatalErr: true, wantLastErr: false},
		{name: "INIT_ERROR fails pass immediately", ret: rsmi.STATUS_INIT_ERROR, wantFatalErr: true, wantLastErr: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var lastErr error
			err := handleCardQueryError(klog.Background(), 1, "get device UUID", tc.ret, &lastErr)
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

func TestSnapshotLiveDetectedExcludesWithheldDivergentProfiles(t *testing.T) {
	shared := device.AcceleratorPhysicalSlicedProfile{Name: "2g.20gb", MemoryMib: 20480, Count: 2}
	grpList := device.DevicesGroupList{
		{
			ID:           "dcu-k100",
			Manufacturer: Manufacturer,
			Name:         "DCU K100",
			Memory:       65536,
			Accelerators: []device.Accelerator{
				{
					ID:    "DCU-0",
					Index: 0,
					Status: device.AcceleratorStatus{
						PhysicalSliced: device.AcceleratorPhysicalSliced{
							Count: 4,
							Profiles: []device.AcceleratorPhysicalSlicedProfile{
								shared,
								{Name: "1g.10gb", MemoryMib: 10240, Count: 4},
							},
						},
					},
				},
				{
					ID:    "DCU-1",
					Index: 1,
					Status: device.AcceleratorStatus{
						PhysicalSliced: device.AcceleratorPhysicalSliced{
							Count: 4,
							Profiles: []device.AcceleratorPhysicalSlicedProfile{
								shared,
								{Name: "1g.10gb", MemoryMib: 12288, Count: 4},
							},
						},
					},
				},
			},
		},
	}

	rejected := rejectDivergentGroupProfiles(&grpList[0])
	require.Len(t, rejected, 1)

	det := &hygon{
		cachedDetected:  make(map[int]cachedAccelerator),
		cachedMonitored: make(map[int]cachedMetrics),
	}
	liveSlots := map[int]struct{}{0: {}, 1: {}}
	det.updateDetectedCache(snapshotLiveDetected(grpList, liveSlots), map[int]cachedMetrics{}, map[int]struct{}{})

	var nextGrpList device.DevicesGroupList
	det.retainUnqueryableDetected(&nextGrpList, map[int]struct{}{}, 1)
	require.Len(t, nextGrpList, 1)
	require.Len(t, nextGrpList[0].Accelerators, 1)

	restored := nextGrpList[0].Accelerators[0]
	assert.Equal(t, "DCU-1", restored.ID)
	assert.True(t, restored.Status.Unhealthy)
	assert.Equal(t, int32(2), restored.Status.PhysicalSliced.Count)
	assert.Equal(t, []device.AcceleratorPhysicalSlicedProfile{shared}, restored.Status.PhysicalSliced.Profiles)
}
