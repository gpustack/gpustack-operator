package broker

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLedgerLock takes the broker root's exclusive single-writer lock.
// The lock lives for the broker's whole life: two brokers sharing one
// ledger root would interleave record writes, generation counters, and
// assignment offers, so the second one refuses to start instead.
func acquireLedgerLock(root string) (*os.File, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("prepare the ledger root %s: %w", root, err)
	}
	path := filepath.Join(root, "writer.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the ledger lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another broker holds the ledger lock %s; "+
			"each vendor scope needs its own broker root: %w", path, err)
	}
	return file, nil
}
