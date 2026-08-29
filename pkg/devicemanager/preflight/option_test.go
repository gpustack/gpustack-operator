package preflight

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --runtime names one of a fixed set, and a name outside it has to be refused here rather than
// downstream. The resolution downstream cannot tell a typo from a host without that runtime -- both
// arrive as "the requested runtime is not drivable" -- and it answers the second by emitting the
// steps and exiting 0, which is the wrong answer to the first: the operator asked for a preflight of
// something and got a successful run of nothing.
func TestOptionsValidate_Runtime(t *testing.T) {
	testCases := []struct {
		name    string
		runtime string
		wantErr bool
	}{
		{
			name: "empty follows the kubelet, and is the default",
		},
		{
			name:    "docker is drivable",
			runtime: "docker",
		},
		{
			name:    "nerdctl is drivable",
			runtime: "nerdctl",
		},
		{
			name:    "ctr is drivable",
			runtime: "ctr",
		},
		{
			name:    "a typo is a usage error, not a host that has no such runtime",
			runtime: "dokcer",
			wantErr: true,
		},
		{
			// The shape that made this worth refusing: a shell that expanded --runtime into a flag
			// value reads as a runtime named "true", and used to be reported as one in the emitted
			// steps' own reasoning.
			name:    "a flag value swallowed as the runtime is a usage error",
			runtime: "true",
			wantErr: true,
		},
		{
			name:    "a container runtime this command does not drive is a usage error",
			runtime: "podman",
			wantErr: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			o := NewOptions()
			o.Runtime = tc.runtime

			err := o.Validate(context.Background())

			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--runtime", "the message must name the flag that is wrong")
			for _, rt := range hostRuntimes {
				assert.Contains(t, err.Error(), rt, "and list what would have been accepted")
			}
		})
	}
}
