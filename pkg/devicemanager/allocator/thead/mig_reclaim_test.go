package thead

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noClaims is the empty attribution self-check source: no running Pod claims any placement.
func noClaims() (map[string][]migPlacement, error) { return nil, nil }

// hookedMigDriver wraps the package's fake driver with a hook that runs before each ListInstances,
// so a case can mutate the driver's state in the window BETWEEN the pass's lock-free snapshot and the
// destroy's re-read under the card lock. That window is the only thing that tells a re-read apart
// from a snapshot, so without it "verified under the lock" would be an assertion about structure
// rather than about behavior. It embeds the fake rather than replacing it: every other operation is
// the same fake the reservation tests drive.
//
// onCreate, when set, runs before each CreateInstance, so a case can observe an allocation's mutating
// call from the outside — the one moment at which a create is provably in flight on a card.
type hookedMigDriver struct {
	*fakeMigDriver
	onList   func()
	onCreate func()
}

func (h *hookedMigDriver) ListInstances() ([]migLiveInstance, error) {
	h.onList()
	return h.fakeMigDriver.ListInstances()
}

// CardInstances runs the same hook as ListInstances: the two are the node-wide read and the per-card
// one, and a case arming on "the Nth enumeration of this run" is counting reads of the card's state
// whichever seam they came through.
func (h *hookedMigDriver) CardInstances(cardUUID string) ([]migInstance, error) {
	h.onList()
	return h.fakeMigDriver.CardInstances(cardUUID)
}

func (h *hookedMigDriver) CreateInstance(
	cardUUID, profile string, computeSlices, memorySlices int32, slot migPlacement,
) (migInstance, error) {
	if h.onCreate != nil {
		h.onCreate()
	}
	return h.fakeMigDriver.CreateInstance(cardUUID, profile, computeSlices, memorySlices, slot)
}

// markerRef names one ownership marker by its owner and card, so a case declares the markers it
// expects to survive (or to be gone) without spelling out paths.
type markerRef struct {
	pod  string
	card string
}

// corruptFixture describes an unparseable marker to plant before a case runs: the Pod whose work dir
// holds it and the file name it carries (the name is what attributes it to a card, or to none). stray
// puts it one level above the container dir, where its path names no Pod either.
type corruptFixture struct {
	pod   string
	file  string
	stray bool
}

// seedMarkedInstance seeds a live partition on a card and writes its ownership marker, so the pair
// models one Pod's created-and-recorded partition.
func seedMarkedInstance(t *testing.T, drv *fakeMigDriver, podsDir, podUID, card string, giID uint32, slot migPlacement) {
	t.Helper()
	seedMarkedInstanceOf(t, drv, podsDir, podUID, "c", card, giID, slot)
}

// seedMarkedInstanceOf is seedMarkedInstance for a named container, so a Pod whose several containers
// each carved a partition on ONE card is modeled with one marker per container — the shape the
// ownership record is per container for, and the shape a per-marker driver enumeration multiplies.
func seedMarkedInstanceOf(
	t *testing.T, drv *fakeMigDriver, podsDir, podUID, container, card string, giID uint32, slot migPlacement,
) {
	t.Helper()
	inst := migInstance{
		GiID: giID, CiID: giID, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: slot, UUID: "MIG-" + card + "-" + podUID + "-" + container,
	}
	drv.seedLive(card, inst)
	m := selfMarker(podUID, card, inst)
	m.Container = container
	writeMarkerFixture(t, podsDir, m)
}

// migSeed is one live partition to seed on a card, as the orphan sweep sees it (no parseable marker
// owns it).
type migSeed struct {
	card string
	giID uint32
	slot migPlacement
}

// seedInstances seeds each migSeed as a live marker-less partition on its card.
func seedInstances(drv *fakeMigDriver, seeds ...migSeed) {
	for _, s := range seeds {
		drv.seedLive(s.card, migInstance{
			GiID: s.giID, CiID: s.giID, ProfileID: testProfileID, ComputeSlices: 1,
			Placement: s.slot, UUID: "MIG-orphan-" + s.card,
		})
	}
}

