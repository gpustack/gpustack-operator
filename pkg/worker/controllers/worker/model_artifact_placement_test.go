package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const testArtifactDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// artifactFixture builds a ModelArtifact in team-a named qwen. resolved false leaves it as it is
// before its first resolution; ready false marks access lost after it.
func artifactFixture(claim string, resolved, ready bool) *workercore.ModelArtifact {
	ma := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"}}
	if claim != "" {
		ma.Spec.Source.PersistentVolumeClaim = &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: claim, Path: "qwen"}
	} else {
		ma.Spec.Source.HuggingFace = &workercore.ModelArtifactHubSource{
			Repository: "Qwen/Qwen2.5-72B-Instruct", Revision: "main", SecretRef: &core.LocalObjectReference{Name: "hf-token"},
		}
	}
	if !resolved {
		ModelArtifactConditionResolved.Unknown(ma, "Resolving", "the source has not answered yet")
		return ma
	}
	ma.Status.Resolved = &workercore.ModelArtifactResolved{ResolvedTime: meta.Now()}
	if claim == "" {
		ma.Status.Resolved.Revision = testArtifactRevision
		ma.Status.Resolved.ManifestDigest = testArtifactDigest
		ma.Status.Resolved.SizeBytes = 10 << 30
	}
	if ready {
		ModelArtifactConditionResolved.True(ma, "Resolved", "resolved")
	} else {
		ModelArtifactConditionResolved.False(ma, "AccessDenied", "the credential was revoked")
	}

	return ma
}

func claimFixture(phase core.PersistentVolumeClaimPhase, class string, modes ...core.PersistentVolumeAccessMode) *core.PersistentVolumeClaim {
	pvc := &core.PersistentVolumeClaim{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "models"},
		Spec:       core.PersistentVolumeClaimSpec{AccessModes: modes},
		Status:     core.PersistentVolumeClaimStatus{Phase: phase},
	}
	if class != "" {
		pvc.Spec.StorageClassName = ptr.To(class)
	}
	if phase == core.ClaimBound {
		pvc.Spec.VolumeName = "pv-models"
		pvc.Status.AccessModes = modes
	}

	return pvc
}

func volumeFixture(node string) *core.PersistentVolume {
	pv := &core.PersistentVolume{ObjectMeta: meta.ObjectMeta{Name: "pv-models"}}
	if node != "" {
		pv.Spec.NodeAffinity = &core.VolumeNodeAffinity{Required: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{{
			MatchExpressions: []core.NodeSelectorRequirement{{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: []string{node}}},
		}}}}
	}

	return pv
}

func classFixture(name, provisioner string, mode storage.VolumeBindingMode) *storage.StorageClass {
	return &storage.StorageClass{ObjectMeta: meta.ObjectMeta{Name: name}, Provisioner: provisioner, VolumeBindingMode: &mode}
}

func artifactDeploymentFixture(replicas int32) *workercore.ModelDeployment {
	return newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.KVCache = nil
		md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "qwen"}
		md.Spec.Roles[0].Replicas = replicas
	})
}

func listReplicas(t *testing.T, cli ctrlcli.Client) []core.Pod {
	t.Helper()
	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))

	return pods.Items
}

