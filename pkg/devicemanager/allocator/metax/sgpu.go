package metax

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// maxSGPUPerCard bounds the sgpu subdevices a single MetaX card can host
// (DefaultDevCnt=16). A card's lowest-free index and the cap are both derived
// against the live driver registry UNION the on-disk markers, never markers alone.
const maxSGPUPerCard = 16

// wholeCardIndex marks an occupancy marker for a whole-card slice: the native
// whole-card path creates no sgpu subdevice, but the marker still records the card
// as taken so the on-disk scanner never double-books it.
const wholeCardIndex = -1

// markerName is the per-container correlation + slot-ledger file written under the
// pod work dir. It is the restart-surviving analog of Ascend's npu_info.config.
const markerName = "metax-sgpu.json"

// sgpuAliasPrefix tags an operator-created sgpu subdevice with its owning pod UID.
// It is the marker-independent correlation key that makes the crash-orphan reclaim
// rule safe: a marker-less subdevice is destroyed only when its embedded UID is dead.
// A subdevice whose alias the driver does not expose reads back empty, and reclaim
// then treats it conservatively (never auto-destroys an unidentifiable subdevice).
const sgpuAliasPrefix = "gpustack-"

// reclaimMaxMisses matches deviceplugin's podDirGC: a pod UID must be absent from the
// live set for this many consecutive reconciles before its slice is reclaimed, so a
// transient list gap never destroys a live slice.
const reclaimMaxMisses = 3

// allocMu serializes the whole scan -> validate -> create -> write-marker cycle in
// Allocate AND the scan -> destroy critical section in the reclaim loop. The
// device-plugin responder runs outside the reconciler's allocateMutex and kubelet
// does not serialize Allocate (concurrent Kueue batches), so without this two
// allocations could derive the same free sgpu index, or reclaim could observe an
// in-flight create-before-marker window and destroy a live slice.
var allocMu sync.Mutex

// schedClass is a MetaX sgpu QoS scheduling class. The operator always uses
// fixed-share so .sliced.cores-percentage is a hard compute quota (not best-effort).
type schedClass int

const (
	schedBestEffort schedClass = 0
	schedFixedShare schedClass = 1
	schedBurstShare schedClass = 2
)

// sgpuSubdevice is one sgpu subdevice enumerated from the driver registry: the card
// it lives on, its per-card index, and the alias the operator tagged it with (empty
// when the driver does not expose the tag).
type sgpuSubdevice struct {
	bdf   string
	index int
	alias string
}

// sgpuManager is the injectable seam over the MetaX sysfs sgpu controls. The real
// impl writes /sys/bus/pci/devices/<bdf>/sgpu/*; the test impl uses a fake root so
// the encode / marker / slot-derivation / reclaim logic is table-tested without
// hardware. List is global (returns every card's subdevices, each carrying its bdf)
// so the reclaim loop can catch a crash-orphan on a card that has no marker yet.
type sgpuManager interface {
	// EnsureModel puts the card into sgpu mode. Called only when the card has no
	// existing subdevice (a card already hosting subdevices is already in sgpu mode).
	EnsureModel(bdf string) error
	// SetSchedClass sets the card's QoS scheduling class. Called only when the card
	// has no existing operator subdevice, so a live card is never mutated in place.
	SetSchedClass(bdf string, c schedClass) error
	// Create creates a subdevice at the operator-derived index with the given VRAM
	// quota (MiB) and correlation alias.
	Create(bdf string, index int, vramMiB int64, alias string) error
	// Remove destroys the subdevice at index on bdf. It is idempotent: removing an
	// already-absent subdevice is not an error, so a subdevice-less marker cleans up
	// cleanly.
	Remove(bdf string, index int) error
	// List enumerates every sgpu subdevice across all cards.
	List() ([]sgpuSubdevice, error)
}

// encodeAlias tags a subdevice with its owning pod UID.
func encodeAlias(podUID string) string {
	return sgpuAliasPrefix + podUID
}

// decodeAliasUID extracts the pod UID an operator alias carries. It returns false for
// an empty or foreign (non-operator) alias, so reclaim can distinguish "our orphan"
// from an untaggable / third-party subdevice.
func decodeAliasUID(alias string) (string, bool) {
	uid, ok := strings.CutPrefix(alias, sgpuAliasPrefix)
	if !ok || uid == "" {
		return "", false
	}
	return uid, true
}

