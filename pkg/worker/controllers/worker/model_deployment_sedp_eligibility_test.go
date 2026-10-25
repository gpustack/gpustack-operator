package worker

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// endpointEligibilityMemberReady sets the readiness of the member at the given index and leaves
// every other member's readiness alone.
//
// THE INDEX, NOT THE NAME, because a member's name carries its ordinal and a case that named
// the member by name would be asserting the naming scheme rather than the withdrawal.
func endpointEligibilityMemberReady(t *testing.T, cli ctrlcli.Client, index string, ready bool) {
	t.Helper()

	for _, pod := range endpointEligibilityPods(t, cli) {
		if pod.Labels[modelDeploymentMemberIndexLabel] != index {
			continue
		}
		if ready {
			pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
		} else {
			pod.Status.Conditions = nil
		}
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}
}

// TestModelDeploymentReconciler_ANonHeadMemberFaultWithdrawsTheWholeGroup pins the group-atomic
// withdrawal: a member that stops qualifying takes its whole group with it, because a group
// whose members do not all qualify serves nothing.
//
// THE FAULT IS ON THE NON-HEAD MEMBER ON PURPOSE. A leader-served group answers through its
// leader, so a follower that faults changes nothing about what the group serves. The shape
// that puts every member on the front is the one whose withdrawal must be atomic, and it is
// the shape a member fault would otherwise be allowed to half-withdraw.
func TestModelDeploymentReconciler_ANonHeadMemberFaultWithdrawsTheWholeGroup(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].ExtraArgs = []string{
			"--data-parallel-size", "2", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)",
		}
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	setEndpointEligibilityPodsReady(t, cli, true)
	// THE PROBE ASKS A MEMBER AT ITS ADDRESS, so a member with no address is a member the
	// group-forward leg cannot ask, and the group stays held for want of an answer.
	for i, pod := range endpointEligibilityPods(t, cli) {
		pod.Status.PodIP = "10.0.0." + strconv.Itoa(1+i)
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}

	_, err := reconcileModelDeploymentActivated(t, cli)
	require.NoError(t, err)

	pods := endpointEligibilityPods(t, cli)
	require.Len(t, pods, 2, "one replica of two members")
	for _, pod := range pods {
		require.Equal(t, modelDeploymentEndpointEligibleValue,
			pod.Labels[modelDeploymentLabelKeyEndpointEligible], "%s qualified", pod.Name)
	}

	// The non-head member faults. The head is untouched and still healthy.
	endpointEligibilityMemberReady(t, cli, "1", false)

	_, err = reconcileModelDeploymentActivated(t, cli)
	require.NoError(t, err)

	for _, pod := range endpointEligibilityPods(t, cli) {
		assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
			"%s lost the key with its group", pod.Name)
	}
}
