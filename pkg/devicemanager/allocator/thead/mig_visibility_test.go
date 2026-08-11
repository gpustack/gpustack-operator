package thead

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/deviceplugin"
)

const (
	// visPodUID is the Pod both containers belong to, visOwner the accelerator-holding container
	// whose markers the resolution reads (the name the shared marker fixture records), and
	// visSidecar the container co-allocating its partitions.
	visPodUID  = "pod-vis"
	visOwner   = "c"
	visSidecar = "sshd"
)

// visInstance is the partition the owner holds on the first accelerator, and visInstance1 the one it
// holds on the second — matching the node fixtures the actuator tests already use for those two.
var (
	visInstance = migInstance{
		GiID: 1, CiID: 1, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{0, 2}, UUID: "MIG-owned-0",
	}
	visInstance1 = migInstance{
		GiID: 2, CiID: 2, ProfileID: testProfileID, ComputeSlices: 1,
		Placement: migPlacement{2, 2}, UUID: "MIG-owned-1",
	}
)

// seedOwnedPartition puts an accelerator into the state the owner's own Allocate left behind: a live
// partition plus the ownership marker naming it.
func seedOwnedPartition(t *testing.T, drv *fakeMigDriver, podsDir, cardUUID string, inst migInstance) {
	t.Helper()
	drv.seedLive(cardUUID, inst)
	writeMarkerFixture(t, podsDir, ownerMarker(cardUUID, inst))
}

// ownerMarker is the marker the owner container's reservation would have written for an accelerator.
func ownerMarker(cardUUID string, inst migInstance) migMarker {
	m := selfMarker(visPodUID, cardUUID, inst)
	m.Container = visOwner
	return m
}

// visibilityPod is the Pod the co-allocating container is served for.
func visibilityPod() (*core.Pod, *core.Container) {
	pod, _ := partitionPod(visPodUID)
	return pod, &core.Container{Name: visSidecar}
}

