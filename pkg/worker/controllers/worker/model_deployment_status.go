package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
)

const (
	// ModelDeploymentPhaseStarting is reported while replicas are still coming up and none is ready.
	ModelDeploymentPhaseStarting = "Starting"
	// ModelDeploymentPhaseReady is reported when every role's ready count equals its desired count.
	ModelDeploymentPhaseReady = "Ready"
	// ModelDeploymentPhaseDegraded is reported when at least one replica is ready and at least one
	// is not. It is a distinct phase rather than a shade of Starting because it is the state a
	// deployment sits in when it is serving and under-provisioned at the same time.
	ModelDeploymentPhaseDegraded = "Degraded"
	// ModelDeploymentPhaseDeleting is reported while the replicas are being torn down.
	ModelDeploymentPhaseDeleting = "Deleting"
)

// The condition types a ModelDeployment reports, one per axis. The three are independent: "quota
// reserved but cache not attached" is a real and actionable state, which is what a single phase
// string cannot carry.
//
// DomainRegistered is declared beside the rule that resolves the Binding, and CacheAttached beside
// the reading that judges it, so that each condition's vocabulary sits with the code that can
// actually observe it.
const (
	ModelDeploymentConditionQuotaReserved kubeapistatus.ConditionType = "QuotaReserved"
)

// modelDeploymentReasonPodGroupIncomplete is QuotaReserved's reason for a group that is short of the
// total it declares.
//
// It exists because that state has NO Workload at all — Kueue declines to compose one for a group it
// has not fully seen — so it is indistinguishable, from the Workload's side, from admission that has
// simply not happened yet. The two need telling apart: one clears in a moment, the other is a
// deployment sitting with gated Pods and an empty `kubectl get workloads` until something creates
// the missing replica.
const modelDeploymentReasonPodGroupIncomplete = "PodGroupIncomplete"

// syncModelDeploymentStatus rebuilds the status from what was observed this pass and writes it only
// if it differs from what is stored.
//
// It is REBUILT rather than patched, so a stale field cannot survive a disagreement with the Pods:
// a role that was Ready and is not any more reports the count that was just measured, not the one
// that was true when it last changed. Anything a later task adds must fold into this one function
// for the same reason — a second writer would be free to leave its own field behind.
// The domain is what THIS pass observed about the referenced Binding, or nil for a pass that did not
// look. Nil leaves the domain projection and its condition exactly as they were, because a teardown
// pass reporting "not observed" would read as the domain having gone away.
func (r *ModelDeploymentReconciler) syncModelDeploymentStatus(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod, domain *modelDeploymentDomain,
) error {
	desired, err := r.computeModelDeploymentStatus(ctx, md, pods, domain)
	if err != nil {
		return err
	}

	// kubemeta.DeepEqual covers LastTransitionTime too, and that is correct rather than incidental:
	// the condition accessors move it only when the condition's value changes, so an unchanged pass
	// leaves it alone and compares equal.
	if kubemeta.DeepEqual(*desired, md.Status) {
		return nil
	}

	md.Status = *desired

	return r.Client.Status().Update(ctx, md)
}

// computeModelDeploymentStatus derives the whole status from the spec and the observed Pods.
func (r *ModelDeploymentReconciler) computeModelDeploymentStatus(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod, domain *modelDeploymentDomain,
) (*workercore.ModelDeploymentStatus, error) {
	// The condition accessors mutate the object they are given, so they work on a copy of the
	// observed status: conditions carry a LastTransitionTime that must not be reset on every pass,
	// and only the accessors know when it should move.
	holder := &workercore.ModelDeployment{Status: *md.Status.DeepCopy()}

	// ONE Workload for the whole deployment, resolved once: every replica of every role is one Kueue
	// pod group, so both the per-role flavor and the group-level quota answer come from the same
	// object. Reading it twice would let the two halves of the status describe two moments.
	wl, err := r.findModelDeploymentGroupWorkload(ctx, md, pods)
	if err != nil {
		return nil, err
	}

	holder.Status.Roles = modelDeploymentRoleStatuses(md, pods, wl)
	holder.Status.Endpoint = modelDeploymentEndpoint(md)

	observeModelDeploymentDomain(holder, domain)

	observeModelDeploymentQuota(md, pods, wl, holder)

	r.observeModelDeploymentCache(ctx, md, pods, domain, holder)

	deriveModelDeploymentPhase(md, holder)

	return &holder.Status, nil
}

