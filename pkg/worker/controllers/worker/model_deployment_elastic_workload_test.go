package worker

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestElasticConvergenceCreatesChildren(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.KVCache = nil
	md.Spec.Engine.Version = "0.29.0"
	md.Spec.Roles[0].Replicas = 1
	md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: resource.NewQuantity(1, resource.DecimalSI)}
	md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2, HeadInstanceType: "cpu"}
	cpu := newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "cpu"
		it.Spec.Acceleratable = false
		it.Status.Entrance = "cpu-queue"
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType(), cpu)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	_, err := r.convergeModelDeployment(context.Background(), md)
	require.NoError(t, err)
	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace(md.Namespace)))
	require.Len(t, pods.Items, 3, "two GPU members and a CPU head must exist")
	gpuResource := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	groups := map[string]bool{}
	var master core.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		groups[pod.Labels[kueuepodconst.GroupNameLabel]] = true
		pod.UID = types.UID(fmt.Sprintf("uid-%d", i))
		c := pod.Spec.Containers[0]
		if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(md) {
			require.Equal(t, "cpu-queue", pod.Labels["kueue.x-k8s.io/queue-name"])
			require.NotContains(t, c.Resources.Limits, gpuResource)
			require.Contains(t, c.Command[2], "--num-gpus=0")
		} else {
			q := c.Resources.Limits[gpuResource]
			require.Equal(t, int64(1), q.Value())
			require.Contains(t, c.Command[2], "gpustack-pod-uid=\"$GPUSTACK_ELASTIC_POD_UID\"")
			if modelDeploymentOrdinalOrFloor(pod) == 0 {
				master = *pod.DeepCopy()
				require.Contains(t, c.Command, "--enable-elastic-ep")
			}
		}
		require.NoError(t, cli.Update(context.Background(), pod))
	}
	require.Len(t, groups, 3)
	require.NotEmpty(t, master.Name)
	services := new(core.ServiceList)
	require.NoError(t, cli.List(context.Background(), services, ctrlcli.InNamespace(md.Namespace)))
	require.GreaterOrEqual(t, len(services.Items), 2, "API and Ray head services must exist")
	for _, service := range services.Items {
		if service.Name != modelDeploymentElasticHeadName(md) {
			require.Equal(t, "0", service.Spec.Selector[modelDeploymentReplicaOrdinalLabel])
		}
	}
	for _, width := range []int32{4, 2} {
		md.Spec.Roles[0].ElasticEP.Width = width
		md.Annotations = map[string]string{ModelDeploymentElasticBootWidthAnnotation: "63"}
		_, err = r.convergeModelDeployment(context.Background(), md)
		require.NoError(t, err)
		require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 5, "width reduction must retain workers")
		found := false
		for _, pod := range pods.Items {
			if pod.Name == master.Name {
				found = true
				require.True(t, reflect.DeepEqual(master.Spec, pod.Spec))
				require.Equal(t, "2", pod.Annotations[ModelDeploymentElasticMasterBootWidthAnnotation])
				require.False(t, strings.Contains(pod.Spec.Containers[0].Command[2], "'63'"))
				require.Equal(t, master.UID, pod.UID)
			}
		}
		require.True(t, found, "master must survive width edits")
	}
}

func TestElasticUnknownBootstrapRetainsMembers(t *testing.T) {
	for _, raw := range []string{"", "bad", "1", "65"} {
		t.Run("boot="+raw, func(t *testing.T) {
			md := newRenderDeployment()
			md.Spec.KVCache = nil
			md.Spec.Roles[0].Replicas = 1
			md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2, HeadInstanceType: "cpu"}
			cpu := newRenderInstanceType(func(it *worker.InstanceType) { it.Name = "cpu"; it.Spec.Acceleratable = false })
			cli := newModelDeploymentClient(md, newRenderInstanceType(), cpu)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
			_, err := r.convergeModelDeployment(context.Background(), md)
			require.NoError(t, err)
			pods := new(core.PodList)
			require.NoError(t, cli.List(context.Background(), pods))
			for i := range pods.Items {
				pod := &pods.Items[i]
				if modelDeploymentPodRole(pod) == "server" && modelDeploymentOrdinalOrFloor(pod) == 0 {
					pod.UID = "master-uid"
					pod.Annotations[ModelDeploymentElasticMasterBootWidthAnnotation] = raw
					require.NoError(t, cli.Update(context.Background(), pod))
				}
			}
			md.Spec.Roles[0].ElasticEP.Width = 4
			_, err = r.convergeModelDeployment(context.Background(), md)
			require.NoError(t, err)
			require.NoError(t, cli.List(context.Background(), pods))
			require.Len(t, pods.Items, 3, "unknown bootstrap must neither create nor delete members")
		})
	}
}

func TestElasticCreateUsesUncachedOccupancy(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.KVCache = nil
	md.Spec.Roles[0].Replicas = 1
	md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2, HeadInstanceType: "cpu"}
	cpu := newRenderInstanceType(func(it *worker.InstanceType) { it.Name = "cpu"; it.Spec.Acceleratable = false })
	fresh := newModelDeploymentClient(md, newRenderInstanceType(), cpu)
	r := &ModelDeploymentReconciler{Client: fresh, APIReader: fresh}
	_, err := r.convergeModelDeployment(context.Background(), md)
	require.NoError(t, err)
	stale := newModelDeploymentClient(md, newRenderInstanceType(), cpu)
	r.Client = stale
	_, err = r.convergeModelDeployment(context.Background(), md)
	require.NoError(t, err)
	pods := new(core.PodList)
	require.NoError(t, stale.List(context.Background(), pods))
	require.Empty(t, pods.Items, "a lost create response must not create a second member")
}

func TestElasticWithdrawalWithoutRouterRetainsCapacity(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.md.Spec.Router = nil
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	before, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	_, master, err := elasticRuntimeMembers(f.md, before)
	require.NoError(t, err)
	confirmed, reason := f.reconciler.elasticWithdrawal(ctx, f.md, master)
	require.False(t, confirmed)
	require.Contains(t, reason, "declares no router")
	require.Zero(t, f.scaleCalls.Load())
	after, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	require.Len(t, after, len(before))
	for i := range before {
		retained := new(core.Pod)
		require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(&before[i]), retained))
		require.Equal(t, before[i].UID, retained.UID)
	}
}
