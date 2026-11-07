package artifact

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// testUID is the live artifact identity every happy-path build binds.
const testUID = "artifact-uid-1"

// stTensor is one tensor a test safetensors file is built from.
type stTensor struct {
	name  string
	dtype string
	shape []int64
	data  []byte
}

// buildSafetensors lays tensors' data out sequentially after the header, the
// way the format stores them, and returns the whole file's bytes.
func buildSafetensors(t *testing.T, tensors []stTensor) []byte {
	t.Helper()
	header := map[string]any{}
	var data []byte
	for _, ten := range tensors {
		header[ten.name] = map[string]any{
			"dtype": ten.dtype, "shape": ten.shape,
			"data_offsets": []int64{int64(len(data)), int64(len(data) + len(ten.data))},
		}
		data = append(data, ten.data...)
	}
	headerJSON, err := stdjson.Marshal(header)
	require.NoError(t, err)
	file := leBytes(int64(len(headerJSON)))
	file = append(file, headerJSON...)

	return append(file, data...)
}

// leBytes is the little-endian 8-byte length prefix the format carries.
func leBytes(n int64) []byte {
	b := make([]byte, 8)
	for i := 0; i < 8; i++ {
		b[i] = byte(n >> (8 * i))
	}

	return b
}

// sha256digest is the "sha256:<hex>" of the bytes, the digest a hub lists an
// LFS file with.
func sha256digest(data []byte) string {
	return modelartifact.DigestSHA256 + ":" + sha256Hex(data)
}

// gitshadigest is the git blob digest of the bytes, the digest a hub lists a
// non-LFS file with.
func gitshadigest(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)

	return modelartifact.DigestGitSHA1 + ":" + hex.EncodeToString(h.Sum(nil))
}

// publishTree seals a real store attempt holding files, with the listing the
// materializer stores beside the marker, and returns its content address.
// A nil value writes no file: the entry stays listed for cases where the
// test puts something else at that path after the publish.
func publishTree(t *testing.T, st *store.Store, files map[string][]byte) published {
	t.Helper()
	return publishTreeWith(t, st, files, func(_ string, data []byte) string {
		return sha256digest(data)
	})
}

// published is a tree the test published into the store.
type published struct {
	hex    string
	digest string
}

// publishTreeWith is publishTree with the per-file digest algorithm spelled
// by the test.
func publishTreeWith(
	t *testing.T, st *store.Store, files map[string][]byte,
	digestOf func(path string, data []byte) string,
) published {
	t.Helper()
	entries := make([]modelartifact.ManifestEntry, 0, len(files))
	for path, data := range files {
		entries = append(entries, modelartifact.ManifestEntry{
			Path: path, Size: int64(len(data)), Digest: digestOf(path, data),
		})
	}
	canonical, err := modelartifact.NewManifest(entries)
	require.NoError(t, err)
	hex := store.HexOf(canonical.Digest)

	attempt, err := st.NewAttempt(hex)
	require.NoError(t, err)
	for path, data := range files {
		if data == nil {
			continue
		}
		p, err := attempt.FilePath(path)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, data, 0o644))
	}
	listing := peer.Listing{Digest: canonical.Digest}
	for _, e := range canonical.Entries {
		listing.Entries = append(listing.Entries, peer.ManifestFile{
			Path: e.Path, Size: e.Size, Digest: e.Digest,
		})
	}
	b, err := json.Marshal(listing)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(attempt.Dir(), "manifest.json"), b, 0o600))
	require.NoError(t, attempt.Publish(store.Marker{
		Digest: canonical.Digest, ManifestFormat: modelartifact.ManifestHeader,
		SizeBytes: canonical.SizeBytes, FileCount: canonical.FileCount,
		PublishedTime: time.Now(), Source: store.SourceHub,
	}))

	return published{hex: hex, digest: canonical.Digest}
}

// build runs the happy-path Build: the test identity against a published
// digest.
func build(t *testing.T, st *store.Store, p published) (*View, error) {
	t.Helper()

	return Build(context.Background(), st, testUID, 1, p.digest)
}

// happyTree publishes a small but complete bundle: an ancillary config, an
// empty ancillary, and two safetensors shards in one subdirectory, so
// multi-component paths and cross-file tensor ordering are exercised.
func happyTree(t *testing.T, st *store.Store) published {
	t.Helper()

	return publishTree(t, st, map[string][]byte{
		"config.json":    []byte(`{"architectures":["TinyForCausalLM"]}`),
		"tokenizer.json": {},
		"model.safetensors": buildSafetensors(t, []stTensor{
			{name: "b.weight", dtype: "F32", shape: []int64{2, 2}, data: []byte{
				0x00, 0x00, 0x80, 0x3f, 0x00, 0x00, 0x00, 0x40,
				0x00, 0x00, 0x40, 0x40, 0x00, 0x00, 0x80, 0x40,
			}},
			{name: "c.bias", dtype: "F16", shape: []int64{2}, data: []byte{0x00, 0x38, 0x00, 0x3c}},
		}),
		"sub/nested.safetensors": buildSafetensors(t, []stTensor{
			{name: "a.embedding", dtype: "I64", shape: []int64{1}, data: []byte{
				0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			}},
		}),
	})
}

