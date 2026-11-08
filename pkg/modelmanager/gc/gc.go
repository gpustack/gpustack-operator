// Package gc keeps a node's model cache within its watermarks: it removes only content no Pod
// references, oldest use first, and reserves room before a download starts.
package gc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/metrics"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// Defaults of collection.
const (
	// DefaultGrace keeps a tree whose last reference went recently, so a Pod being replaced finds
	// its content again.
	DefaultGrace = 10 * time.Minute
	// DefaultPartialTTL is how long an attempt's files nobody asks for stay for a resume.
	DefaultPartialTTL = 24 * time.Hour
)

// Usage is a filesystem's size and use.
type Usage struct {
	Total uint64
	Used  uint64
}

// Percent is the use as a percentage.
func (u Usage) Percent() float64 {
	if u.Total == 0 {
		return 0
	}
	return float64(u.Used) / float64(u.Total) * 100
}

// Collector removes what the cache may remove.
type Collector struct {
	Store *store.Store
	// Usage reads the cache filesystem's usage.
	Usage func() (Usage, error)
	// Watermarks are the effective high and low watermarks, in percent, or why there are none to
	// collect against: no configuration applied yet, or the node's configuration failing its check.
	// Either way nothing is collected, rather than collect under watermarks not in force.
	Watermarks func() (high, low int32, unavailable string)
	// Referenced and Downloading are the digests that must stay. References that cannot be read
	// are an error, never an empty set: an empty set would make every mounted tree removable.
	Referenced  func() (map[string]bool, error)
	Downloading func() map[string]bool
	// Pinned are the digests the node's configuration says to retain: never collection candidates,
	// though they still count toward the filesystem's usage and so keep the saturated state honest.
	// Nil means nothing is pinned. A view whose source digest is pinned is kept too: a source
	// tree's files must stay restorable for the view's sessions while the generation is admitted.
	Pinned func() map[string]bool
	// ViewRoot is the directory of this node's metadata views,
	// <store-root>/model-transfer/views, swept beside the published layout.
	// Its terminal content-addressed view directories are reclaimed like
	// published trees: only with no reference naming their source digest and
	// past the same grace, under the claim lock so a builder promoting a view
	// or a mount resolving it cannot race the removal. Empty keeps none.
	ViewRoot string
	Now      func() time.Time
	Grace    time.Duration
	// PartialTTL is how long an attempt's files nobody asks for stay for a resume.
	PartialTTL time.Duration

	// mu protects reservations. collectMu keeps collection passes from overlapping.
	mu        sync.Mutex
	collectMu sync.Mutex
	// reservations are the running downloads' reservations, by their token.
	reservations map[*reservation]struct{}
}

// reservation is a running download's claim on the filesystem: its whole size, of which written
// is already on disk and so already in the filesystem's usage.
type reservation struct {
	size    int64
	written func() int64
}

type reserveCheck struct {
	release  func()
	bytes    int64
	incoming int64
	usage    Usage
	high     int32
}

// Result is what a collection did.
type Result struct {
	RemovedBytes int64
	// CapacityLow says usage stays above the high watermark with nothing the collector may remove.
	CapacityLow bool
	Usage       Usage
	// Skipped says why no published tree was considered this time, empty when they were.
	Skipped string
}

// Collect removes stale partials, and, above the high watermark, unreferenced trees past their
// grace, oldest use first, until usage is at or below the low watermark. It never removes a
// referenced tree or the files of a running attempt.
func (c *Collector) Collect(ctx context.Context) (Result, error) {
	c.collectMu.Lock()
	defer c.collectMu.Unlock()

	c.mu.Lock()
	incoming := c.inFlight()
	c.mu.Unlock()

	return c.collect(ctx, incoming)
}

