package thead

import (
	"errors"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
)

// reclaimMaxMisses debounces a liveness decision: a pod (or a drained card's orphans) must be
// absent/idle for this many consecutive reconciles before its partition is destroyed, so a transient
// list gap never reclaims live state. Against the resync interval it is also the
// create-before-marker guard: a crash-then-retry Allocate rebinds its GPU instance well within
// reclaimMaxMisses × resync, so the orphan collector never destroys a partition an in-flight retry
// still owns.
const reclaimMaxMisses = 3

// reclaimMaxDestroyMisses bounds how many consecutive reconciles a destroy may fail as busy before
// the loop surfaces an operator-visible log — a residual process is holding the instance. The
// debounce is never cleared meanwhile, so the destroy keeps retrying every pass; sibling cards are
// never blocked (per-card locks). Devices.Status is rebuilt wholesale each reconcile from Spec + Pod
// annotations, so a status condition would be stomped; the log is the operator-visible surface.
const reclaimMaxDestroyMisses = 8

// reclaimMaxCorruptHoldMisses bounds how many consecutive reconciles an unattributable corrupt path —
// one whose own path names no Pod, so no liveness evidence can ever retire it — may hold the node
// before the loop surfaces an operator-visible log. The hold itself is never released (there is
// nothing to release it on), so unlike the debounces above this bound changes no decision: it exists
// because that hold is node-wide and permanent (no adoption on any card, no orphan collected on any
// card) and must not degrade the node silently. It mirrors the busy-destroy bound's surface: an
// operator-visible log, once, since Devices.Status is rebuilt wholesale each reconcile and a status
// condition would be stomped.
const reclaimMaxCorruptHoldMisses = 8

// errCorruptPathUnattributable is the fault the bounded hold above reports: a path under the pod work
// root that neither parses as a marker nor names the Pod or card it belonged to. It is carried as an
// error rather than as message text so the operator-visible log reads like the busy-destroy one.
var errCorruptPathUnattributable = errors.New("unreadable path names neither a pod nor a card")

// cardMissPrefix namespaces a per-card orphan-collection miss counter in the same misses map as the
// per-pod counters; pod UIDs are UUIDs, so they never collide with this prefix.
const cardMissPrefix = "card:"

// corruptMissPrefix namespaces a per-corrupt-marker-path miss counter in the same misses map, so
// retiring an unparseable marker is debounced exactly like every other liveness decision here (never
// acted on from one transient pass). Pod UIDs are UUIDs, so they never collide with it.
const corruptMissPrefix = "corrupt:"

// reclaimer is the level-based partition reclaim loop's state, driven by the reconciler's live
// pod-UID set plus a periodic resync. A physically sliced pool has no Release callback, so a Pod's
// GPU/compute instances are freed here. Each reconcile re-scans the markers and re-lists the driver,
// so it self-heals across restarts with no in-memory instance registry. It runs single-threaded (the
// caller invokes reconcile serially), so its counter maps need no lock; only the per-card lock
// coordinates with concurrent allocations.
type reclaimer struct {
	driver  migDriver
	podsDir string
	logger  klog.Logger
	// liveClaims returns, per card UUID, the memory-slice placements live (non-terminating) Pods
	// currently claim by annotation — the attribution self-check source, so a mis-attributed marker
	// (the allocating-Pod heuristic can bind an allocation to the wrong same-profile Pod) never
	// destroys an instance a running Pod holds. It is injected so the loop is table-tested without a
	// Kubernetes client.
	liveClaims func() (map[string][]migPlacement, error)
	misses     map[string]int // pod UID / "card:<uuid>" / "corrupt:<path>" -> consecutive absent-or-idle reconciles
	inUse      map[string]int // pod UID / "card:<uuid>" -> consecutive busy-failed destroy reconciles
}

// newReclaimer builds the reclaim loop over a driver seam, a pods root and a live-claims source. The
// dependencies are parameters rather than package state so the loop is driven from a table test.
func newReclaimer(
	driver migDriver, podsDir string, logger klog.Logger, liveClaims func() (map[string][]migPlacement, error),
) *reclaimer {
	return &reclaimer{
		driver: driver, podsDir: podsDir, logger: logger, liveClaims: liveClaims,
		misses: make(map[string]int), inUse: make(map[string]int),
	}
}

