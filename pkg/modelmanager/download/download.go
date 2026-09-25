package download

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

// Defaults of a Downloader's tuning.
const (
	// DefaultSegmentBytes is the byte range one request fetches of a large file.
	DefaultSegmentBytes = 64 << 20
	// DefaultWindow is how many segments may be in flight or finished ahead of the hashed position.
	// With DefaultSegmentBytes it bounds the bytes waiting to be hashed of one file to 256 MiB,
	// which stay in the page cache, so hashing them never reads the disk.
	DefaultWindow = 4
	// DefaultStallTimeout is how long a request may deliver nothing before it is retried from its
	// last received byte.
	DefaultStallTimeout = 30 * time.Second
	// DefaultRetries is how many times one segment is retried before its file fails.
	DefaultRetries = 5
	// DefaultRetryDelay is the pause before a segment's first retry, growing with each one to at
	// most ten times it.
	DefaultRetryDelay = time.Second
	// DefaultCheckpointBytes is how often a file's hashed position is synced and recorded, so a
	// restart resumes rather than starts over.
	DefaultCheckpointBytes = 256 << 20
)

// readChunk is the unit a response body is read and rate-limited in.
const readChunk = 32 << 10

// File is one manifest file to download.
type File struct {
	// Path is the manifest path; errors name the file by it.
	Path string
	// Size and Digest are the manifest's.
	Size   int64
	Digest string
	// URL is where the hub serves it.
	URL string
	// Dest is where it is written, and Checkpoint where its hashed position is recorded, outside
	// the tree Dest belongs to.
	Dest       string
	Checkpoint string
	// Received, when set, is told this file's byte counts as they arrive. A large file's ranges
	// arrive at once, so it is called concurrently and must be safe for that.
	Received func(n int64)
	// Token returns the credential for this file's repository at the moment a request is sent, so a
	// rotated Secret reaches the next request. It returns "" for a public repository. A credential
	// is only ever sent with a request for the repository that presented it. Each range's request
	// calls it, concurrently, so it must be safe for that.
	Token func() string
}

// Downloader downloads files under node-wide limits: at most Concurrency requests at once and at
// most BytesPerSecond, across every download it runs.
type Downloader struct {
	// Client sends the requests, configured with the node's proxy and CA.
	Client *http.Client

	SegmentBytes    int64
	Window          int
	StallTimeout    time.Duration
	Retries         int
	RetryDelay      time.Duration
	CheckpointBytes int64

	// Received, when set, is told every byte count as it arrives, concurrently from every running
	// request, so it must be safe for that.
	Received func(n int64)

	mu      sync.Mutex
	streams chan struct{}
	limiter *rate.Limiter
}

// New returns a downloader with the default tuning and the given limits.
func New(client *http.Client, concurrency int, bytesPerSecond int64) *Downloader {
	d := &Downloader{
		Client:          client,
		SegmentBytes:    DefaultSegmentBytes,
		Window:          DefaultWindow,
		StallTimeout:    DefaultStallTimeout,
		Retries:         DefaultRetries,
		RetryDelay:      DefaultRetryDelay,
		CheckpointBytes: DefaultCheckpointBytes,
	}
	d.SetLimits(concurrency, bytesPerSecond)

	return d
}

// SetLimits replaces the limits; requests already running keep the tokens they hold.
func (d *Downloader) SetLimits(concurrency int, bytesPerSecond int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.streams = make(chan struct{}, max(concurrency, 1))
	d.limiter = nil
	if bytesPerSecond > 0 {
		d.limiter = rate.NewLimiter(rate.Limit(bytesPerSecond), max(int(bytesPerSecond), readChunk))
	}
}

// SetClient replaces the client; requests already sent keep theirs.
func (d *Downloader) SetClient(c *http.Client) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.Client = c
}

func (d *Downloader) limits() (chan struct{}, *rate.Limiter) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.streams, d.limiter
}

