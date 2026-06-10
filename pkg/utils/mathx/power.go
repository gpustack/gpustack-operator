package mathx

import "gpustack.ai/gpustack/pkg/utils/typex"

// PowersOfTwoUpTo returns a slice of powers of two up to the given number n (inclusive).
func PowersOfTwoUpTo[I typex.Integer](n I) []I {
	if n < 1 {
		return []I{}
	}
	var result []I
	for val := I(1); val <= n; val <<= 1 {
		result = append(result, val)
	}
	return result
}
