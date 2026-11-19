package ascend

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
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
	"gpustack.ai/gpustack/pkg/utils/hostfs"
)

const (
	testAccelID0 = "E0F4EE64 802061B1 6A691492 89528485 104301E3"
	testAccelID1 = "E281A66C 140C979 2CFBED72 A4500485 104301E3"
)

// redirectLogicalSliceDirs points the logical-slicing host paths at a temp dir for the test.
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

func ascendDevicesFixture() *workercore.Devices {
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:             "910b2",
				Manufacturer:   Manufacturer,
				Memory:         65536, // MiB
				RuntimeVersion: "8.5",
				Family:         "910B",
				// PhysicalIndexes is the detector's {physical id, card id, device id in card}:
				// the physical id names the accelerator a visibility env or a quota config
				// carries, while the card/device pair addresses the container-share flag. Those
				// three and the logical Index are given deliberately distinct values, because the
				// detector derives them from different counters -- Index skips an accelerator
				// that failed detection, PhysicalIndexes carries dcmi's own numbers -- so a
				// fixture where they agreed would let the responder read the wrong one and pass.
				Accelerators: []workercore.Accelerator{
					{ID: testAccelID0, Index: 0, PhysicalIndexes: []uint32{3, 0, 0}},
					{ID: testAccelID1, Index: 1, PhysicalIndexes: []uint32{7, 1, 0}},
				},
			}},
		},
	}
}

// slicedPod builds a pending pod whose container requests the decoupled compute and
// VRAM dimensions the allocator reads: ".sliced.cores-percentage" (aicore) and
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
	return newSlicedServerWithShare(&fakeShareDriver{enabled: map[[2]int32]bool{{0, 0}: true, {1, 0}: true}})
}

// newSlicedServerWithShare builds the sliced responder over a caller-supplied container-share
// driver, which the caller keeps a reference to in order to assert what the responder did to it.
func newSlicedServerWithShare(share shareDriver) *server {
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			// Production always hands the responder a logger, and the preflight reports through it,
			// so the fake carries one too rather than leaving the zero value behind.
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeSliced,
		},
		share:   share,
		product: newFakeProductResolver(),
	}
}

func TestGetSlicedContainerAllocateResponse(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()

	// Allocate cores=10% aicore + memory=25% VRAM (independent dimensions) of accelerator index 0.
	pod, ctr := slicedPod("pod-uid-1", "train", 10, 25)
	allocated := map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	// Env: the accelerator's driver index, 3 in the fixture, not its logical Index of 0.
	assert.Equal(t, "3", resp.Envs["ASCEND_VISIBLE_DEVICES"])
	assert.Equal(t, "1", resp.Envs["ENPU_LOG_LEVEL"]) // quiet vcann-rt by default
	assert.Equal(t, "1", resp.Envs["ENPU_DSMI_HOOK"]) // slice visible in npu-smi by default

	// npu_info.config: aicore 10% (.sliced.cores-percentage), memory floor(65536*25%)=16384MiB.
	podWorkDir := deviceplugin.PodWorkDir("pod-uid-1", "train")
	configPath := filepath.Join(podWorkDir, "etc/enpu/vcann-rt/npu_info.config")
	body, err := os.ReadFile(configPath)
	require.NoError(t, err)
	want := "physical-npu-id=3\n" +
		"virtual-npu-id=0\n" +
		"aicore-quota=10\n" +
		"memory-quota=16384\n" +
		"shm-id=E0F4EE64-802061B1-6A691492-89528485-104301E3\n" +
		"scheduling-policy=2\n"
	assert.Equal(t, want, string(body))

	// Config file mode is 0644.
	info, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	// Mounts: preload (ro), libvruntime (ro), enpu-monitor (ro), config (ro), /dev/shm (rw).
	libDir := filepath.Join(deviceplugin.OperatorLibDir, "ascend")
	byContainerPath := make(map[string]*deviceplugin.Mount, len(resp.Mounts))
	for _, m := range resp.Mounts {
		byContainerPath[m.ContainerPath] = m
	}
	cases := []struct {
		ctrPath  string
		hostPath string
		readOnly bool
	}{
		{ctrLdPreloadPath, filepath.Join(libDir, "ld.so.preload"), true},
		{ctrVruntimePath, filepath.Join(libDir, "cann-8-910b/lib/libvruntime.so"), true},
		{ctrMonitorPath, filepath.Join(libDir, "cann-8-910b/tools/enpu-monitor"), true},
		{ctrConfigPath, configPath, true},
		{ctrDevShmPath, ctrDevShmPath, false},
	}
	for _, c := range cases {
		m, ok := byContainerPath[c.ctrPath]
		require.Truef(t, ok, "missing mount for %s", c.ctrPath)
		assert.Equalf(t, c.hostPath, m.HostPath, "host path for %s", c.ctrPath)
		assert.Equalf(t, c.readOnly, m.ReadOnly, "readOnly for %s", c.ctrPath)
	}
}

