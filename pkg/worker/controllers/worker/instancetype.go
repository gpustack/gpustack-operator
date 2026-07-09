package worker

import (
	"context"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/worker/apistatus"
	"gpustack.ai/gpustack/pkg/worker/kuberequest"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// InstanceTypeReconciler owns the lifecycle of worker.gpustack.ai InstanceType CRs and the
// existence + metadata of their backing Kueue ClusterQueue — creation, teardown, and the schedule
// labels. The NodeQueueReconciler owns the queue's quota (resource groups + StopPolicy) and its
// node-devices AdmissionCheck reference; the InstanceType webhook stamps the entrance label and
// the Pod webhook reads the per-card VRAM off the InstanceType spec:
//   - From an InstanceType it ensures the name-identical CQ exists, carrying the schedule labels
//     (derived from the spec identity); the queue is created with the fixed no-borrow isolation
//     policy (empty cohort, no-borrow preemption) stamped into its spec.
//   - It watches the CQ to refresh its status (phase + CPU view) from the queue's quota and
//     conditions, and to recreate the queue on an admin's accidental delete while its
//     InstanceType still lives. The NodeQueueReconciler owns the quota/StopPolicy; this
//     reconciler only reads them for status and never writes them.
//   - It does not author InstanceTypes: the NodeFlavorReconciler creates a derived InstanceType
//     (create-only) after it syncs a pool's flavors. This reconciler manages only types that
//     already exist, and never deletes one for lack of flavors — the NodeQueueReconciler
//     drains/empties the backing queue instead.
//   - It materializes the three-view (or CPU) status from the Devices ledger + the
//     ClusterQueue; the hardware-descriptor spec is a one-time snapshot the defaulting webhook
//     fills at admission, no longer refreshed here.
//   - On delete a finalizer holds the InstanceType until it has deleted the backing CQ and the
//     CQ has actually disappeared; the NodeQueueReconciler drains the deleting queue.
type InstanceTypeReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

var _ ctrlreconcile.Reconciler = (*InstanceTypeReconciler)(nil)

func (r *InstanceTypeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	it := new(workercore.InstanceType)
	err := r.Client.Get(ctx, req.NamespacedName, it, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch instance type")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Deleting: run the finalizer teardown.
	if it.DeletionTimestamp != nil {
		return r.teardownInstanceType(ctx, it)
	}

	// Lock.
	if !systemmeta.Lock(it) {
		err = r.Client.Update(ctx, it)
		if err != nil {
			logger.Error(err, "lock instance type")
			return ctrl.Result{}, err
		}
	}

	// Ensure the backing ClusterQueue exists with its labels (the
	// NodeQueueReconciler fills its quota), then refresh the status from the Devices ledger +
	// the queue. The hardware-descriptor spec is a one-time snapshot the defaulting webhook
	// fills at admission and is no longer refreshed here; the write is DeepEqual-guarded.
	cq, err := r.ensureClusterQueue(ctx, it)
	if err != nil {
		logger.Error(err, "ensure cluster queue")
		return ctrl.Result{}, err
	}
	if cq.DeletionTimestamp != nil {
		// The backing queue is mid-deletion under a live InstanceType (an admin's accidental
		// delete). Requeue until Kueue removes it; the next reconcile then recreates it. Don't
		// refresh status from a terminating queue.
		logger.V(2).Info("cluster queue terminating; requeue to recreate once gone")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	overcommit := settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(ctx)
	desiredStatus := r.computeStatus(ctx, it, cq, overcommit)
	if !kubemeta.DeepEqual(desiredStatus, it.Status) {
		it.Status = desiredStatus
		if err = r.Client.Status().Update(ctx, it); err != nil {
			logger.Error(err, "update instance type status")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("refreshed instance type status")
	}
	return ctrl.Result{}, nil
}

// ensureClusterQueue guarantees the backing ClusterQueue matches the InstanceType. It creates
// the queue when missing — first creation or a recreation after an accidental delete — stamping
// the fixed no-borrow isolation policy into the spec at that point, and afterwards only aligns
// the queue's schedule labels (from the spec identity). The Pod webhook reads the per-card VRAM
// off the InstanceType spec, so the queue carries no memory note. It never fills the resource groups,
// references the node-devices AdmissionCheck, or converges the StopPolicy: the NodeQueueReconciler
// owns the quota and admission gating, and the teardown owns the StopPolicy while the type is
// being deleted. A queue a user is deleting (DeletionTimestamp set) while its InstanceType lives
// is left to finish; the reconcile recreates it once gone, so an accidental delete self-heals.
func (r *InstanceTypeReconciler) ensureClusterQueue(
	ctx context.Context, it *workercore.InstanceType,
) (*kueue.ClusterQueue, error) {
	logger := ctrllog.FromContext(ctx)

	cq := new(kueue.ClusterQueue)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: it.Name}, cq, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, err
		}
		return r.createClusterQueue(ctx, it)
	}

	// The user is deleting a queue whose InstanceType still lives: return it as-is so the caller
	// requeues (we cannot recreate under the same name until it is gone), rather than fight a
	// terminating object. The caller recreates it on the reconcile that finds it gone.
	if cq.DeletionTimestamp != nil {
		return cq, nil
	}

	// Align the existing queue's metadata only — the isolation is written once at creation and
	// the NodeQueueReconciler owns the quota.
	eLabels := instanceTypeScheduleLabels(ctx, it)
	changed := !systemmeta.NoteResource(cq, _ClusterQueueResType, nil)
	// Drop a stale feature-key label left by a group/acceleratable change (only os/arch keep a
	// fixed key, so a plain merge would overwrite them but leave the old feature key). The flavor
	// and device selectors AND every feature-key label on the queue, so a leftover key would match
	// no pool and strand the queue — group/acceleratable are not frozen on update.
	for k := range cq.Labels {
		if _, want := eLabels[k]; want {
			continue
		}
		if strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
			strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix) {
			delete(cq.Labels, k)
			changed = true
		}
	}
	if !mapx.Contain(cq.Labels, eLabels) {
		if cq.Labels == nil {
			cq.Labels = make(map[string]string)
		}
		for k, v := range eLabels {
			cq.Labels[k] = v
		}
		changed = true
	}
	if changed {
		if err = r.Client.Update(ctx, cq); err != nil {
			logger.Error(err, "align cluster queue")
			return nil, err
		}
		logger.V(2).Info("aligned cluster queue")
	}
	return cq, nil
}

