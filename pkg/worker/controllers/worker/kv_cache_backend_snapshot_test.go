package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// snapshotBackend is a backend keeping its snapshot on the named claim, or on none when the name is
// blank.
func snapshotBackend(claimName string) *workercore.KVCacheBackend {
	ha := &workercore.KVCacheBackendLeaderHighAvailability{}
	if claimName != "" {
		ha.Snapshot = &workercore.KVCacheBackendLeaderSnapshot{
			PersistentVolumeClaimName: claimName,
		}
	}
	return &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "store"},
		Spec: workercore.KVCacheBackendSpec{
			Type: "Mooncake",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Leader: workercore.KVCacheBackendLeader{
						Replicas:         ptr.To[int32](3),
						HighAvailability: ha,
					},
				},
			},
		},
	}
}

// boundClaim is a claim whose BOUND volume offers the given modes. The status rather than the spec
// is what this check reads, so the fixture sets the status.
func boundClaim(name string, modes ...core.PersistentVolumeAccessMode) *core.PersistentVolumeClaim {
	return &core.PersistentVolumeClaim{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: kuberess.SystemNamespaceName},
		Spec: core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteMany},
		},
		Status: core.PersistentVolumeClaimStatus{
			Phase:       core.ClaimBound,
			AccessModes: modes,
		},
	}
}

// TestSnapshotStorageCondition covers every answer this check can give, and the two it must NOT
// give are what the table is for.
//
// A shared claim and an unshared one are the pair the feature turns on. The rest are states in
// which the answer is not yet knowable, and reporting either of them as a verdict is how a check
// comes to pass on a cluster that never honored the request: spec.accessModes is what was asked
// for, and only the bound volume's modes are what was given.
func TestSnapshotStorageCondition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		claim      *core.PersistentVolumeClaim
		wantStatus meta.ConditionStatus
		wantReason string
	}{
		{
			name:       "a claim bound to a shared volume",
			claim:      boundClaim("snaps", core.ReadWriteMany),
			wantStatus: meta.ConditionTrue,
			wantReason: "Shared",
		},
		{
			// The failure the whole condition exists for. Every other signal on the object agrees
			// the backend is fine, because it is: one pod writes snapshots nothing else can read.
			name:       "a claim bound to a volume only one pod can write",
			claim:      boundClaim("snaps", core.ReadWriteOnce),
			wantStatus: meta.ConditionFalse,
			wantReason: "ClaimNotShared",
		},
		{
			// A volume several pods can READ is not one several pods can write, and the serving
			// replica is the writer. Pinned separately because it is the mode most likely to be
			// mistaken for the right one.
			name:       "a claim bound to a read-only-many volume",
			claim:      boundClaim("snaps", core.ReadOnlyMany),
			wantStatus: meta.ConditionFalse,
			wantReason: "ClaimNotShared",
		},
		{
			name: "a claim that exists and is not bound yet",
			claim: func() *core.PersistentVolumeClaim {
				c := boundClaim("snaps", core.ReadWriteMany)
				c.Status = core.PersistentVolumeClaimStatus{Phase: core.ClaimPending}
				return c
			}(),
			wantStatus: meta.ConditionUnknown,
			wantReason: "ClaimNotBound",
		},
		{
			name:       "no claim of that name",
			claim:      nil,
			wantStatus: meta.ConditionFalse,
			wantReason: "ClaimNotFound",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme)
			if tc.claim != nil {
				builder = builder.WithObjects(tc.claim)
			}
			r := &KVCacheBackendReconciler{Client: builder.Build()}

			kvcb := snapshotBackend("snaps")
			holder := kvcb.DeepCopy()
			r.reportSnapshotStorage(context.Background(), kvcb, holder)

			assert.Equal(t, string(tc.wantStatus),
				KVCacheBackendConditionSnapshotStorageShared.GetStatus(holder),
				KVCacheBackendConditionSnapshotStorageShared.GetMessage(holder))
			assert.Equal(t, tc.wantReason,
				KVCacheBackendConditionSnapshotStorageShared.GetReason(holder))
		})
	}
}

// TestSnapshotStorageConditionIsAbsentWithoutASnapshot pins that the condition is a verdict about
// storage and not a field every backend carries.
//
// The removal half is the load-bearing one: the status this pass builds starts as a copy of the
// observed one, so a backend whose snapshot was withdrawn would otherwise go on publishing the last
// verdict about a claim nothing mounts.
func TestSnapshotStorageConditionIsAbsentWithoutASnapshot(t *testing.T) {
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
	}

	holder := snapshotBackend("").DeepCopy()
	r.reportSnapshotStorage(context.Background(), snapshotBackend(""), holder)
	assert.False(t, KVCacheBackendConditionSnapshotStorageShared.Exists(holder),
		"a backend asking for no snapshot has no storage for this to be a verdict about")

	// Now with a verdict already on the object, which is the state a withdrawal arrives in.
	holder.Status.Conditions = append(holder.Status.Conditions, gpustack.Condition{
		Type:    string(KVCacheBackendConditionSnapshotStorageShared),
		Status:  meta.ConditionFalse,
		Reason:  "ClaimNotShared",
		Message: "stale",
	})
	r.reportSnapshotStorage(context.Background(), snapshotBackend(""), holder)
	assert.False(t, KVCacheBackendConditionSnapshotStorageShared.Exists(holder),
		"withdrawing the snapshot has to take its verdict with it")
}

// TestSnapshotStorageConditionOnAnUnreadableClaim pins the one answer that must NOT be False.
//
// A read that failed says nothing about the claim, and reporting it as a storage fault would put a
// briefly unreachable API server and a genuinely single-writer volume in the same state — with the
// operator's remedy for one being useless against the other.
func TestSnapshotStorageConditionOnAnUnreadableClaim(t *testing.T) {
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(
					context.Context, ctrlcli.WithWatch, ctrlcli.ObjectKey, ctrlcli.Object,
					...ctrlcli.GetOption,
				) error {
					return errors.New("the api server said no")
				},
			}).Build(),
	}

	kvcb := snapshotBackend("snaps")
	holder := kvcb.DeepCopy()
	r.reportSnapshotStorage(context.Background(), kvcb, holder)

	assert.Equal(t, string(meta.ConditionUnknown),
		KVCacheBackendConditionSnapshotStorageShared.GetStatus(holder))
	assert.Equal(t, "ClaimUnreadable",
		KVCacheBackendConditionSnapshotStorageShared.GetReason(holder))
}
