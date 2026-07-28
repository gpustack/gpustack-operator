package manager

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	core "k8s.io/api/core/v1"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
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
		// SubscribeWorker subscribes a worker for the given cluster and starts informers for the
		// given GroupVersionKinds. If gvks is empty, no informers are started: the worker stays
		// listable through the live-list proxy (see IterateWorkers), but a watch request for it
		// delivers no events, since events are published only by informers.
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

// newReadinessBackoff paces the retries of the worker api service readiness wait. Each wait already
// polls for 30s, so this only spaces a failed attempt from the next one; capping it at a minute
// keeps a cluster that never becomes ready from being probed — and logged — in a tight loop.
//
// It is handed out by value: wait.Backoff.Step has a pointer receiver, so a shared one would let a
// single cluster's retries pace every other cluster's.
func newReadinessBackoff() wait.Backoff {
	return wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2,
		Cap:      time.Minute,
		Steps:    math.MaxInt32,
	}
}

type (
	Cluster = string

	_Manager struct {
		sync.RWMutex

		Logger  klog.Logger
		Context context.Context
		Workers map[Cluster]*_Worker
		// Pending holds the subscribe attempts that have not registered a worker yet.
		// A worker is registered only once its api services are ready, so without this
		// a repeated subscribe starts a second readiness loop and an unsubscribe cannot
		// stop the one already running.
		Pending             map[Cluster]*_Pending
		ConstructRestConfig ConstructRestConfigFunc
		ResyncPeriod        time.Duration
		// WaitForServicesReady and ReadinessBackoff are the readiness policy, defaulted by
		// New. Tests replace them to drive the loop without waiting on real probes.
		WaitForServicesReady func(context.Context, kubernetes.Interface) error
		ReadinessBackoff     wait.Backoff
	}

	// _Pending is one subscribe attempt. It is compared by pointer, so an attempt that
	// finishes late never releases the claim of the attempt that replaced it.
	_Pending struct {
		Cancel context.CancelFunc
	}

	_Worker struct {
		Context   context.Context
		Cancel    context.CancelFunc
		Cluster   string
		Client    kubernetes.Interface
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
		Pending:             make(map[Cluster]*_Pending),

		WaitForServicesReady: apis.WaitForServicesReady,
		ReadinessBackoff:     newReadinessBackoff(),
	}, nil
}

func (wm *_Manager) SubscribeWorker(ctx context.Context, cluster, token string, gvks []schema.GroupVersionKind, force bool) error {
	logger := wm.Logger.WithValues("cluster", cluster)

	if force {
		wm.UnsubscribeWorker(ctx, cluster)
	}

	wkCtx, wkCancel := context.WithCancel(klog.NewContext(wm.Context, logger))

	// Claim the cluster before doing any work, so that a repeated subscribe cannot start a
	// second readiness loop and an unsubscribe can stop this attempt while it is still
	// waiting for the worker api services.
	pending, claimed := wm.claimPending(cluster, wkCancel)
	if !claimed {
		wkCancel()
		logger.V(2).Info("worker already subscribed or pending, skip")
		return nil
	}

	cfg, err := wm.ConstructRestConfig(cluster, token)
	if err != nil {
		wm.releasePending(cluster, pending)
		wkCancel()
		logger.Error(err, "construct rest config")
		return fmt.Errorf("construct rest config for cluster %q: %w", cluster, err)
	}

	cli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		wm.releasePending(cluster, pending)
		wkCancel()
		logger.Error(err, "construct kubernetes client")
		return fmt.Errorf("construct kubernetes client for cluster %q: %w", cluster, err)
	}

	gox.Go(func() {
		// The claim is held for as long as this loop owns the cluster, including while its
		// worker is registered, so no concurrent attempt can take it over.
		defer wm.releasePending(cluster, pending)

		backoff := wm.ReadinessBackoff
		reported := false

		for {
			select {
			case <-wkCtx.Done():
				return
			default:
			}

			logger.V(2).Info("checking worker api services")
			if err := wm.WaitForServicesReady(wkCtx, cli); err != nil {
				if wkCtx.Err() != nil {
					// Unsubscribed while waiting, so the wait was cut short on
					// purpose rather than failing.
					return
				}
				if reported {
					// The wait retries until the cluster becomes reachable, so only the
					// first failure of a run is reported as an error.
					logger.V(2).Info("worker api services still not ready", "err", err)
				} else {
					logger.Error(err, "wait for api services ready")
					reported = true
				}

				select {
				case <-wkCtx.Done():
					return
				case <-time.After(backoff.Step()):
				}
				continue
			}
			backoff = wm.ReadinessBackoff
			reported = false

			wm.Lock()
			// The wait can succeed just as this attempt is unsubscribed or replaced, and
			// registering then would hand out a worker on a canceled context and make the
			// replacement stand down. Only the attempt that still owns the cluster registers.
			if wkCtx.Err() != nil || wm.Pending[cluster] != pending {
				wm.Unlock()
				wkCancel()
				logger.V(2).Info("attempt no longer owns the cluster, skip")
				return
			}
			wk := newWorker(wkCtx, wkCancel, cluster, cli, wm.ResyncPeriod, gvks)
			wm.Workers[cluster] = wk
			wm.Unlock()

			logger.Info("subscribing worker", "gvks", gvks)
			// Subscribe returns the worker context's own cancellation once it is
			// unsubscribed, which is how it is meant to end — reporting that would put
			// an error in the log on every unsubscribe.
			if err := wk.Subscribe(); err != nil && wkCtx.Err() == nil {
				logger.Error(err, "subscribe worker")
			}

			wm.Lock()
			// Only this attempt's own registration may be dropped: a forced
			// re-subscribe replaces it, and unregistering the replacement here would
			// leave the cluster running with no entry.
			if wm.Workers[cluster] == wk {
				delete(wm.Workers, cluster)
			}
			wm.Unlock()
		}
	})

	return nil
}

