//go:build !linux

package binding

import (
	"runtime"
)

func getCPUSize() int {
	return runtime.NumCPU()
}

func getNumaNodeSize() int {
	return 1
}

func getCPUNumaNodeMapping() []int {
	mapping := make([]int, cpuSize)
	for i := 0; i < cpuSize; i++ {
		mapping[i] = 0
	}

	return mapping
}

func getNumaNodeByBDF(bdf string) string {
	return "0"
}

func getPhysicalPackageIdByBDF(bdf string) string {
	return bdf
}

func getPCIDevices(vendors, classPrefixes []string) PCIDevices {
	return PCIDevices{}
}

func getPCIDeviceNames(vendors []string) PCIDeviceNames {
	return PCIDeviceNames{}
}

func getLibFromEnv(libName string) string {
	return ""
}

func getLibFromLdCache(libName string) string {
	return ""
}

func getSystemDeviceFromPath(path string) *SystemDevice {
	return nil
}
