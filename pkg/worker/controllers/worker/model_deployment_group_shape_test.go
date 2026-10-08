package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// deployedMember builds one member of a replica, carrying only what the deployed reader reads: the
// Kueue group total it was rendered with, the seat it claims, and the serving-port annotation the
// render stamps on a command line it built.
//
// THE PORT ANNOTATION IS THE EXECUTION MARKER BECAUSE THAT IS WHAT THE RENDER DOES: it stamps
// prometheus.io/port only when the operator wrote the command. A fixture that invented a separate
// label would test the fixture rather than the contract.
func deployedMember(name string, total, seat int, managed, labelled bool) *core.Pod {
	pod := &core.Pod{}
	pod.Name = name
	pod.Annotations = map[string]string{}
	if total > 0 {
		pod.Annotations[kueuepodconst.GroupTotalCountAnnotation] = strconvx.Itoa(total)
	}
	if managed {
		pod.Annotations["prometheus.io/port"] = "8000"
	}
	if labelled {
		pod.Labels = map[string]string{modelDeploymentMemberIndexLabel: strconvx.Itoa(seat)}
	}
	pod.Spec.Containers = []core.Container{{Name: modelDeploymentMainContainerName}}

	return pod
}

// seatedMember is a member whose seat label is present but carries something that is not a seat,
// which is the corrupt case the reader must not confuse with a Pod written before labels existed.
func seatedMember(name string, total int, seat string) *core.Pod {
	pod := deployedMember(name, total, 0, true, true)
	pod.Labels[modelDeploymentMemberIndexLabel] = seat

	return pod
}

// deployedReplica is the same fixture for a replica, so the cases below read as the group they
// describe rather than as a slice assembled three times.
func deployedReplica(role string, ordinal int, members ...*core.Pod) modelDeploymentReplicaView {
	view := modelDeploymentReplicaView{Role: role, Ordinal: ordinal, Seated: true}
	view.Members = append(view.Members, members...)

	return view
}

