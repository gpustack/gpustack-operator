package runtimecontrol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	// MinProbeSteps is the lower bound enforced by the engine sentinel on short-window probes.
	MinProbeSteps = 3
	// MaxProbeSteps is the upper bound enforced by the engine sentinel on short-window probes.
	MaxProbeSteps = 5
	// DefaultProbeSteps is the default step count used when ProbeRequest.Steps is omitted.
	DefaultProbeSteps = 4

	// ProbeStateIdle indicates no probe has run or is currently active.
	ProbeStateIdle = "idle"
	// ProbeStateRunning indicates a short-window probe is currently capturing or distilling.
	ProbeStateRunning = "running"
	// ProbeStateCompleted indicates the most recent probe finished and populated Payload.
	ProbeStateCompleted = "completed"
	// ProbeStateFailed indicates the most recent probe failed and populated Error/ErrorCode.
	ProbeStateFailed = "failed"
	// ProbeStateAborted indicates the most recent probe was aborted via DELETE /gpustack/v1/probe.
	ProbeStateAborted = "aborted"
)

var (
	// ErrRolePaused reports HTTP 409 role_paused when probing is refused on a paused role.
	ErrRolePaused = errors.New("role paused")
	// ErrProbeInProgress reports HTTP 409 probe_in_progress when another probe is already running.
	ErrProbeInProgress = errors.New("probe in progress")
	// ErrUnsupportedEngineVersion reports HTTP 422 unsupported_engine_version when the running
	// engine lacks the probe capability hook.
	ErrUnsupportedEngineVersion = errors.New("unsupported engine version")
	// ErrProbeCooldown reports HTTP 429 probe_cooldown when the sentinel cooldown has not elapsed.
	ErrProbeCooldown = errors.New("probe cooldown")
)

// ProbeInProgressError reports HTTP 409 probe_in_progress from POST /gpustack/v1/probe.
type ProbeInProgressError struct {
	BootID  string
	ProbeID string
	Message string
}

func (e *ProbeInProgressError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.ProbeID != "" {
		return fmt.Sprintf("probe %s is already in progress", e.ProbeID)
	}
	return ErrProbeInProgress.Error()
}

func (e *ProbeInProgressError) Unwrap() error {
	return ErrProbeInProgress
}

// UnsupportedEngineVersionError reports HTTP 422 unsupported_engine_version from /gpustack/v1/probe.
type UnsupportedEngineVersionError struct {
	BootID  string
	Message string
}

func (e *UnsupportedEngineVersionError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return ErrUnsupportedEngineVersion.Error()
}

func (e *UnsupportedEngineVersionError) Unwrap() error {
	return ErrUnsupportedEngineVersion
}

// ProbeCooldownError reports HTTP 429 probe_cooldown from POST /gpustack/v1/probe.
type ProbeCooldownError struct {
	BootID     string
	RetryAfter time.Duration
	Cooldown   time.Duration
	Message    string
}

func (e *ProbeCooldownError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("probe cooldown active; retry after %s", e.RetryAfter)
	}
	return ErrProbeCooldown.Error()
}

func (e *ProbeCooldownError) Unwrap() error {
	return ErrProbeCooldown
}

