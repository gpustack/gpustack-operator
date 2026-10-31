package driver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

const modelTransferAudience = "gpustack-model-transfer"

const modelTransferMetadataAttempts = 16

var errModelTransferMetadataUnavailable = errors.New("no authenticated source supplied the required metadata view")

func (d *Driver) fetchModelTransferMetadata(ctx context.Context, req *csi.NodePublishVolumeRequest,
	digest string, required []string,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if d.APIReader == nil {
		return "", errors.New("live workload and discovery reader is unavailable")
	}
	vc := req.GetVolumeContext()
	pod := new(core.Pod)
	if err := d.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: vc[attrPodNamespace], Name: vc[attrPodName]}, pod); err != nil {
		ctrllog.FromContext(ctx).Error(err, "read metadata workload identity")
		return "", errors.New("metadata workload identity is unavailable")
	}
	if string(pod.UID) != vc[attrPodUID] || pod.UID == "" || pod.DeletionTimestamp != nil ||
		pod.Spec.NodeName != d.NodeID || pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed {
		return "", errors.New("metadata workload identity is stale")
	}
	owner := meta.GetControllerOf(pod)
	if owner == nil || owner.Kind != "ModelDeployment" || owner.UID == "" {
		return "", errors.New("metadata workload has no managed deployment owner")
	}
	cm := new(core.ConfigMap)
	if err := d.APIReader.Get(ctx, ctrlcli.ObjectKey{
		Namespace: pod.Namespace,
		Name:      vc[modelstore.ModelTransferClientConfigAttribute],
	}, cm); err != nil {
		ctrllog.FromContext(ctx).Error(err, "read public source discovery")
		return "", errors.New("public source discovery is unavailable")
	}
	cmOwner := meta.GetControllerOf(cm)
	if cmOwner == nil || cmOwner.UID != owner.UID || cmOwner.Kind != owner.Kind || cmOwner.Name != owner.Name ||
		cmOwner.APIVersion != owner.APIVersion || cm.DeletionTimestamp != nil {
		return "", errors.New("public source discovery belongs to another deployment")
	}
	raw := cm.Data["endpoints.json"]
	if len(raw) > 1<<20 {
		return "", errors.New("public source discovery exceeds its bound")
	}
	var discovery modelstore.ModelTransferDiscovery
	if err := json.Unmarshal([]byte(raw), &discovery); err != nil {
		ctrllog.FromContext(ctx).Error(err, "decode public source discovery")
		return "", errors.New("public source discovery is invalid")
	}
	if discovery.Version != 1 || len(discovery.Endpoints) > 4096 {
		return "", errors.New("public source discovery is invalid")
	}
	token, err := modelTransferCSIToken(vc["csi.storage.k8s.io/serviceAccount.tokens"])
	if err != nil {
		return "", err
	}
	attempts := min(len(discovery.Endpoints), modelTransferMetadataAttempts)
	start := d.metadataEndpointCursor.Add(1) - 1
	for offset := range attempts {
		endpoint := discovery.Endpoints[(start+uint64(offset))%uint64(len(discovery.Endpoints))]
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !d.validMetadataEndpoint(ctx, endpoint) {
			ctrllog.FromContext(ctx).V(2).Info("skip invalid source metadata endpoint", "sourcePod", endpoint.PodName, "attempt", offset+1)
			continue
		}
		caName := filepath.Base(endpoint.CAFile)
		ca := cm.Data[caName]
		roots := x509.NewCertPool()
		if len(ca) == 0 || len(ca) > 256<<10 || !roots.AppendCertsFromPEM([]byte(ca)) {
			ctrllog.FromContext(ctx).V(2).Info("skip source metadata endpoint with invalid trust anchor", "sourcePod", endpoint.PodName, "attempt", offset+1)
			continue
		}
		dir, err := d.fetchMetadataEndpoint(ctx, req, endpoint, roots, token, digest, required)
		if err == nil {
			return dir, nil
		}
		ctrllog.FromContext(ctx).V(2).Info("fetch source metadata endpoint failed", "sourcePod", endpoint.PodName, "attempt", offset+1, "error", err)
	}
	return "", errModelTransferMetadataUnavailable
}

