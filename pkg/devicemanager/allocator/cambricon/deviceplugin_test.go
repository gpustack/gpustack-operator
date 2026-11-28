package cambricon

import (
	"context"
	"errors"
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

const testGroupID = "mlu590"

// testMLU0Index is the cnDev device index the fixture records for MLU-0, which is what names its
// card node.
const testMLU0Index = 3

func cambriconDevicesFixture() *workercore.Devices {
	return &workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           testGroupID,
				Manufacturer: Manufacturer,
				Memory:       49152, // MiB
				// The cnDev device index deliberately differs from the logical Index: the detector
				// derives them from different counters (Index skips a card that failed detection,
				// PhysicalIndexes carries the raw cnDev loop position), so a fixture where they
				// agree would let the responder read the wrong one and still pass.
				Accelerators: []workercore.Accelerator{
					{ID: "MLU-0", Index: 0, PhysicalIndexes: []uint32{testMLU0Index}, Topology: workercore.DeviceTopology{PciBusID: testCard0}},
					{ID: "MLU-1", Index: 1, PhysicalIndexes: []uint32{7}, Topology: workercore.DeviceTopology{PciBusID: testCard1}},
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
			// Production always hands the responder a logger, and the preflight reports through it,
			// so the fake carries one too rather than leaving the zero value behind.
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: workercore.DeviceAllocationModeSliced,
		},
		smlu: driver,
	}
}

func allocateMLU0() map[deviceplugin.Resource]int32 {
	return map[deviceplugin.Resource]int32{{Group: testGroupID, Device: "MLU-0"}: 1}
}

// missingCardIndex is a driver index no host is expected to have a device node for. A case that
// needs the required-node refusal addresses a card by it, so the case behaves the same wherever the
// suite runs -- including on a machine that does have Cambricon cards.
const missingCardIndex = 4095

// requireNoCardNode states the assumption missingCardIndex rests on. No numeric device name is
// intrinsically impossible, so a host that does carry that node would send the case down the success
// path and fail it somewhere confusing; this fails it here instead, saying what to change.
func requireNoCardNode(t *testing.T) {
	t.Helper()
	require.NoFileExists(t, cardDevicePath(missingCardIndex),
		"this case needs a host without that node; pick another index for missingCardIndex")
}

// redirectCardDevicePath points the card node's path format at a temp dir for the test, so a case
// can serve a card node the host lacks or withhold one it may have; the responder only stats the
// path, so a plain file stands in for the character device. The IPC node is left where it is: it is
// optional, and every host this suite runs on lacks it.
func redirectCardDevicePath(t *testing.T) {
	t.Helper()
	orig := cardDevicePathFormat
	cardDevicePathFormat = filepath.Join(t.TempDir(), "cambricon_dev%d")
	t.Cleanup(func() { cardDevicePathFormat = orig })
}

// stageCardNode serves the card node for the driver index under redirectCardDevicePath, so a sliced
// grant is exercised the same way on a host without Cambricon cards as on one with them.
func stageCardNode(t *testing.T, index uint32) {
	t.Helper()
	require.NoError(t, os.WriteFile(cardDevicePath(index), nil, 0o600))
}

// stagedFakeDriver is a fake driver whose CreateInstance materializes the instance's device node
// under a test dir, the way a real driver hands the host a node with the instance.
func stagedFakeDriver(t *testing.T) *fakeSMLUDriver {
	t.Helper()
	d := newFakeDriver()
	d.nodeDir = t.TempDir()
	return d
}

// instanceNodePath is the node stagedFakeDriver's CreateInstance materializes for an instance.
func instanceNodePath(d *fakeSMLUDriver, name string) string {
	return filepath.Join(d.nodeDir, "cambricon_smlu-"+name)
}

// hostPaths renders the device nodes a response carries, for a containment assertion.
func hostPaths(devs []*deviceplugin.DeviceSpec) []string {
	paths := make([]string, 0, len(devs))
	for i := range devs {
		paths = append(paths, devs[i].HostPath)
	}
	return paths
}

// newWholeCardServer builds a responder for one of the modes that hand out whole cards.
func newWholeCardServer(mode workercore.DeviceAllocationMode) *server {
	return &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logr.Discard(),
			Manufacturer:   Manufacturer,
			AllocationMode: mode,
		},
	}
}

// wholeCardPod builds a pending pod whose container requests no slicing dimension at all, the shape
// the exclusive, shared and visibility responders are handed.
func wholeCardPod(uid, ctrName string) (*core.Pod, *core.Container) {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: ctrName + "-pod", UID: types.UID(uid)},
		Spec: core.PodSpec{
			Containers: []core.Container{{Name: ctrName}},
		},
	}
	return pod, &pod.Spec.Containers[0]
}

