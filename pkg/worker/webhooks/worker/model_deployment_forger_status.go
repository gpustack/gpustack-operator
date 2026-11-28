package worker

import (
	"context"
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// modelDeploymentForgerStatusRuleWriter names the rule that refuses a status write from anyone but
// the worker, so tenants cannot forge status.roles[*].activeSandbox.podUID.
const modelDeploymentForgerStatusRuleWriter = "only the worker writes a ModelDeploymentForger's status"

// validateModelDeploymentForgerStatus admits an update of the status subresource only from the
// worker's own identity.
func (r *ModelDeploymentForgerWebhook) validateModelDeploymentForgerStatus(
	ctx context.Context, mdf *workercore.ModelDeploymentForger,
) error {
	req, err := ctrladmission.RequestFromContext(ctx)
	if err != nil {
		return kerrors.NewInternalError(fmt.Errorf("read the admission request: %w", err))
	}
	if r.worker == "" || req.UserInfo.Username != r.worker {
		return kerrors.NewInvalid(
			workercore.SchemeGroupVersionKind("ModelDeploymentForger").GroupKind(), mdf.Name,
			field.ErrorList{field.Forbidden(field.NewPath("status"), fmt.Sprintf(
				"%s: %q is not the worker", modelDeploymentForgerStatusRuleWriter, req.UserInfo.Username,
			))},
		)
	}

	return nil
}
