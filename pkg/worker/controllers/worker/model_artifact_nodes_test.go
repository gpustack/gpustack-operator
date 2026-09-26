package worker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// resolvedTestArtifact resolves the test artifact and returns its digest.
func resolvedTestArtifact(t *testing.T, env *testArtifactEnv) string {
	t.Helper()
	env.hub.revision.Store(http.StatusOK)
	ma, _ := env.reconcile(t, "qwen")
	require.NotNil(t, ma.Status.Resolved)
	return ma.Status.Resolved.ManifestDigest
}

func setTestNodeStore(t *testing.T, cli ctrlcli.Client, node string, models ...workercore.NodeModelStoreModel) {
	t.Helper()
	nms := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: node}}
	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node}, nms)
	nms.Status.Models = models
	if err != nil {
		require.NoError(t, cli.Create(context.Background(), nms))
		return
	}
	require.NoError(t, cli.Update(context.Background(), nms))
}

func TestModelArtifactReconcileCountsItsNodes(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact(""))
	digest := resolvedTestArtifact(t, env)
	setTestNodeStore(t, env.cli, "n1", workercore.NodeModelStoreModel{Digest: digest, State: workercore.NodeModelStoreModelStateReady})
	setTestNodeStore(t, env.cli, "n2", workercore.NodeModelStoreModel{
		Digest: digest, State: workercore.NodeModelStoreModelStateDownloading, SizeBytes: 100, DownloadedBytes: 47,
	})
	setTestNodeStore(t, env.cli, "n3",
		workercore.NodeModelStoreModel{Digest: digest, State: workercore.NodeModelStoreModelStateFailed, Reason: "SourceUnavailable"},
		workercore.NodeModelStoreModel{Digest: "sha256:another", State: workercore.NodeModelStoreModelStateReady})
	env.clock.now = env.clock.now.Add(time.Minute)

	ma, _ := env.reconcile(t, "qwen")
	assert.Equal(t, &workercore.ModelArtifactNodes{Ready: 1, Downloading: 1, Failed: 1, DownloadingPercent: ptr.To[int32](45)}, ma.Status.Nodes,
		"47% of the one downloading node, in a step of 5; the ready node is counted, not averaged")
}

func TestModelArtifactReconcileWritesItsNodesAtMostOncePerWindow(t *testing.T) {
	env := newTestArtifactEnv(t, testHubArtifact(""))
	digest := resolvedTestArtifact(t, env)
	downloading := func(n int64) workercore.NodeModelStoreModel {
		return workercore.NodeModelStoreModel{Digest: digest, State: workercore.NodeModelStoreModelStateDownloading, SizeBytes: 100, DownloadedBytes: n}
	}
	env.clock.now = env.clock.now.Add(time.Minute)

	setTestNodeStore(t, env.cli, "n1", downloading(10))
	ma, _ := env.reconcile(t, "qwen")
	require.Equal(t, ptr.To[int32](10), ma.Status.Nodes.DownloadingPercent)
	rv := ma.ResourceVersion

	// Inside the window a new step is held, and the pass asks to come back when the window ends.
	env.clock.now = env.clock.now.Add(10 * time.Second)
	setTestNodeStore(t, env.cli, "n1", downloading(20))
	ma, res := env.reconcile(t, "qwen")
	assert.Equal(t, rv, ma.ResourceVersion, "no write inside the window")
	assert.Equal(t, ptr.To[int32](10), ma.Status.Nodes.DownloadingPercent)
	assert.Equal(t, 20*time.Second, res.RequeueAfter)

	// Past it the held step is written.
	env.clock.now = env.clock.now.Add(20 * time.Second)
	ma, _ = env.reconcile(t, "qwen")
	assert.Equal(t, ptr.To[int32](20), ma.Status.Nodes.DownloadingPercent)

	// Progress inside a step writes nothing, window or not.
	env.clock.now = env.clock.now.Add(time.Minute)
	setTestNodeStore(t, env.cli, "n1", downloading(24))
	rv = ma.ResourceVersion
	ma, _ = env.reconcile(t, "qwen")
	assert.Equal(t, rv, ma.ResourceVersion)
}

