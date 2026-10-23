package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// THE CASES CALL THREE THINGS THE PROJECTION OWNS, AND THE ORDER IS THE ORDER THE STATUS IS
// BUILT IN: the builder attaches the group entries, the answer resolves the Router's hold, and
// the times settle the moment each group last became eligible.
//
//	applyModelDeploymentGroupStatuses           attaches the entries
//	applyModelDeploymentGroupAnswer             resolves the Router's hold
//	applyModelDeploymentGroupQualificationTimes settles the qualification time
//
// THE ANSWER IS THE PASS'S CARRIER, modelDeploymentPassObservation, which is what the status and
// the restoration read, so the two answer about the same moment. The hold itself is decided by
// modelDeploymentGroupProtectionOf, which the status asks over the views it derives from its own
// Pod list.

// groupStatusCase is one replica's shape, declared as data.
type groupStatusCase struct {
	role    string
	ordinal int
	// size is the number of members in the replica. TWO MEMBERS MAKE A GROUP, and a group of two
	// is the protected shape the Router's observation holds.
	size int
	// readiness is each member's readiness. AN EMPTY LIST MEANS EVERY MEMBER IS READY, and a nil
	// entry is a member nobody has reported on.
	readiness []*core.ConditionStatus
	forward   modelDeploymentGroupForward
	legs      []modelDeploymentQualificationLeg
	// shape is the shape of the pair attempt that verified this replica, when one did.
	shape modelDeploymentGroupForwardPairShape
}

// readinessAt is the readiness of the member in the given seat.
func (c groupStatusCase) readinessAt(seat int) *core.ConditionStatus {
	if len(c.readiness) == 0 {
		return ptr.To(core.ConditionTrue)
	}
	if seat >= len(c.readiness) {
		return nil
	}

	return c.readiness[seat]
}

// groupStatusMember builds one member Pod of a replica: the given seat of the given ordinal.
//
// THE SEAT AND THE ORDINAL ARE LABELS, because that is where the cluster puts them. A member that
// claims no seat reads as the leader, and a member that claims no ordinal belongs to no replica,
// so a fixture that left them off would describe a different group than the case means.
//
// A NIL READINESS IS A MEMBER NOBODY HAS REPORTED ON, which is a different fact from a member that
// was observed and found not ready.
func groupStatusMember(
	md *workercore.ModelDeployment, role workercore.ModelDeploymentRole, ordinal, seat int,
	ready *core.ConditionStatus,
) *core.Pod {
	pod := &core.Pod{}
	pod.Name = fmt.Sprintf("%s-%s-%d-%d", md.Name, role.Name, ordinal, seat)
	pod.Namespace = md.Namespace
	pod.UID = types.UID(pod.Name + "-uid")
	pod.Labels = modelDeploymentPodLabels(md, &role, "fixture-entrance")
	pod.Labels[modelDeploymentReplicaOrdinalLabel] = strconvx.Itoa(ordinal)
	pod.Labels[modelDeploymentMemberIndexLabel] = strconvx.Itoa(seat)
	kubemeta.ControlOnWithoutBlock(pod, md, workercore.SchemeGroupVersionKind("ModelDeployment"))
	pod.Spec.Containers = []core.Container{{
		Name: modelDeploymentMainContainerName, Image: "vllm/vllm-openai:v0.29.0",
	}}
	pod.Annotations = map[string]string{
		kueuepodconst.GroupTotalCountAnnotation: strconvx.Itoa(modelDeploymentRoleSize(&role)),
	}
	if ready != nil {
		pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: *ready}}
	}

	return pod
}

