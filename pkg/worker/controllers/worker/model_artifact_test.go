package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelartifact"
)

const (
	testArtifactCommit = "0123456789abcdef0123456789abcdef01234567"
	testArtifactToken  = "artifact-token-never-in-status"
)

// testArtifactHub is a fake Hub for one repository, owner/repo. Each answer is swappable while a
// case runs, and every request is counted.
type testArtifactHub struct {
	server   *httptest.Server
	revision atomic.Int32 // HTTP status of the revision endpoint
	access   atomic.Int32 // HTTP status of the revalidation HEAD
	whoami   atomic.Int32 // HTTP status of whoami-v2
	requests atomic.Int64
	auth     atomic.Value
}

func newTestArtifactHub(t *testing.T) *testArtifactHub {
	t.Helper()
	h := &testArtifactHub{}
	h.revision.Store(http.StatusOK)
	h.access.Store(http.StatusOK)
	h.whoami.Store(http.StatusOK)
	h.auth.Store("")
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.auth.Store(r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/models/owner/repo/revision/main":
			if status := int(h.revision.Load()); status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": testArtifactCommit})
		case "/api/models/owner/repo/tree/" + testArtifactCommit:
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"type": "file", "path": "config.json", "size": 10, "oid": strings.Repeat("b", 40)},
				{
					"type": "file", "path": "model.safetensors", "size": 100, "oid": strings.Repeat("9", 40),
					"lfs": map[string]any{"oid": strings.Repeat("a", 64), "size": 100},
				},
			})
		case "/owner/repo/resolve/" + testArtifactCommit + "/config.json":
			w.WriteHeader(int(h.access.Load()))
		case "/api/whoami-v2":
			w.WriteHeader(int(h.whoami.Load()))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.server.Close)

	return h
}

// testArtifactClock is a settable clock.
type testArtifactClock struct{ now time.Time }

func (c *testArtifactClock) Now() time.Time { return c.now }

type testArtifactEnv struct {
	r        *ModelArtifactReconciler
	cli      ctrlcli.Client
	hub      *testArtifactHub
	clock    *testArtifactClock
	recorder *ctrlrecord.FakeRecorder
}

func newTestArtifactEnv(t *testing.T, objs ...ctrlcli.Object) *testArtifactEnv {
	t.Helper()
	hub := newTestArtifactHub(t)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelArtifact{}).
		WithIndex(&workercore.ModelArtifact{}, IndexingModelArtifactByManifestDigest, indexModelArtifactByManifestDigest).
		WithIndex(&workercore.NodeModelStore{}, IndexingNodeModelStoreByModelDigest, indexNodeModelStoreByModelDigest).
		WithObjects(objs...).Build()
	clock := &testArtifactClock{now: time.Date(2026, 9, 25, 6, 0, 0, 0, time.UTC)}
	recorder := ctrlrecord.NewFakeRecorder(16)
	r := &ModelArtifactReconciler{
		Client:   cli,
		Recorder: recorder,
		Now:      clock.Now,
		NewHuggingFace: func(context.Context) (*modelartifact.HuggingFace, error) {
			return &modelartifact.HuggingFace{Endpoint: hub.server.URL, Client: hub.server.Client()}, nil
		},
	}

	return &testArtifactEnv{r: r, cli: cli, hub: hub, clock: clock, recorder: recorder}
}

func (e *testArtifactEnv) reconcile(t *testing.T, name string) (*workercore.ModelArtifact, ctrl.Result) {
	t.Helper()
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: name}
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	// The first pass of a new artifact only writes Resolving and asks for another.
	for i := 0; res.Requeue && i < 3; i++ {
		res, err = e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	ma := new(workercore.ModelArtifact)
	require.NoError(t, e.cli.Get(context.Background(), key, ma))

	return ma, res
}

func testHubArtifact(secret string) *workercore.ModelArtifact {
	ma := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen", UID: "uid-qwen", Generation: 1},
		Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
			HuggingFace: &workercore.ModelArtifactHubSource{Repository: "owner/repo", Revision: "main"},
		}},
	}
	if secret != "" {
		ma.Spec.Source.HuggingFace.SecretRef = &core.LocalObjectReference{Name: secret}
	}

	return ma
}

