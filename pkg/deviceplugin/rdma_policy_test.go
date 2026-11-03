package deviceplugin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// TestResolveRDMAPolicy pins the pure desired resolution over the policy list, the Node
// labels, and the internal verbs observation. Every case is deterministic: policy list
// order never matters, and the resolution binds the policy UID and generation, the Node,
// and the inventory revision.
func TestResolveRDMAPolicy(t *testing.T) {
	inventory := func() *device.RDMASnapshot {
		return &device.RDMASnapshot{
			Complete: true,
			Revision: "rev-1",
			Devices: []device.RDMADevice{
				{Name: "mlx5_0", Owner: device.RDMADeviceOwnerGPUStack},
				{Name: "mlx5_4", Owner: device.RDMADeviceOwnerGPUStack},
				{Name: "mlx5_5", Owner: device.RDMADeviceOwnerGPUStack},
				{Name: "mlx5_10", Owner: device.RDMADeviceOwnerGPUStack},
				{Name: "ibp1", Owner: device.RDMADeviceOwnerGPUStack, VirtualFunction: true},
				{Name: "efa0", Owner: device.RDMADeviceOwnerExternal, Driver: "efa"},
			},
		}
	}

	labels := map[string]string{"example.com/rdma-group": "compute-a"}
	node := RDMAPolicyNodeInput{Name: "node-a", UID: "node-uid-1", Labels: labels}

	policy := func(mutate func(*workercore.DevicesRDMASelectionPolicy)) workercore.DevicesRDMASelectionPolicy {
		p := workercore.DevicesRDMASelectionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			Spec:       workercore.DevicesRDMASelectionPolicySpec{NodeSelector: &metav1.LabelSelector{}},
		}
		if mutate != nil {
			mutate(&p)
		}
		return p
	}
	selectorPolicy := func(selector metav1.LabelSelector) workercore.DevicesRDMASelectionPolicy {
		return policy(func(p *workercore.DevicesRDMASelectionPolicy) { p.Spec.NodeSelector = &selector })
	}

	defaultSelected := []string{"ibp1", "mlx5_0", "mlx5_10", "mlx5_4", "mlx5_5"}

	tests := []struct {
		name    string
		inputs  func() RDMAPolicyInputs
		want    RDMAPolicyResolutionStatus
		wantRef *workercore.DevicesRDMASelectionPolicyReference
		wantSel []string
		// wantExcluded keys are device names, values the expected reason.
		wantExcluded   map[string]RDMAPolicyExclusionReason
		wantMessageHas string
	}{
		{
			name: "cache not synced is unknown even with matching policies",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Node: node, Policies: []workercore.DevicesRDMASelectionPolicy{policy(nil)}, Inventory: inventory()}
			},
			want: RDMAPolicyResolutionUnknown,
		},
		{
			name: "no inventory observation is unknown",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node}
			},
			want: RDMAPolicyResolutionUnknown,
		},
		{
			name: "incomplete inventory is unknown",
			inputs: func() RDMAPolicyInputs {
				snap := inventory()
				snap.Complete = false
				snap.Message = " enumeration failed"
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: snap}
			},
			want: RDMAPolicyResolutionUnknown,
		},
		{
			name: "zero matching policies resolves the default after sync",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory()}
			},
			want:         RDMAPolicyResolutionDefault,
			wantSel:      defaultSelected,
			wantExcluded: map[string]RDMAPolicyExclusionReason{"efa0": RDMAPolicyExclusionExternallyOwned},
		},
		{
			name: "successful empty inventory default resolves to no devices",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: &device.RDMASnapshot{Complete: true, Revision: "rev-empty"}}
			},
			want:    RDMAPolicyResolutionDefault,
			wantSel: []string{},
		},
		{
			name: "one matching policy excludes the named device",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(func(p *workercore.DevicesRDMASelectionPolicy) { p.Spec.Exclude = []string{"mlx5_5"} }),
				}}
			},
			want:         RDMAPolicyResolutionPolicy,
			wantRef:      &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantSel:      []string{"ibp1", "mlx5_0", "mlx5_10", "mlx5_4"},
			wantExcluded: map[string]RDMAPolicyExclusionReason{"efa0": RDMAPolicyExclusionExternallyOwned, "mlx5_5": RDMAPolicyExclusionExcludedByPolicy},
		},
		{
			name: "setting include and exclude together is invalid",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(func(p *workercore.DevicesRDMASelectionPolicy) {
						p.Spec.Include = []string{"mlx5_5"}
						p.Spec.Exclude = []string{"mlx5_4"}
					}),
				}}
			},
			want:           RDMAPolicyResolutionInvalid,
			wantRef:        &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantMessageHas: "include and exclude are mutually exclusive",
		},
		{
			name: "include names absent from a complete inventory are invalid",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(func(p *workercore.DevicesRDMASelectionPolicy) {
						p.Spec.Include = []string{"mlx5_1"}
					}),
				}}
			},
			want:           RDMAPolicyResolutionInvalid,
			wantRef:        &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantMessageHas: "mlx5_1",
		},
		{
			name: "exact matching does not bleed across name prefixes",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(func(p *workercore.DevicesRDMASelectionPolicy) {
						p.Spec.Include = []string{"mlx5_1", "mlx5_10"}
					}),
				}}
			},
			want:           RDMAPolicyResolutionInvalid,
			wantRef:        &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantMessageHas: "mlx5_1",
		},
		{
			name: "include of an externally owned known device is not invalid",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(func(p *workercore.DevicesRDMASelectionPolicy) {
						p.Spec.Include = []string{"efa0"}
					}),
				}}
			},
			want:    RDMAPolicyResolutionPolicy,
			wantRef: &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantSel: []string{},
			wantExcluded: map[string]RDMAPolicyExclusionReason{
				"efa0": RDMAPolicyExclusionExternallyOwned, "ibp1": RDMAPolicyExclusionExcludedByPolicy,
				"mlx5_0": RDMAPolicyExclusionExcludedByPolicy, "mlx5_10": RDMAPolicyExclusionExcludedByPolicy,
				"mlx5_4": RDMAPolicyExclusionExcludedByPolicy, "mlx5_5": RDMAPolicyExclusionExcludedByPolicy,
			},
		},
		{
			name: "exclude names absent from a complete inventory are invalid",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(func(p *workercore.DevicesRDMASelectionPolicy) { p.Spec.Exclude = []string{"nope0"} }),
				}}
			},
			want:           RDMAPolicyResolutionInvalid,
			wantRef:        &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantMessageHas: "nope0",
		},
		{
			name: "duplicate include names resolve once",
			inputs: func() RDMAPolicyInputs {
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(func(p *workercore.DevicesRDMASelectionPolicy) {
						p.Spec.Include = []string{"mlx5_4", "mlx5_4"}
					}),
				}}
			},
			want:    RDMAPolicyResolutionPolicy,
			wantRef: &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantSel: []string{"mlx5_4"},
			wantExcluded: map[string]RDMAPolicyExclusionReason{
				"efa0": RDMAPolicyExclusionExternallyOwned, "ibp1": RDMAPolicyExclusionExcludedByPolicy,
				"mlx5_0": RDMAPolicyExclusionExcludedByPolicy, "mlx5_10": RDMAPolicyExclusionExcludedByPolicy,
				"mlx5_5": RDMAPolicyExclusionExcludedByPolicy,
			},
		},
		{
			name: "two matching policies conflict even with identical rules",
			inputs: func() RDMAPolicyInputs {
				other := policy(nil)
				other.Name = "same-rules"
				other.UID = "uid-b"
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{
					policy(nil), other,
				}}
			},
			want: RDMAPolicyResolutionConflict,
		},
		{
			name: "a nil selector policy is a configuration error, not the default",
			inputs: func() RDMAPolicyInputs {
				p := policy(nil)
				p.Spec.NodeSelector = nil
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{p}}
			},
			want:    RDMAPolicyResolutionInvalid,
			wantRef: &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
		},
		{
			name: "a non-matching policy with overlapping unknown names never touches the node",
			inputs: func() RDMAPolicyInputs {
				mismatch := policy(func(p *workercore.DevicesRDMASelectionPolicy) {
					p.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"example.com/rdma-group": "storage-z"}}
					p.Spec.Include = []string{"ghost9"}
					p.Spec.Exclude = []string{"ghost9"}
				})
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{mismatch}}
			},
			want:         RDMAPolicyResolutionDefault,
			wantSel:      defaultSelected,
			wantExcluded: map[string]RDMAPolicyExclusionReason{"efa0": RDMAPolicyExclusionExternallyOwned},
		},
		{
			name: "selector change moves the node between policies",
			inputs: func() RDMAPolicyInputs {
				matching := selectorPolicy(metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "example.com/rdma-group",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{"compute-a", "compute-b"},
				}}})
				return RDMAPolicyInputs{Synced: true, Node: node, Inventory: inventory(), Policies: []workercore.DevicesRDMASelectionPolicy{matching}}
			},
			want:         RDMAPolicyResolutionPolicy,
			wantRef:      &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			wantSel:      defaultSelected,
			wantExcluded: map[string]RDMAPolicyExclusionReason{"efa0": RDMAPolicyExclusionExternallyOwned},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveRDMAPolicy(tt.inputs())

			assert.Equal(t, tt.want, got.Status)
			if tt.want == RDMAPolicyResolutionPolicy || tt.want == RDMAPolicyResolutionInvalid {
				require.NotNil(t, got.PolicyRef, "the bound policy is reported for policy resolutions")
				assert.Equal(t, *tt.wantRef, *got.PolicyRef)
				assert.Equal(t, node.Name, got.NodeName)
				assert.Equal(t, node.UID, got.NodeUID)
				assert.Equal(t, "rev-1", got.InventoryRevision)
			} else {
				assert.Nil(t, got.PolicyRef, "no policy reference for %v", tt.want)
			}
			if tt.wantSel != nil {
				assert.Equal(t, tt.wantSel, got.Selected, "selected HCA names, sorted")
			}
			if tt.wantExcluded != nil {
				gotExcluded := map[string]RDMAPolicyExclusionReason{}
				for _, excl := range got.Excluded {
					gotExcluded[excl.Name] = excl.Reason
				}
				assert.Equal(t, tt.wantExcluded, gotExcluded)
			}
			if tt.wantMessageHas != "" {
				assert.Contains(t, got.Message, tt.wantMessageHas)
			}
		})
	}
}

