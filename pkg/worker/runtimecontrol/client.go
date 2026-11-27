package runtimecontrol

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	// DefaultRequestTimeout bounds every control-plane HTTP call.
	DefaultRequestTimeout = 5 * time.Second
	// DefaultMaxResponseBytes bounds every HTTP response body read.
	DefaultMaxResponseBytes int64 = 1 << 20

	// PathReady is the engine sentinel readiness probe route.
	PathReady = "/gpustack/v1/ready"
	// PathLive is the engine sentinel liveness probe route.
	PathLive = "/gpustack/v1/live"
	// PathAck is the engine sentinel boot-latch acknowledgement route.
	PathAck = "/gpustack/v1/ack"
	// PathPause is the engine sentinel pause inspection and control route.
	PathPause = "/gpustack/v1/pause"
	// PathRuntimeConfig is the engine sentinel runtime-config inspection and apply route.
	PathRuntimeConfig = "/gpustack/v1/runtime_config"
	// PathProbe is the engine sentinel short-window profiler probe route.
	PathProbe = "/gpustack/v1/probe"
)

var (
	// ErrBootIDMismatch reports HTTP 409 when a fenced request targets a stale boot_id.
	ErrBootIDMismatch = errors.New("boot_id mismatch")
	// ErrInvalidArgument reports HTTP 400 or a rejected client input.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrRedirectRefused reports that the engine answered with an HTTP redirect.
	ErrRedirectRefused = errors.New("http redirect refused")
	// ErrResponseTooLarge reports that a response exceeded the configured byte bound.
	ErrResponseTooLarge = errors.New("response body exceeded limit")
	// ErrRankApplyFailed reports that one or more engine ranks failed to converge.
	ErrRankApplyFailed = errors.New("rank apply failed")
)

// UnsupportedFieldsError reports HTTP 422 when an engine does not support requested fields.
type UnsupportedFieldsError struct {
	UnsupportedFields []string
	SupportedFields   []string
	BootID            string
	Message           string
}

func (e *UnsupportedFieldsError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("unsupported runtime control fields: %s", strings.Join(e.UnsupportedFields, ", "))
}

// AmbiguousOperationError reports a transport or timeout failure on a mutating POST whose
// execution state on the engine cannot be determined without reading live state first.
type AmbiguousOperationError struct {
	Operation   string
	OperationID string
	Reason      string
	Err         error
}

func (e *AmbiguousOperationError) Error() string {
	if e.OperationID != "" {
		return fmt.Sprintf("ambiguous operation %s (%s): %s", e.Operation, e.OperationID, e.Reason)
	}
	return fmt.Sprintf("ambiguous operation %s: %s", e.Operation, e.Reason)
}

func (e *AmbiguousOperationError) Unwrap() error {
	return e.Err
}

// IsAmbiguousOperation reports whether err is or wraps an AmbiguousOperationError.
func IsAmbiguousOperation(err error) bool {
	var target *AmbiguousOperationError
	return errors.As(err, &target)
}

// HTTPStatusError reports an unexpected non-2xx HTTP response status from an engine.
type HTTPStatusError struct {
	Method     string
	Path       string
	StatusCode int
	ErrorCode  string
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s %s returned status %d (%s): %s", e.Method, e.Path, e.StatusCode, e.ErrorCode, e.Message)
	}
	return fmt.Sprintf("%s %s returned status %d", e.Method, e.Path, e.StatusCode)
}

// Target identifies one answering engine sentinel HTTP endpoint.
type Target struct {
	Scheme             string
	Host               string
	Port               int
	TLSConfig          *tls.Config
	InsecureSkipVerify bool
	ProbeAuthToken     string
}

func (t Target) baseURL() (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(t.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: unsupported scheme %q", ErrInvalidArgument, t.Scheme)
	}
	host := strings.TrimSpace(t.Host)
	if host == "" {
		return "", fmt.Errorf("%w: target host is empty", ErrInvalidArgument)
	}
	if t.Port <= 0 || t.Port > 65535 {
		return "", fmt.Errorf("%w: target port %d is out of range", ErrInvalidArgument, t.Port)
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(t.Port)), nil
}

