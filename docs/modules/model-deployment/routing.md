# Model Deployment Routing

A managed router selects among serving endpoints, then its policy chooses among them. This page covers
endpoint selection and SGLang's tested DP routing modes.

The recorded combinations are vLLM `0.29.0` and `0.30.0`, SGLang `0.5.18` and `0.5.22`,
`vllm-router` `v0.1.16.1`, the `sglang-gateway` binary of `gpustack/llm-router:v0.3.1`, and
`llm-d-router` `v0.11.0`.
Facts on this page apply to those versions; newer builds must be re-verified before they inherit a claim.

## Contents

- [Which Pods receive traffic](#which-pods-receive-traffic)
- [Encode stage routing](#encode-stage-routing)
- [Each router's default](#each-routers-default)
- [Prefix affinity](#prefix-affinity)
- [Switching to round robin](#switching-to-round-robin)
- [No policy flag](#no-policy-flag)
- [Serving observations](#serving-observations)
- [Per-worker routing metrics](#per-worker-routing-metrics)

## Which Pods receive traffic

The engine's *load balancing (LB)* mode determines the candidate endpoints. The Router policy chooses among those candidates.
`round_robin` does not change the endpoint selector.

| Serving configuration | HTTP candidates |
|---|---|
| Internal Server group | Member 0 of each replica. |
| Qualified External Server group | Every qualified member; each member serves one *data parallelism (DP)* rank. |
| *Prefill* or *Decode* group | Member 0 of each replica, including groups with `size > 1`. |
| *Encode* group | Excluded from answering candidates and the deployment-wide `Service`. |

*Encode* `Pod`s are never part of the deployment's answering set. Under `llm-d-router` they are
discovered for stage routing, and under `vllm-router` their `Service` endpoint is passed through
`--epd-config`, while the deployment `Service` never targets them. All three routers (`llm-d-router`,
`vllm-router` and `sglang-gateway`) are admitted with an *Encode* role for their supported engine
and platform combinations. See [Encode stage routing](#encode-stage-routing).

For an eligible External Server role the selectors carry no member-index term at all, so every
qualified rank is discovered; the leader restriction applies only to a shape whose subordinate members
expose no HTTP endpoint. The selectors keep the endpoint eligibility requirement, so an unhealthy
group cannot receive new traffic.
The replica's headless `Service` always publishes every member for peer communication.

On the SGLang External DP shape the operator renders `--dp-aware` for the gateway. With the flag,
the gateway registers one worker per attention-DP rank: each rank is a logical worker whose URL
carries a rank suffix (`http://host:port@0`, `http://host:port@1`), and every suffix dials the
member's one physical serving listener — the suffix is a rank, not a port.

That registration is the gateway patch this project ships: an image carrying it is
`gpustack/llm-router:v0.4.0` or later, while the default
[`model-deployment-router-image`](../../reference/settings.md) still names `v0.3.1`, which
predates the patch. The registration activates only where *`spec.router.image`* or the setting
names an image that carries the patch.

No runtime run has exercised the flag: the render gate and the patched fan-out are verified in
source and unit tests on the recorded shape.

Without the flag the gateway registers at most one worker per group — recorded on
`gpustack/llm-router:v0.3.1`, and on the `v0.4.0` gateway build with the flag withheld. Each
group's subordinate member times out inside the gateway's 10 s discovery step and never enters
the worker registry, and under load a group's registered worker can miss its registration too:
a two-group, four-member deployment presented two healthy workers in one run and one in another.

Known non-Internal modes follow the External selector rule. Status retains the engine's distinct mode name.
Hybrid GPU behavior remains untested. See [Prefill and Decode](prefill-decode.md) for *PD* limitations.

Managed roles expose one fixed HTTP port per `Pod`, defaulting to 8000.
All routed roles must use the same serving port. A shared custom port remains supported.
Admission rejects vLLM's MultiPort mode because it starts endpoints on additional, incremented ports.
A decoder proxy's internal backend port remains separate from its published HTTP port.
See the [External DP walkthrough](../../walkthroughs/model-deployment/external-dp.md) for a minimal per-rank configuration.

### How groups are excluded

A managed protected group is excluded through its `Pod` set, not through a selector that loses a term.
The requirement stays in the selector from the first enable, decided or not, and only the members whose
group is qualified carry the matching label.

| Group class | Exclusion |
|---|---|
| Managed protected group: multi-member ordinary, External DP, or *Prefill*/*Decode* | The selector carries the endpoint-eligibility requirement from the first enable, decided or not. Only the members whose group is qualified carry the matching label, so only they are discovered. |
| Unsupported custom route, or an engine outside the capability matrix | The compatibility carve-out: the existing leader restriction, and no claim of qualification. |
| Ordinary single-member Server | The leader restriction, with no eligibility requirement in the selector. |

The Router and the ordinary `Service` take the same decision, so a request cannot reach an endpoint the
`Service` does not front.

### Managed and custom roles

A deployment whose roles do not all carry the same requirement needs one deployment-wide selector to
express their union, and the operator names that union through an internal candidate label,
`modeldeployment.gpustack.ai/router-candidate`. The label says membership, not qualification: a
candidate is a member that may be discovered, and the label is no proof that its group qualifies.

Actual discovery is the intersection of the deployment-wide selector with the router's native role and
member filters, so a candidate the native filter excludes is not discovered. A per-kind filter admits
that kind's union, because two roles of one kind share the filter and a filter demanding one role's
requirement would withhold the other's members.

## Encode stage routing

How the encoder is reached depends on the engine and router. Send requests to
*`status.endpoint`* in every case.

| Router | Engine | How the encoder is reached |
|---|---|---|
| `llm-d-router` | `vLLM` | The router picks an encoder for requests with media, then the *decode* sidecar coordinates the transfer |
| `vllm-router` | `vLLM` | The router receives the encoder `Service` URL via `--epd-config` and drives *encode*, *prefill* and *decode* directly without a *decode* sidecar |
| `llm-d-router` | `SGLang` | The *Prefill* role calls the *Encode* role directly through `--encoder-urls`; the router has no *encode* stage |
| `sglang-gateway` | `SGLang` | The gateway routes the pair natively; the *Prefill* role calls the *Encode* role directly |

With `llm-d-router` and `vLLM`, the router defines an `encode` scheduling profile over the `Pod`s
labelled `modeldeployment.gpustack.ai/role-kind: encode`. The profile applies no queue, KV cache or
prefix scorers, because an encoder keeps no KV cache and tracks no tokens. A multimodal-aware
decider sends a request with media through an encoder first. A text-only request goes straight to
*prefill* and *decode*.

On the Ascend *encode/prefill/decode (EPD)* configuration (`llm-d-router` + `vLLM-Ascend` + `ECExampleConnector`), the `encode`
role answers requests that carry media and stays idle for text-only traffic. The connector hands
the result to *Prefill* through the shared volume both roles mount; the router is not in that path.

The operator keeps a vLLM encoder in the router's candidate set on every platform. Its
address-based encoder path applies only to SGLang.

The *decode* sidecar's `--ec-connector` follows the admitted row: `ec-example` on Ascend and `ec-nixl`
on NVIDIA and AMD. See [Admitted matrix](encode-prefill-decode.md#admitted-matrix).

With `vllm-router` and `vLLM` (on NVIDIA and AMD), the operator passes `--epd-config={"encoder_urls":["http://<md>-<encode-role>.<ns>.svc:<port>"]}` to the router. The router calls the encoder `Service` first, forwards `ec_transfer_params` to the *prefill* worker so it pulls the embeddings over NIXL (`ECCPUConnector`), and strips `ec_transfer_params` before forwarding to *decode*. `--epd-config` is operator-owned and refused in *`spec.router.extraArgs`*.

With `llm-d-router` and `SGLang`, the router splits every request between *prefill* and *decode*,
because an SGLang *decode* role cannot *prefill*. The sidecar runs without `--ec-connector`, since this
engine uses no encoder-cache connector.

Encoders are also excluded from KV cache event discovery, because they produce no cache blocks.

## Each router's default

**`vllm-router` and `sglang-gateway` route by `cache_aware`, and `llm-d-router` scores every
candidate for each request.** The operator renders no policy flag for the first two, so each runs its
upstream default. The table describes the bundled vLLM router `v0.1.16.1` and the `sglang-gateway` binary of
`gpustack/llm-router:v0.3.1`; the llm-d row describes the operator's scoring profile.

| *`spec.router.name`* | Default | Replica selection |
|---|---|---|
| `vllm-router` | `cache_aware` | The replica whose past requests share the longest prefix with this one, when that share is above `--cache-threshold` (`0.3`); below it, the replica with the smallest record. Once the busiest replica has more than `--balance-abs-threshold` (`64`) requests over the idlest and more than `--balance-rel-threshold` (`1.5`) times as many, the least-loaded replica instead |
| `sglang-gateway` | `cache_aware` | The same algorithm, with the same three defaults |
| `llm-d-router` | A fixed scoring profile | The highest total of prefix-cache match (weight `3`, not scored on a *decode* half), queue depth (`2`) and KV-cache utilization (`2`) |

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

**Add `--policy round_robin` to *`spec.router.extraArgs`* on `vllm-router` or `sglang-gateway`** to
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

`--policy` is in neither router's [refused list](prefill-decode.md#router-configuration), so admission
passes it and the operator appends it to the command line it renders. The router then logs
`Starting router … | policy: RoundRobin`.

Measured on the same two replicas and the same kind of traffic, with the flag set at creation:
under each router, 120 requests with none failing, 60 on each replica. `vllm-router` counted them
in `vllm_router_policy_decisions_total{policy="round_robin"}`, 60 per replica, and `sglang-gateway`
in `smg_worker_selection_total{policy="round_robin"}`, 120 in all.

*`router.extraArgs`* is [editable](deployment.md#deployment-identity-fields), so a
running deployment can switch as well. The router `Pod` is then replaced, and the new one starts with
no record of earlier prefixes. Only a flag set at creation has been run.

**The value is each project's own, and the router checks it, not admission**, so a misspelled one
stops the router at startup:

| *`spec.router.name`* | `--policy` values |
|---|---|
| `vllm-router` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `consistent_hash`, `rendezvous_hash` |
| `sglang-gateway` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `prefix_hash`, `manual` |

Only `round_robin` has been run. The three `cache_aware` thresholds pass through *`extraArgs`* the same
way, as do both routers' `--prefill-policy` and `--decode-policy` for a *prefill/decode* pair; none of
those has been run either.

## No policy flag

**`llm-d-router` cannot be switched through *`extraArgs`*.** Its scorers and their weights are in the
configuration document the operator renders and mounts, and `--config-file`, which would point the
router at another one, is refused there. No field sets them either.

## Serving observations

**Which replicas a Router can reach is not the same question as which replicas it is serving, and
the operator reports both.** The first is the qualification list, and the second is an observation
of the Router itself; they are published separately in
[`status.roles[].endpoints`](status.md#status) and never collapse into one number.

**A group adds a third surface, and the operator publishes three related answers without collapsing
them.** The role's counts, the group's own entry in
[`status.roles[].groups[]`](status.md#group-status), and the Router's serving confirmation are three
readings of one deployment, and the group entry answers a question the counts cannot: which members
the group holds, and what holds it.

`AwaitingObservation` names a group whose qualification passed and whose restoration the Router's
observation blocks. A group that was already eligible and whose fresh membership is complete,
unchanged, safe and current stays `Eligible` through an observation loss, with its serving
confirmation `Unknown`. The group's reason and the Router's reason name different facts, so a lost
Router view never reads as an engine fault.

Retained eligibility requires the [Group status](status.md#group-status) membership checks.

**A Router process health result is not an engine group's health.** The Router answers for its own
process; the group answers for the engine's members.

The observation is a plain HTTP GET with a short timeout against each Router `Pod`'s own address. It
deliberately does not go through the Router `Service`, which would load balance across the Router's
replicas and report one of them as the whole Router. What `Confirmed` then means is the
serving-observation contract [`status.roles[].endpoints`](status.md#status) defines once.

**Three bounds apply, and they serve different purposes.**

| Bound | Value | What it covers |
|---|---|---|
| Fetch deadline | 2 seconds | one read of one Router's view |
| Collection budget | 14 seconds | one collection, across every Router it reads |
| Freshness policy | 15 seconds | how long a collected view informs the answer |

**A collected view informs the answer for fifteen seconds.** Past that the answer becomes `Unknown`,
so a Router that stops being reachable is reported as unobserved and never as serving whatever it
served last. The fetch deadline keeps one unreachable Router from consuming the collection budget the
other Routers share.

> The freshness bound is what keeps a stopped Router from reading as a healthy one. A view that
> outlived its evidence would report a serving count for a process that is gone, and the only way
> back would be an edit to the deployment.

## Per-worker routing metrics

The deployment's [metrics snapshot](metrics.md) does not say which replica served a
request. Each router's own `/metrics` does, for the series below, all seen exported in runs. A
`worker` label is the worker's URL, which carries its `Pod` address.

| *`spec.router.name`* | Series | Meaning |
|---|---|---|
| `vllm-router` | `vllm_router_processed_requests_total{worker}` | requests sent to each worker, in both modes |
| `vllm-router` | `vllm_router_policy_decisions_total{policy,worker}` | picks each policy made, per worker |
| `vllm-router` | `vllm_router_pd_prefill_requests_total{worker}`, `vllm_router_pd_decode_requests_total{worker}` | in *PD* mode, requests sent to each *prefill* and each *decode* worker |
| `sglang-gateway` | `smg_worker_requests_active{worker}` | requests in flight to each worker at the scrape |
| `sglang-gateway` | `smg_worker_selection_total{worker_type,policy}` | picks per policy and per `regular`, `prefill` or `decode` worker type, not per worker |
| `llm-d-router` | `llm_d_epp_per_endpoint_queue_size{model_server_endpoint}` | requests queued at each endpoint at the scrape |
| `llm-d-router` | `llm_d_epp_disagg_decision_total{decision_type}` | requests sent through a *prefill* replica (`prefill-decode`) or straight to *decode* (`decode-only`) |

Neither `sglang-gateway` nor `llm-d-router` keeps a running count per replica. Behind them, compare
the engine `Pod`s' own counts instead, such as each `Pod`'s *time to first token (TTFT)* histogram `_count`.

A scale event is visible in these series. On a *SGLang* multi-Server deployment scaled one to two,
the gateway's worker set grew to two `smg_worker_health` series at `1`; on the scale back, the
withdrawn worker's series stayed in the exposition at `-1` while
`smg_discovery_deregistrations_total{reason="pod_deleted"}` incremented, and the surviving worker
kept its `1`.

Count the healthy series, not the lines, on a scaled deployment. The same run's four content
classes served through the gateway with zero failures.

---

**See also** — [Prefill and Decode](prefill-decode.md) for *`spec.router`* and the flags each router refuses · [Encode, Prefill and Decode](encode-prefill-decode.md) for *EPD* disaggregation and connector options · [Model Deployment Metrics](metrics.md) for the router counters a deployment's snapshot reads.

**Next** → [Model Deployment Status](status.md)
