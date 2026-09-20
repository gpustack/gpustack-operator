package nodefeature

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

// fabricGroups builds one group whose accelerators carry the given fabric records, in order. A nil
// entry is an accelerator with no fabric record at all.
func fabricGroups(fabrics ...*device.Fabric) device.DevicesGroupList {
	accels := make([]device.Accelerator, 0, len(fabrics))
	for i, fabric := range fabrics {
		accels = append(accels, device.Accelerator{
			ID:       string(rune('a' + i)),
			Topology: device.Topology{Fabric: fabric},
		})
	}
	return device.DevicesGroupList{{ID: "grp", Accelerators: accels}}
}

func TestConstructFabricNodeLabels(t *testing.T) {
	ub7 := &device.Fabric{Kind: "ub", ID: "7", MemberCount: 384}

	cases := []struct {
		name   string
		groups device.DevicesGroupList
		want   map[string]string
	}{
		{
			name:   "one domain across every accelerator",
			groups: fabricGroups(ub7, ub7),
			want: map[string]string{
				NodeFabricDomainLabelKey:  "ub-7",
				NodeFabricMembersLabelKey: "384",
			},
		},
		{
			// The kind is in the value, so two fabrics that both call themselves 7 do not read as
			// one domain.
			name: "two kinds sharing an id do not agree",
			groups: fabricGroups(ub7,
				&device.Fabric{Kind: "xgmi", ID: "7"}),
			want: map[string]string{},
		},
		{
			name: "two domains of one kind do not agree",
			groups: fabricGroups(ub7,
				&device.Fabric{Kind: "ub", ID: "8", MemberCount: 384}),
			want: map[string]string{},
		},
		{
			// Silence is disagreement: half a node on a fabric has no node-level domain.
			name:   "an accelerator with no record disagrees",
			groups: fabricGroups(ub7, nil),
			want:   map[string]string{},
		},
		{
			// The detector publishes a record with no domain id when the coordinates were
			// unreadable but the endpoints were not. That is not an answer this label can carry.
			name:   "a record with no domain id is not a domain",
			groups: fabricGroups(&device.Fabric{Kind: "ub", Endpoints: []string{"fe80"}}),
			want:   map[string]string{},
		},
		{
			// A domain reporting no size is still a domain.
			name:   "no member count",
			groups: fabricGroups(&device.Fabric{Kind: "ub", ID: "7"}),
			want:   map[string]string{NodeFabricDomainLabelKey: "ub-7"},
		},
		{
			// The domain is what the cards were compared on, so disagreeing about its size costs
			// only the size. Publishing one of the two counts would publish whichever card the loop
			// reached first, and two nodes in one super pod would advertise different sizes for it.
			name: "cards disagreeing on the size keep the domain and lose the count",
			groups: fabricGroups(ub7,
				&device.Fabric{Kind: "ub", ID: "7", MemberCount: 128}),
			want: map[string]string{NodeFabricDomainLabelKey: "ub-7"},
		},
		{
			// The same pair the other way round. Without this the assertion above passes for a
			// reduction that simply keeps the last count it saw, which is the same defect.
			name: "the disagreement is not resolved by iteration order",
			groups: fabricGroups(&device.Fabric{Kind: "ub", ID: "7", MemberCount: 128},
				ub7),
			want: map[string]string{NodeFabricDomainLabelKey: "ub-7"},
		},
		{
			// A card reporting no size is a size nobody reported, not agreement with the card that
			// did report one.
			name: "a card reporting no size withholds the count too",
			groups: fabricGroups(ub7,
				&device.Fabric{Kind: "ub", ID: "7"}),
			want: map[string]string{NodeFabricDomainLabelKey: "ub-7"},
		},
		{
			// The clique is in the value, so a partitioned fabric publishes one value per clique
			// rather than one for the cluster. Two nodes on either side of a partition are what
			// this keeps apart; a node holding both halves, below, has no value at all.
			name: "a partitioned fabric names the clique in the value",
			groups: fabricGroups(
				&device.Fabric{Kind: "nvlink", ID: "c0", CliqueID: "1"},
				&device.Fabric{Kind: "nvlink", ID: "c0", CliqueID: "1"}),
			want: map[string]string{NodeFabricDomainLabelKey: "nvlink-c0-1"},
		},
		{
			// One fabric, partitioned across this node. Neither half can address the other, so the
			// node has no single answer to a key that promises the whole node.
			name: "one fabric whose cliques disagree",
			groups: fabricGroups(
				&device.Fabric{Kind: "nvlink", ID: "c0", CliqueID: "0"},
				&device.Fabric{Kind: "nvlink", ID: "c0", CliqueID: "1"}),
			want: map[string]string{},
		},
		{
			// Zero is a real clique, so it is rendered like any other rather than dropping back to
			// the two-part form -- which would make clique 0 collide with the whole cluster.
			name:   "the first clique is named like any other",
			groups: fabricGroups(&device.Fabric{Kind: "nvlink", ID: "c0", CliqueID: "0"}),
			want:   map[string]string{NodeFabricDomainLabelKey: "nvlink-c0-0"},
		},
		{
			name:   "no accelerators at all",
			groups: device.DevicesGroupList{},
			want:   map[string]string{},
		},
		{
			// A composite value the sanitizer would rewrite is withheld whole rather than published
			// truncated: a truncated domain names a different domain other nodes may be in.
			name:   "a domain too long to publish is withheld",
			groups: fabricGroups(&device.Fabric{Kind: "nvlink", ID: strings.Repeat("a", 64)}),
			want:   map[string]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, ConstructFabricNodeLabels(c.groups))
		})
	}
}

