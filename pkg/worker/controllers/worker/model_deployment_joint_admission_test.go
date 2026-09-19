package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// jointCheckObject is the AdmissionCheck this controller claims. FilterForController reads it to
// decide which of a Workload's checks are this controller's, so a fixture without it leaves the
// controller with nothing to answer and every case passes for the wrong reason.
func jointCheckObject() *kueue.AdmissionCheck {
	ac := new(kueue.AdmissionCheck)
	ac.Name = _JointAdmissionCheckName
	ac.Spec.ControllerName = _JointAdmissionControllerName

	return ac
}

// jointGroupPod is one replica of a group, carrying the three things the controller reads: the group
// it joined, the instance label its replica list is scoped by, and the deployment that owns it.
//
// THE INSTANCE LABEL IS WHAT THE RENDERER PUTS ON A REPLICA, and the controller lists by it rather
// than by namespace. A fixture without it is invisible to that list, which reads as a deployment
// whose groups have no replicas -- a shape that answers Pending to everything and so passes every
// negative case here for the wrong reason.
func jointGroupPod(name, group, mdName string) *core.Pod {
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{
		Name:      name,
		Namespace: "team-a",
		UID:       types.UID("uid-" + name),
		Labels:    map[string]string{kueuepodconst.GroupNameLabel: group},
	}}
	if mdName != "" {
		pod.Labels[modelDeploymentLabelKeyInstance] = mdName
		pod.OwnerReferences = []meta.OwnerReference{{
			APIVersion: workercore.SchemeGroupVersion.String(),
			Kind:       "ModelDeployment",
			Name:       mdName,
			UID:        types.UID("uid-" + mdName),
			Controller: ptrTo(true),
		}}
	}

	return pod
}

func ptrTo[T any](v T) *T { return &v }

// jointWorkload is a Workload owned by the given replicas and carrying this controller's check.
func jointWorkload(name string, reserved bool, pods ...*core.Pod) *kueue.Workload {
	wl := new(kueue.Workload)
	wl.Name, wl.Namespace = name, "team-a"
	for _, pod := range pods {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		})
	}

	status := meta.ConditionFalse
	if reserved {
		status = meta.ConditionTrue
	}
	wl.Status.Conditions = []meta.Condition{{
		Type:               kueue.WorkloadQuotaReserved,
		Status:             status,
		Reason:             "Test",
		LastTransitionTime: meta.Now(),
	}}
	wl.Status.AdmissionChecks = []kueue.AdmissionCheckState{{
		Name:  kueue.AdmissionCheckReference(_JointAdmissionCheckName),
		State: kueue.CheckStatePending,
	}}

	return wl
}

// jointDeployment is a ModelDeployment whose roles sit on the given instance types.
func jointDeployment(name string, instanceTypes ...string) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{
			Name: name, Namespace: "team-a", UID: types.UID("uid-" + name),
		},
	}
	for i, it := range instanceTypes {
		md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
			Name: []string{"prefill", "decode", "third"}[i], Replicas: 1, InstanceType: it,
		})
	}

	return md
}

func reconcileJoint(t *testing.T, cli ctrlcli.Client, wlName string) *kueue.Workload {
	t.Helper()

	r := &ModelDeploymentJointAdmissionReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-a", Name: wlName},
	})
	require.NoError(t, err)

	got := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: wlName}, got))

	return got
}

// jointCheckState is the check's State field, or "" when the Workload does not carry the check.
//
// It reads through the production lookup rather than scanning the slice again. A copy here would be
// a second implementation of the thing these tests are asserting about, so a change to how the entry
// is found could leave every assertion below still green against the old rule.
func jointCheckState(wl *kueue.Workload) kueue.CheckState {
	if acs := jointCheckEntry(wl); acs != nil {
		return acs.State
	}

	return ""
}

func newJointClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&kueue.Workload{}, &kueue.AdmissionCheck{}).
		Build()
}

