package worker

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/elasticprofile"
)

func TestElasticRayLoggingEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ordinary bool
		env      []workercore.ModelDeploymentEnvVar
		want     map[string]string
	}{
		{
			name: "defaults",
			want: map[string]string{"RAY_LOG_TO_STDERR": "1", "RAY_LOGGER_LEVEL": "info", "RAY_BACKEND_LOG_LEVEL": "info"},
		},
		{
			name: "debug overrides",
			env: []workercore.ModelDeploymentEnvVar{
				{Name: "RAY_LOGGER_LEVEL", Value: "debug"},
				{Name: "RAY_BACKEND_LOG_LEVEL", Value: "debug"},
				{Name: "RAY_DEDUP_LOGS", Value: "0"},
			},
			want: map[string]string{"RAY_LOG_TO_STDERR": "1", "RAY_LOGGER_LEVEL": "debug", "RAY_BACKEND_LOG_LEVEL": "debug", "RAY_DEDUP_LOGS": "0"},
		},
		{
			name: "file logging override",
			env:  []workercore.ModelDeploymentEnvVar{{Name: "RAY_LOG_TO_STDERR", Value: "0"}},
			want: map[string]string{"RAY_LOG_TO_STDERR": "0", "RAY_LOGGER_LEVEL": "info", "RAY_BACKEND_LOG_LEVEL": "info"},
		},
		{name: "ordinary deployment", ordinary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment()
			md.Spec.KVCache = nil
			md.Spec.Roles[0].Env = append(slices.Clone(tc.env), workercore.ModelDeploymentEnvVar{Name: "VLLM_LOGGING_LEVEL", Value: "DEBUG"})
			if !tc.ordinary {
				md.Spec.Engine.Version = "0.29.0"
				md.Spec.Roles[0].Replicas = 1
				md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2, HeadInstanceType: "cpu"}
			}
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
			wantPods := 3
			if tc.ordinary {
				wantPods = 2
			}
			require.Len(t, pods.Items, wantPods)
			for _, pod := range pods.Items {
				env := map[string]string{}
				for _, value := range pod.Spec.Containers[0].Env {
					require.NotContains(t, env, value.Name, "duplicate env on %s", pod.Name)
					env[value.Name] = value.Value
				}
				if tc.ordinary {
					for _, name := range []string{"RAY_LOG_TO_STDERR", "RAY_LOGGER_LEVEL", "RAY_BACKEND_LOG_LEVEL", "RAY_DEDUP_LOGS"} {
						require.NotContains(t, env, name)
					}
				} else {
					for name, value := range tc.want {
						require.Equal(t, value, env[name], "%s on %s", name, pod.Name)
					}
				}
				if modelDeploymentPodRole(&pod) == modelDeploymentElasticHeadName(md) {
					require.NotContains(t, env, "VLLM_LOGGING_LEVEL")
				} else {
					require.Equal(t, "DEBUG", env["VLLM_LOGGING_LEVEL"])
				}
			}
		})
	}
}

func TestElasticConvergenceCreatesChildren(t *testing.T) {
	for _, tp := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("tp-%d", tp), func(t *testing.T) {
			md := newRenderDeployment()
			md.Spec.KVCache = nil
			md.Spec.Engine.Version = "0.29.0"
			md.Spec.Roles[0].Replicas = 1
			md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: resource.NewQuantity(int64(tp), resource.DecimalSI)}
			md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2, HeadInstanceType: "cpu"}
			md.Spec.Roles[0].ExtraArgs = []string{"--tensor-parallel-size", strconv.Itoa(tp)}
			cpu := newRenderInstanceType(func(it *worker.InstanceType) {
				it.Name = "cpu"
				it.Spec.Acceleratable = false
				it.Status.Entrance = "cpu-queue"
			})
			cli := newModelDeploymentClient(md, newRenderInstanceType(), cpu)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
			gpuType := new(worker.InstanceType)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: md.Spec.Roles[0].InstanceType}, gpuType))
			unitCPU := resource.MustParse(gpuType.Spec.UnitResources.CPU)
			unitMemory := resource.MustParse(gpuType.Spec.UnitResources.RAM)
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
					require.Equal(t, int64(tp), q.Value())
					require.Contains(t, c.Command[2], fmt.Sprintf("--num-gpus=%d", tp))
					require.Equal(t, unitCPU.MilliValue()*int64(tp), c.Resources.Limits.Cpu().MilliValue())
					require.Equal(t, unitMemory.Value()*int64(tp), c.Resources.Limits.Memory().Value())
					require.Contains(t, c.Command[2], "gpustack-pod-uid=\"$GPUSTACK_ELASTIC_POD_UID\"")
					if modelDeploymentOrdinalOrFloor(pod) == 0 {
						master = *pod.DeepCopy()
						require.Contains(t, c.Command, "--enable-elastic-ep")
						tpArg := slices.Index(c.Command, "--tensor-parallel-size")
						require.GreaterOrEqual(t, tpArg, 0)
						require.Equal(t, strconv.Itoa(tp), c.Command[tpArg+1])
						require.Equal(t, 1, strings.Count(strings.Join(c.Command, " "), "--tensor-parallel-size"))
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
		})
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

