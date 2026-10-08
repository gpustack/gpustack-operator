package worker

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

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

// shapeSizedMember is a member whose group total is written explicitly, for the cases where the
// replica was NOT rendered at the size the role now declares. planFor leaves a total it has already
// stamped alone, so this is how a case says what the replica was actually built at.
func shapeSizedMember(ordinal, member int, uid string, total int, command ...string) core.Pod {
	pod := shapeMember(ordinal, member, uid, command...)
	pod.Annotations[kueuepodconst.GroupTotalCountAnnotation] = strconv.Itoa(total)

	return pod
}

// shapeNoEngineMember is a member with no engine container at all, which is what a Pod this operator
// never rendered looks like. The retirement guards hold on one rather than classify the replica from
// its siblings.
func shapeNoEngineMember(ordinal int, uid string, total int) core.Pod {
	pod := healthPod("server", ordinal, 0, healthBool(true), uid)
	pod.Spec.Containers = nil
	pod.Annotations = map[string]string{kueuepodconst.GroupTotalCountAnnotation: strconv.Itoa(total)}

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

	// THE GROUP TOTAL IS STAMPED FROM THE ROLE'S OWN SIZE, which is what every case below means by
	// its members: these replicas were rendered from this deployment at this size. A member carrying
	// seat one of a group declaring one is a shape the renderer never emits, and a fixture that
	// produced one would be testing a group this operator could not classify rather than the drain.
	for i := range pods {
		if _, ok := pods[i].Annotations[kueuepodconst.GroupTotalCountAnnotation]; ok {
			continue
		}
		total := 1
		for r := range md.Spec.Roles {
			if pods[i].Labels[modelDeploymentLabelKeyComponent] == md.Spec.Roles[r].Name {
				total = modelDeploymentRoleSize(&md.Spec.Roles[r])
			}
		}
		if pods[i].Annotations == nil {
			pods[i].Annotations = map[string]string{}
		}
		pods[i].Annotations[kueuepodconst.GroupTotalCountAnnotation] = strconv.Itoa(total)
	}

	uids := make([]string, 0, len(pods))
	for i := range pods {
		uids = append(uids, string(pods[i].UID))
	}
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, uids))
	// The reconciler patches THIS object when it writes the release record, and that write is a
	// real optimistic lock, so it compares against a resource version. A server always hands one
	// out; a fixture that seeds the object but leaves the caller's copy without it describes a
	// state no server produces, and the lock refuses it before it reaches the client.
	md = withResourceVersion(md)
	// The supplied pods are SEEDED, not just handed to the planner. The release capture reads the
	// target's members back from the server before it will authorize a delete, because a member that
	// is not observable cannot be said to have held anything. A fixture that passes pods to the
	// planner without creating them is exercising a target the capture cannot see, which is a
	// fixture gap rather than a behavior to assert.
	seeded := make([]ctrlcli.Object, 0, len(pods)+1)
	seeded = append(seeded, md.DeepCopy())
	seen := map[ctrlcli.ObjectKey]struct{}{}
	for i := range pods {
		key := ctrlcli.ObjectKeyFromObject(&pods[i])
		// Seeded once per object. A case whose members deliberately collapse to one name is
		// asserting that they do, and the server cannot hold two objects under one name.
		if _, already := seen[key]; already {
			continue
		}
		seen[key] = struct{}{}
		seeded = append(seeded, pods[i].DeepCopy())
	}
	cli := newModelDeploymentClient(seeded...)
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
				shapeSizedMember(1, 0, "m-0", 2, shapePlainCommand...),
				shapeSizedMember(1, 1, "m-1", 2, shapePlainCommand...),
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
			// THE MEMBERS ANSWER EVEN WITH NO ROLE TO ASK. The role is the one thing this protocol
			// tolerates missing, and letting its absence decide the guard would have drained a
			// disaggregated replica on the strength of a role that no longer exists.
			name: "an External-DP replica whose role is gone is still refused",
			md:   shapeAbsentRole(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapeExternalDPCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "a disaggregated replica has no verifiable decoder-side release",
		},
		{
			// The refusal comes before any read, so the busy member never gets its turn. The busy
			// member path stays covered above on leader-served replicas, where it is reachable.
			name: "an External-DP replica holds before reading a busy member",
			md:   shapeAbsentRole(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapeExternalDPCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			busy:       map[types.UID]bool{"m-1": true},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "a disaggregated replica has no verifiable decoder-side release",
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
			// A GROUP WITH NO MEMBER ON SEAT ZERO HAS ITS SEATS OUT OF ITS OWN RANGE, and the
			// deployed-shape reader says so. It also means the leader search's own "no member carries
			// the leader index" branch is unreachable from a readable shape: seats are unique and in
			// range, so a group of N always has a seat zero. That branch stays as defence behind the
			// shape check rather than as a route a case can reach.
			name: "a replica whose seats are all outside its declared total holds",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 1, "m-1", shapePlainCommand...),
				shapeMember(1, 2, "m-2", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "claims seat 2 of a replica declaring only 2 members",
		},
		{
			// A MULTI-MEMBER GROUP WITH NO SEATS IS NOT A LEGACY REPLICA. Replicas written before the
			// member-index label existed were exactly the single-member ones, so this is a shape the
			// renderer never emits and the reader refuses it rather than counting its members.
			name: "a multi-member replica whose members carry no member index holds",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeUnlabelledMember(1, "m-0", shapePlainCommand...),
				shapeUnlabelledMember(1, "m-1", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "no member declares which seat it holds",
		},
		{
			// TWO MEMBERS ON ONE SEAT IS A DUPLICATE, so the reader that validates seats answers before
			// the leader search that counts them. Both refuse the group; the seat reader names the
			// smaller fact, and two members on seat zero cannot happen in a readable group at all.
			name: "a replica with two members on the leader seat holds",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 0, "m-0b", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "both claim seat 0",
		},
		{
			// THE COUNTEREXAMPLE FOR THE DEPLOYED-SHAPE CHECK. Three members declaring a group of
			// three, with two of them claiming seat one. There is exactly one member carrying the
			// leader index, so the leader search finds a leader, names it and lets the drain run --
			// over a group that cannot be told apart from itself, which is the shape every other
			// reader here refuses to classify.
			name: "a three-member group whose seats duplicate holds",
			md:   shapeDeployment(3),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 1, "m-1", shapePlainCommand...),
				shapeMember(1, 1, "m-1b", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "both claim seat 1",
		},
		{
			// THE SAME HOLE FROM THE OTHER SIDE: every seat is distinct, so nothing duplicates, and
			// the member holding seat ninety-nine is outside a group of three. The leader search
			// still finds seat zero and never looks at the other two.
			name: "a three-member group with a seat outside its declared total holds",
			md:   shapeDeployment(3),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapePlainCommand...),
				shapeMember(1, 1, "m-1", shapePlainCommand...),
				shapeMember(1, 99, "m-99", shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "claims seat 99",
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
			// THE SIZE FREEZE THIS TASK REMOVES. The role now asks for three members; the replica
			// standing was rendered at one and holds its one. Comparing against the role made every
			// old replica of an edited role undeclassifiable, and an undeclassifiable replica is one
			// this operator never drains -- so a size edit would have pinned the old shape in place
			// for as long as the edit existed.
			name:      "a replica built smaller than the role now declares still drains",
			md:        shapeDeployment(3),
			pods:      []core.Pod{shapeSizedMember(1, 0, "m-0", 1, shapePlainCommand...)},
			wantState: workercore.ModelDeploymentRetirementStateDeleting,
			wantReads: map[string]int{"m-0": 2},
		},
		{
			// The genuine fault, still a refusal: the group says it was built at three and is not.
			name:       "a replica short of the total its own group declared holds",
			md:         shapeDeployment(3),
			pods:       []core.Pod{shapeSizedMember(1, 0, "m-0", 3, shapePlainCommand...)},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "short of the group it was built at",
		},
		{
			// THE UNSUPPORTED OLD SHAPE, REACHED WITH THE ROLE DISAGREEING. A leader-served role
			// above members whose own command is disaggregated is a group the prefiller queues do not
			// measure. Reading the role instead of the members drained it on the replacement's
			// description.
			name: "a leader-served role over disaggregated members is refused on their command",
			md:   shapeDeployment(2),
			pods: []core.Pod{
				shapeMember(1, 0, "m-0", shapeExternalDPCommand...),
				shapeMember(1, 1, "m-1", shapeExternalDPCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "a disaggregated replica has no verifiable decoder-side release",
		},
		{
			// THE MISSING-ENGINE CONTROL WITH A CHANGED DESIRED SPEC. The role now declares the
			// disaggregated flag, so every reading about the replacement says the group is
			// disaggregated and the guard has a tempting answer available. The members still carry no
			// engine container, so this operator has not read what is running and must not delete on
			// the replacement's description.
			name: "a member with no engine container holds even when the role declares the shape",
			md:   shapeDeployment(1, "--data-parallel-external-lb"),
			pods: []core.Pod{
				healthPod("server", 1, 0, healthBool(true), "m-0"),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "could not be established",
		},
		{
			name: "a replica with no role and no readable command cannot be classified",
			md:   shapeAbsentRole(2),
			pods: []core.Pod{
				shapeNoEngineMember(1, "m-0", 2),
				shapeSizedMember(1, 1, "m-1", 2, shapePlainCommand...),
			},
			wantState:  workercore.ModelDeploymentRetirementStateDraining,
			wantReads:  map[string]int{},
			wantReason: "carries no engine container to read",
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