// TestGetPhysicalSlicedVisibilityResponse covers the co-allocating container's partition resolution:
// the happy path hands it the very node set the owner's own response carried, in the same accelerator
// order, and every record the resolver cannot prove still describes a live partition of the owner's
// on that accelerator fails the allocation closed — answering with the parent accelerator would
// grant a device-cgroup access over every partition carved on it, including other tenants'.
func TestGetPhysicalSlicedVisibilityResponse(t *testing.T) {
	cases := []struct {
		name  string
		cards []string
		// setup seeds the live partitions, the ownership markers and the vendor node tree.
		setup func(t *testing.T, drv *fakeMigDriver, podsDir string)
		// wantNodes are the device-node names, in order, relative to the redirected /dev root.
		wantNodes []string
		// breakOrdinal drops the first accelerator's recorded minor number, leaving nothing to prove that its
		// ordinal addresses the accelerator the detector measured.
		breakOrdinal bool
		wantErr      string
	}{
		{
			name:  "one accelerator is shown the owner's partition, not the parent accelerator alone",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedOwnedPartition(t, drv, podsDir, testPPUUUID0, visInstance)
				createdFixture().write(t)
			},
			wantNodes: []string{
				"alixpu", "alixpu_ctl", "alixpu_ppu14",
				filepath.Join("alixpu-caps", "alixpu-cap1280"),
				filepath.Join("alixpu-caps", "alixpu-cap1281"),
			},
		},
		{
			// Seeded in reverse, so the order can only come from devs — the order the owner's own
			// response used, which is the whole point of the two responses reading the same.
			name:  "two accelerators join in devs order, not marker-write order",
			cards: []string{testPPUUUID0, testPPUUUID1},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedOwnedPartition(t, drv, podsDir, testPPUUUID1, visInstance1)
				seedOwnedPartition(t, drv, podsDir, testPPUUUID0, visInstance)
				createdFixture().write(t)
				nodeFixture{
					cardIndex: testPPUIndex1, cardMinor: testPPUMinor1,
					giID: 2, ciID: 2, giMinor: 4352, ciMinor: 4353,
				}.write(t)
			},
			wantNodes: []string{
				"alixpu", "alixpu_ctl", "alixpu_ppu14",
				filepath.Join("alixpu-caps", "alixpu-cap1280"),
				filepath.Join("alixpu-caps", "alixpu-cap1281"),
				"alixpu_ppu15",
				filepath.Join("alixpu-caps", "alixpu-cap4352"),
				filepath.Join("alixpu-caps", "alixpu-cap4353"),
			},
		},
		{
			name:  "a missing marker fails closed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, _ string) {
				drv.seedLive(testPPUUUID0, visInstance)
				createdFixture().write(t)
			},
			wantErr: "ownership marker",
		},
		{
			name:  "a malformed marker fails closed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, visInstance)
				createdFixture().write(t)
				writeFixtureFile(t, markerPath(podsDir, visPodUID, visOwner, testPPUUUID0), "{not json")
			},
			wantErr: "ownership marker",
		},
		{
			name:  "a marker recording another accelerator fails closed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, visInstance)
				createdFixture().write(t)
				// Written at the first accelerator's own path, but recording the second accelerator.
				m := ownerMarker(testPPUUUID1, visInstance)
				require.NoError(t, writeMarker(markerPath(podsDir, visPodUID, visOwner, testPPUUUID0), m))
			},
			wantErr: "records card",
		},
		{
			name:  "a profile the accelerator no longer offers fails closed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				drv.seedLive(testPPUUUID0, visInstance)
				createdFixture().write(t)
				m := ownerMarker(testPPUUUID0, visInstance)
				m.Profile = "2c.20g"
				writeMarkerFixture(t, podsDir, m)
			},
			wantErr: "no longer offers",
		},
		{
			name:  "a partition the driver no longer reports fails closed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, _ *fakeMigDriver, podsDir string) {
				// The owner died between the two calls: its marker survives, its partition does not.
				writeMarkerFixture(t, podsDir, ownerMarker(testPPUUUID0, visInstance))
				createdFixture().write(t)
			},
			wantErr: "missing gpu instance",
		},
		{
			name:  "a reused gpu-instance id fails closed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				// The id the marker records now belongs to a different partition.
				reused := visInstance
				reused.UUID = "MIG-somebody-else"
				drv.seedLive(testPPUUUID0, reused)
				writeMarkerFixture(t, podsDir, ownerMarker(testPPUUUID0, visInstance))
				createdFixture().write(t)
			},
			wantErr: "id reused",
		},
		{
			// The GPU instance is live and the marker's identity matches, yet the compute instance it
			// records is gone: its capability minor is read fresh from procfs at the per-GI-per-CI
			// path, so a stale compute-instance id leaves that path absent. The resolution must fail
			// rather than hand over a node set missing the partition's compute instance.
			name:  "a marker whose compute instance is gone fails closed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedOwnedPartition(t, drv, podsDir, testPPUUUID0, visInstance)
				createdFixture().write(t)
				removeFixture(t, ciAccessPath(testPPUIndex0, visInstance.GiID, visInstance.CiID))
			},
			wantErr: "read capability file",
		},
		{
			name:  "an accelerator the detector recorded no minor number for is refused rather than addressed",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedOwnedPartition(t, drv, podsDir, testPPUUUID0, visInstance)
				createdFixture().write(t)
			},
			breakOrdinal: true,
			wantErr:      "no recorded minor number to prove its device node addresses it",
		},
		{
			name:  "a shared control node the container needs is missing",
			cards: []string{testPPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver, podsDir string) {
				seedOwnedPartition(t, drv, podsDir, testPPUUUID0, visInstance)
				createdFixture().write(t)
				removeFixture(t, sharedControlNodePaths()[0])
			},
			wantErr: "alixpu\":",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devDir, podsDir := redirectNodeRoots(t)
			s, drv := newPartitionedServer(c.cards...)
			c.setup(t, drv, podsDir)

			devs := partitionDevices(c.cards...)
			if c.breakOrdinal {
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil
			}
			pod, ctr := visibilityPod()

			resp, err := s.GetPhysicalSlicedVisibilityResponse(
				context.Background(), pod, ctr, devs, allocatedOn(c.cards...), visOwner)

			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				assert.Nil(t, resp, "a rejected visibility resolution returns no response at all")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)

			want := make([]string, 0, len(c.wantNodes))
			for _, name := range c.wantNodes {
				want = append(want, filepath.Join(devDir, name))
			}
			assert.Equal(t, want, devicePaths(resp),
				"the co-allocating container is shown the owner's partitions, and nothing else")
			assert.Empty(t, resp.Envs, "the device nodes are the whole of the container's access")
			for i := range resp.Devices {
				spec := resp.Devices[i]
				assert.Equal(t, spec.HostPath, spec.ContainerPath, "a node is injected at its host path")
				assert.Equal(t, "rw", spec.Permissions)
			}
			assert.Zero(t, drv.createCalls, "a visibility resolution creates no partition")
			assert.Empty(t, drv.destroyed, "a visibility resolution destroys no partition")
		})
	}
}

