package deviceplugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// pl builds one placement interval [start, start+length).
func pl(start, length int32) workercore.AcceleratorPhysicalPlacement {
	return workercore.AcceleratorPhysicalPlacement{Start: start, Length: length}
}

// a100Placements is the A100 capability inventory for the four worked-example profiles,
// each carrying its full empty-card legal placement set (the detect-time cache the
// reconciler subtracts occupied from).
func a100Placements() []workercore.AcceleratorPhysicalSlicedProfile {
	return []workercore.AcceleratorPhysicalSlicedProfile{
		{
			Name: "1g.5gb", MemoryMib: 5120, ComputeSlices: 1, MemorySlices: 1, Count: 7,
			Placements: []workercore.AcceleratorPhysicalPlacement{pl(0, 1), pl(1, 1), pl(2, 1), pl(3, 1), pl(4, 1), pl(5, 1), pl(6, 1)},
		},
		{
			Name: "1g.10gb", MemoryMib: 10240, ComputeSlices: 1, MemorySlices: 2, Count: 4,
			Placements: []workercore.AcceleratorPhysicalPlacement{pl(0, 2), pl(2, 2), pl(4, 2), pl(6, 2)},
		},
		{
			Name: "2g.10gb", MemoryMib: 10240, ComputeSlices: 2, MemorySlices: 2, Count: 3,
			Placements: []workercore.AcceleratorPhysicalPlacement{pl(0, 2), pl(2, 2), pl(4, 2)},
		},
		{
			Name: "3g.20gb", MemoryMib: 20480, ComputeSlices: 3, MemorySlices: 4, Count: 2,
			Placements: []workercore.AcceleratorPhysicalPlacement{pl(0, 4), pl(4, 4)},
		},
	}
}

// physicalAndLogicalDevices returns a node's Devices with one MIG-enabled card (mig-0, a non-empty
// PhysicalSliced.Profiles carrying cached Placements) and one logical-slice card (logical-0), so
// a reconcile exercises the ledger fold on the MIG card and its absence on the logical card.
func physicalAndLogicalDevices(nodeName string) *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{
					{
						ID:    "mig-0",
						Index: 0,
						Status: workercore.AcceleratorStatus{PhysicalSliced: workercore.AcceleratorPhysicalSliced{
							Profiles: a100Placements(),
							Count:    7,
						}},
					},
					{
						ID:     "logical-0",
						Index:  1,
						Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 128}},
					},
				},
			}},
		},
	}
}

// allocationAnnotation renders the per-container allocation records the way the device plugin
// persists them.
func allocationAnnotation(t *testing.T, allocations PodAllocations) map[string]string {
	t.Helper()
	annoBytes, err := json.Marshal(allocations)
	require.NoError(t, err)
	return map[string]string{AllocatedAcceleratorAnnoKey: string(annoBytes)}
}

// physicalSliceAllocation records one MIG instance of profile on (group, device) at the given
// placements — the upward record the reconciler unions into the card's occupied set.
func physicalSliceAllocation(group, device, profile string, placements ...workercore.AcceleratorPhysicalPlacement) workercore.DevicesStatus {
	return workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
		ID:           group,
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.AcceleratorAllocation{{
			ID:                          device,
			Mode:                        workercore.DeviceAllocationModeSliced,
			Allocated:                   nodefeature.ResourceMaxUnits / 8,
			AllocatedPhysicalProfile:    profile,
			AllocatedPhysicalPlacements: placements,
		}},
	}}}
}

// physicalSlicePod builds a Pod whose single workload container holds one MIG instance.
func physicalSlicePod(t *testing.T, name, group, device, profile, node string, placements ...workercore.AcceleratorPhysicalPlacement) *core.Pod {
	t.Helper()
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name),
			Annotations: allocationAnnotation(t, PodAllocations{
				"main": {Devices: physicalSliceAllocation(group, device, profile, placements...)},
			}),
		},
		Spec: core.PodSpec{NodeName: node},
	}
}

