// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package ixml

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding"
)

// The table pins both halves of the predicate: every code that means the call could not be made,
// and the near-misses that must not be mistaken for one.
//
// ERROR_ARGUMENT_VERSION_MISMATCH is the deliberate exclusion: the entry point is there and a
// caller can retry at a struct version the driver knows, so it is not an absent API.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", ERROR_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", ERROR_FUNCTION_NOT_FOUND, true},
		{"no driver is loaded", ERROR_DRIVER_NOT_LOADED, true},
		{"the library and the driver disagree on version", ERROR_LIB_RM_VERSION_MISMATCH, true},

		{"success", SUCCESS, false},
		{"the device does not support the feature", ERROR_NOT_SUPPORTED, false},
		{"the struct version is unsupported", ERROR_ARGUMENT_VERSION_MISMATCH, false},
		{"the library is not initialized", ERROR_UNINITIALIZED, false},
		{"permission is denied", ERROR_NO_PERMISSION, false},
		{"the queried object was not found", ERROR_NOT_FOUND, false},
		{"the gpu is lost", ERROR_GPU_IS_LOST, false},
		{"the failure is unknown", ERROR_UNKNOWN, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}

type fakeLibrary struct {
	loadErr error
	symbols map[string]error
}

func (f *fakeLibrary) Load() error   { return f.loadErr }
func (f *fakeLibrary) Unload() error { return nil }
func (f *fakeLibrary) Lookup(sym string) error {
	if err, ok := f.symbols[sym]; ok {
		return err
	}
	return errors.New("symbol not found")
}
func (f *fakeLibrary) Path() string { return "fake.so" }

const testIXMLFixtureC = `
#ifdef DEFINE_INIT
int nvmlInit_v2(void) {
	return INIT_RET;
}
#endif

#ifdef DEFINE_CUDA_V1
int nvmlSystemGetCudaDriverVersion(int *version) {
	if (version) {
		*version = CUDA_V1_VAL;
	}
	return CUDA_V1_RET;
}
#endif

#ifdef DEFINE_CUDA_V2
int nvmlSystemGetCudaDriverVersion_v2(int *version) {
	if (version) {
		*version = CUDA_V2_VAL;
	}
	return CUDA_V2_RET;
}
#endif
`

func compileIXMLFixture(t *testing.T, cflags ...string) string {
	t.Helper()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "fixture.c")
	soPath := filepath.Join(dir, "libixml_test.so")
	require.NoError(t, os.WriteFile(srcPath, []byte(testIXMLFixtureC), 0o600))

	args := make([]string, 0, len(cflags)+5)
	if runtime.GOOS == "darwin" {
		args = append(args, "-dynamiclib")
	} else {
		args = append(args, "-shared", "-fPIC")
	}
	args = append(args, cflags...)
	args = append(args, srcPath, "-o", soPath)

	output, err := exec.Command("cc", args...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return soPath
}

