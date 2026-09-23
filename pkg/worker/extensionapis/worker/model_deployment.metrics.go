package worker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

const (
	modelDeploymentMetricsTimeout  = 8 * time.Second
	modelDeploymentScrapeTimeout   = 2 * time.Second
	modelDeploymentMetricsMaxBytes = 1 << 20
	modelDeploymentMetricsMaxPods  = 64
	modelDeploymentCounterMaxAge   = 5 * time.Minute

	// modelDeploymentMetricsConcurrency is sized so that every Pod up to the limit is fetched
	// within half the request budget even when each fetch runs to its own limit, leaving the
	// other half for discovery. With fewer concurrent fetches, Pods that hang would spend the
	// budget before the Pods queued behind them are asked at all.
	modelDeploymentMetricsConcurrency = 32
)

type modelDeploymentPreviousCounter struct {
	value modelDeploymentCounterPair
	at    time.Time
}

type modelDeploymentPreviousWindow struct {
	value modelDeploymentWindowPair
	at    time.Time
}

// ModelDeploymentMetricsHandler handles the authorized metrics subresource.
type ModelDeploymentMetricsHandler struct {
	extensionapi.GetOperation
	APIReader  ctrlcli.Reader
	HTTPClient *http.Client

	mu       sync.Mutex
	previous map[string]modelDeploymentPreviousCounter
	windows  map[string]modelDeploymentPreviousWindow
}

func newModelDeploymentMetricsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *ModelDeploymentMetricsHandler {
	h := &ModelDeploymentMetricsHandler{
		APIReader: opts.Manager.GetAPIReader(),
		HTTPClient: &http.Client{
			Transport: httpx.Transport(httpx.TransportOptions().WithoutProxy()),
			Timeout:   modelDeploymentScrapeTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		previous: make(map[string]modelDeploymentPreviousCounter),
		windows:  make(map[string]modelDeploymentPreviousWindow),
	}
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)
	return h
}

var (
	_ rest.Storage = (*ModelDeploymentMetricsHandler)(nil)
	_ rest.Getter  = (*ModelDeploymentMetricsHandler)(nil)
)

func (h *ModelDeploymentMetricsHandler) New() runtime.Object { return &worker.ModelDeploymentMetrics{} }
func (h *ModelDeploymentMetricsHandler) Destroy()            {}

type modelDeploymentPodScrape struct {
	pod    core.Pod
	router bool
	value  modelDeploymentScrape
	at     time.Time
	err    error
}