// modelDeploymentRoleStatuses counts, per role, how many replicas the spec asks for and how many of
// them are Ready.
//
// A role's replicas are identified by the Pod's resource note rather than by parsing its name,
// because a name is a rendering and a note is what the renderer recorded.
func modelDeploymentRoleStatuses(
	md *workercore.ModelDeployment, pods []core.Pod, wl *kueue.Workload,
) []workercore.ModelDeploymentRoleStatus {
	ready := make(map[string]int32, len(md.Spec.Roles))
	for i := range pods {
		if pods[i].DeletionTimestamp != nil || !podIsReady(&pods[i]) {
			continue
		}
		ready[modelDeploymentPodRole(&pods[i])]++
	}

	statuses := make([]workercore.ModelDeploymentRoleStatus, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		statuses = append(statuses, workercore.ModelDeploymentRoleStatus{
			Name: role.Name,
			// The kind is ECHOED so an operator reading status alone sees which role is which. It is
			// resolved rather than copied: this field is required and carries no enum, so an unset
			// spec kind would be written out and stored as the empty string.
			Kind:    ModelDeploymentEffectiveRoleKind(role),
			Desired: role.Replicas,
			Ready:   ready[role.Name],
			// A role that replaced the whole command line got no synthesized argument and no client
			// environment, so nothing here can claim it is attached to the cache.
			Unmanaged:      role.Template != nil && len(role.Template.Command) > 0,
			AssignedFlavor: modelDeploymentAssignedFlavor(wl, role),
		})
	}

	return statuses
}

// modelDeploymentAssignedFlavor is the accelerator model Kueue actually gave this role, or nil.
//
// NIL RATHER THAN EMPTY, and the distinction is the whole reason the field is a pointer: "not
// assigned yet" and "assigned to a flavor whose name is empty" are different facts, and a zero value
// collapses them into one that reads as an assignment.
//
// The flavor is read through the same function the per-accelerator check reads it with, so the
// answer status reports and the answer the gate fits against cannot diverge. That function returns
// empty for a Workload with no admission, a PodSet it does not name, and an assignment that is
// ambiguous — all three of which are "no answer", which is exactly what nil says.
func modelDeploymentAssignedFlavor(
	wl *kueue.Workload, role *workercore.ModelDeploymentRole,
) *string {
	if wl == nil {
		return nil
	}

	flavor := assignedFlavor(wl, kueue.PodSetReference(role.Name))
	if flavor == "" {
		return nil
	}

	return ptr.To(string(flavor))
}

// modelDeploymentPodRole reads which role a replica belongs to.
func modelDeploymentPodRole(pod *core.Pod) string {
	return pod.Labels[modelDeploymentLabelKeyComponent]
}

// observeModelDeploymentQuota reports whether the deployment's ONE Workload holds quota.
//
// The condition became a statement about a single object when the replicas became a pod group: Kueue
// composes one Workload per group and admits it as a unit, so `True` covers every role by
// construction and cannot be true for one role and not another.
//
// It reads that Workload's OWN conditions rather than asking the admission gate. The gate stops
// evaluating a Workload once it is admitted, so anything derived from the gate would answer for the
// moment of admission and never again — and a Workload that has been preempted since would still
// read as reserved.
//
// THE ORDER OF THE BRANCHES IS THE POINT, and PodGroupIncomplete has to come before "no Workload
// yet". A group short of its declared total has NO Workload by construction — Kueue refuses to
// compose one and says so unretryably on the Pods — so the two states look identical from here and
// mean opposite things: one resolves itself in a moment, the other is the deployment sitting with
// gated Pods and an empty `kubectl get workloads` forever. Reporting the second as the first is the
// failure this reason exists to name.
func observeModelDeploymentQuota(
	md *workercore.ModelDeployment, pods []core.Pod, wl *kueue.Workload,
	holder *workercore.ModelDeployment,
) {
	if len(pods) == 0 {
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "NoReplicas",
			"no replica has been created yet")

		return
	}

	var live int
	for i := range pods {
		if pods[i].DeletionTimestamp == nil {
			live++
		}
	}

	// EVERY REPLICA TERMINATING IS NOT "ALL RESERVED". The early return above guards an empty LIST;
	// this guards an empty RESULT, and they are different emptinesses. A recreate rollout or a group
	// rebuild hands this function a non-empty slice whose every member is skipped, and reporting on
	// what is left would claim a guarantee over a set it just finished emptying.
	//
	// Unknown rather than False: no replica holds quota, but none is being refused any either.
	if live == 0 {
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "AllReplicasTerminating", fmt.Sprintf(
			"all %d replicas are terminating, so none holds quota to report on", len(pods)))

		return
	}

	// The ClusterQueue is named after the InstanceType, so the queue a refusal points at is read
	// off the spec rather than resolved: the LocalQueue the entrance label names is derived from it.
	queue := md.Spec.Roles[0].InstanceType

	if want := int(modelDeploymentPodGroupTotalCount(md)); live < want {
		ModelDeploymentConditionQuotaReserved.False(holder, modelDeploymentReasonPodGroupIncomplete,
			fmt.Sprintf(
				"%d of %d of the group's replicas exist, so Kueue composes no workload for it at "+
					"all and there is nothing in cluster queue %q to hold quota",
				live, want, queue))

		return
	}

	switch {
	case wl == nil:
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "AdmissionInFlight", fmt.Sprintf(
			"the group is complete at %d replicas but has no workload yet in cluster queue %q",
			live, queue))
	case kubeapistatus.ConditionType(kueue.WorkloadQuotaReserved).IsTrue(wl):
		ModelDeploymentConditionQuotaReserved.True(holder, "Reserved", fmt.Sprintf(
			"the group of %d replicas has quota reserved in cluster queue %q", live, queue))
	default:
		ModelDeploymentConditionQuotaReserved.False(holder, "Pending", fmt.Sprintf(
			"the group of %d replicas is waiting for quota in cluster queue %q", live, queue))
	}
}

