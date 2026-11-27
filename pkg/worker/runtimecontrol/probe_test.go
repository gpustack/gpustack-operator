package runtimecontrol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/utils/json"
)

func TestClient_ProbeLifecycleAndPayloads(t *testing.T) {
	t.Parallel()

	const (
		bootID  = "boot-probe-1"
		probeID = "probe-uuid-1"
		kKey    = "vllm/0.8.5/nvidia/sm_90a/rmsnorm/0123456789abcdef"
		authTok = "shared-probe-secret"
	)

	var (
		gotMethod  string
		gotToken   string
		gotReqBody ProbeRequest
	)

	hidden := int64(4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, PathProbe, r.URL.Path)
		gotMethod = r.Method
		gotToken = r.Header.Get("X-GPUStack-Probe-Token")
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodPost:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReqBody))
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(ProbeResponse{
				BootID:  bootID,
				State:   ProbeStateRunning,
				ProbeID: probeID,
				Steps:   gotReqBody.Steps,
			})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(ProbeResponse{
				BootID:  bootID,
				State:   ProbeStateCompleted,
				ProbeID: probeID,
				Steps:   4,
				Diagnostics: ProbeDiagnostics{
					StepsCaptured:         4,
					FilteredExcludedCount: 2,
					RawBytesStaged:        4096,
					WallClockSeconds:      0.42,
				},
				Payload: &ProbePayload{
					SchemaVersion: "flashinfer-trace/v1",
					Engine:        "vllm",
					EngineVersion: "0.8.5",
					Manufacturer:  "nvidia",
					Architecture:  "sm_90a",
					Definitions: []ProbeDefinition{
						{
							Name:           "rmsnorm_0123456789abcdef",
							OpType:         "rmsnorm",
							LocalKernelKey: kKey,
							Axes: map[string]ProbeAxisSpec{
								"hidden_size": {Type: "const", Value: &hidden},
								"num_tokens":  {Type: "var"},
							},
							Inputs: map[string]ProbeTensorSpec{
								"x":      {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
								"weight": {Shape: []string{"hidden_size"}, Dtype: "bfloat16"},
							},
							Outputs: map[string]ProbeTensorSpec{
								"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
							},
							Reference: "def run(x, weight): return x",
						},
					},
					Workloads: []ProbeWorkload{
						{
							UUID:               "wl-1234",
							Definition:         "rmsnorm_0123456789abcdef",
							LocalKernelKey:     kKey,
							Axes:               map[string]int64{"num_tokens": 128},
							Inputs:             map[string]ProbeWorkloadInput{"x": {Type: "random"}, "weight": {Type: "random"}},
							ObservedDurationUs: 42.5,
							CallCount:          12,
						},
					},
					Bottlenecks: []ProbeBottleneck{
						{
							LocalKernelKey:      kKey,
							OpFamily:            "rmsnorm",
							GPUTimeUs:           42.5,
							GPUTimeSharePercent: 64.2,
							CallCount:           12,
						},
					},
				},
			})
		case http.MethodDelete:
			_ = json.NewEncoder(w).Encode(ProbeResponse{
				BootID:  bootID,
				State:   ProbeStateAborted,
				ProbeID: probeID,
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	client := NewClient()
	target := targetFromServer(t, srv)
	target.ProbeAuthToken = authTok
	ctx := context.Background()

	// Default steps (0 -> 4) accepted with HTTP 202.
	started, err := client.StartProbe(ctx, target, ProbeRequest{
		ExpectedBootID: bootID,
		EngineVersion:  "0.8.5",
		Manufacturer:   "nvidia",
		Architecture:   "sm_90a",
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, authTok, gotToken)
	require.Equal(t, DefaultProbeSteps, gotReqBody.Steps)
	require.Equal(t, ProbeStateRunning, started.State)
	require.Equal(t, probeID, started.ProbeID)

	// GET returns full distilled payload.
	got, err := client.GetProbe(ctx, target)
	require.NoError(t, err)
	require.Equal(t, ProbeStateCompleted, got.State)
	require.NotNil(t, got.Payload)
	require.Len(t, got.Payload.Definitions, 1)
	require.Len(t, got.Payload.Workloads, 1)
	require.Len(t, got.Payload.Bottlenecks, 1)
	require.Equal(t, kKey, got.Payload.Bottlenecks[0].LocalKernelKey)
	require.InDelta(t, 64.2, got.Payload.Bottlenecks[0].GPUTimeSharePercent, 1e-6)

	// DELETE aborts active probe.
	deleted, err := client.DeleteProbe(ctx, target)
	require.NoError(t, err)
	require.Equal(t, ProbeStateAborted, deleted.State)
}

func TestClient_ProbeErrorClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    int
		headers   map[string]string
		body      map[string]any
		assertErr func(t *testing.T, err error)
	}{
		{
			name:   "409 boot_id_mismatch",
			status: http.StatusConflict,
			body: map[string]any{
				"error":            "boot_id_mismatch",
				"message":          "boot_id mismatch",
				"boot_id":          "boot-live",
				"expected_boot_id": "boot-old",
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrBootIDMismatch)
			},
		},
		{
			name:   "409 role_paused",
			status: http.StatusConflict,
			body: map[string]any{
				"error":   "role_paused",
				"message": "Cannot start probe while role is paused",
				"boot_id": "boot-live",
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrRolePaused)
			},
		},
		{
			name:   "409 probe_in_progress",
			status: http.StatusConflict,
			body: map[string]any{
				"error":    "probe_in_progress",
				"message":  "Probe p-1 is already in progress",
				"boot_id":  "boot-live",
				"probe_id": "p-1",
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrProbeInProgress)
				var pipErr *ProbeInProgressError
				require.ErrorAs(t, err, &pipErr)
				require.Equal(t, "p-1", pipErr.ProbeID)
				require.Equal(t, "boot-live", pipErr.BootID)
			},
		},
		{
			name:   "422 unsupported_engine_version",
			status: http.StatusUnprocessableEntity,
			body: map[string]any{
				"error":   "unsupported_engine_version",
				"message": "vLLM collective_rpc hook unavailable",
				"boot_id": "boot-live",
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrUnsupportedEngineVersion)
				var uevErr *UnsupportedEngineVersionError
				require.ErrorAs(t, err, &uevErr)
				require.Contains(t, uevErr.Error(), "collective_rpc")
			},
		},
		{
			name:   "429 probe_cooldown with JSON retry_after_seconds and Retry-After header",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After": "45",
			},
			body: map[string]any{
				"error":               "probe_cooldown",
				"message":             "Probe cooldown active; retry after 44.5s",
				"boot_id":             "boot-live",
				"retry_after_seconds": 44.5,
				"cooldown_seconds":    60.0,
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrProbeCooldown)
				var cdErr *ProbeCooldownError
				require.ErrorAs(t, err, &cdErr)
				require.Equal(t, 44500*time.Millisecond, cdErr.RetryAfter)
				require.Equal(t, 60*time.Second, cdErr.Cooldown)
			},
		},
		{
			name:   "429 probe_cooldown using Retry-After header fallback",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After": "12",
			},
			body: map[string]any{
				"error": "probe_cooldown",
			},
			assertErr: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrProbeCooldown)
				var cdErr *ProbeCooldownError
				require.ErrorAs(t, err, &cdErr)
				require.Equal(t, 12*time.Second, cdErr.RetryAfter)
				require.Contains(t, cdErr.Error(), "12s")
			},
		},
		{
			name:   "401 unauthorized surfaces as HTTPStatusError",
			status: http.StatusUnauthorized,
			body: map[string]any{
				"error":   "unauthorized",
				"message": "bad probe token",
			},
			assertErr: func(t *testing.T, err error) {
				var stErr *HTTPStatusError
				require.ErrorAs(t, err, &stErr)
				require.Equal(t, http.StatusUnauthorized, stErr.StatusCode)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()

			client := NewClient()
			target := targetFromServer(t, srv)
			_, err := client.StartProbe(context.Background(), target, ProbeRequest{
				ExpectedBootID: "boot-old",
				Steps:          3,
			})
			require.Error(t, err)
			tc.assertErr(t, err)
		})
	}
}

