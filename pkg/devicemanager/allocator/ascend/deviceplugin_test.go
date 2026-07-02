package ascend

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
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const (
	testAccelID0 = "E0F4EE64 802061B1 6A691492 89528485 104301E3"
	testAccelID1 = "E281A66C 140C979 2CFBED72 A4500485 104301E3"
)

// redirectSoftSliceDirs points the soft-slicing host paths at a temp dir for the test.
func redirectSoftSliceDirs(t *testing.T) {
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
				Accelerators: []workercore.Accelerator{
					{ID: testAccelID0, Index: 0},
					{ID: testAccelID1, Index: 1},
				},
			}},
		},
	}
}

// slicedPod builds a pending pod whose container requests `units` of the Ascend
// sliced.units resource.
func slicedPod(uid, ctrName string, units int64) (*core.Pod, *core.Container) {
	resName := nodefeature.GetAcceleratableSlicedUnitsResourceName(Manufacturer)
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: ctrName + "-pod", UID: types.UID(uid)},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name: ctrName,
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						resName: *resource.NewQuantity(units, resource.DecimalSI),
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

func TestGetSlicedContainerAllocateResponse(t *testing.T) {
	redirectSoftSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()

	// Allocate a 1/8 slice (units=200000, D=1600000 -> R=0.125) of accelerator index 0.
	pod, ctr := slicedPod("pod-uid-1", "train", 200000)
	allocated := map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1}

	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
	require.NoError(t, err)

	// Env: the visible device index.
	assert.Equal(t, "0", resp.Envs["ASCEND_VISIBLE_DEVICES"])

	// npu_info.config content (R=0.125: aicore 12%, memory floor(65536*0.125)=8192MiB).
	podWorkDir := deviceplugin.PodWorkDir("pod-uid-1", "train")
	configPath := filepath.Join(podWorkDir, "etc/enpu/vcann-rt/npu_info.config")
	body, err := os.ReadFile(configPath)
	require.NoError(t, err)
	want := "physical-npu-id=0\n" +
		"virtual-npu-id=0\n" +
		"aicore-quota=12\n" +
		"memory-quota=8192\n" +
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

// Two concurrent slices on the same physical NPU must get distinct virtual-npu-ids;
// a slice on a different physical NPU starts back at 0.
func TestSlicedVirtualNPUIDAssignment(t *testing.T) {
	redirectSoftSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()

	readVNPU := func(uid, ctr string) int {
		_, vnpu, ok := parseNPUInfoConfig(
			filepath.Join(deviceplugin.PodWorkDir(uid, ctr), "etc/enpu/vcann-rt/npu_info.config"))
		require.True(t, ok)
		return vnpu
	}

	// First slice on physical NPU 0 -> vnpu 0.
	p1, c1 := slicedPod("uid-a", "train", 200000)
	_, err := s.GetContainerAllocateResponse(context.Background(), p1, c1, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, readVNPU("uid-a", "train"))

	// Second slice on the SAME physical NPU 0 -> vnpu 1 (lowest free).
	p2, c2 := slicedPod("uid-b", "train", 200000)
	_, err = s.GetContainerAllocateResponse(context.Background(), p2, c2, devs,
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, readVNPU("uid-b", "train"))

	// Slice on a DIFFERENT physical NPU (index 1) -> vnpu 0 again.
	p3, c3 := slicedPod("uid-c", "train", 200000)
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

func TestGetSlicedContainerAllocateResponse_NoUnitsRequest(t *testing.T) {
	redirectSoftSliceDirs(t)
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

// vcann-rt is single-NPU: a multi-card sliced allocation must be rejected rather than
// silently quota-isolating only the first card.
func TestGetSlicedContainerAllocateResponse_MultiCardRejected(t *testing.T) {
	redirectSoftSliceDirs(t)
	s := newSlicedServer()
	devs := ascendDevicesFixture()

	pod, ctr := slicedPod("uid-multi", "train", 200000)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs,
		map[deviceplugin.Resource]int32{
			{Group: "910b2", Device: testAccelID0}: 1,
			{Group: "910b2", Device: testAccelID1}: 1,
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-NPU")
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
