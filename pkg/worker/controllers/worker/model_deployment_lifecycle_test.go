package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"
	kueuepod "sigs.k8s.io/kueue/pkg/controller/jobs/pod"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// These are the observable lifecycle cases: each drives real reconcile passes over a fake cluster
// and asserts what a reader could see afterwards. The function-level composition controls in the
// sibling file cannot stand in for them, because what they prove is that a check rejects a
// Workload, not that the controller reaches the point where the check runs.

// lifecycleFixture drives a deployment through passes, with the Kueue handshake between them.
type lifecycleFixture struct {
	t  *testing.T
	md *workercore.ModelDeployment
	// cli is the watch-capable client the fake client builds, so a case can wrap the very one the
	// passes ran against rather than a second one over an empty cluster.
	cli  ctrlcli.WithWatch
	past [][]string
	// allocated counts the identities this fixture has handed out, which is what keeps two
	// incarnations of one ordinal from being the same object to every comparison made here.
	allocated int
}

func newLifecycleFixture(t *testing.T, md *workercore.ModelDeployment, extra ...ctrlcli.Object) *lifecycleFixture {
	t.Helper()

	return newLifecycleFixtureAround(t, md, newCompositionClient, extra...)
}

// newLifecycleFixtureAround builds the fixture over a client the case builds itself, so a write can
// be made to fail the way a real one does rather than described in prose.
func newLifecycleFixtureAround(
	t *testing.T, md *workercore.ModelDeployment, build func(...ctrlcli.Object) ctrlcli.WithWatch,
	extra ...ctrlcli.Object,
) *lifecycleFixture {
	t.Helper()

	objs := make([]ctrlcli.Object, 0, 2+len(extra))
	objs = append(objs, md, newRenderInstanceType())
	objs = append(objs, extra...)
	// An object read from a server always carries a resource version, and the lifecycle writes its
	// replacement intent through a real optimistic lock that needs one to compare against.
	if md.ResourceVersion == "" {
		md.ResourceVersion = "1"
	}
	cli := build(objs...)
	stamped := getModelDeployment(t, cli)
	stamped.ResourceVersion = "1"
	require.NoError(t, cli.Update(context.Background(), stamped))

	f := &lifecycleFixture{t: t, md: md}
	f.cli = ctrlinterceptor.NewClient(cli, ctrlinterceptor.Funcs{
		Create: func(ctx context.Context, next ctrlcli.WithWatch, obj ctrlcli.Object,
			opts ...ctrlcli.CreateOption,
		) error {
			switch obj.(type) {
			case *core.Pod:
				if obj.GetUID() == "" {
					obj.SetUID(f.nextUID("pod"))
				}
			case *kueue.Workload:
				if obj.GetUID() == "" {
					obj.SetUID(f.nextUID("wl"))
				}
			}
			return next.Create(ctx, obj, opts...)
		},
	})
	return f
}

// nextUID allocates an identity the way an API server would, once per object rather than once per
// name: two Pods of one ordinal and two Workloads of one group name are told apart by this and
// nothing else.
func (f *lifecycleFixture) nextUID(kind string) types.UID {
	f.allocated++

	return types.UID(fmt.Sprintf("%s-%03d", kind, f.allocated))
}

// groupsByName returns the live members of every group standing in the cluster, keyed by the group
// name the members carry.
func (f *lifecycleFixture) groupsByName() map[string][]*core.Pod {
	f.t.Helper()

	groups := map[string][]*core.Pod{}
	for _, pod := range replicaPods(f.t, f.cli) {
		if pod.DeletionTimestamp != nil {
			continue
		}
		group := pod.Labels[kueuepodconst.GroupNameLabel]
		member := pod
		groups[group] = append(groups[group], &member)
	}
	for group := range groups {
		slices.SortFunc(groups[group], func(a, b *core.Pod) int { return strings.Compare(a.Name, b.Name) })
	}

	return groups
}

// workloadNamed returns the Workload holding a group name, or nil when the name is free.
func (f *lifecycleFixture) workloadNamed(group string) *kueue.Workload {
	f.t.Helper()

	wl := new(kueue.Workload)
	if err := f.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: group}, wl); err != nil {
		return nil
	}

	return wl
}

// pass constructs a new reconciler on every call, including the restart cases.
func (f *lifecycleFixture) pass(_ bool) {
	f.t.Helper()

	r := &ModelDeploymentReconciler{
		Client: f.cli, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64),
	}
	_, err := reconcileModelDeploymentWith(f.t, r)
	require.NoError(f.t, err, "pass %d", len(f.past))
	f.past = append(f.past, replicaNames(f.t, f.cli))
}

// passExpectingFailure runs a pass whose create is about to fail, and requires that it reported it.
// A failed create is still a pass that looked: the error is what tells the next one to try again.
func (f *lifecycleFixture) passExpectingFailure(fresh bool) {
	f.t.Helper()

	r := &ModelDeploymentReconciler{
		Client: f.cli, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64),
	}
	if fresh {
		r = &ModelDeploymentReconciler{
			Client: f.cli, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		}
	}
	_, err := reconcileModelDeploymentWith(f.t, r)
	require.Error(f.t, err, "pass %d reported the create that never landed", len(f.past))
	f.past = append(f.past, replicaNames(f.t, f.cli))
}