// TestModelDeploymentJointAdmission_ReadyAtOnceForEverythingElse is the case that must be present or
// the check parks the cluster.
//
// The check is referenced from every operator-owned queue, so every Workload in one carries it --
// including workloads this operator did not create. A controller that answered only about its own
// objects would leave all the others Pending forever, with nothing naming the cause.
func TestModelDeploymentJointAdmission_ReadyAtOnceForEverythingElse(t *testing.T) {
	cases := []struct {
		name string
		objs func() []ctrlcli.Object
	}{
		{
			// A Workload of a Pod this operator does not own: the walk from Workload to Pod finds no
			// ModelDeployment, and the answer has to be Ready rather than a wait for something that
			// will never arrive.
			name: "not_this_operators_workload",
			objs: func() []ctrlcli.Object {
				pod := jointGroupPod("someone-elses", "their-group", "")

				return []ctrlcli.Object{jointCheckObject(), pod, jointWorkload("wl", true, pod)}
			},
		},
		{
			// One role is one group, and Kueue admits a group as a unit without help.
			name: "single_group_deployment",
			objs: func() []ctrlcli.Object {
				md := jointDeployment("qwen", "h20-8x")
				pod := jointGroupPod("qwen-prefill-0", "qwen", "qwen")

				return []ctrlcli.Object{jointCheckObject(), md, pod, jointWorkload("wl", true, pod)}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileJoint(t, newJointClient(tc.objs()...), "wl")

			assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
				"anything that is not a multi-group model deployment is Ready at once")
		})
	}
}

// twoGroupFixture builds a deployment over two instance types, one group's replica and Workload per
// entry, with the second group's reservation controlled by the caller.
func twoGroupFixture(secondReserved bool) []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "a100-8x")
	groups := modelDeploymentPodGroups(md)

	first := jointGroupPod("qwen-prefill-0", groups[0].Name, "qwen")
	second := jointGroupPod("qwen-decode-0", groups[1].Name, "qwen")

	return []ctrlcli.Object{
		jointCheckObject(), md, first, second,
		jointWorkload("wl-first", true, first),
		jointWorkload("wl-second", secondReserved, second),
	}
}

// TestModelDeploymentJointAdmission_TheWholeSetOrNone covers the barrier itself.
//
// BOTH DIRECTIONS ARE REQUIRED. A controller that answered Pending to everything passes the negative
// case, and one that answered Ready to everything passes the positive case; only the pair says the
// barrier is reading anything at all.
func TestModelDeploymentJointAdmission_TheWholeSetOrNone(t *testing.T) {
	t.Run("every_group_reserved_is_ready", func(t *testing.T) {
		got := reconcileJoint(t, newJointClient(twoGroupFixture(true)...), "wl-first")

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got))
	})

	t.Run("one_group_short_holds_the_others", func(t *testing.T) {
		cli := newJointClient(twoGroupFixture(false)...)

		got := reconcileJoint(t, cli, "wl-first")

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"the group that can run waits for the one that cannot")
		assert.NotEqual(t, kueue.CheckStateRetry, jointCheckState(got),
			"a Retry evicts and drops the reservation the barrier is made of")
		assert.Contains(t, jointCheckMessage(got), "decode",
			"the message names the role still waiting, which is what an operator acts on")

		// THE QUOTA IS KEPT WHILE IT WAITS. Pending is a hold, and the reservation is what the other
		// group is waiting to observe; a controller that dropped it here would take the barrier apart
		// in the act of applying it.
		assert.True(t, workloadHasReservation(t, cli, "wl-first"),
			"every role keeps the quota it reserved while the set assembles")
	})
}

