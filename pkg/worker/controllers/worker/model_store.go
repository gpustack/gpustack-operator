package worker

import (
	"context"
	"maps"
	"sort"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// The conditions of a ModelStore's status.
const (
	// ModelStoreConditionReady says the store's policy is applied on every matched node: each one's
	// NodeModelStore names this store as its effective configuration source.
	ModelStoreConditionReady = "Ready"
	// ModelStoreConditionSelectorOverlap says another store's selector also matches at least one of
	// this store's nodes. The node still gets a deterministic answer — the alphabetically first
	// store wins — and both stores carry this condition, so the misconfiguration is visible where
	// it was made.
	ModelStoreConditionSelectorOverlap = "SelectorOverlap"
)

// selectModelStore narrows the stores to the ones whose selector matches the node, ordered by
// name: the first entry is the node's effective store, and the length is the overlap count.
func selectModelStore(stores []workercore.ModelStore, node *core.Node) ([]*workercore.ModelStore, error) {
	nodeLabels := labels.Set(node.Labels)
	var matched []*workercore.ModelStore
	for i := range stores {
		sel, err := meta.LabelSelectorAsSelector(&stores[i].Spec.NodeSelector)
		if err != nil {
			return nil, err
		}
		if sel.Matches(nodeLabels) {
			matched = append(matched, &stores[i])
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Name < matched[j].Name })

	return matched, nil
}

// modelStoreLayer reads a store's policy as one merge layer. A field the store leaves out stays
// nil, so the layers under it keep their say — the store cannot restate a default by accident, and
// an explicit one, bytesPerSecond 0 among them, is a real override.
func modelStoreLayer(store *workercore.ModelStore) modelstore.Layer {
	var layer modelstore.Layer
	if w := store.Spec.Watermarks; w != nil {
		layer.HighWatermarkPercent = &w.HighPercent
		layer.LowWatermarkPercent = &w.LowPercent
	}
	if d := store.Spec.Download; d != nil {
		layer.DownloadConcurrency = d.Concurrency
		layer.DownloadBytesPerSecond = d.BytesPerSecond
	}

	return layer
}

// ModelStoreReconciler keeps a store's status equal to what the pool actually is: the nodes the
// selector matches, the cache those nodes hold, and whether another store reaches into the same
// nodes. The per-node effects of the policy belong to the NodeModelStoreReconciler, which writes
// them into each node's spec.
type ModelStoreReconciler struct {
	Client ctrlcli.Client
	Now    func() time.Time
}

var _ ctrlreconcile.Reconciler = (*ModelStoreReconciler)(nil)

// modelStoreObservation is one pass's reading of the pool, the raw material of the status.
type modelStoreObservation struct {
	nodes   int32 // nodes the selector matches
	applied int32 // matched nodes whose spec names this store
	overlap bool  // another store matches at least one matched node

	capacity       bool  // at least one matched node reported a capacity reading
	capacityTotal  int64 // summed sizes of the matched nodes' cache filesystems
	capacityStored int64 // summed published trees and partial downloads
}

func (r *ModelStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	store := new(workercore.ModelStore)
	switch err := r.Client.Get(ctx, req.NamespacedName, store); {
	case kerrors.IsNotFound(err):
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	}
	if store.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	selector, err := meta.LabelSelectorAsSelector(&store.Spec.NodeSelector)
	if err != nil {
		// The API server's own schema refuses an unparseable selector on write, so this takes a hand
		// edit; report the store plainly broken and wait for its next generation.
		logger.Error(err, "the store's nodeSelector does not parse; its status reports the store broken")
		before := store.Status.DeepCopy()
		store.Status.Conditions = carryConditionTransitions(store.Status.Conditions,
			modelStoreConditions(store, &modelStoreObservation{}, err), r.now())

		return ctrl.Result{}, modelStoreCommitStatus(ctx, r.Client, store, before)
	}

	nodes := new(core.NodeList)
	listOpts := []ctrlcli.ListOption{}
	if m := store.Spec.NodeSelector.MatchLabels; len(m) > 0 && len(store.Spec.NodeSelector.MatchExpressions) == 0 {
		// A pure matchLabels selector filters in the cache: the cluster's nodes never all need to
		// cross the wire for a pool that names three labels.
		listOpts = append(listOpts, ctrlcli.MatchingLabels(m))
	}
	if err := r.Client.List(ctx, nodes, listOpts...); err != nil {
		return ctrl.Result{}, err
	}
	stores := new(workercore.ModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		return ctrl.Result{}, err
	}
	// Every store but this one, for the overlap reading.
	others := make([]workercore.ModelStore, 0, len(stores.Items))
	for i := range stores.Items {
		if stores.Items[i].Name != store.Name {
			others = append(others, stores.Items[i])
		}
	}

	reports := new(workercore.NodeModelStoreList)
	if err := r.Client.List(ctx, reports); err != nil {
		return ctrl.Result{}, err
	}
	reportByName := map[string]*workercore.NodeModelStore{}
	for i := range reports.Items {
		reportByName[reports.Items[i].Name] = &reports.Items[i]
	}

	obs := modelStoreObservation{}
	for i := range nodes.Items {
		nd := &nodes.Items[i]
		if nd.DeletionTimestamp != nil || !selector.Matches(labels.Set(nd.Labels)) {
			continue
		}
		obs.nodes++

		for j := range others {
			other, err := meta.LabelSelectorAsSelector(&others[j].Spec.NodeSelector)
			if err != nil {
				continue
			}
			if other.Matches(labels.Set(nd.Labels)) {
				obs.overlap = true
				break
			}
		}

		nms, ok := reportByName[nd.Name]
		if !ok {
			// The node has no plugin object yet, so nothing of this store's is applied on it.
			continue
		}
		if nms.Spec.Store == store.Name {
			obs.applied++
		}
		if c := nms.Status.Capacity; c != nil {
			obs.capacity = true
			obs.capacityTotal += c.TotalBytes
			obs.capacityStored += c.StoredBytes
		}
	}

	before := store.Status.DeepCopy()
	store.Status.Conditions = carryConditionTransitions(store.Status.Conditions,
		modelStoreConditions(store, &obs, nil), r.now())
	store.Status.Nodes = obs.nodes
	if obs.capacity {
		store.Status.Capacity = &workercore.ModelStoreCapacity{
			TotalBytes: obs.capacityTotal, StoredBytes: obs.capacityStored,
		}
	} else {
		store.Status.Capacity = nil
	}
	if kubemeta.DeepEqual(before, &store.Status) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.Client.Status().Update(ctx, store)
}

// modelStoreConditions turns an observation into the status's conditions.
func modelStoreConditions(store *workercore.ModelStore, obs *modelStoreObservation, broken error) []gpustack.Condition {
	ready := gpustack.Condition{Type: ModelStoreConditionReady, ObservedGeneration: store.Generation}
	switch {
	case broken != nil:
		ready.Status, ready.Reason = meta.ConditionFalse, "SelectorInvalid"
		ready.Message = "the nodeSelector does not parse, so no node can be matched"
	case obs.nodes == 0:
		ready.Status, ready.Reason = meta.ConditionTrue, "NoNodes"
		ready.Message = "the selector matches no nodes"
	case obs.applied == obs.nodes:
		ready.Status, ready.Reason = meta.ConditionTrue, "Applied"
		ready.Message = "the policy is applied on every matched node"
	default:
		ready.Status, ready.Reason = meta.ConditionFalse, "Applying"
		ready.Message = "the policy is applied on some matched nodes and pending on the rest"
	}
	if obs.overlap && ready.Status == meta.ConditionFalse {
		// Only the losing store's message is overwritten: a store that reads Applied or NoNodes
		// keeps its own pairing, and the overlap fact already lives on SelectorOverlap.
		ready.Message = "the selector overlaps another store's, so the alphabetically first store wins the shared nodes"
		ready.Reason = "Overridden"
	}

	overlap := gpustack.Condition{
		Type:               ModelStoreConditionSelectorOverlap,
		Status:             meta.ConditionFalse,
		Reason:             "Exclusive",
		Message:            "no other store matches this store's nodes",
		ObservedGeneration: store.Generation,
	}
	if obs.overlap {
		overlap.Status, overlap.Reason = meta.ConditionTrue, "SharedNodes"
		overlap.Message = "another store's selector matches at least one of this store's nodes; the alphabetically first name wins them"
	}

	return []gpustack.Condition{ready, overlap}
}

// carryConditionTransitions keeps a condition's transition time while its status holds, so an
// unchanged pool reads equal to the stored status and writes nothing.
func carryConditionTransitions(old, next []gpustack.Condition, now time.Time) []gpustack.Condition {
	for i := range next {
		next[i].LastTransitionTime = meta.NewTime(now.UTC().Truncate(time.Second))
		for _, o := range old {
			if o.Type == next[i].Type && o.Status == next[i].Status {
				next[i].LastTransitionTime = o.LastTransitionTime
			}
		}
	}

	return next
}

func (r *ModelStoreReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

// modelStoreCommitStatus writes the status when this pass changed it.
func modelStoreCommitStatus(ctx context.Context, cli ctrlcli.Client, store *workercore.ModelStore, before *workercore.ModelStoreStatus) error {
	if kubemeta.DeepEqual(before, &store.Status) {
		return nil
	}

	return cli.Status().Update(ctx, store)
}

func (r *ModelStoreReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modelstore").
		For(&workercore.ModelStore{}, ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{})).
		Watches(
			&core.Node{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllStores),
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
				DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					o, ok := e.ObjectOld.(*core.Node)
					if !ok {
						return false
					}
					n, ok := e.ObjectNew.(*core.Node)
					if !ok {
						return false
					}

					return !maps.Equal(o.Labels, n.Labels)
				},
				GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
			}),
		).
		Watches(
			&workercore.NodeModelStore{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllStores),
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
				DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					o, ok := e.ObjectOld.(*workercore.NodeModelStore)
					if !ok {
						return false
					}
					n, ok := e.ObjectNew.(*workercore.NodeModelStore)
					if !ok {
						return false
					}

					// Only the two facts a store's status reads move it: which store the node
					// names, and what the node's cache holds. The rest of the node's report —
					// per-model churn the plugin writes on thresholds — is another object's feed.
					return o.Spec.Store != n.Spec.Store ||
						!kubemeta.DeepEqual(o.Status.Capacity, n.Status.Capacity)
				},
				GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
			}),
		).
		Watches(
			&workercore.ModelStore{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueSiblingStores),
			ctrlbuilder.WithPredicates(modelStoreSelectorChanged),
		).
		Complete(r)
}