// admit runs the Kueue half of the handshake through Kueue's own constructor: departures finish,
// and every complete group composes its
// Workload and is admitted.
func (f *lifecycleFixture) admit() {
	f.t.Helper()
	ctx := context.Background()

	// A delete is accepted while the finalizer is on the Pod, so the group stays occupied until the
	// finalizer comes off. That is the window the vacancy proof waits on.
	for _, pod := range replicaPods(f.t, f.cli) {
		if pod.DeletionTimestamp == nil || !slices.Contains(pod.Finalizers, kueuepodconst.PodFinalizer) {
			continue
		}
		released := pod.DeepCopy()
		released.Finalizers = nil
		require.NoError(f.t, f.cli.Update(ctx, released))
	}

	// A composed Workload carries Kueue's finalizer too, so the group name stays held until it comes
	// off. The vacancy proof reads the server rather than the cache for the same reason.
	departing := new(kueue.WorkloadList)
	require.NoError(f.t, f.cli.List(ctx, departing, ctrlcli.InNamespace("team-a")))
	for i := range departing.Items {
		if departing.Items[i].DeletionTimestamp == nil {
			continue
		}
		released := departing.Items[i].DeepCopy()
		released.Finalizers = nil
		require.NoError(f.t, f.cli.Update(ctx, released))
	}

	for _, group := range slices.Sorted(maps.Keys(f.groupsByName())) {
		members := f.groupsByName()[group]
		if held := f.workloadNamed(group); held != nil {
			// A Workload already holding this name that owns none of the members standing now is not
			// adopted. The constructor composes nothing under a taken name, so writing over one would
			// assert something Kueue never does; the refusal is stated rather than worked around
			// because refusing it is the property under test.
			uids := sets.New[types.UID]()
			for _, member := range members {
				uids.Insert(member.UID)
			}
			require.True(f.t, modelDeploymentWorkloadOwnsAny(held, uids),
				"the fixture will not let the pre-existing workload %q absorb the members standing now",
				group)

			continue
		}

		// AN INCOMPLETE GROUP, OR ONE WHOSE MEMBERS DISAGREE ON THEIR TOTAL, COMPOSES NOTHING. That
		// is Kueue's own answer, and a pass that created one of those groups is a pass whose next
		// step has to wait rather than a fixture failure.
		composed, err := kueueComposeGroup(ctx, f.cli, group)
		if err != nil || composed == nil {
			continue
		}

		composed.UID = f.nextUID("wl")
		require.NoError(f.t, f.cli.Create(ctx, composed))

		// Kueue puts the finalizer on the members of a group it has composed, and that is what makes
		// the departure above take a pass rather than completing at once.
		for _, member := range members {
			held := new(core.Pod)
			require.NoError(f.t, f.cli.Get(ctx, ctrlcli.ObjectKeyFromObject(member), held))
			if slices.Contains(held.Finalizers, kueuepodconst.PodFinalizer) {
				continue
			}
			held.Finalizers = append(held.Finalizers, kueuepodconst.PodFinalizer)
			require.NoError(f.t, f.cli.Update(ctx, held))
		}

		admitted := new(kueue.Workload)
		require.NoError(f.t, f.cli.Get(ctx,
			ctrlcli.ObjectKey{Namespace: "team-a", Name: group}, admitted))
		admitted.Status.Conditions = []meta.Condition{{
			Type:               kueue.WorkloadAdmitted,
			Status:             meta.ConditionTrue,
			Reason:             "Admitted",
			LastTransitionTime: meta.Now(),
		}}
		require.NoError(f.t, f.cli.Status().Update(ctx, admitted))
	}
}

// live returns the live members at one ordinal.
func (f *lifecycleFixture) live(role string, ordinal int) []*core.Pod {
	f.t.Helper()

	var members []*core.Pod
	for _, pod := range replicaPods(f.t, f.cli) {
		if pod.DeletionTimestamp != nil || modelDeploymentPodRole(&pod) != role {
			continue
		}
		if at, ok := modelDeploymentPodOrdinal(&pod); ok && at == ordinal {
			copied := pod.DeepCopy()
			members = append(members, copied)
		}
	}

	return members
}

// slot returns the role's recorded replacement slot and whether it has one.
func (f *lifecycleFixture) slot(role string) (modelDeploymentReplacementSlot, bool) {
	f.t.Helper()

	return modelDeploymentReplacementSlotsOf(getModelDeployment(f.t, f.cli)).slotFor(role)
}

// images reports the container image of every live member at one ordinal.
func (f *lifecycleFixture) images(role string, ordinal int) []string {
	f.t.Helper()

	seen := make([]string, 0, len(f.live(role, ordinal)))
	for _, member := range f.live(role, ordinal) {
		seen = append(seen, member.Spec.Containers[0].Image)
	}

	return seen
}