// TestResolveRDMAPolicyOrderIndependent pins that the policy list order never changes the
// resolution, including which UID wins a conflict report and every selected set.
func TestResolveRDMAPolicyOrderIndependent(t *testing.T) {
	inventory := &device.RDMASnapshot{
		Complete: true,
		Revision: "rev-1",
		Devices: []device.RDMADevice{
			{Name: "mlx5_0", Owner: device.RDMADeviceOwnerGPUStack},
			{Name: "efa0", Owner: device.RDMADeviceOwnerExternal},
		},
	}
	mk := func(name, uid string, gen int64, include []string) workercore.DevicesRDMASelectionPolicy {
		p := workercore.DevicesRDMASelectionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), Generation: gen},
			Spec:       workercore.DevicesRDMASelectionPolicySpec{NodeSelector: &metav1.LabelSelector{}},
		}
		if include != nil {
			p.Spec.Include = include
		}
		return p
	}
	node := RDMAPolicyNodeInput{Name: "node-a", UID: "node-uid-1"}

	first := ResolveRDMAPolicy(RDMAPolicyInputs{
		Synced: true, Node: node, Inventory: inventory,
		Policies: []workercore.DevicesRDMASelectionPolicy{
			mk("p1", "uid-1", 1, []string{"mlx5_0"}),
			mk("p2", "uid-2", 2, []string{"mlx5_0", "efa0"}),
		},
	})
	second := ResolveRDMAPolicy(RDMAPolicyInputs{
		Synced: true, Node: node, Inventory: inventory,
		Policies: []workercore.DevicesRDMASelectionPolicy{
			mk("p2", "uid-2", 2, []string{"mlx5_0", "efa0"}),
			mk("p1", "uid-1", 1, []string{"mlx5_0"}),
		},
	})

	assert.Equal(t, first.Status, second.Status)
	assert.Equal(t, RDMAPolicyResolutionConflict, first.Status)
	assert.Equal(t, first.Selected, second.Selected)
	assert.Equal(t, first.Excluded, second.Excluded)
	assert.Equal(t, first.Message, second.Message)
	assert.Nil(t, first.PolicyRef)
	assert.Nil(t, second.PolicyRef)
}

