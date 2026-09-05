package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// twoRoleDeployment is the P/D shape: prefill 2 and decode 2 on one instanceType, four Pods in one
// group.
func twoRoleDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	return newRenderDeployment(append([]func(*workercore.ModelDeployment){
		func(md *workercore.ModelDeployment) {
			decode := md.Spec.Roles[0]
			md.Spec.Roles[0].Name = "prefill"
			decode.Name = "decode"
			md.Spec.Roles = append(md.Spec.Roles, decode)
		},
	}, mutate...)...)
}

// replicaPods lists the replicas the deployment owns, unsorted, for the cases that read metadata off
// them rather than names.
func replicaPods(t *testing.T, cli ctrlcli.Client) []core.Pod {
	t.Helper()

	podList := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), podList, ctrlcli.InNamespace("team-a")))

	return podList.Items
}

// groupTotals collects the group total every live replica declares, so a case can assert the SET
// rather than probe one Pod.
func groupTotals(t *testing.T, cli ctrlcli.Client) map[string]int {
	t.Helper()

	totals := make(map[string]int)
	for _, pod := range replicaPods(t, cli) {
		totals[pod.Annotations[kueuepodconst.GroupTotalCountAnnotation]]++
	}

	return totals
}

// TestModelDeployment_GroupIsCreatedInOnePass is F2's first obligation.
//
// Kueue composes NO Workload for a group it has not fully seen: fewer runnable Pods than the
// declared total is an unretryable compose error. So the creates for every role's every replica are
// issued in one pass, and none of them waits on another's readiness. A reconciler that staged them
// role by role would leave the group short of its total for as long as the staging took, and the
// symptom of that is nothing at all -- Pods exist, they are gated, and `kubectl get workloads` is
// empty.
func TestModelDeployment_GroupIsCreatedInOnePass(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"qwen-decode-0", "qwen-decode-1", "qwen-prefill-0", "qwen-prefill-1",
	}, replicaNames(t, cli), "every role's every replica, in the first pass")

	assert.Equal(t, map[string]int{"4": 4}, groupTotals(t, cli),
		"and all four declare the same total, which is what makes them one group")
}

// TestModelDeployment_GroupMembersAgreeAndAreOwned covers the metadata the group is made of, read
// off the Pods the reconciler actually created rather than off the render.
func TestModelDeployment_GroupMembersAgreeAndAreOwned(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	pods := replicaPods(t, cli)
	require.Len(t, pods, 4)

	byRole := map[string]int{}
	for i := range pods {
		pod := &pods[i]

		assert.Equal(t, "qwen", pod.Labels[kueuepodconst.GroupNameLabel],
			"%s must be in the deployment's one group", pod.Name)
		assert.Equal(t, modelDeploymentPodRole(pod),
			pod.Annotations[kueuepodconst.RoleHashAnnotation],
			"%s: the label status reads a Pod's role from and the PodSet identity must name the "+
				"same role, or a role's readiness is counted against another's PodSet", pod.Name)

		require.Len(t, pod.OwnerReferences, 1, "%s must be owned by the deployment", pod.Name)
		assert.Equal(t, "qwen", pod.OwnerReferences[0].Name)

		byRole[pod.Annotations[kueuepodconst.RoleHashAnnotation]]++
	}

	assert.Equal(t, map[string]int{"prefill": 2, "decode": 2}, byRole,
		"two PodSets of two, not one of four")
}

// TestModelDeployment_ReplicasChangeRebuildsTheGroup is F10, and its middle assertion is the point.
//
// A replicas change moves the group's declared total, which every Pod carries and which Kueue
// requires them all to agree on. So the change cannot be a per-replica rollout: while any Pod still
// declares the old total, adding one that declares the new one produces a group Kueue refuses to
// compose -- and refuses SILENTLY, with no Workload and no condition naming the cause.
//
// THE INTERMEDIATE STATE IS WHAT IS ASSERTED, not only the end state. The end state is identical
// whether the rebuild waited or not, so a test that only checked it would pass against exactly the
// implementation this exists to rule out.
func TestModelDeployment_ReplicasChangeRebuildsTheGroup(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"4": 4}, groupTotals(t, cli))

	grown := getModelDeployment(t, cli)
	grown.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), grown))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Empty(t, replicaNames(t, cli),
		"the old group goes before the new one arrives; a fifth Pod created here would declare 5 "+
			"beside four Pods declaring 4")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"5": 5}, groupTotals(t, cli),
		"and the rebuilt group agrees on the new total")
	assert.Equal(t, []string{
		"qwen-decode-0", "qwen-decode-1",
		"qwen-prefill-0", "qwen-prefill-1", "qwen-prefill-2",
	}, replicaNames(t, cli))
}

