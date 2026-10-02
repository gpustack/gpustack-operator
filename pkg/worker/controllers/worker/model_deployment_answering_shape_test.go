package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// THE MATRIX IS ABOUT ONE QUESTION: WHICH MEMBERS GET READ. A replica the operator cannot retire is
// not a slow retirement, it is no retirement at all, so every case here is about whether the drain
// asks the right Pods and refuses when it cannot tell.

// shapeMember is a member the shape read can actually look at: the health fixture carries labels and
// annotations but no engine container, and this task's question is answered from a member's own
// rendered command, so the container has to be there to be read.
func shapeMember(ordinal, member int, uid string, command ...string) core.Pod {
	pod := healthPod("server", ordinal, member, healthBool(true), uid)
	pod.Spec.Containers = []core.Container{{
		Name:    modelDeploymentMainContainerName,
		Image:   "vllm/vllm-openai:v0.29.0",
		Command: command,
	}}

	return pod
}

// shapeUnlabelledMember is a member carrying no member-index label at all, which is what a replica
// rendered before the label existed looks like. It is the case the leader search has to get right
// for the wrong reason not to be right by accident: read through a helper that defaults a missing
// label to the leader index, every member of this replica reports itself the leader.
func shapeUnlabelledMember(ordinal int, uid string, command ...string) core.Pod {
	pod := shapeMember(ordinal, 0, uid, command...)
	delete(pod.Labels, modelDeploymentMemberIndexLabel)

	return pod
}

// shapeExternalDPCommand is the rendered command of a member running an External-DP shape. It is
// the flag modelDeploymentLoadBalance votes a shape from, so a member carrying it and a role
// declaring the same flag agree about what the replica is.
var shapeExternalDPCommand = []string{"vllm", "serve", "model", "--data-parallel-external-lb"}

// shapePlainCommand is a leader-served member: it runs its engine and no balance flag.
var shapePlainCommand = []string{"vllm", "serve", "model", "--tensor-parallel-size", "2"}

// shapeDeployment is a deployment whose single role declares the given instance size, which is the
// number of Pods one replica is made of.
func shapeDeployment(replicaSize int32, extraArgs ...string) *workercore.ModelDeployment {
	return retirementDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = replicaSize
		md.Spec.Roles[0].ExtraArgs = extraArgs
	})
}

// shapeAbsentRole is the same deployment with the role removed from the spec, which is the case the
// drain path exists to tolerate: the removal is for a role the spec no longer names.
func shapeAbsentRole(replicaSize int32) *workercore.ModelDeployment {
	md := shapeDeployment(replicaSize)
	md.Spec.Roles = nil

	return md
}

// recordingDrainReader answers idle or busy per member and RECORDS which members were asked, because
// "the follower was not read" and "the follower was read and its answer ignored" are the same state
// transition and only one of them is the repair.
type recordingDrainReader struct {
	busy  map[types.UID]bool
	read  []types.UID
	order []string
}

func (r *recordingDrainReader) Drain(
	_ context.Context, target modelDeploymentDrainTarget,
) (modelDeploymentDrainAnswer, error) {
	r.read = append(r.read, target.PodUID)
	r.order = append(r.order, target.Container)

	if r.busy[target.PodUID] {
		return busyDrain(), nil
	}

	return idleDrain(), nil
}

// readCounts is every member's read count, so a case can assert the whole set rather than the one
// member it happens to be about.
func (r *recordingDrainReader) readCounts() map[string]int {
	counts := map[string]int{}
	for _, uid := range r.read {
		counts[string(uid)]++
	}

	return counts
}

// planFor reserves the replica at Draining and returns the plan the protocol reaches.
func planFor(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod, reader *recordingDrainReader,
) *modelDeploymentRetirementPlan {
	t.Helper()

	uids := make([]string, 0, len(pods))
	for i := range pods {
		uids = append(uids, string(pods[i].UID))
	}
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, uids))
	cli := newModelDeploymentClient(md.DeepCopy())
	r := holdReconciler(cli, reader, time.Now())

	return r.planModelDeploymentRetirement(context.Background(), md, pods)
}

