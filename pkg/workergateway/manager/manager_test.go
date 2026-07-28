package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
)

// TestWorkerSubscribe_BlocksWithoutInformers guards the lifecycle fix for a worker subscribed with
// no GVKs (empty informer set): Subscribe must stay blocked until the worker is canceled, so
// SubscribeWorker does not immediately delete and re-subscribe it. Such a worker stays registered
// and reachable through the live-list proxy.
func TestWorkerSubscribe_BlocksWithoutInformers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wk := &_Worker{
		Context:   ctx,
		Cancel:    cancel,
		Cluster:   "test",
		Informers: make(map[schema.GroupVersionKind]cache.SharedIndexInformer),
	}

	done := make(chan error, 1)
	go func() { done <- wk.Subscribe() }()

	// With no informers Subscribe must not return on its own.
	select {
	case err := <-done:
		t.Fatalf("Subscribe returned before cancel: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	assert.True(t, wk.AllReady.Load(), "a zero-informer worker should report ready")

	// Canceling, as Unsubscribe does, must unblock Subscribe.
	wk.Cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after cancel")
	}
}

// _ProbeRecorder stands in for a cluster whose worker api services never become available. Each
// readiness probe is held until its caller cancels it, so the number of requests in flight is the
// number of readiness loops running.
type _ProbeRecorder struct {
	Started  atomic.Int64
	InFlight atomic.Int64

	arrived chan struct{}
}

func newProbeRecorder() *_ProbeRecorder {
	return &_ProbeRecorder{arrived: make(chan struct{}, 1)}
}

func (p *_ProbeRecorder) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	p.Started.Add(1)
	p.InFlight.Add(1)
	defer p.InFlight.Add(-1)

	select {
	case p.arrived <- struct{}{}:
	default:
	}

	<-r.Context().Done()
}

// WaitForProbe blocks until a readiness probe arrives, leaving the signal drained.
func (p *_ProbeRecorder) WaitForProbe(t *testing.T) {
	t.Helper()

	select {
	case <-p.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("no readiness probe arrived")
	}
}

func newTestManager(t *testing.T, rec *_ProbeRecorder) *_Manager {
	t.Helper()

	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &Config{
		ConstructRestConfig: func(_, _ string) (*rest.Config, error) {
			return &rest.Config{Host: srv.URL}, nil
		},
		ResyncPeriod: time.Minute,
	}
	m, err := cfg.Apply(ctx)
	require.NoError(t, err)

	return m.(*_Manager)
}

// TestSubscribeWorker_UnsubscribePendingStopsProbing guards gpustack#5947: unsubscribing a cluster
// whose readiness wait has not finished must stop that wait. The pending attempt used to be
// untracked, so UnsubscribeWorker found no worker and returned, leaving the loop probing the
// cluster — through the gpustack server's own proxy — forever.
func TestSubscribeWorker_UnsubscribePendingStopsProbing(t *testing.T) {
	rec := newProbeRecorder()
	wm := newTestManager(t, rec)

	require.NoError(t, wm.SubscribeWorker(context.Background(), "6", "token", nil, false))
	rec.WaitForProbe(t)
	assert.EqualValues(t, 1, rec.InFlight.Load(), "exactly one readiness probe in flight")

	wm.UnsubscribeWorker(context.Background(), "6")

	assert.Eventually(t, func() bool { return rec.InFlight.Load() == 0 },
		5*time.Second, 20*time.Millisecond, "unsubscribe must abort the in-flight probe")
}

// TestSubscribeWorker_RepeatedSubscribeStartsOneLoop guards the other half of gpustack#5947: while an
// attempt is pending, every further Cluster event re-entered SubscribeWorker and started another
// client, context and readiness loop for the same cluster.
func TestSubscribeWorker_RepeatedSubscribeStartsOneLoop(t *testing.T) {
	rec := newProbeRecorder()
	wm := newTestManager(t, rec)

	for i := 0; i < 3; i++ {
		require.NoError(t, wm.SubscribeWorker(context.Background(), "6", "token", nil, false))
	}

	rec.WaitForProbe(t)
	// Loops started together probe together, so give any extra one the same chance to arrive.
	time.Sleep(500 * time.Millisecond)

	assert.EqualValues(t, 1, rec.Started.Load(), "only one readiness loop may run per cluster")
	assert.EqualValues(t, 1, rec.InFlight.Load())
}

// newEmptyListServer serves every list request an empty result and counts the requests it saw, so a
// worker pointed at it records how many times it was visited.
func newEmptyListServer(t *testing.T, kind string) (kubernetes.Interface, *atomic.Int64) {
	t.Helper()

	var lists atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lists.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"` + worker.SchemeGroupVersion.String() +
			`","kind":"` + kind + `List","items":[]}`))
	}))
	t.Cleanup(srv.Close)

	cli, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	require.NoError(t, err)

	return cli, &lists
}

// TestIterateWorkers_VisitsEachRegisteredClusterOnce guards the cluster list that IterateWorkers
// falls back to when the caller names none. It is derived from the registrations instead of tracked
// beside them, so it can neither drift from them nor report one cluster twice.
func TestIterateWorkers_VisitsEachRegisteredClusterOnce(t *testing.T) {
	m, err := New(context.Background(), &Config{})
	require.NoError(t, err)
	wm := m.(*_Manager)

	counts := make(map[Cluster]*atomic.Int64, 2)
	for _, cluster := range []Cluster{"a", "b"} {
		cli, lists := newEmptyListServer(t, "Devices")
		counts[cluster] = lists
		wm.Workers[cluster] = &_Worker{
			Context:   context.Background(),
			Cancel:    func() {},
			Cluster:   cluster,
			Client:    cli,
			Informers: make(map[schema.GroupVersionKind]cache.SharedIndexInformer),
		}
	}

	// No informer for the GVK, so each cluster visit is one live list against its own worker.
	err = wm.IterateWorkers(context.Background(), nil, worker.SchemeGroupVersionKind("Devices"),
		IteratorOptions{}, func(string, runtime.Object) error { return nil })
	require.NoError(t, err)

	for cluster, lists := range counts {
		assert.EqualValues(t, 1, lists.Load(), "cluster %q must be visited exactly once", cluster)
	}
}

// TestDefaultInformerFactories_IncludesInstanceTypeFlavor guards the wiring the flavor watch depends
// on: a worker subscribed with the InstanceTypeFlavor GVK must get a watch-backed informer, not just
// the live-list fallback (which delivers no watch events).
func TestDefaultInformerFactories_IncludesInstanceTypeFlavor(t *testing.T) {
	gvk := worker.SchemeGroupVersionKind("InstanceTypeFlavor")
	_, ok := defaultInformerFactories[gvk]
	assert.True(t, ok, "InstanceTypeFlavor must have an informer factory registered")
}
