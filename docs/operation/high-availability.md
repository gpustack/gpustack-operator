# High Availability

> **Purpose** — which replica knob to raise per control-plane component, what each bundled subchart can
> and cannot spread, and the one topology that must stay single-replica.
> **Audience** operators · **Prerequisites** [Two install
> modes](../architecture/install-modes.md) · **Read time** ~8 min

Every control-plane component the operator chart deploys elects a leader. Extra replicas therefore
stand by rather than share the work: **they buy failover, not throughput.** A highly available
install raises the replica count of each one and turns on the disruption budget that goes with it.

Everything else the chart deploys is already one pod per node — the device managers, the NFD worker,
and both CSI node DaemonSets — so there is nothing to make redundant there.

All of it is configured in the file you already edit, `values.yaml`. No separate HA values file
ships with the chart; every knob below is declared there at its own chart's default, with the same
caveats repeated inline.

## Contents

- [Before you start: count your nodes](#before-you-start-count-your-nodes)
- [The knobs, per component](#the-knobs-per-component)
- [A values file to start from](#a-values-file-to-start-from)
- [Worker (control plane)](#worker-control-plane)
- [Kueue controller manager](#kueue-controller-manager)
- [NFD master](#nfd-master)
- [The two CSI controllers](#the-two-csi-controllers)
- [The one topology that cannot be made redundant](#the-one-topology-that-cannot-be-made-redundant)
- [Verify](#verify)

## Before you start: count your nodes

A `DoNotSchedule` topology spread is only satisfiable when the cluster has **at least as many
schedulable nodes as the largest replica count you set**. On a smaller cluster the surplus replicas
stay `Pending` forever. Three replicas across three nodes is the shape the rest of this page assumes;
with fewer nodes, either lower the counts or use `ScheduleAnyway`, which degrades to a preference.

The components below also default to tolerating control-plane taints, so a three-node cluster whose
workers are all control-plane nodes still spreads correctly.

## The knobs, per component

| Component | Replicas | PodDisruptionBudget | Node spread |
|---|---|---|---|
| Worker (control plane) | `worker.replicas` | `worker.podDisruptionBudget.enabled` + `.minAvailable` | `worker.topologySpreadConstraints`, or `worker.affinity` |
| Kueue controller manager | `kueue.controllerManager.replicas` | `kueue.controllerManager.podDisruptionBudget.enabled` + `.minAvailable` | `kueue.controllerManager.topologySpreadConstraints` |
| NFD master | `node-feature-discovery.master.replicaCount` | `node-feature-discovery.master.podDisruptionBudget.enable` + `.minAvailable` | `node-feature-discovery.master.affinity` only |
| NFS CSI controller | `csi-driver-nfs.controller.replicas` | — none — | — none — |
| S3 CSI controller | `csi-driver-s3.controller.replicas` | — none — | — none — |

Three of those cells are limitations of the upstream chart, not oversights on our side. They are
spelled out per component below, because each one changes what "three replicas" actually gets you.

## A values file to start from

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
helm upgrade gpustack-operator oci://docker.io/gpustack/charts/gpustack-operator \
  --namespace gpustack-system --values ha.yaml
```

## Worker (control plane)

The worker runs the aggregated extension API server *and* the scheduling-chain controllers in one
process. Leader election is always on, so the reconcilers only ever run in one replica — but **every
replica serves the extension API and the admission webhooks**, and that is what the extra replicas
are for: with one replica, losing its node takes `kubectl get instancetypes` and Pod admission down
until it is rescheduled.

`worker.topologySpreadConstraints` is where node spread belongs. The chart's default affinity is a
`preferred` pod anti-affinity on purpose — it keeps replicas off one node without ever making a
replica unschedulable, so three replicas still come up on a two-node cluster. An entry that omits
`labelSelector` gets the worker's own, so the example above needs no selector. Setting
`worker.affinity` **replaces** that default anti-affinity rather than adding to it.

## Kueue controller manager

Kueue's `managerConfig` elects a leader, so the standby replicas do not reconcile — but every one of
them serves Kueue's admission webhook, and that webhook has `failurePolicy: Fail`. With a single
replica, losing its node **blocks Pod creation in every namespace Kueue manages** until it is
rescheduled. This is the component where HA buys the most.

Two things to know:

- **The spread constraints carry no selector of their own.** Unlike the worker's, they are rendered
  exactly as given, so a `DoNotSchedule` spread across nodes needs its `labelSelector` spelled out —
  `app.kubernetes.io/name: kueue` plus `control-plane: controller-manager`, as in the example. Omit
  it and the constraint counts every pod in the namespace.
- **The Kueue chart has no affinity key at all**, so spread constraints are the only placement
  control for it.

## NFD master

The NFD master is what turns a device manager's detections into node labels, so while it is down no
node is ever (re)classified and the scheduling chain stalls for new or changed nodes. Nodes already
labelled keep their labels, and workloads already admitted keep running.

Three things to know:

- **NFD spells the budget key `enable`, not `enabled`.** A stray `enabled: true` here is accepted by
  the schema and silently does nothing.
- **Above one replica, NFD's own chart adds `-enable-leader-election` for you.** The standbys watch
  without writing; you do not pass anything.
- **NFD's templates render no topology spread constraints at all.** Spreading the master replicas
  across nodes therefore means setting `node-feature-discovery.master.affinity` — which **replaces**
  NFD's own preference for control-plane nodes rather than adding to it. If you set it, re-state that
  preference yourself if you still want it.

The NFD **garbage collector** is deliberately left at one replica: a stalled GC only delays the
cleanup of a departed node's objects, which nothing downstream reads.

## The two CSI controllers

Losing a CSI controller delays volume provisioning, resizing and snapshotting. **Volumes already
mounted keep working**, because the mounting side is the node DaemonSet — so this is the least urgent
of the four, and it is also the one with the weakest chart support.

Both charts render **neither a PodDisruptionBudget nor topology spread constraints** for the
controller, and both honour `controller.affinity` **only when that affinity carries
`nodeSelectorTerms`** — a pod anti-affinity is accepted by the schema and then silently dropped. So
two controller replicas may well land on the same node, and a node drain can take both at once. Raise
the count anyway for process-level failover; just do not read it as node-level redundancy.

Also set `strategyType: RollingUpdate`. Both charts default to `Recreate`, which takes every replica
down before starting the new one — giving up, during every upgrade, exactly the failover the extra
replica was added for.

## The one topology that cannot be made redundant

When the worker runs **outside** the cluster it manages but near it (image mode with
`!LoopbackKubeInside && LoopbackKubeNearby`), it registers its admission webhooks against a single
node IP URL instead of a Service. A URL names one endpoint, so extra replicas are not reachable
through it — the code's own comment reads "launch multiple instances, only one takes working". **Keep
that topology at one replica.**

Chart mode is unaffected: it always runs in-cluster, and its webhooks are Service-backed.

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

Then test the failover that matters: drain the node running the worker's leader and confirm a
standby takes over.

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker
kubectl get instancetypes          # served throughout by the surviving replicas
kubectl uncordon <node>
```

---

**See also** — [Two install modes](../architecture/install-modes.md) (image mode has no user-values
channel, so these knobs need chart mode) · [Internals](../architecture/internals.md#worker-startup-order-matters)
(what every replica runs before leader election) · [Settings](../settings.md)

**Next** → [NVIDIA MIG Operations](nvidia-mig.md) — the other operator runbook.
