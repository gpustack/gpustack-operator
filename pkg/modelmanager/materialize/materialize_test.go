package materialize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/driver"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// testClock is a settable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testHub serves one repository's files per path prefix, honoring ranges, and records each request's
// path and credential. A repository listed in denied answers 401 to every request.
type testHub struct {
	server *httptest.Server

	mu       sync.Mutex
	files    map[string]map[string][]byte // repository -> path -> content
	served   map[string][]byte            // what is actually served, when different
	denied   map[string]string            // repository -> token it refuses
	requests []struct{ path, auth string }
	gate     chan struct{}
}

func newTestHub(t *testing.T, files map[string]map[string][]byte) *testHub {
	t.Helper()
	h := &testHub{files: files, served: map[string][]byte{}, denied: map[string]string{}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, struct{ path, auth string }{r.URL.Path, r.Header.Get("Authorization")})
		gate := h.gate
		h.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		repo, path, ok := h.locate(r.URL.Path)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.mu.Lock()
		deniedToken, isDenied := h.denied[repo]
		body, alt := h.served[repo+"/"+path]
		if !alt {
			body = h.files[repo][path]
		}
		h.mu.Unlock()
		if isDenied && r.Header.Get("Authorization") == "Bearer "+deniedToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(h.server.Close)
	return h
}

// locate splits "/<repo owner>/<repo name>/resolve/<commit>/<path>".
func (h *testHub) locate(urlPath string) (repo, path string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(urlPath, "/"), "/", 5)
	if len(parts) != 5 || parts[2] != "resolve" || parts[3] != testCommit {
		return "", "", false
	}
	return parts[0] + "/" + parts[1], parts[4], true
}

func (h *testHub) manifest(repo string) modelartifact.Manifest {
	entries := make([]modelartifact.ManifestEntry, 0, len(h.files[repo]))
	for p, b := range h.files[repo] {
		sum := sha256.Sum256(b)
		entries = append(entries, modelartifact.ManifestEntry{Path: p, Size: int64(len(b)), Digest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	m, err := modelartifact.NewManifest(entries)
	if err != nil {
		panic(err)
	}
	return m
}

func (h *testHub) ListManifest(_ context.Context, repo, commit, token string, _ modelartifact.Filter) (modelartifact.Manifest, error) {
	h.mu.Lock()
	deniedToken, isDenied := h.denied[repo]
	h.mu.Unlock()
	if isDenied && token == deniedToken {
		return modelartifact.Manifest{}, &modelartifact.SourceError{Reason: modelartifact.ReasonAccessDenied, Message: "denied"}
	}
	if commit != testCommit {
		return modelartifact.Manifest{}, &modelartifact.SourceError{Reason: modelartifact.ReasonRevisionNotFound, Message: "no such commit"}
	}
	return h.manifest(repo), nil
}

func (h *testHub) FileURL(repo, commit, path string) string {
	return h.server.URL + "/" + repo + "/resolve/" + commit + "/" + path
}

func (h *testHub) recorded() []struct{ path, auth string } {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]struct{ path, auth string }(nil), h.requests...)
}

type testEnv struct {
	m     *Materializer
	hub   *testHub
	clock *testClock
	store *store.Store
}

func newTestEnv(t *testing.T, files map[string]map[string][]byte) *testEnv {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(root)
	require.NoError(t, err)
	hub := newTestHub(t, files)
	clock := &testClock{now: time.Date(2026, 9, 25, 6, 0, 0, 0, time.UTC)}
	dl := download.New(hub.server.Client(), 8, 0)
	dl.RetryDelay = time.Millisecond
	return &testEnv{
		hub: hub, clock: clock, store: st,
		m: &Materializer{
			Store:         st,
			Environment:   func(context.Context) (Hub, *download.Downloader, error) { return hub, dl, nil },
			Now:           clock.Now,
			CheckInterval: 10 * time.Millisecond,
		},
	}
}

func (e *testEnv) artifact(repo, uid, digest string) *workercore.ModelArtifact {
	return &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{UID: types.UID(uid)},
		Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
			HuggingFace: &workercore.ModelArtifactHubSource{Repository: repo},
		}},
		Status: workercore.ModelArtifactStatus{Resolved: &workercore.ModelArtifactResolved{Revision: testCommit, ManifestDigest: digest}},
	}
}

