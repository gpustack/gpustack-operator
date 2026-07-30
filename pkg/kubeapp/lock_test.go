package kubeapp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordination "k8s.io/api/coordination/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	coordinationv1 "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/typed/coordination/v1"
)

const (
	testLockNamespace = "gpustack-system"
	testLockName      = "applications.worker.gpustack.ai"
)

// testLockDuration is short enough to watch a claim stand still within a test, and long
// enough that the reads which do the watching are not the thing being timed.
const testLockDuration = 300 * time.Millisecond

// newTestClientset returns a fake client that enforces optimistic concurrency on Leases.
//
// The lock's mutual exclusion is a compare-and-swap: it writes back the object it read,
// resource version and all, so that of two processes finding the same free Lease exactly
// one update lands. The stock object tracker ignores resource versions entirely, which
// would let an update built from nothing overwrite a live claim and every test still pass.
func newTestClientset(t *testing.T) *kubefake.Clientset {
	t.Helper()

	cli := kubefake.NewSimpleClientset()

	cli.PrependReactor("create", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.CreateAction).GetObject().(*coordination.Lease).DeepCopy()
		lease.ResourceVersion = "1"

		return true, lease, cli.Tracker().Create(action.GetResource(), lease, action.GetNamespace())
	})

	cli.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordination.Lease).DeepCopy()

		stored, err := cli.Tracker().Get(action.GetResource(), action.GetNamespace(), lease.Name)
		if err != nil {
			return true, nil, err
		}
		current := stored.(*coordination.Lease).ResourceVersion
		if lease.ResourceVersion != current {
			return true, nil, kerrors.NewConflict(action.GetResource().GroupResource(), lease.Name,
				fmt.Errorf("the object has been modified: %q != %q", lease.ResourceVersion, current))
		}

		version, _ := strconv.Atoi(current)
		lease.ResourceVersion = strconv.Itoa(version + 1)

		return true, lease, cli.Tracker().Update(action.GetResource(), lease, action.GetNamespace())
	})

	return cli
}

// newTestLease builds a Lease as the given holder just claimed it.
func newTestLease(holder string) *coordination.Lease {
	return &coordination.Lease{
		ObjectMeta: meta.ObjectMeta{Namespace: testLockNamespace, Name: testLockName},
		Spec: coordination.LeaseSpec{
			HolderIdentity:       ptr.To(holder),
			LeaseDurationSeconds: ptr.To(int32(1)),
			RenewTime:            ptr.To(meta.NewMicroTime(time.Now())),
		},
	}
}

// newTestLock builds a lock over the given Lease client, timed to give up before a peer's
// claim could ever be watched long enough to take over. A case that wants the takeover
// raises the timeout.
func newTestLock(leases coordinationv1.LeaseInterface, holder string) Lock {
	return Lock{
		Leases:   leases,
		Name:     testLockName,
		Holder:   holder,
		Duration: testLockDuration,
		Timeout:  testLockDuration / 3,
	}
}

// Test_Lock_Do covers who gets to run the guarded function, which is the whole point of
// the lock: a free claim is taken, a peer's is waited out, and one seen standing still for
// its whole duration is taken over.
func Test_Lock_Do(t *testing.T) {
	cases := []struct {
		name string
		// existing is the Lease already in the cluster, if any.
		existing *coordination.Lease
		// timeout overrides the lock's own, for a case that means to outlast a claim.
		timeout time.Duration
		wantRun bool
		// wantPredecessor is the holder the guarded function must be told the claim was
		// taken from — empty where it was found free, which is a caller's evidence that
		// nothing of its predecessor is still running.
		wantPredecessor string
		wantErr         string
	}{
		{
			name:    "no lease yet is claimed",
			wantRun: true,
		},
		{
			name:     "a released lease is claimed",
			existing: newTestLease(""),
			wantRun:  true,
		},
		{
			name:            "a claim left standing is taken over",
			existing:        newTestLease("peer-0"),
			timeout:         10 * testLockDuration,
			wantRun:         true,
			wantPredecessor: "peer-0",
		},
		{
			name:     "this process's own claim is reclaimed",
			existing: newTestLease("worker-0"),
			wantRun:  true,
		},
		{
			name:     "a peer's claim is waited out",
			existing: newTestLease("peer-0"),
			wantRun:  false,
			wantErr:  "acquire lock applications.worker.gpustack.ai as worker-0: held by peer-0",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := newTestClientset(t)
			leases := cli.CoordinationV1().Leases(testLockNamespace)
			if c.existing != nil {
				_, err := leases.Create(t.Context(), c.existing, meta.CreateOptions{})
				require.NoError(t, err, "seed the lease")
			}

			lock := newTestLock(leases, "worker-0")
			if c.timeout > 0 {
				lock.Timeout = c.timeout
			}

			var ran bool
			err := lock.Do(t.Context(), func(_ context.Context, predecessor string) error {
				ran = true
				assert.Equal(t, c.wantPredecessor, predecessor, "the holder the claim was taken from")

				// The claim is held for as long as the guarded function runs, so that a peer
				// polling right now still sees it taken.
				lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
				require.NoError(t, err, "read the lease from inside the guarded function")
				assert.Equal(t, "worker-0", ptr.Deref(lease.Spec.HolderIdentity, ""))

				return nil
			})

			assert.Equal(t, c.wantRun, ran, "the guarded function ran")
			if c.wantErr != "" {
				assert.EqualError(t, err, c.wantErr)
				return
			}
			require.NoError(t, err)

			// Releasing is what lets the next replica in without waiting out the duration.
			lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
			require.NoError(t, err, "read the released lease")
			assert.Empty(t, ptr.Deref(lease.Spec.HolderIdentity, ""), "the claim is released")
		})
	}
}

