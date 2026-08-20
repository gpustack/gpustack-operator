package nvidia

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// fakeMigDriver is an in-memory migDriver: it records create/destroy calls and holds a
// per-accelerator possible-placement set and live-instance list, so the marker/slot-pick/reuse core
// is table-tested without NVML. It is concurrency-safe (the reclaim/actuator holds the
// per-accelerator lock, but different accelerators run in parallel and Go maps are not).
type fakeMigDriver struct {
	mu       sync.Mutex
	possible map[string][]migPlacement
	live     map[string][]migInstance

	nextGiID    uint32
	createCalls int
	createErr   error
	destroyed   []migInstance
	// inUseGiIDs marks GPU-instance ids whose DestroyInstance fails with errInstanceInUse (a
	// residual process), so the reclaim loop's bounded-retry path is table-tested.
	inUseGiIDs map[uint32]bool
	// processesByGiID is how many compute processes each partition answers with, and
	// processesErr makes the query itself fail — the partition that cannot be asked.
	processesByGiID map[uint32]int
	processesErr    error
	listErr         error

	// listCalls and cardListCalls count the node-wide and the per-accelerator enumerations, and
	// listHook runs before each of either with the combined count. Together they let a case change the
	// accelerator's live set between the enumeration a pass opens with and the one taken under the
	// accelerator lock — the only way to model an out-of-band destroy plus id reuse landing inside that
	// window — and let one assert that the under-lock read costs an accelerator rather than the node.
	listCalls     int
	cardListCalls int
	listHook      func(drv *fakeMigDriver, call int)
}

func newFakeMigDriver() *fakeMigDriver {
	return &fakeMigDriver{
		possible: make(map[string][]migPlacement),
		live:     make(map[string][]migInstance),
	}
}

func (f *fakeMigDriver) CardState(cardUUID, _ string, _, _ int32) (migCardState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return migCardState{
		Possible: append([]migPlacement(nil), f.possible[cardUUID]...),
		Live:     append([]migInstance(nil), f.live[cardUUID]...),
	}, nil
}

func (f *fakeMigDriver) CreateInstance(cardUUID, _ string, computeSlices, _ int32, slot migPlacement) (migInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return migInstance{}, f.createErr
	}
	f.nextGiID++
	inst := migInstance{
		GiID:          f.nextGiID,
		CiID:          f.nextGiID,
		ComputeSlices: computeSlices,
		Placement:     slot,
		UUID:          fmt.Sprintf("MIG-%s-%d", cardUUID, f.nextGiID),
	}
	f.live[cardUUID] = append(f.live[cardUUID], inst)
	return inst, nil
}

func (f *fakeMigDriver) DestroyInstance(cardUUID string, inst migInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inUseGiIDs[inst.GiID] {
		return fmt.Errorf("card %s: destroy gpu instance %d: %w", cardUUID, inst.GiID, errInstanceInUse)
	}
	f.destroyed = append(f.destroyed, inst)
	kept := f.live[cardUUID][:0]
	for _, l := range f.live[cardUUID] {
		if l.GiID != inst.GiID {
			kept = append(kept, l)
		}
	}
	f.live[cardUUID] = kept
	return nil
}

// InstanceProcesses answers how many compute processes hold one partition (the reclaim
// process-check seam). An unseeded partition holds none, which is the ordinary case.
func (f *fakeMigDriver) InstanceProcesses(_ string, inst migInstance) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.processesErr != nil {
		return 0, f.processesErr
	}
	return f.processesByGiID[inst.GiID], nil
}

// ListInstances returns every seeded live instance across all accelerators (the reclaim orphan-GC seam).
func (f *fakeMigDriver) ListInstances() ([]migLiveInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listHook != nil {
		f.listHook(f, f.listCalls+f.cardListCalls)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []migLiveInstance
	for card, insts := range f.live {
		for _, inst := range insts {
			out = append(out, migLiveInstance{Card: card, Inst: inst})
		}
	}
	return out, nil
}

// CardInstances returns one accelerator's seeded live instances (the verification-re-read seam), so
// a caller holding that accelerator's lock never pays for the node-wide walk.
func (f *fakeMigDriver) CardInstances(cardUUID string) ([]migInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cardListCalls++
	if f.listHook != nil {
		f.listHook(f, f.listCalls+f.cardListCalls)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]migInstance(nil), f.live[cardUUID]...), nil
}

