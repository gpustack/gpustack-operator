//go:build !linux

package thead

import "errors"

// newMigDriver returns a stub on non-linux platforms. The device-manager runs only on linux, and
// linking the cgo vendor management binding into a darwin test binary aborts at dyld load on the
// unresolved vendor symbols, so the real driver (mig_driver_linux.go) is linux-only; this stub
// lets the package build and its vendor-library-free marker/slot-pick core be table-tested on the
// development platform.
func newMigDriver() migDriver {
	return stubMigDriver{}
}

// The seam is a build-tag pair whose only production caller is the device-plugin server wiring, so
// nothing in this file has an in-package caller yet. Assert the stub satisfies the seam here, so
// the two halves of the pair cannot drift apart unnoticed.
var _ migDriver = newMigDriver()

var errStubMigDriver = errors.New("hgml mig driver is not available on this platform")

type stubMigDriver struct{}

func (stubMigDriver) CardState(_, _ string, _, _ int32) (migCardState, error) {
	return migCardState{}, errStubMigDriver
}

func (stubMigDriver) CreateInstance(_, _ string, _, _ int32, _ migPlacement) (migInstance, error) {
	return migInstance{}, errStubMigDriver
}

func (stubMigDriver) DestroyInstance(string, migInstance) error { return errStubMigDriver }

func (stubMigDriver) InstanceProcesses(string, migInstance) (int, error) { return 0, errStubMigDriver }

func (stubMigDriver) ListInstances() ([]migLiveInstance, error) { return nil, errStubMigDriver }

func (stubMigDriver) CardInstances(string) ([]migInstance, error) { return nil, errStubMigDriver }
