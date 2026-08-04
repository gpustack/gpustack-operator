package thead

import (
	"cmp"
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

// markerName is the per-container-per-card partition ownership marker written under the pod work
// dir. It is the restart-surviving record reclaim destroys the exact GPU/compute instance from,
// and that lets a create tell a reusable unbound instance from a bound one.
const markerName = "thead-mig.json"

// migPlacement is a memory-slice interval [Start, Start+Length) on a card — the platform-
// independent placement geometry the slot-pick and marker use, so the pure core is testable
// without the vendor library. The vendor placement type stays inside the _linux seam.
type migPlacement struct {
	Start  int32
	Length int32
}

// migInstance is one live or freshly created partition on a card: its GPU-instance and
// compute-instance ids, the raw vendor profile id the GPU instance was carved from, the
// compute-slice count, the memory-slice placement, and the partition identity string a container
// is given to address it.
//
// ProfileID is the adoption identity, and it has no "unknown" value: the vendor numbering makes 0
// a legal profile id, so a seam that cannot read a live instance's profile id must return an error
// rather than leave the field zero — a zero would read as some real profile and could be adopted
// for it.
type migInstance struct {
	GiID          uint32
	CiID          uint32
	ProfileID     uint32
	ComputeSlices int32
	Placement     migPlacement
	UUID          string
}

// migCardState is a card's live partition state read for one profile: the raw vendor profile id
// the requested profile name resolves to on that card, the profile's legal empty-card placements,
// and every live GPU instance on the card (across all profiles). The core subtracts the live
// placements (occupied) from Possible to pick a free slot, and scans Live for a reusable unbound
// instance of the same raw profile.
//
// The profile id is resolved by the seam rather than by the core: the vendor keeps the upstream
// numbering but does not assign it the upstream slice-count meaning, so an id is only ever read
// back from the driver, never computed from a slice count.
type migCardState struct {
	ProfileID uint32
	Possible  []migPlacement
	Live      []migInstance
}

// errInstanceInUse marks a destroy the driver rejected as busy — a residual process still holds
// the instance. The reclaim loop treats it as a bounded, retryable partial failure (never clearing
// the debounce) and surfaces an operator-visible log at the bound. The _linux seam wraps it, so
// the platform-independent core and reclaim loop can test with errors.Is.
var errInstanceInUse = errors.New("mig instance in use")

// migLiveInstance is one live GPU instance enumerated globally for reclaim: its card id plus the
// instance. It lets the orphan collector find a marker-less GPU instance on any card (including
// one carrying no marker at all) without a per-card profile hint.
type migLiveInstance struct {
	Card string
	Inst migInstance
}

// migDriver is the vendor management-library actuator seam behind a _linux.go/_other.go build tag:
// the real implementation drives the vendor binding on linux; the non-linux stub errors, so the
// pure marker/slot-pick core is table-tested with a fake driver. Every operation addresses a card
// by its vendor UUID (the operator accelerator ID). CardState and CreateInstance take the
// profile's name and geometry because a compute-slice count alone cannot pick the GPU-instance
// profile id — the seam matches the profile by name over a probe of every id.
//
// Every enumerating operation must return an error whenever it cannot prove its enumeration is
// complete, and must never report partial state as success. A live partition that reads as absent
// is not a harmless gap: its ownership marker is then removable as "already gone", its occupied
// slot is handed out a second time, and a marker-less one leaks past the orphan collector.
type migDriver interface {
	// CardState reads the card's live partition state for the given profile: the raw vendor
	// profile id the name resolves to, the profile's legal empty-card placements, and every live
	// GPU instance on the card. It errors rather than describing a card it could not read whole.
	CardState(cardUUID, profile string, computeSlices, memorySlices int32) (migCardState, error)
	// CreateInstance materializes a GPU instance of the profile at slot plus its whole-GI compute
	// instance, returning the created partition (ids, raw profile id, placement, identity).
	CreateInstance(cardUUID, profile string, computeSlices, memorySlices int32, slot migPlacement) (migInstance, error)
	// DestroyInstance tears down the partition's compute instance then its GPU instance, after
	// re-verifying under the card lock that the instance still carries the identity it was
	// snapshotted with. It returns an error wrapping errInstanceInUse when a residual process
	// blocks the destroy.
	DestroyInstance(cardUUID string, inst migInstance) error
	// ListInstances enumerates every live GPU instance across all partition-capable cards, each
	// carrying its card id, so the orphan collector can find a marker-less instance on a drained
	// card. A GPU instance carries no operator tag, so this is the only way to see an untracked
	// one. It errors rather than returning a list it cannot prove complete.
	ListInstances() ([]migLiveInstance, error)
}

// cardLocks holds a per-card mutex guarding the create+marker-write (and reclaim destroy) critical
// section, so concurrent allocations on the same card serialize their slot selection while sibling
// cards proceed in parallel. It is keyed by card UUID.
var cardLocks sync.Map // cardUUID -> *sync.Mutex

// lockCard locks the card's mutex and returns its unlock func.
func lockCard(cardUUID string) func() {
	m, _ := cardLocks.LoadOrStore(cardUUID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// migMarker is one parsed per-container-per-card ownership record: the pod<->partition correlation
// reclaim keys its liveness decision on, and the create's idempotent-retry and reuse checks read.
// It records the raw profile id beside the profile name, so a decision taken after a restart rests
// on the driver's own identity rather than on a name that a normalization change could move.
type migMarker struct {
	PodUID        string `json:"podUID"`
	Container     string `json:"container"`
	Card          string `json:"card"`
	Profile       string `json:"profile"`
	ProfileID     uint32 `json:"profileID"`
	GiID          uint32 `json:"giID"`
	CiID          uint32 `json:"ciID"`
	MigUUID       string `json:"migUUID"`
	ComputeSlices int32  `json:"computeSlices"`
	Start         int32  `json:"start"`
	Length        int32  `json:"length"`
}

// instance rebuilds the migInstance a marker records.
func (m migMarker) instance() migInstance {
	return migInstance{
		GiID:          m.GiID,
		CiID:          m.CiID,
		ProfileID:     m.ProfileID,
		ComputeSlices: m.ComputeSlices,
		Placement:     migPlacement{Start: m.Start, Length: m.Length},
		UUID:          m.MigUUID,
	}
}

// markerEntry pairs a parsed marker with its on-disk path so reclaim removes only the specific
// marker file (never a sibling's).
type markerEntry struct {
	path   string
	marker migMarker
}

// markerFileName is the per-card marker file name: thead-mig-<card>.json, so a container spanning
// multiple cards keeps one independent marker per card, each guarded by that card's lock. Naming
// the card in the file name is also what lets an unparseable marker still be attributed to its
// card, and so kept from poisoning any other card's decisions.
func markerFileName(cardUUID string) string {
	return strings.TrimSuffix(markerName, ".json") + "-" + cardUUID + ".json"
}

// markerPath returns the marker file path for a partitioned container on a card, under an explicit
// pods root. The root is a parameter rather than the shared package variable so a test writes to a
// temporary directory without mutating process-wide state; the layout below it mirrors the shared
// pod work directory, which the package test pins.
func markerPath(podsDir, podUID, container, cardUUID string) string {
	return filepath.Join(podsDir, podUID, "c-"+container, markerFileName(cardUUID))
}

// parseMarker reads a marker fail-closed: a missing, malformed or incomplete record is an error,
// so the self-marker reuse and reclaim never silently mis-read a live partition. The raw profile
// id is not checked for presence because 0 is a legal vendor id.
func parseMarker(path string) (migMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return migMarker{}, err
	}
	var m migMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return migMarker{}, fmt.Errorf("marker %q: %w", path, err)
	}
	if m.PodUID == "" || m.Card == "" || m.Profile == "" || m.MigUUID == "" {
		return migMarker{}, fmt.Errorf("marker %q: incomplete record", path)
	}
	return m, nil
}

// writeMarker publishes a marker via a temp file + atomic rename, so a concurrent scanner never
// reads a partially written record.
func writeMarker(path string, m migMarker) error {
	dir := filepath.Dir(path)
	if err := osx.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("create marker dir %q: %w", dir, err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".thead-mig-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp marker: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp marker: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod temp marker: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename marker into place: %w", err)
	}
	return nil
}

