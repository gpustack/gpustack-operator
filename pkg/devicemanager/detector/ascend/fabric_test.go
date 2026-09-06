package ascend

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/device"
	productascend "gpustack.ai/gpustack/pkg/devicemanager/product/ascend"
)

// A UB endpoint identifier's 16 bytes and the 32 lowercase hex characters they publish as. The
// bytes are deliberately not palindromic and not all-equal, so a rendering that reversed them or
// dropped one is visible rather than accidentally correct.
var (
	eidBytes = [16]byte{
		0xfe, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x0a, 0x1b, 0x2c, 0x3d, 0x4e, 0x5f, 0x60, 0x07,
	}
	eidHex = "fe800000000000000a1b2c3d4e5f6007"
)

// Every derived field a consumer reads out of an endpoint is an index into these characters, so the
// encoding is pinned whole: all 16 bytes, in order, lowercase.
func TestFabricEndpoints(t *testing.T) {
	got := fabricEndpoints([]dcmi.UrmaEidInfo{
		{Eid: eidBytes, Index: 3},
		{Eid: [16]byte{}},
	})

	require.Len(t, got, 2)
	assert.Equal(t, eidHex, got[0])
	// A zero endpoint still renders its full width, so a consumer indexing into it reads a field
	// rather than running off the end.
	assert.Equal(t, strings.Repeat("0", 32), got[1])
	for _, endpoint := range got {
		assert.Len(t, endpoint, 2*len(eidBytes), "all 16 bytes are rendered")
		assert.Equal(t, strings.ToLower(endpoint), endpoint, "lowercase, as the vendor publishes it")
	}
}

// The record is assembled from whichever of the three reads answered, and no read is required for
// the others to be published. What it must never do is claim a fabric on a worker that reported
// nothing.
func TestNewFabric(t *testing.T) {
	spod := &dcmi.SpodInfo{
		Super_pod_id:   7,
		Scale_type:     384,
		Server_id:      3,
		Chassis_id:     11,
		Super_pod_type: 2,
	}

	cases := []struct {
		name      string
		spod      *dcmi.SpodInfo
		product   productascend.Type
		endpoints []string
		want      *device.Fabric
	}{
		{
			name:      "every read answered",
			spod:      spod,
			product:   productascend.TypePod2D,
			endpoints: []string{eidHex},
			want: &device.Fabric{
				Kind: "ub", ID: "7", Type: "pod-2d",
				MemberCount: 384, NodeIndex: "3", RackID: "11",
				Endpoints: []string{eidHex},
			},
		},
		{
			// A standalone inference card names its shape from its mainboard and need not sit in a
			// super pod at all, so the endpoints still travel and the domain id is simply absent.
			name:      "no super pod, shape and endpoints still published",
			product:   productascend.TypeCard4P,
			endpoints: []string{eidHex},
			want: &device.Fabric{
				Kind: "ub", Type: "card-4p", Endpoints: []string{eidHex},
			},
		},
		{
			name:    "coordinates alone",
			spod:    spod,
			product: "",
			want: &device.Fabric{
				Kind: "ub", ID: "7", MemberCount: 384, NodeIndex: "3", RackID: "11",
			},
		},
		{
			// The whole point of the nil: a driver that refused every one of these reads must leave
			// the accelerator with no fabric record rather than one asserting an unknown domain.
			name: "nothing answered",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, newFabric(c.spod, c.product, c.endpoints))
		})
	}
}

// The domain id is the super pod's own id and not its type, its server index or its rack: reading
// the wrong member would publish an id that collides across unrelated pods.
func TestNewFabric_ReadsTheSuperPodsOwnMembers(t *testing.T) {
	// Every member is given a distinct value, so a field wired to the wrong one is visible rather
	// than accidentally correct.
	got := newFabric(&dcmi.SpodInfo{
		Sdid:           1,
		Scale_type:     2,
		Super_pod_id:   3,
		Server_id:      4,
		Chassis_id:     5,
		Super_pod_type: 6,
	}, productascend.TypePod1D, nil)

	assert.Equal(t, "3", got.ID)
	assert.Equal(t, uint32(2), got.MemberCount)
	assert.Equal(t, "4", got.NodeIndex)
	assert.Equal(t, "5", got.RackID)
	// The shape comes from the shared resolver, never from Super_pod_type read raw here: the
	// resolver is what applies the mainboard branch that the two inference cards need.
	assert.Equal(t, "pod-1d", got.Type)
}