// rdmaFixtureInventory returns one complete observation with a PF/VF pair under one PCI
// parent, a prefix twin of the PF, and an externally owned device.
func rdmaFixtureInventory() *device.RDMASnapshot {
	return &device.RDMASnapshot{
		Complete: true,
		Revision: "rev-1",
		Devices: []device.RDMADevice{
			{Name: "mlx5_0", Owner: device.RDMADeviceOwnerGPUStack, PciBusID: "0000:0a:00.0"},
			{Name: "mlx5_4", Owner: device.RDMADeviceOwnerGPUStack, PciBusID: "0000:0b:00.0"},
			{Name: "mlx5_6", Owner: device.RDMADeviceOwnerGPUStack, PciBusID: "0000:0b:00.2", ParentPciBusID: "0000:0b:00.0", VirtualFunction: true},
			{Name: "mlx5_10", Owner: device.RDMADeviceOwnerGPUStack, PciBusID: "0000:0c:00.0"},
			{Name: "efa0", Owner: device.RDMADeviceOwnerExternal, Driver: "efa"},
		},
	}
}

func rdmaFixtureNode() RDMAPolicyNodeInput {
	return RDMAPolicyNodeInput{
		Name:   "node-a",
		UID:    "node-uid-1",
		Labels: map[string]string{"example.com/rdma-group": "compute-a"},
	}
}

// rdmaFixturePolicy returns one policy with the given identity and selector.
func rdmaFixturePolicy(name, uid string, generation int64, selector *metav1.LabelSelector) workercore.DevicesRDMASelectionPolicy {
	return workercore.DevicesRDMASelectionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), Generation: generation},
		Spec:       workercore.DevicesRDMASelectionPolicySpec{NodeSelector: selector},
	}
}

// assertRDMAResolution compares every observable field of one resolution.
func assertRDMAResolution(t *testing.T, want, got RDMAPolicyResolution) {
	t.Helper()
	assert.Equal(t, want.Status, got.Status)
	if want.PolicyRef == nil {
		assert.Nil(t, got.PolicyRef)
	} else {
		require.NotNil(t, got.PolicyRef)
		assert.Equal(t, *want.PolicyRef, *got.PolicyRef)
	}
	assert.Equal(t, want.Selected, got.Selected)
	assert.Equal(t, want.Excluded, got.Excluded)
	assert.Equal(t, want.Port, got.Port)
	assert.Equal(t, want.Message, got.Message)
	assert.Equal(t, want.NodeName, got.NodeName)
	assert.Equal(t, want.NodeUID, got.NodeUID)
	assert.Equal(t, want.InventoryRevision, got.InventoryRevision)
}

