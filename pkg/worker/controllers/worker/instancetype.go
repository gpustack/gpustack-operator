package worker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/apistatus"
	"gpustack.ai/gpustack/pkg/worker/kuberequest"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	// _InstanceTypeDerivedFromNodeLabel marks an InstanceType the operator authored by
	// deriving it from the node-fed ResourceFlavors (instance-type-derived-from-node).
	// Only these are auto-removed when their pool's flavors vanish; admin-created
	// InstanceTypes are admin-owned and left alone.
	_InstanceTypeDerivedFromNodeLabel = "schedule.gpustack.ai/derived-from-node"

	// _ClusterQueueResType is the systemmeta resource type carried by the backing
	// ClusterQueue this reconciler owns.
	_ClusterQueueResType = "instancetypes"

	// QueueEntranceLabelKey, on a backing ClusterQueue, records the name of the
	// namespaced LocalQueue that fronts it (a workload's "kueue.x-k8s.io/queue-name"
	// value). The Pod webhook reverse-looks-up the operator-owned ClusterQueue by this
	// label to read the authoritative per-card VRAM, never trusting the user-writable
	// LocalQueue. The value is nodefeature.FormatLocalQueueName(<ClusterQueue name>).
	QueueEntranceLabelKey = ScheduleLabelPrefix + "queue-entrance"

	// IndexingResourceFlavorByNodeQueue indexes a managed ResourceFlavor by the pool
	// (ClusterQueue / InstanceType) name it feeds, so a pool's flavors resolve with a
	// single List.
	IndexingResourceFlavorByNodeQueue = "resourceflavors.schedule.gpustack.ai/node-queue"

	// valueTrue is the canonical "true" label/note value.
	valueTrue = "true"
)

// InstanceTypeReconciler owns the full lifecycle of worker.gpustack.ai InstanceType
// CRs and their backing Kueue ClusterQueue — it is the sole owner of the CQ (creation,
// alignment, and teardown all live here; there is no separate queue reconciler):
//   - From an InstanceType it ensures the name-identical CQ exists and aligns it to the
//     pool's ResourceFlavors: it builds the credit/CPU resource groups from the flavors'
//     pooled capacity, carries the InstanceType's schedule labels, and notes the
//     descriptive fields + the unit spec (admin-set wins; else derived from the flavors).
//     Auto-derived pools also get the isolation policy (empty cohort, no-borrow
//     preemption) and the node-devices AdmissionCheck reference once it is Active.
//   - In instance-type-derived-from-node mode it authors an InstanceType from the
//     ResourceFlavors of a pool that has none yet (marked derived), and removes its
//     own derived InstanceTypes once the pool's flavors are gone.
//   - It materializes the three-view (or CPU) status from the Devices ledger + the
//     ClusterQueue and refreshes the InstanceType's hardware-descriptor spec fields
//     from the queue notes, so watch consumers observe allocation churn.
//   - On delete a finalizer holds the InstanceType until it has driven the backing CQ
//     through HoldAndDrain and removed it.
type InstanceTypeReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

var _ ctrlreconcile.Reconciler = (*InstanceTypeReconciler)(nil)

func (r *InstanceTypeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch the InstanceType (may not exist yet).
	it := new(workercore.InstanceType)
	err := r.Client.Get(ctx, req.NamespacedName, it, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch instance type")
			return ctrl.Result{}, err
		}
		it = nil
	}

	derived := settings.InstanceTypeDerivedFromNode.ShouldValueBool(ctx)

	// The ResourceFlavors feeding this pool (indexed by pool name).
	rfList := new(kueue.ResourceFlavorList)
	err = r.Client.List(ctx, rfList,
		ctrlcli.MatchingFields{IndexingResourceFlavorByNodeQueue: req.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list resource flavors by node queue")
		return ctrl.Result{}, err
	}

	// No InstanceType: author one from the flavors when derived, else nothing to do.
	if it == nil {
		if derived && len(rfList.Items) > 0 {
			return r.createDerivedInstanceType(ctx, req.Name, rfList)
		}
		return ctrl.Result{}, nil
	}

	// Deleting: run the finalizer teardown.
	if it.DeletionTimestamp != nil {
		return r.teardownInstanceType(ctx, it)
	}

	// Ensure the finalizer, then continue in the same reconcile.
	if !systemmeta.Lock(it) {
		err = r.Client.Update(ctx, it)
		if err != nil {
			logger.Error(err, "add instance type finalizer")
			return ctrl.Result{}, err
		}
	}

	// A derived InstanceType whose pool has lost all its flavors is removed (its
	// finalizer then drains the backing queue). Admin-created ones stay.
	if len(rfList.Items) == 0 && it.Labels[_InstanceTypeDerivedFromNodeLabel] == valueTrue {
		err = r.Client.Delete(ctx, it)
		if err != nil && !kerrors.IsNotFound(err) {
			logger.Error(err, "delete orphaned derived instance type")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure the backing ClusterQueue exists and is aligned to the pool's flavors.
	cq, err := r.ensureClusterQueue(ctx, it, rfList, derived)
	if err != nil {
		logger.Error(err, "ensure backing cluster queue")
		return ctrl.Result{}, err
	}

	// Refresh the hardware-descriptor spec from the queue notes and the status from
	// the Devices ledger + the queue.
	return r.syncInstanceType(ctx, it, cq)
}

// createDerivedInstanceType authors an operator-owned InstanceType for a pool that
// has ResourceFlavors but no InstanceType yet. It carries the pool's schedule labels
// (so the backing queue and its Devices are reverse-looked-up) plus the derived
// marker; the descriptor spec and unit spec are filled by the subsequent reconcile.
func (r *InstanceTypeReconciler) createDerivedInstanceType(
	ctx context.Context, name string, rfList *kueue.ResourceFlavorList,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	rf := &rfList.Items[0]
	key, os, arch, _ := parseResourceFlavorSchedule(rf)
	if key == "" || os == "" || arch == "" {
		return ctrl.Result{}, nil
	}
	_, notes := systemmeta.DescribeResource(rf)
	acceleratable := notes["acceleratable"] == valueTrue

	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				featureKeyLabel(acceleratable, key): valueTrue,
				core.LabelOSStable:                  os,
				core.LabelArchStable:                arch,
				_InstanceTypeDerivedFromNodeLabel:   valueTrue,
			},
		},
	}
	if err := r.Client.Create(ctx, it); err != nil && !kerrors.IsAlreadyExists(err) {
		logger.Error(err, "create derived instance type")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("authored derived instance type")
	return ctrl.Result{}, nil
}

