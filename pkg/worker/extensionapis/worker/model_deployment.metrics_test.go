package worker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

const metricsVLLMFixture = `# TYPE vllm:num_requests_running gauge
vllm:num_requests_running 2
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting 0
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total %d
# TYPE vllm:prefix_cache_queries_total counter
vllm:prefix_cache_queries_total %d
`

const metricsSGLangFixture = `# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs 3
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs 1
# TYPE sglang:prefill_effective_tokens_total counter
sglang:prefill_effective_tokens_total{mode="input"} 20
sglang:prefill_effective_tokens_total{mode="device_hit"} 5
sglang:prefill_effective_tokens_total{mode="host_hit"} 3
sglang:prefill_effective_tokens_total{mode="storage_hit"} 2
# TYPE sglang:num_decode_transfer_queue_reqs gauge
sglang:num_decode_transfer_queue_reqs 2
# TYPE sglang:kv_transfer_latency_ms histogram
sglang:kv_transfer_latency_ms_sum 25
sglang:kv_transfer_latency_ms_count 5
# TYPE sglang:num_transfer_failed_reqs_total counter
sglang:num_transfer_failed_reqs_total 1
`

func metricsVLLMResponse(hits, queries, observations int) string {
	return fmt.Sprintf(metricsVLLMFixture, hits, queries) + fmt.Sprintf(`# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_sum %d
vllm:time_to_first_token_seconds_count %d
# TYPE vllm:request_time_per_output_token_seconds histogram
vllm:request_time_per_output_token_seconds_sum %d
vllm:request_time_per_output_token_seconds_count %d
# TYPE vllm:inter_token_latency_seconds histogram
vllm:inter_token_latency_seconds_sum %d
vllm:inter_token_latency_seconds_count %d
`, observations, observations, observations*2, observations, observations*3, observations)
}

func TestParseModelDeploymentMetrics(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		engine     string
		router     string
		gauge      string
		wantGauge  float64
		cacheScope string
		wantPair   modelDeploymentCounterPair
		wantErr    bool
	}{
		{name: "vLLM zero queue and local cache", body: fmt.Sprintf(metricsVLLMFixture, 5, 10), engine: "vllm", gauge: "waiting", cacheScope: "local-prefix", wantPair: modelDeploymentCounterPair{5, 10}},
		{name: "SGLang effective token denominator", body: metricsSGLangFixture, engine: "sglang", gauge: "running", wantGauge: 3, cacheScope: "host-prefix", wantPair: modelDeploymentCounterPair{3, 30}},
		{name: "router reports workers without proving readiness", body: "# TYPE vllm_router_active_workers gauge\nvllm_router_active_workers 1\n", router: "vllm-router", gauge: "router-reported-workers", wantGauge: 1},
		{name: "llm-d availability", body: "# TYPE llm_d_epp_ready_endpoints gauge\nllm_d_epp_ready_endpoints{name=\"chat\"} 2\n", router: "llm-d-router", gauge: "router-backends", wantGauge: 2},
		{name: "SGLang gateway availability", body: "# TYPE smg_worker_pool_size gauge\nsmg_worker_pool_size{worker_type=\"prefill\"} 2\nsmg_worker_pool_size{worker_type=\"decode\"} 3\n", router: "sglang-gateway", gauge: "router-backends", wantGauge: 5},
		{name: "malformed exposition", body: "broken{", engine: "vllm", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseModelDeploymentMetrics([]byte(tc.body), tc.engine, tc.router)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantGauge, got.gauges[tc.gauge])
			if tc.cacheScope != "" {
				assert.Equal(t, tc.wantPair, got.counters[tc.cacheScope])
			}
		})
	}
}

func TestModelDeploymentMetricsHandler_VLLMRouterWorkerCountDoesNotClaimReadiness(t *testing.T) {
	md := metricsModelDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "vllm-router"}
	read := &modelDeploymentPodScrape{
		pod:    core.Pod{ObjectMeta: meta.ObjectMeta{Name: "router"}},
		router: true,
		value:  modelDeploymentScrape{gauges: map[string]float64{"router-reported-workers": 1}},
		at:     time.Now(),
	}
	result := &worker.ModelDeploymentMetrics{}
	(&ModelDeploymentMetricsHandler{}).mergeMetrics(result, md, read)
	require.Len(t, result.Processing, 1)
	assert.Equal(t, "router-reported-workers", result.Processing[0].Name)
	assert.Equal(t, "workers", result.Processing[0].Unit)
	assert.Equal(t, float64(1), result.Processing[0].Value)
}

func TestParseModelDeploymentMetrics_Windows(t *testing.T) {
	tests := []struct {
		name, body, engine, router, key string
		want                            modelDeploymentWindowPair
	}{
		{"vLLM TTFT", metricsVLLMResponse(5, 10, 4), "vllm", "", "ttft", modelDeploymentWindowPair{4, 4}},
		{"SGLang P/D transfer", metricsSGLangFixture, "sglang", "", "transfer-latency", modelDeploymentWindowPair{25, 5}},
		{"SGLang transfer failure", metricsSGLangFixture, "sglang", "", "transfer-failures", modelDeploymentWindowPair{count: 1}},
		{"vLLM router successful request rate", "# TYPE vllm_router_requests_total counter\nvllm_router_requests_total{route=\"/v1/chat/completions\"} 4\n", "", "vllm-router", "successful-requests", modelDeploymentWindowPair{count: 4}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseModelDeploymentMetrics([]byte(tc.body), tc.engine, tc.router)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.windows[tc.key])
		})
	}
}

func TestParseModelDeploymentMetrics_SGLangGatewayUnavailableWorker(t *testing.T) {
	body, err := os.ReadFile("testdata/model_deployment_metrics/sglang_gateway_no_workers.prom")
	require.NoError(t, err)
	got, err := parseModelDeploymentMetrics(body, "", "sglang-gateway")
	require.NoError(t, err)
	assert.Equal(t, modelDeploymentWindowPair{count: 1}, got.windows["requests"])
	assert.Equal(t, modelDeploymentWindowPair{count: 1}, got.windows["http-5xx-responses"])
	assert.NotContains(t, got.windows, "errors")
	noFailure, err := parseModelDeploymentMetrics([]byte("# TYPE smg_http_responses_total counter\nsmg_http_responses_total{status_code=\"200\",error_code=\"\"} 1\n"), "", "sglang-gateway")
	require.NoError(t, err)
	assert.Contains(t, noFailure.windows, "http-5xx-responses")
	assert.Equal(t, modelDeploymentWindowPair{}, noFailure.windows["http-5xx-responses"])
	noSamples, err := parseModelDeploymentMetrics([]byte("# TYPE smg_http_responses_total counter\n"), "", "sglang-gateway")
	require.NoError(t, err)
	assert.NotContains(t, noSamples.windows, "http-5xx-responses")
}

func TestParseModelDeploymentMetrics_VLLMRouterBackendFailure(t *testing.T) {
	body, err := os.ReadFile("testdata/model_deployment_metrics/vllm_router_backend_failure.prom")
	require.NoError(t, err)
	got, err := parseModelDeploymentMetrics(body, "", "vllm-router")
	require.NoError(t, err)
	assert.Equal(t, modelDeploymentWindowPair{count: 1}, got.windows["successful-requests"])
	assert.Equal(t, modelDeploymentWindowPair{count: 1}, got.windows["retries-exhausted"])
	assert.NotContains(t, got.windows, "errors")
}

