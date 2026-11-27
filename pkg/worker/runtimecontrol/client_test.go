package runtimecontrol

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/utils/json"
)

func targetFromServer(t *testing.T, srv *httptest.Server) Target {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	scheme := "http"
	if srv.TLS != nil {
		scheme = "https"
	}

	return Target{
		Scheme:             scheme,
		Host:               host,
		Port:               port,
		InsecureSkipVerify: srv.TLS != nil,
	}
}

func TestClient_GpustackV1Endpoints(t *testing.T) {
	t.Parallel()

	const (
		bootID = "boot-uuid-1"
		digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)

	var (
		mu      sync.Mutex
		acked   bool
		paused  bool
		obsGen  *int64
		applied string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"), "sentinel client must never send Authorization headers")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == PathReady:
			mu.Lock()
			isReady := acked && !paused
			isPaused := paused
			gen := obsGen
			dig := applied
			mu.Unlock()
			if !isReady {
				reason := "awaiting_ack"
				if isPaused {
					reason = "role_paused"
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(ReadyResponse{
					Ready:               false,
					BootID:              bootID,
					ObservedGeneration:  gen,
					AppliedConfigDigest: dig,
					Reason:              reason,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(ReadyResponse{
				Ready:               true,
				BootID:              bootID,
				ObservedGeneration:  gen,
				AppliedConfigDigest: dig,
			})
		case r.Method == http.MethodGet && r.URL.Path == PathLive:
			mu.Lock()
			isPaused := paused
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(LiveResponse{
				Alive:  true,
				BootID: bootID,
				Paused: isPaused,
			})
		case r.Method == http.MethodPost && r.URL.Path == PathAck:
			var req AckRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.BootID != bootID {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":            "boot_id_mismatch",
					"message":          "boot_id mismatch",
					"boot_id":          bootID,
					"expected_boot_id": req.BootID,
				})
				return
			}
			mu.Lock()
			acked = true
			g := req.ObservedGeneration
			obsGen = &g
			applied = req.AppliedConfigDigest
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(ReadyResponse{
				Ready:               true,
				BootID:              bootID,
				ObservedGeneration:  &g,
				AppliedConfigDigest: req.AppliedConfigDigest,
			})
		case r.Method == http.MethodGet && r.URL.Path == PathPause:
			mu.Lock()
			isPaused := paused
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(PauseResponse{
				Paused: isPaused,
				BootID: bootID,
			})
		case r.Method == http.MethodPost && r.URL.Path == PathPause:
			var req PauseRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.ExpectedBootID != "" && req.ExpectedBootID != bootID {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":            "boot_id_mismatch",
					"message":          "stale boot_id",
					"boot_id":          bootID,
					"expected_boot_id": req.ExpectedBootID,
				})
				return
			}
			mu.Lock()
			paused = req.Paused
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(PauseResponse{
				Paused: req.Paused,
				BootID: bootID,
			})
		case r.Method == http.MethodGet && r.URL.Path == PathRuntimeConfig:
			mu.Lock()
			isPaused := paused
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(RuntimeConfigState{
				Engine:              "sglang",
				BootID:              bootID,
				MembershipEpoch:     2,
				AppliedConfigDigest: digest,
				Ready:               !isPaused,
				Paused:              isPaused,
				SupportedFields:     []string{ParamSchedulingMaxActiveRequests, ParamSpeculativeDraftBudgetFraction},
				UnsupportedFields:   []string{},
				BootDefaults:        map[string]any{ParamSchedulingMaxActiveRequests: 64},
				Ranks: []RankReceipt{
					{RankID: 0, BootID: "rank-0", MembershipEpoch: 2, AppliedDigest: digest},
				},
				Values: map[string]any{
					ParamSchedulingMaxActiveRequests:    16,
					ParamSpeculativeDraftBudgetFraction: 0.5,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == PathRuntimeConfig:
			var req ApplyRuntimeConfigRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.ExpectedBootID != bootID {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":            "boot_id_mismatch",
					"message":          "stale boot_id",
					"boot_id":          bootID,
					"expected_boot_id": req.ExpectedBootID,
				})
				return
			}
			if _, ok := req.Parameters["scheduling.unsupportedKnob"]; ok {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":              "unsupported_fields",
					"message":            "Unsupported runtime control fields: scheduling.unsupportedKnob",
					"unsupported_fields": []string{"scheduling.unsupportedKnob"},
					"supported_fields":   []string{ParamSchedulingMaxActiveRequests},
					"boot_id":            bootID,
				})
				return
			}
			if req.Generation == 999 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":   "invalid_argument",
					"message": "bad generation",
				})
				return
			}
			if req.OperationID == "bad-rank-digest" {
				_ = json.NewEncoder(w).Encode(RuntimeConfigState{
					Engine:              "vllm",
					BootID:              bootID,
					MembershipEpoch:     1,
					AppliedConfigDigest: req.AppliedConfigDigest,
					Ranks: []RankReceipt{
						{RankID: 0, BootID: "rank-0", MembershipEpoch: 1, AppliedDigest: req.AppliedConfigDigest},
						{RankID: 1, BootID: "rank-1", MembershipEpoch: 1, AppliedDigest: "sha256:stale"},
					},
				})
				return
			}
			gen := req.Generation
			_ = json.NewEncoder(w).Encode(RuntimeConfigState{
				Engine:              "vllm",
				BootID:              bootID,
				MembershipEpoch:     1,
				ObservedGeneration:  &gen,
				AppliedConfigDigest: req.AppliedConfigDigest,
				LastOperationID:     req.OperationID,
				Ready:               true,
				BootDefaults:        map[string]any{ParamSchedulingMaxActiveRequests: 64},
				Ranks: []RankReceipt{
					{RankID: 0, BootID: "rank-0", MembershipEpoch: 1, AppliedDigest: req.AppliedConfigDigest, Values: req.Parameters},
					{RankID: 1, BootID: "rank-1", MembershipEpoch: 1, AppliedDigest: req.AppliedConfigDigest, Values: req.Parameters},
				},
				Values: req.Parameters,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient()
	target := targetFromServer(t, srv)
	ctx := context.Background()

	// Unacked ready returns 503 JSON parsed cleanly.
	readyResp, err := client.GetReady(ctx, target)
	require.NoError(t, err)
	require.False(t, readyResp.Ready)
	require.Equal(t, bootID, readyResp.BootID)
	require.Equal(t, "awaiting_ack", readyResp.Reason)

	// Ack with wrong boot_id returns ErrBootIDMismatch.
	_, err = client.Ack(ctx, target, AckRequest{BootID: "wrong-boot", ObservedGeneration: 3, AppliedConfigDigest: digest})
	require.ErrorIs(t, err, ErrBootIDMismatch)

	// Ack with valid boot_id opens readiness.
	ackResp, err := client.Ack(ctx, target, AckRequest{BootID: bootID, ObservedGeneration: 3, AppliedConfigDigest: digest})
	require.NoError(t, err)
	require.True(t, ackResp.Ready)
	require.NotNil(t, ackResp.ObservedGeneration)
	require.Equal(t, int64(3), *ackResp.ObservedGeneration)

	readyResp, err = client.GetReady(ctx, target)
	require.NoError(t, err)
	require.True(t, readyResp.Ready)

	// Pause toggle via /gpustack/v1/pause.
	pauseState, err := client.GetPause(ctx, target)
	require.NoError(t, err)
	require.False(t, pauseState.Paused)
	require.Equal(t, bootID, pauseState.BootID)

	_, err = client.SetPause(ctx, target, PauseRequest{Paused: true, ExpectedBootID: "wrong-boot"})
	require.ErrorIs(t, err, ErrBootIDMismatch)

	pauseState, err = client.SetPause(ctx, target, PauseRequest{Paused: true, ExpectedBootID: bootID})
	require.NoError(t, err)
	require.True(t, pauseState.Paused)

	// Live probe remains alive while paused, ready reports role_paused.
	liveResp, err := client.GetLive(ctx, target)
	require.NoError(t, err)
	require.True(t, liveResp.Alive)
	require.True(t, liveResp.Paused)

	readyResp, err = client.GetReady(ctx, target)
	require.NoError(t, err)
	require.False(t, readyResp.Ready)
	require.Equal(t, "role_paused", readyResp.Reason)

	// Unpause without ExpectedBootID works.
	pauseState, err = client.SetPause(ctx, target, PauseRequest{Paused: false})
	require.NoError(t, err)
	require.False(t, pauseState.Paused)

	// GetRuntimeConfig decodes flat Values and BootDefaults.
	rcState, err := client.GetRuntimeConfig(ctx, target)
	require.NoError(t, err)
	require.Equal(t, int64(2), rcState.MembershipEpoch)
	require.Equal(t, float64(64), rcState.BootDefaults[ParamSchedulingMaxActiveRequests])
	require.Equal(t, float64(16), rcState.Values[ParamSchedulingMaxActiveRequests])
	require.InDelta(t, 0.5, rcState.Values[ParamSpeculativeDraftBudgetFraction], 1e-9)

	// ApplyRuntimeConfig 409 -> ErrBootIDMismatch.
	_, err = client.ApplyRuntimeConfig(ctx, target, ApplyRuntimeConfigRequest{
		OperationID:         "op-1",
		ExpectedBootID:      "wrong-boot",
		Generation:          4,
		AppliedConfigDigest: digest,
	})
	require.ErrorIs(t, err, ErrBootIDMismatch)

	// ApplyRuntimeConfig 422 -> UnsupportedFieldsError.
	_, err = client.ApplyRuntimeConfig(ctx, target, ApplyRuntimeConfigRequest{
		OperationID:         "op-2",
		ExpectedBootID:      bootID,
		Generation:          4,
		AppliedConfigDigest: digest,
		Parameters:          map[string]any{"scheduling.unsupportedKnob": 1},
	})
	var unsupportedErr *UnsupportedFieldsError
	require.ErrorAs(t, err, &unsupportedErr)
	require.Equal(t, []string{"scheduling.unsupportedKnob"}, unsupportedErr.UnsupportedFields)

	// ApplyRuntimeConfig 400 -> ErrInvalidArgument.
	_, err = client.ApplyRuntimeConfig(ctx, target, ApplyRuntimeConfigRequest{
		OperationID:         "op-3",
		ExpectedBootID:      bootID,
		Generation:          999,
		AppliedConfigDigest: digest,
	})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// ApplyRuntimeConfig with mismatched per-rank digest -> ErrRankApplyFailed.
	_, err = client.ApplyRuntimeConfig(ctx, target, ApplyRuntimeConfigRequest{
		OperationID:         "bad-rank-digest",
		ExpectedBootID:      bootID,
		Generation:          4,
		AppliedConfigDigest: digest,
	})
	require.ErrorIs(t, err, ErrRankApplyFailed)

	// ApplyRuntimeConfig 200 with verified per-rank receipts succeeds.
	appliedState, err := client.ApplyRuntimeConfig(ctx, target, ApplyRuntimeConfigRequest{
		OperationID:         "op-ok",
		ExpectedBootID:      bootID,
		Generation:          4,
		AppliedConfigDigest: digest,
		Parameters:          map[string]any{ParamSchedulingMaxActiveRequests: int64(16)},
		ResetFields:         []string{ParamSchedulingCacheAwareWindow},
	})
	require.NoError(t, err)
	require.Equal(t, digest, appliedState.AppliedConfigDigest)
	require.Len(t, appliedState.Ranks, 2)
}

func TestClient_TimeoutProducesAmbiguousOperationError(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	client := NewClient(WithTimeout(80 * time.Millisecond))
	target := targetFromServer(t, srv)

	_, err := client.ApplyRuntimeConfig(context.Background(), target, ApplyRuntimeConfigRequest{
		OperationID:         "op-timeout",
		ExpectedBootID:      "boot-1",
		Generation:          1,
		AppliedConfigDigest: "sha256:abc",
	})
	require.Error(t, err)
	require.True(t, IsAmbiguousOperation(err))

	var ambErr *AmbiguousOperationError
	require.ErrorAs(t, err, &ambErr)
	require.Equal(t, "op-timeout", ambErr.OperationID)

	// Mutating SetPause and Ack POSTs also wrap timeouts in AmbiguousOperationError.
	_, err = client.SetPause(context.Background(), target, PauseRequest{Paused: true, ExpectedBootID: "boot-1"})
	require.Error(t, err)
	require.True(t, IsAmbiguousOperation(err))

	_, err = client.Ack(context.Background(), target, AckRequest{BootID: "boot-1", ObservedGeneration: 1})
	require.Error(t, err)
	require.True(t, IsAmbiguousOperation(err))

	// Non-mutating GET does not produce AmbiguousOperationError.
	_, err = client.GetReady(context.Background(), target)
	require.Error(t, err)
	require.False(t, IsAmbiguousOperation(err))
}

func TestClient_RefusesRedirects(t *testing.T) {
	t.Parallel()

	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			followed = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer srv.Close()

	client := NewClient()
	target := targetFromServer(t, srv)

	_, err := client.GetRuntimeConfig(context.Background(), target)
	require.ErrorIs(t, err, ErrRedirectRefused)
	require.False(t, followed)

	_, err = client.SetPause(context.Background(), target, PauseRequest{Paused: true})
	require.ErrorIs(t, err, ErrRedirectRefused)
	require.False(t, followed)
}

func TestClient_BoundedResponseBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(make([]byte, 2048))
	}))
	defer srv.Close()

	client := NewClient(WithMaxResponseBytes(512))
	target := targetFromServer(t, srv)
	_, err := client.GetRuntimeConfig(context.Background(), target)
	require.ErrorIs(t, err, ErrResponseTooLarge)
}

