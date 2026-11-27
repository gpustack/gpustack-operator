package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	testKernelCommit         = "0123456789abcdef0123456789abcdef01234567"
	testKernelManifestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testKernelDispatchDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testKernelImageRef       = "registry.example.com/team/kernels@sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

type testKernelArtifactHub struct {
	server          *httptest.Server
	revisionStatus  atomic.Int32
	revisionCommit  atomic.Value
	includeDispatch atomic.Bool
	whoamiStatus    atomic.Int32
	requests        atomic.Int64
	auth            atomic.Value
}

func newTestKernelArtifactHub(t *testing.T) *testKernelArtifactHub {
	t.Helper()
	h := &testKernelArtifactHub{}
	h.revisionStatus.Store(http.StatusOK)
	h.revisionCommit.Store(testKernelCommit)
	h.includeDispatch.Store(true)
	h.whoamiStatus.Store(http.StatusOK)
	h.auth.Store("")
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.auth.Store(r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/models/owner/kernels/revision/" + testKernelCommit:
			if status := int(h.revisionStatus.Load()); status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": h.revisionCommit.Load().(string)})
		case "/api/models/owner/kernels/tree/" + h.revisionCommit.Load().(string):
			entries := []map[string]any{
				{
					"type": "file",
					"path": "kernels/rmsnorm/kernel.py",
					"size": 120,
					"oid":  strings.Repeat("b", 40),
					"lfs":  map[string]any{"oid": strings.Repeat("c", 64), "size": 120},
				},
			}
			if h.includeDispatch.Load() {
				entries = append(entries, map[string]any{
					"type": "file",
					"path": "dispatch-manifest.json",
					"size": 80,
					"oid":  strings.Repeat("a", 40),
					"lfs":  map[string]any{"oid": strings.TrimPrefix(testKernelDispatchDigest, "sha256:"), "size": 80},
				})
			}
			_ = json.NewEncoder(w).Encode(entries)
		case "/api/whoami-v2":
			w.WriteHeader(int(h.whoamiStatus.Load()))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

type testKernelArtifactEnv struct {
	r        *ModelKernelArtifactReconciler
	cli      ctrlcli.Client
	hub      *testKernelArtifactHub
	clock    *testArtifactClock
	recorder *ctrlrecord.FakeRecorder
	version  kubediscovery.Version
}

func newTestKernelArtifactEnv(t *testing.T, objs ...ctrlcli.Object) *testKernelArtifactEnv {
	t.Helper()
	hub := newTestKernelArtifactHub(t)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelKernelArtifact{}).
		WithObjects(objs...).Build()
	clock := &testArtifactClock{now: time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)}
	recorder := ctrlrecord.NewFakeRecorder(32)
	env := &testKernelArtifactEnv{
		cli:      cli,
		hub:      hub,
		clock:    clock,
		recorder: recorder,
		version:  kubediscovery.Version{GitVersion: "v1.35.5"},
	}
	env.r = &ModelKernelArtifactReconciler{
		Client:   cli,
		Recorder: recorder,
		Now:      clock.Now,
		NewHuggingFace: func(context.Context) (modelArtifactHub, error) {
			return &modelartifact.HuggingFace{Endpoint: hub.server.URL, Client: hub.server.Client()}, nil
		},
		ClusterVersion: func() kubediscovery.Version { return env.version },
	}
	return env
}

func (e *testKernelArtifactEnv) reconcile(t *testing.T, name string) (*workercore.ModelKernelArtifact, ctrl.Result) {
	t.Helper()
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: name}
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	for i := 0; res.Requeue && i < 3; i++ {
		res, err = e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	mka := new(workercore.ModelKernelArtifact)
	require.NoError(t, e.cli.Get(context.Background(), key, mka))
	return mka, res
}

func testNodeStoreKernelArtifact(name string) *workercore.ModelKernelArtifact {
	return &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "team-a",
			Name:       name,
			UID:        types.UID("uid-" + name),
			Generation: 1,
		},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest:         testKernelManifestDigest,
					DispatchManifestDigest: testKernelDispatchDigest,
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "node-1",
					ForgerRef: workercore.ModelKernelArtifactForgerReference{
						Name: "qwen-forger",
						UID:  "uid-forger",
					},
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        "vLLM",
				EngineVersion: "0.8.0",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{"vllm/0.8.0/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
			},
		},
	}
}

func testHFKernelArtifact(name, revision, secret string) *workercore.ModelKernelArtifact {
	mka := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "team-a",
			Name:       name,
			UID:        types.UID("uid-" + name),
			Generation: 1,
		},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{
					Repository: "owner/kernels",
					Revision:   revision,
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        "vLLM",
				EngineVersion: "0.8.0",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{"vllm/0.8.0/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
			},
		},
	}
	if secret != "" {
		mka.Spec.Source.HuggingFace.SecretRef = &core.LocalObjectReference{Name: secret}
	}
	return mka
}

func testImageKernelArtifact(name, ref string) *workercore.ModelKernelArtifact {
	return &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "team-a",
			Name:       name,
			UID:        types.UID("uid-" + name),
			Generation: 1,
		},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				Image: &workercore.ModelArtifactImageSource{Reference: ref},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        "vLLM",
				EngineVersion: "0.8.0",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{"vllm/0.8.0/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
			},
		},
	}
}

