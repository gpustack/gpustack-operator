# Spec: Instance Metrics Totals and Node-Local Prometheus Exporter

Status: Building
Type: Feature

## Summary

Second iteration on the Instance utilization metrics shipped by
`specs/2026-08-07-instance-utilization-metrics.md`. Two changes. First, the `metrics`
subresource gains the denominators it never carried: every figure becomes a strict
`XXXTotal` / `XXXUsed` pair reported in one unit, so a consumer computes a percentage from
one response — and the disk gauge is corrected, because today it divides a subset by an
aggregate. Second, the Device Manager exports the same per-Instance gauges for every
Instance on its node on the `/metrics` endpoint it already serves, sampled by a background
poller on the monitor period so a scrape never performs I/O and never blocks. No Prometheus
component enters the chart and no new dependency enters the module; the only chart additions
are an opt-in NetworkPolicy and a fix to a scrape annotation that is wrong today.

## Motivation

The shipped subresource answers "how much is this Instance using" but not "out of how
much". A gauge without its denominator is not a utilization view, and the asymmetry is
visible in the response itself: `accelerators[].memoryMiB` already carries the card's total
while CPU, RAM and DISK carry none. Second, the only way to read these figures is one
aggregated-API request per Instance; an operator running Prometheus has no way to collect
them at all.

Grounding facts, verified against the code:

- `Instance.spec.resources` carries `CPU`, `RAM` and `LocalStorage` as required quantities
  (`api/worker/v1alpha1/instance.go:203-218`), and `getResourceRequirements` writes them as
  the Pod's **limits** unchanged — `rr.Limits[n] = q`
  (`pkg/worker/controllers/worker/instance.go:1050`). Only *requests* are overcommit-scaled
  (`:1052`). The sshd sidecar is built with `withGeneral=false` (`:563`) and declares no
  general resources, so the sum of the Pod's container limits equals `spec.resources`
  exactly. The denominator needs no new source of truth.
- `LocalStorage` maps to `core.ResourceEphemeralStorage`
  (`pkg/worker/controllers/worker/instance.go:1048`). The kubelet enforces that limit
  against the pod-level ephemeral-storage aggregate — container writable layers **plus**
  container logs **plus** local emptyDir volumes — not against the writable layers alone.
  The shipped contract divides `rootfs.usedBytes` by `spec.resources.localStorage`, i.e. a
  subset over the aggregate's ceiling. An Instance filling its `workspace` emptyDir
  (`pkg/worker/controllers/worker/instance.go:632-640`) grows the aggregate while
  `rootfsUsedMiB` stays near zero, so an Instance about to be evicted for ephemeral-storage
  can report a near-zero disk gauge. This is a correctness fix, not a rename.
