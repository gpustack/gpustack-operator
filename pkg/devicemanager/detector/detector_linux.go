package detector

import (
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/amd"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/ascend"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/cambricon"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/hygon"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/iluvatar"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/metax"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/mthreads"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/nvidia"
	"gpustack.ai/gpustack/pkg/devicemanager/detector/thead"
	"k8s.io/apimachinery/pkg/util/sets"
)

var supportedDetectorCreators = []struct {
	m string
	c func(device.DetectorOptions) device.Detector
}{
	{amd.Manufacturer, amd.New},
	{ascend.Manufacturer, ascend.New},
	{cambricon.Manufacturer, cambricon.New},
	{hygon.Manufacturer, hygon.New},
	{iluvatar.Manufacturer, iluvatar.New},
	{metax.Manufacturer, metax.New},
	{mthreads.Manufacturer, mthreads.New},
	{nvidia.Manufacturer, nvidia.New},
	{thead.Manufacturer, thead.New},
}

func getAllowedManufacturers(request sets.Set[string]) (allowed sets.Set[string]) {
	original := sets.New[string]()
	for i := range supportedDetectorCreators {
		original.Insert(supportedDetectorCreators[i].m)
	}
	return original.Intersection(request)
}

func getAllowedDetectorCreators(allowed sets.Set[string]) (creators []func(device.DetectorOptions) device.Detector) {
	creators = make([]func(device.DetectorOptions) device.Detector, 0, len(supportedDetectorCreators))
	for i := range supportedDetectorCreators {
		if allowed.Has(supportedDetectorCreators[i].m) {
			creators = append(creators, supportedDetectorCreators[i].c)
		}
	}
	return creators
}