func TestModelKernelArtifactReconcileNodeStoreLifecycleAndSourceLost(t *testing.T) {
	nms1 := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
	}
	env := newTestKernelArtifactEnv(t, testNodeStoreKernelArtifact("gen-1"), nms1)

	// 1. Before seedNode reports Ready: Resolving, protection finalizer added, Unpinned.
	mka, _ := env.reconcile(t, "gen-1")
	assert.Contains(t, mka.Finalizers, workercore.ModelKernelArtifactProtectionFinalizer)
	assert.Equal(t, "Unknown", ModelKernelArtifactConditionResolved.GetStatus(mka))
	assert.Equal(t, workercore.ModelKernelArtifactReasonResolving, ModelKernelArtifactConditionResolved.GetReason(mka))
	assert.Equal(t, "False", ModelKernelArtifactConditionDegraded.GetStatus(mka))
	assert.Equal(t, "False", ModelKernelArtifactConditionPinned.GetStatus(mka))
	assert.Equal(t, workercore.ModelKernelArtifactReasonUnpinned, ModelKernelArtifactConditionPinned.GetReason(mka))
	assert.Nil(t, mka.Status.Resolved)
	require.NotNil(t, mka.Status.Nodes)
	assert.Equal(t, int32(0), mka.Status.Nodes.Ready)

	// 2. Seed node publishes Ready and pin lands; peer node starts downloading; third node fails.
	nms1.Spec.Pinned = []string{testKernelManifestDigest}
	nms1.Status.Models = []workercore.NodeModelStoreModel{{
		Digest:    testKernelManifestDigest,
		State:     workercore.NodeModelStoreModelStateReady,
		Source:    workercore.NodeModelStoreModelSourceForge,
		SizeBytes: 4096,
	}}
	require.NoError(t, env.cli.Update(context.Background(), nms1))
	require.NoError(t, env.cli.Create(context.Background(), &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-2"},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{
				Digest:          testKernelManifestDigest,
				State:           workercore.NodeModelStoreModelStateDownloading,
				SizeBytes:       100,
				DownloadedBytes: 47,
			}},
		},
	}))
	require.NoError(t, env.cli.Create(context.Background(), &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-3"},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{
				Digest: testKernelManifestDigest,
				State:  workercore.NodeModelStoreModelStateFailed,
				Reason: "SourceUnavailable",
			}},
		},
	}))

	mka, _ = env.reconcile(t, "gen-1")
	assert.Equal(t, "True", ModelKernelArtifactConditionResolved.GetStatus(mka))
	assert.Equal(t, workercore.ModelKernelArtifactReasonResolved, ModelKernelArtifactConditionResolved.GetReason(mka))
	assert.Equal(t, "False", ModelKernelArtifactConditionDegraded.GetStatus(mka))
	assert.Equal(t, "True", ModelKernelArtifactConditionPinned.GetStatus(mka))
	assert.Equal(t, workercore.ModelKernelArtifactReasonPinned, ModelKernelArtifactConditionPinned.GetReason(mka))
	require.NotNil(t, mka.Status.Resolved)
	assert.Equal(t, testKernelManifestDigest, mka.Status.Resolved.ManifestDigest)
	assert.Equal(t, testKernelDispatchDigest, mka.Status.Resolved.DispatchManifestDigest)
	assert.Equal(t, workercore.ModelKernelArtifactDigestSourceForge, mka.Status.Resolved.DigestSource)
	assert.Equal(t, int64(3), mka.Status.Resolved.FileCount)
	assert.Equal(t, int64(4096), mka.Status.Resolved.SizeBytes)
	assert.Equal(t, &workercore.ModelArtifactNodes{
		Ready: 1, Downloading: 1, Failed: 1, DownloadingPercent: ptr.To[int32](45),
	}, mka.Status.Nodes)

	// 3. Seed node loses the Ready copy (nodes.ready drops to 0) -> SourceLost!
	nms1.Status.Models = nil
	require.NoError(t, env.cli.Update(context.Background(), nms1))

	mka, _ = env.reconcile(t, "gen-1")
	assert.Equal(t, "False", ModelKernelArtifactConditionResolved.GetStatus(mka))
	assert.Equal(t, workercore.ModelKernelArtifactReasonSourceLost, ModelKernelArtifactConditionResolved.GetReason(mka))
	assert.Equal(t, "True", ModelKernelArtifactConditionDegraded.GetStatus(mka))
	assert.Equal(t, workercore.ModelKernelArtifactReasonSourceLost, ModelKernelArtifactConditionDegraded.GetReason(mka))
	require.NotNil(t, mka.Status.Resolved, "previous resolved metadata is preserved for audit on SourceLost")
	require.NotNil(t, mka.Status.Nodes)
	assert.Equal(t, int32(0), mka.Status.Nodes.Ready)
	require.NotEmpty(t, env.recorder.Events)
	assert.Contains(t, <-env.recorder.Events, workercore.ModelKernelArtifactReasonSourceLost)

	// 4. Recovery when a node reports Ready again.
	nms1.Status.Models = []workercore.NodeModelStoreModel{{
		Digest: testKernelManifestDigest,
		State:  workercore.NodeModelStoreModelStateReady,
	}}
	require.NoError(t, env.cli.Update(context.Background(), nms1))
	mka, _ = env.reconcile(t, "gen-1")
	assert.Equal(t, "True", ModelKernelArtifactConditionResolved.GetStatus(mka))
	assert.Equal(t, "False", ModelKernelArtifactConditionDegraded.GetStatus(mka))
	assert.Equal(t, int32(1), mka.Status.Nodes.Ready)
}