func TestIXML_Init(t *testing.T) {
	if soPath := os.Getenv("GPUSTACK_TEST_IXML_INIT_SO"); soPath != "" {
		so := binding.NewLibrary([]string{soPath})
		lib := &IXML{so: so}
		wantRet, err := strconv.Atoi(os.Getenv("GPUSTACK_TEST_IXML_WANT_RET"))
		require.NoError(t, err)
		assert.Equal(t, Return(wantRet), lib.Init())
		return
	}

	fakeCases := []struct {
		name string
		so   binding.Library
		want Return
	}{
		{
			name: "library load failure returns ERROR_LIBRARY_NOT_FOUND",
			so:   &fakeLibrary{loadErr: errors.New("missing")},
			want: ERROR_LIBRARY_NOT_FOUND,
		},
		{
			name: "missing nvmlInit_v2 returns ERROR_FUNCTION_NOT_FOUND",
			so:   &fakeLibrary{},
			want: ERROR_FUNCTION_NOT_FOUND,
		},
	}
	for _, tc := range fakeCases {
		t.Run(tc.name, func(t *testing.T) {
			lib := &IXML{so: tc.so}
			assert.Equal(t, tc.want, lib.Init())
		})
	}

	soCases := []struct {
		name   string
		cflags []string
		want   Return
	}{
		{
			name:   "invokes nvmlInit_v2 when present and returns SUCCESS",
			cflags: []string{"-DDEFINE_INIT", "-DINIT_RET=0"},
			want:   SUCCESS,
		},
		{
			name:   "invokes nvmlInit_v2 when present and propagates non-zero status",
			cflags: []string{"-DDEFINE_INIT", "-DINIT_RET=1"},
			want:   ERROR_UNINITIALIZED,
		},
		{
			name:   "returns ERROR_FUNCTION_NOT_FOUND when nvmlInit_v2 is absent from shared object",
			cflags: nil,
			want:   ERROR_FUNCTION_NOT_FOUND,
		},
	}
	for _, tc := range soCases {
		t.Run(tc.name, func(t *testing.T) {
			soPath := compileIXMLFixture(t, tc.cflags...)
			cmd := exec.Command(os.Args[0], "-test.run=^TestIXML_Init$")
			cmd.Env = append(os.Environ(),
				"GPUSTACK_TEST_IXML_INIT_SO="+soPath,
				"GPUSTACK_TEST_IXML_WANT_RET="+strconv.Itoa(int(tc.want)),
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

func TestIXML_SystemGetCudaDriverVersion(t *testing.T) {
	if soPath := os.Getenv("GPUSTACK_TEST_IXML_CUDA_SO"); soPath != "" {
		so := binding.NewLibrary([]string{soPath})
		require.NoError(t, so.Load())
		lib := &IXML{so: so}

		wantVer, err := strconv.Atoi(os.Getenv("GPUSTACK_TEST_IXML_WANT_VER"))
		require.NoError(t, err)
		wantRet, err := strconv.Atoi(os.Getenv("GPUSTACK_TEST_IXML_WANT_RET"))
		require.NoError(t, err)

		gotVer, gotRet := lib.SystemGetCudaDriverVersion()
		assert.Equal(t, int32(wantVer), gotVer)
		assert.Equal(t, Return(wantRet), gotRet)
		return
	}

	t.Run("fake library with neither symbol returns ERROR_FUNCTION_NOT_FOUND", func(t *testing.T) {
		lib := &IXML{so: &fakeLibrary{}}
		ver, ret := lib.SystemGetCudaDriverVersion()
		assert.Equal(t, int32(0), ver)
		assert.Equal(t, ERROR_FUNCTION_NOT_FOUND, ret)
	})

	soCases := []struct {
		name    string
		cflags  []string
		wantVer int32
		wantRet Return
	}{
		{
			name: "prefers nvmlSystemGetCudaDriverVersion when both symbols exist",
			cflags: []string{
				"-DDEFINE_CUDA_V1", "-DCUDA_V1_VAL=12040", "-DCUDA_V1_RET=0",
				"-DDEFINE_CUDA_V2", "-DCUDA_V2_VAL=12080", "-DCUDA_V2_RET=0",
			},
			wantVer: 12040,
			wantRet: SUCCESS,
		},
		{
			name: "falls back to nvmlSystemGetCudaDriverVersion_v2 when v1 is absent",
			cflags: []string{
				"-DDEFINE_CUDA_V2", "-DCUDA_V2_VAL=12080", "-DCUDA_V2_RET=0",
			},
			wantVer: 12080,
			wantRet: SUCCESS,
		},
		{
			name: "returns v1 error without falling back to v2 when v1 is present",
			cflags: []string{
				"-DDEFINE_CUDA_V1", "-DCUDA_V1_VAL=0", "-DCUDA_V1_RET=3",
				"-DDEFINE_CUDA_V2", "-DCUDA_V2_VAL=12080", "-DCUDA_V2_RET=0",
			},
			wantVer: 0,
			wantRet: ERROR_NOT_SUPPORTED,
		},
		{
			name:    "returns ERROR_FUNCTION_NOT_FOUND when neither symbol is present",
			cflags:  nil,
			wantVer: 0,
			wantRet: ERROR_FUNCTION_NOT_FOUND,
		},
	}
	for _, tc := range soCases {
		t.Run(tc.name, func(t *testing.T) {
			soPath := compileIXMLFixture(t, tc.cflags...)
			cmd := exec.Command(os.Args[0], "-test.run=^TestIXML_SystemGetCudaDriverVersion$")
			cmd.Env = append(os.Environ(),
				"GPUSTACK_TEST_IXML_CUDA_SO="+soPath,
				"GPUSTACK_TEST_IXML_WANT_VER="+strconv.Itoa(int(tc.wantVer)),
				"GPUSTACK_TEST_IXML_WANT_RET="+strconv.Itoa(int(tc.wantRet)),
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}
