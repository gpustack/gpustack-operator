package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/rest"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepod "sigs.k8s.io/kueue/pkg/controller/jobs/pod"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	gpustack "gpustack.ai/gpustack/api/v1"
	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/manager"
	"gpustack.ai/gpustack/pkg/modelforger"
	"gpustack.ai/gpustack/pkg/modelkernelkey"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

func buildTestKernelKey(t *testing.T, opFamily string, hiddenSize int64) string {
	t.Helper()
	key, err := modelkernelkey.Build(modelkernelkey.BuildSpec{
		Engine:        "vLLM",
		EngineVersion: "0.19.0",
		Manufacturer:  nodefeature.ManufacturerNVIDIA,
		Arch:          "sm_90a",
		OpFamily:      opFamily,
		ConstAxes:     map[string]int64{"hidden_size": hiddenSize},
		InputDtypes:   []string{"bfloat16", "bfloat16"},
		OutputDtypes:  []string{"bfloat16"},
	})
	require.NoError(t, err)
	raw, err := key.Format()
	require.NoError(t, err)
	return raw
}

func newForgerTestNode(name string) *core.Node {
	labels := nodefeature.ConstructAcceleratableNodeLabels(device.DevicesGroupList{
		{
			ID:                "h20",
			Manufacturer:      nodefeature.ManufacturerNVIDIA,
			Name:              "NVIDIA-H20",
			Memory:            98304,
			Cores:             16896,
			Family:            "Hopper",
			ComputeCapability: "9.0a",
			Accelerators:      []device.Accelerator{{ID: "0"}},
		},
	})
	labels["kubernetes.io/hostname"] = name
	labels["gpustack.ai/managed"] = "true"
	labels["node.gpustack.ai/pool"] = "h20-8x"
	labels["kueue.gpustack.ai/flavor"] = "h20-8x"
	return &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}

func newForgerTestDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := newKernelTestDeployment(func(d *workercore.ModelDeployment) {
		d.Name = "qwen-pd"
		d.UID = "md-qwen-pd-uid"
		d.Generation = 1
		d.Spec.Roles[0].Name = "decode"
		d.Spec.Roles[0].Replicas = 2
	})
	for _, fn := range mutate {
		fn(md)
	}
	return md
}

func newTestModelDeploymentForger(mutate ...func(*workercore.ModelDeploymentForger)) *workercore.ModelDeploymentForger {
	mdf := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "team-a",
			Name:       "qwen-decode-forger",
			UID:        "mdf-uid-1",
			Generation: 1,
		},
		Spec: workercore.ModelDeploymentForgerSpec{
			TargetRef: core.LocalObjectReference{Name: "qwen-pd"},
			Mode:      workercore.ModelDeploymentForgerModeActive,
			Roles: []workercore.ModelDeploymentForgerRoleSpec{
				{
					Name:                    "decode",
					MaxKernelsPerGeneration: 4,
					MinSpeedupRatio:         "1.05",
				},
			},
			Probe: workercore.ModelDeploymentForgerProbeSpec{
				Steps:             4,
				CooldownSeconds:   600,
				MinActiveRequests: 1,
			},
			Sandbox: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode:            workercore.ModelDeploymentForgerRunnerModeMock,
				ActiveDeadlineSeconds: 1800,
			},
			Rollout: workercore.ModelDeploymentForgerRolloutSpec{
				MaxRetainedGenerations: 1,
				FailureTimeoutSeconds:  600,
			},
		},
	}
	for _, fn := range mutate {
		fn(mdf)
	}
	return mdf
}

func newForgerTestFakeClient(objs ...ctrlcli.Object) ctrlcli.Client {
	var seq atomic.Int64
	v1AT := newRenderAcceleratorType()
	v1alpha1AT := (*workercore.AcceleratorType)(v1AT.DeepCopy())
	v1alpha1AT.TypeMeta = meta.TypeMeta{}
	all := make([]ctrlcli.Object, 0, 5+len(objs))
	all = append(all,
		v1AT,
		v1alpha1AT,
		&nodev1.RuntimeClass{ObjectMeta: meta.ObjectMeta{Name: "nvidia"}, Handler: "nvidia"},
		&storagev1.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}},
	)
	all = append(all, objs...)
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(
			&workercore.ModelDeployment{},
			&workercore.ModelDeploymentForger{},
			&workercore.ModelDeploymentScaler{},
			&workercore.ModelKernelArtifact{},
			&workercore.ModelPrefetch{},
			&workercore.NodeModelStore{},
			&core.Pod{},
			&kueue.Workload{},
		).
		WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string {
			pod, ok := obj.(*core.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		WithIndex(&core.Pod{}, kueuepod.PodGroupNameCacheKey, func(obj ctrlcli.Object) []string {
			pod, ok := obj.(*core.Pod)
			if !ok {
				return nil
			}
			group := pod.Labels[kueuepodconst.GroupNameLabel]
			if group == "" {
				return nil
			}
			return []string{group}
		}).
		WithIndex(&core.Pod{}, modelDeploymentDrainIndexPodUID, func(obj ctrlcli.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithIndex(&workercore.ModelPrefetch{}, modelPrefetchArtifactIndex, modelPrefetchArtifactIndexValues).
		WithObjects(all...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				n := seq.Add(1)
				if obj.GetName() == "" && obj.GetGenerateName() != "" {
					obj.SetName(fmt.Sprintf("%s%02d", obj.GetGenerateName(), n))
				}
				if obj.GetUID() == "" {
					obj.SetUID(types.UID(fmt.Sprintf("uid-%s-%d", obj.GetName(), n)))
				}
				if obj.GetGeneration() == 0 {
					obj.SetGeneration(1)
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
				if md, ok := obj.(*workercore.ModelDeployment); ok {
					var prev workercore.ModelDeployment
					if err := c.Get(ctx, ctrlcli.ObjectKeyFromObject(md), &prev); err == nil {
						if !kubemeta.DeepEqual(prev.Spec, md.Spec) && md.Generation == prev.Generation {
							md.Generation = prev.Generation + 1
						}
					}
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()
}

func findForgerCondition(mdf *workercore.ModelDeploymentForger, condType string) *gpustack.Condition {
	for i := range mdf.Status.Conditions {
		if mdf.Status.Conditions[i].Type == condType {
			return &mdf.Status.Conditions[i]
		}
	}
	return nil
}

func findForgerRoleStatus(mdf *workercore.ModelDeploymentForger, roleName string) *workercore.ModelDeploymentForgerRoleStatus {
	for i := range mdf.Status.Roles {
		if mdf.Status.Roles[i].Name == roleName {
			return &mdf.Status.Roles[i]
		}
	}
	return nil
}

func markServingPodReadyEligible(t *testing.T, cli ctrlcli.Client, pod *core.Pod, nodeName, podIP string, port int) {
	t.Helper()
	var current core.Pod
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), &current))
	current.Spec.NodeName = nodeName
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	current.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations["prometheus.io/scrape"] = "true"
	current.Annotations["prometheus.io/port"] = strconv.Itoa(port)
	current.Annotations["prometheus.io/path"] = "/metrics"
	current.Annotations["prometheus.io/scheme"] = "http"
	if len(current.Spec.Containers) > 0 {
		hasPort := false
		for _, p := range current.Spec.Containers[0].Ports {
			if int(p.ContainerPort) == port {
				hasPort = true
				break
			}
		}
		if !hasPort {
			current.Spec.Containers[0].Ports = append(current.Spec.Containers[0].Ports, core.ContainerPort{
				Name:          "http",
				ContainerPort: int32(port),
			})
		}
	}
	require.NoError(t, cli.Update(context.Background(), &current))

	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), &current))
	current.Status.Phase = core.PodRunning
	current.Status.PodIP = podIP
	current.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
	current.Status.ContainerStatuses = []core.ContainerStatus{{Name: modelDeploymentMainContainerName, Ready: true}}
	require.NoError(t, cli.Status().Update(context.Background(), &current))
	*pod = current
}

type mockSentinelServer struct {
	Server *httptest.Server
	Host   string
	Port   int

	mu                     sync.Mutex
	BootID                 string
	Ready                  bool
	RunningReqs            float64
	WaitingReqs            float64
	ProbeState             string
	ProbeCompleteOnNextGet bool
	ProbePayload           *runtimecontrol.ProbePayload
	ProbeStartCalls        int
	ProbeGetCalls          int
	ProbeDeleteCalls       int

	DefaultDispatchDigest string
	DefaultLoadedKeys     []string
	DefaultLoadError      string
	PodRuntimeByKernel    map[string]runtimecontrol.RuntimeConfigState
}

func newMockSentinelServer(t *testing.T, key1 string) *mockSentinelServer {
	t.Helper()
	hidden := int64(4096)
	ms := &mockSentinelServer{
		BootID:      "boot-leader-1",
		Ready:       true,
		RunningReqs: 4,
		WaitingReqs: 0,
		ProbePayload: &runtimecontrol.ProbePayload{
			SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
			Engine:        "vLLM",
			EngineVersion: "0.19.0",
			Manufacturer:  nodefeature.ManufacturerNVIDIA,
			Architecture:  "sm_90a",
			Definitions: []runtimecontrol.ProbeDefinition{
				{
					Name:           "rmsnorm_h4096",
					OpType:         modelkernelkey.OpFamilyRMSNorm,
					LocalKernelKey: key1,
					Axes: map[string]runtimecontrol.ProbeAxisSpec{
						"num_tokens":  {Type: "var"},
						"hidden_size": {Type: "const", Value: &hidden},
					},
					Inputs: map[string]runtimecontrol.ProbeTensorSpec{
						"x":      {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
						"weight": {Shape: []string{"hidden_size"}, Dtype: "bfloat16"},
					},
					Outputs: map[string]runtimecontrol.ProbeTensorSpec{
						"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
					},
					Reference: "def run(x, weight): return x * weight",
				},
			},
			Workloads: []runtimecontrol.ProbeWorkload{
				{
					UUID:               "wl-1",
					Definition:         "rmsnorm_h4096",
					LocalKernelKey:     key1,
					Axes:               map[string]int64{"num_tokens": 128},
					Inputs:             map[string]runtimecontrol.ProbeWorkloadInput{"x": {Type: "random"}, "weight": {Type: "random"}},
					ObservedDurationUs: 42.5,
					CallCount:          16,
				},
			},
			Bottlenecks: []runtimecontrol.ProbeBottleneck{
				{
					LocalKernelKey:      key1,
					OpFamily:            modelkernelkey.OpFamilyRMSNorm,
					GPUTimeUs:           680.0,
					GPUTimeSharePercent: 18.5,
					CallCount:           16,
				},
				{
					LocalKernelKey:      "vllm/0.19/nvidia/sm_90a/gemm/0123456789abcdef",
					OpFamily:            "gemm",
					GPUTimeUs:           2400.0,
					GPUTimeSharePercent: 65.0,
					CallCount:           16,
				},
			},
			Diagnostics: runtimecontrol.ProbeDiagnostics{
				StepsCaptured:    4,
				WallClockSeconds: 0.35,
			},
		},
		PodRuntimeByKernel: map[string]runtimecontrol.RuntimeConfigState{},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms.mu.Lock()
		defer ms.mu.Unlock()

		switch r.URL.Path {
		case "/v1/completions":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "cmpl-probe",
				"choices": []map[string]any{{"text": "ok"}},
			})
		case "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = fmt.Fprintf(w,
				"vllm:num_requests_running %.1f\nvllm:num_requests_waiting %.1f\n"+
					"vllm:prompt_tokens_total 1000\nvllm:generation_tokens_total 2000\n",
				ms.RunningReqs, ms.WaitingReqs,
			)
		case runtimecontrol.PathLive:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(runtimecontrol.LiveResponse{
				Alive:  true,
				BootID: ms.BootID,
				Paused: false,
			})
		case runtimecontrol.PathPause:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(runtimecontrol.PauseResponse{
				Paused: false,
				BootID: ms.BootID,
			})
		case runtimecontrol.PathAck:
			var ackReq runtimecontrol.AckRequest
			_ = json.NewDecoder(r.Body).Decode(&ackReq)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(runtimecontrol.ReadyResponse{
				Ready:                  ms.Ready,
				BootID:                 ms.BootID,
				ObservedGeneration:     ptr.To(ackReq.ObservedGeneration),
				AppliedConfigDigest:    ackReq.AppliedConfigDigest,
				LoadedKernelKeys:       slices.Clone(ms.DefaultLoadedKeys),
				DispatchManifestDigest: ms.DefaultDispatchDigest,
				KernelLoadError:        ms.DefaultLoadError,
			})
		case runtimecontrol.PathReady:
			w.Header().Set("Content-Type", "application/json")
			if !ms.Ready {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			_ = json.NewEncoder(w).Encode(runtimecontrol.ReadyResponse{
				Ready:                  ms.Ready,
				BootID:                 ms.BootID,
				LoadedKernelKeys:       slices.Clone(ms.DefaultLoadedKeys),
				DispatchManifestDigest: ms.DefaultDispatchDigest,
				KernelLoadError:        ms.DefaultLoadError,
			})
		case runtimecontrol.PathRuntimeConfig:
			artifactKey := r.URL.Query().Get("artifact")
			if st, ok := ms.PodRuntimeByKernel[artifactKey]; ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(st)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(runtimecontrol.RuntimeConfigState{
				Engine:                 "vllm",
				BootID:                 ms.BootID,
				Ready:                  ms.Ready,
				Paused:                 false,
				LoadedKernelKeys:       slices.Clone(ms.DefaultLoadedKeys),
				DispatchManifestDigest: ms.DefaultDispatchDigest,
				KernelLoadError:        ms.DefaultLoadError,
			})
		case runtimecontrol.PathProbe:
			switch r.Method {
			case http.MethodPost:
				ms.ProbeStartCalls++
				var reqBody runtimecontrol.ProbeRequest
				_ = json.NewDecoder(r.Body).Decode(&reqBody)
				if reqBody.ExpectedBootID != ms.BootID {
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error":            "boot_id_mismatch",
						"boot_id":          ms.BootID,
						"expected_boot_id": reqBody.ExpectedBootID,
					})
					return
				}
				ms.ProbeState = runtimecontrol.ProbeStateRunning
				ms.ProbeCompleteOnNextGet = true
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(runtimecontrol.ProbeResponse{
					BootID:  ms.BootID,
					State:   runtimecontrol.ProbeStateRunning,
					ProbeID: fmt.Sprintf("probe-%d", ms.ProbeStartCalls),
					Steps:   reqBody.Steps,
				})
			case http.MethodGet:
				ms.ProbeGetCalls++
				if ms.ProbeCompleteOnNextGet {
					ms.ProbeState = runtimecontrol.ProbeStateCompleted
					ms.ProbeCompleteOnNextGet = false
				}
				if ms.ProbeState == "" {
					ms.ProbeState = runtimecontrol.ProbeStateIdle
				}
				resp := runtimecontrol.ProbeResponse{
					BootID: ms.BootID,
					State:  ms.ProbeState,
				}
				if ms.ProbeState == runtimecontrol.ProbeStateCompleted && ms.ProbePayload != nil {
					cp := *ms.ProbePayload
					resp.Payload = &cp
					resp.Diagnostics = cp.Diagnostics
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			case http.MethodDelete:
				ms.ProbeDeleteCalls++
				ms.ProbeState = runtimecontrol.ProbeStateAborted
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(runtimecontrol.ProbeResponse{
					BootID: ms.BootID,
					State:  runtimecontrol.ProbeStateAborted,
				})
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	ms.Server = srv
	ms.Host = host
	ms.Port = port
	return ms
}

func (ms *mockSentinelServer) SetProbeSingleKey(key, opFamily, defName string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	hidden := int64(4096)
	ms.ProbePayload = &runtimecontrol.ProbePayload{
		SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
		Engine:        "vLLM",
		EngineVersion: "0.19.0",
		Manufacturer:  nodefeature.ManufacturerNVIDIA,
		Architecture:  "sm_90a",
		Definitions: []runtimecontrol.ProbeDefinition{
			{
				Name:           defName,
				OpType:         opFamily,
				LocalKernelKey: key,
				Axes: map[string]runtimecontrol.ProbeAxisSpec{
					"num_tokens":  {Type: "var"},
					"hidden_size": {Type: "const", Value: &hidden},
				},
				Inputs: map[string]runtimecontrol.ProbeTensorSpec{
					"x":      {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
					"weight": {Shape: []string{"hidden_size"}, Dtype: "bfloat16"},
				},
				Outputs: map[string]runtimecontrol.ProbeTensorSpec{
					"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
				},
				Reference: "def run(x, weight): return x * weight",
			},
		},
		Workloads: []runtimecontrol.ProbeWorkload{
			{
				UUID:               "wl-" + defName,
				Definition:         defName,
				LocalKernelKey:     key,
				Axes:               map[string]int64{"num_tokens": 256},
				Inputs:             map[string]runtimecontrol.ProbeWorkloadInput{"x": {Type: "random"}, "weight": {Type: "random"}},
				ObservedDurationUs: 55.0,
				CallCount:          12,
			},
		},
		Bottlenecks: []runtimecontrol.ProbeBottleneck{
			{
				LocalKernelKey:      key,
				OpFamily:            opFamily,
				GPUTimeUs:           660.0,
				GPUTimeSharePercent: 22.0,
				CallCount:           12,
			},
		},
		Diagnostics: runtimecontrol.ProbeDiagnostics{
			StepsCaptured:    4,
			WallClockSeconds: 0.25,
		},
	}
}

func TestModelDeploymentForger_BindingAndModeLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	t.Run("TargetNotFound sets Bound False and Ready Degraded", func(t *testing.T) {
		mdf := newTestModelDeploymentForger()
		cli := newForgerTestFakeClient(mdf)
		r := &ModelDeploymentForgerReconciler{
			Client:    cli,
			APIReader: cli,
			Recorder:  ctrlrecord.NewFakeRecorder(32),
			Now:       func() time.Time { return now },
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// Pass 1 adds finalizer; Pass 2 observes missing target.
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Contains(t, got.Finalizers, workercore.ModelDeploymentForgerFinalizerKey)
		bound := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBound)
		require.NotNil(t, bound)
		assert.Equal(t, meta.ConditionFalse, bound.Status)
		assert.Equal(t, workercore.ModelDeploymentForgerReasonTargetNotFound, bound.Reason)
	})

	t.Run("InvalidBinding and AlreadyBound across modes", func(t *testing.T) {
		mdInvalid := newForgerTestDeployment(func(md *workercore.ModelDeployment) {
			md.Annotations = map[string]string{
				workercore.ModelDeploymentForgerBindingAnnotationKey: "{corrupt-json",
			}
		})
		mdf := newTestModelDeploymentForger()
		cli := newForgerTestFakeClient(mdInvalid, mdf)
		rec := ctrlrecord.NewFakeRecorder(32)
		r := &ModelDeploymentForgerReconciler{Client: cli, APIReader: cli, Recorder: rec, Now: func() time.Time { return now }}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		bound := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBound)
		require.NotNil(t, bound)
		assert.Equal(t, meta.ConditionFalse, bound.Status)
		assert.Equal(t, workercore.ModelDeploymentForgerReasonInvalidBinding, bound.Reason)

		// Replace annotation with another owner's valid binding.
		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(mdInvalid), &liveMD))
		otherRaw, err := json.Marshal(workercore.ModelDeploymentForgerBinding{
			ForgerName: "other-forger",
			ForgerUID:  "other-uid",
			Term:       3,
		})
		require.NoError(t, err)
		liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] = string(otherRaw)
		require.NoError(t, cli.Update(context.Background(), &liveMD))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		bound = findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBound)
		require.NotNil(t, bound)
		assert.Equal(t, workercore.ModelDeploymentForgerReasonAlreadyBound, bound.Reason)
	})

	t.Run("TargetReplaced blocks until spec generation increments", func(t *testing.T) {
		md := newForgerTestDeployment(func(md *workercore.ModelDeployment) {
			md.UID = "md-new-uid"
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Generation = 2
			f.Status.ObservedGeneration = 2
			f.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
				TargetUID:  "md-old-uid",
				TargetName: "qwen-pd",
				Term:       1,
			}
		})
		cli := newForgerTestFakeClient(md, mdf)
		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		bound := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBound)
		require.NotNil(t, bound)
		assert.Equal(t, meta.ConditionFalse, bound.Status)
		assert.Equal(t, workercore.ModelDeploymentForgerReasonTargetReplaced, bound.Reason)

		// Bump Generation to acknowledge target replacement -> binds at Term 2.
		got.Generation = 3
		require.NoError(t, cli.Update(context.Background(), &got))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		bound = findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBound)
		require.NotNil(t, bound)
		assert.Equal(t, meta.ConditionTrue, bound.Status)
		require.NotNil(t, got.Status.Binding)
		assert.Equal(t, "md-new-uid", got.Status.Binding.TargetUID)
	})

	t.Run("Suspend ModeSwitch and Deletion preserve scaler-binding and clean up active sandbox", func(t *testing.T) {
		scalerBindingRaw := `{"scalerName":"qwen-scaler","scalerUID":"scaler-uid-1","term":5}`
		scalerDecisionRaw := `{"scalerUID":"scaler-uid-1","decisionID":"d-5","term":5,"vector":{"decode":2}}`
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Annotations = map[string]string{
				workercore.ModelDeploymentScalerBindingAnnotationKey:  scalerBindingRaw,
				workercore.ModelDeploymentScalerDecisionAnnotationKey: scalerDecisionRaw,
			}
		})
		mdf := newTestModelDeploymentForger()
		sandboxCM := &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{
				Namespace: mdf.Namespace,
				Name:      "qwen-decode-forger-decode-gen-1-input",
				Labels: map[string]string{
					modelforger.SandboxLabelName:     "model-deployment-forger",
					modelforger.SandboxLabelInstance: mdf.Name,
				},
			},
		}
		sandboxPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: mdf.Namespace,
				Name:      "qwen-decode-forger-decode-gen-1-sandbox",
				UID:       "sb-pod-uid-1",
				Labels: map[string]string{
					modelforger.SandboxLabelName:     "model-deployment-forger",
					modelforger.SandboxLabelInstance: mdf.Name,
				},
			},
		}

		cli := newForgerTestFakeClient(md, mdf, sandboxCM, sandboxPod)
		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// Acquire Active binding at Term 1.
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Equal(t, scalerBindingRaw, liveMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey])
		binding, hasBinding, err := readForgerBinding(&liveMD)
		require.NoError(t, err)
		require.True(t, hasBinding)
		assert.Equal(t, int64(1), binding.Term)

		// Attach activeSandbox status, then Suspend: true -> releases forger-binding and cleans up sandbox Pod + CM.
		var liveMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
			Name:  "decode",
			Phase: workercore.ModelDeploymentForgerRolePhaseForging,
			ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
				PodName:       sandboxPod.Name,
				PodUID:        string(sandboxPod.UID),
				ConfigMapName: sandboxCM.Name,
			},
		}}
		require.NoError(t, cli.Status().Update(context.Background(), &liveMDF))

		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Spec.Suspend = true
		liveMDF.Generation++
		require.NoError(t, cli.Update(context.Background(), &liveMDF))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Empty(t, liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])
		assert.Equal(t, scalerBindingRaw, liveMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey],
			"scaler-binding must stay completely untouched")
		assert.Equal(t, scalerDecisionRaw, liveMD.Annotations[workercore.ModelDeploymentScalerDecisionAnnotationKey],
			"scaler-decision must stay completely untouched")
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(sandboxPod), &core.Pod{})))
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(sandboxCM), &core.ConfigMap{})))

		// Unsuspend back to Active -> re-acquires at monotonic Term 2!
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Spec.Suspend = false
		liveMDF.Generation++
		require.NoError(t, cli.Update(context.Background(), &liveMDF))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		binding, hasBinding, err = readForgerBinding(&liveMD)
		require.NoError(t, err)
		require.True(t, hasBinding)
		assert.Equal(t, int64(2), binding.Term)

		// Switch to ObserveOnly -> releases forger-binding while keeping Bound=True and Active=False/ObserveOnlyMode.
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Spec.Mode = workercore.ModelDeploymentForgerModeObserveOnly
		liveMDF.Generation++
		require.NoError(t, cli.Update(context.Background(), &liveMDF))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Empty(t, liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		assert.Equal(t, workercore.ModelDeploymentForgerReasonObserveOnlyMode,
			findForgerCondition(&liveMDF, workercore.ModelDeploymentForgerConditionTypeActive).Reason)

		// Re-enter Active -> Term 3, then Delete -> releases binding, decision, and finalizer cleanly.
		liveMDF.Spec.Mode = workercore.ModelDeploymentForgerModeActive
		liveMDF.Generation++
		require.NoError(t, cli.Update(context.Background(), &liveMDF))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Delete(context.Background(), &liveMDF))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), req.NamespacedName, &workercore.ModelDeploymentForger{})))
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Empty(t, liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])
		assert.Equal(t, scalerBindingRaw, liveMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey])
	})
}

func TestModelDeploymentForger_RuntimeHoldsAndBlockedConditions(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 30, 0, 0, time.UTC)

	cases := []struct {
		name       string
		mutateMD   func(*workercore.ModelDeployment)
		mutateMDF  func(*workercore.ModelDeploymentForger)
		extraObjs  func() []ctrlcli.Object
		wantReason string
	}{
		{
			name: "RolePaused when spec.roles[].pause is true",
			mutateMD: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Pause = true
			},
			wantReason: workercore.ModelDeploymentForgerReasonRolePaused,
		},
		{
			name: "RuntimeControlUnsynced when status.roles[].runtimeControl.synced is false",
			mutateMD: func(md *workercore.ModelDeployment) {
				md.Status.Roles = []workercore.ModelDeploymentRoleStatus{{
					Name: "decode",
					Kind: workercore.ModelDeploymentRoleKindServer,
					RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControlStatus{
						ObservedGeneration: 1,
						Synced:             false,
						Reason:             ModelDeploymentReasonRuntimeControlReconciling,
					},
				}}
			},
			wantReason: workercore.ModelDeploymentForgerReasonRuntimeControlUnsynced,
		},
		{
			name: "RetirementInProgress when md.Status.Retirement is active",
			mutateMD: func(md *workercore.ModelDeployment) {
				md.Status.Retirement = &workercore.ModelDeploymentRetirementStatus{
					RoleName:       "decode",
					ReplicaOrdinal: 1,
					State:          workercore.ModelDeploymentRetirementStateDraining,
				}
			},
			wantReason: workercore.ModelDeploymentForgerReasonRetirementInProgress,
		},
		{
			name: "PeerSyncDisabled when ModelDeployment reports KernelsReady False PeerSyncDisabled",
			mutateMD: func(md *workercore.ModelDeployment) {
				ModelDeploymentConditionKernelsReady.False(
					md, workercore.ModelDeploymentReasonKernelPeerSyncDisabled, "peer sync is disabled on node-1",
				)
			},
			wantReason: workercore.ModelDeploymentForgerReasonPeerSyncDisabled,
		},
		{
			name: "SandboxQuotaPending when ForgeSandbox stays unscheduled past 60s grace window",
			mutateMDF: func(mdf *workercore.ModelDeploymentForger) {
				started := meta.NewTime(now.Add(-90 * time.Second))
				mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name:             "decode",
					Phase:            workercore.ModelDeploymentForgerRolePhaseForging,
					ActiveGeneration: 1,
					ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
						PodName:   "qwen-decode-forger-decode-gen-1-sandbox",
						PodUID:    "sb-quota-uid",
						StartedAt: &started,
					},
				}}
			},
			extraObjs: func() []ctrlcli.Object {
				return []ctrlcli.Object{
					&core.Pod{
						ObjectMeta: meta.ObjectMeta{
							Namespace: "team-a",
							Name:      "qwen-decode-forger-decode-gen-1-sandbox",
							UID:       "sb-quota-uid",
						},
						Status: core.PodStatus{Phase: core.PodPending},
					},
				}
			},
			wantReason: workercore.ModelDeploymentForgerReasonSandboxQuotaPending,
		},
		{
			name: "PinnedCapacityExceeded when ModelKernelArtifact reports PinnedCapacityExceeded",
			mutateMDF: func(mdf *workercore.ModelDeploymentForger) {
				mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name:                 "decode",
					Phase:                workercore.ModelDeploymentForgerRolePhaseIdle,
					ActiveGeneration:     1,
					ActiveKernelArtifact: "qwen-decode-forger-decode-gen-1",
				}}
			},
			extraObjs: func() []ctrlcli.Object {
				mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "1", "a", func(k *workercore.ModelKernelArtifact) {
					k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
						Name: "qwen-decode-forger",
						UID:  "mdf-uid-1",
					}
					ModelKernelArtifactConditionPinned.False(
						k, workercore.ModelKernelArtifactReasonPinnedCapacityExceeded, "pin cap reached",
					)
				})
				return []ctrlcli.Object{mka}
			},
			wantReason: workercore.ModelDeploymentForgerReasonPinnedCapacityExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newForgerTestDeployment()
			if tc.mutateMD != nil {
				tc.mutateMD(md)
			}
			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			})
			if tc.mutateMDF != nil {
				tc.mutateMDF(mdf)
			}
			objs := []ctrlcli.Object{md, mdf}
			if tc.extraObjs != nil {
				objs = append(objs, tc.extraObjs()...)
			}
			cli := newForgerTestFakeClient(objs...)
			r := &ModelDeploymentForgerReconciler{
				Client:        cli,
				APIReader:     cli,
				Recorder:      ctrlrecord.NewFakeRecorder(32),
				Now:           func() time.Time { return now },
				OperatorImage: "gpustack/gpustack-operator:test",
			}
			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			var got workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
			blocked := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBlocked)
			require.NotNil(t, blocked)
			assert.Equal(t, meta.ConditionTrue, blocked.Status)
			assert.Equal(t, tc.wantReason, blocked.Reason)
		})
	}
}

