package preflight

import (
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/amd"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/ascend"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/cambricon"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/hygon"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/iluvatar"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/metax"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/mthreads"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/nvidia"
	"gpustack.ai/gpustack/pkg/devicemanager/allocator/thead"
)

// supportedPreflighterCreators holds a creator for each manufacturer that has a preflighter, which
// is now every manufacturer with an allocator.
//
// Having one is not the same as having something to read. Six of the nine read an allocation-time
// precondition through a driver seam; the other three read no driver at all when they serve an
// allocation, and say so in their own words rather than being left out — which would report them
// through the caller's absence rather than their own answer, and would cost them the simulated and
// measured depths too, since those need the injection their responder produces.
//
// The map stays keyed by manufacturer and is populated as each vertical lands. A manufacturer is
// listed once its own package carries a preflighter and never before: listing one earlier would not
// compile, and listing one whose preflighter is a stub would report a check nobody wrote as a check
// that passed.
var supportedPreflighterCreators = map[string]func(device.PreflighterOptions) device.AcceleratorPreflighter{
	amd.Manufacturer:       amd.NewPreflighter,
	ascend.Manufacturer:    ascend.NewPreflighter,
	cambricon.Manufacturer: cambricon.NewPreflighter,
	hygon.Manufacturer:     hygon.NewPreflighter,
	iluvatar.Manufacturer:  iluvatar.NewPreflighter,
	metax.Manufacturer:     metax.NewPreflighter,
	mthreads.Manufacturer:  mthreads.NewPreflighter,
	nvidia.Manufacturer:    nvidia.NewPreflighter,
	thead.Manufacturer:     thead.NewPreflighter,
}
