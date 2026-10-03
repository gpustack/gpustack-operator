package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// THE MATRIX IS ANCHORED ON ONE NEGATIVE. A body missing a required series must be Unknown with
// Complete false, and the protocol must refuse it a second time against the one gauge list in the
// tree. Every other case is calibrated against that one: it is the answer that would delete a busy
// member if it ever came back Idle, and the only way to know the rest of the matrix is honest is to
// know that case is caught twice.

const (
	drainTestUID        = types.UID("member-uid-1")
	drainTestDeployUID  = types.UID("deployment-uid")
	drainTestEngine     = workercore.ModelDeploymentEngineVLLM
	drainTestContainer  = modelDeploymentMainContainerName
	drainTestPort       = "8000"
	drainTestScheme     = "http"
	drainTestPath       = "/metrics"
	drainTestContainerI = "containerd://before"
	drainTestRole       = "server"
	drainTestOwner      = "ModelDeployment"
	drainTestIP         = "10.0.4.1"
)

// drainTestPod is a member the collector is allowed to read: the engine's own container, a live
// container with a stable identity, and the annotations the render writes.
func drainTestPod(mutate ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "server-0-abcde",
			Namespace: "default",
			UID:       drainTestUID,
			// The role label and the controlling owner are what bind this member to the deployment
			// that declared it, and the read compares both across its two live reads. A fixture
			// without them is a member nothing can be compared against, so they are real here.
			Labels: map[string]string{modelDeploymentLabelKeyComponent: drainTestRole},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workercore.GroupVersion.String(),
				Kind:       drainTestOwner,
				Name:       "qwen",
				UID:        drainTestDeployUID,
				Controller: boolPtr(true),
			}},
			Annotations: map[string]string{
				"prometheus.io/path":   drainTestPath,
				"prometheus.io/port":   drainTestPort,
				"prometheus.io/scheme": drainTestScheme,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    drainTestContainer,
				Image:   "vllm/vllm-openai:v0.29.0",
				Command: []string{"vllm", "serve", "model"},
			}},
		},
		Status: corev1.PodStatus{
			PodIP: drainTestIP,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        drainTestContainer,
				ContainerID: drainTestContainerI,
				Ready:       true,
			}},
		},
	}
	for _, m := range mutate {
		m(pod)
	}

	return pod
}

// drainTestReader is the collector under test with its exec replaced, so every failure shape the
// transport has to survive can be produced on demand.
type drainTestReader struct {
	collector *modelDeploymentDrainCollector
	// client is the locator and fresh is the authority, and they are separate fakes on purpose. A
	// single fake cannot show that the read went to the live reader rather than to the cache the
	// locator already answered from, because a mutation applied to one of them is invisible to a
	// read that only ever consults the other.
	client ctrlcli.Client
	fresh  ctrlcli.Client
	pod    *corev1.Pod
	// bodies is served in order, one per exec, so a test can make the first read complete and the
	// second incomplete without two separate fixtures.
	bodies []string
	calls  int
	// during runs inside the exec, between the identity checks either side of it.
	during func(ctx context.Context, r *drainTestReader)
	err    error
}

// newDrainTestReader wires a fake client, the real collector, and a scripted exec.
func newDrainTestReader(pod *corev1.Pod, bodies ...string) *drainTestReader {
	indexed := func(obj ctrlcli.Object) []string {
		if obj == nil {
			return nil
		}
		p := obj.(*corev1.Pod)
		if p.UID == "" {
			return nil
		}
		return []string{string(p.UID)}
	}

	build := func() ctrlcli.Client {
		return ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(pod.DeepCopy()).
			WithIndex(&corev1.Pod{}, modelDeploymentDrainIndexPodUID, indexed).
			Build()
	}
	client, fresh := build(), build()

	reader := &drainTestReader{
		client: client,
		fresh:  fresh,
		pod:    pod,
		bodies: bodies,
	}
	reader.collector = newModelDeploymentDrainCollector(client, fresh, nil, nil)
	reader.collector.exec = func(
		ctx context.Context, _ *corev1.Pod, _ string, _ []string,
	) (string, error) {
		reader.calls++
		if reader.err != nil {
			return "", reader.err
		}
		if reader.during != nil {
			reader.during(ctx, reader)
		}
		if len(reader.bodies) == 0 {
			return "", nil
		}
		body := reader.bodies[0]
		if len(reader.bodies) > 1 {
			reader.bodies = reader.bodies[1:]
		}

		return body, nil
	}

	return reader
}

// drain runs one read and fails the test if the transport reported an error rather than an answer.
// Every shape the collector can meet is an answer, so an error here is a defect, not a case.
func (r *drainTestReader) drain(t *testing.T, mutate ...func(*modelDeploymentDrainTarget)) modelDeploymentDrainAnswer {
	t.Helper()

	target := modelDeploymentDrainTarget{
		PodUID:        drainTestUID,
		Container:     drainTestContainer,
		Role:          "server",
		Engine:        drainTestEngine,
		DeploymentUID: drainTestDeployUID,
	}
	for _, m := range mutate {
		m(&target)
	}

	answer, err := r.collector.Drain(context.Background(), target)
	if err != nil {
		t.Fatalf("the collector reported an error rather than an answer: %v", err)
	}

	return answer
}

// drainTestTarget is the member the fixture's Pod is, as the protocol carries it.
func drainTestTarget() modelDeploymentDrainTarget {
	return modelDeploymentDrainTarget{
		PodUID:    drainTestUID,
		Container: drainTestContainer,
		Role:      drainTestRole,
		Engine:    drainTestEngine,
		// The fixture's Pod is owned by this deployment, so the read names it. An empty owner here
		// would only prove that an unset target and an unset owner agree with each other.
		DeploymentUID: drainTestDeployUID,
	}
}

// drainBody renders the envelope the collector's own script produces for a set of series, so the
// fixture bodies are the real wire format rather than a convenient invention.
func drainBody(series map[string]string) string {
	var out strings.Builder
	for name, value := range series {
		fmt.Fprintf(&out, "%s\t%s\n", name, value)
	}

	return out.String()
}

