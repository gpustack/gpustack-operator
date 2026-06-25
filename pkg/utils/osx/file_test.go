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