func TestModelDeploymentForger_ObserveOnlyShadowAndSkipModes(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	sentinel := newMockSentinelServer(t, key1)
	now := time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)
	curNow := now

	t.Run("ObserveOnly gates on minActiveRequests distills non-GEMM bottlenecks and never creates sandbox", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Spec.Mode = workercore.ModelDeploymentForgerModeObserveOnly
			f.Spec.Probe.MinActiveRequests = 2
		})
		cli := newForgerTestFakeClient(node0, md, mdf)

		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		_, err := mdRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1)
		leader := pods.Items[0].DeepCopy()
		markServingPodReadyEligible(t, cli, leader, "node-0", sentinel.Host, sentinel.Port)

		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return curNow },
			OperatorImage: "gpustack/gpustack-operator:test",
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// Finalizer pass.
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		// Running requests (1) < MinActiveRequests (2) -> stays Idle without probing!
		sentinel.mu.Lock()
		sentinel.RunningReqs = 1
		beforeStarts := sentinel.ProbeStartCalls
		sentinel.mu.Unlock()

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		sentinel.mu.Lock()
		assert.Equal(t, beforeStarts, sentinel.ProbeStartCalls, "must wait for minActiveRequests before arming probe")
		sentinel.RunningReqs = 4
		sentinel.mu.Unlock()

		// Traffic >= 2 -> Pass 1 arms probe (Probing), Pass 2 collects distilled bottlenecks and returns to Idle.
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		var mid workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &mid))
		rs := findForgerRoleStatus(&mid, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseProbing, rs.Phase)

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &mid))
		rs = findForgerRoleStatus(&mid, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
		require.Len(t, rs.ObservedBottlenecks, 1, "dense GEMM bottleneck must be filtered out")
		assert.Equal(t, key1, rs.ObservedBottlenecks[0].LocalKernelKey)
		assert.Equal(t, modelkernelkey.OpFamilyRMSNorm, rs.ObservedBottlenecks[0].OpFamily)

		// Verify zero ForgeSandbox Pods or ConfigMaps were created and ModelDeployment was not mutated.
		var allPods core.PodList
		require.NoError(t, cli.List(context.Background(), &allPods, ctrlcli.InNamespace(md.Namespace)))
		assert.Len(t, allPods.Items, 1, "ObserveOnly must never create ForgeSandbox Pods")
		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Nil(t, liveMD.Spec.Roles[0].KernelArtifactRef)

		// Cooldown check: reconciling again before 600s does not arm a second probe.
		curNow = curNow.Add(60 * time.Second)
		sentinel.mu.Lock()
		startsAfterFirst := sentinel.ProbeStartCalls
		sentinel.mu.Unlock()
		res, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.Greater(t, res.RequeueAfter, 500*time.Second)
		sentinel.mu.Lock()
		assert.Equal(t, startsAfterFirst, sentinel.ProbeStartCalls, "cooldownSeconds must hold subsequent probes")
		sentinel.mu.Unlock()
	})

	t.Run("Shadow publishes verified ModelKernelArtifact without mutating ModelDeployment and skips covered keys next pass", func(t *testing.T) {
		curNow = now
		tokenFile := filepath.Join(t.TempDir(), "ingest-token")
		require.NoError(t, os.WriteFile(tokenFile, []byte("projected-ingest-jwt"), 0o600))

		node0 := newForgerTestNode("node-0")
		nms0 := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-0"},
			Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
		}
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
		})
		cli := newForgerTestFakeClient(node0, nms0, md, mdf)

		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		_, err := mdRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1)
		leader := pods.Items[0].DeepCopy()
		markServingPodReadyEligible(t, cli, leader, "node-0", sentinel.Host, sentinel.Port)

		manifestDigest := "sha256:" + strings.Repeat("b", 64)
		dispatchDigest := "sha256:" + strings.Repeat("c", 64)

		mmTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer projected-ingest-jwt", r.Header.Get("Authorization"))
			var ireq modelstore.IngestTreeRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&ireq))

			// Verify activeSandbox.podUID was persisted in ModelDeploymentForger.status BEFORE ingest!
			var checkMDF workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Namespace: ireq.Namespace, Name: ireq.ForgerName}, &checkMDF))
			rs := findForgerRoleStatus(&checkMDF, "decode")
			require.NotNil(t, rs)
			require.NotNil(t, rs.ActiveSandbox, "activeSandbox must be persisted before calling /ingest/v1/trees")
			assert.Equal(t, ireq.PodUID, rs.ActiveSandbox.PodUID)

			var storeObj workercore.NodeModelStore
			require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "node-0"}, &storeObj))
			storeObj.Status.Models = append(storeObj.Status.Models, workercore.NodeModelStoreModel{
				Digest:    manifestDigest,
				State:     workercore.NodeModelStoreModelStateReady,
				SizeBytes: 8192,
				Source:    workercore.NodeModelStoreModelSourceForge,
			})
			require.NoError(t, cli.Status().Update(context.Background(), &storeObj))

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(modelstore.IngestTreeResponse{
				ManifestDigest:         manifestDigest,
				DispatchManifestDigest: dispatchDigest,
				SizeBytes:              8192,
				FileCount:              4,
				SeedNode:               "node-0",
			})
		}))
		defer mmTLS.Close()
		mmHost, mmPortStr, err := net.SplitHostPort(strings.TrimPrefix(mmTLS.URL, "https://"))
		require.NoError(t, err)
		mmPort, err := strconv.Atoi(mmPortStr)
		require.NoError(t, err)

		ingestCli, err := modelforger.NewIngestClient(
			modelforger.WithIngestTokenFile(tokenFile),
			modelforger.WithIngestInsecureSkipVerify(true),
		)
		require.NoError(t, err)

		mkaRec := &ModelKernelArtifactReconciler{Client: cli, Recorder: ctrlrecord.NewFakeRecorder(32), Now: func() time.Time { return curNow }}
		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return curNow },
			IngestClient:  ingestCli,
			OperatorImage: "gpustack/gpustack-operator:test",
			ResolveSeedEndpoint: func(context.Context, string) (string, int, error) {
				return mmHost, mmPort, nil
			},
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// Finalizer -> Probing -> Forging.
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var mid workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &mid))
		rs := findForgerRoleStatus(&mid, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseForging, rs.Phase)
		require.NotNil(t, rs.ActiveSandbox)
		sbPodKey := types.NamespacedName{Namespace: md.Namespace, Name: rs.ActiveSandbox.PodName}
		sbCMKey := types.NamespacedName{Namespace: md.Namespace, Name: rs.ActiveSandbox.ConfigMapName}

		// Runner exits 0, verifier running -> Verifying.
		var sbPod core.Pod
		require.NoError(t, cli.Get(context.Background(), sbPodKey, &sbPod))
		sbPod.Spec.NodeName = "node-0"
		require.NoError(t, cli.Update(context.Background(), &sbPod))
		require.NoError(t, cli.Get(context.Background(), sbPodKey, &sbPod))
		sbPod.Status.Phase = core.PodPending
		sbPod.Status.InitContainerStatuses = []core.ContainerStatus{
			{Name: modelforger.RunnerContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
			{Name: modelforger.VerifierContainerName, State: core.ContainerState{Running: &core.ContainerStateRunning{}}},
		}
		require.NoError(t, cli.Status().Update(context.Background(), &sbPod))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &mid))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseVerifying, findForgerRoleStatus(&mid, "decode").Phase)

		// Verifier exits 0 -> Publishing + Ingest + ModelKernelArtifact created + Sandbox Pod/CM deleted.
		require.NoError(t, cli.Get(context.Background(), sbPodKey, &sbPod))
		sbPod.Status.Phase = core.PodRunning
		sbPod.Status.InitContainerStatuses[1].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}
		require.NoError(t, cli.Status().Update(context.Background(), &sbPod))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		mkaKey := types.NamespacedName{Namespace: md.Namespace, Name: "qwen-decode-forger-decode-gen-1"}
		var createdMKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), mkaKey, &createdMKA))
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), sbPodKey, &core.Pod{})), "sandbox Pod must be deleted after ingest")
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), sbCMKey, &core.ConfigMap{})), "input ConfigMap must be deleted after ingest")

		// Reconcile ModelPrefetch + ModelKernelArtifact -> Resolved=True, nodes.ready=1, Pinned=True -> Shadow pass completes without touching ModelDeployment!
		pfRec := &ModelPrefetchReconciler{Client: cli, Recorder: ctrlrecord.NewFakeRecorder(32), Now: func() time.Time { return curNow }}
		_, err = pfRec.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeModelStorePinnedRequestName}})
		require.NoError(t, err)
		_, err = mkaRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: mkaKey})
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &mid))
		rs = findForgerRoleStatus(&mid, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
		assert.Equal(t, mkaKey.Name, rs.ActiveKernelArtifact)
		require.Len(t, rs.Generations, 1)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomePromoted, rs.Generations[0].Outcome)

		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Nil(t, liveMD.Spec.Roles[0].KernelArtifactRef, "Shadow mode must NEVER mutate ModelDeployment")

		// Advance past cooldown with identical bottleneck -> skipped as NoEligibleBottlenecks!
		curNow = curNow.Add(700 * time.Second)
		skippedBefore := testutil.ToFloat64(ForgerPromotionsTotal.WithLabelValues(forgerPromotionOutcomeSkipped))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &mid))
		rs = findForgerRoleStatus(&mid, "decode")
		require.Len(t, rs.Generations, 2)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeSkipped, rs.Generations[1].Outcome)
		assert.Equal(t, workercore.ModelDeploymentForgerReasonNoEligibleBottlenecks, rs.Generations[1].Reason)
		assert.Equal(t, skippedBefore+1, testutil.ToFloat64(ForgerPromotionsTotal.WithLabelValues(forgerPromotionOutcomeSkipped)))
	})
}

func TestModelDeploymentForger_SandboxAndPublishFailures(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		mutatePod  func(*core.Pod)
		ingestCode int
		sourceLost bool
		wantReason string
	}{
		{
			name: "forge-runner non-zero exit records RunnerFailed and deletes sandbox",
			mutatePod: func(p *core.Pod) {
				p.Spec.NodeName = "node-0"
				p.Status.Phase = core.PodFailed
				p.Status.InitContainerStatuses = []core.ContainerStatus{{
					Name:  modelforger.RunnerContainerName,
					State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1, Message: "synth crash"}},
				}}
			},
			wantReason: "RunnerFailed",
		},
		{
			name: "forge-verifier non-zero exit records VerificationFailed and deletes sandbox",
			mutatePod: func(p *core.Pod) {
				p.Spec.NodeName = "node-0"
				p.Status.Phase = core.PodFailed
				p.Status.InitContainerStatuses = []core.ContainerStatus{
					{Name: modelforger.RunnerContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
					{Name: modelforger.VerifierContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 2, Message: "atol exceeded"}}},
				}
			},
			wantReason: "VerificationFailed",
		},
		{
			name: "ingest 422 invalid output records IngestRejected and deletes sandbox",
			mutatePod: func(p *core.Pod) {
				p.Spec.NodeName = "node-0"
				p.Status.Phase = core.PodRunning
				p.Status.InitContainerStatuses = []core.ContainerStatus{
					{Name: modelforger.RunnerContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
					{Name: modelforger.VerifierContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
				}
			},
			ingestCode: http.StatusUnprocessableEntity,
			wantReason: "IngestRejected",
		},
		{
			name:       "ModelKernelArtifact SourceLost during Publishing fails cleanly for re-forging",
			sourceLost: true,
			wantReason: workercore.ModelKernelArtifactReasonSourceLost,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newForgerTestDeployment()
			started := meta.NewTime(now.Add(-30 * time.Second))
			var extra []ctrlcli.Object
			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
			})

			if tc.sourceLost {
				mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name:             "decode",
					Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
					ActiveGeneration: 1,
				}}
				mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "9", "8", func(k *workercore.ModelKernelArtifact) {
					k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
						Name: mdf.Name,
						UID:  string(mdf.UID),
					}
					ModelKernelArtifactConditionResolved.False(k, workercore.ModelKernelArtifactReasonSourceLost, "seed node lost")
				})
				extra = append(extra, mka)
			} else {
				cm, _, err := modelforger.BuildInputConfigMap(modelforger.InputConfigMapSpec{
					Forger:     mdf,
					Name:       "qwen-decode-forger-decode-gen-1-input",
					RoleName:   "decode",
					Generation: 1,
					Request: modelforger.ForgeRequest{
						Engine:           "vLLM",
						EngineVersion:    "0.19.0",
						Manufacturer:     nodefeature.ManufacturerNVIDIA,
						Architecture:     "sm_90a",
						TargetKernelKeys: []string{buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)},
						MinSpeedupRatio:  1.05,
						Atol:             1e-2,
						Rtol:             1e-2,
					},
				})
				require.NoError(t, err)
				pod := &core.Pod{
					ObjectMeta: meta.ObjectMeta{
						Namespace: "team-a",
						Name:      "qwen-decode-forger-decode-gen-1-sandbox",
						UID:       "sb-fail-uid",
					},
				}
				tc.mutatePod(pod)
				mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name:             "decode",
					Phase:            workercore.ModelDeploymentForgerRolePhaseForging,
					ActiveGeneration: 1,
					ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
						PodName:       pod.Name,
						PodUID:        string(pod.UID),
						NodeName:      pod.Spec.NodeName,
						ConfigMapName: cm.Name,
						StartedAt:     &started,
					},
				}}
				extra = append(extra, cm, pod)
			}

			cli := newForgerTestFakeClient(append([]ctrlcli.Object{md, mdf}, extra...)...)
			r := &ModelDeploymentForgerReconciler{
				Client:        cli,
				APIReader:     cli,
				Recorder:      ctrlrecord.NewFakeRecorder(32),
				Now:           func() time.Time { return now },
				OperatorImage: "gpustack/gpustack-operator:test",
			}
			if tc.ingestCode != 0 {
				tokenFile := filepath.Join(t.TempDir(), "tok")
				require.NoError(t, os.WriteFile(tokenFile, []byte("tok"), 0o600))
				mmSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.ingestCode)
					_ = json.NewEncoder(w).Encode(map[string]string{"reason": "InvalidSandboxOutput", "error": "bad output"})
				}))
				defer mmSrv.Close()
				h, pStr, _ := net.SplitHostPort(strings.TrimPrefix(mmSrv.URL, "https://"))
				pInt, _ := strconv.Atoi(pStr)
				ic, err := modelforger.NewIngestClient(modelforger.WithIngestTokenFile(tokenFile), modelforger.WithIngestInsecureSkipVerify(true))
				require.NoError(t, err)
				r.IngestClient = ic
				r.ResolveSeedEndpoint = func(context.Context, string) (string, int, error) { return h, pInt, nil }
			}

			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			var got workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
			rs := findForgerRoleStatus(&got, "decode")
			require.NotNil(t, rs)
			assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
			assert.Nil(t, rs.ActiveSandbox)
			require.Len(t, rs.Generations, 1)
			assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeFailed, rs.Generations[0].Outcome)
			assert.Equal(t, tc.wantReason, rs.Generations[0].Reason)
		})
	}
}

func TestModelDeploymentForger_MultiControllerClosedLoopIntegration(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	key2 := buildTestKernelKey(t, modelkernelkey.OpFamilyRopeKVAppend, 4096)
	key3 := buildTestKernelKey(t, modelkernelkey.OpFamilyActivationQuant, 4096)

	sentinel := newMockSentinelServer(t, key1)
	curNow := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	tokenFile := filepath.Join(t.TempDir(), "projected-ingest-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("worker-ingest-jwt"), 0o600))

	node0 := newForgerTestNode("node-0")
	node1 := newForgerTestNode("node-1")
	nms0 := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-0"},
		Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
	}
	nms1 := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
		Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
	}

	md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
		d.Spec.Roles[0].Replicas = 2
	})
	mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
		f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
		f.Spec.Rollout.MaxRetainedGenerations = 1
		f.Spec.Rollout.FailureTimeoutSeconds = 300
	})
	scaler := &workercore.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  "team-a",
			Name:       "qwen-scaler",
			UID:        "scaler-uid-coexist",
			Generation: 1,
		},
		Spec: workercore.ModelDeploymentScalerSpec{
			TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: md.Name},
			Mode:      workercore.ModelDeploymentScalerModeActive,
			Roles: []workercore.ModelDeploymentScalerRoleBounds{{
				Name:        "decode",
				MinReplicas: 2,
				MaxReplicas: 3,
			}},
			Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeProportional,
				Proportional: &workercore.ModelDeploymentScalerProportionalSpec{
					Metric: workercore.ModelDeploymentScalerProportionalMetricRunningRequests,
					RoleTargets: []workercore.ModelDeploymentScalerRoleTarget{
						{Name: "decode", Target: resource.MustParse("1")},
					},
				},
			},
		},
	}

	var mmSeq atomic.Int32
	var cli ctrlcli.Client

	mmTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer worker-ingest-jwt", r.Header.Get("Authorization"))
		var ireq modelstore.IngestTreeRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&ireq))

		var checkMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Namespace: ireq.Namespace, Name: ireq.ForgerName}, &checkMDF))
		rs := findForgerRoleStatus(&checkMDF, "decode")
		require.NotNil(t, rs)
		require.NotNil(t, rs.ActiveSandbox)
		assert.Equal(t, ireq.PodUID, rs.ActiveSandbox.PodUID)

		idx := int(mmSeq.Add(1))
		manifestDigest := fmt.Sprintf("sha256:%064x", idx)
		dispatchDigest := fmt.Sprintf("sha256:%064x", idx+100)

		var storeObj workercore.NodeModelStore
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "node-0"}, &storeObj))
		storeObj.Status.Models = append(storeObj.Status.Models, workercore.NodeModelStoreModel{
			Digest:    manifestDigest,
			State:     workercore.NodeModelStoreModelStateReady,
			SizeBytes: 16384,
			Source:    workercore.NodeModelStoreModelSourceForge,
		})
		require.NoError(t, cli.Status().Update(context.Background(), &storeObj))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(modelstore.IngestTreeResponse{
			ManifestDigest:         manifestDigest,
			DispatchManifestDigest: dispatchDigest,
			SizeBytes:              16384,
			FileCount:              4,
			SeedNode:               "node-0",
		})
	}))
	defer mmTLS.Close()
	mmHost, mmPortStr, err := net.SplitHostPort(strings.TrimPrefix(mmTLS.URL, "https://"))
	require.NoError(t, err)
	mmPort, err := strconv.Atoi(mmPortStr)
	require.NoError(t, err)

	mmPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: kuberess.SystemNamespaceName,
			Name:      "gpustack-model-manager-node-0",
			Labels: map[string]string{
				deviceplugin.ComponentLabelKey: modelstore.PluginComponent,
			},
		},
		Spec: core.PodSpec{
			NodeName: "node-0",
			Containers: []core.Container{{
				Name: "main",
				Ports: []core.ContainerPort{{
					Name:          modelstore.PluginSecurePortName,
					ContainerPort: int32(mmPort),
				}},
			}},
		},
		Status: core.PodStatus{
			Phase:      core.PodRunning,
			PodIP:      mmHost,
			Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}},
		},
	}

	cli = newForgerTestFakeClient(node0, node1, nms0, nms1, mmPod, md, mdf, scaler)

	ingestCli, err := modelforger.NewIngestClient(
		modelforger.WithIngestTokenFile(tokenFile),
		modelforger.WithIngestInsecureSkipVerify(true),
	)
	require.NoError(t, err)

	resolvePodSentinelTarget := func(_ context.Context, _ *workercore.ModelDeployment, _ *workercore.ModelDeploymentRole, pod *core.Pod) (runtimecontrol.Target, error) {
		return runtimecontrol.Target{Scheme: "http", Host: sentinel.Host, Port: sentinel.Port}, nil
	}

	mdRec := &ModelDeploymentReconciler{
		Client:               cli,
		APIReader:            cli,
		Recorder:             ctrlrecord.NewFakeRecorder(128),
		clock:                func() time.Time { return curNow },
		runtimeControlTarget: resolvePodSentinelTarget,
		KernelRuntimeReader: func(ctx context.Context, _ *workercore.ModelDeployment, _ *workercore.ModelDeploymentRole, pod *core.Pod, _ runtimecontrol.Target) (*ModelDeploymentKernelRuntimeState, error) {
			artifactName := ""
			if src := modelDeploymentPodKernelVolumeSource(pod); src != nil && src.CSI != nil {
				artifactName = src.CSI.VolumeAttributes[modelstore.VolumeAttrArtifact]
			}
			sentinel.mu.Lock()
			st, ok := sentinel.PodRuntimeByKernel[artifactName]
			sentinel.mu.Unlock()
			if ok {
				return &ModelDeploymentKernelRuntimeState{
					BootID:                 st.BootID,
					Ready:                  st.Ready,
					DispatchManifestDigest: st.DispatchManifestDigest,
					LoadedKernelKeys:       slices.Clone(st.LoadedKernelKeys),
					KernelLoadError:        st.KernelLoadError,
				}, nil
			}
			return &ModelDeploymentKernelRuntimeState{BootID: sentinel.BootID, Ready: true}, nil
		},
	}
	mkaRec := &ModelKernelArtifactReconciler{
		Client:   cli,
		Recorder: ctrlrecord.NewFakeRecorder(64),
		Now:      func() time.Time { return curNow },
	}
	pfRec := &ModelPrefetchReconciler{
		Client:   cli,
		Recorder: ctrlrecord.NewFakeRecorder(64),
		Now:      func() time.Time { return curNow },
	}
	scalerRec := &ModelDeploymentScalerReconciler{
		Client:    cli,
		APIReader: cli,
		Recorder:  ctrlrecord.NewFakeRecorder(64),
		clock:     func() time.Time { return curNow },
		sampler:   &modelDeploymentScalerSampler{HTTPClient: defaultModelDeploymentScalerHTTPClient},
	}
	forgerRec := &ModelDeploymentForgerReconciler{
		Client:        cli,
		APIReader:     cli,
		Recorder:      ctrlrecord.NewFakeRecorder(128),
		Now:           func() time.Time { return curNow },
		IngestClient:  ingestCli,
		OperatorImage: "gpustack/gpustack-operator:test",
		ResolveSentinelTarget: func(_ context.Context, _ *workercore.ModelDeployment, _ *workercore.ModelDeploymentRole, pod *core.Pod) (runtimecontrol.Target, error) {
			artifactName := ""
			if src := modelDeploymentPodKernelVolumeSource(pod); src != nil && src.CSI != nil {
				artifactName = src.CSI.VolumeAttributes[modelstore.VolumeAttrArtifact]
			}
			sentinel.mu.Lock()
			if st, ok := sentinel.PodRuntimeByKernel[artifactName]; ok {
				sentinel.DefaultDispatchDigest = st.DispatchManifestDigest
				sentinel.DefaultLoadedKeys = slices.Clone(st.LoadedKernelKeys)
				sentinel.DefaultLoadError = st.KernelLoadError
			} else {
				sentinel.DefaultDispatchDigest = ""
				sentinel.DefaultLoadedKeys = nil
				sentinel.DefaultLoadError = ""
			}
			sentinel.mu.Unlock()
			return runtimecontrol.Target{Scheme: "http", Host: sentinel.Host, Port: sentinel.Port}, nil
		},
	}

	mdReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)}
	mdfReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
	scalerReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(scaler)}
	pinReq := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeModelStorePinnedRequestName}}

	admitAllDeploymentPods := func(readyNewPods bool) {
		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), mdReq.NamespacedName, &liveMD))
		var podList core.PodList
		require.NoError(t, cli.List(context.Background(), &podList,
			ctrlcli.InNamespace(md.Namespace),
			ctrlcli.MatchingLabels{modelDeploymentLabelKeyName: modelDeploymentLabelValueName},
		))
		for i := range podList.Items {
			p := podList.Items[i].DeepCopy()
			if p.DeletionTimestamp != nil {
				continue
			}
			ord, ok := modelDeploymentPodOrdinal(p)
			if !ok {
				continue
			}
			nodeName := "node-0"
			if ord%2 == 1 {
				nodeName = "node-1"
			}
			if readyNewPods && !podIsReady(p) {
				markServingPodReadyEligible(t, cli, p, nodeName, sentinel.Host, sentinel.Port)
			}
			wl := admit(composeReplicaGroup(&liveMD, "decode", ord, []*core.Pod{p}))
			var existingWL kueue.Workload
			if err := cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(wl), &existingWL); kerrors.IsNotFound(err) {
				require.NoError(t, cli.Create(context.Background(), wl))
			} else if err == nil {
				wl.ResourceVersion = existingWL.ResourceVersion
				wl.UID = existingWL.UID
				_ = cli.Update(context.Background(), wl)
				_ = cli.Status().Update(context.Background(), wl)
			}
		}
	}

	completeActiveSandbox := func() {
		var curMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), mdfReq.NamespacedName, &curMDF))
		rs := findForgerRoleStatus(&curMDF, "decode")
		require.NotNil(t, rs)
		require.NotNil(t, rs.ActiveSandbox)
		sbKey := types.NamespacedName{Namespace: md.Namespace, Name: rs.ActiveSandbox.PodName}
		var sbPod core.Pod
		require.NoError(t, cli.Get(context.Background(), sbKey, &sbPod))
		sbPod.Spec.NodeName = "node-0"
		require.NoError(t, cli.Update(context.Background(), &sbPod))
		require.NoError(t, cli.Get(context.Background(), sbKey, &sbPod))
		sbPod.Status.Phase = core.PodRunning
		sbPod.Status.InitContainerStatuses = []core.ContainerStatus{
			{Name: modelforger.RunnerContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
			{Name: modelforger.VerifierContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
		}
		require.NoError(t, cli.Status().Update(context.Background(), &sbPod))
	}

	// Bring up initial 2 stock replicas + bind ModelDeploymentScaler simultaneously.
	_, err = mdRec.Reconcile(context.Background(), mdReq)
	require.NoError(t, err)
	admitAllDeploymentPods(true)
	_, err = mdRec.Reconcile(context.Background(), mdReq)
	require.NoError(t, err)

	_, err = scalerRec.Reconcile(context.Background(), scalerReq)
	require.NoError(t, err)
	_, err = scalerRec.Reconcile(context.Background(), scalerReq)
	require.NoError(t, err)

	// Step A: Drive gen-1 from Idle -> Probing -> Forging -> Publishing -> Promoting.
	for range 3 {
		_, err = forgerRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)
	}
	completeActiveSandbox()
	// Verify + Ingest + create gen-1 ModelKernelArtifact.
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)

	gen1Name := "qwen-decode-forger-decode-gen-1"
	gen1Key := types.NamespacedName{Namespace: md.Namespace, Name: gen1Name}
	_, err = pfRec.Reconcile(context.Background(), pinReq)
	require.NoError(t, err)
	_, err = mkaRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: gen1Key})
	require.NoError(t, err)

	var gen1MKA workercore.ModelKernelArtifact
	require.NoError(t, cli.Get(context.Background(), gen1Key, &gen1MKA))
	gen1Digest := gen1MKA.Spec.Source.NodeStore.ManifestDigest
	gen1Dispatch := gen1MKA.Spec.Source.NodeStore.DispatchManifestDigest

	sentinel.mu.Lock()
	sentinel.PodRuntimeByKernel[gen1Name] = runtimecontrol.RuntimeConfigState{
		Engine:                 "vllm",
		BootID:                 sentinel.BootID,
		Ready:                  true,
		DispatchManifestDigest: gen1Dispatch,
		LoadedKernelKeys:       []string{key1},
	}
	sentinel.mu.Unlock()

	// Promote gen-1 onto ModelDeployment (coexisting with scaler-binding!).
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)

	var liveMD workercore.ModelDeployment
	require.NoError(t, cli.Get(context.Background(), mdReq.NamespacedName, &liveMD))
	require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
	assert.Equal(t, gen1Name, liveMD.Spec.Roles[0].KernelArtifactRef.Name)
	assert.NotEmpty(t, liveMD.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey],
		"scaler-binding must coexist with forger-binding")
	assert.NotEmpty(t, liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])

	// Simulate concurrent ModelDeploymentScaler scaling Replicas 2 -> 3 mid-rollout!
	liveMD.Spec.Roles[0].Replicas = 3
	require.NoError(t, cli.Update(context.Background(), &liveMD))

	// Drive ModelDeployment readiness-gated canary rollout across all 3 replicas.
	for range 8 {
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)
		admitAllDeploymentPods(true)
	}
	_, err = pfRec.Reconcile(context.Background(), pinReq)
	require.NoError(t, err)
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)

	var curMDF workercore.ModelDeploymentForger
	require.NoError(t, cli.Get(context.Background(), mdfReq.NamespacedName, &curMDF))
	rs := findForgerRoleStatus(&curMDF, "decode")
	require.NotNil(t, rs)
	assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
	assert.Equal(t, gen1Name, rs.ActiveKernelArtifact)
	require.Len(t, rs.Generations, 1)
	assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomePromoted, rs.Generations[0].Outcome)

	var checkNMS0 workercore.NodeModelStore
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "node-0"}, &checkNMS0))
	assert.Contains(t, checkNMS0.Spec.Pinned, gen1Digest)

	// Step B: Advance clock past cooldown, forge gen-2, verify previousKernelArtifact pin protection during rollout,
	// then verify gen-1 pruning & unpinning once gen-2 converges (maxRetainedGenerations == 1).
	curNow = curNow.Add(700 * time.Second)
	sentinel.SetProbeSingleKey(key2, modelkernelkey.OpFamilyRopeKVAppend, "rope_kv_append_h4096")

	for range 2 {
		_, err = forgerRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)
	}
	completeActiveSandbox()
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)

	gen2Name := "qwen-decode-forger-decode-gen-2"
	gen2Key := types.NamespacedName{Namespace: md.Namespace, Name: gen2Name}
	_, err = pfRec.Reconcile(context.Background(), pinReq)
	require.NoError(t, err)
	_, err = mkaRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: gen2Key})
	require.NoError(t, err)

	var gen2MKA workercore.ModelKernelArtifact
	require.NoError(t, cli.Get(context.Background(), gen2Key, &gen2MKA))
	gen2Digest := gen2MKA.Spec.Source.NodeStore.ManifestDigest
	gen2Dispatch := gen2MKA.Spec.Source.NodeStore.DispatchManifestDigest

	sentinel.mu.Lock()
	sentinel.PodRuntimeByKernel[gen2Name] = runtimecontrol.RuntimeConfigState{
		Engine:                 "vllm",
		BootID:                 sentinel.BootID,
		Ready:                  true,
		DispatchManifestDigest: gen2Dispatch,
		LoadedKernelKeys:       []string{key2},
	}
	sentinel.mu.Unlock()

	// Start promoting gen-2 -> previousKernelArtifact must be gen-1, and BOTH digests must stay pinned!
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)
	_, err = pfRec.Reconcile(context.Background(), pinReq)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), mdfReq.NamespacedName, &curMDF))
	rs = findForgerRoleStatus(&curMDF, "decode")
	assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePromoting, rs.Phase)
	assert.Equal(t, gen1Name, rs.PreviousKernelArtifact)
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "node-0"}, &checkNMS0))
	assert.Contains(t, checkNMS0.Spec.Pinned, gen1Digest, "previousKernelArtifact must stay pinned during rollout")
	assert.Contains(t, checkNMS0.Spec.Pinned, gen2Digest)

	// Converge gen-2 across all 3 replicas -> gen-1 is pruned and unpinned!
	for range 10 {
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)
		admitAllDeploymentPods(true)
	}
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)
	_, err = mkaRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: gen1Key})
	require.NoError(t, err)
	_, err = pfRec.Reconcile(context.Background(), pinReq)
	require.NoError(t, err)

	assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), gen1Key, &workercore.ModelKernelArtifact{})),
		"retired gen-1 beyond maxRetainedGenerations=1 must be pruned and finalized")
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "node-0"}, &checkNMS0))
	assert.NotContains(t, checkNMS0.Spec.Pinned, gen1Digest, "pruned gen-1 digest must be unpinned from NodeModelStore")
	assert.Contains(t, checkNMS0.Spec.Pinned, gen2Digest)

	// Step C: Forge gen-3, simulate canary reporting unsupported_kernel_artifact -> auto-rollback to gen-2 + persistent quarantine!
	curNow = curNow.Add(700 * time.Second)
	sentinel.SetProbeSingleKey(key3, modelkernelkey.OpFamilyActivationQuant, "act_quant_h4096")

	for range 2 {
		_, err = forgerRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)
	}
	completeActiveSandbox()
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)

	gen3Name := "qwen-decode-forger-decode-gen-3"
	gen3Key := types.NamespacedName{Namespace: md.Namespace, Name: gen3Name}
	_, err = pfRec.Reconcile(context.Background(), pinReq)
	require.NoError(t, err)
	_, err = mkaRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: gen3Key})
	require.NoError(t, err)

	sentinel.mu.Lock()
	sentinel.PodRuntimeByKernel[gen3Name] = runtimecontrol.RuntimeConfigState{
		Engine:          "vllm",
		BootID:          sentinel.BootID,
		Ready:           false,
		KernelLoadError: "unsupported_kernel_artifact",
	}
	sentinel.mu.Unlock()

	// Promote gen-3 -> ModelDeployment creates canary replacement at highest ordinal (2) while keeping ordinals 0 & 1 on gen-2!
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)
	_, err = mdRec.Reconcile(context.Background(), mdReq)
	require.NoError(t, err)
	_, err = mdRec.Reconcile(context.Background(), mdReq)
	require.NoError(t, err)
	admitAllDeploymentPods(true)

	rollbacksBefore := testutil.ToFloat64(ForgerRollbacksTotal.WithLabelValues("UnsupportedKernelArtifact"))

	// Forger detects unsupported_kernel_artifact on canary -> reverts to gen-2 and quarantines gen-3!
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)

	require.NoError(t, cli.Get(context.Background(), mdReq.NamespacedName, &liveMD))
	require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
	assert.Equal(t, gen2Name, liveMD.Spec.Roles[0].KernelArtifactRef.Name,
		"auto-rollback must revert ModelDeployment kernelArtifactRef to previousKernelArtifact gen-2")

	var gen3MKA workercore.ModelKernelArtifact
	require.NoError(t, cli.Get(context.Background(), gen3Key, &gen3MKA))
	assert.True(t, gen3MKA.Status.Quarantined, "failed artifact must be persistently marked quarantined")
	assert.Equal(t, "UnsupportedKernelArtifact", gen3MKA.Status.QuarantineReason)

	require.NoError(t, cli.Get(context.Background(), mdfReq.NamespacedName, &curMDF))
	rs = findForgerRoleStatus(&curMDF, "decode")
	assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
	assert.Equal(t, gen2Name, rs.ActiveKernelArtifact)
	assert.Empty(t, rs.PreviousKernelArtifact)
	assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeRolledBack, rs.Generations[len(rs.Generations)-1].Outcome)
	assert.Equal(t, workercore.ModelDeploymentForgerReasonRollbackCompleted,
		findForgerCondition(&curMDF, workercore.ModelDeploymentForgerConditionTypeReady).Reason)
	assert.Equal(t, rollbacksBefore+1, testutil.ToFloat64(ForgerRollbacksTotal.WithLabelValues("UnsupportedKernelArtifact")))

	// Recreate ModelDeploymentForger with a brand-new UID: gen-3 quarantine persists and skips key3!
	require.NoError(t, cli.Delete(context.Background(), &curMDF))
	_, err = forgerRec.Reconcile(context.Background(), mdfReq)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), gen3Key, &gen3MKA))
	assert.True(t, gen3MKA.Status.Quarantined, "quarantine must persist across ModelDeploymentForger deletion")

	recreatedMDF := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
		f.UID = "mdf-uid-recreated-2"
	})
	require.NoError(t, cli.Create(context.Background(), recreatedMDF))
	for range 3 {
		_, err = forgerRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)
	}
	require.NoError(t, cli.Get(context.Background(), mdfReq.NamespacedName, &curMDF))
	rs = findForgerRoleStatus(&curMDF, "decode")
	require.NotNil(t, rs)
	require.NotEmpty(t, rs.Generations)
	assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeSkipped, rs.Generations[len(rs.Generations)-1].Outcome,
		"recreated forger must skip quarantined kernel key3")
}

