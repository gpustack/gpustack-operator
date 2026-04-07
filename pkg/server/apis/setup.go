package apis

import (
	"context"

	apireg "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	gpustack "gpustack.ai/gpustack/api/v1"
	"gpustack.ai/gpustack/pkg/api"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
)

var (
	apiCrdGetters = []api.CRDGetter{
		servercore.GetCustomResourceDefinitions,
	}
	apiSvcGetters = []api.ServiceGetter{
		server.GetAPIService,
		gpustack.GetAPIService,
	}
)

// InstallCRDs installs the custom resource definitions.
func InstallCRDs(ctx context.Context, cli kubernetes.Interface) error {
	return api.InstallCRDs(ctx, cli, apiCrdGetters)
}

// InstallServices installs the api services.
func InstallServices(ctx context.Context, cli kubernetes.Interface, svc apireg.ServiceReference, ca []byte) error {
	return api.InstallServices(ctx, cli, svc, ca, apiSvcGetters)
}

// WaitForServicesReady waits for the api services to be ready.
func WaitForServicesReady(ctx context.Context, cli kubernetes.Interface) error {
	return api.WaitForServicesReady(ctx, cli, apiSvcGetters)
}
