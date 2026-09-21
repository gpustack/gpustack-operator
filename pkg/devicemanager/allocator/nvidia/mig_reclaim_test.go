package nvidia

import (
	"errors"
	"fmt"
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

// seedMarkedInstance seeds a live instance on an accelerator and writes its ownership marker, so the
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
		if strings.Contains(args, "not reclaimable after bounded retries") {
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

// migSeed is one live GPU instance to seed on an accelerator, as the orphan sweep sees it (no parseable
// marker owns it).
type migSeed struct {
	card string
	giID uint32
}

// seedInstances seeds each migSeed as a live instance at a distinct slot on its accelerator.
func seedInstances(drv *fakeMigDriver, seeds []migSeed) {
	for i, s := range seeds {
		drv.seedLive(s.card, migInstance{
			GiID: s.giID, CiID: s.giID, ComputeSlices: 1,
			Placement: migPlacement{Start: int32(i) * 2, Length: 2}, UUID: "MIG-" + s.card,
		})
	}
}

// TestReclaim_PartitionRunningAProcessIsKept asserts the process check that precedes every destroy
// this loop makes. A partition carrying a compute process is left alone in both directions — a dead
// Pod's recorded one, and a marker-less orphan on an otherwise drained accelerator, which is the case
// that would otherwise take a partition carved outside this operator out from under its user. It is
// left alone in silence: nothing is wrong, so nothing is reported as an error.
//
// A failure to ask splits in two, and the two lead opposite ways. A partition NO MIG DEVICE
// ADDRESSES has nothing there to ask — a GPU instance carrying no compute instance, or every
// partition as a container the driver's MIG devices are hidden from sees them — and the destroy
// proceeds. A partition the driver WOULD NOT ANSWER ABOUT is kept: beginning a teardown on a
// partition whose state is unknown is what sweeps an idle compute instance out of a busy one.
func TestReclaim_PartitionRunningAProcessIsKept(t *testing.T) {
	cases := []struct {
		name string
		// marked owns the partition with a dead Pod's ownership marker; otherwise the sweep finds it
		// as a marker-less orphan.
		marked        bool
		processes     int
		processesErr  error
		wantDestroyed bool
	}{
		{name: "a dead pod's partition running a process is kept", marked: true, processes: 1},
		{name: "a dead pod's idle partition is reclaimed", marked: true, wantDestroyed: true},
		{name: "an orphan running a process is kept on a drained card", processes: 2},
		{name: "an idle orphan is reclaimed on a drained card", wantDestroyed: true},
		{
			name:   "a dead pod's partition no mig device addresses is left to the driver",
			marked: true, processesErr: fmt.Errorf("probe: %w", errNoAddressableDevice), wantDestroyed: true,
		},
		{
			name:         "an orphan no mig device addresses is left to the driver",
			processesErr: fmt.Errorf("probe: %w", errNoAddressableDevice), wantDestroyed: true,
		},
		{
			name:   "a dead pod's partition the driver would not answer about is kept",
			marked: true, processesErr: assert.AnError,
		},
		{
			name:         "an orphan the driver would not answer about is kept",
			processesErr: assert.AnError,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			drv.processesByGiID = map[uint32]int{1: c.processes}
			drv.processesErr = c.processesErr
			if c.marked {
				seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)
			} else {
				seedInstances(drv, []migSeed{{card: testGPUUUID0, giID: 1}})
			}

			var errorRecords int
			logger := funcr.New(func(_, args string) {
				if strings.Contains(args, `"error"=`) {
					errorRecords++
				}
			}, funcr.Options{})
			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)

			for i := 0; i < reclaimMaxMisses+3; i++ {
				r.reconcile(nil)
			}

			if c.wantDestroyed {
				require.Len(t, drv.destroyed, 1)
				assert.Equal(t, uint32(1), drv.destroyed[0].GiID)
			} else {
				assert.Empty(t, drv.destroyed, "a partition in use is never destroyed")
			}
			if c.marked {
				_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
				if c.wantDestroyed {
					require.Error(t, err, "the marker goes with the partition")
				} else {
					require.NoError(t, err, "the marker is kept, so a later pass finds the partition again")
				}
			}
			assert.Zero(t, errorRecords, "a partition in use is not a failure to report")
		})
	}
}

