package ascend

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/dcmi"
)

// shareFlagUndeclared is the only production code that turns a driver's refusal into permission to
// put a second tenant on an accelerator, so both halves of its conjunction are pinned here against
// every return code the path can carry. Dropping the generation check would admit any V2 failure;
// dropping the code check would admit a V2 device the DM merely lacks the privilege to query.
//
// This file is linux-only for the same reason its subject is: binding/dcmi cannot be linked into a
// darwin test binary. No CI job runs a linux Go suite either, so this runs in a linux container --
// the same seam check the rest of this file's package documents.
func TestShareFlagUndeclared(t *testing.T) {
	testCases := []struct {
		name    string
		version dcmi.APIVersion
		ret     dcmi.Return
		want    bool
	}{
		// The generation that declares no such flag, answering the two ways a call can be reported
		// as not served.
		{"V2 refusing the call has no flag to speak of", dcmi.APIVersionV2, dcmi.ERROR_NOT_SUPPORT, true},
		{"V2 missing the entry point likewise", dcmi.APIVersionV2, dcmi.ERROR_FUNCTION_NOT_FOUND, true},

		// Ordinary failures on that same generation. Each is a driver that would have answered
		// about the flag, so none may be waved through.
		{"V2 without the privilege is an ordinary failure", dcmi.APIVersionV2, dcmi.ERROR_OPER_NOT_PERMITTED, false},
		{"V2 timing out is an ordinary failure", dcmi.APIVersionV2, dcmi.ERROR_TIME_OUT, false},
		{"V2 refusing inside a container is an ordinary failure", dcmi.APIVersionV2, dcmi.ERROR_NOT_SUPPORT_IN_CONTAINER, false},
		{"V2 on an unknown error is an ordinary failure", dcmi.APIVersionV2, dcmi.ERROR_UNKNOWN, false},
		// A library that never loaded has said nothing about any generation, so this path must
		// refuse even though it is an API-unavailable code like FUNCTION_NOT_FOUND.
		{"a library that never loaded says nothing about a generation", dcmi.APIVersionV2, dcmi.ERROR_LIBRARY_NOT_FOUND, false},

		// The generation that does declare the flag. NOT_SUPPORT here is the driver speaking about
		// a device, which is the whole reason a return code cannot classify this on its own.
		{"V1 refusing the call is about the device, not the API", dcmi.APIVersionV1, dcmi.ERROR_NOT_SUPPORT, false},
		{"V1 missing the entry point is an ordinary failure", dcmi.APIVersionV1, dcmi.ERROR_FUNCTION_NOT_FOUND, false},
		{"V1 without the privilege is an ordinary failure", dcmi.APIVersionV1, dcmi.ERROR_OPER_NOT_PERMITTED, false},

		// A generation nothing has established yet cannot be the one that declares no flag.
		{"an uninitialized library claims no generation", dcmi.APIVersionUnknown, dcmi.ERROR_NOT_SUPPORT, false},
		{"an uninitialized library claims none on a missing entry point either", dcmi.APIVersionUnknown, dcmi.ERROR_FUNCTION_NOT_FOUND, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, shareFlagUndeclared(tc.version, tc.ret),
				"shareFlagUndeclared(%v, %v)", tc.version, tc.ret)
		})
	}
}