func TestClient_ProbeValidationAndTimeout(t *testing.T) {
	t.Parallel()

	client := NewClient(WithTimeout(60 * time.Millisecond))
	dummy := Target{Host: "127.0.0.1", Port: 8000}

	_, err := client.StartProbe(context.Background(), dummy, ProbeRequest{})
	require.ErrorIs(t, err, ErrInvalidArgument)

	_, err = client.StartProbe(context.Background(), dummy, ProbeRequest{ExpectedBootID: "b1", Steps: 2})
	require.ErrorIs(t, err, ErrInvalidArgument)

	_, err = client.StartProbe(context.Background(), dummy, ProbeRequest{ExpectedBootID: "b1", Steps: 6})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// Response boot_id mismatch on StartProbe and HTTP 204 on DeleteProbe.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(ProbeResponse{BootID: "other-boot", State: ProbeStateRunning})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	target := targetFromServer(t, srv)
	_, err = client.StartProbe(context.Background(), target, ProbeRequest{ExpectedBootID: "want-boot", Steps: 4})
	require.ErrorIs(t, err, ErrBootIDMismatch)

	delResp, err := client.DeleteProbe(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, ProbeStateAborted, delResp.State)

	_, err = client.GetProbe(context.Background(), target)
	require.Error(t, err)
	require.False(t, IsAmbiguousOperation(err))
}
