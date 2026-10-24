package worker

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// THIS FILE IS THE CONTRACT BETWEEN THE OPERATOR'S FOUR MOVING PARTS: the qualification that
// judges a group, the collector that watches the Router, the label writer that marks the members,
// and the status that publishes the answer.
//
// EVERY CASE BELOW DRIVES THE PRODUCTION CODE. The pass is a real reconcile, so the qualification
// is computed by the production predicate from the Pods' own readiness and from the probe's own
// answer; the collection is made by the production collector through the Router seam; the labels
// are written by the production writer; and the status is the one the pass published.
//
// A SEEDED VERDICT IS NOT USED. modelDeploymentTestQualification builds a qualification by hand,
// so a case that used it would prove the shape of the rule rather than the rule: a readiness or
// probe outcome the real qualification would refuse would never be refused, because nothing
// computes it.
//
// THE SELECTED POD SET IS WHAT IS ASSERTED. The selector is not the outcome: a selector that named
// nothing at all would also select nothing, and it is the wrong surface. A protected surface keeps
// its eligibility requirement from the first enable and through a withdrawal, and the group is
// excluded because no Pod carries the matching label, so every case reads the members the
// requirement actually admits.

// contractFixture is a Router-bearing deployment whose group is standing, with the pieces a
// contract case needs to drive the real collector, the real label writer and the real status.
//
// IT IS THE SHAPE OF modelDeploymentGroupFixture WITH A CONFIGURED MEMBER COUNT, because a case
// about a PARTIAL RECOVERY needs a group with more members than a fault and a recovery: a
// two-member group has only the fault and the whole.
type contractFixture struct {
	md      *workercore.ModelDeployment
	cli     ctrlcli.Client
	members func() []*corev1.Pod
	serving *[]string
	fail    *bool
	now     *time.Time
	fetch   routerViewFetch
}

// newContractFixture stands up a group of the given size and a Router process the collection can
// place. The group answers as an External DP group, where every rank answers, or as an ordinary
// one, where only the leader does.
func newContractFixture(t *testing.T, size int32, externalDP bool) *contractFixture {
	t.Helper()

	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = size
		if externalDP {
			md.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
		}
	})
	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	endpointEligibilityTestSeed(t, cli, md)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	setEndpointEligibilityPodsReady(t, cli, true)

	stored := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name}, stored))
	stored.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	require.NoError(t, cli.Update(context.Background(), stored))
	md.Spec.Router = stored.Spec.Router
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	seedRouterProcess(t, cli, md, "router-zero", "10.0.9.1")
	seedRouterProcess(t, cli, md, "router-one", "10.0.9.2")

	fail := false
	fixture := &contractFixture{md: md, cli: cli, serving: &[]string{}, fail: &fail}
	fixture.members = func() []*corev1.Pod {
		live := replicaPods(t, cli)
		owned := make([]*corev1.Pod, 0, len(live))
		for i := range live {
			owned = append(owned, &live[i])
		}

		return owned
	}
	for i, pod := range fixture.members() {
		pod.Status.PodIP = "10.0.0." + strconv.Itoa(1+i)
		pod.Status.Phase = corev1.PodRunning
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}
	now := time.Now()
	fixture.now = &now
	fixture.fetch = func(_ context.Context, _ string) ([]byte, error) {
		if *fixture.fail {
			return nil, errors.New("the router did not answer")
		}
		live := fixture.members()
		values := make([]corev1.Pod, 0, len(live))
		for _, pod := range live {
			values = append(values, *pod)
		}

		return retirementRouterView(*fixture.serving, values), nil
	}

	return fixture
}

// generation is the membership generation of the group's current members.
func (f *contractFixture) generation() modelDeploymentMembershipGeneration {
	members := f.members()
	uids := make([]string, 0, len(members))
	for _, pod := range members {
		uids = append(uids, string(pod.UID))
	}
	slices.Sort(uids)

	return modelDeploymentMembershipGeneration(strings.Join(uids, "/"))
}

// answering is the set of members the Router reports as serving.
func (f *contractFixture) answering(externalDP bool) []string {
	ids := []string{}
	for _, pod := range f.members() {
		if externalDP || pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
			ids = append(ids, string(pod.UID))
		}
	}

	return ids
}

// member is the group's member carrying the given member index.
func (f *contractFixture) member(index string) *corev1.Pod {
	for _, pod := range f.members() {
		if pod.Labels[modelDeploymentMemberIndexLabel] == index {
			return pod
		}
	}

	return nil
}

