// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package cndev

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table covers every declared Return: the nineteen the vendor header enumerates plus the two
// sentinels this package returns of its own accord. The distinction the predicate has to hold is
// between a code that says the call could not be made and a code that is itself the driver's answer.
//
// Two rows carry that distinction. ERROR_NOT_SUPPORTED: the driver reached the device and reported it
// lacks the queried feature, which is an answer, so a caller must not read it as a missing API.
// ERROR_UNSUPPORTED_API_VERSION: the header says "Use the correct version number for the API", so
// the entry point is there and a retry at another struct version can succeed — the same reasoning
// that keeps ERROR_ARGUMENT_VERSION_MISMATCH false in the NVML-shaped bindings.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library never loaded", ERROR_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", ERROR_FUNCTION_NOT_FOUND, true},
		{"no driver is installed", ERROR_NO_DRIVER, true},
		{"the driver is too old", ERROR_LOW_DRIVER_VERSION, true},

		{"success", SUCCESS, false},
		{"the struct version is unsupported", ERROR_UNSUPPORTED_API_VERSION, false},
		{"the library is not initialized", ERROR_UNINITIALIZED, false},
		{"the argument is invalid", ERROR_INVALID_ARGUMENT, false},
		{"the device ID is invalid", ERROR_INVALID_DEVICE_ID, false},
		{"the failure is unknown", ERROR_UNKNOWN, false},
		{"the allocation failed", ERROR_MALLOC, false},
		{"the buffer is too small", ERROR_INSUFFICIENT_SPACE, false},
		{"the device does not support the feature", ERROR_NOT_SUPPORTED, false},
		{"the link is invalid", ERROR_INVALID_LINK, false},
		{"no devices are present", ERROR_NO_DEVICES, false},
		{"permission is denied", ERROR_NO_PERMISSION, false},
		{"the queried object was not found", ERROR_NOT_FOUND, false},
		{"the resource is in use", ERROR_IN_USE, false},
		{"the resource already exists", ERROR_DUPLICATE, false},
		{"the call timed out", ERROR_TIMEOUT, false},
		{"the device is in problem", ERROR_IN_PROBLEM, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}
