package nvidia

import (
	"fmt"
	"strings"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
)

// deriveSlicedProfiles turns a probed set of NVIDIA MIG GPU-instance profiles into the
// card's physical-slice profile inventory: it drops the +me (dedicated media engines)
// and +gfx (graphics-capable) variants, derives each kept profile's canonical name and
// slice geometry, and de-duplicates by name.
//
// infos holds one entry per successfully probed GI profile id; the caller skips ids that
// NVML reports as unsupported. cardMemoryMiB is the card's total memory in MiB, used to
// express a profile's memory size in memory-slice units (a slice is 1/8 of the card).
func deriveSlicedProfiles(infos []nvml.GpuInstanceProfileInfo_v3, cardMemoryMiB uint64) []device.AcceleratorPhysicalSlicedProfile {
	perSlice := cardMemoryMiB / 8

	seen := make(map[string]struct{}, len(infos))
	var profiles []device.AcceleratorPhysicalSlicedProfile
	for _, info := range infos {
		name := info.GetName()
		if isMediaOrGraphicsVariant(info, name) {
			continue
		}

		// Round the memory size to whole memory slices: round(MemorySizeMB / perSlice).
		var memorySlices int32
		if perSlice > 0 {
			memorySlices = int32((info.MemorySizeMB + perSlice/2) / perSlice)
		}

		if name == "" {
			// V1 fallback (no Name): derive the "<compute>g.<mem>gb" name from geometry,
			// never from MemorySizeMB/1024 (which rounds off the marketing size).
			memGiB := int64(memorySlices) * int64(cardMemoryMiB/1024) / 8
			name = fmt.Sprintf("%dg.%dgb", info.SliceCount, memGiB)
		}

		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}

		profiles = append(profiles, device.AcceleratorPhysicalSlicedProfile{
			Name:          name,
			MemoryMib:     int64(info.MemorySizeMB),
			ComputeSlices: int32(info.SliceCount),
			MemorySlices:  memorySlices,
			Count:         int32(info.InstanceCount),
		})
	}
	return profiles
}

// isMediaOrGraphicsVariant reports whether a probed profile is a media-engine (+me,
// +me.all) or graphics (+gfx) variant, which GPUStack does not expose. Only the plain
// "<C>g.<M>gb" profiles are kept; every variant carries a "+..." suffix, so the presence
// of a "+" in the NVML Name is the discriminator — the profile ids are not a stable
// cross-generation taxonomy (on Hopper the base ids are the +me variants). The V3 GFX
// capability bit is a backstop for a graphics profile whose naming differs. Only the V1
// path (no Name) falls back to the id range, which is safe because the variants exist
// solely on Hopper+/Blackwell whose drivers expose the versioned (named) call.
func isMediaOrGraphicsVariant(info nvml.GpuInstanceProfileInfo_v3, name string) bool {
	if name != "" {
		return strings.ContainsRune(name, '+') ||
			info.Capabilities&nvml.GPU_INSTANCE_PROFILE_CAPS_GFX != 0
	}
	return info.Id >= nvml.GPU_INSTANCE_PROFILE_1_SLICE_GFX
}

// detectMigProfiles probes every GPU instance profile id on the device and returns the card's
// physical-slice profile inventory (filtered and derived by deriveSlicedProfiles). Unsupported
// ids surface as non-success returns and are skipped.
func detectMigProfiles(dev nvml.Device, cardMemoryMiB uint64) []device.AcceleratorPhysicalSlicedProfile {
	var infos []nvml.GpuInstanceProfileInfo_v3
	for id := uint32(0); id < nvml.GPU_INSTANCE_PROFILE_COUNT; id++ {
		info, ret := dev.GetGpuInstanceProfileInfo(id)
		if !ret.IsSuccess() {
			continue
		}
		infos = append(infos, info)
	}
	return deriveSlicedProfiles(infos, cardMemoryMiB)
}

// maxProfileCount returns the card's physical-slice ceiling — the largest per-profile Count
// (e.g. 7 on A100, from 7x 1g.5gb). Zero for an empty profile list.
func maxProfileCount(profiles []device.AcceleratorPhysicalSlicedProfile) int32 {
	var ceiling int32
	for _, p := range profiles {
		if p.Count > ceiling {
			ceiling = p.Count
		}
	}
	return ceiling
}
