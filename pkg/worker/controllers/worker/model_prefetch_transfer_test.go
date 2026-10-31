package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

func testSourceGrantBinding() *workercore.ModelStoreBinding {
	return &workercore.ModelStoreBinding{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "cache"},
		Spec: workercore.ModelStoreBindingSpec{
			StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "h100"}},
			Quota:     workercore.ModelStoreBindingQuota{Bytes: resource.MustParse("10Gi")},
			SourceMemory: &workercore.ModelStoreBindingSourceMemory{
				Bytes:         resource.MustParse("16Gi"),
				Registrations: 4,
				Sessions:      8,
			},
		},
	}
}

func testResolvedArtifactSized(name string, size int64) *workercore.ModelArtifact {
	artifact := testResolvedArtifact(name)
	artifact.Status.Resolved.SizeBytes = size

	return artifact
}

func testSourcePrefetch(name string, mutate func(*workercore.ModelPrefetch)) *workercore.ModelPrefetch {
	return testPrefetch(name, func(pf *workercore.ModelPrefetch) {
		pf.Spec.Source = &workercore.ModelPrefetchSource{}
		if mutate != nil {
			mutate(pf)
		}
	})
}

// testNodeModelStoreModelTransfer builds a node report carrying one generation entry for the digest.
func testNodeModelStoreModelTransfer(node, generation, digest string,
	state workercore.NodeModelStoreModelTransferGenerationState,
) *workercore.NodeModelStore {
	return &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: node},
		Status: workercore.NodeModelStoreStatus{
			ModelTransfer: &workercore.NodeModelStoreModelTransferStatus{
				Generations: []workercore.NodeModelStoreModelTransferGeneration{{
					Generation: generation, Digest: digest, State: state,
					ResidentBytes: 5 << 30, Registrations: 2, ActiveSessions: 1,
					RejectedAdmissions: 3,
				}},
			},
		},
	}
}

// The source-worker image Settings the model-transfer tests run against: the CUDA flavor for nodes that
// name no Ascend accelerator group, the CANN flavor for the ones that do.
const (
	testSourceWorkerCUDAImage = "registry.example/gpustack/model-transfer:cuda-test"
	testSourceWorkerCANNImage = "registry.example/gpustack/model-transfer:cann-test"
)

func testSourceDaemonSet(manufacturer, image string) *apps.DaemonSet {
	return &apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      "source-" + manufacturer,
			Namespace: setting.DelegatedSecretNamespace,
			UID:       types.UID("ds-" + manufacturer),
			Labels: map[string]string{
				"app.kubernetes.io/part-of":    "gpustack-operator",
				"app.kubernetes.io/component":  "model-transfer",
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/instance":   "test",
				"gpustack.ai/manufacturer":     manufacturer,
			},
		},
		Spec: apps.DaemonSetSpec{
			UpdateStrategy: apps.DaemonSetUpdateStrategy{Type: apps.OnDeleteDaemonSetStrategyType},
			Template: core.PodTemplateSpec{
				Spec: core.PodSpec{
					Containers: []core.Container{{Name: "worker", Image: image}},
				},
			},
		},
	}
}

func newModelTransferTestEnv(t *testing.T, objs ...ctrlcli.Object) (*ModelPrefetchReconciler, ctrlcli.Client) {
	t.Helper()
	hasVendorDS := map[string]bool{}
	for _, obj := range objs {
		if ds, ok := obj.(*apps.DaemonSet); ok && ds.Labels["app.kubernetes.io/component"] == "model-transfer" {
			hasVendorDS[ds.Labels["gpustack.ai/manufacturer"]] = true
		}
	}
	defaults := make([]ctrlcli.Object, 0, 2)
	if !hasVendorDS["nvidia"] {
		defaults = append(defaults, testSourceDaemonSet("nvidia", testSourceWorkerCUDAImage))
	}
	if !hasVendorDS["ascend"] {
		defaults = append(defaults, testSourceDaemonSet("ascend", testSourceWorkerCANNImage))
	}
	objs = append(defaults, objs...)

	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelPrefetch{}, &workercore.ModelStoreBinding{}, &core.Pod{}).
		WithObjects(objs...).Build()

	r := &ModelPrefetchReconciler{Client: cli, Now: func() time.Time { return testNow }}

	return r, cli
}

func listSourcePods(t *testing.T, cli ctrlcli.Client, ns, pf string) []core.Pod {
	t.Helper()
	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace(ns),
		ctrlcli.MatchingLabels{"worker.gpustack.ai/model-prefetch": pf}))

	return pods.Items
}

func getPrefetch(t *testing.T, cli ctrlcli.Client, ns, name string) *workercore.ModelPrefetch {
	t.Helper()
	pf := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: ns, Name: name}, pf))

	return pf
}

