package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	coordination "k8s.io/api/coordination/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// reportHandoverVia and reportElectionVia run the PAIR of steps the reconciler runs: one read of the
// lease, then the reporter that judges it. The read moved out of the reporters so that two of them
// judging one object issue one Get rather than two, and these helpers keep the tests exercising that
// pairing instead of a shortcut that no longer resembles the caller.
func reportHandoverVia(
	ctx context.Context, r *KVCacheBackendReconciler, kvcb *workercore.KVCacheBackend,
) {
	lease, err := r.leaderLease(ctx, kvcb)
	r.reportLeaderHandover(kvcb, lease, err)
}

func reportElectionVia(
	ctx context.Context, r *KVCacheBackendReconciler, kvcb, holder *workercore.KVCacheBackend,
) {
	lease, err := r.leaderLease(ctx, kvcb)
	r.reportElectionObserved(ctx, kvcb, holder, lease, err)
}

// electingBackend is a backend with an election running, which is the only shape a handover can
// happen under.
func electingBackend(name string) *workercore.KVCacheBackend {
	return &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.KVCacheBackendSpec{
			Type: "Mooncake",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Leader: workercore.KVCacheBackendLeader{
						Replicas:         ptr.To[int32](3),
						HighAvailability: &workercore.KVCacheBackendLeaderHighAvailability{},
					},
				},
			},
		},
	}
}

func electionLease(backend, holder string, transitions int32) *coordination.Lease {
	return &coordination.Lease{
		ObjectMeta: meta.ObjectMeta{
			Name:      backend + mooncake.LeaderObjectNameSuffix,
			Namespace: kuberess.SystemNamespaceName,
		},
		Spec: coordination.LeaseSpec{
			HolderIdentity:   ptr.To(holder),
			LeaseTransitions: ptr.To(transitions),
		},
	}
}

// drain reads every event a fake recorder has buffered.
func drain(events <-chan string) []string {
	var got []string
	for {
		select {
		case e := <-events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// TestLeaderHandoverIsRecordedOnceWhenTheLeaseMoves walks the sequence a real failover produces
// rather than asserting one pass, because every interesting property of this feature is about the
// SECOND reading of the same object.
//
// The first pass establishing a baseline and recording nothing is the load-bearing one: without it
// every operator restart would report a handover on the first backend it looked at, which is the
// shape of a report that is always there and therefore says nothing.
func TestLeaderHandoverIsRecordedOnceWhenTheLeaseMoves(t *testing.T) {
	kvcb := electingBackend("store")
	lease := electionLease("store", "store-leader-a", 0)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(kvcb, lease).Build()
	recorder := record.NewFakeRecorder(10)
	events := recorder.Events
	r := &KVCacheBackendReconciler{Client: cli, Recorder: recorder}

	ctx := context.Background()

	reportHandoverVia(ctx, r, kvcb)
	assert.Empty(t, drain(events),
		"a first sighting is a baseline, not a handover: nothing here observed the before")

	reportHandoverVia(ctx, r, kvcb)
	assert.Empty(t, drain(events),
		"the same holder read twice is a renewal, which is what a lease does every few seconds")

	moved := lease.DeepCopy()
	moved.Spec.HolderIdentity = ptr.To("store-leader-b")
	moved.Spec.LeaseTransitions = ptr.To[int32](1)
	require.NoError(t, cli.Update(ctx, moved))

	reportHandoverVia(ctx, r, kvcb)
	got := drain(events)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], kvCacheBackendEventLeaderHandover)
	assert.Contains(t, got[0], "moved to another replica")
	assert.Contains(t, got[0], "1 handovers")

	reportHandoverVia(ctx, r, kvcb)
	assert.Empty(t, drain(events),
		"one event per handover, not one per pass: the timer wakes this every fifteen seconds")
}

// TestLeaderHandoverSurvivesAnEmptyHolderBetweenPasses covers the failover that passes through a
// released lease, which is what an orderly handover looks like from the outside: the holder is
// cleared, and a different replica takes it on a later pass.
//
// THE EMPTY READING IS NOT A STATE ANYBODY HELD, and remembering it as the baseline consumes the
// handover in two halves that each report nothing -- the first has no holder now, the second has no
// holder before. A genuine failover then produces no Event at all, which is the one outcome this
// feature exists to prevent.
func TestLeaderHandoverSurvivesAnEmptyHolderBetweenPasses(t *testing.T) {
	kvcb := electingBackend("store")
	lease := electionLease("store", "store-leader-a", 0)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(kvcb, lease).Build()
	recorder := record.NewFakeRecorder(10)
	events := recorder.Events
	r := &KVCacheBackendReconciler{Client: cli, Recorder: recorder}

	ctx := context.Background()

	reportHandoverVia(ctx, r, kvcb)
	require.Empty(t, drain(events), "the baseline pass reports nothing")

	released := lease.DeepCopy()
	released.Spec.HolderIdentity = nil
	require.NoError(t, cli.Update(ctx, released))

	reportHandoverVia(ctx, r, kvcb)
	assert.Empty(t, drain(events),
		"a released lease is not a handover on its own: nothing has taken it yet")

	taken := new(coordination.Lease)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(lease), taken))
	taken.Spec.HolderIdentity = ptr.To("store-leader-b")
	taken.Spec.LeaseTransitions = ptr.To[int32](1)
	require.NoError(t, cli.Update(ctx, taken))

	reportHandoverVia(ctx, r, kvcb)
	got := drain(events)
	require.Len(t, got, 1,
		"the handover is reported once the lease is taken again, even though the pass between the "+
			"two holders saw nobody holding it")
	assert.Contains(t, got[0], kvCacheBackendEventLeaderHandover)
}

