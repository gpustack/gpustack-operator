package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// readyReplica builds a replica of the fixture deployment at the given ordinal, Ready or not.
func readyReplica(md *workercore.ModelDeployment, ordinal int32, ready bool) *core.Pod {
	pod := &core.Pod{}
	pod.Name = modelDeploymentPodName(md, &md.Spec.Roles[0], ordinal)
	pod.Namespace = md.Namespace
	pod.UID = types.UID(pod.Name + "-uid")
	// A LITERAL rather than FormatLocalQueueName(role.InstanceType): the entrance a replica carries
	// is read from the InstanceType's status, so a fixture that re-derived it from the name would
	// agree with a render that wrongly did the same and stop discriminating between them.
	pod.Labels = modelDeploymentPodLabels(md, &md.Spec.Roles[0], "fixture-entrance")
	// The controller reference is what the reconciler selects on, so a fixture without one is
	// invisible to every path that lists replicas rather than being handed them.
	kubemeta.ControlOnWithoutBlock(pod, md, workercore.SchemeGroupVersionKind("ModelDeployment"))
	if ready {
		pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
	}

	return pod
}

// groupWorkload builds the ONE Kueue Workload a pod group produces, with or without quota reserved.
//
// ITS OWNER REFERENCES CARRY NO CONTROLLER, and that is the fixture's load-bearing detail rather
// than a shortcut. Kueue sets a CONTROLLER reference when it composes a Workload from a single Pod
// and PLAIN owner references to every member when it composes one from a group. A fixture that
// stamped a controller here would make the reader agree with a shape the cluster never produces.
func groupWorkload(pods []core.Pod, reserved bool) *kueue.Workload {
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace = "pod-"+pods[0].Name+"-abcde", pods[0].Namespace
	for i := range pods {
		wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
			APIVersion: "v1", Kind: "Pod", Name: pods[i].Name, UID: pods[i].UID,
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

	return wl
}

// TestComputeModelDeploymentStatus_Phase walks the phase vocabulary. Degraded is the state worth
// telling apart from Starting: the deployment is serving, at less than the capacity asked for.
func TestComputeModelDeploymentStatus_Phase(t *testing.T) {
	testCases := []struct {
		name        string
		replicas    int32
		ready       int32
		deleting    bool
		wantPhase   string
		wantMessage string
	}{
		{name: "nothing ready yet", replicas: 2, ready: 0, wantPhase: ModelDeploymentPhaseStarting},
		{
			name: "some ready", replicas: 4, ready: 2, wantPhase: ModelDeploymentPhaseDegraded,
			wantMessage: "2 of 4 replicas are ready",
		},
		{name: "all ready", replicas: 2, ready: 2, wantPhase: ModelDeploymentPhaseReady},
		{
			name: "on the way out", replicas: 2, ready: 2, deleting: true,
			wantPhase: ModelDeploymentPhaseDeleting,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = tc.replicas
			})
			if tc.deleting {
				now := meta.Now()
				md.DeletionTimestamp = &now
				// An object under deletion is only still readable because a finalizer holds it, and
				// the fake client enforces that rather than letting a fixture exist that could not.
				md.Finalizers = []string{systemmeta.LockedResourceFinalizer}
			}

			objs := []ctrlcli.Object{md, newRenderInstanceType()}
			pods := make([]core.Pod, 0, tc.replicas)
			for i := range tc.replicas {
				pod := readyReplica(md, i, i < tc.ready)
				objs = append(objs, pod)
				pods = append(pods, *pod)
			}

			r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(objs...)}
			status, err := r.computeModelDeploymentStatus(context.Background(), md, pods, nil, nil)
			require.NoError(t, err)

			assert.Equal(t, tc.wantPhase, status.Phase)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, status.PhaseMessage)
			}
		})
	}
}

// TestComputeModelDeploymentStatus_Roles pins the per-role counts, including that a zero is an
// OBSERVED zero. The counts carry no omitempty precisely so that "none ready" and "not measured"
// cannot be confused, and a status built from a Pod list that succeeded must say the first.
func TestComputeModelDeploymentStatus_Roles(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })

	pods := []core.Pod{
		*readyReplica(md, 0, true),
		*readyReplica(md, 1, false),
	}
	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}

	status, err := r.computeModelDeploymentStatus(context.Background(), md, pods, nil, nil)
	require.NoError(t, err)

	require.Len(t, status.Roles, 1)
	assert.Equal(t, "server", status.Roles[0].Name)
	assert.Equal(t, int32(3), status.Roles[0].Desired, "desired comes from the spec, not the Pods")
	assert.Equal(t, int32(1), status.Roles[0].Ready)
	assert.False(t, status.Roles[0].Unmanaged)
}

// TestComputeModelDeploymentStatus_Unmanaged pins the flag that tells a reader why no cache
// condition will ever be True for this role.
func TestComputeModelDeploymentStatus_Unmanaged(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Template.Command = []string{"/bin/my-server"}
	})
	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}

	status, err := r.computeModelDeploymentStatus(context.Background(), md, nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, status.Roles, 1)
	assert.True(t, status.Roles[0].Unmanaged)
}

