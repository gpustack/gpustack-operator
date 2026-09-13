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
		string(ModelDeploymentConditionRoleKindsReady),
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
	// ROLE KINDS IS THE ONE THAT ANSWERS FALSE HERE, and the difference is what was observed rather
	// than how much of it. The counts this pass wrote are counted from a Pod list that SUCCEEDED and
	// returned nothing, so "no replica of any kind is ready" is an observation rather than an
	// absence of one. Unknown is reserved for a pass that accounted for no role at all, which is a
	// different input and reaches a different reason.
	assert.True(t, ModelDeploymentConditionRoleKindsReady.IsFalse(holder))
	assert.Equal(t, modelDeploymentReasonKindsNotReady,
		ModelDeploymentConditionRoleKindsReady.GetReason(holder))
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
			// THE REASON IS THE DISCRIMINATOR, and the message names the preemption here too. An
			// operator whose whole deployment was reclaimed needs that fact as much as one whose half
			// was. What they must not be told is that something of theirs is still holding
			// accelerators, which is what the other reason says and this state does not have.
			wantIn:    []string{"higher-priority"},
			wantNotIn: []string{"still admitted"},
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

// TestObserveModelDeploymentQuota_PreemptionIsCarriedIntoTheOtherAnswers covers the asymmetry a
// reviewer asked about: a group with no Workload counts as neither preempted nor surviving.
//
// WITH NO SURVIVOR THERE IS NOTHING BEING HELD, so this is not the state PreemptedInPart names and
// widening that reason to cover it would make it describe a harm this state does not have. What was
// wrong is that the branch which does answer said nothing about the preemption at all: it reported a
// group short of its replicas and sent the reader to look for a scheduling problem, while something
// else had taken the other group's quota.
//
// A GROUP KUEUE HAS COMPOSED NO WORKLOAD FOR CANNOT BE EITHER, and that is why the asymmetry is real
// rather than an oversight: it holds nothing, so it is no survivor, and there is nothing on it to
// read, so it cannot be shown to have been preempted.
func TestObserveModelDeploymentQuota_PreemptionIsCarriedIntoTheOtherAnswers(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		decode := md.Spec.Roles[0]
		md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
		decode.Name, decode.Replicas, decode.InstanceType = "decode", 2, "a100-8x"
		md.Spec.Roles = append(md.Spec.Roles, decode)
	})

	prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")

	// The prefiller was preempted. The decoder declares two replicas and has one, so Kueue composes
	// no Workload for it and it is neither a survivor nor visibly preempted.
	taken := preemptedWorkload(groupWorkload([]core.Pod{prefill}, false), "preempted")
	taken.Name = "wl-prefill"

	holder := new(workercore.ModelDeployment)
	observeQuotaOver(md, []core.Pod{prefill, decode}, []*kueue.Workload{taken}, holder)

	assert.Equal(t, modelDeploymentReasonPodGroupIncomplete,
		ModelDeploymentConditionQuotaReserved.GetReason(holder),
		"with nothing admitted there is no survivor holding accelerators, so this is not the state "+
			"PreemptedInPart names")

	msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
	assert.Contains(t, msg, "a100-8x", "the branch still says what else is wrong")
	assert.Contains(t, msg, "higher-priority",
		"and it carries the preemption, which it used to drop entirely")
	assert.Contains(t, msg, "h20-8x",
		"naming the group whose quota was taken, which is not the group this branch is about")
}

