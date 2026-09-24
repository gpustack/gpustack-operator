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
	"k8s.io/apimachinery/pkg/util/sets"
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

// TestModelDeployment_GroupIsCreatedInOnePass covers what a pass owes Kueue before it can
// compose anything at all.
//
// Kueue composes NO Workload for a group it has not fully seen: fewer runnable Pods than the
// declared total is an unretryable compose error. Every group is one replica now, so the group is
// complete the moment its Pod exists -- and the creates for every role's every ordinal are still
// issued in one pass, none of them waiting on another's readiness. A reconciler that staged them
// role by role would leave ordinals missing for as long as the staging took, and the symptom of
// that is nothing at all -- Pods exist, they are gated, and the incomplete one composes no
// Workload.
func TestModelDeployment_GroupIsCreatedInOnePass(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 2, "prefill": 2}, replicaRoleCounts(t, cli),
		"every role's every replica, in the first pass")

	assert.Equal(t, map[string]int{"1": 4}, groupTotals(t, cli),
		"each Pod declares its own one-member group's total, which is the only count there is")
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
		"two PodSets of two, not one of four: the role hash stays the role's, per replica")
	require.Len(t, byGroup, 4, "four replicas are four groups, one member each")
	for group, n := range byGroup {
		assert.Equal(t, 1, n, "group %s holds one replica and nobody else's", group)
	}
}

// TestModelDeployment_ScaleUpKeepsTheSurvivorsAndAddsTheMissingOrdinal covers the property the
// per-replica groups buy: a replicas change moves no total any running Pod carries and no group any
// running Pod is a member of, so the survivors are left exactly as they stand and only the ordinals
// the new count names that no Pod holds are created.
//
// THE SURVIVORS ARE THE SAME OBJECTS, asserted with a marker the renderer never writes -- the
// precedent this file set before the ordinals existed. A name cannot say it: the API server owns
// the names, and a UID the fake client assigns is the environment's to stamp, not the test's, so
// both ends of a same-object comparison can be made to agree without anything having stayed.
func TestModelDeployment_ScaleUpKeepsTheSurvivorsAndAddsTheMissingOrdinal(t *testing.T) {
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"1": 4}, groupTotals(t, cli))

	const stayed = "test.gpustack.ai/stayed"
	ctx := context.Background()
	for _, pod := range replicaPods(t, cli) {
		live := pod.DeepCopy()
		live.Annotations[stayed] = "yes"
		require.NoError(t, cli.Update(ctx, live))
	}

	grown := getModelDeployment(t, cli)
	grown.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(ctx, grown))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	// ONE PASS, no rebuild: the two prefill survivors stay as objects and the third ordinal is
	// created beside them -- a rebuild that deleted every prefill first would leave this count
	// short of three until a later pass, and would drop every marker with it.
	assert.Equal(t, map[string]int{"decode": 2, "prefill": 3}, replicaRoleCounts(t, cli),
		"the grown role gains its third ordinal in the same pass, and the sibling pays nothing")

	marked := 0
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) != "prefill" {
			continue
		}
		if pod.Annotations[stayed] == "yes" {
			marked++
		}
	}
	assert.Equal(t, 2, marked,
		"the two survivors are the very objects the edit found running; only the third is new")
	assert.Equal(t, map[string]int{"1": 5}, groupTotals(t, cli))
}

