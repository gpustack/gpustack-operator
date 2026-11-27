package peer

import (
	"context"
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
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/stringx"
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
	manifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{
		Path: "weights.bin", Size: int64(len(content)), Digest: "sha256:" + stringx.SumBySHA256(content),
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
	file := download.File{
		Path:       "weights.bin",
		Size:       int64(len(ps.content)),
		Digest:     "sha256:" + stringx.SumBySHA256(ps.content),
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
	wrongHex := stringx.SumBySHA256(strings.Repeat("X", len(good.content)))

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

func TestPullerFetchManifest(t *testing.T) {
	ps := newPeerServer(t, strings.Repeat("abcdef", 100))
	source := ps.source(t, "peer-1")
	p := &Puller{Discover: func(context.Context, string) ([]*Source, error) {
		return []*Source{source}, nil
	}}
	manifest, err := p.FetchManifest(context.Background(), "sha256:"+ps.hex)
	require.NoError(t, err)
	assert.Equal(t, "sha256:"+ps.hex, manifest.Digest, "the listing is bound to the tree's digest")
	require.Len(t, manifest.Entries, 1)
	assert.Equal(t, "weights.bin", manifest.Entries[0].Path)
	assert.Equal(t, int64(len(ps.content)), manifest.Entries[0].Size)

	dead := newPeerServer(t, strings.Repeat("abcdef", 100))
	dead.Close()
	p = &Puller{Discover: func(context.Context, string) ([]*Source, error) { return nil, nil }}
	_, err = p.FetchManifest(context.Background(), "sha256:"+dead.hex)
	require.Error(t, err, "no peer holding the tree is the loud no-source failure")
}

func TestPullerFailsWithoutADigest(t *testing.T) {
	ps := newPeerServer(t, "0123456789")
	p := &Puller{}
	err := p.fetchFile(t, ps, "not-a-digest", ps.source(t, "peer-1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a manifest digest")
}

func TestPullerEnforcesListingKind(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(writableAgain(st.Root()))
	content := `{"schemaVersion":"gpustack-forge-dispatch/v1","entries":[]}`
	manifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{
		Path: "dispatch-manifest.json", Size: int64(len(content)), Digest: "sha256:" + stringx.SumBySHA256(content),
	}})
	require.NoError(t, err)
	khex := strings.TrimPrefix(manifest.Digest, "sha256:")
	a, err := st.NewAttempt(khex)
	require.NoError(t, err)
	dest, err := a.FilePath("dispatch-manifest.json")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest, []byte(content), 0o644))
	stored, err := json.Marshal(Listing{
		Digest: manifest.Digest,
		Kind:   store.MarkerKindKernel,
		Entries: []ManifestFile{{
			Path: "dispatch-manifest.json", Size: manifest.SizeBytes, Digest: manifest.Entries[0].Digest,
		}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(a.Dir(), "manifest.json"), stored, 0o600))
	require.NoError(t, a.Publish(store.Marker{
		Digest: manifest.Digest, SizeBytes: manifest.SizeBytes, FileCount: manifest.FileCount,
		Source: store.SourceForge, Kind: store.MarkerKindKernel,
	}))
	kernelTS := httptest.NewServer((&Server{Store: st}).Handler())
	t.Cleanup(kernelTS.Close)

	client, err := NewClient(AuthOptions{Mode: AuthNone})
	require.NoError(t, err)
	kernelSrc := &Source{NodeName: "seed-1", BaseURL: kernelTS.URL, Client: client}

	modelPS := newPeerServer(t, "model-weights")
	modelSrc := modelPS.source(t, "model-1")

	t.Run("kernel pull succeeds on kernel peer and refuses model peer", func(t *testing.T) {
		p := &Puller{Discover: func(context.Context, string) ([]*Source, error) {
			return []*Source{kernelSrc}, nil
		}}
		got, err := p.FetchManifestForKind(context.Background(), manifest.Digest, store.MarkerKindKernel)
		require.NoError(t, err)
		assert.Equal(t, manifest.Digest, got.Digest)

		dir := t.TempDir()
		err = p.FetchFileForKind(context.Background(), manifest.Digest, store.MarkerKindKernel, download.File{
			Path:       "dispatch-manifest.json",
			Size:       int64(len(content)),
			Digest:     manifest.Entries[0].Digest,
			Dest:       filepath.Join(dir, "dispatch-manifest.json"),
			Checkpoint: filepath.Join(dir, "cp"),
		})
		require.NoError(t, err)

		pModel := &Puller{Discover: func(context.Context, string) ([]*Source, error) {
			return []*Source{modelSrc}, nil
		}}
		_, err = pModel.FetchManifestForKind(context.Background(), "sha256:"+modelPS.hex, store.MarkerKindKernel)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "kind")
	})

	t.Run("model pull refuses kernel peer", func(t *testing.T) {
		p := &Puller{Discover: func(context.Context, string) ([]*Source, error) {
			return []*Source{kernelSrc}, nil
		}}
		_, err := p.FetchManifest(context.Background(), manifest.Digest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "kind")
	})
}
