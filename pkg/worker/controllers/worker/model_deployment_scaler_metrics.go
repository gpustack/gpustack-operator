package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	core "k8s.io/api/core/v1"

	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

// modelDeploymentScalerLatencyQuantile is the quantile of a latency histogram the sampler
// reports.
//
// IT IS THE NINETY-FIFTH PERCENTILE, because a tail is what a latency guard acts on: a
// deployment whose p95 is inside its target is serving inside its target, and one whose
// average is inside it while its p95 is not is not.
const modelDeploymentScalerLatencyQuantile = 0.95

var defaultModelDeploymentScalerHTTPClient = &http.Client{
	Transport: httpx.Transport(httpx.TransportOptions().WithoutProxy()),
	Timeout:   samplerPodTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// modelDeploymentScalerRankLabels are the rank labels an engine may put on a series it
// exports from one process of a multi-rank engine. The zero rank is the one that holds the
// engine's own accounting.
var modelDeploymentScalerRankLabels = []string{"tp_rank", "pp_rank", "moe_ep_rank"}

// modelDeploymentScalerPodSample is what one engine Pod reported in one pass.
type modelDeploymentScalerPodSample struct {
	// restartCount says which process the numbers belong to.
	restartCount int32
	// observedAt is when the pass read the Pod.
	observedAt time.Time

	// running, waiting and decodeTransfer are the request gauges.
	running        float64
	waiting        float64
	decodeTransfer float64
	// kvUsage is the engine's cache usage, as a fraction of the cache it holds.
	kvUsage float64
	// kvUsageFound says whether the cache usage was reported at all.
	kvUsageFound bool
	// requestsReadable says whether the request gauges were readable.
	requestsReadable bool

	// promptTokens, generationTokens and cacheHits are the token counters.
	promptTokens     float64
	generationTokens float64
	cacheHits        float64
	// cacheQueries is how many times the prefix cache was asked for a prompt.
	cacheQueries float64
	// cacheQueriesFound says whether the prefix cache was asked at all.
	cacheQueriesFound bool
	// effectiveInput is the prompt token count an engine reports as actually prefilled.
	effectiveInput float64
	// effectiveInputFound says whether that count was reported at all.
	effectiveInputFound bool
	// effectiveDevice, effectiveHost and effectiveStorage are the cache hit counts.
	effectiveDevice  float64
	effectiveHost    float64
	effectiveStorage float64
	// effectiveHitsFound says whether any cache hit count was reported.
	effectiveHitsFound bool
	// tokensReadable says whether the token counters were readable.
	tokensReadable bool

	// ttft and itl are the latency histograms.
	ttft modelDeploymentScalerHistogram
	itl  modelDeploymentScalerHistogram
	// ttftFound and itlFound say whether the latency histograms were reported.
	ttftFound bool
	itlFound  bool
	// latencyReadable says whether both latency histograms were readable.
	latencyReadable bool
}

// modelDeploymentScalerHistogram is one latency histogram as it was read.
type modelDeploymentScalerHistogram struct {
	// sum and count are the histogram's own totals.
	sum   float64
	count float64
	// buckets holds the cumulative count of every upper bound the histogram exports.
	buckets map[float64]float64
}

// modelDeploymentScalerWindow is one Pod's change over the window since its last sample.
type modelDeploymentScalerWindow struct {
	// state says whether the window is a reading, a stale one, or none at all.
	state algorithm.SignalState
	// reason says why the window is not a reading.
	reason string
	// seconds is the length of the window.
	seconds float64
	// promptTokens and generationTokens are what the window saw.
	promptTokens     float64
	generationTokens float64
	// cacheMissedPromptTokens are the prompt tokens the cache did not serve.
	cacheMissedPromptTokens float64
	// ttft and itl are the latency the window saw.
	ttft modelDeploymentScalerHistogram
	itl  modelDeploymentScalerHistogram
	// gaugesReadable says whether the Pod's request gauges were readable.
	gaugesReadable bool
	// tokensReadable says whether the Pod's token counters were readable.
	tokensReadable bool
	// latencyReadable says whether the Pod's latency histograms were readable.
	latencyReadable bool
}

// modelDeploymentScalerPodRead is one Pod's answer to a pass.
type modelDeploymentScalerPodRead struct {
	// pod is the Pod that was asked.
	pod *core.Pod
	// sample is what it reported, when it answered.
	sample modelDeploymentScalerPodSample
	// err is why it did not answer.
	err error
}

// scrape reads one Pod's exposition, over the injected transport when there is one.
func (s *modelDeploymentScalerSampler) scrape(ctx context.Context, pod *core.Pod) ([]byte, error) {
	if s.Scrape != nil {
		return s.Scrape(ctx, pod)
	}
	return s.scrapeHTTP(ctx, pod)
}

// scrapeHTTP reads one engine Pod's metrics endpoint over the sampler's own transport.
func (s *modelDeploymentScalerSampler) scrapeHTTP(ctx context.Context, pod *core.Pod) ([]byte, error) {
	url, ok := modelDeploymentScalerMetricsURL(pod)
	if !ok {
		return nil, fmt.Errorf("the pod advertises no metrics endpoint")
	}
	requestCtx, cancel := context.WithTimeout(ctx, samplerPodTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build the metrics request: %w", err)
	}
	client := s.HTTPClient
	if client == nil {
		client = defaultModelDeploymentScalerHTTPClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read the metrics endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the metrics endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, samplerMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the metrics body: %w", err)
	}
	if len(body) > samplerMaxBodyBytes {
		return nil, fmt.Errorf("the metrics body exceeds %d bytes", samplerMaxBodyBytes)
	}
	return body, nil
}

// modelDeploymentScalerMetricsURL returns the address an engine Pod's metrics are read at.
//
// IT IS READ FROM THE POD RATHER THAN DERIVED, because the annotations are written at render
// and carry the port the engine actually bound, which a role's own port declaration can
// disagree with.
func modelDeploymentScalerMetricsURL(pod *core.Pod) (string, bool) {
	if pod.Status.PodIP == "" || pod.Annotations["prometheus.io/scrape"] != modelDeploymentEndpointEligibleValue {
		return "", false
	}
	port, err := strconv.ParseInt(pod.Annotations["prometheus.io/port"], 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	if !modelDeploymentScalerPortDeclared(pod, int32(port)) {
		return "", false
	}
	scheme := pod.Annotations["prometheus.io/scheme"]
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	path := pod.Annotations["prometheus.io/path"]
	if path == "" {
		path = "/metrics"
	}
	if path != "/metrics" {
		return "", false
	}
	return scheme + "://" + net.JoinHostPort(pod.Status.PodIP, strconv.FormatInt(port, 10)) + path, true
}

func modelDeploymentScalerPortDeclared(pod *core.Pod, port int32) bool {
	for i := range pod.Spec.Containers {
		for j := range pod.Spec.Containers[i].Ports {
			if pod.Spec.Containers[i].Ports[j].ContainerPort == port {
				return true
			}
		}
	}
	return false
}

// modelDeploymentScalerParseSample reads one engine Pod's exposition into the sample the
// sampler keeps.
func modelDeploymentScalerParseSample(
	pod *core.Pod, body []byte, engine string, observedAt time.Time,
) (modelDeploymentScalerPodSample, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return modelDeploymentScalerPodSample{}, fmt.Errorf("parse Prometheus metrics: %w", err)
	}

	sample := modelDeploymentScalerPodSample{
		restartCount: modelDeploymentScalerRestartCount(pod),
		observedAt:   observedAt,
	}
	switch engine {
	case "vllm":
		modelDeploymentScalerReadVLLM(&sample, families)
	case "sglang":
		modelDeploymentScalerReadSGLang(&sample, families)
	}
	return sample, nil
}

// modelDeploymentScalerRestartCount reads how many times a Pod's engine container restarted.
func modelDeploymentScalerRestartCount(pod *core.Pod) int32 {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == modelDeploymentMainContainerName {
			return pod.Status.ContainerStatuses[i].RestartCount
		}
	}
	return 0
}

