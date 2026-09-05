package worker

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// NodeQueueReconciler owns the quota and admission gating of an operator-owned Kueue
// ClusterQueue — its resource groups, the StopPolicy, and the node-devices AdmissionCheck
// reference — driven by ClusterQueue, ResourceFlavor, and AdmissionCheck changes. The
// InstanceTypeReconciler owns the queue's lifecycle (creation, schedule labels,
// cohort/preemption isolation, and deletion); this reconciler
// converges the credit/CPU quota from the flavors alone — it does not look at the owning
// InstanceType:
//   - Being deleted: drive HoldAndDrain unconditionally so Kueue evicts the admitted workloads
//     and can then drop its own finalizer and remove the queue — Kueue never evicts on delete by
//     itself. (This covers both an admin's direct delete and the InstanceType teardown's delete.)
//   - Flavors present: fill the resource groups from the flavors, smallest per-node count
//     first so Kueue packs small nodes before large ones, reference the node-devices
//     AdmissionCheck on an accelerated derived queue once it is Active, and reactivate a queue
//     that had been drained to empty (StopPolicy None).
//   - No flavors, quota still defined: gated by instance-type-drain-when-no-flavors, drive the
//     queue to HoldAndDrain and requeue until every reservation clears, then empty the resource
//     groups — so Kueue's reservation counters never go negative.
//
// Reactivation fires only on a queue whose resource groups are already empty, so it never
// contends with a drain still in progress.
type NodeQueueReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeQueueReconciler)(nil)

// _ClusterQueueResType is the systemmeta resource type carried by the backing
// ClusterQueue this reconciler owns.
const _ClusterQueueResType = "instancetypes"

func (r *NodeQueueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	cq := new(kueue.ClusterQueue)
	err := r.Client.Get(ctx, req.NamespacedName, cq,
		ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch cluster queue")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// The queue is being deleted (an admin's delete, or the InstanceType teardown). Kueue holds
	// its own ResourceInUse finalizer until the queue is empty but never evicts admitted
	// workloads on its own, so drive HoldAndDrain to evict them; once every reservation clears
	// Kueue drops its finalizer and removes the queue. This drain is unconditional —
	// instance-type-drain-when-no-flavors gates only the no-flavors auto-drain, not an explicit
	// delete. We keep no finalizer of our own: Kueue's finalizer holds the queue alive while
	// workloads remain, which is exactly the window this write needs.
	if cq.DeletionTimestamp != nil {
		if ptr.Deref(cq.Spec.StopPolicy, kueue.None) == kueue.HoldAndDrain {
			return ctrl.Result{}, nil
		}
		cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)
		if err = r.Client.Update(ctx, cq); err != nil {
			logger.Error(err, "hold and drain deleting cluster queue")
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}
		logger.V(2).Info("holding and draining deleting cluster queue")
		return ctrl.Result{}, nil
	}

	// The pool's ResourceFlavors, matched by the queue's schedule labels (feature key + os +
	// arch). Admin queue names are arbitrary, so the pool is resolved by labels, not by name.
	lbs := nodefeature.PoolFlavorSelector(cq.Labels)
	if len(lbs) == 0 {
		return ctrl.Result{}, nil
	}
	// Scope to operator-owned flavors: a foreign ResourceFlavor that happens to share the pool's
	// schedule labels must not pollute the queue's quota (mirrors the InstanceType webhook selector).
	lbs[systemmeta.ResourceTypeLabel] = _ResourceFlavorResType
	rfList := new(kueue.ResourceFlavorList)
	err = r.Client.List(ctx, rfList,
		ctrlcli.MatchingLabels(lbs),
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list resource flavors by cluster queue")
		return ctrl.Result{}, err
	}

	// Drop ResourceFlavors Kueue is still finalizing (DeletionTimestamp set): the NodeFlavor
	// reconciler deleted them because their nodes left the pool, but Kueue holds each one's
	// resource-in-use finalizer until no ClusterQueue references it, and removes it only on a
	// ClusterQueue update that drops the reference. Keeping a mid-deletion flavor in the resource
	// groups re-holds that finalizer and deadlocks its removal, so it is treated as gone: filling
	// without it (a partial pool) drops it from the groups — the very update Kueue waits for — and
	// an all-terminating pool falls through to the drain/empty path.
	//
	// A workload still admitted on a dropped (partial-pool) flavor is evicted by Kueue and
	// re-admitted on the pool's remaining live flavors — Kueue re-evaluates admission when a
	// flavor leaves a ClusterQueue's resource groups. The dropped flavor's node has already left
	// the pool, so that workload must move regardless; the graceful whole-pool drain (HoldAndDrain,
	// gated by instance-type-drain-when-no-flavors) only governs the all-terminating path below.
	live := rfList.Items[:0]
	for i := range rfList.Items {
		if rfList.Items[i].DeletionTimestamp == nil {
			live = append(live, rfList.Items[i])
		}
	}
	rfList.Items = live

	if len(rfList.Items) > 0 {
		return r.fillClusterQueue(ctx, cq, rfList)
	}
	return r.drainOrEmptyClusterQueue(ctx, cq)
}