// TestModelDeployment_ScaleDownRemovesTheDepartingOrdinalsWorkload removes both halves, and the
// Workload half is the point.
//
// A scale-down removes the Pod AND the Workload of the replica whose ordinal the new count no
// longer names, from the high end. The Pod alone would leak: the departing replica's Workload holds
// Kueue's finalizer on it and the quota it was admitted for, and nothing but the Workload's removal
// releases either -- the deployment would sit at its new count while holding the old one's quota,
// with nothing erroring anywhere.
func TestModelDeployment_ScaleDownRemovesTheDepartingOrdinalsWorkload(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// The Workload Kueue composed for each replica: plain owner references to its member, no
	// controller reference, which is the shape the operator has to find them by. The SURVIVOR keeps
	// one -- a scale-down must free the departing quota and nobody else's.
	//
	// THE PODS' UIDs ARE STAMPED BY THE FIXTURE, not the environment: the fake client leaves them
	// empty on a generated-name create, and a Workload lookup that matches by UID would then match
	// EVERY fixture Workload against every Pod -- the scale-down would look precise while deleting
	// them all, or none. An API server assigns real UIDs, so stamped ones are the faithful shape.
	const stayed = "test.gpustack.ai/stayed"
	for _, pod := range replicaPods(t, cli) {
		live := pod.DeepCopy()
		live.UID = types.UID("uid-" + pod.Name)
		live.Annotations[stayed] = "yes"
		require.NoError(t, cli.Update(ctx, live))

		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = "wl-"+pod.Name, pod.Namespace
		wl.OwnerReferences = []meta.OwnerReference{{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: live.UID,
		}}
		require.NoError(t, cli.Create(ctx, wl))
	}

	shrunk := getModelDeployment(t, cli)
	shrunk.Spec.Roles[0].Replicas = 1
	require.NoError(t, cli.Update(ctx, shrunk))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	// One prefill survivor keeps its object; the departing ordinal's Pod AND its Workload are both
	// gone. Which prefill survives is the ordinal's call -- the survivor is whichever carried
	// ordinal 0, asserted by the marker rather than by a name.
	assert.Equal(t, map[string]int{"decode": 2, "prefill": 1}, replicaRoleCounts(t, cli),
		"the shrunk role sheds its highest ordinal, and the sibling pays nothing")

	survivorKept := false
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) != "prefill" {
			continue
		}
		assert.Equal(t, "yes", pod.Annotations[stayed],
			"%s carries a fresh render, so the survivor was rebuilt after all", pod.Name)
		survivorKept = true
	}
	require.True(t, survivorKept)

	// EVERY SURVIVING WORKLOAD BELONGS TO A SURVIVING POD, and that is the leak check: the
	// departing ordinal's Workload went with its Pod, while the survivors' stayed exactly where
	// they were. A pass that deleted only the Pods leaves the same count of survivors but one
	// orphaned Workload holding the departed quota.
	liveNames := sets.New[string]()
	for _, pod := range replicaPods(t, cli) {
		liveNames.Insert(pod.Name)
	}
	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(ctx, wlList, ctrlcli.InNamespace("team-a")))
	require.Len(t, wlList.Items, liveNames.Len(),
		"one Workload per surviving replica: the departed ordinal's went with its Pod")
	for i := range wlList.Items {
		owner := wlList.Items[i].OwnerReferences[0].Name
		assert.Truef(t, liveNames.Has(owner),
			"workload %s outlives its Pod %s: the scale-down leaked it", wlList.Items[i].Name, owner)
	}
}

// TestModelDeployment_RoleSetChangeSweepsTheRemovedRoleAlone covers the other axis: removing a role
// takes that role's replicas and Workloads down at once, and the survivor's Pods are left exactly
// where they stand.
//
// THE SURVIVOR'S GROUP NAMES NEVER MOVED, which is the property the per-role derivation could not
// offer: a role's group names are its own and not a function of how many roles the deployment
// declares, so losing a sibling renames nothing and rolls nothing.
func TestModelDeployment_RoleSetChangeSweepsTheRemovedRoleAlone(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// A Workload per replica, so the sweep's obligation covers them: the removed role's replicas
	// take theirs along, and the survivor's stay admitted. The pods' UIDs are stamped by the
	// fixture, for the reason the scale-down case above states: empty UIDs make an ownership match
	// answer every Workload for every Pod.
	const stayed = "test.gpustack.ai/stayed"
	for _, pod := range replicaPods(t, cli) {
		live := pod.DeepCopy()
		live.UID = types.UID("uid-" + pod.Name)
		live.Annotations[stayed] = "yes"
		require.NoError(t, cli.Update(ctx, live))

		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = "wl-"+pod.Name, pod.Namespace
		wl.OwnerReferences = []meta.OwnerReference{{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: live.UID,
		}}
		require.NoError(t, cli.Create(ctx, wl))
	}

	shrunk := getModelDeployment(t, cli)
	shrunk.Spec.Roles = shrunk.Spec.Roles[:1]
	require.NoError(t, cli.Update(ctx, shrunk))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"prefill": 2}, replicaRoleCounts(t, cli),
		"the removed role's replicas go in the first pass, and the survivor's are nobody's business")

	for _, pod := range replicaPods(t, cli) {
		assert.Equal(t, "yes", pod.Annotations[stayed],
			"%s carries a fresh render, so the surviving role turned over after all", pod.Name)
	}

	// The survivor's Workloads stay admitted; the removed role's are swept with their Pods. A
	// Workload whose owner is gone is the leak: it holds quota and a finalizer on nothing.
	live := replicaPods(t, cli)
	liveNames := sets.New[string]()
	for _, pod := range live {
		liveNames.Insert(pod.Name)
	}
	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(ctx, wlList, ctrlcli.InNamespace("team-a")))
	require.Len(t, wlList.Items, len(live),
		"one Workload per surviving replica, and none for the removed role's")
	for i := range wlList.Items {
		owner := wlList.Items[i].OwnerReferences[0].Name
		assert.Truef(t, liveNames.Has(owner),
			"workload %s outlives its Pod %s: the sweep left it behind", wlList.Items[i].Name, owner)
	}
}

