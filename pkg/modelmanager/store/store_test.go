package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testHex  = strings.Repeat("a", 64)
	otherHex = strings.Repeat("b", 64)
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = removeAllWritable(s.Root()) })
	return s
}

func writeAttemptFile(t *testing.T, a *Attempt, path, content string) {
	t.Helper()
	p, err := a.FilePath(path)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), workingDirMode))
	require.NoError(t, os.WriteFile(p, []byte(content), workingFileMode))
}

func TestOpenLayout(t *testing.T) {
	root := t.TempDir()
	_, err := Open(root)
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(root, layoutFile))
	require.NoError(t, err)
	assert.Equal(t, Layout+"\n", string(b))

	_, err = Open(root)
	require.NoError(t, err, "a root of this layout opens again")

	require.NoError(t, os.WriteFile(filepath.Join(root, layoutFile), []byte("gpustack-model-store v2\n"), 0o644))
	_, err = Open(root)
	assert.ErrorIs(t, err, ErrLayout)

	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, layoutFile), []byte("gpustack-model-store v2\n"), 0o644))
	_, err = Open(other)
	assert.ErrorIs(t, err, ErrLayout)
	entries, err := os.ReadDir(other)
	require.NoError(t, err)
	require.Len(t, entries, 1, "a refused root is left as it was")
	assert.Equal(t, layoutFile, entries[0].Name())
}

func TestHexOf(t *testing.T) {
	assert.Equal(t, testHex, HexOf("sha256:"+testHex))
	for _, bad := range []string{testHex, "sha256:" + strings.ToUpper(testHex), "sha256:" + testHex[:63], "md5:" + testHex, "sha256:../" + testHex[:61]} {
		assert.Empty(t, HexOf(bad), bad)
	}
}

func TestAttemptNumbersAreNeverReused(t *testing.T) {
	s := openTestStore(t)

	a1, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	a2, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	assert.Equal(t, int64(1), a1.Number)
	assert.Equal(t, int64(2), a2.Number)

	// The number survives the partial's removal, so a late writer of attempt 2 never finds its
	// directory again under a new attempt.
	require.NoError(t, s.RemovePartial(testHex, 2))
	a3, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	assert.Equal(t, int64(3), a3.Number)
}

func TestNewAttemptCarriesThePreviousFiles(t *testing.T) {
	s := openTestStore(t)
	a1, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	writeAttemptFile(t, a1, "sub/model.safetensors", "partial bytes")
	// A marker an interrupted publication left behind.
	require.NoError(t, os.WriteFile(filepath.Join(a1.Dir(), markerFile), []byte("{}"), 0o644))

	a2, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	p, err := a2.FilePath("sub/model.safetensors")
	require.NoError(t, err)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "partial bytes", string(b))
	assert.NoDirExists(t, a1.Dir())
	assert.NoFileExists(t, filepath.Join(a2.Dir(), markerFile))
}

func TestNewAttemptUnsealsAnInterruptedPublication(t *testing.T) {
	s := openTestStore(t)
	a1, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	writeAttemptFile(t, a1, "sub/model.safetensors", "partial bytes")
	// Sealed the way Publish seals, and interrupted before the rename.
	tree := filepath.Join(a1.Dir(), treeDir)
	require.NoError(t, os.Chmod(filepath.Join(tree, "sub", "model.safetensors"), publishedFileMode))
	require.NoError(t, os.Chmod(filepath.Join(tree, "sub"), publishedDirMode))
	require.NoError(t, os.Chmod(tree, publishedDirMode))

	a2, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	p, err := a2.FilePath("sub/model.safetensors")
	require.NoError(t, err)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	require.NoError(t, err, "a carried file reopens for the resumed download")
	require.NoError(t, f.Close())
	p, err = a2.FilePath("sub/tokenizer.json")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, []byte("{}"), workingFileMode), "a carried directory takes new files")
}

func TestAttemptFilePath(t *testing.T) {
	s := openTestStore(t)
	a, err := s.NewAttempt(testHex)
	require.NoError(t, err)

	cases := []struct {
		path    string
		wantErr bool
	}{
		{path: "config.json"},
		{path: "a/b/model.safetensors"},
		{path: "my file.bin"},
		{path: "../escape", wantErr: true},
		{path: "a/../../escape", wantErr: true},
		{path: "a/./b", wantErr: true},
		{path: "a//b", wantErr: true},
		{path: "/abs", wantErr: true},
		{path: "", wantErr: true},
		{path: ".", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			p, err := a.FilePath(c.path)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(p, filepath.Join(a.Dir(), treeDir)+string(filepath.Separator)))
		})
	}
}

