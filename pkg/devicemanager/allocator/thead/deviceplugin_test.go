package thead

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// The capability minor numbers of the vendor's own isolation example: a card's first partition sits at
// 1280 and its compute instance at 1281, neither derivable from the instance ids nor from the card.
// The fixtures use them so a test would fail if the minors were ever computed instead of read. Hardware
// measured a first instance at 229632 with its compute instance at 229633 — a different magnitude and
// exactly as underivable, which is the only property the code relies on, so either pair serves.
const (
	testCapGiMinor = uint32(1280)
	testCapCiMinor = uint32(1281)
)

// testPPUIndex0 and testPPUIndex1 are the accelerator indexes of the two fixture cards — the ordinals
// the vendor names their device nodes after (/dev/alixpu_ppu<ordinal>) and keys their procfs capability
// subtrees by — while testPPUMinor0 and testPPUMinor1 are the kernel minor numbers those nodes carry,
// which are the numbers the detector records. Both halves of a pair are stated rather than derived from
// one another, because nothing in the allocator may compute either from the other and a fixture that
// computed one would prove nothing.
//
// The pairs reproduce what was measured on one 16-card host running one driver version, where the node at
// ordinal N carried kernel minor N+1 — the vendor's shared control node holds minor 0 of the same
// character-device major as the per-card nodes, which is why the cards start one along. That relation is
// an observation about that host, not a rule: neither these tests nor the code they exercise would change
// at another offset, or at none, because what is asserted is only that the node an ordinal names carries
// the minor its accelerator's record states.
//
// Reproducing it is what makes the fixture sharp. Card 1 is card 0's decoy, and both failure modes seen on
// that host fall out of the one pair: card 0's RECORDED minor, 15, names card 1's real node, so a path
// built from the record lands on the neighboring card silently; and card 1's record, 16, names an
// alixpu_ppu16 that exists on no 16-card host at all, so that card is refused outright.
const (
	testPPUIndex0 = uint32(14)
	testPPUMinor0 = uint32(15)
	testPPUIndex1 = uint32(15)
	testPPUMinor1 = uint32(16)
)

// notCharDevice marks a fixture path that exists but is not a character device.
const notCharDevice = "notchar"

// redirectNodeRoots points the vendor device-node root, the driver procfs root and the pod work root at
// temporary directories, and makes the temporary device nodes readable as character devices, so a test
// never touches the host's /dev, /proc or pods tree. It returns the device and pods roots.
func redirectNodeRoots(t *testing.T) (devDir, podsDir string) {
	t.Helper()

	root := t.TempDir()
	origDev, origProc, origPods := hostDevDir, hostProcDriverDir, deviceplugin.OperatorPodsDir
	origMinor := deviceNodeMinor
	hostDevDir = filepath.Join(root, "dev")
	hostProcDriverDir = filepath.Join(root, "proc", "driver")
	deviceplugin.OperatorPodsDir = filepath.Join(root, "pods")
	deviceNodeMinor = fakeDeviceNodeMinor
	t.Cleanup(func() {
		hostDevDir, hostProcDriverDir, deviceplugin.OperatorPodsDir = origDev, origProc, origPods
		deviceNodeMinor = origMinor
	})

	return hostDevDir, deviceplugin.OperatorPodsDir
}

// fakeDeviceNodeMinor reads a temporary tree as if it held character devices: a node file's contents
// carry its minor number, and the contents "notchar" mark a path that exists but is not a character
// device. Existence stays a real filesystem property, so removing a node file is a genuine removal —
// only the one fact a temporary directory cannot hold without root is substituted.
func fakeDeviceNodeMinor(path string) (uint32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	body := strings.TrimSpace(string(data))
	if body == notCharDevice {
		return 0, fmt.Errorf("%q is not a character device", path)
	}
	minor, perr := strconv.ParseUint(body, 10, 32)
	if perr != nil {
		return 0, fmt.Errorf("fixture node %q: %w", path, perr)
	}
	return uint32(minor), nil
}

// writeFixtureFile writes one fixture file, creating its parents.
func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, osx.MkdirAll(filepath.Dir(path), 0o777))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// writeCharNode writes a fixture device node carrying the given minor number.
func writeCharNode(t *testing.T, path string, minor uint32) {
	t.Helper()
	writeFixtureFile(t, path, strconv.FormatUint(uint64(minor), 10))
}

// writeAccessFile writes a procfs capability access file in the driver's own shape, so the minor is
// read out of a record rather than out of the file name.
func writeAccessFile(t *testing.T, path string, minor uint32) {
	t.Helper()
	writeFixtureFile(t, path, fmt.Sprintf("DeviceFileMinor: %d\nDeviceFileMode: 292\nDeviceFileModify: 1\n", minor))
}

// nodeFixture is the vendor node tree one partition on one card needs. A case builds it and then
// removes or corrupts exactly one member. cardIndex is the card's accelerator index — the ordinal its
// device node is NAMED after and its procfs capability subtree is keyed by — and cardMinor the kernel
// minor number that node carries, which is the number the detector records for the card.
type nodeFixture struct {
	cardIndex uint32
	cardMinor uint32
	giID      uint32
	ciID      uint32
	giMinor   uint32
	ciMinor   uint32
}

// write publishes the whole node set: the two shared control nodes, the card's node under the name its
// ordinal gives it and carrying the minor the detector recorded, both capability nodes, and both procfs
// access files.
func (f nodeFixture) write(t *testing.T) {
	t.Helper()
	for _, path := range sharedControlNodePaths() {
		writeCharNode(t, path, 0)
	}
	writeCharNode(t, cardNodePath(f.cardIndex), f.cardMinor)
	writeCharNode(t, capNodePath(f.giMinor), f.giMinor)
	writeCharNode(t, capNodePath(f.ciMinor), f.ciMinor)
	writeAccessFile(t, giAccessPath(f.cardIndex, f.giID), f.giMinor)
	writeAccessFile(t, ciAccessPath(f.cardIndex, f.giID, f.ciID), f.ciMinor)
}

// createdFixture is the node tree of the partition the fake driver creates first (GPU instance 1,
// compute instance 1) on the first card, which sits at accelerator index testPPUIndex0.
func createdFixture() nodeFixture {
	return nodeFixture{
		cardIndex: testPPUIndex0, cardMinor: testPPUMinor0,
		giID: 1, ciID: 1, giMinor: testCapGiMinor, ciMinor: testCapCiMinor,
	}
}

// partitionDevices builds the shared partition fixture, giving each card the accelerator index its
// device node and procfs capability subtree are named after and recording the kernel minor number that
// node carries. The two are deliberately different for every card, so every expected path in these
// tests can only be right if it was built from the index, and a path built from the record lands on the
// neighboring card or on nothing at all.
func partitionDevices(uuids ...string) *workercore.Devices {
	indexes := []uint32{testPPUIndex0, testPPUIndex1}
	minors := []uint32{testPPUMinor0, testPPUMinor1}
	devs := theadDevices(testProfile, 1, 2, uuids...)
	for i := range devs.Spec.Groups {
		accels := devs.Spec.Groups[i].Accelerators
		for j := range accels {
			accels[j].Index = indexes[j]
			accels[j].PhysicalIndexes = []uint32{minors[j]}
		}
	}
	return devs
}

