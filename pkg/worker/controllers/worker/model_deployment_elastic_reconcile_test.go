package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlmeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clitesting "k8s.io/client-go/testing"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workerapi "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/elasticengine"
)

func TestElasticRecordDeletionChecksOwnerAndVersion(t *testing.T) {
	for _, change := range []string{"owner", "version"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			cli := elasticTestClient()
			store := newElasticOperationStore(cli)
			op := elasticTestOperation(elasticWidth{Old: 2, Target: 4})
			_, err := store.Create(ctx, op)
			require.NoError(t, err)
			cm := new(core.ConfigMap)
			require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: op.Namespace, Name: elasticOperationRecordName(op.DeploymentUID)}, cm))
			if change == "owner" {
				cm.OwnerReferences[0].UID = "foreign-owner"
			} else {
				cm.Data["other"] = "concurrent-write"
			}
			require.NoError(t, cli.Update(ctx, cm))
			require.Error(t, store.Delete(ctx, op), "concurrent record must survive")
			require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(cm), new(core.ConfigMap)))
		})
	}
}

// The fixture supplies transport bytes and live objects. The production observer,
// native client, operation store and convergence entry point derive every result.
type elasticConvergenceFixture struct {
	md                *workercore.ModelDeployment
	reconciler        *ModelDeploymentReconciler
	nativeWidth       atomic.Int32
	scaleCalls        atomic.Int32
	forwardCalls      atomic.Int32
	rayMalformed      atomic.Bool
	forwardsFail      atomic.Bool
	scaling           atomic.Bool
	scaleStatus       atomic.Int32
	intentObserved    atomic.Bool
	commitBeforeError atomic.Bool
}

func newElasticConvergenceFixture(t *testing.T) *elasticConvergenceFixture {
	t.Helper()
	md := newRenderDeployment()
	md.Generation = 1
	md.Spec.KVCache = nil
	md.Spec.Engine.Version = "0.29.0"
	md.Spec.Roles[0].Replicas = 1
	md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: resource.NewQuantity(1, resource.DecimalSI)}
	md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 2, HeadInstanceType: "cpu"}
	cpu := newRenderInstanceType(func(it *workerapi.InstanceType) {
		it.Name = "cpu"
		it.Spec.Acceleratable = false
		it.Status.Entrance = "cpu-queue"
	})
	md.ResourceVersion = "1"
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjectTracker(clitesting.NewObjectTracker(scheme.Scheme, scheme.Codecs.UniversalDecoder())).
		WithObjects(md, newRenderInstanceType(), cpu).
		WithStatusSubresource(&workercore.ModelDeployment{}, &workercore.Devices{}).
		WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string { return []string{obj.(*core.Pod).Spec.NodeName} }).
		WithIndex(&core.Pod{}, modelDeploymentDrainIndexPodUID, func(obj ctrlcli.Object) []string { return []string{string(obj.GetUID())} }).Build()
	f := &elasticConvergenceFixture{md: md, reconciler: &ModelDeploymentReconciler{Client: cli, APIReader: cli}}
	f.nativeWidth.Store(2)
	f.scaleStatus.Store(500)
	_, err := f.reconciler.convergeModelDeployment(context.Background(), md)
	require.NoError(t, err)
	f.startMembers(t)
	observer := newModelDeploymentElasticObserver(cli, cli, nil, nil)
	observer.exec = func(ctx context.Context, head *core.Pod, container string, argv []string) (string, error) {
		if f.rayMalformed.Load() {
			return `{}`, nil
		}
		pods, err := f.reconciler.elasticLiveMembers(ctx, md)
		if err != nil {
			return "", err
		}
		nodes := []elasticRayNodeWire{}
		actors := []elasticRayActorWire{}
		groups := []elasticRayPGWire{}
		for i := range pods {
			pod := &pods[i]
			id := fmt.Sprintf("%02x", i+1)
			node := observerTestNode(id, true, map[string]string{modelDeploymentElasticPodUIDLabelKey: string(pod.UID)})
			if modelDeploymentPodRole(pod) != modelDeploymentElasticHeadName(md) {
				node.Resources["GPU"] = "1"
				ordinal, valid := modelDeploymentPodOrdinal(pod)
				if !valid {
					return "", fmt.Errorf("fixture member has no ordinal")
				}
				if ordinal < int(f.nativeWidth.Load()) {
					pg := fmt.Sprintf("b%02x", ordinal)
					groups = append(groups, observerTestPG(pg, fmt.Sprintf("dp_rank_%d", ordinal), elasticRayPGCreated))
					actors = append(actors, observerTestActor(fmt.Sprintf("a%02x", ordinal), elasticRayActorAlive, modelDeploymentElasticActorClassEngineCore, id, pg))
				}
			}
			nodes = append(nodes, node)
		}
		return string(observerTestDocumentJSON(t, nodes, actors, groups)), nil
	}
	f.reconciler.elasticObserver = observer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1/completions":
			if req.Header.Get("X-data-parallel-rank") == "-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":{"message":"data_parallel_rank -1 is out of range [0, %d).","type":"BadRequestError","param":null,"code":400}}`, f.nativeWidth.Load())
				return
			}
			f.forwardCalls.Add(1)
			if f.forwardsFail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"text_completion","choices":[{"index":0,"text":" "}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		case "/is_scaling_elastic_ep":
			_, _ = fmt.Fprintf(w, `{"is_scaling_elastic_ep":%t}`, f.scaling.Load())
		case "/scale_elastic_ep":
			f.scaleCalls.Add(1)
			var payload struct {
				Target int `json:"new_data_parallel_size"`
			}
			if json.NewDecoder(req.Body).Decode(&payload) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			op, err := newElasticOperationStore(cli).Read(req.Context(), md.Namespace, md.Name, md.UID)
			f.intentObserved.Store(err == nil && op.State == elasticStateCommandSent && op.CommandSent && op.Width.Target == payload.Target && op.Generation == md.Generation)
			code := int(f.scaleStatus.Load())
			if code == 200 || f.commitBeforeError.Load() {
				f.nativeWidth.Store(int32(payload.Target))
			}
			w.WriteHeader(code)
			if code == 200 {
				_, _ = w.Write([]byte(`{"message":"accepted"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	f.reconciler.elasticClient = func(*core.Pod) (*elasticengine.Client, error) { return elasticengine.New(server.URL, time.Second) }
	return f
}

