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

// reclaimMaxCorruptHoldMisses bounds how many consecutive reconciles an unattributable corrupt path —
// one whose own path names no Pod, so no liveness evidence can ever retire it — may hold the node
// before the loop surfaces an operator-visible log. The hold itself is never released (there is
// nothing to release it on), so unlike the debounces above this bound changes no decision: it exists
// because that hold is node-wide and permanent (no adoption on any card, no orphan GC'd on any card)
// and must not degrade the node silently. It mirrors the NVML_ERROR_IN_USE bound's surface: an
// operator-visible log, once, since Devices.Status is rebuilt wholesale each reconcile and a status
// condition would be stomped.
const reclaimMaxCorruptHoldMisses = 8

// errCorruptPathUnattributable is the fault the bounded hold above reports: a path under the pod work
// root that neither parses as a marker nor names the Pod or card it belonged to. It is carried as an
// error rather than as message text so the operator-visible log reads like the IN_USE one.
var errCorruptPathUnattributable = errors.New("unreadable path names neither a pod nor a card")

// errCorruptMarkerHeldByLivePod is the fault the live-owner hold reports: an unreadable ownership
// record whose Pod is still running, so no liveness evidence can retire it and nothing can rebuild
// it. It is carried as an error rather than as message text for the same reason as the fault above.
var errCorruptMarkerHeldByLivePod = errors.New("unreadable marker of a live pod")

// cardMissPrefix namespaces a per-card orphan-GC miss counter in the same misses map as the
// per-pod counters; pod UIDs are UUIDs, so they never collide with this prefix.
const cardMissPrefix = "card:"

// corruptMissPrefix namespaces a per-corrupt-marker-path miss counter in the same misses map, so
// retiring an unparseable marker is debounced exactly like every other liveness decision here
// (never acted on from one transient pass). Pod UIDs are UUIDs, so they never collide with it.
const corruptMissPrefix = "corrupt:"

// corruptHeldMissPrefix namespaces the same path's held-passes counter, which cannot share the key
// above: that one is reset to zero on every pass whose Pod is alive — the retirement debounce needs
// that reset, so a Pod outliving a transient list gap never has its record retired — and a hold
// counted there would therefore never reach any bound. Pod UIDs are UUIDs, so neither prefix
// collides with them.
const corruptHeldMissPrefix = "corrupt-held:"

