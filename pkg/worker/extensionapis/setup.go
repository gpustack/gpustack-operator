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
var setups = []extensionapi.Setup{
	extensionapi.NewSettingHandler(settings.Indexer()),
	new(worker.DevicesHandler),
	new(worker.InstanceHandler),
	new(worker.InstanceImagePullSecretHandler),
	new(worker.InstancePersistentVolumeHandler),
	new(worker.InstancePersistentVolumeTypeHandler),
	new(worker.InstanceSSHPublicKeyHandler),
	new(worker.InstanceTypeHandler),
}

// Setup installs the extension api handlers.
func Setup(
	ctx context.Context,
	srv *genericapiserver.GenericAPIServer,
	mgr ctrl.Manager,
) error {
	return extensionapi.ExecuteSetup(ctx, srv, Scheme, ParameterCodec, Codecs, mgr, setups)
}