func TestModelDeploymentForger_RolloutTimeoutRollbackToNilStockAndRestartRecovery(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	sentinel := newMockSentinelServer(t, key1)
	curNow := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

	t.Run("Canary readiness timeout rolls back to nil stock kernels and quarantines artifact", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "7", "8", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
		})
		nms0 := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-0"},
			Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: mka.Spec.Source.NodeStore.ManifestDigest,
					State:  workercore.NodeModelStoreModelStateReady,
				}},
			},
		}
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 2
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Rollout.FailureTimeoutSeconds = 120
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:             "decode",
				Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
				ActiveGeneration: 1,
			}}
		})

		cli := newForgerTestFakeClient(node0, nms0, mka, md, mdf)
		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		mdReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)}
		_, err := mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		for i := range pods.Items {
			p := pods.Items[i].DeepCopy()
			ord, _ := modelDeploymentPodOrdinal(p)
			markServingPodReadyEligible(t, cli, p, "node-0", sentinel.Host, sentinel.Port)
			require.NoError(t, cli.Create(context.Background(), admit(composeReplicaGroup(md, "decode", ord, []*core.Pod{p}))))
		}
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)

		forgerRec := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(64),
			Now:           func() time.Time { return curNow },
			OperatorImage: "gpustack/gpustack-operator:test",
		}
		mdfReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// Pass 1: Acquires binding and promotes gen-1 (previousKernelArtifact == "" stock!).
		_, err = forgerRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)
		_, err = forgerRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)

		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), mdReq.NamespacedName, &liveMD))
		require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
		assert.Equal(t, mka.Name, liveMD.Spec.Roles[0].KernelArtifactRef.Name)

		// ModelDeployment replaces ordinal 1 with an unready canary Pod (leave NotReady!).
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)

		// Advance clock past failureTimeoutSeconds (120s) on a BRAND-NEW controller instance (testing restart recovery of timeout!).
		curNow = curNow.Add(150 * time.Second)
		restartedRec := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(64),
			Now:           func() time.Time { return curNow },
			OperatorImage: "gpustack/gpustack-operator:test",
		}
		_, err = restartedRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), mdReq.NamespacedName, &liveMD))
		assert.Nil(t, liveMD.Spec.Roles[0].KernelArtifactRef, "rollback from stock must revert kernelArtifactRef to nil")
		decision, hasDecision, err := readForgerDecision(&liveMD)
		require.NoError(t, err)
		require.True(t, hasDecision)
		assert.Equal(t, "", decision.KernelArtifacts["decode"])

		var gotMKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(mka), &gotMKA))
		assert.True(t, gotMKA.Status.Quarantined)
		assert.Equal(t, "RolloutTimeout", gotMKA.Status.QuarantineReason)
	})

	t.Run("Controller restart recovers in-flight promotion from persisted status and forger-decision", func(t *testing.T) {
		curNow = time.Date(2026, 11, 27, 17, 0, 0, 0, time.UTC)
		node0 := newForgerTestNode("node-0")
		mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "5", "6", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
		})
		nms0 := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-0"},
			Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: mka.Spec.Source.NodeStore.ManifestDigest,
					State:  workercore.NodeModelStoreModelStateReady,
				}},
			},
		}

		bindingBytes, _ := json.Marshal(workercore.ModelDeploymentForgerBinding{
			ForgerName: "qwen-decode-forger",
			ForgerUID:  "mdf-uid-1",
			Term:       1,
		})
		decisionBytes, _ := json.Marshal(workercore.ModelDeploymentForgerDecision{
			DecisionID:      fmt.Sprintf("mdf-uid-1-1-1-%d", curNow.Add(-10*time.Second).UnixNano()),
			Term:            1,
			KernelArtifacts: map[string]string{"decode": mka.Name},
		})
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
			d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: mka.Name}
			d.Annotations = map[string]string{
				workercore.ModelDeploymentForgerBindingAnnotationKey:  string(bindingBytes),
				workercore.ModelDeploymentForgerDecisionAnnotationKey: string(decisionBytes),
			}
		})
		// Simulate crash right after writing ModelDeployment before status transitioned from Publishing to Promoting.
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
				TargetUID:  string(md.UID),
				TargetName: md.Name,
				Term:       1,
			}
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:             "decode",
				Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
				ActiveGeneration: 1,
			}}
		})

		cli := newForgerTestFakeClient(node0, nms0, mka, md, mdf)
		sentinel.mu.Lock()
		sentinel.DefaultDispatchDigest = mka.Spec.Source.NodeStore.DispatchManifestDigest
		sentinel.DefaultLoadedKeys = []string{key1}
		sentinel.DefaultLoadError = ""
		sentinel.mu.Unlock()

		mdRec := &ModelDeploymentReconciler{
			Client:    cli,
			APIReader: cli,
			runtimeControlTarget: func(context.Context, *workercore.ModelDeployment, *workercore.ModelDeploymentRole, *core.Pod) (runtimecontrol.Target, error) {
				return runtimecontrol.Target{Scheme: "http", Host: sentinel.Host, Port: sentinel.Port}, nil
			},
		}
		mdReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)}
		_, err := mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1)
		p0 := pods.Items[0].DeepCopy()
		markServingPodReadyEligible(t, cli, p0, "node-0", sentinel.Host, sentinel.Port)
		require.NoError(t, cli.Create(context.Background(), admit(composeReplicaGroup(md, "decode", 0, []*core.Pod{p0}))))
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)

		freshRec := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return curNow },
			OperatorImage: "gpustack/gpustack-operator:test",
			ResolveSentinelTarget: func(context.Context, *workercore.ModelDeployment, *workercore.ModelDeploymentRole, *core.Pod) (runtimecontrol.Target, error) {
				return runtimecontrol.Target{Scheme: "http", Host: sentinel.Host, Port: sentinel.Port}, nil
			},
		}
		mdfReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		_, err = freshRec.Reconcile(context.Background(), mdfReq)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), mdfReq.NamespacedName, &got))
		rs := findForgerRoleStatus(&got, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
		assert.Equal(t, mka.Name, rs.ActiveKernelArtifact)
		require.Len(t, rs.Generations, 1)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomePromoted, rs.Generations[0].Outcome)
	})
}

type stubProbeClient struct {
	readyResp         *runtimecontrol.ReadyResponse
	readyErr          error
	runtimeConfigResp *runtimecontrol.RuntimeConfigState
	runtimeConfigErr  error
	startResp         *runtimecontrol.ProbeResponse
	startErr          error
	getResp           *runtimecontrol.ProbeResponse
	getErr            error
	deleteCalls       int
}

func (s *stubProbeClient) GetReady(context.Context, runtimecontrol.Target) (*runtimecontrol.ReadyResponse, error) {
	return s.readyResp, s.readyErr
}

func (s *stubProbeClient) GetRuntimeConfig(context.Context, runtimecontrol.Target) (*runtimecontrol.RuntimeConfigState, error) {
	return s.runtimeConfigResp, s.runtimeConfigErr
}

func (s *stubProbeClient) StartProbe(context.Context, runtimecontrol.Target, runtimecontrol.ProbeRequest) (*runtimecontrol.ProbeResponse, error) {
	return s.startResp, s.startErr
}

func (s *stubProbeClient) GetProbe(context.Context, runtimecontrol.Target) (*runtimecontrol.ProbeResponse, error) {
	return s.getResp, s.getErr
}

func (s *stubProbeClient) DeleteProbe(context.Context, runtimecontrol.Target) (*runtimecontrol.ProbeResponse, error) {
	s.deleteCalls++
	return &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateAborted}, nil
}

func TestModelDeploymentForger_PredicatesAndWatchMappers(t *testing.T) {
	mdfPred := modelDeploymentForgerPredicate()
	baseMDF := newTestModelDeploymentForger()
	assert.True(t, mdfPred.Create(ctrlevent.CreateEvent{Object: baseMDF}))
	assert.True(t, mdfPred.Delete(ctrlevent.DeleteEvent{Object: baseMDF}))
	assert.True(t, mdfPred.Generic(ctrlevent.GenericEvent{Object: baseMDF}))
	assert.True(t, mdfPred.Update(ctrlevent.UpdateEvent{}))

	statusOnly := baseMDF.DeepCopy()
	statusOnly.Status.ObservedGeneration = 99
	assert.False(t, mdfPred.Update(ctrlevent.UpdateEvent{ObjectOld: baseMDF, ObjectNew: statusOnly}))

	genBumped := baseMDF.DeepCopy()
	genBumped.Generation++
	assert.True(t, mdfPred.Update(ctrlevent.UpdateEvent{ObjectOld: baseMDF, ObjectNew: genBumped}))

	finalizerChanged := baseMDF.DeepCopy()
	finalizerChanged.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
	assert.True(t, mdfPred.Update(ctrlevent.UpdateEvent{ObjectOld: baseMDF, ObjectNew: finalizerChanged}))

	delChanged := baseMDF.DeepCopy()
	nowMeta := meta.Now()
	delChanged.DeletionTimestamp = &nowMeta
	assert.True(t, mdfPred.Update(ctrlevent.UpdateEvent{ObjectOld: baseMDF, ObjectNew: delChanged}))

	podPred := modelDeploymentForgerPodPredicate()
	sbPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "sb-pod",
			Labels: map[string]string{
				modelforger.SandboxLabelName:     "model-deployment-forger",
				modelforger.SandboxLabelInstance: baseMDF.Name,
			},
		},
	}
	mdPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "md-pod",
			Labels: map[string]string{
				modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance: "qwen-pd",
			},
		},
	}
	unrelatedPod := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "other"}}
	assert.True(t, podPred.Create(ctrlevent.CreateEvent{Object: sbPod}))
	assert.True(t, podPred.Create(ctrlevent.CreateEvent{Object: mdPod}))
	assert.False(t, podPred.Create(ctrlevent.CreateEvent{Object: unrelatedPod}))

	wlPred := modelDeploymentForgerWorkloadPredicate()
	sbWL := &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "wl-sb",
			Labels: map[string]string{
				modelforger.SandboxLabelName:     "model-deployment-forger",
				modelforger.SandboxLabelInstance: baseMDF.Name,
			},
		},
	}
	mdWL := &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "wl-md",
			Labels:    map[string]string{modelDeploymentLabelKeyInstance: "qwen-pd"},
		},
	}
	otherWL := &kueue.Workload{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "wl-other"}}
	assert.True(t, wlPred.Create(ctrlevent.CreateEvent{Object: sbWL}))
	assert.True(t, wlPred.Create(ctrlevent.CreateEvent{Object: mdWL}))
	assert.False(t, wlPred.Create(ctrlevent.CreateEvent{Object: otherWL}))

	mdfOther := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
		f.Name = "other-forger"
		f.Spec.TargetRef.Name = "other-md"
	})
	md := newForgerTestDeployment()
	cli := newForgerTestFakeClient(baseMDF, mdfOther, md, sbPod, mdPod)
	r := &ModelDeploymentForgerReconciler{Client: cli}

	wantMDFReq := []ctrlreconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: baseMDF.Name}}}
	assert.Equal(t, wantMDFReq, r.mapTargetDeployment(context.Background(), md))
	assert.Equal(t, wantMDFReq, r.mapPod(context.Background(), sbPod))
	assert.Equal(t, wantMDFReq, r.mapPod(context.Background(), mdPod))
	assert.Nil(t, r.mapPod(context.Background(), unrelatedPod))

	nodeStoreMKA := newNodeStoreKernelArtifact("mka-ns", "team-a", "1", "2", func(k *workercore.ModelKernelArtifact) {
		k.Spec.Source.NodeStore.ForgerRef.Name = baseMDF.Name
	})
	assert.Equal(t, wantMDFReq, r.mapKernelArtifact(context.Background(), nodeStoreMKA))

	imgMKA := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "mka-img"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				Image: &workercore.ModelArtifactImageSource{Reference: "example.com/kernels:v1"},
			},
		},
	}
	fanoutReqs := r.mapKernelArtifact(context.Background(), imgMKA)
	assert.Len(t, fanoutReqs, 2)
	assert.Nil(t, r.mapKernelArtifact(context.Background(), unrelatedPod))

	podOwnedWL := &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{
			Namespace:       "team-a",
			Name:            "wl-owned",
			OwnerReferences: []meta.OwnerReference{{Kind: "Pod", Name: sbPod.Name}},
		},
	}
	assert.Equal(t, wantMDFReq, r.mapWorkload(context.Background(), podOwnedWL))
	assert.Equal(t, wantMDFReq, r.mapWorkload(context.Background(), sbWL))
	assert.Nil(t, r.mapWorkload(context.Background(), otherWL))
}

func TestModelDeploymentForger_ProbingEdgeCases(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)

	makeReadyEnv := func(t *testing.T, mdMutate func(*workercore.ModelDeployment), mdfMutate func(*workercore.ModelDeploymentForger)) (ctrlcli.Client, *workercore.ModelDeployment, *workercore.ModelDeploymentForger, *core.Pod) {
		t.Helper()
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
			if mdMutate != nil {
				mdMutate(d)
			}
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Probe.MinActiveRequests = 0
			if mdfMutate != nil {
				mdfMutate(f)
			}
		})
		cli := newForgerTestFakeClient(node0, md, mdf)
		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		_, err := mdRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1)
		p0 := pods.Items[0].DeepCopy()
		markServingPodReadyEligible(t, cli, p0, "node-0", "10.0.0.10", 8000)
		return cli, md, mdf, p0
	}

	cases := []struct {
		name           string
		probeCli       *stubProbeClient
		mdfMutate      func(*workercore.ModelDeploymentForger)
		mdMutate       func(*workercore.ModelDeployment)
		primeProbeAge  time.Duration
		wantPhase      workercore.ModelDeploymentForgerRolePhase
		wantGenReason  string
		wantRequeue    time.Duration
		wantDeleteCall int
	}{
		{
			name: "StartProbe cooldown error records metric and honors RetryAfter",
			probeCli: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startErr:  &runtimecontrol.ProbeCooldownError{RetryAfter: 17 * time.Second},
			},
			wantPhase:   workercore.ModelDeploymentForgerRolePhaseIdle,
			wantRequeue: 17 * time.Second,
		},
		{
			name: "StartProbe ErrRolePaused sets Blocked RolePaused",
			probeCli: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startErr:  runtimecontrol.ErrRolePaused,
			},
			wantPhase:   workercore.ModelDeploymentForgerRolePhaseIdle,
			wantRequeue: forgerIdleRetryInterval,
		},
		{
			name: "StartProbe transport error stays Idle and retries",
			probeCli: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startErr:  errors.New("dial tcp timeout"),
			},
			wantPhase:   workercore.ModelDeploymentForgerRolePhaseIdle,
			wantRequeue: forgerIdleRetryInterval,
		},
		{
			name: "Mid-probe RolePaused aborts probe via DeleteProbe and returns to Idle",
			probeCli: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
			},
			mdMutate: func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0].Pause = true
			},
			mdfMutate: func(f *workercore.ModelDeploymentForger) {
				f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name: "decode", Phase: workercore.ModelDeploymentForgerRolePhaseProbing, ActiveGeneration: 1,
				}}
			},
			primeProbeAge:  3 * time.Second,
			wantPhase:      workercore.ModelDeploymentForgerRolePhaseIdle,
			wantRequeue:    forgerIdleRetryInterval,
			wantDeleteCall: 1,
		},
		{
			name: "Running probe exceeding wall-clock timeout aborts and records ProbeTimeout",
			probeCli: &stubProbeClient{
				getResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateRunning, BootID: "b1"},
			},
			mdfMutate: func(f *workercore.ModelDeploymentForger) {
				f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name: "decode", Phase: workercore.ModelDeploymentForgerRolePhaseProbing, ActiveGeneration: 1,
				}}
			},
			primeProbeAge:  25 * time.Second,
			wantPhase:      workercore.ModelDeploymentForgerRolePhaseIdle,
			wantGenReason:  "ProbeTimeout",
			wantRequeue:    600 * time.Second,
			wantDeleteCall: 1,
		},
		{
			name: "Completed probe with nil payload records EmptyProbePayload",
			probeCli: &stubProbeClient{
				getResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, Payload: nil},
			},
			mdfMutate: func(f *workercore.ModelDeploymentForger) {
				f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name: "decode", Phase: workercore.ModelDeploymentForgerRolePhaseProbing, ActiveGeneration: 1,
				}}
			},
			primeProbeAge: 2 * time.Second,
			wantPhase:     workercore.ModelDeploymentForgerRolePhaseIdle,
			wantGenReason: "EmptyProbePayload",
			wantRequeue:   600 * time.Second,
		},
		{
			name: "ProbeStateFailed records ProbeFailed generation",
			probeCli: &stubProbeClient{
				getResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateFailed, Error: "cupti attach error"},
			},
			mdfMutate: func(f *workercore.ModelDeploymentForger) {
				f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name: "decode", Phase: workercore.ModelDeploymentForgerRolePhaseProbing, ActiveGeneration: 1,
				}}
			},
			primeProbeAge: 2 * time.Second,
			wantPhase:     workercore.ModelDeploymentForgerRolePhaseIdle,
			wantGenReason: "ProbeFailed",
			wantRequeue:   600 * time.Second,
		},
		{
			name: "Synchronous completed StartProbe immediately launches sandbox",
			probeCli: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startResp: &runtimecontrol.ProbeResponse{
					State:  runtimecontrol.ProbeStateCompleted,
					BootID: "b1",
					Payload: &runtimecontrol.ProbePayload{
						SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
						Engine:        "vLLM",
						EngineVersion: "0.19.0",
						Manufacturer:  nodefeature.ManufacturerNVIDIA,
						Architecture:  "sm_90a",
						Bottlenecks: []runtimecontrol.ProbeBottleneck{{
							LocalKernelKey:      key1,
							OpFamily:            modelkernelkey.OpFamilyRMSNorm,
							GPUTimeUs:           300,
							GPUTimeSharePercent: 20,
							CallCount:           10,
						}},
					},
				},
			},
			wantPhase:   workercore.ModelDeploymentForgerRolePhaseForging,
			wantRequeue: forgerDefaultPollInterval,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, _, mdf, p0 := makeReadyEnv(t, tc.mdMutate, tc.mdfMutate)
			r := &ModelDeploymentForgerReconciler{
				Client:        cli,
				Recorder:      ctrlrecord.NewFakeRecorder(32),
				Now:           func() time.Time { return now },
				ProbeClient:   tc.probeCli,
				OperatorImage: "gpustack/gpustack-operator:test",
			}
			if tc.primeProbeAge > 0 {
				r.rememberProbe(mdf.UID, "decode", forgerProbeAttempt{
					PodName:   p0.Name,
					PodUID:    string(p0.UID),
					BootID:    "b1",
					NodeName:  "node-0",
					StartedAt: now.Add(-tc.primeProbeAge),
				})
			}

			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			res, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, tc.wantRequeue, res.RequeueAfter)
			assert.Equal(t, tc.wantDeleteCall, tc.probeCli.deleteCalls)

			var got workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
			rs := findForgerRoleStatus(&got, "decode")
			require.NotNil(t, rs)
			assert.Equal(t, tc.wantPhase, rs.Phase)
			if tc.wantGenReason != "" {
				require.NotEmpty(t, rs.Generations)
				assert.Equal(t, tc.wantGenReason, rs.Generations[len(rs.Generations)-1].Reason)
			}
		})
	}
}