func (h *ModelDeploymentMetricsHandler) OnGet(
	ctx context.Context, key types.NamespacedName, _ ctrlcli.GetOptions,
) (runtime.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, modelDeploymentMetricsTimeout)
	defer cancel()
	md := &workercore.ModelDeployment{}
	if err := h.APIReader.Get(ctx, key, md); err != nil {
		return nil, err
	}
	list := &core.PodList{}
	if err := h.APIReader.List(ctx, list, ctrlcli.InNamespace(key.Namespace), ctrlcli.MatchingLabels{
		"app.kubernetes.io/name": "model-deployment", "app.kubernetes.io/instance": key.Name,
	}); err != nil {
		return nil, fmt.Errorf("list ModelDeployment Pods: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b core.Pod) int { return compareNames(a.Name, b.Name) })
	selected := make([]core.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		owned, err := h.ownedMetricsPod(ctx, md, pod)
		if err != nil {
			return nil, err
		}
		if owned {
			selected = append(selected, *pod)
		}
	}
	if len(selected) == 0 {
		return nil, kerrors.NewServiceUnavailable("ModelDeployment has no owned metrics Pods")
	}
	result := &worker.ModelDeploymentMetrics{ObjectMeta: meta.ObjectMeta{Name: md.Name, Namespace: md.Namespace}}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) != 0 {
			continue
		}
		found := 0
		for j := range selected {
			if selected[j].Labels["app.kubernetes.io/component"] == role.Name {
				found++
			}
		}
		expected := max(int(role.Replicas), 1) * max(int(role.ReplicaSize), 1)
		if found == 0 {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "engine/" + role.Name, Reason: "no owned metrics Pod",
			})
		} else if found < expected {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "engine/" + role.Name, Reason: fmt.Sprintf("%d of %d owned metrics Pods present", found, expected),
			})
		}
	}
	if md.Spec.Router != nil {
		found := 0
		for j := range selected {
			if selected[j].Labels["modeldeployment.gpustack.ai/router"] == md.Spec.Router.Name {
				found++
			}
		}
		expected := 1
		if md.Spec.Router.Replicas != nil {
			expected = max(int(*md.Spec.Router.Replicas), 1)
		}
		if found == 0 {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "router", Reason: "no owned metrics Pod",
			})
		} else if found < expected {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "router", Reason: fmt.Sprintf("%d of %d owned metrics Pods present", found, expected),
			})
		}
	}
	if len(selected) > modelDeploymentMetricsMaxPods {
		result.Partial = true
		result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
			Source: "pod-discovery", Reason: "Pod count exceeds the bounded scrape limit",
		})
		selected = selected[:modelDeploymentMetricsMaxPods]
	}
	reads := make([]modelDeploymentPodScrape, len(selected))
	var wg sync.WaitGroup
	sem := make(chan struct{}, modelDeploymentMetricsConcurrency)
	for i := range selected {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			reads[i] = h.scrapeMetricsPod(ctx, md, selected[i])
		}(i)
	}
	wg.Wait()
	readable := 0
	for i := range reads {
		read := &reads[i]
		if read.err != nil {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: "scrape", Reason: read.err.Error(),
			})
			continue
		}
		readable++
		h.mergeMetrics(result, md, read)
	}
	if readable == 0 {
		return nil, kerrors.NewServiceUnavailable("all ModelDeployment metrics Pods are unreadable: " + reads[0].err.Error())
	}
	slices.SortFunc(result.Processing, func(a, b worker.ModelDeploymentMetricGauge) int { return compareNames(a.Name+a.Scope, b.Name+b.Scope) })
	slices.SortFunc(result.Queueing, func(a, b worker.ModelDeploymentMetricGauge) int { return compareNames(a.Name+a.Scope, b.Name+b.Scope) })
	slices.SortFunc(result.CacheHits, func(a, b worker.ModelDeploymentCacheHit) int { return compareNames(a.Pod+a.Scope, b.Pod+b.Scope) })
	slices.SortFunc(result.Latency, func(a, b worker.ModelDeploymentMetricWindow) int { return compareNames(a.Pod+a.Name, b.Pod+b.Name) })
	slices.SortFunc(result.Traffic, func(a, b worker.ModelDeploymentMetricWindow) int { return compareNames(a.Pod+a.Name, b.Pod+b.Name) })
	slices.SortFunc(result.Transfer, func(a, b worker.ModelDeploymentMetricWindow) int { return compareNames(a.Pod+a.Name, b.Pod+b.Name) })
	slices.SortFunc(result.Missing, func(a, b worker.ModelDeploymentMetricMissing) int {
		return compareNames(a.Pod+a.Source, b.Pod+b.Source)
	})
	result.Partial = result.Partial || len(result.Missing) > 0
	result.Timestamp = meta.NewTime(time.Now())
	return result, nil
}

