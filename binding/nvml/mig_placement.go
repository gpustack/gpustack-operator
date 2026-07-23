package nvml

// sliceCountToGPUInstanceProfileID and sliceCountToComputeInstanceProfileID map a
// profile's compute-slice count to the NVML GPU-instance and compute-instance profile
// ids. The relationship is a fixed NVML lookup, not arithmetic: the 7-, 8- and 6-slice
// profiles carry ids 4, 5 and 6 respectively (the id order is 1,2,3,4,7,8,6 slices), so
// the mapping is spelled out rather than computed.
var (
	sliceCountToGPUInstanceProfileID = map[int32]uint32{
		1: GPU_INSTANCE_PROFILE_1_SLICE,
		2: GPU_INSTANCE_PROFILE_2_SLICE,
		3: GPU_INSTANCE_PROFILE_3_SLICE,
		4: GPU_INSTANCE_PROFILE_4_SLICE,
		6: GPU_INSTANCE_PROFILE_6_SLICE,
		7: GPU_INSTANCE_PROFILE_7_SLICE,
		8: GPU_INSTANCE_PROFILE_8_SLICE,
	}
	sliceCountToComputeInstanceProfileID = map[int32]uint32{
		1: COMPUTE_INSTANCE_PROFILE_1_SLICE,
		2: COMPUTE_INSTANCE_PROFILE_2_SLICE,
		3: COMPUTE_INSTANCE_PROFILE_3_SLICE,
		4: COMPUTE_INSTANCE_PROFILE_4_SLICE,
		6: COMPUTE_INSTANCE_PROFILE_6_SLICE,
		7: COMPUTE_INSTANCE_PROFILE_7_SLICE,
		8: COMPUTE_INSTANCE_PROFILE_8_SLICE,
	}
)

// MigInstanceProfileIDs bundles the NVML profile ids needed to create one MIG instance
// of a kept "<C>g.<M>gb" profile. For these profiles the compute instance spans the
// whole GPU instance, so ComputeInstanceProfileID is the slice-count-matched
// COMPUTE_INSTANCE_PROFILE_* and the engine profile is always SHARED.
type MigInstanceProfileIDs struct {
	GpuInstanceProfileID           uint32
	ComputeInstanceProfileID       uint32
	ComputeInstanceEngineProfileID uint32
}

// MigProfileIDsForComputeSlices returns the GPU-instance and compute-instance profile
// ids used to create a MIG instance whose GPU instance spans computeSlices compute
// slices. It returns ok=false for a slice count NVML defines no profile for (e.g. 5).
// The compute instance spans the whole GPU instance with the SHARED engine profile.
func MigProfileIDsForComputeSlices(computeSlices int32) (MigInstanceProfileIDs, bool) {
	gi, ok := sliceCountToGPUInstanceProfileID[computeSlices]
	if !ok {
		return MigInstanceProfileIDs{}, false
	}
	return MigInstanceProfileIDs{
		GpuInstanceProfileID:           gi,
		ComputeInstanceProfileID:       sliceCountToComputeInstanceProfileID[computeSlices],
		ComputeInstanceEngineProfileID: COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED,
	}, true
}