// ProbeRequest matches POST /gpustack/v1/probe request bodies.
type ProbeRequest struct {
	ExpectedBootID string         `json:"expected_boot_id"`
	Steps          int            `json:"steps,omitempty"`
	EngineVersion  string         `json:"engine_version,omitempty"`
	Manufacturer   string         `json:"manufacturer,omitempty"`
	Architecture   string         `json:"architecture,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// ProbeAxisSpec describes one FlashInfer Trace axis in a distilled ProbeDefinition.
type ProbeAxisSpec struct {
	Type  string `json:"type"`
	Value *int64 `json:"value,omitempty"`
}

// ProbeTensorSpec describes one input or output tensor in a distilled ProbeDefinition.
type ProbeTensorSpec struct {
	Shape []string `json:"shape"`
	Dtype string   `json:"dtype"`
}

// ProbeDefinition matches one distilled FlashInfer Trace Definition emitted by sentinel.
type ProbeDefinition struct {
	Name           string                     `json:"name"`
	OpType         string                     `json:"op_type"`
	LocalKernelKey string                     `json:"local_kernel_key,omitempty"`
	Axes           map[string]ProbeAxisSpec   `json:"axes"`
	Inputs         map[string]ProbeTensorSpec `json:"inputs"`
	Outputs        map[string]ProbeTensorSpec `json:"outputs"`
	Reference      string                     `json:"reference"`
	Tags           []string                   `json:"tags,omitempty"`
}

// ProbeWorkloadInput describes one input generator entry in a distilled ProbeWorkload.
type ProbeWorkloadInput struct {
	Type  string `json:"type"`
	Value any    `json:"value,omitempty"`
}

// ProbeWorkload matches one distilled FlashInfer Trace Workload emitted by sentinel.
type ProbeWorkload struct {
	UUID               string                        `json:"uuid"`
	Definition         string                        `json:"definition,omitempty"`
	LocalKernelKey     string                        `json:"local_kernel_key,omitempty"`
	Axes               map[string]int64              `json:"axes"`
	Inputs             map[string]ProbeWorkloadInput `json:"inputs"`
	ObservedDurationUs float64                       `json:"observed_duration_us,omitempty"`
	CallCount          int64                         `json:"call_count,omitempty"`
}

// ProbeBottleneck summarizes one ranked single-GPU non-GEMM operator bottleneck.
type ProbeBottleneck struct {
	LocalKernelKey      string  `json:"local_kernel_key"`
	OpFamily            string  `json:"op_family"`
	GPUTimeUs           float64 `json:"gpu_time_us"`
	GPUTimeSharePercent float64 `json:"gpu_time_share_percent"`
	CallCount           int64   `json:"call_count"`
}

// ProbeHotspot records one CUDA-graph kernel launch hotspot observed during probing.
type ProbeHotspot struct {
	KernelName      string  `json:"kernel_name"`
	TotalDurationUs float64 `json:"total_duration_us"`
	CallCount       int64   `json:"call_count"`
}

// ProbeDiagnostics captures bounded probe capture metadata and safety counters.
type ProbeDiagnostics struct {
	StepsCaptured         int            `json:"steps_captured,omitempty"`
	FilteredExcludedCount int            `json:"filtered_excluded_count,omitempty"`
	UnclassifiedCount     int            `json:"unclassified_count,omitempty"`
	RawBytesStaged        int64          `json:"raw_bytes_staged,omitempty"`
	WallClockSeconds      float64        `json:"wall_clock_seconds,omitempty"`
	MaxShmBytes           int64          `json:"max_shm_bytes,omitempty"`
	WallClockCapSeconds   float64        `json:"wall_clock_cap_seconds,omitempty"`
	CUDAGraphHotspots     []ProbeHotspot `json:"cuda_graph_hotspots,omitempty"`
	Truncated             bool           `json:"truncated,omitempty"`
}

// ProbePayload carries the distilled FlashInfer Trace definitions, workloads, and bottlenecks.
type ProbePayload struct {
	SchemaVersion string            `json:"schema_version"`
	Engine        string            `json:"engine"`
	EngineVersion string            `json:"engine_version"`
	Manufacturer  string            `json:"manufacturer"`
	Architecture  string            `json:"architecture"`
	Definitions   []ProbeDefinition `json:"definitions"`
	Workloads     []ProbeWorkload   `json:"workloads"`
	Bottlenecks   []ProbeBottleneck `json:"bottlenecks"`
	Diagnostics   ProbeDiagnostics  `json:"diagnostics"`
}

// ProbeResponse matches GET, POST, and DELETE /gpustack/v1/probe responses.
type ProbeResponse struct {
	BootID      string           `json:"boot_id"`
	State       string           `json:"state"`
	ProbeID     string           `json:"probe_id,omitempty"`
	Steps       int              `json:"steps,omitempty"`
	Error       string           `json:"error,omitempty"`
	ErrorCode   string           `json:"error_code,omitempty"`
	Diagnostics ProbeDiagnostics `json:"diagnostics,omitempty"`
	Payload     *ProbePayload    `json:"payload,omitempty"`
}

type probeErrorEnvelope struct {
	Error             string   `json:"error"`
	Message           string   `json:"message"`
	BootID            string   `json:"boot_id"`
	ExpectedBootID    string   `json:"expected_boot_id"`
	ProbeID           string   `json:"probe_id"`
	RetryAfterSeconds *float64 `json:"retry_after_seconds"`
	CooldownSeconds   *float64 `json:"cooldown_seconds"`
}

func decodeProbeError(method, path string, resp *rawResponse) error {
	var env probeErrorEnvelope
	_ = json.Unmarshal(resp.Body, &env)

	switch resp.StatusCode {
	case http.StatusConflict:
		switch env.Error {
		case "role_paused":
			if env.Message != "" {
				return fmt.Errorf("%w: %s", ErrRolePaused, env.Message)
			}
			return fmt.Errorf("%w: %s %s returned 409", ErrRolePaused, method, path)
		case "probe_in_progress":
			return &ProbeInProgressError{
				BootID:  env.BootID,
				ProbeID: env.ProbeID,
				Message: env.Message,
			}
		case "boot_id_mismatch":
			if env.BootID != "" || env.ExpectedBootID != "" {
				return fmt.Errorf("%w: current=%q expected=%q", ErrBootIDMismatch, env.BootID, env.ExpectedBootID)
			}
			return fmt.Errorf("%w: %s %s returned 409", ErrBootIDMismatch, method, path)
		default:
			if env.Error == "" && (env.BootID != "" || env.ExpectedBootID != "") {
				return fmt.Errorf("%w: current=%q expected=%q", ErrBootIDMismatch, env.BootID, env.ExpectedBootID)
			}
			return &HTTPStatusError{
				Method:     method,
				Path:       path,
				StatusCode: resp.StatusCode,
				ErrorCode:  env.Error,
				Message:    env.Message,
			}
		}
	case http.StatusUnprocessableEntity:
		if env.Error == "unsupported_engine_version" {
			return &UnsupportedEngineVersionError{
				BootID:  env.BootID,
				Message: env.Message,
			}
		}
		return decodeAPIError(method, path, resp)
	case http.StatusTooManyRequests:
		if env.Error != "" && env.Error != "probe_cooldown" {
			return decodeAPIError(method, path, resp)
		}
		retryAfter := parseProbeRetryAfter(resp.Headers, env.RetryAfterSeconds)
		var cooldown time.Duration
		if env.CooldownSeconds != nil && !math.IsNaN(*env.CooldownSeconds) && !math.IsInf(*env.CooldownSeconds, 0) && *env.CooldownSeconds > 0 {
			cooldown = time.Duration(*env.CooldownSeconds * float64(time.Second))
		}
		return &ProbeCooldownError{
			BootID:     env.BootID,
			RetryAfter: retryAfter,
			Cooldown:   cooldown,
			Message:    env.Message,
		}
	default:
		return decodeAPIError(method, path, resp)
	}
}

func parseProbeRetryAfter(headers http.Header, bodySeconds *float64) time.Duration {
	if bodySeconds != nil && !math.IsNaN(*bodySeconds) && !math.IsInf(*bodySeconds, 0) && *bodySeconds > 0 {
		return time.Duration(*bodySeconds * float64(time.Second))
	}
	if headers == nil {
		return 0
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(secs) && !math.IsInf(secs, 0) && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// StartProbe sends POST /gpustack/v1/probe to arm a bounded short-window probe on the sentinel.
func (c *Client) StartProbe(ctx context.Context, target Target, req ProbeRequest) (*ProbeResponse, error) {
	req.ExpectedBootID = strings.TrimSpace(req.ExpectedBootID)
	if req.ExpectedBootID == "" {
		return nil, fmt.Errorf("%w: expected_boot_id is required", ErrInvalidArgument)
	}
	if req.Steps == 0 {
		req.Steps = DefaultProbeSteps
	}
	if req.Steps < MinProbeSteps || req.Steps > MaxProbeSteps {
		return nil, fmt.Errorf(
			"%w: steps %d must be between %d and %d",
			ErrInvalidArgument, req.Steps, MinProbeSteps, MaxProbeSteps,
		)
	}

	resp, err := c.do(ctx, target, http.MethodPost, PathProbe, req, true, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, decodeProbeError(http.MethodPost, PathProbe, resp)
	}

	var out ProbeResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, wrapAmbiguous(http.MethodPost+" "+PathProbe, "", fmt.Errorf("decode %s response: %w", PathProbe, err))
	}
	if out.BootID != "" && out.BootID != req.ExpectedBootID {
		return nil, fmt.Errorf("%w: response boot_id=%q expected=%q", ErrBootIDMismatch, out.BootID, req.ExpectedBootID)
	}
	return &out, nil
}

// GetProbe queries GET /gpustack/v1/probe for current probe state and distilled FlashInfer payloads.
func (c *Client) GetProbe(ctx context.Context, target Target) (*ProbeResponse, error) {
	resp, err := c.do(ctx, target, http.MethodGet, PathProbe, nil, false, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, decodeProbeError(http.MethodGet, PathProbe, resp)
	}

	var out ProbeResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", PathProbe, err)
	}
	return &out, nil
}

// DeleteProbe sends DELETE /gpustack/v1/probe to abort an active probe and purge staged traces.
func (c *Client) DeleteProbe(ctx context.Context, target Target) (*ProbeResponse, error) {
	resp, err := c.do(ctx, target, http.MethodDelete, PathProbe, nil, true, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return &ProbeResponse{State: ProbeStateAborted}, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, decodeProbeError(http.MethodDelete, PathProbe, resp)
	}

	var out ProbeResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, wrapAmbiguous(http.MethodDelete+" "+PathProbe, "", fmt.Errorf("decode %s response: %w", PathProbe, err))
	}
	return &out, nil
}
