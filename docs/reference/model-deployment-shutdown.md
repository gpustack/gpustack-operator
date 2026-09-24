# Model Deployment Shutdown Reference

> **Purpose** — what a `ModelDeployment` replica does between its Pod's delete and its engine's exit,
> and which requests that window does not save.
> **Audience** users, operators · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** ~4 min

## Contents

- [The drain window](#the-drain-window)
- [What the window does not cover](#what-the-window-does-not-cover)

## The drain window

A replica whose command line the operator builds drains before its engine stops, whatever deleted
its Pod: a scale-down, a rollout, a node drain, a preemption or `kubectl delete pod`.

| From the delete | What happens |
|---|---|
| 0 s | the routers drop the replica, and the kubelet starts the engine container's preStop hook |
| 0–5 s | the engine keeps serving normally, including a request routed to it just before the delete |
| 5–25 s | the hook reads the engine's in-flight gauges once a second and returns on the second idle read |
| 27 s at the latest | the engine receives SIGTERM once the hook returns: the hook starts no read after 25 s, and its last read can take 2 s more |
| 30 s | the kubelet kills what is left: the Pod's `terminationGracePeriodSeconds` is 30 |

An idle replica therefore leaves about 6 s after its delete. The timings are constants, not fields.

> **Why** — every supported router drops a replica on its deletion timestamp rather than on its
> readiness, but a router can pick the replica in the instant before it learns of the delete; the
> first 5 s is where that request lands. The wait happens before SIGTERM because vLLM aborts every
> running request on the signal by default, and serves nothing new while it drains if told to wait.

The hook sums these gauges from the listener the Pod's
[scrape annotations](model-deployment-metrics.md#scraping-the-pods) name. On a direct decoder that is
the engine behind the routing proxy, not the port the Service fronts.

| Engine | Gauges summed |
|---|---|
| vLLM | `vllm:num_requests_running`, `vllm:num_requests_waiting` — the second includes a decoder's requests still waiting for their KV |
| SGLang | `sglang:num_running_reqs`, `sglang:num_queue_reqs`, `sglang:num_prefill_bootstrap_queue_reqs`, `sglang:num_prefill_inflight_queue_reqs`, `sglang:num_decode_prealloc_queue_reqs`, `sglang:num_decode_transfer_queue_reqs` |

A listener that refuses the read, answers an HTTP error or fails the TLS handshake ends the hook at
once, since there is nothing it can measure. A read that times out counts as busy, and so does a
sample the hook cannot parse.

Adding the hook and the grace changed every replica's Pod spec, so upgrading to the release that
carries them turns each existing replica over once; see
[Rollout is a rolling replacement](model-deployment.md#rollout-is-a-rolling-replacement).

## What the window does not cover

- **A request still running about 25 s after the delete is cut.** vLLM aborts it when SIGTERM arrives;
  SGLang keeps draining it on its own until the kill at 30 s. No supported router moves a running
  request to another replica: the SGLang gateway retries only while the response has not started,
  and the other two routers do not retry.
- **A take-over role** — one that sets `command` — gets no hook and no rendered grace, because the
  operator cannot claim that container serves the gauges. Its Pod keeps the Kubernetes default.
- **A role that moves the engine with its own `--port`** gets only the first 5 s: nothing answers the
  hook on the port the operator rendered.
- **A replica of several Pods** drains only its leader. The other members serve no metrics, so their
  hooks return after the first 5 s, and they receive SIGTERM while the leader is still draining.
- **A direct decoder on a cluster below Kubernetes 1.29** runs its routing proxy as a classic
  container, which stops at the delete while the engine behind it drains, so a request in flight
  through the proxy still resets. The proxy's image has no shell to run a hook of its own.

---

**See also** — [Model Deployment Reference](model-deployment.md) for what turns a replica over ·
[Model Deployment Metrics Reference](model-deployment-metrics.md) for the scrape endpoints the hook
reads.

**Next** → [Model Deployment Status Reference](model-deployment-status.md)
