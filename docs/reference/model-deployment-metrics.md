# Model Deployment Metrics Reference

> **Purpose** — the structured `ModelDeployment` metrics snapshot and the managed Pods' raw
> Prometheus scrape endpoints.
> **Audience** users, operators, console developers · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** ~7 min

The aggregated API combines selected current gauges from a deployment's engine and router Pods.
Each managed Pod also exposes its own native `/metrics` output for Prometheus users.

## Contents

- [The subresource](#the-subresource)
- [Fields and sources](#fields-and-sources)
- [What each field counts](#what-each-field-counts)
- [Windowed cache hits](#windowed-cache-hits)
- [Latency, traffic, and transfer](#latency-traffic-and-transfer)
- [Missing and partial samples](#missing-and-partial-samples)
- [Scraping the Pods](#scraping-the-pods)

## The subresource

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/modeldeployments/<name>/metrics"
```

The response is one `ModelDeploymentMetrics` object, read when requested. Access requires the
aggregated API's `get` permission on `modeldeployments/metrics`; a missing deployment returns the
ordinary not-found error. The response carries `metadata`, `timestamp`, `processing[]`,
`queueing[]`, `cacheHits[]`, `latency[]`, `traffic[]`, `transfer[]`, `missing[]` and `partial`.

`timestamp` is when the response was assembled. Each gauge and hit entry has an `observedAt` time
for its HTTP read. It is a scrape time, not a producer-side timestamp; these engines do not attach
sample timestamps to the selected Prometheus series.

## Fields and sources

`processing[]` holds `running`, `router-running`, `router-backends` and `router-reported-workers`;
`queueing[]` holds `waiting` and `decode-transfer-waiting`. Engine gauges are summed by role over
readable Pods. The unit is `requests`, except `backends` for `router-backends` and `workers` for
`router-reported-workers`, a count that alone does not prove a worker reachable or ready. Which
series each field reads is in [What each field counts](#what-each-field-counts).

Every gauge includes its exact Prometheus `source`, `scope` (`engine` or `router`), `unit`, numeric
`value`, `podCount` and `observedAt`. Sources are kept separate: a router's view of active requests
is never added to the engine's view. Prefill and decode roles also stay separate. A measured `0`
is present as zero.

## What each field counts

Each cell names the series a field reads, without its `vllm:` or `sglang:` prefix, and what it
counts there. A dash means the field is not read there. vLLM-Ascend Pods are read as vLLM; no
vLLM-Ascend snapshot has been checked.

| Field | vLLM | SGLang | Read from |
|---|---|---|---|
| `running` | `num_requests_running`: requests in the running batch | `num_running_reqs`: requests in the running batch | every role |
| `waiting` | `num_requests_waiting`: requests not yet scheduled, including those deferred while their KV blocks arrive | `num_queue_reqs`: the scheduler's waiting queue alone | every role |
| `decode-transfer-waiting` | — | `num_decode_transfer_queue_reqs`: requests whose KV blocks are still arriving | decode of a pair |
| `local-prefix` | `prefix_cache_*`: prompt tokens found in the Pod's own KV cache | — | every role |
| `external-store` | `external_prefix_cache_*`: of the tokens not found locally, those the KV connector supplies | — | every role but decode of a pair, when the deployment attaches a KV cache pool |
| `device-prefix`, `host-prefix`, `storage-prefix` | — | `prefill_effective_tokens_total` by mode: prefill tokens found on the device, in host memory, and in the storage backend | every role but decode of a pair; `host-prefix` and `storage-prefix` only where the Pod builds that tier |
| `ttft` | `time_to_first_token_seconds` | `time_to_first_token_seconds` | vLLM every role; SGLang every role but prefill of a pair |
| `tpot` | `request_time_per_output_token_seconds`: zero for a request with at most one output token | — | every role but prefill of a pair |
| `itl` | `inter_token_latency_seconds` | `inter_token_latency_seconds`, exported once a request streams | every role but prefill of a pair |
| `transfer-*` | — | `kv_transfer_latency_ms`, `kv_transfer_speed_gb_s`, `kv_transfer_total_mb`, `num_transfer_failed_reqs_total` | prefill of a pair |

**vLLM `external-store` is not read where it does not measure the store.** vLLM counts every
token any KV connector supplies as an external hit, the point-to-point leg of a pair included.
Decode's leg supplies the blocks the prefill half sends, so every token decode lacks counts as a hit
and the rate stays at one, with or without a store; it is not read on decode.

The prefill half's leg never supplies a token, so with a pool that half's rate is the store's, and
without one it is not read. Measured on a pair with a store and on one without: decode hits
equalled queries, prefill hits were zero.

The prefill half of a vLLM pair answers the router's one-token prefill request, so its `ttft`
counts each paired request once. Its `tpot` is not read: with no second token its mean is always
zero. A vLLM decode half's `waiting` includes requests deferred until the prefill half's blocks
arrive.

SGLang keeps those in separate queues: decode's transfer queue is `decode-transfer-waiting`; its
preallocation queue, and prefill's bootstrap and in-flight queues, are not read.

**SGLang `host-prefix` and `storage-prefix` are read only where the Pod builds that tier.** SGLang
exports every hit mode from its start, and a tier the Pod does not build stays at zero, which would
read as a measured miss. Elsewhere `missing[]` names the scope as an unsupported source.

`host-prefix` is read where the Pod's arguments select a cache with a host tier:
`--enable-hierarchical-cache`, which the operator renders with a KV cache pool, or a user's own
`--enable-lmcache`, `--enable-flexkv` or `--radix-cache-backend flexkv`. `storage-prefix` is read
where the deployment attaches a pool, which renders the storage backend.

| Field | `llm-d-router` | `vllm-router` | `vllm-router`, P/D mode | `sglang-gateway` |
|---|---|---|---|---|
| `router-running` | `llm_d_epp_request_running`: requests the router is handling | `vllm_router_running_requests` | not exported | `smg_worker_requests_active`, summed over workers: requests in flight to a worker |
| `router-backends` | `llm_d_epp_ready_endpoints`: endpoints it holds ready | — | — | `smg_worker_pool_size`: workers in its pool |
| `router-reported-workers` | — | `vllm_router_active_workers` | not exported | — |
| `requests` | `llm_d_epp_request_total` | — | — | `smg_router_requests_total` |
| `successful-requests` | — | `vllm_router_requests_total`: completed successes | — | — |
| `pd-requests` | — | — | `vllm_router_pd_requests_total` | — |
| `request-errors`, `errors`, `pd-errors` | `llm_d_epp_request_error_total` | `vllm_router_request_errors_total` | `vllm_router_pd_errors_total` | `smg_router_request_errors_total` |
| `retries-exhausted` | — | `vllm_router_retries_exhausted_total` | — | — |
| `error-ratio` | — | — | — | `errors` over `requests` |
| `http-5xx-responses` | — | — | — | `smg_http_responses_total` with a 5xx `status_code` |
| `ttft`, `tpot` | `llm_d_epp_request_ttft_seconds`, `llm_d_epp_request_streaming_tpot_seconds` | — | — | — |

Of the error series only `llm_d_epp_request_error_total` has been seen exported. The other four are
labeled counters that export nothing before their first increment, and none has been seen
exported, so what each counts has not been checked against a failed request. Why each router has or lacks `error-ratio` is
in [Latency, traffic, and transfer](#latency-traffic-and-transfer).

Where a router sent each request is not in the snapshot; its own per-replica series say, listed in
[Seeing where requests went](model-deployment-routing.md#seeing-where-requests-went).

## Windowed cache hits

`cacheHits[]` contains one entry per readable engine Pod and cache scope. It carries `pod`,
`source`, `scope`, `unit: tokens`, `hits`, `queries`, `rate`, `windowSeconds` and `observedAt`.
`rate` is `hits / queries` from two reads of the same Pod's counters. A first read establishes the
baseline; a second read within five minutes supplies the window. A counter reset or a window with
no new queries produces a `missing[]` entry rather than a fabricated zero rate.

vLLM reports `local-prefix` and, on a Pod that attaches a KV cache pool and is not the decode half
of a pair, `external-store` from separate prefix-cache hit and query counters.

SGLang reports `device-prefix` and, where the Pod builds those tiers, `host-prefix` and
`storage-prefix` from `prefill_effective_tokens_total` modes. Each SGLang tier uses the sum of
`input` and all three hit modes as its denominator. Ratios with different scopes or sampling windows stay per Pod; they are
not averaged into one deployment-wide cache-hit claim.

vLLM exports its external prefix cache counters without a KV connector too, and they never move. On
a Pod whose arguments render no connector and whose external query counter has never moved,
`missing[]` names `external-store` as an unsupported source rather than a window with no queries.
A Pod with a connector that is not read, as described in
[What each field counts](#what-each-field-counts), is named the same way with its own reason.

## Latency, traffic, and transfer

`latency[]` reports TTFT and inter-token latency (ITL) from vLLM and SGLang engine histograms.
vLLM also reports request-level TPOT. The llm-d router reports TTFT and streaming TPOT. The managed
SGLang gateway uses HTTP service discovery, while its router TTFT and TPOT series are gRPC-only;
the vLLM router does not supply those series in the pinned version. Router measurements include
more of the request path than engine measurements; their scopes stay separate.

`traffic[]` reports router requests per second, router-source errors per second, and their error
fraction when the counters share a sampling window and a denominator. For the SGLang gateway it
also reports `http-5xx-responses` per second from `smg_http_responses_total`.

That counter covers gateway HTTP responses across paths, including a `503 no_available_workers`
response that its router error counter does not record. Its labels do not identify the routed
request endpoint, so the API keeps this rate separate from the routed-request error fraction.

For the llm-d router, `requests` comes from `llm_d_epp_request_total` and `request-errors` from
`llm_d_epp_request_error_total`. That error counter also counts requests the request counter never
records, such as a bad request that names no model, so this router has no `error-ratio` entry.

For the vLLM router, `successful-requests` counts only completed successes. `errors` is its own
router error counter and misses some upstream failures. `retries-exhausted` records exhausted
attempts, including a failure on the first attempt. These counters do not supply a compatible
all-request denominator, so this router has no `error-ratio` entry.

Behind a prefill/decode pair the vLLM router runs its P/D mode, which records only its `pd_*`
series: `pd-requests` from `vllm_router_pd_requests_total` and `pd-errors` from
`vllm_router_pd_errors_total`. That error counter also counts refusals that never reach the request
counter, so there is no `error-ratio`. The mode exports no running-request or worker gauge, so
`missing[]` names both as unsupported sources; the engine processing gauges are unaffected.

`transfer[]` reports SGLang P/D KV transfer latency, speed, size and failures per second when
emitted, read from the prefill half, which sends the blocks and records them. vLLM connector metrics depend on the selected connector, so this API does not
promise a common transfer measurement for vLLM P/D.

Each entry names its Pod, source, scope, unit, value, sample count, window duration and read time.
Values from histograms are means of new observations between two reads. Rate values use the delta
of counters over the same interval. A first read, counter reset or window without observations is
reported in `missing[]`. The API does not calculate p95 or p99 from a single scrape; retain the
native Pod histograms in Prometheus for percentiles.

vLLM's request-level TPOT includes zero-valued
observations for requests with at most one output token, while ITL measures gaps between streamed
outputs. Compare like sources and roles. The prefill role of a prefill/decode pair answers with its
first token alone, so neither ITL nor TPOT is read from it and neither absence is reported in
`missing[]`.

In an SGLang pair TTFT is read from the decode half, which records it, and cache hits from the
prefill half alone: the decode half never prefills, so its token counters hold no ratio.

## Missing and partial samples

`partial: true` means at least one required source did not contribute. Each `missing[]` entry names
the Pod or source and the reason: an absent metric, an unreadable endpoint, missing owned replicas, a
first counter sample, a reset, incompatible request and error windows, or a window with no queries.
A successful Pod remains in the result when another fails. If no owned Pod can be read, the
subresource returns Service Unavailable.

Three kinds of `missing[]` entry leave `partial` unset. Each is listed with its reason, and none is
reported as zero. The first is a labeled counter that exports nothing before its first increment,
absent while the same scrape read its pair: a router's error counter or the vLLM router's
retries-exhausted counter beside the router's request counter, and SGLang's transfer failure
counter beside the transfer size. No fraction is derived from it.

The second is a source the deployment's shape does not provide: the vLLM router's P/D processing
gauges, `external-store` on a vLLM Pod without a KV connector or without a pool, or on the decode
half of a vLLM pair, and an SGLang tier the Pod does not build, all described above.

The third is a Pod that served no request in the sampling window, which its TTFT histogram shows by
recording none. Its latency histograms and cache-hit counters then have no new sample and are listed
as an idle sampling window. SGLang exports its inter-token histogram only once a request streams, so
an idle SGLang server that has answered nothing but its warmup request lists it as not exported yet.

Every other entry sets `partial`, on an idle Pod as on a busy one: an unreadable endpoint, a first
sample, a reset, or an absent TTFT histogram. A latency or cache-hit source that records nothing
while TTFT moved, or an SGLang inter-token histogram absent while TTFT moved, also sets it.

A request reads at most 64 owned Pods, with 32 concurrent fetches, a two-second limit per fetch,
an eight-second whole-request limit and a 1 MiB response cap per Pod. Two rounds of fetches fit in
half the request limit, so Pods queued behind hanging ones are still read. Reaching the Pod limit
sets `partial` and records the omitted coverage in `missing[]`.

## Scraping the Pods

Managed engine and router Pods advertise `prometheus.io/scrape: "true"`,
`prometheus.io/path: /metrics`, `prometheus.io/port` with their actual metrics listener port,
and `prometheus.io/scheme` matching the listener's HTTP or HTTPS transport.

The aggregated API validates HTTPS certificates. A TLS engine whose certificate is not trusted
for its Pod IP, or which requires a client certificate, appears in `missing[]`. Configure trust
and client credentials separately when scraping native Pod endpoints.

The direct-decode proxy does not replace the engine target: its Pod advertises the engine's own
port. A role with a user-supplied `command` owns its endpoint and receives no managed scrape
annotation. The operator installs no `PodMonitor` or `ServiceMonitor`.

---

**See also** — [Model Deployment Reference](model-deployment.md) for role configuration ·
[Instance Metrics Reference](instance-metrics.md) for the separate Instance utilization API.

**Next** → [Model Deployment Status Reference](model-deployment-status.md)