// reconcile reconciles the partitions against the on-disk markers for one live pod-UID snapshot.
// Every liveness decision is debounced by reclaimMaxMisses consecutive absent reconciles, so a
// transient list gap never reclaims live state; each destroy runs under its card's lock (never a
// node-wide mutex) so it never races an in-flight same-card allocation's create+marker window while
// sibling cards proceed in parallel. It reconciles in two directions:
//   - a marker whose pod is dead -> destroy its partition (compute instance then GPU instance),
//     unless a running Pod still claims that placement (attribution self-check); a busy destroy is a
//     bounded, retryable partial failure (the debounce is not cleared) surfacing a log at the bound;
//   - a marker-less GPU instance (a crash between the create and the marker write, or an out-of-band
//     one) is destroyed only once its card is fully drained (no live Pod claims or marks it) — a GPU
//     instance carries no operator tag, so per-pod attribution of a marker-less one is impossible.
//
// An unparseable marker is not merely logged: its GPU instance is absent from the parsed ownership
// set, so it would look exactly like a collectable orphan while a running Pod still holds it. Its
// card is therefore held back from the drained verdict (fail closed per card, through the shared
// ownershipUnknownOnCard predicate the allocation path consults too), and the corrupt file itself is
// retired once its Pod — read from its path — is gone, which is what lets the card converge instead
// of leaking a partition for the node's lifetime.
//
// Both reads fail closed: neither the marker scan's driver view nor the live-claim set may be
// substituted by an empty one, so a failure of either skips the whole pass rather than acting on a
// view it cannot trust.
func (r *reclaimer) reconcile(livePodUIDs []string) {
	live := sets.New[string](livePodUIDs...)

	markers, corrupt := scanMarkers(r.podsDir)

	// The attribution self-check needs the live claim set; without it fail closed (skip the whole
	// pass) rather than risk destroying an instance a running Pod holds.
	claims, cerr := r.liveClaims()
	if cerr != nil {
		r.logger.Error(cerr, "reclaim: read live pod claims, skipping this pass")
		return
	}

	// The live instance list backs the orphan sweep; without it fail closed (skip the pass) rather
	// than act on an unvalidated view — a partition read as absent would have its marker removed as
	// "already gone" and a marker-less one would leak past the collector.
	instances, lerr := r.driver.ListInstances()
	if lerr != nil {
		r.logger.Error(lerr, "reclaim: list partitions, skipping this pass")
		return
	}

	// touched marks every miss key still relevant this pass; the rest are pruned at the end.
	touched := sets.New[string]()

	// A card is "live" while any live Pod marks or claims it, so its marker-less orphans are kept
	// (one could be a live Pod's create-before-marker instance). markedGI indexes every GPU instance
	// a marker owns so orphan detection finds the marker-less ones.
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

	// Retire the corrupt markers whose Pod is gone. It runs after the per-pod decisions (a dead Pod's
	// parseable markers are reclaimed by the same evidence) and before the orphan collection, but the
	// corrupt list of THIS pass still holds its cards back below: the retirement is only observed by
	// the next pass's scan, which is deliberate — the card is released once the file is provably gone,
	// never on the strength of having just tried to remove it.
	r.pruneCorruptMarkers(corrupt, live, touched)

	// Orphan collection: a marker-less GPU instance is destroyed only on a fully drained card.
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
		// A card whose ownership set an unparseable marker leaves unknowable is never treated as
		// drained: one of these "orphans" may be the instance that unreadable record owns, and
		// destroying it would rip the partition out of a running container whose only ownership record
		// was truncated. The debounce is reset, so the card starts the count from scratch once its
		// ownership is readable again.
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

// destroyPod tears down one dead pod's partitions: the markers are grouped by card and each card is
// taken once — one lock hold and one driver re-read per card, however many of the pod's containers
// carved a partition on it — and only that pod's own marker files are removed. Two guards precede the
// destroy:
//   - attribution self-check — if a running Pod claims the placement, the marker is mis-attributed (a
//     dead pod's marker over a live pod's instance), so it is never destroyed;
//   - identity check — the GPU-instance id must still carry the instance the marker recorded, by BOTH
//     the raw vendor profile id and the partition identity string. The check re-reads the driver's
//     live set INSIDE the card lock rather than trusting the pass's lock-free snapshot: the snapshot
//     can age by a whole allocation, and an out-of-band destroy plus id reuse can put a different,
//     possibly live, instance at that id in exactly that window. On a mismatch the stale marker is
//     dropped without any destroy, and an instance already gone needs only its marker removed. A
//     re-read that fails is a per-card skip, never a destroy on an unvalidated view.
//
// A residual busy rejection is a bounded retryable failure: the pod's miss counter is not cleared
// (retry next pass) and an operator-visible log is surfaced once the retries cross the bound. The
// miss/in-use counters are cleared only when every one of the pod's partitions is reclaimed.
func (r *reclaimer) destroyPod(uid string, entries []markerEntry, claims map[string][]migPlacement) {
	ok := true
	inUseHit := false

	// Group by card first: the attribution self-check needs no driver call and no lock, so it filters
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
				"reclaim: a partition is still in use after bounded destroy retries; a residual process is holding it, "+
					"reclamation is blocked until it exits",
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
// carries the recorded raw profile id and identity string. It reports whether every marker on the card
// is now gone, and whether any destroy was rejected as busy. A reused id is retained, not destroyed:
// only the stale marker is dropped, because the instance now at that id belongs to somebody else.
//
// The driver's live set is re-read ONCE for the whole card rather than once per marker, which is the
// shape that matters: several containers of one Pod on one card are the common multi-marker case, and
// the enumeration is node-wide and expensive. Reading it once is not weaker: the lock is held across
// the whole group, so nothing outside this loop can change the card between the markers — only this
// loop's own destroys do, and each marker is verified against the identity it recorded, not against
// the residue of a sibling's destroy.
func (r *reclaimer) destroyMarkedInstancesOnCard(uid, card string, entries []markerEntry) (done, inUseHit bool) {
	unlock := lockCard(card)
	defer unlock()

	instances, lerr := r.driver.ListInstances()
	if lerr != nil {
		r.logger.Error(lerr, "reclaim: re-list partitions before destroy, skipping this card",
			"podUID", uid, "card", card, "partitions", len(entries))
		return false, false
	}

	done = true
	for i := range entries {
		m := entries[i].marker
		inst, present := findLiveGiOnCard(instances, card, m.GiID)
		switch {
		case present && (inst.UUID != m.MigUUID || inst.ProfileID != m.ProfileID):
			r.logger.Info("reclaim: gpu-instance id reused by a different partition, dropping stale marker without destroy",
				"podUID", uid, "card", card, "giID", m.GiID,
				"markerUUID", m.MigUUID, "liveUUID", inst.UUID,
				"markerProfileID", m.ProfileID, "liveProfileID", inst.ProfileID)
		case present:
			if derr := r.driver.DestroyInstance(card, m.instance()); derr != nil {
				done = false
				if errors.Is(derr, errInstanceInUse) {
					inUseHit = true
					continue
				}
				r.logger.Error(derr, "reclaim: destroy partition", "podUID", uid, "card", card, "giID", m.GiID)
				continue
			}
		}
		// The partition is destroyed, was already gone, or belongs to somebody else: drop the marker.
		if !r.removeMarker(entries[i].path) {
			done = false
		}
	}
	return done, inUseHit
}

// findLiveGiOnCard returns the live GPU instance with giID on the card, if the enumeration holds one.
func findLiveGiOnCard(instances []migLiveInstance, cardUUID string, giID uint32) (migInstance, bool) {
	for i := range instances {
		if instances[i].Card == cardUUID && instances[i].Inst.GiID == giID {
			return instances[i].Inst, true
		}
	}
	return migInstance{}, false
}

// removeMarker removes a marker file (and its now-empty container/pod dirs, so a sibling container's
// live marker is never dropped) and reports whether the removal succeeded.
func (r *reclaimer) removeMarker(path string) bool {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		r.logger.Error(err, "reclaim: remove marker", "path", path)
		return false
	}
	removeIfEmpty(filepath.Dir(path))
	removeIfEmpty(filepath.Dir(filepath.Dir(path)))
	return true
}

// pruneCorruptMarkers retires the unparseable markers whose Pod is gone, so a truncated record holds
// its card back only while it can still stand for something. A corrupt file's contents say nothing,
// but its path names its Pod, and that is evidence enough on its own: once the Pod is absent from the
// live set for reclaimMaxMisses consecutive passes — the same debounce every other liveness decision
// in this loop uses, so a transient list gap never retires a live Pod's record — the file is removed.
// The partition it shadowed then becomes a genuine marker-less orphan the collector takes once its
// card drains, which is how the card converges instead of leaking a partition for the node's
// lifetime. No card lock is needed: the Pod is dead, so no concurrent allocation is writing that path.
//
// Two cases are kept indefinitely, deliberately:
//   - a corrupt marker whose Pod is alive — it still records an ownership its Pod depends on;
//   - a corrupt path whose Pod cannot be read from it (a walk error, or not a marker file at marker
//     depth) — with no owner there is no liveness evidence to act on, and it is a filesystem fault to
//     be repaired, not an ownership record to retire.
//
// The second case is permanent, and its cost is node-wide rather than per card: such a path names no
// card either, so ownershipUnknownOnCard darkens every card — no adoption anywhere and no orphan
// collected anywhere — for as long as it stays unreadable. Nothing here can fix that without
// destroying state it cannot account for, so the hold stands; what it must not do is stand silently.
// The bounded count below surfaces one operator-visible log naming the path, at
// reclaimMaxCorruptHoldMisses consecutive passes, so the repair can be made by the one actor able to
// make it.
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
			r.logger.Info("reclaim: unparseable marker of a live pod, holding its card closed", "path", path, "podUID", uid)
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
			"reclaim: an unreadable path under the pod work root names neither a pod nor a card, so partition "+
				"ownership cannot be proven on ANY card of this node: no leftover partition is adopted and none is "+
				"reclaimed while it persists; this is a filesystem fault that will not clear by itself — repair or "+
				"remove the path",
			"path", path, "passes", r.misses[key])
	}
}

