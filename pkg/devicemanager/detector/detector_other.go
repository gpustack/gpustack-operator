//go:build !linux

package detector

import (
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/device"
)

func getAllowedManufacturers(sets.Set[string]) sets.Set[string] {
	return sets.New[string]()
}

func getAllowedDetectorCreators(sets.Set[string]) []func(device.DetectorOptions) device.Detector {
	return nil
}
