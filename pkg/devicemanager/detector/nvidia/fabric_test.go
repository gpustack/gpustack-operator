package nvidia

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
)

// A fabric cluster uuid's 16 bytes and the 32 lowercase hex characters they publish as. The bytes
// are deliberately not palindromic and not all-equal, so a rendering that reversed them or dropped
// one is visible rather than accidentally correct.
var (
	clusterUUIDBytes = [nvml.GPU_FABRIC_UUID_LEN]uint8{
		0x5b, 0x0e, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66,
		0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xef,
	}
	clusterUUIDHex = "5b0e112233445566778899aabbccddef"

	// A second cluster, for the recabling case. It differs from the first in every byte, so a
	// replacement that kept part of the old answer is visible.
	otherClusterUUIDBytes = [nvml.GPU_FABRIC_UUID_LEN]uint8{
		0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10,
	}
	otherClusterUUIDHex = "0123456789abcdeffedcba9876543210"
)

// registered is a registration that answers every one of newFabric's checks, so a case below that
// publishes nothing differs from it in exactly the one field it is about.
func registered() *fabricInfo {
	return &fabricInfo{
		clusterUUID: clusterUUIDBytes,
		status:      nvml.SUCCESS,
		cliqueID:    3,
		state:       nvml.GPU_FABRIC_STATE_COMPLETED,
	}
}

// withState copies the registration above and puts it in one of the other states the driver reports.
func withState(state uint8) *fabricInfo {
	info := registered()
	info.state = state
	return info
}

// The gate this record exists for: the two identifiers are filled in by the fabric manager, so they
// mean nothing until it says the accelerator finished registering. Every case that publishes nothing
// carries a cluster uuid and a clique id that would otherwise be published.
func TestNewFabric(t *testing.T) {
	cases := []struct {
		name string
		info *fabricInfo
		want *device.Fabric
	}{
		{
			name: "a completed registration publishes the domain",
			info: registered(),
			want: &device.Fabric{Kind: "nvlink", ID: clusterUUIDHex, CliqueID: "3"},
		},
		{
			// This generation has no such fabric at all. The bytes beside the state are whatever the
			// driver left there.
			name: "a generation with no fabric publishes nothing",
			info: withState(nvml.GPU_FABRIC_STATE_NOT_SUPPORTED),
		},
		{
			name: "a registration that has not started publishes nothing",
			info: withState(nvml.GPU_FABRIC_STATE_NOT_STARTED),
		},
		{
			// Mid-negotiation the cluster is not settled, so publishing it would put this machine in
			// a domain it may not end up in.
			name: "a registration still in progress publishes nothing",
			info: withState(nvml.GPU_FABRIC_STATE_IN_PROGRESS),
		},
		{
			// The status is the vendor's second half of the same gate, and is to be read exactly
			// here: a registration that completed unsuccessfully identifies no cluster.
			name: "a registration that completed with an error publishes nothing",
			info: &fabricInfo{
				clusterUUID: clusterUUIDBytes,
				status:      nvml.ERROR_TIMEOUT,
				cliqueID:    3,
				state:       nvml.GPU_FABRIC_STATE_COMPLETED,
			},
		},
		{
			// The case the two gates above cannot catch. An all-zero uuid is a buffer nobody filled,
			// and every machine reporting it would otherwise land in one shared domain -- which is
			// what publishing an id compared across workers costs when it is wrong.
			name: "a cluster nobody named publishes nothing",
			info: &fabricInfo{
				status: nvml.SUCCESS,
				state:  nvml.GPU_FABRIC_STATE_COMPLETED,
			},
		},
		{
			// Zero is a real clique, not an absent answer, so the record is published and carries it.
			name: "the first clique is a clique",
			info: &fabricInfo{
				clusterUUID: clusterUUIDBytes,
				status:      nvml.SUCCESS,
				state:       nvml.GPU_FABRIC_STATE_COMPLETED,
			},
			want: &device.Fabric{Kind: "nvlink", ID: clusterUUIDHex, CliqueID: "0"},
		},
		{
			// A state outside the vendor's four is this build's understanding being out of date, so
			// it is refused rather than folded into the one value that publishes.
			name: "a state this build has no name for publishes nothing",
			info: withState(nvml.GPU_FABRIC_STATE_COMPLETED + 1),
		},
		{
			// A driver that answered nothing leaves the accelerator with no record rather than one
			// asserting an unknown domain.
			name: "nothing answered",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, newFabric(c.info))
		})
	}
}