// A sliced container that declares ENPU_LOG_LEVEL keeps its own value: the allocator
// must not inject the quiet default over it (the debugging escape hatch).
func TestGetSlicedContainerAllocateResponse_RespectsContainerLogLevel(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()
	pod, ctr := slicedPod("uid-loglevel", "train", 10, 25)
	ctr.Env = []core.EnvVar{{Name: "ENPU_LOG_LEVEL", Value: "3"}}
	allocated := map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	_, injected := resp.Envs["ENPU_LOG_LEVEL"]
	assert.False(t, injected, "must not override a container-declared ENPU_LOG_LEVEL")
}

// ENPU_DSMI_HOOK turns the npu-smi slice view on. The allocator injects the enabled
// default, but a container that declares the variable owns it — whatever the value,
// since the library, not the allocator, decides what a value means.
func TestGetSlicedContainerAllocateResponse_DsmiHookEnv(t *testing.T) {
	cases := []struct {
		name     string
		declared string // "" == the container declares nothing
		want     string // only meaningful when wantInjected
		// Asserted separately from want: an absent key and an injected empty value both
		// read as "" out of the map, and only one of them honors the contract.
		wantInjected bool
	}{
		{name: "not declared", declared: "", wantInjected: true, want: "1"},
		{name: "opted out", declared: "0", wantInjected: false},
		{name: "opted in", declared: "1", wantInjected: false},
		{name: "unparsable", declared: "yes-please", wantInjected: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			s := newSlicedServer()
			pod, ctr := slicedPod("uid-dsmi-"+c.name, "train", 10, 25)
			if c.declared != "" {
				ctr.Env = []core.EnvVar{{Name: "ENPU_DSMI_HOOK", Value: c.declared}}
			}

			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, ascendDevicesFixture(),
				map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
			require.NoError(t, err)

			got, injected := resp.Envs["ENPU_DSMI_HOOK"]
			require.Equal(t, c.wantInjected, injected, "allocator injected ENPU_DSMI_HOOK")
			if c.wantInjected {
				assert.Equal(t, c.want, got)
			}
		})
	}
}

// Two concurrent slices on the same physical NPU must get distinct virtual-npu-ids;
// a slice on a different physical NPU starts back at 0.
func TestSlicedVirtualNPUIDAssignment(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()

	readVNPU := func(uid, ctr string) int {
		_, vnpu, ok := parseNPUInfoConfig(
			filepath.Join(deviceplugin.PodWorkDir(uid, ctr), "etc/enpu/vcann-rt/npu_info.config"),
		)
		require.True(t, ok)
		return vnpu
	}

	// First slice on physical NPU 3 -> vnpu 0.
	p1, c1 := slicedPod("uid-a", "train", 10, 25)
	_, err := s.GetContainerAllocateResponse(context.Background(), p1, c1, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, readVNPU("uid-a", "train"))

	// Second slice on the SAME physical NPU 3 -> vnpu 1 (lowest free).
	p2, c2 := slicedPod("uid-b", "train", 10, 25)
	_, err = s.GetContainerAllocateResponse(context.Background(), p2, c2, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, readVNPU("uid-b", "train"))

	// Slice on a DIFFERENT physical NPU (driver index 7) -> vnpu 0 again.
	p3, c3 := slicedPod("uid-c", "train", 10, 25)
	_, err = s.GetContainerAllocateResponse(context.Background(), p3, c3, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID1}: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, readVNPU("uid-c", "train"))

	// Re-allocating the first container is idempotent -> still vnpu 0.
	_, err = s.GetContainerAllocateResponse(context.Background(), p1, c1, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, readVNPU("uid-a", "train"))
}

