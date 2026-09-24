package worker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestRenderModelDeploymentPod_DrainsBeforeTheEngineStops asserts that every replica whose command
// line the operator builds carries the drain hook on its engine container, pointed at the engine's
// own metrics listener and counting that engine's gauges, and the grace the hook is budgeted
// against.
//
// THE PORT AND SCHEME ARE COMPARED WITH THE SCRAPE ANNOTATIONS rather than with a literal alone:
// the hook and the scrape name one listener, and on a direct decoder that listener is the engine
// behind the proxy, not the port the Service fronts.
func TestRenderModelDeploymentPod_DrainsBeforeTheEngineStops(t *testing.T) {
	directDecoder := func(native bool) func(*testing.T) ModelDeploymentRenderInput {
		return func(t *testing.T) ModelDeploymentRenderInput {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindDecode
			})

			return ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[0], InstanceType: newRenderInstanceType(),
				Connector: ModelDeploymentConnectorRender{
					Args: []string{"--kv-transfer-config", `{}`}, KVTransfer: true, RoutingSidecar: true,
				},
				NativeSidecar: native,
			}
		}
	}
	plain := func(roleIndex int, mutate ...func(*workercore.ModelDeployment)) func(*testing.T) ModelDeploymentRenderInput {
		return func(t *testing.T) ModelDeploymentRenderInput {
			md := newRenderDeployment(mutate...)

			return ModelDeploymentRenderInput{
				Deployment: md, Role: &md.Spec.Roles[roleIndex], InstanceType: newRenderInstanceType(),
			}
		}
	}

	testCases := []struct {
		name   string
		input  func(*testing.T) ModelDeploymentRenderInput
		engine string
		url    string
	}{
		{
			name:   "a vLLM server",
			input:  plain(0),
			engine: workercore.ModelDeploymentEngineVLLM,
			url:    "http://127.0.0.1:8000/metrics",
		},
		{
			name: "an SGLang server",
			input: plain(0, func(md *workercore.ModelDeployment) {
				md.Spec.Engine = workercore.ModelDeploymentEngine{
					Name: workercore.ModelDeploymentEngineSGLang, Version: "0.5.18",
				}
				md.Spec.Roles[0].Image = "lmsysorg/sglang:v0.5.18"
			}),
			engine: workercore.ModelDeploymentEngineSGLang,
			url:    "http://127.0.0.1:8000/metrics",
		},
		{
			name:   "the prefill role of a two-role deployment",
			input:  plain(0, twoRoleDeploymentMutations()...),
			engine: workercore.ModelDeploymentEngineVLLM,
			url:    "http://127.0.0.1:8000/metrics",
		},
		{
			name:   "the decode role of a two-role deployment",
			input:  plain(1, twoRoleDeploymentMutations()...),
			engine: workercore.ModelDeploymentEngineVLLM,
			url:    "http://127.0.0.1:8000/metrics",
		},
		{
			name:   "a direct decoder with a native routing sidecar",
			input:  directDecoder(true),
			engine: workercore.ModelDeploymentEngineVLLM,
			url:    "http://127.0.0.1:8200/metrics",
		},
		{
			name:   "a direct decoder with a classic routing sidecar",
			input:  directDecoder(false),
			engine: workercore.ModelDeploymentEngineVLLM,
			url:    "http://127.0.0.1:8200/metrics",
		},
		{
			name: "a TLS-listening role on a declared port",
			input: plain(0, func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{{Protocol: core.ProtocolTCP, Port: 9000}}
				md.Spec.Roles[0].ExtraArgs = []string{"--ssl-certfile", "/etc/tls/tls.crt"}
			}),
			engine: workercore.ModelDeploymentEngineVLLM,
			url:    "https://127.0.0.1:9000/metrics",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod, err := renderModelDeploymentPod(context.Background(), tc.input(t))
			require.NoError(t, err)

			require.NotNil(t, pod.Spec.TerminationGracePeriodSeconds)
			assert.Equal(t, int64(30), *pod.Spec.TerminationGracePeriodSeconds,
				"the ceiling the drain is budgeted against, rendered rather than left to the default")

			var engineC *core.Container
			for i := range pod.Spec.Containers {
				c := &pod.Spec.Containers[i]
				if c.Name == modelDeploymentMainContainerName {
					engineC = c
					continue
				}
				assert.Nil(t, c.Lifecycle, "only the engine drains; %q is not the engine", c.Name)
			}
			for _, c := range pod.Spec.InitContainers {
				assert.Nil(t, c.Lifecycle,
					"a native sidecar is stopped only after the engine exits, so it needs no hook")
			}
			require.NotNil(t, engineC)
			require.NotNil(t, engineC.Lifecycle)
			require.NotNil(t, engineC.Lifecycle.PreStop)
			require.NotNil(t, engineC.Lifecycle.PreStop.Exec)

			command := engineC.Lifecycle.PreStop.Exec.Command
			require.Len(t, command, 3)
			assert.Equal(t, []string{"python3", "-c"}, command[:2])
			script := command[2]

			assert.Contains(t, script, strconv.Quote(tc.url))
			assert.Contains(t, script, strconv.Quote(fmt.Sprintf("%s://127.0.0.1:%s/metrics",
				pod.Annotations["prometheus.io/scheme"], pod.Annotations["prometheus.io/port"])),
				"the hook reads the listener the scrape annotations name")

			for engine, names := range modelDeploymentInFlightMetrics {
				for _, name := range names {
					if engine == tc.engine {
						assert.Contains(t, script, strconv.Quote(name))
					} else {
						assert.NotContains(t, script, strconv.Quote(name),
							"a %s gauge in a %s replica's hook", engine, tc.engine)
					}
				}
			}
			assert.Contains(t, script, fmt.Sprintf("time.sleep(%d)", modelDeploymentDrainSettleSeconds))
			assert.Contains(t, script, fmt.Sprintf("start < %d:", modelDeploymentDrainDeadlineSeconds))
		})
	}
}