// seedLive appends a live instance to an accelerator (an out-of-band / reusable partition).
func (f *fakeMigDriver) seedLive(cardUUID string, inst migInstance) {
	f.live[cardUUID] = append(f.live[cardUUID], inst)
}

// evenSlots returns the memory-slice placements of a 2-slice profile on an 8-slice accelerator
// ([0,2),[2,2),[4,2),[6,2)) — the 1g.10gb legal set used across the tests.
func evenSlots() []migPlacement {
	return []migPlacement{{0, 2}, {2, 2}, {4, 2}, {6, 2}}
}

func TestPickPlacement(t *testing.T) {
	cases := []struct {
		name     string
		possible []migPlacement
		occupied []migPlacement
		want     migPlacement
		wantOK   bool
	}{
		{
			name:     "empty accelerator picks lowest",
			possible: evenSlots(),
			want:     migPlacement{0, 2},
			wantOK:   true,
		},
		{
			name:     "lowest free skips the overlapping slot",
			possible: evenSlots(),
			occupied: []migPlacement{{0, 2}},
			want:     migPlacement{2, 2},
			wantOK:   true,
		},
		{
			name:     "a wide occupant blocks several slots",
			possible: evenSlots(),
			occupied: []migPlacement{{0, 4}}, // a 3g/4g partition on slices 0-3
			want:     migPlacement{4, 2},
			wantOK:   true,
		},
		{
			name:     "full accelerator yields nothing",
			possible: evenSlots(),
			occupied: []migPlacement{{0, 8}},
			wantOK:   false,
		},
		{
			name:     "unsorted possible still returns the lowest",
			possible: []migPlacement{{4, 2}, {0, 2}, {6, 2}, {2, 2}},
			occupied: []migPlacement{{0, 2}},
			want:     migPlacement{2, 2},
			wantOK:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pickPlacement(c.possible, c.occupied)
			assert.Equal(t, c.wantOK, ok)
			if c.wantOK {
				assert.Equal(t, c.want, got)
			}
		})
	}
}

