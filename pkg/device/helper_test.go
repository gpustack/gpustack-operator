package device

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		name       string
		prefix     string
		maxLength  int
		stripCruft bool
		expected   string
	}{
		{"AMD Instinct MI300X OAM", "amd", 49, false, "instinct-mi300x-oam"},
		{"AMD Instinct MI308X OAM", "amd", 49, false, "instinct-mi308x-oam"},
		{"Navi 32 [Radeon RX 7700 XT / 7800 XT]", "amd", 49, false, "navi-32-radeon-rx-7700-xt-7800-xt"},
		{"Ascend910B2", "ascend", 46, false, "910b2"},
		{"310P3", "ascend", 46, false, "310p3"},
		{"K100_AI", "hygon", 47, false, "k100_ai"},
		{"MXC500", "metax", 47, false, "mxc500"},
		{"NVIDIA A100", "nvidia", 46, false, "a100"},
		{"NVIDIA H100 80GB HBM3", "nvidia", 46, false, "h100-80gb-hbm3"},
		{"NVIDIA H200", "nvidia", 46, false, "h200"},
		{"Tesla 4", "nvidia", 46, false, "tesla-4"},
		{"Intel(R) Xeon(R) Platinum 8358 CPU @ 2.60GHz", "intel", 0, true, "xeon-platinum-8358"},
		{"Intel(R) Xeon(R) Platinum 8480+ CPU @ 2.00GHz", "intel", 0, true, "xeon-platinum-8480"},
		{"Intel(R) Core(TM) i9-14900K", "intel", 0, true, "core-i9-14900k"},
		{"Intel(R) Xeon(R) Platinum 8358 CPU @ 2.60GHz", "intel", 10, true, "xeon-plati"},
		{"AMD EPYC 7763 64-Core Processor", "amd", 0, true, "epyc-7763"},
		{"AMD Ryzen 9 7950X 16-Core Processor", "amd", 0, true, "ryzen-9-7950x"},
		{"AMD Ryzen Threadripper PRO 5995WX 64-Cores", "amd", 0, true, "ryzen-threadripper-pro-5995wx"},
		{"11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz", "intel", 0, true, "core-i7-1165g7"},
		{"13th Gen Intel(R) Core(TM) i9-13900K", "intel", 0, true, "core-i9-13900k"},
		{"Genuine Intel(R) CPU 0000 @ 2.00GHz", "intel", 0, true, "cpu-0000"},
		// Without stripCruft the trademark/frequency/Processor cruft is retained.
		{"AMD EPYC 7763 64-Core Processor", "amd", 0, false, "epyc-7763-64-core-processor"},
		{"Intel(R) Core(TM) i9-14900K", "intel", 0, false, "r-coretm-i9-14900k"},
		{"NVIDIA", "nvidia", 46, false, ""},
		{"", "nvidia", 46, false, ""},
		{"   ", "", 0, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := NormalizeName(c.name, c.prefix, c.maxLength, c.stripCruft)
			assert.Equal(t, c.expected, result, "NormalizeName(%q, %q, %d, %t) should return %q, got %q", c.name, c.prefix, c.maxLength, c.stripCruft, c.expected, result)
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

func TestRuntimeMajor(t *testing.T) {
	cases := []struct {
		ver, fallback, want string
	}{
		{ver: "12.4", fallback: "12", want: "12"},
		{ver: "13.0", fallback: "12", want: "13"},
		{ver: "9.0", fallback: "8", want: "9"},
		{ver: "8", fallback: "8", want: "8"}, // no dot
		{ver: "", fallback: "12", want: "12"},
		{ver: "", fallback: "8", want: "8"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, RuntimeMajor(c.ver, c.fallback))
	}
}

func TestAcceleratorsFeature_MaxSlices(t *testing.T) {
	cases := []struct {
		name    string
		feature AcceleratorsFeature
		want    int32
	}{
		{"no capability", AcceleratorsFeature{}, 0},
		{"logical only", AcceleratorsFeature{LogicalSliced: AcceleratorSliced{MaxSize: 128}}, 128},
		{"physical only", AcceleratorsFeature{PhysicalSliced: AcceleratorSliced{MaxSize: 7}}, 7},
		{"both, physical larger", AcceleratorsFeature{PhysicalSliced: AcceleratorSliced{MaxSize: 63}, LogicalSliced: AcceleratorSliced{MaxSize: 16}}, 63},
		{"both, logical larger", AcceleratorsFeature{PhysicalSliced: AcceleratorSliced{MaxSize: 4}, LogicalSliced: AcceleratorSliced{MaxSize: 16}}, 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.feature.MaxSlices())
		})
	}
}
