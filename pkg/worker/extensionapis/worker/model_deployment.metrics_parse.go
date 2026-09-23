package worker

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type modelDeploymentCounterPair struct {
	hits, queries float64
}

type modelDeploymentWindowPair struct {
	sum, count float64
}

type modelDeploymentScrape struct {
	gauges   map[string]float64
	counters map[string]modelDeploymentCounterPair
	windows  map[string]modelDeploymentWindowPair
}

// parseModelDeploymentMetrics accepts only the pinned engine and router metric families.
// Other Prometheus series stay on the Pod's own endpoint.
func parseModelDeploymentMetrics(body []byte, engine, router string) (modelDeploymentScrape, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return modelDeploymentScrape{}, fmt.Errorf("parse Prometheus metrics: %w", err)
	}
	out := modelDeploymentScrape{
		gauges:   map[string]float64{},
		counters: map[string]modelDeploymentCounterPair{},
		windows:  map[string]modelDeploymentWindowPair{},
	}
	if engine != "" {
		switch engine {
		case "vllm":
			addGauge(out.gauges, families, "running", "vllm:num_requests_running")
			addGauge(out.gauges, families, "waiting", "vllm:num_requests_waiting")
			addCounterPair(out.counters, families, "local-prefix", "vllm:prefix_cache_hits", "vllm:prefix_cache_queries")
			addCounterPair(out.counters, families, "external-store", "vllm:external_prefix_cache_hits", "vllm:external_prefix_cache_queries")
			addHistogram(out.windows, families, "ttft", "vllm:time_to_first_token_seconds")
			addHistogram(out.windows, families, "tpot", "vllm:request_time_per_output_token_seconds")
			addHistogram(out.windows, families, "itl", "vllm:inter_token_latency_seconds")
		case "sglang":
			addGauge(out.gauges, families, "running", "sglang:num_running_reqs")
			addGauge(out.gauges, families, "waiting", "sglang:num_queue_reqs")
			name := "sglang:prefill_effective_tokens_total"
			input, hasInput := metricSum(families, name, map[string]string{"mode": "input"})
			device, hasDevice := metricSum(families, name, map[string]string{"mode": "device_hit"})
			host, hasHost := metricSum(families, name, map[string]string{"mode": "host_hit"})
			storage, hasStorage := metricSum(families, name, map[string]string{"mode": "storage_hit"})
			if hasInput && hasDevice && hasHost && hasStorage {
				queries := input + device + host + storage
				out.counters["device-prefix"] = modelDeploymentCounterPair{device, queries}
				out.counters["host-prefix"] = modelDeploymentCounterPair{host, queries}
				out.counters["storage-prefix"] = modelDeploymentCounterPair{storage, queries}
			}
			addHistogram(out.windows, families, "ttft", "sglang:time_to_first_token_seconds")
			addHistogram(out.windows, families, "itl", "sglang:inter_token_latency_seconds")
			addGauge(out.gauges, families, "decode-transfer-waiting", "sglang:num_decode_transfer_queue_reqs")
			addHistogram(out.windows, families, "transfer-latency", "sglang:kv_transfer_latency_ms")
			addHistogram(out.windows, families, "transfer-speed", "sglang:kv_transfer_speed_gb_s")
			addHistogram(out.windows, families, "transfer-size", "sglang:kv_transfer_total_mb")
			addCounterWindow(out.windows, families, "transfer-failures", "sglang:num_transfer_failed_reqs_total")
		}
	}
	if router != "" {
		switch router {
		case "llm-d-router":
			addGauge(out.gauges, families, "router-running", "llm_d_epp_request_running")
			addGauge(out.gauges, families, "router-backends", "llm_d_epp_ready_endpoints")
			addHistogram(out.windows, families, "ttft", "llm_d_epp_request_ttft_seconds")
			addHistogram(out.windows, families, "tpot", "llm_d_epp_request_streaming_tpot_seconds")
			addCounterWindow(out.windows, families, "requests", "llm_d_epp_request_total")
			addCounterWindow(out.windows, families, "request-errors", "llm_d_epp_request_error_total")
		case "vllm-router":
			addGauge(out.gauges, families, "router-running", "vllm_router_running_requests")
			addGauge(out.gauges, families, "router-reported-workers", "vllm_router_active_workers")
			addCounterWindow(out.windows, families, "successful-requests", "vllm_router_requests_total")
			addCounterWindow(out.windows, families, "errors", "vllm_router_request_errors_total")
			addCounterWindow(out.windows, families, "retries-exhausted", "vllm_router_retries_exhausted_total")
			addCounterWindow(out.windows, families, "pd-requests", "vllm_router_pd_requests_total")
			addCounterWindow(out.windows, families, "pd-errors", "vllm_router_pd_errors_total")
		case "sglang-gateway":
			addGauge(out.gauges, families, "router-running", "smg_worker_requests_active")
			addGauge(out.gauges, families, "router-backends", "smg_worker_pool_size")
			addCounterWindow(out.windows, families, "requests", "smg_router_requests_total")
			addCounterWindow(out.windows, families, "errors", "smg_router_request_errors_total")
			if count, ok := sglangGatewayHTTP5xx(families); ok {
				out.windows["http-5xx-responses"] = modelDeploymentWindowPair{count: count}
			}
		}
	}
	return out, nil
}

