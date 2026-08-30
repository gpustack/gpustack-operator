package hygon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// migMarkerName is the base name of the per-container-per-accelerator ownership record. It is an
// ON-DISK FORMAT: a record written by an earlier release is still on real nodes, and renaming it
// would make retry, adoption and reclamation blind to everything already allocated.
const migMarkerName = "hygon-mig.json"

// _MigInstanceIDPrefix is what the vendor's own tooling prefixes an instance identity with, in its
// listing and in the selection environment a container reads. Identifiers recorded upward carry it
// so the value a Pod's annotation holds is the value an operator sees everywhere else -- and, on
// this vendor, the value the container's own environment must carry to select the instance at all.
const _MigInstanceIDPrefix = "MIG-"

// errInstanceInUse reports that the driver refused to destroy a partition because something still
// holds it. It is the driver's own answer rather than a count this side took: the vendor refuses the
// teardown with "the device is exist and may be in use" while a workload has the instance open,
// which is a stronger signal than any process query here could be -- this library serves none for a
// partition at all.
var errInstanceInUse = errors.New("partition is in use")

// migPlacement is a run of GPU slices on one accelerator, in the vendor's own slice units.
type migPlacement struct {
	Start  int32
	Length int32
}

// migInstance is one materialized partition: the GPU instance, the compute instance inside it, and
// the two things a container needs to use it.
//
// The identity is the compute instance's UUID rather than anything derived. It is worth knowing that
// this vendor's UUID is NOT placement-derived: destroying a partition and creating the same profile
// at the same placement yields a DIFFERENT UUID, measured. That makes the identity check below
// strictly reliable -- a reused GPU-instance id can never masquerade as the partition a marker
// recorded -- and it is why nothing here tries to recompute a UUID from a placement.
type migInstance struct {
	GiID      uint32
	CiID      uint32
	ProfileID uint32
	Placement migPlacement
	// UUID is the vendor's own mig_uuid for the compute instance.
	UUID string
	// ConfPath is the vendor's compute-instance file on the host. It is the unit of injection: a
	// container is given this file, at this same path, and the vendor runtime activates the instance
	// from it.
	ConfPath string
}

// migLiveInstance pairs a live partition with the PCI address of the accelerator holding it.
type migLiveInstance struct {
	PciBusID string
	Instance migInstance
}

// migCardState is one accelerator's partition state for one profile: the vendor profile id the name
// resolved to, the profile's legal empty-accelerator placements, and every live GPU instance on the
// accelerator whatever its profile.
//
// Live spans every profile on purpose. Placement selection has to avoid a slice another profile's
// instance occupies, and a state listing only same-profile instances would hand out an occupied run.
type migCardState struct {
	ProfileID uint32
	Possible  []migPlacement
	Live      []migInstance
}

// migDriver is the driver-facing half of the partition allocator. It is an interface so the
// reservation logic stays hardware-free and testable, and so a non-linux build has something to
// compile against.
//
// Every method errors rather than describing state it could not read whole. An accelerator read as
// empty when it is not would hand the same slot out twice.
type migDriver interface {
	// CardState reads one accelerator's live partition state for the given profile name.
	CardState(pciBusID, profile string) (migCardState, error)
	// CreateInstance materializes a GPU instance of the profile at slot, plus the whole-GI compute
	// instance inside it, and returns the partition including the file a container binds to use it.
	CreateInstance(pciBusID, profile string, slot migPlacement) (migInstance, error)
	// DestroyInstance tears down the compute instance then its GPU instance, in that order, which
	// the vendor requires. It returns an error wrapping errInstanceInUse when the driver refuses
	// because something still holds the partition.
	DestroyInstance(pciBusID string, inst migInstance) error
	// ListInstances enumerates every live partition on the node, each carrying the PCI address of
	// the accelerator holding it, so the orphan collector can find a marker-less one.
	ListInstances() ([]migLiveInstance, error)
}

