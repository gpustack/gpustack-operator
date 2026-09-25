package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

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
	cases := []struct {
		name       string
		weights    *modelArtifactWeights
		pods       []core.Pod
		wantStatus string
		wantReason string
	}{
		{name: "no artifact", wantStatus: "True", wantReason: "NotApplicable"},
		{name: "blocked", weights: blockedModelArtifactWeights("ClaimNotBound", "x"), wantStatus: "False", wantReason: "ClaimNotBound"},
		{name: "a claim not yet mounted", weights: claim, pods: []core.Pod{pod(false, true), pod(false, false)}, wantStatus: "False", wantReason: "WeightsNotMounted"},
		{name: "a claim mounted everywhere", weights: claim, pods: []core.Pod{pod(false, true), pod(true, true)}, wantStatus: "True", wantReason: "Mounted"},
		{name: "a download still running", weights: engine, pods: []core.Pod{pod(true, true), pod(false, true)}, wantStatus: "False", wantReason: "Downloading"},
		{name: "a download done everywhere", weights: engine, pods: []core.Pod{pod(true, true)}, wantStatus: "True", wantReason: "Downloaded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			holder := &workercore.ModelDeployment{}
			observeModelDeploymentWeights(holder, c.pods, c.weights)
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