// The device-node paths and the index that names them. Two of the optional ones carry a colon,
// which no other vendor's paths in this repo do, so they are spelled out here.
func TestDevicePaths(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "the card's own node",
			got:  []string{cardDevicePath(3)},
			want: []string{"/dev/cambricon_dev3"},
		},
		{
			name: "the card's IPC node, which the sliced path injects too",
			got:  []string{cardIPCMDevicePath(3)},
			want: []string{"/dev/cambricon_ipcm3"},
		},
		{
			name: "the card's optional nodes",
			got:  optionalCardDevicePaths(3),
			want: []string{
				"/dev/cambr-msgq:3",
				"/dev/cambr-rpc:3",
				"/dev/cmsg_ctrl3",
				"/dev/commu3",
				"/dev/cambricon_ipcm3",
			},
		},
		{
			name: "the node-level nodes, which no index names",
			got:  nodeDevicePaths,
			want: []string{"/dev/cambricon_ctl", "/dev/cambricon_gdr"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.got)
		})
	}
}

// The visibility env carries every allocated card's driver index, in the order the container
// numbers them. The fixture's indexes are 3 and 7, so a responder reading the logical one would emit
// "0" or "0,1" and fail here.
func TestVisibleDevices(t *testing.T) {
	cases := []struct {
		name      string
		allocated map[deviceplugin.Resource]int32
		want      string
	}{
		{
			name:      "one allocated card",
			allocated: allocateMLU0(),
			want:      "3",
		},
		{
			name: "two allocated cards",
			allocated: map[deviceplugin.Resource]int32{
				{Group: testGroupID, Device: "MLU-0"}: 1,
				{Group: testGroupID, Device: "MLU-1"}: 1,
			},
			want: "3,7",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			visible, err := visibleDevices(
				deviceplugin.AllocatedAccelerators(cambriconDevicesFixture(), c.allocated),
			)
			require.NoError(t, err)
			assert.Equal(t, c.want, visible)
		})
	}
}

