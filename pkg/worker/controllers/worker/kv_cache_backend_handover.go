package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	coordination "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// kvCacheBackendEventLeaderHandover is recorded when the lease this backend elects through changes
// hands.
const kvCacheBackendEventLeaderHandover = "KVCacheLeaderHandover"

// leaseHolders remembers which replica each backend's lease named the last time this operator
// looked, so a change can be told from a renewal.
//
// It is IN MEMORY and deliberately not a field. A handover is a property of the transition, and a
// field recording it would be a copy of a value the Lease already publishes, kept only to be looked
// at -- which is the test three candidate fields have already failed in this design. The cost is
// stated rather than hidden: a handover that happens while this operator is restarting produces no
// Event, because nothing here observed the before. The Lease's own leaseTransitions is what survives
// that, and it is a count rather than an event.
//
// The held value NEVER leaves this map. Which replica holds the lease is a question this API does
// not answer, so the identity is read to compare and the Event says only that it moved.
//
// Its ZERO VALUE is usable, so a reconciler built without SetupController -- which is every test
// helper in this package -- observes handovers the same way the real one does rather than taking a
// different path through the code under test.
type leaseHolders struct {
	mu sync.Mutex
	by map[string]string
}

// observe records the holder now and reports whether it MOVED, which is not the same as differing
// from the zero value: the first sighting of a backend establishes a baseline and reports nothing,
// because a first sighting is not a handover.
//
// THE BASELINE IS THE LAST NON-EMPTY HOLDER, not the last value seen, because a lease passes through
// an empty holder on its way from one replica to the next and that intermediate reading is not a
// state anybody held. Storing it would consume the handover in two halves -- one pass sees a holder
// leaving and the next sees a holder arriving, and with the empty value remembered between them
// neither half has a previous holder to compare against, so a genuine failover reports nothing at
// all. Skipping the write costs nothing in the other direction: a release followed by the SAME
// replica reacquiring compares equal and stays silent, which is what it should do.
func (h *leaseHolders) observe(backend, holder string) (moved bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.by == nil {
		h.by = make(map[string]string)
	}
	previous, seen := h.by[backend]
	if holder != "" {
		h.by[backend] = holder
	}

	return seen && holder != "" && previous != holder
}

// forget drops a backend, so a deleted one does not hold its last holder for the life of the
// process and a backend recreated under the same name starts from no baseline rather than from a
// stale one.
func (h *leaseHolders) forget(backend string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.by, backend)
}

// leaderLease reads ONCE, for both reporters below, the lease the leader elects through.
//
// They ask different questions of the same object -- did it change hands, and does it name anybody
// at all -- and a Get in each is a duplicate per-object round-trip on a path that runs on every
// pass. The read is passed down rather than cached on the reconciler, so nothing outlives the pass
// that took it.
//
// IT READS ONLY WHEN A REPORTER WILL LOOK. Each of them returns early on a backend that elects
// nothing, and the condition here is the union of those two: a backend with no high availability and
// at most one leader replica pays for no read, because neither reporter would reach the answer.
//
// A NotFound is returned as the error rather than flattened to a nil lease, because the two readers
// treat it differently: one forgets its baseline, the other carries on to ask whether a leader is
// even ready yet.
func (r *KVCacheBackendReconciler) leaderLease(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) (*coordination.Lease, error) {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil ||
		(managed.Leader.HighAvailability == nil && mooncake.LeaderReplicas(managed.Leader) <= 1) {
		return nil, nil
	}

	lease := new(coordination.Lease)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{
		Name:      mooncake.LeaderObjectName(kvcb),
		Namespace: kuberess.SystemNamespaceName,
	}, lease); err != nil {
		return nil, err
	}

	return lease, nil
}

// reportLeaderHandover records one Event when the lease has changed hands since the last pass.
//
// It answers "did service just move", which is the first diagnostic question after a latency spike
// and has no answer anywhere a user is already looking. It does NOT answer "how many times last
// night": Events are collected on the cluster's event TTL, one hour by default, and the durable
// count is the Lease's own leaseTransitions.
//
// REQUIRED: it names no replica. Which one holds the lease is a Non-Goal of this design, answered
// from the Kubernetes side by the label the store puts on its own pod. What moved is a property of
// the handover; who holds it is a property of a replica, and only the first is reported here.
// IT TAKES NO CONTEXT, because it issues no call: the lease is read once by the caller and handed
// to both reporters, and recording an Event goes through the recorder rather than the API.
func (r *KVCacheBackendReconciler) reportLeaderHandover(
	kvcb *workercore.KVCacheBackend, lease *coordination.Lease, leaseErr error,
) {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil || managed.Leader.HighAvailability == nil {
		return
	}

	if leaseErr != nil || lease == nil {
		// A missing lease is the ordinary state of a backend whose election has not started -- one
		// replica, or a leader still coming up. Forgetting rather than keeping the last holder is
		// what stops the first campaign after a gap from reading as a handover.
		if leaseErr == nil || kerrors.IsNotFound(leaseErr) {
			r.leaseHolders.forget(kvcb.Name)
		}
		return
	}

	if !r.leaseHolders.observe(kvcb.Name, leaseHolderIdentity(lease)) {
		return
	}

	// Normal rather than Warning. Every rolling update of a high-availability backend produces one,
	// so a warning here would arrive on every ordinary operation and teach a reader to filter the
	// series that carries the unplanned case too.
	var transitions int32
	if lease.Spec.LeaseTransitions != nil {
		transitions = *lease.Spec.LeaseTransitions
	}
	r.recordNormal(kvcb, kvCacheBackendEventLeaderHandover,
		"the store's leader moved to another replica; the lease has recorded %d handovers in total, "+
			"and the cache it serves is whatever the new leader could restore", transitions)
}