func (e *testEnv) request(repo, uid, token string) driver.Request {
	digest := e.hub.manifest(repo).Digest
	return driver.Request{Hex: store.HexOf(digest), Artifact: e.artifact(repo, uid, digest), Token: token}
}

// waitIdle waits until no attempt runs for hex.
func (e *testEnv) waitIdle(t *testing.T, hex string) {
	t.Helper()
	require.Eventually(t, func() bool { return !e.m.Downloading()[hex] }, 10*time.Second, 5*time.Millisecond)
}

func repoFiles() map[string]map[string][]byte {
	return map[string]map[string][]byte{
		"owner/repo": {"config.json": []byte(`{"a":1}`), "sub/model.safetensors": bytes.Repeat([]byte("w"), 3000)},
	}
}

func TestEnsureMaterializesAndPublishes(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	req := env.request("owner/repo", "uid-a", "token-a")

	p := env.m.Ensure(context.Background(), req)
	assert.False(t, p.Published)
	assert.Equal(t, codes.Aborted, p.Code)
	assert.Contains(t, p.Message, "materializing sha256:"+req.Hex[:12])
	env.waitIdle(t, req.Hex)

	require.True(t, env.store.IsPublished(req.Hex))
	b, err := os.ReadFile(filepath.Join(env.store.PublishedTree(req.Hex), "sub", "model.safetensors"))
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte("w"), 3000), b)
	assert.NoDirExists(t, filepath.Join(env.store.PublishedTree(req.Hex), "state"), "resume state never travels with the tree")
	m, err := env.store.ReadMarker(req.Hex)
	require.NoError(t, err)
	assert.Equal(t, int64(2), m.FileCount)
	assert.Equal(t, modelartifact.ManifestHeader, m.ManifestFormat)

	assert.True(t, env.m.Ensure(context.Background(), req).Published)
	for _, r := range env.hub.recorded() {
		assert.Equal(t, "Bearer token-a", r.auth)
	}
}

func TestEnsureJoinsTheRunningAttempt(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	env.hub.gate = make(chan struct{})
	req := env.request("owner/repo", "uid-a", "token-a")

	for range 5 {
		assert.Equal(t, codes.Aborted, env.m.Ensure(context.Background(), req).Code)
	}
	close(env.hub.gate)
	env.waitIdle(t, req.Hex)

	require.True(t, env.store.IsPublished(req.Hex))
	perPath := map[string]int{}
	for _, r := range env.hub.recorded() {
		perPath[r.path]++
	}
	assert.Len(t, perPath, 2)
	for path, n := range perPath {
		assert.Equal(t, 1, n, "%s was downloaded once for every caller", path)
	}
}

func TestEnsureBacksOffAFailingDigest(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	req := env.request("owner/repo", "uid-a", "token-a")
	env.hub.served["owner/repo/config.json"] = []byte(`{"a":2}`)

	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	require.False(t, env.store.IsPublished(req.Hex))
	rec, err := env.store.ReadDigest(req.Hex)
	require.NoError(t, err)
	assert.Equal(t, download.ReasonIntegrityMismatch, rec.Reason)
	assert.Equal(t, 1, rec.Failures)
	assert.Equal(t, env.clock.Now().Add(time.Minute), rec.RetryTime)

	// Until the retry time every mount answers at once, without a download.
	before := len(env.hub.recorded())
	p := env.m.Ensure(context.Background(), req)
	assert.Equal(t, codes.Unavailable, p.Code)
	assert.Contains(t, p.Message, download.ReasonIntegrityMismatch)
	assert.Len(t, env.hub.recorded(), before)

	// The backoff survives a restart: a new materializer on the same store reads it.
	restarted := &Materializer{Store: env.store, Environment: env.m.Environment, Now: env.clock.Now}
	assert.Equal(t, codes.Unavailable, restarted.Ensure(context.Background(), req).Code)

	// Past it, the next attempt runs, fails again and doubles the wait.
	env.clock.Advance(time.Minute + time.Second)
	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	rec, err = env.store.ReadDigest(req.Hex)
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Failures)
	assert.Equal(t, env.clock.Now().Add(2*time.Minute), rec.RetryTime)

	// A success resets it.
	delete(env.hub.served, "owner/repo/config.json")
	env.clock.Advance(3 * time.Minute)
	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	require.True(t, env.store.IsPublished(req.Hex))
	rec, err = env.store.ReadDigest(req.Hex)
	require.NoError(t, err)
	assert.Zero(t, rec.Failures)
	assert.Empty(t, rec.Reason)
}

