package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	modelartifact "gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
)

// Defaults of a Fetcher and a Puller.
const (
	// DefaultSegmentBytes is the byte range one request fetches of a large file.
	DefaultSegmentBytes = 64 << 20
	// DefaultRetries is how many chances one source gets across a file before it is set aside
	// for the next candidate.
	DefaultRetries = 2
	// peerCooldown is how long a source that just failed sits out, across files and attempts:
	// one dead peer must not collect a failure from every parallel file.
	peerCooldown = time.Minute
	// readChunk is the unit a response body is read in.
	readChunk = 32 << 10
)

// Puller discovers the sources of a digest and fetches files from them, one source at a time.
// A source that fails sits out for a while and the next candidate takes over; the hub is the
// caller's fallback, never this package's business.
type Puller struct {
	// Discover returns the nodes that hold the digest published and ready, as ordered
	// candidates. It carries the source-set extension point: multi-source scheduling would
	// widen what comes back, not change this interface.
	Discover func(ctx context.Context, digest string) ([]*Source, error)
	// SegmentBytes is one request's byte range; zero is the default.
	SegmentBytes int64
	// StreamsPerSource is how many requests one source serves to this node at once; zero is
	// the default.
	StreamsPerSource int
	// Received is told each byte count with the node that delivered it, concurrently from
	// every running request; nil counts nothing. Peer bytes are counted here, apart from the
	// hub downloader's own counter.
	Received func(node string, n int64)

	mu      sync.Mutex
	cooling map[string]time.Time
}

// Enabled reports whether peer pulling is configured.
func (p *Puller) Enabled() bool { return p != nil && p.Discover != nil }

// FetchFile fills file from a peer that holds the digest, verifying every byte against the
// manifest the reassembled listing is bound to. The attempt starts from what an earlier one
// checkpointed, whoever delivered it, and what arrives is checkpointed on the way, so a peer
// dying mid-file costs the bytes after the last checkpoint rather than the file. Every failure
// rotates to the next candidate; the caller falls back to the hub when all of them are spent.
func (p *Puller) FetchFile(ctx context.Context, digest string, file download.File) error {
	treeHex, ok := HexOf(digest)
	if !ok {
		return fmt.Errorf("digest %q is not a manifest digest", digest)
	}
	sources, err := p.Discover(ctx, digest)
	if err != nil {
		return err
	}
	var lastErr error
	for _, src := range sources {
		if p.sitsOut(src) {
			continue
		}
		err = p.fetchFrom(ctx, src, treeHex, file)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		p.setAside(src)
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no candidate was usable")
	}

	return fmt.Errorf("%s: %w", file.Path, lastErr)
}

// fetchFrom pulls the whole file from one source: its listing is reassembled into a canonical
// manifest and bound to the tree's digest before a byte is trusted, then the file's ranges are
// fetched and hashed in byte order, checkpointing as they go.
func (p *Puller) fetchFrom(ctx context.Context, src *Source, treeHex string, file download.File) error {
	manifest, err := p.fetchListing(ctx, src, treeHex)
	if err != nil {
		return err
	}
	var entry modelartifact.ManifestEntry
	found := false
	for _, e := range manifest.Entries {
		if e.Path == file.Path {
			entry, found = e, true

			break
		}
	}
	if !found {
		return fmt.Errorf("the manifest does not name %s", file.Path)
	}
	if entry.Size != file.Size || entry.Digest != file.Digest {
		// The listing was bound to the right root, so its line is the truth; the caller's file
		// was built from the same canonical manifest and disagrees only if something is wrong.
		return fmt.Errorf("the manifest says %d bytes %s, the attempt says %d %s",
			entry.Size, entry.Digest, file.Size, file.Digest)
	}

	hasher, offset, err := download.Resume(file.Digest, file.Size, file.Dest, file.Checkpoint, checkpointInterval)
	if err != nil {
		return err
	}
	if offset < file.Size {
		if err := p.fetchRanges(ctx, src, treeHex, file, hasher, offset); err != nil {
			return err
		}
	}
	if err := hasher.Verify(file.Dest); err != nil {
		return err
	}

	return nil
}

