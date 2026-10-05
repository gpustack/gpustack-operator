package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

var testDigest = "sha256:" + strings.Repeat("7", 64)

var testNow = time.Date(2026, 9, 27, 6, 0, 0, 0, time.UTC)

func testResolvedArtifact(name string) *workercore.ModelArtifact {
	return &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "team-a", UID: types.UID("artifact-uid")},
		Status: workercore.ModelArtifactStatus{
			Resolved: &workercore.ModelArtifactResolved{ManifestDigest: testDigest},
		},
	}
}

func testPrefetch(name string, mutate func(*workercore.ModelPrefetch)) *workercore.ModelPrefetch {
	pf := &workercore.ModelPrefetch{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "team-a", Generation: 1},
		Spec: workercore.ModelPrefetchSpec{
			ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  workercore.ModelPrefetchBindingReference{Name: "cache"},
			Placement: &workercore.ModelPrefetchPlacement{
				NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}},
			},
		},
	}
	if mutate != nil {
		mutate(pf)
	}

	return pf
}

func testNodeModelStoreEntry(node, state string, lastUsed *meta.Time, pinned ...[]string) *workercore.NodeModelStore {
	nms := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: node},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{Digest: testDigest, State: workercore.NodeModelStoreModelState(state), LastUsedTime: lastUsed}},
		},
	}
	if len(pinned) > 0 {
		nms.Spec.Pinned = pinned[0]
	}

	return nms
}

func newTestModelPrefetchEnv(t *testing.T, objs ...ctrlcli.Object) (*ModelPrefetchReconciler, ctrlcli.Client) {
	t.Helper()
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelPrefetch{}, &core.Pod{}).
		WithObjects(objs...).Build()

	r := &ModelPrefetchReconciler{Client: cli, Now: func() time.Time { return testNow }}

	return r, cli
}

func reconcileModelPrefetch(t *testing.T, r *ModelPrefetchReconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: ns, Name: name}})
	require.NoError(t, err)

	return res
}

func hourAgo(h int) *meta.Time {
	t := meta.NewTime(testNow.Add(-time.Duration(h) * time.Hour))

	return &t
}

func TestModelPrefetchRendersTheWarmupShape(t *testing.T) {
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testPrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	require.Len(t, pods.Items, 1, "one warm-up Pod is rendered for the one target node")
	pod := &pods.Items[0]
	assert.True(t, strings.HasPrefix(pod.Name, "warm-warmup-node-1-"),
		"the generated name carries the prefetch and its node, got %q", pod.Name)

	assert.Equal(t, "node-1", pod.Spec.NodeName, "the warm-up Pod is pinned to its target")
	assert.Equal(t, core.RestartPolicyNever, pod.Spec.RestartPolicy)
	require.NotNil(t, pod.Spec.SecurityContext)
	assert.Equal(t, ptrOf(true), pod.Spec.SecurityContext.RunAsNonRoot)
	require.Len(t, pod.Spec.Containers, 1)
	assert.Empty(t, pod.Spec.Containers[0].Resources.Limits, "a warm-up asks for no accelerator")
	require.NotNil(t, pod.Spec.Containers[0].SecurityContext)
	assert.Equal(t, ptrOf(false), pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation)

	for k := range pod.Labels {
		assert.NotEqual(t, "kueue.x-k8s.io/queue-name", k, "a warm-up Pod must never carry a queue-name label")
	}

	require.Len(t, pod.Spec.Volumes, 1)
	require.NotNil(t, pod.Spec.Volumes[0].CSI)
	assert.Equal(t, modelstore.DriverName, pod.Spec.Volumes[0].CSI.Driver)
	assert.Equal(t, map[string]string{
		modelstore.VolumeAttrArtifact:       "model",
		modelstore.VolumeAttrArtifactUID:    "artifact-uid",
		modelstore.VolumeAttrManifestDigest: testDigest,
	}, pod.Spec.Volumes[0].CSI.VolumeAttributes)

	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "ModelPrefetch", pod.OwnerReferences[0].Kind)
	assert.True(t, *pod.OwnerReferences[0].Controller)
}

func ptrOf[T any](v T) *T { return &v }

