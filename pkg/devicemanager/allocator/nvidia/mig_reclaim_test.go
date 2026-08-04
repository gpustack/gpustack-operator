package nvidia

import (
	"errors"
	"path/filepath"
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
	redirectLogicalSliceDirs(t)
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
	redirectLogicalSliceDirs(t)
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
	redirectLogicalSliceDirs(t)
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
	redirectLogicalSliceDirs(t)
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
	redirectLogicalSliceDirs(t)
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
	redirectLogicalSliceDirs(t)
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
	redirectLogicalSliceDirs(t)
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

// migSeed is one live GPU instance to seed on a card, as the orphan sweep sees it (no parseable
// marker owns it).
type migSeed struct {
	card string
	giID uint32
}

// seedInstances seeds each migSeed as a live instance at a distinct slot on its card.
func seedInstances(drv *fakeMigDriver, seeds []migSeed) {
	for i, s := range seeds {
		drv.seedLive(s.card, migInstance{
			GiID: s.giID, CiID: s.giID, ComputeSlices: 1,
			Placement: migPlacement{Start: int32(i) * 2, Length: 2}, UUID: "MIG-" + s.card,
		})
	}
}

// TestReclaim_CorruptMarkerHoldsCardClosed asserts a card an unparseable marker names is never
// treated as drained: its instance would otherwise look exactly like a marker-less orphan and be
// destroyed under a running container. The hold is per card (a sibling card's orphan is still
// collected, so one bad file cannot deny the node's capacity), and a corrupt marker whose Pod is
// alive is kept rather than retired. A corrupt path that names no Pod is held indefinitely: with no
// owner there is no liveness evidence to retire it on.
func TestReclaim_CorruptMarkerHoldsCardClosed(t *testing.T) {
	cases := []struct {
		name string
		// corruptPod owns the corrupt marker; strayPath writes it one level above a container dir,
		// where its path names no Pod.
		corruptPod  string
		corruptFile string
		strayPath   bool
		live        []string
		seeds       []migSeed
		// wantDestroyed lists the GPU-instance ids reclaimed after the passes below.
		wantDestroyed []uint32
	}{
		{
			name:        "a live pod's corrupt marker keeps its instance off the orphan sweep",
			corruptPod:  "pod-live",
			corruptFile: markerFileName(testGPUUUID0),
			live:        []string{"pod-live"},
			seeds:       []migSeed{{card: testGPUUUID0, giID: 1}},
		},
		{
			name:        "a corrupt marker naming no card holds every card",
			corruptPod:  "pod-live",
			corruptFile: markerFileName(""),
			live:        []string{"pod-live"},
			seeds:       []migSeed{{card: testGPUUUID0, giID: 1}, {card: testGPUUUID1, giID: 2}},
		},
		{
			name:          "a sibling card's orphan is still collected",
			corruptPod:    "pod-live",
			corruptFile:   markerFileName(testGPUUUID1),
			live:          []string{"pod-live"},
			seeds:         []migSeed{{card: testGPUUUID0, giID: 1}, {card: testGPUUUID1, giID: 2}},
			wantDestroyed: []uint32{1},
		},
		{
			name:        "a corrupt path naming no pod is held indefinitely",
			corruptPod:  "pod-dead",
			corruptFile: markerFileName(testGPUUUID0),
			strayPath:   true,
			seeds:       []migSeed{{card: testGPUUUID0, giID: 1}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			seedInstances(drv, c.seeds)
			dir := deviceplugin.PodWorkDir(c.corruptPod, "c")
			if c.strayPath {
				dir = filepath.Join(deviceplugin.OperatorPodsDir, c.corruptPod)
			}
			path := writeCorruptMarker(t, dir, c.corruptFile)
			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

			for i := 0; i < reclaimMaxMisses*2+2; i++ {
				r.reconcile(c.live)
			}

			destroyed := make([]uint32, 0, len(drv.destroyed))
			for _, inst := range drv.destroyed {
				destroyed = append(destroyed, inst.GiID)
			}
			assert.ElementsMatch(t, c.wantDestroyed, destroyed)
			assert.FileExists(t, path, "the corrupt marker is kept while it can still stand for an owner")
		})
	}
}

// TestReclaim_UnattributableCorruptPathBoundedLog asserts the one hold this loop cannot release is not
// silent: a corrupt path naming neither a Pod nor a card keeps failing closed node-wide forever (there
// is no liveness evidence to retire it on), and the loop surfaces the operator-visible log naming the
// path exactly once, at the bound — the same surface the IN_USE path uses, because a status condition
// would be stomped by the wholesale Devices.Status rebuild.
func TestReclaim_UnattributableCorruptPathBoundedLog(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	seedInstances(drv, []migSeed{{card: testGPUUUID0, giID: 1}})
	// A marker-named file one level above a container dir: its path names no Pod, and its own name is
	// not attributable to a card either.
	path := writeCorruptMarker(t, filepath.Join(deviceplugin.OperatorPodsDir, "pod-dead"), markerFileName(""))

	var bounded int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "will not clear by itself") {
			bounded++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)

	for i := 0; i < reclaimMaxCorruptHoldMisses+3; i++ {
		r.reconcile(nil)
	}

	assert.FileExists(t, path, "an unattributable path is kept: there is no evidence to retire it on")
	assert.Empty(t, drv.destroyed, "and it keeps every card off the orphan sweep while it persists")
	assert.Equal(t, 1, bounded, "the operator-visible log fires exactly once at the bound")
}

