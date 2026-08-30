package hygon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding/dmi"
	"gpustack.ai/gpustack/pkg/device"
)

// profileInfo builds one probed GPU-instance profile. The name is a fixed-width int8 array in the
// generated type, so a helper is the only readable way to write test data for it.
func profileInfo(id uint32, name string, maxCount, cu, slices uint32, memMiB uint64) dmi.GpuInstanceProfileInfo {
	info := dmi.GpuInstanceProfileInfo{
		Id:              id,
		Gi_count_max:    maxCount,
		Cu_count:        cu,
		Gpu_slice_count: slices,
		Memory_size_MB:  memMiB,
	}
	for i, b := range []byte(name) {
		if i >= len(info.Name) {
			break
		}
		info.Name[i] = int8(b)
	}
	return info
}

func placements(spans ...[2]int32) []device.AcceleratorPlacement {
	out := make([]device.AcceleratorPlacement, 0, len(spans))
	for _, s := range spans {
		out = append(out, device.AcceleratorPlacement{Start: s[0], Length: s[1]})
	}
	return out
}

// The three answers a profile probe can give have to stay separable. The sweep walks a fixed width
// space that every measured card has a hole in -- no card offers a three-slice profile -- so folding
// "this card has no profile that wide" into the error would report every healthy card as faulty,
// and folding a real failure into the gap would publish a card as offering less than it does with
// nothing said about why.
func TestProbeMigProfiles(t *testing.T) {
	twoG := profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380)
	fourG := profileInfo(1, "MIG 4g.31gb", 2, 40, 2, 32760)
	eightG := profileInfo(0, "MIG 8g.63gb", 1, 80, 4, 65520)

	testCases := []struct {
		name    string
		answers map[uint32]struct {
			info dmi.GpuInstanceProfileInfo
			ret  dmi.Return
		}
		wantIDs     []uint32
		wantErrText string
	}{
		{
			name: "the measured card: three widths answered, the three-slice one disclaimed",
			answers: map[uint32]struct {
				info dmi.GpuInstanceProfileInfo
				ret  dmi.Return
			}{
				1: {twoG, dmi.SUCCESS},
				2: {fourG, dmi.SUCCESS},
				3: {dmi.GpuInstanceProfileInfo{}, dmi.ERROR_INVALID_ARGUMENT},
				4: {eightG, dmi.SUCCESS},
			},
			wantIDs: []uint32{3, 1, 0},
		},
		{
			name: "a card that cannot be partitioned disclaims every width and is not an error",
			answers: map[uint32]struct {
				info dmi.GpuInstanceProfileInfo
				ret  dmi.Return
			}{
				1: {dmi.GpuInstanceProfileInfo{}, dmi.ERROR_NOT_SUPPORTED},
				2: {dmi.GpuInstanceProfileInfo{}, dmi.ERROR_NOT_SUPPORTED},
				3: {dmi.GpuInstanceProfileInfo{}, dmi.ERROR_NOT_SUPPORTED},
				4: {dmi.GpuInstanceProfileInfo{}, dmi.ERROR_NOT_SUPPORTED},
			},
			wantIDs: nil,
		},
		{
			name: "a width the driver could not answer for is reported, and the rest still publish",
			answers: map[uint32]struct {
				info dmi.GpuInstanceProfileInfo
				ret  dmi.Return
			}{
				1: {twoG, dmi.SUCCESS},
				2: {dmi.GpuInstanceProfileInfo{}, dmi.ERROR_NO_PERMISSION},
				3: {dmi.GpuInstanceProfileInfo{}, dmi.ERROR_INVALID_ARGUMENT},
				4: {eightG, dmi.SUCCESS},
			},
			wantIDs:     []uint32{3, 0},
			wantErrText: "2-slice profile",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			infos, err := probeMigProfiles(func(width uint32) (dmi.GpuInstanceProfileInfo, dmi.Return) {
				a := tc.answers[width]
				return a.info, a.ret
			})

			gotIDs := make([]uint32, 0, len(infos))
			for _, info := range infos {
				gotIDs = append(gotIDs, info.Id)
			}
			assert.Equal(t, tc.wantIDs, nilIfEmpty(gotIDs))

			if tc.wantErrText == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrText)
		})
	}
}

