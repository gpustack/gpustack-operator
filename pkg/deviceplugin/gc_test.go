package deviceplugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makePodDir creates a <dir>/<uid>/c-main marker so the GC sees a pod directory.
func makePodDir(t *testing.T, dir, uid string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, uid, "c-main"), 0o777))
}

func podDirExists(dir, uid string) bool {
	_, err := os.Stat(filepath.Join(dir, uid))
	return err == nil
}

func TestPodDirGC_RemovesAfterThreeConsecutiveMisses(t *testing.T) {
	dir := t.TempDir()
	makePodDir(t, dir, "alive")
	makePodDir(t, dir, "dead")

	// Seeds both UUIDs from disk.
	gc := newPodDirGC(dir)

	// "dead" missing from the live set; "alive" present. Two misses: not yet removed.
	gc.reconcile([]string{"alive"})
	assert.True(t, podDirExists(dir, "dead"), "removed after 1 miss")
	gc.reconcile([]string{"alive"})
	assert.True(t, podDirExists(dir, "dead"), "removed after 2 misses")

	// Third consecutive miss: removed. "alive" untouched.
	gc.reconcile([]string{"alive"})
	assert.False(t, podDirExists(dir, "dead"), "should be removed after 3 misses")
	assert.True(t, podDirExists(dir, "alive"), "live pod dir must be kept")
}

func TestPodDirGC_MissStreakResetsWhenPodReappears(t *testing.T) {
	dir := t.TempDir()
	makePodDir(t, dir, "flap")
	gc := newPodDirGC(dir)

	gc.reconcile(nil)              // miss 1 (empty live set)
	gc.reconcile([]string{})       // miss 2
	gc.reconcile([]string{"flap"}) // reappears -> streak resets
	gc.reconcile(nil)              // miss 1 again
	gc.reconcile(nil)              // miss 2
	assert.True(t, podDirExists(dir, "flap"), "non-consecutive misses must not remove")

	gc.reconcile(nil) // miss 3 consecutive -> removed
	assert.False(t, podDirExists(dir, "flap"))
}

func TestPodDirGC_EmptyLiveSetReclaimsAll(t *testing.T) {
	dir := t.TempDir()
	makePodDir(t, dir, "a")
	makePodDir(t, dir, "b")
	gc := newPodDirGC(dir)

	for i := 0; i < podDirGCMaxMisses; i++ {
		gc.reconcile(nil)
	}
	assert.False(t, podDirExists(dir, "a"))
	assert.False(t, podDirExists(dir, "b"))
}

func TestPodDirGC_TracksDirsAppearingAfterSeed(t *testing.T) {
	dir := t.TempDir()
	gc := newPodDirGC(dir) // empty at seed time

	// A pod dir appears later while its pod is live: kept, miss count stays 0.
	makePodDir(t, dir, "late")
	gc.reconcile([]string{"late"})
	gc.reconcile([]string{"late"})
	gc.reconcile([]string{"late"})
	assert.True(t, podDirExists(dir, "late"))

	// Once it dies, three consecutive misses remove it.
	gc.reconcile(nil)
	gc.reconcile(nil)
	gc.reconcile(nil)
	assert.False(t, podDirExists(dir, "late"))
}

// A failed removal must keep the miss entry so the next reconcile retries, rather
// than dropping it (which would reset the streak and leave the directory orphaned).
func TestPodDirGC_RetriesWhenRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the directory permission used to force a removal failure")
	}
	dir := t.TempDir()
	makePodDir(t, dir, "stuck")
	gc := newPodDirGC(dir)

	// Drop write on the pod dir so RemoveAll of its child fails (EACCES).
	target := filepath.Join(dir, "stuck")
	require.NoError(t, os.Chmod(target, 0o555))
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })

	// Three consecutive misses trigger a removal attempt that fails.
	for i := 0; i < podDirGCMaxMisses; i++ {
		gc.reconcile(nil)
	}
	assert.True(t, podDirExists(dir, "stuck"), "dir must remain when removal fails")
	_, tracked := gc.misses["stuck"]
	assert.True(t, tracked, "miss entry must be retained so the next reconcile retries")

	// Once removable again, the next reconcile retries and reclaims it.
	require.NoError(t, os.Chmod(target, 0o755))
	gc.reconcile(nil)
	assert.False(t, podDirExists(dir, "stuck"), "dir reclaimed once removal succeeds")
}
