package worker

import (
	"context"
	"fmt"
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

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
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

func TestIndexResourceFlavorByQueueName(t *testing.T) {
	const (
		exclusiveQueue = "gpustack--amd-epyc-7r13-processor-ln-x64-12c-48g--nvidia-a10g-1d"
		slicedQueue    = exclusiveQueue + "-8s"
	)

	cases := []struct {
		name  string
		queue string
		want  []string
	}{
		{
			name:  "non-sliced queue indexes only itself",
			queue: exclusiveQueue,
			want:  []string{exclusiveQueue},
		},
		{
			// A sliced flavor borrows credits from the exclusive queue, so it must
			// be discoverable under both the sliced queue and the exclusive one.
			name:  "sliced queue indexes itself and the exclusive queue",
			queue: slicedQueue,
			want:  []string{slicedQueue, exclusiveQueue},
		},
		{
			name:  "cpu-only queue ending in g is not stripped",
			queue: "gpustack--generic-ln-x64-4c-16g",
			want:  []string{"gpustack--generic-ln-x64-4c-16g"},
		},
		{
			name:  "trailing segment with empty digits is not stripped",
			queue: exclusiveQueue + "-s",
			want:  []string{exclusiveQueue + "-s"},
		},
		{
			name:  "trailing non-s suffix is not stripped",
			queue: exclusiveQueue + "-12x",
			want:  []string{exclusiveQueue + "-12x"},
		},
		{
			name:  "queue without a hyphen is not stripped",
			queue: "8s",
			want:  []string{"8s"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rf := newQueuedResourceFlavor("flavor", c.queue, c.queue, false)
			assert.Equal(t, c.want, indexResourceFlavorByQueueName(rf))
		})
	}
}

// TestEnqueueCohortWhenResourceFlavorChanged verifies a changed sliced flavor
// enqueues both its sliced queue and the suffix-stripped exclusive queue (the
// queue it lends credits to, under the shared cohort), so the exclusive queue
// re-reconciles to pick up the lent flavor. A non-sliced flavor enqueues only
// itself.
func TestEnqueueCohortWhenResourceFlavorChanged(t *testing.T) {
	const (
		cohort         = "gpustack--amd-epyc-7r13-processor-ln-x64-12c-48g--nvidia-a10g-1d"
		exclusiveQueue = cohort
		slicedQueue    = exclusiveQueue + "-8s"
	)

	cases := []struct {
		name  string
		queue string
		want  []ctrlreconcile.Request
	}{
		{
			name:  "non-sliced flavor enqueues only its queue",
			queue: exclusiveQueue,
			want: []ctrlreconcile.Request{
				{NamespacedName: ctrlcli.ObjectKey{Name: exclusiveQueue, Namespace: cohort}},
			},
		},
		{
			name:  "sliced flavor enqueues the sliced and exclusive queues",
			queue: slicedQueue,
			want: []ctrlreconcile.Request{
				{NamespacedName: ctrlcli.ObjectKey{Name: slicedQueue, Namespace: cohort}},
				{NamespacedName: ctrlcli.ObjectKey{Name: exclusiveQueue, Namespace: cohort}},
			},
		},
	}

	r := &ClusterQueueReconciler{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rf := newQueuedResourceFlavor("flavor", c.queue, cohort, false)
			assert.Equal(t, c.want, r.enqueueCohortWhenResourceFlavorChanged(context.Background(), rf))
		})
	}
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

// TestClusterQueueReconciler_ReclaimWithinCohort pins Task 7: accelerator queues
// (exclusive and sliced) enable cohort reclaim so the exclusive side can take back
// the credits it lends to the sliced side, while CPU-only queues never reclaim.
// BorrowWithinCohort stays Never to satisfy Kueue's XValidation (reclaim=Never
// requires borrow=Never), per the Task 0 finding.
func TestClusterQueueReconciler_ReclaimWithinCohort(t *testing.T) {
	const (
		exclusiveQueue = "gpustack--amd-epyc-7r13-processor-ln-x64-12c-48g--nvidia-a10g-1d"
		slicedQueue    = exclusiveQueue + "-8s"
		cpuQueue       = "gpustack--generic-ln-x64-4c-16g"
	)

	cases := []struct {
		name        string
		queue       string
		cohort      string
		wantReclaim kueue.PreemptionPolicy
	}{
		{
			name:        "cpu-only queue never reclaims",
			queue:       cpuQueue,
			cohort:      cpuQueue,
			wantReclaim: kueue.PreemptionPolicyNever,
		},
		{
			name:        "exclusive accelerator queue reclaims",
			queue:       exclusiveQueue,
			cohort:      exclusiveQueue,
			wantReclaim: kueue.PreemptionPolicyAny,
		},
		{
			name:        "sliced accelerator queue reclaims",
			queue:       slicedQueue,
			cohort:      exclusiveQueue,
			wantReclaim: kueue.PreemptionPolicyAny,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rf := newQueuedResourceFlavor("flavor", c.queue, c.cohort, false)
			cohort := &kueue.Cohort{ObjectMeta: meta.ObjectMeta{Name: c.cohort}}
			cli := buildQueueClient(rf, cohort)

			_, err := reconcileQueue(t, cli, c.cohort, c.queue)
			require.NoError(t, err)

			got := &kueue.ClusterQueue{}
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: c.queue}, got))
			require.NotNil(t, got.Spec.Preemption, "queue must carry a preemption policy")
			assert.Equal(t, c.wantReclaim, got.Spec.Preemption.ReclaimWithinCohort, "ReclaimWithinCohort")
			require.NotNil(t, got.Spec.Preemption.BorrowWithinCohort)
			assert.Equal(t, kueue.BorrowWithinCohortPolicyNever,
				got.Spec.Preemption.BorrowWithinCohort.Policy, "BorrowWithinCohort stays Never")
		})
	}
}