// TestModelDeploymentStatus_AssignedFlavorIsAbsentUntilItIsAssigned is why the field is a pointer.
//
// "Not assigned yet" and "assigned to a flavor whose name is empty" are different facts, and a
// zero value collapses them into one that READS AS AN ASSIGNMENT. The assertion is therefore nil,
// not "", on the same object across two observations -- which is also what Story 4 needs: an
// operator asking which accelerator model a role actually landed on must be able to tell "not yet"
// from an answer.
func TestModelDeploymentStatus_AssignedFlavorIsAbsentUntilItIsAssigned(t *testing.T) {
	md := twoRoleDeployment()
	prefill, decode := &md.Spec.Roles[0], &md.Spec.Roles[1]

	// BOTH ROLES SIT ON ONE instanceType HERE, so this is one group with one Workload, and the
	// replicas are what attach that Workload to the group. A case with no replicas resolves no
	// Workload for any group and would answer nil to everything below -- passing the absence
	// assertions for a reason that has nothing to do with the guard they exist to pin.
	pods := []core.Pod{roleReplica(md, prefill.Name), roleReplica(md, decode.Name)}
	owning := func(wl *kueue.Workload) []*kueue.Workload {
		for i := range pods {
			wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
				APIVersion: "v1", Kind: "Pod", Name: pods[i].Name, UID: pods[i].UID,
			})
		}

		return []*kueue.Workload{wl}
	}

	before := roleStatusesOver(md, pods, nil)
	require.Len(t, before, 2)
	assert.Nil(t, before[0].AssignedFlavor, "no Workload means no answer, not an empty answer")

	// THE HARDER ABSENCE, and the one the nil above does not reach: a Workload that EXISTS and has
	// no answer for this role. It is what a group looks like between composition and admission, and
	// it is the input that turns a missing guard into ptr.To("") -- a pointer that is set, to
	// nothing, which reads as an assignment to a flavor with no name.
	unadmitted := roleStatusesOver(md, pods, owning(&kueue.Workload{}))
	require.Len(t, unadmitted, 2)
	assert.Nil(t, unadmitted[0].AssignedFlavor,
		"a Workload with no admission has no answer either")

	wl := &kueue.Workload{}
	wl.Status.Admission = &kueue.Admission{
		PodSetAssignments: []kueue.PodSetAssignment{
			{
				Name: kueue.PodSetReference(prefill.Name),
				Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{
					nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA): "h20-8",
				},
			},
			{
				Name: kueue.PodSetReference(decode.Name),
				Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{
					nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA): "a100-8",
				},
			},
		},
	}

	after := roleStatusesOver(md, pods, owning(wl))
	require.Len(t, after, 2)
	require.NotNil(t, after[0].AssignedFlavor)
	require.NotNil(t, after[1].AssignedFlavor)

	// TWO ROLES, TWO FLAVORS. Kueue assigns a ResourceFlavor per PodSet, which is what lets one pool
	// serve two accelerator models -- and reading them per role is what makes that visible.
	assert.Equal(t, "h20-8", *after[0].AssignedFlavor)
	assert.Equal(t, "a100-8", *after[1].AssignedFlavor)

	// One role assigned and the other not is a real intermediate state, and the unassigned one must
	// still be nil rather than a set pointer to nothing.
	partial := &kueue.Workload{}
	partial.Status.Admission = &kueue.Admission{
		PodSetAssignments: wl.Status.Admission.PodSetAssignments[:1],
	}
	half := roleStatusesOver(md, pods, owning(partial))
	require.Len(t, half, 2)
	require.NotNil(t, half[0].AssignedFlavor)
	assert.Nil(t, half[1].AssignedFlavor,
		"a Workload that names no assignment for this role has no answer for it")
}

// roleReplica builds a replica belonging to a named role, which is how a Pod is attributed to a
// group. The role label is the attribution every figure on this status reads.
func roleReplica(md *workercore.ModelDeployment, role string) core.Pod {
	return core.Pod{ObjectMeta: meta.ObjectMeta{
		Name:      md.Name + "-" + role + "-0",
		Namespace: md.Namespace,
		UID:       types.UID("uid-" + md.Name + "-" + role),
		Labels:    map[string]string{modelDeploymentLabelKeyComponent: role},
	}}
}

// TestModelDeploymentStatus_AssignedFlavorIsPerGroup is the regression for reading one Workload for
// a deployment that has one per group.
//
// EACH GROUP GETS ITS OWN ANSWER FROM KUEUE. Roles on two instanceTypes are two pod groups and two
// Workloads, and each Workload names only its own group's PodSets. An implementation taking whichever
// Workload sorts first answers correctly for that one group and reports NOTHING for every other --
// so flavor attribution disappears exactly for the multi-instanceType deployments this change exists
// to enable, and it disappears silently, because nil is also what "not assigned yet" looks like.
//
// THE FIXTURE PUTS THE ANSWER IN THE SECOND WORKLOAD BY NAME ORDER. With both flavors in the first,
// a first-Workload implementation would pass.
func TestModelDeploymentStatus_AssignedFlavorIsPerGroup(t *testing.T) {
	md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[1].InstanceType = "a100-8x"
	})
	prefill, decode := &md.Spec.Roles[0], &md.Spec.Roles[1]

	prefillPod, decodePod := roleReplica(md, prefill.Name), roleReplica(md, decode.Name)

	admitted := func(pod core.Pod, role, flavor string) *kueue.Workload {
		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = "wl-"+role, md.Namespace
		wl.OwnerReferences = []meta.OwnerReference{{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		}}
		wl.Status.Admission = &kueue.Admission{
			PodSetAssignments: []kueue.PodSetAssignment{{
				Name: kueue.PodSetReference(role),
				Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{
					nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA): kueue.ResourceFlavorReference(flavor),
				},
			}},
		}

		return wl
	}

	// "wl-decode" sorts before "wl-prefill", so the first Workload is the decoder's.
	wls := []*kueue.Workload{
		admitted(decodePod, decode.Name, "a100-8"),
		admitted(prefillPod, prefill.Name, "h20-8"),
	}

	statuses := roleStatusesOver(md, []core.Pod{prefillPod, decodePod}, wls)
	require.Len(t, statuses, 2)

	require.NotNil(t, statuses[0].AssignedFlavor,
		"the prefiller's flavor is in the SECOND workload, which a first-workload reader never opens")
	assert.Equal(t, "h20-8", *statuses[0].AssignedFlavor)

	require.NotNil(t, statuses[1].AssignedFlavor)
	assert.Equal(t, "a100-8", *statuses[1].AssignedFlavor,
		"and the decoder reads its own group's answer rather than its sibling's")
}

