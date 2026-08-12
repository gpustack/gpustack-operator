# Walkthrough

> **Purpose** — the whole scheduling chain on a real four-node cluster: every materialized object as
> real YAML, and a before/after for each operation.
> **Audience** everyone · **Prerequisites** [Architecture](./architecture.md) · **Read time** ~12 min

Real `kubectl` invocations with their real output, objects as YAML trimmed to `metadata.labels` /
`spec` / `status`, and a **before / after** per operation via `kubectl get instancetypes`. Node names
are genericized (`node-cpu`, `node-a10g`, `node-t4-a`, `node-t4-b`).

The run uses the defaults: `instance-type-derived-from-node=true` auto-derives the pool objects,
`instance-type-aware-cpu-manufacturer=false` keeps the CPU manufacturer out of the aggregation layer
until section 5 flips it on.

Watch three columns: **UNIT(CPU/RAM)/STORAGE**, the per-unit request the InstanceType charges; **CPU**,
the collapsed CPU pool's `remaining/capacity` cores; **ACCELERATOR(EX/SH/SL/PT)**, the
`onceMaxRequest/remaining` of each [four-view](./architecture/admission.md#four-view-status)
projection.

Each accelerator counts in exactly one of `EX`/`SH`/`SL` (unpartitioned) or `PT` (partitioned), so
`0/0` under `PT` throughout means none is in a partitioning mode. For the all-partitioned and **mixed**
configurations see the [three-configuration
walkthrough](./operation/nvidia-mig.md#walkthrough-three-mig-configurations-on-one-node).

## Contents

- [The cluster](#the-cluster)
- [1. Initial state](#1-initial-state)
- [2. Removing a node from management](#2-removing-a-node-from-management)
- [3. Requesting a logical sliced GPU](#3-requesting-a-logical-sliced-gpu)
- [4. Managing a custom InstanceType](#4-managing-a-custom-instancetype)
- [5. Enabling CPU-manufacturer awareness](#5-enabling-cpu-manufacturer-awareness)
- [6. Pinning an Instance to a node, and mounting more than the workspace](#6-pinning-an-instance-to-a-node-and-mounting-more-than-the-workspace)

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

Flip that label off:

```console
$ kubectl -n gpustack-system patch nodefeature node-a10g-gpustack-worker \
    --type=merge -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'
nodefeature.nfd.k8s-sigs.io/node-a10g-gpustack-worker patched
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

---

## 3. Requesting a logical sliced GPU

A sliceable InstanceType — the A10G reports logical slicing in its status detail — admits
fractional-accelerator workloads. Request 20 % of an accelerator's VRAM with
`acceleratorSlicedMemoryPercentage`:

```yaml
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
**EX** and **SH** fall to `0/0`, a partly-sliced accelerator being neither whole nor a shared unit;
**PT** stays `0/0` — no partitioning mode, no hardware partition.

Inside the Instance, the logical-slicing runtime caps visible VRAM to the slice, ≈ 20 % of 24 GiB:

```console
$ kubectl exec sliced-demo -- nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
NVIDIA A10G, 4912 MiB
```

Deleting the Instance releases the slice: the row returns to `1/1 10/10 100/100 0/0`.

> **Physical partitioning (MIG).** The A10G slices *logically*: a runtime caps a shared accelerator, and
> the `SL` view above tracks the per-accelerator credit budget. A MIG-capable accelerator (A100 / H100)
> instead **hard-partitions** into fixed hardware instances the operator materializes on demand:
>
> - a different resource family (`.partitioned*`, reported under `PT`), with a different request shape —
>   keys and rules in [Accelerator Requests](./accelerator-requests.md);
> - MIG *mode* is the administrator's, driven with `nvidia-smi`, so it has its own runbook and a recorded
>   enable → request → reclaim → disable walkthrough in
>   [NVIDIA MIG Operations](./operation/nvidia-mig.md).

---

## 4. Managing a custom InstanceType

A derived pool is sized from a **per-product preset**: 8 CPU / 64 GiB for this A10G, 4 CPU / 16 GiB for
an unrecognised accelerator ([preset reference](./reference/instance-type-unit-resources.md)). For
another size, an admin authors an InstanceType referencing a catalog flavor by its `acceleratorGroup`,
with a unit spec of its own:

```yaml
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

The new row shows `12/128Gi/200Gi` against the derived `8/64Gi/100Gi`, and both siblings `1/1` — one
accelerator, two views of it.

Deploy an Instance onto the custom type, whole accelerator:

```yaml
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

Once `custom-demo` is `Ready`, both siblings drop to `0/0 0/0 0/0` — **consistent** across the
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

Deleting it retires gracefully: the operator drains the Instance (`HoldAndDrain`), the Instance stops,
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

Flipping `instance-type-aware-cpu-manufacturer` on makes the **aggregation layer** split every pool by
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

$ kubectl get instancetypes
NAME                                               ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64   gpustack-fnv64-029fd9550e0c70bd   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
gpustack--nvidia-a10g-linux-amd64                  gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
```

`custom-demo` goes `Ready` on the aware pool, which drops to `0/0 0/0 0/0`, and the collapsed
`gpustack--nvidia-a10g-linux-amd64` **also** drops — one accelerator, two consistent views.

The aware ClusterQueue's labels now **carry the CPU key**, the difference from section 1:

```yaml
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

The **`ResourceFlavor` set is byte-for-byte unchanged**, the same 7 as section 1 — the flip re-grouped
only the aggregation layer:

```console
$ kubectl get resourceflavor --no-headers | wc -l
7
```

Turning it back off collapses the layer again, still without touching a flavor.

---

## 6. Pinning an Instance to a node, and mounting more than the workspace

An Instance normally lets the scheduler pick any node its pool covers. `spec.nodeName` narrows that to
one, and `spec.additionalVolumes` mounts paths beside the workspace — a shared dataset, one ConfigMap
key, a directory on the node itself:

```yaml
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
- **Both new fields are immutable while the Instance runs**, editable while stopped — the rule the rest
  of `spec` follows.

Two cross the host boundary, so each has its own administrator Setting, both `false` by default and
kept separate so node-path mounts can be allowed without a container escape:

| Setting | Gates |
|---|---|
| `instance-privileged-allowed` | `spec.privileged` — escapes the container boundary, exposing the node's devices and kernel surface. |
| `instance-host-path-volume-allowed` | `spec.additionalVolumes[*].hostPath` — reaches the node's filesystem, but not its devices or kernel. |

Each gates **taking** its escape, so turning one off stops new grants without stranding an Instance
that already holds one; [Settings](./settings.md#online-adjustable-settings) has the exact terms.

Both govern the **node** boundary, not the namespace one: a `persistent`, `configMap` or `secret`
source names an object in the Instance's own namespace and may always be mounted, the same reach a Pod
there has. Namespaces stay the tenancy boundary — put Instances whose authors should not read each
other's Secrets in their own.

---

**See also** — [Accelerator Requests](./accelerator-requests.md) (the contract behind step 3) ·
[NVIDIA MIG Operations](./operation/nvidia-mig.md#walkthrough-three-mig-configurations-on-one-node)
(hardware partitioning) · [Settings](./settings.md#online-adjustable-settings) (the two gates)

**Next** → [NVIDIA MIG Operations](./operation/nvidia-mig.md) — hardware partitioning, from enabling
the mode to reclaiming the instance.
