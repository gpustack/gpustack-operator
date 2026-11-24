package mock

import (
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const (
	// EnvMockAllocatedDevices carries the comma-separated mock accelerator or partition IDs
	// granted to a container.
	EnvMockAllocatedDevices = "GPUSTACK_MOCK_ALLOCATED_DEVICES"

	// EnvMockAllocationMode carries the lowercase allocation mode name served for the container
	// ("exclusive", "shared", "sliced", "partitioned", or "visibility").
	EnvMockAllocationMode = "GPUSTACK_MOCK_ALLOCATION_MODE"

	// EnvMockManufacturer carries the manufacturer or resource family name served for the container.
	EnvMockManufacturer = "GPUSTACK_MOCK_MANUFACTURER"

	// EnvHygonMIGVisibleDevice is the partition selection variable read by Hygon Multi-Instance runtimes.
	EnvHygonMIGVisibleDevice = "DMI_MIG_VISIBLE_DEVICE"
)

var manufacturerVisibleDevicesEnv = map[string]string{
	nodefeature.ManufacturerAMD:       "ROCR_VISIBLE_DEVICES",
	nodefeature.ManufacturerAscend:    "ASCEND_VISIBLE_DEVICES",
	nodefeature.ManufacturerCambricon: "CAMBRICON_VISIBLE_DEVICES",
	nodefeature.ManufacturerHygon:     "HIP_VISIBLE_DEVICES",
	nodefeature.ManufacturerIluvatar:  "IX_VISIBLE_DEVICES",
	nodefeature.ManufacturerMetaX:     "MACA_VISIBLE_DEVICES",
	nodefeature.ManufacturerMThreads:  "MTHREADS_VISIBLE_DEVICES",
	nodefeature.ManufacturerNVIDIA:    "NVIDIA_VISIBLE_DEVICES",
	nodefeature.ManufacturerTHead:     "PPU_VISIBLE_DEVICES",
}

// VisibleDevicesEnv returns the standard visible-devices environment variable name for manufacturer.
// Unknown manufacturer names fall back to "<UPPERCASE_MANUFACTURER>_VISIBLE_DEVICES".
func VisibleDevicesEnv(manufacturer string) string {
	if env, ok := manufacturerVisibleDevicesEnv[manufacturer]; ok {
		return env
	}
	trimmed := strings.TrimSpace(manufacturer)
	if trimmed == "" {
		return "GPU_VISIBLE_DEVICES"
	}
	return strings.ToUpper(trimmed) + "_VISIBLE_DEVICES"
}

func buildMockAllocateEnvs(
	manufacturer string,
	mode workercore.DeviceAllocationMode,
	ids []string,
	partitionBacked bool,
) map[string]string {
	joined := strings.Join(ids, ",")
	visEnv := VisibleDevicesEnv(manufacturer)
	envs := map[string]string{
		EnvMockAllocatedDevices: joined,
		EnvMockAllocationMode:   device.PreflightModeOf(mode),
		EnvMockManufacturer:     manufacturer,
		visEnv:                  joined,
	}
	if partitionBacked && manufacturer == nodefeature.ManufacturerHygon {
		envs[EnvHygonMIGVisibleDevice] = joined
	}
	return envs
}