// migCard is one accelerator under both the identities this package needs: the UUID everything
// upward records and joins by, and the PCI address that is the ONLY way to reach the same card
// through the Multi-Instance library -- it serves no UUID at all and answers its own PCI query with
// an empty string.
type migCard struct {
	UUID     string
	PciBusID string
}

// migCardLocks serializes the reserve-and-record critical section per accelerator, so two concurrent
// allocations cannot both pick the same free placement.
//
// The lock is per accelerator rather than global because the driver itself is: creating an instance
// on one card while a workload runs on another is safe and was measured to be, so a global lock
// would serialize allocations that never contend.
var migCardLocks sync.Map

// lockMigCard takes the accelerator's lock and returns its release.
func lockMigCard(cardUUID string) func() {
	v, _ := migCardLocks.LoadOrStore(cardUUID, &sync.Mutex{})
	mu := v.(*sync.Mutex) //nolint:forcetypeassert
	mu.Lock()
	return mu.Unlock
}

// migMarker is one parsed ownership record: the correlation between a container and the partition it
// holds, which the retry path reads to rebind and the reclaim path reads to decide liveness.
//
// The field names and their json tags are an ON-DISK FORMAT; see migMarkerName.
type migMarker struct {
	PodUID    string `json:"podUID"`
	Container string `json:"container"`
	Card      string `json:"card"`
	// PciBusID is the accelerator's PCI address, recorded because the Multi-Instance library can be
	// reached by nothing else -- it serves no UUID lookup at all. Without it here, reclamation could
	// read a record naming a card it then had no way to address.
	PciBusID  string `json:"pciBusID"`
	Profile   string `json:"profile"`
	ProfileID uint32 `json:"profileID"`
	GiID      uint32 `json:"giID"`
	CiID      uint32 `json:"ciID"`
	MigUUID   string `json:"migUUID"`
	ConfPath  string `json:"confPath"`
	Start     int32  `json:"start"`
	Length    int32  `json:"length"`
}

// instance rebuilds the partition a marker records.
func (m migMarker) instance() migInstance {
	return migInstance{
		GiID:      m.GiID,
		CiID:      m.CiID,
		ProfileID: m.ProfileID,
		Placement: migPlacement{Start: m.Start, Length: m.Length},
		UUID:      m.MigUUID,
		ConfPath:  m.ConfPath,
	}
}

// migMarkerEntry pairs a parsed marker with its path, so reclamation removes only that file.
type migMarkerEntry struct {
	path   string
	marker migMarker
}

// migMarkerFileName names the record after the accelerator it belongs to, so a container holding
// partitions on several accelerators keeps one independent record per accelerator -- and so an
// unparseable record is still attributable to its accelerator rather than poisoning every other's
// decisions.
func migMarkerFileName(cardUUID string) string {
	return strings.TrimSuffix(migMarkerName, ".json") + "-" + cardUUID + ".json"
}

// migMarkerPath returns a partitioned container's record path on one accelerator, under an explicit
// pods root so a test writes to a temporary directory without mutating process-wide state.
func migMarkerPath(podsDir, podUID, container, cardUUID string) string {
	return filepath.Join(podsDir, podUID, "c-"+container, migMarkerFileName(cardUUID))
}

// parseMigMarker reads a record fail-closed: missing, malformed or incomplete is an error, so the
// rebind and reclaim paths never mis-read a live partition. The raw profile id is not checked for
// presence because 0 is a legal vendor id.
//
// The recorded accelerator must be the one the file's own NAME encodes. A record disagreeing with
// its name is internally inconsistent and unsafe either way round: the ownership set is grouped by
// the recorded accelerator, so its instance would look unowned on the accelerator the file belongs
// to and a second Pod could adopt a partition already in use.
func parseMigMarker(path string) (migMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return migMarker{}, err
	}
	var m migMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return migMarker{}, fmt.Errorf("marker %q: %w", path, err)
	}
	if m.PodUID == "" || m.Card == "" || m.PciBusID == "" || m.Profile == "" ||
		m.MigUUID == "" || m.ConfPath == "" {
		return migMarker{}, fmt.Errorf("marker %q: incomplete record", path)
	}
	if card, ok := cardFromMigMarkerPath(path); !ok || card != m.Card {
		return migMarker{}, fmt.Errorf(
			"marker %q records card %q, not the card its file name names: fail closed", path, m.Card)
	}
	return m, nil
}

