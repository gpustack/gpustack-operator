package hostfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"
	"syscall"
)

const (
	// DefaultHostRoot is the default container mount path where the node root
	// filesystem is mounted read-only.
	DefaultHostRoot = "/host"

	// maxSymlinkHops caps total symbolic link expansions per path lookup.
	maxSymlinkHops = 40
)

// hostRootMarkers are top-level directories required in every valid Linux host root.
var hostRootMarkers = [...]string{"etc", "proc", "usr"}

// ReadOnlyHostFS provides strictly read-only access to the node root filesystem
// mounted at a host-root prefix (default "/host"). Callers pass native host paths
// such as "/etc/hccl_rootinfo.json", "/lib/route.conf", or "/" without prepending
// the mount prefix.
type ReadOnlyHostFS struct {
	root *os.Root
}

// OpenHostRoot opens rootPath via os.OpenRoot and verifies that etc, proc, and usr
// exist inside the root as directories. If validation fails, the opened root is
// closed and an error is returned.
func OpenHostRoot(rootPath string) (*ReadOnlyHostFS, error) {
	if rootPath == "" {
		return nil, fmt.Errorf("%w: host root path must not be empty", fs.ErrInvalid)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("hostfs: open root %q: %w", rootPath, err)
	}

	h := &ReadOnlyHostFS{root: root}
	for _, marker := range hostRootMarkers {
		info, err := h.Stat("/" + marker)
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("hostfs: host root %q missing required directory %q: %w", rootPath, marker, err)
		}
		if !info.IsDir() {
			_ = root.Close()
			return nil, fmt.Errorf("hostfs: host root %q marker %q is not a directory", rootPath, marker)
		}
	}

	return h, nil
}

// Close releases the underlying root directory descriptor. It is safe to call on a
// nil or already-closed ReadOnlyHostFS.
func (h *ReadOnlyHostFS) Close() error {
	if h == nil || h.root == nil {
		return nil
	}
	return h.root.Close()
}

// Open opens hostPath strictly read-only inside the host root, resolving both
// intermediate and leaf symbolic links within the root boundary.
func (h *ReadOnlyHostFS) Open(hostPath string) (*os.File, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	resolved, err := h.resolve(hostPath, true)
	if err != nil {
		return nil, err
	}
	f, err := h.root.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("hostfs: open %q: %w", hostPath, err)
	}
	return f, nil
}

// ReadFile reads the full contents of hostPath inside the host root, resolving both
// intermediate and leaf symbolic links within the root boundary.
func (h *ReadOnlyHostFS) ReadFile(hostPath string) ([]byte, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	resolved, err := h.resolve(hostPath, true)
	if err != nil {
		return nil, err
	}
	data, err := h.root.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("hostfs: read file %q: %w", hostPath, err)
	}
	return data, nil
}

// ReadRegularFile reads up to maxBytes from a regular non-symlink file at hostPath.
// Intermediate directory symlinks (such as merged-usr /lib -> /usr/lib) are resolved
// inside the host root, whereas a symbolic link, directory, FIFO, device, or file
// exceeding maxBytes at the leaf is rejected.
func (h *ReadOnlyHostFS) ReadRegularFile(hostPath string, maxBytes int64) ([]byte, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: maxBytes must be positive, got %d", fs.ErrInvalid, maxBytes)
	}

	resolved, err := h.resolve(hostPath, false)
	if err != nil {
		return nil, err
	}

	lstatInfo, err := h.root.Lstat(resolved)
	if err != nil {
		return nil, fmt.Errorf("hostfs: lstat %q: %w", hostPath, err)
	}
	if !lstatInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("hostfs: %q is not a regular file (mode %s)", hostPath, lstatInfo.Mode().Type())
	}
	if lstatInfo.Size() > maxBytes {
		return nil, fmt.Errorf("hostfs: %q size %d exceeds maxBytes %d", hostPath, lstatInfo.Size(), maxBytes)
	}

	f, err := h.root.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("hostfs: open regular file %q: %w", hostPath, err)
	}
	defer func() { _ = f.Close() }()

	return readVerifiedRegularFile(hostPath, maxBytes, lstatInfo, f, f.Stat)
}

// Stat returns FileInfo for hostPath inside the host root, following both
// intermediate and leaf symbolic links within the root boundary.
func (h *ReadOnlyHostFS) Stat(hostPath string) (fs.FileInfo, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	resolved, err := h.resolve(hostPath, true)
	if err != nil {
		return nil, err
	}
	info, err := h.root.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("hostfs: stat %q: %w", hostPath, err)
	}
	return info, nil
}

// Lstat returns FileInfo for hostPath inside the host root, resolving intermediate
// directory symlinks within the root boundary without following the leaf component.
func (h *ReadOnlyHostFS) Lstat(hostPath string) (fs.FileInfo, error) {
	if err := h.checkOpen(); err != nil {
		return nil, err
	}
	resolved, err := h.resolve(hostPath, false)
	if err != nil {
		return nil, err
	}
	info, err := h.root.Lstat(resolved)
	if err != nil {
		return nil, fmt.Errorf("hostfs: lstat %q: %w", hostPath, err)
	}
	return info, nil
}

