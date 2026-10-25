package worker

import (
	"maps"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// The metrics subresource keeps the previous sample of every deployment in handler memory and
// replaces it on every read. Two readers of one deployment therefore share one baseline, and the
// reader that came second measures its window against a sample the first reader never took. The
// proof below drives two readers through one handler and reads the window each of them was
// given.
//
// The token counters an autoscaler needs are not exposed by the subresource at all, so the
// fixtures pin the series names one engine exports and the deltas they carry between two
// scrapes. A tensor-parallel engine exports the same counter on every rank, and summing the
// ranks multiplies the token count by the rank count, so the rank the sampler reads is pinned
// here as well.

const scalerPoCFixtureDir = "testdata/model_deployment_metrics/"

// scalerPoCRankZero is the label filter that keeps one rank of a multi-rank counter.
var scalerPoCRankZero = map[string]string{"tp_rank": "0"}

// scalerPoCModelDeployment is the deployment the proof reads.
func scalerPoCModelDeployment() *workercore.ModelDeployment {
	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "chat", Namespace: "team", UID: types.UID("md-one")},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
			Roles:  []workercore.ModelDeploymentRole{{Name: "server"}},
		},
	}
}

// scalerPoCPod is the engine Pod the proof reads.
func scalerPoCPod() core.Pod {
	return core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "chat-server-1", Namespace: "team", UID: types.UID("chat-server-1-uid"),
			Labels: map[string]string{"app.kubernetes.io/component": "server"},
		},
	}
}

// scalerPoCFixtureBody reads one fixture of the exposition an engine serves.
func scalerPoCFixtureBody(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(scalerPoCFixtureDir + name + ".prom")
	require.NoError(t, err)
	return body
}

// scalerPoCFamilies parses one fixture with the Prometheus text parser the subresource uses.
func scalerPoCFamilies(t *testing.T, name string) map[string]*dto.MetricFamily {
	t.Helper()
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(scalerPoCFixtureBody(t, name))))
	require.NoError(t, err)
	return families
}

