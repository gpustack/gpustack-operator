package manager

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/utils/events/topic"
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
		Cluster  string                    `json:"cluster"`
		AllReady bool                      `json:"allReady"`
		GVKs     []schema.GroupVersionKind `json:"gvks"`
	}

	// WorkerEventType defines the type of events observed by worker informers,
	// which is the same as watch.EventType.
	WorkerEventType = watch.EventType

	// WorkerEvent represents an event observed by a worker informer.
	WorkerEvent struct {
		// Type is the type of the event, which can be Added, Modified, or Deleted.
		Type WorkerEventType `json:"type"`
		// Cluster is the name of the cluster where the event occurred.
		Cluster string `json:"cluster"`
		// Object is the object associated with the event.
		// if the event type is Deleted, and the object is nil,
		// that means to delete all objects of the cluster.
		Object any `json:"object"`
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
		// SubscribeWorker subscribes a worker for the given cluster and starts informers
		// for the given GroupVersionKinds. If gvks is empty, all built-in GVKs are used.
		// If force is true, it will unsubscribe the existing worker before subscribing a new one.
		SubscribeWorker(ctx context.Context, cluster, token string, gvks []schema.GroupVersionKind, force bool) error
		// UnsubscribeWorker unsubscribes the worker for the given cluster.
		UnsubscribeWorker(ctx context.Context, cluster string)
		// ListWorkers lists all subscribed workers with their status.
		ListWorkers(ctx context.Context) []WorkerInfo
		// IterateWorkers iterates over the informers of the subscribed workers for the given clusters and GroupVersionKind (GVK).
		// It applies the provided processor function to each object that matches the given options.
		IterateWorkers(ctx context.Context, clusters []string, gvk schema.GroupVersionKind, opts IteratorOptions, processor ProcessObjectFunc) error
	}
)

func (we *WorkerEvent) DeepCopy() *WorkerEvent {
	if we == nil {
		return nil
	}

	ro, ok := we.Object.(runtime.Object)
	if ok {
		return &WorkerEvent{
			Type:    we.Type,
			Cluster: we.Cluster,
			Object:  ro.DeepCopyObject(),
		}
	}

	return &WorkerEvent{
		Type:    we.Type,
		Cluster: we.Cluster,
		Object:  we.Object,
	}
}

const (
	WorkerEventAdded    = watch.Added
	WorkerEventModified = watch.Modified
	WorkerEventDeleted  = watch.Deleted
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
		Context   context.Context
		Cancel    context.CancelFunc
		Cluster   string
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

func (wm *_Manager) SubscribeWorker(ctx context.Context, cluster, token string, gvks []schema.GroupVersionKind, force bool) error {
	logger := wm.Logger.WithValues("cluster", cluster)

	if force {
		wm.UnsubscribeWorker(ctx, cluster)
	} else if wm.hasWorker(cluster) {
		logger.V(2).Info("worker already exists, skip")
		return nil
	}

	if len(gvks) == 0 {
		gvks = defaultGVKs()
	}

	cfg, err := wm.ConstructRestConfig(cluster, token)
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

			logger.Info("checking worker api services")
			if err := apis.WaitForServicesReady(wkCtx, cli); err != nil {
				logger.Error(err, "wait for api services ready")
				continue
			}

			wm.Lock()
			if _, ok := wm.Workers[cluster]; ok {
				wm.Unlock()
				logger.Info("worker already exists, skip")
				return
			}
			wk := newWorker(wkCtx, wkCancel, cluster, cli, wm.ResyncPeriod, gvks)
			wm.Clusters = append(wm.Clusters, cluster)
			wm.Workers[cluster] = wk
			wm.Unlock()

			logger.Info("subscribing worker", "gvks", gvks)
			if err := wk.Subscribe(); err != nil {
				logger.Error(err, "subscribe worker")
			}

			wm.Lock()
			delete(wm.Workers, cluster)
			wm.Unlock()
		}
	})

	return nil
}