// A sliced container with no memory dimension (neither .sliced.memory-percentage nor
// .sliced.memory-mib) must be rejected rather than silently given the whole accelerator.
func TestGetSlicedContainerAllocateResponse_NoMemoryRequest(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()

	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-x")},
		Spec:       core.PodSpec{Containers: []core.Container{{Name: "train"}}},
	}
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, &pod.Spec.Containers[0], devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.Error(t, err)
}

// vcann-rt is single-NPU: a multi-accelerator sliced allocation must be rejected rather than
// silently quota-isolating only the first accelerator.
func TestGetSlicedContainerAllocateResponse_MultiCardRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()

	pod, ctr := slicedPod("uid-multi", "train", 10, 25)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{
			{Group: "910b2", Device: testAccelID0}: 1,
			{Group: "910b2", Device: testAccelID1}: 1,
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-NPU")
}

// TestGetContainerAllocateResponse_Visibility verifies the visibility-mode responder emits
// only ASCEND_VISIBLE_DEVICES with the exact allocated index(es) — the same plain
// device-visibility response as exclusive/shared, and never `all` (Ascend has no wildcard)
// — with no vcann-rt logical-slicing env or mounts.
func TestGetContainerAllocateResponse_Visibility(t *testing.T) {
	// The driver-tree mount is pinned by TestGetContainerAllocateResponse_MountsAscendDriver;
	// this case stands a host without the tree so "no vcann-rt mounts" stays the exact answer
	// whatever the host this suite runs on carries.
	absentAscendDriverHostPath(t)
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeVisibility,
		},
		// Visibility co-allocates a second container onto its owner's accelerator, so it drives the
		// container-share seam too; here the flag is already on and only read.
		share: &fakeShareDriver{enabled: map[[2]int32]bool{{0, 0}: true}},
	}
	devs := ascendDevicesFixture()
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "sshd-pod", UID: types.UID("uid-vis")}}
	// Only the first NPU is reserved to the workload; visibility must scope to exactly its index.
	allocated := map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs, allocated)
	require.NoError(t, err)

	assert.Equal(t, "3", resp.Envs["ASCEND_VISIBLE_DEVICES"], "exact allocated accelerator, never `all`")
	assert.Len(t, resp.Envs, 1, "visibility emits only ASCEND_VISIBLE_DEVICES")
	assert.Empty(t, resp.Mounts, "no vcann-rt preload/lib mounts")
}

// Ascend numbers one accelerator two ways, and which number ASCEND_VISIBLE_DEVICES carries depends
// on the generation: everywhere but A5 the vendor runtime resolves the physical id, while on A5 the
// vendor's own device plugin converts to the dcmi device (logic) id before emitting the env, and
// both of the runtime's injection paths then name /dev/davinci<N> from that value verbatim. The
// accelerator is given distinct numbers in both slots, so a responder reading the wrong one cannot
// pass.
func TestGetContainerAllocateResponse_VisibleDevicesPerFamily(t *testing.T) {
	cases := []struct {
		name   string
		family string
		want   string
	}{
		{name: "910B renders the physical id", family: "910B", want: "7"},
		{name: "950 renders the dcmi device id", family: "950", want: "3"},
	}
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeVisibility,
	}
	for _, c := range cases {
		for _, mode := range modes {
			t.Run(c.name+"/"+mode.String(), func(t *testing.T) {
				s := &server{
					ResourceServer: deviceplugin.ResourceServer{
						Logger:         logr.Discard(),
						Manufacturer:   Manufacturer,
						AllocationMode: mode,
					},
					share:   &fakeShareDriver{enabled: map[[2]int32]bool{{3, 0}: true}},
					product: newFakeProductResolver(),
				}
				devs := ascendDevicesFixture()
				devs.Spec.Groups[0].Family = c.family
				devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{7, 3, 0}

				pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-visible-" + c.family)}}
				resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs,
					map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
				require.NoError(t, err)

				assert.Equal(t, c.want, resp.Envs["ASCEND_VISIBLE_DEVICES"])
			})
		}
	}
}