func (f *elasticConvergenceFixture) startMembers(t *testing.T) {
	t.Helper()
	cli := f.reconciler.Client
	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace(f.md.Namespace)))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.UID != "" {
			continue
		}
		pod.UID = types.UID("runtime-" + pod.Name)
		require.NoError(t, cli.Update(context.Background(), pod))
		pod.Status.PodIP = fmt.Sprintf("10.0.0.%d", i+1)
		pod.Status.ContainerStatuses = []core.ContainerStatus{{Name: modelDeploymentMainContainerName, ContainerID: "container-" + string(pod.UID), State: core.ContainerState{Running: &core.ContainerStateRunning{}}}}
		require.NoError(t, cli.Status().Update(context.Background(), pod))
	}
}

func (f *elasticConvergenceFixture) observation(t *testing.T) modelDeploymentElasticStatus {
	t.Helper()
	cm := new(core.ConfigMap)
	require.NoError(t, f.reconciler.Client.Get(context.Background(), ctrlcli.ObjectKey{Namespace: f.md.Namespace, Name: modelDeploymentElasticObservationName(f.md)}, cm))
	var status modelDeploymentElasticStatus
	require.NoError(t, json.Unmarshal([]byte(cm.Data["observation.json"]), &status))
	return status
}

func TestElasticConvergenceObservesNativeWorld(t *testing.T) {
	cases := []struct {
		name                        string
		malformedRay, failedForward bool
		effective                   bool
	}{
		{name: "native ranks and every forward agree", effective: true},
		{name: "malformed Ray document holds", malformedRay: true},
		{name: "unserved native rank holds", failedForward: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newElasticConvergenceFixture(t)
			f.rayMalformed.Store(tc.malformedRay)
			f.forwardsFail.Store(tc.failedForward)
			_, err := f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			status := f.observation(t)
			require.Equal(t, tc.effective, status.Effective.Known, status.Reason)
			if tc.effective {
				require.Equal(t, 2, status.Effective.Value)
				require.GreaterOrEqual(t, f.forwardCalls.Load(), int32(2))
			}
			require.Zero(t, f.scaleCalls.Load())
			pods := new(core.PodList)
			require.NoError(t, f.reconciler.Client.List(context.Background(), pods))
			require.Len(t, pods.Items, 3)
		})
	}
}

