package metadata

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestRecoveryPreservesAHeldStagingAndReclaimsReleasedOne(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStaging(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stage.Dir(), "view.bin"), []byte("partial"), 0o600))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, stage.Dir(), "a staging whose lock its builder holds survives recovery")
	assert.FileExists(t, stage.LockPath())
	stage.Release()
	assert.NoDirExists(t, stage.Dir())
	assert.NoFileExists(t, stage.LockPath())
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.NoDirExists(t, stage.Dir())
	assert.NoFileExists(t, stage.LockPath())
}

func TestAbandonedStagingIsReclaimed(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStaging(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stage.Dir(), "view.bin"), []byte("partial"), 0o600))
	// Abandon is the crashed-builder state: the directory, the lock file,
	// and the published marker stay behind with the lock free, so recovery
	// can take it, which is all the proof it needs.
	stage.Abandon()
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.NoDirExists(t, stage.Dir())
	assert.NoFileExists(t, stage.LockPath())
}

func TestStagingSurvivesOnlyWhileItsBuilderLives(t *testing.T) {
	if os.Getenv("GPUSTACK_METADATA_STAGING_TEST_CHILD") != "" {
		stagingHoldChild()
		return
	}
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStagingSurvivesOnlyWhileItsBuilderLives$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"GPUSTACK_METADATA_STAGING_TEST_CHILD=1",
		"GPUSTACK_METADATA_STAGING_TEST_ROOT="+root,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// The child answers only once its staging exists and its lock is held.
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	stagingDir := strings.TrimSpace(line)
	lockPath := stagingDir + stagingLockSuffix
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, stagingDir,
		"another process's held staging lock proves its tree active")
	assert.FileExists(t, lockPath)
	// Killing the builder frees the lock without any release step: the same
	// staging is now provably abandoned, with no elapsed-time or naming
	// guesswork.
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait(), "the builder is killed on purpose")
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.NoDirExists(t, stagingDir)
	assert.NoFileExists(t, lockPath)
}

// stagingHoldChild creates a staging under the given root, reports its
// directory, and blocks until the parent closes or kills it: exiting
// without Release is exactly the crashed-builder state under test.
func stagingHoldChild() {
	root := os.Getenv("GPUSTACK_METADATA_STAGING_TEST_ROOT")
	stage, err := NewStaging(root)
	if err != nil {
		os.Exit(3)
	}
	fmt.Println(stage.Dir())
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = stage
	os.Exit(0)
}

func TestOrphanStagingLockIsReclaimedOnlyWhenFree(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStaging(root)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(stage.Dir()))
	// Held: a builder that has not created its directory yet is alive.
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.FileExists(t, stage.LockPath())
	// Free: the builder died before the directory existed.
	require.NoError(t, stage.lock.Close())
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.NoFileExists(t, stage.LockPath())
}

func TestStagingWithoutALockIsPreserved(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, stagingPrefix+"legacy")
	require.NoError(t, os.MkdirAll(legacy, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "view.bin"), []byte("partial"), 0o600))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, legacy,
		"staging without an ownership lock cannot be proven inactive")
}

func TestBuildersEachHoldTheirOwnStaging(t *testing.T) {
	root := t.TempDir()
	first, err := NewStaging(root)
	require.NoError(t, err)
	defer first.Release()
	second, err := NewStaging(root)
	require.NoError(t, err)
	defer second.Release()
	require.NotEqual(t, first.Dir(), second.Dir())
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, first.Dir())
	assert.DirExists(t, second.Dir())
}

func TestReleaseKeepsTheLockWhenTheTreeCannotBeRemoved(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStaging(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stage.Dir(), "partial"), []byte("data"), 0o600))
	require.NoError(t, os.Chmod(stage.Dir(), 0o500))
	t.Cleanup(func() { _ = os.Chmod(stage.Dir(), 0o700) })
	stage.Release()
	require.DirExists(t, stage.Dir(), "a failed tree removal keeps the staging")
	require.FileExists(t, stage.LockPath(),
		"a failed tree removal keeps the lock path, so the residue stays owned")
	// The descriptor is closed even on failure: the lock can be taken, and
	// that is what lets recovery prove the residue inactive later.
	probe, err := os.Open(stage.LockPath())
	require.NoError(t, err)
	require.NoError(t, flockExclusive(probe))
	require.NoError(t, probe.Close())
	// Once the removal can succeed, the same residue is reclaimed with its
	// lock, and Release stays idempotent after all of it.
	require.NoError(t, os.Chmod(stage.Dir(), 0o700))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.NoDirExists(t, stage.Dir())
	assert.NoFileExists(t, stage.LockPath())
	require.NotPanics(t, func() { stage.Release() })
	assert.NoDirExists(t, stage.Dir())
	assert.NoFileExists(t, stage.LockPath())
}

