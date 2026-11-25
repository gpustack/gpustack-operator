package worker

import (
	"context"
	"maps"
	"math"
	"math/bits"
	"sort"
	"strconv"
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
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
)

// NodeCapacityReconciler reconciles the accelerator counting extended resources on a managed
// Kubernetes Node's status.capacity, driven by Node feature-label/capacity changes and by the
// same-named Devices object:
//   - Software slicing and hardware partitioning are two families over two card populations
//     that never overlap, so each has its own counting keys and each is presence-gated on its
//     own bare device-plugin token pool. A manufacturer's ".sliced.*" keys appear only while
//     its ".sliced" pool does, and its ".partitioned.*" keys only while its ".partitioned"
//     pool does; a pool that disappears or reaches 0 reverse-patches its family's keys away.
//     See desiredAcceleratorCapacity for the key set and how each is valued.
//   - The per-profile partition keys are derived from the runtime ledger rather than from a
//     static ceiling, because the profiles compete for the same physical slices: carving one
//     shape consumes room another shape needed. That makes an allocation or a release move a
//     capacity value with the capability untouched, which is why the Devices watch reacts to
//     the status side as well as the spec side.
//   - It re-applies level-based after a kubelet restart wipes the capacity.
//
// The large magnitude of the ".units" keys (D per card) is reported here rather than through
// the device-plugin's fake-device pool, which would otherwise pressure the kubelet device
// manager.
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
	capacityPatch := buildAcceleratorCapacityPatch(desiredAcceleratorCapacity(nd, devs), nd.Status.Capacity)
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
		logger.Error(err, "patch node accelerator capacity")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("patched node accelerator capacity")
	return ctrl.Result{}, nil
}

