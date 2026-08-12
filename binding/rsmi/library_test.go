// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package rsmi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table pins both halves of the predicate: every code that means the call could not be made,
// and the near-misses that must not be mistaken for one.
//
// STATUS_NOT_YET_IMPLEMENTED counts: the entry point exists in the header but this
// library build does not serve it. STATUS_NOT_SUPPORTED does not — that one is the driver
// answering about a device.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", STATUS_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", STATUS_FUNCTION_NOT_FOUND, true},
		{"no driver is loaded", STATUS_DRIVER_NOT_LOADED, true},
		{"the library does not implement the call yet", STATUS_NOT_YET_IMPLEMENTED, true},

		{"success", STATUS_SUCCESS, false},
		{"the device does not support the feature", STATUS_NOT_SUPPORTED, false},
		{"initialization failed", STATUS_INIT_ERROR, false},
		{"permission is denied", STATUS_PERMISSION, false},
		{"the queried object was not found", STATUS_NOT_FOUND, false},
		{"a sysfs file could not be read", STATUS_FILE_ERROR, false},
		{"the resource is busy", STATUS_BUSY, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}
