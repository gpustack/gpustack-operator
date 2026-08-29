package preflight

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// withStagingRoots points inImageLibDir at a scratch "in-image" source tree and
// deviceplugin.OperatorLibDir at a short host-relative path, restoring both when the test ends --
// the real values are an absolute image path and an absolute host path, neither of which a test
// may write to.
func withStagingRoots(t *testing.T) (imageRoot string) {
	t.Helper()

	imageRoot = t.TempDir()
	origImage, origLib := inImageLibDir, deviceplugin.OperatorLibDir
	inImageLibDir = imageRoot
	deviceplugin.OperatorLibDir = "/operator-lib"
	t.Cleanup(func() {
		inImageLibDir, deviceplugin.OperatorLibDir = origImage, origLib
	})
	return imageRoot
}

// A standalone probe container has no init container to stage the in-image manufacturer tree onto
// the host, which is exactly the gap StageLib fills -- so what it writes, and where, is the whole
// point, and what it reports when it cannot write is what keeps a caller from mounting an
// injection whose source was never staged.
func TestStageLib(t *testing.T) {
	t.Run("it copies the manufacturer tree onto the host through the host root, preserving mode", func(t *testing.T) {
		imageRoot := withStagingRoots(t)
		hostRoot := fakeHostRoot(t)

		require.NoError(t, os.MkdirAll(filepath.Join(imageRoot, "ascend", "lib"), 0o755))
		libSrc := filepath.Join(imageRoot, "ascend", "lib", "libvruntime.so")
		require.NoError(t, os.WriteFile(libSrc, []byte("so-bytes"), 0o755))
		preloadSrc := filepath.Join(imageRoot, "ascend", "ld.so.preload")
		require.NoError(t, os.WriteFile(preloadSrc, []byte("preload"), 0o644))

		res := StageLib(hostRoot, "ascend")

		require.False(t, res.Failed, res.Reason)
		assert.Equal(t, "ascend", res.Manufacturer)

		libDst := filepath.Join(hostRoot, deviceplugin.OperatorLibDir, "ascend", "lib", "libvruntime.so")
		gotLib, err := os.ReadFile(libDst)
		require.NoError(t, err, "the mount source an injection would read must exist after staging")
		assert.Equal(t, "so-bytes", string(gotLib))

		info, err := os.Stat(libDst)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode(), "an executable's mode must survive staging")

		preloadDst := filepath.Join(hostRoot, deviceplugin.OperatorLibDir, "ascend", "ld.so.preload")
		gotPreload, err := os.ReadFile(preloadDst)
		require.NoError(t, err)
		assert.Equal(t, "preload", string(gotPreload))
	})

	// Staging happens on a host that has been staged before -- by an earlier preflight, or by the
	// device-manager's own init container -- so the interesting case is the second write, not the
	// first. Truncating a destination in place carries neither of the two things a mount needs: the
	// old file's mode outlives it, and a copy interrupted halfway leaves a file that is neither
	// version. Both are settled by writing beside the destination and renaming over it.
	t.Run("it replaces an existing file whole, with the source's mode and no leftovers", func(t *testing.T) {
		imageRoot := withStagingRoots(t)
		hostRoot := fakeHostRoot(t)

		require.NoError(t, os.MkdirAll(filepath.Join(imageRoot, "ascend", "lib"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(imageRoot, "ascend", "lib", "libvruntime.so"), []byte("new-bytes"), 0o755))

		// What an earlier stage left: shorter, and not executable.
		dstDir := filepath.Join(hostRoot, deviceplugin.OperatorLibDir, "ascend", "lib")
		require.NoError(t, os.MkdirAll(dstDir, 0o755))
		dst := filepath.Join(dstDir, "libvruntime.so")
		require.NoError(t, os.WriteFile(dst, []byte("stale"), 0o644))

		res := StageLib(hostRoot, "ascend")
		require.False(t, res.Failed, res.Reason)

		got, err := os.ReadFile(dst)
		require.NoError(t, err)
		assert.Equal(t, "new-bytes", string(got))

		info, err := os.Stat(dst)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode(),
			"an executable staged over a non-executable must end up executable")

		entries, err := os.ReadDir(dstDir)
		require.NoError(t, err)
		require.Len(t, entries, 1, "the temporary the rename went through must not be left behind")
		assert.Equal(t, "libvruntime.so", entries[0].Name())
	})

	t.Run("a manufacturer this image carries no tree for is a named failure, not a silent skip", func(t *testing.T) {
		withStagingRoots(t)
		hostRoot := fakeHostRoot(t)

		res := StageLib(hostRoot, "ascend")

		require.True(t, res.Failed)
		assert.Contains(t, res.Reason, "ascend")
		assert.NoDirExists(t, filepath.Join(hostRoot, deviceplugin.OperatorLibDir, "ascend"),
			"nothing must be mounted from a directory staging never wrote")
	})

	t.Run("a host that refuses the write is a named failure naming what could not be written", func(t *testing.T) {
		imageRoot := withStagingRoots(t)
		hostRoot := fakeHostRoot(t)

		require.NoError(t, os.MkdirAll(filepath.Join(imageRoot, "ascend"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(imageRoot, "ascend", "ld.so.preload"), []byte("x"), 0o644))

		// A file sits where staging needs to create a directory, so the write is refused --
		// standing in for a host root mounted read-only or otherwise unwritable.
		blocked := filepath.Join(hostRoot, deviceplugin.OperatorLibDir)
		require.NoError(t, os.MkdirAll(filepath.Dir(blocked), 0o755))
		require.NoError(t, os.WriteFile(blocked, []byte("occupied"), 0o644))

		res := StageLib(hostRoot, "ascend")

		require.True(t, res.Failed)
		assert.NotEmpty(t, res.Reason)
	})
}