// Reserve makes room for a download of sizeBytes whose attempt holds written() of them on disk so
// far, and holds the room until the returned release is called. The rest of the download, together
// with the unwritten rest of every other running reservation, must fit under the high watermark:
// reservations that each fit and together do not are never all admitted. If they would not fit it
// collects, and if they still would not, it refuses with InsufficientCapacity.
func (c *Collector) Reserve(ctx context.Context, hex string, sizeBytes int64, written func() int64) (func(), error) {
	check, err := c.tryReserve(sizeBytes, written)
	if err != nil || check.release != nil {
		return check.release, err
	}

	c.collectMu.Lock()
	defer c.collectMu.Unlock()
	check, err = c.tryReserve(sizeBytes, written)
	if err != nil || check.release != nil {
		return check.release, err
	}
	res, err := c.collect(ctx, check.incoming)
	if err != nil {
		return nil, err
	}

	check, err = c.tryReserve(sizeBytes, written)
	if err != nil || check.release != nil {
		return check.release, err
	}
	// The node's own numbers name no tenant, so the whole message is the detail.
	if res.Skipped != "" {
		return nil, nodeCapacityError(fmt.Sprintf("%d bytes of sha256:%s do not fit under the high watermark of %d%%, "+
			"and collection was skipped: %s", check.bytes, hex[:12], check.high, res.Skipped))
	}
	return nil, nodeCapacityError(fmt.Sprintf("%d bytes of sha256:%s do not fit under the high watermark of %d%%: "+
		"the cache's filesystem is %.1f%% used of %d bytes with %d more reserved by running downloads, "+
		"and nothing unreferenced is left to remove",
		check.bytes, hex[:12], check.high, check.usage.Percent(), check.usage.Total, check.incoming-check.bytes))
}

// tryReserve checks capacity and records a fitting reservation while holding c.mu.
func (c *Collector) tryReserve(sizeBytes int64, written func() int64) (reserveCheck, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var check reserveCheck
	var unavailable string
	check.high, _, unavailable = c.Watermarks()
	if unavailable != "" {
		return check, &download.Error{Reason: download.ReasonInvalidRequest, Message: unavailable, Detail: unavailable}
	}
	check.bytes = max(sizeBytes-written(), 0)
	check.incoming = check.bytes + c.inFlight()
	var err error
	check.usage, err = c.Usage()
	if err != nil {
		return check, err
	}
	if fits(check.usage, check.incoming, check.high) {
		check.release = c.addReservation(sizeBytes, written)
	}

	return check, nil
}

// addReservation records a claim while c.mu is held.
func (c *Collector) addReservation(sizeBytes int64, written func() int64) func() {
	r := &reservation{size: sizeBytes, written: written}
	if c.reservations == nil {
		c.reservations = map[*reservation]struct{}{}
	}
	c.reservations[r] = struct{}{}

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.reservations, r)
	}
}

// inFlight is what the running reservations have yet to write. The caller holds c.mu.
func (c *Collector) inFlight() int64 {
	var n int64
	for r := range c.reservations {
		n += max(r.size-r.written(), 0)
	}

	return n
}

func nodeCapacityError(msg string) error {
	return &download.Error{Reason: download.ReasonInsufficientCapacity, Message: msg, Detail: msg}
}

func fits(u Usage, bytes int64, high int32) bool {
	if u.Total == 0 {
		return false
	}

	return float64(u.Used+uint64(max(bytes, 0)))/float64(u.Total)*100 <= float64(high) // nolint: gosec
}