// TestDrainReadsTheMembersTheShapeMakesAnswerable is the matrix.
//
// A leader-served replica answers through its leader, an External-DP replica through every member,
// and a replica of one Pod through itself. A replica whose answering members cannot be named holds,
// and the reason says which shape could not be established.
func TestDrainReadsTheMembersTheShapeMakesAnswerable(t *testing.T) {
	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		pods []core.Pod
		busy map[types.UID]bool
		// wantState is the state the protocol reaches: Deleting means it drained.
		wantState workercore.ModelDeploymentRetirementState
		// wantReads is exactly which members were read and how many times each.
		wantReads map[string]int
		// wantReason, when set, must appear in the hold reason.
		wantReason string
	}{
		{
			name: "a leader-served replica of two drains through its leader alone",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 1, "m-1", shapePlainCommand...),
			},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2},
		},
		{
			name: "a busy follower is never read, so it cannot hold the operation",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 1, "m-1", shapePlainCommand...),
			},
			busy:      map[types.UID]bool{"m-1": true},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2},
		},
		{
			name: "a busy leader holds, because the leader is the replica's answer",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 1, "m-1", shapePlainCommand...),
			},
			busy:       map[types.UID]bool{"m-0": true},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{"m-0": 1},
			wantReason: "still holds",
		},
		{
			name:      "a replica of one Pod drains through itself",
			md:        shapeDeployment(1),
			pods:      []core.Pod{shapeMember(1, 0, "m-0", shapePlainCommand...)},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2},
		},
		{
			name: "a leader-served replica whose role is gone drains through the leader",
			md:   shapeAbsentRole(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 1, "m-1", shapePlainCommand...),
			},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2},
		},
		{
			name:      "a replica of one Pod whose role is gone drains through itself",
			md:        shapeAbsentRole(1),
			pods:      []core.Pod{shapeMember(1, 0, "m-0", shapePlainCommand...)},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2},
		},
		{
			// THE EXTERNAL-DP SHAPE IS REACHABLE FROM THE DRAIN ONLY WITHOUT A ROLE. With a role
			// declared, the disaggregation refusal above the selection is the earlier and the
			// stronger answer, and it is not this task's to move.
			name: "an External-DP replica whose role is gone reads every member",
			md:   shapeAbsentRole(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapeExternalDPCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2, "m-1": 2},
		},
		{
			name: "an External-DP replica without a role holds when one of its members is busy",
			md:   shapeAbsentRole(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapeExternalDPCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			busy:       map[types.UID]bool{"m-1": true},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{"m-0": 2, "m-1": 1},
			wantReason: "still holds",
		},
		{
			name: "a role declaring External-DP is refused before any member is read",
			md:   shapeDeployment(2, "--data-parallel-external-lb"),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapeExternalDPCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "no verifiable decoder-side release",
		},
		{
			name: "a leader-served replica with no member carrying the leader index holds",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 1, "m-1", shapePlainCommand...),
				shapeMember(1, 2, "m-2", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "no member carrying the leader index",
		},
		{
			// THE CASE THE LABEL READ EXISTS FOR. Neither member says which rank it is, so neither
			// can be shown to be the leader, and a search that treated "unknown rank" as "rank zero"
			// would return this whole replica as its own answer and drain it on a guess.
			name: "a leader-served replica whose members carry no member index holds",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeUnlabelledMember(1, "m-0", shapePlainCommand...),
				shapeUnlabelledMember(1, "m-1", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "no member carrying the leader index",
		},
		{
			name: "a leader-served replica with two leaders holds and says how many",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 0, "m-0b", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "2 members carrying the leader index",
		},
		{
			name: "a replica whose members disagree on the shape holds",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "the replica's members disagree on whether it is disaggregated",
		},
		{
			name:       "a role that declares more members than the replica holds",
			md:         shapeDeployment(3),
			pods:       []core.Pod{shapeMember(1, 0, "m-0", shapePlainCommand...)},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "declares 3 members and the replica holds 1",
		},
		{
			// THE DIRECTION THAT REACHES THE CHECK. A role declaring External-DP is refused one
			// step earlier, so the only disagreement this can observe is a leader-served role
			// above members whose own rendered command says otherwise.
			name: "a role and a rendered command that disagree on the shape",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapeExternalDPCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "the role and the replica's own rendered command disagree",
		},
		{
			name: "a replica with no role and no readable command cannot be classified",
			md:   shapeAbsentRole(2),
			pods: []core.Pod{
				healthPod("server", 1, 0, healthBool(true), "m-0"),
				shapeMember(1, 1, "m-1", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "no engine container to read",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recordingDrainReader{busy: tc.busy}
			plan := planFor(t, tc.md, tc.pods, reader)

			assert.Equal(t, tc.wantState, plan.Reservation.State,
				"reason was %q", plan.Reservation.Reason)
			if tc.wantReason != "" {
				assert.Contains(t, plan.Reservation.Reason, tc.wantReason)
			}

			assert.Equal(t, tc.wantReads, reader.readCounts(),
				"the read set is the repair; a member read here that should not be, or missing, is the defect")
		})
	}
}

// TestDrainSizeOneIsUnchanged pins the case the escalation was about: a replica of one Pod must
// behave exactly as it did before this change.
//
// It is asserted as a COUNT AND A SEQUENCE rather than as a passing test, because the property that
// matters is that the selection is a no-op there, and a test that only checked the final state would
// pass just as happily if the selection had started skipping the only member it had.
func TestDrainSizeOneIsUnchanged(t *testing.T) {
	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
	}{
		{name: "with the role still declared", md: shapeDeployment(1)},
		{name: "with the role no longer declared", md: shapeAbsentRole(1)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recordingDrainReader{}
			plan := planFor(t, tc.md, []core.Pod{shapeMember(1, 0, "m-0", shapePlainCommand...)}, reader)

			assert.Equal(t, workercore.ModelDeploymentRetirementStateDeleting, plan.Reservation.State)
			assert.Equal(t, []string{
				modelDeploymentMainContainerName, modelDeploymentMainContainerName,
			}, reader.order, "the sole member is read twice, in the container the renderer names")
			assert.Len(t, reader.read, 2, "the two-consecutive rule is unchanged at size one")
		})
	}
}