// TestModelDeploymentStatus_KindIsEchoedAndNeverEmpty pins the field that must never be written
// blank.
//
// status.roles[].kind is REQUIRED and enumerated, so an unset value written through is refused, and
// the API server refuses the whole status write rather than the one field. Resolving it here is what
// made adding that marker safe rather than the change that starts rejecting every status write.
func TestModelDeploymentStatus_KindIsEchoedAndNeverEmpty(t *testing.T) {
	md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		// The second role's kind is deliberately left unset: it is what a deployment written before
		// disaggregation carries, and what any in-process caller builds.
		md.Spec.Roles[1].Kind = ""
	})

	statuses := roleStatusesOver(md, nil, nil)
	require.Len(t, statuses, 2)

	assert.Equal(t, workercore.ModelDeploymentRoleKindPrefill, statuses[0].Kind)
	assert.Equal(t, workercore.ModelDeploymentRoleKindServer, statuses[1].Kind,
		"an unset kind is the schema's server default, not a kind of its own")
	for i := range statuses {
		assert.NotEmpty(t, statuses[i].Kind, "roles[%d].kind is required and must never be blank", i)
	}
}

// TestObserveModelDeploymentQuota walks the single-group shape, where the deployment has one pod
// group and therefore one Workload. The multi-group cases are their own tests below, because the
// condition is an answer about every group and this fixture cannot express more than one.
//
// It reads that Workload's OWN conditions, which is not an implementation detail: the admission gate
// stops evaluating a Workload once it is admitted, so anything derived from the gate would answer
// for the moment of admission and never again — a Workload preempted since would still read as
// reserved.
//
// THE INCOMPLETE ROW IS THE ONE THAT MATTERS. A group short of its declared total has no Workload at
// all, which is byte-for-byte the same observation as admission not having happened yet, and the two
// mean opposite things: one clears in a moment, the other is the deployment sitting with gated Pods
// and an empty `kubectl get workloads` until something creates the missing replica.
func TestObserveModelDeploymentQuota(t *testing.T) {
	testCases := []struct {
		name      string
		namespace string
		// replicas is what the role declares; live is how many Pods actually exist. They differ only
		// in the incomplete case, which is the whole point of carrying them separately.
		replicas    int32
		live        int
		workload    bool
		reserved    bool
		wantStatus  meta.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name: "the group has quota", replicas: 2, live: 2, workload: true, reserved: true,
			wantStatus: meta.ConditionTrue, wantReason: "Reserved",
			wantMessage: `the group of 2 replicas has quota reserved in cluster queue "h20-8x"`,
		},
		{
			name: "the group is waiting", replicas: 2, live: 2, workload: true,
			wantStatus: meta.ConditionFalse, wantReason: "Pending",
			wantMessage: `the group of 2 replicas is waiting for quota in cluster queue "h20-8x"`,
		},
		{
			// Kueue composes the Workload asynchronously from the Pods, so its absence for a
			// COMPLETE group is admission in flight and not a refusal.
			name: "the group is complete but has no workload yet", replicas: 2, live: 2,
			wantStatus: meta.ConditionUnknown, wantReason: "AdmissionInFlight",
		},
		{
			name: "a reserved namespace has no entrance queue", namespace: "gpustack-system",
			replicas: 2, live: 2,
			wantStatus: meta.ConditionFalse, wantReason: "NoQueueInReservedNamespace",
			wantMessage: "the deployment is in reserved namespace \"gpustack-system\" and will never be scheduled",
		},
		{
			// The same absence, for the opposite reason, and the message carries have/want so a
			// reader can tell which one they are looking at without counting Pods themselves.
			name: "the group is short of its total", replicas: 4, live: 3,
			wantStatus: meta.ConditionFalse, wantReason: "PodGroupIncomplete",
			wantMessage: `3 of 4 of the group's replicas exist, so Kueue composes no workload for ` +
				`it at all and there is nothing in cluster queue "h20-8x" to hold quota`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				if tc.namespace != "" {
					md.Namespace = tc.namespace
				}
				md.Spec.Roles[0].Replicas = tc.replicas
			})

			pods := make([]core.Pod, 0, tc.live)
			for i := range tc.live {
				pods = append(pods, *readyReplica(md, int32(i), true))
			}

			var wl *kueue.Workload
			if tc.workload {
				wl = groupWorkload(pods, tc.reserved)
			}

			holder := new(workercore.ModelDeployment)
			observeQuotaOver(md, pods, workloadSlice(wl), holder)

			assert.Equal(t, string(tc.wantStatus),
				ModelDeploymentConditionQuotaReserved.GetStatus(holder))
			assert.Equal(t, tc.wantReason,
				ModelDeploymentConditionQuotaReserved.GetReason(holder))
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage,
					ModelDeploymentConditionQuotaReserved.GetMessage(holder))
			}
		})
	}
}

// TestObserveModelDeploymentQuota_TrueCoversEveryRole is F8's last acceptance: True cannot be true
// for one role and not another.
//
// IT USED TO HOLD BY CONSTRUCTION AND NOW IT IS ENFORCED, which is why the case matters more than it
// did. With one Workload covering both PodSets there was no way for the condition to disagree
// between roles. Roles on several instanceTypes are several Workloads, so what keeps this true is
// the per-group answer rather than the shape of the object it reads.
func TestObserveModelDeploymentQuota_TrueCoversEveryRole(t *testing.T) {
	md := twoRoleDeployment()

	pods := make([]core.Pod, 0, 4)
	for i := range md.Spec.Roles {
		for ordinal := range md.Spec.Roles[i].Replicas {
			pod := readyReplica(md, ordinal, true)
			pod.Name = modelDeploymentPodName(md, &md.Spec.Roles[i], ordinal)
			pod.UID = types.UID(pod.Name)
			pods = append(pods, *pod)
		}
	}
	require.Len(t, pods, 4)

	holder := new(workercore.ModelDeployment)
	wl := groupWorkload(pods, true)
	observeQuotaOver(md, pods, workloadSlice(wl), holder)

	assert.True(t, ModelDeploymentConditionQuotaReserved.IsTrue(holder))
	assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(holder), "group of 4")
}

