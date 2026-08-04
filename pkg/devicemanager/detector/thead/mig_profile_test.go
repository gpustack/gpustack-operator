package thead

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/binding/hgml"
	"gpustack.ai/gpustack/pkg/device"
)

// profileName packs a Go string into the NUL-terminated C char array the vendor library
// reports a profile name in.
func profileName(s string) [96]int8 {
	var a [96]int8
	for i := 0; i < len(s) && i < len(a)-1; i++ {
		a[i] = int8(s[i])
	}
	return a
}

// placementsFrom builds a profile's legal empty-card placement set: count slots of the
// given span, laid end to end from slot zero, which is the layout a card partitioned into
// whole memory slices offers.
func placementsFrom(span, count int32) []device.AcceleratorPhysicalPlacement {
	out := make([]device.AcceleratorPhysicalPlacement, 0, count)
	for i := int32(0); i < count; i++ {
		out = append(out, device.AcceleratorPhysicalPlacement{Start: i * span, Length: span})
	}
	return out
}

// zw810eProfiles is the profile set of the product this vendor currently ships (96 GiB over
// eight memory slices). Its shape — the "<compute>c.<memory>g" naming, the halving instance
// counts and the end-to-end placements — is derived from the card's geometry and from the
// vendor's CLI documentation; the raw ids and the exact memory sizes await confirmation
// against a real host, which is why nothing here derives meaning from an id.
func zw810eProfiles() []hgml.GpuInstanceProfileInfo_v3 {
	return []hgml.GpuInstanceProfileInfo_v3{
		{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("1c.12g")},
		{Id: 5, SliceCount: 2, InstanceCount: 4, MemorySizeMB: 24576, Name: profileName("2c.24g")},
		{Id: 6, SliceCount: 4, InstanceCount: 2, MemorySizeMB: 49152, Name: profileName("4c.48g")},
		{Id: 9, SliceCount: 8, InstanceCount: 1, MemorySizeMB: 98304, Name: profileName("8c.96g")},
	}
}

func zw810ePlacements() map[uint32][]device.AcceleratorPhysicalPlacement {
	return map[uint32][]device.AcceleratorPhysicalPlacement{
		3: placementsFrom(1, 8),
		5: placementsFrom(2, 4),
		6: placementsFrom(4, 2),
		9: placementsFrom(8, 1),
	}
}

