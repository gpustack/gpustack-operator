package nvidia

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

// nvidiaDevices builds a fixture with one group (runtimeVersion drives the cuda dir,
// memory drives the VRAM limit) holding the given GPU UUIDs.
func nvidiaDevices(runtimeVersion string, memoryMiB uint64, uuids ...string) *workercore.Devices {
	accels := make([]workercore.Accelerator, len(uuids))
	for i, u := range uuids {
		accels[i] = workercore.Accelerator{ID: u, Index: uint32(i)}
	}
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:             "a10g",
				Manufacturer:   Manufacturer,
				Memory:         memoryMiB,
				RuntimeVersion: runtimeVersion,
				Accelerators:   accels,
			}},
		},
	}
}

// slicedPod builds a sliced container requesting the decoupled compute and VRAM
// dimensions the allocator now reads: ".sliced.cores-percentage" (SM) and
// ".sliced.memory-percentage" (per-accelerator VRAM).
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

// TestNew_ServerSet pins which families NVIDIA registers a device-plugin server for, and that
// each control flag removes exactly its own. The hardware-partitioning server is additionally
// gated on the manufacturer having a partition kind: without one its resource name is empty, and
// registering a server that advertises an empty name to kubelet is worse than registering none.
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
				workercore.DeviceAllocationModePartitioned,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-partitioned drops only the partition server",
			opts: device.AllocatorOptions{NoPartitioned: true},
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
				workercore.DeviceAllocationModePartitioned,
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

	// The gate the partition server is registered behind, stated at its source: a manufacturer
	// with no partition kind has no ".partitioned" resource name at all.
	assert.NotEmpty(t, nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned),
		"nvidia declares a partition kind, so it serves the family")
	assert.Empty(t, nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerMThreads, workercore.DeviceAllocationModePartitioned),
		"a manufacturer with no partition kind yields no resource name, so it registers no server")
}

// TestNew_ReclaimLoopFollowsThePartitionServer pins that the MIG reclaim loop is gated on the
// server that creates the instances it frees. Gating it on the logical slicing server would run
// it with no partitions to reclaim, and stop it while partitions were still live.
func TestNew_ReclaimLoopFollowsThePartitionServer(t *testing.T) {
	withPartitions, ok := New(device.AllocatorOptions{NoSliced: true}).(aggregated)
	require.True(t, ok)
	assert.True(t, withPartitions.partitioned, "the reclaim loop runs while the partition server does")

	withoutPartitions, ok := New(device.AllocatorOptions{NoPartitioned: true}).(aggregated)
	require.True(t, ok)
	assert.False(t, withoutPartitions.partitioned, "no partition server, nothing to reclaim")
}

