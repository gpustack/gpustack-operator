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
	"k8s.io/utils/ptr"
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

func TestClusterQueueReconciler_Reconcile(t *testing.T) {
	cases := []struct {
		name string

		// Inputs.
		flavorDraining    bool
		stopPolicy        *kueue.StopPolicy
		admittedWorkloads int32
		withCohort        bool

		// Expectations.
		wantRequeueAfter time.Duration     // 0 → not asserted
		wantDeleted      bool              // queue is hard-deleted
		wantStopPolicy   *kueue.StopPolicy // nil → not asserted
	}{
		{
			// Phase 1: every ResourceFlavor is draining → the queue switches to
			// HoldAndDrain and requeues, rather than being hard-deleted.
			name:             "all drained enters HoldAndDrain",
			flavorDraining:   true,
			wantRequeueAfter: 15 * time.Second,
			wantStopPolicy:   ptr.To(kueue.HoldAndDrain),
		},
		{
			// Phase 2: already HoldAndDrain and no reservation remains → delete.
			name:           "drained, no reservation, deletes",
			flavorDraining: true,
			stopPolicy:     ptr.To(kueue.HoldAndDrain),
			wantDeleted:    true,
		},
		{
			// HoldAndDrain but still has admitted workloads → wait, do not delete.
			name:              "drained, with reservation, waits",
			flavorDraining:    true,
			stopPolicy:        ptr.To(kueue.HoldAndDrain),
			admittedWorkloads: 1,
			wantRequeueAfter:  15 * time.Second,
		},
		{
			// An active flavor (not draining) → rebuild the queue and force
			// StopPolicy back to None, lifting a previous drain.
			name:           "active restores StopPolicy None",
			stopPolicy:     ptr.To(kueue.HoldAndDrain),
			withCohort:     true,
			wantStopPolicy: ptr.To(kueue.None),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rf := newQueuedResourceFlavor(drainFlavorName, drainQueueName, drainCohortName, c.flavorDraining)
			cq := &kueue.ClusterQueue{
				ObjectMeta: meta.ObjectMeta{Name: drainQueueName},
				Spec:       kueue.ClusterQueueSpec{StopPolicy: c.stopPolicy},
				Status:     kueue.ClusterQueueStatus{AdmittedWorkloads: c.admittedWorkloads},
			}
			objs := []ctrlcli.Object{rf, cq}
			if c.withCohort {
				objs = append(objs, &kueue.Cohort{ObjectMeta: meta.ObjectMeta{Name: drainCohortName}})
			}
			cli := buildQueueClient(objs...)

			res, err := reconcileQueue(t, cli, drainCohortName, drainQueueName)
			require.NoError(t, err)
			if c.wantRequeueAfter != 0 {
				assert.Equal(t, c.wantRequeueAfter, res.RequeueAfter, "RequeueAfter")
			}

			got := &kueue.ClusterQueue{}
			err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: drainQueueName}, got)
			if c.wantDeleted {
				assert.True(t, kerrors.IsNotFound(err),
					"drained queue must be deleted, got err=%v", err)
				return
			}
			require.NoError(t, err, "queue must not be deleted")
			if c.wantStopPolicy != nil {
				require.NotNil(t, got.Spec.StopPolicy, "queue must carry an explicit StopPolicy")
				assert.Equal(t, *c.wantStopPolicy, *got.Spec.StopPolicy, "Spec.StopPolicy")
			}
		})
	}
}
