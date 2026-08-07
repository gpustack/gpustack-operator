# Spec: Instance Utilization Metrics (CPU/RAM/DISK/GPU)

Status: Planned
Type: Feature

## Summary

Expose per-Instance CPU/memory/disk/GPU utilization — **current gauges, no history** — through a
new `metrics` subresource on the aggregated `worker.gpustack.ai/v1` Instance API. CPU/memory/disk
are queried **in real time** from the node kubelet's stats Summary API via the API-server node proxy
(with `metrics.k8s.io` as a degraded fallback for CPU/memory when the kubelet read fails); GPU
metrics come from the DeviceManager, which samples the node's accelerators every 15s and keeps only
the **latest snapshot** (no ring buffer, no retention). The subresource merges both into one
unified, strictly instance-scoped sample. No Prometheus, no metrics-server dependency, no
`custom.metrics.k8s.io`, no CRD schema change.

This is the second design iteration. The first iteration (per-node 15-minute ring-buffer history,
pod stats sampled by the DeviceManager, `sinceSeconds` window queries) was implemented and then
**descoped by product decision (2026-08-07)**: history is dropped; only current gauges are served.
The rejected design is recorded under Alternatives.

## Motivation

The Instance CR exposes no CPU/RAM/DISK utilization today; the goal is a RunPod-style utilization
view (gauges). Key grounding facts (verified against code and upstream docs during research):

- The DeviceManager `Detector` samples accelerator metrics (VRAM usage/utilization, core
  utilization, temperature, power, health — `pkg/device/types.go`) every monitor period from vendor
  libraries. These are **node-level per-card** metrics; nothing correlates them to pods, and no
  host/pod CPU/RAM/DISK is collected anywhere in first-party code.
- The kubelet stats Summary API (`/stats/summary`) carries per-pod CPU (usageNanoCores), memory
  (workingSetBytes), per-container rootfs usedBytes (the writable layer — the disk gauge's
  numerator) and pod-level ephemeral-storage usedBytes. It is served on every node; no Prometheus
  needed. `metrics.k8s.io` (metrics-server) carries only CPU/memory latest values and exists only
  when a metrics-server is deployed.
- The worker runs an aggregated API server with programmatically installed APIService objects;
  `pkg/extensionapi` supports arbitrary named subresources and Instance already has computed
  read-only `log`/`events` subresources. The worker's service account is cluster-admin, so it may
  read kubelet stats through the API-server node proxy
  (`/api/v1/nodes/<node>/proxy/stats/summary`).
- Pod ↔ Instance attribution is free: the backing pod's name equals the Instance name and carries
  the label `app.kubernetes.io/part-of: <instance UID>`
  (`pkg/worker/controllers/worker/instance.go`); allocated accelerator UUIDs are recorded in the
  pod's `device.gpustack.ai/accelerator.allocated` annotation
  (`pkg/deviceplugin/controller.go`).
- The disk gauge contract is `rootfs.usedBytes` (numerator, from kubelet stats) over
  `spec.resources.localStorage` (denominator) — Kubernetes' `ephemeral-storage` limit is an eviction
  threshold and never produces a `df`-visible sized filesystem; that is upstream semantics, not a
  bug (KEP-1029; kubernetes.io ephemeral-storage docs).
- Product decisions (2026-08-07): utilization-based autoscaling (HPA/VPA) is **not** an Instance
  goal, so serving `custom.metrics.k8s.io` is definitively out; **no history retention** — only
  current gauges are exposed (charting, if ever wanted, is the console's own polling concern).

### Goals

- An Instance user can read their Instance's current CPU/memory/disk utilization — plus
  whole-allocated-card GPU metrics (VRAM usage/utilization, core utilization, temperature, power) —
  without entering the instance, via the aggregated API (kubectl `--raw`, Swagger, console):
  `kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/metrics"`.
- CPU/RAM/DISK figures are real-time (fetched at request time); GPU figures are at most one monitor
  period stale (default 15s).
- Works on any cluster out of the box: no Prometheus, no metrics-server (when absent, only the
  degraded fallback is skipped), no new components.
- Measurable success criteria:
  - The endpoint returns one current sample with CPU/memory/rootfs/ephemeral-storage of the
    Instance's pod and, when accelerators are allocated, their per-card metrics.
  - With the kubelet path artificially unavailable, CPU/memory still come back from
    `metrics.k8s.io` when a metrics-server exists (disk fields absent).
  - A request for instance A never returns another instance's (or any non-instance pod's) data;
    a caller without `get instances/metrics` RBAC is denied.

