// Package materialize runs the node's materializations: one attempt per digest at a time, that
// recomputes the manifest, reserves room, downloads and verifies every file, and publishes; with a
// backoff per digest after a failure and a cancellation once nobody waits.
package materialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/driver"
	"gpustack.ai/gpustack/pkg/modelmanager/metrics"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// Defaults of the materializer's timing.
const (
	// DefaultWaiterWindow is how long an attempt runs after the last mount that asked for it: more
	// than kubelet's longest mount retry interval, 2 minutes 2 seconds, so a Pod still waiting always
	// asks again within it.
	DefaultWaiterWindow = 5 * time.Minute
	// DefaultBackoff and DefaultMaxBackoff are the wait after a first failure and its cap; each
	// further failure doubles it.
	DefaultBackoff    = time.Minute
	DefaultMaxBackoff = time.Hour
	// DefaultCheckInterval is how often a running attempt checks for waiters.
	DefaultCheckInterval = 30 * time.Second
	// fileParallelism bounds the files one attempt downloads at once; the node-wide request limit
	// is the downloader's.
	fileParallelism = 16
)

// Hub is the model hub as the node reaches it.
type Hub interface {
	ListManifest(ctx context.Context, repository, commit, token string, filter modelartifact.Filter) (modelartifact.Manifest, error)
	FileURL(repository, commit, path string) string
}

// Environment returns the hub and the downloader built from the node's current effective
// configuration, or an InvalidRequest error while that configuration is invalid.
type Environment func(ctx context.Context) (Hub, *download.Downloader, error)

// Materializer implements driver.Materializer.
type Materializer struct {
	Store       *store.Store
	Environment Environment
	// Reserve makes room for bytes more on the cache's filesystem, or returns an
	// InsufficientCapacity error; nil reserves nothing.
	Reserve func(ctx context.Context, bytes int64) error
	// Changed is told a digest's state changed, for status.
	Changed func(hex string)
	Now     func() time.Time
	// Base is the context every attempt derives from, so stopping the plugin stops its downloads;
	// nil is context.Background.
	Base context.Context

	WaiterWindow  time.Duration
	Backoff       time.Duration
	MaxBackoff    time.Duration
	CheckInterval time.Duration

	mu   sync.Mutex
	jobs map[string]*job
}

var _ driver.Materializer = (*Materializer)(nil)

// job is one running attempt at a digest.
type job struct {
	hex      string
	cancel   context.CancelCauseFunc
	asked    atomic.Int64 // unix nanoseconds of the last mount that asked
	received atomic.Int64
	total    atomic.Int64

	mu sync.Mutex
	// sources are the artifacts that asked, by UID, each with its own repository and credential,
	// in the order they first asked.
	sources map[string]*source
	order   []string
}

// source is one artifact's way to the content: its repository at its commit, and the credential its
// namespace's mount handed over. A credential is only ever sent for its own repository.
type source struct {
	repository string
	commit     string
	filter     modelartifact.Filter
	token      atomic.Value
}

func (s *source) currentToken() string {
	t, _ := s.token.Load().(string)
	return t
}

// errNoWaiters cancels an attempt nobody asked for within the waiter window.
var errNoWaiters = errors.New("no mount asked for it")

func (m *Materializer) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Ensure starts or joins the materialization of req's digest and says where it stands.
func (m *Materializer) Ensure(_ context.Context, req driver.Request) driver.Progress {
	if m.Store.IsPublished(req.Hex) {
		return driver.Progress{Published: true}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs == nil {
		m.jobs = map[string]*job{}
	}
	if j, ok := m.jobs[req.Hex]; ok {
		j.join(req, m.now())
		return j.progress()
	}
	// An attempt may have published and left between the check above and the lock.
	if m.Store.IsPublished(req.Hex) {
		return driver.Progress{Published: true}
	}

	rec, err := m.Store.ReadDigest(req.Hex)
	if err != nil {
		return driver.Progress{Code: codes.Unavailable, Message: fmt.Sprintf("read the node's ledger: %v", err)}
	}
	if rec.Reason != "" && m.now().Before(rec.RetryTime) {
		return backingOff(req.Hex, rec)
	}

	base := m.Base
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancelCause(base)
	j := &job{hex: req.Hex, cancel: cancel, sources: map[string]*source{}}
	j.join(req, m.now())
	m.jobs[req.Hex] = j
	go m.run(ctx, j)

	return j.progress()
}

// Downloading returns the digests being materialized.
func (m *Materializer) Downloading() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := make(map[string]bool, len(m.jobs))
	for hex := range m.jobs {
		running[hex] = true
	}

	return running
}