// contractPass runs one real pass over the fixture: the production qualification with a bound
// group-forward probe, the production collector through the fixture's Router seam, the production
// label writer and the production status.
func (f *contractFixture) contractPass(t *testing.T) {
	t.Helper()

	f.pass(t, boundProbeFetch)
}

// contractPassUnprobed runs one real pass whose group-forward probe cannot be answered, so the
// group is never qualified. It is the first-enable case: a group the operator has not judged.
func (f *contractFixture) contractPassUnprobed(t *testing.T) {
	t.Helper()

	f.pass(t, func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		return nil, errors.New("the engine did not answer")
	})
}

// pass runs one real pass with the given group-forward probe.
func (f *contractFixture) pass(t *testing.T, probe modelDeploymentGroupForwardFetch) {
	t.Helper()

	r := &ModelDeploymentReconciler{
		Client: f.cli, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		clock:             func() time.Time { return *f.now },
		groupForwardFetch: probe,
		servingViewFetch:  f.fetch,
	}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
		NamespacedName: ctrlcli.ObjectKey{Namespace: f.md.Namespace, Name: f.md.Name},
	})
	require.NoError(t, err)
}

// makeMembersBindable puts a member in the state the collector can bind it by: an address and a
// running phase.
//
// BOTH ARE NEEDED, AND NEITHER IS ABOUT QUALIFICATION. The group-forward probe has to ask a
// member's engine, so the member needs an address; and the collector binds an observed address to
// the live Pod it names, so the member has to be running. A member that is neither is not a
// qualified member and not a bindable one, and a case about either would be measuring the
// fixture rather than the operator.
func (f *contractFixture) makeMembersBindable(t *testing.T) {
	t.Helper()

	for i, pod := range f.members() {
		if pod.Status.PodIP != "" && pod.Status.Phase == corev1.PodRunning {
			continue
		}
		if pod.Status.PodIP == "" {
			pod.Status.PodIP = "10.0.0." + strconv.Itoa(1+i)
		}
		pod.Status.Phase = corev1.PodRunning
		require.NoError(t, f.cli.Status().Update(context.Background(), pod))
	}
}

// setMemberNotReady reports one member as not ready, which is a definite fault: the operator has
// been told the member is not ready, so the group is withdrawn rather than merely awaited.
func (f *contractFixture) setMemberNotReady(t *testing.T, index string) {
	t.Helper()

	pod := f.member(index)
	require.NotNil(t, pod, "the member %s stands", index)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	require.NoError(t, f.cli.Status().Update(context.Background(), pod))
}

// clearMemberReadiness erases one member's readiness report, which is not a fault: the operator
// has not been told anything about the member, so the group awaits its readiness.
func (f *contractFixture) clearMemberReadiness(t *testing.T, index string) {
	t.Helper()

	pod := f.member(index)
	require.NotNil(t, pod, "the member %s stands", index)
	pod.Status.Conditions = nil
	require.NoError(t, f.cli.Status().Update(context.Background(), pod))
}

// setMemberReady reports one member as ready.
func (f *contractFixture) setMemberReady(t *testing.T, index string) {
	t.Helper()

	pod := f.member(index)
	require.NotNil(t, pod, "the member %s stands", index)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, f.cli.Status().Update(context.Background(), pod))
}

// setAllMembersReady sets every member's readiness.
func (f *contractFixture) setAllMembersReady(t *testing.T, ready bool) {
	t.Helper()

	for _, pod := range f.members() {
		if ready {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		} else {
			pod.Status.Conditions = nil
		}
		require.NoError(t, f.cli.Status().Update(context.Background(), pod))
	}
}

// addReplica gives the role a second replica, so the deployment stands two groups and a fault in
// one of them can be shown not to touch the other.
func (f *contractFixture) addReplica(t *testing.T) {
	t.Helper()

	stored := new(workercore.ModelDeployment)
	require.NoError(t, f.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: f.md.Namespace, Name: f.md.Name}, stored))
	stored.Spec.Roles[0].Replicas = 2
	require.NoError(t, f.cli.Update(context.Background(), stored))
	f.md.Spec.Roles[0].Replicas = 2
}

// replaceMember puts a new member in a member's place, so the membership generation moves.
func (f *contractFixture) replaceMember(t *testing.T, index string) {
	t.Helper()

	old := f.member(index)
	require.NotNil(t, old)
	replacement := old.DeepCopy()
	replacement.UID = types.UID("replaced-" + index + "-" + old.Name)
	replacement.Name = old.Name + "-replaced"
	replacement.ResourceVersion = ""
	require.NoError(t, f.cli.Delete(context.Background(), old))
	require.NoError(t, f.cli.Create(context.Background(), replacement))
}

