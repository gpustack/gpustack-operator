package nvidia

import (
	"testing"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestMigProfileUnitsFold(t *testing.T) {
	const d = nodefeature.ResourceMaxUnits

	// A MIG instance folds into .sliced.units via the same VRAM-anchored
	// MemoryMibToUnits the logical .sliced.memory-mib path uses, so a MIG profile and a
	// logical slice of the same VRAM charge identical credits. These pin the H100-80GB
	// worked-example fold values (10GiB->D/8, 20GiB->D/4, 40GiB->D/2, 80GiB->D).
	const h100VRAM = int64(81920)
	testCases := []struct {
		name   string
		memMib int64
		want   int64
	}{
		{"1g.10gb -> D/8", 10 * 1024, d / 8},
		{"1g.20gb / 2g.20gb -> D/4", 20 * 1024, d / 4},
		{"3g.40gb / 4g.40gb -> D/2", 40 * 1024, d / 2},
		{"7g.80gb (whole card) -> D", 80 * 1024, d},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodefeature.MemoryMibToUnits(tc.memMib, h100VRAM)
			if got != tc.want {
				t.Errorf("MemoryMibToUnits(%d, %d) = %d, want %d", tc.memMib, h100VRAM, got, tc.want)
			}
		})
	}
}
