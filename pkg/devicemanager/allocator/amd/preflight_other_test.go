//go:build !linux

package amd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/device"
)

// Off linux there is no HSA topology reader at all, and readTopology answers with a sentinel of its
// own. It has to classify as unavailable like every other failed read: an allocation is refused on
// it just the same, and a build that reported it as anything softer would say a card can serve a
// mode that this binary cannot even ask about.
//
// It lives in a file this build tag excludes from linux because the sentinel does. The generic
// "any failed read is unavailable" cases live in preflight_test.go and run on both platforms; this
// one is the end of the stub's own path, and only the stub's platform can take it.
func TestClassifyTopologyCall_StubSentinel(t *testing.T) {
	state, detail, reason := classifyTopologyCall(Topology{}, errTopologyUnsupported)

	assert.Equal(t, device.PreflightStateUnavailable, state)
	assert.Empty(t, detail)
	assert.Equal(t, errTopologyUnsupported.Error(), reason)
}
