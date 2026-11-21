# GPU Instance Operations

Manage `Instance` stop/start transitions and NodePort persistence, configure namespace network
isolation, query live utilization, logs, and events, and diagnose stuck workspaces.

## Contents

- [Workspace stop and restart](#workspace-stop-and-restart)
- [Tenant network isolation](#tenant-network-isolation)
- [Metrics subresource](#metrics-subresource)
- [Log and event subresources](#log-and-event-subresources)
- [Troubleshooting](#troubleshooting)

## Workspace stop and restart

An `Instance` declaring *`spec.ports`* owns a NodePort `Service` of the same name and publishes
allocated node ports in *`status.ports`*. The `Service` selects the backing `Pod` by its
`app.kubernetes.io/part-of` UID label, surviving both stop/start cycles and *`spec.type`* changes.

### Step 1: Stop workspace

Patch `spec.stop` to `true` to delete the backing `Pod` and release accelerator and queue quota
while keeping the `Service` and its NodePort allocations:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: Instance
 metadata:
   name: workspace-demo
   namespace: team-a
 spec:
+  stop: true
```

Wait until `status.phase` transitions from `Stopping` to `Stopped`. If the referenced `InstanceType`
is deleted or its `ClusterQueue` enters `HoldAndDrain`, the controller sets `spec.stop: true`
automatically; setting `spec.inactive: true` on an `InstanceType` (`Hold`) blocks new admission
without stopping running `Instance`s.

### Step 2: Edit stopped workspace

While `status.phase` is `Stopped`, you can edit *`spec.type`*, *`spec.image`*, *`spec.resources`*,
*`spec.ports`*, *`spec.nodeName`*, or *`spec.additionalVolumes`* (*`spec.volume`* stays immutable):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: Instance
 metadata:
   name: workspace-demo
   namespace: team-a
 spec:
   stop: true
   ports:
     - name: http
       port: 8080  # keeps its existing NodePort on restart
+    - name: metrics
+      port: 9090  # allocates a new NodePort on restart
```

Unchanged `(protocol, port)` entries keep their allocated NodePort on restart, removed entries drop
from the `Service`, and clearing *`spec.ports`* deletes the `Service` on the next start.

### Step 3: Start workspace

Set `spec.stop: false` once `status.phase` is `Stopped` (starting from `Stopping` is refused at
admission):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: Instance
 metadata:
   name: workspace-demo
   namespace: team-a
 spec:
-  stop: true
+  stop: false
```

```bash
kubectl -n team-a wait --for=jsonpath='{.status.phase}'=Ready instance/workspace-demo --timeout=5m
kubectl -n team-a get instance workspace-demo
```

**Verification:** `PHASE` returns to `Ready` and unchanged ports report the same NodePort numbers.
Deleting the `Instance` deletes both the `Pod` and the `Service`.

## Tenant network isolation

Cross-namespace `Instance` isolation is opt-in. While the `instance-isolation-pod-cidrs` `Setting`
(`worker.instanceIsolationPodCIDRs` in Helm values) is blank (default), no policy is rendered.

Find your cluster's IPv4 pod CIDRs from your CNI IPAM (`kubectl get ippools.crd.projectcalico.org`
on Calico) or node specs (`kubectl get nodes -o jsonpath='{.items[*].spec.podCIDRs}'`), then set
the `Setting`:

```diff
 apiVersion: gpustack.ai/v1
 kind: Setting
 metadata:
   name: instance-isolation-pod-cidrs
   namespace: gpustack-system
 spec:
-  value: ""
+  value: 10.244.0.0/16
```

The worker immediately reconciles a `NetworkPolicy` named `gpustack-instance-isolation` in every
non-reserved namespace, selecting `Pod`s labeled `resource.gpustack.ai/type: instances`:

- **Allowed traffic**: Ingress and egress within the same namespace, ingress and egress to
  `0.0.0.0/0` except the configured pod CIDRs, and egress on UDP/TCP port `53` for DNS.
- **Namespace ownership**: Deleting the last `Instance` in a namespace leaves the policy in place,
  and manual edits to operator-rendered policies are overwritten. Clearing the `Setting` deletes
  only operator-rendered policies and leaves hand-authored same-named policies untouched.
- **Boundaries**: Enforcement requires a `NetworkPolicy`-capable CNI (such as Calico or Cilium).
  Other policies selecting the same `Pod`s can add permissions, and Kubernetes allows traffic from
  the hosting node (including NodePort requests via a node IP).

## Metrics subresource

Query live CPU, memory, ephemeral storage, and per-grant accelerator utilization through the
worker's read-only `metrics` subresource:

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/metrics"
```

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceMetrics
metadata:
  name: workspace-demo
  namespace: team-a
  uid: <instance-uid>
sample:
  timestamp: "2026-08-13T12:00:00Z"
  cpuTotalMilliCores: 8000  # declared container limits (init containers excluded)
  cpuUsedMilliCores: 500
  memoryTotalMiB: 65536
  memoryUsedMiB: 2048
  storageTotalMiB: 15360
  storageUsedMiB: 256
  accelerators:  # one entry per grant (device, logical slice, or partition)
    - id: <device-or-mig-uuid>
      mode: Exclusive  # enum: Exclusive, Shared, Sliced, Partitioned, Visibility
      memoryTotalMiB: 24576
      memoryUsedMiB: 4096
      memoryUtilizationPercent: 17
      coresUtilizationPercent: 45
      temperatureCelsius: 58
      powerUsageWatts: 165
      unhealthy: false
```

- **Required RBAC**: Callers need `get` on `instances/metrics` in `worker.gpustack.ai`; `get` on
  `instances` alone is insufficient.
- **Pre-start zero**: Before any container has started (unscheduled, `Stopped`, or `Pod` absent),
  the endpoint returns `200` with declared totals and `0` usage. Once started, `0` means idle.
- **Unavailable sources (`503`)**: After a container has started, if both kubelet `nodes/proxy`
  stats and `metrics.k8s.io` fail, the endpoint returns `503 ServiceUnavailable` naming the failed
  source.
- **Omitted accelerator samples**: If Device Manager is unreachable, not `Ready`, stale (over three
  monitor periods), or unable to measure a process or vendor counter, CPU/memory/storage still
  return while `accelerators` or unmeasured `used` fields are omitted rather than `0`.

Each Device Manager also exposes `gpustack_instance_*` Prometheus gauges on HTTPS port `32443`
(`/metrics`), omitting unmeasured `used` series. On multi-tenant clusters, restrict port `32443`
with `deviceManager.networkPolicy.enabled: true` and `deviceManager.networkPolicy.scrapers`.

## Log and event subresources

Stream `main` container logs or list Kubernetes `Event`s for the current backing `Pod` through the
`log` and `events` subresources:

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/log?tailLines=100&timestamps=true"
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/events"
```

- **Required RBAC**: Grant `get` on `instances/log` or `instances/events` in `worker.gpustack.ai`
  (`instancepersistentvolumes/<name>/events` serves the same view for persistent volumes).
- **Log options**: `instances/<name>/log` accepts `follow`, `tailLines` (`>= 0`), `limitBytes`
  (`>= 1`), `timestamps`, and at most one of `sinceSeconds` (`>= 1`) or `sinceTime` (RFC3339).

## Troubleshooting

Inspect *`status.phase`*, *`status.phaseMessage`*, and `Pod` events first:

```bash
kubectl -n <ns> get instance <name> -o jsonpath='{.status.phase}: {.status.phaseMessage}{"\n"}'
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/instances/<name>/events"
```

| Symptom | Reason | Resolution |
|---|---|---|
| Refused on create (`reserved namespace` or `inactive`) | System namespace or held `InstanceType` | Create in a workload namespace and select an `Active` `InstanceType`. |
| Retryable error (`not ready yet; retry`) | Slice or partition requested before `InstanceType` `status.detail` synced | Wait a few seconds for `InstanceType` reconciliation and re-apply. |
| Refused on update (`is immutable`) | Edited template, ports, node pin, or volumes while running | Set `spec.stop: true`, wait for `Stopped`, apply the edit, then set `spec.stop: false` (recreate to change *`spec.volume`*). |
| Refused on `privileged` or `hostPath` | Cluster host-access gate is `false` | Ask an administrator to enable `instance-privileged-allowed` or `instance-host-path-volume-allowed`, or remove the field. |
| Stuck in `Starting` naming a volume or runtime floor | Unresolved `ModelArtifact`, missing `CSIDriver`, unbound claim, or pinned node below kubelet `1.35` / containerd `2.1` | Check `ModelArtifact` `Resolved=True`, `model.csi.gpustack.ai`, `InstancePersistentVolume` binding, or pin a compatible node. |
| Stuck in `Starting` or `Pod` `Failed` (`UnexpectedAdmissionError`) | Queued behind capacity, incompatible `spec.nodeName`, or persistent device-plugin rejection past the 5-minute retry window | Inspect the Kueue `Workload`, verify `spec.nodeName` labels, and follow [scheduling troubleshooting](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md#troubleshooting). |

---

**See also** — [GPU Instances](/gpustack-operator/main/docs/modules/instances/index.md) (pool selection and instance resource defaults) · [GPU Instance Configuration](/gpustack-operator/main/docs/modules/instances/configuration/index.md) (SSH access, volumes, and custom `InstanceType`s) · [Heterogeneous Devices Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md#troubleshooting) (queue and device scheduling diagnosis)

**Next** → [Heterogeneous Devices Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md) — trace a scheduled `Instance` end to end.
