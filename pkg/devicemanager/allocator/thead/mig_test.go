package thead

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

const (
	testPPUUUID0 = "PPU-aaaa0000-0000-0000-0000-000000000000"
	testPPUUUID1 = "PPU-bbbb1111-1111-1111-1111-111111111111"

	// testProfile is a 1-compute-slice / 2-memory-slice profile, and testProfileID the raw vendor
	// id it resolves to. testOtherProfileID is a different raw profile of the same geometry — the
	// media/graphics variant adoption must refuse.
	testProfile        = "1c.10g"
	testProfileID      = uint32(3)
	testOtherProfileID = uint32(9)
)

// fakeMigDriver is an in-memory migDriver: it records create/destroy calls and holds a per-card
// possible-placement set, resolved profile id and live-instance list, so the marker/slot-pick/
// adoption core is table-tested without the vendor library. It is concurrency-safe (the caller
// holds the per-card lock, but different cards run in parallel and Go maps are not).
type fakeMigDriver struct {
	mu sync.Mutex
	// possible and profileIDs are keyed by card and profile name, mirroring the seam's contract
	// that a profile is resolved to a raw vendor id by name, per card.
	possible   map[string][]migPlacement
	profileIDs map[string]uint32
	live       map[string][]migInstance

	nextGiID    uint32
	createCalls int
	// stateCalls and listCalls count the per-card and the node-wide enumerations. They are the cost
	// side of the seam contract: the node-wide one probes every card's whole profile space, so a loop
	// that calls it once per marker instead of once per card is a defect the counts make visible.
	stateCalls int
	listCalls  int
	// cardStateErr, createErr and listErr inject the enumeration/actuation failures the seam
	// contract requires to be errors rather than partial state.
	cardStateErr error
	createErr    error
	listErr      error
	destroyed    []migInstance
	// inUseGiIDs marks GPU-instance ids whose DestroyInstance fails with errInstanceInUse (a
	// residual process holding the instance).
	inUseGiIDs map[uint32]bool
}

func newFakeMigDriver() *fakeMigDriver {
	return &fakeMigDriver{
		possible:   make(map[string][]migPlacement),
		profileIDs: make(map[string]uint32),
		live:       make(map[string][]migInstance),
	}
}

func (f *fakeMigDriver) CardState(cardUUID, profile string, _, _ int32) (migCardState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateCalls++
	if f.cardStateErr != nil {
		return migCardState{}, f.cardStateErr
	}
	key := cardUUID + "/" + profile
	return migCardState{
		ProfileID: f.profileIDs[key],
		Possible:  append([]migPlacement(nil), f.possible[key]...),
		Live:      append([]migInstance(nil), f.live[cardUUID]...),
	}, nil
}

func (f *fakeMigDriver) CreateInstance(cardUUID, profile string, computeSlices, _ int32, slot migPlacement) (migInstance, error) {
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
		ProfileID:     f.profileIDs[cardUUID+"/"+profile],
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
	kept := make([]migInstance, 0, len(f.live[cardUUID]))
	for _, l := range f.live[cardUUID] {
		if l.GiID != inst.GiID {
			kept = append(kept, l)
		}
	}
	f.live[cardUUID] = kept
	return nil
}

