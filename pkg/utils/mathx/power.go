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

// LargestPowerOfTwoUpTo returns the largest power of two less than or equal to n
// (the last element PowersOfTwoUpTo would yield), or 0 when n < 1.
func LargestPowerOfTwoUpTo[I typex.Integer](n I) I {
	if n < 1 {
		return 0
	}
	p := I(1)
	for p<<1 <= n {
		p <<= 1
	}
	return p
}
