package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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
// DomainRegistered and CacheAttached arrive with the tasks that can observe them — there is no
// Binding to resolve and no connector to scrape yet — and writing them here would report a state
// nothing measured.
const (
	ModelDeploymentConditionQuotaReserved kubeapistatus.ConditionType = "QuotaReserved"
)

// syncModelDeploymentStatus rebuilds the status from what was observed this pass and writes it only
// if it differs from what is stored.
//
// It is REBUILT rather than patched, so a stale field cannot survive a disagreement with the Pods:
// a role that was Ready and is not any more reports the count that was just measured, not the one
// that was true when it last changed. Anything a later task adds must fold into this one function
// for the same reason — a second writer would be free to leave its own field behind.
func (r *ModelDeploymentReconciler) syncModelDeploymentStatus(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) error {
	desired, err := r.computeModelDeploymentStatus(ctx, md, pods)
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
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) (*workercore.ModelDeploymentStatus, error) {
	// The condition accessors mutate the object they are given, so they work on a copy of the
	// observed status: conditions carry a LastTransitionTime that must not be reset on every pass,
	// and only the accessors know when it should move.
	holder := &workercore.ModelDeployment{Status: *md.Status.DeepCopy()}

	holder.Status.Roles = modelDeploymentRoleStatuses(md, pods)
	holder.Status.Endpoint = modelDeploymentEndpoint(md)

	if err := r.observeModelDeploymentQuota(ctx, md, pods, holder); err != nil {
		return nil, err
	}

	deriveModelDeploymentPhase(md, holder)

	return &holder.Status, nil
}

// modelDeploymentRoleStatuses counts, per role, how many replicas the spec asks for and how many of
// them are Ready.
//
// A role's replicas are identified by the Pod's resource note rather than by parsing its name,
// because a name is a rendering and a note is what the renderer recorded.
func modelDeploymentRoleStatuses(
	md *workercore.ModelDeployment, pods []core.Pod,
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
			Name:    role.Name,
			Desired: role.Replicas,
			Ready:   ready[role.Name],
			// A role that replaced the whole command line got no synthesized argument and no client
			// environment, so nothing here can claim it is attached to the cache.
			Unmanaged: role.Template != nil && len(role.Template.Command) > 0,
		})
	}

	return statuses
}

// modelDeploymentPodRole reads which role a replica belongs to.
func modelDeploymentPodRole(pod *core.Pod) string {
	return pod.Labels[modelDeploymentLabelKeyComponent]
}

// observeModelDeploymentQuota reports whether every replica has quota reserved.
//
// ⛔ It reads each Workload's OWN conditions rather than asking the admission gate. The gate stops
// evaluating a Workload once it is admitted, so anything derived from the gate would answer for the
// moment of admission and never again — and a Workload that has been preempted since would still
// read as reserved.
//
// A replica with no Workload yet is Unknown rather than False: Kueue creates it asynchronously from
// the Pod, so its absence is admission in flight and not a refusal.
func (r *ModelDeploymentReconciler) observeModelDeploymentQuota(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod, holder *workercore.ModelDeployment,
) error {
	if len(pods) == 0 {
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "NoReplicas",
			"no replica has been created yet")

		return nil
	}

	wls, err := r.listModelDeploymentWorkloads(ctx, md)
	if err != nil {
		return err
	}

	var reserved, pending, missing int
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			continue
		}
		wl, found := wls[pods[i].UID]
		switch {
		case !found:
			missing++
		case kubeapistatus.ConditionType(kueue.WorkloadQuotaReserved).IsTrue(wl):
			reserved++
		default:
			pending++
		}
	}

	// The ClusterQueue is named after the InstanceType, so the queue a refusal points at is read
	// off the spec rather than resolved: the LocalQueue the entrance label names is derived from it.
	queue := md.Spec.Roles[0].InstanceType

	switch {
	case pending > 0:
		ModelDeploymentConditionQuotaReserved.False(holder, "Pending", fmt.Sprintf(
			"%d of %d replicas are waiting for quota in cluster queue %q",
			pending, reserved+pending+missing, queue))
	case missing > 0:
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "AdmissionInFlight", fmt.Sprintf(
			"%d of %d replicas have no workload yet in cluster queue %q",
			missing, reserved+missing, queue))
	default:
		ModelDeploymentConditionQuotaReserved.True(holder, "Reserved", fmt.Sprintf(
			"all %d replicas have quota reserved in cluster queue %q", reserved, queue))
	}

	return nil
}

// listModelDeploymentWorkloads maps each replica's UID to the Kueue Workload that represents it.
//
// The mapping is the Workload's controller reference, which Kueue sets to the Pod it was created
// from. Deriving the Workload's NAME instead would mean importing Kueue's Pod integration, which
// drags in every other framework it supports; the reference is the same fact without the dependency.
func (r *ModelDeploymentReconciler) listModelDeploymentWorkloads(
	ctx context.Context, md *workercore.ModelDeployment,
) (map[types.UID]*kueue.Workload, error) {
	wlList := new(kueue.WorkloadList)
	err := r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}

	byPod := make(map[types.UID]*kueue.Workload, len(wlList.Items))
	for i := range wlList.Items {
		for _, ref := range wlList.Items[i].OwnerReferences {
			if ref.Kind != "Pod" {
				continue
			}
			byPod[ref.UID] = &wlList.Items[i]

			break
		}
	}

	return byPod, nil
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