// ListInstances returns every seeded live instance across all cards (the orphan-collection seam).
func (f *fakeMigDriver) ListInstances() ([]migLiveInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
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

// seedCard makes the card offer testProfile at its raw id, with the placements of a 2-slice profile
// on an 8-slice card.
func (f *fakeMigDriver) seedCard(cardUUID string) {
	f.seedProfile(cardUUID, testProfile, testProfileID, evenSlots())
}

// seedProfile makes the card offer one more profile, so a request naming it resolves to its own raw
// id and its own legal placement set.
func (f *fakeMigDriver) seedProfile(cardUUID, profile string, profileID uint32, possible []migPlacement) {
	key := cardUUID + "/" + profile
	f.profileIDs[key] = profileID
	f.possible[key] = possible
}

// seedLive appends a live instance to a card (an out-of-band or adoptable leftover).
func (f *fakeMigDriver) seedLive(cardUUID string, inst migInstance) {
	f.live[cardUUID] = append(f.live[cardUUID], inst)
}

// evenSlots returns the memory-slice placements of a 2-slice profile on an 8-slice card
// ([0,2),[2,2),[4,2),[6,2)) — the legal set used across the tests.
func evenSlots() []migPlacement {
	return []migPlacement{{0, 2}, {2, 2}, {4, 2}, {6, 2}}
}

// selfMarker builds the ownership marker a pod's own prior allocation would have written.
func selfMarker(podUID, cardUUID string, inst migInstance) migMarker {
	return migMarker{
		PodUID: podUID, Container: "c", Card: cardUUID, Profile: testProfile, ProfileID: inst.ProfileID,
		GiID: inst.GiID, CiID: inst.CiID, MigUUID: inst.UUID,
		ComputeSlices: inst.ComputeSlices, Start: inst.Placement.Start, Length: inst.Placement.Length,
	}
}

// writeMarkerFixture publishes a marker for a test, failing the test if the setup cannot be built.
func writeMarkerFixture(t *testing.T, podsDir string, m migMarker) {
	t.Helper()
	require.NoError(t, writeMarker(markerPath(podsDir, m.PodUID, m.Container, m.Card), m))
}

// theadDevices builds a partition-capable fixture: one group whose accelerators each carry the
// given profile geometry, so profileGeometry resolves it.
func theadDevices(profile string, computeSlices, memorySlices int32, uuids ...string) *workercore.Devices {
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
				ID:           "ppu",
				Manufacturer: Manufacturer,
				Accelerators: accels,
			}},
		},
	}
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
			occupied: []migPlacement{{0, 4}},
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
	errEnumeration := errors.New("card enumeration incomplete")
	errCreate := errors.New("create rejected")

	cases := []struct {
		name string
		// setup seeds the driver and the marker root; the card under test is always testPPUUUID0.
		setup  func(t *testing.T, drv *fakeMigDriver, podsDir string)
		podUID string
		// wantErr is the substring the reservation must fail with; keepsSelfMarker marks the cases
		// whose failure is a pre-existing self marker, which the failure must leave in place.
		wantErr         string
		keepsSelfMarker bool
		wantOutcome     migReserveOutcome
		wantCreateCalls int
		wantPlacement   migPlacement
		wantGiID        uint32
		wantUUID        string
	}{
		{
			name:            "creates at the lowest free placement",
			podUID:          "pod-a",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{0, 2},
			wantGiID:        1,
		},
		{
			name: "another pod's partition holds the lowest slot",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				inst := migInstance{
					GiID: 100, CiID: 100, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-old",
				}
				drv.seedLive(testPPUUUID0, inst)
				writeMarkerFixture(t, podsDir, selfMarker("pod-old", testPPUUUID0, inst))
			},
			podUID:          "pod-b",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{2, 2},
			wantGiID:        1,
		},
		{
			name: "a retry rebinds its own live partition",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				inst := migInstance{
					GiID: 5, CiID: 5, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-self",
				}
				drv.seedLive(testPPUUUID0, inst)
				writeMarkerFixture(t, podsDir, selfMarker("pod-c", testPPUUUID0, inst))
			},
			podUID:        "pod-c",
			wantOutcome:   migRebound,
			wantPlacement: migPlacement{0, 2},
			wantGiID:      5,
			wantUUID:      "MIG-self",
		},
		{
			name: "a self-marker whose partition is gone fails closed",
			setup: func(t *testing.T, _ *fakeMigDriver, podsDir string) {
				writeMarkerFixture(t, podsDir, selfMarker("pod-d", testPPUUUID0, migInstance{
					GiID: 42, CiID: 42, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-gone",
				}))
			},
			podUID:          "pod-d",
			wantErr:         "missing gpu instance",
			keepsSelfMarker: true,
		},
		{
			name: "a self-marker of another profile fails closed",
			setup: func(t *testing.T, _ *fakeMigDriver, podsDir string) {
				m := selfMarker("pod-e", testPPUUUID0, migInstance{
					GiID: 1, CiID: 1, ProfileID: testOtherProfileID, ComputeSlices: 2,
					Placement: migPlacement{0, 4}, UUID: "MIG-other",
				})
				m.Profile = "2c.20g"
				writeMarkerFixture(t, podsDir, m)
			},
			podUID:          "pod-e",
			wantErr:         "mismatches request",
			keepsSelfMarker: true,
		},
		{
			name: "a reused gpu-instance id fails closed",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				// The live instance 5 is now a different partition than the marker recorded.
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 5, CiID: 5, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-new",
				})
				writeMarkerFixture(t, podsDir, selfMarker("pod-f", testPPUUUID0, migInstance{
					GiID: 5, CiID: 5, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-old",
				}))
			},
			podUID:          "pod-f",
			wantErr:         "id reused",
			keepsSelfMarker: true,
		},
		{
			name: "adopts an unowned leftover of the same raw profile",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 7, CiID: 7, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{4, 2}, UUID: "MIG-leftover",
				})
			},
			podUID:        "pod-g",
			wantOutcome:   migBound,
			wantPlacement: migPlacement{4, 2},
			wantGiID:      7,
			wantUUID:      "MIG-leftover",
		},
		{
			name: "refuses a leftover of the same geometry but another raw profile",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 8, CiID: 8, ProfileID: testOtherProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-variant",
				})
			},
			podUID:          "pod-h",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{2, 2},
			wantGiID:        1,
		},
		{
			name: "refuses a leftover of the same raw profile whose shape disagrees",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 11, CiID: 11, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 4}, UUID: "MIG-inconsistent",
				})
			},
			podUID:          "pod-i",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{4, 2},
			wantGiID:        1,
		},
		{
			name: "refuses a leftover with no partition identity",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 9, CiID: 0, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "",
				})
			},
			podUID:          "pod-j",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{2, 2},
			wantGiID:        1,
		},
		{
			name: "refuses adoption while a marker of that card is unparseable",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 12, CiID: 12, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-maybe-owned",
				})
				// A corrupt marker naming this card: its owner is unknowable, so the leftover
				// above may already be bound and must not be adopted.
				path := markerPath(podsDir, "pod-unknown", "c", testPPUUUID0)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
			},
			podUID:          "pod-k",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{2, 2},
			wantGiID:        1,
		},
		{
			name: "refuses adoption while an unparseable marker names no card at all",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 14, CiID: 14, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-maybe-owned",
				})
				// This card's own markers all parse, but a corrupt file naming no card may stand for a
				// record of any card — including this one — so the leftover above cannot be proven
				// unbound. Capacity is unaffected: the create below still takes a free slot.
				path := markerPath(podsDir, "pod-unknown", "c", "")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
			},
			podUID:          "pod-q",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{2, 2},
			wantGiID:        1,
		},
		{
			// The marker parses, carries every required field, and owns the leftover — but it names a
			// card its own file name does not. Grouping the ownership set by the recorded card alone
			// would leave the leftover looking unowned and hand it to this second Pod, putting two Pods
			// on one hardware partition. It must instead count as unreadable ownership on the card its
			// file belongs to, which refuses the adoption.
			name: "refuses adoption while a marker's record disagrees with its own file name",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				inst := migInstance{
					GiID: 15, CiID: 15, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-owned-elsewhere",
				}
				drv.seedLive(testPPUUUID0, inst)
				m := selfMarker("pod-mismatch", testPPUUUID1, inst)
				require.NoError(t, writeMarker(markerPath(podsDir, m.PodUID, m.Container, testPPUUUID0), m))
			},
			podUID:          "pod-r",
			wantOutcome:     migCreated,
			wantCreateCalls: 1,
			wantPlacement:   migPlacement{2, 2},
			wantGiID:        1,
		},
		{
			name: "a corrupt marker of another card does not block adoption",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 13, CiID: 13, ProfileID: testProfileID, ComputeSlices: 1,
					Placement: migPlacement{0, 2}, UUID: "MIG-adoptable",
				})
				path := markerPath(podsDir, "pod-unknown", "c", testPPUUUID1)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
			},
			podUID:        "pod-l",
			wantOutcome:   migBound,
			wantPlacement: migPlacement{0, 2},
			wantGiID:      13,
			wantUUID:      "MIG-adoptable",
		},
		{
			name: "a full card fails without creating",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.seedLive(testPPUUUID0, migInstance{
					GiID: 1, CiID: 1, ProfileID: testOtherProfileID, ComputeSlices: 7,
					Placement: migPlacement{0, 8}, UUID: "MIG-whole",
				})
			},
			podUID:  "pod-m",
			wantErr: "no free placement",
		},
		{
			name: "a self marker that cannot be read at all fails closed",
			setup: func(t *testing.T, _ *fakeMigDriver, podsDir string) {
				// A regular file where the pod work directory belongs: the marker is neither
				// present nor absent, so the reservation must not treat it as a fresh allocation.
				require.NoError(t, os.WriteFile(filepath.Join(podsDir, "pod-p"), nil, 0o600))
			},
			podUID:  "pod-p",
			wantErr: "read self marker",
		},
		{
			name: "an incomplete card enumeration is an error, not an empty card",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.cardStateErr = errEnumeration
			},
			podUID:  "pod-n",
			wantErr: "card enumeration incomplete",
		},
		{
			name: "a failed create is returned",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.createErr = errCreate
			},
			podUID:          "pod-o",
			wantErr:         "create rejected",
			wantCreateCalls: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			podsDir := t.TempDir()
			drv := newFakeMigDriver()
			drv.seedCard(testPPUUUID0)
			if c.setup != nil {
				c.setup(t, drv, podsDir)
			}

			inst, outcome, err := reserveMigInstance(
				drv, podsDir, c.podUID, "c", testPPUUUID0, testProfile, 1, 2)

			assert.Equal(t, c.wantCreateCalls, drv.createCalls)
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				if !c.keepsSelfMarker {
					_, merr := parseMarker(markerPath(podsDir, c.podUID, "c", testPPUUUID0))
					assert.Error(t, merr, "a failed reservation writes no ownership marker")
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, c.wantOutcome, outcome)
			assert.Equal(t, c.wantPlacement, inst.Placement)
			assert.Equal(t, c.wantGiID, inst.GiID)
			assert.Equal(t, testProfileID, inst.ProfileID)
			if c.wantUUID != "" {
				assert.Equal(t, c.wantUUID, inst.UUID)
			}
			assert.NotEmpty(t, inst.UUID)

			// The marker names the reserved partition, so reclaim and the visibility responder can
			// find it after a restart.
			m, err := parseMarker(markerPath(podsDir, c.podUID, "c", testPPUUUID0))
			require.NoError(t, err)
			assert.Equal(t, testProfile, m.Profile)
			assert.Equal(t, testProfileID, m.ProfileID)
			assert.Equal(t, inst.GiID, m.GiID)
			assert.Equal(t, inst.CiID, m.CiID)
			assert.Equal(t, inst.UUID, m.MigUUID)
			assert.Equal(t, inst.Placement, migPlacement{Start: m.Start, Length: m.Length})
			assert.Equal(t, inst, m.instance())
		})
	}
}