func TestModelKernelArtifactReconcileInvalidSources(t *testing.T) {
	cases := []struct {
		name       string
		mka        *workercore.ModelKernelArtifact
		wantReason string
	}{
		{
			name: "empty source union",
			mka: &workercore.ModelKernelArtifact{
				ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "bad", UID: "uid-bad", Generation: 1},
			},
			wantReason: workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
		{
			name: "invalid nodeStore manifestDigest",
			mka: func() *workercore.ModelKernelArtifact {
				m := testNodeStoreKernelArtifact("bad")
				m.Spec.Source.NodeStore.ManifestDigest = "not-a-digest"
				return m
			}(),
			wantReason: workercore.ModelKernelArtifactReasonDigestMismatch,
		},
		{
			name: "invalid nodeStore dispatchManifestDigest",
			mka: func() *workercore.ModelKernelArtifact {
				m := testNodeStoreKernelArtifact("bad")
				m.Spec.Source.NodeStore.DispatchManifestDigest = "sha256:short"
				return m
			}(),
			wantReason: workercore.ModelKernelArtifactReasonDigestMismatch,
		},
		{
			name: "missing nodeStore seedNode",
			mka: func() *workercore.ModelKernelArtifact {
				m := testNodeStoreKernelArtifact("bad")
				m.Spec.Source.NodeStore.SeedNode = ""
				return m
			}(),
			wantReason: workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
		{
			name: "missing nodeStore forgerRef",
			mka: func() *workercore.ModelKernelArtifact {
				m := testNodeStoreKernelArtifact("bad")
				m.Spec.Source.NodeStore.ForgerRef.UID = ""
				return m
			}(),
			wantReason: workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
		{
			name: "zero nodeStore sizeBytes",
			mka: func() *workercore.ModelKernelArtifact {
				m := testNodeStoreKernelArtifact("bad")
				m.Spec.Source.NodeStore.SizeBytes = 0
				return m
			}(),
			wantReason: workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
		{
			name: "nodeStore fileCount below 2",
			mka: func() *workercore.ModelKernelArtifact {
				m := testNodeStoreKernelArtifact("bad")
				m.Spec.Source.NodeStore.FileCount = 1
				return m
			}(),
			wantReason: workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
		{
			name:       "huggingFace non-40-hex revision",
			mka:        testHFKernelArtifact("bad", "main", ""),
			wantReason: workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestKernelArtifactEnv(t, tc.mka)
			mka, _ := env.reconcile(t, "bad")
			assert.Equal(t, "False", ModelKernelArtifactConditionResolved.GetStatus(mka))
			assert.Equal(t, tc.wantReason, ModelKernelArtifactConditionResolved.GetReason(mka))
			assert.Equal(t, "True", ModelKernelArtifactConditionDegraded.GetStatus(mka))
			assert.Zero(t, env.hub.requests.Load())
		})
	}
}

func TestModelKernelArtifactReconcilePinnedCapacityExceeded(t *testing.T) {
	fullPins := make([]string, 0, nodeModelStoreMaxPinned)
	for i := 0; i < nodeModelStoreMaxPinned; i++ {
		fullPins = append(fullPins, numberedDigest("ffffffff", i))
	}

	t.Run("seed node full pin list surfaces PinnedCapacityExceeded", func(t *testing.T) {
		nms := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec:       workercore.NodeModelStoreSpec{Pinned: fullPins},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: testKernelManifestDigest,
					State:  workercore.NodeModelStoreModelStateReady,
				}},
			},
		}
		env := newTestKernelArtifactEnv(t, testNodeStoreKernelArtifact("gen-1"), nms)
		mka, _ := env.reconcile(t, "gen-1")

		assert.Equal(t, "True", ModelKernelArtifactConditionResolved.GetStatus(mka))
		assert.Equal(t, "False", ModelKernelArtifactConditionPinned.GetStatus(mka))
		assert.Equal(t, workercore.ModelKernelArtifactReasonPinnedCapacityExceeded,
			ModelKernelArtifactConditionPinned.GetReason(mka))
		require.NotEmpty(t, env.recorder.Events)
		assert.Contains(t, <-env.recorder.Events, workercore.ModelKernelArtifactReasonPinnedCapacityExceeded)
	})

	t.Run("consumer deployment pod node full pin list surfaces PinnedCapacityExceeded", func(t *testing.T) {
		nms1 := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec:       workercore.NodeModelStoreSpec{Pinned: []string{testKernelManifestDigest}},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: testKernelManifestDigest,
					State:  workercore.NodeModelStoreModelStateReady,
				}},
			},
		}
		nms2 := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-2"},
			Spec:       workercore.NodeModelStoreSpec{Pinned: fullPins},
		}
		md := &workercore.ModelDeployment{
			ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
			Spec: workercore.ModelDeploymentSpec{
				Roles: []workercore.ModelDeploymentRole{
					{Name: "decode", KernelArtifactRef: &core.LocalObjectReference{Name: "gen-1"}},
				},
			},
		}
		pod := testModelDeploymentPod("team-a", "qwen", "decode", "qwen-decode-0", "node-2", core.PodRunning)

		env := newTestKernelArtifactEnv(t, testNodeStoreKernelArtifact("gen-1"), nms1, nms2, md, pod)
		mka, _ := env.reconcile(t, "gen-1")

		assert.Equal(t, "False", ModelKernelArtifactConditionPinned.GetStatus(mka))
		assert.Equal(t, workercore.ModelKernelArtifactReasonPinnedCapacityExceeded,
			ModelKernelArtifactConditionPinned.GetReason(mka))
	})
}

