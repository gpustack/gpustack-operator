package nvidia

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// markerName is the per-container-per-card MIG ownership marker written under the pod work
// dir. It is the restart-surviving record reclaim destroys the exact GPU/compute instance
// from, and that lets a create tell a reusable unbound instance from a bound one. It is the
// NVIDIA analog of MetaX's metax-sgpu.json / Cambricon's cambricon-smlu.json.
const markerName = "nvidia-mig.json"

// migPlacement is a memory-slice interval [Start, Start+Length) on a card — the platform-
// independent placement geometry the slot-pick and marker use, so the pure core is testable
// on darwin. The NVML placement type stays inside the _linux seam.
type migPlacement struct {
	Start  int32
	Length int32
}

// migInstance is one live or freshly created MIG partition on a card: its GPU-instance and
// compute-instance ids, the compute-slice count (which, with the placement length, pins the
// profile geometry), the memory-slice placement, and the MIG-device UUID injected as
// NVIDIA_VISIBLE_DEVICES.
type migInstance struct {
	GiID          uint32
	CiID          uint32
	ComputeSlices int32
	Placement     migPlacement
	UUID          string
}

// migCardState is a card's live MIG state read for one profile: the profile's legal
// empty-card placements and every live GPU instance on the card (across all profiles). The
// core subtracts the live placements (occupied) from Possible to pick a free slot, and
// scans Live for a reusable unbound instance.
type migCardState struct {
	Possible []migPlacement
	Live     []migInstance
}

// migDriver is the NVML actuator seam behind a _linux.go/_other.go build tag: the real impl
// drives binding/nvml on linux; the darwin stub errors, so the pure marker/slot-pick core is
// table-tested with a fake driver. Every op addresses a card by its GPU UUID (the operator
// accelerator ID for NVIDIA). CardState/CreateInstance take the profile's geometry because a
// compute-slice count alone cannot pick the GPU-instance profile id for same-compute REV
// profiles (1g.5gb vs 1g.10gb) — the seam matches the profile by name.
type migDriver interface {
	// CardState reads the card's live MIG state for the given profile (its legal empty-card
	// placements and every live GPU instance on the card).
	CardState(cardUUID, profile string, computeSlices, memorySlices int32) (migCardState, error)
	// CreateInstance materializes a GPU instance of the profile at slot plus its whole-GI
	// compute instance, returning the created partition (ids, placement, MIG UUID).
	CreateInstance(cardUUID, profile string, computeSlices, memorySlices int32, slot migPlacement) (migInstance, error)
	// DestroyInstance tears down the partition's compute instance then its GPU instance.
	DestroyInstance(cardUUID string, inst migInstance) error
}

// cardLocks holds a per-card mutex guarding the create+marker-write (and reclaim destroy)
// critical section, so concurrent Allocates on the same card serialize their slot selection
// while sibling cards proceed in parallel. The node-wide allocateMutex keeps only its short
// identify→reserve role (see deviceplugin.server). It is keyed by GPU UUID.
var cardLocks sync.Map // cardUUID -> *sync.Mutex

