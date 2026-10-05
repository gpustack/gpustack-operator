package deviceplugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestDevicesReconciler_KubeletConflictRefusesRebuildAndAllocate(t *testing.T) {
	for _, tc := range []struct {
		name           string
		annotationCard string
		runtimeID      string
		missingPod     bool
		unavailable    bool
		wantError      string
	}{
		{name: "stale annotation after restart", annotationCard: "dev-1", runtimeID: "grp-0:dev-0:0000", wantError: "allocation identity conflict"},
		{name: "missing annotation", runtimeID: "grp-0:dev-0:0000", wantError: "allocation identity conflict"},
		{name: "unknown accelerator", annotationCard: "dev-0", runtimeID: "grp-0:unknown:0000", wantError: "absent from inventory"},
		{name: "invalid token", annotationCard: "dev-0", runtimeID: "invalid", wantError: "parse kubelet device ID"},
		{name: "uncached runtime pod", missingPod: true, runtimeID: "grp-0:dev-0:0000", wantError: "uncached pod"},
		{name: "unavailable kubelet", annotationCard: "dev-0", unavailable: true, wantError: "verify kubelet device assignments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			const node = "node-bookkeeping"
			devs := twoCardDevices(node, workercore.DeviceAllocationModeExclusive)
			pod := concurrentAllocatePod(node, "served", "uid-served", workercore.DeviceAllocationModeExclusive, 1)
			pod.Status.Phase = core.PodRunning
			if tc.annotationCard != "" {
				pod.Annotations = wholeCardAnnotation(t, tc.annotationCard)
			}
			objects := []ctrlcli.Object{devs}
			if !tc.missingPod {
				objects = append(objects, pod)
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(objects...).
				WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string { return []string{obj.(*core.Pod).Spec.NodeName} }).Build()
			rec := &DevicesReconciler{NodeName: node, Client: cli, kubeletPods: func(context.Context) ([]*podresources.PodResources, error) {
				return wholeCardRuntime(pod, tc.runtimeID), nil
			}}
			if tc.unavailable {
				rec.kubeletPods = kubeletUnavailable
			}
			// A fresh reconciler has no reservations. Refusal must rely on durable and runtime records.
			for range 2 {
				_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: node}})
				require.ErrorContains(t, err, tc.wantError)
				got := new(workercore.Devices)
				require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(devs), got))
				require.Equal(t, devs.Status, got.Status, "conflict must not publish a guessed ledger")
				server := &ResourceServer{Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeExclusive, Reconciler: rec, Responder: stubResponder{}}
				_, err = server.Allocate(ctx, &AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-1:0000"}}}})
				require.Equal(t, grpccodes.FailedPrecondition, grpcstatus.Code(err))
				require.Contains(t, err.Error(), tc.wantError)
			}
			if !tc.missingPod {
				got := new(core.Pod)
				require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), got))
				require.Equal(t, pod.Annotations, got.Annotations, "refusal must preserve all durable claims")
			}
		})
	}
}

func TestDevicesReconciler_KubeletConsistentAndLostCheckpoint(t *testing.T) {
	for _, checkpointLost := range []bool{false, true} {
		t.Run(map[bool]string{false: "consistent", true: "checkpoint lost"}[checkpointLost], func(t *testing.T) {
			ctx := context.Background()
			const node = "node-bookkeeping"
			devs := twoCardDevices(node, workercore.DeviceAllocationModeNone)
			pod := concurrentAllocatePod(node, "served", "uid-served", workercore.DeviceAllocationModeExclusive, 1)
			pod.Status.Phase = core.PodRunning
			pod.Annotations = wholeCardAnnotation(t, "dev-0")
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(devs, pod).
				WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string { return []string{obj.(*core.Pod).Spec.NodeName} }).Build()
			rec := &DevicesReconciler{NodeName: node, Client: cli, kubeletPods: func(context.Context) ([]*podresources.PodResources, error) {
				if checkpointLost {
					return nil, nil
				}
				return wholeCardRuntime(pod, "grp-0:dev-0:0000"), nil
			}}
			pods := &core.PodList{Items: []core.Pod{*pod}}
			_, err := rec.verifyKubeletAllocations(ctx, devs, pods)
			require.NoError(t, err)
			status, _ := BuildDesiredStatus(ctrl.LoggerFrom(ctx), devs, pods)
			require.EqualValues(t, 0, status.Groups[0].Accelerators[0].Remaining)
			require.EqualValues(t, nodefeature.ResourceMaxUnits, status.Groups[0].Accelerators[1].Remaining)
		})
	}
}

