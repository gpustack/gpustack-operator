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
