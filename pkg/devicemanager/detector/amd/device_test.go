package amd

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

	"gpustack.ai/gpustack/binding/amdsmi"
	"gpustack.ai/gpustack/pkg/device"
)

// TestIsUnavailableReading pins which driver readings are the vendor's "unavailable" answer and
// which are measurements, because the two are told apart by value alone: the call that produced
// either one succeeded.
//
// The cases are written as the two field widths that reach it, not as bare numbers, because the
// width is the whole reason one constant serves both. GpuMetrics reports utilization and
// temperature in uint16, where the sentinel is that type's own maximum; PowerInfo reports wattage in
// uint32, and the driver writes the same 65535 into the wider field rather than that type's
// maximum.
func TestIsUnavailableReading(t *testing.T) {
	cases := []struct {
		name  string
		value uint32
		want  bool
	}{
		{
			name:  "a uint16 metrics field the driver did not fill",
			value: uint32(^uint16(0)),
			want:  true,
		},
		{
			// What an SR-IOV virtual function answers for power: it exposes no host-side telemetry
			// to its guest, and reports this instead of failing the call.
			name:  "a uint32 power field the driver wrote the sentinel into",
			value: 0xFFFF,
			want:  true,
		},
		{
			// Deliberately not caught. AMD's own binding compares the whole power block against
			// 65535, and widening this to the uint32 maximum would stop catching the reading that
			// is actually reported. A field whose maximum this is has no known sentinel here.
			name:  "a uint32 field at its own maximum, which is not the convention",
			value: ^uint32(0),
			want:  false,
		},
		{
			name:  "an ordinary reading",
			value: 42,
			want:  false,
		},
		{
			// The value an unavailable reading is reported as, so it must not read back as one:
			// otherwise a real idle card and an unreadable one become the same answer twice over.
			name:  "an idle reading of zero",
			value: 0,
			want:  false,
		},
		{
			name:  "the reading just below the sentinel",
			value: 0xFFFE,
			want:  false,
		},
		{
			name:  "the reading just above the sentinel",
			value: 0x10000,
			want:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isUnavailableReading(c.value))
		})
	}
}

// TestEvaluateDeviceUnhealthy pins the AMD health rule: an uncorrectable UMC ECC error makes the
// card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name     string
		eccCount amdsmi.ErrorCount
		ret      amdsmi.Return
		want     bool
	}{
		{
			name:     "no uncorrectable ECC error is healthy",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 0},
			ret:      amdsmi.STATUS_SUCCESS,
			want:     false,
		},
		{
			name:     "an uncorrectable ECC error is unhealthy",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 3},
			ret:      amdsmi.STATUS_SUCCESS,
			want:     true,
		},
		{
			name:     "an unsupported ECC query is healthy even with a nonzero count",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 3},
			ret:      amdsmi.STATUS_NOT_SUPPORTED,
			want:     false,
		},
		{
			name:     "an ECC query whose driver could not be loaded is healthy even with a nonzero count",
			eccCount: amdsmi.ErrorCount{Uncorrectable_count: 3},
			ret:      amdsmi.STATUS_FAIL_LOAD_MODULE,
			want:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccCount, tc.ret))
		})
	}
}

