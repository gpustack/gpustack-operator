package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
)

// newGeneralNode builds a managed Node carrying the general(CPU-only) feature
// labels for a "generic-ln-x64" profile — enough for ExtractNodeResourceFlavors
// to emit exactly one NodeResourceFlavor.
func newGeneralNode(name string) *core.Node {
	nd := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				systemname.ManagedLabelKey: "true",
				core.LabelOSStable:         "linux",
				core.LabelArchStable:       "amd64",
			},
		},
	}
	base := nodefeature.GeneralFeatureLabelPrefix + nodefeature.ExtractGeneralNodeKey(nd)
	nd.Labels[base+".z-flavor"] = "4c-16g-32g"
	nd.Labels[base+".z-queue"] = "4c-16g"
	nd.Labels[base+".z-cohort"] = "4c-16g"
	nd.Labels[base+".cpu"] = "4"
	nd.Labels[base+".ram"] = "16"
	nd.Labels[base+".storage"] = "32"
	return nd
}

// newNodesResourceFlavor builds a "nodes" ResourceFlavor with the per-node
// notes the reconciler expects, optionally pre-marked as draining.
func newNodesResourceFlavor(name string, draining bool) *kueue.ResourceFlavor {
	rf := &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: name}}
	systemmeta.NoteResource(rf, "nodes", map[string]string{
		"acceleratable": "false",
		"manufacturer":  "generic",
		"accelerator":   "",
		"cpu":           "4",
		"ram":           "16",
		"localStorage":  "32",
	})
	if draining {
		rf.Annotations[_ResourceFlavorDrainAnnoKey] = "true"
	}
	return rf
}

func buildFlavorClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile).
		Build()
}

func reconcileFlavor(t *testing.T, cli ctrlcli.Client, name string) {
	t.Helper()
	r := &ResourceFlavorReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.NoError(t, err)
}

func TestResourceFlavorReconciler_Reconcile(t *testing.T) {
	// The flavor name depends only on the general profile, not the node name, so
	// derive it once from a probe node.
	flavorName := nodefeature.ExtractNodeResourceFlavors(newGeneralNode("probe"))[0].ProfileFlavor

	cases := []struct {
		name string

		withFlavor     bool
		flavorDraining bool
		withNode       bool
		nodeUnmanaged  bool // node present but gpustack.ai/managed=false

		wantExists   bool
		wantDraining bool // _ResourceFlavorDrainAnnoKey present
	}{
		{
			// An orphaned flavor (no Node references it) must be kept and marked
			// draining, never hard-deleted.
			name:         "orphan gets drain annotation",
			withFlavor:   true,
			wantExists:   true,
			wantDraining: true,
		},
		{
			// Re-reconciling an already-draining orphan is a no-op (still draining).
			name:           "orphan drain is idempotent",
			withFlavor:     true,
			flavorDraining: true,
			wantExists:     true,
			wantDraining:   true,
		},
		{
			// When a Node uses the flavor's profile again, the drain mark clears.
			name:           "active removes drain annotation",
			withFlavor:     true,
			flavorDraining: true,
			withNode:       true,
			wantExists:     true,
		},
		{
			// No flavor and no node: reconcile is a no-op, nothing is created.
			name: "not found is noop",
		},
		{
			// A Node introduces a flavor profile that has no ResourceFlavor yet: the
			// reconciler must CREATE the flavor, active (not draining).
			name:       "creates flavor for new node profile",
			withNode:   true,
			wantExists: true,
		},
		{
			// A Node exists but is no longer managed (gpustack.ai/managed=false), so
			// indexNodeByFlavorProfile excludes it: the flavor is orphaned and must be
			// marked draining. Guards the index's managed filter (the path a node
			// leaving management relies on); does NOT exercise the Node-watch predicate.
			name:          "unmanaged node drains flavor",
			withFlavor:    true,
			withNode:      true,
			nodeUnmanaged: true,
			wantExists:    true,
			wantDraining:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.withFlavor {
				objs = append(objs, newNodesResourceFlavor(flavorName, c.flavorDraining))
			}
			if c.withNode {
				nd := newGeneralNode("node-1")
				if c.nodeUnmanaged {
					nd.Labels[systemname.ManagedLabelKey] = "false"
				}
				objs = append(objs, nd)
			}
			cli := buildFlavorClient(objs...)

			reconcileFlavor(t, cli, flavorName)

			got := &kueue.ResourceFlavor{}
			err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: flavorName}, got)
			if !c.wantExists {
				assert.True(t, kerrors.IsNotFound(err),
					"flavor must not be created, got err=%v", err)
				return
			}
			require.NoError(t, err, "flavor must be kept/created")
			_, draining := got.Annotations[_ResourceFlavorDrainAnnoKey]
			assert.Equal(t, c.wantDraining, draining, "drain annotation presence")
		})
	}
}
