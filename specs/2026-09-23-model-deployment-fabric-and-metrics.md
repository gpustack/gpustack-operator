# Spec: ModelDeployment Fabric Requests and Metrics

Status: Shipped
Type: Feature

## Summary

Add an explicit interface quantity to each ModelDeployment role so its engine Pods can receive the fabric device grants needed by a shared cache, direct prefill/decode transfer, or both. Add a ModelDeployment metrics subresource that combines serving gauges and bounded-window latency, traffic, cache, and transfer signals from its router and engine Pods. Preserve the raw Pod metrics endpoints through scrape annotations. Validate the change with local checks and a numbered hardware verification matrix.

## Motivation

### Goals

- Let a deployment author request a positive integer number of fabric interfaces per role Pod and see the matching extended resource on the rendered engine container.
- Select the resource key from the effective fabric protocol: `device.gpustack.ai/rdma.shared` for one RDMA interface, `device.gpustack.ai/rdma` for multiple RDMA interfaces, and `vpc.amazonaws.com/efa` for EFA.
- Support direct prefill/decode transfer even when no shared cache backend is attached.
- Expose a unified, request-time view covering router processing, request queueing, cache hits, latency, traffic, and supported P/D transfer signals.
- Keep each Pod's own metrics discoverable by an ordinary Prometheus scraper.

### Non-Goals

- Do not wire the existing `ModelDeploymentReconciler.CacheScraper` or change `CacheAttached` in this spec.
- Do not make the operator install a PodMonitor or ServiceMonitor.
- Do not unify the two device plugins under one Kubernetes resource name or add a second public `rdma`/`efa` quantity.
- Do not provide topology guarantees beyond those of the selected device plugin.
- Do not resolve B1, the empty B4 slot, or the undecided B12 design here.

## Proposal

`spec.roles[].resources.interface` is an optional per-Pod integer quantity. An absent or zero value adds no fabric resource request. A positive value names one effective fabric family:

- A shared-cache leg uses the bound backend's effective member-group protocols. A positive request requires all groups to resolve to the same protocol, including when TCP is one of the offers; admission names conflicting protocols rather than selecting one group silently.
- A direct prefill/decode leg uses the protocol actually rendered for that engine and role. For a native vLLM pair this is `spec.kvTransfer.protocol`, with its existing unset value meaning TCP. A declared value that the selected engine ignores or does not render cannot select a device resource.
- If only one leg requires RDMA or EFA, use that family. If both require the same family, request it once. If both require different fabric families, admission rejects a positive interface request with an actionable error.
- A positive request without an effective RDMA or EFA leg is rejected rather than rendered as an unused device allocation.
- The quantity is part of the role's existing immutable resource identity. Existing deployments with the field absent keep their current rendering.

The EFA device may support RDMA-class data transfer, but the current Device Manager does not advertise it under this operator's RDMA keys. An EFA-enabled cluster using the EFA device plugin therefore renders `vpc.amazonaws.com/efa`, including for a pure prefill/decode deployment whose engine actually renders EFA transfer. The Kubernetes resource key follows the allocating plugin, not a generic description of the transport.

A `worker.gpustack.ai/v1` ModelDeployment view exposes a `metrics` subresource, following the existing Instance registration and authorization pattern. Its response is a structured snapshot of selected gauges and two-scrape windows with source, unit, freshness, and partial-read information. It aggregates compatible counts, but keeps incompatible latency and cache-hit definitions separately identified; it never presents an unweighted average of ratios as a deployment-wide fact. Each managed router and engine Pod also receives a scrape annotation for its own reachable metrics endpoint.

### User Stories

#### Story 1

As a deployment author, I want to request a count of fabric interfaces for each engine role, so that both shared-cache traffic and direct prefill/decode traffic can use the intended device rather than silently falling back.

#### Story 2

As an operator, I want one ModelDeployment metrics request to show router processing, queueing, and cache hits, so that I can understand serving behavior without assembling Pod addresses myself.

#### Story 3

As a verifier, I want local and hardware-only checks attached to the existing B identifiers, so that a later cluster run can close the right gaps without renumbering them.

### Core Features & Acceptance Criteria

#### A1: Role interface request