// TestLeaderHandoverIsSilentWhenTheSameReplicaReacquires is the counterpart the change above must
// not break: remembering the last NON-EMPTY holder must not turn a release-and-reacquire by one
// replica into a move, because nothing moved.
func TestLeaderHandoverIsSilentWhenTheSameReplicaReacquires(t *testing.T) {
	kvcb := electingBackend("store")
	lease := electionLease("store", "store-leader-a", 0)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(kvcb, lease).Build()
	recorder := record.NewFakeRecorder(10)
	events := recorder.Events
	r := &KVCacheBackendReconciler{Client: cli, Recorder: recorder}

	ctx := context.Background()
	reportHandoverVia(ctx, r, kvcb)
	require.Empty(t, drain(events))

	released := lease.DeepCopy()
	released.Spec.HolderIdentity = nil
	require.NoError(t, cli.Update(ctx, released))
	reportHandoverVia(ctx, r, kvcb)

	back := new(coordination.Lease)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(lease), back))
	back.Spec.HolderIdentity = ptr.To("store-leader-a")
	require.NoError(t, cli.Update(ctx, back))

	reportHandoverVia(ctx, r, kvcb)
	assert.Empty(t, drain(events),
		"the same replica holding it again is not a handover, whatever the lease did in between")
}

// TestLeaderHandoverEventNamesNoReplica is the assertion that keeps this feature on the right side
// of a Non-Goal.
//
// Which replica holds the lease is deliberately not reported by this API, and the identity here is
// read only to compare two readings. An event carrying it would answer the refused question by
// accident, and would read as perfectly reasonable.
func TestLeaderHandoverEventNamesNoReplica(t *testing.T) {
	kvcb := electingBackend("store")
	lease := electionLease("store", "store-leader-a", 0)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(kvcb, lease).Build()
	recorder := record.NewFakeRecorder(10)
	events := recorder.Events
	r := &KVCacheBackendReconciler{Client: cli, Recorder: recorder}

	ctx := context.Background()
	reportHandoverVia(ctx, r, kvcb)

	moved := lease.DeepCopy()
	moved.Spec.HolderIdentity = ptr.To("store-leader-b")
	require.NoError(t, cli.Update(ctx, moved))
	reportHandoverVia(ctx, r, kvcb)

	got := drain(events)
	require.Len(t, got, 1)
	assert.NotContains(t, got[0], "store-leader-a")
	assert.NotContains(t, got[0], "store-leader-b")
}

// TestLeaderHandoverIgnoresBackendsWithNoElection covers the two shapes that have no lease to
// change hands, separately, because each reaches a different early return.
func TestLeaderHandoverIgnoresBackendsWithNoElection(t *testing.T) {
	for _, tc := range []struct {
		name string
		kvcb *workercore.KVCacheBackend
	}{
		{
			name: "no high availability",
			kvcb: func() *workercore.KVCacheBackend {
				k := electingBackend("store")
				k.Spec.Connection.Managed.Leader.HighAvailability = nil
				return k
			}(),
		},
		{
			name: "an external backend, which renders nothing at all",
			kvcb: func() *workercore.KVCacheBackend {
				k := electingBackend("store")
				k.Spec.Connection.Managed = nil
				return k
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(10)
			r := &KVCacheBackendReconciler{
				Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
					WithObjects(electionLease("store", "store-leader-a", 0)).Build(),
				Recorder: recorder,
			}

			reportHandoverVia(context.Background(), r, tc.kvcb)
			assert.Empty(t, drain(recorder.Events))
		})
	}
}