func TestModelDeploymentMetricsHandler_VLLMRouterDoesNotDivideSuccessesByErrors(t *testing.T) {
	md := metricsModelDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "vllm-router"}
	h := &ModelDeploymentMetricsHandler{}
	read := &modelDeploymentPodScrape{
		pod:    core.Pod{ObjectMeta: meta.ObjectMeta{Name: "router", UID: "router-uid"}},
		router: true,
		value: modelDeploymentScrape{windows: map[string]modelDeploymentWindowPair{
			"successful-requests": {count: 1},
			"errors":              {count: 1},
			"retries-exhausted":   {count: 1},
		}},
		at: time.Now(),
	}
	h.mergeWindowMetrics(&worker.ModelDeploymentMetrics{}, md, read)
	for key := range read.value.windows {
		read.value.windows[key] = modelDeploymentWindowPair{count: 2}
	}
	read.at = read.at.Add(time.Second)
	result := &worker.ModelDeploymentMetrics{}
	h.mergeWindowMetrics(result, md, read)
	require.Len(t, result.Traffic, 3)
	for _, metric := range result.Traffic {
		assert.NotEqual(t, "error-ratio", metric.Name)
		assert.Equal(t, float64(1), metric.Value)
	}
}

func TestModelDeploymentMetricsHandler_SGLangGatewayHTTP5xxHasItsOwnRate(t *testing.T) {
	md := metricsModelDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "sglang-gateway"}
	h := &ModelDeploymentMetricsHandler{}
	read := &modelDeploymentPodScrape{
		pod:    core.Pod{ObjectMeta: meta.ObjectMeta{Name: "gateway", UID: "gateway-uid"}},
		router: true,
		value: modelDeploymentScrape{windows: map[string]modelDeploymentWindowPair{
			"requests":           {count: 1},
			"http-5xx-responses": {count: 1},
		}},
		at: time.Now(),
	}
	h.mergeWindowMetrics(&worker.ModelDeploymentMetrics{}, md, read)
	read.value.windows["requests"] = modelDeploymentWindowPair{count: 2}
	read.value.windows["http-5xx-responses"] = modelDeploymentWindowPair{count: 2}
	read.at = read.at.Add(time.Second)
	result := &worker.ModelDeploymentMetrics{}
	h.mergeWindowMetrics(result, md, read)
	var responseRate *worker.ModelDeploymentMetricWindow
	for i := range result.Traffic {
		if result.Traffic[i].Name == "http-5xx-responses" {
			responseRate = &result.Traffic[i]
		}
		assert.NotEqual(t, "error-ratio", result.Traffic[i].Name)
	}
	require.NotNil(t, responseRate)
	assert.Equal(t, "smg_http_responses_total", responseRate.Source)
	assert.Equal(t, "responses/second", responseRate.Unit)
	assert.Equal(t, float64(1), responseRate.Value)
}

func TestModelDeploymentMetricsHandler_SGLangHTTPGatewayDoesNotRequireGRPCLatency(t *testing.T) {
	md := metricsModelDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "sglang-gateway"}
	read := &modelDeploymentPodScrape{
		pod:    core.Pod{ObjectMeta: meta.ObjectMeta{Name: "gateway", UID: "gateway-uid"}},
		router: true,
		value:  modelDeploymentScrape{windows: map[string]modelDeploymentWindowPair{}},
		at:     time.Now(),
	}
	result := &worker.ModelDeploymentMetrics{}
	(&ModelDeploymentMetricsHandler{}).mergeWindowMetrics(result, md, read)
	require.NotEmpty(t, result.Missing)
	assert.Equal(t, "smg_router_requests_total", result.Missing[0].Source)
	for _, missing := range result.Missing {
		assert.NotEqual(t, "smg_router_ttft_seconds", missing.Source)
		assert.NotEqual(t, "smg_router_tpot_seconds", missing.Source)
	}
}

func metricsModelDeployment() *workercore.ModelDeployment {
	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "chat", Namespace: "team", UID: types.UID("md-one")},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: "vllm"},
			Roles:  []workercore.ModelDeploymentRole{{Name: "server"}},
		},
	}
}

func metricsEnginePod(md *workercore.ModelDeployment, name, host, port string) *core.Pod {
	n, _ := strconv.Atoi(port)
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: name, Namespace: md.Namespace, UID: types.UID(name + "-uid"),
			Labels: map[string]string{
				"app.kubernetes.io/name":      "model-deployment",
				"app.kubernetes.io/instance":  md.Name,
				"app.kubernetes.io/component": "server",
			},
			Annotations: map[string]string{
				"prometheus.io/scrape": "true", "prometheus.io/path": "/metrics", "prometheus.io/port": port,
			},
			OwnerReferences: []meta.OwnerReference{{APIVersion: "worker.gpustack.ai/v1alpha1", Kind: "ModelDeployment", Name: md.Name, UID: md.UID, Controller: ptr.To(true)}},
		},
		Spec:   core.PodSpec{Containers: []core.Container{{Name: "engine", Ports: []core.ContainerPort{{Name: "http", ContainerPort: int32(n)}}}}},
		Status: core.PodStatus{PodIP: host},
	}
}

func TestModelDeploymentMetricsHandler_EngineSnapshot(t *testing.T) {
	var stage atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if stage.Load() == 0 {
			fmt.Fprint(w, metricsVLLMResponse(5, 10, 2))
		} else {
			fmt.Fprint(w, metricsVLLMResponse(10, 20, 4))
		}
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	first := metricsEnginePod(md, "chat-server-1", host, port)
	second := metricsEnginePod(md, "chat-server-2", host, port)
	unrelated := metricsEnginePod(md, "other-owner", host, port)
	unrelated.OwnerReferences[0].UID = "another-deployment"
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, first, second, unrelated).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: server.Client()}
	key := types.NamespacedName{Namespace: md.Namespace, Name: md.Name}

	obj, err := h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	initial := obj.(*worker.ModelDeploymentMetrics)
	assert.True(t, initial.Partial, "a first counter reading has no sampling window")
	require.Len(t, initial.Processing, 1)
	assert.Equal(t, float64(4), initial.Processing[0].Value)
	assert.Equal(t, int32(2), initial.Processing[0].PodCount)
	require.Len(t, initial.Queueing, 1)
	assert.Equal(t, float64(0), initial.Queueing[0].Value, "measured zero is retained")
	assert.Empty(t, initial.CacheHits)
	require.Len(t, initial.Missing, 8)

	stage.Store(1)
	obj, err = h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	current := obj.(*worker.ModelDeploymentMetrics)
	assert.False(t, current.Partial)
	require.Len(t, current.Latency, 6)
	for _, latency := range current.Latency {
		assert.Equal(t, float64(2), latency.Samples)
		assert.Equal(t, "engine/server", latency.Scope)
	}
	require.Len(t, current.CacheHits, 2)
	for _, hit := range current.CacheHits {
		assert.Equal(t, float64(5), hit.Hits)
		assert.Equal(t, float64(10), hit.Queries)
		assert.Equal(t, 0.5, hit.Rate)
		assert.Greater(t, hit.WindowSeconds, float64(0))
		assert.NotEqual(t, "other-owner", hit.Pod)
	}
}

