package deviceplugin

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The redirect mutates two process-global paths, so the restore is the whole safety of it: a
// redirect that did not put them back would leave every later caller in this process -- a test, or
// a second manufacturer's pass -- rendering its injection into a directory that no longer exists.
func TestRedirectHostWrites(t *testing.T) {
	origLib, origPods := OperatorLibDir, OperatorPodsDir
	require.NotEmpty(t, origLib)
	require.NotEmpty(t, origPods)

	root := t.TempDir()
	restore := RedirectHostWrites(root)

	assert.Equal(t, filepath.Join(root, "lib"), OperatorLibDir)
	assert.Equal(t, filepath.Join(root, "pods"), OperatorPodsDir)
	assert.NotEqual(t, origLib, OperatorLibDir, "a redirect that changed nothing would write to the host")

	restore()

	assert.Equal(t, origLib, OperatorLibDir)
	assert.Equal(t, origPods, OperatorPodsDir)
}

// A manufacturer carrying a host path of its own hands it to the redirect rather than joining it
// under the returned root itself, because a path the redirect does not know about is a path nothing
// rewrites back. Measured on hardware: NVIDIA's HAMi-core lock directory came out of a dry run as
// /tmp/gpustack-preflight-1194651507/vgpulock -- neither the /tmp/vgpulock a real allocation uses
// nor a directory that exists on the node at all, so a reader running the emitted command would
// create an empty one instead of sharing the lock the slice is supposed to coordinate through.
func TestNewPreflightRedirectRehostsAPrivatePath(t *testing.T) {
	lock := "/tmp/vgpulock"

	root, restore, err := NewPreflightRedirect(&lock)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(root, "0-vgpulock"), lock,
		"a private path left where it was would have the responder write to the node it is inspecting")
	assert.Equal(t, map[string]string{filepath.Join(root, "0-vgpulock"): "/tmp/vgpulock"}, PreflightRehosts(),
		"the caller cannot name a manufacturer's private path, so the redirect has to report it")

	restore()

	assert.Equal(t, "/tmp/vgpulock", lock)
	assert.Empty(t, PreflightRehosts(), "a restored redirect leaves nothing to rewrite against")
}

// Two redirects are open at once whenever a behavior needs an owner and a second tenant, and the
// private path is one package variable rather than one per redirect -- so the second redirect reads
// the first one's scratch value as the original. Reporting that verbatim would rewrite the second
// tenant's mount onto the first tenant's scratch directory, which is gone by the time anyone runs
// the command.
func TestNewPreflightRedirectRehostsThroughANestedRedirect(t *testing.T) {
	lock := "/tmp/vgpulock"

	rootA, restoreA, err := NewPreflightRedirect(&lock)
	require.NoError(t, err)
	rootB, restoreB, err := NewPreflightRedirect(&lock)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		filepath.Join(rootA, "0-vgpulock"): "/tmp/vgpulock",
		filepath.Join(rootB, "0-vgpulock"): "/tmp/vgpulock",
	}, PreflightRehosts(), "both tenants' mounts name the path a real allocation would")

	restoreB()
	assert.Equal(t, filepath.Join(rootA, "0-vgpulock"), lock, "the outer redirect is still open")

	restoreA()
	assert.Equal(t, "/tmp/vgpulock", lock)
	assert.Empty(t, PreflightRehosts())
}
