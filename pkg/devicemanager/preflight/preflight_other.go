//go:build !linux

package preflight

import (
	"gpustack.ai/gpustack/pkg/device"
)

// supportedPreflighterCreators is empty off linux, mirroring the allocator registry: the device
// manager runs only on linux, and the manufacturer bindings the preflighters drive cannot be linked
// into a darwin binary at all.
var supportedPreflighterCreators = map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{}
