package nvidia

import (
	"reflect"
	"strings"
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
		name  string
		infos []nvml.GpuInstanceProfileInfo_v3
		// spans gives each profile id the memory-slice span its enumerated placements
		// report. An id absent from the map is one the driver placed nowhere, which is
		// unpublishable: the span is read from the placement set and has no other source.
		spans           map[uint32]int32
		want            []device.AcceleratorPhysicalSlicedProfile
		wantErrContains []string
	}{
		{
			// The full A100-40GB probe set (ids 0-4 + REV1 id 7 supported); asserts the
			// documented six-row table, with 1g.10gb (id 7) kept and memory rounded to
			// whole slices (4864/5120 = 0.95 rounds up to 1).
			name:  "a100-40gb full profile set",
			spans: map[uint32]int32{0: 1, 1: 2, 2: 4, 3: 4, 4: 8, 7: 2},
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864, Name: profileName("1g.5gb")},
				{Id: 1, SliceCount: 2, InstanceCount: 3, MemorySizeMB: 9856, Name: profileName("2g.10gb")},
				{Id: 2, SliceCount: 3, InstanceCount: 2, MemorySizeMB: 19968, Name: profileName("3g.20gb")},
				{Id: 3, SliceCount: 4, InstanceCount: 1, MemorySizeMB: 19968, Name: profileName("4g.20gb")},
				{Id: 4, SliceCount: 7, InstanceCount: 1, MemorySizeMB: 40192, Name: profileName("7g.40gb")},
				{Id: 7, SliceCount: 1, InstanceCount: 4, MemorySizeMB: 9856, Name: profileName("1g.10gb")},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{Name: "1g.5gb", MemoryMib: 4864, ComputeSlices: 1, MemorySlices: 1, Count: 7, Placements: onePlacement(1)},
				{Name: "2g.10gb", MemoryMib: 9856, ComputeSlices: 2, MemorySlices: 2, Count: 3, Placements: onePlacement(2)},
				{Name: "3g.20gb", MemoryMib: 19968, ComputeSlices: 3, MemorySlices: 4, Count: 2, Placements: onePlacement(4)},
				{Name: "4g.20gb", MemoryMib: 19968, ComputeSlices: 4, MemorySlices: 4, Count: 1, Placements: onePlacement(4)},
				{Name: "7g.40gb", MemoryMib: 40192, ComputeSlices: 7, MemorySlices: 8, Count: 1, Placements: onePlacement(8)},
				{Name: "1g.10gb", MemoryMib: 9856, ComputeSlices: 1, MemorySlices: 2, Count: 4, Placements: onePlacement(2)},
			},
		},
		{
			// An H100-shaped set where the base ids are the +me variants and the NO_ME id
			// is the plain keeper — proving the filter is Name-based, not id-based. The
			// +me / +me.all / +gfx variants and a GFX-capability-bit profile are all
			// dropped, and the +me/plain pair for "1g.10gb" must not double-count.
			name:  "h100 me/gfx variants dropped, plain kept",
			spans: map[uint32]int32{1: 2, 13: 1},
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
				{Name: "2g.20gb", MemoryMib: 19968, ComputeSlices: 2, MemorySlices: 2, Count: 3, Placements: onePlacement(2)},
				{Name: "1g.10gb", MemoryMib: 9856, ComputeSlices: 1, MemorySlices: 1, Count: 7, Placements: onePlacement(1)},
			},
		},
		{
			// V1 path (no Name): nothing is publishable, because a name the driver never
			// reported is a resource key the allocator's name probe can never resolve back
			// to a profile id. Every dropped id is named in the error.
			name:  "a driver that names nothing publishes nothing",
			spans: map[uint32]int32{0: 1, 4: 8, 10: 1},
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864},
				{Id: 4, SliceCount: 7, InstanceCount: 1, MemorySizeMB: 40192},
				{Id: 10, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864},
			},
			want:            nil,
			wantErrContains: []string{"profile 0", "profile 4", "profile 10", "no name"},
		},
		{
			// A driver naming only some ids: the named profiles are published untouched and
			// only the nameless one is dropped, so one unnamed id costs no other capacity.
			name:  "named profiles survive a nameless sibling intact",
			spans: map[uint32]int32{0: 1, 1: 2, 4: 8},
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864, Name: profileName("1g.5gb")},
				{Id: 1, SliceCount: 2, InstanceCount: 3, MemorySizeMB: 9856},
				{Id: 4, SliceCount: 7, InstanceCount: 1, MemorySizeMB: 40192, Name: profileName("7g.40gb")},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{Name: "1g.5gb", MemoryMib: 4864, ComputeSlices: 1, MemorySlices: 1, Count: 7, Placements: onePlacement(1)},
				{Name: "7g.40gb", MemoryMib: 40192, ComputeSlices: 7, MemorySlices: 8, Count: 1, Placements: onePlacement(8)},
			},
			wantErrContains: []string{"profile 1", "no name"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveSlicedProfiles(tc.infos, placementsFromSpans(tc.spans))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deriveSlicedProfiles() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
			assertErrContains(t, err, tc.wantErrContains)
		})
	}
}