func testTokenSecret(token string) *core.Secret {
	return &core.Secret{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "hf-token"},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

func TestModelArtifactReconcileResolvesHuggingFace(t *testing.T) {
	cases := []struct {
		name         string
		objs         []ctrlcli.Object
		revision     int
		wantResolved string
		wantReason   string
		wantDegraded bool
	}{
		{
			name:         "a public repository",
			objs:         []ctrlcli.Object{testHubArtifact("")},
			revision:     http.StatusOK,
			wantResolved: "True", wantReason: "Resolved",
		},
		{
			name:         "a private repository with its token",
			objs:         []ctrlcli.Object{testHubArtifact("hf-token"), testTokenSecret(testArtifactToken)},
			revision:     http.StatusOK,
			wantResolved: "True", wantReason: "Resolved",
		},
		{
			name:         "a private repository without a token",
			objs:         []ctrlcli.Object{testHubArtifact("")},
			revision:     http.StatusUnauthorized,
			wantResolved: "False", wantReason: modelartifact.ReasonAccessDenied, wantDegraded: true,
		},
		{
			name:         "a Secret that does not exist yet",
			objs:         []ctrlcli.Object{testHubArtifact("hf-token")},
			revision:     http.StatusOK,
			wantResolved: "False", wantReason: "SecretNotFound", wantDegraded: true,
		},
		{
			name:         "a Secret without the token key",
			objs:         []ctrlcli.Object{testHubArtifact("hf-token"), testTokenSecret("")},
			revision:     http.StatusOK,
			wantResolved: "False", wantReason: "SecretNotFound", wantDegraded: true,
		},
		{
			name:         "an outage",
			objs:         []ctrlcli.Object{testHubArtifact("")},
			revision:     http.StatusBadGateway,
			wantResolved: "False", wantReason: modelartifact.ReasonSourceUnavailable, wantDegraded: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestArtifactEnv(t, c.objs...)
			env.hub.revision.Store(int32(c.revision))

			ma, res := env.reconcile(t, "qwen")
			assert.Contains(t, ma.Finalizers, ModelArtifactProtectionFinalizer)
			assert.Equal(t, c.wantResolved, ModelArtifactConditionResolved.GetStatus(ma))
			assert.Equal(t, c.wantReason, ModelArtifactConditionResolved.GetReason(ma))
			assert.Equal(t, c.wantDegraded, ModelArtifactConditionDegraded.IsTrue(ma))
			assert.Positive(t, res.RequeueAfter)
			raw, err := json.Marshal(ma.Status)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), testArtifactToken)
			if c.wantResolved != "True" {
				assert.Nil(t, ma.Status.Resolved)
				return
			}
			require.NotNil(t, ma.Status.Resolved)
			assert.Equal(t, testArtifactCommit, ma.Status.Resolved.Revision)
			assert.True(t, strings.HasPrefix(ma.Status.Resolved.ManifestDigest, "sha256:"))
			assert.Equal(t, int64(2), ma.Status.Resolved.FileCount)
			assert.Equal(t, int64(110), ma.Status.Resolved.SizeBytes)
			assert.Equal(t, int64(1), ma.Status.ObservedGeneration)
		})
	}
}

func TestModelArtifactReconcileFiltersTheManifest(t *testing.T) {
	cases := []struct {
		name         string
		allow        []string
		ignore       []string
		wantResolved string
		wantReason   string
		wantFiles    int64
		wantSize     int64
	}{
		{name: "no pattern is the whole commit", wantResolved: "True", wantReason: "Resolved", wantFiles: 2, wantSize: 110},
		{
			name: "an allow pattern keeps its files", allow: []string{"*.safetensors"},
			wantResolved: "True", wantReason: "Resolved", wantFiles: 1, wantSize: 100,
		},
		{
			name: "an ignore pattern drops its files", ignore: []string{"*.safetensors"},
			wantResolved: "True", wantReason: "Resolved", wantFiles: 1, wantSize: 10,
		},
		{
			name: "a filter keeping nothing is an empty manifest", allow: []string{"*.gguf"},
			wantResolved: "False", wantReason: modelartifact.ReasonEmptyManifest,
		},
	}
	var wholeDigest string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ma := testHubArtifact("")
			ma.Spec.AllowPatterns, ma.Spec.IgnorePatterns = c.allow, c.ignore
			env := newTestArtifactEnv(t, ma)

			got, _ := env.reconcile(t, "qwen")
			assert.Equal(t, c.wantResolved, ModelArtifactConditionResolved.GetStatus(got))
			assert.Equal(t, c.wantReason, ModelArtifactConditionResolved.GetReason(got))
			if c.wantResolved != "True" {
				assert.Nil(t, got.Status.Resolved)
				return
			}
			require.NotNil(t, got.Status.Resolved)
			assert.Equal(t, c.wantFiles, got.Status.Resolved.FileCount)
			assert.Equal(t, c.wantSize, got.Status.Resolved.SizeBytes)
			if c.allow == nil && c.ignore == nil {
				wholeDigest = got.Status.Resolved.ManifestDigest
				return
			}
			assert.NotEqual(t, wholeDigest, got.Status.Resolved.ManifestDigest)
		})
	}
}