// TestDevicesReconciler_KubeletConflictStillPaysPendingReleases pins that a kubelet record this node
// cannot verify blocks the LEDGER and not the node's bookkeeping. The compensating write that hands
// an accelerator back is owed whatever else kubelet reports, because no event brings this reconciler
// back for it: the Pod watch fires on the change that is itself failing. A pass that returned the
// conflict before that write left a device reading as held for as long as the conflict lasted.
func TestDevicesReconciler_KubeletConflictStillPaysPendingReleases(t *testing.T) {
	ctx := context.Background()
	const node = "node-conflict-compensation"
	// The served pod holds a claim and owes a give-back. It is unrelated to the conflict below.
	served := concurrentAllocatePod(node, "served", "uid-served", workercore.DeviceAllocationModeExclusive, 1)
	served.Status.Phase = core.PodRunning
	served.Annotations = wholeCardAnnotation(t, "dev-0")
	devs := twoCardDevices(node, workercore.DeviceAllocationModeNone)
	// Kubelet still holds an accelerator for a pod the API no longer has: the conflict this pass
	// cannot verify around.
	vanished := concurrentAllocatePod(node, "vanished", "uid-vanished", workercore.DeviceAllocationModeExclusive, 1)

	failing := false
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(devs, served).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			Patch: func(
				ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object,
				patch ctrlcli.Patch, opts ...ctrlcli.PatchOption,
			) error {
				if failing {
					return errors.New("the api server is unreachable")
				}
				return cli.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	rec := &DevicesReconciler{NodeName: node, Client: cli, kubeletPods: func(context.Context) ([]*podresources.PodResources, error) {
		return wholeCardRuntime(vanished, "grp-0:dev-1:0000"), nil
	}}

	// Seed the give-back the way a refused allocation leaves it: the patch cannot land, so the entry
	// waits in the pending table for a pass to pay it.
	failing = true
	require.Error(t, rec.unpatchAllocatingPod(ctx, served, workloadContainer, nil))
	require.Equal(t, []string{"uid-served/" + workloadContainer}, pendingReleaseKeys(rec))
	failing = false

	_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: node}})
	require.ErrorContains(t, err, "uncached pod", "the conflict is still reported")
	require.Empty(t, pendingReleaseKeys(rec),
		"a give-back owed to an unrelated pod must not wait on a conflict about another one")
	recorded := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(served), recorded))
	allocations, err := AllocatedAcceleratorsOf(recorded)
	require.NoError(t, err)
	require.NotContains(t, allocations, workloadContainer,
		"the compensation landed during the very pass that refused the ledger")
	published := new(workercore.Devices)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(devs), published))
	require.Equal(t, devs.Status, published.Status, "and the conflict still publishes no guessed ledger")
}

func wholeCardAnnotation(t *testing.T, card string) map[string]string {
	t.Helper()
	index := uint32(0)
	if card == "dev-1" {
		index = 1
	}
	encoded, err := json.Marshal(PodAllocations{workloadContainer: {
		DeviceIDs: []string{"grp-0:" + card + ":0000"},
		Devices: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
			ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{ID: card, Index: index, Mode: workercore.DeviceAllocationModeExclusive, Allocated: nodefeature.ResourceMaxUnits}},
		}}},
	}})
	require.NoError(t, err)
	return map[string]string{AllocatedAcceleratorAnnoKey: string(encoded)}
}

func wholeCardRuntime(pod *core.Pod, id string) []*podresources.PodResources {
	return []*podresources.PodResources{{Name: pod.Name, Namespace: pod.Namespace, Containers: []*podresources.ContainerResources{{
		Name: workloadContainer, Devices: []*podresources.ContainerDevices{{ResourceName: string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)), DeviceIds: []string{id}}},
	}}}}
}
