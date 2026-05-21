package manager

import (
	"context"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
)

type TypedClientSet[T ctrlcli.Object, L ctrlcli.ObjectList] interface {
	Get(ctx context.Context, name string, opts meta.GetOptions) (T, error)
	List(ctx context.Context, opts meta.ListOptions) (L, error)
	Watch(ctx context.Context, opts meta.ListOptions) (watch.Interface, error)
}

// NewSharedIndexInformerWithOptions creates a new SharedIndexInformer for the given object type and client set.
func NewSharedIndexInformerWithOptions[T ctrlcli.Object, L ctrlcli.ObjectList, CS TypedClientSet[T, L]](
	clientSet CS, object T, resyncPeriod time.Duration,
) cache.SharedIndexInformer {
	lw := &cache.ListWatch{
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

	inf := cache.NewSharedIndexInformerWithOptions(lw, object, cache.SharedIndexInformerOptions{
		ResyncPeriod: resyncPeriod,
		Indexers:     cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	})
	_ = inf.SetWatchErrorHandlerWithContext(cache.DefaultWatchErrorHandler)

	return inf
}
