package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/gc"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/report"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	testNamespace  = "team-secret"
	testForgerName = "qwen-decode-forger"
	testForgerUID  = "forger-uid-1111"
	testPodName    = "qwen-decode-forger-sandbox-1"
	testPodUID     = "sandbox-pod-uid-2222"
	testNodeName   = "gpu-node-1"
	testWorkerSA   = "custom-release-worker"
	testSysNS      = "gpustack-system"
	testKernelKey  = "vllm/0.8.5/nvidia/sm_90a/rmsnorm/0123456789abcdef"
)

var testTime = time.Date(2026, 11, 27, 8, 30, 0, 0, time.UTC)

type fakeTokenReviewer struct {
	username       string
	audiences      []string
	authenticated  bool
	err            error
	requestedAuds  []string
	reviewRequests int
}

func (f *fakeTokenReviewer) Create(
	_ context.Context, review *authenticationv1.TokenReview, _ meta.CreateOptions,
) (*authenticationv1.TokenReview, error) {
	f.reviewRequests++
	f.requestedAuds = append([]string(nil), review.Spec.Audiences...)
	if f.err != nil {
		return nil, f.err
	}
	out := &authenticationv1.TokenReview{}
	if !f.authenticated {
		out.Status.Error = "invalid token"
		return out, nil
	}
	out.Status.Authenticated = true
	out.Status.User.Username = f.username
	out.Status.Audiences = f.audiences
	return out, nil
}

type ingestTestEnv struct {
	handler      http.Handler
	server       *Server
	store        *store.Store
	kubeletDir   string
	cli          ctrlcli.Client
	reviewer     *fakeTokenReviewer
	reporter     *report.Reporter
	reserveCalls atomic.Int32
	reserveErr   error
	ready        bool
}

func newIngestTestEnv(t *testing.T, objs ...ctrlcli.Object) *ingestTestEnv {
	t.Helper()
	cacheRoot := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(cacheRoot, func(p string, d os.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			} else if d != nil {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
	})
	st, err := store.Open(cacheRoot)
	require.NoError(t, err)
	kubeletDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(kubeletDir, "pods"), 0o755))

	nms := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: testNodeName, Generation: 1},
		Spec: workercore.NodeModelStoreSpec{
			Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 80, LowPercent: 70},
			Download:   workercore.NodeModelStoreDownload{Concurrency: 4},
			Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: "https://huggingface.co"},
		},
	}
	allObjs := append([]ctrlcli.Object{nms}, objs...)
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.NodeModelStore{}, &workercore.ModelDeploymentForger{}).
		WithObjects(allObjs...).
		Build()

	col := &gc.Collector{
		Store:       st,
		Usage:       func() (gc.Usage, error) { return gc.Usage{Total: 10 << 30, Used: 1 << 30}, nil },
		Referenced:  func() (map[string]bool, error) { return map[string]bool{}, nil },
		Downloading: func() map[string]bool { return nil },
		Now:         func() time.Time { return testTime },
	}
	rep := &report.Reporter{
		Reader:      cli,
		Client:      cli,
		NodeName:    testNodeName,
		Namespace:   testSysNS,
		Store:       st,
		Collector:   col,
		Downloading: func() map[string]bool { return nil },
		Downloader:  download.New(nil, 1, 0),
		Now:         func() time.Time { return testTime },
	}
	col.Watermarks = rep.Watermarks
	col.Pinned = rep.Pinned
	require.NoError(t, rep.Report(context.Background()))

	reviewer := &fakeTokenReviewer{
		authenticated: true,
		username:      "system:serviceaccount:" + testSysNS + ":" + testWorkerSA,
		audiences:     []string{modelstore.IngestAudience},
	}
	env := &ingestTestEnv{
		store:      st,
		kubeletDir: kubeletDir,
		cli:        cli,
		reviewer:   reviewer,
		reporter:   rep,
		ready:      true,
	}
	admit, err := NewTokenAdmit(testSysNS, testWorkerSA, reviewer)
	require.NoError(t, err)
	srv := &Server{
		NodeID:     testNodeName,
		KubeletDir: kubeletDir,
		Store:      st,
		APIReader:  cli,
		Admit:      admit,
		Ready:      func() bool { return env.ready },
		Reserve: func(ctx context.Context, hex string, sizeBytes int64, written func() int64) (func(), error) {
			env.reserveCalls.Add(1)
			if env.reserveErr != nil {
				return nil, env.reserveErr
			}
			return col.Reserve(ctx, hex, sizeBytes, written)
		},
		FlushReport: rep.Flush,
		Now:         func() time.Time { return testTime },
	}
	env.server = srv
	env.handler = srv.Handler()
	return env
}