func TestDeriveSlicedProfiles(t *testing.T) {
	const zw810eMemoryMib = 98304

	testCases := []struct {
		name           string
		infos          []hgml.GpuInstanceProfileInfo_v3
		cardMemoryMiB  uint64
		placementsByID map[uint32][]device.AcceleratorPhysicalPlacement
		want           []device.AcceleratorPhysicalSlicedProfile
		wantRejected   int
	}{
		{
			// The documented set of the current product yields one profile per record,
			// in probe order, each spanning the memory slices its placements state.
			name:           "current product profile set",
			cardMemoryMiB:  zw810eMemoryMib,
			infos:          zw810eProfiles(),
			placementsByID: zw810ePlacements(),
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "1c.12g", MemoryMib: 12288, ComputeSlices: 1, MemorySlices: 1, Count: 8,
					Placements: placementsFrom(1, 8),
				},
				{
					Name: "2c.24g", MemoryMib: 24576, ComputeSlices: 2, MemorySlices: 2, Count: 4,
					Placements: placementsFrom(2, 4),
				},
				{
					Name: "4c.48g", MemoryMib: 49152, ComputeSlices: 4, MemorySlices: 4, Count: 2,
					Placements: placementsFrom(4, 2),
				},
				{
					Name: "8c.96g", MemoryMib: 98304, ComputeSlices: 8, MemorySlices: 8, Count: 1,
					Placements: placementsFrom(8, 1),
				},
			},
		},
		{
			// A card whose driver offers no profile yields no profile, so its caller
			// reports no physical capability rather than an empty one.
			name:          "a driver offering no profile yields none",
			cardMemoryMiB: zw810eMemoryMib,
		},
		{
			// The vendor's display prefix, stray whitespace and upper case all reduce to
			// the bare geometry the resource key carries.
			name:          "names are normalized to the bare geometry",
			cardMemoryMiB: zw810eMemoryMib,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("PPU MIG 1C.12G")},
				{Id: 5, SliceCount: 2, InstanceCount: 4, MemorySizeMB: 24576, Name: profileName("  2c.24g\t")},
			},
			placementsByID: zw810ePlacements(),
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "1c.12g", MemoryMib: 12288, ComputeSlices: 1, MemorySlices: 1, Count: 8,
					Placements: placementsFrom(1, 8),
				},
				{
					Name: "2c.24g", MemoryMib: 24576, ComputeSlices: 2, MemorySlices: 2, Count: 4,
					Placements: placementsFrom(2, 4),
				},
			},
		},
		{
			// Media-engine and graphics variants are not offered, and are dropped before
			// normalization so they raise no rejection of their own.
			name:          "media and graphics variants are dropped silently",
			cardMemoryMiB: zw810eMemoryMib,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("1c.12g+me")},
				{Id: 4, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("1c.12g+me.all")},
				{Id: 7, SliceCount: 2, InstanceCount: 4, MemorySizeMB: 24576, Name: profileName("2c.24g+gfx")},
				{
					Id: 8, SliceCount: 4, InstanceCount: 2, MemorySizeMB: 49152, Name: profileName("4c.48g"),
					Capabilities: hgml.GPU_INSTANCE_PROFILE_CAPS_GFX,
				},
				{Id: 5, SliceCount: 2, InstanceCount: 4, MemorySizeMB: 24576, Name: profileName("2c.24g")},
			},
			placementsByID: zw810ePlacements(),
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "2c.24g", MemoryMib: 24576, ComputeSlices: 2, MemorySlices: 2, Count: 4,
					Placements: placementsFrom(2, 4),
				},
			},
		},
		{
			// A name that cannot form a resource-name segment makes the profile
			// unrequestable, so it is dropped with a reason rather than published broken.
			name:          "a name that cannot form a key is dropped with a reason",
			cardMemoryMiB: zw810eMemoryMib,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("1c/12g")},
				{Id: 5, SliceCount: 2, InstanceCount: 4, MemorySizeMB: 24576, Name: profileName("-2c.24g-")},
				// The key's name part is limited to 63 characters, of which the family and
				// kind segments already take twenty.
				{Id: 6, SliceCount: 4, InstanceCount: 2, MemorySizeMB: 49152, Name: profileName(strings.Repeat("a", 44))},
				{Id: 9, SliceCount: 8, InstanceCount: 1, MemorySizeMB: 98304, Name: profileName("8c.96g")},
			},
			placementsByID: zw810ePlacements(),
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "8c.96g", MemoryMib: 98304, ComputeSlices: 8, MemorySlices: 8, Count: 1,
					Placements: placementsFrom(8, 1),
				},
			},
			wantRejected: 3,
		},
		{
			// A nameless record is dropped: the published key is the profile's name, and
			// the driver seam resolves that key back to a raw id by matching names, so a
			// synthesized name could never match anything the driver reports.
			name:          "a nameless profile is dropped with a reason",
			cardMemoryMiB: zw810eMemoryMib,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288},
				{Id: 9, SliceCount: 8, InstanceCount: 1, MemorySizeMB: 98304, Name: profileName("8c.96g")},
			},
			placementsByID: zw810ePlacements(),
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "8c.96g", MemoryMib: 98304, ComputeSlices: 8, MemorySlices: 8, Count: 1,
					Placements: placementsFrom(8, 1),
				},
			},
			wantRejected: 1,
		},
		{
			// A driver that enumerates no placement leaves the memory division as the only
			// source of the span: 12288 MiB of a 98304 MiB eight-slice card is one slice.
			name:          "no enumerated placement keeps the divided span",
			cardMemoryMiB: zw810eMemoryMib,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("1c.12g")},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{Name: "1c.12g", MemoryMib: 12288, ComputeSlices: 1, MemorySlices: 1, Count: 8},
			},
		},
		{
			// The driver's placement is the authority: on a card divided into four memory
			// slices the eight-slice division would overstate the span as four.
			name:          "placement length overrides the division",
			cardMemoryMiB: 24576,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 5, SliceCount: 2, InstanceCount: 2, MemorySizeMB: 12288, Name: profileName("2c.12g")},
			},
			placementsByID: map[uint32][]device.AcceleratorPhysicalPlacement{5: placementsFrom(2, 2)},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "2c.12g", MemoryMib: 12288, ComputeSlices: 2, MemorySlices: 2, Count: 2,
					Placements: placementsFrom(2, 2),
				},
			},
		},
		{
			// A zero-length placement spans nothing, so it cannot be the published span.
			name:          "a zero-length placement keeps the divided span",
			cardMemoryMiB: zw810eMemoryMib,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 5, SliceCount: 2, InstanceCount: 4, MemorySizeMB: 24576, Name: profileName("2c.24g")},
			},
			placementsByID: map[uint32][]device.AcceleratorPhysicalPlacement{
				5: {{Start: 0, Length: 0}},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "2c.24g", MemoryMib: 24576, ComputeSlices: 2, MemorySlices: 2, Count: 4,
					Placements: []device.AcceleratorPhysicalPlacement{{Start: 0, Length: 0}},
				},
			},
		},
		{
			// Two raw names normalizing to one are the same profile when every published
			// field agrees, so one is kept and nothing is rejected.
			name:          "an agreeing normalization duplicate is de-duplicated",
			cardMemoryMiB: zw810eMemoryMib,
			infos: []hgml.GpuInstanceProfileInfo_v3{
				{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("1c.12g")},
				{Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("MIG 1C.12G")},
			},
			placementsByID: zw810ePlacements(),
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "1c.12g", MemoryMib: 12288, ComputeSlices: 1, MemorySlices: 1, Count: 8,
					Placements: placementsFrom(1, 8),
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, rejected := deriveSlicedProfiles(tc.infos, tc.cardMemoryMiB, func(
				id uint32,
			) []device.AcceleratorPhysicalPlacement {
				return tc.placementsByID[id]
			})
			assert.Equal(t, tc.want, got)
			assert.Len(t, rejected, tc.wantRejected)
		})
	}
}

