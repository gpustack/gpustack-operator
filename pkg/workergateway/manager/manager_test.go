package manager

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
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
// whose readiness check has not finished must stop that check. The pending attempt used to be
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

// TestSubscribeWorker_StalledCheckIsAbandonedAndRetried guards the per-check time bound: a cluster
// that accepts the connection and then never answers must have its check given up on and retried.
// The check used to be a wait with a deadline of its own; taking that away left the bound to the
// kubernetes client's timeout, which is minutes, so the loop sat in one probe for that long and the
// backoff schedule stopped describing the rate the cluster was probed at.
func TestSubscribeWorker_StalledCheckIsAbandonedAndRetried(t *testing.T) {
	rec := newProbeRecorder()
	wm := newTestManager(t, rec)
	// The test server's rest config carries no timeout of its own, so a probe can only end
	// by running out of the time this bound gives it.
	wm.ReadinessCheckTimeout = 50 * time.Millisecond
	wm.ReadinessBackoff = wait.Backoff{
		Duration: 10 * time.Millisecond,
		Factor:   1,
		Steps:    math.MaxInt32,
	}

	require.NoError(t, wm.SubscribeWorker(context.Background(), "6", "token", nil, false))

	// The handler never answers any of them, so a probe arriving after the first proves the
	// one before it was abandoned rather than waited on.
	assert.Eventually(t, func() bool { return rec.Started.Load() >= 3 },
		5*time.Second, 20*time.Millisecond,
		"a stalled readiness check must be given up on and retried")

	wm.UnsubscribeWorker(context.Background(), "6")
	assert.Eventually(t, func() bool { return rec.InFlight.Load() == 0 },
		5*time.Second, 20*time.Millisecond, "no abandoned check may stay in flight")
}

// _FakeReadiness stands in for the worker api service readiness check, recording when each attempt
// started so the loop around it can be driven without real probes.
type _FakeReadiness struct {
	mu     sync.Mutex
	starts []time.Time

	// err is returned by every attempt.
	err error
}

func (f *_FakeReadiness) Check(_ context.Context, _ kubernetes.Interface) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.starts = append(f.starts, time.Now())

	return f.err
}

func (f *_FakeReadiness) Attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.starts)
}

func (f *_FakeReadiness) Gaps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	gaps := make([]time.Duration, 0, len(f.starts))
	for i := 1; i < len(f.starts); i++ {
		gaps = append(gaps, f.starts[i].Sub(f.starts[i-1]))
	}
	return gaps
}

// newFakeReadinessManager returns a manager whose readiness check is fake and whose retry backoff is
// short enough to observe, so the loop's own behavior can be tested without real probes.
func newFakeReadinessManager(t *testing.T, fake *_FakeReadiness) *_Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	m, err := New(ctx, &Config{
		ConstructRestConfig: func(_, _ string) (*rest.Config, error) {
			return &rest.Config{Host: "https://worker.invalid"}, nil
		},
		ResyncPeriod: time.Minute,
	})
	require.NoError(t, err)

	wm := m.(*_Manager)
	wm.IsServicesReady = fake.Check
	wm.ReadinessBackoff = wait.Backoff{
		Duration: 20 * time.Millisecond,
		Factor:   2,
		Cap:      time.Second,
		Steps:    math.MaxInt32,
	}

	return wm
}

// TestSubscribeWorker_FailedReadinessRetriesUntilUnsubscribed guards the retry half of the loop: a
// cluster whose api services never come up must keep being retried, and must stop the moment it is
// unsubscribed. Before the fix the retry was immediate, so a cluster was probed continuously.
func TestSubscribeWorker_FailedReadinessRetriesUntilUnsubscribed(t *testing.T) {
	fake := &_FakeReadiness{err: errors.New("api services are not ready")}
	wm := newFakeReadinessManager(t, fake)

	require.NoError(t, wm.SubscribeWorker(context.Background(), "6", "token", nil, false))
	assert.Eventually(t, func() bool { return fake.Attempts() >= 3 },
		5*time.Second, 10*time.Millisecond, "a failed readiness wait must be retried")

	wm.UnsubscribeWorker(context.Background(), "6")

	// The loop can already be inside one last wait when the cancel lands, so let that
	// one finish before taking the count. A surviving loop would retry many times over
	// the window that follows.
	time.Sleep(200 * time.Millisecond)
	settled := fake.Attempts()
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, settled, fake.Attempts(), "unsubscribe must stop the retries")
}

// TestSubscribeWorker_BacksOffBetweenFailedAttempts guards the pacing itself: drop the backoff sleep
// from the loop and the gaps collapse, putting the tight retry half of the bug back.
func TestSubscribeWorker_BacksOffBetweenFailedAttempts(t *testing.T) {
	fake := &_FakeReadiness{err: errors.New("api services are not ready")}
	wm := newFakeReadinessManager(t, fake)

	require.NoError(t, wm.SubscribeWorker(context.Background(), "6", "token", nil, false))
	assert.Eventually(t, func() bool { return fake.Attempts() >= 4 },
		5*time.Second, 10*time.Millisecond)
	wm.UnsubscribeWorker(context.Background(), "6")

	// A timer only ever overshoots, so each gap is compared against the step it was
	// scheduled with rather than against another observed gap — which on a loaded runner
	// says nothing. Drop the backoff and the gaps collapse; make it constant and the
	// third one stays at the first one's step.
	step := wm.ReadinessBackoff.Duration
	gaps := fake.Gaps()
	require.GreaterOrEqual(t, len(gaps), 3)
	assert.GreaterOrEqual(t, gaps[0], step, "the first retry must wait its first step")
	assert.GreaterOrEqual(t, gaps[2], 4*step, "each retry must wait longer than the last")
}

// TestReadinessBackoff_GrowsAndCapsAtAMinute pins the default schedule the loop is handed. Since the
// readiness check is a single pass, these gaps are the rate a cluster is actually probed at. The
// loop's use of it is covered by TestSubscribeWorker_BacksOffBetweenFailedAttempts; this fixes the
// numbers.
func TestReadinessBackoff_GrowsAndCapsAtAMinute(t *testing.T) {
	backoff := newReadinessBackoff()

	first := backoff.Step()
	second := backoff.Step()
	third := backoff.Step()

	assert.Greater(t, second, first, "the gap between attempts must grow")
	assert.Greater(t, third, first, "the gap between attempts must keep growing")

	// However long a cluster stays unready, it is probed at least once a minute and no more.
	var last time.Duration
	for i := 0; i < 10; i++ {
		last = backoff.Step()
		assert.LessOrEqual(t, last, time.Minute, "the gap must not grow past the cap")
	}
	assert.EqualValues(t, time.Minute, last, "the gap must settle at the cap")
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
