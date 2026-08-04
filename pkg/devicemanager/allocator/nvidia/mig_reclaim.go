package nvidia

import (
	"errors"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
)

// reclaimMaxMisses debounces a liveness decision: a pod (or a drained card's orphans) must be
// absent/idle for this many consecutive reconciles before its partition is destroyed, so a
// transient list gap never reclaims live state. It matches deviceplugin's podDirGC and the
// MetaX/Cambricon loops. Against the 60s resync it is the create-before-marker guard: a
// crash-then-retry Allocate rebinds its GI well within reclaimMaxMisses × resync, so the orphan
// GC never destroys a partition an in-flight retry still owns (spec F4: size it > the kubelet
// Allocate-retry window).
const reclaimMaxMisses = 3

// reclaimMaxDestroyMisses bounds how many consecutive reconciles a destroy may fail with
// NVML_ERROR_IN_USE before the loop surfaces an operator-visible log — a residual process is
// holding the instance. The debounce is never cleared meanwhile, so the destroy keeps retrying
// every pass; sibling cards are never blocked (per-card locks). Devices.Status is rebuilt
// wholesale each reconcile from Spec + Pod annotations, so a status condition would be stomped;
// the log is the operator-visible surface.
const reclaimMaxDestroyMisses = 8

// cardMissPrefix namespaces a per-card orphan-GC miss counter in the same misses map as the
// per-pod counters; pod UIDs are UUIDs, so they never collide with this prefix.
const cardMissPrefix = "card:"

// reclaimer is the level-based MIG reclaim loop's state, driven by the reconciler's broadcast
// live pod-UID set plus a periodic resync ticker (deviceplugin.RunSlicedReclaimLoop). A sliced
// pool has no Release callback, so a Pod's GPU/compute instances are freed here. Each reconcile
// re-scans the markers and re-lists the driver, so it self-heals across restarts with no
// in-memory instance registry. It runs single-threaded (the loop calls reconcile serially), so
// its counter maps need no lock; only the per-card lock coordinates with concurrent Allocates.
type reclaimer struct {
	driver  migDriver
	podsDir string
	logger  klog.Logger
	// liveClaims returns, per card UUID, the physical-slice placements live (non-terminating)
	// Pods currently claim by annotation — the attribution self-check source, so a mis-attributed
	// marker (the oldest-Pending getAllocatingPod heuristic can bind an Allocate to the wrong
	// same-profile Pod) never destroys an instance a running Pod holds. It is injected so the
	// loop is table-tested without a Kubernetes client.
	liveClaims func() (map[string][]migPlacement, error)
	misses     map[string]int // pod UID / "card:<uuid>" -> consecutive absent-or-idle reconciles
	inUse      map[string]int // pod UID -> consecutive IN_USE-failed destroy reconciles
}

func newReclaimer(driver migDriver, podsDir string, logger klog.Logger, liveClaims func() (map[string][]migPlacement, error)) *reclaimer {
	return &reclaimer{
		driver: driver, podsDir: podsDir, logger: logger, liveClaims: liveClaims,
		misses: make(map[string]int), inUse: make(map[string]int),
	}
}

