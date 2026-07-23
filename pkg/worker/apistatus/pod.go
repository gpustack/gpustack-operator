package apistatus

import (
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/kubeapistatus"
)

// podStatusPaths makes the following decision.
//
//	|  Condition Type  |     Condition Status    | Human Readable Status | Human Sensible Status |
//	| ---------------- | ----------------------- | --------------------- | --------------------- |
//	| PodScheduled     | Unknown                 | Scheduling            | Transitioning         |
//	| PodScheduled     | False                   | Pending               | Transitioning         |
//	| PodScheduled     | True                    | Scheduled             | /                     |
//	| PodInitialized   | Unknown                 | Initializing          | Transitioning         |
//	| PodInitialized   | False                   | InitializeFailed      | Interrupted           |
//	| PodInitialized   | True                    | Initialized           | /                     |
//	| PodReady         | Unknown                 | Starting              | Transitioning         |
//	| PodReady         | False                   | NotReady              | Interrupted           |
//	| PodReady         | True                    | Ready                 | /                     |
var podStatusPaths = kubeapistatus.NewSummarizer(
	[][]core.PodConditionType{
		{
			core.PodScheduled,
			core.PodInitialized,
			core.PodReady,
		},
	},
	func(d kubeapistatus.Decision[core.PodConditionType]) {
		d.Make(core.PodInitialized,
			func(st meta.ConditionStatus, reason string) (string, string, kubeapistatus.StatusScore) {
				switch st {
				case meta.ConditionUnknown:
					return "Initializing", "", kubeapistatus.StatusTransitioning
				case meta.ConditionFalse:
					if reason == "ContainersNotInitialized" {
						message := "One or more init containers are not initialized, " +
							"they may be pulling images or performing other startup operations, " +
							"please check events for more details."
						return "Initializing", message, kubeapistatus.StatusTransitioning
					}
					return "InitializeFailed", "", kubeapistatus.StatusInterrupted
				}
				return "Initialized", "", kubeapistatus.StatusDone
			})
		d.Make(core.PodScheduled,
			func(st meta.ConditionStatus, reason string) (string, string, kubeapistatus.StatusScore) {
				switch st {
				case meta.ConditionUnknown:
					return "Scheduling", "", kubeapistatus.StatusTransitioning
				case meta.ConditionFalse:
					return "Pending", "", kubeapistatus.StatusTransitioning
				}
				return "Scheduled", "", kubeapistatus.StatusDone
			})
		d.Make(core.PodReady,
			func(st meta.ConditionStatus, reason string) (string, string, kubeapistatus.StatusScore) {
				switch st {
				case meta.ConditionUnknown:
					return "Starting", "", kubeapistatus.StatusTransitioning
				case meta.ConditionFalse:
					if reason == "ContainersNotReady" {
						message := "One or more containers are not ready, " +
							"they may be pulling images or performing other startup operations, " +
							"please check events for more details."
						return "Starting", message, kubeapistatus.StatusTransitioning
					}
					return "NotReady", "", kubeapistatus.StatusInterrupted
				}
				return "Ready", "", kubeapistatus.StatusDone
			})
	},
)

// GetSummaryOfPod summarizes the given status by pod flow,
// and returns the phase and phase message.
//
// When the summarized phase is "Initializing" or "Starting", the container waiting states
// refine the result: the phase message is replaced with the waiting message of the first
// waiting container, surfacing the real reason, e.g. an image pull failure, instead of
// the generic startup hint; and the phase degrades to "NotReady" when the waiting reason
// indicates an image pull failure, which hardly recovers without user intervention.
func GetSummaryOfPod(podStatus *core.PodStatus) (phase, phaseMessage string) {
	phase, phaseMessage = podStatusPaths.GetSummary(podStatus)
	if phase != "Initializing" && phase != "Starting" {
		return phase, phaseMessage
	}
	waiting := getFirstWaitingContainerState(podStatus)
	if waiting == nil {
		return phase, phaseMessage
	}
	if waiting.Message != "" {
		phaseMessage = waiting.Message
	}
	if isImagePullWaitingReason(waiting.Reason) {
		phase = "NotReady"
	}
	return phase, phaseMessage
}

// getFirstWaitingContainerState returns the waiting state of the first waiting container,
// preferring one whose reason indicates an image pull failure.
func getFirstWaitingContainerState(podStatus *core.PodStatus) *core.ContainerStateWaiting {
	var first *core.ContainerStateWaiting
	for _, statuses := range [2][]core.ContainerStatus{podStatus.InitContainerStatuses, podStatus.ContainerStatuses} {
		for i := range statuses {
			waiting := statuses[i].State.Waiting
			if waiting == nil {
				continue
			}
			if isImagePullWaitingReason(waiting.Reason) {
				return waiting
			}
			if first == nil {
				first = waiting
			}
		}
	}
	return first
}

// isImagePullWaitingReason reports whether the given container waiting reason indicates
// that the container image cannot be pulled.
func isImagePullWaitingReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		return true
	}
	return false
}