// TestObserveModelDeploymentQuota_ThePreemptionNoteContract pins the CONTRACT the comment states,
// not the one branch that was missing it.
//
// THE DEFECT THIS GUARDS IS A COMMENT THAT CLAIMED MORE THAN THE CODE DID. The note was appended to
// five answers while the comment beside it read as though every answer below carried it, and four
// early returns did not. Testing only the branch that was added back would verify the fix and leave
// the claim itself unguarded: the next time that comment widens, nothing would notice again.
//
// SO BOTH SIDES OF THE CLAIM ARE ASSERTED. The answers the contract names carry the note, and the
// ones it excludes are checked against the reason they are excluded for rather than merely left out.
//
// WHAT THIS DOES NOT REACH: the exact wording of each answer. A branch that carried the note and
// dropped its own message would pass here, which the per-answer cases above are what cover.
func TestObserveModelDeploymentQuota_ThePreemptionNoteContract(t *testing.T) {
	twoGroups := func() *workercore.ModelDeployment {
		return newRenderDeployment(func(md *workercore.ModelDeployment) {
			decode := md.Spec.Roles[0]
			md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
			decode.Name, decode.Replicas, decode.InstanceType = "decode", 1, "a100-8x"
			md.Spec.Roles = append(md.Spec.Roles, decode)
		})
	}

	// The prefiller is the group that was reclaimed in every case below.
	reclaimed := func(md *workercore.ModelDeployment, pod core.Pod) *kueue.Workload {
		wl := preemptedWorkload(groupWorkload([]core.Pod{pod}, false), "preempted")
		wl.Name = "wl-prefill"

		return wl
	}

	t.Run("every_answer_the_contract_names_carries_the_note", func(t *testing.T) {
		cases := []struct {
			name       string
			build      func() (*workercore.ModelDeployment, []core.Pod, []*kueue.Workload)
			wantReason string
		}{
			{
				// The branch that was missing it, and the one that looks unreachable and is not: a
				// group's replicas resolve to a Workload whether or not they are terminating.
				name:       "AllReplicasTerminating",
				wantReason: "AllReplicasTerminating",
				build: func() (*workercore.ModelDeployment, []core.Pod, []*kueue.Workload) {
					md := twoGroups()
					prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")
					now := meta.Now()
					prefill.DeletionTimestamp, decode.DeletionTimestamp = &now, &now

					return md, []core.Pod{prefill, decode}, []*kueue.Workload{reclaimed(md, prefill)}
				},
			},
			{
				name:       "PodGroupIncomplete",
				wantReason: modelDeploymentReasonPodGroupIncomplete,
				build: func() (*workercore.ModelDeployment, []core.Pod, []*kueue.Workload) {
					md := twoGroups()
					md.Spec.Roles[1].Replicas = 2
					prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")

					return md, []core.Pod{prefill, decode}, []*kueue.Workload{reclaimed(md, prefill)}
				},
			},
			{
				name:       "AdmissionInFlight",
				wantReason: "AdmissionInFlight",
				build: func() (*workercore.ModelDeployment, []core.Pod, []*kueue.Workload) {
					md := twoGroups()
					prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")
					_ = decode

					// Both groups are complete; the decoder's has no Workload yet.
					return md, []core.Pod{prefill, decode}, []*kueue.Workload{reclaimed(md, prefill)}
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				md, pods, wls := tc.build()

				holder := new(workercore.ModelDeployment)
				observeQuotaOver(md, pods, wls, holder)

				assert.Equal(t, tc.wantReason, ModelDeploymentConditionQuotaReserved.GetReason(holder))
				assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(holder), "higher-priority",
					"this answer is one the comment says carries the preemption, and it must: %s",
					ModelDeploymentConditionQuotaReserved.GetMessage(holder))
			})
		}
	})

	// THE EXCLUSIONS ARE CHECKED AGAINST THEIR REASON, not merely observed to be absent. An exclusion
	// nobody can tell from an oversight is an invitation to "fix" it later.
	t.Run("NoReplicas_cannot_be_reached_with_a_preemption", func(t *testing.T) {
		md := twoGroups()

		// The claim is structural: a group's Workload is resolved through that group's replicas, so
		// with no Pods there is no Workload on which a preemption could have been seen. Asserting it
		// on the predicate rather than on the answer is what makes it a statement about the reason.
		lost, kept := modelDeploymentPreemptedInPart(md,
			modelDeploymentWorkloadByGroup(md, nil, []*kueue.Workload{
				preemptedWorkload(groupWorkload([]core.Pod{roleReplica(md, "prefill")}, false), "preempted"),
			}, modelDeploymentGroupOfRole(md)))

		assert.Empty(t, lost, "with no replicas no Workload is resolved, so nothing can be seen preempted")
		assert.Empty(t, kept)
	})

	t.Run("Parked_supersedes_it_and_claims_nothing_is_held", func(t *testing.T) {
		md := twoGroups()
		prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")

		parked := groupWorkload([]core.Pod{decode}, true)
		parked.Name = "wl-decode"
		parked.Spec.Active = ptr.To(false)
		parked.Status.AdmissionChecks = []kueue.AdmissionCheckState{{
			Name:    kueue.AdmissionCheckReference(_JointAdmissionCheckName),
			State:   kueue.CheckStatePending,
			Message: "the deployment is " + _JointAdmissionParkedMarker + ": its workloads are deactivated",
		}}

		holder := new(workercore.ModelDeployment)
		observeQuotaOver(md, []core.Pod{prefill, decode},
			[]*kueue.Workload{reclaimed(md, prefill), parked}, holder)

		assert.Equal(t, "Parked", ModelDeploymentConditionQuotaReserved.GetReason(holder),
			"a parked deployment is answered before the preemption is even computed")

		msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
		assert.NotContains(t, msg, "still admitted",
			"nothing of this deployment's is holding anything: its workloads are deactivated")
	})
}