func compareNames(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (h *ModelDeploymentMetricsHandler) ownedMetricsPod(
	ctx context.Context, md *workercore.ModelDeployment, pod *core.Pod,
) (bool, error) {
	if pod.Labels["modeldeployment.gpustack.ai/router"] != "" {
		if md.Spec.Router == nil || pod.Labels["modeldeployment.gpustack.ai/router"] != md.Spec.Router.Name {
			return false, nil
		}
		podOwner := meta.GetControllerOf(pod)
		if podOwner == nil || podOwner.Kind != "ReplicaSet" {
			return false, nil
		}
		rs := &app.ReplicaSet{}
		if err := h.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: podOwner.Name}, rs); err != nil {
			if kerrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if rs.UID != podOwner.UID {
			return false, nil
		}
		rsOwner := meta.GetControllerOf(rs)
		if rsOwner == nil || rsOwner.Kind != "Deployment" || rsOwner.Name != md.Name+"-router" {
			return false, nil
		}
		deployment := &app.Deployment{}
		if err := h.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: rsOwner.Name}, deployment); err != nil {
			if kerrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if deployment.UID != rsOwner.UID {
			return false, nil
		}
		owner := meta.GetControllerOf(deployment)
		return owner != nil && owner.Kind == "ModelDeployment" && owner.UID == md.UID, nil
	}
	role := pod.Labels["app.kubernetes.io/component"]
	validRole := false
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name == role && len(md.Spec.Roles[i].Command) == 0 {
			validRole = true
			break
		}
	}
	owner := meta.GetControllerOf(pod)
	return validRole && owner != nil && owner.Kind == "ModelDeployment" && owner.UID == md.UID, nil
}

func (h *ModelDeploymentMetricsHandler) scrapeMetricsPod(
	ctx context.Context, md *workercore.ModelDeployment, pod core.Pod,
) modelDeploymentPodScrape {
	read := modelDeploymentPodScrape{pod: pod, router: pod.Labels["modeldeployment.gpustack.ai/router"] != ""}
	if pod.Status.PodIP == "" || pod.Annotations["prometheus.io/scrape"] != "true" ||
		pod.Annotations["prometheus.io/path"] != "/metrics" {
		read.err = fmt.Errorf("pod has no ready advertised metrics endpoint")
		return read
	}
	port, err := strconv.ParseInt(pod.Annotations["prometheus.io/port"], 10, 32)
	if err != nil || port < 1 || port > 65535 {
		read.err = fmt.Errorf("pod advertises an invalid metrics port")
		return read
	}
	if !modelDeploymentMetricsPortDeclared(&pod, int32(port)) {
		read.err = fmt.Errorf("pod advertises a metrics port absent from its containers")
		return read
	}
	scheme := pod.Annotations["prometheus.io/scheme"]
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		read.err = fmt.Errorf("pod advertises an invalid metrics scheme")
		return read
	}
	url := scheme + "://" + net.JoinHostPort(pod.Status.PodIP, strconv.FormatInt(port, 10)) + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		read.err = err
		return read
	}
	response, err := h.HTTPClient.Do(req)
	if err != nil {
		read.err = err
		return read
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		read.err = fmt.Errorf("metrics endpoint returned HTTP %d", response.StatusCode)
		return read
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, modelDeploymentMetricsMaxBytes+1))
	if err != nil {
		read.err = err
		return read
	}
	if len(body) > modelDeploymentMetricsMaxBytes {
		read.err = fmt.Errorf("metrics response exceeds %d bytes", modelDeploymentMetricsMaxBytes)
		return read
	}
	engine, router := md.Spec.Engine.Name, ""
	if read.router {
		engine, router = "", md.Spec.Router.Name
	}
	read.value, read.err = parseModelDeploymentMetrics(body, engine, router)
	read.at = time.Now()
	return read
}

func modelDeploymentMetricsPortDeclared(pod *core.Pod, port int32) bool {
	for _, container := range append(slices.Clone(pod.Spec.Containers), pod.Spec.InitContainers...) {
		for _, declared := range container.Ports {
			if declared.ContainerPort == port {
				return true
			}
		}
	}
	return false
}

