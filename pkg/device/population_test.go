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
			// A not-partitioned accelerator that reports a logical-slice count is logically
			// sliceable and, since it has no hardware partitions, still whole-accelerator
			// capable.
			name:               "logically sliceable, not partitioned",
			status:             AcceleratorStatus{LogicalSliced: AcceleratorLogicalSliced{Count: 4}},
			wantLogicallySlice: true,
			wantPartitioned:    false,
			wantWholeCard:      true,
		},
		{
			// A MIG-enabled accelerator reports zero logical count (by construction) and a
			// non-zero physical ceiling; it is partitioned and cannot serve a
			// whole-accelerator claim.
			name:            "partitioned",
			status:          AcceleratorStatus{PhysicalSliced: AcceleratorPhysicalSliced{Count: 7}},
			wantPartitioned: true,
			wantWholeCard:   false,
		},
		{
			// An accelerator reporting neither capability is not sliceable in either mode,
			// and remains whole-accelerator capable since it is not partitioned.
			name:          "reports neither capability",
			status:        AcceleratorStatus{},
			wantWholeCard: true,
		},
		{
			// No detector produces this — the capabilities are written in exclusive
			// branches — but the predicates must not let it serve both families if one
			// ever did. Partitioning wins: the accelerator serves the partition family alone.
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
			if got := IsWholeAcceleratorCapable(tc.status); got != tc.wantWholeCard {
				t.Errorf("IsWholeAcceleratorCapable() = %v, want %v", got, tc.wantWholeCard)
			}
		})
	}
}