// The widest value a manufacturer can report still publishes, and this is the assertion that says
// so at production width.
//
// The other size case asserts that an oversized value is withheld; on its own it would pass for a
// construction that withheld everything, and the ids it uses are two characters long. This is its
// positive baseline: a real cluster id is 32 characters, a clique is up to ten digits, and the value
// is withheld whole rather than truncated when it does not fit -- so a domain that outgrew the cap
// would take the node out of it with nothing reporting that it had.
func TestConstructFabricNodeLabels_TheWidestValueFits(t *testing.T) {
	widest := &device.Fabric{
		Kind:     "nvlink",
		ID:       strings.Repeat("f", 32), // a cluster uuid, all 16 bytes as lowercase hex
		CliqueID: "4294967295",            // the largest clique a uint32 can name
	}

	got := ConstructFabricNodeLabels(fabricGroups(widest, widest))

	value, ok := got[NodeFabricDomainLabelKey]
	require.True(t, ok, "the widest value is published rather than withheld")
	assert.Equal(t, "nvlink-"+strings.Repeat("f", 32)+"-4294967295", value)
	assert.LessOrEqual(t, len(value), 63, "the label value cap this construction has to fit")
	assert.Equal(t, value, kubemeta.SanitizeLabelValue(value), "and the sanitizer leaves it unchanged")
}

// The two rules the reduction enforces, tested where they are observable.
//
// At the label level they are invisible: a record with no domain id renders `<kind>-`, which ends in
// a separator and so is refused by the sanitizer anyway — meaning a labels-only test passes whether
// or not the guard exists, and would keep passing if the guard were deleted. Reaching the reduction
// directly is what gives the assertion teeth.
func TestSoleFabricDomain(t *testing.T) {
	cases := []struct {
		name    string
		groups  device.DevicesGroupList
		wantOK  bool
		wantDom string
	}{
		{
			name:    "a domain every accelerator agrees on",
			groups:  fabricGroups(&device.Fabric{Kind: "ub", ID: "7"}, &device.Fabric{Kind: "ub", ID: "7"}),
			wantOK:  true,
			wantDom: "ub-7",
		},
		{
			name:   "a record with no domain id is not a domain",
			groups: fabricGroups(&device.Fabric{Kind: "ub"}),
		},
		{
			name:   "a record with no kind is not a domain",
			groups: fabricGroups(&device.Fabric{ID: "7"}),
		},
		{
			// The clique reaches the value through the reduction, not through the label writer, so
			// this is where composing it is observable.
			name: "a clique every accelerator agrees on is part of the domain",
			groups: fabricGroups(
				&device.Fabric{Kind: "nvlink", ID: "c0", CliqueID: "2"},
				&device.Fabric{Kind: "nvlink", ID: "c0", CliqueID: "2"}),
			wantOK:  true,
			wantDom: "nvlink-c0-2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			domain, _, ok := soleFabricDomain(c.groups)

			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.wantDom, domain)
		})
	}
}

// Every key this file writes carries the prefix the set is identified by, so a reader can find them
// all and none of them collides with another feature's.
func TestFabricNodeLabelKeys_ShareThePrefix(t *testing.T) {
	for _, key := range []string{NodeFabricDomainLabelKey, NodeFabricMembersLabelKey} {
		assert.True(t, strings.HasPrefix(key, FabricFeatureLabelPrefix), key)
		assert.NotEqual(t, FabricFeatureLabelPrefix, key, "the prefix is not a key")
	}
	assert.NotEqual(t, NodeFabricDomainLabelKey, NodeFabricMembersLabelKey)
}
