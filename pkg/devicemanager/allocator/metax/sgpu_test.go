package metax

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

const (
	testBDF0 = "0000:3d:00.0"
	testBDF1 = "0000:3e:00.0"
)

// redirectLogicalSliceDirs points the logical-slicing host paths at a temp dir for the test.
func redirectLogicalSliceDirs(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	origLib, origPods := deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir
	deviceplugin.OperatorLibDir = filepath.Join(root, "lib")
	deviceplugin.OperatorPodsDir = filepath.Join(root, "pods")
	t.Cleanup(func() {
		deviceplugin.OperatorLibDir = origLib
		deviceplugin.OperatorPodsDir = origPods
	})
}

// fakeSGPUManager is an in-memory sgpuManager: the injectable seam that lets the
// encode / marker / slot / reclaim logic be tested without MetaX hardware.
type fakeSGPUManager struct {
	mu      sync.Mutex
	subdevs map[subdevKey]string // (bdf,index) -> alias
	ensured map[string]bool
	sched   map[string]schedClass
	failBDF map[string]bool // Create fails here (simulates a foreign/incompatible card)
	creates int
	removed []subdevKey
}

func newFakeMgr() *fakeSGPUManager {
	return &fakeSGPUManager{
		subdevs: map[subdevKey]string{},
		ensured: map[string]bool{},
		sched:   map[string]schedClass{},
		failBDF: map[string]bool{},
	}
}

func (f *fakeSGPUManager) EnsureModel(bdf string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured[bdf] = true
	return nil
}

func (f *fakeSGPUManager) SetSchedClass(bdf string, c schedClass) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sched[bdf] = c
	return nil
}

func (f *fakeSGPUManager) Create(bdf string, index int, _ int64, alias string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failBDF[bdf] {
		return fmt.Errorf("driver rejected create on %s", bdf)
	}
	f.subdevs[subdevKey{bdf, index}] = alias
	f.creates++
	return nil
}

func (f *fakeSGPUManager) Remove(bdf string, index int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subdevs, subdevKey{bdf, index})
	f.removed = append(f.removed, subdevKey{bdf, index})
	return nil
}

func (f *fakeSGPUManager) List() ([]sgpuSubdevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sgpuSubdevice, 0, len(f.subdevs))
	for k, alias := range f.subdevs {
		out = append(out, sgpuSubdevice{bdf: k.bdf, index: k.index, alias: alias})
	}
	return out, nil
}

// seed injects a subdevice directly, simulating a pre-existing / crash-orphaned one.
func (f *fakeSGPUManager) seed(bdf string, index int, alias string) {
	f.subdevs[subdevKey{bdf, index}] = alias
}

func (f *fakeSGPUManager) has(bdf string, index int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.subdevs[subdevKey{bdf, index}]
	return ok
}

func seedMarker(t *testing.T, uid, ctr, bdf string, index, cores int, mem int64) {
	t.Helper()
	require.NoError(t, writeMarker(markerPath(uid, ctr), sgpuMarker{
		PodUID: uid, Container: ctr, CardBDF: bdf, Index: index, CoresPct: cores, MemMiB: mem,
	}))
}

func markerExists(uid, ctr string) bool {
	_, err := os.Stat(markerPath(uid, ctr))
	return err == nil
}

func Test_encodeMetaxSGPUs(t *testing.T) {
	got := encodeMetaxSGPUs(testBDF0, 2, 60, 32768, encodeAlias("pod-uid-1"))
	assert.Equal(t, "sgpu=0000:3d:00.0#2;compute=60;vram=32768;alias=gpustack-pod-uid-1", got)
}

