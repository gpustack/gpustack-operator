package deviceplugin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

// reclaimHarness drives one reclaimLoop with a latest closure the test writes and a wake channel
// the test fills. It is the seam the loop's contract is stated in: a store is a write the getter
// will serve, a wake is a signal, and a call is one reconcile.
type reclaimHarness struct {
	mu      sync.Mutex
	current []string
	stored  bool

	calls  chan []string
	wake   chan struct{}
	resync chan time.Time
	cancel context.CancelFunc
	done   chan struct{}
}

// newReclaimHarness builds a loop whose wake channel has the given buffer size and starts it.
// Buffer 1 is what production hands the loop: one pending wake, a second send dropped.
func newReclaimHarness(t *testing.T, wakeBuffer int) *reclaimHarness {
	t.Helper()
	h := newReclaimHarnessUnstarted()
	h.wake = make(chan struct{}, wakeBuffer)
	h.start(t)
	return h
}

// newReclaimHarnessUnstarted builds a loop with a one-slot wake channel and does NOT start it, so
// a row can fill the wake slot and store sets before anything exists to drain or read them.
func newReclaimHarnessUnstarted() *reclaimHarness {
	return &reclaimHarness{
		calls:  make(chan []string, 16),
		wake:   make(chan struct{}, 1),
		resync: make(chan time.Time),
		done:   make(chan struct{}),
	}
}

// start runs the loop. Split from the constructor so a row can fill the wake slot before the loop
// exists to drain it.
func (h *reclaimHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		reclaimLoop(ctx, func(live []string) { h.calls <- live }, h.latest, h.wake, h.resync)
		close(h.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(time.Second):
			t.Error("reclaim loop did not stop on cancel")
		}
	})
}

// latest is the getter handed to the loop.
func (h *reclaimHarness) latest() ([]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current, h.stored
}

// store makes set the newest stored set.
func (h *reclaimHarness) store(set []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.current = set
	h.stored = true
}

// expectCall reads one reconcile call and checks its set.
func (h *reclaimHarness) expectCall(t *testing.T, want []string, when string) {
	t.Helper()
	select {
	case got := <-h.calls:
		assert.Equal(t, want, got, when)
	case <-time.After(time.Second):
		t.Fatalf("%s: reconcile was not called", when)
	}
}

// expectNoCall checks that the loop stays quiet for a while.
func (h *reclaimHarness) expectNoCall(t *testing.T, when string) {
	t.Helper()
	select {
	case got := <-h.calls:
		t.Fatalf("%s: unexpected reconcile with %v", when, got)
	case <-time.After(50 * time.Millisecond):
	}
}

// expectCalls reads exactly the wanted calls, in order, then checks the loop is quiet.
func (h *reclaimHarness) expectCalls(t *testing.T, want [][]string) {
	t.Helper()
	for _, w := range want {
		h.expectCall(t, w, "expected call")
	}
	h.expectNoCall(t, "after the expected calls")
}

// Test_reclaimLoop pins the loop's contract: a tick or a wake before any set is stored never
// reconciles, a wake or a tick reconciles with the newest stored set, two stores before one wake
// reconcile once with the newer set, and a wake dropped behind a pending one still reconciles with
// the newest set.
func Test_reclaimLoop(t *testing.T) {
	t.Run("tick before any stored set does not reconcile", func(t *testing.T) {
		h := newReclaimHarness(t, 0)
		h.resync <- time.Time{}
		h.expectNoCall(t, "a tick before any broadcast")
	})

	t.Run("wake before any stored set does not reconcile", func(t *testing.T) {
		h := newReclaimHarness(t, 0)
		h.wake <- struct{}{}
		h.expectNoCall(t, "a wake before any broadcast")
	})

	t.Run("a wake reconciles with the stored set", func(t *testing.T) {
		h := newReclaimHarness(t, 0)
		h.store([]string{"pod-a"})
		h.wake <- struct{}{}
		h.expectCalls(t, [][]string{{"pod-a"}})
	})

	t.Run("a tick reconciles with the newest stored set", func(t *testing.T) {
		h := newReclaimHarness(t, 0)
		h.store([]string{"pod-a"})
		h.resync <- time.Time{}
		h.expectCall(t, []string{"pod-a"}, "first tick")

		h.store([]string{"pod-a", "pod-b"})
		h.resync <- time.Time{}
		h.expectCalls(t, [][]string{{"pod-a", "pod-b"}})
	})

	t.Run("two stores before the loop runs then one wake reconcile once with the newer", func(t *testing.T) {
		// The loop does not exist yet, so nothing can drain the wake or read a set early. This is
		// the reclaim loop starting after a burst of Reconciles: it must read the set as it is NOW.
		h := newReclaimHarnessUnstarted()
		h.store([]string{"pod-a"})
		h.store([]string{"pod-b"})
		h.wake <- struct{}{}
		h.start(t)
		h.expectCalls(t, [][]string{{"pod-b"}})
	})

	t.Run("a store landed after the wake was sent is what the wake reconciles with", func(t *testing.T) {
		// The ordering that defeated the old buffered channel: a wake is already pending, THEN a
		// newer set is stored, THEN the loop gets to run. The old loop replayed the set the wake
		// carried; this one reads the set at the moment it runs, which is the newer one.
		h := newReclaimHarnessUnstarted()
		h.store([]string{"pod-a", "pod-dead"})
		h.wake <- struct{}{}
		h.store([]string{"pod-a"})
		h.start(t)
		h.expectCalls(t, [][]string{{"pod-a"}})
	})

	t.Run("a wake dropped behind a pending one still reconciles with the newest set", func(t *testing.T) {
		// Store, wake, store, wake: the second wake is dropped behind the first because no loop
		// is running to drain it, and the drop is therefore deterministic. The one wake that is
		// delivered must still reconcile with the second set.
		h := newReclaimHarnessUnstarted()
		h.store([]string{"pod-a"})
		h.wake <- struct{}{}
		h.store([]string{"pod-b"})
		select {
		case h.wake <- struct{}{}:
			t.Fatal("precondition: the wake slot must be full so this send is dropped")
		default:
		}
		h.start(t)
		h.expectCalls(t, [][]string{{"pod-b"}})
	})

	t.Run("ctx cancel stops the loop", func(t *testing.T) {
		h := newReclaimHarness(t, 0)
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(time.Second):
			t.Fatal("loop did not stop on ctx cancel")
		}
	})
}

