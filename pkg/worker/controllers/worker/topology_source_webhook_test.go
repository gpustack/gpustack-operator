package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

func TestFetchTopologyWebhookUsesConfiguredCAAndBearerToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer inventory-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"inventory-1","nodes":{}}`))
	}))
	defer server.Close()
	certificate := server.Certificate()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	ref := workercore.TopologySourceObjectReference{Namespace: "gpustack-system", Name: "credential"}
	source := &workercore.TopologySource{Spec: workercore.TopologySourceSpec{Webhook: &workercore.TopologySourceWebhook{
		URL: server.URL, CABundleConfigMapRef: &workercore.TopologySourceObjectReference{Namespace: ref.Namespace, Name: "ca"}, BearerTokenSecretRef: &ref,
	}}}
	caConfigMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: ref.Namespace, Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
	secret := &core.Secret{ObjectMeta: meta.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name}, Data: map[string][]byte{"token": []byte("inventory-token")}}
	r := &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(caConfigMap, secret).Build()}

	snapshot, err := r.fetchTopologyWebhook(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, "inventory-1", snapshot.Revision)
}

func TestFetchTopologyWebhookUsesMutualTLS(t *testing.T) {
	clientCertificate, clientCertificatePEM, clientKeyPEM := newClientCertificate(t)
	clientPool := x509.NewCertPool()
	clientPool.AddCert(clientCertificate)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"mtls-1","nodes":{}}`))
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientPool}
	server.StartTLS()
	defer server.Close()

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	ref := workercore.TopologySourceObjectReference{Namespace: "gpustack-system", Name: "client"}
	source := webhookTopologySource(server.URL)
	source.Spec.Webhook.CABundleConfigMapRef = &workercore.TopologySourceObjectReference{Namespace: ref.Namespace, Name: "ca"}
	source.Spec.Webhook.BearerTokenSecretRef = nil
	source.Spec.Webhook.TLSClientCertificateSecretRef = &ref
	caConfigMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: ref.Namespace, Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
	secret := &core.Secret{ObjectMeta: meta.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name}, Data: map[string][]byte{"tls.crt": clientCertificatePEM, "tls.key": clientKeyPEM}}
	r := &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(caConfigMap, secret).Build()}

	snapshot, err := r.fetchTopologyWebhook(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, "mtls-1", snapshot.Revision)
}

func TestFetchTopologyWebhookRejectsRedirectAndOversizedResponse(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.Handler
		wantError string
	}{
		{name: "redirect", handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			http.Redirect(w, request, "/other", http.StatusFound)
		}), wantError: "topology webhook returned HTTP 302"},
		{name: "oversized", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.CopyN(w, zeroReader{}, topologyWebhookMaxResponseBytes+1)
		}), wantError: "topology webhook response exceeds the size limit"},
		{name: "client error", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "private response", http.StatusBadRequest) }), wantError: "topology webhook returned HTTP 400"},
		{name: "server error", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "private response", http.StatusInternalServerError)
		}), wantError: "topology webhook returned HTTP 500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				tc.handler.ServeHTTP(w, request)
			}))
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			source := webhookTopologySource(server.URL)
			configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
			r := &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(configMap, webhookTokenSecret("inventory-token")).Build()}
			_, err := r.fetchTopologyWebhook(context.Background(), source)
			require.ErrorContains(t, err, tc.wantError)
			assert.NotContains(t, err.Error(), "private response")
			assert.EqualValues(t, 1, requests.Load(), "the webhook response must be reached")
		})
	}
}

func TestFetchTopologyWebhookRejectsBadCertificateAndHonorsDeadline(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.Handler
		configure func(*workercore.TopologySource, *core.ConfigMap)
		timeout   time.Duration
	}{
		{
			name: "bad certificate",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"bad-ca","nodes":{}}`))
			}),
			configure: func(source *workercore.TopologySource, _ *core.ConfigMap) {
				source.Spec.Webhook.CABundleConfigMapRef = nil
			},
			timeout: time.Second,
		},
		{
			name: "deadline",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(100 * time.Millisecond)
				_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"late","nodes":{}}`))
			}),
			timeout: 10 * time.Millisecond,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tc.handler)
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			source := webhookTopologySource(server.URL)
			configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
			secret := webhookTokenSecret("inventory-token")
			if tc.configure != nil {
				tc.configure(source, configMap)
			}
			r := &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(configMap, secret).Build()}
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			_, err := r.fetchTopologyWebhook(ctx, source)
			require.Error(t, err)
		})
	}
}