// TestObserveModelDeploymentQuota_NamesTheClusterQueue states where an operator is sent when quota
// is short. The ClusterQueue is named after the InstanceType, so the queue is read off the spec
// rather than resolved — and a message that named the LocalQueue hash instead would send a reader
// to an object they cannot map back to anything they wrote.
func TestObserveModelDeploymentQuota_NamesTheClusterQueue(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].InstanceType = "a100-4x"
	})
	pods := []core.Pod{*readyReplica(md, 0, true)}

	holder := new(workercore.ModelDeployment)
	pendingWL := groupWorkload(pods, false)
	observeQuotaOver(md, pods, workloadSlice(pendingWL), holder)

	assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(holder), `"a100-4x"`)
}

// TestObserveModelDeploymentQuota_NoReplicas keeps the condition off False before anything exists.
// A deployment that has not rendered a Pod yet has not been refused quota.
func TestObserveModelDeploymentQuota_NoReplicas(t *testing.T) {
	md := newRenderDeployment()

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, nil, nil, holder)

	assert.True(t, ModelDeploymentConditionQuotaReserved.IsUnknown(holder))
	assert.Equal(t, "NoReplicas", ModelDeploymentConditionQuotaReserved.GetReason(holder),
		"a deployment with no Pods yet is not an incomplete group: it has not started")
}

// TestObserveModelDeploymentQuota_AllReplicasTerminating separates the two emptinesses this
// function can be handed. The test above covers an empty LIST; this one covers a non-empty list
// whose every member is filtered out, which is what a recreate rollout or a scale-down produces.
// The counters read zero either way, and the difference was invisible: the True branch reported
// "all 0 replicas have quota reserved" — a guarantee over a set it had just finished emptying.
func TestObserveModelDeploymentQuota_AllReplicasTerminating(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
	})

	pods := make([]core.Pod, 0, 2)
	for i := int32(0); i < 2; i++ {
		pod := readyReplica(md, i, true)
		pod.DeletionTimestamp = ptr.To(meta.Now())
		pods = append(pods, *pod)
	}

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, pods, nil, holder)

	assert.True(t, ModelDeploymentConditionQuotaReserved.IsUnknown(holder))
	assert.Equal(t, "AllReplicasTerminating",
		ModelDeploymentConditionQuotaReserved.GetReason(holder),
		"a group being torn down is not a group short of its total: nothing is waiting to be created")
	assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(holder), "all 2 replicas")
}

// TestSyncModelDeploymentStatus_RebuiltWholesale is F7's last acceptance, and the reason every
// observed field is derived rather than patched: a value that was true once must not survive a
// disagreement with the Pods.
func TestSyncModelDeploymentStatus_RebuiltWholesale(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })
	md.Status = workercore.ModelDeploymentStatus{
		Phase:        ModelDeploymentPhaseReady,
		PhaseMessage: "a message from a pass that is over",
		Endpoint:     "http://stale.elsewhere.svc:1234",
		Roles: []workercore.ModelDeploymentRoleStatus{
			{Name: "server", Desired: 9, Ready: 9},
		},
	}

	pods := []core.Pod{*readyReplica(md, 0, true), *readyReplica(md, 1, false)}
	cli := newModelDeploymentClient(md, newRenderInstanceType())
	r := &ModelDeploymentReconciler{Client: cli}

	require.NoError(t, r.syncModelDeploymentStatus(context.Background(), md, pods, nil, nil))

	stored := getModelDeployment(t, cli)
	assert.Equal(t, ModelDeploymentPhaseDegraded, stored.Status.Phase)
	assert.Equal(t, "1 of 2 replicas are ready", stored.Status.PhaseMessage)
	assert.Equal(t, "http://qwen.team-a.svc:8000", stored.Status.Endpoint)
	require.Len(t, stored.Status.Roles, 1)
	assert.Equal(t, int32(2), stored.Status.Roles[0].Desired, "the stale 9 must not survive")
	assert.Equal(t, int32(1), stored.Status.Roles[0].Ready)
}

// TestSyncModelDeploymentStatus_WritesNothingWhenUnchanged is what keeps a status rebuilt on every
// pass from becoming a write on every pass — including through LastTransitionTime, which the
// condition accessors must leave alone when the condition's value did not move.
func TestSyncModelDeploymentStatus_WritesNothingWhenUnchanged(t *testing.T) {
	md := newRenderDeployment()
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, md, newRenderInstanceType())
	r := &ModelDeploymentReconciler{Client: cli}

	pods := []core.Pod{*readyReplica(md, 0, true), *readyReplica(md, 1, true)}
	require.NoError(t, r.syncModelDeploymentStatus(context.Background(), md, pods, nil, nil))
	require.Equal(t, 1, writes.statusUpdates)

	require.NoError(t, r.syncModelDeploymentStatus(context.Background(), md, pods, nil, nil))
	assert.Equal(t, 1, writes.statusUpdates, "an unchanged status must not be written again")
}

