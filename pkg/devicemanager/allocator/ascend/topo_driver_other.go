//go:build !linux

package ascend

import (
	"errors"

	klog "k8s.io/klog/v2"
)

// newTopoDriver returns a stub on non-linux platforms. The device-manager runs only on linux, and
// linking the cgo binding/dcmi into a darwin test binary (which links Go's plugin package) aborts at
// dyld load on the unresolved DCMI symbols, so the real driver (topo_driver_linux.go) is linux-only;
// this stub lets the package build and its dcmi-free resolution be table-tested on darwin.
func newTopoDriver(_ klog.Logger) topoDriver {
	return stubTopoDriver{}
}

var errStubTopoDriver = errors.New("dcmi topology reader is not available on this platform")

type stubTopoDriver struct{}

func (stubTopoDriver) MainboardID(_, _ int32) (uint32, error) { return 0, errStubTopoDriver }

func (stubTopoDriver) SuperPodType(_, _ int32) (uint32, error) { return 0, errStubTopoDriver }