func (j *job) join(req driver.Request, now time.Time) {
	j.asked.Store(now.UnixNano())
	ma := req.Artifact
	uid := string(ma.UID)

	j.mu.Lock()
	defer j.mu.Unlock()
	s, ok := j.sources[uid]
	if !ok {
		s = &source{
			repository: ma.Spec.Source.HuggingFace.Repository,
			commit:     ma.Status.Resolved.Revision,
			filter:     modelartifact.Filter{Allow: ma.Spec.AllowPatterns, Ignore: ma.Spec.IgnorePatterns},
		}
		j.sources[uid] = s
		j.order = append(j.order, uid)
	}
	// The credential this namespace's mount handed over now; a rotated Secret reaches the next request.
	s.token.Store(req.Token)
}

func (j *job) snapshotSources() []*source {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*source, 0, len(j.order))
	for _, uid := range j.order {
		out = append(out, j.sources[uid])
	}

	return out
}

func (j *job) progress() driver.Progress {
	short := j.hex[:12]
	total := j.total.Load()
	if total == 0 {
		return driver.Progress{Code: codes.Aborted, Message: fmt.Sprintf("materializing sha256:%s: listing the files", short)}
	}

	return driver.Progress{Code: codes.Aborted, Message: fmt.Sprintf("materializing sha256:%s: %s of %s received",
		short, humanBytes(j.received.Load()), humanBytes(total))}
}

func backingOff(hex string, rec store.DigestRecord) driver.Progress {
	code := codes.Unavailable
	if rec.Reason == download.ReasonInsufficientCapacity {
		code = codes.ResourceExhausted
	}

	return driver.Progress{Code: code, Message: fmt.Sprintf("materializing sha256:%s failed, %s: %s; the next attempt starts at %s",
		hex[:12], rec.Reason, rec.Message, rec.RetryTime.UTC().Format(time.RFC3339))}
}

// run is one attempt, from its number to its publication or its failure.
func (m *Materializer) run(ctx context.Context, j *job) {
	start := m.now()
	m.changed(j.hex)

	stopWatch := m.watchWaiters(ctx, j)
	err := m.attempt(ctx, j)
	stopWatch()
	j.cancel(nil)

	// The outcome is recorded before the attempt leaves the running set, so a mount that finds no
	// running attempt always finds its backoff.
	if err := m.record(j.hex, err, start); err != nil {
		klog.ErrorS(err, "record the attempt's outcome", "digest", j.hex)
	}
	m.mu.Lock()
	delete(m.jobs, j.hex)
	m.mu.Unlock()
	m.changed(j.hex)
}

// watchWaiters cancels the attempt once no mount asked for it within the waiter window. The files
// it wrote stay for a later attempt to resume.
func (m *Materializer) watchWaiters(ctx context.Context, j *job) func() {
	interval := m.CheckInterval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	window := m.WaiterWindow
	if window <= 0 {
		window = DefaultWaiterWindow
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if m.now().Sub(time.Unix(0, j.asked.Load())) > window {
					j.cancel(errNoWaiters)
					return
				}
			}
		}
	}()

	return func() { close(done) }
}

