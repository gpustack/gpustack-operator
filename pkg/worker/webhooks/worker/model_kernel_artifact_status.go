package worker

import (
	"context"
	"fmt"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// modelKernelArtifactStatusRuleWriter names the rule that refuses a status write from anyone but
// the worker, so a refusal names the rule that fired.
const modelKernelArtifactStatusRuleWriter = "only the worker writes a ModelKernelArtifact's status"

// validateModelKernelArtifactStatus admits an update of the status subresource only from the
// worker's own identity so tenants cannot forge Resolved or clear Quarantined state.
func (r *ModelKernelArtifactWebhook) validateModelKernelArtifactStatus(
	ctx context.Context, mka *workercore.ModelKernelArtifact,
) error {
	req, err := ctrladmission.RequestFromContext(ctx)
	if err != nil {
		return kerrors.NewInternalError(fmt.Errorf("read the admission request: %w", err))
	}
	if r.worker == "" || req.UserInfo.Username != r.worker {
		return kerrors.NewInvalid(
			workercore.SchemeGroupVersionKind("ModelKernelArtifact").GroupKind(), mka.Name,
			field.ErrorList{field.Forbidden(field.NewPath("status"), fmt.Sprintf(
				"%s: %q is not the worker", modelKernelArtifactStatusRuleWriter, req.UserInfo.Username,
			))},
		)
	}

	return nil
}