// ensureClusterQueue creates the backing ClusterQueue when missing and otherwise
// aligns the existing one to the pool's ResourceFlavors. It is the sole owner of the
// CQ: it builds the credit/CPU resource groups from the flavors' pooled capacity
// (buildResourceGroups), carries the InstanceType's schedule labels, and notes the
// descriptive fields + the unit spec. The isolation policy (empty cohort, no-borrow
// preemption, active StopPolicy, node-devices AdmissionCheck ref) is owned only for
// auto-derived pools; an admin-owned queue keeps its own policy.
func (r *InstanceTypeReconciler) ensureClusterQueue(
	ctx context.Context, it *workercore.InstanceType, rfList *kueue.ResourceFlavorList, derived bool,
) (*kueue.ClusterQueue, error) {
	logger := ctrllog.FromContext(ctx)

	// Stable order so the aggregated queue is deterministic.
	sort.Slice(rfList.Items, func(i, j int) bool {
		if rfList.Items[i].CreationTimestamp.Equal(&rfList.Items[j].CreationTimestamp) {
			return rfList.Items[i].Name < rfList.Items[j].Name
		}
		return rfList.Items[i].CreationTimestamp.Before(&rfList.Items[j].CreationTimestamp)
	})

	var (
		acceleratable bool
		manufacturer  string
		firstNotes    map[string]string
	)
	if len(rfList.Items) > 0 {
		_, firstNotes = systemmeta.DescribeResource(&rfList.Items[0])
		acceleratable = firstNotes["acceleratable"] == valueTrue
		manufacturer = firstNotes["manufacturer"]
	}

	cq := new(kueue.ClusterQueue)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: it.Name}, cq, ctrlclix.WithoutQuorum)
	exists := true
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, err
		}
		exists = false
		cq = nil
	}

	eResGroups := buildResourceGroups(rfList, acceleratable, manufacturer)
	eNotes := assembleClusterQueueNotes(it, cq, rfList, firstNotes)
	eLabels := instanceTypeScheduleLabels(it)
	// Advertise the entrance LocalQueue name so the Pod webhook reverse-looks-up this
	// operator-owned queue for the authoritative per-card VRAM.
	eLabels[QueueEntranceLabelKey] = nodefeature.FormatLocalQueueName(it.Name)

	// Reference the node-devices AdmissionCheck on an auto-derived accelerated queue,
	// but only once it reports Active: Kueue turns a ClusterQueue that lists a missing
	// or inactive AdmissionCheck inactive, so it would stop admitting. The Watches on
	// the AdmissionCheck re-runs this reconcile when it activates.
	var admissionChecks *kueue.AdmissionChecksStrategy
	if acceleratable && derived && r.nodeDevicesCheckActive(ctx) {
		admissionChecks = &kueue.AdmissionChecksStrategy{
			AdmissionChecks: []kueue.AdmissionCheckStrategyRule{
				{Name: kueue.AdmissionCheckReference(_NodeDevicesAdmissionCheckName)},
			},
		}
	}

	if !exists {
		cq = &kueue.ClusterQueue{
			ObjectMeta: meta.ObjectMeta{
				Name:   it.Name,
				Labels: eLabels,
			},
			Spec: kueue.ClusterQueueSpec{
				NamespaceSelector: &meta.LabelSelector{},
				StopPolicy:        ptr.To(kueue.None),
				ResourceGroups:    eResGroups,
			},
		}
		if derived {
			applyClusterQueueIsolation(&cq.Spec, admissionChecks)
		}
		systemmeta.NoteResource(cq, _ClusterQueueResType, eNotes)
		err = r.Client.Create(ctx, cq)
		if err != nil {
			if kerrors.IsAlreadyExists(err) {
				return cq, r.Client.Get(ctx, ctrlcli.ObjectKey{Name: it.Name}, cq, ctrlclix.WithoutQuorum)
			}
			return nil, err
		}
		logger.V(2).Info("created backing cluster queue")
		return cq, nil
	}

	// Align the existing queue.
	changed := false
	// Converge the resource groups from the flavors' pooled capacity (the operator
	// owns the credit/cpu quota). With no flavors there is nothing to derive, so the
	// existing quota is left intact rather than cleared.
	if len(rfList.Items) > 0 && !kubemeta.DeepEqual(cq.Spec.ResourceGroups, eResGroups) {
		cq.Spec.ResourceGroups = eResGroups
		changed = true
	}
	// Always converge the notes (descriptive fields + unit spec).
	if !systemmeta.NoteResource(cq, _ClusterQueueResType, eNotes) {
		changed = true
	}
	// Always converge the schedule discriminator labels (the reverse-lookup keys).
	if !mapx.Contain(cq.Labels, eLabels) {
		if cq.Labels == nil {
			cq.Labels = make(map[string]string)
		}
		for k, v := range eLabels {
			cq.Labels[k] = v
		}
		changed = true
	}
	// Enforce isolation + active state only for auto-derived queues; an admin-owned
	// queue keeps its own cohort/preemption policy.
	if derived && applyClusterQueueIsolation(&cq.Spec, admissionChecks) {
		changed = true
	}
	if changed {
		err = r.Client.Update(ctx, cq)
		if err != nil {
			logger.Error(err, "align backing cluster queue")
			return nil, err
		}
		logger.V(2).Info("aligned backing cluster queue")
	}
	return cq, nil
}