func TestReserveMigInstance(t *testing.T) {
	const profile = "1g.10gb"

	t.Run("creates at the lowest free placement", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		drv.possible[testGPUUUID0] = evenSlots()

		inst, outcome, err := reserveMigInstance(
			drv, deviceplugin.OperatorPodsDir, "pod-a", "c", testGPUUUID0, profile, 1, 2)
		require.NoError(t, err)
		assert.Equal(t, migCreated, outcome)
		assert.Equal(t, migPlacement{0, 2}, inst.Placement)
		assert.Equal(t, 1, drv.createCalls)

		// The marker round-trips: it names the created instance so reclaim can destroy it.
		m, err := parseMarker(markerPath("pod-a", "c", testGPUUUID0))
		require.NoError(t, err)
		assert.Equal(t, profile, m.Profile)
		assert.Equal(t, inst.GiID, m.GiID)
		assert.Equal(t, inst.UUID, m.MigUUID)
	})

	t.Run("second accelerator of a new pod picks the next free slot", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		drv.possible[testGPUUUID0] = evenSlots()
		// A prior pod already holds slot 0 (its marker owns GI 100).
		drv.seedLive(testGPUUUID0, migInstance{GiID: 100, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-x"})
		require.NoError(t, writeMarker(markerPath("pod-old", "c", testGPUUUID0), migMarker{
			PodUID: "pod-old", Container: "c", Card: testGPUUUID0, Profile: profile,
			GiID: 100, CiID: 100, MigUUID: "MIG-x", ComputeSlices: 1, Start: 0, Length: 2,
		}))

		inst, outcome, err := reserveMigInstance(
			drv, deviceplugin.OperatorPodsDir, "pod-b", "c", testGPUUUID0, profile, 1, 2)
		require.NoError(t, err)
		assert.Equal(t, migCreated, outcome)
		assert.Equal(t, migPlacement{2, 2}, inst.Placement, "slot 0 is occupied by pod-old")
	})

	t.Run("binds a reusable unbound instance instead of creating", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		drv.possible[testGPUUUID0] = evenSlots()
		// An unbound instance (no marker references it) of the requested geometry.
		drv.seedLive(testGPUUUID0, migInstance{GiID: 7, ComputeSlices: 1, Placement: migPlacement{4, 2}, UUID: "MIG-reuse"})

		inst, outcome, err := reserveMigInstance(
			drv, deviceplugin.OperatorPodsDir, "pod-c", "c", testGPUUUID0, profile, 1, 2)
		require.NoError(t, err)
		assert.Equal(t, migBound, outcome, "an unbound instance is reused, not created")
		assert.Equal(t, uint32(7), inst.GiID)
		assert.Equal(t, "MIG-reuse", inst.UUID)
		assert.Equal(t, 0, drv.createCalls)
	})

	t.Run("full accelerator fails without creating", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		drv.possible[testGPUUUID0] = evenSlots()
		drv.seedLive(testGPUUUID0, migInstance{GiID: 1, ComputeSlices: 7, Placement: migPlacement{0, 8}, UUID: "MIG-whole"})

		_, _, err := reserveMigInstance(
			drv, deviceplugin.OperatorPodsDir, "pod-d", "c", testGPUUUID0, profile, 1, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no free placement")
		assert.Equal(t, 0, drv.createCalls)
	})

	t.Run("rebinds its own live instance on a retry", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		drv.possible[testGPUUUID0] = evenSlots()
		drv.seedLive(testGPUUUID0, migInstance{GiID: 5, CiID: 5, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-self"})
		require.NoError(t, writeMarker(markerPath("pod-e", "c", testGPUUUID0), migMarker{
			PodUID: "pod-e", Container: "c", Card: testGPUUUID0, Profile: profile,
			GiID: 5, CiID: 5, MigUUID: "MIG-self", ComputeSlices: 1, Start: 0, Length: 2,
		}))

		inst, outcome, err := reserveMigInstance(
			drv, deviceplugin.OperatorPodsDir, "pod-e", "c", testGPUUUID0, profile, 1, 2)
		require.NoError(t, err)
		assert.Equal(t, migRebound, outcome, "a retry rebinds its own partition, not a new one")
		assert.Equal(t, "MIG-self", inst.UUID)
		assert.Equal(t, 0, drv.createCalls)
	})

	t.Run("fails closed when its gpu-instance id was reused by another partition", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		drv := newFakeMigDriver()
		drv.possible[testGPUUUID0] = evenSlots()
		// The live GI 5 is now a different partition (different MIG UUID) than the marker recorded.
		drv.seedLive(testGPUUUID0, migInstance{GiID: 5, CiID: 5, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-new"})
		require.NoError(t, writeMarker(markerPath("pod-f", "c", testGPUUUID0), migMarker{
			PodUID: "pod-f", Container: "c", Card: testGPUUUID0, Profile: profile,
			GiID: 5, CiID: 5, MigUUID: "MIG-old", ComputeSlices: 1, Start: 0, Length: 2,
		}))

		_, _, err := reserveMigInstance(
			drv, deviceplugin.OperatorPodsDir, "pod-f", "c", testGPUUUID0, profile, 1, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "id reused")
		assert.Equal(t, 0, drv.createCalls)
	})
}

// TestReserveMigInstance_SkipsEmptyUUIDInstance asserts a live GPU instance with no MIG-device
// UUID (a compute-instance-less orphan a crash left behind) is not reused — the allocation
// creates a fresh, usable partition instead of adopting the broken one and injecting an empty
// NVIDIA_VISIBLE_DEVICES.
func TestReserveMigInstance_SkipsEmptyUUIDInstance(t *testing.T) {
	const profile = "1g.10gb"
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	// A geometry-matching but CI-less orphan on slot 0 (no UUID); no marker owns it.
	drv.seedLive(testGPUUUID0, migInstance{GiID: 9, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: ""})

	inst, outcome, err := reserveMigInstance(
		drv, deviceplugin.OperatorPodsDir, "pod-a", "c", testGPUUUID0, profile, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, migCreated, outcome, "a UUID-less orphan is not adopted")
	assert.Equal(t, 1, drv.createCalls)
	assert.NotEqual(t, uint32(9), inst.GiID)
	assert.Equal(t, migPlacement{2, 2}, inst.Placement, "the orphan's slot 0 is treated as occupied")
	assert.NotEmpty(t, inst.UUID)
}

// writeCorruptMarker writes an unparseable marker file (a truncated write an unclean node
// shutdown leaves behind: the name is complete, the record is not) into dir, so a scan collects it
// as a corrupt path. dir and fileName are passed verbatim so a test can place the file where a real
// marker lives or one level off, and name the accelerator the corrupt record belonged to or name none.
func writeCorruptMarker(t *testing.T, dir, fileName string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o777))
	path := filepath.Join(dir, fileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"podUID":"pod-crash","card":"GPU-aaa`), 0o644))
	return path
}

