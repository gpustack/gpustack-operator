package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

func TestDecodeTopologySnapshot(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		valid bool
	}{
		{name: "yaml", raw: "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: inventory-1\nnodes:\n  node-a:\n    topology.kubernetes.io/region: region-a\n" + "    topology.kubernetes.io/zone: zone-a\n", valid: true},
		{name: "json", raw: `{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"inventory-1","nodes":{}}`, valid: true},
		{name: "duplicate key", raw: "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: first\nrevision: second\nnodes: {}\n"},
		{name: "unknown field", raw: "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: inventory-1\nnodes: {}\nextra: value\n"},
		{name: "multiple documents", raw: "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: inventory-1\nnodes: {}\n---\napiVersion: topology.gpustack.ai/v1alpha1\nrevision: inventory-2\nnodes: {}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := decodeTopologySnapshot([]byte(tc.raw))
			assert.Equal(t, tc.valid, err == nil)
			if tc.valid {
				require.Equal(t, topologySnapshotAPIVersion, snapshot.APIVersion)
			}
		})
	}
}

func TestTopologySourceReconcileConfigMapAppliesSnapshot(t *testing.T) {
	source := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{Name: "inventory", UID: types.UID("source")}, Spec: workercore.TopologySourceSpec{
		NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/topology": "enabled"}},
		Levels:       []string{topologySourceRegionLabel, topologySourceZoneLabel},
		ConfigMap: &workercore.TopologySourceConfigMap{
			ConfigMapRef: workercore.TopologySourceObjectReference{Namespace: "gpustack-system", Name: "inventory"}, Key: "snapshot.yaml",
			MaxStaleness: meta.Duration{Duration: time.Minute},
		},
	}}
	configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "inventory"}, Data: map[string]string{"snapshot.yaml": "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: inventory-1\nnodes:\n  node-a:\n    topology.gpustack.ai/region: region-a\n    topology.gpustack.ai/zone: zone-a\n"}}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{"test.gpustack.ai/topology": "enabled"}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source, configMap, node).
		WithStatusSubresource(&workercore.TopologySource{}).Build()
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	r := &TopologySourceReconciler{Client: cli, Now: func() time.Time { return now }}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}})
	require.NoError(t, err)
	gotNode := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node.Name}, gotNode))
	assert.NotContains(t, gotNode.Labels, topologySourceRegionLabel)
	assert.NotContains(t, gotNode.Labels, topologySourceZoneLabel)
	feature := new(nfd.NodeFeature)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: kuberess.SystemNamespaceName, Name: topologySourceNodeFeatureName(source.UID, node.Name)}, feature))
	assert.Equal(t, map[string]string{topologySourceRegionLabel: "region-a", topologySourceZoneLabel: "zone-a"}, feature.Spec.Labels)
	assert.Equal(t, node.Name, feature.Labels[nfd.NodeFeatureObjNodeNameLabel])
	assert.Equal(t, []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}}},
		r.enqueueTopologySourcesWhenNodeFeatureChanged(context.Background(), feature))
	gotSource := new(workercore.TopologySource)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: source.Name}, gotSource))
	assert.Equal(t, "inventory-1", gotSource.Status.LastSuccessfulRevision)
	assert.True(t, TopologySourceConditionReady.IsTrue(gotSource))

	require.NoError(t, cli.Delete(context.Background(), configMap))
	now = now.Add(2 * time.Minute)
	_, err = r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}})
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node.Name}, gotNode))
	assert.NotContains(t, gotNode.Labels, topologySourceRegionLabel)
	assert.NotContains(t, gotNode.Labels, topologySourceZoneLabel)
	assert.True(t, apierrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: kuberess.SystemNamespaceName, Name: topologySourceNodeFeatureName(source.UID, node.Name)}, feature)))
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: source.Name}, gotSource))
	assert.True(t, TopologySourceConditionReady.IsFalse(gotSource))
	assert.Equal(t, "Expired", TopologySourceConditionReady.GetReason(gotSource))
}

func TestTopologySourceConflictedNodes(t *testing.T) {
	first := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{Name: "first", UID: types.UID("first")}, Spec: workercore.TopologySourceSpec{
		NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/topology": "enabled"}},
		Levels:       []string{core.LabelTopologyRegion},
		ConfigMap:    &workercore.TopologySourceConfigMap{},
	}}
	second := first.DeepCopy()
	second.Name = "second"
	second.UID = types.UID("second")
	node := core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{"test.gpustack.ai/topology": "enabled"}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(first, second).Build()
	r := &TopologySourceReconciler{Client: cli}

	conflicted, sources, err := r.topologySourceConflictedNodes(context.Background(), first, []core.Node{node})
	require.NoError(t, err)
	assert.Equal(t, 1, conflicted)
	assert.Equal(t, []string{"second"}, sources)
}

