// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package hgml

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
