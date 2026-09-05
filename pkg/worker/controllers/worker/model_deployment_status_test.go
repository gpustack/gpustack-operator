package worker

import (
	"context"
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
	pod.Labels = modelDeploymentPodLabels(md, &md.Spec.Roles[0])
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
			status, err := r.computeModelDeploymentStatus(context.Background(), md, pods, nil)
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

	status, err := r.computeModelDeploymentStatus(context.Background(), md, pods, nil)
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

	status, err := r.computeModelDeploymentStatus(context.Background(), md, nil, nil)
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

	before := modelDeploymentRoleStatuses(md, nil, nil)
	require.Len(t, before, 2)
	assert.Nil(t, before[0].AssignedFlavor, "no Workload means no answer, not an empty answer")

	// THE HARDER ABSENCE, and the one the nil above does not reach: a Workload that EXISTS and has
	// no answer for this role. It is what a group looks like between composition and admission, and
	// it is the input that turns a missing guard into ptr.To("") -- a pointer that is set, to
	// nothing, which reads as an assignment to a flavor with no name.
	unadmitted := modelDeploymentRoleStatuses(md, nil, &kueue.Workload{})
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

	after := modelDeploymentRoleStatuses(md, nil, wl)
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
	half := modelDeploymentRoleStatuses(md, nil, partial)
	require.Len(t, half, 2)
	require.NotNil(t, half[0].AssignedFlavor)
	assert.Nil(t, half[1].AssignedFlavor,
		"a Workload that names no assignment for this role has no answer for it")
}

// TestModelDeploymentStatus_KindIsEchoedAndNeverEmpty pins the field that must never be written
// blank.
//
// status.roles[].kind is REQUIRED and carries no enum, so an unset value is serialized and stored as
// the empty string -- legal today only because the marker is missing. Resolving it here is what makes
// adding that marker safe rather than the change that starts rejecting every status write.
func TestModelDeploymentStatus_KindIsEchoedAndNeverEmpty(t *testing.T) {
	md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		// The second role's kind is deliberately left unset: it is what a deployment written before
		// disaggregation carries, and what any in-process caller builds.
		md.Spec.Roles[1].Kind = ""
	})

	statuses := modelDeploymentRoleStatuses(md, nil, nil)
	require.Len(t, statuses, 2)

	assert.Equal(t, workercore.ModelDeploymentRoleKindPrefill, statuses[0].Kind)
	assert.Equal(t, workercore.ModelDeploymentRoleKindServer, statuses[1].Kind,
		"an unset kind is the schema's server default, not a kind of its own")
	for i := range statuses {
		assert.NotEmpty(t, statuses[i].Kind, "roles[%d].kind is required and must never be blank", i)
	}
}

// TestObserveModelDeploymentQuota is a statement about ONE Workload, because the replicas are one
// pod group.
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
		name string
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
			observeModelDeploymentQuota(md, pods, wl, holder)

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

// TestObserveModelDeploymentQuota_TrueCoversEveryRole is F8's last acceptance, and it is only
// checkable because there is ONE Workload: the condition cannot be true for one role and not another
// when it is a statement about a single object that covers both PodSets.
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
	observeModelDeploymentQuota(md, pods, groupWorkload(pods, true), holder)

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
	observeModelDeploymentQuota(md, pods, groupWorkload(pods, false), holder)

	assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(holder), `"a100-4x"`)
}

// TestObserveModelDeploymentQuota_NoReplicas keeps the condition off False before anything exists.
// A deployment that has not rendered a Pod yet has not been refused quota.
func TestObserveModelDeploymentQuota_NoReplicas(t *testing.T) {
	md := newRenderDeployment()

	holder := new(workercore.ModelDeployment)
	observeModelDeploymentQuota(md, nil, nil, holder)

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
	observeModelDeploymentQuota(md, pods, nil, holder)

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

	require.NoError(t, r.syncModelDeploymentStatus(context.Background(), md, pods, nil))

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
	require.NoError(t, r.syncModelDeploymentStatus(context.Background(), md, pods, nil))
	require.Equal(t, 1, writes.statusUpdates)

	require.NoError(t, r.syncModelDeploymentStatus(context.Background(), md, pods, nil))
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

	status, err := r.computeModelDeploymentStatus(context.Background(), md, nil, nil)
	require.NoError(t, err)

	declared := make([]string, 0, len(status.Conditions))
	for _, c := range status.Conditions {
		declared = append(declared, c.Type)
	}
	assert.ElementsMatch(t, []string{
		string(ModelDeploymentConditionQuotaReserved),
		string(ModelDeploymentConditionCacheAttached),
	}, declared)
	assert.NotContains(t, declared, string(ModelDeploymentConditionDomainRegistered),
		"this pass was handed no reading of the Binding, and a pass that did not look must not report")

	holder := &workercore.ModelDeployment{Status: *status}
	assert.True(t, ModelDeploymentConditionQuotaReserved.IsUnknown(holder))
	assert.True(t, ModelDeploymentConditionCacheAttached.IsUnknown(holder))
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
			got, err := r.findModelDeploymentGroupWorkload(
				context.Background(), md, []core.Pod{*ours})
			require.NoError(t, err)

			if !tc.found {
				assert.Nil(t, got, tc.why)

				return
			}
			require.NotNil(t, got, tc.why)
			assert.Equal(t, wl.Name, got.Name)
		})
	}
}