// writeMigMarker publishes a record durably, so a concurrent scanner never reads a partial one and a
// written record survives an unclean shutdown.
//
// The record is read by nothing outside this process -- not by the container, not by the vendor
// library -- so it is written for its writer alone, unlike the container-facing artifacts beside it.
func writeMigMarker(path string, m migMarker) error {
	dir := filepath.Dir(path)
	if err := osx.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("create marker dir %q: %w", dir, err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	if err := osx.DurableWrite(path, data, 0o600); err != nil {
		return fmt.Errorf("write marker %q: %w", path, err)
	}
	return nil
}

// scanMigMarkers parses every partition record under podsDir. An unparseable one is collected as a
// corrupt path rather than failing the scan, so one bad file never blocks the node; callers narrow
// that through migOwnershipUnknownOnCard to the accelerator the file name encodes, which is the only
// scope where an unknowable ownership set can change a decision.
func scanMigMarkers(podsDir string) (entries []migMarkerEntry, corrupt []string) {
	_ = filepath.WalkDir(podsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			corrupt = append(corrupt, path)
			return nil
		}
		if d.IsDir() || !isMigMarkerFile(d.Name()) {
			return nil
		}
		m, perr := parseMigMarker(path)
		if perr != nil {
			corrupt = append(corrupt, path)
			return nil //nolint:nilerr
		}
		entries = append(entries, migMarkerEntry{path: path, marker: m})
		return nil
	})
	return entries, corrupt
}

// isMigMarkerFile reports whether a name is a partition record.
func isMigMarkerFile(name string) bool {
	base := strings.TrimSuffix(migMarkerName, ".json")
	return strings.HasPrefix(name, base+"-") && strings.HasSuffix(name, ".json")
}

// cardFromMigMarkerPath recovers the accelerator a record's file name encodes.
func cardFromMigMarkerPath(path string) (string, bool) {
	name := filepath.Base(path)
	if !isMigMarkerFile(name) {
		return "", false
	}
	base := strings.TrimSuffix(migMarkerName, ".json")
	card := strings.TrimSuffix(strings.TrimPrefix(name, base+"-"), ".json")
	if card == "" {
		return "", false
	}
	return card, true
}

// migOwnedGiIDsOnCard returns the GPU-instance ids some container already owns on one accelerator.
func migOwnedGiIDsOnCard(entries []migMarkerEntry, cardUUID string) map[uint32]bool {
	owned := make(map[uint32]bool)
	for i := range entries {
		if entries[i].marker.Card == cardUUID {
			owned[entries[i].marker.GiID] = true
		}
	}
	return owned
}

// migOwnershipUnknownOnCard reports whether a corrupt record leaves this accelerator's ownership set
// unknowable -- either because the record names this accelerator, or because it names none at all
// and so could belong to any.
//
// Adoption is suppressed when it is true: adopting an instance whose owner might exist but be
// unreadable would hand a live partition to a second container.
func migOwnershipUnknownOnCard(corrupt []string, cardUUID string) bool {
	for _, path := range corrupt {
		card, ok := cardFromMigMarkerPath(path)
		if !ok || card == cardUUID {
			return true
		}
	}
	return false
}

// pickMigPlacement returns the lowest legal placement that overlaps nothing occupied.
//
// Lowest rather than any is deliberate: a deterministic choice makes a node's layout reproducible
// and a test's expectation writable, and it packs from one end so a wide profile keeps a run free
// for as long as possible.
func pickMigPlacement(possible, occupied []migPlacement) (migPlacement, bool) {
	candidates := slices.Clone(possible)
	slices.SortFunc(candidates, func(a, b migPlacement) int { return int(a.Start - b.Start) })
	for _, slot := range candidates {
		if !migPlacementOverlapsAny(slot, occupied) {
			return slot, true
		}
	}
	return migPlacement{}, false
}