// TestLifecycle_AQueuedReplacementHoldsTheRoleAcrossPassesAndRestarts covers the cadence rule: one
// unresolved replacement per role, and it survives a controller restart because the intent was
// persisted before anything was deleted.
func TestLifecycle_AQueuedReplacementHoldsTheRoleAcrossPassesAndRestarts(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 3
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()
	before := f.live("server", 2)
	require.Len(t, before, 1, "three replicas of one member each")

	// The edit that makes every replica outdated.
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))

	f.pass(false)
	slot, held := f.slot("server")
	require.True(t, held, "selecting a replacement records the intent before deleting anything")
	assert.Len(t, slot.MemberUIDs, 1, "and the slot captures the member it is about to remove")
	assert.Equal(t, modelDeploymentReplacementCleanup, slot.Phase)

	// Kueue releases the finalizer the captured member holds, so it finishes leaving. The ordinal is
	// empty from here on, and the replacement comes back queued behind the vacancy.
	f.admit()
	require.Empty(t, f.live("server", slot.Ordinal), "the captured member has finished leaving")

	// The next passes must not turn over another healthy replica, and a reconciler built fresh must
	// reach the same conclusion from the persisted slot.
	for pass := 0; pass < 3; pass++ {
		f.pass(pass%2 == 0)
		assert.Len(t, f.live("server", 1), 1,
			"pass %d: the healthy replica beside the queued one is untouched", pass)
		assert.Len(t, f.live("server", 0), 1,
			"pass %d: and so is the one below it", pass)
	}
	assert.Contains(t, f.images("server", 2), "vllm/vllm-openai:v0.26.0",
		"while the replacement itself is built from the new spec")

	// The admission releases the role, and only then does the next turnover happen. The pass that
	// releases it may open the next slot in the same breath, so what is asserted is the turnover
	// itself rather than the absence of a slot.
	// Once the replacement is admitted the role is free again, so the remaining replicas turn over
	// in their own turns rather than all at once.
	for pass := 0; pass < 8; pass++ {
		if allAtImage(f, "vllm/vllm-openai:v0.26.0", 3) {
			break
		}
		f.pass(pass%2 == 0)
		f.admit()
	}

	for ordinal := range 3 {
		members := f.live("server", ordinal)
		require.Len(t, members, 1, "ordinal %d never empties", ordinal)
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", members[0].Spec.Containers[0].Image,
			"ordinal %d carries the edit once the role has settled", ordinal)
	}
}

// TestLifecycle_PersistedIntentSurvivesAnInterruptionBetweenDeleteAndCreate covers the window the
// slot exists for: the members are gone, the record is not, and nothing observable names the
// replica being replaced.
func TestLifecycle_PersistedIntentSurvivesAnInterruptionBetweenDeleteAndCreate(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()
	before := getModelDeployment(t, f.cli)

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))

	f.pass(false)
	slot, held := f.slot("server")
	require.True(t, held)

	// Everything the pass issued happens, and then the controller dies.
	f.admit()
	require.Len(t, f.live("server", slot.Ordinal), 0, "the departing member is gone from the cluster")

	// A fresh controller over the same cluster finds the ordinal empty and nothing else to go on.
	f.pass(true)
	after, stillHeld := f.slot("server")
	require.True(t, stillHeld,
		"an empty ordinal names no replica, so the slot is the only thing that can complete this one")
	assert.Equal(t, slot.Ordinal, after.Ordinal, "and it is the same ordinal, not a new selection")
	assert.NotEmpty(t, after.MemberHashes, "which now records what the replacement is being built from")

	f.pass(true)
	f.admit()
	f.pass(true)
	assert.Equal(t, []string{"vllm/vllm-openai:v0.26.0"}, f.images("server", slot.Ordinal),
		"and the ordinal comes back built from the new spec")
	assert.Equal(t, before.Generation, getModelDeployment(t, f.cli).Generation,
		"through all of it the spec itself never moved again")
}

// TestLifecycle_ASecondEditSupersedesTheQueuedConfiguration covers the supersession rule: the
// queued configuration is discarded and rebuilt in the same slot, without a second one opening and
// without waiting for an admission that can never come.
func TestLifecycle_ASecondEditSupersedesTheQueuedConfiguration(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()

	first := getModelDeployment(t, f.cli)
	first.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), first))

	f.pass(false)
	slot, held := f.slot("server")
	require.True(t, held)
	f.admit()
	require.Empty(t, f.live("server", slot.Ordinal), "the captured member has finished leaving")

	// A second edit lands while the first replacement is still queued.
	second := getModelDeployment(t, f.cli)
	second.Spec.Roles[0].Image = "vllm/vllm-openai:v0.27.0"
	require.NoError(t, f.cli.Update(context.Background(), second))

	sibling := 1 - slot.Ordinal
	var siblingUID types.UID
	if held := f.live("server", sibling); len(held) == 1 {
		siblingUID = held[0].UID
	}

	for pass := 0; pass < 4; pass++ {
		f.pass(pass%2 == 0)
	}

	_, stillHeld := f.slot("server")
	assert.True(t, stillHeld, "the role is still committed to the same ordinal")
	assert.Equal(t, "vllm/vllm-openai:v0.27.0", f.images("server", slot.Ordinal)[0],
		"and the obsolete queued configuration was replaced rather than waited for")

	for pass := 0; pass < 8; pass++ {
		if allAtImage(f, "vllm/vllm-openai:v0.27.0", 2) {
			break
		}
		f.pass(pass%2 == 0)
		f.admit()
	}

	for _, ordinal := range []int{slot.Ordinal, sibling} {
		members := f.live("server", ordinal)
		require.Len(t, members, 1, "ordinal %d keeps serving throughout", ordinal)
		assert.Equal(t, "vllm/vllm-openai:v0.27.0", members[0].Spec.Containers[0].Image,
			"ordinal %d ends on the newest spec, never on the obsolete one", ordinal)
	}
	if siblingUID != types.UID("") {
		assert.NotEqual(t, siblingUID, f.live("server", sibling)[0].UID,
			"while the obsolete sibling was turned over in its own turn rather than kept")
	}
}