// TestModelDeployment_RoleSetChangeRebuildsTheGroup covers the other axis: adding a role and
// removing one both move the total, so both rebuild.
func TestModelDeployment_RoleSetChangeRebuildsTheGroup(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	shrunk := getModelDeployment(t, cli)
	shrunk.Spec.Roles = shrunk.Spec.Roles[:1]
	require.NoError(t, cli.Update(context.Background(), shrunk))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Empty(t, replicaNames(t, cli), "removing a role rebuilds the group")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, []string{"qwen-prefill-0", "qwen-prefill-1"}, replicaNames(t, cli))
	assert.Equal(t, map[string]int{"2": 2}, groupTotals(t, cli))
}

// TestModelDeployment_GroupIsIdempotent pins that the rebuild predicate does not fire on a spec that
// has not moved.
//
// This is the failure mode a rebuild policy invites: a predicate that answers "the group changed" on
// every pass deletes and recreates the whole deployment forever, and each cycle looks, from a single
// pass, exactly like a legitimate rollout.
func TestModelDeployment_GroupIsIdempotent(t *testing.T) {
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Zero(t, writes.creates, "an unchanged spec creates nothing")
	assert.Zero(t, writes.deletes, "and deletes nothing")
	assert.Len(t, replicaNames(t, cli), 4)
}

// TestModelDeployment_HandDeletedReplicaIsRecreatedWithoutARebuild separates the two paths.
//
// A missing Pod does not move the declared total, so it is the ONE case that must not rebuild: the
// group is short of its total already, and deleting its survivors would widen exactly the gap that
// keeps Kueue from composing the Workload.
//
// GONE IS NOT DEPARTING, and the difference is the whole reason both cases exist. This fixture's Pod
// is already absent, which is what the fake client's Delete produces and what a live cluster reaches
// only after Kueue's finalizer has been released. Getting there from an operator's `kubectl delete
// pod` goes through the state the departing-replica case covers, and that one does rebuild -- so this
// is not the assertion that a hand-deleted replica is replaced in isolation on a real cluster.
func TestModelDeployment_HandDeletedReplicaIsRecreatedWithoutARebuild(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	gone := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-decode-1"}, gone))
	require.NoError(t, cli.Delete(context.Background(), gone))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Len(t, replicaNames(t, cli), 4, "the survivors stay and the missing one comes back")
	assert.Equal(t, map[string]int{"4": 4}, groupTotals(t, cli))
}