func TestModelDeploymentForger_SandboxAndPromotingEdgeCases(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	now := time.Date(2026, 11, 27, 19, 0, 0, 0, time.UTC)

	t.Run("Sandbox transitions Forging to Verifying and handles Timeout, Lost, UIDMismatch, and PodFailed", func(t *testing.T) {
		subcases := []struct {
			name           string
			age            time.Duration
			deletePod      bool
			mismatchUID    bool
			podFailed      bool
			runnerExitZero bool
			wantPhase      workercore.ModelDeploymentForgerRolePhase
			wantReason     string
		}{
			{
				name:           "Runner done while verifier still running advances phase to Verifying",
				age:            10 * time.Second,
				runnerExitZero: true,
				wantPhase:      workercore.ModelDeploymentForgerRolePhaseVerifying,
			},
			{
				name:       "Sandbox exceeding activeDeadlineSeconds records SandboxTimeout",
				age:        2000 * time.Second,
				wantPhase:  workercore.ModelDeploymentForgerRolePhaseIdle,
				wantReason: "SandboxTimeout",
			},
			{
				name:       "Deleted sandbox Pod records SandboxLost",
				age:        10 * time.Second,
				deletePod:  true,
				wantPhase:  workercore.ModelDeploymentForgerRolePhaseIdle,
				wantReason: "SandboxLost",
			},
			{
				name:        "Recreated sandbox Pod with mismatched UID records SandboxUIDMismatch",
				age:         10 * time.Second,
				mismatchUID: true,
				wantPhase:   workercore.ModelDeploymentForgerRolePhaseIdle,
				wantReason:  "SandboxUIDMismatch",
			},
			{
				name:       "PodFailed before init containers complete records SandboxFailed",
				age:        10 * time.Second,
				podFailed:  true,
				wantPhase:  workercore.ModelDeploymentForgerRolePhaseIdle,
				wantReason: "SandboxFailed",
			},
		}

		for _, tc := range subcases {
			t.Run(tc.name, func(t *testing.T) {
				md := newForgerTestDeployment()
				mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
					f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
					f.Spec.Sandbox.ActiveDeadlineSeconds = 1800
				})
				started := meta.NewTime(now.Add(-tc.age))
				pod := &core.Pod{
					ObjectMeta: meta.ObjectMeta{
						Namespace: md.Namespace,
						Name:      "sb-edge-pod",
						UID:       "sb-edge-uid",
					},
					Spec:   core.PodSpec{NodeName: "node-0"},
					Status: core.PodStatus{Phase: core.PodRunning},
				}
				if tc.runnerExitZero {
					pod.Status.InitContainerStatuses = []core.ContainerStatus{{
						Name:  modelforger.RunnerContainerName,
						State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}},
					}}
				}
				if tc.podFailed {
					pod.Status.Phase = core.PodFailed
					pod.Status.Message = "evicted by kubelet"
				}
				recordedUID := "sb-edge-uid"
				if tc.mismatchUID {
					recordedUID = "sb-old-uid"
				}
				mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name:             "decode",
					Phase:            workercore.ModelDeploymentForgerRolePhaseForging,
					ActiveGeneration: 1,
					ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
						PodName:   pod.Name,
						PodUID:    recordedUID,
						NodeName:  "node-0",
						StartedAt: &started,
					},
				}}

				objs := []ctrlcli.Object{md, mdf}
				if !tc.deletePod {
					objs = append(objs, pod)
				}
				cli := newForgerTestFakeClient(objs...)
				r := &ModelDeploymentForgerReconciler{
					Client:   cli,
					Recorder: ctrlrecord.NewFakeRecorder(32),
					Now:      func() time.Time { return now },
				}
				req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
				_, err := r.Reconcile(context.Background(), req)
				require.NoError(t, err)

				var got workercore.ModelDeploymentForger
				require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
				rs := findForgerRoleStatus(&got, "decode")
				require.NotNil(t, rs)
				assert.Equal(t, tc.wantPhase, rs.Phase)
				if tc.wantReason != "" {
					require.Len(t, rs.Generations, 1)
					assert.Equal(t, tc.wantReason, rs.Generations[0].Reason)
				}
			})
		}
	})

	t.Run("Promoting handles ArtifactMissing, external Quarantine, SourceLost, and multi-rank KernelLoadError", func(t *testing.T) {
		promoCases := []struct {
			name       string
			mkaMutate  func(*workercore.ModelKernelArtifact)
			omitMKA    bool
			rcState    *runtimecontrol.RuntimeConfigState
			wantReason string
		}{
			{
				name:       "Missing promoted artifact rolls back with ArtifactMissing",
				omitMKA:    true,
				wantReason: "ArtifactMissing",
			},
			{
				name: "Externally quarantined artifact rolls back immediately",
				mkaMutate: func(k *workercore.ModelKernelArtifact) {
					k.Status.Quarantined = true
					k.Status.QuarantineReason = "AdminQuarantine"
					k.Status.QuarantineMessage = "quarantined manually"
				},
				wantReason: "AdminQuarantine",
			},
			{
				name: "SourceLost mid-rollout rolls back and quarantines artifact",
				mkaMutate: func(k *workercore.ModelKernelArtifact) {
					ModelKernelArtifactConditionResolved.False(k, workercore.ModelKernelArtifactReasonSourceLost, "seed node gone")
				},
				wantReason: workercore.ModelKernelArtifactReasonSourceLost,
			},
			{
				name: "Multi-rank runtime_config kernel_load_error rolls back with KernelLoadError",
				rcState: &runtimecontrol.RuntimeConfigState{
					Engine: "vllm",
					BootID: "b1",
					Ready:  false,
					Ranks: []runtimecontrol.RankReceipt{
						{RankID: 0, BootID: "b1", LoadedKernelKeys: []string{key1}, DispatchManifestDigest: "sha256:" + strings.Repeat("8", 64)},
						{RankID: 1, BootID: "b1", KernelLoadError: "abi_symbol_not_found"},
					},
				},
				wantReason: "KernelLoadError",
			},
		}

		for _, tc := range promoCases {
			t.Run(tc.name, func(t *testing.T) {
				mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-2", "team-a", "7", "8", func(k *workercore.ModelKernelArtifact) {
					k.Spec.Compatibility.KernelKeys = []string{key1}
					if tc.mkaMutate != nil {
						tc.mkaMutate(k)
					}
				})
				prevMKA := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "1", "2")
				md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
					d.Spec.Roles[0].Replicas = 1
					d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: mka.Name}
				})
				mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
					f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
					f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
						Name:                   "decode",
						Phase:                  workercore.ModelDeploymentForgerRolePhasePromoting,
						ActiveGeneration:       2,
						ActiveKernelArtifact:   mka.Name,
						PreviousKernelArtifact: prevMKA.Name,
					}}
				})

				objs := []ctrlcli.Object{prevMKA, md, mdf}
				if !tc.omitMKA {
					objs = append(objs, mka)
				}
				if tc.rcState != nil {
					rkRender := buildModelDeploymentKernelRender(mka, kernelArtifactDeliveryKind(mka))
					vols, _ := rkRender.volumes(false)
					canaryPod := &core.Pod{
						ObjectMeta: meta.ObjectMeta{
							Namespace: md.Namespace,
							Name:      "qwen-pd-decode-0",
							Labels: map[string]string{
								modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
								modelDeploymentLabelKeyInstance:    md.Name,
								modelDeploymentLabelKeyComponent:   "decode",
								modelDeploymentReplicaOrdinalLabel: "0",
							},
						},
						Spec:   core.PodSpec{NodeName: "node-0", Volumes: vols},
						Status: core.PodStatus{Phase: core.PodRunning, PodIP: "10.0.0.11"},
					}
					objs = append(objs, canaryPod)
				}

				cli := newForgerTestFakeClient(objs...)
				r := &ModelDeploymentForgerReconciler{
					Client:   cli,
					Recorder: ctrlrecord.NewFakeRecorder(32),
					Now:      func() time.Time { return now },
					ProbeClient: &stubProbeClient{
						runtimeConfigResp: tc.rcState,
					},
				}

				req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
				_, err := r.Reconcile(context.Background(), req)
				require.NoError(t, err)

				var liveMD workercore.ModelDeployment
				require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
				require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
				assert.Equal(t, prevMKA.Name, liveMD.Spec.Roles[0].KernelArtifactRef.Name)

				var got workercore.ModelDeploymentForger
				require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
				rs := findForgerRoleStatus(&got, "decode")
				require.NotNil(t, rs)
				assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
				assert.Equal(t, prevMKA.Name, rs.ActiveKernelArtifact)
				require.Len(t, rs.Generations, 1)
				assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeRolledBack, rs.Generations[0].Outcome)
				assert.Equal(t, tc.wantReason, rs.Generations[0].Reason)
			})
		}
	})
}

func TestModelDeploymentForger_DefaultsResolversAndHistoryBounds(t *testing.T) {
	// 1. Long resource names stay within Kubernetes DNS label / subdomain limits.
	longForger := strings.Repeat("very-long-forger-name-", 12) + "end"
	longRole := strings.Repeat("decode-role-", 4) + "x"
	artName := forgerArtifactName(longForger, longRole, 42)
	assert.LessOrEqual(t, len(artName), 253)
	parsedGen, ok := parseForgerArtifactGeneration(longForger, longRole, artName)
	assert.True(t, ok)
	assert.Equal(t, int64(42), parsedGen)

	podName, cmName, err := newForgerSandboxObjectNames(longForger, longRole, 42)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(podName), forgerMaxKubeResourceNameLen)
	assert.LessOrEqual(t, len(cmName), forgerMaxKubeResourceNameLen)
	podName2, cmName2, err := newForgerSandboxObjectNames(longForger, longRole, 42)
	require.NoError(t, err)
	assert.NotEqual(t, podName, podName2)
	assert.NotEqual(t, cmName, cmName2)

	// 2. Bounded 16-entry generation ring buffer.
	rs := &workercore.ModelDeploymentForgerRoleStatus{Name: "decode"}
	for g := int64(1); g <= 22; g++ {
		appendForgerGenerationOutcome(rs, workercore.ModelDeploymentForgerGenerationStatus{
			Generation: g,
			Outcome:    workercore.ModelDeploymentForgerGenerationOutcomePromoted,
		})
	}
	assert.Len(t, rs.Generations, forgerMaxGenerationHistory)
	assert.Equal(t, int64(7), rs.Generations[0].Generation)
	assert.Equal(t, int64(22), rs.Generations[len(rs.Generations)-1].Generation)

	// 3. Default effective helpers on zero-valued specs.
	assert.Equal(t, workercore.ModelDeploymentForgerModeShadow, effectiveForgerMode(nil))
	assert.Equal(t, workercore.ModelDeploymentForgerRunnerModeExternal, effectiveRunnerMode(nil))
	assert.Equal(t, forgerDefaultProbeSteps, effectiveProbeSteps(nil))
	assert.Equal(t, int32(0), effectiveMinActiveRequests(&workercore.ModelDeploymentForger{
		Spec: workercore.ModelDeploymentForgerSpec{Probe: workercore.ModelDeploymentForgerProbeSpec{MinActiveRequests: -1}},
	}))
	assert.Equal(t, forgerDefaultMaxKernels, effectiveMaxKernelsPerGeneration(nil))
	assert.Equal(t, modelforger.DefaultMinSpeedupRatio, effectiveMinSpeedupRatio(nil))
	assert.Equal(t, modelforger.DefaultMinSpeedupRatio, effectiveMinSpeedupRatio(&workercore.ModelDeploymentForgerRoleSpec{MinSpeedupRatio: "bad"}))
	assert.Equal(t, int64(forgerDefaultSandboxTimeoutSeconds), effectiveSandboxTimeoutSeconds(nil))
	assert.Equal(t, int64(900), effectiveSandboxTimeoutSeconds(&workercore.ModelDeploymentForger{
		Spec: workercore.ModelDeploymentForgerSpec{
			Sandbox: workercore.ModelDeploymentForgerSandboxSpec{ActiveDeadlineSeconds: 900},
		},
	}))
	assert.Equal(t, forgerDefaultMaxRetainedGenerations, effectiveMaxRetainedGenerations(nil))
	assert.Equal(t, int32(forgerDefaultFailureTimeoutSeconds), effectiveFailureTimeoutSeconds(nil))

	// 4. Operator image, seed endpoint, sentinel target, and v1alpha1 AcceleratorType fallback resolvers.
	workerPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: kuberess.SystemNamespaceName,
			Name:      "gpustack-worker-0",
			Labels:    map[string]string{"app.kubernetes.io/component": "worker"},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{Name: "main", Image: "registry.local/gpustack-operator:v9.9.9"}},
		},
	}
	selfPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: kuberess.SystemNamespaceName,
			Name:      "self-worker-pod",
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{Name: "main", Image: "registry.local/gpustack-operator:self"}},
		},
	}
	v1alpha1OnlyAT := &workercore.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "custom-at"},
		Status: workercore.AcceleratorTypeStatus{
			Entrance: "custom-queue",
		},
	}
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(workerPod, selfPod, v1alpha1OnlyAT).
		Build()

	r := &ModelDeploymentForgerReconciler{Client: cli}
	assert.False(t, r.now().IsZero())
	ic1, err := r.ingestClient()
	require.NoError(t, err)
	ic2, err := r.ingestClient()
	require.NoError(t, err)
	assert.Same(t, ic1, ic2)

	img, err := r.resolveOperatorImage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "registry.local/gpustack-operator:v9.9.9", img)

	t.Setenv("KUBERNETES_POD_NAMESPACE", kuberess.SystemNamespaceName)
	t.Setenv("KUBERNETES_POD_NAME", "self-worker-pod")
	imgSelf, err := r.resolveOperatorImage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "registry.local/gpustack-operator:self", imgSelf)

	rHook := &ModelDeploymentForgerReconciler{
		Client:               ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		ResolveOperatorImage: func(context.Context) (string, error) { return "hook-img:v1", nil },
	}
	imgHook, err := rHook.resolveOperatorImage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "hook-img:v1", imgHook)

	rFallback := &ModelDeploymentForgerReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
	}
	t.Setenv("KUBERNETES_POD_NAME", "nonexistent")
	imgDefault, err := rFallback.resolveOperatorImage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, forgerDefaultOperatorImage, imgDefault)

	_, _, seedErr := r.resolveSeedEndpoint(context.Background(), "missing-node")
	assert.Error(t, seedErr)

	md := newForgerTestDeployment()
	httpsPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Annotations: map[string]string{
				"prometheus.io/port":   "8443",
				"prometheus.io/scheme": "https",
			},
		},
		Status: core.PodStatus{PodIP: "10.0.0.99"},
	}
	tgt, err := r.resolveSentinelTarget(context.Background(), md, &md.Spec.Roles[0], httpsPod)
	require.NoError(t, err)
	assert.Equal(t, "https", tgt.Scheme)
	assert.Equal(t, 8443, tgt.Port)
	assert.True(t, tgt.InsecureSkipVerify)

	// Confirm v1alpha1 AcceleratorType lookup when v1 object is absent.
	var checkV1 worker.AcceleratorType
	assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), types.NamespacedName{Name: "custom-at"}, &checkV1)))
	gotAT, err := r.getAcceleratorType(context.Background(), "custom-at")
	require.NoError(t, err)
	require.NotNil(t, gotAT)
	assert.Equal(t, "custom-queue", gotAT.Status.Entrance)
	nilAT, err := r.getAcceleratorType(context.Background(), "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, nilAT)
}

type stubIngestClient struct {
	resp *modelstore.IngestTreeResponse
	err  error
}

func (s *stubIngestClient) IngestTree(
	context.Context, string, int, modelstore.IngestTreeRequest,
) (*modelstore.IngestTreeResponse, error) {
	return s.resp, s.err
}

func TestModelDeploymentForger_LaunchPublishAndConvergenceContracts(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	now := time.Date(2026, 11, 27, 20, 0, 0, 0, time.UTC)

	t.Run("Oversized >512KiB sandbox input fails cleanly and pre-existing tenant ConfigMap/Pod cannot hijack activeSandbox", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
			d.Spec.Roles[0].Image = ""
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Probe.MinActiveRequests = 0
		})
		var collideNextPodCreate atomic.Bool
		const hijackedPodUID = types.UID("tenant-hijack-pod-uid")
		baseCli := newForgerTestFakeClient(node0, md, mdf)
		cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				if pod, isPod := obj.(*core.Pod); isPod && collideNextPodCreate.CompareAndSwap(true, false) {
					hijack := &core.Pod{
						ObjectMeta: meta.ObjectMeta{
							Namespace: pod.Namespace,
							Name:      pod.Name,
							UID:       hijackedPodUID,
						},
						Spec: core.PodSpec{NodeName: "node-0"},
					}
					_ = c.Create(ctx, hijack)
					return kerrors.NewAlreadyExists(core.Resource("pods"), pod.Name)
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		_, err := mdRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		p0 := pods.Items[0].DeepCopy()
		markServingPodReadyEligible(t, cli, p0, "node-0", "10.0.0.10", 8000)

		hugeRef := "def run(x, weight):\n    # " + strings.Repeat("A", 600*1024) + "\n    return x\n"
		hidden := int64(4096)
		oversizedPayload := &runtimecontrol.ProbePayload{
			SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
			Engine:        "vLLM",
			EngineVersion: "0.19.0",
			Manufacturer:  nodefeature.ManufacturerNVIDIA,
			Architecture:  "sm_90a",
			Definitions: []runtimecontrol.ProbeDefinition{{
				Name:           "huge_def",
				OpType:         modelkernelkey.OpFamilyRMSNorm,
				LocalKernelKey: key1,
				Axes:           map[string]runtimecontrol.ProbeAxisSpec{"num_tokens": {Type: "var"}, "hidden_size": {Type: "const", Value: &hidden}},
				Inputs:         map[string]runtimecontrol.ProbeTensorSpec{"x": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"}, "weight": {Shape: []string{"hidden_size"}, Dtype: "bfloat16"}},
				Outputs:        map[string]runtimecontrol.ProbeTensorSpec{"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"}},
				Reference:      hugeRef,
			}},
			Bottlenecks: []runtimecontrol.ProbeBottleneck{{
				LocalKernelKey:      key1,
				OpFamily:            modelkernelkey.OpFamilyRMSNorm,
				GPUTimeUs:           100,
				GPUTimeSharePercent: 25,
				CallCount:           4,
			}},
		}

		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
			ProbeClient: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: oversizedPayload},
			},
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		rs := findForgerRoleStatus(&got, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
		require.Len(t, rs.Generations, 1)
		assert.Equal(t, "InvalidSandboxInput", rs.Generations[0].Reason)

		// Advance past cooldown and pre-create predictable tenant ConfigMap & Pod to attempt adoption hijack.
		now = now.Add(700 * time.Second)
		tenantCMName := fmt.Sprintf("%s-decode-gen-2-input", mdf.Name)
		tenantPodName := fmt.Sprintf("%s-decode-gen-2-sandbox", mdf.Name)
		require.NoError(t, cli.Create(context.Background(), &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{Namespace: md.Namespace, Name: tenantCMName},
			Data:       map[string]string{"injected.py": "import os; os.system('id')"},
		}))
		tenantPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: md.Namespace,
				Name:      tenantPodName,
				UID:       "tenant-predictable-pod-uid",
			},
			Spec: core.PodSpec{NodeName: "node-0"},
		}
		require.NoError(t, cli.Create(context.Background(), tenantPod))

		validPayload := &runtimecontrol.ProbePayload{
			SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
			Engine:        "vLLM",
			EngineVersion: "0.19.0",
			Manufacturer:  nodefeature.ManufacturerNVIDIA,
			Architecture:  "sm_90a",
			Bottlenecks: []runtimecontrol.ProbeBottleneck{{
				LocalKernelKey:      key1,
				OpFamily:            modelkernelkey.OpFamilyRMSNorm,
				GPUTimeUs:           100,
				GPUTimeSharePercent: 25,
				CallCount:           4,
			}},
		}
		r.ProbeClient = &stubProbeClient{
			readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
			startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: validPayload},
		}

		// Force an AlreadyExists collision on the first sandbox Pod Create: controller must NOT adopt hijackedPodUID,
		// must delete the freshly created ConfigMap from the failed attempt, and must return an error to retry with a fresh nonce!
		collideNextPodCreate.Store(true)
		_, err = r.Reconcile(context.Background(), req)
		require.Error(t, err, "AlreadyExists on sandbox creation must return conflict error rather than adopting existing Pod")
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		rs = findForgerRoleStatus(&got, "decode")
		assert.Nil(t, rs.ActiveSandbox, "failed AlreadyExists attempt must never adopt pre-existing Pod into activeSandbox")

		var cmList core.ConfigMapList
		require.NoError(t, cli.List(context.Background(), &cmList, ctrlcli.InNamespace(md.Namespace)))
		for i := range cmList.Items {
			assert.Equal(t, tenantCMName, cmList.Items[i].Name, "freshly created ConfigMap from collided attempt must be deleted")
		}

		// Retry reconcile: succeeds with a fresh random-nonce ConfigMap and Pod, ignoring both tenant objects!
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		rs = findForgerRoleStatus(&got, "decode")
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseForging, rs.Phase)
		require.NotNil(t, rs.ActiveSandbox)
		assert.NotEqual(t, tenantPodName, rs.ActiveSandbox.PodName)
		assert.NotEqual(t, tenantCMName, rs.ActiveSandbox.ConfigMapName)
		assert.NotEqual(t, string(tenantPod.UID), rs.ActiveSandbox.PodUID)
		assert.NotEqual(t, string(hijackedPodUID), rs.ActiveSandbox.PodUID)
		assert.LessOrEqual(t, len(rs.ActiveSandbox.PodName), forgerMaxKubeResourceNameLen)
		assert.LessOrEqual(t, len(rs.ActiveSandbox.ConfigMapName), forgerMaxKubeResourceNameLen)
	})

	t.Run("Publishing waits for Pinned=True before promoting and advances once Pinned=True", func(t *testing.T) {
		mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "5", "6", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
			ModelKernelArtifactConditionPinned.False(k, workercore.ModelKernelArtifactReasonUnpinned, "waiting for NodeModelStore pin")
		})
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:             "decode",
				Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
				ActiveGeneration: 1,
			}}
		})

		cli := newForgerTestFakeClient(mka, md, mdf)
		r := &ModelDeploymentForgerReconciler{
			Client:    cli,
			APIReader: cli,
			Recorder:  ctrlrecord.NewFakeRecorder(32),
			Now:       func() time.Time { return now },
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// 1. Pinned=False (Unpinned): Publishing waits with forgerDefaultPollInterval and does NOT mutate ModelDeployment!
		res, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, forgerDefaultPollInterval, res.RequeueAfter)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePublishing, findForgerRoleStatus(&got, "decode").Phase)
		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Nil(t, liveMD.Spec.Roles[0].KernelArtifactRef, "must not promote unpinned ModelKernelArtifact")

		// 2. Pinned=False (PinnedCapacityExceeded): Publishing still waits and surfaces Blocked=True/PinnedCapacityExceeded!
		var liveMKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(mka), &liveMKA))
		ModelKernelArtifactConditionPinned.False(&liveMKA, workercore.ModelKernelArtifactReasonPinnedCapacityExceeded, "pin cap reached")
		require.NoError(t, cli.Status().Update(context.Background(), &liveMKA))

		res, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, forgerDefaultPollInterval, res.RequeueAfter)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePublishing, findForgerRoleStatus(&got, "decode").Phase)
		assert.Equal(t, workercore.ModelDeploymentForgerReasonPinnedCapacityExceeded,
			findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBlocked).Reason)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Nil(t, liveMD.Spec.Roles[0].KernelArtifactRef)

		// 3. Pinned=True: Publishing advances to Promoting and commits kernelArtifactRef onto ModelDeployment!
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(mka), &liveMKA))
		ModelKernelArtifactConditionPinned.True(&liveMKA, workercore.ModelKernelArtifactReasonPinned, "pinned on seedNode")
		require.NoError(t, cli.Status().Update(context.Background(), &liveMKA))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePromoting, findForgerRoleStatus(&got, "decode").Phase)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
		assert.Equal(t, mka.Name, liveMD.Spec.Roles[0].KernelArtifactRef.Name)
	})

	t.Run("Active ModelKernelArtifact entering SourceLost while Idle bypasses cooldown and re-forges its keys", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		mka1 := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "1", "2", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
			ModelKernelArtifactConditionResolved.False(k, workercore.ModelKernelArtifactReasonSourceLost, "no ready copies remain")
		})
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
			d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: mka1.Name}
		})
		recentFin := meta.NewTime(now.Add(-15 * time.Second)) // well inside 600s cooldownSeconds!
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Probe.CooldownSeconds = 600
			f.Spec.Probe.MinActiveRequests = 0
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:                 "decode",
				Phase:                workercore.ModelDeploymentForgerRolePhaseIdle,
				ActiveGeneration:     1,
				ActiveKernelArtifact: mka1.Name,
				Generations: []workercore.ModelDeploymentForgerGenerationStatus{{
					Generation:   1,
					ArtifactName: mka1.Name,
					Outcome:      workercore.ModelDeploymentForgerGenerationOutcomePromoted,
					FinishedAt:   &recentFin,
				}},
			}}
		})

		cli := newForgerTestFakeClient(node0, mka1, md, mdf)
		p0 := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: md.Namespace,
				Name:      "qwen-pd-decode-0",
				UID:       "leader-pod-uid-1",
				Labels: map[string]string{
					modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
					modelDeploymentLabelKeyInstance:    md.Name,
					modelDeploymentLabelKeyComponent:   "decode",
					modelDeploymentReplicaOrdinalLabel: "0",
				},
			},
			Spec: core.PodSpec{
				NodeName:   "node-0",
				Containers: []core.Container{{Name: modelDeploymentMainContainerName}},
			},
		}
		require.NoError(t, cli.Create(context.Background(), p0))
		markServingPodReadyEligible(t, cli, p0, "node-0", "10.0.0.10", 8000)

		payload := &runtimecontrol.ProbePayload{
			SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
			Engine:        "vLLM",
			EngineVersion: "0.19.0",
			Manufacturer:  nodefeature.ManufacturerNVIDIA,
			Architecture:  "sm_90a",
			Bottlenecks: []runtimecontrol.ProbeBottleneck{{
				LocalKernelKey:      key1,
				OpFamily:            modelkernelkey.OpFamilyRMSNorm,
				GPUTimeUs:           100,
				GPUTimeSharePercent: 30,
				CallCount:           4,
			}},
		}
		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
			ProbeClient: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
			},
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		rs := findForgerRoleStatus(&got, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseForging, rs.Phase,
			"SourceLost active artifact must bypass cooldown and re-forge key1 instead of waiting or skipping with NoEligibleBottlenecks")
		assert.Equal(t, int64(2), rs.ActiveGeneration)
		require.NotNil(t, rs.ActiveSandbox)
	})

	t.Run("Probing GetProbe transport/idle resets and Publishing transient ingest retry, missing ConfigMap, and RolePaused gate", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment()
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name: "decode", Phase: workercore.ModelDeploymentForgerRolePhaseProbing, ActiveGeneration: 1,
			}}
		})
		cli := newForgerTestFakeClient(node0, md, mdf)
		pCli := &stubProbeClient{getErr: errors.New("connection reset")}
		r := &ModelDeploymentForgerReconciler{
			Client:      cli,
			Recorder:    ctrlrecord.NewFakeRecorder(32),
			Now:         func() time.Time { return now },
			ProbeClient: pCli,
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		// First without any leader pod -> resets to Idle.
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, findForgerRoleStatus(&got, "decode").Phase)

		// Create leader pod and test GetProbe transport error -> resets to Idle.
		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		_, err = mdRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		p0 := pods.Items[0].DeepCopy()
		markServingPodReadyEligible(t, cli, p0, "node-0", "10.0.0.10", 8000)

		got.Status.Roles[0].Phase = workercore.ModelDeploymentForgerRolePhaseProbing
		require.NoError(t, cli.Status().Update(context.Background(), &got))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, findForgerRoleStatus(&got, "decode").Phase)

		// GetProbe returning ProbeStateIdle -> resets to Idle.
		pCli.getErr = nil
		pCli.getResp = &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateIdle}
		got.Status.Roles[0].Phase = workercore.ModelDeploymentForgerRolePhaseProbing
		require.NoError(t, cli.Status().Update(context.Background(), &got))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, findForgerRoleStatus(&got, "decode").Phase)

		// Publishing with missing input ConfigMap -> InvalidSandboxInput failure & cleanup.
		started := meta.NewTime(now.Add(-5 * time.Second))
		sbPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{Namespace: md.Namespace, Name: "sb-pub-pod", UID: "sb-pub-uid"},
			Spec:       core.PodSpec{NodeName: "node-0"},
		}
		require.NoError(t, cli.Create(context.Background(), sbPod))
		got.Status.Roles[0].Phase = workercore.ModelDeploymentForgerRolePhasePublishing
		got.Status.Roles[0].ActiveGeneration = 1
		got.Status.Roles[0].ActiveSandbox = &workercore.ModelDeploymentForgerSandboxStatus{
			PodName:       sbPod.Name,
			PodUID:        string(sbPod.UID),
			ConfigMapName: "missing-cm",
			StartedAt:     &started,
		}
		require.NoError(t, cli.Status().Update(context.Background(), &got))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, findForgerRoleStatus(&got, "decode").Phase)
		assert.Equal(t, "InvalidSandboxInput", findForgerRoleStatus(&got, "decode").Generations[0].Reason)

		// Publishing transient ingest error keeps ActiveSandbox intact and retries.
		validCM, _, err := modelforger.BuildInputConfigMap(modelforger.InputConfigMapSpec{
			Forger:     &got,
			Name:       "valid-pub-cm",
			RoleName:   "decode",
			Generation: 2,
			Request: modelforger.ForgeRequest{
				SchemaVersion:    modelforger.ForgeRequestSchemaVersion,
				Engine:           "vLLM",
				EngineVersion:    "0.19.0",
				Manufacturer:     nodefeature.ManufacturerNVIDIA,
				Architecture:     "sm_90a",
				TargetKernelKeys: []string{key1},
				MinSpeedupRatio:  1.05,
				Atol:             1e-2,
				Rtol:             1e-2,
			},
		})
		require.NoError(t, err)
		require.NoError(t, cli.Create(context.Background(), validCM))
		sbPod2 := &core.Pod{
			ObjectMeta: meta.ObjectMeta{Namespace: md.Namespace, Name: "sb-pub-pod-2", UID: "sb-pub-uid-2"},
			Spec:       core.PodSpec{NodeName: "node-0"},
		}
		require.NoError(t, cli.Create(context.Background(), sbPod2))

		r.ResolveSeedEndpoint = func(context.Context, string) (string, int, error) { return "10.0.0.50", 10250, nil }
		r.IngestClient = &stubIngestClient{err: errors.New("temporary 503")}
		got.Status.Roles[0].Phase = workercore.ModelDeploymentForgerRolePhasePublishing
		got.Status.Roles[0].ActiveGeneration = 2
		got.Status.Roles[0].ActiveSandbox = &workercore.ModelDeploymentForgerSandboxStatus{
			PodName:       sbPod2.Name,
			PodUID:        string(sbPod2.UID),
			NodeName:      "node-0",
			ConfigMapName: validCM.Name,
			StartedAt:     &started,
		}
		require.NoError(t, cli.Status().Update(context.Background(), &got))
		res, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, forgerIdleRetryInterval, res.RequeueAfter)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePublishing, findForgerRoleStatus(&got, "decode").Phase)
		assert.NotNil(t, findForgerRoleStatus(&got, "decode").ActiveSandbox)

		// Publishing hold when RolePaused is set right before promotion of an already-resolved artifact.
		mka2 := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-2", md.Namespace, "a", "b", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: got.Name,
				UID:  string(got.UID),
			}
		})
		require.NoError(t, cli.Create(context.Background(), mka2))
		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		liveMD.Spec.Roles[0].Pause = true
		require.NoError(t, cli.Update(context.Background(), &liveMD))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePublishing, findForgerRoleStatus(&got, "decode").Phase)
		assert.Nil(t, findForgerRoleStatus(&got, "decode").ActiveSandbox, "sandbox must still be cleaned up once artifact exists")
		assert.Equal(t, workercore.ModelDeploymentForgerReasonRolePaused,
			findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBlocked).Reason)
	})

	t.Run("Promoting re-commits missing kernelArtifactRef, converges zero-replica role, and clears state on Suspend", func(t *testing.T) {
		mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "3", "4", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef.Name = "qwen-decode-forger"
		})
		nms0 := &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-0"},
			Spec:       workercore.NodeModelStoreSpec{PeerSyncEnabled: ptr.To(true)},
			Status: workercore.NodeModelStoreStatus{
				Models: []workercore.NodeModelStoreModel{{
					Digest: mka.Spec.Source.NodeStore.ManifestDigest,
					State:  workercore.NodeModelStoreModelStateReady,
				}},
			},
		}
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 0
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:                 "decode",
				Phase:                workercore.ModelDeploymentForgerRolePhasePromoting,
				ActiveGeneration:     1,
				ActiveKernelArtifact: mka.Name,
			}}
		})

		cli := newForgerTestFakeClient(nms0, mka, md, mdf)
		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		r := &ModelDeploymentForgerReconciler{
			Client:   cli,
			Recorder: ctrlrecord.NewFakeRecorder(32),
			Now:      func() time.Time { return now },
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		mdReq := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)}

		// Pass 1: mdRole.KernelArtifactRef was nil -> re-commits mka.Name onto ModelDeployment!
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), mdReq.NamespacedName, &liveMD))
		require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
		assert.Equal(t, mka.Name, liveMD.Spec.Roles[0].KernelArtifactRef.Name)

		// ModelDeployment observes KernelsReady=True on 0-replica role -> Pass 2 converges immediately!
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)
		_, err = mdRec.Reconcile(context.Background(), mdReq)
		require.NoError(t, err)

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		rs := findForgerRoleStatus(&got, "decode")
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
		require.Len(t, rs.Generations, 1)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomePromoted, rs.Generations[0].Outcome)

		// Populate in-memory probe & promotion tracking, then Suspend -> forgetForgerState clears both maps.
		r.rememberProbe(got.UID, "decode", forgerProbeAttempt{PodName: "p0", StartedAt: now})
		r.rememberPromotion(got.UID, "decode", forgerPromotionAttempt{ArtifactName: mka.Name, StartedAt: now})
		got.Spec.Suspend = true
		require.NoError(t, cli.Update(context.Background(), &got))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		_, hasProbe := r.loadProbe(got.UID, "decode")
		_, hasPromo := r.loadPromotion(got.UID, "decode")
		assert.False(t, hasProbe)
		assert.False(t, hasPromo)
	})
}