// lockCard locks the card's mutex and returns its unlock func.
func lockCard(cardUUID string) func() {
	m, _ := cardLocks.LoadOrStore(cardUUID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// migMarker is one parsed per-container-per-card ownership record: the pod<->partition
// correlation reclaim keys its liveness decision on, and the create's idempotent-retry and
// reuse checks read.
type migMarker struct {
	PodUID        string `json:"podUID"`
	Container     string `json:"container"`
	Card          string `json:"card"`
	Profile       string `json:"profile"`
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
		ComputeSlices: m.ComputeSlices,
		Placement:     migPlacement{Start: m.Start, Length: m.Length},
		UUID:          m.MigUUID,
	}
}

// markerEntry pairs a parsed marker with its on-disk path so reclaim removes only the
// specific marker file (never a sibling's).
type markerEntry struct {
	path   string
	marker migMarker
}

// markerFileName is the per-card marker file name: nvidia-mig-<card>.json, so a container
// spanning multiple cards keeps one independent marker per card, each guarded by that card's
// lock. The card UUID is used verbatim (NVIDIA GPU UUIDs are filesystem-safe).
func markerFileName(cardUUID string) string {
	return strings.TrimSuffix(markerName, ".json") + "-" + cardUUID + ".json"
}

// markerPath returns the marker file path for a sliced container on a card.
func markerPath(podUID, container, cardUUID string) string {
	return filepath.Join(deviceplugin.PodWorkDir(podUID, container), markerFileName(cardUUID))
}

// parseMarker reads a marker fail-closed: a missing/malformed/incomplete record is an error,
// so the self-marker reuse and reclaim never silently mis-read a live partition.
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

// writeMarker publishes a marker via a temp file + atomic rename, so a concurrent scanner
// never reads a partially written record.
func writeMarker(path string, m migMarker) error {
	dir := filepath.Dir(path)
	if err := osx.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("create marker dir %q: %w", dir, err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".nvidia-mig-*.tmp")
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

// scanMarkers parses every MIG marker under podsDir. Like MetaX it is lenient: an
// unparseable marker is collected as a corrupt path (for the caller to log) rather than
// failing the whole scan; the fail-closed guard lives at the self-marker reuse check, scoped
// to the owning pod's allocation on that card.
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

// isMarkerFile reports whether name is a MIG marker file (nvidia-mig-<card>.json).
func isMarkerFile(name string) bool {
	return strings.HasPrefix(name, strings.TrimSuffix(markerName, ".json")+"-") &&
		strings.HasSuffix(name, ".json")
}

// ownedGiIDsOnCard returns the set of GPU-instance ids on cardUUID that any marker owns, so
// the reuse check can tell a bound instance from a reusable unbound one.
func ownedGiIDsOnCard(entries []markerEntry, cardUUID string) map[uint32]bool {
	owned := make(map[uint32]bool)
	for i := range entries {
		if entries[i].marker.Card == cardUUID {
			owned[entries[i].marker.GiID] = true
		}
	}
	return owned
}

// reuseUnboundInstance returns a live instance on the card whose geometry matches the profile
// (compute slices + memory-slice length) and that no marker owns — an unbound instance a
// crashed create or an out-of-band tool left behind, which binding avoids re-creating a
// colliding one. It returns ok=false when none is reusable. It skips an instance with an empty
// MIG-device UUID: that is a GPU instance with no materialized compute instance (a crash
// between GI and CI create, exactly the leftover this path targets), which exposes no MIG
// device — binding it would inject an empty NVIDIA_VISIBLE_DEVICES and persist a UUID-less
// marker that later fails closed. Such a GI is left for reclaim to destroy.
func reuseUnboundInstance(state migCardState, owned map[uint32]bool, computeSlices, memorySlices int32) (migInstance, bool) {
	for i := range state.Live {
		inst := state.Live[i]
		if inst.UUID == "" {
			continue
		}
		if inst.ComputeSlices == computeSlices && inst.Placement.Length == memorySlices && !owned[inst.GiID] {
			return inst, true
		}
	}
	return migInstance{}, false
}

// pickPlacement returns the free placement with the smallest Start that overlaps no occupied
// interval, and ok=false when the card is full. Deterministic (lowest slot first) so
// concurrent creates on the same card under the per-card lock never pick the same slot.
func pickPlacement(possible, occupied []migPlacement) (migPlacement, bool) {
	sorted := make([]migPlacement, len(possible))
	copy(sorted, possible)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
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

// migReserveOutcome tells the caller how a reservation resolved, so its rollback undoes
// exactly what this call did and nothing a prior allocation still owns:
//   - migCreated: a fresh GI+CI was created and its marker written — rollback destroys it and
//     removes the marker;
//   - migBound: a pre-existing unbound instance was adopted and a marker written — rollback
//     removes the marker only (the instance was not ours to create), returning it to unbound;
//   - migRebound: an existing self-marker was reused unchanged (a kubelet Allocate retry) —
//     rollback leaves the marker and instance intact (they belong to that prior allocation).
type migReserveOutcome int

const (
	migCreated migReserveOutcome = iota
	migBound
	migRebound
)

// reserveMigInstance is the per-card MIG allocation core, run under the card's lock. It (1)
// reuses an existing self-marker on an exact (card, profile) match — verifying its instance
// still lives — so a kubelet Allocate retry rebinds its own partition instead of double-
// creating; (2) binds a reusable unbound instance of the profile if one is present; (3)
// otherwise picks the lowest free placement and creates a new GI+CI. It writes the ownership
// marker inside the critical section, rolling back a just-created instance if the marker
// write fails. The returned outcome tells the caller's rollback exactly what to undo.
func reserveMigInstance(
	drv migDriver, podsDir, podUID, container, cardUUID, profile string, computeSlices, memorySlices int32,
) (inst migInstance, outcome migReserveOutcome, err error) {
	self := markerPath(podUID, container, cardUUID)
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
		// Guard against GPU-instance id reuse: a destroyed GI's id can be reassigned to a
		// different partition, so verify the live instance's MIG-device UUID still matches the
		// marker before rebinding — otherwise a retry would inject a stale/foreign UUID.
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

	entries, _ := scanMarkers(podsDir)
	owned := ownedGiIDsOnCard(entries, cardUUID)

	if reused, ok := reuseUnboundInstance(state, owned, computeSlices, memorySlices); ok {
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
		PodUID: podUID, Container: container, Card: cardUUID, Profile: profile,
		GiID: inst.GiID, CiID: inst.CiID, MigUUID: inst.UUID,
		ComputeSlices: inst.ComputeSlices, Start: inst.Placement.Start, Length: inst.Placement.Length,
	}
	if werr := writeMarker(self, m); werr != nil {
		if outcome == migCreated {
			// Roll back the just-created instance so a create-before-marker crash window is not
			// left behind by our own error path (a real crash between the two is reclaimed).
			_ = drv.DestroyInstance(cardUUID, inst)
		}
		return migInstance{}, outcome, werr
	}
	return inst, outcome, nil
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

// profileGeometry returns the per-card compute/memory slice geometry of a physical-slice
// profile on the allocated card, read from the card's detect-time capability in devs.Spec.
// It reports ok=false when the card or profile is absent, so the actuator fails the
// allocation rather than guessing geometry.
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

// ActuatePhysicalSliced materializes one MIG partition of profile per allocated card,
// serialized per card by the card lock, records each chosen placement upward for the ledger
// reconciler, and returns the container response injecting only NVIDIA_VISIBLE_DEVICES set to
// the partitions' MIG UUIDs (no libvgpu/CUDA_DEVICE_* soft-slice artifacts). On any card's
// failure it rolls back exactly what this call did — per the per-card outcome — so no half-owned
// Pod persists and no partition a prior allocation owns is touched.
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

	// Allocate onto cards in devs order (= NVIDIA_VISIBLE_DEVICES order), so the injected UUID
	// list is deterministic and matches the ledger's per-card records.
	cards := make([]string, 0, len(allocated))
	for i := range devs.Spec.Groups {
		grp := &devs.Spec.Groups[i]
		for j := range grp.Accelerators {
			res := deviceplugin.Resource{Group: grp.ID, Device: grp.Accelerators[j].ID}
			if _, ok := allocated[res]; ok {
				cards = append(cards, grp.Accelerators[j].ID)
			}
		}
	}
	if len(cards) == 0 {
		return nil, fmt.Errorf("no allocated card for physical-slice container %q", ctr.Name)
	}

	placements := make(map[deviceplugin.Resource][]workercore.AcceleratorPhysicalPlacement, len(cards))
	uuids := make([]string, 0, len(cards))
	// results records how each card resolved so rollback undoes exactly this call's work under
	// the same per-card lock the create took (so it never races a concurrent same-card Allocate's
	// state read, and never removes a marker or destroys an instance a prior allocation owns).
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
				_ = os.Remove(markerPath(string(pod.UID), ctr.Name, r.card))
			case migBound:
				// The instance was pre-existing (adopted), so only drop our ownership marker,
				// returning it to the unbound pool; reclaim destroys it once the card drains.
				_ = os.Remove(markerPath(string(pod.UID), ctr.Name, r.card))
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
		unlock := lockCard(cardUUID)
		inst, outcome, err := reserveMigInstance(
			s.mig, deviceplugin.OperatorPodsDir, string(pod.UID), ctr.Name, cardUUID, profile, computeSlices, memorySlices)
		unlock()
		if err != nil {
			rollback()
			return nil, err
		}
		results = append(results, cardResult{card: cardUUID, inst: inst, outcome: outcome})
		res := resourceForCard(devs, cardUUID)
		placements[res] = []workercore.AcceleratorPhysicalPlacement{{Start: inst.Placement.Start, Length: inst.Placement.Length}}
		uuids = append(uuids, inst.UUID)
	}

	return &deviceplugin.PhysicalSlicedAllocation{
		Profile:    profile,
		Placements: placements,
		Response: &deviceplugin.ContainerAllocateResponse{
			Envs: map[string]string{"NVIDIA_VISIBLE_DEVICES": strings.Join(uuids, ",")},
		},
		Rollback: rollback,
	}, nil
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
