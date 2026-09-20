package worker

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
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
			// One role is exempt from the barrier no matter how many replicas it declares: Kueue
			// admits each replica as its own unit without help.
			name: "single_role_deployment",
			objs: func() []ctrlcli.Object {
				md := jointDeployment("qwen", "h20-8x")
				pod := jointGroupPod("qwen-prefill-0",
					modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")

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

// twoGroupFixture builds a deployment over two instance types, one replica and one Workload per
// role, with the second role's reservation controlled by the caller.
//
// THE GROUP NAMES COME FROM THE SAME DERIVATION THE RENDERER STAMPS, per (role, ordinal): a
// fixture spelling names of its own would pass against a barrier that joins on any other key, which
// is exactly the defect per-replica enumeration exists to close.
func twoGroupFixture(secondReserved bool) []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "a100-8x")

	first := jointGroupPod("qwen-prefill-0",
		modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")
	second := jointGroupPod("qwen-decode-0",
		modelDeploymentReplicaGroupName(md, "decode", 0), "qwen")

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

// terminatingFixture is a role mid-replacement: the second role declares two replicas, both exist
// as objects, and one has already been asked to go.
//
// It is the reading the count has to get right. A Pod carrying a deletion timestamp is not a member
// the role will have, so counting it reports the role as assembled for as long as the kubelet takes
// to finish -- and an assembled-looking role is what lets the settled bound fire and park the
// replacement it exists to protect.
func terminatingFixture() []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "a100-8x")
	md.Spec.Roles[1].Replicas = 2

	first := jointGroupPod("qwen-prefill-0",
		modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")
	second := jointGroupPod("qwen-decode-0",
		modelDeploymentReplicaGroupName(md, "decode", 0), "qwen")
	leaving := jointGroupPod("qwen-decode-1",
		modelDeploymentReplicaGroupName(md, "decode", 1), "qwen")
	leaving.DeletionTimestamp = ptr.To(meta.Now())
	leaving.Finalizers = []string{"kueue.x-k8s.io/managed"}

	return []ctrlcli.Object{
		jointCheckObject(), md, first, second, leaving,
		jointWorkload("wl-first", true, first),
	}
}

// rebuildingFixture is a role short of its declared replicas: the second role declares two, one of
// them exists, and Kueue has therefore composed no Workload for the other. That is the state the
// reconciler leaves behind between an ordinal's departure and its replacement's creation.
func rebuildingFixture() []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "a100-8x")
	md.Spec.Roles[1].Replicas = 2

	first := jointGroupPod("qwen-prefill-0",
		modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")
	second := jointGroupPod("qwen-decode-0",
		modelDeploymentReplicaGroupName(md, "decode", 0), "qwen")

	return []ctrlcli.Object{
		jointCheckObject(), md, first, second,
		jointWorkload("wl-first", true, first),
	}
}

// sameTypePairFixture builds the prefill/decode deployment whose two roles name ONE instance type,
// with one replica and one Workload per role and the second role's reservation controlled by the
// caller.
//
// BOTH ROLES SCHEDULE INTO THE SAME CLUSTER QUEUE, which is what separates this shape from
// twoGroupFixture: the queue is named after the instance type, so the replica holding quota holds
// it in the very queue its sibling waits for, and the two compete for one pool instead of waiting
// in two parallel ones.
func sameTypePairFixture(secondReserved bool) []ctrlcli.Object {
	md := jointDeployment("qwen", "h20-8x", "h20-8x")

	first := jointGroupPod("qwen-prefill-0",
		modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")
	second := jointGroupPod("qwen-decode-0",
		modelDeploymentReplicaGroupName(md, "decode", 0), "qwen")

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
		pod := jointGroupPod("qwen-prefill-0",
			modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")
		cli := newJointClient(jointCheckObject(), md, pod, jointWorkload("wl", true, pod))
		pendingSince(t, cli, "wl", start)

		got := reconcileJointAt(t, cli, "wl", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
			"one role is exempt no matter how many replicas it declares, and Kueue admits each of "+
				"them as its own unit without this barrier's help")
		assert.True(t, kueueworkload.IsActive(got), "so the bound has nothing to say to it")
	})
}