func TestModelPrefetchAggregatesReadiness(t *testing.T) {
	// Two targets: node-1 Ready, node-2 Downloading. The pod for the finished node goes.
	old := hourAgo(1)
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Ready", old),
		testNodeModelStoreEntry("node-2", "Downloading", nil),
		&core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Name: "warm-warmup-node-1", Namespace: "team-a",
				Labels: map[string]string{"worker.gpustack.ai/model-prefetch": "warm"},
			},
			Spec:   core.PodSpec{NodeName: "node-1"},
			Status: core.PodStatus{Phase: core.PodSucceeded},
		},
		testPrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pf := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, pf))
	assert.Equal(t, int32(2), pf.Status.DesiredNodes)
	assert.Equal(t, int32(1), pf.Status.ReadyNodes)
	assert.Equal(t, int32(1), pf.Status.DownloadingNodes)
	available := testCondition(pf.Status.Conditions, ModelPrefetchConditionAvailable)
	require.NotNil(t, available)
	assert.Equal(t, meta.ConditionFalse, available.Status, "one ready node of two is not available with MinReady 0")
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, meta.ConditionTrue, progressing.Status)
}

func TestModelPrefetchReachesAvailableWhenMinReadyHolds(t *testing.T) {
	old := hourAgo(1)
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Ready", old),
		testNodeModelStoreEntry("node-2", "Ready", old),
		testPrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pf := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, pf))
	assert.Equal(t, int32(2), pf.Status.ReadyNodes)
	available := testCondition(pf.Status.Conditions, ModelPrefetchConditionAvailable)
	require.NotNil(t, available)
	assert.Equal(t, meta.ConditionTrue, available.Status)
	progressing := testCondition(pf.Status.Conditions, ModelPrefetchConditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, meta.ConditionFalse, progressing.Status)
}

func testPinnedBinding() *workercore.ModelStoreBinding {
	tr := true
	return &workercore.ModelStoreBinding{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "cache"},
		Spec: workercore.ModelStoreBindingSpec{
			StoreRefs:   []workercore.ModelStoreBindingStoreReference{{Name: "h100"}},
			AllowPinned: &tr,
		},
	}
}

func TestModelPrefetchTTLExpiresAndUnpins(t *testing.T) {
	stale := hourAgo(3)
	fresh := hourAgo(1)
	pf := testPrefetch("warm", func(p *workercore.ModelPrefetch) {
		p.Spec.Retention.Pinned = true
		ttl := meta.Duration{Duration: 2 * time.Hour}
		p.Spec.Retention.TTLAfterLastUse = &ttl
	})
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Ready", stale, []string{testDigest}),
		testNodeModelStoreEntry("node-2", "Ready", fresh),
		testPinnedBinding(),
		pf,
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	got := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, got))
	assert.Equal(t, int32(1), got.Status.ReadyNodes, "the stale node's use ran past the TTL")

	nms := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
	assert.Empty(t, nms.Spec.Pinned, "the expired node's pin is released by the union pass")

	nms2 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-2"}, nms2))
	assert.Equal(t, []string{testDigest}, nms2.Spec.Pinned, "the fresh node stays pinned")
}

func TestModelPrefetchLapseExplainsTheShortfall(t *testing.T) {
	// Two targets, one copy past the TTL: the gap below minReady is exactly the lapsed copy, and
	// the Lapsed condition says so — the pin is released, nothing re-warms, and Available stays
	// False for a reason a reader can now see.
	stale := hourAgo(3)
	fresh := hourAgo(1)
	pf := testPrefetch("warm", func(p *workercore.ModelPrefetch) {
		p.Spec.Retention.Pinned = true
		ttl := meta.Duration{Duration: 2 * time.Hour}
		p.Spec.Retention.TTLAfterLastUse = &ttl
	})
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Ready", stale, []string{testDigest}),
		testNodeModelStoreEntry("node-2", "Ready", fresh),
		testPinnedBinding(),
		pf,
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	got := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, got))
	assert.Equal(t, int32(1), got.Status.ReadyNodes, "the lapsed copy stopped counting as ready")

	available := testCondition(got.Status.Conditions, ModelPrefetchConditionAvailable)
	require.NotNil(t, available)
	assert.Equal(t, meta.ConditionFalse, available.Status, "one lapsed copy of two leaves the prefetch short")

	lapsed := testCondition(got.Status.Conditions, ModelPrefetchConditionLapsed)
	require.NotNil(t, lapsed, "the lapse is surfaced, not left to read out of the counters")
	assert.Equal(t, meta.ConditionTrue, lapsed.Status)
	assert.Equal(t, "RetentionTTLExpired", lapsed.Reason)
	assert.Contains(t, lapsed.Message, "1 of 2 target node copies lapsed")
	assert.Contains(t, lapsed.Message, "retention.ttlAfterLastUse")
	assert.Contains(t, lapsed.Message, "pins were released", "a pinned prefetch's lapse releases the pins")

	degraded := testCondition(got.Status.Conditions, ModelPrefetchConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, meta.ConditionFalse, degraded.Status, "a lapse is not a failure")
}

