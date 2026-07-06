package worker

import (
	"context"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/webhook"
)

// InstanceTypeWebhook validates the admin-writable unit spec of a v1alpha1.InstanceType.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="instancetypes",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type InstanceTypeWebhook struct{}

func (r *InstanceTypeWebhook) SetupWebhook(_ context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	return &workercore.InstanceType{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*InstanceTypeWebhook)(nil)

func (r *InstanceTypeWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, validateInstanceTypeUnitSpec(obj.(*workercore.InstanceType))
}

func (r *InstanceTypeWebhook) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (ctrladmission.Warnings, error) {
	return nil, validateInstanceTypeUnitSpec(newObj.(*workercore.InstanceType))
}

func (r *InstanceTypeWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

// validateInstanceTypeUnitSpec enforces the unit spec: all three fields must be set and
// well-formed — unitCPU a unitless positive integer, unitRAM and localStorage a positive
// integer with a case-sensitive "Gi" suffix. The operator stamps this complete triple on
// every derived InstanceType at creation, and an admin must provide it, so the reconciler
// never fills it and an InstanceType always carries a complete unit spec (an empty or
// partial spec is rejected here).
func validateInstanceTypeUnitSpec(it *workercore.InstanceType) error {
	cpu, ram, localStg := it.Spec.UnitResources.CPU, it.Spec.UnitResources.RAM, it.Spec.LocalStorage

	var errs field.ErrorList
	if extractPositiveNumberFromString(cpu) == "" {
		errs = append(errs, field.Invalid(field.NewPath("spec", "unitResources", "cpu"),
			cpu, "must be a positive integer with no unit suffix"))
	}
	if extractPositiveNumberFromQuantity(ram, "Gi") == "" {
		errs = append(errs, field.Invalid(field.NewPath("spec", "unitResources", "ram"),
			ram, "must be a positive integer with a case-sensitive \"Gi\" suffix"))
	}
	if extractPositiveNumberFromQuantity(localStg, "Gi") == "" {
		errs = append(errs, field.Invalid(field.NewPath("spec", "localStorage"),
			localStg, "must be a positive integer with a case-sensitive \"Gi\" suffix"))
	}
	if len(errs) > 0 {
		return kerrors.NewInvalid(workercore.Kind("InstanceType"), it.Name, errs)
	}
	return nil
}

// extractPositiveNumberFromString returns v unchanged when it is a positive integer
// carrying no unit suffix, and "" otherwise (including empty input).
func extractPositiveNumberFromString(v string) string {
	n, err := strconvx.ParseInt[int32](v, 10, 32)
	if err == nil && n > 0 {
		return v
	}
	return ""
}

// extractPositiveNumberFromQuantity strips the given suffix and returns the bare
// positive integer, or "" when the suffix is absent or the remainder is not a positive
// integer (including empty input).
func extractPositiveNumberFromQuantity(v, suffix string) string {
	b, ok := strings.CutSuffix(v, suffix)
	if ok {
		return extractPositiveNumberFromString(b)
	}
	return ""
}
