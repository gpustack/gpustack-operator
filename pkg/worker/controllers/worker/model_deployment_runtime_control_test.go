package worker

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	gpustackv1 "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

type mockSentinelEngine struct {
	mu sync.Mutex

	engine string

	bootID            string
	rankCount         int
	rankAppliedDigest []string
	appliedDigest     string
	acked             bool
	observedGen       int64
	paused            bool

	supportedFields   []string
	unsupportedFields []string
	rejectFields422   []string

	schedulingState   map[string]any
	lastAppliedParams map[string]any
	lastResetFields   []string
	lastOperationID   string

	runningReqs float64
	waitingReqs float64
	failMetrics bool
	unreachable bool

	pauseBlockNext          bool
	pauseDelay              time.Duration
	pauseSuppressStateApply bool

	applyCalls  int
	ackCalls    int
	pauseCalls  int
	resumeCalls int
}

func newMockSentinelEngine(
	t *testing.T,
	engine, bootID string,
	ranks int,
) (*mockSentinelEngine, *httptest.Server, string, int32) {
	t.Helper()
	if ranks <= 0 {
		ranks = 1
	}
	mock := &mockSentinelEngine{
		engine:            engine,
		bootID:            bootID,
		rankCount:         ranks,
		rankAppliedDigest: make([]string, ranks),
		schedulingState:   make(map[string]any),
	}
	if engine == workercore.ModelDeploymentEngineSGLang {
		mock.supportedFields = []string{
			runtimecontrol.ParamSchedulingMaxActiveRequests,
			runtimecontrol.ParamSchedulingMaxBatchedTokens,
			runtimecontrol.ParamSchedulingPrefillMaxRequests,
			runtimecontrol.ParamSchedulingScheduleConservativeness,
			runtimecontrol.ParamSchedulingPrefillDecodeInterval,
			runtimecontrol.ParamSchedulingPPMaxMicroBatchSize,
			runtimecontrol.ParamSpeculativeAcceptThresholdSingle,
			runtimecontrol.ParamSpeculativeAcceptThresholdAcc,
			runtimecontrol.ParamSpeculativeDraftBudgetFraction,
		}
	} else {
		mock.supportedFields = []string{
			runtimecontrol.ParamSchedulingMaxActiveRequests,
			runtimecontrol.ParamSchedulingMaxBatchedTokens,
			runtimecontrol.ParamSchedulingLongPrefillTokenThreshold,
			runtimecontrol.ParamSchedulingCacheAwareWindow,
			runtimecontrol.ParamSchedulingCacheAwareThreshold,
			runtimecontrol.ParamSchedulingMaxInflightKVLoads,
			runtimecontrol.ParamSchedulingMaxAsyncKVLoadsPerStep,
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(mock.serveHTTP))
	t.Cleanup(srv.Close)

	parsedURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(parsedURL.Host)
	require.NoError(t, err)
	portNum, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return mock, srv, host, int32(portNum)
}

func (m *mockSentinelEngine) setBootID(newBootID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bootID = newBootID
	m.acked = false
	m.appliedDigest = ""
	m.observedGen = 0
	m.paused = false
	m.schedulingState = make(map[string]any)
	for i := range m.rankAppliedDigest {
		m.rankAppliedDigest[i] = ""
	}
}

func (m *mockSentinelEngine) expandRanks(newTotal int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.rankAppliedDigest) < newTotal {
		m.rankAppliedDigest = append(m.rankAppliedDigest, "")
	}
	m.rankCount = newTotal
}

func (m *mockSentinelEngine) counts() (apply, ack, pause, resume int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyCalls, m.ackCalls, m.pauseCalls, m.resumeCalls
}

func (m *mockSentinelEngine) snapshotLocked() runtimecontrol.RuntimeConfigState {
	ranks := make([]runtimecontrol.RankReceipt, len(m.rankAppliedDigest))
	for i, dig := range m.rankAppliedDigest {
		ranks[i] = runtimecontrol.RankReceipt{
			RankID:        i,
			BootID:        m.bootID,
			AppliedDigest: dig,
		}
	}
	var obsGenPtr *int64
	if m.acked {
		g := m.observedGen
		obsGenPtr = &g
	}
	return runtimecontrol.RuntimeConfigState{
		Engine:              strings.ToLower(m.engine),
		BootID:              m.bootID,
		Ready:               m.acked && !m.paused,
		ObservedGeneration:  obsGenPtr,
		AppliedConfigDigest: m.appliedDigest,
		LastOperationID:     m.lastOperationID,
		Paused:              m.paused,
		SupportedFields:     slices.Clone(m.supportedFields),
		UnsupportedFields:   slices.Clone(m.unsupportedFields),
		Ranks:               ranks,
		Values:              maps.Clone(m.schedulingState),
	}
}

func (m *mockSentinelEngine) serveHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.unreachable {
		m.mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	m.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		m.mu.Lock()
		fail := m.failMetrics
		run := m.runningReqs
		wait := m.waitingReqs
		eng := m.engine
		m.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		if eng == workercore.ModelDeploymentEngineSGLang {
			_, _ = fmt.Fprintf(w, "sglang:num_running_reqs %v\nsglang:num_queue_reqs %v\n", run, wait)
		} else {
			_, _ = fmt.Fprintf(w, "vllm:num_requests_running %v\nvllm:num_requests_waiting %v\n", run, wait)
		}

	case r.Method == http.MethodGet && r.URL.Path == "/gpustack/v1/ready":
		m.mu.Lock()
		ready := m.acked && !m.paused
		reason := "ready"
		if m.paused {
			reason = "role_paused"
		} else if !m.acked {
			reason = "awaiting_ack"
		}
		bootID := m.bootID
		var obsGenPtr *int64
		if m.acked {
			g := m.observedGen
			obsGenPtr = &g
		}
		dig := m.appliedDigest
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(runtimecontrol.ReadyResponse{
			Ready:               ready,
			Reason:              reason,
			BootID:              bootID,
			ObservedGeneration:  obsGenPtr,
			AppliedConfigDigest: dig,
		})

	case r.Method == http.MethodGet && r.URL.Path == "/gpustack/v1/live":
		m.mu.Lock()
		paused := m.paused
		bootID := m.bootID
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runtimecontrol.LiveResponse{
			Alive:  true,
			BootID: bootID,
			Paused: paused,
		})

	case r.Method == http.MethodGet && r.URL.Path == "/gpustack/v1/pause":
		m.mu.Lock()
		paused := m.paused
		bootID := m.bootID
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runtimecontrol.PauseResponse{
			Paused: paused,
			BootID: bootID,
		})

	case r.Method == http.MethodPost && r.URL.Path == "/gpustack/v1/pause":
		var req runtimecontrol.PauseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if req.ExpectedBootID != "" && req.ExpectedBootID != m.bootID {
			m.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		block := m.pauseBlockNext
		delay := m.pauseDelay
		suppress := m.pauseSuppressStateApply
		m.pauseBlockNext = false
		if req.Paused {
			m.pauseCalls++
		} else {
			m.resumeCalls++
		}
		if !suppress {
			m.paused = req.Paused
		}
		pausedNow := m.paused
		bootID := m.bootID
		m.mu.Unlock()

		if block && delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runtimecontrol.PauseResponse{
			Paused: pausedNow,
			BootID: bootID,
		})

	case r.Method == http.MethodGet && r.URL.Path == "/gpustack/v1/runtime_config":
		m.mu.Lock()
		state := m.snapshotLocked()
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)

	case r.Method == http.MethodPost && r.URL.Path == "/gpustack/v1/runtime_config":
		var req runtimecontrol.ApplyRuntimeConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if req.ExpectedBootID != "" && req.ExpectedBootID != m.bootID {
			m.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		if len(m.rejectFields422) > 0 {
			unsup := slices.Clone(m.rejectFields422)
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":              "unsupported_fields",
				"message":            "fields unsupported by this engine version",
				"unsupported_fields": unsup,
			})
			return
		}
		m.applyCalls++
		m.lastAppliedParams = maps.Clone(req.Parameters)
		m.lastResetFields = slices.Clone(req.ResetFields)
		for _, rf := range req.ResetFields {
			delete(m.schedulingState, rf)
		}
		for fullKey, v := range req.Parameters {
			m.schedulingState[fullKey] = v
		}
		m.appliedDigest = req.AppliedConfigDigest
		m.lastOperationID = req.OperationID
		for i := range m.rankAppliedDigest {
			m.rankAppliedDigest[i] = req.AppliedConfigDigest
		}
		state := m.snapshotLocked()
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)

	case r.Method == http.MethodPost && r.URL.Path == "/gpustack/v1/ack":
		var req runtimecontrol.AckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if req.BootID != m.bootID {
			m.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		m.ackCalls++
		m.acked = true
		m.observedGen = req.ObservedGeneration
		m.appliedDigest = req.AppliedConfigDigest
		ready := !m.paused
		reason := "ready"
		if m.paused {
			reason = "role_paused"
		}
		g := m.observedGen
		resp := runtimecontrol.ReadyResponse{
			Ready:               ready,
			Reason:              reason,
			BootID:              m.bootID,
			ObservedGeneration:  &g,
			AppliedConfigDigest: m.appliedDigest,
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newRuntimeControlDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	return newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Generation = 1
		md.Spec.Roles[0].Replicas = 1
		for _, fn := range mutate {
			fn(md)
		}
	})
}

