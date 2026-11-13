package modeltransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
)

// recordingDiscoveryClient records every live read a source pass makes, so
// a test can require that the fleet lists are gone while the proofs of a
// candidate's identity, grant, and artifact stay live.
type recordingDiscoveryClient struct {
	ctrlcli.Client
	mu    sync.Mutex
	gets  map[string]int
	lists map[string]int
}

func newRecordingDiscoveryClient(objects ...ctrlcli.Object) *recordingDiscoveryClient {
	return &recordingDiscoveryClient{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build(),
		gets:   map[string]int{},
		lists:  map[string]int{},
	}
}

func (c *recordingDiscoveryClient) Get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
	c.mu.Lock()
	c.gets[fmt.Sprintf("%T %s/%s", obj, key.Namespace, key.Name)]++
	c.mu.Unlock()
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *recordingDiscoveryClient) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	c.mu.Lock()
	c.lists[fmt.Sprintf("%T", list)]++
	c.mu.Unlock()
	return c.Client.List(ctx, list, opts...)
}

func (c *recordingDiscoveryClient) getCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for key, count := range c.gets {
		if len(key) >= len(name) && key[len(key)-len(name):] == name {
			total += count
		}
	}
	return total
}

func (c *recordingDiscoveryClient) listCount(list ctrlcli.ObjectList) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists[fmt.Sprintf("%T", list)]
}

// fakeSnapshot is a controlled sourceSnapshot: tests flip its readiness and
// its candidate view directly, without an API server.
type fakeSnapshot struct {
	mu         sync.Mutex
	ready      bool
	nodes      []core.Node
	prefetches []workercore.ModelPrefetch
}

func (f *fakeSnapshot) Ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeSnapshot) Nodes() []core.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Node(nil), f.nodes...)
}

func (f *fakeSnapshot) Prefetches() []workercore.ModelPrefetch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workercore.ModelPrefetch(nil), f.prefetches...)
}

func (f *fakeSnapshot) set(ready bool, nodes []core.Node, prefetches []workercore.ModelPrefetch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready, f.nodes, f.prefetches = ready, nodes, prefetches
}

// snapshotFixture carries the broker, the live cluster objects, and a ready
// snapshot holding one node and one prefetch that target it.
type snapshotFixture struct {
	cfg        *Config
	src        *kubernetesIntentSource
	client     *recordingDiscoveryClient
	snapshot   *fakeSnapshot
	node       *core.Node
	prefetch   *workercore.ModelPrefetch
	binding    *workercore.ModelStoreBinding
	model      *workercore.ModelArtifact
	modelStore *workercore.ModelStore
}

