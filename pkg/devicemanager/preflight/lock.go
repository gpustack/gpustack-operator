package preflight

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// lockName is the file two preflights on one node contend for, under the tree preflight already
// owns. It is a file rather than a directory because the lock is the open descriptor, not the
// entry: the entry is left behind on purpose and means nothing on its own.
const lockName = "lock"

// errContended marks the one failure that means another preflight holds this node, as opposed to
// every other reason the lock could not be taken: a permission denial, a read-only host root, a
// directory that could not be created. Both stop the pass and both are reported, but only one of
// them is another operator, and saying so when it is not sends someone looking for a process that
// does not exist. Measured on hardware: a non-root run reported "another preflight holds this
// node's lock" while its own reason read "permission denied".
var errContended = errors.New("another preflight is already running on this node")

// flock is syscall.Flock, indirected the way this package already indirects the host's own command
// runner. The branch below turns on one errno against every other, and taking the lock twice can
// only ever produce the one -- so without this, the half that matters is the half no test reaches.
var flock = syscall.Flock

// lockHost takes this node's preflight lock, and returns the function that drops it.
//
// Two preflights on one node are not two independent runs. They share the label every probe
// container carries, so each one's stale-container sweep removes the other's *live* probes -- and a
// probe killed mid-measurement reports the accelerator as unable to slice, which is a healthy node
// failed by nothing but a second operator. They also share one rendered-artifact tree and one
// barrier root, both keyed by a Pod UID this command fabricates the same way every time.
//
// Refused rather than queued: this is a command someone is watching, and the useful answer to "a
// preflight is already running here" is that sentence, not a prompt that hangs until the other one
// finishes.
//
// flock rather than a lock file's existence, because the failure mode this whole command is built
// around is a run that dies without cleaning up. A descriptor lock is released by the kernel when
// the process goes, whatever killed it; an existence check would leave a node permanently refusing
// to be preflighted after one SIGKILL, and the remedy would be a stale file nobody documents.
func lockHost(hostRoot string) (release func(), err error) {
	dir := filepath.Join(hostRoot, deviceplugin.OperatorPreflightDir)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the preflight directory to lock: %w", err)
	}

	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open the preflight lock: %w", err)
	}

	if err = flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		// Only the would-block answer means another preflight. The call has other ways to fail --
		// a kernel without it, a filesystem that does not implement it, an interrupted call -- and
		// each of those is a node this cannot lock rather than a node someone else is holding.
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("take the preflight lock on %s: %w",
				filepath.Join(deviceplugin.OperatorPreflightDir, lockName), err)
		}
		return nil, fmt.Errorf("%w: it holds %s. Two runs sweep each other's probe containers and "+
			"share one rendered-artifact tree, so this one would report failures that belong to "+
			"neither: %w", errContended,
			filepath.Join(deviceplugin.OperatorPreflightDir, lockName), err)
	}

	return func() {
		// Unlocked explicitly rather than left to the close, so the order is the one written here:
		// the lock is what the next run waits on, and it is dropped after everything this run swept.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