// modelDeploymentScalerReadVLLM reads the vLLM series out of one exposition.
func modelDeploymentScalerReadVLLM(
	sample *modelDeploymentScalerPodSample, families map[string]*dto.MetricFamily,
) {
	running, runningFound := modelDeploymentScalerSum(families, "vllm:num_requests_running", nil)
	waiting, waitingFound := modelDeploymentScalerSum(families, "vllm:num_requests_waiting", nil)
	sample.running = running
	sample.waiting = waiting
	sample.requestsReadable = runningFound && waitingFound
	sample.kvUsage, sample.kvUsageFound = modelDeploymentScalerSum(families, "vllm:kv_cache_usage_perc", nil)

	prompt, promptFound := modelDeploymentScalerSum(families, "vllm:prompt_tokens_total", nil)
	generation, generationFound := modelDeploymentScalerSum(families, "vllm:generation_tokens_total", nil)
	hits, _ := modelDeploymentScalerSum(families, "vllm:prefix_cache_hits_total", nil)
	queries, queriesFound := modelDeploymentScalerSum(families, "vllm:prefix_cache_queries_total", nil)
	sample.promptTokens = prompt
	sample.generationTokens = generation
	sample.cacheHits = hits
	sample.cacheQueries = queries
	sample.cacheQueriesFound = queriesFound
	sample.tokensReadable = promptFound && generationFound

	sample.ttft, sample.ttftFound = modelDeploymentScalerReadHistogram(families, "vllm:time_to_first_token_seconds")
	sample.itl, sample.itlFound = modelDeploymentScalerReadHistogram(families, "vllm:inter_token_latency_seconds")
	sample.latencyReadable = sample.ttftFound && sample.itlFound
}

