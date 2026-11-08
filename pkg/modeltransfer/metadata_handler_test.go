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
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
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
	// A crashed view build leaves its staging directory, ownership lock,
	// and published marker behind; recovery reclaims them only while the
	// lock can be taken and the marker matches the held file. A lock
	// another builder still holds, staging whose lock carries no published
	// marker, and staging that predates the protocol survive untouched, as
	// do published views. The proof fixtures are built through the actual
	// builder helper.
	viewsDir := filepath.Join(st.Root(), "model-transfer", "views")
	require.NoError(t, os.MkdirAll(viewsDir, 0o755))
	crashed, err := metadata.NewStaging(viewsDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(crashed.Dir(), "view.bin"), []byte("partial"), 0o600))
	crashed.Abandon()
	active, err := metadata.NewStaging(viewsDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(active.Dir(), "view.bin"), []byte("partial"), 0o600))
	t.Cleanup(func() { active.Release() })
	require.NoError(t, os.MkdirAll(filepath.Join(viewsDir, ".staging-unowned"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(viewsDir, ".staging-unowned.lock"), []byte{}, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(viewsDir, ".staging-legacy"), 0o700))
	published := filepath.Join(viewsDir, strings.Repeat("9", 64))
	require.NoError(t, os.MkdirAll(published, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(viewsDir, "finished-view"), 0o700))
	require.NoError(t, cleanupMetadataRequests(st, "pod-a", "nvidia"))
	assert.NoDirExists(t, crashed.Dir(),
		"a crashed view build's staging directory is recovered")
	assert.NoFileExists(t, crashed.LockPath(),
		"a reclaimed staging's ownership lock goes with it")
	assert.DirExists(t, active.Dir(),
		"a staging tree whose ownership lock another builder holds survives")
	assert.FileExists(t, active.LockPath(),
		"an active builder's ownership lock survives")
	assert.DirExists(t, filepath.Join(viewsDir, ".staging-unowned"),
		"staging whose lock carries no published marker cannot be proven owned and is preserved")
	assert.FileExists(t, filepath.Join(viewsDir, ".staging-unowned.lock"),
		"an unmarked lock file survives with its staging")
	assert.DirExists(t, filepath.Join(viewsDir, ".staging-legacy"),
		"staging without an ownership lock cannot be proven inactive and is preserved")
	assert.DirExists(t, published,
		"a published view is never touched by recovery")
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

func TestMetadataRecoveryReclaimsViewStagingWithoutMetadataTemp(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	// metadata-temp has never been created: recovery must still reach the
	// view root, which a missing directory does not exempt.
	viewsDir := filepath.Join(st.Root(), "model-transfer", "views")
	require.NoError(t, os.MkdirAll(viewsDir, 0o755))
	crashed, err := metadata.NewStaging(viewsDir)
	require.NoError(t, err)
	crashed.Abandon()
	require.NoError(t, os.MkdirAll(filepath.Join(viewsDir, ".staging-legacy"), 0o700))
	published := filepath.Join(viewsDir, strings.Repeat("8", 64))
	require.NoError(t, os.MkdirAll(published, 0o700))
	require.NoError(t, cleanupMetadataRequests(st, "pod-a", "nvidia"))
	assert.NoDirExists(t, crashed.Dir(),
		"recovery reclaims an abandoned staging even without metadata-temp")
	assert.NoFileExists(t, crashed.LockPath())
	assert.DirExists(t, filepath.Join(viewsDir, ".staging-legacy"),
		"staging without an ownership lock is preserved")
	assert.DirExists(t, published,
		"a published view is never touched by recovery")
}

func TestMetadataRecoveryToleratesMissingDirectories(t *testing.T) {
	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, cleanupMetadataRequests(st, "pod-a", "nvidia"),
		"neither metadata-temp nor the view root exists yet")
	require.NoError(t, st.WriteRef(store.Ref{
		VolumeID: metadataPrefix("pod-b", "ascend") + "other", PodUID: "pod-b", Hex: strings.Repeat("a", 64),
	}))
	require.NoError(t, cleanupMetadataRequests(st, "pod-a", "nvidia"))
	_, exists, err := st.ReadRef(metadataPrefix("pod-b", "ascend") + "other")
	require.NoError(t, err)
	assert.True(t, exists, "recovery still resolves claims with nothing to sweep")
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
	client := kubefake.NewSimpleClientset(ds, pod, node)
	cfg := &Config{Store: st, Client: client, NodeName: "node-1", WorkerNamespace: "operator", WorkerID: "pod-uid", VendorScope: "nvidia", Authorizer: metadataTestAuth{}}
	body, err := json.Marshal(map[string]any{"podNamespace": "tenant", "podName": "engine", "podUID": "engine-uid", "artifactName": "model", "artifactUID": "artifact", "manifestDigest": digest, "sourcePodUID": "pod-uid", "required": []string{"config.json", "manifest.json"}})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/v1/metadata", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer good-token")
	response := httptest.NewRecorder()
	cfg.metadataHandler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	daemonReads := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "daemonsets" {
			daemonReads++
		}
	}
	assert.Equal(t, 1, daemonReads, "one source identity check reuses its validated DaemonSet")
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

	unauthClient := kubefake.NewSimpleClientset(ds, pod, node)
	cfg.Client = unauthClient
	request = httptest.NewRequest(http.MethodPost, "/v1/metadata", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer bad-token")
	response = httptest.NewRecorder()
	cfg.metadataHandler().ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.Empty(t, unauthClient.Actions(), "unauthorized metadata requests must not trigger sourcePod API reads")
}

func (a metadataTestAuth) AuthenticateActivePod(ctx context.Context, req authorization.Request) (authorization.PodIdentity, *protocol.Error) {
	return a.AuthenticatePod(ctx, req)
}