func nilIfEmpty(ids []uint32) []uint32 {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// On a node in Multi-Instance mode this is the only source for a card's compute-unit count: HSA
// exposes one instance per process there, so it reports the partition's 20 rather than the card's 80
// and answers for no other card at all. Each profile carries the card's count factored as its own
// units times the number of instances that fill the card.
func TestMigCardCores(t *testing.T) {
	testCases := []struct {
		name  string
		infos []dmi.GpuInstanceProfileInfo
		want  uint32
	}{
		{
			name: "the measured card, whose three profiles all factor to 80",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380),
				profileInfo(1, "MIG 4g.31gb", 2, 40, 2, 32760),
				profileInfo(0, "MIG 8g.63gb", 1, 80, 4, 65520),
			},
			want: 80,
		},
		{
			name: "one profile understating its maximum does not understate the card",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 1, 20, 1, 16380),
				profileInfo(0, "MIG 8g.63gb", 1, 80, 4, 65520),
			},
			want: 80,
		},
		{
			name: "a card with no probed profile reports nothing rather than zero compute",
			want: 0,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, migCardCores(tc.infos))
		})
	}
}

// A placement query that failed and one that answered with nothing must not collapse into each
// other: the first leaves the card's geometry unverified and the profile has to be withheld, the
// second is an answer the derivation can act on.
func TestMigPlacementsByProfile(t *testing.T) {
	twoG := profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380)
	fourG := profileInfo(1, "MIG 4g.31gb", 2, 40, 2, 32760)

	testCases := []struct {
		name    string
		answers map[uint32]struct {
			slots []dmi.GpuInstancePlacement
			ret   dmi.Return
		}
		wantAnswered []uint32
		wantByID     map[uint32][]device.AcceleratorPlacement
		wantErrText  string
	}{
		{
			name: "both answered",
			answers: map[uint32]struct {
				slots []dmi.GpuInstancePlacement
				ret   dmi.Return
			}{
				3: {[]dmi.GpuInstancePlacement{{Start: 0, Size: 1}, {Start: 1, Size: 1}}, dmi.SUCCESS},
				1: {[]dmi.GpuInstancePlacement{{Start: 0, Size: 2}}, dmi.SUCCESS},
			},
			wantAnswered: []uint32{3, 1},
			wantByID: map[uint32][]device.AcceleratorPlacement{
				3: placements([2]int32{0, 1}, [2]int32{1, 1}),
				1: placements([2]int32{0, 2}),
			},
		},
		{
			name: "a profile the driver placed nowhere is kept, with a nil set",
			answers: map[uint32]struct {
				slots []dmi.GpuInstancePlacement
				ret   dmi.Return
			}{
				3: {nil, dmi.SUCCESS},
				1: {[]dmi.GpuInstancePlacement{{Start: 0, Size: 2}}, dmi.SUCCESS},
			},
			wantAnswered: []uint32{3, 1},
			wantByID: map[uint32][]device.AcceleratorPlacement{
				3: nil,
				1: placements([2]int32{0, 2}),
			},
		},
		{
			name: "a profile whose query failed is withheld and named",
			answers: map[uint32]struct {
				slots []dmi.GpuInstancePlacement
				ret   dmi.Return
			}{
				3: {nil, dmi.ERROR_UNKNOWN},
				1: {[]dmi.GpuInstancePlacement{{Start: 0, Size: 2}}, dmi.SUCCESS},
			},
			wantAnswered: []uint32{1},
			wantByID: map[uint32][]device.AcceleratorPlacement{
				1: placements([2]int32{0, 2}),
			},
			wantErrText: "profile 3",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			answered, byID, err := migPlacementsByProfile(
				[]dmi.GpuInstanceProfileInfo{twoG, fourG},
				func(profileID uint32) ([]dmi.GpuInstancePlacement, dmi.Return) {
					a := tc.answers[profileID]
					return a.slots, a.ret
				})

			gotIDs := make([]uint32, 0, len(answered))
			for _, info := range answered {
				gotIDs = append(gotIDs, info.Id)
			}
			assert.Equal(t, tc.wantAnswered, gotIDs)
			assert.Equal(t, tc.wantByID, byID)

			if tc.wantErrText == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrText)
		})
	}
}

