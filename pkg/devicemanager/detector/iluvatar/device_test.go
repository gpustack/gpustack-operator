package iluvatar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/ixml"
)

// TestEvaluateDeviceUnhealthy pins the Iluvatar health rule: any health state other than the
// vendor's OK makes the card unhealthy, while a state the driver could not read is not a fault of
// the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name   string
		health uint64
		ret    ixml.Return
		want   bool
	}{
		{
			name:   "the OK health state is healthy",
			health: ixml.HealthOK,
			ret:    ixml.SUCCESS,
			want:   false,
		},
		{
			name:   "a memory error health state is unhealthy",
			health: ixml.HealthMemoryError,
			ret:    ixml.SUCCESS,
			want:   true,
		},
		{
			name:   "an unsupported health query is healthy even with a faulty state",
			health: ixml.HealthMemoryError,
			ret:    ixml.ERROR_NOT_SUPPORTED,
			want:   false,
		},
		{
			name:   "a health query whose driver is not loaded is healthy even with a faulty state",
			health: ixml.HealthMemoryError,
			ret:    ixml.ERROR_DRIVER_NOT_LOADED,
			want:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.health, tc.ret))
		})
	}
}
