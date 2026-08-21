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

// EnsureServices keeps the api services installed until the given context is done.
func EnsureServices(ctx context.Context, cli kubernetes.Interface, svc apireg.ServiceReference, ca []byte) error {
	return api.EnsureServices(ctx, cli, svc, ca, apiSvcGetters)
}

// DeleteServicesBackedBy deletes the api services backed by a service in the given namespace —
// the ones this worker registered and the ones its sub-releases did (Kueue's visibility
// pair), which outlive the namespace alike when the deletion skips the chart's uninstall.
func DeleteServicesBackedBy(ctx context.Context, cli kubernetes.Interface, namespace string) error {
	return api.DeleteServicesBackedBy(ctx, cli, namespace)
}

// IsServicesReady reports whether the api services are ready, checking each of them once.
func IsServicesReady(ctx context.Context, cli kubernetes.Interface) error {
	return api.IsServicesReady(ctx, cli, apiSvcGetters)
}

// WaitForServicesReady waits for the api services to be ready.
func WaitForServicesReady(ctx context.Context, cli kubernetes.Interface) error {
	return api.WaitForServicesReady(ctx, cli, apiSvcGetters)
}