// fillClusterQueue converges the resource groups from the pool's flavors (smallest per-node
// count first) and reactivates a queue that had been drained to empty.
func (r *NodeQueueReconciler) fillClusterQueue(
	ctx context.Context, cq *kueue.ClusterQueue, rfList *kueue.ResourceFlavorList,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Smallest per-node count first, so Kueue's flavor fungibility fills small nodes first.
	slices.SortStableFunc(rfList.Items, func(a, b kueue.ResourceFlavor) int {
		return cmp.Compare(parseNodeFlavorCount(a.Name), parseNodeFlavorCount(b.Name))
	})

	_, firstNotes := systemmeta.DescribeResource(&rfList.Items[0])
	acceleratable := firstNotes["acceleratable"] == "true"
	eGroups := buildResourceGroups(rfList, acceleratable, firstNotes["manufacturer"])

	changed := false
	// Reference the node-devices AdmissionCheck on an accelerated queue, but only once it
	// reports Active: Kueue turns a ClusterQueue that lists a missing or inactive
	// AdmissionCheck inactive, so it would stop admitting. The Watches on the AdmissionCheck
	// re-runs this reconcile when it activates, and drops the reference when it goes away.
	//
	// The derived-from-node setting below is read as a cluster-wide switch, not as "was THIS
	// queue derived": with it off the administrator authors ClusterQueues through the
	// InstanceType API, and this reconciler still fills them but references the check on none
	// of them. So the gate runs on every accelerated queue in the cluster, or on no queue at all.
	var admissionChecks *kueue.AdmissionChecksStrategy
	if acceleratable &&
		settings.InstanceTypeDerivedFromNode.ShouldValueBool(ctx) &&
		r.nodeDevicesCheckActive(ctx) {
		admissionChecks = &kueue.AdmissionChecksStrategy{
			AdmissionChecks: []kueue.AdmissionCheckStrategyRule{
				{Name: kueue.AdmissionCheckReference(_NodeDevicesAdmissionCheckName)},
			},
		}
	}
	if !kubemeta.DeepEqual(cq.Spec.AdmissionChecksStrategy, admissionChecks) {
		cq.Spec.AdmissionChecksStrategy = admissionChecks
		changed = true
	}
	// Reactivate only a queue WE drained to empty (HoldAndDrain + empty quota). An admin Hold is
	// owned by the InstanceTypeReconciler (a type marked Inactive) and stays sticky across a pool
	// losing and regaining its flavors, so it must not be flipped back to None here — doing so would
	// briefly admit workloads onto an Inactive type until syncInactive re-holds it.
	if ptr.Deref(cq.Spec.StopPolicy, kueue.None) == kueue.HoldAndDrain && len(cq.Spec.ResourceGroups) == 0 {
		cq.Spec.StopPolicy = ptr.To(kueue.None)
		changed = true
	}
	if !kubemeta.DeepEqual(cq.Spec.ResourceGroups, eGroups) {
		cq.Spec.ResourceGroups = eGroups
		changed = true
	}
	if changed {
		if err := r.Client.Update(ctx, cq); err != nil {
			logger.Error(err, "fill cluster queue resource groups")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("filled cluster queue resource groups")
	}
	return ctrl.Result{}, nil
}

// drainOrEmptyClusterQueue handles a queue whose pool has lost all its flavors: it empties the
// quota, but only once every reservation has cleared so Kueue never counts negative. While
// reservations remain it optionally drives HoldAndDrain (gated by the setting) and requeues;
// an already-empty queue is a no-op.
func (r *NodeQueueReconciler) drainOrEmptyClusterQueue(
	ctx context.Context, cq *kueue.ClusterQueue,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	if len(cq.Spec.ResourceGroups) == 0 {
		return ctrl.Result{}, nil
	}

	if hasReserved(cq) {
		// Reservations still outstanding: draining (when enabled) evicts them; emptying the
		// quota now would drive the counters negative, so wait and re-check.
		if settings.InstanceTypeDrainWhenNoFlavors.ShouldValueBool(ctx) &&
			ptr.Deref(cq.Spec.StopPolicy, kueue.None) != kueue.HoldAndDrain {
			cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)
			if err := r.Client.Update(ctx, cq); err != nil {
				logger.Error(err, "hold and drain cluster queue with no flavors")
				return ctrl.Result{}, err
			}
			logger.V(2).Info("held and draining cluster queue with no flavors; requeue in 60s")
		}
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	// Every reservation is zero: empty the quota (an empty resource-group list is valid).
	cq.Spec.ResourceGroups = nil
	if err := r.Client.Update(ctx, cq); err != nil {
		logger.Error(err, "empty cluster queue resource groups")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("emptied cluster queue resource groups")
	return ctrl.Result{}, nil
}

// hasReserved reports whether the ClusterQueue still holds reserved quota or
// admitted/reserving workloads. The queue must not be emptied or deleted until Kueue has
// finished draining; the workload counters guard against the flavor reservation snapshot
// momentarily reading zero while eviction is still in flight. Pending workloads are
// intentionally not counted — they hold no reservation, and gating on them would block
// deletion forever.
func hasReserved(cq *kueue.ClusterQueue) bool {
	if cq.Status.ReservingWorkloads != 0 || cq.Status.AdmittedWorkloads != 0 {
		return true
	}
	return slicex.Any(cq.Status.FlavorsReservation, func(i int) bool {
		return slicex.Any(cq.Status.FlavorsReservation[i].Resources, func(j int) bool {
			return !cq.Status.FlavorsReservation[i].Resources[j].Total.IsZero() ||
				!cq.Status.FlavorsReservation[i].Resources[j].Borrowed.IsZero()
		})
	})
}

// buildResourceGroups builds the ClusterQueue resource groups from the feeding flavors. An
// accelerated queue covers only the manufacturer's credits resource (nominal = capacity×M per
// flavor); a CPU-only queue covers only cpu (nominal = capacity cores).
func buildResourceGroups(rfList *kueue.ResourceFlavorList, acceleratable bool, manufacturer string) []kueue.ResourceGroup {
	covered := []core.ResourceName{core.ResourceCPU}
	if acceleratable {
		covered = []core.ResourceName{nodefeature.GetAcceleratableCreditsResourceName(manufacturer)}
	}

	var groups []kueue.ResourceGroup
	for i := range rfList.Items {
		rf := &rfList.Items[i]
		capacity := parseResourceFlavorCapacity(rf)
		if capacity <= 0 {
			continue
		}

		nominal := *resource.NewQuantity(capacity, resource.DecimalSI)
		if acceleratable {
			nominal = nodefeature.AcceleratorsToCredits(nominal)
		}

		// A resource group holds at most 16 flavors.
		if len(groups) == 0 || len(groups[len(groups)-1].Flavors) >= 16 {
			groups = append(groups, kueue.ResourceGroup{CoveredResources: covered})
		}
		g := &groups[len(groups)-1]
		g.Flavors = append(g.Flavors, kueue.FlavorQuotas{
			Name: kueue.ResourceFlavorReference(rf.Name),
			Resources: []kueue.ResourceQuota{
				{
					Name:         covered[0],
					NominalQuota: nominal,
					// No borrowing/lending limit: the queue keeps an empty cohort, and Kueue
					// rejects a ClusterQueue that carries a limit while it belongs to no
					// cohort. The empty cohort is the isolation.
				},
			},
		})
	}
	return groups
}

// parseNodeFlavorCount extracts the per-node count encoded in a node ResourceFlavor name
// "gpustack--${key}-${os}-${arch}-${count}{c|d}" (CPU cores for a CPU flavor, device count for
// a device flavor); returns 0 when the name lacks the suffix.
func parseNodeFlavorCount(name string) int64 {
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return 0
	}
	seg := name[i+1:]
	if len(seg) < 2 {
		return 0
	}
	switch seg[len(seg)-1] {
	case 'c', 'd':
		if v, err := strconvx.Atoi[int64](seg[:len(seg)-1]); err == nil {
			return v
		}
	}
	return 0
}

// parseResourceFlavorCapacity reads a flavor's pooled capacity (nodes × count) from the
// ".capacity" sibling of its OWN feature-key label: an accelerated flavor sizes on its
// "acceleratable." key, a CPU flavor on its "general." key. An accelerated flavor also carries the
// "general.<gKey>" selector label (without a ".capacity" sibling), so the search is scoped to the
// own-key prefix by the feature.gpustack.ai/acceleratable boolean and skips a key whose ".capacity"
// is absent or non-positive — otherwise the map's random iteration order could read the wrong
// (missing) key and silently drop the flavor's quota. Returns 0 when no own capacity is found.
func parseResourceFlavorCapacity(rf *kueue.ResourceFlavor) int64 {
	prefix := nodefeature.GeneralFeatureLabelPrefix
	if rf.Labels[nodefeature.NodeAcceleratableLabelKey] == "true" {
		prefix = nodefeature.AcceleratableFeatureLabelPrefix
	}
	for k, v := range rf.Labels {
		if v != "true" || !strings.HasPrefix(k, prefix) {
			continue
		}
		if capacity, err := strconvx.Atoi[int64](rf.Labels[k+_ResourceFlavorCapacityLabelSuffix]); err == nil && capacity > 0 {
			return capacity
		}
	}
	return 0
}

// nodeDevicesCheckActive reports whether the node-devices AdmissionCheck exists and
// is Active. The queue references it only when true, since listing an inactive
// check would turn the ClusterQueue inactive and stop it admitting.
func (r *NodeQueueReconciler) nodeDevicesCheckActive(ctx context.Context) bool {
	ac := new(kueue.AdmissionCheck)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: _NodeDevicesAdmissionCheckName}, ac,
		ctrlclix.WithoutQuorum)
	if err != nil {
		return false
	}
	return kubemeta.IsConditionTrue(ac.Status.Conditions, kueue.AdmissionCheckActive)
}

