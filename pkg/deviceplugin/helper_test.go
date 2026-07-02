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

func TestSlicedCoresPercent(t *testing.T) {
	const coresName core.ResourceName = "nvidia.com/gpu.sliced.cores-percentage"
	ctrWith := func(limits core.ResourceList) *core.Container {
		return &core.Container{Name: "main", Resources: core.ResourceRequirements{Limits: limits}}
	}
	q := func(v int64) resource.Quantity { return *resource.NewQuantity(v, resource.DecimalSI) }

	cases := []struct {
		name string
		ctr  *core.Container
		want int
	}{
		{"explicit 10%", ctrWith(core.ResourceList{coresName: q(10)}), 10},
		{"explicit 100%", ctrWith(core.ResourceList{coresName: q(100)}), 100},
		{"absent defaults to 100", ctrWith(nil), 100},
		{"non-positive defaults to 100", ctrWith(core.ResourceList{coresName: q(0)}), 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, SlicedCoresPercent(c.ctr, coresName))
		})
	}
}

func TestSlicedMemoryMib(t *testing.T) {
	const (
		pctName  core.ResourceName = "nvidia.com/gpu.sliced.memory-percentage"
		mibName  core.ResourceName = "nvidia.com/gpu.sliced.memory-mib"
		cardVRAM int64             = 24576 // 24Gi
	)
	ctrWith := func(limits core.ResourceList) *core.Container {
		return &core.Container{Name: "main", Resources: core.ResourceRequirements{Limits: limits}}
	}
	q := func(v int64) resource.Quantity { return *resource.NewQuantity(v, resource.DecimalSI) }

	cases := []struct {
		name    string
		ctr     *core.Container
		want    int64
		wantErr bool
	}{
		{"percentage of card VRAM", ctrWith(core.ResourceList{pctName: q(25)}), 6144, false},
		{"percentage floors", ctrWith(core.ResourceList{pctName: q(33)}), 8110, false}, // floor(24576*33/100)
		{"absolute mib", ctrWith(core.ResourceList{mibName: q(3072)}), 3072, false},
		{"percentage wins over mib", ctrWith(core.ResourceList{pctName: q(50), mibName: q(1024)}), 12288, false},
		{"mib capped at card VRAM", ctrWith(core.ResourceList{mibName: q(99999)}), cardVRAM, false},
		{"neither set errors", ctrWith(nil), 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SlicedMemoryMib(c.ctr, pctName, mibName, cardVRAM)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}