// A sliced allocation carries both numbers at once, and they are not interchangeable: the env is
// what the vendor runtime resolves, while npu_info.config's physical-npu-id is what vcann-rt keys
// its quota by. On A5 only the first moves to the dcmi device id.
func TestGetSlicedContainerAllocateResponse_A5EnvAndQuotaUseDifferentIDs(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServerWithShare(&fakeShareDriver{enabled: map[[2]int32]bool{{3, 0}: true}})

	devs := ascendDevicesFixture()
	devs.Spec.Groups[0].Family = "950"
	devs.Spec.Groups[0].RuntimeVersion = "9.0"
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{7, 3, 0}

	pod, ctr := slicedPod("uid-a5-sliced", "train", 10, 25)
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)

	assert.Equal(t, "3", resp.Envs["ASCEND_VISIBLE_DEVICES"], "the runtime resolves the dcmi device id on A5")

	body, err := os.ReadFile(filepath.Join(
		deviceplugin.PodWorkDir("uid-a5-sliced", "train"), "etc/enpu/vcann-rt/npu_info.config",
	))
	require.NoError(t, err)
	assert.Contains(t, string(body), "physical-npu-id=7\n", "vcann-rt still keys its quota by the physical id")
}

// A 950 accelerator recording only its physical id carries no number the vendor runtime could
// resolve, so the allocation is refused rather than served the physical id under another name.
func TestGetContainerAllocateResponse_A5MissingDeviceIndexRejected(t *testing.T) {
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
	}
	devs := ascendDevicesFixture()
	devs.Spec.Groups[0].Family = "950"
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{7}

	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a5-noslot")}}
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carries no dcmi device index")
}

// The detector always records dcmi's addressing, so an accelerator without it is malformed. Every
// mode rejects it before touching the host, rather than naming an accelerator by a guessed number.
func TestGetContainerAllocateResponse_MissingDriverIndexRejected(t *testing.T) {
	for _, mode := range []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeVisibility,
		workercore.DeviceAllocationModeSliced,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			share := &fakeShareDriver{enabled: map[[2]int32]bool{{0, 0}: true}}
			s := &server{
				ResourceServer: deviceplugin.ResourceServer{
					Logger:         logr.Discard(),
					Manufacturer:   Manufacturer,
					AllocationMode: mode,
				},
				share: share,
			}
			devs := ascendDevicesFixture()
			devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil

			uid := "uid-noindex-" + mode.String()
			pod, ctr := slicedPod(uid, "train", 10, 25)
			_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
				map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "carries no dcmi physical index")

			assert.Empty(t, share.getCalls, "a malformed record is rejected before the flag is read")
			assert.Empty(t, share.setCalls, "and before it is written")
			// The work dir is created before the quota config is written into it, so asserting the
			// dir away covers both: nothing at all reached the host.
			_, statErr := os.Stat(deviceplugin.PodWorkDir(uid, "train"))
			assert.True(t, os.IsNotExist(statErr), "and before any host state is written")
		})
	}
}

// A request that is malformed in both ways at once -- an accelerator with no driver index, and no
// memory dimension -- reports the record, not the request. Both manufacturers order it that way, so
// one defect always produces one message wherever it is hit.
func TestGetSlicedContainerAllocateResponse_MissingDriverIndexOutranksMissingMemory(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := newSlicedServer()

	devs := ascendDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil

	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-both-bad")},
		Spec:       core.PodSpec{Containers: []core.Container{{Name: "train"}}}, // no memory dimension
	}
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, &pod.Spec.Containers[0], devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carries no dcmi physical index")
}

func Test_ascendCANNDir(t *testing.T) {
	cases := []struct {
		ver, family, want string
	}{
		{ver: "8.5", family: "910B", want: "cann-8-910b"},
		{ver: "9.0", family: "910C", want: "cann-9-910c"},
		{ver: "9.0", family: "950", want: "cann-9-950"},
		{ver: "", family: "910B", want: "cann-8-910b"}, // empty major defaults to cann-8
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ascendCANNDir(c.ver, c.family))
	}
}

// redirectAscendDriverHostPath stands the driver tree where the responder's existence check
// stats it, so the mount decision is exercised for real without touching the host's own
// /usr/local/Ascend/driver. Callers wanting a host without the tree point the path at a
// directory that was never created instead.
func redirectAscendDriverHostPath(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "driver")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	orig := ascendDriverHostPath
	ascendDriverHostPath = dir
	t.Cleanup(func() { ascendDriverHostPath = orig })

	return dir
}

// absentAscendDriverHostPath points the driver tree at a path that does not exist, standing a
// host that carries no /usr/local/Ascend/driver for the responder to stat.
func absentAscendDriverHostPath(t *testing.T) {
	t.Helper()

	orig := ascendDriverHostPath
	ascendDriverHostPath = filepath.Join(t.TempDir(), "driver", "absent")
	t.Cleanup(func() { ascendDriverHostPath = orig })
}