// roleKindStatus builds one role status for the role-kind cases.
//
// THE KIND IS ALWAYS EXPLICIT, and that is the point of having this helper rather than reusing the
// deployment fixtures. A role's NAME does not carry its kind: the kind is a separate field that
// defaults to server, so a fixture built by naming two roles "prefill" and "decode" and setting
// nothing else is a SINGLE-KIND fixture that reads as a disaggregated one. Cases below assert
// against a kind, so a fixture that quietly carried one kind would answer every case the same way
// and pass.
func roleKindStatus(name string, kind workercore.ModelDeploymentRoleKind, ready int32) workercore.ModelDeploymentRoleStatus {
	return workercore.ModelDeploymentRoleStatus{Name: name, Kind: kind, Desired: 1, Ready: ready}
}

func TestModelDeploymentKindsWithoutReady(t *testing.T) {
	testCases := []struct {
		name  string
		roles []workercore.ModelDeploymentRoleStatus
		want  []string
	}{
		{
			// The discriminating fixture. One kind, one role with nothing ready: the KIND is still
			// represented, so nothing is missing. A rule written per ROLE reports the opposite here,
			// and this is the only arrangement in this table where the two disagree.
			name: "single_kind_one_role_without_ready_replicas",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("a", workercore.ModelDeploymentRoleKindServer, 0),
				roleKindStatus("b", workercore.ModelDeploymentRoleKindServer, 1),
			},
			want: nil,
		},
		{
			name: "one_kind_has_no_ready_replica",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 0),
				roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
			},
			want: []string{string(workercore.ModelDeploymentRoleKindPrefill)},
		},
		{
			name: "every_kind_has_a_ready_replica",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 1),
				roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 2),
			},
			want: nil,
		},
		{
			// Both missing, and the ORDER is the roles' order rather than a map's. The message this
			// list renders into is compared against the stored one to decide whether to write at
			// all, so an unstable order would rewrite the status on every pass.
			name: "no_kind_has_a_ready_replica_reports_both_in_role_order",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 0),
				roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 0),
			},
			want: []string{
				string(workercore.ModelDeploymentRoleKindDecode),
				string(workercore.ModelDeploymentRoleKindPrefill),
			},
		},
		{
			// A kind is represented if ANY of its roles has a ready replica, including when the
			// role that has one is not the first of that kind.
			name: "a_kind_is_represented_by_its_second_role",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("p1", workercore.ModelDeploymentRoleKindPrefill, 0),
				roleKindStatus("p2", workercore.ModelDeploymentRoleKindPrefill, 1),
				roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
			},
			want: nil,
		},
		{
			// An unmanaged role counts. Its Pods carry no probes, so Ready means only that the
			// containers started -- a weaker statement, deliberately accepted, because the operator
			// did not build that command line and has nothing better to judge it by.
			name: "an_unmanaged_role_represents_its_kind",
			roles: []workercore.ModelDeploymentRoleStatus{
				func() workercore.ModelDeploymentRoleStatus {
					rs := roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 1)
					rs.Unmanaged = true

					return rs
				}(),
				roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
			},
			want: nil,
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, modelDeploymentKindsWithoutReady(c.roles))
		})
	}
}