func TestPublish(t *testing.T) {
	s := openTestStore(t)
	a, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	writeAttemptFile(t, a, "config.json", "{}")
	writeAttemptFile(t, a, "sub/model.safetensors", "weights")
	assert.False(t, s.IsPublished(testHex), "an attempt is not published")

	require.NoError(t, a.Publish(Marker{Digest: "sha256:" + testHex, ManifestFormat: "gpustack-manifest v1", SizeBytes: 9, FileCount: 2}))

	assert.True(t, s.IsPublished(testHex))
	m, err := s.ReadMarker(testHex)
	require.NoError(t, err)
	assert.Equal(t, a.Number, m.Attempt)
	assert.Equal(t, "sha256:"+testHex, m.Digest)
	assert.NoDirExists(t, a.Dir())

	tree := s.PublishedTree(testHex)
	b, err := os.ReadFile(filepath.Join(tree, "sub", "model.safetensors"))
	require.NoError(t, err)
	assert.Equal(t, "weights", string(b))
	for path, want := range map[string]fs.FileMode{
		tree:                               publishedDirMode,
		filepath.Join(tree, "sub"):         publishedDirMode,
		filepath.Join(tree, "config.json"): publishedFileMode,
		filepath.Join(tree, "sub", "model.safetensors"): publishedFileMode,
	} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, want, info.Mode().Perm(), path)
	}

	markers, err := s.Published()
	require.NoError(t, err)
	require.Len(t, markers, 1)
	assert.Equal(t, int64(2), markers[0].FileCount)
}

func TestPublishRefusesAnythingButRegularFiles(t *testing.T) {
	s := openTestStore(t)
	a, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	writeAttemptFile(t, a, "config.json", "{}")
	p, err := a.FilePath("link")
	require.NoError(t, err)
	require.NoError(t, os.Symlink("/etc/passwd", p))

	require.Error(t, a.Publish(Marker{Digest: "sha256:" + testHex}))
	assert.False(t, s.IsPublished(testHex))
}

func TestATreeWithoutAMarkerIsNotPublished(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, os.MkdirAll(filepath.Join(s.Root(), publishedDir, testHex, treeDir), 0o755))

	assert.False(t, s.IsPublished(testHex))
	markers, err := s.Published()
	require.NoError(t, err)
	assert.Empty(t, markers)
}

func TestRemovePublished(t *testing.T) {
	s := openTestStore(t)
	a, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	writeAttemptFile(t, a, "sub/model.safetensors", "weights")
	require.NoError(t, a.Publish(Marker{Digest: "sha256:" + testHex}))

	require.NoError(t, s.RemovePublished(testHex))
	assert.False(t, s.IsPublished(testHex))
	assert.NoDirExists(t, filepath.Join(s.Root(), publishedDir, testHex))
	trash, err := os.ReadDir(filepath.Join(s.Root(), trashDir))
	require.NoError(t, err)
	assert.Empty(t, trash)
	require.NoError(t, s.RemovePublished(testHex), "removing twice is not an error")
}

func TestEmptyTrash(t *testing.T) {
	s := openTestStore(t)
	left := filepath.Join(s.Root(), trashDir, testHex+"-1", treeDir)
	require.NoError(t, os.MkdirAll(left, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(left, "x"), []byte("x"), 0o444))
	require.NoError(t, os.Chmod(left, publishedDirMode))

	require.NoError(t, s.EmptyTrash())
	trash, err := os.ReadDir(filepath.Join(s.Root(), trashDir))
	require.NoError(t, err)
	assert.Empty(t, trash)
}

func TestPartials(t *testing.T) {
	s := openTestStore(t)
	a, err := s.NewAttempt(testHex)
	require.NoError(t, err)
	writeAttemptFile(t, a, "model.safetensors", "12345")
	b, err := s.NewAttempt(otherHex)
	require.NoError(t, err)
	writeAttemptFile(t, b, "config.json", "{}")

	partials, err := s.Partials()
	require.NoError(t, err)
	require.Len(t, partials, 2)
	sizes := map[string]int64{}
	for _, p := range partials {
		sizes[p.Hex] = p.SizeBytes
		assert.WithinDuration(t, time.Now(), p.ModTime, time.Minute)
	}
	assert.Equal(t, map[string]int64{testHex: 5, otherHex: 2}, sizes)

	require.NoError(t, s.RemovePartial(testHex, a.Number))
	partials, err = s.Partials()
	require.NoError(t, err)
	require.Len(t, partials, 1)
	assert.Equal(t, otherHex, partials[0].Hex)
	assert.NoDirExists(t, filepath.Join(s.Root(), partialDir, testHex))
}
