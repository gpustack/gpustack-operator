package manager

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	klog "k8s.io/klog/v2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/worker/apis"
)

type (
	// ProcessObjectFunc defines a function type for processing a runtime.Object from a given cluster.
	// Returning an error will stop the iteration and return the error.
	ProcessObjectFunc = func(cluster string, obj runtime.Object) error

	// WorkerInfo represents the information of a subscribed worker,
	// including the cluster name and whether all informers are ready.
	WorkerInfo struct {
		Cluster  string `json:"cluster"`
		AllReady bool   `json:"allReady"`
	}

	// IteratorOptions defines the options for iterating over worker informers.
	IteratorOptions struct {
		// Namespace specifies the namespace to filter objects. If empty, it will iterate over all namespaces.
		Namespace string
		// DeepCopy indicates whether to deep copy the object before processing.
		// If false, it will pass the original object, which should not be modified.
		DeepCopy bool
		// LabelSelector specifies the label selector to filter objects.
		// If empty, it will not filter by labels.
		LabelSelector labels.Set
	}

	// Manager defines the interface for managing workers across multiple clusters.
	Manager interface {
		// SubscribeWorker subscribes a worker for the given cluster.
		// If force is true, it will unsubscribe the existing worker before subscribing a new one.
		SubscribeWorker(ctx context.Context, cluster string, force bool) error
		// UnsubscribeWorker unsubscribes the worker for the given cluster.
		UnsubscribeWorker(ctx context.Context, cluster string)
		// ListWorkers lists all subscribed workers with their status.
		ListWorkers(ctx context.Context) []WorkerInfo
		// IterateWorkers iterates over the informers of the subscribed workers for the given clusters and GroupVersionKind (GVK).
		// It applies the provided processor function to each object that matches the given options.
		IterateWorkers(ctx context.Context, clusters []string, gvk schema.GroupVersionKind, opts IteratorOptions, processor ProcessObjectFunc) error
	}
)

type (
	Cluster = string

	_Manager struct {
		sync.RWMutex

		Logger              klog.Logger
		Context             context.Context
		Clusters            []Cluster
		Workers             map[Cluster]*_Worker
		ConstructRestConfig ConstructRestConfigFunc
		ResyncPeriod        time.Duration
	}

	_Worker struct {
		Cluster   string
		Cancel    context.CancelFunc
		Informers map[schema.GroupVersionKind]cache.SharedIndexInformer
		AllReady  atomic.Bool
	}
)

// New creates a Manager with the given context and configuration.
func New(ctx context.Context, config *Config) (Manager, error) {
	return &_Manager{
		Logger:              klog.FromContext(ctx).WithName("manager"),
		Context:             ctx,
		ConstructRestConfig: config.ConstructRestConfig,
		ResyncPeriod:        config.ResyncPeriod,
		Workers:             make(map[Cluster]*_Worker),
	}, nil
}

func (wm *_Manager) SubscribeWorker(ctx context.Context, cluster string, force bool) error {
	logger := wm.Logger.WithValues("cluster", cluster)

	if force {
		wm.UnsubscribeWorker(ctx, cluster)
	} else if wm.hasWorker(cluster) {
		logger.Info("worker already exists, skip")
		return nil
	}

	cfg, err := wm.ConstructRestConfig(cluster)
	if err != nil {
		logger.Error(err, "construct rest config")
		return fmt.Errorf("construct rest config for cluster %q: %w", cluster, err)
	}

	cli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "construct kubernetes client")
		return fmt.Errorf("construct kubernetes client for cluster %q: %w", cluster, err)
	}

	wkCtx, wkCancel := context.WithCancel(klog.NewContext(wm.Context, logger))

	gox.Go(func() {
		for {
			select {
			case <-wkCtx.Done():
				return
			default:
			}

			if err := apis.WaitForServicesReady(wkCtx, cli); err != nil {
				logger.Error(err, "wait for api services ready")
				continue
			}
			logger.Info("api services are ready")

			wm.Lock()
			if _, ok := wm.Workers[cluster]; ok {
				wm.Unlock()
				logger.Info("worker already exists, skip")
				return
			}
			wk := newWorker(cluster, wkCancel, cli, wm.ResyncPeriod)
			wm.Clusters = append(wm.Clusters, cluster)
			wm.Workers[cluster] = wk
			logger.Info("subscribed worker")
			wm.Unlock()

			logger.Info("run worker")
			if err := wk.Run(wkCtx); err != nil {
				logger.Error(err, "run worker")
			}

			wm.Lock()
			delete(wm.Workers, cluster)
			wm.Unlock()
		}
	})

	return nil
}

func (wm *_Manager) UnsubscribeWorker(_ context.Context, cluster string) {
	if !wm.hasWorker(cluster) {
		return
	}

	wm.Lock()
	defer wm.Unlock()
	wk, ok := wm.Workers[cluster]
	if !ok {
		return
	}
	wk.Cancel()
	delete(wm.Workers, cluster)
	for i, c := range wm.Clusters {
		if c == cluster {
			wm.Clusters = append(wm.Clusters[:i], wm.Clusters[i+1:]...)
			break
		}
	}
	wm.Logger.Info("unsubscribed worker", "cluster", cluster)
}