// TestComputeModelDeploymentStatus_DeclaresOnlyWhatItObserved states the difference between a
// condition this pass DECLARED and a field it INVENTED.
//
// A pass that observed nothing still declares the two conditions it evaluates every time — an axis
// with no answer is Unknown, which is an answer — but it must not fabricate the domain projection.
// DomainRegistered is the one condition absent here, and for a reason worth keeping: this pass was
// handed no reading of the Binding at all, and a pass that did not look must not report.
func TestComputeModelDeploymentStatus_DeclaresOnlyWhatItObserved(t *testing.T) {
	md := newRenderDeployment()
	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}

	status, err := r.computeModelDeploymentStatus(context.Background(), md, nil, nil, nil)
	require.NoError(t, err)

	declared := make([]string, 0, len(status.Conditions))
	for _, c := range status.Conditions {
		declared = append(declared, c.Type)
	}
	assert.ElementsMatch(t, []string{
		string(ModelDeploymentConditionQuotaReserved),
		string(ModelDeploymentConditionCacheAttached),
		string(ModelDeploymentConditionReplicasUpToDate),
	}, declared)
	assert.NotContains(t, declared, string(ModelDeploymentConditionDomainRegistered),
		"this pass was handed no reading of the Binding, and a pass that did not look must not report")

	holder := &workercore.ModelDeployment{Status: *status}
	assert.True(t, ModelDeploymentConditionQuotaReserved.IsUnknown(holder))
	assert.True(t, ModelDeploymentConditionCacheAttached.IsUnknown(holder))
	// The rollout axis belongs with those two rather than with the domain: the replicas are this
	// controller's own, so a pass that could account for none of them has no answer rather than no
	// question, and Unknown is that answer. The domain is a reading of somebody else's object.
	assert.True(t, ModelDeploymentConditionReplicasUpToDate.IsUnknown(holder))
	assert.Nil(t, status.KVCache, "and it invents no domain to report")
}

// TestFindModelDeploymentGroupWorkload_MatchesAPlainOwnerReference is the inverse of the test it
// replaces, and the inversion is the fix.
//
// The single-role version required a CONTROLLER reference, which was right for its time: Kueue sets
// one when it composes a Workload from a single Pod. It composes a pod group's Workload with PLAIN
// owner references to every member instead -- SetOwnerReference per Pod, never
// SetControllerReference. So the moment the replicas became a group, a controller-reference filter
// began matching nothing, and every deployment would have reported "no workload yet" forever while
// being admitted normally. Nothing would have errored.
//
// The API version is still checked with the kind: "Pod" is not a reserved word, so a resource of
// that kind in another group matches on the kind alone.
func TestFindModelDeploymentGroupWorkload_MatchesAPlainOwnerReference(t *testing.T) {
	md := newRenderDeployment()
	ours := readyReplica(md, 0, true)
	other := types.UID("uid-someone-else")

	testCases := []struct {
		name  string
		refs  []meta.OwnerReference
		found bool
		why   string
	}{
		{
			name: "the group's plain owner references",
			refs: []meta.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: "sibling", UID: other},
				{APIVersion: "v1", Kind: "Pod", Name: ours.Name, UID: ours.UID},
			},
			found: true,
			why:   "a group Workload carries no controller reference and must still be found",
		},
		{
			name: "a controller reference, which a single-Pod Workload carries",
			refs: []meta.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: ours.Name, UID: ours.UID, Controller: boolPtr(true)},
			},
			found: true,
			why:   "the single-Pod shape is still one of ours and must not stop being found",
		},
		{
			name: "another deployment's Pods only",
			refs: []meta.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: "theirs", UID: other},
			},
			why: "a Workload owning nobody we rendered is not ours to report on",
		},
		{
			name: "a Pod kind from another group is not a Pod",
			refs: []meta.OwnerReference{
				{APIVersion: "acme.io/v1", Kind: "Pod", Name: ours.Name, UID: ours.UID},
			},
			why: "the kind alone does not identify the resource",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			wl := &kueue.Workload{}
			wl.Name, wl.Namespace = "pod-qwen-server-0-abcde", "team-a"
			wl.OwnerReferences = tc.refs

			r := &ModelDeploymentReconciler{
				Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(wl).Build(),
			}
			got, err := r.findModelDeploymentGroupWorkloads(
				context.Background(), md, []core.Pod{*ours})
			require.NoError(t, err)

			if !tc.found {
				assert.Empty(t, got, tc.why)

				return
			}
			require.Len(t, got, 1, tc.why)
			assert.Equal(t, wl.Name, got[0].Name)
		})
	}
}

// TestObserveModelDeploymentQuota_CountsPerGroup is the case the deployment-wide sum fails.
//
// Kueue composes one Workload per group and withholds it until THAT group has its own declared
// total, so one group can be complete while a sibling is short. Summing the deployment reads the
// complete group as short whenever its sibling is, and names a queue that is holding nothing back --
// pointing the operator at the pool that is fine.
//
// THE NUMBERS AND THE QUEUE ARE BOTH ASSERTED. A message carrying the right shape with the
// deployment-wide numbers still reads as a correct answer, and it is the numbers an operator acts
// on.
func TestObserveModelDeploymentQuota_CountsPerGroup(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		decode := md.Spec.Roles[0]
		md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 2
		decode.Name, decode.Replicas, decode.InstanceType = "decode", 3, "a100-8x"
		md.Spec.Roles = append(md.Spec.Roles, decode)
	})

	replica := func(role string, i int) core.Pod {
		return core.Pod{ObjectMeta: meta.ObjectMeta{
			Name:      fmt.Sprintf("qwen-%s-%d", role, i),
			Namespace: md.Namespace,
			Labels:    map[string]string{modelDeploymentLabelKeyComponent: role},
		}}
	}

	// prefill is complete at its own 2; decode is one short of its own 3.
	pods := []core.Pod{
		replica("prefill", 0), replica("prefill", 1),
		replica("decode", 0), replica("decode", 1),
	}

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, pods, nil, holder)

	assert.Equal(t, string(meta.ConditionFalse),
		ModelDeploymentConditionQuotaReserved.GetStatus(holder))
	assert.Equal(t, "PodGroupIncomplete",
		ModelDeploymentConditionQuotaReserved.GetReason(holder))
	assert.Equal(t,
		`2 of 3 of the group's replicas exist, so Kueue composes no workload for it at all and `+
			`there is nothing in cluster queue "a100-8x" to hold quota`,
		ModelDeploymentConditionQuotaReserved.GetMessage(holder),
		"the short group's own numbers and its own queue, not the deployment's 4 of 5 on the other pool")
}