func (r *NodeQueueReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodequeue").
		For(
			// Reconcile each operator-owned ClusterQueue by its own name. The reconcile is
			// idempotent, so the operator's own writes settle without looping.
			&kueue.ClusterQueue{},
			ctrlbuilder.WithPredicates(
				// Interested in relevant ClusterQueue objects (WithPredicates ANDs, so this
				// gates every event below).
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ClusterQueueResType)
				}),
				// Trigger reconciliation when a ClusterQueue is:
				// - created (incl. the start-up resync).
				// - updated if its generation (spec) changed, or its DeletionTimestamp was set
				//   (so an explicit delete is drained).
				// Never react to the final deletion (a gone queue has nothing to reconcile) or to
				// status churn (reservation counters, conditions), which Kueue writes constantly.
				ctrlpredicate.Funcs{
					DeleteFunc: func(ctrlevent.DeleteEvent) bool { return false },
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldCq, newCq := e.ObjectOld.(*kueue.ClusterQueue), e.ObjectNew.(*kueue.ClusterQueue)
						if !oldCq.DeletionTimestamp.Equal(newCq.DeletionTimestamp) {
							return true
						}
						return oldCq.Generation != newCq.Generation
					},
				},
			),
		).
		Watches(
			// Re-converge the quota of every ClusterQueue a flavor feeds when the pool gains or
			// loses flavors.
			&kueue.ResourceFlavor{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueNodeQueueWhenResourceFlavorChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a relevant ResourceFlavor is created, updated, or
				// deleted — any of them changes a pool's flavor set.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ResourceFlavorResType)
				}),
			),
		).
		Watches(
			// Re-enqueue the operator-owned queues when the node-devices AdmissionCheck changes,
			// so an accelerated derived queue acquires the reference once it turns Active (or drops
			// it should the check go inactive/away).
			&kueue.AdmissionCheck{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueNodeQueuesWhenAdmissionCheckChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when the node-devices AdmissionCheck is created, updated,
				// or deleted (its Active state gates the reference).
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					ac := obj.(*kueue.AdmissionCheck)
					return ac.Spec.ControllerName == _NodeDevicesControllerName
				}),
			),
		).
		Complete(r)
}

