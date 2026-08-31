# Instance Metrics Reference

> **Purpose** — the two surfaces reporting an Instance's utilization: the
> `instances/<name>/metrics` subresource and the Device Manager's Prometheus exporter — their
> fields, names, sources and limits.
> **Audience** users, operators, console developers · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~9 min

Both surfaces report the same figures from the same code, so they can never disagree by a
rounding step. They differ in who asks: the subresource answers one Instance per request, the
exporter publishes every Instance of one node for a scrape.

## Contents

- [The subresource](#the-subresource)
- [The sample](#the-sample)
- [One accelerator entry, whatever the mode](#one-accelerator-entry-whatever-the-mode)
- [Where each figure comes from](#where-each-figure-comes-from)
- [Which manufacturers measure a share](#which-manufacturers-measure-a-share)
- [Why a share's figure is absent](#why-a-shares-figure-is-absent)
- [Scoping and authorization](#scoping-and-authorization)
- [Degradation rules](#degradation-rules)
- [The node exporter](#the-node-exporter)
- [Metric families](#metric-families)
- [Querying it](#querying-it)
- [Scraping it](#scraping-it)
- [Restricting who may scrape](#restricting-who-may-scrape)
- [Limits](#limits)

## The subresource

The worker's aggregated API serves a read-only `metrics` subresource per Instance:

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/metrics"
```

One **current sample**, not a history — nothing is stored. CPU, memory and storage are read live
per request; accelerator figures come from the Device Manager's latest sample.

## The sample

`InstanceMetrics` carries the Instance's identity and one `sample`. Every figure is a
**`Total`/`Used` pair**, so a percentage can be computed from the response alone:

| Field | Unit | Meaning |
|---|---|---|
| `sample.timestamp` | RFC 3339 | when the CPU/memory/storage figures were measured |
| `sample.cpuTotalMilliCores` | milli-cores | the Instance's CPU limit |
| `sample.cpuUsedMilliCores` | milli-cores | pod CPU usage, averaged over the kubelet's sample window |
| `sample.memoryTotalMiB` | MiB | the Instance's memory limit |
| `sample.memoryUsedMiB` | MiB | pod working-set memory |
| `sample.storageTotalMiB` | MiB | the Instance's ephemeral-storage limit |
| `sample.storageUsedMiB` | MiB | pod ephemeral-storage usage: writable layers, logs and disk-backed `emptyDir` |
| `sample.accelerators[]` | — | one entry per allocated accelerator |

A **total is the Instance's declaration** — the sum of its containers' limits, init containers
excluded — and is always present. A **used figure is a measurement**, and is **absent rather
than zero** when no source could measure it: zero means measured-and-idle.

Memory and storage are in **MiB** throughout: the kubelet measures bytes, the manufacturers'
libraries MiB, so the coarser unit is reported rather than mixed in one response. Byte figures
are **rounded up** — a sub-1 MiB working set reads `1`, never `0`.

An `accelerators[]` entry carries `id`, `mode`, `memoryTotalMiB`, `memoryUsedMiB`,
`memoryUtilizationPercent`, `coresUtilizationPercent`, `temperatureCelsius`, `powerUsageWatts`
and `unhealthy`. Every figure is **the Instance's own**, whatever the allocation did to the card —
see [One accelerator entry, whatever the mode](#one-accelerator-entry-whatever-the-mode).

Two caveats come with the pairs:

- **A percentage can exceed 100.** The `sshd` sidecar of an SSH-enabled Instance declares no CPU
  or memory of its own but does consume some, while the totals count only what was declared. The
  denominator is deliberately what the user asked for.
- **`storageTotalMiB` is not exactly the eviction point.** Kubernetes counts writable layers,
  logs and disk-backed `emptyDir` toward the pod's ephemeral-storage limit, but does not enforce
  it on every filesystem layout, counts tmpfs-backed `emptyDir` as memory instead, and can evict
  for node disk pressure independently of any pod limit.

> **Why** there is no `rootfs` figure — an earlier revision reported the containers' writable
> layer alone against the Instance's storage limit, which put two different quantities on the two
> sides of one percentage. The kubelet evicts on the pod-level aggregate, so that aggregate is
> the only numerator the limit can be read against, and it is the one `storageUsedMiB` carries.

## One accelerator entry, whatever the mode

**Every mode reports the same fields with the same meaning.** An Instance holding a whole device
reads the device's own figures, because the device is what it was granted; one holding a **logical
slice** (`Sliced`) or a **hardware partition** (`Partitioned`) reads that share's quota and that
share's usage. So "how much memory is this Instance using, and how close is it to its ceiling" is the
same two fields whatever the allocation did, and a consumer never has to know.

| Field | Unit | Meaning |
|---|---|---|
| `id` | — | what the Instance holds: the device's identifier, or under `Partitioned` the **partition's own** — a MIG UUID rather than the parent card's |
| `mode` | — | how the grant was made: `Exclusive`, `Shared`, `Sliced`, `Partitioned` or `Visibility`. The **name**, spelled the same here and on the `mode` label of every exporter family |
| `memoryTotalMiB` | MiB | the memory granted: the device's own, a slice's quota, or a partition's own capacity |
| `memoryUsedMiB` | MiB | the memory measured held **of that grant** |
| `memoryUtilizationPercent` | 0–100 | `memoryUsedMiB` over `memoryTotalMiB` |
| `coresUtilizationPercent` | 0–100 | how much of the Instance's **own compute allowance** it was measured using |

> **One entry per grant, not per card.** A card serves one grant in every mode but `Partitioned`,
> where an Instance may hold several of its partitions at once — one per container. Each is its own
> entry, keyed by its own `id`, because collapsing them would report one tenant of a card as the card.
> Two *logical* slices of one card cannot be two entries: a slice has no identity of its own, so both
> would carry the parent card's `id`. An Instance in that shape reports the card once, with no figures
> of its own.
| `temperatureCelsius` | °C | the **whole device's** — a share has none of its own |
| `powerUsageWatts` | W | the whole device's |
| `unhealthy` | bool | the whole device's — an unhealthy card carries every share of it down |

A **total comes from the allocation** and is present whenever the allocation can state it. A **used
figure is a measurement**, and is **absent rather than zero** when nothing on this node could measure
it. A carved share whose usage could not be measured therefore reports **no usage at all** rather
than the device's, whose figure counts every other tenant on the card.

Four properties are worth knowing before reading a number off an entry:

- **A used figure may exceed its total, and is not clamped.** It measures the hardware while the
  total is the quota, so an overshoot is an anomaly to investigate — a leaking quota, a floor in the
  quota's unit conversion, or driver accounting overhead — and clamping it would present every
  leaking quota as a perfectly enforced one.
- **`coresUtilizationPercent` is against the Instance's own allowance, so it may exceed 100 too.** A
  slice capped at a fifth of a card and saturating that fifth reads `100`, not `20`; one whose shim
  let it burst past its cap reads above `100`, which is the cap not being enforced and exactly what
  clamping would hide.
- **That same denominator makes it coarse under a small cap.** The manufacturers measure the card in
  whole percent, so a 5% cap can only ever yield multiples of 20 here.
- **A partition names and sizes itself.** Its identity and its capacity are read on the partition's
  **own** device handle, so a `1g.10gb` of an H100 reports 9856 MiB — what the driver says — rather
  than the 10240 the profile name rounds to, or an eighth of the card folded out of the allocation's
  units. An idle partition reports `0` — measured — rather than an absence.
- **A partition's compute is its own or it is absent.** It is reported where the vendor answers for
  the partition's handle and absent where none does — the matrix below says which is which — and it
  is never restated against a cap, because a partition makes no compute request to be capped by.

> **Why** the card's own figures are not reported beside the share's — they answer a different
> question ("is this card hot"), and putting both in one entry is what made the earlier revision hard
> to consume: a reader had to know the mode to know which of two places held its own number. The
> card-wide view belongs to a node- or device-scoped surface, not to the per-Instance one.

## Where each figure comes from

- **CPU / memory / storage** — the kubelet's stats summary, read through the API-server node
  proxy (`/api/v1/nodes/<node>/proxy/stats/summary`); no Prometheus or metrics-server needed. The
  caller therefore needs `nodes/proxy`, which the worker's and Device Manager's bindings already
  carry.
- **One read per node, not per Instance** — a node's summary is cached for the caller's freshness
  bound (15 s for the subresource) and concurrent readers of one node share a single in-flight
  read, so a console polling many Instances of a node still costs the kubelet one request. The
  exporter below reads afresh instead: it already samples on a period, and a cache on top of a
  fixed cadence only serves the previous round back.
- **CPU / memory fallback** — if that read fails, or the kubelet does not know the pod yet,
  `metrics.k8s.io` answers where a metrics-server is deployed. It has no storage figures, so
  `storageUsedMiB` is absent; an entry predating the pod is rejected.
- **Accelerator figures** — the node's Device Manager samples the manufacturer's libraries every
  monitor period (default 15 s) and serves only the latest snapshot at `/monitor/snapshot`; one
  older than three periods is dropped as a failing monitor. A Device Manager holds only its own
  manufacturer's accelerators, so an allocation spanning two is read from both, never
  substituted. Only accelerators in the pod's allocation annotation are returned.

## Which manufacturers measure a share

A carved share's usage exists only where the manufacturer's own library answers a **per-process**
query. So the totals are available on every backend that can carve a share, while the measurements
vary by what the vendor exposes — and by whether we have been able to run it against real hardware.

| Manufacturer | `memoryUsedMiB` | `coresUtilizationPercent` | On hardware |
|---|---|---|---|
| NVIDIA | ✅ | ✅ | ✅ logical · ✅ MIG partition |
| AMD | ✅ | ⚠️ driver-dependent | ✅ |
| T-Head | ✅ | ✅ logical · — MIG partition | ✅ logical · — MIG partition |
| Ascend | ✅ | — | ✅ |
| Hygon | ✅ | ⚠️ driver-dependent logical · ✅ MIG partition | ✅ logical · ✅ MIG partition |
| Cambricon | ✅ | ✅ | — |
| Iluvatar | ✅ | — | — |
| Metax | ✅ | — | — |

- **A `—` in one of the first two columns is a capability, not a fault.** The vendor's library
  offers no entry point for that figure on that backend, so the field is permanently absent there and
  the [capability gauge](#metric-families) says `unsupported` rather than leaving it unexplained.
  Ascend is the sharpest case: its per-vNPU compute is not reachable from either published header, so
  its partitioned NPUs report memory alone.
- **⚠️ means the entry point exists and your hardware decides.** AMD and Hygon read compute from
  `cu_occupancy`, which a GFX revision may not measure at all: an RDNA3 GPU returned the invalidation
  sentinel for every process while reporting memory normally. The figure is then absent per process,
  with its reason, and the memory beside it is unaffected — never a zero, because a zero would read as
  idle.
- **The last column is about evidence, not code.** Every backend is covered by unit tests over
  recorded vendor payloads; the ones marked `—` have never been run against a driver, because the
  project has no such card. Read their figures as untested rather than as wrong.
- **On Hygon, memory was cross-checked against other tools and compute could not be.** On a BW
  card (gfx936, DTK 25.04) holding a quarter-card slice, this page's `memoryUsedMiB` read 4236 —
  the same figure the vendor's `hy-smi --showpids` and the kernel's `vram_<gpuid>` reported for
  that same process. Three sources, one number.
- **The vendor's own tool publishes no per-process compute figure**, so compute gets one comparison
  where memory got two: `hy-smi --showpids` prints VRAM and SDMA only. The kernel's `cu_occupancy`
  does publish one, and it read a steady 10 while the 44 reported here implies the library measured
  11 — the cap-relative restatement above turns 11 into `ceil(11 × 100 / 25) = 44` for a 25 % slice,
  where 10 would give 40. The one comparison available therefore disagrees by one, with no third
  source to settle it.
- **What the run does settle is that the figure follows the work.** It read 0 while the process
  merely held its allocation, 44 once that same process ran a kernel, and board power moved from
  93 W to 223 W with it. Read it as a measurement of the right quantity rather than as the
  kernel's own number.
- **Hygon is the one vendor that measures a partition's compute**, and it does so on the partition's
  own handle rather than by attributing the card's processes: an instance running a kernel read 85%
  while its idle siblings on the same card read 0. That is why its `coresUtilizationPercent` is a
  plain `✅` under partitioning while the logical column beside it stays hardware-dependent — the two
  come from different entry points, and the partition's does not go through `cu_occupancy`.
- **A partition is addressed by the identifier its allocation recorded**, and by nothing else, on
  every partitioning manufacturer. The alternative is translating the recorded profile name back into
  a driver profile id, which walks the vendor's whole profile catalog — 17 ids on NVIDIA, 85 on
  T-Head — on every card of every monitor period. A partition allocated by a Device Manager older
  than that field therefore reports an absence with a reason until its Pod is allocated again; the
  parent card's figures are every tenant's, so reporting them as this Instance's would be worse.

## Why a share's figure is absent

An absent figure always has a reason, and the reason is discoverable — on the exporter's
[capability gauge](#metric-families) and in the Device Manager's log under
`instance-accelerator-metrics`. The entry itself carries no reason field, which keeps the response
minimal.

The device's own query decides most of it:

| Reason | Meaning |
|---|---|
| `unsupported` | the library serves no such query on this backend — permanent |
| `permission` | the driver refused the query to the Device Manager's user |
| `transient_driver_error` | the query failed this pass; the next one tries again |
| `truncated` | the driver reported more rows than the read accepted; a partial list is never published as a complete one |
| `invalid_data` | the vendor's own rows contradict the card — a figure above its physical capacity |
| `bounded` | the node's records exceeded the snapshot's size bound, so this device's were dropped whole |
| `version_skew` | the Device Manager predates this consumer's schema, so its section cannot be read as what it is |

The rest is attribution. The vendor libraries report **host** process ids, which the Device Manager
maps to a Pod and container through `/proc/<pid>/cgroup`. A row it cannot place — a process that
exited mid-read, a host process, a mediating daemon, a Pod the node's index does not carry — makes
every figure of **that whole device** absent for that sample, with `no_pod_component`, `mediated`,
`unknown_pod`, `exited` or one of their siblings as the reason.

> **Why** one unplaceable row costs the whole device rather than only itself — dropping the row would
> publish a partial sum as a complete one, and if the dropped row was the Instance's only process, it
> would publish a plausible measured zero. A number that is quietly wrong is worse than no number.

## Scoping and authorization

- Only the named Instance's data is returned: the backing pod matches by name, namespace and the
  `app.kubernetes.io/part-of` UID label, kubelet entries by pod UID — a deleted-and-recreated
  Instance never reads its previous incarnation's figures.
- Callers need `get` on the `instances/metrics` subresource in group `worker.gpustack.ai`; `get
  instances` alone does not grant it. The worker's calls are covered by its existing binding.

## Degradation rules

- An Instance **none of whose containers has ever started** → `200` with its declared totals and
  every measurement zero. Unscheduled, no pod rendered yet, stopped, or holding only a previous
  incarnation's pod are all this same state: nothing has run, so nothing has been used, and that is
  an answer rather than a failure. It replaces the three `503`s this surface used to return, so a
  console needs no branch for an Instance that is merely starting up or stopped.
- The gate is **"has started", not "is ready"**. A pod can be running with a failing readiness
  probe, an unready sidecar, or a termination already begun while its main container still holds
  accelerator memory — reporting zero for that would fabricate an idle measurement. Every trace a
  container leaves opens the gate: a restart, a previous termination, an init or ephemeral container.
- Kubelet unreachable and no metrics-server → `503 ServiceUnavailable`, the message naming which
  source failed and how, so "not served here" is never confused with "returned an error". This is the
  one condition still served as unavailable: something did start, so its usage is a measurement, and
  no source could take it.
- Device Manager unreachable, no Ready Device Manager pod for the allocated manufacturer, or a
  stale snapshot → the sample still returns CPU/memory/storage, `accelerators` simply absent; the
  worker logs the reason under `instance-metrics`.

## The node exporter

Every Device Manager publishes the Instances of **its own node** as Prometheus gauges on the
`/metrics` route of its secure port (`32443` by default, HTTPS):

```bash
kubectl exec -n gpustack-system <device-manager-pod> -- \
  curl -sk https://127.0.0.1:32443/metrics | grep gpustack_instance_
```

The figures are sampled by a **background loop on the monitor period**, never at scrape time: a
scrape must not perform I/O it can block or fail on, and the kubelet's summary is cached on
roughly the same cadence anyway, so scrape-time reads would buy no freshness. A scrape reads
memory only.

Three rules decide which series exist:

- **One exporter per node.** A node carrying two vendors runs two Device Managers, and both see
  all of its Instances. The manufacturer sorting first among the node's **Ready** Device Manager
  pods publishes the pod-level families; the others publish none. The role hands over by itself
  when that pod stops being Ready, with no lease and no shared state.
- **Every Device Manager publishes its own accelerators.** Device IDs are disjoint across
  manufacturers, so the accelerator families never collide and are not subject to the rule above.
- **A node with no accelerators has no series here.** It runs no Device Manager, so its Instances
  appear on this surface at all only once one is rolled out; the subresource still answers for
  them. This is the exporter's one gap, and it is deliberate: the exporter lives where the
  accelerators are.

The collector reports on itself, so a degraded exporter is visible in Prometheus rather than only
in a log:

| Family | Labels | Meaning |
|---|---|---|
| `gpustack_instance_metrics_collector_success` | `source="kubelet"`, `source="snapshot"` | whether that source's last sampling round succeeded |
| `gpustack_instance_metrics_collector_duration_seconds` | `source="kubelet"` | how long the last poll took, whether it succeeded or not |

**One failed source never blanks the other.** A round whose kubelet read failed still publishes the
accelerator families and the declared totals — the allocations come from the Pod and the readings
from the monitor loop, neither of which the kubelet touches. Only the measured pod-level figures
go absent, beside `success{source="kubelet"} 0`.

What such a round never does is carry the previous one's measurements forward: reporting the
figures of several periods ago as current is worse than reporting none.

A round that failed outright publishes its verdict and **no figures at all**, because without it
there is no list of this node's Instances, and every family here is labelled by one.

## Metric families

Pod-level families carry `namespace`, `instance_name`, `instance_uid` and `node`. Accelerator
families carry those plus `id`, `index`, `manufacturer` and `mode` — `index` being the ordinal the
manufacturer's own tools name the device by, so a figure here lines up with what `nvidia-smi` or
`rocm-smi` shows on the host. All are gauges.

| Family | Unit | Sample field |
|---|---|---|
| `gpustack_instance_cpu_total_millicores` | milli-cores | `cpuTotalMilliCores` |
| `gpustack_instance_cpu_used_millicores` | milli-cores | `cpuUsedMilliCores` |
| `gpustack_instance_memory_total_mib` | MiB | `memoryTotalMiB` |
| `gpustack_instance_memory_used_mib` | MiB | `memoryUsedMiB` |
| `gpustack_instance_storage_total_mib` | MiB | `storageTotalMiB` |
| `gpustack_instance_storage_used_mib` | MiB | `storageUsedMiB` |
| `gpustack_instance_accelerator_memory_total_mib` | MiB | `accelerators[].memoryTotalMiB` |
| `gpustack_instance_accelerator_memory_used_mib` | MiB | `accelerators[].memoryUsedMiB` |
| `gpustack_instance_accelerator_memory_utilization_percent` | 0–100 | `accelerators[].memoryUtilizationPercent` |
| `gpustack_instance_accelerator_cores_utilization_percent` | 0–100 | `accelerators[].coresUtilizationPercent` |
| `gpustack_instance_accelerator_temperature_celsius` | °C | `accelerators[].temperatureCelsius` |
| `gpustack_instance_accelerator_power_usage_watts` | W | `accelerators[].powerUsageWatts` |
| `gpustack_instance_accelerator_unhealthy` | 0 or 1 | `accelerators[].unhealthy` |

An unmeasured `used` figure is **not published**, exactly as it is absent from the sample. A `total`
is published whenever the allocation can state it.

One more family explains the absences, and is the only one here that carries **no Instance labels**:

| Family | Labels | Meaning |
|---|---|---|
| `gpustack_accelerator_process_capability` | `node`, `manufacturer`, `id`, `entry_point`, `reason` | whether the per-process query answered on this node's driver, `1` or `0` |

`entry_point` is `memory` or `cores`, because a driver commonly serves process memory while refusing
process utilization; `reason` is empty when the query answered and otherwise one of the
[reasons above](#why-a-shares-figure-is-absent). It is a property of the node's driver and one of its
cards rather than of any tenant, so two Instances sharing a card have one answer between them —
giving it Instance labels would publish that one answer twice.

> **Why** an absent measurement publishes no sample at all rather than a zero — a Prometheus gauge
> cannot say "unknown", and a zero would read as idle. `rate()` and `avg_over_time()` over a series
> that disappears are honest about the gap; over a fabricated zero they are not.

The Instance is labeled `instance_name`, **not `instance`**: Prometheus attaches its own
`instance` target label, and under the default `honor_labels: false` a colliding exposed label is
renamed to `exported_instance` — so a query grouping by `instance` would silently group by scrape
target.

> **Why** the units are `_mib`, `_millicores` and `_percent` rather than Prometheus's base-unit
> idiom (bytes, cores, ratios) — they mirror the API fields above one for one, so the two
> surfaces reporting the same figure can never disagree by a rounding step. The unit is in every
> name, so nothing is ambiguous, only unidiomatic.

## Querying it

"What is this Instance using" is **one metric name**, in every mode:

```promql
gpustack_instance_accelerator_memory_used_mib
gpustack_instance_accelerator_cores_utilization_percent
```

and "how close is it to its ceiling" is the pair beside them:

```promql
gpustack_instance_accelerator_memory_utilization_percent
```

Nothing branches on how the Instance holds the card. `mode` is there to **group or filter** by, not
to choose a metric with:

```promql
# The busiest logical slices on the fleet.
topk(10, gpustack_instance_accelerator_cores_utilization_percent{mode="Sliced"})

# Slices whose compute cap is not being enforced.
gpustack_instance_accelerator_cores_utilization_percent{mode="Sliced"} > 100
```

`Shared` is the one mode with no quota to read a ceiling against: the device is granted whole to
several holders with nothing dividing it, so its entry reports the device's own figures and
`memoryUtilizationPercent` is a share of the device rather than of anything that Instance was
promised.

> **Why** `mode` is a label rather than a family of its own — every series in these families is one
> quantity, "what this Instance was granted and is using", so a label partitions it correctly and
> `sum by (mode)` is meaningful. Splitting into `…_slice_*` families was the earlier revision's
> mistake: it made the metric name depend on the allocation, so every dashboard needed a fallback
> and every `sum()` risked counting a carved Instance twice.

## Scraping it

The Device Manager Service carries the conventional annotations — `prometheus.io/scrape`,
`prometheus.io/port` (the **secure port**, not the Service's 443), `prometheus.io/path` and
`prometheus.io/scheme` — so an annotation-driven configuration needs nothing per cluster:

```yaml
scrape_configs:
  - job_name: gpustack-instances
    kubernetes_sd_configs:
      - role: endpoints
        namespaces:
          names: [gpustack-system]
    # The serving certificate is self-signed unless the Device Manager is given --cert-dir,
    # so either skip verification or supply that CA here.
    tls_config:
      insecure_skip_verify: true
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
      - source_labels: [__meta_kubernetes_service_annotation_prometheus_io_scheme]
        target_label: __scheme__
        regex: (https?)
      - source_labels: [__meta_kubernetes_service_annotation_prometheus_io_path]
        target_label: __metrics_path__
        regex: (.+)
      - source_labels: [__address__, __meta_kubernetes_service_annotation_prometheus_io_port]
        target_label: __address__
        regex: ([^:]+)(?::\d+)?;(\d+)
        replacement: $1:$2
```

Two things are worth knowing before adapting it:

- **Leave `honor_labels` at its default.** The exposed label set is chosen to avoid the target
  labels; turning it on would let a series overwrite `instance` or `job`.
- **Scrape endpoints, not the Service.** `role: endpoints` yields one target per Device Manager
  pod, which is what the per-node series need; a Service-level scrape would land on one pod at
  random per request, since the Service load-balances across every node's Device Managers.

## Restricting who may scrape

`/metrics` is **unauthenticated**, and it lists every Instance running on the node — names,
namespaces, declared sizes, live usage and accelerator IDs — to anything that can reach port
32443. The chart ships an opt-in NetworkPolicy for it:

```yaml
deviceManager:
  networkPolicy:
    enabled: true
    scrapers:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
```

It selects every manufacturer's Device Manager DaemonSet, is `Ingress`-only, and admits the
worker plus each `scrapers` peer on the secure port. **The default is `false`**, so an upgrade
changes no existing behaviour; enabling it is recommended on a multi-tenant cluster.

Three properties decide whether it does what you expect:

- **It guards a port, not a route.** NetworkPolicy is L3/L4, and `/metrics`, `/monitor/snapshot`,
  `/readyz`, `/livez` and `/debug/*` all share 32443, so admitting a scraper admits it to the
  snapshot too — and since per-slice reporting the snapshot carries **more** than `/metrics` does:
  one record per (Pod UID, container, device) for every carved share on the node, plus the
  diagnostics behind each figure.

  It carries no process ids — those never leave the producer — but it does name which Pod holds
  which share of which card. Enabling the policy is recommended on a multi-tenant cluster for that
  reason, and the recommendation is stronger than it was before this surface existed.
- **It has no peer for "the node".** `ingress.from` accepts only `podSelector`,
  `namespaceSelector` and `ipBlock`, while the kubelet's probes come from the node's host network
  namespace, and upstream leaves node-to-pod traffic implementation-defined.

  Verified end to end on a cluster whose CNI enforces NetworkPolicy: a pod matching no peer was
  refused on the secure port, the worker's and a configured scraper's peers were admitted, and
  the Device Manager stayed Ready across its readiness and liveness probes without a restart.
  That CNI exempts the kubelet.
- **On a CNI that does not exempt it, admit the nodes yourself.** All three Device Manager probes
  are HTTP GETs on 32443, so a policy that cuts them takes every Device Manager NotReady. Put the
  node address range in `extraIngress`, whose rules are appended verbatim:

  ```bash
  kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'
  ```

A CNI that does not implement NetworkPolicy at all makes the object inert rather than an error —
it is accepted and never enforced, so the port stays open.

## Limits

- **No history.** Both surfaces report the current sample; charting means polling.
- **A shared allocation has no ceiling to report.** `Shared` hands a whole card to several Instances
  with no per-Instance quota, so its entry reports the card's own figures and its utilization is a
  share of the card rather than of anything that Instance was promised.
- **An accelerator two containers of one Pod were separately granted reports no figures of its own.**
  One `accelerators[]` entry cannot hold two grants, and picking one or summing them would report a
  quota nobody was granted; the card's temperature, power and health still publish. A `Visibility`
  sidecar is not a second grant — it sees what its sibling holds — and does not trigger this.
- **An Instance on a node with no accelerators has no accelerator series on the exporter** — the
  exporter lives where the accelerators are. The subresource still answers for it, and for a
  stopped Instance: the exporter publishes nothing at all for one, rather than a row of zeros.
- **Pod-IP networking.** The worker dials the Device Manager pod directly with TLS verification
  skipped, accepted for the self-signed serving certificate. Neither surface is reachable from an
  out-of-cluster worker.

---

**See also** — [Architecture](../architecture.md) · [Device
Discovery](../architecture/device-discovery.md) for how the Device Manager samples accelerators ·
[Internals](../architecture/internals.md) for the Device Manager's subcommands and flags.
**Next** → [Accelerator Requests](../accelerator-requests.md)
