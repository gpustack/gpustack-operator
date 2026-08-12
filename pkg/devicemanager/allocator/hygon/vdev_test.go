package hygon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bdfA = "0000:3d:00.0"
	bdfB = "0000:3e:00.0"
)

func selfPathFor(root, podUID, ctr string, i int) string {
	return filepath.Join(root, podUID, "c-"+ctr, "etc/vdev/docker", fmt.Sprintf("vdev%d.conf", i))
}

// seedConf writes a vdev.conf on disk to simulate a live slice the scanner must see.
func seedConf(t *testing.T, root, podUID, ctr string, i int, c vdevConf) string {
	t.Helper()
	p := selfPathFor(root, podUID, ctr, i)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o777))
	require.NoError(t, os.WriteFile(p, []byte(c.render()), 0o644))
	return p
}

func TestSliceCUCount(t *testing.T) {
	cases := []struct {
		coresPct int
		cores    uint32
		want     int
	}{
		{coresPct: 25, cores: 64, want: 16},  // exact
		{coresPct: 1, cores: 64, want: 1},    // ceil(0.64) -> a positive percent never yields 0
		{coresPct: 33, cores: 60, want: 20},  // ceil(19.8)
		{coresPct: 100, cores: 64, want: 64}, // whole card
		{coresPct: 100, cores: 128, want: 128},
		{coresPct: 150, cores: 64, want: 64}, // capped at the card total
		{coresPct: 0, cores: 64, want: 0},    // non-positive percent
		{coresPct: 50, cores: 0, want: 0},    // no compute units
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, sliceCUCount(c.coresPct, c.cores), "cores%%=%d cores=%d", c.coresPct, c.cores)
	}
}

func TestPackCUMask(t *testing.T) {
	full64 := fullMask(64)
	cases := []struct {
		name    string
		used    cuMask
		cuCount int
		cores   int
		wantLo  uint64
		wantHi  uint64
		wantErr bool
	}{
		{name: "lowest 16 of empty card", cuCount: 16, cores: 64, wantLo: 0x000000000000ffff},
		{name: "next 16 avoid used", used: cuMask{lo: 0x000000000000ffff}, cuCount: 16, cores: 64, wantLo: 0x00000000ffff0000},
		{name: "boundary CU 63", used: cuMask{lo: 0x7fffffffffffffff}, cuCount: 1, cores: 64, wantLo: 0x8000000000000000},
		{name: "spill to high word CU 64", used: full64, cuCount: 1, cores: 128, wantHi: 0x1}, // only the newly picked bit
		{name: "boundary CU 127 whole 128", cuCount: 128, cores: 128, wantLo: 0xffffffffffffffff, wantHi: 0xffffffffffffffff},
		{name: "insufficient free", used: cuMask{lo: 0x000000000000ffff}, cuCount: 64, cores: 64, wantErr: true},
		{name: "used references CU beyond card", used: cuMask{lo: 0x0000010000000000}, cuCount: 1, cores: 32, wantErr: true},
		{name: "cores exceed 128-bit mask width", cuCount: 1, cores: 130, wantErr: true},
	}
	for _, c := range cases {
		got, err := packCUMask(c.used, c.cuCount, c.cores)
		if c.wantErr {
			assert.Errorf(t, err, c.name)
			continue
		}
		require.NoErrorf(t, err, c.name)
		assert.Equalf(t, c.wantLo, got.lo, "%s lo", c.name)
		assert.Equalf(t, c.wantHi, got.hi, "%s hi", c.name)
		// The packed mask is disjoint from the used set.
		assert.Zerof(t, got.lo&c.used.lo, "%s lo overlap", c.name)
		assert.Zerof(t, got.hi&c.used.hi, "%s hi overlap", c.name)
	}
}

// The exact rendered bytes lock the vdev.conf schema the DTK/hyhal runtime reads, and a
// render->parse round-trip proves an atomically published file is complete and readable.
func TestVdevConfRenderRoundTrip(t *testing.T) {
	c := vdevConf{
		pciBusID: bdfA,
		mask:     cuMask{lo: 0x000000000000ffff},
		cuCount:  16,
		memMib:   24576,
		deviceID: 0,
		vdevID:   3,
		pipeID:   2,
	}
	want := "PciBusId: 0000:3d:00.0\n" +
		"cu_mask: 0x000000000000ffff\n" +
		"cu_mask: 0x0000000000000000\n" +
		"cu_count: 16\n" +
		"mem: 24576 MiB\n" +
		"device_id: 0\n" +
		"vdev_id: 3\n" +
		"pipe_id: 2\n" +
		"enable: 1\n"
	assert.Equal(t, want, c.render())

	root := t.TempDir()
	p := selfPathFor(root, "pod-x", "train", 0)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o777))
	require.NoError(t, os.WriteFile(p, []byte(c.render()), 0o644))
	got, err := parseVdevConf(p)
	require.NoError(t, err)
	assert.Equal(t, c, got)
}