// TestReclaim_ProcessHeldPartitionReachesTheBound asserts the process pre-check does not cost the
// operator the one surface this loop has. Skipping the destroy means the driver never refuses it, so
// nothing would increment the in-use counter and the bounded log could never fire for the very
// condition it was written for — a partition a leaked process holds forever, occupying its placement.
// The skip therefore counts as in-use: the log fires once at the bound, and only once, exactly as a
// refused destroy would have made it.
func TestReclaim_ProcessHeldPartitionReachesTheBound(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.processesByGiID = map[uint32]int{1: 1}
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)

	var bounded int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "not reclaimable after bounded retries") {
			bounded++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)

	for i := 0; i < reclaimMaxMisses+reclaimMaxDestroyMisses+2; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "the destroy is never attempted while a process holds it")
	_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.NoError(t, err, "the marker is retained")
	assert.Equal(t, 1, bounded, "the operator-visible log fires exactly once at the bound")

	// Once the process exits the next pass reclaims it, which is what returns the placement.
	drv.processesByGiID = nil
	r.reconcile(nil)
	require.Len(t, drv.destroyed, 1)
	_, err = parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
	require.Error(t, err)
}

// TestReclaim_ProcessHeldOrphanReachesTheBound is the same assertion for the other destroy this loop
// makes. The two routes count in-use passes on keys of their own — a dead Pod's UID, and the
// accelerator whose orphans are being swept — and each raises its own log, so one route counting
// while the other does not would leave a held partition off the only surface an operator has. This
// is the route that carries a partition carved outside this operator: it never had a marker to be
// found by, so it is only ever reached as an orphan.
func TestReclaim_ProcessHeldOrphanReachesTheBound(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	seedInstances(drv, []migSeed{{card: testGPUUUID0, giID: 1}})
	drv.processesByGiID = map[uint32]int{1: 1}

	var bounded int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "marker-less mig instance on a drained card is still not reclaimable") {
			bounded++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)

	for i := 0; i < reclaimMaxMisses+reclaimMaxDestroyMisses+2; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "the destroy is never attempted while a process holds it")
	assert.Equal(t, 1, bounded, "the operator-visible log fires exactly once at the bound")

	// Once the process exits the next pass reclaims it, which is what returns the placement.
	drv.processesByGiID = nil
	r.reconcile(nil)
	require.Len(t, drv.destroyed, 1)
	assert.Equal(t, uint32(1), drv.destroyed[0].GiID)
}

// TestReclaim_BoundNamesWhatBlockedTheReclaim asserts the one log an operator actually sees names the
// right fault. Both a process running on a partition and a partition that cannot be asked about its
// processes block the destroy and count toward the same bound, but they call for opposite things —
// find the process, or give the container access to the driver — and the per-pass lines that would
// tell them apart are below the shipped verbosity. Reporting a query failure as errInstanceInUse
// would send an operator hunting a process that does not exist.
func TestReclaim_BoundNamesWhatBlockedTheReclaim(t *testing.T) {
	cases := []struct {
		name string
		// processes is how many the partition answers with; processesErr makes the query itself fail.
		processes    int
		processesErr error
		// wantNamed must appear in the bounded log, wantNotNamed must not.
		wantNamed    string
		wantNotNamed string
	}{
		{
			name:      "a process holding it is reported as in use",
			processes: 1, wantNamed: errInstanceInUse.Error(),
		},
		{
			name:         "a query failure is reported as itself, not as a process",
			processesErr: assert.AnError,
			wantNamed:    assert.AnError.Error(), wantNotNamed: errInstanceInUse.Error(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			drv.processesByGiID = map[uint32]int{1: c.processes}
			drv.processesErr = c.processesErr
			seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)

			var bounded []string
			logger := funcr.New(func(_, args string) {
				if strings.Contains(args, "not reclaimable after bounded retries") {
					bounded = append(bounded, args)
				}
			}, funcr.Options{})
			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)

			for i := 0; i < reclaimMaxMisses+reclaimMaxDestroyMisses+2; i++ {
				r.reconcile(nil)
			}
			require.Len(t, bounded, 1, "the operator-visible log fires exactly once at the bound")
			assert.Contains(t, bounded[0], c.wantNamed, "the log names what actually blocked the reclaim")
			if c.wantNotNamed != "" {
				assert.NotContains(t, bounded[0], c.wantNotNamed, "and does not name a cause it cannot know")
			}
			assert.Empty(t, drv.destroyed, "the destroy is never attempted while the partition is blocked")
		})
	}
}

