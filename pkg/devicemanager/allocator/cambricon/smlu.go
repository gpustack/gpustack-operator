package cambricon

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// markerName is the per-container correlation + profile ledger file written under the
// pod work dir. It is the restart-surviving analog of Ascend's npu_info.config.
const markerName = "cambricon-smlu.json"

// smluInstancePrefix tags an operator-created sMLU instance name with its owning pod
// UID. cnDev instance names are operator-set, so the name is always a decodable
// correlation key: a marker-less instance is destroyed only when its embedded UID is
// dead, and an instance whose name does not carry the prefix is treated as foreign and
// never touched.
const smluInstancePrefix = "gpustack"

// smluNameSep separates the fields of an encoded instance name.
const smluNameSep = ":"

// smluHashLen bounds the container short-hash suffix. The full name
// (prefix + UID + short hash) stays well under the cnDev 100-byte instance-name buffer:
// 8 + 1 + 36 + 1 + 12 = 58 bytes, so a 36-byte UID and a 63-byte container name always fit.
const smluHashLen = 12

// reclaimMaxMisses matches deviceplugin's podDirGC: a pod UID must be absent from the
// live set for this many consecutive reconciles before its instance is reclaimed, so a
// transient list gap never destroys a live slice.
const reclaimMaxMisses = 3

// allocMu serializes the whole scan -> validate -> create -> write-marker cycle in
// Allocate AND the scan -> destroy critical section in the reclaim loop. The
// device-plugin responder runs outside the reconciler's allocateMutex and kubelet does
// not serialize Allocate (concurrent Kueue batches), so without this two allocations
// could reuse or double-create a profile, or reclaim could observe an in-flight
// create-before-marker window and destroy a live instance.
var allocMu sync.Mutex

// smluInstance is one sMLU instance enumerated from the driver: the accelerator it lives on,
// its operator-assigned name, the profile it was cut from, the compute/VRAM quota, and
// its device node (injected into the container).
type smluInstance struct {
	card      string
	name      string
	profileID int32
	coresPct  int
	memMiB    int64
	devNode   string
}

// smluRef is the minimal identity the reclaim loop needs to destroy an instance: the
// parent accelerator and the instance name.
type smluRef struct {
	card string
	name string
}

// profileKey identifies an sMLU profile by its parent accelerator and profile ID.
type profileKey struct {
	card      string
	profileID int32
}

// smluDriver is the injectable seam over the cnDev sMLU wrappers. The real impl (wired in
// deviceplugin.go on linux) drives cnDev on an accelerator addressed by PCI bus ID; the test impl
// is in-memory so the mapping / name-encoding / marker / reclaim logic is table-tested
// without hardware. Profile reuse and refcount live above this seam (in reserveInstance /
// reclaim) so they are testable; the seam itself is thin create/destroy/list over the
// wrappers. ListInstances is global (every accelerator's instances, each carrying its own) so
// reclaim can catch a crash-orphan on an accelerator that has no marker yet.
type smluDriver interface {
	// EnsureSMLUMode puts the accelerator into sMLU mode.
	EnsureSMLUMode(card string) error
	// CreateProfile creates a profile with the given compute (%) and VRAM (MiB) quota and
	// returns its profile ID.
	CreateProfile(card string, coresPct int, memMiB int64) (int32, error)
	// DestroyProfile destroys a profile. Called only when no instance references it.
	DestroyProfile(card string, profileID int32) error
	// CreateInstance instantiates a named instance from the profile and returns it,
	// including the device node read back from the driver.
	CreateInstance(card string, profileID int32, name string) (smluInstance, error)
	// DestroyInstance destroys the named instance on the accelerator. It is idempotent:
	// destroying an already-absent instance is not an error.
	DestroyInstance(card, name string) error
	// ListInstances enumerates every sMLU instance across all accelerators.
	ListInstances() ([]smluInstance, error)
	// ListProfiles enumerates every sMLU profile across all accelerators, so the reclaim loop
	// can destroy a profile no instance references (including a crash orphan left when a
	// create fell between the profile and its instance).
	ListProfiles() ([]profileKey, error)
}

// smluSetFor maps the decoupled compute (%) and VRAM (MiB) dimensions to the cnDev sMLU
// quota fields: mluQuota is the compute percent directly (no CU conversion), memorySize
// is bytes (MiB << 20). Returned as plain values so the core stays free of the cnDev cgo
// binding; the driver assembles the cnDev SMluSet. Verified against the report but
// hardware-unvalidated (see the spec's Open Questions).
func smluSetFor(coresPct int, memMiB int64) (mluQuota uint32, memorySize uint64) {
	return uint32(coresPct), uint64(memMiB) << 20
}

