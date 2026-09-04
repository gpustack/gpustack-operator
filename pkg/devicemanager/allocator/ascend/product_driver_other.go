//go:build !linux

package ascend

import (
	"errors"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/devicemanager/ascendproduct"
)

// newProductDriver returns a stub on non-linux platforms. The device-manager runs only on linux, and
// linking the cgo binding/dcmi into a darwin test binary (which links Go's plugin package) aborts at
// dyld load on the unresolved DCMI symbols, so the real driver (product_driver_linux.go) is linux-only;
// this stub lets the package build and its dcmi-free callers be table-tested on darwin.
func newProductDriver(_ klog.Logger) ascendproduct.Driver {
	return stubProductDriver{}
}

var errStubProductDriver = errors.New("dcmi topology reader is not available on this platform")

type stubProductDriver struct{}

func (stubProductDriver) MainboardID(_, _ int32) (uint32, error) { return 0, errStubProductDriver }

func (stubProductDriver) SuperPodType(_, _ int32) (uint32, error) { return 0, errStubProductDriver }