func validForgerAndSandboxPod() (*workercore.ModelDeploymentForger, *core.Pod) {
	forger := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testForgerName,
			UID:       types.UID(testForgerUID),
		},
		Status: workercore.ModelDeploymentForgerStatus{
			Roles: []workercore.ModelDeploymentForgerRoleStatus{
				{
					Name:  "decode",
					Phase: workercore.ModelDeploymentForgerRolePhasePublishing,
					ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
						PodName:  testPodName,
						PodUID:   testPodUID,
						NodeName: testNodeName,
					},
				},
			},
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testNamespace,
			Name:      testPodName,
			UID:       types.UID(testPodUID),
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion: workercore.SchemeGroupVersion.String(),
					Kind:       "ModelDeploymentForger",
					Name:       testForgerName,
					UID:        types.UID(testForgerUID),
					Controller: ptr.To(true),
				},
			},
		},
		Spec: core.PodSpec{
			NodeName: testNodeName,
		},
		Status: core.PodStatus{
			Phase: core.PodRunning,
			InitContainerStatuses: []core.ContainerStatus{
				{
					Name: "forge-runner",
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{ExitCode: 0},
					},
				},
				{
					Name: "forge-verifier",
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{ExitCode: 0},
					},
				},
			},
		},
	}
	return forger, pod
}

func writeValidSandboxFiles(t *testing.T, kubeletDir, podUID string) (outputDir, verifiedDir, dispatchDigest string) {
	t.Helper()
	base := filepath.Join(kubeletDir, "pods", podUID, "volumes", "kubernetes.io~empty-dir")
	outputDir = filepath.Join(base, "forge-output")
	verifiedDir = filepath.Join(base, "forge-verified")
	require.NoError(t, os.MkdirAll(filepath.Join(outputDir, "kernels", "rmsnorm_v1"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(outputDir, "solutions"), 0o755))
	require.NoError(t, os.MkdirAll(verifiedDir, 0o755))

	kernelPy := "def run(x):\n    return x\n"
	solutionJSON := `{"name":"rmsnorm_v1","definition":"rmsnorm"}`
	require.NoError(t, os.WriteFile(filepath.Join(outputDir, "kernels", "rmsnorm_v1", "kernel.py"), []byte(kernelPy), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outputDir, "solutions", "rmsnorm_v1.json"), []byte(solutionJSON), 0o644))

	dispatch := fmt.Sprintf(`{
  "schemaVersion": "gpustack-forge-dispatch/v1",
  "compatibility": {
    "engine": "vLLM",
    "engineVersion": "0.8.5",
    "manufacturer": "nvidia",
    "architecture": "sm_90a"
  },
  "entries": [
    {
      "localKernelKey": %q,
      "definition": "rmsnorm",
      "solutionPath": "solutions/rmsnorm_v1.json",
      "libraryPath": "kernels/rmsnorm_v1/kernel.py",
      "entrypoint": "run"
    }
  ]
}`, testKernelKey)
	require.NoError(t, os.WriteFile(filepath.Join(outputDir, "dispatch-manifest.json"), []byte(dispatch), 0o644))
	dispatchDigest = "sha256:" + stringx.SumBySHA256(dispatch)

	reportJSON := fmt.Sprintf(`{
  "schemaVersion": "gpustack-forge-verification/v1",
  "overallStatus": "PASSED",
  "dispatchManifestDigest": %q,
  "passedCount": 1,
  "failedCount": 0
}`, dispatchDigest)
	require.NoError(t, os.WriteFile(filepath.Join(verifiedDir, "verification-report.json"), []byte(reportJSON), 0o644))

	return outputDir, verifiedDir, dispatchDigest
}