func TestModelArtifactReconcileSendsTheNamespaceToken(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact("hf-token"), testTokenSecret(testArtifactToken))

	env.reconcile(t, "qwen")
	assert.Equal(t, "Bearer "+testArtifactToken, env.hub.auth.Load())
}

func TestModelArtifactReconcileWarnsOnARejectedToken(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact("hf-token"), testTokenSecret("mistyped"))
	env.hub.whoami.Store(http.StatusUnauthorized)

	ma, _ := env.reconcile(t, "qwen")
	assert.True(t, ModelArtifactConditionResolved.IsTrue(ma))
	require.Len(t, env.recorder.Events, 1)
	event := <-env.recorder.Events
	assert.Contains(t, event, "Warning InvalidToken")
	assert.NotContains(t, event, "mistyped")
}

func TestModelArtifactReconcileRevalidation(t *testing.T) {
	type step struct {
		after        time.Duration // clock advance before the reconcile
		access       int           // HEAD status the Hub answers
		wantResolved string
		wantDegraded string // reason, "" for not degraded
		wantAsked    bool   // whether the Hub was asked at all
	}
	day := 24 * time.Hour
	cases := []struct {
		name  string
		steps []step
	}{
		{
			name: "a confirmed refusal revokes after a minute, and an early reconcile asks nothing",
			steps: []step{
				{after: day, access: http.StatusUnauthorized, wantResolved: "True", wantDegraded: modelartifact.ReasonAccessDenied, wantAsked: true},
				{after: 10 * time.Second, access: http.StatusUnauthorized, wantResolved: "True", wantDegraded: modelartifact.ReasonAccessDenied},
				{after: time.Minute, access: http.StatusUnauthorized, wantResolved: "False", wantDegraded: modelartifact.ReasonAccessDenied, wantAsked: true},
			},
		},
		{
			name: "a refusal that clears before its confirmation never revokes",
			steps: []step{
				{after: day, access: http.StatusForbidden, wantResolved: "True", wantDegraded: modelartifact.ReasonAccessDenied, wantAsked: true},
				{after: time.Minute, access: http.StatusOK, wantResolved: "True", wantAsked: true},
			},
		},
		{
			name: "an outage never revokes",
			steps: []step{
				{after: day, access: http.StatusServiceUnavailable, wantResolved: "True", wantDegraded: modelartifact.ReasonSourceUnavailable, wantAsked: true},
				{after: time.Minute, access: http.StatusServiceUnavailable, wantResolved: "True", wantDegraded: modelartifact.ReasonSourceUnavailable, wantAsked: true},
				{after: time.Minute, access: http.StatusServiceUnavailable, wantResolved: "True", wantDegraded: modelartifact.ReasonSourceUnavailable, wantAsked: true},
			},
		},
		{
			name: "a revoked artifact comes back when access returns",
			steps: []step{
				{after: day, access: http.StatusUnauthorized, wantResolved: "True", wantDegraded: modelartifact.ReasonAccessDenied, wantAsked: true},
				{after: time.Minute, access: http.StatusUnauthorized, wantResolved: "False", wantDegraded: modelartifact.ReasonAccessDenied, wantAsked: true},
				{after: 10 * time.Minute, access: http.StatusOK, wantResolved: "True", wantAsked: true},
			},
		},
		{
			name: "nothing is asked before the interval",
			steps: []step{
				{after: time.Hour, access: http.StatusUnauthorized, wantResolved: "True"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestArtifactEnv(t, testHubArtifact(""))
			resolved, _ := env.reconcile(t, "qwen")
			require.True(t, ModelArtifactConditionResolved.IsTrue(resolved))
			digest := resolved.Status.Resolved.ManifestDigest

			for i, s := range c.steps {
				env.clock.now = env.clock.now.Add(s.after)
				env.hub.access.Store(int32(s.access))
				asked := env.hub.requests.Load()

				ma, res := env.reconcile(t, "qwen")
				assert.Equal(t, s.wantAsked, env.hub.requests.Load() > asked, "step %d asked", i)
				assert.Equal(t, s.wantResolved, ModelArtifactConditionResolved.GetStatus(ma), "step %d resolved", i)
				if s.wantDegraded == "" {
					assert.False(t, ModelArtifactConditionDegraded.IsTrue(ma), "step %d degraded", i)
				} else {
					assert.Equal(t, s.wantDegraded, ModelArtifactConditionDegraded.GetReason(ma), "step %d degraded", i)
				}
				assert.Equal(t, testArtifactCommit, ma.Status.Resolved.Revision, "step %d", i)
				assert.Equal(t, digest, ma.Status.Resolved.ManifestDigest, "step %d", i)
				assert.Positive(t, res.RequeueAfter, "step %d", i)
			}
		})
	}
}

func TestModelArtifactReconcileChecksAtOnceWhenTheSecretChanges(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact("hf-token"), testTokenSecret(testArtifactToken))
	env.reconcile(t, "qwen")

	secret := new(core.Secret)
	require.NoError(t, env.cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "hf-token"}, secret))
	secret.Data["token"] = []byte("revoked")
	require.NoError(t, env.cli.Update(context.Background(), secret))
	env.hub.access.Store(http.StatusUnauthorized)
	env.clock.now = env.clock.now.Add(time.Second)

	ma, _ := env.reconcile(t, "qwen")
	assert.True(t, ModelArtifactConditionResolved.IsTrue(ma))
	assert.Equal(t, modelartifact.ReasonAccessDenied, ModelArtifactConditionDegraded.GetReason(ma))
	assert.Equal(t, "Bearer revoked", env.hub.auth.Load())
}

