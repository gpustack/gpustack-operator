package cambricon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

const (
	testCard0 = "0000:5c:00.0"
	testCard1 = "0000:5d:00.0"
)

// redirectSoftSliceDirs points the soft-slicing host paths at a temp dir for the test.
func redirectSoftSliceDirs(t *testing.T) {
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

type fakeQuota struct {
	cores int
	mem   int64
}

// fakeSMLUDriver is an in-memory smluDriver: the injectable seam that lets the mapping /
// name-encoding / marker / profile-reuse / reclaim logic be tested without Cambricon
// hardware.
type fakeSMLUDriver struct {
	mu              sync.Mutex
	instances       map[string]smluInstance // name -> instance
	profiles        map[profileKey]fakeQuota
	ensured         map[string]bool
	nextProfileID   int32
	profileCreates  int
	instanceCreates int
	destroyedProfs  []profileKey
	failCard        map[string]bool
	failList        bool
	failProfileList bool
}

func newFakeDriver() *fakeSMLUDriver {
	return &fakeSMLUDriver{
		instances: map[string]smluInstance{},
		profiles:  map[profileKey]fakeQuota{},
		ensured:   map[string]bool{},
		failCard:  map[string]bool{},
	}
}

func (f *fakeSMLUDriver) EnsureSMLUMode(card string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured[card] = true
	return nil
}

func (f *fakeSMLUDriver) CreateProfile(card string, coresPct int, memMiB int64) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextProfileID
	f.nextProfileID++
	f.profiles[profileKey{card: card, profileID: id}] = fakeQuota{cores: coresPct, mem: memMiB}
	f.profileCreates++
	return id, nil
}

func (f *fakeSMLUDriver) DestroyProfile(card string, profileID int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pk := profileKey{card: card, profileID: profileID}
	delete(f.profiles, pk)
	f.destroyedProfs = append(f.destroyedProfs, pk)
	return nil
}

func (f *fakeSMLUDriver) CreateInstance(card string, profileID int32, name string) (smluInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCard[card] {
		return smluInstance{}, fmt.Errorf("driver rejected create on %s", card)
	}
	q := f.profiles[profileKey{card: card, profileID: profileID}]
	inst := smluInstance{
		card:      card,
		name:      name,
		profileID: profileID,
		coresPct:  q.cores,
		memMiB:    q.mem,
		devNode:   "/dev/cambricon_dev-" + name,
	}
	f.instances[name] = inst
	f.instanceCreates++
	return inst, nil
}

func (f *fakeSMLUDriver) DestroyInstance(_, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, name)
	return nil
}

func (f *fakeSMLUDriver) ListInstances() ([]smluInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList {
		return nil, fmt.Errorf("driver list failed")
	}
	out := make([]smluInstance, 0, len(f.instances))
	for _, inst := range f.instances {
		out = append(out, inst)
	}
	return out, nil
}

func (f *fakeSMLUDriver) ListProfiles() ([]profileKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList || f.failProfileList {
		return nil, fmt.Errorf("driver profile list failed")
	}
	out := make([]profileKey, 0, len(f.profiles))
	for pk := range f.profiles {
		out = append(out, pk)
	}
	return out, nil
}

// seedInstance injects an instance (and its profile) directly, simulating a
// pre-existing / crash-orphaned one with no marker.
func (f *fakeSMLUDriver) seedInstance(inst smluInstance) {
	f.instances[inst.name] = inst
	f.profiles[profileKey{card: inst.card, profileID: inst.profileID}] = fakeQuota{cores: inst.coresPct, mem: inst.memMiB}
}

func (f *fakeSMLUDriver) hasInstance(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.instances[name]
	return ok
}

func seedMarker(t *testing.T, uid, ctr, card, instance string, profileID int32, cores int, mem int64) {
	t.Helper()
	require.NoError(t, writeMarker(markerPath(uid, ctr), smluMarker{
		PodUID: uid, Container: ctr, Card: card, Instance: instance, ProfileID: profileID, CoresPct: cores, MemMiB: mem,
	}))
}

func markerExists(uid, ctr string) bool {
	_, err := os.Stat(markerPath(uid, ctr))
	return err == nil
}

func Test_smluSetFor(t *testing.T) {
	quota, mem := smluSetFor(25, 16384)
	assert.Equal(t, uint32(25), quota, "mluQuota is the compute percent directly")
	assert.Equal(t, uint64(16384)<<20, mem, "memorySize is bytes (MiB << 20)")
	assert.Equal(t, uint64(17179869184), mem)
}

