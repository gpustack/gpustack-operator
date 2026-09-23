package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	core "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

const topologyWebhookMaxResponseBytes = 1 << 20

func (r *TopologySourceReconciler) fetchTopologyWebhook(ctx context.Context, source *workercore.TopologySource) (topologySnapshot, error) {
	webhook := source.Spec.Webhook
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	endpoint, err := url.Parse(webhook.URL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return topologySnapshot{}, fmt.Errorf("topology webhook URL must be an absolute HTTPS URL without user information or a fragment")
	}
	if (webhook.BearerTokenSecretRef == nil) == (webhook.TLSClientCertificateSecretRef == nil) {
		return topologySnapshot{}, fmt.Errorf("topology webhook requires exactly one credential reference")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if webhook.CABundleConfigMapRef != nil {
		ref := webhook.CABundleConfigMapRef
		configMap := new(core.ConfigMap)
		if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, configMap); err != nil {
			return topologySnapshot{}, fmt.Errorf("read webhook CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(configMap.Data["ca.crt"])) {
			return topologySnapshot{}, fmt.Errorf("webhook CA bundle is invalid")
		}
		tlsConfig.RootCAs = pool
	}
	request, err := httpx.NewGetRequestWithContext(ctx, endpoint.String())
	if err != nil {
		return topologySnapshot{}, fmt.Errorf("create webhook request: %w", err)
	}
	clientOptions := httpx.ClientOptions().WithTimeout(0)
	if webhook.BearerTokenSecretRef != nil {
		ref := webhook.BearerTokenSecretRef
		secret := new(core.Secret)
		if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
			return topologySnapshot{}, fmt.Errorf("read webhook bearer token: %w", err)
		}
		token := strings.TrimSpace(string(secret.Data["token"]))
		if token == "" {
			return topologySnapshot{}, fmt.Errorf("webhook bearer token is missing")
		}
		clientOptions.WithBearerAuth(token)
	}
	if webhook.TLSClientCertificateSecretRef != nil {
		ref := webhook.TLSClientCertificateSecretRef
		secret := new(core.Secret)
		if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
			return topologySnapshot{}, fmt.Errorf("read webhook client certificate: %w", err)
		}
		certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
		if err != nil {
			return topologySnapshot{}, fmt.Errorf("webhook client certificate is invalid")
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	// The request context carries the source's configured timeout, not httpx's default.
	client := httpx.Client(clientOptions.WithTransportOption(httpx.TransportOptions().WithoutProxy().WithTLSClientConfig(tlsConfig)))
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := client.Do(request)
	if err != nil {
		return topologySnapshot{}, fmt.Errorf("read topology webhook: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return topologySnapshot{}, fmt.Errorf("topology webhook returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, topologyWebhookMaxResponseBytes+1))
	if err != nil {
		return topologySnapshot{}, fmt.Errorf("read topology webhook body: %w", err)
	}
	if len(body) > topologyWebhookMaxResponseBytes {
		return topologySnapshot{}, fmt.Errorf("topology webhook response exceeds the size limit")
	}
	return decodeTopologySnapshot(body)
}

func (r *TopologySourceReconciler) reconcileTopologySourceWebhook(
	ctx context.Context,
	source *workercore.TopologySource,
	before workercore.TopologySourceStatus,
) (ctrl.Result, error) {
	requestContext, cancel := context.WithTimeout(ctx, source.Spec.Webhook.Timeout.Duration)
	defer cancel()
	snapshot, err := r.fetchTopologyWebhook(requestContext, source)
	if err != nil {
		result, updateErr := r.markTopologySourceWebhookInvalid(ctx, source, before, err)
		if updateErr == nil && (result.RequeueAfter == 0 || source.Spec.Webhook.PollInterval.Duration < result.RequeueAfter) {
			result.RequeueAfter = source.Spec.Webhook.PollInterval.Duration
		}
		return result, updateErr
	}
	result, err := r.applyTopologySourceSnapshot(ctx, source, before, snapshot)
	if err != nil {
		return result, err
	}
	pollInterval := source.Spec.Webhook.PollInterval.Duration
	if result.RequeueAfter == 0 || pollInterval < result.RequeueAfter {
		result.RequeueAfter = pollInterval
	}
	return result, nil
}

func (r *TopologySourceReconciler) markTopologySourceWebhookInvalid(
	ctx context.Context,
	source *workercore.TopologySource,
	before workercore.TopologySourceStatus,
	err error,
) (ctrl.Result, error) {
	return r.markTopologySourceSnapshotInvalid(
		ctx,
		source,
		before,
		"WebhookReadFailed",
		err,
		source.Spec.Webhook.MaxStaleness.Duration,
		"webhook snapshot",
	)
}
