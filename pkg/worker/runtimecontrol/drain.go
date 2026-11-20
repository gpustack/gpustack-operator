package runtimecontrol

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// DefaultMetricsPath is the Prometheus metrics scrape path on inference engines.
const DefaultMetricsPath = "/metrics"

type engineMetricSpec struct {
	runningMetric         string
	waitingMetric         string
	optionalWaitingMetric []string
}

var drainMetricSpecs = map[string]engineMetricSpec{
	workercore.ModelDeploymentEngineVLLM: {
		runningMetric: "vllm:num_requests_running",
		waitingMetric: "vllm:num_requests_waiting",
	},
	workercore.ModelDeploymentEngineSGLang: {
		runningMetric: "sglang:num_running_reqs",
		waitingMetric: "sglang:num_queue_reqs",
		optionalWaitingMetric: []string{
			"sglang:num_prefill_bootstrap_queue_reqs",
			"sglang:num_prefill_inflight_queue_reqs",
			"sglang:num_decode_prealloc_queue_reqs",
			"sglang:num_decode_transfer_queue_reqs",
		},
	},
}

// DrainTarget identifies one live Pod incarnation whose Prometheus /metrics endpoint
// should be scraped for in-flight requests, regardless of container restartCount.
type DrainTarget struct {
	Target       Target
	MetricsPath  string
	Engine       string
	PodUID       types.UID
	BootID       string
	RestartCount int32
}

// DrainReading captures one non-blocking in-flight request snapshot bound to (PodUID, BootID).
type DrainReading struct {
	PodUID       types.UID
	BootID       string
	RestartCount int32
	Running      float64
	Waiting      float64
	Drained      bool
	ObservedAt   time.Time
}

// DrainReaderOption configures a DrainReader.
type DrainReaderOption func(*DrainReader)

// WithDrainTimeout overrides the per-scrape HTTP timeout when positive.
func WithDrainTimeout(d time.Duration) DrainReaderOption {
	return func(r *DrainReader) {
		if d > 0 {
			r.client.timeout = d
		}
	}
}

// WithDrainMaxResponseBytes overrides the response body byte bound when positive.
func WithDrainMaxResponseBytes(n int64) DrainReaderOption {
	return func(r *DrainReader) {
		if n > 0 {
			r.client.maxResponseBytes = n
		}
	}
}

// WithDrainClock overrides the timestamp clock used for ObservedAt.
func WithDrainClock(now func() time.Time) DrainReaderOption {
	return func(r *DrainReader) {
		if now != nil {
			r.now = now
		}
	}
}

// DrainReader scrapes engine Prometheus /metrics endpoints over bounded HTTP/HTTPS calls
// to measure in-flight requests for a specific (PodUID, BootID) incarnation.
type DrainReader struct {
	client *Client
	now    func() time.Time
}