// newSlicedAccelNode builds a managed Node whose nvidia-a10g card model is sliced
// into `partitions`, reporting `cards` participating cards as ".sliced.units" (D
// units per card). Its single acceleratable profile is the sliced flavor/queue
// exercised by the borrow+reclaim credits test below.
func newSlicedAccelNode(name string, cards, partitions int64) *core.Node {
	nd := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				systemname.ManagedLabelKey: "true",
				core.LabelOSStable:         "linux",
				core.LabelArchStable:       "amd64",
			},
		},
		Status: core.NodeStatus{
			Allocatable: core.ResourceList{
				nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced): *resource.NewQuantity(cards*nodefeature.ResourceMaxUnits, resource.DecimalSI),
			},
		},
	}
	accKey := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g"
	nd.Labels[accKey] = "true"
	nd.Labels[accKey+".z-flavor"] = fmt.Sprintf("48c-192g-88g-%dd-%ds", cards, partitions)
	nd.Labels[accKey+".z-queue"] = fmt.Sprintf("12c-48g-1d-%ds", partitions)
	nd.Labels[accKey+".z-cohort"] = "12c-48g-1d"
	return nd
}

// creditsQuota returns the nvidia credits ResourceQuota for the named flavor in
// the constructed resource groups, or nil when absent.
func creditsQuota(groups []kueue.ResourceGroup, flavor string) *kueue.ResourceQuota {
	creditsName := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)
	for gi := range groups {
		for fi := range groups[gi].Flavors {
			fq := &groups[gi].Flavors[fi]
			if string(fq.Name) != flavor {
				continue
			}
			for ri := range fq.Resources {
				if fq.Resources[ri].Name == creditsName {
					return &fq.Resources[ri]
				}
			}
		}
	}
	return nil
}

// TestConstructResourceGroups_SlicedCredits pins Story 1's borrow+reclaim
// topology: the sliced flavor holds zero credits in its own sliced queue (it
// borrows) and contributes the card count to the exclusive queue (it lends), and
// no quota carries a BorrowingLimit.
func TestConstructResourceGroups_SlicedCredits(t *testing.T) {
	const (
		cards      = 4
		partitions = 8
	)
	node5 := newSlicedAccelNode("node-5", cards, partitions)

	profiles := nodefeature.ExtractNodeProfiles(node5)
	require.Len(t, profiles, 1, "node must expose exactly the sliced profile")
	slicedFlavor := profiles[0].Flavor
	slicedQueue := profiles[0].Queue
	exclusiveQueue, ok := stripSlicedQueueSuffix(slicedQueue)
	require.True(t, ok, "sliced queue must carry a -Ns suffix")

	rf := &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: slicedFlavor}}
	systemmeta.NoteResource(rf, "nodes", map[string]string{
		"cpu":          "48",
		"ram":          "192",
		"localStorage": "88",
	})

	cases := []struct {
		name        string
		queue       string
		wantCredits int64
	}{
		{
			// In its own sliced queue the flavor holds no credits and borrows.
			name:        "sliced queue holds zero credits",
			queue:       slicedQueue,
			wantCredits: 0,
		},
		{
			// Lent into the exclusive queue it contributes the card count so the
			// exclusive side can reclaim it.
			name:        "exclusive queue holds the card count",
			queue:       exclusiveQueue,
			wantCredits: cards,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cli := buildQueueClient(node5)
			r := &ClusterQueueReconciler{Client: cli, APIReader: cli}
			rfList := &kueue.ResourceFlavorList{Items: []kueue.ResourceFlavor{*rf}}

			groups, _ := r.constructResourceGroups(context.Background(), c.queue, rfList)

			cq := creditsQuota(groups, slicedFlavor)
			require.NotNil(t, cq, "credits quota must be present for the sliced flavor")
			assert.Equal(t, c.wantCredits, cq.NominalQuota.Value(), "credits nominal quota")
			assert.Nil(t, cq.BorrowingLimit, "credits must leave BorrowingLimit nil (unlimited borrowing)")
		})
	}
}