func doIngestRequest(t *testing.T, h http.Handler, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, modelstore.IngestTreesPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func defaultIngestRequest() modelstore.IngestTreeRequest {
	return modelstore.IngestTreeRequest{
		Namespace:  testNamespace,
		ForgerName: testForgerName,
		ForgerUID:  testForgerUID,
		PodName:    testPodName,
		PodUID:     testPodUID,
	}
}

func TestIngestPublishesVerifiedKernelTreeAndFlushesStatus(t *testing.T) {
	forger, pod := validForgerAndSandboxPod()
	env := newIngestTestEnv(t, forger, pod)
	_, _, wantDispatchDigest := writeValidSandboxFiles(t, env.kubeletDir, testPodUID)

	rec := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{modelstore.IngestAudience}, env.reviewer.requestedAuds)
	assert.Equal(t, int32(1), env.reserveCalls.Load())

	var resp modelstore.IngestTreeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, wantDispatchDigest, resp.DispatchManifestDigest)
	assert.Equal(t, int64(3), resp.FileCount)
	assert.Greater(t, resp.SizeBytes, int64(0))
	assert.Equal(t, testNodeName, resp.SeedNode)

	hex := store.HexOf(resp.ManifestDigest)
	require.NotEmpty(t, hex)
	require.True(t, env.store.IsPublished(hex))

	marker, err := env.store.ReadMarker(hex)
	require.NoError(t, err)
	assert.Equal(t, store.SourceForge, marker.Source)
	assert.Equal(t, store.MarkerKindKernel, marker.Kind)
	assert.Equal(t, resp.FileCount, marker.FileCount)
	assert.Equal(t, resp.SizeBytes, marker.SizeBytes)

	manifestPath := env.store.PublishedManifestPath(hex)
	manifestInfo, err := os.Stat(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), manifestInfo.Mode().Perm(), "manifest.json must be written 0600")

	var listing peer.Listing
	rawListing, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(rawListing, &listing))
	assert.Equal(t, resp.ManifestDigest, listing.Digest)
	assert.Equal(t, store.MarkerKindKernel, listing.Kind)
	assert.Len(t, listing.Entries, 3)

	// Published files and directories are sealed read-only.
	pubDispatch := filepath.Join(env.store.PublishedTree(hex), "dispatch-manifest.json")
	pubInfo, err := os.Stat(pubDispatch)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o444), pubInfo.Mode().Perm())

	// Digest ledger records LastUsedTime = testTime.
	drec, err := env.store.ReadDigest(hex)
	require.NoError(t, err)
	assert.Equal(t, testTime, drec.LastUsedTime)

	// NodeModelStore.status.models was immediately flushed with Ready + Forge and zero tenant names.
	nms := new(workercore.NodeModelStore)
	require.NoError(t, env.cli.Get(context.Background(), ctrlcli.ObjectKey{Name: testNodeName}, nms))
	require.Len(t, nms.Status.Models, 1)
	assert.Equal(t, resp.ManifestDigest, nms.Status.Models[0].Digest)
	assert.Equal(t, workercore.NodeModelStoreModelStateReady, nms.Status.Models[0].State)
	assert.Equal(t, workercore.NodeModelStoreModelSourceForge, nms.Status.Models[0].Source)
	rawNMS, err := json.Marshal(nms.Status)
	require.NoError(t, err)
	for _, forbidden := range []string{testNamespace, testForgerName, testPodName, testPodUID, testForgerUID} {
		assert.NotContains(t, string(rawNMS), forbidden, "NodeModelStore.status must never leak tenant or sandbox names")
	}

	// Idempotent retry returns the same response cleanly.
	rec2 := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var resp2 modelstore.IngestTreeResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, resp, resp2)
}

func TestIngestAuthenticationGuards(t *testing.T) {
	cases := []struct {
		name      string
		serviceSA string
		token     string
		mutate    func(*fakeTokenReviewer)
		wantCode  int
	}{
		{
			name:      "missing bearer token",
			serviceSA: testWorkerSA,
			token:     "",
			wantCode:  http.StatusUnauthorized,
		},
		{
			name:      "ingest service account unconfigured",
			serviceSA: "",
			token:     "tok",
			wantCode:  http.StatusUnauthorized,
		},
		{
			name:      "unauthenticated token",
			serviceSA: testWorkerSA,
			token:     "bad-tok",
			mutate:    func(r *fakeTokenReviewer) { r.authenticated = false },
			wantCode:  http.StatusUnauthorized,
		},
		{
			name:      "wrong audience token such as peer or default apiserver",
			serviceSA: testWorkerSA,
			token:     "peer-tok",
			mutate:    func(r *fakeTokenReviewer) { r.audiences = []string{peer.PeerAudience} },
			wantCode:  http.StatusUnauthorized,
		},
		{
			name:      "wrong service account in same namespace",
			serviceSA: testWorkerSA,
			token:     "mm-tok",
			mutate: func(r *fakeTokenReviewer) {
				r.username = "system:serviceaccount:" + testSysNS + ":custom-release-model-manager"
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name:      "tenant service account with same short name in another namespace",
			serviceSA: testWorkerSA,
			token:     "tenant-tok",
			mutate: func(r *fakeTokenReviewer) {
				r.username = "system:serviceaccount:" + testNamespace + ":" + testWorkerSA
			},
			wantCode: http.StatusUnauthorized,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forger, pod := validForgerAndSandboxPod()
			env := newIngestTestEnv(t, forger, pod)
			if c.mutate != nil {
				c.mutate(env.reviewer)
			}
			admit, err := NewTokenAdmit(testSysNS, c.serviceSA, env.reviewer)
			require.NoError(t, err)
			env.server.Admit = admit

			rec := doIngestRequest(t, env.server.Handler(), c.token, defaultIngestRequest())
			assert.Equal(t, c.wantCode, rec.Code)
			assert.Zero(t, env.reserveCalls.Load(), "unauthorized requests never reserve capacity")
		})
	}
}

