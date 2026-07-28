package kuberess

import (
	"context"
	"slices"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/systemkuberess"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

const (
	// CSIProvisionerNFS is the driver name of the bundled NFS CSI driver, as the
	// csi-driver-nfs subchart's "driver.name" declares it.
	CSIProvisionerNFS = "nfs.csi.gpustack.ai"
	// CSIProvisionerS3 is the driver name of the bundled S3 CSI driver, as the
	// csi-driver-s3 subchart's "driver.name" declares it.
	CSIProvisionerS3 = "s3.csi.gpustack.ai"
)

var (
	// SystemNamespaceName is the namespace name of the system resources.
	SystemNamespaceName = systemname.NamespaceName

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

// ReservedNamespaces is the list of namespaces that are reserved for system resources and should not be used for user workloads.
var _ReservedNamespaces = []string{
	SystemNamespaceName,
	"kube-system",
}

// IsReservedNamespace returns true if the given namespace is reserved for system resources.
func IsReservedNamespace(ns string) bool {
	return slices.Contains(_ReservedNamespaces, ns)
}
