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
		withBias bool
		expected string
	}{
		{"0gb", 0, false, "0gb"},
		{"1gb", 1, false, "1gb"},
		{"1gb", 1024, false, "1gb"},
		{"2gb", 1025, false, "2gb"},
		{"44gb with bias", 44280, true, "44gb"},
		{"43gb with bias", 43693, true, "44gb"},
		{"15gb", 15360, false, "15gb"},
		{"141gb", 143771, false, "141gb"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := formatMemory(c.mib, c.withBias)
			assert.Equal(t, c.expected, result, "formatMemory(%d) should return %s, got %s", c.mib, c.expected, result)
		})
	}
}