// A first slice, a second slice on the same accelerator, and a third on a different accelerator exercise
// the three pools together: node-wide vdev_id, per-accelerator pipe_id, and per-accelerator CU bits.
func TestAllocateVdev_PoolsAcrossCards(t *testing.T) {
	root := t.TempDir()

	c0, err := allocateVdev(root, selfPathFor(root, "pod-0", "train", 0), bdfA, 64, 25, 24576, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x000000000000ffff), c0.mask.lo)
	assert.Equal(t, 0, c0.vdevID)
	assert.Equal(t, 0, c0.pipeID)
	assert.Equal(t, 16, c0.cuCount)

	// Same accelerator, different pod: next 16 CU bits, next node vdev id, next per-accelerator pipe id.
	c1, err := allocateVdev(root, selfPathFor(root, "pod-1", "train", 0), bdfA, 64, 25, 24576, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x00000000ffff0000), c1.mask.lo)
	assert.Equal(t, 1, c1.vdevID)
	assert.Equal(t, 1, c1.pipeID)

	// Different accelerator: CU bits and pipe id reset, but the node-wide vdev id keeps climbing.
	c2, err := allocateVdev(root, selfPathFor(root, "pod-2", "train", 0), bdfB, 64, 25, 24576, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x000000000000ffff), c2.mask.lo)
	assert.Equal(t, 2, c2.vdevID, "vdev id is node-wide")
	assert.Equal(t, 0, c2.pipeID, "pipe id is per-card")
}

// Different accelerator models advertise different CU counts; cuCount tracks each accelerator's total
// and a whole-accelerator slice fills every CU bit across both mask words.
func TestAllocateVdev_VariedCardCU(t *testing.T) {
	root := t.TempDir()

	half, err := allocateVdev(root, selfPathFor(root, "pod-a", "train", 0), bdfA, 120, 50, 24576, 0)
	require.NoError(t, err)
	assert.Equal(t, 60, half.cuCount) // ceil(50% * 120)
	assert.Equal(t, 0, half.deviceID)

	// The second accelerator of a multi-accelerator container, taken whole: the container-local index
	// is 1.
	whole, err := allocateVdev(root, selfPathFor(root, "pod-a", "train", 1), bdfB, 128, 100, 65536, 1)
	require.NoError(t, err)
	assert.Equal(t, 128, whole.cuCount)
	assert.Equal(t, 1, whole.deviceID)
	assert.Equal(t, uint64(0xffffffffffffffff), whole.mask.lo)
	assert.Equal(t, uint64(0xffffffffffffffff), whole.mask.hi)
}

// Re-allocating the same self path for the same accelerator reuses the on-disk record.
func TestAllocateVdev_Idempotent(t *testing.T) {
	root := t.TempDir()
	self := selfPathFor(root, "pod-0", "train", 0)

	first, err := allocateVdev(root, self, bdfA, 64, 25, 24576, 0)
	require.NoError(t, err)
	again, err := allocateVdev(root, self, bdfA, 64, 25, 24576, 0)
	require.NoError(t, err)
	assert.Equal(t, first, again)
}