// onePlacement is a profile's placement set as a single slot of the given span — the shape a
// case wants when what it asserts is the derivation rather than the placement geometry.
func onePlacement(span int32) []device.AcceleratorPhysicalPlacement {
	return []device.AcceleratorPhysicalPlacement{{Start: 0, Length: span}}
}

// placementsFromSpans resolves each profile id to a single placement of its declared span, and
// to no placement at all for an id the case left out — the driver-placed-nowhere case the
// derivation refuses.
func placementsFromSpans(spans map[uint32]int32) func(uint32) []device.AcceleratorPhysicalPlacement {
	return func(id uint32) []device.AcceleratorPhysicalPlacement {
		span, ok := spans[id]
		if !ok {
			return nil
		}
		return onePlacement(span)
	}
}

// assertErrContains asserts that err mentions every want substring, and that err is nil
// when no substring is wanted — so a case expecting no rejection asserts it.
func assertErrContains(t *testing.T, err error, want []string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("got no error, want one mentioning %v", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %q", err, w)
		}
	}
}

func TestMaxProfileCount(t *testing.T) {
	// The A100-40GB ceiling is 7 (from 7x 1g.5gb).
	a100 := []device.AcceleratorPhysicalSlicedProfile{
		{Name: "1g.5gb", Count: 7}, {Name: "2g.10gb", Count: 3}, {Name: "7g.40gb", Count: 1},
	}
	if got := maxProfileCount(a100); got != 7 {
		t.Errorf("maxProfileCount(a100) = %d, want 7", got)
	}
	if got := maxProfileCount(nil); got != 0 {
		t.Errorf("maxProfileCount(nil) = %d, want 0", got)
	}
}