// TestModelDeployment_AnOrdinalLessPreExistingPodRollsOutRatherThanBeingAdopted covers a Pod that
// carries no ordinal label while holding a Workload of its own.
//
// SUCH A POD IS NOT ANY SLOT'S TO CLAIM -- adopting it onto an ordinal it never carried would key
// the hash comparison on a guess -- so it is judged outdated instead and turns over on the ordinary
// rollout cadence: one per pass, the count healing between departures, and the replacements arrive
// carrying the label. The alternative the cadence rules out is tearing them all out at once, which
// is an outage the turnover does not need to be.
//
// WHAT THIS DOES NOT COVER IS A ROLE WHOSE PODS SHARE ONE WORKLOAD, which is the other side of the
// same arithmetic and has a case of its own: a Pod holding a Workload nobody else answers to makes
// the admitted count LONG against the declared one, and Pods pooled under a single Workload make it
// short. Both are the correspondence breaking rather than two defects, and both are repaired by the
// same exception -- so this case cannot tell on its own whether that exception is wired up, because
// its own numbers happen to agree. TestModelDeployment_ARoleWhosePodsShareOneWorkloadIsRepaired is
// the one that fails when it is not.
func TestModelDeployment_AnOrdinalLessPreExistingPodRollsOutRatherThanBeingAdopted(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)
	// The turnover's currency is an admitted replica, so the fixture starts with every replica's
	// Workload admitted and the stand-in keeps them so as the count moves -- without an admission
	// to trade on, the guard holds the aged pods in place forever and this walk never starts.
	standInForKueue(t, cli, true)

	// Two pre-upgrade decode pods: no ordinal label, everything else as the pass rendered it.
	// NOTE that a real pre-upgrade Pod also carries the OLD group name, and this fixture keeps the
	// new one -- so what it reproduces is the missing ordinal alone, not the whole pre-upgrade shape.
	// That is deliberate, because the missing ordinal is what the adoption rule turns on; but it
	// means this case cannot tell the create gate's two candidate selectors apart the way a real
	// upgrade would, where neither the old group name nor the absent ordinal matches and both
	// selectors read the slot as empty.
	aged := 0
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) != "decode" {
			continue
		}
		live := pod.DeepCopy()
		delete(live.Labels, modelDeploymentReplicaOrdinalLabel)
		require.NoError(t, cli.Update(ctx, live))
		aged++
	}
	require.Equal(t, 2, aged)

	// The turnover: one departure per pass, and a create between departures. The names never drop
	// below one per role, and what settles carries the ordinal label the aged pods lacked. A
	// replacement never lands beside an aged member: the create gate reads the ordinal's group
	// occupied until the aged holder has gone, which is what keeps the turnover one-at-a-time.
	for pass := 0; pass < 8; pass++ {
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err, "pass %d", pass)
		standInForKueue(t, cli, true)

		counts := replicaRoleCounts(t, cli)
		require.GreaterOrEqual(t, counts["decode"], 1,
			"pass %d: the turnover never empties the role", pass)
		require.Equal(t, 2, counts["prefill"], "pass %d: the sibling is nobody's cost", pass)

		settled := counts["decode"] == 2
		if settled {
			for _, pod := range replicaPods(t, cli) {
				if modelDeploymentPodRole(&pod) != "decode" {
					continue
				}
				if _, ok := modelDeploymentPodOrdinal(&pod); !ok {
					settled = false
				}
			}
		}
		if settled {
			return
		}
	}

	t.Fatal("the ordinal-less pods never turned over onto the per-replica groups")
}

// TestModelDeployment_ARoleWhosePodsShareOneWorkloadIsRepaired covers the whole pre-per-replica
// shape rather than the missing ordinal alone: no Pod carries an ordinal, and ONE group -- named
// after the role -- holds all of them, so the role's Pods answer to a single Workload.
//
// THE ROLLOUT GUARD CANNOT READ THAT SHAPE, and the arithmetic is why. It holds until every
// declared replica holds an admitted Workload, counting Workloads on one side and replicas on the
// other; a Pod that claims no ordinal is a replica to neither count. Sharing one Workload makes the
// count short -- two Pods, one admission, two declared -- while a Pod holding one of its own makes
// it long, and either way the comparison is against a number the role can never reach. The removal
// that would repair it is gated behind the same comparison, so the role settles into a state it
// cannot leave and every later edit to it is silently dropped.
//
// THE ASSERTION IS THAT THE ROLE RECOVERS WITHOUT HELP. What repairs it is that a Pod claiming no
// ordinal is turned over WITHOUT waiting for the guard, on the same terms as a replica short of its
// members: neither can ever be admitted as a declared replica, so waiting on that admission is
// waiting for something that cannot arrive.
func TestModelDeployment_ARoleWhosePodsShareOneWorkloadIsRepaired(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)
	standInForKueue(t, cli, true)

	// Rewrite decode into the pre-per-replica shape, and take the per-replica Workloads with it:
	// the groups they were composed for no longer exist, and leaving them standing would let the
	// guard count admissions no Pod answers to.
	perReplica := sets.New[string]()
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) != "decode" {
			continue
		}
		perReplica.Insert(pod.Labels[kueuepodconst.GroupNameLabel])
		aged := pod.DeepCopy()
		delete(aged.Labels, modelDeploymentReplicaOrdinalLabel)
		aged.Labels[kueuepodconst.GroupNameLabel] = "qwen-decode"
		require.NoError(t, cli.Update(ctx, aged))
	}
	require.Equal(t, 2, perReplica.Len(), "decode started as two replicas in two groups")

	wlList := new(kueue.WorkloadList)
	require.NoError(t, cli.List(ctx, wlList, ctrlcli.InNamespace("team-a")))
	for i := range wlList.Items {
		if perReplica.Has(wlList.Items[i].Name) {
			require.NoError(t, cli.Delete(ctx, &wlList.Items[i]))
		}
	}
	// One admitted Workload for the one group the role now holds -- which is the shape, and the
	// number the guard reads as a single admission against two declared replicas.
	standInForKueue(t, cli, true)

	for pass := range 12 {
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err, "pass %d", pass)
		standInForKueue(t, cli, true)

		require.Equal(t, 2, replicaRoleCounts(t, cli)["prefill"],
			"pass %d: the sibling role is nobody's cost", pass)

		seated := 0
		for _, pod := range replicaPods(t, cli) {
			if modelDeploymentPodRole(&pod) != "decode" {
				continue
			}
			if _, ok := modelDeploymentPodOrdinal(&pod); ok {
				seated++
			} else {
				seated = -len(replicaPods(t, cli))
			}
		}
		if seated == 2 {
			return
		}
	}

	t.Fatal("the role never left the shared-workload shape: no path replaces a Pod that claims no " +
		"ordinal while the guard compares an admission count it can never reach, and the same " +
		"comparison gates the removal that would repair it")
}