func TestFetchTopologyWebhookReloadsBearerToken(t *testing.T) {
	var expected atomic.Value
	expected.Store("first-token")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+expected.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"rotated","nodes":{}}`))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
	secret := webhookTokenSecret("first-token")
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(configMap, secret).Build()
	r := &TopologySourceReconciler{Client: cli}
	source := webhookTopologySource(server.URL)

	_, err := r.fetchTopologyWebhook(context.Background(), source)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(secret), secret))
	secret.Data["token"] = []byte("second-token")
	require.NoError(t, cli.Update(context.Background(), secret))
	expected.Store("second-token")
	_, err = r.fetchTopologyWebhook(context.Background(), source)
	require.NoError(t, err)
}

func TestFetchTopologyWebhookReadsCredentialFromAPI(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer current-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"current","nodes":{}}`))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
	stale := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(configMap, webhookTokenSecret("stale-token")).Build()
	current := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(configMap, webhookTokenSecret("current-token")).Build()
	r := &TopologySourceReconciler{Client: stale, APIReader: current}

	snapshot, err := r.fetchTopologyWebhook(context.Background(), webhookTopologySource(server.URL))
	require.NoError(t, err)
	assert.Equal(t, "current", snapshot.Revision)
}

func TestFetchTopologyWebhookDoesNotExposeCredentialOrResponseBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private-inventory-body", http.StatusUnauthorized)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
	secret := webhookTokenSecret("private-bearer-token")
	r := &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(configMap, secret).Build()}

	_, err := r.fetchTopologyWebhook(context.Background(), webhookTopologySource(server.URL))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private-inventory-body")
	assert.NotContains(t, err.Error(), "private-bearer-token")

	secret.Data["token"] = nil
	r = &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(configMap, secret).Build()}
	_, err = r.fetchTopologyWebhook(context.Background(), webhookTopologySource(server.URL))
	require.EqualError(t, err, "webhook bearer token is missing")
}

func TestFetchTopologyWebhookRejectsCredentialCardinality(t *testing.T) {
	ref := &workercore.TopologySourceObjectReference{Namespace: "gpustack-system", Name: "credential"}
	tests := []struct {
		name   string
		bearer *workercore.TopologySourceObjectReference
		mtls   *workercore.TopologySourceObjectReference
	}{
		{name: "none"},
		{name: "both", bearer: ref, mtls: ref},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := webhookTopologySource("https://inventory.example.test")
			source.Spec.Webhook.BearerTokenSecretRef = tc.bearer
			source.Spec.Webhook.TLSClientCertificateSecretRef = tc.mtls
			r := &TopologySourceReconciler{Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()}

			_, err := r.fetchTopologyWebhook(context.Background(), source)
			require.EqualError(t, err, "topology webhook requires exactly one credential reference")
		})
	}
}