func TestDeriveSlicedProfilesCachesPlacementsByID(t *testing.T) {
	// 1g.5gb (id 0) and 1g.10gb (id 7) both span one compute slice; the placement cache
	// must key on the profile's own probed id, not the shared slice count, so each keeps
	// its own legal slots.
	placementsByID := map[uint32][]device.AcceleratorPhysicalPlacement{
		0: {{Start: 0, Length: 1}, {Start: 1, Length: 1}},
		7: {{Start: 0, Length: 2}, {Start: 2, Length: 2}},
	}
	infos := []nvml.GpuInstanceProfileInfo_v3{
		{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864, Name: profileName("1g.5gb")},
		{Id: 7, SliceCount: 1, InstanceCount: 4, MemorySizeMB: 9856, Name: profileName("1g.10gb")},
	}
	got, err := deriveSlicedProfiles(infos, func(id uint32) []device.AcceleratorPhysicalPlacement {
		return placementsByID[id]
	})
	if err != nil {
		t.Fatalf("deriveSlicedProfiles() unexpected error: %v", err)
	}

	want := map[string][]device.AcceleratorPhysicalPlacement{
		"1g.5gb":  {{Start: 0, Length: 1}, {Start: 1, Length: 1}},
		"1g.10gb": {{Start: 0, Length: 2}, {Start: 2, Length: 2}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d profiles, want %d", len(got), len(want))
	}
	for i := range got {
		p := got[i]
		if !reflect.DeepEqual(p.Placements, want[p.Name]) {
			t.Errorf("%s placements = %+v, want %+v", p.Name, p.Placements, want[p.Name])
		}
	}
}

func TestDeriveSlicedProfilesSpanFromPlacements(t *testing.T) {
	testCases := []struct {
		name              string
		infos             []nvml.GpuInstanceProfileInfo_v3
		placementsByID    map[uint32][]device.AcceleratorPhysicalPlacement
		want              []device.AcceleratorPhysicalSlicedProfile
		wantErrContains   []string
		spanFromEverySlot bool
	}{
		{
			// A card partitioned into four memory slices, where dividing by eight understates
			// each slice and so overstates the span (4 instead of 2). The driver's placements
			// are the authority.
			name: "placement length overrides the eight-slice division",
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 1, SliceCount: 2, InstanceCount: 2, MemorySizeMB: 11776, Name: profileName("2g.12gb")},
			},
			placementsByID: map[uint32][]device.AcceleratorPhysicalPlacement{
				1: {{Start: 0, Length: 2}, {Start: 2, Length: 2}},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "2g.12gb", MemoryMib: 11776, ComputeSlices: 2, MemorySlices: 2, Count: 2,
					Placements: []device.AcceleratorPhysicalPlacement{{Start: 0, Length: 2}, {Start: 2, Length: 2}},
				},
			},
			spanFromEverySlot: true,
		},
		{
			// Every legal placement of a profile is as long as the profile's span, so the
			// span read off the first slot holds for all of them.
			name: "every placement of a profile carries the same span",
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 9856, Name: profileName("1g.10gb")},
			},
			placementsByID: map[uint32][]device.AcceleratorPhysicalPlacement{
				0: {{Start: 0, Length: 2}, {Start: 2, Length: 2}, {Start: 4, Length: 2}, {Start: 6, Length: 2}},
			},
			want: []device.AcceleratorPhysicalSlicedProfile{
				{
					Name: "1g.10gb", MemoryMib: 9856, ComputeSlices: 1, MemorySlices: 2, Count: 7,
					Placements: []device.AcceleratorPhysicalPlacement{
						{Start: 0, Length: 2}, {Start: 2, Length: 2}, {Start: 4, Length: 2}, {Start: 6, Length: 2},
					},
				},
			},
			spanFromEverySlot: true,
		},
		{
			// The placement set is the span's only source, so a profile the driver placed
			// nowhere cannot be published: the span is what the allocator matches a leftover
			// instance's identity by, and a computed one would be a guess there. Nothing is
			// forfeited — the ledger is placement-derived, so such a profile could never have
			// been allocated.
			name: "a profile with no enumerated placement is dropped",
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 2, SliceCount: 3, InstanceCount: 2, MemorySizeMB: 19968, Name: profileName("3g.20gb")},
			},
			placementsByID:  map[uint32][]device.AcceleratorPhysicalPlacement{},
			want:            nil,
			wantErrContains: []string{"profile 2", "3g.20gb", "no legal placement"},
		},
		{
			// A placement of zero length spans nothing, so it cannot be the published span
			// either — and there is nothing else to fall back to.
			name: "a zero-length placement is dropped",
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 2, SliceCount: 3, InstanceCount: 2, MemorySizeMB: 19968, Name: profileName("3g.20gb")},
			},
			placementsByID: map[uint32][]device.AcceleratorPhysicalPlacement{
				2: {{Start: 0, Length: 0}},
			},
			want:            nil,
			wantErrContains: []string{"profile 2", "no legal placement"},
		},
		{
			// A nameless profile is dropped however well the driver placed it: a placement
			// set cannot supply the name the allocator has to match.
			name: "a nameless profile is dropped despite enumerated placements",
			infos: []nvml.GpuInstanceProfileInfo_v3{
				{Id: 0, SliceCount: 1, InstanceCount: 7, MemorySizeMB: 4864},
			},
			placementsByID: map[uint32][]device.AcceleratorPhysicalPlacement{
				0: {{Start: 0, Length: 2}, {Start: 2, Length: 2}},
			},
			want:            nil,
			wantErrContains: []string{"profile 0", "no name"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveSlicedProfiles(tc.infos, func(id uint32) []device.AcceleratorPhysicalPlacement {
				return tc.placementsByID[id]
			})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deriveSlicedProfiles() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
			assertErrContains(t, err, tc.wantErrContains)
			if !tc.spanFromEverySlot {
				return
			}
			for _, p := range got {
				for _, pl := range p.Placements {
					if pl.Length != p.MemorySlices {
						t.Errorf("%s placement %+v length != published span %d", p.Name, pl, p.MemorySlices)
					}
				}
			}
		})
	}
}

