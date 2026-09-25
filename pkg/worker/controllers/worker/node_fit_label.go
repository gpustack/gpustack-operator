package worker

import (
	"context"
	"maps"
	"strconv"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
)

// NodeFitLabelReconciler keeps the fit labels on a managed Node, derived from the same-named Devices
// ledger: per accelerator model, the largest free units on any one card that can serve a logical
// slice, and the number of cards that can still grant an ownership share.
//
// Kueue's topology-aware scheduling places a Workload from a node's summed capacity, so on a node
// whose free room is spread over cards none of which fits, it places the Workload there, the
// node-devices check answers Retry, and it places it there again. The Workload webhook pins a slice
// or shared request to these labels through a required node affinity in the Workload's PodSet
// template, which TAS honors, so a fragmented node is skipped instead.
//
// Both values use the node-devices check's own per-card predicates, so for a single Pod asking for
// one card the label admits a node exactly when the check would. They filter only; nothing is
// charged against them. A value is written only when it changes, after a dedup window on ledger
// events, so the cost is at most one Node metadata patch per allocation or release burst.
type NodeFitLabelReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeFitLabelReconciler)(nil)

func (r *NodeFitLabelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch. Read-only (the label patch below targets a fresh Node object), so the deep copy is
	// skipped.
	nd := new(core.Node)
	err := r.Client.Get(ctx, req.NamespacedName, nd, ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		// A deleted Node has nothing left to reconcile.
		if err = ctrlcli.IgnoreNotFound(err); err != nil {
			logger.Error(err, "fetch node")
		}
		return ctrl.Result{}, err
	}

	// Skip if deleted.
	if nd.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted node")
		return ctrl.Result{}, nil
	}

	// Fetch the same-named Devices ledger. A missing one yields no fit labels, which removes any
	// stale ones, so a node without a ledger is skipped by every pinned request, as the
	// node-devices check would hold it.
	devs := new(workercore.Devices)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: nd.Name}, devs, ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch devices")
			return ctrl.Result{}, err
		}
		devs = nil
	}

	labelPatch := buildFitLabelPatch(desiredFitLabels(nd, devs), nd.Labels)
	if labelPatch == nil {
		return ctrl.Result{}, nil
	}
	data := json.ShouldMarshal(map[string]any{
		"metadata": map[string]any{"labels": labelPatch},
	})
	// Patch a fresh Node object rather than nd: the merge-patch response is decoded back into the
	// target, and nd was read without a deep copy.
	patchNode := &core.Node{ObjectMeta: meta.ObjectMeta{Name: nd.Name}}
	err = r.Client.Patch(ctx, patchNode, ctrlcli.RawPatch(types.MergePatchType, data))
	if err != nil {
		logger.Error(err, "patch node fit labels")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("patched node fit labels")
	return ctrl.Result{}, nil
}

// desiredFitLabels returns the fit labels a node should carry: none for an unmanaged node or one
// without a ledger, otherwise those its ledger yields.
func desiredFitLabels(nd *core.Node, devs *workercore.Devices) map[string]string {
	if devs == nil || !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
		return nil
	}
	return fitLabelsOf(devs)
}

// fitLabelsOf derives the fit labels from a Devices ledger, one pair per "<manufacturer>-<id>"
// group, joining each allocation on the status side with its capability on the spec side by
// accelerator ID, as the node-devices check does. A group with no card of a population emits no
// key for it; one whose cards are all full emits "0". A card holding its full count of logical
// slices is full for the sliced key however much memory it has left, since it takes no further slice.
func fitLabelsOf(devs *workercore.Devices) map[string]string {
	capByID := make(map[string]workercore.AcceleratorStatus)
	for gi := range devs.Spec.Groups {
		accs := devs.Spec.Groups[gi].Accelerators
		for ai := range accs {
			capByID[accs[ai].ID] = accs[ai].Status
		}
	}

	out := make(map[string]string)
	for gi := range devs.Status.Groups {
		g := &devs.Status.Groups[gi]
		var (
			sliceable, shareable bool
			maxFreeUnits         int32
			freeShareCards       int
		)
		for ai := range g.Accelerators {
			acc := &g.Accelerators[ai]
			card := cardLedger{
				capability: capByID[acc.ID], mode: acc.Mode, remaining: acc.Remaining,
				allocatedSlices: acc.AllocatedSlices,
			}
			if card.servesFamily(nodefeature.ResourceFamilySliced) {
				sliceable = true
				maxFreeUnits = max(maxFreeUnits, card.freeSliceUnits())
			}
			if card.servesFamily(nodefeature.ResourceFamilyShared) {
				shareable = true
				if card.freeShares() > 0 {
					freeShareCards++
				}
			}
		}
		// A key built from a group ID that is not valid label grammar is skipped: it would make the
		// whole Node patch invalid, and no pin can name it either.
		aKey := acceleratorKeyOf(g.Manufacturer, g.ID)
		if key := nodefeature.FitSlicedMaxFreeUnitsLabelKey(aKey); sliceable && nodefeature.IsFitLabelKey(key) {
			out[key] = strconv.FormatInt(int64(maxFreeUnits), 10)
		}
		if key := nodefeature.FitSharedFreeCardsLabelKey(aKey); shareable && nodefeature.IsFitLabelKey(key) {
			out[key] = strconv.Itoa(freeShareCards)
		}
	}
	return out
}

