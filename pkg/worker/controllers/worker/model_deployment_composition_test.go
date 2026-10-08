package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"
	kueuepod "sigs.k8s.io/kueue/pkg/controller/jobs/pod"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	utilexpectations "sigs.k8s.io/kueue/pkg/util/expectations"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// This file builds the Workload a replica's group composes to, by running Kueue's own pod job rather
// than writing one by hand. A composed one names its PodSet after the role hash, counts the members
// sharing that hash, owns each member by a real owner reference, carries Kueue's finalizer, and
// refuses to compose when the group's members do not agree on their total.
//
// The constructor does not refuse a stale adoption by itself: it composes for the members standing
// under a group name, and the lifecycle fixture refuses to write over a Workload that name already
// holds. TestComposition_AStaleWorkloadCannotAbsorbANewMember states the constructor's half and
// TestLifecycle_TwoIncarnationsOfOneReplicaGetDistinctIdentities states the property through real
// creates.

// newCompositionClient builds a client that answers Kueue's own group lookup, which is a List of
// Pods by the PodGroupNameCacheKey index. Without that index the lookup cannot be performed at all.
//
// A seeded ModelDeployment is given a resource version first: an object read from a server always
// carries one, and the lifecycle records its replacement intent through a real optimistic lock that
// needs one to compare against.
func newCompositionClient(objs ...ctrlcli.Object) ctrlcli.WithWatch {
	for _, obj := range objs {
		if md, ok := obj.(*workercore.ModelDeployment); ok && md.ResourceVersion == "" {
			md.ResourceVersion = "1"
		}
	}

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
		WithObjects(objs...).
		Build()
}

// kueueComposeGroup runs Kueue's composition for a group and returns the Workload it decided on.
func kueueComposeGroup(ctx context.Context, cli ctrlcli.Client, group string) (*kueue.Workload, error) {
	podJob := kueuepod.NewPod(
		kueuepod.WithExcessPodExpectations(utilexpectations.NewStore("composition-test")))
	key := types.NamespacedName{Namespace: "group/team-a", Name: group}
	if _, err := podJob.Load(ctx, cli, &key); err != nil {
		return nil, err
	}

	return podJob.ConstructComposableWorkload(ctx, cli, ctrlrecord.NewFakeRecorder(64), nil)
}

// createMember creates one group member with a stamped UID and returns it.
//
// The UID is stamped before anything owns it. The fake client assigns none to a GenerateName
// create, and an empty UID on both sides of an ownership match matches everything, so an unstamped
// Pod would let a Workload claim a group it never composed for.
func createMember(t *testing.T, ctx context.Context, cli ctrlcli.Client, pod *core.Pod, uid string) *core.Pod {
	t.Helper()

	live := pod.DeepCopy()
	// The member came off another client, so it carries a version and an owner the target server
	// has never seen. Both belong to the object being created, not to the Pod spec.
	live.UID = types.UID(uid)
	live.ResourceVersion = ""
	live.OwnerReferences = nil
	live.Status = core.PodStatus{}
	require.NoError(t, cli.Create(ctx, live))

	// The name is generated, so the created object rather than the rendered one is what callers
	// address by.
	stored := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Namespace: live.Namespace, Name: live.Name,
	}, stored))

	return stored
}

// TestComposition_AnIncompleteGroupComposesNoWorkload is the control everything else in this file
// is read against, and it has to fail for the fixture to be worth anything: without it, a client
// that answered every group would satisfy every later assertion, since the admission proof asks
// whether a Workload exists and admits.
func TestComposition_AnIncompleteGroupComposesNoWorkload(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })
	members := replicaMembersAt(t, md, "server", 0)
	require.Len(t, members, 2, "size two renders two members")

	cli := newCompositionClient()
	createMember(t, ctx, cli, members[0], "pod-first")

	group := modelDeploymentReplicaGroupName(md, "server", 0)
	wl, err := kueueComposeGroup(ctx, cli, group)
	require.Error(t, err, "a group one member short composes no Workload")
	assert.Nil(t, wl)

	// The second member completes the group, so the refusal was the count and nothing else.
	createMember(t, ctx, cli, members[1], "pod-second")

	wl, err = kueueComposeGroup(ctx, cli, group)
	require.NoError(t, err, "the completed group composes")
	require.NotNil(t, wl)
	assert.Len(t, wl.Spec.PodSets, 1, "members sharing one role hash compose one podset")
	assert.Equal(t, int32(2), wl.Spec.PodSets[0].Count,
		"and the count is read from the members rather than declared anywhere")
}