// TestModelDeploymentKindsWithoutReady_OnlyASingleKindFixtureCanFalsifyThePerRoleRule pins the one
// fact that decides whether the table above is worth running.
//
// The rule under test is "every KIND has a ready replica". The rule it must not be is "every ROLE
// has a ready replica". Those two agree on every arrangement in which each kind has exactly one
// role -- which is every fixture a reader would naturally write for a feature about prefill and
// decode. They disagree on ONE shape: several roles of the SAME kind, some of them with nothing
// ready.
//
// SO THE SINGLE-KIND FIXTURE IS LOAD-BEARING, and it looks like the least relevant one in the file.
// This test states that in a form that fails if it stops being true: if a later edit makes the
// fixtures agree under both rules, the discrimination is gone and this reports it, rather than the
// suite going green against an implementation nobody checked.
func TestModelDeploymentKindsWithoutReady_OnlyASingleKindFixtureCanFalsifyThePerRoleRule(t *testing.T) {
	// perRole is the rule this implementation must NOT be, written out so the difference is executed
	// rather than asserted in a comment.
	perRole := func(roles []workercore.ModelDeploymentRoleStatus) bool {
		for i := range roles {
			if roles[i].Ready == 0 {
				return false
			}
		}

		return true
	}

	singleKind := []workercore.ModelDeploymentRoleStatus{
		roleKindStatus("a", workercore.ModelDeploymentRoleKindServer, 0),
		roleKindStatus("b", workercore.ModelDeploymentRoleKindServer, 1),
	}
	multiKindOneMissing := []workercore.ModelDeploymentRoleStatus{
		roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 0),
		roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
	}
	multiKindBothReady := []workercore.ModelDeploymentRoleStatus{
		roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 1),
		roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
	}

	byKind := func(roles []workercore.ModelDeploymentRoleStatus) bool {
		return len(modelDeploymentKindsWithoutReady(roles)) == 0
	}

	assert.NotEqual(t, byKind(singleKind), perRole(singleKind),
		"the single-kind fixture is the only one that tells the two rules apart; if this stops "+
			"disagreeing, nothing in this file can catch a per-role implementation")

	assert.Equal(t, byKind(multiKindOneMissing), perRole(multiKindOneMissing),
		"a multi-kind fixture with one kind missing agrees under both rules, so it cannot falsify")
	assert.Equal(t, byKind(multiKindBothReady), perRole(multiKindBothReady),
		"a multi-kind fixture with everything ready agrees under both rules, so it cannot falsify")
}

// TestObserveModelDeploymentRoleKinds_MultiKindFixturesReallyCarryTwoKinds guards the fixtures
// themselves rather than the code.
//
// A fixture is only a multi-kind fixture if its kinds actually differ, and the field that decides
// that defaults to server when unset. A later refactor routing these through a deployment helper
// would produce roles named "prefill" and "decode" that are both server roles, and every case above
// would still pass -- for the wrong reason and with no symptom. This asserts the property the cases
// rely on instead of trusting how they are spelled.
func TestObserveModelDeploymentRoleKinds_MultiKindFixturesReallyCarryTwoKinds(t *testing.T) {
	roles := []workercore.ModelDeploymentRoleStatus{
		roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 0),
		roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
	}

	kinds := map[workercore.ModelDeploymentRoleKind]struct{}{}
	for i := range roles {
		kinds[roles[i].Kind] = struct{}{}
	}
	require.Len(t, kinds, 2, "a fixture meant to be multi-kind must carry two distinct kinds")
}

func TestObserveModelDeploymentRoleKinds(t *testing.T) {
	testCases := []struct {
		name        string
		roles       []workercore.ModelDeploymentRoleStatus
		wantStatus  string
		wantReason  string
		wantMessage string
	}{
		{
			// No role accounted for is NOT a missing kind. Reporting False here would assert
			// something this pass has no observation for.
			name:        "no_role_statuses_is_unknown",
			roles:       nil,
			wantStatus:  "Unknown",
			wantReason:  modelDeploymentReasonNoRoleStatuses,
			wantMessage: "no role has been accounted for yet",
		},
		{
			name: "every_kind_ready_is_true",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 1),
				roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
			},
			wantStatus:  "True",
			wantReason:  modelDeploymentReasonAllKindsReady,
			wantMessage: "every role kind of this deployment has at least one ready replica",
		},
		{
			name: "a_missing_kind_is_false_and_named",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("p", workercore.ModelDeploymentRoleKindPrefill, 0),
				roleKindStatus("d", workercore.ModelDeploymentRoleKindDecode, 1),
			},
			wantStatus:  "False",
			wantReason:  modelDeploymentReasonKindsNotReady,
			wantMessage: "no replica is ready for role kinds prefill",
		},
		{
			// The single-kind shape, where this condition is deliberately quiet: the degradation is
			// real and is carried by the phase, which sums the counts. Asserting True here is what
			// records that silence as a decision rather than as a gap.
			name: "single_kind_with_one_role_empty_is_true",
			roles: []workercore.ModelDeploymentRoleStatus{
				roleKindStatus("a", workercore.ModelDeploymentRoleKindServer, 0),
				roleKindStatus("b", workercore.ModelDeploymentRoleKindServer, 1),
			},
			wantStatus:  "True",
			wantReason:  modelDeploymentReasonAllKindsReady,
			wantMessage: "every role kind of this deployment has at least one ready replica",
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			holder := &workercore.ModelDeployment{}
			holder.Status.Roles = c.roles

			observeModelDeploymentRoleKinds(holder)

			assert.Equal(t, c.wantStatus,
				ModelDeploymentConditionRoleKindsReady.GetStatus(holder), "status")
			assert.Equal(t, c.wantReason,
				ModelDeploymentConditionRoleKindsReady.GetReason(holder), "reason")
			assert.Equal(t, c.wantMessage,
				ModelDeploymentConditionRoleKindsReady.GetMessage(holder), "message")
		})
	}
}

