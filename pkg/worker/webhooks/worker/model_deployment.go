package worker

import (
	"context"
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

// ModelDeploymentWebhook validates a v1alpha1.ModelDeployment.
//
// It is validating only. Every default this API has — the connector discriminator and the replica
// count — is a CRD schema default, and every enum is a CRD schema enum, so there is no mutating
// half. For the enums a webhook could not help even if one were written: structural schema
// validation runs before the validating admission chain, so a value outside an enum is refused
// before this handler is reached.
//
// What is left here is what a schema cannot express: a bound that must carry an actionable message,
// and a collision between what a user supplies and what the operator owns.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modeldeployments",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelDeploymentWebhook struct{}

func (r *ModelDeploymentWebhook) SetupWebhook(_ context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	// This handler holds no client: every rule below is answered from the object itself and from
	// the renderer's owned-key table. Resolving the referenced Binding is a separate rule that
	// arrives with the Binding's own type.
	return &workercore.ModelDeployment{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*ModelDeploymentWebhook)(nil)

func (r *ModelDeploymentWebhook) ValidateCreate(
	_ context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	md := obj.(*workercore.ModelDeployment)

	if errs := validateModelDeployment(md); len(errs) > 0 {
		return nil, kerrors.NewInvalid(md.GroupVersionKind().GroupKind(), md.Name, errs)
	}

	return nil, nil
}

func (r *ModelDeploymentWebhook) ValidateUpdate(
	_ context.Context, _, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	md := newObj.(*workercore.ModelDeployment)

	// There is nothing immutable to check. Unlike the Instance that shares InstanceTemplate, this
	// CR's template is mutable by design: dropping that rule is what makes a rollout possible.
	if errs := validateModelDeployment(md); len(errs) > 0 {
		return nil, kerrors.NewInvalid(md.GroupVersionKind().GroupKind(), md.Name, errs)
	}

	return nil, nil
}

func (r *ModelDeploymentWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	// Deletion is not refused here. What holds a deployment back is its own finalizer releasing the
	// Binding it holds, which needs the consumers this handler cannot see.
	return nil, nil
}

// validateModelDeployment holds every rule that applies to this version.
//
// It is split into the rules that OUTLIVE this version and the one bound that does not, so that the
// spec introducing P/D roles lifts the bound by deleting one call rather than by unpicking a
// function. A unit test calls validateModelDeploymentRoles alone with two roles and expects no
// error, which is what keeps that seam real rather than intended.
func validateModelDeployment(md *workercore.ModelDeployment) field.ErrorList {
	errs := validateModelDeploymentRoles(md)
	errs = append(errs, validateModelDeploymentSingleRole(md)...)

	return errs
}

// validateModelDeploymentSingleRole is the whole of this version's one-role restriction.
//
// The bound lives here rather than as a schema maxItems for two reasons: the refusal can name the
// spec that lifts it, which is the difference between a user filing a bug and a user reading a
// plan; and lifting it is a webhook edit rather than a CRD schema change every stored object would
// have to survive.
func validateModelDeploymentSingleRole(md *workercore.ModelDeployment) field.ErrorList {
	if len(md.Spec.Roles) <= 1 {
		return nil
	}

	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "roles"), len(md.Spec.Roles),
		"multiple roles are not supported by this version; P/D roles are introduced by the P/D "+
			"atomic admission spec (specs/*-pd-atomic-admission.md)",
	)}
}

// validateModelDeploymentRoles holds the per-role rules that every later version keeps.
func validateModelDeploymentRoles(md *workercore.ModelDeployment) field.ErrorList {
	errs := make(field.ErrorList, 0, len(md.Spec.Roles))

	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role, rolePath := &md.Spec.Roles[i], rolesPath.Index(i)

		errs = append(errs, validateModelDeploymentRoleExtraArgs(md.Spec.Engine, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleEnv(md.Spec.Engine, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleTemplate(role, rolePath)...)
	}

	return errs
}

// validateModelDeploymentRoleExtraArgs refuses an append-tier argument the operator owns.
//
// A silent merge is what this prevents, and the reason is diagnosability rather than tidiness: two
// values for one connector argument leave no way to tell which one won, and the user who wrote the
// second has no way to learn the first exists.
func validateModelDeploymentRoleExtraArgs(
	engine string, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	argsPath := rolePath.Child("extraArgs")
	for i, arg := range role.ExtraArgs {
		name := workerctrl.ModelDeploymentArgName(arg)
		if !workerctrl.ModelDeploymentOwnsArg(engine, name) {
			continue
		}

		errs = append(errs, field.Invalid(argsPath.Index(i), arg, fmt.Sprintf(
			"%q is set by the operator for engine %q and must not be supplied here, because two "+
				"values for it cannot be told apart; replace the whole command line through "+
				"%s to own it instead",
			name, engine, rolePath.Child("template", "command"),
		)))
	}

	return errs
}

// validateModelDeploymentRoleEnv refuses an append-tier environment entry the operator owns.
//
// Ownership here is about what a key destroys rather than what it duplicates: the config-path
// variable is the only pointer to the file the operator wrote, so re-pointing it swaps the entire
// client configuration for another file's and moves every symptom one layer away from its cause.
// Keys the operator merely defaults are not owned, so a user's value wins there with no refusal.
func validateModelDeploymentRoleEnv(
	engine string, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	envPath := rolePath.Child("env")
	for i := range role.Env {
		name := role.Env[i].Name
		if !workerctrl.ModelDeploymentOwnsEnv(engine, name) {
			continue
		}

		errs = append(errs, field.Invalid(envPath.Index(i), name, fmt.Sprintf(
			"%q is set by the operator for engine %q and must not be supplied here, because it "+
				"selects the client configuration the operator rendered; replace the whole "+
				"command line through %s to own it instead",
			name, engine, rolePath.Child("template", "command"),
		)))
	}

	return errs
}

// validateModelDeploymentRoleTemplate keeps the scheduling scalars out of the overlay tier.
//
// The template may override container content and never the resource request. Inferring the request
// from container content would make the admission feasibility check read a ledger that does not
// match reality, so the refusal names the field that does decide it.
func validateModelDeploymentRoleTemplate(
	role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	if role.Template == nil || role.Template.Resources == nil {
		return nil
	}

	return field.ErrorList{field.Invalid(
		rolePath.Child("template", "resources"), role.Template.Resources,
		fmt.Sprintf(
			"resources are decided by %s, which admission and scheduling read; a template that "+
				"could shadow them would make the feasibility check read a ledger that does not "+
				"match reality",
			rolePath.Child("instanceType"),
		),
	)}
}