func TestModelKernelArtifactReconcileHuggingFace(t *testing.T) {
	cases := []struct {
		name            string
		objs            []ctrlcli.Object
		revisionStatus  int
		revisionCommit  string
		includeDispatch bool
		wantResolved    string
		wantReason      string
		wantDegraded    bool
	}{
		{
			name:            "public 40-hex commit with dispatch-manifest.json resolves",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "")},
			revisionStatus:  http.StatusOK,
			revisionCommit:  testKernelCommit,
			includeDispatch: true,
			wantResolved:    "True",
			wantReason:      workercore.ModelKernelArtifactReasonResolved,
		},
		{
			name:            "private repo with Secret token resolves without leaking token",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "hf-token"), testTokenSecret(testArtifactToken)},
			revisionStatus:  http.StatusOK,
			revisionCommit:  testKernelCommit,
			includeDispatch: true,
			wantResolved:    "True",
			wantReason:      workercore.ModelKernelArtifactReasonResolved,
		},
		{
			name:            "missing Secret sets SecretNotFound",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "hf-token")},
			revisionStatus:  http.StatusOK,
			revisionCommit:  testKernelCommit,
			includeDispatch: true,
			wantResolved:    "False",
			wantReason:      workercore.ModelKernelArtifactReasonSecretNotFound,
			wantDegraded:    true,
		},
		{
			name:            "Secret without token key sets SecretNotFound",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "hf-token"), testTokenSecret("")},
			revisionStatus:  http.StatusOK,
			revisionCommit:  testKernelCommit,
			includeDispatch: true,
			wantResolved:    "False",
			wantReason:      workercore.ModelKernelArtifactReasonSecretNotFound,
			wantDegraded:    true,
		},
		{
			name:            "hub outage sets SourceUnavailable",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "")},
			revisionStatus:  http.StatusBadGateway,
			revisionCommit:  testKernelCommit,
			includeDispatch: true,
			wantResolved:    "False",
			wantReason:      modelartifact.ReasonSourceUnavailable,
			wantDegraded:    true,
		},
		{
			name:            "missing dispatch-manifest.json fails with InvalidManifest",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "")},
			revisionStatus:  http.StatusOK,
			revisionCommit:  testKernelCommit,
			includeDispatch: false,
			wantResolved:    "False",
			wantReason:      modelartifact.ReasonInvalidManifest,
			wantDegraded:    true,
		},
		{
			name:            "commit SHA mismatch fails with DigestMismatch",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "")},
			revisionStatus:  http.StatusOK,
			revisionCommit:  strings.Repeat("f", 40),
			includeDispatch: true,
			wantResolved:    "False",
			wantReason:      workercore.ModelKernelArtifactReasonDigestMismatch,
			wantDegraded:    true,
		},
		{
			name:            "hub unauthorized sets AccessDenied",
			objs:            []ctrlcli.Object{testHFKernelArtifact("hf-k", testKernelCommit, "")},
			revisionStatus:  http.StatusUnauthorized,
			revisionCommit:  testKernelCommit,
			includeDispatch: true,
			wantResolved:    "False",
			wantReason:      modelartifact.ReasonAccessDenied,
			wantDegraded:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestKernelArtifactEnv(t, tc.objs...)
			env.hub.revisionStatus.Store(int32(tc.revisionStatus))
			env.hub.revisionCommit.Store(tc.revisionCommit)
			env.hub.includeDispatch.Store(tc.includeDispatch)

			mka, res := env.reconcile(t, "hf-k")
			assert.Contains(t, mka.Finalizers, workercore.ModelKernelArtifactProtectionFinalizer)
			assert.Equal(t, tc.wantResolved, ModelKernelArtifactConditionResolved.GetStatus(mka))
			assert.Equal(t, tc.wantReason, ModelKernelArtifactConditionResolved.GetReason(mka))
			assert.Equal(t, tc.wantDegraded, ModelKernelArtifactConditionDegraded.IsTrue(mka))
			assert.Equal(t, "False", ModelKernelArtifactConditionPinned.GetStatus(mka))
			assert.Equal(t, workercore.ModelKernelArtifactReasonUnpinned, ModelKernelArtifactConditionPinned.GetReason(mka))
			assert.Positive(t, res.RequeueAfter)

			raw, err := json.Marshal(mka.Status)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), testArtifactToken)

			if tc.wantResolved != "True" {
				assert.Nil(t, mka.Status.Resolved)
				return
			}
			require.NotNil(t, mka.Status.Resolved)
			assert.Equal(t, testKernelCommit, mka.Status.Resolved.Revision)
			assert.Equal(t, workercore.ModelKernelArtifactDigestSourceHub, mka.Status.Resolved.DigestSource)
			assert.Equal(t, testKernelDispatchDigest, mka.Status.Resolved.DispatchManifestDigest)
			assert.True(t, strings.HasPrefix(mka.Status.Resolved.ManifestDigest, "sha256:"))
			assert.Equal(t, int64(2), mka.Status.Resolved.FileCount)
			assert.Equal(t, int64(200), mka.Status.Resolved.SizeBytes)
		})
	}

	t.Run("revalidation detects manifest digest drift after confirmation delay", func(t *testing.T) {
		good, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{
			{Path: "dispatch-manifest.json", Size: 80, Digest: testKernelDispatchDigest},
			{Path: "kernels/op.py", Size: 120, Digest: "sha256:" + strings.Repeat("c", 64)},
		})
		require.NoError(t, err)
		drifted, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{
			{Path: "dispatch-manifest.json", Size: 80, Digest: testKernelDispatchDigest},
			{Path: "kernels/op.py", Size: 120, Digest: "sha256:" + strings.Repeat("e", 64)},
		})
		require.NoError(t, err)

		fakeHub := &testModelScopeHub{
			resolution: modelartifact.Resolution{Commit: testKernelCommit, Manifest: good},
			list:       []modelartifact.Manifest{drifted, drifted},
		}
		env := newTestKernelArtifactEnv(t, testHFKernelArtifact("hf-k", testKernelCommit, ""))
		env.r.NewHuggingFace = func(context.Context) (modelArtifactHub, error) { return fakeHub, nil }

		mka, _ := env.reconcile(t, "hf-k")
		require.True(t, ModelKernelArtifactConditionResolved.IsTrue(mka))

		// Advance past revalidate interval: first mismatch degrades only.
		env.clock.now = env.clock.now.Add(25 * time.Hour)
		first, _ := env.reconcile(t, "hf-k")
		assert.True(t, ModelKernelArtifactConditionResolved.IsTrue(first))
		assert.Equal(t, workercore.ModelKernelArtifactReasonDigestMismatch, ModelKernelArtifactConditionDegraded.GetReason(first))

		// Advance past confirm delay: confirmed mismatch revokes.
		env.clock.now = env.clock.now.Add(modelArtifactConfirmDelay)
		revoked, _ := env.reconcile(t, "hf-k")
		assert.False(t, ModelKernelArtifactConditionResolved.IsTrue(revoked))
		assert.Equal(t, workercore.ModelKernelArtifactReasonDigestMismatch, ModelKernelArtifactConditionResolved.GetReason(revoked))
	})

	t.Run("warns on rejected token on public repository", func(t *testing.T) {
		env := newTestKernelArtifactEnv(t, testHFKernelArtifact("hf-k", testKernelCommit, "hf-token"), testTokenSecret("bad-tok"))
		env.hub.whoamiStatus.Store(http.StatusUnauthorized)
		mka, _ := env.reconcile(t, "hf-k")
		assert.True(t, ModelKernelArtifactConditionResolved.IsTrue(mka))
		require.NotEmpty(t, env.recorder.Events)
		ev := <-env.recorder.Events
		assert.Contains(t, ev, "InvalidToken")
		assert.NotContains(t, ev, "bad-tok")
	})
}

