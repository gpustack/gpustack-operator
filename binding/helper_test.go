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

// TestResolvePCITopology pins what root actually holds, which is not what the field it fills is
// called.
//
// Two callers compare their results against each other — the accelerator inventory and the network
// interface inventory — so this answer is only useful if it is the SAME answer on both sides. That
// is why they share this function instead of each deriving the coordinates: sharing makes them
// identical by construction, where two implementations would only be identical by discipline.
func TestResolvePCITopology(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		wantRoot     string
		wantSwitches []string
	}{
		{
			name:         "behind one bridge",
			path:         "/sys/devices/pci0000:00/0000:00:01.0/0000:01:00.0",
			wantRoot:     "0000:00:01.0",
			wantSwitches: []string{"0000:00:01.0"},
		},
		{
			// The fallback that surprises: with no bridge above it, the device reports ITSELF.
			// Equality on this value is then an identity check, not a same-root-complex claim.
			name:         "attached directly to the root complex",
			path:         "/sys/devices/pci0000:00/0000:05:00.0",
			wantRoot:     "0000:05:00.0",
			wantSwitches: nil,
		},
		{
			name:         "behind three bridges, innermost first",
			path:         "/sys/devices/pci0000:00/0000:00:02.0/0000:02:00.0/0000:03:01.0/0000:04:00.0",
			wantRoot:     "0000:00:02.0",
			wantSwitches: []string{"0000:03:01.0", "0000:02:00.0", "0000:00:02.0"},
		},
		{
			// A degenerate input has to terminate rather than walk forever. Nothing guards the
			// loop explicitly: "/" stops it because its base name has no colon, which is the same
			// condition that stops the walk at a root complex. Measured — adding a self-loop
			// guard changes no test, which is why there isn't one.
			name:         "the filesystem root",
			path:         "/",
			wantRoot:     "/",
			wantSwitches: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, switches := ResolvePCITopology(c.path)
			assert.Equal(t, c.wantRoot, root)
			assert.Equal(t, c.wantSwitches, switches)
		})
	}
}