// TestObserveModelDeploymentQuota_AdmissionInFlightNamesTheRightGroups covers a message that named
// one queue for a deployment that has several.
//
// A DEPLOYMENT SPANNING TWO instanceTypes HAS NO SINGLE QUEUE TO NAME. The wording here was read off
// roles[0], which is a statement about one group offered as a statement about the deployment: an
// operator told to look in the prefiller's queue finds a workload there and nothing wrong, while the
// group actually missing one is on the other pool and goes unmentioned.
func TestObserveModelDeploymentQuota_AdmissionInFlightNamesTheRightGroups(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		decode := md.Spec.Roles[0]
		md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
		decode.Name, decode.Replicas, decode.InstanceType = "decode", 1, "a100-8x"
		md.Spec.Roles = append(md.Spec.Roles, decode)
	})

	prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")

	// Both groups are complete; only the prefiller's has a Workload so far.
	only := groupWorkload([]core.Pod{prefill}, false)
	only.Name = "wl-prefill"

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, []core.Pod{prefill, decode}, []*kueue.Workload{only}, holder)

	assert.True(t, ModelDeploymentConditionQuotaReserved.IsUnknown(holder))
	assert.Equal(t, "AdmissionInFlight", ModelDeploymentConditionQuotaReserved.GetReason(holder))

	msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
	assert.Contains(t, msg, "a100-8x",
		"the group with no workload yet is the decoder's, and it is the one to name")
	assert.NotContains(t, msg, "h20-8x",
		"naming the first role's queue sends the operator to the pool where nothing is wrong")
}

// TestObserveModelDeploymentQuota_ParkedIsNotWaiting covers the word the vocabulary did not have.
//
// A PARKED DEPLOYMENT'S GROUPS ARE COMPLETE AND ITS WORKLOADS DEACTIVATED, which every other answer
// here reads as "waiting for admission" -- the opposite of the truth once the bound has fired, and
// the reading that sends an operator to wait for something that is never coming.
//
// IT IS OBSERVED RATHER THAN WRITTEN. The bound is measured on the Workload by another controller;
// this reads the flag that measurement left there, because status is rebuilt from observed state by
// one function and a second writer would leave its own field behind.
func TestObserveModelDeploymentQuota_ParkedIsNotWaiting(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
	})
	pods := []core.Pod{*readyReplica(md, 0, true)}

	active := groupWorkload(pods, true)

	// THE BARRIER'S OWN VERDICT IS PART OF THE FIXTURE, not decoration. spec.active=false says a
	// Workload is deactivated and says nothing about who did it, so what makes this deployment
	// "parked" is that this controller's check is carrying the park verdict on the same object.
	parked := groupWorkload(pods, true)
	parked.Name = "wl-parked"
	parked.Spec.Active = ptr.To(false)
	parked.Status.AdmissionChecks = []kueue.AdmissionCheckState{{
		Name:  kueue.AdmissionCheckReference(_JointAdmissionCheckName),
		State: kueue.CheckStatePending,
		Message: "the groups on instance types a100-8x are waiting. This has not changed for 30m0s, " +
			"so the deployment is " + _JointAdmissionParkedMarker + ": its workloads are deactivated",
	}}

	t.Run("a_deactivated_workload_reports_parked", func(t *testing.T) {
		holder := new(workercore.ModelDeployment)
		observeQuotaOver(md, pods, []*kueue.Workload{active, parked}, holder)

		assert.Equal(t, "Parked", ModelDeploymentConditionQuotaReserved.GetReason(holder))
		msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
		assert.Contains(t, msg, "wl-parked", "the message names what was deactivated")
		assert.Contains(t, msg, "re-apply",
			"and the action that clears it, since an identical re-apply does not")
	})

	t.Run("an_active_set_is_unaffected", func(t *testing.T) {
		// THE NEGATIVE SIDE IS REQUIRED. A rule reporting Parked whenever it found any Workload would
		// pass the case above and turn every healthy deployment into a parked one.
		holder := new(workercore.ModelDeployment)
		observeQuotaOver(md, pods, []*kueue.Workload{active}, holder)

		assert.NotEqual(t, "Parked", ModelDeploymentConditionQuotaReserved.GetReason(holder))
	})

	// SOMEBODY ELSE CAN WRITE THAT FLAG. Kueue deactivates a Workload of its own accord -- a backoff
	// limit reached, for one -- and an operator can pause a group by hand. Reading spec.active alone
	// reports either as "this deployment's set could not be placed", and then instructs the reader to
	// free capacity or delete and recreate: an answer to a question nobody asked, about a state
	// somebody chose. The deactivation here carries no verdict from this barrier, and that is the
	// whole difference between the two cases.
	t.Run("deactivated_by_someone_else_is_not_parked", func(t *testing.T) {
		other := groupWorkload(pods, true)
		other.Name = "wl-paused-by-hand"
		other.Spec.Active = ptr.To(false)

		holder := new(workercore.ModelDeployment)
		observeQuotaOver(md, pods, []*kueue.Workload{other}, holder)

		assert.NotEqual(t, "Parked", ModelDeploymentConditionQuotaReserved.GetReason(holder),
			"an inactive workload this barrier did not park is not this barrier's verdict to report")
	})
}

// workloadSlice carries one Workload into the plural parameter, which is what a single-group
// deployment has in a cluster.
//
// PASSING nil THERE IS NOT THE SAME THING and was a bug while it lasted: the plural parameter now
// answers whether every group holds quota, so nil says "no group has a Workload" and turns a
// reserved deployment into a waiting one. A nil passed only to make a call compile is a value
// nobody chose.
func workloadSlice(wl *kueue.Workload) []*kueue.Workload {
	if wl == nil {
		return nil
	}

	return []*kueue.Workload{wl}
}

