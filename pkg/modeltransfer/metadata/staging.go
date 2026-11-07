package metadata

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// A staging directory under a shared view root is owned by an exclusive
// filesystem lock from before the directory exists until after it has been
// promoted or discarded. The lock file stands beside the directory, named
// after it, so promotion never carries the lock into a published view: a
// view holds only the files its record lists. A builder holds the lock
// without release for the entire life of its staging, so taking the lock
// without contention proves the staging inactive, however the lock came to
// be free; no elapsed time or naming convention is ever consulted. The
// lock is created and held before the directory exists, but its versioned
// marker — naming the exact staging directory and the lock file's own
// device and inode — is published only once that directory stands: an
// active builder is protected because recovery must take the lock before
// it ever reads a marker, and a crash before publication leaves an
// unmarked residue that recovery conservatively preserves, never a proof
// that names a directory the builder did not create. A planted, replaced,
// or copied lock file carries no matching marker and authorizes nothing.
const (
	stagingPrefix     = ".staging-"
	stagingLockSuffix = ".lock"
)

// stagingLockKind is the only marker kind recovery accepts, and
// maxLockMarkerBytes bounds its read: the marker is small, so
// anything larger, empty, malformed, or trailing is corruption or a foreign
// file, never ownership.
const (
	stagingLockKind    = "gpustack-model-transfer-metadata-staging-lock/v1"
	maxLockMarkerBytes = 1 << 10
)

// stagingLockMarker is the versioned ownership proof a builder publishes in
// its held lock after creating its staging directory.
type stagingLockMarker struct {
	Kind string `json:"kind"`
	// Dir is the exact staging directory basename this lock belongs to.
	Dir string `json:"dir"`
	// LockDev and LockIno are the lock file's own device and inode as the
	// builder held them. A copy of this marker on any other inode records
	// an identity that file does not have and is refused.
	LockDev uint64 `json:"lockDev"`
	LockIno uint64 `json:"lockIno"`
}

// errStagingNameTaken marks a burned name: the builder's fresh lock or its
// staging directory already exists there, and the caller tries a fresh
// name.
var errStagingNameTaken = errors.New("the staging name is already taken")

// Staging is one build's private staging directory and its ownership lock.
type Staging struct {
	dir      string
	lockPath string
	lock     *os.File
}

// Dir is the staging directory path.
func (s *Staging) Dir() string {
	return s.dir
}

// LockPath is the path of the ownership lock file standing beside Dir.
func (s *Staging) LockPath() string {
	return s.lockPath
}

// NewStaging creates a staging directory under root together with its
// ownership lock. The lock is created and taken before the directory
// exists; its marker is published once the directory stands and before the
// handle is returned. Any staging directory another process can observe is
// therefore owned by a held lock, and reclaiming by that proof can never
// race a live builder into deleting its tree.
func NewStaging(root string) (*Staging, error) {
	return newStaging(root, stagingPrefix)
}

// newStaging is NewStaging with a caller-chosen name prefix, for the nested
// staging a build stages under its own destination's name.
func newStaging(root, prefix string) (*Staging, error) {
	for range 8 {
		suffix, err := randomSuffix()
		if err != nil {
			return nil, err
		}
		stage, err := newStagingNamed(root, prefix, suffix)
		if errors.Is(err, errStagingNameTaken) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return stage, nil
	}
	return nil, errors.New("a staging ownership lock is unavailable")
}

