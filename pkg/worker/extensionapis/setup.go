package extensionapis

import (
	"context"

	genericapiserver "k8s.io/apiserver/pkg/server"
	ctrl "sigs.k8s.io/controller-runtime"

	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/worker/extensionapis/worker"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// setups is the list of all extension api handlers.
//
// A v1 VIEW OF A CRD MUST SERVE DELETE. v1 is this group's preferred version, and the garbage
// collector watches each resource at the preferred version and only when that version can delete,
// list and watch it: a view without delete leaves every object of that kind whose owner is gone
// behind, such as a node's NodeModelStore after its Node is deleted (measured on Kubernetes 1.36).
// TestEveryCRDViewServesDelete enforces it.
var setups = []extensionapi.Setup{
	extensionapi.NewSettingHandler(settings.Indexer()),
	new(worker.DevicesHandler),
	new(worker.InstanceHandler),
	new(worker.ModelDeploymentHandler),
	new(worker.ModelArtifactHandler),
	new(worker.NodeModelStoreHandler),
	new(worker.InstanceImagePullSecretHandler),
	new(worker.InstancePersistentVolumeHandler),
	new(worker.InstancePersistentVolumeTypeHandler),
	new(worker.InstanceSSHPublicKeyHandler),
	new(worker.InstanceTypeHandler),
	new(worker.InstanceTypeFlavorHandler),
}

// Setup installs the extension api handlers.
func Setup(
	ctx context.Context,
	srv *genericapiserver.GenericAPIServer,
	mgr ctrl.Manager,
) error {
	return extensionapi.ExecuteSetup(ctx, srv, Scheme, ParameterCodec, Codecs, mgr, setups)
}