func TestModelKernelArtifactReconcileImage(t *testing.T) {
	cases := []struct {
		name         string
		version      string
		ref          string
		wantResolved string
		wantReason   string
	}{
		{
			name:         "supported k8s >= 1.35 resolves with Resolved nil",
			version:      "v1.35.5",
			ref:          testKernelImageRef,
			wantResolved: "True",
			wantReason:   workercore.ModelKernelArtifactReasonResolved,
		},
		{
			name:         "unsupported k8s < 1.35 sets UnsupportedSource",
			version:      "v1.34.2",
			ref:          testKernelImageRef,
			wantResolved: "False",
			wantReason:   workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
		{
			name:         "unpinned image tag sets UnsupportedSource",
			version:      "v1.35.5",
			ref:          "registry.example.com/team/kernels:latest",
			wantResolved: "False",
			wantReason:   workercore.ModelKernelArtifactReasonUnsupportedSource,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestKernelArtifactEnv(t, testImageKernelArtifact("img-k", tc.ref))
			env.version = kubediscovery.Version{GitVersion: tc.version}

			mka, res := env.reconcile(t, "img-k")
			assert.Equal(t, tc.wantResolved, ModelKernelArtifactConditionResolved.GetStatus(mka))
			assert.Equal(t, tc.wantReason, ModelKernelArtifactConditionResolved.GetReason(mka))
			assert.Nil(t, mka.Status.Resolved, "image kernel artifacts keep status.resolved nil")
			assert.Nil(t, mka.Status.Nodes, "image kernel artifacts never aggregate status.nodes")
			assert.Equal(t, "False", ModelKernelArtifactConditionPinned.GetStatus(mka))
			assert.Equal(t, workercore.ModelKernelArtifactReasonUnpinned, ModelKernelArtifactConditionPinned.GetReason(mka))
			assert.False(t, res.Requeue)
			assert.Zero(t, res.RequeueAfter)
		})
	}
}