// allocatedOn builds the allocation map naming the given cards of the partition fixture's group.
func allocatedOn(uuids ...string) map[deviceplugin.Resource]int32 {
	allocated := make(map[deviceplugin.Resource]int32, len(uuids))
	for _, u := range uuids {
		allocated[deviceplugin.Resource{Group: "ppu", Device: u}] = 1
	}
	return allocated
}

// partitionPod is the Pod/container pair a partition is actuated for.
func partitionPod(uid string) (*core.Pod, *core.Container) {
	return &core.Pod{ObjectMeta: meta.ObjectMeta{Name: uid, UID: types.UID(uid)}}, &core.Container{Name: "c"}
}

// slicedPod is the Pod/container pair a logical slice is allocated for, carrying the ".sliced.*" limits
// the chain folds a request into. A zero figure means the request carries no limit of that kind at all,
// which is how the cases that turn on an absent dimension state themselves.
func slicedPod(uid string, coresPercent, memPercent, memMib int64) (*core.Pod, *core.Container) {
	limits := core.ResourceList{}
	for res, v := range map[core.ResourceName]int64{
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer):  coresPercent,
		nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer): memPercent,
		nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer):        memMib,
	} {
		if v > 0 {
			limits[res] = *resource.NewQuantity(v, resource.DecimalSI)
		}
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: uid, UID: types.UID(uid)},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name:      "c",
				Resources: core.ResourceRequirements{Limits: limits},
			}},
		},
	}
	return pod, &pod.Spec.Containers[0]
}

// slicedDevices is the sliced fixture: the same cards the whole-card fixture addresses — each carrying
// the ordinal that names its device node and the kernel minor that node holds — in one group whose VRAM
// every per-card memory budget is derived against.
func slicedDevices(memoryMib uint64, uuids ...string) *workercore.Devices {
	devs := partitionDevices(uuids...)
	devs.Spec.Groups[0].Memory = memoryMib
	return devs
}

// newSlicedServer builds the server that serves "<base>.sliced". It holds no partition driver, which is
// the shape New registers it in: a logical slice never touches the partitioning surface.
func newSlicedServer(t *testing.T) *server {
	t.Helper()
	s, ok := newServer(klog.Background(), workercore.DeviceAllocationModeSliced, nil).(*server)
	require.True(t, ok)
	return s
}

// writeCardNodes publishes the whole-card node set: the two shared control nodes, then each fixture
// card's node under the name its ordinal gives it, carrying the minor the detector recorded for it.
func writeCardNodes(t *testing.T) {
	t.Helper()
	for _, path := range sharedControlNodePaths() {
		writeCharNode(t, path, 0)
	}
	writeCharNode(t, cardNodePath(testPPUIndex0), testPPUMinor0)
	writeCharNode(t, cardNodePath(testPPUIndex1), testPPUMinor1)
}

// newPartitionedServer builds a partitioned server over the in-memory driver, with the card(s) seeded
// to offer the test profile.
func newPartitionedServer(cards ...string) (*server, *fakeMigDriver) {
	drv := newFakeMigDriver()
	for _, c := range cards {
		drv.seedCard(c)
	}
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         klog.Background(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModePartitioned,
		},
		mig: drv,
	}, drv
}

// devicePaths returns the host paths of a response's device specifications, in order.
func devicePaths(resp *deviceplugin.ContainerAllocateResponse) []string {
	paths := make([]string, 0, len(resp.Devices))
	for i := range resp.Devices {
		paths = append(paths, resp.Devices[i].HostPath)
	}
	return paths
}

// TestNew_ServerSet pins which families this vendor registers a device-plugin server for, that each
// control flag removes exactly its own, and that the one partition driver is shared with both servers
// that address partitions — the sliced server holds none, because a logical slice never touches the
// partitioning surface.
func TestNew_ServerSet(t *testing.T) {
	serversOf := func(a device.Allocator) []*server {
		agg, ok := a.(aggregated)
		require.True(t, ok)
		srvs := make([]*server, 0, len(agg.servers))
		for i := range agg.servers {
			srv, ok := agg.servers[i].(*server)
			require.True(t, ok)
			srvs = append(srvs, srv)
		}
		return srvs
	}

	cases := []struct {
		name string
		opts device.AllocatorOptions
		want []workercore.DeviceAllocationMode
		// wantDriven are the modes whose server must hold the partition driver.
		wantDriven []workercore.DeviceAllocationMode
	}{
		{
			name: "every family by default",
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantDriven: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-partitioned drops the partition server and builds no driver at all",
			opts: device.AllocatorOptions{NoPartitioned: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-shared drops only the shared server",
			opts: device.AllocatorOptions{NoShared: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantDriven: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-sliced drops only the sliced server",
			opts: device.AllocatorOptions{NoSliced: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantDriven: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srvs := serversOf(New(c.opts))

			modes := make([]workercore.DeviceAllocationMode, 0, len(srvs))
			var driven []workercore.DeviceAllocationMode
			drivers := make(map[migDriver]bool)
			for _, srv := range srvs {
				modes = append(modes, srv.AllocationMode)
				if srv.mig != nil {
					driven = append(driven, srv.AllocationMode)
					drivers[srv.mig] = true
				}
			}

			assert.Equal(t, c.want, modes)
			assert.Equal(t, c.wantDriven, driven, "only the servers addressing partitions hold the driver")
			// Both partition-addressing servers hold one and the same driver value, and a node serving
			// no partitioning holds none. That a second construction did not happen is not observable
			// here: the non-linux driver is an empty struct, so two instances of it compare equal.
			assert.LessOrEqual(t, len(drivers), 1, "the servers addressing partitions share one driver")
		})
	}

	// The gate the partition server is registered behind, stated at its source: a manufacturer with no
	// partition kind has no ".partitioned" resource name at all.
	assert.NotEmpty(t, nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned),
		"thead declares a partition kind, so it serves the family")
	assert.Empty(t, nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerMThreads, workercore.DeviceAllocationModePartitioned),
		"a manufacturer with no partition kind yields no resource name, so it registers no server")
}

// TestNew_ReclaimLoopFollowsThePartitionServer pins that the partition reclaim loop is gated on the
// server that creates the instances it frees, so it never runs with nothing to reclaim and never stops
// while partitions are still live.
func TestNew_ReclaimLoopFollowsThePartitionServer(t *testing.T) {
	withPartitions, ok := New(device.AllocatorOptions{NoShared: true}).(aggregated)
	require.True(t, ok)
	assert.True(t, withPartitions.partitioned, "the reclaim loop runs while the partition server does")

	withoutPartitions, ok := New(device.AllocatorOptions{NoPartitioned: true}).(aggregated)
	require.True(t, ok)
	assert.False(t, withoutPartitions.partitioned, "no partition server, nothing to reclaim")
}

