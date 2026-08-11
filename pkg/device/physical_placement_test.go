package device

import (
	"reflect"
	"testing"
)

// a100PossiblePlacements is the hardcoded A100 legal placement set (start:length in
// memory-slice units) for the six kept profiles — the fixture the free-count math runs
// against.
var a100PossiblePlacements = map[string][]AcceleratorPlacement{
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
		occupied []AcceleratorPlacement
		possible map[string][]AcceleratorPlacement
		want     map[string]int32
	}{
		{
			// The spec's worked example: an accelerator already holding 1x3g.20gb@slot0
			// (occupies memory slices [0,4)) can still build the reduced set.
			name:     "a100 with one 3g.20gb at slot 0",
			occupied: []AcceleratorPlacement{{Start: 0, Length: 4}},
			possible: a100PossiblePlacements,
			want:     map[string]int32{"1g.5gb": 3, "1g.10gb": 2, "2g.10gb": 1, "3g.20gb": 1},
		},
		{
			// An empty accelerator can build every profile up to its per-profile ceiling —
			// the number of legal placements for each.
			name:     "empty a100 accelerator",
			occupied: nil,
			possible: a100PossiblePlacements,
			want:     map[string]int32{"1g.5gb": 7, "1g.10gb": 4, "2g.10gb": 3, "3g.20gb": 2, "4g.20gb": 1, "7g.40gb": 1},
		},
		{
			// One 1g.5gb at slot 0 (occupies [0,1)) reduces every profile whose only
			// low-slot placement is blocked; profiles left unbuildable drop out.
			name:     "a100 fragmented by one 1g.5gb at slot 0",
			occupied: []AcceleratorPlacement{{Start: 0, Length: 1}},
			possible: a100PossiblePlacements,
			want:     map[string]int32{"1g.5gb": 6, "1g.10gb": 3, "2g.10gb": 2, "3g.20gb": 1},
		},
		{
			// A fully occupied accelerator (whole-accelerator 7g.40gb) leaves nothing buildable.
			name:     "a100 fully occupied by 7g.40gb",
			occupied: []AcceleratorPlacement{{Start: 0, Length: 8}},
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

// TestSelectPartitionPlacements covers the decision the device plugin makes in place of the
// kubelet: which accelerator a partition lands on, and where on it.
func TestSelectPartitionPlacements(t *testing.T) {
	// card builds a candidate offering profile's legal placements on the A100 fixture.
	card := func(id, profile string, occupied ...AcceleratorPlacement) PartitionCandidate {
		return PartitionCandidate{ID: id, Possible: a100PossiblePlacements[profile], Occupied: occupied}
	}

	testCases := []struct {
		name       string
		candidates []PartitionCandidate
		count      int
		want       []PartitionSelection
		wantOK     bool
	}{
		{
			// One instance on an empty node takes the lowest interval of the first accelerator.
			name:       "empty node places at the lowest interval",
			candidates: []PartitionCandidate{card("a", "3g.20gb"), card("b", "3g.20gb")},
			count:      1,
			want:       []PartitionSelection{{ID: "a", Placement: AcceleratorPlacement{Start: 0, Length: 4}}},
			wantOK:     true,
		},
		{
			// One selection still takes at most one instance per accelerator, so a two-instance
			// request uses two accelerators however much room the first one has left.
			name:       "two instances of one request use two accelerators",
			candidates: []PartitionCandidate{card("a", "3g.20gb"), card("b", "3g.20gb")},
			count:      2,
			want: []PartitionSelection{
				{ID: "a", Placement: AcceleratorPlacement{Start: 0, Length: 4}},
				{ID: "b", Placement: AcceleratorPlacement{Start: 0, Length: 4}},
			},
			wantOK: true,
		},
		{
			// The accelerator already in use wins over the empty one, so the empty one stays
			// whole for a profile that needs all of it.
			name: "an accelerator already in use is filled before an empty sibling",
			candidates: []PartitionCandidate{
				card("a", "3g.20gb"),
				card("b", "3g.20gb", AcceleratorPlacement{Start: 0, Length: 4}),
			},
			count:  1,
			want:   []PartitionSelection{{ID: "b", Placement: AcceleratorPlacement{Start: 4, Length: 4}}},
			wantOK: true,
		},
		{
			// One selection takes at most one instance per accelerator, whatever room is left:
			// the Pod's annotation records one placement set per accelerator, so a second
			// instance of the same request on the same accelerator could not be counted.
			name:       "a third instance has no accelerator left, even with room on both",
			candidates: []PartitionCandidate{card("a", "3g.20gb"), card("b", "3g.20gb")},
			count:      3,
			wantOK:     false,
		},
		{
			// Another Pod's instance on the same accelerator is different: it carries its own
			// record, so the accelerator is still a candidate for whatever room it has left.
			name: "an accelerator another Pod partly holds still takes one instance",
			candidates: []PartitionCandidate{
				card("a", "3g.20gb", AcceleratorPlacement{Start: 0, Length: 4}),
			},
			count:  1,
			want:   []PartitionSelection{{ID: "a", Placement: AcceleratorPlacement{Start: 4, Length: 4}}},
			wantOK: true,
		},
		{
			// An accelerator whose geometry a live instance already blocks is skipped for a
			// profile that no longer fits, and the emptier one takes the request — the
			// mixed-profile case the kubelet's own pick cannot get right.
			name: "an accelerator blocked for this profile is skipped",
			candidates: []PartitionCandidate{
				card("a", "7g.40gb", AcceleratorPlacement{Start: 0, Length: 1}),
				card("b", "7g.40gb"),
			},
			count:  1,
			want:   []PartitionSelection{{ID: "b", Placement: AcceleratorPlacement{Start: 0, Length: 8}}},
			wantOK: true,
		},
		{
			// No accelerator can host it: the node, not one accelerator, is out of room.
			name: "a node with no free placement fails",
			candidates: []PartitionCandidate{
				card("a", "7g.40gb", AcceleratorPlacement{Start: 0, Length: 1}),
			},
			count:  1,
			wantOK: false,
		},
		{
			name:       "no candidates at all fails",
			candidates: nil,
			count:      1,
			wantOK:     false,
		},
		{
			name:       "a zero count places nothing",
			candidates: []PartitionCandidate{card("a", "3g.20gb")},
			count:      0,
			wantOK:     true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SelectPartitionPlacements(tc.candidates, tc.count)
			if ok != tc.wantOK {
				t.Fatalf("SelectPartitionPlacements() ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if len(tc.want) == 0 && len(got) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SelectPartitionPlacements() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

// TestSelectPartitionPlacementsPacksToPreserveWholeCards shows the reason for the policy the
// way it actually plays out: each Allocate is its own call, and the instance the previous one
// placed is part of the next one's occupied set. Three small partitions arriving one at a time
// fill a single accelerator, leaving its sibling whole for the large profile that arrives later —
// which spreading them across both accelerators would have made unplaceable.
func TestSelectPartitionPlacementsPacksToPreserveWholeCards(t *testing.T) {
	occupied := map[string][]AcceleratorPlacement{}
	candidatesFor := func(profile string) []PartitionCandidate {
		return []PartitionCandidate{
			{ID: "a", Possible: a100PossiblePlacements[profile], Occupied: occupied["a"]},
			{ID: "b", Possible: a100PossiblePlacements[profile], Occupied: occupied["b"]},
		}
	}

	for i := range 3 {
		got, ok := SelectPartitionPlacements(candidatesFor("1g.5gb"), 1)
		if !ok {
			t.Fatalf("small request %d could not be placed", i)
		}
		occupied[got[0].ID] = append(occupied[got[0].ID], got[0].Placement)
	}
	if len(occupied["b"]) != 0 {
		t.Errorf("packing must leave the sibling untouched, got %+v", occupied["b"])
	}

	got, ok := SelectPartitionPlacements(candidatesFor("7g.40gb"), 1)
	if !ok {
		t.Fatal("the whole-card profile must still fit on the untouched card")
	}
	if got[0].ID != "b" {
		t.Errorf("the whole-card profile must land on the untouched card, got %q", got[0].ID)
	}
}

// TestSelectPartitionPlacementsDoesNotMutateCandidates pins that a decision leaves the caller's
// occupied sets untouched, so the ledger a caller holds across several attempts never drifts.
func TestSelectPartitionPlacementsDoesNotMutateCandidates(t *testing.T) {
	occupied := []AcceleratorPlacement{{Start: 0, Length: 4}}
	candidates := []PartitionCandidate{
		{ID: "a", Possible: a100PossiblePlacements["3g.20gb"], Occupied: occupied},
	}

	if _, ok := SelectPartitionPlacements(candidates, 1); !ok {
		t.Fatal("SelectPartitionPlacements() should place one more 3g.20gb")
	}
	if !reflect.DeepEqual(candidates[0].Occupied, []AcceleratorPlacement{{Start: 0, Length: 4}}) {
		t.Errorf("the candidate's occupied set was mutated: %+v", candidates[0].Occupied)
	}
	if len(occupied) != 1 {
		t.Errorf("the caller's backing slice grew to %d entries", len(occupied))
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