func TestModelDeploymentArtifactWait(t *testing.T) {
	cases := []struct {
		name       string
		replicas   int32
		objs       []ctrlcli.Object
		wantPods   int
		wantReason string
	}{
		{name: "a missing artifact creates nothing", replicas: 1, wantReason: "ArtifactNotFound"},
		{
			name: "an artifact before its first resolution creates nothing", replicas: 1,
			objs: []ctrlcli.Object{artifactFixture("", false, false)}, wantReason: "ArtifactNotResolved",
		},
		{
			name: "a resolved hub artifact creates the replicas", replicas: 2,
			objs: []ctrlcli.Object{artifactFixture("", true, true)}, wantPods: 2, wantReason: "Downloading",
		},
		{
			name: "a revoked hub artifact creates nothing", replicas: 1,
			objs: []ctrlcli.Object{artifactFixture("", true, false)}, wantReason: "ArtifactNotResolved",
		},
		{
			name: "a bound claim creates the replicas", replicas: 1,
			objs: []ctrlcli.Object{
				artifactFixture("models", true, true), claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1"),
			},
			wantPods: 1, wantReason: "WeightsNotMounted",
		},
		{
			name: "a pending claim with immediate binding creates nothing", replicas: 1,
			objs: []ctrlcli.Object{
				artifactFixture("models", true, true), claimFixture(core.ClaimPending, "nfs", core.ReadWriteMany),
				classFixture("nfs", "nfs.csi.gpustack.ai", storage.VolumeBindingImmediate),
			},
			wantReason: "ClaimNotBound",
		},
		{
			name: "a pending static local claim creates nothing", replicas: 1,
			objs: []ctrlcli.Object{
				artifactFixture("models", true, true), claimFixture(core.ClaimPending, "local", core.ReadWriteOnce),
				classFixture("local", "kubernetes.io/no-provisioner", storage.VolumeBindingWaitForFirstConsumer),
			},
			wantReason: "ClaimNotBound",
		},
		{
			name: "a pending claim a provisioner creates on the first Pod's node creates the replica", replicas: 1,
			objs: []ctrlcli.Object{
				artifactFixture("models", true, true), claimFixture(core.ClaimPending, "local-path", core.ReadWriteOnce),
				classFixture("local-path", "rancher.io/local-path", storage.VolumeBindingWaitForFirstConsumer),
			},
			wantPods: 1, wantReason: "WeightsNotMounted",
		},
		{
			name: "two replicas on a claim that is not shared create nothing", replicas: 2,
			objs: []ctrlcli.Object{
				artifactFixture("models", true, true), claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1"),
			},
			wantReason: "AccessModeConflict",
		},
		{
			name: "two replicas on a shared claim create both", replicas: 2,
			objs: []ctrlcli.Object{
				artifactFixture("models", true, true), claimFixture(core.ClaimBound, "nfs", core.ReadOnlyMany), volumeFixture(""),
			},
			wantPods: 2, wantReason: "WeightsNotMounted",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objs := append([]ctrlcli.Object{artifactDeploymentFixture(c.replicas), newRenderInstanceType()}, c.objs...)
			cli := newModelDeploymentClient(objs...)

			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			assert.Len(t, listReplicas(t, cli), c.wantPods)
			md := getModelDeployment(t, cli)
			assert.Equal(t, c.wantReason, ModelDeploymentConditionWeightsReady.GetReason(md))
			assert.Equal(t, "False", ModelDeploymentConditionWeightsReady.GetStatus(md))
			if c.wantPods == 0 {
				assert.Equal(t, ModelDeploymentConditionWeightsReady.GetMessage(md), md.Status.PhaseMessage,
					"a deployment waiting on its weights says why in its phase")
			}
		})
	}
}

func TestModelDeploymentArtifactNodeDelivery(t *testing.T) {
	filtered := func() *workercore.ModelArtifact {
		ma := artifactFixture("", true, true)
		ma.Spec.IgnorePatterns = []string{"original/"}
		return ma
	}
	driver := &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: "model.csi.gpustack.ai"}}
	cases := []struct {
		name         string
		delivery     string
		objs         []ctrlcli.Object
		wantPods     int
		wantReason   string
		wantDelivery workercore.ModelDeploymentModelDelivery
	}{
		{
			name: "Node delivery with the plugin creates the replicas", delivery: "Node",
			objs: []ctrlcli.Object{artifactFixture("", true, true), driver}, wantPods: 1, wantReason: "WeightsNotMounted",
			wantDelivery: workercore.ModelDeploymentModelDeliveryNode,
		},
		{
			name: "Node delivery without the plugin creates nothing", delivery: "Node",
			objs: []ctrlcli.Object{artifactFixture("", true, true)}, wantReason: "NodeDeliveryUnavailable",
			wantDelivery: workercore.ModelDeploymentModelDeliveryNode,
		},
		{
			name: "a filtered artifact under Node delivery creates the replicas", delivery: "Node",
			objs: []ctrlcli.Object{filtered(), driver}, wantPods: 1, wantReason: "WeightsNotMounted",
			wantDelivery: workercore.ModelDeploymentModelDeliveryNode,
		},
		{
			name: "a filtered artifact under Engine delivery creates nothing", delivery: "Engine",
			objs: []ctrlcli.Object{filtered(), driver}, wantReason: "FilterNeedsNodeDelivery",
			wantDelivery: workercore.ModelDeploymentModelDeliveryEngine,
		},
		{
			name: "Engine delivery ignores the plugin", delivery: "Engine",
			objs: []ctrlcli.Object{artifactFixture("", true, true), driver}, wantPods: 1, wantReason: "Downloading",
			wantDelivery: workercore.ModelDeploymentModelDeliveryEngine,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := modelArtifactDeliveryMode
			t.Cleanup(func() { modelArtifactDeliveryMode = orig })
			modelArtifactDeliveryMode = func(context.Context) string { return c.delivery }
			objs := append([]ctrlcli.Object{artifactDeploymentFixture(1), newRenderInstanceType()}, c.objs...)
			cli := newModelDeploymentClient(objs...)

			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			pods := listReplicas(t, cli)
			assert.Len(t, pods, c.wantPods)
			md := getModelDeployment(t, cli)
			assert.Equal(t, c.wantReason, ModelDeploymentConditionWeightsReady.GetReason(md))
			require.NotNil(t, md.Status.Model)
			assert.Equal(t, c.wantDelivery, md.Status.Model.Delivery)
			for _, pod := range pods {
				vol := findVolume(&pod, modelDeploymentModelVolumeName)
				if c.wantDelivery == workercore.ModelDeploymentModelDeliveryNode {
					require.NotNil(t, vol)
					require.NotNil(t, vol.CSI, "the node plugin's inline volume")
				} else {
					assert.Nil(t, vol)
				}
			}
		})
	}
}