func TestModelKernelArtifactReconcileProtectionFinalizer(t *testing.T) {
	deleting := func() *workercore.ModelKernelArtifact {
		mka := testNodeStoreKernelArtifact("gen-1")
		now := meta.Now()
		mka.DeletionTimestamp = &now
		mka.Finalizers = []string{workercore.ModelKernelArtifactProtectionFinalizer}
		return mka
	}

	refMD := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelDeploymentSpec{
			Roles: []workercore.ModelDeploymentRole{
				{Name: "decode", KernelArtifactRef: &core.LocalObjectReference{Name: "gen-1"}},
			},
		},
	}
	otherNSRefMD := refMD.DeepCopy()
	otherNSRefMD.Namespace = "team-b"

	refForgerPrev := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "forger-prev"},
		Status: workercore.ModelDeploymentForgerStatus{
			Roles: []workercore.ModelDeploymentForgerRoleStatus{
				{Name: "decode", PreviousKernelArtifact: "gen-1"},
			},
		},
	}
	refForgerActive := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "forger-active"},
		Status: workercore.ModelDeploymentForgerStatus{
			Roles: []workercore.ModelDeploymentForgerRoleStatus{
				{Name: "decode", ActiveKernelArtifact: "gen-1"},
			},
		},
	}
	otherNSRefForger := refForgerPrev.DeepCopy()
	otherNSRefForger.Namespace = "team-b"

	cases := []struct {
		name     string
		objs     []ctrlcli.Object
		wantGone bool
	}{
		{name: "held by referencing ModelDeployment", objs: []ctrlcli.Object{deleting(), refMD}},
		{name: "held by ModelDeploymentForger previousKernelArtifact", objs: []ctrlcli.Object{deleting(), refForgerPrev}},
		{name: "held by ModelDeploymentForger activeKernelArtifact", objs: []ctrlcli.Object{deleting(), refForgerActive}},
		{name: "not held by references in another namespace", objs: []ctrlcli.Object{deleting(), otherNSRefMD, otherNSRefForger}, wantGone: true},
		{name: "released when unreferenced", objs: []ctrlcli.Object{deleting()}, wantGone: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestKernelArtifactEnv(t, tc.objs...)
			env.r.checks.Store(types.UID("uid-gen-1"), modelArtifactCheck{})
			env.r.nodesWritten.Store(types.UID("uid-gen-1"), env.clock.Now())

			key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "gen-1"}
			_, err := env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)

			err = env.cli.Get(context.Background(), key, new(workercore.ModelKernelArtifact))
			assert.Equal(t, tc.wantGone, err != nil, "artifact deleted")
			_, checkKept := env.r.checks.Load(types.UID("uid-gen-1"))
			assert.Equal(t, !tc.wantGone, checkKept)
			_, nodesKept := env.r.nodesWritten.Load(types.UID("uid-gen-1"))
			assert.Equal(t, !tc.wantGone, nodesKept)
		})
	}
}

func TestModelKernelArtifactReconcilePreservesQuarantine(t *testing.T) {
	mka := testNodeStoreKernelArtifact("gen-1")
	mka.Status.Quarantined = true
	mka.Status.QuarantineReason = "CanaryTimeout"
	mka.Status.QuarantineMessage = "canary failed readiness within 600s"

	nms := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
		Spec:       workercore.NodeModelStoreSpec{Pinned: []string{testKernelManifestDigest}},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{
				Digest: testKernelManifestDigest,
				State:  workercore.NodeModelStoreModelStateReady,
			}},
		},
	}

	env := newTestKernelArtifactEnv(t, mka, nms)
	got, _ := env.reconcile(t, "gen-1")
	assert.True(t, got.Status.Quarantined, "normal reconciliation must never clear quarantine")
	assert.Equal(t, "CanaryTimeout", got.Status.QuarantineReason)
	assert.Equal(t, "canary failed readiness within 600s", got.Status.QuarantineMessage)

	// Transition to SourceLost must also preserve quarantine.
	nms.Status.Models = nil
	require.NoError(t, env.cli.Update(context.Background(), nms))
	got, _ = env.reconcile(t, "gen-1")
	assert.Equal(t, workercore.ModelKernelArtifactReasonSourceLost, ModelKernelArtifactConditionResolved.GetReason(got))
	assert.True(t, got.Status.Quarantined)
	assert.Equal(t, "CanaryTimeout", got.Status.QuarantineReason)
	assert.Equal(t, "canary failed readiness within 600s", got.Status.QuarantineMessage)
}