func TestElasticConvergenceResumesSentRecordWithoutReplay(t *testing.T) {
	for _, state := range []elasticOperationState{elasticStateCommandSent, elasticStateAwaitingWidth, elasticStateNativeDone} {
		t.Run(string(state), func(t *testing.T) {
			f := newElasticConvergenceFixture(t)
			f.md.Spec.Roles[0].ElasticEP.Width = 4
			f.md.Generation++
			require.NoError(t, f.reconciler.Client.Update(context.Background(), f.md))
			_, err := f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			f.startMembers(t)
			pods, err := f.reconciler.elasticLiveMembers(context.Background(), f.md)
			require.NoError(t, err)
			op := &elasticOperation{Name: f.md.Name, Namespace: f.md.Namespace, DeploymentUID: f.md.UID, Generation: f.md.Generation, Width: elasticWidth{Old: 2, Target: 4}, State: state, CommandSent: true, CommandIntent: "scale to 4"}
			for i := range pods {
				pod := &pods[i]
				id := elasticCapturedIdentity{Name: pod.Name, UID: pod.UID}
				if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(f.md) {
					op.Head = id
				} else {
					op.Members = append(op.Members, id)
					if modelDeploymentOrdinalOrFloor(pod) == 0 {
						op.Master = id
					}
				}
			}
			_, err = newElasticOperationStore(f.reconciler.Client).Create(context.Background(), op)
			require.NoError(t, err)
			for range 2 {
				_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
				require.NoError(t, err)
			}
			require.Zero(t, f.scaleCalls.Load(), "a sent record cannot repeat an ambiguous mutation")
			f.nativeWidth.Store(4)
			_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			status := f.observation(t)
			require.True(t, status.Effective.Known, status.Reason)
			require.Equal(t, 4, status.StableWidth)
			require.Equal(t, elasticStateCompleted, status.State)
			after, err := f.reconciler.elasticLiveMembers(context.Background(), f.md)
			require.NoError(t, err)
			require.True(t, slices.EqualFunc(pods, after, func(a, b core.Pod) bool { return a.UID == b.UID && a.Name == b.Name }))
			require.Zero(t, f.scaleCalls.Load())
		})
	}
}

func (f *elasticConvergenceFixture) admitMembers(t *testing.T) {
	t.Helper()
	cli := f.reconciler.Client
	pods := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), pods, ctrlcli.InNamespace(f.md.Namespace)))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if modelDeploymentPodRole(pod) != "server" || pod.Spec.NodeName != "" {
			continue
		}
		ordinal, valid := modelDeploymentPodOrdinal(pod)
		require.True(t, valid)
		pod.Spec.NodeName = fmt.Sprintf("node-%d", ordinal)
		devices := releaseDevices()
		devices.Name = pod.Spec.NodeName
		devices.UID = types.UID("devices-" + devices.Name)
		devices.Spec.Groups[0].Accelerators[0].ID = "GPU-" + devices.Name
		allocation := releaseCardRecord()["vllm"]
		allocation.Devices.Groups[0].Accelerators[0].ID = devices.Spec.Groups[0].Accelerators[0].ID
		raw, err := json.Marshal(deviceplugin.PodAllocations{modelDeploymentMainContainerName: allocation})
		require.NoError(t, err)
		pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = string(raw)
		require.NoError(t, cli.Update(context.Background(), pod))
		status, _, err := deviceplugin.BuildDesiredStatusStrict(ctrllogDiscard(), devices, &core.PodList{Items: []core.Pod{*pod}})
		require.NoError(t, err)
		devices.Status = status
		require.NoError(t, cli.Create(context.Background(), devices))
		workload := allocationWorkload("wl-"+pod.Name, "wl-"+string(pod.UID), pod.Name, string(pod.UID))
		workload.Spec.PodSets = []kueue.PodSet{{Name: "server", Count: 1, Template: core.PodTemplateSpec{ObjectMeta: ctrlmeta.ObjectMeta{Labels: pod.Labels}, Spec: *pod.Spec.DeepCopy()}}}
		workload.Status.Admission.ClusterQueue = "gpu-queue"
		workload.Status.Admission.PodSetAssignments[0].Count = &[]int32{1}[0]
		workload.Status.Admission.PodSetAssignments[0].ResourceUsage = pod.Spec.Containers[0].Resources.Requests.DeepCopy()
		require.NoError(t, cli.Create(context.Background(), workload))
	}
}

func (f *elasticConvergenceFixture) publishLedgers(t *testing.T) {
	t.Helper()
	devices := new(workercore.DevicesList)
	require.NoError(t, f.reconciler.Client.List(context.Background(), devices))
	for i := range devices.Items {
		dev := &devices.Items[i]
		pods := new(core.PodList)
		require.NoError(t, f.reconciler.Client.List(context.Background(), pods, ctrlcli.MatchingFields{"spec.nodeName": dev.Name}))
		status, _, err := deviceplugin.BuildDesiredStatusStrict(ctrllogDiscard(), dev, pods)
		require.NoError(t, err)
		dev.Status = status
		require.NoError(t, f.reconciler.Client.Status().Update(context.Background(), dev))
	}
}

