package hygon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/rsmi"
)

// TestEvaluateDeviceUnhealthy pins the Hygon health rule: an uncorrectable UMC ECC error makes
// the card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name     string
		eccCount rsmi.ErrorCount
		ret      rsmi.Return
		want     bool
	}{
		{
			name:     "no uncorrectable ECC error is healthy",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 0},
			ret:      rsmi.STATUS_SUCCESS,
			want:     false,
		},
		{
			name:     "an uncorrectable ECC error is unhealthy",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 3},
			ret:      rsmi.STATUS_SUCCESS,
			want:     true,
		},
		{
			name:     "an unsupported ECC query is healthy even with a nonzero count",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 3},
			ret:      rsmi.STATUS_NOT_SUPPORTED,
			want:     false,
		},
		{
			name:     "an ECC query whose driver could not be initialized is healthy even with a nonzero count",
			eccCount: rsmi.ErrorCount{Uncorrectable_err: 3},
			ret:      rsmi.STATUS_INIT_ERROR,
			want:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccCount, tc.ret))
		})
	}
}
