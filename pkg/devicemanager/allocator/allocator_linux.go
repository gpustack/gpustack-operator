package allocator

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

var supportedAllocatorCreators = map[string]func(device.AllocatorOptions) device.Allocator{
	amd.Manufacturer:       amd.New,
	ascend.Manufacturer:    ascend.New,
	cambricon.Manufacturer: cambricon.New,
	hygon.Manufacturer:     hygon.New,
	iluvatar.Manufacturer:  iluvatar.New,
	metax.Manufacturer:     metax.New,
	mthreads.Manufacturer:  mthreads.New,
	nvidia.Manufacturer:    nvidia.New,
	thead.Manufacturer:     thead.New,
}