// rdmaPermutations returns every ordering of one policy list.
func rdmaPermutations(policies []workercore.DevicesRDMASelectionPolicy) [][]workercore.DevicesRDMASelectionPolicy {
	if len(policies) <= 1 {
		return [][]workercore.DevicesRDMASelectionPolicy{policies}
	}
	var out [][]workercore.DevicesRDMASelectionPolicy
	for i := range policies {
		rest := append(append([]workercore.DevicesRDMASelectionPolicy(nil), policies[:i]...), policies[i+1:]...)
		for _, tail := range rdmaPermutations(rest) {
			out = append(out, append([]workercore.DevicesRDMASelectionPolicy{policies[i]}, tail...))
		}
	}
	return out
}

// rdmaCopyPolicies deep-copies one policy list for input-mutation guards.
func rdmaCopyPolicies(policies []workercore.DevicesRDMASelectionPolicy) []workercore.DevicesRDMASelectionPolicy {
	out := make([]workercore.DevicesRDMASelectionPolicy, len(policies))
	for i := range policies {
		out[i] = *policies[i].DeepCopy()
	}
	return out
}

// TestResolveRDMAPolicyUnknownDeviceNames pins the inventory existence check over both
// filter lists: an unknown name is a configuration error on a complete inventory whatever
// list it sits in, an incomplete inventory stays unknown, and a known external device
// stays known while never allocatable.
func TestResolveRDMAPolicyUnknownDeviceNames(t *testing.T) {
	fixtureRef := workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3}

	tests := []struct {
		name        string
		include     []string
		exclude     []string
		incomplete  bool
		want        RDMAPolicyResolutionStatus
		wantMessage string
		wantSel     []string
	}{
		{
			name:        "unknown exclude name is invalid",
			exclude:     []string{"nope0"},
			want:        RDMAPolicyResolutionInvalid,
			wantMessage: "exclude names the inventory does not know: [nope0]",
		},
		{
			name:        "unknown include name is invalid",
			include:     []string{"mlx5_1"},
			want:        RDMAPolicyResolutionInvalid,
			wantMessage: "include names the inventory does not know: [mlx5_1]",
		},
		{
			name:        "both lists set report mutual exclusion first and the unknown sections after",
			include:     []string{"ghost9"},
			exclude:     []string{"nope0"},
			want:        RDMAPolicyResolutionInvalid,
			wantMessage: "include and exclude are mutually exclusive; include names the inventory does not know: [ghost9]; exclude names the inventory does not know: [nope0]",
		},
		{
			name:        "the same unknown name in both lists reports mutual exclusion and both unknown lists",
			include:     []string{"ghost9"},
			exclude:     []string{"ghost9"},
			want:        RDMAPolicyResolutionInvalid,
			wantMessage: "include and exclude are mutually exclusive; include names the inventory does not know: [ghost9]; exclude names the inventory does not know: [ghost9]",
		},
		{
			name:        "known names in both lists are invalid",
			include:     []string{"mlx5_4", "mlx5_0"},
			exclude:     []string{"mlx5_4"},
			want:        RDMAPolicyResolutionInvalid,
			wantMessage: "include and exclude are mutually exclusive",
		},
		{
			name:        "both lists set are invalid whatever the names are",
			include:     []string{"mlx5_4", "mlx5_0", "mlx5_4"},
			exclude:     []string{"mlx5_4", "mlx5_0"},
			want:        RDMAPolicyResolutionInvalid,
			wantMessage: "include and exclude are mutually exclusive",
		},
		{
			name:        "both lists set over an externally owned device are invalid",
			include:     []string{"efa0"},
			exclude:     []string{"efa0"},
			want:        RDMAPolicyResolutionInvalid,
			wantMessage: "include and exclude are mutually exclusive",
		},
		{
			name:    "include of a known external device stays known but never allocatable",
			include: []string{"efa0"},
			want:    RDMAPolicyResolutionPolicy,
			wantSel: []string{},
		},
		{
			name:    "exclude of a known external device is honored",
			exclude: []string{"efa0"},
			want:    RDMAPolicyResolutionPolicy,
			wantSel: []string{"mlx5_0", "mlx5_10", "mlx5_4", "mlx5_6"},
		},
		{
			name:        "incomplete inventory keeps both-list names unresolved",
			include:     []string{"ghost9"},
			exclude:     []string{"ghost9"},
			incomplete:  true,
			want:        RDMAPolicyResolutionUnknown,
			wantMessage: "the verbs inventory is missing or incomplete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})
			if tt.include != nil {
				p.Spec.Include = tt.include
			}
			p.Spec.Exclude = tt.exclude
			snap := rdmaFixtureInventory()
			if tt.incomplete {
				snap.Complete = false
			}
			got := ResolveRDMAPolicy(RDMAPolicyInputs{
				Synced:    true,
				Node:      rdmaFixtureNode(),
				Inventory: snap,
				Policies:  []workercore.DevicesRDMASelectionPolicy{p},
			})

			assert.Equal(t, tt.want, got.Status)
			if tt.wantMessage != "" {
				assert.Equal(t, tt.wantMessage, got.Message)
			}
			switch tt.want {
			case RDMAPolicyResolutionInvalid:
				// An error never falls back to the default set.
				require.NotNil(t, got.PolicyRef)
				assert.Equal(t, fixtureRef, *got.PolicyRef)
				assert.Nil(t, got.Selected)
				assert.Nil(t, got.Excluded)
			case RDMAPolicyResolutionPolicy:
				require.NotNil(t, got.PolicyRef)
				assert.Equal(t, fixtureRef, *got.PolicyRef)
				assert.Equal(t, tt.wantSel, got.Selected)
			default:
				assert.Nil(t, got.PolicyRef)
			}
		})
	}
}

