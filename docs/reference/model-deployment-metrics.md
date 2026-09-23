# Model Deployment Metrics Reference

> **Purpose** — the structured `ModelDeployment` metrics snapshot and the managed Pods' raw
> Prometheus scrape endpoints.
> **Audience** users, operators, console developers · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** ~5 min

The aggregated API combines selected current gauges from a deployment's engine and router Pods.
Each managed Pod also exposes its own native `/metrics` output for Prometheus users.

## Contents

- [The subresource](#the-subresource)
- [Fields and sources](#fields-and-sources)
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

| Area | Response | Source and scope | Unit |
|---|---|---|---|
| processing | `processing[].name: running` | vLLM `num_requests_running` or SGLang `num_running_reqs`, summed by engine role over readable Pods | requests |
| processing | `router-running` | llm-d `request_running`, vLLM router `running_requests`, or SGLang gateway `worker_requests_active` | requests |
| processing | `router-backends` | llm-d `ready_endpoints` or SGLang gateway `worker_pool_size` | backends |
| processing | `router-reported-workers` | vLLM router `active_workers`; this count alone does not prove a worker is reachable or ready | workers |
| queueing | `queueing[].name: waiting` | vLLM `num_requests_waiting` or SGLang `num_queue_reqs`, summed by engine role over readable Pods | requests |
| queueing | `decode-transfer-waiting` | SGLang P/D decode transfer queue | requests |

Every gauge includes its exact Prometheus `source`, `scope` (`engine` or `router`), `unit`, numeric
`value`, `podCount` and `observedAt`. Sources are kept separate: a router's view of active requests
is never added to the engine's view. Prefill and decode roles also stay separate. A measured `0`
is present as zero.

## Windowed cache hits

`cacheHits[]` contains one entry per readable engine Pod and cache scope. It carries `pod`,
`source`, `scope`, `unit: tokens`, `hits`, `queries`, `rate`, `windowSeconds` and `observedAt`.
`rate` is `hits / queries` from two reads of the same Pod's counters. A first read establishes the
baseline; a second read within five minutes supplies the window. A counter reset or a window with
no new queries produces a `missing[]` entry rather than a fabricated zero rate.

vLLM reports `local-prefix` and, when available, `external-store` from separate prefix-cache hit
and query counters. SGLang reports `device-prefix`, `host-prefix` and `storage-prefix` from
`prefill_effective_tokens_total` modes. Each SGLang tier uses the sum of `input` and all three hit
modes as its denominator. Ratios with different scopes or sampling windows stay per Pod; they are
not averaged into one deployment-wide cache-hit claim.

## Latency, traffic, and transfer

`latency[]` reports TTFT and inter-token latency (ITL) from vLLM and SGLang engine histograms.
vLLM also reports request-level TPOT. The llm-d router reports TTFT and streaming TPOT. The managed
SGLang gateway uses HTTP service discovery, while its router TTFT and TPOT series are gRPC-only;
the vLLM router does not supply those series in the pinned version. Router measurements include
more of the request path than engine measurements; their scopes stay separate.

`traffic[]` reports router requests per second, router-source errors per second, and their error
fraction when the counters have compatible windows. For the SGLang gateway it also reports
`http-5xx-responses` per second from `smg_http_responses_total`.

That counter covers gateway HTTP responses across paths, including a `503 no_available_workers`
response that its router error counter does not record. Its labels do not identify the routed
request endpoint, so the API keeps this rate separate from the routed-request error fraction.

For the vLLM router, `successful-requests` counts only completed successes. `errors` is its own
router error counter and misses some upstream failures. `retries-exhausted` records exhausted
attempts, including a failure on the first attempt. These counters do not supply a compatible
all-request denominator, so this router has no `error-ratio` entry.

`transfer[]` reports SGLang P/D decode KV transfer latency, speed, size and failures per second
when emitted. vLLM connector metrics depend on the selected connector, so this API does not
promise a common transfer measurement for vLLM P/D.

Each entry names its Pod, source, scope, unit, value, sample count, window duration and read time.
Values from histograms are means of new observations between two reads. Rate values use the delta
of counters over the same interval. A first read, counter reset or window without observations is
reported in `missing[]`. The API does not calculate p95 or p99 from a single scrape; retain the
native Pod histograms in Prometheus for percentiles.

vLLM's request-level TPOT includes zero-valued
observations for requests with at most one output token, while ITL measures gaps between streamed
outputs. Compare like sources and roles.

## Missing and partial samples

`partial: true` means at least one required source did not contribute. Each `missing[]` entry names
the Pod or source and the reason: an absent metric, an unreadable endpoint, missing owned replicas, a
first counter sample, a reset, incompatible request and error windows, or a window with no queries.
A successful Pod remains in the result when another fails. If no owned Pod can be read, the
subresource returns Service Unavailable.

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