// encodeInstanceName renders a bounded, decodable instance name
// (<prefix>:<podUID>:<shortHash(container)>). The pod UID is the correlation key; the
// container short hash disambiguates a pod's containers without risking the cnDev
// 100-byte buffer. The full pod<->container<->name map lives in the marker.
func encodeInstanceName(podUID, container string) string {
	short := stringx.SumBySHA256(container)[:smluHashLen]
	return smluInstancePrefix + smluNameSep + podUID + smluNameSep + short
}

// decodeInstanceUID extracts the pod UID an instance name carries. It returns false for
// a name that is not operator-encoded, so reclaim leaves foreign / third-party instances
// alone.
func decodeInstanceUID(name string) (string, bool) {
	parts := strings.Split(name, smluNameSep)
	if len(parts) != 3 || parts[0] != smluInstancePrefix || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// smluMarker is one parsed marker: the pod<->instance correlation and profile ledger a
// scanner treats as occupied and the reclaim loop keys its liveness decision on.
//
// The Card field and its "card" JSON tag are an ON-DISK FORMAT, not vocabulary: markers
// written before this package spoke of accelerators are still on real nodes, and renaming
// either would make them unreadable and break retry, visibility, adoption and reclamation.
// Both are frozen.
type smluMarker struct {
	PodUID    string `json:"podUID"`
	Container string `json:"container"`
	Card      string `json:"card"`
	Instance  string `json:"instance"`
	ProfileID int32  `json:"profileID"`
	CoresPct  int    `json:"coresPct"`
	MemMiB    int64  `json:"memMiB"`
}

// markerEntry pairs a parsed marker with its on-disk path so reclaim removes only the
// specific marker file (never a sibling container's).
type markerEntry struct {
	path   string
	marker smluMarker
}

// markerPath returns the marker file path for a sliced container.
func markerPath(podUID, container string) string {
	return filepath.Join(deviceplugin.PodWorkDir(podUID, container), markerName)
}

// parseMarker reads a marker fail-closed: a missing / malformed / incomplete record is
// an error, so the caller (self-marker reuse, reclaim) never silently mis-reads a live
// slice's correlation.
func parseMarker(path string) (smluMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return smluMarker{}, err
	}
	var m smluMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return smluMarker{}, fmt.Errorf("marker %q: %w", path, err)
	}
	if m.PodUID == "" || m.Card == "" || m.Instance == "" {
		return smluMarker{}, fmt.Errorf("marker %q: incomplete record", path)
	}
	return m, nil
}

// writeMarker publishes a marker durably: a concurrent scanner never reads a partial record,
// and a record that has been written survives an unclean shutdown.
func writeMarker(path string, m smluMarker) error {
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

// scanMarkers parses every per-container marker under podsDir. It is lenient by design:
// an unparseable marker is collected as a corrupt path (for the caller to log) rather
// than failing the whole scan; the self-marker reuse check fails closed per pod instead.
func scanMarkers(podsDir string) (entries []markerEntry, corrupt []string) {
	_ = filepath.WalkDir(podsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			corrupt = append(corrupt, path)
			return nil
		}
		if d.IsDir() || d.Name() != markerName {
			return nil
		}
		m, perr := parseMarker(path)
		if perr != nil {
			// A corrupt marker is recorded above and the walk continues; it is not an
			// error to propagate up, so the scan covers every remaining marker.
			corrupt = append(corrupt, path)
			return nil //nolint:nilerr
		}
		entries = append(entries, markerEntry{path: path, marker: m})
		return nil
	})
	return entries, corrupt
}