// TestDeriveModelDeploymentPhase_SumsAcrossKindsAndDoesNotBranchOnThem pins the phase on the one
// shape that can tell its rule apart from a kind-aware one.
//
// The phase sums every role's counts before judging, so an entire kind with nothing ready and a
// single replica short produce the same value. That is its behaviour today and adding a condition
// about role kinds does NOT change it: the two answer different questions over the same numbers.
//
// THE FIXTURE HAS TO BE MULTI-KIND OR THIS ASSERTS NOTHING. The existing phase table builds one role
// and therefore one kind, and a single-kind fixture reports the same value under both rules -- so it
// would keep passing if the derivation were rewritten to branch on kind, which is exactly the change
// this exists to catch.
func TestDeriveModelDeploymentPhase_SumsAcrossKindsAndDoesNotBranchOnThem(t *testing.T) {
	testCases := []struct {
		name        string
		roles       []workercore.ModelDeploymentRoleStatus
		wantPhase   string
		wantMessage string
	}{
		{
			// One whole kind empty. A kind-aware phase would have something of its own to say here;
			// this one reports the sum, and that is the value being pinned.
			name: "one_kind_entirely_empty_is_still_the_summed_degraded",
			roles: []workercore.ModelDeploymentRoleStatus{
				{Name: "p", Kind: workercore.ModelDeploymentRoleKindPrefill, Desired: 2, Ready: 0},
				{Name: "d", Kind: workercore.ModelDeploymentRoleKindDecode, Desired: 2, Ready: 2},
			},
			wantPhase:   ModelDeploymentPhaseDegraded,
			wantMessage: "2 of 4 replicas are ready",
		},
		{
			// The same sum reached with both kinds represented. Identical phase and identical
			// message: the sum is all the phase looks at.
			name: "both_kinds_partly_ready_reaches_the_same_answer",
			roles: []workercore.ModelDeploymentRoleStatus{
				{Name: "p", Kind: workercore.ModelDeploymentRoleKindPrefill, Desired: 2, Ready: 1},
				{Name: "d", Kind: workercore.ModelDeploymentRoleKindDecode, Desired: 2, Ready: 1},
			},
			wantPhase:   ModelDeploymentPhaseDegraded,
			wantMessage: "2 of 4 replicas are ready",
		},
		{
			name: "every_kind_complete_is_ready",
			roles: []workercore.ModelDeploymentRoleStatus{
				{Name: "p", Kind: workercore.ModelDeploymentRoleKindPrefill, Desired: 2, Ready: 2},
				{Name: "d", Kind: workercore.ModelDeploymentRoleKindDecode, Desired: 1, Ready: 1},
			},
			wantPhase: ModelDeploymentPhaseReady,
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			md := &workercore.ModelDeployment{}
			holder := &workercore.ModelDeployment{}
			holder.Status.Roles = c.roles

			deriveModelDeploymentPhase(md, holder)

			assert.Equal(t, c.wantPhase, holder.Status.Phase, "phase")
			assert.Equal(t, c.wantMessage, holder.Status.PhaseMessage, "phaseMessage")
		})
	}
}

