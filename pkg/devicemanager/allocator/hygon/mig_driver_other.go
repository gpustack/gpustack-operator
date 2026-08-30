//go:build !linux

package hygon

import "errors"

// newMigDriver returns a stub on non-linux platforms. The device-manager runs only on linux, and
// this package's servers link Go's plugin package, which aborts a darwin test binary at dyld load
// over a cgo vendor binding -- so the real driver (mig_driver_linux.go) is linux-only. This stub
// lets the package build and its driver-free record and placement core be table-tested on the
// development platform.
func newMigDriver() migDriver {
	return stubMigDriver{}
}

// The seam is a build-tag pair whose only production caller is the device-plugin server wiring, so
// nothing in this file has an in-package caller yet. Assert the stub satisfies the seam here, so the
// two halves of the pair cannot drift apart unnoticed.
var _ migDriver = newMigDriver()

var errStubMigDriver = errors.New("hygon multi-instance driver is not available on this platform")

type stubMigDriver struct{}

func (stubMigDriver) CardState(_, _ string) (migCardState, error) {
	return migCardState{}, errStubMigDriver
}

func (stubMigDriver) CreateInstance(_, _ string, _ migPlacement) (migInstance, error) {
	return migInstance{}, errStubMigDriver
}

func (stubMigDriver) DestroyInstance(string, migInstance) error { return errStubMigDriver }

func (stubMigDriver) ListInstances() ([]migLiveInstance, error) { return nil, errStubMigDriver }
