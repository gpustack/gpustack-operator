package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// twoRoleDeployment is the P/D shape: prefill 2 and decode 2 on one instanceType, four Pods in two
// groups of two.
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

// replicaRoleCounts collects how many live replicas each role attributes to itself, so a case can
// state the shape it expects without naming server-assigned names.
func replicaRoleCounts(t *testing.T, cli ctrlcli.Client) map[string]int {
	t.Helper()

	counts := make(map[string]int)
	for _, pod := range replicaPods(t, cli) {
		counts[modelDeploymentPodRole(&pod)]++
	}

	return counts
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

	assert.Equal(t, map[string]int{"decode": 2, "prefill": 2}, replicaRoleCounts(t, cli),
		"every role's every replica, in the first pass")

	assert.Equal(t, map[string]int{"2": 4}, groupTotals(t, cli),
		"each Pod declares its own role's count, and each group's members agree on theirs")
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
	byGroup := map[string]int{}
	for i := range pods {
		pod := &pods[i]

		byGroup[pod.Labels[kueuepodconst.GroupNameLabel]]++
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
	require.Len(t, byGroup, 2, "two roles on one instanceType are two groups")
	for group, n := range byGroup {
		assert.Equal(t, 2, n, "group %s holds its own role's two and nobody else's", group)
	}
}

// TestModelDeployment_ReplicasChangeRebuildsTheGroup is F10, and its middle assertion is the point.
//
// A replicas change moves that role's group's declared total, which every Pod of the group carries
// and which Kueue requires them all to agree on. So the change cannot be a per-replica rollout:
// while any Pod still declares the old total, adding one that declares the new one produces a group
// Kueue refuses to compose -- and refuses SILENTLY, with no Workload and no condition naming the
// cause.
//
// THE INTERMEDIATE STATE IS WHAT IS ASSERTED, not only the end state. The end state is identical
// whether the rebuild waited or not, so a test that only checked it would pass against exactly the
// implementation this exists to rule out.
func TestModelDeployment_ReplicasChangeRebuildsTheGroup(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"2": 4}, groupTotals(t, cli))

	grown := getModelDeployment(t, cli)
	grown.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), grown))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"decode": 2}, replicaRoleCounts(t, cli),
		"the moved role's group goes before the new one arrives; a new prefill created here would "+
			"declare 3 beside two declaring 2 -- and the sibling role's Pods are nobody's business")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"3": 3, "2": 2}, groupTotals(t, cli),
		"the rebuilt group agrees on the new count and the untouched one on its old one")
	assert.Equal(t, map[string]int{"decode": 2, "prefill": 3}, replicaRoleCounts(t, cli))
}