func newRuntimeControlReconciler(cli ctrlcli.Client, opts ...runtimecontrol.ClientOption) *ModelDeploymentReconciler {
	return &ModelDeploymentReconciler{
		Client:               cli,
		APIReader:            cli,
		Recorder:             ctrlrecord.NewFakeRecorder(64),
		RuntimeControlClient: runtimecontrol.NewClient(opts...),
		groupForwardFetch: func(_ context.Context, _ string, reqBody []byte) ([]byte, error) {
			var parsed struct {
				RequestID string `json:"request_id"`
				RID       string `json:"rid"`
			}
			_ = json.Unmarshal(reqBody, &parsed)
			if parsed.RID != "" {
				return json.Marshal(map[string]any{
					"meta_info": map[string]any{
						"id":            parsed.RID,
						"finish_reason": map[string]any{"type": "length"},
					},
				})
			}
			return json.Marshal(map[string]any{
				"id": "cmpl-" + parsed.RequestID,
				"choices": []map[string]any{
					{"finish_reason": "length"},
				},
			})
		},
	}
}

func runReconcilePass(t *testing.T, r *ModelDeploymentReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "qwen"},
	})
	require.NoError(t, err)
	return res
}

func markDeploymentPodsRunningWithServer(
	t *testing.T,
	cli ctrlcli.Client,
	host string,
	port int32,
	ready bool,
	restartCount int32,
) []*core.Pod {
	t.Helper()
	var list core.PodList
	require.NoError(t, cli.List(context.Background(), &list, ctrlcli.InNamespace("team-a")))
	var out []*core.Pod
	for i := range list.Items {
		pod := list.Items[i].DeepCopy()
		if pod.Labels[modelDeploymentRouterLabelKey] != "" {
			continue
		}
		pod.Status.Phase = core.PodRunning
		pod.Status.PodIP = host
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations["prometheus.io/port"] = strconv.Itoa(int(port))
		for ci := range pod.Spec.Containers {
			for pi := range pod.Spec.Containers[ci].Ports {
				if pod.Spec.Containers[ci].Ports[pi].ContainerPort == 8000 {
					pod.Spec.Containers[ci].Ports[pi].ContainerPort = port
				}
			}
		}
		require.NoError(t, cli.Update(context.Background(), pod))

		fresh := new(core.Pod)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), fresh))
		fresh.Status.Phase = core.PodRunning
		fresh.Status.PodIP = host
		condStatus := core.ConditionFalse
		if ready {
			condStatus = core.ConditionTrue
		}
		fresh.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: condStatus}}
		fresh.Status.ContainerStatuses = []core.ContainerStatus{{
			Name:         modelDeploymentMainContainerName,
			Ready:        ready,
			RestartCount: restartCount,
		}}
		require.NoError(t, cli.Status().Update(context.Background(), fresh))
		out = append(out, fresh)
	}
	return out
}

func conditionOf(md *workercore.ModelDeployment, condType string) *gpustackv1.Condition {
	for i := range md.Status.Conditions {
		if md.Status.Conditions[i].Type == condType {
			return &md.Status.Conditions[i]
		}
	}
	return nil
}

// 1. Zero auth Secrets are created or referenced even when --api-key and runtimeControl are set.
func TestModelDeploymentRuntimeControl_ZeroAuthSecretsCreated(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-1", 1)
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--api-key=user-infer-secret", "--max-num-seqs=64"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	runReconcilePass(t, r)
	runReconcilePass(t, r)
	pods := markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	require.Len(t, pods, 1)
	runReconcilePass(t, r)

	var secrets core.SecretList
	require.NoError(t, cli.List(context.Background(), &secrets, ctrlcli.InNamespace("team-a")))
	assert.Empty(t, secrets.Items, "reconciler must never create per-deployment runtime-control auth Secrets")

	for _, envVar := range pods[0].Spec.Containers[0].Env {
		if envVar.ValueFrom != nil {
			assert.Nil(t, envVar.ValueFrom.SecretKeyRef, "sentinel Pods must not reference operator auth Secrets")
		}
	}
	assert.Equal(t, 1, mockEng.applyCalls)
	assert.Equal(t, 1, mockEng.ackCalls)
}

// 2. Default sentinel cold-start ACK when runtimeControl is nil and pause is false.
func TestModelDeploymentRuntimeControl_DefaultSentinelColdStartAck(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-default-1", 1)
	md := newRuntimeControlDeployment()
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	runReconcilePass(t, r)
	runReconcilePass(t, r)

	// Pod starts with PodReady=False because /gpustack/v1/ready returns 503 awaiting_ack.
	pods := markDeploymentPodsRunningWithServer(t, cli, host, port, false, 0)
	require.Len(t, pods, 1)
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(pods, true)))

	res := runReconcilePass(t, r)
	assert.Equal(t, 1, mockEng.ackCalls, "controller must ACK unconfigured sentinel Pods on cold start")
	assert.Zero(t, mockEng.applyCalls, "unconfigured sentinel Pods must not receive runtime_config mutations")
	assert.NotZero(t, res.RequeueAfter, "pass must requeue while waiting for kubelet PodReady=True")

	liveMD := getModelDeployment(t, cli)
	cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionTrue, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlNotConfigured, cond.Reason)
	assert.Nil(t, liveMD.Status.Roles[0].RuntimeControl)

	var cms core.ConfigMapList
	require.NoError(t, cli.List(context.Background(), &cms, ctrlcli.InNamespace("team-a")))
	assert.Empty(t, cms.Items, "default unconfigured sentinel bootstrap must create zero ledger ConfigMaps")

	// Once kubelet observes /gpustack/v1/ready 200 and marks PodReady=True, deployment reaches Ready.
	markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	runReconcilePass(t, r)

	liveMD = getModelDeployment(t, cli)
	assert.Equal(t, ModelDeploymentPhaseReady, liveMD.Status.Phase)
	cond = conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionTrue, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlNotConfigured, cond.Reason)
}

