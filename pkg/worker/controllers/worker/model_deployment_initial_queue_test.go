package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestLifecycle_AnInitialQueuedRoleAcceptsUpdatedSchedulingInputs(t *testing.T) {
	cases := []struct {
		name                               string
		replicas, size                     int32
		queuedOrdinal                      int
		admitSibling, changePool, reserved bool
	}{
		{name: "resource reduction", replicas: 1, size: 1},
		{name: "pool change", replicas: 1, size: 1, changePool: true},
		{name: "complete two-member group", replicas: 1, size: 2},
		{name: "queued group before healthy higher ordinal", replicas: 2, size: 1, admitSibling: true},
		{name: "one slot for multiple initial queues", replicas: 2, size: 1, queuedOrdinal: 1},
		{name: "first quota reservation before admission", replicas: 1, size: 1, reserved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = tc.replicas
				md.Spec.Roles[0].ReplicaSize = tc.size
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(resource.MustParse("2"))}
			})
			f := newLifecycleFixture(t, md, newRenderInstanceTypeB())
			f.pass(false)
			for ordinal := range int(tc.replicas) {
				setInitialReplicaQueue(t, f, ordinal)
			}
			standInForKueue(t, f.cli, false)
			if tc.admitSibling {
				setInitialReplicaAdmitted(t, f, 1)
			}
			before := f.live("server", tc.queuedOrdinal)
			require.Len(t, before, int(tc.size))
			accName := nodefeature.GetAcceleratableResourceName(
				nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
			qtyEqual(t, qty("2"), before[0].Spec.Containers[0].Resources.Requests[accName], "the initial request")
			oldWorkload := f.workloadNamed(before[0].Labels[kueuepodconst.GroupNameLabel])
			require.NotNil(t, oldWorkload)
			if tc.reserved {
				oldWorkload.Status.Admission = &kueue.Admission{ClusterQueue: "first-reservation"}
				oldWorkload.Status.Conditions = []meta.Condition{
					initialQueueCondition(kueue.WorkloadQuotaReserved, meta.ConditionTrue, "QuotaReserved"),
					initialQueueCondition(kueue.WorkloadAdmitted, meta.ConditionFalse, "NoReservation"),
					initialQueueCondition(kueue.WorkloadPodsReady, meta.ConditionFalse, kueue.WorkloadWaitForStart),
				}
				require.NoError(t, f.cli.Status().Update(context.Background(), oldWorkload))
			}
			_, holding := f.slot("server")
			require.False(t, holding)
			untouched := initialQueueSiblingUIDs(t, f, tc.queuedOrdinal)
			edited := getModelDeployment(t, f.cli)
			edited.Spec.Roles[0].Resources.Accelerator = ptr.To(resource.MustParse("1"))
			queue := "queue-for-h20-8x"
			if tc.changePool {
				edited.Spec.Roles[0].InstanceType = "a100-8x"
				queue = "queue-for-a100-8x"
			}
			require.NoError(t, f.cli.Update(context.Background(), edited))
			f.pass(false)
			slot, holding := f.slot("server")
			require.True(t, holding, "an initial queue starts replacement before any admission")
			require.Equal(t, tc.queuedOrdinal, slot.Ordinal)
			require.Len(t, slot.MemberUIDs, int(tc.size))
			require.Empty(t, f.live("server", tc.queuedOrdinal))
			f.admit()
			f.pass(false)
			setInitialReplicaQueue(t, f, tc.queuedOrdinal)
			standInForKueue(t, f.cli, false)
			for range 6 {
				f.pass(false)
			}
			members := f.live("server", tc.queuedOrdinal)
			require.Len(t, members, int(tc.size))
			for _, member := range members {
				for _, old := range before {
					require.NotEqual(t, old.UID, member.UID)
				}
				qtyEqual(t, qty("1"), member.Spec.Containers[0].Resources.Requests[accName], "the replacement request")
				qtyEqual(t, qty("16"), member.Spec.Containers[0].Resources.Limits[core.ResourceCPU], "derived CPU capacity")
				require.Equal(t, queue, member.Labels[kueuectrlconst.QueueLabel])
			}
			fresh := f.workloadNamed(members[0].Labels[kueuepodconst.GroupNameLabel])
			require.NotNil(t, fresh)
			require.NotEqual(t, oldWorkload.UID, fresh.UID)
			require.Equal(t, kueue.LocalQueueName(queue), fresh.Spec.QueueName)
			var count int32
			for _, podSet := range fresh.Spec.PodSets {
				count += podSet.Count
				qtyEqual(t, qty("1"), podSet.Template.Spec.Containers[0].Resources.Requests[accName], "the composed request")
			}
			require.Equal(t, tc.size, count)
			require.Equal(t, untouched, initialQueueSiblingUIDs(t, f, tc.queuedOrdinal), "queued replacement preserves sibling Pod and Workload identities")
			slot, holding = f.slot("server")
			require.True(t, holding)
			require.Equal(t, tc.queuedOrdinal, slot.Ordinal)
			setInitialReplicaAdmitted(t, f, tc.queuedOrdinal)
			f.pass(false)
			require.Len(t, f.live("server", tc.queuedOrdinal), int(tc.size))
			for range 8 {
				f.admit()
				for ordinal := range int(tc.replicas) {
					if len(f.live("server", ordinal)) > 0 {
						setInitialReplicaAdmitted(t, f, ordinal)
					}
				}
				f.pass(false)
			}
			_, holding = f.slot("server")
			require.False(t, holding, "every replacement has gained fresh admission")
			for ordinal := range int(tc.replicas) {
				settled := f.live("server", ordinal)
				require.Len(t, settled, int(tc.size))
				for _, member := range settled {
					qtyEqual(t, qty("1"), member.Spec.Containers[0].Resources.Requests[accName], "the converged request")
					require.Equal(t, queue, member.Labels[kueuectrlconst.QueueLabel])
				}
			}
		})
	}
}