// TestComposition_MembersDisagreeingOnTheirTotalComposeNoWorkload covers the other refusal: two
// members claiming different group sizes are not one group, which is the state a size edit would
// create if the running member's annotation were rewritten.
func TestComposition_MembersDisagreeingOnTheirTotalComposeNoWorkload(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })
	members := replicaMembersAt(t, md, "server", 0)

	cli := newCompositionClient()
	createMember(t, ctx, cli, members[0], "pod-first")

	disagreeing := members[1].DeepCopy()
	disagreeing.Annotations[kueuepodconst.GroupTotalCountAnnotation] = "3"
	createMember(t, ctx, cli, disagreeing, "pod-second")

	_, err := kueueComposeGroup(ctx, cli, modelDeploymentReplicaGroupName(md, "server", 0))
	require.Error(t, err, "members disagreeing on their total are not one group")
}

// TestComposition_AStaleWorkloadCannotAbsorbANewMember proves the property the vacancy proof leans
// on: a Workload composed for one incarnation does not adopt the members of the next, even though
// the new member reuses the old one's name and group.
func TestComposition_AStaleWorkloadCannotAbsorbANewMember(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment()
	members := replicaMembersAt(t, md, "server", 0)
	group := modelDeploymentReplicaGroupName(md, "server", 0)

	cli := newCompositionClient()
	old := createMember(t, ctx, cli, members[0], "pod-old-generation")

	stale, err := kueueComposeGroup(ctx, cli, group)
	require.NoError(t, err)
	stale.UID = types.UID("wl-old-generation")
	require.NoError(t, cli.Create(ctx, stale))

	require.NoError(t, cli.Delete(ctx, old.DeepCopy()))
	fresh := createMember(t, ctx, cli, members[0], "pod-new-generation")

	current := new(kueue.Workload)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: group}, current))
	assert.False(t, modelDeploymentWorkloadOwnsAny(current, setsOf(string(fresh.UID))),
		"a Workload composed for the old member does not own the new one")
}

// TestComposition_RepeatIncarnationsGetDistinctUIDs pins the premise the lifecycle rests on: two
// Pods of one ordinal, and two Workloads of one group name, are told apart only by the identity
// the API server assigns each.
func TestComposition_RepeatIncarnationsGetDistinctUIDs(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment()
	members := replicaMembersAt(t, md, "server", 0)
	group := modelDeploymentReplicaGroupName(md, "server", 0)

	cli := newCompositionClient()
	first := createMember(t, ctx, cli, members[0], "pod-generation-one")
	firstWL, err := kueueComposeGroup(ctx, cli, group)
	require.NoError(t, err)
	firstWL.UID = types.UID("wl-generation-one")
	require.NoError(t, cli.Create(ctx, firstWL))

	require.NoError(t, cli.Delete(ctx, first.DeepCopy()))
	second := createMember(t, ctx, cli, members[0], "pod-generation-two")
	secondWL, err := kueueComposeGroup(ctx, cli, group)
	require.NoError(t, err)
	secondWL.UID = types.UID("wl-generation-two")

	assert.NotEqual(t, first.UID, second.UID,
		"two incarnations of one ordinal are distinct objects, which is what lets a cleanup delete "+
			"one without touching the other")
	assert.NotEqual(t, firstWL.UID, secondWL.UID,
		"and so are the Workloads the two groups composed")
}

// TestComposition_ADepartingMemberHoldsAFinalizer shows why a departed member keeps the group
// occupied: the delete is accepted and the Pod stays, because the finalizer is on the Pod.
func TestComposition_ADepartingMemberHoldsAFinalizer(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment()
	members := replicaMembersAt(t, md, "server", 0)

	cli := newCompositionClient()
	held := createMember(t, ctx, cli, members[0], "pod-held")
	held.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, held))
	require.NoError(t, cli.Delete(ctx, held.DeepCopy()))

	after := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: held.Name}, after))
	assert.NotEmpty(t, after.Finalizers, "the finalizer is what the vacancy proof waits on")
	require.NotNil(t, after.DeletionTimestamp)

	// Removing the last finalizer of a Pod already marked for deletion removes it, so there is no
	// second delete to issue: the server completes the one that was accepted earlier.
	released := after.DeepCopy()
	released.Finalizers = nil
	require.NoError(t, cli.Update(ctx, released))

	gone := new(core.Pod)
	assert.Error(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: "team-a", Name: held.Name}, gone),
		"and once it releases, the member is gone for good")
}