func TestClient_AmbiguousOperationHelper(t *testing.T) {
	t.Parallel()

	require.False(t, IsAmbiguousOperation(nil))
	require.False(t, IsAmbiguousOperation(errors.New("plain")))

	ambWithID := &AmbiguousOperationError{
		Operation:   "POST /gpustack/v1/runtime_config",
		OperationID: "op-9",
		Reason:      "timeout",
		Err:         context.DeadlineExceeded,
	}
	require.True(t, IsAmbiguousOperation(ambWithID))
	require.Contains(t, ambWithID.Error(), "op-9")
	require.ErrorIs(t, ambWithID, context.DeadlineExceeded)

	ambNoID := &AmbiguousOperationError{Operation: "POST /gpustack/v1/pause", Reason: "timeout"}
	require.Contains(t, ambNoID.Error(), "POST /gpustack/v1/pause")

	unsup := &UnsupportedFieldsError{UnsupportedFields: []string{"scheduling.foo"}}
	require.Contains(t, unsup.Error(), "scheduling.foo")
	unsupMsg := &UnsupportedFieldsError{Message: "custom msg"}
	require.Equal(t, "custom msg", unsupMsg.Error())

	statusErr := &HTTPStatusError{Method: http.MethodPost, Path: "/x", StatusCode: http.StatusInternalServerError}
	require.Contains(t, statusErr.Error(), "500")
	statusErrMsg := &HTTPStatusError{Method: http.MethodPost, Path: "/x", StatusCode: http.StatusInternalServerError, ErrorCode: "boom", Message: "failed"}
	require.Contains(t, statusErrMsg.Error(), "failed")
}

