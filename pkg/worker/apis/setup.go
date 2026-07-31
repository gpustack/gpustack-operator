package apis

import (
	"context"

	apireg "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/api"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
)

var (
	apiCrdGetters = []api.CRDGetter{
		workercore.GetCustomResourceDefinitions,
	}
	apiSvcGetters = []api.ServiceGetter{
		worker.GetAPIService,
		gpustack.GetAPIService,
	}
)

// InstallCRDs installs the custom resource definitions.
func InstallCRDs(ctx context.Context, cli kubernetes.Interface) error {
	return api.InstallCRDs(ctx, cli, apiCrdGetters)
}

// EnsureCRDs keeps the custom resource definitions installed until the given context is done.
func EnsureCRDs(ctx context.Context, cli kubernetes.Interface) error {
	return api.EnsureCRDs(ctx, cli, apiCrdGetters)
}

// InstallServices installs the api services.
func InstallServices(ctx context.Context, cli kubernetes.Interface, svc apireg.ServiceReference, ca []byte) error {
	return api.InstallServices(ctx, cli, svc, ca, apiSvcGetters)
}

// IsServicesReady reports whether the api services are ready, checking each of them once.
func IsServicesReady(ctx context.Context, cli kubernetes.Interface) error {
	return api.IsServicesReady(ctx, cli, apiSvcGetters)
}

// WaitForServicesReady waits for the api services to be ready.
func WaitForServicesReady(ctx context.Context, cli kubernetes.Interface) error {
	return api.WaitForServicesReady(ctx, cli, apiSvcGetters)
}
