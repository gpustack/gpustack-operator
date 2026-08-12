// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package hsa

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table pins both halves of the predicate: every code that means the call could not be made,
// and the near-misses that must not be mistaken for one.
//
// The runtime declares no version-skew or driver-absence code of its own — every
// hsa_status_t is a statement about the runtime's own state or the caller's arguments — so
// only the two loader sentinels qualify.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", STATUS_ERROR_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", STATUS_ERROR_FUNCTION_NOT_FOUND, true},

		{"success", STATUS_SUCCESS, false},
		{"the runtime is not initialized", STATUS_ERROR_NOT_INITIALIZED, false},
		{"the argument is invalid", STATUS_ERROR_INVALID_ARGUMENT, false},
		{"the agent is invalid", STATUS_ERROR_INVALID_AGENT, false},
		{"the runtime is out of resources", STATUS_ERROR_OUT_OF_RESOURCES, false},
		{"the runtime state is invalid", STATUS_ERROR_INVALID_RUNTIME_STATE, false},
		{"the runtime failed fatally", STATUS_ERROR_FATAL, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}
