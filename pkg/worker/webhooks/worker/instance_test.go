package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

func newInstanceWebhook(objs ...ctrlcli.Object) *InstanceWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &InstanceWebhook{Client: cli, APIReader: cli}
}

// webhookInstance builds a valid Instance (with a volume so non-type validation
// passes) referencing the given InstanceType.
func webhookInstance(name, instType string) *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: name},
		Spec: workercore.InstanceSpec{
			Type:             instType,
			InstanceTemplate: workercore.InstanceTemplate{Image: "img"},
			Volume: workercore.InstanceVolume{
				Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: resource.MustParse("10Gi")},
			},
		},
	}
}

func TestInstanceWebhook_ValidateCreate(t *testing.T) {
	cases := []struct {
		name string

		instType string
		stop     bool

		wantErr bool
	}{
		{
			name:     "stopped allows missing type",
			instType: "missing",
			stop:     true,
		},
		{
			name:     "running requires type",
			instType: "missing",
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook()
			inst := webhookInstance("a", c.instType)
			if c.stop {
				inst.Spec.Stop = ptr.To(true)
			}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_SlicedPercentages pins the sliced request
// contract: the memory/compute percentages must be in [0,100] and the compute
// budget may not be smaller than the memory budget.
func TestInstanceWebhook_ValidateCreate_SlicedPercentages(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name             string
		memPct, coresPct int32
		wantErr          bool
	}{
		{name: "equal budgets allowed", memPct: 20, coresPct: 20},
		{name: "compute larger than memory allowed", memPct: 20, coresPct: 50},
		{name: "whole-card slice allowed", memPct: 100, coresPct: 100},
		{name: "no slice allowed", memPct: 0, coresPct: 0},
		{name: "compute smaller than memory rejected", memPct: 50, coresPct: 20, wantErr: true},
		{name: "memory above 100 rejected", memPct: 101, coresPct: 101, wantErr: true},
		{name: "compute above 100 rejected", memPct: 20, coresPct: 101, wantErr: true},
		{name: "negative memory rejected", memPct: -1, wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: worker.InstanceTypeSpec{
					Acceleratable:           true,
					Manufacturer:            "nvidia",
					InstanceTypeAccelerator: worker.InstanceTypeAccelerator{Sliced: 8},
				},
				Status: worker.InstanceTypeStatus{
					Accelerator: worker.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			acc := resource.MustParse("1")
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:                       &acc,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInstanceWebhook_ValidateUpdate(t *testing.T) {
	cases := []struct {
		name string

		oldType, newType string
		oldStop, newStop *bool  // nil → Stop left unset (distinct from false)
		phase            string // applied to both old and new status
		registerType     string // "" → no InstanceType registered

		wantErr bool
	}{
		{
			name:    "stopped allows type change",
			oldType: "old", newType: "new",
			oldStop: ptr.To(true), newStop: ptr.To(true),
			phase: workerctrl.InstancePhaseStopped,
		},
		{
			name:    "running forbids type change",
			oldType: "old", newType: "new",
			phase:   workerctrl.InstancePhaseReady,
			wantErr: true,
		},
		{
			name:    "start stopped requires existing type",
			oldType: "gone", newType: "gone",
			oldStop: ptr.To(true), newStop: ptr.To(false),
			phase:   workerctrl.InstancePhaseStopped,
			wantErr: true,
		},
		{
			name:    "start stopped with existing type allowed",
			oldType: "live", newType: "live",
			oldStop: ptr.To(true), newStop: ptr.To(false),
			phase:        workerctrl.InstancePhaseStopped,
			registerType: "live",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.registerType != "" {
				objs = append(objs, &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: c.registerType}})
			}
			w := newInstanceWebhook(objs...)

			old := webhookInstance("a", c.oldType)
			old.Spec.Stop = c.oldStop
			old.Status.Phase = c.phase
			neu := webhookInstance("a", c.newType)
			neu.Spec.Stop = c.newStop
			neu.Status.Phase = c.phase

			_, err := w.ValidateUpdate(context.Background(), old, neu)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInstanceWebhook_Default(t *testing.T) {
	cases := []struct {
		name string

		stop  bool
		phase string

		wantErr bool
	}{
		{
			// Fresh (Phase "", not stopped) → the type must exist.
			name:    "fresh requires type",
			wantErr: true,
		},
		{
			name: "stopped skips type",
			stop: true,
		},
		{
			name:    "running update skips type",
			phase:   workerctrl.InstancePhaseReady,
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook()
			inst := webhookInstance("a", "missing")
			if c.stop {
				inst.Spec.Stop = ptr.To(true)
			}
			inst.Status.Phase = c.phase

			err := w.Default(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_Default_SlicedPercentages verifies the webhook copies a
// lone memory/compute percentage to the other so they default to an equal share.
func TestInstanceWebhook_Default_SlicedPercentages(t *testing.T) {
	// Default reads a setting through the loopback client; point it at an empty
	// fake cluster (configured once) so the setting falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-8s"

	cases := []struct {
		name                     string
		memPct, coresPct         int32
		wantMemPct, wantCoresPct int32
	}{
		{name: "memory copied to cores", memPct: 20, coresPct: 0, wantMemPct: 20, wantCoresPct: 20},
		{name: "cores copied to memory", memPct: 0, coresPct: 30, wantMemPct: 30, wantCoresPct: 30},
		{name: "both set left unchanged", memPct: 20, coresPct: 40, wantMemPct: 20, wantCoresPct: 40},
		{name: "both zero left unchanged", memPct: 0, coresPct: 0, wantMemPct: 0, wantCoresPct: 0},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: worker.InstanceTypeSpec{
					Acceleratable:           true,
					Manufacturer:            "nvidia",
					InstanceTypeAccelerator: worker.InstanceTypeAccelerator{Sliced: 8},
					UnitResources:           worker.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			acc := resource.MustParse("1")
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:                       &acc,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			err := w.Default(context.Background(), inst)
			assert.NoError(t, err)
			assert.Equal(t, c.wantMemPct, inst.Spec.Resources.AcceleratorSlicedMemoryPercentage, "memory percentage")
			assert.Equal(t, c.wantCoresPct, inst.Spec.Resources.AcceleratorSlicedCoresPercentage, "cores percentage")
		})
	}
}