// writeCorruptMarker plants a truncated marker file and returns its path.
func writeCorruptMarker(t *testing.T, dir, fileName string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, fileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"podUID":"pod-crash","card":"PPU-aaa`), 0o600))
	return path
}

// corruptDir returns the directory a corruptFixture's file belongs in.
func corruptDir(podsDir string, c corruptFixture) string {
	if c.stray {
		return filepath.Join(podsDir, c.pod)
	}
	return filepath.Dir(markerPath(podsDir, c.pod, "c", testPPUUUID0))
}

// destroyedGiIDs lists the GPU-instance ids the driver actually tore down.
func destroyedGiIDs(drv *fakeMigDriver) []uint32 {
	ids := make([]uint32, 0, len(drv.destroyed))
	for _, inst := range drv.destroyed {
		ids = append(ids, inst.GiID)
	}
	return ids
}

// reclaimCase is one declarative reclaim scenario: the state to seed, the liveness view to reconcile
// against, how many passes to run, and the partitions and markers that must be left afterwards.
type reclaimCase struct {
	name  string
	setup func(t *testing.T, drv *fakeMigDriver, podsDir string)
	// corrupt plants an unparseable marker before setup; a case that declares one asserts it survives
	// every pass (retirement is asserted separately, where the phase transition is the subject).
	corrupt *corruptFixture
	// claims is the attribution self-check source; nil means no running Pod claims anything.
	claims func() (map[string][]migPlacement, error)
	// listHook runs before each driver read of the card state — the node-wide ListInstances a pass
	// opens with and the per-card CardInstances the destroy re-reads under the lock — numbered from 1
	// across the whole run, so a case
	// can act inside the snapshot-to-lock window.
	listHook        func(t *testing.T, drv *fakeMigDriver, podsDir string, call int)
	live            []string
	passes          int
	wantDestroyed   []uint32
	wantMarkers     []markerRef
	wantGoneMarkers []markerRef
}