func TestModelDeploymentRolesSharedMemory(t *testing.T) {
	for _, tc := range []struct {
		name         string
		additional   []workercore.ModelDeploymentAdditionalVolume
		ordinary     bool
		engine       string
		shmSize      string
		takeOver     bool
		wantEmptyDir string
	}{
		{name: "elastic default", wantEmptyDir: "16Gi"},
		{name: "elastic custom", shmSize: "32Gi", wantEmptyDir: "32Gi"},
		{name: "ordinary vllm default", ordinary: true, wantEmptyDir: "16Gi"},
		{name: "ordinary sglang default", ordinary: true, engine: workercore.ModelDeploymentEngineSGLang, wantEmptyDir: "16Gi"},
		{name: "ordinary custom", ordinary: true, shmSize: "32Gi", wantEmptyDir: "32Gi"},
		{name: "take over default", ordinary: true, takeOver: true, wantEmptyDir: "16Gi"},
		{
			name:    "explicit mount overrides custom size",
			shmSize: "32Gi",
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
			if tc.engine != "" {
				md.Spec.Engine.Name = tc.engine
			}
			if tc.shmSize != "" {
				md.Spec.Roles[0].ShmSize = ptr.To(resource.MustParse(tc.shmSize))
			}
			if tc.takeOver {
				md.Spec.Roles[0].Command = []string{"custom-server"}
			}
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
				if modelDeploymentPodRole(pod) != modelDeploymentElasticHeadName(md) {
					members++
				}
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

func TestElasticSharedMemoryEditRetainsExistingMembers(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	pods := new(core.PodList)
	require.NoError(t, f.reconciler.Client.List(ctx, pods))
	original := make(map[string]types.UID, len(pods.Items))
	for _, pod := range pods.Items {
		original[pod.Name] = pod.UID
	}
	require.Len(t, original, 3)
	assertRetained := func() {
		t.Helper()
		for name, uid := range original {
			index := slices.IndexFunc(pods.Items, func(pod core.Pod) bool { return pod.Name == name })
			require.NotEqual(t, -1, index, "original Pod %s must remain", name)
			pod := &pods.Items[index]
			require.Equal(t, uid, pod.UID, name)
			for _, volume := range pod.Spec.Volumes {
				if volume.EmptyDir != nil && volume.EmptyDir.Medium == core.StorageMediumMemory {
					require.NotNil(t, volume.EmptyDir.SizeLimit)
					require.Zero(t, volume.EmptyDir.SizeLimit.Cmp(resource.MustParse("16Gi")), name)
				}
			}
		}
	}

	f.md.Spec.Roles[0].ShmSize = ptr.To(resource.MustParse("32Gi"))
	_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.NoError(t, f.reconciler.Client.List(ctx, pods))
	require.Len(t, pods.Items, 3)
	assertRetained()

	f.md.Spec.Roles[0].ElasticEP.Width = 3
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.NoError(t, f.reconciler.Client.List(ctx, pods))
	require.Len(t, pods.Items, 4)
	assertRetained()
	newMembers := 0
	for _, pod := range pods.Items {
		want := "32Gi"
		if uid, exists := original[pod.Name]; exists {
			require.Equal(t, uid, pod.UID)
			want = "16Gi"
		} else {
			newMembers++
		}
		found := false
		for _, volume := range pod.Spec.Volumes {
			if volume.EmptyDir != nil && volume.EmptyDir.Medium == core.StorageMediumMemory {
				found = true
				require.NotNil(t, volume.EmptyDir.SizeLimit)
				require.Zero(t, volume.EmptyDir.SizeLimit.Cmp(resource.MustParse(want)), pod.Name)
			}
		}
		require.True(t, found, pod.Name)
	}
	require.Equal(t, 1, newMembers)
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

// TestModelDeploymentElasticBootstrapFromMasterWidthBounds is the rendering half of the elastic
// profile contract: a bootstrap width the profile declares is retained, and a width on either
// side of that range holds reconciliation.
//
// THE RANGE IS ASKED FOR, NEVER RESTATED. The widths below come from the shared profile, so a
// bound written into the renderer instead of read from it is what this test catches: it would
// retain or hold one of these widths the other way round.
func TestModelDeploymentElasticBootstrapFromMasterWidthBounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		width int32
		want  bool
	}{
		{name: "the narrowest profile width", width: elasticprofile.WidthMin, want: true},
		{name: "the widest profile width", width: elasticprofile.WidthMax, want: true},
		{name: "one below the profile", width: elasticprofile.WidthMin - 1},
		{name: "one above the profile", width: elasticprofile.WidthMax + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{
					Width: tc.width, HeadInstanceType: "cpu",
				}
			})

			// A new master takes the width from the request; a live one takes it from the
			// record it wrote. Both are the renderer's own answer, so both are held to the
			// shared profile.
			retained, ok := ModelDeploymentElasticBootstrapFromMaster("", "", md)
			require.Equal(t, tc.want, ok, "a new master at width %d", tc.width)
			require.Equal(t, tc.want, retained == tc.width, "retained width %d", retained)

			recorded := strconv.FormatInt(int64(tc.width), 10)
			_, ok = ModelDeploymentElasticBootstrapFromMaster("master-uid", recorded, md)
			require.Equal(t, tc.want, ok, "a live master recording width %d", tc.width)
		})
	}
}