// 3. Router-first rollout gate holds EngineSentinelEnabled=false until Envoy router Deployment converges.
func TestModelDeploymentRuntimeControl_RouterFirstRolloutGate(t *testing.T) {
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:     workercore.ModelDeploymentRouterVLLM,
			Replicas: ptr.To[int32](2),
		}
		md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "16",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	runReconcilePass(t, r)
	runReconcilePass(t, r)

	liveMD := getModelDeployment(t, cli)
	cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionUnknown, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlWaitingForRouterIsolation, cond.Reason)

	var podList core.PodList
	require.NoError(t, cli.List(context.Background(), &podList, ctrlcli.InNamespace("team-a")))
	require.Len(t, podList.Items, 1)
	heldPod := &podList.Items[0]
	assert.Equal(t, "vllm", heldPod.Spec.Containers[0].Command[0])
	assert.False(t, modelDeploymentPodHasRuntimeControlEnable(heldPod))

	// Router Deployment carries the Envoy allowlist proxy + :9091 internal observer listener.
	routerDep := new(app.Deployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}, routerDep))
	require.Len(t, routerDep.Spec.Template.Spec.Containers, 2)
	assert.Equal(t, "envoy", routerDep.Spec.Template.Spec.Containers[0].Name)
	assert.Equal(t, "router", routerDep.Spec.Template.Spec.Containers[1].Name)

	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload([]*core.Pod{heldPod}, true)))
	routerDep.Status.ObservedGeneration = routerDep.Generation
	routerDep.Status.Replicas = 2
	routerDep.Status.UpdatedReplicas = 2
	routerDep.Status.ReadyReplicas = 2
	require.NoError(t, cli.Status().Update(context.Background(), routerDep))

	runReconcilePass(t, r)
	runReconcilePass(t, r)

	require.NoError(t, cli.List(context.Background(), &podList, ctrlcli.InNamespace("team-a")))
	require.Len(t, podList.Items, 1)
	releasedPod := &podList.Items[0]
	assert.True(t, modelDeploymentPodHasRuntimeControlEnable(releasedPod))
	assert.Equal(t, "vllm", releasedPod.Spec.Containers[0].Command[0],
		"gpustack-engine-sentinel injects via PYTHONPATH sitecustomize without wrapping container argv")
	assert.Equal(t, "/gpustack/v1/ready", releasedPod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, "/gpustack/v1/live", releasedPod.Spec.Containers[0].LivenessProbe.HTTPGet.Path)
}

// 4. Dual-hash upgrade currency & zero worker rollout on router-only edits.
func TestModelDeploymentRuntimeControl_DualHashUpgradeCurrencyAndRouterOnlyEdit(t *testing.T) {
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:                  workercore.ModelDeploymentRouterVLLM,
			Replicas:              ptr.To[int32](2),
			RequestTimeoutSeconds: ptr.To[int32](90),
			ExtraArgs:             []string{"--log-level=debug"},
		}
	})
	at := newRenderAcceleratorType()

	renderedPod, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: at,
		Ordinal:         0,
		Member:          0,
	})
	require.NoError(t, err)
	legacyHashes := modelDeploymentDesiredPodLegacyHashes(md, &md.Spec.Roles[0], 0, 0, renderedPod)
	require.NotEmpty(t, legacyHashes)
	legacyHash := legacyHashes[0]
	require.NotEqual(t, legacyHash, renderedPod.Annotations[modelDeploymentPodSpecHashAnnotation])

	preUpgradePod := renderedPod.DeepCopy()
	preUpgradePod.Name = "qwen-server-legacy-0"
	preUpgradePod.UID = "legacy-pod-uid"
	preUpgradePod.Annotations[modelDeploymentPodSpecHashAnnotation] = legacyHash
	preUpgradePod.Annotations[modelDeploymentRenderVersionAnnotation] = "5"
	preUpgradePod.Status.Phase = core.PodRunning
	preUpgradePod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}

	cli := newModelDeploymentClient(md, at, preUpgradePod)
	r := newRuntimeControlReconciler(cli)
	runReconcilePass(t, r)
	runReconcilePass(t, r)

	stillThere := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(preUpgradePod), stillThere))
	assert.Equal(t, types.UID("legacy-pod-uid"), stillThere.UID)

	projectedPod := stillThere.DeepCopy()
	projectedPod.Annotations[modelDeploymentPodSpecHashAnnotation] = renderedPod.Annotations[modelDeploymentPodSpecHashAnnotation]
	require.NoError(t, cli.Update(context.Background(), projectedPod))

	liveMD := getModelDeployment(t, cli)
	liveMD.Spec.Router.Replicas = ptr.To[int32](4)
	liveMD.Spec.Router.RequestTimeoutSeconds = ptr.To[int32](180)
	liveMD.Spec.Router.ExtraArgs = []string{"--log-level=info"}
	liveMD.Generation++
	require.NoError(t, cli.Update(context.Background(), liveMD))

	runReconcilePass(t, r)

	afterRouterEdit := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(preUpgradePod), afterRouterEdit))
	assert.Equal(t, types.UID("legacy-pod-uid"), afterRouterEdit.UID, "router-only field edits must not roll worker Pods")

	routerDep := new(app.Deployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}, routerDep))
	assert.Equal(t, int32(4), *routerDep.Spec.Replicas)

	routerDep.Status.ObservedGeneration = routerDep.Generation
	routerDep.Status.Replicas = 4
	routerDep.Status.UpdatedReplicas = 4
	routerDep.Status.ReadyReplicas = 4
	require.NoError(t, cli.Status().Update(context.Background(), routerDep))
	runReconcilePass(t, r)

	cond := conditionOf(getModelDeployment(t, cli), string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionTrue, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlNotConfigured, cond.Reason)
	assert.Equal(t, ModelDeploymentPhaseReady, getModelDeployment(t, cli).Status.Phase)
}

// 5. External-DP parallelism classification with sentinel + model-transfer wrapper and group convergence gate.
func TestModelDeploymentRuntimeControl_ExternalDPParallelismAndGroupGate(t *testing.T) {
	at := newRenderAcceleratorType()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Generation = 1
		md.Spec.Roles[0].Replicas = 1
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles[0].ExtraArgs = []string{
			"--data-parallel-size=2",
			"--data-parallel-size-local=1",
			"--data-parallel-external-lb",
			"--max-num-seqs=64",
		}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
			},
		}
	})

	for _, tc := range []struct {
		name     string
		artifact *ModelDeploymentArtifactRender
	}{
		{name: "sentinel without model-transfer"},
		{
			name: "sentinel composed with model-transfer shell wrapper",
			artifact: &ModelDeploymentArtifactRender{
				Delivery:             workercore.ModelDeploymentModelDeliveryRemote,
				ModelTransferPolicy:  "Require",
				SourceReady:          true,
				ModelTransferBackend: "nixl",
				ArtifactName:         "qwen-weights",
				ArtifactUID:          "art-uid-1",
				ManifestDigest:       "sha256:fea7da0c5227e595df7f4fa5822011e7a33cabe1dff13e3ffa7be6898dd1c363",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p0, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: at, Artifact: tc.artifact, Ordinal: 0, Member: 0,
			})
			require.NoError(t, err)
			p1, err := renderModelDeploymentPod(t.Context(), ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: at, Artifact: tc.artifact, Ordinal: 0, Member: 1,
			})
			require.NoError(t, err)
			view := modelDeploymentReplicaView{Ordinal: 0, Seated: true, Members: []*core.Pod{p0, p1}}
			assert.Equal(t, modelDeploymentAnsweringAll, modelDeploymentDeployedAnsweringShape(view, workercore.ModelDeploymentEngineVLLM))
		})
	}

	// Verify group convergence gate: rank 0 stays ineligible while rank 1 sentinel is unreachable.
	mock0, _, host0, port0 := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-dp-0", 1)
	mock1, _, host1, port1 := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-dp-1", 1)
	mock1.unreachable = true

	cli := newModelDeploymentClient(md, at)
	r := newRuntimeControlReconciler(cli)
	runReconcilePass(t, r)
	runReconcilePass(t, r)

	var podList core.PodList
	require.NoError(t, cli.List(context.Background(), &podList, ctrlcli.InNamespace("team-a")))
	require.Len(t, podList.Items, 2)
	slices.SortFunc(podList.Items, func(a, b core.Pod) int { return strings.Compare(a.Name, b.Name) })

	hosts := []string{host0, host1}
	ports := []int32{port0, port1}
	ptrs := make([]*core.Pod, 2)
	for i := range podList.Items {
		pod := podList.Items[i].DeepCopy()
		pod.Annotations["prometheus.io/port"] = strconv.Itoa(int(ports[i]))
		require.NoError(t, cli.Update(context.Background(), pod))
		fresh := new(core.Pod)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), fresh))
		fresh.Status.Phase = core.PodRunning
		fresh.Status.PodIP = hosts[i]
		fresh.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
		fresh.Status.ContainerStatuses = []core.ContainerStatus{{Name: modelDeploymentMainContainerName, Ready: true}}
		require.NoError(t, cli.Status().Update(context.Background(), fresh))
		ptrs[i] = fresh
	}
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(ptrs, true)))

	runReconcilePass(t, r)
	assert.Equal(t, 1, mock0.ackCalls)
	p0Live := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(ptrs[0]), p0Live))
	assert.NotEqual(t, modelDeploymentEndpointEligibleValue, p0Live.Labels[modelDeploymentLabelKeyEndpointEligible],
		"rank 0 must stay withheld while rank 1 sentinel has not converged")

	mock1.mu.Lock()
	mock1.unreachable = false
	mock1.mu.Unlock()

	runReconcilePass(t, r)
	assert.Equal(t, 1, mock1.ackCalls)
	for _, p := range ptrs {
		live := new(core.Pod)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(p), live))
		assert.Equal(t, modelDeploymentEndpointEligibleValue, live.Labels[modelDeploymentLabelKeyEndpointEligible],
			"both External-DP seats must open together once every seat converges")
	}
}