func TestModelArtifactReconcileRevokesWhenTheSecretGoes(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact("hf-token"), testTokenSecret(testArtifactToken))
	env.reconcile(t, "qwen")

	require.NoError(t, env.cli.Delete(context.Background(), testTokenSecret("")))
	ma, _ := env.reconcile(t, "qwen")
	assert.Equal(t, "False", ModelArtifactConditionResolved.GetStatus(ma))
	assert.Equal(t, "SecretNotFound", ModelArtifactConditionResolved.GetReason(ma))
	assert.Equal(t, testArtifactCommit, ma.Status.Resolved.Revision)
}

func TestModelArtifactReconcileResolvesAClaim(t *testing.T) {
	claimArtifact := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen", UID: "uid-qwen"},
		Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
			PersistentVolumeClaim: &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: "models", Path: "qwen"},
		}},
	}
	claim := &core.PersistentVolumeClaim{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "models"}}
	cases := []struct {
		name       string
		objs       []ctrlcli.Object
		wantStatus string
		wantReason string
	}{
		{name: "the claim exists", objs: []ctrlcli.Object{claimArtifact.DeepCopy(), claim.DeepCopy()}, wantStatus: "True", wantReason: "Resolved"},
		{name: "the claim does not exist yet", objs: []ctrlcli.Object{claimArtifact.DeepCopy()}, wantStatus: "False", wantReason: "ClaimNotFound"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestArtifactEnv(t, c.objs...)

			ma, _ := env.reconcile(t, "qwen")
			assert.Equal(t, c.wantStatus, ModelArtifactConditionResolved.GetStatus(ma))
			assert.Equal(t, c.wantReason, ModelArtifactConditionResolved.GetReason(ma))
			assert.Zero(t, env.hub.requests.Load())
			if c.wantStatus == "True" {
				require.NotNil(t, ma.Status.Resolved)
				assert.Empty(t, ma.Status.Resolved.Revision)
				assert.Empty(t, ma.Status.Resolved.ManifestDigest)
			}
		})
	}
}