func TestModelDeploymentDeployedReplicaShapeReadsThePodOwnTotal(t *testing.T) {
	cases := []struct {
		name      string
		view      modelDeploymentReplicaView
		total     int
		state     modelDeploymentReplicaShapeState
		whole     bool
		reasonHas string
	}{
		{
			name:  "a single-member replica is whole at the total it was rendered with",
			view:  deployedReplica("server", 0, deployedMember("a", 1, 0, true, true)),
			total: 1, state: modelDeploymentReplicaShapeComplete, whole: true,
		},
		{
			name: "a multi-member replica holding every seat is whole",
			view: deployedReplica("server", 0,
				deployedMember("a", 2, 0, true, true), deployedMember("b", 2, 1, true, true)),
			total: 2, state: modelDeploymentReplicaShapeComplete, whole: true,
		},
		{
			// The fact this whole task exists: a replica the spec no longer counts is still whole.
			name: "a replica short of the DESIRED count is whole at the count it was built at",
			view: deployedReplica("server", 0,
				deployedMember("a", 2, 0, true, true), deployedMember("b", 2, 1, true, true)),
			total: 2, state: modelDeploymentReplicaShapeComplete, whole: true,
		},
		{
			name:  "a replica that lost a member is incomplete at the total it declared",
			view:  deployedReplica("server", 0, deployedMember("a", 2, 0, true, true)),
			total: 2, state: modelDeploymentReplicaShapeIncomplete,
			reasonHas: "1 of 2 members",
		},
		{
			// Every deployment predating multi-member replicas is full of these, so a Pod with no
			// total at all is a replica of one rather than an unreadable object.
			name:  "a member declaring no total is a replica of one",
			view:  deployedReplica("server", 0, deployedMember("a", 0, 0, true, true)),
			total: 1, state: modelDeploymentReplicaShapeComplete, whole: true,
		},
		{
			// The legacy fallback is for ONE member and nothing else. Replicas written before the
			// seat label existed were exactly the single-member ones, so a group of several with no
			// seats is a shape this operator never produced and cannot be read as whole.
			name:      "a multi-member replica whose members claim no seats cannot be classified",
			view:      deployedReplica("server", 0, deployedMember("a", 2, 0, true, false), deployedMember("b", 2, 1, true, false)),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "no member declares which seat it holds",
		},
		{
			// THE CONTROL THE REVIEW NAMED. Two members, one seat label each, both unparsable. The
			// seats are present but useless, which is not the same as absent, and a reader that
			// collapsed the two counted two members against a total of two and called it whole.
			name: "members whose seat labels are present but unparsable cannot be classified",
			view: deployedReplica("server", 0,
				seatedMember("a", 2, "bad"), seatedMember("b", 2, "bad")),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "declare a member index that is not one",
		},
		{
			name:      "a member whose seat is a negative number cannot be classified",
			view:      deployedReplica("server", 0, deployedMember("a", 2, 0, true, true), seatedMember("b", 2, "-1")),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "declare a member index that is not one",
		},
		{
			// One member absent both its seat and its total is the Pod from before either existed,
			// and the single shape the reader may still call whole with no seat evidence at all.
			name:  "a lone member carrying neither total nor seat is the legacy single-member replica",
			view:  deployedReplica("server", 0, deployedMember("a", 0, 0, true, false)),
			total: 1, state: modelDeploymentReplicaShapeComplete, whole: true,
		},
		{
			name: "members declaring different totals cannot be classified",
			view: deployedReplica("server", 0,
				deployedMember("a", 2, 0, true, true), deployedMember("b", 1, 0, true, true)),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "group totals of 2 and 1",
		},
		{
			name: "two members claiming one seat cannot be classified",
			view: deployedReplica("server", 0,
				deployedMember("a", 2, 0, true, true), deployedMember("b", 2, 0, true, true)),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "both claim seat 0",
		},
		{
			name: "a member holding a seat outside the total cannot be classified",
			view: deployedReplica("server", 0,
				deployedMember("a", 2, 0, true, true),
				deployedMember("b", 2, 1, true, true), deployedMember("c", 2, 2, true, true)),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "claims seat 2",
		},
		{
			name: "a replica where only some members claim a seat cannot be classified",
			view: deployedReplica("server", 0,
				deployedMember("a", 2, 0, true, true), deployedMember("b", 2, 1, true, false)),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "declare which seat they hold",
		},
		{
			name:      "a replica holding no members has no shape to read",
			view:      deployedReplica("server", 0),
			state:     modelDeploymentReplicaShapeUnreadable,
			reasonHas: "no members",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shape := modelDeploymentDeployedReplicaShape(tc.view)

			assert.Equal(t, tc.state, shape.State)
			assert.Equal(t, tc.total, shape.Total)
			assert.Equal(t, tc.whole, shape.modelDeploymentReplicaIsWhole())
			if tc.reasonHas != "" {
				assert.Contains(t, shape.Reason, tc.reasonHas)
			} else if shape.State == modelDeploymentReplicaShapeComplete {
				assert.Empty(t, shape.Reason, "a whole replica has nothing to explain")
			}
		})
	}
}

func TestModelDeploymentDeployedReplicaShapeRejectsATotalNoReplicaWasBuiltAt(t *testing.T) {
	for _, total := range []string{"0", "-1", "many", "1.5"} {
		t.Run(total, func(t *testing.T) {
			member := deployedMember("a", 1, 0, true, true)
			member.Annotations[kueuepodconst.GroupTotalCountAnnotation] = total

			shape := modelDeploymentDeployedReplicaShape(deployedReplica("server", 0, member))

			assert.Equal(t, modelDeploymentReplicaShapeUnreadable, shape.State)
			assert.Zero(t, shape.Total, "an unreadable shape carries no total to read figures against")
			assert.Contains(t, shape.Reason, "not a replica size")
		})
	}
}

