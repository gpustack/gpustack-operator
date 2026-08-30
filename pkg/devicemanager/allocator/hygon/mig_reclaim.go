package hygon

import (
	"errors"

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
}

func newMigReclaimer(drv migDriver, podsDir string, logger klog.Logger) *migReclaimer {
	return &migReclaimer{drv: drv, podsDir: podsDir, logger: logger}
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
		r.release(entries[i])
	}

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

	release := lockMigCard(m.Card)
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
		err := r.drv.DestroyInstance(card, inst)
		release()
		switch {
		case err == nil:
			logger.Info("collected a partition no allocation claims")
		case errors.Is(err, errInstanceInUse):
			logger.V(3).Info("a partition no allocation claims is in use; leaving it for the next pass")
		default:
			logger.Error(err, "failed to collect a partition no allocation claims")
		}
	}
}
