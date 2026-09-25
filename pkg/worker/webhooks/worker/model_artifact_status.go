package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	authn "k8s.io/api/authentication/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/system"
)

// modelArtifactStatusRuleWriter names the rule that refuses a status write from anyone but the
// worker, so a refusal says which rule fired.
const modelArtifactStatusRuleWriter = "only the worker writes a ModelArtifact's status"

// validateModelArtifactStatus admits an update of the status subresource only from the worker's
// own identity.
//
// A node mounts a tenant's weights on the strength of this status: an artifact resolved in the Pod's
// namespace, with this UID and this digest. A tenant who could write it could claim any digest on
// the node. No role this chart installs grants tenants the subresource, but a broad role an
// administrator grants would, silently, so the rule is enforced here rather than assumed from RBAC.
func (r *ModelArtifactWebhook) validateModelArtifactStatus(ctx context.Context, ma *workercore.ModelArtifact) error {
	req, err := ctrladmission.RequestFromContext(ctx)
	if err != nil {
		return kerrors.NewInternalError(fmt.Errorf("read the admission request: %w", err))
	}
	if r.worker == "" || req.UserInfo.Username != r.worker {
		return kerrors.NewInvalid(workercore.SchemeGroupVersionKind("ModelArtifact").GroupKind(), ma.Name,
			field.ErrorList{field.Forbidden(field.NewPath("status"), fmt.Sprintf(
				"%s: %q is not the worker", modelArtifactStatusRuleWriter, req.UserInfo.Username))})
	}

	return nil
}

// isStatusRequest reports whether ctx carries an admission request for the status subresource.
func isStatusRequest(ctx context.Context) bool {
	req, err := ctrladmission.RequestFromContext(ctx)
	return err == nil && req.SubResource == "status"
}

// selfUsername is the identity the worker's own requests carry, asked of the API server, so the
// answer holds whether the worker runs with a ServiceAccount or with a kubeconfig user.
func selfUsername(ctx context.Context) (string, error) {
	return learnUsername(ctx, system.LoopbackKubeClient.Get(), loopbackBearerToken)
}

// learnUsername asks the API server who cli is. SelfSubjectReview is served from Kubernetes 1.28;
// an older server is asked with a TokenReview of the bearer token cli sends instead, which answers
// the same authenticated username. Without either there is no writer the status rule could admit,
// so the error stops the worker rather than leave the rule off.
func learnUsername(ctx context.Context, cli kubernetes.Interface, token func() (string, error)) (string, error) {
	review, err := cli.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authn.SelfSubjectReview{}, meta.CreateOptions{})
	switch {
	case err == nil:
		if review.Status.UserInfo.Username == "" {
			return "", errors.New("self subject review answered no username")
		}
		return review.Status.UserInfo.Username, nil
	case !kerrors.IsNotFound(err):
		return "", fmt.Errorf("self subject review: %w", err)
	}

	t, err := token()
	if err != nil {
		return "", fmt.Errorf("the API server serves no SelfSubjectReview (Kubernetes 1.28 and later), "+
			"and a TokenReview needs the worker's bearer token: %w", err)
	}
	tr, err := cli.AuthenticationV1().TokenReviews().Create(ctx,
		&authn.TokenReview{Spec: authn.TokenReviewSpec{Token: t}}, meta.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	if !tr.Status.Authenticated || tr.Status.User.Username == "" {
		return "", fmt.Errorf("token review did not authenticate the worker's own token: %s", tr.Status.Error)
	}

	return tr.Status.User.Username, nil
}

// loopbackBearerToken is the bearer token the loopback client sends: the file's current content
// when it reads one, as a ServiceAccount token file is rotated.
func loopbackBearerToken() (string, error) {
	cfg := system.LoopbackKubeRestConfig.Get()
	if cfg.BearerTokenFile != "" {
		b, err := os.ReadFile(cfg.BearerTokenFile)
		if err != nil {
			return "", err
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	if cfg.BearerToken != "" {
		return cfg.BearerToken, nil
	}

	return "", errors.New("the worker's client authenticates without a bearer token")
}