- `/metrics` **already exists** on the Device Manager's secure webserver
  (`pkg/manager/manager.go:71`, `promhttp.HandlerFor(ctrlmetrics.Registry, …)`; the registry
  is redirected to component-base's `legacyregistry` at `pkg/devicemanager/manager.go:19-27`).
  It carries build info and goroutine-pool stats today and nothing domain-specific.
  Only the Device Manager serves that registration: the worker hands the shared manager a
  `webserver.Null()` (`pkg/manager/option.go:229`) and registers its own handler on the
  aggregated API server (`pkg/worker/worker.go:174`), and worker-gateway owns another
  (`pkg/workergateway/gateway.go:76`). A change to the shared handler therefore has a real
  blast radius of one binary.
- `github.com/prometheus/client_golang` is already a **direct** module dependency
  (`go.mod:40`), and `pkg/utils/gox/metrics.go` is an in-repo custom `prometheus.Collector`
  registered into that same registry (`pkg/manager/manager.go:41-51`) to pattern-match.
- The chart annotates the Device Manager Service with `prometheus.io/scrape|port|path|scheme`
  (`deploy/gpustack-operator/chart/templates/device-manager/service.yaml:10-13`) and ships no
  Prometheus. The vendored Kueue and NFD subcharts carry a ServiceMonitor and a PodMonitor
  respectively, both **disabled by default** (`kueue.enablePrometheus: false`,
  `node-feature-discovery.prometheus.enable: false`, neither overridden by the parent).
  "Expose an endpoint, let the operator's own Prometheus find it" is the established posture.
- **That annotation is wrong today.** The Service publishes `port: 443` while the container
  listens on `--secure-port` 32443 (`service.yaml:21-24`, `daemonset.yaml:125-127`). Under
  endpoint-role discovery the target address is already the correct `podIP:32443`; the
  conventional annotation relabel then *overwrites* it with the annotated 443, where nothing
  listens. The annotation does not help discovery, it breaks it. The worker Service carries
  the same mistake (`templates/worker/service.yaml:10-13`).
- The Device Manager's ServiceAccount is bound to `cluster-admin`
  (`deploy/gpustack-operator/chart/templates/device-manager/serviceaccount.yaml`), and it
  already runs a Pod informer indexed by `spec.nodeName`
  (`pkg/deviceplugin/controller.go:288`). Instance Pods are identifiable by their controller
  ownerReference to the Instance (`pkg/worker/controllers/worker/instance.go:672`) plus the
  `app.kubernetes.io/part-of` UID label (`:594`). The Device Manager already reads its own
  Node object through the loopback client (`pkg/devicemanager/detector/detector.go:262-272`).
- The chart rolls **one Device Manager DaemonSet per manufacturer**
  (`daemonset.yaml:7`), each pinned by a nodeSelector on that vendor's NFD PCI-vendor label
  (`:53-55`) and passing `--manufacturer` to the binary (`:86`), with the manufacturer also
  stamped as a pod label (`:33`). A node with two vendors runs two Device Managers; a node
  with no accelerator runs none. e2e `case-1.sh:9-11` states the latter as expected
  behaviour.
- The kubelet's own sampling is not instantaneous — cAdvisor housekeeping runs on an interval
  of roughly ten seconds and the Summary API serves cached values — so reading it at scrape
  time is not materially fresher than polling it on a comparable period.

### Goals

- Every figure in an `InstanceMetrics` sample is one half of a `Total`/`Used` pair in a
  single unit, so a consumer computes CPU, RAM and DISK percentages from one response
  without reading `Instance.spec`.
- CPU is reported in **milli-cores** on both sides of the pair; memory and storage in
  **MiB** on both sides. Precision beyond that is explicitly not wanted.
- The disk pair compares like with like: `storageUsedMiB` is the ephemeral-storage
  aggregate the kubelet evicts on, against `storageTotalMiB`, the limit it evicts against.
- An operator's existing Prometheus scrapes per-Instance gauges from the Device Manager
  Pods, with no Prometheus component installed by the operator and no new endpoint or port.
- A scrape never performs I/O, never blocks, and never returns 5xx because a data source
  failed.
- Measurable success criteria:
  - A sample for a running Instance carries `cpuTotalMilliCores`, `cpuUsedMilliCores`,
    `memoryTotalMiB`, `memoryUsedMiB`, `storageTotalMiB`, `storageUsedMiB`, and every
    total equals the corresponding `spec.resources` quantity.
  - `curl` against a Device Manager Pod's `/metrics` returns `gpustack_instance_*` gauges
    for the Instances on that node, and on a node running two Device Managers exactly one of
    them carries the pod-level families.
  - With the kubelet source failing, the scrape still returns **200** carrying the
    accelerator gauges and `gpustack_instance_metrics_collector_success{source="kubelet"} 0`.
  - With the NetworkPolicy enabled on a CNI that enforces it, a pod matching neither the
    worker selector nor the configured scrapers cannot reach the Device Manager's secure
    port, while the Device Manager stays Ready.

### Non-Goals

- Any history or time-series retention in the operator; Prometheus is the consumer's, not
  ours.
- Shipping, vendoring or enabling a Prometheus, ServiceMonitor or PodMonitor in the chart.
- Node-level accelerator metrics for cards allocated to no Instance (the exporter is
  Instance-scoped; see Open Questions).
- **Coverage of nodes carrying no accelerator.** The Device Manager DaemonSets are pinned to
  nodes carrying their vendor's PCI-vendor label, so a node with no accelerator runs no
  exporter and its CPU-only Instances produce no series. The `metrics` subresource still
  serves those Instances. Moving the exporter to the worker or a new component would close
  this and was rejected (see Alternatives).
- Authenticating `/metrics`. The endpoint stays unauthenticated, as `/monitor/snapshot`
  already is; the NetworkPolicy is the mitigation (see Risks).
- Per-pod GPU attribution on shared or sliced cards (unchanged: whole allocated cards only).
- Cross-validating `spec.volume.ephemeral.capacity` against `spec.resources.localStorage`
  (pre-existing hazard, see Risks).
- Fixing the same scrape-annotation mistake on the worker Service — out of this spec's
  blast radius, recorded in Open Questions.
- Console/frontend work; mTLS between worker and Device Manager.

## Proposal

1. **Paired, unit-consistent API fields.** `InstanceMetricsSample` becomes three
   `Total`/`Used` pairs plus the accelerator list; `InstanceAcceleratorMetrics` renames its
   memory fields into the same pairing. `rootfsUsedMiB` is removed. Field names change in
   place and the protobuf tags are renumbered contiguously, per this project's practice for
   its pre-release API — no deprecated aliases, no reserved gaps.
2. **One implementation of the figures.** A new `pkg/kubemetrics` owns the whole
   Kubernetes-side read: the computation (totals from the Pod's container limits, usage from
   the kubelet's pod stats, round-up conversions, the ephemeral aggregate as the storage
   numerator), the kubelet-summary read through the API-server node proxy, and the degraded
   `metrics.k8s.io` source behind it. The subresource and the exporter are its two consumers,
   so the two surfaces cannot drift. Because that read is node-wide, a successful readout is
   cached for a caller-stated age, so asking about several Instances of one node costs one
   node-proxy request between them rather than one each.
3. **A background poller in the Device Manager.** On the monitor period the Device Manager
   reads its node's kubelet summary through the API-server node proxy, joins it to the
   Instance Pods its informer already caches for that node, and stores the result in a
   `datax.Snapshot`. Nothing is read at scrape time.
4. **An ordinary registered collector.** Because `Collect` only reads that snapshot, the
   collector registers once at startup into the existing registry — no per-request registry,
   no scrape-deadline plumbing, no shared-handler extension point. Pod-level families are
   emitted by exactly one Device Manager per node; accelerator families by each, since device
   IDs are disjoint across manufacturers.
5. **An opt-in NetworkPolicy.** The chart gains a policy over the Device Manager's secure
   port admitting the worker and a configurable set of scrapers, because that port now
   publishes every tenant's Instance inventory to anything that can reach it.

### User Stories

#### Story 1

As a console developer, I want one `metrics` request to give me both the numerator and the
denominator of every gauge, so that I can render a utilization percentage without a second
request to read the Instance spec.

#### Story 2

As a cluster operator, I want my existing Prometheus to scrape per-Instance CPU, RAM, DISK
and accelerator gauges from each node, so that I can alert and chart on Instance
utilization — while the operator's chart installs no Prometheus of its own.

#### Story 3

As a cluster operator, I want a Device Manager whose kubelet source is failing to still serve
the accelerator gauges it does have, so that one broken source never blanks a whole node's
metrics.

#### Story 4

As an Instance user, I want the disk gauge to reflect what actually gets me evicted, so that
a nearly-full workspace shows as nearly-full instead of near-zero.

#### Story 5

As a cluster operator, I want to restrict who can reach the Device Manager's secure port, so
that a metrics endpoint listing every tenant's workloads is not readable by every pod in the
cluster.

### Core Features & Acceptance Criteria

**F1 — Paired totals and units in the API**

- AC1.1: `InstanceMetricsSample` carries `cpuTotalMilliCores`/`cpuUsedMilliCores`,
  `memoryTotalMiB`/`memoryUsedMiB`, `storageTotalMiB`/`storageUsedMiB`, plus `timestamp`
  and `accelerators`. `cpuUsageNanoCores`, `memoryWorkingSetMiB`, `rootfsUsedMiB` and
  `ephemeralStorageUsedMiB` no longer exist.
- AC1.2: `InstanceAcceleratorMetrics` carries `memoryTotalMiB`/`memoryUsedMiB` (renamed from
  `memoryMiB`/`memoryUsageMiB`); the utilization, temperature, power and health fields are
  unchanged.
- AC1.3: Totals are always populated when a sample is served — they come from the Instance's
  own declaration, not from a measurement source — and are therefore plain values, while
  every `Used` figure stays a pointer that is absent when its source is unavailable. Totals
  survive the `metrics.k8s.io` fallback, which carries no storage figures at all.
- AC1.4: CPU totals come from the Pod's CPU limits in milli-cores; CPU usage converts the
  kubelet's nanocores to milli-cores. Memory and storage figures are MiB. All conversions
  **round up**, the rule the shipped API already documents, so a measured figure below one
  unit reads as `1` and `0` means no usage.
- AC1.5: `storageUsedMiB` is the kubelet's pod-level ephemeral-storage aggregate;
  `storageTotalMiB` is the Pod's ephemeral-storage limit. A test pins that an Instance
  writing only into its `workspace` emptyDir moves `storageUsedMiB`.
- AC1.6: A test pins that the sum of the Pod's container limits equals `spec.resources`, so
  a future sidecar declaring general resources fails the suite instead of silently inflating
  every denominator.
- AC1.7: Protobuf tags are renumbered contiguously with no reserved gaps; `make generate`
  produces clean artifacts and no new OpenAPI violation exception.

**F2 — Per-Instance gauges on the Device Manager's `/metrics`**

- AC2.1: A GET of `/metrics` on a Device Manager Pod returns, in addition to the existing
  process metrics, one gauge per API field for the Instances on that node, named
  `gpustack_instance_<field>` with the API's units in the suffix
  (`_millicores`, `_mib`, `_percent`).
- AC2.2: The label set is `namespace`, `instance_name`, `instance_uid`, `node`. `instance`
  is **not** used: Prometheus attaches its own `instance` target label and, under the default
  `honor_labels: false`, renames a colliding exported label to `exported_instance` — a query
  grouping by `instance` would silently group by scrape target. `instance_uid` keeps a
  deleted-and-recreated Instance of the same name from continuing the previous incarnation's
  series.
- AC2.3: Accelerator families carry `id` and `manufacturer` in addition, and cover only the
  device IDs recorded in that Instance's allocation annotation that belong to this Device
  Manager's own manufacturer.
- AC2.4: Pod-level families are emitted only by the Device Manager whose `--manufacturer`
  sorts first among the **Ready** Device Manager pods on that node, so exactly one target
  carries them; when that pod goes away the next manufacturer takes the role over rather than
  leaving a gap. Accelerator families are emitted by every Device Manager — device IDs are
  disjoint, so they never duplicate.
- AC2.5: Instance Pods are identified by a controller ownerReference of kind `Instance` plus
  the `app.kubernetes.io/part-of` UID label; pods with a `DeletionTimestamp` are skipped and
  at most one pod is kept per Instance UID, so one gather can never carry two identical label
  sets. Kubelet summary entries are matched by pod UID, never by name alone.
- AC2.6: A snapshot older than three monitor periods yields no families and
  `..._collector_success{source=…} 0`.
- AC2.7: The response is valid Prometheus text exposition: every family has `# HELP` and
  `# TYPE gauge`, and the client library's own lint is clean.

**F3 — Polling, not scraping, and failure tolerance**

- AC3.1: A poller on the monitor period (default 15s) reads this node's kubelet summary
  through the API-server node proxy and stores the per-Instance samples in a
  `datax.Snapshot`. `Collect` performs no I/O — verified by a `-race` test that gathers
  concurrently with polling.
- AC3.2: A failing poll drops the snapshot and records the reason; the next scrape reports
  the accelerator families and the failure gauge rather than stale pod figures.
- AC3.3: `/metrics` returns **200** with the metrics it has whenever a data source fails, and
  the shared handler serves partial results instead of discarding everything on a gather
  error.
- AC3.4: `gpustack_instance_metrics_collector_success{source="kubelet"|"snapshot"}` is `1`
  on success and `0` on failure, and
  `gpustack_instance_metrics_collector_duration_seconds{source=…}` reports each source's
  latency, so a silently degraded exporter is visible in Prometheus itself.
- AC3.5: The failure reason is logged at a level that does not spam once per period
  indefinitely.

**F4 — Chart: NetworkPolicy and the scrape annotation**

- AC4.1: A values-gated NetworkPolicy selects the Device Manager pods and admits ingress on
  the secure port from the worker's pod selector and from a configurable list of scraper
  peers. `deviceManager.networkPolicy.enabled` ships **`true`** for the verification round
  described in the Implementation Plan and is flipped to **`false`** before this lands, so an
  upgrade changes no existing behaviour.
- AC4.2: An `extraIngress` escape hatch lets an operator on a CNI that does not exempt
  node-local traffic admit the node's own address range, because NetworkPolicy has no peer
  that can express "the node this pod runs on".
- AC4.3: The Device Manager Service's `prometheus.io/port` annotation names the secure port
  instead of 443, so the conventional annotation relabel no longer rewrites a correct
  endpoint address into one where nothing listens.
- AC4.4: `make lint chart` is clean.

**F5 — Documentation and e2e**

- AC5.1: `docs/reference/instance-metrics.md` documents the new field pairs, the corrected
  disk semantics (including why `rootfs` is gone), the exporter's names, labels and units,
  the deliberate deviation from Prometheus base-unit convention, the single-exporter rule,
  the accelerator-free node gap, the scrape configuration an operator actually needs
  (endpoint port, and a CA or `insecure_skip_verify` for the self-signed serving
  certificate), and the NetworkPolicy's semantics and caveats.
- AC5.2: e2e `case-37` asserts the new pairs and stays cluster-agnostic; `case-38` and
  `case-39` follow the accelerator field rename; a new `case-40` scrapes a Device Manager
  Pod's `/metrics` and asserts the gauges, the label set and the single-exporter rule,
  auto-skipping when the Instance's node runs no Device Manager.

### Notes / Constraints / Caveats

- **Units deliberately deviate from Prometheus convention.** Prometheus idiom is base units
  (bytes, cores, ratio); this exporter uses `_mib`, `_millicores` and `_percent` to match the
  API struct exactly, so the two surfaces never disagree by a rounding step. Accepted and
  documented.
- **Totals are the Instance's declaration; usage is pod-wide.** The sshd sidecar declares no
  general resources but does consume some, so a percentage can exceed 100. Documented rather
  than corrected: the denominator a user cares about is what they asked for.
- **The storage numerator is the eviction quantity, with upstream caveats.** Kubernetes counts
  writable layers, logs and disk-backed emptyDir toward the pod limit, but does not enforce it
  on unsupported filesystem layouts, counts tmpfs-backed emptyDir as memory, and can evict for
  node pressure independently of any pod limit. The docs say this rather than promising that
  100% is exactly the eviction point.
- **NetworkPolicy is L3/L4 and cannot scope a route.** `/metrics`, `/monitor/snapshot`,
  `/readyz`, `/livez` and `/debug/*` all share port 32443, so the policy guards the whole
  port. Admitting a scraper therefore also admits it to `/monitor/snapshot` — which leaks
  nothing, since node-level card metrics are strictly less than what `/metrics` already gives
  it. After this change `/metrics` is the *more* sensitive of the two endpoints.
- **NetworkPolicy has no peer for "the node".** `ingress.from` accepts only `podSelector`,
  `namespaceSelector` and `ipBlock`, and the kubelet's probes originate from the node's host
  network namespace. Upstream leaves node-to-pod traffic implementation-defined; mainstream
  CNIs exempt it so probes survive, but that is practice, not guarantee — hence AC4.2's
  escape hatch and the on-cluster verification in the plan.
- The kubelet read stays on the API-server node proxy (`nodes/proxy`, covered by the existing
  binding). Reading the kubelet directly was rejected: it would send the Device Manager's
  `cluster-admin` ServiceAccount token to a peer whose certificate cannot be verified on most
  distributions, which is strictly worse than the existing unauthenticated snapshot fetch that
  sends no credential at all. At one poll per node per period the proxy's cost is negligible.
- JSON encoding in new or changed code uses `pkg/utils/json`, not `encoding/json`.
- **No module dependency is added.** `golang.org/x/sync` moves from `// indirect` to direct in
  `go.mod` for `singleflight.DoChan` — already in the module graph and in `go.sum`, no version
  change, nothing new to download. The in-repo `github.com/golang/groupcache/singleflight`
  (`pkg/extensionroute/openapi/handler.go`) was the closer precedent but offers only `Do`,
  whose waiters cannot leave on their own context — the exact hazard single-flight introduces
  here.
- Known adjacent hazard, unchanged and now merely *visible*:
  `spec.volume.ephemeral.capacity` is not cross-validated against
  `spec.resources.localStorage`, so a workspace can be sized past the ephemeral-storage limit
  that evicts it. Out of scope; do not regress.

### Boundaries

- **Always:** scope every figure to the Instance it belongs to (ownerReference + UID label +
  pod UID, and allocated device IDs for accelerators); keep the exporter's units identical to
  the API struct's; return 200 from `/metrics` whatever fails; run `make generate` and
  `make lint` after touching `api/`, `make lint chart` after touching the chart.
- **Ask first:** adding a ServiceMonitor/PodMonitor template; adding RBAC beyond the existing
  binding; adding a Go module dependency; exporting anything that is not Instance-scoped;
  any chart change beyond the NetworkPolicy and the annotation fix this spec authorizes.
- **Never:** install, vendor or enable Prometheus in the chart; add a second `/metrics` route
  or a second listening port; persist any metric anywhere; return a 5xx from `/metrics`
  because a data source failed; perform I/O inside `Collect`; send a ServiceAccount token to
  an unverified peer; emit another manufacturer's or another Instance's data.

### Risks and Mitigations

- `/metrics` publishes every tenant's Instance names, namespaces, node placement, declared
  sizes, live usage and accelerator IDs to anything that can reach the Device Manager's port,
  while the `metrics` subresource is RBAC-gated per Instance — a genuine widening, not the
  same posture as the node-level `/monitor/snapshot` → **accepted by the user**, mitigated by
  the opt-in NetworkPolicy and documented explicitly, including a recommendation to enable it.
- Renaming served `worker.gpustack.ai/v1` fields breaks existing consumers, and reusing
  protobuf field numbers is wire-unsafe for any client built against the old descriptor →
  the API is pre-release and this project renumbers contiguously rather than carrying
  deprecated aliases; the break is confined to the `metrics` subresource shipped in
  `b2bea718` and is called out in the release notes. In-repo consumers are the subresource,
  its unit test, `case-37`, `case-38` and `case-39`.
- Enabling the NetworkPolicy on a CNI that does not exempt node-local traffic cuts the
  kubelet's probes and takes every Device Manager NotReady → default `false` at ship time,
  the `extraIngress` escape hatch, an explicit doc warning, and a real on-cluster
  verification round on a CNI that enforces policy before the default is chosen.
- The NetworkPolicy cannot be verified by the local e2e path — the kind CNI does not
  implement NetworkPolicy, so the object would be inert and a green run would prove nothing
  → verification is pinned to a cluster whose CNI enforces it, and the e2e case asserts
  nothing about enforcement.
- Removing `rootfsUsedMiB` loses the "writable layer vs workspace" split → accepted: a lone
  subset answers only one third of the question (writable layer / logs / volumes). A future
  breakdown should be complete, with parts summing to the aggregate.
- Two Device Managers can briefly both consider themselves the node's exporter while
  readiness changes, publishing the same series from two targets → transient, across
  different targets rather than within one gather (so no gather error), and self-resolving;
  documented.
- A CPU-only Instance on an accelerator-free node produces no series at all → recorded as a
  Non-Goal, with the subresource as the answer for those Instances; revisit if it becomes a
  real requirement.
- Series cardinality grows with Instances per node and churns on `instance_uid` → the
  exporter is node-local and Instance-scoped, one small set of families, and the accelerator
  families only materialize for Instances that allocated cards.
- `nodes/proxy` is an undeclared dependency of the exporter that only works because the
  ServiceAccount is `cluster-admin` → named explicitly in the docs so a future least-privilege
  ClusterRole does not silently blank every node's pod-level gauges.
- The node readout cache is process-wide state that could grow one entry per node and could
  serve a figure older than the caller expects → entries aged past the caller's stated age are
  swept on every store, so the map holds only the nodes read inside the window; the age is a
  parameter rather than a constant, so the Device Manager's `--monitor-period` is never capped
  by it; and a sample is stamped with the kubelet's own measurement time, so a cached read
  reports its real age rather than the time it was served.
- A cache alone would not deduplicate concurrent misses: a client asking about N Instances of
  one node on a cold cache would fire N node-proxy reads, halving a steady polling load rather
  than collapsing it → concurrent misses for one node collapse onto a single in-flight read.
  The hazard that buys is a stuck kubelet holding every request for that node, so the waiters
  select on their own contexts and leave when told to, while the read itself is detached from
  whichever caller started it — one caller giving up must not cancel the read the others are
  waiting on — and carries a deadline of its own.

## Design Details

### Commands

Environment (confirmed with the user): **local macOS** for build, test and lint; a **remote
amd64 builder** for image packaging and push; **e2e on a Kubernetes cluster the user
supplies**, and specifically for the NetworkPolicy round a cluster whose CNI actually enforces
NetworkPolicy — the local kind CNI does not.

```bash
make deps                                  # fetch dependencies
make lint                                  # golangci-lint over the whole module
make lint chart                            # chart lint (never `make test chart` locally —
                                           # it installs against the current kube context)
make lint docs                             # docs lint
go test ./pkg/...                          # unit tests
go test -race ./pkg/devicemanager/... ./pkg/kubemetrics/... ./pkg/worker/extensionapis/...
make build                                 # cross build
make generate                              # after any api/ change — see the layout note
```

`make generate`'s protobuf step derives its output directory by trimming `gpustack.ai/gpustack`
off the working directory, so it must run from a checkout path ending in that suffix. From any
other path, run the generator through a symlinked layout:

```bash
mkdir -p /tmp/gpustack-codegen/gpustack.ai && ln -sfn "$(pwd)" /tmp/gpustack-codegen/gpustack.ai/gpustack
cd /tmp/gpustack-codegen/gpustack.ai/gpustack && \
  PATH="$PWD/.sbin:$PWD/.sbin/protoc/bin:$PATH" GODEBUG=gotypesalias=0 go run -mod=mod ./gen/api
```

Verifying the exporter by hand from inside the cluster (the port is the container's, not the
Service's 443):

```bash
kubectl -n <ns> exec <any-pod> -- \
  curl -sk "https://<device-manager-pod-ip>:32443/metrics" | grep '^gpustack_instance_'
```

### Project Structure

- `api/worker/v1/instance.metrics.go` — paired `Total`/`Used` fields, `rootfs` removed,
  protobuf renumbered; regenerated artifacts alongside.
- `pkg/kubemetrics/` (new) — `sample.go` builds the six figures from a Pod plus its kubelet
  pod stats; `kubelet.go` reads the node-wide pod stats through the API-server node proxy
  behind an age-bounded cache and composes the one public entry point, `FetchPodSample`;
  `metricsapi.go` holds the degraded `metrics.k8s.io` source behind it.
- `pkg/worker/extensionapis/worker/instance.metrics.go` — becomes a consumer of
  `pkg/kubemetrics`; keeps the pod resolution, the `metrics.k8s.io` fallback and the
  accelerator merge.
- `pkg/worker/controllers/worker/instance_test.go` — AC1.6's pin lives beside the code that
  builds the Pod, so a sidecar gaining general resources fails the controller's own suite.
- `pkg/manager/manager.go` — `/metrics` serves partial results on a gather error.
- `pkg/devicemanager/exporter/` (new) — the poller, the snapshot, the collector, the
  single-exporter rule, the per-source success and duration gauges.
- `pkg/devicemanager/{manager,option,config}.go` — wire the poller and collector.
- `deploy/gpustack-operator/chart/templates/device-manager/networkpolicy.yaml` (new) and
  `values.yaml` — the policy and its values; `service.yaml` — the annotation fix.
- `docs/reference/instance-metrics.md` — field pairs, corrected disk semantics, exporter and
  NetworkPolicy reference.
- `.claude/skills/gpustack-operator-e2e/cases/case-{37,38,39}.sh` — renames; `case-40.sh`
  (new) — the exporter case.

### Code Style

Follow the repository's existing collector, `pkg/utils/gox/metrics.go` — `prometheus.NewDesc`
built once, `MustNewConstMetric` emitted per `Collect`, and `Collect` reading only memory:

```go
func newInstanceCollector(snapshot func() *Snapshot) prometheus.Collector {
	fqName := func(name string) string {
		return "gpustack_instance_" + name
	}
	// Not "instance": Prometheus attaches its own instance target label and renames a
	// colliding exported label to exported_instance under the default honor_labels.
	instanceLabels := []string{"namespace", "instance_name", "instance_uid", "node"}

	return &instanceCollector{
		snapshot: snapshot,
		cpuTotal: prometheus.NewDesc(
			fqName("cpu_total_millicores"),
			"The CPU limit of the Instance in milli-cores.",
			instanceLabels, nil,
		),
		cpuUsed: prometheus.NewDesc(
			fqName("cpu_used_millicores"),
			"The CPU usage of the Instance in milli-cores.",
			instanceLabels, nil,
		),
		// …
	}
}
```

Conventions: `pkg/utils/json` for JSON, errors wrapped with context, table-driven tests
alongside the code, snake_case multi-word file names, `make generate` after any `api/` change.

### Implementation Plan

- [x] **T1 · Shared instance-metrics core, paired API, subresource**
      Blocked by: None
      Owns: `api/**`, `pkg/kubemetrics/**`, `pkg/worker/extensionapis/worker/**`,
      `pkg/worker/controllers/worker/instance_test.go`
      Gate: review
      Acceptance: F1 in full. New `pkg/kubemetrics` is the single implementation of the
      six figures — totals as the sum of the Pod's container limits, usage from the kubelet's
      pod stats, every conversion rounding up, the storage numerator being the pod-level
      ephemeral-storage aggregate — and of the whole Kubernetes-side read behind them: the
      node-proxy kubelet readout and the degraded `metrics.k8s.io` source, both moved out of
      the worker package so the exporter reuses rather than reimplements them. Because that
      readout is node-wide, a successful one is cached for a caller-stated age, so a client
      walking the Instances of one node costs it one request rather than one per Instance; the
      age is a parameter rather than a constant so the Device Manager's `--monitor-period` can
      never be capped by it, and a handler built with the zero value reads afresh. Nothing is
      exported without a caller outside the package: the surface is `FetchPodSample`,
      `DefaultMaxAge`, and `NewSample` for the pin below — the node-wide `FetchPodSamples` is
      left to T3, which is the caller that defines it. The API becomes the three `Total`/`Used` pairs
      with `rootfsUsedMiB` gone and protobuf renumbered contiguously; totals are plain values,
      used figures stay pointers. The subresource becomes the first consumer, keeping its pod
      resolution and accelerator merge and mapping the read's error onto `ServiceUnavailable`.
      Its unit test is rewritten in place.
      Verify: `go test ./pkg/kubemetrics/... ./pkg/worker/extensionapis/...
      ./pkg/worker/controllers/worker/... && go build ./...`

- [ ] **T2 · `/metrics` serves partial metrics instead of 500**
      Blocked by: None
      Owns: `pkg/manager/**`
      Gate: review
      Acceptance: AC3.3's handler half — the registration at `pkg/manager/manager.go:71`
      degrades to the metrics it can gather instead of returning 500 and discarding
      everything. A comment records that only the Device Manager serves this registration,
      the worker passing `webserver.Null()` and registering its own, so the change's real
      blast radius is one binary.
      Verify: `go test ./pkg/manager/... && go build ./...`

- [ ] **T3 · Device Manager Instance sample poller**
      Blocked by: T1
      Owns: `pkg/devicemanager/exporter/**`, `pkg/devicemanager/manager.go`,
      `pkg/devicemanager/option.go`, `pkg/devicemanager/config.go`, `pkg/kubemetrics/**`
      Gate: review
      Acceptance: AC3.1, AC3.2, AC2.5. A loop on the monitor period reads this node's kubelet
      summary through the API-server node proxy and stores a `datax.Snapshot` of per-Instance
      samples built by `pkg/kubemetrics`. That read is `pkg/kubemetrics`'s node-wide entry
      point, added here rather than in T1 so its shape answers a real caller: it takes the
      Instance Pods (a sample's totals come from their container limits, which the kubelet
      readout does not carry), returns the samples keyed by Pod UID (a sample carries no
      identity of its own), errors only when the whole readout failed, and skips a pod the
      kubelet does not carry rather than degrading it to `metrics.k8s.io` — one absent series
      beats one extra request per Instance per poll. Instance pods come from the existing
      `IndexingPodsByNodeName` cache index, filtered by controller ownerReference of kind
      Instance plus the `app.kubernetes.io/part-of` UID label, with terminating pods skipped
      and at most one pod per Instance UID. Kubelet entries are matched by pod UID. A failed
      poll drops the snapshot and records the reason.
      Verify: `go test -race ./pkg/devicemanager/... && go build ./...`

- [ ] **T4 · The collector: pod-level gauges and the single-exporter rule**
      Blocked by: T3
      Owns: `pkg/devicemanager/exporter/**`
      Gate: review
      Acceptance: AC2.1, AC2.2, AC2.4, AC2.7, AC3.4, AC3.5. A `prometheus.Collector`
      registered once at startup into the existing registry, whose `Collect` reads only the
      snapshot. Labels exactly `namespace`, `instance_name`, `instance_uid`, `node`. Pod-level
      families only when this Device Manager's `--manufacturer` sorts first among the Ready
      Device Manager pods on the node, read from the informer by component label and the
      node-name index. Per-source success and duration gauges.
      Verify: `go test -race ./pkg/devicemanager/...`

- [ ] **T5 · Accelerator gauges from the monitor snapshot**
      Blocked by: T4
      Owns: `pkg/devicemanager/exporter/**`
      Gate: review
      Acceptance: AC2.3, AC2.6. Accelerator families labelled additionally `id` and
      `manufacturer`, covering only the allocated device IDs of this Device Manager's own
      manufacturer, emitted by every Device Manager since device IDs are disjoint. A snapshot
      older than three monitor periods yields none and sets its success gauge to 0.
      Verify: `go test -race ./pkg/devicemanager/...`

- [ ] **T6 · Chart: NetworkPolicy, annotation fix, and an on-cluster verification round**
      Blocked by: None
      Owns: `deploy/gpustack-operator/chart/templates/device-manager/networkpolicy.yaml`,
      `deploy/gpustack-operator/chart/templates/device-manager/service.yaml`,
      `deploy/gpustack-operator/chart/values.yaml`
      Gate: review
      Acceptance: F4 in full. The policy selects the Device Manager pods and admits the
      worker's pod selector plus a configurable scraper list on the secure port, with an
      `extraIngress` escape hatch. The Service's `prometheus.io/port` names the secure port.
      **Verification round, on a cluster whose CNI enforces NetworkPolicy** (the local kind
      CNI does not): ship the default as `true`, install, and prove three things — a pod
      matching no peer is denied, the worker and a configured scraper are admitted, and the
      Device Manager stays Ready, i.e. the kubelet's probes are not cut off by a policy that
      cannot name the node. Record the observed CNI behaviour in the docs, **then flip the
      default to `false`** so an upgrade changes no existing behaviour.
      Verify: `make lint chart`, then the manual round above against the user-supplied context

- [ ] **T7 · Documentation**
      Blocked by: T1, T5, T6
      Owns: `docs/**`
      Acceptance: AC5.1, including the scrape configuration an operator actually needs, the
      `nodes/proxy` dependency, the >100% sshd caveat, the ephemeral-aggregate caveats, and
      the NetworkPolicy behaviour observed in T6.
      Verify: `make lint docs` and the `gpustack-operator-docs` skill's index/link/TOC checks

- [ ] **T8 · e2e: renames and a new exporter case**
      Blocked by: T1, T5, T6
      Owns: `.claude/skills/gpustack-operator-e2e/**`
      Acceptance: AC5.2. `case-37` asserts the new pairs and needs no Device Manager, since
      the subresource path does not use one. `case-38` and `case-39` update their
      `.sample.accelerators[].memoryMiB` reads. A new `case-40` scrapes a Device Manager pod's
      `/metrics`, asserts the per-Instance gauges and label set, and asserts that exactly one
      target on the node carries the pod-level families — auto-skipping when the Instance's
      node runs no Device Manager, which is the honest gate rather than a hardware one.
      Verify: `bash .claude/skills/gpustack-operator-e2e/cases/case-40.sh <ns>`

Start order: **T1, T2 and T6 are independent and run concurrently**; then T3 → T4 → T5; then
T7 and T8 concurrently.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests
to make this code solid enough prior to committing the changes necessary to implement this
enhancement.

#### Prerequisite testing updates

`pkg/worker/extensionapis/worker/instance.metrics_test.go` asserts every renamed and removed
field and is rewritten in place by T1. e2e `case-37` and the `.sample.accelerators[].memoryMiB`
reads in `case-38` and `case-39` are updated by T8. A repository-wide search for the old JSON
field names finds no other consumer; the console lives outside this repository and is covered by
the release note rather than by a test.

#### Unit tests

- `pkg/kubemetrics` — one test file per source file, each beside the code it covers
  (`sample_test.go`, `kubelet_test.go`, `metricsapi_test.go`): `2026-08-12` - `99.1%`. The
  three uncovered statements are the second cache check inside the single-flight closure,
  which only fires in the window between one caller's outer check missing and another's store
  landing; reaching it from a test would mean a seam in production code, which costs more than
  it pins. Covered: totals as the container-limit sum; usage from pod stats; round-up at
  sub-unit values; the storage numerator moving when only an emptyDir grows; absent stat
  fields staying absent; node-proxy fetch decode and error paths; the cache serving a readout
  inside the window without hitting the node, re-reading past it, bypassing on a zero age,
  never caching a failure, and sweeping aged-out entries instead of holding one per node for
  the life of the process; concurrent misses collapsing onto one read, and a waiting caller
  still leaving on its own deadline rather than the read's; the degraded source on its own — a
  served answer, an unserved API reported as absence rather than failure, a broken adapter, an
  undecodable body, a negative quantity clamped, an untimed measurement; and every branch of
  the composed read — kubelet unreachable, kubelet carrying no entry for the pod, an unserved
  `metrics.k8s.io`, both sources failing, and an entry measured before the pod existed.
- `pkg/worker/controllers/worker`: the pin that the built Pod's container limits sum to
  `spec.resources`, with and without the sshd sidecar, so a sidecar gaining general resources
  fails loudly instead of inflating every denominator.
- `pkg/worker/extensionapis/worker`: rewritten happy path with the new pairs; pod UID
  mismatch; missing pod; unscheduled Instance; a degraded sample round-tripping through the
  served object with storage absent but totals still present; an unreadable usage mapped onto
  `ServiceUnavailable` with the reason named; accelerator merge and allocated-ID filtering;
  malformed allocation annotation. The degraded source's own branches are not re-tested here —
  they belong to `pkg/kubemetrics` now.
- `pkg/manager`: a gather error yields 200 with the surviving metrics rather than 500.
- `pkg/devicemanager/exporter`: the poller stores a snapshot per tick and drops it on failure;
  terminating pods skipped; two pods sharing an Instance UID collapse to one; non-Instance pods
  ignored; kubelet entries matched by UID; the single-exporter rule picks the lexicographically
  first Ready manufacturer and hands over when that pod disappears; `Collect` performs no I/O
  under `-race` with concurrent polling; the label set is exactly
  namespace/instance_name/instance_uid/node (+ id/manufacturer); accelerator filtering by
  allocated ID and own manufacturer; a stale snapshot yields none; success and duration gauges
  track each source; exposition lints clean.
  Targets: all new and changed code covered; the iteration-1 suite rewritten in place.

#### Integration tests

No aggregated-apiserver or in-cluster harness exists in the repository. Integration coverage is
the fake-apiserver round trips the subresource tests already use — `TestMain` stands up an
`httptest` server behind `system.LoopbackKubeRestConfig` — extended with a fake kubelet summary
for the exporter's poller, plus the e2e cases below.

#### e2e tests

- `case-37` — the subresource contract with the new pairs: fields present, instance-scoped, an
  unprivileged caller denied. Any cluster; no Device Manager required.
- `case-38` / `case-39` — unchanged contracts, renamed accelerator field; still auto-skipping
  without their respective hardware.
- `case-40` (new) — scrape a Device Manager pod's `/metrics`; assert the test Instance's
  gauges, the exact label set, and that exactly one target on the node carries the pod-level
  families. Auto-skips when the Instance's node runs no Device Manager.
- NetworkPolicy enforcement is deliberately **not** asserted by any e2e case: the local kind
  CNI does not implement NetworkPolicy, so a green run would prove nothing. It is verified once,
  by hand, in T6 against a cluster whose CNI enforces it, and the observed behaviour is written
  into the docs.

## Alternatives

- **Leave the totals out; consumers read `Instance.spec.resources`.** Rejected: two round
  trips for every gauge, and it leaves the response internally inconsistent, since the
  accelerator entries already carry their total.
- **Add totals without renaming the used fields.** Rejected: it cannot produce the
  `Total`/`Used` pairing, and CPU would report its two sides in different units.
- **Keep `rootfsUsedMiB` beside the new pair.** Rejected: a subset sitting next to the
  aggregate with no naming cue is the misreading this spec exists to fix.
- **Prometheus base units (`_bytes`, `_cores`, ratios).** Idiomatic, rejected anyway: two
  surfaces disagreeing by a unit conversion is a worse daily cost than one non-idiomatic
  suffix.
- **Read the kubelet at scrape time, node-locally.** Rejected on two counts. It would send the
  Device Manager's `cluster-admin` ServiceAccount token to a peer whose serving certificate
  cannot be verified on most distributions — strictly worse than the existing credential-free
  snapshot fetch — and it buys freshness the kubelet does not actually offer, since its own
  sampling interval is around ten seconds and the Summary API is cached. It would also have
  required a per-request registry, scrape-deadline plumbing from
  `X-Prometheus-Scrape-Timeout-Seconds`, and an extension point in shared code, all of which
  the poller deletes.
- **`promhttp.HandlerOpts.Timeout` instead of a per-request registry.** Moot once polling
  replaced scrape-time I/O, and it was never right: it bounds the gather and returns 503
  without cancelling the underlying read.
- **Duplicate the pod-level series from every manufacturer's Device Manager and dedupe with
  `max by(...)` at query time.** Rejected: the Device Managers sample independently, so `max`
  is systematically biased upward rather than picking between identical copies, it discards the
  `node` dimension, and a rolling DaemonSet update makes it jump. The single-exporter rule
  costs no shared state — each Device Manager already knows its own manufacturer and can see
  its node's Device Manager pods in the informer.
- **Elect the exporter through a lease.** Rejected: the lexicographic rule over Ready pods is
  already a total order with automatic handover and no new state.
- **Export from the worker instead.** It would cover accelerator-free nodes and need no
  single-exporter rule, but the worker runs several replicas behind leader election: either only
  the leader polls and the series migrate between targets as leadership moves, or every replica
  polls and multiplies the kubelet and apiserver load by the replica count. It would also have
  to fetch every node's Device Manager snapshot over pod IP on every poll — exactly the
  cross-node fan-out the node-local shape avoids.
- **A new `worker-exporter` component.** It solves the replica problem cleanly, at the cost of
  a permanent new Deployment, Service, ServiceAccount, RBAC, chart templates, install-mode
  wiring and documentation for one feature, and it concentrates the whole cluster's fan-out in
  one pod. Rejected as disproportionate.
- **An extra manufacturer-less Device Manager DaemonSet without the PCI nodeSelector**, to
  cover accelerator-free nodes. Keeps the node-local shape and closes the coverage gap, but adds
  a second DaemonSet that coexists with the vendor ones on accelerator nodes and has to be
  folded into the single-exporter rule. Deferred: revisit if the gap becomes a real requirement.
- **Authenticating `/metrics` with delegated authn/authz**, as the worker's API server does.
  Correct security-wise, rejected for this iteration: the Device Manager has no authentication
  chain today, and it would cost the zero-configuration scraping that is the feature's point.
  The NetworkPolicy is the chosen mitigation.
- **A disabled-by-default ServiceMonitor/PodMonitor template**, matching the Kueue and NFD
  subcharts. Not rejected on principle, but out of scope: the annotation path, once its port is
  fixed, covers discovery without requiring the Prometheus Operator CRDs.

## Open Questions

- Should the exporter also emit node-level accelerator gauges for cards allocated to no
  Instance? It would make idle capacity visible, at the cost of leaving the Instance-scoped
  contract.
- The worker Service carries the same wrong `prometheus.io/port: "443"` annotation
  (`templates/worker/service.yaml:10-13`). Out of this spec's scope — fix it separately?
- Do accelerator-free nodes need Instance metrics coverage badly enough to justify the extra
  manufacturer-less DaemonSet recorded under Alternatives?
- Should a disabled-by-default PodMonitor template be added later for Prometheus-Operator
  users, matching the Kueue/NFD precedent?
- Does any out-of-repo consumer (the console) read the renamed JSON fields today? The release
  note has to name the break either way.
