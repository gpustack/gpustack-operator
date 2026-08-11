package amd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The accelerator every fixture below is a slice of: the 60 CU / 3 SE / 1 XCC gfx1101 the conformance
// tables were measured on, so the masks these cases assert are the table's own rows rather than a
// number invented here.
var testTopology = Topology{Name: "gfx1101", CU: 60, SE: 3, SAPerSE: 2, XCC: 1}

const (
	testCardVRAMMib = 16368
	testCardUUID    = "GPU-5c88007d760374f3"
	testCardUUID2   = "GPU-d99e7fe92c7bdf75"
)

// redirectLogicalSliceDirs points the staged-library and pod-working directories into the test's own
// temporary root. Paths only, no files: the response composes paths, it never reads them.
func redirectLogicalSliceDirs(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	origLib, origPods := deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir
	deviceplugin.OperatorLibDir = filepath.Join(root, "lib")
	deviceplugin.OperatorPodsDir = filepath.Join(root, "pods")
	t.Cleanup(func() {
		deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir = origLib, origPods
	})
}

// stubTopology supplies the accelerator the placement arithmetic runs against. The real reader is a cgo
// seam that exists only on linux; the arithmetic above it must stay testable with no accelerator.
func stubTopology(t *testing.T, topo Topology) {
	t.Helper()
	orig := readTopologyFn
	readTopologyFn = func(string, string) (Topology, error) { return topo, nil }
	t.Cleanup(func() { readTopologyFn = orig })
}

func testDevices(uuids ...string) *workercore.Devices {
	accels := make([]workercore.Accelerator, 0, len(uuids))
	for i, uuid := range uuids {
		accels = append(accels, workercore.Accelerator{
			ID:       uuid,
			Index:    uint32(i),
			Topology: workercore.DeviceTopology{PciBusID: "0000:0" + string(rune('4'+i)) + ":00.0"},
		})
	}
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: "node-0"},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: Manufacturer,
				Memory:       testCardVRAMMib,
				Accelerators: accels,
			}},
		},
	}
}

// testResource is the placement-ledger key of one accelerator in the fixture group.
func testResource(uuid string) deviceplugin.Resource {
	return deviceplugin.Resource{Group: "grp-0", Device: uuid}
}

func testAllocated(uuids ...string) map[deviceplugin.Resource]int32 {
	allocated := make(map[deviceplugin.Resource]int32, len(uuids))
	for _, uuid := range uuids {
		allocated[deviceplugin.Resource{Group: "grp-0", Device: uuid}] = 1
	}
	return allocated
}

// testContainer builds a container requesting a slice: coresPct of the accelerator's compute and
// memPct of its VRAM. A zero percentage omits that request, which is how the defaults are exercised.
func testContainer(coresPct, memPct int64, envs ...core.EnvVar) *core.Container {
	limits := core.ResourceList{
		nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModeSliced): resource.MustParse("1"),
	}
	if coresPct > 0 {
		limits[nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)] = *resource.NewQuantity(coresPct, resource.DecimalSI)
	}
	if memPct > 0 {
		limits[nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)] = *resource.NewQuantity(memPct, resource.DecimalSI)
	}
	return &core.Container{
		Name:      "main",
		Env:       envs,
		Resources: core.ResourceRequirements{Limits: limits},
	}
}

func testPod() *core.Pod {
	return &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: types.UID("pod-uid")}}
}

