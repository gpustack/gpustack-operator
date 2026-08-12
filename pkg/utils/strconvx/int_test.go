package strconvx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPadZeroUint(t *testing.T) {
	cases := []struct {
		name  string
		i     uint64
		width int
		want  string
	}{
		{"zero pads to width", 0, 4, "0000"},
		{"one digit", 7, 4, "0007"},
		{"two digits", 42, 4, "0042"},
		{"three digits", 999, 4, "0999"},
		{"exactly width", 1000, 4, "1000"},
		{"wider than width is not truncated", 123456, 4, "123456"},
		{"zero width pads nothing", 7, 0, "7"},
		{"negative width pads nothing", 7, -1, "7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, PadZeroUint(c.i, c.width))
		})
	}
}
