// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package mtml

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table pins both halves of the predicate: every code that means the call could not be made,
// and the near-misses that must not be mistaken for one.
//
// Both driver-version codes count: too old and too new state the same fact about the
// library/driver pair, that no call of this kind can succeed until one side moves.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", ERROR_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", ERROR_FUNCTION_NOT_FOUND, true},
		{"no driver is loaded", ERROR_DRIVER_NOT_LOADED, true},
		{"the driver is too old for the library", ERROR_DRIVER_TOO_OLD, true},
		{"the driver is too new for the library", ERROR_DRIVER_TOO_NEW, true},

		{"success", SUCCESS, false},
		{"the device does not support the feature", ERROR_NOT_SUPPORTED, false},
		{"the driver malfunctioned", ERROR_DRIVER_FAILURE, false},
		{"permission is denied", ERROR_NO_PERMISSION, false},
		{"the queried object was not found", ERROR_NOT_FOUND, false},
		{"the call timed out", ERROR_TIMEOUT, false},
		{"the failure is unknown", ERROR_UNKNOWN, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}