func TestModelDeploymentMetricsHandler_UnreadableAndMissingDeployment(t *testing.T) {
	md := metricsModelDeployment()
	pod := metricsEnginePod(md, "chat-server-1", "127.0.0.1", "8000")
	delete(pod.Annotations, "prometheus.io/path")
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, pod).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: &http.Client{}}
	_, err := h.OnGet(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, ctrlcli.GetOptions{})
	require.Error(t, err)
	assert.True(t, kerrors.IsServiceUnavailable(err))
	assert.Contains(t, err.Error(), "unreadable")
	_, err = h.OnGet(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: "absent"}, ctrlcli.GetOptions{})
	assert.True(t, kerrors.IsNotFound(err))
}

func TestModelDeploymentMetricsHandler_PartialScrape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, metricsVLLMFixture, 0, 0)
	}))
	defer server.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	good := metricsEnginePod(md, "good", host, port)
	failURL, err := url.Parse(failing.URL)
	require.NoError(t, err)
	failHost, failPort, err := net.SplitHostPort(failURL.Host)
	require.NoError(t, err)
	bad := metricsEnginePod(md, "bad", failHost, failPort)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, good, bad).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: server.Client()}
	obj, err := h.OnGet(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)
	assert.True(t, result.Partial)
	require.Len(t, result.Processing, 1)
	assert.Equal(t, int32(1), result.Processing[0].PodCount)
	assert.Equal(t, float64(2), result.Processing[0].Value)
	assert.NotEmpty(t, result.Missing)
	assert.Equal(t, "bad", result.Missing[0].Pod)
}

func TestModelDeploymentMetricsHandler_RouterOwnershipAndScope(t *testing.T) {
	var stage atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# TYPE llm_d_epp_request_running gauge\nllm_d_epp_request_running 2\n"+
			"# TYPE llm_d_epp_ready_endpoints gauge\nllm_d_epp_ready_endpoints{name=\"chat\"} 3\n")
		fmt.Fprintf(w, `# TYPE llm_d_epp_request_ttft_seconds histogram
llm_d_epp_request_ttft_seconds_sum %d
llm_d_epp_request_ttft_seconds_count %d
# TYPE llm_d_epp_request_streaming_tpot_seconds histogram
llm_d_epp_request_streaming_tpot_seconds_sum %d
llm_d_epp_request_streaming_tpot_seconds_count %d
# TYPE llm_d_epp_request_total counter
llm_d_epp_request_total %d
# TYPE llm_d_epp_request_error_total counter
llm_d_epp_request_error_total %d
`, stage.Load()*6, stage.Load()*3, stage.Load()*3, stage.Load()*3, stage.Load()*3, stage.Load())
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}
	deployment := &app.Deployment{ObjectMeta: meta.ObjectMeta{
		Name: "chat-router", Namespace: md.Namespace, UID: "router-deployment",
		OwnerReferences: []meta.OwnerReference{{APIVersion: "worker.gpustack.ai/v1alpha1", Kind: "ModelDeployment", Name: md.Name, UID: md.UID, Controller: ptr.To(true)}},
	}}
	rs := &app.ReplicaSet{ObjectMeta: meta.ObjectMeta{
		Name: "chat-router-rs", Namespace: md.Namespace, UID: "router-rs",
		OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: ptr.To(true)}},
	}}
	pod := metricsEnginePod(md, "chat-router-pod", host, port)
	delete(pod.Labels, "app.kubernetes.io/component")
	pod.Labels["modeldeployment.gpustack.ai/router"] = "llm-d-router"
	pod.OwnerReferences = []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr.To(true)}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, deployment, rs, pod).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: server.Client()}
	obj, err := h.OnGet(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)
	assert.True(t, result.Partial, "the engine role has no Pod")
	require.Len(t, result.Processing, 2)
	for _, gauge := range result.Processing {
		assert.Equal(t, "router", gauge.Scope)
	}
	assert.Contains(t, result.Missing, worker.ModelDeploymentMetricMissing{Source: "engine/server", Reason: "no owned metrics Pod"})
	stage.Store(1)
	obj, err = h.OnGet(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result = obj.(*worker.ModelDeploymentMetrics)
	require.Len(t, result.Latency, 2)
	// The llm-d error counter also counts requests its request counter never saw, so the two
	// rates are reported apart and no fraction is derived from them.
	require.Len(t, result.Traffic, 2)
	rates := map[string]worker.ModelDeploymentMetricWindow{}
	for _, metric := range result.Traffic {
		rates[metric.Name] = metric
	}
	assert.Equal(t, "llm_d_epp_request_total", rates["requests"].Source)
	assert.Equal(t, float64(3), rates["requests"].Samples)
	assert.Equal(t, "llm_d_epp_request_error_total", rates["request-errors"].Source)
	assert.Equal(t, float64(1), rates["request-errors"].Samples)
	assert.NotContains(t, rates, "error-ratio")
}

func TestModelDeploymentMetricsHandler_RejectsUndeclaredPort(t *testing.T) {
	md := metricsModelDeployment()
	pod := metricsEnginePod(md, "chat-server-1", "127.0.0.1", "8000")
	pod.Annotations["prometheus.io/port"] = "1234"
	h := &ModelDeploymentMetricsHandler{HTTPClient: &http.Client{}}
	read := h.scrapeMetricsPod(context.Background(), md, *pod)
	require.Error(t, read.err)
	assert.True(t, strings.Contains(read.err.Error(), "port"))
}

func TestModelDeploymentMetricsHandler_UsesAdvertisedHTTPSWithoutSkippingVerification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, metricsVLLMResponse(1, 2, 1))
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	pod := metricsEnginePod(md, "chat-server-1", host, port)
	pod.Annotations["prometheus.io/scheme"] = "https"

	untrusted := (&ModelDeploymentMetricsHandler{HTTPClient: &http.Client{}}).scrapeMetricsPod(context.Background(), md, *pod)
	require.Error(t, untrusted.err)
	trusted := (&ModelDeploymentMetricsHandler{HTTPClient: server.Client()}).scrapeMetricsPod(context.Background(), md, *pod)
	require.NoError(t, trusted.err)
	assert.Equal(t, float64(2), trusted.value.gauges["running"])
}

func TestModelDeploymentMetricsHandler_RejectsOversizedScrape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", modelDeploymentMetricsMaxBytes+1)))
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	pod := metricsEnginePod(md, "chat-server-1", host, port)
	h := &ModelDeploymentMetricsHandler{HTTPClient: server.Client()}

	read := h.scrapeMetricsPod(context.Background(), md, *pod)
	require.Error(t, read.err)
	assert.ErrorContains(t, read.err, "exceeds")
}

func TestModelDeploymentMetricsHandler_RespectsScrapeDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	pod := metricsEnginePod(md, "chat-server-1", host, port)
	h := &ModelDeploymentMetricsHandler{HTTPClient: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	read := h.scrapeMetricsPod(ctx, md, *pod)
	require.Error(t, read.err)
	assert.ErrorIs(t, read.err, context.DeadlineExceeded)
}

