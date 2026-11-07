package authorization

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

// coverageReader records every live read the authorizer makes, so a test
// can require that an unrelated Binding or StoreRef is never read.
type coverageReader struct {
	ctrlcli.Client
	mu    sync.Mutex
	gets  map[string]int
	lists map[string]int
}

func newCoverageReader(objects ...ctrlcli.Object) *coverageReader {
	return &coverageReader{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build(),
		gets:   map[string]int{},
		lists:  map[string]int{},
	}
}

func (r *coverageReader) Get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
	r.mu.Lock()
	r.gets[fmt.Sprintf("%T %s/%s", obj, key.Namespace, key.Name)]++
	r.mu.Unlock()
	return r.Client.Get(ctx, key, obj, opts...)
}

func (r *coverageReader) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	r.mu.Lock()
	r.lists[fmt.Sprintf("%T", list)]++
	r.mu.Unlock()
	return r.Client.List(ctx, list, opts...)
}

func (r *coverageReader) getCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for key, count := range r.gets {
		if len(key) >= len(name) && key[len(key)-len(name):] == name {
			total += count
		}
	}
	return total
}

func (r *coverageReader) listCount(list ctrlcli.ObjectList) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists[fmt.Sprintf("%T", list)]
}

// startSyncedInformer runs an informer over a fixed initial state; its
// watch never fires, so the sync is one deterministic list.
func startSyncedInformer(t *testing.T, inf cache.SharedIndexInformer) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() { inf.Run(stop) }()
	if !cache.WaitForCacheSync(stop, inf.HasSynced) {
		t.Fatal("informer did not sync")
	}
}

// asItems copies the pointers into the value slice the list type carries.
func asItems[T any](pointers []*T) []T {
	items := make([]T, 0, len(pointers))
	for _, pointer := range pointers {
		items = append(items, *pointer)
	}
	return items
}

// quietWatch returns a watcher that never fires and never closes on its
// own, so the reflector holds one watch for the whole test.
func quietWatch(t *testing.T) watch.Interface {
	t.Helper()
	w := watch.NewFake()
	t.Cleanup(w.Stop)
	return w
}

// rejectInitialEvents makes the watch refuse a streaming initial state, so
// the reflector falls back to the fixture's plain list.
func rejectInitialEvents(t *testing.T, w watch.Interface) func(context.Context, meta.ListOptions) (watch.Interface, error) {
	t.Helper()
	return func(_ context.Context, opts meta.ListOptions) (watch.Interface, error) {
		if ptr.Deref(opts.SendInitialEvents, false) {
			return nil, errors.New("this fixture serves no watch list")
		}
		return w, nil
	}
}

func syncedBindingInformer(t *testing.T, bindings ...*workercore.ModelStoreBinding) cache.SharedIndexInformer {
	t.Helper()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(context.Context, meta.ListOptions) (runtime.Object, error) {
			return &workercore.ModelStoreBindingList{Items: asItems(bindings)}, nil
		},
		WatchFuncWithContext: rejectInitialEvents(t, quietWatch(t)),
	}
	inf := cache.NewSharedIndexInformerWithOptions(lw, &workercore.ModelStoreBinding{},
		cache.SharedIndexInformerOptions{Indexers: cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}})
	startSyncedInformer(t, inf)
	return inf
}

func syncedStoreInformer(t *testing.T, stores ...*workercore.ModelStore) cache.SharedIndexInformer {
	t.Helper()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(context.Context, meta.ListOptions) (runtime.Object, error) {
			return &workercore.ModelStoreList{Items: asItems(stores)}, nil
		},
		WatchFuncWithContext: rejectInitialEvents(t, quietWatch(t)),
	}
	inf := cache.NewSharedIndexInformerWithOptions(lw, &workercore.ModelStore{},
		cache.SharedIndexInformerOptions{Indexers: cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}})
	startSyncedInformer(t, inf)
	return inf
}

func unsyncedBindingInformer() cache.SharedIndexInformer {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(context.Context, meta.ListOptions) (runtime.Object, error) {
			return &workercore.ModelStoreBindingList{}, nil
		},
		WatchFuncWithContext: func(context.Context, meta.ListOptions) (watch.Interface, error) {
			return watch.NewEmptyWatch(), nil
		},
	}
	return cache.NewSharedIndexInformerWithOptions(lw, &workercore.ModelStoreBinding{}, cache.SharedIndexInformerOptions{})
}

func coveringStore(name, pool string) *workercore.ModelStore {
	return &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{
		Name: name, UID: types.UID("uid-" + name),
	}, Spec: workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": pool}}}}
}

