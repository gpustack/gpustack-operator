package metadata

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// publishView serializes repair and publication across processes sharing a view root.
// The persistent lock file must not be removed while the root is in use.
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
	if existing, err := Verify(final); err == nil && viewSatisfies(existing, digest, required) {
		return nil
	}
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	return os.Rename(stage, final)
}
