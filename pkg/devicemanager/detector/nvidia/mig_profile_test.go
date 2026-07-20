package nvidia

import (
	"reflect"
	"testing"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
)

// profileName packs a Go string into the NUL-terminated C char array NVML uses.
func profileName(s string) [96]int8 {
	var a [96]int8
	for i := 0; i < len(s) && i < len(a)-1; i++ {
		a[i] = int8(s[i])
	}
	return a
}

func TestDeriveSlicedProfiles(t *testing.T) {
	testCases := []struct {
		name          string
		infos         []nvml.GpuInstanceProfileInfo_v3
		cardMemoryMiB uint64
		want          []device.AcceleratorPhysicalSlicedProfile
	}{
		{
			// The full A100-40GB probe set (ids 0-4 + REV1 id 7 supported); asserts the
			// documented six-row table, with 1g.10gb (id 7) kept and memory rounded to
			// whole slices (4864/5120 = 0.95 rounds up to 1).
			name:          "a100-40gb full profile set",
			cardMemoryMiB: 40960,
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864, Name: profileName("1g.5gb")},
				{Id: 1, SliceCount: 2, InstanceCount: 3, MemorySizeMB: 9856, Name: profileName("2g.10gb")},
				{Id: 2, SliceCount: 3, InstanceCount: 2, MemorySizeMB: 19968, Name: profileName("3g.20gb")},
				{Id: 3, SliceCount: 4, InstanceCount: 1, MemorySizeMB: 19968, Name: profileName("4g.20gb")},
				{Id: 4, SliceCount: 7, InstanceCount: 1, MemorySizeMB: 40192, Name: profileName("7g.40gb")},
				{Id: 7, SliceCount: 1, InstanceCount: 4, MemorySizeMB: 9856, Name: profileName("1g.10gb")},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{Name: "1g.5gb", MemoryMib: 4864, ComputeSlices: 1, MemorySlices: 1, Count: 7},
				{Name: "2g.10gb", MemoryMib: 9856, ComputeSlices: 2, MemorySlices: 2, Count: 3},
				{Name: "3g.20gb", MemoryMib: 19968, ComputeSlices: 3, MemorySlices: 4, Count: 2},
				{Name: "4g.20gb", MemoryMib: 19968, ComputeSlices: 4, MemorySlices: 4, Count: 1},
				{Name: "7g.40gb", MemoryMib: 40192, ComputeSlices: 7, MemorySlices: 8, Count: 1},
				{Name: "1g.10gb", MemoryMib: 9856, ComputeSlices: 1, MemorySlices: 2, Count: 4},
			},
		},
		{
			// An H100-shaped set where the base ids are the +me variants and the NO_ME id
			// is the plain keeper — proving the filter is Name-based, not id-based. The
			// +me / +me.all / +gfx variants and a GFX-capability-bit profile are all
			// dropped, and the +me/plain pair for "1g.10gb" must not double-count.
			name:          "h100 me/gfx variants dropped, plain kept",
			cardMemoryMiB: 81920,
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 1, SliceCount: 2, InstanceCount: 3, MemorySizeMB: 19968, Name: profileName("2g.20gb")},
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 9856, Name: profileName("1g.10gb+me")},
				{Id: 10, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 9856, Name: profileName("1g.10gb+gfx")},
				{Id: 11, SliceCount: 2, InstanceCount: 3, MemorySizeMB: 19968, Name: profileName("2g.20gb+gfx")},
				{Id: 12, SliceCount: 4, InstanceCount: 1, MemorySizeMB: 39936, Name: profileName("4g.40gb"), Capabilities: nvml.GPU_INSTANCE_PROFILE_CAPS_GFX},
				{Id: 13, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 9856, Name: profileName("1g.10gb")},
				{Id: 15, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 9856, Name: profileName("1g.10gb+me.all")},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{Name: "2g.20gb", MemoryMib: 19968, ComputeSlices: 2, MemorySlices: 2, Count: 3},
				{Name: "1g.10gb", MemoryMib: 9856, ComputeSlices: 1, MemorySlices: 1, Count: 7},
			},
		},
		{
			// V1 path (no Name): ids 0-9 kept, 10-16 dropped, names derived from geometry.
			name:          "v1 fallback derives names from geometry",
			cardMemoryMiB: 40960,
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864},
				{Id: 4, SliceCount: 7, InstanceCount: 1, MemorySizeMB: 40192},
				{Id: 10, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{Name: "1g.5gb", MemoryMib: 4864, ComputeSlices: 1, MemorySlices: 1, Count: 7},
				{Name: "7g.40gb", MemoryMib: 40192, ComputeSlices: 7, MemorySlices: 8, Count: 1},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveSlicedProfiles(tc.infos, tc.cardMemoryMiB)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deriveSlicedProfiles() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}