func metadataBinding(namespace, name, uid string, storeNames ...string) *workercore.ModelStoreBinding {
	refs := make([]workercore.ModelStoreBindingStoreReference, 0, len(storeNames))
	for _, storeName := range storeNames {
		refs = append(refs, workercore.ModelStoreBindingStoreReference{Name: storeName})
	}
	return &workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{
		Namespace: namespace, Name: name, UID: types.UID(uid),
	}, Spec: workercore.ModelStoreBindingSpec{StoreRefs: refs}}
}

// coverageFixture holds the shared shape: the caller's namespace carries one
// unrelated Binding that cannot cover the node and one covering Binding
// whose first StoreRef covers and whose second StoreRef is unrelated.
type coverageFixture struct {
	env      *env
	reader   *coverageReader
	cached   *Coverage
	binding  *workercore.ModelStoreBinding
	unfriend *workercore.ModelStoreBinding
}

func newCoverageFixture(t *testing.T, extraLive ...ctrlcli.Object) *coverageFixture {
	t.Helper()
	e := newEnv(t)
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "source"}}}
	storeCovering := coveringStore("store-covering", "source")
	storeUnrelated := coveringStore("store-unrelated", "other")
	// The unrelated Binding sorts first, so the previous live scan read its
	// StoreRefs on every request before reaching the covering one.
	unfriend := metadataBinding("tenant-a", "binding-unrelated", "binding-uid-unrelated", "store-unrelated")
	binding := metadataBinding("tenant-a", "binding-covering", "binding-uid-covering", "store-covering", "store-unrelated")
	live := append([]ctrlcli.Object{
		runningPod("tenant-a", "engine-1", "pod-uid-1"),
		resolvedArtifact("tenant-a", "model", "artifact-uid-1", e.digest, true, true, 734),
		node, storeCovering, storeUnrelated, unfriend, binding,
	}, extraLive...)
	reader := newCoverageReader(live...)
	cached := NewCoverage(
		syncedBindingInformer(t, unfriend, binding),
		syncedStoreInformer(t, storeCovering, storeUnrelated),
	)
	return &coverageFixture{env: e, reader: reader, cached: cached, binding: binding, unfriend: unfriend}
}

func (f *coverageFixture) authorize(t *testing.T, mutate func(*Request)) *protocol.Error {
	t.Helper()
	auth := &Authorizer{Reader: f.reader, Reviewer: okReviewer(), Store: f.env.store, Coverage: f.cached}
	request := f.env.request()
	if mutate != nil {
		mutate(&request)
	}
	return auth.AuthorizeMetadata(context.Background(), request, "node-1")
}

func TestAuthorizeMetadataCoverageReadsOnlyTheSelectedGrant(t *testing.T) {
	f := newCoverageFixture(t)
	require.Nil(t, f.authorize(t, nil), "the covering Binding must admit the request")
	assert.Equal(t, 0, f.reader.listCount(&workercore.ModelStoreBindingList{}),
		"candidate selection must not list the namespace's Bindings live")
	assert.Equal(t, 1, f.reader.getCount("binding-covering"), "the selected grant is proven live")
	assert.Equal(t, 0, f.reader.getCount("binding-unrelated"), "an unrelated Binding is never read")
	assert.Equal(t, 1, f.reader.getCount("store-covering"), "the selected grant's covering store is proven live")
	assert.Equal(t, 0, f.reader.getCount("store-unrelated"), "an unrelated StoreRef is never read")
}