// TestGetContainerAllocateResponse pins the whole-card node set — the set every Exclusive, Shared and
// non-partition Visibility allocation takes: the vendor's two shared control nodes once, then each
// allocated card's own node, which the vendor names after the card's ORDINAL — its accelerator index —
// and never after the kernel minor number that node carries. That the ordinal reaches the card the
// detector measured is proven, not assumed: the node's own minor number must be the minor the detector
// recorded for the accelerator. Every node is required, so a node the responder cannot produce fails the
// allocation instead of shortening the response: this vendor has no container-runtime hook, so an
// incomplete set that looks like success starts a container whose access to its card is silently missing.
func TestGetContainerAllocateResponse(t *testing.T) {
	cases := []struct {
		name string
		// allocated names the cards the allocation covers, out of the fixture's two.
		allocated []string
		// breaks removes or corrupts exactly one member of the written node set, or one card's
		// recorded minor number.
		breaks func(t *testing.T, devs *workercore.Devices)
		// want are the expected device-node names under the device root, in order.
		want    []string
		wantErr string
	}{
		{
			name:      "the allocated card's node is the one its ordinal names",
			allocated: []string{testPPUUUID0},
			want:      []string{"alixpu", "alixpu_ctl", "alixpu_ppu14"},
		},
		{
			name:      "every allocated card contributes its own node, the control nodes once",
			allocated: []string{testPPUUUID0, testPPUUUID1},
			want:      []string{"alixpu", "alixpu_ctl", "alixpu_ppu14", "alixpu_ppu15"},
		},
		{
			name:      "a card the detector recorded no minor number for is refused",
			allocated: []string{testPPUUUID0},
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil
			},
			wantErr: "no recorded minor number to prove its device node addresses it",
		},
		{
			// The desynchronized index: the accelerator index is a post-filter counter, so a card the
			// detector skipped mid-enumeration shifts every later index and the shifted index addresses
			// the wrong card. The node it names carries a minor the detector never recorded for this
			// accelerator, which is what makes the shift detectable without knowing any offset.
			name:      "the node the ordinal names carries a minor the record disagrees with",
			allocated: []string{testPPUUUID0},
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{testPPUMinor1}
			},
			wantErr: "carries minor 15, want 16",
		},
		{
			// The same disagreement from the other side: the /dev tree was renumbered under the record.
			name:      "the node the ordinal names was renumbered under the record",
			allocated: []string{testPPUUUID0},
			breaks: func(t *testing.T, _ *workercore.Devices) {
				writeCharNode(t, cardNodePath(testPPUIndex0), testPPUMinor1)
			},
			wantErr: "carries minor 16, want 15",
		},
		{
			name:      "an unallocated card missing its record does not fail the allocation",
			allocated: []string{testPPUUUID0},
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[1].PhysicalIndexes = nil
			},
			want: []string{"alixpu", "alixpu_ctl", "alixpu_ppu14"},
		},
		{
			name:      "the allocated card's node is missing",
			allocated: []string{testPPUUUID0},
			breaks: func(t *testing.T, _ *workercore.Devices) {
				removeFixture(t, cardNodePath(testPPUIndex0))
			},
			wantErr: "alixpu_ppu14\":",
		},
		{
			name:      "the allocated card's node exists but is not a character device",
			allocated: []string{testPPUUUID0},
			breaks: func(t *testing.T, _ *workercore.Devices) {
				writeFixtureFile(t, cardNodePath(testPPUIndex0), notCharDevice)
			},
			wantErr: "is not a character device",
		},
		{
			name:      "the shared control node is missing",
			allocated: []string{testPPUUUID0},
			breaks: func(t *testing.T, _ *workercore.Devices) {
				removeFixture(t, sharedControlNodePaths()[0])
			},
			wantErr: "alixpu\":",
		},
		{
			name:      "the shared control ioctl node is missing",
			allocated: []string{testPPUUUID0},
			breaks: func(t *testing.T, _ *workercore.Devices) {
				removeFixture(t, sharedControlNodePaths()[1])
			},
			wantErr: "alixpu_ctl\":",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devDir, _ := redirectNodeRoots(t)
			for _, path := range sharedControlNodePaths() {
				writeCharNode(t, path, 0)
			}
			// Each card's node under the name its ordinal gives it, carrying the kernel minor the
			// detector recorded for it. No separate decoy is needed: the second card's node is the very
			// node the first card's RECORDED minor names, so a response built from the record hands the
			// container the neighboring card and fails here rather than passing silently.
			writeCharNode(t, cardNodePath(testPPUIndex0), testPPUMinor0)
			writeCharNode(t, cardNodePath(testPPUIndex1), testPPUMinor1)

			devs := partitionDevices(testPPUUUID0, testPPUUUID1)
			if c.breaks != nil {
				c.breaks(t, devs)
			}

			s, ok := newServer(klog.Background(), workercore.DeviceAllocationModeExclusive, nil).(*server)
			require.True(t, ok)
			pod, ctr := partitionPod("pod-a")

			got, err := s.GetContainerAllocateResponse(
				context.Background(), pod, ctr, devs, allocatedOn(c.allocated...))
			if c.wantErr != "" {
				require.Error(t, err, "an incomplete device set must fail the allocation, never shorten the response")
				assert.Nil(t, got)
				assert.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)

			want := make([]string, 0, len(c.want))
			for _, name := range c.want {
				want = append(want, filepath.Join(devDir, name))
			}
			assert.Equal(t, want, devicePaths(got),
				"the node the card's ordinal names is injected, not the one its recorded minor would")
			assert.Empty(t, got.Envs, "the device nodes are the whole of the container's access")
			for i := range got.Devices {
				spec := got.Devices[i]
				assert.Equal(t, spec.HostPath, spec.ContainerPath, "a node is injected at its host path")
				assert.Equal(t, "rw", spec.Permissions)
			}
		})
	}
}

