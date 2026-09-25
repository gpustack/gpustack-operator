// Package store owns a node's model cache on disk: the layout, the attempts that fill it, the
// atomic publication of a verified tree, and the node-local ledger of references and failures.
//
// Layout under the cache root:
//
//	layout                          the layout's version; a root holding another is refused
//	published/<hex>/marker          the published tree's record, written and synced before the rename
//	published/<hex>/tree/...        the files, read-only; the directory a consumer is mounted from
//	partial/<hex>/<attempt>/        one attempt's marker and tree, renamed whole into published/
//	trash/                          published trees being removed, renamed out of published/ first
//	ledger/refs/<volume>.json       which target mounts which digest, and for which Pod
//	ledger/digests/<hex>.json       per digest: the last attempt number, last use and failure backoff
//
// A consumer is only ever mounted from published/, and a tree only enters published/ by one rename
// after every byte was verified and the marker was synced, so no consumer can see a partial tree.
// <hex> is the manifest digest's hexadecimal part: a content address the node computed, never a
// name a tenant chose.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Layout is the content of the layout file. A later layout changes it, so a root written by one
// layout is never read as another.
const Layout = "gpustack-model-store v1"

const (
	layoutFile   = "layout"
	markerFile   = "marker"
	treeDir      = "tree"
	publishedDir = "published"
	partialDir   = "partial"
	trashDir     = "trash"
	ledgerDir    = "ledger"
	refsDir      = "refs"
	digestsDir   = "digests"
)

// The modes of published content: readable by any user a restricted Pod runs as, writable by none.
const (
	publishedFileMode fs.FileMode = 0o444
	publishedDirMode  fs.FileMode = 0o555
	workingFileMode   fs.FileMode = 0o644
	workingDirMode    fs.FileMode = 0o755
)

var hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrLayout is returned for a root holding another layout.
var ErrLayout = errors.New("the cache root holds another layout")

// HexOf returns the hexadecimal part of a "sha256:<hex>" manifest digest, or "" when it is not one.
func HexOf(digest string) string {
	h, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !hexDigestPattern.MatchString(h) {
		return ""
	}

	return h
}

// Store is a node's cache root. It is the root's only writer.
type Store struct {
	root string
	// claimMu orders a mount's reference against a removal: Claim and RemoveUnreferenced hold it,
	// so a tree is never removed between the check that it is published and the record that a
	// target mounts it.
	claimMu sync.Mutex
	// digestMu serializes the updates of the digest records.
	digestMu sync.Mutex
}

// Open prepares root and returns the store over it. An empty or new root gets this layout; a root
// holding another layout is refused rather than read, and left as it was: the layout is read before
// anything is created.
func Open(root string) (*Store, error) {
	path := filepath.Join(root, layoutFile)
	b, err := os.ReadFile(path)
	fresh := errors.Is(err, fs.ErrNotExist)
	switch {
	case fresh:
	case err != nil:
		return nil, err
	case strings.TrimSpace(string(b)) != Layout:
		return nil, fmt.Errorf("%w: %q, want %q", ErrLayout, strings.TrimSpace(string(b)), Layout)
	}

	for _, d := range []string{
		publishedDir, partialDir, trashDir, filepath.Join(ledgerDir, refsDir),
		filepath.Join(ledgerDir, digestsDir),
	} {
		if err := os.MkdirAll(filepath.Join(root, d), workingDirMode); err != nil {
			return nil, err
		}
	}
	if fresh {
		if err := writeFileSync(path, []byte(Layout+"\n")); err != nil {
			return nil, err
		}
	}

	return &Store{root: root}, nil
}

// Root is the cache root.
func (s *Store) Root() string { return s.root }

// PublishedTree is the directory a consumer of hex is mounted from.
func (s *Store) PublishedTree(hex string) string {
	return filepath.Join(s.root, publishedDir, hex, treeDir)
}

// Marker is a published tree's record.
type Marker struct {
	// Digest is the manifest digest the tree was verified against.
	Digest string `json:"digest"`
	// Attempt is the attempt that published it.
	Attempt int64 `json:"attempt"`
	// ManifestFormat is the manifest format the digest is in, its header line.
	ManifestFormat string `json:"manifestFormat"`
	// SizeBytes and FileCount are the tree's.
	SizeBytes int64 `json:"sizeBytes"`
	FileCount int64 `json:"fileCount"`
	// PublishedTime is when it was published.
	PublishedTime time.Time `json:"publishedTime"`
}

// IsPublished reports whether hex has a published tree. It reads only the marker, so the check a
// mount makes is one stat whatever the tree's size.
func (s *Store) IsPublished(hex string) bool {
	_, err := os.Stat(filepath.Join(s.root, publishedDir, hex, markerFile))
	return err == nil
}

// ReadMarker reads hex's marker.
func (s *Store) ReadMarker(hex string) (Marker, error) {
	var m Marker
	b, err := os.ReadFile(filepath.Join(s.root, publishedDir, hex, markerFile))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)

	return m, err
}