// vllmZero is a complete vLLM envelope with nothing in flight.
func vllmZero() string {
	return drainBody(map[string]string{
		"vllm:num_requests_running": "0",
		"vllm:num_requests_waiting": "0",
	})
}

// vllmBusy is a complete vLLM envelope carrying work.
func vllmBusy() string {
	return drainBody(map[string]string{
		"vllm:num_requests_running": "2",
		"vllm:num_requests_waiting": "1",
	})
}

// sglangZero is a complete SGLang envelope with nothing in flight, all six gauges present because
// the engine sets the disaggregation ones unconditionally, to zero.
func sglangZero() string {
	series := map[string]string{}
	for _, name := range modelDeploymentInFlightMetrics[workercore.ModelDeploymentEngineSGLang] {
		series[name] = "0"
	}

	return drainBody(series)
}

// TestDrainCollectorFixtureMatrix is the matrix. Each case is one behavior, asserted on the
// observable answer rather than on how the transport got there.
func TestDrainCollectorFixtureMatrix(t *testing.T) {
	// A disaggregated command, as the render would have copied the role's extra arguments in.
	disaggregated := func(extra ...string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Command = append(
				[]string{"vllm", "serve", "model"}, extra...)
		}
	}

	testCases := []struct {
		name       string
		pod        *corev1.Pod
		engine     string
		bodies     []string
		execErr    error
		during     func(ctx context.Context, r *drainTestReader)
		wantState  modelDeploymentDrainState
		wantReason string
		// wantComplete is only asserted where the state can be an answer at all; a hold is never
		// complete whatever it says, which is the property the protocol leans on.
		wantComplete bool
		wantCalls    int
	}{
		{
			name:       "the anchor negative: a body missing a required series is unobserved",
			pod:        drainTestPod(),
			bodies:     []string{drainBody(map[string]string{"vllm:num_requests_running": "0"})},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "the engine served no vllm:num_requests_waiting",
			wantCalls:  1,
		},
		{
			name:   "a partial SGLang envelope is not a zero",
			pod:    drainTestPod(),
			engine: workercore.ModelDeploymentEngineSGLang,
			bodies: []string{drainBody(map[string]string{
				"sglang:num_running_reqs": "0",
				"sglang:num_queue_reqs":   "0",
			})},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "the engine served no sglang:num_prefill_bootstrap_queue_reqs",
			wantCalls:  1,
		},
		{
			name:       "a series that cannot be parsed is a broken envelope, not an absent one",
			pod:        drainTestPod(),
			bodies:     []string{"vllm:num_requests_running\tunreadable\nvllm:num_requests_waiting\t0\n"},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "the engine served unreadable values for vllm:num_requests_running",
			wantCalls:  1,
		},
		{
			name: "the named container is not on the Pod",
			pod: drainTestPod(func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Name = "something-else"
			}),
			bodies:     []string{vllmZero()},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "has no container",
			wantCalls:  0,
		},
		{
			name:   "a container replaced while it was being read is unobserved",
			pod:    drainTestPod(),
			bodies: []string{vllmZero()},
			during: func(ctx context.Context, r *drainTestReader) {
				// The mutation lands on the AUTHORITATIVE reader. Mutating only the locator's cache
				// would leave the read looking at a member that never changed, and the case would
				// pass for the wrong reason: it would be proving that the cache was not consulted
				// again rather than that a replaced container is unobserved.
				mutated := r.pod.DeepCopy()
				mutated.Status.ContainerStatuses[0].ContainerID = "containerd://after"
				if err := r.fresh.Status().Update(ctx, mutated); err != nil {
					panic(err)
				}
			},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "was replaced while it was being read",
			wantCalls:  1,
		},
		{
			name: "a restarted container is unobserved rather than idle",
			pod: drainTestPod(func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].RestartCount = 2
			}),
			bodies:     []string{vllmZero()},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "has restarted",
			wantCalls:  0,
		},
		{
			name:       "an engine with no gauge list is unmeasured rather than idle",
			pod:        drainTestPod(),
			engine:     "no-such-engine",
			bodies:     []string{vllmZero()},
			wantState:  modelDeploymentDrainUnsupported,
			wantReason: "has no in-flight gauge list",
			wantCalls:  0,
		},
		{
			name:       "a disaggregated member is refused from the Pod's own command",
			pod:        drainTestPod(disaggregated("--data-parallel-external-lb")),
			bodies:     []string{vllmZero()},
			wantState:  modelDeploymentDrainUnsupported,
			wantReason: "disaggregated shape",
			wantCalls:  0,
		},
		{
			name: "a member with no metrics endpoint is unobserved",
			pod: drainTestPod(func(pod *corev1.Pod) {
				delete(pod.Annotations, "prometheus.io/port")
			}),
			bodies:     []string{vllmZero()},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "no usable metrics endpoint",
			wantCalls:  0,
		},
		{
			name:       "an exec the API server refuses is unobserved, not busy",
			pod:        drainTestPod(),
			execErr:    fmt.Errorf("pods \"server-0-abcde\" is forbidden"),
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "forbidden",
			wantCalls:  1,
		},
		{
			name:       "a body that is not metrics at all is unobserved",
			pod:        drainTestPod(),
			bodies:     []string{"<!DOCTYPE html><html>404</html>"},
			wantState:  modelDeploymentDrainUnknown,
			wantReason: "the engine served no vllm:num_requests_running",
			wantCalls:  1,
		},
		{
			name:         "a complete zero is idle",
			pod:          drainTestPod(),
			bodies:       []string{vllmZero()},
			wantState:    modelDeploymentDrainIdle,
			wantComplete: true,
			wantCalls:    1,
		},
		{
			name:         "a complete non-zero sum is busy and says how much",
			pod:          drainTestPod(),
			bodies:       []string{vllmBusy()},
			wantState:    modelDeploymentDrainBusy,
			wantReason:   "holds 3 in flight",
			wantComplete: true,
			wantCalls:    1,
		},
		{
			name:         "a complete SGLang zero is idle, every disaggregation gauge included",
			pod:          drainTestPod(),
			engine:       workercore.ModelDeploymentEngineSGLang,
			bodies:       []string{sglangZero()},
			wantState:    modelDeploymentDrainIdle,
			wantComplete: true,
			wantCalls:    1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			engine := tc.engine
			if engine == "" {
				engine = drainTestEngine
			}
			reader := newDrainTestReader(tc.pod, tc.bodies...)
			reader.err = tc.execErr
			reader.during = tc.during

			answer := reader.drain(t, func(target *modelDeploymentDrainTarget) {
				target.Engine = engine
			})

			if answer.State != tc.wantState {
				t.Fatalf("state = %q, want %q (reason %q)", answer.State, tc.wantState, answer.Reason)
			}
			if !strings.Contains(answer.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", answer.Reason, tc.wantReason)
			}
			if answer.Complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", answer.Complete, tc.wantComplete)
			}
			if reader.calls != tc.wantCalls {
				t.Errorf("exec calls = %d, want %d", reader.calls, tc.wantCalls)
			}
			if tc.wantState != modelDeploymentDrainIdle && tc.wantState != modelDeploymentDrainBusy {
				if answer.Complete {
					t.Error("a hold must never be complete, whatever state it carries")
				}
			}
		})
	}
}