// TestModelDeploymentPodGroupIncomplete_IsAReportedStateNotASilentOne is T9, at the level this tree
// can reach.
//
// F2's failure mode is SILENCE: a group short of its declared total has Pods, they are gated, Kueue
// composes no Workload at all, and nothing says why. So the assertion is the ABSENCE of a Workload
// alongside a condition that names the absence -- and an absence is exactly the assertion that
// passes for the wrong reason when the query is wrong.
//
// THE CONTROL IS WHAT MAKES THE ABSENCE MEAN ANYTHING. A Workload belonging to somebody else sits in
// the same namespace, and the case first shows the List returns it. Without that, "no Workload for
// this deployment" and "the List is broken, or looking in the wrong namespace" are the same result.
func TestModelDeploymentPodGroupIncomplete_IsAReportedStateNotASilentOne(t *testing.T) {
	md := twoRoleDeployment()

	// The control: a Workload in the same namespace that owns a Pod this deployment never rendered.
	stranger := &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: "team-a", Name: "somebody-else-0", UID: types.UID("uid-stranger"),
	}}
	control := groupWorkload([]core.Pod{*stranger}, true)

	// The fourth create fails, which is how a group ends up short of its total without anything
	// crashing. The reconcile still issues the other three and still writes the status.
	var failed bool
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(md, newRenderInstanceType(), control).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Create: func(
				ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption,
			) error {
				if pod, ok := obj.(*core.Pod); ok && pod.Name == "qwen-decode-1" && !failed {
					failed = true

					return kerrors.NewInternalError(errors.New("the API server said no"))
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	_, err := reconcileModelDeployment(t, cli)
	require.Error(t, err, "the pass is not successful: a replica the group needs was not created")
	require.Len(t, replicaNames(t, cli), 3, "and the other three were still issued")

	// The control first: the List works and can see a Workload in this namespace.
	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(context.Background(), wlList, ctrlcli.InNamespace("team-a")))
	require.Len(t, wlList.Items, 1, "the control: this List does return Workloads it can see")
	require.Equal(t, control.Name, wlList.Items[0].Name)

	// And none of them is ours, which is the state F2 describes.
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	pods, err := r.listModelDeploymentPods(context.Background(), md)
	require.NoError(t, err)
	ours, err := r.findModelDeploymentGroupWorkload(context.Background(), md, pods)
	require.NoError(t, err)
	assert.Nil(t, ours, "an incomplete group has no Workload, which is why it needs a reason")

	stored := getModelDeployment(t, cli)
	assert.True(t, ModelDeploymentConditionQuotaReserved.IsFalse(stored))
	assert.Equal(t, "PodGroupIncomplete",
		ModelDeploymentConditionQuotaReserved.GetReason(stored))
	assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(stored), "3 of 4",
		"the message carries have/want, so a reader knows how far short it is")
}

// TestModelDeploymentPodGroupIncomplete_ClearsWhenTheGroupCompletes is the other half: the reason
// has to be transient, or it would be indistinguishable from a permanent refusal.
//
// It stops at "the group is complete and Kueue has not answered yet", because nothing in this tree
// runs Kueue: a Workload appearing is that controller's action, and this repository has no envtest
// harness to host it. Asserting a Workload appears here would need a fake one placed by the test,
// which would assert the test's own placement rather than Kueue's composition. The real thing is
// case-49's, on a cluster.
func TestModelDeploymentPodGroupIncomplete_ClearsWhenTheGroupCompletes(t *testing.T) {
	md := twoRoleDeployment()

	var failed bool
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{}).
		WithObjects(md, newRenderInstanceType()).
		WithInterceptorFuncs(ctrlinterceptor.Funcs{
			Create: func(
				ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption,
			) error {
				if pod, ok := obj.(*core.Pod); ok && pod.Name == "qwen-decode-1" && !failed {
					failed = true

					return kerrors.NewInternalError(errors.New("the API server said no"))
				}

				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	_, err := reconcileModelDeployment(t, cli)
	require.Error(t, err)
	require.Equal(t, "PodGroupIncomplete",
		ModelDeploymentConditionQuotaReserved.GetReason(getModelDeployment(t, cli)))

	// The next pass creates the missing replica, exactly as the level-based loop is meant to.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	stored := getModelDeployment(t, cli)
	assert.NotEqual(t, "PodGroupIncomplete",
		ModelDeploymentConditionQuotaReserved.GetReason(stored),
		"a complete group is no longer short of its total")
	assert.Equal(t, "AdmissionInFlight",
		ModelDeploymentConditionQuotaReserved.GetReason(stored),
		"and what it is waiting on now is Kueue, which nothing here runs")
}

// TestModelDeployment_ADepartingReplicaTakesTheWorkloadWithIt covers the wait that had no end.
//
// A replica asked to leave cannot leave on its own. Kueue holds a finalizer on every Pod of a group
// and releases it when the group finishes or when the Workload is deleted, and the group these render
// is annotated serving -- which Kueue defines as never finished. The Workload is owned by that same
// replica without a controller reference, so garbage collection is blocked behind it in turn.
// Measured on a live cluster before the fix: a template edit left one Pod Succeeded and undeletable,
// its replacement uncreatable because the name was taken, and the reconciler reissuing the same delete
// every two seconds with nothing erroring.
//
// THE FIXTURE IS THE FINALIZER. Without it the fake client removes the Pod on Delete, the reconciler
// sees a missing replica rather than a departing one, and the case passes against the very
// implementation it exists to rule out.
func TestModelDeployment_ADepartingReplicaTakesTheWorkloadWithIt(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	leaving := new(core.Pod)
	require.NoError(t, cli.Get(ctx,
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-decode-1"}, leaving))
	leaving.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, leaving))
	require.NoError(t, cli.Delete(ctx, leaving))

	// The Workload Kueue composed for the group: plain owner references to the members, no controller
	// reference, which is the shape the operator has to find it by.
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace = "qwen", "team-a"
	for _, pod := range replicaPods(t, cli) {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		})
	}
	require.NoError(t, cli.Create(ctx, wl))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	err = cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, new(kueue.Workload))
	assert.True(t, kerrors.IsNotFound(err),
		"the Workload is the only thing that releases the replica's finalizer, so it goes: %v", err)

	// The survivors are deleted by the reconciler; the departing one stays because nothing here
	// releases Kueue's finalizer -- there is no Kueue in this tree, which is exactly the state the
	// live cluster was stuck in. Asserting the SET rather than "fewer than four" is what says no fifth
	// Pod was created beside a member that cannot leave.
	assert.Equal(t, []string{"qwen-decode-1"}, replicaNames(t, cli),
		"the departure brings the group down, and nothing is built back while a member is still held")
}