func TestModelArtifactReconcileProtection(t *testing.T) {
	deleting := func() *workercore.ModelArtifact {
		ma := testHubArtifact("")
		now := meta.Now()
		ma.DeletionTimestamp = &now
		ma.Finalizers = []string{ModelArtifactProtectionFinalizer}
		return ma
	}
	referencingDeployment := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "md"},
		Spec:       workercore.ModelDeploymentSpec{Model: workercore.ModelDeploymentModel{Name: "qwen", ArtifactRef: &core.LocalObjectReference{Name: "qwen"}}},
	}
	otherDeployment := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "other"},
		Spec:       workercore.ModelDeploymentSpec{Model: workercore.ModelDeploymentModel{Name: "other", ArtifactRef: &core.LocalObjectReference{Name: "other"}}},
	}
	referencingInstance := &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "inst"},
		Spec: workercore.InstanceSpec{InstanceTemplate: workercore.InstanceTemplate{AdditionalVolumes: []workercore.InstanceAdditionalVolume{
			{MountPath: "/models", Model: &workercore.InstanceModelVolumeSource{ArtifactRef: core.LocalObjectReference{Name: "qwen"}}},
		}}},
	}
	elsewhere := referencingDeployment.DeepCopy()
	elsewhere.Namespace = "team-b"

	cases := []struct {
		name     string
		objs     []ctrlcli.Object
		wantGone bool
	}{
		{name: "a referencing deployment keeps it", objs: []ctrlcli.Object{deleting(), referencingDeployment}},
		{name: "a referencing Instance keeps it", objs: []ctrlcli.Object{deleting(), referencingInstance}},
		{name: "an unrelated deployment does not", objs: []ctrlcli.Object{deleting(), otherDeployment}, wantGone: true},
		{name: "a reference in another namespace does not", objs: []ctrlcli.Object{deleting(), elsewhere}, wantGone: true},
		{name: "no reference does not", objs: []ctrlcli.Object{deleting()}, wantGone: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestArtifactEnv(t, c.objs...)
			env.r.checks.Store(types.UID("uid-qwen"), modelArtifactCheck{})

			key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}
			_, err := env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			err = env.cli.Get(context.Background(), key, new(workercore.ModelArtifact))
			assert.Equal(t, c.wantGone, err != nil, "artifact gone")
			_, kept := env.r.checks.Load(types.UID("uid-qwen"))
			assert.Equal(t, !c.wantGone, kept, "pacing entry kept")
		})
	}
}

func TestModelArtifactReconcileEnqueuesOnlyTerminatingArtifactsForReferences(t *testing.T) {
	terminating := testHubArtifact("")
	now := meta.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{ModelArtifactProtectionFinalizer}
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "md"},
		Spec:       workercore.ModelDeploymentSpec{Model: workercore.ModelDeploymentModel{Name: "qwen", ArtifactRef: &core.LocalObjectReference{Name: "qwen"}}},
	}
	cases := []struct {
		name     string
		artifact *workercore.ModelArtifact
		want     int
	}{
		{name: "a terminating artifact is woken", artifact: terminating, want: 1},
		{name: "a live artifact is not", artifact: testHubArtifact(""), want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestArtifactEnv(t, c.artifact)
			assert.Len(t, env.r.enqueueTerminatingModelArtifactOfDeployment(context.Background(), md), c.want)
		})
	}
}

func TestModelArtifactReconcileWritesResolvingBeforeAsking(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact(""))
	key := ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}

	res, err := env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.True(t, res.Requeue)
	ma := new(workercore.ModelArtifact)
	require.NoError(t, env.cli.Get(context.Background(), key, ma))
	assert.Equal(t, "Unknown|Resolving", ModelArtifactConditionResolved.GetStatus(ma)+"|"+ModelArtifactConditionResolved.GetReason(ma))
	assert.Zero(t, env.hub.requests.Load(), "the source is not asked before Resolving is stored")
}

func TestModelArtifactReconcileAsksAgainWhenTheStatusWriteFails(t *testing.T) {
	var failNext atomic.Bool
	hub := newTestArtifactHub(t)
	base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelArtifact{}).
		WithObjects(testHubArtifact("")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
				if failNext.CompareAndSwap(true, false) {
					return errors.New("injected status write failure")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &ModelArtifactReconciler{
		Client: base, Now: time.Now,
		NewHuggingFace: func(context.Context) (*modelartifact.HuggingFace, error) {
			return &modelartifact.HuggingFace{Endpoint: hub.server.URL, Client: hub.server.Client()}, nil
		},
	}
	key := ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}}

	_, err := r.Reconcile(context.Background(), key) // writes Resolving
	require.NoError(t, err)
	failNext.Store(true)
	_, err = r.Reconcile(context.Background(), key) // resolves, and the write is lost
	require.Error(t, err)
	asked := hub.requests.Load()

	_, err = r.Reconcile(context.Background(), key)
	require.NoError(t, err)
	assert.Greater(t, hub.requests.Load(), asked, "the lost answer is asked for again at once")
	ma := new(workercore.ModelArtifact)
	require.NoError(t, base.Get(context.Background(), key.NamespacedName, ma))
	assert.True(t, ModelArtifactConditionResolved.IsTrue(ma))
}