func TestReclaim(t *testing.T) {
	deadPodMarker := markerRef{pod: "pod-dead", card: testPPUUUID0}
	claimsOn := func(card string, ps ...migPlacement) func() (map[string][]migPlacement, error) {
		return func() (map[string][]migPlacement, error) {
			return map[string][]migPlacement{card: ps}, nil
		}
	}

	cases := []reclaimCase{
		{
			name: "a dead pod's partition survives the passes before the debounce",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})
			},
			passes:      reclaimMaxMisses - 1,
			wantMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a dead pod's partition is reclaimed at the debounce",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})
			},
			passes:          reclaimMaxMisses,
			wantDestroyed:   []uint32{1},
			wantGoneMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a live pod's partition is never reclaimed",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-live", testPPUUUID0, 1, migPlacement{0, 2})
			},
			live:        []string{"pod-live"},
			passes:      reclaimMaxMisses + 2,
			wantMarkers: []markerRef{{pod: "pod-live", card: testPPUUUID0}},
		},
		{
			name: "a placement a running pod claims is never destroyed",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})
			},
			claims:      claimsOn(testPPUUUID0, migPlacement{0, 2}),
			passes:      reclaimMaxMisses + 3,
			wantMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a live-claims read error skips the pass",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})
			},
			claims: func() (map[string][]migPlacement, error) {
				return nil, assert.AnError
			},
			passes:      reclaimMaxMisses + 3,
			wantMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a partition enumeration error skips the pass",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})
				drv.listErr = assert.AnError
			},
			passes:      reclaimMaxMisses + 3,
			wantMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a gpu-instance id reused by another identity is retained, only the stale marker is dropped",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				// A live pod owns the partition now at GPU instance 1, at a non-overlapping slot the
				// placement-based attribution check therefore does not catch.
				seedMarkedInstance(t, drv, podsDir, "pod-new", testPPUUUID0, 1, migPlacement{4, 2})
				stale := selfMarker("pod-dead", testPPUUUID0, migInstance{
					GiID: 1, CiID: 1, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-old",
				})
				writeMarkerFixture(t, podsDir, stale)
			},
			claims:          claimsOn(testPPUUUID0, migPlacement{4, 2}),
			live:            []string{"pod-new"},
			passes:          reclaimMaxMisses,
			wantMarkers:     []markerRef{{pod: "pod-new", card: testPPUUUID0}},
			wantGoneMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a gpu-instance id reused by another raw profile is retained, identity string alone is not enough",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 1, CiID: 1, ProfileID: testOtherProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-same",
				})
				writeMarkerFixture(t, podsDir, selfMarker("pod-dead", testPPUUUID0, migInstance{
					GiID: 1, CiID: 1, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-same",
				}))
			},
			passes:          reclaimMaxMisses,
			wantGoneMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a gpu-instance id reused after the pass snapshot is retained (verified under the card lock)",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})
			},
			// The 4th enumeration is the destroy's re-read under the card lock: three pass snapshots
			// precede it, and the third of them still matched the marker. Replacing the partition here
			// models an out-of-band destroy plus id reuse inside exactly that window.
			listHook: func(_ *testing.T, drv *fakeMigDriver, _ string, call int) {
				if call != reclaimMaxMisses+1 {
					return
				}
				drv.live[testPPUUUID0] = []migInstance{{
					GiID: 1, CiID: 1, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-reused",
				}}
			},
			passes:          reclaimMaxMisses,
			wantGoneMarkers: []markerRef{deadPodMarker},
		},
		{
			name: "a marker-less partition on a card hosting a live pod is kept",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-live", testPPUUUID0, 1, migPlacement{0, 2})
				seedInstances(drv, migSeed{card: testPPUUUID0, giID: 2, slot: migPlacement{2, 2}})
			},
			live:   []string{"pod-live"},
			passes: reclaimMaxMisses + 3,
		},
		{
			name: "a marker-less partition is kept while the card still carries any marker",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				// The marked pod is dead but its partition is wedged in use, so its marker cannot be
				// removed and the card is never provably drained.
				drv.inUseGiIDs = map[uint32]bool{1: true}
				seedMarkedInstance(t, drv, podsDir, "pod-stuck", testPPUUUID0, 1, migPlacement{0, 2})
				seedInstances(drv, migSeed{card: testPPUUUID0, giID: 2, slot: migPlacement{2, 2}})
			},
			passes:      reclaimMaxMisses + 2,
			wantMarkers: []markerRef{{pod: "pod-stuck", card: testPPUUUID0}},
		},
		{
			name: "a marker-less partition on a drained card survives the passes before the debounce",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				seedInstances(drv, migSeed{card: testPPUUUID0, giID: 2, slot: migPlacement{2, 2}})
			},
			passes: reclaimMaxMisses - 1,
		},
		{
			name: "a marker-less partition on a drained card is collected at the debounce",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				seedInstances(drv, migSeed{card: testPPUUUID0, giID: 2, slot: migPlacement{2, 2}})
			},
			passes:        reclaimMaxMisses,
			wantDestroyed: []uint32{2},
		},
		{
			name:    "an unparseable marker of a live pod holds its card off the orphan sweep",
			corrupt: &corruptFixture{pod: "pod-live", file: markerFileName(testPPUUUID0)},
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				seedInstances(drv, migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}})
			},
			live:   []string{"pod-live"},
			passes: reclaimMaxMisses*2 + 2,
		},
		{
			name:    "an unparseable marker naming no card holds every card",
			corrupt: &corruptFixture{pod: "pod-live", file: markerFileName("")},
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				seedInstances(drv,
					migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}},
					migSeed{card: testPPUUUID1, giID: 2, slot: migPlacement{0, 2}})
			},
			live:   []string{"pod-live"},
			passes: reclaimMaxMisses*2 + 2,
		},
		{
			name:    "a sibling card's marker-less partition is still collected",
			corrupt: &corruptFixture{pod: "pod-live", file: markerFileName(testPPUUUID1)},
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				seedInstances(drv,
					migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}},
					migSeed{card: testPPUUUID1, giID: 2, slot: migPlacement{0, 2}})
			},
			live:          []string{"pod-live"},
			passes:        reclaimMaxMisses*2 + 2,
			wantDestroyed: []uint32{1},
		},
		{
			name:    "an unparseable path naming no pod is held indefinitely",
			corrupt: &corruptFixture{pod: "pod-dead", file: markerFileName(testPPUUUID0), stray: true},
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				seedInstances(drv, migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}})
			},
			passes: reclaimMaxMisses*2 + 2,
		},
		{
			name: "an unparseable marker appearing after the pass snapshot spares the partition (re-scanned under the lock)",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				seedInstances(drv, migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}})
			},
			// The pass that matures the card's debounce scanned a clean pods root; the marker lands
			// after that scan and before the collector takes the card lock, so only the under-lock
			// re-scan can see it.
			listHook: func(t *testing.T, _ *fakeMigDriver, podsDir string, call int) {
				if call != reclaimMaxMisses {
					return
				}
				writeCorruptMarker(t, corruptDir(podsDir, corruptFixture{pod: "pod-crash"}), markerFileName(testPPUUUID0))
			},
			passes: reclaimMaxMisses,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			podsDir := t.TempDir()
			drv := newFakeMigDriver()
			drv.seedCard(testPPUUUID0)
			drv.seedCard(testPPUUUID1)

			var corruptPath string
			if c.corrupt != nil {
				corruptPath = writeCorruptMarker(t, corruptDir(podsDir, *c.corrupt), c.corrupt.file)
			}
			if c.setup != nil {
				c.setup(t, drv, podsDir)
			}

			var driver migDriver = drv
			if c.listHook != nil {
				calls := 0
				driver = &hookedMigDriver{fakeMigDriver: drv, onList: func() {
					calls++
					c.listHook(t, drv, podsDir, calls)
				}}
			}
			claims := c.claims
			if claims == nil {
				claims = noClaims
			}

			r := newReclaimer(driver, podsDir, logr.Discard(), claims)
			for range c.passes {
				r.reconcile(c.live)
			}

			assert.ElementsMatch(t, c.wantDestroyed, destroyedGiIDs(drv))
			for _, ref := range c.wantMarkers {
				_, err := parseMarker(markerPath(podsDir, ref.pod, "c", ref.card))
				assert.NoError(t, err, "marker of pod %q on card %q is retained", ref.pod, ref.card)
			}
			for _, ref := range c.wantGoneMarkers {
				assert.NoFileExists(t, markerPath(podsDir, ref.pod, "c", ref.card),
					"marker of pod %q on card %q is gone", ref.pod, ref.card)
			}
			if corruptPath != "" {
				assert.FileExists(t, corruptPath, "the unparseable marker is kept while it can still stand for an owner")
			}
		})
	}
}

