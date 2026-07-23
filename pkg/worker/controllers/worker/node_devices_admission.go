package worker

import (
	"context"
	"fmt"
	"math"
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
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const (
	// _NodeDevicesControllerName is the AdmissionCheck controllerName this operator
	// claims; installKueue applies an AdmissionCheck object carrying it.
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

// cardRequest is the accelerator demand a Workload places on its assigned pool:
// the allocation mode, the total cards summed across its pods and containers, the
// per-card sliced units (zero for exclusive/shared), and — for a physical-slice
// (MIG) request — the profile name it anchors on (empty otherwise).
type cardRequest struct {
	mode        workercore.DeviceAllocationMode
	count       int32
	slicedUnits int32
	profile     string
}

// parseCardRequest reads the accelerator request off a Workload's pod templates.
// Each podset contributes Count × (cards summed across its containers); the mode
// and per-card sliced units come from the requested resource names — the card key
// (<base> exclusive, <base>.shared, <base>.sliced) sets the mode and adds to the
// count, while <base>.sliced.units carries the per-card units. A physical-slice
// (MIG) key <base>.sliced.mig-<profile> sets the sliced mode and the profile the
// request anchors on (its own value is per-card, so it does not add to the count;
// the count comes from the sibling <base>.sliced key). Init containers are scanned
// as well as app containers so an init-only MIG request is gated too. The percentage
// and MiB sliced sub-keys are ignored: the Pod webhook already folded them into units.
// A Workload requesting no known accelerator returns mode None.
func parseCardRequest(wl *kueue.Workload) cardRequest {
	var req cardRequest
	for i := range wl.Spec.PodSets {
		ps := &wl.Spec.PodSets[i]
		var perPod int32
		containers := make([]*core.Container, 0, len(ps.Template.Spec.InitContainers)+len(ps.Template.Spec.Containers))
		for ci := range ps.Template.Spec.InitContainers {
			containers = append(containers, &ps.Template.Spec.InitContainers[ci])
		}
		for ci := range ps.Template.Spec.Containers {
			containers = append(containers, &ps.Template.Spec.Containers[ci])
		}
		for _, ctr := range containers {
			// Scan requests AND limits: extended resources (including MIG keys) are commonly set
			// only under limits, so a requests-only scan would miss the request and wrongly mark
			// feasibility Ready. Merge with requests taking precedence so each resource is counted once.
			merged := ctr.Resources.Requests
			if len(ctr.Resources.Limits) > 0 {
				merged = make(core.ResourceList, len(ctr.Resources.Requests)+len(ctr.Resources.Limits))
				for name, qty := range ctr.Resources.Limits {
					merged[name] = qty
				}
				for name, qty := range ctr.Resources.Requests {
					merged[name] = qty
				}
			}
			for name, qty := range merged {
				switch {
				case strings.Contains(string(name), nodefeature.SlicedMigResourceNameInfix):
					if profile, ok := nodefeature.SlicedMigProfileOf(name); ok {
						req.mode = workercore.DeviceAllocationModeSliced
						req.profile = profile
					}
				case strings.HasSuffix(string(name), nodefeature.SlicedUnitsResourceNameSuffix):
					// Keep the strictest per-card demand across containers/podsets, so
					// feasibility is never checked against an undersized slice.
					if u := clampInt32(qty.Value()); u > req.slicedUnits {
						req.slicedUnits = u
					}
				case strings.HasSuffix(string(name), nodefeature.SlicedResourceNameSuffix):
					if nodefeature.IsKnownAcceleratableResourceName(name) {
						req.mode = workercore.DeviceAllocationModeSliced
						perPod += clampInt32(qty.Value())
					}
				case strings.HasSuffix(string(name), nodefeature.SharedResourceNameSuffix):
					if nodefeature.IsKnownAcceleratableResourceName(name) {
						req.mode = workercore.DeviceAllocationModeShared
						perPod += clampInt32(qty.Value())
					}
				default:
					if nodefeature.IsKnownAcceleratableResourceName(name) {
						req.mode = workercore.DeviceAllocationModeExclusive
						perPod += clampInt32(qty.Value())
					}
				}
			}
		}
		req.count += ps.Count * perPod
	}
	return req
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

// unitsPerCardFor returns the allocatable units one card must still have free to
// host a single card of the request: a whole card for exclusive, one owner's share
// for shared, and the requested sliced units for sliced.
func unitsPerCardFor(mode workercore.DeviceAllocationMode, slicedUnits int32) int32 {
	switch mode {
	case workercore.DeviceAllocationModeShared:
		return nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize
	case workercore.DeviceAllocationModeSliced:
		return slicedUnits
	default:
		return nodefeature.ResourceMaxUnits
	}
}

// cardLedger is a per-card view joining the Spec-side capability (the physical-slice
// profiles + their cached placements) with the Status-side allocation (mode, scalar
// remaining, and the per-profile remaining ledger), matched by accelerator ID. A
// card with a non-empty physicalProfiles is MIG-enabled.
type cardLedger struct {
	mode              workercore.DeviceAllocationMode
	remaining         int32
	remainingProfiles []workercore.AcceleratorProfileCount
	physicalProfiles  []workercore.AcceleratorPhysicalSlicedProfile
}

// eachCard invokes fn for every accelerator across the candidate devices, joining
// each Status allocation with its Spec capability by accelerator ID.
func eachCard(devices []workercore.Devices, fn func(cardLedger)) {
	for i := range devices {
		d := &devices[i]
		capByID := make(map[string][]workercore.AcceleratorPhysicalSlicedProfile)
		for gi := range d.Spec.Groups {
			accs := d.Spec.Groups[gi].Accelerators
			for ai := range accs {
				capByID[accs[ai].ID] = accs[ai].Status.PhysicalSliced.Profiles
			}
		}
		for gi := range d.Status.Groups {
			accs := d.Status.Groups[gi].Accelerators
			for ai := range accs {
				fn(cardLedger{
					mode:              accs[ai].Mode,
					remaining:         accs[ai].Remaining,
					remainingProfiles: accs[ai].RemainingProfiles,
					physicalProfiles:  capByID[accs[ai].ID],
				})
			}
		}
	}
}

// nodeDevicesFeasibility reports whether the request can currently be placed across
// the candidate devices (already scoped to one flavor pool by label), returning the
// check state and the message explaining it. A physical-slice (MIG) request is gated
// on the per-card RemainingProfiles ledger; every other mode is gated on the scalar
// remaining ledger. The shortage is always transient — Retry, never Reject.
func nodeDevicesFeasibility(devices []workercore.Devices, req cardRequest) (kueue.CheckState, string) {
	if req.profile != "" {
		return physicalSlicedFeasibility(devices, req.profile, req.count)
	}
	return scalarFeasibility(devices, req.mode, req.count, req.slicedUnits)
}

// scalarFeasibility gates an exclusive/shared/soft-sliced request on the scalar per-card
// remaining ledger, which seeds every card at ResourceMaxUnits and subtracts each pod's
// allocation, so a card carrying any allocation has Remaining below a whole card and never
// satisfies an exclusive request. A soft-slice request additionally excludes MIG-enabled cards
// (a hard-partitioned card offers no soft budget). Ready once enough cards fit, otherwise Retry.
func scalarFeasibility(devices []workercore.Devices, mode workercore.DeviceAllocationMode, count, slicedUnits int32) (kueue.CheckState, string) {
	demand := unitsPerCardFor(mode, slicedUnits)
	var fit int32
	eachCard(devices, func(c cardLedger) {
		if mode == workercore.DeviceAllocationModeSliced && len(c.physicalProfiles) > 0 {
			return // a soft slice never lands on a MIG-enabled card
		}
		if c.remaining >= demand {
			fit++
		}
	})
	if fit >= count {
		return kueue.CheckStateReady, verdictMessage(kueue.CheckStateReady)
	}
	return kueue.CheckStateRetry, verdictMessage(kueue.CheckStateRetry)
}

// physicalSlicedFeasibility gates a MIG request on the per-card placement-aware ledger: a
// candidate card is MIG-enabled (its capability lists physical-slice profiles), is not held
// whole-card (Mode None or Sliced), and still has a free placement for the profile
// (RemainingProfiles[profile] >= 1). A pool with no MIG-enabled card at all, and a pool whose MIG
// cards have not yet published cached Placements (rollout skew), each get their own distinct Retry
// message (separate from "the profile is momentarily full") so an operator can tell the three apart.
// Never Reject.
func physicalSlicedFeasibility(devices []workercore.Devices, profile string, count int32) (kueue.CheckState, string) {
	var migCards, ledgerReady, fit int32
	eachCard(devices, func(c cardLedger) {
		if len(c.physicalProfiles) == 0 {
			return // not MIG-enabled
		}
		migCards++
		if physicalProfilesHavePlacements(c.physicalProfiles) {
			ledgerReady++
		}
		if c.mode != workercore.DeviceAllocationModeNone && c.mode != workercore.DeviceAllocationModeSliced {
			return // held whole-card by an exclusive/shared allocation
		}
		if remainingProfileCount(c.remainingProfiles, profile) >= 1 {
			fit++
		}
	})
	if migCards == 0 {
		return kueue.CheckStateRetry, physicalNoMigCardsMessage(profile)
	}
	if ledgerReady == 0 {
		return kueue.CheckStateRetry, physicalLedgerNotReadyMessage
	}
	if fit >= count {
		return kueue.CheckStateReady, physicalVerdictMessage(kueue.CheckStateReady, profile)
	}
	return kueue.CheckStateRetry, physicalVerdictMessage(kueue.CheckStateRetry, profile)
}

// physicalProfilesHavePlacements reports whether any capability profile carries a cached
// placement set — the signal that the device manager has published a MIG placement ledger.
func physicalProfilesHavePlacements(profiles []workercore.AcceleratorPhysicalSlicedProfile) bool {
	for i := range profiles {
		if len(profiles[i].Placements) > 0 {
			return true
		}
	}
	return false
}

// remainingProfileCount returns how many more instances of profile the card can still build,
// from its RemainingProfiles ledger (zero when the profile is absent).
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
	// or finish the verdict is moot.
	if !kueueworkload.HasQuotaReservation(wl) || kueueworkload.IsFinished(wl) || !kueueworkload.IsActive(wl) {
		logger.V(3).Info("skip workload not holding quota reservation or already finished")
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

	request := parseCardRequest(wl)
	devices, err := r.candidateDevices(ctx, wl)
	if err != nil {
		logger.Error(err, "list candidate devices")
		return ctrl.Result{}, err
	}
	state, message := nodeDevicesFeasibility(devices, request)

	if err := r.applyVerdict(ctx, wl, checks, state, message); err != nil {
		logger.Error(err, "patch admission check state")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("evaluated node-devices admission",
		"state", state, "mode", request.mode.String(), "cards", request.count, "profile", request.profile)
	return ctrl.Result{}, nil
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

// physicalLedgerNotReadyMessage is the distinct Retry message for a MIG request whose pool
// carries no cached placement ledger yet — the device manager has not published it (rollout
// skew). It is separated from a genuine "profile full" so an operator can tell a transient
// upgrade window from real contention.
const physicalLedgerNotReadyMessage = "the MIG placement ledger is not ready on the assigned flavor pool (device manager rolling out); will retry"

func physicalVerdictMessage(state kueue.CheckState, profile string) string {
	if state == kueue.CheckStateReady {
		return fmt.Sprintf("the assigned flavor pool has enough MIG cards with a free %q placement", profile)
	}
	return fmt.Sprintf("no MIG card in the assigned flavor pool currently has a free %q placement; will retry as capacity frees", profile)
}

// physicalNoMigCardsMessage is the Retry message when the assigned flavor pool has no MIG-enabled
// card at all — distinct from physicalLedgerNotReadyMessage (a device-manager rollout window on a
// pool that IS MIG-enabled), so an operator is not misled into waiting on a rollout that will never
// make a non-MIG pool eligible; the fix is to enable MIG on a card in the pool.
func physicalNoMigCardsMessage(profile string) string {
	return fmt.Sprintf("the assigned flavor pool has no MIG-enabled card for profile %q; enable MIG on a card in this pool (will retry)", profile)
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
