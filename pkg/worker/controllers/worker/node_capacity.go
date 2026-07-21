package worker

import (
	"context"
	"maps"
	"strings"
	"time"

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
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// NodeCapacityReconciler reconciles the per-card ".sliced.*" extended resources on a
// managed Kubernetes Node's status.capacity, driven by Node feature-label/capacity changes
// and by the same-named Devices ledger's slicing detail:
//   - For every acceleratable manufacturer whose bare ".sliced" token pool the
//     device-plugin advertises (> 0), the node advertises up to four counting keys —
//     ".sliced.units" (D × every sliceable card, MIG included) plus, for a model with
//     soft-sliceable cards, ".sliced.cores-percentage", ".sliced.memory-percentage" and
//     ".sliced.memory-mib" — the keys the scheduler/kubelet count and the device-plugin
//     reads to size each slice. The per-card slice counts and the compute-overcommit flag
//     are read from the same-named Devices CR at reconcile time; see desiredSlicedCapacity
//     for the sliceable-vs-soft split.
//   - The keys are presence-gated: they are reverse-patched (removed) once a manufacturer's
//     ".sliced" pool disappears or reaches 0, and the three logical keys drop for a model
//     whose cards are all MIG (no soft budget) while ".sliced.units" stays.
//   - It watches the Devices ledger so a MIG mode change — which re-splits the per-card
//     sliceable/soft populations without necessarily moving the bare ".sliced" pool the
//     Node predicate already watches — enqueues the owning node.
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

// desiredSlicedCapacity computes the per-card ".sliced.*" extended-resource capacity a
// managed node should advertise, summed per manufacturer over that manufacturer's models
// whose same-named Devices group reports a slicing capability (so a non-sliceable model —
// e.g. an Ascend 310 sharing a node with a sliceable 910B — adds nothing), while the
// manufacturer's bare ".sliced" token pool the device-plugin advertises is present and > 0.
// The card population is split by slicing kind, because ".sliced.units" — the universal
// Kueue quota unit, consumed by both logical and physical/MIG requests — counts every
// sliceable card, while the three logical-only keys count only logically-sliceable (soft)
// cards:
//   - ".sliced.units"             = Σ sliceableCards × D   (logical ∨ physical; MIG included)
//   - ".sliced.cores-percentage"  = Σ Detail.Logical.Count × 100 (overcommit) else Σ softCards × 100
//   - ".sliced.memory-percentage" = Σ softCards × 100
//   - ".sliced.memory-mib"        = Σ softCards × per-card VRAM MiB
//
// All card counts come from the Devices ledger's per-card data as the single source of
// truth — never mixed with the Node ".count" label — resolved to each model by the full
// acceleratable node key ("${manufacturer}-${group ID}"), so models of one manufacturer that
// differ in VRAM (or slice count / overcommit) are summed correctly. The three logical keys
// are omitted for a model with no soft cards (e.g. all-MIG), so buildSlicedCapacityPatch
// reverse-patches any stale ones while ".sliced.units" stays.
//
// The per-card VRAM stays the lossy ".memory" label (rounded to Gi), never the exact
// DevicesGroup.Memory, so the advertised memory-mib matches the Pod-webhook anchor. A
// manufacturer whose ".sliced" pool is absent/0, or with no sliceable model, is omitted. It
// returns nil for an unmanaged node.
func desiredSlicedCapacity(nd *core.Node, devs *workercore.Devices) core.ResourceList {
	if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
		return nil
	}
	groupsByKey := devicesGroupsByAcceleratableKey(devs)
	type tally struct {
		units      int64
		cores      int64
		memoryPct  int64
		memoryMib  int64
		hasLogical bool // any soft-sliceable contribution → emit the three logical keys
	}
	byManufacturer := make(map[string]*tally)
	for _, aKey := range nodefeature.ExtractAcceleratableNodeKeys(nd) {
		grp, ok := groupsByKey[aKey]
		if !ok {
			continue
		}
		nodeKey := nodefeature.AcceleratableFeatureLabelPrefix + aKey
		vram := acceleratableCardMemoryMib(nd, nodeKey)

		var units, cores, memoryPct, memoryMib int64
		var logical bool
		sliceable, soft, softSlices := slicedCards(grp)
		if sliceable == 0 {
			// Non-sliceable model: contributes nothing.
			continue
		}
		// ".sliced.units" counts every sliceable card (MIG included); the three logical
		// keys count only the soft cards, all sourced from the same per-card recount.
		units = sliceable * nodefeature.ResourceMaxUnits
		if soft > 0 {
			logical = true
			if grp.AcceleratorSlicedDetail.Logical.CoresPercentageOvercommit {
				cores = softSlices * 100
			} else {
				cores = soft * 100
			}
			memoryPct = soft * 100
			memoryMib = soft * vram
		}

		manufacturer, _, _ := strings.Cut(aKey, "-")
		t := byManufacturer[manufacturer]
		if t == nil {
			t = &tally{}
			byManufacturer[manufacturer] = t
		}
		t.units += units
		t.cores += cores
		t.memoryPct += memoryPct
		t.memoryMib += memoryMib
		t.hasLogical = t.hasLogical || logical
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
		if !t.hasLogical {
			// Every card is hard-partitioned (MIG): the logical keys carry no soft budget,
			// so omit them while ".sliced.units" still counts the MIG cards.
			continue
		}
		out[nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)] = *resource.NewQuantity(t.cores, resource.DecimalSI)
		out[nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer)] = *resource.NewQuantity(t.memoryPct, resource.DecimalSI)
		out[nodefeature.GetAcceleratableSlicedMemoryMibResourceName(manufacturer)] = *resource.NewQuantity(t.memoryMib, resource.DecimalSI)
	}
	return out
}