// TestDevicesReconciler_PatchAllocatingPod_PerContainer verifies the durable allocation record is
// keyed by container. A single slot lets a second container's Allocate erase the first from the
// ledger the reconciler rebuilds, so a card still running a container would read free after a
// device-manager restart; and a repeated Allocate for one container must overwrite its own entry
// rather than charge its card twice.
func TestDevicesReconciler_PatchAllocatingPod_PerContainer(t *testing.T) {
	const nodeName = "node-acc"
	ctx := context.Background()
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-p"},
		Spec:       core.PodSpec{NodeName: nodeName},
	}
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()
	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}

	reread := func() *core.Pod {
		got := new(core.Pod)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), got))
		return got
	}
	chargedCards := func(p *core.Pod) []string {
		status, err := extractAllocatedStatusFromPod(p)
		require.NoError(t, err)
		var cards []string
		for i := range status.Groups {
			for j := range status.Groups[i].Accelerators {
				cards = append(cards, status.Groups[i].Accelerators[j].ID)
			}
		}
		return cards
	}

	require.NoError(t, rec.patchAllocatingPod(ctx, pod,
		"init", physicalSliceAllocation("grp-0", "dev-0", "1g.10gb", pl(0, 2)), []string{"grp-0:dev-0:0000"}))
	require.NoError(t, rec.patchAllocatingPod(ctx, reread(),
		"main", physicalSliceAllocation("grp-0", "dev-1", "1g.10gb", pl(0, 2)), []string{"grp-0:dev-1:0000"}))

	got := reread()
	allocations, err := AllocatedAcceleratorsOf(got)
	require.NoError(t, err)
	require.Len(t, allocations, 2, "both containers must be recorded")
	assert.Equal(t, []string{"grp-0:dev-0:0000"}, allocations["init"].DeviceIDs,
		"the device IDs kubelet offered are recorded with the allocation")
	assert.Equal(t, []string{"dev-0", "dev-1"}, chargedCards(got), "both cards stay charged")

	// Re-recording one container replaces its own entry instead of adding a second charge.
	require.NoError(t, rec.patchAllocatingPod(ctx, got,
		"main", physicalSliceAllocation("grp-0", "dev-1", "1g.10gb", pl(2, 2)), []string{"grp-0:dev-1:0001"}))
	assert.Equal(t, []string{"dev-0", "dev-1"}, chargedCards(reread()),
		"a repeated Allocate for one container is idempotent, not additive")

	// A stale informer copy must not erase a sibling's claim: the patch replaces the annotation's
	// whole value, so the in-process reservations for the pod's other containers are overlaid.
	rec.reserveDevices("uid-p", "init", physicalSliceAllocation("grp-0", "dev-0", "1g.10gb", pl(0, 2)), nil)
	require.NoError(t, rec.patchAllocatingPod(ctx, pod, // the pre-patch copy, as the informer would hand it back
		"main", physicalSliceAllocation("grp-0", "dev-1", "1g.10gb", pl(2, 2)), []string{"grp-0:dev-1:0001"}))
	assert.Equal(t, []string{"dev-0", "dev-1"}, chargedCards(reread()),
		"a sibling container's claim survives a patch built from a stale pod copy")
}

// TestDevicesReconciler_TerminatingPodStillCharges verifies a terminating Pod keeps charging its
// card. The reclaimer destroys an instance when the Pod object is gone, not when its containers
// exit, so dropping it at the deletion timestamp would advertise a slot the hardware still holds.
func TestDevicesReconciler_TerminatingPodStillCharges(t *testing.T) {
	const nodeName = "node-term"
	capture := &statusCapture{}
	pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
	deleting := meta.NewTime(pod.CreationTimestamp.Add(time.Second))
	pod.DeletionTimestamp = &deleting
	pod.Finalizers = []string{"gpustack.ai/test-hold"} // the fake client rejects a deleted object without one
	rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

	reconcileNode(t, rec, nodeName)

	mig := acceleratorByID(t, capture.last, "mig-0")
	assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}}, mig.AllocatedProfiles,
		"a terminating Pod's instance still occupies its card")
	assert.Equal(t, int32(1), remainingByProfile(t, capture.last, "mig-0", "3g.20gb"),
		"the free count must not rebound before the Pod object is gone")
}