// applyClusterQueueIsolation converges an auto-derived queue onto the fixed no-borrow
// isolation policy — empty cohort (no cross-queue borrowing to broker), never
// reclaim/borrow within a (nonexistent) cohort, only in-queue lower-priority
// preemption, active StopPolicy, all-namespace selector — plus the node-devices
// AdmissionCheck reference. Returns true when it changed the spec.
func applyClusterQueueIsolation(spec *kueue.ClusterQueueSpec, admissionChecks *kueue.AdmissionChecksStrategy) bool {
	changed := false
	if spec.CohortName != "" {
		spec.CohortName = ""
		changed = true
	}
	if ptr.Deref(spec.StopPolicy, kueue.None) != kueue.None {
		spec.StopPolicy = ptr.To(kueue.None)
		changed = true
	}
	if spec.NamespaceSelector == nil {
		spec.NamespaceSelector = &meta.LabelSelector{}
		changed = true
	}
	wantFungibility := &kueue.FlavorFungibility{
		WhenCanBorrow:  kueue.TryNextFlavor,
		WhenCanPreempt: kueue.MayStopSearch,
	}
	if !kubemeta.DeepEqual(spec.FlavorFungibility, wantFungibility) {
		spec.FlavorFungibility = wantFungibility
		changed = true
	}
	wantPreemption := &kueue.ClusterQueuePreemption{
		ReclaimWithinCohort: kueue.PreemptionPolicyNever,
		BorrowWithinCohort: &kueue.BorrowWithinCohort{
			Policy: kueue.BorrowWithinCohortPolicyNever,
		},
		WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
	}
	if !kubemeta.DeepEqual(spec.Preemption, wantPreemption) {
		spec.Preemption = wantPreemption
		changed = true
	}
	if !kubemeta.DeepEqual(spec.AdmissionChecksStrategy, admissionChecks) {
		spec.AdmissionChecksStrategy = admissionChecks
		changed = true
	}
	return changed
}

// buildResourceGroups builds the ClusterQueue resource groups from the feeding
// flavors. An accelerated queue covers only the manufacturer's credits resource
// (nominal = capacity×M per flavor); a CPU-only queue covers only cpu (nominal =
// capacity cores).
func buildResourceGroups(rfList *kueue.ResourceFlavorList, acceleratable bool, manufacturer string) []kueue.ResourceGroup {
	covered := []core.ResourceName{core.ResourceCPU}
	if acceleratable {
		covered = []core.ResourceName{nodefeature.GetAcceleratableCreditsResourceName(manufacturer)}
	}

	var groups []kueue.ResourceGroup
	for i := range rfList.Items {
		rf := &rfList.Items[i]
		_, _, _, capacity := parseResourceFlavorSchedule(rf)
		if capacity <= 0 {
			continue
		}

		nominal := *resource.NewQuantity(capacity, resource.DecimalSI)
		if acceleratable {
			nominal = nodefeature.CardsToCredits(nominal)
		}

		// A resource group holds at most 16 flavors.
		if len(groups) == 0 || len(groups[len(groups)-1].Flavors) >= 16 {
			groups = append(groups, kueue.ResourceGroup{CoveredResources: covered})
		}
		g := &groups[len(groups)-1]
		g.Flavors = append(g.Flavors, kueue.FlavorQuotas{
			Name: kueue.ResourceFlavorReference(rf.Name),
			Resources: []kueue.ResourceQuota{
				{
					Name:         covered[0],
					NominalQuota: nominal,
					// No borrowing/lending limit: the queue keeps an empty cohort,
					// and Kueue rejects a ClusterQueue that carries a limit while it
					// belongs to no cohort. The empty cohort is the isolation — there
					// is no cohort to borrow from or lend to in the first place.
				},
			},
		})
	}
	return groups
}

// assembleClusterQueueNotes builds the backing queue's "instancetypes" notes. The
// descriptive fields always reflect the current flavors. The unit spec is
// authoritative from the InstanceType when the admin set it there (all-or-nothing,
// guarded by the webhook); otherwise a unit spec already on the queue is preserved
// (admin-set earlier or previously derived), and only a queue carrying none derives
// the min across the feeding flavors.
func assembleClusterQueueNotes(
	it *workercore.InstanceType, cq *kueue.ClusterQueue, rfList *kueue.ResourceFlavorList, firstNotes map[string]string,
) map[string]string {
	var cqNotes map[string]string
	if cq != nil {
		_, cqNotes = systemmeta.DescribeResource(cq)
	}

	// The descriptive fields come from the feeding flavors; with none there is nothing
	// to derive, so whatever the queue already carries is preserved (the type's
	// identity outlives a transient node drain).
	descriptive := firstNotes
	if len(rfList.Items) == 0 {
		descriptive = cqNotes
	}
	notes := map[string]string{
		"acceleratable": descriptive["acceleratable"],
		"manufacturer":  descriptive["manufacturer"],
		"product":       descriptive["product"],
		"family":        descriptive["family"],
		"memory":        descriptive["memory"],
		"sliceable":     descriptive["sliceable"],
	}

	// The unit spec is authoritative from the InstanceType when the admin set it there
	// (all-or-nothing, guarded by the webhook); otherwise a unit spec already on the
	// queue is preserved (admin-set earlier or previously derived), and only a queue
	// carrying none derives the min positive across the feeding flavors.
	switch admin := adminUnitNotes(it); {
	case admin["unitCPU"] != "":
		notes["unitCPU"], notes["unitRAM"], notes["localStorage"] = admin["unitCPU"], admin["unitRAM"], admin["localStorage"]
	case cqNotes["unitCPU"] != "":
		notes["unitCPU"], notes["unitRAM"], notes["localStorage"] = cqNotes["unitCPU"], cqNotes["unitRAM"], cqNotes["localStorage"]
	default:
		var unitCPU, unitRAM, localStorage string
		for i := range rfList.Items {
			_, n := systemmeta.DescribeResource(&rfList.Items[i])
			unitCPU = minPositiveNumeric(unitCPU, n["unitCPU"])
			unitRAM = minPositiveNumeric(unitRAM, n["unitRAM"])
			localStorage = minPositiveNumeric(localStorage, n["localStorage"])
		}
		notes["unitCPU"], notes["unitRAM"], notes["localStorage"] = unitCPU, unitRAM, localStorage
	}
	// A non-accelerated InstanceType's unit is always a single CPU core; pin unitCPU to
	// "1" so an admin edit to the general type's CPU unit is reset (unitRAM/localStorage
	// stay admin-editable). Read acceleratable from the flavors (authoritative) as well
	// as the spec, since a freshly authored derived InstanceType has not yet materialized
	// spec.Acceleratable from its notes.
	if notes["acceleratable"] != valueTrue && !it.Spec.Acceleratable {
		notes["unitCPU"] = "1"
	}
	return notes
}