// TestJointCheckEntry pins the contract the readers of this check depend on.
//
// THE ENTRY RATHER THAN A BOOL IS THE POINT. Three readers ask three different questions of it --
// presence, how long it has been Pending, and what its message says -- and a lookup that answered
// only "is it there" would leave two of them scanning the slice again, which is the duplication this
// helper exists to remove.
//
// RETURNING A COPY WOULD PASS AN EQUALITY ASSERTION AND STILL BE WRONG, so the found case asserts
// identity by writing through the returned pointer and reading the Workload back. A copy is the
// likely shape of a future rewrite, and nothing else here would notice it.
func TestJointCheckEntry(t *testing.T) {
	joint := kueue.AdmissionCheckState{
		Name:    kueue.AdmissionCheckReference(_JointAdmissionCheckName),
		State:   kueue.CheckStatePending,
		Message: "held",
	}
	other := kueue.AdmissionCheckState{Name: "some-other-check", State: kueue.CheckStateReady}

	testCases := []struct {
		name   string
		checks []kueue.AdmissionCheckState
		want   bool
	}{
		{name: "no_checks_at_all", checks: nil, want: false},
		{name: "only_another_controllers_check", checks: []kueue.AdmissionCheckState{other}, want: false},
		{name: "the_joint_check_alone", checks: []kueue.AdmissionCheckState{joint}, want: true},
		{
			name:   "the_joint_check_beside_another",
			checks: []kueue.AdmissionCheckState{other, joint},
			want:   true,
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			wl := &kueue.Workload{Status: kueue.WorkloadStatus{AdmissionChecks: c.checks}}

			got := jointCheckEntry(wl)
			if !c.want {
				assert.Nil(t, got)
				return
			}

			require.NotNil(t, got)
			assert.Equal(t, kueue.AdmissionCheckReference(_JointAdmissionCheckName), got.Name)

			got.Message = "written through the returned pointer"
			assert.Equal(t, "written through the returned pointer", jointCheckMessage(wl),
				"the entry is the one on the Workload, not a copy of it")
		})
	}
}

// jointCheckMessage is the check's Message field, or "" when the Workload does not carry the check.
// It goes through the production lookup for the same reason jointCheckState does.
func jointCheckMessage(wl *kueue.Workload) string {
	if acs := jointCheckEntry(wl); acs != nil {
		return acs.Message
	}

	return ""
}

func workloadHasReservation(t *testing.T, cli ctrlcli.Client, name string) bool {
	t.Helper()

	wl := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, wl))

	for i := range wl.Status.Conditions {
		c := &wl.Status.Conditions[i]
		if c.Type == kueue.WorkloadQuotaReserved {
			return c.Status == meta.ConditionTrue
		}
	}

	return false
}