// Test_reclaimLoop_DroppedWakeReconcilesNewest reproduces the dropped-wake shape through the
// reconciler's own broadcaster rather than a hand-filled channel: two notifyListeners calls with
// no loop running yet, so the second wake is dropped behind the first, and the loop that then
// starts reconciles with the second set and never the first.
func Test_reclaimLoop_DroppedWakeReconcilesNewest(t *testing.T) {
	reconciler := &DevicesReconciler{}

	// Subscribe first, with no reader, so the wake has somewhere to land and nowhere to be drained.
	wake, release := reconciler.getReconcileNotifier(nodefeature.ManufacturerNVIDIA,
		workercore.DeviceAllocationModePartitioned)
	defer release()
	require.Empty(t, wake, "a fresh subscription before any broadcast must carry no seed")

	broadcast := func(set []string) {
		reconciler.notifiersMutex.Lock()
		reconciler.lastLivePodUIDs = set
		reconciler.notifiersMutex.Unlock()
		reconciler.notifyListeners()
	}
	broadcast([]string{"pod-a"})
	require.Len(t, wake, 1, "the first broadcast leaves one wake pending")
	broadcast([]string{"pod-b"})
	require.Len(t, wake, 1, "the second broadcast is dropped behind the pending wake")

	// Now the loop starts and drains that single wake. It must read the set stored NOW.
	calls := make(chan []string, 16)
	latest := reconciler.livePodUIDs
	resync := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		reclaimLoop(ctx, func(live []string) { calls <- live }, latest, wake, resync)
		close(done)
	}()

	select {
	case got := <-calls:
		assert.Equal(t, []string{"pod-b"}, got, "the loop must read the newest set, not the one the wake was sent for")
	case <-time.After(time.Second):
		t.Fatal("reclaim loop was not woken by the pending wake")
	}
	select {
	case got := <-calls:
		t.Fatalf("a dropped wake must not produce a second reconcile; got %v", got)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop on ctx cancel")
	}
}

// reclaimSubscriptionTimeout bounds the waits below, so a reclaim loop that cannot be ended fails
// the test instead of stalling the suite.
const reclaimSubscriptionTimeout = 10 * time.Second

// TestRunReclaimLoop_StoppedLifecycleReleasesSubscription is the wiring this whole seam exists for: the reclaim
// loop subscribes to the reconciler's broadcast and releases on its way out, so an allocator that
// cannot end its reclaim loop grows the notifier set by one per detect/undetect cycle — a set walked
// in full on every broadcast, some of them synchronously on the allocate path.
func TestRunReclaimLoop_StoppedLifecycleReleasesSubscription(t *testing.T) {
	reconciler := &DevicesReconciler{}
	var l gox.Lifecycle

	errCh := make(chan error, 1)
	go func() {
		errCh <- l.Start(context.Background(), func(ctx context.Context) error {
			RunReclaimLoop(ctx, reconciler, nodefeature.ManufacturerNVIDIA,
				workercore.DeviceAllocationModePartitioned, func([]string) {})
			return nil
		})
	}()

	notifiers := func() int {
		reconciler.notifiersMutex.RLock()
		defer reconciler.notifiersMutex.RUnlock()

		return len(reconciler.notifiers)
	}
	require.Eventually(t, func() bool {
		return notifiers() == 1
	}, reclaimSubscriptionTimeout, 10*time.Millisecond, "reclaim loop did not subscribe")

	l.Stop()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(reclaimSubscriptionTimeout):
		t.Fatal("Start did not return")
	}
	assert.Zero(t, notifiers(), "a stopped allocator must leave no reclaim subscription behind")
}
