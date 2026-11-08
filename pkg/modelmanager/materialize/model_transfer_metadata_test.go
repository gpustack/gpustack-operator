package materialize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// publishTree publishes a small verified tree with the golden fixture's
// manifest and checkpoint plus a config file, the way a completed
// materialization does.
func publishTree(t *testing.T, st *store.Store, hex string) {
	t.Helper()
	root := st.Root()
	t.Cleanup(func() {
		// Publish seals the tree read-only; make it removable again.
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	attempt, err := st.NewAttempt(hex)
	require.NoError(t, err)
	for _, name := range []string{"manifest.json", "checkpoint.safetensors", "config.json"} {
		b, err := os.ReadFile(filepath.Join(fixture, name))
		if os.IsNotExist(err) {
			b, err = []byte(`{"architectures":["TinyForCausalLM"]}`), nil
		}
		require.NoError(t, err)
		p, err := attempt.FilePath(name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, b, 0o644))
	}
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: "sha256:" + hex, SizeBytes: 734 + 1024,
		ManifestFormat: "model-transfer/test", Source: store.SourceHub,
	}))
}

func TestEnsureMetadataBuildsTheViewFromAVerifiedTree(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("a", 64)
	publishTree(t, st, hex)

	viewRoot := filepath.Join(root, "views")
	viewDir, err := EnsureMetadata(st, viewRoot, hex, "sha256:"+hex, nil)
	require.NoError(t, err)

	// The view is verified and lives outside the store's published layout.
	view, err := metadata.Verify(viewDir)
	require.NoError(t, err)
	assert.True(t, view.Incomplete)
	assert.False(t, strings.HasPrefix(viewDir, filepath.Join(st.Root(), "published")),
		"the metadata view never lands in the published tree layout")

	// The published tree itself stays a complete tree; the view is separate.
	assert.True(t, st.IsPublished(hex))

	// A second call is idempotent and answers the same directory.
	again, err := EnsureMetadata(st, viewRoot, hex, "sha256:"+hex, nil)
	require.NoError(t, err)
	assert.Equal(t, viewDir, again)
}

func TestEnsureMetadataNeverMarksACompleteTree(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("a", 64)
	publishTree(t, st, hex)

	// Only metadata was asked for; no new digest became published, and the
	// view's own directory is not a published tree by the store's rules.
	viewDir, err := EnsureMetadata(st, filepath.Join(root, "views"), hex, "sha256:"+hex, nil)
	require.NoError(t, err)
	assert.False(t, st.IsPublished(filepath.Base(viewDir)),
		"a metadata-only view must never register as a published tree")
}

func TestEnsureMetadataRefusesWithoutAVerifiedTree(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("a", 64)

	// Nothing was published: the refusal is actionable, and no download of a
	// full checkpoint is started to satisfy a metadata-only request.
	_, err = EnsureMetadata(st, filepath.Join(root, "views"), hex, "sha256:"+hex, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no verified published tree")
}

func TestEnsureMetadataRefusesAStaleDigest(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("a", 64)
	publishTree(t, st, hex)

	// The tree verifies against its own digest; asking under another one is
	// a stale-identity refusal, not a mismatched copy.
	_, err = EnsureMetadata(st, filepath.Join(root, "views"), hex,
		"sha256:"+strings.Repeat("b", 64), nil)
	require.ErrorIs(t, err, metadata.ErrStaleDigest)
}

func TestEnsureMetadataRejectsUnknownRequiredFiles(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "cache"))
	require.NoError(t, err)
	hex := strings.Repeat("a", 64)
	publishTree(t, st, hex)

	_, err = EnsureMetadata(st, filepath.Join(root, "views"), hex, "sha256:"+hex,
		[]string{"config.json", "weights.bin"})
	require.ErrorIs(t, err, metadata.ErrUnknownRequiredFile)
}
