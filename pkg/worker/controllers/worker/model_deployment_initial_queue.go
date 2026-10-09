package worker

import (
	"context"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	utilpod "sigs.k8s.io/kueue/pkg/util/pod"
)

// initialQueuedReplica verifies that every current member still waits for its first admission.
// Kueue removes the admission gate before starting a Pod. Kubernetes does not permit adding it back.
func (r *ModelDeploymentReconciler) initialQueuedReplica(
	ctx context.Context, view modelDeploymentReplicaView, workloads []kueue.Workload,
) (bool, error) {
	if !view.Seated || !modelDeploymentDeployedReplicaShape(view).modelDeploymentReplicaIsWhole() {
		return false, nil
	}

	// A member whose cached status already shows progress cannot be in the initial queue: the gate
	// is never added back and a node assignment or container status never goes away. Only the
	// survivors are confirmed on the API server.
	for _, member := range view.Members {
		if member.Spec.NodeName != "" || len(member.Status.ContainerStatuses) != 0 ||
			len(member.Status.InitContainerStatuses) != 0 ||
			len(member.Status.EphemeralContainerStatuses) != 0 {
			return false, nil
		}
	}

	for i, member := range view.Members {
		standing := new(core.Pod)
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(member), standing); err != nil {
			if kerrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if standing.UID != member.UID || standing.DeletionTimestamp != nil ||
			standing.Status.Phase != core.PodPending || standing.Spec.NodeName != "" ||
			!utilpod.HasGate(standing, kueuepodconst.SchedulingGateName) ||
			len(standing.Status.ContainerStatuses) != 0 ||
			len(standing.Status.InitContainerStatuses) != 0 ||
			len(standing.Status.EphemeralContainerStatuses) != 0 {
			return false, nil
		}
		view.Members[i] = standing
	}

	wl := replicaOwningWorkload(view.Members, workloadPointers(workloads))
	if wl == nil || wl.UID == "" || wl.DeletionTimestamp != nil || !workloadInInitialQueue(wl) {
		return false, nil
	}
	for _, member := range view.Members {
		if !modelDeploymentWorkloadOwnsAny(wl, sets.New(member.UID)) {
			return false, nil
		}
	}

	return true, nil
}

// workloadInInitialQueue rejects admission and evidence of an earlier execution or eviction.
// A false eviction condition can retain history after Kueue grants quota again.
func workloadInInitialQueue(wl *kueue.Workload) bool {
	if wl.Status.AccumulatedPastExecutionTimeSeconds != nil || wl.Status.RequeueState != nil ||
		len(wl.Status.ReclaimablePods) != 0 ||
		(wl.Status.SchedulingStats != nil && len(wl.Status.SchedulingStats.Evictions) != 0) {
		return false
	}
	for _, condition := range wl.Status.Conditions {
		switch condition.Type {
		case kueue.WorkloadAdmitted:
			if condition.Status != meta.ConditionFalse {
				return false
			}
		case kueue.WorkloadEvicted, kueue.WorkloadPreempted, kueue.WorkloadRequeued, kueue.WorkloadFinished:
			return false
		case kueue.WorkloadPodsReady:
			if condition.Status != meta.ConditionFalse || condition.Reason == kueue.WorkloadWaitForRecovery {
				return false
			}
		}
	}

	return true
}