// TestReserveMigInstanceIdempotentRetry asserts a kubelet Allocate retry rebinds the partition the
// first call created instead of carving a second one.
func TestReserveMigInstanceIdempotentRetry(t *testing.T) {
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)

	first, outcome, err := reserveMigInstance(drv, podsDir, "pod-a", "c", testPPUUUID0, testProfile, 1, 2)
	require.NoError(t, err)
	require.Equal(t, migCreated, outcome)

	second, outcome, err := reserveMigInstance(drv, podsDir, "pod-a", "c", testPPUUUID0, testProfile, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, migRebound, outcome)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, drv.createCalls, "no second create on retry")
}

// TestReserveMigInstanceSecondProfile asserts a request naming another profile the card offers is
// carved from that profile's own raw id and its own legal placement set, and that a partition
// already carved from the first profile only removes the slots it occupies.
func TestReserveMigInstanceSecondProfile(t *testing.T) {
	const wideProfile = "2c.20g"
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	drv.seedProfile(testPPUUUID0, wideProfile, testOtherProfileID, []migPlacement{{0, 4}, {4, 4}})
	drv.seedLive(testPPUUUID0, migInstance{
		GiID: 20, CiID: 20, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-narrow",
	})

	inst, outcome, err := reserveMigInstance(drv, podsDir, "pod-a", "c", testPPUUUID0, wideProfile, 2, 4)
	require.NoError(t, err)
	assert.Equal(t, migCreated, outcome, "a narrow leftover of another profile is not adoptable")
	assert.Equal(t, migPlacement{4, 4}, inst.Placement, "the slots the narrow partition occupies are unavailable")
	assert.Equal(t, testOtherProfileID, inst.ProfileID)

	m, err := parseMarker(markerPath(podsDir, "pod-a", "c", testPPUUUID0))
	require.NoError(t, err)
	assert.Equal(t, wideProfile, m.Profile)
	assert.Equal(t, testOtherProfileID, m.ProfileID)
}