func TestModelDeploymentForger_SetupBindingRecoveryAndHoldDetails(t *testing.T) {
	now := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)

	// 1. SetupController wires client, APIReader, recorder, clock, and watches cleanly.
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:6443"}, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		MapperProvider: func(*rest.Config, *http.Client) (kmeta.RESTMapper, error) {
			return kmeta.NewDefaultRESTMapper(nil), nil
		},
	})
	require.NoError(t, err)
	setupRec := &ModelDeploymentForgerReconciler{}
	require.NoError(t, setupRec.SetupController(context.Background(), controller.SetupOptions{Manager: manager.NewCtrlManager(mgr)}))
	assert.NotNil(t, setupRec.Client)
	assert.NotNil(t, setupRec.APIReader)
	assert.NotNil(t, setupRec.Now)

	// 2. Binding / decision helper edge cases and conflict safety.
	_, ok, err := readForgerBinding(nil)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = readForgerDecision(nil)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, int64(0), forgerTerm(nil))

	owned, _, hasMeta := parseForgerDecisionID("other-uid-1-1-100", "mdf-uid-1")
	assert.False(t, owned)
	assert.False(t, hasMeta)
	owned, _, hasMeta = parseForgerDecisionID("mdf-uid-1-short", "mdf-uid-1")
	assert.True(t, owned)
	assert.False(t, hasMeta)
	owned, _, hasMeta = parseForgerDecisionID("mdf-uid-1-1-badns", "mdf-uid-1")
	assert.True(t, owned)
	assert.False(t, hasMeta)

	// 3. Reconcile NotFound returns cleanly; deletion when target MD is missing or carries corrupt binding still removes finalizer.
	delAt := meta.NewTime(now)
	deletingMDF := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
		f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
		f.DeletionTimestamp = &delAt
	})
	corruptMD := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
		d.Name = "corrupt-md"
		d.Annotations = map[string]string{
			workercore.ModelDeploymentForgerBindingAnnotationKey:  "{bad-json",
			workercore.ModelDeploymentForgerDecisionAnnotationKey: "{bad-json",
		}
	})
	cli := newForgerTestFakeClient(deletingMDF, corruptMD)
	r := &ModelDeploymentForgerReconciler{
		Client:      cli,
		Now:         func() time.Time { return now },
		DrainReader: runtimecontrol.NewDrainReader(),
	}
	assert.Same(t, r.DrainReader, r.drainReader())
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "missing"}})
	require.NoError(t, err)

	// Target qwen-pd does not exist -> deletingMDF still clears finalizer!
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(deletingMDF)})
	require.NoError(t, err)
	assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(deletingMDF), &workercore.ModelDeploymentForger{})))

	// Corrupt binding annotation ignored during releaseOwnedBinding; recoverDecision ignores corrupt decision JSON.
	require.NoError(t, r.releaseOwnedBinding(context.Background(), newTestModelDeploymentForger(), corruptMD))
	r.recoverDecision(context.Background(), newTestModelDeploymentForger(), corruptMD)

	// 4. recoverDecision populates nil Status.Binding and already-promoted ActiveKernelArtifact;
	// commitRoleKernelArtifact re-stamps missing binding and refuses foreign binding or missing role.
	bindJSON, _ := json.Marshal(workercore.ModelDeploymentForgerBinding{
		ForgerName: "qwen-decode-forger",
		ForgerUID:  "mdf-uid-1",
		Term:       2,
	})
	decJSON, _ := json.Marshal(workercore.ModelDeploymentForgerDecision{
		DecisionID:      formatForgerDecisionID("mdf-uid-1", 2, 1, now),
		Term:            2,
		KernelArtifacts: map[string]string{"decode": "qwen-decode-forger-decode-gen-1"},
	})
	recMD := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
		d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "qwen-decode-forger-decode-gen-1"}
		d.Annotations = map[string]string{
			workercore.ModelDeploymentForgerBindingAnnotationKey:  string(bindJSON),
			workercore.ModelDeploymentForgerDecisionAnnotationKey: string(decJSON),
		}
	})
	recMDF := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
		f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
			Name:  "decode",
			Phase: workercore.ModelDeploymentForgerRolePhaseIdle,
			Generations: []workercore.ModelDeploymentForgerGenerationStatus{{
				Generation:   1,
				ArtifactName: "qwen-decode-forger-decode-gen-1",
				Outcome:      workercore.ModelDeploymentForgerGenerationOutcomePromoted,
			}},
		}}
	})
	r.recoverDecision(context.Background(), recMDF, recMD)
	require.NotNil(t, recMDF.Status.Binding)
	assert.Equal(t, int64(2), recMDF.Status.Binding.Term)
	assert.Equal(t, "qwen-decode-forger-decode-gen-1", recMDF.Status.Roles[0].ActiveKernelArtifact)

	unboundMD := newForgerTestDeployment(func(d *workercore.ModelDeployment) { d.Name = "unbound-md" })
	foreignBindJSON, _ := json.Marshal(workercore.ModelDeploymentForgerBinding{
		ForgerName: "other",
		ForgerUID:  "other-uid",
		Term:       1,
	})
	foreignMD := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
		d.Name = "foreign-md"
		d.Annotations = map[string]string{workercore.ModelDeploymentForgerBindingAnnotationKey: string(foreignBindJSON)}
	})
	cli2 := newForgerTestFakeClient(unboundMD, foreignMD)
	r2 := &ModelDeploymentForgerReconciler{Client: cli2}
	commitMDF := newTestModelDeploymentForger()
	require.NoError(t, r2.commitRoleKernelArtifact(context.Background(), commitMDF, unboundMD, "decode", "art-1", now))
	assert.NotEmpty(t, unboundMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])
	assert.Error(t, r2.commitRoleKernelArtifact(context.Background(), commitMDF, unboundMD, "nonexistent-role", "art-1", now))
	assert.Error(t, r2.commitRoleKernelArtifact(context.Background(), commitMDF, foreignMD, "decode", "art-1", now))

	// 5. evaluateRoleRuntimeHold covers un-reported declared runtimeControl and lagging ObservedGeneration.
	unreportedMD := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
		d.Generation = 3
		d.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{runtimecontrol.ParamSchedulingMaxActiveRequests: "64"},
		}
	})
	reason, _, held := evaluateRoleRuntimeHold(unreportedMD, "decode")
	assert.True(t, held)
	assert.Equal(t, workercore.ModelDeploymentForgerReasonRuntimeControlUnsynced, reason)

	laggingMD := unreportedMD.DeepCopy()
	laggingMD.Status.Roles = []workercore.ModelDeploymentRoleStatus{{
		Name: "decode",
		RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControlStatus{
			Synced:             true,
			ObservedGeneration: 2,
		},
	}}
	reason, _, held = evaluateRoleRuntimeHold(laggingMD, "decode")
	assert.True(t, held)
	assert.Equal(t, workercore.ModelDeploymentForgerReasonRuntimeControlUnsynced, reason)
	_, _, held = evaluateRoleRuntimeHold(laggingMD, "missing-role")
	assert.False(t, held)

	// 6. Defensive nil/empty boundaries across metrics, status, and phase helpers.
	recordForgerProbeMetric("", forgerProbeResultError, 0)
	recordForgerSandboxMetric("", forgerSandboxResultFailed)
	recordForgerRollbackMetric("")
	assert.False(t, artifactBelongsToForger(nil, nil))
	assert.True(t, lastPassTimestamp(nil).IsZero())
	defs, wls := buildSandboxDefinitionsAndWorkloads(nil, nil)
	assert.Empty(t, defs)
	assert.Empty(t, wls)
	loadErr, dispDig, loaded := summarizeRuntimeConfigKernelState(nil)
	assert.Empty(t, loadErr)
	assert.Empty(t, dispDig)
	assert.Empty(t, loaded)
	assert.True(t, modelDeploymentForgerStatusChanged(nil, &recMDF.Status))
	assert.False(t, modelDeploymentForgerStatusChanged(nil, nil))
	assert.Nil(t, forgerRoleStatusPtr(recMDF, "missing"))
	appendForgerGenerationOutcome(nil, workercore.ModelDeploymentForgerGenerationStatus{Generation: 1})
	pNil, err := r2.findPodByName(context.Background(), "team-a", "")
	require.NoError(t, err)
	assert.Nil(t, pNil)
	pMissing, err := r2.findPodByName(context.Background(), "team-a", "missing-pod")
	require.NoError(t, err)
	assert.Nil(t, pMissing)
	atEmpty, err := r2.getAcceleratorType(context.Background(), "")
	require.NoError(t, err)
	assert.Nil(t, atEmpty)
}

func TestModelDeploymentForger_PublishingKernelKeysValidation(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	key2 := buildTestKernelKey(t, modelkernelkey.OpFamilyRopeKVAppend, 4096)
	foreignKey := buildTestKernelKey(t, modelkernelkey.OpFamilyActivationQuant, 4096)
	now := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)

	cases := []struct {
		name           string
		ingestedKeys   []string
		wantKeys       []string
		wantFailReason string
	}{
		{
			name:         "uses ingested kernelKeys subset instead of full targetKernelKeys",
			ingestedKeys: []string{key2},
			wantKeys:     []string{key2},
		},
		{
			name:         "falls back to forge-request targetKernelKeys when ingestResp.KernelKeys is empty",
			ingestedKeys: nil,
			wantKeys:     []string{key1, key2},
		},
		{
			name:           "rejects unexpected foreign kernelKey from ingest without creating artifact",
			ingestedKeys:   []string{key1, foreignKey},
			wantFailReason: "IngestRejected",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node0 := newForgerTestNode("node-0")
			md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0].Replicas = 1
			})
			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
			})

			cm, _, err := modelforger.BuildInputConfigMap(modelforger.InputConfigMapSpec{
				Forger:     mdf,
				Name:       "qwen-decode-forger-decode-gen-1-aabbccdd-input",
				RoleName:   "decode",
				Generation: 1,
				Request: modelforger.ForgeRequest{
					SchemaVersion:    modelforger.ForgeRequestSchemaVersion,
					Engine:           "vLLM",
					EngineVersion:    "0.19.0",
					Manufacturer:     nodefeature.ManufacturerNVIDIA,
					Architecture:     "sm_90a",
					TargetKernelKeys: []string{key1, key2},
					MinSpeedupRatio:  1.05,
					Atol:             1e-2,
					Rtol:             1e-2,
				},
			})
			require.NoError(t, err)

			started := meta.NewTime(now.Add(-15 * time.Second))
			sbPod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Namespace: md.Namespace,
					Name:      "qwen-decode-forger-decode-gen-1-aabbccdd-sandbox",
					UID:       "sb-s2-uid",
				},
				Spec: core.PodSpec{NodeName: "node-0"},
			}
			mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:             "decode",
				Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
				ActiveGeneration: 1,
				ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
					PodName:       sbPod.Name,
					PodUID:        string(sbPod.UID),
					NodeName:      "node-0",
					ConfigMapName: cm.Name,
					StartedAt:     &started,
				},
			}}

			cli := newForgerTestFakeClient(node0, md, mdf, cm, sbPod)
			r := &ModelDeploymentForgerReconciler{
				Client:        cli,
				APIReader:     cli,
				Recorder:      ctrlrecord.NewFakeRecorder(32),
				Now:           func() time.Time { return now },
				OperatorImage: "gpustack/gpustack-operator:test",
				ResolveSeedEndpoint: func(context.Context, string) (string, int, error) {
					return "10.0.0.50", 10250, nil
				},
				IngestClient: &stubIngestClient{
					resp: &modelstore.IngestTreeResponse{
						ManifestDigest:         "sha256:" + strings.Repeat("a", 64),
						DispatchManifestDigest: "sha256:" + strings.Repeat("b", 64),
						SizeBytes:              4096,
						FileCount:              3,
						SeedNode:               "node-0",
						KernelKeys:             tc.ingestedKeys,
					},
				},
			}

			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			mkaKey := types.NamespacedName{Namespace: md.Namespace, Name: "qwen-decode-forger-decode-gen-1"}
			var gotMKA workercore.ModelKernelArtifact
			getErr := cli.Get(context.Background(), mkaKey, &gotMKA)

			var gotMDF workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &gotMDF))
			rs := findForgerRoleStatus(&gotMDF, "decode")
			require.NotNil(t, rs)
			assert.Nil(t, rs.ActiveSandbox)
			assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(sbPod), &core.Pod{})))
			assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(cm), &core.ConfigMap{})))

			if tc.wantFailReason != "" {
				assert.True(t, kerrors.IsNotFound(getErr), "foreign kernelKey from ingest must not create ModelKernelArtifact")
				assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
				require.Len(t, rs.Generations, 1)
				assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeFailed, rs.Generations[0].Outcome)
				assert.Equal(t, tc.wantFailReason, rs.Generations[0].Reason)
				return
			}

			require.NoError(t, getErr)
			assert.Equal(t, tc.wantKeys, gotMKA.Spec.Compatibility.KernelKeys)
		})
	}
}

func TestModelDeploymentForger_SandboxStatusFailureCleanupAndOrphanSweep(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	now := time.Date(2026, 11, 27, 22, 30, 0, 0, time.UTC)

	payload := &runtimecontrol.ProbePayload{
		SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
		Engine:        "vLLM",
		EngineVersion: "0.19.0",
		Manufacturer:  nodefeature.ManufacturerNVIDIA,
		Architecture:  "sm_90a",
		Bottlenecks: []runtimecontrol.ProbeBottleneck{{
			LocalKernelKey:      key1,
			OpFamily:            modelkernelkey.OpFamilyRMSNorm,
			GPUTimeUs:           200,
			GPUTimeSharePercent: 25,
			CallCount:           8,
		}},
	}

	t.Run("status update failure on sandbox launch deletes newly created Pod and ConfigMap", func(t *testing.T) {
		for _, startPhase := range []workercore.ModelDeploymentForgerRolePhase{
			workercore.ModelDeploymentForgerRolePhaseIdle,
			workercore.ModelDeploymentForgerRolePhaseProbing,
		} {
			t.Run(string(startPhase), func(t *testing.T) {
				node0 := newForgerTestNode("node-0")
				md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
					d.Spec.Roles[0].Replicas = 1
				})
				mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
					f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
					f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
					f.Spec.Probe.MinActiveRequests = 0
					f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
						Name:             "decode",
						Phase:            startPhase,
						ActiveGeneration: 1,
					}}
				})

				var failStatusWrite atomic.Bool
				baseCli := newForgerTestFakeClient(node0, md, mdf)
				cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, subResourceName string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
						if subResourceName == "status" {
							if _, isMDF := obj.(*workercore.ModelDeploymentForger); isMDF && failStatusWrite.CompareAndSwap(true, false) {
								return kerrors.NewConflict(workercore.Resource("modeldeploymentforgers"), obj.GetName(), errors.New("simulated status conflict"))
							}
						}
						return c.Status().Update(ctx, obj, opts...)
					},
				})

				mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
				_, err := mdRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
				require.NoError(t, err)
				var pods core.PodList
				require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
				require.Len(t, pods.Items, 1)
				p0 := pods.Items[0].DeepCopy()
				markServingPodReadyEligible(t, cli, p0, "node-0", "10.0.0.10", 8000)

				r := &ModelDeploymentForgerReconciler{
					Client:        cli,
					APIReader:     cli,
					Recorder:      ctrlrecord.NewFakeRecorder(32),
					Now:           func() time.Time { return now },
					OperatorImage: "gpustack/gpustack-operator:test",
					ProbeClient: &stubProbeClient{
						readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
						startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
						getResp:   &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
					},
				}
				if startPhase == workercore.ModelDeploymentForgerRolePhaseProbing {
					r.rememberProbe(mdf.UID, "decode", forgerProbeAttempt{
						PodName:   p0.Name,
						PodUID:    string(p0.UID),
						BootID:    "b1",
						NodeName:  "node-0",
						StartedAt: now.Add(-2 * time.Second),
					})
				}

				failStatusWrite.Store(true)
				req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
				_, err = r.Reconcile(context.Background(), req)
				require.Error(t, err)

				var sbPods core.PodList
				require.NoError(t, cli.List(context.Background(), &sbPods,
					ctrlcli.InNamespace(md.Namespace),
					ctrlcli.MatchingLabels{modelforger.SandboxLabelRole: "decode"},
				))
				assert.Empty(t, sbPods.Items, "failed status update on sandbox launch must delete newly created ForgeSandbox Pod")

				var sbCMs core.ConfigMapList
				require.NoError(t, cli.List(context.Background(), &sbCMs,
					ctrlcli.InNamespace(md.Namespace),
					ctrlcli.MatchingLabels{modelforger.SandboxLabelRole: "decode"},
				))
				assert.Empty(t, sbCMs.Items, "failed status update on sandbox launch must delete newly created input ConfigMap")
			})
		}
	})

	t.Run("Idle and Probing automatically sweep orphan ForgeSandbox Pods and ConfigMaps owned by mdf", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
		})
		recentFin := meta.NewTime(now.Add(-30 * time.Second))
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
			f.Spec.Probe.CooldownSeconds = 600
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:             "decode",
				Phase:            workercore.ModelDeploymentForgerRolePhaseIdle,
				ActiveGeneration: 1,
				Generations: []workercore.ModelDeploymentForgerGenerationStatus{{
					Generation: 1,
					Outcome:    workercore.ModelDeploymentForgerGenerationOutcomeSkipped,
					FinishedAt: &recentFin,
				}},
			}}
		})

		orphanOwner := []meta.OwnerReference{{
			APIVersion: workercore.SchemeGroupVersion.String(),
			Kind:       "ModelDeploymentForger",
			Name:       mdf.Name,
			UID:        mdf.UID,
			Controller: ptr.To(true),
		}}
		foreignOwner := []meta.OwnerReference{{
			APIVersion: workercore.SchemeGroupVersion.String(),
			Kind:       "ModelDeploymentForger",
			Name:       "other-forger",
			UID:        "other-mdf-uid",
			Controller: ptr.To(true),
		}}
		orphanPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace:       mdf.Namespace,
				Name:            "qwen-decode-forger-decode-gen-1-orphan-sandbox",
				Labels:          map[string]string{modelforger.SandboxLabelName: "model-deployment-forger", modelforger.SandboxLabelRole: "decode"},
				OwnerReferences: orphanOwner,
			},
		}
		orphanCM := &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{
				Namespace:       mdf.Namespace,
				Name:            "qwen-decode-forger-decode-gen-1-orphan-input",
				Labels:          map[string]string{modelforger.SandboxLabelName: "model-deployment-forger", modelforger.SandboxLabelRole: "decode"},
				OwnerReferences: orphanOwner,
			},
		}
		foreignPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace:       mdf.Namespace,
				Name:            "other-forger-decode-gen-1-sandbox",
				Labels:          map[string]string{modelforger.SandboxLabelName: "model-deployment-forger", modelforger.SandboxLabelRole: "decode"},
				OwnerReferences: foreignOwner,
			},
		}

		cli := newForgerTestFakeClient(node0, md, mdf, orphanPod, orphanCM, foreignPod)
		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
			ProbeClient: &stubProbeClient{
				getResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateRunning, BootID: "b1"},
			},
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// 1. Sweep during Idle (even inside probe cooldown!).
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(orphanPod), &core.Pod{})),
			"orphan sandbox Pod owned by mdf must be swept during Idle")
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(orphanCM), &core.ConfigMap{})),
			"orphan sandbox ConfigMap owned by mdf must be swept during Idle")
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(foreignPod), &core.Pod{}),
			"sandbox Pod owned by another ModelDeploymentForger UID must not be deleted")

		// 2. Sweep during Probing.
		p0 := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: md.Namespace,
				Name:      "qwen-pd-decode-0",
				UID:       "leader-pod-uid",
				Labels: map[string]string{
					modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
					modelDeploymentLabelKeyInstance:    md.Name,
					modelDeploymentLabelKeyComponent:   "decode",
					modelDeploymentReplicaOrdinalLabel: "0",
				},
			},
			Spec: core.PodSpec{NodeName: "node-0", Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
		}
		require.NoError(t, cli.Create(context.Background(), p0))
		markServingPodReadyEligible(t, cli, p0, "node-0", "10.0.0.10", 8000)

		orphanPod2 := orphanPod.DeepCopy()
		orphanPod2.Name = "qwen-decode-forger-decode-gen-2-orphan-sandbox"
		orphanPod2.ResourceVersion = ""
		orphanPod2.UID = ""
		require.NoError(t, cli.Create(context.Background(), orphanPod2))

		var liveMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Status.Roles[0].Phase = workercore.ModelDeploymentForgerRolePhaseProbing
		require.NoError(t, cli.Status().Update(context.Background(), &liveMDF))
		r.rememberProbe(mdf.UID, "decode", forgerProbeAttempt{
			PodName: p0.Name, PodUID: string(p0.UID), BootID: "b1", NodeName: "node-0", StartedAt: now.Add(-2 * time.Second),
		})

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(orphanPod2), &core.Pod{})),
			"orphan sandbox Pod owned by mdf must be swept during Probing")
	})
}