func TestAuthorizeMetadataCoverageFailsClosedOnLiveRevocation(t *testing.T) {
	cases := []struct {
		name   string
		revoke func(t *testing.T, f *coverageFixture)
	}{
		{
			name: "selected Binding deleted",
			revoke: func(t *testing.T, f *coverageFixture) {
				require.NoError(t, f.reader.Delete(context.Background(), f.binding))
			},
		},
		{
			name: "selected Binding deleting",
			revoke: func(t *testing.T, f *coverageFixture) {
				deleting := f.binding.DeepCopy()
				deleting.DeletionTimestamp = ptr.To(meta.Now())
				reader := ctrlinterceptor.NewClient(f.reader.Client.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
					Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
						if _, ok := obj.(*workercore.ModelStoreBinding); ok && key.Name == f.binding.Name {
							*obj.(*workercore.ModelStoreBinding) = *deleting
							return nil
						}
						return cli.Get(ctx, key, obj, opts...)
					},
				})
				f.reader.Client = reader
			},
		},
		{
			name: "selected store deleted",
			revoke: func(t *testing.T, f *coverageFixture) {
				require.NoError(t, f.reader.Delete(context.Background(),
					coveringStore("store-covering", "source")))
			},
		},
		{
			name: "selected store no longer selects the node",
			revoke: func(t *testing.T, f *coverageFixture) {
				live := &workercore.ModelStore{}
				require.NoError(t, f.reader.Get(context.Background(), ctrlcli.ObjectKey{Name: "store-covering"}, live))
				live.Spec.NodeSelector = meta.LabelSelector{MatchLabels: map[string]string{"pool": "other"}}
				require.NoError(t, f.reader.Update(context.Background(), live))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoverageFixture(t)
			tc.revoke(t, f)
			verdict := f.authorize(t, nil)
			require.NotNil(t, verdict, "a revoked grant must never admit")
			assert.Equal(t, protocol.KindDeniedContent, verdict.Kind)
			assert.Equal(t, 0, f.reader.listCount(&workercore.ModelStoreBindingList{}))
			assert.Equal(t, 0, f.reader.getCount("binding-unrelated"), "the unrelated Binding stays unread")
		})
	}
	t.Run("a revoked Binding's refs stay unread", func(t *testing.T) {
		f := newCoverageFixture(t)
		require.NoError(t, f.reader.Delete(context.Background(), f.binding))
		require.NotNil(t, f.authorize(t, nil))
		assert.Equal(t, 0, f.reader.getCount("store-covering"), "a deleted Binding's stores stay unread")
		assert.Equal(t, 0, f.reader.getCount("store-unrelated"))
	})
}

func TestAuthorizeMetadataCoverageFallsBackToAHealthyAlternative(t *testing.T) {
	f := newCoverageFixture(t)
	alternative := metadataBinding("tenant-a", "binding-second", "binding-uid-second", "store-second")
	store := coveringStore("store-second", "source")
	require.NoError(t, f.reader.Create(context.Background(), alternative))
	require.NoError(t, f.reader.Create(context.Background(), store))
	// The revoked candidate sorts first, so the proof has to move past a
	// failed candidate to the healthy one.
	f.cached = NewCoverage(
		syncedBindingInformer(t, f.unfriend, f.binding, alternative),
		syncedStoreInformer(t, coveringStore("store-covering", "source"),
			coveringStore("store-unrelated", "other"), store),
	)
	require.NoError(t, f.reader.Delete(context.Background(), f.binding))
	require.Nil(t, f.authorize(t, nil), "a healthy alternative Binding must admit")
	assert.Equal(t, 1, f.reader.getCount("binding-covering"), "the revoked candidate is attempted once")
	assert.Equal(t, 0, f.reader.getCount("store-covering"), "a deleted Binding's stores stay unread")
	assert.Equal(t, 1, f.reader.getCount("binding-second"))
	assert.Equal(t, 1, f.reader.getCount("store-second"))
}

func TestAuthorizeMetadataCoverageDeniesWithoutReadsWhenNoCandidateCovers(t *testing.T) {
	f := newCoverageFixture(t)
	f.cached = NewCoverage(
		syncedBindingInformer(t, f.unfriend),
		syncedStoreInformer(t, coveringStore("store-covering", "source")),
	)
	verdict := f.authorize(t, nil)
	require.NotNil(t, verdict)
	assert.Equal(t, protocol.KindDeniedContent, verdict.Kind)
	assert.Equal(t, 0, f.reader.listCount(&workercore.ModelStoreBindingList{}))
	assert.Equal(t, 0, f.reader.getCount("binding-unrelated"))
	assert.Equal(t, 0, f.reader.getCount("store-covering"), "a live store that no cached binding selects is never read")
}

func TestAuthorizeMetadataCoverageNotReadyReadsLive(t *testing.T) {
	f := newCoverageFixture(t)
	f.cached = NewCoverage(unsyncedBindingInformer(), unsyncedBindingInformer())
	require.Nil(t, f.authorize(t, nil), "before the caches sync, the live scan decides")
	assert.Equal(t, 1, f.reader.listCount(&workercore.ModelStoreBindingList{}),
		"the unsynced cache falls back to the live namespace scan")
	assert.Equal(t, 1, f.reader.getCount("store-covering"))
}

func TestAuthorizeMetadataCoverageUnreadableCandidateIsUnavailable(t *testing.T) {
	f := newCoverageFixture(t)
	internal := context.Canceled
	reader := ctrlinterceptor.NewClient(f.reader.Client.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
		Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
			if _, ok := obj.(*workercore.ModelStoreBinding); ok {
				return internal
			}
			return cli.Get(ctx, key, obj, opts...)
		},
	})
	f.reader.Client = reader
	verdict := f.authorize(t, nil)
	require.NotNil(t, verdict)
	assert.Equal(t, protocol.KindSourceUnavailable, verdict.Kind,
		"an unreadable grant is an unavailable source, never a quiet denial")
}

