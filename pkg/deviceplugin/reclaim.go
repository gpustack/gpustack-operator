package deviceplugin

import (
	"context"
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// reclaimResyncInterval is the period at which a per-vendor reclaim loop re-runs the reclaim
// against the newest live pod-UID set, independent of the reconciler's broadcast. It is a safety
// net for a reconcile that could not complete (a failed pod list, for instance) and for a
// consumer too slow to act on a broadcast before its wake was consumed. It is not what makes a
// reclaim happen after a broadcast: the broadcast itself is lossless, since every wake reads the
// newest set and a dropped wake leaves the pending one to wake the loop. So no tick can be lost,
// and this interval bounds only how long a reclaim that failed once waits to be retried.
const reclaimResyncInterval = 60 * time.Second

// RunReclaimLoop drives a per-vendor stateful reclaim function from the DevicesReconciler's
// broadcast livePodUIDs AND a periodic resync ticker. Both stateful logical slicing (MetaX
// sysfs, Cambricon cnDev) and hardware partitioning (NVIDIA MIG) create driver-level devices
// that a pod's disappearance cannot free on its own — the device-plugin responder has no
// Release callback — so they are reclaimed level-based: each wake hands the newest live pod-UID
// set to reconcile, which destroys the devices whose pod is gone. mode identifies the
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
	reclaimLoop(ctx, reconcile, reconciler.livePodUIDs, notifier, ticker.C)
}

// reclaimLoop is the pure select loop behind RunReclaimLoop, split out so it is
// unit-testable with injected set sources. latest returns the newest live pod-UID set and
// whether one has ever been stored; wake signals that the set has moved; resync is the periodic
// re-run.
//
// The contract:
//   - A tick before any set has been stored never reconciles: there is nothing to reconcile
//     against, and reclaiming against an empty set would destroy a live instance.
//   - A wake reconciles with the newest stored set, whatever the wake was sent for.
//   - Two stores before the loop runs, then one wake, reconcile once with the newer set and
//     never with the older: the loop reads the set at the moment it runs, not when woken.
//   - A dropped wake is harmless. A wake is dropped only because one is already pending, and
//     the pending one still wakes the loop to read the newest set.
func reclaimLoop(
	ctx context.Context,
	reconcile func([]string),
	latest func() ([]string, bool),
	wake <-chan struct{},
	resync <-chan time.Time,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if live, ok := latest(); ok {
				reconcile(live)
			}
		case <-resync:
			if live, ok := latest(); ok {
				reconcile(live)
			}
		}
	}
}