// group is one group's members, addressed by their ordinal.
func (f *contractFixture) group(t *testing.T, ordinal int) []*corev1.Pod {
	t.Helper()

	name := modelDeploymentReplicaGroupName(f.md, f.md.Spec.Roles[0].Name, ordinal)
	members := []*corev1.Pod{}
	for _, pod := range f.members() {
		if pod.Labels[kueuepodconst.GroupNameLabel] == name {
			members = append(members, pod)
		}
	}
	require.NotEmpty(t, members, "the group at ordinal %d stands", ordinal)

	return members
}

// groupLeader is the leader of one group: its member index zero.
func (f *contractFixture) groupLeader(t *testing.T, ordinal int) *corev1.Pod {
	t.Helper()

	for _, pod := range f.group(t, ordinal) {
		if pod.Labels[modelDeploymentMemberIndexLabel] == "0" {
			return pod
		}
	}
	require.Fail(t, "the group at ordinal %d has no leader", ordinal)

	return nil
}

// surfaces are the ClusterIP surfaces that front the leaders and carry the eligibility
// requirement: the deployment's own and the role's.
//
// BOTH CARRY IT, and both must admit the same members, because a request that arrives at either of
// them has to find the same group. A surface that named a different set would send part of the
// traffic somewhere the operator did not qualify.
func (f *contractFixture) surfaces(t *testing.T) []*corev1.Service {
	t.Helper()

	list := new(corev1.ServiceList)
	require.NoError(t, f.cli.List(context.Background(), list, ctrlcli.InNamespace(f.md.Namespace)))
	surfaces := []*corev1.Service{}
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue
		}
		if svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible] == "" {
			continue
		}
		surfaces = append(surfaces, svc)
	}
	require.NotEmpty(t, surfaces, "the deployment fronts a surface with the eligibility requirement")

	return surfaces
}

// selected is the set of members the rendered requirement actually admits.
//
// THE REQUIREMENT IS THE SURFACE'S OWN SELECTOR, and the selector already carries the native member
// filter beside the eligibility term, so matching the live labels against it IS the intersection of
// the rendered selector and the native filter.
func (f *contractFixture) selected(t *testing.T, selector map[string]string) []string {
	t.Helper()

	requirement := labels.SelectorFromSet(selector)
	selected := []string{}
	for _, pod := range f.members() {
		if requirement.Matches(labels.Set(pod.Labels)) {
			selected = append(selected, string(pod.UID))
		}
	}

	return selected
}

// selectedBySurfaces is the set of members the surfaces admit. Every surface that carries the
// requirement must admit the same members.
func (f *contractFixture) selectedBySurfaces(t *testing.T) []string {
	t.Helper()

	surfaces := f.surfaces(t)
	selected := f.selected(t, surfaces[0].Spec.Selector)
	for _, svc := range surfaces[1:] {
		assert.Equal(t, selected, f.selected(t, svc.Spec.Selector),
			"the surface %s admits the same members", svc.Name)
	}

	return selected
}

// routerSelected is the set of members the published Router discovery admits.
func (f *contractFixture) routerSelected(t *testing.T) []string {
	t.Helper()

	published := new(corev1.ConfigMap)
	require.NoError(t, f.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: f.md.Namespace, Name: f.md.Name + "-router"}, published))
	discovery, err := labels.Parse(published.Data[modelDeploymentRouterSelectorKey])
	require.NoError(t, err)

	selected := []string{}
	for _, pod := range f.members() {
		if discovery.Matches(labels.Set(pod.Labels)) {
			selected = append(selected, string(pod.UID))
		}
	}

	return selected
}

// groups is the published group list of the role.
func (f *contractFixture) groups(t *testing.T) []workercore.ModelDeploymentGroupStatus {
	t.Helper()

	stored := new(workercore.ModelDeployment)
	require.NoError(t, f.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: f.md.Namespace, Name: f.md.Name}, stored))
	require.Len(t, stored.Status.Roles, 1, "the role published one status")

	return stored.Status.Roles[0].Groups
}

// groupStatus is the published entry of one group.
func (f *contractFixture) groupStatus(
	t *testing.T, ordinal int,
) workercore.ModelDeploymentGroupStatus {
	t.Helper()

	for _, group := range f.groups(t) {
		if group.Ordinal == int32(ordinal) {
			return group
		}
	}
	require.Fail(t, "no group at ordinal %d was published", ordinal)

	return workercore.ModelDeploymentGroupStatus{}
}

