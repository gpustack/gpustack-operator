package thead

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/hgml"
)

// TestEvaluateDeviceUnhealthy pins the THead health rule: an uncorrectable ECC error makes the
// card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name      string
		eccErrors uint64
		ret       hgml.Return
		want      bool
	}{
		{
			name:      "no uncorrectable ECC error is healthy",
			eccErrors: 0,
			ret:       hgml.SUCCESS,
			want:      false,
		},
		{
			name:      "an uncorrectable ECC error is unhealthy",
			eccErrors: 3,
			ret:       hgml.SUCCESS,
			want:      true,
		},
		{
			name:      "an unsupported ECC query is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       hgml.ERROR_NOT_SUPPORTED,
			want:      false,
		},
		{
			name:      "an ECC query whose driver is not loaded is healthy even with a nonzero count",
			eccErrors: 3,
			ret:       hgml.ERROR_DRIVER_NOT_LOADED,
			want:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccErrors, tc.ret))
		})
	}
}