func TestBackoffDoublesToItsCap(t *testing.T) {
	m := &Materializer{}
	var got []time.Duration
	for failures := 1; failures <= 9; failures++ {
		got = append(got, m.backoff(failures))
	}
	assert.Equal(t, []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute,
		time.Hour, time.Hour, time.Hour,
	}, got)
}

func TestEnsureRefusesAManifestThatIsNotTheArtifacts(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	req := env.request("owner/repo", "uid-a", "token-a")
	req.Hex = strings.Repeat("f", 64)
	req.Artifact.Status.Resolved.ManifestDigest = "sha256:" + req.Hex

	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	rec, err := env.store.ReadDigest(req.Hex)
	require.NoError(t, err)
	assert.Equal(t, download.ReasonIntegrityMismatch, rec.Reason)
	assert.Contains(t, rec.Message, "the artifact's digest")
	for _, r := range env.hub.recorded() {
		assert.Fail(t, "no file is downloaded for a manifest that does not match", r.path)
	}
}

func TestEnsureCancelsWithoutWaiters(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	env.hub.gate = make(chan struct{})
	env.m.WaiterWindow = time.Minute
	req := env.request("owner/repo", "uid-a", "token-a")

	env.m.Ensure(context.Background(), req)
	require.Eventually(t, func() bool { return len(env.hub.recorded()) > 0 }, 5*time.Second, 5*time.Millisecond)
	env.clock.Advance(2 * time.Minute)
	env.waitIdle(t, req.Hex)

	rec, err := env.store.ReadDigest(req.Hex)
	require.NoError(t, err)
	assert.Equal(t, download.ReasonCanceled, rec.Reason)
	assert.Zero(t, rec.Failures, "a cancellation is not a failure")
	partials, err := env.store.Partials()
	require.NoError(t, err)
	assert.Len(t, partials, 1, "the files stay for a resume")

	// The next mount starts again at once, and resumes the attempt's files.
	close(env.hub.gate)
	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	assert.True(t, env.store.IsPublished(req.Hex))
}

func TestEnsureKeepsEachCredentialWithItsRepository(t *testing.T) {
	files := repoFiles()
	files["other/copy"] = files["owner/repo"]
	env := newTestEnv(t, files)
	env.hub.denied["owner/repo"] = "token-a"
	env.hub.gate = make(chan struct{})

	// Two namespaces, each authorized by its own artifact over the same content.
	a := env.request("owner/repo", "uid-a", "token-a")
	b := env.request("other/copy", "uid-b", "token-b")
	require.Equal(t, a.Hex, b.Hex)
	env.m.Ensure(context.Background(), a)
	env.m.Ensure(context.Background(), b)
	close(env.hub.gate)
	env.waitIdle(t, a.Hex)

	require.True(t, env.store.IsPublished(a.Hex), "the second namespace's source served the content")
	for _, r := range env.hub.recorded() {
		switch {
		case strings.HasPrefix(r.path, "/owner/repo/"):
			assert.Equal(t, "Bearer token-a", r.auth)
		case strings.HasPrefix(r.path, "/other/copy/"):
			assert.Equal(t, "Bearer token-b", r.auth)
		}
	}
}

func TestEnsureReportsNoRoom(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	env.m.Reserve = func(context.Context, int64) error {
		return &download.Error{Reason: download.ReasonInsufficientCapacity, Message: "3 KiB do not fit under the high watermark", Detail: "3 KiB do not fit under the high watermark"}
	}
	req := env.request("owner/repo", "uid-a", "token-a")

	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	p := env.m.Ensure(context.Background(), req)
	assert.Equal(t, codes.ResourceExhausted, p.Code)
	assert.Contains(t, p.Message, "high watermark")
}