// TestModelDeployment_NoDepartureLeavesTheWorkloadAlone is the other half, and without it the case
// above passes against a reconciler that deletes the group's Workload on every pass -- which would
// take the whole deployment down each time, since Kueue answers a deleted Workload by stopping the
// group.
func TestModelDeployment_NoDepartureLeavesTheWorkloadAlone(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	wl := &kueue.Workload{}
	wl.Name, wl.Namespace = "qwen", "team-a"
	for _, pod := range replicaPods(t, cli) {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		})
	}
	require.NoError(t, cli.Create(ctx, wl))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.NoError(t,
		cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, new(kueue.Workload)),
		"a settled group keeps the admission it was granted")
	assert.Len(t, replicaNames(t, cli), 4)
}

// TestModelDeployment_RedistributingReplicasRebuildsTheGroup is the case a total-only predicate
// cannot see.
//
// prefill 2 / decode 2 becoming prefill 1 / decode 3 leaves the group's declared total at four, so a
// rebuild predicate reading only the sum answers "not resizing" — and the converge loop then trims a
// prefill and creates a decode in the SAME pass, which is precisely the mixed group the rebuild
// exists to avoid. The end state is reached anyway, one pass later, because a terminating replica is
// itself a rebuild trigger; that is what makes this failure invisible from the end state alone.
func TestModelDeployment_RedistributingReplicasRebuildsTheGroup(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"4": 4}, groupTotals(t, cli))

	moved := getModelDeployment(t, cli)
	moved.Spec.Roles[0].Replicas = 1
	moved.Spec.Roles[1].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), moved))
	require.Equal(t, int32(4), modelDeploymentPodGroupTotalCount(moved),
		"the premise: the total did not move, so only the per-role share can carry the change")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Empty(t, replicaNames(t, cli),
		"the whole group goes; creating the new decode beside a departing prefill is the mixed state")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"qwen-decode-0", "qwen-decode-1", "qwen-decode-2", "qwen-prefill-0",
	}, replicaNames(t, cli))
	assert.Equal(t, map[string]int{"4": 4}, groupTotals(t, cli))
}

// TestModelDeployment_RenamingARoleWithoutChangingCountsRebuilds covers the other shape change that
// moves neither the total nor any count: the same numbers under a different role name.
func TestModelDeployment_RenamingARoleWithoutChangingCountsRebuilds(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	renamed := getModelDeployment(t, cli)
	renamed.Spec.Roles[1].Name = "decoder"
	require.NoError(t, cli.Update(context.Background(), renamed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Empty(t, replicaNames(t, cli), "a renamed role is a different PodSet, so the group is rebuilt")
}
