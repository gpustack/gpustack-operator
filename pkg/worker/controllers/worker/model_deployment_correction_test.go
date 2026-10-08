package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func releaseLifecyclePod(t *testing.T, cli ctrlcli.Client, pod *core.Pod) {
	t.Helper()
	stored := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), stored))
	stored.Finalizers = nil
	require.NoError(t, cli.Update(context.Background(), stored))
	if stored.DeletionTimestamp == nil {
		require.NoError(t, cli.Delete(context.Background(), stored))
	}
}

func releaseLifecycleWorkload(t *testing.T, cli ctrlcli.Client, wl *kueue.Workload) {
	t.Helper()
	stored := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(wl), stored))
	stored.Finalizers = nil
	require.NoError(t, cli.Update(context.Background(), stored))
	if stored.DeletionTimestamp == nil {
		require.NoError(t, cli.Delete(context.Background(), stored))
	}
}

func TestCorrection_WorkloadVacancyBeforeReusingOrdinal(t *testing.T) {
	cases := []struct {
		name    string
		active  bool
		foreign bool
	}{
		{name: "active_terminating", active: true},
		{name: "ordinary_terminating"},
		{name: "active_foreign", active: true, foreign: true},
		{name: "ordinary_foreign", foreign: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })
			f := newLifecycleFixture(t, md)
			f.pass(false)
			f.admit()
			before := f.live("server", 1)
			require.Len(t, before, 1)
			wl := f.workloadNamed(modelDeploymentReplicaGroupName(md, "server", 1))
			require.NotNil(t, wl)
			sibling := f.live("server", 0)
			require.Len(t, sibling, 1)
			siblingWL := f.workloadNamed(modelDeploymentReplicaGroupName(md, "server", 0))
			require.NotNil(t, siblingWL)
			if tc.active {
				edited := getModelDeployment(t, f.cli)
				edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
				require.NoError(t, f.cli.Update(ctx, edited))
				f.pass(false)
				require.Equal(t, 1, f.slotMust("server").Ordinal)
			} else {
				require.NoError(t, f.cli.Delete(ctx, before[0]))
				require.NoError(t, f.cli.Delete(ctx, wl))
			}
			releaseLifecyclePod(t, f.cli, before[0])
			if tc.foreign {
				releaseLifecycleWorkload(t, f.cli, wl)
				wl = &kueue.Workload{ObjectMeta: meta.ObjectMeta{
					Namespace: md.Namespace, Name: wl.Name, UID: types.UID("foreign-workload"),
					Finalizers: []string{"example.test/held"},
				}}
				require.NoError(t, f.cli.Create(ctx, wl))
			}
			for range 3 {
				f.pass(true)
				assert.Empty(t, f.live("server", 1), "a retained Workload forbids creation")
				held := f.workloadNamed(wl.Name)
				require.NotNil(t, held)
				assert.Equal(t, wl.UID, held.UID)
				if tc.foreign {
					assert.Nil(t, held.DeletionTimestamp, "foreign objects must not be deleted")
				}
				assert.Equal(t, sibling[0].UID, f.live("server", 0)[0].UID)
				assert.Equal(t, siblingWL.UID, f.workloadNamed(siblingWL.Name).UID)
			}
			releaseLifecycleWorkload(t, f.cli, wl)
			f.pass(true)
			after := f.live("server", 1)
			require.Len(t, after, 1, "authorized disappearance permits recreation")
			assert.NotEmpty(t, after[0].UID)
			assert.NotEqual(t, before[0].UID, after[0].UID)
			f.admit()
			fresh := f.workloadNamed(wl.Name)
			require.NotNil(t, fresh)
			assert.NotEqual(t, wl.UID, fresh.UID)
			assert.Equal(t, sibling[0].UID, f.live("server", 0)[0].UID)
			assert.Equal(t, siblingWL.UID, f.workloadNamed(siblingWL.Name).UID)
		})
	}
}

func queuedCorrectionFixture(t *testing.T, observed ...bool) (*lifecycleFixture, []*core.Pod, *kueue.Workload) {
	t.Helper()
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
	})
	f := newLifecycleFixture(t, md)
	f.pass(false)
	f.admit()
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(ctx, edited))
	f.pass(false)
	f.admit()
	f.pass(false)
	members := f.live("server", 1)
	require.Len(t, members, 2)
	wl, err := kueueComposeGroup(ctx, f.cli, modelDeploymentReplicaGroupName(md, "server", 1))
	require.NoError(t, err)
	require.NotNil(t, wl)
	require.NoError(t, f.cli.Create(ctx, wl))
	for _, member := range members {
		stored := new(core.Pod)
		require.NoError(t, f.cli.Get(ctx, ctrlcli.ObjectKeyFromObject(member), stored))
		stored.Finalizers = []string{kueuepodconst.PodFinalizer}
		require.NoError(t, f.cli.Update(ctx, stored))
	}
	if len(observed) == 0 || observed[0] {
		f.pass(true)
	}
	require.Equal(t, 1, f.slotMust("server").Ordinal)
	require.Empty(t, wl.Status.Conditions, "the composed group remains queued")
	return f, f.live("server", 1), f.workloadNamed(wl.Name)
}

