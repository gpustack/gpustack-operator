//go:build !linux

package allocator

import (
	"gpustack.ai/gpustack/pkg/device"
)

var supportedAllocatorCreators = map[string]func(device.AllocatorOptions) device.Allocator{}

// rdmaAllocatorCreator is left unset: the RDMA servers bind to the device-plugin directory the
// kubelet serves, which is a Linux surface, so a non-Linux device manager starts none rather than
// failing on a directory that does not exist.
var rdmaAllocatorCreator func(device.AllocatorOptions) device.Allocator