// newStagingNamed builds one staging under an exact name; a name that is
// already taken comes back as errStagingNameTaken. Every failure path
// leaves no lock that could authorize a directory this builder does not
// own: the builder-created lock is unlinked only while the held file still
// backs its path, and the marker is written through the held descriptor,
// so a swapped path can neither redirect nor truncate it.
func newStagingNamed(root, prefix, suffix string) (*Staging, error) {
	dirBase := prefix + suffix
	lockPath := filepath.Join(root, dirBase+stagingLockSuffix)
	dir := filepath.Join(root, dirBase)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, errStagingNameTaken
	}
	if err != nil {
		return nil, err
	}
	// drop unlinks this builder's lock only under its own held proof, then
	// always lets the lock go.
	drop := func() {
		if lockStillAtPath(lock, lockPath) == nil {
			_ = os.Remove(lockPath)
		}
		_ = lock.Close()
	}
	if err := flockExclusive(lock); err != nil {
		drop()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errStagingNameTaken
		}
		return nil, err
	}
	if err := lockStillAtPath(lock, lockPath); err != nil {
		drop()
		return nil, errStagingNameTaken
	}
	var held unix.Stat_t
	if err := unix.Fstat(int(lock.Fd()), &held); err != nil {
		drop()
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		// A directory already standing at this name is not this builder's,
		// and this builder has published nothing yet that a crash could
		// leave behind to authorize deleting it: the lock goes, the
		// directory stays.
		drop()
		if errors.Is(err, os.ErrExist) {
			return nil, errStagingNameTaken
		}
		return nil, err
	}
	// A path swapped since the lock was taken would strand the directory
	// as residue no marker can authorize; detect it and retry under a
	// fresh name instead of exposing it.
	if err := lockStillAtPath(lock, lockPath); err != nil {
		_ = os.Remove(dir)
		drop()
		return nil, errStagingNameTaken
	}
	// Only now, with this builder's own directory created and its lock
	// proven to back its path, is the marker published: a crash before
	// this point leaves an unmarked residue that recovery conservatively
	// preserves, and never a proof naming a directory the builder did not
	// create.
	marker, err := json.Marshal(stagingLockMarker{
		Kind: stagingLockKind, Dir: dirBase,
		LockDev: uint64(held.Dev), LockIno: held.Ino,
	})
	if err == nil {
		_, err = lock.WriteAt(marker, 0)
	}
	if err == nil {
		err = syncFile(lock)
	}
	if err != nil {
		// The proof could not be published. This builder removes only the
		// empty directory it just created and only while its held file
		// still backs the path; when that proof is ambiguous, the
		// unmarked residue stays for recovery to judge.
		if lockStillAtPath(lock, lockPath) == nil {
			_ = os.Remove(dir)
			_ = os.Remove(lockPath)
		}
		_ = lock.Close()
		return nil, err
	}
	return &Staging{dir: dir, lockPath: lockPath, lock: lock}, nil
}

// Release discards the staging directory and its lock. Nothing is
// destroyed unless this builder's held descriptor still backs its recorded
// lock path: a replaced or vanished path is somebody else's state, never
// this builder's to clean. With the identity proven, the lock path is
// unlinked only once the tree is gone, so a failed tree removal keeps the
// lock and the residue stays an owned, lockable staging recovery can prove
// inactive and reclaim. Once the descriptor is gone, Release is inert:
// repeated calls and abandoned residue belong to recovery, not to a
// builder that no longer holds proof.
func (s *Staging) Release() {
	if s == nil || s.lock == nil {
		return
	}
	if lockStillAtPath(s.lock, s.lockPath) != nil {
		_ = s.lock.Close()
		s.lock = nil
		return
	}
	if err := os.RemoveAll(s.dir); err == nil {
		_ = os.Remove(s.lockPath)
	}
	_ = s.lock.Close()
	s.lock = nil
}

// Abandon gives up the held descriptor without removing anything, leaving
// the directory, its lock file, and the published marker exactly as a
// builder that dies mid-build leaves them: owned state that recovery can
// prove inactive and reclaim on its own terms.
func (s *Staging) Abandon() {
	if s == nil {
		return
	}
	_ = s.lock.Close()
	s.lock = nil
}