// TestModelDeploymentJointAdmission_EveryReplicaOfEveryRole pins the unit the verdict counts.
//
// THE UNIT IS THE REPLICA, NOT THE ROLE. A role's replicas reserve quota separately now, so either
// of them can be the one the set is waiting on: a barrier that answered per role -- any replica
// reserving counts for all of them -- reads this fixture as complete and opens for a deployment
// that is one replica short, which is the partial admission the barrier exists to prevent.
//
// BOTH DIRECTIONS ARE REQUIRED. A barrier that held everything would pass the held case, and one
// that opened everything would pass the Ready case; only the pair says the count is being read.
func TestModelDeploymentJointAdmission_EveryReplicaOfEveryRole(t *testing.T) {
	// threeReplicaFixture: prefill of one replica, decode of two, with decode's second replica's
	// reservation controlled by the caller.
	threeReplicaFixture := func(secondReserved bool) []ctrlcli.Object {
		md := jointDeployment("qwen", "h20-8x", "a100-8x")
		md.Spec.Roles[1].Replicas = 2

		prefill := jointGroupPod("qwen-prefill-0",
			modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")
		first := jointGroupPod("qwen-decode-0",
			modelDeploymentReplicaGroupName(md, "decode", 0), "qwen")
		second := jointGroupPod("qwen-decode-1",
			modelDeploymentReplicaGroupName(md, "decode", 1), "qwen")

		return []ctrlcli.Object{
			jointCheckObject(), md, prefill, first, second,
			jointWorkload("wl-prefill", true, prefill),
			jointWorkload("wl-decode-0", true, first),
			jointWorkload("wl-decode-1", secondReserved, second),
		}
	}

	t.Run("one_replica_short_holds_the_whole_set", func(t *testing.T) {
		cli := newJointClient(threeReplicaFixture(false)...)

		got := reconcileJoint(t, cli, "wl-prefill")

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"decode's first replica reserving does not speak for its sibling: the replica is the "+
				"unit the set waits on")
		assert.NotEqual(t, kueue.CheckStateRetry, jointCheckState(got),
			"a Retry evicts and drops the reservation the barrier is made of")
		assert.Contains(t, jointCheckMessage(got), "decode",
			"the message names the role whose replica is still waiting, which is what an operator acts on")
		assert.True(t, workloadHasReservation(t, cli, "wl-prefill"),
			"every replica keeps the quota it reserved while the set assembles")
	})

	t.Run("every_replica_reserved_is_ready", func(t *testing.T) {
		got := reconcileJoint(t, newJointClient(threeReplicaFixture(true)...), "wl-prefill")

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got))
	})
}

// TestModelDeploymentJointAdmission_TheExemptionCountsRolesNotReplicas keeps the barrier's door
// shut on the deployment it was never for.
//
// THE EXEMPTION AND THE VERDICT COUNT DIFFERENT THINGS, and the difference is deliberate: the
// barrier exists for atomicity ACROSS ROLES, so a deployment with one role is answered Ready at
// once whatever its replica count. An implementation that counted replicas here would pull this
// deployment inside, where it would gain partial-quota holds and exposure to the park bound for no
// atomicity at all -- and the held multi-role cases in this file are the pairing that shows the
// Ready below is an exemption rather than a barrier that never closes.
func TestModelDeploymentJointAdmission_TheExemptionCountsRolesNotReplicas(t *testing.T) {
	md := jointDeployment("qwen", "h20-8x")
	md.Spec.Roles[0].Replicas = 2

	reserved := jointGroupPod("qwen-decode-0",
		modelDeploymentReplicaGroupName(md, "decode", 0), "qwen")
	waiting := jointGroupPod("qwen-decode-1",
		modelDeploymentReplicaGroupName(md, "decode", 1), "qwen")
	cli := newJointClient(
		jointCheckObject(), md, reserved, waiting,
		jointWorkload("wl-decode-0", true, reserved),
		jointWorkload("wl-decode-1", false, waiting))

	got := reconcileJoint(t, cli, "wl-decode-0")

	assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
		"one role is outside the barrier however many replicas it declares, so a sibling with no "+
			"reservation is Kueue's business and not this check's")
}

