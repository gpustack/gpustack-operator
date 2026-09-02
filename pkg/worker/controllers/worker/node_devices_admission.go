package worker

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueadmissioncheck "sigs.k8s.io/kueue/pkg/util/admissioncheck"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

const (
	// _NodeDevicesControllerName is the AdmissionCheck controllerName this operator
	// claims; the worker's Prepare() applies an AdmissionCheck object carrying it.
	_NodeDevicesControllerName = "worker.gpustack.ai/node-devices"
	// _NodeDevicesAdmissionCheckName is the name of that AdmissionCheck object, the
	// reference InstanceTypeReconciler adds to accelerated ClusterQueues.
	_NodeDevicesAdmissionCheckName = "gpustack-node-devices"
	// _NodeDevicesFieldOwner owns the AdmissionCheckState this controller patches.
	_NodeDevicesFieldOwner = "gpustack-node-devices"
	// _NodeDevicesRetryAfterSeconds backs off a held Workload before Kueue re-admits
	// it, so a transient per-card shortage does not hot-loop.
	_NodeDevicesRetryAfterSeconds = int32(30)
)

// familyDemand is one correlated accelerator demand a Workload places on its assigned
// pool: the family, the cards it needs, the units one card must still have free to host a
// single one of them, and — for a partition request — the profile it anchors on (empty
// otherwise). For the three card-bound families each card is a distinct physical card; for
// the partition family a card is one instance, since a partition request is one card per
// Pod, and several of them can share a card.
//
// The three quantities are read from the SAME podset. Pairing a per-card demand taken as a
// maximum over every podset with a card count summed over every podset would gate a small
// request on a large one's budget, which is exactly what the single per-Pod tuple this
// replaces did — and its mode was overwritten by whichever key was scanned last.
type familyDemand struct {
	family       nodefeature.ResourceFamily
	cards        int32
	unitsPerCard int32
	// profile is the requested partition profile in the published spelling its resource key
	// carries — the spelling every verdict message quotes back to the user. A card's own
	// ledger is keyed by its manufacturer's spelling, so the feasibility check converts per
	// card rather than carrying a second copy here.
	profile string
	// flavor is the ResourceFlavor Kueue assigned to the podset this demand came from. It scopes
	// the demand to the cards THAT flavor covers, which a multi-podset Workload needs and a
	// single-podset one is unaffected by: Kueue assigns a flavor per podset, so a Workload whose
	// roles want different accelerator models carries two, and a demand fitted against the union
	// of both would be satisfied by cards its own role can never be placed on.
	flavor kueue.ResourceFlavorReference
	// podSets names the podsets this demand was read from — several once identical demands merge.
	// It is carried for the verdict message only, never for matching: a group-wide "not enough
	// cards" that does not say which role fell short is a message nobody can act on.
	podSets []kueue.PodSetReference
}

// parseFamilyDemands reads the accelerator demands off a Workload's pod templates, one
// correlated tuple per (podset, family), merged across podsets only where the assigned flavor,
// the per-card demand and the profile agree — so the common single-podset Workload yields
// exactly one tuple per family. A Workload requesting no known accelerator yields none.
func parseFamilyDemands(wl *kueue.Workload) []familyDemand {
	var demands []familyDemand
	for i := range wl.Spec.PodSets {
		ps := &wl.Spec.PodSets[i]
		flavor := assignedFlavor(wl, ps.Name)
		for _, d := range podSetFamilyDemands(ps) {
			d.flavor = flavor
			d.podSets = []kueue.PodSetReference{ps.Name}
			demands = mergeDemand(demands, d)
		}
	}
	sortDemands(demands)
	return demands
}

// assignedFlavor returns the ResourceFlavor Kueue assigned to one podset for its ACCELERATOR, empty
// when the Workload holds no admission yet, names no assignment for the podset, or that assignment
// carries no accelerator flavor — or carries several, which is no more decidable than none. Empty
// is not "any flavor": it matches only a card population that is itself unscoped, so a demand whose
// assignment cannot be read is fitted against nothing and the check holds with a verdict that says
// so, rather than guessing.
//
// The assignment maps one flavor per COVERED RESOURCE, and the demand this controller gates is
// always for an accelerator — so the entry to read is the one keyed by the manufacturer's credits
// resource, which is what Kueue accounted after the chart's transformation replaced the raw
// accelerator keys. A ClusterQueue this operator builds covers exactly one resource, so ordinary
// Workloads carry a single entry; reading it by resource key rather than by name is what keeps the
// choice right when they do not.
//
// A queue covering more is reachable by an admin who writes one and references this check from it,
// and it has two shapes. A CPU flavor assigned beside the accelerator one pins no accelerator key,
// so it covers no card: falling back to it would hold the Workload while reporting a capacity
// shortage. Two MANUFACTURERS' credits both covered is the harder one — a demand merges the bases of
// one family, so its card count spans two models, and a flavor pins ONE accelerator key, so no
// flavor's cards can answer for both. Counting a second accelerator credits resource is therefore
// enough to refuse: whether the two resources name two flavors or one makes no difference, and the
// one-flavor case is the dangerous one, since a single reference reads as unambiguous while its
// cards are a single model's. Both return empty, which holds the demand before any card is listed
// with the assignment named as the cause. It is the same answer flavorAcceleratorKey gives an
// ambiguous flavor, for the same reason: a population chosen arbitrarily is the one answer that can
// be wrong in the direction that admits a workload.
func assignedFlavor(wl *kueue.Workload, podSet kueue.PodSetReference) kueue.ResourceFlavorReference {
	if wl.Status.Admission == nil {
		return ""
	}
	for i := range wl.Status.Admission.PodSetAssignments {
		psa := &wl.Status.Admission.PodSetAssignments[i]
		if psa.Name != podSet {
			continue
		}
		var credits kueue.ResourceFlavorReference
		accelerators := 0
		for res, ref := range psa.Flavors {
			if !nodefeature.IsAcceleratableCreditsResourceName(res) {
				continue
			}
			accelerators++
			credits = ref
		}
		// Exactly one, or nothing. Two manufacturers' credits mean the demand merged two models'
		// cards, and no ONE flavor can answer for both: a flavor pins a single accelerator key, so
		// its cards are one model's. That holds whether the two resources name different flavors or
		// the SAME one — a resource group may cover both and quote one flavor for each, and reading
		// that single reference as unambiguous would scope the merged count to one model while
		// counting the other's.
		if accelerators != 1 {
			return ""
		}
		return credits
	}
	return ""
}