// TestModelPrefetchSourceAdmittedWithoutGrantSection pins the shape the node
// defaults own: a Binding without a sourceMemory section is a valid source
// shape — the node's own modelTransfer.source.* capacity governs what its
// sources draw — so the reconcile proceeds past the grant step instead of
// rendering SourceGrantMissing, and stops at the next admission step the
// fixture cannot satisfy (the unpinned authority).
func TestModelPrefetchSourceAdmittedWithoutGrantSection(t *testing.T) {
	binding := testSourceGrantBinding()
	binding.Spec.SourceMemory = nil

	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 5<<30),
		binding,
		testNodeModelStoreEntry("node-1", "Downloading", nil),
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pf := getPrefetch(t, cli, "team-a", "warm")
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.NotEqual(t, "SourceGrantMissing", progressing.Reason,
		"a Binding without a sourceMemory section is not a missing grant")
	assert.Equal(t, "SourceAuthorityUnknown", progressing.Reason,
		"the reconcile proceeds past the grant-less shape into the next admission step")
	assert.Nil(t, pf.Status.Transfer,
		"nothing was admitted, so the transfer view stays absent")
}

// TestModelPrefetchSourceRefusedWithoutPlacement pins the guard for an intent that names no
// target set: the derived-from-deployments default an ordinary prefetch keeps is not available
// to a source.
func TestModelPrefetchSourceRefusedWithoutPlacement(t *testing.T) {
	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 5<<30),
		testSourceGrantBinding(),
		testSourcePrefetch("warm", func(pf *workercore.ModelPrefetch) {
			pf.Spec.Placement = nil
		}),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	assert.Empty(t, sourcePodsOf(listSourcePods(t, cli, "team-a", "warm")))
	pf := getPrefetch(t, cli, "team-a", "warm")
	assert.Nil(t, pf.Status.Transfer)
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, "SourcePlacementMissing", progressing.Reason)
}

// sourcePodsOf returns only the source workers among the prefetch's Pods.
func sourcePodsOf(pods []core.Pod) []core.Pod {
	var out []core.Pod
	for i := range pods {
		if pods[i].Labels[modelTransferSourcePodLabel] != "" {
			out = append(out, pods[i])
		}
	}

	return out
}

// TestModelPrefetchSourceRefusedWithoutImage pins the off switch: an unconfigured source-worker
// image Setting renders no source worker and says so on the status, instead of rendering one
// that cannot pull its runtime — and the disk delivery keeps its own voice.
func TestModelPrefetchSourceRefusedWithoutImage(t *testing.T) {
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelPrefetch{}, &workercore.ModelStoreBinding{}, &core.Pod{}).
		WithObjects(
			&core.Secret{
				ObjectMeta: meta.ObjectMeta{
					Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName,
				},
			},
			testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
			testResolvedArtifactSized("model", 5<<30),
			testSourceGrantBinding(),
			testNodeModelStoreEntry("node-1", "Ready", nil),
			testSourcePrefetch("warm", nil),
		).Build()
	r := &ModelPrefetchReconciler{Client: cli, Now: func() time.Time { return testNow }}
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a"),
		ctrlcli.MatchingLabels{"worker.gpustack.ai/model-prefetch": "warm"}))
	assert.Empty(t, sourcePodsOf(pods.Items), "no image configured, no source worker")
	pf := getPrefetch(t, cli, "team-a", "warm")
	assert.Nil(t, pf.Status.Transfer, "nothing was admitted, so the transfer view stays absent")
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, "SourceWorkerImageMissing", progressing.Reason)
	available := testCondition(pf.Status.Conditions, ModelPrefetchConditionAvailable)
	require.NotNil(t, available)
	assert.Equal(t, "MinReadyReached", available.Reason,
		"the disk half is delivered and keeps its own answer beside the source refusal")
}

// TestModelPrefetchSourceRefusedAboveGrant pins that a checkpoint the grant cannot back is
// windowed rather than refused: the residency clamps to the grant's headroom and the reconcile
// proceeds past the grant check (the fixture's next blocker, the unpinned authority, is what
// renders), instead of the old SourceOverGrant refusal.
func TestModelPrefetchSourceRefusedAboveGrant(t *testing.T) {
	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 17<<30),
		testSourceGrantBinding(),
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pf := getPrefetch(t, cli, "team-a", "warm")
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.NotEqual(t, "SourceOverGrant", progressing.Reason,
		"an over-grant checkpoint is windowed, never refused at the grant check")
	assert.Equal(t, "SourceAuthorityUnknown", progressing.Reason,
		"the reconcile proceeds past the windowed grant into the next admission step")
}