// TestDeriveSlicedProfilesRejectsCollisions asserts a normalization collision is refused
// rather than aggregated: the shared group aggregation merges profiles by name, sums their
// counts and keeps the first one's memory, so publishing two disagreeing profiles under one
// name would silently misstate the pool's capacity and its Kueue credits.
func TestDeriveSlicedProfilesRejectsCollisions(t *testing.T) {
	const cardMemoryMiB = 98304

	base := hgml.GpuInstanceProfileInfo_v3{
		Id: 3, SliceCount: 1, InstanceCount: 8, MemorySizeMB: 12288, Name: profileName("1c.12g"),
	}
	keeper := hgml.GpuInstanceProfileInfo_v3{
		Id: 9, SliceCount: 8, InstanceCount: 1, MemorySizeMB: 98304, Name: profileName("8c.96g"),
	}

	testCases := []struct {
		name    string
		collide func(info *hgml.GpuInstanceProfileInfo_v3)
	}{
		{"differing memory", func(i *hgml.GpuInstanceProfileInfo_v3) { i.MemorySizeMB = 24576 }},
		{"differing compute slices", func(i *hgml.GpuInstanceProfileInfo_v3) { i.SliceCount = 2 }},
		{"differing instance count", func(i *hgml.GpuInstanceProfileInfo_v3) { i.InstanceCount = 4 }},
		// A differing raw id resolves a different placement set, so both the span and the
		// legal slots disagree under one name.
		{"differing placements and span", func(i *hgml.GpuInstanceProfileInfo_v3) { i.Id = 5 }},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			collided := base
			collided.Name = profileName("MIG 1C.12G")
			tc.collide(&collided)

			got, rejected := deriveSlicedProfiles(
				[]hgml.GpuInstanceProfileInfo_v3{base, collided, keeper},
				cardMemoryMiB,
				func(id uint32) []device.AcceleratorPhysicalPlacement {
					return zw810ePlacements()[id]
				})

			// The colliding name is withheld entirely — neither reading of it is
			// trustworthy — while every other profile the card offers is unaffected.
			require.Len(t, got, 1)
			assert.Equal(t, "8c.96g", got[0].Name)
			assert.Len(t, rejected, 1)
			assert.Contains(t, rejected[0], "1c.12g")
		})
	}
}