// driverMountOf returns the response's driver-tree mount, or nil when it carries none.
func driverMountOf(resp *deviceplugin.ContainerAllocateResponse) *deviceplugin.Mount {
	for _, m := range resp.Mounts {
		if m.ContainerPath == ascendDriverHostPath {
			return m
		}
	}
	return nil
}

// TestGetContainerAllocateResponse_MountsAscendDriver pins the driver-tree mount: HIXL and HCCL
// read /usr/local/Ascend/driver directly and ascend-docker-runtime does not mount it in full, so
// every mode a container is served in carries the tree read-only, and a host without the tree
// carries no mount rather than an error.
func TestGetContainerAllocateResponse_MountsAscendDriver(t *testing.T) {
	cases := []struct {
		name       string
		mode       workercore.DeviceAllocationMode
		driverPath func(t *testing.T) string
	}{
		{
			name:       "an exclusive container carries the driver tree read-only",
			mode:       workercore.DeviceAllocationModeExclusive,
			driverPath: redirectAscendDriverHostPath,
		},
		{
			name:       "a visibility container carries the driver tree read-only",
			mode:       workercore.DeviceAllocationModeVisibility,
			driverPath: redirectAscendDriverHostPath,
		},
		{
			name:       "a sliced container carries the driver tree read-only",
			mode:       workercore.DeviceAllocationModeSliced,
			driverPath: redirectAscendDriverHostPath,
		},
		{
			name: "a host without the driver tree gets no mount",
			mode: workercore.DeviceAllocationModeExclusive,
			driverPath: func(t *testing.T) string {
				absentAscendDriverHostPath(t)
				return ascendDriverHostPath
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			driverDir := c.driverPath(t)

			var (
				s   *server
				pod *core.Pod
				ctr *core.Container
			)
			if c.mode == workercore.DeviceAllocationModeSliced {
				redirectLogicalSliceDirs(t)
				s = newSlicedServer()
				pod, ctr = slicedPod("uid-driver-"+c.mode.String(), "train", 10, 25)
			} else {
				s = &server{
					ResourceServer: deviceplugin.ResourceServer{
						Logger:         logr.Discard(),
						Manufacturer:   Manufacturer,
						AllocationMode: c.mode,
					},
					share:   &fakeShareDriver{enabled: map[[2]int32]bool{{3, 0}: true}},
					product: newFakeProductResolver(),
				}
				pod = &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-driver-" + c.mode.String())}}
			}

			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, ascendDevicesFixture(),
				map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
			require.NoError(t, err)

			m := driverMountOf(resp)
			if filepath.Base(driverDir) == "absent" {
				assert.Nil(t, m, "a host without the driver tree must carry no mount")
				return
			}
			require.NotNil(t, m, "the response carries no driver-tree mount")
			assert.Equal(t, driverDir, m.ContainerPath)
			assert.Equal(t, driverDir, m.HostPath)
			assert.True(t, m.ReadOnly, "the driver tree is mounted read-only")
		})
	}
}

func makeFakeHostRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"etc", "proc", "usr"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	return root
}

func ascend950DevicesFixture() *workercore.Devices {
	devs := ascendDevicesFixture()
	devs.Spec.Groups[0].ID = "950"
	devs.Spec.Groups[0].Family = family950
	devs.Spec.Groups[0].RuntimeVersion = "9.0"
	devs.Spec.Groups[0].Accelerators = []workercore.Accelerator{
		{ID: testAccelID0, Index: 0, PhysicalIndexes: []uint32{7, 3, 0}},
		{ID: testAccelID1, Index: 1, PhysicalIndexes: []uint32{8, 4, 0}},
	}
	return devs
}

func mountsMatching(resp *deviceplugin.ContainerAllocateResponse, target string) []*deviceplugin.Mount {
	var out []*deviceplugin.Mount
	for _, m := range resp.Mounts {
		if m.ContainerPath == target {
			out = append(out, m)
		}
	}
	return out
}