func TestModelPrefetchLapseStaysFalseWhileWarming(t *testing.T) {
	// One target still downloading, one ready: the shortfall is the warm-up, not a lapse, and the
	// condition says no copy has lapsed.
	fresh := hourAgo(1)
	pf := testPrefetch("warm", func(p *workercore.ModelPrefetch) {
		ttl := meta.Duration{Duration: 2 * time.Hour}
		p.Spec.Retention.TTLAfterLastUse = &ttl
	})
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Downloading", nil),
		testNodeModelStoreEntry("node-2", "Ready", fresh),
		pf,
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	got := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, got))
	lapsed := testCondition(got.Status.Conditions, ModelPrefetchConditionLapsed)
	require.NotNil(t, lapsed)
	assert.Equal(t, meta.ConditionFalse, lapsed.Status, "a shortfall the warm-up explains is not a lapse")
	assert.Equal(t, "NoLapsedCopies", lapsed.Reason)
}

func TestModelPrefetchLapseNeverMasksAFailure(t *testing.T) {
	// One target failed, one ready: Degraded carries the failure and Lapsed stays False — the
	// condition exists to explain a lapse, never to hide a genuine failure behind one.
	fresh := hourAgo(1)
	pf := testPrefetch("warm", func(p *workercore.ModelPrefetch) {
		ttl := meta.Duration{Duration: 2 * time.Hour}
		p.Spec.Retention.TTLAfterLastUse = &ttl
	})
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Failed", nil),
		testNodeModelStoreEntry("node-2", "Ready", fresh),
		pf,
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	got := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, got))
	lapsed := testCondition(got.Status.Conditions, ModelPrefetchConditionLapsed)
	require.NotNil(t, lapsed)
	assert.Equal(t, meta.ConditionFalse, lapsed.Status, "a failure is not a lapse")
	assert.Equal(t, "NoLapsedCopies", lapsed.Reason)

	degraded := testCondition(got.Status.Conditions, ModelPrefetchConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, meta.ConditionTrue, degraded.Status, "the failure keeps its own voice")
}

func TestModelPrefetchShortfallWiderThanTheLapse(t *testing.T) {
	// Three targets: one lapsed, one failed, one ready. The lapse alone cannot close the gap, so
	// Lapsed stays False and names the lapse it did see — while Degraded carries the failure.
	stale := hourAgo(3)
	fresh := hourAgo(1)
	pf := testPrefetch("warm", func(p *workercore.ModelPrefetch) {
		ttl := meta.Duration{Duration: 2 * time.Hour}
		p.Spec.Retention.TTLAfterLastUse = &ttl
	})
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-3", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Ready", stale),
		testNodeModelStoreEntry("node-2", "Failed", nil),
		testNodeModelStoreEntry("node-3", "Ready", fresh),
		pf,
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	got := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, got))
	assert.Equal(t, int32(1), got.Status.ReadyNodes)
	lapsed := testCondition(got.Status.Conditions, ModelPrefetchConditionLapsed)
	require.NotNil(t, lapsed)
	assert.Equal(t, meta.ConditionFalse, lapsed.Status,
		"the lapse explains one copy of a two-copy gap; the rest is a genuine failure")
	assert.Equal(t, "ShortfallBeyondLapse", lapsed.Reason)
	assert.Contains(t, lapsed.Message, "1 of 3 target node copies lapsed")

	degraded := testCondition(got.Status.Conditions, ModelPrefetchConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, meta.ConditionTrue, degraded.Status)
}