func (wm *_Manager) UnsubscribeWorker(ctx context.Context, cluster string) {
	wm.Lock()

	// Drop the claim here instead of leaving it to the attempt's own cleanup, so a
	// following subscribe — a forced one in particular — can claim the cluster at once.
	pending, isPending := wm.Pending[cluster]
	delete(wm.Pending, cluster)

	wk, hasWorker := wm.Workers[cluster]
	delete(wm.Workers, cluster)
	wm.Unlock()

	if !isPending && !hasWorker {
		return
	}
	// Both teardowns run unlocked. Their targets are already unreachable through the
	// manager, and Unsubscribe publishes to the event bus — calling out to another
	// subsystem while holding this mutex is what makes a lock order a hazard.
	if hasWorker {
		wk.Unsubscribe(ctx)
	}
	if isPending {
		// Stops an attempt that never got as far as registering a worker, which would
		// otherwise keep probing the cluster for its api services forever.
		pending.Cancel()
	}
	if hasWorker {
		wm.Logger.Info("unsubscribed worker", "cluster", cluster)
	} else {
		wm.Logger.Info("canceled pending worker subscription", "cluster", cluster)
	}
}

// claimPending reserves the cluster for one subscribe attempt, reporting false when the cluster
// already has a worker or another attempt in progress.
func (wm *_Manager) claimPending(cluster string, cancel context.CancelFunc) (*_Pending, bool) {
	wm.Lock()
	defer wm.Unlock()

	if _, ok := wm.Workers[cluster]; ok {
		return nil, false
	}
	if _, ok := wm.Pending[cluster]; ok {
		return nil, false
	}

	pending := &_Pending{Cancel: cancel}
	wm.Pending[cluster] = pending
	return pending, true
}

// listClusters snapshots the clusters that currently have a registered worker, sorted so that
// iterating all of them stays in a stable order. It is derived from the registrations rather than
// tracked alongside them, so it cannot drift from them or report one cluster twice.
func (wm *_Manager) listClusters() []Cluster {
	wm.RLock()
	clusters := make([]Cluster, 0, len(wm.Workers))
	for cluster := range wm.Workers {
		clusters = append(clusters, cluster)
	}
	wm.RUnlock()

	// Ordered after unlocking: by then the snapshot is this call's own, and holding
	// subscribes and unsubscribes off while sorting it buys nothing.
	slices.Sort(clusters)

	return clusters
}