// Test_Lock_DoReleasesWhatItFinished separates the two ways a guarded call can end, because
// releasing is what tells the next holder its predecessor is done.
//
// A call that failed is still a call that ended, and a claim kept after it would lock every
// peer out until it expired. A call cut short by a canceled context is not: Helm returns on
// cancellation while its apply goes on, so the claim is left standing — the next holder then
// sees a takeover, which is the truth.
func Test_Lock_DoReleasesWhatItFinished(t *testing.T) {
	cases := []struct {
		name string
		// canceled cancels the caller's context from inside the guarded function.
		cancelled bool
		// wantHolder is what the Lease must name once Do has returned.
		wantHolder string
	}{
		{name: "the guarded function fails"},
		{name: "the context is cancelled", cancelled: true, wantHolder: "worker-0"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := newTestClientset(t)
			leases := cli.CoordinationV1().Leases(testLockNamespace)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			err := newTestLock(leases, "worker-0").Do(ctx, func(ctx context.Context, _ string) error {
				if c.cancelled {
					cancel()
				}

				lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
				require.NoError(t, err, "read the lease from inside the guarded function")
				assert.Equal(t, "worker-0", ptr.Deref(lease.Spec.HolderIdentity, ""))

				return assert.AnError
			})
			assert.ErrorIs(t, err, assert.AnError, "the guarded function's error is reported")

			lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
			require.NoError(t, err, "read the lease Do left behind")
			assert.Equal(t, c.wantHolder, ptr.Deref(lease.Spec.HolderIdentity, ""))
		})
	}
}

// Test_Lock_DoRenewsWhileHeld covers the claim outliving its own duration: an install runs
// far longer than any duration short enough for a waiter to take over a dead holder's
// claim, so without renewal a peer would join it halfway through.
func Test_Lock_DoRenewsWhileHeld(t *testing.T) {
	cli := newTestClientset(t)
	leases := cli.CoordinationV1().Leases(testLockNamespace)

	var claimedAt, renewedAt time.Time
	err := newTestLock(leases, "worker-0").Do(t.Context(), func(context.Context, string) error {
		lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
		require.NoError(t, err, "read the claimed lease")
		claimedAt = lease.Spec.RenewTime.Time

		// Outlast the claim, which is renewed every third of its duration.
		time.Sleep(testLockDuration + testLockDuration/2)

		lease, err = leases.Get(t.Context(), testLockName, meta.GetOptions{})
		require.NoError(t, err, "read the renewed lease")
		renewedAt = lease.Spec.RenewTime.Time
		assert.Equal(t, "worker-0", ptr.Deref(lease.Spec.HolderIdentity, ""), "the claim is still held")

		return nil
	})
	require.NoError(t, err)
	assert.True(t, renewedAt.After(claimedAt), "the claim was renewed while the lock was held")
}

// Test_Lock_DoWaitsOutAPeerThatKeepsRenewing is the guard against judging a peer's claim
// by this process's own clock: the peer here renews constantly, so nothing but a clock
// comparison could make its claim look expired.
func Test_Lock_DoWaitsOutAPeerThatKeepsRenewing(t *testing.T) {
	cli := newTestClientset(t)
	leases := cli.CoordinationV1().Leases(testLockNamespace)

	_, err := leases.Create(t.Context(), newTestLease("peer-0"), meta.CreateOptions{})
	require.NoError(t, err, "seed the peer's claim")

	// Renew the peer's claim far faster than this process re-reads it, and back-date every
	// renewal well past the claim's duration, as a peer with a slow clock would.
	renewing, stopRenewing := context.WithCancel(t.Context())
	defer stopRenewing()
	go func() {
		for {
			select {
			case <-renewing.Done():
				return
			case <-time.After(testLockDuration / 10):
			}

			lease, err := leases.Get(renewing, testLockName, meta.GetOptions{})
			if err != nil {
				return
			}
			lease.Spec.RenewTime = ptr.To(meta.NewMicroTime(time.Now().Add(-time.Hour)))
			_, _ = leases.Update(renewing, lease, meta.UpdateOptions{})
		}
	}()

	lock := newTestLock(leases, "worker-0")
	lock.Timeout = 3 * testLockDuration

	err = lock.Do(t.Context(), func(context.Context, string) error {
		t.Error("the guarded function must not run while a peer keeps renewing its claim")
		return nil
	})
	assert.ErrorContains(t, err, "held by peer-0")
}

