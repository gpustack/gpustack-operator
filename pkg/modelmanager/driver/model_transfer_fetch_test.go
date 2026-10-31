package driver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
	"gpustack.ai/gpustack/pkg/setting"
)

func TestModelTransferCSITokenAudience(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		token string
	}{
		{name: "right audience", raw: `{"gpustack-model-transfer":{"token":"bound-token"}}`, token: "bound-token"},
		{name: "wrong audience", raw: `{"kubernetes":{"token":"bound-token"}}`},
		{name: "missing token", raw: `{}`},
		{name: "malformed", raw: `{`},
		{name: "oversized", raw: strings.Repeat("x", (128<<10)+1)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			token, err := modelTransferCSIToken(test.raw)
			if test.token == "" {
				require.Error(t, err)
				assert.Empty(t, token)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.token, token)
		})
	}
}

func TestModelTransferMetadataEndpointRequiresLiveSourceIdentity(t *testing.T) {
	ns := setting.DelegatedSecretNamespace
	controller := true
	source := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "source-1", Namespace: ns, UID: "source-uid",
			Labels:          map[string]string{"app.kubernetes.io/component": "model-transfer", "gpustack.ai/manufacturer": "ascend"},
			OwnerReferences: []meta.OwnerReference{{APIVersion: apps.SchemeGroupVersion.String(), Kind: "DaemonSet", Name: "source", UID: "daemon-uid", Controller: &controller}},
		},
		Spec: core.PodSpec{NodeName: "source-node"}, Status: core.PodStatus{Phase: core.PodRunning, PodIP: "192.0.2.2"},
	}
	driver := &Driver{APIReader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(source).Build()}
	endpoint := modelstore.ModelTransferEndpoint{
		PodName: source.Name, PodNamespace: ns, PodUID: "source-uid",
		NodeName: "source-node", Address: "192.0.2.2:9251", Manufacturer: "ascend", ServerName: "source." + ns + ".svc",
	}
	require.True(t, driver.validMetadataEndpoint(context.Background(), endpoint))
	endpoint.PodUID = "old-source-uid"
	assert.False(t, driver.validMetadataEndpoint(context.Background(), endpoint), "a recreated source cannot inherit its predecessor's identity")
}

func metadataTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, *x509.CertPool, string) {
	t.Helper()
	name := "source." + setting.DelegatedSecretNamespace + ".svc"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	require.NoError(t, err)
	private, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(certPEM))
	return server, roots, name
}

func TestFetchModelTransferMetadataUsesWorkloadTLSIdentity(t *testing.T) {
	source := newTestEnv(t).d.Store
	publishFullTree(t, source, testHex)
	viewDir, err := metadata.EnsureView(source, filepath.Join(t.TempDir(), "source-views"), testHex, testDigest, nil)
	require.NoError(t, err)
	var calls int
	server, roots, serverName := metadataTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "Bearer workload-only-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/v1/metadata", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "source-uid", body["sourcePodUID"])
		assert.Equal(t, "team-a", body["podNamespace"])
		w.Header().Set("X-GPUStack-Source-Pod-UID", "source-uid")
		require.NoError(t, metadata.ExportView(r.Context(), viewDir, w))
	}))
	destination, err := store.Open(t.TempDir())
	require.NoError(t, err)
	driver := &Driver{Store: destination}
	endpoint := modelstore.ModelTransferEndpoint{Address: strings.TrimPrefix(server.URL, "https://"), ServerName: serverName, PodUID: "source-uid"}
	req := modelTransferRequest("unused", map[string]string{AttrArtifact: "model", AttrArtifactUID: "uid-ma"})
	dir, err := driver.fetchMetadataEndpoint(context.Background(), req, endpoint, roots, "workload-only-token", testDigest, metadata.DefaultRequiredFiles)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.False(t, destination.IsPublished(testHex), "metadata fetch never publishes a full checkpoint")
	view, err := metadata.Verify(dir)
	require.NoError(t, err)
	assert.True(t, view.Incomplete)
	assert.Equal(t, testDigest, view.ArtifactDigest)

	endpoint.ServerName = "untrusted.invalid"
	_, err = driver.fetchMetadataEndpoint(context.Background(), req, endpoint, roots, "workload-only-token", testDigest, metadata.DefaultRequiredFiles)
	require.Error(t, err)
	assert.Equal(t, 1, calls, "a mismatched TLS identity receives no workload token")
}