func newSnapshotFixture(t *testing.T) *snapshotFixture {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	file, err := os.ReadFile(filepath.Join("..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense", "checkpoint.safetensors"))
	require.NoError(t, err)
	fileSum := sha256.Sum256(file)
	fileDigest := "sha256:" + hex.EncodeToString(fileSum[:])
	manifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{Path: "checkpoint.safetensors", Size: int64(len(file)), Digest: fileDigest}})
	require.NoError(t, err)
	attempt, err := st.NewAttempt(store.HexOf(manifest.Digest))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(attempt.Dir(), "tree"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "tree", "checkpoint.safetensors"), file, 0o644))
	listing, err := json.Marshal(peer.Listing{Digest: manifest.Digest, Entries: []peer.ManifestFile{{Path: "checkpoint.safetensors", Size: int64(len(file)), Digest: fileDigest}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "manifest.json"), listing, 0o600))
	require.NoError(t, attempt.Publish(store.Marker{Digest: manifest.Digest, SizeBytes: int64(len(file)), ManifestFormat: modelartifact.ManifestHeader, Source: store.SourceHub}))
	b, err := broker.Open(filepath.Join(root, "broker"), "nvidia", st, broker.Limits{PhysicalBytes: 1 << 30, MaxGenerations: 4, MaxQuarantined: 2, QuarantineBytes: 1 << 28, LeaseTTL: time.Minute}, time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.EnableOperationJournal())
	private := &snapshotClosedSource{}
	b.SetPrivateSource(private)
	require.NoError(t, b.RequireAuthorityInventory("node-1"))

	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "source"}}}
	cpuAware := &gpustack.Setting{ObjectMeta: meta.ObjectMeta{Name: "instance-type-aware-cpu-manufacturer", Namespace: "operator"}, Status: gpustack.SettingStatus{Value: "false"}}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: "operator", UID: "ds-uid", Labels: labels}, Spec: apps.DaemonSetSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}}}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "source-1", Namespace: "operator", UID: "pod-uid", Labels: labels, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: ptr.To(true)}}}, Spec: core.PodSpec{NodeName: node.Name, Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}
	kube := kubefake.NewSimpleClientset(node, cpuAware, ds, pod)
	kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.CreateAction).GetObject().(*core.ConfigMap)
		cm.UID = "authority-uid"
		cm.ResourceVersion = "1"
		return false, nil, nil
	})
	prefetch := &workercore.ModelPrefetch{ObjectMeta: meta.ObjectMeta{Name: "prefetch", Namespace: "tenant", UID: "prefetch-uid", Generation: 1}, Spec: workercore.ModelPrefetchSpec{ArtifactRef: workercore.ModelPrefetchArtifactReference{Name: "model"}, BindingRef: workercore.ModelPrefetchBindingReference{Name: "grant"}, Source: &workercore.ModelPrefetchSource{}, Placement: &workercore.ModelPrefetchPlacement{NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}}}
	model := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Name: "model", Namespace: "tenant", UID: "artifact-uid", Generation: 1}, Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{HuggingFace: &workercore.ModelArtifactHubSource{Repository: "org/model"}}}, Status: workercore.ModelArtifactStatus{ObservedGeneration: 1, Resolved: &workercore.ModelArtifactResolved{ManifestDigest: manifest.Digest, SizeBytes: int64(len(file))}, Conditions: []gpustack.Condition{{Type: "Resolved", Status: meta.ConditionTrue}}}}
	binding := &workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "binding-uid"}, Spec: workercore.ModelStoreBindingSpec{StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "store"}}, SourceMemory: &workercore.ModelStoreBindingSourceMemory{Bytes: resource.MustParse("1Gi"), Registrations: 4, Sessions: 4}}}
	modelStore := &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "store"}, Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}}
	client := newRecordingDiscoveryClient(node, prefetch, model, binding, modelStore)
	cfg := &Config{manifestPageRoot: filepath.Join(root, "broker", "manifest-staging"), Store: st, Broker: b, Client: kube, WorkerNamespace: "operator", WorkerID: "pod-uid", NodeName: "node-1", VendorScope: "nvidia"}
	src := &kubernetesIntentSource{config: cfg, client: client}
	cfg.Intents = src
	b.SetIntentValidator(cfg.validateLiveIntent)
	snapshot := &fakeSnapshot{}
	cfg.snapshot = snapshot
	snapshot.set(true, []core.Node{*node.DeepCopy()}, []workercore.ModelPrefetch{*prefetch.DeepCopy()})
	return &snapshotFixture{
		cfg: cfg, src: src, client: client, snapshot: snapshot,
		node: node, prefetch: prefetch, binding: binding, model: model, modelStore: modelStore,
	}
}

func (f *snapshotFixture) listIntents(t *testing.T) []broker.Intent {
	t.Helper()
	intents, err := f.src.ListIntents(context.Background())
	require.NoError(t, err)
	return intents
}

func TestListIntentsSteadyStateUsesSnapshotWithoutFleetLists(t *testing.T) {
	f := newSnapshotFixture(t)
	require.Len(t, f.listIntents(t), 1)
	require.Len(t, f.listIntents(t), 1)
	assert.Equal(t, 0, f.client.listCount(&core.NodeList{}),
		"the node fleet list must not run live once the snapshot is ready")
	assert.Equal(t, 0, f.client.listCount(&workercore.ModelPrefetchList{}),
		"the prefetch fleet list must not run live once the snapshot is ready")
	// Every admission decision still rests on live proof.
	assert.Equal(t, 2, f.client.getCount("node-1"), "the own node is read live every pass")
	assert.Equal(t, 2, f.client.getCount("prefetch"), "the candidate prefetch is proven live every pass")
	assert.GreaterOrEqual(t, f.client.getCount("grant"), 2,
		"the Binding grant is read live every pass (once by the pass, once more by authority setup)")
	assert.Equal(t, 2, f.client.getCount("/model"), "the artifact is proven live every pass")
}

func TestListIntentsStaleSnapshotFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		revoke func(t *testing.T, f *snapshotFixture)
	}{
		{
			name: "prefetch deleted live",
			revoke: func(t *testing.T, f *snapshotFixture) {
				require.NoError(t, f.client.Delete(context.Background(), f.prefetch))
			},
		},
		{
			name: "prefetch generation moved live",
			revoke: func(t *testing.T, f *snapshotFixture) {
				moved := f.prefetch.DeepCopy()
				moved.Generation = 2
				reader := ctrlinterceptor.NewClient(f.client.Client.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
					Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
						if pf, ok := obj.(*workercore.ModelPrefetch); ok && key.Name == f.prefetch.Name {
							*pf = *moved
							return nil
						}
						return cli.Get(ctx, key, obj, opts...)
					},
				})
				f.client.Client = reader
			},
		},
		{
			name: "prefetch withdrawn live",
			revoke: func(t *testing.T, f *snapshotFixture) {
				withdrawn := f.prefetch.DeepCopy()
				withdrawn.Spec.Source = nil
				reader := ctrlinterceptor.NewClient(f.client.Client.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
					Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
						if pf, ok := obj.(*workercore.ModelPrefetch); ok && key.Name == f.prefetch.Name {
							*pf = *withdrawn
							return nil
						}
						return cli.Get(ctx, key, obj, opts...)
					},
				})
				f.client.Client = reader
			},
		},
		{
			name: "binding deleted live",
			revoke: func(t *testing.T, f *snapshotFixture) {
				require.NoError(t, f.client.Delete(context.Background(), f.binding))
			},
		},
		{
			name: "store no longer selects the node",
			revoke: func(t *testing.T, f *snapshotFixture) {
				live := &workercore.ModelStore{}
				require.NoError(t, f.client.Get(context.Background(), ctrlcli.ObjectKey{Name: "store"}, live))
				live.Spec.NodeSelector = meta.LabelSelector{MatchLabels: map[string]string{"pool": "other"}}
				require.NoError(t, f.client.Update(context.Background(), live))
			},
		},
		{
			name: "artifact deleted live",
			revoke: func(t *testing.T, f *snapshotFixture) {
				require.NoError(t, f.client.Delete(context.Background(), f.model))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapshotFixture(t)
			tc.revoke(t, f)
			require.Empty(t, f.listIntents(t),
				"stale snapshot candidates must not admit against revoked live state")
		})
	}
}

func TestListIntentsSnapshotNotReadyKeepsLiveDiscovery(t *testing.T) {
	f := newSnapshotFixture(t)
	f.snapshot.set(false, nil, nil)
	require.Len(t, f.listIntents(t), 1)
	assert.Equal(t, 1, f.client.listCount(&core.NodeList{}),
		"before the caches sync, the fleet lists run live")
	assert.Equal(t, 1, f.client.listCount(&workercore.ModelPrefetchList{}))
}

// cachedPlacement is a snapshot-only prefetch placed next to the healthy
// fixture prefetch, so a test can prove the unrelated one is filtered
// without ever being read while the healthy one still admits.
func cachedPlacement(t *testing.T, f *snapshotFixture, name, uid string, placement *workercore.ModelPrefetchPlacement) *workercore.ModelPrefetch {
	t.Helper()
	prefetch := f.prefetch.DeepCopy()
	prefetch.Name, prefetch.UID, prefetch.ResourceVersion = name, types.UID(uid), ""
	prefetch.Spec.Placement = placement
	require.NoError(t, f.client.Create(context.Background(), prefetch))
	f.snapshot.set(true, f.snapshot.Nodes(),
		append(f.snapshot.Prefetches(), *prefetch.DeepCopy()))
	return prefetch
}

