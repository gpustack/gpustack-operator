package worker

import (
	"context"
	"fmt"
	"slices"

	apps "k8s.io/api/apps/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// reportRolloutComplete answers whether the leader's last update finished, which is a question
// Kubernetes cannot answer for this workload.
//
// The Deployment's own account of it is the Progressing condition, driven by a deadline this
// operator DISABLES once there are standbys. It has to: the controller's completeness test requires
// availableReplicas to equal spec.replicas, and only one replica is ever available here because the
// standbys deliberately are not ready. Left enabled, Progressing stops advancing the moment a
// rollout finishes -- exactly as it would if the image could not be pulled -- so the timeout's two
// answers are identical and a check whose answers are identical is not a check.
//
// This is that test with the structurally false clause removed. updatedReplicas and replicas both do
// reach spec.replicas on a healthy rollout here, so the pair discriminates where the triple cannot.
//
// FORBIDDEN: folding this into the health rule. Readiness asks whether a leader EXISTS, which is the
// right question for health and the wrong one for a rollout; a health predicate carrying this clause
// would report a correctly progressing backend as unhealthy for the length of every ordinary update.
// The two questions get two conditions.
func (r *KVCacheBackendReconciler) reportRolloutComplete(
	ctx context.Context, kvcb, holder *workercore.KVCacheBackend,
) {
	dropped := func() {
		holder.Status.Conditions = slices.DeleteFunc(holder.Status.Conditions,
			func(c gpustack.Condition) bool {
				return c.Type == string(KVCacheBackendConditionRolloutComplete)
			})
	}

	if kvcb.Spec.Connection.Managed == nil {
		// An external backend has no workload of ours, so there is no rollout to have finished.
		dropped()
		return
	}

	deployName := mooncake.LeaderObjectName(kvcb)
	deploy := new(apps.Deployment)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Name:      deployName,
		Namespace: kuberess.SystemNamespaceName,
	}, deploy)

	switch {
	case kerrors.IsNotFound(err):
		// Nothing has been rendered yet, or a pass could not render. Reporting a rollout as
		// incomplete would name a rollout that never started.
		dropped()

	case err != nil:
		KVCacheBackendConditionRolloutComplete.Unknown(holder, "WorkloadUnreadable", fmt.Sprintf(
			"the leader's deployment %q could not be read: %v",
			deployName, clipFaultDetail(err.Error())))

	case deploy.Status.ObservedGeneration < deploy.Generation:
		// The workload controller has not looked at the spec this operator just wrote, so every
		// count below describes the PREVIOUS one. Answering from them is how a finished old rollout
		// reads as a finished new one.
		KVCacheBackendConditionRolloutComplete.Unknown(holder, "UpdateNotObserved", fmt.Sprintf(
			"the deployment controller has not observed generation %d of %q yet, so its replica "+
				"counts still describe the update before it", deploy.Generation, deployName))

	case ptr.Deref(deploy.Spec.Replicas, 1) != mooncake.LeaderReplicas(kvcb.Spec.Connection.Managed.Leader):
		// Raising the count past one is two writes, and between them the Deployment runs one
		// replica of the elected template while the backend asks for more. Every count below agrees
		// with the Deployment's own spec then, so without this the pause between the writes would
		// read as a finished rollout.
		KVCacheBackendConditionRolloutComplete.False(holder, "ReplicasPending", fmt.Sprintf(
			"the deployment %q runs %d leader replicas and this backend asks for %d; the count is "+
				"raised once no replica of the previous template is left",
			deployName, ptr.Deref(deploy.Spec.Replicas, 1),
			mooncake.LeaderReplicas(kvcb.Spec.Connection.Managed.Leader)))

	default:
		desired := int32(1)
		if deploy.Spec.Replicas != nil {
			desired = *deploy.Spec.Replicas
		}
		if deploy.Status.UpdatedReplicas == desired && deploy.Status.Replicas == desired {
			KVCacheBackendConditionRolloutComplete.True(holder, "Complete", fmt.Sprintf(
				"all %d leader replicas run the current template, and none of the previous one is "+
					"left", desired))
			return
		}
		KVCacheBackendConditionRolloutComplete.False(holder, "Progressing", fmt.Sprintf(
			"%d of %d leader replicas run the current template and %d replicas exist in total; a "+
				"rollout that stays here is one that will not finish on its own, because this "+
				"workload disables the deadline that would otherwise say so",
			deploy.Status.UpdatedReplicas, desired, deploy.Status.Replicas))
	}
}
