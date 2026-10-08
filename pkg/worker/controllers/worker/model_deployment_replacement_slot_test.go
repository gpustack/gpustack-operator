package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// composeReplicaGroup builds the Workload a set of group members composes to, following the rule
// replacementCompositionHolds checks against: one PodSet per role hash, count from the members
// carrying it, and the reservation read off the template's containers.
//
// The negative controls below are what matter here. Each asserts that the production admission
// proof rejects a Workload that IS admitted and DOES own the current members while disagreeing on
// count, template, reservation or queue, so they test this task's checks rather than the fixture.

// composeReplicaGroup builds the Workload a set of group members composes to.
func composeReplicaGroup(md *workercore.ModelDeployment, role string, ordinal int,
	members []*core.Pod,
) *kueue.Workload {
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace = modelDeploymentReplicaGroupName(md, role, ordinal), md.Namespace
	wl.UID = types.UID("wl-" + wl.Name)
	wl.Spec.QueueName = kueue.LocalQueueName(members[0].Labels[kueuectrlconst.QueueLabel])
	wl.Spec.PodSets = replacementPinnedPodSets(members)
	// Kueue owns EVERY member, one reference each, and the admission proof requires all of them:
	// a Workload claiming one member of a group is admitting a different group.
	for _, member := range members {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: member.Name, UID: member.UID,
		})
	}

	return wl
}

// admit records the Workload as admitted, which is the state a replacement has to reach before its
// slot may be released.
func admit(wl *kueue.Workload) *kueue.Workload {
	wl.Status.Conditions = []meta.Condition{{
		Type:               kueue.WorkloadAdmitted,
		Status:             meta.ConditionTrue,
		Reason:             "Admitted",
		LastTransitionTime: meta.Now(),
	}}

	return wl
}

// TestReplacementAdmission_AnHonestCompositionIsAccepted is the positive case the negative controls
// are read against: a Workload composed from the current members releases the slot.
func TestReplacementAdmission_AnHonestCompositionIsAccepted(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })
	members := replicaMembersAt(t, md, "server", 0)
	require.Len(t, members, 2)

	wl := admit(composeReplicaGroup(md, "server", 0, members))

	reason := replacementAdmissionHolds(wl, members, members)
	assert.Empty(t, reason, "a Workload composed from these members is fresh admission")
}

// TestReplacementAdmission_AFollowerRepresentativeIsStillFreshAdmission covers the representative
// Kueue happened to list. Its List carries no order, and rank-specific members share one role hash
// by design, so a template composed from m1 is correct even when the reader sees m0 first.
func TestReplacementAdmission_AFollowerRepresentativeIsStillFreshAdmission(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })
	members := replicaMembersAt(t, md, "server", 0)
	require.Len(t, members, 2)

	wl := admit(composeReplicaGroup(md, "server", 0, members))
	wl.Spec.PodSets = replacementPinnedPodSets(members)
	wl.Spec.PodSets[0].Template = core.PodTemplateSpec{Spec: *members[1].Spec.DeepCopy()}

	assert.Empty(t, replacementAdmissionHolds(wl, members, members),
		"a template composed from any member of the group is a template of that group")
}

// TestReplacementAdmission_ARequestOnlyPodIsReservedAsKubernetesReadsIt covers the reservation rule
// this operator does not own: a request smaller than the limit is reserved on the request, and
// several containers sum.
func TestReplacementAdmission_ARequestOnlyPodIsReservedAsKubernetesReadsIt(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })
	members := replicaMembersAt(t, md, "server", 0)

	withRequests := members[0].DeepCopy()
	withRequests.Spec.Containers[0].Resources = core.ResourceRequirements{
		Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("4")},
		Limits:   core.ResourceList{core.ResourceCPU: resource.MustParse("16")},
	}
	sidecar := core.Container{Name: "sidecar", Image: "busybox"}
	sidecar.Resources.Requests = core.ResourceList{core.ResourceCPU: resource.MustParse("2")}
	withRequests.Spec.Containers = append(withRequests.Spec.Containers, sidecar)

	wl := admit(composeReplicaGroup(md, "server", 0, members))
	wl.Spec.PodSets = replacementPinnedPodSets([]*core.Pod{withRequests})

	group := []*core.Pod{withRequests}
	assert.Empty(t, replacementAdmissionHolds(wl, group, group),
		"a reservation of request plus sidecar is the reservation the members compose to")

	// The same pod shape, reserving less: the fault is the reservation, not the template.
	understated := wl.DeepCopy()
	understated.Spec.PodSets[0].Template.Spec.Containers[1].Resources.Requests = core.ResourceList{core.ResourceCPU: resource.MustParse("1")}
	assert.Contains(t, replacementAdmissionHolds(understated, group, group), "reserves resources",
		"a reservation short of what the sidecar requests reserves for a smaller Pod than the group")

	// A reservation that drops the sidecar outright is a different shape, and is refused as one.
	truncated := wl.DeepCopy()
	truncated.Spec.PodSets[0].Template.Spec.Containers = withRequests.Spec.Containers[:1]
	assert.Contains(t, replacementAdmissionHolds(truncated, group, group), "do not match",
		"and a template naming fewer containers describes a Pod the group does not have")
}

