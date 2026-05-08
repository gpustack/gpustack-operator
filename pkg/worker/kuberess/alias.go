package kuberess

import (
	"context"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/systemkuberess"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

var (
	// SystemNamespaceName is the namespace name of the system resources.
	SystemNamespaceName = systemname.NamespaceName

	// SystemToolkitNamespaceName is the namespace name of the system toolkit resources.
	SystemToolkitNamespaceName = systemname.ToolkitNamespaceName

	// SystemRoutingServiceName is the service name of the system routing service.
	SystemRoutingServiceName string
)

func init() {
	SystemRoutingServiceName = osx.Getenv("KUBERNETES_SERVICE_NAME", "gpustack-operator-worker")
}

// InstallSystemNamespace creates the system namespace.
func InstallSystemNamespace(ctx context.Context, cli kubernetes.Interface) error {
	return systemkuberess.InstallSystemNamespace(ctx, cli, SystemNamespaceName)
}

// InstallFakeSystemRoutingService creates the fake routing service/endpoint for system.
//
// The service points to the SelfIP of the system.
func InstallFakeSystemRoutingService(ctx context.Context, cli kubernetes.Interface, port int32) error {
	return systemkuberess.InstallFakeSystemRoutingService(ctx, cli, SystemNamespaceName, SystemRoutingServiceName, port)
}