func TestTopologySourceStandardParentsAreReadOnly(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		valid    bool
	}{
		{name: "matching cloud parents", existing: map[string]string{core.LabelTopologyRegion: "region-a", core.LabelTopologyZone: "zone-a"}, valid: true},
		{name: "missing cloud parent", existing: map[string]string{core.LabelTopologyRegion: "region-a"}},
		{name: "different cloud parent", existing: map[string]string{core.LabelTopologyRegion: "region-a", core.LabelTopologyZone: "zone-b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{"test.gpustack.ai/topology": "enabled"}
			for key, value := range tc.existing {
				labels[key] = value
			}
			source := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{Name: "inventory", UID: "source"}, Spec: workercore.TopologySourceSpec{
				NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/topology": "enabled"}},
				Levels:       []string{core.LabelTopologyRegion, core.LabelTopologyZone, topologySourceRackLabel},
				ConfigMap: &workercore.TopologySourceConfigMap{ConfigMapRef: workercore.TopologySourceObjectReference{
					Namespace: kuberess.SystemNamespaceName, Name: "inventory",
				}, Key: "snapshot.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}},
			}}
			configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: kuberess.SystemNamespaceName, Name: "inventory"}, Data: map[string]string{
				"snapshot.yaml": "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: inventory-1\nnodes:\n  node-a:\n    topology.kubernetes.io/region: region-a\n    topology.kubernetes.io/zone: zone-a\n    topology.gpustack.ai/rack: rack-a\n",
			}}
			node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: labels}}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source, configMap, node).
				WithStatusSubresource(&workercore.TopologySource{}).Build()
			r := &TopologySourceReconciler{Client: cli}
			_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}})
			require.NoError(t, err)
			gotNode := new(core.Node)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node.Name}, gotNode))
			assert.Equal(t, labels, gotNode.Labels)
			feature := new(nfd.NodeFeature)
			err = cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: kuberess.SystemNamespaceName, Name: topologySourceNodeFeatureName(source.UID, node.Name)}, feature)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, map[string]string{topologySourceRackLabel: "rack-a"}, feature.Spec.Labels)
			} else {
				assert.True(t, apierrors.IsNotFound(err))
			}
		})
	}
}

func TestTopologySourceRepairsAndRemovesNodeFeature(t *testing.T) {
	source := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{Name: "inventory", UID: "source"}, Spec: workercore.TopologySourceSpec{
		NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/topology": "enabled"}},
		Levels:       []string{topologySourceRackLabel},
		ConfigMap: &workercore.TopologySourceConfigMap{ConfigMapRef: workercore.TopologySourceObjectReference{
			Namespace: kuberess.SystemNamespaceName, Name: "inventory",
		}, Key: "snapshot.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}},
	}}
	configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: kuberess.SystemNamespaceName, Name: "inventory"}, Data: map[string]string{
		"snapshot.yaml": "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: first\nnodes:\n  node-a:\n    topology.gpustack.ai/rack: rack-a\n",
	}}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{"test.gpustack.ai/topology": "enabled"}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source, configMap, node).
		WithStatusSubresource(&workercore.TopologySource{}).Build()
	r := &TopologySourceReconciler{Client: cli}
	request := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}}
	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	key := ctrlcli.ObjectKey{Namespace: kuberess.SystemNamespaceName, Name: topologySourceNodeFeatureName(source.UID, node.Name)}
	feature := new(nfd.NodeFeature)
	require.NoError(t, cli.Get(context.Background(), key, feature))
	feature.Spec.Labels[topologySourceRackLabel] = "manual-rack"
	feature.Spec.Features.Flags = map[string]nfd.FlagFeatureSet{"manual": {Elements: map[string]nfd.Nil{"flag": {}}}}
	feature.OwnerReferences = nil
	require.NoError(t, cli.Update(context.Background(), feature))
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), key, feature))
	assert.Equal(t, "rack-a", feature.Spec.Labels[topologySourceRackLabel])
	assert.Empty(t, feature.Spec.Features.Flags)
	assert.True(t, kubemeta.IsControlledBy(feature, source))
	other := source.DeepCopy()
	other.Name, other.UID = "other", "other-source"
	other.ResourceVersion = ""
	require.NoError(t, cli.Create(context.Background(), other))
	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	assert.True(t, apierrors.IsNotFound(cli.Get(context.Background(), key, feature)))
	gotSource := new(workercore.TopologySource)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: source.Name}, gotSource))
	assert.Equal(t, int32(1), gotSource.Status.MutatedNodes)
	require.NoError(t, cli.Delete(context.Background(), other))
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), key, feature))

	configMap.Data["snapshot.yaml"] = "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: second\nnodes: {}\n"
	require.NoError(t, cli.Update(context.Background(), configMap))
	feature.OwnerReferences = nil
	require.NoError(t, cli.Update(context.Background(), feature))
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(cli.Get(context.Background(), key, feature)))
	gotNode := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node.Name}, gotNode))
	assert.Equal(t, node.Labels, gotNode.Labels)
}

func TestTopologySourceSwitchToNodeLabelsRemovesPublishedFeatures(t *testing.T) {
	source := &workercore.TopologySource{ObjectMeta: meta.ObjectMeta{
		Name: "inventory", UID: "source", Finalizers: []string{topologySourceFinalizer},
	}, Spec: workercore.TopologySourceSpec{
		NodeSelector: meta.LabelSelector{},
		Levels:       []string{core.LabelTopologyRegion},
		NodeLabels:   &workercore.TopologySourceNodeLabels{},
	}}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{core.LabelTopologyRegion: "region-a"}}}
	feature := &nfd.NodeFeature{ObjectMeta: meta.ObjectMeta{
		Name: topologySourceNodeFeatureName(source.UID, node.Name), Namespace: kuberess.SystemNamespaceName,
		Labels: map[string]string{nfd.NodeFeatureObjNodeNameLabel: node.Name, topologySourceUIDLabel: string(source.UID)},
	}}
	feature.Spec = *nfd.NewNodeFeatureSpec()
	feature.Spec.Labels = map[string]string{topologySourceRackLabel: "rack-a"}
	kubemeta.ControlOnWithoutBlock(feature, source, workercore.SchemeGroupVersionKind("TopologySource"))
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source, node, feature).
		WithStatusSubresource(&workercore.TopologySource{}).Build()
	r := &TopologySourceReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}})
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(feature), new(nfd.NodeFeature))))
	gotSource := new(workercore.TopologySource)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: source.Name}, gotSource))
	assert.NotContains(t, gotSource.Finalizers, topologySourceFinalizer)
	gotNode := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node.Name}, gotNode))
	assert.Equal(t, node.Labels, gotNode.Labels)
}