const amdsmiFixtureSource = `
#include "amdsmi.h"
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

static unsigned int device_index(amdsmi_processor_handle handle) {
    return (unsigned int)((uintptr_t)handle - 1U);
}

static amdsmi_status_t maybe_fail_device(const char *stage, unsigned int idx) {
    if (strcmp(fixture_env("GPUSTACK_TEST_AMDSMI_FAIL_STAGE"), stage) != 0)
        return AMDSMI_STATUS_SUCCESS;
    const char *fail_idx = fixture_env("GPUSTACK_TEST_AMDSMI_FAIL_INDEX");
    if (fail_idx[0] == '\0')
        return AMDSMI_STATUS_SUCCESS;
    if (strcmp(fail_idx, "all") != 0 && (unsigned int)strtoul(fail_idx, NULL, 10) != idx)
        return AMDSMI_STATUS_SUCCESS;
    return (amdsmi_status_t)atoi(fixture_env("GPUSTACK_TEST_AMDSMI_FAIL_ERR"));
}

amdsmi_status_t amdsmi_init(uint64_t init_flags) {
    (void)init_flags;
    const char *err_env = fixture_env("GPUSTACK_TEST_AMDSMI_INIT_ERR");
    if (err_env[0] != '\0') {
        const char *fail_cnt_env = fixture_env("GPUSTACK_TEST_AMDSMI_INIT_FAIL_COUNT");
        int fail_cnt = fail_cnt_env[0] == '\0' ? -1 : atoi(fail_cnt_env);
        if (fail_cnt < 0 || init_calls < fail_cnt) {
            init_calls++;
            return (amdsmi_status_t)atoi(err_env);
        }
    }
    init_calls++;
    is_initialized = 1;
    return AMDSMI_STATUS_SUCCESS;
}

amdsmi_status_t amdsmi_get_socket_handles(uint32_t *socket_count, amdsmi_socket_handle *socket_handles) {
    if (strcmp(fixture_env("GPUSTACK_TEST_AMDSMI_REQUIRE_INIT"), "true") == 0 && !is_initialized)
        return AMDSMI_STATUS_NOT_INIT;
    const char *err_env = fixture_env("GPUSTACK_TEST_AMDSMI_COUNT_ERR");
    if (err_env[0] != '\0')
        return (amdsmi_status_t)atoi(err_env);
    *socket_count = 1;
    if (socket_handles != NULL)
        socket_handles[0] = (amdsmi_socket_handle)(uintptr_t)1U;
    return AMDSMI_STATUS_SUCCESS;
}

amdsmi_status_t amdsmi_get_processor_handles(
    amdsmi_socket_handle socket_handle,
    uint32_t *processor_count,
    amdsmi_processor_handle *processor_handles
) {
    (void)socket_handle;
    const char *cnt_env = fixture_env("GPUSTACK_TEST_AMDSMI_COUNT");
    uint32_t cnt = cnt_env[0] == '\0' ? 1U : (uint32_t)strtoul(cnt_env, NULL, 10);
    *processor_count = cnt;
    if (processor_handles != NULL) {
        for (uint32_t i = 0; i < cnt; i++) {
            processor_handles[i] = (amdsmi_processor_handle)(uintptr_t)(i + 1U);
        }
    }
    return AMDSMI_STATUS_SUCCESS;
}

amdsmi_status_t amdsmi_get_processor_handle_from_bdf(amdsmi_bdf_t bdf, amdsmi_processor_handle *processor_handle) {
    (void)bdf;
    if (processor_handle != NULL)
        *processor_handle = (amdsmi_processor_handle)(uintptr_t)1U;
    return AMDSMI_STATUS_SUCCESS;
}

amdsmi_status_t amdsmi_get_gpu_asic_info(amdsmi_processor_handle processor_handle, amdsmi_asic_info_t *info) {
    unsigned int idx = device_index(processor_handle);
    amdsmi_status_t ret = maybe_fail_device("asic", idx);
    if (ret != AMDSMI_STATUS_SUCCESS)
        return ret;
    memset(info, 0, sizeof(*info));
    snprintf(info->market_name, sizeof(info->market_name), "AMD Instinct Fixture");
    snprintf(info->asic_serial, sizeof(info->asic_serial), "0x%08x", idx + 1U);
    return AMDSMI_STATUS_SUCCESS;
}

amdsmi_status_t amdsmi_get_gpu_device_bdf(amdsmi_processor_handle processor_handle, amdsmi_bdf_t *bdf) {
    unsigned int idx = device_index(processor_handle);
    amdsmi_status_t ret = maybe_fail_device("bdf", idx);
    if (ret != AMDSMI_STATUS_SUCCESS)
        return ret;
    memset(bdf, 0, sizeof(*bdf));
    uint64_t packed = ((uint64_t)(idx + 1U) << 8);
    memcpy(bdf, &packed, sizeof(packed));
    return AMDSMI_STATUS_SUCCESS;
}

amdsmi_status_t amdsmi_get_gpu_vram_usage(amdsmi_processor_handle processor_handle, amdsmi_vram_usage_t *info) {
    unsigned int idx = device_index(processor_handle);
    amdsmi_status_t ret = maybe_fail_device("vram", idx);
    if (ret != AMDSMI_STATUS_SUCCESS)
        return ret;
    memset(info, 0, sizeof(*info));
    info->vram_total = 65536;
    info->vram_used = 1024;
    return AMDSMI_STATUS_SUCCESS;
}
`

func compileAMDSMIFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "fixture_amdsmi.c")
	require.NoError(t, os.WriteFile(src, []byte(amdsmiFixtureSource), 0o600))

	flags := make([]string, 0, 6)
	if runtime.GOOS == "darwin" {
		flags = append(flags, "-dynamiclib")
	} else {
		flags = append(flags, "-shared", "-fPIC")
	}
	flags = append(flags, "-I../../../../binding/amdsmi", src, "-o", filepath.Join(dir, "libamd_smi.so"))
	output, err := exec.Command("cc", flags...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return dir
}

func TestUnqueryableCardAndErrorHandling(t *testing.T) {
	if mode := os.Getenv("GPUSTACK_TEST_AMDSMI_MODE"); mode != "" {
		d := New(device.DetectorOptions{})
		switch mode {
		case "skip_single":
			groups, err := d.DetectAccelerator(true)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Accelerators, 2)
			assert.Equal(t, "GPU-00000001", groups[0].Accelerators[0].ID)
			assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			assert.Equal(t, "GPU-00000003", groups[0].Accelerators[1].ID)
			assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

			metrics, err := d.MonitorAccelerator(true)
			require.NoError(t, err)
			require.Len(t, metrics, 1)
			require.Len(t, metrics[0].Accelerators, 2)
			assert.Equal(t, "GPU-00000001", metrics[0].Accelerators[0].ID)
			assert.Equal(t, "GPU-00000003", metrics[0].Accelerators[1].ID)
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

	dir := compileAMDSMIFixture(t)

	runCase := func(t *testing.T, extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnqueryableCardAndErrorHandling$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"AMD_SMI_LIB_PATH="+dir,
			"LD_LIBRARY_PATH="+dir,
			"DYLD_LIBRARY_PATH="+dir,
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}

	t.Run("skip card 1 on IO error and preserve index 2", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_AMDSMI_MODE=skip_single",
			"GPUSTACK_TEST_AMDSMI_COUNT=3",
			"GPUSTACK_TEST_AMDSMI_FAIL_STAGE=asic",
			"GPUSTACK_TEST_AMDSMI_FAIL_INDEX=1",
			"GPUSTACK_TEST_AMDSMI_FAIL_ERR="+strconv.Itoa(int(amdsmi.STATUS_IO)),
		)
	})

	t.Run("NO_PERM on card fails detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_AMDSMI_MODE=fatal_per_card",
			"GPUSTACK_TEST_AMDSMI_COUNT=2",
			"GPUSTACK_TEST_AMDSMI_FAIL_STAGE=asic",
			"GPUSTACK_TEST_AMDSMI_FAIL_INDEX=1",
			"GPUSTACK_TEST_AMDSMI_FAIL_ERR="+strconv.Itoa(int(amdsmi.STATUS_NO_PERM)),
		)
	})

	t.Run("all cards unqueryable returns error in detect and monitor", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_AMDSMI_MODE=all_unqueryable",
			"GPUSTACK_TEST_AMDSMI_COUNT=2",
			"GPUSTACK_TEST_AMDSMI_FAIL_STAGE=vram",
			"GPUSTACK_TEST_AMDSMI_FAIL_INDEX=all",
			"GPUSTACK_TEST_AMDSMI_FAIL_ERR="+strconv.Itoa(int(amdsmi.STATUS_DRM_ERROR)),
		)
	})

	t.Run("GetProcessorHandles NO_PERM returns error", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_AMDSMI_MODE=count_error",
			"GPUSTACK_TEST_AMDSMI_COUNT_ERR="+strconv.Itoa(int(amdsmi.STATUS_NO_PERM)),
		)
	})

	t.Run("transient Init NOT_INIT recovers on next DetectAccelerator", func(t *testing.T) {
		runCase(t,
			"GPUSTACK_TEST_AMDSMI_MODE=transient_init_error",
			"GPUSTACK_TEST_AMDSMI_COUNT=1",
			"GPUSTACK_TEST_AMDSMI_REQUIRE_INIT=true",
			"GPUSTACK_TEST_AMDSMI_INIT_FAIL_COUNT=1",
			"GPUSTACK_TEST_AMDSMI_INIT_ERR="+strconv.Itoa(int(amdsmi.STATUS_NOT_INIT)),
		)
	})
}