func TestLifecycle_AnInitialQueueDoesNotIncludeExecutedOrEvictedGroups(t *testing.T) {
	cases := []struct {
		name, condition, reason                               string
		status                                                meta.ConditionStatus
		past, requeue, statistics, ungated, assigned, running bool
	}{
		{name: "admission revoked after execution", ungated: true, assigned: true, running: true, condition: kueue.WorkloadAdmitted, status: meta.ConditionFalse, reason: "NoReservation"},
		{name: "preempted", condition: kueue.WorkloadPreempted, status: meta.ConditionTrue},
		{name: "eviction history after reservation", condition: kueue.WorkloadEvicted, status: meta.ConditionFalse},
		{name: "requeued", condition: kueue.WorkloadRequeued, status: meta.ConditionTrue},
		{name: "previously ready", condition: kueue.WorkloadPodsReady, status: meta.ConditionTrue},
		{name: "waiting for recovery", condition: kueue.WorkloadPodsReady, status: meta.ConditionFalse, reason: kueue.WorkloadWaitForRecovery},
		{name: "zero-second prior execution", past: true},
		{name: "requeue state", requeue: true},
		{name: "eviction statistics", statistics: true},
		{name: "gate already removed", ungated: true},
		{name: "assigned node", assigned: true},
		{name: "runtime status", running: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t, newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 }))
			f.pass(false)
			setInitialReplicaQueue(t, f, 0)
			standInForKueue(t, f.cli, false)
			pod := f.live("server", 0)[0]
			if tc.ungated {
				pod.Spec.SchedulingGates = nil
			}
			if tc.assigned {
				pod.Spec.NodeName = "assigned-node"
			}
			require.NoError(t, f.cli.Update(context.Background(), pod))
			if tc.running {
				pod.Status.Phase = core.PodRunning
				pod.Status.ContainerStatuses = []core.ContainerStatus{{Name: pod.Spec.Containers[0].Name}}
				require.NoError(t, f.cli.Status().Update(context.Background(), pod))
			}
			wl := f.workloadNamed(pod.Labels[kueuepodconst.GroupNameLabel])
			require.NotNil(t, wl)
			if tc.condition != "" {
				wl.Status.Conditions = []meta.Condition{initialQueueCondition(tc.condition, tc.status, tc.reason)}
			}
			if tc.past {
				wl.Status.AccumulatedPastExecutionTimeSeconds = ptr.To(int32(0))
			}
			if tc.requeue {
				wl.Status.RequeueState = &kueue.RequeueState{}
			}
			if tc.statistics {
				wl.Status.SchedulingStats = &kueue.SchedulingStats{Evictions: []kueue.WorkloadSchedulingStatsEviction{{Reason: "Preempted", Count: 1}}}
			}
			require.NoError(t, f.cli.Status().Update(context.Background(), wl))
			edited := getModelDeployment(t, f.cli)
			edited.Spec.Roles[0].Command = []string{"sleep", "100"}
			require.NoError(t, f.cli.Update(context.Background(), edited))
			for range 6 {
				f.pass(false)
			}
			_, holding := f.slot("server")
			require.False(t, holding)
			members := f.live("server", 0)
			require.Len(t, members, 1)
			require.Equal(t, pod.UID, members[0].UID)
			preserved := f.workloadNamed(wl.Name)
			require.NotNil(t, preserved)
			require.Equal(t, wl.UID, preserved.UID)
		})
	}
}

