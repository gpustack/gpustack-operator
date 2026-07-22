package device

import "sort"

// ComputeRemainingProfiles counts, per profile name, how many more hardware GPU
// partitions can still be created on a card given its occupied placement intervals: for
// each profile it counts the legal placements (possible[name]) whose memory-slice
// interval overlaps no occupied interval. Profiles with no buildable placement are
// omitted, so the result lists only what can currently be created. It is pure interval
// arithmetic with no device access.
func ComputeRemainingProfiles(
	occupied []AcceleratorPhysicalPlacement,
	possible map[string][]AcceleratorPhysicalPlacement,
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

// placementOverlapsAny reports whether slot's memory-slice interval [Start, Start+Length)
// intersects any occupied interval. Two half-open intervals [a, a+m) and [b, b+n)
// overlap iff a < b+n and b < a+m.
func placementOverlapsAny(slot AcceleratorPhysicalPlacement, occupied []AcceleratorPhysicalPlacement) bool {
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
// field (a card with no partitions then serializes byte-identically to before the ledger
// existed).
func ProfileCountSlice(counts map[string]int32) []AcceleratorProfileCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]AcceleratorProfileCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, AcceleratorProfileCount{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
