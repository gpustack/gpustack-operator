package nodefeature

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/device"
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
			name:   "no accelerators at all",
			groups: device.DevicesGroupList{},
			want:   map[string]string{},
		},
		{
			// A composite value the sanitizer would rewrite is withheld whole rather than published
			// truncated: a truncated domain names a DIFFERENT domain other nodes may be in.
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