// TestReserveMigInstancePerContainer asserts the ownership record is per container, not per pod: two
// partitioned containers of one pod on one card each carve their own partition and keep their own
// marker, so one container's teardown cannot free the other's.
func TestReserveMigInstancePerContainer(t *testing.T) {
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)

	first, outcome, err := reserveMigInstance(drv, podsDir, "pod-a", "main", testPPUUUID0, testProfile, 1, 2)
	require.NoError(t, err)
	require.Equal(t, migCreated, outcome)

	second, outcome, err := reserveMigInstance(drv, podsDir, "pod-a", "worker", testPPUUUID0, testProfile, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, migCreated, outcome, "a sibling container is not the marker's owner")
	assert.Equal(t, 2, drv.createCalls)
	assert.NotEqual(t, first.GiID, second.GiID)
	assert.Equal(t, migPlacement{0, 2}, first.Placement)
	assert.Equal(t, migPlacement{2, 2}, second.Placement)

	for container, want := range map[string]migInstance{"main": first, "worker": second} {
		m, perr := parseMarker(markerPath(podsDir, "pod-a", container, testPPUUUID0))
		require.NoError(t, perr, "marker of container %q", container)
		assert.Equal(t, want, m.instance())
	}
}

// TestReserveMigInstanceMarkerWriteFailure asserts the marker-write error path undoes exactly what
// the call did: a partition it created is destroyed, one it adopted is left alone.
func TestReserveMigInstanceMarkerWriteFailure(t *testing.T) {
	leftover := migInstance{
		GiID: 4, CiID: 4, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-leftover",
	}
	cases := []struct {
		name         string
		seedLeftover bool
		wantOutcome  migReserveOutcome
		wantDestroys int
	}{
		{
			name:         "a created partition is destroyed",
			wantOutcome:  migCreated,
			wantDestroys: 1,
		},
		{
			name:         "an adopted partition is not destroyed",
			seedLeftover: true,
			wantOutcome:  migBound,
		},
	}
	if os.Geteuid() == 0 {
		t.Skip("the marker write is failed by a directory permission, which root ignores")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The pod directory exists but cannot be written, so the container work directory the
			// marker lives in cannot be created while the absent marker still reads as absent.
			podsDir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(podsDir, "pod-a"), 0o500))

			drv := newFakeMigDriver()
			drv.seedCard(testPPUUUID0)
			if c.seedLeftover {
				drv.seedLive(testPPUUUID0, leftover)
			}

			_, outcome, err := reserveMigInstance(drv, podsDir, "pod-a", "c", testPPUUUID0, testProfile, 1, 2)
			require.Error(t, err)
			assert.Equal(t, c.wantOutcome, outcome, "the outcome tells the caller's rollback what to undo")
			assert.Len(t, drv.destroyed, c.wantDestroys)
		})
	}
}