// TestReserveMigInstance_CorruptMarkerFailsAdoptionClosed asserts a corrupt ownership marker fails
// adoption closed on the accelerator its FILE NAME names, and only there: the record it can no longer
// prove is that the leftover on that accelerator is unowned, so adopting it could put a second Pod on a
// partition another Pod still holds. Everything the missing record does not bear on is unaffected —
// a fresh create in a free slot on the same accelerator (occupancy comes from the driver, not from
// markers) and adoption on a sibling accelerator. A corrupt name that yields no accelerator fails closed
// everywhere, because the scope of what is unknown is itself unknown.
func TestReserveMigInstance_CorruptMarkerFailsAdoptionClosed(t *testing.T) {
	const profile = "1g.10gb"
	// leftoverGiID/leftoverSlot describe the unmarked, geometry-matching leftover a healthy scan
	// would adopt (a crashed create or an out-of-band tool left it behind).
	const leftoverGiID = uint32(7)
	leftoverSlot := migPlacement{0, 2}

	cases := []struct {
		name string
		// corruptFile is the marker file name written unparseable under a dead pod's work dir.
		corruptFile string
		// mismatchedPod, when set, owns a marker that is complete and parseable but records an accelerator its
		// own file name does not — the other way an ownership set becomes unprovable.
		mismatchedPod string
		// seedLeftover seeds the adoptable leftover on the requested accelerator.
		seedLeftover  bool
		wantOutcome   migReserveOutcome
		wantCreates   int
		wantPlacement migPlacement
	}{
		{
			name:          "refuses adopting a leftover on the accelerator the corrupt marker names",
			corruptFile:   markerFileName(testGPUUUID0),
			seedLeftover:  true,
			wantOutcome:   migCreated,
			wantCreates:   1,
			wantPlacement: migPlacement{2, 2}, // the unadopted leftover still occupies slot 0
		},
		{
			name:          "a fresh create in a free slot on that same accelerator still succeeds",
			corruptFile:   markerFileName(testGPUUUID0),
			wantOutcome:   migCreated,
			wantCreates:   1,
			wantPlacement: migPlacement{0, 2},
		},
		{
			name:          "adoption on a sibling accelerator is unaffected",
			corruptFile:   markerFileName(testGPUUUID1),
			seedLeftover:  true,
			wantOutcome:   migBound,
			wantCreates:   0,
			wantPlacement: leftoverSlot,
		},
		{
			// A record that parses and carries every required field, but names an accelerator its own file name
			// does not: grouping the ownership set by the recorded accelerator alone would leave the leftover
			// looking unowned and hand it to this second Pod, putting two Pods on one MIG partition. It
			// must instead count as unreadable ownership on the accelerator its file belongs to.
			name:          "refuses adopting a leftover while a marker's record disagrees with its file name",
			mismatchedPod: "pod-mismatch",
			seedLeftover:  true,
			wantOutcome:   migCreated,
			wantCreates:   1,
			wantPlacement: migPlacement{2, 2},
		},
		{
			name:          "a corrupt name yielding no accelerator refuses adoption everywhere",
			corruptFile:   markerFileName(""), // nvidia-mig-.json: a marker file naming no card
			seedLeftover:  true,
			wantOutcome:   migCreated,
			wantCreates:   1,
			wantPlacement: migPlacement{2, 2},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			drv.possible[testGPUUUID0] = evenSlots()
			if c.seedLeftover {
				drv.seedLive(testGPUUUID0, migInstance{
					GiID: leftoverGiID, CiID: leftoverGiID, ComputeSlices: 1,
					Placement: leftoverSlot, UUID: "MIG-reuse",
				})
			}
			if c.corruptFile != "" {
				writeCorruptMarker(t, deviceplugin.PodWorkDir("pod-crash", "c"), c.corruptFile)
			}
			if c.mismatchedPod != "" {
				// Written at the requested accelerator's own path, but recording the sibling accelerator.
				require.NoError(t, writeMarker(markerPath(c.mismatchedPod, "c", testGPUUUID0), migMarker{
					PodUID: c.mismatchedPod, Container: "c", Card: testGPUUUID1, Profile: profile,
					GiID: leftoverGiID, CiID: leftoverGiID, MigUUID: "MIG-reuse",
					ComputeSlices: 1, Start: leftoverSlot.Start, Length: leftoverSlot.Length,
				}))
			}

			inst, outcome, err := reserveMigInstance(
				drv, deviceplugin.OperatorPodsDir, "pod-new", "c", testGPUUUID0, profile, 1, 2)
			require.NoError(t, err, "a corrupt marker never fails the allocation outright")
			assert.Equal(t, c.wantOutcome, outcome)
			assert.Equal(t, c.wantCreates, drv.createCalls)
			assert.Equal(t, c.wantPlacement, inst.Placement)
			assert.NotEmpty(t, inst.UUID)
			if c.wantOutcome == migBound {
				assert.Equal(t, leftoverGiID, inst.GiID, "the sibling card's leftover is adopted")
			} else if c.seedLeftover {
				assert.NotEqual(t, leftoverGiID, inst.GiID, "the leftover is not adopted")
			}
		})
	}
}

