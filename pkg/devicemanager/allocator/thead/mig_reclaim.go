package thead

import (
	"errors"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
)

// reclaimMaxMisses debounces a liveness decision: a pod (or a drained accelerator's orphans) must
// be absent/idle for this many consecutive reconciles before its partition is destroyed, so a
// transient list gap never reclaims live state. Against the resync interval it is also the
// create-before-marker guard: a crash-then-retry Allocate rebinds its GPU instance well within
// reclaimMaxMisses × resync, so the orphan collector never destroys a partition an in-flight retry
// still owns.
const reclaimMaxMisses = 3

// reclaimMaxDestroyMisses bounds how many consecutive reconciles anything may keep a partition from
// being reclaimed before the loop surfaces an operator-visible log. Three ways reach it: a destroy
// refused as busy, a destroy never attempted because the pre-check saw the process first, and one
// never attempted because the partition could not be asked at all (destroyBlockedBy, whose answer the
// log names, since the three call for different things).
// The debounce is never cleared meanwhile, so the decision is retaken every pass;
// sibling accelerators are never blocked (per-accelerator locks). Devices.Status is rebuilt wholesale each
// reconcile from Spec + Pod annotations, so a status condition would be stomped; the log is the
// operator-visible surface.
const reclaimMaxDestroyMisses = 8

// reclaimMaxCorruptHoldMisses bounds how many consecutive reconciles an unattributable corrupt path
// — one whose own path names no Pod, so no liveness evidence can ever retire it — may hold the node
// before the loop surfaces an operator-visible log. The hold itself is never released (there is
// nothing to release it on), so unlike the debounces above this bound changes no decision: it
// exists because that hold is node-wide and permanent (no adoption on any accelerator, no orphan
// collected on any accelerator) and must not degrade the node silently. It mirrors the busy-destroy
// bound's surface: an operator-visible log, once, since Devices.Status is rebuilt wholesale each
// reconcile and a status condition would be stomped.
const reclaimMaxCorruptHoldMisses = 8

// errCorruptPathUnattributable is the fault the bounded hold above reports: a path under the pod
// work root that neither parses as a marker nor names the Pod or accelerator it belonged to. It is
// carried as an error rather than as message text so the operator-visible log reads like the
// busy-destroy one.
var errCorruptPathUnattributable = errors.New("unreadable path names neither a pod nor a card")

// errCorruptMarkerHeldByLivePod is the fault the live-owner hold reports: an unreadable ownership
// record whose Pod is still running, so no liveness evidence can retire it and nothing can rebuild
// it. It is carried as an error rather than as message text for the same reason as the fault above.
var errCorruptMarkerHeldByLivePod = errors.New("unreadable marker of a live pod")

// cardMissPrefix namespaces a per-accelerator orphan-collection miss counter in the same misses map
// as the per-pod counters; pod UIDs are UUIDs, so they never collide with this prefix. The prefix
// string is a key namespace inside these maps, not vocabulary — the counters it keys are
// per-accelerator.
const cardMissPrefix = "card:"

// corruptMissPrefix namespaces a per-corrupt-marker-path miss counter in the same misses map, so
// retiring an unparseable marker is debounced exactly like every other liveness decision here (never
// acted on from one transient pass). Pod UIDs are UUIDs, so they never collide with it.
const corruptMissPrefix = "corrupt:"

// corruptHeldMissPrefix namespaces the same path's held-passes counter, which cannot share the key
// above: that one is reset to zero on every pass whose Pod is alive — the retirement debounce needs
// that reset, so a Pod outliving a transient list gap never has its record retired — and a hold
// counted there would therefore never reach any bound. Pod UIDs are UUIDs, so neither prefix collides
// with them.
const corruptHeldMissPrefix = "corrupt-held:"