func TestModelPrefetchLapseBelowMinReadyStaysAvailable(t *testing.T) {
	// MinReady 1 of 2: one copy lapsed, the other still holds the bar — Available stays True and
	// Lapsed stays False, reporting the lapse it saw as harmless.
	stale := hourAgo(3)
	fresh := hourAgo(1)
	pf := testPrefetch("warm", func(p *workercore.ModelPrefetch) {
		p.Spec.MinReady = 1
		ttl := meta.Duration{Duration: 2 * time.Hour}
		p.Spec.Retention.TTLAfterLastUse = &ttl
	})
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Ready", stale),
		testNodeModelStoreEntry("node-2", "Ready", fresh),
		pf,
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	got := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, got))
	available := testCondition(got.Status.Conditions, ModelPrefetchConditionAvailable)
	require.NotNil(t, available)
	assert.Equal(t, meta.ConditionTrue, available.Status, "minReady 1 is met by the surviving copy")

	lapsed := testCondition(got.Status.Conditions, ModelPrefetchConditionLapsed)
	require.NotNil(t, lapsed)
	assert.Equal(t, meta.ConditionFalse, lapsed.Status, "no shortfall, so the lapse explains nothing")
	assert.Equal(t, "NoShortfall", lapsed.Reason)
	assert.Contains(t, lapsed.Message, "1 of 2 target node copies lapsed")
}

func TestModelPrefetchPinnedUnionKeepsSharedDigests(t *testing.T) {
	// Two prefetches pin the same node for two different digests: the union writes both, where a
	// per-prefetch writer would let whichever ran last drop the other's pin.
	digestB := "sha256:" + strings.Repeat("b", 64)
	artifactB := testResolvedArtifact("model2")
	artifactB.Status.Resolved.ManifestDigest = digestB
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		artifactB,
		testPinnedBinding(),
		testPrefetch("warm", func(p *workercore.ModelPrefetch) { p.Spec.Retention.Pinned = true }),
		testPrefetch("warm2", func(p *workercore.ModelPrefetch) {
			p.Spec.Retention.Pinned = true
			p.Spec.ArtifactRef.Name = "model2"
		}),
		&workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{
					{Digest: testDigest, State: workercore.NodeModelStoreModelStateReady},
					{Digest: digestB, State: workercore.NodeModelStoreModelStateReady},
				},
			},
		},
	)
	require.NoError(t, r.recomputePinned(context.Background()))

	nms := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
	assert.Equal(t, []string{testDigest, digestB}, nms.Spec.Pinned,
		"the node pins the union: both prefetches' digests, sorted")
}

func TestModelPrefetchBatchesPodCreation(t *testing.T) {
	objs := []ctrlcli.Object{testResolvedArtifact("model"), testPrefetch("warm", nil)}
	for i := 0; i < 12; i++ {
		objs = append(objs, testModelNodeLabeled(nodeNameFor(i), map[string]string{"pool": "h100"}))
	}
	r, cli := newTestModelPrefetchEnv(t, objs...)
	res := reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	assert.Len(t, pods.Items, modelPrefetchPodsPerPass, "one pass creates a bounded batch")
	assert.True(t, res.RequeueAfter > 0, "the batch asks for the next pass")
}

func TestDeploymentInstanceTypesCollectsServingPools(t *testing.T) {
	deps := []workercore.ModelDeployment{
		{
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "model"}},
				Roles: []workercore.ModelDeploymentRole{
					{InstanceType: "h100-8"},
					{InstanceType: "mi300-8"},
				},
			},
		},
		{
			Spec: workercore.ModelDeploymentSpec{
				Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "other"}},
				Roles: []workercore.ModelDeploymentRole{{InstanceType: "ignored"}},
			},
		},
	}

	assert.ElementsMatch(t, []string{"h100-8", "mi300-8"}, deploymentInstanceTypes(deps, "model"))
}

// newPrefetchInstanceTypeEnv builds a fake client carrying the given nodes and InstanceTypes plus
// the ResourceFlavors the nodes genuinely contribute to — reconciled through the NodeFlavor
// reconciler, so the flavors' names and labels are the production ones, never hand-written.
func newPrefetchInstanceTypeEnv(
	t *testing.T, nodes []*core.Node, types ...*workercore.InstanceType,
) ctrlcli.Client {
	t.Helper()
	objs := make([]ctrlcli.Object, 0, len(nodes)+len(types))
	for _, nd := range nodes {
		objs = append(objs, nd)
	}
	for _, it := range types {
		objs = append(objs, it)
	}
	cli := buildNodeFlavorClient(objs...)
	for _, nd := range nodes {
		for _, f := range nodefeature.ExtractNodeFlavors(nd) {
			reconcileNodeFlavor(t, cli,
				topologyQualifiedFlavorName(f.Name, nd.Labels[TopologyProfileLabel]))
		}
	}

	return cli
}

