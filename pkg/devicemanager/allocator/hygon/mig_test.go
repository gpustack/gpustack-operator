package hygon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// fakeMigDriver is a driver whose whole state is data, so the reservation core can be exercised
// without hardware. Create appends to Live, mirroring what the real driver does.
type fakeMigDriver struct {
	state      migCardState
	createErr  error
	stateErr   error
	destroyErr error
	// inUseGiIDs are the instances whose destroy the driver refuses because something holds them,
	// which is the vendor's own answer rather than anything this side counts.
	inUseGiIDs map[uint32]bool
	// listed is what ListInstances reports, and listErr what it fails with.
	listed  []migLiveInstance
	listErr error
	// onList runs inside ListInstances, which is where a test reproduces something happening
	// between a caller's record scan and its decision.
	onList func()

	created   []migPlacement
	destroyed []migInstance
	nextGiID  uint32
	nextUUID  int
}

func (d *fakeMigDriver) CardState(string, string) (migCardState, error) {
	if d.stateErr != nil {
		return migCardState{}, d.stateErr
	}
	return d.state, nil
}

func (d *fakeMigDriver) CreateInstance(_, _ string, slot migPlacement) (migInstance, error) {
	if d.createErr != nil {
		return migInstance{}, d.createErr
	}
	d.created = append(d.created, slot)
	d.nextGiID++
	d.nextUUID++
	inst := migInstance{
		GiID:      d.nextGiID,
		CiID:      0,
		ProfileID: d.state.ProfileID,
		Placement: slot,
		UUID:      fmt.Sprintf("uuid-%d", d.nextUUID),
		ConfPath:  fmt.Sprintf("/etc/dmi_mig_config/ci/dev0gi%dci0.conf", d.nextGiID),
	}
	d.state.Live = append(d.state.Live, inst)
	return inst, nil
}

func (d *fakeMigDriver) DestroyInstance(_ string, inst migInstance) error {
	if d.inUseGiIDs[inst.GiID] {
		return fmt.Errorf("destroy gpu instance %d: %w", inst.GiID, errInstanceInUse)
	}
	d.destroyed = append(d.destroyed, inst)
	return d.destroyErr
}

func (d *fakeMigDriver) ListInstances() ([]migLiveInstance, error) {
	if d.onList != nil {
		d.onList()
	}
	return d.listed, d.listErr
}

// destroyedGiIDs is what a reclaim pass actually tore down.
func (d *fakeMigDriver) destroyedGiIDs() []uint32 {
	out := make([]uint32, 0, len(d.destroyed))
	for _, inst := range d.destroyed {
		out = append(out, inst.GiID)
	}
	return out
}

func testCard() migCard {
	return migCard{UUID: "GPU-abc", PciBusID: "0000:09:00.0"}
}

// A partition must never be placed on slices something else already holds, and the choice must be
// reproducible: a node's layout is only assertable if the same free card always yields the same slot.
func TestPickMigPlacement(t *testing.T) {
	// A four-slice card's one-slice profile, and its two-slice profile.
	oneSlice := []migPlacement{{0, 1}, {1, 1}, {2, 1}, {3, 1}}
	twoSlice := []migPlacement{{0, 2}, {2, 2}}

	testCases := []struct {
		name     string
		possible []migPlacement
		occupied []migPlacement
		want     migPlacement
		wantOK   bool
	}{
		{
			name:     "an empty card yields the lowest slot",
			possible: oneSlice,
			want:     migPlacement{0, 1},
			wantOK:   true,
		},
		{
			name:     "the lowest FREE slot, not the lowest slot",
			possible: oneSlice,
			occupied: []migPlacement{{0, 1}, {1, 1}},
			want:     migPlacement{2, 1},
			wantOK:   true,
		},
		{
			name:     "a wide profile skips a slot a narrow instance partly covers",
			possible: twoSlice,
			occupied: []migPlacement{{1, 1}},
			want:     migPlacement{2, 2},
			wantOK:   true,
		},
		{
			name:     "an unsorted possible set still yields the lowest free slot",
			possible: []migPlacement{{3, 1}, {0, 1}, {2, 1}, {1, 1}},
			occupied: []migPlacement{{0, 1}},
			want:     migPlacement{1, 1},
			wantOK:   true,
		},
		{
			name:     "a full card yields nothing",
			possible: oneSlice,
			occupied: []migPlacement{{0, 4}},
		},
		{
			name:     "a wide profile on a card with only scattered gaps yields nothing",
			possible: twoSlice,
			occupied: []migPlacement{{1, 1}, {2, 1}},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickMigPlacement(tc.possible, tc.occupied)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// Adoption is what keeps a node from accumulating partitions nobody claims after a crash between the
// create and its record. It must never take an instance of another geometry, nor one somebody owns.
func TestAdoptUnboundMigInstance(t *testing.T) {
	state := migCardState{
		ProfileID: 3,
		Live: []migInstance{
			{GiID: 6, ProfileID: 3, Placement: migPlacement{3, 1}, UUID: "u6"},
			{GiID: 4, ProfileID: 3, Placement: migPlacement{1, 1}, UUID: "u4"},
			{GiID: 9, ProfileID: 1, Placement: migPlacement{0, 2}, UUID: "u9"},
		},
	}

	testCases := []struct {
		name   string
		owned  map[uint32]bool
		wantGi uint32
		wantOK bool
	}{
		{
			name:   "the lowest-placed unowned instance of the requested profile",
			owned:  map[uint32]bool{},
			wantGi: 4,
			wantOK: true,
		},
		{
			name:   "an owned instance is skipped for the next unowned one",
			owned:  map[uint32]bool{4: true},
			wantGi: 6,
			wantOK: true,
		},
		{
			name:  "an instance of another profile is never adopted",
			owned: map[uint32]bool{4: true, 6: true},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := adoptUnboundMigInstance(state, tc.owned)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantGi, got.GiID)
			}
		})
	}
}

