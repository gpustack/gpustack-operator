package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestResource_DeviceIDs(t *testing.T) {
	res := Resource{Group: "grp-0", Device: "dev-0"}

	cases := []struct {
		name      string
		mode      workercore.DeviceAllocationMode
		poolSize  int32
		wantLen   int
		wantFirst string
		wantLast  string
	}{
		{
			name:      "exclusive advertises a single whole-card token",
			mode:      workercore.DeviceAllocationModeExclusive,
			wantLen:   1,
			wantFirst: "grp-0:dev-0:0000",
			wantLast:  "grp-0:dev-0:0000",
		},
		{
			name:      "shared advertises one token per owner",
			mode:      workercore.DeviceAllocationModeShared,
			wantLen:   nodefeature.SharedResourceMaxSize,
			wantFirst: "grp-0:dev-0:0000",
		},
		{
			// Sliced advertises a loose token pool sized by the card's logical slice count,
			// not the old per-card MaxUnits (12800) fake-device pool.
			name:      "sliced advertises poolSize tokens per card",
			mode:      workercore.DeviceAllocationModeSliced,
			poolSize:  8,
			wantLen:   8,
			wantFirst: "grp-0:dev-0:0000",
			wantLast:  "grp-0:dev-0:0007",
		},
		{
			name:     "sliced advertises no token when poolSize is non-positive",
			mode:     workercore.DeviceAllocationModeSliced,
			poolSize: 0,
			wantLen:  0,
		},
		{
			// Partitioned has a token shape of its own, sized by the card's partition ceiling.
			name:      "partitioned advertises poolSize tokens per card",
			mode:      workercore.DeviceAllocationModePartitioned,
			poolSize:  7,
			wantLen:   7,
			wantFirst: "grp-0:dev-0:0000",
			wantLast:  "grp-0:dev-0:0006",
		},
		{
			// Visibility advertises a fixed per-card pool sized to SlicedResourceMaxSize,
			// independent of the group's max slice count.
			name:      "visibility advertises SlicedResourceMaxSize tokens per card",
			mode:      workercore.DeviceAllocationModeVisibility,
			wantLen:   nodefeature.SlicedResourceMaxSize,
			wantFirst: "grp-0:dev-0:0000",
			wantLast:  "grp-0:dev-0:0511",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ids := res.DeviceIDs(c.mode, c.poolSize)
			assert.Len(t, ids, c.wantLen)
			if c.wantLen > 0 {
				assert.Equal(t, c.wantFirst, ids[0], "first id")
				if c.wantLast != "" {
					assert.Equal(t, c.wantLast, ids[len(ids)-1], "last id")
				}
			}
		})
	}
}

// TestResourceUnit_String pins the token form a ResourceUnit renders, and its round-trip through
// ParseResourceUnit. A unit names one token of one card, so it must render the same
// three-segment form the card advertises and the parser accepts — dropping the index yields a string
// no consumer can read back.
func TestResourceUnit_String(t *testing.T) {
	unit := func(group, device string, index uint64) ResourceUnit {
		return ResourceUnit{Resource: Resource{Group: group, Device: device}, Index: index}
	}

	cases := []struct {
		name string
		unit ResourceUnit
		want string
	}{
		{"first token pads to four digits", unit("grp-0", "dev-0", 0), "grp-0:dev-0:0000"},
		{"two-digit index", unit("grp-0", "dev-0", 34), "grp-0:dev-0:0034"},
		{"three-digit index", unit("grp-0", "dev-0", 511), "grp-0:dev-0:0511"},
		{"index beyond four digits is not truncated", unit("grp-0", "dev-0", 12800), "grp-0:dev-0:12800"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.unit.String()
			assert.Equal(t, c.want, got)

			back, err := ParseResourceUnit(got)
			require.NoError(t, err, "a rendered unit must parse back as a device ID")
			assert.Equal(t, c.unit, back, "round-trip must preserve the card and the token index")
		})
	}
}