// TestModelPrefetchSourceRefusedUnsized pins that a resolved digest with no measured size
// authorizes no allocation.
func TestModelPrefetchSourceRefusedUnsized(t *testing.T) {
	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testSourceGrantBinding(),
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	assert.Empty(t, sourcePodsOf(listSourcePods(t, cli, "team-a", "warm")))
	pf := getPrefetch(t, cli, "team-a", "warm")
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, "SourceSizeUnresolved", progressing.Reason)
}

// TestModelPrefetchDeliveredDiskKeepsAvailableThroughSourceRefusal pins the two voices: a
// source refusal speaks on Progressing and never drags a fully delivered disk half back to
// Available=False NotEnoughNodes. The refusal here is the unsized artifact, so the disk
// delivery's own answer is the only other voice on the status.
func TestModelPrefetchDeliveredDiskKeepsAvailableThroughSourceRefusal(t *testing.T) {
	old := hourAgo(1)

	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testSourceGrantBinding(),
		testNodeModelStoreEntry("node-1", "Ready", old),
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pf := getPrefetch(t, cli, "team-a", "warm")
	available := testCondition(pf.Status.Conditions, ModelPrefetchConditionAvailable)
	require.NotNil(t, available)
	assert.Equal(t, meta.ConditionTrue, available.Status,
		"the disk delivery is delivered, whatever the source half says")
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, meta.ConditionFalse, progressing.Status)
	assert.Equal(t, "SourceSizeUnresolved", progressing.Reason)
	assert.Nil(t, pf.Status.Transfer)
}

// TestModelPrefetchSourceRetainsTreeWithoutRetentionPinned pins the retention split: an admitted
// generation keeps its published tree pinned through the source grant alone — retention.pinned
// and allowPinned are the disk TTL's levers, not the source's.
func TestModelPrefetchSourceRetainsTreeWithoutRetentionPinned(t *testing.T) {
	old := hourAgo(100)
	nms := testNodeModelStoreEntry("node-1", "Ready", old, []string{})
	nms.Status.ModelTransfer = testNodeModelStoreModelTransfer("node-1", "7", testDigest,
		workercore.NodeModelStoreModelTransferGenerationStateDraining).Status.ModelTransfer

	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 5<<30),
		testSourceGrantBinding(),
		nms,
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	nms = new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
	assert.Contains(t, nms.Spec.Pinned, testDigest,
		"a draining generation's tree is retained through the source intent alone")
}

// TestModelPrefetchDeletingSourceRetainsTreeWhileGenerationDrains pins the deletion window: a
// prefetch being deleted stops speaking for itself, but its digest stays pinned while the node's
// admitted generation drains, whatever survives to reconcile the pin set.
func TestModelPrefetchDeletingSourceRetainsTreeWhileGenerationDrains(t *testing.T) {
	old := hourAgo(1)
	dying := testSourcePrefetch("dying", nil)
	dying.Finalizers = []string{"worker.gpustack.ai/test-holder"}
	now := meta.NewTime(testNow)
	dying.DeletionTimestamp = &now

	living := testSourcePrefetch("warm", nil)

	nms := testNodeModelStoreEntry("node-1", "Ready", old, []string{})
	nms.Status.ModelTransfer = testNodeModelStoreModelTransfer("node-1", "7", testDigest,
		workercore.NodeModelStoreModelTransferGenerationStateDraining).Status.ModelTransfer

	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 5<<30),
		testSourceGrantBinding(),
		nms,
		dying,
		living,
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	nms2 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms2))
	assert.Contains(t, nms2.Spec.Pinned, testDigest,
		"the tree a draining generation reads outlives the prefetch that asked for it")
}

// TestModelPrefetchEnqueuesForItsBinding pins the feed: a grant's growth or removal reaches the
// prefetches that reference it, and only theirs.
func TestModelPrefetchEnqueuesForItsBinding(t *testing.T) {
	otherBinding := testPrefetch("plain", func(pf *workercore.ModelPrefetch) {
		pf.Spec.BindingRef.Name = "cache-2"
	})
	r, cli := newModelTransferTestEnv(t,
		testSourcePrefetch("warm", nil),
		otherBinding,
	)
	elsewhere := testSourcePrefetch("elsewhere", nil)
	elsewhere.Namespace = "team-b"
	require.NoError(t, cli.Create(context.Background(), elsewhere))

	got := r.enqueuePrefetchesForBinding(context.Background(), testSourceGrantBinding())
	names := make([]string, 0, len(got))
	for _, req := range got {
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"warm"}, names,
		"only the prefetches bound to this binding wake")
}