// podSetFamilyDemands reads one podset's per-family demand. Each family contributes
// Count × (cards summed across the podset's containers); the card key (<base> exclusive,
// <base>.shared, <base>.sliced, <base>.partitioned) adds to the count, the counting key
// (<base>.sliced.units, <base>.partitioned.units) carries the per-card units, and the
// per-profile partition key names the profile — its own value is per-card, so it does not
// add to the count. Init containers are scanned as well as app containers, so an init-only
// request is gated too. The percentage and MiB sub-keys are ignored: the Pod webhook
// already folded them into the counting key.
//
// One tuple per family holds even when a podset's containers name two manufacturers' bases —
// the Pod webhook forbids two families, not two bases — so a merged tuple sums the cards of two
// models while naming one family. Through a queue this operator builds it cannot be reached: each
// manufacturer's keys transform onto that manufacturer's own credits resource, an accelerated
// ClusterQueue covers exactly one of them, and a request no flavor can be assigned for leaves the
// Workload unreserved. An admin's queue covering both manufacturers' credits does reach it, and
// there the merged tuple carries no flavor at all — assignedFlavor refuses to choose between the
// two, so the demand is held rather than fitted against one model's cards while counting both.
func podSetFamilyDemands(ps *kueue.PodSet) []familyDemand {
	byFamily := make(map[nodefeature.ResourceFamily]*familyDemand)
	containers := make([]*core.Container, 0, len(ps.Template.Spec.InitContainers)+len(ps.Template.Spec.Containers))
	for ci := range ps.Template.Spec.InitContainers {
		containers = append(containers, &ps.Template.Spec.InitContainers[ci])
	}
	for ci := range ps.Template.Spec.Containers {
		containers = append(containers, &ps.Template.Spec.Containers[ci])
	}

	for _, ctr := range containers {
		for name, qty := range mergedContainerResources(ctr) {
			family := nodefeature.ResourceFamilyOf(name)
			switch family {
			case nodefeature.ResourceFamilyExclusive, nodefeature.ResourceFamilyShared,
				nodefeature.ResourceFamilySliced, nodefeature.ResourceFamilyPartitioned:
			default:
				continue
			}
			d := byFamily[family]
			if d == nil {
				d = &familyDemand{family: family}
				byFamily[family] = d
			}
			switch {
			case strings.HasSuffix(string(name), nodefeature.SlicedUnitsResourceNameSuffix),
				strings.HasSuffix(string(name), nodefeature.PartitionedUnitsResourceNameSuffix):
				// Keep the strictest per-card demand across this podset's containers, so
				// feasibility is never checked against an undersized slice.
				if u := clampInt32(qty.Value()); u > d.unitsPerCard {
					d.unitsPerCard = u
				}
			case isCardKey(name, family):
				d.cards += clampInt32(qty.Value())
			default:
				// One profile per Pod is a request rule, but the Pod webhook enforces it on a
				// Pod and Kueue builds this Workload from a pod TEMPLATE before any Pod exists —
				// so a template naming two profiles reaches here. Resource maps iterate in random
				// order, so take the smallest name rather than the last one seen: the shape is
				// refused at Pod creation either way, and this keeps the verdict message stable
				// across reconciles instead of flipping between the two.
				if profile, ok := nodefeature.PartitionedProfileOf(name); ok {
					if d.profile == "" || profile < d.profile {
						d.profile = profile
					}
				}
			}
		}
	}

	out := make([]familyDemand, 0, len(byFamily))
	for _, d := range byFamily {
		// Scale to the podset's pod count in int64 and clamp, so a crafted per-container
		// count times a large pod count cannot wrap to a small (or negative) demand. A
		// sub-key with no card request behind it, and a podset of zero pods, both demand
		// no card at all.
		d.cards = clampInt32(int64(d.cards) * int64(ps.Count))
		if d.cards <= 0 {
			continue
		}
		out = append(out, *d)
	}
	sortDemands(out)
	return out
}

// isCardKey reports whether name is the family's card key — the one whose value is the
// card count. Every other key of a family is a per-card budget or a profile anchor.
func isCardKey(name core.ResourceName, family nodefeature.ResourceFamily) bool {
	switch family {
	case nodefeature.ResourceFamilyExclusive:
		return true
	case nodefeature.ResourceFamilyShared:
		return strings.HasSuffix(string(name), nodefeature.SharedResourceNameSuffix)
	case nodefeature.ResourceFamilySliced:
		return strings.HasSuffix(string(name), nodefeature.SlicedResourceNameSuffix)
	case nodefeature.ResourceFamilyPartitioned:
		return strings.HasSuffix(string(name), nodefeature.PartitionedResourceNameSuffix)
	}
	return false
}

// mergedContainerResources merges a container's Limits and Requests, requests taking
// precedence, so each resource is read once. Extended resources (accelerator keys) are
// commonly set only under limits, so a requests-only scan would miss the request and
// wrongly mark feasibility Ready.
func mergedContainerResources(ctr *core.Container) core.ResourceList {
	if len(ctr.Resources.Limits) == 0 {
		return ctr.Resources.Requests
	}
	merged := make(core.ResourceList, len(ctr.Resources.Requests)+len(ctr.Resources.Limits))
	for name, qty := range ctr.Resources.Limits {
		merged[name] = qty
	}
	for name, qty := range ctr.Resources.Requests {
		merged[name] = qty
	}
	return merged
}

// mergeDemand folds a podset's demand into the accumulated list, summing the cards of an
// existing entry that shares its shape and appending a new entry otherwise. Two podsets
// demanding different per-card budgets stay separate, so neither is checked against the
// other's.
//
// The assigned flavor is part of that shape. Two podsets on ONE flavor genuinely compete for the
// same cards, so merging them is what keeps the shared budget honest; two on DIFFERENT flavors
// must be satisfied from disjoint card populations, and summing them would let a role's cards
// cover another role's demand — the check reporting Ready on a pool that cannot place the request.
func mergeDemand(demands []familyDemand, d familyDemand) []familyDemand {
	for i := range demands {
		if demands[i].family == d.family &&
			demands[i].unitsPerCard == d.unitsPerCard &&
			demands[i].profile == d.profile &&
			demands[i].flavor == d.flavor {
			demands[i].cards = clampInt32(int64(demands[i].cards) + int64(d.cards))
			demands[i].podSets = append(demands[i].podSets, d.podSets...)
			return demands
		}
	}
	return append(demands, d)
}