// TestReclaimBusyDestroyBoundedRetry asserts a residual busy rejection never destroys the partition,
// keeps retrying every pass (the debounce is not cleared), surfaces the operator-visible log exactly
// once at the bound, and finally reclaims once the holding process exits.
func TestReclaimBusyDestroyBoundedRetry(t *testing.T) {
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	drv.inUseGiIDs = map[uint32]bool{1: true}
	seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})

	var bounded int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "still in use after bounded destroy retries") {
			bounded++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, podsDir, logger, noClaims)

	for range reclaimMaxMisses + reclaimMaxDestroyMisses + 2 {
		r.reconcile(nil)
	}
	assert.Empty(t, drv.destroyed, "a partition in use is never destroyed")
	_, err := parseMarker(markerPath(podsDir, "pod-dead", "c", testPPUUUID0))
	require.NoError(t, err, "the marker is retained while the partition is in use")
	assert.Equal(t, 1, bounded, "the operator-visible log fires exactly once at the bound")

	drv.inUseGiIDs = nil
	r.reconcile(nil)
	assert.Equal(t, []uint32{1}, destroyedGiIDs(drv), "the retry succeeds once the holding process exits")
	assert.NoFileExists(t, markerPath(podsDir, "pod-dead", "c", testPPUUUID0))
}

