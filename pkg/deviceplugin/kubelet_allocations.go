package deviceplugin

import (
	"context"
	"fmt"
	"slices"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// verifiedKubeletPending checks the ledger and identifies candidates from the same kubelet snapshot.
func (r *DevicesReconciler) verifiedKubeletPending(ctx context.Context, resource core.ResourceName) (*_KubeletPending, error) {
	if r.kubeletPods == nil {
		return nil, nil
	}
	devs, err := r.getDevices(ctx)
	if err != nil {
		return nil, err
	}
	pods := new(core.PodList)
	if err := r.Client.List(ctx, pods, ctrlcli.MatchingFields{IndexingPodsByNodeName: r.NodeName}); err != nil {
		return nil, err
	}
	listed, err := r.verifyKubeletAllocations(ctx, devs, pods)
	if err != nil {
		return nil, err
	}
	return kubeletPendingFrom(listed, resource), nil
}

// manufacturableAccelerator is the manufacturer and allocation mode a resource name encodes.
type manufacturableAccelerator struct {
	manufacturer string
	mode         workercore.DeviceAllocationMode
}

// manufacturableAccelerators indexes every resource name this verification answers to, so one
// lookup replaces a rebuild of the name for every manufacturer and mode under every reported device.
// Neither the manufacturers nor the modes vary with the node or with the call, and the names are
// settled during nodefeature's own initialization, so the table is built once and read thereafter.
// The base names are distinct and the shared mode appends a suffix no base name carries, so each
// resource name resolves to exactly one manufacturer and mode.
var manufacturableAccelerators = func() map[string]manufacturableAccelerator {
	table := make(map[string]manufacturableAccelerator)
	for _, manufacturer := range nodefeature.GetKnownAcceleratableManufacturers() {
		for _, mode := range []workercore.DeviceAllocationMode{
			workercore.DeviceAllocationModeExclusive, workercore.DeviceAllocationModeShared,
		} {
			name := string(nodefeature.GetAcceleratableResourceName(manufacturer, mode))
			table[name] = manufacturableAccelerator{manufacturer: manufacturer, mode: mode}
		}
	}

	return table
}()

// verifyKubeletAllocations refuses to publish or allocate from records that contradict kubelet.
// Missing runtime records are preserved: checkpoint loss does not prove that a device is free.
// Podresources has no Pod UID, so conflicts require operator recovery instead of automatic rewrites.
func (r *DevicesReconciler) verifyKubeletAllocations(
	ctx context.Context, devs *workercore.Devices, pods *core.PodList,
) ([]*podresources.PodResources, error) {
	if r.kubeletPods == nil {
		return nil, nil
	}
	listed, err := r.kubeletPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify kubelet device assignments: %w", err)
	}
	for _, reportedPod := range listed {
		for _, container := range reportedPod.GetContainers() {
			// Kubelet can split one resource's devices into several blocks.
			resources := make(map[string][]string)
			for _, devices := range container.GetDevices() {
				name := devices.GetResourceName()
				resources[name] = append(resources[name], devices.GetDeviceIds()...)
			}
			for name, deviceIDs := range resources {
				if len(deviceIDs) == 0 {
					continue
				}
				accelerator, ok := manufacturableAccelerators[name]
				if !ok {
					continue
				}
				if err := verifyKubeletContainer(devs, pods, reportedPod, container.GetName(),
					deviceIDs, accelerator.manufacturer, accelerator.mode); err != nil {
					return nil, err
				}
			}
		}
	}
	return listed, nil
}

// Whole-card token IDs determine the complete claim. Slice geometry requires its durable record.
func verifyKubeletContainer(devs *workercore.Devices, pods *core.PodList,
	reported *podresources.PodResources, container string, deviceIDs []string, manufacturer string, mode workercore.DeviceAllocationMode,
) error {
	index := slices.IndexFunc(pods.Items, func(p core.Pod) bool { return p.Name == reported.GetName() && p.Namespace == reported.GetNamespace() })
	if index < 0 {
		return fmt.Errorf("kubelet holds accelerators for uncached pod %s/%s; refusing a ledger rebuild", reported.GetNamespace(), reported.GetName())
	}
	pod := &pods.Items[index]
	if pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed {
		return nil
	}
	tokens := make(map[Resource][]ResourceToken)
	for _, id := range deviceIDs {
		token, err := ParseResourceToken(id)
		if err != nil {
			return fmt.Errorf("parse kubelet device ID for pod %s: %w", ctrlcli.ObjectKeyFromObject(pod), err)
		}
		tokens[token.Resource] = append(tokens[token.Resource], token)
	}
	server := &ResourceServer{Manufacturer: manufacturer, AllocationMode: mode}
	expected, allocation := server.accumulateAllocation(devs, tokens, 0)
	if len(allocation) != len(tokens) {
		return fmt.Errorf("kubelet assigns an accelerator absent from inventory to pod %s", ctrlcli.ObjectKeyFromObject(pod))
	}
	allocations, err := AllocatedAcceleratorsOf(pod)
	if err != nil {
		return fmt.Errorf("read allocation of pod %s: %w", ctrlcli.ObjectKeyFromObject(pod), err)
	}
	claim := allocations[container]
	if !sets.New(claim.DeviceIDs...).Equal(sets.New(deviceIDs...)) || !kubemeta.DeepEqual(claim.Devices, expected) {
		return fmt.Errorf(
			"allocation identity conflict for pod %s container %q: annotation differs from kubelet device IDs %v; refusing device bookkeeping until recovery",
			ctrlcli.ObjectKeyFromObject(pod), container, deviceIDs,
		)
	}
	return nil
}