func (c *Collector) collect(ctx context.Context, incoming int64) (Result, error) {
	var res Result
	now := c.Now()

	// Without watermarks in force nothing is removed, not even a stale partial: the configuration
	// that would decide what the node keeps is not the one in force.
	high, low, unavailable := c.Watermarks()
	if unavailable != "" {
		u, err := c.Usage()
		if err != nil {
			return res, err
		}
		res.Usage, res.Skipped = u, unavailable
		return c.done(res, 0), nil
	}

	partials, err := c.Store.Partials()
	if err != nil {
		return res, err
	}
	// The running attempts are read after the listing, and again before each removal, so an attempt
	// that starts meanwhile is not taken for an idle one.
	for _, p := range partials {
		if now.Sub(p.ModTime) < c.partialTTL() || c.Downloading()[p.Hex] {
			continue
		}
		if err := c.Store.RemovePartial(p.Hex, p.Attempt); err != nil {
			return res, err
		}
		c.removed(&res, p.SizeBytes)
	}

	if err := c.pruneRecords(now); err != nil {
		return res, err
	}

	u, err := c.Usage()
	if err != nil {
		return res, err
	}
	res.Usage = u

	// References that cannot be read skip this collection rather than fail it: the report still
	// runs and says why nothing was removed. Terminal metadata views need the same set as
	// published trees, so it is read before either is considered.
	referenced, refErr := c.Referenced()
	if res.Skipped = unreadableReferences(refErr); res.Skipped != "" {
		return c.done(res, high), nil
	}

	// Terminal views go before the watermark check: like partials they are residue of finished
	// work, and the usage they freed decides whether trees must go at all.
	viewsGone, err := c.reclaimViews(ctx, referenced)
	if viewsGone {
		if u, uerr := c.Usage(); uerr == nil {
			res.Usage = u
		} else if err == nil {
			return res, uerr
		}
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		if store.IsReferenceReadError(err) {
			res.Skipped = unreadableReferences(err)
			return c.done(res, high), nil
		}
		return res, err
	}
	if fits(res.Usage, incoming, high) {
		return c.done(res, high), nil
	}

	candidates, err := c.candidates(now, referenced)
	if err != nil {
		return res, err
	}
	for _, cand := range candidates {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if fits(res.Usage, incoming, low) {
			break
		}
		if c.Downloading()[cand.hex] || (c.Pinned != nil && c.Pinned()[cand.hex]) {
			continue
		}
		rec, err := c.Store.ReadDigest(cand.hex)
		if err != nil {
			return res, err
		}
		lastUsed := rec.LastUsedTime
		if lastUsed.IsZero() {
			lastUsed = cand.lastUsed
		}
		if c.Now().Sub(lastUsed) < c.grace() {
			continue
		}
		// The store checks live references under the lock a mount claims its tree with, and
		// answers a caller whose context is done before it touches anything.
		removed, refErr := c.Store.RemoveUnreferenced(ctx, cand.hex)
		if refErr != nil {
			// The caller's own cancellation is the collection's outcome, not a reason to skip.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return res, ctxErr
			}
			if res.Skipped = unreadableReferences(refErr); res.Skipped != "" {
				break
			}
		}
		if !removed {
			continue
		}
		c.removed(&res, cand.size)
		if res.Usage, err = c.Usage(); err != nil {
			return res, err
		}
	}

	return c.done(res, high), nil
}

// pruneRecords drops the failure records that no longer describe anything on the node: the digest is
// neither published nor partly on disk nor being materialized, and its last failure is older than a
// partial's TTL. Without it every digest that ever failed would stay listed as Failed.
func (c *Collector) pruneRecords(now time.Time) error {
	records, err := c.Store.Digests()
	if err != nil {
		return err
	}
	partials, err := c.Store.Partials()
	if err != nil {
		return err
	}
	onDisk := map[string]bool{}
	for _, p := range partials {
		onDisk[p.Hex] = true
	}
	for hex, rec := range records {
		if rec.Reason == "" || now.Sub(rec.RetryTime) < c.partialTTL() || onDisk[hex] || c.Store.IsPublished(hex) ||
			c.Downloading()[hex] {
			continue
		}
		if err := c.Store.RemoveDigest(hex); err != nil {
			return err
		}
	}

	return nil
}

// unreadableReferences is why a collection is skipped when the references cannot be read, or "".
func unreadableReferences(err error) string {
	if err == nil {
		return ""
	}
	return "the references cannot be read, so no tree is known to be unmounted: " + err.Error()
}

