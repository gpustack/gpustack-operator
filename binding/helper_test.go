package binding

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The array's length is the bound that does not depend on the library terminating its own buffer,
// which is the whole reason this exists rather than C.GoString.
func TestGoStringN(t *testing.T) {
	cases := []struct {
		name  string
		chars []int8
		want  string
	}{
		{name: "a terminated name stops at the NUL", chars: []int8{'1', 'g', 0, 'x', 'x'}, want: "1g"},
		{name: "a name filling every byte stops at the array", chars: []int8{'1', 'g', '5'}, want: "1g5"},
		{name: "an empty array is the empty string", chars: []int8{}, want: ""},
		{name: "a leading NUL is the empty string", chars: []int8{0, '1', 'g'}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, GoStringN(c.chars))
		})
	}
}

// C's char maps to uint8 on some platforms, so the same array must read the same either way.
func TestGoStringNUnsignedChar(t *testing.T) {
	assert.Equal(t, "1g", GoStringN([]uint8{'1', 'g', 0, 'x'}))
	assert.Equal(t, "1g", GoStringN([]uint8{'1', 'g'}))
}