// TestModelDeployment_GroupIsIdempotent pins that the rebuild predicate does not fire on a spec that
// has not moved.
//
// This is the failure mode a converge policy invites: a pass that answers "something moved" on
// every pass deletes and recreates replicas forever, and each cycle looks, from a single pass,
// exactly like a legitimate rollout.
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

	assert.Len(t, replicaNames(t, cli), 4, "the survivors stay and the missing ordinal comes back")
	assert.Equal(t, map[string]int{"1": 4}, groupTotals(t, cli))
}

// TestModelDeploymentPodGroupIncomplete_IsAReportedStateNotASilentOne covers it at the level this
// tree can reach.
//
// The failure mode of an incomplete group is SILENCE: a group short of its declared total has Pods, they are gated, Kueue
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

	// And none of them is ours, which is the state an incomplete group leaves behind.
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

// TestModelDeployment_ADepartingReplicaIsNotReplacedUntilItsOrdinalReadsEmpty covers the departure
// path the generated names opened.
//
// A replica asked to leave cannot leave on its own: Kueue holds a finalizer on every Pod of a group
// annotated serving -- which Kueue defines as never finished -- so the deleted Pod stays on the
// books, still a member of its ordinal's group, until its Workload is deleted and the drain
// completes. The replacement waits out exactly that: the create gate reads the ordinal's group on
// the API server and creates only once no member reads, and Kueue's WaitingForReplacementPods ask
// is staged mid-wait to show it gates nothing -- the member's existence is the whole of the gate,
// and a replacement created beside it would be the excess member Kueue deletes first.
//
// THE FIXTURE IS THE FINALIZER. Without it the fake client removes the Pod on Delete, the
// reconciler sees a missing replica rather than a departing one, and a reconciler that created
// beside a departing member would pass this case unread. Measured on a live cluster, before the
// generated names: a template edit left one Pod undeletable, its replacement uncreatable because
// the name was taken, and the reconciler reissuing the same delete every two seconds with nothing
// erroring.
func TestModelDeployment_ADepartingReplicaIsNotReplacedUntilItsOrdinalReadsEmpty(t *testing.T) {
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

	// The pass that finds the departure creates nothing: the departing member still reads in its
	// ordinal's group, and a replacement beside it is the excess member Kueue deletes first.
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.NoError(t,
		cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: wl.Name}, new(kueue.Workload)),
		"the Workload stays: nothing of this deployment's own deletes it while its members stand")
	assert.Len(t, replicaNames(t, cli), 4,
		"the departing member is still held, and nothing is created beside it")

	// Kueue reads the departure and asks -- and the ask changes nothing, because the member still
	// reads in the group and that is the whole of the gate.
	setGroupAsk(t, cli, true)
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Len(t, replicaNames(t, cli), 4,
		"the ask reading True does not release the replacement: the departing member is still a "+
			"member of the ordinal's group, whatever the condition on the Workload says")

	// The departure completes -- only Kueue releases the finalizer, on its own clock -- and the
	// object leaves; the pass that reads the ordinal empty creates the replacement, and the
	// sibling role's group is nobody's cost to pay here.
	released := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: departingName}, released))
	released.Finalizers = nil
	require.NoError(t, cli.Update(ctx, released))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 2, "prefill": 2}, replicaRoleCounts(t, cli),
		"the departing member is gone and its replacement stands beside the survivor, and the "+
			"sibling role paid nothing")
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

