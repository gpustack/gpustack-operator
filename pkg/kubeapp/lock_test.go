package kubeapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordination "k8s.io/api/coordination/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		wantErr string
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
			name:     "a claim left standing is taken over",
			existing: newTestLease("peer-0"),
			timeout:  10 * testLockDuration,
			wantRun:  true,
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
			cli := kubefake.NewSimpleClientset()
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
			err := lock.Do(t.Context(), func(context.Context) error {
				ran = true

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

// Test_Lock_DoReleasesAfterFailure holds the release to the guarded function returning,
// not to it succeeding: a claim kept after a failed install would lock every peer out
// until it expired.
func Test_Lock_DoReleasesAfterFailure(t *testing.T) {
	cases := []struct {
		name string
		// canceled runs the guarded function under an already-canceled context, the case
		// the lock is built for: Helm keeps applying past a canceled context, so the claim
		// must outlive the cancellation and be dropped only once the call returns.
		cancelled bool
	}{
		{name: "the guarded function fails"},
		{name: "the context is cancelled", cancelled: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := kubefake.NewSimpleClientset()
			leases := cli.CoordinationV1().Leases(testLockNamespace)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			err := newTestLock(leases, "worker-0").Do(ctx, func(ctx context.Context) error {
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
			require.NoError(t, err, "read the released lease")
			assert.Empty(t, ptr.Deref(lease.Spec.HolderIdentity, ""), "the claim is released")
		})
	}
}

// Test_Lock_DoRenewsWhileHeld covers the claim outliving its own duration: an install runs
// far longer than any duration short enough for a waiter to take over a dead holder's
// claim, so without renewal a peer would join it halfway through.
func Test_Lock_DoRenewsWhileHeld(t *testing.T) {
	cli := kubefake.NewSimpleClientset()
	leases := cli.CoordinationV1().Leases(testLockNamespace)

	var claimedAt, renewedAt time.Time
	err := newTestLock(leases, "worker-0").Do(t.Context(), func(context.Context) error {
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
	cli := kubefake.NewSimpleClientset()
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

	err = lock.Do(t.Context(), func(context.Context) error {
		t.Error("the guarded function must not run while a peer keeps renewing its claim")
		return nil
	})
	assert.ErrorContains(t, err, "held by peer-0")
}

// Test_Lock_DoCancelsTheGuardedWorkWhenTheClaimIsLost covers the wind-down: once a peer
// holds the claim, the work this process was doing under it has to stop rather than run
// beside the peer's.
func Test_Lock_DoCancelsTheGuardedWorkWhenTheClaimIsLost(t *testing.T) {
	cli := kubefake.NewSimpleClientset()
	leases := cli.CoordinationV1().Leases(testLockNamespace)

	err := newTestLock(leases, "worker-0").Do(t.Context(), func(ctx context.Context) error {
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

// Test_Lock_DoRejectsAnUnidentifiedHolder guards the one way this lock fails open: peers
// sharing an identity all read the claim as their own.
func Test_Lock_DoRejectsAnUnidentifiedHolder(t *testing.T) {
	cli := kubefake.NewSimpleClientset()

	err := Lock{
		Leases: cli.CoordinationV1().Leases(testLockNamespace),
		Name:   testLockName,
	}.Do(t.Context(), func(context.Context) error {
		t.Error("the guarded function must not run without a holder identity")
		return nil
	})
	assert.EqualError(t, err, "lock name and holder are required")
}