func TestElasticConvergenceRecordsOneNativeMutation(t *testing.T) {
	for _, code := range []int32{200, 408, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newElasticConvergenceFixture(t)
			f.scaleStatus.Store(code)
			f.commitBeforeError.Store(code != 200)
			f.md.Spec.Roles[0].ElasticEP.Width = 4
			f.md.Generation++
			require.NoError(t, f.reconciler.Client.Update(context.Background(), f.md))
			_, err := f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			require.Zero(t, f.scaleCalls.Load(), "unallocated members cannot authorize a request")
			f.startMembers(t)
			f.admitMembers(t)
			_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			require.Equal(t, int32(1), f.scaleCalls.Load(), f.observation(t).Reason)
			require.True(t, f.intentObserved.Load(), "record must be committed before HTTP mutation")
			_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			require.Equal(t, 4, f.observation(t).StableWidth)
			require.Equal(t, int32(1), f.scaleCalls.Load())
		})
	}
}

func TestElasticConvergenceCompletesTwoFourTwo(t *testing.T) {
	for _, trial := range []struct {
		name            string
		generationDrift bool
	}{
		{name: "unchanged generation"},
		{name: "generation drift before accelerator return", generationDrift: true},
	} {
		t.Run(trial.name, func(t *testing.T) {
			f := newElasticConvergenceFixture(t)
			f.scaleStatus.Store(200)
			f.admitMembers(t)
			masterBefore := new(core.Pod)
			initial, err := f.reconciler.elasticLiveMembers(context.Background(), f.md)
			require.NoError(t, err)
			for _, pod := range initial {
				if modelDeploymentPodRole(&pod) == "server" && modelDeploymentOrdinalOrFloor(&pod) == 0 {
					masterBefore = pod.DeepCopy()
				}
			}
			require.NotEmpty(t, masterBefore.UID)
			f.md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}
			f.md.Spec.Roles[0].ElasticEP.Width = 4
			f.md.Generation++
			require.NoError(t, f.reconciler.Client.Update(context.Background(), f.md))
			_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			f.startMembers(t)
			f.admitMembers(t)
			for range 2 {
				_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
				require.NoError(t, err)
			}
			require.Equal(t, 4, f.observation(t).StableWidth, f.observation(t).Reason)
			require.Equal(t, int32(1), f.scaleCalls.Load())
			for _, obj := range ownedRouterObjects(f.md) {
				if wanted, ok := obj.(*apps.Deployment); ok {
					actual := new(apps.Deployment)
					require.NoError(t, f.reconciler.Client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(wanted), actual))
					actual.UID = wanted.UID
					require.NoError(t, f.reconciler.Client.Update(context.Background(), actual))
					continue
				}
				require.NoError(t, f.reconciler.Client.Create(context.Background(), obj))
			}
			f.reconciler.servingViewFetch = func(context.Context, string) ([]byte, error) { return retirementRouterView(nil, nil), nil }
			collector := newModelDeploymentDrainCollector(f.reconciler.Client, f.reconciler.APIReader, nil, nil)
			collector.exec = func(context.Context, *core.Pod, string, []string) (string, error) { return vllmZero(), nil }
			f.reconciler.drainReader = collector
			f.md.Spec.Roles[0].ElasticEP.Width = 2
			f.md.Generation++
			require.NoError(t, f.reconciler.Client.Update(context.Background(), f.md))
			// Request then observe actor retirement. Published allocation still retains both cards.
			for range 2 {
				_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
				require.NoError(t, err)
			}
			require.Equal(t, int32(2), f.scaleCalls.Load(), f.observation(t).Reason)
			current, err := f.reconciler.elasticLiveMembers(context.Background(), f.md)
			require.NoError(t, err)
			require.Len(t, current, 3, "actor-free captured workers should be deleted; master and CPU head survive")
			require.Equal(t, 4, f.observation(t).StableWidth, "deletion is not a confirmed allocation return")
			if trial.generationDrift {
				f.md.Generation++
				require.NoError(t, f.reconciler.Client.Update(context.Background(), f.md))
				_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
				require.NoError(t, err)
				require.Equal(t, int32(2), f.scaleCalls.Load())
				_, err = newElasticOperationStore(f.reconciler.Client).Read(
					context.Background(), f.md.Namespace, f.md.Name, f.md.UID)
				require.NoError(t, err, "unreturned accelerator accounting retains the old intent")
				require.Equal(t, 4, f.observation(t).StableWidth)
			}
			f.publishLedgers(t)
			_, err = f.reconciler.convergeModelDeployment(context.Background(), f.md)
			require.NoError(t, err)
			require.Equal(t, 2, f.observation(t).StableWidth, f.observation(t).Reason)
			require.Equal(t, elasticStateCompleted, f.observation(t).State)
			masterAfter := new(core.Pod)
			require.NoError(t, f.reconciler.Client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(masterBefore), masterAfter))
			require.Equal(t, masterBefore.UID, masterAfter.UID)
			require.Equal(t, masterBefore.Spec, masterAfter.Spec)
			require.Equal(t, "2", masterAfter.Annotations[ModelDeploymentElasticMasterBootWidthAnnotation])
			require.Equal(t, int32(2), f.scaleCalls.Load())
		})
	}
}