func TestModelDeploymentMetricsHandler_BoundsConcurrentScrapes(t *testing.T) {
	var active, peak, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		requests.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		fmt.Fprint(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 1\n")
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	pods := modelDeploymentMetricsConcurrency + 4
	objects := make([]ctrlcli.Object, 0, pods+1)
	objects = append(objects, md)
	for i := range pods {
		objects = append(objects, metricsEnginePod(md, fmt.Sprintf("chat-server-%d", i), host, port))
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: server.Client()}

	obj, err := h.OnGet(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)
	require.Len(t, result.Processing, 1)
	assert.Equal(t, int32(pods), result.Processing[0].PodCount)
	assert.Equal(t, int32(pods), requests.Load())
	assert.LessOrEqual(t, peak.Load(), int32(modelDeploymentMetricsConcurrency))
}

// TestModelDeploymentMetricsHandler_ScrapeBudgetFitsTheRequest pins the arithmetic the
// concurrency is sized by: every Pod up to the limit, each fetch running to its own limit, fits
// in half the request budget, so Pods queued behind hanging ones are still asked.
func TestModelDeploymentMetricsHandler_ScrapeBudgetFitsTheRequest(t *testing.T) {
	waves := (modelDeploymentMetricsMaxPods + modelDeploymentMetricsConcurrency - 1) /
		modelDeploymentMetricsConcurrency
	assert.LessOrEqual(t, time.Duration(waves)*modelDeploymentScrapeTimeout, modelDeploymentMetricsTimeout/2)
}

func TestModelDeploymentMetricsHandler_DoesNotDivideDifferentCounterWindows(t *testing.T) {
	md := metricsModelDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "sglang-gateway"}
	h := &ModelDeploymentMetricsHandler{}
	pod := core.Pod{ObjectMeta: meta.ObjectMeta{Name: "chat-router", UID: "router-uid"}}
	base := time.Now()
	tests := []struct {
		seconds int
		body    string
		missing bool
	}{
		{0, "# TYPE smg_router_requests_total counter\nsmg_router_requests_total 1\n# TYPE smg_router_request_errors_total counter\nsmg_router_request_errors_total 0\n", false},
		{1, "# TYPE smg_router_requests_total counter\nsmg_router_requests_total 3\n", false},
		{2, "# TYPE smg_router_requests_total counter\nsmg_router_requests_total 5\n# TYPE smg_router_request_errors_total counter\nsmg_router_request_errors_total 1\n", true},
	}
	for _, tc := range tests {
		parsed, err := parseModelDeploymentMetrics([]byte(tc.body), "", "sglang-gateway")
		require.NoError(t, err)
		result := &worker.ModelDeploymentMetrics{}
		read := &modelDeploymentPodScrape{pod: pod, router: true, value: parsed, at: base.Add(time.Duration(tc.seconds) * time.Second)}
		h.mergeWindowMetrics(result, md, read)
		if tc.missing {
			for _, metric := range result.Traffic {
				assert.NotEqual(t, "error-ratio", metric.Name)
			}
			assert.Contains(t, result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: pod.Name, Source: "smg_router_requests_total+smg_router_request_errors_total",
				Reason: "request and error counters have different sampling windows",
			})
		}
	}
}

func TestModelDeploymentMetricsHandler_ReportsMissingRoleReplica(t *testing.T) {
	var stage atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if stage.Load() == 0 {
			fmt.Fprint(w, metricsVLLMResponse(5, 10, 2))
		} else {
			fmt.Fprint(w, metricsVLLMResponse(10, 20, 4))
		}
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	md.Spec.Roles[0].Replicas = 2
	pod := metricsEnginePod(md, "chat-server-1", host, port)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, pod).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: server.Client()}
	key := types.NamespacedName{Namespace: md.Namespace, Name: md.Name}
	_, err = h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	stage.Store(1)
	obj, err := h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)
	assert.True(t, result.Partial)
	assert.Contains(t, result.Missing, worker.ModelDeploymentMetricMissing{
		Source: "engine/server", Reason: "1 of 2 owned metrics Pods present",
	})
}

func TestModelDeploymentMetricsHandler_PDTransferUsesKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, metricsSGLangFixture)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	md := metricsModelDeployment()
	md.Spec.Engine.Name = "sglang"
	md.Spec.Roles = []workercore.ModelDeploymentRole{
		{Name: "prompt", Kind: workercore.ModelDeploymentRoleKindPrefill},
		{Name: "generation", Kind: workercore.ModelDeploymentRoleKindDecode},
	}
	prefill := metricsEnginePod(md, "prompt-pod", host, port)
	prefill.Labels["app.kubernetes.io/component"] = "prompt"
	decode := metricsEnginePod(md, "generation-pod", host, port)
	decode.Labels["app.kubernetes.io/component"] = "generation"
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, prefill, decode).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: server.Client()}
	obj, err := h.OnGet(context.Background(), types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)
	var transferQueues int
	for _, gauge := range result.Queueing {
		if gauge.Name == "decode-transfer-waiting" {
			transferQueues++
			assert.Equal(t, "engine/generation", gauge.Scope)
			assert.Equal(t, float64(2), gauge.Value)
		}
	}
	assert.Equal(t, 1, transferQueues)
	var transferReads int
	for _, missing := range result.Missing {
		if missing.Source == "sglang:kv_transfer_latency_ms" {
			transferReads++
			assert.Equal(t, "prompt-pod", missing.Pod)
		}
	}
	assert.Equal(t, 1, transferReads)
}

func TestModelDeploymentMetricsHandler_PDPrefillDoesNotExpectInterTokenLatency(t *testing.T) {
	var stage atomic.Int32
	serve := func(prefill bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			observations := 2 + 2*int(stage.Load())
			body := metricsVLLMResponse(5*observations/2, 10*observations/2, observations)
			itl := fmt.Sprintf("vllm:inter_token_latency_seconds_sum %d\nvllm:inter_token_latency_seconds_count %d", observations*3, observations)
			switch {
			case prefill:
				// A prefill half answers with its first token only, so its inter-token
				// histogram never moves while its other series do.
				body = strings.Replace(body, itl, "vllm:inter_token_latency_seconds_sum 0\nvllm:inter_token_latency_seconds_count 0", 1)
			case stage.Load() == 2:
				body = strings.Replace(body, itl, "vllm:inter_token_latency_seconds_sum 12\nvllm:inter_token_latency_seconds_count 4", 1)
			}
			fmt.Fprint(w, body)
		}))
	}
	prefillServer, decodeServer := serve(true), serve(false)
	defer prefillServer.Close()
	defer decodeServer.Close()
	md := metricsModelDeployment()
	md.Spec.Roles = []workercore.ModelDeploymentRole{
		{Name: "prompt", Kind: workercore.ModelDeploymentRoleKindPrefill},
		{Name: "generation", Kind: workercore.ModelDeploymentRoleKindDecode},
	}
	pod := func(server *httptest.Server, name, role string) *core.Pod {
		u, err := url.Parse(server.URL)
		require.NoError(t, err)
		host, port, err := net.SplitHostPort(u.Host)
		require.NoError(t, err)
		p := metricsEnginePod(md, name, host, port)
		p.Labels["app.kubernetes.io/component"] = role
		return p
	}
	prefill := pod(prefillServer, "prompt-pod", "prompt")
	decode := pod(decodeServer, "generation-pod", "generation")
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, prefill, decode).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: http.DefaultClient}
	key := types.NamespacedName{Namespace: md.Namespace, Name: md.Name}
	_, err := h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	stage.Store(1)
	obj, err := h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)
	assert.Empty(t, result.Missing)
	assert.False(t, result.Partial)
	itl := map[string]float64{}
	for _, latency := range result.Latency {
		if latency.Name == "itl" {
			itl[latency.Pod] = latency.Samples
		}
	}
	assert.Equal(t, map[string]float64{"generation-pod": 2}, itl,
		"the decode half keeps its inter-token latency")

	// A decode half whose inter-token histogram stops moving is still reported.
	stage.Store(2)
	obj, err = h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result = obj.(*worker.ModelDeploymentMetrics)
	assert.True(t, result.Partial)
	assert.Equal(t, []worker.ModelDeploymentMetricMissing{{
		Pod: "generation-pod", Source: "vllm:inter_token_latency_seconds",
		Reason: "no observations in the sampling window",
	}}, result.Missing)
}

