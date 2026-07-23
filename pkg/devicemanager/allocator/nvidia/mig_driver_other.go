//go:build !linux

package nvidia

import "errors"

// newMigDriver returns a stub on non-linux platforms. The device-manager runs only on linux,
// and linking the cgo binding/nvml into a darwin test binary (which links Go's plugin
// package) aborts at dyld load on the unresolved NVML symbols, so the real driver
// (mig_driver_linux.go) is linux-only; this stub lets the package build and its NVML-free
// marker/slot-pick core be table-tested on darwin.
func newMigDriver() migDriver {
	return stubMigDriver{}
}

var errStubMigDriver = errors.New("nvml mig driver is not available on this platform")

type stubMigDriver struct{}

func (stubMigDriver) CardState(_, _ string, _, _ int32) (migCardState, error) {
	return migCardState{}, errStubMigDriver
}

func (stubMigDriver) CreateInstance(_, _ string, _, _ int32, _ migPlacement) (migInstance, error) {
	return migInstance{}, errStubMigDriver
}

func (stubMigDriver) DestroyInstance(string, migInstance) error { return errStubMigDriver }

func (stubMigDriver) ListInstances() ([]migLiveInstance, error) { return nil, errStubMigDriver }