func TestElasticDeletionRetainsUnprovenCapturedMember(t *testing.T) {
	for _, failure := range []string{"unknown actor", "held actor", "replacement", "allocation changed", "generation changed", "late update"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newElasticConvergenceFixture(t)
			f.admitMembers(t)
			pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
			require.NoError(t, err)
			var target *core.Pod
			for i := range pods {
				if modelDeploymentPodRole(&pods[i]) == "server" && modelDeploymentOrdinalOrFloor(&pods[i]) == 1 {
					target = &pods[i]
				}
			}
			require.NotNil(t, target)
			release, err := f.reconciler.captureModelDeploymentElasticRelease(ctx, f.md, []core.Pod{*target})
			require.NoError(t, err)
			op := &elasticOperation{
				DeploymentUID: f.md.UID, Generation: f.md.Generation,
				Workers: []elasticCapturedIdentity{{Name: target.Name, UID: target.UID}}, Release: release,
			}
			ray := modelDeploymentElasticObservation{ActorFree: map[types.UID]modelDeploymentElasticActorFreeFact{
				target.UID: {ActorFree: true},
			}}
			switch failure {
			case "unknown actor":
				delete(ray.ActorFree, target.UID)
			case "held actor":
				ray.ActorFree[target.UID] = modelDeploymentElasticActorFreeFact{}
			case "replacement":
				require.NoError(t, f.reconciler.Client.Delete(ctx, target))
				target.UID = "replacement-uid"
				target.ResourceVersion = ""
				require.NoError(t, f.reconciler.Client.Create(ctx, target))
			case "allocation changed":
				target.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = "{}"
				require.NoError(t, f.reconciler.Client.Update(ctx, target))
			case "generation changed":
				f.md.Generation++
				require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
			case "late update":
				f.reconciler.Client = ctrlinterceptor.NewClient(f.reconciler.Client.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
					Delete: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.DeleteOption) error {
						options := new(ctrlcli.DeleteOptions).ApplyOptions(opts)
						require.NotNil(t, options.Preconditions)
						require.Equal(t, target.UID, *options.Preconditions.UID)
						require.Equal(t, obj.GetResourceVersion(), *options.Preconditions.ResourceVersion)
						fresh := new(core.Pod)
						require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(obj), fresh))
						fresh.Labels["concurrent-update"] = "true"
						require.NoError(t, cli.Update(ctx, fresh))
						return cli.Delete(ctx, obj, opts...)
					},
				})
			}
			err = f.reconciler.elasticDeleteCaptured(ctx, f.md, op, ray)
			require.Error(t, err)
			retained := new(core.Pod)
			require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(target), retained))
			require.Equal(t, target.UID, retained.UID)
			workloads := new(kueue.WorkloadList)
			require.NoError(t, f.reconciler.Client.List(ctx, workloads))
			require.Len(t, workloads.Items, 2, "retirement cannot delete quota after an unproven Pod deletion")
		})
	}
}

func TestElasticConvergenceHoldsUnresolvedOperation(t *testing.T) {
	for _, failure := range []string{"old world unknown", "generation changed", "generation target changed"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newElasticConvergenceFixture(t)
			f.md.Spec.Roles[0].ElasticEP.Width = 4
			f.md.Generation++
			require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
			_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)
			f.startMembers(t)
			f.admitMembers(t)
			pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
			require.NoError(t, err)
			op := &elasticOperation{
				Name: f.md.Name, Namespace: f.md.Namespace, DeploymentUID: f.md.UID,
				Generation: f.md.Generation, Width: elasticWidth{Old: 2, Target: 4}, State: elasticStateRecorded,
			}
			for i := range pods {
				pod := &pods[i]
				id := elasticCapturedIdentity{Name: pod.Name, UID: pod.UID}
				if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(f.md) {
					op.Head = id
				} else {
					op.Members = append(op.Members, id)
					if modelDeploymentOrdinalOrFloor(pod) == 0 {
						op.Master = id
					}
				}
			}
			_, err = newElasticOperationStore(f.reconciler.Client).Create(ctx, op)
			require.NoError(t, err)
			if failure != "old world unknown" {
				if failure == "generation target changed" {
					f.md.Spec.Roles[0].ElasticEP.Width = 2
				}
				f.md.Generation++
				require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
			} else {
				f.forwardsFail.Store(true)
			}
			_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)
			require.Zero(t, f.scaleCalls.Load())
			retained, err := f.reconciler.elasticLiveMembers(ctx, f.md)
			require.NoError(t, err)
			require.Len(t, retained, 5)
			if failure == "generation changed" {
				// The old world IS current, so the pass observes it against the current
				// generation instead of reporting an unreadable engine, and the unsent record
				// retires on that proof. Nothing was dispatched and nothing was deleted.
				require.True(t, f.observation(t).Effective.Known)
				require.Equal(t, 2, f.observation(t).StableWidth)
				_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
					ctx, f.md.Namespace, f.md.Name, f.md.UID)
				require.True(t, apierrors.IsNotFound(readErr))
				return
			}
			if failure == "generation target changed" {
				require.True(t, f.observation(t).Effective.Known)
			} else {
				require.False(t, f.observation(t).Effective.Known)
			}
			_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
				ctx, f.md.Namespace, f.md.Name, f.md.UID)
			require.NoError(t, readErr)
		})
	}
}