// TestModelDeploymentArtifactDeliverySwitchRolls pins that switching the delivery Setting changes the
// replicas' fingerprint once: the render differs, so each replica is replaced like an image change.
func TestModelDeploymentArtifactDeliverySwitchRolls(t *testing.T) {
	fingerprint := func(delivery string) string {
		orig := modelArtifactDeliveryMode
		t.Cleanup(func() { modelArtifactDeliveryMode = orig })
		modelArtifactDeliveryMode = func(context.Context) string { return delivery }
		cli := newModelDeploymentClient(artifactDeploymentFixture(1), newRenderInstanceType(), artifactFixture("", true, true),
			&storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: "model.csi.gpustack.ai"}})
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		pods := listReplicas(t, cli)
		require.Len(t, pods, 1)
		return pods[0].Annotations[modelDeploymentPodSpecHashAnnotation]
	}
	engine, node := fingerprint("Engine"), fingerprint("Node")
	assert.NotEmpty(t, engine)
	assert.NotEqual(t, engine, node)
	assert.Equal(t, node, fingerprint("Node"), "the same delivery renders the same fingerprint")
}

func TestModelDeploymentArtifactWaitLeavesRunningReplicasAlone(t *testing.T) {
	cli := newModelDeploymentClient(artifactDeploymentFixture(2), newRenderInstanceType(), artifactFixture("", true, true))
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaNames(t, cli)
	require.Len(t, before, 2)

	// Access is revoked, and then the image is edited: every admitted replica is outdated, which
	// would roll one per pass if the replacement could be created.
	ma := new(workercore.ModelArtifact)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, ma))
	ModelArtifactConditionResolved.False(ma, "AccessDenied", "the credential was revoked")
	require.NoError(t, cli.Update(context.Background(), ma))
	md := getModelDeployment(t, cli)
	md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), md))
	standInForKueue(t, cli, true)

	for range 3 {
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err)
	}
	assert.Equal(t, before, replicaNames(t, cli), "no replica is deleted or rolled")

	// A scale-up creates nothing either.
	md = getModelDeployment(t, cli)
	md.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), md))
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, before, replicaNames(t, cli), "no replica is created")
	assert.Equal(t, "ArtifactNotResolved", ModelDeploymentConditionWeightsReady.GetReason(getModelDeployment(t, cli)))
}

// TestModelDeploymentArtifactWaitReportsTheRolloutHeldByWeights pins what the held rollout above says
// about its cause. The deployment declares no KV cache, so a message naming the cache connection
// would send the reader to a store this deployment never used.
func TestModelDeploymentArtifactWaitReportsTheRolloutHeldByWeights(t *testing.T) {
	cli := newModelDeploymentClient(artifactDeploymentFixture(2), newRenderInstanceType(), artifactFixture("", true, true))
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	ma := new(workercore.ModelArtifact)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, ma))
	ModelArtifactConditionResolved.False(ma, "AccessDenied", "the credential was revoked")
	require.NoError(t, cli.Update(context.Background(), ma))
	md := getModelDeployment(t, cli)
	md.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(context.Background(), md))
	standInForKueue(t, cli, true)

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	got := getModelDeployment(t, cli)
	require.Nil(t, got.Spec.KVCache, "the fixture must declare no KV cache for the message check to mean anything")
	assert.True(t, ModelDeploymentConditionReplicasUpToDate.IsFalse(got))
	assert.Equal(t, modelDeploymentReasonRolloutHeldByWeights, ModelDeploymentConditionReplicasUpToDate.GetReason(got))
	message := ModelDeploymentConditionReplicasUpToDate.GetMessage(got)
	assert.Contains(t, message, "2 of 2 replicas")
	assert.Contains(t, message, "WeightsReady", "the message names the condition that says what blocks the weights")
	assert.NotContains(t, message, "KV cache")
}