func TestGetSlicedContainerAllocateResponse(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	// A10G-like: 24576 MiB. cores=10% SM, memory=25% VRAM (independent dimensions).
	devs := nvidiaDevices("12.4", 24576, testGPUUUID0)
	pod, ctr := slicedPod("pod-uid-1", "train", 10, 25)
	allocated := map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	// Envs.
	assert.Equal(t, testGPUUUID0, resp.Envs["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "10", resp.Envs["CUDA_DEVICE_SM_LIMIT"]) // .sliced.cores-percentage
	assert.Equal(t, "/tmp/vgpu/cudevshr.cache", resp.Envs["CUDA_DEVICE_MEMORY_SHARED_CACHE"])
	assert.Equal(t, "0", resp.Envs["LIBCUDA_LOG_LEVEL"]) // quiet HAMi-core by default
	// The positional CUDA_DEVICE_MEMORY_LIMIT_<i> keys only address the intended card
	// when CUDA numbers devices the way NVML does.
	assert.Equal(t, "PCI_BUS_ID", resp.Envs["CUDA_DEVICE_ORDER"])
	// floor(24576 MiB * 25%) = 6144 MiB.
	assert.Equal(t, "6144m", resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_0"])
	_, hasSecond := resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_1"]
	assert.False(t, hasSecond, "single card must not emit LIMIT_1")

	// Working dirs created (0777, forced by osx.MkdirAll regardless of umask).
	podWorkDir := deviceplugin.PodWorkDir("pod-uid-1", "train")
	for _, dir := range []string{hostVgpuLockPath, podWorkDir, filepath.Join(podWorkDir, "tmp/vgpu")} {
		info, statErr := os.Stat(dir)
		require.NoErrorf(t, statErr, "dir %s", dir)
		assert.Equalf(t, os.FileMode(0o777), info.Mode().Perm(), "mode of %s", dir)
	}

	// Mounts.
	libDir := filepath.Join(deviceplugin.OperatorLibDir, "nvidia")
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
		{ctrVgpuLibPath, filepath.Join(libDir, "cuda-12/libvgpu.so"), true},
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

// A sliced container that declares one of the defaulted variables itself keeps its own
// value: the allocator must not inject over it — the quiet log level is the debugging
// escape hatch, the device order the workload's own call on how CUDA numbers its cards.
func TestGetSlicedContainerAllocateResponse_RespectsContainerDeclaredEnv(t *testing.T) {
	cases := []struct{ name, declared string }{
		{"LIBCUDA_LOG_LEVEL", "3"},
		{"CUDA_DEVICE_ORDER", "FASTEST_FIRST"},
	}
	for _, c := range cases {
		redirectLogicalSliceDirs(t)
		s := newSlicedServer()
		devs := nvidiaDevices("12.4", 24576, testGPUUUID0)
		pod, ctr := slicedPod("pod-uid-"+c.name, "train", 10, 25)
		ctr.Env = []core.EnvVar{{Name: c.name, Value: c.declared}}
		allocated := map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1}

		resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
		require.NoErrorf(t, err, "declared %s", c.name)

		_, injected := resp.Envs[c.name]
		assert.Falsef(t, injected, "must not override a container-declared %s", c.name)
	}
}

// One CUDA_DEVICE_MEMORY_LIMIT_<i> per allocated accelerator (.sliced accelerator count).
func TestGetSlicedContainerAllocateResponse_MultiCard(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := nvidiaDevices("12.4", 24576, testGPUUUID0, testGPUUUID1)
	pod, ctr := slicedPod("pod-uid-2", "train", 50, 25) // SM 50%, VRAM 25%
	allocated := map[deviceplugin.Resource]int32{
		{Group: "a10g", Device: testGPUUUID0}: 1,
		{Group: "a10g", Device: testGPUUUID1}: 1,
	}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	assert.Equal(t, testGPUUUID0+","+testGPUUUID1, resp.Envs["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "50", resp.Envs["CUDA_DEVICE_SM_LIMIT"]) // .sliced.cores-percentage
	// floor(24576 MiB * 25%) = 6144 MiB, one entry per accelerator.
	assert.Equal(t, "6144m", resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_0"])
	assert.Equal(t, "6144m", resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_1"])
}

// A positional CUDA_DEVICE_MEMORY_LIMIT_<i> is read by the container's own device number, so
// the entries must be emitted in the order the container numbers its cards — ascending
// recorded index — and not in the order the ledger happens to store them. The ledger groups
// by model and memory, so an allocation spanning two groups is where the two orders diverge:
// here the second group holds the lower-indexed card, and each group carries a different VRAM
// budget, so a group-ordered emission would cap each card at the other's budget.
func TestGetSlicedContainerAllocateResponse_MultiCardAcrossGroupsOrdersByIndex(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{
				{
					ID: "a10g", Manufacturer: Manufacturer, Memory: 24576, RuntimeVersion: "12.4",
					Accelerators: []workercore.Accelerator{{ID: testGPUUUID1, Index: 1}},
				},
				{
					ID: "l4", Manufacturer: Manufacturer, Memory: 8192, RuntimeVersion: "12.4",
					Accelerators: []workercore.Accelerator{{ID: testGPUUUID0, Index: 0}},
				},
			},
		},
	}
	pod, ctr := slicedPod("pod-uid-crossgroup", "train", 50, 25) // SM 50%, VRAM 25%
	allocated := map[deviceplugin.Resource]int32{
		{Group: "a10g", Device: testGPUUUID1}: 1,
		{Group: "l4", Device: testGPUUUID0}:   1,
	}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	assert.Equal(t, testGPUUUID0+","+testGPUUUID1, resp.Envs["NVIDIA_VISIBLE_DEVICES"])
	// Index 0 is the 8192 MiB card: floor(8192 * 25%) = 2048 MiB, and index 1 the 24576 MiB
	// one: floor(24576 * 25%) = 6144 MiB.
	assert.Equal(t, "2048m", resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_0"])
	assert.Equal(t, "6144m", resp.Envs["CUDA_DEVICE_MEMORY_LIMIT_1"])
}

// The libvgpu.so mount tracks the accelerator's CUDA runtime major (default cuda-12).
func TestGetSlicedContainerAllocateResponse_CUDADir(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()

	cases := []struct{ runtimeVersion, wantDir string }{
		{"13.0", "cuda-13"},
		{"12.4", "cuda-12"},
		{"", "cuda-12"}, // empty -> default
	}
	for _, c := range cases {
		devs := nvidiaDevices(c.runtimeVersion, 24576, testGPUUUID0)
		pod, ctr := slicedPod("uid-"+c.wantDir, "train", 10, 25)
		allocated := map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1}
		resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
		require.NoError(t, err)

		want := filepath.Join(deviceplugin.OperatorLibDir, "nvidia", c.wantDir, "libvgpu.so")
		var got string
		for _, m := range resp.Mounts {
			if m.ContainerPath == ctrVgpuLibPath {
				got = m.HostPath
			}
		}
		assert.Equalf(t, want, got, "runtimeVersion %q", c.runtimeVersion)
	}
}

// A sliced container with no memory dimension (neither .sliced.memory-percentage nor
// .sliced.memory-mib) must be rejected rather than silently given the whole accelerator.
func TestGetSlicedContainerAllocateResponse_NoMemoryRequest(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := nvidiaDevices("12.4", 24576, testGPUUUID0)
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-x")},
		Spec:       core.PodSpec{Containers: []core.Container{{Name: "train"}}},
	}
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, &pod.Spec.Containers[0], devs,
		map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1})
	require.Error(t, err)
}

// A single libvgpu is mounted, so a sliced allocation spanning GPUs with different
// CUDA majors must be rejected rather than mounting an incompatible library.
func TestGetSlicedContainerAllocateResponse_MixedCUDAMajorRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	// Two groups: cuda-12 (a10g) and cuda-13 (l4).
	devs := &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{
				{ID: "a10g", Manufacturer: Manufacturer, Memory: 24576, RuntimeVersion: "12.4", Accelerators: []workercore.Accelerator{{ID: testGPUUUID0, Index: 0}}},
				{ID: "l4", Manufacturer: Manufacturer, Memory: 24576, RuntimeVersion: "13.0", Accelerators: []workercore.Accelerator{{ID: testGPUUUID1, Index: 1}}},
			},
		},
	}
	pod, ctr := slicedPod("uid-mixed", "train", 50, 25) // both cards
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{
			{Group: "a10g", Device: testGPUUUID0}: 1,
			{Group: "l4", Device: testGPUUUID1}:   1,
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUDA majors")
}