// TestElasticGenerationDriftReconstructs covers the pass that re-observes a record bound to an older
// generation. Every case declares one current-world condition and asserts the record's fate from it.
// No case may dispatch: a stale record is never a reason to send the current target.
func TestElasticGenerationDriftReconstructs(t *testing.T) {
	for _, failure := range []struct {
		name          string
		failedForward bool
		scaling       bool
		native        int32
		identity      bool
		retire        bool
		desiredOld    bool
	}{
		{name: "sent upward whose target is proven retires", native: 4, retire: true},
		{name: "sent upward with a failed forward is held", native: 4, failedForward: true},
		{name: "sent upward still scaling is held", native: 4, scaling: true},
		{name: "sent upward at its old width is held", native: 2},
		{name: "sent upward at its old width recovers when desired returns", native: 2, desiredOld: true},
		{name: "sent upward with drifted identity is held", native: 4, identity: true},
	} {
		t.Run(failure.name, func(t *testing.T) {
			ctx := context.Background()
			f := newElasticConvergenceFixture(t)
			f.md.Spec.Roles[0].ElasticEP.Width = 4
			require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
			_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)
			f.startMembers(t)
			f.admitMembers(t)

			pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
			require.NoError(t, err)
			op := &elasticOperation{
				Name: f.md.Name, Namespace: f.md.Namespace, DeploymentUID: f.md.UID,
				Generation: f.md.Generation, Width: elasticWidth{Old: 2, Target: 4},
				State: elasticStateCommandSent, CommandSent: true, CommandIntent: "scale to 4",
			}
			for i := range pods {
				pod := &pods[i]
				id := elasticCapturedIdentity{Name: pod.Name, UID: pod.UID}
				if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(f.md) {
					op.Head = id
				} else {
					op.Members = append(op.Members, id)
					if modelDeploymentOrdinalOrFloor(pod) == 0 {
						op.Master = id
					}
				}
			}
			if failure.identity {
				// A same-name replacement under a fresh UID is a different worker.
				op.Members[1].UID = types.UID("00000000-0000-0000-0000-000000000000")
			}
			_, err = newElasticOperationStore(f.reconciler.Client).Create(ctx, op)
			require.NoError(t, err)

			// The commit response was lost, so the engine is already at the target, and the
			// operator has since edited an unrelated field.
			f.nativeWidth.Store(failure.native)
			if failure.scaling {
				f.scaling.Store(true)
			}
			if failure.failedForward {
				f.forwardsFail.Store(true)
			}
			if failure.desiredOld {
				f.md.Spec.Roles[0].ElasticEP.Width = 2
			}
			f.md.Generation++
			require.NoError(t, f.reconciler.Client.Update(ctx, f.md))

			before, err := f.reconciler.elasticLiveMembers(ctx, f.md)
			require.NoError(t, err)
			_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)

			if failure.desiredOld {
				recovered, readErr := newElasticOperationStore(f.reconciler.Client).Read(
					ctx, f.md.Namespace, f.md.Name, f.md.UID)
				require.NoError(t, readErr)
				require.Equal(t, elasticStateReleased, recovered.State)
				require.Equal(t, elasticWidth{Old: 4, Target: 2}, recovered.Width)
				require.Len(t, recovered.Workers, 2)
				require.Equal(t, 2, f.observation(t).StableWidth)
				require.Contains(t, f.observation(t).Reason, "recovered")
				return
			}

			// Nothing is dispatched, in this pass or in a following pass at the same target.
			require.Zero(t, f.scaleCalls.Load(), f.observation(t).Reason)
			_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
			require.NoError(t, err)
			require.Zero(t, f.scaleCalls.Load(), f.observation(t).Reason)

			// Every member and the head survive: this pass creates and deletes no capacity.
			after, err := f.reconciler.elasticLiveMembers(ctx, f.md)
			require.NoError(t, err)
			require.Len(t, after, len(before))

			_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
				ctx, f.md.Namespace, f.md.Name, f.md.UID)
			if failure.retire {
				observed := f.observation(t)
				require.True(t, apierrors.IsNotFound(readErr), observed.Reason,
					"reason=%q state=%q effective=%+v ray=%+v admitted=%+v allocated=%+v stable=%d",
					observed.Reason, observed.State, observed.Effective, observed.Ray,
					observed.Admitted, observed.Allocated, observed.StableWidth)
				require.Equal(t, 4, f.observation(t).StableWidth)
				return
			}
			// The record and every unit of capacity survive; the last proved stable width is
			// independent of this record and may legitimately still name the old world.
			require.NoError(t, readErr)
		})
	}
}