func TestModelDeploymentArtifactPlacement(t *testing.T) {
	cli := newModelDeploymentClient(
		artifactDeploymentFixture(1), newRenderInstanceType(), artifactFixture("models", true, true),
		claimFixture(core.ClaimBound, "local", core.ReadWriteOnce), volumeFixture("node-1"),
	)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	pods := listReplicas(t, cli)
	require.Len(t, pods, 1)
	na := pods[0].Spec.Affinity.NodeAffinity
	require.NotNil(t, na)
	assert.Equal(t, volumeFixture("node-1").Spec.NodeAffinity.Required, na.RequiredDuringSchedulingIgnoredDuringExecution)

	status := getModelDeployment(t, cli).Status.Model
	require.NotNil(t, status)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryPvc, status.Delivery)
	assert.Equal(t, "qwen", status.Artifact)
}

func TestModelDeploymentArtifactPlacementIsNotPartOfTheFingerprint(t *testing.T) {
	cli := newModelDeploymentClient(
		artifactDeploymentFixture(1), newRenderInstanceType(), artifactFixture("models", true, true),
		claimFixture(core.ClaimPending, "local-path", core.ReadWriteOnce),
		classFixture("local-path", "rancher.io/local-path", storage.VolumeBindingWaitForFirstConsumer),
	)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	before := replicaNames(t, cli)
	require.Len(t, before, 1)
	assert.Nil(t, listReplicas(t, cli)[0].Spec.Affinity, "nothing to inject before the claim binds")

	// The first Pod's node decided the binding; the PV now carries that node.
	require.NoError(t, cli.Delete(context.Background(), claimFixture(core.ClaimPending, "", core.ReadWriteOnce)))
	require.NoError(t, cli.Create(context.Background(), claimFixture(core.ClaimBound, "local-path", core.ReadWriteOnce)))
	require.NoError(t, cli.Create(context.Background(), volumeFixture("node-2")))
	standInForKueue(t, cli, true)

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, before, replicaNames(t, cli), "the replica created before the binding is not rolled after it")
}

func TestModelDeploymentArtifactPlacementCrossesExistingTerms(t *testing.T) {
	term := func(key, value string) core.NodeSelectorTerm {
		return core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{
			{Key: key, Operator: core.NodeSelectorOpIn, Values: []string{value}},
		}}
	}
	cases := []struct {
		name     string
		existing *core.NodeSelector
		pv       *core.NodeSelector
		want     *core.NodeSelector
	}{
		{
			name: "nothing to add", existing: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{term("a", "1")}},
			want: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{term("a", "1")}},
		},
		{
			name: "a Pod without affinity takes the PV's", pv: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{term("zone", "z1")}},
			want: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{term("zone", "z1")}},
		},
		{
			name:     "every existing term is crossed with every PV term",
			existing: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{term("a", "1"), term("a", "2")}},
			pv:       &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{term("zone", "z1")}},
			want: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{
				{MatchExpressions: append(term("a", "1").MatchExpressions, term("zone", "z1").MatchExpressions...)},
				{MatchExpressions: append(term("a", "2").MatchExpressions, term("zone", "z1").MatchExpressions...)},
			}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := &core.Pod{}
			if c.existing != nil {
				pod.Spec.Affinity = &core.Affinity{NodeAffinity: &core.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: c.existing}}
			}
			injectModelArtifactAffinity(pod, c.pv)
			assert.Equal(t, c.want, pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
		})
	}
}