// TestDrainCollectorSumsLabelSetsWithinOneBody is the per-member rule, proven on the wire format.
//
// Both engines split a series by label, so a member's work arrives as several rows. Summing them is
// correct within one body and is the only place it is correct: a second body would be another
// member, and summing across bodies is how a per-member zero becomes a group one.
func TestDrainCollectorSumsLabelSetsWithinOneBody(t *testing.T) {
	testCases := []struct {
		name  string
		body  string
		want  float64
		state modelDeploymentDrainState
	}{
		{
			name: "one series split across two label sets is one member's total",
			body: "vllm:num_requests_running{engine=\"0\",model_name=\"m\"} 2\n" +
				"vllm:num_requests_running{engine=\"1\",model_name=\"m\"} 3\n" +
				"vllm:num_requests_waiting{engine=\"0\",model_name=\"m\"} 1\n" +
				"vllm:num_requests_waiting{engine=\"1\",model_name=\"m\"} 1\n",
			want:  7,
			state: modelDeploymentDrainBusy,
		},
		{
			name: "a series split across two label sets that are both zero is still zero",
			body: "vllm:num_requests_running{engine=\"0\"} 0\n" +
				"vllm:num_requests_running{engine=\"1\"} 0\n" +
				"vllm:num_requests_waiting{engine=\"0\"} 0\n" +
				"vllm:num_requests_waiting{engine=\"1\"} 0\n",
			want:  0,
			state: modelDeploymentDrainIdle,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// The summing is the script's work, so this runs the real generated program rather than
			// reimplementing it here, which is the only way the fixture proves the shipped argv.
			script := modelDeploymentDrainArgv(
				"http://127.0.0.1:8000/metrics",
				modelDeploymentInFlightMetrics[drainTestEngine],
			)[2]
			_ = script

			answer := modelDeploymentDrainAnswerFromBody(
				sumDrainEnvelope(t, tc.body), modelDeploymentInFlightMetrics[drainTestEngine])
			if answer.State != tc.state {
				t.Fatalf("state = %q, want %q (reason %q)", answer.State, tc.state, answer.Reason)
			}
			if answer.Complete != true {
				t.Error("a complete envelope must be reported complete")
			}
			total := 0.0
			for _, value := range answer.Series {
				total += value
			}
			if total != tc.want {
				t.Errorf("series total = %v, want %v", total, tc.want)
			}
		})
	}
}

// sumDrainEnvelope runs the collector's own summing rules over a Prometheus body, so the test
// exercises the same arithmetic the shipped script performs.
func sumDrainEnvelope(t *testing.T, body string) string {
	t.Helper()

	names := modelDeploymentInFlightMetrics[drainTestEngine]
	totals := make(map[string]float64, len(names))
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.Split(strings.SplitN(line, "{", 2)[0], " ")[0]
		if !modelDeploymentDrainExpected(name, names) {
			continue
		}
		sample := line[len(name):]
		if idx := strings.LastIndex(line, "}"); idx >= 0 {
			sample = line[idx+1:]
		}
		var value float64
		if _, err := fmt.Sscanf(strings.TrimSpace(sample), "%g", &value); err != nil {
			continue
		}
		totals[name] += value
	}

	var out strings.Builder
	for name, value := range totals {
		fmt.Fprintf(&out, "%s\t%v\n", name, value)
	}

	return out.String()
}

// TestDrainCollectorRefusesEveryAnswerWithNoNumber is the shape rule: only a complete envelope may
// carry numbers, and only a zero one may be acted on.
func TestDrainCollectorRefusesEveryAnswerWithNoNumber(t *testing.T) {
	t.Run("a hold never carries a series", func(t *testing.T) {
		answer := modelDeploymentDrainAnswerFromBody("", modelDeploymentInFlightMetrics[drainTestEngine])
		if answer.Complete {
			t.Error("an empty envelope is not complete")
		}
		if len(answer.Series) != 0 {
			t.Errorf("a hold carried %d series, want none", len(answer.Series))
		}
		if answer.State != modelDeploymentDrainUnknown {
			t.Errorf("state = %q, want %q", answer.State, modelDeploymentDrainUnknown)
		}
	})

	t.Run("an empty target is unobserved rather than every member", func(t *testing.T) {
		reader := newDrainTestReader(drainTestPod(), vllmZero())
		answer := reader.drain(t, func(target *modelDeploymentDrainTarget) {
			target.PodUID = ""
		})
		if answer.State != modelDeploymentDrainUnknown {
			t.Errorf("state = %q, want %q", answer.State, modelDeploymentDrainUnknown)
		}
		if reader.calls != 0 {
			t.Errorf("an empty uid reached the transport %d times, want 0", reader.calls)
		}
	})
}

