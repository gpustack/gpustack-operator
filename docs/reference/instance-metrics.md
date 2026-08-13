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
- [Where each figure comes from](#where-each-figure-comes-from)
- [Scoping and authorization](#scoping-and-authorization)
- [Degradation rules](#degradation-rules)
- [The node exporter](#the-node-exporter)
- [Metric families](#metric-families)
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

An `accelerators[]` entry carries `id`, `memoryTotalMiB`, `memoryUsedMiB`,
`memoryUtilizationPercent`, `coresUtilizationPercent`, `temperatureCelsius`, `powerUsageWatts`
and `unhealthy`. Each is **absent when the manufacturer's library did not produce it**.

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

## Scoping and authorization

- Only the named Instance's data is returned: the backing pod matches by name, namespace and the
  `app.kubernetes.io/part-of` UID label, kubelet entries by pod UID — a deleted-and-recreated
  Instance never reads its previous incarnation's figures.
- Callers need `get` on the `instances/metrics` subresource in group `worker.gpustack.ai`; `get
  instances` alone does not grant it. The worker's calls are covered by its existing binding.

## Degradation rules

- Unscheduled Instance, no backing pod, or a pod from a previous incarnation of the name → `503
  ServiceUnavailable`, not partial data; all three are transient, so retry.
- Kubelet unreachable and no metrics-server → `503 ServiceUnavailable`, the message naming which
  source failed and how, so "not served here" is never confused with "returned an error".
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
families carry those plus `id`, `index` and `manufacturer` — `index` being the ordinal the
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

An unmeasured `used` figure is **not published**, exactly as it is absent from the sample. A
`total` is always published.

The Instance is labeled `instance_name`, **not `instance`**: Prometheus attaches its own
`instance` target label, and under the default `honor_labels: false` a colliding exposed label is
renamed to `exported_instance` — so a query grouping by `instance` would silently group by scrape
target.

> **Why** the units are `_mib`, `_millicores` and `_percent` rather than Prometheus's base-unit
> idiom (bytes, cores, ratios) — they mirror the API fields above one for one, so the two
> surfaces reporting the same figure can never disagree by a rounding step. The unit is in every
> name, so nothing is ambiguous, only unidiomatic.

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
  `/readyz`, `/livez` and `/debug/*` all share 32443. Admitting a scraper therefore admits it to
  `/monitor/snapshot` too — which leaks nothing further, since node-level card metrics are
  strictly less than what `/metrics` already gave it.
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
- **Whole accelerators only.** A shared or sliced allocation shows the whole accelerator's
  figures; per-pod GPU attribution does not exist.
- **Pod-IP networking.** The worker dials the Device Manager pod directly with TLS verification
  skipped, accepted for the self-signed serving certificate. Neither surface is reachable from an
  out-of-cluster worker.

---

**See also** — [Architecture](../architecture.md) · [Device
Discovery](../architecture/device-discovery.md) for how the Device Manager samples accelerators ·
[Internals](../architecture/internals.md) for the Device Manager's subcommands and flags.
**Next** → [Accelerator Requests](../accelerator-requests.md)
