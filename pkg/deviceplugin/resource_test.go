package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// This file is the wire-format golden net for the strings this package puts on the kubelet
// seam. Every expectation below is a literal, never an expression over a nodefeature constant:
// kubelet matches the exact device ID it was offered, so a changed constant is a behavior
// change and must fail here rather than be recomputed into agreement.

func TestResource_String(t *testing.T) {
	cases := []struct {
		name string
		res  Resource
		want string
	}{
		{
			name: "group and device joined by a single colon",
			res:  Resource{Group: "grp-0", Device: "dev-0"},
			want: "grp-0:dev-0",
		},
		{
			name: "a manufacturer UUID is carried through verbatim",
			res:  Resource{Group: "nvidia-0", Device: "GPU-4f2c1a3b-0001"},
			want: "nvidia-0:GPU-4f2c1a3b-0001",
		},
		{
			name: "a whole accelerator carries no index segment",
			res:  Resource{Group: "grp-0", Device: "0"},
			want: "grp-0:0",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.res.String())
		})
	}
}

func TestResource_DeviceIDs(t *testing.T) {
	res := Resource{Group: "grp-0", Device: "dev-0"}

	cases := []struct {
		name     string
		mode     workercore.DeviceAllocationMode
		poolSize int32
		// wantLen is the literal number of advertised device IDs.
		wantLen int
		// want is the complete advertisement, asserted element-wise. Left nil for pools too
		// large to spell out, which assert wantAt instead.
		want []string
		// wantAt pins individual positions of a pool listed by wantLen alone.
		wantAt map[int]string
	}{
		{
			name:    "exclusive advertises a single whole-accelerator token",
			mode:    workercore.DeviceAllocationModeExclusive,
			wantLen: 1,
			want:    []string{"grp-0:dev-0:0000"},
		},
		{
			// The Shared index is a credit offset, not a pool position: it steps by the credit
			// denominator over the owner ceiling, so only the first token is narrow enough to be
			// zero-padded. These ten literals are the whole contract.
			name:    "shared advertises one credit-offset token per owner",
			mode:    workercore.DeviceAllocationModeShared,
			wantLen: 10,
			want: []string{
				"grp-0:dev-0:0000",
				"grp-0:dev-0:160000",
				"grp-0:dev-0:320000",
				"grp-0:dev-0:480000",
				"grp-0:dev-0:640000",
				"grp-0:dev-0:800000",
				"grp-0:dev-0:960000",
				"grp-0:dev-0:1120000",
				"grp-0:dev-0:1280000",
				"grp-0:dev-0:1440000",
			},
		},
		{
			name:     "shared ignores poolSize",
			mode:     workercore.DeviceAllocationModeShared,
			poolSize: 3,
			wantLen:  10,
			wantAt: map[int]string{
				0: "grp-0:dev-0:0000",
				9: "grp-0:dev-0:1440000",
			},
		},
		{
			name:     "sliced advertises poolSize interchangeable tokens",
			mode:     workercore.DeviceAllocationModeSliced,
			poolSize: 8,
			wantLen:  8,
			want: []string{
				"grp-0:dev-0:0000",
				"grp-0:dev-0:0001",
				"grp-0:dev-0:0002",
				"grp-0:dev-0:0003",
				"grp-0:dev-0:0004",
				"grp-0:dev-0:0005",
				"grp-0:dev-0:0006",
				"grp-0:dev-0:0007",
			},
		},
		{
			name:     "sliced advertises nothing when poolSize is zero",
			mode:     workercore.DeviceAllocationModeSliced,
			poolSize: 0,
			wantLen:  0,
		},
		{
			name:     "sliced advertises nothing when poolSize is negative",
			mode:     workercore.DeviceAllocationModeSliced,
			poolSize: -1,
			wantLen:  0,
		},
		{
			name:     "partitioned advertises poolSize interchangeable tokens",
			mode:     workercore.DeviceAllocationModePartitioned,
			poolSize: 7,
			wantLen:  7,
			want: []string{
				"grp-0:dev-0:0000",
				"grp-0:dev-0:0001",
				"grp-0:dev-0:0002",
				"grp-0:dev-0:0003",
				"grp-0:dev-0:0004",
				"grp-0:dev-0:0005",
				"grp-0:dev-0:0006",
			},
		},
		{
			name:     "partitioned advertises nothing when poolSize is zero",
			mode:     workercore.DeviceAllocationModePartitioned,
			poolSize: 0,
			wantLen:  0,
		},
		{
			// 512 tokens are too many to spell out; the ends and the padding boundaries carry
			// the format, and the literal length carries the size.
			name:    "visibility advertises a fixed 512-token pool",
			mode:    workercore.DeviceAllocationModeVisibility,
			wantLen: 512,
			wantAt: map[int]string{
				0:   "grp-0:dev-0:0000",
				1:   "grp-0:dev-0:0001",
				9:   "grp-0:dev-0:0009",
				10:  "grp-0:dev-0:0010",
				99:  "grp-0:dev-0:0099",
				100: "grp-0:dev-0:0100",
				511: "grp-0:dev-0:0511",
			},
		},
		{
			name:     "visibility ignores poolSize",
			mode:     workercore.DeviceAllocationModeVisibility,
			poolSize: 3,
			wantLen:  512,
			wantAt: map[int]string{
				0:   "grp-0:dev-0:0000",
				511: "grp-0:dev-0:0511",
			},
		},
		{
			name:     "a mode with no token shape of its own advertises nothing",
			mode:     workercore.DeviceAllocationModeNone,
			poolSize: 8,
			wantLen:  0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids := res.DeviceIDs(c.mode, c.poolSize)
			require.Len(t, ids, c.wantLen)
			if c.want != nil {
				assert.Equal(t, c.want, ids)
			}
			for i, want := range c.wantAt {
				assert.Equal(t, want, ids[i], "device ID at index %d", i)
			}
		})
	}
}