func Test_instanceNameEncodeDecode(t *testing.T) {
	const maxUID = "3f8b1c2d-4e5a-6b7c-8d9e-0f1a2b3c4d5e" // 36 chars
	longCtr := strings.Repeat("c", 63)                    // max k8s container name length

	name := encodeInstanceName(maxUID, longCtr)
	assert.LessOrEqual(t, len(name)+1, 100, "encoded name + NUL must fit the cnDev 100-byte buffer")

	uid, ok := decodeInstanceUID(name)
	require.True(t, ok)
	assert.Equal(t, maxUID, uid, "the pod UID round-trips out of the name")

	// Two containers on the same pod encode to distinct, non-colliding names.
	other := encodeInstanceName(maxUID, "sidecar")
	assert.NotEqual(t, name, other)

	// A name that is not operator-encoded is not decodable (foreign instance).
	for _, foreign := range []string{"", "someone-elses", "gpustack:", "gpustack::hash"} {
		_, ok := decodeInstanceUID(foreign)
		assert.Falsef(t, ok, "foreign name %q must not decode", foreign)
	}
}

func Test_marker_roundtrip_and_failClosed(t *testing.T) {
	redirectSoftSliceDirs(t)

	path := markerPath("uid-rt", "train")
	want := smluMarker{PodUID: "uid-rt", Container: "train", Card: testCard0, Instance: "gpustack-x", ProfileID: 2, CoresPct: 25, MemMiB: 16384}
	require.NoError(t, writeMarker(path, want))
	got, err := parseMarker(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	_, err = parseMarker(path)
	require.Error(t, err)

	require.NoError(t, os.WriteFile(path, []byte(`{"container":"train"}`), 0o600))
	_, err = parseMarker(path)
	require.Error(t, err)
}

// A corrupt marker for one pod must not block reserving an instance for another pod.
func Test_reserveInstance_corruptOtherMarkerDoesNotBlock(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()

	other := markerPath("uid-corrupt", "train")
	require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o777))
	require.NoError(t, os.WriteFile(other, []byte("{garbage"), 0o600))

	inst, err := reserveInstance(d, "uid-ok", "train", testCard0, 25, 16384)
	require.NoError(t, err)
	assert.NotEmpty(t, inst.name)
	assert.True(t, d.ensured[testCard0], "sMLU mode ensured on first reserve")
}

func Test_reserveInstance_idempotentReuseAndFailClosed(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()

	r0, err := reserveInstance(d, "uid-a", "train", testCard0, 25, 16384)
	require.NoError(t, err)
	assert.Equal(t, 1, d.instanceCreates)
	assert.NotEmpty(t, r0.devNode)

	// Re-Allocate the same pod+container+card+cores+mem: reuse, no new instance.
	r1, err := reserveInstance(d, "uid-a", "train", testCard0, 25, 16384)
	require.NoError(t, err)
	assert.Equal(t, r0.name, r1.name)
	assert.Equal(t, 1, d.instanceCreates, "reuse must not create a second instance")

	// A changed parameter (immutable requests) fails closed rather than mutating.
	_, err = reserveInstance(d, "uid-a", "train", testCard0, 50, 16384)
	require.Error(t, err)
}

// Two instances with an identical quota on one card share a single profile; a differing
// quota cuts a new profile.
func Test_reserveInstance_exactMatchProfileReuse(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()

	_, err := reserveInstance(d, "pod-a", "train", testCard0, 25, 16384)
	require.NoError(t, err)
	_, err = reserveInstance(d, "pod-b", "train", testCard0, 25, 16384)
	require.NoError(t, err)
	assert.Equal(t, 1, d.profileCreates, "identical quota reuses the profile")
	assert.Equal(t, 2, d.instanceCreates)

	// A different quota (here a smaller memory size) on the same card creates a second profile.
	_, err = reserveInstance(d, "pod-c", "train", testCard0, 25, 8192)
	require.NoError(t, err)
	assert.Equal(t, 2, d.profileCreates)

	// The same quota on a DIFFERENT card creates its own profile (reuse is per card).
	_, err = reserveInstance(d, "pod-d", "train", testCard1, 25, 16384)
	require.NoError(t, err)
	assert.Equal(t, 3, d.profileCreates)
}