// The domain id is the cluster uuid and the clique is the clique: a field wired to the wrong member
// would publish an id that collides across unrelated clusters.
func TestNewFabric_ReadsTheRegistrationsOwnMembers(t *testing.T) {
	// Every member is given a distinct value, so a field taking another's is visible rather than
	// accidentally correct.
	got := newFabric(&fabricInfo{
		clusterUUID: clusterUUIDBytes,
		status:      nvml.SUCCESS,
		cliqueID:    9,
		state:       nvml.GPU_FABRIC_STATE_COMPLETED,
	})

	require.NotNil(t, got)
	assert.Equal(t, "nvlink", got.Kind)
	assert.Equal(t, clusterUUIDHex, got.ID)
	assert.Equal(t, "9", got.CliqueID)
	// The manufacturer reports no shape, no size, no place in the domain and no endpoints, so those
	// stay absent rather than being filled with a stand-in.
	assert.Empty(t, got.Type)
	assert.Zero(t, got.MemberCount)
	assert.Empty(t, got.NodeIndex)
	assert.Empty(t, got.RackID)
	assert.Empty(t, got.Endpoints)
}

// The encoding is pinned whole, because the value is compared across workers: all 16 bytes, in
// order, lowercase, undelimited.
func TestNewFabric_PinsTheIDEncoding(t *testing.T) {
	got := newFabric(registered())

	require.NotNil(t, got)
	assert.Len(t, got.ID, 2*len(clusterUUIDBytes), "all 16 bytes are rendered")
	assert.Equal(t, strings.ToLower(got.ID), got.ID, "lowercase")
	assert.NotContains(t, got.ID, "-", "undelimited")
}

// A pass that could not read the registration must not withdraw the domain, and a pass that could
// must be able to.
//
// The two are one mechanism, so they are pinned together: withholding removes the published label,
// so a refused read that reported nothing would take the node out of its domain for a whole detect
// interval, while a successful read reporting a state that is no longer complete is how an
// accelerator that really left the fabric says so, and that must still get through.
func TestNvidia_RememberFabric(t *testing.T) {
	in := &nvidia{fabrics: make(map[string]fabricInfo)}
	first, second := "GPU-first", "GPU-second"

	// A first pass with nothing remembered reports nothing: there is no domain to preserve, and a
	// placeholder invented here would be published as one.
	assert.Nil(t, in.rememberFabric(first, nil))

	read := registered()
	assert.Equal(t, read, in.rememberFabric(first, read), "a read that answered is what this pass publishes")

	// The hiccup this exists for.
	assert.Equal(t, read, in.rememberFabric(first, nil), "a refused read reuses the last answer")

	// Per accelerator, so one card's registration never stands in for another's.
	assert.Nil(t, in.rememberFabric(second, nil))

	// The withdrawal path: the driver answers, and says this accelerator is no longer registered.
	left := withState(nvml.GPU_FABRIC_STATE_NOT_STARTED)
	assert.Equal(t, left, in.rememberFabric(first, left))
	assert.Equal(t, left, in.rememberFabric(first, nil), "and it is the new answer that is remembered")
	assert.Nil(t, newFabric(in.rememberFabric(first, nil)), "which publishes no domain")

	// The acquisition path, which is the withdrawal's other half: an accelerator that finishes
	// registering replaces a remembered non-final state and starts publishing.
	joined := in.rememberFabric(first, registered())
	assert.Equal(t, clusterUUIDHex, newFabric(joined).ID, "a late registration is published")

	// And a domain change, which only a successful read can carry: the replacement has to win over
	// a remembered answer that was itself complete, or a recabled machine keeps advertising the
	// domain it left.
	recabled := registered()
	recabled.clusterUUID = otherClusterUUIDBytes
	recabled.cliqueID = 7
	got := newFabric(in.rememberFabric(first, recabled))
	assert.Equal(t, otherClusterUUIDHex, got.ID, "the new domain replaces the remembered one")
	assert.Equal(t, "7", got.CliqueID)
}

// The four names exist so a log line can separate "this generation has no fabric" from "registration
// has not started" -- the one distinction that is invisible everywhere else, since the driver
// answers both reads successfully and neither publishes a record.
func TestFabricStateName(t *testing.T) {
	cases := map[uint8]string{
		nvml.GPU_FABRIC_STATE_NOT_SUPPORTED: "not-supported",
		nvml.GPU_FABRIC_STATE_NOT_STARTED:   "not-started",
		nvml.GPU_FABRIC_STATE_IN_PROGRESS:   "in-progress",
		nvml.GPU_FABRIC_STATE_COMPLETED:     "completed",
		// Reported as itself rather than folded into one of the four.
		nvml.GPU_FABRIC_STATE_COMPLETED + 1: "unrecognized-4",
	}
	for state, want := range cases {
		assert.Equal(t, want, fabricStateName(state))
	}
}
