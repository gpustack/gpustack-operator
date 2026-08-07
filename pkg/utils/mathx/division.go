package mathx

import "gpustack.ai/gpustack/pkg/utils/typex"

// CeilDiv divides a by b, rounding the quotient up, or returns 0 when b is 0.
// Any non-zero a yields a non-zero result.
func CeilDiv[I typex.Integer](a, b I) I {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}