// buildFitLabelPatch returns the metadata.labels merge patch converging a node's fit labels onto
// desired: a changed or missing value is set, and a fit label absent from desired is removed. It
// names no label that is not a fit label, and returns nil when nothing differs.
func buildFitLabelPatch(desired, current map[string]string) map[string]any {
	patch := make(map[string]any)
	for k, v := range desired {
		if cur, ok := current[k]; !ok || cur != v {
			patch[k] = v
		}
	}
	for k := range current {
		if !nodefeature.IsFitLabelKey(k) {
			continue
		}
		if _, ok := desired[k]; !ok {
			patch[k] = nil
		}
	}
	if len(patch) == 0 {
		return nil
	}
	return patch
}

// nodeFitLabelNodeUpdated reports whether a Node update can move its desired fit labels or undo
// them: the managed mark changed, or someone else rewrote a fit label.
func nodeFitLabelNodeUpdated(oldNd, newNd *core.Node) bool {
	if newNd.DeletionTimestamp != nil {
		return false
	}
	return !mapx.EqualWithKey(oldNd.Labels, newNd.Labels, systemname.ManagedLabelKey) ||
		!maps.Equal(nodefeature.FilterFitLabels(oldNd.Labels), nodefeature.FilterFitLabels(newNd.Labels))
}

// fitSignatureChanged reports whether a Devices change moved any fit label value. Most allocations
// move a card's remaining units without moving the largest one or crossing a share boundary, and
// those do not reconcile the node.
func fitSignatureChanged(oldDevs, newDevs *workercore.Devices) bool {
	return !maps.Equal(fitLabelsOf(oldDevs), fitLabelsOf(newDevs))
}

func (r *NodeFitLabelReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodefitlabel").
		For(
			&core.Node{},
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - updated if its managed mark or one of its fit labels changed.
				ctrlpredicate.Funcs{
					DeleteFunc: func(ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						return nodeFitLabelNodeUpdated(e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node))
					},
				},
			),
		).
		Watches(
			// A ledger change moves the fit labels without touching the Node, so enqueue the
			// name-identical node. The 3s dedup window coalesces an allocation burst into one patch.
			&workercore.Devices{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueNodeWhenDevicesChanged,
			),
			// A create or a delete always reconciles, whatever the ledger's own mark: the node's
			// mark is what decides its labels, and a deleted ledger must take its labels with it.
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					return fitDevicesUpdated(e.ObjectOld.(*workercore.Devices), e.ObjectNew.(*workercore.Devices))
				},
			}),
		).
		Complete(r)
}

// fitDevicesUpdated reports whether a Devices update must reconcile its node's fit labels: its fit
// values moved, or the managed mark itself flipped. The node's own mark decides whether it carries
// labels, so a value change counts whatever the ledger's mark. The Device Manager creates a ledger
// before the mark is synced onto it, and syncing the mark moves no fit value, so without the flip a
// node could wait for its first labels until an unrelated allocation.
func fitDevicesUpdated(oldDevs, newDevs *workercore.Devices) bool {
	return isManagedDevices(oldDevs) != isManagedDevices(newDevs) || fitSignatureChanged(oldDevs, newDevs)
}

// enqueueNodeWhenDevicesChanged maps a changed Devices ledger to its name-identical Node.
func (r *NodeFitLabelReconciler) enqueueNodeWhenDevicesChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: obj.GetName()}}
	ctrllog.FromContext(ctx).V(2).Info("enqueued node from devices", "request", req)
	return []ctrlreconcile.Request{req}
}