// elasticWatchedReader records the options every List reached the API server with and can be made
// to fail one, so a test can see what a reconcile-hot-path read asked for instead of inferring it
// from the objects it happened to return.
type elasticWatchedReader struct {
	ctrlcli.Reader
	listOptions []ctrlcli.ListOptions
	failList    error
	// failAfter lets a test fail one member read in a pass rather than every read in it, which is
	// what it takes to reach the read that convergence makes after its own observations are done.
	failAfter int
}

func (r *elasticWatchedReader) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	r.listOptions = append(r.listOptions, *new(ctrlcli.ListOptions).ApplyOptions(opts))
	if r.failList != nil && len(r.listOptions) > r.failAfter {
		return r.failList
	}
	return r.Reader.List(ctx, list, opts...)
}

// TestTheMemberListIsScopedToItsDeployment pins what the member read asks the API server for. A
// list narrowed to the deployment's own identity labels costs the same on a busy namespace as on
// an empty one, which is what lets this read sit in a path that runs several times per 15s
// requeue.
func TestTheMemberListIsScopedToItsDeployment(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	watched := &elasticWatchedReader{Reader: f.reconciler.APIReader}
	f.reconciler.APIReader = watched

	pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	require.NotEmpty(t, pods, "the fixture must produce members for the scope to mean anything")

	scoped := false
	for _, options := range watched.listOptions {
		if options.LabelSelector == nil {
			continue
		}
		if instance, found := options.LabelSelector.RequiresExactMatch(
			modelDeploymentLabelKeyInstance,
		); found && instance == f.md.Name {
			scoped = true
		}
	}
	require.True(t, scoped,
		"the member read must be narrowed by this deployment's identity labels, not by its namespace")
}

// TestTheMemberListKeepsItsOwnerCheck is the control for the scope above. The labels are a
// server-side prefilter and cannot see the owner, so a Pod a same-name recreated deployment left
// behind is still excluded. Narrowing the list must not become admitting by label.
func TestTheMemberListKeepsItsOwnerCheck(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	members, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	require.NotEmpty(t, members)

	impostor := members[0].DeepCopy()
	impostor.Name = "impostor-" + impostor.Name
	impostor.UID = types.UID("impostor-uid")
	impostor.ResourceVersion = ""
	impostor.OwnerReferences[0].UID = types.UID("a-recreated-deployment")
	require.NoError(t, f.reconciler.Client.Create(ctx, impostor))

	after, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	require.Len(t, after, len(members), "a Pod owned by another deployment is not a member")
	for i := range after {
		require.NotEqual(t, "impostor-"+members[0].Name, after[i].Name)
	}
}

// TestAFailedMemberReadIsNamed pins that a read the pass could not complete is reported as itself.
// The hold is a real state, so the observation says which read failed instead of leaving the
// deployment without a reason to act on.
func TestAFailedMemberReadIsNamed(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.reconciler.APIReader = &elasticWatchedReader{
		Reader:   f.reconciler.APIReader,
		failList: fmt.Errorf("the member list is unavailable"),
	}

	err := f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md)
	require.NoError(t, err, "a reported read failure is not a failed reconcile")

	cm := new(core.ConfigMap)
	require.NoError(t, f.reconciler.Client.Get(ctx,
		ctrlcli.ObjectKey{Namespace: f.md.Namespace, Name: modelDeploymentElasticObservationName(f.md)}, cm),
		"a read that failed still owes the reader a reason")
	require.Contains(t, cm.Data["observation.json"], "the member list is unavailable")
}

// TestAMovedDeploymentIsNotAReadFailure is the control for the one above. A generation edit is not a
// failed read: the observation belongs to a world that no longer exists, so this pass leaves the
// previous one untouched and lets the spec watch requeue, rather than overwriting it with a failure
// the operator did not have.
func TestAMovedDeploymentIsNotAReadFailure(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	key := ctrlcli.ObjectKey{Namespace: f.md.Namespace, Name: modelDeploymentElasticObservationName(f.md)}
	before := new(core.ConfigMap)
	require.NoError(t, f.reconciler.Client.Get(ctx, key, before))

	live := new(workercore.ModelDeployment)
	require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(f.md), live))
	live.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, live))

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	after := new(core.ConfigMap)
	require.NoError(t, f.reconciler.Client.Get(ctx, key, after))
	require.Equal(t, before.Data, after.Data,
		"a moved generation must not overwrite the observation with a read failure")
}

