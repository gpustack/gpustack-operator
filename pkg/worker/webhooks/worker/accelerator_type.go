package worker

import (
	"context"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// AcceleratorTypeWebhook defaults and validates a v1alpha1.AcceleratorType.
//
// The defaulting webhook stamps only the schedule discriminator and entrance labels; the observed
// hardware descriptor is computed by the reconciler into Status.Detail, not enriched onto the spec
// at admission. The validating webhook requires the admin-writable inputs on create and freezes the
// spec on update.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="acceleratortypes",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:group="worker.gpustack.ai",version="v1alpha1",resource="acceleratortypes",scope="Cluster"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type AcceleratorTypeWebhook struct{}

func (r *AcceleratorTypeWebhook) SetupWebhook(_ context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	return &workercore.AcceleratorType{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*AcceleratorTypeWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*AcceleratorTypeWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*AcceleratorTypeWebhook)(nil)
)

// ReceiveDeletionUpdate keeps this webhook validating updates to an AcceleratorType that is being
// deleted. validateAcceleratorTypeSpecImmutable is a masked equality on the two specs and reads
// nothing else, so the spec stays frozen while the reconciler drains the type's ClusterQueue rather
// than only while the type is live.
func (r *AcceleratorTypeWebhook) ReceiveDeletionUpdate() {}

func (r *AcceleratorTypeWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	at := obj.(*workercore.AcceleratorType)

	if errs := validateAcceleratorTypeSpec(at); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("AcceleratorType"), at.Name, errs)
	}
	return nil, nil
}

func (r *AcceleratorTypeWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (ctrladmission.Warnings, error) {
	atOld, at := oldObj.(*workercore.AcceleratorType), newObj.(*workercore.AcceleratorType)

	if errs := validateAcceleratorTypeSpecImmutable(atOld, at); len(errs) > 0 {
		return nil, kerrors.NewInvalid(workercore.Kind("AcceleratorType"), atOld.Name, errs)
	}
	return nil, nil
}

func (r *AcceleratorTypeWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

// Default stamps the AcceleratorType's metadata labels so it is selectable from day one. From the
// spec identity it derives the acceleratable feature key and, with kubernetes.io/os|arch, stamps
// the schedule discriminators the pool's Devices and ResourceFlavors carry (pruning a stale
// feature key left by a group change), plus the entrance label the Pod webhook reverse-looks-up.
func (r *AcceleratorTypeWebhook) Default(ctx context.Context, obj runtime.Object) error {
	at := obj.(*workercore.AcceleratorType)

	if at.Spec.GeneralGroup == "" {
		at.Spec.GeneralGroup = nodefeature.GeneralGroupGeneric
	}
	if at.Spec.AcceleratorGroup == "" {
		return nil
	}

	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx)
	sched := nodefeature.PoolScheduleLabels(
		true, cpuAware,
		at.Spec.GeneralGroup, at.Spec.AcceleratorGroup,
		at.Spec.OS, at.Spec.Arch,
	)

	if at.Labels == nil {
		at.Labels = make(map[string]string, len(sched)+1)
	}
	for k := range at.Labels {
		if _, want := sched[k]; want {
			continue
		}
		if strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
			strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix) {
			delete(at.Labels, k)
		}
	}
	for k, v := range sched {
		at.Labels[k] = v
	}
	at.Labels[QueueEntranceLabelKey] = nodefeature.FormatLocalQueueName(workerctrl.AcceleratorTypePoolQueueName(ctx, at))

	return nil
}

func validateAcceleratorTypeSpec(at *workercore.AcceleratorType) field.ErrorList {
	var errs field.ErrorList
	if at.Spec.AcceleratorGroup == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "acceleratorGroup"), "must be specified"))
	}
	if at.Spec.OS == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "os"), "must be specified"))
	}
	if at.Spec.Arch == "" {
		errs = append(errs, field.Required(field.NewPath("spec", "arch"), "must be specified"))
	}
	return errs
}

func validateAcceleratorTypeSpecImmutable(atOld, at *workercore.AcceleratorType) field.ErrorList {
	oldSpec, newSpec := atOld.Spec, at.Spec
	oldSpec.DisplayName, newSpec.DisplayName = "", ""
	oldSpec.Description, newSpec.Description = "", ""
	oldSpec.Inactive, newSpec.Inactive = false, false
	if oldSpec != newSpec {
		return field.ErrorList{field.Forbidden(field.NewPath("spec"),
			"is immutable except displayName, description and inactive")}
	}
	return nil
}