// TestReserveMigInstanceConcurrentSameCard asserts concurrent same-card reservations, serialized by
// the per-card lock, resolve to distinct non-overlapping slots with no double-create, while a
// sibling card proceeds in parallel.
func TestReserveMigInstanceConcurrentSameCard(t *testing.T) {
	podsDir := t.TempDir()
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	drv.seedCard(testPPUUUID1)

	const n = 4
	var wg sync.WaitGroup
	starts := make([]int32, n)
	sibling := make(chan migPlacement, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		unlock := lockCard(testPPUUUID1)
		defer unlock()
		inst, _, err := reserveMigInstance(drv, podsDir, "pod-sib", "c", testPPUUUID1, testProfile, 1, 2)
		require.NoError(t, err)
		sibling <- inst.Placement
	}()

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock := lockCard(testPPUUUID0)
			defer unlock()
			inst, outcome, err := reserveMigInstance(
				drv, podsDir, fmt.Sprintf("pod-%d", i), "c", testPPUUUID0, testProfile, 1, 2)
			require.NoError(t, err)
			require.Equal(t, migCreated, outcome)
			starts[i] = inst.Placement.Start
		}(i)
	}
	wg.Wait()

	assert.Equal(t, n+1, drv.createCalls, "one create per pod (n on the card plus the sibling), no double-create")
	got := slices.Clone(starts)
	slices.Sort(got)
	assert.Equal(t, []int32{0, 2, 4, 6}, got, "distinct non-overlapping slots")

	select {
	case p := <-sibling:
		assert.Equal(t, int32(0), p.Start, "the sibling card allocates independently")
	default:
		t.Fatal("the sibling card reservation did not complete")
	}
}