// reclaimer is the level-based MIG reclaim loop's state, driven by the reconciler's broadcast
// live pod-UID set plus a periodic resync ticker (deviceplugin.RunReclaimLoop). A sliced
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
	misses     map[string]int // pod UID / "card:<uuid>" / "corrupt:<path>" -> consecutive absent-or-idle reconciles
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
//
// An unparseable marker is not merely logged: its GPU instance is absent from the parsed
// ownership set, so it would look exactly like a collectable orphan while a running Pod still
// holds it. Its card is therefore held back from the drained verdict (fail closed per card, by the
// card the corrupt file's name encodes), and the corrupt file itself is retired once its Pod — read
// from its path — is gone, which is what lets the card converge instead of leaking a partition for
// the node's lifetime.
func (r *reclaimer) reconcile(livePodUIDs []string) {
	live := sets.New[string](livePodUIDs...)

	markers, corrupt := scanMarkers(r.podsDir)

	// The attribution self-check needs the live claim set; without it fail closed (skip the
	// whole pass) rather than risk destroying an instance a running Pod holds.
	claims, cerr := r.liveClaims()
	if cerr != nil {
		r.logger.Error(cerr, "reclaim: read live pod claims, skipping this pass")
		return
	}

	// The live instance list backs the orphan sweep below; without it fail closed (skip the pass)
	// rather than act on an unvalidated view. The marker identity check does NOT read this snapshot:
	// by the time a given card is reached it may be a whole allocation old, so that check re-reads the
	// card inside its own lock instead.
	instances, lerr := r.driver.ListInstances()
	if lerr != nil {
		r.logger.Error(lerr, "reclaim: list mig instances, skipping this pass")
		return
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
		r.destroyPod(uid, entries, claims)
	}

	// Retire the corrupt markers whose Pod is gone. It runs after the per-pod decisions (a dead
	// Pod's parseable markers are reclaimed by the same evidence) and before the orphan GC, but the
	// corrupt list of THIS pass still holds its cards back below: the retirement is only observed
	// by the next pass's scan, which is deliberate — the card is released once the file is provably
	// gone, never on the strength of having just tried to remove it.
	r.pruneCorruptMarkers(corrupt, live, touched)

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
		// A card a corrupt marker names is never treated as drained: one of these "orphans" may be
		// the instance that unreadable record owns, and destroying it would rip the partition out of
		// a running container whose only ownership record was truncated. The debounce is reset, so
		// the card starts the count from scratch once its ownership is readable again.
		if liveOnCard[card] || ownershipUnknownOnCard(corrupt, card) {
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
//   - identity check — the GI id must still carry the instance the marker recorded, compared against
//     a live set re-read INSIDE that card's lock; an out-of-band destroy + NVML GI-id reuse can put a
//     different, possibly live, instance at that id, so on a mismatch the stale marker is dropped
//     without any destroy, and a GI already gone needs only its marker removed. A re-read that fails
//     is a per-card skip, never a destroy on an unvalidated view.
//
// A residual NVML_ERROR_IN_USE is a bounded retryable failure: the pod's miss counter is not
// cleared (retry next pass) and an operator-visible log is surfaced once the retries cross the
// bound. The miss/in-use counters are cleared only when every one of the pod's partitions is
// reclaimed.
func (r *reclaimer) destroyPod(uid string, entries []markerEntry, claims map[string][]migPlacement) {
	ok := true
	inUseHit := false

	// Group by card first: the attribution self-check needs no NVML call and no lock, so it filters
	// here, and what survives it is destroyed one card at a time under that card's own lock.
	byCard := make(map[string][]markerEntry)
	for i := range entries {
		m := entries[i].marker
		if placementOverlapsAny(migPlacement{Start: m.Start, Length: m.Length}, claims[m.Card]) {
			r.logger.Info("reclaim: placement is claimed by a running pod, skipping destroy (attribution conflict)",
				"podUID", uid, "card", m.Card, "giID", m.GiID)
			ok = false
			continue
		}
		byCard[m.Card] = append(byCard[m.Card], entries[i])
	}

	for card, cardEntries := range byCard {
		done, busy := r.destroyMarkedInstancesOnCard(uid, card, cardEntries)
		if !done {
			ok = false
		}
		if busy {
			inUseHit = true
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

// destroyMarkedInstancesOnCard destroys every one of a dead pod's partitions on ONE card, under that
// card's lock, re-verifying inside the critical section that each recorded GPU-instance id still
// carries the recorded identity and placement. It reports whether every marker on the card is now
// gone, and whether any destroy was rejected as in-use. A reused id is retained, not destroyed: only
// the stale marker is dropped, because the instance now at that id belongs to somebody else.
//
// Re-reading inside the lock is what makes the identity check mean anything. The snapshot the pass
// opened with can age by a whole allocation before this card is reached, and an out-of-band
// `nvidia-smi mig -dgi` plus NVML's id reuse can put a different — possibly live — instance at the
// recorded id in exactly that window; a check against the stale snapshot would then match the marker
// against an instance that no longer exists and destroy a running Pod's MIG device instead.
//
// The live set is re-read ONCE for the whole card rather than once per marker: several containers of
// one Pod on one card are the common multi-marker case, and the enumeration is node-wide and
// expensive. Reading it once is not weaker, because the lock is held across the whole group, so
// nothing outside this loop can change the card between the markers — only this loop's own destroys
// do, and each marker is verified against the identity it recorded rather than against the residue of
// a sibling's destroy.
func (r *reclaimer) destroyMarkedInstancesOnCard(uid, card string, entries []markerEntry) (done, inUseHit bool) {
	unlock := lockCard(card)
	defer unlock()

	instances, lerr := r.driver.CardInstances(card)
	if lerr != nil {
		r.logger.Error(lerr, "reclaim: re-read this card's mig instances before destroy, skipping it",
			"podUID", uid, "card", card, "partitions", len(entries))
		return false, false
	}

	done = true
	for i := range entries {
		m := entries[i].marker
		inst, present := findLiveGi(instances, m.GiID)
		switch {
		case present && !inst.matchesMarker(m):
			r.logger.Info("reclaim: gpu-instance id reused by a different instance, dropping stale marker without destroy",
				"podUID", uid, "card", card, "giID", m.GiID,
				"markerUUID", m.MigUUID, "liveUUID", inst.UUID,
				"markerPlacement", migPlacement{Start: m.Start, Length: m.Length}, "livePlacement", inst.Placement)
		case present:
			if derr := r.driver.DestroyInstance(card, m.instance()); derr != nil {
				done = false
				if errors.Is(derr, errInstanceInUse) {
					inUseHit = true
					continue
				}
				r.logger.Error(derr, "reclaim: destroy gpu instance", "podUID", uid, "card", card, "giID", m.GiID)
				continue
			}
		}
		// The instance is destroyed, was already gone, or belongs to somebody else: drop the marker.
		if !r.removeMarker(entries[i].path) {
			done = false
		}
	}
	return done, inUseHit
}

// findLiveGi returns the GPU instance with giID from one card's enumeration, if it holds one.
func findLiveGi(instances []migInstance, giID uint32) (migInstance, bool) {
	for i := range instances {
		if instances[i].GiID == giID {
			return instances[i], true
		}
	}
	return migInstance{}, false
}

// matchesMarker reports whether a live instance is still the one a marker recorded. The identity
// string alone would do against a self-consistent driver; the placement sits beside it as an
// inconsistency trap, since an instance matching one while contradicting the other is exactly the
// unprovable state a destroy must refuse rather than resolve.
func (in migInstance) matchesMarker(m migMarker) bool {
	return in.UUID == m.MigUUID &&
		in.Placement.Start == m.Start &&
		in.Placement.Length == m.Length
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

// pruneCorruptMarkers retires the unparseable markers whose Pod is gone, so a truncated record
// holds its card back only while it can still stand for something. A corrupt file's contents say
// nothing, but its path names its Pod, and that is evidence enough on its own: once the Pod is
// absent from the live set for reclaimMaxMisses consecutive passes — the same debounce every other
// liveness decision in this loop uses, so a transient list gap never retires a live Pod's record —
// the file is removed. The partition it shadowed then becomes a genuine marker-less orphan the
// collector takes once its card drains, which is how the card converges instead of leaking a
// partition for the node's lifetime. No card lock is needed: the Pod is dead, so no concurrent
// Allocate is writing that path.
//
// Two cases are kept indefinitely, deliberately:
//   - a corrupt marker whose Pod is alive — it still records an ownership its Pod depends on;
//   - a corrupt path whose Pod cannot be read from it (a walk error, not a marker file at marker
//     depth) — with no owner there is no liveness evidence to act on, and it is a filesystem fault
//     to be repaired, not an ownership record to retire.
//
// The second case is permanent, and its cost is node-wide rather than per card: such a path names no
// card either, so ownershipUnknownOnCard darkens every card — no adoption anywhere and no orphan
// GC'd anywhere — for as long as it stays unreadable. Nothing here can fix that without destroying
// state it cannot account for, so the hold stands; what it must not do is stand silently.
//
// Neither kept case stands silently: each counts its consecutive passes under its own key and
// surfaces one operator-visible log at reclaimMaxCorruptHoldMisses naming the path and the repair,
// so the repair can be made by the one actor able to make it. The first case needs that as much as
// the second, because it is the one that only looks transient — the Pod is alive, so the hold reads
// as something its exit will clear, and it is not: see holdLiveOwnersMarker.
func (r *reclaimer) pruneCorruptMarkers(corrupt []string, live, touched sets.Set[string]) {
	for _, path := range corrupt {
		uid, ok := podUIDFromMarkerPath(r.podsDir, path)
		if !ok {
			r.holdUnattributablePath(path, touched)
			continue
		}
		key := corruptMissPrefix + path
		touched.Insert(key)
		if live.Has(uid) {
			r.misses[key] = 0
			r.holdLiveOwnersMarker(path, uid, touched)
			continue
		}
		r.misses[key]++
		if r.misses[key] < reclaimMaxMisses {
			continue
		}
		if r.removeMarker(path) {
			delete(r.misses, key)
			r.logger.Info("reclaim: removed unparseable marker of a dead pod", "path", path, "podUID", uid)
		}
	}
}

// holdLiveOwnersMarker keeps the unparseable marker of a live Pod — it still records an ownership its
// Pod depends on — and counts the consecutive passes that hold has cost the card, surfacing one
// operator-visible log at reclaimMaxCorruptHoldMisses. Like the unattributable hold it changes no
// decision: the record is kept either way. The count lives in its own key space, because the
// retirement debounce's key for this same path is reset to zero on every one of these passes. The key
// is touched so the count survives to the next pass, and disappears with it once the path does.
//
// The bound is worth spending on this case precisely because it reads as the transient one: the Pod
// is alive, so the hold looks like something the Pod's exit will clear. It is not. While it stands,
// no leftover on the card can be adopted and none can be reclaimed; and the Pod cannot be re-admitted
// either, because a re-created container's reserve reads its own record first and fails closed on any
// parse failure that is not "absent" — so restarting the Pod cannot clear it, and nothing the
// operator still holds can rebuild the record, since the ids that identify the partition were only
// ever in the file. Deleting the Pod is what releases the card: the retirement debounce then removes
// the record on the evidence of its path.
func (r *reclaimer) holdLiveOwnersMarker(path, uid string, touched sets.Set[string]) {
	key := corruptHeldMissPrefix + path
	touched.Insert(key)
	r.misses[key]++
	r.logger.Info("reclaim: unparseable marker of a live pod, holding its card closed",
		"path", path, "podUID", uid, "passes", r.misses[key])
	if r.misses[key] != reclaimMaxCorruptHoldMisses {
		return
	}
	card, ok := cardFromMarkerPath(path)
	if !ok {
		card = "ALL (the file name encodes no card)"
	}
	r.logger.Error(errCorruptMarkerHeldByLivePod,
		"reclaim: an unreadable MIG ownership record of a RUNNING pod is holding its card closed: while it "+
			"persists no leftover partition on that card is adopted and none is reclaimed, and the pod cannot be "+
			"re-admitted either, because a container's reserve reads its own record first and fails closed on an "+
			"unreadable one — so restarting the pod cannot clear this, and the record cannot be rebuilt from "+
			"anything outside it; delete the pod to release the card",
		"path", path, "podUID", uid, "card", card, "passes", r.misses[key])
}

// holdUnattributablePath keeps a corrupt path whose owner cannot be read from it — there is no
// liveness evidence any decision here could rest on — and counts the consecutive passes it has held
// the node, surfacing one operator-visible log at reclaimMaxCorruptHoldMisses. The count shares the
// misses map under the same corrupt-path key space as the retirement debounce; a given path is
// attributable or not for its whole life, so the two meanings never mix on one key. The path is
// touched so the count survives to the next pass, and disappears with the count once the path does.
func (r *reclaimer) holdUnattributablePath(path string, touched sets.Set[string]) {
	key := corruptMissPrefix + path
	touched.Insert(key)
	r.misses[key]++
	r.logger.Info("reclaim: unparseable marker path names no pod, holding its cards closed",
		"path", path, "passes", r.misses[key])
	if r.misses[key] == reclaimMaxCorruptHoldMisses {
		r.logger.Error(errCorruptPathUnattributable,
			"reclaim: an unreadable path under the pod work root names neither a pod nor a card, so MIG ownership "+
				"cannot be proven on ANY card of this node: no leftover partition is adopted and none is reclaimed "+
				"while it persists; this is a filesystem fault that will not clear by itself — repair or remove the "+
				"path",
			"path", path, "passes", r.misses[key])
	}
}

// destroyOrphans removes the marker-less GPU instances on a fully drained card (no live Pod). It
// re-scans markers under the card lock and bails if the card now carries ANY marker: create+marker
// is atomic under this same lock, so a marker appearing since the lock-free snapshot means an
// allocation arrived and the card is no longer fully drained — its orphans wait for a later pass
// (as MetaX keeps unidentifiable orphans while any pod holds the card). An UNPARSEABLE marker
// naming the card bails the same way: it is an ownership record, and the fact that its contents
// cannot be read is exactly why the card is not provably drained — honoring only the parseable
// ones here would destroy the partition that record owns. A residual
// NVML_ERROR_IN_USE is a bounded retryable failure with the same condition-at-the-bound surface as
// the per-pod path; the miss counter is cleared only when every removal succeeds.
func (r *reclaimer) destroyOrphans(missKey, card string, orphans []migInstance) {
	unlock := lockCard(card)
	defer unlock()

	entries, corrupt := scanMarkers(r.podsDir)
	if len(ownedGiIDsOnCard(entries, card)) > 0 || ownershipUnknownOnCard(corrupt, card) {
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