// desiredAcceleratorCapacity computes the counting extended resources a managed node should
// advertise, summed per manufacturer over that manufacturer's models whose same-named Devices
// group reports a slicing capability (so a non-sliceable model — e.g. an Ascend 310 sharing a
// node with a sliceable 910B — adds nothing).
//
// The two families count disjoint card populations, because a card in a hardware partitioning
// mode can serve no software slice and vice versa:
//   - ".sliced.units"                     = Σ logicalCards × D
//   - ".sliced.cores-percentage"          = Σ Detail.Logical.Count × 100 (overcommit) else Σ logicalCards × 100
//   - ".sliced.memory-percentage"         = Σ logicalCards × 100
//   - ".sliced.memory-mib"                = Σ logicalCards × per-card VRAM MiB
//   - ".partitioned.units"                = Σ partitionedCards × D
//   - ".partitioned.<kind>-<profile>"     = Σ (allocated + remaining) instances of that profile
//
// Each family is presence-gated on its own bare device-plugin token pool, so a node advertises
// a family's counting keys only while the plugin is actually serving that family here. The gate
// reads capacity rather than allocatable: capacity already falls to zero on a node whose cards
// cannot serve the family, while allocatable also falls to zero whenever the family is merely
// saturated or held — gating on that would delete the counting keys while instances are live and
// re-add them on release.
//
// All card counts come from the Devices ledger's per-card data as the single source of truth —
// never mixed with the Node ".count" label — resolved to each model by the full acceleratable
// node key ("${manufacturer}-${group ID}"), so models of one manufacturer that differ in VRAM
// (or slice count / overcommit) are summed correctly. A family with no cards emits no key, so
// buildAcceleratorCapacityPatch reverse-patches any stale one.
//
// The per-card VRAM stays the lossy ".memory" label (rounded to Gi), never the exact
// DevicesGroup.Memory, so the advertised memory-mib matches the Pod-webhook anchor. It returns
// nil for an unmanaged node.
func desiredAcceleratorCapacity(nd *core.Node, devs *workercore.Devices) core.ResourceList {
	if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
		return nil
	}
	groupsByKey := devicesGroupsByAcceleratableKey(devs)
	ledgerByGroup := devicesLedgerByGroup(devs)
	type tally struct {
		units            int64
		cores            int64
		memoryPct        int64
		memoryMib        int64
		hasLogical       bool
		partitionedUnits int64
		hasPartitioned   bool
		// profile name → Σ (allocated + remaining) instances over the partitioned cards.
		partitionProfiles map[string]int64
	}
	byManufacturer := make(map[string]*tally)
	for _, aKey := range nodefeature.ExtractAcceleratableNodeKeys(nd) {
		grp, ok := groupsByKey[aKey]
		if !ok {
			continue
		}
		nodeKey := nodefeature.AcceleratableFeatureLabelPrefix + aKey
		vram := acceleratableCardMemoryMib(nd, nodeKey)
		ledger := ledgerByGroup[aKey]

		contrib := groupCapacityContribution(grp, ledger, vram)
		if !contrib.hasLogical && !contrib.hasPartitioned {
			// Non-sliceable or all-unhealthy idle model: contributes nothing.
			continue
		}

		manufacturer, _, _ := strings.Cut(aKey, "-")
		t := byManufacturer[manufacturer]
		if t == nil {
			t = &tally{}
			byManufacturer[manufacturer] = t
		}
		if contrib.hasLogical {
			t.hasLogical = true
			t.units += contrib.units
			t.cores += contrib.cores
			t.memoryPct += contrib.memoryPct
			t.memoryMib += contrib.memoryMib
		}
		if contrib.hasPartitioned {
			t.hasPartitioned = true
			t.partitionedUnits += contrib.partitionedUnits
			for name, count := range partitionInstancesByProfile(grp, ledger) {
				if t.partitionProfiles == nil {
					t.partitionProfiles = make(map[string]int64)
				}
				t.partitionProfiles[name] += count
			}
		}
	}
	if len(byManufacturer) == 0 {
		return nil
	}
	out := make(core.ResourceList, len(byManufacturer)*6)
	for manufacturer, t := range byManufacturer {
		if t.hasLogical && poolAdvertised(nd, manufacturer, workercore.DeviceAllocationModeSliced) {
			out[nodefeature.GetAcceleratableSlicedUnitsResourceName(manufacturer)] = *resource.NewQuantity(t.units, resource.DecimalSI)
			out[nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)] = *resource.NewQuantity(t.cores, resource.DecimalSI)
			out[nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer)] = *resource.NewQuantity(t.memoryPct, resource.DecimalSI)
			out[nodefeature.GetAcceleratableSlicedMemoryMibResourceName(manufacturer)] = *resource.NewQuantity(t.memoryMib, resource.DecimalSI)
		}
		if !t.hasPartitioned || !poolAdvertised(nd, manufacturer, workercore.DeviceAllocationModePartitioned) {
			continue
		}
		out[nodefeature.GetAcceleratablePartitionedUnitsResourceName(manufacturer)] = *resource.NewQuantity(t.partitionedUnits, resource.DecimalSI)
		// One key per profile the node's partitioned cards offer; a profile whose last
		// offering card leaves simply stops being emitted and is reverse-patched away.
		for name, count := range t.partitionProfiles {
			resName := nodefeature.GetAcceleratablePartitionedProfileResourceName(manufacturer, name)
			if resName == "" {
				continue
			}
			out[resName] = *resource.NewQuantity(count, resource.DecimalSI)
		}
	}
	return out
}