func TestModelDeploymentWeightsReady(t *testing.T) {
	pod := func(ready, mounted bool) core.Pod {
		status := func(b bool) core.ConditionStatus {
			if b {
				return core.ConditionTrue
			}
			return core.ConditionFalse
		}
		return core.Pod{Status: core.PodStatus{Phase: core.PodRunning, Conditions: []core.PodCondition{
			// The Pod's own readiness is left False on purpose: a routing sidecar can hold it, and
			// the engine's container is what says the download is done.
			{Type: core.PodReady, Status: core.ConditionFalse},
			{Type: core.PodReadyToStartContainers, Status: status(mounted)},
		}, ContainerStatuses: []core.ContainerStatus{
			{Name: "routing-sidecar", Ready: false},
			{Name: modelDeploymentMainContainerName, Ready: ready},
		}}}
	}
	claim := &modelArtifactWeights{Render: testPvcArtifactRender(), Status: &workercore.ModelDeploymentModelStatus{Artifact: "qwen"}}
	engine := &modelArtifactWeights{Render: testEngineArtifactRender(), Status: &workercore.ModelDeploymentModelStatus{Artifact: "qwen"}}
	node := &modelArtifactWeights{Render: testNodeArtifactRender(), Status: &workercore.ModelDeploymentModelStatus{Artifact: "qwen"}}
	onNode := func(p core.Pod, name string) core.Pod {
		p.Spec.NodeName = name
		return p
	}
	stored := func(state workercore.NodeModelStoreModelState) *workercore.NodeModelStoreModel {
		m := &workercore.NodeModelStoreModel{Digest: testArtifactDigest, State: state}
		if state == workercore.NodeModelStoreModelStateFailed {
			m.Reason, m.Message = "IntegrityMismatch", "config.json: the content hashes to another digest"
		}
		return m
	}
	cases := []struct {
		name       string
		weights    *modelArtifactWeights
		pods       []core.Pod
		nodeModels map[string]*workercore.NodeModelStoreModel
		wantStatus string
		wantReason string
	}{
		{name: "no artifact", wantStatus: "True", wantReason: "NotApplicable"},
		{name: "blocked", weights: blockedModelArtifactWeights("ClaimNotBound", "x"), wantStatus: "False", wantReason: "ClaimNotBound"},
		{name: "a claim not yet mounted", weights: claim, pods: []core.Pod{pod(false, true), pod(false, false)}, wantStatus: "False", wantReason: "WeightsNotMounted"},
		{name: "a claim mounted everywhere", weights: claim, pods: []core.Pod{pod(false, true), pod(true, true)}, wantStatus: "True", wantReason: "Mounted"},
		{name: "a download still running", weights: engine, pods: []core.Pod{pod(true, true), pod(false, true)}, wantStatus: "False", wantReason: "Downloading"},
		{name: "a download done everywhere", weights: engine, pods: []core.Pod{pod(true, true)}, wantStatus: "True", wantReason: "Downloaded"},
		{
			name: "a node materializing", weights: node, pods: []core.Pod{onNode(pod(false, false), "n1")},
			nodeModels: map[string]*workercore.NodeModelStoreModel{"n1": stored(workercore.NodeModelStoreModelStateDownloading)},
			wantStatus: "False", wantReason: "Materializing",
		},
		{
			name: "a node that failed", weights: node,
			pods: []core.Pod{onNode(pod(false, false), "n1"), onNode(pod(false, false), "n2")},
			nodeModels: map[string]*workercore.NodeModelStoreModel{
				"n1": stored(workercore.NodeModelStoreModelStateDownloading), "n2": stored(workercore.NodeModelStoreModelStateFailed),
			},
			wantStatus: "False", wantReason: "MaterializationFailed",
		},
		{
			name: "a node that has not reported", weights: node, pods: []core.Pod{onNode(pod(false, false), "n1")},
			wantStatus: "False", wantReason: "WeightsNotMounted",
		},
		{
			name: "mounted on every node", weights: node, pods: []core.Pod{onNode(pod(false, true), "n1")},
			nodeModels: map[string]*workercore.NodeModelStoreModel{"n1": stored(workercore.NodeModelStoreModelStateReady)},
			wantStatus: "True", wantReason: "Mounted",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			holder := &workercore.ModelDeployment{}
			observeModelDeploymentWeights(holder, c.pods, c.weights, c.nodeModels)
			assert.Equal(t, c.wantStatus, ModelDeploymentConditionWeightsReady.GetStatus(holder))
			assert.Equal(t, c.wantReason, ModelDeploymentConditionWeightsReady.GetReason(holder))
			if c.weights == nil {
				assert.Nil(t, holder.Status.Model)
			}
		})
	}
}