func TestModelDeploymentDeployedReplicaExecutionReadsTheRenderedCommandLine(t *testing.T) {
	cases := []struct {
		name      string
		members   []*core.Pod
		execution modelDeploymentReplicaExecution
		reasonHas string
	}{
		{
			name:      "a replica whose hook the operator wrote is managed",
			members:   []*core.Pod{deployedMember("a", 1, 0, true, true)},
			execution: modelDeploymentExecutionManaged,
		},
		{
			name:      "a replica running a command line the operator contributed nothing to is a takeover",
			members:   []*core.Pod{deployedMember("a", 1, 0, false, true)},
			execution: modelDeploymentExecutionTakeover,
		},
		{
			name: "every member of a takeover replica agrees on it",
			members: []*core.Pod{
				deployedMember("a", 2, 0, false, true), deployedMember("b", 2, 1, false, true),
			},
			execution: modelDeploymentExecutionTakeover,
		},
		{
			name: "members disagreeing on who wrote their command line cannot be classified",
			members: []*core.Pod{
				deployedMember("a", 2, 0, true, true), deployedMember("b", 2, 1, false, true),
			},
			execution: modelDeploymentExecutionUnreadable,
			reasonHas: "disagree on who built",
		},
		{
			// A member this operator has not read is not a member to skip: the replica is not
			// classifiable from the members that happened to be legible.
			name: "a member with no engine container makes the replica unreadable",
			members: []*core.Pod{
				deployedMember("a", 2, 0, true, true), deployedNoEngineMember("b"),
			},
			execution: modelDeploymentExecutionUnreadable,
			reasonHas: "member b carries no engine container",
		},
		{
			name:      "a replica with nothing to read is unreadable",
			members:   []*core.Pod{deployedNoEngineMember("a")},
			execution: modelDeploymentExecutionUnreadable,
			reasonHas: "carries no engine container",
		},
		{
			name:      "a replica holding no members has no command line to read",
			execution: modelDeploymentExecutionUnreadable,
			reasonHas: "no members",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execution, reason := modelDeploymentDeployedReplicaExecution(tc.members)

			require.Equal(t, tc.execution, execution)
			if tc.reasonHas != "" {
				assert.Contains(t, reason, tc.reasonHas)
			} else {
				assert.Empty(t, reason)
			}
		})
	}
}

func deployedNoEngineMember(name string) *core.Pod {
	pod := &core.Pod{}
	pod.Name = name
	pod.Spec.Containers = []core.Container{{Name: "sidecar"}}

	return pod
}

// oldReplicaPods are the members a role rendered BEFORE an edit: the given group size, the given
// command ownership, and nothing else about them tied to what the role says now.
//
// IT IS WRITTEN THE WAY THE RENDER WRITES A MEMBER -- the group total, the seat, and the
// serving-port annotation the render stamps only on a command line it built. A takeover replica is
// the same member with the port annotation absent, because that is precisely what the render emits
// for a role that supplies its own command.
func oldReplicaPods(md *workercore.ModelDeployment, ordinal, size int, managed bool) []core.Pod {
	pods := make([]core.Pod, 0, size)
	for member := range size {
		pod := healthPod("server", ordinal, member, healthBool(true),
			"uid-old-"+strconvx.Itoa(ordinal)+"-"+strconvx.Itoa(member))
		pod.Annotations = map[string]string{
			kueuepodconst.GroupTotalCountAnnotation: strconvx.Itoa(size),
		}
		pod.Spec.Containers = []core.Container{{
			Name: modelDeploymentMainContainerName, Image: "vllm/vllm-openai:v0.29.0",
		}}
		if managed {
			pod.Annotations["prometheus.io/port"] = "8000"
		}
		pods = append(pods, pod)
	}

	return pods
}

// TestDeployedFactsSurviveADesiredSizeOrCommandEdit is the acceptance this whole task is written
// against, asserted through every observation T1 owns rather than through the reader alone.
//
// THE DEPLOYMENT HAS BEEN EDITED TWICE: the role now asks for four members where these replicas
// were built at two, and it now declares no command of its own where these replicas were built as a
// takeover. Nothing about the running Pods changed. What each reader must still say about them:
//
//   - the replica is WHOLE, because it holds every member of the group it declared;
//   - it QUALIFIES, so its endpoints stay selected and the deployment keeps serving;
//   - status counts it READY, because it is serving exactly what it always served;
//   - the cache condition can still ASK it, and does not claim a client for one whose command line
//     the operator did not write;
//   - its departure is CLASSIFIABLE, so an edit can actually retire it rather than freezing it.
//
// EVERY FIGURE HERE WOULD READ THE OTHER WAY IF ANY READER STILL TOOK THE ROLE'S SIZE, and the
// point of asserting them together is that they are one fact read in five places rather than five
// answers that can drift apart.
func TestDeployedFactsSurviveADesiredSizeOrCommandEdit(t *testing.T) {
	md := healthDeployment(4)
	pods := oldReplicaPods(md, 0, 2, true)

	view := modelDeploymentGroupPodsByReplica(pods)
	require.Len(t, view, 1)

	t.Run("the replica is whole at the size it was built at", func(t *testing.T) {
		shape := modelDeploymentDeployedReplicaShape(view[0])

		assert.Equal(t, 2, shape.Total, "the group declares two, whatever the role now asks for")
		assert.True(t, shape.modelDeploymentReplicaIsWhole())
	})

	t.Run("it qualifies, so its endpoints are not withdrawn", func(t *testing.T) {
		qualifications := qualifyModelDeploymentInstances(
			context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch,
		)
		require.Len(t, qualifications, 1)
		assert.True(t, qualifications[0].Eligible(),
			"a healthy replica the role has been edited away from is still serving")
		assert.False(t, qualifications[0].HasFailure())
	})

	t.Run("status counts it ready", func(t *testing.T) {
		qualifications := qualifyModelDeploymentInstances(
			context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch,
		)
		statuses := modelDeploymentRoleStatuses(
			md, pods, nil, modelDeploymentRoleQualified(qualifications),
		)
		require.Len(t, statuses, 1)
		assert.Equal(t, int32(1), statuses[0].Ready,
			"the replica is serving; a desired-size edit does not unserve it")
		assert.False(t, statuses[0].Unmanaged)
	})

	t.Run("the cache can ask it", func(t *testing.T) {
		assert.Len(t, modelDeploymentReadyReplicas(md, pods), 1)

		unclaimable, _, _ := modelDeploymentUnclaimableReplica(md, pods)
		assert.Empty(t, unclaimable, "these replicas were built from a managed command line")
	})

	t.Run("its departure is classifiable", func(t *testing.T) {
		shape, reason := modelDeploymentRetirementAnsweringShape(md, view[0].Members)

		assert.Empty(t, reason, "an edit must not freeze the replica it means to retire")
		assert.Equal(t, modelDeploymentAnsweringLeader, shape)
	})
}