// TestBuildBridgesAPublishedTreeIntoAPortableManifest pins the bridge's
// whole happy path: a real attempt publication, listing and all, becomes a
// portable manifest whose identity, files, tensors, and derived digests all
// verify — and the same inputs answer with the same bytes.
func TestBuildBridgesAPublishedTreeIntoAPortableManifest(t *testing.T) {
	st := openStore(t)
	p := happyTree(t, st)

	view, err := build(t, st, p)
	require.NoError(t, err)

	// The identity is the caller's, the content digest is kept beside the
	// manifest's own digest, and the own digest digests Raw's exact bytes.
	assert.Equal(t, testUID, view.Manifest.ArtifactUID)
	assert.EqualValues(t, 1, view.Manifest.SourceGeneration)
	assert.Equal(t, p.digest, view.ContentDigest)
	assert.Equal(t, view.SHA256, view.Manifest.ManifestSHA256)
	assert.Equal(t, view.SHA256, sha256Hex(view.Raw))
	assert.NotEqual(t, view.SHA256, view.ContentDigest)
	assert.Equal(t, st.PublishedTree(p.hex), view.TreeDir)

	// The manifest loads under its own digest, and under no other.
	path := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(path, view.Raw, 0o600))
	loaded, err := protocol.LoadManifest(t.Context(), path, view.SHA256)
	require.NoError(t, err)
	assert.Equal(t, view.Manifest.ContractVersion, loaded.ContractVersion)
	_, err = protocol.LoadManifest(t.Context(), path, strings.Repeat("ab", 32))
	require.Error(t, err)
	assert.Equal(t, protocol.KindAuthentication, protocol.KindOf(err))

	// Tensors stand in one deterministic order across all files: name order.
	names := make([]string, 0, len(view.Manifest.Tensors))
	for _, tensor := range view.Manifest.Tensors {
		names = append(names, tensor.Name)
	}
	assert.Equal(t, []string{"a.embedding", "b.weight", "c.bias"}, names)

	// Empty ancillary files retain their layout entry.
	paths := make([]string, 0, len(view.Manifest.Files))
	for _, f := range view.Manifest.Files {
		paths = append(paths, f.Path)
	}
	assert.Equal(t, []string{"config.json", "model.safetensors", "sub/nested.safetensors", "tokenizer.json"}, paths)

	// Registration figures are the published safetensors buffers whole:
	// their full byte sizes, not the tensor payload totals.
	var registered int64
	shards := 0
	for _, f := range view.Manifest.Files {
		if !strings.HasSuffix(f.Path, ".safetensors") {
			continue
		}
		info, err := os.Stat(filepath.Join(view.TreeDir, filepath.FromSlash(f.Path)))
		require.NoError(t, err)
		registered += info.Size()
		shards++
	}
	assert.Equal(t, registered, view.RegisteredBytes)
	assert.Equal(t, 2, view.Registrations)

	// Deterministic: the same tree and identity answer byte for byte.
	again, err := build(t, st, p)
	require.NoError(t, err)
	assert.Equal(t, view.Raw, again.Raw)
	assert.Equal(t, view.SHA256, again.SHA256)
}

func TestBuildManifestIsIndependentOfShardConcurrency(t *testing.T) {
	st := openStore(t)
	p := happyTree(t, st)
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	serial, err := build(t, st, p)
	require.NoError(t, err)
	for _, workers := range []int{2, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			runtime.GOMAXPROCS(workers)
			parallel, err := build(t, st, p)
			require.NoError(t, err)
			assert.Equal(t, serial.Raw, parallel.Raw)
			assert.Equal(t, serial.SHA256, parallel.SHA256)
			assert.Equal(t, serial.RegisteredBytes, parallel.RegisteredBytes)
			assert.Equal(t, serial.Registrations, parallel.Registrations)
		})
	}
}