// sortDemands orders demands most constrained first — a partition request before a scalar
// one, then by descending per-card units — so the greedy fit in nodeDevicesFeasibility
// never spends a card a tighter demand needed, and so the verdict is deterministic.
func sortDemands(demands []familyDemand) {
	slices.SortStableFunc(demands, func(a, b familyDemand) int {
		partitionedA := a.family == nodefeature.ResourceFamilyPartitioned
		partitionedB := b.family == nodefeature.ResourceFamilyPartitioned
		if c := slicex.CompareTrueFirst(partitionedA, partitionedB); c != 0 {
			return c
		}
		if a.unitsPerCard != b.unitsPerCard {
			return cmp.Compare(b.unitsPerCard, a.unitsPerCard)
		}
		return cmp.Compare(a.family, b.family)
	})
}

// clampInt32 narrows a resource quantity value to the int32 the ledger uses,
// saturating at the bounds instead of wrapping, so an oversized (or crafted)
// request can never wrap around to a small or negative demand.
func clampInt32(v int64) int32 {
	switch {
	case v > math.MaxInt32:
		return math.MaxInt32
	case v < 0:
		return 0
	default:
		return int32(v)
	}
}

// unitsPerCardFor returns the allocatable units one card must still have free to host a
// single card of a scalar demand: a whole card for exclusive, one owner's share for shared,
// and the requested per-card units for a logical slice. A logical slice the Pod webhook did
// not shape carries no budget, so any card with room fits.
func unitsPerCardFor(d familyDemand) int32 {
	switch d.family {
	case nodefeature.ResourceFamilyShared:
		return nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize
	case nodefeature.ResourceFamilySliced:
		return d.unitsPerCard
	default:
		return nodefeature.ResourceMaxUnits
	}
}

// cardLedger is a per-card view joining the Spec-side capability (which families the card
// can serve, and the physical partition profiles with their cached placements) with the
// Status-side allocation (mode, scalar remaining, and the per-profile remaining ledger),
// matched by accelerator ID.
type cardLedger struct {
	// manufacturer is the card's own manufacturer, carried because the per-profile ledger
	// below is keyed by that manufacturer's spelling of a profile name while a demand names
	// the published one.
	manufacturer string
	// flavors are the assigned ResourceFlavors that cover this card — those whose selector
	// matched its node AND whose nodeLabels pin its own accelerator key. A card a mixed-model
	// node contributes appears once with the flavors that actually cover it, so the per-card
	// budget below is still spent once no matter how many flavors reach it.
	flavors           sets.Set[kueue.ResourceFlavorReference]
	capability        workercore.AcceleratorStatus
	mode              workercore.DeviceAllocationMode
	remaining         int32
	remainingProfiles []workercore.AcceleratorProfileCount
}

// coveredBy reports whether this card may serve a demand assigned the given flavor. Matching is
// plain equality on the reference, in both directions: a production demand carries a real flavor
// name and is covered only by cards that flavor reaches, while a demand whose assignment could not
// be read carries none and is covered by nothing — so an unreadable assignment holds the workload
// with Retry instead of silently widening it to every card in the pool.
func (c cardLedger) coveredBy(flavor kueue.ResourceFlavorReference) bool {
	return c.flavors.Has(flavor)
}

// flavorScope is one assigned ResourceFlavor's claim on a node: the flavor itself, and the
// accelerator device key ("<manufacturer>-<id>") its nodeLabels pin. Both halves decide which of
// the node's cards it covers — the key alone would pool two flavors of one model that differ by
// node batch, and the node alone would pool the other models a mixed-model node also carries.
type flavorScope struct {
	flavor         kueue.ResourceFlavorReference
	acceleratorKey string
	// acceleratorCount is the per-node card count of that model the flavor pins through its
	// "<key>.count" nodeLabel. It is part of the flavor's identity — the name encodes it, and Kueue
	// admits a Pod onto that batch only — but the Devices selector cannot carry it, because the
	// DeviceManager deliberately omits ".count" from a Devices object's labels. So the batch is
	// dropped from the LIST and re-applied per card group here: without it, the 4-device and the
	// 8-device flavor of one model claim each other's nodes, and free cards on a 4-device node make
	// an 8-device-flavor demand Ready while its Pods stay Pending. Zero means the flavor pins no
	// batch, which covers every count; a negative one means it pins a batch that cannot be read,
	// which covers none (see flavorAcceleratorCount).
	acceleratorCount int
}

// scopedDevices is one node's Devices ledger with the assigned flavors whose selector matched that
// node. A node backs several flavors when it carries several accelerator models, so the scopes are
// a list and which cards each one covers is settled per card by the accelerator key.
type scopedDevices struct {
	devices workercore.Devices
	scopes  []flavorScope
}

// collectCards flattens every accelerator across the candidate nodes into one list, joining each
// Status allocation with its Spec capability by accelerator ID, and stamping each card with the
// assigned flavors that cover it.
//
// One flat list with per-card coverage, rather than one list per flavor, because the budget must
// stay global: two roles assigned the same flavor compete for the same cards, and a card a
// mixed-model node contributes to two flavors must not be spent twice.
func collectCards(pool []scopedDevices) []cardLedger {
	var cards []cardLedger
	for i := range pool {
		d := &pool[i].devices
		capByID := make(map[string]workercore.AcceleratorStatus)
		for gi := range d.Spec.Groups {
			accs := d.Spec.Groups[gi].Accelerators
			for ai := range accs {
				capByID[accs[ai].ID] = accs[ai].Status
			}
		}
		for gi := range d.Status.Groups {
			g := &d.Status.Groups[gi]
			aKey := acceleratorKeyOf(g.Manufacturer, g.ID)
			// The flavors covering every card of this group: those that matched the node, whose
			// own accelerator key is this group's model, AND whose pinned node batch is this
			// group's card count — the three together are what the flavor's identity means.
			covering := sets.New[kueue.ResourceFlavorReference]()
			for _, s := range pool[i].scopes {
				if s.acceleratorKey == aKey &&
					(s.acceleratorCount == 0 || s.acceleratorCount == len(g.Accelerators)) {
					covering.Insert(s.flavor)
				}
			}
			for ai := range g.Accelerators {
				acc := &g.Accelerators[ai]
				cards = append(cards, cardLedger{
					manufacturer:      g.Manufacturer,
					flavors:           covering,
					capability:        capByID[acc.ID],
					mode:              acc.Mode,
					remaining:         acc.Remaining,
					remainingProfiles: acc.RemainingProfiles,
				})
			}
		}
	}
	return cards
}

// _unreadableAcceleratorBatch is the batch of a flavor that states one this operator cannot read.
// It is negative, so no card group's length can equal it and such a flavor covers no card.
const _unreadableAcceleratorBatch = -1

