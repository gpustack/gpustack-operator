// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package cambricon

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

	"gpustack.ai/gpustack/binding/cndev"
	"gpustack.ai/gpustack/pkg/device"
)

func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name  string
		v2    cndev.CardHealthStateV2
		v2Ret cndev.Return
		v1    cndev.CardHealthStateV2
		v1Ret cndev.Return
		want  bool
	}{
		{
			name:  "V2 healthy card",
			v2:    cndev.CardHealthStateV2{Health: 1},
			v2Ret: cndev.SUCCESS,
			want:  false,
		},
		{
			name:  "V2 unhealthy card",
			v2:    cndev.CardHealthStateV2{Health: 0},
			v2Ret: cndev.SUCCESS,
			want:  true,
		},
		{
			name:  "V2 unsupported falls back to V1 healthy",
			v2Ret: cndev.ERROR_NOT_SUPPORTED,
			v1:    cndev.CardHealthStateV2{Health: 1},
			v1Ret: cndev.SUCCESS,
			want:  false,
		},
		{
			name:  "V2 unsupported falls back to V1 unhealthy",
			v2Ret: cndev.ERROR_NOT_SUPPORTED,
			v1:    cndev.CardHealthStateV2{Health: 0},
			v1Ret: cndev.SUCCESS,
			want:  true,
		},
		{
			name:  "both V2 and V1 fail defaults to healthy",
			v2Ret: cndev.ERROR_NOT_SUPPORTED,
			v1Ret: cndev.ERROR_NOT_SUPPORTED,
			want:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateDeviceUnhealthy(
				func() (cndev.CardHealthStateV2, cndev.Return) { return tc.v2, tc.v2Ret },
				func() (cndev.CardHealthStateV2, cndev.Return) { return tc.v1, tc.v1Ret },
			)
			assert.Equal(t, tc.want, got)
		})
	}
}

const cndevFixtureSource = `
#include "cndev.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int init_calls = 0;
static int is_initialized = 0;

static const char *fixture_env(const char *name) {
    const char *value = getenv(name);
    return value == NULL ? "" : value;
}

static cndevRet_t maybe_fail_device(const char *stage, int idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_CNDEV_FAIL_STAGE"), stage) != 0)
        return CNDEV_SUCCESS;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_CNDEV_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return CNDEV_SUCCESS;
    if (strcmp(fail_idx, "all") != 0 && atoi(fail_idx) != idx)
        return CNDEV_SUCCESS;
    return (cndevRet_t)atoi(fixture_env("GPUSTACK_TEST_CNDEV_FAIL_ERR"));
}

cndevRet_t cndevInit(__int32_t reserved) {
    (void)reserved;
    const char *err_env = fixture_env("GPUSTACK_TEST_CNDEV_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_CNDEV_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (cndevRet_t)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    return CNDEV_SUCCESS;
}

cndevRet_t cndevGetDeviceCount(cndevCardInfo_t *cardNum) {
    if (strcmp(fixture_env("GPUSTACK_TEST_CNDEV_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return CNDEV_ERROR_UNINITIALIZED;
    const char *err_env = fixture_env("GPUSTACK_TEST_CNDEV_COUNT_ERR");
    if (err_env[0] != '\0')
        return (cndevRet_t)atoi(err_env);
    const char *cnt_env = fixture_env("GPUSTACK_TEST_CNDEV_COUNT");
    cardNum->number = cnt_env[0] == '\0' ? 1U : (unsigned int)strtoul(cnt_env, NULL, 10);
    return CNDEV_SUCCESS;
}

cndevRet_t cndevGetDeviceHandleByIndex(__int32_t index, cndevDevice_t *handle) {
    cndevRet_t ret = maybe_fail_device("handle", index);
    if (ret != CNDEV_SUCCESS)
        return ret;
    *handle = (cndevDevice_t)index;
    return CNDEV_SUCCESS;
}

cndevRet_t cndevGetUUID(cndevUUID_t *uuidInfo, cndevDevice_t device) {
    cndevRet_t ret = maybe_fail_device("uuid", (int)device);
    if (ret != CNDEV_SUCCESS)
        return ret;
    memset(uuidInfo->uuid, 0, sizeof(uuidInfo->uuid));
    snprintf((char *)uuidInfo->uuid, sizeof(uuidInfo->uuid), "%04d", (int)device);
    return CNDEV_SUCCESS;
}

cndevRet_t cndevGetPCIeInfoV2(cndevPCIeInfoV2_t *deviceInfo, cndevDevice_t device) {
    cndevRet_t ret = maybe_fail_device("pci", (int)device);
    if (ret != CNDEV_SUCCESS)
        return ret;
    memset(deviceInfo, 0, sizeof(*deviceInfo));
    deviceInfo->domain = 0;
    deviceInfo->bus = (unsigned int)device + 1U;
    deviceInfo->device = 0;
    deviceInfo->function = 0;
    return CNDEV_SUCCESS;
}

const char *cndevGetCardNameStringByDevId(cndevDevice_t device) {
    (void)device;
    return "MLU590";
}

cndevRet_t cndevGetMemoryUsageV2(cndevMemoryInfoV2_t *memInfo, cndevDevice_t device) {
    cndevRet_t ret = maybe_fail_device("memory", (int)device);
    if (ret != CNDEV_SUCCESS)
        return ret;
    memset(memInfo, 0, sizeof(*memInfo));
    memInfo->physicalMemoryTotal = 81920;
    memInfo->physicalMemoryUsed = 1024;
    return CNDEV_SUCCESS;
}
`

func compileCNDevFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	libDir := filepath.Join(dir, "lib64")
	require.NoError(t, os.MkdirAll(libDir, 0o755))
	// The vendored header defines CNDEV_EXPORT as static; rewrite a local copy so the
	// fixture exports external symbols while keeping the exact vendored ABI.
	hdr, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "binding", "cndev", "cndev.h"))
	require.NoError(t, err)
	patchedHdr := strings.Replace(string(hdr), "#define CNDEV_EXPORT static", "#define CNDEV_EXPORT", 1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cndev.h"), []byte(patchedHdr), 0o600))

	src := filepath.Join(dir, "fixture_cndev.c")
	require.NoError(t, os.WriteFile(src, []byte(cndevFixtureSource), 0o600))

	flags := make([]string, 0, 7)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-I"+dir, "-I../../../../binding/cndev", src, "-o", filepath.Join(libDir, "libcndev.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_CNDEV_MODE"); mode != "" {
		d := New(device.DetectorOptions{Logger: klog.Background()})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "MLU-0000", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "MLU-0002", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "MLU-0000", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "MLU-0002", metrics[0].Accelerators[1].ID)
		case "fatal_per_card", "all_unqueryable", "count_error":
			groups, err := d.DetectAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.Error(t, err)
			assert.Nil(t, metrics)
		case "init_no_devices":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			assert.Nil(t, groups)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
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

	dir := compileCNDevFixture(t)

	runCase := func(t *testing.T, extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnqueryableCardAndErrorHandling$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"NEUWARE_HOME="+dir,
			"LD_LIBRARY_PATH="+filepath.Join(dir, "lib64"),
			"DYLD_LIBRARY_PATH="+filepath.Join(dir, "lib64"),
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	t.Run("skip MLU-1 on IN_PROBLEM and preserve index 2", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_CNDEV_MODE=skip_single",
			"GPUSTACK_TEST_CNDEV_COUNT=3",
			"GPUSTACK_TEST_CNDEV_FAIL_STAGE=handle",
			"GPUSTACK_TEST_CNDEV_FAIL_INDEX=1",
			"GPUSTACK_TEST_CNDEV_FAIL_ERR="+strconv.Itoa(int(cndev.ERROR_IN_PROBLEM)),
		)
	})

	t.Run("NO_PERMISSION on card fails detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_CNDEV_MODE=fatal_per_card",
			"GPUSTACK_TEST_CNDEV_COUNT=2",
			"GPUSTACK_TEST_CNDEV_FAIL_STAGE=handle",
			"GPUSTACK_TEST_CNDEV_FAIL_INDEX=1",
			"GPUSTACK_TEST_CNDEV_FAIL_ERR="+strconv.Itoa(int(cndev.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("all MLUs unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_CNDEV_MODE=all_unqueryable",
			"GPUSTACK_TEST_CNDEV_COUNT=2",
			"GPUSTACK_TEST_CNDEV_FAIL_STAGE=handle",
			"GPUSTACK_TEST_CNDEV_FAIL_INDEX=all",
			"GPUSTACK_TEST_CNDEV_FAIL_ERR="+strconv.Itoa(int(cndev.ERROR_IN_PROBLEM)),
		)
	})

	t.Run("GetDeviceCount NO_PERMISSION returns error", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_CNDEV_MODE=count_error",
			"GPUSTACK_TEST_CNDEV_COUNT_ERR="+strconv.Itoa(int(cndev.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("Init NO_DEVICES with noPciCheck returns nil nil", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_CNDEV_MODE=init_no_devices",
			"GPUSTACK_TEST_CNDEV_INIT_ERR="+strconv.Itoa(int(cndev.ERROR_NO_DEVICES)),
		)
	})

	t.Run("transient Init failure recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_CNDEV_MODE=transient_init_error",
			"GPUSTACK_TEST_CNDEV_COUNT=1",
			"GPUSTACK_TEST_CNDEV_REQUIRE_INIT=true",
			"GPUSTACK_TEST_CNDEV_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_CNDEV_INIT_ERR="+strconv.Itoa(int(cndev.ERROR_UNKNOWN)),
		)
	})
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         cndev.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: cndev.SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: cndev.ERROR_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "no driver with no pci devices", ret: cndev.ERROR_NO_DRIVER, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "no devices with noPciCheck", ret: cndev.ERROR_NO_DEVICES, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "no devices with pci devices present", ret: cndev.ERROR_NO_DEVICES, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "no driver with pci devices present", ret: cndev.ERROR_NO_DRIVER, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "no permission with noPciCheck", ret: cndev.ERROR_NO_PERMISSION, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "uninitialized with noPciCheck", ret: cndev.ERROR_UNINITIALIZED, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "timeout with pci devices", ret: cndev.ERROR_TIMEOUT, noPciCheck: false, pciDevCount: 1, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize CNDev", tc.ret, tc.noPciCheck, tc.pciDevCount)
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
		ret  cndev.Return
		want bool
	}{
		{name: "IN_PROBLEM", ret: cndev.ERROR_IN_PROBLEM, want: true},
		{name: "INVALID_DEVICE_ID", ret: cndev.ERROR_INVALID_DEVICE_ID, want: true},
		{name: "UNKNOWN", ret: cndev.ERROR_UNKNOWN, want: true},
		{name: "INVALID_LINK", ret: cndev.ERROR_INVALID_LINK, want: true},
		{name: "NOT_FOUND", ret: cndev.ERROR_NOT_FOUND, want: true},
		{name: "IN_USE", ret: cndev.ERROR_IN_USE, want: true},
		{name: "SUCCESS", ret: cndev.SUCCESS, want: false},
		{name: "NO_PERMISSION", ret: cndev.ERROR_NO_PERMISSION, want: false},
		{name: "UNINITIALIZED", ret: cndev.ERROR_UNINITIALIZED, want: false},
		{name: "INVALID_ARGUMENT", ret: cndev.ERROR_INVALID_ARGUMENT, want: false},
		{name: "TIMEOUT", ret: cndev.ERROR_TIMEOUT, want: false},
		{name: "NO_DRIVER", ret: cndev.ERROR_NO_DRIVER, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: cndev.ERROR_LIBRARY_NOT_FOUND, want: false},
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
		ret          cndev.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "IN_PROBLEM skips and records last error", ret: cndev.ERROR_IN_PROBLEM, wantFatalErr: false, wantLastErr: true},
		{name: "UNKNOWN skips and records last error", ret: cndev.ERROR_UNKNOWN, wantFatalErr: false, wantLastErr: true},
		{name: "NO_PERMISSION fails pass immediately", ret: cndev.ERROR_NO_PERMISSION, wantFatalErr: true, wantLastErr: false},
		{name: "TIMEOUT fails pass immediately", ret: cndev.ERROR_TIMEOUT, wantFatalErr: true, wantLastErr: false},
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