// Fetch downloads f into f.Dest, resuming from its checkpoint, and returns once every byte has been
// verified against the manifest in byte order. A file that fails verification is removed, with its
// checkpoint. A file already complete and verified by an earlier call is checked, not fetched.
func (d *Downloader) Fetch(ctx context.Context, f File) error {
	v, err := newVerifier(f.Digest, f.Size)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(f.Dest, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return d.diskError(f, err)
	}
	defer func() { _ = out.Close() }()

	offset, err := v.resume(out, f.Checkpoint)
	if err != nil {
		return d.diskError(f, err)
	}
	if offset < f.Size {
		if err := d.fetchRanges(ctx, f, out, v, offset); err != nil {
			if ReasonOf(err) == ReasonIntegrityMismatch {
				_ = out.Close()
				_ = os.Remove(f.Dest)
				removeCheckpoint(f.Checkpoint)
			}
			return err
		}
	}
	if err := out.Sync(); err != nil {
		return d.diskError(f, err)
	}
	if err := v.check(f.Path); err != nil {
		_ = out.Close()
		_ = os.Remove(f.Dest)
		removeCheckpoint(f.Checkpoint)
		return err
	}

	// A verified file keeps a checkpoint at its end, so an attempt that stops before publishing
	// resumes past it rather than downloading it again.
	return d.checkpoint(f, out, v)
}

// errRangesUnsupported is a hub answering a range request that does not start at zero with the
// whole file.
var errRangesUnsupported = errors.New("the hub does not honor byte ranges")

// fetchRanges downloads [offset, size) and hashes it in order. The first segment goes alone, which
// also learns whether the hub honors ranges: one that answers the whole file is read to its end in
// that one response, and one that does not honor a resume starts the file over. The rest is fetched
// as segments in parallel within the window, each hashed as the contiguous bytes before it complete.
func (d *Downloader) fetchRanges(ctx context.Context, f File, out *os.File, v *verifier, offset int64) error {
	segment := max(d.SegmentBytes, 1)

	reached, err := d.fetchSegment(ctx, f, out, offset, min(offset+segment, f.Size))
	if errors.Is(err, errRangesUnsupported) && offset > 0 {
		if err := out.Truncate(0); err != nil {
			return d.diskError(f, err)
		}
		fresh, verr := newVerifier(f.Digest, f.Size)
		if verr != nil {
			return verr
		}
		*v = *fresh
		offset = 0
		reached, err = d.fetchSegment(ctx, f, out, 0, min(segment, f.Size))
	}
	if err != nil {
		return d.stopped(f, out, v, err)
	}
	if _, err := io.Copy(v, io.NewSectionReader(out, offset, reached-offset)); err != nil {
		return d.diskError(f, err)
	}
	if reached >= f.Size {
		return nil
	}

	return d.fetchParallel(ctx, f, out, v, reached)
}

func (d *Downloader) fetchParallel(ctx context.Context, f File, out *os.File, v *verifier, offset int64) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	segment := max(d.SegmentBytes, 1)
	var starts []int64
	for s := offset; s < f.Size; s += segment {
		starts = append(starts, s)
	}
	window := max(d.Window, 1)

	done := make([]chan struct{}, len(starts))
	for i := range done {
		done[i] = make(chan struct{})
	}
	// slots admits one segment per hashed segment, which keeps the fetched and unhashed bytes of
	// this file within the window.
	slots := make(chan struct{}, window)
	for range window {
		slots <- struct{}{}
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for i, start := range starts {
			select {
			case <-slots:
			case <-ctx.Done():
				return
			}
			end := min(start+segment, f.Size)
			wg.Go(func() {
				if _, err := d.fetchSegment(ctx, f, out, start, end); err != nil {
					cancel(err)
					return
				}
				close(done[i])
			})
		}
	})

	var err error
	lastCheckpoint := v.offset
	for i, start := range starts {
		select {
		case <-done[i]:
		case <-ctx.Done():
			err = context.Cause(ctx)
		}
		if err != nil {
			break
		}
		end := min(start+segment, f.Size)
		if _, err = io.Copy(v, io.NewSectionReader(out, start, end-start)); err != nil {
			err = d.diskError(f, err)
			break
		}
		if v.offset-lastCheckpoint >= d.CheckpointBytes && v.offset < f.Size {
			if err = d.checkpoint(f, out, v); err != nil {
				break
			}
			lastCheckpoint = v.offset
		}
		slots <- struct{}{}
	}
	cancel(err)
	wg.Wait()

	if err == nil {
		return nil
	}

	return d.stopped(f, out, v, err)
}