// reconcile reconciles the MIG partitions against the on-disk markers for one live pod-UID
// snapshot. Every liveness decision is debounced by reclaimMaxMisses consecutive absent
// reconciles, so a transient list gap never reclaims live state; each destroy runs under its
// card's lock (never the node-wide mutex) so it never races an in-flight same-card Allocate's
// create+marker window while sibling cards proceed in parallel. It reconciles in two directions:
//   - a marker whose pod is dead -> destroy its GPU instance (CI then GI), unless a running Pod
//     still claims that placement (attribution self-check); NVML_ERROR_IN_USE is a bounded,
//     retryable partial failure (the debounce is not cleared) surfacing a log at the bound;
//   - a marker-less GPU instance (a crash between GI-create and marker-write, or an out-of-band
//     one) is destroyed only once its card is fully drained (no live Pod claims or marks it), as
//     MetaX does for unidentifiable orphans — a MIG GI carries no operator tag, so per-pod
//     attribution of a marker-less GI is impossible.
func (r *reclaimer) reconcile(livePodUIDs []string) {
	live := sets.New[string](livePodUIDs...)

	markers, corrupt := scanMarkers(r.podsDir)
	for _, p := range corrupt {
		r.logger.Info("reclaim: skipping unparseable marker", "path", p)
	}

	// The attribution self-check needs the live claim set; without it fail closed (skip the
	// whole pass) rather than risk destroying an instance a running Pod holds.
	claims, cerr := r.liveClaims()
	if cerr != nil {
		r.logger.Error(cerr, "reclaim: read live pod claims, skipping this pass")
		return
	}

	// The live instance list backs both the marker identity check (a GI id NVML reused after an
	// out-of-band destroy must not be destroyed under a stale marker) and the orphan sweep;
	// without it fail closed (skip the pass) rather than act on an unvalidated view.
	instances, lerr := r.driver.ListInstances()
	if lerr != nil {
		r.logger.Error(lerr, "reclaim: list mig instances, skipping this pass")
		return
	}
	liveByCard := make(map[string]map[uint32]migInstance)
	for i := range instances {
		li := instances[i]
		if liveByCard[li.Card] == nil {
			liveByCard[li.Card] = make(map[uint32]migInstance)
		}
		liveByCard[li.Card][li.Inst.GiID] = li.Inst
	}

	// touched marks every miss key still relevant this pass; the rest are pruned at the end.
	touched := sets.New[string]()

	// A card is "live" while any live Pod marks or claims it, so its marker-less orphans are
	// kept (one could be a live Pod's create-before-marker GI). markedGI indexes every GI a
	// marker owns so orphan detection finds the marker-less ones.
	liveOnCard := make(map[string]bool)
	for card, ps := range claims {
		if len(ps) > 0 {
			liveOnCard[card] = true
		}
	}
	markedGI := make(map[string]map[uint32]bool)
	byPod := make(map[string][]markerEntry)
	for i := range markers {
		m := markers[i].marker
		byPod[m.PodUID] = append(byPod[m.PodUID], markers[i])
		if markedGI[m.Card] == nil {
			markedGI[m.Card] = make(map[uint32]bool)
		}
		markedGI[m.Card][m.GiID] = true
		if live.Has(m.PodUID) {
			liveOnCard[m.Card] = true
		}
	}

	// Per-pod liveness decision + debounce: a dead pod's markers are reclaimed after the bound.
	for uid, entries := range byPod {
		touched.Insert(uid)
		if live.Has(uid) {
			r.misses[uid] = 0
			r.inUse[uid] = 0
			continue
		}
		r.misses[uid]++
		if r.misses[uid] < reclaimMaxMisses {
			continue
		}
		r.destroyPod(uid, entries, claims, liveByCard)
	}

	// Orphan GC: a marker-less GI is destroyed only on a fully drained card.
	orphansByCard := make(map[string][]migInstance)
	for i := range instances {
		li := instances[i]
		if markedGI[li.Card][li.Inst.GiID] {
			continue // owned by a marker; handled above
		}
		orphansByCard[li.Card] = append(orphansByCard[li.Card], li.Inst)
	}
	for card, orphans := range orphansByCard {
		key := cardMissPrefix + card
		touched.Insert(key)
		if liveOnCard[card] {
			r.misses[key] = 0
			r.inUse[key] = 0
			continue
		}
		r.misses[key]++
		if r.misses[key] < reclaimMaxMisses {
			continue
		}
		r.destroyOrphans(key, card, orphans)
	}

	for k := range r.misses {
		if !touched.Has(k) {
			delete(r.misses, k)
		}
	}
	for k := range r.inUse {
		if !touched.Has(k) {
			delete(r.inUse, k)
		}
	}
}

