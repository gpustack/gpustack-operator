package nvidia

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// noClaims is the empty attribution self-check source: no running Pod claims any placement.
func noClaims() (map[string][]migPlacement, error) { return nil, nil }

// seedMarkedInstance seeds a live instance on a card and writes its ownership marker, so the
// pair models one Pod's created-and-recorded MIG partition.
func seedMarkedInstance(t *testing.T, drv *fakeMigDriver, podUID, card string, giID uint32) {
	t.Helper()
	inst := migInstance{GiID: giID, CiID: giID, ComputeSlices: 1, Placement: migPlacement{Start: 0, Length: 2}, UUID: "MIG-" + card}
	drv.seedLive(card, inst)
	require.NoError(t, writeMarker(markerPath(podUID, "c", card), migMarker{
		PodUID: podUID, Container: "c", Card: card, Profile: "1g.10gb",
		GiID: giID, CiID: giID, MigUUID: inst.UUID, ComputeSlices: 1, Start: 0, Length: 2,
	}))
}

func TestReclaim_DestroysDeadPodAfterDebounce(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

	// pod-dead is absent from the live set, but survives the passes before the debounce.
	for i := 0; i < reclaimMaxMisses-1; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "not destroyed before the debounce")
	_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.NoError(t, err, "marker still present before the debounce")

	// The reclaimMaxMisses-th pass destroys the instance and removes the marker.
	r.reconcile(nil)
	require.Len(t, drv.destroyed, 1)
	assert.Equal(t, uint32(1), drv.destroyed[0].GiID)
	_, err = parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.Error(t, err, "marker removed after reclaim")
}

func TestReclaim_KeepsLivePod(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	seedMarkedInstance(t, drv, "pod-live", testGPUUUID0, 1)
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

	for i := 0; i < reclaimMaxMisses+2; i++ {
		r.reconcile([]string{"pod-live"})
	}
	assert.Empty(t, drv.destroyed, "a live pod's instance is never reclaimed")
	_, err := parseMarker(markerPath("pod-live", "c", testGPUUUID0))
	require.NoError(t, err)
}

// TestReclaim_InUseBoundedRetryAndCondition asserts a residual NVML_ERROR_IN_USE never destroys
// the instance, keeps retrying every pass (the debounce is not cleared), surfaces the
// operator-visible condition exactly once at the bound, and finally reclaims once the process exits.
func TestReclaim_InUseBoundedRetryAndCondition(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	drv.inUseGiIDs = map[uint32]bool{1: true}
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)

	var conditions int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "still in use after bounded destroy retries") {
			conditions++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)

	for i := 0; i < reclaimMaxMisses+reclaimMaxDestroyMisses+2; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "an in-use instance is never destroyed")
	_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.NoError(t, err, "the marker is retained while the instance is in use")
	assert.Equal(t, 1, conditions, "the operator-visible condition fires exactly once at the bound")

	// Once the residual process exits, the next pass reclaims it.
	drv.inUseGiIDs = nil
	r.reconcile(nil)
	require.Len(t, drv.destroyed, 1)
	_, err = parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.Error(t, err)
}

// TestReclaim_MisAttributedMarkerNotDestroyed asserts the attribution self-check: a dead pod's
// marker whose placement a running Pod still claims (the oldest-Pending getAllocatingPod
// heuristic mis-bound the marker) never destroys the running Pod's instance.
func TestReclaim_MisAttributedMarkerNotDestroyed(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)
	claims := func() (map[string][]migPlacement, error) {
		return map[string][]migPlacement{testGPUUUID0: {{Start: 0, Length: 2}}}, nil
	}
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), claims)

	for i := 0; i < reclaimMaxMisses+3; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "a placement a running pod claims is never destroyed")
	_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.NoError(t, err, "the mis-attributed marker is retained")
}

// TestReclaim_FailClosedOnClaimsError asserts a liveClaims read error skips the whole pass — the
// self-check cannot run, so no destroy is risked.
func TestReclaim_FailClosedOnClaimsError(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)
	claims := func() (map[string][]migPlacement, error) {
		return nil, assert.AnError
	}
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), claims)

	for i := 0; i < reclaimMaxMisses+3; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "a claims read error fails closed (no destroy)")
}

// TestReclaim_FailClosedOnListError asserts a ListInstances error skips the whole pass — without
// the live-state view the marker identity check cannot run, so no destroy is risked.
func TestReclaim_FailClosedOnListError(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	drv.listErr = assert.AnError
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

	for i := 0; i < reclaimMaxMisses+3; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "a ListInstances error fails closed (no destroy)")
}

