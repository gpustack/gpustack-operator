package webhooks

import (
	"context"

	admreg "k8s.io/api/admissionregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/webhooks/worker"
)

// setups is the list of all webhook handlers.
var setups = []webhook.Setup{
	new(worker.InstanceWebhook),
}

// cfgGetters is the list of all webhook configuration getters.
var cfgGetters = []webhook.ConfigurationsGetter{
	worker.GetWebhookConfigurations,
}

// Setup registers the webhook API to the given manager and HTTP mux.
func Setup(ctx context.Context, mgr ctrl.Manager, mux webhook.HTTPServeMux) error {
	return webhook.ExecuteSetup(ctx, mgr, mux, setups)
}

// Install installs the webhook configurations to the Kubernetes cluster.
func Install(ctx context.Context, cli kubernetes.Interface, cc admreg.WebhookClientConfig) error {
	return webhook.InstallConfigurations(ctx, "gpustack-worker", cli, cc, cfgGetters)
}