// TestReclaim_CorruptMarkerHoldsCardClosed asserts an accelerator an unparseable marker names is
// never treated as drained: its instance would otherwise look exactly like a marker-less orphan and
// be destroyed under a running container. The hold is per accelerator (a sibling's orphan is still
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
			name:        "a corrupt marker naming no accelerator holds every accelerator",
			corruptPod:  "pod-live",
			corruptFile: markerFileName(""),
			live:        []string{"pod-live"},
			seeds:       []migSeed{{card: testGPUUUID0, giID: 1}, {card: testGPUUUID1, giID: 2}},
		},
		{
			name:          "a sibling accelerator's orphan is still collected",
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

// TestReclaim_UnattributableCorruptPathBoundedLog asserts the one hold this loop cannot release is
// not silent: a corrupt path naming neither a Pod nor an accelerator keeps failing closed node-wide
// forever (there is no liveness evidence to retire it on), and the loop surfaces the log naming the
// path exactly once, at the bound — the same surface the IN_USE path uses, because a status condition
// would be stomped by the wholesale Devices.Status rebuild.
func TestReclaim_UnattributableCorruptPathBoundedLog(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	seedInstances(drv, []migSeed{{card: testGPUUUID0, giID: 1}})
	// A marker-named file one level above a container dir: its path names no Pod, and its own name is
	// not attributable to an accelerator either.
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
// unparseable record whose Pod is still running is kept — the Pod depends on the ownership it
// records — and nothing in this loop can release it while that Pod lives, which is the case that
// reads as transient and is not. So it earns the same surface: one operator-visible log naming the
// accelerator, the Pod and the path, at the bound and only there. What the record does is unchanged,
// before the bound and
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
// and the partition it shadowed then becomes a genuine orphan the collector takes once the accelerator's
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

			// With the record gone, the shadowed partition is a plain orphan on a drained accelerator.
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

// TestReclaim_OrphanKeptWhileCardHasMarker asserts the drained-accelerator guard: a marker-less
// orphan is not GC'd while the accelerator still carries any marker (here a dead-but-in-use pod
// whose marker cannot be removed), because such an accelerator is not fully drained.
func TestReclaim_OrphanKeptWhileCardHasMarker(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.inUseGiIDs = map[uint32]bool{1: true} // the marked pod's GI 1 is wedged in-use
	seedMarkedInstance(t, drv, "pod-stuck", testGPUUUID0, 1)
	// A marker-less orphan shares the accelerator.
	drv.seedLive(testGPUUUID0, migInstance{GiID: 2, CiID: 2, ComputeSlices: 1, Placement: migPlacement{2, 2}, UUID: "MIG-orphan"})
	r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

	for i := 0; i < reclaimMaxMisses+2; i++ {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "an orphan is held while the card still carries any marker (not fully drained)")
}

// TestReclaim_OrphanGCOnlyOnDrainedCard asserts a marker-less GPU instance is kept while any live
// Pod is on its accelerator and reclaimed only once the accelerator fully drains past the debounce.
func TestReclaim_OrphanGCOnlyOnDrainedCard(t *testing.T) {
	t.Run("kept while a live pod is on the accelerator", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		seedMarkedInstance(t, drv, "pod-live", testGPUUUID0, 1)
		// A marker-less orphan (a crash between GI-create and marker-write) shares the accelerator.
		drv.seedLive(testGPUUUID0, migInstance{GiID: 2, CiID: 2, ComputeSlices: 1, Placement: migPlacement{2, 2}, UUID: "MIG-orphan"})
		r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)

		for i := 0; i < reclaimMaxMisses+3; i++ {
			r.reconcile([]string{"pod-live"})
		}
		assert.Empty(t, drv.destroyed, "an orphan on a card hosting a live pod is kept")
	})

	t.Run("gc'd after the accelerator drains and the debounce", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		// A single marker-less orphan on an otherwise-empty accelerator.
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

// The identity check has to read the accelerator INSIDE its lock, not from the snapshot the pass opened
// with. That snapshot can be a whole allocation old by the time a given accelerator is reached, and an
// out-of-band `nvidia-smi mig -dgi` plus NVML's id reuse can put a different — possibly live —
// instance at the recorded id in exactly that window. Checked against the stale view, the marker
// still "matches" and a running Pod's MIG device is destroyed under it.
func TestReclaim_IDReusedAfterThePassSnapshotIsNotDestroyed(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	seedMarkedInstance(t, drv, "pod-dead", testGPUUUID0, 1)

	// The reclaimMaxMisses-th pass is the one that destroys. Its own snapshot is enumeration number
	// reclaimMaxMisses, and the re-read under the accelerator lock is the one after it — so replacing the
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

// A re-read that fails is a per-accelerator skip rather than a destroy on an unvalidated view: the
// marker stays, the debounce is not cleared, and the next pass tries again.
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

// TestReclaim_RecordsTheDestroyedPartitionIdentity pins the other half of the same trail: a
// partition that vanishes between the grant and the container start is either one this loop
// destroyed or one the container engine cannot address, and only a destroy that names the identity
// it tore down can tell those apart. Both sweeps are covered, because they answer different
// questions about the fault -- a dead owner's record versus a partition nothing claimed.
func TestReclaim_RecordsTheDestroyedPartitionIdentity(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, drv *fakeMigDriver) string
	}{
		{
			name: "a dead pod's partition",
			seed: func(t *testing.T, drv *fakeMigDriver) string {
				t.Helper()
				inst := migInstance{
					GiID: 3, CiID: 4, ComputeSlices: 1,
					Placement: migPlacement{Start: 0, Length: 2}, UUID: "MIG-dead-owner",
				}
				drv.seedLive(testGPUUUID0, inst)
				require.NoError(t, writeMarker(markerPath("pod-gone", "c", testGPUUUID0), migMarker{
					PodUID: "pod-gone", Container: "c", Card: testGPUUUID0, Profile: "1g.10gb",
					GiID: inst.GiID, CiID: inst.CiID, MigUUID: inst.UUID,
					ComputeSlices: 1, Start: 0, Length: 2,
				}))
				return inst.UUID
			},
		},
		{
			name: "a marker-less partition on a drained card",
			seed: func(_ *testing.T, drv *fakeMigDriver) string {
				inst := migInstance{
					GiID: 9, CiID: 9, ComputeSlices: 1,
					Placement: migPlacement{Start: 4, Length: 2}, UUID: "MIG-no-owner",
				}
				drv.seedLive(testGPUUUID0, inst)
				return inst.UUID
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			want := c.seed(t, drv)

			var lines []string
			logger := funcr.New(func(prefix, args string) {
				lines = append(lines, prefix+args)
			}, funcr.Options{})
			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logger, noClaims)

			for i := 0; i < reclaimMaxMisses; i++ {
				r.reconcile(nil)
			}
			require.Len(t, drv.destroyed, 1, "the partition is destroyed")

			joined := strings.Join(lines, "\n")
			assert.Contains(t, joined, want, "the destroyed partition identity is recorded")
			assert.Contains(t, joined, testGPUUUID0, "the accelerator it was destroyed on is recorded")
		})
	}
}

