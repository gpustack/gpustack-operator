package mockspec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	klog "k8s.io/klog/v2"
)

// maxSpecFileBytes bounds mock specification reads to 4 MiB.
const maxSpecFileBytes = 4 << 20

// Snapshot is an immutable point-in-time load of a mock hardware specification file.
type Snapshot struct {
	// Revision increments monotonically whenever the loaded file content changes and validates.
	Revision uint64
	// ContentSHA256 is the hex-encoded SHA-256 digest of the raw file bytes.
	ContentSHA256 string
	// ResolvedPath is the symlink-resolved target path at load time.
	ResolvedPath string
	// ModTime is the file modification timestamp observed at load time.
	ModTime time.Time
	// LoadedAt is when this Snapshot was parsed and validated.
	LoadedAt time.Time
	// Document is the validated specification document.
	Document *Document
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithLogger sets the logger used to report hot-reload failures.
func WithLogger(logger klog.Logger) StoreOption {
	return func(s *Store) {
		s.logger = logger
	}
}

// Store manages thread-safe hot-reloading of a mock hardware specification file.
// On reload failure it logs the error and retains the last valid Snapshot.
type Store struct {
	path   string
	logger klog.Logger

	reloadMu sync.Mutex
	current  atomic.Pointer[Snapshot]

	errMu   sync.RWMutex
	lastErr error
}

// NewStore creates a Store backed by filePath and performs the initial synchronous load.
// An unreadable or invalid file fails NewStore immediately.
func NewStore(filePath string, opts ...StoreOption) (*Store, error) {
	if filePath == "" {
		return nil, errors.New("mock devices file path is empty")
	}
	s := &Store{
		path:   filePath,
		logger: klog.Background().WithName("mockspec"),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if _, err := s.Refresh(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the configured specification file path.
func (s *Store) Path() string {
	return s.path
}

// Current returns the latest valid Snapshot without touching the filesystem.
func (s *Store) Current() *Snapshot {
	return s.current.Load()
}

// LastError returns the error from the most recent Refresh attempt, or nil if it succeeded.
func (s *Store) LastError() error {
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.lastErr
}

// Refresh checks the backing file (following symlinks such as Kubernetes ConfigMap ..data swaps)
// and reloads when content changes. When a reload fails after a valid initial load, Refresh logs
// the error and returns both the retained Snapshot and the error.
func (s *Store) Refresh() (*Snapshot, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	prev := s.current.Load()

	raw, resolvedPath, info, err := readBoundedSpecFile(s.path)
	if err != nil {
		return s.handleReloadError(prev, err)
	}

	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	if prev != nil && prev.ContentSHA256 == digest {
		s.setLastError(nil)
		return prev, nil
	}

	doc, err := Parse(raw)
	if err != nil {
		return s.handleReloadError(prev, err)
	}

	var nextRev uint64 = 1
	if prev != nil {
		nextRev = prev.Revision + 1
	}

	next := &Snapshot{
		Revision:      nextRev,
		ContentSHA256: digest,
		ResolvedPath:  resolvedPath,
		ModTime:       info.ModTime(),
		LoadedAt:      time.Now(),
		Document:      doc,
	}
	s.current.Store(next)
	s.setLastError(nil)
	return next, nil
}

// ResolveNode refreshes the Store and returns an isolated ResolvedNodeSpec for nodeName.
// If Refresh encounters a reload error after a valid initial load, ResolveNode returns the
// node specification from the retained Snapshot alongside the reload error.
func (s *Store) ResolveNode(nodeName string) (*ResolvedNodeSpec, error) {
	snap, err := s.Refresh()
	if snap == nil {
		return nil, err
	}
	return snap.Document.ResolveNode(nodeName), err
}

func (s *Store) handleReloadError(prev *Snapshot, err error) (*Snapshot, error) {
	s.setLastError(err)
	if prev != nil {
		s.logger.Error(err, "failed to reload mock devices spec, retaining last valid spec",
			"path", s.path, "revision", prev.Revision)
	}
	return prev, err
}

func (s *Store) setLastError(err error) {
	s.errMu.Lock()
	s.lastErr = err
	s.errMu.Unlock()
}

func resolveRegularFile(filePath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		return "", fmt.Errorf("resolve mock devices file %q: %w", filePath, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat mock devices file %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("mock devices path %q is not a regular file", resolved)
	}
	return resolved, nil
}

func readFileCapped(resolvedPath string, maxBytes int64) ([]byte, os.FileInfo, error) {
	f, err := os.Open(resolvedPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open mock devices file %q: %w", resolvedPath, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat open mock devices file %q: %w", resolvedPath, err)
	}
	if info.Size() > maxBytes {
		return nil, nil, fmt.Errorf("mock devices file %q exceeds %d bytes", resolvedPath, maxBytes)
	}

	buf, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read mock devices file %q: %w", resolvedPath, err)
	}
	if int64(len(buf)) > maxBytes {
		return nil, nil, fmt.Errorf("mock devices file %q exceeds %d bytes", resolvedPath, maxBytes)
	}
	return buf, info, nil
}
