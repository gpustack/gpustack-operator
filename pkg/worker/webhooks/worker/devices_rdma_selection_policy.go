package worker

import (
	"context"
	"strings"
	"unicode"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/webhook"
)

// DevicesRDMASelectionPolicyWebhook validates the administrator HCA filter syntax.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="devicesrdmaselectionpolicies",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type DevicesRDMASelectionPolicyWebhook struct{}

func (*DevicesRDMASelectionPolicyWebhook) SetupWebhook(_ context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	return &workercore.DevicesRDMASelectionPolicy{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*DevicesRDMASelectionPolicyWebhook)(nil)

func (*DevicesRDMASelectionPolicyWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	policy := obj.(*workercore.DevicesRDMASelectionPolicy)
	if errs := validateDevicesRDMASelectionPolicy(policy); len(errs) > 0 {
		return nil, kerrors.NewInvalid(policy.GroupVersionKind().GroupKind(), policy.Name, errs)
	}
	return nil, nil
}

func (*DevicesRDMASelectionPolicyWebhook) ValidateUpdate(
	_ context.Context, _, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	policy := newObj.(*workercore.DevicesRDMASelectionPolicy)
	if errs := validateDevicesRDMASelectionPolicy(policy); len(errs) > 0 {
		return nil, kerrors.NewInvalid(policy.GroupVersionKind().GroupKind(), policy.Name, errs)
	}
	return nil, nil
}

func (*DevicesRDMASelectionPolicyWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (ctrladmission.Warnings, error) {
	return nil, nil
}

// validateDevicesRDMASelectionPolicy checks selector syntax and exact HCA name syntax.
// It never requires the selected Nodes to be online or to exist.
func validateDevicesRDMASelectionPolicy(policy *workercore.DevicesRDMASelectionPolicy) field.ErrorList {
	specPath := field.NewPath("spec")
	var errs field.ErrorList

	if policy.Spec.NodeSelector == nil {
		errs = append(errs, field.Required(specPath.Child("nodeSelector"), "a nil selector is invalid; an empty selector matches every node"))
	} else if _, err := metav1.LabelSelectorAsSelector(policy.Spec.NodeSelector); err != nil {
		errs = append(errs, field.Invalid(specPath.Child("nodeSelector"), policy.Spec.NodeSelector, err.Error()))
	}

	errs = append(errs, validateRDMAHCANames(policy.Spec.Include, specPath.Child("include"))...)
	errs = append(errs, validateRDMAHCANames(policy.Spec.Exclude, specPath.Child("exclude"))...)

	// Include and Exclude are mutually exclusive: a policy may narrow candidates or remove
	// some, never both at once.
	if len(policy.Spec.Include) > 0 && len(policy.Spec.Exclude) > 0 {
		errs = append(errs, field.Invalid(
			specPath.Child("exclude"), policy.Spec.Exclude,
			"include and exclude are mutually exclusive: specify at most one of them",
		))
	}
	return errs
}

// validateRDMAHCANames rejects every name form that is not a bare kernel device name:
// empty, any whitespace character anywhere in the name, glob wildcards, the consumer
// filter prefixes, and port suffixes. Exact matching is the contract; mlx5_1 must never
// reach mlx5_10.
func validateRDMAHCANames(names []string, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	seen := make(map[string]struct{}, len(names))
	for i, name := range names {
		itemPath := path.Index(i)
		switch {
		case name == "":
			errs = append(errs, field.Invalid(itemPath, name, "must be an exact HCA name"))
		case strings.IndexFunc(name, unicode.IsSpace) >= 0:
			errs = append(errs, field.Invalid(itemPath, name, "must not contain whitespace"))
		case strings.ContainsAny(name, "*?[]"):
			errs = append(errs, field.Invalid(itemPath, name, "must not contain wildcard syntax"))
		case strings.ContainsAny(name, "=^!"):
			errs = append(errs, field.Invalid(itemPath, name, "must be a bare name without consumer filter prefixes"))
		case strings.Contains(name, ":"):
			errs = append(errs, field.Invalid(itemPath, name, "must be a whole device name without a port suffix"))
		}
		if _, dup := seen[name]; dup {
			errs = append(errs, field.Duplicate(itemPath, name))
		}
		seen[name] = struct{}{}
	}
	return errs
}
