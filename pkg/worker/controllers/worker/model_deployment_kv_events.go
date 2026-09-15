package worker

import (
	"fmt"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
)

// ModelDeploymentConditionKVEventsPublishing reports whether every role that produces cache blocks
// was configured to publish events.
//
// It reports configuration, not traffic. A publisher that was configured and then crashed remains
// True here because observing the event stream requires a live consumer and is a separate check.
const ModelDeploymentConditionKVEventsPublishing kubeapistatus.ConditionType = "KVEventsPublishing"

const (
	modelDeploymentReasonKVEventsPublishing        = "Publishing"
	modelDeploymentReasonKVEventsNotApplicable     = "NotApplicable"
	modelDeploymentReasonKVEventsPublisherDisabled = "PublisherDisabled"
	modelDeploymentReasonKVEventsNoRouter          = "NoRouter"
	modelDeploymentReasonKVEventsRoleUnmanaged     = "RoleUnmanaged"
)

func observeModelDeploymentKVEvents(
	md, holder *workercore.ModelDeployment, manufacturers map[string]string,
) {
	if md.Spec.Router == nil {
		for i := range md.Spec.Roles {
			if ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) != workercore.ModelDeploymentRoleKindServer {
				ModelDeploymentConditionKVEventsPublishing.False(holder,
					modelDeploymentReasonKVEventsNoRouter,
					"the deployment declares prefill and decode roles but no router consumes their cache events")
				return
			}
		}

		ModelDeploymentConditionKVEventsPublishing.True(holder,
			modelDeploymentReasonKVEventsNotApplicable,
			"the unrouted deployment has only server roles, so nothing consumes cache events")
		return
	}

	configured := 0
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if ModelDeploymentEffectiveRoleKind(role) == workercore.ModelDeploymentRoleKindDecode {
			continue
		}
		if role.Template != nil && len(role.Template.Command) > 0 {
			ModelDeploymentConditionKVEventsPublishing.Unknown(holder,
				modelDeploymentReasonKVEventsRoleUnmanaged,
				fmt.Sprintf("role %q replaced the whole command line, so the operator cannot say whether it publishes cache events", role.Name))
			return
		}
		if !modelDeploymentPublishesKVEvents(md, role, manufacturers[role.Name]) {
			ModelDeploymentConditionKVEventsPublishing.False(holder,
				modelDeploymentReasonKVEventsPublisherDisabled,
				fmt.Sprintf("role %q produces cache blocks but its rendered configuration does not enable event publishing", role.Name))
			return
		}
		configured++
	}

	ModelDeploymentConditionKVEventsPublishing.True(holder,
		modelDeploymentReasonKVEventsPublishing,
		fmt.Sprintf("all %d producing roles are configured to publish cache events", configured))
}
