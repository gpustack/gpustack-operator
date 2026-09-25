package deviceplugin

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// LedgerReleaseTerminatedPodsEnv turns off, when set to "false", the release of what a Pod in a
// terminal phase held: the ledger then charges a Succeeded or Failed Pod until its object is gone,
// as it did before the release existed. Both the device manager, which publishes the ledger, and
// the worker, whose node-devices check rebuilds it, read it once at startup.
const LedgerReleaseTerminatedPodsEnv = "GPUSTACK_LEDGER_RELEASE_TERMINATED_PODS"

// terminatedPodsReleased reports whether a Pod in a terminal phase stops charging its logical
// allocations. It is a variable so a test can set it.
var terminatedPodsReleased = func() bool { return releaseTerminatedPodsAtStartup }

var releaseTerminatedPodsAtStartup = osx.Getenv(LedgerReleaseTerminatedPodsEnv) != "false"

// statefulSlicedManufacturers names the manufacturers whose logical slice is a driver object rather
// than a share of an accelerator: their allocator's reclaim loop destroys it once the Pod object is
// gone, not when the Pod's containers stop, so until then a new slice on the same accelerator cannot
// be created. It must name every manufacturer whose allocator starts RunReclaimLoop for
// DeviceAllocationModeSliced; a test holds the two equal.
var statefulSlicedManufacturers = sets.New(nodefeature.ManufacturerMetaX, nodefeature.ManufacturerCambricon)

// podTerminated reports whether the Pod is in a terminal phase. kubelet sets one only once every
// container has stopped, never restarts a container afterwards, and returns the Pod's devices.
func podTerminated(pod *core.Pod) bool {
	return pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed
}

// heldAllocation returns the part of a Pod's recorded allocation the Pod still holds.
//
// A Pod in a terminal phase holds nothing kubelet hands out: an exclusive or shared card, or a
// logical slice, whose isolation lives in the container and ends with it. It still holds what is
// destroyed only once its object is gone: a hardware partition, and a stateful manufacturer's slice.
// Everything else stays charged until the Pod object is gone, whatever its phase, and so does
// everything when LedgerReleaseTerminatedPodsEnv is "false".
func heldAllocation(pod *core.Pod, status workercore.DevicesStatus) workercore.DevicesStatus {
	if !podTerminated(pod) || !terminatedPodsReleased() {
		return status
	}
	held := workercore.DevicesStatus{Groups: make([]workercore.DevicesAllocationGroup, 0, len(status.Groups))}
	for i := range status.Groups {
		grp := status.Groups[i]
		grp.Accelerators = nil
		for _, acc := range status.Groups[i].Accelerators {
			if heldAfterTermination(grp.Manufacturer, &acc) {
				grp.Accelerators = append(grp.Accelerators, acc)
			}
		}
		if len(grp.Accelerators) != 0 {
			held.Groups = append(held.Groups, grp)
		}
	}
	return held
}

// heldAfterTermination reports whether an accelerator a Pod recorded stays held once the Pod's phase
// is terminal. A physical record counts as a partition whatever mode it was recorded under.
func heldAfterTermination(manufacturer string, acc *workercore.AcceleratorAllocation) bool {
	if acc.AllocatedPhysicalProfile != "" || len(acc.AllocatedPhysicalPlacements) != 0 {
		return true
	}
	switch acc.Mode {
	case workercore.DeviceAllocationModeExclusive, workercore.DeviceAllocationModeShared:
		return false
	case workercore.DeviceAllocationModeSliced:
		return statefulSlicedManufacturers.Has(manufacturer)
	default:
		return true
	}
}

// podUpdateChangesLedger reports whether a Pod update changes what the ledger rebuilt from it
// charges: its allocation record changed, its deletion started or moved, or it reached a terminal
// phase while holding a record.
func podUpdateChangesLedger(e ctrlevent.UpdateEvent) bool {
	oldPod, newPod := e.ObjectOld, e.ObjectNew
	if kubemeta.HasAnnotation(newPod, AllocatedAcceleratorAnnoKey) &&
		podTerminated(newPod.(*core.Pod)) && !podTerminated(oldPod.(*core.Pod)) {
		return true
	}
	if newPod.GetDeletionTimestamp() == nil {
		return !mapx.EqualWithKey(oldPod.GetAnnotations(), newPod.GetAnnotations(), AllocatedAcceleratorAnnoKey)
	}
	if kubemeta.HasAnnotation(oldPod, AllocatedAcceleratorAnnoKey) {
		if oldPod.GetDeletionTimestamp() == nil {
			return true
		}
		return !oldPod.GetDeletionTimestamp().Equal(newPod.GetDeletionTimestamp())
	}
	return false
}
