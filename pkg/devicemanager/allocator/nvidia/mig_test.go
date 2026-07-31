package nvidia

import (
	"context"
	"fmt"
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
// per-card possible-placement set and live-instance list, so the marker/slot-pick/reuse core
// is table-tested without NVML. It is concurrency-safe (the reclaim/actuator holds the
// per-card lock, but different cards run in parallel and Go maps are not).
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
	listErr    error
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

// ListInstances returns every seeded live instance across all cards (the reclaim orphan-GC seam).
func (f *fakeMigDriver) ListInstances() ([]migLiveInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// seedLive appends a live instance to a card (an out-of-band / reusable partition).
func (f *fakeMigDriver) seedLive(cardUUID string, inst migInstance) {
	f.live[cardUUID] = append(f.live[cardUUID], inst)
}

// evenSlots returns the memory-slice placements of a 2-slice profile on an 8-slice card
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
			name:     "empty card picks lowest",
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
			name:     "full card yields nothing",
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

	t.Run("second card of a new pod picks the next free slot", func(t *testing.T) {
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

	t.Run("full card fails without creating", func(t *testing.T) {
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

// TestReserveMigInstance_ConcurrentSameCard asserts that concurrent same-card reservations,
// serialized by the per-card lock, resolve to distinct non-overlapping slots with no double-
// create, while a sibling card proceeds in parallel.
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

	// One reservation races on the sibling card; it must not be blocked by the contended card.
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

	// One placement recorded per card (the ledger's occupied source).
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

	// A marker is written per card.
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
