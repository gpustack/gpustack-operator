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