// encodeMetaxSGPUs renders the METAX_SGPUS env entry the MXMACA runtime reads for a
// partial slice: sgpu=<BDF>#<idx>;compute=<cores%>;vram=<MiB>;alias=<tag>.
func encodeMetaxSGPUs(bdf string, index, coresPct int, vramMiB int64, alias string) string {
	return fmt.Sprintf("sgpu=%s#%d;compute=%d;vram=%d;alias=%s", bdf, index, coresPct, vramMiB, alias)
}

// sgpuMarker is one parsed marker: the pod<->subdevice correlation and slot ledger a
// scanner treats as occupied and the reclaim loop keys its liveness decision on.
// Index is wholeCardIndex for a whole-card occupancy marker (no subdevice).
type sgpuMarker struct {
	PodUID    string `json:"podUID"`
	Container string `json:"container"`
	CardBDF   string `json:"cardBDF"`
	Index     int    `json:"index"`
	CoresPct  int    `json:"coresPct"`
	MemMiB    int64  `json:"memMiB"`
}

func (m sgpuMarker) wholeCard() bool { return m.Index == wholeCardIndex }

// markerEntry pairs a parsed marker with its on-disk path so reclaim removes only the
// specific marker file (never a sibling container's).
type markerEntry struct {
	path   string
	marker sgpuMarker
}

// markerPath returns the marker file path for a sliced container.
func markerPath(podUID, container string) string {
	return filepath.Join(deviceplugin.PodWorkDir(podUID, container), markerName)
}

// parseMarker reads a marker fail-closed: a missing / malformed / incomplete record
// is an error, so the caller (self-marker reuse, reclaim) never silently mis-reads a
// live slice's correlation.
func parseMarker(path string) (sgpuMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sgpuMarker{}, err
	}
	var m sgpuMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return sgpuMarker{}, fmt.Errorf("marker %q: %w", path, err)
	}
	if m.PodUID == "" || m.CardBDF == "" || m.Index < wholeCardIndex {
		return sgpuMarker{}, fmt.Errorf("marker %q: incomplete record", path)
	}
	return m, nil
}