// TestDevicesReconciler_TerminatedContainerStillCharges verifies a container that has already
// exited keeps charging its card while its Pod lives. The reclaimer and kubelet both scope a
// device to the Pod's life, so filtering by container liveness would report a card free while its
// instance still occupies memory slices — room the placement would then refuse. The assertion is
// on the resulting per-profile capacity, because the ledger fixture alone passes either way.
func TestDevicesReconciler_TerminatedContainerStillCharges(t *testing.T) {
	const nodeName = "node-exited"
	capture := &statusCapture{}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "p-init", Namespace: "default", UID: "uid-init",
			Annotations: allocationAnnotation(t, PodAllocations{
				"init": {Devices: physicalSliceAllocation("grp-0", "mig-0", "3g.20gb", pl(0, 4))},
			}),
		},
		Spec: core.PodSpec{NodeName: nodeName},
		Status: core.PodStatus{
			Phase: core.PodRunning,
			InitContainerStatuses: []core.ContainerStatus{{
				Name:  "init",
				State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}},
			}},
		},
	}
	rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

	reconcileNode(t, rec, nodeName)

	assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}},
		acceleratorByID(t, capture.last, "mig-0").AllocatedProfiles,
		"an exited container's instance still occupies its card")
	assert.Equal(t, int32(1), remainingByProfile(t, capture.last, "mig-0", "3g.20gb"),
		"the free count must not rebound while the Pod lives")
}

// statusCapture records the last DevicesStatus a reconcile writes via Status().Update. A
// fake-client interceptor is used (rather than reading the object back) because the
// Devices status carries a uint64 field the fake client's status-subresource path
// (structured-merge-diff) cannot handle; capturing the write asserts exactly what the
// reconciler produces.
type statusCapture struct{ last *workercore.DevicesStatus }

func acceleratorByID(t *testing.T, status *workercore.DevicesStatus, id string) workercore.AcceleratorAllocation {
	t.Helper()
	require.NotNil(t, status, "no status was written")
	for i := range status.Groups {
		for j := range status.Groups[i].Accelerators {
			if status.Groups[i].Accelerators[j].ID == id {
				return status.Groups[i].Accelerators[j]
			}
		}
	}
	t.Fatalf("accelerator %q not found in reconciled status", id)
	return workercore.AcceleratorAllocation{}
}

// remainingByProfile returns the reconciled RemainingProfiles count for a card's profile, 0 if absent.
func remainingByProfile(t *testing.T, status *workercore.DevicesStatus, id, profile string) int32 {
	t.Helper()
	acc := acceleratorByID(t, status, id)
	for _, pc := range acc.RemainingProfiles {
		if pc.Name == profile {
			return pc.Count
		}
	}
	return 0
}

func newReconciler(t *testing.T, nodeName string, capture *statusCapture, objs ...ctrlcli.Object) *DevicesReconciler {
	t.Helper()
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			SubResourceUpdate: func(_ context.Context, _ ctrlcli.Client, _ string, obj ctrlcli.Object, _ ...ctrlcli.SubResourceUpdateOption) error {
				capture.last = obj.(*workercore.Devices).Status.DeepCopy()
				return nil
			},
		}).
		Build()
	return &DevicesReconciler{NodeName: nodeName, Client: cli}
}

func reconcileNode(t *testing.T, rec *DevicesReconciler, nodeName string) {
	t.Helper()
	_, err := rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeName}})
	require.NoError(t, err)
}

