package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordination "k8s.io/api/coordination/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

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

	r.reportLeaderHandover(ctx, kvcb)
	assert.Empty(t, drain(events),
		"a first sighting is a baseline, not a handover: nothing here observed the before")

	r.reportLeaderHandover(ctx, kvcb)
	assert.Empty(t, drain(events),
		"the same holder read twice is a renewal, which is what a lease does every few seconds")

	moved := lease.DeepCopy()
	moved.Spec.HolderIdentity = ptr.To("store-leader-b")
	moved.Spec.LeaseTransitions = ptr.To[int32](1)
	require.NoError(t, cli.Update(ctx, moved))

	r.reportLeaderHandover(ctx, kvcb)
	got := drain(events)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], kvCacheBackendEventLeaderHandover)
	assert.Contains(t, got[0], "moved to another replica")
	assert.Contains(t, got[0], "1 handovers")

	r.reportLeaderHandover(ctx, kvcb)
	assert.Empty(t, drain(events),
		"one event per handover, not one per pass: the timer wakes this every fifteen seconds")
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
	r.reportLeaderHandover(ctx, kvcb)

	moved := lease.DeepCopy()
	moved.Spec.HolderIdentity = ptr.To("store-leader-b")
	require.NoError(t, cli.Update(ctx, moved))
	r.reportLeaderHandover(ctx, kvcb)

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

			r.reportLeaderHandover(context.Background(), tc.kvcb)
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
	r.reportLeaderHandover(ctx, kvcb)
	require.NoError(t, cli.Delete(ctx, lease))
	r.reportLeaderHandover(ctx, kvcb)

	require.NoError(t, cli.Create(ctx, electionLease("store", "store-leader-c", 0)))
	r.reportLeaderHandover(ctx, kvcb)

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