// createClusterQueue builds the backing ClusterQueue from the InstanceType: the spec-derived
// schedule labels, an active StopPolicy, and the
// fixed no-borrow isolation policy written straight into the spec — empty cohort (no cross-queue
// borrowing to broker), never reclaim/borrow within a nonexistent cohort, only in-queue
// lower-priority preemption, all-namespace selector. The NodeQueueReconciler fills the resource
// groups and the node-devices AdmissionCheck reference afterwards.
func (r *InstanceTypeReconciler) createClusterQueue(
	ctx context.Context, it *workercore.InstanceType,
) (*kueue.ClusterQueue, error) {
	logger := ctrllog.FromContext(ctx)

	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			Name:   it.Name,
			Labels: instanceTypeScheduleLabels(ctx, it),
		},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			StopPolicy:        ptr.To(kueue.None),
			FlavorFungibility: &kueue.FlavorFungibility{
				WhenCanBorrow:  kueue.TryNextFlavor,
				WhenCanPreempt: kueue.MayStopSearch,
			},
			Preemption: &kueue.ClusterQueuePreemption{
				ReclaimWithinCohort: kueue.PreemptionPolicyNever,
				BorrowWithinCohort:  &kueue.BorrowWithinCohort{Policy: kueue.BorrowWithinCohortPolicyNever},
				WithinClusterQueue:  kueue.PreemptionPolicyLowerPriority,
			},
		},
	}
	systemmeta.NoteResource(cq, _ClusterQueueResType, nil)
	if err := r.Client.Create(ctx, cq); err != nil {
		if kerrors.IsAlreadyExists(err) {
			return cq, r.Client.Get(ctx, ctrlcli.ObjectKey{Name: it.Name}, cq, ctrlclix.WithoutQuorum)
		}
		return nil, err
	}
	logger.V(2).Info("created cluster queue")
	return cq, nil
}