// TestLifecycle_ACleanupIsBoundedToWhatTheSlotCaptured covers UID authority: a member that took
// the name after the capture is not the one the slot may delete.
func TestLifecycle_ACleanupIsBoundedToWhatTheSlotCaptured(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()

	// The members the slot may capture are read before the pass that removes one, because after
	// that pass its name is free and nothing else names the identity the slot may delete.
	before := map[int]*core.Pod{}
	for ordinal := range 2 {
		members := f.live("server", ordinal)
		require.Len(t, members, 1)
		before[ordinal] = members[0]
	}

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))
	f.pass(false)

	slot, held := f.slot("server")
	require.True(t, held)
	captured := before[slot.Ordinal]
	assert.Equal(t, []types.UID{captured.UID}, slot.MemberUIDs,
		"and the slot names the identity rather than the name")

	// The captured member finishes leaving, which frees its name the way a same-named recreate is
	// only possible after.
	f.admit()
	require.Empty(t, f.live("server", slot.Ordinal))

	// A different Pod takes the departing member's name and seat, as a same-named recreate would.
	stranger := captured.DeepCopy()
	stranger.GenerateName = ""
	stranger.UID = types.UID("pod-someone-else")
	stranger.ResourceVersion = ""
	require.NoError(t, f.cli.Create(context.Background(), stranger))

	f.pass(false)
	f.admit()
	f.pass(false)

	survivor := new(core.Pod)
	require.NoError(t, f.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: captured.Name}, survivor))
	assert.Equal(t, types.UID("pod-someone-else"), survivor.UID,
		"the cleanup deleted the identity the slot captured, not the name")
}

// TestLifecycle_AForeignWorkloadOnTheGroupNameIsNeverDeleted covers the other half of that
// authority: the vacancy proof waits on an object this operator cannot prove it composed, and does
// not remove it to make room.
func TestLifecycle_AForeignWorkloadOnTheGroupNameIsNeverDeleted(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))
	f.pass(false)

	slot, held := f.slot("server")
	require.True(t, held, "the highest ordinal turns over first")

	// Something else holds the name the replacement's own group is composed under. It owns none of
	// the replica's members, so nothing about it is this operator's to delete or to adopt. The
	// slot's own Workload is given up first, because a name still held is not one a stranger could
	// take -- and because a foreign Workload only arrives where this operator's has gone.
	f.admit()
	group := modelDeploymentReplicaGroupName(f.md, "server", slot.Ordinal)
	require.Nil(t, f.workloadNamed(group), "the group name is free before the stranger takes it")
	foreign := &kueue.Workload{}
	foreign.Name, foreign.Namespace = group, "team-a"
	foreign.UID = types.UID("wl-not-ours")
	require.NoError(t, f.cli.Create(context.Background(), foreign))

	for pass := range 4 {
		f.pass(false)
		assert.Equal(t, slot.Ordinal, slotOrdinal(t, f),
			"pass %d: the vacancy proof waits rather than choosing another replica", pass)
	}

	surviving := new(kueue.Workload)
	require.NoError(t, f.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: group}, surviving))
	assert.Equal(t, types.UID("wl-not-ours"), surviving.UID,
		"a Workload on the group's name that owns none of its members is not this operator's to delete")
}

// slotOrdinal returns the ordinal a role's slot names, or -1 when it holds none.
func slotOrdinal(t *testing.T, f *lifecycleFixture) int {
	t.Helper()

	slot, held := f.slot("server")
	if !held {
		return -1
	}

	return slot.Ordinal
}

// TestLifecycle_ABrokenReplicaWaitsBehindTheSlotAndIsRebuiltWholeAfterwards covers a real member
// loss while another replica is being replaced. The broken replica is never topped up from the
// current render -- that would put a member of one configuration beside a member of another -- and
// it is not dismantled either while the role is committed elsewhere. Once the slot is admitted it
// is rebuilt whole, ahead of any healthy old replica.
func TestLifecycle_ABrokenReplicaWaitsBehindTheSlotAndIsRebuiltWholeAfterwards(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].Replicas = 3
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()
	require.Len(t, f.live("server", 0), 2, "size two renders two members")

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))
	f.pass(false)
	slot, held := f.slot("server")
	require.True(t, held)
	require.Equal(t, 2, slot.Ordinal, "the highest ordinal turns over first")

	// While that slot waits, a DIFFERENT replica loses a member for real.
	lost := f.live("server", 0)
	require.NoError(t, f.cli.Delete(context.Background(), lost[1].DeepCopy()))
	healthy := f.live("server", 1)
	require.Len(t, healthy, 2)

	for pass := range 3 {
		f.pass(pass%2 == 0)
		assertOneConfiguration(t, f, 0,
			fmt.Sprintf("pass %d: the broken replica is never mixed", pass))
		assert.Len(t, f.live("server", 0), 1,
			"pass %d: and is not topped up from the render the spec states now", pass)
		assert.Len(t, f.live("server", 1), 2, "pass %d: a healthy sibling is untouched", pass)
		assert.Equal(t, healthy[0].UID, f.live("server", 1)[0].UID,
			"pass %d: down to the member", pass)
		assert.Equal(t, slot.Ordinal, f.slotMust("server").Ordinal,
			"pass %d: the slot still names its own ordinal", pass)
	}

	// The slot's replacement is admitted, and the broken replica is what the role turns over next:
	// the pass that releases the role spends it on the ordinal that is down, not on the one that is
	// still serving.
	f.admit()
	f.pass(false)

	stillServing := f.live("server", 1)
	require.Len(t, stillServing, 2, "the healthy old replica keeps serving")
	assert.Equal(t, healthy[0].UID, stillServing[0].UID, "down to the member")
	assert.Equal(t, "vllm/vllm-openai:v0.25.1", stillServing[0].Spec.Containers[0].Image,
		"and it was not taken ahead of the broken one")

	for pass := range 10 {
		if allAtImage(f, "vllm/vllm-openai:v0.26.0", 6) {
			break
		}
		f.pass(pass%2 == 0)
		f.admit()
		assertOneConfiguration(t, f, 0,
			fmt.Sprintf("settling pass %d: the broken ordinal is never mixed either", pass))
	}

	for ordinal := range 3 {
		members := f.live("server", ordinal)
		require.Len(t, members, 2, "ordinal %d is whole again", ordinal)
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", members[0].Spec.Containers[0].Image,
			"ordinal %d carries the edit the replacement was built from", ordinal)
	}
}

