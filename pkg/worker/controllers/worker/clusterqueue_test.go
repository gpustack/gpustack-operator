package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const (
	drainQueueName  = "gpustack--generic-ln-x64-4c-16g"
	drainCohortName = "gpustack--generic-ln-x64-4c-16g"
	drainFlavorName = "gpustack--generic-ln-x64-4c-16g-32g"
)

// newQueuedResourceFlavor builds a "nodes" ResourceFlavor annotated for the
// given queue/cohort, optionally marked draining.
func newQueuedResourceFlavor(name, queue, cohort string, draining bool) *kueue.ResourceFlavor {
	rf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				_ResourceFlavorQueueNameAnnoKey:  queue,
				_ResourceFlavorCohortNameAnnoKey: cohort,
			},
		},
	}
	systemmeta.NoteResource(rf, "nodes", map[string]string{
		"cpu":          "4",
		"ram":          "16",
		"localStorage": "32",
	})
	if draining {
		rf.Annotations[_ResourceFlavorDrainAnnoKey] = "true"
	}
	return rf
}

func buildQueueClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&kueue.ResourceFlavor{}, IndexingResourceFlavorsByQueueName, indexResourceFlavorByQueueName).
		WithIndex(&core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile).
		Build()
}

func reconcileQueue(t *testing.T, cli ctrlcli.Client, cohort, queue string) (ctrlreconcile.Result, error) {
	t.Helper()
	r := &ClusterQueueReconciler{Client: cli, APIReader: cli}
	return r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: cohort, Name: queue}})
}

func TestHasReserved(t *testing.T) {
	reserved := func(total, borrowed string) kueue.ClusterQueueStatus {
		return kueue.ClusterQueueStatus{
			FlavorsReservation: []kueue.FlavorUsage{{
				Name: kueue.ResourceFlavorReference("f"),
				Resources: []kueue.ResourceUsage{{
					Name:     core.ResourceCPU,
					Total:    resource.MustParse(total),
					Borrowed: resource.MustParse(borrowed),
				}},
			}},
		}
	}

	cases := []struct {
		name   string
		status kueue.ClusterQueueStatus
		want   bool
	}{
		{"empty", kueue.ClusterQueueStatus{}, false},
		{"reserving workloads", kueue.ClusterQueueStatus{ReservingWorkloads: 1}, true},
		{"admitted workloads", kueue.ClusterQueueStatus{AdmittedWorkloads: 1}, true},
		{"pending workloads do not count", kueue.ClusterQueueStatus{PendingWorkloads: 3}, false},
		{"reserved total", reserved("2", "0"), true},
		{"borrowed total", reserved("0", "1"), true},
		{"zero reservation, zero workloads", reserved("0", "0"), false},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cq := &kueue.ClusterQueue{Status: c.status}
			assert.Equal(t, c.want, hasReserved(cq))
		})
	}
}

func TestClusterQueueReconciler_Reconcile_AllDrainedEntersHoldAndDrain(t *testing.T) {
	// Phase 1: every ResourceFlavor is draining → the queue switches to
	// HoldAndDrain and requeues, rather than being hard-deleted.
	rf := newQueuedResourceFlavor(drainFlavorName, drainQueueName, drainCohortName, true)
	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: drainQueueName}}
	cli := buildQueueClient(rf, cq)

	res, err := reconcileQueue(t, cli, drainCohortName, drainQueueName)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, res.RequeueAfter, "should requeue while draining")

	got := &kueue.ClusterQueue{}
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: drainQueueName}, got),
		"queue must not be deleted in phase 1")
	require.NotNil(t, got.Spec.StopPolicy)
	assert.Equal(t, kueue.HoldAndDrain, *got.Spec.StopPolicy)
}

func TestClusterQueueReconciler_Reconcile_DrainedNoReservationDeletes(t *testing.T) {
	// Phase 2: already HoldAndDrain and no reservation remains → delete.
	sp := kueue.HoldAndDrain
	rf := newQueuedResourceFlavor(drainFlavorName, drainQueueName, drainCohortName, true)
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: drainQueueName},
		Spec:       kueue.ClusterQueueSpec{StopPolicy: &sp},
	}
	cli := buildQueueClient(rf, cq)

	_, err := reconcileQueue(t, cli, drainCohortName, drainQueueName)
	require.NoError(t, err)

	got := &kueue.ClusterQueue{}
	err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: drainQueueName}, got)
	assert.True(t, kerrors.IsNotFound(err),
		"drained queue with no reservation must be deleted, got err=%v", err)
}

func TestClusterQueueReconciler_Reconcile_DrainedWithReservationWaits(t *testing.T) {
	// HoldAndDrain but still has admitted workloads → wait, do not delete.
	sp := kueue.HoldAndDrain
	rf := newQueuedResourceFlavor(drainFlavorName, drainQueueName, drainCohortName, true)
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: drainQueueName},
		Spec:       kueue.ClusterQueueSpec{StopPolicy: &sp},
		Status:     kueue.ClusterQueueStatus{AdmittedWorkloads: 1},
	}
	cli := buildQueueClient(rf, cq)

	res, err := reconcileQueue(t, cli, drainCohortName, drainQueueName)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, res.RequeueAfter)

	got := &kueue.ClusterQueue{}
	assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: drainQueueName}, got),
		"queue with admitted workloads must not be deleted")
}

func TestClusterQueueReconciler_Reconcile_ActiveRestoresStopPolicyNone(t *testing.T) {
	// An active flavor (not draining) → rebuild the queue and force StopPolicy
	// back to None, lifting a previous drain.
	sp := kueue.HoldAndDrain
	rf := newQueuedResourceFlavor(drainFlavorName, drainQueueName, drainCohortName, false)
	co := &kueue.Cohort{ObjectMeta: meta.ObjectMeta{Name: drainCohortName}}
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: drainQueueName},
		Spec:       kueue.ClusterQueueSpec{StopPolicy: &sp},
	}
	cli := buildQueueClient(rf, co, cq)

	_, err := reconcileQueue(t, cli, drainCohortName, drainQueueName)
	require.NoError(t, err)

	got := &kueue.ClusterQueue{}
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: drainQueueName}, got))
	require.NotNil(t, got.Spec.StopPolicy, "active queue must carry an explicit StopPolicy")
	assert.Equal(t, kueue.None, *got.Spec.StopPolicy, "active queue must reset StopPolicy to None")
}