// TestModelDeployment_RoleSetChangeRebuildsTheGroup covers the other axis: removing a role takes
// that role's group down at once, and the survivor's Pods turn over ONE AT A TIME.
//
// The survivor's turnover is the sole-rename the group naming performs: a two-role deployment's
// groups are all hashed, and the one remaining role's group becomes the readable deployment name,
// so every existing Pod carries a group label the spec no longer forms and its fingerprint no
// longer matches. The removed role's Pods go in the first pass; the survivor's roll out on the
// ordinary cadence, one departure at a time, and the group settles once the last one landed.
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
	assert.Equal(t, map[string]int{"prefill": 1}, replicaRoleCounts(t, cli),
		"the removed role's group goes in the first pass, and one survivor rolls: the sole role's "+
			"group is renamed by becoming the only one -- a readable name -- so its Pods are "+
			"outdated and turn over on the rollout cadence")

	// The survivor's turnover completes; the group settles under the readable name alone.
	for pass := 0; pass < 6; pass++ {
		if _, err = reconcileModelDeployment(t, cli); err != nil {
			break
		}
		if got := replicaRoleCounts(t, cli); got["prefill"] == 2 && len(groupNameCounts(t, cli)) == 1 {
			break
		}
	}
	assert.Equal(t, map[string]int{"prefill": 2}, replicaRoleCounts(t, cli))
	assert.Equal(t, map[string]int{"qwen": 2}, groupNameCounts(t, cli),
		"the settled group is the readable one, whole and alone")
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
// GONE IS NOT DEPARTING, and the difference is what the two cases cover. This fixture's Pod is
// already absent, which is what the fake client's Delete produces and what a live cluster reaches
// only after Kueue's finalizer has been released; getting there from an operator's `kubectl delete
// pod` goes through the state the departing-replica case covers, where the replacement waits for
// the ask rather than rebuilding anything.
func TestModelDeployment_HandDeletedReplicaIsRecreatedWithoutARebuild(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// decode loses one replica outright -- no finalizer, so it is GONE rather than departing, and its
	// group is merely short rather than resizing.
	var goneName string
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) == "decode" {
			goneName = pod.Name

			break
		}
	}
	require.NotEmpty(t, goneName, "the fixture must hold a decode replica to lose")
	gone := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: goneName}, gone))
	require.NoError(t, cli.Delete(context.Background(), gone))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Len(t, replicaNames(t, cli), 4, "the survivors stay and the missing one comes back")
	assert.Equal(t, map[string]int{"2": 4}, groupTotals(t, cli))
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
				// The replica reaches the API server nameless, so the failed create is picked out by
				// its rendered prefix: the first decode replica the pass submits.
				if pod, ok := obj.(*core.Pod); ok && pod.GenerateName == "qwen-decode-" && !failed {
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
	ours, err := r.findModelDeploymentGroupWorkloads(context.Background(), md, pods)
	require.NoError(t, err)
	assert.Empty(t, ours, "an incomplete group has no Workload, which is why it needs a reason")

	stored := getModelDeployment(t, cli)
	assert.True(t, ModelDeploymentConditionQuotaReserved.IsFalse(stored))
	assert.Equal(t, "PodGroupIncomplete",
		ModelDeploymentConditionQuotaReserved.GetReason(stored))
	assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(stored), "1 of 2",
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
				// The replica reaches the API server nameless, so the failed create is picked out by
				// its rendered prefix: the first decode replica the pass submits.
				if pod, ok := obj.(*core.Pod); ok && pod.GenerateName == "qwen-decode-" && !failed {
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

// TestModelDeployment_ADepartingReplicaKeepsTheWorkloadAndWaitsForTheAsk covers the departure path
// the generated names opened.
//
// A replica asked to leave cannot leave on its own: Kueue holds a finalizer on every Pod of a group
// annotated serving -- which Kueue defines as never finished -- and releases the departed one only
// once a replacement carrying the same role hash exists. The old design forced the release by
// deleting the Workload, and Kueue answers a deleted Workload by stopping every member; the
// replacement path instead keeps the Workload, reads Kueue's WaitingForReplacementPods ask off it,
// and creates the replacement beside the departing member -- which is exactly what releases it.
//
// THE FIXTURE IS THE FINALIZER. Without it the fake client removes the Pod on Delete, the
// reconciler sees a missing replica rather than a departing one, and a reconciler that still
// demolished the group on every departure would pass this case unread. Measured on a live cluster,
// before the generated names: a template edit left one Pod undeletable, its replacement
// uncreatable because the name was taken, and the reconciler reissuing the same delete every two
// seconds with nothing erroring.
func TestModelDeployment_ADepartingReplicaKeepsTheWorkloadAndWaitsForTheAsk(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// decode loses one member the way a live cluster loses it: the delete lands, Kueue's finalizer
	// holds the Pod, and it stays on the books as a departing member.
	var departingName string
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) == "decode" {
			departingName = pod.Name

			break
		}
	}
	require.NotEmpty(t, departingName, "the fixture must hold a decode replica to lose")
	departing := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: departingName}, departing))
	departing.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, departing))
	require.NoError(t, cli.Delete(ctx, departing))

	// The Workload Kueue composed for the groups: plain owner references to the members, no
	// controller reference, which is the shape the operator has to find it by.
	wl := askingGroupWorkload(replicaPods(t, cli), false)
	require.NoError(t, cli.Create(ctx, wl))

	// The pass that finds the departure creates nothing: Kueue has not yet said the departed member
	// stopped counting, and a replacement created now would read to Kueue as one member over the
	// count -- its answer is to evict the replacement itself.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.NoError(t,
		cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: wl.Name}, new(kueue.Workload)),
		"the Workload stays: releasing the departed member no longer costs the group")
	assert.Len(t, replicaNames(t, cli), 4,
		"the departing member is still held, and nothing is created beside it before the ask")

	// Kueue reads the departure and asks; the pass creates the replacement beside the held member,
	// which is what releases it -- and the sibling role's group is nobody's cost to pay here.
	setGroupAsk(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 3, "prefill": 2}, replicaRoleCounts(t, cli),
		"the departing member still carries its role's label; beside it are its survivor and the "+
			"replacement, and the sibling role paid nothing")
	require.Len(t, replicaNames(t, cli), 5,
		"the departing member is still on the books -- only Kueue releases it -- and the replacement "+
			"was created beside it")
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

