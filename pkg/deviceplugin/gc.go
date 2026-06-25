package deviceplugin

import (
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/sets"
)

// podDirGCMaxMisses is the number of consecutive reconciles a pod UUID must be
// absent from the live set before its working directory is reclaimed.
const podDirGCMaxMisses = 3

// podDirGC reclaims per-pod soft-slicing working directories under a base dir once
// their pod is gone. It is level-based: each reconcile re-scans the directory and
// compares it against the live pod-UUID set, so it self-heals across restarts. A
// directory is removed only after its UUID has been absent from the live set for
// podDirGCMaxMisses consecutive reconciles (guards against transient list gaps).
type podDirGC struct {
	dir    string
	misses map[string]int // pod UUID -> consecutive misses
}

// newPodDirGC builds a GC for dir, seeding the tracker with the pod UUIDs already
// present on disk so directories orphaned before this process started are reclaimed.
func newPodDirGC(dir string) *podDirGC {
	g := &podDirGC{dir: dir, misses: make(map[string]int)}
	for uid := range g.scanPodUIDs() {
		g.misses[uid] = 0
	}
	return g
}

// scanPodUIDs returns the set of pod-UUID directory names currently under dir.
func (g *podDirGC) scanPodUIDs() sets.Set[string] {
	uids := sets.New[string]()
	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return uids
	}
	for _, e := range entries {
		if e.IsDir() {
			uids.Insert(e.Name())
		}
	}
	return uids
}

// reconcile compares the on-disk pod directories against the live pod-UUID set:
// a directory whose UUID is live resets its miss count; one absent from the live
// set increments it and is removed after podDirGCMaxMisses consecutive misses.
func (g *podDirGC) reconcile(livePodUIDs []string) {
	live := sets.New[string](livePodUIDs...)
	onDisk := g.scanPodUIDs()

	for uid := range onDisk {
		if live.Has(uid) {
			g.misses[uid] = 0
			continue
		}
		g.misses[uid]++
		if g.misses[uid] >= podDirGCMaxMisses {
			// Clear the tracker entry only when removal succeeds; on failure
			// (permissions, transient IO) keep the entry so the next reconcile
			// retries immediately instead of resetting the miss streak.
			if err := os.RemoveAll(filepath.Join(g.dir, uid)); err == nil {
				delete(g.misses, uid)
			}
		}
	}

	// Drop tracker entries whose directory no longer exists (removed out-of-band).
	for uid := range g.misses {
		if !onDisk.Has(uid) {
			delete(g.misses, uid)
		}
	}
}
