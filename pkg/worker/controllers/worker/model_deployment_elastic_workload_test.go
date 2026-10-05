package worker

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
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

func TestElasticDeletingPodReplacement(t *testing.T) {
	for _, tc := range []struct {
		name         string
		phase        core.PodPhase
		replacements int
	}{
		{name: "succeeded", phase: core.PodSucceeded, replacements: 1},
		{name: "failed", phase: core.PodFailed, replacements: 1},
		{name: "running drain", phase: core.PodRunning, replacements: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newElasticConvergenceFixture(t)
			pods := new(core.PodList)
			require.NoError(t, f.reconciler.Client.List(ctx, pods, ctrlcli.InNamespace(f.md.Namespace)))
			var member *core.Pod
			for i := range pods.Items {
				if modelDeploymentPodRole(&pods.Items[i]) == "server" && modelDeploymentOrdinalOrFloor(&pods.Items[i]) == 1 {
					member = pods.Items[i].DeepCopy()
					break
				}
			}
			require.NotNil(t, member)
			require.NoError(t, f.reconciler.Client.Delete(ctx, member))
			member.UID = "held-member"
			member.ResourceVersion = ""
			member.DeletionTimestamp = nil
			member.Finalizers = append(member.Finalizers, "kueue.x-k8s.io/managed")
			member.Status.Phase = tc.phase
			require.NoError(t, f.reconciler.Client.Create(ctx, member))
			require.NoError(t, f.reconciler.Client.Delete(ctx, member))
			require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(member), member))
			require.NotNil(t, member.DeletionTimestamp)
			require.Equal(t, tc.phase, member.Status.Phase)
			require.Contains(t, member.Finalizers, "kueue.x-k8s.io/managed")

			_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)
			require.NoError(t, f.reconciler.Client.List(ctx, pods, ctrlcli.InNamespace(f.md.Namespace)))
			created := 0
			for i := range pods.Items {
				pod := &pods.Items[i]
				if modelDeploymentPodRole(pod) == "server" && modelDeploymentOrdinalOrFloor(pod) == 1 && pod.UID != member.UID {
					created++
				}
			}
			require.Equal(t, tc.replacements, created)
			require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(member), member))
			require.Contains(t, member.Finalizers, "kueue.x-k8s.io/managed", "replacement must not require finalizer surgery")
		})
	}
}

// elasticTerminalFixtureMember returns the fixture's ordinal-one member, which every case here makes
// terminal: the ordinal is the one the fixture's width leaves free for a replacement.
func elasticTerminalFixtureMember(t *testing.T, f *elasticConvergenceFixture) *core.Pod {
	t.Helper()
	pods := new(core.PodList)
	require.NoError(t, f.reconciler.Client.List(context.Background(), pods,
		ctrlcli.InNamespace(f.md.Namespace)))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if modelDeploymentPodRole(pod) == "server" && modelDeploymentOrdinalOrFloor(pod) == 1 {
			return pod.DeepCopy()
		}
	}
	t.Fatal("the fixture has no ordinal-one member")

	return nil
}

// elasticKueueWorkloadFor is the Workload Kueue admits for one member: one owner reference naming that
// Pod, which is what the release path matches on.
func elasticKueueWorkloadFor(md *workercore.ModelDeployment, member *core.Pod) *kueue.Workload {
	wl := &kueue.Workload{}
	wl.Name, wl.Namespace = "wl-"+string(member.UID), md.Namespace
	wl.OwnerReferences = []meta.OwnerReference{
		{APIVersion: "v1", Kind: "Pod", Name: member.Name, UID: member.UID},
	}

	return wl
}