// TestReplacementAdmission_AnAdmittedPlacementIsNotAStaleReservation covers the mutation admission
// makes: the Workload holds the pre-admission template, the live Pod carries what the admission
// chain added and consumed.
func TestReplacementAdmission_AnAdmittedPlacementIsNotAStaleReservation(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })
	members := replicaMembersAt(t, md, "server", 0)
	require.Len(t, members, 2)

	// The Workload is composed by Kueue's own constructor from these complete members, because a
	// PodSet is the pre-admission template: it is a copy taken before the scheduler and the
	// admission chain touched anything.
	ctx := context.Background()
	cli := newCompositionClient()
	group := modelDeploymentReplicaGroupName(md, "server", 0)
	stamped := make([]*core.Pod, 0, len(members))
	for i, member := range members {
		stamped = append(stamped, createMember(t, ctx, cli, member,
			"pod-placed-"+strconvx.Itoa(i)))
	}

	wl, err := kueueComposeGroup(ctx, cli, group)
	require.NoError(t, err)
	require.NotNil(t, wl)
	require.Len(t, wl.Spec.PodSets, 1)
	require.Equal(t, int32(2), wl.Spec.PodSets[0].Count,
		"the count is read from the members rather than declared anywhere")
	wl.UID = types.UID("wl-" + wl.Name)
	for _, member := range stamped {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: member.Name, UID: member.UID,
		})
	}
	wl = admit(wl)

	// What the cluster then does to the members: the admission chain adds a selector and a
	// toleration and consumes a gate, and the scheduler fills in the node it placed the member on.
	placed := make([]*core.Pod, 0, len(stamped))
	for i, member := range stamped {
		placedMember := member.DeepCopy()
		placedMember.Spec.NodeSelector = map[string]string{"nvidia.com/gpu.present": "true"}
		placedMember.Spec.Tolerations = []core.Toleration{{
			Key: "nvidia.com/gpu", Operator: core.TolerationOpExists, Effect: core.TaintEffectNoSchedule,
		}}
		placedMember.Spec.SchedulingGates = nil
		placedMember.Spec.NodeName = "worker-" + strconvx.Itoa(i)
		placed = append(placed, placedMember)
	}

	assert.Empty(t, replacementAdmissionHolds(wl, placed, placed),
		"an added selector, an added toleration, a consumed gate and a filled node are one "+
			"configuration seen before and after admission, not two")

	changed := wl.DeepCopy()
	changed.Spec.PodSets[0].Template.Spec.Containers[0].Image = "vllm/vllm-openai:v0.0.1"
	assert.Contains(t, replacementAdmissionHolds(changed, placed, placed), "do not match",
		"while a rendered field the live Pods do not carry is still refused")

	shortened := wl.DeepCopy()
	shortened.Spec.PodSets[0].Template.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"] = resource.MustParse("2")
	assert.Contains(t, replacementAdmissionHolds(shortened, placed, placed), "reserves",
		"and a Workload reserving a different amount is still refused")
}