// TestMarkerPathMatchesPodWorkDir pins the rooted marker path against the shared pod work-directory
// layout: the root is a parameter so tests need no process-wide redirect, and this is what keeps the
// two spellings of the layout from drifting.
func TestMarkerPathMatchesPodWorkDir(t *testing.T) {
	want := filepath.Join(deviceplugin.PodWorkDir("pod-a", "c"), markerFileName(testPPUUUID0))
	assert.Equal(t, want, markerPath(deviceplugin.OperatorPodsDir, "pod-a", "c", testPPUUUID0))
	assert.True(t, isMarkerFile(markerFileName(testPPUUUID0)))
	assert.False(t, isMarkerFile("thead-mig.json"), "the unsuffixed name is not a per-card marker")
	assert.False(t, isMarkerFile("nvidia-mig-"+testPPUUUID0+".json"))
}

func TestParseMarkerFailsClosed(t *testing.T) {
	good := selfMarker("pod-a", testPPUUUID0, migInstance{
		GiID: 1, CiID: 1, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-1",
	})
	cases := []struct {
		name    string
		write   func(t *testing.T, path string)
		wantErr bool
	}{
		{
			name: "a complete record round-trips",
			write: func(t *testing.T, path string) {
				require.NoError(t, writeMarker(path, good))
			},
		},
		{
			name:    "a missing record is an error",
			write:   func(*testing.T, string) {},
			wantErr: true,
		},
		{
			name: "a malformed record is an error",
			write: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
			},
			wantErr: true,
		},
		{
			name: "an identity-less record is an error",
			write: func(t *testing.T, path string) {
				m := good
				m.MigUUID = ""
				require.NoError(t, writeMarker(path, m))
			},
			wantErr: true,
		},
		{
			name: "a card-less record is an error",
			write: func(t *testing.T, path string) {
				m := good
				m.Card = ""
				require.NoError(t, writeMarker(path, m))
			},
			wantErr: true,
		},
		{
			// Complete in every field, so nothing but the disagreement with its own file name can
			// reject it — and it must, because the ownership set is grouped by the recorded card while
			// the file belongs to the card its name encodes.
			name: "a complete record naming another card is an error",
			write: func(t *testing.T, path string) {
				m := good
				m.Card = testPPUUUID1
				require.NoError(t, writeMarker(path, m))
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := markerPath(t.TempDir(), "pod-a", "c", testPPUUUID0)
			c.write(t, path)
			m, err := parseMarker(path)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, good, m)
		})
	}
}

