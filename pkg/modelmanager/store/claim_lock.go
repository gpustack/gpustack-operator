package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// claimLockPollInterval is how often lockClaims retries the nonblocking lock and rechecks the
// caller's context while another process holds it. It is the polling interval between checkpoints,
// not a bound on cancellation latency: a caller is asked at entry, before each try, between tries,
// and after a try succeeds.
const claimLockPollInterval = 10 * time.Millisecond

// lockClaimMu admits one claim path at a time in this process and honors a caller whose context is
// done, so a caller does not queue behind a holder that is itself stuck waiting for another process.
// Every claim path takes it before lockClaims, so the two locks always nest in one order.
func (s *Store) lockClaimMu(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.claimSem <- struct{}{}:
		// The send can win the select against a Done that turned ready while the caller waited: a
		// caller that gave up does not keep the admission, so the slot goes back at once.
		if err := ctx.Err(); err != nil {
			<-s.claimSem

			return err
		}

		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlockClaimMu gives the claim path back.
func (s *Store) unlockClaimMu() { <-s.claimSem }

// lockClaims serializes reference admission with collection across processes sharing this
// node-local cache. Keep the lock file in place for the root's life. It waits by trying a
// nonblocking lock and checking the context at entry, before each try, between tries, and again
// after a try succeeds, so a canceled or deadlined caller gives up instead of blocking forever on
// a holder it cannot see, and does not begin or keep the work on a try that races its own Done.
// The caller's context does not interrupt work it has already begun under the lock.
func (s *Store) lockClaims(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("lock cache claims: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(s.root, ledgerDir, "claims.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open cache claim lock: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()

			return nil, fmt.Errorf("lock cache claims: %w", err)
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			// A try can catch the lock free in the same instant the caller's own deadline fires:
			// a caller that already gave up releases the lock again by closing the file it opened,
			// which is also the descriptor's one close.
			if err := ctx.Err(); err != nil {
				_ = file.Close()

				return nil, fmt.Errorf("lock cache claims: %w", err)
			}

			return func() { _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			break
		}
		// Another holder has the lock, or a signal interrupted the call: give the caller a chance
		// to leave before trying again, and close the file on the way out.
		timer := time.NewTimer(claimLockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()

			return nil, fmt.Errorf("lock cache claims: %w", ctx.Err())
		case <-timer.C:
		}
	}
	_ = file.Close()

	return nil, fmt.Errorf("lock cache claims: %w", err)
}