// teardownInstanceType completes a delete: it deletes the backing ClusterQueue and holds the
// InstanceType's finalizer until the queue has actually disappeared. It does not drain the queue
// itself — the NodeQueueReconciler observes the deletion and drives HoldAndDrain so Kueue evicts
// the workloads and removes the queue; this reconciler only requests the deletion and waits.
func (r *InstanceTypeReconciler) teardownInstanceType(
	ctx context.Context, it *workercore.InstanceType,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	if !systemmeta.IsLocked(it) {
		return ctrl.Result{}, nil
	}

	cq := new(kueue.ClusterQueue)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: it.Name}, cq, ctrlclix.WithoutQuorum)
	switch {
	case err == nil:
		// Reflect the teardown in the InstanceType status so consumers — and the Instance
		// reconciler's stop check — see it go Inactive before the queue is gone.
		if it.Status.Phase != InstanceTypePhaseInactive {
			it.Status = workercore.InstanceTypeStatus{
				Phase:        InstanceTypePhaseInactive,
				PhaseMessage: "instance type is terminating",
				Entrance:     nodefeature.FormatLocalQueueName(cq.Name),
			}
			if err = r.Client.Status().Update(ctx, it); err != nil {
				logger.Error(err, "mark instance type inactive while terminating")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}
		}
		// Request the queue's deletion once; the NodeQueueReconciler drains it (HoldAndDrain) so
		// Kueue evicts its workloads and removes it. Wait for it to disappear before releasing
		// the finalizer.
		if cq.DeletionTimestamp == nil {
			if err = r.Client.Delete(ctx, cq); err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete cluster queue")
				return ctrl.Result{}, err
			}
			logger.V(2).Info("requested cluster queue deletion; the node queue drains it")
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case !kerrors.IsNotFound(err):
		logger.Error(err, "fetch cluster queue")
		return ctrl.Result{}, err
	}

	// The queue is gone: release the finalizer.
	systemmeta.Unlock(it)
	if err = r.Client.Update(ctx, it); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	logger.V(2).Info("released drained instance type")
	return ctrl.Result{}, nil
}

// computeStatus builds the InstanceType status: the accelerated three-view is a
// per-card bin-packing projection of the pool's Devices ledger, the CPU view comes
// from the ClusterQueue; a draining queue reports zero (its status views stay at
// their zero value). The phase mirrors the ClusterQueue conditions.
func (r *InstanceTypeReconciler) computeStatus(
	ctx context.Context, it *workercore.InstanceType, cq *kueue.ClusterQueue, withOvercommit bool,
) workercore.InstanceTypeStatus {
	acceleratable := it.Spec.Acceleratable
	draining := cq.Spec.StopPolicy != nil && *cq.Spec.StopPolicy == kueue.HoldAndDrain

	var st workercore.InstanceTypeStatus
	if !draining {
		if acceleratable {
			devices := r.listFlavorPoolDevices(ctx, cq)
			st.Accelerator, st.AcceleratorShared, st.AcceleratorSliced = getAcceleratorResources(devices)
		} else {
			st.CPU = getCPUResource(cq, withOvercommit)
		}
	}
	st.Entrance = nodefeature.FormatLocalQueueName(cq.Name)
	st.Phase, st.PhaseMessage = apistatus.GetSummaryOfClusterQueue(&cq.Status)
	return st
}

