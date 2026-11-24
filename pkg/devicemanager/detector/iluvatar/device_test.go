package iluvatar

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

	"gpustack.ai/gpustack/binding/ixml"
	"gpustack.ai/gpustack/pkg/device"
)

// TestEvaluateDeviceUnhealthy pins the Iluvatar health rule: any health state other than the
// vendor's OK makes the card unhealthy, while a state the driver could not read is not a fault of
// the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name   string
		health uint64
		ret    ixml.Return
		want   bool
	}{
		{
			name:   "the OK health state is healthy",
			health: ixml.HealthOK,
			ret:    ixml.SUCCESS,
			want:   false,
		},
		{
			name:   "a memory error health state is unhealthy",
			health: ixml.HealthMemoryError,
			ret:    ixml.SUCCESS,
			want:   true,
		},
		{
			name:   "an unsupported health query is healthy even with a faulty state",
			health: ixml.HealthMemoryError,
			ret:    ixml.ERROR_NOT_SUPPORTED,
			want:   false,
		},
		{
			name:   "a health query whose driver is not loaded is healthy even with a faulty state",
			health: ixml.HealthMemoryError,
			ret:    ixml.ERROR_DRIVER_NOT_LOADED,
			want:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.health, tc.ret))
		})
	}
}

const ixmlFixtureSource = `
#include "ixml.h"
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

static unsigned int device_index(nvmlDevice_t device) {
    return (unsigned int)((uintptr_t)device.handle - 1U);
}

static nvmlReturn_t maybe_fail_device(const char *stage, unsigned int idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_IXML_FAIL_STAGE"), stage) != 0)
        return NVML_SUCCESS;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_IXML_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return NVML_SUCCESS;
    if (strcmp(fail_idx, "all") != 0 && (unsigned int)strtoul(fail_idx, NULL, 10) != idx)
        return NVML_SUCCESS;
    return (nvmlReturn_t)atoi(fixture_env("GPUSTACK_TEST_IXML_FAIL_ERR"));
}

nvmlReturn_t nvmlInit_v2(void) {
    const char *err_env = fixture_env("GPUSTACK_TEST_IXML_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_IXML_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (nvmlReturn_t)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    const char *ok_ret_env = fixture_env("GPUSTACK_TEST_IXML_INIT_OK_RET");
    if (ok_ret_env[0] != '\0')
        return (nvmlReturn_t)atoi(ok_ret_env);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetCount_v2(unsigned int *count) {
    if (strcmp(fixture_env("GPUSTACK_TEST_IXML_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return NVML_ERROR_UNINITIALIZED;
    const char *err_env = fixture_env("GPUSTACK_TEST_IXML_COUNT_ERR");
    if (err_env[0] != '\0')
        return (nvmlReturn_t)atoi(err_env);
    const char *cnt_env = fixture_env("GPUSTACK_TEST_IXML_COUNT");
    *count = cnt_env[0] == '\0' ? 1U : (unsigned int)strtoul(cnt_env, NULL, 10);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetHandleByIndex_v2(unsigned int index, nvmlDevice_t *device) {
    nvmlReturn_t ret = maybe_fail_device("handle", index);
    if (ret != NVML_SUCCESS)
        return ret;
    device->handle = (struct nvmlDevice_st *)(uintptr_t)(index + 1U);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetUUID(nvmlDevice_t device, char *uuid, unsigned int length) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("uuid", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    snprintf(uuid, length, "IX-GPU-%u", idx);
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetPciInfo_v3(nvmlDevice_t device, nvmlPciInfo_t *pci) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("pci", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    memset(pci, 0, sizeof(*pci));
    pci->domain = 0;
    pci->bus = idx + 1U;
    pci->device = 0;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetName(nvmlDevice_t device, char *name, unsigned int length) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("name", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    snprintf(name, length, "Iluvatar BI-V150");
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo_v2(nvmlDevice_t device, nvmlMemory_v2_t *memory) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("memory", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    memory->total = 32768ULL << 20;
    memory->free = memory->total;
    memory->used = 0;
    return NVML_SUCCESS;
}

nvmlReturn_t nvmlDeviceGetMemoryInfo(nvmlDevice_t device, nvmlMemory_t *memory) {
    unsigned int idx = device_index(device);
    nvmlReturn_t ret = maybe_fail_device("memory", idx);
    if (ret != NVML_SUCCESS)
        return ret;
    memory->total = 32768ULL << 20;
    memory->free = memory->total;
    memory->used = 0;
    return NVML_SUCCESS;
}
`

func compileIXMLFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib64")
	require.NoError(t, os.MkdirAll(libDir, 0o755))
	src := filepath.Join(dir, "fixture_ixml.c")
	require.NoError(t, os.WriteFile(src, []byte(ixmlFixtureSource), 0o600))

	flags := make([]string, 0, 7)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-DNVML_NO_UNVERSIONED_FUNC_DEFS=1", "-I../../../../binding/ixml",
		src, "-o", filepath.Join(libDir, "libixml.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_IXML_MODE"); mode != "" {
		d := New(device.DetectorOptions{})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "IX-GPU-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "IX-GPU-2", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "IX-GPU-0", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "IX-GPU-2", metrics[0].Accelerators[1].ID)
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

	dir := compileIXMLFixture(t)

	runCase := func(t *testing.T, extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnqueryableCardAndErrorHandling$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"COREX_HOME="+dir,
			"LD_LIBRARY_PATH="+filepath.Join(dir, "lib64"),
			"DYLD_LIBRARY_PATH="+filepath.Join(dir, "lib64"),
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	t.Run("skip GPU-1 on GPU_IS_LOST and preserve index 2", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_IXML_MODE=skip_single",
			"GPUSTACK_TEST_IXML_COUNT=3",
			"GPUSTACK_TEST_IXML_FAIL_STAGE=handle",
			"GPUSTACK_TEST_IXML_FAIL_INDEX=1",
			"GPUSTACK_TEST_IXML_FAIL_ERR="+strconv.Itoa(int(ixml.ERROR_GPU_IS_LOST)),
		)
	})

	t.Run("NO_PERMISSION on card fails detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_IXML_MODE=fatal_per_card",
			"GPUSTACK_TEST_IXML_COUNT=2",
			"GPUSTACK_TEST_IXML_FAIL_STAGE=handle",
			"GPUSTACK_TEST_IXML_FAIL_INDEX=1",
			"GPUSTACK_TEST_IXML_FAIL_ERR="+strconv.Itoa(int(ixml.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("all GPUs unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_IXML_MODE=all_unqueryable",
			"GPUSTACK_TEST_IXML_COUNT=2",
			"GPUSTACK_TEST_IXML_FAIL_STAGE=memory",
			"GPUSTACK_TEST_IXML_FAIL_INDEX=all",
			"GPUSTACK_TEST_IXML_FAIL_ERR="+strconv.Itoa(int(ixml.ERROR_GPU_IS_LOST)),
		)
	})

	t.Run("DeviceGetCount NO_PERMISSION returns error", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_IXML_MODE=count_error",
			"GPUSTACK_TEST_IXML_COUNT_ERR="+strconv.Itoa(int(ixml.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("transient Init UNINITIALIZED followed by ALREADY_INITIALIZED succeeds", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_IXML_MODE=transient_init_error",
			"GPUSTACK_TEST_IXML_COUNT=1",
			"GPUSTACK_TEST_IXML_REQUIRE_INIT=true",
			"GPUSTACK_TEST_IXML_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_IXML_INIT_ERR="+strconv.Itoa(int(ixml.ERROR_UNINITIALIZED)),
			"GPUSTACK_TEST_IXML_INIT_OK_RET="+strconv.Itoa(int(ixml.ERROR_ALREADY_INITIALIZED)),
		)
	})
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         ixml.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: ixml.SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: ixml.ERROR_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "driver not loaded with no pci devices", ret: ixml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "driver not loaded with pci devices present", ret: ixml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "no permission with noPciCheck", ret: ixml.ERROR_NO_PERMISSION, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "uninitialized with noPciCheck", ret: ixml.ERROR_UNINITIALIZED, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "timeout with pci devices", ret: ixml.ERROR_TIMEOUT, noPciCheck: false, pciDevCount: 1, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize IXML", tc.ret, tc.noPciCheck, tc.pciDevCount)
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
		ret  ixml.Return
		want bool
	}{
		{name: "GPU_IS_LOST", ret: ixml.ERROR_GPU_IS_LOST, want: true},
		{name: "UNKNOWN", ret: ixml.ERROR_UNKNOWN, want: true},
		{name: "RESET_REQUIRED", ret: ixml.ERROR_RESET_REQUIRED, want: true},
		{name: "INSUFFICIENT_POWER", ret: ixml.ERROR_INSUFFICIENT_POWER, want: true},
		{name: "IRQ_ISSUE", ret: ixml.ERROR_IRQ_ISSUE, want: true},
		{name: "SUCCESS", ret: ixml.SUCCESS, want: false},
		{name: "NO_PERMISSION", ret: ixml.ERROR_NO_PERMISSION, want: false},
		{name: "UNINITIALIZED", ret: ixml.ERROR_UNINITIALIZED, want: false},
		{name: "TIMEOUT", ret: ixml.ERROR_TIMEOUT, want: false},
		{name: "NOT_SUPPORTED", ret: ixml.ERROR_NOT_SUPPORTED, want: false},
		{name: "DRIVER_NOT_LOADED", ret: ixml.ERROR_DRIVER_NOT_LOADED, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: ixml.ERROR_LIBRARY_NOT_FOUND, want: false},
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
		v2Ret ixml.Return
		v1Ret ixml.Return
		want  ixml.Return
	}{
		{name: "v1 succeeds", v2Ret: ixml.ERROR_NOT_SUPPORTED, v1Ret: ixml.SUCCESS, want: ixml.SUCCESS},
		{name: "v2 unsupported uses v1 error", v2Ret: ixml.ERROR_NOT_SUPPORTED, v1Ret: ixml.ERROR_GPU_IS_LOST, want: ixml.ERROR_GPU_IS_LOST},
		{name: "v2 function missing uses v1 error", v2Ret: ixml.ERROR_FUNCTION_NOT_FOUND, v1Ret: ixml.ERROR_NO_PERMISSION, want: ixml.ERROR_NO_PERMISSION},
		{name: "v2 lost preserved when v1 missing", v2Ret: ixml.ERROR_GPU_IS_LOST, v1Ret: ixml.ERROR_FUNCTION_NOT_FOUND, want: ixml.ERROR_GPU_IS_LOST},
		{name: "v2 no permission preserved when v1 missing", v2Ret: ixml.ERROR_NO_PERMISSION, v1Ret: ixml.ERROR_FUNCTION_NOT_FOUND, want: ixml.ERROR_NO_PERMISSION},
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
		ret          ixml.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "GPU_IS_LOST skips and records last error", ret: ixml.ERROR_GPU_IS_LOST, wantFatalErr: false, wantLastErr: true},
		{name: "RESET_REQUIRED skips and records last error", ret: ixml.ERROR_RESET_REQUIRED, wantFatalErr: false, wantLastErr: true},
		{name: "NO_PERMISSION fails pass immediately", ret: ixml.ERROR_NO_PERMISSION, wantFatalErr: true, wantLastErr: false},
		{name: "TIMEOUT fails pass immediately", ret: ixml.ERROR_TIMEOUT, wantFatalErr: true, wantLastErr: false},
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