func TestModelDeploymentForger_ForeignKernelArtifactNeverAdopted(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	now := time.Date(2026, 11, 27, 23, 0, 0, 0, time.UTC)

	newForeignHFArtifact := func(name, ns string) *workercore.ModelKernelArtifact {
		resolvedAt := meta.NewTime(now)
		mka := &workercore.ModelKernelArtifact{
			ObjectMeta: meta.ObjectMeta{
				Namespace:  ns,
				Name:       name,
				UID:        types.UID("foreign-hf-uid-" + name),
				Generation: 1,
			},
			Spec: workercore.ModelKernelArtifactSpec{
				Source: workercore.ModelKernelArtifactSource{
					HuggingFace: &workercore.ModelArtifactHubSource{
						Repository: "evil-tenant/forged-kernels",
						Revision:   strings.Repeat("f", 40),
					},
				},
				Compatibility: workercore.ModelKernelArtifactCompatibility{
					Engine:        "vLLM",
					EngineVersion: "0.19",
					Manufacturer:  nodefeature.ManufacturerNVIDIA,
					Architecture:  "sm_90a",
					KernelKeys:    []string{key1},
				},
			},
			Status: workercore.ModelKernelArtifactStatus{
				ObservedGeneration: 1,
				Resolved: &workercore.ModelKernelArtifactResolved{
					ManifestDigest:         "sha256:" + strings.Repeat("e", 64),
					DispatchManifestDigest: "sha256:" + strings.Repeat("d", 64),
					Revision:               strings.Repeat("f", 40),
					FileCount:              2,
					SizeBytes:              1024,
					ResolvedTime:           resolvedAt,
					LastValidatedTime:      &resolvedAt,
					DigestSource:           workercore.ModelKernelArtifactDigestSourceHub,
				},
				Nodes: &workercore.ModelArtifactNodes{Ready: 1},
			},
		}
		ModelKernelArtifactConditionResolved.True(mka, workercore.ModelKernelArtifactReasonResolved, "resolved")
		ModelKernelArtifactConditionPinned.True(mka, workercore.ModelKernelArtifactReasonPinned, "pinned")
		return mka
	}

	t.Run("pre-existing foreign huggingFace gen-1 artifact is skipped by nextGenerationNumber and never promoted", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
			f.Spec.Probe.MinActiveRequests = 0
		})
		foreignGen1 := newForeignHFArtifact("qwen-decode-forger-decode-gen-1", md.Namespace)

		cli := newForgerTestFakeClient(node0, md, mdf, foreignGen1)
		mdRec := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		_, err := mdRec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(md)})
		require.NoError(t, err)
		var pods core.PodList
		require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace(md.Namespace)))
		require.Len(t, pods.Items, 1)
		p0 := pods.Items[0].DeepCopy()
		markServingPodReadyEligible(t, cli, p0, "node-0", "10.0.0.10", 8000)

		payload := &runtimecontrol.ProbePayload{
			SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
			Engine:        "vLLM",
			EngineVersion: "0.19.0",
			Manufacturer:  nodefeature.ManufacturerNVIDIA,
			Architecture:  "sm_90a",
			Bottlenecks: []runtimecontrol.ProbeBottleneck{{
				LocalKernelKey:      key1,
				OpFamily:            modelkernelkey.OpFamilyRMSNorm,
				GPUTimeUs:           180,
				GPUTimeSharePercent: 28,
				CallCount:           6,
			}},
		}
		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
			ProbeClient: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
			},
			ResolveSeedEndpoint: func(context.Context, string) (string, int, error) {
				return "10.0.0.50", 10250, nil
			},
			IngestClient: &stubIngestClient{
				resp: &modelstore.IngestTreeResponse{
					ManifestDigest:         "sha256:" + strings.Repeat("2", 64),
					DispatchManifestDigest: "sha256:" + strings.Repeat("3", 64),
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "node-0",
					KernelKeys:             []string{key1},
				},
			},
		}

		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		rs := findForgerRoleStatus(&got, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, int64(2), rs.ActiveGeneration, "nextGenerationNumber must count pre-existing foreign gen-1 artifact and advance to gen-2")
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseForging, rs.Phase)
		require.NotNil(t, rs.ActiveSandbox)

		// Complete gen-2 sandbox and verify gen-2 is published while foreign gen-1 is never adopted or promoted!
		var sbPod core.Pod
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: rs.ActiveSandbox.PodName}, &sbPod))
		sbPod.Spec.NodeName = "node-0"
		require.NoError(t, cli.Update(context.Background(), &sbPod))
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(&sbPod), &sbPod))
		sbPod.Status.Phase = core.PodRunning
		sbPod.Status.InitContainerStatuses = []core.ContainerStatus{
			{Name: modelforger.RunnerContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
			{Name: modelforger.VerifierContainerName, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}},
		}
		require.NoError(t, cli.Status().Update(context.Background(), &sbPod))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var gen2MKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: "qwen-decode-forger-decode-gen-2"}, &gen2MKA))
		require.NotNil(t, gen2MKA.Spec.Source.NodeStore)
		assert.Equal(t, string(mdf.UID), gen2MKA.Spec.Source.NodeStore.ForgerRef.UID)

		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		if liveMD.Spec.Roles[0].KernelArtifactRef != nil {
			assert.NotEqual(t, foreignGen1.Name, liveMD.Spec.Roles[0].KernelArtifactRef.Name)
		}
	})

	t.Run("AlreadyExists collision with foreign artifact during Publishing records ArtifactNameConflict and bumps generation", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
		})

		cm, _, err := modelforger.BuildInputConfigMap(modelforger.InputConfigMapSpec{
			Forger:     mdf,
			Name:       "qwen-decode-forger-decode-gen-1-11223344-input",
			RoleName:   "decode",
			Generation: 1,
			Request: modelforger.ForgeRequest{
				SchemaVersion:    modelforger.ForgeRequestSchemaVersion,
				Engine:           "vLLM",
				EngineVersion:    "0.19.0",
				Manufacturer:     nodefeature.ManufacturerNVIDIA,
				Architecture:     "sm_90a",
				TargetKernelKeys: []string{key1},
				MinSpeedupRatio:  1.05,
				Atol:             1e-2,
				Rtol:             1e-2,
			},
		})
		require.NoError(t, err)

		started := meta.NewTime(now.Add(-10 * time.Second))
		sbPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: md.Namespace,
				Name:      "qwen-decode-forger-decode-gen-1-11223344-sandbox",
				UID:       "sb-l2-uid",
			},
			Spec: core.PodSpec{NodeName: "node-0"},
		}
		mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
			Name:             "decode",
			Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
			ActiveGeneration: 1,
			ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
				PodName:       sbPod.Name,
				PodUID:        string(sbPod.UID),
				NodeName:      "node-0",
				ConfigMapName: cm.Name,
				StartedAt:     &started,
			},
		}}

		baseCli := newForgerTestFakeClient(node0, md, mdf, cm, sbPod)
		var collideCreate atomic.Bool
		cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				if mka, isMKA := obj.(*workercore.ModelKernelArtifact); isMKA && collideCreate.CompareAndSwap(true, false) {
					_ = c.Create(ctx, newForeignHFArtifact(mka.Name, mka.Namespace))
					return kerrors.NewAlreadyExists(workercore.Resource("modelkernelartifacts"), mka.Name)
				}
				return c.Create(ctx, obj, opts...)
			},
		})

		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
			ResolveSeedEndpoint: func(context.Context, string) (string, int, error) {
				return "10.0.0.50", 10250, nil
			},
			IngestClient: &stubIngestClient{
				resp: &modelstore.IngestTreeResponse{
					ManifestDigest:         "sha256:" + strings.Repeat("9", 64),
					DispatchManifestDigest: "sha256:" + strings.Repeat("8", 64),
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "node-0",
					KernelKeys:             []string{key1},
				},
			},
		}

		collideCreate.Store(true)
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		rs := findForgerRoleStatus(&got, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
		assert.Empty(t, rs.ActiveKernelArtifact, "foreign pre-existing ModelKernelArtifact must never be adopted")
		assert.Nil(t, rs.ActiveSandbox)
		assert.GreaterOrEqual(t, rs.ActiveGeneration, int64(2), "ArtifactNameConflict must advance ActiveGeneration past the collided name")
		require.Len(t, rs.Generations, 1)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeFailed, rs.Generations[0].Outcome)
		assert.Equal(t, "ArtifactNameConflict", rs.Generations[0].Reason)

		var liveMD workercore.ModelDeployment
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
		assert.Nil(t, liveMD.Spec.Roles[0].KernelArtifactRef, "foreign pre-existing ModelKernelArtifact must never be promoted onto target deployment")
	})
}

func TestModelDeploymentForger_UncommittedCanaryRollbackOnExitAndRecovery(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 15, 0, 0, time.UTC)

	exitCases := []struct {
		name         string
		prevArtifact string
		mutateExit   func(*workercore.ModelDeploymentForger)
	}{
		{
			name:         "Suspend rolls back uncommitted canary to previousKernelArtifact",
			prevArtifact: "qwen-decode-forger-decode-gen-1",
			mutateExit: func(f *workercore.ModelDeploymentForger) {
				f.Spec.Suspend = true
			},
		},
		{
			name:         "Suspend rolls back uncommitted canary to nil when previousKernelArtifact is empty",
			prevArtifact: "",
			mutateExit: func(f *workercore.ModelDeploymentForger) {
				f.Spec.Suspend = true
			},
		},
		{
			name:         "Shadow mode transition rolls back uncommitted canary to previousKernelArtifact",
			prevArtifact: "qwen-decode-forger-decode-gen-1",
			mutateExit: func(f *workercore.ModelDeploymentForger) {
				f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
			},
		},
		{
			name:         "Shadow mode transition rolls back uncommitted canary to nil when previousKernelArtifact is empty",
			prevArtifact: "",
			mutateExit: func(f *workercore.ModelDeploymentForger) {
				f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
			},
		},
		{
			name:         "ObserveOnly mode transition rolls back uncommitted canary to previousKernelArtifact",
			prevArtifact: "qwen-decode-forger-decode-gen-1",
			mutateExit: func(f *workercore.ModelDeploymentForger) {
				f.Spec.Mode = workercore.ModelDeploymentForgerModeObserveOnly
			},
		},
	}

	for _, tc := range exitCases {
		t.Run(tc.name, func(t *testing.T) {
			canaryName := "qwen-decode-forger-decode-gen-2"
			bindRaw, err := json.Marshal(workercore.ModelDeploymentForgerBinding{
				ForgerName: "qwen-decode-forger",
				ForgerUID:  "mdf-uid-1",
				Term:       1,
			})
			require.NoError(t, err)
			decRaw, err := json.Marshal(workercore.ModelDeploymentForgerDecision{
				DecisionID:      formatForgerDecisionID("mdf-uid-1", 1, 1, now.Add(-20*time.Second)),
				Term:            1,
				KernelArtifacts: map[string]string{"decode": canaryName},
			})
			require.NoError(t, err)

			md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0].Replicas = 2
				d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: canaryName}
				d.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey:  string(bindRaw),
					workercore.ModelDeploymentForgerDecisionAnnotationKey: string(decRaw),
				}
			})

			var gens []workercore.ModelDeploymentForgerGenerationStatus
			if tc.prevArtifact != "" {
				fin := meta.NewTime(now.Add(-700 * time.Second))
				gens = append(gens, workercore.ModelDeploymentForgerGenerationStatus{
					Generation:   1,
					ArtifactName: tc.prevArtifact,
					Outcome:      workercore.ModelDeploymentForgerGenerationOutcomePromoted,
					FinishedAt:   &fin,
				})
			}

			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Generation = 2
				f.Status.ObservedGeneration = 1
				f.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
					TargetUID:  string(md.UID),
					TargetName: md.Name,
					Term:       1,
				}
				f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name:                   "decode",
					Phase:                  workercore.ModelDeploymentForgerRolePhasePromoting,
					ActiveGeneration:       2,
					ActiveKernelArtifact:   canaryName,
					PreviousKernelArtifact: tc.prevArtifact,
					Generations:            gens,
				}}
				tc.mutateExit(f)
			})

			var failRollback atomic.Bool
			baseCli := newForgerTestFakeClient(md, mdf)
			cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
					if _, isMD := obj.(*workercore.ModelDeployment); isMD && failRollback.Load() {
						return kerrors.NewConflict(workercore.Resource("modeldeployments"), obj.GetName(), errors.New("simulated rollback conflict"))
					}
					return c.Update(ctx, obj, opts...)
				},
			})

			r := &ModelDeploymentForgerReconciler{
				Client:        cli,
				APIReader:     cli,
				Recorder:      ctrlrecord.NewFakeRecorder(32),
				Now:           func() time.Time { return now },
				OperatorImage: "gpustack/gpustack-operator:test",
			}
			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

			// When rollback update fails, keep forger-binding and previousKernelArtifact intact.
			failRollback.Store(true)
			_, err = r.Reconcile(context.Background(), req)
			require.Error(t, err)

			var liveMD workercore.ModelDeployment
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
			assert.NotEmpty(t, liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])
			require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
			assert.Equal(t, canaryName, liveMD.Spec.Roles[0].KernelArtifactRef.Name)

			var liveMDF workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
			rs := findForgerRoleStatus(&liveMDF, "decode")
			require.NotNil(t, rs)
			assert.Equal(t, tc.prevArtifact, rs.PreviousKernelArtifact)

			// Once rollback succeeds, target reverts to previousKernelArtifact and binding is released.
			failRollback.Store(false)
			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
			assert.Empty(t, liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])
			if tc.prevArtifact == "" {
				assert.Nil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
			} else {
				require.NotNil(t, liveMD.Spec.Roles[0].KernelArtifactRef)
				assert.Equal(t, tc.prevArtifact, liveMD.Spec.Roles[0].KernelArtifactRef.Name)
			}

			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
			rs = findForgerRoleStatus(&liveMDF, "decode")
			require.NotNil(t, rs)
			assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
			assert.Equal(t, tc.prevArtifact, rs.ActiveKernelArtifact)
			assert.Empty(t, rs.PreviousKernelArtifact)
		})
	}

	t.Run("recoverDecision re-enters Promoting when ActiveKernelArtifact equals uncommitted canary", func(t *testing.T) {
		prevName := "qwen-decode-forger-decode-gen-1"
		canaryName := "qwen-decode-forger-decode-gen-2"
		promotedAt := now.Add(-30 * time.Second)
		bindRaw, err := json.Marshal(workercore.ModelDeploymentForgerBinding{
			ForgerName: "qwen-decode-forger",
			ForgerUID:  "mdf-uid-1",
			Term:       1,
		})
		require.NoError(t, err)
		decRaw, err := json.Marshal(workercore.ModelDeploymentForgerDecision{
			DecisionID:      formatForgerDecisionID("mdf-uid-1", 1, 1, promotedAt),
			Term:            1,
			KernelArtifacts: map[string]string{"decode": canaryName},
		})
		require.NoError(t, err)

		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: canaryName}
			d.Annotations = map[string]string{
				workercore.ModelDeploymentForgerBindingAnnotationKey:  string(bindRaw),
				workercore.ModelDeploymentForgerDecisionAnnotationKey: string(decRaw),
			}
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
			f.Spec.Suspend = false
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
				Name:                   "decode",
				Phase:                  workercore.ModelDeploymentForgerRolePhaseIdle,
				ActiveGeneration:       2,
				ActiveKernelArtifact:   canaryName,
				PreviousKernelArtifact: prevName,
				Generations: []workercore.ModelDeploymentForgerGenerationStatus{{
					Generation:   1,
					ArtifactName: prevName,
					Outcome:      workercore.ModelDeploymentForgerGenerationOutcomePromoted,
				}},
			}}
		})

		r := &ModelDeploymentForgerReconciler{Now: func() time.Time { return now }}
		r.recoverDecision(context.Background(), mdf, md)

		rs := findForgerRoleStatus(mdf, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePromoting, rs.Phase)
		assert.Equal(t, canaryName, rs.ActiveKernelArtifact)
		assert.Equal(t, prevName, rs.PreviousKernelArtifact)
		tracked, ok := r.loadPromotion(mdf.UID, "decode")
		require.True(t, ok)
		assert.Equal(t, promotedAt, tracked.StartedAt)
	})
}

func TestModelDeploymentForger_QuarantineScopedByForgerRoleAndRunner(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	now := time.Date(2026, 11, 27, 23, 30, 0, 0, time.UTC)

	payload := &runtimecontrol.ProbePayload{
		SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
		Engine:        "vLLM",
		EngineVersion: "0.19.0",
		Manufacturer:  nodefeature.ManufacturerNVIDIA,
		Architecture:  "sm_90a",
		Bottlenecks: []runtimecontrol.ProbeBottleneck{{
			LocalKernelKey:      key1,
			OpFamily:            modelkernelkey.OpFamilyRMSNorm,
			GPUTimeUs:           240,
			GPUTimeSharePercent: 25,
			CallCount:           8,
		}},
	}

	t.Run("quarantined artifact from sibling forger or sibling role does not block key forging", func(t *testing.T) {
		node0 := newForgerTestNode("node-0")
		mdB := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Name = "qwen-b"
			d.UID = "md-qwen-b-uid"
			prefillRole := d.Spec.Roles[0].DeepCopy()
			prefillRole.Name = "prefill"
			prefillRole.Kind = workercore.ModelDeploymentRoleKindPrefill
			prefillRole.Replicas = 1
			decodeRole := d.Spec.Roles[0].DeepCopy()
			decodeRole.Name = "decode"
			decodeRole.Kind = workercore.ModelDeploymentRoleKindDecode
			decodeRole.Replicas = 1
			d.Spec.Roles = []workercore.ModelDeploymentRole{*prefillRole, *decodeRole}
		})
		mdfB := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Name = "qwen-b-forger"
			f.UID = "mdf-uid-b"
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.TargetRef.Name = mdB.Name
			f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
			f.Spec.Probe.MinActiveRequests = 0
			f.Spec.Roles = []workercore.ModelDeploymentForgerRoleSpec{
				{Name: "prefill", MaxKernelsPerGeneration: 4, MinSpeedupRatio: "1.05"},
				{Name: "decode", MaxKernelsPerGeneration: 4, MinSpeedupRatio: "1.05"},
			}
		})

		quarantinedOtherForger := newNodeStoreKernelArtifact("qwen-a-forger-decode-gen-1", mdB.Namespace, "1", "2", func(k *workercore.ModelKernelArtifact) {
			k.Labels = map[string]string{
				modelforger.SandboxLabelName:     "model-deployment-forger",
				modelforger.SandboxLabelInstance: "qwen-a-forger",
				modelforger.SandboxLabelRole:     "decode",
			}
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-a-forger",
				UID:  "mdf-uid-a",
			}
			k.Status.Quarantined = true
			k.Status.QuarantineReason = "UnsupportedKernelArtifact"
		})
		quarantinedSiblingRole := newNodeStoreKernelArtifact("qwen-b-forger-prefill-gen-1", mdB.Namespace, "3", "4", func(k *workercore.ModelKernelArtifact) {
			k.Labels = map[string]string{
				modelforger.SandboxLabelName:     "model-deployment-forger",
				modelforger.SandboxLabelInstance: mdfB.Name,
				modelforger.SandboxLabelRole:     "prefill",
			}
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: mdfB.Name,
				UID:  string(mdfB.UID),
			}
			k.Status.Quarantined = true
			k.Status.QuarantineReason = "RolloutTimeout"
		})

		prefillPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: mdB.Namespace,
				Name:      "qwen-b-prefill-0",
				UID:       "pod-prefill-0",
				Labels: map[string]string{
					modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
					modelDeploymentLabelKeyInstance:    mdB.Name,
					modelDeploymentLabelKeyComponent:   "prefill",
					modelDeploymentReplicaOrdinalLabel: "0",
				},
			},
			Spec: core.PodSpec{NodeName: "node-0", Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
		}
		decodePod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: mdB.Namespace,
				Name:      "qwen-b-decode-0",
				UID:       "pod-decode-0",
				Labels: map[string]string{
					modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
					modelDeploymentLabelKeyInstance:    mdB.Name,
					modelDeploymentLabelKeyComponent:   "decode",
					modelDeploymentReplicaOrdinalLabel: "0",
				},
			},
			Spec: core.PodSpec{NodeName: "node-0", Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
		}

		cli := newForgerTestFakeClient(node0, mdB, mdfB, quarantinedOtherForger, quarantinedSiblingRole, prefillPod, decodePod)
		markServingPodReadyEligible(t, cli, prefillPod, "node-0", "10.0.0.21", 8000)
		markServingPodReadyEligible(t, cli, decodePod, "node-0", "10.0.0.22", 8000)

		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
			ProbeClient: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
			},
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdfB)}
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var got workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
		prefillRS := findForgerRoleStatus(&got, "prefill")
		require.NotNil(t, prefillRS)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, prefillRS.Phase)
		require.Len(t, prefillRS.Generations, 1)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeSkipped, prefillRS.Generations[0].Outcome)

		decodeRS := findForgerRoleStatus(&got, "decode")
		require.NotNil(t, decodeRS)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseForging, decodeRS.Phase)
		assert.NotNil(t, decodeRS.ActiveSandbox)
	})

	t.Run("updating sandbox runnerImage allows re-forging previously quarantined key while same runner suppresses retry", func(t *testing.T) {
		curNow := now
		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Replicas = 1
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
			f.Spec.Probe.MinActiveRequests = 0
			f.Spec.Sandbox.RunnerMode = workercore.ModelDeploymentForgerRunnerModeExternal
			f.Spec.Sandbox.Runner = &workercore.ModelDeploymentForgerSandboxRunner{
				Image: "registry.local/forge-runner:v1",
			}
		})
		cm, _, err := modelforger.BuildInputConfigMap(modelforger.InputConfigMapSpec{
			Forger:     mdf,
			Name:       "qwen-decode-forger-decode-gen-1-aaaa1111-input",
			RoleName:   "decode",
			Generation: 1,
			Request: modelforger.ForgeRequest{
				SchemaVersion:    modelforger.ForgeRequestSchemaVersion,
				Engine:           "vLLM",
				EngineVersion:    "0.19.0",
				Manufacturer:     nodefeature.ManufacturerNVIDIA,
				Architecture:     "sm_90a",
				TargetKernelKeys: []string{key1},
				MinSpeedupRatio:  1.05,
				Atol:             1e-2,
				Rtol:             1e-2,
			},
		})
		require.NoError(t, err)
		started := meta.NewTime(curNow.Add(-10 * time.Second))
		sbPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: md.Namespace,
				Name:      "qwen-decode-forger-decode-gen-1-aaaa1111-sandbox",
				UID:       "sb-fp-uid-1",
			},
			Spec: core.PodSpec{NodeName: "node-0"},
		}
		mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
			Name:             "decode",
			Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
			ActiveGeneration: 1,
			ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
				PodName:       sbPod.Name,
				PodUID:        string(sbPod.UID),
				NodeName:      "node-0",
				ConfigMapName: cm.Name,
				StartedAt:     &started,
			},
		}}

		leaderPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: md.Namespace,
				Name:      "qwen-pd-decode-0",
				UID:       "leader-fp-uid",
				Labels: map[string]string{
					modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
					modelDeploymentLabelKeyInstance:    md.Name,
					modelDeploymentLabelKeyComponent:   "decode",
					modelDeploymentReplicaOrdinalLabel: "0",
				},
			},
			Spec: core.PodSpec{NodeName: "node-0", Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
		}

		cli := newForgerTestFakeClient(node0, md, mdf, cm, sbPod, leaderPod)
		markServingPodReadyEligible(t, cli, leaderPod, "node-0", "10.0.0.10", 8000)

		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return curNow },
			OperatorImage: "gpustack/gpustack-operator:test",
			ProbeClient: &stubProbeClient{
				readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
				startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
			},
			ResolveSeedEndpoint: func(context.Context, string) (string, int, error) {
				return "10.0.0.50", 10250, nil
			},
			IngestClient: &stubIngestClient{
				resp: &modelstore.IngestTreeResponse{
					ManifestDigest:         "sha256:" + strings.Repeat("5", 64),
					DispatchManifestDigest: "sha256:" + strings.Repeat("6", 64),
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "node-0",
					KernelKeys:             []string{key1},
				},
			},
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// Publish gen-1 and confirm runner-fingerprint annotation is stamped.
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		mkaKey := types.NamespacedName{Namespace: md.Namespace, Name: "qwen-decode-forger-decode-gen-1"}
		var createdMKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), mkaKey, &createdMKA))
		assert.Len(t, createdMKA.Annotations["forger.gpustack.ai/runner-fingerprint"], 16)

		// Quarantine gen-1 and reset role to Idle past cooldown.
		createdMKA.Status.Quarantined = true
		createdMKA.Status.QuarantineReason = "UnsupportedKernelArtifact"
		require.NoError(t, cli.Status().Update(context.Background(), &createdMKA))

		var liveMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Status.Roles[0].Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		require.NoError(t, cli.Status().Update(context.Background(), &liveMDF))

		// Same runnerImage v1 -> suppresses retry of quarantined key1.
		curNow = curNow.Add(700 * time.Second)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		rs := findForgerRoleStatus(&liveMDF, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
		require.Len(t, rs.Generations, 1)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeSkipped, rs.Generations[0].Outcome)

		// Update runner.image to v2 -> fingerprint changes, allowing key1 to be re-forged!
		liveMDF.Spec.Sandbox.Runner = &workercore.ModelDeploymentForgerSandboxRunner{
			Image: "registry.local/forge-runner:v2",
		}
		liveMDF.Generation++
		require.NoError(t, cli.Update(context.Background(), &liveMDF))

		curNow = curNow.Add(700 * time.Second)
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		rs = findForgerRoleStatus(&liveMDF, "decode")
		require.NotNil(t, rs)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseForging, rs.Phase)
		assert.NotNil(t, rs.ActiveSandbox)
	})
}

func TestModelDeploymentForger_MultiRolePromotionDeadlines(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	t0 := time.Date(2026, 11, 27, 23, 45, 0, 0, time.UTC)

	newTwoRoleDeployment := func() *workercore.ModelDeployment {
		return newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			prefillRole := d.Spec.Roles[0].DeepCopy()
			prefillRole.Name = "prefill"
			prefillRole.Kind = workercore.ModelDeploymentRoleKindPrefill
			prefillRole.Replicas = 1
			decodeRole := d.Spec.Roles[0].DeepCopy()
			decodeRole.Name = "decode"
			decodeRole.Kind = workercore.ModelDeploymentRoleKindDecode
			decodeRole.Replicas = 1
			d.Spec.Roles = []workercore.ModelDeploymentRole{*prefillRole, *decodeRole}
		})
	}

	t.Run("committing decode at t=550s preserves prefill t=0 start timestamp across controller restart", func(t *testing.T) {
		curNow := t0
		md := newTwoRoleDeployment()
		prefillMKA := newNodeStoreKernelArtifact("qwen-decode-forger-prefill-gen-1", md.Namespace, "1", "2", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
		})
		decodeMKA := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", md.Namespace, "3", "4", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
		})
		fin := meta.NewTime(t0.Add(-60 * time.Second))
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
			f.Spec.Rollout.FailureTimeoutSeconds = 600
			f.Spec.Roles = []workercore.ModelDeploymentForgerRoleSpec{
				{Name: "prefill", MaxKernelsPerGeneration: 4, MinSpeedupRatio: "1.05"},
				{Name: "decode", MaxKernelsPerGeneration: 4, MinSpeedupRatio: "1.05"},
			}
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{
				{
					Name:             "prefill",
					Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
					ActiveGeneration: 1,
				},
				{
					Name:             "decode",
					Phase:            workercore.ModelDeploymentForgerRolePhaseIdle,
					ActiveGeneration: 1,
					Generations: []workercore.ModelDeploymentForgerGenerationStatus{{
						Generation: 1,
						Outcome:    workercore.ModelDeploymentForgerGenerationOutcomeSkipped,
						FinishedAt: &fin,
					}},
				},
			}
		})

		cli := newForgerTestFakeClient(md, mdf, prefillMKA, decodeMKA)
		rec1 := &ModelDeploymentForgerReconciler{
			Client:    cli,
			APIReader: cli,
			Recorder:  ctrlrecord.NewFakeRecorder(32),
			Now:       func() time.Time { return curNow },
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}

		// At t = 0: prefill promotes onto md.
		_, err := rec1.Reconcile(context.Background(), req)
		require.NoError(t, err)

		// At t = 550s: decode promotes onto md while prefill is still waiting for canary readiness.
		curNow = t0.Add(550 * time.Second)
		var liveMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		for i := range liveMDF.Status.Roles {
			if liveMDF.Status.Roles[i].Name == "decode" {
				liveMDF.Status.Roles[i].Phase = workercore.ModelDeploymentForgerRolePhasePublishing
				liveMDF.Status.Roles[i].Generations = nil
			}
		}
		require.NoError(t, cli.Status().Update(context.Background(), &liveMDF))

		_, err = rec1.Reconcile(context.Background(), req)
		require.NoError(t, err)

		// At t = 560s: controller restarts (empty in-memory promotions), then ticks at t = 600s.
		curNow = t0.Add(560 * time.Second)
		restarted := &ModelDeploymentForgerReconciler{
			Client:    cli,
			APIReader: cli,
			Recorder:  ctrlrecord.NewFakeRecorder(32),
			Now:       func() time.Time { return curNow },
		}
		_, err = restarted.Reconcile(context.Background(), req)
		require.NoError(t, err)

		curNow = t0.Add(600 * time.Second)
		_, err = restarted.Reconcile(context.Background(), req)
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		prefillRS := findForgerRoleStatus(&liveMDF, "prefill")
		require.NotNil(t, prefillRS)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, prefillRS.Phase)
		require.Len(t, prefillRS.Generations, 1)
		assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeRolledBack, prefillRS.Generations[0].Outcome)
		assert.Equal(t, "RolloutTimeout", prefillRS.Generations[0].Reason)

		decodeRS := findForgerRoleStatus(&liveMDF, "decode")
		require.NotNil(t, decodeRS)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePromoting, decodeRS.Phase,
			"decode promoted at t=550s must still have 550s remaining at t=600s")

		var gotDecodeMKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(decodeMKA), &gotDecodeMKA))
		assert.False(t, gotDecodeMKA.Status.Quarantined)
	})

	t.Run("promoting decode at t=1200s after prefill promoted at t=0 does not adopt t=0", func(t *testing.T) {
		curNow := t0.Add(1200 * time.Second)
		prefillMKA := newNodeStoreKernelArtifact("qwen-decode-forger-prefill-gen-1", "team-a", "5", "6", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
		})
		decodeMKA := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "7", "8", func(k *workercore.ModelKernelArtifact) {
			k.Spec.Compatibility.KernelKeys = []string{key1}
			k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
				Name: "qwen-decode-forger",
				UID:  "mdf-uid-1",
			}
			k.Status.Resolved.ResolvedTime = meta.NewTime(curNow)
		})

		bindRaw, err := json.Marshal(workercore.ModelDeploymentForgerBinding{
			ForgerName: "qwen-decode-forger",
			ForgerUID:  "mdf-uid-1",
			Term:       1,
		})
		require.NoError(t, err)
		decRaw, err := json.Marshal(workercore.ModelDeploymentForgerDecision{
			DecisionID:      formatForgerDecisionID("mdf-uid-1", 1, 1, t0),
			Term:            1,
			KernelArtifacts: map[string]string{"prefill": prefillMKA.Name, "decode": ""},
		})
		require.NoError(t, err)

		md := newTwoRoleDeployment()
		md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: prefillMKA.Name}
		md.Spec.Roles[1].KernelArtifactRef = &core.LocalObjectReference{Name: decodeMKA.Name}
		md.Annotations = map[string]string{
			workercore.ModelDeploymentForgerBindingAnnotationKey:  string(bindRaw),
			workercore.ModelDeploymentForgerDecisionAnnotationKey: string(decRaw),
		}

		fin := meta.NewTime(t0.Add(30 * time.Second))
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
			f.Spec.Rollout.FailureTimeoutSeconds = 600
			f.Spec.Roles = []workercore.ModelDeploymentForgerRoleSpec{
				{Name: "prefill", MaxKernelsPerGeneration: 4, MinSpeedupRatio: "1.05"},
				{Name: "decode", MaxKernelsPerGeneration: 4, MinSpeedupRatio: "1.05"},
			}
			f.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
				TargetUID:  string(md.UID),
				TargetName: md.Name,
				Term:       1,
			}
			f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{
				{
					Name:                 "prefill",
					Phase:                workercore.ModelDeploymentForgerRolePhaseIdle,
					ActiveGeneration:     1,
					ActiveKernelArtifact: prefillMKA.Name,
					Generations: []workercore.ModelDeploymentForgerGenerationStatus{{
						Generation:   1,
						ArtifactName: prefillMKA.Name,
						Outcome:      workercore.ModelDeploymentForgerGenerationOutcomePromoted,
						FinishedAt:   &fin,
					}},
				},
				{
					Name:                 "decode",
					Phase:                workercore.ModelDeploymentForgerRolePhasePromoting,
					ActiveGeneration:     1,
					ActiveKernelArtifact: decodeMKA.Name,
				},
			}
		})

		cli := newForgerTestFakeClient(md, mdf, prefillMKA, decodeMKA)
		r := &ModelDeploymentForgerReconciler{
			Client:    cli,
			APIReader: cli,
			Recorder:  ctrlrecord.NewFakeRecorder(32),
			Now:       func() time.Time { return curNow },
		}
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var gotMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &gotMDF))
		decodeRS := findForgerRoleStatus(&gotMDF, "decode")
		require.NotNil(t, decodeRS)
		assert.Equal(t, workercore.ModelDeploymentForgerRolePhasePromoting, decodeRS.Phase,
			"decode promoted at t=1200s must not adopt prefill t=0 decision timestamp and falsely roll back")

		var gotDecodeMKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(decodeMKA), &gotDecodeMKA))
		assert.False(t, gotDecodeMKA.Status.Quarantined)
	})
}