// A record is the only correlation between a container and the partition it holds, so anything
// unreadable about it has to fail closed rather than be interpreted.
func TestParseMigMarker(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	good := `{"podUID":"p","container":"c","card":"GPU-abc","pciBusID":"0000:09:00.0","profile":"2g.15gb","profileID":3,` +
		`"giID":3,"ciID":0,"migUUID":"u","confPath":"/etc/dmi_mig_config/ci/dev0gi3ci0.conf",` +
		`"start":0,"length":1}`

	t.Run("a complete record whose name agrees with it", func(t *testing.T) {
		m, err := parseMigMarker(write(t, migMarkerFileName("GPU-abc"), good))
		require.NoError(t, err)
		assert.Equal(t, uint32(3), m.GiID)
		assert.Equal(t, "u", m.MigUUID)
		assert.Equal(t, migPlacement{0, 1}, m.instance().Placement)
	})

	t.Run("a record naming a different card than its file name is refused", func(t *testing.T) {
		_, err := parseMigMarker(write(t, migMarkerFileName("GPU-other"), good))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not the card its file name names")
	})

	t.Run("a record with no identity is incomplete", func(t *testing.T) {
		body := `{"podUID":"p","container":"c","card":"GPU-abc","pciBusID":"0000:09:00.0","profile":"2g.15gb",` +
			`"confPath":"/x","giID":3}`
		_, err := parseMigMarker(write(t, migMarkerFileName("GPU-abc"), body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "incomplete record")
	})

	t.Run("a record with no registry path is incomplete, because nothing could be mounted",
		func(t *testing.T) {
			body := `{"podUID":"p","container":"c","card":"GPU-abc","pciBusID":"0000:09:00.0","profile":"2g.15gb",` +
				`"migUUID":"u","giID":3}`
			_, err := parseMigMarker(write(t, migMarkerFileName("GPU-abc"), body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "incomplete record")
		})

	t.Run("unparseable json is an error", func(t *testing.T) {
		_, err := parseMigMarker(write(t, migMarkerFileName("GPU-abc"), "{"))
		require.Error(t, err)
	})
}

// The reservation is where a slot is handed out, so every path through it is a place a partition can
// be double-granted or leaked.
func TestReserveMigInstance(t *testing.T) {
	const profile = "2g.15gb"
	freeCard := func() migCardState {
		return migCardState{
			ProfileID: 3,
			Possible:  []migPlacement{{0, 1}, {1, 1}, {2, 1}, {3, 1}},
		}
	}

	t.Run("an empty card carves the lowest slot and records it", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}

		inst, outcome, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.NoError(t, err)
		assert.Equal(t, migCreated, outcome)
		assert.Equal(t, migPlacement{0, 1}, inst.Placement)
		assert.Equal(t, []migPlacement{{0, 1}}, drv.created)

		m, perr := parseMigMarker(migMarkerPath(podsDir, "pod", "ctr", testCard().UUID))
		require.NoError(t, perr)
		assert.Equal(t, inst.UUID, m.MigUUID)
		assert.Equal(t, inst.ConfPath, m.ConfPath)
	})

	t.Run("a retry rebinds the same partition and carves nothing", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}

		first, _, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)
		require.NoError(t, err)

		again, outcome, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.NoError(t, err)
		assert.Equal(t, migRebound, outcome)
		assert.Equal(t, first.UUID, again.UUID)
		assert.Len(t, drv.created, 1, "a retry must not carve a second partition")
	})

	t.Run("a record whose instance now carries another identity fails closed", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}

		_, _, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)
		require.NoError(t, err)

		// The vendor issues a fresh identity on every create, so a reused id ALWAYS shows up here.
		drv.state.Live[0].UUID = "a-different-instance"

		_, _, err = reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "id reused")
	})

	t.Run("a record whose instance is gone fails closed", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}

		_, _, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)
		require.NoError(t, err)
		drv.state.Live = nil

		_, _, err = reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing gpu instance")
	})

	t.Run("a record for another profile fails closed rather than re-carving", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}

		_, _, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)
		require.NoError(t, err)

		_, _, err = reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), "8g.63gb")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "mismatches request")
	})

	t.Run("an unowned leftover is adopted rather than carved beside", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}
		drv.state.Live = []migInstance{{
			GiID: 7, ProfileID: 3, Placement: migPlacement{2, 1},
			UUID: "leftover", ConfPath: "/etc/dmi_mig_config/ci/dev0gi7ci0.conf",
		}}

		inst, outcome, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.NoError(t, err)
		assert.Equal(t, migAdopted, outcome)
		assert.Equal(t, "leftover", inst.UUID)
		assert.Empty(t, drv.created, "an adoptable instance must not be carved beside")
	})

	t.Run("a leftover another container owns is not adopted", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}
		drv.state.Live = []migInstance{{
			GiID: 7, ProfileID: 3, Placement: migPlacement{2, 1},
			UUID: "held", ConfPath: "/etc/dmi_mig_config/ci/dev0gi7ci0.conf",
		}}
		require.NoError(t, writeMigMarker(
			migMarkerPath(podsDir, "other-pod", "other-ctr", testCard().UUID),
			migMarker{
				PodUID: "other-pod", Container: "other-ctr", Card: testCard().UUID, PciBusID: testCard().PciBusID, Profile: profile,
				ProfileID: 3, GiID: 7, MigUUID: "held",
				ConfPath: "/etc/dmi_mig_config/ci/dev0gi7ci0.conf", Start: 2, Length: 1,
			}))

		inst, outcome, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.NoError(t, err)
		assert.Equal(t, migCreated, outcome)
		assert.NotEqual(t, uint32(7), inst.GiID)
		assert.Equal(t, []migPlacement{{0, 1}}, drv.created,
			"the held instance's slot must be treated as occupied")
	})

	t.Run("an unreadable record on this card suppresses adoption", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}
		drv.state.Live = []migInstance{{
			GiID: 7, ProfileID: 3, Placement: migPlacement{2, 1},
			UUID: "leftover", ConfPath: "/etc/dmi_mig_config/ci/dev0gi7ci0.conf",
		}}
		corrupt := migMarkerPath(podsDir, "other-pod", "other-ctr", testCard().UUID)
		require.NoError(t, os.MkdirAll(filepath.Dir(corrupt), 0o777))
		require.NoError(t, os.WriteFile(corrupt, []byte("{"), 0o600))

		_, outcome, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.NoError(t, err)
		assert.Equal(t, migCreated, outcome,
			"ownership that cannot be read must not be assumed absent")
	})

	t.Run("a full card is refused rather than served from somebody else's slot", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard()}
		drv.state.Live = []migInstance{{GiID: 1, ProfileID: 1, Placement: migPlacement{0, 4}, UUID: "big"}}
		// Owned, so it cannot be adopted either.
		require.NoError(t, writeMigMarker(
			migMarkerPath(podsDir, "other", "ctr", testCard().UUID),
			migMarker{
				PodUID: "other", Container: "ctr", Card: testCard().UUID, PciBusID: testCard().PciBusID, Profile: "8g.63gb",
				GiID: 1, MigUUID: "big", ConfPath: "/x", Start: 0, Length: 4,
			}))

		_, _, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no free placement")
	})

	t.Run("a record path that cannot be read fails closed without carving", func(t *testing.T) {
		// A file where the container's record directory must go. The read of the record is what
		// fails, and an unreadable record must never be taken for an absent one: doing so would
		// carve a second partition beside one this container may already hold.
		podsDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(podsDir, "pod"), []byte("x"), 0o600))
		drv := &fakeMigDriver{state: freeCard()}

		_, _, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "read self marker")
		assert.Empty(t, drv.created, "an unreadable record must not be carved past")
	})

	t.Run("a card whose state cannot be read is an error, never an empty card", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{state: freeCard(), stateErr: errors.New("driver is unhappy")}

		_, _, err := reserveMigInstance(drv, podsDir, "pod", "ctr", testCard(), profile)

		require.Error(t, err)
		assert.Empty(t, drv.created, "an unreadable card must not be carved into")
	})
}