// TestReclaim_SameSlotRebuildNotDestroyedUnderStaleMarker covers the one shape the id-reuse guard
// above cannot see. A MIG-device UUID is name-based -- derived from the parent accelerator and the
// instance's own identity -- so a partition destroyed and another created at the SAME placement
// carries the SAME identifier, and the slot pick is deterministic (lowest free first), which makes
// re-taking a just-freed slot the ordinary case rather than a corner. The predecessor's marker then
// matches the successor on every field the identity check has, however often the accelerator is
// re-read under the lock: the successor reproduces the whole identity.
//
// TestReclaim_StaleMarkerGiIdReuseNotDestroyed is the deliberate contrast: it gives the reused id a
// different slot AND a different UUID, so it exercises the identity check where it carries
// information. This one exercises it where it carries none.
//
// The timing is the measured one. The predecessor's record has already spent its debounce, and the
// successor's marker lands one pass before the destroy -- a third of a second, on the node this was
// found on. The two cases differ only in whether the successor has reached the pass's live pod-UID
// set by then. It routinely has not: that set is captured when the pass opens and is built from an
// informer, so deciding ownership through it would read a current record through a stale filter,
// which is why ownership here is decided by the markers alone.
// TestReclaim_TwoDeadRecordsOnOneGiIdStillEndInADestroy is the supersession guard's convergence
// claim, asserted rather than argued. Two records claiming one gpu-instance id are each superseded by
// the other, so a guard that only looked at "does somebody else claim this id" could drop both and
// destroy neither, stranding the partition on an accelerator that never drains — the orphan sweep
// only runs on a drained one, so nothing downstream would collect it.
//
// It converges because the decision is retaken from disk under the accelerator lock: dropping the
// first record is visible to the pass that acts on the second, which then finds itself the only
// claimant and destroys on the ordinary path. The second case keeps another pod's live partition on
// the same accelerator, because that is the shape where a leak would be permanent.
func TestReclaim_TwoDeadRecordsOnOneGiIdStillEndInADestroy(t *testing.T) {
	const sharedUUID = "MIG-contested"
	slot := migPlacement{Start: 0, Length: 2}
	claim := func(podUID string) migMarker {
		return migMarker{
			PodUID: podUID, Container: "c", Card: testGPUUUID0, Profile: "1g.10gb",
			GiID: 1, CiID: 1, MigUUID: sharedUUID,
			ComputeSlices: 1, Start: slot.Start, Length: slot.Length,
		}
	}

	for _, c := range []struct {
		name        string
		alsoLivePod bool // a second pod holding its own partition on the same accelerator
	}{
		{name: "the contested accelerator holds nothing else"},
		{name: "another pod's live partition keeps the accelerator from draining", alsoLivePod: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			drv.seedLive(testGPUUUID0, migInstance{
				GiID: 1, CiID: 1, ComputeSlices: 1, Placement: slot, UUID: sharedUUID,
			})
			require.NoError(t, writeMarker(markerPath("pod-a", "c", testGPUUUID0), claim("pod-a")))
			require.NoError(t, writeMarker(markerPath("pod-b", "c", testGPUUUID0), claim("pod-b")))

			var live []string
			if c.alsoLivePod {
				other := migMarker{
					PodUID: "pod-live", Container: "c", Card: testGPUUUID0, Profile: "1g.10gb",
					GiID: 2, CiID: 2, MigUUID: "MIG-live", ComputeSlices: 1, Start: 2, Length: 2,
				}
				drv.seedLive(testGPUUUID0, migInstance{
					GiID: 2, CiID: 2, ComputeSlices: 1,
					Placement: migPlacement{Start: 2, Length: 2}, UUID: "MIG-live",
				})
				require.NoError(t, writeMarker(markerPath("pod-live", "c", testGPUUUID0), other))
				live = []string{"pod-live"}
			}

			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)
			for i := 0; i < reclaimMaxMisses*2+2; i++ {
				r.reconcile(live)
			}

			assert.Contains(t, drv.destroyed, migInstance{
				GiID: 1, CiID: 1, ComputeSlices: 1, Placement: slot, UUID: sharedUUID,
			}, "the contested partition must not survive both records being dropped")
			for _, p := range []string{"pod-a", "pod-b"} {
				_, err := parseMarker(markerPath(p, "c", testGPUUUID0))
				assert.Error(t, err, p+"'s record is retired")
			}
			if c.alsoLivePod {
				assert.NotContains(t, drv.destroyed, migInstance{
					GiID: 2, CiID: 2, ComputeSlices: 1,
					Placement: migPlacement{Start: 2, Length: 2}, UUID: "MIG-live",
				}, "the live pod's own partition is untouched")
			}
		})
	}
}

