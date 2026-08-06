package iluvatar

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const (
	testGPUUUID0 = "GPU-aaaa0000-0000-0000-0000-000000000000"
	testGPUUUID1 = "GPU-bbbb1111-1111-1111-1111-111111111111"
)

// redirectLogicalSliceDirs points the logical-slicing host paths (incl. the vgpulock dir)
// at a temp dir for the test.
func redirectLogicalSliceDirs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origLib, origPods, origLock := deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir, hostVgpuLockPath
	deviceplugin.OperatorLibDir = filepath.Join(root, "lib")
	deviceplugin.OperatorPodsDir = filepath.Join(root, "pods")
	hostVgpuLockPath = filepath.Join(root, "vgpulock")
	t.Cleanup(func() {
		deviceplugin.OperatorLibDir = origLib
		deviceplugin.OperatorPodsDir = origPods
		hostVgpuLockPath = origLock
	})
	return root
}

// iluvatarDevices builds a fixture with one group (memory drives the per-card VRAM limit)
// holding the given GPU UUIDs. corex presents a single CUDA-compatible driver level, so no
// runtime version is needed — the sliced path mounts one HAMi-core libvgpu regardless.
func iluvatarDevices(memoryMiB uint64, uuids ...string) *workercore.Devices {
	accels := make([]workercore.Accelerator, len(uuids))
	for i, u := range uuids {
		accels[i] = workercore.Accelerator{ID: u, Index: uint32(i)}
	}
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "bi150",
				Manufacturer: Manufacturer,
				Memory:       memoryMiB,
				Accelerators: accels,
			}},
		},
	}
}

// slicedPod builds a sliced container requesting the decoupled compute and VRAM dimensions the
// allocator reads: ".sliced.cores-percentage" (SM) and ".sliced.memory-percentage" (per-card VRAM).
func slicedPod(uid, ctrName string, coresPercent, memPercent int64) (*core.Pod, *core.Container) {
	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: ctrName + "-pod", UID: types.UID(uid)},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name: ctrName,
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						coresRes:  *resource.NewQuantity(coresPercent, resource.DecimalSI),
						memPctRes: *resource.NewQuantity(memPercent, resource.DecimalSI),
					},
				},
			}},
		},
	}
	return pod, &pod.Spec.Containers[0]
}

func newSlicedServer() *server {
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeSliced,
		},
	}
}

// TestNew_ServerSet pins which families Iluvatar registers a device-plugin server for, and that
// each control flag removes exactly its own. Iluvatar declares no partition kind, so it never
// registers a partitioned server.
func TestNew_ServerSet(t *testing.T) {
	modesOf := func(a device.Allocator) []workercore.DeviceAllocationMode {
		agg, ok := a.(aggregated)
		require.True(t, ok)
		modes := make([]workercore.DeviceAllocationMode, 0, len(agg.servers))
		for i := range agg.servers {
			srv, ok := agg.servers[i].(*server)
			require.True(t, ok)
			modes = append(modes, srv.AllocationMode)
		}
		return modes
	}

	cases := []struct {
		name string
		opts device.AllocatorOptions
		want []workercore.DeviceAllocationMode
	}{
		{
			name: "every family by default",
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-sliced drops only the logical slicing server",
			opts: device.AllocatorOptions{NoSliced: true},
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
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModeVisibility,
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, modesOf(New(c.opts)))
		})
	}

	// Iluvatar declares no partition kind, so it has no ".partitioned" resource name and registers
	// no partitioned server in any of the sets above.
	assert.Empty(t, nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned),
		"iluvatar declares no partition kind, so it registers no partitioned server")
}