// migPlacementOverlapsAny reports whether slot shares a slice with anything occupied.
func migPlacementOverlapsAny(slot migPlacement, occupied []migPlacement) bool {
	for _, o := range occupied {
		if slot.Start < o.Start+o.Length && o.Start < slot.Start+slot.Length {
			return true
		}
	}
	return false
}

// findLiveMigInstance returns the live instance with the given GPU-instance id.
func findLiveMigInstance(state migCardState, giID uint32) (migInstance, bool) {
	for i := range state.Live {
		if state.Live[i].GiID == giID {
			return state.Live[i], true
		}
	}
	return migInstance{}, false
}

// adoptUnboundMigInstance returns a live instance of the requested profile that no container owns.
//
// It exists because a create can succeed and its record fail to be written -- or the process can die
// between the two -- leaving an instance nobody claims. Adopting it is what keeps a node from
// accumulating unusable partitions across restarts. Only an instance of the same vendor profile id
// qualifies: a partition of another geometry is not the grant that was asked for.
func adoptUnboundMigInstance(state migCardState, owned map[uint32]bool) (migInstance, bool) {
	candidates := slices.Clone(state.Live)
	slices.SortFunc(candidates, func(a, b migInstance) int { return int(a.Placement.Start - b.Placement.Start) })
	for _, inst := range candidates {
		if inst.ProfileID == state.ProfileID && !owned[inst.GiID] {
			return inst, true
		}
	}
	return migInstance{}, false
}

// migReserveOutcome tells a caller's rollback exactly what a reservation did.
type migReserveOutcome int

const (
	// migRebound: this container already owned the partition; nothing was created or claimed.
	migRebound migReserveOutcome = iota
	// migAdopted: an existing unowned instance was claimed. A rollback drops the claim but must
	// leave the instance alone -- it was not this call's to create.
	migAdopted
	// migCreated: the instance was created here, so a rollback destroys it.
	migCreated
)