// stopped records what was hashed for a resume and classifies why the download stopped.
func (d *Downloader) stopped(f File, out *os.File, v *verifier, err error) error {
	_ = d.checkpoint(f, out, v)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return detailedf(ReasonCanceled, "canceled", "%s: canceled", f.Path)
	}
	if errors.Is(err, errRangesUnsupported) {
		return errorf(ReasonSourceUnavailable, "%s: %v", f.Path, err)
	}

	return err
}

// checkpoint syncs the file and records its hashed position. The sync comes first: a hash state for
// bytes a crash could still lose would verify content that is no longer on disk.
func (d *Downloader) checkpoint(f File, out *os.File, v *verifier) error {
	if f.Checkpoint == "" {
		return nil
	}
	if err := out.Sync(); err != nil {
		return d.diskError(f, err)
	}

	return v.saveCheckpoint(f.Checkpoint)
}

// fetchSegment fetches [start, end) into out, retrying from its last received byte on a stall or
// a transient failure, and returns the position it reached: end, or the file's end when the hub
// answered the first range with the whole file.
func (d *Downloader) fetchSegment(ctx context.Context, f File, out *os.File, start, end int64) (int64, error) {
	var lastErr error
	for attempt := 0; attempt <= max(d.Retries, 0); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return start, context.Cause(ctx)
			case <-time.After(d.RetryDelay * time.Duration(min(attempt, 10))):
			}
		}
		reached, err := d.fetchRange(ctx, f, out, start, end)
		progressed := reached > start
		start = reached
		switch {
		case err == nil && start >= end:
			return start, nil
		case err == nil:
			lastErr = errorf(ReasonSourceUnavailable, "%s: the hub ended a range %d bytes short", f.Path, end-start)
		case errors.Is(err, errRangesUnsupported), ReasonOf(err) == ReasonAccessDenied,
			ReasonOf(err) == ReasonIntegrityMismatch, ReasonOf(err) == ReasonInsufficientCapacity, ctx.Err() != nil:
			return start, err
		default:
			lastErr = err
		}
		if progressed {
			// Progress resets the count: only a range failing again and again fails the file.
			attempt = 0
		}
	}

	return start, detailedf(ReasonSourceUnavailable, DetailOf(lastErr), "%s: bytes %d to %d failed %d times: %v",
		f.Path, start, end, d.Retries+1, lastErr)
}

