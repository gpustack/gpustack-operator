package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const testProgressDigest = "sha256:" + "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"

func testProgressArtifact(resolved, claim bool) *workercore.ModelArtifact {
	ma := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"}}
	if claim {
		ma.Spec.Source.PersistentVolumeClaim = &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: "models"}
		return ma
	}
	ma.Spec.Source.HuggingFace = &workercore.ModelArtifactHubSource{Repository: "owner/secret-model"}
	if resolved {
		ma.Status.Resolved = &workercore.ModelArtifactResolved{ManifestDigest: testProgressDigest, SizeBytes: 1000}
	}
	return ma
}

func testProgressStore(node string, m workercore.NodeModelStoreModel) *workercore.NodeModelStore {
	m.Digest = testProgressDigest
	return &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: node}, Status: workercore.NodeModelStoreStatus{
		Models: []workercore.NodeModelStoreModel{m, {Digest: "sha256:other", State: workercore.NodeModelStoreModelStateReady}},
	}}
}

func testPluginPod(node, ip string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: kuberess.SystemNamespaceName, Name: "model-manager-" + node,
			Labels: map[string]string{deviceplugin.ComponentLabelKey: modelstore.PluginComponent},
		},
		Spec: core.PodSpec{NodeName: node, Containers: []core.Container{{
			Name:  "main",
			Ports: []core.ContainerPort{{Name: modelstore.PluginSecurePortName, ContainerPort: 32444}},
		}}},
		Status: core.PodStatus{PodIP: ip, Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}},
	}
}

func TestModelArtifactProgress(t *testing.T) {
	downloading := workercore.NodeModelStoreModel{State: workercore.NodeModelStoreModelStateDownloading, SizeBytes: 1000, DownloadedBytes: 200}
	stores := []ctrlcli.Object{
		testProgressStore("gpu-node-a", workercore.NodeModelStoreModel{State: workercore.NodeModelStoreModelStateReady, SizeBytes: 1000}),
		testProgressStore("gpu-node-b", downloading),
		testProgressStore("gpu-node-c", downloading),
		testProgressStore("gpu-node-d", workercore.NodeModelStoreModel{State: workercore.NodeModelStoreModelStateFailed, Reason: "SourceUnavailable"}),
		testPluginPod("gpu-node-b", "10.0.0.2"), testPluginPod("gpu-node-c", "10.0.0.3"),
	}
	live := func(ctx context.Context, url string) (*modelstore.Downloads, error) {
		switch {
		case strings.Contains(url, "10.0.0.2"):
			return &modelstore.Downloads{Downloads: []modelstore.Download{{Digest: testProgressDigest, DownloadedBytes: 600, SizeBytes: 1000}}}, nil
		case strings.Contains(url, "10.0.0.3") && ctx.Value(hangKey{}) != nil:
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			return nil, errors.New("connection refused")
		}
	}
	intp := func(v int32) *int32 { return &v }

	cases := []struct {
		name     string
		artifact *workercore.ModelArtifact
		hang     bool
		want     worker.ModelArtifactProgress
	}{
		{
			name: "counts by state, the mean of the downloading nodes with the live reading, the reasons", artifact: testProgressArtifact(true, false),
			want: worker.ModelArtifactProgress{
				ManifestDigest: testProgressDigest, SizeBytes: 1000, Ready: 1, Downloading: 2, Failed: 1,
				DownloadingPercent: intp(40), DownloadingBytes: 800, Live: 1,
				FailureReasons: []worker.ModelArtifactProgressReason{{Reason: "SourceUnavailable", Count: 1}},
			},
		},
		{
			name: "a hanging plugin keeps its stored value within the bound", artifact: testProgressArtifact(true, false), hang: true,
			want: worker.ModelArtifactProgress{
				ManifestDigest: testProgressDigest, SizeBytes: 1000, Ready: 1, Downloading: 2, Failed: 1,
				DownloadingPercent: intp(40), DownloadingBytes: 800, Live: 1,
				FailureReasons: []worker.ModelArtifactProgressReason{{Reason: "SourceUnavailable", Count: 1}},
			},
		},
		{name: "an unresolved artifact counts nothing", artifact: testProgressArtifact(false, false), want: worker.ModelArtifactProgress{Reason: _ModelArtifactProgressNotResolv}},
		{name: "a claim is never downloaded", artifact: testProgressArtifact(false, true), want: worker.ModelArtifactProgress{Reason: _ModelArtifactProgressClaim}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(append(stores, c.artifact)...).Build()
			now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
			h := &ModelArtifactProgressHandler{APIReader: cli, Client: cli, Downloads: live, Now: func() time.Time { return now }}
			ctx := context.Background()
			if c.hang {
				ctx = context.WithValue(ctx, hangKey{}, true)
			}

			start := time.Now()
			obj, err := h.OnGet(ctx, types.NamespacedName{Namespace: "team-a", Name: "qwen"}, ctrlcli.GetOptions{})
			require.NoError(t, err)
			assert.Less(t, time.Since(start), _ModelArtifactProgressTimeout+time.Second)
			got := obj.(*worker.ModelArtifactProgress)
			want := c.want
			want.ObjectMeta = meta.ObjectMeta{Namespace: "team-a", Name: "qwen"}
			want.Timestamp = meta.NewTime(now)
			assert.Equal(t, &want, got)

			raw, err := json.Marshal(got)
			require.NoError(t, err)
			for _, node := range []string{"gpu-node-a", "gpu-node-b", "gpu-node-c", "gpu-node-d", "10.0.0."} {
				assert.NotContains(t, string(raw), node, "the answer names no node")
			}
		})
	}
}

