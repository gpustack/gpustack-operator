// Package artifact bridges a node's verified cache publication into the portable
// model-transfer manifest.
//
// The materializer publishes a tree beside its marker with a peer listing
// (peer.Listing) whose canonical re-encoding is the artifact's content
// digest. Build validates that identity and inspects bounded file headers and
// extents to produce a layout-only protocol.Manifest. Cache publication owns
// payload verification. Publishing the portable manifest stays the caller's step.
//
// Build's source is always the full verified publication: the marker, the
// listing beside it, and the sealed tree. A metadata view (an incomplete
// header-only derivation) is never accepted as the source, and nothing Build
// returns marks a tree more or less published than it was.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"

	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// Bounds of one transfer manifest. A listing is kilobytes per hundred files,
// so megabytes of it are corruption; the entry, tensor, and byte bounds sit
// far above any real checkpoint while keeping every accumulation inside the
// int64 range.
const (
	// MaxListingBytes caps the peer listing's raw JSON.
	MaxListingBytes int64 = 16 << 20
	// MaxListingEntries caps the files one listing may carry.
	MaxListingEntries = 65536
	// MaxTensors caps the tensors one transfer manifest may carry.
	MaxTensors = protocol.MaxManifestTensors
	// MaxRegisteredBytes caps the total size of the safetensors files a
	// transfer manifest registers.
	MaxRegisteredBytes int64 = 16 << 40
)

// The refusals, each naming its own rule. Their text may name a digest and a
// tree-relative file path, and those paths are tenant content: a caller that
// carries an error into shared cluster status sanitizes it or keeps it
// private.
var (
	// ErrNotPublished is a digest with no verified published tree.
	ErrNotPublished = errors.New("no verified published tree for the digest")
	// ErrStaleDigest is a marker or listing verified against another digest.
	ErrStaleDigest = errors.New("the published tree verifies against another digest")
	// ErrListingUnavailable is a tree published before listings were stored.
	ErrListingUnavailable = errors.New("the published tree carries no transfer listing")
	// ErrListingInvalid is a listing that is malformed or outside the bounds.
	ErrListingInvalid = errors.New("the published transfer listing is invalid")
	// ErrContentMismatch is a listing whose canonical re-encoding is another
	// digest than the one asked about.
	ErrContentMismatch = errors.New("the published listing does not re-encode to the expected content digest")
	// ErrFileMismatch is a published file whose bytes do not match its listing.
	ErrFileMismatch = errors.New("a published file does not match its listing")
	// ErrInvalidSafetensors is a safetensors file that is empty, truncated, or
	// not the format its name promises.
	ErrInvalidSafetensors = errors.New("a published safetensors file is invalid")
	// ErrInvalidBundle is a tree whose verified bytes still do not describe a
	// valid transfer manifest: a duplicate tensor name, or a descriptor the
	// reader contract refuses.
	ErrInvalidBundle = errors.New("the published bundle does not describe a valid transfer manifest")
	// ErrNoTensors is a bundle that holds no safetensors tensor at all.
	ErrNoTensors = errors.New("the published tree holds no safetensors tensor")
	// ErrBoundsExceeded is a tree whose figures exceed the transfer bounds.
	ErrBoundsExceeded = errors.New("the published tree exceeds the transfer manifest bounds")
)

// uidPattern is the artifact UID shape the reader contract admits. It is kept
// in lockstep with protocol's own pattern, which the built manifest's Check
// enforces again; matching it here only fails fast, before any IO.
var uidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// View is a published tree's portable transfer manifest and the figures of the
// verification that produced it. It is an in-memory derivation: nothing is
// written to the store, and handing the manifest to its readers is the
// caller's atomic publication.
type View struct {
	// Manifest is the portable manifest, its Check passed and its
	// ManifestSHA256 set to the digest of Raw's exact bytes. It is never the
	// content digest: the content digest travels in ContentDigest.
	Manifest *protocol.Manifest
	// Raw is the manifest's exact JSON encoding, deterministic for one tree
	// and one identity. Raw is what ManifestSHA256 digests.
	Raw []byte
	// SHA256 is the lowercase hex sha256 of Raw.
	SHA256 string
	// ContentDigest is the canonical content address the tree verifies
	// against, "sha256:<hex>", retained separately from the manifest's own
	// digest.
	ContentDigest string
	// TreeDir is the published tree the manifest was verified against. It is
	// a node-local cache path: it is for the caller's own use and must not
	// reach shared status.
	TreeDir string
	// RegisteredBytes is the conservative registration figure: the complete
	// byte size of every nonempty safetensors file the manifest registers,
	// header and alignment gaps included, not the tensor payload total.
	RegisteredBytes int64
	// Registrations is how many nonempty safetensors files the manifest
	// registers.
	Registrations int
}