// What the derivation publishes, and what it refuses. Each refusal costs the card a requestable key,
// so each one is stated as a reason the caller can log rather than dropped silently.
func TestDeriveSlicedProfiles(t *testing.T) {
	testCases := []struct {
		name           string
		infos          []dmi.GpuInstanceProfileInfo
		placements     map[uint32][]device.AcceleratorPlacement
		want           []device.AcceleratorPhysicalSlicedProfile
		wantRejections []string
	}{
		{
			name: "the measured card publishes its three profiles",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380),
				profileInfo(1, "MIG 4g.31gb", 2, 40, 2, 32760),
				profileInfo(0, "MIG 8g.63gb", 1, 80, 4, 65520),
			},
			placements: map[uint32][]device.AcceleratorPlacement{
				3: placements([2]int32{0, 1}, [2]int32{1, 1}, [2]int32{2, 1}, [2]int32{3, 1}),
				1: placements([2]int32{0, 2}, [2]int32{2, 2}),
				0: placements([2]int32{0, 4}),
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "2g.15gb", MemoryMib: 16380, ComputeSlices: 1, MemorySlices: 1, Count: 4,
					Placements: placements([2]int32{0, 1}, [2]int32{1, 1}, [2]int32{2, 1}, [2]int32{3, 1}),
				},
				{
					Name: "4g.31gb", MemoryMib: 32760, ComputeSlices: 2, MemorySlices: 2, Count: 2,
					Placements: placements([2]int32{0, 2}, [2]int32{2, 2}),
				},
				{
					Name: "8g.63gb", MemoryMib: 65520, ComputeSlices: 4, MemorySlices: 4, Count: 1,
					Placements: placements([2]int32{0, 4}),
				},
			},
		},
		{
			name: "a nameless profile cannot be requested and is refused",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "", 4, 20, 1, 16380),
			},
			placements: map[uint32][]device.AcceleratorPlacement{
				3: placements([2]int32{0, 1}),
			},
			wantRejections: []string{"yields no valid resource name"},
		},
		{
			name: "a profile the driver placed nowhere has an unknown span and is refused",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380),
			},
			placements:     map[uint32][]device.AcceleratorPlacement{},
			wantRejections: []string{"has no legal placement"},
		},
		{
			name: "a placement of zero length is no span either",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380),
			},
			placements: map[uint32][]device.AcceleratorPlacement{
				3: placements([2]int32{0, 0}),
			},
			wantRejections: []string{"has no legal placement"},
		},
		{
			name: "two ids normalizing to one name with the same geometry collapse to one profile",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380),
				profileInfo(9, "mig 2G.15GB", 4, 20, 1, 16380),
			},
			placements: map[uint32][]device.AcceleratorPlacement{
				3: placements([2]int32{0, 1}),
				9: placements([2]int32{0, 1}),
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "2g.15gb", MemoryMib: 16380, ComputeSlices: 1, MemorySlices: 1, Count: 4,
					Placements: placements([2]int32{0, 1}),
				},
			},
		},
		{
			name: "two ids normalizing to one name with different memory withhold the name entirely",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380),
				profileInfo(9, "MIG 2g.15gb", 4, 20, 1, 8190),
			},
			placements: map[uint32][]device.AcceleratorPlacement{
				3: placements([2]int32{0, 1}),
				9: placements([2]int32{0, 1}),
			},
			wantRejections: []string{"normalize to \"2g.15gb\""},
		},
		{
			name: "a withheld name does not take an unrelated profile with it",
			infos: []dmi.GpuInstanceProfileInfo{
				profileInfo(3, "MIG 2g.15gb", 4, 20, 1, 16380),
				profileInfo(9, "MIG 2g.15gb", 4, 20, 1, 8190),
				profileInfo(0, "MIG 8g.63gb", 1, 80, 4, 65520),
			},
			placements: map[uint32][]device.AcceleratorPlacement{
				3: placements([2]int32{0, 1}),
				9: placements([2]int32{0, 1}),
				0: placements([2]int32{0, 4}),
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "8g.63gb", MemoryMib: 65520, ComputeSlices: 4, MemorySlices: 4, Count: 1,
					Placements: placements([2]int32{0, 4}),
				},
			},
			wantRejections: []string{"normalize to \"2g.15gb\""},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, rejected := deriveSlicedProfiles(tc.infos, func(id uint32) []device.AcceleratorPlacement {
				return tc.placements[id]
			})

			assert.Equal(t, tc.want, got)
			require.Len(t, rejected, len(tc.wantRejections))
			for i, want := range tc.wantRejections {
				assert.Contains(t, rejected[i], want)
			}
		})
	}
}

