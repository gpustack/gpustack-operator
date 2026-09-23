package worker

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func TestTopologySourceReconcileNodeLabels(t *testing.T) {
	source := &workercore.TopologySource{
		ObjectMeta: meta.ObjectMeta{Name: "cloud"},
		Spec: workercore.TopologySourceSpec{
			NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/source": "cloud"}},
			Levels:       []string{core.LabelTopologyRegion, core.LabelTopologyZone},
			NodeLabels:   &workercore.TopologySourceNodeLabels{},
		},
	}
	matching := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "matching", Labels: map[string]string{
		"test.gpustack.ai/source": "cloud", core.LabelTopologyRegion: "region-a", core.LabelTopologyZone: "zone-a",
	}}}
	notMatching := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "other", Labels: map[string]string{
		"test.gpustack.ai/source": "other", core.LabelTopologyRegion: "region-b", core.LabelTopologyZone: "zone-b",
	}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(source, matching, notMatching).
		WithStatusSubresource(&workercore.TopologySource{}).
		Build()

	r := &TopologySourceReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}})
	require.NoError(t, err)

	gotSource := new(workercore.TopologySource)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(source), gotSource))
	assert.Equal(t, int64(0), gotSource.Status.ObservedGeneration)
	assert.Equal(t, "NodeLabels", gotSource.Status.SourceKind)
	assert.Equal(t, int32(1), gotSource.Status.SelectedNodes)
	assert.Zero(t, gotSource.Status.MutatedNodes)
	assert.Zero(t, gotSource.Status.ConflictedNodes)
	assert.True(t, TopologySourceConditionReady.IsTrue(gotSource))
	assert.True(t, TopologySourceConditionValid.IsTrue(gotSource))
	assert.True(t, TopologySourceConditionOwnershipConflict.IsFalse(gotSource))

	gotMatching := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(matching), gotMatching))
	assert.Equal(t, matching.Labels, gotMatching.Labels)
	gotOther := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(notMatching), gotOther))
	assert.Equal(t, notMatching.Labels, gotOther.Labels)
}

func TestValidateTopologySnapshot(t *testing.T) {
	source := &workercore.TopologySource{Spec: workercore.TopologySourceSpec{
		Levels: []string{core.LabelTopologyRegion, core.LabelTopologyZone, "topology.gpustack.ai/rack"},
	}}
	selected := map[string]struct{}{"node-a": {}, "node-b": {}}
	valid := topologySnapshot{
		APIVersion: topologySnapshotAPIVersion,
		Revision:   "inventory-1",
		Nodes: map[string]map[string]string{
			"node-a": {core.LabelTopologyRegion: "region-a", core.LabelTopologyZone: "zone-a", "topology.gpustack.ai/rack": "rack-a"},
			"node-b": {core.LabelTopologyRegion: "region-a", core.LabelTopologyZone: "zone-a", "topology.gpustack.ai/rack": "rack-b"},
		},
	}
	tests := []struct {
		name  string
		edit  func(*topologySnapshot)
		valid bool
	}{
		{name: "valid", edit: func(*topologySnapshot) {}, valid: true},
		{name: "unknown node", edit: func(snapshot *topologySnapshot) {
			snapshot.Nodes["unknown"] = map[string]string{core.LabelTopologyRegion: "region-a"}
		}},
		{name: "incomplete parent chain", edit: func(snapshot *topologySnapshot) {
			snapshot.Nodes["node-b"] = map[string]string{core.LabelTopologyZone: "zone-a"}
		}},
		{name: "non-tree hierarchy", edit: func(snapshot *topologySnapshot) {
			snapshot.Nodes["node-b"][core.LabelTopologyZone] = "zone-b"
			snapshot.Nodes["node-b"]["topology.gpustack.ai/rack"] = "rack-a"
		}},
		{name: "undeclared label", edit: func(snapshot *topologySnapshot) { snapshot.Nodes["node-a"]["topology.gpustack.ai/fabric"] = "fabric-a" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := valid
			snapshot.Nodes = make(map[string]map[string]string, len(valid.Nodes))
			for name, labels := range valid.Nodes {
				snapshot.Nodes[name] = maps.Clone(labels)
			}
			tc.edit(&snapshot)
			assert.Equal(t, tc.valid, validateTopologySnapshot(source, snapshot, selected) == nil)
		})
	}
}

func TestTopologySourceNodeWatchesSeparateReadOnlyAndWritingSources(t *testing.T) {
	first := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{Name: "first"}, Spec: workercore.TopologySourceSpec{NodeLabels: &workercore.TopologySourceNodeLabels{}}}
	second := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{Name: "second"}, Spec: workercore.TopologySourceSpec{ConfigMap: &workercore.TopologySourceConfigMap{}}}
	r := &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(first, second).Build()}

	reqs := r.enqueueTopologySourcesWhenNodeChanged(context.Background(), &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node"}})
	assert.Equal(t, []ctrlreconcile.Request{
		{NamespacedName: ctrlcli.ObjectKey{Name: "first"}},
	}, reqs)
	reqs = r.enqueueWritingTopologySourcesWhenNodeChanged(context.Background(), &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node"}})
	assert.Equal(t, []ctrlreconcile.Request{
		{NamespacedName: ctrlcli.ObjectKey{Name: "second"}},
	}, reqs)
}
