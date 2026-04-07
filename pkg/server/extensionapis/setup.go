package extensionapis

import (
	"context"

	genericapiserver "k8s.io/apiserver/pkg/server"
	ctrl "sigs.k8s.io/controller-runtime"

	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/server/extensionapis/server"
	"gpustack.ai/gpustack/pkg/server/settings"
)

// setups is the list of all extension api handlers.
var setups = []extensionapi.Setup{
	extensionapi.NewSettingHandler(settings.Indexer()),
	new(server.ClusterHandler),
	new(server.ProjectHandler),
	new(server.SubjectHandler),
	new(server.SubjectProviderHandler),
	new(server.TeamHandler),
}

// Setup installs the extension api handlers.
func Setup(
	ctx context.Context,
	srv *genericapiserver.GenericAPIServer,
	mgr ctrl.Manager,
) error {
	return extensionapi.ExecuteSetup(ctx, srv, Scheme, ParameterCodec, Codecs, mgr, setups)
}
