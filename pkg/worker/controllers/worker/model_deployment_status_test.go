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

// reservedWorkload builds the Kueue Workload a replica's Pod owns, with or without quota reserved.
func reservedWorkload(pod *core.Pod, reserved bool) *kueue.Workload {
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace = "pod-"+pod.Name+"-abcde", pod.Namespace
	wl.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: boolPtr(true),
	}}
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

// TestObserveModelDeploymentQuota reads each Workload's OWN conditions.
//
// That is not an implementation detail. The admission gate stops evaluating a Workload once it is
// admitted, so anything derived from the gate would answer for the moment of admission and never
// again — a Workload preempted since would still read as reserved.
func TestObserveModelDeploymentQuota(t *testing.T) {
	testCases := []struct {
		name string
		// reserved holds one entry per replica: true = its Workload has quota, false = it does not,
		// and a replica missing from this slice has no Workload at all.
		reserved    []bool
		workloads   int
		wantStatus  meta.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name: "every replica has quota", reserved: []bool{true, true}, workloads: 2,
			wantStatus: meta.ConditionTrue, wantReason: "Reserved",
		},
		{
			name: "one replica is waiting", reserved: []bool{true, false}, workloads: 2,
			wantStatus: meta.ConditionFalse, wantReason: "Pending",
			wantMessage: `1 of 2 replicas are waiting for quota in cluster queue "h20-8x"`,
		},
		{
			// Kueue creates the Workload asynchronously from the Pod, so its absence is admission in
			// flight and not a refusal. Reporting False here would name a fault that has not happened.
			name: "a replica has no workload yet", reserved: []bool{true, true}, workloads: 1,
			wantStatus: meta.ConditionUnknown, wantReason: "AdmissionInFlight",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = int32(len(tc.reserved))
			})

			objs := []ctrlcli.Object{md, newRenderInstanceType()}
			pods := make([]core.Pod, 0, len(tc.reserved))
			for i, reserved := range tc.reserved {
				pod := readyReplica(md, int32(i), true)
				pods = append(pods, *pod)
				if i < tc.workloads {
					objs = append(objs, reservedWorkload(pod, reserved))
				}
			}

			r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(objs...)}
			holder := new(workercore.ModelDeployment)
			require.NoError(t, r.observeModelDeploymentQuota(context.Background(), md, pods, holder))

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

// TestObserveModelDeploymentQuota_NamesTheClusterQueue states where an operator is sent when quota
// is short. The ClusterQueue is named after the InstanceType, so the queue is read off the spec
// rather than resolved — and a message that named the LocalQueue hash instead would send a reader
// to an object they cannot map back to anything they wrote.
func TestObserveModelDeploymentQuota_NamesTheClusterQueue(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].InstanceType = "a100-4x"
	})
	pod := readyReplica(md, 0, true)

	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(
		md, newRenderInstanceType(), reservedWorkload(pod, false))}
	holder := new(workercore.ModelDeployment)
	require.NoError(t, r.observeModelDeploymentQuota(
		context.Background(), md, []core.Pod{*pod}, holder))

	assert.Contains(t, ModelDeploymentConditionQuotaReserved.GetMessage(holder), `"a100-4x"`)
}

// TestObserveModelDeploymentQuota_NoReplicas keeps the condition off False before anything exists.
// A deployment that has not rendered a Pod yet has not been refused quota.
func TestObserveModelDeploymentQuota_NoReplicas(t *testing.T) {
	md := newRenderDeployment()
	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}

	holder := new(workercore.ModelDeployment)
	require.NoError(t, r.observeModelDeploymentQuota(context.Background(), md, nil, holder))
	assert.True(t, ModelDeploymentConditionQuotaReserved.IsUnknown(holder))
	assert.Equal(t, "NoReplicas", ModelDeploymentConditionQuotaReserved.GetReason(holder))
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

	r := &ModelDeploymentReconciler{Client: newModelDeploymentClient(md, newRenderInstanceType())}
	holder := new(workercore.ModelDeployment)
	require.NoError(t, r.observeModelDeploymentQuota(context.Background(), md, pods, holder))

	assert.True(t, ModelDeploymentConditionQuotaReserved.IsUnknown(holder))
	assert.Equal(t, "AllReplicasTerminating",
		ModelDeploymentConditionQuotaReserved.GetReason(holder))
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

// TestListModelDeploymentWorkloads_TakesTheControllerReference is the regression the owner-ref fix
// needed and did not ship with.
//
// An object may carry many owner references and at most one controller. Matching the first
// Pod-kinded reference attributes the Workload to whichever owner happens to be listed first, and a
// mis-attributed Workload reports ANOTHER replica's admission state as this one's — a wrong answer
// rather than a missing one, which is the failure that reads as correct.
//
// The API version is asserted for its own reason: "Pod" is not a reserved word, so a resource of
// that kind in another group matches on the kind alone.
func TestListModelDeploymentWorkloads_TakesTheControllerReference(t *testing.T) {
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: "team-a", Name: "qwen-server-0", UID: types.UID("uid-server-0"),
	}}
	other := types.UID("uid-someone-else")

	testCases := []struct {
		name  string
		refs  []meta.OwnerReference
		wantK types.UID
		why   string
	}{
		{
			name: "the controller reference, listed second",
			refs: []meta.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: "decoy", UID: other},
				{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: boolPtr(true)},
			},
			wantK: pod.UID,
			why:   "a non-controller reference listed first must not win",
		},
		{
			name: "a Pod kind from another group is not a Pod",
			refs: []meta.OwnerReference{
				{APIVersion: "acme.io/v1", Kind: "Pod", Name: "x", UID: other, Controller: boolPtr(true)},
			},
			why: "the kind alone does not identify the resource",
		},
		{
			name: "no controller at all",
			refs: []meta.OwnerReference{
				{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID},
			},
			why: "an owner that is not the controller does not say the Workload represents that Pod",
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
			got, err := r.listModelDeploymentWorkloads(context.Background(),
				&workercore.ModelDeployment{ObjectMeta: meta.ObjectMeta{Namespace: "team-a"}})
			require.NoError(t, err)

			if tc.wantK == "" {
				assert.Empty(t, got, tc.why)

				return
			}
			require.Len(t, got, 1, tc.why)
			assert.NotNil(t, got[tc.wantK], tc.why)
		})
	}
}