// reserveInstance is the Allocate core. Under allocMu it: (1) reuses an existing
// self-marker on an exact (accelerator, cores%, memMiB) match, recovering the live
// instance's device node, and fails closed on a mismatch (pod resource requests are
// immutable, so a mismatch signals corruption); (2) otherwise ensures the accelerator is in
// sMLU mode, reuses a profile on an exact (cores%, memMiB) match on that accelerator or
// creates one, instantiates a named instance, and writes the marker (rolling back the
// instance + a freshly created profile if the marker write fails).
func reserveInstance(driver smluDriver, podUID, container, card string, coresPct int, memMiB int64) (smluInstance, error) {
	allocMu.Lock()
	defer allocMu.Unlock()

	self := markerPath(podUID, container)
	if m, err := parseMarker(self); err == nil {
		// Idempotent reuse: the request is immutable, so only an exact match is valid.
		if m.Card != card || m.CoresPct != coresPct || m.MemMiB != memMiB {
			return smluInstance{}, fmt.Errorf(
				"marker %q mismatches request (card=%s cores=%d mem=%d): fail closed",
				self, card, coresPct, memMiB)
		}
		list, err := driver.ListInstances()
		if err != nil {
			return smluInstance{}, fmt.Errorf("list smlu instances: %w", err)
		}
		for i := range list {
			if list[i].name == m.Instance {
				return list[i], nil
			}
		}
		// The marker survives but its instance is gone (create crash / external teardown):
		// fail closed rather than silently re-create under a reused marker; the reclaim
		// loop cleans the instance-less marker.
		return smluInstance{}, fmt.Errorf("marker %q references missing instance %q: fail closed", self, m.Instance)
	} else if !os.IsNotExist(err) {
		return smluInstance{}, fmt.Errorf("read self marker %q: %w", self, err)
	}

	if err := driver.EnsureSMLUMode(card); err != nil {
		return smluInstance{}, fmt.Errorf("card %s: ensure smlu mode: %w", card, err)
	}

	list, err := driver.ListInstances()
	if err != nil {
		return smluInstance{}, fmt.Errorf("list smlu instances: %w", err)
	}
	profileID, created, err := findOrCreateProfile(driver, list, card, coresPct, memMiB)
	if err != nil {
		return smluInstance{}, err
	}

	name := encodeInstanceName(podUID, container)
	inst, err := driver.CreateInstance(card, profileID, name)
	if err != nil {
		if created {
			_ = driver.DestroyProfile(card, profileID)
		}
		return smluInstance{}, fmt.Errorf("card %s: create smlu instance: %w", card, err)
	}

	m := smluMarker{PodUID: podUID, Container: container, Card: card, Instance: name, ProfileID: profileID, CoresPct: coresPct, MemMiB: memMiB}
	if err := writeMarker(self, m); err != nil {
		// Roll back so a create-before-marker window is not left behind by our own error
		// path (a real crash between the two is handled by the reclaim loop).
		_ = driver.DestroyInstance(card, name)
		if created {
			_ = driver.DestroyProfile(card, profileID)
		}
		return smluInstance{}, err
	}
	return inst, nil
}

// findOrCreateProfile reuses a profile on the accelerator whose compute + VRAM quota exactly
// matches the request (a differing quota would violate the requested isolation), or
// creates a new one. It returns the profile ID and whether it was freshly created (so a
// failed instance create can roll the new profile back).
func findOrCreateProfile(driver smluDriver, list []smluInstance, card string, coresPct int, memMiB int64) (int32, bool, error) {
	for i := range list {
		if list[i].card == card && list[i].coresPct == coresPct && list[i].memMiB == memMiB {
			return list[i].profileID, false, nil
		}
	}
	id, err := driver.CreateProfile(card, coresPct, memMiB)
	if err != nil {
		return 0, false, fmt.Errorf("card %s: create smlu profile: %w", card, err)
	}
	return id, true, nil
}

// reclaimer is the level-based per-manufacturer reclaim loop's state. It is driven by the
// broadcast livePodUIDs AND a periodic resync ticker (deviceplugin.go wiring), so a
// dropped broadcast tick never starves reclamation. Each reconcile re-lists the driver
// and scans the markers, so it self-heals across restarts with no in-memory counter.
type reclaimer struct {
	driver  smluDriver
	podsDir string
	logger  klog.Logger
	misses  map[string]int // pod UID -> consecutive absent reconciles
}

func newReclaimer(driver smluDriver, podsDir string, logger klog.Logger) *reclaimer {
	return &reclaimer{driver: driver, podsDir: podsDir, logger: logger, misses: make(map[string]int)}
}

