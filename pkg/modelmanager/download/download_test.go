package download

import (
	"context"
	"crypto/sha1" // nolint: gosec // git blob ids are SHA-1.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "hf_test-token-value-never-in-errors"

// testHub serves files by path, honoring byte ranges, and records every request.
type testHub struct {
	server *httptest.Server

	mu       sync.Mutex
	files    map[string][]byte
	requests []testRequest

	// inFlight and maxInFlight count concurrent requests.
	inFlight    atomic.Int32
	maxInFlight atomic.Int32

	// behavior, set per case.
	status      func(r *http.Request, n int) int // an error status for the n-th request, or 0
	ignoreRange bool
	// ignoreRangeFrom makes every request from the n-th on, counted across the hub's life, answer
	// the whole file whatever range it asked for.
	ignoreRangeFrom int
	corrupt         bool
	short           bool
	// stallOnce makes the first request for a path send half its range and then hang.
	stallOnce map[string]bool
	redirect  *httptest.Server
}

type testRequest struct {
	Path  string
	Range string
	Auth  string
	Host  string
}

func newTestHub(t *testing.T, files map[string][]byte) *testHub {
	t.Helper()
	h := &testHub{files: files, stallOnce: map[string]bool{}}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *testHub) serve(w http.ResponseWriter, r *http.Request) {
	n := h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	for {
		m := h.maxInFlight.Load()
		if n <= m || h.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}

	h.mu.Lock()
	h.requests = append(h.requests, testRequest{
		Path: r.URL.Path, Range: r.Header.Get("Range"),
		Auth: r.Header.Get("Authorization"), Host: r.Host,
	})
	count := len(h.requests)
	content, ok := h.files[strings.TrimPrefix(r.URL.Path, "/")]
	ignoreRange := h.ignoreRange || (h.ignoreRangeFrom > 0 && count >= h.ignoreRangeFrom)
	stall := h.stallOnce[r.URL.Path]
	if stall {
		h.stallOnce[r.URL.Path] = false
	}
	h.mu.Unlock()

	if h.status != nil {
		if code := h.status(r, count); code != 0 {
			w.WriteHeader(code)
			return
		}
	}
	if h.redirect != nil && r.Host == h.server.Listener.Addr().String() {
		http.Redirect(w, r, h.redirect.URL+r.URL.Path+"?X-Amz-Signature=secret-signature", http.StatusFound)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body := append([]byte(nil), content...)
	if h.corrupt && len(body) > 0 {
		body[len(body)/2] ^= 0xff
	}
	if h.short {
		body = body[:len(body)-1]
	}

	start, end := int64(0), int64(len(body))-1
	if rg := r.Header.Get("Range"); rg != "" && !ignoreRange {
		_, _ = fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
		end = min(end, int64(len(body))-1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
	}
	part := body[start : end+1]
	if stall {
		_, _ = w.Write(part[:len(part)/2])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	_, _ = w.Write(part)
}

func (h *testHub) recorded() []testRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]testRequest(nil), h.requests...)
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func gitsha1Digest(b []byte) string {
	h := sha1.New() // nolint: gosec
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(b))
	_, _ = h.Write(b)
	return "gitsha1:" + hex.EncodeToString(h.Sum(nil))
}

func content(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func testDownloader(hub *testHub) *Downloader {
	d := New(hub.server.Client(), 8, 0)
	d.SegmentBytes = 1 << 10
	d.Window = 2
	d.StallTimeout = 200 * time.Millisecond
	d.Retries = 3
	d.RetryDelay = time.Millisecond
	d.CheckpointBytes = 2 << 10
	return d
}

func testFile(t *testing.T, hub *testHub, path string, b []byte, digest string) File {
	t.Helper()
	dir := t.TempDir()
	return File{
		Path: path, Size: int64(len(b)), Digest: digest, URL: hub.server.URL + "/" + path,
		Dest: filepath.Join(dir, "tree-file"), Checkpoint: filepath.Join(dir, "checkpoint"),
		Token: func() string { return testToken },
	}
}

func TestFetchVerifiesWhileDownloading(t *testing.T) {
	small, large := content(300), content(10<<10+17)
	cases := []struct {
		name   string
		body   []byte
		digest func([]byte) string
	}{
		{name: "a small LFS file in one request", body: small, digest: sha256Digest},
		{name: "a non-LFS file by its git blob id", body: small, digest: gitsha1Digest},
		{name: "a large file as parallel ranges", body: large, digest: sha256Digest},
		{name: "an empty file", body: []byte{}, digest: gitsha1Digest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newTestHub(t, map[string][]byte{"f": c.body})
			f := testFile(t, hub, "f", c.body, c.digest(c.body))

			require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
			got, err := os.ReadFile(f.Dest)
			require.NoError(t, err)
			assert.Equal(t, c.body, got)
			for _, r := range hub.recorded() {
				assert.Equal(t, "Bearer "+testToken, r.Auth)
				assert.NotEmpty(t, r.Range)
			}
		})
	}
}

func TestFetchSplitsALargeFileIntoDisjointRanges(t *testing.T) {
	body := content(10 << 10)
	hub := newTestHub(t, map[string][]byte{"f": body})
	f := testFile(t, hub, "f", body, sha256Digest(body))
	d := testDownloader(hub)

	require.NoError(t, d.Fetch(context.Background(), f))
	reqs := hub.recorded()
	ranges := make([]string, 0, len(reqs))
	for _, r := range reqs {
		ranges = append(ranges, r.Range)
	}
	want := make([]string, 0, 10)
	for i := range 10 {
		want = append(want, fmt.Sprintf("bytes=%d-%d", i<<10, (i+1)<<10-1))
	}
	assert.ElementsMatch(t, want, ranges)
	assert.LessOrEqual(t, hub.maxInFlight.Load(), int32(d.Window), "the window bounds one file's ranges")
}

func TestFetchRefusesWhatDoesNotMatch(t *testing.T) {
	body := content(5 << 10)
	cases := []struct {
		name  string
		setup func(*testHub)
	}{
		{name: "a corrupted byte", setup: func(h *testHub) { h.corrupt = true }},
		{name: "a file one byte short", setup: func(h *testHub) { h.short = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newTestHub(t, map[string][]byte{"f": body})
			c.setup(hub)
			f := testFile(t, hub, "f", body, sha256Digest(body))

			err := testDownloader(hub).Fetch(context.Background(), f)
			require.Error(t, err)
			assert.Equal(t, ReasonIntegrityMismatch, ReasonOf(err))
			assert.NoFileExists(t, f.Dest, "content that fails verification is not kept")
			assert.NoFileExists(t, f.Checkpoint)
		})
	}
}

func TestFetchRetriesAStalledRangeFromItsLastByte(t *testing.T) {
	body := content(3 << 10)
	hub := newTestHub(t, map[string][]byte{"f": body})
	hub.stallOnce["/f"] = true
	f := testFile(t, hub, "f", body, sha256Digest(body))

	require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
	got, err := os.ReadFile(f.Dest)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	reqs := hub.recorded()
	require.GreaterOrEqual(t, len(reqs), 2)
	// The stalled first range sent half of its 1 KiB before hanging; the retry asks for the rest.
	assert.Equal(t, "bytes=0-1023", reqs[0].Range)
	assert.Equal(t, "bytes=512-1023", reqs[1].Range)
}

func TestFetchClassifiesTheHubsAnswers(t *testing.T) {
	body := content(2 << 10)
	cases := []struct {
		name         string
		status       func(r *http.Request, n int) int
		wantReason   string
		wantRequests int
	}{
		{
			name: "a refused credential is not retried", status: func(*http.Request, int) int { return http.StatusUnauthorized },
			wantReason: ReasonAccessDenied, wantRequests: 1,
		},
		{
			name: "a missing file", status: func(*http.Request, int) int { return http.StatusNotFound },
			wantReason: ReasonAccessDenied, wantRequests: 1,
		},
		{
			name: "a failing hub is retried, then fails the file", status: func(*http.Request, int) int { return http.StatusBadGateway },
			wantReason: ReasonSourceUnavailable, wantRequests: 4,
		},
		{
			name: "a transient failure is retried", status: func(_ *http.Request, n int) int {
				if n == 1 {
					return http.StatusTooManyRequests
				}
				return 0
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newTestHub(t, map[string][]byte{"f": body})
			hub.status = c.status
			f := testFile(t, hub, "f", body, sha256Digest(body))

			err := testDownloader(hub).Fetch(context.Background(), f)
			if c.wantReason == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, c.wantReason, ReasonOf(err))
			assert.Len(t, hub.recorded(), c.wantRequests)
			assert.NotContains(t, err.Error(), testToken)
		})
	}
}

