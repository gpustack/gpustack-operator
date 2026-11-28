package modelmanager

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/driver"
	"gpustack.ai/gpustack/pkg/modelmanager/gc"
	"gpustack.ai/gpustack/pkg/modelmanager/ingest"
	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
	"gpustack.ai/gpustack/pkg/modelmanager/peer"
	"gpustack.ai/gpustack/pkg/modelmanager/report"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

type pipelineMounter struct {
	mu      sync.Mutex
	mounted map[string]string
}

func (m *pipelineMounter) IsMountPoint(path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mounted[path]
	return ok, nil
}

func (m *pipelineMounter) IsReadOnly(path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mounted[path]
	return ok, nil
}

func (m *pipelineMounter) BindReadOnly(source, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = source
	return nil
}

func (m *pipelineMounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounted, target)
	return nil
}

type pipelineTokenReviewer struct {
	username  string
	audiences []string
}

func (r pipelineTokenReviewer) Create(
	_ context.Context, _ *authenticationv1.TokenReview, _ meta.CreateOptions,
) (*authenticationv1.TokenReview, error) {
	return &authenticationv1.TokenReview{
		Status: authenticationv1.TokenReviewStatus{
			Authenticated: true,
			User:          authenticationv1.UserInfo{Username: r.username},
			Audiences:     r.audiences,
		},
	}, nil
}

type inProcessNode struct {
	nodeName     string
	store        *store.Store
	kubeletDir   string
	collector    *gc.Collector
	reporter     *report.Reporter
	materializer *materialize.Materializer
	driver       *driver.Driver
	mounter      *pipelineMounter
	ingestHTTP   http.Handler
	peerPort     int
	usedBytes    atomic.Uint64
	clockMu      sync.Mutex
	now          time.Time
}

func (n *inProcessNode) currentTime() time.Time {
	n.clockMu.Lock()
	defer n.clockMu.Unlock()
	return n.now
}

func (n *inProcessNode) advanceTime(d time.Duration) {
	n.clockMu.Lock()
	defer n.clockMu.Unlock()
	n.now = n.now.Add(d)
}