func Test_aliasEncodeDecode(t *testing.T) {
	cases := []struct {
		alias   string
		wantUID string
		wantOK  bool
	}{
		{encodeAlias("abc"), "abc", true},
		{"gpustack-", "", false}, // prefix only, no UID
		{"foreign-tag", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		uid, ok := decodeAliasUID(c.alias)
		assert.Equalf(t, c.wantOK, ok, "alias %q", c.alias)
		assert.Equalf(t, c.wantUID, uid, "alias %q", c.alias)
	}
}

func Test_marker_roundtrip_and_failClosed(t *testing.T) {
	redirectLogicalSliceDirs(t)

	// Round-trip: write then parse returns the same record.
	path := markerPath("uid-rt", "train")
	want := sgpuMarker{PodUID: "uid-rt", Container: "train", CardBDF: testBDF0, Index: 3, CoresPct: 60, MemMiB: 32768}
	require.NoError(t, writeMarker(path, want))
	got, err := parseMarker(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// Corrupt JSON fails closed.
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err = parseMarker(path)
	require.Error(t, err)

	// Incomplete record (missing required fields) fails closed.
	require.NoError(t, os.WriteFile(path, []byte(`{"container":"train"}`), 0o644))
	_, err = parseMarker(path)
	require.Error(t, err)
}

// A corrupt marker for one pod must not block deriving a slot for another pod (the
// fail-closed scope is the owning pod's own marker, backstopped by the registry).
func Test_reserveSlice_corruptOtherMarkerDoesNotBlock(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()

	// A corrupt marker belonging to some other pod.
	other := markerPath("uid-corrupt", "train")
	require.NoError(t, osMkdirAllFor(other))
	require.NoError(t, os.WriteFile(other, []byte("{garbage"), 0o644))

	res, err := reserveSlice(mgr, "uid-ok", "train", testBDF0, 50, 1024, false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.index)
}

func osMkdirAllFor(file string) error {
	return os.MkdirAll(filepath.Dir(file), 0o777)
}

func Test_reserveSlice_indexDerivation(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()

	// First slice on accelerator 0 -> index 0.
	r0, err := reserveSlice(mgr, "uid-a", "train", testBDF0, 50, 1024, false)
	require.NoError(t, err)
	assert.Equal(t, 0, r0.index)
	assert.True(t, mgr.ensured[testBDF0], "first slice ensures sgpu model")
	assert.Equal(t, schedFixedShare, mgr.sched[testBDF0], "first slice sets fixed-share")

	// Second slice on the SAME accelerator -> index 1 (lowest free).
	r1, err := reserveSlice(mgr, "uid-b", "train", testBDF0, 50, 1024, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r1.index)

	// Slice on a DIFFERENT accelerator -> index 0 again.
	r2, err := reserveSlice(mgr, "uid-c", "train", testBDF1, 50, 1024, false)
	require.NoError(t, err)
	assert.Equal(t, 0, r2.index)
}

// A driver subdevice with no marker (a crash orphan) still occupies its index, so the
// next allocation must skip it (registry UNION markers, never markers alone).
func Test_reserveSlice_registryOrphanOccupiesIndex(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	mgr.seed(testBDF0, 0, "") // orphan at index 0, no marker

	res, err := reserveSlice(mgr, "uid-a", "train", testBDF0, 50, 1024, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.index, "must skip the marker-less orphan at index 0")
	assert.False(t, mgr.ensured[testBDF0], "card already has a subdevice: do not re-ensure model")
}

func Test_reserveSlice_idempotentReuseAndFailClosed(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()

	r0, err := reserveSlice(mgr, "uid-a", "train", testBDF0, 60, 2048, false)
	require.NoError(t, err)
	assert.Equal(t, 0, r0.index)
	assert.Equal(t, 1, mgr.creates)

	// Re-Allocate the same pod+container+accelerator+cores+mem: reuse, no new subdevice.
	r1, err := reserveSlice(mgr, "uid-a", "train", testBDF0, 60, 2048, false)
	require.NoError(t, err)
	assert.Equal(t, 0, r1.index)
	assert.Equal(t, 1, mgr.creates, "reuse must not create a second subdevice")

	// A changed parameter (immutable requests) fails closed rather than mutating.
	_, err = reserveSlice(mgr, "uid-a", "train", testBDF0, 80, 2048, false)
	require.Error(t, err)

	// A reused marker whose subdevice was removed out-of-band (driver GC / manual cleanup)
	// fails closed rather than injecting METAX_SGPUS for a missing subdevice.
	require.NoError(t, mgr.Remove(testBDF0, 0))
	_, err = reserveSlice(mgr, "uid-a", "train", testBDF0, 60, 2048, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing sgpu subdevice")
}

func Test_reserveSlice_wholeCard(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()

	res, err := reserveSlice(mgr, "uid-whole", "train", testBDF0, 100, 65536, true)
	require.NoError(t, err)
	assert.True(t, res.wholeCard)
	assert.Empty(t, res.envValue, "whole card injects no METAX_SGPUS entry")
	assert.Equal(t, 0, mgr.creates, "whole card creates no sgpu subdevice")

	// The occupancy marker is written with the whole-accelerator sentinel index.
	m, err := parseMarker(markerPath("uid-whole", "train"))
	require.NoError(t, err)
	assert.True(t, m.wholeCard())

	// Reuse is idempotent.
	res, err = reserveSlice(mgr, "uid-whole", "train", testBDF0, 100, 65536, true)
	require.NoError(t, err)
	assert.True(t, res.wholeCard)
	assert.Equal(t, 0, mgr.creates)

	// A partial slice cannot land on an accelerator already taken whole, and vice versa.
	_, err = reserveSlice(mgr, "uid-partial", "train", testBDF0, 50, 1024, false)
	require.Error(t, err)
}

func Test_reserveSlice_poolExhausted(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	for i := 0; i < maxSGPUPerCard; i++ {
		mgr.seed(testBDF0, i, "")
	}
	_, err := reserveSlice(mgr, "uid-a", "train", testBDF0, 50, 1024, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exhausted")
}

func Test_reclaim_deadPodDestroysAfterDebounce(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	_, err := reserveSlice(mgr, "pod-dead", "train", testBDF0, 60, 1024, false)
	require.NoError(t, err)

	r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())

	// Two absent reconciles: debounced, nothing reclaimed.
	r.reconcile(nil)
	r.reconcile(nil)
	assert.True(t, mgr.has(testBDF0, 0), "subdevice must survive the debounce window")
	assert.True(t, markerExists("pod-dead", "train"))

	// Third absent reconcile: reclaimed.
	r.reconcile(nil)
	assert.False(t, mgr.has(testBDF0, 0), "subdevice destroyed after 3 misses")
	assert.False(t, markerExists("pod-dead", "train"), "marker removed")
}

// A live pod's slice is never touched, and a live snapshot resets the miss streak.
func Test_reclaim_livePodPreserved(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	_, err := reserveSlice(mgr, "pod-live", "train", testBDF0, 60, 1024, false)
	require.NoError(t, err)

	r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())
	// Absent twice, then live: the streak resets.
	r.reconcile(nil)
	r.reconcile(nil)
	r.reconcile([]string{"pod-live"})
	// Two more absent: still debounced (streak restarted).
	r.reconcile(nil)
	r.reconcile(nil)
	assert.True(t, mgr.has(testBDF0, 0), "a reset streak must not reclaim early")
	assert.True(t, markerExists("pod-live", "train"))
}

// A marker whose subdevice is already gone (external teardown) is cleaned once its
// pod is dead: Remove is a no-op, the marker file is removed.
func Test_reclaim_subdeviceLessMarker(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	seedMarker(t, "pod-dead", "train", testBDF0, 0, 60, 1024) // marker only, no subdevice

	r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())
	r.reconcile(nil)
	r.reconcile(nil)
	r.reconcile(nil)
	assert.False(t, markerExists("pod-dead", "train"), "subdevice-less marker cleaned")
}

func Test_reclaim_markerLessOrphan(t *testing.T) {
	t.Run("dead UID destroyed", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		mgr := newFakeMgr()
		mgr.seed(testBDF0, 5, encodeAlias("pod-dead")) // orphan, no marker

		r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())
		r.reconcile(nil)
		r.reconcile(nil)
		assert.True(t, mgr.has(testBDF0, 5), "debounced")
		r.reconcile(nil)
		assert.False(t, mgr.has(testBDF0, 5), "dead-UID orphan destroyed after 3 misses")
	})

	t.Run("live UID left intact", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		mgr := newFakeMgr()
		mgr.seed(testBDF0, 5, encodeAlias("pod-live")) // create-before-marker crash on a reserved pod

		r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())
		for i := 0; i < 5; i++ {
			r.reconcile([]string{"pod-live"})
		}
		assert.True(t, mgr.has(testBDF0, 5), "a live-UID marker-less subdevice is never destroyed")
	})

	t.Run("undecodable alias on a drained accelerator reclaimed", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		mgr := newFakeMgr()
		mgr.seed(testBDF0, 5, "")            // driver did not expose the operator's alias
		mgr.seed(testBDF1, 2, "foreign-tag") // undecodable owner

		r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())
		r.reconcile(nil)
		r.reconcile(nil)
		assert.True(t, mgr.has(testBDF0, 5), "debounced")
		r.reconcile(nil)
		assert.False(t, mgr.has(testBDF0, 5), "an orphan on a card with no live pod is reclaimed once drained")
		assert.False(t, mgr.has(testBDF1, 2))
	})

	t.Run("undecodable alias on an accelerator with a live pod left intact", func(t *testing.T) {
		redirectLogicalSliceDirs(t)
		mgr := newFakeMgr()
		// A live pod holds a slice on the accelerator, plus an unidentifiable subdevice (could be
		// that pod's own create-before-marker crash orphan) — never destroy it.
		_, err := reserveSlice(mgr, "pod-live", "train", testBDF0, 60, 1024, false)
		require.NoError(t, err)
		mgr.seed(testBDF0, 9, "")

		r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())
		for i := 0; i < 5; i++ {
			r.reconcile([]string{"pod-live"})
		}
		assert.True(t, mgr.has(testBDF0, 9), "a card hosting a live pod keeps its unidentifiable subdevices")
	})
}

