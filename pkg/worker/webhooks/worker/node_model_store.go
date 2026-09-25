package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/webhook"
)

// The rules that refuse a NodeModelStore status write, named in the refusal so a test and a reader
// can tell which one fired.
const (
	nodeModelStoreRuleIdentity = "only the model-manager plugin writes a NodeModelStore's status"
	nodeModelStoreRuleBinding  = "the plugin's token must be bound to its Pod"
	nodeModelStoreRuleNode     = "the plugin writes only the status of the node its Pod runs on"
)

// NodeModelStoreWebhook admits a NodeModelStore status write only from the model-manager plugin Pod
// running on the node the object is named after.
//
// RBAC CANNOT DO THIS. The plugin's role must allow updating nodemodelstores/status, and a role
// cannot narrow an update to one object name, so without this every plugin could write every node's
// status. The API server places the Pod a bound service-account token belongs to in the request's
// user extra, as its name and UID, on every version this operator supports; the Pod's node is then
// one read away. From Kubernetes 1.30 the extra also names the node, and that is compared directly.
// A token without those extras, a legacy one, is refused, so the rule never silently weakens.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="nodemodelstores",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:validating:subResources=["status"]
type NodeModelStoreWebhook struct {
	webhook.DefaultValidator

	// Reader reads the plugin's Pod.
	Reader ctrlcli.Reader
	// Namespace and ServiceAccount are where the plugin runs and what it runs as.
	Namespace      string
	ServiceAccount string
}

func (r *NodeModelStoreWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Reader = opts.Manager.GetAPIReader()
	r.Namespace = systemname.NamespaceName
	r.ServiceAccount = system.ModelManagerServiceAccount.Get()

	return &workercore.NodeModelStore{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*NodeModelStoreWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*NodeModelStoreWebhook)(nil)
)

// ReceiveDeletionUpdate keeps the rule in force while an object drains. It has no defaulting, and a
// main-resource update is admitted without a read, so no deletion can make it refuse one.
func (*NodeModelStoreWebhook) ReceiveDeletionUpdate() {}

func (r *NodeModelStoreWebhook) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (ctrladmission.Warnings, error) {
	if !isStatusRequest(ctx) {
		// The worker writes spec; the object's lifetime and spec are not this rule's.
		return nil, nil
	}
	nms := newObj.(*workercore.NodeModelStore)
	if rule, msg, err := r.statusWriter(ctx, nms.Name); err != nil || rule != "" {
		if err != nil {
			return nil, kerrors.NewInternalError(err)
		}
		return nil, kerrors.NewInvalid(workercore.SchemeGroupVersionKind("NodeModelStore").GroupKind(), nms.Name,
			field.ErrorList{field.Forbidden(field.NewPath("status"), rule+": "+msg)})
	}

	return nil, nil
}

// statusWriter returns the rule the request breaks and why, or "" when it is the plugin on node.
func (r *NodeModelStoreWebhook) statusWriter(ctx context.Context, node string) (rule, msg string, err error) {
	req, err := ctrladmission.RequestFromContext(ctx)
	if err != nil {
		return "", "", fmt.Errorf("read the admission request: %w", err)
	}
	user := req.UserInfo

	want := serviceaccount.MakeUsername(r.Namespace, r.ServiceAccount)
	if user.Username != want {
		return nodeModelStoreRuleIdentity, fmt.Sprintf("%q is not %q", user.Username, want), nil
	}

	if names := user.Extra[serviceaccount.NodeNameKey]; len(names) == 1 {
		if names[0] != node {
			return nodeModelStoreRuleNode, fmt.Sprintf("the token's node is %q, not %q", names[0], node), nil
		}
		return "", "", nil
	}

	podNames, podUIDs := user.Extra[serviceaccount.PodNameKey], user.Extra[serviceaccount.PodUIDKey]
	if len(podNames) != 1 || len(podUIDs) != 1 {
		return nodeModelStoreRuleBinding, "the token names no Pod", nil
	}
	pod := new(core.Pod)
	if err := r.Reader.Get(ctx, ctrlcli.ObjectKey{Namespace: r.Namespace, Name: podNames[0]}, pod); err != nil {
		if kerrors.IsNotFound(err) {
			return nodeModelStoreRuleBinding, fmt.Sprintf("the token's Pod %q does not exist", podNames[0]), nil
		}
		return "", "", fmt.Errorf("read the plugin's Pod %q: %w", podNames[0], err)
	}
	switch {
	case string(pod.UID) != podUIDs[0]:
		return nodeModelStoreRuleBinding, fmt.Sprintf("the token's Pod %q is not the one it was issued to", podNames[0]), nil
	case pod.Spec.NodeName != node:
		return nodeModelStoreRuleNode, fmt.Sprintf("the token's Pod %q runs on %q, not %q", pod.Name, pod.Spec.NodeName, node), nil
	}

	return "", "", nil
}