// TestLeaderHandoverForgetsAVanishedLease pins that an election which stops and later restarts does
// not report its first campaign as a handover.
//
// The lease is deleted when the leader Deployment goes -- a scale back to one replica, or a
// teardown -- and a remembered holder would then be compared against whoever wins the NEXT
// election, which is a different thing entirely.
func TestLeaderHandoverForgetsAVanishedLease(t *testing.T) {
	kvcb := electingBackend("store")
	lease := electionLease("store", "store-leader-a", 0)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(kvcb, lease).Build()
	recorder := record.NewFakeRecorder(10)
	events := recorder.Events
	r := &KVCacheBackendReconciler{Client: cli, Recorder: recorder}

	ctx := context.Background()
	reportHandoverVia(ctx, r, kvcb)
	require.NoError(t, cli.Delete(ctx, lease))
	reportHandoverVia(ctx, r, kvcb)

	require.NoError(t, cli.Create(ctx, electionLease("store", "store-leader-c", 0)))
	reportHandoverVia(ctx, r, kvcb)

	assert.Empty(t, drain(events),
		"a new election is a first sighting, not a handover from the leader of the previous one")
}

// TestLeaseHolderChangePredicate pins the filter that keeps this watch affordable.
//
// The renewal case is the one that matters: a lease is renewed every few seconds for the life of
// every leader, and letting one through costs the backend three sequential reads of its admin
// surface. A predicate that passed renewals would be correct and unusable.
func TestLeaseHolderChangePredicate(t *testing.T) {
	renewed := electionLease("store", "store-leader-a", 0)
	renewed.Spec.RenewTime = &meta.MicroTime{Time: meta.Now().Time}

	assert.False(t, kvCacheBackendLeaseHolderChanged(
		electionLease("store", "store-leader-a", 0), renewed),
		"a renewal moves renewTime and nothing this feature reads")

	assert.True(t, kvCacheBackendLeaseHolderChanged(
		electionLease("store", "store-leader-a", 0),
		electionLease("store", "store-leader-b", 1)))

	// A lease somebody else's component owns, in another namespace. Nothing this operator renders
	// lands outside the system namespace, so a name that fits the pattern there is a coincidence.
	elsewhere := electionLease("store", "store-leader-b", 1)
	elsewhere.Namespace = "kube-system"
	assert.False(t, kvCacheBackendLeaseHolderChanged(
		electionLease("store", "store-leader-a", 0), elsewhere))
}

// TestLeaseEnqueueEarnsTheLinkBackToItsBackend pins that the name is a QUERY and not a proof.
//
// The store creates this lease, so it carries no resource note and no owner reference and the name
// is the only link there is. Anything able to create a lease in the system namespace can pick a
// name that fits, and each match would cost the named backend a reconcile.
func TestLeaseEnqueueEarnsTheLinkBackToItsBackend(t *testing.T) {
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(electingBackend("store")).Build(),
	}
	ctx := context.Background()

	assert.Equal(t,
		[]ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: "store"}}},
		r.enqueueKVCacheBackendWhenLeaseChanged(ctx,
			electionLease("store", "store-leader-a", 0)))

	assert.Empty(t, r.enqueueKVCacheBackendWhenLeaseChanged(ctx,
		electionLease("nothing-of-ours", "x", 0)),
		"the name fits and no backend answers to it")

	notOurs := electionLease("store", "x", 0)
	notOurs.Name = "some-other-election"
	assert.Empty(t, r.enqueueKVCacheBackendWhenLeaseChanged(ctx, notOurs),
		"a name without the suffix is not this operator's to interpret")
}

// readyLeaderDeployment is the leader's Deployment with a serving replica, which is what makes a
// holderless lease mean anything.
func readyLeaderDeployment(backend string) *apps.Deployment {
	return &apps.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:      backend + mooncake.LeaderObjectNameSuffix,
			Namespace: kuberess.SystemNamespaceName,
		},
		Status: apps.DeploymentStatus{ReadyReplicas: 1},
	}
}