// TestGetSlicedContainerAllocateResponse pins the single-card HAMi-core injection contract: the
// compute (SM) and per-card VRAM limits, the shared cache, the quiet-by-default log level, and the
// preload/lib mounts under ${OperatorLibDir}/iluvatar (one library, no CUDA-major subdir).
func TestGetSlicedContainerAllocateResponse(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	// 32 GiB card. cores=25% SM, memory=50% VRAM (independent dimensions).
	devs := iluvatarDevices(32768, testGPUUUID0)
	pod, ctr := slicedPod("pod-uid-1", "train", 25, 50)
	allocated := map[deviceplugin.Resource]int32{{Group: "bi150", Device: testGPUUUID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	// Envs.
	assert.Equal(t, testGPUUUID0, resp.Envs["IX_VISIBLE_DEVICES"])
	assert.Equal(t, "25", resp.Envs["CUDA_DEVICE_SM_LIMIT"]) // .sliced.cores-percentage
	assert.Equal(t, "/tmp/vgpu/cudevshr.cache", resp.Envs["CUDA_DEVICE_MEMORY_SHARED_CACHE"])
	assert.Equal(t, "0", resp.Envs["LIBCUDA_LOG_LEVEL"]) // quiet HAMi-core by default
	// floor(32768 MiB * 50%) = 16384 MiB.
	assert.Equal(t, "16384m", resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_0"])
	_, hasSecond := resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_1"]
	assert.False(t, hasSecond, "a logical slice is single-card, so no LIMIT_1")

	// Working dirs created (0777, forced by osx.MkdirAll regardless of umask).
	podWorkDir := deviceplugin.PodWorkDir("pod-uid-1", "train")
	for _, dir := range []string{hostVgpuLockPath, podWorkDir, filepath.Join(podWorkDir, "tmp/vgpu")} {
		info, statErr := os.Stat(dir)
		require.NoErrorf(t, statErr, "dir %s", dir)
		assert.Equalf(t, os.FileMode(0o777), info.Mode().Perm(), "mode of %s", dir)
	}

	// Mounts: one libvgpu.so directly under ${OperatorLibDir}/iluvatar, no CUDA-major subdir.
	libDir := filepath.Join(deviceplugin.OperatorLibDir, "iluvatar")
	byCtr := make(map[string]*deviceplugin.Mount, len(resp.Mounts))
	for _, m := range resp.Mounts {
		byCtr[m.ContainerPath] = m
	}
	cases := []struct {
		ctrPath, hostPath string
		readOnly          bool
	}{
		{ctrVgpuLockPath, hostVgpuLockPath, false},
		{ctrLdPreloadPath, filepath.Join(libDir, "ld.so.preload"), true},
		{ctrVgpuLibPath, filepath.Join(libDir, "libvgpu.so"), true},
		{ctrVgpuCacheDir, filepath.Join(podWorkDir, "tmp/vgpu"), false},
		{ctrDevShmPath, ctrDevShmPath, false},
	}
	for _, c := range cases {
		m, ok := byCtr[c.ctrPath]
		require.Truef(t, ok, "missing mount for %s", c.ctrPath)
		assert.Equalf(t, c.hostPath, m.HostPath, "host path for %s", c.ctrPath)
		assert.Equalf(t, c.readOnly, m.ReadOnly, "readOnly for %s", c.ctrPath)
	}
}

// A sliced container with no compute dimension (.sliced.cores-percentage absent) gets the whole
// card's 100% SM budget, mirroring SlicedCoresPercent's default.
func TestGetSlicedContainerAllocateResponse_DefaultCores(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-default-cores")},
		Spec: core.PodSpec{Containers: []core.Container{{
			Name: "train",
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{memPctRes: *resource.NewQuantity(50, resource.DecimalSI)},
			},
		}}},
	}
	devs := iluvatarDevices(32768, testGPUUUID0)
	allocated := map[deviceplugin.Resource]int32{{Group: "bi150", Device: testGPUUUID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, &pod.Spec.Containers[0], devs, allocated)
	require.NoError(t, err)
	assert.Equal(t, "100", resp.Envs["CUDA_DEVICE_SM_LIMIT"], "absent cores-percentage defaults to 100")
}

// A sliced container that declares LIBCUDA_LOG_LEVEL keeps its own value: the allocator
// must not inject the quiet default over it (the debugging escape hatch).
func TestGetSlicedContainerAllocateResponse_RespectsContainerLogLevel(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := iluvatarDevices(32768, testGPUUUID0)
	pod, ctr := slicedPod("pod-uid-loglevel", "train", 25, 50)
	ctr.Env = []core.EnvVar{{Name: "LIBCUDA_LOG_LEVEL", Value: "3"}}
	allocated := map[deviceplugin.Resource]int32{{Group: "bi150", Device: testGPUUUID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	_, injected := resp.Envs["LIBCUDA_LOG_LEVEL"]
	assert.False(t, injected, "must not override a container-declared LIBCUDA_LOG_LEVEL")
}

// A sliced container with no memory dimension (neither .sliced.memory-percentage nor
// .sliced.memory-mib) must be rejected rather than silently given the whole card.
func TestGetSlicedContainerAllocateResponse_NoMemoryRequest(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := iluvatarDevices(32768, testGPUUUID0)
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-x")},
		Spec:       core.PodSpec{Containers: []core.Container{{Name: "train"}}},
	}
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, &pod.Spec.Containers[0], devs,
		map[deviceplugin.Resource]int32{{Group: "bi150", Device: testGPUUUID0}: 1})
	require.Error(t, err)
}

// TestGetContainerAllocateResponse_Visibility verifies the visibility-mode responder emits only
// IX_VISIBLE_DEVICES for the allocated device(s) — the same plain device-visibility response as
// exclusive/shared — with no HAMi logical-slicing env or mounts.
func TestGetContainerAllocateResponse_Visibility(t *testing.T) {
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeVisibility,
		},
	}
	devs := iluvatarDevices(32768, testGPUUUID0, testGPUUUID1)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "sshd-pod", UID: types.UID("uid-vis")}}
	// Only the first card is reserved to the workload; visibility must scope to exactly it.
	allocated := map[deviceplugin.Resource]int32{{Group: "bi150", Device: testGPUUUID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs, allocated)
	require.NoError(t, err)

	assert.Equal(t, testGPUUUID0, resp.Envs["IX_VISIBLE_DEVICES"])
	assert.Len(t, resp.Envs, 1, "visibility emits only IX_VISIBLE_DEVICES")
	_, hasSM := resp.Envs["CUDA_DEVICE_SM_LIMIT"]
	assert.False(t, hasSM, "no HAMi compute limit")
	assert.Empty(t, resp.Mounts, "no HAMi preload/lib mounts")
}