// modelDeploymentScalerReadSGLang reads the SGLang series out of one exposition.
func modelDeploymentScalerReadSGLang(
	sample *modelDeploymentScalerPodSample, families map[string]*dto.MetricFamily,
) {
	running, runningFound := modelDeploymentScalerSum(families, "sglang:num_running_reqs", nil)
	waiting, waitingFound := modelDeploymentScalerSum(families, "sglang:num_queue_reqs", nil)
	sample.running = running
	sample.waiting = waiting
	sample.requestsReadable = runningFound && waitingFound
	sample.kvUsage, sample.kvUsageFound = modelDeploymentScalerSum(families, "sglang:token_usage", nil)
	sample.decodeTransfer, _ = modelDeploymentScalerSum(families, "sglang:num_decode_transfer_queue_reqs", nil)

	prompt, promptFound := modelDeploymentScalerSum(families, "sglang:prompt_tokens_total", nil)
	generation, generationFound := modelDeploymentScalerSum(families, "sglang:generation_tokens_total", nil)
	sample.promptTokens = prompt
	sample.generationTokens = generation
	sample.tokensReadable = promptFound && generationFound

	const effective = "sglang:prefill_effective_tokens_total"
	sample.effectiveInput, sample.effectiveInputFound = modelDeploymentScalerSum(
		families, effective, map[string]string{"mode": "input"})
	device, deviceFound := modelDeploymentScalerSum(families, effective, map[string]string{"mode": "device_hit"})
	host, hostFound := modelDeploymentScalerSum(families, effective, map[string]string{"mode": "host_hit"})
	storage, storageFound := modelDeploymentScalerSum(families, effective, map[string]string{"mode": "storage_hit"})
	sample.effectiveDevice = device
	sample.effectiveHost = host
	sample.effectiveStorage = storage
	sample.effectiveHitsFound = deviceFound || hostFound || storageFound

	sample.ttft, sample.ttftFound = modelDeploymentScalerReadHistogram(families, "sglang:time_to_first_token_seconds")
	sample.itl, sample.itlFound = modelDeploymentScalerReadHistogram(families, "sglang:inter_token_latency_seconds")
	sample.latencyReadable = sample.ttftFound && sample.itlFound
}

