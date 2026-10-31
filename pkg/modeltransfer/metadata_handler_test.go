package modeltransfer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

type metadataTestAuth struct{ deny bool }

func (a metadataTestAuth) AuthorizeSession(context.Context, authorization.Request) (authorization.Result, *protocol.Error) {
	return authorization.Result{}, nil
}

func TestMetadataRecoveryOnlyRemovesOwnedTransientClaims(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	owned := store.Ref{VolumeID: metadataPrefix("pod-a") + "request", PodUID: "pod-a", Hex: strings.Repeat("a", 64)}
	scopedOldMetadata := store.Ref{VolumeID: metadataPrefix("pod-a-old", "nvidia") + "request", PodUID: "pod-a-old", Hex: strings.Repeat("a", 64)}
	scopedOldBuild := store.Ref{VolumeID: buildRefPrefix("nvidia") + "build-1", PodUID: "pod-a-old", Hex: strings.Repeat("a", 64)}
	other := store.Ref{VolumeID: metadataPrefix("pod-b", "ascend") + "request", PodUID: "pod-b", Hex: strings.Repeat("a", 64)}
	otherBuild := store.Ref{VolumeID: buildRefPrefix("ascend") + "build-2", PodUID: "pod-b", Hex: strings.Repeat("a", 64)}
	generation := store.Ref{VolumeID: "model-transfer.nvidia.g-nvidia-0123456789abcdef0123456789abcdef-1", PodUID: "pod-a", Hex: strings.Repeat("a", 64)}
	for _, ref := range []store.Ref{owned, scopedOldMetadata, scopedOldBuild, other, otherBuild, generation} {
		require.NoError(t, st.WriteRef(ref))
	}
	dir := filepath.Join(st.Root(), "model-transfer", "metadata-temp")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, owned.VolumeID), []byte("tmp"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, scopedOldMetadata.VolumeID), []byte("tmp"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, other.VolumeID), []byte("tmp"), 0o600))
	// A crashed view build leaves a staging directory behind; recovery
	// removes it while a finished view stays untouched.
	viewsDir := filepath.Join(st.Root(), "model-transfer", "views")
	require.NoError(t, os.MkdirAll(filepath.Join(viewsDir, ".staging-crashed"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(viewsDir, ".staging-crashed", "view.bin"), []byte("partial"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(viewsDir, "finished-view"), 0o700))
	require.NoError(t, cleanupMetadataRequests(st, "pod-a", "nvidia"))
	assert.NoDirExists(t, filepath.Join(viewsDir, ".staging-crashed"),
		"a crashed view build's staging directory is recovered")
	assert.DirExists(t, filepath.Join(viewsDir, "finished-view"),
		"a finished view is never touched by recovery")
	for _, removed := range []store.Ref{owned, scopedOldMetadata, scopedOldBuild} {
		_, exists, err := st.ReadRef(removed.VolumeID)
		require.NoError(t, err)
		assert.False(t, exists, removed.VolumeID)
	}
	for _, ref := range []store.Ref{other, otherBuild, generation} {
		_, exists, err := st.ReadRef(ref.VolumeID)
		require.NoError(t, err)
		assert.True(t, exists, ref.VolumeID)
	}
	_, err = os.Stat(filepath.Join(dir, owned.VolumeID))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, scopedOldMetadata.VolumeID))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, other.VolumeID))
	assert.NoError(t, err)
}

func TestMetadataRequestCapacityIsBounded(t *testing.T) {
	cfg := &Config{WorkerID: "pod"}
	handler := cfg.metadataHandler()
	for i := 0; i < cap(cfg.metadataSlots); i++ {
		cfg.metadataSlots <- struct{}{}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/metadata", strings.NewReader(`{"sourcePodUID":"pod"}`))
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, req)
	assert.Equal(t, http.StatusServiceUnavailable, result.Code)
}

func (a metadataTestAuth) AuthenticatePod(context.Context, authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	return authorization.PodIdentity{}, nil
}