// TestObserveModelDeploymentQuota_PreemptedInPartSaysWhetherItStillServes pins the sentence the
// partial-preemption message now branches on, on both shapes that reach it.
//
// THE SHAPE DECIDES, NOT THE ROLE NAMES. A role's kind is a separate field that defaults to server,
// so two roles called "prefill" and "decode" with no kind set are two SERVER roles -- and that is
// the shape the defaults produce. Losing one of their groups leaves the other admitted and still
// answering requests, which is why a blanket "the deployment cannot serve" was false.
//
// THE COUNTERPART SETS THE KINDS EXPLICITLY, because a disaggregated deployment that loses every
// group of one kind genuinely cannot serve, and the two sentences must not be reachable by the same
// input. Asserting only one of them would leave the branch half-tested and green.
func TestObserveModelDeploymentQuota_PreemptedInPartSaysWhetherItStillServes(t *testing.T) {
	testCases := []struct {
		name              string
		kinds             bool
		neverAdmitted     bool
		wantIn, wantNotIn []string
	}{
		{
			name:      "two_server_roles_on_two_instance_types_still_serve",
			kinds:     false,
			wantIn:    []string{"still serves", "without the capacity those groups provided"},
			wantNotIn: []string{"cannot serve"},
		},
		{
			name:      "a_disaggregated_deployment_that_lost_a_whole_kind_cannot_serve",
			kinds:     true,
			wantIn:    []string{"cannot serve", "role kind decode"},
			wantNotIn: []string{"still serves"},
		},
		{
			// A kind that was NEVER admitted reaches this branch as well, because a sibling kind's
			// preemption is answered before incompleteness is. The sentence has to be true of that
			// state too, which is what rules out saying the kind is not admitted ANY MORE: it never
			// was. The wording is the whole subject of this case, so it is asserted literally.
			name:          "a_kind_that_never_had_an_admitted_group_is_not_described_as_having_lost_one",
			kinds:         true,
			neverAdmitted: true,
			wantIn:        []string{"role kind decode", "is admitted, so the deployment cannot serve"},
			wantNotIn:     []string{"any more", "still serves"},
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				decode := md.Spec.Roles[0]
				md.Spec.Roles[0].Name, md.Spec.Roles[0].Replicas = "prefill", 1
				decode.Name, decode.Replicas, decode.InstanceType = "decode", 1, "a100-8x"
				if c.kinds {
					md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
					decode.Kind = workercore.ModelDeploymentRoleKindDecode
				}
				md.Spec.Roles = append(md.Spec.Roles, decode)
				if c.neverAdmitted {
					// A SECOND GROUP OF THE PREFILL KIND, so that the reclaimed group and the
					// admitted one belong to the same kind. Prefill therefore stays served, and
					// decode is unserved for the single reason under test rather than for two.
					second := md.Spec.Roles[0]
					second.Name, second.InstanceType = "prefill-2", "h100-8x"
					md.Spec.Roles = append(md.Spec.Roles, second)
				}
			})

			// The fixture asserts its own shape, because the thing that decides these two cases is
			// invisible in the role names they share.
			kinds := map[workercore.ModelDeploymentRoleKind]struct{}{}
			for i := range md.Spec.Roles {
				kinds[ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i])] = struct{}{}
			}
			if c.kinds {
				require.Len(t, kinds, 2, "the disaggregated case must carry two distinct kinds")
			} else {
				require.Len(t, kinds, 1, "the default case must carry exactly one kind")
			}

			prefill, decode := roleReplica(md, "prefill"), roleReplica(md, "decode")

			// Prefill survives, decode is reclaimed: one group admitted and one preempted, which is
			// the only arrangement this branch answers for.
			wlPrefill := admittedWorkload(groupWorkload([]core.Pod{prefill}, true))
			wlPrefill.Name = "wl-prefill"

			pods := []core.Pod{prefill, decode}
			wls := []*kueue.Workload{wlPrefill}

			if c.neverAdmitted {
				// Decode's group is left WITHOUT a Workload, and the reclaimed one is prefill's
				// second group. That is the state the wording has to be true of.
				second := roleReplica(md, "prefill-2")
				wlSecond := preemptedWorkload(groupWorkload([]core.Pod{second}, false), "preempted")
				wlSecond.Name = "wl-prefill-2"
				pods = append(pods, second)
				wls = append(wls, wlSecond)

				require.Len(t, modelDeploymentPodGroups(md), 3,
					"three groups: one admitted, one reclaimed, one that never had a Workload")
				require.Len(t, wls, 2, "exactly one of those groups must carry no Workload at all")
			} else {
				wlDecode := preemptedWorkload(groupWorkload([]core.Pod{decode}, false), "preempted")
				wlDecode.Name = "wl-decode"
				wls = append(wls, wlDecode)
			}

			holder := new(workercore.ModelDeployment)
			observeQuotaOver(md, pods, wls, holder)

			require.Equal(t, modelDeploymentReasonPreemptedInPart,
				ModelDeploymentConditionQuotaReserved.GetReason(holder))

			msg := ModelDeploymentConditionQuotaReserved.GetMessage(holder)
			for _, want := range c.wantIn {
				assert.Contains(t, msg, want, "message: %s", msg)
			}
			for _, notWant := range c.wantNotIn {
				assert.NotContains(t, msg, notWant, "message: %s", msg)
			}
		})
	}
}