func sglangGatewayHTTP5xx(families map[string]*dto.MetricFamily) (float64, bool) {
	family := families["smg_http_responses_total"]
	if family == nil || family.GetType() != dto.MetricType_COUNTER {
		return 0, false
	}
	var total float64
	observed := false
	for _, metric := range family.Metric {
		for _, label := range metric.Label {
			if label.GetName() != "status_code" {
				continue
			}
			status, err := strconv.Atoi(label.GetValue())
			if err == nil && status >= 100 && status < 600 && metric.Counter != nil && finiteNonnegative(metric.Counter.GetValue()) {
				observed = true
				if status >= 500 {
					total += metric.Counter.GetValue()
				}
			}
			break
		}
	}
	return total, observed
}

func addHistogram(out map[string]modelDeploymentWindowPair, families map[string]*dto.MetricFamily, key, name string) {
	family := families[name]
	if family == nil || family.GetType() != dto.MetricType_HISTOGRAM {
		return
	}
	var pair modelDeploymentWindowPair
	for _, metric := range family.Metric {
		if metric.Histogram != nil {
			pair.sum += metric.Histogram.GetSampleSum()
			pair.count += float64(metric.Histogram.GetSampleCount())
		}
	}
	if finiteNonnegative(pair.sum) && finiteNonnegative(pair.count) {
		out[key] = pair
	}
}

func addCounterWindow(out map[string]modelDeploymentWindowPair, families map[string]*dto.MetricFamily, key, name string) {
	if value, ok := metricSum(families, name, nil); ok {
		out[key] = modelDeploymentWindowPair{count: value}
	}
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func addGauge(out map[string]float64, families map[string]*dto.MetricFamily, key, name string) {
	if value, ok := metricSum(families, name, nil); ok {
		out[key] = value
	}
}

func addCounterPair(out map[string]modelDeploymentCounterPair, families map[string]*dto.MetricFamily,
	key, hitsName, queriesName string,
) {
	hits, hasHits := metricSum(families, hitsName, nil)
	queries, hasQueries := metricSum(families, queriesName, nil)
	if hasHits && hasQueries {
		out[key] = modelDeploymentCounterPair{hits, queries}
	}
}

func metricSum(families map[string]*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	var total float64
	found := false
	for _, candidate := range []string{name, name + "_total"} {
		family := families[candidate]
		if family == nil {
			continue
		}
		for _, metric := range family.Metric {
			if !metricLabelsMatch(metric, labels) {
				continue
			}
			value := math.NaN()
			if metric.Gauge != nil {
				value = metric.Gauge.GetValue()
			} else if metric.Counter != nil {
				value = metric.Counter.GetValue()
			}
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				continue
			}
			total += value
			found = true
		}
		if found {
			break
		}
	}
	return total, found
}

func metricLabelsMatch(metric *dto.Metric, want map[string]string) bool {
	for name, value := range want {
		found := false
		for _, label := range metric.Label {
			if label.GetName() == name && label.GetValue() == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