// TestResolveRDMAPolicyMalformedSelectors pins deterministic handling of unreadable cached
// selectors: every policy is inspected, a malformed selector never counts as a match, two
// known matching policies stay a conflict beside any malformed selector, and the invalid
// report is identical under every policy list order.
func TestResolveRDMAPolicyMalformedSelectors(t *testing.T) {
	matchA := rdmaFixturePolicy("match-a", "uid-m1", 1, &metav1.LabelSelector{})
	matchB := rdmaFixturePolicy("match-b", "uid-m2", 1, &metav1.LabelSelector{})
	nilSelector := rdmaFixturePolicy("nil-sel", "uid-n1", 2, nil)
	invalidSelector := rdmaFixturePolicy("bad-sel", "uid-b1", 3, &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      "example.com/rdma-group",
			Operator: metav1.LabelSelectorOpIn,
		}},
	})
	twoBadKeys := rdmaFixturePolicy("two-bad", "uid-t1", 4, &metav1.LabelSelector{
		MatchLabels: map[string]string{"bad key one": "a", "bad key two": "b"},
	})

	tests := []struct {
		name             string
		policies         []workercore.DevicesRDMASelectionPolicy
		want             RDMAPolicyResolutionStatus
		wantRef          *workercore.DevicesRDMASelectionPolicyReference
		wantMessage      string
		wantMessageOrder []string
	}{
		{
			name:        "a single nil selector is invalid",
			policies:    []workercore.DevicesRDMASelectionPolicy{nilSelector},
			want:        RDMAPolicyResolutionInvalid,
			wantRef:     &workercore.DevicesRDMASelectionPolicyReference{Name: "nil-sel", UID: "uid-n1", Generation: 2},
			wantMessage: "policy nil-sel carries a nil nodeSelector",
		},
		{
			name:        "a single unreadable selector is invalid",
			policies:    []workercore.DevicesRDMASelectionPolicy{invalidSelector},
			want:        RDMAPolicyResolutionInvalid,
			wantRef:     &workercore.DevicesRDMASelectionPolicyReference{Name: "bad-sel", UID: "uid-b1", Generation: 3},
			wantMessage: "policy bad-sel carries an invalid nodeSelector",
		},
		{
			name:        "an invalid selector names the policy and field without the volatile detail",
			policies:    []workercore.DevicesRDMASelectionPolicy{twoBadKeys},
			want:        RDMAPolicyResolutionInvalid,
			wantRef:     &workercore.DevicesRDMASelectionPolicyReference{Name: "two-bad", UID: "uid-t1", Generation: 4},
			wantMessage: "policy two-bad carries an invalid nodeSelector",
		},
		{
			name:     "malformed selectors never count as matches",
			policies: []workercore.DevicesRDMASelectionPolicy{nilSelector, invalidSelector},
			want:     RDMAPolicyResolutionInvalid,
			wantRef:  &workercore.DevicesRDMASelectionPolicyReference{Name: "bad-sel", UID: "uid-b1", Generation: 3},
		},
		{
			name:             "one matching policy beside malformed selectors stays invalid",
			policies:         []workercore.DevicesRDMASelectionPolicy{matchA, nilSelector, invalidSelector},
			want:             RDMAPolicyResolutionInvalid,
			wantRef:          &workercore.DevicesRDMASelectionPolicyReference{Name: "bad-sel", UID: "uid-b1", Generation: 3},
			wantMessageOrder: []string{"bad-sel", "nil-sel"},
		},
		{
			name:        "two matching policies stay conflict beside malformed selectors",
			policies:    []workercore.DevicesRDMASelectionPolicy{matchA, matchB, nilSelector, invalidSelector},
			want:        RDMAPolicyResolutionConflict,
			wantMessage: "2 policies target this node; there is no merge or priority rule",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perms := rdmaPermutations(tt.policies)
			var first RDMAPolicyResolution
			for pi, perm := range perms {
				snapshot := rdmaCopyPolicies(perm)
				got := ResolveRDMAPolicy(RDMAPolicyInputs{
					Synced:    true,
					Node:      rdmaFixtureNode(),
					Inventory: rdmaFixtureInventory(),
					Policies:  perm,
				})
				assert.Equal(t, snapshot, perm, "resolution must not mutate the policy list")
				assert.Equal(t, tt.want, got.Status)
				if tt.wantRef == nil {
					assert.Nil(t, got.PolicyRef)
				} else {
					require.NotNil(t, got.PolicyRef)
					assert.Equal(t, *tt.wantRef, *got.PolicyRef)
				}
				if tt.wantMessage != "" {
					assert.Equal(t, tt.wantMessage, got.Message)
				}
				if pi == 0 {
					first = got
					continue
				}
				// The whole result, diagnostics included, is identical under any order.
				assert.Equal(t, first, got)
			}
			if tt.want == RDMAPolicyResolutionInvalid {
				assert.NotContains(t, first.Message, "targets", "a malformed selector never claims a match")
				last := -1
				for _, part := range tt.wantMessageOrder {
					idx := strings.Index(first.Message, part)
					require.Greater(t, idx, last, "diagnostics must appear in sorted policy order: %s", first.Message)
					last = idx
				}
			}
		})
	}
}