// modelDeploymentScalerSum adds up the values of one metric family.
//
// IT ACCEPTS THE RANK-ZERO SERIES ONLY, so that a multi-rank engine's counters are counted
// once rather than once per rank, and it accepts a series that carries no rank at all,
// because an engine that reports no rank has one process and dropping its series would lose
// the whole signal.
func modelDeploymentScalerSum(
	families map[string]*dto.MetricFamily, name string, labels map[string]string,
) (float64, bool) {
	total := 0.0
	found := false
	for _, candidate := range []string{name, name + "_total"} {
		family := families[candidate]
		if family == nil {
			continue
		}
		for _, metric := range family.Metric {
			if !modelDeploymentScalerRankZero(metric) {
				continue
			}
			if !modelDeploymentScalerLabelsMatch(metric, labels) {
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

// modelDeploymentScalerRankZero reports whether a series belongs to the rank-zero process of a
// multi-rank engine.
//
// A SERIES THAT CARRIES NO RANK LABEL IS ACCEPTED, because an engine that reports no rank at
// all has one process and dropping its series would lose the signal entirely. A series that
// carries a rank label is accepted at rank zero only, because every other rank reports the
// same counters and summing them multiplies the engine's own numbers by its rank count.
func modelDeploymentScalerRankZero(metric *dto.Metric) bool {
	for _, label := range metric.Label {
		for _, rank := range modelDeploymentScalerRankLabels {
			if label.GetName() == rank && label.GetValue() != "0" {
				return false
			}
		}
	}
	return true
}

// modelDeploymentScalerLabelsMatch reports whether a series carries every label asked for.
func modelDeploymentScalerLabelsMatch(metric *dto.Metric, want map[string]string) bool {
	for name, value := range want {
		matched := false
		for _, label := range metric.Label {
			if label.GetName() == name && label.GetValue() == value {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// modelDeploymentScalerReadHistogram reads one latency histogram out of an exposition.
//
// IT SUMS THE SERIES THAT PASS THE RANK FILTER, so that a histogram split across a label
// set is read as the one histogram it describes.
func modelDeploymentScalerReadHistogram(
	families map[string]*dto.MetricFamily, name string,
) (modelDeploymentScalerHistogram, bool) {
	family := families[name]
	if family == nil || family.GetType() != dto.MetricType_HISTOGRAM {
		return modelDeploymentScalerHistogram{}, false
	}
	histogram := modelDeploymentScalerHistogram{buckets: map[float64]float64{}}
	found := false
	for _, metric := range family.Metric {
		if !modelDeploymentScalerRankZero(metric) {
			continue
		}
		if metric.Histogram == nil {
			continue
		}
		histogram.sum += metric.Histogram.GetSampleSum()
		histogram.count += float64(metric.Histogram.GetSampleCount())
		for _, bucket := range metric.Histogram.Bucket {
			histogram.buckets[bucket.GetUpperBound()] += float64(bucket.GetCumulativeCount())
		}
		found = true
	}
	return histogram, found
}

// modelDeploymentScalerWindowOf reads the change between two samples of one Pod.
func modelDeploymentScalerWindowOf(
	previous *modelDeploymentScalerPodSample,
	current modelDeploymentScalerPodSample,
	now time.Time,
	freshness time.Duration,
) modelDeploymentScalerWindow {
	window := modelDeploymentScalerWindow{
		state:           algorithm.SignalMissing,
		reason:          "the window over which the rate is read is not established yet",
		gaugesReadable:  current.requestsReadable,
		tokensReadable:  current.tokensReadable,
		latencyReadable: current.latencyReadable,
	}
	if previous == nil {
		return window
	}
	if current.restartCount > previous.restartCount {
		window.reason = "the engine container restarted since the last sample"
		return window
	}
	if modelDeploymentScalerCounterReset(*previous, current) {
		window.reason = "a counter went down since the last sample"
		return window
	}
	elapsed := now.Sub(previous.observedAt)
	if elapsed <= 0 {
		window.reason = "the last sample is not older than this one"
		return window
	}
	state := algorithm.SignalFresh
	if elapsed > freshness {
		state = algorithm.SignalStale
		window.reason = "the last sample is older than the freshness bound"
	}
	window.state = state
	window.seconds = elapsed.Seconds()
	window.promptTokens = current.promptTokens - previous.promptTokens
	window.generationTokens = current.generationTokens - previous.generationTokens
	window.cacheMissedPromptTokens = modelDeploymentScalerCacheMissedPromptTokens(*previous, current)
	window.ttft = modelDeploymentScalerHistogramDelta(previous.ttft, current.ttft)
	window.itl = modelDeploymentScalerHistogramDelta(previous.itl, current.itl)
	return window
}

// modelDeploymentScalerCounterReset reports whether any counter of a sample is lower than the
// same counter of the one before it.
//
// A COUNTER THAT WENT DOWN DID NOT GO DOWN: the process that reported it was replaced, and
// the number now belongs to a different life. Reading the change as a rate would read a
// restart as a burst of work that never happened.
func modelDeploymentScalerCounterReset(
	previous, current modelDeploymentScalerPodSample,
) bool {
	switch {
	case current.promptTokens < previous.promptTokens:
		return true
	case current.generationTokens < previous.generationTokens:
		return true
	case current.cacheHits < previous.cacheHits:
		return true
	case current.cacheQueriesFound && previous.cacheQueriesFound &&
		current.cacheQueries < previous.cacheQueries:
		return true
	case current.effectiveInput < previous.effectiveInput:
		return true
	case current.effectiveDevice < previous.effectiveDevice:
		return true
	case current.effectiveHost < previous.effectiveHost:
		return true
	case current.effectiveStorage < previous.effectiveStorage:
		return true
	case current.ttft.count < previous.ttft.count:
		return true
	case current.itl.count < previous.itl.count:
		return true
	}
	return false
}

// modelDeploymentScalerCacheMissedPromptTokens returns the prompt tokens a window saw that the
// cache did not serve.
//
// THE EFFECTIVE PREFILL COUNT IS AUTHORITATIVE WHERE IT IS READABLE, because it counts what
// was prefilled rather than what was asked for. Otherwise the cache hits are subtracted from
// the prompt tokens, which is what an engine reporting no prefill accounting leaves to be
// read.
//
// THE HITS ARE NOT BOUNDED BY THE QUERIES, because a bound that shrinks the hits would grow
// the cache-missed tokens and the rate read from them, and a rate that is too high scales a
// role up. The query counter is read to see the cache's own history and not to argue with the
// hits.
func modelDeploymentScalerCacheMissedPromptTokens(
	previous, current modelDeploymentScalerPodSample,
) float64 {
	if current.effectiveInputFound && previous.effectiveInputFound {
		return math.Max(0, current.effectiveInput-previous.effectiveInput)
	}
	hits := current.cacheHits - previous.cacheHits
	if current.effectiveHitsFound && previous.effectiveHitsFound {
		hits = (current.effectiveDevice - previous.effectiveDevice) +
			(current.effectiveHost - previous.effectiveHost) +
			(current.effectiveStorage - previous.effectiveStorage)
	}
	return math.Max(0, (current.promptTokens-previous.promptTokens)-hits)
}

// modelDeploymentScalerHistogramDelta returns the change between two readings of a histogram.
func modelDeploymentScalerHistogramDelta(
	previous, current modelDeploymentScalerHistogram,
) modelDeploymentScalerHistogram {
	delta := modelDeploymentScalerHistogram{
		sum:     current.sum - previous.sum,
		count:   current.count - previous.count,
		buckets: map[float64]float64{},
	}
	for bound, count := range current.buckets {
		delta.buckets[bound] = math.Max(0, count-previous.buckets[bound])
	}
	return delta
}

// modelDeploymentScalerHistogramMerge adds one histogram to another.
func modelDeploymentScalerHistogramMerge(
	into, other modelDeploymentScalerHistogram,
) modelDeploymentScalerHistogram {
	merged := modelDeploymentScalerHistogram{
		sum:     into.sum + other.sum,
		count:   into.count + other.count,
		buckets: map[float64]float64{},
	}
	for bound, count := range into.buckets {
		merged.buckets[bound] = count
	}
	for bound, count := range other.buckets {
		merged.buckets[bound] += count
	}
	return merged
}

// modelDeploymentScalerQuantileMilliseconds returns the quantile of a histogram in
// milliseconds.
//
// IT READS THE BUCKETS WHEN THEY RESOLVE THE QUANTILE, and falls back to the mean when they
// do not: a histogram exported without bucket boundaries, or one whose quantile falls in the
// infinite bucket, cannot say which observations it holds, and the mean is the only figure
// left that is true of them.
func modelDeploymentScalerQuantileMilliseconds(
	histogram modelDeploymentScalerHistogram, quantile float64,
) (float64, bool) {
	if histogram.count <= 0 {
		return 0, false
	}
	bounds := make([]float64, 0, len(histogram.buckets))
	for bound := range histogram.buckets {
		bounds = append(bounds, bound)
	}
	sort.Float64s(bounds)
	target := quantile * histogram.count
	for _, bound := range bounds {
		if math.IsInf(bound, 1) {
			break
		}
		if histogram.buckets[bound] >= target {
			return bound * 1000, true
		}
	}
	return histogram.sum / histogram.count * 1000, true
}

// modelDeploymentScalerWorseState returns the state that describes a role made of the states
// its Pods are in.
//
// A MISSING READING IS WORSE THAN A STALE ONE, because a stale reading is a reading of
// something that happened and a missing one is nothing at all.
func modelDeploymentScalerWorseState(
	states ...algorithm.SignalState,
) algorithm.SignalState {
	worst := algorithm.SignalFresh
	for _, state := range states {
		switch state {
		case algorithm.SignalMissing:
			return algorithm.SignalMissing
		case algorithm.SignalStale:
			worst = algorithm.SignalStale
		}
	}
	return worst
}
