package device

import (
	"cmp"
	"slices"
)

// ComputeRemainingProfiles counts, per profile name, how many more hardware GPU
// partitions can still be created on an accelerator given its occupied placement
// intervals: for each profile it counts the legal placements (possible[name]) whose
// memory-slice interval overlaps no occupied interval. Profiles with no buildable
// placement are omitted, so the result lists only what can currently be created. It is
// pure interval arithmetic with no device access.
func ComputeRemainingProfiles(
	occupied []AcceleratorPlacement,
	possible map[string][]AcceleratorPlacement,
) map[string]int32 {
	remaining := make(map[string]int32, len(possible))
	for name, slots := range possible {
		var n int32
		for i := range slots {
			if !placementOverlapsAny(slots[i], occupied) {
				n++
			}
		}
		if n > 0 {
			remaining[name] = n
		}
	}
	return remaining
}

// PartitionCandidate is one partitioned accelerator a placement decision may choose: the
// legal memory-slice intervals the requested profile can occupy on it, and the intervals
// already taken. Accelerators are identified only by an opaque ID the selector echoes back,
// so the decision stays pure interval arithmetic with no device access and no knowledge of
// the caller's resource shape.
type PartitionCandidate struct {
	// ID identifies the accelerator to the caller.
	ID string
	// Possible lists every legal placement of the requested profile on this accelerator,
	// in the accelerator's own capability order.
	Possible []AcceleratorPlacement
	// Occupied lists the intervals already taken on this accelerator — both those a live
	// allocation records and those an in-flight allocation has already claimed.
	Occupied []AcceleratorPlacement
}

// PartitionSelection is one placed instance: the accelerator it lands on and the
// memory-slice interval it will occupy there.
type PartitionSelection struct {
	ID        string
	Placement AcceleratorPlacement
}

// SelectPartitionPlacements chooses where to build count instances of one profile across the
// candidate accelerators, or reports false when the node cannot host them all.
//
// One selection takes at most one instance per accelerator. That is not a placement preference
// but the record's shape: a Pod's allocation annotation carries one profile and one placement
// set per accelerator, so a second instance of the same request on the same accelerator could
// not be counted. Two different Pods still share an accelerator freely — each carries its own
// record — which is what the caller's Occupied sets already reflect.
//
// Accelerators are chosen most-occupied first among those that still fit: filling an
// accelerator that is already in use keeps its siblings whole, so a later large profile still
// has somewhere to go. Spreading instead would put one small partition on every accelerator and
// leave a node with plenty of free memory unable to host a single whole-accelerator profile.
// Hardware partitioning is what makes this the right way round — a MIG instance's memory
// bandwidth is isolated by the hardware, so sharing an accelerator costs the tenant nothing that
// spreading would have bought it. Within an accelerator the lowest free interval wins, so the
// free room stays contiguous at the high end and the decision is deterministic — two identical
// requests against identical state place identically.
func SelectPartitionPlacements(candidates []PartitionCandidate, count int) ([]PartitionSelection, bool) {
	if count <= 0 {
		return nil, true
	}

	used := make([]bool, len(candidates))
	selections := make([]PartitionSelection, 0, count)
	for range count {
		var (
			best     = -1
			bestSlot AcceleratorPlacement
			bestRoom int32
		)
		for i := range candidates {
			if used[i] {
				continue
			}
			slot, ok := lowestFreePlacement(candidates[i].Possible, candidates[i].Occupied)
			if !ok {
				continue
			}
			if room := occupiedLength(candidates[i].Occupied); best < 0 || room > bestRoom {
				best, bestSlot, bestRoom = i, slot, room
			}
		}
		if best < 0 {
			return nil, false
		}
		used[best] = true
		selections = append(selections, PartitionSelection{ID: candidates[best].ID, Placement: bestSlot})
	}
	return selections, true
}

// lowestFreePlacement returns the first legal placement that overlaps nothing occupied.
// Possible is in the accelerator's capability order, which the detector emits by ascending
// start.
func lowestFreePlacement(
	possible, occupied []AcceleratorPlacement,
) (AcceleratorPlacement, bool) {
	for i := range possible {
		if !placementOverlapsAny(possible[i], occupied) {
			return possible[i], true
		}
	}
	return AcceleratorPlacement{}, false
}

// occupiedLength sums the memory slices an accelerator's occupied intervals cover, the
// measure of how full it is.
func occupiedLength(occupied []AcceleratorPlacement) int32 {
	var n int32
	for i := range occupied {
		n += occupied[i].Length
	}
	return n
}

// placementOverlapsAny reports whether slot's memory-slice interval [Start, Start+Length)
// intersects any occupied interval. Two half-open intervals [a, a+m) and [b, b+n)
// overlap iff a < b+n and b < a+m.
func placementOverlapsAny(slot AcceleratorPlacement, occupied []AcceleratorPlacement) bool {
	slotEnd := slot.Start + slot.Length
	for i := range occupied {
		occ := occupied[i]
		if slot.Start < occ.Start+occ.Length && occ.Start < slotEnd {
			return true
		}
	}
	return false
}

// ProfileCountSlice renders a profile-name→count map as a name-sorted
// []AcceleratorProfileCount, returning nil for an empty map so an empty ledger omits the
// field (an accelerator with no partitions then serializes byte-identically to before the
// ledger existed).
func ProfileCountSlice(counts map[string]int32) []AcceleratorProfileCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]AcceleratorProfileCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, AcceleratorProfileCount{Name: name, Count: count})
	}
	slices.SortFunc(out, func(a, b AcceleratorProfileCount) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// PartitionLedgerReady reports whether an accelerator's allocation row carries a usable
// per-profile partition ledger. A missing row, or one reporting neither allocated nor remaining
// instances, is the not-yet-published state: the runtime status is rebuilt from the accelerator's
// cached placement geometry, which an older device manager did not record.
//
// Every reader of the ledger has to agree on this, because they all take the same fallback to the
// accelerator's static capability ceiling when it is false — publishing zero instead would read as
// a full accelerator on a working node.
func PartitionLedgerReady(alloc *AcceleratorAllocation) bool {
	return alloc != nil && (len(alloc.AllocatedProfiles) > 0 || len(alloc.RemainingProfiles) > 0)
}