func TestNodePublishMetadataRetriesReachHealthySource(t *testing.T) {
	source := newTestEnv(t).d.Store
	publishFullTree(t, source, testHex)
	view, err := metadata.EnsureView(source, filepath.Join(t.TempDir(), "views"), testHex, testDigest, nil)
	require.NoError(t, err)
	server, _, serverName := metadataTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SourcePodUID string `json:"sourcePodUID"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "Bearer workload-only-token", r.Header.Get("Authorization"))
		w.Header().Set("X-GPUStack-Source-Pod-UID", body.SourcePodUID)
		if body.SourcePodUID != "source-3" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		require.NoError(t, metadata.ExportView(r.Context(), view, w))
	}))
	artifact := testArtifact("team-a", "model", "uid-ma", testDigest, true)
	env := newTestEnv(t, artifact)
	owner := meta.OwnerReference{APIVersion: workercore.SchemeGroupVersion.String(), Kind: "ModelDeployment", Name: "consumer", UID: "deployment-uid", Controller: ptr.To(true)}
	consumer := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "p", UID: "uid-p", OwnerReferences: []meta.OwnerReference{owner}}, Spec: core.PodSpec{NodeName: env.d.NodeID}}
	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "discovery", OwnerReferences: []meta.OwnerReference{owner}}, Data: map[string]string{
		"ca.crt": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]})),
	}}
	objects := make([]ctrlcli.Object, 0, 6)
	objects = append(objects, consumer, cm)
	discovery := modelstore.ModelTransferDiscovery{Version: 1}
	for i := range 4 {
		name, ip := fmt.Sprintf("source-%d", i), fmt.Sprintf("192.0.2.%d", i+1)
		pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: setting.DelegatedSecretNamespace, Name: name, UID: types.UID(name), Labels: map[string]string{"app.kubernetes.io/component": "model-transfer", "gpustack.ai/manufacturer": "nvidia"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "source", UID: "ds-uid", Controller: ptr.To(true)}}}, Spec: core.PodSpec{NodeName: "source-node"}, Status: core.PodStatus{Phase: core.PodRunning, PodIP: ip}}
		objects = append(objects, pod)
		discovery.Endpoints = append(discovery.Endpoints, modelstore.ModelTransferEndpoint{Manufacturer: "nvidia", Address: net.JoinHostPort(ip, "9251"), ServerName: serverName, CAFile: "/public/ca.crt", PodUID: name, PodName: name, PodNamespace: pod.Namespace, NodeName: pod.Spec.NodeName})
	}
	raw, err := json.Marshal(discovery)
	require.NoError(t, err)
	cm.Data["endpoints.json"] = string(raw)
	env.d.APIReader = ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
	env.d.metadataDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	req := modelTransferRequest(env.target("remote"), map[string]string{
		AttrArtifact: "model", AttrArtifactUID: "uid-ma", AttrManifestDigest: testDigest,
		attrModelTransferDelivery: modelTransferDeliveryMetadata, modelstore.ModelTransferClientConfigAttribute: cm.Name,
		"csi.storage.k8s.io/serviceAccount.tokens": `{"gpustack-model-transfer":{"token":"workload-only-token"}}`,
	})
	for attempt := range 4 {
		timeout := 200 * time.Millisecond
		if attempt == 3 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, err = env.d.NodePublishVolume(ctx, req)
		cancel()
		if attempt < 3 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
	assert.Empty(t, env.materializer.requests)
	assert.False(t, env.d.Store.IsPublished(testHex))
	assert.Equal(t, 1, env.mounter.binds)
}