// TestResolveRDMAPolicyBindingUpdates pins the identity and selector inputs of the
// resolution through before/after pairs: a same-name policy replacement binds the new UID,
// a generation bump binds the new generation, node label changes flip the node between the
// policy and the default, and exact PF and VF names filter independently.
func TestResolveRDMAPolicyBindingUpdates(t *testing.T) {
	node := rdmaFixtureNode()
	defaultResolution := RDMAPolicyResolution{
		Status:            RDMAPolicyResolutionDefault,
		Selected:          []string{"mlx5_0", "mlx5_10", "mlx5_4", "mlx5_6"},
		Excluded:          []RDMAPolicyExclusion{{Name: "efa0", Reason: RDMAPolicyExclusionExternallyOwned}},
		NodeName:          node.Name,
		NodeUID:           node.UID,
		InventoryRevision: "rev-1",
	}
	fixtureRef := workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3}
	resolve := func(policies []workercore.DevicesRDMASelectionPolicy) RDMAPolicyResolution {
		t.Helper()
		return ResolveRDMAPolicy(RDMAPolicyInputs{
			Synced:    true,
			Node:      node,
			Inventory: rdmaFixtureInventory(),
			Policies:  policies,
		})
	}

	t.Run("same-name UID replacement binds the new UID", func(t *testing.T) {
		before := resolve([]workercore.DevicesRDMASelectionPolicy{rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})})
		after := resolve([]workercore.DevicesRDMASelectionPolicy{rdmaFixturePolicy("compute-hcas", "uid-a2", 1, &metav1.LabelSelector{})})

		require.Equal(t, RDMAPolicyResolutionPolicy, before.Status)
		require.NotNil(t, before.PolicyRef)
		assert.Equal(t, "uid-a", before.PolicyRef.UID)
		assert.Equal(t, int64(3), before.PolicyRef.Generation)
		require.NotNil(t, after.PolicyRef)
		assert.Equal(t, "uid-a2", after.PolicyRef.UID)
		assert.Equal(t, int64(1), after.PolicyRef.Generation)

		// The reference is the only difference: the rules themselves did not move.
		afterRef := *after.PolicyRef
		wantAfter := before
		wantAfter.PolicyRef = &afterRef
		assertRDMAResolution(t, wantAfter, after)
	})

	t.Run("same UID generation update binds the new generation", func(t *testing.T) {
		before := resolve([]workercore.DevicesRDMASelectionPolicy{rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})})
		after := resolve([]workercore.DevicesRDMASelectionPolicy{rdmaFixturePolicy("compute-hcas", "uid-a", 4, &metav1.LabelSelector{})})

		require.NotNil(t, before.PolicyRef)
		require.NotNil(t, after.PolicyRef)
		assert.Equal(t, "uid-a", after.PolicyRef.UID)
		assert.Equal(t, int64(4), after.PolicyRef.Generation)

		afterRef := *after.PolicyRef
		wantAfter := before
		wantAfter.PolicyRef = &afterRef
		assertRDMAResolution(t, wantAfter, after)
	})

	t.Run("node label change flips the node between policy and default", func(t *testing.T) {
		selectsComputeA := metav1.LabelSelector{
			MatchLabels: map[string]string{"example.com/rdma-group": "compute-a"},
		}
		policies := []workercore.DevicesRDMASelectionPolicy{
			rdmaFixturePolicy("compute-hcas", "uid-a", 3, &selectsComputeA),
		}

		matched := resolve(policies)
		wantMatched := defaultResolution
		wantMatched.Status = RDMAPolicyResolutionPolicy
		wantMatched.PolicyRef = &fixtureRef
		assertRDMAResolution(t, wantMatched, matched)

		// Relabel the node away from the selector: the same policy list stops targeting it.
		relabeled := node
		relabeled.Labels = map[string]string{"example.com/rdma-group": "compute-b"}
		after := ResolveRDMAPolicy(RDMAPolicyInputs{
			Synced:    true,
			Node:      relabeled,
			Inventory: rdmaFixtureInventory(),
			Policies:  policies,
		})
		assertRDMAResolution(t, defaultResolution, after)

		// And back: the resolution returns to the matched result in full.
		returned := ResolveRDMAPolicy(RDMAPolicyInputs{
			Synced:    true,
			Node:      node,
			Inventory: rdmaFixtureInventory(),
			Policies:  policies,
		})
		assert.Equal(t, matched, returned)
	})

	t.Run("PF include never captures its VF", func(t *testing.T) {
		pf := rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})
		pf.Spec.Include = []string{"mlx5_4"}
		got := resolve([]workercore.DevicesRDMASelectionPolicy{pf})

		want := defaultResolution
		want.Status = RDMAPolicyResolutionPolicy
		want.PolicyRef = &fixtureRef
		want.Selected = []string{"mlx5_4"}
		want.Excluded = []RDMAPolicyExclusion{
			{Name: "efa0", Reason: RDMAPolicyExclusionExternallyOwned},
			{Name: "mlx5_0", Reason: RDMAPolicyExclusionExcludedByPolicy},
			{Name: "mlx5_10", Reason: RDMAPolicyExclusionExcludedByPolicy},
			// The VF keeps its own name and stays outside the include.
			{Name: "mlx5_6", Reason: RDMAPolicyExclusionExcludedByPolicy},
		}
		assertRDMAResolution(t, want, got)
	})

	t.Run("PF exclude leaves its VF selected", func(t *testing.T) {
		pf := rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})
		pf.Spec.Exclude = []string{"mlx5_4"}
		got := resolve([]workercore.DevicesRDMASelectionPolicy{pf})

		want := defaultResolution
		want.Status = RDMAPolicyResolutionPolicy
		want.PolicyRef = &fixtureRef
		want.Selected = []string{"mlx5_0", "mlx5_10", "mlx5_6"}
		want.Excluded = []RDMAPolicyExclusion{
			{Name: "efa0", Reason: RDMAPolicyExclusionExternallyOwned},
			{Name: "mlx5_4", Reason: RDMAPolicyExclusionExcludedByPolicy},
		}
		assertRDMAResolution(t, want, got)
	})

	t.Run("VF include never captures its PF", func(t *testing.T) {
		vf := rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})
		vf.Spec.Include = []string{"mlx5_6"}
		got := resolve([]workercore.DevicesRDMASelectionPolicy{vf})

		want := defaultResolution
		want.Status = RDMAPolicyResolutionPolicy
		want.PolicyRef = &fixtureRef
		want.Selected = []string{"mlx5_6"}
		want.Excluded = []RDMAPolicyExclusion{
			{Name: "efa0", Reason: RDMAPolicyExclusionExternallyOwned},
			{Name: "mlx5_0", Reason: RDMAPolicyExclusionExcludedByPolicy},
			{Name: "mlx5_10", Reason: RDMAPolicyExclusionExcludedByPolicy},
			{Name: "mlx5_4", Reason: RDMAPolicyExclusionExcludedByPolicy},
		}
		assertRDMAResolution(t, want, got)
	})

	t.Run("VF exclude leaves its PF selected", func(t *testing.T) {
		vf := rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})
		vf.Spec.Exclude = []string{"mlx5_6"}
		got := resolve([]workercore.DevicesRDMASelectionPolicy{vf})

		want := defaultResolution
		want.Status = RDMAPolicyResolutionPolicy
		want.PolicyRef = &fixtureRef
		want.Selected = []string{"mlx5_0", "mlx5_10", "mlx5_4"}
		want.Excluded = []RDMAPolicyExclusion{
			{Name: "efa0", Reason: RDMAPolicyExclusionExternallyOwned},
			{Name: "mlx5_6", Reason: RDMAPolicyExclusionExcludedByPolicy},
		}
		assertRDMAResolution(t, want, got)
	})
}

