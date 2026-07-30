package ascend

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/device"
)

func TestGetLogicalSliced(t *testing.T) {
	sliced := device.AcceleratorLogicalSliced{
		Count:                     63,
		CoresPercentageOvercommit: true,
	}

	// Slicing is offered exactly where the image builds a vcann-rt, so every case below mirrors
	// one xbuild-ascend-cann-<major>-<family> stage or the absence of one. The "10.0" rows matter
	// twice over: an unbuilt major must be refused, and the major has to be compared as a number
	// because it arrives as a string, in which "10" sorts below "9" and would slip through a
	// lexicographic ">= 9".
	testCases := []struct {
		name           string
		family         string
		runtimeVersion string
		want           device.AcceleratorLogicalSliced
	}{
		{"310P on CANN 9 slices", "310P", "9.1", sliced},
		{"310P on CANN 8 does not", "310P", "8.5", device.AcceleratorLogicalSliced{}},
		{"310P without a runtime falls back to 8", "310P", "", device.AcceleratorLogicalSliced{}},
		{"910B slices on CANN 8", "910B", "8.5", sliced},
		{"910B slices on CANN 9", "910B", "9.1", sliced},
		{"910C slices on CANN 8", "910C", "8.5", sliced},
		{"910C slices on CANN 9", "910C", "9.1", sliced},
		{"950 slices on CANN 9", "950", "9.1", sliced},
		// No cann-8-950 stage exists, so a 950 on a CANN 8 host must not be offered slicing it
		// cannot start.
		{"950 does not slice on CANN 8", "950", "8.5", device.AcceleratorLogicalSliced{}},
		// An unbuilt major is refused for every family, which is also the numeric-compare guard.
		{"310P does not slice on CANN 10", "310P", "10.0", device.AcceleratorLogicalSliced{}},
		{"910B does not slice on CANN 10", "910B", "10.0", device.AcceleratorLogicalSliced{}},
		{"910 does not slice on CANN 8", "910", "8.5", device.AcceleratorLogicalSliced{}},
		{"910 does not slice on CANN 9", "910", "9.1", device.AcceleratorLogicalSliced{}},
		{"310B does not slice on CANN 9", "310B", "9.1", device.AcceleratorLogicalSliced{}},
		{"an unknown family does not slice", "", "9.1", device.AcceleratorLogicalSliced{}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, getLogicalSliced(tc.family, tc.runtimeVersion),
				"getLogicalSliced(%q, %q)", tc.family, tc.runtimeVersion)
		})
	}
}