// TestReclaimEnumeratesOncePerCard pins the reclaim pass's driver cost: one node-wide enumeration for
// the pass itself, plus one under the card lock per DISTINCT card carrying dead markers — never one per
// marker. The node-wide call probes every card's whole profile space and re-queries placements per
// answered profile, so a Pod whose several containers each hold a partition on one card would multiply
// thousands of driver round-trips by the number of its containers for no added evidence: the lock is
// held across the group, so one re-read describes the card for all of them.
func TestReclaimEnumeratesOncePerCard(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, drv *fakeMigDriver, podsDir string)
		// wantInLockReads is the number of under-lock re-reads the destroying pass must make: one per
		// distinct card carrying a dead pod's markers, however many markers that card carries.
		wantInLockReads int
		wantDestroyed   []uint32
	}{
		{
			name: "two containers of one dead pod on one card share a single re-read",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstanceOf(t, drv, podsDir, "pod-dead", "main", testPPUUUID0, 1, migPlacement{0, 2})
				seedMarkedInstanceOf(t, drv, podsDir, "pod-dead", "worker", testPPUUUID0, 2, migPlacement{2, 2})
			},
			wantInLockReads: 1,
			wantDestroyed:   []uint32{1, 2},
		},
		{
			name: "a dead pod spanning two cards re-reads once per card",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, 1, migPlacement{0, 2})
				seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID1, 2, migPlacement{0, 2})
			},
			wantInLockReads: 2,
			wantDestroyed:   []uint32{1, 2},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			podsDir := t.TempDir()
			drv := newFakeMigDriver()
			drv.seedCard(testPPUUUID0)
			drv.seedCard(testPPUUUID1)
			c.setup(t, drv, podsDir)

			r := newReclaimer(drv, podsDir, logr.Discard(), noClaims)
			for range reclaimMaxMisses {
				r.reconcile(nil)
			}

			assert.ElementsMatch(t, c.wantDestroyed, destroyedGiIDs(drv))
			assert.Equal(t, reclaimMaxMisses, drv.listCalls,
				"the node-wide enumeration is made once per pass and never for the under-lock re-read: "+
					"that read happens with a card's lock held, so it must cost one card, not the node")
			assert.Equal(t, c.wantInLockReads, drv.cardListCalls,
				"one per-card re-read per distinct card with dead markers, shared by that card's markers")
		})
	}
}

// TestReclaimUnattributableCorruptPathBoundedLog asserts the one hold this loop cannot release is not
// silent: a corrupt path naming neither a Pod nor a card keeps failing closed node-wide forever (there
// is no liveness evidence to retire it on), and the loop surfaces the operator-visible log naming the
// path exactly once, at the bound — the same surface the busy-destroy path uses, because a status
// condition would be stomped by the wholesale Devices.Status rebuild.
func TestReclaimUnattributableCorruptPathBoundedLog(t *testing.T) {
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	seedInstances(drv, migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}})
	// A marker-named file one level above a container dir: its path names no Pod, and its own name is
	// not attributable to a card either once the walk collected the directory it sits in.
	path := writeCorruptMarker(t,
		corruptDir(podsDir, corruptFixture{pod: "pod-dead", stray: true}), markerFileName(""))

	var bounded int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "will not clear by itself") {
			bounded++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, podsDir, logger, noClaims)

	for range reclaimMaxCorruptHoldMisses + 3 {
		r.reconcile(nil)
	}

	assert.FileExists(t, path, "an unattributable path is kept: there is no evidence to retire it on")
	assert.Empty(t, drv.destroyed, "and it keeps every card off the orphan sweep while it persists")
	assert.Equal(t, 1, bounded, "the operator-visible log fires exactly once at the bound")
}