// A marker that survives after its instance is gone (create crash / external teardown)
// fails closed on re-Allocate rather than silently re-creating under the reused marker.
func Test_reserveInstance_reuseMissingInstanceFailsClosed(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()

	inst, err := reserveInstance(d, "pod-a", "train", testCard0, 25, 16384)
	require.NoError(t, err)
	require.NoError(t, d.DestroyInstance(inst.card, inst.name)) // instance vanishes, marker remains

	_, err = reserveInstance(d, "pod-a", "train", testCard0, 25, 16384)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing instance")
}

// A failed instance create rolls back the freshly created profile.
func Test_reserveInstance_createFailureRollsBackProfile(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	d.failCard[testCard0] = true

	_, err := reserveInstance(d, "pod-a", "train", testCard0, 25, 16384)
	require.Error(t, err)
	assert.Len(t, d.destroyedProfs, 1, "the profile created for a failed instance is rolled back")
	assert.False(t, markerExists("pod-a", "train"), "no marker on a failed reserve")
}

func Test_reclaim_deadPodDestroysAfterDebounce(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	inst, err := reserveInstance(d, "pod-dead", "train", testCard0, 25, 16384)
	require.NoError(t, err)

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())

	r.reconcile(nil)
	r.reconcile(nil)
	assert.True(t, d.hasInstance(inst.name), "instance must survive the debounce window")
	assert.True(t, markerExists("pod-dead", "train"))

	r.reconcile(nil)
	assert.False(t, d.hasInstance(inst.name), "instance destroyed after 3 misses")
	assert.False(t, markerExists("pod-dead", "train"), "marker removed")
	assert.Len(t, d.destroyedProfs, 1, "its now-unreferenced profile is GC'd")
}

// A live pod's instance is never touched, and a live snapshot resets the miss streak.
func Test_reclaim_livePodPreserved(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	inst, err := reserveInstance(d, "pod-live", "train", testCard0, 25, 16384)
	require.NoError(t, err)

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
	r.reconcile(nil)
	r.reconcile(nil)
	r.reconcile([]string{"pod-live"})
	r.reconcile(nil)
	r.reconcile(nil)
	assert.True(t, d.hasInstance(inst.name), "a reset streak must not reclaim early")
	assert.True(t, markerExists("pod-live", "train"))
}

// A marker whose instance is already gone (external teardown) is cleaned once its pod is
// dead: DestroyInstance is a no-op, the marker file is removed.
func Test_reclaim_instanceLessMarker(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	seedMarker(t, "pod-dead", "train", testCard0, encodeInstanceName("pod-dead", "train"), 0, 25, 16384)

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
	r.reconcile(nil)
	r.reconcile(nil)
	r.reconcile(nil)
	assert.False(t, markerExists("pod-dead", "train"), "instance-less marker cleaned")
}

func Test_reclaim_markerLessOrphan(t *testing.T) {
	t.Run("dead UID destroyed", func(t *testing.T) {
		redirectSoftSliceDirs(t)
		d := newFakeDriver()
		name := encodeInstanceName("pod-dead", "train")
		d.seedInstance(smluInstance{card: testCard0, name: name, profileID: 0, coresPct: 25, memMiB: 16384})

		r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
		r.reconcile(nil)
		r.reconcile(nil)
		assert.True(t, d.hasInstance(name), "debounced")
		r.reconcile(nil)
		assert.False(t, d.hasInstance(name), "dead-UID orphan destroyed after 3 misses")
	})

	t.Run("live UID left intact", func(t *testing.T) {
		redirectSoftSliceDirs(t)
		d := newFakeDriver()
		name := encodeInstanceName("pod-live", "train")
		d.seedInstance(smluInstance{card: testCard0, name: name, profileID: 0, coresPct: 25, memMiB: 16384})

		r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
		for i := 0; i < 5; i++ {
			r.reconcile([]string{"pod-live"})
		}
		assert.True(t, d.hasInstance(name), "a live-UID marker-less instance is never destroyed")
	})

	t.Run("foreign name left alone", func(t *testing.T) {
		redirectSoftSliceDirs(t)
		d := newFakeDriver()
		d.seedInstance(smluInstance{card: testCard0, name: "someone-elses-instance", profileID: 0, coresPct: 25, memMiB: 16384})

		r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
		for i := 0; i < 5; i++ {
			r.reconcile(nil)
		}
		assert.True(t, d.hasInstance("someone-elses-instance"), "a non-operator instance is never touched")
	})
}