// Reclaiming one container of a dead multi-container pod must remove only the specific
// marker files (never RemoveAll a dir), and clean the pod dir only when empty.
func Test_reclaim_multiContainerPod(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	_, err := reserveSlice(mgr, "pod-dead", "c1", testBDF0, 60, 1024, false)
	require.NoError(t, err)
	_, err = reserveSlice(mgr, "pod-dead", "c2", testBDF0, 60, 1024, false)
	require.NoError(t, err)

	r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())
	r.reconcile(nil)
	r.reconcile(nil)
	r.reconcile(nil)

	assert.False(t, markerExists("pod-dead", "c1"))
	assert.False(t, markerExists("pod-dead", "c2"))
	assert.False(t, mgr.has(testBDF0, 0))
	assert.False(t, mgr.has(testBDF0, 1))
	// The now-empty pod dir is cleaned.
	_, statErr := os.Stat(filepath.Join(deviceplugin.OperatorPodsDir, "pod-dead"))
	assert.True(t, os.IsNotExist(statErr), "empty pod dir removed")
}

// removeIfEmpty must never remove a dir that still holds a sibling's marker.
func Test_removeIfEmpty_leavesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o644))
	removeIfEmpty(dir)
	_, err := os.Stat(dir)
	assert.NoError(t, err, "a non-empty dir must survive")
}

