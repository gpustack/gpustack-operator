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
}

// parseFamilyDemands reads the accelerator demands off a Workload's pod templates, one
// correlated tuple per (podset, family), merged across podsets only where the per-card
// demand and the profile agree — so the common single-podset Workload yields exactly one
// tuple per family. A Workload requesting no known accelerator yields none.
func parseFamilyDemands(wl *kueue.Workload) []familyDemand {
	var demands []familyDemand
	for i := range wl.Spec.PodSets {
		for _, d := range podSetFamilyDemands(&wl.Spec.PodSets[i]) {
			demands = mergeDemand(demands, d)
		}
	}
	sortDemands(demands)
	return demands
}

// podSetFamilyDemands reads one podset's per-family demand. Each family contributes
// Count × (cards summed across the podset's containers); the card key (<base> exclusive,
// <base>.shared, <base>.sliced, <base>.partitioned) adds to the count, the counting key
// (<base>.sliced.units, <base>.partitioned.units) carries the per-card units, and the
// per-profile partition key names the profile — its own value is per-card, so it does not
// add to the count. Init containers are scanned as well as app containers, so an init-only
// request is gated too. The percentage and MiB sub-keys are ignored: the Pod webhook
// already folded them into the counting key.
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
func mergeDemand(demands []familyDemand, d familyDemand) []familyDemand {
	for i := range demands {
		if demands[i].family == d.family &&
			demands[i].unitsPerCard == d.unitsPerCard &&
			demands[i].profile == d.profile {
			demands[i].cards = clampInt32(int64(demands[i].cards) + int64(d.cards))
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
	manufacturer      string
	capability        workercore.AcceleratorStatus
	mode              workercore.DeviceAllocationMode
	remaining         int32
	remainingProfiles []workercore.AcceleratorProfileCount
}

// collectCards flattens every accelerator across the candidate devices into one list,
// joining each Status allocation with its Spec capability by accelerator ID.
func collectCards(devices []workercore.Devices) []cardLedger {
	var cards []cardLedger
	for i := range devices {
		d := &devices[i]
		capByID := make(map[string]workercore.AcceleratorStatus)
		for gi := range d.Spec.Groups {
			accs := d.Spec.Groups[gi].Accelerators
			for ai := range accs {
				capByID[accs[ai].ID] = accs[ai].Status
			}
		}
		for gi := range d.Status.Groups {
			accs := d.Status.Groups[gi].Accelerators
			for ai := range accs {
				cards = append(cards, cardLedger{
					manufacturer:      d.Status.Groups[gi].Manufacturer,
					capability:        capByID[accs[ai].ID],
					mode:              accs[ai].Mode,
					remaining:         accs[ai].Remaining,
					remainingProfiles: accs[ai].RemainingProfiles,
				})
			}
		}
	}
	return cards
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
// candidate devices (already scoped to one flavor pool by label) at the same time,
// returning the check state and the message explaining it. Room is consumed as it is
// matched, so two demands of one Workload never both claim it. The reported message is the
// deciding demand's: the first that does not fit, or — when everything fits — the last one
// checked, which for the single-demand Workload is simply its own verdict. A shortage is
// always transient — Retry, never Reject.
func nodeDevicesFeasibility(devices []workercore.Devices, demands []familyDemand) (kueue.CheckState, string) {
	message := verdictMessage(kueue.CheckStateReady)
	if len(demands) == 0 {
		return kueue.CheckStateReady, message
	}

	cards := collectCards(devices)
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

// fitScalarDemand gates an exclusive/shared/logical-slice demand on the scalar per-card
// remaining ledger, which seeds every card at ResourceMaxUnits and subtracts each pod's
// allocation, so a card carrying any allocation has Remaining below a whole card and never
// satisfies an exclusive demand. Its cards count is a card count: each of them needs its
// own card. Ready once enough cards of the family's population fit, otherwise Retry.
func fitScalarDemand(cards []cardLedger, budgets []cardBudget, d familyDemand) (kueue.CheckState, string) {
	units := unitsPerCardFor(d)
	var fit int32
	for i := range cards {
		if budgets[i].whole || !cards[i].servesFamily(d.family) || cards[i].remaining < units {
			continue
		}
		budgets[i].whole = true
		if fit++; fit >= d.cards {
			return kueue.CheckStateReady, verdictMessage(kueue.CheckStateReady)
		}
	}
	return kueue.CheckStateRetry, verdictMessage(kueue.CheckStateRetry)
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
		if !c.servesFamily(nodefeature.ResourceFamilyPartitioned) {
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
			return kueue.CheckStateReady, partitionVerdictMessage(kueue.CheckStateReady, d.profile)
		}
	}
	switch {
	case partitionCards == 0:
		return kueue.CheckStateRetry, partitionNoCardsMessage(d.profile)
	case ledgerReady == 0:
		return kueue.CheckStateRetry, partitionLedgerNotReadyMessage
	}
	return kueue.CheckStateRetry, partitionVerdictMessage(kueue.CheckStateRetry, d.profile)
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
	devices, err := r.candidateDevices(ctx, wl)
	if err != nil {
		logger.Error(err, "list candidate devices")
		return ctrl.Result{}, err
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
func (r *NodeDevicesAdmissionReconciler) candidateDevices(ctx context.Context, wl *kueue.Workload) ([]workercore.Devices, error) {
	if wl.Status.Admission == nil {
		return nil, nil
	}
	flavorRefs := sets.New[kueue.ResourceFlavorReference]()
	for i := range wl.Status.Admission.PodSetAssignments {
		for _, ref := range wl.Status.Admission.PodSetAssignments[i].Flavors {
			flavorRefs.Insert(ref)
		}
	}

	byName := make(map[string]workercore.Devices)
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
			byName[list.Items[i].Name] = list.Items[i]
		}
	}

	devices := make([]workercore.Devices, 0, len(byName))
	for _, d := range byName {
		devices = append(devices, d)
	}
	return devices, nil
}

// applyVerdict writes state onto every check this controller owns, but only when it
// differs from the current state so the controller does not re-patch (and re-trigger
// itself) on a settled Workload. A Retry carries a fixed backoff.
func (r *NodeDevicesAdmissionReconciler) applyVerdict(
	ctx context.Context,
	wl *kueue.Workload,
	checks []kueue.AdmissionCheckReference,
	state kueue.CheckState,
	message string,
) error {
	return kueueworkload.PatchStatus(ctx, r.Client, wl, ctrlcli.FieldOwner(_NodeDevicesFieldOwner),
		func(w *kueue.Workload) (bool, error) {
			changed := false
			for _, name := range checks {
				if cur := kueueadmissioncheck.FindAdmissionCheck(w.Status.AdmissionChecks, name); cur != nil && cur.State == state {
					continue
				}
				acs := kueue.AdmissionCheckState{
					Name:    name,
					State:   state,
					Message: message,
				}
				if state == kueue.CheckStateRetry {
					acs.RequeueAfterSeconds = ptr.To(_NodeDevicesRetryAfterSeconds)
				}
				kueueworkload.SetAdmissionCheckState(&w.Status.AdmissionChecks, acs, clock.RealClock{})
				changed = true
			}
			return changed, nil
		})
}

func verdictMessage(state kueue.CheckState) string {
	if state == kueue.CheckStateReady {
		return "the assigned flavor pool has enough free cards to place the request"
	}
	return "no node in the assigned flavor pool currently has enough free cards; will retry as capacity frees"
}

// partitionLedgerNotReadyMessage is the distinct Retry message for a partition request whose
// pool carries no cached placement ledger yet — the device manager has not published it
// (rollout skew). It is separated from a genuine "profile full" so an operator can tell a
// transient upgrade window from real contention.
const partitionLedgerNotReadyMessage = "the partition placement ledger is not ready on the assigned flavor pool " +
	"(device manager rolling out); will retry"

func partitionVerdictMessage(state kueue.CheckState, profile string) string {
	if state == kueue.CheckStateReady {
		return fmt.Sprintf("the assigned flavor pool has enough partitioned cards with a free %q placement", profile)
	}
	return fmt.Sprintf("no partitioned card in the assigned flavor pool currently has a free %q placement; will retry as capacity frees", profile)
}

// partitionNoCardsMessage is the Retry message when the assigned flavor pool has no partitioned
// card at all — distinct from partitionLedgerNotReadyMessage (a device-manager rollout window on
// a pool that IS partitioned), so an operator is not misled into waiting on a rollout that will
// never make an unpartitioned pool eligible; the fix is to partition a card in the pool.
func partitionNoCardsMessage(profile string) string {
	return fmt.Sprintf("the assigned flavor pool has no partitioned card for profile %q; partition a card in this pool (will retry)", profile)
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
