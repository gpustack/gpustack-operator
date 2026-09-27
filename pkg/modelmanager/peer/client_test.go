package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	modelartifact "gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// peerServer is one peer under test: its store, its published tree, and the HTTP server over them.
type peerServer struct {
	*httptest.Server
	store   *store.Store
	hex     string
	content string
}

func newPeerServer(t *testing.T, content string) *peerServer {
	t.Helper()
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(writableAgain(st.Root()))

	// The tree's digest is what the canonical manifest of its files hashes to, the way a
	// publish derives the marker's.
	fileSum := sha256.Sum256([]byte(content))
	manifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{
		Path: "weights.bin", Size: int64(len(content)), Digest: "sha256:" + hex.EncodeToString(fileSum[:]),
	}})
	require.NoError(t, err)
	treeHex := strings.TrimPrefix(manifest.Digest, "sha256:")
	a, err := st.NewAttempt(treeHex)
	require.NoError(t, err)
	treePath, err := a.FilePath("weights.bin")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(treePath), 0o755))
	require.NoError(t, os.WriteFile(treePath, []byte(content), 0o644))
	require.NoError(t, a.Publish(store.Marker{
		Digest: manifest.Digest, SizeBytes: manifest.SizeBytes, FileCount: manifest.FileCount,
	}))
	stored, err := json.Marshal(listing{Digest: manifest.Digest, Entries: []ManifestFile{
		{Path: "weights.bin", Size: manifest.SizeBytes, Digest: manifest.Entries[0].Digest},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(st.PublishedManifestPath(treeHex), stored, 0o644))

	inner := (&Server{Store: st}).Handler()
	ts := httptest.NewServer(inner)

	return &peerServer{Server: ts, store: st, hex: treeHex, content: content}
}

func (ps *peerServer) source(t *testing.T, name string) *Source {
	client, err := NewClient(AuthOptions{Mode: AuthNone})
	require.NoError(t, err)

	return &Source{NodeName: name, BaseURL: ps.URL, Client: client}
}

// fetchFile is what one fetch of the tree's single file looks like: the digest names the
// content, and the file's own line is its per-file digest, as the manifest carries them.
func (p *Puller) fetchFile(t *testing.T, ps *peerServer, digest string, sources ...*Source) error {
	t.Helper()
	p.Discover = func(context.Context, string) ([]*Source, error) { return sources, nil }
	dir := t.TempDir()
	fileSum := sha256.Sum256([]byte(ps.content))
	file := download.File{
		Path:       "weights.bin",
		Size:       int64(len(ps.content)),
		Digest:     "sha256:" + hex.EncodeToString(fileSum[:]),
		Dest:       filepath.Join(dir, "dest.bin"),
		Checkpoint: filepath.Join(dir, "dest.checkpoint"),
	}

	return p.FetchFile(context.Background(), digest, file)
}

func TestPullerFetchesAndVerifies(t *testing.T) {
	ps := newPeerServer(t, strings.Repeat("abcdef", 100))
	p := &Puller{}
	var got int64
	p.Received = func(node string, n int64) {
		got += n
	}
	require.NoError(t, p.fetchFile(t, ps, "sha256:"+ps.hex, ps.source(t, "peer-1")))
	assert.Equal(t, int64(len(ps.content)), got, "every byte is counted for the node that delivered it")
}

func TestPullerRefusesAForgedListing(t *testing.T) {
	// A peer whose tree answers under another content's digest: the reassembled manifest
	// rebinds to the wrong root and is refused before a byte is pulled.
	good := newPeerServer(t, strings.Repeat("abcdef", 100))
	wrongSum := sha256.Sum256([]byte(strings.Repeat("X", len(good.content))))
	wrongHex := hex.EncodeToString(wrongSum[:])

	inner := (&Server{Store: good.store}).Handler()
	dishonest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.Replace(r.URL.Path, wrongHex, good.hex, 1)
		inner.ServeHTTP(w, r)
	}))
	defer func() { dishonest.Close() }()

	evil := &Source{NodeName: "evil", BaseURL: dishonest.URL, Client: dishonest.Client()}
	p := &Puller{}
	err := p.fetchFile(t, good, "sha256:"+wrongHex, evil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused")
}

func TestPullerRotatesToTheNextCandidate(t *testing.T) {
	good := newPeerServer(t, strings.Repeat("abcdef", 100))
	dead := newPeerServer(t, strings.Repeat("abcdef", 100))
	dead.Close()
	p := &Puller{}
	err := p.fetchFile(t, good, "sha256:"+good.hex, dead.source(t, "dead"), good.source(t, "good"))
	require.NoError(t, err)
}

func TestPullerFailsWithoutCandidates(t *testing.T) {
	ps := newPeerServer(t, "0123456789")
	p := &Puller{}
	err := p.fetchFile(t, ps, "sha256:"+ps.hex)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no candidate was usable")
}

func TestPullerFailsWithoutADigest(t *testing.T) {
	ps := newPeerServer(t, "0123456789")
	p := &Puller{}
	err := p.fetchFile(t, ps, "not-a-digest", ps.source(t, "peer-1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a manifest digest")
}