// flavorAcceleratorCount reads the per-node card count a flavor pins for its own accelerator key —
// the "<key>.count" nodeLabel NodeFlavorReconciler stamps alongside the bare key. Zero means "any
// batch", and is returned when the flavor pins no key at all or the label is ABSENT: a flavor that
// does not state a batch must not be narrowed to none.
//
// A label that is present but is not a positive count is the opposite case and fails closed. Kueue
// still admits Pods only onto nodes carrying that exact label value, and no node can carry an
// unreadable one — so widening such a flavor to "any batch" here would let free cards from another
// batch report Ready while its Pods stay Pending forever. The batch is the one part of a flavor's
// identity the Devices selector cannot carry, so an unreadable one cannot be checked at all, and
// the check holds the Workload instead of guessing.
func flavorAcceleratorCount(rf *kueue.ResourceFlavor, acceleratorKey string) int {
	if acceleratorKey == "" {
		return 0
	}
	raw, pinned := rf.Spec.NodeLabels[nodefeature.AcceleratableFeatureLabelPrefix+acceleratorKey+
		_ResourceFlavorCountLabelSuffix]
	if !pinned {
		return 0
	}
	count, err := strconvx.Atoi[int](raw)
	if err != nil || count <= 0 {
		return _unreadableAcceleratorBatch
	}
	return count
}

// acceleratorKeyOf builds a device group's accelerator key the same way the node labels do —
// "<manufacturer>-<group id>", the key a ResourceFlavor pins as
// "acceleratable.feature.gpustack.ai/<key>". Deriving it here rather than reading it back off a
// label keeps the card side independent of whether the node's labels are current.
func acceleratorKeyOf(manufacturer, groupID string) string {
	return manufacturer + "-" + groupID
}

// servesFamily reports whether the card's reported capability can serve the family at all.
// Logical slicing and hardware partitioning are exclusive card states, and a partitioned
// card is no longer available as a whole card — so a card that cannot serve a family is
// excluded from that family's population here exactly as it is from the device plugin's
// token pool. Without it an exclusive tenant's Pod would be judged feasible against a
// partitioned card, admitted, and left Pending forever.
func (c cardLedger) servesFamily(family nodefeature.ResourceFamily) bool {
	switch family {
	case nodefeature.ResourceFamilyPartitioned:
		return device.IsPartitioned(c.capability)
	case nodefeature.ResourceFamilySliced:
		return device.IsLogicallySliceable(c.capability)
	default:
		return device.IsWholeAcceleratorCapable(c.capability)
	}
}

// cardBudget records what a Workload's already-checked demands claimed from one card, so a
// later demand cannot spend the same room twice. A scalar demand takes the whole card; a
// partition demand takes one of the several placements a card may host. The two never
// contend for the same card — the populations are disjoint by capability — so a single
// budget per card is enough.
type cardBudget struct {
	whole      bool
	placements int32
}

// nodeDevicesFeasibility reports whether every demand can currently be placed across the
// candidate devices at the same time, returning the check state and the message explaining
// it. The devices are every node the Workload's assigned flavors reach, NOT one flavor's
// pool: each card records which of those flavors cover it, and each demand is fitted only
// against the cards of the flavor its own podset was assigned. Room is consumed as it is
// matched, so two demands of one Workload never both claim it. The reported message is the
// deciding demand's: the first that does not fit, or — when everything fits — the last one
// checked, which for the single-demand Workload is simply its own verdict. A shortage is
// always transient — Retry, never Reject.
//
// Each demand's own provenance decides whether the verdict names a role; whether a Workload's
// shape makes that worth saying is the caller's call, not this function's.
//
// The fit is greedy: each demand takes the first cards it may use, and never gives one back to let
// a later demand fit. That is exact while the demands' populations are disjoint or nested the way
// the flavors this operator builds make them — a card carries one model, and one model's flavors
// differ by node batch, so two flavors of one Workload do not reach the same card. Where an admin's
// own flavor selectors overlap asymmetrically, a demand covering many cards can take the one card a
// narrower demand also needed and the narrower one is then held, though a different order would
// have placed both. The remedy would be a matching over demands and cards; the reason it is not
// here is the direction of the error, which the greedy order cannot change: a demand only ever
// takes cards it may itself use, so this reports a shortage that is not there — it never reports
// room that is not there.
func nodeDevicesFeasibility(pool []scopedDevices, demands []familyDemand) (kueue.CheckState, string) {
	message := verdictMessage(kueue.CheckStateReady, nil)
	if len(demands) == 0 {
		return kueue.CheckStateReady, message
	}

	cards := collectCards(pool)
	budgets := make([]cardBudget, len(cards))
	for _, d := range demands {
		var state kueue.CheckState
		if d.family == nodefeature.ResourceFamilyPartitioned {
			state, message = fitPartitionDemand(cards, budgets, d)
		} else {
			state, message = fitScalarDemand(cards, budgets, d)
		}
		if state != kueue.CheckStateReady {
			return state, message
		}
	}
	return kueue.CheckStateReady, message
}

// withoutRoles strips the provenance from every demand, so every verdict about them reads exactly
// as it did before roles existed. Returned as a copy: the caller's demands are its own.
func withoutRoles(demands []familyDemand) []familyDemand {
	bare := make([]familyDemand, len(demands))
	for i, d := range demands {
		d.podSets = nil
		bare[i] = d
	}
	return bare
}

// fitScalarDemand gates an exclusive/shared/logical-slice demand on the scalar per-card
// remaining ledger, which seeds every card at ResourceMaxUnits and subtracts each pod's
// allocation, so a card carrying any allocation has Remaining below a whole card and never
// satisfies an exclusive demand. Its cards count is a card count: each of them needs its
// own card. Ready once enough cards of the family's population fit, otherwise Retry.
func fitScalarDemand(cards []cardLedger, budgets []cardBudget, d familyDemand) (kueue.CheckState, string) {
	units := unitsPerCardFor(d)
	var fit int32
	for i := range cards {
		if budgets[i].whole || !cards[i].coveredBy(d.flavor) ||
			!cards[i].servesFamily(d.family) || cards[i].remaining < units {
			continue
		}
		budgets[i].whole = true
		if fit++; fit >= d.cards {
			return kueue.CheckStateReady, verdictMessage(kueue.CheckStateReady, d.podSets)
		}
	}
	return kueue.CheckStateRetry, verdictMessage(kueue.CheckStateRetry, d.podSets)
}