func TestLifecycle_InitialQueueEligibilityReadsTheServerGate(t *testing.T) {
	f := newLifecycleFixture(t, newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 }))
	f.pass(false)
	setInitialReplicaQueue(t, f, 0)
	standInForKueue(t, f.cli, false)
	stale := f.live("server", 0)[0]
	standing := stale.DeepCopy()
	standing.Spec.SchedulingGates = nil
	require.NoError(t, f.cli.Update(context.Background(), standing))
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Command = []string{"sleep", "100"}
	require.NoError(t, f.cli.Update(context.Background(), edited))
	cache := ctrlinterceptor.NewClient(f.cli, ctrlinterceptor.Funcs{
		List: func(ctx context.Context, next ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
			if err := next.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*core.PodList); ok {
				for i := range pods.Items {
					if pods.Items[i].UID == stale.UID {
						pods.Items[i] = *stale.DeepCopy()
					}
				}
			}
			return nil
		},
	})
	r := &ModelDeploymentReconciler{Client: cache, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	_, holding := f.slot("server")
	require.False(t, holding)
	require.Len(t, f.live("server", 0), 1)
	require.Equal(t, stale.UID, f.live("server", 0)[0].UID)
}

func setInitialReplicaQueue(t *testing.T, f *lifecycleFixture, ordinal int) {
	t.Helper()
	members := f.live("server", ordinal)
	require.NotEmpty(t, members)
	for _, pod := range members {
		pod.Spec.SchedulingGates = []core.PodSchedulingGate{{Name: kueuepodconst.SchedulingGateName}}
		require.NoError(t, f.cli.Update(context.Background(), pod))
		pod.Status.Phase = core.PodPending
		require.NoError(t, f.cli.Status().Update(context.Background(), pod))
	}
}

func setInitialReplicaAdmitted(t *testing.T, f *lifecycleFixture, ordinal int) {
	t.Helper()
	members := f.live("server", ordinal)
	require.NotEmpty(t, members)
	for _, pod := range members {
		pod.Spec.SchedulingGates = nil
		pod.Spec.NodeName = "assigned-node"
		require.NoError(t, f.cli.Update(context.Background(), pod))
		pod.Status.Phase = core.PodRunning
		require.NoError(t, f.cli.Status().Update(context.Background(), pod))
	}
	wl := f.workloadNamed(members[0].Labels[kueuepodconst.GroupNameLabel])
	require.NotNil(t, wl)
	wl.Status.Conditions = []meta.Condition{initialQueueCondition(kueue.WorkloadAdmitted, meta.ConditionTrue, "Admitted")}
	require.NoError(t, f.cli.Status().Update(context.Background(), wl))
}

func initialQueueCondition(kind string, status meta.ConditionStatus, reason string) meta.Condition {
	if reason == "" {
		reason = kind
	}
	return meta.Condition{Type: kind, Status: status, Reason: reason, LastTransitionTime: meta.Now()}
}