// TestReclaim_CorruptMarkerHoldsTheDestroyItMightOwn asserts the supersession guard honors the
// corrupt list the marker scan returns alongside the parseable entries. A corrupt file's contents are
// unreadable, so the gpu instance it claimed cannot be recovered and no per-instance check can see it;
// only holding the accelerator covers the case where that claim is the one superseding the record
// being reclaimed.
//
// The third case is the one that keeps the guard from being useless in the other direction: a pod's
// OWN corrupt marker is not somebody else's claim, and counting it would leave that pod's partitions
// unreclaimable for as long as its own bad file sits there.
func TestReclaim_CorruptMarkerHoldsTheDestroyItMightOwn(t *testing.T) {
	cases := []struct {
		name string
		// corruptPod owns the corrupt file; corruptCard is the accelerator its NAME names.
		corruptPod  string
		corruptCard string
		wantDestroy bool
	}{
		{
			name:        "another pod's corrupt marker on this card holds the destroy",
			corruptPod:  "pod-other",
			corruptCard: testGPUUUID0,
			wantDestroy: false,
		},
		{
			name:        "a corrupt marker naming no accelerator holds this card too",
			corruptPod:  "pod-other",
			corruptCard: "",
			wantDestroy: false,
		},
		{
			name:        "the reclaimed pod's own corrupt marker does not hold its own destroy",
			corruptPod:  "pod-dead",
			corruptCard: testGPUUUID0,
			wantDestroy: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			slot := migPlacement{Start: 0, Length: 2}
			drv.seedLive(testGPUUUID0, migInstance{
				GiID: 1, CiID: 1, ComputeSlices: 1, Placement: slot, UUID: "MIG-held",
			})
			require.NoError(t, writeMarker(markerPath("pod-dead", "c", testGPUUUID0), migMarker{
				PodUID: "pod-dead", Container: "c", Card: testGPUUUID0, Profile: "1g.10gb",
				GiID: 1, CiID: 1, MigUUID: "MIG-held",
				ComputeSlices: 1, Start: slot.Start, Length: slot.Length,
			}))
			// A second container dir, so the corrupt file never overwrites the record above.
			writeCorruptMarker(t, deviceplugin.PodWorkDir(c.corruptPod, "c2"), markerFileName(c.corruptCard))

			// The claiming pod has to be LIVE for its corrupt file to still be there when the
			// debounce elapses: the loop retires a corrupt marker whose pod is gone, so a dead
			// claimant's file disappears before the destroy it is supposed to hold back is reached.
			var live []string
			if c.corruptPod != "pod-dead" {
				live = []string{c.corruptPod}
			}

			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)
			for i := 0; i < reclaimMaxMisses+1; i++ {
				r.reconcile(live)
			}

			if c.wantDestroy {
				assert.NotEmpty(t, drv.destroyed, "the pod's own corrupt file must not shield its partitions")
				return
			}
			assert.Empty(t, drv.destroyed, "a partition a corrupt claim may own is never destroyed")
			_, err := parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
			require.NoError(t, err, "the record is kept so a later pass can find the partition again")
		})
	}
}