// TestModelDeploymentJointAdmission_TheBarrierIsWiredToOpen covers the event that opens the barrier.
//
// A HELD GROUP'S VERDICT IS A STATEMENT ABOUT A DIFFERENT OBJECT. It stays Pending until the last
// sibling reserves quota, and that reservation is written to the sibling's own Workload. A controller
// watching only the object it reconciles never sees it, so the held group is never judged again and
// the barrier that correctly refused a half-feasible set never opens when the set becomes feasible.
//
// THE CLOSING PATH ALONE CANNOT SEE THIS, and neither can a cluster where the groups happen to
// reserve within one reconcile of each other: every group's own event then arrives after the others
// have already reserved. A barrier that never opens looks perfect in every test of it closing.
//
// WHAT THIS DOES NOT COVER IS THE WIRING, and the wiring is where the defect was. These cases call
// the mapping directly; that the controller builder actually registers it is not observable from a
// fake client, so it is asserted in a cluster by the e2e case that holds one group and then makes the
// set feasible. A correct mapping nobody watches with is the state this file cannot distinguish.
func TestModelDeploymentJointAdmission_TheBarrierIsWiredToOpen(t *testing.T) {
	t.Run("a_sibling_reserving_enqueues_the_held_group", func(t *testing.T) {
		cli := newJointClient(twoGroupFixture(true)...)
		r := &ModelDeploymentJointAdmissionReconciler{Client: cli}

		second := new(kueue.Workload)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Namespace: "team-a", Name: "wl-second"}, second))

		reqs := r.jointSiblings(context.Background(), second)

		assert.Equal(t, []ctrl.Request{{
			NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "wl-first"},
		}}, reqs, "the group held by this one is enqueued, and the changed workload is not: its own "+
			"watch already delivers that")
	})

	// THE MAPPING RUNS FOR EVERY WORKLOAD EVENT IN THE CLUSTER, and everything it does reads objects:
	// a Get per owner replica, then a list of the namespace's Workloads. A Workload that does not
	// carry this controller's check cannot be one this barrier holds, and that is answerable from the
	// object already in hand, so it must be answered before any read happens.
	t.Run("a_workload_without_this_check_costs_no_read", func(t *testing.T) {
		// THE FIXTURE IS THE ONE THAT MAPS TO SOMETHING. It is the case above, unchanged except that
		// this Workload carries no check of ours -- so an implementation without the short-circuit
		// resolves the deployment and returns its sibling exactly as it does there. An empty client
		// would have returned nothing either way and proved nothing.
		cli := newJointClient(twoGroupFixture(true)...)
		r := &ModelDeploymentJointAdmissionReconciler{Client: cli}

		second := new(kueue.Workload)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Namespace: "team-a", Name: "wl-second"}, second))
		second.Status.AdmissionChecks = nil

		assert.Empty(t, r.jointSiblings(context.Background(), second),
			"a workload in some other queue is not this barrier's business, and deciding that must "+
				"not cost a read per owner replica on every workload event in the cluster")
	})

	t.Run("a_workload_this_operator_does_not_own_maps_to_nothing", func(t *testing.T) {
		pod := jointGroupPod("someone-elses", "their-group", "")
		wl := jointWorkload("wl", true, pod)
		cli := newJointClient(jointCheckObject(), pod, wl)
		r := &ModelDeploymentJointAdmissionReconciler{Client: cli}

		assert.Empty(t, r.jointSiblings(context.Background(), wl),
			"the check is referenced from every operator-owned queue, so most workloads reaching this "+
				"mapping belong to no deployment of ours")
	})
}

// TestModelDeploymentJointAdmission_UnresolvableIsNotForeign separates the two ways this controller
// can fail to find a deployment behind a Workload.
//
// THE Ready ANSWER EXISTS FOR WORKLOADS THAT ARE NOT OURS, and it has to, because the check is
// referenced from every operator-owned queue: without it every foreign Workload in one waits forever
// with nothing naming the cause. But the walk to the deployment runs Workload to Pod to
// ModelDeployment, and a Pod that cannot be read looks exactly like a Pod that leads nowhere.
//
// A GROUP BEING REBUILT PRODUCES THAT STATE. Its replicas are deleted while its Workload still
// references them, so every Get misses and the walk ends empty -- read as "not ours", that admits a
// set which is mid-teardown, and admission is the one verdict that cannot be taken back.
func TestModelDeploymentJointAdmission_UnresolvableIsNotForeign(t *testing.T) {
	t.Run("no_pod_owner_at_all_is_ready", func(t *testing.T) {
		// A Workload that references no Pod is genuinely none of this operator's, and this is the
		// case the Ready answer exists for.
		wl := jointWorkload("wl", true)
		wl.OwnerReferences = nil
		cli := newJointClient(jointCheckObject(), wl)

		got := reconcileJoint(t, cli, "wl")

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
			"a workload with no pod owner is not one of ours, and holding it would hold the queue")
	})

	t.Run("an_owner_pod_that_cannot_be_read_is_held_and_says_so", func(t *testing.T) {
		// The Pod is referenced and absent, which is what a rebuild leaves behind for a moment.
		pod := jointGroupPod("qwen-prefill-0", "qwen-group", "qwen")
		wl := jointWorkload("wl", true, pod)
		cli := newJointClient(jointCheckObject(), wl)

		// THE STATE ALONE CANNOT CARRY THIS ASSERTION. The fixture arrives Pending already, so
		// asserting Pending after the pass is true whether the controller wrote a verdict or wrote
		// nothing at all -- and writing nothing is the defect. The message is what separates them,
		// which is why it is pinned here rather than merely being non-empty.
		require.Empty(t, jointCheckMessage(wl),
			"the fixture must arrive without a message, or this case cannot see one being written")

		got := reconcileJoint(t, cli, "wl")

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"an unreadable owner replica leaves the answer unknown, and the barrier holds rather "+
				"than opening on a guess")
		assert.Equal(t, _JointAdmissionUndecidedMessage, jointCheckMessage(got),
			"a workload nothing is admitting has to say why, and this is the one verdict this "+
				"controller used to leave blank")
	})

	t.Run("the_undecided_message_is_not_an_ordinary_hold", func(t *testing.T) {
		// BOTH STATES ARE Pending, so the message is the only thing an operator can tell them apart
		// by. A hold names the groups being waited on and clears itself; this one means a read
		// failed and may need acting on.
		//
		// THE HOLD WORDING IS TAKEN FROM A HELD WORKLOAD rather than written out here. Spelling it
		// out would make this case compare two strings this file owns, and it would go on passing
		// after the real hold message changed to something that reads the same as the undecided one.
		cli := newJointClient(twoGroupFixture(false)...)

		held := reconcileJoint(t, cli, "wl-first")
		require.Equal(t, kueue.CheckStatePending, jointCheckState(held),
			"the comparison is only meaningful between two Pending verdicts")

		holdMessage := jointCheckMessage(held)
		require.NotEmpty(t, holdMessage, "an ordinary hold must say something to be compared with")
		assert.NotEqual(t, holdMessage, _JointAdmissionUndecidedMessage,
			"an operator reading the check must be able to tell a set that is still assembling from "+
				"replicas this controller could not read")
	})
}

