package metax

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/mxsml"
)

// TestEvaluateDeviceUnhealthy pins the MetaX health rule: an uncorrectable DRAM ECC error makes
// the card unhealthy, while a count the driver could not read is not a fault of the card.
func TestEvaluateDeviceUnhealthy(t *testing.T) {
	testCases := []struct {
		name      string
		eccErrors mxsml.EccErrorCount
		ret       mxsml.Return
		want      bool
	}{
		{
			name:      "no uncorrectable DRAM ECC error is healthy",
			eccErrors: mxsml.EccErrorCount{DramUE: 0},
			ret:       mxsml.Success,
			want:      false,
		},
		{
			name:      "an uncorrectable DRAM ECC error is unhealthy",
			eccErrors: mxsml.EccErrorCount{DramUE: 3},
			ret:       mxsml.Success,
			want:      true,
		},
		{
			name:      "an unsupported ECC query is healthy even with a nonzero count",
			eccErrors: mxsml.EccErrorCount{DramUE: 3},
			ret:       mxsml.OperationNotSupport,
			want:      false,
		},
		{
			name:      "an ECC query whose driver sysfs could not answer is healthy even with a nonzero count",
			eccErrors: mxsml.EccErrorCount{DramUE: 3},
			ret:       mxsml.SysfsError,
			want:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluateDeviceUnhealthy(tc.eccErrors, tc.ret))
		})
	}
}
