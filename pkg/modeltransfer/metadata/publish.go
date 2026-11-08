package metadata

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// publishView serializes publication across processes sharing a view root.
// A content-addressed final view directory is never removed here: once a
// 64-hex directory exists on disk, only Store.RemoveUnreferencedView may
// retire it under the claim locks. The persistent lock file must not be
// removed while the root is in use.
func publishView(stage, final, digest string, required []string) error {
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(final), ".publish.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return err
	}
	existing, verifyErr := Verify(final)
	if verifyErr == nil && viewSatisfies(existing, digest, required) {
		return nil
	}
	if _, err := os.Lstat(final); err == nil {
		if isHex64(filepath.Base(final)) {
			if verifyErr != nil {
				return verifyErr
			}
			return fmt.Errorf("%w: existing view %s does not satisfy request", ErrTampered, filepath.Base(final))
		}
		if err := os.RemoveAll(final); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(stage, final)
}

// publishStoreView promotes stage into the content-addressed final view path.
// When a corrupt directory already stands at final, it is retired only through
// st.RemoveUnreferencedView after releasing .publish.lock, so .publish.lock
// and the store's claim locks are never nested and a view referenced by any
// other holder is never replaced.
func publishStoreView(
	ctx context.Context, st *store.Store, stage, final, hex, digest string, required []string, selfVolumeID ...string,
) error {
	err := publishView(stage, final, digest, required)
	if err == nil || st == nil || hex == "" {
		return err
	}
	retired, rerr := st.RemoveUnreferencedView(ctx, final, hex, selfVolumeID...)
	if rerr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	if !retired {
		if existing, verr := Verify(final); verr == nil && viewSatisfies(existing, digest, required) {
			return nil
		}
		return err
	}
	return publishView(stage, final, digest, required)
}