func (a metadataTestAuth) AuthorizeMetadata(_ context.Context, req authorization.Request, node string) *protocol.Error {
	if a.deny || req.Token != "good-token" || node != "node-1" {
		return protocol.New(protocol.KindAuthentication, "token", "denied")
	}
	return nil
}

func TestMetadataHandlerVerifiesSourceAndExportsHeaders(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	st, err := store.Open(root)
	require.NoError(t, err)
	digest := "sha256:" + strings.Repeat("a", 64)
	attempt, err := st.NewAttempt(store.HexOf(digest))
	require.NoError(t, err)
	tree := filepath.Join(attempt.Dir(), "tree")
	require.NoError(t, os.MkdirAll(tree, 0o755))
	checkpoint, err := os.ReadFile(filepath.Join("..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense", "checkpoint.safetensors"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tree, "checkpoint.safetensors"), checkpoint, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "config.json"), []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "manifest.json"), []byte(`{}`), 0o644))
	require.NoError(t, attempt.Publish(store.Marker{Digest: digest, SizeBytes: int64(len(checkpoint)), Source: store.SourceHub}))
	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "transfer", Namespace: "operator", UID: "ds-uid", Labels: labels}, Spec: apps.DaemonSetSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}}}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: "operator", UID: "pod-uid", Labels: labels, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "transfer", UID: "ds-uid", Controller: ptr.To(true)}}}, Spec: core.PodSpec{NodeName: "node-1", Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1"}}
	cfg := &Config{Store: st, Client: kubefake.NewSimpleClientset(ds, pod, node), NodeName: "node-1", WorkerNamespace: "operator", WorkerID: "pod-uid", VendorScope: "nvidia", Authorizer: metadataTestAuth{}}
	body, err := json.Marshal(map[string]any{"podNamespace": "tenant", "podName": "engine", "podUID": "engine-uid", "artifactName": "model", "artifactUID": "artifact", "manifestDigest": digest, "sourcePodUID": "pod-uid", "required": []string{"config.json", "manifest.json"}})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/v1/metadata", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer good-token")
	response := httptest.NewRecorder()
	cfg.metadataHandler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "pod-uid", response.Header().Get("X-GPUStack-Source-Pod-UID"))
	archive := tar.NewReader(response.Body)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if header.Name == "checkpoint.safetensors" {
			assert.Less(t, header.Size, int64(len(checkpoint)))
		}
	}
	refs, err := st.Refs()
	require.NoError(t, err)
	assert.Empty(t, refs)
	b, err := broker.Open(filepath.Join(root, "broker"), "nvidia", st, broker.Limits{PhysicalBytes: 1024, MaxGenerations: 2, MaxQuarantined: 1, QuarantineBytes: 512, LeaseTTL: time.Minute}, time.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	cfg.Broker = b
	instance := strings.Repeat("a", 32)
	require.NoError(t, cfg.recordWorkerInstance(context.Background(), "pod-uid", instance, false))
	current, err := cfg.Client.CoreV1().Pods("operator").Get(context.Background(), "source", meta.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, instance, current.Annotations[workerInstanceAnnotation])
	current.Annotations[drainForAnnotation] = "worker:v2"
	_, err = cfg.Client.CoreV1().Pods("operator").Update(context.Background(), current, meta.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, cfg.recordWorkerInstance(context.Background(), "pod-uid", instance, false))
	require.Error(t, cfg.recordWorkerInstance(context.Background(), "pod-uid", instance, true))
	require.Error(t, cfg.recordWorkerInstance(context.Background(), "pod-uid", strings.Repeat("b", 32), false))

	wrongPod := *pod.DeepCopy()
	wrongPod.UID = types.UID("replaced")
	cfg.Client = kubefake.NewSimpleClientset(ds, &wrongPod, node)
	request = httptest.NewRequest(http.MethodPost, "/v1/metadata", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer good-token")
	response = httptest.NewRecorder()
	cfg.metadataHandler().ServeHTTP(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func (a metadataTestAuth) AuthenticateActivePod(ctx context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	return a.AuthenticatePod(ctx, req)
}