// scanMarkers parses every partition marker under podsDir. An unparseable marker is collected as a
// corrupt path rather than failing the whole scan, so one bad file never blocks the node; the
// callers narrow it through ownershipUnknownOnCard to the card the file name names, which is the
// only scope where an unknowable ownership set can change a decision — or to every card when the
// path names none.
func scanMarkers(podsDir string) (entries []markerEntry, corrupt []string) {
	_ = filepath.WalkDir(podsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			corrupt = append(corrupt, path)
			return nil
		}
		if d.IsDir() || !isMarkerFile(d.Name()) {
			return nil
		}
		m, perr := parseMarker(path)
		if perr != nil {
			corrupt = append(corrupt, path)
			return nil //nolint:nilerr
		}
		entries = append(entries, markerEntry{path: path, marker: m})
		return nil
	})
	return entries, corrupt
}

// isMarkerFile reports whether name is a partition marker file (thead-mig-<card>.json).
func isMarkerFile(name string) bool {
	return strings.HasPrefix(name, strings.TrimSuffix(markerName, ".json")+"-") &&
		strings.HasSuffix(name, ".json")
}

// ownedGiIDsOnCard returns the set of GPU-instance ids on cardUUID that any marker owns, so the
// reuse check can tell a bound instance from a reusable unbound one.
func ownedGiIDsOnCard(entries []markerEntry, cardUUID string) map[uint32]bool {
	owned := make(map[uint32]bool)
	for i := range entries {
		if entries[i].marker.Card == cardUUID {
			owned[entries[i].marker.GiID] = true
		}
	}
	return owned
}

