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

// getNumaNodeByBDF reports UNKNOWN on every platform that is not linux, because there is no sysfs
// to read it from. It returned "0" before, which is the same overclaim the linux path was fixed for
// and worse here: it made this the answer on EVERY device, so a proximity comparison run on darwin
// could only ever say the accelerator and the interface share a node. A measurement point with one
// possible reading cannot disagree with anything.
func getNumaNodeByBDF(bdf string) string {
	return ""
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
