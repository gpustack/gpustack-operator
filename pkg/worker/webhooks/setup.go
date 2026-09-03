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
	new(worker.InstanceTypeWebhook),
	new(worker.KVCacheBackendWebhook),
	new(worker.KVCachePoolWebhook),
	new(worker.KVCachePoolBindingWebhook),
	new(worker.PodWebhook),
	new(worker.PodKVCacheWebhook),
}

// cfgGetters is the list of all webhook configuration getters.
var cfgGetters = []webhook.ConfigurationsGetter{
	worker.GetWebhookConfigurations,
}

// configurationPrefix becomes the webhook configuration object names,
// "<prefix>-mutation" and "<prefix>-validation". The API server runs mutating
// webhooks serially in lexicographic order of the configuration object name,
// so this prefix must sort before "kueue-mutating-webhook-configuration": our
// mutating webhook folds the sliced memory request into .sliced.units and
// Kueue's Pod webhook then hashes the container resources into a role
// annotation, so ours must run first. "gpustack-worker" sorts before "kueue-"
// ('g' < 'k'); a prefix at or after "kueue-" would silently reverse the order.
const configurationPrefix = "gpustack-worker"

// Setup registers the webhook API to the given manager and HTTP mux.
func Setup(ctx context.Context, mgr ctrl.Manager, mux webhook.HTTPServeMux) error {
	return webhook.ExecuteSetup(ctx, mgr, mux, setups)
}

// Install installs the webhook configurations to the Kubernetes cluster.
func Install(ctx context.Context, cli kubernetes.Interface, cc admreg.WebhookClientConfig) error {
	return webhook.InstallConfigurations(ctx, configurationPrefix, cli, cc, cfgGetters)
}

// Delete deletes the webhook configurations from the Kubernetes cluster.
func Delete(ctx context.Context, cli kubernetes.Interface) error {
	return webhook.DeleteConfigurations(ctx, configurationPrefix, cli)
}