// devicesGroupsByAcceleratableKey indexes a node's Devices groups by their
// "${manufacturer}-${group ID}" acceleratable key, so desiredSlicedCapacity resolves each
// node model to its own group's per-card slicing data. The key is manufacturer-qualified
// because ConstructGroupID strips the vendor prefix, so a bare group ID can collide across
// manufacturers on one node. It returns nil when devs is nil (no ledger reported yet).
func devicesGroupsByAcceleratableKey(devs *workercore.Devices) map[string]*workercore.DevicesGroup {
	if devs == nil {
		return nil
	}
	out := make(map[string]*workercore.DevicesGroup, len(devs.Spec.Groups))
	for i := range devs.Spec.Groups {
		g := &devs.Spec.Groups[i]
		out[g.Manufacturer+"-"+g.ID] = g
	}
	return out
}

// slicedCards counts a Devices group's sliceable and soft-only cards from its per-card data and
// sums the soft cards' logical slice counts. sliceable counts cards offering any slice budget
// (logical ∨ physical, so MIG cards are included); soft counts cards offering a logical (soft)
// budget only; softSlices is Σ their LogicalSliced.Count. All are 0 for a group with no
// sliceable cards, which contributes nothing to desiredSlicedCapacity.
func slicedCards(g *workercore.DevicesGroup) (sliceable, soft, softSlices int64) {
	for i := range g.Accelerators {
		st := &g.Accelerators[i].Status
		logical := st.LogicalSliced.Count > 0
		if logical || st.PhysicalSliced.Count > 0 {
			sliceable++
		}
		if logical {
			soft++
			softSlices += int64(st.LogicalSliced.Count)
		}
	}
	return sliceable, soft, softSlices
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

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

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
				//   token pool appearing/disappearing/resizing (the presence gate) or an
				//   owned ".sliced.*" key (self-heal after a kubelet restart wipes it). A
				//   change to the per-card slicing detail itself arrives via the Devices
				//   watch below.
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
		Watches(
			// A Devices ledger's slicing detail change — notably a MIG mode toggle that
			// re-splits the per-card sliceable/soft populations — moves the desired
			// ".sliced.*" capacity without necessarily moving the bare ".sliced" pool the
			// Node predicate watches, so enqueue the name-identical node. The 3s dedup
			// window coalesces bursts of detection writes.
			&workercore.Devices{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueNodeWhenDevicesChanged,
			),
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				// A managed Devices' slicing detail: on create/delete the whole capability
				// appears/vanishes; on update fire only when the spec slicing detail changed,
				// never on Status (allocation) churn or non-slicing spec fields like health.
				CreateFunc: func(e ctrlevent.CreateEvent) bool {
					return isManagedDevices(e.Object)
				},
				DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
					return isManagedDevices(e.Object)
				},
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					oldDevs, newDevs := e.ObjectOld.(*workercore.Devices), e.ObjectNew.(*workercore.Devices)
					if !isManagedDevices(newDevs) {
						return false
					}
					return slicedDetailChanged(oldDevs.Spec.Groups, newDevs.Spec.Groups)
				},
			}),
		).
		Complete(r)
}

// isManagedDevices reports whether a Devices ledger belongs to an operator-managed node.
func isManagedDevices(obj ctrlcli.Object) bool {
	return obj.GetLabels()[systemname.ManagedLabelKey] == "true"
}

// enqueueNodeWhenDevicesChanged maps a changed Devices ledger to its name-identical Node
// (both are cluster-scoped and share the node name), so a per-card slicing detail change
// reconciles that node's ".sliced.*" capacity.
func (r *NodeCapacityReconciler) enqueueNodeWhenDevicesChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: obj.GetName()}}
	ctrllog.FromContext(ctx).V(2).Info("enqueued node from devices", "request", req)
	return []ctrlreconcile.Request{req}
}

// slicedSignature captures exactly the Devices group fields desiredSlicedCapacity consumes:
// the per-card sliceable/soft split and the logical detail (count + overcommit). Comparing
// signatures lets the Devices watch fire only on a real slicing-detail change.
type slicedSignature struct {
	sliceable         int64
	soft              int64
	logicalCount      int32
	logicalOvercommit bool
}

// slicedDetailChanged reports whether the slicing capability desiredSlicedCapacity consumes
// changed between two Devices spec snapshots. It reads only Spec.Groups (never Status, where
// allocation churn lives) and projects each group to its slicing signature, so a status
// update or a non-slicing spec change (e.g. a health flip) does not fire the watch (R7).
func slicedDetailChanged(oldGroups, newGroups []workercore.DevicesGroup) bool {
	return !maps.Equal(slicedSignatures(oldGroups), slicedSignatures(newGroups))
}

// slicedSignatures projects a Devices spec's groups to their slicing signatures, keyed by the
// "${manufacturer}-${group ID}" acceleratable key.
func slicedSignatures(groups []workercore.DevicesGroup) map[string]slicedSignature {
	out := make(map[string]slicedSignature, len(groups))
	for i := range groups {
		g := &groups[i]
		sliceable, soft, _ := slicedCards(g)
		out[g.Manufacturer+"-"+g.ID] = slicedSignature{
			sliceable:         sliceable,
			soft:              soft,
			logicalCount:      g.AcceleratorSlicedDetail.Logical.Count,
			logicalOvercommit: g.AcceleratorSlicedDetail.Logical.CoresPercentageOvercommit,
		}
	}
	return out
}