func TestModelDeploymentWeightsReadyPhaseCarriesAnEviction(t *testing.T) {
	holder := &workercore.ModelDeployment{Status: workercore.ModelDeploymentStatus{Phase: ModelDeploymentPhaseStarting}}
	pods := []core.Pod{{
		ObjectMeta: meta.ObjectMeta{Name: "qwen-server-abc"},
		Status: core.PodStatus{
			Phase: core.PodSucceeded, Reason: "Evicted",
			Message: `Usage of EmptyDir volume "gpustack-model-cache" exceeds the limit "11Gi".`,
		},
	}}
	annotateModelDeploymentPhase(holder, pods)
	assert.Contains(t, holder.Status.PhaseMessage, "qwen-server-abc was evicted")
	assert.Contains(t, holder.Status.PhaseMessage, "exceeds the limit")
}

func TestModelArtifactNodeModels(t *testing.T) {
	nms := func(node string, models ...workercore.NodeModelStoreModel) *workercore.NodeModelStore {
		return &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: node}, Status: workercore.NodeModelStoreStatus{Models: models}}
	}
	onNode := func(node string) core.Pod { return core.Pod{Spec: core.PodSpec{NodeName: node}} }
	cli := newModelDeploymentClient(
		nms("n1", workercore.NodeModelStoreModel{Digest: testArtifactDigest, State: workercore.NodeModelStoreModelStateDownloading}),
		nms("n2", workercore.NodeModelStoreModel{Digest: "sha256:" + strings.Repeat("2", 64), State: workercore.NodeModelStoreModelStateReady}),
	)

	got, err := modelArtifactNodeModels(context.Background(), cli,
		[]core.Pod{onNode("n1"), onNode("n1"), onNode("n2"), onNode("n3"), onNode("")}, testArtifactDigest)
	require.NoError(t, err)
	require.NotNil(t, got["n1"])
	assert.Equal(t, workercore.NodeModelStoreModelStateDownloading, got["n1"].State)
	assert.Nil(t, got["n2"], "a node listing other content has no entry for this digest")
	assert.Nil(t, got["n3"], "a node without a NodeModelStore has none either")
	assert.NotContains(t, got, "", "an unscheduled Pod asks no node")
}

func TestMapModelDeploymentNodeModelStore(t *testing.T) {
	md := func(name string, delivery workercore.ModelDeploymentModelDelivery, digest string) *workercore.ModelDeployment {
		d := artifactDeploymentFixture(1)
		d.Name = name
		d.Status.Model = &workercore.ModelDeploymentModelStatus{Artifact: "qwen", ManifestDigest: digest, Delivery: delivery}
		return d
	}
	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(
		md("on-node", workercore.ModelDeploymentModelDeliveryNode, testArtifactDigest),
		md("engine", workercore.ModelDeploymentModelDeliveryEngine, testArtifactDigest),
		md("other", workercore.ModelDeploymentModelDeliveryNode, "sha256:"+strings.Repeat("2", 64)),
	)}
	store := &workercore.NodeModelStore{Status: workercore.NodeModelStoreStatus{Models: []workercore.NodeModelStoreModel{{Digest: testArtifactDigest}}}}

	reqs := r.mapModelDeploymentNodeModelStore(context.Background(), store)
	require.Len(t, reqs, 1)
	assert.Equal(t, "on-node", reqs[0].Name)
}

func TestNodeModelStoreModelsChanged(t *testing.T) {
	nms := func(state workercore.NodeModelStoreModelState, stored int64) *workercore.NodeModelStore {
		return &workercore.NodeModelStore{Status: workercore.NodeModelStoreStatus{
			Capacity: &workercore.NodeModelStoreCapacity{StoredBytes: stored},
			Models:   []workercore.NodeModelStoreModel{{Digest: testArtifactDigest, State: state}},
		}}
	}
	cases := []struct {
		name     string
		old, new *workercore.NodeModelStore
		want     bool
	}{
		{
			name: "a model's state changes", old: nms(workercore.NodeModelStoreModelStateDownloading, 1),
			new: nms(workercore.NodeModelStoreModelStateReady, 1), want: true,
		},
		{
			name: "only the capacity moves", old: nms(workercore.NodeModelStoreModelStateDownloading, 1),
			new: nms(workercore.NodeModelStoreModelStateDownloading, 2),
		},
		{
			name: "only a download's progress moves",
			old: func() *workercore.NodeModelStore {
				o := nms(workercore.NodeModelStoreModelStateDownloading, 1)
				o.Status.Models[0].DownloadedBytes = 10
				return o
			}(),
			new: func() *workercore.NodeModelStore {
				n := nms(workercore.NodeModelStoreModelStateDownloading, 1)
				n.Status.Models[0].DownloadedBytes = 60
				return n
			}(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nodeModelStoreModelsChanged().Update(ctrlevent.UpdateEvent{ObjectOld: c.old, ObjectNew: c.new})
			assert.Equal(t, c.want, got)
		})
	}
}