func TestModelDeploymentMetricsHandler_UnexportedErrorCounterIsNotPartial(t *testing.T) {
	tests := []struct {
		name        string
		windows     []string
		wantReason  string
		wantPartial bool
	}{
		{"request counter read in the same scrape", []string{"ttft", "tpot", "requests"}, modelDeploymentLabeledCounterUnexported, false},
		{"request counter absent too", []string{"ttft", "tpot"}, "metric is absent", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := metricsModelDeployment()
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}
			h := &ModelDeploymentMetricsHandler{}
			read := &modelDeploymentPodScrape{
				pod:    core.Pod{ObjectMeta: meta.ObjectMeta{Name: "router", UID: "router-uid"}},
				router: true,
				value:  modelDeploymentScrape{windows: map[string]modelDeploymentWindowPair{}},
				at:     time.Now(),
			}
			for _, key := range tc.windows {
				read.value.windows[key] = modelDeploymentWindowPair{sum: 1, count: 1}
			}
			h.mergeWindowMetrics(&worker.ModelDeploymentMetrics{}, md, read)
			for _, key := range tc.windows {
				read.value.windows[key] = modelDeploymentWindowPair{sum: 3, count: 3}
			}
			read.at = read.at.Add(time.Second)
			result := &worker.ModelDeploymentMetrics{}
			h.mergeWindowMetrics(result, md, read)
			assert.Contains(t, result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: "router", Source: "llm_d_epp_request_error_total", Reason: tc.wantReason,
			})
			assert.Equal(t, tc.wantPartial, modelDeploymentMissingIsPartial(result.Missing))
			for _, metric := range result.Traffic {
				assert.NotEqual(t, "error-ratio", metric.Name, "no fraction without an error counter")
			}
		})
	}
}

func TestModelDeploymentMetricsHandler_VLLMRouterPDReadsItsOwnSeries(t *testing.T) {
	body, err := os.ReadFile("testdata/model_deployment_metrics/vllm_router_pd_serving.prom")
	require.NoError(t, err)
	parsed, err := parseModelDeploymentMetrics(body, "", "vllm-router")
	require.NoError(t, err)
	assert.Equal(t, modelDeploymentWindowPair{count: 1}, parsed.windows["pd-requests"])
	assert.NotContains(t, parsed.windows, "pd-errors")
	assert.NotContains(t, parsed.windows, "successful-requests")

	md := metricsModelDeployment()
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "vllm-router"}
	md.Spec.Roles = []workercore.ModelDeploymentRole{
		{Name: "prompt", Kind: workercore.ModelDeploymentRoleKindPrefill},
		{Name: "generation", Kind: workercore.ModelDeploymentRoleKindDecode},
	}
	h := &ModelDeploymentMetricsHandler{}
	read := &modelDeploymentPodScrape{
		pod: core.Pod{ObjectMeta: meta.ObjectMeta{Name: "router", UID: "router-uid"}}, router: true,
		value: parsed, at: time.Now(),
	}
	h.mergeMetrics(&worker.ModelDeploymentMetrics{}, md, read)
	read.value.windows["pd-requests"] = modelDeploymentWindowPair{count: 5}
	read.at = read.at.Add(2 * time.Second)
	result := &worker.ModelDeploymentMetrics{}
	h.mergeMetrics(result, md, read)

	assert.Empty(t, result.Processing, "the P/D router exports no processing gauge")
	require.Len(t, result.Traffic, 1)
	assert.Equal(t, "pd-requests", result.Traffic[0].Name)
	assert.Equal(t, "vllm_router_pd_requests_total", result.Traffic[0].Source)
	assert.Equal(t, float64(2), result.Traffic[0].Value)
	assert.ElementsMatch(t, []worker.ModelDeploymentMetricMissing{
		{Pod: "router", Source: "vllm_router_running_requests", Reason: modelDeploymentVLLMRouterPDNoProcessing},
		{Pod: "router", Source: "vllm_router_active_workers", Reason: modelDeploymentVLLMRouterPDNoProcessing},
		{Pod: "router", Source: "vllm_router_pd_errors_total", Reason: modelDeploymentLabeledCounterUnexported},
	}, result.Missing)
	assert.False(t, modelDeploymentMissingIsPartial(result.Missing))

	// The same router in front of one server keeps its non-P/D series.
	md.Spec.Roles = []workercore.ModelDeploymentRole{{Name: "server"}}
	result = &worker.ModelDeploymentMetrics{}
	h.mergeMetrics(result, md, read)
	assert.Contains(t, result.Missing, worker.ModelDeploymentMetricMissing{
		Pod: "router", Source: "vllm_router_running_requests", Reason: "metric is absent",
	})
	assert.True(t, modelDeploymentMissingIsPartial(result.Missing))
}

// sglangPDResponse follows what each half of a pinned SGLang pair exported under load: the
// prefill half carries the prefill token counters and the KV transfer histograms but no TTFT;
// the decode half carries TTFT and ITL but never prefills, so its token counters stay at zero.
func sglangPDResponse(prefill bool, step int) string {
	queue := "# TYPE sglang:num_running_reqs gauge\nsglang:num_running_reqs 1\n" +
		"# TYPE sglang:num_queue_reqs gauge\nsglang:num_queue_reqs 0\n" +
		"# TYPE sglang:num_decode_transfer_queue_reqs gauge\nsglang:num_decode_transfer_queue_reqs 0\n"
	if prefill {
		return queue + fmt.Sprintf(`# TYPE sglang:prefill_effective_tokens_total counter
sglang:prefill_effective_tokens_total{mode="input"} %d
sglang:prefill_effective_tokens_total{mode="device_hit"} %d
sglang:prefill_effective_tokens_total{mode="host_hit"} 0
sglang:prefill_effective_tokens_total{mode="storage_hit"} 0
# TYPE sglang:kv_transfer_latency_ms histogram
sglang:kv_transfer_latency_ms_sum %d
sglang:kv_transfer_latency_ms_count %d
# TYPE sglang:kv_transfer_total_mb histogram
sglang:kv_transfer_total_mb_sum %d
sglang:kv_transfer_total_mb_count %d
# TYPE sglang:kv_transfer_speed_gb_s histogram
sglang:kv_transfer_speed_gb_s_sum %d
sglang:kv_transfer_speed_gb_s_count %d
`, 10*step, 20*step, 100*step, step, 2*step, step, step, step)
	}
	return queue + fmt.Sprintf(`# TYPE sglang:prefill_effective_tokens_total counter
sglang:prefill_effective_tokens_total{mode="input"} 0
sglang:prefill_effective_tokens_total{mode="device_hit"} 0
sglang:prefill_effective_tokens_total{mode="host_hit"} 0
sglang:prefill_effective_tokens_total{mode="storage_hit"} 0
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_sum %d
sglang:time_to_first_token_seconds_count %d
# TYPE sglang:inter_token_latency_seconds histogram
sglang:inter_token_latency_seconds_sum %d
sglang:inter_token_latency_seconds_count %d
`, step, step, 3*step, 30*step)
}

