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

// The zero value of APIVersion has to be Unknown, and this is the assertion that keeps it so.
//
// Every adapted method dispatches on "is this V2", so anything that is not V2 takes the V1 path. A
// DCMI whose initialization never ran, or ran and failed, therefore has to report a generation that
// is neither -- if the numbering ever changed so that V1 sat at zero, such a library would claim V1
// and issue V1 calls at a driver that may well be V2, with nothing in the logs to say why every
// answer was a refusal.
func TestAPIVersion_ZeroValueIsUnknown(t *testing.T) {
	var unset APIVersion

	assert.Equal(t, APIVersionUnknown, unset, "the zero value of APIVersion")
	assert.EqualValues(t, 0, APIVersionUnknown, "APIVersionUnknown")
	assert.NotEqualValues(t, 0, APIVersionV1, "APIVersionV1 must not be the zero value")
	assert.NotEqualValues(t, 0, APIVersionV2, "APIVersionV2 must not be the zero value")
}

// The string is operator-facing: it is what Init logs when a driver answers V2, so an unmapped
// value has to say so rather than print a bare number that reads like a generation.
func TestAPIVersion_String(t *testing.T) {
	testCases := []struct {
		name    string
		version APIVersion
		want    string
	}{
		{"the V1 generation", APIVersionV1, "V1"},
		{"the V2 generation", APIVersionV2, "V2"},
		{"nothing has initialized", APIVersionUnknown, "unknown"},
		{"a value no generation uses", APIVersion(7), "unknown API version: 7"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.version.String())
		})
	}
}
