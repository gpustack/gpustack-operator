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

// RoundDiv divides a by b, rounding the quotient to the nearest integer with halves going up,
// or returns 0 when b is 0.
func RoundDiv[I typex.Integer](a, b I) I {
	if b == 0 {
		return 0
	}
	return (a + b/2) / b
}