// TestReplacementAdmission_ControlsRejectAWorkloadThatIsAdmittedButDescribesAnotherGroup is the
// negative control set, and each case answers one question: a Workload that Kueue has admitted and
// that owns the current members still releases nothing unless its scheduling inputs describe the
// group those members make.
func TestReplacementAdmission_ControlsRejectAnAdmittedButStaleWorkload(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })

	cases := []struct {
		name     string
		mutate   func(*kueue.Workload)
		wantText string
	}{
		{
			name: "an admitted workload on the wrong queue",
			mutate: func(wl *kueue.Workload) {
				wl.Spec.QueueName = kueue.LocalQueueName("queue-for-someone-else")
			},
			wantText: "queued on",
		},
		{
			name: "an admitted workload naming no queue at all",
			mutate: func(wl *kueue.Workload) {
				wl.Spec.QueueName = ""
			},
			wantText: "queued on",
		},
		{
			name: "an admitted workload reserving too few pods",
			mutate: func(wl *kueue.Workload) {
				wl.Spec.PodSets[0].Count = 1
			},
			wantText: "asks for",
		},
		{
			name: "an admitted workload describing a template its members do not match",
			mutate: func(wl *kueue.Workload) {
				wl.Spec.PodSets[0].Template.Spec.Containers[0].Image = "vllm/vllm-openai:v0.0.1"
			},
			wantText: "do not match",
		},
		{
			name: "an admitted workload reserving resources its members do not declare",
			mutate: func(wl *kueue.Workload) {
				// The request, not the limit: Kubernetes reads the request when both are named, so
				// a limit-only difference is not a difference in the reservation.
				wl.Spec.PodSets[0].Template.Spec.Containers[0].Resources.Requests = core.ResourceList{core.ResourceMemory: resource.MustParse("512Gi")}
			},
			wantText: "reserves resources",
		},
		{
			name: "an admitted workload carrying a podset for nothing else",
			mutate: func(wl *kueue.Workload) {
				wl.Spec.PodSets = append(wl.Spec.PodSets, kueue.PodSet{
					Name:  "a-podset-for-another-shape",
					Count: 1,
				})
			},
			wantText: "podsets",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			members := replicaMembersAt(t, md, "server", 0)
			require.Len(t, members, 2)

			wl := admit(composeReplicaGroup(md, "server", 0, members))
			tc.mutate(wl)

			reason := replacementAdmissionHolds(wl, members, members)
			assert.NotEmpty(t, reason,
				"an admitted workload describing another group releases nothing")
			assert.Contains(t, reason, tc.wantText,
				"and the reason names the disagreement, so an operator reading the status can act on it")
		})
	}
}

// TestReplacementAdmission_AStaleWorkloadDoesNotAdmitANewGroup covers the stale case directly: the
// ownership is current-looking, because a recreated Pod takes the same group, and only the
// scheduling inputs reveal that the Workload was composed before it.
func TestReplacementAdmission_AStaleWorkloadDoesNotAdmitANewGroup(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].ReplicaSize = 2 })
	old := replicaMembersAt(t, md, "server", 0)

	stale := admit(composeReplicaGroup(md, "qwen", 0, old))

	// The old members leave and the group is rebuilt at one member per replica, so the workload
	// still owns the names the new members took but describes the group that is gone.
	edited := md.DeepCopy()
	edited.Spec.Roles[0].ReplicaSize = 1
	fresh := replicaMembersAt(t, edited, "server", 0)
	require.Len(t, fresh, 1)

	reason := replacementAdmissionHolds(stale, fresh, fresh)
	assert.NotEmpty(t, reason, "an admission of the previous group is not an admission of this one")
}

// replicaMembersAt runs a pass and returns the members it rendered for one ordinal, each with a
// distinct UID so an ownership match cannot pass on two empty values.
func replicaMembersAt(
	t *testing.T, md *workercore.ModelDeployment, role string, ordinal int,
) []*core.Pod {
	t.Helper()

	cli := newModelDeploymentClient(md, newRenderInstanceType())
	for pass := 0; pass < 4; pass++ {
		_, err := reconcileModelDeploymentWith(t, &ModelDeploymentReconciler{
			Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64),
		})
		require.NoError(t, err, "pass %d", pass)
	}

	members := make([]*core.Pod, 0, 4)
	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodRole(&pod) != role {
			continue
		}
		if at, ok := modelDeploymentPodOrdinal(&pod); !ok || at != ordinal {
			continue
		}
		live := pod.DeepCopy()
		live.UID = types.UID("pod-" + live.Name)
		require.NoError(t, cli.Update(context.Background(), live))
		members = append(members, live)
	}
	require.NotEmpty(t, members, "the render produced no member for ordinal %d", ordinal)

	return members
}