func TestTopologySourceWebhookStalenessExpiryAndRecovery(t *testing.T) {
	var status atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code := int(status.Load()); code != 0 {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"inventory-1","nodes":{"node-a":{"topology.gpustack.ai/rack":"rack-a"}}}`))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	source := webhookTopologySource(server.URL)
	source.ObjectMeta = meta.ObjectMeta{Name: "inventory", UID: "source"}
	source.Spec.NodeSelector = meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/topology": "enabled"}}
	source.Spec.Levels = []string{"topology.gpustack.ai/rack"}
	source.Spec.Webhook.PollInterval = meta.Duration{Duration: 10 * time.Second}
	configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
	secret := webhookTokenSecret("inventory-token")
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{"test.gpustack.ai/topology": "enabled"}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source, configMap, secret, node).
		WithStatusSubresource(&workercore.TopologySource{}).Build()
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	r := &TopologySourceReconciler{Client: cli, Now: func() time.Time { return now }}
	request := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}}

	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "rack-a")

	status.Store(http.StatusServiceUnavailable)
	now = now.Add(30 * time.Second)
	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
	assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "rack-a")
	assertSourceReadyReason(t, cli, source.Name, "Stale")

	now = now.Add(2 * time.Minute)
	result, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
	assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "")
	assertSourceReadyReason(t, cli, source.Name, "Expired")

	status.Store(0)
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "rack-a")
	assertSourceReadyReason(t, cli, source.Name, "Observed")
}

func TestTopologySourceWebhookConflictUsesShorterRequeue(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"inventory-1","nodes":{"node-a":{"topology.gpustack.ai/rack":"rack-a"}}}`))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	source := webhookTopologySource(server.URL)
	source.ObjectMeta = meta.ObjectMeta{Name: "inventory", UID: "source"}
	source.Spec.NodeSelector = meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/topology": "enabled"}}
	source.Spec.Levels = []string{"topology.gpustack.ai/rack"}
	source.Spec.Webhook.PollInterval = meta.Duration{Duration: 10 * time.Minute}
	other := source.DeepCopy()
	other.Name, other.UID = "other", "other-source"
	configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{"test.gpustack.ai/topology": "enabled"}}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source, other, configMap, webhookTokenSecret("token"), node).
		WithStatusSubresource(&workercore.TopologySource{}).Build()
	r := &TopologySourceReconciler{Client: cli}

	result, err := r.reconcileTopologySourceWebhook(context.Background(), source, source.Status)
	require.NoError(t, err)
	assert.Equal(t, time.Minute, result.RequeueAfter)
	got := new(workercore.TopologySource)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: source.Name}, got))
	assert.Contains(t, TopologySourceConditionOwnershipConflict.GetMessage(got), "other")
}

func TestTopologySourceWebhookReferenceWatches(t *testing.T) {
	source := webhookTopologySource("https://inventory.example.test")
	source.Name = "inventory"
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source).Build()
	r := &TopologySourceReconciler{Client: cli}

	configMapRequests := r.enqueueTopologySourcesWhenConfigMapChanged(context.Background(), &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}})
	secretRequests := r.enqueueTopologySourcesWhenSecretChanged(context.Background(), webhookTokenSecret("token"))
	unrelatedRequests := r.enqueueTopologySourcesWhenSecretChanged(context.Background(), &core.Secret{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "other"}})

	require.Len(t, configMapRequests, 1)
	require.Len(t, secretRequests, 1)
	assert.Empty(t, unrelatedRequests)
}

func webhookTopologySource(endpoint string) *workercore.TopologySource {
	return &workercore.TopologySource{Spec: workercore.TopologySourceSpec{Webhook: &workercore.TopologySourceWebhook{
		URL:                  endpoint,
		PollInterval:         meta.Duration{Duration: time.Minute},
		Timeout:              meta.Duration{Duration: time.Second},
		MaxStaleness:         meta.Duration{Duration: time.Minute},
		CABundleConfigMapRef: &workercore.TopologySourceObjectReference{Namespace: "gpustack-system", Name: "ca"},
		BearerTokenSecretRef: &workercore.TopologySourceObjectReference{Namespace: "gpustack-system", Name: "credential"},
	}}}
}

func webhookTokenSecret(token string) *core.Secret {
	return &core.Secret{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "credential"}, Data: map[string][]byte{"token": []byte(token)}}
}

func newClientCertificate(t *testing.T) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "topology-client"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	key, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	return certificate,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

func assertSourceNodeFeatureLabel(t *testing.T, cli ctrlcli.Client, sourceUID types.UID, key, expected string) {
	t.Helper()
	node := new(core.Node)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-a"}, node))
	assert.NotContains(t, node.Labels, key)
	feature := new(nfd.NodeFeature)
	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: kuberess.SystemNamespaceName, Name: topologySourceNodeFeatureName(sourceUID, node.Name)}, feature)
	if expected == "" {
		assert.True(t, kerrors.IsNotFound(err))
		return
	}
	require.NoError(t, err)
	assert.Equal(t, expected, feature.Spec.Labels[key])
}