// The real sysfs manager, exercised against a fake /sys root: this locks the path
// construction and List's parsing (the schema itself is a hardware open question, so
// this documents the assumed layout rather than validating it against a driver).
func Test_sysfsSGPUManager(t *testing.T) {
	root := t.TempDir()
	mgr := &sysfsSGPUManager{root: root}
	cardDir := filepath.Join(root, testBDF0)
	sgpuDir := filepath.Join(cardDir, "sgpu")
	require.NoError(t, os.MkdirAll(sgpuDir, 0o777))

	require.NoError(t, mgr.EnsureModel(testBDF0))
	model, err := os.ReadFile(filepath.Join(cardDir, "model"))
	require.NoError(t, err)
	assert.Equal(t, "sgpu", string(model))

	require.NoError(t, mgr.SetSchedClass(testBDF0, schedFixedShare))
	sc, err := os.ReadFile(filepath.Join(sgpuDir, "sched_class"))
	require.NoError(t, err)
	assert.Equal(t, "1", string(sc))

	require.NoError(t, mgr.Create(testBDF0, 0, 4096, encodeAlias("pod-x")))
	create, err := os.ReadFile(filepath.Join(sgpuDir, "create"))
	require.NoError(t, err)
	assert.Equal(t, "4096", string(create))

	// List enumerates seeded sgpu<N> subdevice dirs and reads back their alias.
	require.NoError(t, os.MkdirAll(filepath.Join(sgpuDir, "sgpu3"), 0o777))
	require.NoError(t, os.WriteFile(filepath.Join(sgpuDir, "sgpu3", "alias"), []byte("gpustack-pod-x\n"), 0o644))
	subdevs, err := mgr.List()
	require.NoError(t, err)
	require.Len(t, subdevs, 1)
	assert.Equal(t, sgpuSubdevice{bdf: testBDF0, index: 3, alias: "gpustack-pod-x"}, subdevs[0])

	// Remove tolerates an already-absent subdevice dir (no error).
	require.NoError(t, mgr.Remove("0000:ff:00.0", 0))
}