// instanceTypeScheduleLabels builds the schedule discriminator labels stamped on the backing
// queue from the InstanceType spec identity and the CPU-manufacturer awareness setting (read
// per-reconcile): the acceleratable boolean, the accelerator key (when accelerated), the CPU
// key (only when aware), plus kubernetes.io/os|arch. They reverse-look-up the queue's
// ResourceFlavors and Devices, and are derived from the spec (admin-authored or derived), not
// the metadata labels, so an admin who sets only the spec still gets a correct queue.
func instanceTypeScheduleLabels(ctx context.Context, it *workercore.InstanceType) map[string]string {
	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx)
	return nodefeature.PoolScheduleLabels(
		it.Spec.Acceleratable, cpuAware,
		it.Spec.GeneralGroup, it.Spec.AcceleratorGroup,
		it.Spec.OS, it.Spec.Arch)
}

// listFlavorPoolDevices reads the Devices ledgers of every managed node backing the
// queue's flavor pool, located by the queue's own schedule labels plus
// gpustack.ai/managed=true. The read is cached (the reconciler watches Devices); a
// missing or empty selector yields no devices rather than matching every object.
func (r *InstanceTypeReconciler) listFlavorPoolDevices(ctx context.Context, cq *kueue.ClusterQueue) []workercore.Devices {
	sel := poolDevicesSelector(cq.Labels)
	if len(sel) == 0 {
		return nil
	}
	list := new(workercore.DevicesList)
	if err := r.Client.List(ctx, list, ctrlcli.MatchingLabels(sel)); err != nil {
		return nil
	}
	return list.Items
}

// poolDevicesSelector extracts the reverse-lookup selector from a queue's labels: its
// feature key and kubernetes.io/os|arch, plus gpustack.ai/managed=true. Returns nil
// when the queue carries no feature key, so the caller never matches every object.
func poolDevicesSelector(cqLabels map[string]string) map[string]string {
	sel := make(map[string]string, 4)
	hasFeatureKey := false
	for k, v := range cqLabels {
		switch {
		case k == core.LabelOSStable, k == core.LabelArchStable:
			sel[k] = v
		case v == "true" &&
			(strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
				strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)):
			sel[k] = v
			hasFeatureKey = true
		}
	}
	if !hasFeatureKey {
		return nil
	}
	sel[systemname.ManagedLabelKey] = "true"
	return sel
}