// cardFromMarkerPath returns the card UUID a marker file's NAME encodes (thead-mig-<card>.json),
// which parses even when the file's contents do not — the property that keeps a corrupt marker's
// blast radius down to one card. It reports ok=false when the base name is not a marker name at all
// (a path a walk error collected, e.g. an unreadable directory) or encodes an empty card, because
// such a path cannot be attributed to any one card.
func cardFromMarkerPath(path string) (string, bool) {
	name := filepath.Base(path)
	if !isMarkerFile(name) {
		return "", false
	}
	card := strings.TrimSuffix(strings.TrimPrefix(name, strings.TrimSuffix(markerName, ".json")+"-"), ".json")
	if card == "" {
		return "", false
	}
	return card, true
}

// podUIDFromMarkerPath returns the Pod UID a marker path encodes — markers live at
// <podsDir>/<podUID>/c-<container>/<marker>, so the owner parses from the path even when the record
// inside is truncated. That is what lets the reclaim loop retire a corrupt marker on liveness
// evidence alone. It reports ok=false for a path that is not a marker file at that depth, whose
// owner is therefore unknowable.
func podUIDFromMarkerPath(podsDir, path string) (string, bool) {
	if !isMarkerFile(filepath.Base(path)) {
		return "", false
	}
	rel, err := filepath.Rel(podsDir, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] == "" || parts[0] == ".." {
		return "", false
	}
	return parts[0], true
}

// ownershipUnknownOnCard reports whether any unparseable marker leaves cardUUID's ownership set
// unknowable, in which case a leftover on that card cannot be proven unbound and every decision
// resting on that proof must fail closed: adoption of an unmarked leftover, and the drained-card
// verdict the reclaim loop's orphan collector destroys on. A card whose markers all parse is
// unaffected, and so is a fresh create, whose occupancy comes from the driver's own complete
// enumeration rather than from the markers.
//
// Scoping is by the card the corrupt file's name encodes, so one bad file never darkens a sibling
// card: failing closed node-wide would let a single truncated file deny a whole node's partition
// capacity, while failing closed on the one card it names cannot.
//
// A corrupt path that names no card darkens every card, because the scope of what is unknown is
// itself unknown — it may stand for markers of any card. The cost is bounded: refusing adoption is
// not refusing capacity (occupancy comes from the driver's live set, so a fresh create in a free slot
// still succeeds), and the state is transient rather than permanent — the reclaim loop retires a
// corrupt marker once its Pod is gone, so the card clears by itself.
func ownershipUnknownOnCard(corrupt []string, cardUUID string) bool {
	for _, p := range corrupt {
		card, ok := cardFromMarkerPath(p)
		if !ok || card == cardUUID {
			return true
		}
	}
	return false
}

// adoptUnboundInstance returns a live instance on the card that no marker owns and that was carved
// from the same raw vendor profile as the request — a leftover a crashed create or an out-of-band
// tool left behind, which adopting avoids re-creating a colliding one over. It returns ok=false
// when none is adoptable.
//
// Identity, not shape, decides: two different profiles can share a compute-slice count and a
// memory-slice span (a media or graphics variant among them), and adopting one for the other hands
// a container a partition that is not what it asked for. The geometry test is kept beside the
// identity test as an inconsistency trap: it is redundant against a self-consistent driver, and an
// instance whose profile id matches while its shape does not is exactly the unprovable state this
// path must refuse rather than resolve.
//
// An instance with an empty identity string is skipped: that is a GPU instance with no compute
// instance yet (a crash between the two creates), which addresses nothing — adopting it would hand
// out an empty device set and persist an identity-less marker that later fails closed. It is left
// for reclaim to destroy.
func adoptUnboundInstance(state migCardState, owned map[uint32]bool, computeSlices, memorySlices int32) (migInstance, bool) {
	for i := range state.Live {
		inst := state.Live[i]
		if inst.UUID == "" || owned[inst.GiID] {
			continue
		}
		if inst.ProfileID == state.ProfileID &&
			inst.ComputeSlices == computeSlices && inst.Placement.Length == memorySlices {
			return inst, true
		}
	}
	return migInstance{}, false
}