// TestScanMarkers asserts the scan collects every parseable marker with its path, reports an
// unparseable one instead of aborting, and that a corrupt file is attributed to the card its own
// name records — the only scope in which an unknowable ownership set changes a decision.
func TestScanMarkers(t *testing.T) {
	podsDir := t.TempDir()
	inst0 := migInstance{
		GiID: 3, CiID: 3, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-0",
	}
	inst1 := migInstance{
		GiID: 4, CiID: 4, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{2, 2}, UUID: "MIG-1",
	}
	writeMarkerFixture(t, podsDir, selfMarker("pod-a", testPPUUUID0, inst0))
	writeMarkerFixture(t, podsDir, selfMarker("pod-b", testPPUUUID1, inst1))
	badPath := markerPath(podsDir, "pod-c", "c", testPPUUUID0)
	require.NoError(t, os.MkdirAll(filepath.Dir(badPath), 0o755))
	require.NoError(t, os.WriteFile(badPath, []byte("{not json"), 0o600))
	// A non-marker file in a pod work dir is ignored entirely.
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(badPath), "other.json"), []byte("{}"), 0o600))

	entries, corrupt := scanMarkers(podsDir)
	require.Len(t, entries, 2)
	assert.Equal(t, []string{badPath}, corrupt)

	assert.Equal(t, map[uint32]bool{3: true}, ownedGiIDsOnCard(entries, testPPUUUID0))
	assert.Equal(t, map[uint32]bool{4: true}, ownedGiIDsOnCard(entries, testPPUUUID1))
	assert.True(t, ownershipUnknownOnCard(corrupt, testPPUUUID0))
	assert.False(t, ownershipUnknownOnCard(corrupt, testPPUUUID1))

	card, ok := cardFromMarkerPath(badPath)
	assert.True(t, ok)
	assert.Equal(t, testPPUUUID0, card)
	uid, ok := podUIDFromMarkerPath(podsDir, badPath)
	assert.True(t, ok)
	assert.Equal(t, "pod-c", uid, "the owner parses from the path even when the record does not")

	// A path naming no card leaves the scope of what is unknown itself unknown, so it darkens every
	// card; a path that is not a marker file at marker depth names no owner either.
	nameless := markerPath(podsDir, "pod-d", "c", "")
	_, ok = cardFromMarkerPath(nameless)
	assert.False(t, ok)
	assert.True(t, ownershipUnknownOnCard([]string{nameless}, testPPUUUID0))
	assert.True(t, ownershipUnknownOnCard([]string{nameless}, testPPUUUID1))
	_, ok = podUIDFromMarkerPath(podsDir, filepath.Join(podsDir, "pod-e"))
	assert.False(t, ok)

	// An empty root is not an error: nothing has been allocated yet.
	entries, corrupt = scanMarkers(filepath.Join(podsDir, "absent"))
	assert.Empty(t, entries)
	assert.Empty(t, corrupt)
}