func (h *ModelDeploymentMetricsHandler) mergeMetrics(
	result *worker.ModelDeploymentMetrics, md *workercore.ModelDeployment, read *modelDeploymentPodScrape,
) {
	metricNames := map[string]string{}
	switch {
	case read.router:
		switch md.Spec.Router.Name {
		case "llm-d-router":
			metricNames["router-running"] = "llm_d_epp_request_running"
			metricNames["router-backends"] = "llm_d_epp_ready_endpoints"
		case "vllm-router":
			metricNames["router-running"] = "vllm_router_running_requests"
			metricNames["router-reported-workers"] = "vllm_router_active_workers"
		case "sglang-gateway":
			metricNames["router-running"] = "smg_worker_requests_active"
			metricNames["router-backends"] = "smg_worker_pool_size"
		}
	case md.Spec.Engine.Name == "vllm":
		metricNames["running"] = "vllm:num_requests_running"
		metricNames["waiting"] = "vllm:num_requests_waiting"
	default:
		metricNames["running"] = "sglang:num_running_reqs"
		metricNames["waiting"] = "sglang:num_queue_reqs"
		if isModelDeploymentPD(md) && modelDeploymentPodRoleKind(md, &read.pod) == workercore.ModelDeploymentRoleKindDecode {
			metricNames["decode-transfer-waiting"] = "sglang:num_decode_transfer_queue_reqs"
		}
	}
	for key, source := range metricNames {
		value, ok := read.value.gauges[key]
		if !ok {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: source, Reason: "metric is absent",
			})
			continue
		}
		scope := "engine"
		if read.router {
			scope = "router"
		} else {
			scope += "/" + read.pod.Labels["app.kubernetes.io/component"]
		}
		gauge := worker.ModelDeploymentMetricGauge{
			Name: key, Source: source, Scope: scope, Unit: "requests",
			Value: value, PodCount: 1, ObservedAt: meta.NewTime(read.at),
		}
		switch key {
		case "router-backends":
			gauge.Unit = "backends"
		case "router-reported-workers":
			gauge.Unit = "workers"
		}
		if key == "waiting" || key == "decode-transfer-waiting" {
			mergeModelDeploymentGauge(&result.Queueing, gauge)
		} else {
			mergeModelDeploymentGauge(&result.Processing, gauge)
		}
	}
	h.mergeWindowMetrics(result, md, read)
	if read.router {
		return
	}
	if len(read.value.counters) == 0 {
		result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
			Pod: read.pod.Name, Source: "cache-hit counters", Reason: "metric is absent",
		})
		return
	}
	for scope, current := range read.value.counters {
		previous, ok := h.replaceCounter(md, &read.pod, scope, current, read.at)
		if !ok {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: "awaiting a second counter sample",
			})
			continue
		}
		hits := current.hits - previous.value.hits
		queries := current.queries - previous.value.queries
		if hits < 0 || queries < 0 || hits > queries {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: "counter reset or incompatible denominator",
			})
			continue
		}
		if queries == 0 {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: "no queries in the sampling window",
			})
			continue
		}
		source := "vllm:prefix_cache"
		if scope == "external-store" {
			source = "vllm:external_prefix_cache"
		}
		if md.Spec.Engine.Name == "sglang" {
			source = "sglang:prefill_effective_tokens_total"
		}
		result.CacheHits = append(result.CacheHits, worker.ModelDeploymentCacheHit{
			Pod: read.pod.Name, Source: source, Scope: scope, Unit: "tokens",
			Hits: hits, Queries: queries, Rate: hits / queries,
			WindowSeconds: read.at.Sub(previous.at).Seconds(),
			PodCount:      1, ObservedAt: meta.NewTime(read.at),
		})
	}
}

func isModelDeploymentPD(md *workercore.ModelDeployment) bool {
	hasPrefill, hasDecode := false, false
	for _, role := range md.Spec.Roles {
		hasPrefill = hasPrefill || role.Kind == workercore.ModelDeploymentRoleKindPrefill
		hasDecode = hasDecode || role.Kind == workercore.ModelDeploymentRoleKindDecode
	}
	return hasPrefill && hasDecode
}

func modelDeploymentPodRoleKind(md *workercore.ModelDeployment, pod *core.Pod) workercore.ModelDeploymentRoleKind {
	name := pod.Labels["app.kubernetes.io/component"]
	for _, role := range md.Spec.Roles {
		if role.Name == name {
			return role.Kind
		}
	}
	return ""
}

type modelDeploymentWindowDefinition struct {
	name, source, area, unit string
	histogram                bool
}