func TestGetContainerAllocateResponse_Ascend950HostMounts_AllModesAndMergedUsr(t *testing.T) {
	libLayouts := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "plain /lib directory",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "lib"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "lib", "route.conf"), []byte("route=1\n"), 0o644))
			},
		},
		{
			name: "merged-usr absolute symlink /lib -> /usr/lib",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "lib"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "usr", "lib", "route.conf"), []byte("route=1\n"), 0o644))
				require.NoError(t, os.Symlink("/usr/lib", filepath.Join(root, "lib")))
			},
		},
		{
			name: "merged-usr relative symlink /lib -> usr/lib",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "lib"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "usr", "lib", "route.conf"), []byte("route=1\n"), 0o644))
				require.NoError(t, os.Symlink("usr/lib", filepath.Join(root, "lib")))
			},
		},
	}

	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
		workercore.DeviceAllocationModeVisibility,
	}

	for _, layout := range libLayouts {
		for _, mode := range modes {
			t.Run(layout.name+"/"+mode.String(), func(t *testing.T) {
				absentAscendDriverHostPath(t)
				root := makeFakeHostRoot(t)
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(rankTableV20), 0o644))
				layout.setup(t, root)

				hfs, err := hostfs.OpenHostRoot(root)
				require.NoError(t, err)
				t.Cleanup(func() { _ = hfs.Close() })

				var (
					s   *server
					pod *core.Pod
					ctr *core.Container
				)
				if mode == workercore.DeviceAllocationModeSliced {
					redirectLogicalSliceDirs(t)
					s = newSlicedServerWithShare(&fakeShareDriver{enabled: map[[2]int32]bool{{3, 0}: true}})
					s.hostFS = hfs
					pod, ctr = slicedPod("uid-a5-mounts", "train", 10, 25)
				} else {
					s = &server{
						ResourceServer: deviceplugin.ResourceServer{
							Logger:         logr.Discard(),
							Manufacturer:   Manufacturer,
							AllocationMode: mode,
						},
						share:   &fakeShareDriver{enabled: map[[2]int32]bool{{3, 0}: true}},
						product: newFakeProductResolver(),
						hostFS:  hfs,
					}
					pod = &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a5-mounts")}}
				}

				resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, ascend950DevicesFixture(),
					map[deviceplugin.Resource]int32{{Group: "950", Device: testAccelID0}: 1})
				require.NoError(t, err)

				for _, wantPath := range []string{hcclRootInfoPath, routeConfPath} {
					ms := mountsMatching(resp, wantPath)
					require.Len(t, ms, 1, "expected exactly one mount for %s", wantPath)
					assert.Equal(t, wantPath, ms[0].HostPath)
					assert.Equal(t, wantPath, ms[0].ContainerPath)
					assert.True(t, ms[0].ReadOnly)
				}
			})
		}
	}
}

func TestGetContainerAllocateResponse_Ascend950HostMounts_MultiCardDedup(t *testing.T) {
	absentAscendDriverHostPath(t)
	root := makeFakeHostRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(rankTableV20), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "lib", "route.conf"), []byte("route=1\n"), 0o644))

	hfs, err := hostfs.OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hfs.Close() })

	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
		product: newFakeProductResolver(),
		hostFS:  hfs,
	}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a5-multi")}}
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, ascend950DevicesFixture(),
		map[deviceplugin.Resource]int32{
			{Group: "950", Device: testAccelID0}: 1,
			{Group: "950", Device: testAccelID1}: 1,
		})
	require.NoError(t, err)

	assert.Len(t, mountsMatching(resp, hcclRootInfoPath), 1)
	assert.Len(t, mountsMatching(resp, routeConfPath), 1)
}

