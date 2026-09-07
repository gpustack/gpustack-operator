package detector

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// nodeFeatureFixture builds a NodeFeature already owned by its node, so the only thing left that
// can move `skip` is the label set under test.
func nodeFeatureFixture(stored map[string]string) (*nfd.NodeFeature, *core.Node) {
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", UID: types.UID("node-1-uid")}}
	object := &nfd.NodeFeature{
		ObjectMeta: meta.ObjectMeta{Name: "node-1-gpustack-device-manager"},
		Spec:       nfd.NodeFeatureSpec{Labels: stored},
	}
	kubemeta.ControlOnWithoutBlock(object, node, core.SchemeGroupVersion.WithKind("Node"))
	return object, node
}

// TestNodeWideReadSynced pins which read failures may withhold a node-wide label.
//
// The two error kinds produce an identical object — this pass's own groups, nothing merged — so the
// distinction lives nowhere but here: one is a node with nothing stored yet, the other is a node
// this pass cannot see. Treating the second as the first removes labels on the strength of a read
// that never happened.
func TestNodeWideReadSynced(t *testing.T) {
	notFound := kerrors.NewNotFound(
		schema.GroupResource{Group: "worker.gpustack.ai", Resource: "devices"}, "node-1")

	assert.True(t, nodeWideReadSynced(nil), "a successful read")
	assert.True(t, nodeWideReadSynced(notFound), "the node's first pass, nothing stored yet")
	assert.False(t, nodeWideReadSynced(errors.New("etcdserver: request timed out")),
		"a read that did not happen cannot support withholding")
	assert.False(t, nodeWideReadSynced(kerrors.NewForbidden(
		schema.GroupResource{Group: "worker.gpustack.ai", Resource: "devices"}, "node-1",
		errors.New("nope"))), "another API error is not a NotFound either")
}

// TestNodeFeatureLabelsGroupSets pins WHICH group set each label set is reduced over.
//
// Reduced over the wrong one every label here still computes and the object still looks healthy, so
// nothing about a single-vendor node shows the difference — which is exactly why the choice needs an
// assertion rather than a reading. The mixed-vendor node below is the only shape that discriminates.
func TestNodeFeatureLabelsGroupSets(t *testing.T) {
	npu := device.DevicesGroup{
		ID: "0", Manufacturer: "ascend", Name: "Ascend950", Memory: 1024, Cores: 1,
		Accelerators: []device.Accelerator{{
			ID:       "npu-0",
			Topology: device.Topology{Fabric: &device.Fabric{Kind: "ub", ID: "7", MemberCount: 384}},
		}},
	}
	// The other manufacturer's group, seen only through the node-wide set. It reports no fabric,
	// which is what makes the node's domain ambiguous.
	gpu := device.DevicesGroup{
		ID: "0", Manufacturer: "nvidia", Name: "H100", Memory: 1024, Cores: 1,
		Accelerators: []device.Accelerator{{ID: "gpu-0"}},
	}
	own := device.DevicesGroupList{npu}
	nodeWide := device.DevicesGroupList{npu, gpu}

	t.Run("the fabric domain is reduced over the whole node", func(t *testing.T) {
		labels := nodeFeatureLabels(own, nodeWide, nil, false, true)

		// Over own alone this key would read `ub-7` — a promise about the GPU too, which is in no
		// such domain.
		assert.NotContains(t, labels, nodefeature.NodeFabricDomainLabelKey,
			"a mixed-vendor node has no single domain")
	})

	t.Run("the accelerator labels are reduced over this pass alone", func(t *testing.T) {
		labels := nodeFeatureLabels(own, nodeWide, nil, false, true)

		assert.Contains(t, labels, "acceleratable.feature.gpustack.ai/ascend",
			"this pass's own manufacturer is labelled")
		// Taken from the node-wide set instead, this pass would claim the other manufacturer's
		// hardware, and each pass would then publish keys it cannot maintain.
		assert.NotContains(t, labels, "acceleratable.feature.gpustack.ai/nvidia",
			"another manufacturer's accelerators are not this pass's to label")
	})

	t.Run("a node-wide read that failed publishes no fabric label", func(t *testing.T) {
		// Same inputs, gate down. The label is withheld rather than computed from a partial view,
		// and nodeFeatureAlignment leaves whatever was published before it standing.
		labels := nodeFeatureLabels(own, own, nil, false, false)

		assert.NotContains(t, labels, nodefeature.NodeFabricDomainLabelKey)
	})

	t.Run("a unanimous node publishes the domain", func(t *testing.T) {
		// The positive baseline: without it every assertion above would still pass if the fabric
		// labels were never produced at all.
		labels := nodeFeatureLabels(own, own, nil, false, true)

		assert.Equal(t, "ub-7", labels[nodefeature.NodeFabricDomainLabelKey])
		assert.Equal(t, "384", labels[nodefeature.NodeFabricMembersLabelKey])
	})
}