// The rollback is the leak guard: it must undo exactly what a reservation did on the failure paths
// that come AFTER it -- the response build, and the annotation patch the server does last.
func TestRollbackMigInstance(t *testing.T) {
	inst := migInstance{
		GiID: 3, ProfileID: 3, Placement: migPlacement{0, 1},
		UUID: "u", ConfPath: "/etc/dmi_mig_config/ci/dev0gi3ci0.conf",
	}

	testCases := []struct {
		name        string
		outcome     migReserveOutcome
		wantDestroy bool
		wantRecord  bool
	}{
		{
			name:        "a partition this allocation created is destroyed and its record dropped",
			outcome:     migCreated,
			wantDestroy: true,
		},
		{
			name:    "an adopted partition keeps its instance and loses only the claim",
			outcome: migAdopted,
		},
		{
			name:       "a rebound partition is left entirely alone",
			outcome:    migRebound,
			wantRecord: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podsDir := t.TempDir()
			restore := deviceplugin.OperatorPodsDir
			deviceplugin.OperatorPodsDir = podsDir
			t.Cleanup(func() { deviceplugin.OperatorPodsDir = restore })

			path := migMarkerPath(podsDir, "pod", "ctr", testCard().UUID)
			require.NoError(t, writeMigMarker(path, migMarker{
				PodUID: "pod", Container: "ctr", Card: testCard().UUID, PciBusID: testCard().PciBusID, Profile: "2g.15gb",
				ProfileID: 3, GiID: 3, MigUUID: "u", ConfPath: inst.ConfPath, Start: 0, Length: 1,
			}))

			drv := &fakeMigDriver{}
			s := &server{mig: drv, ResourceServer: deviceplugin.ResourceServer{Logger: klog.Background()}}

			s.rollbackMigInstance(testCard(), inst, tc.outcome, "pod", "ctr")

			assert.Equal(t, tc.wantDestroy, len(drv.destroyed) == 1,
				"destroyed=%d", len(drv.destroyed))
			_, statErr := os.Stat(path)
			assert.Equal(t, tc.wantRecord, statErr == nil, "record present=%v", statErr == nil)
		})
	}
}