// TestModelDeploymentGroupContract drives the four parts of the operator against each other.
//
// THE GROUP IS ONE SCHEDULING GROUP. A member reported not ready withdraws the whole group and not
// one member of it; a member whose readiness was never reported leaves the group awaiting its
// readiness, which is not a fault; a partial recovery keeps the group withdrawn; a full recovery
// restores it at the generation it belongs to; a replaced member moves the generation and the old
// answer does not carry over; a Router that cannot be seen holds a new group and leaves an
// unchanged eligible group alone; a coherent empty view is not a partial one; and a group that was
// never qualified is discovered by nobody while an unaffected group keeps serving.
func TestModelDeploymentGroupContract(t *testing.T) {
	t.Run("a member reported not ready withdraws the whole group and not one member of it",
		func(t *testing.T) {
			fixture := newContractFixture(t, 2, false)
			*fixture.serving = fixture.answering(false)
			fixture.setAllMembersReady(t, true)

			// THE GROUP IS SERVING, AND THE REQUIREMENT ADMITS ITS LEADER.
			fixture.contractPass(t)
			leader := fixture.member("0")
			require.NotNil(t, leader)
			assert.Equal(t, modelDeploymentEndpointEligibleValue,
				leader.Labels[modelDeploymentLabelKeyEndpointEligible],
				"the qualified leader carries the eligibility label")
			assert.Equal(t, []string{string(leader.UID)}, fixture.selectedBySurfaces(t),
				"the requirement admits the leader of the qualified group")

			// THE FOLLOWER IS REPORTED NOT READY. The qualification is computed by the production
			// predicate, so the fault is a readiness the real predicate reads, not a verdict a case
			// wrote.
			fixture.setMemberNotReady(t, "1")
			fixture.contractPass(t)

			// NO MEMBER OF THE GROUP CARRIES THE LABEL, AND THE REQUIREMENT ADMITS NOBODY.
			for _, pod := range fixture.group(t, 0) {
				assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
					"a faulted group does not mark its members")
			}
			for _, svc := range fixture.surfaces(t) {
				assert.Equal(t, modelDeploymentEndpointEligibleValue,
					svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
					"the requirement is still on %s: the group is excluded by the labels, not by the selector",
					svc.Name)
			}
			assert.Empty(t, fixture.selectedBySurfaces(t),
				"no member of the faulted group is selected")
			assert.Empty(t, fixture.routerSelected(t),
				"and the Router's discovery admits no member of it")

			// THE STATUS SAYS SO, AND NAMES THE LEG THAT HOLDS IT.
			group := fixture.groupStatus(t, 0)
			assert.Equal(t, workercore.ModelDeploymentGroupStateWithdrawn, group.State,
				"a member reported not ready is a fault, and the group is withdrawn")
			assert.NotEmpty(t, group.Reason, "and the status names the fact that holds it")

			// A WITHDRAWN GROUP IS NOT RESTORED BY AN OBSERVATION THE OPERATOR COULD NOT MAKE.
			*fixture.fail = true
			fixture.contractPass(t)
			assert.Empty(t, fixture.selectedBySurfaces(t),
				"a withdrawn group is discovered by nobody, whatever the Router says")
			assert.Equal(t, workercore.ModelDeploymentGroupStateWithdrawn,
				fixture.groupStatus(t, 0).State,
				"and the status still says the group is withdrawn")
		})

	t.Run("a member whose readiness was never reported leaves the group awaiting its readiness",
		func(t *testing.T) {
			fixture := newContractFixture(t, 2, false)
			*fixture.serving = fixture.answering(false)
			fixture.setAllMembersReady(t, true)
			fixture.contractPass(t)

			// THE FOLLOWER'S READINESS IS NEVER REPORTED, WHICH IS NOT A FAULT.
			fixture.clearMemberReadiness(t, "1")
			fixture.contractPass(t)

			assert.Empty(t, fixture.selectedBySurfaces(t),
				"a group the operator has not judged is discovered by nobody")
			group := fixture.groupStatus(t, 0)
			assert.Equal(t, workercore.ModelDeploymentGroupStateAwaitingReady, group.State,
				"and the status awaits the readiness rather than claiming a fault")
		})

	t.Run("a partial recovery keeps the group withdrawn and a full recovery restores it",
		func(t *testing.T) {
			// THREE MEMBERS, SO A RECOVERY CAN BE PARTIAL: ONE OF THE FAULTED MEMBERS COMES BACK
			// WHILE THE OTHER DOES NOT.
			fixture := newContractFixture(t, 3, false)
			*fixture.serving = fixture.answering(false)
			fixture.setAllMembersReady(t, true)
			fixture.contractPass(t)

			// TWO OF THE THREE MEMBERS ARE REPORTED NOT READY.
			fixture.setMemberNotReady(t, "1")
			fixture.setMemberNotReady(t, "2")
			fixture.contractPass(t)
			assert.Empty(t, fixture.selectedBySurfaces(t),
				"the faulted group is withdrawn")
			assert.Equal(t, workercore.ModelDeploymentGroupStateWithdrawn,
				fixture.groupStatus(t, 0).State, "and the status says the group is withdrawn")

			// ONE OF THEM COMES BACK, AND THE GROUP IS STILL NOT WHOLE.
			fixture.setMemberReady(t, "1")
			fixture.contractPass(t)
			assert.Empty(t, fixture.selectedBySurfaces(t),
				"a partial recovery does not restore the group")
			assert.Equal(t, workercore.ModelDeploymentGroupStateWithdrawn,
				fixture.groupStatus(t, 0).State,
				"and the status says the group is still withdrawn")

			// THE LAST ONE COMES BACK, AND THE GROUP IS WHOLE AGAIN.
			fixture.setMemberReady(t, "2")
			fixture.contractPass(t)
			leader := fixture.member("0")
			require.NotNil(t, leader)
			assert.Equal(t, modelDeploymentEndpointEligibleValue,
				leader.Labels[modelDeploymentLabelKeyEndpointEligible],
				"the fully recovered group is marked again")
			assert.Equal(t, []string{string(leader.UID)}, fixture.selectedBySurfaces(t),
				"and the requirement admits it again")
			assert.Equal(t, workercore.ModelDeploymentGroupStateEligible,
				fixture.groupStatus(t, 0).State, "and the status says the group qualifies")
		})

	t.Run("a replaced member moves the generation and the old answer does not carry over",
		func(t *testing.T) {
			fixture := newContractFixture(t, 2, false)
			*fixture.serving = fixture.answering(false)
			fixture.setAllMembersReady(t, true)
			fixture.contractPass(t)

			leader := fixture.member("0")
			require.NotNil(t, leader)
			first := leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration]
			require.NotEmpty(t, first, "the qualified member names the generation it belongs to")

			// THE FOLLOWER IS REPLACED, SO THE MEMBERSHIP GENERATION MOVES.
			fixture.replaceMember(t, "1")
			fixture.setAllMembersReady(t, true)
			*fixture.serving = fixture.answering(false)
			second := fixture.generation()
			require.NotEqual(t, first, second, "the membership generation moved")

			// THE ROUTER CANNOT BE SEEN, SO THE GROUP IS HELD ACROSS THE NEW GENERATION.
			*fixture.fail = true
			fixture.contractPass(t)
			leader = fixture.member("0")
			require.NotNil(t, leader)
			assert.NotEqual(t, second, leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
				"the held group does not acquire the new generation: the old answer does not carry over")
			assert.NotContains(t, leader.Labels, modelDeploymentLabelKeyEndpointEligible,
				"and the group is not restored by a pass that could not see it")

			// A LATER VALID OBSERVATION RESTORES THE GROUP AT THE NEW GENERATION.
			*fixture.fail = false
			*fixture.serving = fixture.answering(false)
			fixture.contractPass(t)
			leader = fixture.member("0")
			require.NotNil(t, leader)
			assert.Equal(t, string(second),
				leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
				"the restored group names the generation it belongs to")
			group := fixture.groupStatus(t, 0)
			assert.Equal(t, string(second), group.Generation,
				"and the published group names the membership its answers were taken against")
		})

	t.Run("a group that was never qualified is discovered by nobody", func(t *testing.T) {
		fixture := newContractFixture(t, 2, false)
		*fixture.serving = fixture.answering(false)
		fixture.setAllMembersReady(t, true)

		// THE PROBE CANNOT BE ANSWERED, SO THE GROUP IS NEVER QUALIFIED.
		fixture.contractPassUnprobed(t)
		for _, svc := range fixture.surfaces(t) {
			assert.Equal(t, modelDeploymentEndpointEligibleValue,
				svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible],
				"the requirement is on %s from the first enable", svc.Name)
		}
		assert.Empty(t, fixture.selectedBySurfaces(t),
			"but no member of an unqualified group is selected")

		// THE PUBLISHED STATE SAYS THE FORWARD PATH IS WHAT IS MISSING, and not the
		// observation: the Router answered this pass, and the group is held on the probe. This
		// is the state the group is really in, measured rather than assumed, and it is a
		// different state from the one the second half of this case publishes.
		group := fixture.groupStatus(t, 0)
		assert.Equal(t, workercore.ModelDeploymentGroupStateAwaitingProbe, group.State,
			"the status awaits the forward path")
		assert.NotEmpty(t, group.Reason, "and names the probe that did not answer")

		// THE ROUTER STOPS ANSWERING AS WELL, AND THE GROUP IS STILL NOT DISCOVERED. The probe
		// answers this time, so the group is qualified, and the observation is the only thing
		// missing: the status says so by name rather than merely declining to say it qualifies.
		*fixture.fail = true
		fixture.contractPass(t)
		assert.Empty(t, fixture.selectedBySurfaces(t),
			"a group the Router cannot be seen is discovered by nobody")
		group = fixture.groupStatus(t, 0)
		assert.Equal(t, workercore.ModelDeploymentGroupStateAwaitingObservation, group.State,
			"and the status waits on the observation")
		assert.NotEmpty(t, group.Reason, "and names the fact that holds it")
		assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
			"and the serving answer is Unknown")
		assert.NotEmpty(t, group.ServingReason, "and carries the Router's reason")
	})

	t.Run("a new group with ready members and a verified probe receives no valid Router observation",
		func(t *testing.T) {
			fixture := newContractFixture(t, 2, false)
			*fixture.serving = fixture.answering(false)
			fixture.setAllMembersReady(t, true)

			// THE ROUTER CANNOT BE SEEN FROM THE FIRST PASS, SO THE GROUP NEVER RECEIVES A VALID
			// OBSERVATION. The members are all ready and the probe answers, so the group is
			// qualified: what is missing is the observation and nothing else.
			*fixture.fail = true
			fixture.contractPass(t)

			// THE GROUP IS ADMITTED BY NOBODY.
			assert.Empty(t, fixture.selectedBySurfaces(t),
				"a group with no valid Router observation is discovered by nobody")

			// AND IT CARRIES NO ELIGIBILITY PROOF OF THE CURRENT GENERATION. The label and the
			// generation annotation are one fact, so they are read together: a member that is not
			// eligible carries neither, and a member that carries an annotation of an earlier
			// membership does not carry a proof of this one.
			generation := string(fixture.generation())
			for _, pod := range fixture.group(t, 0) {
				assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
					"a member of an unobserved group carries no eligibility label")
				assert.NotEqual(t, generation,
					pod.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
					"and no eligibility proof under the current generation")
			}

			// THE STATUS SAYS THE GROUP IS WAITING ON AN OBSERVATION, AND SAYS WHY.
			group := fixture.groupStatus(t, 0)
			assert.Equal(t, workercore.ModelDeploymentGroupStateAwaitingObservation, group.State,
				"a group every leg verified and no observation reached is waiting on the observation")
			assert.NotEmpty(t, group.Reason, "and the status names what holds it")
			assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
				"and the serving answer is Unknown")
			assert.NotEmpty(t, group.ServingReason, "and it carries the observation reason")

			// THE OBSERVATION IS THE WHOLE BARRIER: the moment the Router answers, the same
			// group qualifies and is selected, with no change to its members or its probe.
			*fixture.fail = false
			fixture.contractPass(t)
			assert.Equal(t, workercore.ModelDeploymentGroupStateEligible,
				fixture.groupStatus(t, 0).State, "a valid observation releases the group")
			assert.NotEmpty(t, fixture.selectedBySurfaces(t), "and it is selected")
		})

	t.Run("a withdrawn group that recovers its members and probe still receives no valid Router observation",
		func(t *testing.T) {
			fixture := newContractFixture(t, 2, false)
			*fixture.serving = fixture.answering(false)
			fixture.setAllMembersReady(t, true)
			fixture.contractPass(t)

			// THE GROUP IS SERVING, AND A RANK FAULTS.
			fixture.setMemberNotReady(t, "1")
			fixture.contractPass(t)
			require.Equal(t, workercore.ModelDeploymentGroupStateWithdrawn,
				fixture.groupStatus(t, 0).State, "the fault withdraws the group")

			// THE MEMBERS AND THE PROBE COME BACK, AND THE ROUTER STILL CANNOT BE SEEN. The
			// recovery is complete and the observation is absent, which is the case the probe
			// barrier and the observation barrier must not be confused with.
			fixture.setMemberReady(t, "1")
			*fixture.fail = true
			fixture.contractPass(t)

			// THE GROUP IS ADMITTED BY NOBODY, AGAIN.
			assert.Empty(t, fixture.selectedBySurfaces(t),
				"a recovered group with no valid Router observation is discovered by nobody")

			// AND IT CARRIES NO ELIGIBILITY PROOF OF THE CURRENT GENERATION.
			generation := string(fixture.generation())
			for _, pod := range fixture.group(t, 0) {
				assert.NotContains(t, pod.Labels, modelDeploymentLabelKeyEndpointEligible,
					"a member of an unobserved group carries no eligibility label")
				assert.NotEqual(t, generation,
					pod.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
					"and no eligibility proof under the current generation")
			}

			// AND THE STATUS NAMES THE OBSERVATION, NOT THE FAULT THAT WAS THERE BEFORE.
			group := fixture.groupStatus(t, 0)
			assert.Equal(t, workercore.ModelDeploymentGroupStateAwaitingObservation, group.State,
				"a recovered group with no valid observation is waiting on the observation")
			assert.NotEmpty(t, group.Reason, "and the status names what holds it")
			assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
				"and the serving answer is Unknown")
			assert.NotEmpty(t, group.ServingReason, "and it carries the observation reason")
		})

	t.Run("an unchanged eligible group is retained across an observation loss", func(t *testing.T) {
		fixture := newContractFixture(t, 2, false)
		*fixture.serving = fixture.answering(false)
		fixture.setAllMembersReady(t, true)
		fixture.contractPass(t)

		leader := fixture.member("0")
		require.NotNil(t, leader)
		require.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible],
			"the group qualifies before the observation is lost")

		// THE ROUTER STOPS ANSWERING, AND THE GROUP IS UNCHANGED.
		*fixture.fail = true
		fixture.contractPass(t)

		leader = fixture.member("0")
		require.NotNil(t, leader)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible],
			"an unchanged group keeps its label across an observation loss")
		assert.Equal(t, []string{string(leader.UID)}, fixture.selectedBySurfaces(t),
			"and it is still selected")
		assert.Equal(t, workercore.ModelDeploymentGroupStateEligible,
			fixture.groupStatus(t, 0).State,
			"the observation is not what holds an unchanged group")

		// AND THE SERVING ANSWER IS UNKNOWN, because the confirmation is the part that needs an
		// observation and the eligibility is not. The group keeps serving on what it had, and the
		// published answer says the operator no longer has a fresh one.
		group := fixture.groupStatus(t, 0)
		assert.Equal(t, workercore.ModelDeploymentServingStateUnknown, group.Serving.State,
			"and the serving answer is Unknown while the observation is lost")
		assert.NotEmpty(t, group.ServingReason,
			"and it carries the reason the answer is not fresh")
		assert.Nil(t, group.Serving.Value,
			"and no count is published: a count would be a confirmation, and there is none")

		// THIS IS THE CONTRAST TO THE TWO CASES ABOVE. An unchanged group keeps the proof it
		// earned, and its generation annotation still names the membership that proof was taken
		// against; a new or a recovered group has no such proof to keep, and gains none while the
		// observation is missing.
		assert.Equal(t, string(fixture.generation()),
			leader.Annotations[modelDeploymentAnnotationKeyEndpointEligibleGeneration],
			"and it keeps its eligibility proof under the current generation")
	})

	t.Run("a coherent empty view is not a partial one", func(t *testing.T) {
		fixture := newContractFixture(t, 2, false)
		*fixture.serving = fixture.answering(false)
		fixture.setAllMembersReady(t, true)
		fixture.contractPass(t)

		// THE ROUTER REPORTS THAT NO MEMBER SERVES, AND THAT IS A COHERENT VIEW.
		*fixture.serving = []string{}
		fixture.contractPass(t)
		leader := fixture.member("0")
		require.NotNil(t, leader)
		assert.Equal(t, modelDeploymentEndpointEligibleValue,
			leader.Labels[modelDeploymentLabelKeyEndpointEligible],
			"a coherent empty view does not withdraw a group that qualifies")
	})

	t.Run("a partial view never confirms the group", func(t *testing.T) {
		// EVERY RANK OF AN EXTERNAL DP GROUP ANSWERS, SO A VIEW THAT NAMES ONLY ONE OF THEM IS
		// PARTIAL. An ordinary group has one answering member, and a view naming it is complete.
		fixture := newContractFixture(t, 2, true)
		*fixture.serving = fixture.answering(true)
		fixture.setAllMembersReady(t, true)
		fixture.contractPass(t)

		// THE VIEW NAMES THE LEADER ONLY.
		leader := fixture.member("0")
		require.NotNil(t, leader)
		*fixture.serving = []string{string(leader.UID)}
		fixture.contractPass(t)

		group := fixture.groupStatus(t, 0)
		assert.NotEqual(t, workercore.ModelDeploymentServingStateConfirmed, group.Serving.State,
			"a partial view never confirms the group")
		assert.NotEmpty(t, group.ServingReason,
			"and the published answer says why it is not a confirmation")
	})

	t.Run("an unaffected group keeps serving while another group faults", func(t *testing.T) {
		fixture := newContractFixture(t, 2, false)
		*fixture.serving = fixture.answering(false)
		fixture.setAllMembersReady(t, true)

		// THE SECOND REPLICA'S MEMBERS ARE CREATED BY THE PASS THAT GIVES IT ONE, AND A MEMBER
		// THAT HAS NEVER BEEN REPORTED READY IS NOT A QUALIFIED ONE.
		fixture.addReplica(t)
		fixture.contractPass(t)
		// THE FAKE CLIENT ASSIGNS NO IDENTITY, AND THE HEALTH PREDICATE KEYS EACH MEMBER'S GROUP BY
		// IT, SO A MEMBER WITHOUT ONE CANNOT BE PLACED IN ITS GROUP AT ALL.
		assignEndpointEligibilityUIDs(t, fixture.cli)
		fixture.makeMembersBindable(t)
		fixture.setAllMembersReady(t, true)
		*fixture.serving = fixture.answering(false)
		fixture.contractPass(t)

		// THE LEADERS OF THE TWO GROUPS, BEFORE THE FAULT.
		firstLeader := fixture.groupLeader(t, 0)
		secondLeader := fixture.groupLeader(t, 1)
		selected := fixture.selectedBySurfaces(t)
		assert.Contains(t, selected, string(firstLeader.UID), "the first group's leader is selected")
		assert.Contains(t, selected, string(secondLeader.UID), "the second group's leader is selected")

		// THE FAULT IS IN THE SECOND GROUP, AND THE FIRST ONE IS UNTOUCHED.
		second := fixture.group(t, 1)
		require.Len(t, second, 2, "the second group stands two members")
		for _, pod := range second {
			if pod.Labels[modelDeploymentMemberIndexLabel] == "1" {
				pod.Status.Conditions = []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionFalse,
				}}
				require.NoError(t, fixture.cli.Status().Update(context.Background(), pod))
			}
		}
		fixture.contractPass(t)

		// THE FAULTED GROUP IS WITHDRAWN, AND THE OTHER ONE IS NOT.
		selected = fixture.selectedBySurfaces(t)
		assert.NotContains(t, selected, string(secondLeader.UID),
			"the faulted group is discovered by nobody")
		assert.Contains(t, selected, string(firstLeader.UID),
			"the unaffected group is still selected")
		assert.Equal(t, workercore.ModelDeploymentGroupStateEligible,
			fixture.groupStatus(t, 0).State, "and it still qualifies")
		assert.Equal(t, workercore.ModelDeploymentGroupStateWithdrawn,
			fixture.groupStatus(t, 1).State, "while the other one is withdrawn")
	})

	t.Run("a candidate without an eligibility label is not discovered, in a judged role",
		func(t *testing.T) {
			fixture := newContractFixture(t, 2, false)
			*fixture.serving = fixture.answering(false)
			fixture.setAllMembersReady(t, true)
			fixture.contractPass(t)

			leader := fixture.member("0")
			require.NotNil(t, leader)
			require.Equal(t, modelDeploymentEndpointEligibleValue,
				leader.Labels[modelDeploymentLabelKeyEndpointEligible],
				"the group qualifies")

			// THE CANDIDATE MARKER IS NOT A QUALIFICATION CLAIM.
			delete(leader.Labels, modelDeploymentLabelKeyEndpointEligible)
			require.NoError(t, fixture.cli.Update(context.Background(), leader))

			assert.Empty(t, fixture.selectedBySurfaces(t),
				"a member that carries no eligibility label is not selected")
		})
}
