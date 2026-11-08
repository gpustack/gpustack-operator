package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openSecondHandle is a second Store handle on the same root, the way two
// processes share the node cache: the claims flock is what they see of each
// other.
func openSecondHandle(t *testing.T, s *Store) *Store {
	t.Helper()
	other, err := Open(s.root)
	require.NoError(t, err)

	return other
}

// stageView writes a final view directory the way a builder promotes it.
func stageView(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "view.json"), []byte("{}"), 0o444))
	require.NoError(t, os.Chtimes(dir, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))

	return dir
}

// TestAcquireWaitsForTheClaimLock pins the acquisition boundary: Acquire
// takes the claims flock, so a removal that holds it orders against the
// reference; a bare WriteRef does not, which is why mount paths must not
// use it where the ordering matters.
func TestAcquireWaitsForTheClaimLock(t *testing.T) {
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	other := openSecondHandle(t, s)

	unlock, err := s.lockClaims(context.Background())
	require.NoError(t, err)

	acquired := make(chan error, 1)
	go func() {
		acquired <- other.Acquire(context.Background(), Ref{VolumeID: "csi-later", Hex: strings.Repeat("a", 64)})
	}()
	select {
	case err := <-acquired:
		t.Fatalf("Acquire proceeded while another handle held the claims lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	wrote := make(chan error, 1)
	go func() {
		wrote <- other.WriteRef(Ref{VolumeID: "csi-bare", Hex: strings.Repeat("a", 64)})
	}()
	select {
	case err := <-wrote:
		require.NoError(t, err, "a bare write does not take the flock, which is the gap mounts must not rely on")
	case <-time.After(2 * time.Second):
		t.Fatal("a bare WriteRef blocked on the claims lock")
	}

	unlock()
	select {
	case err := <-acquired:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not proceed once the lock was released")
	}
	ref, exists, err := other.ReadRef("csi-later")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, strings.Repeat("a", 64), ref.Hex)
}

// TestRemoveUnreferencedViewKeepsAnAcquiredView runs the two orders across
// two handles: whatever acquires first wins, and neither order ends with a
// removed view that a live reference protects.
func TestRemoveUnreferencedViewKeepsAnAcquiredView(t *testing.T) {
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	other := openSecondHandle(t, s)
	root := s.ViewsRoot()
	require.NoError(t, os.MkdirAll(root, 0o755))
	hex := strings.Repeat("a", 64)

	t.Run("acquire first, so the removal keeps the view", func(t *testing.T) {
		dir := stageView(t, root, strings.Repeat("a", 64))
		require.NoError(t, other.Acquire(context.Background(), Ref{VolumeID: "csi-mount-1", Hex: hex}))
		gone, err := s.RemoveUnreferencedView(context.Background(), dir, hex)
		require.NoError(t, err)
		assert.False(t, gone, "a reference registered under the claim lock keeps the view")
		assert.DirExists(t, dir)
	})

	t.Run("removal first, so the acquisition refuses and undoes its reference", func(t *testing.T) {
		dir := stageView(t, root, strings.Repeat("b", 64))
		gone, err := s.RemoveUnreferencedView(context.Background(), dir, strings.Repeat("b", 64))
		require.NoError(t, err)
		require.True(t, gone, "an unreferenced view goes")
		// The acquisition the removal beat still registers, and the caller
		// sees the view is gone and undoes the reference.
		require.NoError(t, other.Acquire(context.Background(), Ref{VolumeID: "csi-mount-2", Hex: strings.Repeat("b", 64)}))
		_, err = os.Stat(dir)
		assert.True(t, os.IsNotExist(err), "the removal's retirement stands")
		require.NoError(t, other.RemoveRef("csi-mount-2"))
	})

	refs, err := other.Refs()
	require.NoError(t, err)
	assert.Len(t, refs, 1, "only the live mount's reference remains")
}

// TestAcquireCancelledWritesNothing pins the cancellation contract Claim
// already has: a caller whose context is done registers nothing.
func TestAcquireCancelledWritesNothing(t *testing.T) {
	s, err := Open(t.TempDir())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.Acquire(ctx, Ref{VolumeID: "csi-late", Hex: strings.Repeat("a", 64)})
	require.Error(t, err)
	_, exists, err := s.ReadRef("csi-late")
	require.NoError(t, err)
	assert.False(t, exists, "a cancelled acquisition registers nothing")
	// The locks are free: a following claim proceeds.
	published, err := s.Claim(context.Background(), Ref{VolumeID: "csi-next", Hex: strings.Repeat("a", 64)})
	require.NoError(t, err)
	assert.False(t, published, "nothing was published in this fixture, so the claim refuses as usual")
}

// TestClaimIsNotBlockedByBulkTrashDeletion pins the lock span: the locks
// end at the atomic retirement, so an unrelated claim proceeds while the
// trash is still being unlinked.
func TestClaimIsNotBlockedByBulkTrashDeletion(t *testing.T) {
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	other := openSecondHandle(t, s)
	root := s.ViewsRoot()
	require.NoError(t, os.MkdirAll(root, 0o755))
	dir := stageView(t, root, strings.Repeat("a", 64))
	// Enough files that the unlink takes measurable time behind the hook.
	for i := range 200 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", i)), []byte("x"), 0o444))
	}

	release := make(chan struct{})
	original := beforeViewRemove
	beforeViewRemove = func() { <-release }
	t.Cleanup(func() { beforeViewRemove = original })

	removed := make(chan error, 1)
	go func() {
		_, err := s.RemoveUnreferencedView(context.Background(), dir, strings.Repeat("a", 64))
		removed <- err
	}()
	// The retirement is done once the view path is gone; the deletion is
	// still parked behind the hook.
	require.Eventually(t, func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	}, 2*time.Second, 2*time.Millisecond)

	acquired := make(chan error, 1)
	go func() {
		acquired <- other.Acquire(context.Background(), Ref{VolumeID: "csi-other", Hex: strings.Repeat("b", 64)})
	}()
	select {
	case err := <-acquired:
		require.NoError(t, err, "an unrelated claim proceeds while the trash is being unlinked")
	case <-time.After(2 * time.Second):
		t.Fatal("the bulk deletion still holds the claim locks")
	}

	close(release)
	require.NoError(t, <-removed)
}