// TestReclaim_LiveOwnersCorruptMarkerBoundedLog asserts the sibling hold is not silent either. An
// unparseable record whose Pod is still running is kept — the Pod depends on the ownership it records —
// and nothing in this loop can release it while that Pod lives, which is the case that reads as
// transient and is not. So it earns the same surface: one operator-visible log naming the card, the Pod
// and the path, at the bound and only there. What the record does is unchanged, before the bound and
// after it.
func TestReclaim_LiveOwnersCorruptMarkerBoundedLog(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	seedInstances(drv, []migSeed{{card: testGPUUUID0, giID: 1}})
	path := writeCorruptMarker(t, deviceplugin.PodWorkDir("pod-live", "c"), markerFileName(testGPUUUID0))

	var bounded int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "delete the pod to release the card") {
			bounded++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)
	live := []string{"pod-live"}

	for i := 0; i < reclaimMaxCorruptHoldMisses-1; i++ {
		r.reconcile(live)
	}
	assert.Zero(t, bounded, "the operator-visible log does not fire before the bound")
	assert.FileExists(t, path, "and the record is kept meanwhile")

	r.reconcile(live)
	assert.Equal(t, 1, bounded, "it fires at the bound")

	for i := 0; i < 3; i++ {
		r.reconcile(live)
	}
	assert.Equal(t, 1, bounded, "and exactly once, however long the hold stands")
	assert.FileExists(t, path, "the record is still kept: its live pod depends on the ownership it records")
	assert.Empty(t, drv.destroyed, "and its card stays off the orphan sweep throughout")
}

