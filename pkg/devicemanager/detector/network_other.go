//go:build !linux

package detector

import (
	"errors"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// DetectInterfaces reports that this worker's interfaces cannot be enumerated.
//
// It returns an error rather than an empty list, and the distinction is the point: an empty
// inventory is indistinguishable from a worker that genuinely has no interfaces, so the caller
// keeps whatever was recorded before instead of publishing a claim this platform cannot make.
func DetectInterfaces() ([]workercore.DeviceInterface, error) {
	return nil, errors.New("the network interface inventory is only available on linux")
}