// The used sets are reconstructed from pre-existing on-disk confs (restart-surviving), and
// deleting a conf frees its slot for the lowest-hole reuse on the next allocate.
func TestAllocateVdev_RestartAndHoleReuse(t *testing.T) {
	root := t.TempDir()
	seedConf(t, root, "old-0", "train", 0, vdevConf{pciBusID: bdfA, mask: cuMask{lo: 0x000000000000ffff}, cuCount: 16, memMib: 24576, vdevID: 0, pipeID: 0})
	seedConf(t, root, "old-1", "train", 0, vdevConf{pciBusID: bdfA, mask: cuMask{lo: 0x00000000ffff0000}, cuCount: 16, memMib: 24576, vdevID: 1, pipeID: 1})

	// A fresh allocate reconstructs used bits/ids from disk and avoids them.
	c, err := allocateVdev(root, selfPathFor(root, "new-0", "train", 0), bdfA, 64, 25, 24576, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x0000ffff00000000), c.mask.lo)
	assert.Equal(t, 2, c.vdevID)
	assert.Equal(t, 2, c.pipeID)

	// Free the lowest slot; the next allocate reuses vdev_id 0 / pipe_id 0 / CU bits 0-15.
	require.NoError(t, os.RemoveAll(filepath.Join(root, "old-0")))
	c2, err := allocateVdev(root, selfPathFor(root, "new-1", "train", 0), bdfA, 64, 25, 24576, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, c2.vdevID)
	assert.Equal(t, 0, c2.pipeID)
	assert.Equal(t, uint64(0x000000000000ffff), c2.mask.lo)
}

// Concurrent allocates on one accelerator must serialize on allocMu: distinct vdev/pipe ids and
// pairwise-disjoint CU masks that tile the accelerator. Run under -race.
func TestAllocateVdev_ConcurrentDisjoint(t *testing.T) {
	root := t.TempDir()
	const n = 4 // four 16-CU slices exactly tile a 64-CU card

	confs := make([]vdevConf, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			confs[i], errs[i] = allocateVdev(root, selfPathFor(root, fmt.Sprintf("pod-%d", i), "train", 0), bdfA, 64, 25, 24576, 0)
		}(i)
	}
	wg.Wait()

	var union cuMask
	vdevIDs, pipeIDs := map[int]bool{}, map[int]bool{}
	total := 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.Falsef(t, vdevIDs[confs[i].vdevID], "duplicate vdev_id %d", confs[i].vdevID)
		assert.Falsef(t, pipeIDs[confs[i].pipeID], "duplicate pipe_id %d", confs[i].pipeID)
		vdevIDs[confs[i].vdevID] = true
		pipeIDs[confs[i].pipeID] = true
		assert.Zerof(t, union.lo&confs[i].mask.lo, "overlapping CU mask for slice %d", i)
		union = union.or(confs[i].mask)
		total += confs[i].cuCount
	}
	assert.Equal(t, 64, total, "the four slices tile all 64 CUs")
	assert.Equal(t, uint64(0xffffffffffffffff), union.lo)
}

func TestParseVdevConf_FailClosed(t *testing.T) {
	valid := vdevConf{pciBusID: bdfA, mask: cuMask{lo: 0xffff}, cuCount: 16, memMib: 24576, vdevID: 0, pipeID: 0}.render()
	cases := []struct {
		name string
		body string
	}{
		{"malformed line", "PciBusId 0000:3d:00.0\n"},
		{"bad cu_mask hex", "PciBusId: " + bdfA + "\ncu_mask: 0xZZ\ncu_mask: 0x0\ncu_count: 1\nvdev_id: 0\npipe_id: 0\n"},
		{"only one cu_mask", "PciBusId: " + bdfA + "\ncu_mask: 0x1\ncu_count: 1\nvdev_id: 0\npipe_id: 0\n"},
		{"missing cu_count", "PciBusId: " + bdfA + "\ncu_mask: 0x1\ncu_mask: 0x0\nvdev_id: 0\npipe_id: 0\n"},
		{"missing pci bus id", "cu_mask: 0x1\ncu_mask: 0x0\ncu_count: 1\nvdev_id: 0\npipe_id: 0\n"},
		{"duplicate vdev_id", valid + "vdev_id: 5\n"},
		{"vdev_id out of range", "PciBusId: " + bdfA + "\ncu_mask: 0x1\ncu_mask: 0x0\ncu_count: 1\nvdev_id: 200\npipe_id: 0\n"},
		{"pipe_id out of range", "PciBusId: " + bdfA + "\ncu_mask: 0x1\ncu_mask: 0x0\ncu_count: 1\nvdev_id: 0\npipe_id: 20\n"},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "vdev0.conf")
		require.NoError(t, os.WriteFile(p, []byte(c.body), 0o644))
		_, err := parseVdevConf(p)
		assert.Errorf(t, err, c.name)
	}
}