// fetchRange sends one request for [start, end) and writes what arrives at its offsets, returning
// the position it reached.
func (d *Downloader) fetchRange(ctx context.Context, f File, out *os.File, start, end int64) (int64, error) {
	streams, limiter := d.limits()
	select {
	case streams <- struct{}{}:
	case <-ctx.Done():
		return start, context.Cause(ctx)
	}
	defer func() { <-streams }()

	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, f.URL, nil)
	if err != nil {
		return start, errorf(ReasonInvalidRequest, "%s: %v", f.Path, err)
	}
	req.Header.Set("User-Agent", "gpustack-operator")
	req.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end-1, 10))
	if f.Token != nil {
		if token := f.Token(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	// A stall timer cancels this request, and only it, when nothing arrives for StallTimeout: no
	// answer at all, as much as a body that stops. The client has no overall timeout, which would cut
	// a large range short on a slow link.
	stall := time.AfterFunc(d.StallTimeout, cancelReq)
	defer stall.Stop()

	resp, err := d.client().Do(req)
	if err != nil {
		return start, detailedf(ReasonSourceUnavailable, transportDetail(err), "%s: GET %s: %v", f.Path, f.URL, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	stall.Reset(d.StallTimeout)

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		rangeStart, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		switch {
		case !ok || rangeStart != start:
			return start, errorf(ReasonSourceUnavailable, "%s: the hub answered another range %q", f.Path,
				resp.Header.Get("Content-Range"))
		case total >= 0 && total != f.Size:
			return start, errorf(ReasonIntegrityMismatch, "%s: the hub serves %d bytes, the manifest says %d",
				f.Path, total, f.Size)
		}
	case resp.StatusCode == http.StatusOK && start == 0:
		// The hub ignored the range and sends the whole file: all of it is taken from this response.
		if resp.ContentLength >= 0 && resp.ContentLength != f.Size {
			return start, errorf(ReasonIntegrityMismatch, "%s: the hub serves %d bytes, the manifest says %d",
				f.Path, resp.ContentLength, f.Size)
		}
		end = f.Size
	case resp.StatusCode == http.StatusOK:
		return start, errRangesUnsupported
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusNotFound:
		return start, detailedf(ReasonAccessDenied, "HTTP "+strconv.Itoa(resp.StatusCode),
			"%s: the file does not exist or is not accessible with the credential (HTTP %d)", f.Path, resp.StatusCode)
	default:
		return start, detailedf(ReasonSourceUnavailable, "HTTP "+strconv.Itoa(resp.StatusCode), "%s: HTTP %d", f.Path, resp.StatusCode)
	}

	buf := make([]byte, readChunk)
	pos := start
	for pos < end {
		want := min(int64(len(buf)), end-pos)
		if limiter != nil {
			// Waiting for the node's own bandwidth limit is not the hub stalling: the stall timer is
			// held while the limiter waits, however long a low limit makes that wait.
			stall.Stop()
			if err := limiter.WaitN(ctx, int(want)); err != nil {
				return pos, context.Cause(ctx)
			}
			stall.Reset(d.StallTimeout)
		}
		n, readErr := io.ReadFull(resp.Body, buf[:want])
		if n > 0 {
			stall.Reset(d.StallTimeout)
			if _, err := out.WriteAt(buf[:n], pos); err != nil {
				return pos, d.diskError(f, err)
			}
			pos += int64(n)
			if d.Received != nil {
				d.Received(int64(n))
			}
			if f.Received != nil {
				f.Received(int64(n))
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				if pos < end {
					return pos, errorf(ReasonSourceUnavailable, "%s: the hub closed the range early", f.Path)
				}
				break
			}
			if ctx.Err() != nil {
				return pos, context.Cause(ctx)
			}
			return pos, errorf(ReasonSourceUnavailable, "%s: read: %v", f.Path, readErr)
		}
	}

	return pos, nil
}

// client is Client with redirects that drop the credential when they leave the requested host or
// downgrade from https to http. Go forwards it to a subdomain, and a hub's content host may be one;
// a signed redirect needs nothing of the caller's, and a downgrade would send it in the clear.
func (d *Downloader) client() *http.Client {
	d.mu.Lock()
	c := *d.Client
	d.mu.Unlock()
	next := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && (req.URL.Host != via[0].URL.Host ||
			via[0].URL.Scheme == "https" && req.URL.Scheme != "https") {
			req.Header.Del("Authorization")
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}

	return &c
}

// parseContentRange reads "bytes <start>-<end>/<total>", with total -1 for "*".
func parseContentRange(header string) (start, total int64, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes ")
	if !found {
		return 0, 0, false
	}
	span, size, found := strings.Cut(spec, "/")
	first, _, found2 := strings.Cut(span, "-")
	if !found || !found2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if size == "*" {
		return start, -1, true
	}
	total, err = strconv.ParseInt(size, 10, 64)
	if err != nil {
		return 0, 0, false
	}

	return start, total, true
}

// diskError classifies a local write failure: a full disk is capacity, anything else is not
// something the hub caused and is reported as it is.
func (d *Downloader) diskError(f File, err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return detailedf(ReasonInsufficientCapacity, "no space left on the cache's filesystem",
			"%s: no space left on the cache's filesystem", f.Path)
	}

	return fmt.Errorf("%s: %w", f.Path, err)
}

// transportDetail names the kind of a request that got no answer, and nothing of its URL.
func transportDetail(err error) string {
	var (
		netErr   net.Error
		certErr  *tls.CertificateVerificationError
		unknownA x509.UnknownAuthorityError
		hostErr  x509.HostnameError
		recErr   tls.RecordHeaderError
	)
	switch {
	case errors.As(err, &certErr), errors.As(err, &unknownA), errors.As(err, &hostErr), errors.As(err, &recErr):
		return "a TLS failure"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "a timeout"
	default:
		return "a connection failure"
	}
}

// unwrapURLError drops the request the transport error names: after a redirect that is the target,
// which may be a signed URL.
func unwrapURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}

	return err
}
