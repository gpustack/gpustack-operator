//go:build !linux

package thead

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/device"
)

// Off linux there is no hgml MIG driver at all, and every one of the stub's methods answers with a
// sentinel of its own. It has to classify as unavailable like any other failure to read the
// partition subtree whole, because classifyCardInstancesCall carries no sentinel it treats
// differently -- this one included.
//
// It lives in a file this build tag excludes from linux because the sentinel does. The generic "a
// driver that cannot be asked is unavailable" case lives in preflight_test.go and runs on both
// platforms; this one is the end of the stub's own path, and only the stub's platform can take it.
func TestClassifyCardInstancesCall_StubSentinel(t *testing.T) {
	state, detail, reason := classifyCardInstancesCall(true, nil, errStubMigDriver)

	assert.Equal(t, device.PreflightStateUnavailable, state)
	assert.Empty(t, detail)
	assert.Equal(t, errStubMigDriver.Error(), reason)
}