// minPositiveNumeric returns the smaller of cur and v compared as numeric strings,
// ignoring non-positive candidates (so a flavor reporting no value never lowers the
// min). cur is the running min ("" until the first positive value).
func minPositiveNumeric(cur, v string) string {
	if stringx.CompareNumeric(v, "0") <= 0 {
		return cur
	}
	if cur == "" || stringx.CompareNumeric(v, cur) < 0 {
		return v
	}
	return cur
}

// syncInstanceType refreshes the InstanceType's hardware-descriptor spec from the
// queue notes (preserving the admin unit spec) and its status from the Devices
// ledger + the queue. Both writes are DeepEqual-guarded; the spec write takes
// priority so a single reconcile makes at most one change.
func (r *InstanceTypeReconciler) syncInstanceType(
	ctx context.Context, it *workercore.InstanceType, cq *kueue.ClusterQueue,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	desiredSpec := it.Spec
	applyDescriptorsFromClusterQueue(&desiredSpec, cq)
	// A non-accelerated InstanceType's unit is always a single CPU core; reset an admin
	// edit that set its CPU unit to anything but "1" (unitRAM/localStorage stay editable).
	// An unset value is left alone so a derived type does not churn.
	if !desiredSpec.Acceleratable &&
		desiredSpec.UnitResources.CPU != "" && desiredSpec.UnitResources.CPU != "1" {
		desiredSpec.UnitResources.CPU = "1"
	}
	if !kubemeta.DeepEqual(desiredSpec, it.Spec) {
		it.Spec = desiredSpec
		err := r.Client.Update(ctx, it)
		if err != nil {
			logger.Error(err, "update instance type descriptors")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("refreshed instance type descriptors")
		return ctrl.Result{}, nil
	}

	overcommit := settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(ctx)
	desiredStatus := r.computeStatus(ctx, cq, overcommit)
	if !kubemeta.DeepEqual(desiredStatus, it.Status) {
		it.Status = desiredStatus
		err := r.Client.Status().Update(ctx, it)
		if err != nil {
			logger.Error(err, "update instance type status")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("refreshed instance type status")
	}

	return ctrl.Result{}, nil
}

// teardownInstanceType completes a delete: it drives the backing ClusterQueue to
// HoldAndDrain, waits for Kueue to evict every reservation, deletes the drained queue
// itself, and only then releases the InstanceType's finalizer.
func (r *InstanceTypeReconciler) teardownInstanceType(
	ctx context.Context, it *workercore.InstanceType,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	if !systemmeta.IsLocked(it) {
		return ctrl.Result{}, nil
	}

	cq := new(kueue.ClusterQueue)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: it.Name}, cq, ctrlclix.WithoutQuorum)
	switch {
	case err == nil:
		// Reflect the drain in the InstanceType status so consumers — and the Instance
		// reconciler's stop check — see it go Inactive before the queue is deleted; the
		// ClusterQueue's own Active condition lags the StopPolicy write, so set it from intent.
		if it.Status.Phase != InstanceTypePhaseInactive {
			it.Status = workercore.InstanceTypeStatus{
				Phase:        InstanceTypePhaseInactive,
				PhaseMessage: "instance type is draining",
				Entrance:     nodefeature.FormatLocalQueueName(cq.Name),
			}
			if err = r.Client.Status().Update(ctx, it); err != nil {
				logger.Error(err, "mark instance type inactive while draining")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}
		}
		// Phase 1: ensure HoldAndDrain so Kueue evicts admitted workloads.
		if ptr.Deref(cq.Spec.StopPolicy, kueue.None) != kueue.HoldAndDrain {
			cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)
			if err = r.Client.Update(ctx, cq); err != nil {
				logger.Error(err, "set backing cluster queue to hold and drain")
				return ctrl.Result{}, err
			}
			logger.V(2).Info("set backing cluster queue to hold and drain; requeue in 15s")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		// Phase 2: wait until Kueue has drained all reservations, then delete.
		if hasReserved(cq) {
			logger.V(2).Info("backing cluster queue still has reserved resources; requeue in 15s")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		if err = r.Client.Delete(ctx, cq); err != nil && !kerrors.IsNotFound(err) {
			logger.Error(err, "delete drained backing cluster queue")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("deleted drained backing cluster queue; requeue to release finalizer")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case !kerrors.IsNotFound(err):
		logger.Error(err, "fetch backing cluster queue")
		return ctrl.Result{}, err
	}

	// The queue is gone: release the finalizer.
	systemmeta.Unlock(it)
	if err = r.Client.Update(ctx, it); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	logger.V(2).Info("released drained instance type")
	return ctrl.Result{}, nil
}

// computeStatus builds the InstanceType status: the accelerated three-view is a
// per-card bin-packing projection of the pool's Devices ledger, the CPU view comes
// from the ClusterQueue; a draining queue reports zero (its status views stay at
// their zero value). The phase mirrors the ClusterQueue conditions.
func (r *InstanceTypeReconciler) computeStatus(
	ctx context.Context, cq *kueue.ClusterQueue, withOvercommit bool,
) workercore.InstanceTypeStatus {
	_, notes := systemmeta.DescribeResource(cq)
	acceleratable := notes["acceleratable"] == valueTrue
	draining := cq.Spec.StopPolicy != nil && *cq.Spec.StopPolicy == kueue.HoldAndDrain

	var st workercore.InstanceTypeStatus
	if !draining {
		if acceleratable {
			devices := r.listFlavorPoolDevices(ctx, cq)
			st.Accelerator, st.AcceleratorShared, st.AcceleratorSliced = getAcceleratorResources(devices)
		} else {
			st.CPU = getCPUResource(cq, withOvercommit)
		}
	}
	st.Entrance = nodefeature.FormatLocalQueueName(cq.Name)
	st.Phase, st.PhaseMessage = apistatus.GetSummaryOfClusterQueue(&cq.Status)
	return st
}

// applyDescriptorsFromClusterQueue overwrites the hardware-descriptor spec fields
// (the ones the operator authors from the discovered hardware) from the queue notes,
// and initializes the unit spec (UnitResources / LocalStorage) from those notes when
// the InstanceType carries none — a derived InstanceType is authored without one, yet
// the Instance webhook and the table read the unit from the spec. An admin-set unit
// spec, and the admin-owned Group, are left untouched.
func applyDescriptorsFromClusterQueue(spec *workercore.InstanceTypeSpec, cq *kueue.ClusterQueue) {
	_, notes := systemmeta.DescribeResource(cq)
	acceleratable := notes["acceleratable"] == valueTrue

	spec.Acceleratable = acceleratable
	spec.Manufacturer = notes["manufacturer"]
	spec.Product = notes["product"]
	spec.Family = notes["family"]
	// os/arch live only as schedule labels (the reverse-lookup discriminators), never
	// in the notes; read them from there so the spec surfaces them.
	spec.OS = cq.Labels[core.LabelOSStable]
	spec.Arch = cq.Labels[core.LabelArchStable]

	// Clear the accelerator descriptors before (re)deriving them below, so a type
	// that is non-accelerated — or transitions to it — never keeps stale Memory /
	// Sliceable from a prior accelerated state.
	spec.InstanceTypeAccelerator = workercore.InstanceTypeAccelerator{}
	if acceleratable {
		spec.Memory = notes["memory"]
		spec.Sliceable = notes["sliceable"] == valueTrue
	}

	// Initialize the unit spec from the queue notes when the InstanceType carries
	// none (a derived one is authored without it). Written as a complete triple so
	// the InstanceType validating webhook — which rejects a partial unit spec —
	// accepts the reconciler's write; the notes store bare Gi numbers, so RAM and
	// localStorage regain their "Gi" suffix.
	if spec.UnitResources.CPU == "" && spec.UnitResources.RAM == "" && spec.LocalStorage == "" {
		unitCPU, unitRAM, localStorage := notes["unitCPU"], notes["unitRAM"], notes["localStorage"]
		if unitCPU != "" && unitRAM != "" && localStorage != "" {
			spec.UnitResources.CPU = unitCPU
			spec.UnitResources.RAM = unitRAM + "Gi"
			spec.LocalStorage = localStorage + "Gi"
		}
	}
}

// adminUnitNotes extracts the admin-owned unit spec from the InstanceType as bare
// note values (unitCPU/unitRAM/localStorage). Only present values are returned, so
// an unset unit spec leaves the operator to derive it from the flavors.
func adminUnitNotes(it *workercore.InstanceType) map[string]string {
	notes := make(map[string]string, 3)
	if v := extractPositiveNumberFromString(it.Spec.UnitResources.CPU); v != "" {
		notes["unitCPU"] = v
	}
	if v := extractPositiveNumberFromQuantity(it.Spec.UnitResources.RAM, "Gi"); v != "" {
		notes["unitRAM"] = v
	}
	if v := extractPositiveNumberFromQuantity(it.Spec.LocalStorage, "Gi"); v != "" {
		notes["localStorage"] = v
	}
	return notes
}

// instanceTypeScheduleLabels copies the schedule discriminator labels (the feature
// key plus kubernetes.io/os|arch) from the InstanceType onto the backing queue, so
// the queue's ResourceFlavors and Devices are reverse-looked-up by selector.
func instanceTypeScheduleLabels(it *workercore.InstanceType) map[string]string {
	out := make(map[string]string, 3)
	for k, v := range it.Labels {
		switch {
		case k == core.LabelOSStable, k == core.LabelArchStable:
			out[k] = v
		case v == valueTrue &&
			(strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
				strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)):
			out[k] = v
		}
	}
	return out
}

// listFlavorPoolDevices reads the Devices ledgers of every managed node backing the
// queue's flavor pool, located by the queue's own schedule labels plus
// gpustack.ai/managed=true. The read is cached (the reconciler watches Devices); a
// missing or empty selector yields no devices rather than matching every object.
func (r *InstanceTypeReconciler) listFlavorPoolDevices(ctx context.Context, cq *kueue.ClusterQueue) []workercore.Devices {
	sel := poolDevicesSelector(cq.Labels)
	if len(sel) == 0 {
		return nil
	}
	list := new(workercore.DevicesList)
	if err := r.Client.List(ctx, list, ctrlcli.MatchingLabels(sel)); err != nil {
		return nil
	}
	return list.Items
}

// poolDevicesSelector extracts the reverse-lookup selector from a queue's labels: its
// feature key and kubernetes.io/os|arch, plus gpustack.ai/managed=true. Returns nil
// when the queue carries no feature key, so the caller never matches every object.
func poolDevicesSelector(cqLabels map[string]string) map[string]string {
	sel := make(map[string]string, 4)
	hasFeatureKey := false
	for k, v := range cqLabels {
		switch {
		case k == core.LabelOSStable, k == core.LabelArchStable:
			sel[k] = v
		case v == valueTrue &&
			(strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
				strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)):
			sel[k] = v
			hasFeatureKey = true
		}
	}
	if !hasFeatureKey {
		return nil
	}
	sel[systemname.ManagedLabelKey] = valueTrue
	return sel
}