// The vendor runtime makes exactly ONE partition visible to a container, whatever it is given:
// binding two instance files, passing a comma-separated list of prefixed identifiers, and passing
// "all" were all measured to yield a single visible device on two driver generations. A grant
// spanning two accelerators therefore cannot be delivered, so it is refused rather than half-served
// -- carving on both would consume quota on a card the workload can never reach and report success
// for a container running at a fraction of what it was admitted for.
func TestActuatePhysicalSliced_RefusesMoreThanOneAccelerator(t *testing.T) {
	redirectLogicalSliceDirs(t)

	devs := hygonDevicesFixture()
	drv := &fakeMigDriver{}
	s := &server{mig: drv, ResourceServer: deviceplugin.ResourceServer{Logger: klog.Background()}}
	pod, ctr := slicedPod("two-cards", "train", 0, 0)

	_, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{
			{Group: groupID, Device: "dcu-uuid-0"}: 1,
			{Group: groupID, Device: "dcu-uuid-1"}: 1,
		}, "2g.15gb")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "can only be served by one")
	assert.Empty(t, drv.created, "a refused request must carve nothing at all")
}

// A grant that resolved to no accelerator is refused too: the response would otherwise carry the
// node's control devices and no partition, which the vendor runtime reports as "no devices" at the
// workload rather than here.
func TestActuatePhysicalSliced_RefusesAnEmptyGrant(t *testing.T) {
	redirectLogicalSliceDirs(t)

	drv := &fakeMigDriver{}
	s := &server{mig: drv, ResourceServer: deviceplugin.ResourceServer{Logger: klog.Background()}}
	pod, ctr := slicedPod("no-card", "train", 0, 0)

	_, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr, hygonDevicesFixture(),
		map[deviceplugin.Resource]int32{}, "2g.15gb")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no allocated card")
	assert.Empty(t, drv.created)
}