// TestModelArtifactReconcileExpectedStatusWriteFailuresAreQuiet pins how a reconcile ends when the
// artifact changed or was deleted after it was read, at either status write. Both heal on their own,
// the conflict through the change event that reconciles the artifact again, so they end the reconcile
// without an error and without an error log. Any other failure is still returned and logged, and the
// next reconcile resolves the artifact in every case.
func TestModelArtifactReconcileExpectedStatusWriteFailuresAreQuiet(t *testing.T) {
	gr := schema.GroupResource{Group: workercore.GroupVersion.Group, Resource: "modelartifacts"}
	testCases := []struct {
		name       string
		failWrite  int // which status write fails: 1 writes Resolving, 2 writes the answer
		err        error
		wantErr    bool
		wantLogged int
	}{
		{
			name:      "a conflicting Resolving write ends quietly",
			failWrite: 1,
			err:       kerrors.NewConflict(gr, "qwen", errors.New("the object has been modified")),
		},
		{
			name:      "a Resolving write on a deleted artifact ends quietly",
			failWrite: 1,
			err:       kerrors.NewNotFound(gr, "qwen"),
		},
		{
			name:      "a conflicting answer write ends quietly",
			failWrite: 2,
			err:       kerrors.NewConflict(gr, "qwen", errors.New("the object has been modified")),
		},
		{
			name:      "an answer write on a deleted artifact ends quietly",
			failWrite: 2,
			err:       kerrors.NewNotFound(gr, "qwen"),
		},
		{
			name:       "any other answer write failure is returned and logged",
			failWrite:  2,
			err:        kerrors.NewInternalError(errors.New("etcd unavailable")),
			wantErr:    true,
			wantLogged: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestArtifactEnv(t, testHubArtifact(""))
			var (
				writes   int
				injected bool
			)
			env.r.Client = interceptor.NewClient(env.cli.(ctrlcli.WithWatch), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
					if _, ok := obj.(*workercore.ModelArtifact); ok {
						writes++
						if writes == tc.failWrite {
							injected = true
							return tc.err
						}
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			})
			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}}

			out := reconcileUntilInjected(t, env.r, req, &injected)
			if tc.wantErr {
				assert.Error(t, out.err)
			} else {
				assert.NoError(t, out.err)
			}
			assert.Equal(t, ctrl.Result{}, out.res)
			assert.Equal(t, tc.wantLogged, out.logged, "error log lines")

			// The next passes, which the change event or the returned error triggers, resolve it.
			ma, _ := env.reconcile(t, "qwen")
			assert.True(t, ModelArtifactConditionResolved.IsTrue(ma))
		})
	}
}

func TestModelArtifactReconcileASecretReadFailureNeverRevokes(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact("hf-token"), testTokenSecret(testArtifactToken))
	env.reconcile(t, "qwen")

	var failing atomic.Bool
	failing.Store(true)
	env.r.Client = interceptor.NewClient(env.cli.(ctrlclientWithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
			if _, secret := obj.(*core.Secret); secret && failing.Load() {
				return errors.New("injected read failure")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	env.clock.now = env.clock.now.Add(25 * time.Hour)

	ma, _ := env.reconcile(t, "qwen")
	assert.True(t, ModelArtifactConditionResolved.IsTrue(ma), "an unreadable Secret is not a revoked one")
	assert.Equal(t, modelartifact.ReasonSourceUnavailable, ModelArtifactConditionDegraded.GetReason(ma))
}

func TestModelArtifactReconcileJudgesTheTokenOnlyWhenItChanges(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact("hf-token"), testTokenSecret("mistyped"))
	env.hub.whoami.Store(http.StatusUnauthorized)
	env.reconcile(t, "qwen")
	require.Len(t, env.recorder.Events, 1)
	<-env.recorder.Events

	env.clock.now = env.clock.now.Add(25 * time.Hour)
	env.reconcile(t, "qwen")
	assert.Empty(t, env.recorder.Events, "an unchanged rejected token is not reported again")
}

// ctrlclientWithWatch is the fake client's own interface, which the interceptor wraps.
type ctrlclientWithWatch = ctrlcli.WithWatch