func TestIngestLiveOwnershipAndContainerBarrierGuards(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*workercore.ModelDeploymentForger, *core.Pod, *modelstore.IngestTreeRequest)
		dropForg bool
		dropPod  bool
		wantCode int
	}{
		{
			name:     "missing ModelDeploymentForger",
			dropForg: true,
			wantCode: http.StatusForbidden,
		},
		{
			name: "activeSandbox is nil on ModelDeploymentForger",
			mutate: func(f *workercore.ModelDeploymentForger, _ *core.Pod, _ *modelstore.IngestTreeRequest) {
				f.Status.Roles[0].ActiveSandbox = nil
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "spoofed sandbox podUID does not match activeSandbox.podUID",
			mutate: func(f *workercore.ModelDeploymentForger, _ *core.Pod, _ *modelstore.IngestTreeRequest) {
				f.Status.Roles[0].ActiveSandbox.PodUID = "another-sandbox-uid"
			},
			wantCode: http.StatusForbidden,
		},
		{
			name:     "missing Pod",
			dropPod:  true,
			wantCode: http.StatusForbidden,
		},
		{
			name: "Pod UID mismatch",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.UID = "different-pod-uid"
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "Pod scheduled on a different node",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.Spec.NodeName = "other-node"
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "Pod missing controller ownerReference to forger",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.OwnerReferences[0].Controller = ptr.To(false)
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "Pod controller ownerReference points to another UID",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.OwnerReferences[0].UID = "spoofed-forger-uid"
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "forge-runner still running",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.Status.InitContainerStatuses[0].State = core.ContainerState{
					Running: &core.ContainerStateRunning{},
				}
			},
			wantCode: http.StatusConflict,
		},
		{
			name: "forge-runner exited non-zero",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
			},
			wantCode: http.StatusConflict,
		},
		{
			name: "forge-verifier missing from initContainerStatuses",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.Status.InitContainerStatuses = p.Status.InitContainerStatuses[:1]
			},
			wantCode: http.StatusConflict,
		},
		{
			name: "forge-verifier exited non-zero",
			mutate: func(_ *workercore.ModelDeploymentForger, p *core.Pod, _ *modelstore.IngestTreeRequest) {
				p.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 2
			},
			wantCode: http.StatusConflict,
		},
		{
			name: "path traversal in podUID",
			mutate: func(_ *workercore.ModelDeploymentForger, _ *core.Pod, r *modelstore.IngestTreeRequest) {
				r.PodUID = "../../etc"
			},
			wantCode: http.StatusBadRequest,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forger, pod := validForgerAndSandboxPod()
			req := defaultIngestRequest()
			if c.mutate != nil {
				c.mutate(forger, pod, &req)
			}
			var objs []ctrlcli.Object
			if !c.dropForg {
				objs = append(objs, forger)
			}
			if !c.dropPod {
				objs = append(objs, pod)
			}
			env := newIngestTestEnv(t, objs...)
			// Do not create any files on disk: live APIReader checks must reject before reading a single byte.
			rec := doIngestRequest(t, env.handler, "worker-ingest-jwt", req)
			assert.Equal(t, c.wantCode, rec.Code, rec.Body.String())
			assert.Zero(t, env.reserveCalls.Load())
		})
	}
}

