package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
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

func reconcileFlavor(t *testing.T, cli ctrlcli.Client, name string) {
	t.Helper()
	r := &ResourceFlavorReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.NoError(t, err)
}

func TestResourceFlavorReconciler_Reconcile_OrphanGetsDrainAnnotation(t *testing.T) {
	// An orphaned flavor (no Node references it) must be kept and marked draining,
	// never hard-deleted.
	flavorName := nodefeature.ExtractNodeResourceFlavors(newGeneralNode("probe"))[0].ProfileFlavor
	rf := newNodesResourceFlavor(flavorName, false)

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(rf).
		WithIndex(&core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile).
		Build()

	reconcileFlavor(t, cli, flavorName)

	got := &kueue.ResourceFlavor{}
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: flavorName}, got),
		"orphaned flavor must be kept, not deleted")
	assert.Equal(t, "true", got.Annotations[_ResourceFlavorDrainAnnoKey],
		"orphaned flavor must be marked draining")
}

func TestResourceFlavorReconciler_Reconcile_OrphanDrainIsIdempotent(t *testing.T) {
	// Re-reconciling an already-draining orphan is a no-op (still draining, no error).
	flavorName := nodefeature.ExtractNodeResourceFlavors(newGeneralNode("probe"))[0].ProfileFlavor
	rf := newNodesResourceFlavor(flavorName, true)

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(rf).
		WithIndex(&core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile).
		Build()

	reconcileFlavor(t, cli, flavorName)

	got := &kueue.ResourceFlavor{}
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: flavorName}, got))
	assert.Equal(t, "true", got.Annotations[_ResourceFlavorDrainAnnoKey])
}

func TestResourceFlavorReconciler_Reconcile_ActiveRemovesDrainAnnotation(t *testing.T) {
	// When a Node uses the flavor's profile again, the drain mark must be cleared.
	node := newGeneralNode("node-1")
	ndfs := nodefeature.ExtractNodeResourceFlavors(node)
	require.NotEmpty(t, ndfs)
	flavorName := ndfs[0].ProfileFlavor
	rf := newNodesResourceFlavor(flavorName, true)

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(node, rf).
		WithIndex(&core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile).
		Build()

	reconcileFlavor(t, cli, flavorName)

	got := &kueue.ResourceFlavor{}
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: flavorName}, got))
	_, has := got.Annotations[_ResourceFlavorDrainAnnoKey]
	assert.False(t, has, "drain annotation must be cleared once a node uses the flavor again")
}

func TestResourceFlavorReconciler_Reconcile_NotFoundIsNoop(t *testing.T) {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithIndex(&core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile).
		Build()

	reconcileFlavor(t, cli, "gpustack--generic-ln-x64-4c-16g-32g")
}

func TestResourceFlavorReconciler_Reconcile_CreatesFlavorForNewNodeProfile(t *testing.T) {
	// A Node introduces a flavor profile that has no ResourceFlavor yet: the
	// reconciler must CREATE the flavor, not skip it as "already deleted".
	node := newGeneralNode("node-1")
	ndfs := nodefeature.ExtractNodeResourceFlavors(node)
	require.NotEmpty(t, ndfs)
	flavorName := ndfs[0].ProfileFlavor

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(node). // no pre-existing ResourceFlavor
		WithIndex(&core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile).
		Build()

	reconcileFlavor(t, cli, flavorName)

	got := &kueue.ResourceFlavor{}
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: flavorName}, got),
		"a node-referenced flavor must be created when missing")
	_, draining := got.Annotations[_ResourceFlavorDrainAnnoKey]
	assert.False(t, draining, "a freshly created active flavor must not be marked draining")
}
