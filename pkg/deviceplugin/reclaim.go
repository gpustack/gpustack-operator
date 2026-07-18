package deviceplugin

import (
	"context"
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// reclaimResyncInterval is the period at which a per-vendor sliced reclaim loop
// re-runs against the last known live pod set, independent of the reconciler's
// broadcast. The broadcast channel is lossy (buffered, non-blocking send drops a tick
// when full), so the ticker guarantees reclamation eventually runs regardless of
// dropped ticks.
const reclaimResyncInterval = 60 * time.Second

// RunSlicedReclaimLoop drives a per-vendor stateful reclaim function from the
// DevicesReconciler's broadcast livePodUIDs AND a periodic resync ticker. Stateful
// vendors (MetaX sysfs, Cambricon cnDev) create driver/kernel subdevices that a pod's
// disappearance cannot free on its own — the device-plugin responder has no Release
// callback — so their subdevices are reclaimed level-based: each tick hands the
// current live pod-UID set to reconcile, which destroys the subdevices whose pod is
// gone. It blocks until ctx is canceled.
func RunSlicedReclaimLoop(ctx context.Context, reconciler *DevicesReconciler, manufacturer string, reconcile func(livePodUIDs []string)) {
	notifier := reconciler.getReconcileNotifier(manufacturer, workercore.DeviceAllocationModeSliced)
	ticker := time.NewTicker(reclaimResyncInterval)
	defer ticker.Stop()
	reclaimLoop(ctx, reconcile, notifier, ticker.C)
}

// reclaimLoop is the pure select loop behind RunSlicedReclaimLoop, split out so it is
// unit-testable with injected tick sources. The resync ticker reconciles against the
// last live set the broadcast delivered, and does nothing until the first broadcast
// has seeded that set — so a silent startup (ticker fires before any broadcast) never
// reclaims a live slice against an empty set.
func reclaimLoop(ctx context.Context, reconcile func([]string), notifier <-chan []string, resync <-chan time.Time) {
	var lastLive []string
	seeded := false
	for {
		select {
		case <-ctx.Done():
			return
		case live := <-notifier:
			lastLive = live
			seeded = true
			reconcile(live)
		case <-resync:
			if seeded {
				reconcile(lastLive)
			}
		}
	}
}
