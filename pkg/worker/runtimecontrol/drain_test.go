package runtimecontrol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestDrainReader_VLLMAndSGLangIncludingRestartCountPositive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		engine       string
		restartCount int32
		metricsBody  string
		wantRunning  float64
		wantWaiting  float64
		wantDrained  bool
		wantErr      bool
	}{
		{
			name:         "vllm drained after container restart",
			engine:       workercore.ModelDeploymentEngineVLLM,
			restartCount: 3,
			metricsBody: `# HELP vllm:num_requests_running Number of requests currently running.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="qwen"} 0.0
vllm:num_requests_running{engine="1",model_name="qwen"} 0
vllm:num_requests_waiting{engine="0",model_name="qwen"} 0
`,
			wantRunning: 0,
			wantWaiting: 0,
			wantDrained: true,
		},
		{
			name:         "vllm busy sums across labels",
			engine:       workercore.ModelDeploymentEngineVLLM,
			restartCount: 1,
			metricsBody: `vllm:num_requests_running{engine="0",model_name="qwen"} 2
vllm:num_requests_running{engine="1",model_name="qwen"} 3
vllm:num_requests_waiting{engine="0",model_name="qwen"} 4
`,
			wantRunning: 5,
			wantWaiting: 4,
			wantDrained: false,
		},
		{
			name:         "sglang drained after restart",
			engine:       workercore.ModelDeploymentEngineSGLang,
			restartCount: 5,
			metricsBody: `sglang:num_running_reqs{model_name="qwen"} 0
sglang:num_queue_reqs{model_name="qwen"} 0
`,
			wantRunning: 0,
			wantWaiting: 0,
			wantDrained: true,
		},
		{
			name:         "sglang busy with pd transfer queues",
			engine:       workercore.ModelDeploymentEngineSGLang,
			restartCount: 2,
			metricsBody: `sglang:num_running_reqs{dp_rank="0"} 1
sglang:num_queue_reqs{dp_rank="0"} 0
sglang:num_decode_transfer_queue_reqs{dp_rank="0"} 2
`,
			wantRunning: 1,
			wantWaiting: 2,
			wantDrained: false,
		},
		{
			name:         "missing waiting gauge errors",
			engine:       workercore.ModelDeploymentEngineVLLM,
			restartCount: 0,
			metricsBody:  `vllm:num_requests_running{engine="0"} 0`,
			wantErr:      true,
		},
		{
			name:         "malformed NaN gauge errors",
			engine:       workercore.ModelDeploymentEngineSGLang,
			restartCount: 0,
			metricsBody: `sglang:num_running_reqs 0
sglang:num_queue_reqs NaN
`,
			wantErr: true,
		},
		{
			name:         "negative gauge errors",
			engine:       workercore.ModelDeploymentEngineVLLM,
			restartCount: 0,
			metricsBody: `vllm:num_requests_running -1
vllm:num_requests_waiting 0
`,
			wantErr: true,
		},
	}

	fixedNow := time.Date(2026, 11, 20, 6, 30, 0, 0, time.UTC)
	reader := NewDrainReader(WithDrainClock(func() time.Time { return fixedNow }))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/metrics", r.URL.Path)
				w.Header().Set("Content-Type", "text/plain; version=0.0.4")
				_, _ = w.Write([]byte(tc.metricsBody))
			}))
			defer srv.Close()

			reading, err := reader.Read(context.Background(), DrainTarget{
				Target:       targetFromServer(t, srv),
				Engine:       tc.engine,
				PodUID:       types.UID("pod-uid-1"),
				BootID:       "boot-uuid-1",
				RestartCount: tc.restartCount,
			})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, types.UID("pod-uid-1"), reading.PodUID)
			require.Equal(t, "boot-uuid-1", reading.BootID)
			require.Equal(t, tc.restartCount, reading.RestartCount)
			require.InDelta(t, tc.wantRunning, reading.Running, 1e-9)
			require.InDelta(t, tc.wantWaiting, reading.Waiting, 1e-9)
			require.Equal(t, tc.wantDrained, reading.Drained)
			require.Equal(t, fixedNow, reading.ObservedAt)
		})
	}
}

