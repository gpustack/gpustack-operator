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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// metadataServingFixture is a serving broker whose node holds a pre-staged
// metadata view of the fixture checkpoint and no published tree: the shape
// of a destination node a previous source prepared, or of any source that
// never materialized the complete checkpoint. It also returns the promoted
// view directory and the checkpoint's full length, for the header-stub assertion.
func metadataServingFixture(t *testing.T) (*store.Store, string, string, *Config, int64) {
	t.Helper()
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
	src := filepath.Join(root, "fixture-tree")
	require.NoError(t, os.MkdirAll(src, 0o755))
	checkpoint, err := os.ReadFile(filepath.Join("..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense", "checkpoint.safetensors"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(src, "checkpoint.safetensors"), checkpoint, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "config.json"), []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "manifest.json"), []byte(`{}`), 0o644))
	viewsDir := st.ViewsRoot()
	require.NoError(t, os.MkdirAll(viewsDir, 0o755))
	view, err := metadata.Build(src, filepath.Join(viewsDir, "stage"), digest, []string{"config.json", "manifest.json"})
	require.NoError(t, err)
	// Promotion names the directory by the record's content address, the
	// way EnsureView's publish does.
	viewDir := filepath.Join(viewsDir, view.ID())
	require.NoError(t, os.Rename(filepath.Join(viewsDir, "stage"), viewDir))

	labels := map[string]string{"app.kubernetes.io/component": SourceWorkerComponent, "gpustack.ai/manufacturer": "nvidia"}
	ds := &apps.DaemonSet{ObjectMeta: meta.ObjectMeta{Name: "transfer", Namespace: "operator", UID: "ds-uid", Labels: labels}, Spec: apps.DaemonSetSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}}}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: "operator", UID: "pod-uid", Labels: labels, OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "transfer", UID: "ds-uid", Controller: ptr.To(true)}}}, Spec: core.PodSpec{NodeName: "node-1", Containers: []core.Container{{Name: "worker", Image: "worker:v1"}}}}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1"}}
	client := kubefake.NewSimpleClientset(ds, pod, node)
	cfg := &Config{Store: st, Client: client, NodeName: "node-1", WorkerNamespace: "operator", WorkerID: "pod-uid", VendorScope: "nvidia", Authorizer: metadataTestAuth{}}

	return st, digest, viewDir, cfg, int64(len(checkpoint))
}

func metadataServingRequest(t *testing.T, digest, token string, required []string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"podNamespace": "tenant", "podName": "engine", "podUID": "engine-uid",
		"artifactName": "model", "artifactUID": "artifact", "manifestDigest": digest,
		"sourcePodUID": "pod-uid", "required": required,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/v1/metadata", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)

	return request
}

// TestMetadataHandlerServesPreStagedViewWithoutPublishedTree pins the
// view-only contract: an authenticated request whose digest names a
// verified pre-staged view exports from that view without any published
// tree, holds an export-lifetime reference while it reads, and fails
// closed on identity, digest, and required-file faults.
func TestMetadataHandlerServesPreStagedViewWithoutPublishedTree(t *testing.T) {
	st, digest, viewDir, cfg, checkpointLen := metadataServingFixture(t)
	hex := store.HexOf(digest)

	t.Run("a pre-staged view exports without a published tree", func(t *testing.T) {
		require.False(t, st.IsPublished(hex), "the fixture holds no published tree")
		response := httptest.NewRecorder()
		cfg.metadataHandler().ServeHTTP(response,
			metadataServingRequest(t, digest, "good-token", []string{"config.json", "manifest.json"}))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, "pod-uid", response.Header().Get("X-GPUStack-Source-Pod-UID"))
		seen := map[string]bool{}
		archive := tar.NewReader(response.Body)
		for {
			header, err := archive.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			seen[header.Name] = true
			if header.Name == "checkpoint.safetensors" {
				assert.Less(t, header.Size, checkpointLen,
					"the checkpoint rides as its bounded header stub")
			}
		}
		assert.True(t, seen["config.json"] && seen["manifest.json"],
			"the required files ride in the export")
		refs, err := st.Refs()
		require.NoError(t, err)
		assert.Empty(t, refs, "the export's reference leaves with the response")
	})

	t.Run("a denied identity exports nothing", func(t *testing.T) {
		response := httptest.NewRecorder()
		cfg.metadataHandler().ServeHTTP(response,
			metadataServingRequest(t, digest, "bad-token", []string{"config.json"}))
		assert.Equal(t, http.StatusForbidden, response.Code)
		refs, err := st.Refs()
		require.NoError(t, err)
		assert.Empty(t, refs, "a refused request writes no reference")
	})

	t.Run("a malformed digest is refused without a reference", func(t *testing.T) {
		response := httptest.NewRecorder()
		cfg.metadataHandler().ServeHTTP(response,
			metadataServingRequest(t, "sha256:short", "good-token", []string{"config.json"}))
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		refs, err := st.Refs()
		require.NoError(t, err)
		assert.Empty(t, refs, "a refused digest writes no reference")
	})

	t.Run("an unknown required file is the caller's fault", func(t *testing.T) {
		response := httptest.NewRecorder()
		cfg.metadataHandler().ServeHTTP(response,
			metadataServingRequest(t, digest, "good-token", []string{"../escape"}))
		assert.Equal(t, http.StatusBadRequest, response.Code)
		refs, err := st.Refs()
		require.NoError(t, err)
		assert.Empty(t, refs, "a refused request writes no reference")
	})

	t.Run("a live export reference keeps the view from reclamation", func(t *testing.T) {
		volID := metadataPrefix("pod-uid", "nvidia") + "export"
		require.NoError(t, st.WriteRef(store.Ref{
			VolumeID: volID, Hex: hex, PodUID: "pod-uid",
		}))
		gone, err := st.RemoveUnreferencedView(context.Background(), viewDir, hex)
		require.NoError(t, err)
		assert.False(t, gone, "a reference naming the digest keeps the view")
		assert.DirExists(t, viewDir)

		require.NoError(t, st.RemoveRef(volID))
		gone, err = st.RemoveUnreferencedView(context.Background(), viewDir, hex)
		require.NoError(t, err)
		assert.True(t, gone, "without the export reference the final view directory is reclaimed")
		assert.NoDirExists(t, viewDir)
	})

	t.Run("a retired view before export answers 503 without headers and recovers once restaged", func(t *testing.T) {
		response := httptest.NewRecorder()
		cfg.metadataHandler().ServeHTTP(response,
			metadataServingRequest(t, digest, "good-token", []string{"config.json", "manifest.json"}))
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.Empty(t, response.Header().Get("X-GPUStack-Source-Pod-UID"),
			"a failed export never emits success identity headers")
		refs, err := st.Refs()
		require.NoError(t, err)
		assert.Empty(t, refs, "the failed export cleans up its reference")

		// Re-staging the view allows the retry to export cleanly.
		src := filepath.Join(st.Root(), "fixture-tree")
		_, err = metadata.Build(src, viewDir, digest, []string{"config.json", "manifest.json"})
		require.NoError(t, err)
		retryResp := httptest.NewRecorder()
		cfg.metadataHandler().ServeHTTP(retryResp,
			metadataServingRequest(t, digest, "good-token", []string{"config.json", "manifest.json"}))
		require.Equal(t, http.StatusOK, retryResp.Code, retryResp.Body.String())
		assert.Equal(t, "pod-uid", retryResp.Header().Get("X-GPUStack-Source-Pod-UID"))
	})
}
