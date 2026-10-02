# Model Deployment Shutdown

A `ModelDeployment` replica drains during the interval between Pod deletion and engine exit.
The grace period bounds how long it can wait for running requests and engine shutdown.

## Contents

- [The drain window](#the-drain-window)
- [The retirement protocol](#the-retirement-protocol)
- [Coverage limits](#coverage-limits)

## The drain window

A replica whose command line the operator builds drains before its engine stops, whatever deleted
its Pod: a rollout, a node drain, a preemption or `kubectl delete pod`. A removal the spec makes —
a `replicas` reduction, a surplus shed, a dropped role — goes through
[the retirement protocol](#the-retirement-protocol) first, and its Pod is deleted when that
finishes.

`G` below is the role's grace: `roles[].terminationGracePeriodSeconds`, 30 when unset, written to
the Pod's field of the same name.

| From the delete | At the default | Action |
|---|---|---|
| 0 s | 0 s | the routers drop the replica, and the kubelet starts the engine container's preStop hook |
| 0–5 s | 0–5 s | the engine keeps serving normally, including a request routed to it just before the delete |
| 5 s to `G` − 5 | 5–25 s | the hook reads the engine's in-flight gauges once a second and returns on the second idle read |
| `G` − 3 at the latest | 27 s | the engine receives SIGTERM once the hook returns: the hook starts no read after `G` − 5, and its last read can take 2 s more |
| `G` | 30 s | the kubelet kills what is left |

The hook of an idle replica therefore returns about 6 s after its delete, whatever `G` is, and vLLM
or an SGLang server exits a few seconds later. The first 5 s and the last 5 s are constants; only
the wait between them moves with `G`, which admission takes from 15 to 3600.

> **Why** — every supported router drops a replica on its deletion timestamp rather than on its
> readiness, but a router can pick the replica in the instant before it learns of the delete; the
> first 5 s is where that request lands. The wait happens before SIGTERM because vLLM aborts every
> running request on the signal by default, and serves nothing new while it drains if told to wait.

**Raising `G` costs accelerators and quota.** It is a ceiling, not a wait: an idle replica still
leaves when its engine exits. A busy or slow one holds its accelerators and quota up to `G` on every
delete, and a rollout waits each departing replica out one at a time per role, so the cost multiplies
into every rollout. What it buys is a longer wait for running requests, and a clean exit for an
engine slow to stop after SIGTERM.

**An SGLang prefill or decode role needs up to about 20 s after SIGTERM, even idle, so set its
`terminationGracePeriodSeconds` to about 45.** Measured idle, a decoder left 26 s and a prefiller
28 s after its delete, 2 s short of the kill at the default 30 s. From SGLang 0.5.14 on, its
split-role scheduler ignores the shutdown request SIGTERM leads to, and the engine waits a fixed 15 s
for that scheduler before killing it.

> **Why it is left alone** — no flag shortens that wait; `SGL_FORCE_SHUTDOWN` skips only the drain.
> A grace of 45 buys a clean exit rather than a kill, since the engine kills its scheduler itself
> either way. The replica is idle by then, so the kill at 30 s cuts no request.

The hook sums these gauges from the listener the Pod's
[scrape annotations](metrics.md#scraping-the-pods) name. On a direct decoder that is
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
[Rollout behavior](deployment.md#rollout-behavior).

Changing a role's `terminationGracePeriodSeconds` turns its replicas over the same way, and each
replica that leaves in that rollout leaves with the grace it was created with. Writing 30 onto a
role that set none renders the same Pods, so it turns nothing over.

## The retirement protocol

**A replica the spec takes capacity away from is withdrawn and drained before it is deleted, and
the deletion waits for that to finish.** The protocol covers a `replicas` reduction, a shed of
surplus replicas, and a replica of a role the spec has dropped. Its progress is the
[`status.retirement` field](status.md#status), and the replica stays in place, still counted,
for as long as the protocol holds it.

Two mechanisms are involved and they answer different questions. The protocol decides **whether the
Pod may be deleted**; the [drain window](#the-drain-window) above decides **how long the engine
takes to exit once it is**, on the kubelet's grace clock after the delete. A replica passes the
first and still spends the second.

### Reading a member's work

The read is one bounded, read-only `pods/exec` into the member's own `main` container, which
fetches that engine's `/metrics` over loopback on the port the member's own
[scrape annotations](metrics.md#scraping-the-pods) name. Reading it from inside the member is what
makes the answer per-member: a scrape through the Service would land on whichever replica answered
and could not say which one was idle.

| Bound | Value |
|---|---|
| response size | 1 MiB |
| the engine's own answer | 2 s |
| the whole read including the connection | 5 s |

The gauges read are the same ones the hook sums, and they are named once in the tree, so the
protocol and the hook cannot disagree about what counts as work in flight.

**A member must read idle twice, on complete envelopes, before the deletion commits.** Both reads
have to carry every expected gauge, and a single unlucky zero cannot carry the protocol past a
member that is still working. A second read that finds work holds the operation.

### What the drain read refuses

Every one of these holds the replica in place, and the `reason` on the reservation names which one
applied. They are the ordinary outcomes of a replica that is busy, loading, or not
the shape the protocol knows how to measure.

| The member | Verdict | Why |
|---|---|---|
| still serving requests | held | there is work in flight, which is what the read is for |
| answers with some gauges missing | held | a partial envelope is not evidence of an empty queue, and a reader cannot sum what it did not find |
| the exec fails, times out, or returns a body that is not metrics | held | nothing was measured, and an absence of measurement is not a measurement of absence |
| the container has restarted | held | the gauges now served come from an engine that did not exist when the protocol began watching |
| a prefill or decode replica | refused | see below |
| a take-over role | held | the operator renders no endpoint for it, so there is nothing to read |
| an engine with no gauge list | refused | no such measurement exists for it |

**A restarted container is held, and the hold is bounded.** The gauges a
restarted engine serves describe a fresh process, so they cannot speak for the work the protocol
set out to account for. The drain budget and the operation's total still apply, so a replica that
keeps crash-looping ends as `Aborted` with its capacity retained, and never blocks the queue.

> A restart throws away a loaded model and any request it was serving, so the replica is not worth
> less for it. The protocol cannot tell whether a restarted engine is a replacement it should wait
> for or a crash loop it should give up on, and the budget is what resolves that question.

**A prefill or decode replica is refused outright.** Its queues are visible from both engines, so
this is not a case of unmeasured work.

What is not verifiable is the release. A decode replica reading zero is an engine about to be
handed work, and nothing in either engine's metrics proves it will not be. The shape stays refused
until a decoder-side release can be verified, because the other answer deletes a decode replica in
the middle of a prefill.

Both engines report a decoder's work while it waits to receive its KV: a prefill half holds it in
its transfer queue, and a decode half holds it in its own prealloc and transfer queues.

The refusal is read from the member Pod's own rendered command, so it still applies to a replica
whose role the spec has already dropped. Both engines are covered: a vLLM
member is recognized by its data-parallel balance flags, and an SGLang member by its
`--disaggregation-mode prefill` or `decode`.

## Coverage limits

- **A request still running at `G` − 5 is cut**, 25 s after the delete at the default. vLLM aborts it
  when SIGTERM arrives; SGLang keeps draining it on its own until the kill at `G`. No supported
  router moves a running request to another replica: the SGLang gateway retries only while the
  response has not started, and the other two routers do not retry.
- **An SGLang prefill or decode replica whose hook returns later than `G` − 20 is killed at `G`**,
  since its engine then has less than the 20 s it takes to exit: 10 s after the delete at the
  default, 25 s at 45. The replica is idle by that point, or its hook gave up on it, so the kill cuts
  no request the deadline would not.
- **A take-over role** (one that sets `command`) gets no hook, because the operator cannot claim
  that container serves the gauges. Its Pod gets the role's `terminationGracePeriodSeconds` as given,
  and keeps the Kubernetes default when the role sets none.
- **A role that moves the engine with its own `--port`** gets only the first 5 s: nothing answers the
  hook on the port the operator rendered.
- **A replica of several Pods drains only its leader.** The other members serve no metrics, so their
  hooks return after the first 5 s, and they receive SIGTERM while the leader is still draining. The
  retirement protocol reads **every** member of its target and holds unless each one answers, so a
  multi-member replica is held on its non-serving members and the operation ends as `Aborted` with
  its capacity retained. Reducing or shedding a role at `size` above one therefore leaves its
  replicas in place.
- **A prefill or decode replica is refused by the protocol**, so it is never deleted through one.
  The [drain window](#the-drain-window) still applies to it once something else deletes its Pod.
- **A direct decoder on a cluster below Kubernetes 1.29** runs its routing proxy as a classic
  container, which stops at the delete while the engine behind it drains, so a request in flight
  through the proxy still resets. The proxy's image has no shell to run a hook of its own.

---

**See also** — [Model Deployment](deployment.md) for what turns a replica over ·
[Model Deployment Metrics](metrics.md) for the scrape endpoints the hook
reads.

**Next** → [Model Deployment Status](status.md)
