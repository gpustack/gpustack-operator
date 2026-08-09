package mathx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCeilDiv(t *testing.T) {
	testCases := []struct {
		name     string
		a, b     uint64
		expected uint64
	}{
		{name: "exact division", a: 1 << 21, b: 1 << 20, expected: 2},
		{name: "rounds a remainder up", a: (1 << 20) + 1, b: 1 << 20, expected: 2},
		{name: "keeps a sub-divisor value visible", a: 1, b: 1 << 20, expected: 1},
		{name: "zero stays zero", a: 0, b: 1 << 20, expected: 0},
		{name: "zero divisor yields zero", a: 42, b: 0, expected: 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, CeilDiv(tc.a, tc.b))
		})
	}
}
