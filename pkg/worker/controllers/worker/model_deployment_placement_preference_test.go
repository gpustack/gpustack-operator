package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// placementDeploymentClient holds a node-delivered deployment of one replica of two members, the
// plugin's CSIDriver, and the given objects.
func placementDeploymentClient(t *testing.T, objs ...ctrlcli.Object) ctrlcli.Client {
	t.Helper()
	orig := modelArtifactDeliveryMode
	t.Cleanup(func() { modelArtifactDeliveryMode = orig })
	modelArtifactDeliveryMode = func(context.Context) string { return "Node" }

	md := artifactDeploymentFixture(1)
	md.Spec.Roles[0].ReplicaSize = 2

	return newModelDeploymentClient(append([]ctrlcli.Object{
		md, newRenderInstanceType(), artifactFixture("", true, true),
		&storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}},
	}, objs...)...)
}

func podPreference(pod *core.Pod) []core.PreferredSchedulingTerm {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return nil
	}

	return pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
}

func TestModelDeploymentPlacementPreferenceMembersShareOneTerm(t *testing.T) {
	cli := placementDeploymentClient(t, append(hotNode("n1"), hotNode("n2")...)...)

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	pods := listReplicas(t, cli)
	require.Len(t, pods, 2)
	want := []core.PreferredSchedulingTerm{{
		Weight: modelPlacementPreferenceWeight,
		Preference: core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{
			{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: []string{"n1", "n2"}},
		}},
	}}
	for i := range pods {
		assert.Equal(t, want, podPreference(&pods[i]), "member %s", pods[i].Name)
		assert.NotContains(t, pods[i].Annotations, kueue.PodSetPreferredTopologyAnnotation)
	}
}

func TestModelDeploymentPlacementPreferenceIsNotPartOfTheFingerprint(t *testing.T) {
	fingerprints := func(objs ...ctrlcli.Object) map[string]string {
		cli := placementDeploymentClient(t, objs...)
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		got := map[string]string{}
		for _, pod := range listReplicas(t, cli) {
			got[pod.Name] = pod.Annotations[modelDeploymentPodSpecHashAnnotation]
		}
		return got
	}

	cold, hot := fingerprints(), fingerprints(hotNode("n1")...)
	require.Len(t, cold, 2)
	assert.Equal(t, cold, hot, "a replica's fingerprint is the same whether or not a node holds its weights")
}

func TestModelDeploymentPlacementPreferenceChangeRollsNothing(t *testing.T) {
	cli := placementDeploymentClient(t, hotNode("n1")...)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaNames(t, cli)
	require.Len(t, before, 2)
	standInForKueue(t, cli, true)

	// The digest finishes on a second node, and the first node's plugin leaves it.
	for _, obj := range hotNode("n2") {
		require.NoError(t, cli.Create(context.Background(), obj))
	}
	csiNode := new(storage.CSINode)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "n1"}, csiNode))
	csiNode.Spec.Drivers = nil
	require.NoError(t, cli.Update(context.Background(), csiNode))

	for range 3 {
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err)
	}
	assert.Equal(t, before, replicaNames(t, cli), "no replica is deleted or recreated")
	md := getModelDeployment(t, cli)
	assert.True(t, ModelDeploymentConditionReplicasUpToDate.IsTrue(md),
		"every replica reads current: %s %s", ModelDeploymentConditionReplicasUpToDate.GetReason(md),
		ModelDeploymentConditionReplicasUpToDate.GetMessage(md))
	for _, pod := range listReplicas(t, cli) {
		assert.Equal(t, []string{"n1"}, podPreference(&pod)[0].Preference.MatchExpressions[0].Values,
			"a running replica keeps the preference it was created with")
	}
}

func TestModelDeploymentPlacementPreferenceNotForAClaim(t *testing.T) {
	cli := newModelDeploymentClient(append(hotNode("n1"),
		artifactDeploymentFixture(1), newRenderInstanceType(), artifactFixture("models", true, true),
		claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1"),
	)...)

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	pods := listReplicas(t, cli)
	require.Len(t, pods, 1)
	assert.Equal(t, volumeFixture("node-1").Spec.NodeAffinity.Required,
		pods[0].Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
	assert.Empty(t, podPreference(&pods[0]))
}

// TestModelDeploymentPlacementPreferenceNoRenderAsksForAPreferredLevel pins the other half of the
// rule the injection skips on: a Pod this operator renders never carries the annotation, whatever the
// role's shape, so every node-delivered replica takes the preference.
func TestModelDeploymentPlacementPreferenceNoRenderAsksForAPreferredLevel(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*workercore.ModelDeployment)
	}{
		{name: "one member", mutate: func(*workercore.ModelDeployment) {}},
		{name: "several members", mutate: func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 3 }},
		{name: "a required level", mutate: func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].ReplicaSize = 2
			md.Spec.Roles[0].Topology = &workercore.ModelDeploymentRoleTopology{RequiredLevel: "topology.kubernetes.io/zone"}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := newRenderDeployment(c.mutate)
			role := &md.Spec.Roles[0]
			for member := range modelDeploymentRoleSize(role) {
				pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
					Deployment: md, Role: role, InstanceType: newRenderInstanceType(), Member: member,
				})
				require.NoError(t, err)
				assert.NotContains(t, pod.Annotations, kueue.PodSetPreferredTopologyAnnotation)
			}
		})
	}
}

// TestModelDeploymentPlacementPreferenceReadOnlyWhenCreating pins where the cost of the preference
// falls: it walks every NodeModelStore, so a pass that creates a replica reads them once, and a pass
// that creates nothing does not read them at all.
func TestModelDeploymentPlacementPreferenceReadOnlyWhenCreating(t *testing.T) {
	orig := modelArtifactDeliveryMode
	t.Cleanup(func() { modelArtifactDeliveryMode = orig })
	modelArtifactDeliveryMode = func(context.Context) string { return "Node" }

	md := artifactDeploymentFixture(1)
	md.Spec.Roles[0].ReplicaSize = 2
	lists := 0
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(append([]ctrlcli.Object{
			md, newRenderInstanceType(), artifactFixture("", true, true),
			&storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}},
		}, hotNode("n1")...)...).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			List: func(ctx context.Context, c ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
				if _, ok := list.(*workercore.NodeModelStoreList); ok {
					lists++
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, listReplicas(t, cli), 2)
	assert.Equal(t, 1, lists, "the pass that creates both members reads the stores once")

	standInForKueue(t, cli, true)
	lists = 0
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Zero(t, lists, "a pass that creates nothing reads no store")
}
