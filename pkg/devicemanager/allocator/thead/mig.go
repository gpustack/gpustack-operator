package thead

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	core "k8s.io/api/core/v1"

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
	// CardInstances enumerates one card's live GPU instances, for the callers that already know
	// which card they are deciding about. It exists so the reclaim loop's verification re-read does
	// not have to walk the node: that read happens with the card's lock held, and the node-wide walk
	// is a few thousand driver calls on a fully populated host, every one of which would block every
	// allocation on the card meanwhile. It errors on the same terms as ListInstances.
	CardInstances(cardUUID string) ([]migInstance, error)
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
//
// The recorded card must be the card the file's own NAME encodes. A record that disagrees with its
// name is internally inconsistent, and either reading of it is unsafe: the ownership set is grouped
// by the recorded card, so the gpu instance the record owns would look unowned on the card the file
// belongs to and a second Pod could adopt a partition another Pod is using; while the self-marker
// rebind reads the record's ids against the card its path names, so it would rebind one card's
// instance id onto another card. It is therefore refused here, which reports it to the scan as a
// corrupt path — attributable to the card its name encodes, held closed on that card alone, and
// retired once its Pod is gone, the same as any other unreadable record.
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
	if card, ok := cardFromMarkerPath(path); !ok || card != m.Card {
		return migMarker{}, fmt.Errorf(
			"marker %q records card %q, not the card its file name names: fail closed", path, m.Card)
	}
	return m, nil
}