// resolvePrefetchTargets runs the placement resolution the admission webhook runs: the pool
// resolver over the client, then the shared target expansion.
func resolvePrefetchTargets(
	t *testing.T, cli ctrlcli.Client, pf *workercore.ModelPrefetch,
	nodes []core.Node, deps []workercore.ModelDeployment,
) []string {
	t.Helper()
	pools, err := ResolveInstanceTypeFlavorNames(context.Background(), cli, pf, deps)
	require.NoError(t, err)
	targets, err := PrefetchTargetNodes(pf, nodes, deps, pools)
	require.NoError(t, err)

	return targets
}

// testElasticInstanceType is the issue's shape: an admin-authored accelerated InstanceType with
// a custom name over the nvidia-h100-80gb-hbm3 pool.
func testElasticInstanceType() *workercore.InstanceType {
	return &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "elastic-cpu-gpu"},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable:    true,
			GeneralGroup:     "generic",
			AcceleratorGroup: "nvidia-h100-80gb-hbm3",
			OS:               "linux",
			Arch:             "amd64",
		},
	}
}

func TestPrefetchPlacementNamedInstanceTypeResolvesThePool(t *testing.T) {
	// #717: a custom accelerated InstanceType, Active with capacity, selected by
	// spec.placement.instanceTypes, resolves the nodes carrying the type's flavors — through the
	// same resolution the admission webhook runs — not zero. A CPU-only node in the cluster is
	// the negative control.
	gpu1 := newManagedAccelNodeOf("gpu-node-1", 8, "nvidia-h100-80gb-hbm3", "NVIDIA-H100-80GB-HBM3")
	gpu2 := newManagedAccelNodeOf("gpu-node-2", 8, "nvidia-h100-80gb-hbm3", "NVIDIA-H100-80GB-HBM3")
	cpu := newManagedCPUNode("cpu-node-1", 16, 64, 200)
	cli := newPrefetchInstanceTypeEnv(t, []*core.Node{gpu1, gpu2, cpu}, testElasticInstanceType())
	pf := testPrefetch("warm", func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{
			InstanceTypes: []string{"elastic-cpu-gpu"},
		}
	})

	targets := resolvePrefetchTargets(t, cli, pf, []core.Node{*gpu1, *gpu2, *cpu}, nil)
	assert.Equal(t, []string{"gpu-node-1", "gpu-node-2"}, targets,
		"the named custom InstanceType's pool nodes are the targets")
}

func TestPrefetchPlacementDerivedInstanceTypeResolvesThePool(t *testing.T) {
	// #717: the default placement derives the target set from the namespace deployments' own
	// InstanceType names; a custom name resolves the same way as an explicit one.
	gpu1 := newManagedAccelNodeOf("gpu-node-1", 8, "nvidia-h100-80gb-hbm3", "NVIDIA-H100-80GB-HBM3")
	cli := newPrefetchInstanceTypeEnv(t, []*core.Node{gpu1}, testElasticInstanceType())
	pf := testPrefetch("warm", func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = nil
	})
	dep := workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "served", Namespace: "team-a"},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{ArtifactRef: &core.LocalObjectReference{Name: "model"}},
			Roles: []workercore.ModelDeploymentRole{{InstanceType: "elastic-cpu-gpu"}},
		},
	}

	targets := resolvePrefetchTargets(t, cli, pf, []core.Node{*gpu1}, []workercore.ModelDeployment{dep})
	assert.Equal(t, []string{"gpu-node-1"}, targets,
		"the deployment's custom InstanceType's pool node is a target")
}

