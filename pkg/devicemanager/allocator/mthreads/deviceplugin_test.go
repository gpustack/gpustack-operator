package mthreads

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

const (
	testAccelID0 = "MTGPU-0"
	testAccelID1 = "MTGPU-1"
)

func mthreadsDevicesFixture() *workercore.Devices {
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "mtt-s4000",
				Manufacturer: Manufacturer,
				Memory:       49152, // MiB (48 GiB)
				Accelerators: []workercore.Accelerator{
					{ID: testAccelID0, Index: 0},
					{ID: testAccelID1, Index: 1},
				},
			}},
		},
	}
}

// slicedPod builds a pending pod whose container requests the decoupled compute and
// VRAM dimensions the allocator reads: ".sliced.cores-percentage" (compute weight) and
// ".sliced.memory-percentage" (per-card VRAM). A non-positive dimension is omitted so a
// test can exercise the defaults.
func slicedPod(uid, ctrName string, coresPercent, memPercent int64) (*core.Pod, *core.Container) {
	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	limits := core.ResourceList{}
	if coresPercent > 0 {
		limits[coresRes] = *resource.NewQuantity(coresPercent, resource.DecimalSI)
	}
	if memPercent > 0 {
		limits[memPctRes] = *resource.NewQuantity(memPercent, resource.DecimalSI)
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

func newServerWithMode(mode workercore.DeviceAllocationMode) *server {
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: mode,
		},
	}
}

// A sliced MThreads container gets a hard VRAM cap (MTHREADS_QOS_MEMORY_LIMIT, bytes)
// and a relative compute weight (MTHREADS_QOS_COMPUTING_POWER_WEIGHT = cores%), keeping
// MTHREADS_VISIBLE_DEVICES.
func TestGetSlicedContainerAllocateResponse(t *testing.T) {
	s := newServerWithMode(workercore.DeviceAllocationModeSliced)
	devs := mthreadsDevicesFixture()

	// cores=8% weight + memory=50% of a 48 GiB card -> 24 GiB hard cap.
	pod, ctr := slicedPod("pod-uid-1", "train", 8, 50)
	allocated := map[deviceplugin.Resource]int32{{Group: "mtt-s4000", Device: testAccelID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	assert.Equal(t, testAccelID0, resp.Envs["MTHREADS_VISIBLE_DEVICES"])
	// floor(49152 * 50 / 100) = 24576 MiB -> 24576 * 1024 * 1024 bytes.
	assert.Equal(t, "25769803776", resp.Envs["MTHREADS_QOS_MEMORY_LIMIT"])
	assert.Equal(t, "8", resp.Envs["MTHREADS_QOS_COMPUTING_POWER_WEIGHT"])
	assert.Len(t, resp.Envs, 3, "sliced emits exactly the three QoS envs")
	assert.Empty(t, resp.Mounts, "MThreads slicing is pure env, no mounts")
	assert.Empty(t, resp.Devices, "MThreads slicing is pure env, no device nodes")
}

// An absent .sliced.cores-percentage defaults the compute weight to 100 (a whole card's
// compute); the memory cap is still honored.
func TestGetSlicedContainerAllocateResponse_DefaultCores(t *testing.T) {
	s := newServerWithMode(workercore.DeviceAllocationModeSliced)
	devs := mthreadsDevicesFixture()

	pod, ctr := slicedPod("pod-uid-2", "train", 0, 25)
	allocated := map[deviceplugin.Resource]int32{{Group: "mtt-s4000", Device: testAccelID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	assert.Equal(t, "100", resp.Envs["MTHREADS_QOS_COMPUTING_POWER_WEIGHT"])
	// floor(49152 * 25 / 100) = 12288 MiB -> 12288 * 1024 * 1024 bytes.
	assert.Equal(t, "12884901888", resp.Envs["MTHREADS_QOS_MEMORY_LIMIT"])
}

// A sliced container with no memory dimension (neither .sliced.memory-percentage nor
// .sliced.memory-mib) must be rejected rather than silently given the whole card.
func TestGetSlicedContainerAllocateResponse_NoMemoryRequest(t *testing.T) {
	s := newServerWithMode(workercore.DeviceAllocationModeSliced)
	devs := mthreadsDevicesFixture()

	pod, ctr := slicedPod("pod-uid-3", "train", 8, 0)
	allocated := map[deviceplugin.Resource]int32{{Group: "mtt-s4000", Device: testAccelID0}: 1}

	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.Error(t, err)
}

// The non-sliced modes (exclusive/shared/visibility) emit only MTHREADS_VISIBLE_DEVICES —
// no QoS envs — so the sliced branch stays isolated from the plain device-visibility path.
func TestGetContainerAllocateResponse_NonSlicedIsolation(t *testing.T) {
	devs := mthreadsDevicesFixture()
	pod, ctr := slicedPod("pod-uid-4", "train", 8, 50)
	allocated := map[deviceplugin.Resource]int32{
		{Group: "mtt-s4000", Device: testAccelID0}: 1,
		{Group: "mtt-s4000", Device: testAccelID1}: 1,
	}

	for _, mode := range []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeVisibility,
	} {
		s := newServerWithMode(mode)
		resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
		require.NoErrorf(t, err, "mode %s", mode)
		assert.Equalf(t, testAccelID0+","+testAccelID1, resp.Envs["MTHREADS_VISIBLE_DEVICES"], "mode %s", mode)
		assert.Lenf(t, resp.Envs, 1, "mode %s emits only MTHREADS_VISIBLE_DEVICES", mode)
		assert.Emptyf(t, resp.Mounts, "mode %s has no mounts", mode)
	}
}