func TestDiskErrorClassifiesAFullDisk(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantCapacity bool
	}{
		{name: "a full disk is capacity", err: &fs.PathError{Op: "write", Path: "/cache/f", Err: syscall.ENOSPC}, wantCapacity: true},
		{name: "another write failure is reported as it is", err: &fs.PathError{Op: "write", Path: "/cache/f", Err: syscall.EACCES}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := new(Downloader).diskError(File{Path: "model.safetensors"}, c.err)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "model.safetensors")
			_, isClassified := errors.AsType[*Error](err)
			assert.Equal(t, c.wantCapacity, isClassified)
			if c.wantCapacity {
				assert.Equal(t, ReasonInsufficientCapacity, ReasonOf(err))
				return
			}
			assert.ErrorIs(t, err, c.err)
		})
	}
}

func TestFetchResumes(t *testing.T) {
	body := content(8 << 10)
	// What happens to the checkpoint between the two runs.
	const (
		intact = iota
		corruptState
		removed
	)
	cases := []struct {
		name       string
		checkpoint int
		wantStart  string
	}{
		{name: "from the checkpoint's hash state", checkpoint: intact, wantStart: "bytes=4096-"},
		{name: "from the checkpoint by re-hashing when its state cannot be restored", checkpoint: corruptState, wantStart: "bytes=4096-"},
		{name: "from the start without a checkpoint", checkpoint: removed, wantStart: "bytes=0-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := newTestHub(t, map[string][]byte{"f": body})
			f := testFile(t, hub, "f", body, sha256Digest(body))

			// A first run canceled once 4 KiB is hashed, the checkpoint interval.
			d := testDownloader(hub)
			d.Window = 1
			ctx, cancel := context.WithCancel(context.Background())
			var received atomic.Int64
			d.Received = func(n int64) {
				if received.Add(n) >= 5<<10 {
					cancel()
				}
			}
			err := d.Fetch(ctx, f)
			require.Error(t, err)
			assert.Equal(t, ReasonCanceled, ReasonOf(err))
			require.FileExists(t, f.Checkpoint)
			switch c.checkpoint {
			case corruptState:
				b, err := os.ReadFile(f.Checkpoint)
				require.NoError(t, err)
				b = []byte(strings.Replace(string(b), `"state":"`, `"state":"AAAA`, 1))
				require.NoError(t, os.WriteFile(f.Checkpoint, b, 0o644))
			case removed:
				require.NoError(t, os.Remove(f.Checkpoint))
			}

			before := len(hub.recorded())
			// The caller seeds what the resume carries over; the file reports only what arrives.
			carried := ResumableOffset(f.Dest, f.Checkpoint, f.Size)
			var held atomic.Int64
			f.Received = func(n int64) { held.Add(n) }
			require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
			got, err := os.ReadFile(f.Dest)
			require.NoError(t, err)
			assert.Equal(t, body, got)
			assert.Equal(t, int64(len(body)), carried+held.Load(),
				"the seed plus what the file reports is the whole content, so progress never goes back")
			resumed := hub.recorded()[before]
			assert.True(t, strings.HasPrefix(resumed.Range, c.wantStart), "resumed with %q", resumed.Range)
		})
	}
}