// assertOneConfiguration states that the live members of an ordinal all run the same image, which is
// the property a top-up from a newer render would break. An empty ordinal satisfies it: a replica
// with nothing serving has mixed nothing.
func assertOneConfiguration(t *testing.T, f *lifecycleFixture, ordinal int, message string) {
	t.Helper()

	seen := sets.New[string]()
	for _, member := range f.live("server", ordinal) {
		seen.Insert(member.Spec.Containers[0].Image)
	}
	assert.LessOrEqual(t, seen.Len(), 1, message)
}

// TestLifecycle_APartialCreateIsFilledOnlyInTheReplacingGroup covers a create that lands for one
// member of a replica and not the other. The group is then short of its total, which is a group
// Kueue composes no Workload for, so the slot would wait on it forever unless the missing member is
// filled -- and it is filled only because that group is the one the replacement is being built in.
func TestLifecycle_APartialCreateIsFilledOnlyInTheReplacingGroup(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
	})

	// The second member's create fails once, and then stops failing. The replica renders its
	// members for one group under one name each, so the failure lands on the replica being
	// replaced rather than on a member chosen by the case.
	failing := false
	f := newLifecycleFixtureAround(t, md, func(objs ...ctrlcli.Object) ctrlcli.WithWatch {
		return ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithIndex(&core.Pod{}, kueuepod.PodGroupNameCacheKey, func(obj ctrlcli.Object) []string {
				pod, ok := obj.(*core.Pod)
				if !ok {
					return nil
				}
				group := pod.Labels[kueuepodconst.GroupNameLabel]
				if group == "" {
					return nil
				}

				return []string{group}
			}).
			WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string {
				pod, ok := obj.(*core.Pod)
				if !ok || pod.Spec.NodeName == "" {
					return nil
				}

				return []string{pod.Spec.NodeName}
			}).
			WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.KVCachePoolBinding{},
				&workercore.Devices{}, &kueue.Workload{}).
			WithInterceptorFuncs(ctrlinterceptor.Funcs{
				Create: func(
					c context.Context, next ctrlcli.WithWatch, obj ctrlcli.Object,
					opts ...ctrlcli.CreateOption,
				) error {
					pod, ok := obj.(*core.Pod)
					if !ok || !failing ||
						pod.Labels[modelDeploymentMemberIndexLabel] != "1" {
						return next.Create(c, obj, opts...)
					}

					return errors.New("the create was refused")
				},
			}).
			WithObjects(objs...).
			Build()
	})

	f.pass(false)
	f.admit()
	for ordinal := range 2 {
		require.Len(t, f.live("server", ordinal), 2, "the role is whole before the edit")
	}

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))
	f.pass(false)
	slot, held := f.slot("server")
	require.True(t, held)
	f.admit()
	require.Empty(t, f.live("server", slot.Ordinal), "the captured member has finished leaving")

	// The group comes back one member short of its total.
	failing = true
	f.passExpectingFailure(false)
	assert.Len(t, f.live("server", slot.Ordinal), 1,
		"the create that landed left the replica one member short")

	// And the next pass fills exactly that one member, leaving the one already there alone.
	failing = false
	f.pass(false)
	members := f.live("server", slot.Ordinal)
	require.Len(t, members, 2, "the missing member is filled in the group the replacement is built in")
	assertOneConfiguration(t, f, slot.Ordinal, "and the filled group is one configuration")
	assert.Len(t, f.live("server", 1-slot.Ordinal), 2,
		"while the sibling replica the slot is not about keeps both of its members")

	f.admit()
	f.pass(false)
	assert.Empty(t, f.answering()[slot.Ordinal], "an unadmitted member does not take an endpoint")
	for _, member := range members {
		assert.Equal(t, "vllm/vllm-openai:v0.26.0", member.Spec.Containers[0].Image,
			"and both members carry the configuration the replacement was built from")
	}
}