// observeQuotaOver resolves each group's Workload exactly as the production caller does, then
// reports.
//
// A CASE MUST NOT HAND-BUILD THAT MAPPING. Which Workload answers for which group is part of what
// these cases exercise, so a case supplying its own map would assert against a grouping it wrote
// itself. An earlier version of this table passed nil for a later-added argument to keep the call
// compiling, and every case then answered "no group has a workload" whatever its fixture said.
func observeQuotaOver(
	md *workercore.ModelDeployment, pods []core.Pod, wls []*kueue.Workload,
	holder *workercore.ModelDeployment,
) {
	groupOfRole := modelDeploymentGroupOfRole(md)
	observeModelDeploymentQuota(md, pods, wls,
		modelDeploymentWorkloadByGroup(md, pods, wls, groupOfRole), groupOfRole, holder)
}

// roleStatusesOver is the same resolution for the per-role view, for the same reason.
func roleStatusesOver(
	md *workercore.ModelDeployment, pods []core.Pod, wls []*kueue.Workload,
) []workercore.ModelDeploymentRoleStatus {
	groupOfRole := modelDeploymentGroupOfRole(md)

	return modelDeploymentRoleStatuses(md, pods,
		modelDeploymentWorkloadByGroup(md, pods, wls, groupOfRole), groupOfRole)
}

// TestObserveModelDeploymentQuota_OneGroupReservedIsNotTheDeployment is the regression the cluster
// found.
//
// MEASURED ON A CLUSTER: a two-instanceType deployment whose second group's queue was held reported
// QuotaReserved=True with the reason Reserved, and a message naming the deployment's whole replica
// count -- wrong in both halves at once. The cause was reading ONE Workload, whichever sorted first,
// for a deployment that has one per group.
//
// THE UNIT TEST EXISTS BECAUSE THE CLUSTER CASE IS NOT A SUBSTITUTE FOR IT. The cluster case runs
// when someone runs it; this runs on every change, and the shape it pins -- several groups, mixed
// reservations -- is one a single-group fixture cannot express at all.
func TestObserveModelDeploymentQuota_OneGroupReservedIsNotTheDeployment(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		decode := md.Spec.Roles[0]
		md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
		decode.Name, decode.Replicas, decode.InstanceType = "decode", 1, "a100-8x"
		md.Spec.Roles = append(md.Spec.Roles, decode)
	})

	replica := func(role string) core.Pod {
		return core.Pod{ObjectMeta: meta.ObjectMeta{
			Name:      "qwen-" + role + "-0",
			Namespace: md.Namespace,
			UID:       types.UID("uid-" + role),
			Labels:    map[string]string{modelDeploymentLabelKeyComponent: role},
		}}
	}
	prefill, decode := replica("prefill"), replica("decode")
	pods := []core.Pod{prefill, decode}

	// One Workload per group: prefill's reserved, decode's not.
	reserved := groupWorkload([]core.Pod{prefill}, true)
	reserved.Name = "wl-prefill"
	held := groupWorkload([]core.Pod{decode}, false)
	held.Name = "wl-decode"

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, pods, []*kueue.Workload{held, reserved}, holder)

	assert.Equal(t, string(meta.ConditionFalse),
		ModelDeploymentConditionQuotaReserved.GetStatus(holder),
		"half a deployment holding quota is not the deployment holding quota")
	assert.Equal(t, "Pending", ModelDeploymentConditionQuotaReserved.GetReason(holder))

	msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
	assert.Contains(t, msg, "a100-8x", "the message names the group that is waiting")
	assert.NotContains(t, msg, "prefill",
		"and does not name the one that is not, which would read as the cause")
}

// TestObserveModelDeploymentQuota_EveryGroupReservedIsReserved is the other side. Without it the
// case above passes against a rule that never reports Reserved for a multi-group deployment at all.
func TestObserveModelDeploymentQuota_EveryGroupReservedIsReserved(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		decode := md.Spec.Roles[0]
		md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
		decode.Name, decode.Replicas, decode.InstanceType = "decode", 1, "a100-8x"
		md.Spec.Roles = append(md.Spec.Roles, decode)
	})

	replica := func(role string) core.Pod {
		return core.Pod{ObjectMeta: meta.ObjectMeta{
			Name:      "qwen-" + role + "-0",
			Namespace: md.Namespace,
			UID:       types.UID("uid-" + role),
			Labels:    map[string]string{modelDeploymentLabelKeyComponent: role},
		}}
	}
	prefill, decode := replica("prefill"), replica("decode")

	a := groupWorkload([]core.Pod{prefill}, true)
	a.Name = "wl-prefill"
	b := groupWorkload([]core.Pod{decode}, true)
	b.Name = "wl-decode"

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, []core.Pod{prefill, decode}, []*kueue.Workload{a, b}, holder)

	assert.Equal(t, string(meta.ConditionTrue),
		ModelDeploymentConditionQuotaReserved.GetStatus(holder))
	assert.Equal(t, "Reserved", ModelDeploymentConditionQuotaReserved.GetReason(holder))
}

// preemptedWorkload marks a Workload as reclaimed by a higher-priority one, the way Kueue does.
//
// THE CONDITION SET IS KUEUE'S VOCABULARY AND NOT A STAND-IN. Kueue writes Preempted when it decides
// and Evicted with the Preempted reason when it acts, and `which` selects between them so a case can
// place itself at either moment. A fixture that invented a condition would agree with a reader that
// invented the same one.
func preemptedWorkload(wl *kueue.Workload, which string) *kueue.Workload {
	switch which {
	case "preempted":
		wl.Status.Conditions = append(wl.Status.Conditions, meta.Condition{
			Type: kueue.WorkloadPreempted, Status: meta.ConditionTrue,
			Reason: kueue.InClusterQueueReason, LastTransitionTime: meta.Now(),
		})
	case "evicted":
		wl.Status.Conditions = append(wl.Status.Conditions, meta.Condition{
			Type: kueue.WorkloadEvicted, Status: meta.ConditionTrue,
			Reason: kueue.WorkloadEvictedByPreemption, LastTransitionTime: meta.Now(),
		})
	}

	return wl
}