// TestResolveRDMAPolicyPort pins the port restriction: the resolved port is copied only
// when every selected device carries that physical port, a selected device without it
// fails the node closed naming the devices, and devices outside the selected set —
// excluded or externally owned — are never asked to carry the port.
func TestResolveRDMAPolicyPort(t *testing.T) {
	node := rdmaFixtureNode()
	fixtureRef := workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3}
	inventory := func() *device.RDMASnapshot {
		return &device.RDMASnapshot{
			Complete: true,
			Revision: "rev-1",
			Devices: []device.RDMADevice{
				{
					Name: "mlx5_0", Owner: device.RDMADeviceOwnerGPUStack,
					Ports: []device.RDMAPort{{Number: 1, State: "ACTIVE", PhysicalState: "LinkUp"}},
				},
				{
					Name: "mlx5_1", Owner: device.RDMADeviceOwnerGPUStack,
					Ports: []device.RDMAPort{{Number: 2, State: "ACTIVE", PhysicalState: "LinkUp"}},
				},
				{
					Name: "mlx5_2", Owner: device.RDMADeviceOwnerGPUStack,
					Ports: []device.RDMAPort{{Number: 1, State: "DOWN", PhysicalState: "LinkDown"}},
				},
				{Name: "efa0", Owner: device.RDMADeviceOwnerExternal, Driver: "efa"},
			},
		}
	}
	policyWithPort := func(mutate func(*workercore.DevicesRDMASelectionPolicy)) workercore.DevicesRDMASelectionPolicy {
		p := rdmaFixturePolicy("compute-hcas", "uid-a", 3, &metav1.LabelSelector{})
		if mutate != nil {
			mutate(&p)
		}
		return p
	}
	resolve := func(p workercore.DevicesRDMASelectionPolicy) RDMAPolicyResolution {
		t.Helper()
		return ResolveRDMAPolicy(RDMAPolicyInputs{
			Synced:    true,
			Node:      node,
			Inventory: inventory(),
			Policies:  []workercore.DevicesRDMASelectionPolicy{p},
		})
	}

	t.Run("a port every selected device carries resolves and binds", func(t *testing.T) {
		p := policyWithPort(func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Exclude = []string{"mlx5_1"}
			p.Spec.Port = ptr.To(int32(1))
		})
		got := resolve(p)

		want := RDMAPolicyResolution{
			Status:    RDMAPolicyResolutionPolicy,
			PolicyRef: &workercore.DevicesRDMASelectionPolicyReference{Name: "compute-hcas", UID: "uid-a", Generation: 3},
			Selected:  []string{"mlx5_0", "mlx5_2"},
			Excluded: []RDMAPolicyExclusion{
				{Name: "efa0", Reason: RDMAPolicyExclusionExternallyOwned},
				{Name: "mlx5_1", Reason: RDMAPolicyExclusionExcludedByPolicy},
			},
			Port:              ptr.To(int32(1)),
			NodeName:          node.Name,
			NodeUID:           node.UID,
			InventoryRevision: "rev-1",
		}
		assertRDMAResolution(t, want, got)
	})

	t.Run("a selected device without the port fails the node closed", func(t *testing.T) {
		// mlx5_0 and mlx5_2 carry port 1 only; pinning port 2 leaves mlx5_1 selected but
		// the other two without the port.
		p := policyWithPort(func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Port = ptr.To(int32(2))
		})
		got := resolve(p)

		assert.Equal(t, RDMAPolicyResolutionInvalid, got.Status)
		require.NotNil(t, got.PolicyRef)
		assert.Equal(t, fixtureRef, *got.PolicyRef)
		assert.Nil(t, got.Selected, "an invalid resolution selects nothing")
		assert.Nil(t, got.Excluded, "an invalid resolution reports no exclusions")
		assert.Nil(t, got.Port, "an unenforceable port is not reported as applied")
		assert.Equal(t, "selected devices do not carry port 2: [mlx5_0 mlx5_2]", got.Message)
	})

	t.Run("a device outside the selected set is never asked to carry the port", func(t *testing.T) {
		// mlx5_0 is excluded and efa0 is externally owned; neither carries port 2, and
		// neither invalidates the policy, which mlx5_1 alone satisfies.
		p := policyWithPort(func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Exclude = []string{"mlx5_0", "mlx5_2"}
			p.Spec.Port = ptr.To(int32(2))
		})
		got := resolve(p)

		want := RDMAPolicyResolution{
			Status:    RDMAPolicyResolutionPolicy,
			PolicyRef: &fixtureRef,
			Selected:  []string{"mlx5_1"},
			Excluded: []RDMAPolicyExclusion{
				{Name: "efa0", Reason: RDMAPolicyExclusionExternallyOwned},
				{Name: "mlx5_0", Reason: RDMAPolicyExclusionExcludedByPolicy},
				{Name: "mlx5_2", Reason: RDMAPolicyExclusionExcludedByPolicy},
			},
			Port:              ptr.To(int32(2)),
			NodeName:          node.Name,
			NodeUID:           node.UID,
			InventoryRevision: "rev-1",
		}
		assertRDMAResolution(t, want, got)
	})

	t.Run("a port below one is a configuration error", func(t *testing.T) {
		p := policyWithPort(func(p *workercore.DevicesRDMASelectionPolicy) {
			p.Spec.Port = ptr.To(int32(0))
		})
		got := resolve(p)

		assert.Equal(t, RDMAPolicyResolutionInvalid, got.Status)
		require.NotNil(t, got.PolicyRef)
		assert.Equal(t, fixtureRef, *got.PolicyRef)
		assert.Nil(t, got.Selected)
		assert.Nil(t, got.Port)
		assert.Equal(t, "port 0 must be at least 1", got.Message)
	})

	t.Run("the default selection carries no port", func(t *testing.T) {
		got := ResolveRDMAPolicy(RDMAPolicyInputs{
			Synced:    true,
			Node:      node,
			Inventory: inventory(),
		})
		require.Equal(t, RDMAPolicyResolutionDefault, got.Status)
		assert.Nil(t, got.Port, "no policy, no port restriction")
	})
}
