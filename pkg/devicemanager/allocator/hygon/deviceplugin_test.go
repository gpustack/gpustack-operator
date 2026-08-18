package hygon

import (
	"context"
	"os"
	"path/filepath"
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

const groupID = "z100"

// redirectLogicalSliceDirs points the per-container pods dir at a temp dir so the on-disk
// vdev.conf writes and scans hit a clean tree.
func redirectLogicalSliceDirs(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	origLib, origPods := deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir
	deviceplugin.OperatorLibDir = filepath.Join(root, "lib")
	deviceplugin.OperatorPodsDir = filepath.Join(root, "pods")
	t.Cleanup(func() {
		deviceplugin.OperatorLibDir = origLib
		deviceplugin.OperatorPodsDir = origPods
	})
}

func hygonDevicesFixture() *workercore.Devices {
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           groupID,
				Manufacturer: Manufacturer,
				Memory:       49152, // MiB (48 GiB)
				Cores:        64,
				Accelerators: []workercore.Accelerator{
					{ID: "dcu-uuid-0", Index: 0, PhysicalIndexes: []uint32{0, 128}, Topology: workercore.DeviceTopology{PciBusID: bdfA}},
					{ID: "dcu-uuid-1", Index: 1, PhysicalIndexes: []uint32{1, 129}, Topology: workercore.DeviceTopology{PciBusID: bdfB}},
				},
			}},
		},
	}
}

func slicedPod(uid, ctrName string, coresPct, memPct int64) (*core.Pod, *core.Container) {
	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	limits := core.ResourceList{}
	if coresPct > 0 {
		limits[coresRes] = *resource.NewQuantity(coresPct, resource.DecimalSI)
	}
	if memPct > 0 {
		limits[memPctRes] = *resource.NewQuantity(memPct, resource.DecimalSI)
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

func newSlicedServer() *server {
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeSliced,
		},
	}
}

// redirectDevicePaths points the device-node paths at a temporary tree holding the nodes the fixture
// accelerators name. The responder only stats these paths, so plain files stand in for character
// devices.
func redirectDevicePaths(t *testing.T) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "dev")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dri"), 0o755))

	origNode, origCard, origRender := nodeDevicePaths, cardDevicePathFormat, renderDevicePathFormat
	nodeDevicePaths = []string{filepath.Join(root, "kfd"), filepath.Join(root, "mkfd")}
	cardDevicePathFormat = filepath.Join(root, "dri", "card%d")
	renderDevicePathFormat = filepath.Join(root, "dri", "renderD%d")
	t.Cleanup(func() {
		nodeDevicePaths, cardDevicePathFormat, renderDevicePathFormat = origNode, origCard, origRender
	})

	paths := append([]string{}, nodeDevicePaths...)
	for _, minor := range []uint32{0, 1} {
		paths = append(paths, cardDevicePath(minor))
	}
	for _, minor := range []uint32{128, 129} {
		paths = append(paths, renderDevicePath(minor))
	}
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
}

// deviceHostPaths is what a response's device set names, in the order it carries them.
func deviceHostPaths(resp *deviceplugin.ContainerAllocateResponse) []string {
	paths := make([]string, 0, len(resp.Devices))
	for _, d := range resp.Devices {
		paths = append(paths, d.HostPath)
	}
	return paths
}

func mountByContainerPath(resp *deviceplugin.ContainerAllocateResponse, ctrPath string) (*deviceplugin.Mount, bool) {
	for _, m := range resp.Mounts {
		if m.ContainerPath == ctrPath {
			return m, true
		}
	}
	return nil, false
}

// An accelerator whose drm indexes the detector could not read must not take the device plugin down with
// it. The detector reads them from sysfs and records both numbers, the accelerator number alone, or
// nothing; this handler has no panic recovery, so indexing an absent one killed the process that
// serves every allocation on the node — for every manufacturer — over one unreadable directory.
// The nodes that cannot be named are left out instead.
func TestGetContainerAllocateResponseWithoutDrmIndexes(t *testing.T) {
	devs := hygonDevicesFixture()
	// Neither number readable, and only the accelerator number readable: the two shapes sysfs yields
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
		{Group: groupID, Device: "dcu-uuid-0"}: 1,
		{Group: groupID, Device: "dcu-uuid-1"}: 1,
	}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)
	require.NotNil(t, resp)
	for _, d := range resp.Devices {
		assert.NotContains(t, d.HostPath, "/dev/dri/renderD",
			"a renderD index the detector never recorded names no node")
	}
}