// A record carrying no cnDev index is refused rather than named by a guessed number.
func TestVisibleDevices_MissingDriverIndexRejected(t *testing.T) {
	devs := cambriconDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil

	_, err := visibleDevices(deviceplugin.AllocatedAccelerators(devs, allocateMLU0()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carries no cnDev device index")
}

// A card the host exposes no device node for fails the allocation, rather than starting a container
// with no accelerator; the error names the card and the path it looked for. The path is named by the
// driver index, so a responder reading the logical index would look for the wrong one.
func TestWholeCard_MissingOwnDeviceNodeRejected(t *testing.T) {
	requireNoCardNode(t)
	devs := cambriconDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{missingCardIndex}

	pod, ctr := wholeCardPod("pod-no-devnode", "train")
	_, err := newWholeCardServer(workercore.DeviceAllocationModeExclusive).GetContainerAllocateResponse(
		context.Background(), pod, ctr, devs, allocateMLU0(),
	)
	require.Error(t, err)
	for _, want := range []string{testCard0, cardDevicePath(missingCardIndex)} {
		assert.Containsf(t, err.Error(), want, "the message must name %q", want)
	}
}

// A single-accelerator slice creates one sMLU instance, records the correlation + profile marker,
// and injects VIRTUAL_DEVICES naming the instance's device node.
func TestSliced_PartialSlice(t *testing.T) {
	redirectLogicalSliceDirs(t)
	redirectCardDevicePath(t)
	stageCardNode(t, testMLU0Index)
	d := stagedFakeDriver(t)
	s := newSlicedServer(d)
	devs := cambriconDevicesFixture()

	pod, ctr := slicedPod("pod-uid-1", "train", 25, 50) // 25% compute, 50% of 49152 = 24576 MiB
	resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.NoError(t, err)

	name := encodeInstanceName("pod-uid-1", "train")
	assert.True(t, d.hasInstance(name), "the sMLU instance was created")
	assert.Equal(t, instanceNodePath(d, name), resp.Envs["VIRTUAL_DEVICES"])

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
	redirectCardDevicePath(t)
	stageCardNode(t, testMLU0Index)
	d := stagedFakeDriver(t)
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

// The responder hands the preflight the cnDev device index the detector recorded, so a mode failure
// names the card an operator has to go and fix by both its PCI address and its index.
//
// The expected index is 3, the fixture's PhysicalIndexes[0], not 0, its logical Index: the two differ
// in the fixture precisely so this test fails if the responder reads the wrong one.
func TestSliced_ModeFailureNamesTheCard(t *testing.T) {
	redirectLogicalSliceDirs(t)
	d := newFakeDriver()
	d.failModeWrite = errors.New("set smlu mode: NOT_SUPPORTED")
	s := newSlicedServer(d)

	pod, ctr := slicedPod("pod-mode-fail", "train", 25, 50)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, cambriconDevicesFixture(), allocateMLU0())
	require.Error(t, err)
	for _, want := range []string{testCard0, "device index 3", "cnmon set -c 3 -smlu on"} {
		assert.Containsf(t, err.Error(), want, "the message must name %q", want)
	}
}

// The detector always records a cnDev device index, so a record without one is malformed. It is
// rejected before the card is touched rather than allocated against a guessed index.
func TestSliced_MissingDeviceIndexRejected(t *testing.T) {
	redirectLogicalSliceDirs(t)
	d := newFakeDriver()
	s := newSlicedServer(d)

	devs := cambriconDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil

	pod, ctr := slicedPod("pod-noindex", "train", 25, 50)
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carries no cnDev device index")

	reads, writes := d.modeCalls()
	assert.Zero(t, reads, "a malformed record is rejected before the mode is read")
	assert.Zero(t, writes, "and before it is written")
	assert.False(t, markerExists("pod-noindex", "train"))
}

// A request that is malformed in both ways at once -- an accelerator with no driver index, and no
// memory dimension -- reports the record, not the request. Both manufacturers order it that way, so
// one defect always produces one message wherever it is hit.
func TestSliced_MissingDeviceIndexOutranksMissingMemory(t *testing.T) {
	redirectLogicalSliceDirs(t)
	d := newFakeDriver()
	s := newSlicedServer(d)

	devs := cambriconDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil

	pod, ctr := slicedPod("pod-both-bad", "train", 25, 0) // no memory dimension either
	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carries no cnDev device index")
}

// A sliced response refuses rather than granting a container no accelerator, and every refusal
// releases the instance reserveInstance already persisted. The instance's own node is required
// whatever the card node says -- with the card node alone the container gets the whole card while the
// ledger records a slice -- and the card node is required the way the whole-card path requires it.
// The four cases cross the two, so neither refusal hides behind the other; a fifth proves that a
// refused release is logged without replacing the refusal.
func TestSliced_RefusesWithoutInstanceOrCardNode(t *testing.T) {
	testCases := []struct {
		name         string
		cardNode     bool // the host exposes the card node
		instanceNode bool // the driver readback populated the instance's device node
		// absentNode is set when the readback names a node the host does not have: the node not yet
		// created, or a driver build naming it differently.
		absentNode bool
		// wantRefusal is what the refusal must say beyond the accelerator and its card; empty for
		// the one case that succeeds.
		wantRefusal string
		// wantCardPath is set when the refusal names the card node it looked for.
		wantCardPath bool
		// releaseFails makes the driver refuse DestroyInstance, so the case can prove the release
		// failure is logged without replacing the refusal.
		releaseFails bool
		// wantMarker is whether the reservation's marker is still on disk afterwards: a grant keeps
		// it, a refusal removes it only once the instance is released.
		wantMarker bool
	}{
		{
			name:         "card node and instance node",
			cardNode:     true,
			instanceNode: true,
			wantMarker:   true,
		},
		{
			// The whole-card refusal, mirrored: a card the host exposes no node for cannot be handed
			// to a container at all.
			name:         "no card node",
			instanceNode: true,
			wantRefusal:  "has no device node",
			wantCardPath: true,
		},
		{
			// The isolation break: with the card node alone the container gets the whole card.
			name:         "no instance node",
			cardNode:     true,
			instanceNode: false,
			wantRefusal:  "driver readback",
		},
		{
			// The same break one step later: the readback names a node, but the host has none there,
			// and with the card node alone the container still holds the whole card.
			name:         "instance node absent from the host",
			cardNode:     true,
			instanceNode: true,
			absentNode:   true,
			wantRefusal:  "has no instance device node",
		},
		{
			// The silent grant: zero devices and no VIRTUAL_DEVICES, with the pod Running.
			name:         "neither node",
			instanceNode: false,
			wantRefusal:  "driver readback",
		},
		{
			// The release is best effort: a driver that refuses it leaves the instance for the reclaim
			// loop, and the refusal still says why the container was refused.
			name:         "no card node, release refused",
			instanceNode: true,
			wantRefusal:  "has no device node",
			wantCardPath: true,
			releaseFails: true,
			wantMarker:   true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			redirectLogicalSliceDirs(t)
			redirectCardDevicePath(t)
			if tc.cardNode {
				stageCardNode(t, testMLU0Index)
			}
			d := stagedFakeDriver(t)
			d.emptyDevNode = !tc.instanceNode
			d.absentDevNode = tc.absentNode
			if tc.releaseFails {
				d.failDestroy[testCard0] = errors.New("IN_PROBLEM")
			}
			s := newSlicedServer(d)

			pod, ctr := slicedPod("pod-refuse", "train", 25, 50)
			resp, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr,
				cambriconDevicesFixture(), allocateMLU0())

			name := encodeInstanceName("pod-refuse", "train")
			if tc.wantRefusal == "" {
				require.NoError(t, err)
				assert.True(t, markerExists("pod-refuse", "train"), "a grant keeps its marker")
				assert.Empty(t, d.destroyedInsts, "a grant never releases its instance")
				assert.Equal(t, instanceNodePath(d, name), resp.Envs["VIRTUAL_DEVICES"])
				assert.Contains(t, hostPaths(resp.Devices), instanceNodePath(d, name), "the response carries the instance's node")
				assert.Contains(t, hostPaths(resp.Devices), cardDevicePath(testMLU0Index), "the response carries the card node")
				return
			}

			require.Error(t, err)
			assert.Nil(t, resp)
			for _, want := range []string{"MLU-0", testCard0, tc.wantRefusal} {
				assert.Containsf(t, err.Error(), want, "the refusal must name %q", want)
			}
			if tc.wantCardPath {
				assert.Containsf(t, err.Error(), cardDevicePath(testMLU0Index), "the refusal must name the card node")
			}
			if tc.absentNode {
				assert.Containsf(t, err.Error(), instanceNodePath(d, name), "the refusal must name the instance's node")
			}
			assert.Equal(t, []smluRef{{card: testCard0, name: name}}, d.destroyedInsts,
				"the reserved instance is released exactly once, by card and name")
			assert.Equal(t, tc.wantMarker, markerExists("pod-refuse", "train"),
				"the marker leaves only with a released instance")
			if tc.releaseFails {
				assert.NotContains(t, err.Error(), "IN_PROBLEM", "a failed release is logged, never returned as the refusal")
				assert.True(t, d.hasInstance(name), "and the instance it failed on is left for the reclaim loop")
				return
			}
			assert.False(t, d.hasInstance(name), "and the release reached the driver")
		})
	}
}

