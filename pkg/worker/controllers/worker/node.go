package worker

import (
	"context"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// NodeCapacityReconciler reconciles the ".sliced.units" extended resource on a
// managed Kubernetes Node's status.capacity, driven by Node feature-label and
// capacity changes:
//   - When the admin enables slicing on a model (a valid ".sliced.partitions"
//     feature label), the node advertises "<manufacturer>.sliced.units" = D × the
//     participating card count, the fine-grained sliced counting key Kueue and the
//     scheduler consume. Disabling slicing removes it.
//   - It re-applies level-based after a kubelet restart wipes the capacity.
//
// The large magnitude of ".sliced.units" (D × cards) is reported here rather than
// through the device-plugin's fake-device pool, which would otherwise pressure the
// kubelet device manager.
type NodeCapacityReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeCapacityReconciler)(nil)

func (r *NodeCapacityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	nd := new(core.Node)
	err := r.Client.Get(ctx, req.NamespacedName, nd)
	if err != nil {
		logger.Error(err, "fetch node")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Skip if deleted.
	if nd.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted node")
		return ctrl.Result{}, nil
	}

	// Converge the node's ".sliced.units" capacity onto the desired set (empty for
	// an unmanaged or non-sliced node, which removes any stale key).
	capacityPatch := buildSlicedUnitsCapacityPatch(desiredSlicedUnitsCapacity(nd), nd.Status.Capacity)
	if capacityPatch == nil {
		return ctrl.Result{}, nil
	}
	data := json.ShouldMarshal(map[string]any{
		"status": map[string]any{"capacity": capacityPatch},
	})
	err = r.Client.Status().Patch(ctx, nd, ctrlcli.RawPatch(types.MergePatchType, data))
	if err != nil {
		logger.Error(err, "patch node sliced units capacity")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("patched node sliced units capacity")
	return ctrl.Result{}, nil
}

// desiredSlicedUnitsCapacity computes the ".sliced.units" extended-resource
// capacity a managed node should advertise: for each accelerator model the admin
// has enabled slicing for (a valid ".sliced.partitions" feature label), D × its
// card count (the ".count" feature label), summed per manufacturer. It returns nil
// for an unmanaged node or when no model is sliced.
func desiredSlicedUnitsCapacity(nd *core.Node) core.ResourceList {
	if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
		return nil
	}
	cardsByManufacturer := make(map[string]int64)
	for _, aKey := range nodefeature.ExtractAcceleratableNodeKeys(nd) {
		nodeKey := nodefeature.AcceleratableFeatureLabelPrefix + aKey
		n, err := strconvx.Atoi[int64](nd.Labels[nodeKey+".sliced.partitions"])
		if err != nil || !nodefeature.IsValidSlicedPartitions(n) {
			continue
		}
		cards, err := strconvx.Atoi[int64](nd.Labels[nodeKey+".count"])
		if err != nil || cards <= 0 {
			continue
		}
		manufacturer, _, _ := strings.Cut(aKey, "-")
		cardsByManufacturer[manufacturer] += cards
	}
	if len(cardsByManufacturer) == 0 {
		return nil
	}
	out := make(core.ResourceList, len(cardsByManufacturer))
	for manufacturer, cards := range cardsByManufacturer {
		resName := nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModeSliced)
		out[resName] = *resource.NewQuantity(cards*nodefeature.ResourceMaxUnits, resource.DecimalSI)
	}
	return out
}

// buildSlicedUnitsCapacityPatch returns the status.capacity entries needed to
// converge the node onto desired: each desired ".sliced.units" key whose current
// value differs is set, and any stale ".sliced.units" key absent from desired is
// nulled out (removed). It returns nil when the capacity already matches, so the
// caller can skip the patch — keeping the repatch idempotent. Only ".sliced.units"
// keys are ever emitted, so kubelet-managed capacity is never touched.
func buildSlicedUnitsCapacityPatch(desired, current core.ResourceList) map[string]any {
	patch := make(map[string]any)
	for name, q := range desired {
		if cur, ok := current[name]; !ok || cur.Cmp(q) != 0 {
			patch[string(name)] = q.String()
		}
	}
	for name := range current {
		if !stringx.HasSuffix(string(name), nodefeature.SlicedResourceNameSuffix) {
			continue
		}
		if _, ok := desired[name]; !ok {
			patch[string(name)] = nil
		}
	}
	if len(patch) == 0 {
		return nil
	}
	return patch
}

// slicedUnitsCapacityChanged reports whether any ".sliced.units" capacity entry
// was added, removed, or changed between the two capacity maps.
func slicedUnitsCapacityChanged(oldCap, newCap core.ResourceList) bool {
	for name, q := range newCap {
		if !stringx.HasSuffix(string(name), nodefeature.SlicedResourceNameSuffix) {
			continue
		}
		if old, ok := oldCap[name]; !ok || old.Cmp(q) != 0 {
			return true
		}
	}
	for name := range oldCap {
		if !stringx.HasSuffix(string(name), nodefeature.SlicedResourceNameSuffix) {
			continue
		}
		if _, ok := newCap[name]; !ok {
			return true
		}
	}
	return false
}

func (r *NodeCapacityReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodecapacity").
		For(
			&core.Node{},
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - updated if its managed/acceleratable feature labels changed
				//   (slicing enabled/disabled or card count changed), or a
				//   ".sliced.units" capacity entry changed (self-heal after a
				//   kubelet restart wipes it).
				ctrlpredicate.Funcs{
					DeleteFunc: func(ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp != nil {
							return false
						}
						if !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
							systemname.ManagedLabelKey,
							nodefeature.AcceleratableFeatureLabelPrefix) {
							return true
						}
						return slicedUnitsCapacityChanged(oldNd.Status.Capacity, newNd.Status.Capacity)
					},
				},
			),
		).
		Complete(r)
}