// TestModelPrefetchPlacementEditRetargetsSource pins the drain: a placement edit moves the
// source's target set and stops observing the old node's draining generation.
func TestModelPrefetchPlacementEditRetargetsSource(t *testing.T) {
	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "b200"}),
		testResolvedArtifactSized("model", 5<<30),
		testSourceGrantBinding(),
		testNodeModelStoreModelTransfer("node-1", "7", testDigest, workercore.NodeModelStoreModelTransferGenerationStateDraining),
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")
	require.Len(t, listSourcePods(t, cli, "team-a", "warm"), 1)

	edited := getPrefetch(t, cli, "team-a", "warm")
	edited.Generation = 2
	edited.Spec.Placement = &workercore.ModelPrefetchPlacement{
		NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "b200"}},
	}
	require.NoError(t, cli.Update(context.Background(), edited))

	reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := listSourcePods(t, cli, "team-a", "warm")
	require.Len(t, pods, 1)
	for i := range pods {
		assert.Equal(t, "node-2", pods[i].Spec.NodeName,
			"the worker on the drained node is gone; the new node has its own")
	}
	pf := getPrefetch(t, cli, "team-a", "warm")
	assert.Nil(t, pf.Status.Transfer,
		"the draining generation on the departed node is no longer this prefetch's observation, "+
			"and nothing serves the new node yet")
	assert.Equal(t, int32(1), pf.Status.DesiredNodes)
}

// TestModelPrefetchSourceReconcileRepeatable pins idempotence: a second pass over a settled
// source prefetch writes nothing and renders nothing new.
func TestModelPrefetchSourceReconcileRepeatable(t *testing.T) {
	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 5<<30),
		testSourceGrantBinding(),
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")
	first := getPrefetch(t, cli, "team-a", "warm").Status.DeepCopy()
	firstPods := listSourcePods(t, cli, "team-a", "warm")

	reconcileModelPrefetch(t, r, "team-a", "warm")

	second := getPrefetch(t, cli, "team-a", "warm").Status
	assert.True(t, prefetchStatusEqual(first, &second), "the second pass changed nothing")
	assert.Len(t, listSourcePods(t, cli, "team-a", "warm"), len(firstPods))
}

// prefetchStatusEqual compares the figures two passes must agree on, transition times aside.
func prefetchStatusEqual(a, b *workercore.ModelPrefetchStatus) bool {
	if a.DesiredNodes != b.DesiredNodes || a.ReadyNodes != b.ReadyNodes ||
		a.DownloadingNodes != b.DownloadingNodes {
		return false
	}
	if (a.Transfer == nil) != (b.Transfer == nil) {
		return false
	}
	if a.Transfer != nil {
		if a.Transfer.ReservedBytes != b.Transfer.ReservedBytes ||
			len(a.Transfer.Generations) != len(b.Transfer.Generations) ||
			(a.Transfer.Limits == nil) != (b.Transfer.Limits == nil) {
			return false
		}
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		if a.Conditions[i].Type != b.Conditions[i].Type ||
			a.Conditions[i].Status != b.Conditions[i].Status ||
			a.Conditions[i].Reason != b.Conditions[i].Reason {
			return false
		}
	}

	return true
}

// TestModelPrefetchSourceStopsWithItsBinding pins deletion: a source intent whose Binding is
// gone renders nothing and says so; the grant lives and dies with the Binding.
func TestModelPrefetchSourceStopsWithItsBinding(t *testing.T) {
	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 5<<30),
		testSourcePrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	assert.Empty(t, sourcePodsOf(listSourcePods(t, cli, "team-a", "warm")))
	pf := getPrefetch(t, cli, "team-a", "warm")
	assert.Nil(t, pf.Status.Transfer)
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, "SourceGrantMissing", progressing.Reason)
}

// TestNodeReportChangedWakesOnModelTransfer pins the feed: a node's model-transfer report is a fact the prefetch
// and binding controllers read, so its change wakes them the way a disk report change does.
func TestNodeReportChangedWakesOnModelTransfer(t *testing.T) {
	before := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{Digest: testDigest, State: workercore.NodeModelStoreModelStateReady}},
		},
	}
	after := before.DeepCopy()
	after.Status.ModelTransfer = &workercore.NodeModelStoreModelTransferStatus{
		Generations: []workercore.NodeModelStoreModelTransferGeneration{{
			Generation: "7", Digest: testDigest,
			State: workercore.NodeModelStoreModelTransferGenerationStateServing,
		}},
	}
	assert.True(t, nodeReportChanged.Update(ctrlevent.UpdateEvent{
		ObjectOld: before, ObjectNew: after,
	}), "a source health change must wake the controllers that read it")

	same := before.DeepCopy()
	assert.False(t, nodeReportChanged.Update(ctrlevent.UpdateEvent{
		ObjectOld: before, ObjectNew: same,
	}), "an unchanged report wakes nobody")
}