// TestDeployedFactsFollowTheReplicasNotTheRole is the reverse transition for command ownership.
//
// THE ROLE IS DECLARED MANAGED AND THE RUNNING PODS ARE NOT, which is the state an operator creates
// by clearing Command while the old takeover replicas are still up. Reading the role would report a
// cache client for containers that never received one.
func TestDeployedFactsFollowTheReplicasNotTheRole(t *testing.T) {
	md := healthDeployment(2)
	pods := oldReplicaPods(md, 0, 2, false)

	execution, reason := modelDeploymentDeployedReplicaExecution(
		modelDeploymentGroupPodsByReplica(pods)[0].Members)
	assert.Empty(t, reason)
	assert.Equal(t, modelDeploymentExecutionTakeover, execution)

	replica, reason, why := modelDeploymentUnclaimableReplica(md, pods)
	assert.NotEmpty(t, replica, "a replica running a command line we did not build is named")
	assert.Equal(t, modelDeploymentReasonUnmanaged, reason)
	assert.NotEmpty(t, why)

	qualifications := qualifyModelDeploymentInstances(
		context.Background(), md, pods, modelDeploymentPendingReplacement{}, boundProbeFetch,
	)
	statuses := modelDeploymentRoleStatuses(
		md, pods, nil, modelDeploymentRoleQualified(qualifications),
	)
	require.Len(t, statuses, 1)
	assert.True(t, statuses[0].Unmanaged,
		"the field reports what is running, which is not what the role now declares")
}

// renderedReplica renders one replica member, so the execution facts are read off what this
// operator actually emits.
func renderedReplica(
	t *testing.T, mutate ...func(*workercore.ModelDeployment),
) []*core.Pod {
	t.Helper()

	md := newRenderDeployment(mutate...)
	pod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
	})
	require.NoError(t, err)

	return []*core.Pod{pod}
}

// TestRenderedPodsCarryTheirExecution reads execution facts off rendered Pods.
func TestRenderedPodsCarryTheirExecution(t *testing.T) {
	t.Run("a managed render reads as managed", func(t *testing.T) {
		execution, why := modelDeploymentDeployedReplicaExecution(renderedReplica(t))

		assert.Empty(t, why)
		assert.Equal(t, modelDeploymentExecutionManaged, execution)
	})

	t.Run("a role supplying its own command line reads as a takeover", func(t *testing.T) {
		execution, why := modelDeploymentDeployedReplicaExecution(renderedReplica(t,
			func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"custom-server"}
			}))

		assert.Empty(t, why)
		assert.Equal(t, modelDeploymentExecutionTakeover, execution)
	})

	t.Run("a managed render stripped of its drain hook is still managed", func(t *testing.T) {
		// The Elastic realization clears every Ray worker's Lifecycle, so a reader asking for the
		// hook calls a managed worker a takeover replica.
		members := renderedReplica(t)
		for _, member := range members {
			for i := range member.Spec.Containers {
				member.Spec.Containers[i].Lifecycle = nil
			}
		}

		execution, why := modelDeploymentDeployedReplicaExecution(members)

		assert.Empty(t, why)
		assert.Equal(t, modelDeploymentExecutionManaged, execution,
			"the Elastic worker transformation must not change what the reader concludes")
	})
}