func sglangPDHandler(t *testing.T) (*ModelDeploymentMetricsHandler, types.NamespacedName, *atomic.Int32) {
	t.Helper()
	step := &atomic.Int32{}
	step.Store(1)
	serve := func(prefill bool) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, sglangPDResponse(prefill, int(step.Load())))
		}))
		t.Cleanup(server.Close)
		return server
	}
	md := metricsModelDeployment()
	md.Spec.Engine.Name = "sglang"
	md.Spec.Roles = []workercore.ModelDeploymentRole{
		{Name: "prompt", Kind: workercore.ModelDeploymentRoleKindPrefill},
		{Name: "generation", Kind: workercore.ModelDeploymentRoleKindDecode},
	}
	pod := func(server *httptest.Server, name, role string) *core.Pod {
		u, err := url.Parse(server.URL)
		require.NoError(t, err)
		host, port, err := net.SplitHostPort(u.Host)
		require.NoError(t, err)
		p := metricsEnginePod(md, name, host, port)
		p.Labels["app.kubernetes.io/component"] = role
		return p
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md,
		pod(serve(true), "prompt-pod", "prompt"), pod(serve(false), "generation-pod", "generation")).Build()
	return &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: http.DefaultClient},
		types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, step
}

func TestModelDeploymentMetricsHandler_SGLangPDReadsEachSeriesFromItsHalf(t *testing.T) {
	h, key, step := sglangPDHandler(t)
	_, err := h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	step.Store(3)
	obj, err := h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)

	transfer := map[string]string{}
	for _, metric := range result.Transfer {
		transfer[metric.Name] = metric.Pod
	}
	assert.Equal(t, map[string]string{
		"transfer-latency": "prompt-pod", "transfer-size": "prompt-pod", "transfer-speed": "prompt-pod",
	}, transfer, "the prefill half sends the blocks and observes the transfer")
	for _, hit := range result.CacheHits {
		assert.Equal(t, "prompt-pod", hit.Pod, "only the prefill half prefills")
	}
	assert.Len(t, result.CacheHits, 3)
	latency := map[string]string{}
	for _, metric := range result.Latency {
		latency[metric.Pod+"/"+metric.Name] = metric.Source
	}
	assert.Equal(t, map[string]string{
		"generation-pod/ttft": "sglang:time_to_first_token_seconds",
		"generation-pod/itl":  "sglang:inter_token_latency_seconds",
	}, latency)
	assert.Equal(t, []worker.ModelDeploymentMetricMissing{{
		Pod: "prompt-pod", Source: "sglang:num_transfer_failed_reqs_total", Reason: modelDeploymentLabeledCounterUnexported,
	}}, result.Missing)
	assert.False(t, result.Partial)
}

func TestModelDeploymentMetricsHandler_SGLangTransferFailuresNeedTheirPair(t *testing.T) {
	tests := []struct {
		name        string
		windows     []string
		wantReason  string
		wantPartial bool
	}{
		{"transfer size read in the same scrape", []string{"transfer-latency", "transfer-size", "transfer-speed"}, modelDeploymentLabeledCounterUnexported, false},
		{"transfer size absent", []string{"transfer-latency", "transfer-speed"}, "metric is absent", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := metricsModelDeployment()
			md.Spec.Engine.Name = "sglang"
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{Name: "prompt", Kind: workercore.ModelDeploymentRoleKindPrefill},
				{Name: "generation", Kind: workercore.ModelDeploymentRoleKindDecode},
			}
			pod := core.Pod{ObjectMeta: meta.ObjectMeta{
				Name: "prompt-pod", UID: "prompt-uid", Labels: map[string]string{"app.kubernetes.io/component": "prompt"},
			}}
			read := &modelDeploymentPodScrape{pod: pod, value: modelDeploymentScrape{windows: map[string]modelDeploymentWindowPair{}}, at: time.Now()}
			for _, key := range tc.windows {
				read.value.windows[key] = modelDeploymentWindowPair{sum: 1, count: 1}
			}
			result := &worker.ModelDeploymentMetrics{}
			(&ModelDeploymentMetricsHandler{}).mergeWindowMetrics(result, md, read)
			var failures []worker.ModelDeploymentMetricMissing
			for _, missing := range result.Missing {
				if missing.Source == "sglang:num_transfer_failed_reqs_total" {
					failures = append(failures, missing)
				}
			}
			assert.Equal(t, []worker.ModelDeploymentMetricMissing{{
				Pod: "prompt-pod", Source: "sglang:num_transfer_failed_reqs_total", Reason: tc.wantReason,
			}}, failures)
			assert.Equal(t, tc.wantPartial, modelDeploymentMissingIsPartial(failures))
		})
	}
}

// metricsFixture parses a trimmed scrape of a Pod the serving matrix read under load.
func metricsFixture(t *testing.T, name, engine, router string) modelDeploymentScrape {
	t.Helper()
	body, err := os.ReadFile("testdata/model_deployment_metrics/" + name + ".prom")
	require.NoError(t, err)
	parsed, err := parseModelDeploymentMetrics(body, engine, router)
	require.NoError(t, err)
	return parsed
}

// mergeMetricsWindow merges two reads of one Pod thirty seconds apart and returns what the second
// read contributed.
func mergeMetricsWindow(
	md *workercore.ModelDeployment, pod core.Pod, router bool, first, second modelDeploymentScrape,
) *worker.ModelDeploymentMetrics {
	h := &ModelDeploymentMetricsHandler{}
	at := time.Now()
	h.mergeMetrics(&worker.ModelDeploymentMetrics{}, md, &modelDeploymentPodScrape{pod: pod, router: router, value: first, at: at})
	result := &worker.ModelDeploymentMetrics{}
	h.mergeMetrics(result, md, &modelDeploymentPodScrape{pod: pod, router: router, value: second, at: at.Add(30 * time.Second)})
	return result
}

func metricsServerPod(name string, command ...string) core.Pod {
	return core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Labels: map[string]string{"app.kubernetes.io/component": "server"}},
		Spec:       core.PodSpec{Containers: []core.Container{{Name: "main", Command: command}}},
	}
}