// writeMarker publishes a marker durably: a concurrent scanner never reads a partial record,
// and a record that has been written survives an unclean shutdown.
//
// The two modes are deliberately different. The directory is the shared per-container work
// directory every allocator writes its artifacts into, and it is wide because the logical-slicing
// artifacts living beside it are read by a container running as whatever user its image chose. The
// record itself is read by nothing outside this process — not by the container, not by the vendor
// library — so it is written for its writer alone, as the cambricon allocator's own record is.
func writeMarker(path string, m migMarker) error {
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
// itself unknown — it may stand for markers of any card. Refusing adoption is not refusing capacity
// (occupancy comes from the driver's live set, so a fresh create in a free slot still succeeds), and
// a corrupt MARKER clears by itself: the reclaim loop retires it once the Pod its path names is
// gone. A corrupt path that names no card, however, names no Pod either — a walk error over a pod
// directory is the reachable case — so there is no liveness evidence to retire it on and the loop
// deliberately keeps it. That hold is therefore permanent, not transient: no adoption anywhere on
// the node and no orphan collected on any card, for as long as the path stays unreadable. It is a
// filesystem fault an operator can repair, and the reclaim loop says so out loud at a retry bound
// (reclaimMaxCorruptHoldMisses) rather than letting the node degrade silently.
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
//
// The outcome is only meaningful when the returned error is nil. Several error paths return a non-zero
// outcome — the value a rollback would act on if it trusted it — so a caller must check the error
// first and roll back nothing at all on a failed reservation: the failure has already undone whatever
// it did.
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

// hostDevDir and hostProcDriverDir are the roots of the vendor's device nodes and of the driver's
// procfs capability tree. They are vars rather than consts so a test can point them at a temporary
// directory instead of reading the host's own /dev and /proc.
var (
	hostDevDir        = "/dev"
	hostProcDriverDir = "/proc/driver"
)

// The set of nodes below comes from the vendor's container-isolation documentation, whose own example
// injects exactly the five a partitioned container is given here — the two shared control nodes, the
// parent card's node, and the capability nodes of one GPU instance and its compute instance — and
// describes the result as a container that can use that one partition and see no other device. There
// is no environment-variable equivalent and no runtime hook: these nodes are the whole of the
// container's access.
//
// How those nodes are NAMED is measured on hardware instead, because the documentation's naming rule does
// not describe this driver. On a 16-card host every card's node is /dev/alixpu_ppu<ordinal> and its
// capability subtree is /proc/driver/alixpu/capabilities/ppu<ordinal>, one ordinal for both, running
// 0..15 with no ppu16; a live GPU instance created on the card at ordinal 14 appeared under ppu14.
//
// The kernel minor number each of those nodes carries is a different number, and it is the card's identity
// rather than its name: it is what the detector records, and what proves an ordinal reaches the card that
// record describes. On that host the two ran one apart, because the shared control node /dev/alixpu holds
// minor 0 of the same character-device major as the per-card nodes — but that is an observation about one
// host and one driver, not a rule this file may lean on. Nothing here computes either number from the
// other, in either direction, and the addressing proof below holds at any offset or at none.
const (
	// devControlName and devCtlName are the shared control nodes every container addressing any
	// partition needs. They are not per card, so they are addressed by name alone.
	devControlName = "alixpu"
	devCtlName     = "alixpu_ctl"
	// devCardPrefix names a card's own device node, suffixed by the card's ORDINAL — its accelerator
	// index — and never by the kernel minor number that node carries.
	devCardPrefix = "alixpu_ppu"
	// devCapDir and devCapPrefix name a capability node, which is addressed by its minor number
	// alone — the number the driver publishes in procfs, never one computed here.
	devCapDir    = "alixpu-caps"
	devCapPrefix = "alixpu-cap"
	// procDriverName is the driver's own directory under the procfs driver root, holding the
	// capability tree.
	procDriverName = "alixpu"
	// procCapMinorField is the field an access file carries its capability node's minor number on.
	procCapMinorField = "DeviceFileMinor:"
)

// sharedControlNodePaths returns the vendor's shared control nodes, needed once per container.
func sharedControlNodePaths() []string {
	return []string{
		filepath.Join(hostDevDir, devControlName),
		filepath.Join(hostDevDir, devCtlName),
	}
}

// cardNodePath returns the device node of the card with the given ordinal.
func cardNodePath(ordinal uint32) string {
	return filepath.Join(hostDevDir, devCardPrefix+strconv.FormatUint(uint64(ordinal), 10))
}

// capNodePath returns the capability device node carrying the given minor number.
func capNodePath(minor uint32) string {
	return filepath.Join(hostDevDir, devCapDir, devCapPrefix+strconv.FormatUint(uint64(minor), 10))
}

// cardCapDir returns the procfs capability directory holding one card's partition access files. The card
// is keyed by the same ordinal its device node is named after — the driver publishes both trees under
// that one number — so the two paths cannot come to denote different cards and hand a container one
// card's node with another card's capability. The suffix is always numeric, so this can never address the
// tree's card-less branch — the driver's own config and monitor capabilities, which sit beside the
// per-card directories rather than under one of them.
func cardCapDir(ordinal uint32) string {
	return filepath.Join(hostProcDriverDir, procDriverName, "capabilities",
		"ppu"+strconv.FormatUint(uint64(ordinal), 10), "mig")
}

// giAccessPath returns the procfs access file of a GPU instance on a card.
func giAccessPath(ordinal, giID uint32) string {
	return filepath.Join(cardCapDir(ordinal), "gi"+strconv.FormatUint(uint64(giID), 10), "access")
}

// ciAccessPath returns the procfs access file of a compute instance inside its GPU instance.
func ciAccessPath(ordinal, giID, ciID uint32) string {
	return filepath.Join(cardCapDir(ordinal),
		"gi"+strconv.FormatUint(uint64(giID), 10), "ci"+strconv.FormatUint(uint64(ciID), 10), "access")
}

// readCapMinor reads a capability node's minor number from the driver's procfs access file, failing
// closed on a file it cannot read or cannot find the field in.
//
// The number must be resolved at allocation time and must never be cached at detection time. The
// vendor's numbering is neither per card nor sequential nor derivable from the instance ids — its
// documented example places the first GPU instance's capability at 256 while another instance's sits
// at 1280 and that instance's first compute instance at 1281, with unrelated capabilities numbered
// before any partition exists — and the numbers are reassigned as partitions are created and
// destroyed. A cached value would therefore eventually address a stranger's partition.
func readCapMinor(path string) (uint32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read capability file: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), procCapMinorField)
		if !ok {
			continue
		}
		minor, perr := strconv.ParseUint(strings.TrimSpace(rest), 10, 32)
		if perr != nil {
			return 0, fmt.Errorf("capability file %q: parse %s %w", path, procCapMinorField, perr)
		}
		return uint32(minor), nil
	}
	return 0, fmt.Errorf("capability file %q carries no %s field", path, procCapMinorField)
}