// reclaimViews removes the terminal metadata views under ViewRoot: final
// content-addressed directories whose last reference is gone. Everything
// with a reason to stay stays, the same reasons a published tree has: a
// live reference naming the view's source digest, a source digest the
// node's configuration pins, a use within the grace, and references that
// cannot be read (the caller skips this pass for them before asking).
// Staging residue, lock files and anything unrecognizable are recovery's
// and the metadata package's, not the collector's: only a final directory
// named as the metadata package addresses views is a candidate, and the
// record names the digest whose references and pins decide.
// A record that cannot be read, or one naming no digest, is ownership the
// collector cannot prove gone: the view stays.
//
// A view's byte size is not in its directory entries, so a reclaimed view
// reports no bytes; the next usage read says what its removal freed.
func (c *Collector) reclaimViews(ctx context.Context, referenced map[string]bool) (bool, error) {
	if c.ViewRoot == "" {
		return false, nil
	}
	entries, err := os.ReadDir(c.ViewRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var pinned map[string]bool
	if c.Pinned != nil {
		pinned = c.Pinned()
	}
	anyGone := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return anyGone, err
		}
		name := entry.Name()
		if !isViewID(name) {
			continue
		}
		view, err := metadata.Load(filepath.Join(c.ViewRoot, name))
		if err != nil {
			continue
		}
		hex := store.HexOf(view.ArtifactDigest)
		if hex == "" || referenced[hex] || (pinned != nil && pinned[hex]) {
			continue
		}
		rec, err := c.Store.ReadDigest(hex)
		if err != nil {
			klog.ErrorS(err, "read metadata view digest record", "view", name, "digest", view.ArtifactDigest)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				klog.ErrorS(err, "stat metadata view", "view", name)
			}
			continue
		}
		lastUsed := info.ModTime()
		if rec.LastUsedTime.After(lastUsed) {
			lastUsed = rec.LastUsedTime
		}
		if c.Now().Sub(lastUsed) < c.grace() {
			continue
		}
		gone, err := c.Store.RemoveUnreferencedView(ctx, filepath.Join(c.ViewRoot, name), hex)
		if gone {
			anyGone = true
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return anyGone, ctxErr
			}
			if store.IsReferenceReadError(err) {
				return anyGone, err
			}
			klog.ErrorS(err, "reclaim metadata view", "view", name, "digest", view.ArtifactDigest)
			continue
		}
	}
	return anyGone, nil
}

// isViewID reports whether name is the shape the metadata package gives a
// final view directory: 64 lowercase hexadecimal characters, the record's
// content address. A live mount resolves its view by the same shape, so
// the collector's candidates are exactly the directories a lookup can
// still answer with.
func isViewID(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (c *Collector) done(res Result, high int32) Result {
	res.CapacityLow = res.Skipped == "" && res.Usage.Percent() > float64(high)

	return res
}

// removed counts bytes as removed when they are, so a collection that fails later still counts them.
func (*Collector) removed(res *Result, bytes int64) {
	res.RemovedBytes += bytes
	metrics.GCRemovedBytes.Add(float64(bytes))
}

type candidate struct {
	hex      string
	size     int64
	lastUsed time.Time
}

// candidates are the published trees no Pod references, not pinned, past their grace, oldest use
// first.
func (c *Collector) candidates(now time.Time, referenced map[string]bool) ([]candidate, error) {
	markers, err := c.Store.Published()
	if err != nil {
		return nil, err
	}
	downloading := c.Downloading()
	var pinned map[string]bool
	if c.Pinned != nil {
		pinned = c.Pinned()
	}
	var out []candidate
	for _, m := range markers {
		hex := store.HexOf(m.Digest)
		if hex == "" || referenced[hex] || downloading[hex] || pinned[hex] {
			continue
		}
		rec, err := c.Store.ReadDigest(hex)
		if err != nil {
			return nil, err
		}
		lastUsed := rec.LastUsedTime
		if lastUsed.IsZero() {
			lastUsed = m.PublishedTime
		}
		if now.Sub(lastUsed) < c.grace() {
			continue
		}
		out = append(out, candidate{hex: hex, size: m.SizeBytes, lastUsed: lastUsed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lastUsed.Before(out[j].lastUsed) })

	return out, nil
}

func (c *Collector) grace() time.Duration {
	if c.Grace > 0 {
		return c.Grace
	}
	return DefaultGrace
}

func (c *Collector) partialTTL() time.Duration {
	if c.PartialTTL > 0 {
		return c.PartialTTL
	}
	return DefaultPartialTTL
}
