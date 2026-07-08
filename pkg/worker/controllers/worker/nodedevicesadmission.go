package worker

import (
	"context"
	"math"
	"strings"

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
// the allocation mode, the total cards summed across its pods and containers, and
// the per-card sliced units (zero for exclusive/shared).
type cardRequest struct {
	mode        workercore.DeviceAllocationMode
	count       int32
	slicedUnits int32
}

// parseCardRequest reads the accelerator request off a Workload's pod templates.
// Each podset contributes Count × (cards summed across its containers); the mode
// and per-card sliced units come from the requested resource names — the card key
// (<base> exclusive, <base>.shared, <base>.sliced) sets the mode and adds to the
// count, while <base>.sliced.units carries the per-card units. The percentage and
// MiB sliced sub-keys are ignored: the Pod webhook already folded them into units.
// A Workload requesting no known accelerator returns mode None.
func parseCardRequest(wl *kueue.Workload) cardRequest {
	var req cardRequest
	for i := range wl.Spec.PodSets {
		ps := &wl.Spec.PodSets[i]
		var perPod int32
		for ci := range ps.Template.Spec.Containers {
			for name, qty := range ps.Template.Spec.Containers[ci].Resources.Requests {
				switch {
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

// nodeDevicesFeasibility reports whether count cards — each with at least the
// per-card demand still free — exist across the candidate devices (already scoped
// to one flavor pool by label). The status ledger seeds every card at
// ResourceMaxUnits and subtracts each pod's allocation, so a card carrying any
// allocation has Remaining below a whole card and never satisfies an exclusive
// request. Returns Ready once enough cards fit, otherwise Retry: the shortage is
// transient and re-checked as capacity frees, never rejected.
func nodeDevicesFeasibility(devices []workercore.Devices, mode workercore.DeviceAllocationMode, count, slicedUnits int32) kueue.CheckState {
	demand := unitsPerCardFor(mode, slicedUnits)
	var fit int32
	for i := range devices {
		groups := devices[i].Status.Groups
		for gi := range groups {
			accelerators := groups[gi].Accelerators
			for ai := range accelerators {
				if accelerators[ai].Remaining >= demand {
					fit++
				}
			}
		}
	}
	if fit >= count {
		return kueue.CheckStateReady
	}
	return kueue.CheckStateRetry
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
	state := nodeDevicesFeasibility(devices, request.mode, request.count, request.slicedUnits)

	if err := r.applyVerdict(ctx, wl, checks, state); err != nil {
		logger.Error(err, "patch admission check state")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("evaluated node-devices admission",
		"state", state, "mode", request.mode.String(), "cards", request.count)
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
					Message: verdictMessage(state),
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