func TestIngestRejectsFilesystemHazardsAndCapsBeforeReserve(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, outputDir, verifiedDir, dispatchDigest string)
	}{
		{
			name: "missing verification-report.json",
			tamper: func(t *testing.T, _, verifiedDir, _ string) {
				require.NoError(t, os.Remove(filepath.Join(verifiedDir, "verification-report.json")))
			},
		},
		{
			name: "verification-report.json status FAILED",
			tamper: func(t *testing.T, _, verifiedDir, dispatchDigest string) {
				bad := fmt.Sprintf(`{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"FAILED","dispatchManifestDigest":%q,"passedCount":0}`, dispatchDigest)
				require.NoError(t, os.WriteFile(filepath.Join(verifiedDir, "verification-report.json"), []byte(bad), 0o644))
			},
		},
		{
			name: "verification-report.json passedCount zero",
			tamper: func(t *testing.T, _, verifiedDir, dispatchDigest string) {
				bad := fmt.Sprintf(`{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":%q,"passedCount":0}`, dispatchDigest)
				require.NoError(t, os.WriteFile(filepath.Join(verifiedDir, "verification-report.json"), []byte(bad), 0o644))
			},
		},
		{
			name: "verification-report.json digest mismatch with dispatch-manifest.json",
			tamper: func(t *testing.T, _, verifiedDir, _ string) {
				bad := fmt.Sprintf(`{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":"sha256:%s","passedCount":1}`, strings.Repeat("f", 64))
				require.NoError(t, os.WriteFile(filepath.Join(verifiedDir, "verification-report.json"), []byte(bad), 0o644))
			},
		},
		{
			name: "excluded GEMM kernel key in dispatch-manifest.json",
			tamper: func(t *testing.T, outputDir, verifiedDir, _ string) {
				badDispatch := `{
  "schemaVersion": "gpustack-forge-dispatch/v1",
  "entries": [{"localKernelKey": "vllm/0.8.5/nvidia/sm_90a/gemm/0123456789abcdef", "libraryPath": "kernels/rmsnorm_v1/kernel.py"}]
}`
				require.NoError(t, os.WriteFile(filepath.Join(outputDir, "dispatch-manifest.json"), []byte(badDispatch), 0o644))
				d := "sha256:" + stringx.SumBySHA256(badDispatch)
				rep := fmt.Sprintf(`{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":%q,"passedCount":1}`, d)
				require.NoError(t, os.WriteFile(filepath.Join(verifiedDir, "verification-report.json"), []byte(rep), 0o644))
			},
		},
		{
			name: "intra-root relative symlink inside forge-output",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				require.NoError(t, os.Symlink("dispatch-manifest.json", filepath.Join(outputDir, "intra-link.json")))
			},
		},
		{
			name: "escaping symlink inside forge-output",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(outputDir, "escape-link")))
			},
		},
		{
			name: "symlink directory inside forge-output",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				outside := t.TempDir()
				require.NoError(t, os.Symlink(outside, filepath.Join(outputDir, "linked-dir")))
			},
		},
		{
			name: "hardlinked file with Nlink > 1 inside forge-output",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				src := filepath.Join(outputDir, "kernels", "rmsnorm_v1", "kernel.py")
				dst := filepath.Join(outputDir, "kernels", "rmsnorm_v1", "hardlink.py")
				require.NoError(t, os.Link(src, dst))
			},
		},
		{
			name: "FIFO named pipe inside forge-output does not block and is rejected",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				require.NoError(t, syscall.Mkfifo(filepath.Join(outputDir, "pipe.fifo"), 0o600))
			},
		},
		{
			name: "file count exceeding 1024 cap",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				bulk := filepath.Join(outputDir, "bulk")
				require.NoError(t, os.MkdirAll(bulk, 0o755))
				for i := range MaxFiles + 2 {
					require.NoError(t, os.WriteFile(filepath.Join(bulk, fmt.Sprintf("f-%04d.bin", i)), []byte("x"), 0o644))
				}
			},
		},
		{
			name: "path depth exceeding 16 segments",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				parts := make([]string, MaxPathDepth)
				for i := range parts {
					parts[i] = fmt.Sprintf("d%d", i)
				}
				deepDir := filepath.Join(append([]string{outputDir}, parts...)...)
				require.NoError(t, os.MkdirAll(deepDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(deepDir, "too-deep.bin"), []byte("x"), 0o644))
			},
		},
		{
			name: "relative path length exceeding 256 bytes",
			tamper: func(t *testing.T, outputDir, _, _ string) {
				seg1 := strings.Repeat("a", 140)
				seg2 := strings.Repeat("b", 130) + ".bin"
				dir := filepath.Join(outputDir, seg1)
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, seg2), []byte("x"), 0o644))
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forger, pod := validForgerAndSandboxPod()
			env := newIngestTestEnv(t, forger, pod)
			outDir, verDir, digest := writeValidSandboxFiles(t, env.kubeletDir, testPodUID)
			c.tamper(t, outDir, verDir, digest)

			rec := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Zero(t, env.reserveCalls.Load(), "validation failure must happen before gc.Collector.Reserve")
			published, err := env.store.Published()
			require.NoError(t, err)
			assert.Empty(t, published)
		})
	}
}