// getAcceleratorResources aggregates the per-card Devices ledger of one flavor pool
// into the three InstanceType display views. Each Devices object is one node, and the
// status ledger seeds every card at ResourceMaxUnits (D), subtracting each allocation;
// a card's Mode marks how it is currently used. Per node:
//   - exclusive: cards that are entirely free (Remaining >= D), one each;
//   - shared:    over free and Shared cards, Remaining/(D/SharedResourceMaxSize) shares;
//   - sliced:    over free and Sliced cards, Remaining/(D/100) VRAM-percent units.
//
// Capacity is the whole pool seen as that mode (cards, cards×10, cards×100), Remaining
// sums each node's availability. OnceMaxRequest is the largest single allocation: the
// largest single node's availability for exclusive/shared (one allocation can span a
// node's cards), but the freest single card's percent for sliced (a slice targets one
// card — VRAM is per-card — so a single sliced request is at most 100).
func getAcceleratorResources(devices []workercore.Devices) (exclusive, shared, sliced workercore.InstanceTypeResource) {
	const (
		d         = int64(nodefeature.ResourceMaxUnits)
		sharedMax = int64(nodefeature.SharedResourceMaxSize) // ownership shares per card
		slicedMax = int64(100)                               // VRAM-percent units per card
	)
	sharedUnit, slicedUnit := d/sharedMax, d/slicedMax

	var (
		capExcl, remExcl, ormExcl       int64
		capShared, remShared, ormShared int64
		capSliced, remSliced, ormSliced int64
	)
	for i := range devices {
		dev := &devices[i]
		var nodeCards, nodeExcl, nodeShared, nodeSliced int64
		for j := range dev.Status.Groups {
			g := &dev.Status.Groups[j]
			for k := range g.Accelerators {
				a := &g.Accelerators[k]
				nodeCards++
				rem := int64(a.Remaining)
				free := rem >= d
				if free {
					nodeExcl++
				}
				if free || a.Mode == workercore.DeviceAllocationModeShared {
					nodeShared += rem / sharedUnit
				}
				if free || a.Mode == workercore.DeviceAllocationModeSliced {
					cardSliced := rem / slicedUnit
					nodeSliced += cardSliced
					// A sliced request targets a single card (VRAM is the per-card,
					// non-oversubscribable anchor), so the largest single sliced request is the
					// freest card's percent (≤100), not a node's card-sum — unlike exclusive and
					// shared, whose one request can span a node's cards.
					ormSliced = max(ormSliced, cardSliced)
				}
			}
		}
		capExcl += nodeCards
		capShared += nodeCards * sharedMax
		capSliced += nodeCards * slicedMax
		remExcl += nodeExcl
		remShared += nodeShared
		remSliced += nodeSliced
		ormExcl = max(ormExcl, nodeExcl)
		ormShared = max(ormShared, nodeShared)
		// ormSliced is tracked per-card in the loop above (a slice is single-card), not per-node.
	}

	mk := func(orm, rem, total int64) workercore.InstanceTypeResource {
		return workercore.InstanceTypeResource{
			OnceMaxRequest: *resource.NewQuantity(orm, resource.DecimalSI),
			Remaining:      *resource.NewQuantity(rem, resource.DecimalSI),
			Capacity:       *resource.NewQuantity(total, resource.DecimalSI),
		}
	}
	return mk(ormExcl, remExcl, capExcl),
		mk(ormShared, remShared, capShared),
		mk(ormSliced, remSliced, capSliced)
}

