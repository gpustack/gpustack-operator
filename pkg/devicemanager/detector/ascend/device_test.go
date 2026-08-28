package ascend

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/device"
)

// A device whose die cannot be read yields no uuid and carries the failure out, which is what makes
// all three call sites drop it.
//
// That is the observable form of "no PCI address as a substitute": Accelerator.ID is universally
// unique by contract while a BDF repeats on every node of a fleet, so a device that cannot identify
// itself has to be dropped rather than identified some other way. Returning an empty uuid with a
// success code instead would have every unidentifiable device on a node collapse onto one id.
//
// On a host with no Ascend library to load, every die entry point is unresolved, which is exactly
// this case. A host whose driver does answer for device 0 cannot observe it, hence the skip rather
// than a failure — the same host-dependence the process tests in this package carry.
func TestDeviceUUIDUnreadable(t *testing.T) {
	uuid, ret := deviceUUID(dcmi.Device{})
	if ret.IsSuccess() {
		t.Skip("an Ascend driver answered for device 0 on this host, so an unreadable die cannot be observed")
	}

	assert.Empty(t, uuid, "the uuid of a device whose die cannot be read")
	assert.False(t, ret.IsSuccess(), "the failure must be carried out, not swallowed")
}

// The detector names a group's family by chaining these two, so the chain is what the cases below
// drive. Both ends are asserted: a soc name and a family can agree on the family while disagreeing
// on how it was reached, and reaching it by fallback rather than by an exact name is the failure
// this guards. "Ascend910_9363" is the case in point -- absent from the version map it still ends
// up with a family, just the wrong one, because "^910" catches it.
func TestGuessSocNameAndFamily(t *testing.T) {
	// The 950 generation is the one Ascend recognizes by prefix rather than by name: its suffixes
	// are an open set (PR, DT, ...), and every upstream that reads a chip name -- ascend-common's
	// Is910A5Chip, ascend-docker-runtime's own copy, torch_npu's SetSocVersion -- tests
	// HasPrefix("Ascend950") instead of listing them. These cases pin that same rule here.
	testCases := []struct {
		name        string
		devName     string
		wantSocName string
		wantFamily  string
	}{
		{"a 950 names itself in full", "Ascend950", "Ascend950", "950"},
		{"a 950PR resolves to the 950 family", "Ascend950PR", "Ascend950", "950"},
		{"a 950DT resolves to the 950 family", "Ascend950DT", "Ascend950", "950"},
		{"a 950 suffix nobody has shipped yet still resolves", "Ascend950XX", "Ascend950", "950"},
		{"the Ascend prefix is optional", "950PR", "Ascend950", "950"},
		// 910C parts carry an underscore, so they are named exactly and must never reach "^910",
		// which would resolve them to Ascend910A and hand back the "910" family instead.
		{"a 910C part is named exactly", "Ascend910_9362", "Ascend910_9362", "910C"},
		{"the newest 910C part is named exactly", "Ascend910_9363", "Ascend910_9363", "910C"},
		{"a 910B part is named exactly", "910B2", "Ascend910B2", "910B"},
		{"an unlisted 910B suffix falls back within its family", "910B9", "Ascend910B1", "910B"},
		{"a 310P part is named exactly", "310P3", "Ascend310P3", "310P"},
		{"an unlisted 310P suffix falls back within its family", "310P9", "Ascend310P1", "310P"},
		{"a 310B part falls back within its family", "310B9", "Ascend310B1", "310B"},
		{"a bare 910 is the oldest family", "910A", "Ascend910A", "910"},
		{"an empty name yields no family", "", "", ""},
		{"an unknown name yields no family", "Ascend404", "", ""},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			socName := guessSocNameFromDeviceName(tc.devName)
			assert.Equalf(t, tc.wantSocName, socName, "guessSocNameFromDeviceName(%q)", tc.devName)
			assert.Equalf(t, tc.wantFamily, getFamilyFromSocName(socName),
				"getFamilyFromSocName(guessSocNameFromDeviceName(%q))", tc.devName)
		})
	}
}

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
