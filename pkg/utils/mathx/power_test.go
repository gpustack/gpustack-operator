package mathx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPowersOfTwoUpTo(t *testing.T) {
	cases := map[int][]int{
		-1: {},
		0:  {},
		1:  {1},
		2:  {1, 2},
		3:  {1, 2},
		8:  {1, 2, 4, 8},
		9:  {1, 2, 4, 8},
	}
	for in, want := range cases {
		assert.Equal(t, want, PowersOfTwoUpTo(in), "PowersOfTwoUpTo(%d)", in)
	}
}

func TestLargestPowerOfTwoUpTo(t *testing.T) {
	cases := map[int64]int64{
		-1: 0, 0: 0, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 7: 4, 8: 8, 9: 8, 16: 16, 31: 16, 32: 32,
	}
	for in, want := range cases {
		assert.Equal(t, want, LargestPowerOfTwoUpTo(in), "LargestPowerOfTwoUpTo(%d)", in)
	}
}
