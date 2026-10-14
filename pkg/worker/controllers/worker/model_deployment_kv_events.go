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
			// AN ENCODER IS NOT ONE OF THE ROLES THE MESSAGE NAMES. It reads media and hands
			// embeddings to a prefiller, so it neither produces nor consumes cache blocks; counting
			// it would report a deployment as needing a router for a role no router could serve.
			switch ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) {
			case workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode:
				ModelDeploymentConditionKVEventsPublishing.False(holder,
					modelDeploymentReasonKVEventsNoRouter,
					"the deployment declares prefill and decode roles but no router consumes their cache events")
				return
			}
		}

		// SO THE MESSAGE MAY NAME THE ROLE KIND, because the encoder is not reachable here: the
		// loop above returned on a prefiller or a decoder, and admission refuses an Encode role
		// with no router. The kind is new enough that no stored object predates that rule.
		ModelDeploymentConditionKVEventsPublishing.True(holder,
			modelDeploymentReasonKVEventsNotApplicable,
			"the unrouted deployment has only server roles, so nothing consumes cache events")
		return
	}

	configured := 0
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		// A CONSUMER PUBLISHES NOTHING, AND NEITHER DOES AN ENCODER. An encoder was handed media
		// and produces embeddings for the prefiller, so it is not a producer whose configuration
		// this operator failed to find -- counting it reported a healthy encoder deployment as
		// unconfigured, and named a role that publishes no events at all.
		if kind := ModelDeploymentEffectiveRoleKind(role); kind == workercore.ModelDeploymentRoleKindDecode ||
			kind == workercore.ModelDeploymentRoleKindEncode {
			continue
		}
		if len(role.Command) > 0 {
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