func TestModelKernelArtifactWatchMappers(t *testing.T) {
	nodeMKA := testNodeStoreKernelArtifact("gen-1")
	hfMKA := testHFKernelArtifact("hf-k", testKernelCommit, "hf-token")
	hfMKA.Status.Resolved = &workercore.ModelKernelArtifactResolved{ManifestDigest: testKernelManifestDigest}
	imgMKA := testImageKernelArtifact("img-k", testKernelImageRef)
	termMKA := testImageKernelArtifact("term-k", testKernelImageRef)
	now := meta.Now()
	termMKA.DeletionTimestamp = &now
	termMKA.Finalizers = []string{workercore.ModelKernelArtifactProtectionFinalizer}

	env := newTestKernelArtifactEnv(t, nodeMKA, hfMKA, imgMKA, termMKA)

	// Secret mapper only enqueues referencing huggingFace artifact.
	secReqs := env.r.enqueueKernelArtifactsForSecret(context.Background(), testTokenSecret("tok"))
	assert.Equal(t, []ctrl.Request{{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "hf-k"}}}, secReqs)

	// NodeModelStore mapper enqueues nodeStore and resolved huggingFace, skips image.
	nms := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}}
	nmsReqs := env.r.mapModelKernelArtifactNodeModelStore(context.Background(), nms)
	assert.ElementsMatch(t, []ctrl.Request{
		{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "gen-1"}},
		{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "hf-k"}},
	}, nmsReqs)

	// Deployment mapper enqueues nodeStore and terminating artifacts in the deployment's namespace.
	md := &workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"}}
	mdReqs := env.r.enqueueKernelArtifactsForDeployment(context.Background(), md)
	assert.ElementsMatch(t, []ctrl.Request{
		{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "gen-1"}},
		{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "term-k"}},
	}, mdReqs)

	// Forger mapper enqueues nodeStore and terminating artifacts in the forger's namespace.
	mdf := &workercore.ModelDeploymentForger{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "forger"}}
	mdfReqs := env.r.enqueueKernelArtifactsForForger(context.Background(), mdf)
	assert.ElementsMatch(t, []ctrl.Request{
		{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "gen-1"}},
		{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "term-k"}},
	}, mdfReqs)

	// Forger predicate detects active or previous kernel artifact changes.
	mdfOld := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "forger"},
		Status:     workercore.ModelDeploymentForgerStatus{Roles: []workercore.ModelDeploymentForgerRoleStatus{{Name: "decode"}}},
	}
	mdfActive := mdfOld.DeepCopy()
	mdfActive.Status.Roles[0].ActiveKernelArtifact = "gen-1"
	assert.True(t, modelDeploymentForgerKernelRefsChanged.Create(ctrlevent.CreateEvent{Object: mdfActive}))
	assert.True(t, modelDeploymentForgerKernelRefsChanged.Delete(ctrlevent.DeleteEvent{Object: mdfActive}))
	assert.True(t, modelDeploymentForgerKernelRefsChanged.Update(ctrlevent.UpdateEvent{ObjectOld: mdfOld, ObjectNew: mdfActive}))
	assert.False(t, modelDeploymentForgerKernelRefsChanged.Update(ctrlevent.UpdateEvent{ObjectOld: mdfOld, ObjectNew: mdfOld.DeepCopy()}))
	assert.False(t, modelDeploymentForgerKernelRefsChanged.Generic(ctrlevent.GenericEvent{Object: mdfActive}))

	// Deployment pod mapper enqueues nodeStore artifacts in the pod's namespace.
	pod := testModelDeploymentPod("team-a", "qwen", "decode", "p-0", "node-1", core.PodRunning)
	podReqs := env.r.enqueueKernelArtifactsForDeploymentPod(context.Background(), pod)
	assert.Equal(t, []ctrl.Request{{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "gen-1"}}}, podReqs)

	// NodeModelStore predicate detects pinned and models changes.
	nmsOld := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}}
	nmsPinned := nmsOld.DeepCopy()
	nmsPinned.Spec.Pinned = []string{fmt.Sprintf("sha256:%s", strings.Repeat("9", 64))}
	assert.True(t, nodeKernelReportChanged.Create(ctrlevent.CreateEvent{Object: nmsPinned}))
	assert.True(t, nodeKernelReportChanged.Delete(ctrlevent.DeleteEvent{Object: nmsPinned}))
	assert.True(t, nodeKernelReportChanged.Update(ctrlevent.UpdateEvent{ObjectOld: nmsOld, ObjectNew: nmsPinned}))
	assert.False(t, nodeKernelReportChanged.Update(ctrlevent.UpdateEvent{ObjectOld: nmsOld, ObjectNew: nmsOld.DeepCopy()}))
	assert.False(t, nodeKernelReportChanged.Generic(ctrlevent.GenericEvent{Object: nmsPinned}))

	// Reconciling a non-existent object returns cleanly without error.
	res, err := env.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "missing"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	// Field indexer extracts manifest digest for nodeStore and resolved huggingFace, nil for image.
	assert.Equal(t, []string{testKernelManifestDigest}, indexModelKernelArtifactByManifestDigest(nodeMKA))
	assert.Equal(t, []string{testKernelManifestDigest}, indexModelKernelArtifactByManifestDigest(hfMKA))
	assert.Nil(t, indexModelKernelArtifactByManifestDigest(imgMKA))
	assert.Nil(t, indexModelKernelArtifactByManifestDigest(nmsOld))
}