// 6. Boot-local readiness latch on cold start and same-UID container restart replay + immediate revocation on failure.
func TestModelDeploymentRuntimeControl_BootLatchColdStartAndSameUIDRestart(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-v1", 2)
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64", "--max-num-batched-tokens=8192"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests:   "24",
				runtimecontrol.ParamSchedulingMaxBatchedTokens:    "4096",
				runtimecontrol.ParamSchedulingCacheAwareThreshold: "0.5",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	runReconcilePass(t, r)
	runReconcilePass(t, r)

	// Pod starts with PodReady=False behind un-ACKed boot latch.
	pods := markDeploymentPodsRunningWithServer(t, cli, host, port, false, 0)
	require.Len(t, pods, 1)
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(pods, true)))

	runReconcilePass(t, r)
	assert.Equal(t, 1, mockEng.applyCalls)
	assert.Equal(t, 1, mockEng.ackCalls)
	assert.Equal(t, float64(24), mockEng.lastAppliedParams[runtimecontrol.ParamSchedulingMaxActiveRequests])
	assert.Equal(t, float64(0.5), mockEng.lastAppliedParams[runtimecontrol.ParamSchedulingCacheAwareThreshold])

	// Kubelet marks PodReady=True once /gpustack/v1/ready turns 200.
	markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	runReconcilePass(t, r)

	livePod := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.Equal(t, modelDeploymentEndpointEligibleValue, livePod.Labels[modelDeploymentLabelKeyEndpointEligible])

	liveMD := getModelDeployment(t, cli)
	rcStatus := liveMD.Status.Roles[0].RuntimeControl
	require.NotNil(t, rcStatus)
	assert.True(t, rcStatus.Synced)
	assert.False(t, rcStatus.Paused)
	assert.Equal(t, int32(1), rcStatus.ConvergedPods)
	assert.Equal(t, int32(1), rcStatus.TotalAnsweringPods)

	// Simulate same-UID container restart where sentinel endpoint is temporarily unreachable:
	// endpoint-eligible must be immediately revoked before kubelet even flips PodReady!
	mockEng.setBootID("boot-v2")
	mockEng.mu.Lock()
	mockEng.unreachable = true
	mockEng.mu.Unlock()
	markDeploymentPodsRunningWithServer(t, cli, host, port, true, 1)
	runReconcilePass(t, r)

	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.NotEqual(t, modelDeploymentEndpointEligibleValue, livePod.Labels[modelDeploymentLabelKeyEndpointEligible],
		"same-UID restart with un-ACKed boot_id must immediately revoke endpoint-eligible")

	// Sentinel endpoint recovers on boot-v2: controller replays parameters, re-ACKs boot-v2, and restores eligibility.
	mockEng.mu.Lock()
	mockEng.unreachable = false
	mockEng.mu.Unlock()
	runReconcilePass(t, r)

	assert.Equal(t, 2, mockEng.applyCalls)
	assert.Equal(t, 2, mockEng.ackCalls)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.Equal(t, modelDeploymentEndpointEligibleValue, livePod.Labels[modelDeploymentLabelKeyEndpointEligible])

	owner := runtimecontrol.LedgerOwnerFromModelDeployment(getModelDeployment(t, cli))
	ledger, err := r.runtimeControlLedgerStore().Read(context.Background(), owner, "server")
	require.NoError(t, err)
	entry, ok := ledger.GetEntry(pods[0].Name)
	require.True(t, ok)
	assert.Equal(t, "boot-v2", entry.BootID)
	assert.Equal(t, int32(1), entry.RestartCount)
}