func TestModelDeploymentMetricsHandler_VLLMExternalStoreNeedsAKVConnector(t *testing.T) {
	serve := []string{"vllm", "serve", "Qwen/Qwen2.5-0.5B-Instruct", "--enable-prefix-caching", "--port", "8000"}
	connector := append(slices.Clone(serve), "--kv-transfer-config",
		`{"kv_connector":"MooncakeConnector","kv_role":"kv_consumer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}`)
	tests := []struct {
		name        string
		command     []string
		queries     float64
		want        worker.ModelDeploymentMetricMissing
		wantPartial bool
	}{
		{"no KV connector rendered", serve, 0, worker.ModelDeploymentMetricMissing{Pod: "server", Source: "external-store", Reason: modelDeploymentVLLMNoKVConnector}, false},
		{"KV connector rendered", connector, 0, worker.ModelDeploymentMetricMissing{Pod: "server", Source: "external-store", Reason: "no queries in the sampling window"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := metricsModelDeployment()
			md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "pool"}}
			first := metricsFixture(t, "vllm_server_busy_first", "vllm", "")
			second := metricsFixture(t, "vllm_server_busy_second", "vllm", "")
			result := mergeMetricsWindow(md, metricsServerPod("server", tc.command...), false, first, second)
			assert.ElementsMatch(t, []worker.ModelDeploymentMetricMissing{tc.want}, result.Missing)
			assert.Equal(t, tc.wantPartial, modelDeploymentMissingIsPartial(result.Missing))
			scopes := map[string]float64{}
			for _, hit := range result.CacheHits {
				scopes[hit.Scope] = hit.Queries
			}
			assert.Equal(t, map[string]float64{"local-prefix": 37842}, scopes)
		})
	}

	// An external query counter that moved proves a connector the Pod arguments do not show, so
	// its window is read rather than declared unsupported.
	md := metricsModelDeployment()
	md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "pool"}}
	first := metricsFixture(t, "vllm_server_busy_first", "vllm", "")
	second := metricsFixture(t, "vllm_server_busy_second", "vllm", "")
	first.counters["external-store"] = modelDeploymentCounterPair{hits: 2, queries: 8}
	second.counters["external-store"] = modelDeploymentCounterPair{hits: 6, queries: 16}
	result := mergeMetricsWindow(md, metricsServerPod("server", serve...), false, first, second)
	assert.Empty(t, result.Missing)
	assert.Len(t, result.CacheHits, 2)
}

// metricsVLLMRoleWindow merges two busy vLLM reads of a Pod of the given role: "server" in an
// unpaired deployment, or "prompt" or "generation" in a prefill/decode pair. Its external prefix
// cache counters move, so whether the window is read depends on the role and the pool alone.
func metricsVLLMRoleWindow(t *testing.T, role string, pool bool) *worker.ModelDeploymentMetrics {
	t.Helper()
	md := metricsModelDeployment()
	if role != "server" {
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			{Name: "prompt", Kind: workercore.ModelDeploymentRoleKindPrefill},
			{Name: "generation", Kind: workercore.ModelDeploymentRoleKindDecode},
		}
	}
	if pool {
		md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "pool"}}
	}
	first := metricsFixture(t, "vllm_server_busy_first", "vllm", "")
	second := metricsFixture(t, "vllm_server_busy_second", "vllm", "")
	first.counters["external-store"] = modelDeploymentCounterPair{hits: 2, queries: 8}
	second.counters["external-store"] = modelDeploymentCounterPair{hits: 6, queries: 16}
	pod := metricsServerPod("pod", "vllm", "serve", "--kv-transfer-config", `{"kv_connector":"MultiConnector","kv_role":"kv_both"}`)
	pod.Labels["app.kubernetes.io/component"] = role
	return mergeMetricsWindow(md, pod, false, first, second)
}

func TestModelDeploymentMetricsHandler_VLLMExternalStoreIsReadOnlyFromAStore(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		pool     bool
		wantRead bool
	}{
		{"decode of a pair with a pool", "generation", true, false},
		{"decode of a pair without a pool", "generation", false, false},
		{"prefill of a pair without a pool", "prompt", false, false},
		{"unpaired server without a pool", "server", false, false},
		{"prefill of a pair with a pool", "prompt", true, true},
		{"unpaired server with a pool", "server", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := metricsVLLMRoleWindow(t, tc.role, tc.pool)
			scopes := map[string]float64{}
			for _, hit := range result.CacheHits {
				scopes[hit.Scope] = hit.Queries
			}
			if tc.wantRead {
				assert.Equal(t, map[string]float64{"local-prefix": 37842, "external-store": 8}, scopes)
				assert.Empty(t, result.Missing)
			} else {
				assert.Equal(t, map[string]float64{"local-prefix": 37842}, scopes)
				assert.Equal(t, []worker.ModelDeploymentMetricMissing{
					{Pod: "pod", Source: "external-store", Reason: modelDeploymentVLLMExternalNotStore},
				}, result.Missing)
			}
			assert.False(t, modelDeploymentMissingIsPartial(result.Missing))
		})
	}
}

func TestModelDeploymentMetricsHandler_VLLMPDPrefillDoesNotExpectTPOT(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		wantTPOT bool
	}{
		{"prefill of a pair", "prompt", false},
		{"decode of a pair", "generation", true},
		{"unpaired server", "server", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := metricsVLLMRoleWindow(t, tc.role, true)
			var tpot []string
			for _, latency := range result.Latency {
				if latency.Name == "tpot" {
					tpot = append(tpot, latency.Source)
				}
			}
			if tc.wantTPOT {
				assert.Equal(t, []string{"vllm:request_time_per_output_token_seconds"}, tpot)
			} else {
				assert.Empty(t, tpot)
			}
			for _, missing := range result.Missing {
				assert.NotEqual(t, "vllm:request_time_per_output_token_seconds", missing.Source)
			}
			assert.False(t, modelDeploymentMissingIsPartial(result.Missing))
		})
	}
}

func TestModelDeploymentMetricsHandler_VLLMRouterUnexportedRetriesExhaustedIsNotPartial(t *testing.T) {
	tests := []struct {
		name        string
		drop        string
		wantReason  string
		wantPartial bool
	}{
		{"request counter read in the same scrape", "", modelDeploymentLabeledCounterUnexported, false},
		{"request counter absent too", "successful-requests", "metric is absent", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := metricsModelDeployment()
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "vllm-router"}
			first := metricsFixture(t, "vllm_router_server_first", "", "vllm-router")
			second := metricsFixture(t, "vllm_router_server_second", "", "vllm-router")
			delete(first.windows, tc.drop)
			delete(second.windows, tc.drop)
			router := core.Pod{ObjectMeta: meta.ObjectMeta{Name: "router", UID: "router-uid"}}
			result := mergeMetricsWindow(md, router, true, first, second)
			assert.Contains(t, result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: "router", Source: "vllm_router_retries_exhausted_total", Reason: tc.wantReason,
			})
			assert.Equal(t, tc.wantPartial, modelDeploymentMissingIsPartial(result.Missing))
		})
	}
}