func TestEnsureWaitsForAValidConfiguration(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	env.m.Environment = func(context.Context) (Hub, *download.Downloader, error) {
		return nil, nil, &download.Error{Reason: download.ReasonInvalidRequest, Message: "the endpoint is not a URL"}
	}
	req := env.request("owner/repo", "uid-a", "token-a")

	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	rec, err := env.store.ReadDigest(req.Hex)
	require.NoError(t, err)
	assert.Equal(t, download.ReasonInvalidRequest, rec.Reason)
	assert.Zero(t, rec.Failures, "a configuration the node cannot use yet is not the source's failure")

	// Once the node's configuration arrives, the next mount starts at once: there is no backoff to wait out.
	env.m.Environment = func(context.Context) (Hub, *download.Downloader, error) {
		dl := download.New(env.hub.server.Client(), 8, 0)
		return env.hub, dl, nil
	}
	p := env.m.Ensure(context.Background(), req)
	assert.Equal(t, codes.Aborted, p.Code, "a new attempt runs: %s", p.Message)
	env.waitIdle(t, req.Hex)
	assert.True(t, env.store.IsPublished(req.Hex))
}

func TestEnsureReservesOnlyTheMissingBytes(t *testing.T) {
	env := newTestEnv(t, repoFiles())
	req := env.request("owner/repo", "uid-a", "token-a")
	// An earlier attempt left part of the weights on disk.
	a, err := env.store.NewAttempt(req.Hex)
	require.NoError(t, err)
	partial, err := a.FilePath("sub/model.safetensors")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(partial), 0o755))
	require.NoError(t, os.WriteFile(partial, bytes.Repeat([]byte("w"), 1000), 0o644))

	var reserved []int64
	var mu sync.Mutex
	env.m.Reserve = func(_ context.Context, n int64) error {
		mu.Lock()
		defer mu.Unlock()
		reserved = append(reserved, n)
		return nil
	}
	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)

	mu.Lock()
	defer mu.Unlock()
	total := env.hub.manifest("owner/repo").SizeBytes
	assert.Equal(t, []int64{total - 1000}, reserved, "the 1000 bytes already on disk are not reserved again")
}

// TestAFailurePublishesNoTenant pins that a failure reaches the node's record, and from it the
// NodeModelStore status and every mount's error, without the repository, the hub's URL or a file
// name: a digest is shared across tenants and the status is read across them.
func TestAFailurePublishesNoTenant(t *testing.T) {
	env := newTestEnv(t, map[string]map[string][]byte{"team-secret/private-model": {"config.json": []byte(`{"a":1}`)}})
	req := env.request("team-secret/private-model", "uid-a", "token-a")
	host := strings.TrimPrefix(env.hub.server.URL, "http://")
	env.hub.server.Close()

	env.m.Ensure(context.Background(), req)
	env.waitIdle(t, req.Hex)
	rec, err := env.store.ReadDigest(req.Hex)
	require.NoError(t, err)
	require.Equal(t, download.ReasonSourceUnavailable, rec.Reason)
	assert.Contains(t, rec.Message, "a connection failure")
	backingOff := env.m.Ensure(context.Background(), req).Message
	for _, leak := range []string{"team-secret", "private-model", host, "/resolve/", "config.json"} {
		assert.NotContains(t, rec.Message, leak, "the record")
		assert.NotContains(t, backingOff, leak, "a mount's error")
	}
}

func TestPublicMessage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a listing refusal keeps only the hub's status",
			err: manifestError(&modelartifact.SourceError{
				Reason:  modelartifact.ReasonAccessDenied,
				Message: `list the files of "team-secret/private-model" at main: HTTP 401 (GatedRepo)`,
			}),
			want: "the hub refused the credential, or a file does not exist: HTTP 401",
		},
		{
			name: "a request that cannot be built names nothing of its URL",
			err: &download.Error{
				Reason:  download.ReasonInvalidRequest,
				Message: `config.json: parse "http://hub/team-secret/private-model/resolve/x/config.json": invalid`,
			},
			want: "the node cannot run the download with its configuration",
		},
		{
			name: "a node's own condition is kept whole",
			err:  &download.Error{Reason: download.ReasonInsufficientCapacity, Message: "full", Detail: "3 KiB do not fit under the high watermark"},
			want: "the node's cache has no room for it: 3 KiB do not fit under the high watermark",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := publicMessage(c.err)
			assert.Equal(t, c.want, got)
			assert.NotContains(t, got, "team-secret")
		})
	}
}