// TestDeriveSlicedProfilesSpanMatchesPlacements pins the memory-slice span's redundancy:
// it is the length of every legal placement of its profile, so the published field must
// agree with the set it was derived from rather than be trusted on its own.
func TestDeriveSlicedProfilesSpanMatchesPlacements(t *testing.T) {
	got, rejected := deriveSlicedProfiles(zw810eProfiles(), 98304, func(
		id uint32,
	) []device.AcceleratorPhysicalPlacement {
		return zw810ePlacements()[id]
	})
	require.Empty(t, rejected)
	require.NotEmpty(t, got)

	for _, p := range got {
		require.NotEmpty(t, p.Placements, "%s must carry the placements it was derived from", p.Name)
		assert.Equal(t, p.Placements[0].Length, p.MemorySlices,
			"%s span must be its first placement's length", p.Name)
		for _, pl := range p.Placements {
			assert.Equal(t, p.MemorySlices, pl.Length,
				"%s placement %+v must be as long as the published span", p.Name, pl)
		}
	}
}

func TestMigPlacementsByProfile(t *testing.T) {
	const failing = uint32(6)

	testCases := []struct {
		name         string
		infos        []hgml.GpuInstanceProfileInfo_v3
		slots        map[uint32][]hgml.GpuInstancePlacement
		wantAnswered []uint32
		want         map[uint32][]device.AcceleratorPhysicalPlacement
		wantErr      bool
	}{
		{
			name:  "enumerated placements are converted and keyed by profile id",
			infos: []hgml.GpuInstanceProfileInfo_v3{{Id: 3}, {Id: 5}},
			slots: map[uint32][]hgml.GpuInstancePlacement{
				3: {{Start: 0, Size: 1}, {Start: 1, Size: 1}},
				5: {{Start: 0, Size: 2}},
			},
			wantAnswered: []uint32{3, 5},
			want: map[uint32][]device.AcceleratorPhysicalPlacement{
				3: {{Start: 0, Length: 1}, {Start: 1, Length: 1}},
				5: {{Start: 0, Length: 2}},
			},
		},
		{
			// The driver answered, with nothing: the profile stays publishable and its
			// span falls back to the memory division.
			name:         "an enumerated-none profile is kept with a nil placement set",
			infos:        []hgml.GpuInstanceProfileInfo_v3{{Id: 3}},
			wantAnswered: []uint32{3},
			want:         map[uint32][]device.AcceleratorPhysicalPlacement{3: nil},
		},
		{
			// The driver failed to answer: publishing a span from unverified geometry
			// would make an unreadable card indistinguishable from a placement-free one.
			name:         "a failed query withholds the profile and errors",
			infos:        []hgml.GpuInstanceProfileInfo_v3{{Id: failing}},
			wantAnswered: []uint32{},
			want:         map[uint32][]device.AcceleratorPhysicalPlacement{},
			wantErr:      true,
		},
		{
			name:  "a failed query does not hide the profiles that answered",
			infos: []hgml.GpuInstanceProfileInfo_v3{{Id: 3}, {Id: failing}},
			slots: map[uint32][]hgml.GpuInstancePlacement{
				3: {{Start: 0, Size: 1}},
			},
			wantAnswered: []uint32{3},
			want:         map[uint32][]device.AcceleratorPhysicalPlacement{3: {{Start: 0, Length: 1}}},
			wantErr:      true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			answered, got, err := migPlacementsByProfile(
				tc.infos,
				func(id uint32) ([]hgml.GpuInstancePlacement, hgml.Return) {
					if id == failing {
						return nil, hgml.ERROR_NOT_SUPPORTED
					}
					return tc.slots[id], hgml.SUCCESS
				})
			assert.Equal(t, tc.wantErr, err != nil, "error = %v", err)

			gotAnswered := make([]uint32, 0, len(answered))
			for _, info := range answered {
				gotAnswered = append(gotAnswered, info.Id)
			}
			assert.Equal(t, tc.wantAnswered, gotAnswered)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMigPlacementsFromHGML(t *testing.T) {
	got := migPlacementsFromHGML([]hgml.GpuInstancePlacement{{Start: 0, Size: 2}, {Start: 2, Size: 2}})
	assert.Equal(t, []device.AcceleratorPhysicalPlacement{{Start: 0, Length: 2}, {Start: 2, Length: 2}}, got)
	assert.Nil(t, migPlacementsFromHGML(nil))
}

func TestMaxProfileCount(t *testing.T) {
	profiles := []device.AcceleratorPhysicalSlicedProfile{
		{Name: "1c.12g", Count: 8}, {Name: "2c.24g", Count: 4}, {Name: "8c.96g", Count: 1},
	}
	assert.Equal(t, int32(8), maxProfileCount(profiles))
	assert.Zero(t, maxProfileCount(nil))
}

// TestPhysicalSliced asserts a mode-enabled card whose driver offers no profile reports no
// physical capability rather than an empty one, and that a card with profiles carries the
// ceiling they imply.
func TestPhysicalSliced(t *testing.T) {
	assert.Equal(t, device.AcceleratorPhysicalSliced{}, physicalSliced(nil))
	assert.Equal(t, device.AcceleratorPhysicalSliced{},
		physicalSliced([]device.AcceleratorPhysicalSlicedProfile{}))

	profiles := []device.AcceleratorPhysicalSlicedProfile{
		{Name: "1c.12g", Count: 8}, {Name: "8c.96g", Count: 1},
	}
	assert.Equal(t,
		device.AcceleratorPhysicalSliced{Profiles: profiles, Count: 8},
		physicalSliced(profiles))
}

// physicalCard builds one detected card carrying the given physical-slice profiles, with
// the ceiling the card would have been detected with.
func physicalCard(id string, profiles ...device.AcceleratorPhysicalSlicedProfile) device.Accelerator {
	return device.Accelerator{
		ID: id,
		Status: device.AcceleratorStatus{
			PhysicalSliced: device.AcceleratorPhysicalSliced{
				Profiles: profiles,
				Count:    maxProfileCount(profiles),
			},
		},
	}
}

func TestRejectDivergentGroupProfiles(t *testing.T) {
	narrow := device.AcceleratorPhysicalSlicedProfile{
		Name: "1c.12g", MemoryMib: 12288, ComputeSlices: 1, MemorySlices: 1, Count: 8,
		Placements: placementsFrom(1, 8),
	}
	whole := device.AcceleratorPhysicalSlicedProfile{
		Name: "8c.96g", MemoryMib: 98304, ComputeSlices: 8, MemorySlices: 8, Count: 1,
		Placements: placementsFrom(8, 1),
	}

	divergeMemory := narrow
	divergeMemory.MemoryMib = 24576
	divergeCount := narrow
	divergeCount.Count = 4
	divergeGeometry := narrow
	divergeGeometry.ComputeSlices = 2
	divergePlacements := narrow
	divergePlacements.Placements = placementsFrom(1, 4)

	testCases := []struct {
		name         string
		cards        []device.Accelerator
		wantProfiles [][]string
		wantCounts   []int32
		wantRejected int
	}{
		{
			// Cards of one group agreeing on a profile keep it, so the aggregation may
			// sum their per-card counts.
			name:         "agreeing cards keep every profile",
			cards:        []device.Accelerator{physicalCard("a", narrow, whole), physicalCard("b", narrow, whole)},
			wantProfiles: [][]string{{"1c.12g", "8c.96g"}, {"1c.12g", "8c.96g"}},
			wantCounts:   []int32{8, 8},
		},
		{
			name:         "differing memory under one name is rejected on every card",
			cards:        []device.Accelerator{physicalCard("a", narrow, whole), physicalCard("b", divergeMemory, whole)},
			wantProfiles: [][]string{{"8c.96g"}, {"8c.96g"}},
			wantCounts:   []int32{1, 1},
			wantRejected: 1,
		},
		{
			name:         "differing per-card count under one name is rejected",
			cards:        []device.Accelerator{physicalCard("a", narrow, whole), physicalCard("b", divergeCount, whole)},
			wantProfiles: [][]string{{"8c.96g"}, {"8c.96g"}},
			wantCounts:   []int32{1, 1},
			wantRejected: 1,
		},
		{
			name:         "differing geometry under one name is rejected",
			cards:        []device.Accelerator{physicalCard("a", narrow, whole), physicalCard("b", divergeGeometry, whole)},
			wantProfiles: [][]string{{"8c.96g"}, {"8c.96g"}},
			wantCounts:   []int32{1, 1},
			wantRejected: 1,
		},
		{
			name: "differing placements under one name is rejected",
			cards: []device.Accelerator{
				physicalCard("a", narrow, whole), physicalCard("b", divergePlacements, whole),
			},
			wantProfiles: [][]string{{"8c.96g"}, {"8c.96g"}},
			wantCounts:   []int32{1, 1},
			wantRejected: 1,
		},
		{
			// A card left with nothing reports no physical capability rather than an
			// empty one, so its ceiling cannot outlive the profile it came from.
			name:         "a card stripped of every profile reports no capability",
			cards:        []device.Accelerator{physicalCard("a", narrow), physicalCard("b", divergeMemory)},
			wantProfiles: [][]string{nil, nil},
			wantCounts:   []int32{0, 0},
			wantRejected: 1,
		},
		{
			// A single card cannot disagree with itself, so nothing is stripped.
			name:         "a lone card is left alone",
			cards:        []device.Accelerator{physicalCard("a", narrow, whole)},
			wantProfiles: [][]string{{"1c.12g", "8c.96g"}},
			wantCounts:   []int32{8},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			group := device.DevicesGroup{Accelerators: tc.cards}
			rejected := rejectDivergentGroupProfiles(&group)

			assert.Len(t, rejected, tc.wantRejected)
			require.Len(t, group.Accelerators, len(tc.wantProfiles))
			for i, card := range group.Accelerators {
				names := make([]string, 0, len(card.Status.PhysicalSliced.Profiles))
				for _, p := range card.Status.PhysicalSliced.Profiles {
					names = append(names, p.Name)
				}
				if tc.wantProfiles[i] == nil {
					assert.Empty(t, names, "card %s", card.ID)
				} else {
					assert.Equal(t, tc.wantProfiles[i], names, "card %s", card.ID)
				}
				assert.Equal(t, tc.wantCounts[i], card.Status.PhysicalSliced.Count, "card %s", card.ID)
			}
		})
	}
}