func TestModelDeploymentMetricsHandler_IdleServerHasNoNewSample(t *testing.T) {
	tests := []struct {
		name        string
		engine      string
		fixture     string
		change      func(first, second modelDeploymentScrape)
		want        []worker.ModelDeploymentMetricMissing
		wantPartial bool
	}{
		{
			name: "vLLM server that served no request", engine: "vllm", fixture: "vllm_server_idle",
			want: []worker.ModelDeploymentMetricMissing{
				{Pod: "server", Source: "vllm:time_to_first_token_seconds", Reason: modelDeploymentIdleWindow},
				{Pod: "server", Source: "vllm:request_time_per_output_token_seconds", Reason: modelDeploymentIdleWindow},
				{Pod: "server", Source: "vllm:inter_token_latency_seconds", Reason: modelDeploymentIdleWindow},
				{Pod: "server", Source: "external-store", Reason: modelDeploymentVLLMNoKVConnector},
				{Pod: "server", Source: "local-prefix", Reason: modelDeploymentIdleWindow},
			},
		},
		{
			name: "SGLang server that served no request", engine: "sglang", fixture: "sglang_server_idle",
			want: []worker.ModelDeploymentMetricMissing{
				{Pod: "server", Source: "sglang:time_to_first_token_seconds", Reason: modelDeploymentIdleWindow},
				{Pod: "server", Source: "sglang:inter_token_latency_seconds", Reason: modelDeploymentLabeledHistogramUnexported},
			},
		},
		{
			name: "vLLM server whose TTFT moved while ITL stood still", engine: "vllm", fixture: "vllm_server_busy",
			change: func(first, second modelDeploymentScrape) { second.windows["itl"] = first.windows["itl"] },
			want: []worker.ModelDeploymentMetricMissing{
				{Pod: "server", Source: "vllm:inter_token_latency_seconds", Reason: "no observations in the sampling window"},
				{Pod: "server", Source: "external-store", Reason: modelDeploymentVLLMNoKVConnector},
			},
			wantPartial: true,
		},
		{
			name: "vLLM server whose TTFT moved while its cache queries stood still", engine: "vllm", fixture: "vllm_server_busy",
			change: func(first, second modelDeploymentScrape) {
				second.counters["local-prefix"] = first.counters["local-prefix"]
			},
			want: []worker.ModelDeploymentMetricMissing{
				{Pod: "server", Source: "external-store", Reason: modelDeploymentVLLMNoKVConnector},
				{Pod: "server", Source: "local-prefix", Reason: "no queries in the sampling window"},
			},
			wantPartial: true,
		},
		{
			name: "vLLM server that served no request and exports no TTFT", engine: "vllm", fixture: "vllm_server_idle",
			change: func(first, second modelDeploymentScrape) {
				delete(first.windows, "ttft")
				delete(second.windows, "ttft")
			},
			want: []worker.ModelDeploymentMetricMissing{
				{Pod: "server", Source: "vllm:time_to_first_token_seconds", Reason: "metric is absent"},
				{Pod: "server", Source: "vllm:request_time_per_output_token_seconds", Reason: "no observations in the sampling window"},
				{Pod: "server", Source: "vllm:inter_token_latency_seconds", Reason: "no observations in the sampling window"},
				{Pod: "server", Source: "external-store", Reason: modelDeploymentVLLMNoKVConnector},
				{Pod: "server", Source: "local-prefix", Reason: "no queries in the sampling window"},
			},
			wantPartial: true,
		},
		{
			name: "SGLang server whose TTFT moved while ITL is not exported", engine: "sglang", fixture: "sglang_server_busy",
			change: func(first, second modelDeploymentScrape) {
				delete(first.windows, "itl")
				delete(second.windows, "itl")
			},
			want: []worker.ModelDeploymentMetricMissing{
				{Pod: "server", Source: "sglang:inter_token_latency_seconds", Reason: "metric is absent"},
			},
			wantPartial: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := metricsModelDeployment()
			md.Spec.Engine.Name = tc.engine
			first := metricsFixture(t, tc.fixture+"_first", tc.engine, "")
			second := metricsFixture(t, tc.fixture+"_second", tc.engine, "")
			if tc.change != nil {
				tc.change(first, second)
			}
			result := mergeMetricsWindow(md, metricsServerPod("server", "serve"), false, first, second)
			assert.ElementsMatch(t, tc.want, result.Missing)
			assert.Equal(t, tc.wantPartial, modelDeploymentMissingIsPartial(result.Missing))
		})
	}
}

// The snapshot a router in front of two servers gives when the router's cache-aware policy sends
// every request to one of them, read from what such a deployment exported.
func TestModelDeploymentMetricsHandler_RoutedServersWithOneIdleAreComplete(t *testing.T) {
	var stage atomic.Int32
	serve := func(fixture string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			name := fixture + "_first.prom"
			if stage.Load() == 1 {
				name = fixture + "_second.prom"
			}
			body, err := os.ReadFile("testdata/model_deployment_metrics/" + name)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(body)
		}))
		t.Cleanup(server.Close)
		return server
	}
	md := metricsModelDeployment()
	md.Spec.Roles[0].Replicas = 2
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "vllm-router"}
	pod := func(server *httptest.Server, name string) *core.Pod {
		u, err := url.Parse(server.URL)
		require.NoError(t, err)
		host, port, err := net.SplitHostPort(u.Host)
		require.NoError(t, err)
		return metricsEnginePod(md, name, host, port)
	}
	busy := pod(serve("vllm_server_busy"), "chat-server-busy")
	idle := pod(serve("vllm_server_idle"), "chat-server-idle")
	deployment := &app.Deployment{ObjectMeta: meta.ObjectMeta{
		Name: "chat-router", Namespace: md.Namespace, UID: "router-deployment",
		OwnerReferences: []meta.OwnerReference{{APIVersion: "worker.gpustack.ai/v1alpha1", Kind: "ModelDeployment", Name: md.Name, UID: md.UID, Controller: ptr.To(true)}},
	}}
	rs := &app.ReplicaSet{ObjectMeta: meta.ObjectMeta{
		Name: "chat-router-rs", Namespace: md.Namespace, UID: "router-rs",
		OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: ptr.To(true)}},
	}}
	router := pod(serve("vllm_router_server"), "chat-router-pod")
	delete(router.Labels, "app.kubernetes.io/component")
	router.Labels["modeldeployment.gpustack.ai/router"] = "vllm-router"
	router.OwnerReferences = []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr.To(true)}}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(md, deployment, rs, router, busy, idle).Build()
	h := &ModelDeploymentMetricsHandler{APIReader: cli, HTTPClient: http.DefaultClient}
	key := types.NamespacedName{Namespace: md.Namespace, Name: md.Name}

	obj, err := h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	assert.True(t, obj.(*worker.ModelDeploymentMetrics).Partial, "a first read has no sampling window")

	stage.Store(1)
	obj, err = h.OnGet(context.Background(), key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	result := obj.(*worker.ModelDeploymentMetrics)
	assert.Equal(t, []worker.ModelDeploymentMetricMissing{
		{Pod: "chat-router-pod", Source: "vllm_router_request_errors_total", Reason: modelDeploymentLabeledCounterUnexported},
		{Pod: "chat-router-pod", Source: "vllm_router_retries_exhausted_total", Reason: modelDeploymentLabeledCounterUnexported},
		{Pod: "chat-server-busy", Source: "external-store", Reason: modelDeploymentVLLMNoKVConnector},
		{Pod: "chat-server-idle", Source: "external-store", Reason: modelDeploymentVLLMNoKVConnector},
		{Pod: "chat-server-idle", Source: "local-prefix", Reason: modelDeploymentIdleWindow},
		{Pod: "chat-server-idle", Source: "vllm:inter_token_latency_seconds", Reason: modelDeploymentIdleWindow},
		{Pod: "chat-server-idle", Source: "vllm:request_time_per_output_token_seconds", Reason: modelDeploymentIdleWindow},
		{Pod: "chat-server-idle", Source: "vllm:time_to_first_token_seconds", Reason: modelDeploymentIdleWindow},
	}, result.Missing)
	assert.False(t, result.Partial)
	require.Len(t, result.CacheHits, 1)
	assert.Equal(t, "chat-server-busy", result.CacheHits[0].Pod)
	latency := map[string]string{}
	for _, metric := range result.Latency {
		latency[metric.Name] = metric.Pod
	}
	assert.Equal(t, map[string]string{"ttft": "chat-server-busy", "tpot": "chat-server-busy", "itl": "chat-server-busy"}, latency)
}