// TestPadIndex pins the padding rule both device-ID producers share: four digits wide, and
// never truncated above it. Shared indices exceed the width, so widening or narrowing it would
// silently renumber a live advertisement.
func TestPadIndex(t *testing.T) {
	cases := []struct {
		name string
		idx  uint64
		want string
	}{
		{name: "zero pads to the full width", idx: 0, want: "0000"},
		{name: "one digit pads with three zeroes", idx: 7, want: "0007"},
		{name: "two digits pad with two zeroes", idx: 34, want: "0034"},
		{name: "three digits pad with one zero", idx: 511, want: "0511"},
		{name: "four digits are the width, so no padding", idx: 1600, want: "1600"},
		{name: "five digits are not truncated", idx: 12800, want: "12800"},
		{name: "the shared step is wider than the width", idx: 160000, want: "160000"},
		{name: "the last shared offset is wider still", idx: 1440000, want: "1440000"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, padIndex(c.idx))
		})
	}
}

// TestResourceToken_String pins the token form a ResourceToken renders, and its round-trip through
// ParseResourceToken. A unit names one token of one accelerator, so it must render the same
// three-segment form that accelerator advertises and the parser accepts — dropping the index yields a
// string no consumer can read back.
func TestResourceToken_String(t *testing.T) {
	unit := func(group, device string, index uint64) ResourceToken {
		return ResourceToken{Resource: Resource{Group: group, Device: device}, Index: index}
	}

	cases := []struct {
		name string
		unit ResourceToken
		want string
	}{
		{"first token pads to four digits", unit("grp-0", "dev-0", 0), "grp-0:dev-0:0000"},
		{"two-digit index", unit("grp-0", "dev-0", 34), "grp-0:dev-0:0034"},
		{"three-digit index", unit("grp-0", "dev-0", 511), "grp-0:dev-0:0511"},
		{"index beyond four digits is not truncated", unit("grp-0", "dev-0", 12800), "grp-0:dev-0:12800"},
		{"a shared credit offset renders unpadded", unit("grp-0", "dev-0", 160000), "grp-0:dev-0:160000"},
		{"a manufacturer UUID is carried through verbatim", unit("nvidia-0", "GPU-4f2c1a3b-0001", 3), "nvidia-0:GPU-4f2c1a3b-0001:0003"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.unit.String()
			assert.Equal(t, c.want, got)

			back, err := ParseResourceToken(got)
			require.NoError(t, err, "a rendered unit must parse back as a device ID")
			assert.Equal(t, c.unit, back, "round-trip must preserve the Resource and the token index")
		})
	}
}

// TestResourceToken_Parse pins what ParseResourceToken accepts off the wire. kubelet hands back the
// exact string it was offered, so anything that is not a three-segment device ID with a numeric
// index is a protocol error, not a Resource.
func TestResourceToken_Parse(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		want    ResourceToken
		wantErr bool
	}{
		{
			name: "a padded index parses to its numeric value",
			id:   "grp-0:dev-0:0007",
			want: ResourceToken{Resource: Resource{Group: "grp-0", Device: "dev-0"}, Index: 7},
		},
		{
			name: "an unpadded shared offset parses to its numeric value",
			id:   "grp-0:dev-0:160000",
			want: ResourceToken{Resource: Resource{Group: "grp-0", Device: "dev-0"}, Index: 160000},
		},
		{
			name:    "a bare Resource is not a device ID",
			id:      "grp-0:dev-0",
			wantErr: true,
		},
		{
			name:    "a fourth segment is not a device ID",
			id:      "grp-0:dev-0:0000:0000",
			wantErr: true,
		},
		{
			name:    "an empty group is rejected",
			id:      ":dev-0:0000",
			wantErr: true,
		},
		{
			name:    "an empty device is rejected",
			id:      "grp-0::0000",
			wantErr: true,
		},
		{
			name:    "a non-numeric index is rejected",
			id:      "grp-0:dev-0:abcd",
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseResourceToken(c.id)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.id, got.String(), "a parsed device ID must render back byte-identically")
		})
	}
}