func TestReserveMigInstance_IdempotentRetry(t *testing.T) {
	const profile = "1g.10gb"
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()

	first, outcome, err := reserveMigInstance(
		drv, deviceplugin.OperatorPodsDir, "pod-a", "c", testGPUUUID0, profile, 1, 2)
	require.NoError(t, err)
	require.Equal(t, migCreated, outcome)

	// A kubelet Allocate retry rebinds the same instance rather than double-creating.
	second, outcome, err := reserveMigInstance(
		drv, deviceplugin.OperatorPodsDir, "pod-a", "c", testGPUUUID0, profile, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, migRebound, outcome)
	assert.Equal(t, first.GiID, second.GiID)
	assert.Equal(t, 1, drv.createCalls, "no second create on retry")
}

func TestReserveMigInstance_SelfMarkerStale(t *testing.T) {
	const profile = "1g.10gb"
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	// A marker whose GPU instance is gone from the live set (out-of-band destroy): fail closed
	// rather than rebind a missing partition.
	require.NoError(t, writeMarker(markerPath("pod-a", "c", testGPUUUID0), migMarker{
		PodUID: "pod-a", Container: "c", Card: testGPUUUID0, Profile: profile,
		GiID: 42, CiID: 42, MigUUID: "MIG-gone", ComputeSlices: 1, Start: 0, Length: 2,
	}))

	_, _, err := reserveMigInstance(
		drv, deviceplugin.OperatorPodsDir, "pod-a", "c", testGPUUUID0, profile, 1, 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing gpu instance")
}

func TestReserveMigInstance_ProfileMismatch(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	require.NoError(t, writeMarker(markerPath("pod-a", "c", testGPUUUID0), migMarker{
		PodUID: "pod-a", Container: "c", Card: testGPUUUID0, Profile: "2g.20gb",
		GiID: 1, CiID: 1, MigUUID: "MIG-other", ComputeSlices: 2, Start: 0, Length: 4,
	}))

	_, _, err := reserveMigInstance(
		drv, deviceplugin.OperatorPodsDir, "pod-a", "c", testGPUUUID0, "1g.10gb", 1, 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mismatches request")
}

// TestReserveMigInstance_ConcurrentSameCard asserts that concurrent same-accelerator reservations,
// serialized by the per-accelerator lock, resolve to distinct non-overlapping slots with no double-
// create, while a sibling accelerator proceeds in parallel.
func TestReserveMigInstance_ConcurrentSameCard(t *testing.T) {
	const profile = "1g.10gb"
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	drv.possible[testGPUUUID1] = evenSlots()

	const n = 4
	var wg sync.WaitGroup
	starts := make([]int32, n)
	sibling := make(chan migPlacement, 1)

	// One reservation races on the sibling accelerator; it must not be blocked by the contended one.
	wg.Add(1)
	go func() {
		defer wg.Done()
		unlock := lockCard(testGPUUUID1)
		defer unlock()
		inst, _, err := reserveMigInstance(
			drv, deviceplugin.OperatorPodsDir, "pod-sib", "c", testGPUUUID1, profile, 1, 2)
		require.NoError(t, err)
		sibling <- inst.Placement
	}()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock := lockCard(testGPUUUID0)
			defer unlock()
			inst, outcome, err := reserveMigInstance(
				drv, deviceplugin.OperatorPodsDir, fmt.Sprintf("pod-%d", i), "c", testGPUUUID0, profile, 1, 2)
			require.NoError(t, err)
			require.Equal(t, migCreated, outcome)
			starts[i] = inst.Placement.Start
		}(i)
	}
	wg.Wait()

	assert.Equal(t, n+1, drv.createCalls, "one create per distinct pod (n on the card + 1 sibling), no double-create")
	got := append([]int32(nil), starts...)
	slices.Sort(got)
	assert.Equal(t, []int32{0, 2, 4, 6}, got, "distinct non-overlapping slots")

	select {
	case p := <-sibling:
		assert.Equal(t, int32(0), p.Start, "sibling card allocates independently")
	default:
		t.Fatal("sibling card reservation did not complete")
	}
}

