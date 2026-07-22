package nvml

import "testing"

func TestMigProfileIDsForComputeSlices(t *testing.T) {
	// The kept "<C>g.<M>gb" profiles and the NVML GPU-/compute-instance profile ids
	// each maps to. The slice-count->id relationship is a fixed NVML lookup, not
	// arithmetic (7-slice->4, 8-slice->5, 6-slice->6), so it is pinned per profile.
	testCases := []struct {
		name         string
		computeSlice int32
		wantGI       uint32
		wantCI       uint32
	}{
		{"1g.5gb / 1g.10gb (1 slice)", 1, GPU_INSTANCE_PROFILE_1_SLICE, COMPUTE_INSTANCE_PROFILE_1_SLICE},
		{"2g.10gb (2 slices)", 2, GPU_INSTANCE_PROFILE_2_SLICE, COMPUTE_INSTANCE_PROFILE_2_SLICE},
		{"3g.20gb (3 slices)", 3, GPU_INSTANCE_PROFILE_3_SLICE, COMPUTE_INSTANCE_PROFILE_3_SLICE},
		{"4g.20gb (4 slices)", 4, GPU_INSTANCE_PROFILE_4_SLICE, COMPUTE_INSTANCE_PROFILE_4_SLICE},
		{"6 slices", 6, GPU_INSTANCE_PROFILE_6_SLICE, COMPUTE_INSTANCE_PROFILE_6_SLICE},
		{"7g.40gb (7 slices)", 7, GPU_INSTANCE_PROFILE_7_SLICE, COMPUTE_INSTANCE_PROFILE_7_SLICE},
		{"8 slices (a100-80gb whole card)", 8, GPU_INSTANCE_PROFILE_8_SLICE, COMPUTE_INSTANCE_PROFILE_8_SLICE},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ids, ok := MigProfileIDsForComputeSlices(tc.computeSlice)
			if !ok {
				t.Fatalf("MigProfileIDsForComputeSlices(%d) ok=false, want true", tc.computeSlice)
			}
			if ids.GpuInstanceProfileID != tc.wantGI {
				t.Errorf("GpuInstanceProfileID = %d, want %d", ids.GpuInstanceProfileID, tc.wantGI)
			}
			if ids.ComputeInstanceProfileID != tc.wantCI {
				t.Errorf("ComputeInstanceProfileID = %d, want %d", ids.ComputeInstanceProfileID, tc.wantCI)
			}
			if ids.ComputeInstanceEngineProfileID != COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED {
				t.Errorf("ComputeInstanceEngineProfileID = %d, want SHARED(%d)",
					ids.ComputeInstanceEngineProfileID, COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
			}
		})
	}

	// A slice count NVML defines no profile for is rejected.
	if _, ok := MigProfileIDsForComputeSlices(5); ok {
		t.Errorf("MigProfileIDsForComputeSlices(5) ok=true, want false (no 5-slice profile)")
	}
}