// TestGetPhysicalSlicedVisibilityResponseMatchesOwner asserts the co-allocating container is handed
// exactly the node set the owner's own actuation returned — the two responses are assembled the same
// way, so a shell in the co-allocated container reaches the owner's partition and no other.
func TestGetPhysicalSlicedVisibilityResponseMatchesOwner(t *testing.T) {
	redirectNodeRoots(t)
	createdFixture().write(t)

	s, _ := newPartitionedServer(testPPUUUID0)
	devs := partitionDevices(testPPUUUID0)
	allocated := allocatedOn(testPPUUUID0)
	pod, sidecar := visibilityPod()
	owner := &core.Container{Name: visOwner}

	ownerResp, err := s.ActuatePhysicalSliced(context.Background(), pod, owner, devs, allocated, testProfile)
	require.NoError(t, err)

	resp, err := s.GetPhysicalSlicedVisibilityResponse(
		context.Background(), pod, sidecar, devs, allocated, visOwner)
	require.NoError(t, err)

	assert.Equal(t, devicePaths(ownerResp.Response), devicePaths(resp))
}

// TestGetPhysicalSlicedVisibilityResponseRefusals pins the states the resolution refuses before it
// reads any record, each an error rather than a container shown the parent accelerator.
func TestGetPhysicalSlicedVisibilityResponseRefusals(t *testing.T) {
	cases := []struct {
		name      string
		noDriver  bool
		allocated map[deviceplugin.Resource]int32
		wantErr   string
	}{
		{
			name:      "no driver was built on this node",
			noDriver:  true,
			allocated: allocatedOn(testPPUUUID0),
			wantErr:   "mig driver not configured",
		},
		{
			name:      "no accelerator was allocated",
			allocated: map[deviceplugin.Resource]int32{},
			wantErr:   "no allocated card",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, podsDir := redirectNodeRoots(t)
			s, drv := newPartitionedServer(testPPUUUID0)
			seedOwnedPartition(t, drv, podsDir, testPPUUUID0, visInstance)
			createdFixture().write(t)
			if c.noDriver {
				s.mig = nil
			}
			pod, ctr := visibilityPod()

			resp, err := s.GetPhysicalSlicedVisibilityResponse(
				context.Background(), pod, ctr, partitionDevices(testPPUUUID0), c.allocated, visOwner)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
			assert.Nil(t, resp)
		})
	}
}

// TestGetPhysicalSlicedVisibilityResponseCardStateError asserts an accelerator state the driver
// cannot prove complete is an error, never an empty accelerator — reading a live partition as absent
// here would deny a legitimate co-allocation, and reading one as present would be worse.
func TestGetPhysicalSlicedVisibilityResponseCardStateError(t *testing.T) {
	_, podsDir := redirectNodeRoots(t)
	s, drv := newPartitionedServer(testPPUUUID0)
	seedOwnedPartition(t, drv, podsDir, testPPUUUID0, visInstance)
	createdFixture().write(t)
	drv.cardStateErr = os.ErrClosed
	pod, ctr := visibilityPod()

	resp, err := s.GetPhysicalSlicedVisibilityResponse(
		context.Background(), pod, ctr, partitionDevices(testPPUUUID0), allocatedOn(testPPUUUID0), visOwner)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read card")
	assert.Nil(t, resp)
}