// deviceNodeMinor reports the minor number of the character device at a path. It is a package var so a
// test can substitute the single fact it cannot produce in a temporary directory without root — a real
// character device — while the path's existence stays a genuine filesystem property.
var deviceNodeMinor = statDeviceNodeMinor

// statDeviceNodeMinor stats path and reports its device minor number, erroring when the path is absent
// or is not a character device.
func statDeviceNodeMinor(path string) (uint32, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return 0, fmt.Errorf("%q is not a character device", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%q exposes no device numbers", path)
	}
	// The Rdev field is platform-typed — uint64 on linux, int32 on the development platform — so this
	// conversion is REQUIRED to compile there and redundant on linux, which is why it carries a
	// directive rather than being "cleaned up": removing it breaks the development build, and no single
	// untagged expression satisfies the linter on both. Note this file carries no build tag at all;
	// touching a platform-typed syscall field is enough to make an ordinary file lint differently per
	// platform, which is not the same trap as a _linux.go seam skipping the local checks.
	return unix.Minor(uint64(st.Rdev)), nil //nolint:unconvert // platform-typed Rdev, see above
}

// newPartitionDeviceSpec renders one verified node as a read-write device specification.
func newPartitionDeviceSpec(path string) *deviceplugin.DeviceSpec {
	return &deviceplugin.DeviceSpec{
		ContainerPath: path,
		HostPath:      path,
		Permissions:   "rw",
	}
}

// requireDeviceNode verifies a node the partition's device set needs and returns its specification.
//
// It deliberately does not use the shared device-spec helper, which returns nil for a path that does
// not exist: the whole-card responder appends only what is non-nil, so reusing that helper here would
// turn a missing node into a SUCCESSFUL allocation carrying a silently incomplete device set. A
// partition needs every node in its set, so an absent one — or one that is not a character device —
// fails the allocation.
func requireDeviceNode(path string) (*deviceplugin.DeviceSpec, error) {
	if _, err := deviceNodeMinor(path); err != nil {
		return nil, fmt.Errorf("device node %q: %w", path, err)
	}
	return newPartitionDeviceSpec(path), nil
}

// requireNumberedDeviceNode verifies a node whose minor number is its identity: a capability node, whose
// minor is the one procfs published for it, and a card's own node, whose minor is the one the detector
// recorded for that card. A node carrying a different number is a /dev tree that disagrees with the
// driver, so it fails the allocation rather than being handed over as if it were the right one.
//
// Only the two shared control nodes are verified by requireDeviceNode instead, with no number to
// compare: they are addressed by name alone rather than per card, so there is nothing they could be
// confused with.
func requireNumberedDeviceNode(path string, wantMinor uint32) (*deviceplugin.DeviceSpec, error) {
	minor, err := deviceNodeMinor(path)
	if err != nil {
		return nil, fmt.Errorf("device node %q: %w", path, err)
	}
	if minor != wantMinor {
		return nil, fmt.Errorf("device node %q carries minor %d, want %d: fail closed", path, minor, wantMinor)
	}
	return newPartitionDeviceSpec(path), nil
}

