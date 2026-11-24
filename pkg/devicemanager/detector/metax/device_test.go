package metax

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

	"gpustack.ai/gpustack/binding/mxsml"
	"gpustack.ai/gpustack/pkg/device"
)

// TestEvaluateDeviceUnhealthy pins the MetaX health rule: an uncorrectable DRAM ECC error makes
// the card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name      string
		eccErrors mxsml.EccErrorCount
		ret       mxsml.Return
		want      bool
	}{
		{
			name:      "no uncorrectable DRAM ECC error is healthy",
			eccErrors: mxsml.EccErrorCount{DramUE: 0},
			ret:       mxsml.Success,
			want:      false,
		},
		{
			name:      "an uncorrectable DRAM ECC error is unhealthy",
			eccErrors: mxsml.EccErrorCount{DramUE: 3},
			ret:       mxsml.Success,
			want:      true,
		},
		{
			name:      "an unsupported ECC query is healthy even with a nonzero count",
			eccErrors: mxsml.EccErrorCount{DramUE: 3},
			ret:       mxsml.OperationNotSupport,
			want:      false,
		},
		{
			name:      "an ECC query whose driver sysfs could not answer is healthy even with a nonzero count",
			eccErrors: mxsml.EccErrorCount{DramUE: 3},
			ret:       mxsml.SysfsError,
			want:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccErrors, tc.ret))
		})
	}
}

const mxsmlFixtureSource = `
#include "MxSml.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int init_calls = 0;
static int is_initialized = 0;

static const char *fixture_env(const char *name) {
    const char *value = getenv(name);
    return value == NULL ? "" : value;
}

static mxSmlReturn_t maybe_fail_device(const char *stage, unsigned int idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_MXSML_FAIL_STAGE"), stage) != 0)
        return MXSML_Success;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_MXSML_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return MXSML_Success;
    if (strcmp(fail_idx, "all") != 0 && (unsigned int)strtoul(fail_idx, NULL, 10) != idx)
        return MXSML_Success;
    return (mxSmlReturn_t)atoi(fixture_env("GPUSTACK_TEST_MXSML_FAIL_ERR"));
}

mxSmlReturn_t mxSmlInit(void) {
    const char *err_env = fixture_env("GPUSTACK_TEST_MXSML_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_MXSML_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (mxSmlReturn_t)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    return MXSML_Success;
}

unsigned int mxSmlGetDeviceCount(void) {
    if (strcmp(fixture_env("GPUSTACK_TEST_MXSML_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return 0;
    const char *cnt_env = fixture_env("GPUSTACK_TEST_MXSML_COUNT");
    return cnt_env[0] == '\0' ? 1U : (unsigned int)strtoul(cnt_env, NULL, 10);
}

mxSmlReturn_t mxSmlGetDeviceInfo(unsigned int deviceId, mxSmlDeviceInfo_t *deviceInfo) {
    mxSmlReturn_t ret = maybe_fail_device("info", deviceId);
    if (ret != MXSML_Success)
        return ret;
    memset(deviceInfo, 0, sizeof(*deviceInfo));
    deviceInfo->deviceId = deviceId;
    snprintf(deviceInfo->uuid, sizeof(deviceInfo->uuid), "GPU-MX-%u", deviceId);
    snprintf(deviceInfo->bdfId, sizeof(deviceInfo->bdfId), "0000:%02x:00.0", deviceId + 1U);
    snprintf(deviceInfo->deviceName, sizeof(deviceInfo->deviceName), "MetaX C500");
    if (strcmp(fixture_env("GPUSTACK_TEST_MXSML_ALL_VF"), "true") == 0) {
        deviceInfo->mode = MXSML_Virtualization_Mode_Vf;
    } else {
        deviceInfo->mode = MXSML_Virtualization_Mode_None;
    }
    return MXSML_Success;
}

mxSmlReturn_t mxSmlGetMemoryInfo(unsigned int deviceId, mxSmlMemoryInfo_t *memoryInfo) {
    mxSmlReturn_t ret = maybe_fail_device("memory", deviceId);
    if (ret != MXSML_Success)
        return ret;
    memset(memoryInfo, 0, sizeof(*memoryInfo));
    memoryInfo->vramTotal = 65536L * 1024L;
    memoryInfo->vramUse = 1024L * 1024L;
    return MXSML_Success;
}
`

func compileMXSMLFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "fixture_mxsml.c")
	require.NoError(t, os.WriteFile(src, []byte(mxsmlFixtureSource), 0o600))

	flags := make([]string, 0, 6)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-I../../../../binding/mxsml", src, "-o", filepath.Join(dir, "libmxsml.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_MXSML_MODE"); mode != "" {
		d := New(device.DetectorOptions{})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "GPU-MX-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "GPU-MX-2", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "GPU-MX-0", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "GPU-MX-2", metrics[0].Accelerators[1].ID)
		case "intentional_skip_vf", "init_no_device":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, metrics)
		case "fatal_per_card", "all_unqueryable", "init_error":
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

	dir := compileMXSMLFixture(t)

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

	t.Run("skip GPU-1 on FlrNotReady and preserve index 2", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MXSML_MODE=skip_single",
			"GPUSTACK_TEST_MXSML_COUNT=3",
			"GPUSTACK_TEST_MXSML_FAIL_STAGE=info",
			"GPUSTACK_TEST_MXSML_FAIL_INDEX=1",
			"GPUSTACK_TEST_MXSML_FAIL_ERR="+strconv.Itoa(int(mxsml.FlrNotReady)),
		)
	})

	t.Run("all VF devices skipped intentionally returns nil nil", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MXSML_MODE=intentional_skip_vf",
			"GPUSTACK_TEST_MXSML_COUNT=2",
			"GPUSTACK_TEST_MXSML_ALL_VF=true",
		)
	})

	t.Run("PermissionDenied on card fails detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MXSML_MODE=fatal_per_card",
			"GPUSTACK_TEST_MXSML_COUNT=2",
			"GPUSTACK_TEST_MXSML_FAIL_STAGE=info",
			"GPUSTACK_TEST_MXSML_FAIL_INDEX=1",
			"GPUSTACK_TEST_MXSML_FAIL_ERR="+strconv.Itoa(int(mxsml.PermissionDenied)),
		)
	})

	t.Run("all cards unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MXSML_MODE=all_unqueryable",
			"GPUSTACK_TEST_MXSML_COUNT=2",
			"GPUSTACK_TEST_MXSML_FAIL_STAGE=memory",
			"GPUSTACK_TEST_MXSML_FAIL_INDEX=all",
			"GPUSTACK_TEST_MXSML_FAIL_ERR="+strconv.Itoa(int(mxsml.IOControlFailure)),
		)
	})

	t.Run("Init PermissionDenied fails before bare-int GetDeviceCount", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MXSML_MODE=init_error",
			"GPUSTACK_TEST_MXSML_COUNT=0",
			"GPUSTACK_TEST_MXSML_INIT_ERR="+strconv.Itoa(int(mxsml.PermissionDenied)),
		)
	})

	t.Run("Init NoDevice with noPciCheck returns nil nil", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MXSML_MODE=init_no_device",
			"GPUSTACK_TEST_MXSML_INIT_ERR="+strconv.Itoa(int(mxsml.NoDevice)),
		)
	})

	t.Run("transient Init Failure recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MXSML_MODE=transient_init_error",
			"GPUSTACK_TEST_MXSML_COUNT=1",
			"GPUSTACK_TEST_MXSML_REQUIRE_INIT=true",
			"GPUSTACK_TEST_MXSML_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_MXSML_INIT_ERR="+strconv.Itoa(int(mxsml.Failure)),
		)
	})
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         mxsml.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: mxsml.Success, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: mxsml.LibraryNotFound, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "load dll failure with no pci devices", ret: mxsml.LoadDllFailure, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "no device with noPciCheck", ret: mxsml.NoDevice, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "no device with pci devices present", ret: mxsml.NoDevice, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "library not found with pci devices present", ret: mxsml.LibraryNotFound, noPciCheck: false, pciDevCount: 1, wantErr: true},
		{name: "permission denied with noPciCheck", ret: mxsml.PermissionDenied, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "failure with noPciCheck", ret: mxsml.Failure, noPciCheck: true, pciDevCount: 0, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize MXSML", tc.ret, tc.noPciCheck, tc.pciDevCount)
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
		ret  mxsml.Return
		want bool
	}{
		{name: "Failure", ret: mxsml.Failure, want: true},
		{name: "NoDevice", ret: mxsml.NoDevice, want: true},
		{name: "SysfsError", ret: mxsml.SysfsError, want: true},
		{name: "InvalidDeviceId", ret: mxsml.InvalidDeviceId, want: true},
		{name: "InvalidDieId", ret: mxsml.InvalidDieId, want: true},
		{name: "IOControlFailure", ret: mxsml.IOControlFailure, want: true},
		{name: "FlrNotReady", ret: mxsml.FlrNotReady, want: true},
		{name: "OpenDeviceFileFailure", ret: mxsml.OpenDeviceFileFailure, want: true},
		{name: "CloseDeviceFileFailure", ret: mxsml.CloseDeviceFileFailure, want: true},
		{name: "BusyDevice", ret: mxsml.BusyDevice, want: true},
		{name: "GetPciBridgeFailure", ret: mxsml.GetPciBridgeFailure, want: true},
		{name: "Success", ret: mxsml.Success, want: false},
		{name: "PermissionDenied", ret: mxsml.PermissionDenied, want: false},
		{name: "InvalidInput", ret: mxsml.InvalidInput, want: false},
		{name: "OperationNotSupport", ret: mxsml.OperationNotSupport, want: false},
		{name: "LibraryNotFound", ret: mxsml.LibraryNotFound, want: false},
		{name: "FunctionNotFound", ret: mxsml.FunctionNotFound, want: false},
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
		ret          mxsml.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "FlrNotReady skips and records last error", ret: mxsml.FlrNotReady, wantFatalErr: false, wantLastErr: true},
		{name: "IOControlFailure skips and records last error", ret: mxsml.IOControlFailure, wantFatalErr: false, wantLastErr: true},
		{name: "PermissionDenied fails pass immediately", ret: mxsml.PermissionDenied, wantFatalErr: true, wantLastErr: false},
		{name: "InvalidInput fails pass immediately", ret: mxsml.InvalidInput, wantFatalErr: true, wantLastErr: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var lastErr error
			err := handleCardQueryError(klog.Background(), 1, "get device info", tc.ret, &lastErr)
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
