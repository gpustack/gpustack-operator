package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

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

func jointCheckState(wl *kueue.Workload) kueue.CheckState {
	for i := range wl.Status.AdmissionChecks {
		if wl.Status.AdmissionChecks[i].Name == kueue.AdmissionCheckReference(_JointAdmissionCheckName) {
			return wl.Status.AdmissionChecks[i].State
		}
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
			// One instance type is one group, and Kueue admits a group as a unit without help.
			name: "single_group_deployment",
			objs: func() []ctrlcli.Object {
				md := jointDeployment("qwen", "h20-8x", "h20-8x")
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
		assert.Contains(t, jointCheckMessage(got), "a100-8x",
			"the message names the instance type still waiting, which is what an operator acts on")

		// THE QUOTA IS KEPT WHILE IT WAITS. Pending is a hold, and the reservation is what the other
		// group is waiting to observe; a controller that dropped it here would take the barrier apart
		// in the act of applying it.
		assert.True(t, workloadHasReservation(t, cli, "wl-first"),
			"every role keeps the quota it reserved while the set assembles")
	})
}

func jointCheckMessage(wl *kueue.Workload) string {
	for i := range wl.Status.AdmissionChecks {
		if wl.Status.AdmissionChecks[i].Name == kueue.AdmissionCheckReference(_JointAdmissionCheckName) {
			return wl.Status.AdmissionChecks[i].Message
		}
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