func TestIngestReserveInsufficientCapacityAndMethodChecks(t *testing.T) {
	forger, pod := validForgerAndSandboxPod()
	env := newIngestTestEnv(t, forger, pod)
	writeValidSandboxFiles(t, env.kubeletDir, testPodUID)

	t.Run("GET is rejected with 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, modelstore.IngestTreesPath, nil)
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})

	t.Run("not ready returns 503", func(t *testing.T) {
		env.ready = false
		defer func() { env.ready = true }()
		rec := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})

	t.Run("insufficient capacity from Reserve returns 507", func(t *testing.T) {
		env.reserveErr = &download.Error{
			Reason:  download.ReasonInsufficientCapacity,
			Message: "cache full",
			Detail:  "cache full",
		}
		rec := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
		assert.Equal(t, http.StatusInsufficientStorage, rec.Code)
		assert.Equal(t, int32(1), env.reserveCalls.Load())
	})
}

func TestIngestAdditionalRequestReportAndConflictBranches(t *testing.T) {
	t.Run("malformed JSON and missing fields return 400", func(t *testing.T) {
		forger, pod := validForgerAndSandboxPod()
		env := newIngestTestEnv(t, forger, pod)

		for _, raw := range []string{"", "{bad", `{"namespace":"a"}`, `{"namespace":"a","forgerName":"b","podName":"c","podUID":"d","forgerUID":"../bad"}`} {
			req := httptest.NewRequest(http.MethodPost, modelstore.IngestTreesPath, strings.NewReader(raw))
			req.Header.Set("Authorization", "Bearer worker-ingest-jwt")
			rec := httptest.NewRecorder()
			env.handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, raw)
		}
	})

	t.Run("alternate status and verifiedCount fields plus forger alias succeed", func(t *testing.T) {
		forger, pod := validForgerAndSandboxPod()
		env := newIngestTestEnv(t, forger, pod)
		_, verifiedDir, dispatchDigest := writeValidSandboxFiles(t, env.kubeletDir, testPodUID)

		altReport := fmt.Sprintf(`{
  "schemaVersion": "gpustack-forge-verification/v1",
  "status": "PASSED",
  "dispatchManifestDigest": %q,
  "verifiedCount": 1,
  "entries": [{"passed": true, "evaluation": {"status": "PASSED"}}]
}`, dispatchDigest)
		require.NoError(t, os.WriteFile(filepath.Join(verifiedDir, "verification-report.json"), []byte(altReport), 0o644))

		rec := doIngestRequest(t, env.handler, "worker-ingest-jwt", map[string]string{
			"namespace": testNamespace,
			"forger":    testForgerName,
			"podName":   testPodName,
			"podUID":    testPodUID,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("conflicting digest already published as non-kernel returns 409", func(t *testing.T) {
		forger, pod := validForgerAndSandboxPod()
		env := newIngestTestEnv(t, forger, pod)
		writeValidSandboxFiles(t, env.kubeletDir, testPodUID)

		// First publish via ingest, then overwrite its marker Kind to Model to simulate a pre-existing
		// non-kernel publication at the same digest.
		rec1 := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
		require.Equal(t, http.StatusOK, rec1.Code)
		var resp modelstore.IngestTreeResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp))
		hex := store.HexOf(resp.ManifestDigest)

		markerPath := filepath.Join(env.store.Root(), "published", hex, "marker")
		require.NoError(t, os.Chmod(filepath.Dir(markerPath), 0o755))
		require.NoError(t, os.Chmod(markerPath, 0o644))
		b, err := json.Marshal(store.Marker{Digest: resp.ManifestDigest, Kind: store.MarkerKindModel})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(markerPath, b, 0o644))

		rec2 := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
		assert.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())
	})

	t.Run("activeSandbox podName or nodeName mismatch returns 403", func(t *testing.T) {
		forger, pod := validForgerAndSandboxPod()
		forger.Status.Roles[0].ActiveSandbox.PodName = "other-pod-name"
		env1 := newIngestTestEnv(t, forger, pod)
		assert.Equal(t, http.StatusForbidden, doIngestRequest(t, env1.handler, "worker-ingest-jwt", defaultIngestRequest()).Code)

		forger2, pod2 := validForgerAndSandboxPod()
		forger2.Status.Roles[0].ActiveSandbox.NodeName = "other-node"
		env2 := newIngestTestEnv(t, forger2, pod2)
		assert.Equal(t, http.StatusForbidden, doIngestRequest(t, env2.handler, "worker-ingest-jwt", defaultIngestRequest()).Code)
	})

	t.Run("nil Admit nil APIReader default clock FlushReport error and verification edge cases", func(t *testing.T) {
		forger, pod := validForgerAndSandboxPod()
		env := newIngestTestEnv(t, forger, pod)
		writeValidSandboxFiles(t, env.kubeletDir, testPodUID)

		cloneServer := func(src *Server) *Server {
			return &Server{
				NodeID:      src.NodeID,
				KubeletDir:  src.KubeletDir,
				Store:       src.Store,
				APIReader:   src.APIReader,
				Admit:       src.Admit,
				Ready:       src.Ready,
				Reserve:     src.Reserve,
				FlushReport: src.FlushReport,
				Now:         src.Now,
			}
		}

		// Nil Admit refuses with 401.
		noAdmit := cloneServer(env.server)
		noAdmit.Admit = nil
		assert.Equal(t, http.StatusUnauthorized, doIngestRequest(t, noAdmit.Handler(), "worker-ingest-jwt", defaultIngestRequest()).Code)

		// TokenReview backend error is refused as 401 by peer.AuthOptions.Admit.
		errReviewer := &fakeTokenReviewer{err: fmt.Errorf("apiserver down")}
		errAdmit, err := NewTokenAdmit(testSysNS, testWorkerSA, errReviewer)
		require.NoError(t, err)
		errSrv := cloneServer(env.server)
		errSrv.Admit = errAdmit
		assert.Equal(t, http.StatusUnauthorized, doIngestRequest(t, errSrv.Handler(), "worker-ingest-jwt", defaultIngestRequest()).Code)

		// Non-AuthError returned directly from custom Admit returns 500.
		internalAdmitSrv := cloneServer(env.server)
		internalAdmitSrv.Admit = func(*http.Request) error { return fmt.Errorf("internal auth hook failure") }
		assert.Equal(t, http.StatusInternalServerError, doIngestRequest(t, internalAdmitSrv.Handler(), "worker-ingest-jwt", defaultIngestRequest()).Code)

		// Nil APIReader returns 503.
		noReader := cloneServer(env.server)
		noReader.APIReader = nil
		assert.Equal(t, http.StatusServiceUnavailable, doIngestRequest(t, noReader.Handler(), "worker-ingest-jwt", defaultIngestRequest()).Code)

		// ForgerUID mismatch, PodNotFound, PodUID mismatch, and non-capacity Reserve error.
		badForgerUIDReq := defaultIngestRequest()
		badForgerUIDReq.ForgerUID = "other-forger-uid"
		assert.Equal(t, http.StatusForbidden, doIngestRequest(t, env.handler, "worker-ingest-jwt", badForgerUIDReq).Code)

		forgerNoPod, _ := validForgerAndSandboxPod()
		envNoPod := newIngestTestEnv(t, forgerNoPod)
		assert.Equal(t, http.StatusForbidden, doIngestRequest(t, envNoPod.handler, "worker-ingest-jwt", defaultIngestRequest()).Code)

		forgerPodUID, podUIDMismatch := validForgerAndSandboxPod()
		podUIDMismatch.UID = "different-pod-uid"
		envPodUID := newIngestTestEnv(t, forgerPodUID, podUIDMismatch)
		assert.Equal(t, http.StatusForbidden, doIngestRequest(t, envPodUID.handler, "worker-ingest-jwt", defaultIngestRequest()).Code)

		reserveFailSrv := cloneServer(env.server)
		reserveFailSrv.Reserve = func(context.Context, string, int64, func() int64) (func(), error) {
			return nil, fmt.Errorf("disk stat failure")
		}
		assert.Equal(t, http.StatusServiceUnavailable, doIngestRequest(t, reserveFailSrv.Handler(), "worker-ingest-jwt", defaultIngestRequest()).Code)

		// Default clock (Now == nil) and FlushReport failure returns 503.
		flushErrSrv := cloneServer(env.server)
		flushErrSrv.Now = nil
		flushErrSrv.FlushReport = func(context.Context) error { return fmt.Errorf("conflict") }
		assert.Equal(t, http.StatusServiceUnavailable, doIngestRequest(t, flushErrSrv.Handler(), "worker-ingest-jwt", defaultIngestRequest()).Code)
	})

	t.Run("path helper and directory boundary edge cases", func(t *testing.T) {
		assert.Error(t, validateRelativePath("", 1))
		assert.Error(t, validateRelativePath("a\\b", 1))
		assert.Error(t, validateRelativePath("a\x00b", 1))
		assert.Error(t, validateRelativePath("a/../b", 3))
		assert.Error(t, validateRelativePath("a//b", 3))
		assert.Error(t, validateRelativePath("a/b", 1))
		assert.NoError(t, validateRelativePath("kernels/rmsnorm.so", 2))

		dir := t.TempDir()
		dirF, err := openDirNoFollow(dir)
		require.NoError(t, err)
		defer func() { _ = dirF.Close() }()
		_, err = openSubdirNoFollow(dirF, "../escape")
		assert.Error(t, err)
		_, _, err = openRegularSingleLinkAt(dirF, "../escape")
		assert.Error(t, err)
		_, err = openDirNoFollow(filepath.Join(dir, "does-not-exist"))
		assert.Error(t, err)

		// Missing sandbox directories on disk return 422.
		forger, pod := validForgerAndSandboxPod()
		envMissing := newIngestTestEnv(t, forger, pod)
		assert.Equal(t, http.StatusUnprocessableEntity, doIngestRequest(t, envMissing.handler, "worker-ingest-jwt", defaultIngestRequest()).Code)

		// Alternate verification-report fields (status + verifiedCount + passed entry) succeed.
		envAlt := newIngestTestEnv(t, forger, pod)
		_, verDir, dDigest := writeValidSandboxFiles(t, envAlt.kubeletDir, testPodUID)
		altReport := fmt.Sprintf(
			`{"schemaVersion":"gpustack-forge-verification/v1","status":"PASSED","dispatchManifestDigest":%q,"verifiedCount":1,"entries":[{"passed":true,"evaluation":{"status":"PASSED"}}]}`,
			dDigest,
		)
		require.NoError(t, os.WriteFile(filepath.Join(verDir, "verification-report.json"), []byte(altReport), 0o644))
		assert.Equal(t, http.StatusOK, doIngestRequest(t, envAlt.handler, "worker-ingest-jwt", defaultIngestRequest()).Code)
	})

	t.Run("additional verification report and dispatch manifest rejections", func(t *testing.T) {
		cases := []struct {
			name   string
			report string
			disp   string
		}{
			{
				name:   "wrong report schemaVersion",
				report: `{"schemaVersion":"v0","overallStatus":"PASSED","dispatchManifestDigest":"%s","passedCount":1}`,
			},
			{
				name:   "conflicting status fields",
				report: `{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","status":"FAILED","dispatchManifestDigest":"%s","passedCount":1}`,
			},
			{
				name:   "conflicting passedCount and verifiedCount",
				report: `{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":"%s","passedCount":1,"verifiedCount":2}`,
			},
			{
				name:   "entries present with zero passed",
				report: `{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":"%s","passedCount":1,"entries":[{"passed":false,"evaluation":{"status":"FAILED"}}]}`,
			},
			{
				name:   "invalid dispatchManifestDigest format",
				report: `{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":"not-a-sha256","passedCount":1}`,
			},
			{
				name: "empty dispatch entries",
				disp: `{"schemaVersion":"gpustack-forge-dispatch/v1","entries":[]}`,
			},
			{
				name: "empty libraryPath in dispatch entry",
				disp: fmt.Sprintf(`{"schemaVersion":"gpustack-forge-dispatch/v1","entries":[{"localKernelKey":%q,"libraryPath":""}]}`, testKernelKey),
			},
			{
				name: "missing dispatch-manifest.json",
				disp: "__DELETE__",
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				forger, pod := validForgerAndSandboxPod()
				env := newIngestTestEnv(t, forger, pod)
				outDir, verDir, dDigest := writeValidSandboxFiles(t, env.kubeletDir, testPodUID)
				if c.disp == "__DELETE__" {
					require.NoError(t, os.Remove(filepath.Join(outDir, "dispatch-manifest.json")))
				} else if c.disp != "" {
					require.NoError(t, os.WriteFile(filepath.Join(outDir, "dispatch-manifest.json"), []byte(c.disp), 0o644))
					dDigest = "sha256:" + stringx.SumBySHA256(c.disp)
					rep := fmt.Sprintf(`{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":%q,"passedCount":1}`, dDigest)
					require.NoError(t, os.WriteFile(filepath.Join(verDir, "verification-report.json"), []byte(rep), 0o644))
				}
				if c.report != "" {
					content := c.report
					if strings.Contains(content, "%s") {
						content = fmt.Sprintf(content, dDigest)
					}
					require.NoError(t, os.WriteFile(filepath.Join(verDir, "verification-report.json"), []byte(content), 0o644))
				}
				rec := doIngestRequest(t, env.handler, "worker-ingest-jwt", defaultIngestRequest())
				assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			})
		}
	})
}