// findModelDeploymentGroupWorkload returns the one Workload Kueue composed for this deployment's pod
// group, or nil when there is none.
//
// IT MATCHES ON A PLAIN OWNER REFERENCE, NOT A CONTROLLER REFERENCE, and that is not a relaxation —
// it is the difference between finding the Workload and never finding one. Kueue sets a CONTROLLER
// reference when it builds a Workload from a single Pod, and plain owner references to EVERY member
// when it builds one from a pod group (`SetOwnerReference` per Pod, not `SetControllerReference`).
// So the moment the replicas became a group, a controller-reference filter started matching nothing,
// and every deployment would have reported "no workload yet" forever while being admitted normally.
//
// The API version is checked with the kind: "Pod" is not a reserved word, and a resource of that
// kind in another group would match on the kind alone.
//
// A rebuild can briefly leave the OLD group's Workload owning Pods that are on their way out. The
// scan is over the Pods this pass observed, and the result is taken in name order so that two
// candidates cannot make the reported state flip between passes while the old one drains.
func (r *ModelDeploymentReconciler) findModelDeploymentGroupWorkload(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) (*kueue.Workload, error) {
	if len(pods) == 0 {
		return nil, nil
	}

	ours := sets.New[types.UID]()
	for i := range pods {
		ours.Insert(pods[i].UID)
	}

	wlList := new(kueue.WorkloadList)
	err := r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}

	var found *kueue.Workload
	for i := range wlList.Items {
		wl := &wlList.Items[i]
		if !modelDeploymentWorkloadOwnsAny(wl, ours) {
			continue
		}
		if found == nil || wl.Name < found.Name {
			found = wl
		}
	}

	return found, nil
}

// modelDeploymentWorkloadOwnsAny reports whether the Workload names any of these Pods as an owner.
func modelDeploymentWorkloadOwnsAny(wl *kueue.Workload, pods sets.Set[types.UID]) bool {
	for _, ref := range wl.OwnerReferences {
		if ref.Kind == "Pod" && ref.APIVersion == "v1" && pods.Has(ref.UID) {
			return true
		}
	}

	return false
}

// deriveModelDeploymentPhase summarizes the counts into the one field a human reads first.
//
// Degraded means SOME replicas are ready and some are not, which is the state worth telling apart:
// the deployment is serving, at less than the capacity that was asked for. A deployment with nothing
// ready is Starting whatever the reason — to a reader, a replica that has not been admitted and one
// that has not finished loading its weights are the same state, and phaseMessage is what
// distinguishes them.
func deriveModelDeploymentPhase(md, holder *workercore.ModelDeployment) {
	if md.DeletionTimestamp != nil {
		holder.Status.Phase = ModelDeploymentPhaseDeleting
		holder.Status.PhaseMessage = "the replicas are being torn down"

		return
	}

	var desired, ready int32
	for _, rs := range holder.Status.Roles {
		desired += rs.Desired
		ready += rs.Ready
	}

	switch {
	case ready == desired:
		holder.Status.Phase = ModelDeploymentPhaseReady
		holder.Status.PhaseMessage = ""
	case ready > 0:
		holder.Status.Phase = ModelDeploymentPhaseDegraded
		holder.Status.PhaseMessage = fmt.Sprintf("%d of %d replicas are ready", ready, desired)
	default:
		holder.Status.Phase = ModelDeploymentPhaseStarting
		holder.Status.PhaseMessage = ModelDeploymentConditionQuotaReserved.GetMessage(holder)
	}
}
