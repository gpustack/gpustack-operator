package worker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	require.Len(t, result.Traffic, 3)
	for _, metric := range result.Traffic {
		if metric.Name == "error-ratio" {
			assert.InDelta(t, 1.0/3.0, metric.Value, 1e-9)
			assert.Equal(t, float64(3), metric.Samples)
		}
	}
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
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}
	h := &ModelDeploymentMetricsHandler{}
	pod := core.Pod{ObjectMeta: meta.ObjectMeta{Name: "chat-router", UID: "router-uid"}}
	base := time.Now()
	tests := []struct {
		seconds int
		body    string
		missing bool
	}{
		{0, "# TYPE llm_d_epp_request_total counter\nllm_d_epp_request_total 1\n# TYPE llm_d_epp_request_error_total counter\nllm_d_epp_request_error_total 0\n", false},
		{1, "# TYPE llm_d_epp_request_total counter\nllm_d_epp_request_total 3\n", false},
		{2, "# TYPE llm_d_epp_request_total counter\nllm_d_epp_request_total 5\n# TYPE llm_d_epp_request_error_total counter\nllm_d_epp_request_error_total 1\n", true},
	}
	for _, tc := range tests {
		parsed, err := parseModelDeploymentMetrics([]byte(tc.body), "", "llm-d-router")
		require.NoError(t, err)
		result := &worker.ModelDeploymentMetrics{}
		read := &modelDeploymentPodScrape{pod: pod, router: true, value: parsed, at: base.Add(time.Duration(tc.seconds) * time.Second)}
		h.mergeWindowMetrics(result, md, read)
		if tc.missing {
			for _, metric := range result.Traffic {
				assert.NotEqual(t, "error-ratio", metric.Name)
			}
			assert.Contains(t, result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: pod.Name, Source: "llm_d_epp_request_total+llm_d_epp_request_error_total",
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
	for _, missing := range result.Missing {
		if missing.Source == "sglang:kv_transfer_latency_ms" {
			assert.Equal(t, "generation-pod", missing.Pod)
		}
	}
}