func TestDrainReader_ErrorsOnUnreachableRedirectAndTLS(t *testing.T) {
	t.Parallel()

	// TLS metrics endpoint succeeds.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:num_requests_running 0\nvllm:num_requests_waiting 0\n"))
	}))
	defer tlsSrv.Close()

	reader := NewDrainReader()
	reading, err := reader.Read(context.Background(), DrainTarget{
		Target:       targetFromServer(t, tlsSrv),
		Engine:       workercore.ModelDeploymentEngineVLLM,
		PodUID:       types.UID("pod-tls"),
		BootID:       "boot-tls",
		RestartCount: 4,
	})
	require.NoError(t, err)
	require.True(t, reading.Drained)

	// Redirect is refused.
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer redirSrv.Close()

	_, err = reader.Read(context.Background(), DrainTarget{
		Target: targetFromServer(t, redirSrv),
		Engine: workercore.ModelDeploymentEngineVLLM,
		PodUID: types.UID("pod-redir"),
		BootID: "boot-redir",
	})
	require.ErrorIs(t, err, ErrRedirectRefused)

	// Unreachable endpoint returns error.
	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedTarget := targetFromServer(t, closedSrv)
	closedSrv.Close()

	_, err = reader.Read(context.Background(), DrainTarget{
		Target: closedTarget,
		Engine: workercore.ModelDeploymentEngineVLLM,
		PodUID: types.UID("pod-down"),
		BootID: "boot-down",
	})
	require.Error(t, err)
}

func TestDrainReader_ValidationOptionsAndQuotedBraceLabels(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader := NewDrainReader(
		WithDrainTimeout(2*time.Second),
		WithDrainMaxResponseBytes(1024),
	)

	// Validation errors.
	_, err := reader.Read(ctx, DrainTarget{Engine: workercore.ModelDeploymentEngineVLLM, BootID: "b"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = reader.Read(ctx, DrainTarget{Engine: workercore.ModelDeploymentEngineVLLM, PodUID: "u"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = reader.Read(ctx, DrainTarget{Engine: "Unknown", PodUID: "u", BootID: "b"})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// Custom metrics path and quoted "}" inside Prometheus label value.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/custom-metrics":
			_, _ = w.Write([]byte(
				"vllm:num_requests_running{model=\"qwen}v1\",esc=\"a\\\"b\"} 2\n" +
					"vllm:num_requests_waiting{model=\"qwen}v1\"} 0\n",
			))
		case "/too-big":
			_, _ = w.Write(make([]byte, 2048))
		case "/error-500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	reading, err := reader.Read(ctx, DrainTarget{
		Target:      targetFromServer(t, srv),
		MetricsPath: "custom-metrics",
		Engine:      workercore.ModelDeploymentEngineVLLM,
		PodUID:      types.UID("pod-q"),
		BootID:      "boot-q",
	})
	require.NoError(t, err)
	require.InDelta(t, 2, reading.Running, 1e-9)
	require.False(t, reading.Drained)

	_, err = reader.Read(ctx, DrainTarget{
		Target:      targetFromServer(t, srv),
		MetricsPath: "/too-big",
		Engine:      workercore.ModelDeploymentEngineVLLM,
		PodUID:      types.UID("pod-big"),
		BootID:      "boot-big",
	})
	require.ErrorIs(t, err, ErrResponseTooLarge)

	_, err = reader.Read(ctx, DrainTarget{
		Target:      targetFromServer(t, srv),
		MetricsPath: "/error-500",
		Engine:      workercore.ModelDeploymentEngineVLLM,
		PodUID:      types.UID("pod-500"),
		BootID:      "boot-500",
	})
	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusInternalServerError, statusErr.StatusCode)
}
