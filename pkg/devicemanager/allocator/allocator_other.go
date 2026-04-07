//go:build !linux

package allocator

import (
	"gpustack.ai/gpustack/pkg/device"
)

var supportedAllocatorCreators = map[string]func(device.AllocatorOptions) device.Allocator{}