func (wm *_Manager) hasWorker(cluster string) bool {
	wm.RLock()
	defer wm.RUnlock()

	_, ok := wm.Workers[cluster]
	return ok
}

func (wm *_Manager) ListWorkers(_ context.Context) []WorkerInfo {
	wm.RLock()
	defer wm.RUnlock()

	infos := make([]WorkerInfo, 0, len(wm.Workers))
	for cluster, wk := range wm.Workers {
		infos = append(infos, WorkerInfo{
			Cluster:  cluster,
			AllReady: wk.AllReady.Load(),
		})
	}
	return infos
}

func (wm *_Manager) IterateWorkers(
	_ context.Context, clusters []string, gvk schema.GroupVersionKind, opts IteratorOptions, processor ProcessObjectFunc,
) error {
	if len(clusters) == 0 {
		clusters = wm.Clusters
	}
	if gvk.Empty() {
		return nil
	}
	if processor == nil {
		return nil
	}

	var labelSel labels.Selector
	if len(opts.LabelSelector) != 0 {
		labelSel = labels.SelectorFromValidatedSet(opts.LabelSelector)
		if labelSel.Empty() {
			labelSel = nil
		}
	}

	for _, cluster := range clusters {
		logger := wm.Logger.WithValues("cluster", cluster, "gvk", gvk)

		wm.RLock()
		wk, ok := wm.Workers[cluster]
		if !ok {
			logger.Info("worker not found, skip")
			wm.RUnlock()
			continue
		}
		inf, ok := wk.Informers[gvk]
		if !ok {
			logger.Info("informer not found for gvk, skip")
			wm.RUnlock()
			continue
		}
		if !inf.HasSynced() {
			logger.Info("informer not synced, skip")
			wm.RUnlock()
			continue
		}
		wm.RUnlock()

		var objs []any
		if opts.Namespace == "" {
			objs = inf.GetIndexer().List()
		} else {
			objs, _ = inf.GetIndexer().ByIndex(cache.NamespaceIndex, opts.Namespace)
		}

		for _, obj := range objs {
			ro, ok := obj.(runtime.Object)
			if !ok {
				logger.Error(nil, "assert object to runtime.Object")
				continue
			}
			mo, err := meta.Accessor(ro)
			if err != nil {
				logger.Error(err, "access object meta")
				continue
			}
			if labelSel != nil && !labelSel.Matches(labels.Set(mo.GetLabels())) {
				continue
			}

			var oro runtime.Object
			if opts.DeepCopy {
				oro = ro.DeepCopyObject()
				oro.GetObjectKind().SetGroupVersionKind(gvk)
			} else {
				oro = ro
			}

			if err = processor(cluster, oro); err != nil {
				logger.Error(err, "process object")
				return fmt.Errorf("process object: %w", err)
			}
		}
	}

	return nil
}

func newWorker(cluster string, cancel context.CancelFunc, cli kubernetes.Interface, resyncPeriod time.Duration) *_Worker {
	wk := &_Worker{
		Cluster:   cluster,
		Cancel:    cancel,
		Informers: make(map[schema.GroupVersionKind]cache.SharedIndexInformer),
	}

	// InstanceType.
	instTypeGvk := worker.SchemeGroupVersionKind("InstanceType")
	instTypeInformer := NewSharedIndexInformerWithOptions(
		cli.WorkerV1().InstanceTypes(),
		&worker.InstanceType{},
		resyncPeriod,
	)
	wk.Informers[instTypeGvk] = instTypeInformer

	// Instance.
	inst := worker.SchemeGroupVersionKind("Instance")
	instInformer := NewSharedIndexInformerWithOptions(
		cli.WorkerV1().Instances(""),
		&worker.Instance{},
		resyncPeriod,
	)
	wk.Informers[inst] = instInformer

	// Add more informers here....

	return wk
}

func (w *_Worker) Run(ctx context.Context) error {
	gp := gox.GroupWithContextIn(ctx)

	for gvk := range w.Informers {
		inf := w.Informers[gvk]
		gp.Go(func(ctx context.Context) error {
			inf.RunWithContext(ctx)
			return ctx.Err()
		})
	}

	gp.Go(func(ctx context.Context) error {
		logger := klog.FromContext(ctx)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			allSynced := true
			for gvk, inf := range w.Informers {
				if !inf.HasSynced() {
					logger.Info("informer not synced", "gvk", gvk)
					allSynced = false
					break
				}
			}
			if !allSynced {
				time.Sleep(1 * time.Second)
				continue
			}
			w.AllReady.Store(true)
			logger.Info("all informers are synced")
			return ctx.Err()
		}
	})

	return gp.Wait()
}