// A single corrupt conf in the pods dir fails the whole scan (and thus the allocate),
// rather than being skipped and silently freeing a live slot.
func TestScanVdevConfs_FailClosedOnCorruptNeighbor(t *testing.T) {
	root := t.TempDir()
	seedConf(t, root, "good-0", "train", 0, vdevConf{pciBusID: bdfA, mask: cuMask{lo: 0xffff}, cuCount: 16, memMib: 24576, vdevID: 0, pipeID: 0})
	corrupt := selfPathFor(root, "bad-0", "train", 0)
	require.NoError(t, os.MkdirAll(filepath.Dir(corrupt), 0o777))
	require.NoError(t, os.WriteFile(corrupt, []byte("garbage\n"), 0o644))

	_, err := allocateVdev(root, selfPathFor(root, "new-0", "train", 0), bdfA, 64, 25, 24576, 0)
	require.Error(t, err)
}

// An unreadable pod directory must fail the scan closed (a live conf under it would
// otherwise silently vanish and its slot be double-booked), so the scan uses a walk that
// surfaces directory read errors rather than filepath.Glob, which swallows them.
func TestScanVdevConfs_FailClosedOnUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits; cannot simulate an unreadable dir")
	}
	root := t.TempDir()
	seedConf(t, root, "good-0", "train", 0, vdevConf{pciBusID: bdfA, mask: cuMask{lo: 0xffff}, cuCount: 16, memMib: 24576, vdevID: 0, pipeID: 0})

	blocked := filepath.Join(root, "blocked-pod")
	require.NoError(t, os.MkdirAll(filepath.Join(blocked, "c-train/etc/vdev/docker"), 0o777))
	require.NoError(t, os.Chmod(blocked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o777) }) // let t.TempDir cleanup remove it

	_, err := allocateVdev(root, selfPathFor(root, "new-0", "train", 0), bdfA, 64, 25, 24576, 0)
	require.Error(t, err)
}

// A sliced request that resolves to zero compute units (a cores=0 accelerator, or a non-positive
// compute percent) must fail closed rather than publish a slot-consuming empty conf.
func TestAllocateVdev_ZeroComputeRejected(t *testing.T) {
	root := t.TempDir()
	_, err := allocateVdev(root, selfPathFor(root, "pod-0", "train", 0), bdfA, 64, 0, 24576, 0)
	require.Error(t, err)

	_, err = allocateVdev(root, selfPathFor(root, "pod-1", "train", 0), bdfA, 0, 100, 24576, 0)
	require.Error(t, err)
}

func TestAllocateVdev_Exhaustion(t *testing.T) {
	t.Run("pipe id at 20", func(t *testing.T) {
		root := t.TempDir()
		// 20 single-CU slices on one accelerator fill the pipe pool without exhausting CUs.
		for i := 0; i < maxPipeID; i++ {
			seedConf(t, root, fmt.Sprintf("pod-%d", i), "train", 0,
				vdevConf{pciBusID: bdfA, mask: cuMask{lo: uint64(1) << uint(i)}, cuCount: 1, memMib: 1024, vdevID: i, pipeID: i})
		}
		_, err := allocateVdev(root, selfPathFor(root, "over", "train", 0), bdfA, 64, 1, 1024, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pipe_id")
	})

	t.Run("insufficient free CUs", func(t *testing.T) {
		root := t.TempDir()
		// Four 16-CU slices fill a 64-CU accelerator; a fifth cannot pack 16 more.
		for i := 0; i < 4; i++ {
			seedConf(t, root, fmt.Sprintf("pod-%d", i), "train", 0,
				vdevConf{pciBusID: bdfA, mask: cuMask{lo: uint64(0xffff) << uint(16*i)}, cuCount: 16, memMib: 24576, vdevID: i, pipeID: i})
		}
		_, err := allocateVdev(root, selfPathFor(root, "over", "train", 0), bdfA, 64, 25, 24576, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compute units")
	})

	t.Run("vdev id at 200", func(t *testing.T) {
		root := t.TempDir()
		// 200 single-CU slices spread across many accelerators fill the node vdev pool.
		for i := 0; i < maxVdevID; i++ {
			bdf := fmt.Sprintf("0000:%02x:00.0", i)
			seedConf(t, root, fmt.Sprintf("pod-%d", i), "train", 0,
				vdevConf{pciBusID: bdf, mask: cuMask{lo: 0x1}, cuCount: 1, memMib: 1024, vdevID: i, pipeID: 0})
		}
		_, err := allocateVdev(root, selfPathFor(root, "over", "train", 0), "0000:ff:00.0", 64, 1, 1024, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vdev_id")
	})
}