// TestReclaimLiveOwnersCorruptMarkerBoundedLog asserts the sibling hold is not silent either. An
// unparseable record whose Pod is still running is kept — the Pod depends on the ownership it records —
// and nothing in this loop can release it while that Pod lives, which is the case that reads as
// transient and is not. So it earns the same surface: one operator-visible log naming the card, the Pod
// and the path, at the bound and only there. What the record does is unchanged, before the bound and
// after it.
func TestReclaimLiveOwnersCorruptMarkerBoundedLog(t *testing.T) {
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	seedInstances(drv, migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}})
	path := writeCorruptMarker(t,
		corruptDir(podsDir, corruptFixture{pod: "pod-live"}), markerFileName(testPPUUUID0))

	var bounded int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "delete the pod to release the card") {
			bounded++
		}
	}, funcr.Options{})
	r := newReclaimer(drv, podsDir, logger, noClaims)
	live := []string{"pod-live"}

	for range reclaimMaxCorruptHoldMisses - 1 {
		r.reconcile(live)
	}
	assert.Zero(t, bounded, "the operator-visible log does not fire before the bound")
	assert.FileExists(t, path, "and the record is kept meanwhile")

	r.reconcile(live)
	assert.Equal(t, 1, bounded, "it fires at the bound")

	for range 3 {
		r.reconcile(live)
	}
	assert.Equal(t, 1, bounded, "and exactly once, however long the hold stands")
	assert.FileExists(t, path, "the record is still kept: its live pod depends on the ownership it records")
	assert.Empty(t, drv.destroyed, "and its card stays off the orphan sweep throughout")
}

