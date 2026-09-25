package store

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Device returns the "major:minor" of the filesystem holding path, the form mountinfo writes.
func Device(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}

	return fmt.Sprintf("%d:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev))), nil // nolint: unconvert
}