// Published lists the markers of every published tree. A directory without a marker is not a
// published tree and is not listed.
func (s *Store) Published() ([]Marker, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, publishedDir))
	if err != nil {
		return nil, err
	}
	markers := make([]Marker, 0, len(entries))
	for _, e := range entries {
		if !hexDigestPattern.MatchString(e.Name()) {
			continue
		}
		m, err := s.ReadMarker(e.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("published tree %s: %w", e.Name(), err)
		}
		markers = append(markers, m)
	}

	return markers, nil
}

// RemovePublished removes hex's published tree. It is renamed out of published/ first, so the tree
// disappears in one step and a crash midway leaves only trash, which the next start removes.
func (s *Store) RemovePublished(hex string) error {
	trash, err := s.trashPublished(hex)
	if err != nil || trash == "" {
		return err
	}

	return removeAllWritable(trash)
}

// trashPublished renames hex's published tree into trash/ and returns where it went, or "" when hex
// has no published tree.
func (s *Store) trashPublished(hex string) (string, error) {
	src := filepath.Join(s.root, publishedDir, hex)
	dst := filepath.Join(s.root, trashDir, hex+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}

	return dst, syncDir(filepath.Join(s.root, publishedDir))
}

// RemoveUnreferenced removes hex's published tree unless a reference names it, reporting whether it
// did. It reads the references under the same lock Claim writes them under, so a mount that claimed
// the tree an instant earlier keeps it; a reference that cannot be read keeps it too. Only the rename
// out of published/ holds that lock: a mount then no longer finds the tree, so the files are removed
// after it is released, rather than stalling every mount on the node while a large tree is deleted.
func (s *Store) RemoveUnreferenced(hex string) (bool, error) {
	trash, err := s.trashUnreferenced(hex)
	if err != nil || trash == "" {
		return false, err
	}
	if err := removeAllWritable(trash); err != nil {
		return false, err
	}

	return true, nil
}

func (s *Store) trashUnreferenced(hex string) (string, error) {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	refs, err := s.Refs()
	if err != nil {
		return "", err
	}
	for _, r := range refs {
		if r.Hex == hex {
			return "", nil
		}
	}
	if !s.IsPublished(hex) {
		return "", nil
	}

	return s.trashPublished(hex)
}