// getCPUResource computes the CPU display resource of a non-accelerated InstanceType
// from its ClusterQueue: capacity is the summed nominal CPU quota, remaining subtracts
// the reserved total (scaled back from overcommit units), and the once-max request is
// the largest single node's core count (encoded in a flavor name) bounded by remaining.
func getCPUResource(cq *kueue.ClusterQueue, withOvercommit bool) workercore.InstanceTypeResource {
	var capCPU, ormCPU resource.Quantity
	for i := range cq.Spec.ResourceGroups {
		rg := &cq.Spec.ResourceGroups[i]
		for j := range rg.Flavors {
			flv := &rg.Flavors[j]
			for k := range flv.Resources {
				if flv.Resources[k].Name == core.ResourceCPU {
					capCPU.Add(flv.Resources[k].NominalQuota)
				}
			}
			if c := parseNodeFlavorCount(string(flv.Name)); c > ormCPU.Value() {
				ormCPU = *resource.NewQuantity(c, resource.DecimalSI)
			}
		}
	}

	remCPU := capCPU.DeepCopy()
	for i := range cq.Status.FlavorsReservation {
		flv := &cq.Status.FlavorsReservation[i]
		for j := range flv.Resources {
			res := &flv.Resources[j]
			if res.Name != core.ResourceCPU {
				continue
			}
			total := res.Total
			if withOvercommit {
				total = kuberequest.ScaleBackOvercommit(res.Name, total, false)
			}
			remCPU.Sub(total)
		}
	}
	if remCPU.Sign() < 0 {
		remCPU = *resource.NewQuantity(0, resource.DecimalSI)
	}
	if ormCPU.Cmp(remCPU) > 0 {
		ormCPU = remCPU.DeepCopy()
	}

	return workercore.InstanceTypeResource{
		OnceMaxRequest: ormCPU,
		Remaining:      remCPU,
		Capacity:       capCPU,
	}
}

