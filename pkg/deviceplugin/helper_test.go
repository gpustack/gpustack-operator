package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

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
			wantLen:   MaxUnits / _StepInShared,
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