// reconcile reconciles the driver's instances against the on-disk markers for one live
// pod-UID snapshot. It takes allocMu so it never races an in-flight Allocate's
// create-before-marker window. Every liveness decision is debounced by reclaimMaxMisses
// consecutive absent reconciles, so a transient list gap never reclaims live state. It
// reconciles in two directions:
//   - a marker whose pod is dead (per-pod decision) -> destroy its instance (tolerating
//     an already-absent one, which covers the instance-less-marker case) and remove only
//     that marker file (the pod dir only when empty);
//   - a marker-less instance whose embedded UID is dead -> destroy it; one whose embedded
//     UID is still live (a create-before-marker crash on a reserved pod) is left intact; a
//     name that is not operator-encoded is foreign and never touched.
//
// After the per-pod pass it sweeps profiles once (gcOrphanProfiles), destroying any that
// no instance references.
func (r *reclaimer) reconcile(livePodUIDs []string) {
	allocMu.Lock()
	defer allocMu.Unlock()

	live := sets.New[string](livePodUIDs...)

	instances, err := r.driver.ListInstances()
	if err != nil {
		r.logger.Error(err, "reclaim: list smlu instances, skipping this pass")
		return
	}
	markers, corrupt := scanMarkers(r.podsDir)
	for _, p := range corrupt {
		r.logger.Info("reclaim: skipping unparseable marker", "path", p)
	}

	// Index the markers' instance names so registry orphans can be detected.
	markedName := make(map[string]bool, len(markers))
	for i := range markers {
		markedName[markers[i].marker.Instance] = true
	}

	// Collect reclaim targets per pod UID: the marker files to remove and the instances
	// to destroy. Markers contribute their (accelerator, name, profileID); marker-less instances
	// contribute theirs under their decoded UID (an undecodable name is foreign, skipped).
	type target struct {
		markers   []markerEntry
		instances []smluRef
	}
	targets := make(map[string]*target)
	get := func(uid string) *target {
		t := targets[uid]
		if t == nil {
			t = &target{}
			targets[uid] = t
		}
		return t
	}
	for i := range markers {
		m := markers[i].marker
		t := get(m.PodUID)
		t.markers = append(t.markers, markers[i])
		t.instances = append(t.instances, smluRef{card: m.Card, name: m.Instance})
	}
	for i := range instances {
		inst := instances[i]
		if markedName[inst.name] {
			continue // has a marker; handled above
		}
		uid, ok := decodeInstanceUID(inst.name)
		if !ok {
			continue // not operator-encoded; leave foreign instances alone
		}
		get(uid).instances = append(get(uid).instances, smluRef{card: inst.card, name: inst.name})
	}

	// touched marks every miss key still relevant this pass; the rest are pruned below.
	touched := sets.New[string]()
	for uid, t := range targets {
		touched.Insert(uid)
		if live.Has(uid) {
			r.misses[uid] = 0
			continue
		}
		r.misses[uid]++
		if r.misses[uid] < reclaimMaxMisses {
			continue
		}
		r.destroy(uid, t.instances, t.markers)
	}

	for k := range r.misses {
		if !touched.Has(k) {
			delete(r.misses, k)
		}
	}

	// Sweep profiles once per pass: destroy any profile no instance references — one freed
	// by the destroys above, or a crash orphan left when a create fell between the profile
	// and its instance. Safe under allocMu: no in-flight Allocate holds an as-yet
	// instance-less profile.
	r.gcOrphanProfiles()
}

// destroy tears down one dead pod's instances: destroy each instance (idempotent), then
// remove each marker file and its pod dir when empty. A miss counter is cleared only when
// the whole pod is reclaimed, so a partial failure retries next pass. Freed profiles are
// reclaimed by gcOrphanProfiles at the end of the reconcile pass.
func (r *reclaimer) destroy(uid string, instances []smluRef, markers []markerEntry) {
	ok := true
	for _, inst := range instances {
		if err := r.driver.DestroyInstance(inst.card, inst.name); err != nil {
			r.logger.Error(err, "reclaim: destroy instance", "podUID", uid, "card", inst.card, "instance", inst.name)
			ok = false
			continue
		}
	}
	for i := range markers {
		if err := os.Remove(markers[i].path); err != nil && !os.IsNotExist(err) {
			r.logger.Error(err, "reclaim: remove marker", "path", markers[i].path)
			ok = false
			continue
		}
		// Remove the container dir and the pod dir when empty, so a sibling container's
		// live marker is never dropped.
		removeIfEmpty(filepath.Dir(markers[i].path))
		removeIfEmpty(filepath.Dir(filepath.Dir(markers[i].path)))
	}
	if ok {
		delete(r.misses, uid)
		r.logger.Info("reclaim: reclaimed dead pod's instances", "podUID", uid, "instances", len(instances))
	}
}

// gcOrphanProfiles destroys every profile no surviving instance references, so a profile
// shared by a live instance is never torn out from under it while a genuinely orphaned one
// (its instances reclaimed, or a create-before-instance crash) is freed. It fails closed:
// a list error skips the sweep rather than acting on a partial view.
func (r *reclaimer) gcOrphanProfiles() {
	profiles, err := r.driver.ListProfiles()
	if err != nil {
		r.logger.Error(err, "reclaim: list smlu profiles for gc, skipping")
		return
	}
	if len(profiles) == 0 {
		return
	}
	instances, err := r.driver.ListInstances()
	if err != nil {
		r.logger.Error(err, "reclaim: list smlu instances for profile gc, skipping")
		return
	}
	inUse := make(map[profileKey]bool, len(instances))
	for i := range instances {
		inUse[profileKey{card: instances[i].card, profileID: instances[i].profileID}] = true
	}
	for _, pk := range profiles {
		if inUse[pk] {
			continue
		}
		if err := r.driver.DestroyProfile(pk.card, pk.profileID); err != nil {
			r.logger.Error(err, "reclaim: destroy orphan profile", "card", pk.card, "profileID", pk.profileID)
		}
	}
}

// removeIfEmpty removes dir only when it holds no entries, so reclaiming one container
// never orphans a sibling's marker.
func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
}