// An inventory with nothing in it describes a card that cannot be partitioned, and must not be
// published as a capability with an empty profile list behind it.
func TestPhysicalSliced(t *testing.T) {
	testCases := []struct {
		name     string
		profiles []device.AcceleratorPhysicalSlicedProfile
		want     device.AcceleratorPhysicalSliced
	}{
		{
			name: "no profiles is no capability",
			want: device.AcceleratorPhysicalSliced{},
		},
		{
			name: "the ceiling is the largest per-profile count",
			profiles: []device.AcceleratorPhysicalSlicedProfile{
				{Name: "8g.63gb", Count: 1},
				{Name: "2g.15gb", Count: 4},
				{Name: "4g.31gb", Count: 2},
			},
			want: device.AcceleratorPhysicalSliced{
				Profiles: []device.AcceleratorPhysicalSlicedProfile{
					{Name: "8g.63gb", Count: 1},
					{Name: "2g.15gb", Count: 4},
					{Name: "4g.31gb", Count: 2},
				},
				Count: 4,
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, physicalSliced(tc.profiles))
		})
	}
}

// The group aggregation downstream merges profiles by name, sums their counts and keeps the FIRST
// card's memory, so two cards disagreeing about one name would publish a figure describing neither.
// Withholding the name is what keeps the published capacity true.
func TestRejectDivergentGroupProfiles(t *testing.T) {
	small := device.AcceleratorPhysicalSlicedProfile{
		Name: "2g.15gb", MemoryMib: 16380, ComputeSlices: 1, MemorySlices: 1, Count: 4,
		Placements: placements([2]int32{0, 1}),
	}
	smallOtherMemory := small
	smallOtherMemory.MemoryMib = 8190
	big := device.AcceleratorPhysicalSlicedProfile{
		Name: "8g.63gb", MemoryMib: 65520, ComputeSlices: 4, MemorySlices: 4, Count: 1,
		Placements: placements([2]int32{0, 4}),
	}

	testCases := []struct {
		name           string
		cards          [][]device.AcceleratorPhysicalSlicedProfile
		wantKept       [][]device.AcceleratorPhysicalSlicedProfile
		wantCounts     []int32
		wantRejections int
	}{
		{
			name:       "cards that agree keep everything",
			cards:      [][]device.AcceleratorPhysicalSlicedProfile{{small, big}, {small, big}},
			wantKept:   [][]device.AcceleratorPhysicalSlicedProfile{{small, big}, {small, big}},
			wantCounts: []int32{4, 4},
		},
		{
			name:           "a name the cards disagree on is withheld from every card",
			cards:          [][]device.AcceleratorPhysicalSlicedProfile{{small, big}, {smallOtherMemory, big}},
			wantKept:       [][]device.AcceleratorPhysicalSlicedProfile{{big}, {big}},
			wantCounts:     []int32{1, 1},
			wantRejections: 1,
		},
		{
			name:           "a card left with nothing reports no capability rather than an empty one",
			cards:          [][]device.AcceleratorPhysicalSlicedProfile{{small}, {smallOtherMemory}},
			wantKept:       [][]device.AcceleratorPhysicalSlicedProfile{nil, nil},
			wantCounts:     []int32{0, 0},
			wantRejections: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			grp := device.DevicesGroup{}
			for _, profiles := range tc.cards {
				grp.Accelerators = append(grp.Accelerators, device.Accelerator{
					ID: "card",
					Status: device.AcceleratorStatus{
						PhysicalSliced: physicalSliced(profiles),
					},
				})
			}

			rejected := rejectDivergentGroupProfiles(&grp)

			assert.Len(t, rejected, tc.wantRejections)
			for i := range grp.Accelerators {
				assert.Equal(t, tc.wantKept[i], grp.Accelerators[i].Status.PhysicalSliced.Profiles)
				assert.Equal(t, tc.wantCounts[i], grp.Accelerators[i].Status.PhysicalSliced.Count)
			}
		})
	}
}