// TestReclaimRacesAllocationOnSameCard asserts a reclaim pass and an allocation on the SAME card do not
// corrupt each other's partition, with the interleaving pinned rather than hoped for: the pass is parked
// inside the card lock — after its under-lock re-read, before its destroy — and only then does the
// allocating goroutine exist, so it cannot reach the card until the reclaim leaves.
//
// The freed slot is the assertion that proves the order actually held: the dead partition occupies the
// lowest placement, so an allocation that ran before the destroy would have to take the next one. The
// exclusion counter is the second, order-independent statement of the same thing — a create must never be
// in flight on a card while a destroy holds that card's lock, whichever way the two goroutines interleave.
func TestReclaimRacesAllocationOnSameCard(t *testing.T) {
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	// The dead partition's id is deliberately not the one the fake mints next, so the allocation's own
	// partition is distinguishable from the reclaimed one rather than reusing its id.
	const deadGiID = uint32(5)
	seedMarkedInstance(t, drv, podsDir, "pod-dead", testPPUUUID0, deadGiID, migPlacement{0, 2})

	var (
		armed          bool
		listsWhenArmed int
		enter          = make(chan struct{})
		release        = make(chan struct{})
		// parked is true exactly while the reclaim sits inside the card's critical section, and
		// createdWhileParked counts the creates that got through anyway — which the card lock makes
		// impossible, so any count above zero is the exclusion broken.
		parked             atomic.Bool
		createdWhileParked atomic.Int32
	)
	// The armed pass makes two enumerations: the pass's own lock-free one, then the destroy's re-read
	// inside the card lock. Parking on the second is what puts the reclaim provably inside the critical
	// section the allocation must wait for.
	driver := &hookedMigDriver{
		fakeMigDriver: drv,
		onList: func() {
			if !armed {
				return
			}
			listsWhenArmed++
			if listsWhenArmed == 2 {
				parked.Store(true)
				close(enter)
				<-release
				parked.Store(false)
			}
		},
		onCreate: func() {
			if parked.Load() {
				createdWhileParked.Add(1)
			}
		},
	}
	r := newReclaimer(driver, podsDir, logr.Discard(), noClaims)

	for range reclaimMaxMisses - 1 {
		r.reconcile(nil)
	}

	var wg sync.WaitGroup
	armed = true
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.reconcile(nil)
	}()

	<-enter // the reclaim holds the card lock and has re-read the card

	reserved := make(chan migInstance, 1)
	locking := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(locking)
		unlock := lockCard(testPPUUUID0)
		defer unlock()
		inst, outcome, err := reserveMigInstance(driver, podsDir, "pod-new", "c", testPPUUUID0, testProfile, 1, 2)
		require.NoError(t, err)
		require.Equal(t, migCreated, outcome)
		reserved <- inst
	}()

	<-locking
	close(release)
	wg.Wait()

	assert.Equal(t, []uint32{deadGiID}, destroyedGiIDs(drv), "the dead pod's partition is reclaimed")
	assert.NoFileExists(t, markerPath(podsDir, "pod-dead", "c", testPPUUUID0))

	inst := <-reserved
	assert.Equal(t, migPlacement{0, 2}, inst.Placement,
		"the allocation waited for the destroy: it takes the slot the reclaim freed")
	m, err := parseMarker(markerPath(podsDir, "pod-new", "c", testPPUUUID0))
	require.NoError(t, err, "the allocation's own marker survives the pass that ran beside it")
	assert.Equal(t, inst, m.instance())
	assert.NotContains(t, destroyedGiIDs(drv), inst.GiID, "the reclaim never touches the new allocation's partition")
	assert.Zero(t, createdWhileParked.Load(),
		"no create is in flight on a card while a destroy holds that card's lock")
}

// TestReclaimCorruptMarkerOfDeadPodConverges asserts the hold clears by itself instead of leaking a
// partition for the node's lifetime: an unparseable marker whose Pod is gone is retired on that
// evidence alone (its path names the Pod) after the same debounce every other decision here uses, and
// the partition it shadowed then becomes a genuine marker-less orphan the collector takes once the
// card's own debounce elapses. The retirement is observed by the next pass, never by the one that
// removed the file.
func TestReclaimCorruptMarkerOfDeadPodConverges(t *testing.T) {
	cases := []struct {
		name string
		file string
	}{
		{name: "an unparseable marker naming its card", file: markerFileName(testPPUUUID0)},
		{name: "an unparseable marker naming no card", file: markerFileName("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			podsDir := t.TempDir()
			drv := newFakeMigDriver()
			drv.seedCard(testPPUUUID0)
			seedInstances(drv, migSeed{card: testPPUUUID0, giID: 1, slot: migPlacement{0, 2}})
			path := writeCorruptMarker(t, corruptDir(podsDir, corruptFixture{pod: "pod-dead"}), c.file)
			r := newReclaimer(drv, podsDir, logr.Discard(), noClaims)

			for range reclaimMaxMisses - 1 {
				r.reconcile(nil)
			}
			assert.FileExists(t, path, "the unparseable marker is not retired before the debounce")

			r.reconcile(nil)
			assert.NoFileExists(t, path, "the unparseable marker of a dead pod is retired after the debounce")
			assert.Empty(t, drv.destroyed, "the pass that removed the file still holds the card closed")

			// With the record gone, the shadowed partition is a plain marker-less orphan.
			for range reclaimMaxMisses - 1 {
				r.reconcile(nil)
			}
			assert.Empty(t, drv.destroyed, "the card restarts its own drained debounce from scratch")

			r.reconcile(nil)
			assert.Equal(t, []uint32{1}, destroyedGiIDs(drv), "the shadowed partition is collected once the card drains")
		})
	}
}