// modelStoreSelectorChanged wakes the sibling stores only when a store's overlap-relevant shape
// changed: a create or a delete always can move another store's overlap view, an update only when
// the nodeSelector moved. Status writes and policy churn decide nothing about which nodes a store
// shares, and never wake anyone — which is what keeps the watch from feeding on its own reconciles.
var modelStoreSelectorChanged = ctrlpredicate.Funcs{
	CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
	DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.ModelStore)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.ModelStore)
		if !ok {
			return false
		}

		return !kubemeta.DeepEqual(o.Spec.NodeSelector, n.Spec.NodeSelector)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

// enqueueSiblingStores enqueues every store but the changed one, which its own For event already
// reconciles: a selector moving into or out of another store's nodes changes that store's overlap
// view. Stores are few, so the map is deliberately coarse — the narrow part is the predicate.
func (r *ModelStoreReconciler) enqueueSiblingStores(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	stores := new(workercore.ModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model stores")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(stores.Items))
	for i := range stores.Items {
		if stores.Items[i].Name == obj.GetName() {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: stores.Items[i].Name}})
	}

	return reqs
}

// enqueueAllStores re-enqueues every store: a node's labels decide which stores match it, and a
// node report decides what its pool's capacity reads. Stores are few; the map is deliberately
// coarse.
func (r *ModelStoreReconciler) enqueueAllStores(ctx context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	stores := new(workercore.ModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model stores")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(stores.Items))
	for i := range stores.Items {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: stores.Items[i].Name}})
	}

	return reqs
}