// TestAFailedMemberReadRefusesTheEndpointSync pins the other half: the read that failed must not be
// papered over with the caller's list. Converging the Service selector, the master's endpoint label
// and the router state from a list this pass never confirmed is how a same-name Pod replacement
// mid-resize gets served traffic it should not.
//
// The reader fails only the pass's closing member read, the one convergence makes after its own
// observations have already been collected, so the refusal under test is that read and not an
// earlier one.
func TestAFailedMemberReadRefusesTheEndpointSync(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	before := new(core.ServiceList)
	require.NoError(t, f.reconciler.Client.List(ctx, before))

	// Every member the fixture renders is already present, so the pass converges nothing and goes
	// straight to reading the membership it is about to write eligibility from.
	actual, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	role := ModelDeploymentElasticRole(f.md)
	desired := map[string]map[int][]*core.Pod{}
	for i := range actual {
		if modelDeploymentPodRole(&actual[i]) != role.Name {
			continue
		}
		ordinal, valid := modelDeploymentPodOrdinal(&actual[i])
		require.True(t, valid)
		if desired[role.Name] == nil {
			desired[role.Name] = map[int][]*core.Pod{}
		}
		desired[role.Name][ordinal] = []*core.Pod{actual[i].DeepCopy()}
	}
	require.NotEmpty(t, desired)

	watched := &elasticWatchedReader{Reader: f.reconciler.APIReader, failAfter: 6}
	f.reconciler.APIReader = watched
	watched.failList = fmt.Errorf("the member list is unavailable")

	_, err = f.reconciler.convergeModelDeploymentElastic(ctx, f.md, actual, desired, nil)
	require.ErrorContains(t, err, "the member list is unavailable",
		"a pass that could not read its members must not write eligibility, service or router state")
	require.Greater(t, len(watched.listOptions), 6, "the pass must have reached its closing read")

	after := new(core.ServiceList)
	require.NoError(t, f.reconciler.Client.List(ctx, after))
	require.Equal(t, before.Items, after.Items,
		"the refused pass must leave every service exactly as it found it")
}

// TestANativeReadFailureIsNotAWidthTheEngineHasNotReported pins the difference between a question
// this pass could not ask and an answer that says the work is still running. Reporting the second
// when the transport failed hides the failure behind an ordinary wait and advances the operation on
// an answer that never arrived.
func TestANativeReadFailureIsNotAWidthTheEngineHasNotReported(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.md.Spec.Roles[0].ElasticEP.Width = 4
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)

	pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	op := &elasticOperation{
		Name: f.md.Name, Namespace: f.md.Namespace, DeploymentUID: f.md.UID, Generation: f.md.Generation,
		Width: elasticWidth{Old: 2, Target: 4}, State: elasticStateCommandSent, CommandSent: true,
		CommandIntent: "scale to 4",
	}
	for i := range pods {
		pod := &pods[i]
		id := elasticCapturedIdentity{Name: pod.Name, UID: pod.UID}
		if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(f.md) {
			op.Head = id
		} else {
			op.Members = append(op.Members, id)
			if modelDeploymentOrdinalOrFloor(pod) == 0 {
				op.Master = id
			}
		}
	}
	_, err = newElasticOperationStore(f.reconciler.Client).Create(ctx, op)
	require.NoError(t, err)

	// The engine reports the width the record asked for and serves every rank, so the pass reaches
	// the scaling read, and then refuses every scaling answer.
	f.nativeWidth.Store(4)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/is_scaling_elastic_ep":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/v1/completions":
			if req.Header.Get("X-data-parallel-rank") == "-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w,
					`{"error":{"message":"data_parallel_rank -1 is out of range [0, %d).","type":"BadRequestError","param":null,"code":400}}`,
					f.nativeWidth.Load())
				return
			}
			_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"text_completion","choices":[{"index":0,"text":" "}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(broken.Close)
	f.reconciler.elasticClient = func(*core.Pod) (*elasticengine.Client, error) {
		return elasticengine.New(broken.URL, time.Second)
	}

	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err, "a reported native read failure is not a failed reconcile")

	status := f.observation(t)
	require.NotContains(t, status.Reason, "the engine has not reported the new width yet",
		"a transport failure is not the engine declining to answer")
	require.NotEmpty(t, status.Reason)
}