// TestModelDeploymentJointAdmission_SkipsWhatItMustNotTouch pins the gates.
//
// A Workload without a reservation has nothing to confirm; an admitted one has already passed the
// barrier and re-answering it would be a verdict on a settled placement. Both leave the check exactly
// as they found it, which is what "skip" has to mean here -- a controller that wrote Pending to an
// admitted Workload would evict a running deployment.
func TestModelDeploymentJointAdmission_SkipsWhatItMustNotTouch(t *testing.T) {
	// THE FIXTURE HAS TO MAKE THE TWO PATHS DISAGREE. Both groups are reserved here, so a controller
	// that did not skip would compute Ready; the skip leaves the Pending the Workload arrived with.
	// A fixture where the verdict would also have been Pending -- a Workload short of a reservation,
	// say -- is answered the same way by both, and asserting it proves nothing. Measured: that first
	// version of this case passed against the mutation it exists to catch.
	t.Run("already_admitted", func(t *testing.T) {
		cli := newJointClient(twoGroupFixture(true)...)

		wl := new(kueue.Workload)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Namespace: "team-a", Name: "wl-first"}, wl))
		wl.Status.Conditions = append(wl.Status.Conditions, meta.Condition{
			Type:               kueue.WorkloadAdmitted,
			Status:             meta.ConditionTrue,
			Reason:             "Test",
			LastTransitionTime: meta.Now(),
		})
		require.NoError(t, cli.Status().Update(context.Background(), wl))

		got := reconcileJoint(t, cli, "wl-first")

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"an admitted Workload has already passed the barrier; re-answering it is a verdict on a "+
				"settled placement, and writing one would evict a running deployment")
	})
}

// pendingSince stamps this controller's check as Pending at the given moment, which is the durable
// clock the bound reads.
func pendingSince(t *testing.T, cli ctrlcli.Client, name string, at time.Time) {
	t.Helper()

	wl := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, wl))
	for i := range wl.Status.AdmissionChecks {
		wl.Status.AdmissionChecks[i].LastTransitionTime = meta.NewTime(at)
	}
	require.NoError(t, cli.Status().Update(context.Background(), wl))
}

func reconcileJointAt(t *testing.T, cli ctrlcli.Client, wlName string, at time.Time) *kueue.Workload {
	t.Helper()

	r := &ModelDeploymentJointAdmissionReconciler{Client: cli, Clock: testingclock.NewFakePassiveClock(at)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-a", Name: wlName},
	})
	require.NoError(t, err)

	got := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: wlName}, got))

	return got
}