func TestReclaimRejectsReplacedLockSymlinks(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStaging(root)
	require.NoError(t, err)
	defer stage.Release()
	require.NoError(t, os.WriteFile(filepath.Join(stage.Dir(), "partial"), []byte("data"), 0o600))
	unrelated := filepath.Join(root, "unrelated-lock")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))
	saved := stage.LockPath() + ".saved"
	require.NoError(t, os.Rename(stage.LockPath(), saved))
	require.NoError(t, os.Symlink(unrelated, stage.LockPath()))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, stage.Dir(),
		"a lock path replaced by a symlink is not a protocol lock; the staging survives")
	info, err := os.Lstat(stage.LockPath())
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the symlink itself is preserved")
	assert.FileExists(t, saved, "the renamed real lock is preserved")
	assert.FileExists(t, unrelated)
	b, err := os.ReadFile(unrelated)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(b), "the unrelated lock target is untouched")
	// With the true lock path restored, the sweep acts on it again: the
	// builder of this staging still holds it, so it survives.
	require.NoError(t, os.Rename(saved, stage.LockPath()))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, stage.Dir())
}

func TestReclaimPreservesNonRegularLockCandidates(t *testing.T) {
	root := t.TempDir()
	fifoLock := filepath.Join(root, stagingPrefix+"fifo"+stagingLockSuffix)
	require.NoError(t, unix.Mkfifo(fifoLock, 0o600))
	dirLock := filepath.Join(root, stagingPrefix+"dir"+stagingLockSuffix)
	require.NoError(t, os.Mkdir(dirLock, 0o700))
	require.NoError(t, ReclaimAbandonedStaging(root),
		"the sweep never blocks on, or deletes, a special-file lock candidate")
	assert.FileExists(t, fifoLock, "a FIFO at a lock path is preserved")
	assert.DirExists(t, dirLock, "a directory at a lock path is preserved")
}

func TestHeldLockIdentityIsCheckedAgainstThePath(t *testing.T) {
	root := t.TempDir()
	held := filepath.Join(root, "held")
	f, err := os.OpenFile(held, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	require.NoError(t, lockStillAtPath(f, held))
	other := filepath.Join(root, "other")
	require.NoError(t, os.WriteFile(other, []byte{}, 0o600))
	require.Error(t, lockStillAtPath(f, other),
		"a held file does not back a different path")
	require.Error(t, lockStillAtPath(f, root),
		"a directory is not the held regular file")
	target := filepath.Join(root, "target")
	require.NoError(t, os.WriteFile(target, []byte{}, 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(root, "link")))
	require.Error(t, lockStillAtPath(f, filepath.Join(root, "link")),
		"a symlinked path does not back the held file")
	require.NoError(t, os.Symlink(held, filepath.Join(root, "alias")))
	require.Error(t, lockStillAtPath(f, filepath.Join(root, "alias")),
		"a symlink pointing at the held file itself is still not the directory entry")
}

func TestNewStagingPreservesAPreExistingDirectoryWhenTheNameCollides(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, stagingPrefix+"fixed")
	require.NoError(t, os.Mkdir(existing, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(existing, "theirs"), []byte("keep"), 0o600))
	_, err := newStagingNamed(root, stagingPrefix, "fixed")
	require.ErrorIs(t, err, errStagingNameTaken)
	assert.DirExists(t, existing,
		"a directory that predates the builder survives the failed create")
	b, err := os.ReadFile(filepath.Join(existing, "theirs"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(b), "the pre-existing directory's contents are untouched")
	_, err = os.Lstat(existing + stagingLockSuffix)
	assert.True(t, os.IsNotExist(err),
		"the failed create leaves no lock whose marker could ever authorize deleting the preserved directory")
}

func TestNewStagingLeavesAPlantedLockUntouchedWhenTheNameIsTaken(t *testing.T) {
	root := t.TempDir()
	planted := filepath.Join(root, stagingPrefix+"taken"+stagingLockSuffix)
	require.NoError(t, os.WriteFile(planted, []byte("not mine"), 0o600))
	_, err := newStagingNamed(root, stagingPrefix, "taken")
	require.ErrorIs(t, err, errStagingNameTaken)
	b, err := os.ReadFile(planted)
	require.NoError(t, err)
	assert.Equal(t, "not mine", string(b),
		"a foreign file at a lock path is never dropped by a colliding builder")
}

func TestReclaimRefusesAReplacedRegularLockWhileTheBuilderLives(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStaging(root)
	require.NoError(t, err)
	defer stage.Release()
	require.NoError(t, os.WriteFile(filepath.Join(stage.Dir(), "partial"), []byte("data"), 0o600))
	saved := stage.LockPath() + ".saved"
	require.NoError(t, os.Rename(stage.LockPath(), saved))
	require.NoError(t, os.WriteFile(stage.LockPath(), []byte("unrelated regular file"), 0o600))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, stage.Dir(),
		"a new regular file on the lock path carries no marker and authorizes nothing")
	b, err := os.ReadFile(stage.LockPath())
	require.NoError(t, err)
	assert.Equal(t, "unrelated regular file", string(b), "the planted file itself is preserved")
	_, err = os.Stat(saved)
	require.NoError(t, err, "the renamed real lock is preserved")
}