func TestNew_ServerSet(t *testing.T) {
	cases := []struct {
		name string
		opts device.AllocatorOptions
		want []workercore.DeviceAllocationMode
	}{
		{
			name: "sliced is registered by default, between shared and visibility",
			opts: device.AllocatorOptions{Logger: klog.Background()},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeSliced,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-sliced drops it and leaves the rest alone",
			opts: device.AllocatorOptions{Logger: klog.Background(), NoSliced: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeShared,
				workercore.DeviceAllocationModeVisibility,
			},
		},
		{
			name: "--no-shared and --no-sliced leave the whole-accelerator modes",
			opts: device.AllocatorOptions{Logger: klog.Background(), NoShared: true, NoSliced: true},
			want: []workercore.DeviceAllocationMode{
				workercore.DeviceAllocationModeExclusive,
				workercore.DeviceAllocationModeVisibility,
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			agg, ok := New(c.opts).(aggregated)
			require.True(t, ok)
			got := make([]workercore.DeviceAllocationMode, 0, len(agg.servers))
			for _, srv := range agg.servers {
				got = append(got, srv.(*server).AllocationMode)
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// TestGetContainerAllocateResponse pins the non-sliced response as it stands: one env, no mounts,
// the accelerator's UUID for the container runtime to inject device nodes from.
func TestGetContainerAllocateResponse(t *testing.T) {
	s := &server{}
	resp, err := s.GetContainerAllocateResponse(
		context.Background(), testPod(), testContainer(0, 0),
		testDevices(testCardUUID, testCardUUID2), testAllocated(testCardUUID2))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"AMD_VISIBLE_DEVICES": testCardUUID2}, resp.Envs)
	assert.Empty(t, resp.Mounts)
}

func TestPlaceLogicalSliced(t *testing.T) {
	cases := []struct {
		name     string
		coresPct int64
		occupied []workercore.AcceleratorPlacement
		want     workercore.AcceleratorPlacement
		wantErr  string
	}{
		{
			// Conformance table A's 25% row: 7.5 WGPs rounds to 8, aligns DOWN to 6, and the naive
			// answer (15 CUs, "0:0-14") would have left the container on the whole accelerator.
			name:     "25 percent takes six WGPs, not the naive fifteen CUs",
			coresPct: 25,
			want:     workercore.AcceleratorPlacement{Start: 0, Length: 12},
		},
		{
			name:     "a second slice is packed beside the first, not on top of it",
			coresPct: 25,
			occupied: []workercore.AcceleratorPlacement{{Start: 0, Length: 12}},
			want:     workercore.AcceleratorPlacement{Start: 12, Length: 12},
		},
		{
			name:     "a hole is reused ahead of the tail",
			coresPct: 25,
			occupied: []workercore.AcceleratorPlacement{{Start: 24, Length: 12}},
			want:     workercore.AcceleratorPlacement{Start: 0, Length: 12},
		},
		{
			// SlicedCoresPercent returns 100 when nothing was requested, and a whole-accelerator mask is
			// still emitted: it states what the container may reach rather than leaving it unsaid.
			name:     "no request at all is a whole accelerator",
			coresPct: 0,
			want:     workercore.AcceleratorPlacement{Start: 0, Length: 60},
		},
		{
			// Below one shader-engine round the mask stops confining, so the request is refused
			// rather than quietly rounded up to a ceiling nobody asked for or accounted.
			name:     "a sub-quantum request is refused, naming the accelerator's minimum",
			coresPct: 5,
			wantErr:  "smallest slice is 9%",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			stubTopology(t, testTopology)
			s := &server{}

			placed, err := s.PlaceLogicalSliced(
				context.Background(), testPod(), testContainer(c.coresPct, 50),
				testDevices(testCardUUID), testAllocated(testCardUUID),
				deviceplugin.Placements{testResource(testCardUUID): c.occupied})

			if c.wantErr != "" {
				require.ErrorContains(t, err, c.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t,
				deviceplugin.Placements{
					testResource(testCardUUID): {c.want},
				}, placed)
		})
	}
}

// TestPlaceLogicalSliced_RefusesACardWithNoIdentity pins the one accelerator shape that cannot be
// sliced: an empty UUID is what AsicInfo.GetUniqueId returns when the ASIC serial reads N/A, and
// emitting it would widen the container to every accelerator on the node instead of narrowing it to
// this one.
func TestPlaceLogicalSliced_RefusesACardWithNoIdentity(t *testing.T) {
	stubTopology(t, testTopology)
	s := &server{}

	_, err := s.PlaceLogicalSliced(
		context.Background(), testPod(), testContainer(25, 50),
		testDevices(""), testAllocated(""), nil)
	require.ErrorContains(t, err, "reports no unique id")
}

// TestGetLogicalSlicedResponse_SingleCard asserts the whole injection for the case admission
// actually admits: one accelerator, one window, every environment key and every mount.
func TestGetLogicalSlicedResponse_SingleCard(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := &server{}
	pod, ctr := testPod(), testContainer(25, 25)

	resp, err := s.GetLogicalSlicedResponse(
		context.Background(), pod, ctr, testDevices(testCardUUID), testAllocated(testCardUUID),
		deviceplugin.Placements{testResource(testCardUUID): {{Start: 0, Length: 12}}})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"AMD_VISIBLE_DEVICES":         testCardUUID,
		"ROCR_VISIBLE_DEVICES":        testCardUUID,
		"HSA_CU_MASK":                 "0:0-11",
		"VROCM_DEVICE_MEMORY_LIMIT_0": "4092",
		"VROCM_LEDGER_PATH":           ctrLedgerPath,
		"LIBVROCM_LOG_LEVEL":          "1",
	}, resp.Envs)

	libDir := filepath.Join(deviceplugin.OperatorLibDir, "amd")
	ledgerDir := filepath.Join(deviceplugin.PodWorkDir(string(pod.UID), ctr.Name), "run/vrocm")
	assert.Equal(t, []*deviceplugin.Mount{
		{ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
		{ContainerPath: ctrVrocmLibPath, HostPath: filepath.Join(libDir, "libvrocm.so"), ReadOnly: true},
		{ContainerPath: ctrVrocmMonPath, HostPath: filepath.Join(libDir, "rocm-monitor"), ReadOnly: true},
		{ContainerPath: ctrVrocmCheckPath, HostPath: filepath.Join(libDir, "rocm-cumask-check"), ReadOnly: true},
		{ContainerPath: ctrLedgerDir, HostPath: ledgerDir, ReadOnly: false},
	}, resp.Mounts)
}

// TestGetLogicalSlicedResponse_MultiCard pins the loop's shape. Admission does not admit a
// two-accelerator logical slice today (the Pod webhook requires <base>.sliced to be exactly 1), so
// this case exists to keep the indexing honest for the day that gate is lifted: each memory figure
// is against its own
// accelerator, and each mask segment carries that accelerator's position in ROCR_VISIBLE_DEVICES.
func TestGetLogicalSlicedResponse_MultiCard(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := &server{}

	resp, err := s.GetLogicalSlicedResponse(
		context.Background(), testPod(), testContainer(25, 50),
		testDevices(testCardUUID, testCardUUID2), testAllocated(testCardUUID, testCardUUID2),
		deviceplugin.Placements{
			testResource(testCardUUID):  {{Start: 0, Length: 12}},
			testResource(testCardUUID2): {{Start: 12, Length: 12}},
		})
	require.NoError(t, err)

	assert.Equal(t, testCardUUID+","+testCardUUID2, resp.Envs["ROCR_VISIBLE_DEVICES"])
	assert.Equal(t, "0:0-11;1:12-23", resp.Envs["HSA_CU_MASK"],
		"one segment per card, indexed by position in the visible-devices list")
	assert.Equal(t, "8184", resp.Envs["VROCM_DEVICE_MEMORY_LIMIT_0"])
	assert.Equal(t, "8184", resp.Envs["VROCM_DEVICE_MEMORY_LIMIT_1"])
}

func TestGetLogicalSlicedResponse_Rejections(t *testing.T) {
	cases := []struct {
		name       string
		ctr        *core.Container
		placements deviceplugin.Placements
		wantErr    string
	}{
		{
			name:       "an accelerator with no placed window is refused rather than left unmasked",
			ctr:        testContainer(25, 50),
			placements: deviceplugin.Placements{},
			wantErr:    "no compute window was placed",
		},
		{
			name:       "a container with neither memory request is refused",
			ctr:        testContainer(25, 0),
			placements: deviceplugin.Placements{testResource(testCardUUID): {{Start: 0, Length: 12}}},
			wantErr:    "derive sliced memory limit",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			s := &server{}
			_, err := s.GetLogicalSlicedResponse(
				context.Background(), testPod(), c.ctr,
				testDevices(testCardUUID), testAllocated(testCardUUID), c.placements)
			require.ErrorContains(t, err, c.wantErr)
		})
	}
}

// TestGetLogicalSlicedResponse_KeepsADeclaredLogLevel pins the debugging escape hatch: a workload
// that sets the level itself keeps it, so a user chasing a refusal can raise the verbosity without
// the allocator overwriting them on every restart.
func TestGetLogicalSlicedResponse_KeepsADeclaredLogLevel(t *testing.T) {
	redirectLogicalSliceDirs(t)
	s := &server{}

	resp, err := s.GetLogicalSlicedResponse(
		context.Background(), testPod(),
		testContainer(25, 50, core.EnvVar{Name: "LIBVROCM_LOG_LEVEL", Value: "2"}),
		testDevices(testCardUUID), testAllocated(testCardUUID),
		deviceplugin.Placements{testResource(testCardUUID): {{Start: 0, Length: 12}}})
	require.NoError(t, err)
	assert.NotContains(t, resp.Envs, "LIBVROCM_LOG_LEVEL")
}
