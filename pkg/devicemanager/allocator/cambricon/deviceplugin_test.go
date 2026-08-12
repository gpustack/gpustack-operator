package cambricon

import (
	"context"
	"testing"

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

const testGroupID = "mlu590"

func cambriconDevicesFixture() *workercore.Devices {
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           testGroupID,
				Manufacturer: Manufacturer,
				Memory:       49152, // MiB
				Accelerators: []workercore.Accelerator{
					{ID: "MLU-0", Index: 0, PhysicalIndexes: []uint32{0}, Topology: workercore.DeviceTopology{PciBusID: testCard0}},
					{ID: "MLU-1", Index: 1, PhysicalIndexes: []uint32{1}, Topology: workercore.DeviceTopology{PciBusID: testCard1}},
				},
			}},
		},
	}
}

// slicedPod builds a pending pod whose container requests the decoupled compute and VRAM
// dimensions the allocator reads. A non-positive value omits that limit, so a caller can
// exercise the default-cores and missing-memory paths.
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

func newSlicedServer(driver smluDriver) *server {
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeSliced,
		},
		smlu: driver,
	}
}

func allocateMLU0() map[deviceplugin.Resource]int32 {
	return map[deviceplugin.Resource]int32{{Group: testGroupID, Device: "MLU-0"}: 1}
}

// A single-accelerator slice creates one sMLU instance, records the correlation + profile marker,
// and injects VIRTUAL_DEVICES naming the instance's device node.
func TestSliced_PartialSlice(t *testing.T) {
	redirectLogicalSliceDirs(t)
	d := newFakeDriver()
	s := newSlicedServer(d)
	devs := cambriconDevicesFixture()

	pod, ctr := slicedPod("pod-uid-1", "train", 25, 50) // 25% compute, 50% of 49152 = 24576 MiB
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.NoError(t, err)

	name := encodeInstanceName("pod-uid-1", "train")
	assert.True(t, d.hasInstance(name), "the sMLU instance was created")
	assert.Equal(t, "/dev/cambricon_dev-"+name, resp.Envs["VIRTUAL_DEVICES"])

	m, err := parseMarker(markerPath("pod-uid-1", "train"))
	require.NoError(t, err)
	assert.Equal(t, testCard0, m.Card)
	assert.Equal(t, name, m.Instance)
	assert.Equal(t, 25, m.CoresPct)
	assert.Equal(t, int64(24576), m.MemMiB)
}

// An absent cores-percentage defaults to 100 (a whole-accelerator-exclusive sMLU instance).
func TestSliced_DefaultCores(t *testing.T) {
	redirectLogicalSliceDirs(t)
	d := newFakeDriver()
	s := newSlicedServer(d)
	devs := cambriconDevicesFixture()

	pod, ctr := slicedPod("pod-defcores", "train", 0, 50) // no cores request, 50% VRAM
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.NoError(t, err)

	m, err := parseMarker(markerPath("pod-defcores", "train"))
	require.NoError(t, err)
	assert.Equal(t, 100, m.CoresPct, "an absent cores request defaults to 100")
}

// A sliced container with no memory dimension is rejected rather than silently given the
// whole accelerator.
func TestSliced_NoMemoryRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer(newFakeDriver())
	devs := cambriconDevicesFixture()

	pod, ctr := slicedPod("pod-nomem", "train", 25, 0)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.Error(t, err)
}

// sMLU slicing is single-accelerator: a multi-accelerator sliced allocation is rejected loudly.
func TestSliced_MultiCardRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer(newFakeDriver())
	devs := cambriconDevicesFixture()

	pod, ctr := slicedPod("pod-multi", "train", 25, 50)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{
			{Group: testGroupID, Device: "MLU-0"}: 1,
			{Group: testGroupID, Device: "MLU-1"}: 1,
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-card")
}

// A sliced allocation with no matching accelerator is rejected rather than dereferencing
// a nil group/accelerator.
func TestSliced_NoAllocatedAcceleratorRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer(newFakeDriver())
	devs := cambriconDevicesFixture()

	pod, ctr := slicedPod("pod-none", "train", 25, 50)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no allocated accelerator")
}

// A non-sliced (exclusive) responder never creates an instance or writes a marker: the
// slicing artifacts are isolated to the sliced mode.
func TestSliced_ExclusiveModeHasNoSlicingArtifacts(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
	}
	devs := cambriconDevicesFixture()
	pod, ctr := slicedPod("pod-excl", "train", 25, 50)

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.NoError(t, err)
	assert.Empty(t, resp.Envs["VIRTUAL_DEVICES"])
	assert.NotEmpty(t, resp.Envs["CAMBRICON_VISIBLE_DEVICES"], "exclusive mode keeps the plain visibility env")
	assert.False(t, markerExists("pod-excl", "train"))
}