// 7. Role-level pause=true and pause=false on a routed SGLang sentinel role with router observation & drain.
func TestModelDeploymentRuntimeControl_RolePauseSentinelAndRouterDrainAndResume(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineSGLang, "boot-sg-1", 1)
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:     workercore.ModelDeploymentRouterSGLang,
			Replicas: ptr.To[int32](1),
		}
		md.Spec.Roles[0].ExtraArgs = []string{"--max-running-requests=64"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests:      "32",
				runtimecontrol.ParamSpeculativeAcceptThresholdSingle: "0.5",
				runtimecontrol.ParamSpeculativeDraftBudgetFraction:   "0.8",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	var routerServedPod sync.Map
	r.servingViewFetch = func(_ context.Context, _ string) ([]byte, error) {
		workers := make([]map[string]any, 0)
		routerServedPod.Range(func(k, v any) bool {
			uid := k.(string)
			addr := v.(string)
			workers = append(workers, map[string]any{
				"url":       "http://" + addr,
				"port":      strconv.Itoa(int(port)),
				"role":      "server",
				"pod_hint":  uid,
				"worker_id": uid,
				"selection": map[string]any{
					"registered":   true,
					"healthy":      true,
					"circuit_open": false,
					"in_selection": true,
				},
			})
			return true
		})
		return json.Marshal(map[string]any{
			"router":            map[string]any{"boot_generation": 1, "now_ms": 1},
			"registry_revision": 1,
			"workers":           workers,
		})
	}

	// Converge router Deployment first so sentinel activates on worker Pod.
	runReconcilePass(t, r)
	runReconcilePass(t, r)
	var podList core.PodList
	require.NoError(t, cli.List(context.Background(), &podList, ctrlcli.InNamespace("team-a")))
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload([]*core.Pod{&podList.Items[0]}, true)))

	routerDep := new(app.Deployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}, routerDep))
	routerDep.UID = "router-dep-uid"
	require.NoError(t, cli.Update(context.Background(), routerDep))
	routerDep.Status.ObservedGeneration = routerDep.Generation
	routerDep.Status.Replicas = 1
	routerDep.Status.UpdatedReplicas = 1
	routerDep.Status.ReadyReplicas = 1
	require.NoError(t, cli.Status().Update(context.Background(), routerDep))

	routerPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "qwen-router-pod-0",
			Namespace: "team-a",
			UID:       "router-pod-uid-0",
			Labels: map[string]string{
				modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance: "qwen",
				modelDeploymentRouterLabelKey:   workercore.ModelDeploymentRouterSGLang,
			},
			Annotations: routerDep.Spec.Template.Annotations,
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "qwen-router-rs",
				UID:        "router-rs-uid",
				Controller: ptr.To(true),
			}},
		},
		Spec: routerDep.Spec.Template.Spec,
		Status: core.PodStatus{
			Phase:      core.PodRunning,
			PodIP:      "10.244.0.99",
			Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}},
		},
	}
	routerRS := &app.ReplicaSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      "qwen-router-rs",
			Namespace: "team-a",
			UID:       "router-rs-uid",
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       routerDep.Name,
				UID:        routerDep.UID,
				Controller: ptr.To(true),
			}},
		},
	}
	require.NoError(t, cli.Create(context.Background(), routerRS))
	require.NoError(t, cli.Create(context.Background(), routerPod))

	runReconcilePass(t, r)
	runReconcilePass(t, r)

	// Mark worker Pod running with restartCount=2 so incarnation-relative drain is exercised.
	pods := markDeploymentPodsRunningWithServer(t, cli, host, port, true, 2)
	require.Len(t, pods, 1)
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(pods, true)))
	routerServedPod.Store(string(pods[0].UID), net.JoinHostPort(host, strconv.Itoa(int(port))))

	runReconcilePass(t, r)
	livePod := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.Equal(t, modelDeploymentEndpointEligibleValue, livePod.Labels[modelDeploymentLabelKeyEndpointEligible])

	// Request role.Pause = true while 3 requests are running and router still observes the endpoint.
	mockEng.mu.Lock()
	mockEng.runningReqs = 3
	mockEng.mu.Unlock()

	liveMD := getModelDeployment(t, cli)
	liveMD.Generation = 2
	liveMD.Spec.Roles[0].Pause = true
	require.NoError(t, cli.Update(context.Background(), liveMD))

	// Pass 1: Withdraws endpoint-eligible, router-candidate, and api-answering; does not call SetPause yet.
	runReconcilePass(t, r)
	_, _, pauseCount, _ := mockEng.counts()
	assert.Zero(t, pauseCount)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.NotContains(t, livePod.Labels, modelDeploymentLabelKeyEndpointEligible)
	assert.NotContains(t, livePod.Labels, modelDeploymentRouterCandidateLabel)
	assert.NotContains(t, livePod.Labels, modelDeploymentLabelKeyAPIAnswering)

	// Pass 2: Router still reports residual endpoint -> holds at Draining.
	runReconcilePass(t, r)
	_, _, pauseCount, _ = mockEng.counts()
	assert.Zero(t, pauseCount)
	cond := conditionOf(getModelDeployment(t, cli), string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlDraining, cond.Reason)

	// Pass 3: Router confirms 0 endpoints, but 3 requests are still running -> holds at Draining.
	routerServedPod.Delete(string(pods[0].UID))
	runReconcilePass(t, r)
	_, _, pauseCount, _ = mockEng.counts()
	assert.Zero(t, pauseCount)
	cond = conditionOf(getModelDeployment(t, cli), string(ModelDeploymentConditionRuntimeControlSynced))
	assert.Equal(t, ModelDeploymentReasonRuntimeControlDraining, cond.Reason)

	// Pass 4: Requests finish draining -> calls POST /gpustack/v1/pause and records Paused=true.
	mockEng.mu.Lock()
	mockEng.runningReqs = 0
	mockEng.mu.Unlock()
	runReconcilePass(t, r)
	_, _, pauseCount, _ = mockEng.counts()
	assert.Equal(t, 1, pauseCount)

	liveMD = getModelDeployment(t, cli)
	require.NotNil(t, liveMD.Status.Roles[0].RuntimeControl)
	assert.True(t, liveMD.Status.Roles[0].RuntimeControl.Synced)
	assert.True(t, liveMD.Status.Roles[0].RuntimeControl.Paused)
	assert.Equal(t, int64(2), liveMD.Status.Roles[0].RuntimeControl.ObservedGeneration)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.NotContains(t, livePod.Labels, modelDeploymentLabelKeyEndpointEligible)

	// Return role.Pause = false and update maxActiveRequests to 48: clears pause, replays params, re-ACKs, re-opens eligibility.
	liveMD.Generation = 3
	liveMD.Spec.Roles[0].Pause = false
	liveMD.Spec.Roles[0].RuntimeControl.Parameters[runtimecontrol.ParamSchedulingMaxActiveRequests] = "48"
	require.NoError(t, cli.Update(context.Background(), liveMD))

	runReconcilePass(t, r)
	_, _, _, resumeCount := mockEng.counts()
	assert.Equal(t, 1, resumeCount)
	assert.Equal(t, float64(48), mockEng.lastAppliedParams[runtimecontrol.ParamSchedulingMaxActiveRequests])

	liveMD = getModelDeployment(t, cli)
	require.NotNil(t, liveMD.Status.Roles[0].RuntimeControl)
	assert.True(t, liveMD.Status.Roles[0].RuntimeControl.Synced)
	assert.False(t, liveMD.Status.Roles[0].RuntimeControl.Paused)
	assert.Equal(t, int64(3), liveMD.Status.Roles[0].RuntimeControl.ObservedGeneration)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.Equal(t, modelDeploymentEndpointEligibleValue, livePod.Labels[modelDeploymentLabelKeyEndpointEligible])
}

// 8. Role-level pause=true and pause=false works on non-sentinel CPU-only and custom-command roles.
func TestModelDeploymentRuntimeControl_RolePauseNonSentinelRole(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutateRole func(*workercore.ModelDeploymentRole)
		wantDrain  bool
	}{
		{
			name: "CPU-only managed role",
			mutateRole: func(role *workercore.ModelDeploymentRole) {
				role.AcceleratorType = ""
				role.Resources = &workercore.ModelDeploymentRoleResources{
					CPU: ptr.To(resource.MustParse("4")),
					RAM: ptr.To(resource.MustParse("16Gi")),
				}
			},
			wantDrain: true,
		},
		{
			name: "unmanaged custom command role",
			mutateRole: func(role *workercore.ModelDeploymentRole) {
				role.Command = []string{"/bin/custom-serve"}
			},
			wantDrain: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "unused", 1)
			md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
				tc.mutateRole(&md.Spec.Roles[0])
			})
			cli := newModelDeploymentClient(md, newRenderAcceleratorType())
			r := newRuntimeControlReconciler(cli)

			runReconcilePass(t, r)
			pods := markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
			require.Len(t, pods, 1)
			assert.False(t, modelDeploymentPodHasRuntimeControlEnable(pods[0]))
			if pods[0].Labels["kueue.x-k8s.io/pod-group-name"] != "" {
				require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(pods, true)))
			}
			runReconcilePass(t, r)

			mockEng.mu.Lock()
			mockEng.runningReqs = 2
			mockEng.mu.Unlock()

			liveMD := getModelDeployment(t, cli)
			liveMD.Generation = 2
			liveMD.Spec.Roles[0].Pause = true
			require.NoError(t, cli.Update(context.Background(), liveMD))

			// Pass 1 withdraws routing labels and activates Service eligibility gating.
			runReconcilePass(t, r)
			svc := new(core.Service)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-server"}, svc))
			if svc.Spec.Selector[modelDeploymentLabelKeyAPIAnswering] == modelDeploymentAPIAnsweringValue {
				assert.Equal(t, modelDeploymentEndpointEligibleValue, svc.Annotations[modelDeploymentEligibilityActiveAnnotation])
			} else {
				assert.Equal(t, modelDeploymentEndpointEligibleValue, svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible])
			}

			livePod := new(core.Pod)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
			assert.NotContains(t, livePod.Labels, modelDeploymentLabelKeyEndpointEligible)
			assert.NotContains(t, livePod.Labels, modelDeploymentLabelKeyAPIAnswering)

			// Pass 2 checks drain when managed, or converges immediately when custom-command.
			runReconcilePass(t, r)
			if tc.wantDrain {
				cond := conditionOf(getModelDeployment(t, cli), string(ModelDeploymentConditionRuntimeControlSynced))
				assert.Equal(t, ModelDeploymentReasonRuntimeControlDraining, cond.Reason)
				mockEng.mu.Lock()
				mockEng.runningReqs = 0
				mockEng.mu.Unlock()
				runReconcilePass(t, r)
			}

			assert.Zero(t, mockEng.pauseCalls, "non-sentinel roles must never receive /gpustack/v1/pause HTTP calls")
			liveMD = getModelDeployment(t, cli)
			require.NotNil(t, liveMD.Status.Roles[0].RuntimeControl)
			assert.True(t, liveMD.Status.Roles[0].RuntimeControl.Synced)
			assert.True(t, liveMD.Status.Roles[0].RuntimeControl.Paused)
			assert.Equal(t, int64(2), liveMD.Status.Roles[0].RuntimeControl.ObservedGeneration)

			// Unpause back to false: restores routing and clears status to NotConfigured.
			liveMD.Generation = 3
			liveMD.Spec.Roles[0].Pause = false
			require.NoError(t, cli.Update(context.Background(), liveMD))
			runReconcilePass(t, r)

			liveMD = getModelDeployment(t, cli)
			assert.Nil(t, liveMD.Status.Roles[0].RuntimeControl)
			cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
			require.NotNil(t, cond)
			assert.Equal(t, meta.ConditionTrue, cond.Status)
			assert.Equal(t, ModelDeploymentReasonRuntimeControlNotConfigured, cond.Reason)
		})
	}
}

