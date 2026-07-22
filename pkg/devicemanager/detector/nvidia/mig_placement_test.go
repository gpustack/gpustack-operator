package nvidia

import (
	"reflect"
	"testing"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// a100PossiblePlacements is the hardcoded A100 legal placement set (start:size in
// memory-slice units) NVML reports for the six kept profiles — the fixture the
// free-count math runs against.
var a100PossiblePlacements = map[string][]nvml.GpuInstancePlacement{
	"1g.5gb":  {{Start: 0, Size: 1}, {Start: 1, Size: 1}, {Start: 2, Size: 1}, {Start: 3, Size: 1}, {Start: 4, Size: 1}, {Start: 5, Size: 1}, {Start: 6, Size: 1}},
	"1g.10gb": {{Start: 0, Size: 2}, {Start: 2, Size: 2}, {Start: 4, Size: 2}, {Start: 6, Size: 2}},
	"2g.10gb": {{Start: 0, Size: 2}, {Start: 2, Size: 2}, {Start: 4, Size: 2}},
	"3g.20gb": {{Start: 0, Size: 4}, {Start: 4, Size: 4}},
	"4g.20gb": {{Start: 0, Size: 4}},
	"7g.40gb": {{Start: 0, Size: 8}},
}

func TestComputeFreeProfiles(t *testing.T) {
	testCases := []struct {
		name     string
		occupied []nvml.GpuInstancePlacement
		possible map[string][]nvml.GpuInstancePlacement
		want     map[string]int
	}{
		{
			// The spec's worked example: a card already holding 1x3g.20gb@slot0
			// (occupies memory slices [0,4)) can still build the reduced set.
			name:     "a100 with one 3g.20gb at slot 0",
			occupied: []nvml.GpuInstancePlacement{{Start: 0, Size: 4}},
			possible: a100PossiblePlacements,
			want:     map[string]int{"1g.5gb": 3, "1g.10gb": 2, "2g.10gb": 1, "3g.20gb": 1},
		},
		{
			// An empty card can build every profile up to its per-profile ceiling —
			// the number of legal placements NVML reports for each.
			name:     "empty a100 card",
			occupied: nil,
			possible: a100PossiblePlacements,
			want:     map[string]int{"1g.5gb": 7, "1g.10gb": 4, "2g.10gb": 3, "3g.20gb": 2, "4g.20gb": 1, "7g.40gb": 1},
		},
		{
			// One 1g.5gb at slot 0 (occupies [0,1)) reduces every profile whose only
			// low-slot placement is blocked; profiles left unbuildable drop out.
			name:     "a100 fragmented by one 1g.5gb at slot 0",
			occupied: []nvml.GpuInstancePlacement{{Start: 0, Size: 1}},
			possible: a100PossiblePlacements,
			want:     map[string]int{"1g.5gb": 6, "1g.10gb": 3, "2g.10gb": 2, "3g.20gb": 1},
		},
		{
			// A fully occupied card (whole-card 7g.40gb) leaves nothing buildable.
			name:     "a100 fully occupied by 7g.40gb",
			occupied: []nvml.GpuInstancePlacement{{Start: 0, Size: 8}},
			possible: a100PossiblePlacements,
			want:     map[string]int{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeFreeProfiles(tc.occupied, tc.possible)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("computeFreeProfiles() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

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
		{"1g.5gb / 1g.10gb (1 slice)", 1, nvml.GPU_INSTANCE_PROFILE_1_SLICE, nvml.COMPUTE_INSTANCE_PROFILE_1_SLICE},
		{"2g.10gb (2 slices)", 2, nvml.GPU_INSTANCE_PROFILE_2_SLICE, nvml.COMPUTE_INSTANCE_PROFILE_2_SLICE},
		{"3g.20gb (3 slices)", 3, nvml.GPU_INSTANCE_PROFILE_3_SLICE, nvml.COMPUTE_INSTANCE_PROFILE_3_SLICE},
		{"4g.20gb (4 slices)", 4, nvml.GPU_INSTANCE_PROFILE_4_SLICE, nvml.COMPUTE_INSTANCE_PROFILE_4_SLICE},
		{"6 slices", 6, nvml.GPU_INSTANCE_PROFILE_6_SLICE, nvml.COMPUTE_INSTANCE_PROFILE_6_SLICE},
		{"7g.40gb (7 slices)", 7, nvml.GPU_INSTANCE_PROFILE_7_SLICE, nvml.COMPUTE_INSTANCE_PROFILE_7_SLICE},
		{"8 slices (a100-80gb whole card)", 8, nvml.GPU_INSTANCE_PROFILE_8_SLICE, nvml.COMPUTE_INSTANCE_PROFILE_8_SLICE},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ids, ok := migProfileIDsForComputeSlices(tc.computeSlice)
			if !ok {
				t.Fatalf("migProfileIDsForComputeSlices(%d) ok=false, want true", tc.computeSlice)
			}
			if ids.GpuInstanceProfileID != tc.wantGI {
				t.Errorf("GpuInstanceProfileID = %d, want %d", ids.GpuInstanceProfileID, tc.wantGI)
			}
			if ids.ComputeInstanceProfileID != tc.wantCI {
				t.Errorf("ComputeInstanceProfileID = %d, want %d", ids.ComputeInstanceProfileID, tc.wantCI)
			}
			if ids.ComputeInstanceEngineProfileID != nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED {
				t.Errorf("ComputeInstanceEngineProfileID = %d, want SHARED(%d)",
					ids.ComputeInstanceEngineProfileID, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED)
			}
		})
	}

	// A slice count NVML defines no profile for is rejected.
	if _, ok := migProfileIDsForComputeSlices(5); ok {
		t.Errorf("migProfileIDsForComputeSlices(5) ok=true, want false (no 5-slice profile)")
	}
}

func TestMigProfileUnitsFold(t *testing.T) {
	const d = nodefeature.ResourceMaxUnits

	// A MIG instance folds into .sliced.units via the same VRAM-anchored
	// MemoryMibToUnits the soft .sliced.memory-mib path uses, so a MIG profile and a
	// soft slice of the same VRAM charge identical credits. These pin the H100-80GB
	// worked-example fold values (10GiB->D/8, 20GiB->D/4, 40GiB->D/2, 80GiB->D).
	const h100VRAM = int64(81920)
	testCases := []struct {
		name   string
		memMib int64
		want   int64
	}{
		{"1g.10gb -> D/8", 10 * 1024, d / 8},
		{"1g.20gb / 2g.20gb -> D/4", 20 * 1024, d / 4},
		{"3g.40gb / 4g.40gb -> D/2", 40 * 1024, d / 2},
		{"7g.80gb (whole card) -> D", 80 * 1024, d},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodefeature.MemoryMibToUnits(tc.memMib, h100VRAM)
			if got != tc.want {
				t.Errorf("MemoryMibToUnits(%d, %d) = %d, want %d", tc.memMib, h100VRAM, got, tc.want)
			}
		})
	}
}