// migDevices builds a MIG-capable fixture: one group whose accelerators each carry the given
// physical-slice profile geometry (compute/memory slices), so profileGeometry resolves it.
func migDevices(profile string, computeSlices, memorySlices int32, uuids ...string) *workercore.Devices {
	accels := make([]workercore.Accelerator, len(uuids))
	for i, u := range uuids {
		accels[i] = workercore.Accelerator{
			ID:    u,
			Index: uint32(i),
			Status: workercore.AcceleratorStatus{
				PhysicalSliced: workercore.AcceleratorPhysicalSliced{
					Profiles: []workercore.AcceleratorPhysicalSlicedProfile{{
						Name:          profile,
						ComputeSlices: computeSlices,
						MemorySlices:  memorySlices,
						Count:         4,
					}},
					Count: 4,
				},
			},
		}
	}
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "h100",
				Manufacturer: Manufacturer,
				Accelerators: accels,
			}},
		},
	}
}

func TestActuatePhysicalSliced(t *testing.T) {
	const profile = "1g.10gb"
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	drv.possible[testGPUUUID1] = evenSlots()
	s := &server{mig: drv}

	devs := migDevices(profile, 1, 2, testGPUUUID0, testGPUUUID1)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", UID: types.UID("pod-1")}}
	ctr := &core.Container{Name: "c"}
	allocated := map[deviceplugin.Resource]int32{
		{Group: "h100", Device: testGPUUUID0}: 200000,
		{Group: "h100", Device: testGPUUUID1}: 200000,
	}

	out, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr, devs, allocated, profile)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, profile, out.Profile)

	// One placement recorded per accelerator (the ledger's occupied source).
	require.Len(t, out.Placements, 2)
	for _, res := range []deviceplugin.Resource{
		{Group: "h100", Device: testGPUUUID0},
		{Group: "h100", Device: testGPUUUID1},
	} {
		require.Len(t, out.Placements[res], 1)
		assert.Equal(t, int32(0), out.Placements[res][0].Start)
		assert.Equal(t, int32(2), out.Placements[res][0].Length)
	}

	// The response injects only the MIG UUIDs (no libvgpu / CUDA_DEVICE_* logical-slice env).
	require.NotNil(t, out.Response)
	vis := out.Response.Envs["NVIDIA_VISIBLE_DEVICES"]
	assert.Contains(t, vis, "MIG-"+testGPUUUID0)
	assert.Contains(t, vis, "MIG-"+testGPUUUID1)
	assert.NotContains(t, out.Response.Envs, "CUDA_DEVICE_MEMORY_LIMIT_0")

	// A marker is written per accelerator.
	for _, card := range []string{testGPUUUID0, testGPUUUID1} {
		_, err := parseMarker(markerPath("pod-1", "c", card))
		require.NoError(t, err, "marker for card %s", card)
	}
}