func TestListIntentsSnapshotFiltersUnrelatedPlacementsWithoutLiveReads(t *testing.T) {
	cases := []struct {
		name      string
		placement *workercore.ModelPrefetchPlacement
	}{
		{
			name:      "nodeSelector that excludes this node",
			placement: &workercore.ModelPrefetchPlacement{NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "other"}}},
		},
		{
			name:      "placement without a target set",
			placement: nil,
		},
		{
			name:      "empty placement",
			placement: &workercore.ModelPrefetchPlacement{},
		},
		{
			name: "ambiguous placement",
			placement: &workercore.ModelPrefetchPlacement{
				AcceleratorTypes: []string{"gpu"}, NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}},
			},
		},
		{
			name: "unparsable nodeSelector",
			placement: &workercore.ModelPrefetchPlacement{NodeSelector: &meta.LabelSelector{
				MatchExpressions: []meta.LabelSelectorRequirement{{Key: "pool", Operator: meta.LabelSelectorOperator("nope"), Values: []string{"source"}}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapshotFixture(t)
			cachedPlacement(t, f, "unrelated-prefetch", "unrelated-uid", tc.placement)
			require.Len(t, f.listIntents(t), 1, "the healthy prefetch must still admit")
			assert.Equal(t, 0, f.client.getCount("unrelated-prefetch"),
				"a placement the cache proves unrelated must not be read live")
		})
	}
}

func TestListIntentsAcceleratorTypePlacementStillResolvesLive(t *testing.T) {
	// An AcceleratorType-only placement cannot be excluded from cached facts
	// alone: it stays a candidate, is read live, and its pool resolution and
	// targeting run live. With no AcceleratorTypes in the cluster it targets
	// nothing and admits nothing.
	f := newSnapshotFixture(t)
	cachedPlacement(t, f, "typed-prefetch", "typed-uid",
		&workercore.ModelPrefetchPlacement{AcceleratorTypes: []string{"gpu"}})
	require.Len(t, f.listIntents(t), 1, "the healthy prefetch must still admit")
	assert.GreaterOrEqual(t, f.client.getCount("typed-prefetch"), 1,
		"an AcceleratorType-only candidate keeps its live read; the residual is reported in the handback")
}

func TestListIntentsSnapshotFollowsTargetedChanges(t *testing.T) {
	f := newSnapshotFixture(t)
	require.Len(t, f.listIntents(t), 1)
	f.snapshot.set(true, f.snapshot.Nodes(), nil)
	require.Empty(t, f.listIntents(t), "a withdrawn candidate leaves the resolved view")
	f.snapshot.set(true, f.snapshot.Nodes(), []workercore.ModelPrefetch{*f.prefetch.DeepCopy()})
	require.Len(t, f.listIntents(t), 1, "a restored candidate returns to the resolved view")
}

func TestListIntentsMissingLiveNodeAbortsThePass(t *testing.T) {
	f := newSnapshotFixture(t)
	f.client.Client = ctrlinterceptor.NewClient(f.client.Client.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
		Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
			if _, ok := obj.(*core.Node); ok {
				return kerrors.NewNotFound(core.Resource("nodes"), key.Name)
			}
			return cli.Get(ctx, key, obj, opts...)
		},
	})
	_, err := f.src.ListIntents(context.Background())
	require.Error(t, err, "a source node the live cluster lost must abort the pass")
}

// snapshotClosedSource answers the broker's private calls the way the
// control plane does, so admission can complete.
type snapshotClosedSource struct{}

func (s *snapshotClosedSource) Call(_ context.Context, path string, payload any) (broker.PrivateReply, error) {
	body := payload.(map[string]any)
	id := body["requestID"].(string)
	if path == "/v1/sessions/close" {
		return broker.PrivateReply{State: "closed"}, nil
	}
	if path == "/v1/operations" {
		return broker.PrivateReply{RequestID: id, State: "submitted"}, nil
	}
	if path == "/v1/operations/status" {
		return broker.PrivateReply{RequestID: id, State: "terminal", Verdict: "succeeded"}, nil
	}
	if path == "/v1/operations/ack" {
		return broker.PrivateReply{RequestID: id, State: "acknowledged", Verdict: "succeeded"}, nil
	}
	return broker.PrivateReply{}, errors.New("unexpected private call")
}