// ReadyResponse matches GET /gpustack/v1/ready and POST /gpustack/v1/ack responses.
type ReadyResponse struct {
	Ready                  bool     `json:"ready"`
	BootID                 string   `json:"boot_id"`
	ObservedGeneration     *int64   `json:"observed_generation"`
	AppliedConfigDigest    string   `json:"applied_config_digest"`
	Reason                 string   `json:"reason,omitempty"`
	LoadedKernelKeys       []string `json:"loaded_kernel_keys,omitempty"`
	DispatchManifestDigest string   `json:"dispatch_manifest_digest,omitempty"`
	KernelLoadError        string   `json:"kernel_load_error,omitempty"`
}

// LiveResponse matches GET /gpustack/v1/live responses.
type LiveResponse struct {
	Alive  bool   `json:"alive"`
	BootID string `json:"boot_id"`
	Paused bool   `json:"paused"`
	Error  string `json:"error,omitempty"`
}

// AckRequest matches POST /gpustack/v1/ack request bodies.
type AckRequest struct {
	BootID              string `json:"boot_id"`
	ObservedGeneration  int64  `json:"observed_generation"`
	AppliedConfigDigest string `json:"applied_config_digest"`
}

// PauseRequest matches POST /gpustack/v1/pause request bodies.
type PauseRequest struct {
	Paused         bool   `json:"paused"`
	ExpectedBootID string `json:"expected_boot_id,omitempty"`
}

// PauseResponse matches GET and POST /gpustack/v1/pause responses.
type PauseResponse struct {
	Paused bool   `json:"paused"`
	BootID string `json:"boot_id"`
}

// RankReceipt captures per-rank convergence metadata returned by /gpustack/v1/runtime_config.
type RankReceipt struct {
	RankID                 int            `json:"rank_id"`
	BootID                 string         `json:"boot_id"`
	MembershipEpoch        int64          `json:"membership_epoch"`
	AppliedDigest          string         `json:"applied_digest"`
	Values                 map[string]any `json:"values,omitempty"`
	LoadedKernelKeys       []string       `json:"loaded_kernel_keys,omitempty"`
	DispatchManifestDigest string         `json:"dispatch_manifest_digest,omitempty"`
	KernelLoadError        string         `json:"kernel_load_error,omitempty"`
}

// RuntimeConfigState matches GET and POST /gpustack/v1/runtime_config responses.
type RuntimeConfigState struct {
	Engine                 string         `json:"engine"`
	BootID                 string         `json:"boot_id"`
	MembershipEpoch        int64          `json:"membership_epoch"`
	ObservedGeneration     *int64         `json:"observed_generation"`
	AppliedConfigDigest    string         `json:"applied_config_digest"`
	LastOperationID        string         `json:"last_operation_id"`
	Ready                  bool           `json:"ready"`
	Paused                 bool           `json:"paused"`
	SupportedFields        []string       `json:"supported_fields"`
	UnsupportedFields      []string       `json:"unsupported_fields"`
	BootDefaults           map[string]any `json:"boot_defaults"`
	Ranks                  []RankReceipt  `json:"ranks"`
	Values                 map[string]any `json:"values"`
	LoadedKernelKeys       []string       `json:"loaded_kernel_keys,omitempty"`
	DispatchManifestDigest string         `json:"dispatch_manifest_digest,omitempty"`
	KernelLoadError        string         `json:"kernel_load_error,omitempty"`
}

// ApplyRuntimeConfigRequest matches POST /gpustack/v1/runtime_config request bodies.
type ApplyRuntimeConfigRequest struct {
	OperationID         string         `json:"operation_id"`
	ExpectedBootID      string         `json:"expected_boot_id"`
	Generation          int64          `json:"generation"`
	AppliedConfigDigest string         `json:"applied_config_digest"`
	Parameters          map[string]any `json:"parameters,omitempty"`
	ResetFields         []string       `json:"reset_fields,omitempty"`
}

