package device

import (
	"reflect"
	"testing"
)

// a100PossiblePlacements is the hardcoded A100 legal placement set (start:length in
// memory-slice units) for the six kept profiles — the fixture the free-count math runs
// against.
var a100PossiblePlacements = map[string][]AcceleratorPhysicalPlacement{
	"1g.5gb":  {{Start: 0, Length: 1}, {Start: 1, Length: 1}, {Start: 2, Length: 1}, {Start: 3, Length: 1}, {Start: 4, Length: 1}, {Start: 5, Length: 1}, {Start: 6, Length: 1}},
	"1g.10gb": {{Start: 0, Length: 2}, {Start: 2, Length: 2}, {Start: 4, Length: 2}, {Start: 6, Length: 2}},
	"2g.10gb": {{Start: 0, Length: 2}, {Start: 2, Length: 2}, {Start: 4, Length: 2}},
	"3g.20gb": {{Start: 0, Length: 4}, {Start: 4, Length: 4}},
	"4g.20gb": {{Start: 0, Length: 4}},
	"7g.40gb": {{Start: 0, Length: 8}},
}

func TestComputeRemainingProfiles(t *testing.T) {
	testCases := []struct {
		name     string
		occupied []AcceleratorPhysicalPlacement
		possible map[string][]AcceleratorPhysicalPlacement
		want     map[string]int32
	}{
		{
			// The spec's worked example: a card already holding 1x3g.20gb@slot0
			// (occupies memory slices [0,4)) can still build the reduced set.
			name:     "a100 with one 3g.20gb at slot 0",
			occupied: []AcceleratorPhysicalPlacement{{Start: 0, Length: 4}},
			possible: a100PossiblePlacements,
			want:     map[string]int32{"1g.5gb": 3, "1g.10gb": 2, "2g.10gb": 1, "3g.20gb": 1},
		},
		{
			// An empty card can build every profile up to its per-profile ceiling —
			// the number of legal placements for each.
			name:     "empty a100 card",
			occupied: nil,
			possible: a100PossiblePlacements,
			want:     map[string]int32{"1g.5gb": 7, "1g.10gb": 4, "2g.10gb": 3, "3g.20gb": 2, "4g.20gb": 1, "7g.40gb": 1},
		},
		{
			// One 1g.5gb at slot 0 (occupies [0,1)) reduces every profile whose only
			// low-slot placement is blocked; profiles left unbuildable drop out.
			name:     "a100 fragmented by one 1g.5gb at slot 0",
			occupied: []AcceleratorPhysicalPlacement{{Start: 0, Length: 1}},
			possible: a100PossiblePlacements,
			want:     map[string]int32{"1g.5gb": 6, "1g.10gb": 3, "2g.10gb": 2, "3g.20gb": 1},
		},
		{
			// A fully occupied card (whole-card 7g.40gb) leaves nothing buildable.
			name:     "a100 fully occupied by 7g.40gb",
			occupied: []AcceleratorPhysicalPlacement{{Start: 0, Length: 8}},
			possible: a100PossiblePlacements,
			want:     map[string]int32{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeRemainingProfiles(tc.occupied, tc.possible)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ComputeRemainingProfiles() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestProfileCountSlice(t *testing.T) {
	testCases := []struct {
		name   string
		counts map[string]int32
		want   []AcceleratorProfileCount
	}{
		{
			// A non-empty map renders name-sorted regardless of map iteration order.
			name:   "sorted by name",
			counts: map[string]int32{"2g.10gb": 1, "1g.5gb": 3, "1g.10gb": 2},
			want: []AcceleratorProfileCount{
				{Name: "1g.10gb", Count: 2},
				{Name: "1g.5gb", Count: 3},
				{Name: "2g.10gb", Count: 1},
			},
		},
		{
			// An empty map yields nil so the ledger field is omitted on serialization.
			name:   "empty map yields nil",
			counts: map[string]int32{},
			want:   nil,
		},
		{
			name:   "nil map yields nil",
			counts: nil,
			want:   nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProfileCountSlice(tc.counts)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ProfileCountSlice() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}