// 9. Parameter key removal and full runtimeControl removal reset changed fields to boot defaults.
func TestModelDeploymentRuntimeControl_ParameterKeyRemovalAndFullReset(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-reset-1", 1)
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64", "--max-num-batched-tokens=8192"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
				runtimecontrol.ParamSchedulingMaxBatchedTokens:  "4096",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	runReconcilePass(t, r)
	runReconcilePass(t, r)
	pods := markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(pods, true)))
	runReconcilePass(t, r)
	assert.Equal(t, 1, mockEng.applyCalls)

	// Remove maxBatchedTokens while keeping maxActiveRequests: must send reset_fields=["scheduling.maxBatchedTokens"].
	liveMD := getModelDeployment(t, cli)
	liveMD.Generation = 2
	liveMD.Spec.Roles[0].RuntimeControl.Parameters = map[string]string{
		runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
	}
	require.NoError(t, cli.Update(context.Background(), liveMD))
	runReconcilePass(t, r)

	assert.Equal(t, 2, mockEng.applyCalls)
	assert.Equal(t, []string{runtimecontrol.ParamSchedulingMaxBatchedTokens}, mockEng.lastResetFields)

	// Remove runtimeControl completely: must reset maxActiveRequests, delete ledger ConfigMap, and report NotConfigured.
	liveMD = getModelDeployment(t, cli)
	liveMD.Generation = 3
	liveMD.Spec.Roles[0].RuntimeControl = nil
	require.NoError(t, cli.Update(context.Background(), liveMD))
	runReconcilePass(t, r)

	assert.Equal(t, 3, mockEng.applyCalls)
	assert.Equal(t, []string{runtimecontrol.ParamSchedulingMaxActiveRequests}, mockEng.lastResetFields)

	liveMD = getModelDeployment(t, cli)
	assert.Nil(t, liveMD.Status.Roles[0].RuntimeControl)
	cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionTrue, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlNotConfigured, cond.Reason)

	var cms core.ConfigMapList
	require.NoError(t, cli.List(context.Background(), &cms, ctrlcli.InNamespace("team-a")))
	assert.Empty(t, cms.Items, "ledger ConfigMap must be deleted once runtimeControl restoration completes")
}

// 10. Ambiguous POST timeout fences blind replay and recovers via live readback once settled.
func TestModelDeploymentRuntimeControl_AmbiguousPostFencingAndReadbackRecovery(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-ambig-1", 1)
	md := newRuntimeControlDeployment()
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli, runtimecontrol.WithTimeout(50*time.Millisecond))

	runReconcilePass(t, r)
	runReconcilePass(t, r)
	pods := markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(pods, true)))
	runReconcilePass(t, r)

	// Configure pause=true where POST /gpustack/v1/pause times out on client side without taking effect yet.
	mockEng.mu.Lock()
	mockEng.pauseBlockNext = true
	mockEng.pauseDelay = 150 * time.Millisecond
	mockEng.pauseSuppressStateApply = true
	mockEng.mu.Unlock()

	liveMD := getModelDeployment(t, cli)
	liveMD.Generation = 2
	liveMD.Spec.Roles[0].Pause = true
	require.NoError(t, cli.Update(context.Background(), liveMD))

	runReconcilePass(t, r) // Withdraws endpoint-eligible.
	runReconcilePass(t, r) // Drains and sends POST /gpustack/v1/pause -> times out ambiguously!

	_, _, pauseCount, _ := mockEng.counts()
	assert.Equal(t, 1, pauseCount)
	liveMD = getModelDeployment(t, cli)
	cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionFalse, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlAmbiguousOperation, cond.Reason)

	// Switch to normal timeout while server still reports paused=false: must stay fenced without re-POSTing!
	r.RuntimeControlClient = runtimecontrol.NewClient(runtimecontrol.WithTimeout(2 * time.Second))
	runReconcilePass(t, r)
	_, _, pauseCount, _ = mockEng.counts()
	assert.Equal(t, 1, pauseCount, "fenced ambiguous operation must never blindly replay POST /pause")
	cond = conditionOf(getModelDeployment(t, cli), string(ModelDeploymentConditionRuntimeControlSynced))
	assert.Equal(t, ModelDeploymentReasonRuntimeControlAmbiguousOperation, cond.Reason)

	// Server-side pause finishes taking effect: next pass observes paused=true via GET /pause, clears fence, and converges!
	mockEng.mu.Lock()
	mockEng.paused = true
	mockEng.mu.Unlock()
	runReconcilePass(t, r)

	_, _, pauseCount, _ = mockEng.counts()
	assert.Equal(t, 1, pauseCount, "recovery via readback must not re-send POST /pause")
	liveMD = getModelDeployment(t, cli)
	cond = conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionTrue, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlSynced, cond.Reason)
	assert.True(t, liveMD.Status.Roles[0].RuntimeControl.Paused)
}

// 11. 422 unsupported_fields reports UnsupportedEngineVersion without rolling Pods or dropping standing eligibility.
func TestModelDeploymentRuntimeControl_UnsupportedFields422AndCapabilityProbe(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-unsup-1", 1)
	md := newRuntimeControlDeployment()
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	runReconcilePass(t, r)
	runReconcilePass(t, r)
	pods := markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	origUID := pods[0].UID
	require.NoError(t, cli.Create(context.Background(), admittedReplicaWorkload(pods, true)))
	runReconcilePass(t, r)

	mockEng.mu.Lock()
	mockEng.rejectFields422 = []string{runtimecontrol.ParamSchedulingCacheAwareWindow}
	mockEng.mu.Unlock()

	liveMD := getModelDeployment(t, cli)
	liveMD.Generation = 2
	liveMD.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
		Parameters: map[string]string{
			runtimecontrol.ParamSchedulingCacheAwareWindow: "64",
		},
	}
	require.NoError(t, cli.Update(context.Background(), liveMD))
	runReconcilePass(t, r)

	liveMD = getModelDeployment(t, cli)
	cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionFalse, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlUnsupportedEngineVersion, cond.Reason)
	require.NotNil(t, liveMD.Status.Roles[0].RuntimeControl)
	assert.Equal(t, []string{runtimecontrol.ParamSchedulingCacheAwareWindow}, liveMD.Status.Roles[0].RuntimeControl.UnsupportedFields)

	livePod := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pods[0]), livePod))
	assert.Equal(t, origUID, livePod.UID, "422 unsupported_fields must never trigger Pod rollout")
	assert.Equal(t, modelDeploymentEndpointEligibleValue, livePod.Labels[modelDeploymentLabelKeyEndpointEligible],
		"standing Ready Pod must retain endpoint-eligible on 422 unsupported_fields")
}