// TestGetSlicedContainerAllocateResponse pins the whole response a logically sliced container is
// given: the same node set every whole-card allocation takes, plus the quota figures the shim reads at
// load, the mounts that preload it, and the writable directory its usage region lives in. Every
// variable name and container path is written out rather than referred to through the constants the
// code builds them from, because a container's environment and mount table are a contract with a
// library that reads them by name, and an expectation stated through the same constant would hold
// whatever the name became.
func TestGetSlicedContainerAllocateResponse(t *testing.T) {
	devDir, _ := redirectNodeRoots(t)
	writeCardNodes(t)

	// A PPU-ZW810E-like card: 98304 MiB of VRAM, of which this container asks 25 %, and 10 % of the
	// card's compute — two independent dimensions rather than one ratio.
	devs := slicedDevices(98304, testPPUUUID0, testPPUUUID1)
	pod, ctr := slicedPod("pod-a", 10, 25, 0)

	got, err := newSlicedServer(t).GetContainerAllocateResponse(
		context.Background(), pod, ctr, devs, allocatedOn(testPPUUUID0))
	require.NoError(t, err)

	// The device set is the whole-card one, unchanged: the slice is enforced inside the container, so
	// it is handed the same nodes — the two shared control nodes and the node its card's ordinal names.
	assert.Equal(t, []string{
		filepath.Join(devDir, "alixpu"),
		filepath.Join(devDir, "alixpu_ctl"),
		filepath.Join(devDir, "alixpu_ppu14"),
	}, devicePaths(got), "a slice takes the same nodes a whole card does, no fewer")

	assert.Equal(t, map[string]string{
		"HGGC_DEVICE_SM_LIMIT":       "10",
		"HGGC_DEVICE_MEMORY_LIMIT_0": "24576", // 98304 MiB * 25 %, a bare MiB integer
		"HGGC_LEDGER_PATH":           "/var/run/vppu/ledger",
		"LIBHGGC_LOG_LEVEL":          "1",
	}, got.Envs)

	// The region's directory is created for the container to open the region in, world-writable
	// because the workload's user is not ours to predict.
	ledgerDir := filepath.Join(deviceplugin.PodWorkDir("pod-a", "c"), "run/vppu")
	info, statErr := os.Stat(ledgerDir)
	require.NoError(t, statErr, "the usage region's directory must exist before the container starts")
	assert.Equal(t, os.FileMode(0o777), info.Mode().Perm())

	libDir := filepath.Join(deviceplugin.OperatorLibDir, "thead")
	assert.Equal(t, []*deviceplugin.Mount{
		{ContainerPath: "/etc/ld.so.preload", HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
		{ContainerPath: "/usr/local/vppu/hgml_dlsym_hook.so", HostPath: filepath.Join(libDir, "hgml_dlsym_hook.so"), ReadOnly: true},
		{ContainerPath: "/usr/local/vppu/hggc_quota.so", HostPath: filepath.Join(libDir, "hggc_quota.so"), ReadOnly: true},
		{ContainerPath: "/usr/local/vppu/ppu-monitor", HostPath: filepath.Join(libDir, "ppu-monitor"), ReadOnly: true},
		{ContainerPath: "/var/run/vppu", HostPath: ledgerDir, ReadOnly: false},
	}, got.Mounts, "the preload asset and both shared objects are mounted where that asset names them")
}

// TestGetSlicedContainerAllocateResponseQuota pins how a request becomes quota: one figure per card for
// the exact dimension, one figure for the whole container for the oversubscribable one, and a refusal
// wherever a figure cannot be derived — never a card handed over uncapped.
func TestGetSlicedContainerAllocateResponseQuota(t *testing.T) {
	cases := []struct {
		name string
		// coresPercent, memPercent and memMib are the container's request; zero means the request
		// carries no limit of that kind at all.
		coresPercent int64
		memPercent   int64
		memMib       int64
		// devs overrides the single-group fixture, for the cases that turn on a card's own group.
		devs func() (*workercore.Devices, map[deviceplugin.Resource]int32)
		// breaks corrupts the device record after it is built.
		breaks func(devs *workercore.Devices)
		// wantQuota are the quota variables expected, log level and ledger path aside.
		wantQuota map[string]string
		wantErr   string
	}{
		{
			name:         "a percentage of the card's own VRAM",
			coresPercent: 10,
			memPercent:   25,
			wantQuota: map[string]string{
				"HGGC_DEVICE_SM_LIMIT":       "10",
				"HGGC_DEVICE_MEMORY_LIMIT_0": "24576",
			},
		},
		{
			name:         "an absolute memory figure is taken as it stands",
			coresPercent: 10,
			memMib:       8192,
			wantQuota: map[string]string{
				"HGGC_DEVICE_SM_LIMIT":       "10",
				"HGGC_DEVICE_MEMORY_LIMIT_0": "8192",
			},
		},
		{
			// The one thing not copied from the HAMi-core key shape: an absent compute figure means
			// "no cap" there and "unusable card" to this shim, so a whole card's worth is stated.
			name:       "a request with no compute dimension states the whole card's worth rather than omitting it",
			memPercent: 50,
			wantQuota: map[string]string{
				"HGGC_DEVICE_SM_LIMIT":       "100",
				"HGGC_DEVICE_MEMORY_LIMIT_0": "49152",
			},
		},
		{
			name:         "a request with no memory dimension is refused, not handed the whole card",
			coresPercent: 10,
			wantErr:      "derive sliced memory limit",
		},
		{
			// Admission pins a logical slice to one card today, so no such container is admitted. The
			// loop is written for several anyway, exactly as the NVIDIA branch's is, and this is what
			// keeps its indexing honest until the gate is lifted.
			name:         "two cards each take their own group's VRAM, under one shared compute figure",
			coresPercent: 50,
			memPercent:   25,
			devs: func() (*workercore.Devices, map[deviceplugin.Resource]int32) {
				devs := &workercore.Devices{
					Spec: workercore.DevicesSpec{
						Groups: []workercore.DevicesGroup{
							{
								ID: "ppu-98", Manufacturer: Manufacturer, Memory: 98304,
								Accelerators: []workercore.Accelerator{{
									ID: testPPUUUID0, Index: testPPUIndex0,
									PhysicalIndexes: []uint32{testPPUMinor0},
								}},
							},
							{
								ID: "ppu-49", Manufacturer: Manufacturer, Memory: 49152,
								Accelerators: []workercore.Accelerator{{
									ID: testPPUUUID1, Index: testPPUIndex1,
									PhysicalIndexes: []uint32{testPPUMinor1},
								}},
							},
						},
					},
				}
				return devs, map[deviceplugin.Resource]int32{
					{Group: "ppu-98", Device: testPPUUUID0}: 1,
					{Group: "ppu-49", Device: testPPUUUID1}: 1,
				}
			},
			wantQuota: map[string]string{
				"HGGC_DEVICE_SM_LIMIT":       "50",
				"HGGC_DEVICE_MEMORY_LIMIT_0": "24576",
				"HGGC_DEVICE_MEMORY_LIMIT_1": "12288",
			},
		},
		{
			// The addressing guard runs before any injection, so a card that cannot be proven fails
			// the allocation rather than being quota-capped and then handed a neighbour's node.
			name:         "a card whose minor number cannot be proven is refused before anything is injected",
			coresPercent: 10,
			memPercent:   25,
			breaks: func(devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil
			},
			wantErr: "no recorded minor number to prove its device node addresses it",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectNodeRoots(t)
			writeCardNodes(t)

			devs, allocated := slicedDevices(98304, testPPUUUID0, testPPUUUID1), allocatedOn(testPPUUUID0)
			if c.devs != nil {
				devs, allocated = c.devs()
			}
			if c.breaks != nil {
				c.breaks(devs)
			}
			pod, ctr := slicedPod("pod-"+strconv.Itoa(len(c.name)), c.coresPercent, c.memPercent, c.memMib)

			got, err := newSlicedServer(t).GetContainerAllocateResponse(
				context.Background(), pod, ctr, devs, allocated)
			if c.wantErr != "" {
				require.Error(t, err, "a figure that cannot be derived must fail the allocation")
				assert.Nil(t, got)
				assert.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)

			quota := make(map[string]string, len(got.Envs))
			for k, v := range got.Envs {
				if strings.HasPrefix(k, "HGGC_DEVICE_") {
					quota[k] = v
				}
			}
			assert.Equal(t, c.wantQuota, quota)
		})
	}
}

