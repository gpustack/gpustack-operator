package modeltransfer

import (
	"context"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	kubernetes "gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
)

// sourceSnapshot is the synced watch view a source pass selects candidates
// from. It carries no admission authority: the snapshot may nominate a
// candidate whose live state has already moved on, so every object a pass
// admits on is re-read live at its decision point — a stale nomination is
// denied, and a stale exclusion only withholds a candidate until its next
// watch event. Until Ready, callers read the cluster live.
type sourceSnapshot interface {
	// Ready reports whether the candidate caches finished their initial sync.
	Ready() bool
	// Nodes is the cached fleet the placement selectors match against.
	Nodes() []core.Node
	// Prefetches is the cached set of source preparations.
	Prefetches() []workercore.ModelPrefetch
}

// sourceDiscovery keeps watch-backed caches of the Node, ModelPrefetch,
// ModelStoreBinding, and ModelStore objects the source pass and metadata
// admission select candidates from. Start begins the watches; they end with
// the context. Reads before the first sync report not ready, and the
// callers fall back to live lists, so a cache that cannot start changes
// nothing but the read amplification.
type sourceDiscovery struct {
	nodes      cache.SharedIndexInformer
	prefetches cache.SharedIndexInformer
	bindings   cache.SharedIndexInformer
	stores     cache.SharedIndexInformer
}

// newSourceDiscovery builds the discovery caches over a cluster client.
func newSourceDiscovery(client kubernetes.Interface) *sourceDiscovery {
	return newSourceDiscoveryFromInformers(
		newListWatchInformer(client.CoreV1().Nodes(), &core.Node{}),
		newListWatchInformer(client.WorkerV1alpha1().ModelPrefetches(meta.NamespaceAll), &workercore.ModelPrefetch{}),
		newListWatchInformer(client.WorkerV1alpha1().ModelStoreBindings(meta.NamespaceAll), &workercore.ModelStoreBinding{}),
		newListWatchInformer(client.WorkerV1alpha1().ModelStores(), &workercore.ModelStore{}),
	)
}

func newSourceDiscoveryFromInformers(
	nodes, prefetches, bindings, stores cache.SharedIndexInformer,
) *sourceDiscovery {
	return &sourceDiscovery{nodes: nodes, prefetches: prefetches, bindings: bindings, stores: stores}
}

// Start launches every watch. It returns immediately; readiness arrives with
// the first successful list, and the watches end when ctx ends.
func (d *sourceDiscovery) Start(ctx context.Context) {
	for _, informer := range []cache.SharedIndexInformer{d.nodes, d.prefetches, d.bindings, d.stores} {
		go informer.Run(ctx.Done())
	}
}

// Ready reports whether the node and prefetch caches synced.
func (d *sourceDiscovery) Ready() bool {
	return d.nodes.HasSynced() && d.prefetches.HasSynced()
}

// Nodes reads the synced node store. The objects are the cache's own state,
// shared with the watch: callers read them and never write.
func (d *sourceDiscovery) Nodes() []core.Node {
	return snapshotItems[core.Node, *core.Node](d.nodes.GetStore())
}

// Prefetches reads the synced prefetch store.
func (d *sourceDiscovery) Prefetches() []workercore.ModelPrefetch {
	return snapshotItems[workercore.ModelPrefetch, *workercore.ModelPrefetch](d.prefetches.GetStore())
}

// coverage exposes the metadata authorization candidate selector over the
// binding and store caches.
func (d *sourceDiscovery) coverage() *authorization.Coverage {
	return authorization.NewCoverage(d.bindings, d.stores)
}

// snapshotItems copies the synced store's pointers into one value slice per
// pass. The copies share the cached objects' maps, so callers treat them as
// read-only observation.
func snapshotItems[T any, PT interface {
	*T
	runtime.Object
}](store cache.Store) []T {
	items := store.List()
	out := make([]T, 0, len(items))
	for _, item := range items {
		if typed, ok := item.(PT); ok {
			out = append(out, *typed)
		}
	}
	return out
}

// resourceListWatcher is the typed surface a shared informer watches.
type resourceListWatcher[T ctrlcli.Object, L ctrlcli.ObjectList] interface {
	List(ctx context.Context, opts meta.ListOptions) (L, error)
	Watch(ctx context.Context, opts meta.ListOptions) (watch.Interface, error)
}

// newListWatchInformer builds a shared informer over one typed cluster
// resource. The initial list is served at any resource version and the
// watch reestablishes with bookmarks. There is no resync: the store already
// holds the last observed state and no event handlers run.
func newListWatchInformer[T ctrlcli.Object, L ctrlcli.ObjectList, CS resourceListWatcher[T, L]](
	clientSet CS, object T,
) cache.SharedIndexInformer {
	listWatch := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts meta.ListOptions) (runtime.Object, error) {
			if opts.ResourceVersion == "" && opts.ResourceVersionMatch == "" {
				opts.ResourceVersion = "0"
			}
			return clientSet.List(ctx, opts)
		},
		WatchFuncWithContext: func(ctx context.Context, opts meta.ListOptions) (watch.Interface, error) {
			if !ptr.Deref(opts.SendInitialEvents, false) {
				opts.Watch = true
				opts.AllowWatchBookmarks = true
			}
			return clientSet.Watch(ctx, opts)
		},
	}
	informer := cache.NewSharedIndexInformerWithOptions(listWatch, object, cache.SharedIndexInformerOptions{
		Indexers: cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	})
	_ = informer.SetWatchErrorHandlerWithContext(cache.DefaultWatchErrorHandler)
	return informer
}