// releasePending drops the claim on the cluster if the given attempt still holds it.
func (wm *_Manager) releasePending(cluster string, pending *_Pending) {
	wm.Lock()
	defer wm.Unlock()

	if wm.Pending[cluster] == pending {
		delete(wm.Pending, cluster)
	}
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
	ctx context.Context,
	clusters []string,
	gvk schema.GroupVersionKind,
	opts IteratorOptions,
	processor ProcessObjectFunc,
) error {
	if len(clusters) == 0 {
		clusters = wm.listClusters()
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
		inf, hasInformer := wk.Informers[gvk]
		cli := wk.Client
		wm.RUnlock()

		// Objects come from the informer cache when the worker has one for this GVK, or from a live
		// per-cluster List (defaultListerFactories) as a read-through proxy when it does not — a GVK
		// the worker was subscribed without. A GVK backed by neither is skipped.
		var objs []any
		switch {
		case hasInformer:
			if !inf.HasSynced() {
				logger.Info("informer not synced, skip")
				continue
			}
			if opts.Namespace == "" {
				objs = inf.GetIndexer().List()
			} else {
				objs, _ = inf.GetIndexer().ByIndex(cache.NamespaceIndex, opts.Namespace)
			}
		default:
			lister, ok := defaultListerFactories[gvk]
			if !ok {
				logger.Info("no informer or lister for gvk, skip")
				continue
			}
			ros, err := lister(ctx, cli, opts.Namespace)
			if err != nil {
				// Skip this cluster and keep the others, matching the informer path's
				// graceful degradation for a transiently unavailable worker.
				logger.Error(err, "list objects, skip")
				continue
			}
			objs = ros
		}

		for _, obj := range objs {
			ro, ok := obj.(runtime.Object)
			if !ok {
				logger.Error(nil, "assert object to runtime.Object")
				continue
			}
			mo, err := kmeta.Accessor(ro)
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
		Client:    cli,
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
	ctx context.Context,
	inf cache.SharedIndexInformer,
	cluster string,
	gvk schema.GroupVersionKind,
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

	// A worker with informers keeps Wait blocked until cancel (its run-loops return ctx.Err then),
	// and any task error surfaces here — return it so SubscribeWorker can react. Only a worker with
	// no informers reaches Wait cleanly (nil) at once; block until it is unsubscribed so it stays
	// registered instead of being deleted and re-subscribed.
	if err := gp.Wait(); err != nil {
		return err
	}
	<-w.Context.Done()
	return nil
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
	worker.SchemeGroupVersionKind("InstanceTypeFlavor"): func(cli kubernetes.Interface, p time.Duration) cache.SharedIndexInformer {
		return NewSharedIndexInformerWithOptions(cli.WorkerV1().InstanceTypeFlavors(), &worker.InstanceTypeFlavor{}, p)
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

// _RuntimeObjectLister lists a resource and returns its typed list object, which is itself a
// runtime.Object. Every generated worker client satisfies it for its own list type.
type _RuntimeObjectLister[T runtime.Object] interface {
	List(ctx context.Context, opts meta.ListOptions) (T, error)
}

// listAll lists via lister (reading from the apiserver cache) and boxes each list item as a
// runtime.Object for the IterateWorkers loop, using kmeta.EachListItem to walk the typed items.
func listAll[T runtime.Object](ctx context.Context, lister _RuntimeObjectLister[T]) ([]any, error) {
	list, err := lister.List(ctx, meta.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return nil, err
	}

	var objs []any
	err = kmeta.EachListItem(list, func(obj runtime.Object) error {
		objs = append(objs, obj)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return objs, nil
}

// defaultListerFactories maps each worker GVK to its live per-cluster List, used by IterateWorkers
// as a read-through proxy whenever a worker has no informer for the requested GVK — an
// informer-backed GVK on a worker that was subscribed without it (an empty or partial GVK set).
var defaultListerFactories = map[schema.GroupVersionKind]func(context.Context, kubernetes.Interface, string) ([]any, error){
	worker.SchemeGroupVersionKind("Devices"): func(ctx context.Context, cli kubernetes.Interface, _ string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().Devices())
	},
	worker.SchemeGroupVersionKind("InstanceTypeFlavor"): func(ctx context.Context, cli kubernetes.Interface, _ string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().InstanceTypeFlavors())
	},
	worker.SchemeGroupVersionKind("InstanceType"): func(ctx context.Context, cli kubernetes.Interface, _ string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().InstanceTypes())
	},
	worker.SchemeGroupVersionKind("Instance"): func(ctx context.Context, cli kubernetes.Interface, ns string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().Instances(ns))
	},
	worker.SchemeGroupVersionKind("InstancePersistentVolumeType"): func(ctx context.Context, cli kubernetes.Interface, _ string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().InstancePersistentVolumeTypes())
	},
	worker.SchemeGroupVersionKind("InstancePersistentVolume"): func(ctx context.Context, cli kubernetes.Interface, ns string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().InstancePersistentVolumes(ns))
	},
	worker.SchemeGroupVersionKind("InstanceImagePullSecret"): func(ctx context.Context, cli kubernetes.Interface, ns string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().InstanceImagePullSecrets(ns))
	},
	worker.SchemeGroupVersionKind("InstanceSSHPublicKey"): func(ctx context.Context, cli kubernetes.Interface, ns string) ([]any, error) {
		return listAll(ctx, cli.WorkerV1().InstanceSSHPublicKeys(ns))
	},
}