func TestReclaimRefusesUnmarkedStagingCandidates(t *testing.T) {
	mutate := func(t *testing.T, rewrite func(raw []byte) []byte) {
		t.Helper()
		stage, err := NewStaging(t.TempDir())
		require.NoError(t, err)
		root := filepath.Dir(stage.Dir())
		stage.Abandon()
		raw, err := os.ReadFile(stage.LockPath())
		require.NoError(t, err, "the builder must have published a marker")
		require.NoError(t, os.WriteFile(stage.LockPath(), rewrite(raw), 0o600))
		require.NoError(t, ReclaimAbandonedStaging(root))
		assert.DirExists(t, stage.Dir(),
			"proof-less residue is preserved, never deleted")
		assert.FileExists(t, stage.LockPath())
	}
	t.Run("an empty lock beside an unknown staging", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, stagingPrefix+"unrelated")
		lockPath := dir + stagingLockSuffix
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
		require.NoError(t, ReclaimAbandonedStaging(root))
		assert.DirExists(t, dir, "an empty lock is no marker and no proof")
		assert.FileExists(t, lockPath)
	})
	t.Run("a text file beside an unknown staging", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, stagingPrefix+"words")
		lockPath := dir + stagingLockSuffix
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.WriteFile(lockPath, []byte("unrelated regular file"), 0o600))
		require.NoError(t, ReclaimAbandonedStaging(root))
		assert.DirExists(t, dir)
		assert.FileExists(t, lockPath)
	})
	t.Run("a crash before the marker was published", func(t *testing.T) {
		// A builder that dies between creating its directory and flushing
		// its marker leaves an empty lock: the correct unknown outcome is
		// conservative residue, never deletion.
		root := t.TempDir()
		dir := filepath.Join(root, stagingPrefix+"early")
		lockPath := dir + stagingLockSuffix
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
		require.NoError(t, ReclaimAbandonedStaging(root))
		assert.DirExists(t, dir)
		assert.FileExists(t, lockPath)
	})
	t.Run("a corrupted marker", func(t *testing.T) {
		mutate(t, func(raw []byte) []byte { return []byte("{broken") })
	})
	t.Run("an oversized marker", func(t *testing.T) {
		mutate(t, func(raw []byte) []byte { return bytes.Repeat([]byte("x"), maxLockMarkerBytes+1) })
	})
	t.Run("unknown fields in the marker", func(t *testing.T) {
		mutate(t, func(raw []byte) []byte {
			var m map[string]any
			require.NoError(t, json.Unmarshal(raw, &m))
			m["extra"] = true
			out, err := json.Marshal(m)
			require.NoError(t, err)
			return out
		})
	})
	t.Run("a copied marker on a replacement inode", func(t *testing.T) {
		root := t.TempDir()
		source, err := NewStaging(root)
		require.NoError(t, err)
		target, err := NewStaging(root)
		require.NoError(t, err)
		source.Abandon()
		target.Abandon()
		marker, err := os.ReadFile(source.LockPath())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(target.LockPath(), marker, 0o600))
		require.NoError(t, ReclaimAbandonedStaging(root))
		assert.NoDirExists(t, source.Dir(),
			"the staging the marker truly names is reclaimed")
		assert.DirExists(t, target.Dir(),
			"a marker copied onto another inode records an identity that file does not have")
		assert.FileExists(t, target.LockPath())
	})
}