// TestBuildBindsTheLiveArtifactIdentity pins the identity distinction: the
// same bytes under another UID or another source generation are another
// manifest, and an identity the contract does not admit is refused before
// any IO.
func TestBuildBindsTheLiveArtifactIdentity(t *testing.T) {
	st := openStore(t)
	p := happyTree(t, st)

	one, err := Build(context.Background(), st, testUID, 1, p.digest)
	require.NoError(t, err)
	otherUID, err := Build(context.Background(), st, "artifact-uid-2", 1, p.digest)
	require.NoError(t, err)
	otherGen, err := Build(context.Background(), st, testUID, 2, p.digest)
	require.NoError(t, err)

	assert.Equal(t, "artifact-uid-2", otherUID.Manifest.ArtifactUID)
	assert.EqualValues(t, 2, otherGen.Manifest.SourceGeneration)
	assert.NotEqual(t, one.SHA256, otherUID.SHA256)
	assert.NotEqual(t, one.SHA256, otherGen.SHA256)
	assert.NotEqual(t, otherUID.SHA256, otherGen.SHA256)

	for _, tc := range []struct {
		name             string
		uid              string
		generation       int64
		digest           string
		wantNotPublished bool
	}{
		{name: "empty uid", uid: "", generation: 1, digest: p.digest},
		{name: "uid with a slash", uid: "team/model", generation: 1, digest: p.digest},
		{name: "uid over the length bound", uid: strings.Repeat("a", 129), generation: 1, digest: p.digest},
		{name: "zero generation", uid: testUID, generation: 0, digest: p.digest},
		{name: "negative generation", uid: testUID, generation: -1, digest: p.digest},
		{name: "digest without its algorithm", uid: testUID, generation: 1, digest: p.hex},
		{name: "digest that is not hexadecimal", uid: testUID, generation: 1, digest: "sha256:not-hex"},
		{name: "unpublished digest", uid: testUID, generation: 1, digest: "sha256:" + strings.Repeat("f", 64), wantNotPublished: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(context.Background(), st, tc.uid, tc.generation, tc.digest)
			require.Error(t, err)
			if tc.wantNotPublished {
				assert.ErrorIs(t, err, ErrNotPublished)
			}
		})
	}
}

// TestBuildLeavesTheCacheUntouched pins that Build is read-only: the listing,
// the marker, and the tree stand exactly as the publish left them.
func TestBuildLeavesTheCacheUntouched(t *testing.T) {
	st := openStore(t)
	p := happyTree(t, st)

	before := snapshot(t, st.PublishedTree(p.hex)+"/..")
	listingBefore, err := os.ReadFile(st.PublishedManifestPath(p.hex))
	require.NoError(t, err)

	_, err = build(t, st, p)
	require.NoError(t, err)

	listingAfter, err := os.ReadFile(st.PublishedManifestPath(p.hex))
	require.NoError(t, err)
	assert.Equal(t, listingBefore, listingAfter)
	assert.Equal(t, before, snapshot(t, st.PublishedTree(p.hex)+"/.."))
	assert.True(t, st.IsPublished(p.hex))
}

// TestBuildIgnoresUnlistedFiles pins that only the listing's files are read:
// a stray file a resumed attempt left in the tree changes nothing.
func TestBuildIgnoresUnlistedFiles(t *testing.T) {
	st := openStore(t)
	p := happyTree(t, st)
	first, err := build(t, st, p)
	require.NoError(t, err)

	unseal(t, st.PublishedTree(p.hex))
	require.NoError(t, os.WriteFile(
		filepath.Join(st.PublishedTree(p.hex), "stray.bin"), []byte("not listed"), 0o644,
	))

	second, err := build(t, st, p)
	require.NoError(t, err)
	assert.Equal(t, first.Raw, second.Raw)
}

// TestBuildRefusesStaleMarkers pins the marker checks: a tree verified
// against another digest is refused, not silently accepted.
func TestBuildRefusesStaleMarkers(t *testing.T) {
	st := openStore(t)
	p := happyTree(t, st)

	markerPath := filepath.Join(st.Root(), "published", p.hex, "marker")
	marker, err := os.ReadFile(markerPath)
	require.NoError(t, err)
	tampered := strings.Replace(string(marker), p.digest, "sha256:"+strings.Repeat("9", 64), 1)
	require.NoError(t, os.WriteFile(markerPath, []byte(tampered), 0o644))

	_, err = build(t, st, p)
	require.ErrorIs(t, err, ErrStaleDigest)
}

// openStore opens a store over a fresh root, and gives the root's sealed
// modes back before the test's temporary directory is removed: a published
// tree is read-only, which would otherwise defeat the cleanup. Symlinks are
// skipped, so a test's escape symlink cannot have its target re-chmod'd.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.Type()&os.ModeSymlink != 0 {
				return nil //nolint:nilerr // an entry that vanished mid-walk is left as it is
			}
			if e.IsDir() {
				return os.Chmod(path, 0o755)
			}

			return os.Chmod(path, 0o644)
		})
	})
	st, err := store.Open(root)
	require.NoError(t, err)

	return st
}

// unseal gives a published tree its working modes back, the way a resumed
// attempt would, so a test can inject content into it. Symlinks are left
// alone: os.Chmod follows them, and a test that is about to plant one must
// not re-mode its target.
func unseal(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case e.Type()&os.ModeSymlink != 0:
			return nil
		case e.IsDir():
			return os.Chmod(path, 0o755)
		default:
			return os.Chmod(path, 0o644)
		}
	})
	require.NoError(t, err)
}

// snapshot is the tree's whole shape: every path and size under root,
// including the listing and marker files beside the tree.
func snapshot(t *testing.T, root string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	base, err := filepath.Abs(root)
	require.NoError(t, err)
	require.NoError(t, filepath.WalkDir(base, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = info.Size()

		return nil
	}))

	return out
}