// writeMarker publishes a marker via a temp file + atomic rename, so a concurrent
// scanner never reads a partially written record.
func writeMarker(path string, m sgpuMarker) error {
	dir := filepath.Dir(path)
	if err := osx.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("create marker dir %q: %w", dir, err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".metax-sgpu-*.tmp")
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

// scanMarkers parses every per-container marker under podsDir. It is lenient by
// design: an unparseable marker is collected as a corrupt path (for the caller to
// log) rather than failing the whole scan, because index occupancy is backstopped by
// the driver registry union — a corrupt marker whose subdevice still exists is
// counted via the registry, and one whose subdevice is gone leaves a genuinely free
// index. The fail-closed guard lives at the self-marker reuse check instead, so a
// corrupt marker fails only the owning pod's allocation (scoped to that card), never
// all of the vendor's allocations node-wide.
func scanMarkers(podsDir string) (entries []markerEntry, corrupt []string) {
	_ = filepath.WalkDir(podsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			// A directory read error is recorded as corrupt scope, not fatal.
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

// usedIndexesOnCard collects the sgpu indexes occupied on bdf from the driver
// registry UNION the partial markers. The registry is the backstop: a crash-orphaned
// subdevice with no marker still occupies its index, so deriving against markers
// alone could double-book.
func usedIndexesOnCard(registry []sgpuSubdevice, markers []markerEntry, bdf string) map[int]bool {
	used := make(map[int]bool)
	for i := range registry {
		if registry[i].bdf == bdf {
			used[registry[i].index] = true
		}
	}
	for i := range markers {
		m := markers[i].marker
		if m.CardBDF == bdf && !m.wholeCard() {
			used[m.Index] = true
		}
	}
	return used
}

// lowestFreeIndex returns the lowest sgpu index in [0, maxSGPUPerCard) not in used,
// or an error when the per-card pool is exhausted.
func lowestFreeIndex(used map[int]bool) (int, error) {
	for i := 0; i < maxSGPUPerCard; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("card sgpu pool exhausted (%d subdevices)", maxSGPUPerCard)
}

// sliceResult is what reserveSlice hands back to the responder: a whole-card slice
// injects no METAX_SGPUS entry (the native whole-card devices are returned instead),
// while a partial slice carries the created subdevice's index and env entry.
type sliceResult struct {
	wholeCard bool
	index     int
	envValue  string
}

// reserveSlice is the Allocate core. Under allocMu it: (1) reuses an existing
// self-marker on an exact (card, cores%, memMiB) match and fails closed on a
// mismatch (pod resource requests are immutable, so a mismatch signals corruption);
// (2) for a whole-card slice writes only an occupancy marker; (3) for a partial
// slice ensures the card is in sgpu mode with a fixed-share class (only when the card
// has no existing subdevice, so a live card is never mutated), derives the lowest
// free index against the registry UNION the markers, creates the subdevice, and
// writes the marker. wholeCard is decided by the caller (cores>=100 AND mem>=VRAM).
func reserveSlice(mgr sgpuManager, podUID, container, bdf string, coresPct int, memMiB int64, wholeCard bool) (sliceResult, error) {
	allocMu.Lock()
	defer allocMu.Unlock()

	registry, err := mgr.List()
	if err != nil {
		return sliceResult{}, fmt.Errorf("list sgpu subdevices: %w", err)
	}

	self := markerPath(podUID, container)
	if m, err := parseMarker(self); err == nil {
		// Idempotent reuse: the request is immutable, so only an exact match is valid.
		if m.CardBDF != bdf || m.CoresPct != coresPct || m.MemMiB != memMiB || m.wholeCard() != wholeCard {
			return sliceResult{}, fmt.Errorf(
				"marker %q mismatches request (card=%s cores=%d mem=%d wholeCard=%t): fail closed",
				self, bdf, coresPct, memMiB, wholeCard)
		}
		if m.wholeCard() {
			return sliceResult{wholeCard: true}, nil
		}
		// Fail closed if the marker's subdevice is gone from the driver registry (out-of-band
		// GC / manual cleanup): reusing it would inject METAX_SGPUS for a non-existent
		// subdevice and start a misconfigured container. A truly dead pod's marker is removed
		// by the reclaim loop, so a live pod reaching this is a real fault.
		if !subdeviceOnCard(registry, bdf, m.Index) {
			return sliceResult{}, fmt.Errorf(
				"marker %q references missing sgpu subdevice %s#%d: fail closed", self, bdf, m.Index)
		}
		return sliceResult{index: m.Index, envValue: encodeMetaxSGPUs(bdf, m.Index, coresPct, memMiB, encodeAlias(podUID))}, nil
	} else if !os.IsNotExist(err) {
		// The marker exists but is corrupt: fail closed for this pod (this card) only.
		return sliceResult{}, fmt.Errorf("read self marker %q: %w", self, err)
	}

	markers, _ := scanMarkers(deviceplugin.OperatorPodsDir)

	if wholeCard {
		// A whole card cannot be taken while it hosts partial slices or is already held
		// whole by another pod (accounting should prevent this; fail closed if it slips
		// through rather than expose a shared card).
		if len(usedIndexesOnCard(registry, markers, bdf)) > 0 || cardTakenWhole(markers, bdf) {
			return sliceResult{}, fmt.Errorf("card %s is already occupied, cannot take it whole", bdf)
		}
		m := sgpuMarker{PodUID: podUID, Container: container, CardBDF: bdf, Index: wholeCardIndex, CoresPct: coresPct, MemMiB: memMiB}
		if err := writeMarker(self, m); err != nil {
			return sliceResult{}, err
		}
		return sliceResult{wholeCard: true}, nil
	}

	// A partial slice must never land on a card already taken whole by another pod.
	if cardTakenWhole(markers, bdf) {
		return sliceResult{}, fmt.Errorf("card %s is held whole, cannot slice it", bdf)
	}

	used := usedIndexesOnCard(registry, markers, bdf)
	index, err := lowestFreeIndex(used)
	if err != nil {
		return sliceResult{}, fmt.Errorf("card %s: %w", bdf, err)
	}

	// A card with no existing subdevice needs its mode + fixed-share class set once;
	// a card already hosting subdevices is already configured, and re-writing the
	// class of a live card is rejected by the driver, so never touch it.
	if !cardHasSubdevice(registry, bdf) {
		if err := mgr.EnsureModel(bdf); err != nil {
			return sliceResult{}, fmt.Errorf("card %s: ensure sgpu model: %w", bdf, err)
		}
		if err := mgr.SetSchedClass(bdf, schedFixedShare); err != nil {
			return sliceResult{}, fmt.Errorf("card %s: set fixed-share sched class: %w", bdf, err)
		}
	}

	alias := encodeAlias(podUID)
	if err := mgr.Create(bdf, index, memMiB, alias); err != nil {
		return sliceResult{}, fmt.Errorf("card %s: create sgpu subdevice %d: %w", bdf, index, err)
	}

	m := sgpuMarker{PodUID: podUID, Container: container, CardBDF: bdf, Index: index, CoresPct: coresPct, MemMiB: memMiB}
	if err := writeMarker(self, m); err != nil {
		// Roll back the subdevice so a create-before-marker crash window is not left
		// behind by our own error path (a real crash between the two is handled by the
		// reclaim loop's registry reconciliation).
		_ = mgr.Remove(bdf, index)
		return sliceResult{}, err
	}
	return sliceResult{index: index, envValue: encodeMetaxSGPUs(bdf, index, coresPct, memMiB, alias)}, nil
}

// cardTakenWhole reports whether bdf is held by a whole-card occupancy marker, so a
// partial slice never silently shares a card another pod owns whole (and vice versa).
func cardTakenWhole(markers []markerEntry, bdf string) bool {
	for i := range markers {
		m := markers[i].marker
		if m.CardBDF == bdf && m.wholeCard() {
			return true
		}
	}
	return false
}

// subdeviceOnCard reports whether the driver registry currently holds the sgpu subdevice
// (bdf, index) — used to fail closed when a reused marker references one that is gone.
func subdeviceOnCard(registry []sgpuSubdevice, bdf string, index int) bool {
	for i := range registry {
		if registry[i].bdf == bdf && registry[i].index == index {
			return true
		}
	}
	return false
}

func cardHasSubdevice(registry []sgpuSubdevice, bdf string) bool {
	for i := range registry {
		if registry[i].bdf == bdf {
			return true
		}
	}
	return false
}

// reclaimer is the level-based per-vendor reclaim loop's state. It is driven by the
// broadcast livePodUIDs AND a periodic resync ticker (Task 2 wiring), so a dropped
// broadcast tick never starves reclamation. Each reconcile re-scans the registry and
// markers, so it self-heals across restarts with no in-memory slot counter.
type reclaimer struct {
	mgr     sgpuManager
	podsDir string
	logger  klog.Logger
	misses  map[string]int // pod UID -> consecutive absent reconciles
}

func newReclaimer(mgr sgpuManager, podsDir string, logger klog.Logger) *reclaimer {
	return &reclaimer{mgr: mgr, podsDir: podsDir, logger: logger, misses: make(map[string]int)}
}

// cardMissPrefix namespaces a per-card miss counter (drained-card orphan reclaim) in
// the same misses map as the per-pod counters; pod UIDs are UUIDs, so they never
// collide with this prefix.
const cardMissPrefix = "card:"

// reconcile reconciles the driver registry against the on-disk markers for one live
// pod-UID snapshot. It takes allocMu so it never races an in-flight Allocate's
// create-before-marker window. Every liveness decision is debounced by
// reclaimMaxMisses consecutive absent reconciles, so a transient list gap never
// reclaims live state. It reconciles in three directions:
//   - a marker whose pod is dead (per-pod decision) -> destroy its subdevice
//     (tolerating an already-absent one, which covers the subdevice-less-marker case)
//     and remove only that marker file (the pod dir only when empty);
//   - a marker-less subdevice whose embedded UID is dead -> destroy it; one whose
//     embedded UID is still live (a create-before-marker crash on a reserved pod) is
//     left intact;
//   - a marker-less subdevice with no decodable alias (the driver did not expose the
//     tag, as on current MetaX) is reclaimed only once its card hosts no live-reserved
//     pod at all (a per-card decision) — trading a bounded leak, reclaimed when the
//     card drains, against ever destroying a live slice. A card still hosting any live
//     pod keeps such subdevices intact (one could be that pod's create-before-marker
//     crash orphan).
func (r *reclaimer) reconcile(livePodUIDs []string) {
	allocMu.Lock()
	defer allocMu.Unlock()

	live := sets.New[string](livePodUIDs...)

	registry, err := r.mgr.List()
	if err != nil {
		r.logger.Error(err, "reclaim: list sgpu subdevices, skipping this pass")
		return
	}
	markers, corrupt := scanMarkers(r.podsDir)
	for _, p := range corrupt {
		r.logger.Info("reclaim: skipping unparseable marker", "path", p)
	}

	// Index the markers' partial subdevices so registry orphans can be detected.
	markedSubdev := make(map[subdevKey]bool, len(markers))
	for i := range markers {
		m := markers[i].marker
		if !m.wholeCard() {
			markedSubdev[subdevKey{m.CardBDF, m.Index}] = true
		}
	}

	// Collect reclaim targets per pod UID: the markers to remove and the subdevices to
	// destroy. Markers contribute their (bdf,index); marker-less aliased subdevices
	// contribute their (bdf,index) under their decoded UID. Marker-less subdevices with
	// no decodable owner are held per card, reclaimed only when the card has no live
	// pod. liveOnCard records which cards host a live-reserved pod (via a live marker or
	// a live-UID aliased subdevice), so those cards' unidentifiable subdevices are kept.
	type target struct {
		markers    []markerEntry
		subdevices []subdevKey
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
	unidentified := make(map[string][]subdevKey) // bdf -> marker-less, alias-less subdevices
	liveOnCard := make(map[string]bool)
	for i := range markers {
		m := markers[i].marker
		t := get(m.PodUID)
		t.markers = append(t.markers, markers[i])
		if !m.wholeCard() {
			t.subdevices = append(t.subdevices, subdevKey{m.CardBDF, m.Index})
		}
		if live.Has(m.PodUID) {
			liveOnCard[m.CardBDF] = true
		}
	}
	for i := range registry {
		sd := registry[i]
		if markedSubdev[subdevKey{sd.bdf, sd.index}] {
			continue // has a marker; handled above
		}
		uid, ok := decodeAliasUID(sd.alias)
		if !ok {
			unidentified[sd.bdf] = append(unidentified[sd.bdf], subdevKey{sd.bdf, sd.index})
			continue
		}
		get(uid).subdevices = append(get(uid).subdevices, subdevKey{sd.bdf, sd.index})
		if live.Has(uid) {
			liveOnCard[sd.bdf] = true
		}
	}

	// touched marks every miss key still relevant this pass; the rest are pruned below.
	touched := sets.New[string]()

	// Per-pod liveness decision + debounce.
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
		r.destroy(uid, t.subdevices, t.markers)
	}

	// Per-card drained-card reclaim of unidentifiable subdevices + debounce.
	for bdf, keys := range unidentified {
		key := cardMissPrefix + bdf
		touched.Insert(key)
		if liveOnCard[bdf] {
			r.misses[key] = 0
			continue
		}
		r.misses[key]++
		if r.misses[key] < reclaimMaxMisses {
			continue
		}
		r.destroyCard(key, keys)
	}

	// Drop miss counters no longer relevant (pod gone / card no longer holds orphans).
	for k := range r.misses {
		if !touched.Has(k) {
			delete(r.misses, k)
		}
	}
}

// destroy tears down one dead pod's slices: remove each subdevice (idempotent), then
// remove each marker file and its pod dir when empty. A miss counter is cleared only
// when the whole pod is reclaimed, so a partial failure retries next pass.
func (r *reclaimer) destroy(uid string, subdevices []subdevKey, markers []markerEntry) {
	ok := true
	for _, sd := range subdevices {
		if err := r.mgr.Remove(sd.bdf, sd.index); err != nil {
			r.logger.Error(err, "reclaim: remove subdevice", "podUID", uid, "bdf", sd.bdf, "index", sd.index)
			ok = false
		}
	}
	for i := range markers {
		if err := os.Remove(markers[i].path); err != nil && !os.IsNotExist(err) {
			r.logger.Error(err, "reclaim: remove marker", "path", markers[i].path)
			ok = false
			continue
		}
		// Remove the container dir and the pod dir when they are empty, so a sibling
		// container's live marker is never dropped.
		removeIfEmpty(filepath.Dir(markers[i].path))
		removeIfEmpty(filepath.Dir(filepath.Dir(markers[i].path)))
	}
	if ok {
		delete(r.misses, uid)
		r.logger.Info("reclaim: reclaimed dead pod's slices", "podUID", uid, "subdevices", len(subdevices))
	}
}

// destroyCard removes the unidentifiable marker-less subdevices on a fully drained
// card (no live pod). The miss counter is cleared only when every removal succeeds, so
// a partial failure retries next pass.
func (r *reclaimer) destroyCard(missKey string, subdevices []subdevKey) {
	ok := true
	for _, sd := range subdevices {
		if err := r.mgr.Remove(sd.bdf, sd.index); err != nil {
			r.logger.Error(err, "reclaim: remove orphan subdevice on drained card", "bdf", sd.bdf, "index", sd.index)
			ok = false
		}
	}
	if ok {
		delete(r.misses, missKey)
		r.logger.Info("reclaim: reclaimed unidentifiable orphans on drained card", "count", len(subdevices))
	}
}

// removeIfEmpty removes dir only when it holds no entries, so reclaiming one
// container never orphans a sibling's marker.
func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
}

// subdevKey identifies a subdevice by card and per-card index.
type subdevKey struct {
	bdf   string
	index int
}

// sysfsSGPUManager is the real sgpuManager. It reads and writes the MetaX driver's
// sysfs sgpu controls under root (default /sys/bus/pci/devices).
//
// The exact sysfs schema is a documented hardware open question (see the spec): the
// paths below follow the vendor documentation but are unvalidated on real hardware,
// which is why every write goes through this thin seam so only this type changes when
// the layout is confirmed. All unit tests use a fake manager, not this impl.
type sysfsSGPUManager struct {
	root string
}

func newSysfsSGPUManager() *sysfsSGPUManager {
	return &sysfsSGPUManager{root: "/sys/bus/pci/devices"}
}

func (m *sysfsSGPUManager) cardDir(bdf string) string { return filepath.Join(m.root, bdf, "sgpu") }

func (m *sysfsSGPUManager) EnsureModel(bdf string) error {
	return os.WriteFile(filepath.Join(m.root, bdf, "model"), []byte("sgpu"), 0o600)
}

func (m *sysfsSGPUManager) SetSchedClass(bdf string, c schedClass) error {
	return os.WriteFile(filepath.Join(m.cardDir(bdf), "sched_class"), []byte(strconv.Itoa(int(c))), 0o600)
}

func (m *sysfsSGPUManager) Create(bdf string, index int, vramMiB int64, alias string) error {
	// The driver's create node takes only the VRAM quota (MiB); it assigns the subdevice
	// index itself. The operator passes its predicted lowest-free `index` (for the marker /
	// METAX_SGPUS entry) and ASSUMES the driver honors that slot — UNVERIFIED on real
	// hardware (see the spec's Open Questions); if a real driver diverges, switch this seam
	// to read the created index back from List() before the marker is written. The alias is
	// carried in METAX_SGPUS (the runtime's key), not persisted to sysfs — current MetaX
	// exposes no subdevice tag to read back — so reclaim does not rely on it: a crash-orphan
	// is caught by the marker and, failing that, the drained-card rule in reconcile. The
	// alias-based orphan rule only engages if a future driver exposes the tag via List.
	_ = index
	_ = alias
	return os.WriteFile(filepath.Join(m.cardDir(bdf), "create"), []byte(strconv.FormatInt(vramMiB, 10)), 0o600)
}

func (m *sysfsSGPUManager) Remove(bdf string, index int) error {
	err := os.WriteFile(filepath.Join(m.cardDir(bdf), "destroy"), []byte(strconv.Itoa(index)), 0o600)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (m *sysfsSGPUManager) List() ([]sgpuSubdevice, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var subdevs []sgpuSubdevice
	for _, e := range entries {
		bdf := e.Name()
		subEntries, err := os.ReadDir(m.cardDir(bdf))
		if err != nil {
			continue // not an sgpu-capable card
		}
		for _, se := range subEntries {
			idx, err := strconv.Atoi(strings.TrimPrefix(se.Name(), "sgpu"))
			if err != nil {
				continue
			}
			alias, _ := os.ReadFile(filepath.Join(m.cardDir(bdf), se.Name(), "alias"))
			subdevs = append(subdevs, sgpuSubdevice{bdf: bdf, index: idx, alias: strings.TrimSpace(string(alias))})
		}
	}
	return subdevs, nil
}
