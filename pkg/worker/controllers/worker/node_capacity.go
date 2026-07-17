package worker

import (
	"context"
	"strings"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// NodeCapacityReconciler reconciles the per-card ".sliced.*" extended resources on a
// managed Kubernetes Node's status.capacity, driven by Node feature-label and capacity
// changes:
//   - For every acceleratable manufacturer whose bare ".sliced" token pool the
//     device-plugin advertises (> 0), the node advertises four counting keys —
//     ".sliced.units" (D × cards), ".sliced.cores-percentage" (overcommit →
//     cards × maxSlices × 100, else cards × 100), ".sliced.memory-percentage" (100 ×
//     cards) and ".sliced.memory-mib" (Σ per-card VRAM) — the keys the
//     scheduler/kubelet count and the device-plugin reads to size each slice. The max
//     slice count and the compute-overcommit flag are read from the same-named Devices
//     CR at reconcile time.
//   - The four keys are presence-gated: they are reverse-patched (removed) once a
//     manufacturer's ".sliced" pool disappears or reaches 0. Because the device-plugin
//     sizes that pool as cards × maxSlices off the same Devices CR, every slicing
//     capability change (enable / disable / resize) also moves the bare ".sliced"
//     capacity the Node predicate already watches — so no separate Devices watch is
//     needed.
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

	// Fetch. Read-only (the capacity patch below targets a fresh Node object), so the
	// deep copy is skipped.
	nd := new(core.Node)
	err := r.Client.Get(ctx, req.NamespacedName, nd, ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "fetch node")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Skip if deleted.
	if nd.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted node")
		return ctrl.Result{}, nil
	}

	// Fetch the same-named Devices CR (one per node); it supplies the per-manufacturer
	// max slice count and compute-overcommit flag. A missing ledger yields no slicing
	// capability, so the ".sliced.*" keys stay reverse-patched until it reports.
	devs := new(workercore.Devices)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: nd.Name}, devs, ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch devices")
			return ctrl.Result{}, err
		}
		devs = nil
	}

	// Converge the node's ".sliced.*" capacities onto the desired set (empty for an
	// unmanaged or non-acceleratable node, which removes any stale key).
	capacityPatch := buildSlicedCapacityPatch(desiredSlicedCapacity(nd, devs), nd.Status.Capacity)
	if capacityPatch == nil {
		return ctrl.Result{}, nil
	}
	data := json.ShouldMarshal(map[string]any{
		"status": map[string]any{"capacity": capacityPatch},
	})
	// Patch a fresh Node object rather than nd: the merge-patch response is decoded back
	// into the target, and nd was read without a deep copy (mutating it would corrupt the
	// shared informer cache).
	patchNode := &core.Node{ObjectMeta: meta.ObjectMeta{Name: nd.Name}}
	err = r.Client.Status().Patch(ctx, patchNode, ctrlcli.RawPatch(types.MergePatchType, data))
	if err != nil {
		logger.Error(err, "patch node sliced capacity")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("patched node sliced capacity")
	return ctrl.Result{}, nil
}