// TestModelDeploymentJointAdmission_AnAbsentReplicaIsReadByItsDeparture is the departure rule's
// falsification point, hand-built per state because the rule is a pure judgement over a state and
// the state is the whole subject.
//
// ABSENCE IS TWO STATES AND THEY WANT OPPOSITE VERDICTS. A replica absent because this operator is
// replacing it -- its predecessor draining, its Workload deleted to free the slot -- must read as
// present, or a rollout drags the whole multi-role deployment back to Pending for the length of a
// drain. A replica absent because nothing ever composed it must stay missing, or the barrier opens
// exactly when the set cannot assemble. A preempted replica -- its Pod stopped, its Workload
// surviving without the reservation -- must not read as either of those, or the barrier opens while
// the quota is genuinely gone.
func TestModelDeploymentJointAdmission_AnAbsentReplicaIsReadByItsDeparture(t *testing.T) {
	start := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

	// departingFixture: prefill holds quota; decode's replica is the state under test.
	departingFixture := func(decode core.Pod, decodeWorkload *kueue.Workload) []ctrlcli.Object {
		md := jointDeployment("qwen", "h20-8x", "a100-8x")

		prefill := jointGroupPod("qwen-prefill-0",
			modelDeploymentReplicaGroupName(md, "prefill", 0), "qwen")

		objs := []ctrlcli.Object{
			jointCheckObject(), md, prefill, &decode,
			jointWorkload("wl-first", true, prefill),
		}
		if decodeWorkload != nil {
			objs = append(objs, decodeWorkload)
		}

		return objs
	}

	decodePod := func(md *workercore.ModelDeployment) *core.Pod {
		pod := jointGroupPod("qwen-decode-0",
			modelDeploymentReplicaGroupName(md, "decode", 0), "qwen")
		pod.Finalizers = []string{"kueue.x-k8s.io/managed"}

		return pod
	}

	t.Run("a_draining_predecessor_counts_as_present", func(t *testing.T) {
		md := jointDeployment("qwen", "h20-8x", "a100-8x")
		leaving := decodePod(md)
		leaving.DeletionTimestamp = ptr.To(meta.Now())

		got := reconcileJoint(t, newJointClient(departingFixture(*leaving, nil)...), "wl-first")

		assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
			"a replica whose predecessor is draining and whose Workload this operator deleted to "+
				"free its slot is being replaced, and the barrier does not hold its siblings for the "+
				"length of a drain")
		assert.True(t, kueueworkload.IsActive(got), "and it is never parked")
	})

	t.Run("a_missing_replica_stays_missing_and_is_never_parked", func(t *testing.T) {
		// The predecessor is gone and the replacement has not been created: nothing occupies the
		// ordinal at all. That is the state a role whose creates are erroring sits in, and the one
		// the barrier exists for -- but it is also an ordinary state mid-assembly, so the bound may
		// not touch it.
		md := jointDeployment("qwen", "h20-8x", "a100-8x")
		absent := decodePod(md)
		absent.Finalizers = nil

		cli := newJointClient(departingFixture(*absent, nil)...)
		absent.DeletionTimestamp = ptr.To(meta.Now())
		require.NoError(t, cli.Delete(context.Background(), absent))
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"an ordinal with no replica at all is the deployment still assembling, and the set waits")
		assert.True(t, kueueworkload.IsActive(got),
			"a deployment short of its own replicas is assembling, not infeasible, so the bound "+
				"does not park it")
		assert.Contains(t, jointCheckMessage(got), "decode",
			"the message names the role still assembling")
	})

	t.Run("a_surviving_workload_without_quota_is_not_a_rollout", func(t *testing.T) {
		// The preempted shape: Kueue's preemption stops a group by deleting its Pods while the
		// Workload stands, so the Pod is terminating and the Workload exists holding nothing.
		// Reading that as a replacement would open the barrier while the quota is genuinely gone.
		md := jointDeployment("qwen", "h20-8x", "a100-8x")
		stopped := decodePod(md)
		stopped.DeletionTimestamp = ptr.To(meta.Now())

		got := reconcileJoint(t, newJointClient(departingFixture(*stopped,
			jointWorkload("wl-decode", false, stopped))...), "wl-first")

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"a replica whose Workload survives without its reservation was preempted, not replaced, "+
				"and the set waits for the quota to come back")
		assert.True(t, kueueworkload.IsActive(got))
	})

	t.Run("a_live_successor_without_a_workload_waits_like_any_other", func(t *testing.T) {
		// The compose-lag shape the spec's replacement cadence ends on: the predecessor is gone,
		// the successor exists, and Kueue has not composed its Workload yet. Nothing here says
		// rollout -- no predecessor, no Workload -- so it reads as an ordinary wait, and an
		// ordinary wait past the bound is a park.
		md := jointDeployment("qwen", "h20-8x", "a100-8x")
		successor := decodePod(md)
		successor.Finalizers = nil

		cli := newJointClient(departingFixture(*successor, nil)...)
		pendingSince(t, cli, "wl-first", start)

		got := reconcileJointAt(t, cli, "wl-first", start.Add(_JointAdmissionInfeasibleAfter+time.Minute))

		assert.Equal(t, kueue.CheckStatePending, jointCheckState(got),
			"a live successor with no Workload composed is a wait, and the barrier holds it")
		assert.False(t, kueueworkload.IsActive(got),
			"the shape is stable and the wait is not, so past the bound the hold becomes a park")
	})
}