### Non-Goals

- Any history/time-series retention or window queries (dropped 2026-08-07; the first iteration's
  ring-buffer design is recorded under Alternatives).
- HPA/VPA integration; serving `metrics.k8s.io` or `custom.metrics.k8s.io`.
- Making the root disk size visible to `df` inside the instance (the gauge shows
  `rootfs.usedBytes / localStorage` instead). A real sized block device is a separate spec.
- Per-pod GPU attribution on shared / soft-sliced cards (v1 reports whole allocated cards only).
- Console/frontend work (no frontend lives in this repository; the API is the deliverable).
- v1 transport hardening beyond the agreed baseline (see Risks): mTLS between worker and
  DeviceManager, and NetworkPolicy restriction of the readout endpoint, are follow-ups.

## Proposal

1. **DeviceManager — latest snapshot only.** The monitor loop (default period 15s) stores the
   newest accelerator sample in a single atomic snapshot — a new generic `datax.Snapshot[T]`
   (`atomic.Pointer`-based) — replacing the first iteration's ring buffers. Nothing pod-level is
   collected or stored by the DeviceManager. The existing secure webserver serves the snapshot at a
   read-only GET endpoint.
2. **Worker — real-time pod stats.** The new Instance `metrics` subresource resolves Instance →
   `status.nodeName` → backing pod (verified against the Instance UID), then queries the node
   kubelet's `/stats/summary` **through the API-server node proxy** at request time and extracts
   that pod's CPU/memory/rootfs/ephemeral-storage. If the kubelet read fails, it falls back to
   `metrics.k8s.io` PodMetrics (CPU/memory only) when that API is available.
3. **Worker — GPU merge.** When the pod carries allocated accelerators, the handler resolves the
   node's Ready, non-terminating DeviceManager pod (component label + `spec.nodeName`, preferring
   the allocation's manufacturer), fetches its snapshot over pod IP, filters to the allocated
   device UUIDs, and merges into the same sample. The response is one unified current sample.

### User Stories

#### Story 1

As an Instance user, I want to read my instance's current CPU/memory/disk/GPU utilization via
`kubectl get --raw /apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/metrics`, so that I
can judge resource pressure without entering the instance.

#### Story 2

As a cluster operator, I want instance utilization to work without deploying Prometheus or
metrics-server, so that it is available out of the box on any cluster where the operator runs.

(The first iteration's Story 2 — 5–15 minutes of history for console charting — was dropped by
product decision; if charting returns as a requirement, the console polls the gauge endpoint or a
new spec designs retention.)

### Core Features & Acceptance Criteria

**F1 — DeviceManager latest snapshot**
- AC1.1: `datax.Snapshot[T]` provides race-free `Store`/`Load` of the latest value (atomic;
  covered by a `-race` test with concurrent writers/readers).
- AC1.2: Each monitor tick stores the newest accelerator sample; the default monitor period is
  15s (flag `--monitor-period` remains); the ring buffers, the pod-stats sampler, and the
  `--monitor-history` option of iteration 1 are removed.
- AC1.3: A registered webserver GET path returns the latest snapshot as JSON (single sample, not
  an array); read-only, short timeout; empty before the first tick.

**F2 — Real-time pod stats in the subresource**
- AC2.1: GET `instances/<name>/metrics` fetches the node kubelet summary via the API-server node
  proxy at request time and returns the backing pod's CPU/memory/rootfs/ephemeral-storage.
- AC2.2: The backing pod is verified against the Instance (name + `app.kubernetes.io/part-of` UID
  label); a mismatch, a missing pod, or an unscheduled Instance yields a typed `ServiceUnavailable`
  (transient backing state), never another object's data.
- AC2.3: On kubelet read failure, CPU/memory fall back to `metrics.k8s.io` when served in the
  cluster; disk fields are then absent. When neither works → typed `ServiceUnavailable`.

**F3 — GPU merge**
- AC3.1: With allocated accelerators, the sample includes those cards' metrics (filtered by
  device UUID from the allocation annotation), converted to the unit-bearing API fields.