func TestClient_ValidationErrorPathsAndCustomTLS(t *testing.T) {
	t.Parallel()

	client := NewClient()
	ctx := context.Background()

	// Target URL validation.
	_, err := client.GetReady(ctx, Target{Scheme: "ftp", Host: "127.0.0.1", Port: 8000})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = client.GetReady(ctx, Target{Scheme: "http", Host: "", Port: 8000})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = client.GetReady(ctx, Target{Scheme: "http", Host: "127.0.0.1", Port: 0})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// Input validation before network round trips.
	dummyTarget := Target{Host: "127.0.0.1", Port: 8000}
	_, err = client.Ack(ctx, dummyTarget, AckRequest{})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = client.ApplyRuntimeConfig(ctx, dummyTarget, ApplyRuntimeConfigRequest{ExpectedBootID: "b"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = client.ApplyRuntimeConfig(ctx, dummyTarget, ApplyRuntimeConfigRequest{OperationID: "o"})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// Server error codes, dead live probe, response boot_id mismatch, and custom TLSConfig.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case PathLive:
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(LiveResponse{Alive: false, BootID: "dead-boot", Error: "EngineDeadError"})
		case PathPause:
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "internal", "message": "boom"})
				return
			}
			_ = json.NewEncoder(w).Encode(PauseResponse{Paused: true, BootID: "different-boot"})
		case PathRuntimeConfig:
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized", "message": "external gateway auth"})
				return
			}
			_ = json.NewEncoder(w).Encode(RuntimeConfigState{
				Engine: "vllm",
				BootID: "b1",
				Ranks:  []RankReceipt{},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer tlsSrv.Close()

	tlsTarget := targetFromServer(t, tlsSrv)
	tlsTarget.InsecureSkipVerify = false
	tlsTarget.TLSConfig = tlsSrv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()

	liveResp, err := client.GetLive(ctx, tlsTarget)
	require.NoError(t, err)
	require.False(t, liveResp.Alive)
	require.Equal(t, "EngineDeadError", liveResp.Error)

	// Non-sentinel HTTP 401 surfaces as HTTPStatusError (no ErrUnauthorized auth layer).
	_, err = client.GetRuntimeConfig(ctx, tlsTarget)
	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusUnauthorized, statusErr.StatusCode)

	_, err = client.GetPause(ctx, tlsTarget)
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusInternalServerError, statusErr.StatusCode)

	// Response boot_id mismatch on SetPause returns ErrBootIDMismatch.
	_, err = client.SetPause(ctx, tlsTarget, PauseRequest{Paused: true, ExpectedBootID: "expected-boot"})
	require.ErrorIs(t, err, ErrBootIDMismatch)

	// Empty rank receipts on ApplyRuntimeConfig -> ErrRankApplyFailed.
	_, err = client.ApplyRuntimeConfig(ctx, tlsTarget, ApplyRuntimeConfigRequest{
		OperationID:         "op-empty-ranks",
		ExpectedBootID:      "b1",
		Generation:          1,
		AppliedConfigDigest: "sha256:123",
	})
	require.ErrorIs(t, err, ErrRankApplyFailed)

	// Duplicate rank IDs in rank receipts -> ErrRankApplyFailed.
	dupErr := verifyRankReceipts(&RuntimeConfigState{
		BootID: "b1",
		Ranks: []RankReceipt{
			{RankID: 0, BootID: "r0", AppliedDigest: "sha256:123"},
			{RankID: 0, BootID: "r0-dup", AppliedDigest: "sha256:123"},
		},
	}, "b1", "sha256:123")
	require.ErrorIs(t, dupErr, ErrRankApplyFailed)
}

