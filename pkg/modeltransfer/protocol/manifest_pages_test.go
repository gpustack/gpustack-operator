package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPagedManifestLookup(t *testing.T) {
	path := writePagedFixture(t, [][]TensorDescriptor{
		{{Name: "first", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 8, DataEnd: 12}},
		{{Name: "last", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 12, DataEnd: 16}},
	})
	manifest, err := LoadManifest(path, "")
	require.NoError(t, err)
	got, err := manifest.Tensor("last")
	require.NoError(t, err)
	require.Equal(t, int64(12), got.DataBegin)
	require.Empty(t, manifest.Tensors, "paged loading must not assemble the whole tensor table")
}

func TestPagedManifestRejectsCrossPageCorruption(t *testing.T) {
	for _, tc := range []struct {
		name, secondName string
		begin, end       int64
	}{
		{name: "duplicate name", secondName: "first", begin: 12, end: 16},
		{name: "overlap", secondName: "second", begin: 10, end: 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writePagedFixture(t, [][]TensorDescriptor{
				{{Name: "first", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 8, DataEnd: 12}},
				{{Name: tc.secondName, File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: tc.begin, DataEnd: tc.end}},
			})
			_, err := LoadManifest(path, "")
			require.Error(t, err)
		})
	}
}

func writePagedFixture(t *testing.T, pages [][]TensorDescriptor) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "pages"), 0o700))
	refs := make([]map[string]any, 0, len(pages))
	for _, tensors := range pages {
		raw, err := json.Marshal(tensors)
		require.NoError(t, err)
		digest := digestHex(raw)
		require.NoError(t, os.WriteFile(filepath.Join(root, "pages", digest+".json"), raw, 0o600))
		refs = append(refs, map[string]any{
			"sha256": digest, "size": len(raw), "count": len(tensors), "file": tensors[0].File,
			"begin": tensors[0].DataBegin, "end": tensors[len(tensors)-1].DataEnd,
		})
	}
	raw, err := json.Marshal(map[string]any{
		"contract_version": ContractVersion, "artifact_uid": "artifact", "source_generation": 1,
		"files":   []FileDescriptor{{Path: "weights.safetensors", Size: 32}},
		"tensors": []TensorDescriptor{}, "tensor_pages": refs,
	})
	require.NoError(t, err)
	path := filepath.Join(root, "manifest.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func TestManifestPagesLargerThanLegacyDocument(t *testing.T) {
	directory := t.TempDir()
	const count = 70000
	files := []FileDescriptor{{Path: "weights.safetensors", Size: count}}
	pages, err := WriteManifestPages(context.Background(), filepath.Join(directory, "pages"), files,
		func(yield func(TensorDescriptor, error) bool) {
			for i := range count {
				descriptor := TensorDescriptor{Name: fmt.Sprintf("%06d", i) + strings.Repeat("x", 1024), File: "weights.safetensors", Dtype: "U8", Shape: []int64{1}, DataBegin: int64(i), DataEnd: int64(i + 1)}
				if !yield(descriptor, nil) {
					return
				}
			}
		})
	require.NoError(t, err)
	total := 0
	for _, page := range pages {
		require.LessOrEqual(t, page.Size, MaxManifestPageBytes)
		total += page.Size
	}
	require.Greater(t, total, MaxManifestBytes)
	root := Manifest{ContractVersion: ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1, Files: files, Tensors: []TensorDescriptor{}, TensorPages: pages}
	raw, err := json.Marshal(root)
	require.NoError(t, err)
	require.Less(t, len(raw), 1<<20)
	path := filepath.Join(directory, "manifest.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	loaded, err := LoadManifest(path, "")
	require.NoError(t, err)
	require.Equal(t, count, loaded.TensorCount())
	got, err := loaded.Tensor(fmt.Sprintf("%06d", count-1) + strings.Repeat("x", 1024))
	require.NoError(t, err)
	require.Equal(t, int64(count), got.DataEnd)
	require.Empty(t, loaded.Tensors)
}

func TestPageReadDoesNotBlockUnrelatedLookup(t *testing.T) {
	path := writePagedFixture(t, [][]TensorDescriptor{
		{{Name: "first", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 8, DataEnd: 12}},
		{{Name: "last", File: "weights.safetensors", Dtype: "U8", Shape: []int64{4}, DataBegin: 12, DataEnd: 16}},
	})
	manifest, err := LoadManifest(path, "")
	require.NoError(t, err)
	reader := manifest.pageIndex.reader
	entered, release := make(chan struct{}), make(chan struct{})
	manifest.pageIndex.reader = func(ctx context.Context, page TensorPage) ([]byte, error) {
		if page.SHA256 == manifest.TensorPages[0].SHA256 {
			close(entered)
			<-release
		}
		return reader(ctx, page)
	}
	firstDone, lastDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := manifest.TensorContext(context.Background(), "first"); firstDone <- err }()
	<-entered
	go func() { _, err := manifest.TensorContext(context.Background(), "last"); lastDone <- err }()
	select {
	case err := <-lastDone:
		close(release)
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("unrelated page lookup blocked behind page I/O")
	}
	require.NoError(t, <-firstDone)
}

func TestLoadManifestContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LoadManifestContext(ctx, "unused", "")
	require.ErrorIs(t, err, context.Canceled)
}