// scalerPoCSeriesNames lists the series one exposition declares, without the label set. The name
// is what a sampler pins, so it is read from the exposition itself: a parsed family carries the
// name of a histogram without the suffixes its buckets, sum and count are written under.
func scalerPoCSeriesNames(t *testing.T, name string) []string {
	t.Helper()
	names := []string{}
	for _, line := range strings.Split(string(scalerPoCFixtureBody(t, name)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series, _, _ := strings.Cut(line, "{")
		series, _, _ = strings.Cut(series, " ")
		names = append(names, strings.TrimSpace(series))
	}
	return names
}

// scalerPoCCounterSum sums one counter or gauge family, keeping only the series that carry every
// requested label. An empty filter keeps every series of the family.
func scalerPoCCounterSum(t *testing.T, families map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	value, ok := metricSum(families, name, labels)
	require.True(t, ok, "family %s exports no series matching %v", name, labels)
	return value
}

// scalerPoCCounterDelta returns how much one counter family grew between two samples, keeping
// only the series that carry every requested label.
func scalerPoCCounterDelta(
	t *testing.T, first, second map[string]*dto.MetricFamily, name string, labels map[string]string,
) float64 {
	t.Helper()
	return scalerPoCCounterSum(t, second, name, labels) - scalerPoCCounterSum(t, first, name, labels)
}

// scalerPoCHistogramSample returns the summed count and sum of one histogram family, keeping only
// the series that carry every requested label.
func scalerPoCHistogramSample(
	t *testing.T, families map[string]*dto.MetricFamily, name string, labels map[string]string,
) (sum, count float64) {
	t.Helper()
	family := families[name]
	require.NotNil(t, family, "family %s is absent", name)
	for _, metric := range family.Metric {
		if !metricLabelsMatch(metric, labels) {
			continue
		}
		require.NotNil(t, metric.Histogram, "family %s exports a sample that is not a histogram", name)
		sum += metric.Histogram.GetSampleSum()
		count += float64(metric.Histogram.GetSampleCount())
	}
	return sum, count
}

// scalerPoCCacheMissedPromptTokens returns the prompt tokens one sampling window carried that
// the cache did not serve. vLLM reports the prompt tokens it read and the prefix cache hits
// separately, so the miss is the prompt delta less the hits delta. SGLang reports the effective
// prefill tokens of every cache tier, and its input tier counts only the tokens the cache
// missed, so that tier is the answer; the prompt and hit counters are the fallback for an engine
// that does not export it.
func scalerPoCCacheMissedPromptTokens(promptDelta, hitsDelta, effectiveInputDelta float64, hasEffectiveInput bool) float64 {
	if hasEffectiveInput {
		return effectiveInputDelta
	}
	return math.Max(0, promptDelta-hitsDelta)
}

// scalerPoCScrape is one sample of one Pod, as the metrics handler read it.
type scalerPoCScrape struct {
	caller string
	at     time.Time
	value  modelDeploymentScrape
}

// scalerPoCWindow is the sampling window one read measured, or the absence of one.
type scalerPoCWindow struct {
	seconds float64
	present bool
}

// scalerPoCObservation is what one caller's read of the shared handler state produced.
type scalerPoCObservation struct {
	caller  string
	at      time.Time
	cache   scalerPoCWindow
	latency scalerPoCWindow
}

// scalerPoCAdvance is what one more scrape of a Pod adds to the series the proof reads.
type scalerPoCAdvance struct {
	counterScope string
	hits         float64
	queries      float64
	observations float64
	duration     float64
}

// scalerPoCAdvancedScrape returns a copy of one scrape with the counters of one scope and every
// histogram advanced. A later scrape of a Pod carries larger counters, and the proof needs three
// samples of one Pod to show a window being cut, so the samples after the second fixture
// advance it instead of being written as more fixtures. The proof reads the length of the
// window, not the mean of a histogram, so every windowed series is advanced by the same amount.
func scalerPoCAdvancedScrape(sample modelDeploymentScrape, advance scalerPoCAdvance) modelDeploymentScrape {
	next := modelDeploymentScrape{
		gauges:   maps.Clone(sample.gauges),
		counters: maps.Clone(sample.counters),
		windows:  maps.Clone(sample.windows),
	}
	if pair, ok := next.counters[advance.counterScope]; ok {
		pair.hits += advance.hits
		pair.queries += advance.queries
		next.counters[advance.counterScope] = pair
	}
	for name, pair := range next.windows {
		pair.count += advance.observations
		pair.sum += advance.duration
		next.windows[name] = pair
	}
	return next
}

// scalerPoCDrive reads the given samples through one handler and reports the window each read
// measured against.
func scalerPoCDrive(
	t *testing.T, h *ModelDeploymentMetricsHandler, md *workercore.ModelDeployment, pod core.Pod,
	samples []scalerPoCScrape,
) []scalerPoCObservation {
	t.Helper()
	observations := make([]scalerPoCObservation, 0, len(samples))
	for _, sample := range samples {
		result := &worker.ModelDeploymentMetrics{}
		h.mergeMetrics(result, md, &modelDeploymentPodScrape{pod: pod, value: sample.value, at: sample.at})
		observations = append(observations, scalerPoCObservation{
			caller:  sample.caller,
			at:      sample.at,
			cache:   scalerPoCCacheWindow(result),
			latency: scalerPoCLatencyWindow(result),
		})
	}
	return observations
}

// scalerPoCCacheWindow returns the window the read reported for the prefix cache counters.
func scalerPoCCacheWindow(result *worker.ModelDeploymentMetrics) scalerPoCWindow {
	for _, hit := range result.CacheHits {
		if hit.Scope == "local-prefix" {
			return scalerPoCWindow{seconds: hit.WindowSeconds, present: true}
		}
	}
	return scalerPoCWindow{}
}

// scalerPoCLatencyWindow returns the window the read reported for the time to first token
// histogram.
func scalerPoCLatencyWindow(result *worker.ModelDeploymentMetrics) scalerPoCWindow {
	for _, latency := range result.Latency {
		if latency.Name == "ttft" {
			return scalerPoCWindow{seconds: latency.WindowSeconds, present: true}
		}
	}
	return scalerPoCWindow{}
}

func TestModelDeploymentScalerPoC_MetricWindowInterference(t *testing.T) {
	base := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	md := scalerPoCModelDeployment()
	pod := scalerPoCPod()
	first := scalerPoCFixtureScrape(t, "vllm_metrics_t0", "vllm")
	second := scalerPoCFixtureScrape(t, "vllm_metrics_t1", "vllm")
	third := scalerPoCAdvancedScrape(second, scalerPoCAdvance{
		counterScope: "local-prefix", hits: 150, queries: 200, observations: 12, duration: 0.21,
	})
	fourth := scalerPoCAdvancedScrape(second, scalerPoCAdvance{
		counterScope: "local-prefix", hits: 600, queries: 800, observations: 48, duration: 0.84,
	})

	tests := []struct {
		name    string
		samples []scalerPoCScrape
		want    []scalerPoCWindow
	}{
		{
			name: "one reader keeps the window it measured",
			samples: []scalerPoCScrape{
				{caller: "A", at: base, value: first},
				{caller: "A", at: base.Add(15 * time.Second), value: second},
				{caller: "A", at: base.Add(30 * time.Second), value: fourth},
			},
			want: []scalerPoCWindow{
				{present: false},
				{seconds: 15, present: true},
				{seconds: 15, present: true},
			},
		},
		{
			name: "a second reader cuts the window of the first",
			samples: []scalerPoCScrape{
				{caller: "A", at: base, value: first},
				{caller: "A", at: base.Add(15 * time.Second), value: second},
				{caller: "B", at: base.Add(17 * time.Second), value: third},
				{caller: "A", at: base.Add(30 * time.Second), value: fourth},
			},
			want: []scalerPoCWindow{
				{present: false},
				{seconds: 15, present: true},
				{seconds: 2, present: true},
				{seconds: 13, present: true},
			},
		},
		{
			name: "a second reader leaves the baseline a first reader then measures",
			samples: []scalerPoCScrape{
				{caller: "B", at: base.Add(17 * time.Second), value: third},
				{caller: "A", at: base.Add(30 * time.Second), value: fourth},
			},
			want: []scalerPoCWindow{
				{present: false},
				{seconds: 13, present: true},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &ModelDeploymentMetricsHandler{}
			got := scalerPoCDrive(t, h, md, pod, tc.samples)
			require.Len(t, got, len(tc.want))
			for i := range got {
				assert.Equal(t, tc.samples[i].caller, got[i].caller)
				assert.Equal(t, tc.want[i], got[i].cache, "prefix cache window of read %d", i)
				assert.Equal(t, tc.want[i], got[i].latency, "time to first token window of read %d", i)
			}
		})
	}
}

// scalerPoCFixtureScrape parses one fixture the way the subresource parses a Pod endpoint.
func scalerPoCFixtureScrape(t *testing.T, name, engine string) modelDeploymentScrape {
	t.Helper()
	parsed, err := parseModelDeploymentMetrics(scalerPoCFixtureBody(t, name), engine, "")
	require.NoError(t, err)
	return parsed
}

func TestModelDeploymentScalerPoC_TokenCounterSeries(t *testing.T) {
	tests := []struct {
		name       string
		first      string
		second     string
		labels     map[string]string
		wantSeries []string
		wantGauges map[string]float64
		wantDeltas map[string]float64
	}{
		{
			name:   "vLLM reports prompt, generation and prefix cache counters",
			first:  "vllm_metrics_t0",
			second: "vllm_metrics_t1",
			wantSeries: []string{
				"vllm:num_requests_running",
				"vllm:num_requests_waiting",
				"vllm:kv_cache_usage_perc",
				"vllm:prompt_tokens_total",
				"vllm:generation_tokens_total",
				"vllm:prefix_cache_hits_total",
				"vllm:prefix_cache_queries_total",
				"vllm:time_to_first_token_seconds_bucket",
				"vllm:time_to_first_token_seconds_sum",
				"vllm:time_to_first_token_seconds_count",
				"vllm:inter_token_latency_seconds_bucket",
				"vllm:inter_token_latency_seconds_sum",
				"vllm:inter_token_latency_seconds_count",
			},
			wantGauges: map[string]float64{
				"vllm:num_requests_running": 4,
				"vllm:num_requests_waiting": 2,
				"vllm:kv_cache_usage_perc":  0.35,
			},
			wantDeltas: map[string]float64{
				"vllm:prompt_tokens_total":        12000,
				"vllm:generation_tokens_total":    3600,
				"vllm:prefix_cache_hits_total":    3000,
				"vllm:prefix_cache_queries_total": 4000,
			},
		},
		{
			name:   "SGLang reports prompt and generation counters on every rank",
			first:  "sglang_metrics_t0",
			second: "sglang_metrics_t1",
			labels: scalerPoCRankZero,
			wantSeries: []string{
				"sglang:num_running_reqs",
				"sglang:num_queue_reqs",
				"sglang:token_usage",
				"sglang:prompt_tokens_total",
				"sglang:generation_tokens_total",
				"sglang:prefill_effective_tokens_total",
				"sglang:time_to_first_token_seconds_bucket",
				"sglang:time_to_first_token_seconds_sum",
				"sglang:time_to_first_token_seconds_count",
				"sglang:inter_token_latency_seconds_bucket",
				"sglang:inter_token_latency_seconds_sum",
				"sglang:inter_token_latency_seconds_count",
			},
			wantGauges: map[string]float64{
				"sglang:num_running_reqs": 4,
				"sglang:num_queue_reqs":   2,
				"sglang:token_usage":      0.38,
			},
			wantDeltas: map[string]float64{
				"sglang:prompt_tokens_total":     12000,
				"sglang:generation_tokens_total": 3600,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			names := scalerPoCSeriesNames(t, tc.first)
			for _, want := range tc.wantSeries {
				assert.Contains(t, names, want)
			}
			first := scalerPoCFamilies(t, tc.first)
			second := scalerPoCFamilies(t, tc.second)
			for name, want := range tc.wantGauges {
				assert.Equal(t, want, scalerPoCCounterSum(t, first, name, nil), "gauge %s", name)
			}
			for name, want := range tc.wantDeltas {
				assert.Equal(t, want, scalerPoCCounterDelta(t, first, second, name, tc.labels), "delta of %s", name)
			}
		})
	}
}

func TestModelDeploymentScalerPoC_VLLMCacheMissedPromptTokens(t *testing.T) {
	first := scalerPoCFamilies(t, "vllm_metrics_t0")
	second := scalerPoCFamilies(t, "vllm_metrics_t1")
	promptDelta := scalerPoCCounterDelta(t, first, second, "vllm:prompt_tokens_total", nil)
	hitsDelta := scalerPoCCounterDelta(t, first, second, "vllm:prefix_cache_hits_total", nil)
	generationDelta := scalerPoCCounterDelta(t, first, second, "vllm:generation_tokens_total", nil)

	assert.Equal(t, 9000.0, scalerPoCCacheMissedPromptTokens(promptDelta, hitsDelta, 0, false),
		"the prompt tokens the prefix cache did not serve")
	assert.Equal(t, 3600.0, generationDelta, "the tokens the engine generated")
}

func TestModelDeploymentScalerPoC_TensorParallelRankDeduplication(t *testing.T) {
	first := scalerPoCFamilies(t, "sglang_metrics_t0")
	second := scalerPoCFamilies(t, "sglang_metrics_t1")

	// Every rank of a tensor-parallel engine exports the same counter, so a sum that keeps every
	// rank counts the tokens of the engine once per rank.
	assert.Equal(t, 24000.0, scalerPoCCounterDelta(t, first, second, "sglang:prompt_tokens_total", nil),
		"the rank count multiplies the prompt tokens")
	assert.Equal(t, 12000.0, scalerPoCCounterDelta(t, first, second, "sglang:prompt_tokens_total", scalerPoCRankZero),
		"one rank carries the prompt tokens the engine processed")
	assert.Equal(t, 7200.0, scalerPoCCounterDelta(t, first, second, "sglang:generation_tokens_total", nil))
	assert.Equal(t, 3600.0, scalerPoCCounterDelta(t, first, second, "sglang:generation_tokens_total", scalerPoCRankZero))

	// The effective prefill counter reports the tokens of every cache tier, and its input tier
	// counts the tokens the cache missed.
	inputDelta := scalerPoCCounterDelta(t, first, second, "sglang:prefill_effective_tokens_total",
		map[string]string{"mode": "input", "tp_rank": "0"})
	deviceDelta := scalerPoCCounterDelta(t, first, second, "sglang:prefill_effective_tokens_total",
		map[string]string{"mode": "device_hit", "tp_rank": "0"})
	hostDelta := scalerPoCCounterDelta(t, first, second, "sglang:prefill_effective_tokens_total",
		map[string]string{"mode": "host_hit", "tp_rank": "0"})
	storageDelta := scalerPoCCounterDelta(t, first, second, "sglang:prefill_effective_tokens_total",
		map[string]string{"mode": "storage_hit", "tp_rank": "0"})

	assert.Equal(t, 8400.0, inputDelta, "the prompt tokens the cache missed")
	assert.Equal(t, 1500.0, deviceDelta)
	assert.Equal(t, 200.0, hostDelta)
	assert.Equal(t, 40.0, storageDelta)
	assert.Equal(t, 8400.0, scalerPoCCacheMissedPromptTokens(
		scalerPoCCounterDelta(t, first, second, "sglang:prompt_tokens_total", scalerPoCRankZero),
		deviceDelta+hostDelta+storageDelta, inputDelta, true,
	),
		"the effective prefill counter is the answer of an engine that exports it")

	// The subresource sums every rank of the same series, so a sampler that reads its sums
	// counts the tokens of the engine once per rank.
	parsed := scalerPoCFixtureScrape(t, "sglang_metrics_t0", "sglang")
	assert.Equal(t, 40000.0, parsed.counters["device-prefix"].hits,
		"the subresource counts the device tier hits of both ranks")
	_, observations := scalerPoCHistogramSample(t, first, "sglang:time_to_first_token_seconds", nil)
	assert.Equal(t, 232.0, observations, "the subresource counts the observations of both ranks")
	_, rankZeroObservations := scalerPoCHistogramSample(t, first, "sglang:time_to_first_token_seconds", scalerPoCRankZero)
	assert.Equal(t, 116.0, rankZeroObservations, "one rank observed the requests it answered")
}

func TestModelDeploymentScalerPoC_CacheMissedPromptTokens(t *testing.T) {
	tests := []struct {
		name              string
		promptDelta       float64
		hitsDelta         float64
		effectiveInput    float64
		hasEffectiveInput bool
		want              float64
	}{
		{
			name:        "the prompt tokens the cache did not serve",
			promptDelta: 12000, hitsDelta: 3000, want: 9000,
		},
		{
			name:        "a hit count above the prompt count is not a negative miss",
			promptDelta: 100, hitsDelta: 300, want: 0,
		},
		{
			name:        "the effective prefill counter answers for an engine that exports it",
			promptDelta: 12000, hitsDelta: 1740, effectiveInput: 8400, hasEffectiveInput: true, want: 8400,
		},
		{
			name:        "the prompt and hit counters answer for an engine without it",
			promptDelta: 12000, hitsDelta: 1740, want: 10260,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scalerPoCCacheMissedPromptTokens(
				tc.promptDelta, tc.hitsDelta, tc.effectiveInput, tc.hasEffectiveInput,
			)
			assert.Equal(t, tc.want, got)
		})
	}
}