// newRolloutClient builds the client both rollout cases run on: the deployment's status
// subresources for the convergence loop, and the Workload's and the check's for the barrier's
// verdict writes.
func newRolloutClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(
			&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{},
			&kueue.Workload{}, &kueue.AdmissionCheck{},
		).
		WithObjects(objs...).
		Build()
}

// stampReplicaUIDs gives every replica a distinct UID, standing in for the API server's half of a
// create: the fake client assigns none, and every ownership question in this controller is answered
// by UID -- a fleet of empty UIDs makes one Workload "own" every replica, and a barrier test that
// cannot tell replicas apart proves nothing.
func stampReplicaUIDs(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	for i, pod := range replicaPods(t, cli) {
		if pod.UID != "" {
			continue
		}
		pod.UID = types.UID(fmt.Sprintf("uid-%s-%d", pod.Name, i))
		require.NoError(t, cli.Update(context.Background(), &pod))
	}
}

// armReplicaFinalizers gives every replica Kueue's own finalizer, which is the admission-time shape
// on a real cluster: a deleted replica then lingers as terminating rather than vanishing, and it is
// that lingering the departure rule and the replacement gates read.
func armReplicaFinalizers(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	for _, pod := range replicaPods(t, cli) {
		if slices.Contains(pod.Finalizers, kueuepodconst.PodFinalizer) {
			continue
		}
		pod.Finalizers = append(pod.Finalizers, kueuepodconst.PodFinalizer)
		require.NoError(t, cli.Update(context.Background(), &pod))
	}
}

// composeWorkloadFor stands in for Kueue composing and reserving for one replica: a Workload named
// after the Pod's group verbatim, owning that Pod, holding a reservation, and carrying this
// controller's check as Pending -- the state a Workload is in between reserving and the barrier's
// first verdict.
func composeWorkloadFor(t *testing.T, cli ctrlcli.Client, pod core.Pod) {
	t.Helper()

	wl := new(kueue.Workload)
	wl.Name, wl.Namespace = pod.Labels[kueuepodconst.GroupNameLabel], pod.Namespace
	wl.UID = types.UID("uid-" + wl.Name)
	wl.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
	}}
	wl.Status.Conditions = []meta.Condition{{
		Type:               kueue.WorkloadQuotaReserved,
		Status:             meta.ConditionTrue,
		Reason:             "Test",
		LastTransitionTime: meta.Now(),
	}}
	wl.Status.AdmissionChecks = []kueue.AdmissionCheckState{{
		Name:  kueue.AdmissionCheckReference(_JointAdmissionCheckName),
		State: kueue.CheckStatePending,
	}}

	require.NoError(t, cli.Create(context.Background(), wl))
}

// admitWorkload stands in for Kueue's move once every check on a reserved Workload reads Ready.
func admitWorkload(t *testing.T, cli ctrlcli.Client, wl *kueue.Workload) {
	t.Helper()

	wl.Status.Conditions = append(wl.Status.Conditions, meta.Condition{
		Type:               kueue.WorkloadAdmitted,
		Status:             meta.ConditionTrue,
		Reason:             "Test",
		LastTransitionTime: meta.Now(),
	})
	require.NoError(t, cli.Status().Update(context.Background(), wl))
}