// TestRenderedPodsCarryTheirGroupTotal is the other half of the same argument: the size the replica
// is measured against is one the renderer writes, so a reader that wants it does not need the spec.
func TestRenderedPodsCarryTheirGroupTotal(t *testing.T) {
	members := renderedReplica(t)
	shape := modelDeploymentDeployedReplicaShape(deployedReplica("server", 0,
		members[0].DeepCopy()))

	assert.Equal(t, modelDeploymentReplicaShapeComplete, shape.State)
	assert.Equal(t, 1, shape.Total, "a single-member role renders a group of one")
}

// TestActualElasticRenderCarriesExecutionFacts drives renderModelDeploymentPods on an Elastic
// deployment and asserts the execution facts of the Pods it emits.
func TestActualElasticRenderCarriesExecutionFacts(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.KVCache = nil
	md.Spec.Engine.Version = "0.29.0"
	md.Spec.Roles[0].Replicas = 1
	md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
		Accelerator: resource.NewQuantity(1, resource.DecimalSI),
	}
	md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2}
	md.ResourceVersion = "1"
	require.NotNil(t, ModelDeploymentElasticRole(md), "the dispatcher takes the elastic path")

	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(md, newRenderInstanceType()).
		WithStatusSubresource(&workercore.ModelDeployment{}).
		WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithIndex(&core.Pod{}, modelDeploymentDrainIndexPodUID, func(obj ctrlcli.Object) []string {
			return []string{string(obj.GetUID())}
		}).Build()

	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	rendered, err := r.renderModelDeploymentPods(context.Background(), md, nil, nil, nil)
	require.NoError(t, err)

	var members []core.Pod
	for _, ordinals := range rendered {
		for _, pods := range ordinals {
			for _, pod := range pods {
				members = append(members, *pod)
			}
		}
	}
	require.NotEmpty(t, members, "the elastic render produced no members")

	// Master and Ray worker both carry the port, and the render strips the worker's drain hook.
	head := modelDeploymentElasticHeadName(md)
	workers := 0
	for _, member := range members {
		if member.Labels[modelDeploymentLabelKeyComponent] == head {
			continue
		}
		workers++
		view := deployedReplica("server", 0, member.DeepCopy())
		execution, why := modelDeploymentDeployedReplicaExecution(view.Members)

		assert.Empty(t, why, "member %s", member.Name)
		assert.Equal(t, modelDeploymentExecutionManaged, execution,
			"member %s is an operator-built replica and must not read as a takeover", member.Name)
		assert.Equal(t, modelDeploymentReplicaShapeComplete,
			modelDeploymentDeployedReplicaShape(view).State, "member %s", member.Name)
	}
	assert.GreaterOrEqual(t, workers, 2, "the elastic render produced a master and at least one Ray worker")

	// The head is part of the render and is not a declared role.
	var sawHead bool
	for _, member := range members {
		if member.Labels[modelDeploymentLabelKeyComponent] == head {
			sawHead = true
		}
	}
	require.True(t, sawHead, "the elastic render emitted its auxiliary head")
}

// TestAuxiliaryHeadIsNeverClaimed covers the auxiliary head, which is built by this operator and
// carries none of its annotations. It is not a declared role, so neither the cache condition nor
// status may name it.
func TestAuxiliaryHeadIsNeverClaimed(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.KVCache = nil
	md.Spec.Roles[0].Replicas = 1
	head := renderModelDeploymentElasticHeadPod(md, &md.Spec.Roles[0], core.Container{
		Name: modelDeploymentMainContainerName, Image: "vllm/vllm-openai:v0.29.0",
	})
	head.Name, head.UID = modelDeploymentElasticHeadName(md), types.UID("elastic-head-uid")
	head.Annotations = map[string]string{}

	replica, reason, why := modelDeploymentUnclaimableReplica(md, []core.Pod{*head})
	assert.Empty(t, replica,
		"a role the spec does not declare is not asked about; reason %q why %q", reason, why)

	statuses := modelDeploymentRoleStatuses(md, []core.Pod{*head}, nil, nil)
	require.Len(t, statuses, 1)
	assert.False(t, statuses[0].Unmanaged,
		"the declared role reports on its own members, not on an auxiliary head")
}