// Concurrent Allocate + reclaim must be race-free and never double-book an index.
func Test_reserveSlice_reclaim_race(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	r := newReclaimer(mgr, deviceplugin.OperatorPodsDir, logr.Discard())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = reserveSlice(mgr,
				fmt.Sprintf("pod-%d", i), "train", testBDF0, 50, 1024, false)
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.reconcile(nil)
		}()
	}
	wg.Wait()

	// Every surviving subdevice occupies a distinct index (allocMu prevents double-book).
	seen := map[int]bool{}
	list, _ := mgr.List()
	for _, sd := range list {
		assert.Falsef(t, seen[sd.index], "index %d double-booked", sd.index)
		seen[sd.index] = true
	}
}

// TestLegacyMarkerRoundTrip pins the marker's ON-DISK format (F9/AC9.1). sgpuMarker.CardBDF is
// serialized under the "cardBDF" JSON key, and markers carrying it are already on real nodes: a
// renamed tag
// would make every pre-upgrade marker unreadable and break retry, visibility, adoption and
// reclamation. The other marker tests write and read through the same struct, so they stay green
// under a coordinated tag rename; this one feeds a literal pre-refactor document instead, and
// asserts the exact legacy key still comes back out.
func TestLegacyMarkerRoundTrip(t *testing.T) {
	testCases := []struct {
		name string
		doc  string
		want sgpuMarker
	}{
		{
			name: "pre-refactor partial-slice marker",
			doc: `{"podUID":"pod-a","container":"train","cardBDF":"0000:3d:00.0",` +
				`"index":2,"coresPct":25,"memMiB":16384}`,
			want: sgpuMarker{
				PodUID:    "pod-a",
				Container: "train",
				CardBDF:   "0000:3d:00.0",
				Index:     2,
				CoresPct:  25,
				MemMiB:    16384,
			},
		},
		{
			name: "pre-refactor whole-accelerator occupancy marker",
			doc: `{"memMiB":32768,"coresPct":100,"index":-1,"cardBDF":"0000:3e:00.0",` +
				`"container":"infer","podUID":"pod-b"}`,
			want: sgpuMarker{
				PodUID:    "pod-b",
				Container: "infer",
				CardBDF:   "0000:3e:00.0",
				Index:     wholeCardIndex,
				CoresPct:  100,
				MemMiB:    32768,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), markerName)
			require.NoError(t, os.WriteFile(path, []byte(tc.doc), 0o600))

			got, err := parseMarker(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got, "every identity must survive the legacy document")

			data, err := json.Marshal(got)
			require.NoError(t, err)
			assert.Contains(t, string(data), `"cardBDF":`, "the legacy on-disk key must be re-emitted")
			assert.NotContains(t, string(data), `"accelerator"`, "no vocabulary key may leak on disk")
		})
	}
}
