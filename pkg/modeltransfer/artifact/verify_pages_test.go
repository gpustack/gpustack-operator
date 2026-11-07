package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func TestBuildPagedPreservesPublishedLayout(t *testing.T) {
	st := openStore(t)
	published := happyTree(t, st)
	legacy, err := build(t, st, published)
	require.NoError(t, err)
	directory := t.TempDir()
	view, err := BuildPaged(context.Background(), st, testUID, 1, published.digest, filepath.Join(directory, "pages"))
	require.NoError(t, err)
	require.Equal(t, legacy.RegisteredBytes, view.RegisteredBytes)
	require.Equal(t, legacy.Registrations, view.Registrations)
	require.Empty(t, view.Manifest.Tensors)
	path := filepath.Join(directory, "manifest.json")
	require.NoError(t, os.WriteFile(path, view.Raw, 0o600))
	loaded, err := protocol.LoadManifest(t.Context(), path, view.SHA256)
	require.NoError(t, err)
	require.Equal(t, len(legacy.Manifest.Tensors), loaded.TensorCount())
	for _, want := range legacy.Manifest.Tensors {
		got, err := loaded.Tensor(t.Context(), want.Name)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestBuildPagedRejectsDuplicateNamesAcrossFiles(t *testing.T) {
	st := openStore(t)
	tensors := []stTensor{{name: "duplicate", dtype: "U8", shape: []int64{1}, data: []byte{1}}}
	published := publishTree(t, st, map[string][]byte{
		"one.safetensors": buildSafetensors(t, tensors),
		"two.safetensors": buildSafetensors(t, tensors),
	})
	_, err := BuildPaged(context.Background(), st, testUID, 1, published.digest, t.TempDir())
	require.ErrorContains(t, err, "duplicate")
}