// TestDevicesReconciler_PhysicalLedgerFold verifies the placement-aware MIG ledger is derived
// by pure annotation-merge (no NVML): an empty MIG card reports its full free ceilings and
// a logical card none; annotated placements fold to the worked-example Allocated/Free; two
// same-profile Pods at different slots reconstruct the real occupied set; the ledger is
// recomputed (not stomped) on a second reconcile; and missing occupancy only overstates
// Free, never understates it.
func TestDevicesReconciler_PhysicalLedgerFold(t *testing.T) {
	const nodeName = "node-mig"

	t.Run("empty MIG card reports full free, logical card empty", func(t *testing.T) {
		capture := &statusCapture{}
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName))

		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Nil(t, mig.AllocatedProfiles, "empty MIG card has no allocated instances")
		assert.Equal(t, []workercore.AcceleratorProfileCount{
			{Name: "1g.10gb", Count: 4},
			{Name: "1g.5gb", Count: 7},
			{Name: "2g.10gb", Count: 3},
			{Name: "3g.20gb", Count: 2},
		}, mig.RemainingProfiles, "empty MIG card reports full per-profile ceilings")

		logical := acceleratorByID(t, capture.last, "logical-0")
		assert.Nil(t, logical.AllocatedProfiles, "logical card carries no allocated ledger")
		assert.Nil(t, logical.RemainingProfiles, "logical card carries no free ledger")
	})

	t.Run("one 3g.20gb@slot0 folds to the worked-example ledger", func(t *testing.T) {
		capture := &statusCapture{}
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}}, mig.AllocatedProfiles)
		assert.Equal(t, []workercore.AcceleratorProfileCount{
			{Name: "1g.10gb", Count: 2},
			{Name: "1g.5gb", Count: 3},
			{Name: "2g.10gb", Count: 1},
			{Name: "3g.20gb", Count: 1},
		}, mig.RemainingProfiles, "matches the spec worked example")
	})

	t.Run("two same-profile pods at different slots reconstruct occupied", func(t *testing.T) {
		capture := &statusCapture{}
		p1 := physicalSlicePod(t, "p1", "grp-0", "mig-0", "1g.10gb", nodeName, pl(0, 2))
		p2 := physicalSlicePod(t, "p2", "grp-0", "mig-0", "1g.10gb", nodeName, pl(2, 2))
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), p1, p2)

		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "1g.10gb", Count: 2}}, mig.AllocatedProfiles,
			"two instances of the same profile counted")
		assert.Equal(t, []workercore.AcceleratorProfileCount{
			{Name: "1g.10gb", Count: 2},
			{Name: "1g.5gb", Count: 3},
			{Name: "2g.10gb", Count: 1},
			{Name: "3g.20gb", Count: 1},
		}, mig.RemainingProfiles, "free reflects both occupied slots [0,2)+[2,4), not the empty ceiling")
	})

	t.Run("ledger recomputed (not stomped) on a second reconcile", func(t *testing.T) {
		capture := &statusCapture{}
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

		reconcileNode(t, rec, nodeName)
		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}}, mig.AllocatedProfiles,
			"recomputed inside the wholesale build on the second pass, not lost")
		assert.NotEmpty(t, mig.RemainingProfiles)
	})

	t.Run("missing occupancy overstates free, never understates", func(t *testing.T) {
		// With the instance's annotation recorded, 3g.20gb free = 1. With it missing (a
		// crash-orphan the reconciler cannot see), free rises to the empty ceiling 2 — an
		// overstate. Free for the placements the reconciler does see is never inflated.
		recorded := &statusCapture{}
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		reconcileNode(t, newReconciler(t, nodeName, recorded, physicalAndLogicalDevices(nodeName), pod), nodeName)
		recordedFree := remainingByProfile(t, recorded.last, "mig-0", "3g.20gb")

		missing := &statusCapture{}
		reconcileNode(t, newReconciler(t, nodeName, missing, physicalAndLogicalDevices(nodeName)), nodeName)
		missingFree := remainingByProfile(t, missing.last, "mig-0", "3g.20gb")

		assert.GreaterOrEqual(t, missingFree, recordedFree, "missing occupancy only inflates free")
		assert.Equal(t, int32(1), recordedFree, "recorded instance leaves one 3g.20gb slot free")
		assert.Equal(t, int32(2), missingFree, "unrecorded orphan overstates free to the empty ceiling")
	})
}