// TestElasticTerminalMemberIsRemoved covers the departure no other elastic path makes: a member that
// reached a terminal phase with NO deletion timestamp. Kubernetes never garbage-collects such a Pod,
// and the create loop deliberately reads it as absent so its ordinal can be refilled, so the dead
// member would otherwise stay behind as an admitted member of the very group its replacement joins.
func TestElasticTerminalMemberIsRemoved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase core.PodPhase
	}{
		{name: "succeeded", phase: core.PodSucceeded},
		{name: "failed", phase: core.PodFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newElasticConvergenceFixture(t)
			member := elasticTerminalFixtureMember(t, f)
			member.Status.Phase = tc.phase
			require.NoError(t, f.reconciler.Client.Status().Update(ctx, member))
			require.Nil(t, member.DeletionTimestamp, "the case under test is the one nothing is deleting")
			wl := elasticKueueWorkloadFor(f.md, member)
			require.NoError(t, f.reconciler.Client.Create(ctx, wl))

			_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)

			err = f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(member), new(core.Pod))
			require.True(t, kerrors.IsNotFound(err),
				"a terminal member nothing else deletes must not be left behind")
			err = f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(wl), new(kueue.Workload))
			require.True(t, kerrors.IsNotFound(err),
				"the Workload is what releases the member's finalizer and the group's quota")

			pods := new(core.PodList)
			require.NoError(t, f.reconciler.Client.List(ctx, pods, ctrlcli.InNamespace(f.md.Namespace)))
			live := 0
			for i := range pods.Items {
				pod := &pods.Items[i]
				if modelDeploymentPodRole(pod) != "server" || modelDeploymentOrdinalOrFloor(pod) != 1 {
					continue
				}
				if pod.UID == member.UID {
					continue
				}
				live++
				require.NotEqual(t, core.PodSucceeded, pod.Status.Phase,
					"a replacement must not be born terminal")
			}
			require.Equal(t, 1, live, "the vacated ordinal must be refilled, and the refill must survive the pass")
		})
	}
}

// TestElasticTerminatingMemberWorkloadIsReleased covers a member that is already on its way out
// behind Kueue's finalizer: the delete came from a hand, a drain or an eviction, so no protocol of
// this operator issued the Workload delete, and the fixed path's stranded sweep sits behind the
// elastic return. The Workload is what releases the member's finalizer and the group's quota, and
// the release keeps the fixed path's safety condition: a Workload that also owns a member still
// standing is left alone, because Kueue answers a deleted Workload by stopping the whole group.
func TestElasticTerminatingMemberWorkloadIsReleased(t *testing.T) {
	for _, tc := range []struct {
		name         string
		liveSibling  bool
		wantReleased bool
	}{
		{name: "the Workload also owns a member still standing", liveSibling: true, wantReleased: false},
		{name: "the Workload owns only the departing member", liveSibling: false, wantReleased: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newElasticConvergenceFixture(t)
			pods := new(core.PodList)
			require.NoError(t, f.reconciler.Client.List(ctx, pods, ctrlcli.InNamespace(f.md.Namespace)))
			var member, standing *core.Pod
			for i := range pods.Items {
				pod := &pods.Items[i]
				if modelDeploymentPodRole(pod) != "server" {
					continue
				}
				switch modelDeploymentOrdinalOrFloor(pod) {
				case 0:
					standing = pod.DeepCopy()
				case 1:
					member = pod.DeepCopy()
				}
			}
			require.NotNil(t, member, "the fixture has no ordinal-one member")
			require.NotNil(t, standing, "the fixture has no ordinal-zero member")

			// The member is rebuilt as terminating on a Running phase, so the terminal-member
			// removal is out of the picture and only the stranded release can answer for the
			// Workload: the delete is foreign, the finalizer holds the Pod, nothing replaces it.
			require.NoError(t, f.reconciler.Client.Delete(ctx, member))
			member.UID = "terminating-member"
			member.ResourceVersion = ""
			member.DeletionTimestamp = nil
			member.Finalizers = append(member.Finalizers, "kueue.x-k8s.io/managed")
			member.Status.Phase = core.PodRunning
			require.NoError(t, f.reconciler.Client.Create(ctx, member))
			require.NoError(t, f.reconciler.Client.Delete(ctx, member))
			require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(member), member))
			require.NotNil(t, member.DeletionTimestamp,
				"the case under test is a member stuck terminating behind the finalizer")

			wl := elasticKueueWorkloadFor(f.md, member)
			if tc.liveSibling {
				wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
					APIVersion: "v1", Kind: "Pod", Name: standing.Name, UID: standing.UID,
				})
			}
			require.NoError(t, f.reconciler.Client.Create(ctx, wl))

			_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)

			err = f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(wl), new(kueue.Workload))
			if tc.wantReleased {
				require.True(t, kerrors.IsNotFound(err),
					"a stranded member's Workload must be released: it is what lifts Kueue's finalizer and frees the group's quota")
			} else {
				require.NoError(t, err,
					"a Workload owning a member still standing must be left alone: Kueue answers a deleted Workload by stopping the whole group")
			}
			require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(member), member))
			require.NotNil(t, member.DeletionTimestamp)
			require.Contains(t, member.Finalizers, "kueue.x-k8s.io/managed",
				"the release does no finalizer surgery on the member itself")
		})
	}
}