func TestFetchStartsOverWhenTheHubStopsRanging(t *testing.T) {
	body := content(8 << 10)
	hub := newTestHub(t, map[string][]byte{"f": body})
	f := testFile(t, hub, "f", body, sha256Digest(body))

	// A first run canceled once 4 KiB is hashed, as in TestFetchResumes.
	d := testDownloader(hub)
	d.Window = 1
	ctx, cancel := context.WithCancel(context.Background())
	var received atomic.Int64
	d.Received = func(n int64) {
		if received.Add(n) >= 5<<10 {
			cancel()
		}
	}
	err := d.Fetch(ctx, f)
	require.Error(t, err)
	assert.Equal(t, ReasonCanceled, ReasonOf(err))
	require.FileExists(t, f.Checkpoint)

	// The next run resumes from the checkpoint, but the hub stalls the resumed range and then
	// stops honoring ranges: the file starts over, and everything it was counted as holding —
	// the carried-over offset and the bytes the stall delivered — is told back, negated.
	before := len(hub.recorded())
	hub.stallOnce["/f"] = true
	hub.ignoreRangeFrom = before + 2
	carried := ResumableOffset(f.Dest, f.Checkpoint, f.Size)
	assert.Equal(t, int64(4<<10), carried)
	var held atomic.Int64
	f.Received = func(n int64) { held.Add(n) }
	require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
	got, err := os.ReadFile(f.Dest)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	assert.Equal(t, "bytes=4096-5119", hub.recorded()[before].Range, "the resumed range is stalled")
	assert.Equal(t, int64(len(body)), carried+held.Load(),
		"the seed plus what the file reports is the whole content after the start-over")
}

