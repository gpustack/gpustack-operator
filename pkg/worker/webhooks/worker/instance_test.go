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

// TestInstanceWebhook_ValidateCreate_SlicedUnits pins Task 12: on a sliced 8s
// InstanceType the per-card unit count U must be a power of two, at most the
// OnceMaxRequest (partitions/2 = 4), and strictly less than the partition count.
func TestInstanceWebhook_ValidateCreate_SlicedUnits(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name    string
		units   int32
		onceMax string // OnceMaxRequest; "" → 4 (fresh 8s)
		wantErr bool
	}{
		{name: "one unit is allowed", units: 1},
		{name: "two units is allowed", units: 2},
		{name: "four units (== OnceMaxRequest) is allowed", units: 4},
		{name: "three units (not a power of two) is rejected", units: 3, wantErr: true},
		{name: "zero units is normalized to one and allowed", units: 0},
		{name: "units equal to partitions is rejected", units: 8, wantErr: true},
		{name: "units above partitions is rejected", units: 16, wantErr: true},
		{
			// When capacity is low the dynamic ORM drops (Task 11), so U beyond it
			// is rejected even though it is < partitions.
			name:  "units above a shrunken OnceMaxRequest is rejected",
			units: 4, onceMax: "2", wantErr: true,
		},
		{
			name:  "units within a shrunken OnceMaxRequest is allowed",
			units: 2, onceMax: "2",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			onceMax := c.onceMax
			if onceMax == "" {
				onceMax = "4"
			}
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: worker.InstanceTypeSpec{
					Acceleratable:           true,
					Manufacturer:            "nvidia",
					InstanceTypeAccelerator: worker.InstanceTypeAccelerator{Sliced: 8},
				},
				Status: worker.InstanceTypeStatus{
					Accelerator: worker.InstanceTypeResource{OnceMaxRequest: resource.MustParse(onceMax)},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			acc := resource.MustParse("1")
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:      &acc,
				AcceleratorUnits: c.units,
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
