# Model Deployment Routing

A managed router selects among the HTTP endpoints of serving replicas. Every routing choice measured here ran
on a server role; on a prefill/decode pair, where each half is chosen separately, none has been run.

See [Prefill and Decode](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md) for the External DP boundary within P/D roles.

## Contents

- [Which Pods receive traffic](#which-pods-receive-traffic)
- [Each router's default](#each-routers-default)
- [Prefix affinity](#prefix-affinity)
- [Switching to round robin](#switching-to-round-robin)
- [llm-d-router takes no policy flag](#llm-d-router-takes-no-policy-flag)
- [Confirming that a Router serves](#confirming-that-a-router-serves)
- [Per-worker routing metrics](#per-worker-routing-metrics)

## Which Pods receive traffic

The engine's LB mode determines the candidate endpoints. The Router policy chooses among those candidates.
`round_robin` does not change the endpoint selector.

| Deployment shape | HTTP candidates |
|---|---|
| Internal Server group | Member 0 of each replica. |
| Qualified External Server group | Every qualified member; each member serves one DP rank. |
| Prefill or Decode group | Member 0 of each replica, including groups with `size > 1`. |
| Unknown mode or unverified group | Keep the existing leader restriction; do not expand discovery. |

For an eligible External Server role, Router discovery and ordinary Services omit `member-index=0`.
They retain the endpoint eligibility label, so an unhealthy group cannot receive new traffic.
The replica's headless Service always publishes every member for peer communication.

Known non-Internal modes follow the External selector rule. Status retains the engine's distinct mode name.
Hybrid GPU behavior remains untested. See [Prefill and Decode](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md) for P/D limitations.

Managed roles expose one fixed HTTP port per Pod, defaulting to 8000.
All routed roles must use the same serving port. A shared custom port remains supported.
Admission rejects vLLM's MultiPort mode because it starts endpoints on additional, incremented ports.
A decoder proxy's internal backend port remains separate from its published HTTP port.
See the [External DP walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/external-dp/index.md) for a minimal per-rank configuration.

## Each router's default

**`vllm-router` and `sglang-gateway` route by `cache_aware`, and `llm-d-router` scores every
candidate for each request.** The operator renders no policy flag for the first two, so each runs its
upstream default. The table describes the bundled vLLM router `v0.1.15` and SGLang gateway
`gateway-v0.3.1`; the llm-d row describes the operator's scoring profile.

| `spec.router.name` | Default | Replica selection |
|---|---|---|
| `vllm-router` | `cache_aware` | The replica whose past requests share the longest prefix with this one, when that share is above `--cache-threshold` (`0.3`); below it, the replica with the smallest record. Once the busiest replica has more than `--balance-abs-threshold` (`64`) requests over the idlest and more than `--balance-rel-threshold` (`1.5`) times as many, the least-loaded replica instead |
| `sglang-gateway` | `cache_aware` | The same algorithm, with the same three defaults |
| `llm-d-router` | A fixed scoring profile | The highest total of prefix-cache match (weight `3`, not scored on a decode half), queue depth (`2`) and KV-cache utilization (`2`) |

Both `cache_aware` routers log the policy once at startup, as `policy: CacheAware { cache_threshold:
0.3, … }`.

## Prefix affinity

**Under `cache_aware`, requests that share a long prefix and arrive one after another all go to one
replica, and the others stay idle.** That is the policy working, not a fault: the replica that served
the prefix holds it in its prefix cache, so the next request sharing it does not recompute it.

Such a request only goes elsewhere once the load gap above opens, and traffic that sends one
request at a time never has more than one in flight to open it.

Measured on two server replicas, each on its own GPU, with serial chat requests that open with the
same text of about 500 characters and end in a question of their own, streaming and non-streaming in
turn. No request failed:

| Router | Engine | Requests on each replica |
|---|---|---|
| `vllm-router` | vLLM `0.29.0` | 354 and 0 |
| `sglang-gateway` | SGLang `0.5.18` | 406 and 0 |
| `llm-d-router` | vLLM `0.29.0` | 187 and 170 |
| `llm-d-router` | SGLang `0.5.18` | 375 and 0 |

How any of the three spreads concurrent traffic is not measured.

## Switching to round robin

**Add `--policy round_robin` to `spec.router.extraArgs` on `vllm-router` or `sglang-gateway`** to
send each request to the next replica in turn, whatever it shares with earlier ones:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: round-robin-serving
 spec:
   router:
     name: vllm-router                    # or sglang-gateway
+    extraArgs:
+      - --policy
+      - round_robin
```

`--policy` is in neither router's [refused list](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md#the-router-block), so admission
passes it and the operator appends it to the command line it renders. The router then logs
`Starting router … | policy: RoundRobin`.

Measured on the same two replicas and the same kind of traffic, with the flag set at creation:
under each router, 120 requests with none failing, 60 on each replica. `vllm-router` counted them
in `vllm_router_policy_decisions_total{policy="round_robin"}`, 60 per replica, and `sglang-gateway`
in `smg_worker_selection_total{policy="round_robin"}`, 120 in all.

`router.extraArgs` is [editable](/gpustack-operator/v0.9.0/docs/modules/model-deployment/deployment/index.md#deployment-identity-fields), so a
running deployment can switch as well. The router Pod is then replaced, and the new one starts with
no record of earlier prefixes. Only a flag set at creation has been run.

**The value is each project's own, and the router checks it, not admission**, so a misspelled one
stops the router at startup:

| `spec.router.name` | `--policy` values |
|---|---|
| `vllm-router` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `consistent_hash`, `rendezvous_hash` |
| `sglang-gateway` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `prefix_hash`, `manual` |

Only `round_robin` has been run. The three `cache_aware` thresholds pass through `extraArgs` the same
way, as do both routers' `--prefill-policy` and `--decode-policy` for a prefill/decode pair; none of
those has been run either.

## llm-d-router takes no policy flag

**`llm-d-router` cannot be switched through `extraArgs`.** Its scorers and their weights are in the
configuration document the operator renders and mounts, and `--config-file`, which would point the
router at another one, is refused there. No field sets them either.

## Confirming that a Router serves

**Which replicas a Router can reach is not the same question as which replicas it is serving, and
the operator reports both.** The first is the qualification list, and the second is an observation
of the Router itself; they are published separately in
[`status.roles[].endpoints`](status.md#status) and never collapse into one number.

The observation is a plain HTTP GET with a short timeout against each Router Pod's own address. It
deliberately does not go through the Router Service, which would load balance across the Router's
replicas and report one of them as the whole Router. What `Confirmed` then means is the
serving-observation contract [`status.roles[].endpoints`](status.md#status) defines once.

**A collected view informs the answer for fifteen seconds.** Past that the answer becomes `Unknown`,
so a Router that stops being reachable is reported as unobserved and never as serving whatever it
served last.

> The freshness bound is what keeps a stopped Router from reading as a healthy one. A view that
> outlived its evidence would report a serving count for a process that is gone, and the only way
> back would be an edit to the deployment.

## Per-worker routing metrics

The deployment's [metrics snapshot](/gpustack-operator/v0.9.0/docs/modules/model-deployment/metrics/index.md) does not say which replica served a
request. Each router's own `/metrics` does, for the series below, all seen exported in runs. A
`worker` label is the worker's URL, which carries its Pod address.

| `spec.router.name` | Series | Meaning |
|---|---|---|
| `vllm-router` | `vllm_router_processed_requests_total{worker}` | requests sent to each worker, in both modes |
| `vllm-router` | `vllm_router_policy_decisions_total{policy,worker}` | picks each policy made, per worker |
| `vllm-router` | `vllm_router_pd_prefill_requests_total{worker}`, `vllm_router_pd_decode_requests_total{worker}` | in P/D mode, requests sent to each prefill and each decode worker |
| `sglang-gateway` | `smg_worker_requests_active{worker}` | requests in flight to each worker at the scrape |
| `sglang-gateway` | `smg_worker_selection_total{worker_type,policy}` | picks per policy and per `regular`, `prefill` or `decode` worker type, not per worker |
| `llm-d-router` | `llm_d_epp_per_endpoint_queue_size{model_server_endpoint}` | requests queued at each endpoint at the scrape |
| `llm-d-router` | `llm_d_epp_disagg_decision_total{decision_type}` | requests sent through a prefill replica (`prefill-decode`) or straight to decode (`decode-only`) |

Neither `sglang-gateway` nor `llm-d-router` keeps a running count per replica. Behind them, compare
the engine Pods' own counts instead, such as each Pod's TTFT histogram `_count`.

---

**See also** — [Model Deployment Prefill and Decode](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md)
for `spec.router` and the flags each router refuses · [Model Deployment Metrics](/gpustack-operator/v0.9.0/docs/modules/model-deployment/metrics/index.md) for the router
counters a deployment's snapshot reads.

**Next** → [Model Deployment Status](/gpustack-operator/v0.9.0/docs/modules/model-deployment/status/index.md)