func TestReclaim_SameSlotRebuildNotDestroyedUnderStaleMarker(t *testing.T) {
	const sameUUID = "MIG-6deb82ec-name-based"
	slot := migPlacement{Start: 0, Length: 2}
	marker := func(podUID string) migMarker {
		return migMarker{
			PodUID: podUID, Container: "c", Card: testGPUUUID0, Profile: "1g.10gb",
			GiID: 1, CiID: 1, MigUUID: sameUUID,
			ComputeSlices: 1, Start: slot.Start, Length: slot.Length,
		}
	}

	cases := []struct {
		name string
		// live is the pod-UID set of the pass that acts on the predecessor's record.
		live []string
	}{
		{
			name: "the successor is in the pass's live set",
			live: []string{"pod-new"},
		},
		{
			name: "the successor has not reached the pass's live set yet",
			live: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			drv.seedLive(testGPUUUID0, migInstance{
				GiID: 1, CiID: 1, ComputeSlices: 1, Placement: slot, UUID: sameUUID,
			})
			// The predecessor's record spends its debounce while its Pod is gone.
			require.NoError(t, writeMarker(markerPath("pod-dead", "c", testGPUUUID0), marker("pod-dead")))
			r := newReclaimer(drv, deviceplugin.OperatorPodsDir, logr.Discard(), noClaims)
			for i := 0; i < reclaimMaxMisses-1; i++ {
				r.reconcile(nil)
			}
			require.Empty(t, drv.destroyed, "nothing is reclaimed before the debounce")

			// The successor takes the slot the predecessor's record still names, and writes its own
			// marker inside the accelerator lock -- the record that is already true.
			require.NoError(t, writeMarker(markerPath("pod-new", "c", testGPUUUID0), marker("pod-new")))

			r.reconcile(c.live)

			assert.Empty(t, drv.destroyed, "a partition another pod's marker claims is never destroyed")
			_, err := parseMarker(markerPath("pod-new", "c", testGPUUUID0))
			require.NoError(t, err, "the successor's marker is intact")
			_, err = parseMarker(markerPath("pod-dead", "c", testGPUUUID0))
			require.Error(t, err, "the superseded marker is dropped, so the decision is not retaken every pass")
		})
	}
}