func TestActuatePhysicalSliced_RollbackDestroysCreated(t *testing.T) {
	const profile = "1g.10gb"
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	s := &server{mig: drv}

	devs := migDevices(profile, 1, 2, testGPUUUID0)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", UID: types.UID("pod-1")}}
	ctr := &core.Container{Name: "c"}
	allocated := map[deviceplugin.Resource]int32{{Group: "h100", Device: testGPUUUID0}: 200000}

	out, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr, devs, allocated, profile)
	require.NoError(t, err)
	require.Len(t, drv.destroyed, 0)

	out.Rollback()
	require.Len(t, drv.destroyed, 1, "rollback destroys the created instance")
	_, statErr := parseMarker(markerPath("pod-1", "c", testGPUUUID0))
	require.Error(t, statErr, "rollback removes the marker")
}

// TestActuatePhysicalSliced_RollbackReusedNotDestroyed asserts rollback of an allocation that
// adopted a pre-existing unbound instance drops only the ownership marker (returning it to the
// unbound pool) and never destroys hardware this call did not create.
func TestActuatePhysicalSliced_RollbackReusedNotDestroyed(t *testing.T) {
	const profile = "1g.10gb"
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	// A reusable unbound instance (has a UUID, no marker owns it).
	drv.seedLive(testGPUUUID0, migInstance{GiID: 5, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-pre"})
	s := &server{mig: drv}

	devs := migDevices(profile, 1, 2, testGPUUUID0)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", UID: types.UID("pod-1")}}
	ctr := &core.Container{Name: "c"}
	allocated := map[deviceplugin.Resource]int32{{Group: "h100", Device: testGPUUUID0}: 200000}

	out, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr, devs, allocated, profile)
	require.NoError(t, err)
	assert.Equal(t, 0, drv.createCalls, "the pre-existing instance is reused")

	out.Rollback()
	assert.Len(t, drv.destroyed, 0, "rollback must not destroy an adopted instance")
	_, statErr := parseMarker(markerPath("pod-1", "c", testGPUUUID0))
	require.Error(t, statErr, "rollback drops the ownership marker")
}

func TestActuatePhysicalSliced_UnknownProfileFails(t *testing.T) {
	redirectLogicalSliceDirs(t)
	drv := newFakeMigDriver()
	drv.possible[testGPUUUID0] = evenSlots()
	s := &server{mig: drv}

	devs := migDevices("1g.10gb", 1, 2, testGPUUUID0)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", UID: types.UID("pod-1")}}
	ctr := &core.Container{Name: "c"}
	allocated := map[deviceplugin.Resource]int32{{Group: "h100", Device: testGPUUUID0}: 200000}

	_, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr, devs, allocated, "3g.20gb")
	require.Error(t, err)
	assert.Equal(t, 0, drv.createCalls, "an unknown profile geometry never reaches create")
}