func TestClassifyInitOrCountError(t *testing.T) {
	testCases := []struct {
		name        string
		ret         amdsmi.Return
		noPciCheck  bool
		pciDevCount int
		wantErr     bool
	}{
		{name: "success", ret: amdsmi.STATUS_SUCCESS, noPciCheck: false, pciDevCount: 1, wantErr: false},
		{name: "library not found with noPciCheck", ret: amdsmi.STATUS_LIBRARY_NOT_FOUND, noPciCheck: true, pciDevCount: 1, wantErr: false},
		{name: "driver not loaded with no pci devices", ret: amdsmi.STATUS_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 0, wantErr: false},
		{name: "driver not loaded with pci devices present", ret: amdsmi.STATUS_DRIVER_NOT_LOADED, noPciCheck: false, pciDevCount: 2, wantErr: true},
		{name: "no perm with noPciCheck", ret: amdsmi.STATUS_NO_PERM, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "not init with noPciCheck", ret: amdsmi.STATUS_NOT_INIT, noPciCheck: true, pciDevCount: 0, wantErr: true},
		{name: "timeout with pci devices", ret: amdsmi.STATUS_TIMEOUT, noPciCheck: false, pciDevCount: 1, wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyInitOrCountError("initialize AMDSMI", tc.ret, tc.noPciCheck, tc.pciDevCount)
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
		ret  amdsmi.Return
		want bool
	}{
		{name: "DRM_ERROR", ret: amdsmi.STATUS_DRM_ERROR, want: true},
		{name: "API_FAILED", ret: amdsmi.STATUS_API_FAILED, want: true},
		{name: "IO", ret: amdsmi.STATUS_IO, want: true},
		{name: "FILE_ERROR", ret: amdsmi.STATUS_FILE_ERROR, want: true},
		{name: "FILE_NOT_FOUND", ret: amdsmi.STATUS_FILE_NOT_FOUND, want: true},
		{name: "DIRECTORY_NOT_FOUND", ret: amdsmi.STATUS_DIRECTORY_NOT_FOUND, want: true},
		{name: "BUSY", ret: amdsmi.STATUS_BUSY, want: true},
		{name: "NOT_FOUND", ret: amdsmi.STATUS_NOT_FOUND, want: true},
		{name: "NO_DATA", ret: amdsmi.STATUS_NO_DATA, want: true},
		{name: "AMDGPU_RESTART_ERR", ret: amdsmi.STATUS_AMDGPU_RESTART_ERR, want: true},
		{name: "CORRUPTED_EEPROM", ret: amdsmi.STATUS_CORRUPTED_EEPROM, want: true},
		{name: "INTERNAL_EXCEPTION", ret: amdsmi.STATUS_INTERNAL_EXCEPTION, want: true},
		{name: "IPC_ERROR", ret: amdsmi.STATUS_IPC_ERROR, want: true},
		{name: "SUCCESS", ret: amdsmi.STATUS_SUCCESS, want: false},
		{name: "NO_PERM", ret: amdsmi.STATUS_NO_PERM, want: false},
		{name: "NOT_INIT", ret: amdsmi.STATUS_NOT_INIT, want: false},
		{name: "INIT_ERROR", ret: amdsmi.STATUS_INIT_ERROR, want: false},
		{name: "INVAL", ret: amdsmi.STATUS_INVAL, want: false},
		{name: "TIMEOUT", ret: amdsmi.STATUS_TIMEOUT, want: false},
		{name: "DRIVER_NOT_LOADED", ret: amdsmi.STATUS_DRIVER_NOT_LOADED, want: false},
		{name: "LIBRARY_NOT_FOUND", ret: amdsmi.STATUS_LIBRARY_NOT_FOUND, want: false},
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
		ret          amdsmi.Return
		wantFatalErr bool
		wantLastErr  bool
	}{
		{name: "IO skips and records last error", ret: amdsmi.STATUS_IO, wantFatalErr: false, wantLastErr: true},
		{name: "AMDGPU_RESTART_ERR skips and records last error", ret: amdsmi.STATUS_AMDGPU_RESTART_ERR, wantFatalErr: false, wantLastErr: true},
		{name: "NO_PERM fails pass immediately", ret: amdsmi.STATUS_NO_PERM, wantFatalErr: true, wantLastErr: false},
		{name: "NOT_INIT fails pass immediately", ret: amdsmi.STATUS_NOT_INIT, wantFatalErr: true, wantLastErr: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var lastErr error
			err := handleCardQueryError(klog.Background(), 1, "get device ASIC info", tc.ret, &lastErr)
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
