//go:build !linux || !cgo

package preflight

import (
	"gpustack.ai/gpustack/pkg/device"
)

// supportedPreflighterCreators is empty off linux or in linux builds without cgo, mirroring the
// allocator registry: vendor preflighters link native manufacturer bindings via cgo on linux only.
var supportedPreflighterCreators = map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{}