// TestModelPrefetchSourceRetainsTreeWhileGenerationAdmitted pins the source-file retention: a
// published tree a live generation reads stays pinned past the retention TTL, while the same
// lapse without an admitted generation unpins as it always did.
func TestModelPrefetchSourceRetainsTreeWhileGenerationAdmitted(t *testing.T) {
	old := hourAgo(100)

	build := func(withGeneration bool) (*ModelPrefetchReconciler, ctrlcli.Client) {
		nms := testNodeModelStoreEntry("node-1", "Ready", old, []string{})
		if withGeneration {
			nms.Status.ModelTransfer = testNodeModelStoreModelTransfer("node-1", "7", testDigest,
				workercore.NodeModelStoreModelTransferGenerationStateServing).Status.ModelTransfer
		}
		binding := testSourceGrantBinding()
		allow := true
		binding.Spec.AllowPinned = &allow
		pf := testSourcePrefetch("warm", func(pf *workercore.ModelPrefetch) {
			ttl := meta.Duration{Duration: time.Hour}
			pf.Spec.Retention.TTLAfterLastUse = &ttl
			pf.Spec.Retention.Pinned = true
		})

		return newModelTransferTestEnv(t,
			testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
			testResolvedArtifactSized("model", 5<<30),
			binding,
			nms,
			pf,
		)
	}

	r, cli := build(true)
	reconcileModelPrefetch(t, r, "team-a", "warm")
	nms := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
	assert.Contains(t, nms.Spec.Pinned, testDigest,
		"the tree a live generation reads is retained past the TTL")

	r, cli = build(false)
	reconcileModelPrefetch(t, r, "team-a", "warm")
	nms = new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
	assert.NotContains(t, nms.Spec.Pinned, testDigest,
		"without an admitted generation the lapse unpins exactly as before")
}

func TestSourceWorkerImageSelection(t *testing.T) {
	cases := []struct {
		name          string
		manufacturers []string
		images        map[string]string
		want          string
	}{
		{"nvidia", []string{"nvidia-h100"}, map[string]string{"nvidia": "model-transfer:cuda12.9"}, "model-transfer:cuda12.9"},
		{"ascend", []string{"ascend-910b"}, map[string]string{"ascend": "model-transfer:cann9.2"}, "model-transfer:cann9.2"},
		{"amd vendor", []string{"amd-mi300"}, map[string]string{"amd": "model-transfer:rocm7.2", "nvidia": "model-transfer:cuda12.9"}, "model-transfer:rocm7.2"},
		{"missing vendor", []string{"amd-mi300"}, map[string]string{"nvidia": "model-transfer:cuda12.9"}, ""},
		{"host memory default", nil, map[string]string{"nvidia": "model-transfer:cuda12.9"}, "model-transfer:cuda12.9"},
		{"mixed vendors", []string{"nvidia-h100", "ascend-910b"}, map[string]string{"nvidia": "model-transfer:cuda12.9", "ascend": "model-transfer:cann9.2"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			node := core.Node{ObjectMeta: meta.ObjectMeta{Name: "source", Labels: map[string]string{}}}
			for _, manufacturer := range c.manufacturers {
				node.Labels[nodefeature.AcceleratableFeatureLabelPrefix+manufacturer] = "true"
			}
			objs := make([]ctrlcli.Object, 0, len(c.images))
			for vendor, img := range c.images {
				objs = append(objs, testSourceDaemonSet(vendor, img))
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
			r := &ModelPrefetchReconciler{Client: cli}
			plan, err := r.readSourceWorkerPlan(context.Background(), []core.Node{node}, []string{"source"})
			require.NoError(t, err)
			image, _, ok := plan.Resolve("source")
			assert.Equal(t, c.want, image)
			assert.Equal(t, c.want != "", ok)
		})
	}
}

func TestModelPrefetchSharedSourceKeepsDiskWarmup(t *testing.T) {
	r, cli := newModelTransferTestEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifactSized("model", 5<<30), testSourceGrantBinding(),
		testSourcePrefetch("warm", nil), testSourcePrefetch("warm-two", nil))
	for _, name := range []string{"warm", "warm-two"} {
		reconcileModelPrefetch(t, r, "team-a", name)
		pods := listSourcePods(t, cli, "team-a", name)
		require.Len(t, pods, 1)
		assert.Empty(t, sourcePodsOf(pods))
		assert.Equal(t, settings.ModelPrefetchWarmupImage.DefaultValue(), pods[0].Spec.Containers[0].Image)
		assert.Empty(t, pods[0].Spec.Containers[0].Resources.Requests)
	}
}