type apiErrorEnvelope struct {
	Error             string   `json:"error"`
	Message           string   `json:"message"`
	BootID            string   `json:"boot_id"`
	ExpectedBootID    string   `json:"expected_boot_id"`
	UnsupportedFields []string `json:"unsupported_fields"`
	SupportedFields   []string `json:"supported_fields"`
}

// ClientOption configures a runtime-control HTTP Client.
type ClientOption func(*Client)

// WithTimeout overrides the per-call HTTP timeout when positive.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithMaxResponseBytes overrides the response body byte cap when positive.
func WithMaxResponseBytes(n int64) ClientOption {
	return func(c *Client) {
		if n > 0 {
			c.maxResponseBytes = n
		}
	}
}

// Client is a bounded-timeout, no-redirect, TLS-aware HTTP client for the engine sentinel.
type Client struct {
	timeout           time.Duration
	maxResponseBytes  int64
	baseTransport     *http.Transport
	insecureTransport *http.Transport
}

// NewClient constructs an immutable Client with safe defaults.
func NewClient(opts ...ClientOption) *Client {
	base := defaultBaseTransport()
	insecure := base.Clone()
	// #nosec G402 -- explicit caller opt-in for cluster-internal self-signed engine TLS.
	insecure.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
	c := &Client{
		timeout:           DefaultRequestTimeout,
		maxResponseBytes:  DefaultMaxResponseBytes,
		baseTransport:     base,
		insecureTransport: insecure,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

func defaultBaseTransport() *http.Transport {
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		return def.Clone()
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func (c *Client) httpClientFor(target Target) (*http.Client, func()) {
	transport := c.baseTransport
	cleanup := func() {}

	if target.TLSConfig == nil && target.InsecureSkipVerify {
		transport = c.insecureTransport
	} else if target.TLSConfig != nil {
		cloned := c.baseTransport.Clone()
		tlsCfg := target.TLSConfig.Clone()
		if target.InsecureSkipVerify {
			// #nosec G402 -- explicit caller opt-in for cluster-internal self-signed engine TLS.
			tlsCfg.InsecureSkipVerify = true
		}
		cloned.TLSClientConfig = tlsCfg
		transport = cloned
		cleanup = cloned.CloseIdleConnections
	}

	httpClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return httpClient, cleanup
}

type rawResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func (c *Client) do(
	ctx context.Context,
	target Target,
	method string,
	pathWithQuery string,
	payload any,
	mutating bool,
	operationID string,
) (*rawResponse, error) {
	base, err := target.baseURL()
	if err != nil {
		return nil, err
	}

	var body io.Reader
	if payload != nil {
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, fmt.Errorf("%w: encode request body: %w", ErrInvalidArgument, marshalErr)
		}
		body = bytes.NewReader(encoded)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, method, base+pathWithQuery, body)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrInvalidArgument, err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if pathWithQuery == PathProbe && strings.TrimSpace(target.ProbeAuthToken) != "" {
		req.Header.Set("X-GPUStack-Probe-Token", strings.TrimSpace(target.ProbeAuthToken))
	}

	httpClient, cleanup := c.httpClientFor(target)
	defer cleanup()

	resp, err := httpClient.Do(req)
	if err != nil {
		if mutating {
			return nil, wrapAmbiguous(method+" "+pathWithQuery, operationID, err)
		}
		return nil, fmt.Errorf("%s %s: %w", method, pathWithQuery, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, fmt.Errorf("%w: %s %s returned status %d", ErrRedirectRefused, method, pathWithQuery, resp.StatusCode)
	}

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if readErr != nil {
		if mutating {
			return nil, wrapAmbiguous(method+" "+pathWithQuery, operationID, readErr)
		}
		return nil, fmt.Errorf("read %s %s response: %w", method, pathWithQuery, readErr)
	}
	if int64(len(raw)) > c.maxResponseBytes {
		return nil, fmt.Errorf("%w: %s %s exceeded %d bytes", ErrResponseTooLarge, method, pathWithQuery, c.maxResponseBytes)
	}

	return &rawResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: raw}, nil
}

func wrapAmbiguous(op, operationID string, err error) *AmbiguousOperationError {
	reason := "transport failed before confirmation"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "request timed out before confirmation"
	case errors.Is(err, context.Canceled):
		reason = "context canceled before confirmation"
	}
	return &AmbiguousOperationError{
		Operation:   op,
		OperationID: operationID,
		Reason:      reason,
		Err:         err,
	}
}

func decodeAPIError(method, path string, resp *rawResponse) error {
	var env apiErrorEnvelope
	_ = json.Unmarshal(resp.Body, &env)

	switch resp.StatusCode {
	case http.StatusBadRequest:
		if env.Message != "" {
			return fmt.Errorf("%w: %s", ErrInvalidArgument, env.Message)
		}
		return fmt.Errorf("%w: %s %s returned 400", ErrInvalidArgument, method, path)
	case http.StatusConflict:
		if env.BootID != "" || env.ExpectedBootID != "" {
			return fmt.Errorf("%w: current=%q expected=%q", ErrBootIDMismatch, env.BootID, env.ExpectedBootID)
		}
		return fmt.Errorf("%w: %s %s returned 409", ErrBootIDMismatch, method, path)
	case http.StatusUnprocessableEntity:
		unsupported := append([]string(nil), env.UnsupportedFields...)
		supported := append([]string(nil), env.SupportedFields...)
		sort.Strings(unsupported)
		sort.Strings(supported)
		return &UnsupportedFieldsError{
			UnsupportedFields: unsupported,
			SupportedFields:   supported,
			BootID:            env.BootID,
			Message:           env.Message,
		}
	default:
		return &HTTPStatusError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			ErrorCode:  env.Error,
			Message:    env.Message,
		}
	}
}