// Build derives the portable transfer manifest of one published tree.
//
// contentDigest is the tree's canonical content address, "sha256:<hex>", the
// digest the artifact resolves to. artifactUID and sourceGeneration are the
// live artifact's identity: the UID of the ModelArtifact the caller serves
// and that artifact's spec generation. The generation is the spec's, not the
// broker's opaque registration token, which is assigned per fill and means
// something else. Both are validated before any IO, and both are bound into
// the portable manifest identity.
//
// The caller must hold a cache reference for the digest (store.Claim, or its
// own pin of the tree) across the call: Build reads the tree as it stands and
// pins nothing, so a tree removed mid-Build is a refusal, not a wrong
// manifest.
//
// On success file extents and tensor layouts are valid, and the canonical
// listing re-encodes to contentDigest. Build does not read payload bytes.
// The store and its published state are left untouched.
func Build(
	ctx context.Context, st *store.Store, artifactUID string, sourceGeneration int64,
	contentDigest string,
) (*View, error) {
	return buildManifest(ctx, st, artifactUID, sourceGeneration, contentDigest, "")
}

// BuildPaged derives bounded tensor pages in a caller-owned private directory.
// The caller keeps that directory alive until readers have copied the pages.
func BuildPaged(
	ctx context.Context, st *store.Store, artifactUID string, sourceGeneration int64,
	contentDigest, pageDirectory string,
) (*View, error) {
	if pageDirectory == "" {
		return nil, fmt.Errorf("a private page directory is required")
	}
	return buildManifest(ctx, st, artifactUID, sourceGeneration, contentDigest, pageDirectory)
}

func buildManifest(
	ctx context.Context, st *store.Store, artifactUID string, sourceGeneration int64,
	contentDigest, pageDirectory string,
) (*View, error) {
	if !uidPattern.MatchString(artifactUID) {
		return nil, fmt.Errorf("artifact uid %q is not the shape the transfer contract admits", artifactUID)
	}
	if sourceGeneration < 1 {
		return nil, fmt.Errorf("source generation %d is not positive", sourceGeneration)
	}
	hex := store.HexOf(contentDigest)
	if hex == "" {
		return nil, fmt.Errorf("content digest %q is not a sha256 content address", contentDigest)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	marker, err := st.ReadMarker(hex)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNotPublished, contentDigest, err)
	}
	if marker.Digest != contentDigest {
		return nil, fmt.Errorf("%w: the marker names %s", ErrStaleDigest, marker.Digest)
	}

	listing, err := readListing(st.PublishedManifestPath(hex))
	if err != nil {
		return nil, err
	}
	if listing.Digest != contentDigest {
		return nil, fmt.Errorf("%w: the listing names %s", ErrStaleDigest, listing.Digest)
	}

	entries := make([]modelartifact.ManifestEntry, len(listing.Entries))
	for i, e := range listing.Entries {
		entries[i] = modelartifact.ManifestEntry{Path: e.Path, Size: e.Size, Digest: e.Digest}
	}
	// The canonical encoding sums the sizes unchecked, so the total is
	// checked here, over every listed file, before the encoding is trusted
	// or any byte is read: a listing whose total wraps must be refused, not
	// re-encoded into a plausible digest.
	var listedBytes int64
	for _, e := range entries {
		listedBytes, err = addChecked(listedBytes, e.Size)
		if err != nil {
			return nil, fmt.Errorf("%w: the listed file sizes overflow the total", ErrBoundsExceeded)
		}
	}
	canonical, err := modelartifact.NewManifest(entries)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrListingInvalid, err)
	}
	if canonical.Digest != contentDigest {
		return nil, fmt.Errorf("%w: the listing re-encodes to %s", ErrContentMismatch, canonical.Digest)
	}

	var view *View
	if pageDirectory != "" {
		view, err = verifyPagedTree(ctx, st, artifactUID, sourceGeneration, contentDigest, canonical, pageDirectory)
	} else {
		view, err = verifyTree(ctx, st, artifactUID, sourceGeneration, contentDigest, canonical)
	}
	if err != nil {
		return nil, err
	}
	return view, nil
}

// encodeManifest serializes the manifest deterministically and returns the
// bytes with their sha256, which is also the manifest's own digest: the one
// identity a manifest carries is the identity of its exact bytes.
func encodeManifest(m *protocol.Manifest) (raw []byte, sha256hex string, err error) {
	if err := m.Check(); err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}
	raw, err = json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	limit := protocol.MaxManifestBytes
	if len(m.TensorPages) > 0 {
		limit = protocol.MaxPagedManifestBytes
	}
	if len(raw) > limit {
		return nil, "", fmt.Errorf("%w: the encoded manifest exceeds %d bytes", ErrBoundsExceeded, limit)
	}

	return raw, sha256Hex(raw), nil
}

// addChecked adds b to a, refusing an int64 overflow.
func addChecked(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: the byte total overflows", ErrBoundsExceeded)
	}

	return a + b, nil
}