// 12. Folded Elastic EP width + ExtraArgs max-dp-size, pause serialization during resize, and post-resize replay.
func TestModelDeploymentRuntimeControl_ElasticEPFoldedWidthPauseSerializationAndPostResizeReplay(t *testing.T) {
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-elastic-1", 2)
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.KVCache = nil
		md.Spec.Roles[0].ExtraArgs = []string{
			"--elastic-ep-max-dp-size=8",
			"--max-num-seqs=64",
		}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamElasticDataParallelWidth:    "2",
				runtimecontrol.ParamSchedulingMaxActiveRequests: "20",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	_, err := r.convergeModelDeployment(context.Background(), md)
	require.NoError(t, err)

	var podList core.PodList
	require.NoError(t, cli.List(context.Background(), &podList, ctrlcli.InNamespace("team-a")))
	var masterPod, workerPod *core.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if modelDeploymentPodRole(p) != "server" {
			continue
		}
		if modelDeploymentOrdinalOrFloor(p) == 0 {
			masterPod = p.DeepCopy()
		} else {
			workerPod = p.DeepCopy()
		}
	}
	require.NotNil(t, masterPod)
	require.NotNil(t, workerPod)
	assert.Contains(t, masterPod.Spec.Containers[0].Command, "--elastic-ep-max-dp-size=8",
		"master ordinal 0 must retain --elastic-ep-max-dp-size")
	assert.NotContains(t, workerPod.Spec.Containers[0].Command, "--elastic-ep-max-dp-size=8",
		"headless worker ordinal > 0 must strip --elastic-ep-max-dp-size")
	masterHashAtWidth2 := masterPod.Annotations[modelDeploymentPodSpecHashAnnotation]

	// Configure master Pod endpoint and verify initial apply across 2 ranks.
	masterPod.UID = "elastic-master-uid"
	masterPod.Annotations["prometheus.io/port"] = strconv.Itoa(int(port))
	require.NoError(t, cli.Update(context.Background(), masterPod))
	masterPod.Status.Phase = core.PodRunning
	masterPod.Status.PodIP = host
	masterPod.Status.Conditions = []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}}
	masterPod.Status.ContainerStatuses = []core.ContainerStatus{{
		Name: modelDeploymentMainContainerName, ContainerID: "ctr-master", Ready: true,
		State: core.ContainerState{Running: &core.ContainerStateRunning{}},
	}}
	require.NoError(t, cli.Status().Update(context.Background(), masterPod))

	ready, holdMsg, err := r.ensureModelDeploymentElasticMasterRuntimeControl(context.Background(), md, &md.Spec.Roles[0], masterPod)
	require.NoError(t, err)
	assert.True(t, ready, holdMsg)
	assert.Equal(t, 1, mockEng.applyCalls)
	assert.Equal(t, 1, mockEng.ackCalls)

	// Width edit 2 -> 3 keeps master pod-spec-hash stable; in-flight resize holds role.Pause transition.
	runtimecontrol.SetElasticWidth(&md.Spec.Roles[0], 3)
	md.Spec.Roles[0].Pause = true
	md.Generation = 2
	require.NoError(t, cli.Update(context.Background(), md))

	desired3, err := r.renderModelDeploymentPods(context.Background(), md, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, masterHashAtWidth2, desired3["server"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		"changing elastic.dataParallelWidth or pause must never alter master pod-spec-hash")

	store := newElasticOperationStore(cli)
	_, err = store.Create(context.Background(), &elasticOperation{
		Name: md.Name, Namespace: md.Namespace, DeploymentUID: md.UID, Generation: 2,
		Width: elasticWidth{Old: 2, Target: 3}, State: elasticStateCommandSent, CommandSent: true,
		Master:  elasticCapturedIdentity{Name: masterPod.Name, UID: masterPod.UID},
		Head:    elasticCapturedIdentity{Name: "qwen-elastic-head", UID: "elastic-head-uid"},
		Members: []elasticCapturedIdentity{{Name: masterPod.Name, UID: masterPod.UID}},
	})
	require.NoError(t, err)

	var obs modelDeploymentPassObservation
	require.NoError(t, r.reconcileModelDeploymentRuntimeControl(context.Background(), md, []core.Pod{*masterPod}, nil, true, "", &obs))
	assert.Equal(t, meta.ConditionUnknown, obs.RuntimeControl.ConditionStatus)
	assert.Zero(t, mockEng.pauseCalls, "role.Pause must wait while an Elastic EP resize operation is in flight")

	// Settle resize and expand ranks to 3: post-resize hook replays parameters across all 3 ranks!
	op, err := store.Read(context.Background(), md.Namespace, md.Name, md.UID)
	require.NoError(t, err)
	require.NoError(t, store.Delete(context.Background(), op))
	md.Spec.Roles[0].Pause = false
	require.NoError(t, cli.Update(context.Background(), md))

	mockEng.expandRanks(3)
	ready, holdMsg, err = r.ensureModelDeploymentElasticMasterRuntimeControl(context.Background(), md, &md.Spec.Roles[0], masterPod)
	require.NoError(t, err)
	assert.True(t, ready, holdMsg)
	assert.Equal(t, 2, mockEng.applyCalls, "newly joined rank 2 must receive fresh runtime_config replay")

	// Pause arriving during a routed resize must not deadlock post-resize parameter-only replay,
	// even when the elastic master is already paused.
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterVLLM}
	md.Spec.Roles[0].Pause = true
	md.Generation = 3
	require.NoError(t, cli.Update(context.Background(), md))
	mockEng.mu.Lock()
	mockEng.paused = true
	mockEng.mu.Unlock()
	ready, holdMsg, err = r.ensureModelDeploymentElasticMasterRuntimeControlForWidth(
		context.Background(), md, &md.Spec.Roles[0], masterPod, 3,
	)
	require.NoError(t, err)
	assert.True(t, ready, holdMsg)
	assert.Zero(t, mockEng.resumeCalls, "parameter-only replay on paused elastic master must not unpause")
}

// 13. Generation-only ACK timeout and crash-recovered PendingOperationID settle cleanly via exact generation.
func TestModelDeploymentRuntimeControl_PendingAndGenerationAckSettlement(t *testing.T) {
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "24",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-gen-ack", 1)

	r := newRuntimeControlReconciler(cli)
	runReconcilePass(t, r)
	markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	runReconcilePass(t, r)
	assert.Equal(t, 1, mockEng.ackCalls)

	// Bump generation without changing parameters -> generation-only ACK fast path.
	liveMD := getModelDeployment(t, cli)
	liveMD.Generation = 2
	require.NoError(t, cli.Update(context.Background(), liveMD))

	// Inject timeout on the generation-only ACK POST (simulating ambiguous network timeout after engine received ACK).
	r.RuntimeControlClient = &timeoutOnceAckClient{
		inner: defaultModelDeploymentRuntimeControlClient,
		fail:  true,
	}
	runReconcilePass(t, r)
	liveMD = getModelDeployment(t, cli)
	cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionFalse, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlAmbiguousOperation, cond.Reason)

	// If engine still reports observed_generation=1, fence MUST hold (not clear on stale generation).
	r.RuntimeControlClient = defaultModelDeploymentRuntimeControlClient
	runReconcilePass(t, r)
	liveMD = getModelDeployment(t, cli)
	cond = conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionFalse, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlAmbiguousOperation, cond.Reason)

	// Once engine live readback reports observed_generation=2, fence settles cleanly without duplicate POST.
	mockEng.mu.Lock()
	mockEng.observedGen = 2
	mockEng.mu.Unlock()
	runReconcilePass(t, r)
	liveMD = getModelDeployment(t, cli)
	cond = conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionTrue, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlSynced, cond.Reason)
	assert.Equal(t, 1, mockEng.ackCalls, "settled generation-only ACK must not issue a duplicate ACK POST")
}