// TestRenderModelDeploymentPod_TakeOverRoleGetsNoDrain asserts that a role which replaced the
// command line keeps the Pod it rendered before the drain existed: no hook, since the operator
// cannot claim the container serves the metrics the hook reads, and no grace, since the grace
// exists for the hook.
func TestRenderModelDeploymentPod_TakeOverRoleGetsNoDrain(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Command = []string{"/bin/my-server", "--flag"}
	})
	pod := renderOne(t, md, newRenderInstanceType())

	assert.Nil(t, pod.Spec.TerminationGracePeriodSeconds)
	require.Len(t, pod.Spec.Containers, 1)
	assert.Nil(t, pod.Spec.Containers[0].Lifecycle)
}

// TestModelDeploymentDrain_BudgetFitsTheGrace asserts the relationship between the timings rather
// than either number: the settle ends before the deadline, and the deadline plus the most the last
// read can overrun it -- one read timeout and one poll interval -- still leaves time for the engine
// to exit before the kubelet kills it.
func TestModelDeploymentDrain_BudgetFitsTheGrace(t *testing.T) {
	const lastReadOverrun = 2

	assert.Less(t, modelDeploymentDrainSettleSeconds, modelDeploymentDrainDeadlineSeconds)
	assert.Less(t, modelDeploymentDrainDeadlineSeconds+lastReadOverrun,
		modelDeploymentTerminationGracePeriodSeconds)
	assert.Equal(t, modelDeploymentTerminationGracePeriodSeconds,
		modelDeploymentDrainDeadlineSeconds+modelDeploymentDrainExitSeconds)
}