// TestElectionObservedDiscriminates covers every answer, and the pair that matters is the last two:
// the SAME holderless lease is Unknown under a leader that is not ready and False under one that is.
//
// A condition verified only where it should fire has not been shown to discriminate, and this one is
// keyed on an observation that three different faults produce -- so the reading that carries the
// information is the one where it stays quiet.
func TestElectionObservedDiscriminates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		objects    []ctrlcli.Object
		replicas   int32
		wantAbsent bool
		wantStatus meta.ConditionStatus
		wantReason string
	}{
		{
			name:       "a lease with a holder",
			objects:    []ctrlcli.Object{electionLease("store", "store-leader-a", 0)},
			replicas:   3,
			wantStatus: meta.ConditionTrue,
			wantReason: "Electing",
		},
		{
			name:       "no lease at all, and a leader that is not ready",
			replicas:   3,
			wantStatus: meta.ConditionUnknown,
			wantReason: "LeaderStarting",
		},
		{
			name:       "no lease at all, under a ready leader",
			objects:    []ctrlcli.Object{readyLeaderDeployment("store")},
			replicas:   3,
			wantStatus: meta.ConditionFalse,
			wantReason: "NoHolder",
		},
		{
			// The same object as the True case with its holder taken away. Pinned separately from
			// the missing lease because the store creates the object before it wins anything, so
			// this is the shape an image that cannot elect actually leaves behind.
			name: "a holderless lease under a ready leader",
			objects: []ctrlcli.Object{
				electionLease("store", "", 0), readyLeaderDeployment("store"),
			},
			replicas:   3,
			wantStatus: meta.ConditionFalse,
			wantReason: "NoHolder",
		},
		{
			name:       "one replica, which elects nothing by design",
			objects:    []ctrlcli.Object{readyLeaderDeployment("store")},
			replicas:   1,
			wantAbsent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kvcb := electingBackend("store")
			kvcb.Spec.Connection.Managed.Leader.Replicas = ptr.To(tc.replicas)

			r := &KVCacheBackendReconciler{
				Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
					WithObjects(tc.objects...).Build(),
			}
			holder := kvcb.DeepCopy()
			reportElectionVia(context.Background(), r, kvcb, holder)

			if tc.wantAbsent {
				assert.False(t, KVCacheBackendConditionElectionObserved.Exists(holder))
				return
			}
			assert.Equal(t, string(tc.wantStatus),
				KVCacheBackendConditionElectionObserved.GetStatus(holder),
				KVCacheBackendConditionElectionObserved.GetMessage(holder))
			assert.Equal(t, tc.wantReason,
				KVCacheBackendConditionElectionObserved.GetReason(holder))
		})
	}
}

// TestElectionObservedNamesNoCause pins the wording rule the feature states, because it is the one
// property of this condition a reader acts on and the one an edit would most easily undo.
//
// Three different faults produce a holderless lease, and a message naming the image would be wrong
// in two of them -- sending an operator to rebuild a container when the answer is a role binding.
func TestElectionObservedNamesNoCause(t *testing.T) {
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(electionLease("store", "", 0), readyLeaderDeployment("store")).Build(),
	}
	kvcb := electingBackend("store")
	holder := kvcb.DeepCopy()
	reportElectionVia(context.Background(), r, kvcb, holder)

	message := KVCacheBackendConditionElectionObserved.GetMessage(holder)
	assert.Contains(t, message, "names no holder", "the observation comes first")
	for _, cause := range []string{
		"built without the Kubernetes leadership backend",
		"role binding",
		"first campaign",
	} {
		assert.Contains(t, message, cause, "all three causes are listed, none of them chosen")
	}
}

// TestElectionObservedIsDroppedWhenTheElectionGoesAway pins the removal half, which the status
// carrying forward makes necessary: a backend scaled back to one replica would otherwise go on
// publishing a verdict about a lease nothing takes.
func TestElectionObservedIsDroppedWhenTheElectionGoesAway(t *testing.T) {
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
	}
	kvcb := electingBackend("store")
	kvcb.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](1)

	holder := kvcb.DeepCopy()
	holder.Status.Conditions = append(holder.Status.Conditions, gpustack.Condition{
		Type:    string(KVCacheBackendConditionElectionObserved),
		Status:  meta.ConditionFalse,
		Reason:  "NoHolder",
		Message: "stale",
	})

	reportElectionVia(context.Background(), r, kvcb, holder)
	assert.False(t, KVCacheBackendConditionElectionObserved.Exists(holder))
}