func TestGetContainerAllocateResponse_Ascend950HostMounts_ValidationAndSkipCases(t *testing.T) {
	cases := []struct {
		name          string
		setup         func(t *testing.T, root string)
		wantRankTable bool
		wantRouteConf bool
	}{
		{
			name:          "both absent",
			setup:         func(t *testing.T, root string) {},
			wantRankTable: false,
			wantRouteConf: false,
		},
		{
			name: "invalid JSON ranktable skipped while valid route.conf mounted",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte("not json"), 0o644))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "lib"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "lib", "route.conf"), []byte("ok"), 0o644))
			},
			wantRankTable: false,
			wantRouteConf: true,
		},
		{
			name: "missing version ranktable skipped",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(`{"status":"completed"}`), 0o644))
			},
			wantRankTable: false,
			wantRouteConf: false,
		},
		{
			name: "v1.0 ranktable skipped",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(rankTableV10), 0o644))
			},
			wantRankTable: false,
			wantRouteConf: false,
		},
		{
			name: "v1.2 ranktable skipped",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(`{"version":"1.2"}`), 0o644))
			},
			wantRankTable: false,
			wantRouteConf: false,
		},
		{
			name: "oversized ranktable (> 1 MiB) skipped",
			setup: func(t *testing.T, root string) {
				oversized := `{"version":"2.0","pad":"` + strings.Repeat("a", int(maxRankTableBytes)+16) + `"}`
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(oversized), 0o644))
			},
			wantRankTable: false,
			wantRouteConf: false,
		},
		{
			name: "leaf symlinks skipped for both files",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "real.json"), []byte(rankTableV20), 0o644))
				require.NoError(t, os.Symlink("real.json", filepath.Join(root, "etc", "hccl_rootinfo.json")))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "lib"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "lib", "real.conf"), []byte("ok"), 0o644))
				require.NoError(t, os.Symlink("real.conf", filepath.Join(root, "lib", "route.conf")))
			},
			wantRankTable: false,
			wantRouteConf: false,
		},
		{
			name: "directories skipped for both files",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "etc", "hccl_rootinfo.json"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "lib", "route.conf"), 0o755))
			},
			wantRankTable: false,
			wantRouteConf: false,
		},
		{
			name: "non-directory /lib fails Lstat cleanly without failing allocation",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(rankTableV20), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(root, "lib"), []byte("not a dir"), 0o644))
			},
			wantRankTable: true,
			wantRouteConf: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			absentAscendDriverHostPath(t)
			root := makeFakeHostRoot(t)
			c.setup(t, root)

			hfs, err := hostfs.OpenHostRoot(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = hfs.Close() })

			s := &server{
				ResourceServer: deviceplugin.ResourceServer{
					Logger:         logr.Discard(),
					Manufacturer:   Manufacturer,
					AllocationMode: workercore.DeviceAllocationModeExclusive,
				},
				product: newFakeProductResolver(),
				hostFS:  hfs,
			}
			pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a5-skip")}}
			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, ascend950DevicesFixture(),
				map[deviceplugin.Resource]int32{{Group: "950", Device: testAccelID0}: 1})
			require.NoError(t, err)

			if c.wantRankTable {
				assert.Len(t, mountsMatching(resp, hcclRootInfoPath), 1)
			} else {
				assert.Empty(t, mountsMatching(resp, hcclRootInfoPath))
			}
			if c.wantRouteConf {
				assert.Len(t, mountsMatching(resp, routeConfPath), 1)
			} else {
				assert.Empty(t, mountsMatching(resp, routeConfPath))
			}
		})
	}
}

func TestGetContainerAllocateResponse_Ascend950HostMounts_NonA5FamiliesEmitNeither(t *testing.T) {
	absentAscendDriverHostPath(t)
	root := makeFakeHostRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(rankTableV20), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "lib", "route.conf"), []byte("route=1\n"), 0o644))

	hfs, err := hostfs.OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hfs.Close() })

	for _, family := range []string{"910", "910B", "910C", "310P", "310B"} {
		t.Run(family, func(t *testing.T) {
			s := &server{
				ResourceServer: deviceplugin.ResourceServer{
					Logger:         logr.Discard(),
					Manufacturer:   Manufacturer,
					AllocationMode: workercore.DeviceAllocationModeExclusive,
				},
				product: newFakeProductResolver(),
				hostFS:  hfs,
			}
			devs := ascendDevicesFixture()
			devs.Spec.Groups[0].Family = family

			pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-non-a5-" + family)}}
			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, devs,
				map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
			require.NoError(t, err)

			assert.Empty(t, mountsMatching(resp, hcclRootInfoPath))
			assert.Empty(t, mountsMatching(resp, routeConfPath))
		})
	}
}

