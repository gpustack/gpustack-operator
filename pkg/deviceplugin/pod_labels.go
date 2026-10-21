package deviceplugin

import (
	core "k8s.io/api/core/v1"
)

// The labels the chart and the Instance controller stamp on Pods, and which several components
// read back. They live here, beside the Pod index and the allocation readers, because a second
// copy of a contract is a contract that can drift: rename one in the chart, update one of the
// copies, and the component reading the other silently stops matching anything — no compile
// error, and no test failure where each package pins its own copy.
const (
	// ComponentLabelKey names a Pod's component, and DeviceManagerComponent is the value the
	// chart stamps on every manufacturer's device manager DaemonSet.
	ComponentLabelKey      = "app.kubernetes.io/component"
	DeviceManagerComponent = "device-manager"

	// InstanceResourceType is the systemmeta.ResourceTypeLabel value the Instance controller
	// stamps on every Instance Pod, and the tenant-isolation NetworkPolicy selects Instance
	// Pods by. Unlike InstancePartOfLabelKey, which carries one UID per Instance, this value
	// is stable across all Instances, so one policy per namespace can select them all. It
	// rides the label the operator already notes its rendered objects with, so the policy,
	// the Pods it guards and the rest of the rendered family read as one vocabulary — and a
	// third-party workload stamping a generic recommended label cannot walk into the
	// isolation rules by accident.
	InstanceResourceType = "instances"

	// ManufacturerLabelKey carries the manufacturer a device manager pod was rolled for. The
	// chart rolls one DaemonSet per manufacturer and stamps this on each.
	ManufacturerLabelKey = "gpustack.ai/manufacturer"

	// InstancePartOfLabelKey carries the backing Instance's UID, stamped on the Pod by the
	// Instance controller.
	InstancePartOfLabelKey = "app.kubernetes.io/part-of"
)

// IsPodReady reports whether the pod carries a true Ready condition.
func IsPodReady(pod *core.Pod) bool {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == core.PodReady {
			return pod.Status.Conditions[i].Status == core.ConditionTrue
		}
	}
	return false
}
