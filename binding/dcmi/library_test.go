// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dcmi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table pins both halves of the predicate: every code that means the call could not be made,
// and the near-misses that must not be mistaken for one.
//
// ERROR_NOT_SUPPORT and ERROR_NOT_SUPPORT_IN_CONTAINER are the near-misses: both are the
// driver answering about this device or this container, not about a missing entry point.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", ERROR_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", ERROR_FUNCTION_NOT_FOUND, true},

		{"success", SUCCESS, false},
		{"the device does not support the feature", ERROR_NOT_SUPPORT, false},
		{"the feature is unavailable inside a container", ERROR_NOT_SUPPORT_IN_CONTAINER, false},
		{"the operation is not permitted", ERROR_OPER_NOT_PERMITTED, false},
		{"the device does not exist", ERROR_DEVICE_NOT_EXIST, false},
		{"the call timed out", ERROR_TIME_OUT, false},
		{"the failure is unknown", ERROR_UNKNOWN, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}