// TestModelDeploymentDrainScript_WaitsForTheEngine runs the hook's program against a stand-in
// engine, on a scale of seconds, and times when it returns.
//
// EACH CASE IS TOLD APART FROM THE ONE THE PROGRAM WOULD PRODUCE IF IT WERE BROKEN. "It returned
// early" is also what a program that cannot reach the server produces, so the idle cases assert how
// many reads the server answered as well as the time; "it returned at the deadline" is also what a
// program that never parses a sample produces, so the busy case is paired with one that goes idle.
func TestModelDeploymentDrainScript_WaitsForTheEngine(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH; the hook's program cannot be run here")
	}

	const (
		settle   = 1
		deadline = 8
	)
	names := modelDeploymentInFlightMetrics[workercore.ModelDeploymentEngineVLLM]

	// busyFor answers the first n reads with one running request and every read after that with
	// none. The idle body carries what the parser must pass over: the HELP and TYPE comments, a
	// sibling gauge whose name extends a counted one, a label value holding a brace and a space,
	// and a sample with no labels at all.
	busyFor := func(n int64, reads *atomic.Int64) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			running := "0.0"
			if reads.Add(1) <= n {
				running = "1.0"
			}
			_, _ = fmt.Fprintf(w, "# HELP vllm:num_requests_running Number of requests in model execution batches.\n"+
				"# TYPE vllm:num_requests_running gauge\n"+
				"vllm:num_requests_running{engine=\"0\",model_name=\"a} b\"} %s\n"+
				"vllm:num_requests_waiting 0\n"+
				"vllm:num_requests_waiting_by_reason{engine=\"0\",reason=\"capacity\"} 7.0\n", running)
		}
	}
	run := func(t *testing.T, url string) time.Duration {
		t.Helper()

		began := time.Now()
		out, err := exec.Command(python, "-c", modelDeploymentDrainScript(url, names, settle, deadline)).CombinedOutput()
		require.NoError(t, err, "the hook always exits 0; output: %s", out)

		return time.Since(began)
	}

	t.Run("it returns on the second idle read once the engine goes idle", func(t *testing.T) {
		t.Parallel()

		var reads atomic.Int64
		srv := httptest.NewServer(busyFor(2, &reads))
		defer srv.Close()

		took := run(t, srv.URL+"/metrics")
		assert.EqualValues(t, 4, reads.Load(), "two busy reads, then two idle ones")
		assert.GreaterOrEqual(t, took, (settle+3)*time.Second, "one poll interval between each read")
		assert.Less(t, took, deadline*time.Second)
	})

	t.Run("it reads through a certificate it cannot verify", func(t *testing.T) {
		t.Parallel()

		var reads atomic.Int64
		srv := httptest.NewTLSServer(busyFor(1, &reads))
		defer srv.Close()

		took := run(t, srv.URL+"/metrics")
		assert.EqualValues(t, 3, reads.Load(),
			"a refused handshake would end the hook with no read answered at all")
		assert.Less(t, took, deadline*time.Second)
	})

	t.Run("it gives up at the deadline on an engine that stays busy", func(t *testing.T) {
		t.Parallel()

		var reads atomic.Int64
		srv := httptest.NewServer(busyFor(1<<30, &reads))
		defer srv.Close()

		took := run(t, srv.URL+"/metrics")
		assert.GreaterOrEqual(t, took, deadline*time.Second)
		assert.Less(t, took, (deadline+3)*time.Second, "one read timeout and one poll past it at most")
		assert.GreaterOrEqual(t, reads.Load(), int64(deadline-settle-1))
	})

	t.Run("it counts a sample it cannot read as busy", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "vllm:num_requests_running{engine=\"0\"}\nvllm:num_requests_waiting 0\n")
		}))
		defer srv.Close()

		took := run(t, srv.URL+"/metrics")
		assert.GreaterOrEqual(t, took, deadline*time.Second,
			"a replica is idle only when every counted gauge reads zero")
	})

	t.Run("it keeps waiting on an engine too busy to answer in time", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()

		took := run(t, srv.URL+"/metrics")
		assert.GreaterOrEqual(t, took, deadline*time.Second)
		assert.Less(t, took, (deadline+3)*time.Second)
	})

	t.Run("it returns after the settle when nothing listens on the port", func(t *testing.T) {
		t.Parallel()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		took := run(t, "http://"+addr+"/metrics")
		assert.GreaterOrEqual(t, took, settle*time.Second, "the settle is served even then")
		assert.Less(t, took, (settle+2)*time.Second)
	})
}
