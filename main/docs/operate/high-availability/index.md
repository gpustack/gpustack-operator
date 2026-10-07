# High Availability Operations

Every control-plane component the chart deploys elects a leader, so extra replicas stand by. They buy
failover, not throughput. A highly available install raises each replica count and turns on its
disruption budget; everything else is one pod per node (device managers, NFD worker, both CSI node
DaemonSets).

Configure it in `values.yaml`: no HA values file ships. Every knob below sits at its chart's default,
and the caveats are inline.

## Contents

- [Node count prerequisites](#node-count-prerequisites)
- [Per-component settings](#per-component-settings)
- [The non-redundant topology](#the-non-redundant-topology)
- [Verify](#verify)

## Node count prerequisites

A `DoNotSchedule` spread needs **at least as many schedulable nodes as your largest replica count**; below
that, surplus replicas stay `Pending` forever. This page assumes three replicas on three nodes; with
fewer, lower the counts or use `ScheduleAnyway`, a preference. The components tolerate control-plane
taints, so three control-plane nodes spread fine.

They tolerate **those taints and no others**, a cordon's included, so a pod evicted by a drain never
lands back on the node being drained. Where your nodes carry a taint of your own, add it to each
component's `tolerations`.

Earlier releases let the Kueue controller manager, the NFD master and the NFD gc tolerate every taint;
an install that relied on that must set their three lists before upgrading. Upgrade with
`--reset-then-reuse-values`, not `--reuse-values`, or the new lists never reach the release; see
[Upgrading the Chart](https://github.com/gpustack/gpustack-operator/blob/dc3efd726874454d6954ffbb5b1a6892389d0bfb/deploy/gpustack-operator/chart/README.md#upgrading-the-chart).

## Per-component settings

| Component | Replicas | PodDisruptionBudget | Node spread |
|---|---|---|---|
| Worker (control plane) | `worker.replicas` | `worker.podDisruptionBudget.enabled` + `.minAvailable` | `worker.topologySpreadConstraints`, or `worker.affinity` |
| Kueue controller manager | `kueue.controllerManager.replicas` | `kueue.controllerManager.podDisruptionBudget.enabled` + `.minAvailable` | `kueue.controllerManager.topologySpreadConstraints` |
| NFD master | `node-feature-discovery.master.replicaCount` | `node-feature-discovery.master.podDisruptionBudget.enable` + `.minAvailable` | `node-feature-discovery.master.affinity` only |
| NFS CSI controller | `csi-driver-nfs.controller.replicas` | — none — | `csi-driver-nfs.controller.topologySpreadConstraints` (vendored patch) |
| S3 CSI controller | `csi-driver-s3.controller.replicas` | — none — | — none — |

Those "none" and "only" cells are upstream chart limitations: each changes what three replicas get
you, and each is spelled out below.

### Example values

```yaml
worker:
  replicas: 3
  podDisruptionBudget:
    enabled: true
    minAvailable: 2
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: DoNotSchedule

kueue:
  controllerManager:
    replicas: 3
    podDisruptionBudget:
      enabled: true
      minAvailable: 2
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app.kubernetes.io/name: kueue
            control-plane: controller-manager

node-feature-discovery:
  master:
    replicaCount: 3
    podDisruptionBudget:
      enable: true
      minAvailable: 2

csi-driver-nfs:
  controller:
    replicas: 2
    strategyType: RollingUpdate
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app: csi-nfs-controller

csi-driver-s3:
  controller:
    replicas: 2
    strategyType: RollingUpdate
```

```bash
helm upgrade gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --reset-then-reuse-values --values ha.yaml
```

### Worker (control plane)

The worker runs the aggregated extension API server and the scheduling-chain controllers in one
process. Only one replica reconciles, because leader election is always on. Every replica serves the
extension API and the admission webhooks, so with one, losing its node takes `kubectl get instancetypes`
and Pod admission down until it reschedules.

Node spread belongs in `worker.topologySpreadConstraints`. Its default `preferred` pod anti-affinity is
deliberate: replicas stay off one node without any becoming unschedulable, so three still come up on two
nodes. An entry omitting `labelSelector` gets the worker's own; `worker.affinity` **replaces** the
default rather than adding to it.

### Kueue controller manager

Kueue's `managerConfig` elects a leader, so standbys do not reconcile. Each replica still serves Kueue's
admission webhook, whose `failurePolicy: Fail` means one replica losing its node **blocks Pod creation in
every namespace Kueue manages** until it reschedules. HA buys the most here.

- The spread constraints carry no selector of their own. Unlike the worker's, they render as given, so
  a `DoNotSchedule` spread needs `labelSelector` spelled out: `app.kubernetes.io/name: kueue` plus
  `control-plane: controller-manager`, as in the example. Omit it and the spread counts every pod in
  the namespace.
- The Kueue chart has no affinity key, so spread constraints are its only placement control.

### NFD master

The NFD master turns detections into node labels: while it is down, no node is (re)classified and the
chain stalls for new or changed nodes. Labelled nodes and admitted workloads are unaffected.

- NFD spells the budget key `enable`, not `enabled`: a stray `enabled: true` is schema-valid and
  does nothing.
- Above one replica, NFD's chart adds `-enable-leader-election` for you. Standbys watch without
  writing.
- NFD's templates render no topology spread constraints, so spreading the master means
  `node-feature-discovery.master.affinity`, which **replaces** NFD's preference for control-plane nodes;
  re-state it if wanted.

The garbage collector stays at one replica: a stalled GC only delays cleanup of a departed node's
objects, which nothing reads.

### The two CSI controllers

Losing a CSI controller delays volume provisioning, resizing and snapshotting; volumes already mounted
keep working, because the mounting side is the node DaemonSet. These are the least urgent of the four,
with the weakest chart support.

Both charts render no PodDisruptionBudget, and honour `controller.affinity` **only when it carries
`nodeSelectorTerms`**: a pod anti-affinity is schema-valid, then silently dropped.

The S3 chart renders no topology spread at all, so two replicas may land on one node and a drain can
take both. Raise its count for process-level failover; it gives no node-level redundancy.

The NFS chart renders `controller.topologySpreadConstraints` through a vendored patch, and above one
replica the spread is **required, not just prudent**: its pods run on the host network and bind the
liveness health port there, so two pods on one node leave the second crash-looping on the bind and the
Deployment never fully ready. As with Kueue, the example's `labelSelector` must be spelled out.

Also set `strategyType: RollingUpdate`: both default to `Recreate`, which takes every replica down
before the new one starts and gives up, at every upgrade, the failover the replica was added for.

## The non-redundant topology

When the worker runs **outside** the cluster it manages but near it (image mode, `!LoopbackKubeInside &&
LoopbackKubeNearby`), its admission webhooks register against one node IP URL instead of a Service.
That URL is a single endpoint, so extra replicas receive no traffic; the source calls this "launch
multiple instances, only one takes working". **Keep that topology at one replica.** Chart mode is
unaffected: its webhooks are always in-cluster and Service-backed.

## Verify

```bash
NS=gpustack-system

# Every control-plane Deployment reports its full replica count Ready.
kubectl -n "$NS" get deploy

# The budgets exist and are satisfied (ALLOWED DISRUPTIONS ≥ 1).
kubectl -n "$NS" get pdb

# Replicas really are on distinct nodes — one line per pod, node in the second column.
kubectl -n "$NS" get pods -o wide \
  --field-selector status.phase=Running \
  -o custom-columns='POD:.metadata.name,NODE:.spec.nodeName'

# The NFD master elected a leader (only above one replica).
kubectl -n "$NS" logs deploy/node-feature-discovery-master | grep -i "leader"
```

Then the failover: drain the node running the worker's leader; a standby takes over:

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker
kubectl get instancetypes          # served throughout by the surviving replicas
kubectl uncordon <node>
```

---

**See also** — [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md) (these knobs need chart mode; image
mode has no user-values channel) ·
[Internals](/gpustack-operator/main/docs/contribute/internals/index.md#worker-startup-order) · [Settings](/gpustack-operator/main/docs/reference/settings/index.md) ·
[KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/leader/index.md#high-availability) — a `KVCacheBackend`'s leader elects the
same way but is **not** a chart component: it is a custom resource, so its replica count is a field on
the object rather than a value here

**Next** → [NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md).