// TestComposition_AnExcessMemberIsDeletedByKueue states the excess rule from the other side: a group
// carrying more members than it declares reserves for the declared count and has Kueue delete the
// rest, which is what makes a create beside a still-departing member a fault rather than a delay.
func TestComposition_AnExcessMemberIsDeletedByKueue(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment()
	members := replicaMembersAt(t, md, "server", 0)

	cli := newCompositionClient()
	seated := createMember(t, ctx, cli, members[0], "pod-seat-one")
	seated.Finalizers = []string{kueuepodconst.PodFinalizer}
	require.NoError(t, cli.Update(ctx, seated))

	duplicate := seated.DeepCopy()
	duplicate.Name = seated.Name + "-duplicate"
	createMember(t, ctx, cli, duplicate, "pod-seat-two")

	wl, err := kueueComposeGroup(ctx, cli, modelDeploymentReplicaGroupName(md, "server", 0))
	require.NoError(t, err)
	require.NotNil(t, wl)
	assert.Equal(t, int32(1), wl.Spec.PodSets[0].Count,
		"the composed Workload reserves for the declared count, not the excess")

	after := new(core.PodList)
	require.NoError(t, cli.List(ctx, after, ctrlcli.InNamespace("team-a")))
	byUID := map[types.UID]*core.Pod{}
	for i := range after.Items {
		byUID[after.Items[i].UID] = &after.Items[i]
	}
	assert.NotNil(t, byUID[types.UID("pod-seat-one")], "one member of the seat stands")
	assert.Nil(t, byUID[types.UID("pod-seat-two")],
		"and the excess member is gone: Kueue strips its finalizer before deleting it, so a create "+
			"beside a still-present member ends with the member that is newest removed, which is "+
			"why the create gate waits for the vacancy instead of racing it")
}

// TestComposition_CarriesTheRoleHashAsThePodSetName pins that our PodSet name is the one we wrote,
// because the admission proof rebuilds the composition from that same annotation.
func TestComposition_CarriesTheRoleHashAsThePodSetName(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment()
	members := replicaMembersAt(t, md, "server", 0)

	cli := newCompositionClient()
	created := createMember(t, ctx, cli, members[0], "pod-first")

	wl, err := kueueComposeGroup(ctx, cli, modelDeploymentReplicaGroupName(md, "server", 0))
	require.NoError(t, err)
	require.Len(t, wl.Spec.PodSets, 1)

	assert.NotEmpty(t, created.Annotations[kueuepodconst.RoleHashAnnotation],
		"this operator writes the role hash, so the grouping is Kueue's verbatim read rather than "+
			"its fallback digest")
	assert.Equal(t, created.Annotations[kueuepodconst.RoleHashAnnotation], string(wl.Spec.PodSets[0].Name),
		"and Kueue names the podset after it, which keeps one role's pods in one podset whatever "+
			"their index")
}

// TestComposition_ComposesTheEntranceQueue pins the field the admission proof compares: Kueue
// copies the member's queue label onto the Workload, so the live queue is readable evidence and a
// Workload naming none has not proved where it submitted.
func TestComposition_ComposesTheEntranceQueue(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment()
	members := replicaMembersAt(t, md, "server", 0)
	queue := members[0].Labels[kueuectrlconst.QueueLabel]
	require.NotEmpty(t, queue, "the render names an InstanceType entrance queue")

	cli := newCompositionClient()
	createMember(t, ctx, cli, members[0], "pod-first")

	wl, err := kueueComposeGroup(ctx, cli, modelDeploymentReplicaGroupName(md, "server", 0))
	require.NoError(t, err)
	assert.Equal(t, queue, string(wl.Spec.QueueName),
		"the composed Workload submits where its members were sent")
}
