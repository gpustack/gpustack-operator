package worker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestObserveModelDeploymentKVEventsPublishing(t *testing.T) {
	testCases := []struct {
		name       string
		deployment *workercore.ModelDeployment
		status     meta.ConditionStatus
		reason     string
	}{
		{
			name: "single_server_not_applicable", deployment: newRenderDeployment(),
			status: meta.ConditionTrue, reason: modelDeploymentReasonKVEventsNotApplicable,
		},
		{
			name: "routed_servers_do_publish",
			deployment: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			}),
			status: meta.ConditionTrue, reason: modelDeploymentReasonKVEventsPublishing,
		},
		{
			name: "pair_without_router", deployment: twoRoleDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
			}),
			status: meta.ConditionFalse, reason: modelDeploymentReasonKVEventsNoRouter,
		},
		{
			name: "publisher_disabled",
			deployment: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
			}),
			status: meta.ConditionFalse, reason: modelDeploymentReasonKVEventsPublisherDisabled,
		},
		{
			name: "producer_unmanaged",
			deployment: routedModelDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"/bin/custom-server"}
			}),
			status: meta.ConditionUnknown, reason: modelDeploymentReasonKVEventsRoleUnmanaged,
		},
		{
			name: "all_configured_without_shared_store", deployment: routedModelDeployment(),
			status: meta.ConditionTrue, reason: modelDeploymentReasonKVEventsPublishing,
		},
		{
			// AN ENCODER PRODUCES NO CACHE BLOCKS. It reads media and hands embeddings to a
			// prefiller, so counting it as a producer says a deployment publishes events when its
			// encoder is the first role a reader would look at, and the message would name a role
			// that publishes none.
			name: "an_encoder_is_not_a_producer", deployment: epdDeployment(t,
				[]workercore.ModelDeploymentRoleKind{
					workercore.ModelDeploymentRoleKindEncode,
					workercore.ModelDeploymentRoleKindPrefill,
					workercore.ModelDeploymentRoleKindDecode,
				}, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				}),
			status: meta.ConditionTrue, reason: modelDeploymentReasonKVEventsPublishing,
		},
		{
			// The pair is what publishes here, not the encoder: the count in the message is one,
			// which is the prefiller alone.
			name: "an_encoder_deployment_counts_only_its_pair",
			deployment: epdDeployment(t,
				[]workercore.ModelDeploymentRoleKind{
					workercore.ModelDeploymentRoleKindPrefill,
					workercore.ModelDeploymentRoleKindDecode,
					workercore.ModelDeploymentRoleKindEncode,
				}, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				}),
			status: meta.ConditionTrue, reason: modelDeploymentReasonKVEventsPublishing,
		},
		{
			// A ROLE THAT REPLACED ITS OWN COMMAND LINE cannot be judged by this operator, and that
			// judgement is not owed to an encoder: it publishes no cache blocks to be silent about.
			name: "an_unmanaged_encoder_is_not_held_unmanaged", deployment: epdDeployment(t,
				[]workercore.ModelDeploymentRoleKind{
					workercore.ModelDeploymentRoleKindEncode,
					workercore.ModelDeploymentRoleKindPrefill,
					workercore.ModelDeploymentRoleKindDecode,
				}, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.Roles[0].Command = []string{"/bin/custom-encoder"}
				}),
			status: meta.ConditionTrue, reason: modelDeploymentReasonKVEventsPublishing,
		},
		{
			// THE UNROUTED BRANCH READS THE SAME DECLARED ROLES. With no router there is nothing
			// consuming cache events, but the encoder is not one of the roles that produce them, so
			// it cannot be the reason the deployment is reported as needing one.
			//
			// The shape is one an admission rule would refuse -- an encoder with no pair -- and it is
			// stated anyway because this rule runs on every pass and a spec that reached the
			// controller before that rule, or beside it, gets a verdict from here.
			name: "an_unrouted_encoder_is_not_a_reason_to_need_a_router",
			deployment: newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = []workercore.ModelDeploymentRole{{
					Name:         "encode",
					Kind:         workercore.ModelDeploymentRoleKindEncode,
					InstanceType: "h20-8x",
					Image:        "vllm/vllm-openai:v0.30.0",
				}}
			}),
			status: meta.ConditionTrue, reason: modelDeploymentReasonKVEventsNotApplicable,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			holder := new(workercore.ModelDeployment)
			observeModelDeploymentKVEvents(tc.deployment, holder, nil)

			assert.Equal(t, string(tc.status), ModelDeploymentConditionKVEventsPublishing.GetStatus(holder))
			assert.Equal(t, tc.reason, ModelDeploymentConditionKVEventsPublishing.GetReason(holder))
		})
	}
}

// TestObserveModelDeploymentKVEventsPublishing_CountsOnlyTheRolesThatProduce is the count the
// Publishing message reports.
//
// THE COUNT IS THE CONDITION'S EVIDENCE. "all 2 producing roles" and "all 3 producing roles" disagree
// about whether an encoder belongs in the set, so a message that counted one would leave the reason
// and the reason's own justification disagreeing while both read plausibly.
func TestObserveModelDeploymentKVEventsPublishing_CountsOnlyTheRolesThatProduce(t *testing.T) {
	testCases := []struct {
		name  string
		order []workercore.ModelDeploymentRoleKind
		want  string
	}{
		{
			// The encoder listed first is the case a producer rule reading Roles[0] gets wrong.
			name: "the_encoder_first_leaves_one_producer",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			want: "all 1 producing roles are configured to publish cache events",
		},
		{
			name: "the_encoder_last_leaves_one_producer",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
				workercore.ModelDeploymentRoleKindEncode,
			},
			want: "all 1 producing roles are configured to publish cache events",
		},
		{
			// THE POSITIVE BASELINE for the count itself: without it, a message that said zero for
			// everything would pass the two cases above.
			name: "a_pair_alone_leaves_one_producer",
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			want: "all 1 producing roles are configured to publish cache events",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := epdDeployment(t, tc.order)
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			holder := new(workercore.ModelDeployment)

			observeModelDeploymentKVEvents(md, holder, nil)

			assert.Equal(t, modelDeploymentReasonKVEventsPublishing,
				ModelDeploymentConditionKVEventsPublishing.GetReason(holder))
			assert.Equal(t, tc.want, ModelDeploymentConditionKVEventsPublishing.GetMessage(holder))
		})
	}
}

func TestObserveModelDeploymentKVEventsPublishing_PreservesTransitionTime(t *testing.T) {
	md := routedModelDeployment()
	holder := new(workercore.ModelDeployment)
	observeModelDeploymentKVEvents(md, holder, nil)
	require.Len(t, holder.Status.Conditions, 1)

	want := meta.NewTime(time.Unix(1, 0))
	holder.Status.Conditions[0].LastTransitionTime = want
	observeModelDeploymentKVEvents(md, holder, nil)

	assert.Equal(t, want, holder.Status.Conditions[0].LastTransitionTime)
}
