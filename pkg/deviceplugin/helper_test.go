package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// TestPartitionedUnitGranularity guards the invariant that D divides evenly by the
// maximum partition count, so a single sliced partition maps to a whole number of
// units (12800/512 = 25). The sliced path no longer materializes these units in the
// device plugin (counting moved to Patch Node), but the invariant must still hold if
// the per-partition units math is reintroduced for real isolation later.
func TestPartitionedUnitGranularity(t *testing.T) {
	assert.Zerof(t, nodefeature.ResourceMaxUnits%nodefeature.SlicedResourceMaxSize,
		"D=%d must divide evenly by max partitions %d", nodefeature.ResourceMaxUnits, nodefeature.SlicedResourceMaxSize)
	assert.Equal(t, 25, nodefeature.ResourceMaxUnits/nodefeature.SlicedResourceMaxSize, "units per smallest partition")
}

func TestPadSlicedUnits(t *testing.T) {
	const d = nodefeature.ResourceMaxUnits // 12800

	cases := []struct {
		name          string
		units         int64
		maxPartitions int64
		want          int64
	}{
		{name: "rounds up to the next coarser slice", units: 2000, maxPartitions: 8, want: 3200}, // 1/4 card
		{name: "exact slice boundary is unchanged", units: 1600, maxPartitions: 8, want: 1600},   // 1/8 card
		{name: "finer than hardware rounds up to finest slice", units: 100, maxPartitions: 8, want: 1600},
		{name: "whole card or larger caps at D", units: 20000, maxPartitions: 8, want: d},
		{name: "exactly a whole card caps at D", units: d, maxPartitions: 8, want: d},
		{name: "finest 1/512 slice", units: 30, maxPartitions: 512, want: 50}, // D/256
		{name: "exact finest slice is unchanged", units: 25, maxPartitions: 512, want: 25},
		{name: "no slicing capacity yields a whole card", units: 100, maxPartitions: 1, want: d},
		{name: "non-positive maxPartitions yields a whole card", units: 100, maxPartitions: 0, want: d},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, PadSlicedUnits(c.units, c.maxPartitions))
		})
	}
}

func TestResource_GetDeviceIds(t *testing.T) {
	res := Resource{Group: "grp-0", Device: "dev-0"}

	cases := []struct {
		name          string
		mode          workercore.DeviceAllocationMode
		maxPartitions int32
		wantLen       int
		wantFirst     string
		wantLast      string
	}{
		{
			name:      "exclusive advertises a single whole-card token",
			mode:      workercore.DeviceAllocationModeExclusive,
			wantLen:   1,
			wantFirst: "grp-0:dev-0:0000",
			wantLast:  "grp-0:dev-0:0000",
		},
		{
			name:      "shared advertises one token per owner",
			mode:      workercore.DeviceAllocationModeShared,
			wantLen:   nodefeature.SharedResourceMaxSize,
			wantFirst: "grp-0:dev-0:0000",
		},
		{
			// Sliced advertises a loose token pool sized by the card's MaxPartitions,
			// not the old per-card MaxUnits (12800) fake-device pool.
			name:          "sliced advertises MaxPartitions tokens per card",
			mode:          workercore.DeviceAllocationModeSliced,
			maxPartitions: 8,
			wantLen:       8,
			wantFirst:     "grp-0:dev-0:0000",
			wantLast:      "grp-0:dev-0:0007",
		},
		{
			name:          "sliced clamps a non-positive MaxPartitions to one token",
			mode:          workercore.DeviceAllocationModeSliced,
			maxPartitions: 0,
			wantLen:       1,
			wantFirst:     "grp-0:dev-0:0000",
			wantLast:      "grp-0:dev-0:0000",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ids := res.GetDeviceIds(c.mode, c.maxPartitions)
			assert.Len(t, ids, c.wantLen)
			if c.wantLen > 0 {
				assert.Equal(t, c.wantFirst, ids[0], "first id")
				if c.wantLast != "" {
					assert.Equal(t, c.wantLast, ids[len(ids)-1], "last id")
				}
			}
		})
	}
}

func TestSliceRatio(t *testing.T) {
	const unitsName core.ResourceName = "nvidia.com/gpu.sliced.units"

	ctrWith := func(units int64) *core.Container {
		return &core.Container{
			Name: "main",
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{
					unitsName: *resource.NewQuantity(units, resource.DecimalSI),
				},
			},
		}
	}

	cases := []struct {
		name    string
		ctr     *core.Container
		wantR   float64
		wantErr bool
	}{
		{name: "1/8 card", ctr: ctrWith(1600), wantR: 0.125}, // 1600/12800
		{name: "1/4 card", ctr: ctrWith(3200), wantR: 0.25},  // 3200/12800
		{name: "finest 1/512 slice", ctr: ctrWith(25), wantR: 0.001953125},
		{name: "missing request errors", ctr: &core.Container{Name: "main"}, wantErr: true},
		{name: "zero request errors", ctr: ctrWith(0), wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r, err := SliceRatio(c.ctr, unitsName)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.wantR, r)
		})
	}
}

func TestFloorPercent(t *testing.T) {
	cases := []struct {
		r    float64
		want int
	}{
		{r: 0.125, want: 12}, // floor(12.5)
		{r: 0.25, want: 25},
		{r: 0.5, want: 50},
		{r: 1, want: 100},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, FloorPercent(c.r))
	}
}