- AC3.2: DeviceManager unreachable → the sample still returns CPU/RAM/DISK with the accelerator
  section absent (GPU is best-effort; pod stats are authoritative).

**F4 — API contract**
- AC4.1: `InstanceMetrics` carries one current sample (no `samples` array, no options type);
  regenerated OpenAPI/register/conversion artifacts are clean; the endpoint shows up in the
  embedded Swagger UI.
- AC4.2: Authorization follows Kubernetes subresource RBAC: callers need `get` on
  `instances/metrics` specifically.

### Notes / Constraints / Caveats

- Pinned product decisions: no history, no `custom.metrics.k8s.io`, no metrics-server dependency,
  no Prometheus, no metrics in CR status, no CRD schema change.
- JSON encoding in new/changed code uses the project's `pkg/utils/json` wrapper, not
  `encoding/json` directly.
- The kubelet read goes through the API-server node proxy (`nodes/proxy` is covered by the
  worker's cluster-admin binding); no kubelet IP resolution, no per-node dialing, no TLS juggling
  for this path.
- The `metrics.k8s.io` fallback uses the generic REST client (`AbsPath`), so no `k8s.io/metrics`
  dependency is added; availability is probed per request (a 404/NotFound means "no adapter").
- DeviceManager TLS is self-signed by default; the worker dials it with verification skipped inside
  the same trust domain (v1 decision, confirmed 2026-08-07 — see Risks for the mTLS follow-up).
  The project's HTTP transport honors proxy env vars (`pkg/utils/httpx/transport_options.go:15`);
  the pod-IP client disables proxying.
- A single latest snapshot needs no response-size cap; the handler still bounds the operation with
  one timeout covering resolution + fetch (+ one retry).
- The 15s default monitor period also paces device re-detection (the detect loop exits and
  re-detects on device-key change observed during monitor ticks); 15s detection lag is accepted.
- Known adjacent hazards (out of scope, do not regress): workspace `volume.ephemeral.capacity` is
  not cross-validated against `localStorage`; overcommit truncates sub-Gi storage requests to zero.

### Boundaries

- **Always:** scope every response to the requesting Instance (name + UID label); use
  `pkg/utils/json`; reuse the existing `pkg/extensionapi` subresource registration path; run
  `make generate` and `make lint` after touching API types.
- **Ask first:** any CRD schema change; adding RBAC beyond the existing cluster-admin bindings;
  changing chart templates (including adding a NetworkPolicy); adding a new Go module dependency.
- **Never:** persist metrics anywhere (no ring buffers, no CR status, no files); add a
  Prometheus/metrics-server dependency; return other tenants' pod data; route DeviceManager reads
  through the cross-node ClusterIP Service; gate the feature behind autoscaling-related APIs.

### Risks and Mitigations

- v1 worker→DeviceManager fetch skips TLS verification and the snapshot endpoint is unauthenticated
  (it serves only node-level accelerator metrics) → Mitigation: follow-up hardening —
  cert-manager-issued shared CA with proper server identity, plus a NetworkPolicy allowing only
  worker ingress (chart change, ask-first).
- API-server node proxy adds per-request apiserver load → Mitigation: one proxied call per metrics
  request; the console is expected to poll moderately; caching can be added later if profiling says
  so.
- `metrics.k8s.io` fallback may surprise when a partial adapter serves stale data → Mitigation:
  fallback only on kubelet failure, disk fields absent, and the sample does not pretend to be
  complete (documented in the API comment).
- GPU staleness up to one monitor period (15s) → accepted; the snapshot carries its timestamp.
- Wrong GPU attribution under soft slicing / shared cards → v1 reports whole allocated cards only,
  documented; per-pod GPU split stays a Non-Goal.

## Design Details

### Commands

Environment (confirmed): **local** macOS (go 1.26.4) for build/test/lint; **remote**
`frank@192.168.50.17` for image packaging/push; E2E on the k3s test cluster
(context `k3s-192-168-50-17`) via the `gpustack-operator-e2e` skill.

```bash
make deps                 # fetch dependencies
make lint                 # golangci-lint via hack/lint.sh
go test ./pkg/...         # unit tests (local)
go test -race ./pkg/utils/datax/... ./pkg/devicemanager/... ./pkg/worker/extensionapis/...
make build                # cross build (local)

# remote package + push (remote tree is at /home/frank/gpustack.ai/gpustack):
ssh frank@192.168.50.17 'cd /home/frank/gpustack.ai/gpustack && \
  PACKAGE_NAMESPACE=thxcode PACKAGE_TAG=dev-<hash> PACKAGE_PUSH=true make package'
```

Note: the protobuf step of `make generate` derives its output dir by trimming `gpustack.ai/gpustack`
off the working directory, so codegen must run from a checkout path ending in `gpustack.ai/gpustack`
(the test machine layout satisfies this). On any other path, run the generator through a symlinked
layout instead of `make generate`:

```bash
mkdir -p /tmp/gpustack-codegen/gpustack.ai && ln -sfn "$(pwd)" /tmp/gpustack-codegen/gpustack.ai/gpustack
cd /tmp/gpustack-codegen/gpustack.ai/gpustack && \
  PATH="$PWD/.sbin:$PWD/.sbin/protoc/bin:$PATH" GODEBUG=gotypesalias=0 go run -mod=mod ./gen/api
```

### Project Structure

- `pkg/utils/datax/snapshot.go` (new) — generic atomic latest-value snapshot + test.
- `pkg/device/types.go` — remove iteration-1 pod-stats types.
- `pkg/devicemanager/detector/` — detector stores the latest accelerator sample in a snapshot;
  remove `podstats.go` and the ring-buffer wiring; `option.go` monitor period default 15s, drop
  the monitor-history option.
- `pkg/devicemanager/history.go` → snapshot readout (single latest sample; rename path to
  `/monitor/snapshot`).
- `api/worker/v1/instance.metrics.go` — `InstanceMetrics` with one current sample; drop
  `InstanceMetricsOptions`; regenerate.
- `pkg/worker/extensionapis/worker/instance.metrics.go` — rewrite: real-time kubelet proxy read +
  metrics API fallback + snapshot merge; registration stays in `instance.go`.
- `.claude/skills/gpustack-operator-e2e/cases/case-37.sh` — rework to current-gauge assertions.

### Code Style

Follow the existing subresource pattern — plain computed GET via the extensionapi mixin, e.g.
`instance.events.go`:

```go
type InstanceEventsHandler struct {
	extensionapi.GetOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func newInstanceEventsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *InstanceEventsHandler {
	h := &InstanceEventsHandler{}
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()
	return h
}
```

Conventions: `pkg/utils/json` for JSON, wrapped errors with context, table-driven tests alongside
code, `.golangci.yaml` lint config, `make generate` after any `api/` change (see the layout note in
Commands).

### Implementation Plan

Rework supersedes iteration 1 (commits `0abb6f44`, `39c0110d`, `aaf6a0eb`, `e241ec56` on this
branch are its product; the R-tasks replace/rewrite them in place — `/my-ship` folds the history).

- [x] **R1 · `datax.Snapshot` + DeviceManager single-snapshot monitor**
      Blocked by: None
      Owns: `pkg/utils/datax/**`, `pkg/device/types.go`, `pkg/devicemanager/**`
      Acceptance: `datax.Snapshot[T]` (atomic Store/Load) with a `-race` concurrency test; the
      detector keeps only the latest accelerator sample (default period 15s); the pod-stats
      sampler, both ring buffers, and the `--monitor-history` option are removed; the webserver
      GET endpoint (renamed `/monitor/snapshot`) returns the single latest snapshot as JSON
      (empty object before the first tick), GET-only with a short timeout; JSON via
      `pkg/utils/json`.
      Verify: `go test -race ./pkg/utils/datax/... ./pkg/devicemanager/... && go build ./...`

- [ ] **R2 · API types rework — current gauge, no options**
      Blocked by: None
      Owns: `api/**`
      Acceptance: `InstanceMetrics` carries a single current `sample` (the existing unit-bearing
      `InstanceMetricsSample`/`InstanceAcceleratorMetrics` shapes are kept); `InstanceMetricsOptions`
      is removed; codegen rerun via the documented layout; `go build ./api/...` clean; no new
      OpenAPI violation exceptions.
      Verify: `make generate` (symlinked layout) `&& go build ./api/...`

- [ ] **R3 · Instance `metrics` subresource — real-time**
      Blocked by: R1, R2
      Owns: `pkg/worker/extensionapis/worker/**`
      Gate: review
      Acceptance: plain GET (no options) returns the unified current sample: CPU/memory/rootfs/
      ephemeral-storage read at request time from the node kubelet via the API-server node proxy;
      on kubelet failure CPU/memory fall back to `metrics.k8s.io` when available (typed
      `ServiceUnavailable` when neither works); accelerators merged from the DeviceManager
      snapshot when the pod has allocated cards (best-effort — absent on DM failure); the backing
      pod is verified by name + UID label; operation-wide timeout (resolution + fetch + one
      retry); proxying disabled for the pod-IP fetch; `pkg/utils/json` throughout;
      `make generate && make lint` clean.
      Verify: `go test ./pkg/worker/extensionapis/...`

- [ ] **R4 · e2e case-37 rework + run on the test cluster**
      Blocked by: R3
      Owns: `.claude/skills/gpustack-operator-e2e/**`
      Acceptance: case-37 asserts the current-gauge contract (fields present, instance-scoped,
      unprivileged caller denied) with no history assertions; the remote image builds **and
      pushes** (iteration-1 push failed on a Docker Hub auth token EOF — retry/resolve docker
      login on the builder) and the case passes on the k3s test cluster.
      Verify: `bash .claude/skills/gpustack-operator-e2e/cases/case-37.sh gpustack-system`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

Iteration-1 tests that pin the abandoned design are rewritten or deleted in place by R1/R3
(detector history/ring tests, podstats tests, subresource windowing tests).

#### Unit tests

- `pkg/utils/datax` (snapshot): concurrent Store/Load under `-race`; Load returns the latest stored
  value; zero-value snapshot loads nil.
- `pkg/devicemanager` (snapshot readout): JSON shape (single sample), empty-before-first-tick,
  method rejection; detector stores a new snapshot per tick (15s default, flag intact).
- `pkg/worker/extensionapis/worker` (subresource): fake kubelet summary via injected REST fake —
  happy path, pod UID mismatch, missing pod, unscheduled instance, kubelet down → `metrics.k8s.io`
  fallback (present/absent), both down → `ServiceUnavailable`; DM snapshot merge (allocated-ID
  filtering, malformed annotation, DM down → GPU absent but pod stats returned); timeout/
  retry behavior.
  Targets: all new/changed code covered; iteration-1 suites updated in place.

#### Integration tests

No aggregated-apiserver harness exists in the repo; integration coverage is the fake-kubelet +
fake-DM round trips in unit tests plus the e2e case.

#### e2e tests

R4 / case-37 on the k3s test cluster: current gauge fields present and instance-scoped;
unprivileged caller denied; metrics-server-absent behavior is inherent to the test cluster (it runs
one — the fallback path is exercised in unit tests only).

## Alternatives

- **Iteration-1 design: DeviceManager-sampled pod stats + 15min ring-buffer history +
  `sinceSeconds` windowing** — implemented (commits on this branch) then rejected by product
  decision: history is not wanted; storing anything node-side (buffers, pod indexes) is unnecessary
  moving parts, and the ring buffer's lock-free wrap semantics needed an extra mutex to read safely.
  Real-time reads make the whole storage question disappear.
- **Serve `custom.metrics.k8s.io` via an adapter** — rejected: current gauges only, no history; the
  sole hard-requirement scenario (HPA/VPA autoscaling) was ruled out as a product goal. Revisit only
  for third-party platform integration; can then be added to the existing GenericAPIServer without a
  new component.
- **metrics.k8s.io as the primary pod-stats source** — rejected: it carries CPU/memory only (no
  ephemeral-storage/rootfs) and exists only when a metrics-server is deployed; kept as a degraded
  fallback instead.
- **Worker dials the kubelet directly (node IP:10250)** — rejected in favor of the API-server node
  proxy: no address resolution, no kubelet serving-cert handling, RBAC already covered.
- **DeviceManager keeps the last N samples (small N) instead of one** — rejected: history is
  descoped; a single snapshot is the minimal correct state.

## Open Questions

- Whether node-level aggregate metrics (host CPU/RAM) belong in a follow-up.