func TestResumableOffset(t *testing.T) {
	body := content(8 << 10)
	cases := []struct {
		name string
		// checkpoint is the checkpoint file's content; empty means there is no checkpoint.
		checkpoint string
		destBytes  int
		want       int64
	}{
		{name: "no checkpoint"},
		{name: "a checkpoint that is not JSON", checkpoint: "garbage", destBytes: 5 << 10},
		{name: "an offset beyond the size", checkpoint: `{"offset":9000,"state":""}`, destBytes: 9 << 10},
		{name: "a negative offset", checkpoint: `{"offset":-1,"state":""}`, destBytes: 5 << 10},
		{name: "a file shorter than the offset", checkpoint: `{"offset":4096,"state":""}`, destBytes: 3 << 10},
		{name: "a checkpoint the file covers", checkpoint: `{"offset":4096,"state":""}`, destBytes: 5 << 10, want: 4096},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "tree-file")
			if c.destBytes > 0 {
				// The content does not matter: only the size is looked at.
				require.NoError(t, os.WriteFile(dest, make([]byte, c.destBytes), 0o644))
			}
			checkpoint := filepath.Join(dir, "checkpoint")
			if c.checkpoint != "" {
				require.NoError(t, os.WriteFile(checkpoint, []byte(c.checkpoint), 0o600))
			}

			assert.Equal(t, c.want, ResumableOffset(dest, checkpoint, int64(len(body))))
		})
	}
}

func TestFetchSkipsAFileAlreadyVerified(t *testing.T) {
	body := content(3 << 10)
	hub := newTestHub(t, map[string][]byte{"f": body})
	f := testFile(t, hub, "f", body, sha256Digest(body))
	require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
	before := len(hub.recorded())

	require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
	assert.Len(t, hub.recorded(), before, "a verified file is checked, not fetched again")
}

