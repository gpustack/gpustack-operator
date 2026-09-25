package gc

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Statfs reads the usage of the filesystem holding path. Used counts every block not available to
// an unprivileged writer, reserved ones included, the way kubelet reads a filesystem's available
// space, so the watermarks and kubelet's eviction thresholds measure the same thing.
func Statfs(path string) (Usage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Usage{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize) // nolint: gosec // a block size is positive.

	return Usage{Total: st.Blocks * bsize, Used: (st.Blocks - st.Bavail) * bsize}, nil
}