// ReadDir reads the directory named by hostPath inside the host root and returns
// its entries sorted by filename.
func (h *ReadOnlyHostFS) ReadDir(hostPath string) ([]fs.DirEntry, error) {
	f, err := h.Open(hostPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("hostfs: readdir %q: %w", hostPath, err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// Readlink returns the destination of the named symbolic link inside the host root,
// resolving intermediate directory symlinks without following the leaf link itself.
func (h *ReadOnlyHostFS) Readlink(hostPath string) (string, error) {
	if err := h.checkOpen(); err != nil {
		return "", err
	}
	resolved, err := h.resolve(hostPath, false)
	if err != nil {
		return "", err
	}
	target, err := h.root.Readlink(resolved)
	if err != nil {
		return "", fmt.Errorf("hostfs: readlink %q: %w", hostPath, err)
	}
	return target, nil
}

// Exists reports whether hostPath exists inside the host root after resolving
// symbolic links within the root boundary. It returns (false, nil) only when the
// target does not exist, and surfaces invalid-path, escape, or I/O errors.
func (h *ReadOnlyHostFS) Exists(hostPath string) (bool, error) {
	_, err := h.Stat(hostPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (h *ReadOnlyHostFS) checkOpen() error {
	if h == nil || h.root == nil {
		return fmt.Errorf("hostfs: host root is not open: %w", fs.ErrClosed)
	}
	return nil
}

// normalizeHostPath validates a caller-supplied host path without folding ".."
// segments. It maps "/" (and repeated slashes) to "." and rejects empty paths,
// NUL bytes, and any ".." path component.
func normalizeHostPath(hostPath string) (string, error) {
	if hostPath == "" {
		return "", fmt.Errorf("%w: host path must not be empty", fs.ErrInvalid)
	}
	if strings.ContainsRune(hostPath, '\x00') {
		return "", fmt.Errorf("%w: host path %q contains NUL byte", fs.ErrInvalid, hostPath)
	}

	rawParts := strings.Split(hostPath, "/")
	cleanParts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w: host path %q must not contain '..' segment", fs.ErrInvalid, hostPath)
		default:
			cleanParts = append(cleanParts, part)
		}
	}

	if len(cleanParts) == 0 {
		return ".", nil
	}
	return strings.Join(cleanParts, "/"), nil
}

// resolve walks hostPath component-by-component inside h.root, resolving relative
// and absolute directory symlinks back inside h.root up to maxSymlinkHops. When
// followLeaf is false, the final path component is not dereferenced.
func (h *ReadOnlyHostFS) resolve(hostPath string, followLeaf bool) (string, error) {
	normalized, err := normalizeHostPath(hostPath)
	if err != nil {
		return "", err
	}
	if normalized == "." {
		return ".", nil
	}

	queue := strings.Split(normalized, "/")
	resolved := make([]string, 0, len(queue))
	hops := 0

	for len(queue) > 0 {
		seg := queue[0]
		rest := queue[1:]

		if seg == "" || seg == "." {
			queue = rest
			continue
		}
		if seg == ".." {
			if len(resolved) == 0 {
				return "", fmt.Errorf("hostfs: symlink target in %q escapes host root", hostPath)
			}
			resolved = resolved[:len(resolved)-1]
			queue = rest
			continue
		}

		isLeaf := len(rest) == 0
		if isLeaf && !followLeaf {
			resolved = append(resolved, seg)
			break
		}

		candidate := joinSegments(resolved, seg)
		info, err := h.root.Lstat(candidate)
		if err != nil {
			return "", fmt.Errorf("hostfs: resolve %q: %w", hostPath, err)
		}

		if info.Mode()&fs.ModeSymlink != 0 {
			hops++
			if hops > maxSymlinkHops {
				return "", fmt.Errorf("hostfs: resolve %q exceeded %d symlink hops: %w", hostPath, maxSymlinkHops, syscall.ELOOP)
			}

			target, err := h.root.Readlink(candidate)
			if err != nil {
				return "", fmt.Errorf("hostfs: readlink %q while resolving %q: %w", candidate, hostPath, err)
			}
			if target == "" || strings.ContainsRune(target, '\x00') {
				return "", fmt.Errorf("%w: invalid symlink target %q while resolving %q", fs.ErrInvalid, target, hostPath)
			}

			if strings.HasPrefix(target, "/") {
				resolved = resolved[:0]
			}
			targetParts := strings.Split(target, "/")
			nextQueue := make([]string, 0, len(targetParts)+len(rest))
			nextQueue = append(nextQueue, targetParts...)
			nextQueue = append(nextQueue, rest...)
			queue = nextQueue
			continue
		}

		if !isLeaf && !info.IsDir() {
			return "", &os.PathError{Op: "open", Path: hostPath, Err: syscall.ENOTDIR}
		}

		resolved = append(resolved, seg)
		queue = rest
	}

	if len(resolved) == 0 {
		return ".", nil
	}
	return strings.Join(resolved, "/"), nil
}

func joinSegments(prefix []string, elem string) string {
	if len(prefix) == 0 {
		return elem
	}
	return strings.Join(prefix, "/") + "/" + elem
}

func readVerifiedRegularFile(
	hostPath string,
	maxBytes int64,
	lstatInfo fs.FileInfo,
	r io.Reader,
	fstat func() (fs.FileInfo, error),
) ([]byte, error) {
	fstatInfo, err := fstat()
	if err != nil {
		return nil, fmt.Errorf("hostfs: fstat %q: %w", hostPath, err)
	}
	if !fstatInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("hostfs: opened %q is not a regular file (mode %s)", hostPath, fstatInfo.Mode().Type())
	}
	if !os.SameFile(lstatInfo, fstatInfo) {
		return nil, fmt.Errorf("hostfs: file identity changed while opening %q", hostPath)
	}
	if fstatInfo.Size() > maxBytes {
		return nil, fmt.Errorf("hostfs: %q size %d exceeds maxBytes %d", hostPath, fstatInfo.Size(), maxBytes)
	}

	limit := maxBytes + 1
	if maxBytes == math.MaxInt64 {
		limit = math.MaxInt64
	}
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, fmt.Errorf("hostfs: read regular file %q: %w", hostPath, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("hostfs: %q grew beyond maxBytes %d during read", hostPath, maxBytes)
	}
	return data, nil
}