// EmptyTrash removes what an interrupted removal left.
func (s *Store) EmptyTrash() error {
	entries, err := os.ReadDir(filepath.Join(s.root, trashDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeAllWritable(filepath.Join(s.root, trashDir, e.Name())); err != nil {
			return err
		}
	}

	return nil
}

// Partial is one attempt's directory left on disk.
type Partial struct {
	Hex     string
	Attempt int64
	// SizeBytes is what its files occupy.
	SizeBytes int64
	// ModTime is its latest file modification.
	ModTime time.Time
}

// Partials lists every attempt directory on disk.
func (s *Store) Partials() ([]Partial, error) {
	digests, err := os.ReadDir(filepath.Join(s.root, partialDir))
	if err != nil {
		return nil, err
	}
	var partials []Partial
	for _, d := range digests {
		// An attempt that publishes or starts renames its directory away while this walks, so
		// anything that vanished mid-walk is skipped rather than failing every reader of the list.
		attempts, err := os.ReadDir(filepath.Join(s.root, partialDir, d.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, a := range attempts {
			n, err := strconv.ParseInt(a.Name(), 10, 64)
			if err != nil || !hexDigestPattern.MatchString(d.Name()) {
				continue
			}
			p := Partial{Hex: d.Name(), Attempt: n}
			walkErr := filepath.WalkDir(filepath.Join(s.root, partialDir, d.Name(), a.Name()),
				func(_ string, e fs.DirEntry, err error) error {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					if err != nil {
						return err
					}
					info, err := e.Info()
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					if err != nil {
						return err
					}
					if !e.IsDir() {
						p.SizeBytes += info.Size()
					}
					if info.ModTime().After(p.ModTime) {
						p.ModTime = info.ModTime()
					}
					return nil
				})
			if walkErr != nil {
				return nil, walkErr
			}
			partials = append(partials, p)
		}
	}

	return partials, nil
}

// RemovePartial removes one attempt's directory, and the digest's directory once it is empty.
func (s *Store) RemovePartial(hex string, attempt int64) error {
	if err := os.RemoveAll(s.attemptDir(hex, attempt)); err != nil {
		return err
	}
	// Removing a non-empty directory fails, which is the point: another attempt is in it.
	_ = os.Remove(filepath.Join(s.root, partialDir, hex))

	return nil
}

func (s *Store) attemptDir(hex string, attempt int64) string {
	return filepath.Join(s.root, partialDir, hex, strconv.FormatInt(attempt, 10))
}

// Attempt is one attempt at materializing a digest: a directory of its own, fenced by a number no
// other attempt on this node has, so a late write of an attempt that was superseded can never land
// in the tree another attempt publishes.
type Attempt struct {
	store  *Store
	Hex    string
	Number int64
}

// NewAttempt starts the next attempt at hex. When an earlier attempt's files are on disk, the most
// recent one is carried into the new attempt's directory, so a download resumes rather than
// restarts; it is renamed, never copied, and the caller has made sure no earlier attempt is running.
func (s *Store) NewAttempt(hex string) (*Attempt, error) {
	if !hexDigestPattern.MatchString(hex) {
		return nil, fmt.Errorf("digest %q is not 64 hexadecimal digits", hex)
	}
	previous, err := s.latestPartial(hex)
	if err != nil {
		return nil, err
	}
	var number int64
	if err := s.UpdateDigest(hex, func(rec *DigestRecord) {
		number = max(rec.LastAttempt, previous) + 1
		rec.LastAttempt = number
	}); err != nil {
		return nil, err
	}

	a := &Attempt{store: s, Hex: hex, Number: number}
	dir := s.attemptDir(hex, number)
	if previous > 0 {
		if err := os.Rename(s.attemptDir(hex, previous), dir); err != nil {
			return nil, err
		}
		// A marker an interrupted publication left is dropped: the bytes are verified again first.
		if err := os.Remove(filepath.Join(dir, markerFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// And the files it sealed are made writable again, so the download can reopen them.
		if err := unseal(filepath.Join(dir, treeDir)); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, treeDir), workingDirMode); err != nil {
		return nil, err
	}

	return a, nil
}

func (s *Store) latestPartial(hex string) (int64, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, partialDir, hex))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && n > latest {
			latest = n
		}
	}

	return latest, nil
}

// Dir is the attempt's directory.
func (a *Attempt) Dir() string { return a.store.attemptDir(a.Hex, a.Number) }

// SizeBytes is what the attempt's files occupy so far: the bytes a resumed attempt carried over.
func (a *Attempt) SizeBytes() int64 {
	var size int64
	// Best effort: a file that cannot be read is counted as missing, which only reserves more.
	_ = filepath.WalkDir(filepath.Join(a.Dir(), treeDir), func(_ string, e fs.DirEntry, walkErr error) error {
		if walkErr == nil && !e.IsDir() {
			if info, ierr := e.Info(); ierr == nil {
				size += info.Size()
			}
		}
		return nil
	})

	return size
}

// FilePath is where a manifest path's file is written in this attempt. Manifest paths are relative
// and hold no empty, "." or ".." segment by the manifest format's own rules; this checks it again,
// so a path that would escape the attempt's tree is refused rather than joined.
func (a *Attempt) FilePath(manifestPath string) (string, error) {
	tree := filepath.Join(a.Dir(), treeDir)
	p := filepath.Join(tree, filepath.FromSlash(manifestPath))
	rel, err := filepath.Rel(tree, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		filepath.ToSlash(rel) != manifestPath {
		return "", fmt.Errorf("manifest path %q escapes the tree", manifestPath)
	}

	return p, nil
}

// Publish seals the attempt and publishes it as hex's tree: every file made read-only and synced,
// the marker written and synced, the directories synced, one rename into published/, and the parent
// synced. Only verified content may reach it; it re-reads nothing.
func (a *Attempt) Publish(m Marker) error {
	dir := a.Dir()
	tree := filepath.Join(dir, treeDir)
	var dirs []string
	err := filepath.WalkDir(tree, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			dirs = append(dirs, path)
			return nil
		case !e.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file", path)
		}
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		// Sealed before the sync, so the read-only mode is as durable as the bytes.
		if err := f.Chmod(publishedFileMode); err != nil {
			return err
		}
		return f.Sync()
	})
	if err != nil {
		return err
	}
	// Deepest first, so each directory is sealed after everything in it.
	for _, d := range slices.Backward(dirs) {
		if err := os.Chmod(d, publishedDirMode); err != nil {
			return err
		}
		if err := syncDir(d); err != nil {
			return err
		}
	}

	m.Attempt = a.Number
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dir, markerFile), b); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}

	dst := filepath.Join(a.store.root, publishedDir, a.Hex)
	if err := os.Rename(dir, dst); err != nil {
		return fmt.Errorf("publish %s: %w", a.Hex, err)
	}
	if err := syncDir(filepath.Join(a.store.root, publishedDir)); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(a.store.root, partialDir, a.Hex))

	return nil
}

func writeFileSync(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, workingFileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	return d.Sync()
}

// unseal gives a tree Publish sealed the working modes back. The plugin runs as root, which may write
// sealed files anyway; unsealing keeps a resumed attempt correct for a caller that is not.
func unseal(path string) error {
	err := filepath.WalkDir(path, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := workingFileMode
		if e.IsDir() {
			mode = workingDirMode
		}
		return os.Chmod(p, mode)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// removeAllWritable removes a tree whose directories were sealed read-only. The plugin runs as root,
// which may remove them anyway; making them writable first keeps the removal correct for a caller
// that is not, the tests among them.
func removeAllWritable(path string) error {
	_ = filepath.WalkDir(path, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() {
			_ = os.Chmod(p, workingDirMode)
		}
		return nil
	})

	return os.RemoveAll(path)
}