func TestModelPrefetchOldResolutionIsNotSourceHealth(t *testing.T) {
	artifact := testResolvedArtifactSized("model", 5<<30)
	artifact.Generation = 2
	artifact.Status.ObservedGeneration = 1
	r, cli := newModelTransferTestEnv(t, testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		artifact, testSourceGrantBinding(), testNodeModelStoreModelTransfer("node-1", "7", testDigest, workercore.NodeModelStoreModelTransferGenerationStateServing), testSourcePrefetch("warm", nil))
	reconcileModelPrefetch(t, r, "team-a", "warm")
	assert.Nil(t, getPrefetch(t, cli, "team-a", "warm").Status.Transfer)
}

func TestModelPrefetchCleansOnlyInertLegacyHolder(t *testing.T) {
	pf := testSourcePrefetch("warm", nil)
	pf.UID = "prefetch-1"
	holder := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "old", Namespace: "team-a", UID: "old-1", Labels: map[string]string{modelTransferSourcePodLabel: "warm"}, OwnerReferences: []meta.OwnerReference{{APIVersion: workercore.SchemeGroupVersion.String(), Kind: "ModelPrefetch", Name: pf.Name, UID: pf.UID, Controller: ptr.To(true)}}}, Spec: core.PodSpec{Containers: []core.Container{{Name: "source", Command: []string{"sh", "-c", "exec sleep infinity"}}}, Volumes: []core.Volume{{Name: "model", VolumeSource: core.VolumeSource{CSI: &core.CSIVolumeSource{Driver: modelstore.DriverName}}}}}}
	native := holder.DeepCopy()
	native.Name = "native"
	native.UID = "native-1"
	native.Spec.Containers[0].Command = []string{"python3", "-m", "native"}
	r, cli := newModelTransferTestEnv(t, pf, holder, native)
	require.NoError(t, r.removeLegacySourceHolders(context.Background(), pf))
	assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(holder), new(core.Pod))))
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(native), new(core.Pod)))
}

func testSourceAuthorityFixture(t *testing.T) (*ModelPrefetchReconciler, ctrlcli.Client, *workercore.ModelPrefetch, *core.Pod, *core.ConfigMap, *workercore.NodeModelStore) {
	t.Helper()
	pf := testSourcePrefetch("warm", nil)
	pf.UID = "prefetch-uid"
	artifact := testResolvedArtifactSized("model", 5<<30)
	artifact.Generation = 1
	artifact.Status.ObservedGeneration = 1
	binding := testSourceGrantBinding()
	binding.UID = "binding-uid"
	epoch := strings.Repeat("a", 64)
	pin, err := json.Marshal(broker.AuthorityPin{UID: "authority-uid", Epoch: epoch})
	require.NoError(t, err)
	binding.Annotations = map[string]string{broker.AuthorityAnnotation: string(pin)}
	entry := broker.ReservationEntry{ReservationID: "reservation", NodeScope: "nvidia", LedgerIdentity: "ledger", Generation: "gen-1", IntentUID: string(pf.UID), IntentGeneration: pf.Generation, Digest: testDigest, Namespace: pf.Namespace, NodeName: "node-1", ArtifactUID: string(artifact.UID), SourceGeneration: artifact.Generation, SourcePodUID: "source-pod-uid", Bytes: 5 << 30, Registrations: 2, State: "admitted"}
	doc, err := json.Marshal(map[string]any{"apiVersion": 1, "bindingUID": string(binding.UID), "epoch": epoch, "entries": []broker.ReservationEntry{entry}})
	require.NoError(t, err)
	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: setting.DelegatedSecretNamespace, Name: broker.AuthorityName(binding.UID), UID: "authority-uid"}, Data: map[string]string{"reservations.json": string(doc)}}
	labels := map[string]string{"app.kubernetes.io/part-of": "gpustack-operator", "app.kubernetes.io/component": "model-transfer", "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "test", "gpustack.ai/manufacturer": "nvidia"}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: setting.DelegatedSecretNamespace, UID: "ds-uid", Labels: labels}, Spec: apps.DaemonSetSpec{UpdateStrategy: apps.DaemonSetUpdateStrategy{Type: apps.OnDeleteDaemonSetStrategyType}, Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "worker", Image: testSourceWorkerCUDAImage}}}}}}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "source-pod", Namespace: setting.DelegatedSecretNamespace, UID: "source-pod-uid", Labels: labels, Annotations: map[string]string{transferWorkerInstance: "instance-1"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: ptr.To(true)}}}, Spec: core.PodSpec{NodeName: "node-1", Containers: []core.Container{{Name: "worker", Image: testSourceWorkerCUDAImage}}}}
	report := testNodeModelStoreModelTransfer("node-1", "gen-1", testDigest, workercore.NodeModelStoreModelTransferGenerationStateServing)
	g := &report.Status.ModelTransfer.Generations[0]
	g.SourcePodUID = string(pod.UID)
	g.SourceInstanceID = "instance-1"
	g.SourceGeneration = 1
	expiry := meta.NewTime(testNow.Add(time.Minute))
	g.LeaseExpiresAt = &expiry
	store := &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "h100"}, Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}}}}
	r, cli := newModelTransferTestEnv(t, testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}), store, artifact, binding, pf, ds, pod, cm, report)
	return r, cli, pf, pod, cm, report
}