func assertSourceReadyReason(t *testing.T, cli ctrlcli.Client, name, expected string) {
	t.Helper()
	source := new(workercore.TopologySource)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, source))
	assert.Equal(t, expected, TopologySourceConditionReady.GetReason(source))
}

type zeroReader struct{}

func (zeroReader) Read(data []byte) (int, error) {
	for i := range data {
		data[i] = 'x'
	}
	return len(data), nil
}

// TestTopologySourceWebhookRejectsInvalidSnapshotContent pins that a webhook answering HTTP 200 with
// a well-formed but invalid snapshot is rejected under the webhook's own staleness window: before
// any success the source reports the rejection, after a success the last valid NodeFeature is
// retained as Stale, and once maxStaleness passes it is removed as Expired.
func TestTopologySourceWebhookRejectsInvalidSnapshotContent(t *testing.T) {
	const valid = `{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"inventory-1","nodes":{"node-a":{"topology.kubernetes.io/zone":"zone-a","topology.gpustack.ai/rack":"rack-a"}}}`
	tests := []struct {
		name    string
		invalid string
	}{
		{name: "unknown node", invalid: `{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"inventory-2","nodes":{"ghost":{"topology.kubernetes.io/zone":"zone-a","topology.gpustack.ai/rack":"rack-a"}}}`},
		{name: "different standard zone", invalid: `{"apiVersion":"topology.gpustack.ai/v1alpha1","revision":"inventory-2","nodes":{"node-a":{"topology.kubernetes.io/zone":"zone-b","topology.gpustack.ai/rack":"rack-a"}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body atomic.Value
			body.Store(tc.invalid)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body.Load().(string)))
			}))
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			source := webhookTopologySource(server.URL)
			source.ObjectMeta = meta.ObjectMeta{Name: "inventory", UID: "source"}
			source.Spec.NodeSelector = meta.LabelSelector{MatchLabels: map[string]string{"test.gpustack.ai/topology": "enabled"}}
			source.Spec.Levels = []string{core.LabelTopologyZone, topologySourceRackLabel}
			source.Spec.Webhook.PollInterval = meta.Duration{Duration: 10 * time.Second}
			configMap := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "gpustack-system", Name: "ca"}, Data: map[string]string{"ca.crt": string(ca)}}
			node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{
				"test.gpustack.ai/topology": "enabled", core.LabelTopologyZone: "zone-a",
			}}}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source, configMap, webhookTokenSecret("inventory-token"), node).
				WithStatusSubresource(&workercore.TopologySource{}).Build()
			now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
			r := &TopologySourceReconciler{Client: cli, Now: func() time.Time { return now }}
			request := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: source.Name}}
			reconcile := func() ctrlreconcile.Result {
				t.Helper()
				var result ctrlreconcile.Result
				require.NotPanics(t, func() {
					var err error
					result, err = r.Reconcile(context.Background(), request)
					require.NoError(t, err)
				})
				return result
			}

			reconcile()
			assertSourceReadyReason(t, cli, source.Name, "SnapshotInvalid")
			assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "")

			body.Store(valid)
			reconcile()
			assertSourceReadyReason(t, cli, source.Name, "Observed")
			assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "rack-a")

			body.Store(tc.invalid)
			now = now.Add(30 * time.Second)
			result := reconcile()
			assert.Equal(t, 10*time.Second, result.RequeueAfter)
			assertSourceReadyReason(t, cli, source.Name, "Stale")
			assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "rack-a")
			got := new(workercore.TopologySource)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: source.Name}, got))
			assert.Equal(t, "SnapshotInvalid", TopologySourceConditionValid.GetReason(got))
			assert.Equal(t, "inventory-1", got.Status.LastSuccessfulRevision)

			now = now.Add(2 * time.Minute)
			reconcile()
			assertSourceReadyReason(t, cli, source.Name, "Expired")
			assertSourceNodeFeatureLabel(t, cli, source.UID, topologySourceRackLabel, "")
		})
	}
}
