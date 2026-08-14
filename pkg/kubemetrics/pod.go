package kubemetrics

import (
	core "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// HasStartedContainer reports whether any container of the pod has ever started.
//
// It is the gate both surfaces apply before measuring anything. An Instance none of whose
// containers has started consumes nothing, so its usage is zero by reasoning rather than by
// measurement, and reading the kubelet, the metrics API and every device manager for it spends
// three requests to learn that — or to serve another tenant's figures as its own.
//
// The predicate is "has started", NOT "is ready". A pod can be running with a failing readiness
// probe, an unready sidecar, or a termination already begun while its main container holds
// accelerator memory; reporting zero for it would fabricate an idle measurement. For the same
// reason every trace a container leaves counts — a restart, a previous termination, an init or
// ephemeral container running — because each of those can only open the gate, which is the
// direction that never invents a measurement.
//
// A nil pod has started nothing. That is a caller holding an Instance and no pod of its own:
// stopped, unscheduled, or carrying only a previous incarnation's pod. It takes the same
// closed-gate answer as a pod that has not started yet, so the gate has one answer rather than
// two.
func HasStartedContainer(pod *core.Pod) bool {
	if pod == nil {
		return false
	}
	for _, statuses := range [][]core.ContainerStatus{
		pod.Status.InitContainerStatuses,
		pod.Status.ContainerStatuses,
		pod.Status.EphemeralContainerStatuses,
	} {
		for i := range statuses {
			if containerStarted(&statuses[i]) {
				return true
			}
		}
	}
	return false
}

// containerStarted reports whether one container status shows a container that has run at some
// point, whether or not it is running now: a container that has already exited held whatever it
// held, and a container waiting to be restarted has run before.
func containerStarted(cs *core.ContainerStatus) bool {
	switch {
	case cs.State.Running != nil, cs.State.Terminated != nil:
		return true
	case cs.RestartCount > 0:
		return true
	case cs.LastTerminationState.Terminated != nil, cs.LastTerminationState.Running != nil:
		return true
	}
	return ptr.Deref(cs.Started, false)
}

// NewUnstartedSample returns the sample of an Instance none of whose containers has started: the
// declared totals, and every measured figure present and zero.
//
// Zero here is a measurement, not a default. Nothing has started, so nothing exists that could
// have consumed anything — which is exactly the claim a present zero makes, and the reason this
// path takes no reading at all.
//
// The totals come from the pod's container limits when there is a pod, and from the Instance's
// own spec.resources when there is not; the two agree for the same declaration, because both
// funnel through the same rounding arithmetic. A caller with neither — an Instance declaring no
// resources — gets zero totals, which reads as "no declared ceiling" exactly as an unlimited
// container does.
func NewUnstartedSample(
	pod *core.Pod,
	resources *workercore.InstanceResources,
) *worker.InstanceMetricsSample {
	var sample *worker.InstanceMetricsSample
	switch {
	case pod != nil:
		sample = NewSample(pod)
	case resources != nil:
		sample = NewSampleFromResources(*resources)
	default:
		sample = NewSampleFromResources(workercore.InstanceResources{})
	}

	sample.CPUUsedMilliCores = ptr.To[uint64](0)
	sample.MemoryUsedMiB = ptr.To[uint64](0)
	sample.StorageUsedMiB = ptr.To[uint64](0)
	return sample
}