// A profile with no instance (a create that crashed between the profile and its instance)
// is reclaimed by the once-per-pass profile sweep, even though no marker or instance ever
// referenced it.
func Test_reclaim_orphanProfileGCd(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	id, err := d.CreateProfile(testCard0, 25, 16384) // orphan: profile created, no instance
	require.NoError(t, err)

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
	r.reconcile(nil)

	assert.Contains(t, d.destroyedProfs, profileKey{card: testCard0, profileID: id},
		"an instance-less orphan profile is swept")
}

// A profile-list error skips the sweep rather than destroying a profile on a partial view.
func Test_reclaim_profileListErrorSkipsSweep(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	id, err := d.CreateProfile(testCard0, 25, 16384)
	require.NoError(t, err)
	d.failProfileList = true

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
	r.reconcile(nil)

	assert.NotContains(t, d.destroyedProfs, profileKey{card: testCard0, profileID: id},
		"a profile-list error skips the sweep; nothing is destroyed on partial data")
}

// A driver list error skips the reconcile pass rather than reclaiming on partial data.
func Test_reclaim_listErrorSkipsPass(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	name := encodeInstanceName("pod-dead", "train")
	d.seedInstance(smluInstance{card: testCard0, name: name, profileID: 0, coresPct: 25, memMiB: 16384})
	d.failList = true

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
	for i := 0; i < 5; i++ {
		r.reconcile(nil)
	}
	assert.True(t, d.hasInstance(name), "a driver list error skips the pass; nothing is reclaimed")
}

// A profile shared by a live instance is never destroyed; it is GC'd only once its last
// instance is reclaimed.
func Test_reclaim_neverDestroysSharedProfile(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	_, err := reserveInstance(d, "pod-a", "train", testCard0, 25, 16384)
	require.NoError(t, err)
	_, err = reserveInstance(d, "pod-b", "train", testCard0, 25, 16384) // shares profile 0
	require.NoError(t, err)

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())

	// pod-a dies while pod-b stays live: a's instance goes, the shared profile stays.
	r.reconcile([]string{"pod-b"})
	r.reconcile([]string{"pod-b"})
	r.reconcile([]string{"pod-b"})
	assert.Empty(t, d.destroyedProfs, "a profile a live instance references is not destroyed")

	// pod-b dies too: the now-unreferenced profile is destroyed once.
	r.reconcile(nil)
	r.reconcile(nil)
	r.reconcile(nil)
	assert.Len(t, d.destroyedProfs, 1, "the profile is destroyed once no instance references it")
}

// Reclaiming one container of a dead multi-container pod must remove only the specific
// marker files (never RemoveAll a dir), and clean the pod dir only when empty.
func Test_reclaim_multiContainerPod(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	i1, err := reserveInstance(d, "pod-dead", "c1", testCard0, 25, 16384)
	require.NoError(t, err)
	i2, err := reserveInstance(d, "pod-dead", "c2", testCard0, 25, 16384)
	require.NoError(t, err)

	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())
	r.reconcile(nil)
	r.reconcile(nil)
	r.reconcile(nil)

	assert.False(t, markerExists("pod-dead", "c1"))
	assert.False(t, markerExists("pod-dead", "c2"))
	assert.False(t, d.hasInstance(i1.name))
	assert.False(t, d.hasInstance(i2.name))
	_, statErr := os.Stat(filepath.Join(deviceplugin.OperatorPodsDir, "pod-dead"))
	assert.True(t, os.IsNotExist(statErr), "empty pod dir removed")
}

func Test_removeIfEmpty_leavesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600))
	removeIfEmpty(dir)
	_, err := os.Stat(dir)
	assert.NoError(t, err, "a non-empty dir must survive")
}

// Concurrent Allocate + reclaim must be race-free.
func Test_reserveInstance_reclaim_race(t *testing.T) {
	redirectSoftSliceDirs(t)
	d := newFakeDriver()
	r := newReclaimer(d, deviceplugin.OperatorPodsDir, logr.Discard())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = reserveInstance(d,
				fmt.Sprintf("pod-%d", i), "train", testCard0, 25, 16384)
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

	// Every surviving instance has a distinct name (allocMu serializes the create cycle).
	seen := map[string]bool{}
	list, _ := d.ListInstances()
	for _, inst := range list {
		assert.Falsef(t, seen[inst.name], "instance %q double-created", inst.name)
		seen[inst.name] = true
	}
}