func TestCorrection_CurrentReplacementMemberLoss(t *testing.T) {
	cases := []struct {
		name       string
		all        bool
		disappears bool
		unobserved bool
	}{
		{name: "terminating_gated_member"},
		{name: "member_gone", disappears: true},
		{name: "all_members_gone", all: true, disappears: true},
		{name: "all_members_gone_before_next_pass", all: true, disappears: true, unobserved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, members, wl := queuedCorrectionFixture(t, !tc.unobserved)
			require.NotNil(t, wl)
			require.NotEmpty(t, wl.UID)
			sibling := f.live("server", 0)
			require.Len(t, sibling, 2)
			siblingWL := f.workloadNamed(modelDeploymentReplicaGroupName(f.md, "server", 0))
			require.NotNil(t, siblingWL)
			lost := members[:1]
			if tc.all {
				lost = members
			}
			for _, member := range lost {
				require.NotEmpty(t, member.UID)
				require.NoError(t, f.cli.Delete(ctx, member))
				if tc.disappears {
					releaseLifecyclePod(t, f.cli, member)
				}
			}
			f.pass(true)
			held := f.workloadNamed(wl.Name)
			require.NotNil(t, held)
			assert.Equal(t, wl.UID, held.UID)
			require.NotNil(t, held.DeletionTimestamp, "the broken active group's reservation must leave")
			slot := f.slotMust("server")
			assert.Equal(t, 1, slot.Ordinal, "repair uses the current slot")
			assert.Empty(t, f.live("server", 1), "all surviving members leave before any replacement")
			for range 3 {
				f.pass(true)
				assert.Empty(t, f.live("server", 1), "held old identities cannot be topped up")
				for i, pod := range f.live("server", 0) {
					assert.Equal(t, sibling[i].UID, pod.UID)
				}
				assert.Equal(t, siblingWL.UID, f.workloadNamed(siblingWL.Name).UID)
			}
			f.admit()
			f.pass(true)
			after := f.live("server", 1)
			require.Len(t, after, 2)
			for _, pod := range after {
				require.NotEmpty(t, pod.UID)
				for _, old := range members {
					assert.NotEqual(t, old.UID, pod.UID)
				}
				assert.Equal(t, "vllm/vllm-openai:v0.26.0", pod.Spec.Containers[0].Image)
			}
			f.admit()
			fresh := f.workloadNamed(wl.Name)
			require.NotNil(t, fresh)
			assert.NotEqual(t, wl.UID, fresh.UID)
			for _, pod := range after {
				assert.True(t, modelDeploymentWorkloadOwnsAny(fresh, sets.New(pod.UID)))
			}
			for i, pod := range f.live("server", 0) {
				assert.Equal(t, sibling[i].UID, pod.UID)
			}
			assert.Equal(t, siblingWL.UID, f.workloadNamed(siblingWL.Name).UID)
		})
	}
}