// TestModelDeploymentJointAdmission_TheBound covers what happens when a set never assembles.
//
// THE CLOCK IS FAKE ON PURPOSE. A test that waited out the real bound is a test nobody runs, and a
// bound nobody runs is a bound nobody has checked.
//
// BOTH SIDES OF THE BOUND ARE REQUIRED. A controller that parked immediately would pass the "after"
// case, and one that never parked would pass the "before" case; only the pair says the bound is being
// read at all.
func TestModelDeploymentJointAdmission_TheBound(t *testing.T) {
	start := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

	t.Run("before_the_bound_it_only_holds", func(t *testing.T) {
		cli := newJointClient(twoGroupFixture(false)...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter-time.Minute))

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got))
		assert.True(t, kueueworkload.IsActive(got),
			"a set that might still assemble is held, not parked")
		assert.NotContains(t, jointCheckMessage(got), "parked")
	})

	t.Run("after_the_bound_it_parks", func(t *testing.T) {
		cli := newJointClient(twoGroupFixture(false)...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		// THE CYCLE STOPS THROUGH DURABLE STATE. A deleted Workload is composed again by Kueue from
		// the very Pods that are still there, so deleting removes one turn of the loop and nothing
		// else; spec.active is read by the scheduler and keeps the group from being rebuilt behind it.
		assert.False(t, kueueworkload.IsActive(got),
			"the workload is deactivated, which is what stops the reserve-and-hold cycle")
		assert.NotEmpty(t, got.Name, "and it still exists: parking is not a delete")

		msg := jointCheckMessage(got)
		assert.Contains(t, msg, "decode", "the message names the group that could not be placed")
		assert.Contains(t, msg, "re-apply",
			"and the action that clears it, because an identical re-apply bumps no resourceVersion, "+
				"delivers no event, and leaves an operator watching nothing happen")

		// THE OTHER HALF OF A CONTRACT THAT SPANS TWO CONTROLLERS. The status layer reports Parked
		// only when this check carries the verdict, because spec.active=false does not say who wrote
		// it: Kueue deactivates a Workload of its own accord, and an operator can pause one group by
		// hand. Asserting the message here and the reader there leaves the join untested, and the
		// join is the part that breaks -- a reworded message would pass both sides separately and
		// report a parked deployment as merely waiting.
		assert.True(t, jointBarrierParked(got),
			"the status layer reads this verdict off the check to tell this barrier's park from "+
				"anyone else's deactivation, and it must find it here")
	})

	t.Run("feasible_before_the_bound_is_admitted_normally", func(t *testing.T) {
		cli := newJointClient(twoGroupFixture(true)...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
			"the bound is about a set that cannot assemble, and this one did")
		assert.True(t, kueueworkload.IsActive(got), "and it is never parked")
	})

	// A DEPLOYMENT SHORT OF ITS OWN REPLICAS IS NOT WHAT THE BOUND IS FOR. The reconciler rebuilds a
	// group by deleting its replicas and creating them again on a later pass, so between the two the
	// group is short of its declared total and Kueue composes no Workload for it -- which reads from
	// the sibling exactly like a group that cannot be placed. A bound that cannot tell them apart
	// parks a deployment that was about to start, and nothing in the cluster sets spec.active back to
	// true, so that park is permanent.
	t.Run("a_deployment_still_assembling_is_never_parked", func(t *testing.T) {
		cli := newJointClient(rebuildingFixture()...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"the group that can run still waits for the one that is being rebuilt")
		assert.True(t, kueueworkload.IsActive(got),
			"and the deployment is held rather than parked, because the wait has an ordinary cause")
		assert.NotContains(t, jointCheckMessage(got), "parked")
		assert.Contains(t, jointCheckMessage(got), "decode",
			"the message names the group still assembling, which is what an operator acts on")
	})

	// THE SAME BOUND, ONE STEP EARLIER, WHERE THE COUNT IS THE ONLY THING THAT CAN TELL. Every
	// replica the group declares exists as an object, so a count over objects reports it assembled;
	// one of them has been asked to go, so a count over members reports it short. The first reading
	// lets the settled bound fire and deactivate a Workload whose rebuild is proceeding normally.
	t.Run("a_group_whose_replica_is_terminating_is_still_assembling", func(t *testing.T) {
		cli := newJointClient(terminatingFixture()...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"the sibling still waits: the group it waits for has not got its members yet")
		assert.True(t, kueueworkload.IsActive(got),
			"and it is held rather than parked -- a replica on its way out is an ordinary cause")
		assert.NotContains(t, jointCheckMessage(got), "parked")
		assert.Contains(t, jointCheckMessage(got), "decode",
			"the message names the group whose replica is leaving")
	})
}

