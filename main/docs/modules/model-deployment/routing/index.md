# Model Deployment Routing

A managed router selects among qualified serving endpoints using its configured routing policy.
Endpoint selection, per-router defaults, SGLang *DP* registration and qualification checks govern
which replicas receive traffic.

## Contents

- [Traffic targets](#traffic-targets)
- [Default routing policies](#default-routing-policies)
- [Policy overrides](#policy-overrides)
- [Endpoint status](#endpoint-status)
- [Troubleshooting](#troubleshooting)

## Traffic targets

The serving mode decides which `Pod`s are HTTP candidates; the router policy chooses only among
those candidates.

| Serving mode | HTTP candidates |
|---|---|
| Internal *Server* group | Member 0 of each replica |
| Qualified External *Server* group | Every qualified member (one *data parallelism (DP)* rank per member) |
| *Prefill* or *Decode* group | Member 0 of each replica, even when `size > 1` |
| *Elastic EP* collective | Master endpoint only |
| *Encode* group | Excluded from answering candidates and deployment `Service` |

`vllm-router` pairs only with vLLM and `sglang-gateway` only with SGLang:

| Mode | `llm-d-router` | `vllm-router` | `sglang-gateway` |
|---|---|---|---|
| Multi-replica *Server* | Fixed scoring profile (prefix, queue, KV); measured 187/170 on vLLM and 375/0 on SGLang | `cache_aware` default; `--policy round_robin` measured 60/60 | Same policies; `round_robin` measured 60/60 |
| *PD* pair | Token-counted *PD* split, connector docs and decode sidecar | Native *PD* mode | Native `--pd-disaggregation` (never combined with `--dp-aware`) |
| *EPD* | `encode` profile + decode sidecar transfer | Operator-owned `--epd-config`, no sidecar | Native pair; *Prefill* dials `--encoder-urls` |
| *Elastic EP* | Engine internal load balancing; single endpoint | Same | Same |
| *External DP* | Admitted, but no `--policy` flag; prefer engine router | Per-rank endpoints with `--policy round_robin` | Per-member endpoints (`--dp-aware` fan-out below) |

Where even spread matters (multi-replica *Server* or *External DP*), choose `vllm-router` for vLLM
or `sglang-gateway` for SGLang because `llm-d-router` has no policy switch
([Fixed scoring profile](#fixed-scoring-profile)).

- **External selectors**: Qualified *External* *Server* roles omit `member-index=0` while keeping
  the `endpoint-eligible` selector; the headless `Service` always publishes all members for peer
  communication. Hybrid GPU behavior is untested.
- **Shared serving port**: Managed roles expose one HTTP port (`8000` default; all routed roles must
  match). Admission rejects vLLM `MultiPort`. See
  [External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md).
- **SGLang `--dp-aware` registration**: For SGLang *External DP*, the operator renders `--dp-aware`
  on `sglang-gateway` so `gpustack/llm-router:v0.4.0`+ registers one logical worker per attention-DP
  rank (`http://host:port@0`, `http://host:port@1`) pointing at each member's single listener. This
  render gate and fan-out are unit-tested (no live runtime capture yet). Without `--dp-aware` (for
  example on `v0.3.1`), subordinate members time out in the gateway's 10 s discovery step, leaving
  at most one registered worker per group.

### Excluded replica groups

Protected groups are gated by `Pod` labels, never by dropping selector terms. Both the Router and
the role `Service` enforce the same rule:

| Group class | Selector rule |
|---|---|
| Managed protected group (multi-member ordinary, *External DP*, *PD*) | Requires `endpoint-eligible: "true"` from first enable; only qualified groups receive the label |
| Unsupported custom route or engine outside capability matrix | Keeps leader restriction with no qualification claim |
| Ordinary single-member *Server* | Leader restriction, no eligibility label required |

### Managed and custom roles

When roles in one deployment carry mixed requirements, the operator labels eligible discovery
members with `modeldeployment.gpustack.ai/router-candidate` to form a deployment-wide union. That
label marks membership only; actual routing intersects it with native role and member filters.

### Encode stage routing

Send all client requests to *`status.endpoint`*. *Encode* `Pod`s stay excluded from the deployment
`Service` and KV-event discovery:

| Router | Engine | Encoder routing |
|---|---|---|
| `llm-d-router` | `vLLM` | `encode` profile picks an encoder for media requests; decode sidecar (`--ec-connector=ec-nixl` or `ec-example`) coordinates transfer |
| `vllm-router` | `vLLM` | Operator renders `--epd-config` (owned, refused in *`extraArgs`*); router drives *Encode* → *Prefill* (NIXL) → *Decode* without a sidecar |
| `llm-d-router` | `SGLang` | Router splits every request to *Prefill*/*Decode* (no `--ec-connector`); *Prefill* dials *Encode* via `--encoder-urls` |
| `sglang-gateway` | `SGLang` | Gateway routes *Prefill*/*Decode* natively; *Prefill* dials *Encode* via `--encoder-urls` |

Under `llm-d-router` + vLLM, the `encode` profile omits queue, KV-cache and prefix scorers, sends
media requests through *Encode* first, and routes text-only requests straight to *Prefill* and
*Decode*. See [Supported serving configurations](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md#supported-serving-configurations) for verified combinations.

## Default routing policies

`vllm-router` and `sglang-gateway` default to `cache_aware` (logging
`policy: CacheAware { cache_threshold: 0.3, ... }`); `llm-d-router` uses its fixed scoring profile:

| Router | Default | Replica selection |
|---|---|---|
| `vllm-router` | `cache_aware` | Longest matching prefix above `--cache-threshold` (`0.3`), else smallest tree; switches to least-loaded when load gap exceeds `--balance-abs-threshold` (`64`) and `--balance-rel-threshold` (`1.5×`) |
| `sglang-gateway` | `cache_aware` | Same algorithm and three defaults |
| `llm-d-router` | Fixed profile | Highest sum of prefix match (weight `3`, omitted on *Decode*), queue depth (`2`) and KV utilization (`2`) |

### Prefix affinity

**Under `cache_aware`, serial requests sharing a long prefix land on one replica while peers stay
idle.** This is expected prefix reuse, not a fault; traffic spills over only when the load gap
exceeds the thresholds above.

Measured on two single-GPU *Server* replicas with serial 500-character shared-prefix chat requests
(zero errors; concurrent spread unmeasured):

| Router | Engine | Requests per replica |
|---|---|---|
| `vllm-router` | vLLM | 354 and 0 |
| `sglang-gateway` | SGLang | 406 and 0 |
| `llm-d-router` | vLLM | 187 and 170 |
| `llm-d-router` | SGLang | 375 and 0 |

## Policy overrides

Add `--policy round_robin` to *`spec.router.extraArgs`* on `vllm-router` or `sglang-gateway` to
spread requests evenly across replicas:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: round-robin-serving
   namespace: team-a
 spec:
   router:
     name: vllm-router  # or sglang-gateway
+    extraArgs:
+      - --policy
+      - round_robin
```

`--policy` is accepted by admission ([Router configuration](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#router-configuration));
the router logs `Starting router ... | policy: RoundRobin`. On the same two-replica test at
creation, 120 requests split `60 / 60` on both routers with zero errors.

Editing *`router.extraArgs`* replaces only the router `Deployment` without rolling worker `Pod`s
([Router rollout effects](/gpustack-operator/main/docs/modules/model-deployment/runtime-control/index.md#router-rollout-effects)); the new router starts with
an empty prefix table. Both routers also run behind the shared Envoy inference allowlist sidecar
with a `:9091` observer when managed accelerated non-*Encode* roles exist.

Admission does not validate `--policy` values; an unknown value stops the router at startup:

| Router | Accepted `--policy` values |
|---|---|
| `vllm-router` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `consistent_hash`, `rendezvous_hash` |
| `sglang-gateway` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `prefix_hash`, `manual` |

Only `round_robin` was live-tested; `cache_aware` thresholds, `--prefill-policy` and
`--decode-policy` pass through *`extraArgs`* untested.

### Fixed scoring profile

`llm-d-router` cannot switch policies through *`extraArgs`* because the operator owns
`--config-file` and exposes no scorer weight fields.

## Endpoint status

*`status.roles[].endpoints.eligible`* counts operator-qualified endpoints (`null` before first
observation, `0` when empty), while *`status.roles[].endpoints.serving.state`* reports the direct
per-Router union (`Confirmed` with *`serving.value`*, `NotConverged`, `Unknown` or `NotConfigured`).

Protected groups must pass all five criteria to hold `modeldeployment.gpustack.ai/endpoint-eligible: "true"`:

| Criterion | Requirement | Failure effect |
|---|---|---|
| `MemberSetComplete` | Every declared member `Pod` exists | Revokes `endpoint-eligible`; `Unknown` keeps prior state |
| `MembersReady` | Every member passes readiness (`/health` or `/gpustack/v1/ready`) | Revokes `endpoint-eligible` immediately |
| `GroupForward` | Collective forward generation probe succeeds | Revokes `endpoint-eligible`; `Unknown` keeps prior state |
| `NoPlannedRetirement` | No active retirement holds the ordinal | Revokes `endpoint-eligible` during withdrawal and drain |
| `NoPendingReplacement` | No replacement is queued for the ordinal | Revokes `endpoint-eligible` immediately |

*`status.roles[].groups[]`* publishes member UID `generation`, `state` (`Eligible`, `Withdrawn`,
`AwaitingMembers`, `AwaitingReady`, `AwaitingProbe`, `AwaitingObservation`, `Unsupported`,
`Unknown`) and `serving` confirmation. An already-`Eligible` group with unchanged ready members
stays `Eligible` during a transient Router scrape loss while *`serving.state`* turns `Unknown`.

The operator queries every Router `Pod` directly under three timeouts (all Router replicas must
agree for `Confirmed`):

| Bound | Value | Scope |
|---|---|---|
| Fetch deadline | `2 s` | Single Router `Pod` read |
| Collection budget | `14 s` | One collection across all Router `Pod`s |
| Freshness policy | `15 s` | Validity of a collected view before falling back to `Unknown` |

### Per-worker routing metrics

The `ModelDeployment` `/metrics` subresource aggregates role and router counters without per-replica
labels. Scrape each router's own `/metrics` for per-worker attribution (`worker` carries the worker
URL/`Pod` address):

| Router | Series | Meaning |
|---|---|---|
| `vllm-router` | `vllm_router_processed_requests_total{worker}` | Requests sent to each worker |
| `vllm-router` | `vllm_router_policy_decisions_total{policy,worker}` | Policy picks per worker |
| `vllm-router` | `vllm_router_pd_prefill_requests_total{worker}`, `vllm_router_pd_decode_requests_total{worker}` | *PD* requests per *Prefill* and *Decode* worker |
| `sglang-gateway` | `smg_worker_requests_active{worker}` | Active in-flight requests per worker |
| `sglang-gateway` | `smg_worker_selection_total{worker_type,policy}` | Picks by policy and `regular`/`prefill`/`decode` type |
| `llm-d-router` | `llm_d_epp_per_endpoint_queue_size{model_server_endpoint}` | Queued requests per endpoint |
| `llm-d-router` | `llm_d_epp_disagg_decision_total{decision_type}` | `prefill-decode` versus `decode-only` split counts |

Neither `sglang-gateway` nor `llm-d-router` exposes cumulative per-worker request totals; compare
each engine `Pod`'s *time to first token (TTFT)* histogram `_count` instead. On `sglang-gateway`
after scale-down, retired workers remain in `smg_worker_health` with value `-1` while
`smg_discovery_deregistrations_total{reason="pod_deleted"}` increments; count series equal to `1`.

## Troubleshooting

Inspect router readiness, endpoint eligibility and per-group status on the `ModelDeployment`:

```bash
kubectl -n <namespace> get modeldeployment <name> -o json \
  | jq '{phase: .status.phase, endpoint: .status.endpoint,
         conditions: [.status.conditions[] | select(.type | IN("RouterReady","RoleKindsReady","EndpointEligibility"))],
         roles: [.status.roles[] | {name, ready, desired, endpoints, groups}]}'
```

| Symptom | Signal | Owner & action |
|---|---|---|
| *`status.endpoint`* is empty | `RouterReady=False` (`RenderFailed`, `NoReadyReplicas`) or `Unknown` (`NotDeployed`) | Fix refused *`spec.router`* flags on the `ModelDeployment` or inspect `<deployment>-router` `Pod` logs and events. |
| `Pod` `Running` (`0/1` `Ready`) | `groups[].state: AwaitingReady` or `RoleKindsReady=False` | Wait for weight load (`startupTimeoutSeconds`, default `1800 s`), or check worker `Pod` logs, `/gpustack/v1/ready` and *`roles[].pause`*. |
| `Pod` `1/1` `Ready` without `endpoint-eligible` | `groups[].state: AwaitingMembers`, `AwaitingProbe` or `Withdrawn` | Read `groups[].reason` on the `ModelDeployment` for missing peer `Pod`s, failed forward probe, *`status.retirement`* or *`roles[].pause: true`*. |
| *`endpoints.eligible`* is `null` | `EndpointEligibility=Unknown` (`NotObserved`) | Wait for qualification pass or inspect operator worker logs if status stalls. |
| Qualified group gets no traffic | `AwaitingObservation` or `serving.state: Unknown` / `NotConverged` | Inspect `<deployment>-router` `Pod`s and `:9091/observer/endpoints` (`:9090` on `llm-d-router`) so all Router replicas report fresh views within `15 s`. |

---

**See also** — [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) (*`spec.router`* pairing and refused flags) ·
[Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md) (*EPD* disaggregation and connectors) ·
[Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) (readiness probes and rollout behavior) ·
[Online Runtime Control](/gpustack-operator/main/docs/modules/model-deployment/runtime-control/index.md) (router hash decoupling and role pause)

**Next** → [Model Deployment Scaling](/gpustack-operator/main/docs/modules/model-deployment/scaling/index.md) — scale serving replicas and observe graceful retirement.
