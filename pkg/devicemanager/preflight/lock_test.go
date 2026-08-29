package preflight

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// Two preflights on one node share the label every probe carries, so the second one's stale sweep
// removes the first one's live probes -- and an accelerator whose probe was killed mid-measurement
// reports as unable to slice. The lock is what keeps a healthy node from being failed by a second
// operator rather than by its hardware.
func TestLockHost(t *testing.T) {
	t.Run("the second caller is refused while the first holds it", func(t *testing.T) {
		root := fakeHostRoot(t)

		release, err := lockHost(root)
		require.NoError(t, err)

		_, err = lockHost(root)
		require.Error(t, err, "two runs took the lock at once")
		assert.Contains(t, err.Error(), "another preflight is already running")

		release()

		again, err := lockHost(root)
		require.NoError(t, err, "the lock outlived the run that held it")
		again()
	})

	t.Run("contention carries the sentinel and the errno that established it", func(t *testing.T) {
		root := fakeHostRoot(t)

		release, err := lockHost(root)
		require.NoError(t, err)
		defer release()

		_, err = lockHost(root)
		require.Error(t, err)
		assert.ErrorIs(t, err, errContended)
		assert.ErrorIs(t, err, syscall.EWOULDBLOCK)
	})

	t.Run("a lock failure that is not contention is not reported as one", func(t *testing.T) {
		// flock has other ways to fail -- a kernel without it, a filesystem that does not implement
		// it, an interrupted call -- and each is a node this cannot lock rather than a node someone
		// else is holding. None of them is reachable by taking the lock twice, which is why the
		// call is indirected.
		root := fakeHostRoot(t)
		orig := flock
		t.Cleanup(func() { flock = orig })
		flock = func(_, _ int) error { return syscall.ENOSYS }

		_, err := lockHost(root)
		require.Error(t, err)
		assert.NotErrorIs(t, err, errContended,
			"a kernel without flock was reported as another preflight holding the node")
		assert.ErrorIs(t, err, syscall.ENOSYS, "the reason the lock failed was lost")
	})

	t.Run("the lock file lives under the tree preflight owns", func(t *testing.T) {
		root := fakeHostRoot(t)

		release, err := lockHost(root)
		require.NoError(t, err)
		defer release()

		assert.FileExists(t, filepath.Join(root, deviceplugin.OperatorPreflightDir, lockName))
	})
}

// A contended node reports rather than crashes: the exit code is read by the same automation that
// reads the rows, so every manufacturer asked about has to carry an answer.
func TestPreflightAccelerator_AContendedNodeIsReportedNotRun(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{})

	root := fakeHostRoot(t)
	release, err := lockHost(root)
	require.NoError(t, err)
	defer release()

	asked := sets.New(nodefeature.ManufacturerNVIDIA, nodefeature.ManufacturerAscend)
	p, err := New(&Config{Manufacturers: asked, HostRoot: root})
	require.NoError(t, err)

	grpList := p.PreflightAccelerator(context.Background())

	require.Len(t, grpList, asked.Len(), "every manufacturer asked about still reports")
	for i := range grpList {
		assert.Equal(t, device.PreflightStateUnavailable, grpList[i].Detection.State)
		assert.Contains(t, grpList[i].Detection.Reason, "another preflight is already running")
		assert.Equal(t, noteContended, grpList[i].Note)
		assert.Empty(t, grpList[i].Checks, "a run that never started reports no verdict")
	}
	assert.NotEmpty(t, Failed(grpList), "a contended node must reach the exit code")
}

// The lock has more than one way to fail and only one of them is another operator. A permission
// denial or a tree that cannot be created stops the pass just as surely, and calling that contention
// sends a reader looking for a second preflight that is not running.
func TestPreflightAccelerator_AnUnlockableNodeIsNotBlamedOnAnotherPass(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{})

	root := fakeHostRoot(t)
	// "var" as a file rather than a directory, so the preflight directory under it cannot be
	// created. A failure that does not depend on this test running as an unprivileged user.
	require.NoError(t, os.WriteFile(filepath.Join(root, "var"), nil, 0o644))

	p, err := New(&Config{Manufacturers: sets.New(nodefeature.ManufacturerNVIDIA), HostRoot: root})
	require.NoError(t, err)

	grpList := p.PreflightAccelerator(context.Background())

	require.Len(t, grpList, 1)
	assert.Equal(t, device.PreflightStateUnavailable, grpList[0].Detection.State)
	assert.Equal(t, noteUnlockable, grpList[0].Note,
		"a lock that could not be opened was reported as another preflight holding it")
}

// A pass with no host root has nowhere to put a lock, which is not the same as not needing one: the
// mode toggles are driver calls this process makes directly, so they reach the flag whether or not a
// host root was mounted. Left alone, such a pass would be the one able to turn a shared flag on
// underneath another and unable to be serialized against it.
func TestPreflightAccelerator_NoHostRootWithholdsTheWrites(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{})

	// Exists and is readable, so it fails Validate on the markers alone -- the shape a caller who
	// forgot to mount the host's root arrives in, rather than a path that is simply not there.
	p, err := New(&Config{
		Manufacturers: sets.New(nodefeature.ManufacturerNVIDIA), HostRoot: t.TempDir(),
	})
	require.NoError(t, err)
	require.False(t, p.dryRun, "the caller asked for a real pass")

	p.PreflightAccelerator(context.Background())

	assert.True(t, p.dryRun, "an unlockable pass was still free to ask a driver mode on")
}

// A dry run starts no container and writes nothing, so there is nothing for a second one to sweep
// or share -- and refusing it would make the read-only mode the one that needs exclusive access.
func TestPreflightAccelerator_ADryRunNeedsNoLock(t *testing.T) {
	withRegistry(t, map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{})

	root := fakeHostRoot(t)
	release, err := lockHost(root)
	require.NoError(t, err)
	defer release()

	p, err := New(&Config{
		Manufacturers: sets.New(nodefeature.ManufacturerNVIDIA), HostRoot: root, DryRun: true,
	})
	require.NoError(t, err)

	grpList := p.PreflightAccelerator(context.Background())

	require.Len(t, grpList, 1)
	assert.NotEqual(t, noteContended, grpList[0].Note, "a dry run was refused for want of a lock")
}
