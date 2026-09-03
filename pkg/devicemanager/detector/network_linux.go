package detector

import (
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// sysfsRoot is where the kernel exposes the device model the interface inventory reads.
const sysfsRoot = "/sys"

// DetectInterfaces reports this worker's network interfaces, each carrying its RDMA link state.
//
// Naming the root is the whole of the platform-specific part. Everything that interprets what is
// under it is platform-independent on purpose: this file does not compile off Linux, so anything
// placed here would also escape the compiler, the linter and the tests on the development
// platform.
func DetectInterfaces() ([]workercore.DeviceInterface, error) {
	return enumerateInterfaces(sysfsRoot)
}