func TestCorrection_MixedServicesSelectRoutingUnion(t *testing.T) {
	cases := []struct {
		name      string
		active    bool
		qualified bool
		external  bool
	}{
		{name: "revoked_managed", active: true},
		{name: "unknown_retains_active", active: true, qualified: true},
		{name: "never_activated_legacy"},
		{name: "external_ranks", active: true, qualified: true, external: true},
	}
	for _, tc := range cases {
		for _, desiredTakeover := range []bool{false, true} {
			name := tc.name + map[bool]string{false: "_to_managed", true: "_to_takeover"}[desiredTakeover]
			t.Run(name, func(t *testing.T) {
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Replicas = 2
					md.Spec.Roles[0].ReplicaSize = 2
					if tc.external {
						md.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-size", "2", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
					}
				})
				managed := replicaMembersAt(t, md, "server", 0)
				takeoverMD := md.DeepCopy()
				takeoverMD.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
				takeoverMD.Spec.Roles[0].ExtraArgs = nil
				takeover := replicaMembersAt(t, takeoverMD, "server", 1)
				require.Len(t, managed, 2)
				require.Len(t, takeover, 2)
				if tc.active {
					ModelDeploymentConditionEndpointEligibility.True(md, "EndpointsQualified", "activate engine qualification")
				}
				pods := make([]core.Pod, 0, 4)
				for i, pod := range append(managed, takeover...) {
					pod.UID = types.UID(string(rune('a' + i)))
					pod.Name = "member-" + string(pod.UID)
					pod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
					if i < 2 && tc.qualified {
						pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
					}
					pods = append(pods, *pod)
				}
				cli := newCompositionClient(md, &pods[0], &pods[1], &pods[2], &pods[3])
				if tc.active {
					for _, svc := range renderModelDeploymentServices(md, nil, pods[:2]) {
						require.NoError(t, cli.Create(context.Background(), svc))
					}
				}
				if desiredTakeover {
					md.Spec.Roles[0].Command = takeoverMD.Spec.Roles[0].Command
					md.Spec.Roles[0].ExtraArgs = nil
				}
				if tc.name == "unknown_retains_active" {
					ModelDeploymentConditionEndpointEligibility.Unknown(md, "ObservationHeld", "retain the activated selector")
				}
				r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
				require.NoError(t, r.convergeModelDeploymentEndpointEligibility(context.Background(), md, pods, nil, nil))
				actual := replicaPods(t, cli)
				require.Len(t, actual, 4)
				require.NoError(t, r.syncModelDeploymentService(context.Background(), md, false, actual))
				published := new(core.ServiceList)
				require.NoError(t, cli.List(context.Background(), published, ctrlcli.InNamespace(md.Namespace)))
				svcs := published.Items
				fronts, peers := 0, 0
				for _, svc := range svcs {
					selector := labels.SelectorFromSet(svc.Spec.Selector)
					if svc.Spec.ClusterIP == core.ClusterIPNone {
						peers++
						assert.NotContains(t, svc.Spec.Selector, modelDeploymentLabelKeyEndpointEligible)
						assert.NotContains(t, svc.Spec.Selector, modelDeploymentLabelKeyAPIAnswering)
						continue
					}
					fronts++
					for _, pod := range actual {
						ordinal, ok := modelDeploymentPodOrdinal(&pod)
						require.True(t, ok)
						leader := modelDeploymentAnsweringMemberLeader(&pod)
						want := leader
						if ordinal == 0 {
							want = (leader || tc.external) && (!tc.active || tc.qualified)
						}
						assert.Equal(t, want, selector.Matches(labels.Set(pod.Labels)), "%s full selector for %s", svc.Name, pod.Name)
					}
				}
				require.Equal(t, 2, fronts, "deployment and role HTTP selectors are both checked")
				require.Equal(t, 2, peers)
				// A later Unknown observation still honors the original activation decision.
				if tc.active {
					ModelDeploymentConditionEndpointEligibility.Unknown(md, "ObservationHeld", "retain activation")
					for i := range actual {
						delete(actual[i].Labels, modelDeploymentLabelKeyEndpointEligible)
						require.NoError(t, cli.Update(context.Background(), &actual[i]))
					}
					require.NoError(t, r.convergeModelDeploymentEndpointEligibility(context.Background(), md, actual, nil, nil))
					actual = replicaPods(t, cli)
					require.NoError(t, r.syncModelDeploymentService(context.Background(), md, false, actual))
					require.NoError(t, cli.List(context.Background(), published, ctrlcli.InNamespace(md.Namespace)))
					for _, svc := range published.Items {
						if svc.Spec.ClusterIP == core.ClusterIPNone {
							continue
						}
						for _, pod := range actual {
							ordinal, _ := modelDeploymentPodOrdinal(&pod)
							want := ordinal == 1 && modelDeploymentAnsweringMemberLeader(&pod)
							assert.Equal(t, want, labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels)),
								"%s retains managed revocation after Unknown for %s", svc.Name, pod.Name)
						}
					}
				}
			})
		}
	}
}