// pickPlacement returns the free placement with the smallest Start that overlaps no occupied
// interval, and ok=false when the card is full. Deterministic (lowest slot first) so concurrent
// creates on the same card under the per-card lock never pick the same slot.
func pickPlacement(possible, occupied []migPlacement) (migPlacement, bool) {
	sorted := make([]migPlacement, len(possible))
	copy(sorted, possible)
	slices.SortFunc(sorted, func(a, b migPlacement) int { return cmp.Compare(a.Start, b.Start) })
	for _, slot := range sorted {
		if !placementOverlapsAny(slot, occupied) {
			return slot, true
		}
	}
	return migPlacement{}, false
}

// placementOverlapsAny reports whether slot's interval intersects any occupied interval. Two
// half-open intervals [a, a+m) and [b, b+n) overlap iff a < b+n and b < a+m.
func placementOverlapsAny(slot migPlacement, occupied []migPlacement) bool {
	slotEnd := slot.Start + slot.Length
	for _, occ := range occupied {
		if slot.Start < occ.Start+occ.Length && occ.Start < slotEnd {
			return true
		}
	}
	return false
}

// findLiveInstance returns the live GPU instance with giID on the card, if any.
func findLiveInstance(state migCardState, giID uint32) (migInstance, bool) {
	for i := range state.Live {
		if state.Live[i].GiID == giID {
			return state.Live[i], true
		}
	}
	return migInstance{}, false
}

// migReserveOutcome tells the caller how a reservation resolved, so its rollback undoes exactly
// what this call did and nothing a prior allocation still owns:
//   - migCreated: a fresh GPU+compute instance was created and its marker written — rollback
//     destroys it and removes the marker;
//   - migBound: a pre-existing unbound instance was adopted and a marker written — rollback removes
//     the marker only (the instance was not ours to create), returning it to unbound;
//   - migRebound: an existing self-marker was reused unchanged (a kubelet Allocate retry) —
//     rollback leaves the marker and instance intact (they belong to that prior allocation).
type migReserveOutcome int

const (
	migCreated migReserveOutcome = iota
	migBound
	migRebound
)