func TestModelPrefetchSourceReadinessRequiresAuthorityAndLiveInstance(t *testing.T) {
	cases := []struct {
		name         string
		mutate       func(*testing.T, ctrlcli.Client, *core.Pod, *core.ConfigMap, *workercore.NodeModelStore)
		wantReady    bool
		wantTransfer bool
	}{
		{name: "matched", wantReady: true, wantTransfer: true},
		{name: "store no longer covers source", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			store := new(workercore.ModelStore)
			require.NoError(t, c.Get(context.Background(), ctrlcli.ObjectKey{Name: "h100"}, store))
			store.Spec.NodeSelector.MatchLabels["pool"] = "other"
			require.NoError(t, c.Update(context.Background(), store))
		}},
		{name: "store deleted", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			require.NoError(t, c.Delete(context.Background(), &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "h100"}}))
		}},
		{name: "stale instance", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, p *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			p.Annotations[transferWorkerInstance] = "instance-2"
			require.NoError(t, c.Update(context.Background(), p))
		}},
		{name: "image mismatch", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, p *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			p.Spec.Containers[0].Image = "source:old"
			require.NoError(t, c.Update(context.Background(), p))
		}},
		{name: "drain requested", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, p *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			p.Annotations[transferDrainFor] = "source:new"
			require.NoError(t, c.Update(context.Background(), p))
		}},
		{name: "expired lease", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, _ *core.ConfigMap, n *workercore.NodeModelStore) {
			expired := meta.NewTime(testNow.Add(-time.Second))
			n.Status.ModelTransfer.Generations[0].LeaseExpiresAt = &expired
			require.NoError(t, c.Update(context.Background(), n))
		}},
		{name: "artifact revision stale", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, _ *core.ConfigMap, n *workercore.NodeModelStore) {
			n.Status.ModelTransfer.Generations[0].SourceGeneration = 2
			require.NoError(t, c.Update(context.Background(), n))
		}},
		{name: "removed image", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			require.NoError(t, c.Delete(context.Background(), &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Namespace: setting.DelegatedSecretNamespace, Name: "source"}}))
		}},
		{name: "pod disappeared", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, p *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			require.NoError(t, c.Delete(context.Background(), p))
		}},
		{name: "old owner", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, p *core.Pod, _ *core.ConfigMap, _ *workercore.NodeModelStore) {
			p.OwnerReferences[0].UID = "replaced-ds"
			require.NoError(t, c.Update(context.Background(), p))
		}},
		{name: "report names old pod", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, _ *core.ConfigMap, n *workercore.NodeModelStore) {
			n.Status.ModelTransfer.Generations[0].SourcePodUID = "old-pod"
			require.NoError(t, c.Update(context.Background(), n))
		}},
		{name: "missing lease", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, _ *core.ConfigMap, n *workercore.NodeModelStore) {
			n.Status.ModelTransfer.Generations[0].LeaseExpiresAt = nil
			require.NoError(t, c.Update(context.Background(), n))
		}},
		{name: "authority missing", mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, cm *core.ConfigMap, _ *workercore.NodeModelStore) {
			require.NoError(t, c.Delete(context.Background(), cm))
		}},
		{name: "legacy authority identity", wantTransfer: true, mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, cm *core.ConfigMap, _ *workercore.NodeModelStore) {
			cm.Data["reservations.json"] = strings.ReplaceAll(cm.Data["reservations.json"], `"artifactUID":"artifact-uid",`, "")
			require.NoError(t, c.Update(context.Background(), cm))
		}},
		{name: "other prefetch", mutate: func(t *testing.T, c ctrlcli.Client, _ *core.Pod, cm *core.ConfigMap, _ *workercore.NodeModelStore) {
			cm.Data["reservations.json"] = strings.ReplaceAll(cm.Data["reservations.json"], "prefetch-uid", "other-uid")
			require.NoError(t, c.Update(context.Background(), cm))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c, pf, pod, cm, report := testSourceAuthorityFixture(t)
			if tc.mutate != nil {
				tc.mutate(t, c, pod, cm, report)
			}
			reconcileModelPrefetch(t, r, pf.Namespace, pf.Name)
			got := getPrefetch(t, c, pf.Namespace, pf.Name)
			if !tc.wantTransfer {
				assert.Nil(t, got.Status.Transfer)
				return
			}
			require.NotNil(t, got.Status.Transfer)
			assert.Equal(t, int64(5<<30), got.Status.Transfer.ReservedBytes)
			if !tc.wantReady {
				assert.Empty(t, got.Status.Transfer.Generations)
				return
			}
			require.Len(t, got.Status.Transfer.Generations, 1)
			assert.Equal(t, int32(1), got.Status.Transfer.Generations[0].ReadyNodes)
		})
	}
}