func TestAuthorizeMetadataWithoutCoverageKeepsTheLiveScan(t *testing.T) {
	f := newCoverageFixture(t)
	f.cached = nil
	require.Nil(t, f.authorize(t, nil))
	assert.Equal(t, 1, f.reader.listCount(&workercore.ModelStoreBindingList{}))
	assert.Equal(t, 1, f.reader.getCount("store-covering"))
}

func TestCoverageCandidatesSelectOnlyCoveringLiveBindings(t *testing.T) {
	t.Run("deleting binding is not a candidate", func(t *testing.T) {
		inf := syncedBindingInformer(t)
		binding := &workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{
			Namespace: "tenant-a", Name: "binding-deleting", UID: "uid-2",
			DeletionTimestamp: ptr.To(meta.Now()),
		}, Spec: workercore.ModelStoreBindingSpec{StoreRefs: []workercore.ModelStoreBindingStoreReference{{Name: "store-1"}}}}
		require.NoError(t, inf.GetStore().Add(binding))
		c := NewCoverage(inf, syncedStoreInformer(t, coveringStore("store-1", "source")))
		assert.Empty(t, c.Candidates("tenant-a", map[string]string{"pool": "source"}))
	})
	t.Run("second ref can cover", func(t *testing.T) {
		binding := metadataBinding("tenant-a", "binding-1", "uid-1", "store-1", "store-2")
		c := NewCoverage(
			syncedBindingInformer(t, binding),
			syncedStoreInformer(t, coveringStore("store-1", "other"), coveringStore("store-2", "source")),
		)
		candidates := c.Candidates("tenant-a", map[string]string{"pool": "source"})
		require.Len(t, candidates, 1)
		assert.Equal(t, "binding-1", candidates[0].Name)
	})
	t.Run("missing cached store fails closed", func(t *testing.T) {
		binding := metadataBinding("tenant-a", "binding-1", "uid-1", "store-unknown")
		c := NewCoverage(
			syncedBindingInformer(t, binding),
			syncedStoreInformer(t),
		)
		assert.Empty(t, c.Candidates("tenant-a", map[string]string{"pool": "source"}))
	})
	t.Run("unparsable selector fails closed", func(t *testing.T) {
		binding := metadataBinding("tenant-a", "binding-1", "uid-1", "store-1")
		broken := &workercore.ModelStore{ObjectMeta: meta.ObjectMeta{Name: "store-1"}, Spec: workercore.ModelStoreSpec{
			NodeSelector: meta.LabelSelector{MatchExpressions: []meta.LabelSelectorRequirement{{
				Key: "pool", Operator: meta.LabelSelectorOperator("nope"), Values: []string{"source"},
			}}},
		}}
		c := NewCoverage(
			syncedBindingInformer(t, binding),
			syncedStoreInformer(t, broken),
		)
		assert.Empty(t, c.Candidates("tenant-a", map[string]string{"pool": "source"}))
	})
	t.Run("other namespace is not selected", func(t *testing.T) {
		binding := metadataBinding("tenant-b", "binding-1", "uid-1", "store-1")
		c := NewCoverage(
			syncedBindingInformer(t, binding),
			syncedStoreInformer(t, coveringStore("store-1", "source")),
		)
		assert.Empty(t, c.Candidates("tenant-a", map[string]string{"pool": "source"}))
	})
	t.Run("candidates sort by name", func(t *testing.T) {
		later := metadataBinding("tenant-a", "binding-b", "uid-b", "store-1")
		earlier := metadataBinding("tenant-a", "binding-a", "uid-a", "store-1")
		c := NewCoverage(
			syncedBindingInformer(t, later, earlier),
			syncedStoreInformer(t, coveringStore("store-1", "source")),
		)
		candidates := c.Candidates("tenant-a", labels.Set{"pool": "source"})
		require.Len(t, candidates, 2)
		assert.Equal(t, "binding-a", candidates[0].Name)
		assert.Equal(t, "binding-b", candidates[1].Name)
	})
	t.Run("not ready selects nothing", func(t *testing.T) {
		c := NewCoverage(unsyncedBindingInformer(), unsyncedBindingInformer())
		assert.Empty(t, c.Candidates("tenant-a", map[string]string{"pool": "source"}))
	})
	t.Run("nil coverage selects nothing", func(t *testing.T) {
		var c *Coverage
		assert.Empty(t, c.Candidates("tenant-a", nil))
		assert.False(t, c.Ready())
	})
}
