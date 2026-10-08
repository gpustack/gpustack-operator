package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestFinalReview_OrdinalLessPodDoesNotInterruptAnActiveReplacement(t *testing.T) {
	ctx := context.Background()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })
	f := newLifecycleFixture(t, md)
	f.pass(false)
	f.admit()
	sibling := f.live("server", 0)
	require.Len(t, sibling, 1)
	siblingWL := f.workloadNamed(modelDeploymentReplicaGroupName(md, "server", 0))
	require.NotNil(t, siblingWL)
	edited := getModelDeployment(t, f.cli)
	edited.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
	require.NoError(t, f.cli.Update(ctx, edited))
	f.pass(false)
	slot := f.slotMust("server")
	require.Equal(t, modelDeploymentReplacementCleanup, slot.Phase)
	require.NotEmpty(t, slot.MemberUIDs)

	legacy := sibling[0].DeepCopy()
	legacy.Name = "qwen-server-legacy"
	legacy.UID = ""
	legacy.ResourceVersion = ""
	delete(legacy.Labels, modelDeploymentReplicaOrdinalLabel)
	legacy.Labels[kueuepodconst.GroupNameLabel] = legacy.Name
	require.NoError(t, f.cli.Create(ctx, legacy))
	require.NotEmpty(t, legacy.UID)
	view := ctrlinterceptor.NewClient(f.cli, ctrlinterceptor.Funcs{
		List: func(ctx context.Context, next ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
			if err := next.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*core.PodList); ok {
				for i := range pods.Items {
					if replacementCapturesMember(slot, pods.Items[i].UID) {
						pods.Items[i].DeletionTimestamp = nil
					}
				}
			}
			return nil
		},
	})
	r := &ModelDeploymentReconciler{Client: view, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
	require.NotPanics(t, func() {
		_, err := reconcileModelDeploymentWith(t, r)
		require.NoError(t, err)
	})
	removed := new(core.Pod)
	require.NoError(t, f.cli.Get(ctx, ctrlcli.ObjectKeyFromObject(legacy), removed))
	assert.NotNil(t, removed.DeletionTimestamp, "the ordinal-less Pod is repaired independently")
	assert.Equal(t, slot.Ordinal, f.slotMust("server").Ordinal)
	assert.Equal(t, sibling[0].UID, f.live("server", 0)[0].UID)
	assert.Equal(t, siblingWL.UID, f.workloadNamed(siblingWL.Name).UID)
	for range 8 {
		f.admit()
		f.pass(true)
	}
	assert.True(t, allAtImage(f, "vllm/vllm-openai:v0.26.0", 2))
}

func TestFinalReview_ServicesKeepDeployedRanksBeforeNewConfigurationAppears(t *testing.T) {
	cases := []struct {
		name     string
		takeover bool
	}{
		{name: "managed_to_takeover", takeover: true},
		{name: "external_to_internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-size", "2", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
			})
			ModelDeploymentConditionEndpointEligibility.True(md, "EndpointsQualified", "activate qualification")
			members := replicaMembersAt(t, md, "server", 0)
			require.Len(t, members, 2)
			pods := make([]core.Pod, 0, len(members))
			objects := make([]ctrlcli.Object, 0, 1+len(members))
			objects = append(objects, md)
			for _, member := range members {
				member.UID = types.UID("rank-" + member.Name)
				member.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
				member.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
				pods = append(pods, *member)
				objects = append(objects, member)
			}
			cli := newCompositionClient(objects...)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
			require.NoError(t, r.convergeModelDeploymentEndpointEligibility(ctx, md, pods, nil, nil))
			pods = replicaPods(t, cli)
			require.Len(t, pods, 2)
			require.NoError(t, r.syncModelDeploymentService(ctx, md, false, pods))
			for _, pod := range pods {
				for _, name := range []string{md.Name, md.Name + "-server"} {
					svc := new(core.Service)
					require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: name}, svc))
					require.True(t, labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels)), "the starting selector serves each rank")
				}
			}

			md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", "2"}
			if tc.takeover {
				md.Spec.Roles[0].Command = []string{"vllm", "serve", "qwen", "--port", "8000"}
				md.Spec.Roles[0].ExtraArgs = nil
			}
			require.NoError(t, r.convergeModelDeploymentEndpointEligibility(ctx, md, pods, nil, nil))
			pods = replicaPods(t, cli)
			require.Len(t, pods, 2, "no new mode or shape has appeared")
			require.NoError(t, r.syncModelDeploymentService(ctx, md, false, pods))
			for _, name := range []string{md.Name, md.Name + "-server"} {
				svc := new(core.Service)
				require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: name}, svc))
				for _, pod := range pods {
					assert.True(t, labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels)), "%s still serves deployed rank %s", name, pod.Name)
				}
			}
		})
	}
}

func TestFinalReview_CacheLagDoesNotRemovePeerDNS(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		name := map[bool]string{false: "standing", true: "terminating"}[terminating]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 1
				md.Spec.Roles[0].ReplicaSize = 2
			})
			f := newLifecycleFixture(t, md)
			f.pass(false)
			f.admit()
			members := f.live("server", 0)
			require.Len(t, members, 2)
			published := new(core.ServiceList)
			require.NoError(t, f.cli.List(ctx, published, ctrlcli.InNamespace(md.Namespace)))
			peers := make([]core.Service, 0, 1)
			for _, svc := range published.Items {
				if svc.Spec.ClusterIP == core.ClusterIPNone {
					peers = append(peers, svc)
				}
			}
			require.Len(t, peers, 1)
			peerName := peers[0].Name
			peer := peers[0].DeepCopy()
			if terminating {
				for _, member := range members {
					require.NoError(t, f.cli.Delete(ctx, member))
				}
			}
			edited := getModelDeployment(t, f.cli)
			edited.Spec.Roles[0].ReplicaSize = 1
			require.NoError(t, f.cli.Update(ctx, edited))
			view := ctrlinterceptor.NewClient(f.cli, ctrlinterceptor.Funcs{
				List: func(ctx context.Context, next ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
					if err := next.List(ctx, list, opts...); err != nil {
						return err
					}
					if pods, ok := list.(*core.PodList); ok {
						pods.Items = nil
					}
					return nil
				},
			})
			r := &ModelDeploymentReconciler{Client: view, APIReader: f.cli, Recorder: ctrlrecord.NewFakeRecorder(64)}
			_, err := reconcileModelDeploymentWith(t, r)
			require.NoError(t, err)
			require.Len(t, replicaPods(t, f.cli), 2, "the server still holds the old members")
			require.Empty(t, replicaPods(t, view), "the cache has not observed them")
			err = f.cli.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: peerName}, peer)
			assert.NoError(t, err, "old members retain peer DNS through cache lag")
			for _, member := range members {
				releaseLifecyclePod(t, f.cli, member)
			}
			f.pass(true)
			err = f.cli.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: peerName}, peer)
			assert.True(t, kerrors.IsNotFound(err), "peer DNS leaves once every old member is gone")
		})
	}
}