// A sliced container that declares LIBHGGC_LOG_LEVEL keeps its own value: the level is the debugging
// escape hatch, so the allocator states a default and never overrides a stated one.
func TestGetSlicedContainerAllocateResponseRespectsContainerLogLevel(t *testing.T) {
	redirectNodeRoots(t)
	writeCardNodes(t)

	devs := slicedDevices(98304, testPPUUUID0, testPPUUUID1)
	pod, ctr := slicedPod("pod-loglevel", 10, 25, 0)
	ctr.Env = []core.EnvVar{{Name: "LIBHGGC_LOG_LEVEL", Value: "2"}}

	got, err := newSlicedServer(t).GetContainerAllocateResponse(
		context.Background(), pod, ctr, devs, allocatedOn(testPPUUUID0))
	require.NoError(t, err)

	_, injected := got.Envs["LIBHGGC_LOG_LEVEL"]
	assert.False(t, injected, "a container that states its own verbosity keeps it")
}

// TestActuatePhysicalSliced pins the whole node set a partitioned container is given, in order: the
// vendor's two shared control nodes, the parent card's node, and the capability nodes of the GPU
// instance and of its compute instance — nothing else, and no environment variable, because this
// vendor has no container-runtime hook to interpret one.
func TestActuatePhysicalSliced(t *testing.T) {
	devDir, podsDir := redirectNodeRoots(t)
	createdFixture().write(t)

	s, drv := newPartitionedServer(testPPUUUID0)
	pod, ctr := partitionPod("pod-a")

	got, err := s.ActuatePhysicalSliced(
		context.Background(), pod, ctr, partitionDevices(testPPUUUID0), allocatedOn(testPPUUUID0), testProfile)
	require.NoError(t, err)

	assert.Equal(t, testProfile, got.Profile)
	assert.Equal(t,
		map[deviceplugin.Resource][]workercore.AcceleratorPhysicalPlacement{
			{Group: "ppu", Device: testPPUUUID0}: {{Start: 0, Length: 2}},
		},
		got.Placements, "the placement actually taken is published upward for the ledger")

	assert.Equal(t, []string{
		filepath.Join(devDir, "alixpu"),
		filepath.Join(devDir, "alixpu_ctl"),
		filepath.Join(devDir, "alixpu_ppu14"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1280"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1281"),
	}, devicePaths(got.Response))
	assert.Empty(t, got.Response.Envs, "the device nodes are the whole of the container's access")
	for i := range got.Response.Devices {
		spec := got.Response.Devices[i]
		assert.Equal(t, spec.HostPath, spec.ContainerPath, "a node is injected at its host path")
		assert.Equal(t, "rw", spec.Permissions)
	}

	assert.Equal(t, 1, drv.createCalls)
	assert.FileExists(t, markerPath(podsDir, "pod-a", "c", testPPUUUID0))
}

// TestActuatePhysicalSlicedVendorExample reproduces the instance numbering of the vendor's own isolation
// example — GPU instance 4, its compute instance 0, capability minors 1280 and 1281 — over an adopted
// instance, so the injected set can be compared against the documented one directly. The card carrying
// it sits at the fixture's measured ordinal rather than the example's card 0, because the numbering under
// test here is the instances' and not the card's.
func TestActuatePhysicalSlicedVendorExample(t *testing.T) {
	devDir, _ := redirectNodeRoots(t)
	nodeFixture{
		cardIndex: testPPUIndex0, cardMinor: testPPUMinor0,
		giID: 4, ciID: 0, giMinor: testCapGiMinor, ciMinor: testCapCiMinor,
	}.write(t)

	s, drv := newPartitionedServer(testPPUUUID0)
	drv.seedLive(testPPUUUID0, migInstance{
		GiID: 4, CiID: 0, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-adopted",
	})
	pod, ctr := partitionPod("pod-a")

	got, err := s.ActuatePhysicalSliced(
		context.Background(), pod, ctr, partitionDevices(testPPUUUID0), allocatedOn(testPPUUUID0), testProfile)
	require.NoError(t, err)

	assert.Equal(t, []string{
		filepath.Join(devDir, "alixpu"),
		filepath.Join(devDir, "alixpu_ctl"),
		filepath.Join(devDir, "alixpu_ppu14"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1280"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1281"),
	}, devicePaths(got.Response))
	assert.Zero(t, drv.createCalls, "an adoptable instance is bound, not re-created")
}

// TestActuatePhysicalSlicedMultiCard pins that the shared control nodes are injected once per container
// while every card contributes its own node triple, and that each card's placement is published.
func TestActuatePhysicalSlicedMultiCard(t *testing.T) {
	devDir, _ := redirectNodeRoots(t)
	createdFixture().write(t)
	// The second card's partition: the fake driver creates GPU instance 2 on it, and its capability
	// minors are unrelated to the first card's.
	nodeFixture{
		cardIndex: testPPUIndex1, cardMinor: testPPUMinor1,
		giID: 2, ciID: 2, giMinor: 4352, ciMinor: 4353,
	}.write(t)

	s, _ := newPartitionedServer(testPPUUUID0, testPPUUUID1)
	pod, ctr := partitionPod("pod-a")

	got, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr,
		partitionDevices(testPPUUUID0, testPPUUUID1), allocatedOn(testPPUUUID0, testPPUUUID1), testProfile)
	require.NoError(t, err)

	assert.Equal(t, []string{
		filepath.Join(devDir, "alixpu"),
		filepath.Join(devDir, "alixpu_ctl"),
		filepath.Join(devDir, "alixpu_ppu14"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1280"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1281"),
		filepath.Join(devDir, "alixpu_ppu15"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap4352"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap4353"),
	}, devicePaths(got.Response))
	assert.Len(t, got.Placements, 2)
}