func TestCorrection_CurrentCleanupLostDeleteResponse(t *testing.T) {
	ctx := context.Background()
	f, members, wl := queuedCorrectionFixture(t)
	sibling := f.live("server", 0)
	require.Len(t, sibling, 2)
	siblingWL := f.workloadNamed(modelDeploymentReplicaGroupName(f.md, "server", 0))
	require.NotNil(t, siblingWL)
	require.NoError(t, f.cli.Delete(ctx, members[0]))
	lost := false
	writer := ctrlinterceptor.NewClient(f.cli, ctrlinterceptor.Funcs{
		Delete: func(ctx context.Context, next ctrlcli.WithWatch, obj ctrlcli.Object,
			opts ...ctrlcli.DeleteOption,
		) error {
			if err := next.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			if pod, ok := obj.(*core.Pod); ok && pod.UID == members[1].UID && !lost {
				lost = true
				return errors.New("connection lost after successful delete")
			}
			return nil
		},
	})
	r := &ModelDeploymentReconciler{Client: writer, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
	_, err := reconcileModelDeploymentWith(t, r)
	require.ErrorContains(t, err, "connection lost")
	require.True(t, lost)
	slot := f.slotMust("server")
	assert.Equal(t, wl.UID, slot.WorkloadUID, "identity was persisted before cleanup")
	assert.Equal(t, 1, slot.Ordinal)
	// All Pod finalizers complete before the retry. Only the persisted Workload identity remains.
	f.admit()
	require.Empty(t, f.live("server", 1))
	require.NotNil(t, f.workloadNamed(wl.Name))
	f.pass(true)
	held := f.workloadNamed(wl.Name)
	require.NotNil(t, held)
	require.NotNil(t, held.DeletionTimestamp, "retry deletes the captured reservation without live Pods")
	assert.Empty(t, f.live("server", 1))
	f.admit()
	f.pass(true)
	after := f.live("server", 1)
	require.Len(t, after, 2)
	for _, pod := range after {
		for _, old := range members {
			assert.NotEqual(t, old.UID, pod.UID)
		}
	}
	for i, pod := range f.live("server", 0) {
		assert.Equal(t, sibling[i].UID, pod.UID)
	}
	assert.Equal(t, siblingWL.UID, f.workloadNamed(siblingWL.Name).UID)
}

func TestCorrection_UnknownShapeRetainsOnlyCarriedRouting(t *testing.T) {
	cases := []struct {
		name       string
		absent     bool
		notReady   bool
		peerFailed bool
		replacing  bool
		want       bool
	}{
		{name: "retain_selected", want: true},
		{name: "never_grant_absent", absent: true},
		{name: "not_ready_withdraws", notReady: true},
		{name: "positive_peer_failure_withdraws", peerFailed: true},
		{name: "selected_replacement_withdraws", replacing: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].Replicas = 2
			})
			managed := replicaMembersAt(t, md, "server", 0)
			takeoverMD := md.DeepCopy()
			takeoverMD.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
			takeover := replicaMembersAt(t, takeoverMD, "server", 1)
			require.Len(t, managed, 2)
			require.Len(t, takeover, 2)
			pods := make([]core.Pod, 0, 4)
			objects := make([]ctrlcli.Object, 0, 1+len(managed)+len(takeover))
			objects = append(objects, md)
			for i, member := range append(managed, takeover...) {
				member.UID = types.UID("unknown-member-" + string(rune('a'+i)))
				member.Name = string(member.UID)
				member.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
				pods = append(pods, *member)
			}
			leader := &pods[0]
			require.True(t, modelDeploymentAnsweringMemberLeader(leader))
			leader.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
			if !tc.absent {
				leader.Labels[modelDeploymentLabelKeyAPIAnswering] = modelDeploymentAPIAnsweringValue
			}
			pods[1].Annotations[kueuepodconst.GroupTotalCountAnnotation] = "3"
			if tc.notReady {
				leader.Status.Conditions[0].Status = core.ConditionFalse
			}
			if tc.peerFailed {
				pods[1].Status.Conditions[0].Status = core.ConditionFalse
			}
			for i := range pods {
				objects = append(objects, &pods[i])
			}
			cli := newCompositionClient(objects...)
			ModelDeploymentConditionEndpointEligibility.True(md, "EndpointsQualified", "already activated")
			for _, svc := range renderModelDeploymentServices(md, nil, pods) {
				require.NoError(t, cli.Create(ctx, svc))
			}
			ModelDeploymentConditionEndpointEligibility.Unknown(md, "ObservationHeld", "unreadable shape")
			var replacing map[string]map[int]bool
			if tc.replacing {
				replacing = map[string]map[int]bool{"server": {0: true}}
			}
			qualifications := qualifyModelDeploymentInstances(ctx, md, pods,
				modelDeploymentPendingReplacement{ordinals: replacing}, defaultGroupForwardFetch)
			byMember := modelDeploymentQualificationsByMember(qualifications)
			q, found := byMember[leader.UID]
			require.True(t, found)
			require.Equal(t, modelDeploymentLegUnknown, q.Leg(modelDeploymentLegMemberSetComplete).Verdict)
			require.Equal(t, tc.notReady || tc.peerFailed || tc.replacing, q.HasFailure(),
				"unknown shape alone supplies no positive health failure")
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
			require.NoError(t, r.convergeModelDeploymentEndpointEligibility(ctx, md, pods, byMember, replacing))
			actual := replicaPods(t, cli)
			require.Len(t, actual, 4)
			require.NoError(t, r.syncModelDeploymentService(ctx, md, false, actual))
			stored := new(core.Pod)
			require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(leader), stored))
			assert.Equal(t, tc.want, stored.Labels[modelDeploymentLabelKeyAPIAnswering] == modelDeploymentAPIAnsweringValue)
			for _, name := range []string{md.Name, md.Name + "-server"} {
				svc := new(core.Service)
				require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: name}, svc))
				assert.Equal(t, tc.want, labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(stored.Labels)),
					"%s uses carried membership through Unknown", name)
			}
		})
	}
}
