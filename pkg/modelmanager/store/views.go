package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

const viewsSubpath = "model-transfer/views"

type referenceReadError struct {
	err error
}

func (e *referenceReadError) Error() string { return e.err.Error() }
func (e *referenceReadError) Unwrap() error { return e.err }

// IsReferenceReadError reports whether err came from failing to lock or read
// the store's reference ledger during a view removal check.
func IsReferenceReadError(err error) bool {
	var target *referenceReadError
	return errors.As(err, &target)
}

// beforeViewRemove runs between a view's atomic retirement and the bulk
// unlink of its trash, indirected the way this repository tests (the
// metadata package's syncFile): a test parks the deletion there to prove
// the claim locks are already free.
var beforeViewRemove = func() {}

// ViewsRoot is where this store keeps its metadata views, beside and never
// inside the published layout. The collector, the mounts and the metadata
// endpoint all resolve views here, so the path has one definition.
func (s *Store) ViewsRoot() string {
	return filepath.Join(s.root, filepath.FromSlash(viewsSubpath))
}

// Acquire records a reference under the same claim locks a removal reads,
// without the publication gate: a use whose content may not exist yet — a
// metadata view to resolve or build, a fetch in flight, a mount to come —
// protects itself before it resolves. The locks end once the reference is
// registered; what they buy is ordering, not exclusion for the work that
// follows. A removal that held the lock an instant earlier has already
// retired the content, and the caller sees its resolution fail and undoes
// this reference; a removal that waits for the lock sees the reference and
// keeps the content. Cancellation stops waiting for the locks and
// registers nothing.
func (s *Store) Acquire(ctx context.Context, r Ref) error {
	if err := s.lockClaimMu(ctx); err != nil {
		return err
	}
	defer s.unlockClaimMu()
	unlock, err := s.lockClaims(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	return s.WriteRef(r)
}

// RemoveUnreferencedView removes the final metadata view directory at path
// unless a reference names the source digest hex, reporting whether it did.
// It reads the references under the same lock Claim and Acquire register
// them under, so a mount that acquired the view's digest an instant earlier
// keeps the view; a reference that cannot be read keeps it too. Only the
// rename out of the view root holds that lock: a builder's publishView or a
// findView caller either sees the whole directory or none of it, and the
// bytes are removed after the locks are released, rather than stalling
// every claim on the node while a directory is deleted. Cancellation stops
// waiting for the claim locks without renaming the view.
//
// The path must be a directory this store keeps its views in; the caller
// derives it from its own view root. Removal goes through trash like a
// published tree's, so a crash midway leaves only trash for the next start.
func (s *Store) RemoveUnreferencedView(ctx context.Context, path, hex string, ignoreVolumeID ...string) (bool, error) {
	trash, err := s.trashUnreferencedView(ctx, path, hex, ignoreVolumeID...)
	if trash == "" {
		return false, err
	}
	beforeViewRemove()

	return true, errors.Join(err, removeAllWritable(trash))
}

// trashUnreferencedView renames the view at path into trash/ unless a
// reference names hex (ignoring any caller-owned pre-resolution reference in
// ignoreVolumeID), and returns where it went, or "" when a reference keeps it
// or it is already gone. The claim locks end when it returns.
func (s *Store) trashUnreferencedView(ctx context.Context, path, hex string, ignoreVolumeID ...string) (string, error) {
	if err := s.lockClaimMu(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return "", ctxErr
		}
		return "", &referenceReadError{err: err}
	}
	defer s.unlockClaimMu()
	unlock, err := s.lockClaims(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return "", ctxErr
		}
		return "", &referenceReadError{err: err}
	}
	defer unlock()

	refs, err := s.Refs()
	if err != nil {
		return "", &referenceReadError{err: err}
	}
	for _, r := range refs {
		if r.Hex == hex && !slices.Contains(ignoreVolumeID, r.VolumeID) {
			return "", nil
		}
	}
	dst := filepath.Join(s.root, trashDir, "view-"+filepath.Base(path)+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(path, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return dst, err
	}

	return dst, nil
}