// reserveMigInstance is the per-card partition allocation core, run under the card's lock. It (1)
// reuses an existing self-marker on an exact (card, profile) match — verifying its instance still
// lives and still carries the recorded identity — so a kubelet Allocate retry rebinds its own
// partition instead of double-creating; (2) adopts an unowned leftover instance of the same raw
// profile if one is present; (3) otherwise picks the lowest free placement and creates a new
// GPU+compute instance. It writes the ownership marker inside the critical section, rolling back a
// just-created instance if the marker write fails. The returned outcome tells the caller's rollback
// exactly what to undo.
//
// A card state the driver cannot prove complete is an error, never an empty card: reading a live
// partition as absent would hand its slot out twice.
func reserveMigInstance(
	drv migDriver, podsDir, podUID, container, cardUUID, profile string, computeSlices, memorySlices int32,
) (inst migInstance, outcome migReserveOutcome, err error) {
	self := markerPath(podsDir, podUID, container, cardUUID)
	if m, perr := parseMarker(self); perr == nil {
		if m.Profile != profile {
			return migInstance{}, migRebound, fmt.Errorf(
				"marker %q mismatches request (profile=%s): fail closed", self, profile)
		}
		state, serr := drv.CardState(cardUUID, profile, computeSlices, memorySlices)
		if serr != nil {
			return migInstance{}, migRebound, fmt.Errorf("read card %s state: %w", cardUUID, serr)
		}
		liveInst, ok := findLiveInstance(state, m.GiID)
		if !ok {
			return migInstance{}, migRebound, fmt.Errorf(
				"marker %q references missing gpu instance %d on card %s: fail closed", self, m.GiID, cardUUID)
		}
		// Guard against GPU-instance id reuse: a destroyed instance's id can be reassigned to a
		// different partition, so verify the live instance's identity still matches the marker
		// before rebinding — otherwise a retry would hand out a stale or foreign partition.
		if liveInst.UUID != m.MigUUID {
			return migInstance{}, migRebound, fmt.Errorf(
				"marker %q gpu instance %d uuid %q no longer matches live uuid %q (id reused): fail closed",
				self, m.GiID, m.MigUUID, liveInst.UUID)
		}
		return m.instance(), migRebound, nil
	} else if !os.IsNotExist(perr) {
		return migInstance{}, migRebound, fmt.Errorf("read self marker %q: %w", self, perr)
	}

	state, err := drv.CardState(cardUUID, profile, computeSlices, memorySlices)
	if err != nil {
		return migInstance{}, migCreated, fmt.Errorf("read card %s state: %w", cardUUID, err)
	}

	entries, corrupt := scanMarkers(podsDir)
	owned := ownedGiIDsOnCard(entries, cardUUID)

	reused, adopt := migInstance{}, false
	if !ownershipUnknownOnCard(corrupt, cardUUID) {
		reused, adopt = adoptUnboundInstance(state, owned, computeSlices, memorySlices)
	}

	if adopt {
		inst = reused
		outcome = migBound
	} else {
		occupied := make([]migPlacement, 0, len(state.Live))
		for i := range state.Live {
			occupied = append(occupied, state.Live[i].Placement)
		}
		slot, ok := pickPlacement(state.Possible, occupied)
		if !ok {
			return migInstance{}, migCreated, fmt.Errorf(
				"card %s has no free placement for profile %s", cardUUID, profile)
		}
		inst, err = drv.CreateInstance(cardUUID, profile, computeSlices, memorySlices, slot)
		if err != nil {
			return migInstance{}, migCreated, fmt.Errorf("create %s instance on card %s: %w", profile, cardUUID, err)
		}
		outcome = migCreated
	}

	m := migMarker{
		PodUID: podUID, Container: container, Card: cardUUID, Profile: profile, ProfileID: inst.ProfileID,
		GiID: inst.GiID, CiID: inst.CiID, MigUUID: inst.UUID,
		ComputeSlices: inst.ComputeSlices, Start: inst.Placement.Start, Length: inst.Placement.Length,
	}
	if werr := writeMarker(self, m); werr != nil {
		if outcome == migCreated {
			// Roll back the just-created instance so a create-before-marker crash window is not
			// left behind by our own error path (a real crash between the two is reclaimed). An
			// adopted instance is left alone: it was not ours to create.
			_ = drv.DestroyInstance(cardUUID, inst)
		}
		return migInstance{}, outcome, werr
	}
	return inst, outcome, nil
}

// profileGeometry returns the per-card compute/memory slice geometry of a partition profile on the
// allocated card, read from the card's detect-time capability in devs.Spec. It reports ok=false
// when the card or profile is absent, so the caller fails the allocation rather than guessing
// geometry.
func profileGeometry(devs *workercore.Devices, cardUUID, profile string) (computeSlices, memorySlices int32, ok bool) {
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if acc.ID != cardUUID {
				continue
			}
			for k := range acc.Status.PhysicalSliced.Profiles {
				p := &acc.Status.PhysicalSliced.Profiles[k]
				if p.Name == profile {
					return p.ComputeSlices, p.MemorySlices, true
				}
			}
		}
	}
	return 0, 0, false
}

// allocatedCards returns the UUIDs of the allocated cards in devs order, so a container's
// partition list and a co-allocating container's are assembled the same way and read the same.
func allocatedCards(devs *workercore.Devices, allocated map[deviceplugin.Resource]int32) []string {
	cards := make([]string, 0, len(allocated))
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			res := deviceplugin.Resource{
				Group:  grp.ID,
				Device: grp.Accelerators[j].ID,
			}
			if _, ok := allocated[res]; ok {
				cards = append(cards, grp.Accelerators[j].ID)
			}
		}
	}
	return cards
}

// resourceForCard returns the Resource (group:device) of the card with the given UUID.
func resourceForCard(devs *workercore.Devices, cardUUID string) deviceplugin.Resource {
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			if grp.Accelerators[j].ID == cardUUID {
				return deviceplugin.Resource{Group: grp.ID, Device: cardUUID}
			}
		}
	}
	return deviceplugin.Resource{Device: cardUUID}
}
