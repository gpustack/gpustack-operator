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
// The fixtures use them so a test would fail if the minors were ever computed instead of read.
const (
	testCapGiMinor = uint32(1280)
	testCapCiMinor = uint32(1281)
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
// removes or corrupts exactly one member.
type nodeFixture struct {
	ordinal uint32
	giID    uint32
	ciID    uint32
	giMinor uint32
	ciMinor uint32
}

// write publishes the whole node set: the two shared control nodes, the card's node at its ordinal plus
// one, both capability nodes, and both procfs access files.
func (f nodeFixture) write(t *testing.T) {
	t.Helper()
	for _, path := range sharedControlNodePaths() {
		writeCharNode(t, path, 0)
	}
	writeCharNode(t, cardNodePath(f.ordinal), f.ordinal+1)
	writeCharNode(t, capNodePath(f.giMinor), f.giMinor)
	writeCharNode(t, capNodePath(f.ciMinor), f.ciMinor)
	writeAccessFile(t, giAccessPath(f.ordinal, f.giID), f.giMinor)
	writeAccessFile(t, ciAccessPath(f.ordinal, f.giID, f.ciID), f.ciMinor)
}

// createdFixture is the node tree of the partition the fake driver creates first (GPU instance 1,
// compute instance 1) on the card at ordinal 0.
func createdFixture() nodeFixture {
	return nodeFixture{ordinal: 0, giID: 1, ciID: 1, giMinor: testCapGiMinor, ciMinor: testCapCiMinor}
}

// partitionDevices builds the shared partition fixture and records each card's driver minor number as
// its accelerator index plus one — the invariant the ordinal guard proves — since the shared helper
// carries the index alone.
func partitionDevices(uuids ...string) *workercore.Devices {
	devs := theadDevices(testProfile, 1, 2, uuids...)
	for i := range devs.Spec.Groups {
		accels := devs.Spec.Groups[i].Accelerators
		for j := range accels {
			accels[j].PhysicalIndexes = []uint32{accels[j].Index + 1}
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
// that address partitions. The vendor has no logical slicing, so no Sliced server is ever registered.
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
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-shared drops only the shared server",
			opts: device.AllocatorOptions{NoShared: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
			wantDriven: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-sliced changes nothing: this vendor has no logical slicing",
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
		filepath.Join(devDir, "alixpu_ppu0"),
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

// TestActuatePhysicalSlicedVendorExample reproduces the card/instance numbering of the vendor's own
// isolation example — card 0, GPU instance 4, its compute instance 0, capability minors 1280 and 1281 —
// over an adopted instance, so the injected set can be compared against the documented one directly.
func TestActuatePhysicalSlicedVendorExample(t *testing.T) {
	devDir, _ := redirectNodeRoots(t)
	nodeFixture{ordinal: 0, giID: 4, ciID: 0, giMinor: testCapGiMinor, ciMinor: testCapCiMinor}.write(t)

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
		filepath.Join(devDir, "alixpu_ppu0"),
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
	nodeFixture{ordinal: 1, giID: 2, ciID: 2, giMinor: 4352, ciMinor: 4353}.write(t)

	s, _ := newPartitionedServer(testPPUUUID0, testPPUUUID1)
	pod, ctr := partitionPod("pod-a")

	got, err := s.ActuatePhysicalSliced(context.Background(), pod, ctr,
		partitionDevices(testPPUUUID0, testPPUUUID1), allocatedOn(testPPUUUID0, testPPUUUID1), testProfile)
	require.NoError(t, err)

	assert.Equal(t, []string{
		filepath.Join(devDir, "alixpu"),
		filepath.Join(devDir, "alixpu_ctl"),
		filepath.Join(devDir, "alixpu_ppu0"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1280"),
		filepath.Join(devDir, "alixpu-caps", "alixpu-cap1281"),
		filepath.Join(devDir, "alixpu_ppu1"),
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
			name:        "the parent card's node is missing",
			breaks:      func(t *testing.T, f nodeFixture) { removeFixture(t, cardNodePath(f.ordinal)) },
			wantErr:     "alixpu_ppu0\":",
			wantCreated: true,
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
			breaks:      func(t *testing.T, f nodeFixture) { removeFixture(t, giAccessPath(f.ordinal, f.giID)) },
			wantErr:     "read capability file",
			wantCreated: true,
		},
		{
			name: "the compute instance's procfs access file is missing",
			breaks: func(t *testing.T, f nodeFixture) {
				removeFixture(t, ciAccessPath(f.ordinal, f.giID, f.ciID))
			},
			wantErr:     "read capability file",
			wantCreated: true,
		},
		{
			name: "the procfs access file carries no minor field",
			breaks: func(t *testing.T, f nodeFixture) {
				writeFixtureFile(t, giAccessPath(f.ordinal, f.giID), "DeviceFileMode: 292\n")
			},
			wantErr:     "carries no DeviceFileMinor: field",
			wantCreated: true,
		},
		{
			name: "the procfs access file's minor is unparseable",
			breaks: func(t *testing.T, f nodeFixture) {
				writeFixtureFile(t, giAccessPath(f.ordinal, f.giID), "DeviceFileMinor: none\n")
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
			name: "the card's node carries a minor that is not its ordinal plus one",
			breaks: func(t *testing.T, f nodeFixture) {
				writeCharNode(t, cardNodePath(f.ordinal), f.ordinal+7)
			},
			wantErr:     "carries minor 7, want 1",
			wantCreated: true,
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
				assert.Zero(t, drv.createCalls, "a control node missing before any reservation costs no create")
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

// TestActuatePhysicalSlicedOrdinalGuard pins that a card whose recorded minor number is not its
// accelerator index plus one is refused rather than addressed. The index is a post-filter counter, so a
// card skipped mid-enumeration desynchronizes it from the driver's ordinal; addressing it anyway would
// hand the container the next card's node. The guard runs before the reservation, so it costs no create.
func TestActuatePhysicalSlicedOrdinalGuard(t *testing.T) {
	cases := []struct {
		name            string
		physicalIndexes []uint32
	}{
		{name: "the recorded minor equals the index, as if the offset were absent", physicalIndexes: []uint32{0}},
		{name: "the recorded minor is a later card's", physicalIndexes: []uint32{2}},
		{name: "no minor number was recorded at all", physicalIndexes: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectNodeRoots(t)
			createdFixture().write(t)

			devs := partitionDevices(testPPUUUID0)
			devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = c.physicalIndexes

			s, drv := newPartitionedServer(testPPUUUID0)
			pod, ctr := partitionPod("pod-a")

			got, err := s.ActuatePhysicalSliced(
				context.Background(), pod, ctr, devs, allocatedOn(testPPUUUID0), testProfile)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), "not its accelerator index plus one")
			assert.Zero(t, drv.createCalls, "an unprovable ordinal is refused before anything is created")
		})
	}
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
			fixture:       nodeFixture{ordinal: 0, giID: 4, ciID: 0, giMinor: testCapGiMinor, ciMinor: testCapCiMinor},
			wantDestroyed: 0,
		},
		{
			name: "a prior allocation's marker and instance are left intact",
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, adopted)
				writeMarkerFixture(t, podsDir, selfMarker("pod-a", testPPUUUID0, adopted))
			},
			fixture:         nodeFixture{ordinal: 0, giID: 4, ciID: 0, giMinor: testCapGiMinor, ciMinor: testCapCiMinor},
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

// TestCardOrdinal pins that the ordinal comes from the accelerator index — the value both the device
// node and the procfs capability subtree are keyed by — and only when the recorded minor number proves
// it.
func TestCardOrdinal(t *testing.T) {
	devs := partitionDevices(testPPUUUID0, testPPUUUID1)

	ordinal, ok := cardOrdinal(devs, testPPUUUID1)
	assert.True(t, ok)
	assert.Equal(t, uint32(1), ordinal, "the second card's ordinal is its accelerator index, not its minor")

	_, ok = cardOrdinal(devs, "PPU-absent")
	assert.False(t, ok, "an unknown card has no ordinal")
}