// TestActuatePhysicalSlicedRequiredNodes removes or corrupts each member of a partition's node set in
// turn. Every one of them is required: the shared device-spec helper returns nil for a missing path and
// the whole-card responder appends only what is non-nil, so the failure mode this guards against is a
// SUCCESSFUL allocation carrying a silently incomplete set. Each case must instead fail the allocation
// and roll back the instance it created.
func TestActuatePhysicalSlicedRequiredNodes(t *testing.T) {
	cases := []struct {
		name string
		// breaks removes or corrupts exactly one member of the written node set.
		breaks  func(t *testing.T, f nodeFixture)
		wantErr string
		// wantCreated marks the cases whose failure comes after the reservation, so a created
		// instance must be destroyed and its marker removed.
		wantCreated bool
	}{
		{
			name:    "the shared control node is missing",
			breaks:  func(t *testing.T, _ nodeFixture) { removeFixture(t, sharedControlNodePaths()[0]) },
			wantErr: "alixpu\":",
		},
		{
			name:    "the shared control ioctl node is missing",
			breaks:  func(t *testing.T, _ nodeFixture) { removeFixture(t, sharedControlNodePaths()[1]) },
			wantErr: "alixpu_ctl\":",
		},
		{
			// The card's own node is verified by the addressing guard, which runs before the
			// reservation, so an unusable one costs no create — unlike every capability-node case
			// below, which can only be reached once the partition exists.
			name:    "the parent card's node is missing",
			breaks:  func(t *testing.T, f nodeFixture) { removeFixture(t, cardNodePath(f.cardIndex)) },
			wantErr: "alixpu_ppu14\":",
		},
		{
			name:        "the gpu instance's capability node is missing",
			breaks:      func(t *testing.T, f nodeFixture) { removeFixture(t, capNodePath(f.giMinor)) },
			wantErr:     "alixpu-cap1280\":",
			wantCreated: true,
		},
		{
			name:        "the compute instance's capability node is missing",
			breaks:      func(t *testing.T, f nodeFixture) { removeFixture(t, capNodePath(f.ciMinor)) },
			wantErr:     "alixpu-cap1281\":",
			wantCreated: true,
		},
		{
			name:        "the gpu instance's procfs access file is missing",
			breaks:      func(t *testing.T, f nodeFixture) { removeFixture(t, giAccessPath(f.cardIndex, f.giID)) },
			wantErr:     "read capability file",
			wantCreated: true,
		},
		{
			name: "the compute instance's procfs access file is missing",
			breaks: func(t *testing.T, f nodeFixture) {
				removeFixture(t, ciAccessPath(f.cardIndex, f.giID, f.ciID))
			},
			wantErr:     "read capability file",
			wantCreated: true,
		},
		{
			name: "the procfs access file carries no minor field",
			breaks: func(t *testing.T, f nodeFixture) {
				writeFixtureFile(t, giAccessPath(f.cardIndex, f.giID), "DeviceFileMode: 292\n")
			},
			wantErr:     "carries no DeviceFileMinor: field",
			wantCreated: true,
		},
		{
			name: "the procfs access file's minor is unparseable",
			breaks: func(t *testing.T, f nodeFixture) {
				writeFixtureFile(t, giAccessPath(f.cardIndex, f.giID), "DeviceFileMinor: none\n")
			},
			wantErr:     "parse DeviceFileMinor:",
			wantCreated: true,
		},
		{
			name: "a required path exists but is not a character device",
			breaks: func(t *testing.T, f nodeFixture) {
				writeFixtureFile(t, capNodePath(f.giMinor), notCharDevice)
			},
			wantErr:     "is not a character device",
			wantCreated: true,
		},
		{
			// Also the addressing guard's, and so also before the reservation: a path that is not a
			// character device carries no minor to compare the record against, which is a /dev tree
			// that cannot be proven to address this card rather than one that proves it does not.
			name: "the parent card's node exists but is not a character device",
			breaks: func(t *testing.T, f nodeFixture) {
				writeFixtureFile(t, cardNodePath(f.cardIndex), notCharDevice)
			},
			wantErr: "is not a character device",
		},
		{
			name: "a capability node carries a minor the driver did not publish for it",
			breaks: func(t *testing.T, f nodeFixture) {
				writeCharNode(t, capNodePath(f.giMinor), f.giMinor+1)
			},
			wantErr:     "carries minor 1281, want 1280",
			wantCreated: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, podsDir := redirectNodeRoots(t)
			fixture := createdFixture()
			fixture.write(t)
			c.breaks(t, fixture)

			s, drv := newPartitionedServer(testPPUUUID0)
			pod, ctr := partitionPod("pod-a")

			got, err := s.ActuatePhysicalSliced(
				context.Background(), pod, ctr, partitionDevices(testPPUUUID0), allocatedOn(testPPUUUID0), testProfile)
			require.Error(t, err, "an incomplete device set must fail the allocation, never shorten the response")
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), c.wantErr)

			if c.wantCreated {
				assert.Len(t, drv.destroyed, 1, "the instance this call created is rolled back")
			} else {
				assert.Zero(t, drv.createCalls, "a node verified before any reservation costs no create")
			}
			assert.NoFileExists(t, markerPath(podsDir, "pod-a", "c", testPPUUUID0),
				"a rolled-back allocation leaves no ownership marker")
		})
	}
}

// removeFixture deletes one fixture path.
func removeFixture(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Remove(path))
}

// TestActuatePhysicalSlicedCardAddressingGuard pins the states in which a card's ordinal cannot be shown
// to address the card the detector measured, each refused before anything is reserved so it costs no
// create. The proof is the node's own kernel minor number against the minor the detector recorded for
// that accelerator: equal means this ordinal reaches that card, whatever offset the driver's numbering
// puts between the two numbers. Nothing here computes one number from the other, because a card that
// violated an assumed offset would be addressed anyway and a host with a different offset would have
// every card refused.
func TestActuatePhysicalSlicedCardAddressingGuard(t *testing.T) {
	cases := []struct {
		name string
		// breaks corrupts the record, or the node the record is compared against.
		breaks  func(t *testing.T, devs *workercore.Devices)
		wantErr string
	}{
		{
			// The detector records nothing for a card whose driver could not answer for its minor
			// number, rather than substituting the enumeration counter, exactly so this refusal is
			// reachable: a substituted number is indistinguishable from a real one here, and would let
			// an unprovable ordinal be addressed as if it had been proven.
			name: "the detector recorded no minor number to prove the ordinal against",
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil
			},
			wantErr: "no recorded minor number to prove its device node addresses it",
		},
		{
			// The desynchronized index: the accelerator index is a post-filter counter, so a card the
			// detector skipped mid-enumeration shifts every later index onto a neighbor. The shifted
			// ordinal names a node whose minor is not the one recorded for this accelerator.
			name: "the node the ordinal names carries a minor the record disagrees with",
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{testPPUMinor1}
			},
			wantErr: "carries minor 15, want 16",
		},
		{
			name: "the node the ordinal names was renumbered under the record",
			breaks: func(t *testing.T, _ *workercore.Devices) {
				writeCharNode(t, cardNodePath(testPPUIndex0), testPPUMinor1)
			},
			wantErr: "carries minor 16, want 15",
		},
		{
			name: "the node the ordinal names is missing",
			breaks: func(t *testing.T, _ *workercore.Devices) {
				removeFixture(t, cardNodePath(testPPUIndex0))
			},
			wantErr: "alixpu_ppu14\":",
		},
		{
			name: "the node the ordinal names is not a character device",
			breaks: func(t *testing.T, _ *workercore.Devices) {
				writeFixtureFile(t, cardNodePath(testPPUIndex0), notCharDevice)
			},
			wantErr: "is not a character device",
		},
		{
			name: "an unallocated card missing its record does not fail the allocation",
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[1].PhysicalIndexes = nil
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, podsDir := redirectNodeRoots(t)
			createdFixture().write(t)

			devs := partitionDevices(testPPUUUID0, testPPUUUID1)
			c.breaks(t, devs)

			s, drv := newPartitionedServer(testPPUUUID0)
			pod, ctr := partitionPod("pod-a")

			got, err := s.ActuatePhysicalSliced(
				context.Background(), pod, ctr, devs, allocatedOn(testPPUUUID0), testProfile)
			if c.wantErr == "" {
				require.NoError(t, err, "an unallocated card is never addressed, so its record is never needed")
				assert.Equal(t, 1, drv.createCalls)
				return
			}
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), c.wantErr)
			assert.Zero(t, drv.createCalls,
				"a card whose ordinal cannot be proven is refused before anything is created")
			assert.NoFileExists(t, markerPath(podsDir, "pod-a", "c", testPPUUUID0),
				"a refused allocation leaves no ownership marker")
		})
	}
}

