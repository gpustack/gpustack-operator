package hygon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

// migReclaimer frees the partitions whose holder is gone.
//
// It exists because nothing else can. The device-plugin protocol has no release callback, so a Pod's
// disappearance frees nothing on the driver: a GPU instance carved for it stays carved for the life
// of the node. On this vendor that is worse than a leak of capacity -- Multi-Instance mode cannot be
// turned off while any instance exists, so a node accumulating abandoned partitions cannot be
// returned to whole-card service at all.
//
// The reconcile is level-based: each pass is handed the live Pod set and destroys what no live Pod
// holds. Nothing is remembered between passes, so a missed tick costs a delay and never a decision.
type migReclaimer struct {
	drv     migDriver
	podsDir string
	logger  klog.Logger
	// now is the clock the grace window below is measured against, injectable for tests.
	now func() time.Time
}

// migReclaimGrace is how long a freshly written ownership record is left alone.
//
// The live Pod set this pass is handed is a SNAPSHOT, and a record written after that snapshot was
// taken cannot be judged by it -- the Pod is live, it is simply not in the set yet. Without this,
// a burst of admissions loses its partitions: measured with three Pods admitted together, all three
// had their instance destroyed and their record removed while they were starting, and all three then
// resolved to the same stale partition. There is no cheaper signal to key on, because a record is
// written before the allocation annotation the snapshot is built from.
//
// The cost of the window is only latency on a genuinely abandoned partition, which the next pass
// takes.
const migReclaimGrace = 2 * time.Minute

func newMigReclaimer(drv migDriver, podsDir string, logger klog.Logger) *migReclaimer {
	return &migReclaimer{drv: drv, podsDir: podsDir, logger: logger, now: time.Now}
}

// reconcile destroys every partition whose Pod is gone, then collects the ones no record claims.
//
// The two halves are separate because they are certain in different degrees. A record naming a dead
// Pod is unambiguous: something owned that partition and no longer exists. An instance with no record
// is not -- it can equally be one being created right now, by a reservation that has not written its
// record yet -- so it is only collected when the record set is complete enough to prove it.
func (r *migReclaimer) reconcile(livePodUIDs []string) {
	live := sets.New(livePodUIDs...)

	// Every instance ANY record named this pass, whether it survived or was just released. The
	// orphan sweep below considers only what no record mentioned at all: an instance this pass
	// already destroyed is not an orphan, and asking the driver to destroy it a second time would
	// race a concurrent reservation that has since taken the same id.
	entries, corrupt := scanMigMarkers(r.podsDir)
	seen := make(map[string]sets.Set[uint32], len(entries))
	for i := range entries {
		m := entries[i].marker
		r.note(seen, m)
		if live.Has(m.PodUID) {
			continue
		}
		if r.withinGrace(entries[i].path) {
			// Written after the snapshot this pass was handed, so the snapshot says nothing about it.
			continue
		}
		r.release(entries[i])
	}

	r.sweepRuntimeDirs()

	if len(corrupt) != 0 {
		// A record that cannot be read might claim any instance, so nothing can be shown to be
		// unclaimed. Reporting it once per pass is worth the noise: an operator who never clears it
		// has a node whose abandoned partitions are never collected.
		r.logger.Info("skipped the orphan-partition sweep because some ownership records are unreadable",
			"records", len(corrupt))
		return
	}
	r.collectOrphans(seen)
}

// withinGrace reports whether a record is too young to be judged by this pass's live set. A record
// whose age cannot be read is treated as young: refusing to destroy is the safe direction.
func (r *migReclaimer) withinGrace(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	return r.now().Sub(info.ModTime()) < migReclaimGrace
}

// note records that some ownership record named this partition during this pass.
func (r *migReclaimer) note(seen map[string]sets.Set[uint32], m migMarker) {
	if seen[m.PciBusID] == nil {
		seen[m.PciBusID] = sets.New[uint32]()
	}
	seen[m.PciBusID].Insert(m.GiID)
}

// release destroys one abandoned partition and drops its record.
//
// The record is dropped only after the driver confirms the partition is gone: a record removed while
// its instance survives makes that instance an orphan nobody can attribute, and the sweep below
// would then be the only thing left to notice it.
func (r *migReclaimer) release(entry migMarkerEntry) {
	m := entry.marker
	logger := r.logger.WithValues(
		"pod", m.PodUID, "container", m.Container, "card", m.Card, "gpuInstance", m.GiID)

	release := lockMigCard(m.PciBusID)
	defer release()

	if err := r.drv.DestroyInstance(m.PciBusID, m.instance()); err != nil {
		if errors.Is(err, errInstanceInUse) {
			// A process outliving its Pod's deletion. Nothing to do but wait: destroying it out from
			// under a running workload is what the driver is refusing, and it is right to.
			logger.V(3).Info("a partition whose pod is gone is still in use; leaving it for the next pass")
			return
		}
		logger.Error(err, "failed to destroy the partition of a pod that is gone")
		return
	}

	if err := osx.Remove(entry.path); err != nil {
		logger.Error(err, "destroyed an abandoned partition but could not drop its ownership record")
		return
	}
	logger.Info("destroyed the partition of a pod that is gone")
}

