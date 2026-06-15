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

func TestInstanceWebhook_ValidateCreate_StoppedAllowsMissingType(t *testing.T) {
	w := newInstanceWebhook()
	inst := webhookInstance("a", "missing")
	inst.Spec.Stop = ptr.To(true)

	_, err := w.ValidateCreate(context.Background(), inst)
	assert.NoError(t, err)
}

func TestInstanceWebhook_ValidateCreate_RunningRequiresType(t *testing.T) {
	w := newInstanceWebhook()
	inst := webhookInstance("a", "missing")

	_, err := w.ValidateCreate(context.Background(), inst)
	assert.Error(t, err)
}

func TestInstanceWebhook_ValidateUpdate_StoppedAllowsTypeChange(t *testing.T) {
	w := newInstanceWebhook()
	old := webhookInstance("a", "old")
	old.Spec.Stop = ptr.To(true)
	old.Status.Phase = workerctrl.InstancePhaseStopped
	neu := webhookInstance("a", "new")
	neu.Spec.Stop = ptr.To(true)
	neu.Status.Phase = workerctrl.InstancePhaseStopped

	_, err := w.ValidateUpdate(context.Background(), old, neu)
	assert.NoError(t, err)
}

func TestInstanceWebhook_ValidateUpdate_RunningForbidsTypeChange(t *testing.T) {
	w := newInstanceWebhook()
	old := webhookInstance("a", "old")
	old.Status.Phase = workerctrl.InstancePhaseReady
	neu := webhookInstance("a", "new")
	neu.Status.Phase = workerctrl.InstancePhaseReady

	_, err := w.ValidateUpdate(context.Background(), old, neu)
	assert.Error(t, err)
}

func TestInstanceWebhook_ValidateUpdate_StartStoppedRequiresExistingType(t *testing.T) {
	w := newInstanceWebhook() // no InstanceType registered
	old := webhookInstance("a", "gone")
	old.Spec.Stop = ptr.To(true)
	old.Status.Phase = workerctrl.InstancePhaseStopped
	neu := webhookInstance("a", "gone")
	neu.Spec.Stop = ptr.To(false)
	neu.Status.Phase = workerctrl.InstancePhaseStopped

	_, err := w.ValidateUpdate(context.Background(), old, neu)
	assert.Error(t, err)
}

func TestInstanceWebhook_ValidateUpdate_StartStoppedWithExistingTypeAllowed(t *testing.T) {
	it := &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "live"}}
	w := newInstanceWebhook(it)
	old := webhookInstance("a", "live")
	old.Spec.Stop = ptr.To(true)
	old.Status.Phase = workerctrl.InstancePhaseStopped
	neu := webhookInstance("a", "live")
	neu.Spec.Stop = ptr.To(false)
	neu.Status.Phase = workerctrl.InstancePhaseStopped

	_, err := w.ValidateUpdate(context.Background(), old, neu)
	assert.NoError(t, err)
}

func TestInstanceWebhook_Default_FreshRequiresType(t *testing.T) {
	w := newInstanceWebhook()
	inst := webhookInstance("a", "missing") // Phase "", not stopped

	err := w.Default(context.Background(), inst)
	assert.Error(t, err)
}

func TestInstanceWebhook_Default_StoppedSkipsType(t *testing.T) {
	w := newInstanceWebhook()
	inst := webhookInstance("a", "missing")
	inst.Spec.Stop = ptr.To(true)

	err := w.Default(context.Background(), inst)
	assert.NoError(t, err)
}

func TestInstanceWebhook_Default_RunningUpdateSkipsType(t *testing.T) {
	w := newInstanceWebhook()
	inst := webhookInstance("a", "missing")
	inst.Status.Phase = workerctrl.InstancePhaseReady

	err := w.Default(context.Background(), inst)
	assert.Error(t, err)
}
