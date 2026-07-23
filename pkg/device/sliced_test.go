package device

import (
	"reflect"
	"testing"
)

func card(logicalCount int32, overcommit bool, physicalCeiling int32, profiles ...AcceleratorSlicedPhysicalDetailProfile) Accelerator {
	physical := make([]AcceleratorPhysicalSlicedProfile, 0, len(profiles))
	for _, p := range profiles {
		physical = append(physical, AcceleratorPhysicalSlicedProfile{Name: p.Name, Count: p.Count, MemoryMib: p.MemoryMib})
	}
	return Accelerator{
		Status: AcceleratorStatus{
			LogicalSliced: AcceleratorLogicalSliced{Count: logicalCount, CoresPercentageOvercommit: overcommit},
			PhysicalSliced: AcceleratorPhysicalSliced{
				Profiles: physical,
				Count:    physicalCeiling,
			},
		},
	}
}

func TestAggregateAcceleratorSlicedDetail(t *testing.T) {
	prof := func(name string, count int32) AcceleratorSlicedPhysicalDetailProfile {
		return AcceleratorSlicedPhysicalDetailProfile{Name: name, Count: count}
	}

	testCases := []struct {
		name  string
		cards []Accelerator
		want  AcceleratorSlicedDetail
	}{
		{
			// Two soft-sliceable NVIDIA cards + one MIG card: units logic aside, the logical
			// count sums the two soft cards only, and the physical detail comes from the MIG card.
			name: "mixed logical + mig",
			cards: []Accelerator{
				card(128, true, 0),
				card(128, true, 0),
				card(0, false, 7, prof("1g.5gb", 7), prof("2g.10gb", 3), prof("7g.40gb", 1)),
			},
			want: AcceleratorSlicedDetail{
				Logical: AcceleratorSlicedLogicalDetail{CoresPercentageOvercommit: true, Count: 256},
				Physical: AcceleratorSlicedPhysicalDetail{
					Count:    7,
					Profiles: []AcceleratorSlicedPhysicalDetailProfile{prof("1g.5gb", 7), prof("2g.10gb", 3), prof("7g.40gb", 1)},
				},
			},
		},
		{
			// Compute-partitioned model (no overcommit): the flag stays false.
			name:  "all logical, no overcommit",
			cards: []Accelerator{card(4, false, 0), card(4, false, 0), card(4, false, 0)},
			want: AcceleratorSlicedDetail{
				Logical: AcceleratorSlicedLogicalDetail{CoresPercentageOvercommit: false, Count: 12},
			},
		},
		{
			// All-MIG group: no logical budget; physical profiles and ceiling sum by name.
			name: "all mig",
			cards: []Accelerator{
				card(0, false, 7, prof("1g.5gb", 7), prof("2g.10gb", 3)),
				card(0, false, 7, prof("1g.5gb", 7), prof("2g.10gb", 3)),
			},
			want: AcceleratorSlicedDetail{
				Physical: AcceleratorSlicedPhysicalDetail{
					Count:    14,
					Profiles: []AcceleratorSlicedPhysicalDetailProfile{prof("1g.5gb", 14), prof("2g.10gb", 6)},
				},
			},
		},
		{
			// MemoryMib rides through the aggregation: it is uniform per profile name (one
			// instance's VRAM), so it is carried once while Count sums across the group's cards.
			name: "mig carries per-profile memory",
			cards: []Accelerator{
				card(0, false, 7, AcceleratorSlicedPhysicalDetailProfile{Name: "1g.10gb", Count: 7, MemoryMib: 10240}),
				card(0, false, 7, AcceleratorSlicedPhysicalDetailProfile{Name: "1g.10gb", Count: 7, MemoryMib: 10240}),
			},
			want: AcceleratorSlicedDetail{
				Physical: AcceleratorSlicedPhysicalDetail{
					Count:    14,
					Profiles: []AcceleratorSlicedPhysicalDetailProfile{{Name: "1g.10gb", Count: 14, MemoryMib: 10240}},
				},
			},
		},
		{
			name:  "no cards",
			cards: nil,
			want:  AcceleratorSlicedDetail{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := AggregateAcceleratorSlicedDetail(tc.cards)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("AggregateAcceleratorSlicedDetail() mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}