func TestPrefetchPlacementFollowsTheTypeAcrossTopologyProfiles(t *testing.T) {
	// The same hardware in two topology profiles publishes two flavors; the named type's pool
	// selects both, and the target set follows the flavors rather than one qualified name.
	gpuA := newManagedAccelNodeOf("gpu-node-a", 8, "nvidia-h100-80gb-hbm3", "NVIDIA-H100-80GB-HBM3")
	gpuB := newManagedAccelNodeOf("gpu-node-b", 8, "nvidia-h100-80gb-hbm3", "NVIDIA-H100-80GB-HBM3")
	gpuB.Labels[TopologyProfileLabel] = topologyProfile([]string{
		core.LabelHostname,
		"topology.kubernetes.io/zone",
	})
	cli := newPrefetchInstanceTypeEnv(t, []*core.Node{gpuA, gpuB}, testElasticInstanceType())
	pf := testPrefetch("warm", func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{
			InstanceTypes: []string{"elastic-cpu-gpu"},
		}
	})

	targets := resolvePrefetchTargets(t, cli, pf, []core.Node{*gpuA, *gpuB}, nil)
	assert.Equal(t, []string{"gpu-node-a", "gpu-node-b"}, targets,
		"both profiles' flavors resolve into the target set")
}

func TestPrefetchPlacementUnknownInstanceTypeResolvesNoNodes(t *testing.T) {
	// The placement contract is "every node carrying the type's flavors": a name nothing
	// publishes carries none, so the target set is empty and the minReady admission check
	// explains the shortfall — no error, no invented targets.
	gpu1 := newManagedAccelNodeOf("gpu-node-1", 8, "nvidia-h100-80gb-hbm3", "NVIDIA-H100-80GB-HBM3")
	cli := newPrefetchInstanceTypeEnv(t, []*core.Node{gpu1})
	pf := testPrefetch("warm", func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{
			InstanceTypes: []string{"no-such-type"},
		}
	})

	targets := resolvePrefetchTargets(t, cli, pf, []core.Node{*gpu1}, nil)
	assert.Empty(t, targets, "an unknown InstanceType name contributes no nodes")
}

func TestPrefetchPlacementAmbiguousStillRefuses(t *testing.T) {
	// Both placement kinds set is refused before any resolution runs.
	pf := testPrefetch("warm", func(pf *workercore.ModelPrefetch) {
		pf.Spec.Placement = &workercore.ModelPrefetchPlacement{
			InstanceTypes: []string{"elastic-cpu-gpu"},
			NodeSelector:  &meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}},
		}
	})

	_, err := prefetchTargetNodes(pf, nil, nil, nil)
	assert.ErrorIs(t, err, ErrPrefetchPlacementAmbiguous)
}

func nodeNameFor(i int) string {
	return "node-" + string(rune('a'+i))
}

func TestPrefetchPodNameStaysBoundedAndDistinct(t *testing.T) {
	longPf := strings.Repeat("w", 250)
	longA := longPf + "-extra"
	longB := longPf + "-extra2"
	longNode := strings.Repeat("n", 240)

	a := prefetchPodName(longA, longNode)
	b := prefetchPodName(longB, longNode)
	assert.LessOrEqual(t, len(a), 247, "the generated name never passes what a pod name accepts once the server appends its suffix")
	assert.LessOrEqual(t, len(b), 247, "the generated name never passes what a pod name accepts once the server appends its suffix")
	assert.NotEqual(t, a, b, "two prefetches on one long node never collide")
	assert.NotContains(t, a, "\x00")

	// A name that fits keeps the node readable.
	assert.Equal(t, "warm-warmup-"+longNode[:200], prefetchPodName("warm", longNode[:200]+"-tail")[:len("warm-warmup-")+200],
		"the readable form keeps the node's own name where it fits")
}

func TestModelPrefetchFailedEntryStopsRecreation(t *testing.T) {
	// The node's report reads Failed with a done pod on it: the pod is removed and NOT replaced —
	// the failure is the prefetch's Degraded, and a replacement would re-download at event rate.
	failed := testNodeModelStoreEntry("node-1", "Failed", nil)
	donePod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "warm-warmup-node-1", Namespace: "team-a",
			Labels: map[string]string{"worker.gpustack.ai/model-prefetch": "warm"},
		},
		Spec:   core.PodSpec{NodeName: "node-1"},
		Status: core.PodStatus{Phase: core.PodFailed},
	}
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		failed,
		donePod,
		testPrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	assert.Empty(t, pods.Items, "a failed delivery's pod is not replaced")

	pf := new(workercore.ModelPrefetch)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "warm"}, pf))
	degraded := testCondition(pf.Status.Conditions, ModelPrefetchConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, meta.ConditionTrue, degraded.Status, "the failure is the prefetch's Degraded, not a silent loop")
}