func TestReclaimRefusesSwappedLocks(t *testing.T) {
	root := t.TempDir()
	first, err := NewStaging(root)
	require.NoError(t, err)
	second, err := NewStaging(root)
	require.NoError(t, err)
	first.Abandon()
	second.Abandon()
	swapped := first.LockPath() + ".swapped"
	require.NoError(t, os.Rename(first.LockPath(), swapped))
	require.NoError(t, os.Rename(second.LockPath(), first.LockPath()))
	require.NoError(t, os.Rename(swapped, second.LockPath()))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.DirExists(t, first.Dir(), "a lock naming another staging proves nothing here")
	assert.DirExists(t, second.Dir())
	require.NoError(t, os.Rename(second.LockPath(), swapped))
	require.NoError(t, os.Rename(first.LockPath(), second.LockPath()))
	require.NoError(t, os.Rename(swapped, first.LockPath()))
	require.NoError(t, ReclaimAbandonedStaging(root))
	assert.NoDirExists(t, first.Dir(), "with the true locks restored, each proof applies again")
	assert.NoDirExists(t, second.Dir())
	assert.NoFileExists(t, first.LockPath())
	assert.NoFileExists(t, second.LockPath())
}

func TestMarkerIsPublishedOnlyAfterItsDirectoryExists(t *testing.T) {
	root := t.TempDir()
	sawSync := false
	original := syncFile
	syncFile = func(f *os.File) error {
		sawSync = true
		dir := strings.TrimSuffix(f.Name(), stagingLockSuffix)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the marker was published before its staging directory existed: %v", err)
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = original })
	stage, err := NewStaging(root)
	require.NoError(t, err)
	defer stage.Release()
	require.True(t, sawSync, "the marker publication must be flushed")
}

func TestNewStagingMarkerFailureRemovesOnlyItsOwnState(t *testing.T) {
	root := t.TempDir()
	original := syncFile
	syncFile = func(f *os.File) error { return errors.New("forced flush failure") }
	t.Cleanup(func() { syncFile = original })
	_, err := NewStaging(root)
	require.Error(t, err)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries,
		"a failed marker publication removes this builder's own empty directory and identity-matched lock")
}

func TestNewStagingMarkerFailureOnAmbiguousProofPreservesResidue(t *testing.T) {
	root := t.TempDir()
	original := syncFile
	syncFile = func(f *os.File) error {
		// The path is swapped while the builder is publishing: the held
		// proof no longer backs the path, so the builder may not trust
		// even its own cleanup.
		if err := os.Rename(f.Name(), f.Name()+".saved"); err != nil {
			return err
		}
		return errors.New("forced flush failure")
	}
	t.Cleanup(func() { syncFile = original })
	_, err := NewStaging(root)
	require.Error(t, err)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.NotEmpty(t, entries,
		"when the proof is ambiguous, the unmarked residue stays for recovery to judge")
}

func TestReleaseIsInertWhenItsPathNoLongerBacksTheHeldLock(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStaging(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stage.Dir(), "partial"), []byte("data"), 0o600))
	saved := stage.LockPath() + ".saved"
	require.NoError(t, os.Rename(stage.LockPath(), saved))
	require.NoError(t, os.WriteFile(stage.LockPath(), []byte("planted"), 0o600))
	stage.Release()
	assert.DirExists(t, stage.Dir(),
		"with the held identity gone, Release destroys nothing")
	b, err := os.ReadFile(stage.LockPath())
	require.NoError(t, err)
	assert.Equal(t, "planted", string(b), "the replacement path is preserved")
	_, err = os.Stat(saved)
	require.NoError(t, err, "the renamed real lock is preserved")
	// The descriptor is released even so: the real lock can be taken.
	probe, err := os.Open(saved)
	require.NoError(t, err)
	require.NoError(t, flockExclusive(probe))
	require.NoError(t, probe.Close())
	stage.Release()
	b, err = os.ReadFile(stage.LockPath())
	require.NoError(t, err)
	assert.Equal(t, "planted", string(b), "repeated Release stays inert")
}