// TestModelDeployment_AReplicaNothingHereSentAwayGetsItsWorkloadReleased covers the departure that
// no path of this operator's own initiates: a hand deleting the Pod, a node drain, an eviction.
//
// THE FIXTURE IS ONE WORKLOAD PER REPLICA, and that is what the cases above cannot express. They
// pool every Pod under a single Workload, so a member is always left standing beside the departing
// one and a Workload is always still meaningful. A group of one has no such sibling: its Workload
// outlives the group it was composed for, owning nothing but a Pod that is trying to leave.
//
// WITHOUT THE RELEASE THE ORDINAL IS HELD INDEFINITELY, and neither terminal phase recovers on its
// own. Measured on a live cluster: a Pod deleted this way was still present, finalizered, with its
// Workload still admitted 100 seconds later; a container killed past its grace period reaches
// Failed, which Kueue counts as inactive, while one exiting cleanly on SIGTERM reaches Succeeded,
// which Kueue counts as ACTIVE -- and a replacement created beside that one is the excess member
// Kueue deletes, so waiting it out and creating around it both fail. Deleting the Workload is what
// releases the finalizer, and it is the only remedy that covers both phases.
func TestModelDeployment_AReplicaNothingHereSentAwayGetsItsWorkloadReleased(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaPods(t, cli), 2)

	// THE UIDs ARE STAMPED BECAUSE THE FAKE CLIENT ASSIGNS NONE, and an ownerReference matches on
	// one. Two Pods carrying no UID are one Pod to every reader of a Workload's owners, so the
	// stranded Workload and the surviving replica's would be indistinguishable and this case could
	// not fail whatever the reconciler did.
	for _, pod := range replicaPods(t, cli) {
		stamped := pod.DeepCopy()
		stamped.UID = types.UID("uid-" + pod.Name)
		require.NoError(t, cli.Update(ctx, stamped))
	}
	pods := replicaPods(t, cli)

	for i := range pods {
		require.NoError(t, cli.Create(ctx,
			replicaGroupWorkload(pods[i].Labels[kueuepodconst.GroupNameLabel], pods[i])))
	}

	// The replica leaves the way a hand or a drain takes it: the delete lands, Kueue's finalizer
	// holds the Pod, and nothing of this operator's own was what asked for it.
	departing := pods[0].DeepCopy()
	departing.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, departing))
	require.NoError(t, cli.Delete(ctx, departing))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.True(t, kerrors.IsNotFound(cli.Get(ctx, ctrlcli.ObjectKey{
		Namespace: "team-a", Name: pods[0].Labels[kueuepodconst.GroupNameLabel],
	}, new(kueue.Workload))),
		"the stranded Workload is deleted: it owns nobody still standing, and deleting it is the "+
			"only thing that releases the finalizer holding the ordinal")

	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Namespace: "team-a", Name: pods[1].Labels[kueuepodconst.GroupNameLabel],
	}, new(kueue.Workload)),
		"the surviving replica's Workload is untouched: Kueue stops the group of a deleted "+
			"Workload, so a member still serving is one this path must never reach")

	// Kueue releases the finalizer once the Workload is gone, on its own clock, and the ordinal
	// reads empty to the create gate.
	released := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: pods[0].Name}, released))
	released.Finalizers = nil
	require.NoError(t, cli.Update(ctx, released))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Len(t, replicaNames(t, cli), 2,
		"the replacement lands on the freed ordinal, which is what the release bought")
}

// TestModelDeployment_AWorkloadWithAMemberStillStandingSurvivesADeparture is the other half of the
// release above, and the only shape that can tell the two apart: ONE Workload owning a departing
// member AND a standing one.
//
// The release deletes a Workload that owns nobody still standing. In the case above every Workload
// owns exactly one Pod, so that condition is false for every Workload it could reach and the case
// cannot see whether it is read at all. Here it is the whole question -- and Kueue answers a deleted
// Workload by stopping its group, so deleting this one would take a serving member down to free the
// ordinal of a member already gone.
//
// THE UIDs ARE STAMPED FOR THE REASON THE CASE ABOVE STAMPS THEM, and it matters more here: with
// none, the standing member is read through an empty UID that the departing member carries too, so
// the guard would answer correctly for a reason that has nothing to do with anybody standing.
func TestModelDeployment_AWorkloadWithAMemberStillStandingSurvivesADeparture(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(newRenderDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaPods(t, cli), 2)

	for _, pod := range replicaPods(t, cli) {
		stamped := pod.DeepCopy()
		stamped.UID = types.UID("uid-" + pod.Name)
		require.NoError(t, cli.Update(ctx, stamped))
	}
	pods := replicaPods(t, cli)

	require.NoError(t, cli.Create(ctx, replicaGroupWorkload("shared-group", pods...)))

	departing := pods[0].DeepCopy()
	departing.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, departing))
	require.NoError(t, cli.Delete(ctx, departing))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.NoError(t, cli.Get(ctx,
		ctrlcli.ObjectKey{Namespace: "team-a", Name: "shared-group"}, new(kueue.Workload)),
		"the Workload stays while a member of it is still standing: deleting it would stop that "+
			"member too, which is a serving replica paying for a departed one")
}