// rolloutWorkloads lists the namespace's Workloads, for the rounds that run the barrier over all of
// them.
func rolloutWorkloads(t *testing.T, cli ctrlcli.Client) []kueue.Workload {
	t.Helper()

	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(context.Background(), wlList, ctrlcli.InNamespace("team-a")))

	return wlList.Items
}

// rolloutImages reports the image of every live replica, which is what tells a replica built before
// an edit from one built after it.
func rolloutImages(t *testing.T, cli ctrlcli.Client) map[string]string {
	t.Helper()

	images := make(map[string]string)
	for _, pod := range replicaPods(t, cli) {
		if pod.DeletionTimestamp == nil {
			images[pod.Name] = pod.Spec.Containers[0].Image
		}
	}

	return images
}

// rolloutSetup converges the deployment, stands in for Kueue on its whole admission half, and runs
// the barrier once -- the steady state every rollout case then edits.
func rolloutSetup(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	stampReplicaUIDs(t, cli)
	armReplicaFinalizers(t, cli)
	for _, pod := range replicaPods(t, cli) {
		composeWorkloadFor(t, cli, pod)
	}

	for _, wl := range rolloutWorkloads(t, cli) {
		got := reconcileJoint(t, cli, wl.Name)
		require.Equal(t, kueue.CheckStateReady, jointCheckState(got),
			"the steady state is feasible: every replica holds quota, so the barrier opens")
		admitWorkload(t, cli, got)
	}
}

// TestModelDeploymentJointAdmission_AnAdmittedWorkloadIsSkippedThroughARollout drives a rollout
// under the mechanics this operator runs today -- a replaced replica's Pod and Workload are deleted
// together, and its replacement is a fresh Pod whose own fresh Workload reserves and is judged on
// its own -- and pins what that makes of the barrier.
//
// WHAT THIS FALSIFIES IS THE SKIP, not the verdict. A Workload that was admitted stays admitted
// through the rollout, so the only answer the barrier may give about it is none: re-answering an
// admitted Workload mid-rollout -- while a replacement already exists with no Workload composed for
// it yet and the deployment reads as assembling -- would write Pending over a settled placement and
// drag a running deployment back through the barrier it already passed. A barrier that stopped
// skipping turns Ready into Pending here; nothing else in this file can see that, because every
// other case judges Workloads that are still waiting.
//
// THE ASSERTIONS ARE SCOPED TO WORKLOADS ALREADY ADMITTED, and the scope is the mechanics rather
// than a relaxation: a replacement's Workload does not exist before its ordinal turns over, and it
// cannot be admitted before the barrier has judged it, so the loop admits it once the verdict reads
// Ready -- Kueue's own move -- and from then on holds it to the same skip as the originals.
func TestModelDeploymentJointAdmission_AnAdmittedWorkloadIsSkippedThroughARollout(t *testing.T) {
	ctx := context.Background()
	cli := newRolloutClient(jointCheckObject(), twoRoleDeployment(), newRenderInstanceType())

	rolloutSetup(t, cli)

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	changed.Spec.Roles[1].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(ctx, changed))

	for round := 0; round < 40; round++ {
		// KUEUE RELEASES A DEPARTED REPLICA'S FINALIZER ONCE ITS WORKLOAD IS GONE. The converger
		// deletes the pair together, and the fake cluster holds the Pod until this clears it -- the
		// compressed shape of the drain a real cluster measures in tens of seconds.
		wls := rolloutWorkloads(t, cli)
		for _, pod := range replicaPods(t, cli) {
			if pod.DeletionTimestamp == nil || anyWorkloadOwnsAny(wls, sets.New(pod.UID)) {
				continue
			}
			pod.Finalizers = nil
			require.NoError(t, cli.Update(ctx, &pod))
		}

		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err, "round %d", round)
		stampReplicaUIDs(t, cli)

		// THE BARRIER RUNS WHILE THE NEWEST REPLICAS HAVE NO WORKLOAD YET: Kueue composes on its
		// own events, and this loop composes at the round's end. The gap is the state the skip
		// exists for -- a barrier that re-judged an admitted Workload now would read the
		// deployment as assembling and answer Pending over a settled placement.
		for _, wl := range rolloutWorkloads(t, cli) {
			// Read off the object as it ARRIVED, not as the reconcile leaves it: a Workload that
			// was already admitted is owed no new answer, while a replacement's fresh Workload is
			// Kueue's to admit once the barrier opens. The two cannot be told apart by name -- a
			// replacement's Workload carries the departed one's -- so the admission on the object
			// is what says which this one is.
			if !kueueworkload.IsAdmitted(&wl) {
				got := reconcileJoint(t, cli, wl.Name)
				if jointCheckState(got) == kueue.CheckStateReady {
					admitWorkload(t, cli, got)
				}
				continue
			}

			got := reconcileJoint(t, cli, wl.Name)
			assert.Equal(t, kueue.CheckStateReady, jointCheckState(got),
				"round %d: a Workload that already passed the barrier keeps the verdict it was "+
					"admitted on, through a rollout that momentarily reads the deployment as "+
					"assembling", round)
			assert.True(t, kueueworkload.IsAdmitted(got),
				"round %d: nothing here may evict an admitted Workload", round)
			assert.True(t, kueueworkload.IsActive(got),
				"round %d: and nothing here may park one", round)
		}

		// KUEUE COMPOSES ONE WORKLOAD PER LIVE REPLICA THAT HAS NONE, named after the replica's own
		// group: the replacement does not ride the departed replica's Workload in -- it is a fresh
		// reservation of its own, which the barrier judges on the next round.
		wls = rolloutWorkloads(t, cli)
		for _, pod := range replicaPods(t, cli) {
			if pod.DeletionTimestamp == nil && !anyWorkloadOwnsAny(wls, sets.New(pod.UID)) {
				composeWorkloadFor(t, cli, pod)
			}
		}

		current := true
		for _, image := range rolloutImages(t, cli) {
			current = current && image == "vllm/vllm-openai:v0.26.0"
		}
		if current && len(rolloutImages(t, cli)) == 4 {
			return
		}
	}

	t.Fatal("the rollout did not complete within its rounds")
}