// fetchRanges fetches [offset, size) of the file in byte-range segments and feeds them to the
// hasher in order. A segment that fails after its retries fails the source.
func (p *Puller) fetchRanges(
	ctx context.Context, src *Source, treeHex string, file download.File,
	hasher *download.StreamHasher, offset int64,
) error {
	out, err := os.OpenFile(file.Dest, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	segment := p.segmentBytes()
	for start := offset; start < file.Size; start += segment {
		end := min(start+segment, file.Size)
		if err := p.fetchOne(ctx, src, treeHex, file, out, start, end, hasher); err != nil {
			return err
		}
	}

	return nil
}

// fetchOne sends one range request and writes what arrives at its offsets, hashing it in order.
func (p *Puller) fetchOne(
	ctx context.Context, src *Source, treeHex string, file download.File, out *os.File, start, end int64,
	hasher *download.StreamHasher,
) error {
	release, err := src.take(ctx)
	if err != nil {
		return err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL(src.BaseURL, treeHex, file.Path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gpustack-operator")
	req.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end-1, 10))
	resp, err := src.Client.Do(req)
	if err != nil {
		return fmt.Errorf("the peer did not answer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		if got := contentRangeStart(resp.Header.Get("Content-Range")); got != start {
			return fmt.Errorf("the peer answered another range %q", resp.Header.Get("Content-Range"))
		}
	case resp.StatusCode == http.StatusOK && start == 0:
		if resp.ContentLength >= 0 && resp.ContentLength != end {
			return fmt.Errorf("the peer serves %d bytes, the manifest says %d", resp.ContentLength, end)
		}
	case resp.StatusCode == http.StatusOK:
		return errors.New("the peer does not honor byte ranges")
	default:
		return fmt.Errorf("the peer answered HTTP %d", resp.StatusCode)
	}

	buf := make([]byte, min(readChunk, end-start))
	pos := start
	for pos < end {
		want := int64(len(buf))
		if left := end - pos; left < want {
			want = left
		}
		n, readErr := io.ReadFull(resp.Body, buf[:want])
		if n > 0 {
			if _, err := out.WriteAt(buf[:n], pos); err != nil {
				return err
			}
			if err := hasher.Hash(buf[:n]); err != nil {
				return err
			}
			pos += int64(n)
			if file.Received != nil {
				file.Received(int64(n))
			}
			if p.Received != nil {
				p.Received(src.NodeName, int64(n))
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				if pos < end {
					return errors.New("the peer closed the range early")
				}

				break
			}

			return readErr
		}
	}

	return nil
}

// fetchListing asks one source for its tree's manifest and binds it to the tree's digest: the
// entries are reassembled into the canonical manifest, whose recomputed root digest must be the
// one the artifact resolved to. A listing that does not rebind — truncated, reordered with
// altered content, or self-consistently forged — is refused here, before any byte is pulled.
func (p *Puller) fetchListing(ctx context.Context, src *Source, treeHex string) (modelartifact.Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listingURL(src.BaseURL, treeHex), nil)
	if err != nil {
		return modelartifact.Manifest{}, err
	}
	req.Header.Set("User-Agent", "gpustack-operator")
	resp, err := src.Client.Do(req)
	if err != nil {
		return modelartifact.Manifest{}, fmt.Errorf("the peer did not answer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return modelartifact.Manifest{}, fmt.Errorf("the peer answered HTTP %d", resp.StatusCode)
	}
	var l listing
	if err := json.NewDecoder(io.LimitReader(resp.Body, manifestLimit)).Decode(&l); err != nil {
		return modelartifact.Manifest{}, fmt.Errorf("the peer's listing does not parse: %w", err)
	}
	entries := make([]modelartifact.ManifestEntry, 0, len(l.Entries))
	for _, e := range l.Entries {
		entries = append(entries, modelartifact.ManifestEntry{Path: e.Path, Size: e.Size, Digest: e.Digest})
	}
	manifest, err := modelartifact.NewManifest(entries)
	if err != nil {
		return modelartifact.Manifest{}, fmt.Errorf("the peer's listing is not a manifest: %w", err)
	}
	if manifest.Digest != "sha256:"+treeHex {
		return modelartifact.Manifest{}, fmt.Errorf("the peer's listing hashes to %s, the content is %s: refused",
			manifest.Digest, "sha256:"+treeHex)
	}

	return manifest, nil
}

// manifestLimit bounds one listing's answer: tens of thousands of files at most.
const manifestLimit = 64 << 20

// setAside makes a source sit out for a while.
func (p *Puller) setAside(src *Source) {
	p.mu.Lock()
	if p.cooling == nil {
		p.cooling = map[string]time.Time{}
	}
	p.cooling[src.BaseURL] = time.Now().Add(peerCooldown)
	p.mu.Unlock()
}

// sitsOut says whether a source is serving its cooldown.
func (p *Puller) sitsOut(src *Source) bool {
	p.mu.Lock()
	until, ok := p.cooling[src.BaseURL]
	p.mu.Unlock()

	return ok && time.Now().Before(until)
}

func (p *Puller) segmentBytes() int64 {
	if p.SegmentBytes > 0 {
		return p.SegmentBytes
	}

	return DefaultSegmentBytes
}

// checkpointInterval is how often a fetch checkpoints its hash state: every 64 MiB hashed.
const checkpointInterval = 64 << 20

// fileURL is one file's peer address.
func fileURL(base, treeHex, name string) string {
	u, err := parseBase(base)
	if err != nil {
		return base
	}

	return u.JoinPath("peer/v1/trees", treeHex, "files", filepath.ToSlash(name)).String()
}

// listingURL is one tree's listing address.
func listingURL(base, treeHex string) string {
	u, err := parseBase(base)
	if err != nil {
		return base
	}

	return u.JoinPath("peer/v1/trees", treeHex).String()
}

func parseBase(base string) (*url.URL, error) {
	return url.Parse(base)
}

// contentRangeStart reads the start of "bytes <start>-<end>/<total>".
func contentRangeStart(header string) int64 {
	spec, found := strings.CutPrefix(header, "bytes ")
	if !found {
		return -1
	}
	span, _, found := strings.Cut(spec, "/")
	first, _, found2 := strings.Cut(span, "-")
	if !found || !found2 {
		return -1
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return -1
	}

	return start
}