// requireCardNode returns the ordinal that names cardUUID's own device node and keys its procfs
// capability subtree — the card's accelerator index — together with the verified specification of that
// device node. It is the one definition of how a card is addressed on this vendor's node, so both the
// whole-card and the partition responders reach a card through it and cannot come to address one
// differently.
//
// The ordinal is only usable once it is shown to reach the card the detector measured, and the proof is
// the node's OWN kernel minor number against the minor number the detector recorded for that
// accelerator: equal means this ordinal addresses that card. It asserts nothing about how the two numbers
// relate. Whatever offset the driver's numbering puts between them belongs to the driver, and is not
// reconstructed here: an offset assumed from one host's observation would address a card that departed
// from it anyway, and would refuse every card on a host that numbered differently.
//
// The proof is what makes the accelerator index safe to name a path with, because the index is a
// post-filter counter — it advances only for a card the detector accepted — so a card skipped
// mid-enumeration shifts every later index onto its neighbor. A shifted index names a node whose minor
// is not the one recorded for the accelerator, which is exactly what this refuses.
//
// It fails closed and never substitutes: an accelerator absent from devs, one carrying no recorded minor
// number, a node that is missing or is not a character device, and a node whose minor disagrees with the
// record are all errors. The detector is what makes the no-record refusal meaningful — it records nothing
// when the driver cannot answer for a card's minor number, rather than substituting the enumeration
// counter, because a substituted number is indistinguishable from a real one here and would let an
// unprovable ordinal be handed over as a proven one.
func requireCardNode(devs *workercore.Devices, cardUUID string) (uint32, *deviceplugin.DeviceSpec, error) {
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			if acc.ID != cardUUID {
				continue
			}
			if len(acc.PhysicalIndexes) == 0 {
				return 0, nil, fmt.Errorf(
					"card %s: no recorded minor number to prove its device node addresses it: fail closed", cardUUID)
			}
			spec, err := requireNumberedDeviceNode(cardNodePath(acc.Index), acc.PhysicalIndexes[0])
			if err != nil {
				return 0, nil, fmt.Errorf("card %s: %w", cardUUID, err)
			}
			return acc.Index, spec, nil
		}
	}
	return 0, nil, fmt.Errorf("card %s: absent from the device record: fail closed", cardUUID)
}