// TestModelDeployment_RedistributingReplicasRebuildsEachRolesOwnGroup is the case a type-keyed
// group cannot survive.
//
// prefill 2 / decode 2 becoming prefill 1 / decode 3 moves counts between roles under a
// deployment-wide sum that does not move. One role per group makes each role's total the thing that
// moved, so BOTH groups rebuild -- each on its own predicate -- and no group ever holds a departing
// role's replica beside an arriving one's. An implementation still keyed on the instanceType reads
// one unchanged sum, rebuilds nothing, and lets the converge loop trim and create in place: exactly
// the mixed group the rebuild exists to avoid.
func TestModelDeployment_RedistributingReplicasRebuildsEachRolesOwnGroup(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	moved := getModelDeployment(t, cli)
	moved.Spec.Roles[0].Replicas = 1
	moved.Spec.Roles[1].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), moved))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Empty(t, replicaNames(t, cli),
		"both groups go: each role's total moved, and each group is its role's alone")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"decode": 3, "prefill": 1}, replicaRoleCounts(t, cli))
	assert.Equal(t, map[string]int{"1": 1, "3": 3}, groupTotals(t, cli),
		"each rebuilt group declares its own role's new count")
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
	assert.Equal(t, map[string]int{"decoder": 2, "prefill": 2}, replicaRoleCounts(t, cli),
		"a renamed role is a different group, so its replicas are rebuilt -- and only its: the "+
			"sibling's Pods agree with everything they declare and never move")
}

// newRenderInstanceTypeB is the SECOND InstanceType, and it is what makes every two-group case
// expressible: another name, and another queue entrance so a role placed on it lands in another pool.
//
// The entrance is again a value no derivation produces, for the reason the first fixture states: one
// spelled the way a name-derived render would spell it could not tell a read from a derivation apart.
func newRenderInstanceTypeB() *worker.InstanceType {
	return newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "a100-8x"
		it.Status.Entrance = "queue-for-a100-8x"
	})
}

// twoTypeDeployment puts the two roles on two instanceTypes, which is two groups.
func twoTypeRoleDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	return twoRoleDeployment(append([]func(*workercore.ModelDeployment){
		func(md *workercore.ModelDeployment) { md.Spec.Roles[1].InstanceType = "a100-8x" },
	}, mutate...)...)
}

// groupNameCounts collects how many live replicas carry each group name.
func groupNameCounts(t *testing.T, cli ctrlcli.Client) map[string]int {
	t.Helper()

	counts := make(map[string]int)
	for _, pod := range replicaPods(t, cli) {
		counts[pod.Labels[kueuepodconst.GroupNameLabel]]++
	}

	return counts
}

// TestModelDeployment_TwoTypesAreCreatedAsTwoGroupsInOnePass is F2's obligation for the split shape.
//
// Both groups are created by the SAME pass, for the reason the single-group case already states:
// Kueue composes no Workload for a group short of its declared total, so a reconciler that staged one
// group after the other would leave the first one gated with nothing reporting why.
func TestModelDeployment_TwoTypesAreCreatedAsTwoGroupsInOnePass(t *testing.T) {
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 2, "prefill": 2}, replicaRoleCounts(t, cli),
		"every group's every replica, in the first pass")

	// Two group names, two replicas each -- and each group's total counts only its own role, which is
	// the number Kueue waits for before it composes that group's Workload.
	counts := groupNameCounts(t, cli)
	assert.Len(t, counts, 2, "two instanceTypes cannot be one Workload, so they are two groups")
	for group, n := range counts {
		assert.Equal(t, 2, n, "group %s", group)
	}
	assert.Equal(t, map[string]int{"2": 4}, groupTotals(t, cli),
		"each group declares its own two, not the deployment's four")
}

