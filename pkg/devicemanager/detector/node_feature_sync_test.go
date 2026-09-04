package detector

import (
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

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
		wantSkip       bool
		wantStored     map[string]string
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stored, node := nodeFeatureFixture(tc.stored)
			alignment := nodeFeatureAlignment{
				expected:       &nfd.NodeFeature{Spec: nfd.NodeFeatureSpec{Labels: tc.reported}},
				node:           node,
				syncInterfaces: tc.syncInterfaces,
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