// partitionDeviceSpecs returns the device specifications a container needs to address exactly one
// partition on one card: the parent card's node, then the capability nodes of the GPU instance and of
// its compute instance. The shared control nodes are not included — they are per container rather than
// per card, so the caller adds them once.
//
// Every node is required. This vendor has no container-runtime hook, so the injected nodes are the
// whole of the container's access: too few leaves the partition unusable, and a node belonging to
// another card or another partition re-opens the isolation the partition exists to provide.
//
// The card's own node arrives already verified, from the addressing guard the caller cleared before
// reserving anything, and is passed through rather than re-derived here: the node handed to the container
// is then the very node whose kernel minor was proven against the detector's record. The ordinal is that
// guard's result too, so the capability subtree read below is the one belonging to the card that proof
// addressed.
func partitionDeviceSpecs(
	ordinal uint32,
	card *deviceplugin.DeviceSpec,
	inst migInstance,
) ([]*deviceplugin.DeviceSpec, error) {
	specs := []*deviceplugin.DeviceSpec{card}
	for _, access := range []string{
		giAccessPath(ordinal, inst.GiID),
		ciAccessPath(ordinal, inst.GiID, inst.CiID),
	} {
		minor, merr := readCapMinor(access)
		if merr != nil {
			return nil, merr
		}
		spec, derr := requireNumberedDeviceNode(capNodePath(minor), minor)
		if derr != nil {
			return nil, derr
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// ActuatePhysicalSliced materializes one partition of profile per allocated card, serialized per card
// by the card lock, records each chosen placement upward for the ledger reconciler, and returns the
// container response injecting the partitions' device nodes: the shared control nodes once, then each
// card's own node and the capability nodes of its partition's GPU and compute instances.
//
// Nothing is delegated to a container runtime here, so the response's device specifications are the
// whole of the container's access and are assembled fail-closed — any node it cannot produce fails the
// allocation. On any card's failure it rolls back exactly what this call did, per the per-card
// reservation outcome, so no half-owned Pod persists and no partition a prior allocation owns is
// touched.
//
// This is one half of the physical-sliced responder capability; the compile-time assertion that the
// server implements the whole of it belongs with the other half, the visibility response.
func (s *server) ActuatePhysicalSliced(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	profile string,
) (*deviceplugin.PhysicalSlicedAllocation, error) {
	if s.mig == nil {
		return nil, fmt.Errorf("mig actuator not configured")
	}

	cards := allocatedCards(devs, allocated)
	if len(cards) == 0 {
		return nil, fmt.Errorf("no allocated card for physical-slice container %q", ctr.Name)
	}

	// The shared control nodes are verified before anything is reserved: they are needed by every
	// card's partition, so a node set that cannot include them is a failure worth taking for free.
	sharedPaths := sharedControlNodePaths()
	devices := make([]*deviceplugin.DeviceSpec, 0, len(sharedPaths)+3*len(cards))
	for _, path := range sharedPaths {
		spec, err := requireDeviceNode(path)
		if err != nil {
			return nil, err
		}
		devices = append(devices, spec)
	}

	placements := make(map[deviceplugin.Resource][]workercore.AcceleratorPhysicalPlacement, len(cards))
	// results records how each card resolved so rollback undoes exactly this call's work under the same
	// per-card lock the create took (so it never races a concurrent same-card allocation's state read,
	// and never removes a marker or destroys an instance a prior allocation owns).
	type cardResult struct {
		card    string
		inst    migInstance
		outcome migReserveOutcome
	}
	var results []cardResult
	rollback := func() {
		for i := range results {
			r := results[i]
			unlock := lockCard(r.card)
			switch r.outcome {
			case migCreated:
				_ = s.mig.DestroyInstance(r.card, r.inst)
				_ = os.Remove(markerPath(deviceplugin.OperatorPodsDir, string(pod.UID), ctr.Name, r.card))
			case migBound:
				// The instance was pre-existing (adopted), so only drop our ownership marker,
				// returning it to the unbound pool; reclaim destroys it once the card drains.
				_ = os.Remove(markerPath(deviceplugin.OperatorPodsDir, string(pod.UID), ctr.Name, r.card))
			case migRebound:
				// A prior allocation owns this marker and instance; leave both intact.
			}
			unlock()
		}
	}

	for _, cardUUID := range cards {
		computeSlices, memorySlices, ok := profileGeometry(devs, cardUUID, profile)
		if !ok {
			rollback()
			return nil, fmt.Errorf("card %s has no physical-slice profile %q", cardUUID, profile)
		}
		// The card is addressed before the reservation, so a card whose ordinal cannot be shown to reach
		// the card the detector measured costs no create: it is refused with a warning rather than
		// addressed, since addressing it would carve a partition on one card and hand the container
		// another card's node and capability tree.
		ordinal, cardNode, aerr := requireCardNode(devs, cardUUID)
		if aerr != nil {
			s.Logger.Info("refusing a partition on a card whose device node cannot be shown to address it; "+
				"the node named by the card's accelerator index must carry the minor number the detector "+
				"recorded for that card",
				"card", cardUUID, "profile", profile, "container", ctr.Name, "reason", aerr.Error())
			rollback()
			return nil, aerr
		}

		unlock := lockCard(cardUUID)
		inst, outcome, err := reserveMigInstance(
			s.mig, deviceplugin.OperatorPodsDir, string(pod.UID), ctr.Name, cardUUID, profile, computeSlices, memorySlices)
		unlock()
		if err != nil {
			rollback()
			return nil, err
		}
		results = append(results, cardResult{card: cardUUID, inst: inst, outcome: outcome})

		specs, derr := partitionDeviceSpecs(ordinal, cardNode, inst)
		if derr != nil {
			rollback()
			return nil, fmt.Errorf("card %s partition device nodes: %w", cardUUID, derr)
		}
		devices = append(devices, specs...)

		res := resourceForCard(devs, cardUUID)
		placements[res] = []workercore.AcceleratorPhysicalPlacement{
			{Start: inst.Placement.Start, Length: inst.Placement.Length},
		}
	}

	return &deviceplugin.PhysicalSlicedAllocation{
		Profile:    profile,
		Placements: placements,
		Response:   &deviceplugin.ContainerAllocateResponse{Devices: devices},
		Rollback:   rollback,
	}, nil
}