func modelTransferCSIToken(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > 128<<10 {
		return "", errors.New("the workload-scoped model-transfer CSI token is unavailable")
	}
	var tokens map[string]struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(raw), &tokens); err != nil || tokens[modelTransferAudience].Token == "" {
		return "", errors.New("the workload-scoped model-transfer CSI token is unavailable")
	}
	return tokens[modelTransferAudience].Token, nil
}

func (d *Driver) validMetadataEndpoint(ctx context.Context, endpoint modelstore.ModelTransferEndpoint) bool {
	ip, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || port != "9251" || net.ParseIP(ip) == nil || endpoint.PodUID == "" ||
		endpoint.PodNamespace != setting.DelegatedSecretNamespace || endpoint.PodName == "" {
		return false
	}
	source := new(core.Pod)
	if err := d.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: endpoint.PodNamespace, Name: endpoint.PodName}, source); err != nil {
		ctrllog.FromContext(ctx).V(2).Info("read source metadata endpoint identity failed", "sourcePod", endpoint.PodName, "error", err)
		return false
	}
	owner := meta.GetControllerOf(source)
	return string(source.UID) == endpoint.PodUID && source.DeletionTimestamp == nil && source.Status.Phase == core.PodRunning &&
		source.Status.PodIP == ip && source.Spec.NodeName == endpoint.NodeName &&
		source.Labels["app.kubernetes.io/component"] == "model-transfer" &&
		source.Labels["gpustack.ai/manufacturer"] == endpoint.Manufacturer &&
		owner != nil && owner.APIVersion == apps.SchemeGroupVersion.String() && owner.Kind == "DaemonSet" && owner.UID != "" &&
		endpoint.ServerName == owner.Name+"."+source.Namespace+".svc"
}

func (d *Driver) fetchMetadataEndpoint(ctx context.Context, req *csi.NodePublishVolumeRequest,
	endpoint modelstore.ModelTransferEndpoint, roots *x509.CertPool, token, digest string, required []string,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vc := req.GetVolumeContext()
	body, err := json.Marshal(map[string]any{
		"podNamespace": vc[attrPodNamespace], "podName": vc[attrPodName], "podUID": vc[attrPodUID],
		"artifactName": vc[AttrArtifact], "artifactUID": vc[AttrArtifactUID], "manifestDigest": digest,
		"sourcePodUID": endpoint.PodUID, "required": required,
	})
	if err != nil {
		return "", err
	}
	request, err := httpx.NewPostRequestWithContext(ctx, "https://"+endpoint.Address+"/v1/metadata", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	transportOpts := httpx.TransportOptions().
		WithoutProxy().
		WithoutKeepalive().
		WithTLSClientConfig(&tls.Config{
			RootCAs:    roots,
			ServerName: endpoint.ServerName,
			MinVersion: tls.VersionTLS12,
		})
	if d.metadataDialContext != nil {
		transportOpts.WithDial(d.metadataDialContext)
	}
	transport := httpx.Transport(transportOpts)
	defer transport.CloseIdleConnections()
	client := httpx.Client(
		httpx.ClientOptions().
			WithTransportOption(transportOpts).
			WithTimeout(30*time.Second).
			WithoutRedirect().
			WithBearerAuth(token).
			WithHeader("Content-Type", "application/json"),
	)
	response, err := client.Do(request)
	if err != nil {
		ctrllog.FromContext(ctx).V(2).Info("source metadata TLS request failed", "sourcePod", endpoint.PodName, "error", err)
		return "", errors.New("source metadata TLS request failed")
	}
	defer httpx.Close(response)
	if response.StatusCode != http.StatusOK || response.Header.Get("X-GPUStack-Source-Pod-UID") != endpoint.PodUID {
		return "", errors.New("source metadata identity was refused")
	}
	return metadata.ImportView(ctx, response.Body, filepath.Join(d.Store.Root(), "model-transfer", "views"), digest, required)
}