// destroyOrphans removes the marker-less GPU instances on a fully drained card (no live Pod). It
// re-scans the markers under the card lock and bails if the card now carries ANY marker: create+marker
// is atomic under this same lock, so a marker appearing since the lock-free snapshot means an
// allocation arrived and the card is no longer fully drained — its orphans wait for a later pass. An
// UNPARSEABLE marker leaving the card's ownership unknowable bails the same way, through the same
// shared predicate the pass itself consults: the two checks are not redundant, because the pass's
// verdict rests on a snapshot the lock does not cover, and this one is what actually spares the
// partition that record owns. A residual busy rejection is a bounded retryable failure with the same
// log-at-the-bound surface as the per-pod path; the miss counter is cleared only when every removal
// succeeds.
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
			r.logger.Error(derr, "reclaim: destroy marker-less partition on drained card", "card", card, "giID", inst.GiID)
			continue
		}
		destroyed++
	}

	if inUseHit {
		r.inUse[missKey]++
		if r.inUse[missKey] == reclaimMaxDestroyMisses {
			r.logger.Error(errInstanceInUse,
				"reclaim: a marker-less partition on a drained card is still in use after bounded destroy retries; "+
					"a residual process is holding it",
				"card", card, "attempts", r.inUse[missKey])
		}
	} else {
		r.inUse[missKey] = 0
	}

	if ok {
		delete(r.misses, missKey)
		delete(r.inUse, missKey)
		if destroyed > 0 {
			r.logger.Info("reclaim: reclaimed marker-less partitions on drained card", "card", card, "count", destroyed)
		}
	}
}

// removeIfEmpty removes dir only when it holds no entries, so reclaiming one container never orphans
// a sibling's marker.
func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
}