// TestDrainSelectionDoesNotNarrowTheDeleteBinding is the seam test.
//
// THE SELECTION IS A MEASUREMENT, NOT A DELETE. If it moved into target resolution the operation
// would commit against fewer member UIDs than admission bound, and could report completion with a
// member of the target still running. The bound set is what the commit's preconditions come from, so
// it has to survive the drain reading one member of a two-member replica.
func TestDrainSelectionDoesNotNarrowTheDeleteBinding(t *testing.T) {
	md := shapeDeployment(2)
	pods := []core.Pod{
		shapeMember(1, 0, "m-0", shapePlainCommand...),
		shapeMember(1, 1, "m-1", shapePlainCommand...),
	}
	reader := &recordingDrainReader{}

	plan := planFor(t, md, pods, reader)

	require.Equal(t, workercore.ModelDeploymentRetirementStateDeleting, plan.Reservation.State)
	assert.Len(t, plan.Target.Members, 2,
		"the delete binding still covers every member admission admitted")
	assert.Equal(t, 2, plan.held.Len(),
		"the held set is the delete precondition set and must not be narrowed by a measurement")
}

// TestAnsweringMembersRefusesAnUnnameableLeader is the unit behind the matrix's two leader cases,
// kept separate so the reason strings are asserted where they are written.
func TestAnsweringMembersRefusesAnUnnameableLeader(t *testing.T) {
	leader := shapeMember(1, 0, "m-0", shapePlainCommand...)
	follower := shapeMember(1, 1, "m-1", shapePlainCommand...)
	other := shapeMember(1, 2, "m-2", shapePlainCommand...)
	duplicate := leaderClone(leader)
	unlabelledA := shapeUnlabelledMember(1, "m-u0", shapePlainCommand...)
	unlabelledB := shapeUnlabelledMember(1, "m-u1", shapePlainCommand...)

	testCases := []struct {
		name       string
		shape      modelDeploymentAnsweringShape
		members    []*core.Pod
		wantCount  int
		wantReason string
	}{
		{
			name:      "an External-DP replica answers through every member",
			shape:     modelDeploymentAnsweringAll,
			members:   []*core.Pod{&leader, &follower},
			wantCount: 2,
		},
		{
			name:      "a replica of one Pod answers through itself",
			shape:     modelDeploymentAnsweringSole,
			members:   []*core.Pod{&leader},
			wantCount: 1,
		},
		{
			name:      "a leader-served replica answers through its leader",
			shape:     modelDeploymentAnsweringLeader,
			members:   []*core.Pod{&leader, &follower},
			wantCount: 1,
		},
		{
			name:       "a leader-served replica with no leader holds",
			shape:      modelDeploymentAnsweringLeader,
			members:    []*core.Pod{&follower, &other},
			wantReason: "no member carrying the leader index",
		},
		{
			name:       "a leader-served replica with two leaders holds",
			shape:      modelDeploymentAnsweringLeader,
			members:    []*core.Pod{&leader, duplicate},
			wantReason: "2 members carrying the leader index",
		},
		{
			// AN UNLABELLED MEMBER IS NOT A LEADER AND NOT A FOLLOWER. The index is read rather than
			// defaulted precisely so that a replica whose members all lack it produces no leader at
			// all, instead of every one of them claiming to be the leader.
			name:       "members with no index are not read as leaders",
			shape:      modelDeploymentAnsweringLeader,
			members:    []*core.Pod{&unlabelledA, &unlabelledB},
			wantReason: "no member carrying the leader index",
		},
		{
			name:       "an unestablished shape holds and names itself",
			shape:      modelDeploymentAnsweringUnknown,
			members:    []*core.Pod{&leader, &follower},
			wantReason: "shape could not be established",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			answering, reason := modelDeploymentAnsweringMembers(tc.shape, tc.members)

			if tc.wantReason != "" {
				require.NotEmpty(t, reason, "this case must hold")
				assert.Contains(t, reason, tc.wantReason)
				assert.Empty(t, answering)
				return
			}
			assert.Empty(t, reason)
			assert.Len(t, answering, tc.wantCount)
		})
	}
}