func modelDeploymentWindowDefinitions(md *workercore.ModelDeployment, router bool) []modelDeploymentWindowDefinition {
	if router {
		switch md.Spec.Router.Name {
		case "llm-d-router":
			return []modelDeploymentWindowDefinition{
				{"ttft", "llm_d_epp_request_ttft_seconds", "latency", "seconds", true},
				{"tpot", "llm_d_epp_request_streaming_tpot_seconds", "latency", "seconds", true},
				{"requests", "llm_d_epp_request_total", "traffic", "requests/second", false},
				{"errors", "llm_d_epp_request_error_total", "traffic", "errors/second", false},
			}
		case "vllm-router":
			return []modelDeploymentWindowDefinition{
				{"successful-requests", "vllm_router_requests_total", "traffic", "requests/second", false},
				{"errors", "vllm_router_request_errors_total", "traffic", "errors/second", false},
				{"retries-exhausted", "vllm_router_retries_exhausted_total", "traffic", "events/second", false},
			}
		case "sglang-gateway":
			return []modelDeploymentWindowDefinition{
				{"requests", "smg_router_requests_total", "traffic", "requests/second", false},
				{"errors", "smg_router_request_errors_total", "traffic", "errors/second", false},
				{"http-5xx-responses", "smg_http_responses_total", "traffic", "responses/second", false},
			}
		}
	}
	if md.Spec.Engine.Name == "vllm" {
		return []modelDeploymentWindowDefinition{
			{"ttft", "vllm:time_to_first_token_seconds", "latency", "seconds", true},
			{"tpot", "vllm:request_time_per_output_token_seconds", "latency", "seconds", true},
			{"itl", "vllm:inter_token_latency_seconds", "latency", "seconds", true},
		}
	}
	definitions := []modelDeploymentWindowDefinition{
		{"ttft", "sglang:time_to_first_token_seconds", "latency", "seconds", true},
		{"itl", "sglang:inter_token_latency_seconds", "latency", "seconds", true},
	}
	if isModelDeploymentPD(md) {
		definitions = append(definitions,
			modelDeploymentWindowDefinition{"transfer-latency", "sglang:kv_transfer_latency_ms", "transfer", "milliseconds", true},
			modelDeploymentWindowDefinition{"transfer-speed", "sglang:kv_transfer_speed_gb_s", "transfer", "GB/second", true},
			modelDeploymentWindowDefinition{"transfer-size", "sglang:kv_transfer_total_mb", "transfer", "MB", true},
			modelDeploymentWindowDefinition{"transfer-failures", "sglang:num_transfer_failed_reqs_total", "transfer", "errors/second", false},
		)
	}
	return definitions
}

