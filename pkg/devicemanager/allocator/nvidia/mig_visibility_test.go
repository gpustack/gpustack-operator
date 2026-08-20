package nvidia

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

const (
	visPodUID  = "pod-vis"
	visOwner   = "main"
	visSidecar = "sshd"
	visProfile = "1g.10gb"
)

// seedOwnedPartition puts an accelerator into the state a workload Allocate leaves behind: a live MIG
// instance plus the owner container's ownership marker naming it.
func seedOwnedPartition(t *testing.T, drv *fakeMigDriver, card string, giID uint32, uuid string) {
	t.Helper()
	drv.seedLive(card, migInstance{
		GiID: giID, CiID: giID, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: uuid,
	})
	writeOwnerMarker(t, card, migMarker{
		PodUID: visPodUID, Container: visOwner, Card: card, Profile: visProfile,
		GiID: giID, CiID: giID, MigUUID: uuid, ComputeSlices: 1, Start: 0, Length: 2,
	})
}

// writeOwnerMarker writes the owner container's marker for an accelerator verbatim, so a case can
// record something the live hardware contradicts.
func writeOwnerMarker(t *testing.T, card string, m migMarker) {
	t.Helper()
	require.NoError(t, writeMarker(markerPath(visPodUID, visOwner, card), m))
}