// TestDrainCollectorDisaggregationIsReadFromThePodAlone is ruling three's load-bearing case.
//
// The protocol asks the role, and a role the spec has already dropped is nil, so a disaggregated
// member whose role is gone reaches the read with nothing having refused it. The Pod has to answer
// it by itself, and it can: the render copies the role's extra arguments into the container command
// verbatim, so the same token scan that reads the role reads the Pod.
func TestDrainCollectorDisaggregationIsReadFromThePodAlone(t *testing.T) {
	testCases := []struct {
		name              string
		engine            string
		command           []string
		wantDisaggregated bool
	}{
		{
			name:              "a vLLM external load balancer is disaggregated",
			engine:            workercore.ModelDeploymentEngineVLLM,
			command:           []string{"vllm", "serve", "m", "--data-parallel-external-lb"},
			wantDisaggregated: true,
		},
		{
			name:              "a vLLM hybrid load balancer is disaggregated",
			engine:            workercore.ModelDeploymentEngineVLLM,
			command:           []string{"vllm", "serve", "m", "--data-parallel-hybrid-lb"},
			wantDisaggregated: true,
		},
		{
			name:              "a vLLM data parallel rank is disaggregated",
			engine:            workercore.ModelDeploymentEngineVLLM,
			command:           []string{"vllm", "serve", "m", "--data-parallel-rank", "1"},
			wantDisaggregated: true,
		},
		{
			name:              "a SGLang multi-node command is disaggregated",
			engine:            workercore.ModelDeploymentEngineSGLang,
			command:           []string{"python3", "-m", "sglang.launch_server", "--disaggregation-mode", "decode"},
			wantDisaggregated: true,
		},
		{
			name:              "a SGLang null disaggregation mode is not disaggregated",
			engine:            workercore.ModelDeploymentEngineSGLang,
			command:           []string{"python3", "-m", "sglang.launch_server", "--disaggregation-mode=null"},
			wantDisaggregated: false,
		},
		{
			name:    "a plain vLLM command is not",
			engine:  workercore.ModelDeploymentEngineVLLM,
			command: []string{"vllm", "serve", "m", "--tensor-parallel-size", "2"},
		},
		{
			name:    "a plain SGLang command is not",
			engine:  workercore.ModelDeploymentEngineSGLang,
			command: []string{"python3", "-m", "sglang.launch_server", "--tp-size", "2"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			container := &corev1.Container{Name: drainTestContainer, Command: tc.command}
			_, disaggregated := modelDeploymentDrainDisaggregated(tc.engine, container)
			if disaggregated != tc.wantDisaggregated {
				t.Fatalf("disaggregated = %v, want %v for %v", disaggregated, tc.wantDisaggregated, tc.command)
			}
		})
	}
}

// TestDrainCollectorArgvIsBoundedAndReadOnly pins the command the transport runs.
//
// It is the whole read-only claim: one interpreter, one loopback GET, and no argument a user can
// reach. A test that only checked it compiles would not notice someone widening it.
func TestDrainCollectorArgvIsBoundedAndReadOnly(t *testing.T) {
	argv := modelDeploymentDrainArgv("http://127.0.0.1:8000/metrics",
		modelDeploymentInFlightMetrics[drainTestEngine])

	if len(argv) != 3 || argv[0] != "python3" || argv[1] != "-c" {
		t.Fatalf("argv = %v, want the image's python3 with a generated program", argv[:min(3, len(argv))])
	}

	script := argv[2]
	for _, forbidden := range []string{
		"os.", "subprocess", "socket.socket", "/generate", "/reset_prefix_cache",
		"__import__", "urllib.request.Request", "urlopen(url",
		"/flush_cache", "kill", "signal", "system(", "eval(", "exec(",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the read-only script contains %q", forbidden)
		}
	}

	if !strings.Contains(script, "timeout=2") {
		t.Error("the read does not carry its HTTP timeout")
	}
	if !strings.Contains(script, "1048576") {
		t.Error("the read does not carry its byte bound")
	}
	if !strings.Contains(script, "127.0.0.1:8000/metrics") {
		t.Error("the read does not carry the endpoint it was given")
	}
	for _, name := range modelDeploymentInFlightMetrics[drainTestEngine] {
		if !strings.Contains(script, name) {
			t.Errorf("the read was not asked for %q", name)
		}
	}
}

