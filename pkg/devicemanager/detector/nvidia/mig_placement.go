package nvidia

import (
	"gpustack.ai/gpustack/binding/nvml"
)

// sliceCountToGpuInstanceProfileID and sliceCountToComputeInstanceProfileID map a
// profile's compute-slice count to the NVML GPU-instance and compute-instance
// profile ids. The relationship is a fixed NVML lookup, not arithmetic: the
// 7-, 8- and 6-slice profiles carry ids 4, 5 and 6 respectively (the id order is
// 1,2,3,4,7,8,6 slices), so the mapping is spelled out rather than computed.
var (
	sliceCountToGpuInstanceProfileID = map[int32]uint32{
		1: nvml.GPU_INSTANCE_PROFILE_1_SLICE,
		2: nvml.GPU_INSTANCE_PROFILE_2_SLICE,
		3: nvml.GPU_INSTANCE_PROFILE_3_SLICE,
		4: nvml.GPU_INSTANCE_PROFILE_4_SLICE,
		6: nvml.GPU_INSTANCE_PROFILE_6_SLICE,
		7: nvml.GPU_INSTANCE_PROFILE_7_SLICE,
		8: nvml.GPU_INSTANCE_PROFILE_8_SLICE,
	}
	sliceCountToComputeInstanceProfileID = map[int32]uint32{
		1: nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE,
		2: nvml.COMPUTE_INSTANCE_PROFILE_2_SLICE,
		3: nvml.COMPUTE_INSTANCE_PROFILE_3_SLICE,
		4: nvml.COMPUTE_INSTANCE_PROFILE_4_SLICE,
		6: nvml.COMPUTE_INSTANCE_PROFILE_6_SLICE,
		7: nvml.COMPUTE_INSTANCE_PROFILE_7_SLICE,
		8: nvml.COMPUTE_INSTANCE_PROFILE_8_SLICE,
	}
)

// migInstanceProfileIDs bundles the NVML profile ids needed to create one MIG
// instance of a kept "<C>g.<M>gb" profile. For these profiles the compute instance
// spans the whole GPU instance, so ComputeInstanceProfileID is the
// slice-count-matched COMPUTE_INSTANCE_PROFILE_* and the engine profile is always
// SHARED.
type migInstanceProfileIDs struct {
	GpuInstanceProfileID           uint32
	ComputeInstanceProfileID       uint32
	ComputeInstanceEngineProfileID uint32
}

// migProfileIDsForComputeSlices returns the GPU-instance and compute-instance
// profile ids used to create a MIG instance whose GPU instance spans computeSlices
// compute slices. It returns ok=false for a slice count NVML defines no profile for
// (e.g. 5). The compute instance spans the whole GPU instance with the SHARED
// engine profile.
func migProfileIDsForComputeSlices(computeSlices int32) (migInstanceProfileIDs, bool) {
	gi, ok := sliceCountToGpuInstanceProfileID[computeSlices]
	if !ok {
		return migInstanceProfileIDs{}, false
	}
	return migInstanceProfileIDs{
		GpuInstanceProfileID:           gi,
		ComputeInstanceProfileID:       sliceCountToComputeInstanceProfileID[computeSlices],
		ComputeInstanceEngineProfileID: nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED,
	}, true
}

// computeFreeProfiles counts, per profile name, how many more GPU instances can
// still be created on a card given its occupied placement intervals. For each
// profile it counts the legal placements (possible[name]) whose memory-slice
// interval does not overlap any occupied interval. Profiles with no buildable
// placement are omitted, so the result lists only what can currently be created.
func computeFreeProfiles(occupied []nvml.GpuInstancePlacement, possible map[string][]nvml.GpuInstancePlacement) map[string]int {
	free := make(map[string]int, len(possible))
	for name, slots := range possible {
		var n int
		for _, slot := range slots {
			if !placementOverlapsAny(slot, occupied) {
				n++
			}
		}
		if n > 0 {
			free[name] = n
		}
	}
	return free
}

// placementOverlapsAny reports whether slot's memory-slice interval
// [Start, Start+Size) intersects any occupied interval. Two half-open intervals
// [a, a+m) and [b, b+n) overlap iff a < b+n and b < a+m.
func placementOverlapsAny(slot nvml.GpuInstancePlacement, occupied []nvml.GpuInstancePlacement) bool {
	slotEnd := slot.Start + slot.Size
	for _, occ := range occupied {
		if slot.Start < occ.Start+occ.Size && occ.Start < slotEnd {
			return true
		}
	}
	return false
}