// collectOrphans destroys the live partitions no ownership record claims.
//
// These are what a crash between a create and its record leaves behind. The reservation path adopts
// one when the next request for the same profile arrives, which handles the common case; this
// handles the rest -- an orphan of a profile nobody asks for again would otherwise hold its slices
// until the node is rebuilt.
//
// The ownership is re-read UNDER THE ACCELERATOR LOCK before anything is destroyed, and the snapshot
// taken by the caller is only a pre-filter. Without that, this sweep tears down partitions that
// allocations are still assembling: a reservation writes its record and releases the lock before its
// caller finishes building the container response, so an unlocked sweep whose record scan predates
// that write sees a live instance nobody claims and destroys it. That is not a narrow window --
// observed three times in one second on a node admitting three Pods, each losing the partition it
// had just been granted -- and it is made likelier by the driver reusing a freed instance id
// immediately.
func (r *migReclaimer) collectOrphans(seen map[string]sets.Set[uint32]) {
	live, err := r.drv.ListInstances()
	if err != nil {
		r.logger.V(3).Error(err, "could not enumerate partitions, so none were collected this pass")
		return
	}

	for i := range live {
		card, inst := live[i].PciBusID, live[i].Instance
		if seen[card].Has(inst.GiID) {
			continue
		}
		logger := r.logger.WithValues("card", card, "gpuInstance", inst.GiID)

		release := lockMigCard(card)
		err := r.destroyIfStillUnclaimed(card, inst)
		release()
		switch {
		case err == nil:
			logger.Info("collected a partition no allocation claims")
		case errors.Is(err, errMigInstanceClaimed):
			logger.V(3).Info("a partition was claimed while this pass was deciding; leaving it alone")
		case errors.Is(err, errInstanceInUse):
			logger.V(3).Info("a partition no allocation claims is in use; leaving it for the next pass")
		default:
			logger.Error(err, "failed to collect a partition no allocation claims")
		}
	}
}

// sweepRuntimeDirs removes the DIRECTORIES a container runtime can leave in the vendor's instance
// registry, which permanently poison the instance ids they are named after.
//
// The driver writes plain files here, one per compute instance, and a container is given one of them
// as a bind mount. A runtime asked to bind a source path that does not exist creates it -- as a
// DIRECTORY -- so an instance destroyed between the allocation and the container starting leaves a
// directory sitting on its name. The driver can then never write that name again: creating a compute
// instance whose id maps to it fails with INSUFFICIENT_RESOURCES, and since ids are handed back out
// after a destroy, that failure outlives everything that caused it. Observed on a node where two ids
// became permanently uncreatable.
//
// Only directories are removed, and only empty ones. A file here is the driver's own record and is
// never this function's business; a non-empty directory is something else again and is reported
// rather than removed.
func (r *migReclaimer) sweepRuntimeDirs() {
	ciDir := filepath.Join(migConfigDir, "ci")
	entries, err := os.ReadDir(ciDir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.logger.V(3).Error(err, "could not read the instance registry to sweep it", "dir", ciDir)
		}
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(ciDir, entry.Name())
		if err := os.Remove(path); err != nil {
			r.logger.Error(err,
				"a directory occupies an instance registry name, so the driver can never create that "+
					"instance again, and it could not be removed", "path", path)
			continue
		}
		r.logger.Info("removed a directory a container runtime left on an instance registry name",
			"path", path)
	}
}

// errMigInstanceClaimed reports that an ownership record for a candidate appeared between the
// caller's scan and this lock being taken, so the candidate is not an orphan after all.
var errMigInstanceClaimed = errors.New("partition is claimed")

// destroyIfStillUnclaimed re-reads the ownership records with the accelerator's lock held and
// destroys the instance only if nothing claims it now.
//
// A record that cannot be read makes ownership unknowable, which is refused for the same reason the
// caller refuses the whole sweep on one: an instance whose owner might exist but be unreadable must
// not be taken for an orphan.
func (r *migReclaimer) destroyIfStillUnclaimed(pciBusID string, inst migInstance) error {
	entries, corrupt := scanMigMarkers(r.podsDir)
	r.sweepRuntimeDirs()

	if len(corrupt) != 0 {
		return fmt.Errorf("%w: some ownership records are unreadable", errMigInstanceClaimed)
	}
	for i := range entries {
		m := entries[i].marker
		if m.PciBusID == pciBusID && m.GiID == inst.GiID {
			return errMigInstanceClaimed
		}
	}
	return r.drv.DestroyInstance(pciBusID, inst)
}