// NewDrainReader constructs a DrainReader with safe timeout and redirect bounds.
func NewDrainReader(opts ...DrainReaderOption) *DrainReader {
	r := &DrainReader{
		client: NewClient(),
		now:    time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// Read scrapes the Pod's /metrics endpoint once and returns an incarnation-bound DrainReading.
func (r *DrainReader) Read(ctx context.Context, target DrainTarget) (*DrainReading, error) {
	if target.PodUID == "" {
		return nil, fmt.Errorf("%w: podUID is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(target.BootID) == "" {
		return nil, fmt.Errorf("%w: bootID is required", ErrInvalidArgument)
	}
	spec, ok := drainMetricSpecs[target.Engine]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported engine %q", ErrInvalidArgument, target.Engine)
	}

	base, err := target.Target.baseURL()
	if err != nil {
		return nil, err
	}
	metricsPath := strings.TrimSpace(target.MetricsPath)
	if metricsPath == "" {
		metricsPath = DefaultMetricsPath
	}
	if !strings.HasPrefix(metricsPath, "/") {
		metricsPath = "/" + metricsPath
	}

	callCtx, cancel := context.WithTimeout(ctx, r.client.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, base+metricsPath, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build metrics request: %w", ErrInvalidArgument, err)
	}
	req.Header.Set("Accept", "text/plain; version=0.0.4, */*;q=0.1")

	httpClient, cleanup := r.client.httpClientFor(target.Target)
	defer cleanup()

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape metrics %s: %w", metricsPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, fmt.Errorf("%w: GET %s returned status %d", ErrRedirectRefused, metricsPath, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{
			Method:     http.MethodGet,
			Path:       metricsPath,
			StatusCode: resp.StatusCode,
		}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, r.client.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read metrics response: %w", err)
	}
	if int64(len(raw)) > r.client.maxResponseBytes {
		return nil, fmt.Errorf("%w: metrics exceeded %d bytes", ErrResponseTooLarge, r.client.maxResponseBytes)
	}

	running, waiting, err := parseInFlightMetrics(string(raw), spec)
	if err != nil {
		return nil, err
	}

	return &DrainReading{
		PodUID:       target.PodUID,
		BootID:       target.BootID,
		RestartCount: target.RestartCount,
		Running:      running,
		Waiting:      waiting,
		Drained:      running == 0 && waiting == 0,
		ObservedAt:   r.now().UTC(),
	}, nil
}

func parseInFlightMetrics(body string, spec engineMetricSpec) (float64, float64, error) {
	wanted := make(map[string]struct{}, 2+len(spec.optionalWaitingMetric))
	wanted[spec.runningMetric] = struct{}{}
	wanted[spec.waitingMetric] = struct{}{}
	for _, opt := range spec.optionalWaitingMetric {
		wanted[opt] = struct{}{}
	}

	totals := make(map[string]float64, len(wanted))
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, valueStr, ok := splitPrometheusSample(line)
		if !ok {
			continue
		}
		if _, match := wanted[name]; !match {
			continue
		}
		if valueStr == "" {
			return 0, 0, fmt.Errorf("metric %q missing sample value", name)
		}
		val, err := strconv.ParseFloat(valueStr, 64)
		if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val < 0 {
			return 0, 0, fmt.Errorf("metric %q reported invalid sample %q", name, valueStr)
		}
		sum := totals[name] + val
		if math.IsNaN(sum) || math.IsInf(sum, 0) {
			return 0, 0, fmt.Errorf("metric %q overflowed", name)
		}
		totals[name] = sum
	}

	running, hasRunning := totals[spec.runningMetric]
	if !hasRunning {
		return 0, 0, fmt.Errorf("metrics output missing required gauge %q", spec.runningMetric)
	}
	waiting, hasWaiting := totals[spec.waitingMetric]
	if !hasWaiting {
		return 0, 0, fmt.Errorf("metrics output missing required gauge %q", spec.waitingMetric)
	}
	for _, opt := range spec.optionalWaitingMetric {
		if v, present := totals[opt]; present {
			waiting += v
			if math.IsNaN(waiting) || math.IsInf(waiting, 0) {
				return 0, 0, fmt.Errorf("waiting metrics total overflowed")
			}
		}
	}
	return running, waiting, nil
}

func splitPrometheusSample(line string) (string, string, bool) {
	idx := strings.IndexAny(line, "{ \t")
	if idx <= 0 {
		return "", "", false
	}
	name := line[:idx]
	rest := line[idx:]
	if strings.HasPrefix(rest, "{") {
		closeIdx := findClosingBrace(rest)
		if closeIdx < 0 {
			return name, "", true
		}
		rest = rest[closeIdx+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return name, "", true
	}
	return name, fields[0], true
}

func findClosingBrace(s string) int {
	inQuote := false
	escaped := false
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inQuote {
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inQuote = false
			}
			continue
		}
		switch ch {
		case '"':
			inQuote = true
		case '}':
			return i
		}
	}
	return -1
}