// desiredSlicedCapacity computes the per-card ".sliced.*" extended-resource capacity
// a managed node should advertise. It sums, per manufacturer, over that manufacturer's
// models whose same-named Devices group actually reports a slicing capability (so a
// non-sliceable model — e.g. an Ascend 310 sharing a node with a sliceable 910B — adds
// nothing), while the manufacturer's bare ".sliced" token pool the device-plugin
// advertises is present and > 0:
//   - ".sliced.units"             = Σ cards × D
//   - ".sliced.cores-percentage"  = Σ cards × maxSlices × 100 for a model whose compute
//     may be overcommitted (each of its maxSlices slots may claim 100%), else Σ cards ×
//     100 (compute is a single partitioned per-card budget)
//   - ".sliced.memory-percentage" = Σ cards × 100
//   - ".sliced.memory-mib"        = Σ cards × per-card VRAM MiB
//
// Each model's slice count and overcommit flag are read from its own Devices group,
// matched by the full acceleratable node key ("${manufacturer}-${group
// ID}"), so models of one manufacturer that differ in VRAM (or, in principle, in slice
// count / overcommit) are summed correctly. A manufacturer whose ".sliced" pool is
// absent/0, or with no sliceable model, is omitted, so buildSlicedCapacityPatch
// reverse-patches any stale keys. It returns nil for an unmanaged node.
func desiredSlicedCapacity(nd *core.Node, devs *workercore.Devices) core.ResourceList {
	if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
		return nil
	}
	featuresByKey := slicedFeatureByAcceleratableKey(devs)
	type tally struct {
		units     int64
		cores     int64
		memoryPct int64
		memoryMib int64
	}
	byManufacturer := make(map[string]*tally)
	for _, aKey := range nodefeature.ExtractAcceleratableNodeKeys(nd) {
		nodeKey := nodefeature.AcceleratableFeatureLabelPrefix + aKey
		cards, err := strconvx.Atoi[int64](nd.Labels[nodeKey+".count"])
		if err != nil || cards <= 0 {
			continue
		}
		// The acceleratable node key is "${manufacturer}-${group ID}"; resolve this
		// model's own Devices group by that full key (not the bare group ID, which can
		// collide across manufacturers) and skip it unless the group reports a slicing
		// capability, so a non-sliceable model contributes no ".sliced.*".
		feature, ok := featuresByKey[aKey]
		if !ok {
			continue
		}
		manufacturer, _, _ := strings.Cut(aKey, "-")
		t := byManufacturer[manufacturer]
		if t == nil {
			t = &tally{}
			byManufacturer[manufacturer] = t
		}
		t.units += cards * nodefeature.ResourceMaxUnits
		if feature.SlicedCoresOvercommit() {
			t.cores += cards * int64(feature.MaxSlices()) * 100
		} else {
			t.cores += cards * 100
		}
		t.memoryPct += cards * 100
		t.memoryMib += cards * acceleratableCardMemoryMib(nd, nodeKey)
	}
	if len(byManufacturer) == 0 {
		return nil
	}
	out := make(core.ResourceList, len(byManufacturer)*4)
	for manufacturer, t := range byManufacturer {
		// Presence-gate on the device-plugin's bare ".sliced" token pool: only
		// advertise ".sliced.*" while the plugin is actually serving slices here.
		pool := nd.Status.Capacity[nodefeature.GetAcceleratableResourceName(
			manufacturer, workercore.DeviceAllocationModeSliced)]
		if pool.Sign() <= 0 {
			continue
		}
		out[nodefeature.GetAcceleratableSlicedUnitsResourceName(manufacturer)] = *resource.NewQuantity(t.units, resource.DecimalSI)
		out[nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)] = *resource.NewQuantity(t.cores, resource.DecimalSI)
		out[nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer)] = *resource.NewQuantity(t.memoryPct, resource.DecimalSI)
		out[nodefeature.GetAcceleratableSlicedMemoryMibResourceName(manufacturer)] = *resource.NewQuantity(t.memoryMib, resource.DecimalSI)
	}
	return out
}

// slicedFeatureByAcceleratableKey indexes a node's Devices groups that report a slicing
// capability (MaxSlices() > 0) by their "${manufacturer}-${group ID}" key, so
// desiredSlicedCapacity can resolve each acceleratable model to its own capability. The key
// is manufacturer-qualified because ConstructGroupID strips the vendor prefix, so a bare
// group ID can collide across manufacturers on a node. A group with no slicing capability is
// omitted, so its cards contribute no ".sliced.*". It returns nil when devs is nil (no
// ledger reported yet).
func slicedFeatureByAcceleratableKey(devs *workercore.Devices) map[string]workercore.AcceleratorsFeature {
	if devs == nil {
		return nil
	}
	out := make(map[string]workercore.AcceleratorsFeature, len(devs.Spec.Groups))
	for i := range devs.Spec.Groups {
		g := &devs.Spec.Groups[i]
		if g.AcceleratorsFeature.MaxSlices() <= 0 {
			continue
		}
		out[g.Manufacturer+"-"+g.ID] = g.AcceleratorsFeature
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

// isSlicedFamilyCapacityKey reports whether name is any ".sliced"-family capacity key
// the reconciler must react to: the bare device-plugin ".sliced" token pool (the
// presence gate that decides whether to advertise) or one of the four owned ".sliced.*"
// counting keys. Only the owned keys are ever patched (see isSlicedCapacityKey); the
// bare pool is watched but never written.
func isSlicedFamilyCapacityKey(name core.ResourceName) bool {
	return isSlicedCapacityKey(name) ||
		stringx.HasSuffix(string(name), nodefeature.SlicedResourceNameSuffix)
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

// slicedFamilyCapacityChanged reports whether any ".sliced"-family capacity entry — the
// bare device-plugin ".sliced" token pool (the presence gate) or one of the four owned
// ".sliced.*" counting keys — was added, removed, or changed between the two capacity
// maps.
func slicedFamilyCapacityChanged(oldCap, newCap core.ResourceList) bool {
	for name, q := range newCap {
		if !isSlicedFamilyCapacityKey(name) {
			continue
		}
		if old, ok := oldCap[name]; !ok || old.Cmp(q) != 0 {
			return true
		}
	}
	for name := range oldCap {
		if !isSlicedFamilyCapacityKey(name) {
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
				//   (model added/removed or card count changed), or a ".sliced"-family
				//   capacity entry changed — either the device-plugin's bare ".sliced"
				//   token pool appearing/disappearing/resizing (the presence gate and the
				//   sole signal of a slicing-capability change) or an owned ".sliced.*"
				//   key (self-heal after a kubelet restart wipes it).
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
						return slicedFamilyCapacityChanged(oldNd.Status.Capacity, newNd.Status.Capacity)
					},
				},
			),
		).
		Complete(r)
}