// TestReclaim_CorruptMarkerOfDeadPodConverges asserts the hold clears by itself instead of leaking
// a partition for the node's lifetime: a corrupt marker whose Pod is gone is retired on that
// evidence alone (its path names the Pod) after the same debounce every other decision here uses,
// and the partition it shadowed then becomes a genuine orphan the collector takes once the card's
// own debounce elapses. The retirement is observed by the next pass, never by the one that removed
// the file.
func TestReclaim_CorruptMarkerOfDeadPodConverges(t *testing.T) {
	cases := []struct {
		name        string
		corruptFile string
	}{
		{name: "corrupt marker naming its card", corruptFile: markerFileName(testGPUUUID0)},
		{name: "corrupt marker naming no card", corruptFile: markerFileName("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			seedInstances(drv, []migSeed{{card: testGPUUUID0, giID: 1}})
			path := writeCorruptMarker(t, deviceplugin.PodWorkDir("pod-dead", "c"), c.corruptFile)
			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

			for i := 0; i < reclaimMaxMisses-1; i++ {
				r.reconcile(nil)
			}
			assert.FileExists(t, path, "the corrupt marker is not retired before the debounce")

			r.reconcile(nil)
			assert.NoFileExists(t, path, "the corrupt marker of a dead pod is retired after the debounce")
			assert.Empty(t, drv.destroyed, "the pass that removed the file still holds the card closed")

			// With the record gone, the shadowed partition is a plain orphan on a drained card.
			for i := 0; i < reclaimMaxMisses-1; i++ {
				r.reconcile(nil)
			}
			assert.Empty(t, drv.destroyed, "the card restarts its own drained debounce from scratch")

			r.reconcile(nil)
			require.Len(t, drv.destroyed, 1, "the shadowed partition is collected once the card drains")
			assert.Equal(t, uint32(1), drv.destroyed[0].GiID)
		})
	}
}

// TestReclaim_OrphanKeptWhileCardHasMarker asserts the drained-card guard: a marker-less orphan is
// not GC'd while the card still carries any marker (here a dead-but-in-use pod whose marker cannot
// be removed), because such a card is not fully drained.
func TestReclaim_OrphanKeptWhileCardHasMarker(t *testing.T) {
	redirectLogicalSliceDirs(t)
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
		redirectLogicalSliceDirs(t)
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
		redirectLogicalSliceDirs(t)
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

// The identity check has to read the card INSIDE its lock, not from the snapshot the pass opened
// with. That snapshot can be a whole allocation old by the time a given card is reached, and an
// out-of-band `nvidia-smi mig -dgi` plus NVML's id reuse can put a different — possibly live —
// instance at the recorded id in exactly that window. Checked against the stale view, the marker
// still "matches" and a running Pod's MIG device is destroyed under it.
func TestReclaim_IDReusedAfterThePassSnapshotIsNotDestroyed(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)

	// The reclaimMaxMisses-th pass is the one that destroys. Its own snapshot is enumeration number
	// reclaimMaxMisses, and the re-read under the card lock is the one after it — so replacing the
	// instance there lands strictly between the two.
	drv.listHook = func(d *fakeMigDriver, call int) {
		if call != reclaimMaxMisses+1 {
			return
		}
		d.live[testGPUUUID0] = []migInstance{{
			GiID: 1, CiID: 1, ComputeSlices: 1,
			Placement: migPlacement{Start: 0, Length: 2}, UUID: "MIG-reused",
		}}
	}

	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)
	for i := 0; i < reclaimMaxMisses; i++ {
		r.reconcile(nil)
	}

	assert.Empty(t, drv.destroyed,
		"the id now carries somebody else's instance, so nothing may be destroyed under this marker")
	_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.Error(t, err, "the stale marker is dropped, since it describes an instance that is gone")
}

// A re-read that fails is a per-card skip rather than a destroy on an unvalidated view: the marker
// stays, the debounce is not cleared, and the next pass tries again.
func TestReclaim_FailsClosedWhenTheLockedRereadFails(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)

	// Fail only the re-read under the lock, so the pass gets as far as deciding to destroy.
	drv.listHook = func(d *fakeMigDriver, call int) {
		d.listErr = nil
		if call == reclaimMaxMisses+1 {
			d.listErr = errors.New("nvml enumeration failed")
		}
	}

	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)
	for i := 0; i < reclaimMaxMisses; i++ {
		r.reconcile(nil)
	}

	assert.Empty(t, drv.destroyed, "an unreadable card is never destroyed on")
	_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.NoError(t, err, "the marker is kept, so the next pass can retry")
}