// A partial Hygon slice writes a vdev.conf carrying its CU bitmask + VRAM cap and mounts
// the per-pod vdev dir at /etc/vdev/docker/.
func TestGetSlicedContainerAllocateResponse(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := hygonDevicesFixture()

	pod, ctr := slicedPod("pod-uid-1", "train", 25, 50)
	allocated := map[deviceplugin.Resource]int32{{Group: groupID, Device: "dcu-uuid-0"}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	vdevHostDir := filepath.Join(deviceplugin.PodWorkDir("pod-uid-1", "train"), "etc/vdev/docker")
	confPath := filepath.Join(vdevHostDir, "vdev0.conf")
	body, err := os.ReadFile(confPath)
	require.NoError(t, err)
	// cores 25% of 64 -> 16 CU (0xffff); memory floor(49152*50%)=24576 MiB.
	want := "PciBusId: 0000:3d:00.0\n" +
		"cu_mask: 0x000000000000ffff\n" +
		"cu_mask: 0x0000000000000000\n" +
		"cu_count: 16\n" +
		"mem: 24576 MiB\n" +
		"device_id: 0\n" +
		"vdev_id: 0\n" +
		"pipe_id: 0\n" +
		"enable: 1\n"
	assert.Equal(t, want, string(body))

	info, err := os.Stat(confPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	m, ok := mountByContainerPath(resp, "/etc/vdev/docker")
	require.True(t, ok, "the per-pod vdev dir must be mounted at /etc/vdev/docker")
	assert.Equal(t, vdevHostDir, m.HostPath)
	assert.True(t, m.ReadOnly, "the tenant-facing slot ledger must be read-only")
}

// A whole-accelerator slice (100% compute, full VRAM) still writes a full-mask / full-memory
// vdev.conf occupancy marker, so the on-disk scanner never misses a taken accelerator.
func TestGetSlicedContainerAllocateResponse_WholeCardMarker(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := hygonDevicesFixture()

	pod, ctr := slicedPod("pod-uid-whole", "train", 100, 100)
	allocated := map[deviceplugin.Resource]int32{{Group: groupID, Device: "dcu-uuid-0"}: 1}

	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	confPath := filepath.Join(deviceplugin.PodWorkDir("pod-uid-whole", "train"), "etc/vdev/docker", "vdev0.conf")
	body, err := os.ReadFile(confPath)
	require.NoError(t, err)
	want := "PciBusId: 0000:3d:00.0\n" +
		"cu_mask: 0xffffffffffffffff\n" +
		"cu_mask: 0x0000000000000000\n" +
		"cu_count: 64\n" +
		"mem: 49152 MiB\n" +
		"device_id: 0\n" +
		"vdev_id: 0\n" +
		"pipe_id: 0\n" +
		"enable: 1\n"
	assert.Equal(t, want, string(body))
}

// A multi-accelerator allocation writes one vdev<i>.conf per accelerator, each independently slotted:
// the node-wide vdev_id climbs while the per-accelerator pipe_id resets on the second accelerator.
func TestGetSlicedContainerAllocateResponse_MultiCard(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := hygonDevicesFixture()

	pod, ctr := slicedPod("pod-uid-multi", "train", 25, 50)
	allocated := map[deviceplugin.Resource]int32{
		{Group: groupID, Device: "dcu-uuid-0"}: 1,
		{Group: groupID, Device: "dcu-uuid-1"}: 1,
	}

	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	vdevHostDir := filepath.Join(deviceplugin.PodWorkDir("pod-uid-multi", "train"), "etc/vdev/docker")
	c0, err := parseVdevConf(filepath.Join(vdevHostDir, "vdev0.conf"))
	require.NoError(t, err)
	c1, err := parseVdevConf(filepath.Join(vdevHostDir, "vdev1.conf"))
	require.NoError(t, err)

	assert.Equal(t, bdfA, c0.pciBusID)
	assert.Equal(t, 0, c0.deviceID)
	assert.Equal(t, bdfB, c1.pciBusID)
	assert.Equal(t, 1, c1.deviceID, "the second card is container-local index 1")
	assert.Equal(t, 1, c1.vdevID, "vdev_id is node-wide")
	assert.Equal(t, 0, c1.pipeID, "pipe_id resets on a different card")
}

// A sliced container with no memory dimension is rejected rather than silently given the
// whole accelerator's VRAM.
func TestGetSlicedContainerAllocateResponse_NoMemory(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := hygonDevicesFixture()

	pod, ctr := slicedPod("pod-uid-nomem", "train", 25, 0)
	allocated := map[deviceplugin.Resource]int32{{Group: groupID, Device: "dcu-uuid-0"}: 1}

	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.Error(t, err)
}

// The device-node paths, asserted without the redirect the cases below install: those prove the
// responder composes the set correctly, and only this one proves the set names the host's real nodes
// rather than a temporary tree.
func TestDevicePaths(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "the accelerator's own drm nodes",
			got:  []string{cardDevicePath(3), renderDevicePath(131)},
			want: []string{"/dev/dri/card3", "/dev/dri/renderD131"},
		},
		{
			name: "the node-level control nodes, which no drm minor names",
			got:  nodeDevicePaths,
			want: []string{"/dev/kfd", "/dev/mkfd"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.got)
		})
	}
}