// TestLifecycle_TwoIncarnationsOfOneReplicaGetDistinctIdentities pins the premise every comparison
// above rests on. A replaced replica and the one that took its seat share a name and a group, so
// only the identities the API server assigns tell them apart -- and a fixture that gave one pair of
// identities to two objects would pass a cleanup that deleted the wrong one.
func TestLifecycle_TwoIncarnationsOfOneReplicaGetDistinctIdentities(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()
	before := f.live("server", 0)
	require.Len(t, before, 2)
	beforeWorkload := f.workloadNamed(modelDeploymentReplicaGroupName(f.md, "server", 0))
	require.NotNil(t, beforeWorkload)

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))

	for range 6 {
		f.pass(false)
		f.admit()
		if allAtImage(f, "vllm/vllm-openai:v0.26.0", 2) {
			break
		}
	}
	after := f.live("server", 0)
	require.Len(t, after, 2)
	afterWorkload := f.workloadNamed(modelDeploymentReplicaGroupName(f.md, "server", 0))
	require.NotNil(t, afterWorkload)

	beforeUIDs := sets.New[types.UID]()
	for _, member := range before {
		beforeUIDs.Insert(member.UID)
	}
	for _, member := range after {
		assert.False(t, beforeUIDs.Has(member.UID),
			"%s is a different object from the member that held this seat", member.Name)
	}
	assert.NotEqual(t, beforeWorkload.UID, afterWorkload.UID,
		"and so is the Workload the group composed for each of them")
	assert.False(t, modelDeploymentWorkloadOwnsAny(afterWorkload, beforeUIDs),
		"the Workload composed for the replacement owns none of the members it replaced")
}

// allAtImage reports whether the role holds its declared replica count and every member carries an
// image. An empty set is not convergence: a role with nothing serving has not settled, it has
// stopped.
func allAtImage(f *lifecycleFixture, image string, replicas int) bool {
	live := 0
	for _, pod := range replicaPods(f.t, f.cli) {
		if modelDeploymentPodRole(&pod) != "server" || pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Spec.Containers[0].Image != image {
			return false
		}
		live++
	}

	return live == replicas
}

// slotMust returns a role's slot, failing the test when there is none.
func (f *lifecycleFixture) slotMust(role string) modelDeploymentReplacementSlot {
	f.t.Helper()

	slot, held := f.slot(role)
	require.True(f.t, held, "expected an active replacement slot for role %q", role)

	return slot
}

// setReady puts the live members of one ordinal in or out of the Ready condition, which is the
// unconditional half of every routing decision.
func (f *lifecycleFixture) setReady(ordinal int, ready bool) {
	f.t.Helper()

	for _, member := range f.live("server", ordinal) {
		ready := ready
		stored := new(core.Pod)
		require.NoError(f.t, f.cli.Get(context.Background(),
			ctrlcli.ObjectKeyFromObject(member), stored))
		if ready {
			stored.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
		} else {
			stored.Status.Conditions = nil
		}
		require.NoError(f.t, f.cli.Status().Update(context.Background(), stored))
	}
}

// setEligible puts the engine-eligibility key on the live members of one ordinal, or takes it off.
// A key that is absent is a member this operator has not found qualified, which is the state the
// negative controls need.
func (f *lifecycleFixture) setEligible(ordinal int, present bool) {
	f.t.Helper()

	for _, member := range f.live("server", ordinal) {
		patched := new(core.Pod)
		require.NoError(f.t, f.cli.Get(context.Background(),
			ctrlcli.ObjectKeyFromObject(member), patched))
		modelDeploymentApplyLabel(patched, modelDeploymentLabelKeyEndpointEligible, present)
		require.NoError(f.t, f.cli.Patch(context.Background(), patched, ctrlcli.MergeFrom(member)))
	}
}

// answering reports the routing label of every live member, keyed by the replica it sits in. A
// member without the key reports the empty string, which is how the Service selector reads it.
func (f *lifecycleFixture) answering() map[int]string {
	f.t.Helper()

	seen := map[int]string{}
	for _, member := range f.live("server", 0) {
		ordinal, ok := modelDeploymentPodOrdinal(member)
		if ok {
			seen[ordinal] = member.Labels[modelDeploymentLabelKeyAPIAnswering]
		}
	}
	for _, member := range f.live("server", 1) {
		ordinal, ok := modelDeploymentPodOrdinal(member)
		if ok {
			seen[ordinal] = member.Labels[modelDeploymentLabelKeyAPIAnswering]
		}
	}

	return seen
}

// frontService is the Service clients address the deployment through.
func (f *lifecycleFixture) frontService() core.Service {
	f.t.Helper()

	svc := new(core.Service)
	require.NoError(f.t, f.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "team-a", Name: f.md.Name}, svc))

	return *svc
}

// routesByMembership reports whether the front Service selects on the routing label at all. A
// homogeneous role does not, and keeps its ordinary selector.
func (f *lifecycleFixture) routesByMembership() bool {
	f.t.Helper()

	_, present := f.frontService().Spec.Selector[modelDeploymentLabelKeyAPIAnswering]

	return present
}