// parseNodeFlavorCount extracts the per-node count encoded in a node ResourceFlavor
// name "gpustack-${key}-${os}-${arch}-${count}{c|d}" (CPU cores for a CPU flavor,
// device count for a device flavor); returns 0 when the name lacks the suffix.
func parseNodeFlavorCount(name string) int64 {
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return 0
	}
	seg := name[i+1:]
	if len(seg) < 2 {
		return 0
	}
	switch seg[len(seg)-1] {
	case 'c', 'd':
		if v, err := strconvx.Atoi[int64](seg[:len(seg)-1]); err == nil {
			return v
		}
	}
	return 0
}

// extractPositiveNumberFromString returns v unchanged when it is a positive integer
// carrying no unit suffix, and "" otherwise (including empty input).
func extractPositiveNumberFromString(v string) string {
	n, err := strconvx.ParseInt[int32](v, 10, 32)
	if err == nil && n > 0 {
		return v
	}
	return ""
}

// extractPositiveNumberFromQuantity strips the given suffix and returns the bare
// positive integer, or "" when the suffix is absent or the remainder is not a positive
// integer (including empty input).
func extractPositiveNumberFromQuantity(v, suffix string) string {
	b, ok := strings.CutSuffix(v, suffix)
	if ok {
		return extractPositiveNumberFromString(b)
	}
	return ""
}

// instanceTypeNameFromScheduleLabels returns the pool's InstanceType/ClusterQueue name
// ("gpustack-${key}-${os}-${arch}") from an object carrying the schedule discriminator
// labels (a ResourceFlavor's are parsed by parseResourceFlavorSchedule; a Devices'
// here). Returns "" when a discriminator is missing.
func instanceTypeNameFromScheduleLabels(labels map[string]string) string {
	os := labels[core.LabelOSStable]
	arch := labels[core.LabelArchStable]
	var key string
	for k, v := range labels {
		if v != valueTrue {
			continue
		}
		switch {
		case strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix):
			key = strings.TrimPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)
		case strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix):
			key = strings.TrimPrefix(k, nodefeature.GeneralFeatureLabelPrefix)
		default:
			continue
		}
		break
	}
	if key == "" || os == "" || arch == "" {
		return ""
	}
	return fmt.Sprintf("gpustack-%s-%s-%s", key, os, arch)
}

// hasReserved reports whether the ClusterQueue still holds reserved quota or
// admitted/reserving workloads. The queue must not be deleted until Kueue has
// finished draining; the workload counters guard against the flavor reservation
// snapshot momentarily reading zero while eviction is still in flight. Pending
// workloads are intentionally not counted — they hold no reservation, and gating
// on them would block deletion forever.
func hasReserved(cq *kueue.ClusterQueue) bool {
	if cq.Status.ReservingWorkloads != 0 || cq.Status.AdmittedWorkloads != 0 {
		return true
	}
	return slicex.Any(cq.Status.FlavorsReservation, func(i int) bool {
		return slicex.Any(cq.Status.FlavorsReservation[i].Resources, func(j int) bool {
			return !cq.Status.FlavorsReservation[i].Resources[j].Total.IsZero() ||
				!cq.Status.FlavorsReservation[i].Resources[j].Borrowed.IsZero()
		})
	})
}

// nodeDevicesCheckActive reports whether the node-devices AdmissionCheck exists and
// is Active. The queue references it only when true, since listing an inactive
// check would turn the ClusterQueue inactive and stop it admitting.
func (r *InstanceTypeReconciler) nodeDevicesCheckActive(ctx context.Context) bool {
	ac := new(kueue.AdmissionCheck)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: _NodeDevicesAdmissionCheckName}, ac,
		ctrlclix.WithoutQuorum)
	if err != nil {
		return false
	}
	return kubemeta.IsConditionTrue(ac.Status.Conditions, kueue.AdmissionCheckActive)
}

// resourceFlavorNodeQueueName is the name of the pool a flavor feeds:
// "gpustack-${key}-${os}-${arch}", the flavor name with its "-${count}{c|d}"
// suffix dropped, so flavors differing only in per-node count aggregate together.
// It returns "" when the flavor lacks the schedule labels.
func resourceFlavorNodeQueueName(rf *kueue.ResourceFlavor) string {
	key, os, arch, _ := parseResourceFlavorSchedule(rf)
	if key == "" || os == "" || arch == "" {
		return ""
	}
	return fmt.Sprintf("gpustack-%s-%s-%s", key, os, arch)
}

// parseResourceFlavorSchedule reads a flavor's schedule labels: its feature key
// (the bare "general."/"acceleratable." prefixed label whose value is "true"), the
// kubernetes.io/os|arch values, and the key's ".capacity" sibling (the pooled
// capacity = nodes × count). Missing fields come back empty/zero.
func parseResourceFlavorSchedule(rf *kueue.ResourceFlavor) (key, os, arch string, capacity int64) {
	os = rf.Labels[core.LabelOSStable]
	arch = rf.Labels[core.LabelArchStable]
	for k, v := range rf.Labels {
		if v != valueTrue {
			continue
		}
		switch {
		case strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix):
			key = strings.TrimPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)
		case strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix):
			key = strings.TrimPrefix(k, nodefeature.GeneralFeatureLabelPrefix)
		default:
			continue
		}
		capacity, _ = strconvx.Atoi[int64](rf.Labels[k+_ResourceFlavorCapacityLabelSuffix])
		break
	}
	return key, os, arch, capacity
}