- The API accepts only non-negative whole interface quantities; admission reports the role and field for invalid values.
- RDMA quantity 1 renders one `device.gpustack.ai/rdma.shared` limit; RDMA quantity greater than 1 renders that quantity under `device.gpustack.ai/rdma`. This matches the current member-side allocation rule and does not treat multiple shared tokens as distinct interfaces.
- EFA quantity N renders N under `vpc.amazonaws.com/efa`; scheduling remains Pending when the device plugin advertises fewer than N.
- The rule works for a shared-cache-only deployment, a direct-transfer-only deployment, and a deployment using both legs. Equal fabric protocols produce one request; a TCP leg adds none; different fabric protocols with a positive interface request are rejected.
- A positive request with different effective member-group protocols is rejected, including a TCP and RDMA mix. The error identifies the conflicting protocols. A transfer protocol declared on a leg the engine does not render cannot cause an unused fabric grant.
- No fabric request is added when the field is absent or zero. Existing role resource and scheduling behavior stays valid.
- An operator-rendered engine Pod granted an interface can open the device, initialize its declared fabric transport, and move bytes. A Ready Pod or connector log alone is insufficient evidence; the test records the transport selected and a nonzero transfer or device-counter reading.

#### EFA investigation: independent prerequisite task

- Reconcile the existing EFA test reports with current detector and device-plugin behavior before the API implementation is finalized. Record separately: host RDMA devices, `Devices.spec.interfaces[]`, node allocatable keys, Pod device access, and transfer result.
- Verify the offline conclusion against the EFA plugin and libfabric contracts: EFA transport capability does not imply that this operator advertises an RDMA extended resource. Explain why `vpc.amazonaws.com/efa` is the current allocating key.
- State the boundary of existing measurements: controlled Pods have moved bytes across nodes without `hostNetwork`, but an operator-rendered engine Pod with the new request has not yet supplied the product acceptance reading.
- On a suitable cluster, test the operator-rendered engine path, at least one interface and a multi-interface-capable shape when available. Record the node's actual allocatable count, selected transport, byte movement, and any registration or scheduling failure. Do not turn an unavailable multi-interface shape into a passing result.
- Treat clusters exposing EFA only through Dynamic Resource Allocation, without `vpc.amazonaws.com/efa`, as outside this extended-resource contract and report the missing allocatable key clearly.

#### A2: Unified serving metrics view

- `GET .../modeldeployments/<name>/metrics` is authorized through the aggregated API and returns a typed snapshot for the named deployment. Missing objects and authorization failures retain ordinary API semantics.
- **Router processing:** expose at least an active or in-flight request gauge and a router availability or backend-processing gauge, with their source and scope. Accepted contract gap: the vLLM router's prefill/decode mode exports no router processing gauge, so that combination reports both as unsupported sources, neither zero nor stale; its engine processing gauges remain.
- **Request queueing:** expose the current waiting-request count, distinguishing a measured zero from a missing or stale sample.
- **Cache hits:** expose a measured hit gauge or bounded-window hit rate with its denominator, window, source, and cache scope. Local prefix-cache hits, router-cache hits, and external-store hits are not silently treated as identical.
- **Latency:** expose TTFT and inter-token latency (ITL) when the pinned engine emits them; expose request-level TPOT only from a source that measures it. Keep router and engine scopes distinct. Histogram means require two scrapes and report their sample counts and windows. Native Pod histograms remain available for percentile queries.
- **Traffic:** expose router request and error rates from counter deltas, plus an error fraction only when the counters share a compatible window and denominator.
- **P/D transfer:** expose the SGLang decode transfer queue, and the transfer latency, speed, size, and failures its prefill half records, when its pinned metrics emit them. vLLM connector telemetry is connector-dependent and has no common promised series here.
- All three information areas are present for each supported managed router/engine combination under a representative serving workload. Upstream metric names and exact cache-hit scopes may differ; an unsupported or unreadable sample is explicit rather than fabricated as zero.
- A failed Pod scrape does not discard successful samples. The response identifies missing sources and freshness; it does not label a partial sample complete. The all-unreadable case returns an actionable failure. A labeled failure or error counter absent while the same scrape read the counter it is explicitly paired with (a router's request counter, SGLang's transfer size) stays listed as missing but does not mark the response partial, because a labeled counter exports nothing before its first increment; no fraction is derived from it. An accepted unsupported source is listed the same way, including vLLM's external-store counters on a Pod that renders no KV connector. So are the latency and cache-hit sources of a Pod that served no request in the window, which its TTFT histogram shows by recording none, and SGLang's inter-token histogram absent beside that idle TTFT, since it is labeled and not exported before its first observation. A source that records nothing while TTFT moved still marks the response partial.
- Scrapes have bounded time, response size, and concurrency. Only Pods belonging to the requested deployment contribute.
- Every managed router and engine Pod advertises its own reachable metrics port and path through Prometheus scrape annotations. No PodMonitor or ServiceMonitor is created.
- The subresource is a structured current and bounded-window view. Pod endpoints retain their native Prometheus exposition for percentiles, longer time series, and source-specific metrics.

#### Documentation, comments, and e2e revision

