package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	apps "k8s.io/api/apps/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// leaderDeployment builds the leader's Deployment with the counts a rollout moves.
func leaderDeployment(
	backend string, generation, observed, desired, updated, total int32,
) *apps.Deployment {
	return &apps.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:       backend + mooncake.LeaderObjectNameSuffix,
			Namespace:  kuberess.SystemNamespaceName,
			Generation: int64(generation),
		},
		Spec: apps.DeploymentSpec{Replicas: ptr.To(desired)},
		Status: apps.DeploymentStatus{
			ObservedGeneration: int64(observed),
			UpdatedReplicas:    updated,
			Replicas:           total,
		},
	}
}

// TestRolloutCompleteDiscriminates is built around the case that gives this condition its reason to
// exist: THREE ready replicas is never reached here, because the standbys deliberately are not
// ready, so a predicate reading availableReplicas answers False on a rollout that finished
// perfectly.
//
// The first case is exactly that shape. It has to come out True, and a predicate that carried the
// availableReplicas clause would fail it -- which is what makes this table a check rather than a
// description.
func TestRolloutCompleteDiscriminates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deploy     *apps.Deployment
		external   bool
		wantAbsent bool
		wantStatus meta.ConditionStatus
		wantReason string
	}{
		{
			name:       "three replicas updated, one of them ready, which is the steady state here",
			deploy:     leaderDeployment("store", 4, 4, 3, 3, 3),
			wantStatus: meta.ConditionTrue,
			wantReason: "Complete",
		},
		{
			name:       "an update the workload controller has not looked at yet",
			deploy:     leaderDeployment("store", 5, 4, 3, 3, 3),
			wantStatus: meta.ConditionUnknown,
			wantReason: "UpdateNotObserved",
		},
		{
			name:       "two of three on the new template",
			deploy:     leaderDeployment("store", 4, 4, 3, 2, 3),
			wantStatus: meta.ConditionFalse,
			wantReason: "Progressing",
		},
		{
			// The stall this condition exists for: every replica is on the new template and the old
			// ones were never removed. Kubernetes reports nothing, because the deadline that would
			// is disabled on this workload.
			name:       "the old replicas never went away",
			deploy:     leaderDeployment("store", 4, 4, 3, 3, 4),
			wantStatus: meta.ConditionFalse,
			wantReason: "Progressing",
		},
		{
			// Rising past one replica: the elected template has rolled out at one replica and the
			// standbys are the next write. Every count agrees with the Deployment's own spec, so only
			// the backend's asked-for count can say this is not finished.
			name:       "one replica rolled out while the backend asks for three",
			deploy:     leaderDeployment("store", 4, 4, 1, 1, 1),
			wantStatus: meta.ConditionFalse,
			wantReason: "ReplicasPending",
		},
		{
			name:       "nothing rendered yet",
			deploy:     nil,
			wantAbsent: true,
		},
		{
			name:       "an external backend, which has no workload of ours",
			deploy:     leaderDeployment("store", 4, 4, 3, 3, 3),
			external:   true,
			wantAbsent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objects []ctrlcli.Object
			if tc.deploy != nil {
				objects = append(objects, tc.deploy)
			}
			r := &KVCacheBackendReconciler{
				Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
					WithObjects(objects...).Build(),
			}

			kvcb := electingBackend("store")
			if tc.external {
				kvcb.Spec.Connection.Managed = nil
				kvcb.Spec.Connection.External = &workercore.KVCacheBackendExternal{}
			}
			holder := kvcb.DeepCopy()
			// Seeded so the absent cases assert a REMOVAL rather than an omission: the status this
			// pass builds starts as a copy of the observed one.
			holder.Status.Conditions = append(holder.Status.Conditions, gpustack.Condition{
				Type:    string(KVCacheBackendConditionRolloutComplete),
				Status:  meta.ConditionFalse,
				Reason:  "Progressing",
				Message: "stale",
			})

			r.reportRolloutComplete(context.Background(), kvcb, holder)

			if tc.wantAbsent {
				assert.False(t, KVCacheBackendConditionRolloutComplete.Exists(holder))
				return
			}
			assert.Equal(t, string(tc.wantStatus),
				KVCacheBackendConditionRolloutComplete.GetStatus(holder),
				KVCacheBackendConditionRolloutComplete.GetMessage(holder))
			assert.Equal(t, tc.wantReason,
				KVCacheBackendConditionRolloutComplete.GetReason(holder))
		})
	}
}

// TestRolloutCompleteDoesNotTouchHealth pins the separation Q3 settled: the rollout predicate is its
// own condition and reaches neither the health rule nor the phase.
//
// Folding it into readiness would report a correctly progressing backend as unhealthy for the length
// of every ordinary update, which is strictly worse than the gap it closes.
func TestRolloutCompleteDoesNotTouchHealth(t *testing.T) {
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(leaderDeployment("store", 4, 4, 3, 2, 3)).Build(),
	}

	kvcb := electingBackend("store")
	holder := kvcb.DeepCopy()
	KVCacheBackendConditionLeaderAvailable.True(holder, "Serving", "the leader serves")
	holder.Status.Phase = KVCacheBackendPhaseReady

	r.reportRolloutComplete(context.Background(), kvcb, holder)

	assert.Equal(t, string(meta.ConditionFalse),
		KVCacheBackendConditionRolloutComplete.GetStatus(holder))
	assert.True(t, KVCacheBackendConditionLeaderAvailable.IsTrue(holder),
		"a rollout in flight is not a health fault")
	assert.Equal(t, KVCacheBackendPhaseReady, holder.Status.Phase)
}
