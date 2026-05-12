package kuberess

import (
	"context"

	"gpustack.ai/gpustack/pkg/kubeapp"
	"gpustack.ai/gpustack/pkg/server/settings"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

// installs is the list of application installers.
var installs []kubeapp.Install

// InstallApplications installs applications.
func InstallApplications(ctx context.Context) error {
	gvc := map[string]any{
		"ContainerRegistry":  funcx.NoError(settings.ContainerRegistry.ValueFromRemote(ctx)),
		"ContainerNamespace": funcx.NoError(settings.ContainerNamespace.ValueFromRemote(ctx)),
	}

	return kubeapp.ExecuteInstall(
		ctx,
		system.LoopbackKubeRestConfig.Get(),
		system.DisableApplications.Get(),
		SystemNamespaceName,
		installs,
		gvc,
	)
}
