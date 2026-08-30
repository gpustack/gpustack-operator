package deviceplugin

import (
	"fmt"
	"os"
	"path/filepath"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// AcceleratorInjectionPreflighter is an optional companion implemented by the manufacturers whose
// allocator can be built over a substituted vendor driver.
//
// It hands back the manufacturer's own responder rather than an injection, so what a preflight
// reports is produced by the code an allocation runs rather than by a preflight-shaped copy of it.
// There is consequently nothing for the two to drift apart over, which no test could guarantee
// about a copy.
//
// ContainerAllocateResponder is what it offers because every allocator implements it, so one shape
// serves every manufacturer. What a caller may do with the value is not uniform, and the difference
// matters:
//
//   - Asserting it to LogicalSlicedResponder is allowed and sometimes necessary. Ascend renders its
//     logical slice inside GetContainerAllocateResponse, so driving the universal entry point
//     covers it; AMD renders its own in GetLogicalSlicedResponse, so for that manufacturer the
//     universal entry point returns the same injection whatever the mode, and its slicing is
//     reachable only through the assertion. Neither method touches hardware — they render files,
//     under the paths RedirectHostWrites neutralizes.
//   - Asserting it to PhysicalSlicedResponder is forbidden. ActuatePhysicalSliced creates a real
//     hardware partition, and a preflight killed mid-run would leave an ownerless one behind. This
//     is a rule and not a property of the type: the value returned is the allocator's own server,
//     which is the whole point of the seam, and that server implements more than it is meant to be
//     asked for here. Nothing in this repository asserts for it, and nothing should.
type AcceleratorInjectionPreflighter interface {
	// PreflightResponder returns the responder this manufacturer's allocator serves mode with,
	// built over a driver seam that records what it was asked to write instead of writing it,
	// together with the function that undoes the redirection it set up.
	//
	// **The caller must defer the restore.** Between the call and the restore, every host path this
	// responder renders into points at a scratch directory the restore then removes; outside that
	// window the paths are the real ones and driving the responder would write to the node. The
	// restore is returned rather than left implicit because the set of paths is not the same for
	// every manufacturer — NVIDIA's sliced path creates a shared lock directory of its own, on top
	// of the two every manufacturer shares — so a caller cannot know what to undo, and a
	// manufacturer that redirected without restoring would leave the rest of the process pointing
	// at a directory that no longer exists.
	//
	// It returns an error rather than a responder that would reach the device, so a caller cannot
	// obtain a writing one by accident.
	PreflightResponder(mode workercore.DeviceAllocationMode) (ContainerAllocateResponder, func(), error)
}

// NewPreflightRedirect creates the scratch directory a simulated pass renders into, points the
// shared host paths at it, and returns that directory together with the function restoring the
// paths and removing it.
//
// It is what every manufacturer's PreflightResponder composes: the two shared paths are handled
// here once, and a manufacturer carrying host paths of its own hands them over as private — each
// moved under the returned root, each restored by the returned function, and each recorded in
// PreflightRehosts so a caller that never knew their names can still rewrite them back.
//
// Handing a private path over rather than joining it under the root by hand is what keeps an
// emitted command runnable. A path only the manufacturer knows about is a path nothing rewrites,
// and what comes out then names a scratch directory that stopped existing when this process did.
func NewPreflightRedirect(private ...*string) (root string, restore func(), err error) {
	root, err = os.MkdirTemp("", "gpustack-preflight-")
	if err != nil {
		return "", nil, fmt.Errorf("create preflight scratch directory: %w", err)
	}

	restoreShared := RedirectHostWrites(root)
	restorePrivate := redirectPrivatePaths(root, private)
	return root, func() {
		restorePrivate()
		restoreShared()
		_ = os.RemoveAll(root)
	}, nil
}

// preflightRehosts maps every scratch path a private redirect currently holds open onto the path the
// host knows it by. It is package-global for the same reason the two shared paths are, and carries
// the same constraint: safe only where nothing is serving.
var preflightRehosts = map[string]string{}

// PreflightRehosts reports, for every redirect open right now, the scratch path a manufacturer's
// private host path was pointed at and the path a real allocation would have used.
//
// It exists because the rewrite and the knowledge are in different places: the manufacturer is the
// only one who knows it carries a private path, and the caller driving the responder is the only one
// who has the injection to rewrite. Reading it is only meaningful inside the redirect window — a
// restored redirect removes its entries, because a path nobody redirected must not be rewritten.
func PreflightRehosts() map[string]string {
	out := make(map[string]string, len(preflightRehosts))
	for scratch, real := range preflightRehosts {
		out[scratch] = real
	}
	return out
}

// redirectPrivatePaths points each private path under root, records how to address it on the host,
// and returns the function undoing both.
//
// The original is followed through any redirect already open, because a private path is one package
// variable rather than one per redirect: a second redirect opened while the first still holds it
// reads the first one's scratch value as the original, and recording that verbatim would rewrite the
// second tenant's mount onto a directory the first tenant's restore removes.
func redirectPrivatePaths(root string, private []*string) (restore func()) {
	// restoreTo and host are the same value for the first redirect and different for a nested one:
	// the variable goes back to whatever held it on the way in, while the mount is addressed by the
	// path a real allocation would have used. Conflating them would have a nested restore hand the
	// still-open outer redirect the production path, and every mount it rendered would then be
	// written to the node.
	type moved struct {
		path      *string
		restoreTo string
		dest      string
	}

	movements := make([]moved, 0, len(private))
	for i, path := range private {
		if path == nil {
			continue
		}
		restoreTo := *path
		host := restoreTo
		if outer, redirected := preflightRehosts[host]; redirected {
			host = outer
		}
		// Indexed, so two private paths sharing a basename get two directories. Naming the scratch
		// after the basename alone would map /tmp/x and /var/run/x onto one, and the second
		// registration would then rewrite both mounts onto the second path.
		dest := filepath.Join(root, fmt.Sprintf("%d-%s", i, filepath.Base(host)))

		*path = dest
		preflightRehosts[dest] = host
		movements = append(movements, moved{path: path, restoreTo: restoreTo, dest: dest})
	}

	return func() {
		for i := len(movements) - 1; i >= 0; i-- {
			m := movements[i]
			delete(preflightRehosts, m.dest)
			*m.path = m.restoreTo
		}
	}
}

// RedirectHostWrites points the two host paths a responder renders its injected files into at root,
// and returns the function that puts them back.
//
// Substituting a manufacturer's driver does not on its own make a pass read-only: a responder can
// reach the host through this second door as well — Ascend's sliced path renders its quota config
// under these paths — so a pass that only faked the driver would still write to the very node it
// claims to be inspecting.
//
// The paths are process-global, so this is safe only where nothing is serving: a one-shot command,
// or a test. Calling it while a device-plugin server is running would hand the kubelet mounts
// pointing into a directory that stops existing the moment the caller restores them.
func RedirectHostWrites(root string) (restore func()) {
	origLib, origPods := OperatorLibDir, OperatorPodsDir
	OperatorLibDir = filepath.Join(root, "lib")
	OperatorPodsDir = filepath.Join(root, "pods")

	return func() {
		OperatorLibDir, OperatorPodsDir = origLib, origPods
	}
}
