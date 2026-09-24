package worker

import (
	"fmt"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const (
	// modelDeploymentTerminationGracePeriodSeconds is the whole time a departing replica gets, from
	// the delete to the kill. It is the Kubernetes default, rendered explicitly because the drain
	// below is budgeted against it: the kubelet starts this countdown BEFORE the preStop hook and
	// sends SIGTERM only once the hook returns, so the hook and the engine's own exit share it.
	//
	// It is a constant and not a field. A longer grace holds the replica's accelerators and quota for
	// longer on every delete, and a rollout replaces one replica per role at a time and waits each
	// departing one out, so the grace multiplies into every rollout. What a longer request needs is a
	// router that can move it, which no supported router does.
	modelDeploymentTerminationGracePeriodSeconds int64 = 30

	// modelDeploymentDrainSettleSeconds is how long the engine keeps serving normally after the
	// delete before the hook starts waiting for it to go idle.
	//
	// EVERY SUPPORTED ROUTER DROPS A REPLICA ON ITS DELETION TIMESTAMP, not on its readiness -- the
	// llm-d endpoint picker, vllm-router and the SGLang gateway all key on the timestamp -- but a
	// router can pick this replica in the instant before it learns of the delete. The settle is the
	// window that request arrives in. Reading the in-flight count at once would find it zero, end the
	// hook, and send that request to an engine that is already shutting down.
	modelDeploymentDrainSettleSeconds int64 = 5

	// modelDeploymentDrainExitSeconds is the part of the grace kept for the engine to exit once the
	// hook returns and SIGTERM arrives. It holds no request: by then the replica has been idle, or
	// the hook gave up on the requests still running.
	modelDeploymentDrainExitSeconds int64 = 5

	// modelDeploymentDrainDeadlineSeconds is the latest the hook returns, counted from its own start.
	//
	// It is DERIVED from the grace rather than set beside it, so the two cannot be set such that the
	// kubelet kills the engine in the middle of the wait. The last read can end up to two seconds past
	// it -- one read timeout and one poll interval -- which the exit reserve absorbs.
	//
	// A REQUEST STILL RUNNING AT THIS POINT IS CUT, and that is the accepted ceiling: vLLM aborts it on
	// SIGTERM, and SGLang drains it on its own until the kill.
	modelDeploymentDrainDeadlineSeconds = modelDeploymentTerminationGracePeriodSeconds -
		modelDeploymentDrainExitSeconds
)

// modelDeploymentInFlightMetrics names, per engine, the gauges whose sum is the work a replica
// still has in flight. They are read off the engine's own /metrics, which both engines serve on
// their API port: vLLM by default, SGLang because the rendered command carries --enable-metrics.
//
// A request a decoder is still waiting to receive KV for is IN these sums, and it is the one a
// naive "running requests" read would miss. vLLM parks it as WAITING_FOR_REMOTE_KVS and counts it
// under num_requests_waiting; SGLang keeps it in its prealloc and transfer queues, which it reports
// as gauges of their own, and a prefiller's bootstrap and in-flight transfer queues are the same on
// the other side of the pair.
var modelDeploymentInFlightMetrics = map[string][]string{
	workercore.ModelDeploymentEngineVLLM: {
		"vllm:num_requests_running",
		"vllm:num_requests_waiting",
	},
	workercore.ModelDeploymentEngineSGLang: {
		"sglang:num_running_reqs",
		"sglang:num_queue_reqs",
		"sglang:num_prefill_bootstrap_queue_reqs",
		"sglang:num_prefill_inflight_queue_reqs",
		"sglang:num_decode_prealloc_queue_reqs",
		"sglang:num_decode_transfer_queue_reqs",
	},
}

// modelDeploymentDrainHook is the preStop hook of a replica's engine container: it keeps the engine
// serving through the settle window, then waits for the engine's in-flight gauges to read zero,
// and returns no later than the deadline, after which the kubelet sends SIGTERM.
//
// THE WAIT HAS TO HAPPEN BEFORE SIGTERM, because neither engine drains well on the signal itself.
// vLLM's --shutdown-timeout defaults to 0, which aborts every running request at once; set higher,
// it drains but refuses every request that arrives meanwhile, including the ones routed to it in
// the instant before the delete. SGLang drains on the signal, but it checks for the signal only
// every five seconds and has no bound of its own. Waiting here, the engine is untouched: late
// requests are served, and the signal arrives only when nothing is left to abort.
//
// It reads the port and scheme the scrape annotations name, which is the engine's own listener: on
// a direct decoder that is the port behind the routing proxy, so the wait counts the engine's work
// and not the proxy's. A role declaring no ports moves that port with its own --port, and the hook
// follows it; a declared port its own --port disagrees with, which only an object stored before
// admission refused that shape can carry, leaves nothing on the declared port, and the hook then
// returns after the settle.
//
// It is an exec of the image's own python3, as the KV cache member's hook is: both engines are
// Python programs, so the interpreter is there. An httpGet hook is not an option, because neither
// engine has a route that answers only once it is idle.
func modelDeploymentDrainHook(engine string, port int32, scheme core.URIScheme) *core.LifecycleHandler {
	url := fmt.Sprintf("%s://127.0.0.1:%d/metrics", strings.ToLower(string(scheme)), port)
	script := modelDeploymentDrainScript(url, modelDeploymentInFlightMetrics[engine],
		modelDeploymentDrainSettleSeconds, modelDeploymentDrainDeadlineSeconds)

	return &core.LifecycleHandler{
		Exec: &core.ExecAction{Command: []string{"python3", "-c", script}},
	}
}

// modelDeploymentDrainScript renders the hook's program. It takes the timings as arguments so a
// test can run the program on a scale of seconds; the hook passes the constants.
//
// THE ENGINE'S ANSWER DECIDES WHAT HAPPENS NEXT, and the cases are not alike:
//   - a refused connection, an HTTP error or a TLS failure means there is nothing this hook can
//     measure -- an engine already gone, still loading, or listening elsewhere -- so it returns;
//   - a read that times out means an engine too busy to answer, which is busy, so it keeps waiting;
//     socket.timeout is matched beside TimeoutError because it is not one before Python 3.10, and
//     engine.version is free-form, so an older image may carry such an interpreter;
//   - a sample it cannot parse counts as busy, since it cannot prove the replica idle.
//
// It returns on the SECOND consecutive idle read, one poll apart. A prefiller's gauges drop the
// moment its prefill is computed, while the decoder's pull of that KV starts just after; the second
// read is the margin for that pull to begin.
//
// It always exits 0. A failing hook only records an event and lets the kubelet go on to SIGTERM, so
// a non-zero exit would add noise and change nothing.
func modelDeploymentDrainScript(url string, metrics []string, settleSeconds, deadlineSeconds int64) string {
	names := make([]string, 0, len(metrics))
	for _, m := range metrics {
		names = append(names, strconv.Quote(m))
	}

	// The values are operator constants and an integer port, quoted by strconv. For the ASCII they
	// carry, a Go-quoted string is also a valid Python string literal.
	return fmt.Sprintf(`import socket, ssl, sys, time, urllib.request
start = time.monotonic()
time.sleep(%[1]d)
names = [%[3]s]
context = ssl.create_default_context()
context.check_hostname = False
context.verify_mode = ssl.CERT_NONE
idle = 0
while time.monotonic() - start < %[2]d:
    try:
        body = urllib.request.urlopen(%[4]s, timeout=1, context=context).read().decode()
    except Exception as err:
        if not isinstance(getattr(err, "reason", err), (TimeoutError, socket.timeout)):
            sys.exit(0)
        body = None
    busy = 1.0 if body is None else 0.0
    for line in (body or "").splitlines():
        name = line.split("{", 1)[0].split(" ", 1)[0]
        if name not in names:
            continue
        sample = line.rsplit("}", 1)[1] if "{" in line else line[len(name):]
        try:
            busy += float(sample.split()[0])
        except (IndexError, ValueError):
            busy += 1.0
    idle = idle + 1 if busy == 0 else 0
    if idle == 2:
        sys.exit(0)
    time.sleep(1)
sys.exit(0)
`, settleSeconds, deadlineSeconds, strings.Join(names, ", "), strconv.Quote(url))
}