func Test_nvidiaCUDADir(t *testing.T) {
	assert.Equal(t, "cuda-12", nvidiaCUDADir("12.4"))
	assert.Equal(t, "cuda-13", nvidiaCUDADir("13.0"))
	assert.Equal(t, "cuda-12", nvidiaCUDADir("")) // empty major defaults to cuda-12
}

// TestGetContainerAllocateResponse_Visibility verifies the visibility-mode responder emits
// only NVIDIA_VISIBLE_DEVICES for the allocated device(s) — the same plain device-visibility
// response as exclusive/shared — with no HAMi logical-slicing env or mounts.
func TestGetContainerAllocateResponse_Visibility(t *testing.T) {
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeVisibility,
		},
	}
	devs := nvidiaDevices("12.4", 24576, testGPUUUID0, testGPUUUID1)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "sshd-pod", UID: types.UID("uid-vis")}}
	// Only the first accelerator is reserved to the workload; visibility must scope to exactly it.
	allocated := map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs, allocated)
	require.NoError(t, err)

	assert.Equal(t, testGPUUUID0, resp.Envs["NVIDIA_VISIBLE_DEVICES"])
	assert.Len(t, resp.Envs, 1, "visibility emits only NVIDIA_VISIBLE_DEVICES")
	_, hasSM := resp.Envs["CUDA_DEVICE_SM_LIMIT"]
	assert.False(t, hasSM, "no HAMi compute limit")
	assert.Empty(t, resp.Mounts, "no HAMi preload/lib mounts")
}