type hangKey struct{}

func TestModelArtifactProgressOfAMissingArtifact(t *testing.T) {
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	h := &ModelArtifactProgressHandler{APIReader: cli, Client: cli}
	_, err := h.OnGet(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "qwen"}, ctrlcli.GetOptions{})
	require.Error(t, err)
}

// TestV1ViewsWriteNothingThroughTheWorker pins the two write surfaces the v1 views must not have:
// ModelArtifact's status, which the proxy would write as the worker the status guard admits, and
// every create or update of NodeModelStore; and the delete NodeModelStore must keep, which the
// garbage collector needs at the preferred version.
func TestV1ViewsWriteNothingThroughTheWorker(t *testing.T) {
	_, hasStatus := any(&worker.ModelArtifact{}).(extensionapi.ObjectWithStatusSubResource)
	assert.False(t, hasStatus, "v1 ModelArtifact serves no status subresource")

	var nms any = &NodeModelStoreHandler{}
	for name, ok := range map[string]bool{
		"create": func() bool { _, ok := nms.(rest.Creater); return ok }(),
		"update": func() bool { _, ok := nms.(rest.Updater); return ok }(),
	} {
		assert.False(t, ok, "v1 NodeModelStore serves no %s", name)
	}
	for name, ok := range map[string]bool{
		"get":    func() bool { _, ok := nms.(rest.Getter); return ok }(),
		"list":   func() bool { _, ok := nms.(rest.Lister); return ok }(),
		"watch":  func() bool { _, ok := nms.(rest.Watcher); return ok }(),
		"delete": func() bool { _, ok := nms.(rest.GracefulDeleter); return ok }(),
	} {
		assert.True(t, ok, "v1 NodeModelStore serves %s", name)
	}
}

// TestFetchPluginDownloads pins the live read over HTTPS against a plugin that answers, refuses,
// answers what is not the envelope, and hangs.
func TestFetchPluginDownloads(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/answers" + modelstore.DownloadsPath:
			_, _ = w.Write([]byte(`{"downloads":[{"digest":"sha256:d","downloadedBytes":5,"sizeBytes":9,"source":"Hub"}]}`))
		case "/garbage" + modelstore.DownloadsPath:
			_, _ = w.Write([]byte(`<html>`))
		case "/hangs" + modelstore.DownloadsPath:
			<-r.Context().Done()
		default:
			http.Error(w, "no", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := fetchPluginDownloads(context.Background(), srv.URL+"/answers"+modelstore.DownloadsPath)
	require.NoError(t, err)
	assert.Equal(t, []modelstore.Download{{Digest: "sha256:d", DownloadedBytes: 5, SizeBytes: 9, Source: "Hub"}}, got.Downloads)

	for _, path := range []string{"/refuses", "/garbage"} {
		_, err := fetchPluginDownloads(context.Background(), srv.URL+path+modelstore.DownloadsPath)
		assert.Error(t, err, path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = fetchPluginDownloads(ctx, srv.URL+"/hangs"+modelstore.DownloadsPath)
	assert.Error(t, err, "a hanging plugin ends with the caller's bound")
}
