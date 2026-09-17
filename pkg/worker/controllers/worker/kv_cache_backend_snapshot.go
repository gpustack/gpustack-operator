package worker

import (
	"context"
	"fmt"
	"slices"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// reportSnapshotStorage answers whether the claim a snapshot is kept on can be read by more than
// one leader pod at a time.
//
// It is the late half of a check admission cannot make. The claim is usually applied alongside the
// backend, so a webhook asking for it would refuse the ordinary case; and the requirement it asks
// about — ReadWriteMany — is not a property of the request but of the volume that was bound to it.
//
// REQUIRED: this REPORTS and refuses nothing. A claim only one pod can mount still renders, still
// starts a serving leader, and still writes snapshots that one pod can read. What it loses is the
// only thing the feature is for: a STANDBY reading them. Nothing else on the object says so — the
// backend reaches Ready either way — which is why the answer needs a condition of its own.
//
// It also does not touch Phase. A backend whose snapshots are landing somewhere the standby cannot
// reach is serving normally, and reporting it as Degraded would put a storage arrangement in the
// same field as a leader that cannot be reached at all.
func (r *KVCacheBackendReconciler) reportSnapshotStorage(
	ctx context.Context, kvcb, holder *workercore.KVCacheBackend,
) {
	var snapshot *workercore.KVCacheBackendLeaderSnapshot
	if managed := kvcb.Spec.Connection.Managed; managed != nil {
		snapshot = mooncake.LeaderSnapshot(managed.Leader)
	}

	// Dropped rather than left alone, because the status this pass builds STARTS as a copy of the
	// observed one: a backend that had a snapshot and no longer does would otherwise go on
	// publishing the last verdict about a claim nothing mounts any more, with a transition time that
	// makes it look current.
	if snapshot == nil {
		holder.Status.Conditions = slices.DeleteFunc(holder.Status.Conditions,
			func(c gpustack.Condition) bool {
				return c.Type == string(KVCacheBackendConditionSnapshotStorageShared)
			})
		return
	}

	claim := new(core.PersistentVolumeClaim)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Name:      snapshot.PersistentVolumeClaimName,
		Namespace: kuberess.SystemNamespaceName,
	}, claim)

	switch {
	case kerrors.IsNotFound(err):
		KVCacheBackendConditionSnapshotStorageShared.False(holder, "ClaimNotFound", fmt.Sprintf(
			"no persistent volume claim named %q exists in namespace %q, so no leader replica can "+
				"start until one does", snapshot.PersistentVolumeClaimName,
			kuberess.SystemNamespaceName))

	case err != nil:
		// Unknown and not False. The claim may be exactly right; what failed is the reading, and a
		// False here would report a storage fault every time the API server was briefly unreachable.
		KVCacheBackendConditionSnapshotStorageShared.Unknown(holder, "ClaimUnreadable", fmt.Sprintf(
			"the persistent volume claim %q could not be read: %v",
			snapshot.PersistentVolumeClaimName, clipFaultDetail(err.Error())))

	case claim.Status.Phase != core.ClaimBound:
		// Also Unknown, and for a sharper reason: the modes that decide this are the BOUND volume's,
		// which do not exist yet. spec.accessModes is a request, and reporting a request as the
		// answer is how this check would come to pass on a cluster that ignored it.
		KVCacheBackendConditionSnapshotStorageShared.Unknown(holder, "ClaimNotBound", fmt.Sprintf(
			"the persistent volume claim %q is in phase %q, so the access modes of the volume "+
				"behind it are not observable yet", snapshot.PersistentVolumeClaimName,
			clipFaultDetail(string(claim.Status.Phase))))

	case slices.Contains(claim.Status.AccessModes, core.ReadWriteMany):
		KVCacheBackendConditionSnapshotStorageShared.True(holder, "Shared", fmt.Sprintf(
			"the persistent volume claim %q is bound to a ReadWriteMany volume, so a standby reads "+
				"what the serving leader writes", snapshot.PersistentVolumeClaimName))

	default:
		KVCacheBackendConditionSnapshotStorageShared.False(holder, "ClaimNotShared", fmt.Sprintf(
			"the persistent volume claim %q is bound to a volume offering %v, which does not "+
				"include ReadWriteMany: the serving leader writes snapshots a standby cannot read, "+
				"so a failover starts from nothing", snapshot.PersistentVolumeClaimName,
			claim.Status.AccessModes))
	}
}