// fitPartitionDemand gates a partition demand on the per-card placement-aware ledger: a
// candidate card is physically partitioned, is not held whole-card (Mode None or
// Partitioned), and still has free placements for the profile. Its cards count is an
// INSTANCE count — a partition request is one card per Pod (rule 3), so a replicated
// Workload asks for that many instances — and one card can host several of them, which is
// exactly what RemainingProfiles reports and what the placement-authoritative Allocate will
// do. Counting one instance per card would leave every replica after the first in Retry
// forever on a node that has room for them all.
//
// A pool with no partitioned card at all, and a pool whose partitioned cards have not yet
// published cached Placements (rollout skew), each get their own distinct Retry message
// (separate from "the profile is momentarily full") so an operator can tell the three apart.
// Never Reject.
func fitPartitionDemand(cards []cardLedger, budgets []cardBudget, d familyDemand) (kueue.CheckState, string) {
	var partitionCards, ledgerReady, fit int32
	for i := range cards {
		c := &cards[i]
		if !c.coveredBy(d.flavor) || !c.servesFamily(nodefeature.ResourceFamilyPartitioned) {
			continue
		}
		partitionCards++
		if partitionProfilesHavePlacements(c.capability.PhysicalSliced.Profiles) {
			ledgerReady++
		}
		if c.mode != workercore.DeviceAllocationModeNone && c.mode != workercore.DeviceAllocationModePartitioned {
			continue // held whole-card by an exclusive/shared allocation
		}
		// Every profile is charged one placement, whatever its size: the ledger reports
		// remaining per profile, and profiles of one card overlap physically, so a finer
		// account would need the placement intervals the plugin resolves at Allocate time.
		// The demand names the profile as the published key spells it; this card's ledger is
		// keyed as its own manufacturer spells it.
		vendorProfile := nodefeature.VendorPartitionedProfileName(c.manufacturer, d.profile)
		free := remainingProfileCount(c.remainingProfiles, vendorProfile) - budgets[i].placements
		if free <= 0 {
			continue
		}
		take := min(free, d.cards-fit)
		budgets[i].placements += take
		if fit += take; fit >= d.cards {
			return kueue.CheckStateReady, partitionVerdictMessage(kueue.CheckStateReady, d.profile, d.podSets)
		}
	}
	switch {
	case partitionCards == 0:
		return kueue.CheckStateRetry, partitionNoCardsMessage(d.profile, d.podSets)
	case ledgerReady == 0:
		return kueue.CheckStateRetry, partitionLedgerNotReadyMessage(d.podSets)
	}
	return kueue.CheckStateRetry, partitionVerdictMessage(kueue.CheckStateRetry, d.profile, d.podSets)
}

// partitionProfilesHavePlacements reports whether any capability profile carries a cached
// placement set — the signal that the device manager has published a partition placement ledger.
func partitionProfilesHavePlacements(profiles []workercore.AcceleratorPhysicalSlicedProfile) bool {
	for i := range profiles {
		if len(profiles[i].Placements) > 0 {
			return true
		}
	}
	return false
}

// remainingProfileCount returns how many more instances of profile the card can still build,
// from its RemainingProfiles ledger (zero when the profile is absent). The ledger is keyed by
// the manufacturer's own profile spelling, so profile must be that spelling — a published name
// would silently miss every row and read as a full card.
func remainingProfileCount(profiles []workercore.AcceleratorProfileCount, profile string) int32 {
	for i := range profiles {
		if profiles[i].Name == profile {
			return profiles[i].Count
		}
	}
	return 0
}

// NodeDevicesAdmissionReconciler is the external Kueue AdmissionCheck controller
// for _NodeDevicesControllerName: after Kueue reserves quota it reads the per-card
// Devices ledger of the assigned flavor's pool and reports Ready when the request
// can be placed, or Retry when no node currently has enough free cards (the credits
// gate admits on a scalar total and cannot see whole-card availability). It is
// check-only: it never preempts and never rejects.
type NodeDevicesAdmissionReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

var _ ctrlreconcile.Reconciler = (*NodeDevicesAdmissionReconciler)(nil)