func TestTwoNodeForgerIngestPeerSyncIsolationAndGC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		sysNamespace   = "gpustack-system"
		workerSA       = "rel-gpustack-operator-worker"
		tenantNS       = "team-alpha"
		forgerName     = "qwen-forger"
		forgerUID      = "uid-forger-alpha"
		sandboxPodName = "qwen-forger-sandbox-1"
		sandboxPodUID  = "uid-sandbox-alpha-1"
		kernelKey      = "vllm/0.8/nvidia/sm_90a/rmsnorm/0123456789abcdef"
		totalDiskBytes = uint64(10_000)
	)
	baseTime := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

	// External Hub server: must receive ZERO requests during nodeStore Peer Sync.
	var hubHits atomic.Int32
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hubHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hubSrv.Close)

	nmsSpec := func() workercore.NodeModelStoreSpec {
		return workercore.NodeModelStoreSpec{
			Watermarks: workercore.NodeModelStoreWatermarks{HighPercent: 80, LowPercent: 70},
			Download:   workercore.NodeModelStoreDownload{Concurrency: 4},
			Hub:        workercore.NodeModelStoreHub{HuggingFaceEndpoint: hubSrv.URL},
		}
	}

	forger := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: tenantNS, Name: forgerName, UID: types.UID(forgerUID)},
		Status: workercore.ModelDeploymentForgerStatus{
			Roles: []workercore.ModelDeploymentForgerRoleStatus{{
				Name:  "decode",
				Phase: workercore.ModelDeploymentForgerRolePhasePublishing,
				ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
					PodName:  sandboxPodName,
					PodUID:   sandboxPodUID,
					NodeName: "node-a",
				},
			}},
		},
	}
	sandboxPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: tenantNS,
			Name:      sandboxPodName,
			UID:       types.UID(sandboxPodUID),
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: workercore.SchemeGroupVersion.String(),
				Kind:       "ModelDeploymentForger",
				Name:       forgerName,
				UID:        types.UID(forgerUID),
				Controller: ptr.To(true),
			}},
		},
		Spec: core.PodSpec{NodeName: "node-a"},
		Status: core.PodStatus{
			Phase: core.PodRunning,
			InitContainerStatuses: []core.ContainerStatus{
				{Name: "forge-runner", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "forge-verifier", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
			},
		},
	}

	// Pick an available local port for TLS peer.Server on node-a.
	probeLis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peerPort := probeLis.Addr().(*net.TCPAddr).Port
	require.NoError(t, probeLis.Close())

	mmPodA := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: sysNamespace,
			Name:      "model-manager-node-a",
			Labels:    map[string]string{deviceplugin.ComponentLabelKey: modelstore.PluginComponent},
		},
		Spec: core.PodSpec{
			NodeName: "node-a",
			Containers: []core.Container{{
				Name:  "main",
				Ports: []core.ContainerPort{{Name: "peer-sync", ContainerPort: int32(peerPort)}},
			}},
		},
		Status: core.PodStatus{
			PodIP:      "127.0.0.1",
			Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(
			&workercore.NodeModelStore{},
			&workercore.ModelKernelArtifact{},
			&workercore.ModelArtifact{},
			&workercore.ModelDeploymentForger{},
		).
		WithObjects(
			&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-a", Generation: 1}, Spec: nmsSpec()},
			&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-b", Generation: 1}, Spec: nmsSpec()},
			forger,
			sandboxPod,
			mmPodA,
		).
		Build()

	buildNode := func(name string, servePeer bool) *inProcessNode {
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
		st, oerr := store.Open(cacheRoot)
		require.NoError(t, oerr)
		kubeletDir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(kubeletDir, "pods"), 0o755))

		node := &inProcessNode{
			nodeName:   name,
			store:      st,
			kubeletDir: kubeletDir,
			mounter:    &pipelineMounter{mounted: map[string]string{}},
			peerPort:   peerPort,
			now:        baseTime,
		}
		node.usedBytes.Store(4000)

		mat := &materialize.Materializer{
			Store:         st,
			Now:           node.currentTime,
			Base:          ctx,
			CheckInterval: 20 * time.Millisecond,
		}
		col := &gc.Collector{
			Store: st,
			Usage: func() (gc.Usage, error) {
				markers, merr := st.Published()
				if merr != nil {
					return gc.Usage{}, merr
				}
				used := node.usedBytes.Load()
				for _, m := range markers {
					used += uint64(m.SizeBytes)
				}
				return gc.Usage{Total: totalDiskBytes, Used: used}, nil
			},
			Referenced: func() (map[string]bool, error) {
				refs, rerr := st.Refs()
				if rerr != nil {
					return nil, rerr
				}
				out := map[string]bool{}
				for _, r := range refs {
					out[r.Hex] = true
				}
				return out, nil
			},
			Downloading: mat.Downloading,
			Now:         node.currentTime,
		}
		dl := download.New(hubSrv.Client(), 4, 0)
		rep := &report.Reporter{
			Reader:      cli,
			Client:      cli,
			NodeName:    name,
			Namespace:   sysNamespace,
			Store:       st,
			Collector:   col,
			Downloading: mat.Downloading,
			Progress:    mat.Progress,
			Downloader:  dl,
			Now:         node.currentTime,
		}
		col.Watermarks = rep.Watermarks
		col.Pinned = rep.Pinned
		mat.Environment = rep.Environment
		mat.Reserve = col.Reserve
		mat.Changed = rep.Trigger

		peerClient, cerr := peer.NewClient(peer.AuthOptions{Mode: peer.AuthNone})
		require.NoError(t, cerr)
		mat.Peers = &peer.Puller{
			Discover: func(dctx context.Context, digest string) ([]*peer.Source, error) {
				if !rep.PeerSyncEnabled() {
					return nil, nil
				}
				return peer.Discover(dctx, cli, peer.DiscoverOptions{
					Namespace:        sysNamespace,
					SelfNode:         name,
					Port:             peerPort,
					StreamsPerSource: 4,
					Client:           peerClient,
				}, digest)
			},
			StreamsPerSource: 4,
		}
		require.NoError(t, rep.Report(ctx))

		admit, aerr := ingest.NewTokenAdmit(sysNamespace, workerSA, pipelineTokenReviewer{
			username:  "system:serviceaccount:" + sysNamespace + ":" + workerSA,
			audiences: []string{modelstore.IngestAudience},
		})
		require.NoError(t, aerr)
		ingSrv := &ingest.Server{
			NodeID:      name,
			KubeletDir:  kubeletDir,
			Store:       st,
			APIReader:   cli,
			Admit:       admit,
			Ready:       func() bool { return true },
			Reserve:     col.Reserve,
			FlushReport: rep.Flush,
			Now:         node.currentTime,
		}

		drv := &driver.Driver{
			Name:         modelstore.DriverName,
			Version:      "v0.0.0-test",
			NodeID:       name,
			KubeletDir:   kubeletDir,
			Store:        st,
			Artifacts:    cli,
			APIReader:    cli,
			Ready:        func() bool { return true },
			Mounter:      node.mounter,
			Materializer: mat,
			Events:       rep,
		}

		if servePeer {
			lis, lerr := peer.Listen(peerPort, peer.AuthOptions{Mode: peer.AuthNone})
			require.NoError(t, lerr)
			ps := &peer.Server{Store: st, MaxStreams: 8, Now: node.currentTime}
			go func() { _ = ps.Serve(ctx, lis) }()
		}

		node.collector = col
		node.reporter = rep
		node.materializer = mat
		node.driver = drv
		node.ingestHTTP = ingSrv.Handler()
		return node
	}

	nodeA := buildNode("node-a", true)
	nodeB := buildNode("node-b", false)

	// Stage verified ForgeSandbox emptyDir output on node-a.
	emptyDirRoot := filepath.Join(nodeA.kubeletDir, "pods", sandboxPodUID, "volumes", "kubernetes.io~empty-dir")
	outDir := filepath.Join(emptyDirRoot, "forge-output")
	verDir := filepath.Join(emptyDirRoot, "forge-verified")
	require.NoError(t, os.MkdirAll(filepath.Join(outDir, "kernels", "rmsnorm"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(outDir, "solutions"), 0o755))
	require.NoError(t, os.MkdirAll(verDir, 0o755))

	kernelCode := "def run(x):\n    return x * 2\n"
	solutionMeta := `{"name":"rmsnorm_v1","definition":"rmsnorm"}`
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "kernels", "rmsnorm", "kernel.py"), []byte(kernelCode), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "solutions", "rmsnorm_v1.json"), []byte(solutionMeta), 0o644))
	dispatchRaw := fmt.Sprintf(`{
  "schemaVersion": "gpustack-forge-dispatch/v1",
  "compatibility": {"engine": "vllm", "engineVersion": "0.8.5", "manufacturer": "nvidia", "architecture": "sm_90a"},
  "entries": [{"localKernelKey": %q, "solutionPath": "solutions/rmsnorm_v1.json", "libraryPath": "kernels/rmsnorm/kernel.py"}]
}`, kernelKey)
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "dispatch-manifest.json"), []byte(dispatchRaw), 0o644))
	dispatchDigest := "sha256:" + stringx.SumBySHA256(dispatchRaw)
	verRaw := fmt.Sprintf(`{"schemaVersion":"gpustack-forge-verification/v1","overallStatus":"PASSED","dispatchManifestDigest":%q,"passedCount":1}`, dispatchDigest)
	require.NoError(t, os.WriteFile(filepath.Join(verDir, "verification-report.json"), []byte(verRaw), 0o644))

	// Step 1: Ingest forged emptyDir tree on node-a via POST /ingest/v1/trees.
	reqBody, err := json.Marshal(modelstore.IngestTreeRequest{
		Namespace:  tenantNS,
		ForgerName: forgerName,
		ForgerUID:  forgerUID,
		PodName:    sandboxPodName,
		PodUID:     sandboxPodUID,
	})
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost, modelstore.IngestTreesPath, bytes.NewReader(reqBody))
	httpReq.Header.Set("Authorization", "Bearer worker-ingest-token")
	rec := httptest.NewRecorder()
	nodeA.ingestHTTP.ServeHTTP(rec, httpReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var ingestResp modelstore.IngestTreeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ingestResp))
	kernelHex := store.HexOf(ingestResp.ManifestDigest)
	require.NotEmpty(t, kernelHex)
	assert.Equal(t, "node-a", ingestResp.SeedNode)

	// Step 2: Publish an unpinned Hub model on node-a, push disk usage above HighPercent (80%),
	// pin the forged kernel digest in NodeModelStore.spec.pinned, and verify GC evicts the unpinned
	// Hub model while keeping the pinned Forge kernel tree intact.
	hubModelBody := bytes.Repeat([]byte("m"), 2500)
	hubManifest, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{{
		Path: "weights.safetensors", Size: int64(len(hubModelBody)), Digest: "sha256:" + stringx.SumBytesBySHA256(hubModelBody),
	}})
	require.NoError(t, err)
	hubHex := store.HexOf(hubManifest.Digest)
	hubAttempt, err := nodeA.store.NewAttempt(hubHex)
	require.NoError(t, err)
	hubDest, err := hubAttempt.FilePath("weights.safetensors")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(hubDest, hubModelBody, 0o644))
	require.NoError(t, hubAttempt.Publish(store.Marker{
		Digest: hubManifest.Digest, ManifestFormat: modelartifact.ManifestHeader,
		SizeBytes: hubManifest.SizeBytes, FileCount: 1, PublishedTime: baseTime, Source: store.SourceHub,
	}))
	require.NoError(t, nodeA.store.WriteDigest(hubHex, store.DigestRecord{LastUsedTime: baseTime}))

	nmsA := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: "node-a"}, nmsA))
	nmsA.Spec.Pinned = []string{ingestResp.ManifestDigest}
	require.NoError(t, cli.Update(ctx, nmsA))

	nodeA.usedBytes.Store(6500) // 6500 + 2500 + kernelSize > 8000 (80% high watermark)
	nodeA.advanceTime(30 * time.Minute)
	require.NoError(t, nodeA.reporter.Report(ctx))
	assert.False(t, nodeA.store.IsPublished(hubHex), "unpinned Hub model past grace must be evicted above high watermark")
	assert.True(t, nodeA.store.IsPublished(kernelHex), "pinned Forge kernel tree must survive watermark GC")

	// Step 3: Create resolved worker-owned ModelKernelArtifact and mount on cold node-b via CSI
	// NodePublishVolume (fromPeersOnly over TLS Peer Sync, zero Hub hits).
	mka := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: tenantNS, Name: "qwen-decode-gen-1", UID: "uid-mka-1"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest:         ingestResp.ManifestDigest,
					DispatchManifestDigest: ingestResp.DispatchManifestDigest,
					SizeBytes:              ingestResp.SizeBytes,
					FileCount:              ingestResp.FileCount,
					SeedNode:               "node-a",
					ForgerRef:              workercore.ModelKernelArtifactForgerReference{Name: forgerName, UID: forgerUID},
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        "vLLM",
				EngineVersion: "0.8",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{kernelKey},
			},
		},
		Status: workercore.ModelKernelArtifactStatus{
			Resolved: &workercore.ModelKernelArtifactResolved{
				ManifestDigest:         ingestResp.ManifestDigest,
				DispatchManifestDigest: ingestResp.DispatchManifestDigest,
				DigestSource:           workercore.ModelKernelArtifactDigestSourceForge,
			},
			Conditions: []gpustack.Condition{{Type: "Resolved", Status: meta.ConditionTrue}},
		},
	}
	require.NoError(t, cli.Create(ctx, mka))

	targetB := filepath.Join(nodeB.kubeletDir, "pods", "pod-b-1", "volumes", "kubernetes.io~csi", "gpustack-kernels", "mount")
	csiReqB := &csi.NodePublishVolumeRequest{
		VolumeId:         "csi-kernel-b-1",
		TargetPath:       targetB,
		Readonly:         true,
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}},
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/ephemeral":      "true",
			"csi.storage.k8s.io/pod.namespace":  tenantNS,
			"csi.storage.k8s.io/pod.name":       "qwen-decode-0",
			"csi.storage.k8s.io/pod.uid":        "pod-b-1",
			modelstore.VolumeAttrArtifactKind:   modelstore.ArtifactKindKernel,
			modelstore.VolumeAttrArtifact:       mka.Name,
			modelstore.VolumeAttrArtifactUID:    string(mka.UID),
			modelstore.VolumeAttrManifestDigest: ingestResp.ManifestDigest,
		},
	}

	// First NodePublishVolume starts peer materialization on cold node-b; retry mounts once published.
	_, err = nodeB.driver.NodePublishVolume(ctx, csiReqB)
	if err != nil {
		assert.Equal(t, codes.Aborted, status.Code(err))
		require.Eventually(t, func() bool {
			return nodeB.store.IsPublished(kernelHex) && !nodeB.materializer.Downloading()[kernelHex]
		}, 10*time.Second, 20*time.Millisecond)
		_, err = nodeB.driver.NodePublishVolume(ctx, csiReqB)
	}
	require.NoError(t, err)
	assert.Zero(t, hubHits.Load(), "cold peer pull must never contact external Hub")
	assert.Equal(t, nodeB.store.PublishedTree(kernelHex), nodeB.mounter.mounted[targetB])

	// Verify Kind: "Kernel" survived Peer Sync onto node-b, along with 0444/0555 modes.
	markerB, err := nodeB.store.ReadMarker(kernelHex)
	require.NoError(t, err)
	assert.Equal(t, store.MarkerKindKernel, markerB.Kind)
	assert.Equal(t, store.SourcePeer, markerB.Source)

	treeDirInfo, err := os.Stat(nodeB.store.PublishedTree(kernelHex))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o555), treeDirInfo.Mode().Perm())
	fileInfo, err := os.Stat(filepath.Join(nodeB.store.PublishedTree(kernelHex), "dispatch-manifest.json"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o444), fileInfo.Mode().Perm())

	// Step 4: Verify a ModelArtifact (expectedDigest) on node-b is strictly blocked from mounting
	// the replicated Kind: "Kernel" tree.
	hijackMA := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: tenantNS, Name: "hijack-model", UID: "uid-hijack"},
		Spec: workercore.ModelArtifactSpec{
			Source: workercore.ModelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{Repository: "owner/repo", Revision: "main"},
			},
		},
		Status: workercore.ModelArtifactStatus{
			Resolved: &workercore.ModelArtifactResolved{
				ManifestDigest: ingestResp.ManifestDigest,
				DigestSource:   workercore.ModelArtifactDigestSourceExpected,
			},
			Conditions: []gpustack.Condition{{Type: "Resolved", Status: meta.ConditionTrue}},
		},
	}
	require.NoError(t, cli.Create(ctx, hijackMA))
	hijackTarget := filepath.Join(nodeB.kubeletDir, "pods", "pod-hijack", "volumes", "kubernetes.io~csi", "gpustack-model", "mount")
	_, err = nodeB.driver.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:         "csi-hijack-1",
		TargetPath:       hijackTarget,
		Readonly:         true,
		VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}},
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/ephemeral":      "true",
			"csi.storage.k8s.io/pod.namespace":  tenantNS,
			"csi.storage.k8s.io/pod.name":       "hijack-pod",
			"csi.storage.k8s.io/pod.uid":        "pod-hijack",
			modelstore.VolumeAttrArtifact:       hijackMA.Name,
			modelstore.VolumeAttrArtifactUID:    string(hijackMA.UID),
			modelstore.VolumeAttrManifestDigest: ingestResp.ManifestDigest,
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))

	// Step 5: Unpin on node-a, advance past the 10-minute grace window under high watermark, and
	// verify GC reclaims the unreferenced forged kernel tree.
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: "node-a"}, nmsA))
	nmsA.Spec.Pinned = nil
	require.NoError(t, cli.Update(ctx, nmsA))
	nodeA.usedBytes.Store(8500)
	nodeA.advanceTime(15 * time.Minute)
	require.NoError(t, nodeA.reporter.Report(ctx))
	assert.False(t, nodeA.store.IsPublished(kernelHex), "unpinned unreferenced Forge kernel tree must be reclaimed after grace")
}
