# Instance Metrics Reference

> **Purpose** — the `instances/<name>/metrics` subresource: one current CPU/memory/disk/GPU
> utilization sample per Instance, where each figure comes from, and the limits of the contract.
> **Audience** users, console developers · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~6 min

The worker's aggregated API serves a read-only `metrics` subresource for every Instance:

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/metrics"
```

It returns one **current sample** — not a history. Nothing is stored for this feature: CPU, memory
and disk are read in real time at request time, and accelerator figures come from the Device
Manager's latest sample.

## Contents

- [The response](#the-response)
- [Where each figure comes from](#where-each-figure-comes-from)
- [Scoping and authorization](#scoping-and-authorization)
- [Degradation rules](#degradation-rules)
- [Limits](#limits)

## The response

`InstanceMetrics` carries the Instance's identity and one `sample`:

| Field | Unit | Meaning |
|---|---|---|
| `sample.timestamp` | RFC 3339 | when the CPU/memory/storage figures were measured |
| `sample.cpuUsageNanoCores` | nanocores | pod CPU usage, averaged over the kubelet's sample window |
| `sample.memoryWorkingSetMiB` | MiB | pod working-set memory |
| `sample.rootfsUsedMiB` | MiB | containers' writable-layer usage — the disk gauge's numerator against `spec.resources.localStorage` |
| `sample.ephemeralStorageUsedMiB` | MiB | writable layers + logs + emptyDir volumes |
| `sample.accelerators[]` | — | one entry per allocated accelerator (see below) |

Every memory and storage figure is in **MiB**. The sources do not agree on a unit — the kubelet
measures in bytes, the manufacturers' device libraries in MiB — so the sample reports the coarser
one throughout rather than mixing units within one response. Byte figures are **rounded up**: an
idle instance's working set and writable layer are routinely under 1 MiB, and they read as `1`, not
`0`. A `0` therefore means the source measured no usage, never that the figure was too small to
show.

Each `accelerators[]` entry carries `id`, `memoryMiB`, `memoryUsageMiB`,
`memoryUtilizationPercent`, `coresUtilizationPercent`, `temperatureCelsius`, `powerUsageWatts`,
`unhealthy`. Pointer fields are **absent when the source is unavailable**; accelerator zero values
may also mean the manufacturer's library could not read that metric.

## Where each figure comes from

- **CPU / memory / disk** — the node kubelet's stats summary, read live through the API-server
  node proxy (`/api/v1/nodes/<node>/proxy/stats/summary`). No Prometheus or metrics-server is
  needed on this path.
- **CPU / memory fallback** — when the kubelet read fails, or when it answers without knowing the
  pod yet, `metrics.k8s.io` answers instead, if a metrics-server is deployed. That API carries no
  storage figures, so `rootfsUsedMiB` and `ephemeralStorageUsedMiB` are then absent. An entry
  measured before the pod existed (a previous incarnation of the same name) is rejected.
- **Accelerator metrics** — the Device Manager pod of the Instance's node samples the manufacturer's
  libraries every monitor period (default 15 s) and keeps only the latest snapshot, served at
  `/monitor/snapshot`. There is one Device Manager per manufacturer, each holding only its own
  manufacturer's accelerators, so an allocation spanning two manufacturers is read from both —
  another manufacturer's Device Manager is never substituted. Only the accelerators recorded in the
  pod's allocation annotation are returned — an Instance never sees an accelerator it did not
  allocate. A snapshot older than three monitor periods is treated as a failing monitor and dropped.

## Scoping and authorization

- A request only ever returns data for the Instance it names: the backing pod is matched by name,
  namespace and the `app.kubernetes.io/part-of` UID label, and kubelet entries are matched by pod
  UID, so a deleted-and-recreated Instance never reads its previous incarnation's figures.
- Callers need `get` on the `instances/metrics` subresource in group `worker.gpustack.ai` —
  `get instances` alone does not grant it. The worker's own calls are covered by its existing
  binding.

## Degradation rules

- Unscheduled Instance, no backing pod yet, or a backing pod still belonging to a previous
  incarnation of the name → `503 ServiceUnavailable`, not partial data. All three are transient
  backing state, so the caller should retry.
- Kubelet down and no metrics-server → `503 ServiceUnavailable`. The message names which of the
  two sources failed and how, so "the metrics API is not served here" is never confused with
  "the metrics API returned an error".
- Device Manager unreachable, no Ready Device Manager pod for the allocated manufacturer, or a
  stale snapshot → the sample still returns CPU/memory/disk; `accelerators` is simply absent.
  The reason is logged by the worker under `instance-metrics`.

## Limits

- **No history.** Charting requires polling this endpoint from the console; the operator retains
  nothing.
- **Whole accelerators only.** A shared or sliced allocation shows the whole accelerator's metrics;
  per-pod GPU attribution does not exist.
- **Pod-IP networking.** The worker dials the Device Manager pod directly with TLS verification
  skipped (self-signed certs) — accepted for v1; mTLS and a NetworkPolicy are on the hardening
  backlog. The endpoint is also unreachable for an out-of-cluster worker.

**See also** — [Architecture](../architecture.md) · [Device
Discovery](../architecture/device-discovery.md) for how the Device Manager samples accelerators.
**Next** → [Settings & Environment Variables](../settings.md)
