package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdClaims takes both claim locks the way Claim, Rebuild and RemoveUnreferenced do and returns
// the release. A test that schedules release later makes a waiter contend with a holder.
func holdClaims(t *testing.T, s *Store) func() {
	t.Helper()
	require.NoError(t, s.lockClaimMu(context.Background()))
	unlock, err := s.lockClaims(context.Background())
	require.NoError(t, err)
	return func() {
		unlock()
		s.unlockClaimMu()
	}
}

// claimResult carries what a claim goroutine reports, so a waiting test never asserts from a
// goroutine it does not own.
type claimResult struct {
	claimed bool
	err     error
}

// claimInGoroutine runs one claim the way a mount call does and reports its outcome.
func claimInGoroutine(s *Store, ctx context.Context, ref Ref) <-chan claimResult {
	done := make(chan claimResult, 1)
	go func() {
		claimed, err := s.Claim(ctx, ref)
		done <- claimResult{claimed: claimed, err: err}
	}()

	return done
}

func claimRef(volumeID string) Ref {
	return Ref{VolumeID: volumeID, TargetPath: testTarget("uid-1", "gpustack-model"), Hex: testHex}
}

func TestClaimWaitHonorsDeadlineWhileClaimLockHeld(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	release := holdClaims(t, s)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	select {
	case got := <-claimInGoroutine(s, ctx, claimRef("csi-deadline")):
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
		assert.False(t, got.claimed)
	case <-time.After(10 * time.Second):
		t.Fatal("a deadlined claim kept waiting for the claim lock holder")
	}
	_, found, err := s.ReadRef("csi-deadline")
	require.NoError(t, err)
	assert.False(t, found, "a claim that gave up records nothing")
}

func TestClaimWaitHonorsDeadlineWhileFileLockHeld(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	// The holder of the file lock alone, the way another process holds it.
	unlock, err := s.lockClaims(context.Background())
	require.NoError(t, err)
	go func() {
		time.Sleep(300 * time.Millisecond)
		unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	select {
	case got := <-claimInGoroutine(s, ctx, claimRef("csi-file")):
		require.ErrorIs(t, got.err, context.DeadlineExceeded, "a deadlined claim must not take the lock the holder released")
		assert.False(t, got.claimed)
	case <-time.After(10 * time.Second):
		t.Fatal("a deadlined claim kept waiting for the file lock holder")
	}

	// Waiting does not poison the lock: the same call with room to wait claims once the tree is free.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	got := <-claimInGoroutine(s, ctx2, claimRef("csi-file"))
	require.NoError(t, got.err)
	assert.True(t, got.claimed, "a claim succeeds after the holder releases")
}

func TestCanceledClaimRecordsNothing(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	claimed, err := s.Claim(ctx, claimRef("csi-cancel"))
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, claimed)
	_, found, err := s.ReadRef("csi-cancel")
	require.NoError(t, err)
	assert.False(t, found)
	assert.True(t, s.IsPublished(testHex))
}

func TestCollectionDoesNotRemoveALiveClaim(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	release := holdClaims(t, s)

	done := claimInGoroutine(s, context.Background(), claimRef("csi-live"))
	select {
	case got := <-done:
		t.Fatalf("the claim proceeded while the holder kept the locks: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.True(t, got.claimed)
	case <-time.After(5 * time.Second):
		t.Fatal("the claim never proceeded after the holder released")
	}

	removed, err := s.RemoveUnreferenced(t.Context(), testHex)
	require.NoError(t, err)
	assert.False(t, removed, "a live claim keeps its tree")
	assert.True(t, s.IsPublished(testHex))
	_, found, err := s.ReadRef("csi-live")
	require.NoError(t, err)
	assert.True(t, found)
}

func TestCanceledWaitLeavesNoLockOrDescriptor(t *testing.T) {
	s := openTestStore(t)
	other, err := Open(s.Root())
	require.NoError(t, err)
	holder, err := other.lockClaims(context.Background())
	require.NoError(t, err)

	before := countOpenFiles(t)
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, err := s.lockClaims(ctx)
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	after := countOpenFiles(t)
	holder()
	assert.LessOrEqual(t, after, before+5, "each canceled wait must close its own lock descriptor")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := s.lockClaims(ctx)
	require.NoError(t, err, "a canceled waiter must not retain the claim lock")
	unlock()
}

func TestCanceledCallerDoesNotTakeAFreeClaimLock(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	unlock, err := s.lockClaims(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, unlock, "a caller that gave up hands back no unlock")
	_, found, err := s.ReadRef("csi-free")
	require.NoError(t, err)
	assert.False(t, found)

	// Nothing was taken: the next claim path over the same root acquires at once.
	other, err := Open(s.Root())
	require.NoError(t, err)
	unlock, err = other.lockClaims(context.Background())
	require.NoError(t, err, "a canceled caller must not retain the free claim lock")
	unlock()
}

func TestDeadlineExpiredBetweenTheLocksDoesNotClaim(t *testing.T) {
	// The holder keeps the caller between the claim admission and the file lock; the deadline
	// expires while it waits and the admission frees afterwards. Whichever select arm the caller's
	// own race lands on, it must answer the deadline and record nothing.
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	release := holdClaims(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := claimInGoroutine(s, ctx, claimRef("csi-seam"))
	time.Sleep(100 * time.Millisecond)
	release()
	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("a deadlined claim proceeded after the holder released")
	}
	_, found, err := s.ReadRef("csi-seam")
	require.NoError(t, err)
	assert.False(t, found, "a claim that gave up between the locks records nothing")
}

func TestDeadlineExpiredWhileWaitingTakesNoFreeLockAfterwards(t *testing.T) {
	// The holder of the file lock alone frees it after the caller's deadline has expired: a try
	// that catches the lock free then must release it again and answer the deadline, not begin
	// the ledger work.
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	unlock, err := s.lockClaims(context.Background())
	require.NoError(t, err)
	go func() {
		time.Sleep(300 * time.Millisecond)
		unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := claimInGoroutine(s, ctx, claimRef("csi-after-wait"))
	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("a deadlined claim took the lock the holder freed afterwards")
	}
	_, found, err := s.ReadRef("csi-after-wait")
	require.NoError(t, err)
	assert.False(t, found)

	// The lock the try caught is not kept: another claim path over the same root acquires at once.
	other, err := Open(s.Root())
	require.NoError(t, err)
	release, err := other.lockClaims(context.Background())
	require.NoError(t, err, "a try that gave the lock up must not retain it")
	release()
}

func TestRebuildWaitHonorsDeadlineAndWritesNothing(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	require.NoError(t, s.WriteRef(claimRef("csi-kept")))
	before, err := s.Refs()
	require.NoError(t, err)
	release := holdClaims(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rebuilt, err := s.Rebuild(ctx, nil, testDevice)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, rebuilt.References())
	after, err := s.Refs()
	require.NoError(t, err)
	assert.Equal(t, before, after, "a rebuild that gave up rewrote nothing")
	release()
}

func TestCollectionWaitHonorsDeadlineAndKeepsTheTree(t *testing.T) {
	s := openTestStore(t)
	publishTestTree(t, s, testHex)
	release := holdClaims(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	removed, err := s.RemoveUnreferenced(ctx, testHex)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, removed)
	assert.True(t, s.IsPublished(testHex), "a collection that gave up waiting renamed nothing")
	release()
}

func countOpenFiles(t *testing.T) int {
	t.Helper()
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err == nil {
			return len(entries)
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			continue
		}
	}
	t.Skip("this platform lists no open file descriptors")

	return 0
}
