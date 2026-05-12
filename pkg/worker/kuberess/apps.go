package kuberess

import (
	"context"

	"gpustack.ai/gpustack/pkg/kubeapp"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// installs is the list of application installers.
var installs = []kubeapp.Install{
	installKueue,
	installNodeFeatureDiscovery,
	installGPUStackDeviceManager,
}

// InstallApplications installs applications.
func InstallApplications(ctx context.Context, manufacturers []string) error {
	gvc := map[string]any{
		"ContainerRegistry":  funcx.NoError(settings.ContainerRegistry.ValueFromRemote(ctx)),
		"ContainerNamespace": funcx.NoError(settings.ContainerNamespace.ValueFromRemote(ctx)),
		"Manufacturers":      manufacturers,
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
