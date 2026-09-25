package driver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHostMounter mounts for real, so it runs only as root, as in the privileged Linux container
// the plugin's packages are tested in.
func TestHostMounter(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("bind mounts need root")
	}
	source, target := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "config.json"), []byte("{}"), 0o644))
	m := HostMounter{}

	mounted, err := m.IsMountPoint(target)
	require.NoError(t, err)
	require.False(t, mounted)

	require.NoError(t, m.BindReadOnly(source, target))
	t.Cleanup(func() { _ = m.Unmount(target) })
	mounted, err = m.IsMountPoint(target)
	require.NoError(t, err)
	assert.True(t, mounted)
	b, err := os.ReadFile(filepath.Join(target, "config.json"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(b))
	assert.Error(t, os.WriteFile(filepath.Join(target, "x"), []byte("x"), 0o644), "the mount is read-only")
	readOnly, err := m.IsReadOnly(target)
	require.NoError(t, err)
	assert.True(t, readOnly)
	readOnly, err = m.IsReadOnly(source)
	require.NoError(t, err)
	assert.False(t, readOnly, "the writable source is not")

	require.NoError(t, m.Unmount(target))
	mounted, err = m.IsMountPoint(target)
	require.NoError(t, err)
	assert.False(t, mounted)
	require.NoError(t, m.Unmount(target), "unmounting what is not mounted is not an error")
}