- Revise existing API and renderer comments that say role resources contain only accelerators; document the interface count, protocol selection, and mixed-fabric refusal. Keep member-side resource comments consistent with the same quantity rule.
- Revise `docs/reference/model-deployment.md`, `docs/reference/model-deployment-status.md`, `docs/operation/rdma.md`, and `docs/architecture/network-topology.md` where old no-engine-fabric or metrics statements become false. Add a dedicated ModelDeployment metrics reference page if needed, then update `docs/README.md` and the docs skill routing table. Existing pages already at the section cap must not gain another `##` heading.
- Revise existing e2e case assumptions and fixtures affected by resource rendering or scrape annotations. Add discriminating A1 and A2 cases: absent versus positive interface, RDMA 1 versus multiple, EFA key, pure P/D, conflicting fabric protocols, all three gauge areas, partial scrape, and annotation reachability. A case must read the rendered Pod or API response, not infer success from the input manifest.

#### Verification matrix

| Environment | Existing item | Required reading |
| --- | --- | --- |
| Local with real controllers | B3 | Re-run the lease comparison with a bounded client call; retain the measured election-dominated caveat. |
| Local with real Kueue | B7 | Delete a surplus replica and observe whether Kueue releases its finalizer without a replacement. A fake-client test cannot settle this. |
| Local if a compatible election-flag image is available | B9 | Assert `ElectionObserved=True/Electing` and a Lease holder; do not assert against the old image workaround. |
| Fabric-capable cluster, first priority | B10 transfer | Show nonzero store transfer bytes and overturn or retain the recorded `allocator_used_bytes: 0` baseline; connector names and Ready status do not count. |
| Fabric-capable cluster | B6 | Record TTFT, moved block count, and evidence that decode did not recompute the prompt on an RDMA-capable node. |
| Two nodes with suitable RDMA interfaces | B8 | Measure actual byte distribution and performance with one versus multiple interfaces; discovery of two HCAs alone does not count. |
| Cluster running the store images | B11 | Exercise `0.3.11.post1-cpu` and the pipeline-built `0.3.10.post2-cpu` through real deployments. |
| Accelerator cluster | B2 | Determine whether direct-decode readiness checks the engine rather than merely a healthy sidecar. Keep any correction in that branch. |
| Accelerator cluster with metrics | B10 performance | Produce concurrent TPOT and later-round TTFT curves with fixed cache-hit rates; do not decide by one TTFT sample. |
| Accelerator cluster with metrics | B13 part 3 | Measure the performance of a prefill role serving a complete request separately from its correctness. |
| Only if an NPU cluster is acquired | B5 | Prove R1 serving before R2 cross-image segment reads; two started images alone do not count. |

B1 needs a separate node shape with a configurable kubelet and is excluded. B4 remains an empty identifier. B12 needs a product decision about leader scaling before a test can have a verdict and is excluded. Every B identifier keeps its original meaning.

### Notes / Constraints / Caveats

