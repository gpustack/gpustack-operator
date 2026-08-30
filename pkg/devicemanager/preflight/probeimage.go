package preflight

import (
	"fmt"
	"strings"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// ascendProbeImages are the vendor images preflight's Ascend probe starts, one per CANN
// runtime-major/family pair -- exactly the base images pack/gpustack-operator/Dockerfile builds
// vcann-rt against (the xbuild-ascend-cann-<major>-<family> stages). They are keyed the same way
// the Ascend allocator names the lib subdirectory it mounts (ascendCANNDir in
// pkg/devicemanager/allocator/ascend/deviceplugin.go), which ascendCANNDir below mirrors.
//
// Measured on hardware: a bare base image exits 127 here. The injected ld.so.preload resolves
// against libc_sec/libdcmi, which only a CANN image carries.
var ascendProbeImages = map[string]string{
	"cann-8-910b": "quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11",
	"cann-8-910c": "quay.io/ascend/cann:8.5.0-a3-ubuntu22.04-py3.11",
	"cann-9-910b": "quay.io/ascend/cann:9.1.0-beta.3-910b-ubuntu22.04-py3.12",
	"cann-9-910c": "quay.io/ascend/cann:9.1.0-beta.3-a3-ubuntu22.04-py3.12",
	"cann-9-950":  "quay.io/ascend/cann:9.1.0-beta.3-950-ubuntu22.04-py3.12",
	"cann-9-310p": "quay.io/ascend/cann:9.1.0-beta.3-310p-ubuntu22.04-py3.12",
}

// nvidiaProbeImages are the vendor images preflight's NVIDIA probe starts, one per CUDA runtime
// major -- the same base images xbuild-nvidia-cuda-<major> in the Dockerfile builds HAMi-core's
// libvgpu.so against. They are keyed the same way the NVIDIA allocator names the lib subdirectory
// it mounts (nvidiaCUDADir in pkg/devicemanager/allocator/nvidia/deviceplugin.go), which
// nvidiaCUDADir below mirrors.
var nvidiaProbeImages = map[string]string{
	"cuda-12": "nvidia/cuda:12.9.2-cudnn-devel-ubi8",
	"cuda-13": "nvidia/cuda:13.0.3-cudnn-devel-ubi8",
}

// amdProbeImage is the base preflight's AMD probe starts.
//
// AMD's slicing shim links only glibc -- it hooks ROCr through dlsym rather than linking a vendor
// runtime -- so, unlike Ascend and NVIDIA, no ROCm-specific image is needed: any base works. This
// is the same Ubuntu base pack/gpustack-operator/Dockerfile itself builds the final image FROM
// (UBUNTU_IMAGE), so it is not a guess about what runs, it is the image already proven to.
const amdProbeImage = "ubuntu:24.04"

// ResolveProbeImage picks the vendor image preflight starts a probe container from.
//
// override, when non-empty, wins verbatim -- including for a manufacturer or family this package
// has no default for. Overriding is the only way to probe one of those.
//
// A manufacturer or family with no resolvable default is reported as an error naming
// --probe-image, not guessed: guessing here would start a container that cannot run the vendor's
// runtime and report a node that is fine as broken.
func ResolveProbeImage(manufacturer, family, runtimeVersion, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	switch manufacturer {
	case nodefeature.ManufacturerAscend:
		dir := ascendCANNDir(runtimeVersion, family)
		if img, ok := ascendProbeImages[dir]; ok {
			return img, nil
		}
		return "", fmt.Errorf(
			"no default probe image for ascend family %q on CANN runtime %q; pass --probe-image",
			family, runtimeVersion)
	case nodefeature.ManufacturerNVIDIA:
		dir := nvidiaCUDADir(runtimeVersion)
		if img, ok := nvidiaProbeImages[dir]; ok {
			return img, nil
		}
		return "", fmt.Errorf(
			"no default probe image for nvidia CUDA runtime %q; pass --probe-image", runtimeVersion)
	case nodefeature.ManufacturerAMD:
		return amdProbeImage, nil
	default:
		return "", fmt.Errorf("no default probe image for manufacturer %q; pass --probe-image", manufacturer)
	}
}

// ascendCANNDir mirrors ascendCANNDir in pkg/devicemanager/allocator/ascend/deviceplugin.go -- the
// lib subdirectory name the Ascend allocator mounts, and pack/gpustack-operator/Dockerfile stages
// into. It is reproduced here rather than imported because it is unexported there; the two must be
// kept in step by hand.
func ascendCANNDir(runtimeVersion, family string) string {
	return "cann-" + device.RuntimeMajor(runtimeVersion, "8") + "-" + strings.ToLower(family)
}

// nvidiaCUDADir mirrors nvidiaCUDADir in pkg/devicemanager/allocator/nvidia/deviceplugin.go -- the
// lib subdirectory name the NVIDIA allocator mounts. It is reproduced here rather than imported
// because it is unexported there; the two must be kept in step by hand.
func nvidiaCUDADir(runtimeVersion string) string {
	return "cuda-" + device.RuntimeMajor(runtimeVersion, "12")
}
