package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// publishFixtureTree publishes a small verified tree; manifestInside, when
// false, publishes a tree that does not carry manifest.json in its files,
// the way a sealed checkpoint tree without the reader manifest would.
func publishFixtureTree(t *testing.T, st *store.Store, hex string, manifestInside bool) {
	t.Helper()
	root := st.Root()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	attempt, err := st.NewAttempt(hex)
	require.NoError(t, err)
	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	checkpoint, err := os.ReadFile(filepath.Join(fixture, "checkpoint.safetensors"))
	require.NoError(t, err)
	tree := filepath.Join(attempt.Dir(), "tree")
	require.NoError(t, os.MkdirAll(tree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "checkpoint.safetensors"),
		checkpoint, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "config.json"),
		[]byte(`{"architectures":["TinyForCausalLM"]}`), 0o644))
	if manifestInside {
		manifest, err := os.ReadFile(filepath.Join(fixture, "manifest.json"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(tree, "manifest.json"),
			manifest, 0o644))
	}
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: "sha256:" + hex, SizeBytes: int64(len(checkpoint)) + 32,
		ManifestFormat: "model-transfer/test", Source: store.SourceHub,
	}))
}

// preStageView builds a verified view into the view root under its content
// address, the way a metadata pre-staging or remote delivery would.
func preStageView(t *testing.T, viewRoot, digest string) string {
	t.Helper()
	src := buildSourceTree(t)
	staging := filepath.Join(t.TempDir(), "staging")
	view, err := Build(src, staging, digest, nil)
	require.NoError(t, err)
	final := filepath.Join(viewRoot, view.ID())
	require.NoError(t, os.MkdirAll(viewRoot, 0o755))
	require.NoError(t, os.Rename(staging, final))
	return final
}

func TestEnsureViewServesAPreStagedViewWithoutAPublishedTree(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("a", 64)
	digest := "sha256:" + hex
	viewRoot := filepath.Join(root, "views")

	preStaged := preStageView(t, viewRoot, digest)

	// The node holds no published tree for the digest at all, and the
	// pre-staged view still answers: a metadata-only destination mounts.
	dir, err := EnsureView(st, viewRoot, hex, digest, nil)
	require.NoError(t, err)
	assert.Equal(t, preStaged, dir)
	assert.False(t, st.IsPublished(hex))
}

func TestEnsureViewBuildsFromThePublishedTreeWhenNoViewStands(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("b", 64)
	digest := "sha256:" + hex
	publishFixtureTree(t, st, hex, true)

	dir, err := EnsureView(st, filepath.Join(root, "views"), hex, digest, nil)
	require.NoError(t, err)
	view, err := Verify(dir)
	require.NoError(t, err)
	assert.Equal(t, digest, view.ArtifactDigest)
}

func TestEnsureViewReadsTheManifestBesideTheMarker(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("c", 64)
	digest := "sha256:" + hex
	publishFixtureTree(t, st, hex, false)

	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	manifest, err := os.ReadFile(filepath.Join(fixture, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(st.PublishedManifestPath(hex), manifest, 0o644))

	dir, err := EnsureView(st, filepath.Join(root, "views"), hex, digest,
		[]string{"manifest.json", "config.json"})
	require.NoError(t, err)
	view, err := Verify(dir)
	require.NoError(t, err)
	held := map[string]bool{}
	for _, f := range view.Files {
		held[f.Name] = true
	}
	assert.True(t, held["manifest.json"], "the manifest came from beside the marker")
	staged, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	require.NoError(t, err)
	assert.Equal(t, manifest, staged)
}

func TestEnsureViewWithoutATreeOrAViewIsActionable(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	_, err = EnsureView(st, filepath.Join(root, "views"), strings.Repeat("d", 64),
		"sha256:"+strings.Repeat("d", 64), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no verified published tree")
	assert.Contains(t, err.Error(), "pre-stage the view")
}

func TestEnsureViewConcurrentCallsAgreeOnOneDirectory(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("e", 64)
	digest := "sha256:" + hex
	publishFixtureTree(t, st, hex, true)
	viewRoot := filepath.Join(root, "views")

	var wg sync.WaitGroup
	dirs := make(chan string, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir, err := EnsureView(st, viewRoot, hex, digest, nil)
			if err != nil {
				failures <- err
				return
			}
			dirs <- dir
		}()
	}
	wg.Wait()
	close(dirs)
	close(failures)
	for err := range failures {
		t.Fatalf("a concurrent ensure failed: %v", err)
	}
	var first string
	for dir := range dirs {
		if first == "" {
			first = dir
			continue
		}
		require.Equal(t, first, dir, "concurrent ensures diverged")
	}
	entries, err := os.ReadDir(viewRoot)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".staging-", "a staging directory leaked")
	}
}

func TestFindViewHonorsTheRequiredSet(t *testing.T) {
	root := t.TempDir()
	viewRoot := filepath.Join(root, "views")
	digest := "sha256:" + strings.Repeat("f", 64)
	preStageView(t, viewRoot, digest)

	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)

	// A request for a file the standing view does not hold does not match
	// it: the view cannot answer what it does not carry.
	_, err = EnsureView(st, viewRoot, strings.Repeat("f", 64), digest,
		[]string{"config.json", "generation_config.json"})
	require.Error(t, err)
}

func TestEnsureViewRepairsCorruptDirectory(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("b", 64)
	digest := "sha256:" + hex
	publishFixtureTree(t, st, hex, true)
	viewRoot := filepath.Join(root, "views")
	dir, err := EnsureView(st, viewRoot, hex, digest, nil)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(filepath.Join(dir, "config.json"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("corrupt"), 0o644))
	_, err = Verify(dir)
	require.Error(t, err)
	repaired, err := EnsureView(st, viewRoot, hex, digest, nil)
	require.NoError(t, err)
	require.Equal(t, dir, repaired)
	_, err = Verify(repaired)
	require.NoError(t, err)
}