- The existing member renderer is the resource-name reference: one RDMA interface uses the shared key, multiple use the exclusive key, and EFA uses its plugin key.
- The offline EFA baseline separates five readings: hosts exposed `rdmap*` functions; the operator's `Devices.spec.interfaces[]` contained no matching EFA inventory; tested nodes advertised zero operator RDMA resources but one `vpc.amazonaws.com/efa`; a controlled Pod opened the plugin-granted device while an ungranted Pod could not; controlled Pods and a patched rendered cache member moved nonzero cross-node bytes with matching device counters. This proves the allocating key and transport capability on that shape, not ModelDeployment engine acceptance. Earlier nodes offered one EFA interface each, so multi-interface behavior remains unmeasured. The readings come from earlier controlled rounds on EFA-capable nodes of a managed Kubernetes cluster.
- A device grant proves allocation and access, not topology alignment or useful transfer. The existing EFA reports measured both successful controlled-Pod transfer and failure of the current engine rendering.
- Metrics source contracts must be checked against the pinned images used by the deployment; a local upstream checkout is not proof of a shipped image's metric names.
- The current router metric selector exposes queue and running gauges but does not establish a cache-hit source for every router and engine combination. A pinned-image metric spike must resolve this before the A2 schema and claims are locked; an unavailable source is reported to the user for a contract decision rather than mapped to zero or to cache utilization.
- Offline source inspection at the pinned engine refs found vLLM v0.25.1 running and waiting gauges plus separate local and external prefix-cache token hit/query counters; SGLang v0.5.18 running and queued gauges plus `prefill_effective_tokens_total` counters labeled `input`, `device_hit`, `host_hit` and `storage_hit`. A bounded-window hit ratio can divide counter deltas with the same source and token unit. SGLang's `cache_hit_rate` is a last-batch gauge and must not be averaged across Pods. Sources: [vLLM metrics logger](https://github.com/vllm-project/vllm/blob/v0.25.1/vllm/v1/metrics/loggers.py), [SGLang metrics collector](https://github.com/sgl-project/sglang/blob/v0.5.18/python/sglang/srt/observability/metrics_collector.py).
- Offline router inspection found llm-d router v0.10.0 `llm_d_epp_request_running` and pool queue gauges; vLLM router v0.1.15 `vllm_router_active_workers` and `vllm_router_running_requests`; SGLang gateway `gateway-v0.3.1` `smg_worker_pool_size` and `smg_worker_requests_active`. These are router-scope processing or worker-count signals, not substitutes for engine queueing, cache hits, or a backend health check. Sources: [llm-d metrics catalog](https://github.com/llm-d/llm-d-router/blob/v0.10.0/docs/metrics.md), [vLLM router metrics](https://github.com/vllm-project/router/blob/v0.1.15/src/metrics.rs), [SGLang gateway metrics](https://github.com/sgl-project/sglang/blob/gateway-v0.3.1/sgl-model-gateway/src/observability/metrics.rs). Image exposition and second-Pod reachability remain cluster checks in T8.
- Pinned source inspection also found vLLM and SGLang TTFT and ITL histograms, vLLM request-level TPOT, llm-d router TTFT and streaming TPOT, and SGLang gateway router TTFT and TPOT on its gRPC path. SGLang exposes P/D decode transfer queue, transfer latency, speed, size and failure counters. Router request and error counters exist in all three pinned routers. These are source-level contracts; actual image exposition and second-Pod reachability remain cluster checks in T8. Sources: [vLLM engine](https://github.com/vllm-project/vllm/blob/v0.25.1/vllm/v1/metrics/loggers.py), [SGLang engine](https://github.com/sgl-project/sglang/blob/v0.5.18/python/sglang/srt/observability/metrics_collector.py), [llm-d router](https://github.com/llm-d/llm-d-router/blob/v0.10.0/docs/metrics.md), [vLLM router](https://github.com/vllm-project/router/blob/v0.1.15/src/metrics.rs), [SGLang gateway](https://github.com/sgl-project/sglang/blob/gateway-v0.3.1/sgl-model-gateway/src/observability/metrics.rs).
- The Instance metrics subresource supplies a registration and fetch pattern, but its single-Pod JSON sample does not define ModelDeployment aggregation.
- Run code generation after API or webhook edits. Go and shell changes require `make lint`; Markdown changes require `make lint docs`. A change spanning both requires both checks.
- Cluster acceptance of cross-host direct transfer depends on two rendering corrections and one image limit. First, a `tcp` point-to-point leg is pinned rather than only requested: native vLLM receives a defaulted `MC_FORCE_TCP=1` and SGLang renders `--disaggregation-transfer-backend mooncake_tcp`, because the Mooncake transfer engine selects its transport from the host's hardware, and on hosts without RDMA a build with multi-node NVLink installs NVLink between hosts that have no NVLink path. Neither pin renders beside a store whose transport is not `tcp`, because the variable is process-wide and would leave the store client without its fabric. Second, an SGLang role with a store renders `--enable-hierarchical-cache` unless it starts as a decode half, which renders `--disaggregation-decode-retraction-backup cpu_tensor` instead; both are defaulted, and SGLang refuses a hierarchical host pool unless the node's host-wide available memory exceeds a fixed 10 GiB reserve plus the pool, so acceptance nodes are sized for it. The image limit: only Mooncake clients from 0.3.12 on read the pin. The measured vLLM 0.25.1 and 0.27.1 CUDA runner images embed 0.3.10.post2 and cannot run the leg over TCP between hosts without RDMA, so acceptance runs use vLLM 0.29.0, whose images embed 0.3.13.post1; SGLang 0.5.18 embeds 0.3.12.post1 and needs a store image on the 0.3.12 line.

### Boundaries

- **Always:** preserve existing A/B/C/D identifiers, inspect current source before using report coordinates, and keep committed text in English without environment identifiers.
- **Ask first:** any later change to the approved public interface quantity or mixed-fabric rule.
- **Never:** edit the source handoff, touch the separate topology worktree, install a PodMonitor, or treat Pod readiness as proof of fabric byte transfer.

### Risks and Mitigations

- EFA supports a fabric transport while the operator's RDMA inventory stays empty → select the actual device-plugin key and verify the rendered Pod's allocation.
- One interface count cannot express different quantities for two fabric families → reject a positive mixed-fabric request.
- Metrics can disappear or change names between image versions → pin source mappings to tested images and expose missing or stale samples explicitly.
- An engine may ignore the declared direct-transfer protocol → choose the resource key from the rendered leg and reject a positive count when no matching RDMA or EFA leg exists.
- Backend member groups can have different effective protocols → refuse a positive interface count and list the conflicting protocols.
- A device grant may still leave the engine unable to open or use the interface → verify the rendered Pod's access, selected transport, and nonzero transfer independently of readiness.
- Router replicas and engine roles can multiply scrape work → bound concurrency, time, and payload size; report partial coverage.
- A cache-hit ratio can mix incompatible meanings or denominators → preserve source and scope, and aggregate only compatible samples.
- A TLS engine may serve metrics with a certificate that the operator cannot verify for its Pod IP, or require a client certificate → advertise HTTPS and preserve certificate validation; report the unreadable source in `missing[]`. Native Prometheus scrapers may configure their own trust and client credentials.

## Design Details

### Commands

```bash
make generate
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... ./pkg/worker/extensionapis/worker/...
make test
make lint
make lint docs
make build
```

Hardware e2e cases are run only against an explicitly selected test cluster and namespace after their fixtures and case numbers are finalized.

Run generation, package tests, build, and lint in this local worktree. The local Go test environment was smoke-checked with `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/kvcache/router`. Run e2e only after an explicit Kubernetes context and namespace have been supplied; no cluster is assumed available now. `make generate` is required after API or webhook source edits, and its shared output prevents concurrent API-writing tasks.

### Project Structure

- `api/worker/v1alpha1/` owns the ModelDeployment request field and schema.
- `api/worker/v1/` and `pkg/worker/extensionapis/worker/` own the aggregated metrics view.
- `pkg/worker/controllers/worker/` owns role and router Pod rendering.
- `pkg/worker/webhooks/worker/` owns request validation.
- `pkg/nodefeature/` and `pkg/worker/kvcache/mooncake/` define the existing RDMA and EFA key contract.
- `docs/` and `.agents/skills/gpustack-operator-e2e/cases/` own user guidance and executable verification.

### Code Style

Existing resource fields use typed Kubernetes quantities and JSON/protobuf tags:

```go
Accelerator *resource.Quantity `json:"accelerator,omitempty" protobuf:"bytes,1,opt,name=accelerator"`
```

Use the same conventions for the interface quantity. Keep validation errors tied to the role's field path, preserve level-based reconciliation, and test observable Pod and API results with the repository's table-driven patterns.

### Implementation Plan

- [x] **T1 · EFA allocation and access evidence**
      Blocked by: None
      Owns: `pkg/worker/kvcache/mooncake/*test.go`, `specs/2026-09-23-model-deployment-fabric-and-metrics.md`
      Gate: review
      Acceptance: Recheck the plugin key and current device inventory against the existing controlled-Pod reports; distinguish host devices, `Devices.spec.interfaces[]`, node allocatable, Pod grant, and actual transfer. Record what still requires an operator-rendered Pod and a multi-interface node. Do not claim hardware acceptance from the offline evidence.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/kvcache/mooncake`

- [x] **T2 · Pinned-image metrics feasibility**
      Blocked by: None
      Owns: `pkg/worker/extensionapis/worker/testdata/model_deployment_metrics/**`
      Gate: review
      Acceptance: Capture or otherwise verify Prometheus exposition for each supported managed router and engine combination at the pinned image versions. Map processing, queueing, and cache-hit samples by name, type, scope, unit, and denominator or window. Identify missing sources and test reachability from a second Pod. If any promised cache-hit view is unavailable, return for a public-contract decision before T4.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/extensionapis/worker`

- [x] **T3 · Role interface request from API to Pod**
      Blocked by: T1
      Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/zz_generated*.go`, `pkg/kubeclients/**/modeldeployment*.go`, `pkg/worker/webhooks/worker/model_deployment*.go`, `pkg/worker/controllers/worker/model_deployment.go`, `pkg/worker/controllers/worker/model_deployment_binding.go`, `pkg/worker/controllers/worker/model_deployment_connector.go`, `pkg/worker/controllers/worker/model_deployment_render.go`, `pkg/worker/controllers/worker/model_deployment_*test.go`, `pkg/worker/kvcache/mooncake/*.go`
      Gate: review
      Acceptance: A nonnegative whole per-role count passes validation and joins immutable resource identity; a negative or fractional count names the role field. Effective cache-group and rendered direct-leg protocols select one key, or reject mixed protocols and a count without a usable fabric leg. Pod limits show RDMA shared for one, RDMA exclusive for multiple, or EFA for N; absent and zero preserve old output. Cover cache only, direct P/D only, both legs, and ignored declared protocols.
      Verify: `make generate` and `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/webhooks/worker ./pkg/worker/controllers/worker ./pkg/worker/kvcache/mooncake`

- [x] **T4 · ModelDeployment metrics vertical path**
      Blocked by: T2, T3
      Owns: `api/worker/v1/model_deployment.metrics.go`, `api/worker/v1/zz_generated*.go`, `pkg/worker/extensionapis/worker/model_deployment.go`, `pkg/worker/extensionapis/worker/model_deployment.metrics*.go`, `pkg/worker/extensionapis/setup.go`
      Gate: review
      Acceptance: For one source combination proved by T2, the authorized `worker.gpustack.ai/v1` `/metrics` subresource returns a typed snapshot for the named deployment with processing, queueing, and cache-hit data and explicit source, unit, freshness, scope, and missing-sample semantics. Missing deployment and denied access retain ordinary API behavior. Tests read the response object.
      Verify: `make generate` and `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/extensionapis/worker ./pkg/worker/extensionapis/...`

- [x] **T5 · Metrics combinations and partial reads**
      Blocked by: T4
      Owns: `pkg/worker/extensionapis/worker/model_deployment.metrics*.go`, `pkg/worker/extensionapis/worker/testdata/model_deployment_metrics/**`
      Acceptance: Every promised router and engine combination reports the three areas under a representative workload, or an explicit unresolved source gap goes back to the user. Aggregate only compatible counts, preserve cache-hit scope and denominator, distinguish zero/missing/stale, and bound scrape time, payload, and concurrency. Tests cover multiple replicas, a failed Pod, all-unreadable Pods, and exclusion of unrelated Pods.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/extensionapis/worker`

- [x] **T6 · Pod scrape discovery and reachability**
      Blocked by: T3
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go`, `pkg/worker/controllers/worker/model_deployment_router.go`, `pkg/worker/controllers/worker/model_deployment_render_test.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`
      Acceptance: Each managed engine and router Pod advertises a reachable metrics port and path through Prometheus annotations, including router variants, custom serving ports, and multi-container Pods. A second Pod can fetch the advertised target in cluster verification; local tests inspect the rendered annotation and listener configuration. No monitoring CR is installed.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/controllers/worker`

- [x] **T7 · Documentation and e2e contract revision**
      Blocked by: T3, T5, T6
      Owns: `docs/reference/model-deployment*.md`, `docs/operation/rdma.md`, `docs/architecture/network-topology.md`, `docs/README.md`, `.agents/skills/gpustack-operator-docs/SKILL.md`, `.agents/skills/gpustack-operator-e2e/cases/**`
      Acceptance: API and rendering comments, referenced docs, index, and e2e assumptions state the implemented interface and metrics behavior. Discriminating cases inspect rendered Pods and `/metrics` responses for absent/positive requests, RDMA one/multiple, EFA, pure P/D, mixed protocols, the three gauge areas, partial reads, and annotation reachability. Existing B identifiers remain stable.
      Verify: `make lint docs` and `make lint`

- [x] **T8 · Cluster acceptance and B evidence over TCP**
      Blocked by: T3, T5, T6, T7, and an explicitly selected Kubernetes context and namespace with suitable hardware
      Owns: `specs/2026-09-23-model-deployment-fabric-and-metrics.md`
      Gate: review
      Acceptance: Record the operator-rendered Pod's actual device access, selected transport, nonzero byte transfer, and the served three-area metrics response. Execute the B matrix rows whose stated environments exist; keep other rows pending and do not infer a pass from a Ready Pod, connector log, HCA discovery, or one latency sample. Record multi-interface availability and failures explicitly.
      Verify: Run the numbered e2e cases with the selected context and namespace; attach Pod resource limits, allocatable counts, transport and byte readings, metric response, and B-case measurements to the report.

- [ ] **T9 · Fabric transfer acceptance on an interface-granting cluster**
      Blocked by: T8, and a cluster whose engine images carry a fabric-capable transfer client
      Owns: `specs/2026-09-23-model-deployment-fabric-and-metrics.md`
      Acceptance: An operator-rendered engine Pod with a positive interface count opens its granted device, selects the fabric transport, and moves nonzero bytes with matching device counters; a multi-interface shape is measured when one is available.
      Verify: Run `case-89` with its hardware branch on such a cluster and record limits, allocatable counts, selected transport, byte and device-counter readings.

T9 is outside what this spec ships. The runner CUDA engine images embed a Mooncake transfer client built without EFA support, so no rendering change can make those engines select EFA; the remaining rows need either such an image or an RDMA cluster. The shipped scope is the interface request and its allocation, the metrics view, and the TCP cluster acceptance recorded below.

An EFA run on two single-GPU nodes with one EFA interface each, in one node group, measured both sides of that image limit. T9 stays open because it depends on an engine image the runner does not ship:

- With the pinned vLLM 0.29.0 runner image, an operator-rendered engine granted the interface found the device and then failed to initialize its transfer engine (`Failed to create completion queue: Operation not supported`), on a direct prefill/decode leg and on a store-only leg alike, so neither deployment ever served. Without the grant, the same image falls back to TCP silently.
- A test image that differs from the pinned runner only by replacing its Mooncake transfer engine with the same version's EFA build, plus libfabric, selected EFA in operator-rendered Pods whose only fabric-related rendering was the device grant: no host network, no extra mounts, no added capabilities.
- On that image's direct prefill/decode leg, the prefill engine reported nonzero transferred bytes (7,864,320 for a 10-request batch, no failed transfers), and the EFA send byte counter on the prefill node equalled the receive byte counter on the decode node for the same batch.
- On that image's store-only leg, with engine and store member on different nodes, the engine's store write bytes for 8 requests equalled the growth of the member's `allocator_used_bytes` (6,094,848 bytes), and the EFA send and receive byte counters on the two nodes were equal.
- An operator-rendered Pod without the grant could not open the device although the host exposed it, and admission refused an EFA store combined with an RDMA direct leg, naming the two fabrics.
- Still pending: both legs together, because this shape has one EFA slot per node against the three that a prefill engine, a decode engine and a store member need, and because the vLLM MultiConnector is blocked upstream; a multi-interface shape; the store read path over EFA; and whether the engine's GPU buffers moved by GPUDirect or were staged through host memory.

T1 and T2 can proceed independently. T4 follows T3 because both modify shared API generation output. T5 and T6 can proceed independently after their stated dependencies. Each implementation task must leave focused tests green; after T7, run `make test`, `make build`, `make lint`, and `make lint docs` locally before cluster acceptance.

Cluster acceptance ran on a managed Kubernetes cluster with two single-GPU nodes without RDMA, prefill and decode on different nodes, CPU nodes hosting the store members, and the transfer leg on TCP:

- vLLM 0.29.0 prefill/decode with no store moved nonzero bytes through Mooncake (`vllm:mooncake_bytes_transferred_sum` above zero, zero failed transfers), with the operator-rendered `MC_FORCE_TCP=1` on both engines and no NVLink transport selected, behind both the llm-d router and the vLLM router.
- SGLang 0.5.18 prefill/decode with no store and with a Mooncake DRAM store served routed requests with nonzero transfer size and nonzero store batch writes, rendering `mooncake_tcp`, the decode `cpu_tensor` retraction backup, and the prefill hierarchical cache only where a store is attached.
- vLLM 0.29.0 with both the prefill/decode connector and the store connector failed in the engine: the decode side exits on its first request with an upstream store-scheduler assertion, so that combination and the MultiConnector transfer-metrics check remain blocked upstream.
- The metrics subresource returned all three information areas for vLLM with the llm-d router, vLLM with the vLLM router, SGLang with the SGLang gateway, and SGLang with the gateway and a store; every annotated engine and router endpoint was fetched from a second Pod; a stopped engine produced a partial snapshot that kept the other sources.
- The B matrix readings: direct-decode readiness checks the engine through its sidecar (B2); a prefill engine completes a full request (B13, single-stream performance only); store writes are nonzero over TCP (B10 transfer, TCP only); the two older store images could not start their master until the leader stopped rendering pod identity flags outside an election (B11). B6, B8 and B10 performance remain pending.


Local image inspection confirmed `vllm` package version `0.25.1+cu129` in a pinned runner image and found the selected running, waiting, local and external prefix-cache token counters, TTFT, ITL, and request-level TPOT definitions inside that image. A local run of the pinned router bundle emitted `vllm_router_active_workers 1` during startup with a configured but unreachable worker, while the SGLang gateway returned an empty HTTP 200 `/metrics` body with no worker. These are actual idle endpoint readings, not evidence of the promised series under serving traffic; in particular, `active_workers` alone does not prove backend health. The bundled llm-d endpoint picker exited before binding its metrics listener when started without Kubernetes credentials; it needs a Kubernetes REST configuration even with inline plugin configuration. A read-only inspection of an available test host found a compatible vLLM runner image but no running engine or router container and no existing metrics fixture. The remaining T2 acceptance needs representative traffic on the pinned engines and all managed routers, plus a fetch from a second Pod. Python argument parsers in the pinned vLLM and SGLang versions accept unique long-option prefixes; the operator's listener and ownership checks currently recognize full spellings, so abbreviated user flags can bypass those checks. This is a known local validation gap, not evidence that an abbreviated option was exercised in the endpoint readings.

An actual pinned SGLang gateway HTTP request returned `503 no_available_workers` and emitted `smg_router_requests_total` and `smg_http_responses_total{status_code="503"}`, but no `smg_router_request_errors_total`. The HTTP response counter has no request-endpoint label, so its 5xx rate is reported separately instead of dividing it by routed requests. A sanitized subset of that exposition is a parser regression fixture. This local refusal is not representative successful serving traffic and does not close T2.

The pinned gateway also forwarded successful non-streaming and SSE chat requests to a local stub worker, emitting routed-request counters and request-duration histogram samples. It emitted no router TTFT or TPOT samples. The pinned source defines those series for its gRPC path, while the managed gateway's service-discovery arguments provide no `grpc://` worker URL and therefore select HTTP mode. The aggregated snapshot does not require or promise those gRPC-only series for this managed router. This stub does not validate engine metrics or complete T2's representative-load and second-Pod checks.

A pinned vLLM router locally forwarded one successful chat request and then returned an upstream 500 and a no-worker 503. Its `vllm_router_requests_total` counted only the success; the upstream 500 emitted `vllm_router_retries_exhausted_total` but no `vllm_router_request_errors_total`, while the no-worker 503 emitted both failure counters. The router also kept `active_workers=1` with `worker_health=0`. The aggregated snapshot therefore names the first counter `successful-requests`, reports the two failure counters separately, and does not divide router errors by successful requests. A sanitized subset of the upstream-failure exposition is a regression fixture. This stub does not establish engine behavior or a complete user-request failure rate.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Preserve current resource-render and member-protocol cases as baselines; add a case where the backend has TCP and RDMA member groups so a first-offer choice fails visibly.
- Capture pinned-image metric exposition fixtures before writing parsers. Include a valid zero, absent sample, malformed sample, and cache-hit sample with its denominator or window. If the image cannot supply the promised hit measurement, resolve the API contract with the user before implementation.
- Separate offline EFA plugin-key checks from the later operator-rendered Pod transfer test. Existing controlled-Pod transfer reports are evidence of capability, not completion of A1.

#### Unit tests

- `pkg/worker/webhooks/worker`: 2026-09-23 - target at least 80% coverage of added validation branches; table cases for negative, fractional, zero, pure P/D, mixed legs, mixed member groups, and ignored protocol.
- `pkg/worker/controllers/worker`: 2026-09-23 - target at least 80% coverage of added resource and annotation branches; assert the actual engine limit key and quantity, immutable resource comparison, router/engine annotations, and unchanged absent-field rendering.
- `pkg/worker/kvcache/mooncake`: 2026-09-23 - target at least 80% coverage of added protocol selection logic; distinguish each member group and plugin key.
- `pkg/worker/extensionapis/worker`: 2026-09-23 - target at least 80% coverage of added parser and aggregation branches; test compatible sums, incompatible cache scopes, zero versus absent, freshness, partial and all-failed reads, unrelated Pods, timeout, size, and concurrency bounds.

#### Integration tests

- With fake API clients, read an authorized ModelDeployment metrics subresource and inspect the typed response and ordinary not-found and forbidden behavior.
- Exercise generated CRD/schema and webhook validation with a role quantity and a bound backend; read the resulting Pod limit rather than restating the input.
- Check annotations against actual listener ports for each managed router and engine shape. Keep real device-plugin access in e2e because a fake client cannot grant a device.

#### e2e tests

- On a specifically named test cluster and namespace, run A1 with RDMA one and multiple, EFA, cache only, direct P/D only, both legs, and mixed-protocol rejection. Record allocatable resources, Pod limits, device open, selected transport, and nonzero transferred bytes. An unavailable multi-interface node leaves that case pending.
- Under representative serving traffic, fetch the ModelDeployment metrics subresource and each annotated Pod endpoint from a second Pod. Verify processing, waiting count, and cache-hit source, scope, denominator or window; simulate a failed scrape and verify partial status.
- Run B3, B7, and B9 locally only where their specified real-controller, Kueue, or compatible-image prerequisites exist. Run B2, B5, B6, B8, B10, B11, and B13 only with the environment named in the verification matrix. Keep B1, B4, and B12 outside this plan.

## Alternatives

- Two public quantities, `rdma` and `efa`: rejected because the user asks for one interface count and the effective protocol already identifies the allocating plugin.
- Infer the resource key from node inventory alone: rejected because a mixed cluster can advertise different keys and a node's transport capability is not an allocation grant.
- Request both RDMA and EFA keys when both legs differ: rejected because one count cannot express the intended quantity of each.
- Pod metrics annotations alone: rejected because they do not provide the requested ModelDeployment entry point.
- Prometheus text as the aggregated subresource: rejected for this selected gauge snapshot; the annotated Pod endpoints retain raw text for Prometheus users.

## Open Questions

None about the requested public contract. Exact source metric mappings and any image-specific gaps are verification work and must be resolved before claiming the three A2 information areas complete.