// TestGetPhysicalSlicedVisibilityResponse covers the sidecar's partition resolution: the happy
// path names the owner's partitions in devs order, and every record the resolver cannot prove
// still describes a live partition of the owner's on that accelerator fails the admission closed —
// answering with the parent accelerator would hand the sidecar a device-cgroup grant over every
// partition carved on it, including other tenants'.
func TestGetPhysicalSlicedVisibilityResponse(t *testing.T) {
	cases := []struct {
		name    string
		cards   []string
		setup   func(t *testing.T, drv *fakeMigDriver)
		want    string
		wantErr string
	}{
		{
			name:  "one accelerator names the owner's partition",
			cards: []string{testGPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver) {
				seedOwnedPartition(t, drv, testGPUUUID0, 1, "MIG-one")
			},
			want: "MIG-one",
		},
		{
			// Seeded in reverse, so the order can only come from devs — which is the order the
			// workload's own response used, and the whole point of the assertion sshd == main.
			name:  "two accelerators join in devs order, not marker-write order",
			cards: []string{testGPUUUID0, testGPUUUID1},
			setup: func(t *testing.T, drv *fakeMigDriver) {
				seedOwnedPartition(t, drv, testGPUUUID1, 2, "MIG-second")
				seedOwnedPartition(t, drv, testGPUUUID0, 1, "MIG-first")
			},
			want: "MIG-first,MIG-second",
		},
		{
			name:    "a missing marker fails closed",
			cards:   []string{testGPUUUID0},
			setup:   func(*testing.T, *fakeMigDriver) {},
			wantErr: "ownership marker",
		},
		{
			name:  "a malformed marker fails closed",
			cards: []string{testGPUUUID0},
			setup: func(t *testing.T, _ *fakeMigDriver) {
				path := markerPath(visPodUID, visOwner, testGPUUUID0)
				require.NoError(t, os.MkdirAll(deviceplugin.PodWorkDir(visPodUID, visOwner), 0o777))
				require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
			},
			wantErr: "ownership marker",
		},
		{
			name:  "an incomplete marker fails closed",
			cards: []string{testGPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver) {
				drv.seedLive(testGPUUUID0, migInstance{GiID: 1, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-one"})
				// No MigUUID: the one field the sidecar's grant is made of.
				writeOwnerMarker(t, testGPUUUID0, migMarker{
					PodUID: visPodUID, Container: visOwner, Card: testGPUUUID0, Profile: visProfile,
					GiID: 1, ComputeSlices: 1, Start: 0, Length: 2,
				})
			},
			wantErr: "ownership marker",
		},
		{
			name:  "a marker recording another accelerator fails closed",
			cards: []string{testGPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver) {
				drv.seedLive(testGPUUUID0, migInstance{GiID: 1, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-one"})
				writeOwnerMarker(t, testGPUUUID0, migMarker{
					PodUID: visPodUID, Container: visOwner, Card: testGPUUUID1, Profile: visProfile,
					GiID: 1, CiID: 1, MigUUID: "MIG-one", ComputeSlices: 1, Start: 0, Length: 2,
				})
			},
			wantErr: "records card",
		},
		{
			name:  "a marker whose gpu instance is gone fails closed",
			cards: []string{testGPUUUID0},
			setup: func(t *testing.T, _ *fakeMigDriver) {
				writeOwnerMarker(t, testGPUUUID0, migMarker{
					PodUID: visPodUID, Container: visOwner, Card: testGPUUUID0, Profile: visProfile,
					GiID: 7, CiID: 7, MigUUID: "MIG-destroyed", ComputeSlices: 1, Start: 0, Length: 2,
				})
			},
			wantErr: "missing gpu instance",
		},
		{
			// The GPU-instance id outlived the partition it named: NVIDIA can reassign it to a
			// freshly created one, which may belong to another tenant.
			name:  "a reused gpu-instance id fails closed",
			cards: []string{testGPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver) {
				drv.seedLive(testGPUUUID0, migInstance{GiID: 1, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-somebody-else"})
				writeOwnerMarker(t, testGPUUUID0, migMarker{
					PodUID: visPodUID, Container: visOwner, Card: testGPUUUID0, Profile: visProfile,
					GiID: 1, CiID: 1, MigUUID: "MIG-ours", ComputeSlices: 1, Start: 0, Length: 2,
				})
			},
			wantErr: "id reused",
		},
		{
			name:  "a profile the accelerator no longer offers fails closed",
			cards: []string{testGPUUUID0},
			setup: func(t *testing.T, drv *fakeMigDriver) {
				drv.seedLive(testGPUUUID0, migInstance{GiID: 1, ComputeSlices: 1, Placement: migPlacement{0, 2}, UUID: "MIG-one"})
				writeOwnerMarker(t, testGPUUUID0, migMarker{
					PodUID: visPodUID, Container: visOwner, Card: testGPUUUID0, Profile: "7g.80gb",
					GiID: 1, CiID: 1, MigUUID: "MIG-one", ComputeSlices: 7, Start: 0, Length: 8,
				})
			},
			wantErr: "no longer offers",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			drv := newFakeMigDriver()
			for _, card := range c.cards {
				drv.possible[card] = evenSlots()
			}
			c.setup(t, drv)

			s := &server{mig: drv}
			devs := migDevices(visProfile, 1, 2, c.cards...)
			resp, err := s.GetPhysicalSlicedVisibilityResponse(
				context.Background(), visibilityPod(), &core.Container{Name: visSidecar},
				devs, allocatedCardSet(devs, c.cards), visOwner)

			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				assert.Nil(t, resp, "a rejected visibility resolution must return no response at all")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.Equal(t, map[string]string{"NVIDIA_VISIBLE_DEVICES": c.want}, resp.Envs,
				"the sidecar is told the partitions, and nothing else")
		})
	}
}

// TestGetPhysicalSlicedVisibilityResponse_NoDriver pins that a visibility server built on a node
// that serves no partitioning rejects rather than answering with the parent accelerator.
func TestGetPhysicalSlicedVisibilityResponse_NoDriver(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := &server{}
	devs := migDevices(visProfile, 1, 2, testGPUUUID0)

	resp, err := s.GetPhysicalSlicedVisibilityResponse(
		context.Background(), visibilityPod(), &core.Container{Name: visSidecar},
		devs, allocatedCardSet(devs, []string{testGPUUUID0}), visOwner)
	require.Error(t, err)
	assert.Nil(t, resp)
}

// TestNew_VisibilityHoldsTheMigDriver pins which servers are handed the MIG driver: the
// visibility server needs it to prove a sidecar's partition is live, and a node that serves no
// partitioning must construct none at all (the real driver takes an NVML init). That it is the
// same instance both servers hold is not asserted: the darwin stub is a zero-size value type, so
// two of them are indistinguishable.
func TestNew_VisibilityHoldsTheMigDriver(t *testing.T) {
	assert.Equal(t,
		map[workercore.DeviceAllocationMode]bool{
			workercore.DeviceAllocationModeExclusive:   false,
			workercore.DeviceAllocationModeShared:      false,
			workercore.DeviceAllocationModePartitioned: true,
			workercore.DeviceAllocationModeVisibility:  true,
		},
		migDriverByMode(t, device.AllocatorOptions{NoSliced: true}),
		"the partition-addressing servers hold the driver, the rest hold none")

	assert.Equal(t,
		map[workercore.DeviceAllocationMode]bool{
			workercore.DeviceAllocationModeExclusive:  false,
			workercore.DeviceAllocationModeShared:     false,
			workercore.DeviceAllocationModeSliced:     false,
			workercore.DeviceAllocationModeVisibility: false,
		},
		migDriverByMode(t, device.AllocatorOptions{NoPartitioned: true}),
		"a node that serves no partitioning initializes no MIG driver")
}

// migDriverByMode reports, per registered server, whether it holds a MIG driver.
func migDriverByMode(t *testing.T, opts device.AllocatorOptions) map[workercore.DeviceAllocationMode]bool {
	t.Helper()
	agg, ok := New(opts).(*aggregated)
	require.True(t, ok)
	held := make(map[workercore.DeviceAllocationMode]bool, len(agg.servers))
	for i := range agg.servers {
		srv, ok := agg.servers[i].(*server)
		require.True(t, ok)
		held[srv.AllocationMode] = srv.mig != nil
	}
	return held
}

// visibilityPod is the sidecar's pod: only its UID matters, since the marker is keyed by it.
func visibilityPod() *core.Pod {
	return &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", UID: types.UID(visPodUID)}}
}

// allocatedCardSet is the owner's accelerator map the server hands the resolver.
func allocatedCardSet(devs *workercore.Devices, cards []string) map[deviceplugin.Resource]int32 {
	allocated := make(map[deviceplugin.Resource]int32, len(cards))
	for _, card := range cards {
		allocated[resourceForAccelerator(devs, card)] = 1
	}
	return allocated
}