func TestModelPrefetchFailedPodOnWaitingNodeRetries(t *testing.T) {
	// A Failed pod on a node whose entry still reads Downloading is a dead attempt blocking the
	// retry by its name: the reconcile removes it and renders a fresh one.
	donePod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "warm-warmup-node-1", Namespace: "team-a", UID: types.UID("dead-attempt"),
			Labels: map[string]string{"worker.gpustack.ai/model-prefetch": "warm"},
		},
		Spec:   core.PodSpec{NodeName: "node-1"},
		Status: core.PodStatus{Phase: core.PodFailed},
	}
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Downloading", nil),
		donePod,
		testPrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	require.Len(t, pods.Items, 1, "the dead attempt is replaced")
	assert.NotEqual(t, donePod.UID, pods.Items[0].UID, "the replacement is a fresh pod")
}

// warmupAttemptPod is a warm-up pod as the controller renders it for node-1: the prefetch's own
// label and the artifact's digest on the volume, so the pass reads it as a current attempt.
func warmupAttemptPod(name string, uid types.UID, phase core.PodPhase) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: name, Namespace: "team-a", UID: uid,
			Labels: map[string]string{"worker.gpustack.ai/model-prefetch": "warm"},
		},
		Spec: core.PodSpec{
			NodeName: "node-1",
			Volumes: []core.Volume{{
				Name: "model",
				VolumeSource: core.VolumeSource{
					CSI: &core.CSIVolumeSource{
						Driver:           modelstore.DriverName,
						VolumeAttributes: map[string]string{modelstore.VolumeAttrManifestDigest: testDigest},
					},
				},
			}},
		},
		Status: core.PodStatus{Phase: phase},
	}
}

func TestModelPrefetchReplacesATerminatingAttemptInTheSamePass(t *testing.T) {
	// The dead attempt carries a finalizer, so deleting it leaves it Terminating the way a real
	// cluster does for the grace period. The node still reads Downloading: the replacement must
	// not wait for the old pod to leave the API.
	deadPod := warmupAttemptPod("warm-warmup-node-1", types.UID("dead-attempt"), core.PodFailed)
	deadPod.Finalizers = []string{"test/holds-the-pod"}
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Downloading", nil),
		deadPod,
		testPrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	require.Len(t, pods.Items, 2, "the replacement is created while the dead attempt still terminates")

	byUID := map[types.UID]core.Pod{}
	for _, p := range pods.Items {
		byUID[p.UID] = p
	}
	old, ok := byUID[types.UID("dead-attempt")]
	require.True(t, ok, "the deleted attempt lingers until its grace period ends")
	assert.NotNil(t, old.DeletionTimestamp, "the dead attempt is terminating")

	fresh := pods.Items[0]
	if fresh.UID == old.UID {
		fresh = pods.Items[1]
	}
	assert.Nil(t, fresh.DeletionTimestamp, "the fresh attempt is not the terminating one")
	assert.Equal(t, "node-1", fresh.Spec.NodeName)
}

func TestModelPrefetchKeepsTheRunningAttempt(t *testing.T) {
	// A live attempt on a waiting node is the node's delivery: the pass renders no second pod
	// beside it.
	r, cli := newTestModelPrefetchEnv(t,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testResolvedArtifact("model"),
		testNodeModelStoreEntry("node-1", "Downloading", nil),
		warmupAttemptPod("warm-warmup-node-1", types.UID("running-attempt"), core.PodRunning),
		testPrefetch("warm", nil),
	)
	reconcileModelPrefetch(t, r, "team-a", "warm")

	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace("team-a")))
	require.Len(t, pods.Items, 1, "a running attempt is not duplicated")
	assert.Equal(t, types.UID("running-attempt"), pods.Items[0].UID)
}

func TestWarmupDigestLabelPassesValidation(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	pod := warmupPod(testPrefetch("warm", nil), testResolvedArtifact("model"), "python:3.12-alpine", "node-1", digest)

	value := pod.Labels["worker.gpustack.ai/model-prefetch-digest"]
	if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
		t.Fatalf("the digest label value is rejected by the API server: %v", errs)
	}
	if value != strings.Repeat("a", 32) {
		t.Fatalf("the label value should carry the first half of the digest hex, got %q", value)
	}
}