// delayRecorder is a work queue that records what is added after a delay; nothing else is called.
type delayRecorder struct {
	workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]
	added map[ctrlreconcile.Request]time.Duration
}

func (q *delayRecorder) AddAfter(req ctrlreconcile.Request, d time.Duration) { q.added[req] = d }

// TestModelDeploymentDeliveryWaitClears pins that a deployment held by the delivery wakes up when
// what holds it changes, though its own object does not: the plugin's CSIDriver appearing, and the
// delivery Setting switching to Node.
func TestModelDeploymentDeliveryWaitClears(t *testing.T) {
	filtered := func() *workercore.ModelArtifact {
		ma := artifactFixture("", true, true)
		ma.Spec.IgnorePatterns = []string{"original/"}
		return ma
	}
	driver := &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: "model.csi.gpustack.ai"}}
	settingsSecret := &core.Secret{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "gpustack-settings"}}
	cases := []struct {
		name        string
		artifact    *workercore.ModelArtifact
		delivery    string
		driver      bool
		heldBy      string
		fixDelivery string
		fixDriver   bool
		// viaSettings is whether the fix reaches the deployment through the Settings Secret's
		// delayed handler rather than the CSIDriver's mapping.
		viaSettings bool
	}{
		{
			name: "the plugin's CSIDriver appearing", artifact: artifactFixture("", true, true), delivery: "Node",
			heldBy: "NodeDeliveryUnavailable", fixDelivery: "Node", fixDriver: true,
		},
		{
			name: "the delivery switching to Node", artifact: filtered(), delivery: "Engine", driver: true,
			heldBy: "FilterNeedsNodeDelivery", fixDelivery: "Node", viaSettings: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := modelArtifactDeliveryMode
			t.Cleanup(func() { modelArtifactDeliveryMode = orig })
			modelArtifactDeliveryMode = func(context.Context) string { return c.delivery }
			objs := []ctrlcli.Object{artifactDeploymentFixture(1), newRenderInstanceType(), c.artifact, settingsSecret}
			if c.driver {
				objs = append(objs, driver)
			}
			other := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Name = "no-artifact"; md.Spec.KVCache = nil })
			cli := newModelDeploymentClient(append(objs, other)...)
			r := &ModelDeploymentReconciler{Client: cli}

			res, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)
			require.Equal(t, c.heldBy, ModelDeploymentConditionWeightsReady.GetReason(getModelDeployment(t, cli)))
			require.Empty(t, listReplicas(t, cli))
			assert.Equal(t, modelDeploymentDeliveryRecheck, res.RequeueAfter, "a held deployment is looked at again without an event")

			modelArtifactDeliveryMode = func(context.Context) string { return c.fixDelivery }
			var reqs []ctrlreconcile.Request
			if c.fixDriver {
				require.NoError(t, cli.Create(context.Background(), driver.DeepCopy()))
				reqs = r.mapModelDeploymentDelivery(context.Background(), driver)
			}
			if c.viaSettings {
				q := &delayRecorder{added: map[ctrlreconcile.Request]time.Duration{}}
				enqueueAfterSettingsRead(r.mapModelDeploymentDelivery).Update(context.Background(),
					ctrlevent.UpdateEvent{ObjectOld: settingsSecret, ObjectNew: settingsSecret}, q)
				for req, d := range q.added {
					assert.Greater(t, d, 30*time.Second, "enqueued after the Settings read cache expires")
					reqs = append(reqs, req)
				}
			}
			want := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}}
			require.Equal(t, []ctrlreconcile.Request{want}, reqs, "only the deployment on an artifact is woken")

			_, err = reconcileModelDeployment(t, cli)
			require.NoError(t, err)
			assert.Equal(t, "WeightsNotMounted", ModelDeploymentConditionWeightsReady.GetReason(getModelDeployment(t, cli)))
			assert.Len(t, listReplicas(t, cli), 1)
		})
	}
}
