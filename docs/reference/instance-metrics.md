# Instance Metrics Reference

> **Purpose** — the `instances/<name>/metrics` subresource: one current CPU/memory/disk/GPU
> utilization sample per Instance, where each figure comes from, and the limits of the contract.
> **Audience** users, console developers · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~3 min

The worker's aggregated API serves a read-only `metrics` subresource per Instance:

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/metrics"
```

One **current sample**, not a history — nothing is stored. CPU, memory and disk are read live per
request; accelerator figures come from the Device Manager's latest sample.

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

Every memory and storage figure is in **MiB**: the kubelet measures bytes, the manufacturers'
libraries MiB, so the coarser unit is reported throughout rather than mixed in one response. Byte
figures are **rounded up** — an idle sub-1 MiB working set or writable layer reads `1`, not `0`,
so a `0` means no usage measured, never a figure too small to show.

An `accelerators[]` entry carries `id`, `memoryMiB`, `memoryUsageMiB`,
`memoryUtilizationPercent`, `coresUtilizationPercent`, `temperatureCelsius`, `powerUsageWatts`,
`unhealthy`. Pointer fields are **absent when the source is unavailable**; a zero may also mean
the manufacturer's library could not read it.

## Where each figure comes from

- **CPU / memory / disk** — the kubelet's stats summary, read live through the API-server node
  proxy (`/api/v1/nodes/<node>/proxy/stats/summary`); no Prometheus or metrics-server needed.
- **CPU / memory fallback** — if that read fails or the kubelet does not know the pod yet,
  `metrics.k8s.io` answers, when a metrics-server is deployed. It has no storage figures, so
  `rootfsUsedMiB` and `ephemeralStorageUsedMiB` are absent; an entry predating the pod (a previous
  incarnation of the name) is rejected.
- **Accelerator metrics** — the node's Device Manager samples the manufacturer's libraries every
  monitor period (default 15 s) and serves only the latest snapshot at `/monitor/snapshot`; one
  older than three periods is dropped as a failing monitor. A Device Manager holds only its own
  manufacturer's accelerators, so an allocation spanning two is read from both, never substituted.
  Only accelerators in the pod's allocation annotation are returned.

## Scoping and authorization

- Only the named Instance's data is returned: the backing pod matches by name, namespace and the
  `app.kubernetes.io/part-of` UID label, kubelet entries by pod UID — a deleted-and-recreated
  Instance never reads its previous incarnation's figures.
- Callers need `get` on the `instances/metrics` subresource in group `worker.gpustack.ai`; `get
  instances` alone does not grant it. The worker's calls are covered by its existing binding.

## Degradation rules

- Unscheduled Instance, no backing pod, or a pod from a previous incarnation of the name → `503
  ServiceUnavailable`, not partial data; all three are transient, so retry.
- Kubelet down and no metrics-server → `503 ServiceUnavailable`, the message naming which source
  failed and how, so "not served here" is never confused with "returned an error".
- Device Manager unreachable, no Ready Device Manager pod for the allocated manufacturer, or a
  stale snapshot → the sample still returns CPU/memory/disk, `accelerators` simply absent; the
  worker logs the reason under `instance-metrics`.

## Limits

- **No history.** Charting means polling this endpoint; nothing is retained.
- **Whole accelerators only.** A shared or sliced allocation shows the whole accelerator's metrics;
  per-pod GPU attribution does not exist.
- **Pod-IP networking.** The worker dials the Device Manager pod directly, TLS verification
  skipped (self-signed certs), accepted for v1; mTLS and a NetworkPolicy are on the hardening
  backlog. The endpoint is also unreachable for an out-of-cluster worker.

---

**See also** — [Architecture](../architecture.md) · [Device
Discovery](../architecture/device-discovery.md) for how the Device Manager samples accelerators.
**Next** → [Settings & Environment Variables](../settings.md)
