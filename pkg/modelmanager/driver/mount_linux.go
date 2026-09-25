package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// HostMounter mounts in the plugin's mount namespace, which shares the kubelet pods directory with
// the host through Bidirectional propagation.
type HostMounter struct{}

// IsMountPoint reports whether path is a mount point, from mountinfo: a bind mount of a directory on
// the same filesystem has the same device as its parent, so a stat cannot tell.
func (HostMounter) IsMountPoint(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	mounts, err := store.ParseMountInfo(f)
	if err != nil {
		return false, err
	}
	clean := filepath.Clean(path)
	for _, m := range mounts {
		if m.MountPoint == clean {
			return true, nil
		}
	}

	return false, nil
}

// BindReadOnly bind-mounts source onto target read-only, without set-user-ID or device files. A
// bind mount takes the read-only flag only on a remount, so it is two calls; a failed remount is
// undone rather than left writable.
func (HostMounter) BindReadOnly(source, target string) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s onto %s: %w", source, target, err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		if uerr := unix.Unmount(target, 0); uerr != nil {
			return fmt.Errorf("make %s read-only: %w; and the writable bind is left: %w", target, err, uerr)
		}
		return fmt.Errorf("make %s read-only: %w", target, err)
	}

	return nil
}

// IsReadOnly reports whether the mount at path is read-only, without set-user-ID or device files,
// from the flags statfs reports for it.
func (HostMounter) IsReadOnly(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, fmt.Errorf("statfs %s: %w", path, err)
	}
	want := int64(unix.ST_RDONLY | unix.ST_NOSUID | unix.ST_NODEV)

	return st.Flags&want == want, nil
}

// Unmount unmounts target; one that is not mounted is not an error.
func (HostMounter) Unmount(target string) error {
	if err := unix.Unmount(target, 0); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unmount %s: %w", target, err)
	}

	return nil
}