func TestModelArtifactReconcileNodesOfAClaim(t *testing.T) {
	assert.Nil(t, modelArtifactNodes(&workercore.ModelArtifact{Spec: workercore.ModelArtifactSpec{Source: workercore.ModelArtifactSource{
		PersistentVolumeClaim: &workercore.ModelArtifactPersistentVolumeClaimSource{ClaimName: "models"},
	}}}, nil), "a claim is never downloaded")
	assert.Nil(t, modelArtifactNodes(testHubArtifact(""), nil), "an unresolved hub source has nothing to count")
}

func TestNodeModelStoreHandlerEnqueuesOnViewChanges(t *testing.T) {
	store := func(models ...workercore.NodeModelStoreModel) *workercore.NodeModelStore {
		return &workercore.NodeModelStore{Status: workercore.NodeModelStoreStatus{Models: models}}
	}
	dl := func(digest string, n int64) workercore.NodeModelStoreModel {
		return workercore.NodeModelStoreModel{Digest: digest, State: workercore.NodeModelStoreModelStateDownloading, SizeBytes: 100, DownloadedBytes: n}
	}
	ready := workercore.NodeModelStoreModel{Digest: "sha256:a", State: workercore.NodeModelStoreModelStateReady}

	cases := []struct {
		name     string
		old, new *workercore.NodeModelStore
		want     map[string]bool
	}{
		{name: "progress inside a step", old: store(dl("sha256:a", 11)), new: store(dl("sha256:a", 14)), want: map[string]bool{}},
		{name: "a new step", old: store(dl("sha256:a", 14)), new: store(dl("sha256:a", 15)), want: map[string]bool{"sha256:a": true}},
		{name: "published", old: store(dl("sha256:a", 99)), new: store(ready), want: map[string]bool{"sha256:a": true}},
		{name: "collected", old: store(ready, dl("sha256:b", 1)), new: store(dl("sha256:b", 1)), want: map[string]bool{"sha256:a": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, changedDigests(nodeModelStoreView(c.old), nodeModelStoreView(c.new)))
		})
	}
}

// TestModelArtifactNodesOfASharedDigest pins that artifacts in different namespaces naming one digest
// are each enqueued by a store change and each written, every one on its own window.
func TestModelArtifactNodesOfASharedDigest(t *testing.T) {
	other := testHubArtifact("")
	other.Namespace, other.UID = "team-b", "uid-other"
	env := newTestArtifactEnv(t, testHubArtifact(""), other)
	digest := resolvedTestArtifact(t, env)
	key := ctrlcli.ObjectKey{Namespace: "team-b", Name: "qwen"}
	for i := 0; i < 3; i++ {
		_, err := env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	env.clock.now = env.clock.now.Add(time.Minute)

	old := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "n1"}}
	setTestNodeStore(t, env.cli, "n1", workercore.NodeModelStoreModel{Digest: digest, State: workercore.NodeModelStoreModelStateReady})
	updated := new(workercore.NodeModelStore)
	require.NoError(t, env.cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "n1"}, updated))

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrlreconcile.Request]())
	t.Cleanup(q.ShutDown)
	env.r.nodeModelStoreHandler().Update(context.Background(), ctrlevent.UpdateEvent{ObjectOld: old, ObjectNew: updated}, q)
	require.Equal(t, 2, q.Len(), "both artifacts naming the digest are enqueued")

	for _, ns := range []string{"team-a", "team-b"} {
		_, err := env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: ns, Name: "qwen"}})
		require.NoError(t, err)
		ma := new(workercore.ModelArtifact)
		require.NoError(t, env.cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: ns, Name: "qwen"}, ma))
		require.NotNil(t, ma.Status.Nodes, ns)
		assert.Equal(t, int32(1), ma.Status.Nodes.Ready, ns)
	}
}