// poolAdvertised reports whether the device plugin currently advertises a manufacturer's bare
// token pool for a family on this node, which is what gates the family's counting keys.
func poolAdvertised(nd *core.Node, manufacturer string, mode workercore.DeviceAllocationMode) bool {
	resName := nodefeature.GetAcceleratableResourceName(manufacturer, mode)
	if resName == "" {
		return false
	}
	pool := nd.Status.Capacity[resName]
	return pool.Sign() > 0
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

type groupCapacity struct {
	hasLogical       bool
	units            int64
	cores            int64
	memoryPct        int64
	memoryMib        int64
	hasPartitioned   bool
	partitionedUnits int64
}

// groupCapacityContribution computes a Devices group's capacity contribution across its two
// disjoint populations (logical-sliced and partitioned). Healthy cards contribute their full card
// capacity; unhealthy cards contribute zero free capacity while still accounting for any live
// allocations so running Pods on an unhealthy card do not starve healthy siblings.
func groupCapacityContribution(
	g *workercore.DevicesGroup,
	ledger map[string]*workercore.AcceleratorAllocation,
	vram int64,
) groupCapacity {
	const d = int64(nodefeature.ResourceMaxUnits)
	overcommit := g.AcceleratorSlicedDetail.Logical.CoresPercentageOvercommit

	var out groupCapacity
	for i := range g.Accelerators {
		acc := &g.Accelerators[i]
		st := acc.Status
		alloc := ledger[acc.ID]

		switch {
		case device.IsPartitioned(st):
			if !st.Unhealthy {
				out.hasPartitioned = true
				out.partitionedUnits += d
				continue
			}
			if alloc == nil || alloc.Mode != workercore.DeviceAllocationModePartitioned {
				continue
			}
			usedUnits := liveAllocatedUnits(alloc, d)
			if usedUnits > 0 || hasPositiveProfileCount(alloc.AllocatedProfiles) {
				out.hasPartitioned = true
				out.partitionedUnits += usedUnits
			}
		case device.IsLogicallySliceable(st):
			if !st.Unhealthy {
				out.hasLogical = true
				out.units += d
				if overcommit {
					out.cores += int64(st.LogicalSliced.Count) * 100
				} else {
					out.cores += 100
				}
				out.memoryPct += 100
				out.memoryMib += vram
				continue
			}
			if alloc == nil || alloc.Mode != workercore.DeviceAllocationModeSliced {
				continue
			}
			usedUnits := liveAllocatedUnits(alloc, d)
			if usedUnits == 0 && alloc.AllocatedSlices <= 0 {
				continue
			}
			out.hasLogical = true
			out.units += usedUnits
			if overcommit {
				slices := min(max(alloc.AllocatedSlices, 1), st.LogicalSliced.Count)
				out.cores += int64(slices) * 100
			} else {
				// Devices.Status does not record per-card allocated cores-percentage,
				// so reserve one card's 100% ceiling for the running slice(s).
				out.cores += 100
			}
			out.memoryPct += ceilMulDiv(usedUnits, 100, d)
			out.memoryMib += ceilMulDiv(usedUnits, vram, d)
		}
	}
	return out
}

func liveAllocatedUnits(alloc *workercore.AcceleratorAllocation, maxUnits int64) int64 {
	if alloc == nil || alloc.Mode == workercore.DeviceAllocationModeNone {
		return 0
	}
	rem := max(int64(alloc.Remaining), 0)
	return max(maxUnits-rem, 0)
}

func hasPositiveProfileCount(profiles []workercore.AcceleratorProfileCount) bool {
	for i := range profiles {
		if profiles[i].Count > 0 {
			return true
		}
	}
	return false
}

// ceilMulDiv returns ceil(a*b/d) for non-negative inputs without int64 overflow, capped at b when a >= d.
func ceilMulDiv(a, b, d int64) int64 {
	if a <= 0 || b <= 0 || d <= 0 {
		return 0
	}
	if a >= d {
		return b
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, rem := bits.Div64(hi, lo, uint64(d))
	if rem != 0 {
		q++
	}
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// partitionInstancesByProfile counts, per profile name, the hardware partitions a group's
// partitioned cards can account for: summed over those cards, each profile's allocated instances
// plus, for healthy cards, the instances it can still host.
//
// Both terms are needed. The scheduler fits a Pod by subtracting the requests of the Pods already
// on the node from the advertised capacity, so publishing only what is still free would subtract
// every live instance twice and the key would read one short per running partition. An unhealthy
// card offers zero remaining room while keeping its live allocated instances counted.
func partitionInstancesByProfile(g *workercore.DevicesGroup, ledger map[string]*workercore.AcceleratorAllocation) map[string]int64 {
	out := make(map[string]int64)
	for i := range g.Accelerators {
		acc := &g.Accelerators[i]
		if !device.IsPartitioned(acc.Status) {
			continue
		}
		alloc := ledger[acc.ID]
		ready := device.PartitionLedgerReady(alloc)
		// Every profile the card offers gets an entry, even at zero. A profile whose room another
		// profile's instance consumed then reads zero instead of vanishing, so the key's value
		// moves as the card fills rather than the key itself appearing and disappearing — one
		// fewer reason to rewrite the node object on every carve and release.
		for k := range acc.Status.PhysicalSliced.Profiles {
			p := &acc.Status.PhysicalSliced.Profiles[k]
			if _, seen := out[p.Name]; !seen {
				out[p.Name] = 0
			}
			if !acc.Status.Unhealthy && !ready {
				out[p.Name] += int64(p.Count)
			}
		}
		if alloc != nil {
			for k := range alloc.AllocatedProfiles {
				if c := alloc.AllocatedProfiles[k].Count; c > 0 {
					out[alloc.AllocatedProfiles[k].Name] += int64(c)
				}
			}
		}
		if acc.Status.Unhealthy || !ready {
			continue
		}
		for k := range alloc.RemainingProfiles {
			if c := alloc.RemainingProfiles[k].Count; c > 0 {
				out[alloc.RemainingProfiles[k].Name] += int64(c)
			}
		}
	}
	return out
}

// devicesLedgerByGroup indexes a node's runtime allocation ledger by the same
// "${manufacturer}-${group ID}" key the spec side uses, and then by accelerator ID. The
// capability lives on the Devices spec and the occupancy on its status, so the per-profile
// capacity key is a join of the two; reading occupancy off the spec silently yields nothing.
// The key is manufacturer-qualified for the same reason the spec index is: ConstructGroupID
// strips the vendor prefix, so a bare group ID can collide across manufacturers on one node and
// one vendor's ledger would then be read for another vendor's cards.
func devicesLedgerByGroup(devs *workercore.Devices) map[string]map[string]*workercore.AcceleratorAllocation {
	if devs == nil {
		return nil
	}
	out := make(map[string]map[string]*workercore.AcceleratorAllocation, len(devs.Status.Groups))
	for i := range devs.Status.Groups {
		grp := &devs.Status.Groups[i]
		accs := make(map[string]*workercore.AcceleratorAllocation, len(grp.Accelerators))
		for j := range grp.Accelerators {
			accs[grp.Accelerators[j].ID] = &grp.Accelerators[j]
		}
		out[grp.Manufacturer+"-"+grp.ID] = accs
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

// ownedCapacitySuffixes are the fixed counting-key suffixes the NodeCapacityReconciler owns via
// Patch Node. The per-profile ".partitioned.<kind>-<profile>" keys have a variable suffix (the
// profile name) and are matched by parsing instead (see isOwnedCapacityKey). The bare ".sliced",
// ".partitioned" and ".shared" keys the device-plugin advertises are deliberately excluded so
// they are never nulled out.
var ownedCapacitySuffixes = []string{
	nodefeature.SlicedUnitsResourceNameSuffix,
	nodefeature.SlicedCoresPercentageResourceNameSuffix,
	nodefeature.SlicedMemoryPercentageResourceNameSuffix,
	nodefeature.SlicedMemoryMibResourceNameSuffix,
	nodefeature.PartitionedUnitsResourceNameSuffix,
}

// isOwnedCapacityKey reports whether name is one of the counting keys this reconciler owns —
// the five fixed keys or a per-profile ".partitioned.<kind>-<profile>" key — and not a bare
// device-plugin token key.
func isOwnedCapacityKey(name core.ResourceName) bool {
	// Positively identify a per-profile key (known accelerator base, known partition kind,
	// non-empty profile) rather than matching a raw infix, so a foreign extended resource that
	// merely looks similar is never claimed as owned and nulled out when absent from desired.
	if _, ok := nodefeature.PartitionedProfileOf(name); ok {
		return true
	}
	return hasAcceleratorSuffix(name, ownedCapacitySuffixes...)
}

// isAcceleratorFamilyCapacityKey reports whether name is any capacity key this reconciler must
// react to: a bare device-plugin token pool for either slicing family (the presence gate that
// decides whether to advertise) or one of the owned counting keys. Only the owned keys are ever
// patched (see isOwnedCapacityKey); the bare pools are watched but never written.
func isAcceleratorFamilyCapacityKey(name core.ResourceName) bool {
	return isOwnedCapacityKey(name) ||
		hasAcceleratorSuffix(name,
			nodefeature.SlicedResourceNameSuffix,
			nodefeature.PartitionedResourceNameSuffix)
}

// hasAcceleratorSuffix reports whether name is one of our suffixes behind a known accelerator
// base. The base check is what keeps a foreign extended resource that happens to end the same
// way — another plugin's "example.com/foo.partitioned.units" — from being claimed as ours and
// then nulled out for being absent from the desired set.
func hasAcceleratorSuffix(name core.ResourceName, suffixes ...string) bool {
	for _, suffix := range suffixes {
		base, ok := strings.CutSuffix(string(name), suffix)
		if ok && nodefeature.IsKnownAcceleratableResourceName(core.ResourceName(base)) {
			return true
		}
	}
	return false
}

// buildAcceleratorCapacityPatch returns the status.capacity entries needed to converge the node
// onto desired: each desired counting key whose current value differs is set, and any stale
// owned key absent from desired is nulled out (removed). It returns nil when the capacity
// already matches, so the caller can skip the patch — keeping the repatch idempotent. Only
// owned keys are ever emitted, so kubelet-managed capacity is never touched.
func buildAcceleratorCapacityPatch(desired, current core.ResourceList) map[string]any {
	patch := make(map[string]any)
	for name, q := range desired {
		if cur, ok := current[name]; !ok || cur.Cmp(q) != 0 {
			patch[string(name)] = q.String()
		}
	}
	for name := range current {
		if !isOwnedCapacityKey(name) {
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

// acceleratorFamilyCapacityChanged reports whether any capacity entry this reconciler reacts to
// — a bare device-plugin token pool for either family, or one of the owned counting keys — was
// added, removed, or changed between the two capacity maps.
func acceleratorFamilyCapacityChanged(oldCap, newCap core.ResourceList) bool {
	for name, q := range newCap {
		if !isAcceleratorFamilyCapacityKey(name) {
			continue
		}
		if old, ok := oldCap[name]; !ok || old.Cmp(q) != 0 {
			return true
		}
	}
	for name := range oldCap {
		if !isAcceleratorFamilyCapacityKey(name) {
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
						return acceleratorFamilyCapacityChanged(oldNd.Status.Capacity, newNd.Status.Capacity)
					},
				},
			),
		).
		Watches(
			// A Devices change moves the desired counting capacity without necessarily
			// moving the bare token pools the Node predicate watches, so enqueue the
			// name-identical node. Two kinds of change matter: a capability change on the
			// spec side (notably a partitioning-mode toggle, which re-splits the per-card
			// populations), and an occupancy change on the status side, since the
			// per-profile partition key is now derived from the runtime ledger — carving or
			// freeing a partition must re-patch the node. The 3s dedup window coalesces
			// the resulting bursts.
			&workercore.Devices{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueNodeWhenDevicesChanged,
			),
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				// A managed Devices' capacity inputs: on create/delete the whole capability
				// appears/vanishes; on update fire only when slicing detail, card health,
				// unhealthy-card live allocations, or partition profile ledgers change.
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
					return acceleratorDetailChanged(oldDevs, newDevs)
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

// acceleratorSignature captures exactly the Devices fields the desired capacity is computed
// from: the per-card logical/partitioned split, the logical detail (count + overcommit), and the
// per-profile partition instance counts. Comparing signatures lets the Devices watch fire only on
// a change that can move a capacity key, including a re-partition that changes the profile set
// without moving the card counts. It must stay comparable (it is a maps.Equal value), so the
// profiles are folded into a canonical string, not a map.
type acceleratorSignature struct {
	hasLogical        bool
	logicalUnits      int64
	logicalCores      int64
	logicalMemPct     int64
	hasPartitioned    bool
	partitionedUnits  int64
	logicalCount      int32
	logicalOvercommit bool
	partitionProfiles string
}

// partitionProfileSignature renders a group's per-profile partition instance counts as a
// canonical, order-independent "name=count;..." string, so a change to the profile set or to any
// profile's count re-enqueues the node while identical inventories compare equal.
func partitionProfileSignature(profiles map[string]int64) string {
	if len(profiles) == 0 {
		return ""
	}
	parts := make([]string, 0, len(profiles))
	for name, count := range profiles {
		parts = append(parts, name+"="+strconv.FormatInt(count, 10))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// acceleratorDetailChanged reports whether anything the desired capacity is computed from moved
// between two Devices snapshots. Unlike its predecessor it reads the status side as well as the
// spec: the per-profile partition key is derived from the runtime ledger, and unhealthy cards
// track live allocated usage rather than full capacity. Healthy-card mode/units movements still
// project to the same signature and do not fire.
func acceleratorDetailChanged(oldDevs, newDevs *workercore.Devices) bool {
	return !maps.Equal(acceleratorSignatures(oldDevs), acceleratorSignatures(newDevs))
}

// acceleratorSignatures projects a Devices object's groups to their signatures, keyed by the
// "${manufacturer}-${group ID}" acceleratable key.
func acceleratorSignatures(devs *workercore.Devices) map[string]acceleratorSignature {
	if devs == nil {
		return nil
	}
	var ledgerByGroup map[string]map[string]*workercore.AcceleratorAllocation
	out := make(map[string]acceleratorSignature, len(devs.Spec.Groups))
	for i := range devs.Spec.Groups {
		g := &devs.Spec.Groups[i]
		hasPartitionedCard, hasUnhealthyCard := false, false
		for j := range g.Accelerators {
			st := g.Accelerators[j].Status
			if device.IsPartitioned(st) {
				hasPartitionedCard = true
			}
			if st.Unhealthy {
				hasUnhealthyCard = true
			}
		}
		key := g.Manufacturer + "-" + g.ID
		var ledger map[string]*workercore.AcceleratorAllocation
		if hasPartitionedCard || hasUnhealthyCard {
			if ledgerByGroup == nil {
				ledgerByGroup = devicesLedgerByGroup(devs)
			}
			ledger = ledgerByGroup[key]
		}
		contrib := groupCapacityContribution(g, ledger, 0)
		var profiles string
		if hasPartitionedCard {
			profiles = partitionProfileSignature(partitionInstancesByProfile(g, ledger))
		}
		out[key] = acceleratorSignature{
			hasLogical:        contrib.hasLogical,
			logicalUnits:      contrib.units,
			logicalCores:      contrib.cores,
			logicalMemPct:     contrib.memoryPct,
			hasPartitioned:    contrib.hasPartitioned,
			partitionedUnits:  contrib.partitionedUnits,
			logicalCount:      g.AcceleratorSlicedDetail.Logical.Count,
			logicalOvercommit: g.AcceleratorSlicedDetail.Logical.CoresPercentageOvercommit,
			partitionProfiles: profiles,
		}
	}
	return out
}