// Neither the control devices nor the runtime mount is an accelerator, so a grant that resolved to
// none of them is a success the container cannot use. The sliced path already refused this shape; the
// whole-card path now does too.
func TestGetContainerAllocateResponseRefusesAnEmptyGrant(t *testing.T) {
	redirectDevicePaths(t)
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
	}
	pod, ctr := slicedPod("pod-empty-grant", "ctr", 0, 0)

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr,
		hygonDevicesFixture(), map[deviceplugin.Resource]int32{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no allocated accelerator found")
	assert.Nil(t, resp)
}

// The device set a grant carries: the control nodes once however many accelerators were granted, then
// each accelerator's own drm pair, in the order they were granted. The whole-card and the sliced path
// render it from the same helpers, so both are asserted here.
func TestDeviceSet(t *testing.T) {
	cases := []struct {
		name    string
		mode    workercore.DeviceAllocationMode
		granted []string
		want    []string
	}{
		{
			name:    "one accelerator, whole card",
			mode:    workercore.DeviceAllocationModeExclusive,
			granted: []string{"dcu-uuid-1"},
			want:    []string{"kfd", "mkfd", "dri/card1", "dri/renderD129"},
		},
		{
			name:    "two accelerators share one pair of control nodes",
			mode:    workercore.DeviceAllocationModeShared,
			granted: []string{"dcu-uuid-0", "dcu-uuid-1"},
			want:    []string{"kfd", "mkfd", "dri/card0", "dri/renderD128", "dri/card1", "dri/renderD129"},
		},
		{
			name:    "a slice reaches its accelerator through the same nodes",
			mode:    workercore.DeviceAllocationModeSliced,
			granted: []string{"dcu-uuid-0"},
			want:    []string{"kfd", "mkfd", "dri/card0", "dri/renderD128"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			redirectDevicePaths(t)

			s := &server{
				ResourceServer: deviceplugin.ResourceServer{
					Logger:         logr.Discard(),
					Manufacturer:   Manufacturer,
					AllocationMode: c.mode,
				},
			}
			pod, ctr := slicedPod("pod-device-set", "ctr", 50, 50)
			allocated := make(map[deviceplugin.Resource]int32, len(c.granted))
			for _, id := range c.granted {
				allocated[deviceplugin.Resource{Group: groupID, Device: id}] = 1
			}

			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr,
				hygonDevicesFixture(), allocated)
			require.NoError(t, err)

			want := make([]string, 0, len(c.want))
			for _, rel := range c.want {
				switch rel {
				case "kfd":
					want = append(want, nodeDevicePaths[0])
				case "mkfd":
					want = append(want, nodeDevicePaths[1])
				default:
					want = append(want, filepath.Join(filepath.Dir(nodeDevicePaths[0]), rel))
				}
			}
			assert.Equal(t, want, deviceHostPaths(resp))
		})
	}
}
