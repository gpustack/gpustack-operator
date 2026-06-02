package device

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_formatName(t *testing.T) {
	cases := []struct {
		name         string
		manufacturer string
		expected     string
	}{
		{"AMD Instinct MI300X OAM", "amd", "instinct-mi300x-oam"},
		{"AMD Instinct MI308X OAM", "amd", "instinct-mi308x-oam"},
		{"Navi 32 [Radeon RX 7700 XT / 7800 XT]", "amd", "navi-32-radeon-rx-7700-xt-7800-xt"},
		{"Ascend910B2", "ascend", "910b2"},
		{"310P3", "ascend", "310p3"},
		{"K100_AI", "hygon", "k100_ai"},
		{"MXC500", "metax", "mxc500"},
		{"NVIDIA A100", "nvidia", "a100"},
		{"NVIDIA H100 80GB HBM3", "nvidia", "h100-80gb-hbm3"},
		{"NVIDIA H200", "nvidia", "h200"},
		{"Tesla 4", "nvidia", "tesla-4"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := formatName(c.name, c.manufacturer)
			assert.Equal(t, c.expected, result, "formatName(%s, %s) should return %s, got %s", c.name, c.manufacturer, c.expected, result)
		})
	}
}

func Test_formatMemory(t *testing.T) {
	cases := []struct {
		name     string
		mib      uint64
		expected string
	}{
		{"0", 0, "0g"},
		{"1Mi", 1, "1g"},
		{"1024Mi", 1024, "1g"},
		{"1025Mi", 1025, "1g"},
		{"44280Mi", 44280, "43g"},
		{"43693Mi", 43693, "43g"},
		{"15360Mi", 15360, "15g"},
		{"143771Mi", 143771, "140g"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := formatMemory(c.mib)
			assert.Equal(t, c.expected, result, "formatMemory(%d) should return %s, got %s", c.mib, c.expected, result)
		})
	}
}
