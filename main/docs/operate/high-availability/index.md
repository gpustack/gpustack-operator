# High Availability Operations

Every control-plane component in the chart elects a single active leader, so extra replicas provide
failover rather than higher reconciliation throughput. Enable high availability in `values.yaml` by
raising control-plane replica counts and enabling disruption budgets; node agents (Device Managers,
NFD worker, and CSI node `DaemonSet`s) already run one `Pod` per node.

## Contents

- [Prerequisites](#prerequisites)
- [Configuration](#configuration)
- [External worker webhook limits](#external-worker-webhook-limits)
- [Verification](#verification)

## Prerequisites

A `DoNotSchedule` spread requires at least as many schedulable `Node`s as your largest replica count,
or surplus `Pod`s stay `Pending`. The example below targets three replicas across three `Node`s; on
smaller clusters, lower replica counts or switch to `ScheduleAnyway`.

Control-plane components tolerate standard control-plane taints and no others (including cordon
taints), so drained `Pod`s reschedule elsewhere. If your `Node`s carry custom taints, add them to each
component's *`tolerations`*.

If you are upgrading from an older release where Kueue controller manager, NFD master, and NFD GC
tolerated all taints, add explicit *`tolerations`* first and run `helm upgrade` with
`--reset-then-reuse-values` rather than `--reuse-values`; see
[Upgrading the Chart](https://github.com/gpustack/gpustack-operator/blob/74aa9d360844952d0a6d400303a5e871bdc9e05a/deploy/gpustack-operator/chart/README.md#upgrading-the-chart).

## Configuration

Save these values as `ha.yaml` and apply them with `helm upgrade` (chart mode is required because
image mode exposes no subchart values):

```yaml
worker:
  replicas: 3
  podDisruptionBudget:
    enabled: true
    minAvailable: 2
  topologySpreadConstraints:  # labelSelector auto-populated when omitted; worker.affinity replaces default
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: DoNotSchedule

kueue:
  controllerManager:
    replicas: 3
    podDisruptionBudget:
      enabled: true
      minAvailable: 2
    topologySpreadConstraints:  # explicit labelSelector required; chart has no affinity key
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app.kubernetes.io/name: kueue
            control-plane: controller-manager

node-feature-discovery:
  master:
    replicaCount: 3  # auto-enables -enable-leader-election above 1
    podDisruptionBudget:
      enable: true  # NFD key is "enable", not "enabled"
      minAvailable: 2
    # affinity: {}  # optional; replaces NFD's default control-plane preference

csi-driver-nfs:
  controller:
    replicas: 2
    strategyType: RollingUpdate  # overrides Recreate default; chart has no PDB
    topologySpreadConstraints:  # required >1 replica due to hostNetwork port bind
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app: csi-nfs-controller

csi-driver-s3:
  controller:
    replicas: 2  # process-level failover only; chart has no PDB or spread field
    strategyType: RollingUpdate
```

```bash
helm upgrade gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --reset-then-reuse-values --values ha.yaml
```

### Worker control plane

Only the elected worker leader runs controllers, while every replica serves the aggregated extension
API (`instancetypes`, `devices`) and admission webhooks. Running one replica makes node loss block API
reads and `Pod` admission until reschedule.

Node spread belongs in `worker.topologySpreadConstraints`. By default, `worker.affinity` sets
preferred `Pod` anti-affinity so three replicas still start on two `Node`s; setting `worker.affinity`
replaces that default entirely.

### Kueue controller manager

Standby Kueue replicas do not reconcile, but every replica serves Kueue's webhooks (`failurePolicy: Fail`).
Losing a single-replica Kueue node blocks `Pod` creation across every Kueue-managed namespace.

Always include `labelSelector` in `kueue.controllerManager.topologySpreadConstraints`; without it,
Kubernetes counts every `Pod` in the namespace.

### NFD master

While NFD master is unavailable, existing labels and running workloads remain intact, and only new or
changed `Node`s pause classification. Keep NFD GC at one replica, because brief GC downtime only delays
cleanup of deleted `Node` records.

NFD spells its disruption budget switch `enable` (`enabled: true` is ignored) and supports placement
only through `node-feature-discovery.master.affinity`.

### NFS and S3 controllers

CSI controller outages delay new volume provisioning, resizing, and snapshots while mounted volumes
continue running through the CSI node `DaemonSet`s. Both bundled CSI charts omit `PodDisruptionBudget`
templates, honor `controller.affinity` only when *`nodeSelectorTerms`* is set, and default
`strategyType` to `Recreate` unless overridden to `RollingUpdate`.

On `csi-driver-nfs`, `DoNotSchedule` hostname spread is mandatory above one replica because controller
`Pod`s use `hostNetwork` and collide on the liveness port if co-located. `csi-driver-s3` exposes no
spread field, so two replicas provide process-level failover without guaranteed node separation.

## External worker webhook limits

When an image-mode worker runs outside the target cluster on an adjacent network
(`!LoopbackKubeInside && LoopbackKubeNearby`), its webhooks register against a single node IP URL
rather than a Kubernetes `Service`. Standby replicas receive no webhook traffic, so keep that topology
at **one replica**. Chart-mode webhooks always route through an in-cluster `Service`.

## Verification

Confirm that all control-plane `Deployment`s, disruption budgets, node placements, and leader
elections are healthy:

```bash
NS=gpustack-system

# 1. Every control-plane Deployment reports all replicas Ready:
kubectl -n "$NS" get deploy

# 2. Disruption budgets exist with ALLOWED DISRUPTIONS >= 1:
kubectl -n "$NS" get pdb

# 3. Replicas run on distinct Nodes:
kubectl -n "$NS" get pods --field-selector status.phase=Running \
  -o custom-columns='POD:.metadata.name,NODE:.spec.nodeName'

# 4. NFD master elected a leader:
kubectl -n "$NS" logs deploy/node-feature-discovery-master | grep -i "leader"
```

Test failover by draining the `Node` hosting the active worker leader and confirming uninterrupted API
reads:

```bash
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker
kubectl get instancetypes
kubectl uncordon <node>
```

---

**See also** — [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md) (HA values require chart mode) ·
[KV Cache Election and Failover](/gpustack-operator/main/docs/modules/kv-cache/leader/index.md#high-availability) (leader election on
`KVCacheBackend` custom resources)

**Next** → [NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md) — configure and verify MIG partition lifecycles.