func TestClient_AdditiveKernelLoaderFields(t *testing.T) {
	t.Parallel()

	const (
		bootID         = "boot-kernel-1"
		dispatchDigest = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
		kernelKey      = "vllm/0.8.5/nvidia/sm_90a/rmsnorm/0123456789abcdef"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case PathReady:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ready":                    true,
				"boot_id":                  bootID,
				"observed_generation":      int64(2),
				"applied_config_digest":    "sha256:cfg",
				"loaded_kernel_keys":       []string{kernelKey},
				"dispatch_manifest_digest": dispatchDigest,
			})
		case PathRuntimeConfig:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"engine":                   "vllm",
				"boot_id":                  bootID,
				"membership_epoch":         int64(1),
				"observed_generation":      int64(2),
				"applied_config_digest":    "sha256:cfg",
				"last_operation_id":        "op-1",
				"ready":                    true,
				"paused":                   false,
				"supported_fields":         []string{},
				"unsupported_fields":       []string{},
				"boot_defaults":            map[string]any{},
				"values":                   map[string]any{},
				"loaded_kernel_keys":       []string{kernelKey},
				"dispatch_manifest_digest": dispatchDigest,
				"kernel_load_error":        "unsupported_kernel_artifact",
				"ranks": []map[string]any{
					{
						"rank_id":                  0,
						"boot_id":                  "rank-0",
						"membership_epoch":         int64(1),
						"applied_digest":           "sha256:cfg",
						"loaded_kernel_keys":       []string{kernelKey},
						"dispatch_manifest_digest": dispatchDigest,
						"kernel_load_error":        "unsupported_kernel_artifact",
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient()
	target := targetFromServer(t, srv)
	ctx := context.Background()

	readyResp, err := client.GetReady(ctx, target)
	require.NoError(t, err)
	require.True(t, readyResp.Ready)
	require.Equal(t, []string{kernelKey}, readyResp.LoadedKernelKeys)
	require.Equal(t, dispatchDigest, readyResp.DispatchManifestDigest)
	require.Empty(t, readyResp.KernelLoadError)

	rcResp, err := client.GetRuntimeConfig(ctx, target)
	require.NoError(t, err)
	require.Equal(t, []string{kernelKey}, rcResp.LoadedKernelKeys)
	require.Equal(t, dispatchDigest, rcResp.DispatchManifestDigest)
	require.Equal(t, "unsupported_kernel_artifact", rcResp.KernelLoadError)
	require.Len(t, rcResp.Ranks, 1)
	require.Equal(t, []string{kernelKey}, rcResp.Ranks[0].LoadedKernelKeys)
	require.Equal(t, dispatchDigest, rcResp.Ranks[0].DispatchManifestDigest)
	require.Equal(t, "unsupported_kernel_artifact", rcResp.Ranks[0].KernelLoadError)
}
