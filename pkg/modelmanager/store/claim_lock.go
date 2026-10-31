package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockClaims serializes reference admission with collection across processes
// sharing this node-local cache. Keep the lock file in place for the root's life.
func (s *Store) lockClaims() (func(), error) {
	file, err := os.OpenFile(filepath.Join(s.root, ledgerDir, "claims.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open cache claim lock: %w", err)
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock cache claims: %w", err)
	}
	return func() { _ = file.Close() }, nil
}