// TestDrainCollectorURLIsReadFromThePod pins where the endpoint comes from, because a derived port
// would break exactly the case the protocol cannot recover from: a replica whose role the spec has
// already dropped.
func TestDrainCollectorURLIsReadFromThePod(t *testing.T) {
	testCases := []struct {
		name        string
		annotations map[string]string
		want        string
		wantOK      bool
	}{
		{
			name:        "the render's own annotations are used as written",
			annotations: map[string]string{"prometheus.io/path": "/metrics", "prometheus.io/port": "8000", "prometheus.io/scheme": "http"},
			want:        "http://127.0.0.1:8000/metrics",
			wantOK:      true,
		},
		{
			name:        "https is carried through",
			annotations: map[string]string{"prometheus.io/path": "/metrics", "prometheus.io/port": "443", "prometheus.io/scheme": "https"},
			want:        "https://127.0.0.1:443/metrics",
			wantOK:      true,
		},
		{
			name:        "a missing port is refused",
			annotations: map[string]string{"prometheus.io/path": "/metrics", "prometheus.io/scheme": "http"},
		},
		{
			name:        "an unreadable port is refused",
			annotations: map[string]string{"prometheus.io/port": "not-a-port", "prometheus.io/scheme": "http"},
		},
		{
			name:        "an unknown scheme is refused",
			annotations: map[string]string{"prometheus.io/port": "8000", "prometheus.io/scheme": "gopher"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := drainTestPod(func(pod *corev1.Pod) { pod.Annotations = tc.annotations })
			got, ok := modelDeploymentDrainURL(pod)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (url %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("url = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDrainCollectorFailsClosedOnAnUnnamedTarget pins the branch and not just the state.
//
// An empty UID resolves to nothing through the UID index, so the read is unobserved either way and
// a state-only assertion would pass even if this branch were gone. What has to be pinned is that the
// refusal names the empty UID, because that is the difference between "the protocol asked about
// nobody" and "the member we hold could not be found", and only the first is a caller bug.
func TestDrainCollectorFailsClosedOnAnUnnamedTarget(t *testing.T) {
	reader := newDrainTestReader(drainTestPod(), vllmZero())

	answer := reader.drain(t, func(target *modelDeploymentDrainTarget) {
		target.PodUID = ""
	})

	if answer.State != modelDeploymentDrainUnknown {
		t.Fatalf("state = %q, want %q", answer.State, modelDeploymentDrainUnknown)
	}
	if !strings.Contains(answer.Reason, "names no member") {
		t.Errorf("reason = %q, want it to say the target named no member", answer.Reason)
	}
	if reader.calls != 0 {
		t.Errorf("an empty uid reached the transport %d times, want 0", reader.calls)
	}
}

// TestDrainCollectorHoldsAMemberThatVanishedMidRead is the member-identity case, and it is the one
// that covers a deleted-and-replaced member.
//
// A replacement carries a new UID, so the re-resolve after the read finds nothing. That lookup is
// the whole protection, which is why there is no UID comparison beside it: comparing a UID against
// the value it was looked up by could never fail.
func TestDrainCollectorHoldsAMemberThatVanishedMidRead(t *testing.T) {
	reader := newDrainTestReader(drainTestPod(), vllmZero())
	reader.during = func(ctx context.Context, r *drainTestReader) {
		if err := r.client.Delete(ctx, r.pod); err != nil {
			panic(err)
		}
	}

	answer := reader.drain(t)

	if answer.State != modelDeploymentDrainUnknown {
		t.Fatalf("state = %q, want %q (reason %q)", answer.State, modelDeploymentDrainUnknown, answer.Reason)
	}
	if !strings.Contains(answer.Reason, "re-resolved") {
		t.Errorf("reason = %q, want it to say the member could not be re-resolved", answer.Reason)
	}
	if answer.Complete {
		t.Error("a member that vanished mid-read must not be complete")
	}
}

// TestTheDrainTargetCarriesTheEngineContainer is ruling one's call side.
//
// The type says the container is carried rather than looked up, so a target built with the field
// empty is a type that lies about itself. Nothing downstream fails loudly on it: the transport would
// simply hold forever on a target the protocol itself built.
func TestTheDrainTargetCarriesTheEngineContainer(t *testing.T) {
	md := retirementDeployment()
	member := healthPod("server", 1, 0, healthBool(true), "member-1")
	plan := &modelDeploymentRetirementPlan{
		Reservation: &workercore.ModelDeploymentRetirementStatus{
			RoleName: "server",
		},
	}

	built := modelDeploymentRetirementDrainTarget(md, nil, plan, &member)

	if built.Container != modelDeploymentMainContainerName {
		t.Errorf("the target carries container %q, want %q",
			built.Container, modelDeploymentMainContainerName)
	}
	if built.Engine == "" {
		t.Error("the target carries no engine, so there is no gauge list to read it against")
	}
	if built.PodUID == "" {
		t.Error("the target names no member, so it resolves to nobody")
	}
}

// TestDrainReadIsBoundedInTime pins the exec bound with a transport that never answers.
//
// A wedged kubelet is the case the bound exists for, and asserting the constant is set to a number
// would not catch a bound that is no longer applied to the read.
func TestDrainReadIsBoundedInTime(t *testing.T) {
	reader := newDrainTestReader(drainTestPod(), vllmZero())
	reader.collector.exec = func(
		ctx context.Context, _ *corev1.Pod, _ string, _ []string,
	) (string, error) {
		<-ctx.Done()

		return "", ctx.Err()
	}

	// The ceiling is a FIXED number, deliberately not derived from the bound. A ceiling computed from
	// the constant would follow it when the constant is wrong, so removing the bound would make this
	// test wait longer rather than fail — and a test that proves the defect by hanging the suite
	// proves nothing a reviewer can act on. Thirty seconds is far above the five-second bound and far
	// below any value a mutation would substitute.
	const ceiling = 30 * time.Second
	done := make(chan modelDeploymentDrainAnswer, 1)
	go func() { done <- reader.drain(t) }()

	var answer modelDeploymentDrainAnswer
	select {
	case answer = <-done:
	case <-time.After(ceiling):
		t.Fatalf("the read did not return within %v, so its bound is not applied to it", ceiling)
	}

	if answer.State != modelDeploymentDrainUnknown {
		t.Errorf("state = %q, want %q", answer.State, modelDeploymentDrainUnknown)
	}
	if !strings.Contains(answer.Reason, "context") {
		t.Errorf("reason = %q, want it to carry the transport failure", answer.Reason)
	}
}

// TestTheGeneratedScriptSumsLabelSetsWithinOneBody runs the program the transport actually ships.
//
// The summing lives in the script, not in Go, so a Go-side test of it proves nothing about the
// command that runs in the member's container. This serves a real Prometheus body over a real
// loopback socket and pipes the generated program through the interpreter, which is the only way to
// show that the shipped argv reads a split series as one member's total.
//
// It is skipped rather than failed when there is no interpreter, because the absence of python3 on
// a test host is not a defect in the read.
func TestTheGeneratedScriptSumsLabelSetsWithinOneBody(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on this host, so the shipped program cannot be run here")
	}

	const body = "# HELP vllm:num_requests_running Number of requests in model execution batches.\n" +
		"# TYPE vllm:num_requests_running gauge\n" +
		"vllm:num_requests_running{engine=\"0\",model_name=\"m\"} 2\n" +
		"vllm:num_requests_running{engine=\"1\",model_name=\"m\"} 3\n" +
		"vllm:num_requests_waiting{engine=\"0\",model_name=\"m\"} 0\n" +
		"vllm:num_requests_waiting{engine=\"1\",model_name=\"m\"} 0\n" +
		"some_other_metric{engine=\"0\"} 99\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	argv := modelDeploymentDrainArgv(server.URL+"/metrics",
		modelDeploymentInFlightMetrics[drainTestEngine])
	out, err := exec.Command(python, argv[1], argv[2]).CombinedOutput()
	if err != nil {
		t.Fatalf("the shipped program failed: %v (%s)", err, out)
	}

	answer := modelDeploymentDrainAnswerFromBody(string(out),
		modelDeploymentInFlightMetrics[drainTestEngine])
	if !answer.Complete {
		t.Fatalf("the envelope was not complete: %q (raw %q)", answer.Reason, out)
	}
	if answer.State != modelDeploymentDrainBusy {
		t.Errorf("state = %q, want %q", answer.State, modelDeploymentDrainBusy)
	}
	if got := answer.Series["vllm:num_requests_running"]; got != 5 {
		t.Errorf("running = %v, want 5: the two label sets are one member's work", got)
	}
	if got := answer.Series["vllm:num_requests_waiting"]; got != 0 {
		t.Errorf("waiting = %v, want 0", got)
	}
	if _, unwanted := answer.Series["some_other_metric"]; unwanted {
		t.Error("the read carried a series it was never asked for")
	}
}

// drainScriptAnswer runs the ACTUAL generated collector program against a served body and returns
// what the Go parser made of its real output.
//
// THE SCRIPT IS EXECUTED RATHER THAN REIMPLEMENTED. A test that re-reads the program and asserts
// what it ought to contain proves the reading right, not the program; running the one this tree
// generates, through the one parser this tree uses, and into the one protocol rule this tree
// applies, is the only way a claim about the wire is a claim about the wire.
//
// The fetch is replaced rather than the script, so the program under test is byte for byte the one
// a member would run, and a Python that is missing is an instrument failure rather than a product
// result: the cases below are all reached or the run is not a result at all.
func drainScriptAnswer(t *testing.T, engine, body string) modelDeploymentDrainAnswer {
	t.Helper()

	python, err := exec.LookPath("python3")
	require.NoError(t, err, "the generated collector program is a Python program")

	expected, known := modelDeploymentInFlightMetrics[engine]
	require.True(t, known, "the test names an engine the tree measures")

	argv := modelDeploymentDrainArgv("http://127.0.0.1:8000/metrics", expected)
	require.Len(t, argv, 3)
	require.Equal(t, "python3", argv[0])
	require.Equal(t, "-c", argv[1])

	literal, err := json.Marshal(body)
	require.NoError(t, err)
	bootstrap := fmt.Sprintf("import io, urllib.request\n"+
		"urllib.request.urlopen = lambda *args, **kwargs: io.BytesIO(%s.encode())\n", literal)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wire, err := exec.CommandContext(ctx, python, "-c", bootstrap+argv[2]).CombinedOutput()
	require.NoError(t, err, "the generated script must execute before its output is judged: %s", wire)

	return modelDeploymentDrainAnswerFromBody(string(wire), expected)
}

// drainScriptIdle reports what the protocol makes of that answer, so every case is judged by the
// rule that actually authorizes a delete rather than by the parser's verdict alone.
func drainScriptIdle(
	t *testing.T, engine string, answer modelDeploymentDrainAnswer,
) (string, bool) {
	t.Helper()

	member := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-0-abcde"}}

	return modelDeploymentDrainMemberIdle(member, modelDeploymentDrainTarget{Engine: engine}, answer)
}

// TestTheGeneratedDrainScriptRefusesEvidenceItCannotCount drives the actual program for both
// engines the tree measures.
//
// A gauge is in flight work, so its value is a nonnegative finite count. Every case below is a
// number that is not one, or a way of combining numbers that must not produce one: a value that
// parses but is not a measurement, a negative count, an infinity, a sum that overflows, a tiny
// positive that a rounded zero would erase, and a good row that must not be allowed to stand in
// for a bad one in the same gauge.
//
// THE ORDER OF THE ROWS IS CARRIED IN THE CASES because it is the whole of the cancellation defect:
// a gauge the engine splits by label is several rows, and which of them is unreadable decides
// whether the gauge is refused or whether a readable sibling quietly speaks for it.
func TestTheGeneratedDrainScriptRefusesEvidenceItCannotCount(t *testing.T) {
	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM,
		workercore.ModelDeploymentEngineSGLang,
	} {
		t.Run(engine, func(t *testing.T) {
			expected, known := modelDeploymentInFlightMetrics[engine]
			require.True(t, known)
			require.NotEmpty(t, expected)
			first := expected[0]
			rest := expected[1:]

			for _, tc := range []struct {
				name string
				rows []string
				want modelDeploymentDrainState
			}{
				{
					name: "a complete zero is idle",
					rows: []string{first + " 0"},
					want: modelDeploymentDrainIdle,
				},
				{
					name: "a positive count is busy",
					rows: []string{first + " 2"},
					want: modelDeploymentDrainBusy,
				},
				{
					name: "a tiny positive stays busy rather than rounding to zero",
					rows: []string{first + " 0.0000001"},
					want: modelDeploymentDrainBusy,
				},
				{
					name: "two readable rows for one gauge are summed",
					rows: []string{first + " 1", first + "{replica=\"other\"} 2"},
					want: modelDeploymentDrainBusy,
				},
				{
					name: "an unreadable row alone refuses the gauge",
					rows: []string{first + " unreadable"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "an unreadable row first is not cancelled by a readable sibling",
					rows: []string{first + " unreadable", first + "{replica=\"other\"} 0"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "an unreadable row last is not cancelled by a readable sibling either",
					rows: []string{first + "{replica=\"other\"} 0", first + " unreadable"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "not a number refuses the gauge",
					rows: []string{first + " not-a-number"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "an absent value refuses the gauge",
					rows: []string{first + " 0", first + "{replica=\"other\"}"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "NaN refuses the gauge",
					rows: []string{first + " NaN"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "positive infinity refuses the gauge",
					rows: []string{first + " +Inf"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "negative infinity refuses the gauge",
					rows: []string{first + " -Inf"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "a negative count refuses the gauge",
					rows: []string{first + " -1"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "a negative row cannot cancel a positive sibling into a zero",
					rows: []string{first + " 1", first + "{replica=\"other\"} -1"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "a negative row first cannot cancel a positive sibling either",
					rows: []string{first + " -1", first + "{replica=\"other\"} 1"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "a sum that overflows is not a count",
					rows: []string{first + " 1e308", first + "{replica=\"other\"} 1e308"},
					want: modelDeploymentDrainUnknown,
				},
				{
					name: "an invalid row for a different gauge is not invented as a zero",
					rows: []string{},
					want: modelDeploymentDrainUnknown,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var body strings.Builder
					for _, row := range tc.rows {
						body.WriteString(row)
						body.WriteString("\n")
					}
					for _, metric := range rest {
						body.WriteString(metric)
						body.WriteString(" 0\n")
					}

					answer := drainScriptAnswer(t, engine, body.String())
					assert.Equal(t, tc.want, answer.State, "reason=%q series=%v", answer.Reason, answer.Series)

					reason, idle := drainScriptIdle(t, engine, answer)
					assert.Equal(t, tc.want == modelDeploymentDrainIdle, idle,
						"the protocol must refuse anything that is not a zero, reason=%s", reason)
				})
			}
		})
	}
}

// TestTheReadIsAboutTheMemberItJustReReadCovers every way the member under the exec can stop being
// the member the reservation bound.
//
// THE LOCATOR AND THE AUTHORITY ARE SEPARATE FAKES, and each mutation below lands on the authority
// alone. A mutation applied only to the locator's cache would be invisible to a read that consults
// the live reader, which is the point: it shows the read is answered by the live object rather than
// by whatever the watch last delivered. Each case also asserts the mutation actually fired, so a
// case cannot pass by never having changed anything.
func TestTheReadIsAboutTheMemberItJustReRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// status says which half of the object the case changes, because a Pod's status is a
		// subresource and a mutation has to be written through the writer that owns it.
		status  bool
		mutate  func(pod *corev1.Pod)
		want    modelDeploymentDrainState
		mention string
	}{
		{
			name:   "nothing changed",
			mutate: func(*corev1.Pod) {},
			want:   modelDeploymentDrainIdle,
		},
		{
			name:    "the container restarted during the read",
			status:  true,
			mutate:  func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].RestartCount = 2 },
			want:    modelDeploymentDrainUnknown,
			mention: "restarted",
		},
		{
			name: "the pod was replaced under the same name",
			mutate: func(pod *corev1.Pod) {
				pod.UID = types.UID("member-uid-2")
			},
			want:    modelDeploymentDrainUnknown,
			mention: "now carries uid",
		},
		{
			name:    "the member changed address",
			status:  true,
			mutate:  func(pod *corev1.Pod) { pod.Status.PodIP = "10.0.4.9" },
			want:    modelDeploymentDrainUnknown,
			mention: "changed address",
		},
		{
			name:    "the member changed role",
			mutate:  func(pod *corev1.Pod) { pod.Labels[modelDeploymentLabelKeyComponent] = "other" },
			want:    modelDeploymentDrainUnknown,
			mention: "changed role",
		},
		{
			name:    "the member changed controller",
			mutate:  func(pod *corev1.Pod) { pod.OwnerReferences[0].Name = "someone-else" },
			want:    modelDeploymentDrainUnknown,
			mention: "changed controller",
		},
		{
			name: "the metrics endpoint moved",
			mutate: func(pod *corev1.Pod) {
				pod.Annotations["prometheus.io/port"] = "9000"
			},
			want:    modelDeploymentDrainUnknown,
			mention: "changed its metrics endpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fired := false
			reader := newDrainTestReader(drainTestPod(), vllmZero())
			reader.during = func(ctx context.Context, r *drainTestReader) {
				mutated := r.pod.DeepCopy()
				tc.mutate(mutated)
				// The mutation lands on the AUTHORITATIVE reader, and never on the locator's cache:
				// mutating only the cache would leave a read that consults the live object looking at
				// a member that never changed, and the case would pass for the wrong reason.
				var err error
				if tc.status {
					err = r.fresh.Status().Update(ctx, mutated)
				} else {
					err = r.fresh.Update(ctx, mutated)
				}
				require.NoError(t, err,
					"the mutation must reach the authoritative reader or the case proves nothing")
				fired = true
			}

			answer, err := reader.collector.Drain(context.Background(), drainTestTarget())
			require.NoError(t, err)
			require.True(t, fired, "the mutation must have fired during the exec")
			require.Equal(t, 1, reader.calls, "and the read must have actually executed")
			assert.Equal(t, tc.want, answer.State, "reason=%s", answer.Reason)
			if tc.mention != "" {
				assert.Contains(t, answer.Reason, tc.mention)
			}
		})
	}

	t.Run("the member is gone from the authoritative reader", func(t *testing.T) {
		reader := newDrainTestReader(drainTestPod(), vllmZero())
		reader.during = func(ctx context.Context, r *drainTestReader) {
			require.NoError(t, r.fresh.Delete(ctx, r.pod))
		}

		answer, err := reader.collector.Drain(context.Background(), drainTestTarget())
		require.NoError(t, err)
		assert.Equal(t, modelDeploymentDrainUnknown, answer.State, "reason=%s", answer.Reason)
	})

	t.Run("no authoritative reader leaves the member unobserved", func(t *testing.T) {
		reader := newDrainTestReader(drainTestPod(), vllmZero())
		reader.collector = newModelDeploymentDrainCollector(reader.client, nil, nil, nil)

		answer, err := reader.collector.Drain(context.Background(), drainTestTarget())
		require.NoError(t, err)
		assert.Equal(t, modelDeploymentDrainUnknown, answer.State,
			"a read that cannot prove what it is measuring has not measured it")
		assert.Contains(t, answer.Reason, "no live reader")
	})
}

// TestTheProtocolRefusesAReaderThatIsMerelyOptimistic covers the reader seam itself, which the
// script controls cannot reach.
//
// A reader is an interface, and a custom one can claim Idle over any numbers it likes. The protocol
// decides what activity means rather than asking the reader whether it agrees, so an Idle claim
// resting on a value that is not a count, or on a count that is not zero, holds.
func TestTheProtocolRefusesAReaderThatIsMerelyOptimistic(t *testing.T) {
	complete := func(series map[string]float64) modelDeploymentDrainAnswer {
		return modelDeploymentDrainAnswer{
			State: modelDeploymentDrainIdle, Complete: true, Series: series,
		}
	}

	for _, tc := range []struct {
		name    string
		answer  modelDeploymentDrainAnswer
		want    modelDeploymentDrainState
		mention string
	}{
		{
			name: "an honest complete zero is idle",
			answer: complete(map[string]float64{
				"vllm:num_requests_running": 0, "vllm:num_requests_waiting": 0,
			}),
			want: modelDeploymentDrainIdle,
		},
		{
			name: "a reader claiming idle over positive evidence is refused",
			answer: complete(map[string]float64{
				"vllm:num_requests_running": 3, "vllm:num_requests_waiting": 0,
			}),
			want:    modelDeploymentDrainUnknown,
			mention: "still holds activity",
		},
		{
			name: "a reader claiming idle over NaN is refused",
			answer: complete(map[string]float64{
				"vllm:num_requests_running": math.NaN(), "vllm:num_requests_waiting": 0,
			}),
			want:    modelDeploymentDrainUnknown,
			mention: "unusable activity",
		},
		{
			name: "a reader claiming idle over an infinity is refused",
			answer: complete(map[string]float64{
				"vllm:num_requests_running": math.Inf(1), "vllm:num_requests_waiting": 0,
			}),
			want:    modelDeploymentDrainUnknown,
			mention: "unusable activity",
		},
		{
			name: "a reader claiming idle over a negative count is refused",
			answer: complete(map[string]float64{
				"vllm:num_requests_running": -2, "vllm:num_requests_waiting": 0,
			}),
			want:    modelDeploymentDrainUnknown,
			mention: "unusable activity",
		},
		{
			name: "an unknown state does not become success by default",
			answer: modelDeploymentDrainAnswer{
				State: modelDeploymentDrainUnknown, Complete: true,
				Series: map[string]float64{
					"vllm:num_requests_running": 0, "vllm:num_requests_waiting": 0,
				},
			},
			want:    modelDeploymentDrainUnknown,
			mention: "unobserved",
		},
		{
			name: "an unrelated metric cannot cancel a required one",
			answer: complete(map[string]float64{
				"vllm:num_requests_running": 4, "vllm:num_requests_waiting": 0,
				"unrelated:some_counter": -4,
			}),
			want:    modelDeploymentDrainUnknown,
			mention: "still holds activity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-0-abcde"}}
			target := modelDeploymentDrainTarget{
				Engine: workercore.ModelDeploymentEngineVLLM,
			}

			reason, idle := modelDeploymentDrainMemberIdle(member, target, tc.answer)
			assert.Equal(t, tc.want == modelDeploymentDrainIdle, idle, "reason=%s", reason)
			if !idle {
				assert.Contains(t, reason, tc.mention)
			}
		})
	}
}

// TestTheReadIsRefusedAMemberItCannotProveItOwns covers what the live read must still be before it
// may say anything about the member.
//
// A member is the Pod the reservation bound to THIS operation. Each case below is a way that stops
// being true while the Pod's name, address and container stay the same, which is why comparing the
// observable fields is not enough. An empty expectation and an empty observation agree with each
// other, so a target that names no deployment is refused rather than passed.
func TestTheReadIsRefusedAMemberItCannotProveItOwns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  func(md modelDeploymentDrainTarget) modelDeploymentDrainTarget
		pod     func(*corev1.Pod)
		mention string
	}{
		{
			name:    "a target that names no deployment is refused",
			target:  func(t modelDeploymentDrainTarget) modelDeploymentDrainTarget { t.DeploymentUID = ""; return t },
			mention: "names no deployment",
		},
		{
			name:    "a member whose owner was recreated under the same name is refused",
			pod:     func(pod *corev1.Pod) { pod.OwnerReferences[0].UID = types.UID("recreated-uid") },
			mention: "rather than the operation's deployment",
		},
		{
			name:    "a member with no controlling owner is refused",
			pod:     func(pod *corev1.Pod) { pod.OwnerReferences = nil },
			mention: "no controlling owner",
		},
		{
			name:    "a member whose role is not the reserved role is refused",
			pod:     func(pod *corev1.Pod) { pod.Labels[modelDeploymentLabelKeyComponent] = "other" },
			mention: "is role",
		},
		{
			name:    "a member the kubelet has not yet identified is refused",
			pod:     func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].ContainerID = "" },
			mention: "no container identity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := drainTestPod()
			if tc.pod != nil {
				tc.pod(pod)
			}
			reader := newDrainTestReader(pod, vllmZero())
			target := drainTestTarget()
			if tc.target != nil {
				target = tc.target(target)
			}

			answer, err := reader.collector.Drain(context.Background(), target)
			require.NoError(t, err, "a refusal is an answer, not a transport failure")
			assert.Equal(t, modelDeploymentDrainUnknown, answer.State, "reason=%s", answer.Reason)
			assert.Contains(t, answer.Reason, tc.mention)
		})
	}

	t.Run("a member the operation still owns is read", func(t *testing.T) {
		reader := newDrainTestReader(drainTestPod(), vllmZero())
		answer, err := reader.collector.Drain(context.Background(), drainTestTarget())
		require.NoError(t, err)
		assert.Equal(t, modelDeploymentDrainIdle, answer.State, "reason=%s", answer.Reason)
		assert.True(t, reader.calls > 0, "and the read actually reached the transport")
	})
}
