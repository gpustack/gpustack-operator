package device

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		name      string
		prefix    string
		maxLength int
		expected  string
	}{
		{"AMD Instinct MI300X OAM", "amd", 49, "instinct-mi300x-oam"},
		{"AMD Instinct MI308X OAM", "amd", 49, "instinct-mi308x-oam"},
		{"Navi 32 [Radeon RX 7700 XT / 7800 XT]", "amd", 49, "navi-32-radeon-rx-7700-xt-7800-xt"},
		{"Ascend910B2", "ascend", 46, "910b2"},
		{"310P3", "ascend", 46, "310p3"},
		{"K100_AI", "hygon", 47, "k100_ai"},
		{"MXC500", "metax", 47, "mxc500"},
		{"NVIDIA A100", "nvidia", 46, "a100"},
		{"NVIDIA H100 80GB HBM3", "nvidia", 46, "h100-80gb-hbm3"},
		{"NVIDIA H200", "nvidia", 46, "h200"},
		{"Tesla 4", "nvidia", 46, "tesla-4"},
		{"Intel(R) Xeon(R) Platinum 8358 CPU @ 2.60GHz", "intel", 0, "xeon-platinum-8358"},
		{"Intel(R) Xeon(R) Platinum 8480+ CPU @ 2.00GHz", "intel", 0, "xeon-platinum-8480"},
		{"Intel(R) Core(TM) i9-14900K", "intel", 0, "core-i9-14900k"},
		{"AMD EPYC 7763 64-Core Processor", "amd", 0, "epyc-7763-64-core-processor"},
		{"Intel(R) Xeon(R) Platinum 8358 CPU @ 2.60GHz", "intel", 10, "xeon-plati"},
		{"NVIDIA", "nvidia", 46, ""},
		{"", "nvidia", 46, ""},
		{"   ", "", 0, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := NormalizeName(c.name, c.prefix, c.maxLength)
			assert.Equal(t, c.expected, result, "NormalizeName(%q, %q, %d) should return %q, got %q", c.name, c.prefix, c.maxLength, c.expected, result)
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
