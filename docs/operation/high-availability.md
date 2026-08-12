# High Availability Operations

> **Purpose** — which replica knob to raise per control-plane component, what each subchart can and
> cannot spread, and the one topology that must stay single-replica.
> **Audience** operators · **Prerequisites** [Two install
> modes](../architecture/installation-modes.md) · **Read time** ~4 min

Every control-plane component the chart deploys elects a leader, so extra replicas stand by: **they buy
failover, not throughput.** A highly available install raises each replica count and turns on its
disruption budget; everything else is one pod per node (device managers, NFD worker, both CSI node
DaemonSets).

Configure it in `values.yaml`: no HA values file ships; every knob below sits at its chart's default,
caveats inline.

## Contents

- [Before you start: count your nodes](#before-you-start-count-your-nodes)
- [The knobs, per component](#the-knobs-per-component)
- [The one topology that cannot be made redundant](#the-one-topology-that-cannot-be-made-redundant)
- [Verify](#verify)

## Before you start: count your nodes

A `DoNotSchedule` spread needs **at least as many schedulable nodes as your largest replica count**; below
that, surplus replicas stay `Pending` forever. This page assumes three replicas on three nodes; with
fewer, lower the counts or use `ScheduleAnyway`, a preference. The components tolerate control-plane
taints, so three control-plane nodes spread fine.

## The knobs, per component

| Component | Replicas | PodDisruptionBudget | Node spread |
|---|---|---|---|
| Worker (control plane) | `worker.replicas` | `worker.podDisruptionBudget.enabled` + `.minAvailable` | `worker.topologySpreadConstraints`, or `worker.affinity` |
| Kueue controller manager | `kueue.controllerManager.replicas` | `kueue.controllerManager.podDisruptionBudget.enabled` + `.minAvailable` | `kueue.controllerManager.topologySpreadConstraints` |
| NFD master | `node-feature-discovery.master.replicaCount` | `node-feature-discovery.master.podDisruptionBudget.enable` + `.minAvailable` | `node-feature-discovery.master.affinity` only |
| NFS CSI controller | `csi-driver-nfs.controller.replicas` | — none — | — none — |
| S3 CSI controller | `csi-driver-s3.controller.replicas` | — none — | — none — |

Those "none"/"only" cells are upstream chart limitations, not oversights: each changes what "three
replicas" gets you, and is spelled out below.

### A values file to start from

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

csi-driver-s3:
  controller:
    replicas: 2
    strategyType: RollingUpdate
```

```bash
helm upgrade gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --values ha.yaml
```

### Worker (control plane)

The worker runs the aggregated extension API server *and* the scheduling-chain controllers in one
process. Only one replica reconciles (leader election is always on) — but **every replica serves the
extension API and the admission webhooks**: with one, losing its node takes `kubectl get instancetypes`
and Pod admission down until it reschedules.

Node spread belongs in `worker.topologySpreadConstraints`. Its default `preferred` pod anti-affinity is
deliberate: replicas stay off one node without any becoming unschedulable, so three still come up on two
nodes. An entry omitting `labelSelector` gets the worker's own; `worker.affinity` **replaces** the
default, not adds to it.

### Kueue controller manager

Kueue's `managerConfig` elects a leader, so standbys do not reconcile — but each serves Kueue's admission
webhook, `failurePolicy: Fail`: with one replica, losing its node **blocks Pod creation in every namespace
Kueue manages** until rescheduled. HA buys the most here.

- **The spread constraints carry no selector of their own.** Unlike the worker's they render as given, so
  a `DoNotSchedule` spread needs `labelSelector` spelled out — `app.kubernetes.io/name: kueue` plus
  `control-plane: controller-manager`, as in the example; omit it and it counts every pod in the
  namespace.
- **The Kueue chart has no affinity key**: spread constraints are its only placement control.

### NFD master

The NFD master turns detections into node labels: while it is down, no node is (re)classified and the
chain stalls for new or changed nodes — labelled nodes and admitted workloads are unaffected.

- **NFD spells the budget key `enable`, not `enabled`** — a stray `enabled: true` is schema-valid and
  does nothing.
- **Above one replica, NFD's chart adds `-enable-leader-election` for you.** Standbys watch without
  writing.
- **NFD's templates render no topology spread constraints**, so spreading the master means
  `node-feature-discovery.master.affinity` — which **replaces** NFD's preference for control-plane nodes;
  re-state it if wanted.

The **garbage collector** stays at one replica: a stalled GC only delays cleanup of a departed node's
objects, which nothing reads.

### The two CSI controllers

Losing a CSI controller delays volume provisioning, resizing and snapshotting; **volumes already mounted
keep working**: the mounting side is the node DaemonSet. The least urgent of the four, with the weakest
chart support.

Both charts render **neither a PodDisruptionBudget nor topology spread constraints**, and honour
`controller.affinity` **only when it carries `nodeSelectorTerms`** — a pod anti-affinity is schema-valid,
then silently dropped. Two replicas may land on one node, a drain taking both: raise the count for
process-level failover, not node-level redundancy.

Also set `strategyType: RollingUpdate`: both default to `Recreate`, taking every replica down before the
new one starts — giving up at every upgrade the failover the replica was added for.

## The one topology that cannot be made redundant

When the worker runs **outside** the cluster it manages but near it (image mode, `!LoopbackKubeInside &&
LoopbackKubeNearby`), its admission webhooks register against one node IP URL, not a Service: one
endpoint, so extra replicas are unreachable ("launch multiple instances, only one takes working", says
the code). **Keep that topology at one replica.** Chart mode is unaffected: always in-cluster,
Service-backed webhooks.

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

**See also** — [Installation Modes](../architecture/installation-modes.md) (these knobs need chart mode; image
mode has no user-values channel) ·
[Internals](../architecture/internals.md#worker-startup-order-matters) · [Settings](../settings.md)

**Next** → [NVIDIA MIG Operations](nvidia-mig.md).