// TestModelDeploymentJointAdmission_ARolloutRunsToCompletionWithTheBarrierActive drives a rollout
// under the mechanics the spec's replacement path is built for -- freeing a replica's slot deletes
// its Workload, so each replacement is a fresh reservation the barrier has to judge -- with the
// barrier running over every Workload on every round.
//
// THIS ONE IS A SMOKE FOR THE ROLLOUT, NOT THE DEPARTURE RULE'S FALSIFICATION POINT. Its
// assertions hold under either interleaving of predecessor and successor, which is exactly why it
// can drive the convergence loop's real cadence without guessing at the create gate's timing: what
// it proves is that the whole loop -- edit, departures, slot frees, recompositions, verdicts,
// admissions -- runs to the end with no Retry, no Reject and no park anywhere, and that the
// deployment comes out the other side fully admitted. The departure rule itself is pinned
// hand-built, per state, in the case above.
func TestModelDeploymentJointAdmission_ARolloutRunsToCompletionWithTheBarrierActive(t *testing.T) {
	ctx := context.Background()
	cli := newRolloutClient(jointCheckObject(), twoRoleDeployment(), newRenderInstanceType())

	rolloutSetup(t, cli)

	changed := getModelDeployment(t, cli)
	changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	changed.Spec.Roles[1].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, cli.Update(ctx, changed))

	// draining counts the rounds a departed replica has been draining. The converger deletes the
	// replica's Workload with the Pod, and the fake cluster holds the Pod until the finalizer is
	// cleared below -- the compressed shape of a drain the real cluster measures in tens of seconds.
	draining := make(map[string]int)
	admittedMidReplacement := false

	for round := 0; round < 40; round++ {
		for _, pod := range replicaPods(t, cli) {
			if pod.DeletionTimestamp == nil {
				continue
			}
			if _, seen := draining[pod.Name]; seen {
				continue
			}
			draining[pod.Name] = 0
		}
		for name := range draining {
			draining[name]++
		}
		// ONE DRAIN COMPLETES PER ROUND and the other stays open. The pass deletes one replica per
		// role, so both predecessors of a pair are draining together; releasing both in one round
		// would seat both replacements before either is judged, and every admission would land after
		// the window had shut. Holding one predecessor draining -- Workload gone, Pod still on the
		// books -- while the other ordinal's replacement is composed and judged is the state the
		// departure rule exists for, and the flag below proves the loop reached it.
		for _, pod := range replicaPods(t, cli) {
			if pod.DeletionTimestamp == nil || draining[pod.Name] < 2 {
				continue
			}
			pod.Finalizers = nil
			require.NoError(t, cli.Update(ctx, &pod))
			delete(draining, pod.Name)
			break
		}

		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err, "round %d", round)
		stampReplicaUIDs(t, cli)

		// KUEUE COMPOSES PER GROUP ON ITS OWN EVENTS, and one composition per round is the
		// interleaving where a sibling is judged while another ordinal's recomposition has not
		// landed. Composing all of them at once would answer every verdict through quota and never
		// exercise the window between a slot freeing and its reservation returning.
		composed := false
		wls := rolloutWorkloads(t, cli)
		for _, pod := range replicaPods(t, cli) {
			if composed || pod.DeletionTimestamp != nil {
				continue
			}
			owned := false
			for i := range wls {
				for _, ref := range wls[i].OwnerReferences {
					owned = owned || (ref.Kind == "Pod" && ref.UID == pod.UID)
				}
			}
			if owned {
				continue
			}
			composeWorkloadFor(t, cli, pod)
			composed = true
		}

		// THE BARRIER RUNS OVER EVERY WORKLOAD, ADMITTED ONES INCLUDED; nothing here skips it.
		for _, wl := range rolloutWorkloads(t, cli) {
			got := reconcileJoint(t, cli, wl.Name)

			assert.NotEqual(t, kueue.CheckStateRetry, jointCheckState(got),
				"round %d: a Retry would evict this Workload and drop the very reservation the "+
					"rollout's next step depends on", round)
			assert.NotEqual(t, kueue.CheckStateRejected, jointCheckState(got),
				"round %d: a Rejected is final, and a set mid-rollout is not infeasible", round)
			assert.True(t, kueueworkload.IsActive(got),
				"round %d: a deployment mid-rollout is changing shape, so the bound never parks it",
				round)

			if jointCheckState(got) != kueue.CheckStateReady || kueueworkload.IsAdmitted(got) {
				continue
			}
			admitWorkload(t, cli, got)
			if anyReplicaMidReplacement(t, cli) {
				admittedMidReplacement = true
			}
		}

		current := true
		for _, image := range rolloutImages(t, cli) {
			current = current && image == "vllm/vllm-openai:v0.26.0"
		}
		wlsNow := rolloutWorkloads(t, cli)
		allAdmitted := len(wlsNow) == 4
		for i := range wlsNow {
			allAdmitted = allAdmitted && kueueworkload.IsAdmitted(&wlsNow[i])
		}
		if current && len(rolloutImages(t, cli)) == 4 && allAdmitted {
			assert.True(t, admittedMidReplacement,
				"the rollout passed through a state where a sibling was admitted while another "+
					"ordinal's predecessor was draining with its Workload deleted -- the window the "+
					"departure rule exists for, and this loop's interleaving was built to reach it")
			return
		}
	}

	t.Fatal("the rollout did not complete within its rounds")
}

// anyReplicaMidReplacement reports whether some declared ordinal currently sits in the departure
// window: a member draining while no Workload at all owns the ordinal's replicas.
//
// It reads through the production predicate rather than restating it, so what the flag observes is
// the same judgement the verdict made.
func anyReplicaMidReplacement(t *testing.T, cli ctrlcli.Client) bool {
	t.Helper()

	md := getModelDeployment(t, cli)
	byGroup, liveByGroup, err := modelDeploymentReplicaGroups(context.Background(), cli, md)
	require.NoError(t, err)

	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(context.Background(), wlList, ctrlcli.InNamespace("team-a")))

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		for ordinal := range int(role.Replicas) {
			group := modelDeploymentReplicaGroupName(md, role.Name, ordinal)
			if jointReplicaDepartingRollout(md, wlList.Items, byGroup[group], liveByGroup[group]) {
				return true
			}
		}
	}

	return false
}