// The record is written for its writer alone while the directory holding it stays traversable, and
// the two are easy to conflate: they sit two lines apart and the wide one is load-bearing for the
// logical-slicing artifacts every other allocator puts in the same place. Pin both.
func TestWriteMarkerModes(t *testing.T) {
	redirectLogicalSliceDirs(t)
	m := migMarker{
		PodUID: "pod-a", Container: "c", Card: testGPUUUID0, Profile: "1g.5gb",
		GiID: 1, CiID: 0, MigUUID: "MIG-0", ComputeSlices: 1, Start: 0, Length: 1,
	}
	path := markerPath(m.PodUID, m.Container, m.Card)
	require.NoError(t, writeMarker(path, m))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"nothing outside this process reads the record, so nothing outside it may")

	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o777), dirInfo.Mode().Perm(),
		"the per-container work directory is shared with artifacts a container reads as its own user")
}

// The marker's JSON tags are an on-disk format: markers written before the accelerator-vocabulary
// rename exist on running nodes, and a tag that moved would make every one of them unreadable,
// breaking retry, visibility, adoption and reclamation. Every other test in this package writes and
// reads through the same struct, so all of them would stay green through a coordinated tag rename.
// This one does not: it feeds a literal pre-rename document and asserts the exact wire keys come
// back out.
func TestMigMarker_LegacyJSONRoundTrip(t *testing.T) {
	// legacyKeys is the complete on-disk key set, spelled literally rather than derived from the
	// struct, so a renamed tag fails here instead of being mirrored into the expectation.
	legacyKeys := []string{
		"podUID", "container", "card", "profile",
		"giID", "ciID", "migUUID", "computeSlices", "start", "length",
	}

	cases := []struct {
		name string
		doc  string
		want migMarker
	}{
		{
			name: "a complete pre-rename record",
			doc: `{"podUID":"pod-a","container":"c0","card":"GPU-aaaa0000-0000-0000-0000-000000000000",` +
				`"profile":"1g.10gb","giID":3,"ciID":1,"migUUID":"MIG-legacy-0",` +
				`"computeSlices":1,"start":2,"length":2}`,
			want: migMarker{
				PodUID: "pod-a", Container: "c0", Card: "GPU-aaaa0000-0000-0000-0000-000000000000",
				Profile: "1g.10gb", GiID: 3, CiID: 1, MigUUID: "MIG-legacy-0",
				ComputeSlices: 1, Start: 2, Length: 2,
			},
		},
		{
			name: "the zero-valued numeric fields of a slot-0 record",
			doc: `{"podUID":"pod-b","container":"c1","card":"GPU-bbbb0000-0000-0000-0000-000000000000",` +
				`"profile":"7g.80gb","giID":0,"ciID":0,"migUUID":"MIG-legacy-1",` +
				`"computeSlices":7,"start":0,"length":8}`,
			want: migMarker{
				PodUID: "pod-b", Container: "c1", Card: "GPU-bbbb0000-0000-0000-0000-000000000000",
				Profile: "7g.80gb", GiID: 0, CiID: 0, MigUUID: "MIG-legacy-1",
				ComputeSlices: 7, Start: 0, Length: 8,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got migMarker
			require.NoError(t, json.Unmarshal([]byte(c.doc), &got),
				"a pre-rename marker must still parse")
			assert.Equal(t, c.want, got, "every identity a pre-rename marker records must survive")

			out, err := json.Marshal(got)
			require.NoError(t, err)

			var keyed map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(out, &keyed))
			assert.ElementsMatch(t, legacyKeys, slices.Collect(maps.Keys(keyed)),
				"the marshalled record must carry exactly the legacy keys")
			assert.NotContains(t, keyed, "accelerator",
				"the vocabulary rename must not have reached the on-disk format")
		})
	}
}