// indexResourceFlavorByNodeQueue maps a managed ResourceFlavor to the pool name it
// feeds, so the reconciler resolves a pool's flavors with a single List.
func indexResourceFlavorByNodeQueue(obj ctrlcli.Object) []string {
	rf, ok := obj.(*kueue.ResourceFlavor)
	if !ok || rf == nil {
		return nil
	}
	if rf.DeletionTimestamp != nil {
		return nil
	}
	if !systemmeta.MatchResource(rf, _ResourceFlavorResType) {
		return nil
	}
	name := resourceFlavorNodeQueueName(rf)
	if name == "" {
		return nil
	}
	return []string{name}
}

func (r *InstanceTypeReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure the ResourceFlavor→pool field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &kueue.ResourceFlavor{}, IndexingResourceFlavorByNodeQueue, indexResourceFlavorByNodeQueue)
	if err != nil {
		return fmt.Errorf("index resource flavor '%s': %w", IndexingResourceFlavorByNodeQueue, err)
	}

	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("instancetype").
		For(
			// Reconcile each InstanceType by its own name. Fire on create, on a spec
			// change (admin edit), when the deletion timestamp appears, and when the
			// object is finally removed; ignore the operator's own status/finalizer
			// writes to avoid self-triggering.
			&workercore.InstanceType{},
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				DeleteFunc: func(ctrlevent.DeleteEvent) bool {
					// A removed InstanceType must re-trigger its pool reconcile: when the
					// pool's ResourceFlavor still exists (a transient drain, or an admin
					// deleting a derived InstanceType), the reconcile re-authors it — the
					// derived-pool existence is level-based, not edge-triggered on a
					// ResourceFlavor change alone. When the flavor is gone too the
					// reconcile is a no-op, so this never loops.
					return true
				},
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					oldIt, newIt := e.ObjectOld.(*workercore.InstanceType), e.ObjectNew.(*workercore.InstanceType)
					if !oldIt.DeletionTimestamp.Equal(newIt.DeletionTimestamp) {
						return true
					}
					return oldIt.Generation != newIt.Generation
				},
			}),
		).
		Watches(
			// A ResourceFlavor change drives derived authoring, re-derivation of the
			// pool's InstanceType (existence + capacity), and CQ realignment.
			&kueue.ResourceFlavor{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstanceTypeWhenResourceFlavorChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ResourceFlavorResType)
				}),
			),
		).
		Watches(
			// A backing ClusterQueue change refreshes the descriptor spec (notes) and
			// the CPU view (reservation).
			&kueue.ClusterQueue{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstanceTypeWhenClusterQueueChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ClusterQueueResType)
				}),
			),
		).
		Watches(
			// A Devices ledger change moves the accelerated three-view; this is the
			// watch a ClusterQueue projection could not serve.
			&workercore.Devices{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstanceTypeWhenDevicesChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return obj.GetLabels()[systemname.ManagedLabelKey] == valueTrue
				}),
			),
		).
		Watches(
			// Re-enqueue the pools when the node-devices AdmissionCheck changes, so an
			// accelerated derived queue acquires the reference once it turns Active.
			&kueue.AdmissionCheck{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueInstanceTypesWhenAdmissionCheckChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return obj.(*kueue.AdmissionCheck).Spec.ControllerName == _NodeDevicesControllerName
				}),
			),
		).
		Complete(r)
}

func (r *InstanceTypeReconciler) enqueueInstanceTypeWhenResourceFlavorChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("resource flavor", ctrlcli.ObjectKeyFromObject(obj))

	name := resourceFlavorNodeQueueName(obj.(*kueue.ResourceFlavor))
	if name == "" {
		logger.V(3).Info("resource flavor has no schedule labels, skip")
		return nil
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{Name: name},
		},
	}
	logger.V(2).Info("enqueued instance type from resource flavor", "requests", reqs)
	return reqs
}

func (r *InstanceTypeReconciler) enqueueInstanceTypeWhenClusterQueueChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("cluster queue", ctrlcli.ObjectKeyFromObject(obj))

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKeyFromObject(obj),
		},
	}
	logger.V(2).Info("enqueued instance type from cluster queue", "requests", reqs)
	return reqs
}

func (r *InstanceTypeReconciler) enqueueInstanceTypeWhenDevicesChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("devices", ctrlcli.ObjectKeyFromObject(obj))

	name := instanceTypeNameFromScheduleLabels(obj.GetLabels())
	if name == "" {
		logger.V(3).Info("devices has no schedule labels, skip")
		return nil
	}

	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{Name: name},
		},
	}
	logger.V(2).Info("enqueued instance type from devices", "requests", reqs)
	return reqs
}

// enqueueInstanceTypesWhenAdmissionCheckChanged enqueues every InstanceType when the
// node-devices AdmissionCheck changes, so each pool (re)acquires the reference once
// the check turns Active (or drops it should the check go away).
func (r *InstanceTypeReconciler) enqueueInstanceTypesWhenAdmissionCheckChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("admission check", ctrlcli.ObjectKeyFromObject(obj))

	ac, ok := obj.(*kueue.AdmissionCheck)
	if !ok || ac.Spec.ControllerName != _NodeDevicesControllerName {
		return nil
	}

	itList := new(workercore.InstanceTypeList)
	if err := r.Client.List(ctx, itList, ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy); err != nil {
		logger.Error(err, "list instance types for admission check")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(itList.Items))
	for i := range itList.Items {
		it := &itList.Items[i]
		if it.DeletionTimestamp != nil {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: it.Name}})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueued instance types from admission check", "requests", reqs)
	return reqs
}