// TestModelDeployment_AFailedReplicaIsReplaced covers the replica the kubelet ends without deleting
// it: an eviction under node pressure, or a Pod the kubelet rejects at admission. Either leaves the
// Pod in phase Failed with no deletion timestamp, and a Failed Pod never runs again.
//
// LEFT ALONE IT HOLDS ITS ORDINAL FOREVER. It still carries its ordinal and the current render's
// hash, so a pass reading only those counts it as a current replica: nothing is created for the
// ordinal, and the rollout axis reports every replica up to date while the role serves one short.
//
// THE UIDs ARE STAMPED for the reason the stranded-Workload case above states: with none, every Pod
// is the same owner to every Workload, and the case could not tell the failed replica's Workload from
// its sibling's.
func TestModelDeployment_AFailedReplicaIsReplaced(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaPods(t, cli), 4)

	for _, pod := range replicaPods(t, cli) {
		stamped := pod.DeepCopy()
		stamped.UID = types.UID("uid-" + pod.Name)
		require.NoError(t, cli.Update(ctx, stamped))
	}
	pods := replicaPods(t, cli)
	for i := range pods {
		require.NoError(t, cli.Create(ctx,
			replicaGroupWorkload(pods[i].Labels[kueuepodconst.GroupNameLabel], pods[i])))
	}

	// decode loses one replica the way the kubelet takes it: phase Failed, reason Evicted, Kueue's
	// finalizer still on it, and no delete issued by anyone.
	var failed, sibling *core.Pod
	for i := range pods {
		if modelDeploymentPodRole(&pods[i]) != "decode" {
			continue
		}
		if failed == nil {
			failed = pods[i].DeepCopy()
		} else {
			sibling = pods[i].DeepCopy()
		}
	}
	require.NotNil(t, failed, "the fixture must hold a decode replica to lose")
	require.NotNil(t, sibling, "the fixture must hold a second decode replica to leave alone")
	failed.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, failed))
	failed.Status.Phase = core.PodFailed
	failed.Status.Reason = _PodReasonEvicted
	failed.Status.Message = "Pod was rejected: The node had condition: [DiskPressure]."
	require.NoError(t, cli.Status().Update(ctx, failed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	observed := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: failed.Name}, observed))
	assert.NotNil(t, observed.DeletionTimestamp,
		"the failed replica is deleted: it never runs again, and while it stands its ordinal reads taken")
	assert.True(t, kerrors.IsNotFound(cli.Get(ctx, ctrlcli.ObjectKey{
		Namespace: "team-a", Name: failed.Labels[kueuepodconst.GroupNameLabel],
	}, new(kueue.Workload))),
		"the failed replica's Workload is deleted: it is what releases Kueue's finalizer on the Pod")
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Namespace: "team-a", Name: sibling.Labels[kueuepodconst.GroupNameLabel],
	}, new(kueue.Workload)),
		"the sibling replica's Workload is untouched: nothing about it failed")

	md := new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen"}, md))
	assert.Equal(t, "False", ModelDeploymentConditionReplicasUpToDate.GetStatus(md),
		"a role serving one replica short does not read as up to date: %s",
		ModelDeploymentConditionReplicasUpToDate.GetMessage(md))

	// Kueue releases the finalizer once the Workload is gone, and the ordinal reads empty.
	observed.Finalizers = nil
	require.NoError(t, cli.Update(ctx, observed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 2, "prefill": 2}, replicaRoleCounts(t, cli),
		"the replacement lands on the freed ordinal beside the untouched sibling")
	for _, pod := range replicaPods(t, cli) {
		assert.NotEqual(t, core.PodFailed, pod.Status.Phase, "no failed replica is left standing")
	}
}

// TestModelDeployment_AFailedMemberTurnsItsReplicaOver is the multi-Member shape of the case above.
// A replica's Members share one Kueue group, so one Failed Member condemns the replica: deleting the
// group's Workload to release the failed Member makes Kueue stop the survivors anyway, and a Member
// seated alone beside them would join a group whose collective has already lost a rank.
func TestModelDeployment_AFailedMemberTurnsItsReplicaOver(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaPods(t, cli), 2, "the instance is two Members before anything fails")

	pods := replicaPods(t, cli)
	failed := pods[1].DeepCopy()
	failed.Status.Phase = core.PodFailed
	failed.Status.Reason = _PodReasonEvicted
	require.NoError(t, cli.Status().Update(ctx, failed))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Empty(t, replicaPods(t, cli),
		"both Members leave: the failed one, and the survivor whose group lost it")

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	rebuilt := replicaPods(t, cli)
	assert.Len(t, rebuilt, 2, "the replica comes back whole")
	for _, pod := range rebuilt {
		assert.NotEqual(t, core.PodFailed, pod.Status.Phase, "no failed Member is left standing")
	}
}

