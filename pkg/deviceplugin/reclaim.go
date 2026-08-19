package deviceplugin

import (
	"context"
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// reclaimResyncInterval is the period at which a per-vendor reclaim loop re-runs
// against the last known live pod set, independent of the reconciler's
// broadcast. The broadcast channel is lossy (buffered, non-blocking send drops a tick
// when full), so the ticker guarantees reclamation eventually runs regardless of
// dropped ticks.
const reclaimResyncInterval = 60 * time.Second

// RunReclaimLoop drives a per-vendor stateful reclaim function from the DevicesReconciler's
// broadcast livePodUIDs AND a periodic resync ticker. Both stateful logical slicing (MetaX
// sysfs, Cambricon cnDev) and hardware partitioning (NVIDIA MIG) create driver-level devices
// that a pod's disappearance cannot free on its own — the device-plugin responder has no
// Release callback — so they are reclaimed level-based: each tick hands the current live
// pod-UID set to reconcile, which destroys the devices whose pod is gone. mode identifies the
// subscribing family to the reconciler; every notifier receives the same broadcast. It blocks
// until ctx is canceled.
func RunReclaimLoop(
	ctx context.Context,
	reconciler *DevicesReconciler,
	manufacturer string,
	mode workercore.DeviceAllocationMode,
	reconcile func(livePodUIDs []string),
) {
	notifier, release := reconciler.getReconcileNotifier(manufacturer, mode)
	defer release()
	ticker := time.NewTicker(reclaimResyncInterval)
	defer ticker.Stop()
	reclaimLoop(ctx, reconcile, notifier, ticker.C)
}

// reclaimLoop is the pure select loop behind RunReclaimLoop, split out so it is
// unit-testable with injected tick sources. The resync ticker reconciles against the
// last live set the broadcast delivered, and does nothing until the first broadcast
// has seeded that set — so a silent startup (ticker fires before any broadcast) never
// reclaims a live instance against an empty set.
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