// GetReady queries GET /gpustack/v1/ready.
// Both 200 and 503 responses carry structured ReadyResponse JSON.
func (c *Client) GetReady(ctx context.Context, target Target) (*ReadyResponse, error) {
	resp, err := c.do(ctx, target, http.MethodGet, PathReady, nil, false, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, decodeAPIError(http.MethodGet, PathReady, resp)
	}
	var out ReadyResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", PathReady, err)
	}
	return &out, nil
}

// GetLive queries GET /gpustack/v1/live.
// Both 200 and 503 responses carry structured LiveResponse JSON.
func (c *Client) GetLive(ctx context.Context, target Target) (*LiveResponse, error) {
	resp, err := c.do(ctx, target, http.MethodGet, PathLive, nil, false, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, decodeAPIError(http.MethodGet, PathLive, resp)
	}
	var out LiveResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", PathLive, err)
	}
	return &out, nil
}

// Ack sends POST /gpustack/v1/ack to confirm boot_id convergence and open the readiness latch.
func (c *Client) Ack(ctx context.Context, target Target, req AckRequest) (*ReadyResponse, error) {
	if strings.TrimSpace(req.BootID) == "" {
		return nil, fmt.Errorf("%w: boot_id is required", ErrInvalidArgument)
	}
	resp, err := c.do(ctx, target, http.MethodPost, PathAck, req, true, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(http.MethodPost, PathAck, resp)
	}
	var out ReadyResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, wrapAmbiguous(http.MethodPost+" "+PathAck, "", fmt.Errorf("decode %s response: %w", PathAck, err))
	}
	if out.BootID != req.BootID {
		return nil, fmt.Errorf("%w: ack response boot_id=%q expected=%q", ErrBootIDMismatch, out.BootID, req.BootID)
	}
	if req.ObservedGeneration > 0 && (out.ObservedGeneration == nil || *out.ObservedGeneration != req.ObservedGeneration) {
		return nil, fmt.Errorf(
			"%w: ack response observed_generation=%v expected=%d",
			ErrRankApplyFailed, out.ObservedGeneration, req.ObservedGeneration,
		)
	}
	if req.AppliedConfigDigest != "" && out.AppliedConfigDigest != req.AppliedConfigDigest {
		return nil, fmt.Errorf(
			"%w: ack response applied_config_digest=%q expected=%q",
			ErrRankApplyFailed, out.AppliedConfigDigest, req.AppliedConfigDigest,
		)
	}
	return &out, nil
}

