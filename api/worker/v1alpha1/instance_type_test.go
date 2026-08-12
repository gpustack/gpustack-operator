package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInstanceTypeAcceleratorDetailSliceablePredicates pins that the pool's two slicing
// predicates are independent. Logical and physical slicing are mutually exclusive per
// ACCELERATOR — the per-accelerator device.IsLogicallySliceable folds in !IsPartitioned for
// exactly that reason — but a pool aggregates accelerators of both kinds, and a mixed node
// advertises both families at once. Making
// either predicate exclude the other here would starve a mixed pool of logical slices, or let an
// all-partitioned pool read as logically sliceable.
func TestInstanceTypeAcceleratorDetailSliceablePredicates(t *testing.T) {
	cases := []struct {
		name         string
		logical      int32
		physical     int32
		wantLogical  bool
		wantPhysical bool
	}{
		{name: "no slicing at all", wantLogical: false, wantPhysical: false},
		{name: "logical only", logical: 128, wantLogical: true, wantPhysical: false},
		{name: "physical only", physical: 4, wantLogical: false, wantPhysical: true},
		{
			name: "mixed pool advertises both", logical: 128, physical: 4,
			wantLogical: true, wantPhysical: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			detail := InstanceTypeAcceleratorDetail{
				SlicedDetail: AcceleratorSlicedDetail{
					Logical:  AcceleratorSlicedLogicalDetail{Count: c.logical},
					Physical: AcceleratorSlicedPhysicalDetail{Count: c.physical},
				},
			}
			assert.Equal(t, c.wantLogical, detail.IsLogicallySliceable())
			assert.Equal(t, c.wantPhysical, detail.IsPhysicallySliceable())
		})
	}
}
