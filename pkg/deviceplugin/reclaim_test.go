package deviceplugin

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

// reclaimLoop must (1) never reconcile on a resync before the first broadcast seeds
// the live set, (2) reconcile with the broadcast's set on every broadcast, (3)
// reconcile against the last seeded set on a resync tick, and (4) stop on ctx cancel.
func Test_reclaimLoop(t *testing.T) {
	calls := make(chan []string, 8)
	reconcile := func(live []string) { calls <- live }

	notifier := make(chan []string)
	resync := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		reclaimLoop(ctx, reconcile, notifier, resync)
		close(done)
	}()

	// A resync before any broadcast (unseeded) must not reconcile. The unbuffered send
	// synchronizes with the loop's receive, so the loop has processed it once this returns.
	resync <- time.Time{}
	select {
	case <-calls:
		t.Fatal("resync before seeding must not reconcile")
	case <-time.After(50 * time.Millisecond):
	}

	// A broadcast seeds the live set and reconciles with it.
	notifier <- []string{"pod-a"}
	assert.Equal(t, []string{"pod-a"}, <-calls)

	// A resync now reconciles against the last seeded set.
	resync <- time.Time{}
	assert.Equal(t, []string{"pod-a"}, <-calls)

	// A newer broadcast updates the seeded set for later resyncs.
	notifier <- []string{}
	assert.Empty(t, <-calls)
	resync <- time.Time{}
	assert.Empty(t, <-calls)

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