func TestModelDeploymentForger_SafeLabelsAndPromotionAdmissionRejection(t *testing.T) {
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	now := time.Date(2026, 11, 27, 23, 55, 0, 0, time.UTC)

	t.Run("SafeLabelValue is used consistently across artifact labels, sandbox selectors, and pod watch mapping", func(t *testing.T) {
		longForgerName := "qwen-extremely-long-model-deployment-forger-instance-name-over-sixtythree"
		rawRoleName := "prefill/v1"
		safeForger := modelforger.SafeLabelValue(longForgerName)
		safeRole := modelforger.SafeLabelValue(rawRoleName)
		require.NotEqual(t, longForgerName, safeForger)
		require.NotEqual(t, rawRoleName, safeRole)

		node0 := newForgerTestNode("node-0")
		md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
			d.Spec.Roles[0].Name = rawRoleName
			d.Spec.Roles[0].Replicas = 1
		})
		mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Name = longForgerName
			f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
			f.Spec.Roles = []workercore.ModelDeploymentForgerRoleSpec{
				{Name: rawRoleName, MaxKernelsPerGeneration: 4, MinSpeedupRatio: "1.05"},
			}
		})

		cm, items, err := modelforger.BuildInputConfigMap(modelforger.InputConfigMapSpec{
			Forger:     mdf,
			Name:       "safe-label-gen-1-input",
			RoleName:   rawRoleName,
			Generation: 1,
			Request: modelforger.ForgeRequest{
				SchemaVersion:    modelforger.ForgeRequestSchemaVersion,
				Engine:           "vLLM",
				EngineVersion:    "0.19.0",
				Manufacturer:     nodefeature.ManufacturerNVIDIA,
				Architecture:     "sm_90a",
				TargetKernelKeys: []string{key1},
				MinSpeedupRatio:  1.05,
				Atol:             1e-2,
				Rtol:             1e-2,
			},
		})
		require.NoError(t, err)

		hw := modelkernelkey.NodeHardware{
			Manufacturer:   nodefeature.ManufacturerNVIDIA,
			Arch:           "sm_90a",
			SelectorLabels: map[string]string{"gpu": "h20"},
		}
		sbPod, err := modelforger.BuildSandboxPod(modelforger.SandboxPodSpec{
			Forger:              mdf,
			Name:                "safe-label-gen-1-sandbox",
			RoleName:            rawRoleName,
			Generation:          1,
			InputConfigMapName:  cm.Name,
			InputConfigMapItems: items,
			EntranceQueue:       "default-queue",
			Manufacturer:        nodefeature.ManufacturerNVIDIA,
			Hardware:            hw,
			OperatorImage:       "gpustack/gpustack-operator:test",
			SandboxSpec:         workercore.ModelDeploymentForgerSandboxSpec{RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock},
		})
		require.NoError(t, err)
		sbPod.UID = "sb-safe-uid-1"
		sbPod.Spec.NodeName = "node-0"

		orphanCM := cm.DeepCopy()
		orphanCM.Name = "safe-label-orphan-input"
		orphanPod := sbPod.DeepCopy()
		orphanPod.Name = "safe-label-orphan-sandbox"
		orphanPod.UID = "sb-orphan-uid"

		started := meta.NewTime(now.Add(-10 * time.Second))
		mdf.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
			Name:             rawRoleName,
			Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
			ActiveGeneration: 1,
			ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
				PodName:       sbPod.Name,
				PodUID:        string(sbPod.UID),
				NodeName:      "node-0",
				ConfigMapName: cm.Name,
				StartedAt:     &started,
			},
		}}

		cli := newForgerTestFakeClient(node0, md, mdf, cm, sbPod, orphanCM, orphanPod)
		r := &ModelDeploymentForgerReconciler{
			Client:        cli,
			APIReader:     cli,
			Recorder:      ctrlrecord.NewFakeRecorder(32),
			Now:           func() time.Time { return now },
			OperatorImage: "gpustack/gpustack-operator:test",
			ResolveSeedEndpoint: func(context.Context, string) (string, int, error) {
				return "10.0.0.50", 10250, nil
			},
			IngestClient: &stubIngestClient{
				resp: &modelstore.IngestTreeResponse{
					ManifestDigest:         "sha256:" + strings.Repeat("a", 64),
					DispatchManifestDigest: "sha256:" + strings.Repeat("b", 64),
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               "node-0",
					KernelKeys:             []string{key1},
				},
			},
		}

		// Verify mapPod resolves hashed SandboxLabelInstance back to mdf.Name (with and without OwnerReferences).
		unownedPod := sbPod.DeepCopy()
		unownedPod.OwnerReferences = nil
		wantReqs := []ctrlreconcile.Request{{NamespacedName: types.NamespacedName{Namespace: mdf.Namespace, Name: mdf.Name}}}
		assert.Equal(t, wantReqs, r.mapPod(context.Background(), sbPod))
		assert.Equal(t, wantReqs, r.mapPod(context.Background(), unownedPod))

		// Publishing creates ModelKernelArtifact with SafeLabelValue labels.
		req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)

		artName := forgerArtifactName(mdf.Name, rawRoleName, 1)
		var gotMKA workercore.ModelKernelArtifact
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Namespace: mdf.Namespace, Name: artName}, &gotMKA))
		assert.Equal(t, safeForger, gotMKA.Labels[modelforger.SandboxLabelInstance])
		assert.Equal(t, safeRole, gotMKA.Labels[modelforger.SandboxLabelRole])
		assert.Empty(t, kvalidation.IsValidLabelValue(gotMKA.Labels[modelforger.SandboxLabelInstance]))
		assert.Empty(t, kvalidation.IsValidLabelValue(gotMKA.Labels[modelforger.SandboxLabelRole]))

		// Idle sweeps orphan sandbox Pod and ConfigMap matching SafeLabelValue(roleName).
		var liveMDF workercore.ModelDeploymentForger
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Status.Roles[0].Phase = workercore.ModelDeploymentForgerRolePhaseIdle
		require.NoError(t, cli.Status().Update(context.Background(), &liveMDF))

		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(orphanPod), &core.Pod{})))
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(orphanCM), &core.ConfigMap{})))

		// cleanupAllForgerSandboxes on Suspend deletes sandboxes matching SafeLabelValue(mdf.Name).
		extraPod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: mdf.Namespace,
				Name:      "safe-label-suspend-sandbox",
				Labels: map[string]string{
					modelforger.SandboxLabelName:     "model-deployment-forger",
					modelforger.SandboxLabelInstance: safeForger,
				},
			},
		}
		require.NoError(t, cli.Create(context.Background(), extraPod))
		require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
		liveMDF.Spec.Suspend = true
		liveMDF.Generation++
		require.NoError(t, cli.Update(context.Background(), &liveMDF))
		_, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(extraPod), &core.Pod{})))
	})

	t.Run("admission Invalid or Forbidden error on promotion commit marks generation Failed without wedging in Promoting", func(t *testing.T) {
		admissionErrs := []struct {
			name string
			err  error
		}{
			{
				name: "422 Invalid overlapping kernel volume mount",
				err: kerrors.NewInvalid(
					kschema.GroupKind{Group: workercore.GroupName, Kind: "ModelDeployment"},
					"qwen-pd",
					field.ErrorList{field.Invalid(
						field.NewPath("spec", "roles").Index(0).Child("additionalVolumes").Index(0).Child("mountPath"),
						"/var/lib/gpustack/kernels",
						"overlaps \"/var/lib/gpustack/kernels\"",
					)},
				),
			},
			{
				name: "403 Forbidden admission rejection",
				err: kerrors.NewForbidden(
					workercore.Resource("modeldeployments"),
					"qwen-pd",
					errors.New("kernelArtifactRef forbidden by policy"),
				),
			},
		}

		for _, tc := range admissionErrs {
			t.Run(tc.name, func(t *testing.T) {
				mka := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "9", "a", func(k *workercore.ModelKernelArtifact) {
					k.Spec.Compatibility.KernelKeys = []string{key1}
					k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
						Name: "qwen-decode-forger",
						UID:  "mdf-uid-1",
					}
				})
				md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
					d.Spec.Roles[0].Replicas = 1
				})
				mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
					f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
					f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
					f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
						Name:             "decode",
						Phase:            workercore.ModelDeploymentForgerRolePhasePublishing,
						ActiveGeneration: 1,
					}}
				})

				baseCli := newForgerTestFakeClient(mka, md, mdf)
				cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
					Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
						if updatedMD, isMD := obj.(*workercore.ModelDeployment); isMD &&
							len(updatedMD.Spec.Roles) > 0 && updatedMD.Spec.Roles[0].KernelArtifactRef != nil {
							return tc.err
						}
						return c.Update(ctx, obj, opts...)
					},
				})

				rec := ctrlrecord.NewFakeRecorder(16)
				r := &ModelDeploymentForgerReconciler{
					Client:    cli,
					APIReader: cli,
					Recorder:  rec,
					Now:       func() time.Time { return now },
				}
				req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
				res, err := r.Reconcile(context.Background(), req)
				require.NoError(t, err, "admission rejection must not return an unbounded requeue error")
				assert.Equal(t, effectiveProbeCooldown(mdf), res.RequeueAfter)

				var got workercore.ModelDeploymentForger
				require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
				rs := findForgerRoleStatus(&got, "decode")
				require.NotNil(t, rs)
				assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseIdle, rs.Phase)
				assert.Empty(t, rs.ActiveKernelArtifact)
				assert.Empty(t, rs.PreviousKernelArtifact)
				require.Len(t, rs.Generations, 1)
				assert.Equal(t, workercore.ModelDeploymentForgerGenerationOutcomeFailed, rs.Generations[0].Outcome)

				readyCond := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeReady)
				require.NotNil(t, readyCond)
				assert.Equal(t, meta.ConditionFalse, readyCond.Status)
				assert.Equal(t, workercore.ModelDeploymentForgerReasonDegraded, readyCond.Reason)

				select {
				case ev := <-rec.Events:
					assert.Contains(t, ev, core.EventTypeWarning)
				default:
					t.Fatal("expected Warning event for rejected promotion")
				}
			})
		}
	})
}

func TestModelDeploymentForger_BindingAcquisitionAdmissionRejection(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 58, 0, 0, time.UTC)

	cases := []struct {
		name string
		err  error
	}{
		{
			name: "422 Invalid overlapping kernel volume during binding acquisition",
			err: kerrors.NewInvalid(
				kschema.GroupKind{Group: workercore.GroupName, Kind: "ModelDeployment"},
				"qwen-pd",
				field.ErrorList{field.Invalid(
					field.NewPath("spec", "roles").Index(0).Child("additionalVolumes").Index(0).Child("mountPath"),
					"/var/lib/gpustack/kernels",
					"overlaps \"/var/lib/gpustack/kernels\", where the bound ModelDeploymentForger promotes spec.roles[].kernelArtifactRef",
				)},
			),
		},
		{
			name: "403 Forbidden policy rejection during binding acquisition",
			err: kerrors.NewForbidden(
				workercore.Resource("modeldeployments"),
				"qwen-pd",
				errors.New("forger-binding update forbidden"),
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := newForgerTestDeployment()
			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
			})

			baseCli := newForgerTestFakeClient(md, mdf)
			cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
					if updatedMD, isMD := obj.(*workercore.ModelDeployment); isMD &&
						updatedMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] != "" {
						return tc.err
					}
					return c.Update(ctx, obj, opts...)
				},
			})

			rec := ctrlrecord.NewFakeRecorder(16)
			r := &ModelDeploymentForgerReconciler{
				Client:    cli,
				APIReader: cli,
				Recorder:  rec,
				Now:       func() time.Time { return now },
			}
			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			res, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, forgerIdleRetryInterval, res.RequeueAfter)

			var got workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &got))
			assert.Equal(t, mdf.Generation, got.Status.ObservedGeneration)

			bound := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBound)
			require.NotNil(t, bound)
			assert.Equal(t, meta.ConditionFalse, bound.Status)
			assert.Equal(t, workercore.ModelDeploymentForgerReasonInvalidBinding, bound.Reason)
			assert.Contains(t, bound.Message, tc.err.Error())

			active := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeActive)
			require.NotNil(t, active)
			assert.Equal(t, meta.ConditionFalse, active.Status)
			assert.Equal(t, workercore.ModelDeploymentForgerReasonInvalidBinding, active.Reason)

			blocked := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeBlocked)
			require.NotNil(t, blocked)
			assert.Equal(t, meta.ConditionFalse, blocked.Status)

			ready := findForgerCondition(&got, workercore.ModelDeploymentForgerConditionTypeReady)
			require.NotNil(t, ready)
			assert.Equal(t, meta.ConditionFalse, ready.Status)
			assert.Equal(t, workercore.ModelDeploymentForgerReasonDegraded, ready.Reason)
			assert.Contains(t, ready.Message, tc.err.Error())

			select {
			case ev := <-rec.Events:
				assert.Contains(t, ev, core.EventTypeWarning)
				assert.Contains(t, ev, workercore.ModelDeploymentForgerReasonInvalidBinding)
			default:
				t.Fatal("expected Warning event when binding acquisition is rejected")
			}
		})
	}
}

func TestModelDeploymentForger_DeletionRollsBackUncommittedPromotions(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 59, 0, 0, time.UTC)

	cases := []struct {
		name         string
		prevArtifact string
		wantRef      *core.LocalObjectReference
	}{
		{
			name:         "reverts to previous gen-1 artifact before releasing binding and finalizer",
			prevArtifact: "qwen-decode-forger-decode-gen-1",
			wantRef:      &core.LocalObjectReference{Name: "qwen-decode-forger-decode-gen-1"},
		},
		{
			name:         "clears kernelArtifactRef to stock kernels when previous artifact is empty",
			prevArtifact: "",
			wantRef:      nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const canaryArtifact = "qwen-decode-forger-decode-gen-2"
			bindRaw, err := json.Marshal(workercore.ModelDeploymentForgerBinding{
				ForgerName: "qwen-decode-forger",
				ForgerUID:  "mdf-uid-1",
				Term:       2,
			})
			require.NoError(t, err)
			decRaw, err := json.Marshal(forgerDecisionPayload{
				ModelDeploymentForgerDecision: workercore.ModelDeploymentForgerDecision{
					DecisionID:      formatForgerDecisionID("mdf-uid-1", 2, 1, now.Add(-30*time.Second)),
					Term:            2,
					KernelArtifacts: map[string]string{"decode": canaryArtifact},
				},
				RolePromotedAt: map[string]int64{"decode": now.Add(-30 * time.Second).UnixNano()},
			})
			require.NoError(t, err)

			md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: canaryArtifact}
				d.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey:  string(bindRaw),
					workercore.ModelDeploymentForgerDecisionAnnotationKey: string(decRaw),
				}
			})
			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
				f.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
					TargetUID:  string(md.UID),
					TargetName: md.Name,
					Term:       2,
				}
				f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{{
					Name:                   "decode",
					Phase:                  workercore.ModelDeploymentForgerRolePhasePromoting,
					ActiveGeneration:       2,
					ActiveKernelArtifact:   canaryArtifact,
					PreviousKernelArtifact: tc.prevArtifact,
				}}
			})

			type mdMutation struct {
				ref        *core.LocalObjectReference
				hasBinding bool
			}
			var mutations []mdMutation

			baseCli := newForgerTestFakeClient(md, mdf)
			cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
					if updatedMD, isMD := obj.(*workercore.ModelDeployment); isMD {
						var refCopy *core.LocalObjectReference
						if len(updatedMD.Spec.Roles) > 0 && updatedMD.Spec.Roles[0].KernelArtifactRef != nil {
							cp := *updatedMD.Spec.Roles[0].KernelArtifactRef
							refCopy = &cp
						}
						mutations = append(mutations, mdMutation{
							ref:        refCopy,
							hasBinding: updatedMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey] != "",
						})
					}
					return c.Update(ctx, obj, opts...)
				},
			})

			require.NoError(t, cli.Delete(context.Background(), mdf))

			r := &ModelDeploymentForgerReconciler{
				Client:    cli,
				APIReader: cli,
				Recorder:  ctrlrecord.NewFakeRecorder(16),
				Now:       func() time.Time { return now },
			}
			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), req.NamespacedName, &workercore.ModelDeploymentForger{})))

			var liveMD workercore.ModelDeployment
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
			assert.Equal(t, tc.wantRef, liveMD.Spec.Roles[0].KernelArtifactRef)
			assert.Empty(t, liveMD.Annotations[workercore.ModelDeploymentForgerBindingAnnotationKey])
			assert.Empty(t, liveMD.Annotations[workercore.ModelDeploymentForgerDecisionAnnotationKey])

			require.Len(t, mutations, 2, "rollback must update KernelArtifactRef while holding binding before releasing binding")
			assert.Equal(t, tc.wantRef, mutations[0].ref)
			assert.True(t, mutations[0].hasBinding)
			assert.Equal(t, tc.wantRef, mutations[1].ref)
			assert.False(t, mutations[1].hasBinding)
		})
	}
}

func TestModelDeploymentForger_RemovedRoleRollsBackCanaryAndCleansSandbox(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 59, 30, 0, time.UTC)

	cases := []struct {
		name           string
		canaryArtifact string
		prevArtifact   string
		prevGens       []workercore.ModelDeploymentForgerGenerationStatus
		failFirstWrite bool
		wantPrefillRef *core.LocalObjectReference
	}{
		{
			name:           "reverts removed prefill canary gen-2 to gen-1 and cleans up prefill sandbox while preserving decode",
			canaryArtifact: "qwen-decode-forger-prefill-gen-2",
			prevArtifact:   "qwen-decode-forger-prefill-gen-1",
			prevGens: []workercore.ModelDeploymentForgerGenerationStatus{{
				Generation:   1,
				ArtifactName: "qwen-decode-forger-prefill-gen-1",
				Outcome:      workercore.ModelDeploymentForgerGenerationOutcomePromoted,
			}},
			wantPrefillRef: &core.LocalObjectReference{Name: "qwen-decode-forger-prefill-gen-1"},
		},
		{
			name:           "reverts removed prefill canary gen-1 to nil stock kernels",
			canaryArtifact: "qwen-decode-forger-prefill-gen-1",
			prevArtifact:   "",
			wantPrefillRef: nil,
		},
		{
			name:           "retains removed role in status when rollback fails transiently and completes on retry",
			canaryArtifact: "qwen-decode-forger-prefill-gen-2",
			prevArtifact:   "qwen-decode-forger-prefill-gen-1",
			failFirstWrite: true,
			wantPrefillRef: &core.LocalObjectReference{Name: "qwen-decode-forger-prefill-gen-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const decodeCommitted = "qwen-decode-forger-decode-gen-1"
			bindRaw, err := json.Marshal(workercore.ModelDeploymentForgerBinding{
				ForgerName: "qwen-decode-forger",
				ForgerUID:  "mdf-uid-1",
				Term:       2,
			})
			require.NoError(t, err)
			decRaw, err := json.Marshal(forgerDecisionPayload{
				ModelDeploymentForgerDecision: workercore.ModelDeploymentForgerDecision{
					DecisionID: formatForgerDecisionID("mdf-uid-1", 2, 1, now.Add(-30*time.Second)),
					Term:       2,
					KernelArtifacts: map[string]string{
						"prefill": tc.canaryArtifact,
						"decode":  decodeCommitted,
					},
				},
				RolePromotedAt: map[string]int64{
					"prefill": now.Add(-30 * time.Second).UnixNano(),
					"decode":  now.Add(-10 * time.Minute).UnixNano(),
				},
			})
			require.NoError(t, err)

			md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
				d.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				d.Spec.Roles = []workercore.ModelDeploymentRole{
					{
						Name:              "prefill",
						Kind:              workercore.ModelDeploymentRoleKindPrefill,
						Replicas:          1,
						AcceleratorType:   "h20-8x",
						KernelArtifactRef: &core.LocalObjectReference{Name: tc.canaryArtifact},
					},
					{
						Name:              "decode",
						Kind:              workercore.ModelDeploymentRoleKindDecode,
						Replicas:          1,
						AcceleratorType:   "h20-8x",
						KernelArtifactRef: &core.LocalObjectReference{Name: decodeCommitted},
					},
				}
				d.Annotations = map[string]string{
					workercore.ModelDeploymentForgerBindingAnnotationKey:  string(bindRaw),
					workercore.ModelDeploymentForgerDecisionAnnotationKey: string(decRaw),
				}
			})

			prefillSBName, prefillCMName := forgerSandboxObjectNames("qwen-decode-forger", "prefill", 2, "a1b2c3d4")
			prefillOrphanName, _ := forgerSandboxObjectNames("qwen-decode-forger", "prefill", 1, "deadbeef")
			decodeSBName, decodeCMName := forgerSandboxObjectNames("qwen-decode-forger", "decode", 2, "e5f60718")

			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Generation = 2
				f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
				f.Spec.Roles = []workercore.ModelDeploymentForgerRoleSpec{{
					Name:                    "decode",
					MaxKernelsPerGeneration: 4,
					MinSpeedupRatio:         "1.05",
				}}
				f.Status.ObservedGeneration = 1
				f.Status.Binding = &workercore.ModelDeploymentForgerBindingStatus{
					TargetUID:  string(md.UID),
					TargetName: md.Name,
					Term:       2,
				}
				fin := meta.NewTime(now.Add(-5 * time.Minute))
				f.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{
					{
						Name:                   "prefill",
						Phase:                  workercore.ModelDeploymentForgerRolePhasePromoting,
						ActiveGeneration:       2,
						ActiveKernelArtifact:   tc.canaryArtifact,
						PreviousKernelArtifact: tc.prevArtifact,
						Generations:            tc.prevGens,
						ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
							PodName:       prefillSBName,
							ConfigMapName: prefillCMName,
						},
					},
					{
						Name:                 "decode",
						Phase:                workercore.ModelDeploymentForgerRolePhaseForging,
						ActiveGeneration:     2,
						ActiveKernelArtifact: decodeCommitted,
						ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
							PodName:       decodeSBName,
							PodUID:        "uid-decode-sb",
							ConfigMapName: decodeCMName,
							StartedAt:     ptr.To(meta.NewTime(now.Add(-10 * time.Second))),
						},
						Generations: []workercore.ModelDeploymentForgerGenerationStatus{{
							Generation:   1,
							ArtifactName: decodeCommitted,
							Outcome:      workercore.ModelDeploymentForgerGenerationOutcomePromoted,
							FinishedAt:   &fin,
						}},
					},
				}
			})

			ownerRef := meta.OwnerReference{
				APIVersion: workercore.SchemeGroupVersion.String(),
				Kind:       "ModelDeploymentForger",
				Name:       mdf.Name,
				UID:        mdf.UID,
				Controller: ptr.To(true),
			}
			prefillSBPod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Namespace:       mdf.Namespace,
					Name:            prefillSBName,
					UID:             "uid-prefill-sb",
					OwnerReferences: []meta.OwnerReference{ownerRef},
					Labels: map[string]string{
						modelforger.SandboxLabelName:     "model-deployment-forger",
						modelforger.SandboxLabelInstance: mdf.Name,
						modelforger.SandboxLabelRole:     "prefill",
					},
				},
			}
			prefillOrphanPod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Namespace:       mdf.Namespace,
					Name:            prefillOrphanName,
					UID:             "uid-prefill-orphan",
					OwnerReferences: []meta.OwnerReference{ownerRef},
					Labels: map[string]string{
						modelforger.SandboxLabelName:     "model-deployment-forger",
						modelforger.SandboxLabelInstance: mdf.Name,
						modelforger.SandboxLabelRole:     "prefill",
					},
				},
			}
			prefillSBCM := &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Namespace:       mdf.Namespace,
					Name:            prefillCMName,
					OwnerReferences: []meta.OwnerReference{ownerRef},
					Labels: map[string]string{
						modelforger.SandboxLabelName:     "model-deployment-forger",
						modelforger.SandboxLabelInstance: mdf.Name,
						modelforger.SandboxLabelRole:     "prefill",
					},
				},
			}
			decodeSBPod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Namespace:       mdf.Namespace,
					Name:            decodeSBName,
					UID:             "uid-decode-sb",
					OwnerReferences: []meta.OwnerReference{ownerRef},
					Labels: map[string]string{
						modelforger.SandboxLabelName:     "model-deployment-forger",
						modelforger.SandboxLabelInstance: mdf.Name,
						modelforger.SandboxLabelRole:     "decode",
					},
				},
			}
			decodeSBCM := &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Namespace:       mdf.Namespace,
					Name:            decodeCMName,
					OwnerReferences: []meta.OwnerReference{ownerRef},
					Labels: map[string]string{
						modelforger.SandboxLabelName:     "model-deployment-forger",
						modelforger.SandboxLabelInstance: mdf.Name,
						modelforger.SandboxLabelRole:     "decode",
					},
				},
			}

			var failedOnce atomic.Bool
			baseCli := newForgerTestFakeClient(md, mdf, prefillSBPod, prefillOrphanPod, prefillSBCM, decodeSBPod, decodeSBCM)
			cli := interceptor.NewClient(baseCli.(ctrlcli.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
					if _, isMD := obj.(*workercore.ModelDeployment); isMD && tc.failFirstWrite && failedOnce.CompareAndSwap(false, true) {
						return errors.New("transient apiserver conflict")
					}
					return c.Update(ctx, obj, opts...)
				},
			})

			r := &ModelDeploymentForgerReconciler{
				Client:    cli,
				APIReader: cli,
				Recorder:  ctrlrecord.NewFakeRecorder(16),
				Now:       func() time.Time { return now },
			}
			r.rememberProbe(mdf.UID, "prefill", forgerProbeAttempt{PodName: "prefill-0", StartedAt: now})
			r.rememberPromotion(mdf.UID, "prefill", forgerPromotionAttempt{ArtifactName: tc.canaryArtifact, StartedAt: now})

			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			if tc.failFirstWrite {
				_, err = r.Reconcile(context.Background(), req)
				require.Error(t, err)

				var midMDF workercore.ModelDeploymentForger
				require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &midMDF))
				require.NotNil(t, findForgerRoleStatus(&midMDF, "prefill"),
					"removed role must stay in status.roles until rollback succeeds")
			}

			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			_, hasProbe := r.loadProbe(mdf.UID, "prefill")
			assert.False(t, hasProbe)
			_, hasPromo := r.loadPromotion(mdf.UID, "prefill")
			assert.False(t, hasPromo)

			assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(prefillSBPod), &core.Pod{})))
			assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(prefillOrphanPod), &core.Pod{})))
			assert.True(t, kerrors.IsNotFound(cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(prefillSBCM), &core.ConfigMap{})))
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(decodeSBPod), &core.Pod{}))
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(decodeSBCM), &core.ConfigMap{}))

			var liveMD workercore.ModelDeployment
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), &liveMD))
			assert.Equal(t, tc.wantPrefillRef, liveMD.Spec.Roles[0].KernelArtifactRef)
			assert.Equal(t, &core.LocalObjectReference{Name: decodeCommitted}, liveMD.Spec.Roles[1].KernelArtifactRef)

			decAfter, hasDecAfter, decErr := readForgerDecision(&liveMD)
			require.NoError(t, decErr)
			require.True(t, hasDecAfter)
			_, prefillInDec := decAfter.KernelArtifacts["prefill"]
			assert.False(t, prefillInDec)
			_, prefillInPromotedAt := decAfter.RolePromotedAt["prefill"]
			assert.False(t, prefillInPromotedAt)
			assert.Equal(t, decodeCommitted, decAfter.KernelArtifacts["decode"])

			var liveMDF workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &liveMDF))
			assert.Nil(t, findForgerRoleStatus(&liveMDF, "prefill"))
			decodeRS := findForgerRoleStatus(&liveMDF, "decode")
			require.NotNil(t, decodeRS)
			assert.Equal(t, decodeCommitted, decodeRS.ActiveKernelArtifact)
		})
	}
}