// TestModelDeployment_RedistributingReplicasMovesEachRolesOwnOrdinals is the case a type-keyed
// group cannot survive.
//
// prefill 2 / decode 2 becoming prefill 1 / decode 3 moves counts between roles under a
// deployment-wide sum that does not move. Per-replica groups make each role's ordinals the thing
// that moved: prefill sheds its highest slot, decode gains its third, and every survivor stands
// exactly where it was -- one pass, no teardown, no mixed group, because there is no group for a
// departing and an arriving replica to share any more.
func TestModelDeployment_RedistributingReplicasMovesEachRolesOwnOrdinals(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoRoleDeployment(), newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	const stayed = "test.gpustack.ai/stayed"
	for _, pod := range replicaPods(t, cli) {
		live := pod.DeepCopy()
		live.Annotations[stayed] = "yes"
		require.NoError(t, cli.Update(ctx, live))
	}

	moved := getModelDeployment(t, cli)
	moved.Spec.Roles[0].Replicas = 1
	moved.Spec.Roles[1].Replicas = 3
	require.NoError(t, cli.Update(ctx, moved))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"decode": 3, "prefill": 1}, replicaRoleCounts(t, cli),
		"both moves land in the SAME pass: the shed slot and the gained slot are different "+
			"replicas, and neither is the other's group to wait for")
	assert.Equal(t, map[string]int{"1": 4}, groupTotals(t, cli),
		"a total is a replica's own one, so no count any Pod carries moved at all")

	marked := 0
	for _, pod := range replicaPods(t, cli) {
		if pod.Annotations[stayed] == "yes" {
			marked++
		}
	}
	assert.Equal(t, 3, marked,
		"decode's two survivors and prefill's one are the very objects the edit found; decode's "+
			"third is new and carries no marker")
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
		"a renamed role names its replicas to groups nothing forms any more, so they are swept and "+
			"rebuilt -- and only its: the sibling's replicas agree with every group they declare "+
			"and never move")
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

// TestModelDeployment_TwoTypesAreCreatedAsTwoGroupsInOnePass is the same obligation for the split
// shape.
//
// Both roles' every ordinal is created by the SAME pass, for the reason the single-group case
// already states: Kueue composes no Workload for a group it has not seen its member of, so a
// reconciler that staged one role after the other would leave the first one gated with nothing
// reporting why.
func TestModelDeployment_TwoTypesAreCreatedAsTwoGroupsInOnePass(t *testing.T) {
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 2, "prefill": 2}, replicaRoleCounts(t, cli),
		"every role's every replica, in the first pass")

	// Four group names, one replica each -- and each group's total counts its own replica only,
	// which is the number Kueue waits for before it composes that group's Workload.
	counts := groupNameCounts(t, cli)
	require.Len(t, counts, 4, "four replicas are four groups, whatever the instanceTypes beneath")
	for group, n := range counts {
		assert.Equal(t, 1, n, "group %s", group)
	}
	assert.Equal(t, map[string]int{"1": 4}, groupTotals(t, cli),
		"each group declares one member, not the deployment's four")
}

// TestModelDeployment_OneRolesScaleLeavesEverySurvivorAlone is the case a deployment-wide decision
// fails, sharpened by the per-replica groups.
//
// Changing one role's replica count names ordinals only that role owns. The other role's replicas
// agree with every group they declare, and so do the scaled role's SURVIVORS: a scale-up creates the
// slot it adds and touches nothing else. Restarting any of them would reload a model's weights for
// an edit that could not reach them.
//
// THE SURVIVING OBJECTS ARE NOT THE NAMES, AND A TEST THAT USED THEM PASSES AGAINST THE BUG.
// A rebuild that deletes every replica and creates the untouched role's back in the SAME pass
// leaves the names identical either way. Measured: with the per-role decision mutated away to a
// deployment-wide one, a name-based assertion still passed. A marker the renderer never writes is
// what tells a Pod that stayed from one that was replaced by an identical render -- the UID is no
// better, for the reason the fake client states: it assigns nothing on a generated-name create.
func TestModelDeployment_OneRolesScaleLeavesEverySurvivorAlone(t *testing.T) {
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// A MARKER THE RENDERER NEVER WRITES is what tells a Pod that stayed from one that was replaced
	// by an identical render. It is used rather than the UID or the resourceVersion because those
	// are the fake client's to assign, and an assertion comparing two values it leaves empty is
	// vacuously true -- measured: a UID-based version of this assertion passed against the mutation
	// it exists to catch.
	const stayed = "test.gpustack.ai/stayed"

	ctx := context.Background()
	for _, pod := range replicaPods(t, cli) {
		live := pod.DeepCopy()
		live.Annotations[stayed] = "yes"
		require.NoError(t, cli.Update(ctx, live))
	}

	moved := getModelDeployment(t, cli)
	moved.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(ctx, moved))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	// The scaled role gains its third ordinal; nobody else's object moves.
	assert.Equal(t, map[string]int{"decode": 2, "prefill": 3}, replicaRoleCounts(t, cli),
		"the scaled role gains its slot, and the sibling pays nothing")

	marked := 0
	for _, pod := range replicaPods(t, cli) {
		if pod.Annotations[stayed] == "yes" {
			marked++
		}
	}
	assert.Equal(t, 4, marked,
		"the two decode replicas AND the two prefill survivors are the very objects the edit found; "+
			"prefill's third is new and carries no marker")
}

