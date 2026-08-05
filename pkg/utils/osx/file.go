package osx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InlineTilde replaces the leading ~ with the home directory.
func InlineTilde(path string) string {
	if path == "" {
		return path
	}
	if strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		hd, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(hd, path[2:])
		}
	}
	return path
}

// Open is similar to os.Open but supports a leading ~/ as the home directory.
func Open(path string) (*os.File, error) {
	p := filepath.Clean(path)
	p = InlineTilde(p)
	return os.Open(p)
}

// ExistsParentDir checks if the parent directory of the given path exists.
func ExistsParentDir(path string) bool {
	p := filepath.Clean(path)
	p = InlineTilde(p)

	return ExistsDir(filepath.Dir(p))
}

// Exists checks if the given path exists.
func Exists(path string, checks ...func(os.FileInfo) bool) bool {
	p := filepath.Clean(path)
	p = InlineTilde(p)

	stat, err := os.Lstat(p)
	if err != nil {
		return false
	}

	for i := range checks {
		if checks[i] == nil {
			continue
		}

		if !checks[i](stat) {
			return false
		}
	}

	return true
}

// ExistsDir checks if the given path exists and is a directory.
func ExistsDir(path string) bool {
	return Exists(path, func(stat os.FileInfo) bool {
		return stat.Mode().IsDir()
	})
}

// ExistsLink checks if the given path exists and is a symbolic link.
func ExistsLink(path string) bool {
	return Exists(path, func(stat os.FileInfo) bool {
		return stat.Mode()&os.ModeSymlink != 0
	})
}

// ExistsFile checks if the given path exists and is a regular file.
func ExistsFile(path string) bool {
	return Exists(path, func(stat os.FileInfo) bool {
		return stat.Mode().IsRegular()
	})
}

// ExistsSocket checks if the given path exists and is a socket.
func ExistsSocket(path string) bool {
	return Exists(path, func(stat os.FileInfo) bool {
		return stat.Mode()&os.ModeSocket != 0
	})
}

// ExistsDevice checks if the given path exists and is a device.
func ExistsDevice(path string) bool {
	return Exists(path, func(stat os.FileInfo) bool {
		return stat.Mode()&os.ModeDevice != 0
	})
}

// MkdirAll is similar to os.MkdirAll but supports a leading ~/ as the home directory, and forces
// the leaf directory's permission bits to perm with a follow-up Chmod so the mode is
// not reduced by the process umask (os.MkdirAll applies umask to the perm argument).
func MkdirAll(name string, perm os.FileMode) error {
	p := filepath.Clean(name)
	p = InlineTilde(p)

	if err := os.MkdirAll(p, perm); err != nil {
		return err
	}

	return os.Chmod(p, perm)
}

// WriteFile is similar to os.WriteFile but supports a leading ~/ as the home directory,
// and also supports the parent directory creation.
func WriteFile(name string, data []byte, perm os.FileMode) error {
	p := filepath.Clean(name)
	p = InlineTilde(p)

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}

	return os.WriteFile(p, data, perm)
}

// CreateFile is similar to os.Create but supports a leading ~/ as the home directory,
// and also supports the parent directory creation.
func CreateFile(name string, perm os.FileMode) (*os.File, error) {
	p := filepath.Clean(name)
	p = InlineTilde(p)

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}

	return os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
}

// OpenFile is similar to os.OpenFile but supports a leading ~/ as the home directory,
// and also supports the parent directory creation.
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	p := filepath.Clean(name)
	p = InlineTilde(p)

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}

	return os.OpenFile(p, flag, perm)
}

// Symlink is similar to os.Symlink but supports a leading ~/ as the home directory,
// and also supports the parent directory creation.
func Symlink(oldname, newname string) error {
	op, np := filepath.Clean(oldname), filepath.Clean(newname)
	op, np = InlineTilde(op), InlineTilde(np)

	if err := os.MkdirAll(filepath.Dir(op), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(np), 0o700); err != nil {
		return err
	}

	return os.Symlink(oldname, newname)
}

func ForceSymlink(oldname, newname string) error {
	np := filepath.Clean(newname)
	np = InlineTilde(np)

	if err := os.Remove(np); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing destination %s: %w", np, err)
	}

	return Symlink(oldname, np)
}

// TempFile creates a temporary file with the given pattern.
func TempFile(pattern string) string {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		panic(fmt.Errorf("create temp file: %w", err))
	}

	defer func() { _ = f.Close() }()

	return f.Name()
}

// TempDir creates a temporary directory with the given pattern.
func TempDir(pattern string) string {
	n, err := os.MkdirTemp("", pattern)
	if err != nil {
		panic(fmt.Errorf("create temp dir: %w", err))
	}

	return n
}

