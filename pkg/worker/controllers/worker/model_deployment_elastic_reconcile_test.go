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
	"gpustack.ai/gpustack/pkg/kubeapistatus"
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
	scaleMalformed    atomic.Bool
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
			if f.scaleMalformed.Load() {
				// A 200 whose body is not the acknowledgement the route promises: the answer
				// arrived and said nothing, which the client classifies as malformed.
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"unexpected":true}`))
				return
			}
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

// stageRecovery drives the fixture into the recovery pass: a sent upward record at an older
// generation whose target the engine never reached, with the desired width back at the record's
// own old width. withExcess decides between the recovery's two exits: without excess members the
// stale record retires outright, with them it swaps for a release-only record at the proven old
// width.
func (f *elasticConvergenceFixture) stageRecovery(t *testing.T, withExcess bool) {
	t.Helper()
	ctx := context.Background()
	if withExcess {
		f.md.Spec.Roles[0].ElasticEP.Width = 4
		f.md.Generation++
		require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
		_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
		require.NoError(t, err)
		f.startMembers(t)
		f.admitMembers(t)
		// The desired width returns to the record's own old width, which is what makes the
		// recorded upward command stale.
		f.md.Spec.Roles[0].ElasticEP.Width = 2
	} else {
		f.startMembers(t)
	}
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
	_, err = newElasticOperationStore(f.reconciler.Client).Create(ctx, op)
	require.NoError(t, err)
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
}

// TestRecoveryWithoutExcessMembersRetiresTheStaleRecord pins the exit where the interrupted
// upscale never left its old width: the stale record retires on the proven old world, and the
// observation reads a completed deployment at that width.
func TestRecoveryWithoutExcessMembersRetiresTheStaleRecord(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageRecovery(t, false)

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.True(t, apierrors.IsNotFound(readErr))
	observed := f.observation(t)
	require.Equal(t, 2, observed.StableWidth)
	require.Equal(t, elasticStateCompleted, observed.State)
	require.Contains(t, observed.Reason, "never left the proven old native width")
}

// TestTheRecoveryRetirementSurvivesAFailedObservationWrite pins the recovery exits' ordering: the
// observation claiming the outcome is written BEFORE the record moves, so a pass that could not
// make its proof durable leaves the stale record holding the deployment instead of deleting or
// swapping it behind an observation nothing backs.
func TestTheRecoveryRetirementSurvivesAFailedObservationWrite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withExcess bool
		exitReason string
	}{
		{
			name: "no retiring members", withExcess: false,
			exitReason: "never left the proven old native width",
		},
		{
			name: "retiring members", withExcess: true,
			exitReason: "recovered at the proven old native width",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newElasticConvergenceFixture(t)
			f.stageRecovery(t, tc.withExcess)

			var attempted string
			f.reconciler.Client = ctrlinterceptor.NewClient(f.reconciler.Client.(ctrlcli.WithWatch),
				ctrlinterceptor.Funcs{
					Update: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object,
						opts ...ctrlcli.UpdateOption,
					) error {
						if cm, ok := obj.(*core.ConfigMap); ok &&
							cm.Name == modelDeploymentElasticObservationName(f.md) {
							attempted = cm.Data["observation.json"]
							return fmt.Errorf("the observation write is unavailable")
						}
						return cli.Update(ctx, obj, opts...)
					},
				})

			require.Error(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))
			require.NotEmpty(t, attempted,
				"the pass never attempted the recovery exit, so it proves nothing")
			require.Contains(t, attempted, tc.exitReason,
				"the write the pass could not make is the recovery exit's own claim")

			op, readErr := newElasticOperationStore(f.reconciler.Client).Read(
				ctx, f.md.Namespace, f.md.Name, f.md.UID)
			require.NoError(t, readErr, "a failed proof leaves the record holding the deployment")
			require.Equal(t, elasticStateCommandSent, op.State)
			require.Equal(t, elasticWidth{Old: 2, Target: 4}, op.Width)
		})
	}
}

// TestTheRecoverySwapIsRetryableWhenItsWriteFails pins the swap's failure shape: a failed
// recovery write leaves the stale record byte-for-byte intact, and the next pass with a healthy
// store completes the same swap without a new command and without losing the retirement.
func TestTheRecoverySwapIsRetryableWhenItsWriteFails(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageRecovery(t, true)

	plain := f.reconciler.Client
	f.reconciler.Client = ctrlinterceptor.NewClient(plain.(ctrlcli.WithWatch),
		ctrlinterceptor.Funcs{
			Update: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object,
				opts ...ctrlcli.UpdateOption,
			) error {
				if cm, ok := obj.(*core.ConfigMap); ok &&
					cm.Name == elasticOperationRecordName(f.md.UID) {
					return fmt.Errorf("the record write is unavailable")
				}
				return cli.Update(ctx, obj, opts...)
			},
		})
	require.Error(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	op, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.NoError(t, readErr, "the stale record survives a failed swap")
	require.Equal(t, elasticStateCommandSent, op.State)
	require.Equal(t, elasticWidth{Old: 2, Target: 4}, op.Width)
	require.Equal(t, "scale to 4", op.CommandIntent)

	// The next pass writes through a healthy store: the same recovery completes, still without
	// any native command, and the record it leaves is the release-only one.
	f.reconciler.Client = plain
	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))
	recovered, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.NoError(t, readErr)
	require.Equal(t, elasticStateReleased, recovered.State)
	require.Equal(t, elasticWidth{Old: 4, Target: 2}, recovered.Width)
	require.Len(t, recovered.Workers, 2)
	require.Zero(t, f.scaleCalls.Load())
}

// TestANodeLossRecoversFromTheLiveWorldInsteadOfHolding pins the scene where every captured
// member dies with its node: the mismatch never heals (replacements carry new identities), the
// pending retirement has no work left, and the pass recovers from the live world — the stale
// record retires on the proven live width, the observation completes at it, and no native command
// is issued, because recovery proves rather than scales.
func TestANodeLossRecoversFromTheLiveWorldInsteadOfHolding(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageRecovery(t, true)

	// The node takes every captured member with it.
	pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	require.NotEmpty(t, pods, "the fixture must have captured members for a node loss to mean anything")
	for i := range pods {
		require.NoError(t, f.reconciler.Client.Delete(ctx, &pods[i]))
	}

	// The workload is recreated at the desired width with fresh identities, and the next pass
	// reads only replacements.
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)
	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.True(t, apierrors.IsNotFound(readErr),
		"a record whose members are all gone must retire, not hold: %v", f.observation(t).Reason)
	observed := f.observation(t)
	require.Equal(t, 2, observed.StableWidth)
	require.Equal(t, elasticStateCompleted, observed.State)
	require.Contains(t, observed.Reason, "never left the proven old native width")
	require.Zero(t, f.scaleCalls.Load(), "recovery proves the live world; it issues no native command")
}

// TestTheSameGenerationMemberDriftStillHolds is the deliberate hold the node-loss recovery must
// not swallow: pods replaced while the spec is untouched stay an in-flight scene the record may
// still describe, so the identity mismatch keeps holding until the spec itself moves on.
func TestTheSameGenerationMemberDriftStillHolds(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.md.Spec.Roles[0].ElasticEP.Width = 4
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)
	f.admitMembers(t)

	// The record is captured at the generation that is still current: no spec edit follows.
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
	_, err = newElasticOperationStore(f.reconciler.Client).Create(ctx, op)
	require.NoError(t, err)

	// Every captured pod is replaced at the same generation.
	for i := range pods {
		require.NoError(t, f.reconciler.Client.Delete(ctx, &pods[i]))
	}
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)
	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	held, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.NoError(t, readErr, "a same-generation drift keeps the record")
	require.Equal(t, elasticStateCommandSent, held.State)
	require.Equal(t, "a captured member identity no longer matches; the operation holds",
		f.observation(t).Reason)
	require.Zero(t, f.scaleCalls.Load())
}

// TestAPartialNodeLossStillHolds pins the other edge of the recovery gate: as long as one
// captured member is still the pod it was recorded against, the retirement may still have work to
// do, so even a generation-stale record holds on the identity mismatch instead of recovering.
func TestAPartialNodeLossStillHolds(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageRecovery(t, true)

	// The node takes two of the four captured members; the master and one member survive.
	pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	lost := 0
	for i := range pods {
		pod := &pods[i]
		if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(f.md) {
			continue
		}
		ordinal, valid := modelDeploymentPodOrdinal(pod)
		if valid && (ordinal == 1 || ordinal == 3) {
			require.NoError(t, f.reconciler.Client.Delete(ctx, pod))
			lost++
		}
	}
	require.Equal(t, 2, lost, "the case must lose captured members to mean anything")

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	held, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.NoError(t, readErr, "a partial survival keeps the record")
	require.Equal(t, elasticStateCommandSent, held.State)
	require.Equal(t, "a captured member identity no longer matches; the operation holds",
		f.observation(t).Reason)
	require.Zero(t, f.scaleCalls.Load())
}

// stageLostUpscaleWorld drives the fixture to the record-loss wedge scene: an upscale stood at
// width 4 natively, the desired width came back to 2, and the operation record is deleted, so the
// next pass reads an empty store against a live world wider than the bookkeeping width.
func (f *elasticConvergenceFixture) stageLostUpscaleWorld(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f.md.Spec.Roles[0].ElasticEP.Width = 4
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)
	f.admitMembers(t)
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	// The engine followed the command before the record was lost.
	f.nativeWidth.Store(4)
	record := new(core.ConfigMap)
	require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: f.md.Namespace, Name: elasticOperationRecordName(f.md.UID),
	}, record), "the staged world must hold a record before it is lost")
	require.NoError(t, f.reconciler.Client.Delete(ctx, record))
	f.md.Spec.Roles[0].ElasticEP.Width = 2
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
}

// TestALostRecordOverAWiderLiveWorldCreatesTheDownscale pins the wedge: with no operation record
// and a live Ray cluster wider than the bookkeeping width, the pass must prove the live world at
// its own width and let the ordinary corrective block create the downscale, instead of failing
// the rank-map check at the stale bookkeeping width forever.
func TestALostRecordOverAWiderLiveWorldCreatesTheDownscale(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageLostUpscaleWorld(t)
	scalesBefore := f.scaleCalls.Load()

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	op, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.NoError(t, readErr, "the pass must create the corrective downscale; observation says: %s",
		f.observation(t).Reason)
	require.Equal(t, elasticStateRecorded, op.State)
	require.Equal(t, elasticWidth{Old: 4, Target: 2}, op.Width, "the live width is the proven old width")
	require.Len(t, op.Workers, 2, "the two members beyond the desired width are the retirement")
	require.NotEmpty(t, op.Head.Name)
	require.NotEmpty(t, op.Master.Name)
	require.NotNil(t, op.Release)
	require.Equal(t, scalesBefore, f.scaleCalls.Load(),
		"the corrective operation is recorded; the ordinary flow sends it on a later pass")
	require.Equal(t, 4, f.observation(t).StableWidth, "the proven live width heals the stale bookkeeping")
}

// TestOpNilAtTheBookkeepingWidthStillConverges is the unchanged guard: with no record and no live
// drift, the bookkeeping width still proves and the pass still converges without any operation.
func TestOpNilAtTheBookkeepingWidthStillConverges(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.True(t, apierrors.IsNotFound(readErr))
	observed := f.observation(t)
	require.Equal(t, "native ranks and forwards agree", observed.Reason)
	require.Equal(t, 2, observed.StableWidth)
	require.Zero(t, f.scaleCalls.Load())
}

// TestOpNilUnderANarrowerLiveWorldKeepsHolding pins the edge the recovery does not cross: a live
// world narrower than the bookkeeping width is a degraded scene the width hint cannot speak for,
// so the pass keeps today's behavior — an unknown effective layer and no synthesized operation.
func TestOpNilUnderANarrowerLiveWorldKeepsHolding(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)

	// The bookkeeping width climbs to 4 while the live world drops to one member: the stale-high
	// bookkeeping a lost downscale would leave behind.
	cm, err := f.reconciler.elasticObservationDocument(ctx, f.md)
	require.NoError(t, err)
	require.NotNil(t, cm)
	var observed modelDeploymentElasticStatus
	require.NoError(t, json.Unmarshal([]byte(cm.Data["observation.json"]), &observed))
	observed.StableWidth = 4
	body, err := json.Marshal(observed)
	require.NoError(t, err)
	cm.Data = map[string]string{"observation.json": string(body)}
	require.NoError(t, f.reconciler.Client.Update(ctx, cm))

	pods, err := f.reconciler.elasticLiveMembers(ctx, f.md)
	require.NoError(t, err)
	for i := range pods {
		pod := &pods[i]
		if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(f.md) {
			continue
		}
		ordinal, valid := modelDeploymentPodOrdinal(pod)
		if valid && ordinal == 1 {
			require.NoError(t, f.reconciler.Client.Delete(ctx, pod))
		}
	}
	f.nativeWidth.Store(1)

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.True(t, apierrors.IsNotFound(readErr),
		"a narrower live world synthesizes no corrective operation")
	require.Equal(t, "native rank identities are unknown", f.observation(t).Reason)
	require.Zero(t, f.scaleCalls.Load())
}

// TestTheLiveWidthReprobeStillOwesTheForwardProof pins the proof discipline of the re-probe: it
// is the same rank and forward proof at the live width, so a live width whose native forwards
// fail still creates no operation and reports the forward failure, not the stale-width one.
func TestTheLiveWidthReprobeStillOwesTheForwardProof(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageLostUpscaleWorld(t)
	f.forwardsFail.Store(true)
	scalesBefore := f.scaleCalls.Load()

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.True(t, apierrors.IsNotFound(readErr),
		"a live width that fails its forward proof synthesizes no operation")
	require.Positive(t, f.forwardCalls.Load(),
		"the re-probe must have run the native forward proof at the live width")
	require.NotEqual(t, "native rank identities are unknown", f.observation(t).Reason,
		"the reported failure is the re-probe's, not the stale bookkeeping probe's")
	require.Equal(t, scalesBefore, f.scaleCalls.Load(),
		"an unproven live width issues no corrective command")
}

// TestTheReprobeDoesNotDependOnTheObservationBooks pins the live-surgery lesson: the stableWidth
// override is gated on the recorded master identity, so a master that moved since the last
// persisted observation silently reads the bootstrap width instead, and the surgery's edited
// width is ignored outright. The recovery must not depend on those books: the admitted live world
// alone names the wider world to re-prove.
func TestTheReprobeDoesNotDependOnTheObservationBooks(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageLostUpscaleWorld(t)

	// The books are stale twice over: the stable width is wrong and the recorded master is
	// foreign, so the width override cannot be applied at all.
	cm, err := f.reconciler.elasticObservationDocument(ctx, f.md)
	require.NoError(t, err)
	require.NotNil(t, cm)
	var observed modelDeploymentElasticStatus
	require.NoError(t, json.Unmarshal([]byte(cm.Data["observation.json"]), &observed))
	observed.MasterUID = "a-master-that-no-longer-exists"
	body, err := json.Marshal(observed)
	require.NoError(t, err)
	cm.Data = map[string]string{"observation.json": string(body)}
	require.NoError(t, f.reconciler.Client.Update(ctx, cm))

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	op, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.NoError(t, readErr, "the re-prove must not depend on the observation books: %s",
		f.observation(t).Reason)
	require.Equal(t, elasticWidth{Old: 4, Target: 2}, op.Width)
}

// TestAnUnreadableRayWorldStillCreatesNoOperation pins the floor under the re-probe: when the Ray
// document itself cannot be joined to the captured members, no width proves, the re-probe fails
// with the same honest unknown, and no operation is synthesized from a world the observer cannot
// see. The rank-identity proof stays the safety floor no width hint may bypass.
func TestAnUnreadableRayWorldStillCreatesNoOperation(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.stageLostUpscaleWorld(t)
	f.rayMalformed.Store(true)

	require.NoError(t, f.reconciler.reconcileModelDeploymentElasticResize(ctx, f.md))

	_, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.True(t, apierrors.IsNotFound(readErr),
		"an unprovable Ray world synthesizes no operation")
	require.Equal(t, "native rank identities are unknown", f.observation(t).Reason)
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

// TestElasticConvergenceRetriesARefusedScaleCommand pins the whole life of a refused scale
// command: the record carries the engine's own refusal, the command is re-issued on a bounded
// backoff while the bound holds, the bound's end is a terminal refusal in the observation reason
// and on the deployment's condition — never a silent AwaitingWidth — and a spec that returns to
// the old width still retires the abandoned record through the ordinary recovery.
func TestElasticConvergenceRetriesARefusedScaleCommand(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.scaleStatus.Store(500)
	now := time.Now()
	f.reconciler.clock = func() time.Time { return now }

	recordDoc := func(t *testing.T) map[string]any {
		t.Helper()
		record := new(core.ConfigMap)
		require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKey{
			Namespace: f.md.Namespace, Name: elasticOperationRecordName(f.md.UID),
		}, record))
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(record.Data[elasticOperationDataKey]), &doc))
		return doc
	}
	elasticCondition := kubeapistatus.ConditionType("ElasticResize")
	liveCondition := func(t *testing.T) (status, reason, message string) {
		t.Helper()
		live := new(workercore.ModelDeployment)
		require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(f.md), live))
		return elasticCondition.GetStatus(live), elasticCondition.GetReason(live),
			elasticCondition.GetMessage(live)
	}

	f.md.Spec.Roles[0].ElasticEP.Width = 4
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)
	f.admitMembers(t)

	// The first send is refused. The record keeps the engine's own refusal, and the observation
	// says what the engine said rather than that the width is merely unreported.
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.scaleCalls.Load(), f.observation(t).Reason)
	doc := recordDoc(t)
	require.Contains(t, doc["scaleLastError"], "status 500",
		"the record must keep the engine's own refusal")
	require.Equal(t, float64(1), doc["scaleAttempts"])
	require.Contains(t, f.observation(t).Reason, "status 500", f.observation(t).Reason)
	require.Equal(t, "CommandSent", doc["state"], "a refused command is not an unreported width")

	// Inside the backoff the command is not re-issued, and the wait still names the refusal.
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.scaleCalls.Load())
	require.Contains(t, f.observation(t).Reason, "status 500", f.observation(t).Reason)
	status, reason, _ := liveCondition(t)
	require.NotEqual(t, "False", status,
		"a refusal still inside its retry bound is not terminal")
	require.Equal(t, "NoRefusal", reason)

	// Each elapsed backoff re-issues the command, until the bound is spent.
	for _, step := range []struct {
		advance  time.Duration
		attempts float64
	}{
		{20 * time.Second, 2}, {31 * time.Second, 3}, {61 * time.Second, 4}, {121 * time.Second, 5},
	} {
		now = now.Add(step.advance)
		_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
		require.NoError(t, err)
		doc = recordDoc(t)
		require.Equal(t, step.attempts, doc["scaleAttempts"], f.observation(t).Reason)
		require.Contains(t, doc["scaleLastError"], "status 500")
	}
	require.Equal(t, int32(5), f.scaleCalls.Load())

	// The bound's end is terminal: the record is abandoned, and the refusal reaches both the
	// observation reason and the deployment's condition with the engine text intact.
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, "Abandoned", recordDoc(t)["state"], f.observation(t).Reason)
	require.Contains(t, f.observation(t).Reason, "status 500", f.observation(t).Reason)
	status, reason, message := liveCondition(t)
	require.Equal(t, "False", status, "a spent retry bound is a terminal refusal")
	require.Equal(t, "ScaleRefused", reason)
	require.Contains(t, message, "status 500")

	// Terminal means terminal: no pass re-issues the command and the refusal does not fade.
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(5), f.scaleCalls.Load())
	require.Equal(t, "Abandoned", recordDoc(t)["state"])
	status, _, message = liveCondition(t)
	require.Equal(t, "False", status)
	require.Contains(t, message, "status 500")

	// The escape is the ordinary one: a spec returned to the proven old width recovers the
	// abandoned record. Two members beyond the old width are still admitted, so the recovery
	// swaps the record for the release-only retirement they owe — and no refusal stands.
	f.md.Spec.Roles[0].ElasticEP.Width = 2
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	recovered, readErr := newElasticOperationStore(f.reconciler.Client).Read(
		ctx, f.md.Namespace, f.md.Name, f.md.UID)
	require.NoError(t, readErr, "recovery keeps the retirement the excess members owe: %v",
		f.observation(t).Reason)
	require.Equal(t, elasticStateReleased, recovered.State)
	require.Equal(t, elasticWidth{Old: 4, Target: 2}, recovered.Width)
	require.Len(t, recovered.Workers, 2)
	observed := f.observation(t)
	require.Equal(t, 2, observed.StableWidth)
	status, reason, _ = liveCondition(t)
	require.Equal(t, "True", status, "recovery leaves no refusal standing")
	require.Equal(t, "NoRefusal", reason)
	require.Equal(t, int32(5), f.scaleCalls.Load(), "recovery proves; it issues no command")
}

// TestAnUnansweredScaleCommandIsNotARefusal pins the line between an answer and a silence: a 200
// whose acknowledgement never arrived says nothing about whether the command was applied, so the
// record keeps no refusal, the bound consumes nothing, and no pass re-issues the command — the
// world the engine actually proves is what decides, exactly as it did before the retry bound.
func TestAnUnansweredScaleCommandIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.scaleMalformed.Store(true)
	now := time.Now()
	f.reconciler.clock = func() time.Time { return now }

	f.md.Spec.Roles[0].ElasticEP.Width = 4
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)
	f.admitMembers(t)

	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.scaleCalls.Load(), f.observation(t).Reason)

	// The record carries no refusal and no spent attempt: the answer said nothing.
	record := new(core.ConfigMap)
	require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKey{
		Namespace: f.md.Namespace, Name: elasticOperationRecordName(f.md.UID),
	}, record))
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(record.Data[elasticOperationDataKey]), &doc))
	require.NotContains(t, doc, "scaleLastError")
	require.NotContains(t, doc, "scaleAttempts")

	// The command is not re-issued however long the pass waits: only observation completes it.
	now = now.Add(10 * time.Minute)
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.scaleCalls.Load())
	require.Equal(t, "the engine has not reported the new width yet", f.observation(t).Reason)
}

// TestAnUnansweredRetryStillEscalatesTheBackoff pins the schedule's treatment of a re-issued
// command whose answer never arrives: the refusal bound consumes nothing — the engine never
// declined — but the backoff still moves, so a persistently unreachable engine is probed on the
// doubling schedule the definite refusals earned instead of on a fixed hot cadence. The recorded
// refusal text survives every unanswered re-issue.
func TestAnUnansweredRetryStillEscalatesTheBackoff(t *testing.T) {
	ctx := context.Background()
	f := newElasticConvergenceFixture(t)
	f.scaleStatus.Store(500)
	now := time.Now()
	f.reconciler.clock = func() time.Time { return now }

	recordDoc := func(t *testing.T) map[string]any {
		t.Helper()
		record := new(core.ConfigMap)
		require.NoError(t, f.reconciler.Client.Get(ctx, ctrlcli.ObjectKey{
			Namespace: f.md.Namespace, Name: elasticOperationRecordName(f.md.UID),
		}, record))
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(record.Data[elasticOperationDataKey]), &doc))
		return doc
	}

	f.md.Spec.Roles[0].ElasticEP.Width = 4
	f.md.Generation++
	require.NoError(t, f.reconciler.Client.Update(ctx, f.md))
	_, err := f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	f.startMembers(t)
	f.admitMembers(t)

	// The first send earns a definite refusal: the bound has one spent attempt and a 15s wait.
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.scaleCalls.Load(), f.observation(t).Reason)
	doc := recordDoc(t)
	require.Equal(t, float64(1), doc["scaleAttempts"])
	require.NotContains(t, doc, "scaleAmbiguousAttempts")

	// The re-issue's answer never arrives: the engine takes the command and returns an
	// acknowledgement that says nothing.
	f.scaleMalformed.Store(true)
	now = now.Add(16 * time.Second)
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(2), f.scaleCalls.Load(), f.observation(t).Reason)
	doc = recordDoc(t)
	require.Equal(t, float64(1), doc["scaleAttempts"], "an unanswered re-issue spends no bound")
	require.Equal(t, float64(1), doc["scaleAmbiguousAttempts"])
	require.Contains(t, doc["scaleLastError"], "status 500",
		"the last definite refusal survives an unanswered re-issue")

	// The wait doubled: sixteen seconds in, the next attempt is not due yet.
	now = now.Add(16 * time.Second)
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(2), f.scaleCalls.Load(), f.observation(t).Reason)

	// Once the doubled wait has run, the command is re-issued again — and a second silence moves
	// the schedule once more without touching the refusal bound.
	now = now.Add(15 * time.Second)
	_, err = f.reconciler.convergeModelDeployment(ctx, f.md)
	require.NoError(t, err)
	require.Equal(t, int32(3), f.scaleCalls.Load(), f.observation(t).Reason)
	doc = recordDoc(t)
	require.Equal(t, float64(1), doc["scaleAttempts"])
	require.Equal(t, float64(2), doc["scaleAmbiguousAttempts"])
	require.Contains(t, doc["scaleLastError"], "status 500")
}