func (r *NodeDevicesAdmissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	wl := new(kueue.Workload)
	if err := r.Client.Get(ctx, req.NamespacedName, wl); err != nil {
		logger.Error(err, "fetch workload")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Evaluate only a live, unfinished Workload that already holds a quota
	// reservation; before reservation there is nothing to confirm, after eviction
	// or finish the verdict is moot. Eviction needs its own test because Kueue
	// resets the checks to Pending and drops the reservation in two separate
	// writes, so in between an evicted Workload still reports one. Overwriting that
	// reset with a fresh verdict wedges the Workload for good: Kueue stops resetting
	// checks while the eviction condition is set, its scheduler refuses to reserve
	// quota while a check is Retry, and without a reservation this controller stops
	// evaluating. Re-reserving quota clears the eviction condition, which is what
	// re-opens evaluation.
	if !kueueworkload.HasQuotaReservation(wl) || kueueworkload.IsEvicted(wl) ||
		kueueworkload.IsFinished(wl) || !kueueworkload.IsActive(wl) {
		logger.V(3).Info("skip workload not holding quota reservation, evicted, finished or deactivated")
		return ctrl.Result{}, nil
	}

	// Once the Workload is admitted its placement is settled and its own device
	// allocation is already subtracted from the per-card ledger. Re-checking would count
	// that allocation against itself — a slice larger than half a card leaves the card
	// with Remaining below the demand it just satisfied — and flip the check to Retry,
	// evicting a healthy running Workload in a recreate loop. The gate only needs to hold
	// before admission (it holds Retry until a card frees); after admission there is
	// nothing left to gate, so leave the settled verdict untouched.
	if kueueworkload.IsAdmitted(wl) {
		logger.V(3).Info("skip already-admitted workload; placement settled")
		return ctrl.Result{}, nil
	}

	// Limit to the checks this controller owns.
	checks, err := kueueadmissioncheck.FilterForController(ctx, r.Client, wl.Status.AdmissionChecks, _NodeDevicesControllerName)
	if err != nil {
		logger.Error(err, "filter admission checks for controller")
		return ctrl.Result{}, err
	}
	if len(checks) == 0 {
		return ctrl.Result{}, nil
	}

	demands := parseFamilyDemands(wl)

	// The role clause on a verdict exists to say WHICH role it is about, which is worth saying only
	// when the Workload has more than one podset to tell apart. That is read off the Workload's own
	// shape rather than off the demands, because a Workload whose accelerator role sits beside a
	// CPU-only one parses to a single demand and still needs that role named. A genuinely
	// single-podset Workload — every Workload rendered from a plain Pod — has nothing to
	// disambiguate, so its verdicts read exactly as they did before roles existed rather than
	// gaining a noise clause naming Kueue's default podset.
	if len(wl.Spec.PodSets) <= 1 {
		demands = withoutRoles(demands)
	}

	// A demand whose podset has no assigned flavor is held here, explicitly, before any card is
	// listed. The fit would hold it anyway — an unnamed flavor is covered by no card — but that
	// answer is arrived at indirectly, and this state is severe enough that its reason must be
	// stated rather than inferred: every accelerator workload of the pool would sit in Retry with
	// nothing in the verdict pointing at the cause. Kueue's own SetQuotaReservation dereferences
	// the admission it stores, so it cannot produce a reserved Workload without one; reaching this
	// means something outside Kueue wrote the status, or Kueue's own contract moved. Either way the
	// operator reads why on the Workload instead of seeing an unexplained pool-wide stall.
	if unassigned := unassignedDemands(demands); len(unassigned) > 0 {
		roles := rolesOf(unassigned)
		logger.Info("holding workload whose podset carries no assigned flavor",
			"roles", roles)
		if err := r.applyVerdict(ctx, wl, checks, kueue.CheckStateRetry, unassignedFlavorMessage(roles)); err != nil {
			logger.Error(err, "patch admission check state")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	devices, err := r.candidateDevices(ctx, wl)
	if err != nil {
		logger.Error(err, "list candidate devices")
		return ctrl.Result{}, err
	}

	// A demand whose flavor reaches nodes of the pool but resolves to no card population there is
	// held with that as the reason. The fit holds it either way — a keyless scope covers no card —
	// but what an operator would read is "no node has enough free cards" about a pool whose cards
	// are free, and no amount of waiting clears it: the flavor's own labels are what have to change.
	// It is the same false negative the missing-assignment hold above exists to prevent, one step
	// later, because this cause is only visible once the flavors have been read.
	if unresolved := unresolvedDemands(devices, demands); len(unresolved) > 0 {
		roles := rolesOf(unresolved)
		logger.Info("holding workload whose assigned flavor resolves to no card population",
			"roles", roles)
		if err := r.applyVerdict(ctx, wl, checks, kueue.CheckStateRetry, unresolvedFlavorMessage(roles)); err != nil {
			logger.Error(err, "patch admission check state")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	state, message := nodeDevicesFeasibility(devices, demands)

	if err := r.applyVerdict(ctx, wl, checks, state, message); err != nil {
		logger.Error(err, "patch admission check state")
		return ctrl.Result{}, err
	}
	// demandsSummary formats eagerly, so gate it: this runs on every Workload reconcile.
	if logger.V(2).Enabled() {
		logger.V(2).Info("evaluated node-devices admission",
			"state", state, "demands", demandsSummary(demands))
	}
	return ctrl.Result{}, nil
}

// unassignedDemands returns the demands whose podset resolves to no single accelerator flavor.
// There is one cause with five shapes — the Workload holds no admission, the admission names no
// assignment for the podset, that assignment carries no flavor, it carries only flavors for other
// resources, or it carries several accelerator flavors and none of them is THE one — and all five
// leave the demand unplaceable, so they are answered together rather than told apart.
//
// A Workload demanding no accelerator yields none, so the ordinary CPU-only case is untouched. The
// demands are returned rather than their role names because the hold must fire for a single-podset
// Workload too, and that one's demands carry no name to be found by.
func unassignedDemands(demands []familyDemand) []familyDemand {
	var unassigned []familyDemand
	for _, d := range demands {
		if d.flavor == "" {
			unassigned = append(unassigned, d)
		}
	}
	return unassigned
}

// unresolvedDemands returns the demands whose assigned flavor reaches nodes of the candidate pool
// but resolves to no card population on them — the flavor pins no accelerator key, or several, so
// which model its cards are is not decidable. An admin's flavor is the only way to reach either.
//
// "Reaches nodes" is the qualifier that keeps this apart from a plain shortage: a flavor whose
// selector matches nothing contributes no scope, and there a capacity verdict is the right one —
// the pool really is empty of cards. What is reported here is a flavor whose nodes ARE in the pool
// while none of its scopes can say which cards on them are its.
func unresolvedDemands(pool []scopedDevices, demands []familyDemand) []familyDemand {
	reached := sets.New[kueue.ResourceFlavorReference]()
	resolved := sets.New[kueue.ResourceFlavorReference]()
	for i := range pool {
		for _, s := range pool[i].scopes {
			reached.Insert(s.flavor)
			if s.acceleratorKey != "" {
				resolved.Insert(s.flavor)
			}
		}
	}

	var unresolved []familyDemand
	for _, d := range demands {
		if d.flavor != "" && reached.Has(d.flavor) && !resolved.Has(d.flavor) {
			unresolved = append(unresolved, d)
		}
	}
	return unresolved
}

// unresolvedFlavorMessage explains a hold that free capacity can never clear, so it points at the
// flavor rather than at the pool: the cards are there, and nothing in the workload or the cluster's
// utilization is what stops them from being counted.
func unresolvedFlavorMessage(roles []kueue.PodSetReference) string {
	return "the ResourceFlavor assigned" + forRoles(roles) +
		" pins no single accelerator key, so which cards of its nodes are its own cannot be" +
		" resolved; correct the flavor's node labels (will retry)"
}

// rolesOf returns the podsets the given demands were read from, sorted and deduplicated. It is
// empty when they carry no provenance, which is how a single-podset Workload's verdict reads as it
// did before roles existed.
func rolesOf(demands []familyDemand) []kueue.PodSetReference {
	roles := sets.New[kueue.PodSetReference]()
	for _, d := range demands {
		roles.Insert(d.podSets...)
	}
	if roles.Len() == 0 {
		return nil
	}
	return sets.List(roles)
}

// unassignedFlavorMessage explains a hold that no amount of free capacity can clear, so it says
// what is missing rather than what is full: the verdict an operator would otherwise read is "no
// node has enough free cards" on a pool that is empty of nothing.
func unassignedFlavorMessage(roles []kueue.PodSetReference) string {
	return "the Workload's admission does not resolve to a single accelerator flavor" + forRoles(roles) +
		", so no card population can be resolved; will retry once it does"
}

// demandsSummary renders the parsed demands for the controller log; the familyDemand
// fields are unexported, so the default struct rendering would print nothing useful.
func demandsSummary(demands []familyDemand) string {
	parts := make([]string, 0, len(demands))
	for _, d := range demands {
		part := fmt.Sprintf("%s cards=%d", d.family, d.cards)
		if d.profile != "" {
			part += " profile=" + d.profile
		}
		if d.unitsPerCard > 0 {
			part += fmt.Sprintf(" units=%d", d.unitsPerCard)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// candidateDevices gathers the Devices ledgers of every node backing the flavors
// Kueue assigned to the Workload, located by the flavor's nodeLabels (the same
// labels the DeviceManager stamps onto each Devices object). A non-accelerator
// flavor matches no Devices, so listing all assigned flavors is safe. The list is
// read uncached: the worker manager does not watch Devices.
func (r *NodeDevicesAdmissionReconciler) candidateDevices(ctx context.Context, wl *kueue.Workload) ([]scopedDevices, error) {
	if wl.Status.Admission == nil {
		return nil, nil
	}
	flavorRefs := sets.New[kueue.ResourceFlavorReference]()
	for i := range wl.Status.Admission.PodSetAssignments {
		for _, ref := range wl.Status.Admission.PodSetAssignments[i].Flavors {
			flavorRefs.Insert(ref)
		}
	}

	// One entry per node, carrying every assigned flavor whose selector matched it: a node with
	// two accelerator models backs one flavor per model, and its cards must be listed once so the
	// per-card budget cannot be spent twice.
	byName := make(map[string]*scopedDevices)
	var order []string
	for ref := range flavorRefs {
		rf := new(kueue.ResourceFlavor)
		if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: string(ref)}, rf); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		// An empty selector would match every Devices object; skip it.
		if len(rf.Spec.NodeLabels) == 0 {
			continue
		}
		aKey := flavorAcceleratorKey(rf)
		scope := flavorScope{
			flavor:           ref,
			acceleratorKey:   aKey,
			acceleratorCount: flavorAcceleratorCount(rf, aKey),
		}
		// The flavor's nodeLabels carry a ".count" node-batch pin (for Kueue scheduling) that the
		// DeviceManager deliberately omits from a Devices object's selector labels; drop it so the
		// List matches — the real CPU key, the acceleratable key, os/arch and managed remain, and
		// those are exactly what NodeDevicesReconciler + the DeviceManager stamp onto the Devices.
		sel := make(ctrlcli.MatchingLabels, len(rf.Spec.NodeLabels))
		for k, v := range rf.Spec.NodeLabels {
			if strings.HasSuffix(k, _ResourceFlavorCountLabelSuffix) {
				continue
			}
			sel[k] = v
		}
		list := new(workercore.DevicesList)
		if err := r.APIReader.List(ctx, list, sel); err != nil {
			return nil, err
		}
		for i := range list.Items {
			name := list.Items[i].Name
			sd, seen := byName[name]
			if !seen {
				sd = &scopedDevices{devices: list.Items[i]}
				byName[name] = sd
				order = append(order, name)
			}
			sd.scopes = append(sd.scopes, scope)
		}
	}

	// Sorted by node name so the greedy fit — and therefore the verdict — is the same on every
	// reconcile. First-appearance order is NOT stable here: it is built while ranging a set of
	// flavor references, whose iteration order Go randomizes, and the List behind each one carries
	// no ordering guarantee either. Since the fit spends a per-card budget in list order, an
	// unstable order can change which eligible card a demand consumes.
	slices.Sort(order)
	pool := make([]scopedDevices, 0, len(order))
	for _, name := range order {
		pool = append(pool, *byName[name])
	}
	return pool, nil
}

// flavorAcceleratorKey reads the accelerator device key a ResourceFlavor pins in its
// spec.nodeLabels — the "acceleratable.feature.gpustack.ai/<key>=true" entry NodeFlavorReconciler
// writes. A CPU flavor carries none and returns empty, which matches no card group's key, so it
// contributes no cards even on a node whose Devices its broader selector also reaches.
//
// The predicate is nodefeature's own, applied to the flavor's labels rather than restated here. It
// decides which card population a role is scoped to, and a private copy of it drifted once already
// — reading a period as a metadata suffix, which discards a legal group id (device.NormalizeName
// preserves "-", "_" and ".") and holds every workload on that flavor in Retry with its cards free.
//
// nodefeature.ExtractNodeFlavors writes exactly one acceleratable key per flavor, so a flavor
// pinning several is a violation of that writer's invariant — and one this cannot answer, since the
// cards it covers would be genuinely ambiguous. Taking one of them would scope a role to a model
// chosen arbitrarily, so none is taken: the flavor covers no card. Ambiguity is the one input where
// withholding capacity is the only answer that cannot be wrong.
//
// The empty key is not itself the verdict, and this cannot hold the Workload: the demand still
// carries the flavor Kueue assigned, so what turns a keyless scope into a stated cause is
// unresolvedDemands, which sees the pool this scope ends up in.
func flavorAcceleratorKey(rf *kueue.ResourceFlavor) string {
	keys := nodefeature.ExtractAcceleratableKeys(rf.Spec.NodeLabels)
	if len(keys) != 1 {
		return ""
	}
	return keys[0]
}

// applyVerdict writes state onto every check this controller owns, but only when the state or the
// message differs from what the Workload already carries, so the controller does not re-apply (and
// re-trigger itself) on a settled Workload. A Retry carries a fixed backoff.
//
// The comparison is against the WHOLE verdict — state, message and requeue delay — because
// anything left out of it is a field the guard would pin to whatever got there first. The message
// matters most and shows why: a verdict now names its cause and its role, so two Retry verdicts of
// one Workload can differ in everything but the state — a hold for a missing flavor assignment,
// then a card shortage, or one role falling short after another. Comparing only the state would
// pin the FIRST cause onto the Workload for as long as the state held, telling an operator to wait
// for an assignment that has already arrived. The delay is in for the same reason: a Retry whose
// delay went missing would have Kueue retry immediately, which is the hot loop the backoff exists
// to prevent. All three are deterministic functions of what was observed, so writing on a change
// still settles: the next reconcile finds them equal.
func (r *NodeDevicesAdmissionReconciler) applyVerdict(
	ctx context.Context,
	wl *kueue.Workload,
	checks []kueue.AdmissionCheckReference,
	state kueue.CheckState,
	message string,
) error {
	desired, changed := desiredCheckStates(wl, checks, state, message)
	if !changed {
		return nil
	}
	return kueueworkload.PatchStatus(ctx, r.Client, wl, ctrlcli.FieldOwner(_NodeDevicesFieldOwner),
		func(w *kueue.Workload) (bool, error) {
			for i := range desired {
				kueueworkload.SetAdmissionCheckState(&w.Status.AdmissionChecks, desired[i], clock.RealClock{})
			}
			return true, nil
		})
}

// desiredCheckStates renders the verdict for every check this controller owns, and reports whether
// any of them differs from what the Workload already carries.
//
// Every owned check is returned, not only the ones that differ, because the write is a server-side
// apply and admissionChecks is a list keyed by name: an entry this field owner applied before and
// then omits is an entry it has stopped claiming, so the API server prunes the fields it owned there.
// Omitting the settled checks would therefore reset them each time a sibling changed. The whole
// desired set is applied, or nothing is.
//
// It is separated from the apply so this can be checked at all: the fake client used in tests does
// not implement apply-time pruning, so a test driving Reconcile cannot tell a complete payload from
// a partial one — it is the payload itself that has to be asserted.
func desiredCheckStates(
	wl *kueue.Workload,
	checks []kueue.AdmissionCheckReference,
	state kueue.CheckState,
	message string,
) ([]kueue.AdmissionCheckState, bool) {
	desired := make([]kueue.AdmissionCheckState, 0, len(checks))
	changed := false
	for _, name := range checks {
		acs := kueue.AdmissionCheckState{
			Name:    name,
			State:   state,
			Message: message,
		}
		if state == kueue.CheckStateRetry {
			acs.RequeueAfterSeconds = ptr.To(_NodeDevicesRetryAfterSeconds)
		}
		desired = append(desired, acs)

		// The comparison reads the Workload as FETCHED. The object PatchStatus hands an update
		// function is a bare server-side-apply skeleton carrying no status at all, so a check
		// looked up there is never found and this comparison would pass everything through.
		cur := kueueadmissioncheck.FindAdmissionCheck(wl.Status.AdmissionChecks, name)
		if cur == nil || cur.State != acs.State || cur.Message != acs.Message ||
			!ptr.Equal(cur.RequeueAfterSeconds, acs.RequeueAfterSeconds) {
			changed = true
		}
	}
	return desired, changed
}

func verdictMessage(state kueue.CheckState, podSets []kueue.PodSetReference) string {
	if state == kueue.CheckStateReady {
		return "the assigned flavor pool has enough free cards to place the request" + forRoles(podSets)
	}
	return "no node in the assigned flavor pool currently has enough free cards" +
		forRoles(podSets) + "; will retry as capacity frees"
}

// forRoles renders the podsets a verdict is about as a trailing " for role \"x\"" clause, empty
// when there are none. A multi-role Workload is judged per role — its roles can be assigned
// different flavors and so different cards — so a verdict that named only the Workload would leave
// an operator to guess which half of a prefill/decode deployment fell short. A Workload that names
// no podset (nothing has been assigned yet) reads exactly as it did before roles existed.
func forRoles(podSets []kueue.PodSetReference) string {
	if len(podSets) == 0 {
		return ""
	}
	names := make([]string, len(podSets))
	for i, ps := range podSets {
		names[i] = fmt.Sprintf("%q", ps)
	}
	slices.Sort(names)
	if len(names) == 1 {
		return " for role " + names[0]
	}
	return " for roles " + strings.Join(names, ", ")
}

// partitionLedgerNotReadyMessage is the distinct Retry message for a partition request whose pool
// carries no cached placement ledger yet — the device manager has not published it (rollout skew).
// It is separated from a genuine "profile full" so an operator can tell a transient upgrade window
// from real contention.
//
// It names the role, as every other capacity verdict does: the ledger-ready count is taken over the
// cards THIS role's flavor covers, so on a multi-role Workload the pool it reports unready is that
// role's own, and an unqualified wording would claim a rollout window over a pool another role is
// running on.
func partitionLedgerNotReadyMessage(podSets []kueue.PodSetReference) string {
	return "the partition placement ledger is not ready on the assigned flavor pool" +
		forRoles(podSets) + " (device manager rolling out); will retry"
}

func partitionVerdictMessage(state kueue.CheckState, profile string, podSets []kueue.PodSetReference) string {
	if state == kueue.CheckStateReady {
		return fmt.Sprintf("the assigned flavor pool has enough partitioned cards with a free %q placement%s",
			profile, forRoles(podSets))
	}
	return fmt.Sprintf("no partitioned card in the assigned flavor pool currently has a free %q placement%s; will retry as capacity frees",
		profile, forRoles(podSets))
}

// partitionNoCardsMessage is the Retry message when the assigned flavor pool has no partitioned
// card at all — distinct from partitionLedgerNotReadyMessage (a device-manager rollout window on
// a pool that IS partitioned), so an operator is not misled into waiting on a rollout that will
// never make an unpartitioned pool eligible; the fix is to partition a card in the pool.
// The population it reports empty is the demand's OWN — the cards of the flavor that role was
// assigned — so it names the role, as every other capacity verdict does. Without that, a
// prefill/decode deployment reads "this pool has no partitioned card" while the other role is
// running on partitioned cards of the same pool.
func partitionNoCardsMessage(profile string, podSets []kueue.PodSetReference) string {
	return fmt.Sprintf("the assigned flavor pool has no partitioned card for profile %q%s; partition a card in this pool (will retry)",
		profile, forRoles(podSets))
}

func (r *NodeDevicesAdmissionReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodedevicesadmission").
		For(
			// A start-up resync re-evaluates every reserved Workload; once a Retry'd
			// Workload is re-admitted by Kueue it reserves quota again and is re-checked.
			&kueue.Workload{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
				wl := obj.(*kueue.Workload)
				return kueueworkload.HasQuotaReservation(wl) && len(wl.Status.AdmissionChecks) > 0
			})),
		).
		Complete(r)
}

// NodeDevicesAdmissionCheckReconciler keeps the operator-applied AdmissionCheck object
// (the one carrying _NodeDevicesControllerName) marked Active. Kueue turns a
// ClusterQueue that references an inactive AdmissionCheck inactive — and its
// workloads Inadmissible — so this must report Active for the gate to function.
type NodeDevicesAdmissionCheckReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeDevicesAdmissionCheckReconciler)(nil)

func (r *NodeDevicesAdmissionCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	ac := new(kueue.AdmissionCheck)
	if err := r.Client.Get(ctx, req.NamespacedName, ac); err != nil {
		logger.Error(err, "fetch admission check")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	if ac.Spec.ControllerName != _NodeDevicesControllerName {
		logger.V(3).Info("skip admission check not owned by this controller")
		return ctrl.Result{}, nil
	}

	if kubemeta.IsConditionTrue(ac.Status.Conditions, kueue.AdmissionCheckActive) {
		return ctrl.Result{}, nil
	}

	kubemeta.SetCondition(&ac.Status.Conditions, meta.Condition{
		Type:    kueue.AdmissionCheckActive,
		Status:  meta.ConditionTrue,
		Reason:  "Ready",
		Message: "the node-devices admission check controller is running",
	})
	if err := r.Client.Status().Update(ctx, ac); err != nil {
		logger.Error(err, "mark admission check active")
		return ctrl.Result{}, err
	}

	logger.V(2).Info("activated node-devices admission check", "name", ac.Name)
	return ctrl.Result{}, nil
}

func (r *NodeDevicesAdmissionCheckReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodedevicesadmissioncheck").
		For(
			&kueue.AdmissionCheck{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
				ac := obj.(*kueue.AdmissionCheck)
				return ac.Spec.ControllerName == _NodeDevicesControllerName
			})),
		).
		Complete(r)
}