// reclaimer is the level-based partition reclaim loop's state, driven by the reconciler's live
// pod-UID set plus a periodic resync. A physically sliced pool has no Release callback, so a Pod's
// GPU/compute instances are freed here. Each reconcile re-scans the markers and re-lists the
// driver, so it self-heals across restarts with no in-memory instance registry. It runs
// single-threaded (the caller invokes reconcile serially), so its counter maps need no lock; only
// the per-accelerator lock coordinates with concurrent allocations.
type reclaimer struct {
	driver  migDriver
	podsDir string
	logger  klog.Logger
	// liveClaims returns, per accelerator UUID, the memory-slice placements live (non-terminating)
	// Pods currently claim by annotation — the attribution self-check source, so a mis-attributed
	// marker (the allocating-Pod heuristic can bind an allocation to the wrong same-profile Pod) never
	// destroys an instance a running Pod holds. It is injected so the loop is table-tested without a
	// Kubernetes client.
	liveClaims func() (map[string][]migPlacement, error)
	misses     map[string]int // pod UID / "card:<uuid>" / "corrupt:<path>" -> consecutive absent-or-idle reconciles
	inUse      map[string]int // pod UID / "card:<uuid>" -> consecutive reconciles something blocked its reclaim
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
// transient list gap never reclaims live state; each destroy runs under its accelerator's lock
// (never a node-wide mutex) so it never races an in-flight same-accelerator allocation's
// create+marker window while sibling accelerators proceed in parallel. It reconciles in two
// directions:
//   - a marker whose pod is dead -> destroy its partition (compute instance then GPU instance),
//     unless a running Pod still claims that placement (attribution self-check); a busy destroy is
//     a bounded, retryable partial failure (the debounce is not cleared) surfacing a log at the
//     bound;
//   - a marker-less GPU instance (a crash between the create and the marker write, or an
//     out-of-band one) is destroyed only once its accelerator is fully drained (no live Pod claims
//     or marks it) — a GPU instance carries no operator tag, so per-pod attribution of a
//     marker-less one is impossible.
//
// Both directions ask one question before any destroy: whether a process is running on the partition
// itself. One that carries a process is never destroyed, however dead its recorded owner looks — see
// destroyBlockedBy.
//
// An unparseable marker is not merely logged: its GPU instance is absent from the parsed ownership
// set, so it would look exactly like a collectable orphan while a running Pod still holds it. Its
// accelerator is therefore held back from the drained verdict (fail closed per accelerator, through
// the shared ownershipUnknownOnCard predicate the allocation path consults too), and the corrupt
// file itself is retired once its Pod — read from its path — is gone, which is what lets the
// accelerator converge instead of leaking a partition for the node's lifetime.
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

	// An accelerator is "live" while any live Pod marks or claims it, so its marker-less orphans are
	// kept (one could be a live Pod's create-before-marker instance). markedGI indexes every GPU
	// instance a marker owns so orphan detection finds the marker-less ones.
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
	// corrupt list of THIS pass still holds its accelerators back below: the retirement is only
	// observed by the next pass's scan, which is deliberate — the accelerator is released once the
	// file is provably gone, never on the strength of having just tried to remove it.
	r.pruneCorruptMarkers(corrupt, live, touched)

	// Orphan collection: a marker-less GPU instance is destroyed only on a fully drained accelerator.
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
		// An accelerator whose ownership set an unparseable marker leaves unknowable is never treated as
		// drained: one of these "orphans" may be the instance that unreadable record owns, and destroying
		// it would rip the partition out of a running container whose only ownership record was
		// truncated. The debounce is reset, so the accelerator starts the count from scratch once its
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

// destroyPod tears down one dead pod's partitions: the markers are grouped by accelerator and each
// accelerator is taken once — one lock hold and one driver re-read per accelerator, however many of
// the pod's containers carved a partition on it — and only that pod's own marker files are removed.
// Three guards precede the destroy:
//   - attribution self-check — if a running Pod claims the placement, the marker is mis-attributed
//     (a dead pod's marker over a live pod's instance), so it is never destroyed;
//   - process check — a partition running a process is left alone, marker and all, so a later pass
//     finds it again;
//   - identity check — the GPU-instance id must still carry the instance the marker recorded, by
//     BOTH the raw vendor profile id and the partition identity string. The check re-reads the
//     driver's live set INSIDE the accelerator lock rather than trusting the pass's lock-free
//     snapshot: the snapshot can age by a whole allocation, and an out-of-band destroy plus id
//     reuse can put a different, possibly live, instance at that id in exactly that window. On a
//     mismatch the stale marker is dropped without any destroy, and an instance already gone needs
//     only its marker removed. A re-read that fails is a per-accelerator skip, never a destroy on
//     an unvalidated view.
//
// A residual busy rejection is a bounded retryable failure: the pod's miss counter is not cleared
// (retry next pass) and an operator-visible log is surfaced once the retries cross the bound. The
// miss/in-use counters are cleared only when every one of the pod's partitions is reclaimed.
func (r *reclaimer) destroyPod(uid string, entries []markerEntry, claims map[string][]migPlacement) {
	ok := true
	var blocked error

	// Group by accelerator first: the attribution self-check needs no driver call and no lock, so it
	// filters here, and what survives it is destroyed one accelerator at a time under that
	// accelerator's own lock.
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
		done, cardBlocked := r.destroyMarkedInstancesOnCard(uid, card, cardEntries)
		if !done {
			ok = false
		}
		if cardBlocked != nil {
			blocked = worseBlock(blocked, cardBlocked)
		}
	}

	if blocked != nil {
		r.inUse[uid]++
		if r.inUse[uid] == reclaimMaxDestroyMisses {
			r.logger.Error(blocked,
				"reclaim: a partition is still not reclaimable after bounded retries; its placement stays "+
					"occupied until this clears",
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

// destroyMarkedInstancesOnCard destroys every one of a dead pod's partitions on ONE accelerator,
// under that accelerator's lock, re-verifying inside the critical section that each recorded
// GPU-instance id still carries the recorded raw profile id and identity string. It reports whether
// every marker on the accelerator is now gone, and whether any destroy was rejected as busy. A
// reused id is retained, not destroyed: only the stale marker is dropped, because the instance now
// at that id belongs to somebody else.
//
// The driver's live set is re-read ONCE for the whole accelerator rather than once per marker,
// which is the shape that matters: several containers of one Pod on one accelerator are the common
// multi-marker case, and the enumeration is node-wide and expensive. Reading it once is not weaker:
// the lock is held across the whole group, so nothing outside this loop can change the accelerator
// between the markers — only this loop's own destroys do, and each marker is verified against the
// identity it recorded, not against the residue of a sibling's destroy.
func (r *reclaimer) destroyMarkedInstancesOnCard(uid, card string, entries []markerEntry) (done bool, blocked error) {
	unlock := lockCard(card)
	defer unlock()

	instances, lerr := r.driver.CardInstances(card)
	if lerr != nil {
		r.logger.Error(lerr, "reclaim: re-read this card's partitions before destroy, skipping it",
			"podUID", uid, "card", card, "partitions", len(entries))
		return false, nil
	}

	done = true
	for i := range entries {
		m := entries[i].marker
		inst, present := findLiveGi(instances, m.GiID)
		switch {
		case present && (inst.UUID != m.MigUUID || inst.ProfileID != m.ProfileID):
			r.logger.Info("reclaim: gpu-instance id reused by a different partition, dropping stale marker without destroy",
				"podUID", uid, "card", card, "giID", m.GiID,
				"markerUUID", m.MigUUID, "liveUUID", inst.UUID,
				"markerProfileID", m.ProfileID, "liveProfileID", inst.ProfileID)
		case present:
			if berr := r.destroyBlockedBy(card, m.instance()); berr != nil {
				// Keep the marker as well as the partition: it is the record that lets a later pass
				// find this partition again once whatever blocks it clears. Counted as blocked, so a
				// partition held forever crosses the same bounded operator-visible surface it would
				// have crossed by being refused.
				done = false
				blocked = worseBlock(blocked, berr)
				continue
			}
			if derr := r.driver.DestroyInstance(card, m.instance()); derr != nil {
				done = false
				if errors.Is(derr, errInstanceInUse) {
					blocked = worseBlock(blocked, derr)
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
	return done, blocked
}

// destroyBlockedBy reports why a partition must be left alone, or nil when the destroy may proceed.
// It is asked before every destroy this loop makes, in both directions, and what it returns is what
// the bounded log names: errInstanceInUse for a process running on the partition, the query's own
// error for a partition that could not be asked. Those are different faults with different remedies —
// find the process, or give the container access to the driver — and the caller has no other way to
// tell them apart, since the per-pass lines below are not shown at the shipped verbosity.
//
// A partition carrying a live process is somebody's working accelerator whatever the records here
// say, and the orphan sweep is where that matters most: a partition carved outside this operator has
// no marker by definition, so a drained accelerator's sweep is exactly what would rip it out from
// under a running process. Such a partition is left whole and re-examined next pass, so it is
// reclaimed as soon as its last process exits.
//
// What asking FIRST buys, beyond the driver's own refusal, is that the teardown is never started:
// destroying a GPU instance sweeps its compute instances first, so an instance holding one idle and
// one busy compute instance would lose the idle one and only then be refused. That is the failure
// this removes; the refusal alone never covered it.
//
// A skip counts as in use, so a partition held forever still crosses the bounded operator-visible
// surface at reclaimMaxDestroyMisses. It has to: the skip itself is reported at this logger's
// verbosity, which the shipped verbosity does not show, and a placement nothing will ever release
// must not stay occupied silently.
//
// A failure to ask separates into two answers, and they lead opposite ways. NOTHING TO ASK — no
// partition device addresses the partition — is how a GPU instance carrying no compute instance
// reads, and how every partition reads to a container the driver's partition devices are hidden from;
// the destroy proceeds there, because on the first the partition is a routine reclaim target and on
// the second the destroy is partition-scoped too and fails on its own. ASKED AND NO ANSWER is treated
// as in use: starting a teardown on a partition whose state is unknown is what sweeps an idle compute
// instance out of a partition that turns out to be busy, and that is the one thing asking first
// exists to prevent.
//
// The driver's own refusal (errInstanceInUse) remains the backstop either way, for a process
// attaching between this question and the destroy.
func (r *reclaimer) destroyBlockedBy(card string, inst migInstance) error {
	procs, err := r.driver.InstanceProcesses(card, inst)
	switch {
	case errors.Is(err, errNoAddressableDevice):
		r.logger.Info("reclaim: no partition device addresses this partition, leaving the destroy to the driver",
			"card", card, "giID", inst.GiID, "err", err.Error())
		return nil
	case err != nil:
		r.logger.Info("reclaim: this partition could not be asked about its processes, skipping destroy",
			"card", card, "giID", inst.GiID, "err", err.Error())
		return err
	case procs == 0:
		return nil
	default:
		r.logger.Info("reclaim: a process is running on this partition, skipping destroy",
			"card", card, "giID", inst.GiID, "processes", procs)
		return errInstanceInUse
	}
}

// worseBlock picks which blocked partition the bound reports when a pass blocked on several. A
// partition that could not be asked outranks one a process is holding: the first is a fault to
// investigate, the second is a workload doing its job, and naming the workload would bury the fault.
func worseBlock(held, next error) error {
	if held == nil || (errors.Is(held, errInstanceInUse) && !errors.Is(next, errInstanceInUse)) {
		return next
	}
	return held
}

// findLiveGi returns the GPU instance with giID from one accelerator's enumeration, if it holds
// one.
func findLiveGi(instances []migInstance, giID uint32) (migInstance, bool) {
	for i := range instances {
		if instances[i].GiID == giID {
			return instances[i], true
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

// pruneCorruptMarkers retires the unparseable markers whose Pod is gone, so a truncated record
// holds its accelerator back only while it can still stand for something. A corrupt file's contents
// say nothing, but its path names its Pod, and that is evidence enough on its own: once the Pod is
// absent from the live set for reclaimMaxMisses consecutive passes — the same debounce every other
// liveness decision in this loop uses, so a transient list gap never retires a live Pod's record —
// the file is removed. The partition it shadowed then becomes a genuine marker-less orphan the
// collector takes once its accelerator drains, which is how the accelerator converges instead of
// leaking a partition for the node's lifetime. No accelerator lock is needed: the Pod is dead, so
// no concurrent allocation is writing that path.
//
// Two cases are kept indefinitely, deliberately:
//   - a corrupt marker whose Pod is alive — it still records an ownership its Pod depends on;
//   - a corrupt path whose Pod cannot be read from it (a walk error, or not a marker file at marker
//     depth) — with no owner there is no liveness evidence to act on, and it is a filesystem fault
//     to be repaired, not an ownership record to retire.
//
// The second case is permanent, and its cost is node-wide rather than per accelerator: such a path
// names no accelerator either, so ownershipUnknownOnCard darkens every accelerator — no adoption
// anywhere and no orphan collected anywhere — for as long as it stays unreadable. Nothing here can
// fix that without destroying state it cannot account for, so the hold stands; what it must not do
// is stand silently.
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

// holdLiveOwnersMarker keeps the unparseable marker of a live Pod — it still records an ownership
// its Pod depends on — and counts the consecutive passes that hold has cost the accelerator,
// surfacing one operator-visible log at reclaimMaxCorruptHoldMisses. Like the unattributable hold
// it changes no decision: the record is kept either way. The count lives in its own key space,
// because the retirement debounce's key for this same path is reset to zero on every one of these
// passes. The key is touched so the count survives to the next pass, and disappears with it once
// the path does.
//
// The bound is worth spending on this case precisely because it reads as the transient one: the Pod
// is alive, so the hold looks like something the Pod's exit will clear. It is not. While it stands,
// no leftover on the accelerator can be adopted and none can be reclaimed; and the Pod cannot be
// re-admitted either, because a re-created container's reserve reads its own record first and fails
// closed on any parse failure that is not "absent" — so restarting the Pod cannot clear it, and
// nothing the operator still holds can rebuild the record, since the ids that identify the
// partition were only ever in the file. Deleting the Pod is what releases the accelerator: the
// retirement debounce then removes the record on the evidence of its path.
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
		"reclaim: an unreadable partition ownership record of a RUNNING pod is holding its card closed: while it "+
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
			"reclaim: an unreadable path under the pod work root names neither a pod nor a card, so partition "+
				"ownership cannot be proven on ANY card of this node: no leftover partition is adopted and none is "+
				"reclaimed while it persists; this is a filesystem fault that will not clear by itself — repair or "+
				"remove the path",
			"path", path, "passes", r.misses[key])
	}
}

// destroyOrphans removes the marker-less GPU instances on a fully drained accelerator (no live
// Pod). It re-scans the markers under the accelerator lock and bails if the accelerator now carries
// ANY marker: create+marker is atomic under this same lock, so a marker appearing since the
// lock-free snapshot means an allocation arrived and the accelerator is no longer fully drained —
// its orphans wait for a later pass. An UNPARSEABLE marker leaving the accelerator's ownership
// unknowable bails the same way, through the same shared predicate the pass itself consults: the
// two checks are not redundant, because the pass's verdict rests on a snapshot the lock does not
// cover, and this one is what actually spares the partition that record owns. An orphan running a
// process is skipped as well: carved outside this operator or not, it is in use, and a drained
// accelerator says nothing about that. A residual busy rejection is a bounded retryable failure with
// the same log-at-the-bound surface as the per-pod path; the miss counter is cleared only when every
// removal succeeds.
func (r *reclaimer) destroyOrphans(missKey, card string, orphans []migInstance) {
	unlock := lockCard(card)
	defer unlock()

	entries, corrupt := scanMarkers(r.podsDir)
	if len(ownedGiIDsOnCard(entries, card)) > 0 || ownershipUnknownOnCard(corrupt, card) {
		return
	}

	ok := true
	var blocked error
	destroyed := 0
	for _, inst := range orphans {
		if berr := r.destroyBlockedBy(card, inst); berr != nil {
			ok = false
			blocked = worseBlock(blocked, berr)
			continue
		}
		if derr := r.driver.DestroyInstance(card, inst); derr != nil {
			ok = false
			if errors.Is(derr, errInstanceInUse) {
				blocked = worseBlock(blocked, derr)
				continue
			}
			r.logger.Error(derr, "reclaim: destroy marker-less partition on drained card", "card", card, "giID", inst.GiID)
			continue
		}
		destroyed++
	}

	if blocked != nil {
		r.inUse[missKey]++
		if r.inUse[missKey] == reclaimMaxDestroyMisses {
			r.logger.Error(blocked,
				"reclaim: a marker-less partition on a drained card is still not reclaimable after bounded "+
					"retries; its placement stays occupied until this clears",
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