// 16. Definitive 400/422 engine rejections clear PendingOperationID so corrected specs converge cleanly.
func TestModelDeploymentRuntimeControl_DefinitiveRejectionClearsPendingIntent(t *testing.T) {
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "24",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	mockEng, _, host, port := newMockSentinelEngine(t, workercore.ModelDeploymentEngineVLLM, "boot-reject-clear", 1)

	r := newRuntimeControlReconciler(cli)
	runReconcilePass(t, r)
	markDeploymentPodsRunningWithServer(t, cli, host, port, true, 0)
	runReconcilePass(t, r)

	// Patch an updated value and have the engine return a definitive 400 invalid_argument refusal.
	liveMD := getModelDeployment(t, cli)
	liveMD.Generation = 2
	liveMD.Spec.Roles[0].RuntimeControl.Parameters[runtimecontrol.ParamSchedulingMaxActiveRequests] = "48"
	require.NoError(t, cli.Update(context.Background(), liveMD))

	r.RuntimeControlClient = &rejectOnceApplyClient{
		inner: defaultModelDeploymentRuntimeControlClient,
		fail:  true,
	}
	runReconcilePass(t, r)
	liveMD = getModelDeployment(t, cli)
	cond := conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionFalse, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlReconcileFailed, cond.Reason)

	// Correct the parameter and restore normal client: Pod must converge to Synced, NOT AmbiguousOperation.
	liveMD.Generation = 3
	liveMD.Spec.Roles[0].RuntimeControl.Parameters[runtimecontrol.ParamSchedulingMaxActiveRequests] = "32"
	require.NoError(t, cli.Update(context.Background(), liveMD))
	r.RuntimeControlClient = defaultModelDeploymentRuntimeControlClient
	runReconcilePass(t, r)

	liveMD = getModelDeployment(t, cli)
	cond = conditionOf(liveMD, string(ModelDeploymentConditionRuntimeControlSynced))
	require.NotNil(t, cond)
	assert.Equal(t, meta.ConditionTrue, cond.Status)
	assert.Equal(t, ModelDeploymentReasonRuntimeControlSynced, cond.Reason)
	assert.Equal(t, 2, mockEng.applyCalls)
}

type rejectOnceApplyClient struct {
	inner ModelDeploymentRuntimeControlClient
	fail  bool
}

func (c *rejectOnceApplyClient) GetLive(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.LiveResponse, error) {
	return c.inner.GetLive(ctx, target)
}

func (c *rejectOnceApplyClient) GetReady(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.ReadyResponse, error) {
	return c.inner.GetReady(ctx, target)
}

func (c *rejectOnceApplyClient) GetRuntimeConfig(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.RuntimeConfigState, error) {
	return c.inner.GetRuntimeConfig(ctx, target)
}

func (c *rejectOnceApplyClient) ApplyRuntimeConfig(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.ApplyRuntimeConfigRequest) (*runtimecontrol.RuntimeConfigState, error) {
	if c.fail {
		c.fail = false
		return nil, fmt.Errorf("%w: exceeds runtime boot ceiling", runtimecontrol.ErrInvalidArgument)
	}
	return c.inner.ApplyRuntimeConfig(ctx, target, req)
}

func (c *rejectOnceApplyClient) GetPause(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.PauseResponse, error) {
	return c.inner.GetPause(ctx, target)
}

func (c *rejectOnceApplyClient) SetPause(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.PauseRequest) (*runtimecontrol.PauseResponse, error) {
	return c.inner.SetPause(ctx, target, req)
}

func (c *rejectOnceApplyClient) Ack(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.AckRequest) (*runtimecontrol.ReadyResponse, error) {
	return c.inner.Ack(ctx, target, req)
}

type timeoutOnceAckClient struct {
	inner ModelDeploymentRuntimeControlClient
	fail  bool
}

func (c *timeoutOnceAckClient) GetLive(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.LiveResponse, error) {
	return c.inner.GetLive(ctx, target)
}

func (c *timeoutOnceAckClient) GetReady(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.ReadyResponse, error) {
	return c.inner.GetReady(ctx, target)
}

func (c *timeoutOnceAckClient) GetRuntimeConfig(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.RuntimeConfigState, error) {
	return c.inner.GetRuntimeConfig(ctx, target)
}

func (c *timeoutOnceAckClient) ApplyRuntimeConfig(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.ApplyRuntimeConfigRequest) (*runtimecontrol.RuntimeConfigState, error) {
	return c.inner.ApplyRuntimeConfig(ctx, target, req)
}

func (c *timeoutOnceAckClient) GetPause(ctx context.Context, target runtimecontrol.Target) (*runtimecontrol.PauseResponse, error) {
	return c.inner.GetPause(ctx, target)
}

func (c *timeoutOnceAckClient) SetPause(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.PauseRequest) (*runtimecontrol.PauseResponse, error) {
	return c.inner.SetPause(ctx, target, req)
}

func (c *timeoutOnceAckClient) Ack(ctx context.Context, target runtimecontrol.Target, req runtimecontrol.AckRequest) (*runtimecontrol.ReadyResponse, error) {
	if c.fail {
		c.fail = false
		return nil, &runtimecontrol.AmbiguousOperationError{
			Operation:   "POST " + runtimecontrol.PathAck,
			OperationID: "ack-timeout",
			Reason:      "timeout",
			Err:         context.DeadlineExceeded,
		}
	}
	return c.inner.Ack(ctx, target, req)
}

// 14. Already-isolated router single-replica restart must not drop routerIsolationReady or healthy backends.
func TestModelDeploymentRuntimeControl_RouterSingleReplicaBlipKeepsIsolation(t *testing.T) {
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:     workercore.ModelDeploymentRouterVLLM,
			Replicas: ptr.To[int32](2),
		}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "16",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)
	runReconcilePass(t, r)

	routerDep := new(app.Deployment)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "qwen-router"}, routerDep))
	// All 2 replicas are on the allowlist revision, but 1 of 2 is temporarily unready.
	routerDep.Status.ObservedGeneration = routerDep.Generation
	routerDep.Status.Replicas = 2
	routerDep.Status.UpdatedReplicas = 2
	routerDep.Status.ReadyReplicas = 1
	require.NoError(t, cli.Status().Update(context.Background(), routerDep))

	ready, msg, err := r.resolveModelDeploymentRouterIsolation(context.Background(), md)
	require.NoError(t, err)
	assert.True(t, ready, "1-of-2 replica blip on allowlist revision must keep isolation ready: %s", msg)

	// A terminating legacy Pod without the Envoy sidecar must block isolation even when
	// Deployment.Status.UpdatedReplicas == Replicas.
	now := meta.Now()
	legacyPod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace:         "team-a",
			Name:              "qwen-router-legacy-terminating",
			Labels:            maps.Clone(routerDep.Spec.Selector.MatchLabels),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test/hold"},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{Name: "router", Image: "gpustack/llm-router:v0.5.0"}},
		},
		Status: core.PodStatus{Phase: core.PodRunning},
	}
	require.NoError(t, cli.Create(context.Background(), legacyPod))
	ready, msg, err = r.resolveModelDeploymentRouterIsolation(context.Background(), md)
	require.NoError(t, err)
	assert.False(t, ready, "terminating legacy router Pod must block router isolation")
	assert.Contains(t, msg, "waiting for router deployment")
}

// 15. Creating a paused Elastic EP deployment with width=3 creates all 3 bootstrap ordinals.
func TestModelDeploymentRuntimeControl_PausedElasticBootstrapCreatesAllRanks(t *testing.T) {
	md := newRuntimeControlDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Pause = true
		md.Spec.Roles[0].ExtraArgs = []string{"--elastic-ep-max-dp-size=4", "--max-num-seqs=64"}
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamElasticDataParallelWidth: "3",
			},
		}
	})
	cli := newModelDeploymentClient(md, newRenderAcceleratorType())
	r := newRuntimeControlReconciler(cli)

	desired, err := r.renderModelDeploymentPods(context.Background(), md, nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, desired["server"], 3)

	_, _, err = r.convergeModelDeploymentElasticWithIsolation(
		context.Background(), md, nil, desired, nil, true, "",
	)
	require.NoError(t, err)

	var pods core.PodList
	require.NoError(t, cli.List(context.Background(), &pods, ctrlcli.InNamespace("team-a")))
	gpuPods := 0
	for i := range pods.Items {
		if modelDeploymentPodRole(&pods.Items[i]) == "server" {
			gpuPods++
		}
	}
	assert.Equal(t, 3, gpuPods, "paused Elastic EP bootstrap at width=3 must create all 3 GPU ordinals")
}