// TestNodeFeatureAlignmentWithholdsByRemoving pins the one branch that makes a withheld label mean
// anything.
//
// Every other label update here adds or overwrites, so not reporting a key leaves the key exactly
// as it was. For the RDMA set that is the whole gate: withholding the capable key is how a node
// with an unusable link stops being selected by a flavor that pins it, and a key that is never
// removed is never withheld. Without this branch the label would read `true` for as long as the
// object lives, while the same pass's inventory reported the link as broken.
func TestNodeFeatureAlignmentWithholdsByRemoving(t *testing.T) {
	const otherKey = "acceleratable.feature.gpustack.ai/nvidia-h100"

	testCases := []struct {
		name string
		// stored is what the object already carries; reported is what this pass produced.
		stored, reported map[string]string
		// syncInterfaces is false when the pass could not enumerate at all.
		syncInterfaces bool
		// syncFabric is false when the pass could not read the node's other manufacturers' groups.
		syncFabric bool
		wantSkip   bool
		wantStored map[string]string
	}{
		{
			// THE criterion. The link went down, so this pass reports no RDMA key, and the stored
			// one has to go.
			name: "a key that stops being reported is removed",
			stored: map[string]string{
				otherKey:                            "true",
				nodefeature.NodeRDMACapableLabelKey: "true",
				nodefeature.NodeRDMANumaLabelKey:    "0",
			},
			reported:       map[string]string{otherKey: "true"},
			syncInterfaces: true,
			wantSkip:       false,
			wantStored:     map[string]string{otherKey: "true"},
		},
		{
			// The converse, and equally a criterion: an unchanged pass must write nothing, or the
			// object is rewritten on every pass forever with correct labels in it throughout.
			name: "nothing changed, so nothing is written",
			stored: map[string]string{
				otherKey:                            "true",
				nodefeature.NodeRDMACapableLabelKey: "true",
			},
			reported: map[string]string{
				otherKey:                            "true",
				nodefeature.NodeRDMACapableLabelKey: "true",
			},
			syncInterfaces: true,
			wantSkip:       true,
			wantStored: map[string]string{
				otherKey:                            "true",
				nodefeature.NodeRDMACapableLabelKey: "true",
			},
		},
		{
			// A failed enumeration must not withhold anything. Removing the key here would take
			// the node out of scheduling on the strength of a read that never happened.
			name: "enumeration failed, so the previously published keys are kept",
			stored: map[string]string{
				otherKey:                            "true",
				nodefeature.NodeRDMACapableLabelKey: "true",
			},
			reported:       map[string]string{otherKey: "true"},
			syncInterfaces: false,
			wantSkip:       true,
			wantStored: map[string]string{
				otherKey:                            "true",
				nodefeature.NodeRDMACapableLabelKey: "true",
			},
		},
		{
			// The removal is scoped to one prefix. The accelerator keys beside it keep their
			// existing add-only behavior, which this change is not fixing — and a removal that
			// swept everything unreported would delete labels this pass does not speak for.
			name: "an unreported key outside the prefix is left alone",
			stored: map[string]string{
				otherKey:                            "true",
				"feature.gpustack.ai/acceleratable": "true",
			},
			reported:       map[string]string{nodefeature.NodeRDMACapableLabelKey: "true"},
			syncInterfaces: true,
			wantSkip:       false,
			wantStored: map[string]string{
				otherKey:                            "true",
				"feature.gpustack.ai/acceleratable": "true",
				nodefeature.NodeRDMACapableLabelKey: "true",
			},
		},
		{
			name:           "a value that changed is overwritten",
			stored:         map[string]string{nodefeature.NodeRDMADistanceLabelKey: "SYS"},
			reported:       map[string]string{nodefeature.NodeRDMADistanceLabelKey: "PIX"},
			syncInterfaces: true,
			wantSkip:       false,
			wantStored:     map[string]string{nodefeature.NodeRDMADistanceLabelKey: "PIX"},
		},
		{
			// The same criterion for the fabric set: a node moved out of its super pod stops
			// reporting the domain, and a key that outlives the membership names a super pod this
			// node has left — which a scheduler would read as co-location that no longer exists.
			name: "a fabric key that stops being reported is removed",
			stored: map[string]string{
				otherKey:                              "true",
				nodefeature.NodeFabricDomainLabelKey:  "ub-7",
				nodefeature.NodeFabricMembersLabelKey: "384",
			},
			reported:   map[string]string{otherKey: "true"},
			syncFabric: true,
			wantSkip:   false,
			wantStored: map[string]string{otherKey: "true"},
		},
		{
			// The fabric labels are reduced over the node-wide groups, so a failure to read them is
			// exactly the case where this pass cannot see enough of the node to withhold anything.
			name: "the node-wide read failed, so the fabric keys are kept",
			stored: map[string]string{
				nodefeature.NodeFabricDomainLabelKey: "ub-7",
			},
			reported:   map[string]string{},
			syncFabric: false,
			wantSkip:   true,
			wantStored: map[string]string{
				nodefeature.NodeFabricDomainLabelKey: "ub-7",
			},
		},
		{
			// THE discriminating case for the two gates being two. They are separate reads that fail
			// independently, so one succeeding must not license removing the other's keys: with a
			// single shared gate this pass would delete a fabric domain on the strength of having
			// enumerated the network interfaces, which says nothing about it.
			name: "each prefix is gated on its own read",
			stored: map[string]string{
				nodefeature.NodeRDMACapableLabelKey:  "true",
				nodefeature.NodeFabricDomainLabelKey: "ub-7",
			},
			reported:       map[string]string{},
			syncInterfaces: true,
			syncFabric:     false,
			wantSkip:       false,
			wantStored: map[string]string{
				nodefeature.NodeFabricDomainLabelKey: "ub-7",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stored, node := nodeFeatureFixture(tc.stored)
			alignment := nodeFeatureAlignment{
				expected:       &nfd.NodeFeature{Spec: nfd.NodeFeatureSpec{Labels: tc.reported}},
				node:           node,
				syncInterfaces: tc.syncInterfaces,
				syncFabric:     tc.syncFabric,
			}

			got, skip, err := alignment.apply(stored)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if skip != tc.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tc.wantSkip)
			}
			if len(got.Spec.Labels) != len(tc.wantStored) {
				t.Fatalf("labels = %v, want %v", got.Spec.Labels, tc.wantStored)
			}
			for k, v := range tc.wantStored {
				if got.Spec.Labels[k] != v {
					t.Errorf("label %s = %q, want %q", k, got.Spec.Labels[k], v)
				}
			}
		})
	}
}
