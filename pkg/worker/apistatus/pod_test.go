package apistatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
)

func TestGetSummaryOfPod(t *testing.T) {
	readyConditions := []core.PodCondition{
		{Type: core.PodScheduled, Status: core.ConditionTrue},
		{Type: core.PodInitialized, Status: core.ConditionTrue},
		{Type: core.PodReady, Status: core.ConditionTrue},
	}
	notReadyConditions := []core.PodCondition{
		{Type: core.PodScheduled, Status: core.ConditionTrue},
		{Type: core.PodInitialized, Status: core.ConditionTrue},
		{Type: core.PodReady, Status: core.ConditionFalse, Reason: "ContainersNotReady"},
	}
	notInitializedConditions := []core.PodCondition{
		{Type: core.PodScheduled, Status: core.ConditionTrue},
		{Type: core.PodInitialized, Status: core.ConditionFalse, Reason: "ContainersNotInitialized"},
	}

	cases := []struct {
		name string

		podStatus *core.PodStatus

		wantPhase   string
		wantMessage string // asserted only when non-empty
	}{
		{
			name: "ready pod is ready",
			podStatus: &core.PodStatus{
				Conditions: readyConditions,
			},
			wantPhase: "Ready",
		},
		{
			name: "image pull backoff degrades to not ready with the waiting message",
			podStatus: &core.PodStatus{
				Conditions: notReadyConditions,
				ContainerStatuses: []core.ContainerStatus{
					{
						Name: "main",
						State: core.ContainerState{
							Waiting: &core.ContainerStateWaiting{
								Reason:  "ImagePullBackOff",
								Message: `Back-off pulling image "example.com/missing:latest": not found`,
							},
						},
					},
				},
			},
			wantPhase:   "NotReady",
			wantMessage: `Back-off pulling image "example.com/missing:latest": not found`,
		},
		{
			name: "invalid image name degrades to not ready",
			podStatus: &core.PodStatus{
				Conditions: notReadyConditions,
				ContainerStatuses: []core.ContainerStatus{
					{
						Name: "main",
						State: core.ContainerState{
							Waiting: &core.ContainerStateWaiting{
								Reason:  "InvalidImageName",
								Message: `Failed to apply default image tag "example.com/@@": invalid reference format`,
							},
						},
					},
				},
			},
			wantPhase:   "NotReady",
			wantMessage: `Failed to apply default image tag "example.com/@@": invalid reference format`,
		},
		{
			name: "init container image pull failure degrades to not ready",
			podStatus: &core.PodStatus{
				Conditions: notInitializedConditions,
				InitContainerStatuses: []core.ContainerStatus{
					{
						Name: "init",
						State: core.ContainerState{
							Waiting: &core.ContainerStateWaiting{
								Reason:  "ErrImagePull",
								Message: `rpc error: code = NotFound desc = failed to pull image`,
							},
						},
					},
				},
			},
			wantPhase:   "NotReady",
			wantMessage: `rpc error: code = NotFound desc = failed to pull image`,
		},
		{
			name: "non-image-pull waiting message is surfaced without degrading the phase",
			podStatus: &core.PodStatus{
				Conditions: notReadyConditions,
				ContainerStatuses: []core.ContainerStatus{
					{
						Name: "main",
						State: core.ContainerState{
							Waiting: &core.ContainerStateWaiting{
								Reason:  "CrashLoopBackOff",
								Message: "back-off 5m0s restarting failed container",
							},
						},
					},
				},
			},
			wantPhase:   "Starting",
			wantMessage: "back-off 5m0s restarting failed container",
		},
		{
			name: "waiting container without message keeps the generic hint",
			podStatus: &core.PodStatus{
				Conditions: notReadyConditions,
				ContainerStatuses: []core.ContainerStatus{
					{
						Name: "main",
						State: core.ContainerState{
							Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"},
						},
					},
				},
			},
			wantPhase: "Starting",
			wantMessage: "One or more containers are not ready, " +
				"they may be pulling images or performing other startup operations, " +
				"please check events for more details.",
		},
		{
			name: "no waiting container keeps the generic hint",
			podStatus: &core.PodStatus{
				Conditions: notReadyConditions,
			},
			wantPhase: "Starting",
			wantMessage: "One or more containers are not ready, " +
				"they may be pulling images or performing other startup operations, " +
				"please check events for more details.",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			phase, message := GetSummaryOfPod(c.podStatus)
			assert.Equal(t, c.wantPhase, phase, "phase")
			if c.wantMessage != "" {
				assert.Equal(t, c.wantMessage, message, "message")
			}
		})
	}
}