// terminatingFixture is the same rebuild one step earlier, where the group is at its declared total
// ON PAPER: both replicas exist as objects, but one has already been asked to go.
//
// It is the reading the count has to get right. A Pod carrying a deletion timestamp is not a member
// the group will have, so counting it reports the group as assembled for as long as the kubelet
// takes to finish -- and an assembled-looking group is what lets the settled bound fire and park the
// rebuild it exists to protect.
func terminatingFixture() []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "a100-8x")
	md.Spec.Roles[1].Replicas = 2
	groups := modelDeploymentPodGroups(md)

	first := jointGroupPod("qwen-prefill-0", groups[0].Name, "qwen")
	second := jointGroupPod("qwen-decode-0", groups[1].Name, "qwen")
	leaving := jointGroupPod("qwen-decode-1", groups[1].Name, "qwen")
	leaving.DeletionTimestamp = ptr.To(meta.Now())
	leaving.Finalizers = []string{"kueue.x-k8s.io/managed"}

	return []ctrlcli.Object{
		jointCheckObject(), md, first, second, leaving,
		jointWorkload("wl-first", true, first),
	}
}

// rebuildingFixture is a deployment mid-rebuild: the second group declares two replicas, one of them
// exists, and Kueue has therefore composed no Workload for it. That is the state the reconciler
// leaves behind between deleting a group's replicas and creating them again.
func rebuildingFixture() []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "a100-8x")
	md.Spec.Roles[1].Replicas = 2
	groups := modelDeploymentPodGroups(md)

	first := jointGroupPod("qwen-prefill-0", groups[0].Name, "qwen")
	second := jointGroupPod("qwen-decode-0", groups[1].Name, "qwen")

	return []ctrlcli.Object{
		jointCheckObject(), md, first, second,
		jointWorkload("wl-first", true, first),
	}
}

// sameTypePairFixture builds the prefill/decode deployment whose two roles name ONE instance type,
// with one replica and one Workload per group and the second group's reservation controlled by the
// caller.
//
// BOTH GROUPS SCHEDULE INTO THE SAME CLUSTER QUEUE, which is what separates this shape from
// twoGroupFixture: the queue is named after the instance type, so the group holding quota holds it
// in the very queue its sibling waits for, and the two compete for one pool instead of waiting in
// two parallel ones.
func sameTypePairFixture(secondReserved bool) []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "h20-8x")
	groups := modelDeploymentPodGroups(md)

	first := jointGroupPod("qwen-prefill-0", groups[0].Name, "qwen")
	second := jointGroupPod("qwen-decode-0", groups[1].Name, "qwen")

	return []ctrlcli.Object{
		jointCheckObject(), md, first, second,
		jointWorkload("wl-first", true, first),
		jointWorkload("wl-second", secondReserved, second),
	}
}