// getAcceleratorResources aggregates the per-card Devices ledger of one flavor pool
// into the three InstanceType display views. Each Devices object is one node, and the
// status ledger seeds every card at ResourceMaxUnits (D), subtracting each allocation;
// a card's Mode marks how it is currently used. Per node:
//   - exclusive: cards that are entirely free (Remaining >= D), one each;
//   - shared:    over free and Shared cards, Remaining/(D/SharedResourceMaxSize) shares;
//   - sliced:    over free and Sliced cards, Remaining/(D/100) VRAM-percent units.
//
// Capacity is the whole pool seen as that mode (cards, cards×10, cards×100), Remaining
// sums each node's availability. OnceMaxRequest is the largest single allocation: the
// largest single node's availability for exclusive/shared (one allocation can span a
// node's cards), but the freest single card's percent for sliced (a slice targets one
// card — VRAM is per-card — so a single sliced request is at most 100).
func getAcceleratorResources(devices []workercore.Devices) (exclusive, shared, sliced workercore.InstanceTypeResource) {
	const (
		d         = int64(nodefeature.ResourceMaxUnits)
		sharedMax = int64(nodefeature.SharedResourceMaxSize) // ownership shares per card
		slicedMax = int64(100)                               // VRAM-percent units per card
	)
	sharedUnit, slicedUnit := d/sharedMax, d/slicedMax

	var (
		capExcl, remExcl, ormExcl       int64
		capShared, remShared, ormShared int64
		capSliced, remSliced, ormSliced int64
	)
	for i := range devices {
		dev := &devices[i]
		var nodeCards, nodeExcl, nodeShared, nodeSliced int64
		for j := range dev.Status.Groups {
			g := &dev.Status.Groups[j]
			for k := range g.Accelerators {
				a := &g.Accelerators[k]
				nodeCards++
				rem := int64(a.Remaining)
				free := rem >= d
				if free {
					nodeExcl++
				}
				if free || a.Mode == workercore.DeviceAllocationModeShared {
					nodeShared += rem / sharedUnit
				}
				if free || a.Mode == workercore.DeviceAllocationModeSliced {
					cardSliced := rem / slicedUnit
					nodeSliced += cardSliced
					// A sliced request targets a single card (VRAM is the per-card,
					// non-oversubscribable anchor), so the largest single sliced request is the
					// freest card's percent (≤100), not a node's card-sum — unlike exclusive and
					// shared, whose one request can span a node's cards.
					ormSliced = max(ormSliced, cardSliced)
				}
			}
		}
		capExcl += nodeCards
		capShared += nodeCards * sharedMax
		capSliced += nodeCards * slicedMax
		remExcl += nodeExcl
		remShared += nodeShared
		remSliced += nodeSliced
		ormExcl = max(ormExcl, nodeExcl)
		ormShared = max(ormShared, nodeShared)
		// ormSliced is tracked per-card in the loop above (a slice is single-card), not per-node.
	}

	mk := func(orm, rem, total int64) workercore.InstanceTypeResource {
		return workercore.InstanceTypeResource{
			OnceMaxRequest: *resource.NewQuantity(orm, resource.DecimalSI),
			Remaining:      *resource.NewQuantity(rem, resource.DecimalSI),
			Capacity:       *resource.NewQuantity(total, resource.DecimalSI),
		}
	}
	return mk(ormExcl, remExcl, capExcl),
		mk(ormShared, remShared, capShared),
		mk(ormSliced, remSliced, capSliced)
}

// getCPUResource computes the CPU display resource of a non-accelerated InstanceType
// from its ClusterQueue: capacity is the summed nominal CPU quota, remaining subtracts
// the reserved total (scaled back from overcommit units), and the once-max request is
// the largest single node's core count (encoded in a flavor name) bounded by remaining.
func getCPUResource(cq *kueue.ClusterQueue, withOvercommit bool) workercore.InstanceTypeResource {
	var capCPU, ormCPU resource.Quantity
	for i := range cq.Spec.ResourceGroups {
		rg := &cq.Spec.ResourceGroups[i]
		for j := range rg.Flavors {
			flv := &rg.Flavors[j]
			for k := range flv.Resources {
				if flv.Resources[k].Name == core.ResourceCPU {
					capCPU.Add(flv.Resources[k].NominalQuota)
				}
			}
			if c := parseNodeFlavorCount(string(flv.Name)); c > ormCPU.Value() {
				ormCPU = *resource.NewQuantity(c, resource.DecimalSI)
			}
		}
	}

	remCPU := capCPU.DeepCopy()
	for i := range cq.Status.FlavorsReservation {
		flv := &cq.Status.FlavorsReservation[i]
		for j := range flv.Resources {
			res := &flv.Resources[j]
			if res.Name != core.ResourceCPU {
				continue
			}
			total := res.Total
			if withOvercommit {
				total = kuberequest.ScaleBackOvercommit(res.Name, total, false)
			}
			remCPU.Sub(total)
		}
	}
	if remCPU.Sign() < 0 {
		remCPU = *resource.NewQuantity(0, resource.DecimalSI)
	}
	if ormCPU.Cmp(remCPU) > 0 {
		ormCPU = remCPU.DeepCopy()
	}

	return workercore.InstanceTypeResource{
		OnceMaxRequest: ormCPU,
		Remaining:      remCPU,
		Capacity:       capCPU,
	}
}