// enqueueNodeQueueWhenResourceFlavorChanged enqueues every operator-owned ClusterQueue whose
// flavor selector this changed flavor feeds. The flavor is the finest grain (it always carries
// the CPU key), whereas a queue's pool may be collapsed and carry fewer discriminators, so a
// MatchingLabels query keyed on the flavor's labels would miss a collapsed queue. Instead it
// lists the operator queues and keeps those whose own poolFlavorSelector is a subset of the
// flavor's discriminators.
func (r *NodeQueueReconciler) enqueueNodeQueueWhenResourceFlavorChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("resource flavor", ctrlcli.ObjectKeyFromObject(obj))

	rfSel := nodefeature.PoolFlavorSelector(obj.GetLabels())
	if len(rfSel) == 0 {
		return nil
	}

	// Narrow to the operator-owned queues server-side via the resource-type label instead of
	// listing every ClusterQueue. The subset match below still runs in-memory: a collapsed queue
	// carries fewer discriminators than the flavor, so a labels-keyed query on the flavor's own
	// labels would miss it.
	cqList := new(kueue.ClusterQueueList)
	err := r.Client.List(ctx, cqList,
		systemmeta.GetResourcesLabelSetOfType[ctrlcli.MatchingLabels](_ClusterQueueResType),
		ctrlclix.WithoutQuorum,
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list cluster queues for resource flavor")
		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range cqList.Items {
		cq := &cqList.Items[i]
		cqSel := nodefeature.PoolFlavorSelector(cq.Labels)
		if len(cqSel) == 0 || !mapx.Contain(rfSel, cqSel) {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: cq.Name},
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueued node queues from resource flavor", "requests", reqs)
	return reqs
}

// enqueueNodeQueuesWhenAdmissionCheckChanged enqueues every operator-owned ClusterQueue when
// the node-devices AdmissionCheck changes, so each accelerated pool (re)acquires the reference
// once the check turns Active (or drops it should the check go inactive/away).
func (r *NodeQueueReconciler) enqueueNodeQueuesWhenAdmissionCheckChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("admission check", ctrlcli.ObjectKeyFromObject(obj))

	ac := obj.(*kueue.AdmissionCheck)
	if ac.Spec.ControllerName != _NodeDevicesControllerName {
		return nil
	}

	cqList := new(kueue.ClusterQueueList)
	if err := r.Client.List(ctx, cqList, ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy); err != nil {
		logger.Error(err, "list cluster queues for admission check")
		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range cqList.Items {
		cq := &cqList.Items[i]
		if !systemmeta.MatchResource(cq, _ClusterQueueResType) || cq.DeletionTimestamp != nil {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: cq.Name},
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueued node queues from admission check", "requests", reqs)
	return reqs
}
