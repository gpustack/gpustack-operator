// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package mxsml

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table pins both halves of the predicate: every code that means the call could not be made,
// and the near-misses that must not be mistaken for one.
//
// LoadDllFailure counts because the header defines it as a failed dynamic-library load,
// which is the absence itself rather than a report about a device.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", LibraryNotFound, true},
		{"the symbol is absent from the library", FunctionNotFound, true},
		{"a dynamic library failed to load", LoadDllFailure, true},

		{"success", Success, false},
		{"the operation is not supported", OperationNotSupport, false},
		{"no device is present", NoDevice, false},
		{"permission is denied", PermissionDenied, false},
		{"sysfs could not be read", SysfsError, false},
		{"the input is invalid", InvalidInput, false},
		{"the call failed", Failure, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}