// destroyPod tears down one dead pod's partitions: for each marker, destroy the GPU instance
// (under that card's lock) and remove only that marker file. Two guards precede the destroy:
//   - attribution self-check — if a running Pod claims the placement, the marker is
//     mis-attributed (a dead pod's marker over a live pod's instance), so it is never destroyed;
//   - identity check — the GI id must still carry the instance the marker recorded (compare the
//     MIG-device UUID against liveByCard); an out-of-band destroy + NVML GI-id reuse can put a
//     different, possibly live, instance at that id, so on a UUID mismatch the stale marker is
//     dropped without any destroy, and a GI already gone needs only its marker removed.
//
// A residual NVML_ERROR_IN_USE is a bounded retryable failure: the pod's miss counter is not
// cleared (retry next pass) and an operator-visible log is surfaced once the retries cross the
// bound. The miss/in-use counters are cleared only when every one of the pod's partitions is
// reclaimed.
func (r *reclaimer) destroyPod(uid string, entries []markerEntry, claims map[string][]migPlacement, liveByCard map[string]map[uint32]migInstance) {
	ok := true
	inUseHit := false
	for i := range entries {
		m := entries[i].marker
		card := m.Card

		if placementOverlapsAny(migPlacement{Start: m.Start, Length: m.Length}, claims[card]) {
			r.logger.Info("reclaim: placement is claimed by a running pod, skipping destroy (attribution conflict)",
				"podUID", uid, "card", card, "giID", m.GiID)
			ok = false
			continue
		}

		if live, present := liveByCard[card][m.GiID]; present && live.UUID != m.MigUUID {
			// The GI id was reused by a different instance; drop the stale marker, never destroy.
			r.logger.Info("reclaim: gpu-instance id reused by a different instance, dropping stale marker without destroy",
				"podUID", uid, "card", card, "giID", m.GiID, "markerUUID", m.MigUUID, "liveUUID", live.UUID)
			if !r.removeMarker(entries[i].path) {
				ok = false
			}
			continue
		} else if present {
			// The marker still describes the live instance: destroy it under the card lock.
			unlock := lockCard(card)
			derr := r.driver.DestroyInstance(card, m.instance())
			unlock()
			if derr != nil {
				ok = false
				if errors.Is(derr, errInstanceInUse) {
					inUseHit = true
					continue
				}
				r.logger.Error(derr, "reclaim: destroy gpu instance", "podUID", uid, "card", card, "giID", m.GiID)
				continue
			}
		}
		// The instance is destroyed or was already gone: remove the marker.
		if !r.removeMarker(entries[i].path) {
			ok = false
		}
	}

	if inUseHit {
		r.inUse[uid]++
		if r.inUse[uid] == reclaimMaxDestroyMisses {
			r.logger.Error(errInstanceInUse,
				"reclaim: a mig instance is still in use after bounded destroy retries; a residual process is holding it, reclamation is blocked until it exits",
				"podUID", uid, "attempts", r.inUse[uid])
		}
	} else {
		r.inUse[uid] = 0
	}

	if ok {
		delete(r.misses, uid)
		delete(r.inUse, uid)
		r.logger.Info("reclaim: reclaimed dead pod's partitions", "podUID", uid, "partitions", len(entries))
	}
}

// removeMarker removes a marker file (and its now-empty container/pod dirs, so a sibling
// container's live marker is never dropped) and reports whether the removal succeeded.
func (r *reclaimer) removeMarker(path string) bool {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		r.logger.Error(err, "reclaim: remove marker", "path", path)
		return false
	}
	removeIfEmpty(filepath.Dir(path))
	removeIfEmpty(filepath.Dir(filepath.Dir(path)))
	return true
}

// destroyOrphans removes the marker-less GPU instances on a fully drained card (no live Pod). It
// re-scans markers under the card lock and bails if the card now carries ANY marker: create+marker
// is atomic under this same lock, so a marker appearing since the lock-free snapshot means an
// allocation arrived and the card is no longer fully drained — its orphans wait for a later pass
// (as MetaX keeps unidentifiable orphans while any pod holds the card). A residual
// NVML_ERROR_IN_USE is a bounded retryable failure with the same condition-at-the-bound surface as
// the per-pod path; the miss counter is cleared only when every removal succeeds.
func (r *reclaimer) destroyOrphans(missKey, card string, orphans []migInstance) {
	unlock := lockCard(card)
	defer unlock()

	entries, _ := scanMarkers(r.podsDir)
	if len(ownedGiIDsOnCard(entries, card)) > 0 {
		return
	}

	ok := true
	inUseHit := false
	destroyed := 0
	for _, inst := range orphans {
		if derr := r.driver.DestroyInstance(card, inst); derr != nil {
			ok = false
			if errors.Is(derr, errInstanceInUse) {
				inUseHit = true
				continue
			}
			r.logger.Error(derr, "reclaim: destroy orphan gpu instance on drained card", "card", card, "giID", inst.GiID)
			continue
		}
		destroyed++
	}

	if inUseHit {
		r.inUse[missKey]++
		if r.inUse[missKey] == reclaimMaxDestroyMisses {
			r.logger.Error(errInstanceInUse,
				"reclaim: a marker-less mig instance on a drained card is still in use after bounded destroy retries; a residual process is holding it",
				"card", card, "attempts", r.inUse[missKey])
		}
	} else {
		r.inUse[missKey] = 0
	}

	if ok {
		delete(r.misses, missKey)
		delete(r.inUse, missKey)
		if destroyed > 0 {
			r.logger.Info("reclaim: reclaimed marker-less orphans on drained card", "card", card, "count", destroyed)
		}
	}
}

// removeIfEmpty removes dir only when it holds no entries, so reclaiming one container never
// orphans a sibling's marker.
func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
}