// Test_Lock_DoCancelsTheGuardedWorkWhenTheClaimIsLost covers the wind-down: once a peer
// holds the claim, the work this process was doing under it has to stop rather than run
// beside the peer's.
func Test_Lock_DoCancelsTheGuardedWorkWhenTheClaimIsLost(t *testing.T) {
	cli := newTestClientset(t)
	leases := cli.CoordinationV1().Leases(testLockNamespace)

	err := newTestLock(leases, "worker-0").Do(t.Context(), func(ctx context.Context, _ string) error {
		lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
		require.NoError(t, err, "read the claimed lease")

		// A peer decides the claim was abandoned and takes it.
		lease.Spec.HolderIdentity = ptr.To("peer-0")
		_, err = leases.Update(t.Context(), lease, meta.UpdateOptions{})
		require.NoError(t, err, "hand the claim to a peer")

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * testLockDuration):
			return errors.New("the guarded context outlived the claim")
		}
	})
	assert.ErrorIs(t, err, context.Canceled)

	// The claim is the peer's now, so releasing must leave it alone.
	lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
	require.NoError(t, err, "read the lease")
	assert.Equal(t, "peer-0", ptr.Deref(lease.Spec.HolderIdentity, ""), "the peer keeps the claim")
}

// Test_Lock_DoLeavesAClaimItLostToExpire covers the other way the guarded work is cut
// short: not a peer taking the claim, but this process's own renewal giving up on it.
//
// It is the case where releasing does damage. No peer has written the Lease, so it still
// names this holder and a release genuinely frees it — telling whoever comes next that its
// predecessor finished, while Helm may still be applying past the canceled context.
func Test_Lock_DoLeavesAClaimItLostToExpire(t *testing.T) {
	cli := newTestClientset(t)
	leases := cli.CoordinationV1().Leases(testLockNamespace)

	// Renewals fail while this is set, which is what makes the claim go unrenewed for its
	// whole duration without anyone else touching it.
	var stalled atomic.Bool
	cli.PrependReactor("update", "leases", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		if !stalled.Load() {
			return false, nil, nil
		}

		return true, nil, errors.New("the api server is unreachable")
	})

	err := newTestLock(leases, "worker-0").Do(t.Context(), func(ctx context.Context, _ string) error {
		stalled.Store(true)
		select {
		case <-ctx.Done():
		case <-time.After(10 * testLockDuration):
			return errors.New("the guarded context outlived the claim")
		}
		// Whatever broke the renewal is over before the claim would be released, so a
		// release attempted here would succeed. Only the gate stops it.
		stalled.Store(false)

		return nil
	})
	require.NoError(t, err)

	lease, err := leases.Get(t.Context(), testLockName, meta.GetOptions{})
	require.NoError(t, err, "read the lease")
	assert.Equal(t, "worker-0", ptr.Deref(lease.Spec.HolderIdentity, ""),
		"a claim lost mid-work is left to expire, not released")
}

// Test_Lock_DoLetsOneReplicaInAtATime is the property the whole lock exists for, run
// against a client that enforces the compare-and-swap it rests on: replicas starting
// together take their turns instead of overlapping.
func Test_Lock_DoLetsOneReplicaInAtATime(t *testing.T) {
	const replicas = 3

	cli := newTestClientset(t)
	leases := cli.CoordinationV1().Leases(testLockNamespace)

	var (
		mu        sync.Mutex
		inside    int
		overlaps  int
		admitted  int
		waitGroup sync.WaitGroup
	)

	for i := range replicas {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			lock := newTestLock(leases, fmt.Sprintf("worker-%d", i))
			// Long enough for every replica to take its turn behind the others.
			lock.Timeout = 20 * testLockDuration

			err := lock.Do(t.Context(), func(context.Context, string) error {
				mu.Lock()
				inside++
				if inside > 1 {
					overlaps++
				}
				admitted++
				mu.Unlock()

				time.Sleep(testLockDuration / 10)

				mu.Lock()
				inside--
				mu.Unlock()

				return nil
			})
			assert.NoError(t, err, "every replica gets its turn")
		}()
	}
	waitGroup.Wait()

	assert.Equal(t, 0, overlaps, "no two replicas were inside the guarded function at once")
	assert.Equal(t, replicas, admitted, "every replica ran once")
}

// Test_Lock_DoRejectsAnIncompleteLock covers what a lock refuses to do rather than attempt.
// A missing holder is the one way this lock could fail open — peers sharing an identity all
// read the claim as their own — and a missing client would fail on a nil dereference deep
// inside instead of saying what is wrong.
func Test_Lock_DoRejectsAnIncompleteLock(t *testing.T) {
	cli := newTestClientset(t)

	cases := []struct {
		name string
		lock Lock
	}{
		{
			name: "without a holder identity",
			lock: Lock{Leases: cli.CoordinationV1().Leases(testLockNamespace), Name: testLockName},
		},
		{
			name: "without a lease client",
			lock: Lock{Name: testLockName, Holder: "worker-0"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.lock.Do(t.Context(), func(context.Context, string) error {
				t.Error("the guarded function must not run under an incomplete lock")
				return nil
			})
			assert.EqualError(t, err, "lock client, name and holder are required")
		})
	}
}