// reportElectionObserved answers whether an election is actually happening, for a backend that asked
// for one.
//
// The question it exists for is an image too old to elect. The Kubernetes leadership backend does
// not exist below a certain store version, and a backend pinned to such an image renders the
// election flags, starts, serves from one replica and reports Ready -- while nothing elects. Reading
// the version out of the image tag is not a check: a tag is a name, and the one users pin most often
// is a digest or a local rebuild.
//
// What IS observable from the Kubernetes side is the artifact of the election. A lease with a holder
// means one happened. A lease with none, under a leader that is ready, means one did not.
//
// REQUIRED: the message states what was OBSERVED and then the things that produce it, in that order,
// and never names a cause. A holderless lease is equally what a missing role binding and a process
// still starting look like, and a message naming the image would be wrong in both of those.
func (r *KVCacheBackendReconciler) reportElectionObserved(
	ctx context.Context, kvcb, holder *workercore.KVCacheBackend,
	lease *coordination.Lease, leaseErr error,
) {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil || mooncake.LeaderReplicas(managed.Leader) <= 1 {
		// Below two replicas there is nothing to elect between, so there is no lease to be the
		// artifact of anything. Dropped rather than left, because the status this pass builds starts
		// as a copy of the observed one -- see the snapshot condition for the same reasoning.
		holder.Status.Conditions = slices.DeleteFunc(holder.Status.Conditions,
			func(c gpustack.Condition) bool {
				return c.Type == string(KVCacheBackendConditionElectionObserved)
			})
		return
	}

	leaseName := mooncake.LeaderObjectName(kvcb)

	switch {
	case leaseErr != nil && !kerrors.IsNotFound(leaseErr):
		KVCacheBackendConditionElectionObserved.Unknown(holder, "LeaseUnreadable", fmt.Sprintf(
			"the lease %q could not be read: %v", leaseName, clipFaultDetail(leaseErr.Error())))

	case leaseErr == nil && lease != nil && leaseHolderIdentity(lease) != "":
		KVCacheBackendConditionElectionObserved.True(holder, "Electing", fmt.Sprintf(
			"the lease %q names a holder, so an election has taken place", leaseName))

	case !r.leaderPodIsReady(ctx, kvcb):
		// The same observation with no information in it. A leader that has not become ready has
		// not had the chance to campaign, so the absence says nothing about whether it can.
		KVCacheBackendConditionElectionObserved.Unknown(holder, "LeaderStarting", fmt.Sprintf(
			"no replica of the leader is ready yet, so the lease %q says nothing about whether an "+
				"election can happen", leaseName))

	default:
		KVCacheBackendConditionElectionObserved.False(holder, "NoHolder", fmt.Sprintf(
			"a replica of the leader is ready while the lease %q names no holder. A store image "+
				"built without the Kubernetes leadership backend, a role binding for it that has not "+
				"been applied, and a process that is serving without having finished its first "+
				"campaign all produce this", leaseName))
	}
}

// enqueueKVCacheBackendWhenLeaseChanged maps a Lease back to the backend that elects through it.
//
// The Lease carries no resource note and no owner reference, because THE STORE creates it rather
// than this operator -- so the only link is the name, which is the backend's own with the leader
// suffix. That is a guess until the backend is read back, which is what this does: a lease belonging
// to something else that happens to fit the pattern names no backend and enqueues nothing.
func (r *KVCacheBackendReconciler) enqueueKVCacheBackendWhenLeaseChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	name, ok := strings.CutSuffix(obj.GetName(), mooncake.LeaderObjectNameSuffix)
	if !ok || name == "" {
		return nil
	}

	kvcb := new(workercore.KVCacheBackend)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: name}, kvcb); err != nil {
		return nil
	}
	if kvcb.Spec.Connection.Managed == nil ||
		kvcb.Spec.Connection.Managed.Leader.HighAvailability == nil {
		return nil
	}

	return []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: name}}}
}

// kvCacheBackendLeaseHolderChanged passes only the events that could BE a handover.
//
// This is a load-bearing filter rather than noise reduction. A lease is renewed every few seconds
// for the life of every leader, and each update reaching the reconciler would cost that backend
// three sequential HTTP reads against its admin surface -- so an unfiltered watch here would turn a
// steady cluster into a permanent load on every store in it.
//
// The holder is compared rather than any other field because it is the only one a handover moves:
// renewTime moves on every renewal and acquireTime moves with the holder.
func kvCacheBackendLeaseHolderChanged(old, updated ctrlcli.Object) bool {
	if updated.GetNamespace() != kuberess.SystemNamespaceName {
		return false
	}

	oldLease, ok := old.(*coordination.Lease)
	if !ok {
		return false
	}
	newLease, ok := updated.(*coordination.Lease)
	if !ok {
		return false
	}

	return leaseHolderIdentity(oldLease) != leaseHolderIdentity(newLease)
}

func leaseHolderIdentity(lease *coordination.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// recordNormal publishes one ordinary event against obj, with the same nil-recorder guard
// recordWarning carries and for the same reason.
func (r *KVCacheBackendReconciler) recordNormal(
	obj ctrlcli.Object, reason, format string, args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, core.EventTypeNormal, reason, format, args...)
}
