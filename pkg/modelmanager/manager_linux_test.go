//go:build linux

package modelmanager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

func TestKubeletCapStatsPodsDirectory(t *testing.T) {
	cache, err := store.Open(t.TempDir())
	require.NoError(t, err)
	kubeletDir := t.TempDir()
	m := &Manager{Store: cache, KubeletDir: kubeletDir}

	_, err = m.kubeletCap()
	require.ErrorContains(t, err, filepath.Join(kubeletDir, "pods"))

	require.NoError(t, os.Mkdir(filepath.Join(kubeletDir, "pods"), 0o755))
	watermarkCap, err := m.kubeletCap()
	require.NoError(t, err)
	require.NotNil(t, watermarkCap)
}
