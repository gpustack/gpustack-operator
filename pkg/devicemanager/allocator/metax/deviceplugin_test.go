package metax

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func metaxDevicesFixture() *workercore.Devices {
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "c500",
				Manufacturer: Manufacturer,
				Memory:       65536, // MiB
				Accelerators: []workercore.Accelerator{
					{ID: "GPU-0", Index: 0, PhysicalIndexes: []uint32{0, 128}, Topology: workercore.DeviceTopology{PciBusID: testBDF0}},
					{ID: "GPU-1", Index: 1, PhysicalIndexes: []uint32{1, 129}, Topology: workercore.DeviceTopology{PciBusID: testBDF1}},
				},
			}},
		},
	}
}

// slicedPod builds a pending pod whose container requests the decoupled compute and
// VRAM dimensions the allocator reads. A non-positive value omits that limit, so a
// caller can exercise the default-cores and missing-memory paths.
func slicedPod(uid, ctrName string, coresPercent, memPercent int64) (*core.Pod, *core.Container) {
	limits := core.ResourceList{}
	if coresPercent > 0 {
		limits[nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)] = *resource.NewQuantity(coresPercent, resource.DecimalSI)
	}
	if memPercent > 0 {
		limits[nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)] = *resource.NewQuantity(memPercent, resource.DecimalSI)
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: ctrName + "-pod", UID: types.UID(uid)},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name:      ctrName,
				Resources: core.ResourceRequirements{Limits: limits},
			}},
		},
	}
	return pod, &pod.Spec.Containers[0]
}

func newSlicedServer(mgr sgpuManager) *server {
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeSliced,
		},
		sgpu: mgr,
	}
}

func allocateGPU0() map[deviceplugin.Resource]int32 {
	return map[deviceplugin.Resource]int32{{Group: "c500", Device: "GPU-0"}: 1}
}

// A card whose drm indexes the detector could not read must not take the device plugin down with
// it. The detector reads them from sysfs and records both numbers, the card number alone, or
// nothing; this handler has no panic recovery, so indexing an absent one killed the process that
// serves every allocation on the node — for every manufacturer — over one unreadable directory.
// The nodes that cannot be named are left out instead.
func TestGetContainerAllocateResponseWithoutDrmIndexes(t *testing.T) {
	devs := metaxDevicesFixture()
	// Neither number readable, and only the card number readable: the two shapes sysfs yields
	// besides the full pair.
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil
	devs.Spec.Groups[0].Accelerators[1].PhysicalIndexes = []uint32{1}

	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
	}
	pod, ctr := slicedPod("pod-uid-no-drm", "ctr", 0, 0)
	allocated := map[deviceplugin.Resource]int32{
		{Group: "c500", Device: "GPU-0"}: 1,
		{Group: "c500", Device: "GPU-1"}: 1,
	}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)
	require.NotNil(t, resp)
	for _, d := range resp.Devices {
		assert.NotContains(t, d.HostPath, "/dev/dri/renderD",
			"a renderD index the detector never recorded names no node")
	}
}

// A partial slice creates one sgpu subdevice and injects METAX_SGPUS with the hard
// compute and VRAM quota, plus the correlation + slot marker.
func TestSliced_PartialSlice(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	s := newSlicedServer(mgr)
	devs := metaxDevicesFixture()

	pod, ctr := slicedPod("pod-uid-1", "train", 60, 50) // 60% compute, 50% of 65536 = 32768 MiB
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateGPU0())
	require.NoError(t, err)

	assert.Equal(t,
		"sgpu=0000:3d:00.0#0;compute=60;vram=32768;alias=gpustack-pod-uid-1",
		resp.Envs["METAX_SGPUS"])

	// The subdevice was created and the marker records the slice.
	assert.True(t, mgr.has(testBDF0, 0))
	m, err := parseMarker(markerPath("pod-uid-1", "train"))
	require.NoError(t, err)
	assert.Equal(t, testBDF0, m.CardBDF)
	assert.Equal(t, 0, m.Index)
	assert.Equal(t, 60, m.CoresPct)
	assert.Equal(t, int64(32768), m.MemMiB)
}

// A whole-card slice (100% compute AND full VRAM) takes the native path: no sgpu
// subdevice, no METAX_SGPUS, but an occupancy marker so the scanner sees the card taken.
func TestSliced_WholeCard(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	s := newSlicedServer(mgr)
	devs := metaxDevicesFixture()

	pod, ctr := slicedPod("pod-whole", "train", 100, 100)
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateGPU0())
	require.NoError(t, err)

	assert.Empty(t, resp.Envs["METAX_SGPUS"], "whole card injects no METAX_SGPUS")
	assert.Equal(t, 0, mgr.creates, "whole card creates no sgpu subdevice")
	m, err := parseMarker(markerPath("pod-whole", "train"))
	require.NoError(t, err)
	assert.True(t, m.wholeCard())
}

// An absent cores-percentage defaults to 100; combined with a partial memory request
// this stays a partial slice (compute 100, VRAM < card), never a whole card.
func TestSliced_DefaultCores(t *testing.T) {
	redirectLogicalSliceDirs(t)
	mgr := newFakeMgr()
	s := newSlicedServer(mgr)
	devs := metaxDevicesFixture()

	pod, ctr := slicedPod("pod-defcores", "train", 0, 50) // no cores request, 50% VRAM
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateGPU0())
	require.NoError(t, err)

	assert.Equal(t,
		"sgpu=0000:3d:00.0#0;compute=100;vram=32768;alias=gpustack-pod-defcores",
		resp.Envs["METAX_SGPUS"])
	assert.True(t, mgr.has(testBDF0, 0), "compute=100 with partial VRAM is still a partial slice")
}

// A sliced container with no memory dimension is rejected rather than silently given
// the whole card.
func TestSliced_NoMemoryRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer(newFakeMgr())
	devs := metaxDevicesFixture()

	pod, ctr := slicedPod("pod-nomem", "train", 60, 0)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateGPU0())
	require.Error(t, err)
}

// sgpu slicing is single-card: a multi-card sliced allocation is rejected loudly.
func TestSliced_MultiCardRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer(newFakeMgr())
	devs := metaxDevicesFixture()

	pod, ctr := slicedPod("pod-multi", "train", 60, 50)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{
			{Group: "c500", Device: "GPU-0"}: 1,
			{Group: "c500", Device: "GPU-1"}: 1,
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-card")
}

// A non-sliced (exclusive) responder never emits METAX_SGPUS or writes a marker: the
// slicing artifacts are isolated to the sliced mode.
func TestSliced_ExclusiveModeHasNoSlicingArtifacts(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
	}
	devs := metaxDevicesFixture()
	pod, ctr := slicedPod("pod-excl", "train", 60, 50)

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateGPU0())
	require.NoError(t, err)
	assert.Empty(t, resp.Envs["METAX_SGPUS"])
	assert.False(t, markerExists("pod-excl", "train"))
}