// ReclaimAbandonedStaging reclaims the staging directories under root whose
// ownership lock can be taken and whose published marker names this exact
// staging and matches the held file. An active builder's lock is held
// without release, so its tree survives. Lock files without a valid
// marker, markers that name another directory or another inode, and
// candidates predating the marker are all proof-less residue: they are
// preserved. A `.staging-` name is only a candidate: locks that are not
// regular files, that were replaced by something else, or that no longer
// back their path once held, are left untouched with everything around
// them. Published views, the publishers' lock, and every unrelated entry
// are untouched.
func ReclaimAbandonedStaging(root string) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, stagingPrefix) {
			continue
		}
		path := filepath.Join(root, name)
		switch {
		case entry.IsDir() && !strings.HasSuffix(name, stagingLockSuffix):
			if err := reclaimStaging(path, path+stagingLockSuffix); err != nil {
				return err
			}
		case !entry.IsDir() && strings.HasSuffix(name, stagingLockSuffix):
			// An orphan lock: its builder died between publishing the
			// marker and creating the directory, or the directory is
			// already gone. The directory pass owns any staging that
			// still stands.
			dirPath := filepath.Join(root, strings.TrimSuffix(name, stagingLockSuffix))
			if _, err := os.Lstat(dirPath); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := reclaimStaging(dirPath, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// reclaimStaging removes a staging directory and its lock once the lock
// can be taken, the held file is proven to back the candidate path, and
// the lock's marker names this exact staging with the held file's own
// identity. The lock is held across both removals, so no builder can
// appear in between: a builder owns the lock, and its published marker,
// before its directory exists. A held, replaced, unmarked, or
// non-matching candidate is skipped with its staging; a tree that cannot
// be removed keeps its lock path so the owned residue stays reclaimable;
// any other failure is reported and stops the sweep.
func reclaimStaging(dirPath, lockPath string) error {
	lock, err := openLockCandidate(lockPath)
	if err != nil {
		return err
	}
	if lock == nil {
		return nil
	}
	defer lock.Close()
	if err := flockExclusive(lock); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	// An uncontended take is the inactivity proof. Before deleting
	// anything, the held file must still back the path, and the lock's
	// marker must name this exact staging and this held file: proof and
	// name that disagree prove nothing, and everything under them is
	// preserved.
	if err := lockStillAtPath(lock, lockPath); err != nil {
		return nil
	}
	marker, err := readLockMarker(lock)
	if err != nil {
		return nil
	}
	var held unix.Stat_t
	if err := unix.Fstat(int(lock.Fd()), &held); err != nil {
		return err
	}
	if marker.LockDev != uint64(held.Dev) || marker.LockIno != held.Ino ||
		marker.Dir != strings.TrimSuffix(filepath.Base(lockPath), stagingLockSuffix) {
		return nil
	}
	if err := os.RemoveAll(dirPath); err != nil {
		return err
	}
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// openLockCandidate opens a staging lock candidate without following
// symlinks and without blocking on special files. A name under the
// protocol's namespace is only a candidate, never proof of protocol
// ownership: entries that are not regular files are reported as no
// candidate and left untouched.
func openLockCandidate(lockPath string) (*os.File, error) {
	info, err := os.Lstat(lockPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		// The entry changed shape between the Lstat and the open, so it is
		// not the regular candidate it claimed to be.
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) ||
			errors.Is(err, unix.EISDIR) || errors.Is(err, unix.ENXIO) {
			return nil, nil
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), lockPath), nil
}

// readLockMarker bounded-reads and strictly validates the ownership marker
// of an opened, held lock candidate: an empty, oversized, malformed,
// foreign-kind, or trailing-marker file is no proof at all.
func readLockMarker(lock *os.File) (stagingLockMarker, error) {
	var marker stagingLockMarker
	raw, err := io.ReadAll(io.LimitReader(lock, maxLockMarkerBytes+1))
	if err != nil {
		return marker, err
	}
	if len(raw) == 0 || len(raw) > maxLockMarkerBytes {
		return marker, errors.New("the staging lock holds no bounded marker")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return marker, errors.New("the staging lock marker carries trailing bytes")
	}
	if marker.Kind != stagingLockKind || !safeRelative(marker.Dir) {
		return marker, errors.New("the staging lock marker is not of this protocol")
	}
	return marker, nil
}

// flockExclusive takes the lock without waiting, retrying an interrupted
// call the way the publishers' lock does. Every caller here holds or seeks
// a lock that no one may wait on: a builder's fresh lock, or a lock whose
// holder proves a tree active.
func flockExclusive(lock *os.File) error {
	for {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// lockStillAtPath reports whether the open lock still backs its path as a
// regular file. The directory entry is read with Lstat, so a symlinked
// path is refused even when it points at the held file itself: both the
// held descriptor and the entry as it stands in the directory must be the
// same regular file.
func lockStillAtPath(lock *os.File, lockPath string) error {
	var linked, held unix.Stat_t
	if err := unix.Lstat(lockPath, &linked); err != nil {
		return err
	}
	if err := unix.Fstat(int(lock.Fd()), &held); err != nil {
		return err
	}
	if linked.Dev != held.Dev || linked.Ino != held.Ino ||
		linked.Mode&unix.S_IFMT != unix.S_IFREG ||
		held.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("the staging lock no longer backs its path")
	}
	return nil
}

func randomSuffix() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// syncFile flushes a file to disk; indirected the way this repository
// indirects syscalls, so tests can force and observe the publication path.
var syncFile = func(f *os.File) error { return f.Sync() }