// A non-sliced (exclusive) responder never creates an instance or writes a marker: the
// slicing artifacts are isolated to the sliced mode. Asserted on a card the host has no device node
// for, so that the whole-card path is exercised the same way wherever this runs — it refuses that
// card, and must leave no slicing state behind while doing so.
func TestSliced_ExclusiveModeHasNoSlicingArtifacts(t *testing.T) {
	redirectLogicalSliceDirs(t)
	requireNoCardNode(t)
	s := newWholeCardServer(workercore.DeviceAllocationModeExclusive)
	devs := cambriconDevicesFixture()
	devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = []uint32{missingCardIndex}
	pod, ctr := slicedPod("pod-excl", "train", 25, 50)

	_, err := s.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
	require.Error(t, err)
	assert.False(t, markerExists("pod-excl", "train"))
}

func TestSliced_SequentialInitContainersReuseInstance(t *testing.T) {
	redirectLogicalSliceDirs(t)
	redirectCardDevicePath(t)
	stageCardNode(t, testMLU0Index)
	d := stagedFakeDriver(t)
	s := newSlicedServer(d)
	devs := cambriconDevicesFixture()

	lim := core.ResourceList{
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer):  *resource.NewQuantity(60, resource.DecimalSI),
		nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer): *resource.NewQuantity(60, resource.DecimalSI),
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "forge", UID: types.UID("pod-init-mlu")},
		Spec: core.PodSpec{
			InitContainers: []core.Container{
				{Name: "forge-runner", Resources: core.ResourceRequirements{Limits: lim}},
				{Name: "forge-verifier", Resources: core.ResourceRequirements{Limits: lim}},
			},
			Containers: []core.Container{{Name: "pause"}},
		},
	}

	resp1, err := s.GetContainerAllocateResponse(context.Background(), pod, &pod.Spec.InitContainers[0], devs, allocateMLU0())
	require.NoError(t, err)
	resp2, err := s.GetContainerAllocateResponse(context.Background(), pod, &pod.Spec.InitContainers[1], devs, allocateMLU0())
	require.NoError(t, err)

	assert.Equal(t, 1, d.instanceCreates, "sequential init containers reuse the canonical sMLU instance")
	assert.Equal(t, resp1.Envs["VIRTUAL_DEVICES"], resp2.Envs["VIRTUAL_DEVICES"])
	assert.True(t, markerExists("pod-init-mlu", "forge-runner"))
	assert.False(t, markerExists("pod-init-mlu", "forge-verifier"))
}
