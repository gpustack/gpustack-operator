package osx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MkdirAll forces the leaf directory's mode to perm regardless of the process umask.
func TestMkdirAll(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a/b/c")
	require.NoError(t, MkdirAll(dir, 0o777))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o777), info.Mode().Perm())
}

func TestDurableWrite(t *testing.T) {
	cases := []struct {
		name string
		// before, when non-nil, is written at the target before the call, so a case can
		// assert what a failed replacement leaves behind.
		before []byte
		data   []byte
		perm   os.FileMode
	}{
		{name: "a new file is created with the requested mode", data: []byte("one"), perm: 0o644},
		{name: "a restrictive mode is not widened by the umask", data: []byte("two"), perm: 0o600},
		{name: "an existing file is replaced", before: []byte("old"), data: []byte("new"), perm: 0o644},
		// Empty content is a valid record for a caller whose format says so, and must not be
		// confused with the truncated file this function exists to prevent.
		{name: "empty content is written as empty", data: []byte{}, perm: 0o644},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "record.json")
			if c.before != nil {
				require.NoError(t, os.WriteFile(path, c.before, 0o644))
			}

			require.NoError(t, DurableWrite(path, c.data, c.perm))

			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, c.data, got)

			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, c.perm, info.Mode().Perm(), "mode must not be reduced by the umask")

			// The temporary file is an implementation detail that must never outlive the call:
			// a leftover would be picked up by a caller that scans the directory.
			entries, err := os.ReadDir(filepath.Dir(path))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, "record.json", entries[0].Name())
		})
	}
}

// DurableWrite creates nothing, so a caller keeps ownership of its directories' modes. The
// target is left untouched by the failure.
func TestDurableWriteRequiresTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "record.json")
	require.Error(t, DurableWrite(path, []byte("x"), 0o644))
	assert.False(t, Exists(filepath.Dir(path)))
}

// A failed replacement leaves the previous contents readable at the target itself, which is what
// makes a retry safe. The failure has to be injected on the very file being replaced for that to be
// what is asserted — a failed call against some other path proves nothing about this one.
func TestDurableWriteLeavesTheTargetOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permission this case fails on")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	require.NoError(t, os.WriteFile(path, []byte("kept"), 0o644))

	// A directory that cannot be written to fails the temporary file's creation, so the call fails
	// before it has produced anything to rename.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	require.Error(t, DurableWrite(path, []byte("replaced"), 0o644))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("kept"), got, "the target still holds exactly what it held before")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "record.json", entries[0].Name(), "no temporary file may outlive a failed call")
}

// A replacement that gets as far as the rename and fails there must not leave the temporary file it
// did produce, and must not disturb what stands at the target's name.
func TestDurableWriteFailedRenameLeavesNoTemporary(t *testing.T) {
	dir := t.TempDir()
	// A directory at the target's name cannot be replaced by a rename from a file.
	blocked := filepath.Join(dir, "blocked.json")
	require.NoError(t, os.Mkdir(blocked, 0o755))
	inside := filepath.Join(blocked, "kept")
	require.NoError(t, os.WriteFile(inside, []byte("kept"), 0o644))

	require.Error(t, DurableWrite(blocked, []byte("x"), 0o644))

	assert.True(t, ExistsFile(inside), "what stands at the target's name is untouched")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "blocked.json", entries[0].Name(), "no temporary file may outlive a failed call")
}