// TestLifecycle_ReplicasGrowingAndShrinkingDuringAReplacement covers both directions of a count
// change while a slot is open: an ordinal the count no longer declares cancels the slot it holds
// and is not created again, and an ordinal that re-enters the count is created from the spec the
// role states now rather than from the configuration it left.
func TestLifecycle_ReplicasGrowingAndShrinkingDuringAReplacement(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 3
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))
	f.pass(false)

	slot, held := f.slot("server")
	require.True(t, held, "the highest ordinal turns over first")
	require.Equal(t, 2, slot.Ordinal, "which is the one the smaller count no longer declares")

	// The count shrinks past the ordinal the slot owns. The slot is canceled rather than carried
	// on an ordinal nothing declares, the ordinal is not created again, and the two the count still
	// declares are then rebuilt from the new spec in their own turns.
	scaled := getModelDeployment(t, f.cli)
	scaled.Spec.Roles[0].Replicas = 2
	require.NoError(t, f.cli.Update(context.Background(), scaled))

	for pass := range 4 {
		f.pass(pass%2 == 0)
		assert.Empty(t, f.live("server", 2),
			"pass %d: an ordinal the spec no longer declares is not recreated", pass)
	}

	for pass := range 10 {
		if allAtImage(f, "vllm/vllm-openai:v0.26.0", 2) {
			break
		}
		f.pass(pass%2 == 0)
		f.admit()
		assert.Empty(t, f.live("server", 2),
			fmt.Sprintf("pass %d: nor once the role is free again", pass))
	}
	require.True(t, allAtImage(f, "vllm/vllm-openai:v0.26.0", 2), "the role settles at two replicas")

	// The count grows back. The ordinal that returns is created from the spec the role states now.
	grown := getModelDeployment(t, f.cli)
	grown.Spec.Roles[0].Replicas = 3
	require.NoError(t, f.cli.Update(context.Background(), grown))

	for pass := range 6 {
		f.pass(pass%2 == 0)
		if len(f.live("server", 2)) > 0 {
			break
		}
	}
	members := f.live("server", 2)
	require.Len(t, members, 1, "the returning ordinal is created")
	assert.Equal(t, "vllm/vllm-openai:v0.26.0", members[0].Spec.Containers[0].Image,
		"from the spec the role states now, not the one it left")
}

// TestLifecycle_AFourFieldEditIsOneReplacementNotFour covers the contract the whole task exists for:
// changing size, resources, instance type and command together still turns over one replica at a
// time, and each replica comes back carrying all four.
func TestLifecycle_AFourFieldEditIsOneReplacementNotFour(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	f := newLifecycleFixture(t, md, newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "h20-2x"
		it.Spec.UnitResources = workercore.InstanceTypeUnitResources{CPU: "4", RAM: "16Gi"}
		it.Status.Entrance = "queue-for-h20-2x"
	}))

	f.pass(false)
	f.admit()

	// The four fields the role makes mutable, each observable in the rendered Pod: two members per
	// replica, host resources sized for two cards, the narrower pool's queue, and the command line
	// the role supplies in full.
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].ReplicaSize = 2
	edited.Spec.Roles[0].InstanceType = "h20-2x"
	edited.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
		Accelerator: ptr.To(resource.MustParse("2")),
	}
	edited.Spec.Roles[0].Command = []string{"python", "-m", "vllm.entrypoints.openai.api_server"}
	require.NoError(t, f.cli.Update(context.Background(), edited))

	// One pass turns over exactly one replica, whatever the edit touched.
	f.pass(false)
	_, held := f.slot("server")
	require.True(t, held)
	assert.Len(t, f.live("server", 0), 1, "the sibling is untouched by a four-field edit")
	assert.Empty(t, f.live("server", 1), "and one replica is on its way out")

	for range 6 {
		f.pass(false)
		f.admit()
	}

	f.pass(false)
	for _, ordinal := range []int{0, 1} {
		members := f.live("server", ordinal)
		require.Len(t, members, 2, "ordinal %d is rebuilt at the new member count", ordinal)
		for _, member := range members {
			assert.Equal(t, "vllm/vllm-openai:v0.25.1", member.Spec.Containers[0].Image,
				"a field the edit did not name is unchanged")
			assert.Equal(t, []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
				member.Spec.Containers[0].Command, "the command is the one the role supplied")
			assert.Equal(t, "queue-for-h20-2x", member.Labels[kueuectrlconst.QueueLabel],
				"and the group is submitted to the pool the role named")
			// Four cards' worth of unit resources are one: the narrower pool's four CPU by the two
			// cards the role asked for, where the unedited render asked for one card of the wider.
			assert.Equal(t, int64(8), member.Spec.Containers[0].Resources.Limits.Cpu().Value(),
				"the host resources follow the pool the edit named and the cards it asked for")
		}
	}
}

// TestLifecycle_SiblingRolesAreNobodyElsesCost covers a two-role deployment: the role being edited
// spends its own replicas and leaves the other's alone.
func TestLifecycle_SiblingRolesAreNobodyElsesCost(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})
	sibling := md.Spec.Roles[0]
	sibling.Name = "decode"
	sibling.Replicas = 1
	md.Spec.Roles = append(md.Spec.Roles, sibling)

	f := newLifecycleFixture(t, md)
	f.pass(false)
	f.admit()

	before := f.live("decode", 0)
	require.Len(t, before, 1)

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(context.Background(), edited))

	for pass := 0; pass < 3; pass++ {
		f.pass(false)
		f.admit()
	}

	after := f.live("decode", 0)
	require.Len(t, after, 1)
	assert.Equal(t, before[0].UID, after[0].UID,
		"the sibling role is not a replacement candidate and keeps its member")
	assert.Equal(t, "vllm/vllm-openai:v0.25.1", after[0].Spec.Containers[0].Image,
		"built from the image the sibling role's edit never changed")
}