// admittedWorkload marks a Workload as admitted, which is what makes a surviving group a survivor
// rather than one more group that is waiting.
func admittedWorkload(wl *kueue.Workload) *kueue.Workload {
	wl.Status.Conditions = append(wl.Status.Conditions, meta.Condition{
		Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue,
		Reason: "Admitted", LastTransitionTime: meta.Now(),
	})

	return wl
}

// TestObserveModelDeploymentQuota_PreemptedInPart covers the second of the two failures #199 names.
//
// A HIGHER-PRIORITY WORKLOAD RECLAIMS ONE ROLE AND THE SURVIVING HALF KEEPS ITS CARDS WHILE SERVING
// NOTHING. Nothing in this path used to notice: the phase is derived from ready replica counts, so
// this reads as Degraded, the same word as "still starting" and "a replica crashed" -- three states
// with three different operator actions behind one reading.
//
// THE SURVIVOR IS THE WHOLE PREDICATE, which is why the table carries the all-preempted case. A
// deployment every group of which was preempted holds nothing and serves nothing, exactly like one
// that never started; reading only "was anything preempted" answers the same for both.
func TestObserveModelDeploymentQuota_PreemptedInPart(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		decode := md.Spec.Roles[0]
		md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
		decode.Name, decode.Replicas, decode.InstanceType = "decode", 1, "a100-8x"
		md.Spec.Roles = append(md.Spec.Roles, decode)
	})

	prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")
	pods := []core.Pod{prefill, decode}

	build := func(prefillState, decodeState string) []*kueue.Workload {
		mk := func(pod core.Pod, name, state string) *kueue.Workload {
			wl := groupWorkload([]core.Pod{pod}, state != "preempted" && state != "evicted")
			wl.Name = name
			switch state {
			case "admitted":
				return admittedWorkload(wl)
			case "preempted", "evicted":
				return preemptedWorkload(wl, state)
			}

			return wl
		}

		return []*kueue.Workload{mk(prefill, "wl-prefill", prefillState), mk(decode, "wl-decode", decodeState)}
	}

	cases := []struct {
		name              string
		prefill, decode   string
		wantReason        string
		wantIn, wantNotIn []string
	}{
		{
			// The state the issue names: the decoder's quota was taken, the prefiller is still
			// admitted and holding its cards, and the deployment serves nothing.
			name:    "one_group_preempted_while_a_sibling_is_admitted",
			prefill: "admitted", decode: "preempted",
			wantReason: "PreemptedInPart",
			wantIn:     []string{"a100-8x", "h20-8x"},
		},
		{
			// Kueue writes the two conditions at different moments; a reader of one alone answers
			// differently depending on which write it arrives between.
			name:    "the_evicted_form_of_the_same_state",
			prefill: "admitted", decode: "evicted",
			wantReason: "PreemptedInPart",
			wantIn:     []string{"a100-8x"},
		},
		{
			// THE CASE THE PREDICATE EXISTS TO NOT MATCH. Everything was preempted, so nothing is
			// held and nothing is served: an ordinary wait for capacity, and reporting it as a
			// fragmented deployment would send an operator looking for cards nobody is holding.
			//
			// IT GUARDS AN ASSUMPTION RATHER THAN DESCRIBING A COMMON STATE, and that is why it must
			// not be deleted as unreachable. Every queue this operator renders sets
			// ReclaimWithinCohort and BorrowWithinCohort to Never, so a preemptor can only take from
			// its own ClusterQueue and a deployment's other groups are out of its reach: reaching
			// this state today needs two preemptors acting in two queues at once. The assumption is
			// those two Never policies. Change either one and this stops being defensive and becomes
			// a path the cluster takes, and the case is already here when that happens.
			name:    "every_group_preempted_is_not_fragmented",
			prefill: "preempted", decode: "preempted",
			wantReason: "Pending",
			wantNotIn:  []string{"higher-priority"},
		},
		{
			// And the ordinary healthy shape still reports Reserved, or the rows above would pass
			// against a rule that reported PreemptedInPart for everything.
			name:    "neither_preempted_is_reserved",
			prefill: "admitted", decode: "admitted",
			wantReason: "Reserved",
			wantNotIn:  []string{"higher-priority"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			holder := new(workercore.ModelDeployment)
			observeQuotaOver(md, pods, build(tc.prefill, tc.decode), holder)

			assert.Equal(t, tc.wantReason, ModelDeploymentConditionQuotaReserved.GetReason(holder))

			msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
			for _, want := range tc.wantIn {
				assert.Contains(t, msg, want, "the message names it: %s", msg)
			}
			for _, notWant := range tc.wantNotIn {
				assert.NotContains(t, msg, notWant, "and does not claim this: %s", msg)
			}
		})
	}
}

// TestObserveModelDeploymentQuota_PreemptedInPartDoesNotClaimNothingIsAdmitted pins the sentence that
// was false in this state.
//
// The multi-group wait ends with "no role is admitted until the whole set can run". Under a partial
// preemption a role IS admitted, and it is the reason the state matters at all: that role is holding
// accelerators. A status that denies it sends the operator past the only thing worth acting on.
func TestObserveModelDeploymentQuota_PreemptedInPartDoesNotClaimNothingIsAdmitted(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		decode := md.Spec.Roles[0]
		md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
		decode.Name, decode.Replicas, decode.InstanceType = "decode", 1, "a100-8x"
		md.Spec.Roles = append(md.Spec.Roles, decode)
	})

	prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")

	held := admittedWorkload(groupWorkload([]core.Pod{prefill}, true))
	held.Name = "wl-prefill"
	taken := preemptedWorkload(groupWorkload([]core.Pod{decode}, false), "preempted")
	taken.Name = "wl-decode"

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, []core.Pod{prefill, decode}, []*kueue.Workload{held, taken}, holder)

	msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
	assert.NotContains(t, msg, "no role is admitted",
		"a role is admitted, and it is the one holding the accelerators: %s", msg)
	assert.Contains(t, msg, "still admitted",
		"the message has to say which groups are holding, since that is what an operator acts on")
}