// An accelerator with no recorded PCI address cannot be reached through the Multi-Instance library
// at all -- it serves no UUID lookup and answers its own PCI query with an empty string -- so the
// allocation is refused rather than served against a card this call could not name.
func TestActuatePhysicalSliced_RefusesACardWithNoPciAddress(t *testing.T) {
	redirectLogicalSliceDirs(t)

	devs := hygonDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].Topology.PciBusID = ""
	drv := &fakeMigDriver{}
	s := &server{mig: drv, ResourceServer: deviceplugin.ResourceServer{Logger: klog.Background()}}
	pod, ctr := slicedPod("no-bdf", "train", 0, 0)

	_, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{{Group: groupID, Device: "dcu-uuid-0"}: 1}, "2g.15gb")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recorded pci address")
	assert.Empty(t, drv.created)
}

// Nothing else on the node can free a partition: the device-plugin protocol has no release callback,
// and this vendor refuses to leave Multi-Instance mode while any instance survives. So what this
// pass decides is the difference between a node that can be returned to whole-card service and one
// that cannot.
func TestMigReclaimerReconcile(t *testing.T) {
	const card = "GPU-abc"
	bdf := testCard().PciBusID

	// record lays down one container's ownership of a partition.
	record := func(t *testing.T, podsDir, podUID string, giID uint32) {
		t.Helper()
		require.NoError(t, writeMigMarker(
			migMarkerPath(podsDir, podUID, "ctr", card),
			migMarker{
				PodUID: podUID, Container: "ctr", Card: card, PciBusID: bdf, Profile: "2g.15gb",
				ProfileID: 3, GiID: giID, MigUUID: fmt.Sprintf("u%d", giID),
				ConfPath: fmt.Sprintf("/etc/dmi_mig_config/ci/dev0gi%dci0.conf", giID),
				Start:    int32(giID), Length: 1,
			}))
	}
	instance := func(giID uint32) migLiveInstance {
		return migLiveInstance{PciBusID: bdf, Instance: migInstance{
			GiID: giID, ProfileID: 3, Placement: migPlacement{int32(giID), 1},
			UUID: fmt.Sprintf("u%d", giID),
		}}
	}

	t.Run("a live pod keeps its partition", func(t *testing.T) {
		podsDir := t.TempDir()
		record(t, podsDir, "alive", 3)
		drv := &fakeMigDriver{listed: []migLiveInstance{instance(3)}}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile([]string{"alive"})

		assert.Empty(t, drv.destroyedGiIDs())
		assert.FileExists(t, migMarkerPath(podsDir, "alive", "ctr", card))
	})

	t.Run("a dead pod's partition is destroyed and its record dropped", func(t *testing.T) {
		podsDir := t.TempDir()
		record(t, podsDir, "alive", 3)
		record(t, podsDir, "gone", 4)
		drv := &fakeMigDriver{listed: []migLiveInstance{instance(3), instance(4)}}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile([]string{"alive"})

		assert.Equal(t, []uint32{4}, drv.destroyedGiIDs())
		assert.FileExists(t, migMarkerPath(podsDir, "alive", "ctr", card))
		assert.NoFileExists(t, migMarkerPath(podsDir, "gone", "ctr", card))
	})

	t.Run("a partition still in use is left, record and all", func(t *testing.T) {
		podsDir := t.TempDir()
		record(t, podsDir, "gone", 4)
		drv := &fakeMigDriver{
			listed:     []migLiveInstance{instance(4)},
			inUseGiIDs: map[uint32]bool{4: true},
		}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile(nil)

		assert.Empty(t, drv.destroyedGiIDs())
		assert.FileExists(t, migMarkerPath(podsDir, "gone", "ctr", card),
			"a record must outlive a destroy the driver refused, or nothing attributes the instance")
	})

	t.Run("an instance this pass just released is not collected a second time", func(t *testing.T) {
		// The driver's enumeration is taken after the release, so in production a destroyed instance
		// is normally gone from it. Relying on that is what this guards: a stale list -- or a
		// concurrent reservation that has since taken the freed id -- would otherwise have the sweep
		// destroy an instance twice, the second time possibly somebody else's.
		podsDir := t.TempDir()
		record(t, podsDir, "gone", 4)
		drv := &fakeMigDriver{listed: []migLiveInstance{instance(4)}}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile(nil)

		assert.Equal(t, []uint32{4}, drv.destroyedGiIDs())
	})

	t.Run("an instance no record claims is collected", func(t *testing.T) {
		podsDir := t.TempDir()
		record(t, podsDir, "alive", 3)
		drv := &fakeMigDriver{listed: []migLiveInstance{instance(3), instance(7)}}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile([]string{"alive"})

		assert.Equal(t, []uint32{7}, drv.destroyedGiIDs())
	})

	t.Run("an unreadable record suppresses the orphan sweep entirely", func(t *testing.T) {
		podsDir := t.TempDir()
		corrupt := migMarkerPath(podsDir, "mystery", "ctr", card)
		require.NoError(t, os.MkdirAll(filepath.Dir(corrupt), 0o777))
		require.NoError(t, os.WriteFile(corrupt, []byte("{"), 0o600))
		drv := &fakeMigDriver{listed: []migLiveInstance{instance(7)}}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile(nil)

		assert.Empty(t, drv.destroyedGiIDs(),
			"ownership that cannot be read must not let an instance be taken for an orphan")
	})

	t.Run("a destroy that failed keeps its record for the next pass", func(t *testing.T) {
		podsDir := t.TempDir()
		record(t, podsDir, "gone", 4)
		drv := &fakeMigDriver{destroyErr: errors.New("driver is unhappy")}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile(nil)

		assert.FileExists(t, migMarkerPath(podsDir, "gone", "ctr", card))
	})

	t.Run("an enumeration that failed collects nothing rather than everything", func(t *testing.T) {
		podsDir := t.TempDir()
		drv := &fakeMigDriver{listErr: errors.New("driver is unhappy")}

		newMigReclaimer(drv, podsDir, klog.Background()).reconcile(nil)

		assert.Empty(t, drv.destroyedGiIDs())
	})
}