func TestMigPlacementsByProfile(t *testing.T) {
	const failing = uint32(3)

	testCases := []struct {
		name         string
		infos        []nvml.GpuInstanceProfileInfo_v3
		slots        map[uint32][]nvml.GpuInstancePlacement
		wantAnswered []uint32
		want         map[uint32][]device.AcceleratorPhysicalPlacement
		wantErr      bool
	}{
		{
			name:  "enumerated placements are converted and keyed by profile id",
			infos: []nvml.GpuInstanceProfileInfo_v3{{Id: 0}, {Id: 7}},
			slots: map[uint32][]nvml.GpuInstancePlacement{
				0: {{Start: 0, Size: 1}, {Start: 1, Size: 1}},
				7: {{Start: 0, Size: 2}},
			},
			wantAnswered: []uint32{0, 7},
			want: map[uint32][]device.AcceleratorPhysicalPlacement{
				0: {{Start: 0, Length: 1}, {Start: 1, Length: 1}},
				7: {{Start: 0, Length: 2}},
			},
		},
		{
			// The driver answered, with nothing: the profile stays publishable and the
			// derivation falls back to arithmetic for its span.
			name:         "an enumerated-none profile is kept with a nil placement set",
			infos:        []nvml.GpuInstanceProfileInfo_v3{{Id: 0}},
			wantAnswered: []uint32{0},
			want:         map[uint32][]device.AcceleratorPhysicalPlacement{0: nil},
		},
		{
			// The driver failed to answer: the profile is withheld and the failure reported.
			name:         "a failed query withholds the profile and errors",
			infos:        []nvml.GpuInstanceProfileInfo_v3{{Id: failing}},
			wantAnswered: []uint32{},
			want:         map[uint32][]device.AcceleratorPhysicalPlacement{},
			wantErr:      true,
		},
		{
			name:  "a failed query does not hide the profiles that answered",
			infos: []nvml.GpuInstanceProfileInfo_v3{{Id: 0}, {Id: failing}},
			slots: map[uint32][]nvml.GpuInstancePlacement{
				0: {{Start: 0, Size: 1}},
			},
			wantAnswered: []uint32{0},
			want:         map[uint32][]device.AcceleratorPhysicalPlacement{0: {{Start: 0, Length: 1}}},
			wantErr:      true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			answered, got, err := migPlacementsByProfile(
				tc.infos,
				func(id uint32) ([]nvml.GpuInstancePlacement, nvml.Return) {
					if id == failing {
						return nil, nvml.ERROR_NOT_SUPPORTED
					}
					return tc.slots[id], nvml.SUCCESS
				})
			if (err != nil) != tc.wantErr {
				t.Errorf("migPlacementsByProfile() error = %v, wantErr %v", err, tc.wantErr)
			}
			gotAnswered := make([]uint32, 0, len(answered))
			for _, info := range answered {
				gotAnswered = append(gotAnswered, info.Id)
			}
			if !reflect.DeepEqual(gotAnswered, tc.wantAnswered) {
				t.Errorf("migPlacementsByProfile() answered = %v, want %v", gotAnswered, tc.wantAnswered)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("migPlacementsByProfile() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestMigPlacementsFromNVML(t *testing.T) {
	got := migPlacementsFromNVML([]nvml.GpuInstancePlacement{{Start: 0, Size: 2}, {Start: 4, Size: 2}})
	want := []device.AcceleratorPhysicalPlacement{{Start: 0, Length: 2}, {Start: 4, Length: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("migPlacementsFromNVML() = %+v, want %+v", got, want)
	}
	if migPlacementsFromNVML(nil) != nil {
		t.Errorf("migPlacementsFromNVML(nil) = non-nil, want nil")
	}
}