// TestModelDeploymentJointAdmission_TwoRolesOnOneInstanceType covers the deployment a grouping by
// instance type used to fold into one group.
//
// A PAIR ON ONE INSTANCE TYPE IS TWO GROUPS, AND WAS NOT ALWAYS. Keying a group on the instance
// type made this deployment one group, and one group is a unit Kueue admits atomically on its own,
// so the pair sat outside the barrier: its decoder could wait for capacity forever while its
// prefiller held quota in the same queue, with nothing to stop either side. Grouping on the role
// makes the two roles two groups, and these cases keep them two -- a grouping that counts types
// instead of roles answers Ready to this fixture and goes red against the held and parked cases
// while every two-instanceType case in this file stays green, which is exactly the hole the role
// key closed.
//
// THE MESSAGE NAMES THE WAITING ROLE, WHICH IS THE ONLY NAME THAT PICKS ONE OF THE TWO GROUPS OUT.
// Both schedule into the queue named after the one type there is, so the type names a queue the
// waiting side and the holding side share alike and identifies neither; the role names exactly the
// group that waits, and the queue an operator has to free follows from that role's own instance
// type.
//
// THE NOT-PARKED SIDE OF THE TABLE IS REQUIRED. A controller that held or parked every deployment
// would pass the pair cases alone, so the feasible pair and the single-role deployment beside them
// answer Ready to the same reconcile that holds and parks the short pair.
func TestModelDeploymentJointAdmission_TwoRolesOnOneInstanceType(t *testing.T) {
	start := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

	t.Run("one_role_short_still_holds_the_other", func(t *testing.T) {
		cli := newJointClient(sameTypePairFixture(false)...)

		got := reconcileJoint(t, cli, "wl-first")

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"a prefiller waits for its decoder although both name one instance type, because two "+
				"roles are two groups")
		assert.NotEqual(t, kueue.CheckStateRetry, jointCheckState(got),
			"a Retry evicts, and these siblings compete for the one queue named after their shared "+
				"type, so two Retrying sides would evict each other out of it indefinitely")
		assert.Contains(t, jointCheckMessage(got), "decode",
			"the message names the waiting role, which picks its group out of the two on one type")
		assert.True(t, workloadHasReservation(t, cli, "wl-first"),
			"the side that can run keeps the quota it reserved in that queue while it waits")
	})

	t.Run("after_the_bound_it_parks_and_reaches_status", func(t *testing.T) {
		cli := newJointClient(sameTypePairFixture(false)...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.False(t, kueueworkload.IsActive(got),
			"a pair that cannot assemble is parked out of the queue it holds, not held in it forever")
		assert.NotEmpty(t, got.Name, "and the Workload still exists: parking is not a delete")

		// THE PARK REACHES THE DEPLOYMENT'S STATUS THROUGH THIS READER, which is the function the
		// status observer calls to report Parked. It takes the verdict from exactly what this
		// controller wrote -- inactive plus its own marker -- and the status cases build their parked
		// Workload by hand, so a park this controller can no longer be seen to have made is caught
		// nowhere but here.
		assert.Equal(t, []string{"wl-first"}, parkedModelDeploymentWorkloads([]*kueue.Workload{got}),
			"the parked state reaches the deployment's reported status, not just this Workload")
	})

	t.Run("a_feasible_pair_is_admitted_even_past_the_bound", func(t *testing.T) {
		cli := newJointClient(sameTypePairFixture(true)...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
			"the barrier holds a set that cannot assemble, and this one assembled")
		assert.True(t, kueueworkload.IsActive(got), "a feasible pair is never parked")
	})

	t.Run("a_single_role_deployment_is_never_parked", func(t *testing.T) {
		md := jointDeployment("qwen", "h20-8x")
		groups := modelDeploymentPodGroups(md)
		pod := jointGroupPod("qwen-prefill-0", groups[0].Name, "qwen")
		cli := newJointClient(jointCheckObject(), md, pod, jointWorkload("wl", true, pod))
		pendingSince(t, cli, "wl", start)

		got := reconcileJointAt(t, cli, "wl", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
			"one role is one group, which Kueue admits as a unit without this barrier's help")
		assert.True(t, kueueworkload.IsActive(got), "so the bound has nothing to say to it")
	})
}