func TestFetchTakesAWholeFileAnsweredToARange(t *testing.T) {
	body := content(5 << 10)
	hub := newTestHub(t, map[string][]byte{"f": body})
	hub.ignoreRange = true
	f := testFile(t, hub, "f", body, sha256Digest(body))

	require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
	assert.Len(t, hub.recorded(), 1, "the whole file came in the first response")
	got, err := os.ReadFile(f.Dest)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestFetchSendsEachRepositoryItsOwnCredential(t *testing.T) {
	a, b := content(700), content(900)
	hub := newTestHub(t, map[string][]byte{"repo-a/f": a, "repo-b/f": b})
	fa := testFile(t, hub, "repo-a/f", a, sha256Digest(a))
	fa.Token = func() string { return "token-a" }
	fb := testFile(t, hub, "repo-b/f", b, sha256Digest(b))
	fb.Token = func() string { return "token-b" }
	fc := testFile(t, hub, "repo-b/f", b, sha256Digest(b))
	fc.Token = func() string { return "" }

	d := testDownloader(hub)
	var wg sync.WaitGroup
	for _, f := range []File{fa, fb, fc} {
		wg.Go(func() { assert.NoError(t, d.Fetch(context.Background(), f)) })
	}
	wg.Wait()

	for _, r := range hub.recorded() {
		switch {
		case strings.HasPrefix(r.Path, "/repo-a/"):
			assert.Equal(t, "Bearer token-a", r.Auth)
		default:
			assert.Contains(t, []string{"Bearer token-b", ""}, r.Auth)
		}
	}
}

func TestFetchDropsTheCredentialOnARedirectToAnotherHost(t *testing.T) {
	body := content(2 << 10)
	content := newTestHub(t, map[string][]byte{"f": body})
	hub := newTestHub(t, nil)
	hub.redirect = content.server
	f := testFile(t, hub, "f", body, sha256Digest(body))

	require.NoError(t, testDownloader(hub).Fetch(context.Background(), f))
	for _, r := range hub.recorded() {
		assert.Equal(t, "Bearer "+testToken, r.Auth, "the hub itself gets the credential")
	}
	require.NotEmpty(t, content.recorded())
	for _, r := range content.recorded() {
		assert.Empty(t, r.Auth, "the content host never does")
	}

	// A failure there names the file and the hub's URL, never the signed redirect.
	content.status = func(*http.Request, int) int { return http.StatusBadGateway }
	err := testDownloader(hub).Fetch(context.Background(), testFile(t, hub, "f", body, sha256Digest(body)))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-signature")
	assert.NotContains(t, err.Error(), testToken)
}

func TestRedirectKeepsTheCredentialOnlyOnTheSameHostAndScheme(t *testing.T) {
	cases := []struct {
		name, from, to string
		wantKept       bool
	}{
		{name: "the same host and scheme", from: "https://hub.example/f", to: "https://hub.example/g", wantKept: true},
		{name: "another host", from: "https://hub.example/f", to: "https://cdn.hub.example/f"},
		{name: "a downgrade to http on the same host", from: "https://hub.example/f", to: "http://hub.example/f"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			from, err := url.Parse(c.from)
			require.NoError(t, err)
			to, err := url.Parse(c.to)
			require.NoError(t, err)
			req := &http.Request{URL: to, Header: http.Header{"Authorization": {"Bearer " + testToken}}}

			require.NoError(t, New(http.DefaultClient, 1, 0).client().CheckRedirect(req, []*http.Request{{URL: from}}))
			assert.Equal(t, c.wantKept, req.Header.Get("Authorization") != "")
		})
	}
}

func TestFetchHonorsTheNodeLimits(t *testing.T) {
	t.Run("concurrency across files", func(t *testing.T) {
		files := map[string][]byte{}
		for i := range 4 {
			files["f"+strconv.Itoa(i)] = content(4 << 10)
		}
		hub := newTestHub(t, files)
		d := testDownloader(hub)
		d.SetLimits(2, 0)

		var wg sync.WaitGroup
		for name, b := range files {
			f := testFile(t, hub, name, b, sha256Digest(b))
			wg.Go(func() { assert.NoError(t, d.Fetch(context.Background(), f)) })
		}
		wg.Wait()
		assert.LessOrEqual(t, hub.maxInFlight.Load(), int32(2))
	})
	t.Run("bandwidth", func(t *testing.T) {
		body := content(96 << 10)
		hub := newTestHub(t, map[string][]byte{"f": body})
		d := testDownloader(hub)
		d.SetLimits(8, 64<<10)
		f := testFile(t, hub, "f", body, sha256Digest(body))

		start := time.Now()
		require.NoError(t, d.Fetch(context.Background(), f))
		// 96 KiB at 64 KiB/s with a 64 KiB burst takes at least half a second.
		assert.GreaterOrEqual(t, time.Since(start), 450*time.Millisecond)
	})
}

func TestFetchUnderALowLimitIsNotAStall(t *testing.T) {
	// Three chunks at a limit of two chunks a second: the third waits about half a second for the
	// limiter, longer than the stall timeout, with the hub delivering all along.
	body := content(3 * readChunk)
	hub := newTestHub(t, map[string][]byte{"f": body})
	f := testFile(t, hub, "f", body, sha256Digest(body))
	d := testDownloader(hub)
	d.SegmentBytes = int64(len(body))
	d.SetLimits(1, 2*readChunk)

	require.NoError(t, d.Fetch(context.Background(), f))
	assert.Len(t, hub.recorded(), 1, "waiting for the node's own limit does not retry the request")
}