// A reservation writes its ownership record and releases the accelerator's lock before its caller
// finishes building the container response, so a sweep that decided from a record scan taken before
// that write would destroy a partition an allocation had just been granted. Observed three times in
// one second on a node admitting three Pods, each losing its partition -- and made likelier by the
// driver handing a freed instance id straight back out.
//
// The window is reproduced faithfully rather than raced for: the claim appears while the sweep is
// enumerating, which is exactly between the scan it decided from and the lock it destroys under.
func TestMigReclaimerDoesNotCollectAPartitionClaimedMidSweep(t *testing.T) {
	const card = "GPU-abc"
	bdf := testCard().PciBusID
	podsDir := t.TempDir()

	drv := &fakeMigDriver{
		listed: []migLiveInstance{{PciBusID: bdf, Instance: migInstance{
			GiID: 4, ProfileID: 3, Placement: migPlacement{1, 1}, UUID: "u4",
		}}},
	}
	drv.onList = func() {
		require.NoError(t, writeMigMarker(
			migMarkerPath(podsDir, "just-admitted", "main", card),
			migMarker{
				PodUID: "just-admitted", Container: "main", Card: card, PciBusID: bdf,
				Profile: "2g.15gb", ProfileID: 3, GiID: 4, MigUUID: "u4",
				ConfPath: "/etc/dmi_mig_config/ci/dev0gi4ci0.conf", Start: 1, Length: 1,
			}))
	}

	newMigReclaimer(drv, podsDir, klog.Background()).reconcile([]string{"just-admitted"})

	assert.Empty(t, drv.destroyedGiIDs(),
		"a partition claimed while the sweep was deciding must survive it")
}

// The accelerator lock is what makes that re-read exclusive, and it only does so if every caller
// takes the SAME lock. The reclaim sweep reaches a card through the driver, which knows only PCI
// addresses, while an allocation starts from the Devices record, which is keyed by UUID -- so keying
// the lock on the UUID would have the two serialize with nothing at all.
func TestLockMigCardIsKeyedByPciAddress(t *testing.T) {
	release := lockMigCard(testCard().PciBusID)

	taken := make(chan struct{})
	go func() {
		defer close(taken)
		lockMigCard(testCard().PciBusID)()
	}()

	select {
	case <-taken:
		t.Fatal("a second holder of the same accelerator's lock was not blocked")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-taken:
	case <-time.After(2 * time.Second):
		t.Fatal("the lock was never released to the waiting holder")
	}
}