func TestModelPrefetchRetainsChargesAcrossIntentChanges(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, ctrlcli.Client, *workercore.ModelPrefetch)
	}{
		{name: "spec revision", change: func(_ *testing.T, _ ctrlcli.Client, pf *workercore.ModelPrefetch) { pf.Generation++ }},
		{name: "retargeted", change: func(_ *testing.T, _ ctrlcli.Client, pf *workercore.ModelPrefetch) {
			pf.Spec.Placement.NodeSelector = &meta.LabelSelector{MatchLabels: map[string]string{"pool": "other"}}
			pf.Generation++
		}},
		{name: "source removed", change: func(_ *testing.T, _ ctrlcli.Client, pf *workercore.ModelPrefetch) {
			pf.Spec.Source = nil
			pf.Generation++
		}},
		{name: "artifact revised", change: func(t *testing.T, c ctrlcli.Client, _ *workercore.ModelPrefetch) {
			artifact := new(workercore.ModelArtifact)
			require.NoError(t, c.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "model"}, artifact))
			artifact.Generation++
			artifact.Status.ObservedGeneration = artifact.Generation
			require.NoError(t, c.Update(context.Background(), artifact))
		}},
		{name: "grant shrunk", change: func(t *testing.T, c ctrlcli.Client, _ *workercore.ModelPrefetch) {
			binding := new(workercore.ModelStoreBinding)
			require.NoError(t, c.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "cache"}, binding))
			binding.Spec.SourceMemory.Bytes = resource.MustParse("1Gi")
			require.NoError(t, c.Update(context.Background(), binding))
		}},
		{name: "binding changed", change: func(t *testing.T, c ctrlcli.Client, pf *workercore.ModelPrefetch) {
			binding := testSourceGrantBinding()
			binding.Name, binding.UID = "other-cache", "other-binding"
			require.NoError(t, c.Create(context.Background(), binding))
			pf.Spec.BindingRef.Name = binding.Name
			pf.Generation++
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c, pf, _, cm, _ := testSourceAuthorityFixture(t)
			reconcileModelPrefetch(t, r, pf.Namespace, pf.Name)
			pf = getPrefetch(t, c, pf.Namespace, pf.Name)
			require.NotNil(t, pf.Status.Transfer)
			tc.change(t, c, pf)
			require.NoError(t, c.Update(context.Background(), pf))
			reconcileModelPrefetch(t, r, pf.Namespace, pf.Name)
			got := getPrefetch(t, c, pf.Namespace, pf.Name)
			require.NotNil(t, got.Status.Transfer)
			assert.Equal(t, int64(5<<30), got.Status.Transfer.ReservedBytes)
			assert.Empty(t, got.Status.Transfer.Generations)
			var document map[string]any
			require.NoError(t, json.Unmarshal([]byte(cm.Data["reservations.json"]), &document))
			document["entries"] = []any{}
			raw, err := json.Marshal(document)
			require.NoError(t, err)
			cm.Data["reservations.json"] = string(raw)
			require.NoError(t, c.Update(context.Background(), cm))
			reconcileModelPrefetch(t, r, pf.Namespace, pf.Name)
			assert.Nil(t, getPrefetch(t, c, pf.Namespace, pf.Name).Status.Transfer)
		})
	}
}

func TestNodeReportChangedOnLeaseRenewal(t *testing.T) {
	before := testNodeModelStoreModelTransfer("node-1", "gen-1", testDigest, workercore.NodeModelStoreModelTransferGenerationStateServing)
	first := meta.NewTime(testNow.Add(time.Minute))
	before.Status.ModelTransfer.Generations[0].LeaseExpiresAt = &first
	after := before.DeepCopy()
	second := meta.NewTime(testNow.Add(2 * time.Minute))
	after.Status.ModelTransfer.Generations[0].LeaseExpiresAt = &second
	assert.True(t, nodeReportChanged.Update(ctrlevent.UpdateEvent{ObjectOld: before, ObjectNew: after}))
	assert.False(t, nodeReportChanged.Update(ctrlevent.UpdateEvent{ObjectOld: before, ObjectNew: before.DeepCopy()}))
}