// TestActuatePhysicalSlicedAddressesTheNodeTheOrdinalNames pins the vendor's addressing rule end to end:
// the injected card node and the procfs capability subtree the partition's minors are read from are both
// the ones the card's ORDINAL — its accelerator index — names, never the ones its recorded kernel minor
// number would name.
//
// The fixture reproduces a measured 16-card host rather than a constructed arrangement: the card under
// test sits at ordinal 14 and its node, /dev/alixpu_ppu14, carries kernel minor 15, which is what the
// detector recorded for it. The decoy at /dev/alixpu_ppu15 carrying minor 16 is that host's next card,
// and the decoy procfs branch under ppu15 is that card's own capability tree. On the real host the last
// card's record names a ppu16 that does not exist at all — so a response carrying the decoy, or reading
// the capability minors out of the decoy's procfs branch, is the neighboring card's partition and fails
// here.
func TestActuatePhysicalSlicedAddressesTheNodeTheOrdinalNames(t *testing.T) {
	devDir, _ := redirectNodeRoots(t)
	createdFixture().write(t)

	// The neighboring card, at the path the card under test's RECORDED minor number names: the node a
	// record-keyed response would have injected, and the procfs branch it would have read the capability
	// minors from. Both belong to a DIFFERENT card.
	writeCharNode(t, cardNodePath(testPPUIndex1), testPPUMinor1)
	writeAccessFile(t, giAccessPath(testPPUIndex1, 1), 9990)
	writeAccessFile(t, ciAccessPath(testPPUIndex1, 1, 1), 9991)
	writeCharNode(t, capNodePath(9990), 9990)
	writeCharNode(t, capNodePath(9991), 9991)

	s, drv := newPartitionedServer(testPPUUUID0)
	devs := partitionDevices(testPPUUUID0)
	acc := devs.Spec.Groups[0].Accelerators[0]
	require.Equal(t, testPPUIndex0, acc.Index)
	require.Equal(t, []uint32{testPPUMinor0}, acc.PhysicalIndexes,
		"the card under test is addressed by an ordinal its recorded minor number is deliberately not")
	pod, ctr := partitionPod("pod-a")

	got, err := s.ActuatePhysicalSliced(
		context.Background(), pod, ctr, devs, allocatedOn(testPPUUUID0), testProfile)
	require.NoError(t, err)

	assert.Equal(t, []string{
		filepath.Join(devDir, "alixpu"),
		filepath.Join(devDir, "alixpu_ctl"),
		filepath.Join(devDir, "alixpu_ppu14"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1280"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1281"),
	}, devicePaths(got.Response), "the node the ordinal names is injected, not the one the record would")
	assert.Equal(t, 1, drv.createCalls)
}

// TestActuatePhysicalSlicedRollbackPerOutcome pins that a failure undoes exactly what this call did, per
// the card's reservation outcome: a created instance is destroyed and its marker dropped, an adopted one
// keeps living with only the marker dropped, and a prior allocation's own marker and instance are left
// untouched. The failure is injected on the second card, whose node set is absent.
func TestActuatePhysicalSlicedRollbackPerOutcome(t *testing.T) {
	adopted := migInstance{
		GiID: 4, CiID: 0, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-adopted",
	}

	cases := []struct {
		name string
		// setup seeds the first card so its reservation resolves to the outcome under test.
		setup func(t *testing.T, drv *fakeMigDriver, podsDir string)
		// fixture is the first card's node tree, which must match the instance it resolves to.
		fixture         nodeFixture
		wantDestroyed   int
		wantKeepsMarker bool
	}{
		{
			name:          "a created instance is destroyed and its marker dropped",
			fixture:       createdFixture(),
			wantDestroyed: 1,
		},
		{
			name: "an adopted instance survives, only its marker is dropped",
			setup: func(_ *testing.T, drv *fakeMigDriver, _ string) {
				drv.seedLive(testPPUUUID0, adopted)
			},
			fixture: nodeFixture{
				cardIndex: testPPUIndex0, cardMinor: testPPUMinor0,
				giID: 4, ciID: 0, giMinor: testCapGiMinor, ciMinor: testCapCiMinor,
			},
			wantDestroyed: 0,
		},
		{
			name: "a prior allocation's marker and instance are left intact",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, adopted)
				writeMarkerFixture(t, podsDir, selfMarker("pod-a", testPPUUUID0, adopted))
			},
			fixture: nodeFixture{
				cardIndex: testPPUIndex0, cardMinor: testPPUMinor0,
				giID: 4, ciID: 0, giMinor: testCapGiMinor, ciMinor: testCapCiMinor,
			},
			wantDestroyed:   0,
			wantKeepsMarker: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, podsDir := redirectNodeRoots(t)
			c.fixture.write(t)

			s, drv := newPartitionedServer(testPPUUUID0, testPPUUUID1)
			if c.setup != nil {
				c.setup(t, drv, podsDir)
			}
			pod, ctr := partitionPod("pod-a")

			got, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr,
				partitionDevices(testPPUUUID0, testPPUUUID1), allocatedOn(testPPUUUID0, testPPUUUID1), testProfile)
			require.Error(t, err, "the second card has no node set, so the allocation fails")
			assert.Nil(t, got)

			destroyedOnFirstCard := 0
			for _, inst := range drv.destroyed {
				if inst.GiID == c.fixture.giID {
					destroyedOnFirstCard++
				}
			}
			assert.Equal(t, c.wantDestroyed, destroyedOnFirstCard,
				"rollback destroys only an instance this call created")

			marker := markerPath(podsDir, "pod-a", "c", testPPUUUID0)
			if c.wantKeepsMarker {
				assert.FileExists(t, marker, "a prior allocation's marker is not this call's to remove")
			} else {
				assert.NoFileExists(t, marker)
			}
		})
	}
}

