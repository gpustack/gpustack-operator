package deviceplugin

import (
	"context"
	"encoding/json"
	"testing"

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

// physicalAndSoftDevices returns a node's Devices with one MIG-enabled card (mig-0, a non-empty
// PhysicalSliced.Profiles carrying cached Placements) and one soft-slice card (soft-0), so
// a reconcile exercises the ledger fold on the MIG card and its absence on the soft card.
func physicalAndSoftDevices(nodeName string) *workercore.Devices {
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
						ID:     "soft-0",
						Index:  1,
						Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 128}},
					},
				},
			}},
		},
	}
}

// physicalSlicePod builds a Pod whose allocation annotation records one MIG instance of profile on
// (group, device) at the given placements — the upward record the reconciler unions into
// the card's occupied set.
func physicalSlicePod(t *testing.T, name, group, device, profile, node string, placements ...workercore.AcceleratorPhysicalPlacement) *core.Pod {
	t.Helper()
	allocated := workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
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
	annoBytes, err := json.Marshal(allocated)
	require.NoError(t, err)
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			UID:         types.UID(name),
			Annotations: map[string]string{AllocatedAcceleratorAnnoKey: string(annoBytes)},
		},
		Spec: core.PodSpec{NodeName: node},
	}
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
// a soft card none; annotated placements fold to the worked-example Allocated/Free; two
// same-profile Pods at different slots reconstruct the real occupied set; the ledger is
// recomputed (not stomped) on a second reconcile; and missing occupancy only overstates
// Free, never understates it.
func TestDevicesReconciler_PhysicalLedgerFold(t *testing.T) {
	const nodeName = "node-mig"

	t.Run("empty MIG card reports full free, soft card empty", func(t *testing.T) {
		capture := &statusCapture{}
		rec := newReconciler(t, nodeName, capture, physicalAndSoftDevices(nodeName))

		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Nil(t, mig.AllocatedProfiles, "empty MIG card has no allocated instances")
		assert.Equal(t, []workercore.AcceleratorProfileCount{
			{Name: "1g.10gb", Count: 4},
			{Name: "1g.5gb", Count: 7},
			{Name: "2g.10gb", Count: 3},
			{Name: "3g.20gb", Count: 2},
		}, mig.RemainingProfiles, "empty MIG card reports full per-profile ceilings")

		soft := acceleratorByID(t, capture.last, "soft-0")
		assert.Nil(t, soft.AllocatedProfiles, "soft card carries no allocated ledger")
		assert.Nil(t, soft.RemainingProfiles, "soft card carries no free ledger")
	})

	t.Run("one 3g.20gb@slot0 folds to the worked-example ledger", func(t *testing.T) {
		capture := &statusCapture{}
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		rec := newReconciler(t, nodeName, capture, physicalAndSoftDevices(nodeName), pod)

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
		rec := newReconciler(t, nodeName, capture, physicalAndSoftDevices(nodeName), p1, p2)

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
		rec := newReconciler(t, nodeName, capture, physicalAndSoftDevices(nodeName), pod)

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
		reconcileNode(t, newReconciler(t, nodeName, recorded, physicalAndSoftDevices(nodeName), pod), nodeName)
		recordedFree := remainingByProfile(t, recorded.last, "mig-0", "3g.20gb")

		missing := &statusCapture{}
		reconcileNode(t, newReconciler(t, nodeName, missing, physicalAndSoftDevices(nodeName)), nodeName)
		missingFree := remainingByProfile(t, missing.last, "mig-0", "3g.20gb")

		assert.GreaterOrEqual(t, missingFree, recordedFree, "missing occupancy only inflates free")
		assert.Equal(t, int32(1), recordedFree, "recorded instance leaves one 3g.20gb slot free")
		assert.Equal(t, int32(2), missingFree, "unrecorded orphan overstates free to the empty ceiling")
	})
}
