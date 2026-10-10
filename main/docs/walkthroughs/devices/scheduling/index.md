# Scheduling Walkthrough

This page records a real session: every `kubectl` invocation and its real output, objects as YAML
trimmed to `metadata.labels` / `spec` / `status`, and a before / after `kubectl get instancetypes`
per operation. Node names are genericized (`node-cpu`, `node-a10g`, `node-t4-a`, `node-t4-b`).

The run uses the defaults: `instance-type-derived-from-node=true` auto-derives the pool objects,
`instance-type-aware-cpu-manufacturer=false` keeps the CPU manufacturer out of the aggregation layer
until section 5 flips it on.

Watch three columns: **UNIT(CPU/RAM)/STORAGE**, the per-unit request the InstanceType charges; **CPU**,
the collapsed CPU pool's `remaining/capacity` cores; **ACCELERATOR(EX/SH/SL/PT)**, the
`onceMaxRequest/remaining` of each [four-view](/gpustack-operator/main/docs/modules/devices/admission/index.md#four-view-status)
projection. This run predates the shared card-count reading, so its `SH` once-max figures are
shares, not accelerators ([Pre-release breaks](/gpustack-operator/main/docs/modules/devices/requests/index.md#pre-release-breaks)).

Each accelerator counts in exactly one of `EX`/`SH`/`SL` (unpartitioned) or `PT` (partitioned), so
`0/0` under `PT` throughout means none is in a partitioning mode. For the all-partitioned and **mixed**
configurations see the [three-configuration
walkthrough](../../modules/devices/nvidia-mig.md#walkthrough-three-mig-configurations-on-one-node).

## Contents

- [The cluster](#the-cluster)
- [1. Initial state](#1-initial-state)
- [2. Removing a node from management](#2-removing-a-node-from-management)
- [3. Requesting a logical sliced GPU](#3-requesting-a-logical-sliced-gpu)
- [4. Managing a custom InstanceType](#4-managing-a-custom-instancetype)
- [5. Enabling CPU-manufacturer awareness](#5-enabling-cpu-manufacturer-awareness)
- [6. Pinning an Instance to a node and adding volumes](#6-pinning-an-instance-to-a-node-and-adding-volumes)
- [Troubleshooting](#troubleshooting)

## The cluster

Four `linux/amd64` nodes, all operator-managed:

| Node | CPU (`gKey`) | Cores | Accelerator (`aKey`) |
|---|---|---|---|
| `node-cpu` | `amd-epyc-7r13` | 16 | — |
| `node-a10g` | `amd-epyc-7r32` | 4 | 1 × NVIDIA A10G (`nvidia-a10g`) |
| `node-t4-a` | `intel-xeon-platinum-8259cl` | 4 | 1 × NVIDIA Tesla T4 (`nvidia-tesla-t4`) |
| `node-t4-b` | `intel-xeon-platinum-8259cl` | 48 | 4 × NVIDIA Tesla T4 (`nvidia-tesla-t4`) |

---

## 1. Initial state

With `instance-type-derived-from-node` on, the operator materializes the finest-grain `ResourceFlavor`s
and one collapsed pool per accelerator plus a generic CPU pool:

```console
$ kubectl get resourceflavor
NAME                                                                   AGE
gpustack--amd-epyc-7r13-linux-amd64-16c                                5h19m
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d                    9m26s
gpustack--amd-epyc-7r32-linux-amd64-4c                                 9m26s
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4-linux-amd64-1d   5h19m
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4-linux-amd64-4d   5h19m
gpustack--intel-xeon-platinum-8259cl-linux-amd64-48c                   5h19m
gpustack--intel-xeon-platinum-8259cl-linux-amd64-4c                    5h19m

$ kubectl get clusterqueue
NAME                                    COHORT   PENDING WORKLOADS
gpustack--generic-linux-amd64                    0
gpustack--nvidia-a10g-linux-amd64                0
gpustack--nvidia-tesla-t4-linux-amd64            0

$ kubectl get instancetype
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active

$ kubectl get instancetypeflavor
NAME                        GENERALGROUP   ACCELERATORGROUP   ACCELERATABLE   MANUFACTURER   PRODUCT       MEMORY   CORES   SLICEABLE
gpustack--generic           generic                          false           generic
gpustack--nvidia-a10g                      nvidia-a10g        true            nvidia         NVIDIA-A10G   24Gi     10240   true
gpustack--nvidia-tesla-t4                  nvidia-tesla-t4    true            nvidia         Tesla-T4      16Gi     2560    true

$ kubectl get devices
NAME
node-t4-a
node-a10g
node-t4-b
```

- **7 ResourceFlavors**, one per `(gKey, [aKey,] os, arch, count)` — `node-t4-b` (48 cores, 4×T4)
  yields both `…-48c` and `…--nvidia-tesla-t4-…-4d`.
- **3 ClusterQueues / InstanceTypes**, collapsed — **A10G** `1/1 10/10 100/100 0/0` (1 accelerator),
  **T4** `4/5 40/50 100/500 0/0` (5 = `node-t4-a`'s 1 + `node-t4-b`'s 4, none partitioned).
- **No `Devices` for `node-cpu`** — it carries no accelerator.

One of each kind, from the A10G node:

### Node

Labeled by two NodeFeatures: `node-a10g-gpustack-worker` (the `general.*` CPU keys + `managed`) and
`node-a10g-gpustack-device-manager` (the `acceleratable.*` device keys):

```yaml
apiVersion: v1
kind: Node
metadata:
  name: node-a10g
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    general.feature.gpustack.ai/amd-epyc-7r32.count: "4"
    acceleratable.feature.gpustack.ai/nvidia: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g.count: "1"
    acceleratable.feature.gpustack.ai/nvidia-a10g.memory: "24Gi"
    acceleratable.feature.gpustack.ai/nvidia-a10g.cores: "10240"
    acceleratable.feature.gpustack.ai/nvidia-a10g.family: "Ampere"
    acceleratable.feature.gpustack.ai/nvidia-a10g.product: "NVIDIA-A10G"
    acceleratable.feature.gpustack.ai/nvidia-a10g.comcap: "8.6"
    acceleratable.feature.gpustack.ai/nvidia.driver-version: "580.159.03"
    acceleratable.feature.gpustack.ai/nvidia.runtime-version: "13.0"
```

### Devices

Cluster-scoped, named after the node: the worker stamps `gpustack.ai/managed` + the real CPU key, the
Device Manager the accelerator key and the `.status` ledger:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Devices
metadata:
  name: node-a10g
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
status:
  groups:
    - id: a10g
      manufacturer: nvidia
      accelerators:
        - id: GPU-e0587d2e-127c-4fb8-e2c1-6e517529f575
          index: 0
          mode: 0
          remaining: 1600000        # per-accelerator credit ledger the AdmissionCheck reads
```

### ResourceFlavor

The finest, setting-independent grain, `gpustack--${gKey}[--${aKey}]-${os}-${arch}-${count}{c|d}`. An
**accelerated** one carries `feature.gpustack.ai/acceleratable=true`, both the CPU (`general.`) and
device (`acceleratable.`) keys, and pins nodes via `spec.nodeLabels`:

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g.count: "1"       # per-node accelerator count
    acceleratable.feature.gpustack.ai/nvidia-a10g.capacity: "1"    # pooled capacity (nodes × count)
    resource.gpustack.ai/type: nodes                               # operator-owned marker
spec:
  nodeLabels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g.count: "1"
```

A **non-accelerated** (CPU) flavor carries `feature.gpustack.ai/acceleratable=false`, its capacity the
node's CPU-core count:

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: gpustack--amd-epyc-7r13-linux-amd64-16c
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "false"
    general.feature.gpustack.ai/amd-epyc-7r13: "true"
    general.feature.gpustack.ai/amd-epyc-7r13.count: "16"
    general.feature.gpustack.ai/amd-epyc-7r13.capacity: "16"
    resource.gpustack.ai/type: nodes
spec:
  nodeLabels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd-epyc-7r13: "true"
    general.feature.gpustack.ai/amd-epyc-7r13.count: "16"
```

### ClusterQueue

One isolated pool per accelerator, covering the manufacturer's `credits` and gating admission with the
per-accelerator `AdmissionCheck`. With awareness off its labels carry **no** `general.` key, so it
aggregates the A10G across every CPU:

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: gpustack--nvidia-a10g-linux-amd64
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    resource.gpustack.ai/type: instancetypes
spec:
  admissionChecksStrategy:
    admissionChecks:
      - name: gpustack-node-devices
  resourceGroups:
    - coveredResources:
        - credits.gpustack.ai/nvidia
      flavors:
        - name: gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d
          resources:
            - name: credits.gpustack.ai/nvidia
              nominalQuota: 1600k
```

### InstanceType

The schedulable pool: webhook-stamped labels (schedule discriminators, `derived-from-node` provenance,
the fronting `queue-entrance`), `spec` enriched from the matching flavor, `status` the reconciled
four-view:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: gpustack--nvidia-a10g-linux-amd64
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    schedule.gpustack.ai/derived-from-node: "true"
    schedule.gpustack.ai/queue-entrance: gpustack-fnv64-c4680bb149644f1c
spec:
  acceleratable: true
  acceleratorGroup: nvidia-a10g
  generalGroup: generic          # collapsed — awareness is off
  os: linux
  arch: amd64
  manufacturer: nvidia
  product: NVIDIA-A10G
  family: Ampere
  memory: 24Gi
  cores: "10240"
  feature:
    logicalSliced:
      maxSize: 128
      coresPercentageOvercommit: true
      memoryPercentageStep: 1
    physicalSliced:
      maxSize: 0
  unitResources:            # the nvidia-a10g preset, not a fixed default
    cpu: "8"
    ram: 64Gi
  localStorage: 100Gi
status:
  phase: Active
  accelerator:
    capacity: "1"
    onceMaxRequest: "1"
    remaining: "1"
  acceleratorShared:
    capacity: "10"
    onceMaxRequest: "10"
    remaining: "10"
  acceleratorSliced:
    capacity: "100"
    onceMaxRequest: "100"
    remaining: "100"
  acceleratorPartitioned:          # no accelerator here is in a partitioning mode
    capacity: "0"
    onceMaxRequest: "0"
    remaining: "0"
  entrance: gpustack-fnv64-c4680bb149644f1c
```

### InstanceTypeFlavor

An os/arch-agnostic catalog view aggregated read-only from the flavors, with **no
`metadata.labels`** — its grouping identity lives in `spec`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceTypeFlavor
metadata:
  name: gpustack--nvidia-a10g
spec:
  acceleratable: true
  acceleratorGroup: nvidia-a10g
  manufacturer: nvidia
  product: NVIDIA-A10G
  memory: 24Gi
  cores: "10240"
  sliceable: true
```

---

## 2. Removing a node from management

A node stays in a pool only while it carries `gpustack.ai/managed=true`, required by the flavor's
`spec.nodeLabels` and stamped through the node's worker NodeFeature.

**Before** — `node-a10g` managed:

```console
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The worker writes one NodeFeature per node, in `gpustack-system`, named `<node>-gpustack-worker`
(see [Device Scheduling](/gpustack-operator/main/docs/modules/devices/scheduling/index.md)).
This is the one for `node-a10g`, which carries the managed label and the CPU keys from the Node above:

```yaml
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: node-a10g-gpustack-worker
  namespace: gpustack-system
  labels:
    nfd.node.kubernetes.io/node-name: node-a10g
    app.kubernetes.io/part-of: gpustack-operator-worker
spec:
  labels:
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    general.feature.gpustack.ai/amd-epyc-7r32.count: "4"
```

Flip that label off:

```console
$ kubectl -n gpustack-system patch nodefeature node-a10g-gpustack-worker \
    --type=merge -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'
nodefeature.nfd.k8s-sigs.io/node-a10g-gpustack-worker patched
```

```diff
 apiVersion: nfd.k8s-sigs.io/v1alpha1
 kind: NodeFeature
 metadata:
   name: node-a10g-gpustack-worker
   namespace: gpustack-system
   labels:
     nfd.node.kubernetes.io/node-name: node-a10g
     app.kubernetes.io/part-of: gpustack-operator-worker
 spec:
   labels:
-    gpustack.ai/managed: "true"
+    gpustack.ai/managed: "false"
     general.feature.gpustack.ai/amd: "true"
     general.feature.gpustack.ai/amd-epyc-7r32: "true"
     general.feature.gpustack.ai/amd-epyc-7r32.count: "4"
```

**After** — NFD propagates it; the operator retires the now-nodeless A10G flavor:

```console
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/68   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

Two rows move: the A10G to `0/0 0/0 0/0` (its ResourceFlavor is deleted), and the generic **CPU**
`48/72` → `48/68`, as `node-a10g`'s 4 cores leave the pool.

Re-admit it and one reconcile rebuilds the flavor and restores the counts:

```console
$ kubectl -n gpustack-system patch nodefeature node-a10g-gpustack-worker \
    --type=merge -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}'

$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

```diff
 apiVersion: nfd.k8s-sigs.io/v1alpha1
 kind: NodeFeature
 metadata:
   name: node-a10g-gpustack-worker
   namespace: gpustack-system
   labels:
     nfd.node.kubernetes.io/node-name: node-a10g
     app.kubernetes.io/part-of: gpustack-operator-worker
 spec:
   labels:
-    gpustack.ai/managed: "false"
+    gpustack.ai/managed: "true"
     general.feature.gpustack.ai/amd: "true"
     general.feature.gpustack.ai/amd-epyc-7r32: "true"
     general.feature.gpustack.ai/amd-epyc-7r32.count: "4"
```

---

## 3. Requesting a logical sliced GPU

A sliceable InstanceType — the A10G reports logical slicing in its status detail — admits
fractional-accelerator workloads. Request 20 % of an accelerator's VRAM with
`acceleratorSlicedMemoryPercentage`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: sliced-demo
  namespace: default
spec:
  type: gpustack--nvidia-a10g-linux-amd64
  image: ubuntu:24.04
  command:
    - sleep
    - "86400"
  resources:
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: 20      # 20% of the accelerator's VRAM
    acceleratorSlicedCoresPercentage: 100
  volume:
    ephemeral:
      capacity: 1Gi
```

**Before**, the A10G shows `1/1 10/10 100/100`. Apply it and wait for `Ready`:

```console
$ kubectl apply -f sliced-demo.yaml
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 80/80 0/0          0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The A10G row moves `1/1 10/10 100/100 0/0` → `0/0 0/0 80/80 0/0`: **SL** gives up the 20 % slice;
**EX** and **SH** fall to `0/0` because a partly-sliced accelerator is neither whole nor a shared
unit; **PT** stays `0/0`, since no partitioning mode means no hardware partition.

Inside the Instance, the logical-slicing runtime caps visible VRAM to the slice, ≈ 20 % of 24 GiB:

```console
$ kubectl exec sliced-demo -- nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
NVIDIA A10G, 4912 MiB
```

Deleting the Instance releases the slice: the row returns to `1/1 10/10 100/100 0/0`.

> **Physical partitioning (MIG).** The A10G slices logically: a runtime caps a shared accelerator, and
> the `SL` view above tracks the per-accelerator credit budget. A MIG-capable accelerator (A100 / H100)
> instead hard-partitions into fixed hardware instances the operator materializes on demand:
>
> - a different resource family (`.partitioned*`, reported under `PT`), with a different request shape —
>   keys and rules in [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md);
> - MIG *mode* is the administrator's, driven with `nvidia-smi`, so it has its own runbook and a recorded
>   enable → request → reclaim → disable walkthrough in
>   [NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md).

---

## 4. Managing a custom InstanceType

A derived pool is sized from a **per-product preset**: 8 CPU / 64 GiB for this A10G, 4 CPU / 16 GiB for
an unrecognised accelerator ([preset reference](/gpustack-operator/main/docs/reference/instance-type-unit-resources/index.md)). For
another size, an admin authors an InstanceType referencing a catalog flavor by its `acceleratorGroup`,
with a unit spec of its own:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: a10g-12c128g
spec:
  acceleratable: true
  acceleratorGroup: nvidia-a10g          # references the gpustack--nvidia-a10g catalog flavor
  os: linux
  arch: amd64
  unitResources:
    cpu: "12"
    ram: 128Gi
  localStorage: 200Gi
```

Apply it: the defaulting webhook enriches its descriptors from the matching flavor, and it lands as a
**sibling** of the derived A10G pool, on the one physical accelerator:

```console
$ kubectl apply -f a10g-12c128g.yaml
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
a10g-12c128g                            gpustack-fnv64-8cf5b3114035c84a   12/128Gi/200Gi          1/1 10/10 100/100 0/0      0/0     Active
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The new row shows `12/128Gi/200Gi` against the derived `8/64Gi/100Gi`, and both siblings `1/1`: one
accelerator, two views of it.

Deploy an Instance onto the custom type, whole accelerator:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: custom-demo
  namespace: default
spec:
  type: a10g-12c128g
  image: ubuntu:24.04
  command:
    - sleep
    - "86400"
  resources:
    accelerator: "1"
  volume:
    ephemeral:
      capacity: 1Gi
```

Once `custom-demo` is `Ready`, both siblings drop to `0/0 0/0 0/0`, consistent across the
accelerator's two views:

```console
$ kubectl apply -f custom-demo.yaml
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
a10g-12c128g                            gpustack-fnv64-8cf5b3114035c84a   12/128Gi/200Gi          0/0 0/0 0/0 0/0            0/0     Active
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

Deleting it retires in order: the operator drains the Instance (`HoldAndDrain`), the Instance stops,
and the type plus its ClusterQueue go:

```console
$ kubectl delete instancetype a10g-12c128g
instancetype.worker.gpustack.ai "a10g-12c128g" deleted

$ kubectl -n default get instance custom-demo -o jsonpath='{.status.phase}'
Stopped

$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

`a10g-12c128g` is gone, `custom-demo` is `Stopped` (kept for section 5), and the released accelerator
returns the derived pool to `1/1 10/10 100/100`.

---

## 5. Enabling CPU-manufacturer awareness

Flipping `instance-type-aware-cpu-manufacturer` on makes the aggregation layer split every pool by
the CPU key. `ResourceFlavor`s are never rewritten; only queues, types and catalog re-group.

```console
$ kubectl -n gpustack-system patch secret gpustack-settings --type=merge \
    -p '{"data":{"instance-type-aware-cpu-manufacturer":"'"$(printf true | base64)"'"}}'
$ kubectl -n gpustack-system rollout restart deploy/gpustack-operator-worker
```

**Before**, 3 InstanceTypes; **after** the re-derive, CPU-aware types appear (create-only)
**alongside** the old collapsed ones:

```console
$ kubectl get instancetypes
NAME                                                                ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--amd-epyc-7r13-linux-amd64                                 gpustack-fnv64-8dde992f64a17a2f   1/2Gi/100Gi             0/0 0/0 0/0 0/0            16/16   Active
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64                    gpustack-fnv64-029fd9550e0c70bd   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--amd-epyc-7r32-linux-amd64                                 gpustack-fnv64-d3390f10cd57a632   1/2Gi/100Gi             0/0 0/0 0/0 0/0            4/4     Active
gpustack--generic-linux-amd64                                       gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-5b59e508edc027b7   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
gpustack--intel-xeon-platinum-8259cl-linux-amd64                    gpustack-fnv64-c6aee2b7b5c4dc6b   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/52   Active
gpustack--nvidia-a10g-linux-amd64                                   gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64                               gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

- Per-CPU pools split the generic 72 cores: `amd-epyc-7r13` `16/16`, `amd-epyc-7r32` `4/4`,
  `intel-xeon-platinum-8259cl` `48/52`.
- Per-`(gKey,aKey)` accelerated pools appear: `…amd-epyc-7r32--nvidia-a10g…`,
  `…intel-xeon-platinum-8259cl--nvidia-tesla-t4…`.
- The old collapsed rows remain, create-only and not garbage-collected.

> **Cleanup hint.** An admin may delete the stale ones:
> `kubectl delete instancetype gpustack--generic-linux-amd64 gpustack--nvidia-a10g-linux-amd64 gpustack--nvidia-tesla-t4-linux-amd64`.

Re-purpose the `custom-demo` Instance stopped in section 4: a drained Instance carries operator-set
`spec.stop: true`, so repoint its `type` at a CPU-aware pool and clear the stop:

```console
$ kubectl -n default patch instance custom-demo --type=merge \
    -p '{"spec":{"type":"gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64","stop":false}}'
```

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: Instance
 metadata:
   name: custom-demo
   namespace: default
 spec:
-  type: a10g-12c128g
-  stop: true
+  type: gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64
+  stop: false
```

```console
$ kubectl get instancetypes
NAME                                               ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64   gpustack-fnv64-029fd9550e0c70bd   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
gpustack--nvidia-a10g-linux-amd64                  gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
```

`custom-demo` goes `Ready` on the aware pool, which drops to `0/0 0/0 0/0`, and the collapsed
`gpustack--nvidia-a10g-linux-amd64` also drops: one accelerator, two consistent views.

The re-derive also creates an aware ClusterQueue. It is a second object beside the collapsed
`gpustack--nvidia-a10g-linux-amd64` queue from section 1, and its labels **carry the CPU key**:

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"     # <-- CPU key added when aware
    resource.gpustack.ai/type: instancetypes
```

The InstanceTypeFlavor catalog re-groups to per-`(gKey,aKey)` + per-`gKey`, replacing the collapsed
rows:

```console
$ kubectl get instancetypeflavor
NAME                                                    GENERALGROUP                 ACCELERATORGROUP   ACCELERATABLE   MANUFACTURER   PRODUCT                                          MEMORY   CORES   SLICEABLE
gpustack--amd-epyc-7r13                                 amd-epyc-7r13                                   false           amd            AMD EPYC 7R13 Processor
gpustack--amd-epyc-7r32                                 amd-epyc-7r32                                   false           amd            AMD EPYC 7R32
gpustack--intel-xeon-platinum-8259cl                    intel-xeon-platinum-8259cl                      false           intel          Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz
gpustack--amd-epyc-7r32--nvidia-a10g                    amd-epyc-7r32                nvidia-a10g        true            nvidia         NVIDIA-A10G                                      24Gi     10240   true
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4   intel-xeon-platinum-8259cl   nvidia-tesla-t4    true            nvidia         Tesla-T4                                         16Gi     2560    true
```

The `ResourceFlavor` set is byte-for-byte unchanged, the same 7 as section 1: the flip re-grouped
only the aggregation layer.

```console
$ kubectl get resourceflavor --no-headers | wc -l
7
```

Turning it back off collapses the layer again, still without touching a flavor.

---

## 6. Pinning an Instance to a node and adding volumes

An Instance normally lets the scheduler pick any node its pool covers. `spec.nodeName` narrows that to
one, and `spec.additionalVolumes` mounts paths beside the workspace — a shared dataset, one ConfigMap
key, a directory on the node itself:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: pinned-demo
  namespace: default
spec:
  type: gpustack--nvidia-tesla-t4-linux-amd64
  nodeName: node-t4-b                          # pin to this node
  image: ubuntu:24.04
  command:
    - sleep
    - "86400"
  resources:
    accelerator: "1"
  volume:
    ephemeral:
      capacity: 1Gi
  volumeMount: /workspace                      # the workspace, as always
  additionalVolumes:
    - mountPath: /mnt/datasets                 # a shared dataset, read-write
      persistent:
        name: datasets
    - mountPath: /etc/model/config.json        # one ConfigMap key, as a file
      subPath: config.json
      readOnly: true
      configMap:
        name: model-config
    - mountPath: /mnt/host-cache               # a directory on node-t4-b itself
      readOnly: true
      hostPath:
        path: /var/lib/gpustack-cache
        type: DirectoryOrCreate
```

- **The pin is a `nodeSelector`, never a direct assignment.** The Pod gets one selector entry,
  `kubernetes.io/hostname: <the node's own hostname label>`, read from the Node because a provider may
  set it to something other than the Node's name. `pod.spec.nodeName` stays with the scheduler, so the
  Pod still queues through Kueue's `ClusterQueue` quota and the `node-devices` AdmissionCheck's
  per-accelerator feasibility gate; an unsatisfiable pin goes Pending with the scheduler's own reason
  instead of running elsewhere.
- **The node only has to exist**, checked at *creation*. It need not be managed by the operator nor
  belong to the pinned type's pool: an accelerator-less Instance that only downloads a model must
  still land on a specific accelerated node.
- **Pool membership stays the scheduler's business.** A `type`'s pool and a pin's node are decided
  independently, so a pin into a heterogeneous pool can be admitted by Kueue and then stay Pending
  because the chosen flavor's labels do not match that node.
- **Each additional volume needs an absolute, canonical `mountPath`** duplicating neither another
  entry's path nor `spec.volumeMount`, and **exactly one** source: `persistent` (an
  `InstancePersistentVolume` in the same namespace), `configMap`, `secret` or `hostPath`. `readOnly`
  and `subPath` behave as on any Pod volume mount.
- **They mount into the workload container only.** The SSH sidecar needs no change: it enters that
  container's mount namespace per session, so every mount is visible over SSH too.
- **A persistent claim places the Instance where it can attach.** The workspace claim and every
  `persistent` entry follow the [claim placement rules](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md#claim-delivery-and-placement):
  a node-local volume pins the Instance to its node and that node's capacity; network or `ReadWriteMany`
  storage avoids the pin. A claim nothing binds creates no Pod and says why in `status.phaseMessage`;
  [`instance-persistent-volume-placement`](/gpustack-operator/main/docs/reference/settings/index.md#online-adjustable-settings) turns this off.
- **Both new fields are immutable while the Instance runs**, editable while stopped — the rule the rest
  of `spec` follows.

Two cross the host boundary, so each has its own administrator Setting, both `false` by default and
kept separate so node-path mounts can be allowed without a container escape:

| Setting | Gates |
|---|---|
| `instance-privileged-allowed` | `spec.privileged` — escapes the container boundary, exposing the node's devices and kernel surface. |
| `instance-host-path-volume-allowed` | `spec.additionalVolumes[*].hostPath` — reaches the node's filesystem, but not its devices or kernel. |

Each gates taking its escape, so turning one off stops new grants without stranding an Instance
that already holds one; [Settings](/gpustack-operator/main/docs/reference/settings/index.md#online-adjustable-settings) has the exact terms.

Both govern the **node** boundary, not the namespace one: a `persistent`, `configMap` or `secret`
source names an object in the Instance's own namespace and may always be mounted, the same reach a Pod
there has. Namespaces stay the tenancy boundary: put Instances whose authors should not read each
other's Secrets in their own.

## Troubleshooting

| Symptom | Root cause / Condition | Check & Mitigation |
|---|---|---|
| An Instance does not become Ready and its Workload stays pending | Quota, flavor labels or a node pin the scheduler cannot satisfy | Read `kubectl -n <namespace> get instance <name> -o jsonpath='{.status.phaseMessage}'`, then `kubectl -n <namespace> get workloads`. Compare the type's remaining counts in `kubectl get instancetypes`. See [section 6](#6-pinning-an-instance-to-a-node-and-adding-volumes) for pins |
| Pod rejected: `a Pod may request only one accelerator family, found [...]` | Two accelerator families in one Pod | Request one family. See [rule 1](/gpustack-operator/main/docs/modules/devices/requests/index.md#rule-1--one-family-in-exactly-one-container-group) |
| Pod rejected: `at most one container may request a slicing family, found N` | More than one container requests a slice or a partition | Keep one claiming container. See [rule 6](/gpustack-operator/main/docs/modules/devices/requests/index.md#rule-6--at-most-one-container-may-request-a-slicing-family) |
| Pod rejected: `a restartable init container (a native sidecar) may not request an accelerator` | A native sidecar carries the accelerator request | Move the request to an app container. See [rule 7](/gpustack-operator/main/docs/modules/devices/requests/index.md#rule-7--a-restartable-init-container-may-not-request-an-accelerator) |
| A node's accelerators are missing from the `InstanceType` pool | The node's NodeFeature carries `gpustack.ai/managed: "false"`, or `node-management-manual` is `true` and no one labeled the node | Run `kubectl -n gpustack-system get nodefeature <node>-gpustack-worker -o yaml` and read `spec.labels`. See [section 2](#2-removing-a-node-from-management) |
| Instance rejected: `privileged mode is not allowed: enable the "instance-privileged-allowed" setting to allow it` | The `instance-privileged-allowed` setting is `false` | Allow it only if the node boundary is acceptable: `kubectl -n gpustack-system patch setting instance-privileged-allowed --type merge -p '{"spec":{"value":"true"}}'`. See [Settings](/gpustack-operator/main/docs/reference/settings/index.md) |

---

**See also** — [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) (the contract behind step 3) ·
[NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#walkthrough-three-mig-configurations-on-one-node)
(hardware partitioning) · [Settings](/gpustack-operator/main/docs/reference/settings/index.md#online-adjustable-settings) (the two gates)

**Next** → [RDMA Network Endpoints Walkthrough](/gpustack-operator/main/docs/walkthroughs/rdma/network-endpoints/index.md) — request and verify RDMA endpoints with NUMA alignment.