// TestActuatePhysicalSlicedRefusals pins the states a partition allocation refuses outright, each an
// error rather than a container started with no partition.
func TestActuatePhysicalSlicedRefusals(t *testing.T) {
	cases := []struct {
		name      string
		noDriver  bool
		allocated map[deviceplugin.Resource]int32
		profile   string
		wantErr   string
	}{
		{
			name:      "no driver was built on this node",
			noDriver:  true,
			allocated: allocatedOn(testPPUUUID0),
			profile:   testProfile,
			wantErr:   "mig actuator not configured",
		},
		{
			name:      "no card was allocated",
			allocated: map[deviceplugin.Resource]int32{},
			profile:   testProfile,
			wantErr:   "no allocated card",
		},
		{
			name:      "the card does not offer the requested profile",
			allocated: allocatedOn(testPPUUUID0),
			profile:   "2c.20g",
			wantErr:   "has no physical-slice profile",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectNodeRoots(t)
			createdFixture().write(t)

			s, _ := newPartitionedServer(testPPUUUID0)
			if c.noDriver {
				s.mig = nil
			}
			pod, ctr := partitionPod("pod-a")

			got, err := s.ActuatePhysicalSliced(
				context.Background(), pod, ctr, partitionDevices(testPPUUUID0), c.allocated, c.profile)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

// TestStatDeviceNodeMinor covers the real character-device predicate, which the fixtures substitute
// because a temporary directory cannot hold a device node without root. /dev/null is a character device
// on every platform this builds for; its numbers are platform-specific, so only the predicate is
// asserted.
func TestStatDeviceNodeMinor(t *testing.T) {
	_, err := statDeviceNodeMinor(os.DevNull)
	assert.NoError(t, err, "a real character device is accepted")

	regular := filepath.Join(t.TempDir(), "alixpu")
	writeFixtureFile(t, regular, "0")
	_, err = statDeviceNodeMinor(regular)
	require.Error(t, err, "a regular file is not a device node")
	assert.Contains(t, err.Error(), "is not a character device")

	_, err = statDeviceNodeMinor(filepath.Join(t.TempDir(), "absent"))
	assert.Error(t, err, "an absent path is not a device node")
}

// TestReadCapMinor pins that a capability minor is read out of the driver's record and that anything
// unreadable is an error rather than a zero — a zero would address the shared control node.
func TestReadCapMinor(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		body    string
		write   bool
		want    uint32
		wantErr string
	}{
		{name: "reads the field out of the record", body: "DeviceFileMinor: 1280\nDeviceFileMode: 292\n", write: true, want: 1280},
		{name: "tolerates surrounding whitespace", body: "  DeviceFileMinor:   1281  \n", write: true, want: 1281},
		{name: "no minor field", body: "DeviceFileMode: 292\n", write: true, wantErr: "carries no DeviceFileMinor: field"},
		{name: "unparseable minor", body: "DeviceFileMinor: 0x500\n", write: true, wantErr: "parse DeviceFileMinor:"},
		{name: "absent file", wantErr: "read capability file"},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, strconv.Itoa(i), "access")
			if c.write {
				writeFixtureFile(t, path, c.body)
			}

			got, err := readCapMinor(path)
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				assert.Zero(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestRequireCardNode pins the single definition of how a card is addressed on this vendor's node: the
// ordinal that names its device node and keys its procfs capability subtree is the card's accelerator
// index, and that ordinal may only be used once the node it names has been shown to carry the very kernel
// minor number the detector recorded for that accelerator. Neither number is ever derived from the other,
// in either direction — the equality of the node's own minor with the record is the whole proof, and it
// holds whatever offset the driver's numbering puts between an ordinal and a minor.
func TestRequireCardNode(t *testing.T) {
	cases := []struct {
		name string
		card string
		// breaks corrupts the record, or the node the record is compared against.
		breaks func(t *testing.T, devs *workercore.Devices)
		// want is the ordinal the card's paths are keyed by.
		want    uint32
		wantErr string
	}{
		{
			name: "a card's ordinal is its accelerator index, proven by the node's own minor",
			card: testPPUUUID0,
			want: testPPUIndex0,
		},
		{
			name: "the second card's ordinal is its own index, not a number derived from the first's",
			card: testPPUUUID1,
			want: testPPUIndex1,
		},
		{
			name:    "a card absent from the device record has no ordinal to address",
			card:    "PPU-absent",
			wantErr: "absent from the device record",
		},
		{
			name: "a card the detector recorded no minor number for cannot be proven",
			card: testPPUUUID0,
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil
			},
			wantErr: "no recorded minor number to prove its device node addresses it",
		},
		{
			name: "a node carrying the neighbor's minor is refused, not addressed",
			card: testPPUUUID0,
			breaks: func(t *testing.T, _ *workercore.Devices) {
				writeCharNode(t, cardNodePath(testPPUIndex0), testPPUMinor1)
			},
			wantErr: "carries minor 16, want 15",
		},
		{
			name: "a record naming the neighbor's minor is refused, not addressed",
			card: testPPUUUID0,
			breaks: func(_ *testing.T, devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{testPPUMinor1}
			},
			wantErr: "carries minor 15, want 16",
		},
		{
			name: "a missing node proves nothing",
			card: testPPUUUID0,
			breaks: func(t *testing.T, _ *workercore.Devices) {
				removeFixture(t, cardNodePath(testPPUIndex0))
			},
			wantErr: "alixpu_ppu14\":",
		},
		{
			name: "a node that is not a character device carries no minor to compare",
			card: testPPUUUID0,
			breaks: func(t *testing.T, _ *workercore.Devices) {
				writeFixtureFile(t, cardNodePath(testPPUIndex0), notCharDevice)
			},
			wantErr: "is not a character device",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectNodeRoots(t)
			writeCharNode(t, cardNodePath(testPPUIndex0), testPPUMinor0)
			writeCharNode(t, cardNodePath(testPPUIndex1), testPPUMinor1)

			devs := partitionDevices(testPPUUUID0, testPPUUUID1)
			if c.breaks != nil {
				c.breaks(t, devs)
			}

			ordinal, spec, err := requireCardNode(devs, c.card)
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				assert.Zero(t, ordinal)
				assert.Nil(t, spec, "an unproven card yields no node to inject")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, ordinal)
			require.NotNil(t, spec)
			assert.Equal(t, cardNodePath(c.want), spec.HostPath,
				"the node handed over is the one the ordinal names, and the one that was proven")
			assert.Equal(t, spec.HostPath, spec.ContainerPath)
			assert.Equal(t, "rw", spec.Permissions)
		})
	}
}