func initialQueueSiblingUIDs(t *testing.T, f *lifecycleFixture, selected int) map[int][]types.UID {
	t.Helper()
	uids := map[int][]types.UID{}
	for ordinal := range int(f.md.Spec.Roles[0].Replicas) {
		if ordinal == selected {
			continue
		}
		members := f.live("server", ordinal)
		require.NotEmpty(t, members)
		for _, member := range members {
			uids[ordinal] = append(uids[ordinal], member.UID)
		}
		wl := f.workloadNamed(members[0].Labels[kueuepodconst.GroupNameLabel])
		require.NotNil(t, wl)
		uids[ordinal] = append(uids[ordinal], wl.UID)
	}
	return uids
}

func TestLifecycle_InitialQueueSlotAcceptsASecondResourceAndPoolEdit(t *testing.T) {
	f := newLifecycleFixture(t, newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(resource.MustParse("2"))}
	}), newRenderInstanceTypeB())
	f.pass(false)
	setInitialReplicaQueue(t, f, 0)
	standInForKueue(t, f.cli, false)
	first := getModelDeployment(t, f.cli)
	first.Spec.Roles[0].Resources.Accelerator = ptr.To(resource.MustParse("1"))
	require.NoError(t, f.cli.Update(context.Background(), first))
	f.pass(false)
	f.admit()
	f.pass(false)
	setInitialReplicaQueue(t, f, 0)
	standInForKueue(t, f.cli, false)
	queued := f.live("server", 0)[0]
	oldWorkload := f.workloadNamed(queued.Labels[kueuepodconst.GroupNameLabel])
	require.NotNil(t, oldWorkload)
	second := getModelDeployment(t, f.cli)
	second.Spec.Roles[0].Resources.Accelerator = ptr.To(resource.MustParse("3"))
	second.Spec.Roles[0].InstanceType = "a100-8x"
	require.NoError(t, f.cli.Update(context.Background(), second))
	f.pass(false)
	f.admit()
	f.pass(false)
	setInitialReplicaQueue(t, f, 0)
	standInForKueue(t, f.cli, false)
	for range 3 {
		f.pass(false)
	}
	members := f.live("server", 0)
	require.Len(t, members, 1)
	require.NotEqual(t, queued.UID, members[0].UID)
	accName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	qtyEqual(t, qty("3"), members[0].Spec.Containers[0].Resources.Requests[accName], "superseding request")
	wl := f.workloadNamed(members[0].Labels[kueuepodconst.GroupNameLabel])
	require.NotNil(t, wl)
	require.NotEqual(t, oldWorkload.UID, wl.UID)
	require.Equal(t, kueue.LocalQueueName("queue-for-a100-8x"), wl.Spec.QueueName)
	require.Len(t, wl.Spec.PodSets, 1)
	qtyEqual(t, qty("3"), wl.Spec.PodSets[0].Template.Spec.Containers[0].Resources.Requests[accName], "superseding composition")
	slot, holding := f.slot("server")
	require.True(t, holding)
	require.Equal(t, 0, slot.Ordinal)
}

// TestInitialQueuedReplica_CachedProgressSkipsTheServerRead locks in the cheap pre-filter: a member
// whose cached status already shows a node assignment cannot be in the initial queue, so the replica
// is refused without a quorum read per member.
func TestInitialQueuedReplica_CachedProgressSkipsTheServerRead(t *testing.T) {
	f := newLifecycleFixture(t, newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 }))
	f.pass(false)
	setInitialReplicaQueue(t, f, 0)

	member := f.live("server", 0)[0].DeepCopy()
	member.Spec.NodeName = "node-a"
	reads := 0
	reader := ctrlinterceptor.NewClient(f.cli, ctrlinterceptor.Funcs{
		Get: func(ctx context.Context, next ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
			reads++
			return next.Get(ctx, key, obj, opts...)
		},
	})
	r := &ModelDeploymentReconciler{Client: f.cli, APIReader: reader, Recorder: ctrlrecord.NewFakeRecorder(64)}

	queued, err := r.initialQueuedReplica(context.Background(), modelDeploymentReplicaView{
		Role: "server", Ordinal: 0, Seated: true, Members: []*core.Pod{member},
	}, nil)
	require.NoError(t, err)
	require.False(t, queued)
	require.Zero(t, reads, "a member with a node assignment must be refused before any server read")
}