// TestModelDeployment_TeardownRemovesEveryReplicasWorkload covers the half a first-match lookup
// fails.
//
// Each replica has its own Workload and each holds Kueue's finalizer on its OWN Pod. Deleting one
// of them releases one replica and leaves the others unable to leave at all, which strands the
// deployment in Deleting with nothing erroring anywhere.
func TestModelDeployment_TeardownRemovesEveryReplicasWorkload(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaPods(t, cli), 4)

	// One Workload per replica, owned by its member without a controller reference -- the shape
	// Kueue builds and the one the operator has to find them by. The names are chosen so the
	// decode replicas' sort FIRST: a lookup taking the first match would then delete it and leave
	// the others behind, and a case whose names sorted the other way would not notice.
	byGroup := make(map[string][]core.Pod)
	for _, pod := range replicaPods(t, cli) {
		group := pod.Labels[kueuepodconst.GroupNameLabel]
		byGroup[group] = append(byGroup[group], pod)
	}
	require.Len(t, byGroup, 4, "four replicas, four one-member groups")

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
			"every replica's Workload goes, not the first: %s is still there", name)
	}
}

// TestModelDeployment_ARolesScaleDoesNotDelayAnotherRolesRepair is the half the assertions above
// cannot reach.
//
// Withholding creates is what keeps a replacement from landing beside a member on its way out, and
// that hazard belongs to ONE ordinal of ONE role. Held for the deployment, a role that merely lost
// a replica would wait a pass for a scale happening somewhere it cannot reach -- and it would wait
// while short of its own count, which is the state in which Kueue composes no Workload for its
// missing ordinal at all.
//
// The other role has to be MISSING something for this to be observable: a complete role has no
// create to withhold, which is why every other two-role case here passes either way.
func TestModelDeployment_ARolesScaleDoesNotDelayAnotherRolesRepair(t *testing.T) {
	ctx := context.Background()
	cli := newModelDeploymentClient(twoTypeRoleDeployment(),
		newRenderInstanceType(), newRenderInstanceTypeB())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 4)

	// decode loses one replica outright -- no finalizer, so it is GONE rather than departing, and
	// its role is merely short rather than shedding.
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

	// ...while prefill's count moves, which scales prefill's ordinals and nothing else.
	moved := getModelDeployment(t, cli)
	moved.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(ctx, moved))

	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"decode": 2, "prefill": 3}, replicaRoleCounts(t, cli),
		"decode's missing ordinal is created in this pass and prefill's third with it; holding "+
			"either would leave a role short of its own count for a scale it has nothing to do with")
}

// TestModelDeployment_AReplicaShortOneMemberIsRepaired is the state a ReplicaGroup reaches when one
// of its Members is lost and the others are not: a create that failed after a sibling succeeded, a
// node that took one Pod, an eviction.
//
// IT IS THE ONE STATE NOTHING ELSE REACHES. A whole replica disappearing is an empty ordinal, which
// the create gate fills; a replica that is merely outdated is a whole group the rollout turns over.
// This is neither: the ordinal is occupied, so no create is owed, and the group is incomplete, so
// Kueue composes NO Workload for it -- which is exactly what any guard reading "is this role fully
// admitted" cannot see past.
//
// THE ASSERTION IS THAT THE ROLE RECOVERS WITHOUT HELP, and it is written over repeated passes
// rather than one, because a level-based converger is allowed to take more than one pass to reach a
// state. What it is not allowed to do is settle into one it cannot leave.
func TestModelDeployment_AReplicaShortOneMemberIsRepaired(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaPods(t, cli), 2, "the instance is two Members before anything is lost")

	// Lose ONE member. Deleted outright rather than marked terminating, because the question is what
	// the converger does about a Member that is gone and an ordinal that is still occupied.
	pods := replicaPods(t, cli)
	require.NoError(t, cli.Delete(ctx, &pods[1]))
	require.Len(t, replicaPods(t, cli), 1, "exactly one Member is left standing")

	// Several passes, because repair may legitimately take more than one -- a replacement can be
	// created only once the replica it belongs to has finished leaving.
	for range 8 {
		_, err = reconcileModelDeployment(t, cli)
		require.NoError(t, err)
	}

	assert.Len(t, replicaPods(t, cli), 2,
		"the role never returned to two Members: an ordinal holding one Member of two is owed no "+
			"create, and the rollout that was to repair it cannot run while the incomplete group "+
			"has no admitted Workload -- so the role serves nothing and no later spec edit can roll")
}