// SubTempDir is different to TempDir.
//
// TempDir creates a temporary directory randomly,
// but SubTempDir creates a subdirectory under the temporary directory with the given path.
func SubTempDir(path string) string {
	n := filepath.Join(os.TempDir(), filepath.Clean(path))
	err := os.MkdirAll(n, 0o700)
	if err != nil {
		panic(fmt.Errorf("create temp subdir: %w", err))
	}

	return n
}

// Close closes the given io.Closer without error.
func Close(c io.Closer) {
	if c == nil {
		return
	}
	_ = c.Close()
}

// IsEmptyDir checks if the given directory is empty.
func IsEmptyDir(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	defer Close(f)

	_, err = f.Readdir(1)
	return errors.Is(err, io.EOF)
}

// IsEmptyFile checks if the given file is empty.
func IsEmptyFile(file string) bool {
	s, err := os.Lstat(file)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	if !s.Mode().IsRegular() {
		return false
	}
	return s.Size() == 0
}

// Remove removes the given file or directory,
// and also supports additional checks before removal.
func Remove(path string, checks ...func(os.FileInfo) error) error {
	p := filepath.Clean(path)
	p = InlineTilde(p)

	stat, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for i := range checks {
		if checks[i] == nil {
			continue
		}

		if err = checks[i](stat); err != nil {
			return err
		}
	}

	return os.Remove(p)
}

// RemoveDir removes the given directory, and returns an error if the path is not a directory.
func RemoveDir(path string) error {
	return Remove(path, func(stat os.FileInfo) error {
		if !stat.Mode().IsDir() {
			return fmt.Errorf("not a directory")
		}
		return nil
	})
}

// RemoveFile removes the given file, and returns an error if the path is not a regular file.
func RemoveFile(path string) error {
	return Remove(path, func(stat os.FileInfo) error {
		if !stat.Mode().IsRegular() {
			return fmt.Errorf("not a regular file")
		}
		return nil
	})
}

// RemoveLink removes the given symbolic link, and returns an error if the path is not a symbolic link.
func RemoveLink(path string) error {
	return Remove(path, func(stat os.FileInfo) error {
		if stat.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("not a symbolic link")
		}
		return nil
	})
}

// RemoveSocket removes the given socket, and returns an error if the path is not a socket.
func RemoveSocket(path string) error {
	return Remove(path, func(stat os.FileInfo) error {
		if stat.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("not a socket")
		}
		return nil
	})
}

// RemoveDevice removes the given device, and returns an error if the path is not a device.
func RemoveDevice(path string) error {
	return Remove(path, func(stat os.FileInfo) error {
		if stat.Mode()&os.ModeDevice == 0 {
			return fmt.Errorf("not a device")
		}
		return nil
	})
}

// DurableRemove removes the given file or directory,
// and also syncs the parent directory to ensure the removal is durable.
func DurableRemove(path string) error {
	p := filepath.Clean(path)
	p = InlineTilde(p)

	err := os.Remove(p)
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// DurableWrite atomically replaces the file at path with data, and makes the replacement
// durable: it writes a temporary file beside the target, syncs it, renames it into place and
// syncs the parent directory. A concurrent reader therefore observes either the previous
// contents or the complete new ones, never a partial record, and once the call returns the
// replacement survives an unclean shutdown.
//
// Both syncs are load-bearing. A rename is journaled while the data blocks it points at are
// not, so a crash inside the writeback window would otherwise leave the new name published
// over a truncated or zero-length file — a file that exists, parses as nothing, and is
// indistinguishable from one written that way on purpose.
//
// The parent directory must already exist; unlike WriteFile this creates nothing, so it can
// never decide a directory's permissions on a caller's behalf. The temporary file is named after
// the target, so one a crash leaves behind names what it was replacing.
//
// An error does not always mean the target is untouched, and a caller that retries needs to know
// which it got. Every failure up to and including the rename leaves the previous contents in
// place and no temporary file behind. A failure from the final directory sync is the exception:
// the replacement is already published and readable, and what could not be confirmed is only
// that the rename itself survives an unclean shutdown. Retrying is safe either way.
func DurableWrite(path string, data []byte, perm os.FileMode) error {
	p := filepath.Clean(path)
	p = InlineTilde(p)

	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()

	// Every step below acts through the handle rather than the path, so none of them can land on a
	// file some other writer has since put at that name, and each one that fails takes the
	// temporary file with it.
	fail := func(err error) error {
		Close(tmp)
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	// The mode is set BEFORE the sync, not after: a sync flushes the inode as well as the data, so
	// the mode reaching disk is part of the same guarantee. Chmod'ing afterwards would leave a crash
	// in that window publishing the file under the temporary file's own creation mode (0600) instead
	// of perm — harmless for a record only its writer reads, but not for one a container must read.
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}

	if err := os.Rename(name, p); err != nil {
		_ = os.Remove(name)
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	defer Close(d)

	err = d.Sync()
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