// TestModelDeployment_OneGroupsChangeLeavesTheOtherAlone is the case a deployment-wide rebuild
// decision fails.
//
// Changing one role's replica count moves ONE group's shape. The other group's replicas agree with
// their own total and their own share, so nothing about them changed -- and restarting them would
// reload a model's weights for an edit that could not reach them.
//
// THE SURVIVING NAMES ARE NOT THE ASSERTION, AND A TEST THAT USED THEM PASSES AGAINST THE BUG.
// A deployment-wide rebuild deletes every replica and then creates the untouched group's back in the
// SAME pass -- its creates are not the ones being held -- so the names afterwards are identical
// either way. Measured: with the per-group decision mutated away to a deployment-wide one, a
// name-based assertion still passed. The UID is what tells a Pod that stayed from one that was
// replaced by an identical one.
func TestModelDeployment_OneGroupsChangeLeavesTheOtherAlone(t *testing.T) {
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// A MARKER THE RENDERER NEVER WRITES is what tells a Pod that stayed from one that was replaced by
	// an identical render. It is used rather than the UID or the resourceVersion because those are the
	// fake client's to assign, and an assertion comparing two values it leaves empty is vacuously
	// true -- measured: a UID-based version of this assertion passed against the mutation it exists to
	// catch.
	const stayed = "test.gpustack.ai/stayed"

	ctx := context.Background()
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) != "decode" {
			continue
		}
		live := pod.DeepCopy()
		live.Annotations[stayed] = "yes"
		require.NoError(t, cli.Update(ctx, live))
	}

	moved := getModelDeployment(t, cli)
	moved.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(ctx, moved))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	// prefill's group comes down whole; decode's is untouched, still holding the very same Pods.
	assert.Equal(t, map[string]int{"decode": 2}, replicaRoleCounts(t, cli),
		"only the group whose shape moved is rebuilt")

	for _, pod := range replicaPods(t, cli) {
		assert.Equal(t, "yes", pod.Annotations[stayed],
			"%s carries a fresh render, so the other group came down and was rebuilt after all", pod.Name)
	}
}

// TestModelDeployment_TeardownRemovesEveryGroupsWorkload covers the half a first-match lookup fails.
//
// Each group has its own Workload and each holds Kueue's finalizer on its OWN replicas. Deleting one
// of them releases one group and leaves the other's replicas unable to leave at all, which strands
// the deployment in Deleting with nothing erroring anywhere.
func TestModelDeployment_TeardownRemovesEveryGroupsWorkload(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaPods(t, cli), 4)

	// One Workload per group, owned by that group's members without a controller reference -- the
	// shape Kueue builds and the one the operator has to find them by. The names are chosen so the
	// decode group's sorts FIRST: a lookup taking the first match would then delete it and leave the
	// prefill group's behind, and a case whose names sorted the other way would not notice.
	byGroup := make(map[string][]core.Pod)
	for _, pod := range replicaPods(t, cli) {
		group := pod.Labels[kueuepodconst.GroupNameLabel]
		byGroup[group] = append(byGroup[group], pod)
	}
	require.Len(t, byGroup, 2)

	names := make([]string, 0, len(byGroup))
	for i, group := range slices.Sorted(maps.Keys(byGroup)) {
		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = fmt.Sprintf("qwen-%d", i), "team-a"
		for _, pod := range byGroup[group] {
			wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
				APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
			})
		}
		require.NoError(t, cli.Create(ctx, wl))
		names = append(names, wl.Name)
	}

	require.NoError(t, cli.Delete(ctx, getModelDeployment(t, cli)))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	for _, name := range names {
		err = cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: name}, new(kueue.Workload))
		assert.True(t, kerrors.IsNotFound(err),
			"every group's Workload goes, not the first: %s is still there", name)
	}
}

// TestModelDeployment_ARebuildDoesNotDelayAnotherGroupsRepair is the half the assertions above cannot
// reach.
//
// Withholding creates is what keeps a replacement from landing beside a member on its way out, and
// that hazard belongs to ONE group. Held for the deployment, a group that merely lost a replica waits
// a pass for a rebuild happening somewhere it cannot reach -- and it waits while short of its own
// declared total, which is the state in which Kueue composes no Workload for it at all.
//
// The untouched group has to be MISSING something for this to be observable: a complete group has no
// create to withhold, which is why every other two-group case here passes either way.
func TestModelDeployment_ARebuildDoesNotDelayAnotherGroupsRepair(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// decode loses one replica outright -- no finalizer, so it is GONE rather than departing, and its
	// group is merely short rather than resizing.
	var goneName string
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) == "decode" {
			goneName = pod.Name

			break
		}
	}
	require.NotEmpty(t, goneName, "the fixture must hold a decode replica to lose")
	gone := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: goneName}, gone))
	require.NoError(t, cli.Delete(ctx, gone))

	// ...while prefill's shape moves, which rebuilds prefill's group and nothing else.
	moved := getModelDeployment(t, cli)
	moved.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(ctx, moved))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 2}, replicaRoleCounts(t, cli),
		"decode's missing replica is created in this pass; holding it would leave that group short "+
			"of its own total for a rebuild it has nothing to do with")
}