// GetPause queries GET /gpustack/v1/pause.
func (c *Client) GetPause(ctx context.Context, target Target) (*PauseResponse, error) {
	resp, err := c.do(ctx, target, http.MethodGet, PathPause, nil, false, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(http.MethodGet, PathPause, resp)
	}
	var out PauseResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", PathPause, err)
	}
	return &out, nil
}

// SetPause sends POST /gpustack/v1/pause to toggle ASGI pause gating on the engine sentinel.
func (c *Client) SetPause(ctx context.Context, target Target, req PauseRequest) (*PauseResponse, error) {
	req.ExpectedBootID = strings.TrimSpace(req.ExpectedBootID)
	resp, err := c.do(ctx, target, http.MethodPost, PathPause, req, true, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(http.MethodPost, PathPause, resp)
	}
	var out PauseResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, wrapAmbiguous(http.MethodPost+" "+PathPause, "", fmt.Errorf("decode %s response: %w", PathPause, err))
	}
	if req.ExpectedBootID != "" && out.BootID != req.ExpectedBootID {
		return nil, fmt.Errorf("%w: response boot_id=%q expected=%q", ErrBootIDMismatch, out.BootID, req.ExpectedBootID)
	}
	return &out, nil
}

// GetRuntimeConfig queries GET /gpustack/v1/runtime_config.
func (c *Client) GetRuntimeConfig(ctx context.Context, target Target) (*RuntimeConfigState, error) {
	resp, err := c.do(ctx, target, http.MethodGet, PathRuntimeConfig, nil, false, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(http.MethodGet, PathRuntimeConfig, resp)
	}
	var out RuntimeConfigState
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", PathRuntimeConfig, err)
	}
	return &out, nil
}

// ApplyRuntimeConfig sends POST /gpustack/v1/runtime_config and verifies every per-rank receipt.
func (c *Client) ApplyRuntimeConfig(
	ctx context.Context, target Target, req ApplyRuntimeConfigRequest,
) (*RuntimeConfigState, error) {
	if strings.TrimSpace(req.OperationID) == "" {
		return nil, fmt.Errorf("%w: operation_id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(req.ExpectedBootID) == "" {
		return nil, fmt.Errorf("%w: expected_boot_id is required", ErrInvalidArgument)
	}

	resp, err := c.do(ctx, target, http.MethodPost, PathRuntimeConfig, req, true, req.OperationID)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(http.MethodPost, PathRuntimeConfig, resp)
	}

	var out RuntimeConfigState
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, wrapAmbiguous(
			http.MethodPost+" "+PathRuntimeConfig, req.OperationID,
			fmt.Errorf("decode %s response: %w", PathRuntimeConfig, err),
		)
	}
	if err := verifyRankReceipts(&out, req.ExpectedBootID, req.AppliedConfigDigest); err != nil {
		return nil, err
	}
	return &out, nil
}

func verifyRankReceipts(state *RuntimeConfigState, expectedBootID, expectedDigest string) error {
	if expectedBootID != "" && state.BootID != expectedBootID {
		return fmt.Errorf("%w: response boot_id=%q expected=%q", ErrBootIDMismatch, state.BootID, expectedBootID)
	}
	if len(state.Ranks) == 0 {
		return fmt.Errorf("%w: response carried no rank receipts", ErrRankApplyFailed)
	}
	seenRanks := make(map[int]struct{}, len(state.Ranks))
	for _, rank := range state.Ranks {
		if _, dup := seenRanks[rank.RankID]; dup {
			return fmt.Errorf("%w: duplicate rank %d in response receipts", ErrRankApplyFailed, rank.RankID)
		}
		seenRanks[rank.RankID] = struct{}{}
		if strings.TrimSpace(rank.BootID) == "" {
			return fmt.Errorf("%w: rank %d reported empty boot_id", ErrRankApplyFailed, rank.RankID)
		}
		if expectedDigest != "" && rank.AppliedDigest != expectedDigest {
			return fmt.Errorf(
				"%w: rank %d applied_digest=%q expected=%q",
				ErrRankApplyFailed, rank.RankID, rank.AppliedDigest, expectedDigest,
			)
		}
	}
	return nil
}