// TestLifecycle_APartialCreateWhoseResponseIsLostIsNotRepeated covers the create gate against the
// server rather than the cache: a create the server accepted and never answered is found, not
// repeated.
func TestLifecycle_APartialCreateWhoseResponseIsLostIsNotRepeated(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
	})
	f := newLifecycleFixture(t, md)
	server := f.cli
	hidden := true
	lost := 0
	attempts := 0
	view := ctrlinterceptor.NewClient(server, ctrlinterceptor.Funcs{
		List: func(ctx context.Context, next ctrlcli.WithWatch, list ctrlcli.ObjectList,
			opts ...ctrlcli.ListOption,
		) error {
			if err := next.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*core.PodList); ok && hidden {
				pods.Items = nil
			}
			return nil
		},
		Create: func(ctx context.Context, next ctrlcli.WithWatch, obj ctrlcli.Object,
			opts ...ctrlcli.CreateOption,
		) error {
			if _, ok := obj.(*core.Pod); ok {
				attempts++
			}
			if err := next.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*core.Pod); ok {
				lost++
				return errors.New("connection lost: the create's response never arrived")
			}
			return nil
		},
	})
	r := &ModelDeploymentReconciler{Client: view, APIReader: server, Recorder: ctrlrecord.NewFakeRecorder(64)}
	_, err := reconcileModelDeploymentWith(t, r)
	require.ErrorContains(t, err, "connection lost")
	require.Equal(t, 4, lost, "all members were accepted by the authoritative server")
	before := replicaPods(t, server)
	require.Len(t, before, 4)
	uids := sets.New[types.UID]()
	for _, pod := range before {
		require.NotEmpty(t, pod.UID, "identity exists before any lifecycle read")
		uids.Insert(pod.UID)
	}
	require.Len(t, uids, 4, "every create has a distinct identity")
	require.Empty(t, replicaPods(t, view), "the cache remains behind on retry")

	_, err = reconcileModelDeploymentWith(t, r)
	require.NoError(t, err, "the uncached gate finds successful writes despite lost responses")
	assert.Equal(t, 4, lost, "no successful create was repeated")
	assert.Equal(t, 4, attempts, "no duplicate create was attempted")
	after := replicaPods(t, server)
	require.Len(t, after, 4, "the authoritative member set did not grow")
	for _, pod := range after {
		assert.True(t, uids.Has(pod.UID))
	}

	// Composition uses the authoritative complete groups and Kueue's pinned constructor.
	f.admit()
	workloadUIDs := sets.New[types.UID]()
	for ordinal := range 2 {
		wl := f.workloadNamed(modelDeploymentReplicaGroupName(md, "server", ordinal))
		require.NotNil(t, wl)
		require.NotEmpty(t, wl.UID)
		workloadUIDs.Insert(wl.UID)
		for _, pod := range f.live("server", ordinal) {
			assert.True(t, modelDeploymentWorkloadOwnsAny(wl, sets.New(pod.UID)))
		}
	}
	require.Len(t, workloadUIDs, 2)
	assert.Empty(t, uids.Intersection(workloadUIDs), "Pod and Workload identities are distinct")
	hidden = false
	_, err = reconcileModelDeploymentWith(t, r)
	require.NoError(t, err, "the cache eventually catches up")
	assert.Equal(t, 4, lost)
	assert.True(t, allAtImage(f, "vllm/vllm-openai:v0.25.1", 4))
	for ordinal := range 2 {
		members := f.live("server", ordinal)
		require.Len(t, members, 2)
		for _, pod := range members {
			assert.True(t, uids.Has(pod.UID))
		}
		assert.True(t, workloadUIDs.Has(f.workloadNamed(modelDeploymentReplicaGroupName(md, "server", ordinal)).UID))
	}
}

// TestLifecycle_SizingDownDoesNotTrimTheHealthyGroup covers the arithmetic the size edit depends
// on: a group built at four members is not excess while the spec asks for two.
func TestLifecycle_SizingDownDoesNotTrimTheHealthyGroup(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ReplicaSize = 4
		md.Spec.Roles[0].Replicas = 2
	})
	f := newLifecycleFixture(t, md)

	f.pass(false)
	f.admit()
	require.Len(t, f.live("server", 0), 4, "the group is built at the size the spec asked for")

	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].ReplicaSize = 2
	require.NoError(t, f.cli.Update(context.Background(), edited))

	f.pass(false)
	slot, held := f.slot("server")
	require.True(t, held)
	sibling := 1 - slot.Ordinal

	// Three passes with nothing admitted behind the first replacement: the group that is not being
	// replaced must still be whole, because a desired-size difference is not surplus.
	for pass := range 3 {
		f.pass(false)
		assert.Len(t, f.live("server", sibling), 4,
			"pass %d: the healthy group keeps all four members while one replica is replaced", pass)
	}

	for range 8 {
		if len(f.live("server", slot.Ordinal)) == 2 && len(f.live("server", sibling)) == 2 {
			break
		}
		f.pass(false)
		f.admit()
	}
	assert.Len(t, f.live("server", slot.Ordinal), 2, "the replaced replica comes back at the new size")
	assert.Len(t, f.live("server", sibling), 2, "and so does the one behind it, in its own turn")
}