func (h *ModelDeploymentMetricsHandler) mergeWindowMetrics(
	result *worker.ModelDeploymentMetrics, md *workercore.ModelDeployment, read *modelDeploymentPodScrape,
) {
	scope := "router"
	if !read.router {
		scope = "engine/" + read.pod.Labels["app.kubernetes.io/component"]
	}
	deltas := map[string]float64{}
	durations := map[string]float64{}
	starts := map[string]time.Time{}
	sources := map[string]string{}
	for _, definition := range modelDeploymentWindowDefinitions(md, read.router) {
		sources[definition.name] = definition.source
		if definition.area == "transfer" && modelDeploymentPodRoleKind(md, &read.pod) != workercore.ModelDeploymentRoleKindDecode {
			continue
		}
		current, ok := read.value.windows[definition.name]
		if !ok {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: "metric is absent",
			})
			continue
		}
		previous, ok := h.replaceWindow(md, &read.pod, definition.source, current, read.at)
		if !ok {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: "awaiting a second counter sample",
			})
			continue
		}
		window := read.at.Sub(previous.at).Seconds()
		count := current.count - previous.value.count
		sum := current.sum - previous.value.sum
		if count < 0 || sum < 0 {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: "counter reset",
			})
			continue
		}
		deltas[definition.name] = count
		durations[definition.name] = window
		starts[definition.name] = previous.at
		if count == 0 && definition.histogram {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: "no observations in the sampling window",
			})
			continue
		}
		value := count / window
		if definition.histogram {
			value = sum / count
		}
		metric := worker.ModelDeploymentMetricWindow{
			Name: definition.name, Pod: read.pod.Name, Source: definition.source,
			Scope: scope, Unit: definition.unit, Value: value, Samples: count,
			WindowSeconds: window, ObservedAt: meta.NewTime(read.at),
		}
		switch definition.area {
		case "latency":
			result.Latency = append(result.Latency, metric)
		case "traffic":
			result.Traffic = append(result.Traffic, metric)
		case "transfer":
			result.Transfer = append(result.Transfer, metric)
		}
	}
	if requests, ok := deltas["requests"]; ok {
		if errors, hasErrors := deltas["errors"]; hasErrors && !starts["requests"].Equal(starts["errors"]) {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: sources["requests"] + "+" + sources["errors"],
				Reason: "request and error counters have different sampling windows",
			})
		} else if hasErrors && requests > 0 && errors <= requests {
			result.Traffic = append(result.Traffic, worker.ModelDeploymentMetricWindow{
				Name: "error-ratio", Pod: read.pod.Name, Source: sources["requests"] + "+" + sources["errors"],
				Scope: scope, Unit: "fraction", Value: errors / requests, Samples: requests,
				WindowSeconds: durations["requests"], ObservedAt: meta.NewTime(read.at),
			})
		} else if hasErrors {
			reason := "no requests in the sampling window"
			if errors > requests {
				reason = "error counter exceeds request counter"
			}
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: sources["requests"] + "+" + sources["errors"], Reason: reason,
			})
		}
	}
}

func (h *ModelDeploymentMetricsHandler) replaceWindow(
	md *workercore.ModelDeployment, pod *core.Pod, source string,
	value modelDeploymentWindowPair, at time.Time,
) (modelDeploymentPreviousWindow, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.windows == nil {
		h.windows = make(map[string]modelDeploymentPreviousWindow)
	}
	for key, sample := range h.windows {
		if at.Sub(sample.at) > 2*modelDeploymentCounterMaxAge {
			delete(h.windows, key)
		}
	}
	key := string(md.UID) + "/" + string(pod.UID) + "/" + source
	previous, ok := h.windows[key]
	h.windows[key] = modelDeploymentPreviousWindow{value: value, at: at}
	return previous, ok && at.Sub(previous.at) > 0 && at.Sub(previous.at) <= modelDeploymentCounterMaxAge
}

func mergeModelDeploymentGauge(gauges *[]worker.ModelDeploymentMetricGauge, next worker.ModelDeploymentMetricGauge) {
	for i := range *gauges {
		current := &(*gauges)[i]
		if current.Name == next.Name && current.Source == next.Source && current.Scope == next.Scope {
			current.Value += next.Value
			current.PodCount++
			if next.ObservedAt.After(current.ObservedAt.Time) {
				current.ObservedAt = next.ObservedAt
			}
			return
		}
	}
	*gauges = append(*gauges, next)
}

func (h *ModelDeploymentMetricsHandler) replaceCounter(
	md *workercore.ModelDeployment, pod *core.Pod, scope string,
	value modelDeploymentCounterPair, at time.Time,
) (modelDeploymentPreviousCounter, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.previous == nil {
		h.previous = make(map[string]modelDeploymentPreviousCounter)
	}
	for key, sample := range h.previous {
		if at.Sub(sample.at) > 2*modelDeploymentCounterMaxAge {
			delete(h.previous, key)
		}
	}
	key := string(md.UID) + "/" + string(pod.UID) + "/" + scope
	previous, ok := h.previous[key]
	h.previous[key] = modelDeploymentPreviousCounter{value: value, at: at}
	return previous, ok && at.Sub(previous.at) > 0 && at.Sub(previous.at) <= modelDeploymentCounterMaxAge
}