// staticListWatch serves one initial list and hands the given watch back;
// it refuses a streaming initial state so the reflector uses the list.
func staticListWatch(list runtime.Object, w watch.Interface) *cache.ListWatch {
	return &cache.ListWatch{
		ListFunc: func(options meta.ListOptions) (runtime.Object, error) {
			return list, nil
		},
		WatchFuncWithContext: func(_ context.Context, options meta.ListOptions) (watch.Interface, error) {
			if ptr.Deref(options.SendInitialEvents, false) {
				return nil, errors.New("this fixture serves no watch list")
			}
			return w, nil
		},
	}
}

func TestSourceDiscoveryInformersSyncReadAndSeeDeletes(t *testing.T) {
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "source"}}}
	prefetch := &workercore.ModelPrefetch{ObjectMeta: meta.ObjectMeta{Name: "prefetch", Namespace: "tenant", UID: "prefetch-uid", Generation: 1}, Spec: workercore.ModelPrefetchSpec{Source: &workercore.ModelPrefetchSource{}}}
	binding := &workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "binding-uid"}, Spec: workercore.ModelStoreBindingSpec{StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "store"}}}}
	modelStore := &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "store"}, Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": "source"}}}}

	nodeWatch, prefetchWatch := watch.NewFake(), watch.NewFake()
	idleWatches := []watch.Interface{nodeWatch, prefetchWatch, watch.NewFake(), watch.NewFake()}
	t.Cleanup(func() {
		for _, w := range idleWatches {
			w.Stop()
		}
	})
	nodes := cache.NewSharedIndexInformerWithOptions(
		staticListWatch(&core.NodeList{Items: []core.Node{*node}}, nodeWatch), &core.Node{},
		cache.SharedIndexInformerOptions{},
	)
	prefetches := cache.NewSharedIndexInformerWithOptions(
		staticListWatch(&workercore.ModelPrefetchList{Items: []workercore.ModelPrefetch{*prefetch}}, prefetchWatch), &workercore.ModelPrefetch{},
		cache.SharedIndexInformerOptions{Indexers: cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}},
	)
	bindings := cache.NewSharedIndexInformerWithOptions(
		staticListWatch(&workercore.ModelStoreBindingList{Items: []workercore.ModelStoreBinding{*binding}}, idleWatches[2]), &workercore.ModelStoreBinding{},
		cache.SharedIndexInformerOptions{Indexers: cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}},
	)
	stores := cache.NewSharedIndexInformerWithOptions(
		staticListWatch(&workercore.ModelStoreList{Items: []workercore.ModelStore{*modelStore}}, idleWatches[3]), &workercore.ModelStore{},
		cache.SharedIndexInformerOptions{Indexers: cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}},
	)
	d := newSourceDiscoveryFromInformers(nodes, prefetches, bindings, stores)
	{
		pending := cache.NewSharedIndexInformerWithOptions(
			staticListWatch(&core.NodeList{}, watch.NewFake()), &core.Node{}, cache.SharedIndexInformerOptions{},
		)
		empty := newSourceDiscoveryFromInformers(pending, prefetches, bindings, stores)
		assert.False(t, empty.Ready(), "a cache without its first sync is not ready")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	assert.False(t, d.Ready(), "nothing is ready before the watches run")
	d.Start(ctx)
	require.True(t, cache.WaitForCacheSync(ctx.Done(), nodes.HasSynced, prefetches.HasSynced, bindings.HasSynced, stores.HasSynced))
	assert.True(t, d.Ready(), "synced informers are ready once Start is running")
	assert.Equal(t, []core.Node{*node}, d.Nodes())
	require.Len(t, d.Prefetches(), 1)
	assert.True(t, d.coverage().Ready())
	require.Len(t, d.coverage().Candidates("tenant", node.Labels), 1)

	prefetchWatch.Delete(prefetch)
	require.Eventually(t, func() bool { return len(d.Prefetches()) == 0 },
		2*time.Second, 10*time.Millisecond,
		"a watch deletion reaches the candidate view")
	candidates := d.coverage().Candidates("tenant", node.Labels)
	require.Len(t, candidates, 1)
	assert.Equal(t, "grant", candidates[0].Name)
}
