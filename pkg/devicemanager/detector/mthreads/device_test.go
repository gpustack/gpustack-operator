package mthreads

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

	"gpustack.ai/gpustack/binding/mtml"
	"gpustack.ai/gpustack/pkg/device"
)

// TestEvaluateDeviceUnhealthy pins the MThreads health rule: an uncorrectable ECC error makes the
// card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name      string
		eccErrors uint64
		ret       mtml.Return
		want      bool
	}{
		{
			name:      "no uncorrectable ECC error is healthy",
			eccErrors: 0,
			ret:       mtml.SUCCESS,
			want:      false,
		},
		{
			name:      "an uncorrectable ECC error is unhealthy",
			eccErrors: 3,
			ret:       mtml.SUCCESS,
			want:      true,
		},
		{
			name:      "an unsupported ECC query is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       mtml.ERROR_NOT_SUPPORTED,
			want:      false,
		},
		{
			name:      "an ECC query whose driver is not loaded is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       mtml.ERROR_DRIVER_NOT_LOADED,
			want:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccErrors, tc.ret))
		})
	}
}

const mtmlFixtureSource = `
#include "mtml.h"
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int init_calls = 0;
static int is_initialized = 0;
static int fake_lib_obj = 0;
static int fake_dev_objs[16];
static int fake_mem_objs[16];
static int fake_gpu_objs[16];

static const char *fixture_env(const char *name) {
    const char *value = getenv(name);
    return value == NULL ? "" : value;
}

static unsigned int device_index(const MtmlDevice *dev) {
    return (unsigned int)((const int *)dev - fake_dev_objs);
}

static MtmlReturn maybe_fail_device(const char *stage, unsigned int idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_MTML_FAIL_STAGE"), stage) != 0)
        return MTML_SUCCESS;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_MTML_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return MTML_SUCCESS;
    if (strcmp(fail_idx, "all") != 0 && (unsigned int)strtoul(fail_idx, NULL, 10) != idx)
        return MTML_SUCCESS;
    return (MtmlReturn)atoi(fixture_env("GPUSTACK_TEST_MTML_FAIL_ERR"));
}

MtmlReturn mtmlLibraryInit(MtmlLibrary **lib) {
    const char *err_env = fixture_env("GPUSTACK_TEST_MTML_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_MTML_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (MtmlReturn)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    *lib = (MtmlLibrary *)&fake_lib_obj;
    return MTML_SUCCESS;
}

MtmlReturn mtmlLibraryCountDevice(const MtmlLibrary *lib, unsigned int *count) {
    (void)lib;
    if (strcmp(fixture_env("GPUSTACK_TEST_MTML_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return MTML_ERROR_INVALID_ARGUMENT;
    const char *err_env = fixture_env("GPUSTACK_TEST_MTML_COUNT_ERR");
    if (err_env[0] != '\0')
        return (MtmlReturn)atoi(err_env);
    const char *cnt_env = fixture_env("GPUSTACK_TEST_MTML_COUNT");
    *count = cnt_env[0] == '\0' ? 1U : (unsigned int)strtoul(cnt_env, NULL, 10);
    return MTML_SUCCESS;
}

MtmlReturn mtmlLibraryInitDeviceByIndex(const MtmlLibrary *lib, unsigned int index, MtmlDevice **dev) {
    (void)lib;
    MtmlReturn ret = maybe_fail_device("handle", index);
    if (ret != MTML_SUCCESS)
        return ret;
    *dev = (MtmlDevice *)&fake_dev_objs[index];
    return MTML_SUCCESS;
}

MtmlReturn mtmlLibraryFreeDevice(MtmlDevice *dev) {
    (void)dev;
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceGetProperty(const MtmlDevice *dev, MtmlDeviceProperty *prop) {
    unsigned int idx = device_index(dev);
    MtmlReturn ret = maybe_fail_device("property", idx);
    if (ret != MTML_SUCCESS)
        return ret;
    memset(prop, 0, sizeof(*prop));
    if (strcmp(fixture_env("GPUSTACK_TEST_MTML_ALL_GUEST_VF"), "true") == 0) {
        prop->virtCap = MTML_VIRT_ROLE_GUEST_VIRTDEVICE;
    } else {
        prop->virtCap = MTML_VIRT_ROLE_NONE;
    }
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceGetUUID(const MtmlDevice *dev, char *uuid, unsigned int length) {
    unsigned int idx = device_index(dev);
    MtmlReturn ret = maybe_fail_device("uuid", idx);
    if (ret != MTML_SUCCESS)
        return ret;
    snprintf(uuid, length, "MTT-%u", idx);
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceGetPciInfo(const MtmlDevice *dev, MtmlPciInfo *pci) {
    unsigned int idx = device_index(dev);
    MtmlReturn ret = maybe_fail_device("pci", idx);
    if (ret != MTML_SUCCESS)
        return ret;
    memset(pci, 0, sizeof(*pci));
    pci->segment = 0;
    pci->bus = idx + 1U;
    pci->device = 0;
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceGetName(const MtmlDevice *dev, char *name, unsigned int length) {
    unsigned int idx = device_index(dev);
    MtmlReturn ret = maybe_fail_device("name", idx);
    if (ret != MTML_SUCCESS)
        return ret;
    snprintf(name, length, "MTT S4000");
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceInitMemory(const MtmlDevice *dev, MtmlMemory **mem) {
    unsigned int idx = device_index(dev);
    MtmlReturn ret = maybe_fail_device("memory", idx);
    if (ret != MTML_SUCCESS)
        return ret;
    *mem = (MtmlMemory *)&fake_mem_objs[idx];
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceFreeMemory(MtmlMemory *mem) {
    (void)mem;
    return MTML_SUCCESS;
}

MtmlReturn mtmlMemoryGetTotal(const MtmlMemory *mem, unsigned long long *total) {
    (void)mem;
    *total = 49152ULL << 20;
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceInitGpu(const MtmlDevice *dev, MtmlGpu **gpu) {
    unsigned int idx = device_index(dev);
    MtmlReturn ret = maybe_fail_device("gpu", idx);
    if (ret != MTML_SUCCESS)
        return ret;
    *gpu = (MtmlGpu *)&fake_gpu_objs[idx];
    return MTML_SUCCESS;
}

MtmlReturn mtmlDeviceFreeGpu(MtmlGpu *gpu) {
    (void)gpu;
    return MTML_SUCCESS;
}
`

func compileMTMLFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "fixture_mtml.c")
	require.NoError(t, os.WriteFile(src, []byte(mtmlFixtureSource), 0o600))

	flags := make([]string, 0, 6)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-I../../../../binding/mtml", src, "-o", filepath.Join(dir, "libmtml.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_MTML_MODE"); mode != "" {
		d := New(device.DetectorOptions{})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "MTT-0", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "MTT-2", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "MTT-0", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "MTT-2", metrics[0].Accelerators[1].ID)
		case "intentional_skip_guest_vf":
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
		case "optional_gpu_handler_error":
			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "MTT-0", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "MTT-1", metrics[0].Accelerators[1].ID)
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

	dir := compileMTMLFixture(t)

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

	t.Run("skip MTT-1 on DRIVER_FAILURE and preserve index 2", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MTML_MODE=skip_single",
			"GPUSTACK_TEST_MTML_COUNT=3",
			"GPUSTACK_TEST_MTML_FAIL_STAGE=property",
			"GPUSTACK_TEST_MTML_FAIL_INDEX=1",
			"GPUSTACK_TEST_MTML_FAIL_ERR="+strconv.Itoa(int(mtml.ERROR_DRIVER_FAILURE)),
		)
	})

	t.Run("all guest VF devices skipped intentionally returns nil nil", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MTML_MODE=intentional_skip_guest_vf",
			"GPUSTACK_TEST_MTML_COUNT=2",
			"GPUSTACK_TEST_MTML_ALL_GUEST_VF=true",
		)
	})

	t.Run("NO_PERMISSION on card fails detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MTML_MODE=fatal_per_card",
			"GPUSTACK_TEST_MTML_COUNT=2",
			"GPUSTACK_TEST_MTML_FAIL_STAGE=property",
			"GPUSTACK_TEST_MTML_FAIL_INDEX=1",
			"GPUSTACK_TEST_MTML_FAIL_ERR="+strconv.Itoa(int(mtml.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("all cards unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MTML_MODE=all_unqueryable",
			"GPUSTACK_TEST_MTML_COUNT=2",
			"GPUSTACK_TEST_MTML_FAIL_STAGE=memory",
			"GPUSTACK_TEST_MTML_FAIL_INDEX=all",
			"GPUSTACK_TEST_MTML_FAIL_ERR="+strconv.Itoa(int(mtml.ERROR_DRIVER_FAILURE)),
		)
	})

	t.Run("CountDevice NO_PERMISSION returns error", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MTML_MODE=count_error",
			"GPUSTACK_TEST_MTML_COUNT_ERR="+strconv.Itoa(int(mtml.ERROR_NO_PERMISSION)),
		)
	})

	t.Run("optional InitGpu error degrades cleanly without dropping card or failing monitor", func(t *testing.T) {
		for _, ret := range []mtml.Return{mtml.ERROR_NOT_SUPPORTED, mtml.ERROR_RESOURCE_IS_BUSY} {
			t.Run(ret.String(), func(t *testing.T) {
				runCase(t,
					"GPUSTACK_TEST_MTML_MODE=optional_gpu_handler_error",
					"GPUSTACK_TEST_MTML_COUNT=2",
					"GPUSTACK_TEST_MTML_FAIL_STAGE=gpu",
					"GPUSTACK_TEST_MTML_FAIL_INDEX=1",
					"GPUSTACK_TEST_MTML_FAIL_ERR="+strconv.Itoa(int(ret)),
				)
			})
		}
	})

	t.Run("transient Init failure recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_MTML_MODE=transient_init_error",
			"GPUSTACK_TEST_MTML_COUNT=1",
			"GPUSTACK_TEST_MTML_REQUIRE_INIT=true",
			"GPUSTACK_TEST_MTML_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_MTML_INIT_ERR="+strconv.Itoa(int(mtml.ERROR_DRIVER_FAILURE)),
		)
	})
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         mtml.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: mtml.SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: mtml.ERROR_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "driver not loaded with no pci devices", ret: mtml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "driver not loaded with pci devices present", ret: mtml.ERROR_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "no permission with noPciCheck", ret: mtml.ERROR_NO_PERMISSION, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "driver failure with noPciCheck", ret: mtml.ERROR_DRIVER_FAILURE, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "timeout with pci devices", ret: mtml.ERROR_TIMEOUT, noPciCheck: false, pciDevCount: 1, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize MTML", tc.ret, tc.noPciCheck, tc.pciDevCount)
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
		ret  mtml.Return
		want bool
	}{
		{name: "DRIVER_FAILURE", ret: mtml.ERROR_DRIVER_FAILURE, want: true},
		{name: "NOT_FOUND", ret: mtml.ERROR_NOT_FOUND, want: true},
		{name: "RESOURCE_IS_BUSY", ret: mtml.ERROR_RESOURCE_IS_BUSY, want: true},
		{name: "UNKNOWN", ret: mtml.ERROR_UNKNOWN, want: true},
		{name: "SUCCESS", ret: mtml.SUCCESS, want: false},
		{name: "NO_PERMISSION", ret: mtml.ERROR_NO_PERMISSION, want: false},
		{name: "INVALID_ARGUMENT", ret: mtml.ERROR_INVALID_ARGUMENT, want: false},
		{name: "TIMEOUT", ret: mtml.ERROR_TIMEOUT, want: false},
		{name: "NOT_SUPPORTED", ret: mtml.ERROR_NOT_SUPPORTED, want: false},
		{name: "DRIVER_NOT_LOADED", ret: mtml.ERROR_DRIVER_NOT_LOADED, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: mtml.ERROR_LIBRARY_NOT_FOUND, want: false},
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
		ret          mtml.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "DRIVER_FAILURE skips and records last error", ret: mtml.ERROR_DRIVER_FAILURE, wantFatalErr: false, wantLastErr: true},
		{name: "NOT_FOUND skips and records last error", ret: mtml.ERROR_NOT_FOUND, wantFatalErr: false, wantLastErr: true},
		{name: "NO_PERMISSION fails pass immediately", ret: mtml.ERROR_NO_PERMISSION, wantFatalErr: true, wantLastErr: false},
		{name: "TIMEOUT fails pass immediately", ret: mtml.ERROR_TIMEOUT, wantFatalErr: true, wantLastErr: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var lastErr error
			err := handleCardQueryError(klog.Background(), 1, "get device property", tc.ret, &lastErr)
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