func (wm *_Manager) UnsubscribeWorker(ctx context.Context, cluster string) {
	if !wm.hasWorker(cluster) {
		return
	}

	wm.Lock()
	defer wm.Unlock()
	wk, ok := wm.Workers[cluster]
	if !ok {
		return
	}
	wk.Unsubscribe(ctx)
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
		gvks := make([]schema.GroupVersionKind, 0, len(wk.Informers))
		for gvk := range wk.Informers {
			gvks = append(gvks, gvk)
		}
		infos = append(infos, WorkerInfo{
			Cluster:  cluster,
			AllReady: wk.AllReady.Load(),
			GVKs:     gvks,
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

// WorkerEventTopic returns the topic.Topic used to publish and subscribe
// WorkerEvent for the given GroupVersionKind.
func WorkerEventTopic(gvk schema.GroupVersionKind) topic.Topic {
	return topic.Topic("workergateway/" + gvk.String())
}

// newWorker constructs a _Worker that starts a SharedIndexInformer for each given GVK.
// GVKs without a registered informer factory are skipped with a log message.
func newWorker(
	ctx context.Context,
	cancel context.CancelFunc,
	cluster string,
	cli kubernetes.Interface,
	resyncPeriod time.Duration,
	gvks []schema.GroupVersionKind,
) *_Worker {
	logger := klog.FromContext(ctx)

	wk := &_Worker{
		Context:   ctx,
		Cancel:    cancel,
		Cluster:   cluster,
		Informers: make(map[schema.GroupVersionKind]cache.SharedIndexInformer),
	}

	seen := make(map[schema.GroupVersionKind]struct{}, len(gvks))
	for _, gvk := range gvks {
		if _, ok := seen[gvk]; ok {
			continue
		}
		seen[gvk] = struct{}{}

		factory, ok := defaultInformerFactories[gvk]
		if !ok {
			logger.Info("no informer factory registered for gvk, skip", "gvk", gvk)
			continue
		}
		inf := factory(cli, resyncPeriod)
		registerEventHandler(ctx, inf, cluster, gvk)
		wk.Informers[gvk] = inf
	}

	return wk
}

func registerEventHandler(
	ctx context.Context, inf cache.SharedIndexInformer, cluster string, gvk schema.GroupVersionKind,
) {
	t := WorkerEventTopic(gvk)
	publishEvent := func(et watch.EventType, obj any) {
		ro, ok := obj.(runtime.Object)
		if !ok {
			if dfu, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				ro, _ = dfu.Obj.(runtime.Object)
			}
		}
		if ro == nil {
			return
		}
		_ = topic.Publish(ctx, t, &WorkerEvent{
			Type:    et,
			Cluster: cluster,
			Object:  ro,
		})
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { publishEvent(WorkerEventAdded, obj) },
		UpdateFunc: func(_, obj any) { publishEvent(WorkerEventModified, obj) },
		DeleteFunc: func(obj any) { publishEvent(WorkerEventDeleted, obj) },
	}
	opts := cache.HandlerOptions{
		Logger: ptr.To(klog.FromContext(ctx).WithValues("gvk", gvk).V(4)),
	}

	_, _ = inf.AddEventHandlerWithOptions(handler, opts)
}

func (w *_Worker) Subscribe() error {
	gp := gox.GroupWithContextIn(w.Context)

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
			for _, inf := range w.Informers {
				if !inf.HasSynced() {
					allSynced = false
					break
				}
			}
			if !allSynced {
				time.Sleep(1 * time.Second)
				continue
			}
			w.AllReady.Store(true)
			logger.V(2).Info("synced all informers")
			return ctx.Err()
		}
	})

	return gp.Wait()
}

func (w *_Worker) Unsubscribe(ctx context.Context) {
	w.Cancel()

	for gvk := range w.Informers {
		t := WorkerEventTopic(gvk)
		_ = topic.Publish(ctx, t, &WorkerEvent{
			Type:    WorkerEventDeleted,
			Cluster: w.Cluster,
			Object:  nil,
		})
	}
}

var defaultInformerFactories = map[schema.GroupVersionKind]func(kubernetes.Interface, time.Duration) cache.SharedIndexInformer{
	worker.SchemeGroupVersionKind("Devices"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().Devices(), &worker.Devices{}, p)
	},
	worker.SchemeGroupVersionKind("InstanceType"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().InstanceTypes(), &worker.InstanceType{}, p)
	},
	worker.SchemeGroupVersionKind("Instance"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().Instances(core.NamespaceAll), &worker.Instance{}, p)
	},
	worker.SchemeGroupVersionKind("InstancePersistentVolumeType"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().InstancePersistentVolumeTypes(), &worker.InstancePersistentVolumeType{}, p)
	},
	worker.SchemeGroupVersionKind("InstancePersistentVolume"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().InstancePersistentVolumes(core.NamespaceAll), &worker.InstancePersistentVolume{}, p)
	},
	worker.SchemeGroupVersionKind("InstanceImagePullSecret"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().InstanceImagePullSecrets(core.NamespaceAll), &worker.InstanceImagePullSecret{}, p)
	},
	worker.SchemeGroupVersionKind("InstanceSSHPublicKey"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().InstanceSSHPublicKeys(core.NamespaceAll), &worker.InstanceSSHPublicKey{}, p)
	},
}

func defaultGVKs() []schema.GroupVersionKind {
	gvks := make([]schema.GroupVersionKind, 0, len(defaultInformerFactories))
	for gvk := range defaultInformerFactories {
		gvks = append(gvks, gvk)
	}
	return gvks
}
