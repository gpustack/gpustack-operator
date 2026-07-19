//go:build !linux

package cambricon

import "errors"

// newSMLUDriver returns a stub on non-linux platforms. The device-manager runs only on
// linux, so the real cnDev-backed driver (smlu_driver_linux.go) is linux-only; this stub
// lets the package build and its cnDev-free core be tested on darwin.
func newSMLUDriver() smluDriver {
	return stubSMLUDriver{}
}

var errStubDriver = errors.New("cndev sMLU driver is not available on this platform")

type stubSMLUDriver struct{}

func (stubSMLUDriver) EnsureSMLUMode(string) error { return errStubDriver }

func (stubSMLUDriver) CreateProfile(string, int, int64) (int32, error) { return 0, errStubDriver }

func (stubSMLUDriver) DestroyProfile(string, int32) error { return errStubDriver }

func (stubSMLUDriver) CreateInstance(string, int32, string) (smluInstance, error) {
	return smluInstance{}, errStubDriver
}

func (stubSMLUDriver) DestroyInstance(_, _ string) error { return errStubDriver }

func (stubSMLUDriver) ListInstances() ([]smluInstance, error) { return nil, errStubDriver }

func (stubSMLUDriver) ListProfiles() ([]profileKey, error) { return nil, errStubDriver }