func TestFindLiveInstance(t *testing.T) {
	inst := migInstance{
		GiID: 6, CiID: 6, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{4, 2}, UUID: "MIG-6",
	}
	state := migCardState{ProfileID: testProfileID, Live: []migInstance{inst}}

	got, ok := findLiveInstance(state, 6)
	require.True(t, ok)
	assert.Equal(t, inst, got)

	_, ok = findLiveInstance(state, 7)
	assert.False(t, ok)
}

func TestProfileGeometry(t *testing.T) {
	devs := theadDevices(testProfile, 1, 2, testPPUUUID0)
	cases := []struct {
		name        string
		card        string
		profile     string
		wantCompute int32
		wantMemory  int32
		wantOK      bool
	}{
		{name: "known card and profile", card: testPPUUUID0, profile: testProfile, wantCompute: 1, wantMemory: 2, wantOK: true},
		{name: "unknown profile", card: testPPUUUID0, profile: "3c.30g"},
		{name: "unknown card", card: testPPUUUID1, profile: testProfile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			computeSlices, memorySlices, ok := profileGeometry(devs, c.card, c.profile)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.wantCompute, computeSlices)
			assert.Equal(t, c.wantMemory, memorySlices)
		})
	}
}

// TestAllocatedCards asserts the allocated cards are returned in devs order — the order every
// container's partition list is assembled in — and that an unallocated card is left out.
func TestAllocatedCards(t *testing.T) {
	devs := theadDevices(testProfile, 1, 2, testPPUUUID0, testPPUUUID1)

	both := map[deviceplugin.Resource]int32{
		{Group: "ppu", Device: testPPUUUID1}: 1,
		{Group: "ppu", Device: testPPUUUID0}: 1,
	}
	assert.Equal(t, []string{testPPUUUID0, testPPUUUID1}, allocatedCards(devs, both))

	one := map[deviceplugin.Resource]int32{{Group: "ppu", Device: testPPUUUID1}: 1}
	assert.Equal(t, []string{testPPUUUID1}, allocatedCards(devs, one))

	// A resource naming another group is not this group's card.
	other := map[deviceplugin.Resource]int32{{Group: "gpu", Device: testPPUUUID0}: 1}
	assert.Empty(t, allocatedCards(devs, other))
}

func TestResourceForCard(t *testing.T) {
	devs := theadDevices(testProfile, 1, 2, testPPUUUID0)
	assert.Equal(t,
		deviceplugin.Resource{Group: "ppu", Device: testPPUUUID0},
		resourceForCard(devs, testPPUUUID0))
	assert.Equal(t,
		deviceplugin.Resource{Device: testPPUUUID1},
		resourceForCard(devs, testPPUUUID1))
}

// TestFakeDriverBusyDestroy pins the seam's busy-destroy contract the reclaim loop reads with
// errors.Is: a driver rejection for a residual process wraps the shared sentinel.
func TestFakeDriverBusyDestroy(t *testing.T) {
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	inst := migInstance{
		GiID: 2, CiID: 2, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-busy",
	}
	drv.seedLive(testPPUUUID0, inst)
	drv.inUseGiIDs = map[uint32]bool{2: true}

	err := drv.DestroyInstance(testPPUUUID0, inst)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errInstanceInUse))
	assert.Empty(t, drv.destroyed)

	live, err := drv.ListInstances()
	require.NoError(t, err)
	assert.Equal(t, []migLiveInstance{{Card: testPPUUUID0, Inst: inst}}, live)

	drv.listErr = errors.New("instance enumeration incomplete")
	_, err = drv.ListInstances()
	assert.Error(t, err, "an enumeration that cannot prove completeness is an error")
}