func TestModelKernelArtifactReconcileNodesWindowAndHuggingFaceRevalidation(t *testing.T) {
	t.Run("nodes window paces steady-state progress updates", func(t *testing.T) {
		nms := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Spec:       workercore.NodeModelStoreSpec{Pinned: []string{testKernelManifestDigest}},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: testKernelManifestDigest,
					State:  workercore.NodeModelStoreModelStateReady,
				}},
			},
		}
		nms2 := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-2"},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest:          testKernelManifestDigest,
					State:           workercore.NodeModelStoreModelStateDownloading,
					SizeBytes:       100,
					DownloadedBytes: 10,
				}},
			},
		}
		env := newTestKernelArtifactEnv(t, testNodeStoreKernelArtifact("gen-1"), nms, nms2)
		mka, _ := env.reconcile(t, "gen-1")
		require.Equal(t, ptr.To[int32](10), mka.Status.Nodes.DownloadingPercent)

		// Advance inside the 30s window and update progress: held until window elapses.
		env.clock.now = env.clock.now.Add(10 * time.Second)
		nms2.Status.Models[0].DownloadedBytes = 35
		require.NoError(t, env.cli.Update(context.Background(), nms2))
		mka, res := env.reconcile(t, "gen-1")
		assert.Equal(t, ptr.To[int32](10), mka.Status.Nodes.DownloadingPercent)
		assert.Equal(t, 20*time.Second, res.RequeueAfter)

		// Advance past window: update lands.
		env.clock.now = env.clock.now.Add(20 * time.Second)
		mka, _ = env.reconcile(t, "gen-1")
		assert.Equal(t, ptr.To[int32](35), mka.Status.Nodes.DownloadingPercent)
	})

	t.Run("huggingFace revalidation stays healthy, ignores transient outage, and respects pacing", func(t *testing.T) {
		good, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{
			{Path: "dispatch-manifest.json", Size: 80, Digest: modelartifact.DigestGitSHA1 + ":" + strings.Repeat("a", 40)},
			{Path: "kernels/op.py", Size: 120, Digest: "sha256:" + strings.Repeat("c", 64)},
		})
		require.NoError(t, err)
		fakeHub := &testModelScopeHub{
			resolution: modelartifact.Resolution{Commit: testKernelCommit, Manifest: good},
			list:       []modelartifact.Manifest{good},
		}
		env := newTestKernelArtifactEnv(t, testHFKernelArtifact("hf-k", testKernelCommit, ""))
		env.r.NewHuggingFace = func(context.Context) (modelArtifactHub, error) { return fakeHub, nil }

		mka, _ := env.reconcile(t, "hf-k")
		require.True(t, ModelKernelArtifactConditionResolved.IsTrue(mka))
		assert.Empty(t, mka.Status.Resolved.DispatchManifestDigest, "non-sha256 gitsha1 entry leaves SHA-256 dispatch digest empty")

		// Early reconcile before interval does not re-query the hub.
		asksBefore := fakeHub.asks
		env.clock.now = env.clock.now.Add(time.Hour)
		_, res := env.reconcile(t, "hf-k")
		assert.Equal(t, asksBefore, fakeHub.asks)
		assert.Positive(t, res.RequeueAfter)

		// Past interval: revalidation confirms healthy access.
		env.clock.now = env.clock.now.Add(24 * time.Hour)
		mka, _ = env.reconcile(t, "hf-k")
		assert.True(t, ModelKernelArtifactConditionResolved.IsTrue(mka))
		assert.False(t, ModelKernelArtifactConditionDegraded.IsTrue(mka))
		assert.Greater(t, fakeHub.asks, asksBefore)

		// Revalidation with missing dispatch-manifest.json degrades first, then revokes.
		noDispatch, err := modelartifact.NewManifest([]modelartifact.ManifestEntry{
			{Path: "kernels/op.py", Size: 120, Digest: "sha256:" + strings.Repeat("c", 64)},
		})
		require.NoError(t, err)
		fakeHub.mu.Lock()
		fakeHub.list = []modelartifact.Manifest{noDispatch, noDispatch}
		fakeHub.mu.Unlock()
		env.clock.now = env.clock.now.Add(25 * time.Hour)
		mka, _ = env.reconcile(t, "hf-k")
		assert.True(t, ModelKernelArtifactConditionResolved.IsTrue(mka))
		assert.Equal(t, modelartifact.ReasonInvalidManifest, ModelKernelArtifactConditionDegraded.GetReason(mka))
		env.clock.now = env.clock.now.Add(modelArtifactConfirmDelay)
		mka, _ = env.reconcile(t, "hf-k")
		assert.False(t, ModelKernelArtifactConditionResolved.IsTrue(mka))
		assert.Equal(t, modelartifact.ReasonInvalidManifest, ModelKernelArtifactConditionResolved.GetReason(mka))

		// Default HuggingFace client builder builds cleanly from Settings.
		hub, err := env.r.newHuggingFaceFromSettings(context.Background())
		require.NoError(t, err)
		assert.NotNil(t, hub)
	})

	t.Run("nodeStore initial failure degrades while staying Resolving", func(t *testing.T) {
		nms := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1"},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: testKernelManifestDigest,
					State:  workercore.NodeModelStoreModelStateFailed,
					Reason: "IntegrityMismatch",
				}},
			},
		}
		env := newTestKernelArtifactEnv(t, testNodeStoreKernelArtifact("gen-1"), nms)
		mka, _ := env.reconcile(t, "gen-1")
		assert.Equal(t, "Unknown", ModelKernelArtifactConditionResolved.GetStatus(mka))
		assert.Equal(t, workercore.ModelKernelArtifactReasonResolving, ModelKernelArtifactConditionResolved.GetReason(mka))
		assert.Equal(t, "True", ModelKernelArtifactConditionDegraded.GetStatus(mka))
		assert.Equal(t, workercore.ModelKernelArtifactReasonSourceLost, ModelKernelArtifactConditionDegraded.GetReason(mka))
	})
}