func (m *Materializer) attempt(ctx context.Context, j *job) error {
	hub, dl, err := m.Environment(ctx)
	if err != nil {
		return err
	}
	a, err := m.Store.NewAttempt(j.hex)
	if err != nil {
		return err
	}

	var lastErr error
	for _, src := range j.snapshotSources() {
		lastErr = m.fromSource(ctx, j, a, hub, dl, src)
		// Only a refusal of this source's credential is worth another source: the content is the
		// same wherever it is authorized, and any other failure would repeat.
		if lastErr == nil || reasonOf(lastErr) != download.ReasonAccessDenied {
			break
		}
	}
	if lastErr != nil {
		return lastErr
	}

	return nil
}

func (m *Materializer) fromSource(
	ctx context.Context, j *job, a *store.Attempt, hub Hub, dl *download.Downloader, src *source,
) error {
	manifest, err := hub.ListManifest(ctx, src.repository, src.commit, src.currentToken(), src.filter)
	if err != nil {
		return manifestError(err)
	}
	if manifest.Digest != "sha256:"+j.hex {
		return &download.Error{Reason: download.ReasonIntegrityMismatch, Message: fmt.Sprintf(
			"the hub lists %s at commit %s as %s, the artifact's digest is sha256:%s",
			src.repository, src.commit, manifest.Digest, j.hex), Detail: "the hub lists " + manifest.Digest}
	}
	j.total.Store(manifest.SizeBytes)

	if m.Reserve != nil {
		// A resumed attempt already holds some of the bytes on disk, and the filesystem's usage
		// counts them, so only the rest is reserved.
		if err := m.Reserve(ctx, max(manifest.SizeBytes-a.SizeBytes(), 0)); err != nil {
			return err
		}
	}

	stateDir := filepath.Join(a.Dir(), "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fileParallelism)
	// Every path is checked before the first download starts, and a file's directory is made inside
	// its own download: once one has started, the attempt only ends through Wait, so no download
	// is left writing into the attempt after it was recorded.
	dests := make([]string, len(manifest.Entries))
	for i, e := range manifest.Entries {
		dest, err := a.FilePath(e.Path)
		if err != nil {
			return &download.Error{Reason: download.ReasonIntegrityMismatch, Message: err.Error()}
		}
		dests[i] = dest
	}
	for i, e := range manifest.Entries {
		sum := sha256.Sum256([]byte(e.Path))
		f := download.File{
			Path: e.Path, Size: e.Size, Digest: e.Digest,
			URL:        hub.FileURL(src.repository, src.commit, e.Path),
			Dest:       dests[i],
			Checkpoint: filepath.Join(stateDir, hex.EncodeToString(sum[:])),
			Token:      src.currentToken,
			Received:   func(n int64) { j.received.Add(n) },
		}
		g.Go(func() error {
			if err := os.MkdirAll(filepath.Dir(f.Dest), 0o755); err != nil {
				return err
			}
			return dl.Fetch(gctx, f)
		})
	}
	if err := g.Wait(); err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, errNoWaiters) {
			const msg = "no mount asked for it within the waiter window"
			return &download.Error{Reason: download.ReasonCanceled, Message: msg, Detail: msg}
		}
		return err
	}
	// The resume state lives beside the tree, not in it, and does not travel with the tree.
	if err := os.RemoveAll(stateDir); err != nil {
		return err
	}

	return a.Publish(store.Marker{
		Digest: manifest.Digest, ManifestFormat: modelartifact.ManifestHeader,
		SizeBytes: manifest.SizeBytes, FileCount: manifest.FileCount, PublishedTime: m.now(),
	})
}

// record writes the attempt's outcome to the ledger: a success clears the failures, a failure backs
// off, doubling per consecutive failure up to the cap, and a cancellation or a configuration the
// node cannot use yet leaves the next mount free to start at once. The configuration is not the
// source's fault: it changes by itself when the worker writes the node's spec, and a backoff would
// hold every mount for a minute after it did.
func (m *Materializer) record(hex string, err error, start time.Time) error {
	return m.Store.UpdateDigest(hex, func(rec *store.DigestRecord) { m.outcome(hex, rec, err, start) })
}

