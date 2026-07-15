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

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// NodeCapacityReconciler reconciles the per-card ".sliced.*" extended resources on a
// managed Kubernetes Node's status.capacity, driven by Node feature-label and
// capacity changes:
//   - For every acceleratable model the node advertises four counting keys per
//     manufacturer — ".sliced.units" (D × cards), ".sliced.cores-percentage"
//     (SlicedResourceMaxSize × 100 × cards), ".sliced.memory-percentage" (100 ×
//     cards) and ".sliced.memory-mib" (Σ per-card VRAM) — the gate-2 keys the
//     scheduler/kubelet count and the device-plugin reads to size each slice.
//   - It re-applies level-based after a kubelet restart wipes the capacity.
//
// The large magnitude of these keys (notably ".sliced.units" = D × cards) is reported
// here rather than through the device-plugin's fake-device pool, which would
// otherwise pressure the kubelet device manager.
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

	// Converge the node's ".sliced.*" capacities onto the desired set (empty for an
	// unmanaged or non-acceleratable node, which removes any stale key).
	capacityPatch := buildSlicedCapacityPatch(desiredSlicedCapacity(nd), nd.Status.Capacity)
	if capacityPatch == nil {
		return ctrl.Result{}, nil
	}
	data := json.ShouldMarshal(map[string]any{
		"status": map[string]any{"capacity": capacityPatch},
	})
	err = r.Client.Status().Patch(ctx, nd, ctrlcli.RawPatch(types.MergePatchType, data))
	if err != nil {
		logger.Error(err, "patch node sliced capacity")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("patched node sliced capacity")
	return ctrl.Result{}, nil
}

// desiredSlicedCapacity computes the per-card ".sliced.*" extended-resource capacity
// a managed node should advertise. For every acceleratable model with a positive card
// count it emits, summed per manufacturer:
//   - ".sliced.units"             = cards × D
//   - ".sliced.cores-percentage"  = cards × SlicedResourceMaxSize × 100 (slices may
//     oversubscribe compute, so each of the MaxPartitions slots is worth 100%)
//   - ".sliced.memory-percentage" = cards × 100
//   - ".sliced.memory-mib"        = Σ (cards × per-card VRAM MiB), weighted per model
//     since different models of one manufacturer can have different VRAM
//
// It returns nil for an unmanaged node or when no acceleratable model is present.
// Slicing is no longer admin-gated by ".sliced.partitions": every acceleratable model
// is sliceable (the detector reports a fixed SlicedResourceMaxSize budget).
func desiredSlicedCapacity(nd *core.Node) core.ResourceList {
	if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
		return nil
	}
	type tally struct {
		cards     int64
		memoryMib int64
	}
	byManufacturer := make(map[string]*tally)
	for _, aKey := range nodefeature.ExtractAcceleratableNodeKeys(nd) {
		nodeKey := nodefeature.AcceleratableFeatureLabelPrefix + aKey
		cards, err := strconvx.Atoi[int64](nd.Labels[nodeKey+".count"])
		if err != nil || cards <= 0 {
			continue
		}
		manufacturer, _, _ := strings.Cut(aKey, "-")
		t := byManufacturer[manufacturer]
		if t == nil {
			t = &tally{}
			byManufacturer[manufacturer] = t
		}
		t.cards += cards
		t.memoryMib += cards * acceleratableCardMemoryMib(nd, nodeKey)
	}
	if len(byManufacturer) == 0 {
		return nil
	}
	out := make(core.ResourceList, len(byManufacturer)*4)
	for manufacturer, t := range byManufacturer {
		units := nodefeature.GetAcceleratableSlicedUnitsResourceName(manufacturer)
		cores := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)
		memPct := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer)
		memMib := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(manufacturer)
		out[units] = *resource.NewQuantity(t.cards*nodefeature.ResourceMaxUnits, resource.DecimalSI)
		out[cores] = *resource.NewQuantity(t.cards*nodefeature.SlicedResourceMaxSize*100, resource.DecimalSI)
		out[memPct] = *resource.NewQuantity(t.cards*100, resource.DecimalSI)
		out[memMib] = *resource.NewQuantity(t.memoryMib, resource.DecimalSI)
	}
	return out
}

// acceleratableCardMemoryMib parses the per-card VRAM (in MiB) from a model's
// "<nodeKey>.memory" feature label, a resource.Quantity such as "24Gi". It returns 0
// when the label is absent or unparseable, so a model with no known VRAM simply
// contributes no ".sliced.memory-mib" capacity.
func acceleratableCardMemoryMib(nd *core.Node, nodeKey string) int64 {
	q, err := resource.ParseQuantity(nd.Labels[nodeKey+".memory"])
	if err != nil {
		return 0
	}
	return q.Value() / (1 << 20) // bytes → MiB
}

// slicedCapacitySuffixes are the ".sliced.*" extended-resource suffixes the
// NodeCapacityReconciler owns via Patch Node. The bare ".sliced"/".shared" keys
// advertised by the device-plugin are deliberately excluded so they are never
// nulled out.
var slicedCapacitySuffixes = []string{
	nodefeature.SlicedUnitsResourceNameSuffix,
	nodefeature.SlicedCoresPercentageResourceNameSuffix,
	nodefeature.SlicedMemoryPercentageResourceNameSuffix,
	nodefeature.SlicedMemoryMibResourceNameSuffix,
}

// isSlicedCapacityKey reports whether name is one of the NodeCapacityReconciler-owned
// ".sliced.*" keys (and not a bare ".sliced"/".shared" device-plugin key).
func isSlicedCapacityKey(name core.ResourceName) bool {
	for _, suffix := range slicedCapacitySuffixes {
		if stringx.HasSuffix(string(name), suffix) {
			return true
		}
	}
	return false
}

// buildSlicedCapacityPatch returns the status.capacity entries needed to converge the
// node onto desired: each desired ".sliced.*" key whose current value differs is set,
// and any stale ".sliced.*" key absent from desired is nulled out (removed). It
// returns nil when the capacity already matches, so the caller can skip the patch —
// keeping the repatch idempotent. Only NodeCapacityReconciler-owned ".sliced.*" keys
// are ever emitted, so kubelet-managed capacity is never touched.
func buildSlicedCapacityPatch(desired, current core.ResourceList) map[string]any {
	patch := make(map[string]any)
	for name, q := range desired {
		if cur, ok := current[name]; !ok || cur.Cmp(q) != 0 {
			patch[string(name)] = q.String()
		}
	}
	for name := range current {
		if !isSlicedCapacityKey(name) {
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

// slicedCapacityChanged reports whether any ".sliced.*" capacity entry was added,
// removed, or changed between the two capacity maps.
func slicedCapacityChanged(oldCap, newCap core.ResourceList) bool {
	for name, q := range newCap {
		if !isSlicedCapacityKey(name) {
			continue
		}
		if old, ok := oldCap[name]; !ok || old.Cmp(q) != 0 {
			return true
		}
	}
	for name := range oldCap {
		if !isSlicedCapacityKey(name) {
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
				//   (model added/removed or card count changed), or a
				//   ".sliced.*" capacity entry changed (self-heal after a
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
						return slicedCapacityChanged(oldNd.Status.Capacity, newNd.Status.Capacity)
					},
				},
			),
		).
		Complete(r)
}
