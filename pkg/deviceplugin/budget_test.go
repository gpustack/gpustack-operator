package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

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

func TestContainerEnvDeclared(t *testing.T) {
	ctrWith := func(env ...core.EnvVar) *core.Container {
		return &core.Container{Name: "main", Env: env}
	}
	cases := []struct {
		name string
		ctr  *core.Container
		key  string
		want bool
	}{
		{"declared", ctrWith(core.EnvVar{Name: "LIBCUDA_LOG_LEVEL", Value: "3"}), "LIBCUDA_LOG_LEVEL", true},
		{"absent among others", ctrWith(core.EnvVar{Name: "FOO", Value: "1"}), "LIBCUDA_LOG_LEVEL", false},
		{"empty env", ctrWith(), "LIBCUDA_LOG_LEVEL", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, ContainerEnvDeclared(c.ctr, c.key))
		})
	}
}
