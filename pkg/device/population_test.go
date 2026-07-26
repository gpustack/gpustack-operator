package device

import "testing"

func TestPopulationPredicates(t *testing.T) {
	testCases := []struct {
		name               string
		status             AcceleratorStatus
		wantLogicallySlice bool
		wantPartitioned    bool
		wantWholeCard      bool
	}{
		{
			// A not-partitioned card that reports a logical-slice count is logically
			// sliceable and, since it has no hardware partitions, still whole-card
			// capable.
			name:               "logically sliceable, not partitioned",
			status:             AcceleratorStatus{LogicalSliced: AcceleratorLogicalSliced{Count: 4}},
			wantLogicallySlice: true,
			wantPartitioned:    false,
			wantWholeCard:      true,
		},
		{
			// A MIG-enabled card reports zero logical count (by construction) and a
			// non-zero physical ceiling; it is partitioned and cannot serve a
			// whole-card claim.
			name:            "partitioned",
			status:          AcceleratorStatus{PhysicalSliced: AcceleratorPhysicalSliced{Count: 7}},
			wantPartitioned: true,
			wantWholeCard:   false,
		},
		{
			// A card reporting neither capability is not sliceable in either mode,
			// and remains whole-card capable since it is not partitioned.
			name:          "reports neither capability",
			status:        AcceleratorStatus{},
			wantWholeCard: true,
		},
		{
			// No detector produces this — the capabilities are written in exclusive
			// branches — but the predicates must not let it serve both families if one
			// ever did. Partitioning wins: the card serves the partition family alone.
			name: "reports both capabilities",
			status: AcceleratorStatus{
				LogicalSliced:  AcceleratorLogicalSliced{Count: 4},
				PhysicalSliced: AcceleratorPhysicalSliced{Count: 7},
			},
			wantLogicallySlice: false,
			wantPartitioned:    true,
			wantWholeCard:      false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLogicallySliceable(tc.status); got != tc.wantLogicallySlice {
				t.Errorf("IsLogicallySliceable() = %v, want %v", got, tc.wantLogicallySlice)
			}
			if got := IsPartitioned(tc.status); got != tc.wantPartitioned {
				t.Errorf("IsPartitioned() = %v, want %v", got, tc.wantPartitioned)
			}
			if got := IsWholeCardCapable(tc.status); got != tc.wantWholeCard {
				t.Errorf("IsWholeCardCapable() = %v, want %v", got, tc.wantWholeCard)
			}
		})
	}
}