func (m *Materializer) outcome(hex string, rec *store.DigestRecord, err error, start time.Time) {
	now := m.now()
	switch reason := reasonOf(err); {
	case err == nil:
		rec.Failures, rec.Reason, rec.Message, rec.RetryTime = 0, "", "", time.Time{}
		rec.LastUsedTime = now
		metrics.Materializations.WithLabelValues("published").Inc()
		metrics.PublishDuration.Observe(now.Sub(start).Seconds())
		klog.InfoS("published", "digest", hex, "seconds", now.Sub(start).Seconds())
	case reason == download.ReasonCanceled, reason == download.ReasonInvalidRequest:
		rec.Reason, rec.Message, rec.RetryTime = reason, publicMessage(err), now
		metrics.Materializations.WithLabelValues(reason).Inc()
		klog.V(1).InfoS("materialization stopped", "digest", hex, "reason", reason, "error", messageOf(err))
	default:
		rec.Failures++
		rec.Reason, rec.Message = reason, publicMessage(err)
		rec.RetryTime = now.Add(m.backoff(rec.Failures))
		metrics.Materializations.WithLabelValues(reason).Inc()
		klog.InfoS("materialization failed", "digest", hex, "reason", reason, "error", messageOf(err),
			"retryTime", rec.RetryTime)
	}
}

func (m *Materializer) backoff(failures int) time.Duration {
	base, ceiling := m.Backoff, m.MaxBackoff
	if base <= 0 {
		base = DefaultBackoff
	}
	if ceiling <= 0 {
		ceiling = DefaultMaxBackoff
	}
	d := base
	for i := 1; i < failures && d < ceiling; i++ {
		d *= 2
	}

	return min(d, ceiling)
}

func (m *Materializer) changed(hex string) {
	if m.Changed != nil {
		m.Changed(hex)
	}
}

// manifestError classifies a failure to list the manifest: the hub's refusal and unavailability keep
// their reasons, and a listing the hub answers but that is not the artifact's content is an
// integrity mismatch.
func manifestError(err error) error {
	detail := httpStatusPattern.FindString(err.Error())
	switch r := modelartifact.ReasonOf(err); r {
	case modelartifact.ReasonAccessDenied, modelartifact.ReasonSourceUnavailable:
		return &download.Error{Reason: r, Message: err.Error(), Detail: detail}
	default:
		return &download.Error{Reason: download.ReasonIntegrityMismatch, Message: err.Error(), Detail: detail}
	}
}

// httpStatusPattern finds the hub's status in a listing failure, the one part of its message that
// names no repository.
var httpStatusPattern = regexp.MustCompile(`\bHTTP [0-9]{3}\b`)

// reasonMeanings say what each reason means, in words that name no tenant.
var reasonMeanings = map[string]string{
	download.ReasonInvalidRequest:       "the node cannot run the download with its configuration",
	download.ReasonAccessDenied:         "the hub refused the credential, or a file does not exist",
	download.ReasonSourceUnavailable:    "the hub could not be reached or failed",
	download.ReasonIntegrityMismatch:    "the content, or the hub's file list, does not match the artifact's digest",
	download.ReasonInsufficientCapacity: "the node's cache has no room for it",
	download.ReasonCanceled:             "the attempt was canceled",
}

// publicMessage is a failure as the node's status and every mount's error carry it. A digest is
// shared by every tenant whose artifact resolves to it, and the status is read across tenants, so
// it names no repository, URL, file or namespace: only what the reason means and the error's
// tenant-free detail, such as the hub's HTTP status. The full error goes to the plugin's log.
func publicMessage(err error) string {
	msg := reasonMeanings[reasonOf(err)]
	if msg == "" {
		msg = "the attempt failed"
	}
	if d := download.DetailOf(err); d != "" {
		msg += ": " + d
	}

	return msg
}

func reasonOf(err error) string {
	return download.ReasonOf(err)
}

func messageOf(err error) string {
	if e, ok := errors.AsType[*download.Error](err); ok {
		return e.Message
	}

	return err.Error()
}

func humanBytes(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
