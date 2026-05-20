package manager

import (
	"context"
	"fmt"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/workergateway/apis"
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

// AggregateInstanceTypes aggregates the instance types from the given clusters using the provided manager.
func AggregateInstanceTypes(ctx context.Context, clusters []string, manager Manager) (*apis.AggregatedInstanceTypeList, error) {
	gvk := worker.SchemeGroupVersionKind("InstanceType")
	opts := IteratorOptions{}

	var (
		list            apis.AggregatedInstanceTypeList
		itemIndexer     = make(map[apis.AggregatedInstanceTypeSpec]int)
		itemTierIndexer []map[string]int
	)

	processor := func(cluster string, obj runtime.Object) error {
		instType, ok := obj.(*worker.InstanceType)
		if !ok {
			return fmt.Errorf("object is not of type InstanceType")
		}

		itemIndex, existed := itemIndexer[instType.Spec]
		if !existed {
			itemIndex = len(list.Items)
			itemIndexer[instType.Spec] = itemIndex
			itemTierIndexer = append(itemTierIndexer, make(map[string]int))
			item := apis.AggregatedInstanceType{
				Name: instType.GenerateName,
				Spec: instType.Spec,
			}
			list.Items = append(list.Items, item)
		}

		item := &list.Items[itemIndex]
		tierIndexer := itemTierIndexer[itemIndex]

		if item.Status.Accelerator.OnceMaxRequest.Cmp(instType.Status.Accelerator.OnceMaxRequest) < 0 {
			item.Status.Accelerator.OnceMaxRequest = instType.Status.Accelerator.OnceMaxRequest
		}
		item.Status.Accelerator.Remaining.Add(instType.Status.Accelerator.Remaining)
		item.Status.Accelerator.Capacity.Add(instType.Status.Accelerator.Capacity)

		if item.Status.CPU.OnceMaxRequest.Cmp(instType.Status.CPU.OnceMaxRequest) < 0 {
			item.Status.CPU.OnceMaxRequest = instType.Status.CPU.OnceMaxRequest
		}
		item.Status.CPU.Remaining.Add(instType.Status.CPU.Remaining)
		item.Status.CPU.Capacity.Add(instType.Status.CPU.Capacity)

		if item.Status.RAM.OnceMaxRequest.Cmp(instType.Status.RAM.OnceMaxRequest) < 0 {
			item.Status.RAM.OnceMaxRequest = instType.Status.RAM.OnceMaxRequest
		}
		item.Status.RAM.Remaining.Add(instType.Status.RAM.Remaining)
		item.Status.RAM.Capacity.Add(instType.Status.RAM.Capacity)

		if item.Status.LocalStorage.OnceMaxRequest.Cmp(instType.Status.LocalStorage.OnceMaxRequest) < 0 {
			item.Status.LocalStorage.OnceMaxRequest = instType.Status.LocalStorage.OnceMaxRequest
		}
		item.Status.LocalStorage.Remaining.Add(instType.Status.LocalStorage.Remaining)
		item.Status.LocalStorage.Capacity.Add(instType.Status.LocalStorage.Capacity)

		tierIndexKey := instType.Status.Accelerator.OnceMaxRequest.String()
		tierIndex, existed := tierIndexer[tierIndexKey]
		if !existed {
			tierIndex = len(item.Status.Tiers)
			tierIndexer[tierIndexKey] = tierIndex
			tier := apis.AggregatedInstanceTypeOnceMaxRequestTier{
				OnceMaxRequest: instType.Status.Accelerator.OnceMaxRequest,
			}
			item.Status.Tiers = append(item.Status.Tiers, tier)
		}

		tier := &item.Status.Tiers[tierIndex]
		candidate := apis.AggregatedInstanceTypeOnceMaxRequestCandidate{
			Cluster:      cluster,
			InstanceType: instType.Name,
			CPU:          instType.Status.CPU.OnceMaxRequest,
			RAM:          instType.Status.RAM.OnceMaxRequest,
			LocalStorage: instType.Status.LocalStorage.OnceMaxRequest,
		}
		tier.Candidates = append(tier.Candidates, candidate)

		return nil
	}

	err := manager.IterateWorkers(ctx, clusters, gvk, opts, processor)
	if err != nil {
		return nil, fmt.Errorf("failed to iterate workers: %w", err)
	}

	return &list, nil
}
