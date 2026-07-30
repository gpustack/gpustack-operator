//go:build !linux

package ascend

import (
	"errors"

	klog "k8s.io/klog/v2"
)

// newShareDriver returns a stub on non-linux platforms. The device-manager runs only on linux,
// and linking the cgo binding/dcmi into a darwin test binary (which links Go's plugin package)
// aborts at dyld load on the unresolved DCMI symbols, so the real driver
// (share_driver_linux.go) is linux-only; this stub lets the package build and its dcmi-free
// injection core be table-tested on darwin.
func newShareDriver(_ klog.Logger) shareDriver {
	return stubShareDriver{}
}

var errStubShareDriver = errors.New("dcmi container-share driver is not available on this platform")

type stubShareDriver struct{}

func (stubShareDriver) GetShareEnabled(_, _ int32) (bool, error) { return false, errStubShareDriver }

func (stubShareDriver) SetShareEnabled(_, _ int32, _ bool) error { return errStubShareDriver }