// leaderClone is a second Pod carrying the same leader index, which is the duplicated-leadership
// case a cluster that rendered its own members cannot produce and a corrupted or hand-edited one
// can.
func leaderClone(leader core.Pod) *core.Pod {
	clone := *leader.DeepCopy()
	clone.Name = leader.Name + "-duplicate"
	clone.UID = leader.UID + "-dup"

	return &clone
}

// TestAnsweringShapeIsOneDerivation pins that the shape is computed in exactly one place, so
// eligibility and the drain cannot come to answer the same question differently.
func TestAnsweringShapeIsOneDerivation(t *testing.T) {
	testCases := []struct {
		name       string
		externalDP bool
		size       int
		want       modelDeploymentAnsweringShape
	}{
		{
			name:       "external DP of any size answers through every member",
			externalDP: true, size: 4, want: modelDeploymentAnsweringAll,
		},
		{
			name:       "external DP of one still answers through its only member",
			externalDP: true, size: 1, want: modelDeploymentAnsweringAll,
		},
		{
			name: "a single member is its own leader", size: 1, want: modelDeploymentAnsweringSole,
		},
		{
			name: "a replica with no members is treated as a sole one",
			size: 0, want: modelDeploymentAnsweringSole,
		},
		{
			name: "a leader-served replica of several answers through its leader",
			size: 3, want: modelDeploymentAnsweringLeader,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDeploymentAnsweringShapeOf(tc.externalDP, tc.size))
		})
	}
}

// TestDrainReasonsNameTheShape is the acceptance the gate added: an operator reading a held
// reservation has to be able to tell which shape produced the selection, and therefore whether the
// replica is waiting on its leader or on a member nobody can identify.
func TestDrainReasonsNameTheShape(t *testing.T) {
	followerOnly := shapeMember(1, 1, "m-1", shapePlainCommand...)
	other := shapeMember(1, 2, "m-2", shapePlainCommand...)

	_, reason := modelDeploymentAnsweringMembers(
		modelDeploymentAnsweringLeader, []*core.Pod{&followerOnly, &other})

	assert.True(t, strings.Contains(reason, "leader-served"),
		"the reason names the shape, not just the symptom: %q", reason)
}
