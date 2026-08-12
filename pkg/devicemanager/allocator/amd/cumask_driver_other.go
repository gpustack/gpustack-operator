//go:build !linux

package amd

import (
	"errors"
)

// The seam is a build-tag pair whose only production caller is the device-plugin server wiring, so
// nothing in this file has an in-package caller yet. Assert the stub's shape here, so the two
// halves of the pair cannot drift apart unnoticed.
var _ func(string, string) (Topology, error) = readTopology

var errTopologyUnsupported = errors.New("hsa topology reader is not available on this platform")

// readTopology is a stub on non-linux platforms. The device-manager runs only on linux, and
// linking the cgo binding/hsa into a darwin test binary (which links Go's plugin package through
// pkg/deviceplugin) aborts at dyld load on the unresolved HSA symbols, so the real reader
// (cumask_driver_linux.go) is linux-only; this stub lets the package build and its
// manufacturer-library-free mask arithmetic be table-tested on the development platform.
func readTopology(_, _ string) (Topology, error) {
	return Topology{}, errTopologyUnsupported
}