// groupStatusQualification builds one replica's qualification from a declarative case.
//
// EVERY LEG VERIFIED IS THE DEFAULT, because most cases are about something else and a case that
// spelled all five out would bury the one leg it means to fail.
func groupStatusQualification(c groupStatusCase, members []*core.Pod) modelDeploymentInstanceQualification {
	legs := c.legs
	if len(legs) == 0 {
		legs = []modelDeploymentQualificationLeg{
			{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
			{Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegVerified},
			{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
			{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
			{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
		}
	}
	// THE GROUP-FORWARD LEG FOLLOWS THE OBSERVATION, exactly as the pass derives it, so a case that
	// names an observation cannot describe a qualification the pass would never produce.
	if c.forward.State != "" {
		for i := range legs {
			if legs[i].Name != modelDeploymentLegGroupForward {
				continue
			}
			legs[i].Verdict = groupForwardVerdict(c.forward.State)
			legs[i].Reason = c.forward.Reason
		}
	}
	view := modelDeploymentReplicaView{
		Role: c.role, Ordinal: c.ordinal, Seated: true, Members: members,
	}
	generation := modelDeploymentGenerationOf(view)

	// The attempt carries the shape that answered, and a case that names no shape produced none.
	var attempt *modelDeploymentGroupForwardPairAttempt
	if c.shape != "" {
		attempt = &modelDeploymentGroupForwardPairAttempt{
			PrefillGeneration: generation,
			DecodeGeneration:  generation,
			Shape:             c.shape,
		}
	}

	return modelDeploymentInstanceQualification{
		View:         view,
		Generation:   generation,
		GroupForward: c.forward,
		PairAttempt:  attempt,
		Observed:     true,
		Legs:         legs,
	}
}

// groupStatusMembers builds the members of one replica, and the Pod list the status pass reads.
func groupStatusMembers(
	md *workercore.ModelDeployment, c groupStatusCase,
) ([]*core.Pod, []core.Pod) {
	members := make([]*core.Pod, 0, c.size)
	pods := make([]core.Pod, 0, c.size)
	for seat := range c.size {
		member := groupStatusMember(md, md.Spec.Roles[0], c.ordinal, seat, c.readinessAt(seat))
		members = append(members, member)
		pods = append(pods, *member)
	}

	return members, pods
}

// groupStatusPublish builds the published status for the given replicas.
//
// THE FACT IS FILED UNDER EACH REPLICA'S OWN KEY, so a case never addresses the answer itself.
//
// THE ANSWER IS THE PASS'S CARRIER, which is what the status and the restoration read, so the two
// answer about the same moment.
func groupStatusPublish(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod,
	qualifications []modelDeploymentInstanceQualification, fact *modelDeploymentGroupServing,
) *workercore.ModelDeploymentStatus {
	t.Helper()

	answer := ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}}
	for _, qualification := range qualifications {
		if fact == nil {
			continue
		}
		key := groupStatusKey(qualification.View.Role, qualification.View.Ordinal, qualification.Generation)
		answer.Groups[key] = *fact
	}

	return groupStatusPublishWith(t, md, pods, qualifications, answer)
}

// groupStatusPublishWith builds the published status for an answer the case addressed itself.
//
// THE COLLECTION INSTANT IS PART OF THE ANSWER, because a view that has grown stale is not
// published as a confirmation, and a case that leaves it out would be testing the staleness rule
// rather than the answer it means to state.
//
// THE GROUP'S VIEW GOES WITH IT IN THE QUALIFICATION, because the hold is decided by the SAME
// classification the restore barrier uses, and that classification needs the replica's view. The
// qualification carries it, so the status does not group the Pods a second time.
func groupStatusPublishWith(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod,
	qualifications []modelDeploymentInstanceQualification, answer ServingAnswer,
) *workercore.ModelDeploymentStatus {
	t.Helper()

	return groupStatusPublishMembership(t, md, pods, qualifications, answer, nil)
}

// groupStatusPublishMembership publishes the status with a collective's membership on the carrier.
//
// THE MEMBERSHIP IS EMPTY FOR EVERY SHAPE THAT HAS NO COLLECTIVE, and the fixed shape's groups are
// the replica-derived ones.
//
// THE GROUPS ARE ATTACHED AFTER THE COUNT, the way the status itself attaches them, so the
// collective's membership reaches the publication.
func groupStatusPublishMembership(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod,
	qualifications []modelDeploymentInstanceQualification, answer ServingAnswer,
	collectiveMembers []core.Pod,
) *workercore.ModelDeploymentStatus {
	t.Helper()

	answer.CollectedAt = time.Now()

	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}
	status, err := r.computeModelDeploymentStatusWithQualifications(
		context.Background(), md, pods, nil, nil, nil, qualifications)
	require.NoError(t, err)

	collective := applyModelDeploymentGroupStatuses(
		md, status.Roles, qualifications, pods, collectiveMembers)
	r.applyModelDeploymentGroupAnswer(status, modelDeploymentPassObservation{
		Serving:        answer,
		Qualifications: qualifications,
	}, md, collective)

	return status
}

// groupStatusRun builds the published status for the given replicas.
//
// THE WHOLE SETUP LIVES HERE, so a case says what it means and nothing else: the shape of the
// replica, the legs that do not verify, and what the Router said.
func groupStatusRun(
	t *testing.T, cases []groupStatusCase, fact *modelDeploymentGroupServing, routed bool,
) *workercore.ModelDeploymentStatus {
	t.Helper()

	md := newRenderDeployment()
	md.Spec.Roles[0].ReplicaSize = int32(cases[0].size)
	if routed {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	}

	pods := make([]core.Pod, 0, len(cases)*cases[0].size)
	qualifications := make([]modelDeploymentInstanceQualification, 0, len(cases))
	for _, c := range cases {
		members, memberPods := groupStatusMembers(md, c)
		pods = append(pods, memberPods...)
		qualifications = append(qualifications, groupStatusQualification(c, members))
	}

	return groupStatusPublish(t, md, pods, qualifications, fact)
}

// groupStatusKey identifies a role, ordinal and generation.
func groupStatusKey(
	role string, ordinal int, generation modelDeploymentMembershipGeneration,
) modelDeploymentGroupKey {
	return modelDeploymentGroupKey{Role: role, Ordinal: ordinal, Generation: generation}
}

// groupStatusGroup is the published entry of the one group the case published.
func groupStatusGroup(t *testing.T, status *workercore.ModelDeploymentStatus) workercore.ModelDeploymentGroupStatus {
	t.Helper()

	require.Len(t, status.Roles, 1)
	require.Len(t, status.Roles[0].Groups, 1)

	return status.Roles[0].Groups[0]
}

// TestModelDeploymentGroupStatusReportsEveryGroup pins what one group entry carries: which role
// and ordinal it is, the membership it was taken against, the state and the reason, and the
// members with their seats and their readiness.
func TestModelDeploymentGroupStatusReportsEveryGroup(t *testing.T) {
	cases := []groupStatusCase{
		{role: "server", ordinal: 0, size: 2},
		{role: "server", ordinal: 1, size: 2},
	}
	status := groupStatusRun(t, cases, nil, false)

	require.Len(t, status.Roles[0].Groups, 2, "one entry per group, not per member")
	group := status.Roles[0].Groups[0]
	assert.Equal(t, "server", group.Role)
	assert.Equal(t, int32(0), group.Ordinal)
	assert.Equal(t, workercore.ModelDeploymentGroupStateEligible, group.State)
	assert.Equal(t, "every leg of the group verified", group.Reason)
	assert.Equal(t, int32(1), status.Roles[0].Groups[1].Ordinal,
		"the groups are ordered by ordinal, so a reader can find one without searching")

	require.Len(t, group.Members, 2)
	assert.Equal(t, "qwen-server-0-0", group.Members[0].Name)
	assert.Equal(t, "qwen-server-0-0-uid", group.Members[0].UID)
	require.NotNil(t, group.Members[0].Seat)
	assert.Equal(t, int32(0), *group.Members[0].Seat)
	assert.Equal(t, meta.ConditionTrue, group.Members[0].Ready)
	require.NotNil(t, group.Members[1].Seat)
	assert.Equal(t, int32(1), *group.Members[1].Seat)
}

// TestModelDeploymentGroupStatusDistinguishesTheFailureModes pins that each leg of the whole-group
// predicate has its own published state, and that the reason names the SAME leg the state names.
func TestModelDeploymentGroupStatusDistinguishesTheFailureModes(t *testing.T) {
	tests := []struct {
		name       string
		legs       []modelDeploymentQualificationLeg
		forward    modelDeploymentGroupForward
		wantState  workercore.ModelDeploymentGroupState
		wantReason string
	}{
		{
			name:       "every_leg_verified",
			wantState:  workercore.ModelDeploymentGroupStateEligible,
			wantReason: "every leg of the group verified",
		},
		{
			name: "the_member_set_is_not_established",
			legs: []modelDeploymentQualificationLeg{
				{
					Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegUnknown,
					Reason: "the member set is not established",
				},
				{Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
			wantState:  workercore.ModelDeploymentGroupStateAwaitingMembers,
			wantReason: "the member set is not established",
		},
		{
			name: "a_member_is_not_ready",
			legs: []modelDeploymentQualificationLeg{
				{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
				{
					Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegUnknown,
					Reason: "member qwen-server-0-1 is not Ready",
				},
				{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
			wantState:  workercore.ModelDeploymentGroupStateAwaitingReady,
			wantReason: "member qwen-server-0-1 is not Ready",
		},
		{
			name: "the_forward_path_could_not_be_observed",
			legs: []modelDeploymentQualificationLeg{
				{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegVerified},
				{
					Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegUnknown,
					Reason: "the forward path could not be observed",
				},
				{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
			wantState:  workercore.ModelDeploymentGroupStateAwaitingProbe,
			wantReason: "the forward path could not be observed",
		},
		{
			name: "a_definite_fault_withdraws_the_group",
			legs: []modelDeploymentQualificationLeg{
				{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
				{
					Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegFailed,
					Reason: "member qwen-server-0-1 failed its health check",
				},
				{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
			wantState:  workercore.ModelDeploymentGroupStateWithdrawn,
			wantReason: "member qwen-server-0-1 failed its health check",
		},
		{
			name: "a_shape_no_observation_exists_for",
			forward: modelDeploymentGroupForward{
				State:  modelDeploymentGroupForwardUnsupported,
				Reason: "the engine has no group-forward observation",
			},
			wantState:  workercore.ModelDeploymentGroupStateUnsupported,
			wantReason: "the engine has no group-forward observation",
		},
		{
			name: "a_leg_this_operator_cannot_name",
			legs: []modelDeploymentQualificationLeg{
				{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
				{
					Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegUnknown,
					Reason: "the retirement record could not be read",
				},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
			wantState:  workercore.ModelDeploymentGroupStateUnknown,
			wantReason: "the retirement record could not be read",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := groupStatusRun(t, []groupStatusCase{{
				role: "server", ordinal: 0, size: 2,
				legs: tc.legs, forward: tc.forward,
			}}, nil, false)

			group := groupStatusGroup(t, status)
			assert.Equal(t, tc.wantState, group.State)
			assert.Equal(t, tc.wantReason, group.Reason,
				"the reason names the same leg the state names")
		})
	}
}

// TestModelDeploymentGroupStatusIsTheSameForTheSamePodsInAnyOrder pins that the published group
// entry does not depend on the order the cluster listed the Pods in: the same membership is the
// same group, and a listing that returns them in another order describes the same group.
func TestModelDeploymentGroupStatusIsTheSameForTheSamePodsInAnyOrder(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.Roles[0].ReplicaSize = 2
	case_ := groupStatusCase{role: "server", ordinal: 0, size: 2}
	members, pods := groupStatusMembers(md, case_)
	qualification := groupStatusQualification(case_, members)

	forwards := groupStatusPublishWith(t, md, pods,
		[]modelDeploymentInstanceQualification{qualification}, ServingAnswer{})
	backwards := groupStatusPublishWith(t, md, []core.Pod{pods[1], pods[0]},
		[]modelDeploymentInstanceQualification{qualification}, ServingAnswer{})

	assert.Equal(t, forwards.Roles[0].Groups, backwards.Roles[0].Groups)
}

// TestModelDeploymentGroupStatusKeepsTheQualificationTimeAcrossPasses pins that the published
// time is the moment the group became eligible, and not the moment of the latest pass: a group
// that stays eligible keeps the instant it earned.
func TestModelDeploymentGroupStatusKeepsTheQualificationTimeAcrossPasses(t *testing.T) {
	cases := []groupStatusCase{{role: "server", ordinal: 0, size: 2}}
	first := groupStatusRun(t, cases, nil, false)
	earned := meta.Now().Rfc3339Copy()
	applyModelDeploymentGroupQualificationTimes(first, first.Roles, earned, nil)
	require.NotNil(t, groupStatusGroup(t, first).LastQualificationTime)

	second := groupStatusRun(t, cases, nil, false)
	applyModelDeploymentGroupQualificationTimes(second, first.Roles, meta.NewTime(earned.Add(time.Hour)), nil)

	assert.Equal(t, earned.UTC(), groupStatusGroup(t, second).LastQualificationTime.UTC(),
		"a group that stays eligible keeps the instant it earned")
}

// TestModelDeploymentGroupStatusMovesTheQualificationTimeOnAMembershipChange pins that a replaced
// member is a new membership, and that the new membership earns its own instant: the earlier
// verification was taken against Pods that are no longer the group.
func TestModelDeploymentGroupStatusMovesTheQualificationTimeOnAMembershipChange(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.Roles[0].ReplicaSize = 2
	case_ := groupStatusCase{role: "server", ordinal: 0, size: 2}
	members, pods := groupStatusMembers(md, case_)

	first := groupStatusPublish(t, md, pods,
		[]modelDeploymentInstanceQualification{groupStatusQualification(case_, members)}, nil)
	earned := meta.Now().Rfc3339Copy()
	applyModelDeploymentGroupQualificationTimes(first, first.Roles, earned, nil)

	// THE SAME REPLICA WITH ONE MEMBER REPLACED, which is a different membership and therefore a
	// different generation. THE REPLACEMENT CARRIES ITS OWN IDENTITY, because a member that kept
	// the UID of the one it replaced would be the same membership wearing another name.
	replaced := groupStatusMember(md, md.Spec.Roles[0], 0, 1, ptr.To(core.ConditionTrue))
	replaced.Name = "qwen-server-0-1-replacement"
	replaced.UID = types.UID(replaced.Name + "-uid")
	newMembers := []*core.Pod{members[0], replaced}
	newPods := []core.Pod{*newMembers[0], *newMembers[1]}
	second := groupStatusPublish(t, md, newPods,
		[]modelDeploymentInstanceQualification{groupStatusQualification(case_, newMembers)}, nil)
	later := meta.NewTime(earned.Add(time.Hour))
	applyModelDeploymentGroupQualificationTimes(second, first.Roles, later, nil)

	assert.Equal(t, later.UTC(), groupStatusGroup(t, second).LastQualificationTime.UTC(),
		"a new membership earns its own instant")
	assert.NotEqual(t, string(modelDeploymentGenerationOf(modelDeploymentReplicaView{
		Role: "server", Ordinal: 0, Seated: true, Members: members,
	})), groupStatusGroup(t, second).Generation)
}

// TestModelDeploymentGroupStatusPublishesNoInheritedTimeOnANewGeneration pins that a new membership
// inherits no qualification time: the membership it names is a membership that was never qualified,
// and an inherited instant would claim otherwise.
//
// THE GROUP HAS NOT QUALIFIED IN ITS NEW MEMBERSHIP, so it has nothing to inherit and nothing to
// record: the earlier verification was taken against Pods that are no longer the group.
func TestModelDeploymentGroupStatusPublishesNoInheritedTimeOnANewGeneration(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.Roles[0].ReplicaSize = 2
	case_ := groupStatusCase{role: "server", ordinal: 0, size: 2}
	members, pods := groupStatusMembers(md, case_)

	first := groupStatusPublish(t, md, pods,
		[]modelDeploymentInstanceQualification{groupStatusQualification(case_, members)}, nil)
	earned := meta.Now().Rfc3339Copy()
	applyModelDeploymentGroupQualificationTimes(first, first.Roles, earned, nil)
	require.NotNil(t, groupStatusGroup(t, first).LastQualificationTime,
		"the membership that qualified earned an instant")

	// THE SAME REPLICA WITH ONE MEMBER REPLACED, and a leg that does not verify: a different
	// membership, and a group that has not qualified in it.
	replaced := groupStatusMember(md, md.Spec.Roles[0], 0, 1, ptr.To(core.ConditionTrue))
	replaced.Name = "qwen-server-0-1-replacement"
	replaced.UID = types.UID(replaced.Name + "-uid")
	newMembers := []*core.Pod{members[0], replaced}
	held := groupStatusCase{
		role: "server", ordinal: 0, size: 2,
		legs: []modelDeploymentQualificationLeg{
			{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
			{
				Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegUnknown,
				Reason: "member qwen-server-0-1 is not Ready",
			},
			{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
			{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
			{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
		},
	}
	second := groupStatusPublish(t, md, []core.Pod{*newMembers[0], *newMembers[1]},
		[]modelDeploymentInstanceQualification{groupStatusQualification(held, newMembers)}, nil)
	applyModelDeploymentGroupQualificationTimes(second, first.Roles, meta.NewTime(earned.Add(time.Hour)), nil)

	assert.Nil(t, groupStatusGroup(t, second).LastQualificationTime,
		"a new membership inherits no qualification time")
}

// TestModelDeploymentGroupMemberReadinessIsThreeStates pins that a member nobody has reported on
// is Unknown rather than False: a member that was observed and found not ready is a different
// fact from a member whose readiness was never read.
func TestModelDeploymentGroupMemberReadinessIsThreeStates(t *testing.T) {
	tests := []struct {
		name      string
		readiness []*core.ConditionStatus
		want      meta.ConditionStatus
	}{
		{name: "ready", readiness: []*core.ConditionStatus{ptr.To(core.ConditionTrue)}, want: meta.ConditionTrue},
		{name: "not_ready", readiness: []*core.ConditionStatus{ptr.To(core.ConditionFalse)}, want: meta.ConditionFalse},
		{name: "never_reported", readiness: []*core.ConditionStatus{nil}, want: meta.ConditionUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := groupStatusRun(t, []groupStatusCase{{
				role: "server", ordinal: 0, size: 1, readiness: tc.readiness,
			}}, nil, false)

			group := groupStatusGroup(t, status)
			require.Len(t, group.Members, 1)
			assert.Equal(t, tc.want, group.Members[0].Ready)
		})
	}
}

// TestModelDeploymentGroupServingReportsEveryState pins the four serving answers the Router can
// give, and the one the deployment itself decides: a deployment with no Router has nothing to
// confirm. ONLY A CONFIRMATION CARRIES A COUNT, because a count that is not a confirmation is a
// number the operator never observed.
func TestModelDeploymentGroupServingReportsEveryState(t *testing.T) {
	tests := []struct {
		name    string
		routed  bool
		fact    *modelDeploymentGroupServing
		want    workercore.ModelDeploymentServingState
		wantNil bool
	}{
		{
			name:    "a_deployment_with_no_router_has_nothing_to_confirm",
			want:    workercore.ModelDeploymentServingStateNotConfigured,
			wantNil: true,
		},
		{
			name:   "an_unobserved_router_says_unknown",
			routed: true,
			fact: &modelDeploymentGroupServing{
				SnapshotValid: false,
				InvalidReason: "the Router's view could not be read",
				State:         workercore.ModelDeploymentServingStateUnknown,
			},
			want:    workercore.ModelDeploymentServingStateUnknown,
			wantNil: true,
		},
		{
			name:   "a_router_that_has_not_converged_says_so",
			routed: true,
			fact: &modelDeploymentGroupServing{
				SnapshotValid: true,
				State:         workercore.ModelDeploymentServingStateNotConverged,
			},
			want:    workercore.ModelDeploymentServingStateNotConverged,
			wantNil: true,
		},
		{
			name:   "a_confirmed_router_carries_a_count",
			routed: true,
			fact: &modelDeploymentGroupServing{
				SnapshotValid: true,
				State:         workercore.ModelDeploymentServingStateConfirmed,
				Endpoints:     1,
			},
			want: workercore.ModelDeploymentServingStateConfirmed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := groupStatusRun(t, []groupStatusCase{{role: "server", ordinal: 0, size: 2}},
				tc.fact, tc.routed)

			serving := groupStatusGroup(t, status).Serving
			assert.Equal(t, tc.want, serving.State)
			if tc.wantNil {
				assert.Nil(t, serving.Value, "only a confirmation carries a count")
			} else {
				require.NotNil(t, serving.Value)
				assert.Equal(t, int32(1), *serving.Value)
			}
		})
	}
}

// TestModelDeploymentGroupServingTellsNoFactFromAFactThatSaysUnknown pins that a group no Router
// observation covers and a group whose observation says Unknown are different readings: the first
// has no reason because nobody looked, and the second carries the reason the Router gave.
func TestModelDeploymentGroupServingTellsNoFactFromAFactThatSaysUnknown(t *testing.T) {
	cases := []groupStatusCase{{role: "server", ordinal: 0, size: 2}}
	withoutFact := groupStatusRun(t, cases, nil, true)
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown,
		groupStatusGroup(t, withoutFact).Serving.State,
		"a missing observation never publishes the empty state, so the group says what it could not read")
	assert.NotEmpty(t, groupStatusGroup(t, withoutFact).ServingReason,
		"whatever explains the serving answer is published with it")

	withFact := groupStatusRun(t, cases, &modelDeploymentGroupServing{
		SnapshotValid: false,
		InvalidReason: "the Router's view could not be read",
		State:         workercore.ModelDeploymentServingStateUnknown,
	}, true)
	assert.Equal(t, "the Router's view could not be read",
		groupStatusGroup(t, withFact).ServingReason)
}

// TestModelDeploymentGroupServingDoesNotAttachAStaleGenerationAnswer pins that an answer taken
// against an earlier membership is not this group's answer: the group's members changed, so the
// earlier observation describes Pods that are no longer the group.
func TestModelDeploymentGroupServingDoesNotAttachAStaleGenerationAnswer(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.Roles[0].ReplicaSize = 2
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	case_ := groupStatusCase{role: "server", ordinal: 0, size: 2}
	members, pods := groupStatusMembers(md, case_)

	// THE DEPLOYMENT'S OWN ANSWER IS A CONFIRMATION, and this group is not in it: the fact is
	// filed under an earlier membership, so it describes Pods that are no longer the group.
	answer := ServingAnswer{
		State: workercore.ModelDeploymentServingStateConfirmed,
		Value: ptr.To(int32(1)),
		Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{
			groupStatusKey("server", 0, modelDeploymentMembershipGeneration("uid-from-an-earlier-membership")): {
				SnapshotValid: true,
				State:         workercore.ModelDeploymentServingStateConfirmed,
				Endpoints:     1,
			},
		},
	}

	status := groupStatusPublishWith(t, md, pods,
		[]modelDeploymentInstanceQualification{groupStatusQualification(case_, members)}, answer)

	assert.NotEqual(t, workercore.ModelDeploymentServingStateConfirmed,
		groupStatusGroup(t, status).Serving.State,
		"an answer about an earlier membership is not this group's answer")
}

// TestModelDeploymentGroupBarrier pins the hold and the two cases beside it: a group whose legs
// pass waits for the observation, a group that retains keeps the state it had, a deployment with
// no Router has no observation to be missing, and an ordinary single-member Server keeps the
// behavior it always had.
//
// THE ROUTER'S REASON IS PUBLISHED IN BOTH PLACES, because the serving field has to be complete on
// its own and the group reason may repeat it.
func TestModelDeploymentGroupBarrier(t *testing.T) {
	invalid := &modelDeploymentGroupServing{
		SnapshotValid: false,
		InvalidReason: "the Router's view could not be read",
		State:         workercore.ModelDeploymentServingStateUnknown,
	}
	retained := &modelDeploymentGroupServing{
		SnapshotValid: false,
		InvalidReason: "the Router's view could not be read",
		State:         workercore.ModelDeploymentServingStateUnknown,
		Retained:      true,
	}

	tests := []struct {
		name        string
		size        int
		routed      bool
		fact        *modelDeploymentGroupServing
		wantState   workercore.ModelDeploymentGroupState
		wantServing workercore.ModelDeploymentServingState
		wantReason  string
	}{
		{
			name:        "a_qualified_group_without_a_valid_observation_waits",
			size:        2,
			routed:      true,
			fact:        invalid,
			wantState:   workercore.ModelDeploymentGroupStateAwaitingObservation,
			wantServing: workercore.ModelDeploymentServingStateUnknown,
			wantReason:  "the Router's view could not be read",
		},
		{
			name:        "a_group_that_retains_stays_eligible",
			size:        2,
			routed:      true,
			fact:        retained,
			wantState:   workercore.ModelDeploymentGroupStateEligible,
			wantServing: workercore.ModelDeploymentServingStateUnknown,
			wantReason:  "every leg of the group verified",
		},
		{
			// A DEPLOYMENT WITH NO ROUTER MAKES NO OBSERVATION AT ALL, so the answer carries no
			// per-group fact: there is nothing that could be missing.
			name:        "without_a_router_there_is_no_observation_to_be_missing",
			size:        2,
			wantState:   workercore.ModelDeploymentGroupStateEligible,
			wantServing: workercore.ModelDeploymentServingStateNotConfigured,
			wantReason:  "every leg of the group verified",
		},
		{
			name:        "an_ordinary_single_member_server_keeps_its_behaviour",
			size:        1,
			routed:      true,
			fact:        invalid,
			wantState:   workercore.ModelDeploymentGroupStateEligible,
			wantServing: workercore.ModelDeploymentServingStateUnknown,
			wantReason:  "every leg of the group verified",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := groupStatusRun(t, []groupStatusCase{{
				role: "server", ordinal: 0, size: tc.size,
			}}, tc.fact, tc.routed)

			group := groupStatusGroup(t, status)
			assert.Equal(t, tc.wantState, group.State)
			assert.Equal(t, tc.wantServing, group.Serving.State)
			if tc.fact != nil {
				assert.Equal(t, tc.fact.InvalidReason, group.ServingReason,
					"the serving field carries the Router's reason in the loss case as well")
			}
			if tc.wantState == workercore.ModelDeploymentGroupStateAwaitingObservation {
				assert.Contains(t, group.Reason, tc.fact.InvalidReason)
			} else {
				assert.Equal(t, tc.wantReason, group.Reason)
			}
		})
	}
}

// TestModelDeploymentGroupBarrierDoesNotTouchAGroupTheQualificationHolds pins that a group the
// qualification already holds keeps its own state and its own reason: the observation's reason
// never takes the place of the leg that holds the group.
func TestModelDeploymentGroupBarrierDoesNotTouchAGroupTheQualificationHolds(t *testing.T) {
	tests := []struct {
		name       string
		legs       []modelDeploymentQualificationLeg
		forward    modelDeploymentGroupForward
		wantState  workercore.ModelDeploymentGroupState
		wantReason string
	}{
		{
			name: "a_definite_fault",
			legs: []modelDeploymentQualificationLeg{
				{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
				{
					Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegFailed,
					Reason: "member qwen-server-0-1 failed its health check",
				},
				{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
			wantState:  workercore.ModelDeploymentGroupStateWithdrawn,
			wantReason: "member qwen-server-0-1 failed its health check",
		},
		{
			name: "a_shape_no_observation_exists_for",
			forward: modelDeploymentGroupForward{
				State:  modelDeploymentGroupForwardUnsupported,
				Reason: "the engine has no group-forward observation",
			},
			wantState:  workercore.ModelDeploymentGroupStateUnsupported,
			wantReason: "the engine has no group-forward observation",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := groupStatusRun(t, []groupStatusCase{{
				role: "server", ordinal: 0, size: 2,
				legs: tc.legs, forward: tc.forward,
			}}, &modelDeploymentGroupServing{
				SnapshotValid: false,
				InvalidReason: "the Router's view could not be read",
				State:         workercore.ModelDeploymentServingStateUnknown,
			}, true)

			group := groupStatusGroup(t, status)
			assert.Equal(t, tc.wantState, group.State)
			assert.Equal(t, tc.wantReason, group.Reason)
		})
	}
}

// TestModelDeploymentGroupQualificationTimeTracksThePublishedState pins the time against the
// state the group is published in: a group held in the same generation keeps the instant it
// earned, and a group that comes back records a new one.
//
// THE FIRST GROUP IS PUBLISHED AS ELIGIBLE, because that is the state the instant belongs to. A
// group the Router's observation holds is published as AwaitingObservation, and the case that says
// so is the next one.
func TestModelDeploymentGroupQualificationTimeTracksThePublishedState(t *testing.T) {
	cases := []groupStatusCase{{role: "server", ordinal: 0, size: 2}}
	eligible := groupStatusRun(t, cases, nil, false)
	earned := meta.Now().Rfc3339Copy()
	applyModelDeploymentGroupQualificationTimes(eligible, eligible.Roles, earned, nil)
	require.NotNil(t, groupStatusGroup(t, eligible).LastQualificationTime)

	// The group is held by the observation, in the SAME generation.
	held := groupStatusRun(t, cases, &modelDeploymentGroupServing{
		SnapshotValid: false,
		InvalidReason: "the Router's view could not be read",
		State:         workercore.ModelDeploymentServingStateUnknown,
	}, true)
	later := meta.NewTime(earned.Add(time.Hour))
	applyModelDeploymentGroupQualificationTimes(held, eligible.Roles, later, nil)

	require.NotNil(t, groupStatusGroup(t, held).LastQualificationTime)
	assert.Equal(t, earned.UTC(), groupStatusGroup(t, held).LastQualificationTime.UTC(),
		"a group held in the same generation keeps the instant it earned")

	// The group comes back in the SAME generation, and records a new instant.
	back := groupStatusRun(t, cases, &modelDeploymentGroupServing{
		SnapshotValid: true,
		State:         workercore.ModelDeploymentServingStateConfirmed,
		Endpoints:     1,
	}, true)
	applyModelDeploymentGroupQualificationTimes(back, held.Roles, later, nil)

	require.NotNil(t, groupStatusGroup(t, back).LastQualificationTime)
	assert.Equal(t, later.UTC(), groupStatusGroup(t, back).LastQualificationTime.UTC(),
		"a group that comes back records the new instant")
}

// TestModelDeploymentGroupQualificationTimeIsNotGivenToAHeldGroup pins that a group the Router's
// observation holds earns no qualification time of its own.
//
// THE INSTANT RECORDS THE TRANSITION INTO PUBLISHED ELIGIBLE, and a held group has not made that
// transition: every engine leg of this group verified, and what holds it is the Router's view
// rather than the engine's health. An instant here would name a transition that never happened,
// and it would be an instant a group that never qualified could not have earned.
func TestModelDeploymentGroupQualificationTimeIsNotGivenToAHeldGroup(t *testing.T) {
	cases := []groupStatusCase{{role: "server", ordinal: 0, size: 2}}
	held := groupStatusRun(t, cases, &modelDeploymentGroupServing{
		SnapshotValid: false,
		InvalidReason: "the Router's view could not be read",
		State:         workercore.ModelDeploymentServingStateUnknown,
	}, true)
	require.Equal(t, workercore.ModelDeploymentGroupStateAwaitingObservation,
		groupStatusGroup(t, held).State,
		"the observation holds a group whose engine legs all verified")

	applyModelDeploymentGroupQualificationTimes(held, held.Roles, meta.Now().Rfc3339Copy(), nil)

	assert.Nil(t, groupStatusGroup(t, held).LastQualificationTime,
		"a group that has never been published Eligible has no qualification time")
}

// groupStatusMemberReadinessLegs is the members-ready leg of a group whose members have not
// reported yet, and the member-set leg of a group that is short of one.
//
// THE TWO ARE DECLARED RATHER THAN DERIVED, because what the status publishes about a group is the
// qualification the pass reached, and the fixture states that qualification.
func groupStatusMemberReadinessLegs() []modelDeploymentQualificationLeg {
	return []modelDeploymentQualificationLeg{
		{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
		{
			Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegUnknown,
			Reason: "no member's readiness has been observed yet",
		},
		{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
		{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
		{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
	}
}

// TestModelDeploymentGroupStatusPublishesWhatTheMembersReport pins what the status publishes for
// the groups the pass observed, member by member.
//
// THE MEMBER READINESS IS THREE-VALUED, and the third value is not an absence: a member nobody has
// reported on is Unknown, which is a different fact from a member that was observed and found not
// ready.
//
// A GROUP NOTHING HAS REPORTED ON IS PUBLISHED, because it is a current group: it has members, the
// members have identities, and the state says they are still starting. What the pass did not
// evaluate has no view, no members and no entry, and nothing is invented about it.
func TestModelDeploymentGroupStatusPublishesWhatTheMembersReport(t *testing.T) {
	tests := []struct {
		name      string
		legs      []modelDeploymentQualificationLeg
		readiness []*core.ConditionStatus
		phases    []core.PodPhase
		wantState workercore.ModelDeploymentGroupState
		wantReady []meta.ConditionStatus
	}{
		{
			name:      "every member reported ready",
			readiness: []*core.ConditionStatus{ptr.To(core.ConditionTrue)},
			wantState: workercore.ModelDeploymentGroupStateEligible,
			wantReady: []meta.ConditionStatus{meta.ConditionTrue},
		},
		{
			name:      "a member nobody has reported on is Unknown beside one that has",
			readiness: []*core.ConditionStatus{ptr.To(core.ConditionTrue), nil},
			legs:      groupStatusMemberReadinessLegs(),
			wantState: workercore.ModelDeploymentGroupStateAwaitingReady,
			wantReady: []meta.ConditionStatus{meta.ConditionTrue, meta.ConditionUnknown},
		},
		{
			name:      "a pending pod is Unknown beside one that has reported",
			readiness: []*core.ConditionStatus{ptr.To(core.ConditionTrue), nil},
			phases:    []core.PodPhase{core.PodRunning, core.PodPending},
			legs:      groupStatusMemberReadinessLegs(),
			wantState: workercore.ModelDeploymentGroupStateAwaitingReady,
			wantReady: []meta.ConditionStatus{meta.ConditionTrue, meta.ConditionUnknown},
		},
		{
			name:      "a member found not ready publishes False",
			readiness: []*core.ConditionStatus{ptr.To(core.ConditionFalse)},
			legs: []modelDeploymentQualificationLeg{
				{Name: modelDeploymentLegMemberSetComplete, Verdict: modelDeploymentLegVerified},
				{
					Name: modelDeploymentLegMembersReady, Verdict: modelDeploymentLegFailed,
					Reason: "member qwen-server-0-0 reports not ready",
				},
				{Name: modelDeploymentLegGroupForward, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPlannedRetirement, Verdict: modelDeploymentLegVerified},
				{Name: modelDeploymentLegNoPendingReplacement, Verdict: modelDeploymentLegVerified},
			},
			wantState: workercore.ModelDeploymentGroupStateWithdrawn,
			wantReady: []meta.ConditionStatus{meta.ConditionFalse},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			md.Spec.Roles[0].ReplicaSize = int32(len(tc.readiness))
			case_ := groupStatusCase{
				role: "server", ordinal: 0, size: len(tc.readiness), legs: tc.legs,
				readiness: tc.readiness,
			}
			members, pods := groupStatusMembers(md, case_)
			for i, member := range members {
				if i < len(tc.phases) {
					member.Status.Phase = tc.phases[i]
				}
			}
			pods = pods[:0]
			for _, member := range members {
				pods = append(pods, *member)
			}

			status := groupStatusPublish(t, md, pods,
				[]modelDeploymentInstanceQualification{groupStatusQualification(case_, members)}, nil)

			groups := status.Roles[0].Groups
			require.Len(t, groups, 1, "a group the pass evaluated is published")
			assert.Equal(t, tc.wantState, groups[0].State)
			require.Len(t, groups[0].Members, len(tc.wantReady))
			for i, member := range groups[0].Members {
				assert.Equal(t, tc.wantReady[i], member.Ready,
					"the member readiness is what the Pod reports, and what it does not report is Unknown")
			}
		})
	}
}

// TestModelDeploymentGroupStatusPublishesNoGroupThePassDidNotEvaluate pins that a replica the
// pass never saw is not published at all.
//
// THE ABSENT REPLICA IS NOT A GROUP WITH AN UNKNOWN STATE: it has no view, no members and no
// membership, and publishing an entry for it would name a group that does not exist.
func TestModelDeploymentGroupStatusPublishesNoGroupThePassDidNotEvaluate(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.Roles[0].ReplicaSize = 1
	present := groupStatusCase{role: "server", ordinal: 0, size: 1}
	members, pods := groupStatusMembers(md, present)

	status := groupStatusPublish(t, md, pods,
		[]modelDeploymentInstanceQualification{groupStatusQualification(present, members)}, nil)

	groups := status.Roles[0].Groups
	require.Len(t, groups, 1, "only the replica the pass evaluated is published")
	assert.Equal(t, int32(0), groups[0].Ordinal,
		"the published group is the one the pass evaluated")
}

// TestModelDeploymentGroupStatusDistinguishesAnObservationTransitionFromAStablePass measures what
// the group projection publishes across three reconciles of one deployment.
//
// THE THREE PASSES ARE DIFFERENT IN KIND, and the difference is the whole point of the case. The
// first creates the replicas. The second is the pass that first OBSERVES them: the replicas it
// created are in its input now. The third changes nothing at all.
//
// WHAT IS UNDER TEST IS THAT THE OBSERVATION MOVED AND THE PUBLISHED ANSWER DID NOT. The first
// pass has no qualification for the replicas it just created -- its qualification input is the list
// the pass STARTED from, and the replicas were not in it -- so the pending state is DERIVED from
// the view. The second pass has one, and it is unobserved, because no member ever reported its
// readiness. The derived state and the qualified state are ONE classification, taken from the same
// member-leg helper, so the two passes publish the same groups.
//
// THE READINESS NEVER MOVED. No member of this deployment ever reported its readiness, so what
// changed between the first and the second pass was the AVAILABILITY of a qualification, not the
// readiness it reads.
//
// THE ZERO IN THE FIRST PASS IS NOT PUBLISHED ANYMORE. The rule that hid it -- a group the pass
// had no qualification for was left out of the status -- was this projection's own and was wrong:
// it made the answer about the pass's memory rather than about the cluster. Measured with that rule
// in place, the same three passes published 0, 2 and 2 groups; measured now, they publish 2, 2
// and 2.
//
// THE WRITE COUNTS ARE NOT ASSERTED HERE. That the second pass writes nothing is the existing
// idempotence fixture's contract, and a second copy of one fact is not a second proof of it.
func TestModelDeploymentGroupStatusDistinguishesAnObservationTransitionFromAStablePass(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, newRenderDeployment(), newRenderInstanceType())

	published := make([][]workercore.ModelDeploymentGroupStatus, 0, 3)
	for pass := 1; pass <= 3; pass++ {
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		published = append(published, publishedModelDeploymentGroups(getModelDeployment(t, cli)))
	}

	require.Len(t, published[0], 2, "the pass that creates the replicas publishes them")
	assert.Equal(t, published[0], published[1],
		"the pass that first observes the replicas publishes the same groups")
	assert.Equal(t, published[1], published[2],
		"the pass that changes nothing publishes the same groups")
}

// publishedModelDeploymentGroups collects the group entries a status publishes, in the order the
// roles carry them.
//
// THE ROLE IS NOT CARRIED ALONGSIDE, because every entry names its own role, and a reader that
// compared two collections could not tell a role's groups from another role's without it.
func publishedModelDeploymentGroups(
	md *workercore.ModelDeployment,
) []workercore.ModelDeploymentGroupStatus {
	published := make([]workercore.ModelDeploymentGroupStatus, 0)
	for _, role := range md.Status.Roles {
		published = append(published, role.Groups...)
	}

	return published
}

// TestModelDeploymentGroupStateVocabularyCarriesTheBarrierHold pins that the published vocabulary
// and the controller's own vocabulary cannot drift apart: the API carries every one of the
// controller's seven states, spelled the same way, and carries exactly one state of its own, the
// hold the qualification cannot name.
func TestModelDeploymentGroupStateVocabularyCarriesTheBarrierHold(t *testing.T) {
	controller := []modelDeploymentGroupState{
		modelDeploymentGroupStateEligible,
		modelDeploymentGroupStateWithdrawn,
		modelDeploymentGroupStateAwaitingMembers,
		modelDeploymentGroupStateAwaitingReady,
		modelDeploymentGroupStateAwaitingProbe,
		modelDeploymentGroupStateUnsupported,
		modelDeploymentGroupStateUnknown,
	}
	published := []workercore.ModelDeploymentGroupState{
		workercore.ModelDeploymentGroupStateEligible,
		workercore.ModelDeploymentGroupStateWithdrawn,
		workercore.ModelDeploymentGroupStateAwaitingMembers,
		workercore.ModelDeploymentGroupStateAwaitingReady,
		workercore.ModelDeploymentGroupStateAwaitingProbe,
		workercore.ModelDeploymentGroupStateUnsupported,
		workercore.ModelDeploymentGroupStateUnknown,
		workercore.ModelDeploymentGroupStateAwaitingObservation,
	}

	for _, state := range controller {
		assert.Contains(t, published, workercore.ModelDeploymentGroupState(state),
			"the published vocabulary carries every state the controller names")
	}
	assert.Contains(t, published, workercore.ModelDeploymentGroupStateAwaitingObservation,
		"the published vocabulary carries the hold the qualification cannot name")
	assert.Len(t, published, len(controller)+1,
		"the published vocabulary carries exactly one state of its own")
}

// The elastic collective's published group.
//
// THE COLLECTIVE IS ONE LOGICAL REPLICA of the elastic role, and its membership is the complete
// current set of GPU workers rather than the members of one ordinal. The helpers below build that
// shape, and the cases that follow are the shapes it can be in.

// collectiveStatusDeployment is an elastic deployment with one role of the given width.
//
// THE ROLE DECLARES ONE REPLICA, because the elastic profile freezes it there: the collective is
// that one replica, and the width is the engine world rather than a replica count.
func collectiveStatusDeployment(width int32) *workercore.ModelDeployment {
	md := newRenderDeployment()
	md.Spec.Roles[0].Replicas = 1
	md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: width}

	return md
}

// collectiveStatusWorker is one GPU worker of the elastic collective.
//
// THE MEMBER INDEX IS ZERO, because each rank is a replica of one member: the seat a rank claims
// inside its own replica is not a seat of the collective, and a fixture that gave it another one
// would describe a different shape.
func collectiveStatusWorker(
	md *workercore.ModelDeployment, ordinal int, ready *core.ConditionStatus,
) core.Pod {
	member := groupStatusMember(md, md.Spec.Roles[0], ordinal, 0, ready)

	return *member
}

// collectiveStatusHead is the ancillary Ray head, which is not a worker of the collective.
func collectiveStatusHead(md *workercore.ModelDeployment) core.Pod {
	member := groupStatusMember(
		md, workercore.ModelDeploymentRole{Name: modelDeploymentElasticHeadName(md)}, 0, 0, nil)

	return *member
}

// collectiveStatusPublish publishes the elastic status with the given members on the carrier.
//
// THE STATUS COUNTS THE MASTER, the way the elastic path does, and the collective names every
// worker the pass observed.
func collectiveStatusPublish(
	t *testing.T, md *workercore.ModelDeployment, workers, members []core.Pod,
	qualifications []modelDeploymentInstanceQualification,
	answer ServingAnswer,
) *workercore.ModelDeploymentStatus {
	t.Helper()

	return groupStatusPublishMembership(t, md, workers, qualifications, answer, members)
}

// TestElasticCollectiveStatusPublishesOneGroupForTheWholeCollective proves that the elastic
// deployment publishes ONE group whose membership is the complete current worker set.
//
// THE STATE IS UNKNOWN, because the operator never qualified the collective as one group, and the
// reason says so. THE MEMBERSHIP IS PUBLISHED ALL THE SAME, because the members and their
// readiness are what the operator knows.
//
// THE COUNTS STILL DESCRIBE THE MASTER. They are the role's replica counts, and the role declares
// one replica; the collective names the workers that make up that one replica.
func TestElasticCollectiveStatusPublishesOneGroupForTheWholeCollective(t *testing.T) {
	md := collectiveStatusDeployment(3)
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

	workers := make([]core.Pod, 0, 4)
	workers = append(workers,
		collectiveStatusWorker(md, 0, ptr.To(core.ConditionTrue)),
		collectiveStatusWorker(md, 1, ptr.To(core.ConditionTrue)),
		collectiveStatusWorker(md, 2, nil),
	)
	head := collectiveStatusHead(md)

	master := groupStatusQualification(
		groupStatusCase{role: md.Spec.Roles[0].Name, ordinal: 0}, []*core.Pod{&workers[0]})
	status := collectiveStatusPublish(t, md, workers[:1], append(workers, head),
		[]modelDeploymentInstanceQualification{master},
		ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

	require.Len(t, status.Roles, 1)
	require.Len(t, status.Roles[0].Groups, 1)

	group := status.Roles[0].Groups[0]
	assert.Equal(t, md.Spec.Roles[0].Name, group.Role)
	assert.Equal(t, int32(0), group.Ordinal)
	assert.Equal(t, workercore.ModelDeploymentGroupStateUnknown, group.State)
	assert.Equal(t, modelDeploymentReasonCollectiveNotEvaluated, group.Reason)
	assert.Nil(t, group.LastQualificationTime)

	require.Len(t, group.Members, 3)
	for i, member := range group.Members {
		assert.Equal(t, workers[i].Name, member.Name, "the collective names every current worker")
		assert.Equal(t, string(workers[i].UID), member.UID)
	}

	// THE COUNTS ARE THE ROLE'S REPLICA COUNTS, and the role declares one replica.
	assert.Equal(t, int32(1), status.Roles[0].Desired)
	assert.Equal(t, int32(1), status.Roles[0].Ready)
}

// TestElasticCollectiveStatusPublishesNoGroupWithoutWorkers proves that a collective with no
// workers is no group.
//
// THE MASTER IS ONE OF THE WORKERS, so a deployment whose workers are all absent has no master
// either, and there is nothing to name.
func TestElasticCollectiveStatusPublishesNoGroupWithoutWorkers(t *testing.T) {
	md := collectiveStatusDeployment(3)

	status := collectiveStatusPublish(t, md, nil, nil, nil,
		ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

	require.Len(t, status.Roles, 1)
	assert.Empty(t, status.Roles[0].Groups)
}

// TestElasticCollectiveStatusDoesNotReuseTheMastersServingFact proves that the Router's fact about
// the master is never published as the collective's answer.
//
// THE FACT IS FILED UNDER THE MASTER'S OWN KEY, and it CONFIRMS the group with a count. The
// collective's generation is the master's own generation here, because the collective holds one
// worker, so a lookup by key alone would find the fact and publish the confirmation. The
// collective is not the group the Router observed, so the fact is not its answer.
func TestElasticCollectiveStatusDoesNotReuseTheMastersServingFact(t *testing.T) {
	md := collectiveStatusDeployment(2)
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

	workers := []core.Pod{collectiveStatusWorker(md, 0, ptr.To(core.ConditionTrue))}
	master := groupStatusQualification(
		groupStatusCase{role: md.Spec.Roles[0].Name, ordinal: 0}, []*core.Pod{&workers[0]})

	answer := ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{
		groupStatusKey(md.Spec.Roles[0].Name, 0, master.Generation): {
			SnapshotValid: true,
			State:         workercore.ModelDeploymentServingStateConfirmed,
			Endpoints:     3,
		},
	}}

	status := collectiveStatusPublish(t, md, workers, workers,
		[]modelDeploymentInstanceQualification{master}, answer)

	require.Len(t, status.Roles, 1)
	require.Len(t, status.Roles[0].Groups, 1)

	group := status.Roles[0].Groups[0]
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
		"the Router's fact is about the master, not about the collective")
	assert.Equal(t, modelDeploymentReasonGroupUnobserved, group.ServingReason)
	assert.Nil(t, group.Serving.Value, "only a confirmation carries a count")
	assert.Nil(t, group.LastQualificationTime, "the collective never earned a qualification time")
}

// TestElasticCollectiveStatusSeatsAreTheAdmissionOrdinals proves that a member's seat is its
// admission ordinal, and that the published order is the seat order.
//
// EVERY WORKER CLAIMS SEAT ZERO INSIDE ITS OWN REPLICA, because each rank is a replica of one
// member. Publishing that would put three members in one seat, so the collective's seat is the
// ordinal the worker was admitted at.
func TestElasticCollectiveStatusSeatsAreTheAdmissionOrdinals(t *testing.T) {
	md := collectiveStatusDeployment(3)

	workers := []core.Pod{
		collectiveStatusWorker(md, 2, ptr.To(core.ConditionTrue)),
		collectiveStatusWorker(md, 0, ptr.To(core.ConditionTrue)),
		collectiveStatusWorker(md, 1, ptr.To(core.ConditionTrue)),
	}
	// THE FIXTURE LISTED THE PODS IN ANOTHER ORDER, and the published order is the seat order.
	expects := []core.Pod{workers[1], workers[2], workers[0]}

	status := collectiveStatusPublish(t, md, workers[:1], workers, nil,
		ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

	require.Len(t, status.Roles, 1)
	require.Len(t, status.Roles[0].Groups, 1)

	group := status.Roles[0].Groups[0]
	require.Len(t, group.Members, 3)
	for i, member := range group.Members {
		require.NotNil(t, member.Seat)
		assert.Equal(t, int32(i), *member.Seat, "the seat is the admission ordinal")
		assert.Equal(t, expects[i].Name, member.Name, "the published order is the seat order")
	}
}

// TestElasticCollectiveStatusPublishesTriStateReadiness proves that the collective's members keep
// the three-valued readiness every other member publishes.
//
// A WORKER NOBODY HAS REPORTED ON IS UNKNOWN, and a worker observed and found not ready is False.
// The collective's own state says nothing about either: it names the collective, not its members.
func TestElasticCollectiveStatusPublishesTriStateReadiness(t *testing.T) {
	cases := []struct {
		name    string
		ready   *core.ConditionStatus
		expects meta.ConditionStatus
	}{
		{name: "ready", ready: ptr.To(core.ConditionTrue), expects: meta.ConditionTrue},
		{name: "not ready", ready: ptr.To(core.ConditionFalse), expects: meta.ConditionFalse},
		{name: "never reported", ready: nil, expects: meta.ConditionUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := collectiveStatusDeployment(2)
			workers := []core.Pod{collectiveStatusWorker(md, 0, tc.ready)}

			status := collectiveStatusPublish(t, md, workers, workers, nil,
				ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

			require.Len(t, status.Roles, 1)
			require.Len(t, status.Roles[0].Groups, 1)
			require.Len(t, status.Roles[0].Groups[0].Members, 1)
			assert.Equal(t, tc.expects, status.Roles[0].Groups[0].Members[0].Ready)
		})
	}
}

// TestElasticCollectiveStatusFollowsTheMembershipGeneration proves that a replaced worker makes a
// new generation.
//
// THE GENERATION IS THE MEMBERSHIP, so a worker that came back under a new identity is a different
// membership and the collective says so.
func TestElasticCollectiveStatusFollowsTheMembershipGeneration(t *testing.T) {
	md := collectiveStatusDeployment(2)

	workers := []core.Pod{
		collectiveStatusWorker(md, 0, ptr.To(core.ConditionTrue)),
		collectiveStatusWorker(md, 1, ptr.To(core.ConditionTrue)),
	}
	first := collectiveStatusPublish(t, md, workers[:1], workers, nil,
		ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

	replaced := workers[1]
	replaced.UID = types.UID(replaced.Name + "-replaced-uid")
	second := collectiveStatusPublish(t, md, workers[:1], []core.Pod{workers[0], replaced}, nil,
		ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

	require.Len(t, first.Roles, 1)
	require.Len(t, second.Roles, 1)
	require.Len(t, first.Roles[0].Groups, 1)
	require.Len(t, second.Roles[0].Groups, 1)

	assert.NotEqual(t, first.Roles[0].Groups[0].Generation, second.Roles[0].Groups[0].Generation,
		"a replaced worker is a different membership")
}

// TestElasticCollectiveStatusExcludesTheAncillaryHead proves that the Ray head is not a member of
// the collective.
//
// THE HEAD IS ANCILLARY: it requests no container resources, it never counts toward the width, and
// it is not one of the workers the collective is made of.
func TestElasticCollectiveStatusExcludesTheAncillaryHead(t *testing.T) {
	md := collectiveStatusDeployment(2)

	workers := make([]core.Pod, 0, 2)
	workers = append(workers, collectiveStatusWorker(md, 0, ptr.To(core.ConditionTrue)))
	head := collectiveStatusHead(md)

	status := collectiveStatusPublish(t, md, workers[:1], append(workers, head), nil,
		ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

	require.Len(t, status.Roles, 1)
	require.Len(t, status.Roles[0].Groups, 1)

	group := status.Roles[0].Groups[0]
	require.Len(t, group.Members, 1)
	assert.Equal(t, workers[0].Name, group.Members[0].Name)
}

// TestElasticCollectiveStatusLeavesTheFixedShapeAlone proves that a deployment with no collective
// publishes the replica-derived groups it always published.
//
// THE FIXED SHAPE HAS NO COLLECTIVE, and a shape with no collective keeps its own groups.
func TestElasticCollectiveStatusLeavesTheFixedShapeAlone(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.Roles[0].ReplicaSize = 2

	members, pods := groupStatusMembers(md, groupStatusCase{
		role: md.Spec.Roles[0].Name, ordinal: 0, size: 2,
	})
	qualification := groupStatusQualification(groupStatusCase{
		role: md.Spec.Roles[0].Name, ordinal: 0, size: 2,
	}, members)

	status := collectiveStatusPublish(t, md, pods, nil,
		[]modelDeploymentInstanceQualification{qualification},
		ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{}})

	require.Len(t, status.Roles, 1)
	require.Len(t, status.Roles[0].Groups, 1)

	group := status.Roles[0].Groups[0]
	assert.Equal(t, md.Spec.Roles[0].Name, group.Role)
	assert.Equal(t, int32(0), group.Ordinal)
	require.Len(t, group.Members, 2)
}

// TestElasticCollectiveStatusIsPublishedByTheElasticPath proves that the elastic path itself
// publishes the collective, and not only the projection that the controls above drive.
//
// THE COLLECTIVE'S MEMBERSHIP IS THE LIST THE PASS ALREADY READ, so the path spends no second
// fetch on it, and the head is excluded by construction.
//
// THE PASS RUNS TWICE, because the fake client does not give a created Pod the identity the API
// server gives it, and the live-member list refuses a Pod without one. The fixture gives the Pods
// their identities when it starts them, and the pass after that sees the collective. A cluster
// assigns the identity at creation, so its first pass already sees it.
func TestElasticCollectiveStatusIsPublishedByTheElasticPath(t *testing.T) {
	f := newElasticConvergenceFixture(t)

	// THE FIRST PASS PUBLISHES NO GROUP. The Pods the fixture created have no identity yet, so the
	// live-member list the carrier is built from is empty, and an absent membership publishes no
	// group rather than the master's own replica group: that group answers a question about one
	// replica of the role, and the collective is the role's one replica seen whole.
	first := new(workercore.ModelDeployment)
	require.NoError(t, f.reconciler.Client.Get(
		context.Background(), ctrlcli.ObjectKeyFromObject(f.md), first))
	require.Len(t, first.Status.Roles, 1)
	assert.Empty(t, first.Status.Roles[0].Groups,
		"an absent collective membership publishes no group at all")

	_, err := f.reconciler.convergeModelDeployment(context.Background(), f.md)
	require.NoError(t, err)

	stored := new(workercore.ModelDeployment)
	require.NoError(t, f.reconciler.Client.Get(
		context.Background(), ctrlcli.ObjectKeyFromObject(f.md), stored))

	pods := new(core.PodList)
	require.NoError(t, f.reconciler.Client.List(context.Background(), pods))

	workers := 0
	for i := range pods.Items {
		if modelDeploymentPodRole(&pods.Items[i]) == f.md.Spec.Roles[0].Name {
			workers++
		}
	}
	require.Positive(t, workers, "the fixture's elastic deployment holds workers")

	require.Len(t, stored.Status.Roles, 1)
	require.Len(t, stored.Status.Roles[0].Groups, 1)

	group := stored.Status.Roles[0].Groups[0]
	assert.Equal(t, f.md.Spec.Roles[0].Name, group.Role)
	assert.Equal(t, int32(0), group.Ordinal)
	assert.Equal(t, workercore.ModelDeploymentGroupStateUnknown, group.State)
	assert.Equal(t, modelDeploymentReasonCollectiveNotEvaluated, group.Reason)
	assert.Len(t, group.Members, workers, "the collective names every worker and not the head")
}

// TestElasticCollectiveStatusInheritsNeitherTheMastersInstantNorItsServingFact proves that the
// collective takes nothing from the entry that was published in its slot before.
//
// THE SEEDED PUBLICATION IS THE MASTER'S OWN GROUP, WITH AN INSTANT AND AN EXACT CONFIRMED
// SERVING FACT, and the collective carries the master's own generation, because the collective
// holds one worker. A settlement that compared the generation alone would find the prior entry in
// its own slot and copy the instant across, and a lookup by key alone would find the fact and
// publish the confirmation. The collective is neither the group that was published before nor the
// group the Router observed.
func TestElasticCollectiveStatusInheritsNeitherTheMastersInstantNorItsServingFact(t *testing.T) {
	md := collectiveStatusDeployment(2)
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}

	workers := []core.Pod{collectiveStatusWorker(md, 0, ptr.To(core.ConditionTrue))}
	master := groupStatusQualification(
		groupStatusCase{role: md.Spec.Roles[0].Name, ordinal: 0}, []*core.Pod{&workers[0]})

	earned := meta.NewTime(time.Now().Add(-time.Hour))
	previous := &workercore.ModelDeploymentStatus{
		Roles: []workercore.ModelDeploymentRoleStatus{{
			Name: md.Spec.Roles[0].Name,
			Groups: []workercore.ModelDeploymentGroupStatus{{
				Role:                  md.Spec.Roles[0].Name,
				Ordinal:               0,
				Generation:            string(master.Generation),
				State:                 workercore.ModelDeploymentGroupStateEligible,
				LastQualificationTime: &earned,
				Serving: workercore.ModelDeploymentServingStatus{
					State: workercore.ModelDeploymentServingStateConfirmed,
					Value: ptr.To(int32(3)),
				},
			}},
		}},
	}

	answer := ServingAnswer{Groups: map[modelDeploymentGroupKey]modelDeploymentGroupServing{
		groupStatusKey(md.Spec.Roles[0].Name, 0, master.Generation): {
			SnapshotValid: true,
			State:         workercore.ModelDeploymentServingStateConfirmed,
			Endpoints:     3,
		},
	}}

	status := groupStatusPublishMembership(t, md, workers, nil, answer, workers)
	// THE SETTLEMENT RUNS AGAINST THE PREVIOUS PUBLICATION, the way a second pass settles it.
	applyModelDeploymentGroupQualificationTimes(
		status, previous.Roles, meta.NewTime(time.Now()),
		modelDeploymentElasticCollectiveView(md, workers))

	require.Len(t, status.Roles, 1)
	require.Len(t, status.Roles[0].Groups, 1)

	group := status.Roles[0].Groups[0]
	assert.Equal(t, string(master.Generation), group.Generation,
		"the collective carries the master's own generation at one worker")
	assert.Nil(t, group.LastQualificationTime, "the collective inherits no instant")
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
		"the Router's fact is about the master, not about the collective")
	assert.Nil(t, group.Serving.Value, "only a confirmation carries a count")
}

// TestModelDeploymentGroupStatusPublishesAGroupThatRetainedItsQualification pins the status half
// of the retention: the group a lost collection left qualified publishes Eligible, and its
// serving field publishes Unknown.
//
// THE ANSWER IS THE COLLECTOR'S OWN, not a fact this case assembled. The retention is proved by
// the fresh membership the collector read, so a status case that built its own fact would be
// testing a fact nothing produced.
//
// THE TWO FIELDS ANSWER DIFFERENT QUESTIONS, and the case keeps them apart: the state names what
// holds the GROUP, and the serving field names what the ROUTER says about it. A group that kept
// its eligibility is still the group that was qualified, and no view was read, so no count is
// published.
func TestModelDeploymentGroupStatusPublishesAGroupThatRetainedItsQualification(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: &routerListFailureClient{Reader: fixture.cli},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	answer := r.observeModelDeploymentServing(context.Background(), fixture.md)
	fact, observed := answer.Groups[fixture.key]
	require.True(t, observed, "the collection lost the Router, not the group")
	require.True(t, fact.Retained, "the membership was read fresh and every member carried the label")

	qualification := modelDeploymentTestQualification(fixture.members, fixture.generation, false)
	qualifications := []modelDeploymentInstanceQualification{qualification}
	pods := make([]core.Pod, 0, len(fixture.members))
	for _, pod := range fixture.members {
		pods = append(pods, *pod)
	}

	status, err := r.computeModelDeploymentStatusWithQualifications(
		context.Background(), fixture.md, pods, nil, nil, nil, qualifications)
	require.NoError(t, err)
	collective := applyModelDeploymentGroupStatuses(
		fixture.md, status.Roles, qualifications, pods, nil)
	r.applyModelDeploymentGroupAnswer(status, modelDeploymentPassObservation{
		Serving:        answer,
		Qualifications: qualifications,
	}, fixture.md, collective)

	group := groupStatusGroup(t, status)
	assert.Equal(t, workercore.ModelDeploymentGroupStateEligible, group.State,
		"the group is still the group that was qualified")
	assert.Equal(t, "every leg of the group verified", group.Reason)
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
		"no view was read, so the serving answer confirms nothing")
	assert.Nil(t, group.Serving.Value, "only a confirmation carries a count")
}

// TestModelDeploymentGroupStatusPublishesAwaitingReadyForAFreshlyUnreadyMember pins the state a
// group the fresh membership found unsafe publishes.
//
// THE FRESH MEMBERSHIP IS THE AUTHORITY. The qualification's own readiness legs read the caller's
// list, and a list that is behind the API server cannot say that a member is ready, so it cannot
// keep a group open that the fresh read has found unsafe. The status publishes what the fresh
// evidence supports, and the fresh evidence here says the group may not be served.
//
// THE STATE NAMES THE LEG THAT HOLDS THE GROUP. A group whose member is not ready is held by that
// member's readiness, so it is AwaitingReady, and not AwaitingObservation, which names a group the
// Router's observation holds. The reason names the member, and the serving answer confirms nothing.
func TestModelDeploymentGroupStatusPublishesAwaitingReadyForAFreshlyUnreadyMember(t *testing.T) {
	fixture := newQualifiedGroupFixture(t)

	now := time.Now()
	r := &ModelDeploymentReconciler{
		Client: fixture.cli, APIReader: &unreadyMemberClient{
			Reader: fixture.cli, uid: fixture.leaderUID(t),
		},
		Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:    func() time.Time { return now },
		servingViewFetch: func(context.Context, string) ([]byte, error) {
			return []byte(observerHealthyPayload), nil
		},
	}

	answer := r.observeModelDeploymentServing(context.Background(), fixture.md)
	fact, observed := answer.Groups[fixture.key]
	require.True(t, observed, "the collection read the group")
	require.False(t, fact.Retained, "a group with an unready member is not retained")
	require.False(t, fact.SnapshotValid, "an unsafe group has no valid observation to restore from")

	qualification := modelDeploymentTestQualification(fixture.members, fixture.generation, false)
	qualifications := []modelDeploymentInstanceQualification{qualification}
	pods := make([]core.Pod, 0, len(fixture.members))
	for _, pod := range fixture.members {
		pods = append(pods, *pod)
	}

	status, err := r.computeModelDeploymentStatusWithQualifications(
		context.Background(), fixture.md, pods, nil, nil, nil, qualifications)
	require.NoError(t, err)
	collective := applyModelDeploymentGroupStatuses(
		fixture.md, status.Roles, qualifications, pods, nil)
	r.applyModelDeploymentGroupAnswer(status, modelDeploymentPassObservation{
		Serving:        answer,
		Qualifications: qualifications,
	}, fixture.md, collective)

	group := groupStatusGroup(t, status)
	assert.Equal(t, workercore.ModelDeploymentGroupStateAwaitingReady, group.State,
		"the state names the leg that holds the group, and the leg is the member's readiness")
	assert.Contains(t, group.Reason, fixture.leaderName(t),
		"the reason names the member the fresh read found not ready")
	assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
		"unsafe evidence confirms nothing")
	assert.Nil(t, group.Serving.Value, "only a confirmation carries a count")
}

// TestModelDeploymentGroupStatusNamesTheProxyShapeInTheReasonOfAVerifiedGroup pins the reason a
// qualified group publishes when the routing proxy answered its forward probe.
func TestModelDeploymentGroupStatusNamesTheProxyShapeInTheReasonOfAVerifiedGroup(t *testing.T) {
	const proxyReason = "routing proxy returned the expected content with the probe request ID while " +
		"naming the prefiller; individual rank coverage and KV transfer remain unproven"

	tests := []struct {
		name       string
		shape      modelDeploymentGroupForwardPairShape
		wantReason string
	}{
		{
			name:       "the_routing_proxy_shape_names_the_limit_it_carries",
			shape:      modelDeploymentGroupForwardPairShapeViaRoutingProxy,
			wantReason: proxyReason,
		},
		{
			name:       "the_head_to_head_shape_keeps_the_generic_reason",
			shape:      modelDeploymentGroupForwardPairShapeHeadToHead,
			wantReason: "every leg of the group verified",
		},
		{
			name:       "a_group_with_no_pair_attempt_keeps_the_generic_reason",
			wantReason: "every leg of the group verified",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := groupStatusRun(t, []groupStatusCase{{
				role: "server", ordinal: 0, size: 2, shape: tc.shape,
				forward: modelDeploymentGroupForward{
					State:  modelDeploymentGroupForwardVerified,
					Reason: "the group answered the forward observation",
				},
			}}, &modelDeploymentGroupServing{
				SnapshotValid: false,
				InvalidReason: "the Router's view could not be read",
				State:         workercore.ModelDeploymentServingStateUnknown,
				Retained:      true,
			}, true)

			group := groupStatusGroup(t, status)
			assert.Equal(t, workercore.ModelDeploymentGroupStateEligible, group.State,
				"the state does not change: the group is still qualified")
			assert.Equal(t, tc.wantReason, group.Reason)
			assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
				"the serving answer confirms nothing either way")
			assert.Nil(t, group.Serving.Value, "only a confirmation carries a count")
		})
	}
}
