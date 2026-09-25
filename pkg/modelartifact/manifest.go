// Package modelartifact holds what a ModelArtifact's identity is made of: the canonical manifest
// format, and the client that resolves a Hugging Face source into one.
//
// It imports nothing from the controllers, so node-side code that verifies downloads against a
// manifest digest can use the same definition the controller published.
package modelartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ManifestHeader is the first line of every canonical manifest. Any change to the format changes
// the version in it, so two formats can never produce the same digest.
const ManifestHeader = "gpustack-manifest v1"

// The per-file digest algorithms the format admits, as they are spelled before the colon.
const (
	// DigestSHA256 is the SHA-256 of the file's content: an LFS file's oid on Hugging Face.
	DigestSHA256 = "sha256"
	// DigestGitSHA1 is the git blob id of the file: a non-LFS file's oid on Hugging Face.
	DigestGitSHA1 = "gitsha1"
)

// manifestDigestHexLength is the closed set of per-file algorithms and the length of their
// lowercase hexadecimal form. An algorithm outside it is refused rather than passed through,
// because a digest nobody can recompute on a node verifies nothing.
var manifestDigestHexLength = map[string]int{
	DigestSHA256:  64,
	DigestGitSHA1: 40,
}

// ErrInvalidManifest is wrapped by every refusal of an entry, so a caller can tell a manifest the
// source served wrongly from a failure to reach the source.
var ErrInvalidManifest = errors.New("invalid manifest")

// ManifestEntry is one file of a manifest.
type ManifestEntry struct {
	// Path is the slash-separated path relative to the repository root, in the bytes the source
	// returned. It is never normalized: the files on disk must carry the names the source serves.
	Path string
	// Size is the file size in bytes.
	Size int64
	// Digest is "<algorithm>:<lowercase hexadecimal>", the algorithm one of DigestSHA256 and
	// DigestGitSHA1.
	Digest string
}

// Manifest is a canonical manifest and the figures a ModelArtifact publishes from it.
type Manifest struct {
	// Encoded is the canonical encoding: the header line, then one "<digest> <size> <path>" line
	// per file in ascending byte order of the path, every line terminated by LF.
	Encoded []byte
	// Digest is "sha256:" followed by the SHA-256 of Encoded, which is the manifest digest.
	Digest string
	// FileCount is the number of files.
	FileCount int64
	// SizeBytes is the sum of the files' sizes.
	SizeBytes int64
	// Entries are the files, in the encoding's order: what a node downloads and verifies.
	Entries []ManifestEntry
}

// NewManifest encodes entries into the canonical manifest and computes its digest.
//
// The input order never matters. An invalid entry, or two entries with one path, refuses the whole
// manifest rather than dropping the entry: a manifest silently missing a file would publish a
// digest for content that is not the commit's.
func NewManifest(entries []ManifestEntry) (Manifest, error) {
	sorted := slices.Clone(entries)
	// Go compares strings by their bytes, which for valid UTF-8 is also code-point order.
	slices.SortFunc(sorted, func(a, b ManifestEntry) int { return strings.Compare(a.Path, b.Path) })

	var (
		b    strings.Builder
		size int64
	)
	b.WriteString(ManifestHeader)
	b.WriteByte('\n')
	for i, e := range sorted {
		if err := validateManifestEntry(e); err != nil {
			return Manifest{}, err
		}
		if i > 0 && sorted[i-1].Path == e.Path {
			return Manifest{}, fmt.Errorf("%w: duplicate path %q", ErrInvalidManifest, e.Path)
		}
		b.WriteString(e.Digest)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(e.Size, 10))
		b.WriteByte(' ')
		b.WriteString(e.Path)
		b.WriteByte('\n')
		size += e.Size
	}

	encoded := []byte(b.String())
	sum := sha256.Sum256(encoded)

	return Manifest{
		Encoded:   encoded,
		Digest:    DigestSHA256 + ":" + hex.EncodeToString(sum[:]),
		FileCount: int64(len(sorted)),
		SizeBytes: size,
		Entries:   sorted,
	}, nil
}

func validateManifestEntry(e ManifestEntry) error {
	if err := validateManifestPath(e.Path); err != nil {
		return err
	}
	if e.Size < 0 {
		return fmt.Errorf("%w: path %q has a negative size %d", ErrInvalidManifest, e.Path, e.Size)
	}
	algorithm, value, ok := strings.Cut(e.Digest, ":")
	n, known := manifestDigestHexLength[algorithm]
	if !ok || !known || len(value) != n || !isLowerHex(value) {
		return fmt.Errorf("%w: path %q has a malformed digest %q", ErrInvalidManifest, e.Path, e.Digest)
	}

	return nil
}

// validateManifestPath accepts a relative slash-separated path of valid UTF-8 with no control
// character and no empty, "." or ".." segment. Those are the paths that cannot escape the
// directory a node publishes them under.
func validateManifestPath(p string) error {
	if p == "" || !utf8.ValidString(p) {
		return fmt.Errorf("%w: path %q is empty or not valid UTF-8", ErrInvalidManifest, p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: path %q contains a control character", ErrInvalidManifest, p)
		}
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: path %q has an empty, \".\" or \"..\" segment", ErrInvalidManifest, p)
		}
	}

	return nil
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}