// reserveMigInstance is the whole reserve-and-record critical section for one accelerator. The
// caller holds that accelerator's lock.
//
// It rebinds this container's own partition if it already holds one, adopts an unowned leftover of
// the same profile if there is one, and otherwise creates a new GPU instance and its whole-GI
// compute instance at the lowest free placement.
//
// The ownership record is written inside the section, and a just-created instance is rolled back if
// that write fails, so this call's own error paths never leave an unclaimed instance behind.
//
// The outcome is only meaningful when the returned error is nil: several error paths return the
// value a rollback would act on if it trusted it, so a caller must check the error first and roll
// back nothing at all on a failed reservation -- the failure has already undone whatever it did.
func reserveMigInstance(
	drv migDriver, podsDir, podUID, container string, card migCard, profile string,
) (inst migInstance, outcome migReserveOutcome, err error) {
	cardUUID := card.UUID
	self := migMarkerPath(podsDir, podUID, container, cardUUID)

	if m, perr := parseMigMarker(self); perr == nil {
		if m.Profile != profile {
			return migInstance{}, migRebound, fmt.Errorf(
				"marker %q mismatches request (profile=%s): fail closed", self, profile)
		}
		state, serr := drv.CardState(card.PciBusID, profile)
		if serr != nil {
			return migInstance{}, migRebound, fmt.Errorf("read card %s state: %w", cardUUID, serr)
		}
		live, ok := findLiveMigInstance(state, m.GiID)
		if !ok {
			return migInstance{}, migRebound, fmt.Errorf(
				"marker %q references missing gpu instance %d on card %s: fail closed", self, m.GiID, cardUUID)
		}
		// Guard against GPU-instance id reuse. The vendor assigns ids from a pool, so a destroyed
		// instance's id can come back attached to a different partition -- and because this vendor's
		// UUID is freshly issued on every create rather than derived from the placement, comparing it
		// is a complete check: a recreated instance NEVER carries the recorded UUID.
		if live.UUID != m.MigUUID {
			return migInstance{}, migRebound, fmt.Errorf(
				"marker %q gpu instance %d uuid %q no longer matches live uuid %q (id reused): fail closed",
				self, m.GiID, m.MigUUID, live.UUID)
		}
		return m.instance(), migRebound, nil
	} else if !os.IsNotExist(perr) {
		return migInstance{}, migRebound, fmt.Errorf("read self marker %q: %w", self, perr)
	}

	state, err := drv.CardState(card.PciBusID, profile)
	if err != nil {
		return migInstance{}, migCreated, fmt.Errorf("read card %s state: %w", cardUUID, err)
	}

	entries, corrupt := scanMigMarkers(podsDir)
	owned := migOwnedGiIDsOnCard(entries, cardUUID)

	reused, adopt := migInstance{}, false
	if !migOwnershipUnknownOnCard(corrupt, cardUUID) {
		reused, adopt = adoptUnboundMigInstance(state, owned)
	}

	if adopt {
		inst, outcome = reused, migAdopted
	} else {
		occupied := make([]migPlacement, 0, len(state.Live))
		for i := range state.Live {
			occupied = append(occupied, state.Live[i].Placement)
		}
		slot, ok := pickMigPlacement(state.Possible, occupied)
		if !ok {
			return migInstance{}, migCreated, fmt.Errorf(
				"card %s has no free placement for profile %s", cardUUID, profile)
		}
		inst, err = drv.CreateInstance(card.PciBusID, profile, slot)
		if err != nil {
			return migInstance{}, migCreated, fmt.Errorf(
				"create %s instance on card %s: %w", profile, cardUUID, err)
		}
		outcome = migCreated
	}

	m := migMarker{
		PodUID: podUID, Container: container, Card: cardUUID, PciBusID: card.PciBusID, Profile: profile,
		ProfileID: inst.ProfileID, GiID: inst.GiID, CiID: inst.CiID, MigUUID: inst.UUID,
		ConfPath: inst.ConfPath, Start: inst.Placement.Start, Length: inst.Placement.Length,
	}
	if werr := writeMigMarker(self, m); werr != nil {
		if outcome == migCreated {
			// Undo the create so this call's own error path leaves nothing behind. A real crash in
			// the same window is what the orphan collector is for. An adopted instance is left
			// alone: it was not ours to create.
			_ = drv.DestroyInstance(card.PciBusID, inst)
		}
		return migInstance{}, outcome, werr
	}
	return inst, outcome, nil
}

// migAccelerator is one allocated accelerator under what the response builder needs from it: its
// group, for the resource key recorded upward, and the record itself, for the device nodes.
type migAccelerator struct {
	groupID string
	accel   *workercore.Accelerator
}

// migAllocatedAccelerator returns the allocated accelerator with the given identity.
func migAllocatedAccelerator(
	devs *workercore.Devices, allocated map[deviceplugin.Resource]int32, cardUUID string,
) (migAccelerator, bool) {
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	for i := range accelerators {
		if accelerators[i].Accel.ID == cardUUID {
			return migAccelerator{
				groupID: accelerators[i].Group.ID,
				accel:   accelerators[i].Accel,
			}, true
		}
	}
	return migAccelerator{}, false
}

// migAllocatedCards returns the accelerators an allocation resolved to, each under both identities.
//
// An accelerator whose record carries no PCI address is refused rather than skipped: the library
// cannot be reached for it at all, so continuing would allocate on a card this call could not name,
// or silently allocate nothing.
func migAllocatedCards(
	devs *workercore.Devices, allocated map[deviceplugin.Resource]int32,
) ([]migCard, error) {
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	cards := make([]migCard, 0, len(accelerators))
	for i := range accelerators {
		accel := accelerators[i].Accel
		if accel.Topology.PciBusID == "" {
			return nil, fmt.Errorf(
				"card %s: no recorded pci address to reach it through the multi-instance library: fail closed",
				accel.ID)
		}
		cards = append(cards, migCard{UUID: accel.ID, PciBusID: accel.Topology.PciBusID})
	}
	return cards, nil
}