func TestModelDeploymentForger_LaunchForgeSandboxPodSpecRendering(t *testing.T) {
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
	ms := newMockSentinelServer(t, key1)
	payload := ms.ProbePayload

	findInitContainer := func(t *testing.T, pod *core.Pod, name string) core.Container {
		t.Helper()
		for i := range pod.Spec.InitContainers {
			if pod.Spec.InitContainers[i].Name == name {
				return pod.Spec.InitContainers[i]
			}
		}
		t.Fatalf("initContainer %q not found in pod %s", name, pod.Name)
		return core.Container{}
	}

	cases := []struct {
		name                 string
		mutateAT             func(*worker.AcceleratorType)
		rolePullSecrets      []core.LocalObjectReference
		sandboxPullSecrets   []core.LocalObjectReference
		resources            *workercore.ModelDeploymentForgerSandboxResources
		wantPullSecrets      []core.LocalObjectReference
		wantResourceRequests map[core.ResourceName]resource.Quantity
		absentResources      []core.ResourceName
	}{
		{
			name:            "exclusive default renders whole GPU with runner args env envFrom and role imagePullSecrets fallback",
			rolePullSecrets: []core.LocalObjectReference{{Name: "role-pull-secret"}},
			resources: &workercore.ModelDeploymentForgerSandboxResources{
				CPU:          ptr.To(resource.MustParse("8")),
				RAM:          ptr.To(resource.MustParse("32Gi")),
				LocalStorage: ptr.To(resource.MustParse("20Gi")),
			},
			wantPullSecrets: []core.LocalObjectReference{{Name: "role-pull-secret"}},
			wantResourceRequests: map[core.ResourceName]resource.Quantity{
				"nvidia.com/gpu":              resource.MustParse("1"),
				core.ResourceCPU:              resource.MustParse("8"),
				core.ResourceMemory:           resource.MustParse("32Gi"),
				core.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			},
			absentResources: []core.ResourceName{
				"nvidia.com/gpu.sliced",
				"nvidia.com/gpu.partitioned",
			},
		},
		{
			name: "sliced acceleratorSlicedMemoryPercentage 50 renders sliced GPU resources and merges sandbox imagePullSecrets",
			mutateAT: func(at *worker.AcceleratorType) {
				at.Status.Detail.SlicedDetail.Logical.Count = 128
			},
			rolePullSecrets:    []core.LocalObjectReference{{Name: "role-pull-secret"}},
			sandboxPullSecrets: []core.LocalObjectReference{{Name: "sandbox-pull-secret"}, {Name: "role-pull-secret"}},
			resources: &workercore.ModelDeploymentForgerSandboxResources{
				AcceleratorSlicedMemoryPercentage: 50,
			},
			wantPullSecrets: []core.LocalObjectReference{
				{Name: "role-pull-secret"},
				{Name: "sandbox-pull-secret"},
			},
			wantResourceRequests: map[core.ResourceName]resource.Quantity{
				"nvidia.com/gpu.sliced":                   resource.MustParse("1"),
				"nvidia.com/gpu.sliced.memory-percentage": resource.MustParse("50"),
			},
			absentResources: []core.ResourceName{
				"nvidia.com/gpu",
				"nvidia.com/gpu.partitioned",
			},
		},
		{
			name: "partitioned acceleratorPartitionedProfile 3g.40gb renders MIG partitioned GPU resources",
			mutateAT: func(at *worker.AcceleratorType) {
				at.Status.Detail.Memory = "80Gi"
				at.Status.Detail.SlicedDetail.Physical = workercore.AcceleratorSlicedPhysicalDetail{
					Count: 2,
					Profiles: []workercore.AcceleratorSlicedPhysicalDetailProfile{
						{Name: "3g.40gb", MemoryMib: 40 << 10, Count: 2},
					},
				}
			},
			sandboxPullSecrets: []core.LocalObjectReference{{Name: "mig-pull-secret"}},
			resources: &workercore.ModelDeploymentForgerSandboxResources{
				AcceleratorPartitionedProfile: "3g.40gb",
			},
			wantPullSecrets: []core.LocalObjectReference{{Name: "mig-pull-secret"}},
			wantResourceRequests: map[core.ResourceName]resource.Quantity{
				"nvidia.com/gpu.partitioned":             resource.MustParse("1"),
				"nvidia.com/gpu.partitioned.mig-3g.40gb": resource.MustParse("1"),
			},
			absentResources: []core.ResourceName{
				"nvidia.com/gpu",
				"nvidia.com/gpu.sliced",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v1AT := newRenderAcceleratorType(func(at *worker.AcceleratorType) {
				at.Name = "h20-sandbox-at"
				at.Status.Entrance = "queue-for-h20-sandbox"
				at.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
				at.Status.Detail.Memory = "80Gi"
				if tc.mutateAT != nil {
					tc.mutateAT(at)
				}
			})
			v1alpha1AT := (*workercore.AcceleratorType)(v1AT.DeepCopy())
			v1alpha1AT.TypeMeta = meta.TypeMeta{}

			node0 := newForgerTestNode("node-0")
			md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0].Replicas = 1
				d.Spec.Roles[0].AcceleratorType = v1AT.Name
				d.Spec.Roles[0].ImagePullSecrets = slices.Clone(tc.rolePullSecrets)
			})
			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
				f.Spec.Probe.MinActiveRequests = 0
				f.Spec.Sandbox = workercore.ModelDeploymentForgerSandboxSpec{
					RunnerMode:            workercore.ModelDeploymentForgerRunnerModeExternal,
					ActiveDeadlineSeconds: 900,
					ImagePullSecrets:      slices.Clone(tc.sandboxPullSecrets),
					Resources:             tc.resources,
					Runner: &workercore.ModelDeploymentForgerSandboxRunner{
						Image:   "registry.local/forge-runner:v1",
						Command: []string{"/usr/local/bin/forge-runner"},
						Args:    []string{"--beam-width", "4", "--fast"},
						Env: []workercore.ModelDeploymentForgerSandboxEnvVar{
							{Name: "FORGE_OPT_LEVEL", Value: "3"},
							{
								Name: "FORGE_API_KEY",
								ValueFrom: &workercore.ModelDeploymentForgerSandboxEnvVarSource{
									SecretKeyRef: &core.SecretKeySelector{
										LocalObjectReference: core.LocalObjectReference{Name: "runner-secret"},
										Key:                  "api-key",
									},
								},
							},
						},
						EnvFrom: []core.EnvFromSource{
							{
								Prefix: "CFG_",
								ConfigMapRef: &core.ConfigMapEnvSource{
									LocalObjectReference: core.LocalObjectReference{Name: "runner-config"},
								},
							},
						},
					},
				}
			})
			leaderPod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Namespace: md.Namespace,
					Name:      "qwen-pd-decode-0",
					UID:       "leader-render-uid",
					Labels: map[string]string{
						modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
						modelDeploymentLabelKeyInstance:    md.Name,
						modelDeploymentLabelKeyComponent:   "decode",
						modelDeploymentReplicaOrdinalLabel: "0",
					},
				},
				Spec: core.PodSpec{NodeName: "node-0", Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
			}

			cli := newForgerTestFakeClient(v1AT, v1alpha1AT, node0, md, mdf, leaderPod)
			markServingPodReadyEligible(t, cli, leaderPod, "node-0", "10.0.0.10", 8000)

			r := &ModelDeploymentForgerReconciler{
				Client:        cli,
				APIReader:     cli,
				Recorder:      ctrlrecord.NewFakeRecorder(32),
				Now:           func() time.Time { return now },
				OperatorImage: "gpustack/gpustack-operator:test",
				ProbeClient: &stubProbeClient{
					readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
					startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
				},
			}

			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			var gotMDF workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &gotMDF))
			rs := findForgerRoleStatus(&gotMDF, "decode")
			require.NotNil(t, rs)
			assert.Equal(t, workercore.ModelDeploymentForgerRolePhaseForging, rs.Phase)
			require.NotNil(t, rs.ActiveSandbox)

			var sbPod core.Pod
			require.NoError(t, cli.Get(context.Background(), types.NamespacedName{
				Namespace: mdf.Namespace,
				Name:      rs.ActiveSandbox.PodName,
			}, &sbPod))

			require.NotNil(t, sbPod.Spec.ActiveDeadlineSeconds)
			assert.Equal(t, int64(900), *sbPod.Spec.ActiveDeadlineSeconds)
			assert.Equal(t, tc.wantPullSecrets, sbPod.Spec.ImagePullSecrets)

			runnerCtr := findInitContainer(t, &sbPod, modelforger.RunnerContainerName)
			verifierCtr := findInitContainer(t, &sbPod, modelforger.VerifierContainerName)

			assert.Equal(t, "registry.local/forge-runner:v1", runnerCtr.Image)
			assert.Equal(t, []string{"/usr/local/bin/forge-runner"}, runnerCtr.Command)
			assert.Equal(t, []string{"--beam-width", "4", "--fast"}, runnerCtr.Args)
			require.Len(t, runnerCtr.EnvFrom, 1)
			assert.Equal(t, "CFG_", runnerCtr.EnvFrom[0].Prefix)
			require.NotNil(t, runnerCtr.EnvFrom[0].ConfigMapRef)
			assert.Equal(t, "runner-config", runnerCtr.EnvFrom[0].ConfigMapRef.Name)

			envByName := make(map[string]core.EnvVar, len(runnerCtr.Env))
			for _, ev := range runnerCtr.Env {
				envByName[ev.Name] = ev
			}
			assert.Equal(t, "3", envByName["FORGE_OPT_LEVEL"].Value)
			require.NotNil(t, envByName["FORGE_API_KEY"].ValueFrom)
			require.NotNil(t, envByName["FORGE_API_KEY"].ValueFrom.SecretKeyRef)
			assert.Equal(t, "runner-secret", envByName["FORGE_API_KEY"].ValueFrom.SecretKeyRef.Name)
			assert.Equal(t, "api-key", envByName["FORGE_API_KEY"].ValueFrom.SecretKeyRef.Key)
			assert.Equal(t, modelforger.InputMountPath, envByName["GPUSTACK_FORGE_INPUT_DIR"].Value)
			assert.Equal(t, modelforger.OutputMountPath, envByName["GPUSTACK_FORGE_OUTPUT_DIR"].Value)

			for _, ctr := range []core.Container{runnerCtr, verifierCtr} {
				for resName, wantQty := range tc.wantResourceRequests {
					gotReq, hasReq := ctr.Resources.Requests[resName]
					require.True(t, hasReq, "container %s missing request %s", ctr.Name, resName)
					assert.True(t, wantQty.Equal(gotReq), "container %s request %s: want %s, got %s", ctr.Name, resName, wantQty.String(), gotReq.String())

					gotLim, hasLim := ctr.Resources.Limits[resName]
					require.True(t, hasLim, "container %s missing limit %s", ctr.Name, resName)
					assert.True(t, wantQty.Equal(gotLim), "container %s limit %s: want %s, got %s", ctr.Name, resName, wantQty.String(), gotLim.String())
				}
				for _, absent := range tc.absentResources {
					_, hasReq := ctr.Resources.Requests[absent]
					assert.False(t, hasReq, "container %s unexpectedly requested %s", ctr.Name, absent)
					_, hasLim := ctr.Resources.Limits[absent]
					assert.False(t, hasLim, "container %s unexpectedly limited %s", ctr.Name, absent)
				}
			}
		})
	}
}

func TestModelDeploymentForger_RunnerFingerprintQuarantineReforge(t *testing.T) {
	t.Run("forgerRunnerFingerprint is nil-safe and sensitive to image command args env and envFrom", func(t *testing.T) {
		assert.Len(t, forgerRunnerFingerprint(nil), 16)

		base := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Spec.Sandbox.RunnerMode = workercore.ModelDeploymentForgerRunnerModeExternal
			f.Spec.Sandbox.Runner = &workercore.ModelDeploymentForgerSandboxRunner{
				Image:   "registry.local/forge-runner:v1",
				Command: []string{"/bin/runner"},
				Args:    []string{"--beam", "2"},
				Env:     []workercore.ModelDeploymentForgerSandboxEnvVar{{Name: "OPT", Value: "1"}},
				EnvFrom: []core.EnvFromSource{{
					ConfigMapRef: &core.ConfigMapEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "cm-a"}},
				}},
			}
		})
		baseFP := forgerRunnerFingerprint(base)
		assert.Len(t, baseFP, 16)

		nilRunner := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
			f.Spec.Sandbox.RunnerMode = workercore.ModelDeploymentForgerRunnerModeExternal
			f.Spec.Sandbox.Runner = nil
		})
		assert.NotEqual(t, baseFP, forgerRunnerFingerprint(nilRunner))

		mutations := []struct {
			name   string
			mutate func(*workercore.ModelDeploymentForgerSandboxRunner)
		}{
			{
				name:   "image changed",
				mutate: func(r *workercore.ModelDeploymentForgerSandboxRunner) { r.Image = "registry.local/forge-runner:v2" },
			},
			{
				name:   "command changed",
				mutate: func(r *workercore.ModelDeploymentForgerSandboxRunner) { r.Command = []string{"/bin/runner-v2"} },
			},
			{
				name:   "args changed",
				mutate: func(r *workercore.ModelDeploymentForgerSandboxRunner) { r.Args = []string{"--beam", "8"} },
			},
			{
				name: "env changed",
				mutate: func(r *workercore.ModelDeploymentForgerSandboxRunner) {
					r.Env = []workercore.ModelDeploymentForgerSandboxEnvVar{{Name: "OPT_V2", Value: "1"}}
				},
			},
			{
				name: "envFrom changed",
				mutate: func(r *workercore.ModelDeploymentForgerSandboxRunner) {
					r.EnvFrom = []core.EnvFromSource{{
						ConfigMapRef: &core.ConfigMapEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "cm-b"}},
					}}
				},
			},
		}
		for _, mc := range mutations {
			cp := base.DeepCopy()
			mc.mutate(cp.Spec.Sandbox.Runner)
			assert.NotEqual(t, baseFP, forgerRunnerFingerprint(cp), mc.name)
		}
	})

	t.Run("updating runner.image or runner.args re-forges previously quarantined keys cleanly", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 19, 0, 0, 0, time.UTC)
		key1 := buildTestKernelKey(t, modelkernelkey.OpFamilyRMSNorm, 4096)
		ms := newMockSentinelServer(t, key1)
		payload := ms.ProbePayload

		cases := []struct {
			name              string
			mutateRunner      func(*workercore.ModelDeploymentForgerSandboxRunner)
			wantPhase         workercore.ModelDeploymentForgerRolePhase
			wantActiveSandbox bool
			wantOutcome       workercore.ModelDeploymentForgerGenerationOutcome
		}{
			{
				name:              "unchanged runner suppresses quarantined key",
				mutateRunner:      func(*workercore.ModelDeploymentForgerSandboxRunner) {},
				wantPhase:         workercore.ModelDeploymentForgerRolePhaseIdle,
				wantActiveSandbox: false,
				wantOutcome:       workercore.ModelDeploymentForgerGenerationOutcomeSkipped,
			},
			{
				name: "updated runner.image re-forges quarantined key",
				mutateRunner: func(r *workercore.ModelDeploymentForgerSandboxRunner) {
					r.Image = "registry.local/forge-runner:v2"
				},
				wantPhase:         workercore.ModelDeploymentForgerRolePhaseForging,
				wantActiveSandbox: true,
			},
			{
				name: "updated runner.args re-forges quarantined key",
				mutateRunner: func(r *workercore.ModelDeploymentForgerSandboxRunner) {
					r.Args = []string{"--search-budget", "64"}
				},
				wantPhase:         workercore.ModelDeploymentForgerRolePhaseForging,
				wantActiveSandbox: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				node0 := newForgerTestNode("node-0")
				md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
					d.Spec.Roles[0].Replicas = 1
				})
				mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
					f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
					f.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
					f.Spec.Probe.MinActiveRequests = 0
					f.Spec.Sandbox.RunnerMode = workercore.ModelDeploymentForgerRunnerModeExternal
					f.Spec.Sandbox.Runner = &workercore.ModelDeploymentForgerSandboxRunner{
						Image: "registry.local/forge-runner:v1",
						Args:  []string{"--search-budget", "16"},
					}
				})
				initialFP := forgerRunnerFingerprint(mdf)

				quarantinedMKA := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", md.Namespace, "1", "2", func(k *workercore.ModelKernelArtifact) {
					k.Labels = map[string]string{
						modelforger.SandboxLabelName:       "model-deployment-forger",
						modelforger.SandboxLabelInstance:   mdf.Name,
						modelforger.SandboxLabelRole:       "decode",
						modelforger.SandboxLabelGeneration: "1",
					}
					k.Annotations = map[string]string{
						forgerRunnerFingerprintAnnotationKey: initialFP,
						forgerTargetDeploymentAnnotationKey:  md.Name,
					}
					k.Spec.Compatibility.KernelKeys = []string{key1}
					k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
						Name: mdf.Name,
						UID:  string(mdf.UID),
					}
					k.Status.Quarantined = true
					k.Status.QuarantineReason = "UnsupportedKernelArtifact"
				})

				tc.mutateRunner(mdf.Spec.Sandbox.Runner)
				mdf.Generation++

				leaderPod := &core.Pod{
					ObjectMeta: meta.ObjectMeta{
						Namespace: md.Namespace,
						Name:      "qwen-pd-decode-0",
						UID:       "leader-q-uid",
						Labels: map[string]string{
							modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
							modelDeploymentLabelKeyInstance:    md.Name,
							modelDeploymentLabelKeyComponent:   "decode",
							modelDeploymentReplicaOrdinalLabel: "0",
						},
					},
					Spec: core.PodSpec{NodeName: "node-0", Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
				}

				cli := newForgerTestFakeClient(node0, md, mdf, quarantinedMKA, leaderPod)
				markServingPodReadyEligible(t, cli, leaderPod, "node-0", "10.0.0.10", 8000)

				r := &ModelDeploymentForgerReconciler{
					Client:        cli,
					APIReader:     cli,
					Recorder:      ctrlrecord.NewFakeRecorder(32),
					Now:           func() time.Time { return now },
					OperatorImage: "gpustack/gpustack-operator:test",
					ProbeClient: &stubProbeClient{
						readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
						startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
					},
				}

				req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
				_, err := r.Reconcile(context.Background(), req)
				require.NoError(t, err)

				var gotMDF workercore.ModelDeploymentForger
				require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &gotMDF))
				rs := findForgerRoleStatus(&gotMDF, "decode")
				require.NotNil(t, rs)
				assert.Equal(t, tc.wantPhase, rs.Phase)
				if tc.wantActiveSandbox {
					require.NotNil(t, rs.ActiveSandbox)
					assert.Equal(t, int64(2), rs.ActiveGeneration)
				} else {
					assert.Nil(t, rs.ActiveSandbox)
					require.Len(t, rs.Generations, 1)
					assert.Equal(t, tc.wantOutcome, rs.Generations[0].Outcome)
				}
			})
		}
	})
}

func TestModelDeploymentForger_PatchUpgradeReusesArtifactWhileMinorUpgradeReforges(t *testing.T) {
	now := time.Date(2026, 11, 27, 23, 55, 0, 0, time.UTC)

	buildKey := func(ver string) string {
		k, err := modelkernelkey.Build(modelkernelkey.BuildSpec{
			Engine:        "vLLM",
			EngineVersion: ver,
			Manufacturer:  nodefeature.ManufacturerNVIDIA,
			Arch:          "sm_90a",
			OpFamily:      modelkernelkey.OpFamilyRMSNorm,
			ConstAxes:     map[string]int64{"hidden_size": 4096},
			InputDtypes:   []string{"bfloat16", "bfloat16"},
			OutputDtypes:  []string{"bfloat16"},
		})
		require.NoError(t, err)
		return k.String()
	}

	key085 := buildKey("0.8.5")
	key086 := buildKey("0.8.6")
	require.Equal(t, key085, key086)

	cases := []struct {
		name              string
		deploymentVersion string
		probeKey          string
		wantPhase         workercore.ModelDeploymentForgerRolePhase
		wantActiveSandbox bool
		wantOutcome       workercore.ModelDeploymentForgerGenerationOutcome
	}{
		{
			name:              "patch upgrade 0.8.5 to 0.8.6 reuses covered vllm/0.8 artifact",
			deploymentVersion: "0.8.6",
			probeKey:          key086,
			wantPhase:         workercore.ModelDeploymentForgerRolePhaseIdle,
			wantActiveSandbox: false,
			wantOutcome:       workercore.ModelDeploymentForgerGenerationOutcomeSkipped,
		},
		{
			name:              "minor upgrade 0.8.5 to 0.9.0 triggers new ForgeSandbox",
			deploymentVersion: "0.9.0",
			probeKey:          buildKey("0.9.0"),
			wantPhase:         workercore.ModelDeploymentForgerRolePhaseForging,
			wantActiveSandbox: true,
		},
		{
			name:              "pre-release 0.8.6rc1 does not match release vllm/0.8 artifact",
			deploymentVersion: "0.8.6rc1",
			probeKey:          buildKey("0.8.6rc1"),
			wantPhase:         workercore.ModelDeploymentForgerRolePhaseForging,
			wantActiveSandbox: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node0 := newForgerTestNode("node-0")
			mdf := newTestModelDeploymentForger(func(f *workercore.ModelDeploymentForger) {
				f.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
				f.Spec.Mode = workercore.ModelDeploymentForgerModeActive
				f.Spec.Probe.MinActiveRequests = 0
			})
			coveredMKA := newNodeStoreKernelArtifact("qwen-decode-forger-decode-gen-1", "team-a", "1", "2", func(k *workercore.ModelKernelArtifact) {
				k.Labels = map[string]string{
					modelforger.SandboxLabelName:       "model-deployment-forger",
					modelforger.SandboxLabelInstance:   mdf.Name,
					modelforger.SandboxLabelRole:       "decode",
					modelforger.SandboxLabelGeneration: "1",
				}
				k.Spec.Compatibility.EngineVersion = "0.8"
				k.Spec.Compatibility.KernelKeys = []string{key085}
				k.Spec.Source.NodeStore.ForgerRef = workercore.ModelKernelArtifactForgerReference{
					Name: mdf.Name,
					UID:  string(mdf.UID),
				}
			})
			md := newForgerTestDeployment(func(d *workercore.ModelDeployment) {
				d.Spec.Engine.Version = tc.deploymentVersion
				d.Spec.Engine.Image = "vllm/vllm-openai:v" + tc.deploymentVersion
				d.Spec.Roles[0].Image = "vllm/vllm-openai:v" + tc.deploymentVersion
				d.Spec.Roles[0].Replicas = 1
				d.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: coveredMKA.Name}
			})
			leaderPod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Namespace: md.Namespace,
					Name:      "qwen-pd-decode-0",
					UID:       "leader-patch-uid",
					Labels: map[string]string{
						modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
						modelDeploymentLabelKeyInstance:    md.Name,
						modelDeploymentLabelKeyComponent:   "decode",
						modelDeploymentReplicaOrdinalLabel: "0",
					},
				},
				Spec: core.PodSpec{NodeName: "node-0", Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
			}

			cli := newForgerTestFakeClient(node0, md, mdf, coveredMKA, leaderPod)
			markServingPodReadyEligible(t, cli, leaderPod, "node-0", "10.0.0.10", 8000)

			payload := &runtimecontrol.ProbePayload{
				SchemaVersion: modelforger.FlashInferTraceSchemaVersion,
				Engine:        "vLLM",
				EngineVersion: tc.deploymentVersion,
				Manufacturer:  nodefeature.ManufacturerNVIDIA,
				Architecture:  "sm_90a",
				Bottlenecks: []runtimecontrol.ProbeBottleneck{{
					LocalKernelKey:      tc.probeKey,
					OpFamily:            modelkernelkey.OpFamilyRMSNorm,
					GPUTimeUs:           180,
					GPUTimeSharePercent: 25,
					CallCount:           8,
				}},
			}
			r := &ModelDeploymentForgerReconciler{
				Client:        cli,
				APIReader:     cli,
				Recorder:      ctrlrecord.NewFakeRecorder(32),
				Now:           func() time.Time { return now },
				OperatorImage: "gpustack/gpustack-operator:test",
				ProbeClient: &stubProbeClient{
					readyResp: &runtimecontrol.ReadyResponse{Ready: true, BootID: "b1"},
					startResp: &runtimecontrol.ProbeResponse{State: runtimecontrol.ProbeStateCompleted, BootID: "b1", Payload: payload},
				},
			}

			req := ctrl.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(mdf)}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			var gotMDF workercore.ModelDeploymentForger
			require.NoError(t, cli.Get(context.Background(), req.NamespacedName, &gotMDF))
			rs := findForgerRoleStatus(&gotMDF, "decode")
			require.NotNil(t, rs)
			assert.Equal(t, tc.wantPhase, rs.Phase)
			if tc.wantActiveSandbox {
				require.NotNil(t, rs.ActiveSandbox)
				assert.Equal(t, int64(2), rs.ActiveGeneration)
			} else {
				assert.Nil(t, rs.ActiveSandbox)
				require.Len(t, rs.Generations, 1)
				assert.Equal(t, tc.wantOutcome, rs.Generations[0].Outcome)
			}
		})
	}
}