// TestReclaim_StaleMarkerGiIdReuseNotDestroyed asserts the identity check: a dead pod's stale
// marker whose GI id NVML reused for a different (live) instance at a non-overlapping slot — which
// the placement-based attribution check does not catch — is dropped without destroying the live
// instance, because the marker's recorded MIG-device UUID no longer matches the live one.
func TestReclaim_StaleMarkerGiIdReuseNotDestroyed(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	// GI 1 is now a live pod's instance at slot 4 (UUID MIG-new), recorded by its own marker.
	drv.seedLive(testGPUUUID0, migInstance{GiID: 1, CiID: 1, ComputeSlices: 1, Placement: migPlacement{4, 2}, UUID: "MIG-new"})
	require.NoError(t, writeMarker(markerPath("pod-new", "c", testGPUUUID0), migMarker{
		PodUID: "pod-new", Container: "c", Card: testGPUUUID0, Profile: "1g.10gb",
		GiID: 1, CiID: 1, MigUUID: "MIG-new", ComputeSlices: 1, Start: 4, Length: 2,
	}))
	// A dead pod's stale marker recorded GI 1 with the OLD UUID at a different, non-overlapping slot.
	require.NoError(t, writeMarker(markerPath("pod-dead", "c", testGPUUUID0), migMarker{
		PodUID: "pod-dead", Container: "c", Card: testGPUUUID0, Profile: "1g.10gb",
		GiID: 1, CiID: 1, MigUUID: "MIG-old", ComputeSlices: 1, Start: 0, Length: 2,
	}))
	claims := func() (map[string][]migPlacement, error) {
		return map[string][]migPlacement{testGPUUUID0: {{Start: 4, Length: 2}}}, nil
	}
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), claims)

	for i := 0; i < reclaimMaxMisses+1; i++ {
		r.reconcile([]string{"pod-new"})
	}
	assert.Empty(t, drv.destroyed, "a reused GI id (UUID mismatch) never destroys the live instance")
	_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.Error(t, err, "the stale marker is dropped")
	_, err = parseMarker(markerPath("pod-new", "c", testGPUUUID0))
	require.NoError(t, err, "the live pod's marker is intact")
}

// TestReclaim_OrphanKeptWhileCardHasMarker asserts the drained-card guard: a marker-less orphan is
// not GC'd while the card still carries any marker (here a dead-but-in-use pod whose marker cannot
// be removed), because such a card is not fully drained.
func TestReclaim_OrphanKeptWhileCardHasMarker(t *testing.T) {
	redirectSoftSliceDirs(t)
	drv := newFakeMigDriver()
	drv.inUseGiIDs = map[uint32]bool{1: true} // the marked pod's GI 1 is wedged in-use
	seedMarkedInstance(t, drv, "pod-stuck", testGPUUUID0, 1)
	// A marker-less orphan shares the card.
	drv.seedLive(testGPUUUID0, migInstance{GiID: 2, CiID: 2, ComputeSlices: 1, Placement: migPlacement{2, 2}, UUID: "MIG-orphan"})
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

	for i := 0; i < reclaimMaxMisses+2; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "an orphan is held while the card still carries any marker (not fully drained)")
}

// TestReclaim_OrphanGCOnlyOnDrainedCard asserts a marker-less GPU instance is kept while any live
// Pod is on its card and reclaimed only once the card fully drains past the debounce.
func TestReclaim_OrphanGCOnlyOnDrainedCard(t *testing.T) {
	t.Run("kept while a live pod is on the card", func(t *testing.T) {
		redirectSoftSliceDirs(t)
		drv := newFakeMigDriver()
		seedMarkedInstance(t, drv, "pod-live", testGPUUUID0, 1)
		// A marker-less orphan (a crash between GI-create and marker-write) shares the card.
		drv.seedLive(testGPUUUID0, migInstance{GiID: 2, CiID: 2, ComputeSlices: 1, Placement: migPlacement{2, 2}, UUID: "MIG-orphan"})
		r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

		for i := 0; i < reclaimMaxMisses+3; i++ {
			r.reconcile([]string{"pod-live"})
		}
		assert.Empty(t, drv.destroyed, "an orphan on a card hosting a live pod is kept")
	})

	t.Run("gc'd after the card drains and the debounce", func(t *testing.T) {
		redirectSoftSliceDirs(t)
		drv := newFakeMigDriver()
		// A single marker-less orphan on an otherwise-empty card.
		drv.seedLive(testGPUUUID0, migInstance{GiID: 2, CiID: 2, ComputeSlices: 1, Placement: migPlacement{2, 2}, UUID: "MIG-orphan"})
		r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

		for i := 0; i < reclaimMaxMisses-1; i++ {
			r.reconcile(nil)
		}
		assert.Empty(t, drv.destroyed, "not gc'd before the drained-card debounce")

		r.reconcile(nil)
		require.Len(t, drv.destroyed, 1, "gc'd once the card is drained past the debounce")
		assert.Equal(t, uint32(2), drv.destroyed[0].GiID)
	})
}
