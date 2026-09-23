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

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func TestTopologyProfileUsesStableFNV64Name(t *testing.T) {
	levels := []string{core.LabelTopologyRegion, core.LabelTopologyZone, core.LabelHostname}
	profile := topologyProfile(levels)

	assert.Equal(t, "fnv64-570aeab478df5111", profile)
	assert.Equal(t, "gpustack-fnv64-570aeab478df5111", topologyName(profile))
	assert.Regexp(t, `^fnv64-[0-9a-f]{16}$`, profile)
	assert.NotEqual(t, profile, topologyProfile([]string{
		core.LabelTopologyZone, core.LabelTopologyRegion, core.LabelHostname,
	}), "ordered topology levels must remain part of the identity")
}

func TestNodeTopologyReconcileProfiles(t *testing.T) {
	tests := []struct {
		name       string
		nodeLabels map[string]string
		sources    []*workercore.TopologySource
		wantLevels []string
	}{
		{
			name: "cloud hierarchy",
			nodeLabels: map[string]string{
				"inventory": "cloud", core.LabelTopologyRegion: "region-a", core.LabelTopologyZone: "zone-a",
			},
			sources: []*workercore.TopologySource{readyNodeLabelSource("cloud", map[string]string{"inventory": "cloud"},
				core.LabelTopologyRegion, core.LabelTopologyZone)},
			wantLevels: []string{core.LabelTopologyRegion, core.LabelTopologyZone, core.LabelHostname},
		},
		{
			name: "partial hierarchy uses longest prefix",
			nodeLabels: map[string]string{
				"inventory": "rack", core.LabelTopologyRegion: "region-a", core.LabelTopologyZone: "zone-a",
			},
			sources: []*workercore.TopologySource{readyNodeLabelSource("rack", map[string]string{"inventory": "rack"},
				core.LabelTopologyRegion, core.LabelTopologyZone, "topology.gpustack.ai/rack")},
			wantLevels: []string{core.LabelTopologyRegion, core.LabelTopologyZone, core.LabelHostname},
		},
		{
			name:       "unclaimed node uses hostname only",
			nodeLabels: map[string]string{"inventory": "none"},
			wantLevels: []string{core.LabelHostname},
		},
		{
			name:       "overlapping sources use hostname only",
			nodeLabels: map[string]string{"inventory": "shared", core.LabelTopologyZone: "zone-a", "topology.gpustack.ai/rack": "rack-a"},
			sources: []*workercore.TopologySource{
				readyNodeLabelSource("zone", map[string]string{"inventory": "shared"}, core.LabelTopologyZone),
				readyNodeLabelSource("rack", map[string]string{"inventory": "shared"}, "topology.gpustack.ai/rack"),
			},
			wantLevels: []string{core.LabelHostname},
		},
		{
			name: "Topograph fabric tiers reverse closest first",
			nodeLabels: map[string]string{
				"inventory": "fabric", "fabric.topograph.run/tier-0": "leaf", "fabric.topograph.run/tier-1": "spine",
			},
			sources: []*workercore.TopologySource{readyNodeLabelSource("fabric", map[string]string{"inventory": "fabric"},
				"fabric.topograph.run/tier-0", "fabric.topograph.run/tier-1")},
			wantLevels: []string{"fabric.topograph.run/tier-1", "fabric.topograph.run/tier-0", core.LabelHostname},
		},
		{
			name: "accelerator domain remains opaque",
			nodeLabels: map[string]string{
				"inventory": "accelerator", "feature.gpustack.ai/fabric.domain": "nvlink-0-1-2",
			},
			sources: []*workercore.TopologySource{readyNodeLabelSource("accelerator", map[string]string{"inventory": "accelerator"},
				"feature.gpustack.ai/fabric.domain")},
			wantLevels: []string{"feature.gpustack.ai/fabric.domain", core.LabelHostname},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: tc.nodeLabels}}
			objects := []ctrlcli.Object{node}
			for _, source := range tc.sources {
				objects = append(objects, source)
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
			r := &NodeTopologyReconciler{Client: cli}

			_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: node.Name}})
			require.NoError(t, err)

			gotNode := new(core.Node)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(node), gotNode))
			profile := gotNode.Labels[TopologyProfileLabel]
			require.Equal(t, topologyProfile(tc.wantLevels), profile)
			topology := new(kueue.Topology)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: topologyName(profile)}, topology))
			assert.Equal(t, topologyLevelObjects(tc.wantLevels), topology.Spec.Levels)
		})
	}
}

func TestNodeTopologyRecreatesDeletedTopology(t *testing.T) {
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node).Build()
	r := &NodeTopologyReconciler{Client: cli}
	request := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: node.Name}}

	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	profile := topologyProfile([]string{core.LabelHostname})
	topology := new(kueue.Topology)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: topologyName(profile)}, topology))
	require.NoError(t, cli.Delete(context.Background(), topology))

	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: topologyName(profile)}, topology))
	assert.Equal(t, topologyLevelObjects([]string{core.LabelHostname}), topology.Spec.Levels)
}

func TestNodeTopologyRejectsImmutableTopologyDrift(t *testing.T) {
	levels := []string{core.LabelHostname}
	profile := topologyProfile(levels)
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{}}}
	topology := &kueue.Topology{
		ObjectMeta: meta.ObjectMeta{Name: topologyName(profile)},
		Spec: kueue.TopologySpec{Levels: topologyLevelObjects([]string{
			core.LabelTopologyZone, core.LabelHostname,
		})},
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node, topology).Build()
	r := &NodeTopologyReconciler{Client: cli}

	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: node.Name}})
	require.EqualError(t, err, "managed Topology \""+topology.Name+"\" has immutable level drift")
	gotNode := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(node), gotNode))
	assert.NotContains(t, gotNode.Labels, TopologyProfileLabel)
}

func TestTopologySourceNodeLabelsRejectsContradictoryHierarchy(t *testing.T) {
	source := readyNodeLabelSource("rack", map[string]string{"inventory": "rack"}, core.LabelTopologyZone, "topology.gpustack.ai/rack")
	source.Status.Conditions = nil
	nodes := []ctrlcli.Object{
		&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{"inventory": "rack", core.LabelTopologyZone: "zone-a", "topology.gpustack.ai/rack": "rack-shared"}}},
		&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-b", Labels: map[string]string{"inventory": "rack", core.LabelTopologyZone: "zone-b", "topology.gpustack.ai/rack": "rack-shared"}}},
	}
	objects := append([]ctrlcli.Object{source}, nodes...)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).
		WithStatusSubresource(&workercore.TopologySource{}).Build()
	r := &TopologySourceReconciler{Client: cli}

	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}})
	require.NoError(t, err)
	got := new(workercore.TopologySource)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(source), got))
	assert.True(t, TopologySourceConditionReady.IsFalse(got))
	assert.Equal(t, "InvalidHierarchy", TopologySourceConditionReady.GetReason(got))
}

func readyNodeLabelSource(name string, selector map[string]string, levels ...string) *workercore.TopologySource {
	source := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{Name: name}, Spec: workercore.TopologySourceSpec{
		NodeSelector: meta.LabelSelector{MatchLabels: selector}, Levels: levels, NodeLabels: &workercore.TopologySourceNodeLabels{},
	}}
	TopologySourceConditionReady.True(source, "Observed", "ready")
	return source
}