func (r *InstanceTypeReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("instancetype").
		For(
			// Reconcile each InstanceType by its own name.
			&workercore.InstanceType{},
			// Trigger reconciliation when an InstanceType is:
			// - created, or updated on a generation (spec) change — (re)align its ClusterQueue.
			// - marked for deletion (its DeletionTimestamp is set) — run the teardown.
			// Never react to the final removal (nothing to do once gone) or to the operator's own
			// status/finalizer writes (status churn), to avoid self-triggering.
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				DeleteFunc: func(ctrlevent.DeleteEvent) bool { return false },
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					oldIt, newIt := e.ObjectOld.(*workercore.InstanceType), e.ObjectNew.(*workercore.InstanceType)
					if !oldIt.DeletionTimestamp.Equal(newIt.DeletionTimestamp) {
						return true
					}
					return oldIt.Generation != newIt.Generation
				},
			}),
		).
		Watches(
			// The InstanceType tracks its backing ClusterQueue: an update refreshes its status
			// (phase + CPU view) from the queue's quota and conditions (which the
			// NodeQueueReconciler and Kueue write), and a deletion is recreated if the
			// InstanceType still lives (guards an admin's accidental delete).
			&kueue.ClusterQueue{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstanceTypeWhenClusterQueueChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a relevant ClusterQueue is created, updated, or
				// deleted — an update keeps the status fresh, a deletion drives the recreate.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ClusterQueueResType)
				}),
			),
		).
		Watches(
			// A Devices ledger change moves the accelerated three-view; this is the
			// watch a ClusterQueue projection could not serve.
			&workercore.Devices{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstanceTypeWhenDevicesChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a managed node's Devices ledger is created, updated,
				// or deleted (it drives the accelerated three-view status).
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return obj.GetLabels()[systemname.ManagedLabelKey] == "true"
				}),
			),
		).
		Complete(r)
}

// enqueueInstanceTypeWhenClusterQueueChanged maps a changed backing ClusterQueue back to its
// name-identical InstanceType, so the reconcile refreshes status from the queue (or recreates it
// when the queue was deleted while the InstanceType still lives).
func (r *InstanceTypeReconciler) enqueueInstanceTypeWhenClusterQueueChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("cluster queue", ctrlcli.ObjectKeyFromObject(obj))

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKeyFromObject(obj),
		},
	}

	logger.V(2).Info("enqueued instance type from cluster queue", "requests", reqs)
	return reqs
}

// enqueueInstanceTypeWhenDevicesChanged enqueues every InstanceType whose pool the changed
// Devices contributes to. For each feature key the Devices carries it lists the InstanceTypes
// sharing that key plus kubernetes.io/os|arch — a node with accelerators serves both its CPU pool
// and its device pool, so a single Devices can enqueue several types — resolving them by label so
// an admin-named type is found without guessing its name.
func (r *InstanceTypeReconciler) enqueueInstanceTypeWhenDevicesChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("devices", ctrlcli.ObjectKeyFromObject(obj))

	labels := obj.GetLabels()
	os, arch := labels[core.LabelOSStable], labels[core.LabelArchStable]

	var reqs []ctrlreconcile.Request
	seen := make(map[string]bool)
	for k, v := range labels {
		if v != "true" ||
			(!strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) &&
				!strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)) {
			continue
		}
		match := ctrlcli.MatchingLabels{k: "true"}
		if os != "" {
			match[core.LabelOSStable] = os
		}
		if arch != "" {
			match[core.LabelArchStable] = arch
		}
		itList := new(workercore.InstanceTypeList)
		if err := r.Client.List(ctx, itList, match,
			ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy); err != nil {
			logger.Error(err, "list instance types by devices labels")
			continue
		}
		for i := range itList.Items {
			name := itList.Items[i].Name
			if seen[name] {
				continue
			}
			seen[name] = true
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
		}
	}
	if len(reqs) == 0 {
		logger.V(3).Info("devices has no schedule labels or no matching instance type, skip")
		return nil
	}

	logger.V(2).Info("enqueued instance types from devices", "requests", reqs)
	return reqs
}