func TestGetContainerAllocateResponse_Ascend950HostMounts_ContainerMountDedupAndNilSafety(t *testing.T) {
	absentAscendDriverHostPath(t)
	root := makeFakeHostRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte(rankTableV20), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "lib", "route.conf"), []byte("route=1\n"), 0o644))

	hfs, err := hostfs.OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hfs.Close() })

	cases := []struct {
		name          string
		ctr           *core.Container
		wantRankTable bool
		wantRouteConf bool
	}{
		{
			name:          "nil container emits both",
			ctr:           nil,
			wantRankTable: true,
			wantRouteConf: true,
		},
		{
			name: "container already mounts hccl_rootinfo with trailing slash",
			ctr: &core.Container{
				Name: "app",
				VolumeMounts: []core.VolumeMount{
					{Name: "custom-ranktable", MountPath: "/etc/hccl_rootinfo.json/"},
				},
			},
			wantRankTable: false,
			wantRouteConf: true,
		},
		{
			name: "container already mounts route.conf with double slash",
			ctr: &core.Container{
				Name: "app",
				VolumeMounts: []core.VolumeMount{
					{Name: "custom-route", MountPath: "/lib//route.conf"},
				},
			},
			wantRankTable: true,
			wantRouteConf: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &server{
				ResourceServer: deviceplugin.ResourceServer{
					Logger:         logr.Discard(),
					Manufacturer:   Manufacturer,
					AllocationMode: workercore.DeviceAllocationModeExclusive,
				},
				product: newFakeProductResolver(),
				hostFS:  hfs,
			}
			pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a5-dedup")}}
			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, c.ctr, ascend950DevicesFixture(),
				map[deviceplugin.Resource]int32{{Group: "950", Device: testAccelID0}: 1})
			require.NoError(t, err)

			assert.Equal(t, c.wantRankTable, len(mountsMatching(resp, hcclRootInfoPath)) == 1)
			assert.Equal(t, c.wantRouteConf, len(mountsMatching(resp, routeConfPath)) == 1)
		})
	}

	t.Run("existing response mounts are not duplicated", func(t *testing.T) {
		s := &server{
			ResourceServer: deviceplugin.ResourceServer{
				Logger:         logr.Discard(),
				Manufacturer:   Manufacturer,
				AllocationMode: workercore.DeviceAllocationModeExclusive,
			},
			product: newFakeProductResolver(),
			hostFS:  hfs,
		}
		accels := deviceplugin.AllocatedAccelerators(ascend950DevicesFixture(),
			map[deviceplugin.Resource]int32{{Group: "950", Device: testAccelID0}: 1})
		resp := &deviceplugin.ContainerAllocateResponse{
			Mounts: []*deviceplugin.Mount{
				{HostPath: hcclRootInfoPath, ContainerPath: "/etc/hccl_rootinfo.json/", ReadOnly: true},
				{HostPath: routeConfPath, ContainerPath: "/lib//route.conf", ReadOnly: true},
			},
		}

		s.appendAscend950HostMounts(resp, nil, accels)
		assert.Len(t, resp.Mounts, 2)
	})
}

func TestGetContainerAllocateResponse_Ascend950HostMounts_ZeroValueServerSafety(t *testing.T) {
	absentAscendDriverHostPath(t)
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			AllocationMode: workercore.DeviceAllocationModeExclusive,
		},
		product: newFakeProductResolver(),
	}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{UID: types.UID("uid-a5-zero")}}
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, nil, ascend950DevicesFixture(),
		map[deviceplugin.Resource]int32{{Group: "950", Device: testAccelID0}: 1})
	require.NoError(t, err)
	assert.Empty(t, mountsMatching(resp, hcclRootInfoPath))
	assert.Empty(t, mountsMatching(resp, routeConfPath))
}

func TestNewAndStop_HostRootLifecycle(t *testing.T) {
	cases := []struct {
		name         string
		hostRoot     func(t *testing.T) string
		wantHostOpen bool
	}{
		{
			name:         "valid host root opens and Stop closes it",
			hostRoot:     makeFakeHostRoot,
			wantHostOpen: true,
		},
		{
			name:         "empty host root leaves hostFS nil",
			hostRoot:     func(t *testing.T) string { return "" },
			wantHostOpen: false,
		},
		{
			name: "missing or invalid host root skips cleanly without failing New",
			hostRoot: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "unmounted")
			},
			wantHostOpen: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			alloc, ok := New(device.AllocatorOptions{
				Logger:   logr.Discard(),
				HostRoot: c.hostRoot(t),
			}).(*aggregated)
			require.True(t, ok)

			if !c.wantHostOpen {
				assert.Nil(t, alloc.hostFS)
				alloc.Stop()
				return
			}

			require.NotNil(t, alloc.hostFS)
			for _, srv := range alloc.servers {
				s, ok := srv.(*server)
				require.True(t, ok)
				assert.Same(t, alloc.hostFS, s.hostFS)
			}

			_, err := alloc.hostFS.Stat("/etc")
			require.NoError(t, err)

			alloc.Stop()

			_, err = alloc.hostFS.Stat("/etc")
			require.Error(t, err)
			assert.ErrorIs(t, err, fs.ErrClosed)
		})
	}
}