func TestElasticGPUMembersCarryIsolatedSharedMemory(t *testing.T) {
	for _, tc := range []struct {
		name         string
		additional   []workercore.ModelDeploymentAdditionalVolume
		ordinary     bool
		wantEmptyDir string
	}{
		{name: "default isolated emptyDir", wantEmptyDir: "512Mi"},
		{name: "ordinary render is unchanged", ordinary: true},
		{
			name: "explicit mount is preserved",
			additional: []workercore.ModelDeploymentAdditionalVolume{{
				MountPath: "/dev/shm",
				ConfigMap: &core.LocalObjectReference{Name: "user-shm"},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			md.Spec.KVCache = nil
			md.Spec.Roles[0].Replicas = 1
			if !tc.ordinary {
				md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2, HeadInstanceType: "cpu"}
			}
			md.Spec.Roles[0].AdditionalVolumes = tc.additional
			cpu := newRenderInstanceType(func(it *worker.InstanceType) { it.Name = "cpu"; it.Spec.Acceleratable = false })
			cli := newModelDeploymentClient(md, newRenderInstanceType(), cpu)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
			_, err := r.convergeModelDeployment(context.Background(), md)
			require.NoError(t, err)
			pods := new(core.PodList)
			require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace(md.Namespace)))

			require.NotEmpty(t, pods.Items)
			members := 0
			for i := range pods.Items {
				pod := &pods.Items[i]
				container := pod.Spec.Containers[0]
				var shmNames []string
				seen := map[string]bool{}
				for _, m := range container.VolumeMounts {
					require.False(t, seen[m.MountPath], "%s mounts %s twice", pod.Name, m.MountPath)
					seen[m.MountPath] = true
					if m.MountPath == "/dev/shm" {
						shmNames = append(shmNames, m.Name)
					}
				}
				if tc.ordinary {
					require.Empty(t, shmNames, "%s is an ordinary render, not an elastic member", pod.Name)
					continue
				}
				if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(md) {
					// Explicit role mounts also reach the CPU head. Only the default must stay absent.
					if tc.additional == nil {
						require.Empty(t, shmNames, "the CPU head gains no shared memory of its own")
					}
					continue
				}
				members++
				require.Len(t, shmNames, 1, "%s must mount /dev/shm exactly once", pod.Name)
				var backing *core.Volume
				for j := range pod.Spec.Volumes {
					require.Nil(t, pod.Spec.Volumes[j].HostPath, "%s reaches the host filesystem", pod.Name)
					if pod.Spec.Volumes[j].Name == shmNames[0] {
						backing = &pod.Spec.Volumes[j]
					}
				}
				require.NotNil(t, backing, "%s backs /dev/shm with a declared volume", pod.Name)
				volumes := map[string]bool{}
				for _, v := range pod.Spec.Volumes {
					require.False(t, volumes[v.Name], "%s declares volume %s twice", pod.Name, v.Name)
					volumes[v.Name] = true
				}
				if tc.wantEmptyDir == "" {
					require.NotNil(t, backing.ConfigMap, "%s keeps the mount its role declared", pod.Name)
					continue
				}
				require.Nil(t, backing.ConfigMap)
				require.NotNil(t, backing.EmptyDir, "%s backs /dev/shm with an emptyDir", pod.Name)
				require.Equal(t, core.StorageMediumMemory, backing.EmptyDir.Medium, "%s backs shared memory in RAM", pod.Name)
				require.NotNil(t, backing.EmptyDir.SizeLimit)
				require.Zero(t, backing.EmptyDir.SizeLimit.Cmp(resource.MustParse(tc.wantEmptyDir)),
					"%s declares %s, not %s", pod.Name, backing.EmptyDir.SizeLimit, tc.wantEmptyDir)
			}
			if !tc.ordinary {
				require.Equal(t, 2, members, "the master and its worker each carry their own shared memory")
			}
		})
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
