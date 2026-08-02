# Walkthrough

> **Purpose** — the whole scheduling chain on a real four-node cluster: every materialized object as
> real YAML, and a before/after for each operation.
> **Audience** everyone · **Prerequisites** [Architecture](./architecture.md) · **Read time** ~20 min

A recorded run of the scheduling chain on a live Kubernetes cluster, referenced from the
[architecture](./architecture.md). Every command is the real `kubectl` invocation and
its real output; every object is shown as YAML (trimmed to the relevant `metadata.labels` / `spec` /
`status`); every operation shows a **before / after** comparison via `kubectl get instancetypes`.

Node names are genericized (`node-cpu`, `node-a10g`, `node-t4-a`, `node-t4-b`). The run uses the
defaults — `instance-type-derived-from-node=true` (the operator auto-derives the pool objects) and
`instance-type-aware-cpu-manufacturer=false` (the aggregation layer ignores the CPU manufacturer; the
last section flips this on).

The `kubectl get instancetypes` columns used throughout:

- **UNIT(CPU/RAM)/STORAGE** — the per-unit request the InstanceType charges.
- **ACCELERATOR(EX/SH/SL/PT)** — the four-view `onceMaxRequest/remaining` for **EX**clusive (whole
  cards), **SH**ared (ownership units), **SL**iced (logical, per-card VRAM-percent budget), and
  **PT** — physically **P**ar**T**itioned (hardware partition instances, e.g. NVIDIA MIG). Every card
  feeds exactly one side of the split: `EX`/`SH`/`SL` count unpartitioned cards, `PT` partitioned ones,
  so the `0/0` in the `PT` group throughout this run means "no card here is in a partitioning mode".
  For the other two configurations — every card partitioned, and a **mixed** node serving both families
  at once — see the [three-configuration walkthrough](./operation/nvidia-mig.md#walkthrough-three-mig-configurations-on-one-node).
- **CPU** — the collapsed CPU pool's `remaining/capacity` cores.

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

The operator materializes the finest-grain `ResourceFlavor`s and, because
`instance-type-derived-from-node` is on, one collapsed pool per accelerator plus one generic CPU pool.

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

Reading the initial state:

- **7 ResourceFlavors** — one per `(gKey, [aKey,] os, arch, count)`; e.g. `node-t4-b` (48 cores, 4×T4)
  yields both `…-48c` and `…--nvidia-tesla-t4-…-4d`.
- **3 ClusterQueues / InstanceTypes** — collapsed: one generic CPU pool + one per accelerator.
- **A10G InstanceType** shows `1/1 10/10 100/100 0/0` (1 whole card, 10 shared units, 100 % logical
  slice budget, no partitioned card) and the **T4** shows `4/5 40/50 100/500 0/0` (5 cards total across `node-t4-a`'s 1 + `node-t4-b`'s 4).
- **`node-cpu` has no `Devices`** object — it carries no accelerator.

Below is one representative of each kind, keyed off the A10G node.

### Node

Labeled by two NodeFeatures — `node-a10g-gpustack-worker` (the `general.*` CPU keys + `managed`) and
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

Cluster-scoped, named after the node. The worker stamps `gpustack.ai/managed` + the real CPU key; the
Device Manager stamps the accelerator key and reports the per-card ledger in `.status`:

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
          remaining: 1600000        # per-card credit ledger the AdmissionCheck reads
```

### ResourceFlavor

The finest, setting-independent grain: `gpustack--${gKey}[--${aKey}]-${os}-${arch}-${count}{c|d}`. An
**accelerated** flavor carries the `feature.gpustack.ai/acceleratable=true` discriminator and both the
CPU (`general.`) and device (`acceleratable.`) keys, and pins its nodes via `spec.nodeLabels`:

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
    acceleratable.feature.gpustack.ai/nvidia-a10g.count: "1"       # per-node card count
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

A **non-accelerated** (CPU) flavor carries `feature.gpustack.ai/acceleratable=false`, and its capacity
is the node's CPU-core count:

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

One isolated pool per accelerator. Its labels mirror the flavor discriminators (**no** `general.` key
while awareness is off, so it aggregates the A10G across every CPU). It covers the manufacturer's
`credits` resource and gates admission with the per-card `AdmissionCheck`:

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

The schedulable pool. Its labels are stamped by the defaulting webhook (the schedule discriminators +
`derived-from-node` provenance + the fronting `queue-entrance`); its `spec` descriptors are enriched
from the matching flavor; its `status` is the reconciled four-view:

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
  acceleratorPartitioned:          # no card here is in a partitioning mode
    capacity: "0"
    onceMaxRequest: "0"
    remaining: "0"
  entrance: gpustack-fnv64-c4680bb149644f1c
```

### InstanceTypeFlavor

An os/arch-agnostic catalog view, aggregated read-only from the flavors — it carries **no
`metadata.labels`**; its grouping identity lives in `spec`:

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

A node participates in a pool only while it carries `gpustack.ai/managed=true` (required by the
flavor's `spec.nodeLabels`). That label is stamped through the node's worker NodeFeature.

**Before** — `node-a10g` managed:

```console
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

Take `node-a10g` out of management by flipping the label on its worker NodeFeature:

```console
$ kubectl -n gpustack-system patch nodefeature node-a10g-gpustack-worker \
    --type=merge -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'
nodefeature.nfd.k8s-sigs.io/node-a10g-gpustack-worker patched
```

**After** — NFD propagates the label to the node; the operator retires the now-nodeless A10G flavor:

```console
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/68   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The comparison:

- The A10G row drops `1/1 10/10 100/100` → `0/0 0/0 0/0` (its ResourceFlavor is deleted).
- The generic **CPU** capacity drops `48/72` → `48/68` — `node-a10g`'s 4 cores leave the CPU pool.

Re-admit it and the flavor is rebuilt and the counts restored within one reconcile:

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

A sliceable InstanceType (the A10G reports logical slicing in its observed status detail) admits fractional-card workloads. Request 20 % of
a card's VRAM with `acceleratorSlicedMemoryPercentage`:

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
    acceleratorSlicedMemoryPercentage: 20      # 20% of the card's VRAM
    acceleratorSlicedCoresPercentage: 100
  volume:
    ephemeral:
      capacity: 1Gi
```

**Before** the A10G shows `1/1 10/10 100/100`. Apply the Instance; once it is `Ready`:

```console
$ kubectl apply -f sliced-demo.yaml
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 80/80 0/0          0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The comparison — the A10G row moves `1/1 10/10 100/100 0/0` → `0/0 0/0 80/80 0/0`:

- **SL** (logical slice) drops `100 → 80` — the 20 % slice is taken.
- **EX** and **SH** drop to `0/0` — a partially-sliced card can no longer be handed out whole or as a
  shared unit.
- **PT** stays `0/0` — the card is not in a partitioning mode, so it serves no hardware partition.

Inside the Instance, the GPU's visible VRAM is capped to the slice — the logical-slicing runtime enforces
the budget (≈ 20 % of the card's 24 GiB):

```console
$ kubectl exec sliced-demo -- nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
NVIDIA A10G, 4912 MiB
```

Deleting the Instance releases the slice (the A10G row returns to `1/1 10/10 100/100 0/0`).

> **Physical partitioning (MIG).** The A10G slices *logically* — a runtime caps a shared card, and the
> `SL` view above tracks the per-card credit budget. A MIG-capable card (A100 / H100) instead
> **hard-partitions** into fixed hardware instances the operator materializes on demand, which is a
> different resource family (`.partitioned*`, reported under `PT`) and a different request shape. MIG
> *mode* is driven by the administrator with `nvidia-smi`, so it has its own runbook and a worked
> enable → request → reclaim → disable walkthrough (with real `kubectl` output at every step) in
> [NVIDIA MIG Operations](./operation/nvidia-mig.md); the request keys and rules for every family are in
> [Accelerator Requests](./accelerator-requests.md).

---

## 4. Managing a custom InstanceType

Beyond the auto-derived pools, an admin can author an InstanceType that references a catalog flavor (by
its `acceleratorGroup`) with a unit spec of its own. A derived pool is sized from a **per-product
preset** — this A10G pool got 8 CPU / 64 GiB, an unrecognised accelerator would get 4 CPU / 16 GiB (see
[Instance Type Unit Resources Preset Reference](./reference/instance-type-unit-resources.md)) — and an
admin who wants a different size authors their own type:

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

Apply it — the defaulting webhook enriches the descriptors from the matching flavor, and it appears as
a **sibling** of the auto-derived A10G pool (both feed off the one physical card):

```console
$ kubectl apply -f a10g-12c128g.yaml
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
a10g-12c128g                            gpustack-fnv64-8cf5b3114035c84a   12/128Gi/200Gi          1/1 10/10 100/100 0/0      0/0     Active
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

- The new row carries the admin's unit spec `12/128Gi/200Gi` (vs the derived preset `8/64Gi/100Gi`).
- Both A10G siblings show `1/1` — they share the same single card.

Deploy an Instance onto the custom type (whole card):

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

Once `custom-demo` is `Ready`, the consumption shows up **consistently on both siblings** (same card):

```console
$ kubectl apply -f custom-demo.yaml
$ kubectl get instancetypes
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
a10g-12c128g                            gpustack-fnv64-8cf5b3114035c84a   12/128Gi/200Gi          0/0 0/0 0/0 0/0            0/0     Active
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

Both `a10g-12c128g` and `gpustack--nvidia-a10g-linux-amd64` drop to `0/0 0/0 0/0`.

Delete the custom InstanceType — it retires gracefully: the operator drains its Instance
(`HoldAndDrain`), the Instance stops, and the type plus its ClusterQueue are removed:

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

- `a10g-12c128g` is gone; the `custom-demo` Instance is `Stopped` (kept for section 5).
- The shared card is released — `gpustack--nvidia-a10g-linux-amd64` returns to `1/1 10/10 100/100`.

---

## 5. Enabling CPU-manufacturer awareness

Flipping `instance-type-aware-cpu-manufacturer` on makes the **aggregation layer** split every pool by
the CPU key. `ResourceFlavor`s are never rewritten — only the queues/types/catalog re-group.

```console
$ kubectl -n gpustack-system patch secret gpustack-settings --type=merge \
    -p '{"data":{"instance-type-aware-cpu-manufacturer":"'"$(printf true | base64)"'"}}'
$ kubectl -n gpustack-system rollout restart deploy/gpustack-operator-worker
```

**Before** there are 3 InstanceTypes. **After** the re-derive, new CPU-aware types appear (create-only)
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

- New per-CPU pools split the generic 72 cores by manufacturer: `amd-epyc-7r13` = `16/16`,
  `amd-epyc-7r32` = `4/4`, `intel-xeon-platinum-8259cl` = `48/52`.
- New per-`(gKey,aKey)` accelerated pools: `…amd-epyc-7r32--nvidia-a10g…`,
  `…intel-xeon-platinum-8259cl--nvidia-tesla-t4…`.
- The old collapsed `gpustack--generic-…` / `gpustack--nvidia-a10g-…` / `gpustack--nvidia-tesla-t4-…`
  remain (create-only, not garbage-collected).

> **Cleanup hint.** An admin may delete the stale ones:
> `kubectl delete instancetype gpustack--generic-linux-amd64 gpustack--nvidia-a10g-linux-amd64 gpustack--nvidia-tesla-t4-linux-amd64`.

Re-purpose the `custom-demo` Instance stopped in section 4. A drained Instance carries `spec.stop: true`
(set by the operator), so repoint its `type` at a CPU-aware pool and clear the stop:

```console
$ kubectl -n default patch instance custom-demo --type=merge \
    -p '{"spec":{"type":"gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64","stop":false}}'

$ kubectl get instancetypes
NAME                                               ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64   gpustack-fnv64-029fd9550e0c70bd   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
gpustack--nvidia-a10g-linux-amd64                  gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
```

- `custom-demo` becomes `Ready` on the aware pool `gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64`,
  which drops to `0/0 0/0 0/0`.
- The old collapsed `gpustack--nvidia-a10g-linux-amd64` **also** drops to `0/0` — the same physical card
  is consumed, and both views of it stay consistent.

The aware ClusterQueue's labels now **carry the CPU key** (the difference from section 1):

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

The InstanceTypeFlavor catalog re-groups to per-`(gKey,aKey)` + per-`gKey` (the collapsed
`gpustack--generic` / per-accelerator rows are replaced):

```console
$ kubectl get instancetypeflavor
NAME                                                    GENERALGROUP                 ACCELERATORGROUP   ACCELERATABLE   MANUFACTURER   PRODUCT                                          MEMORY   CORES   SLICEABLE
gpustack--amd-epyc-7r13                                 amd-epyc-7r13                                   false           amd            AMD EPYC 7R13 Processor
gpustack--amd-epyc-7r32                                 amd-epyc-7r32                                   false           amd            AMD EPYC 7R32
gpustack--intel-xeon-platinum-8259cl                    intel-xeon-platinum-8259cl                      false           intel          Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz
gpustack--amd-epyc-7r32--nvidia-a10g                    amd-epyc-7r32                nvidia-a10g        true            nvidia         NVIDIA-A10G                                      24Gi     10240   true
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4   intel-xeon-platinum-8259cl   nvidia-tesla-t4    true            nvidia         Tesla-T4                                         16Gi     2560    true
```

And the **`ResourceFlavor` set is byte-for-byte unchanged** — still the same 7 flavors from section 1;
the flip only re-grouped the aggregation layer:

```console
$ kubectl get resourceflavor --no-headers | wc -l
7
```

Turning the setting back off collapses the aggregation layer again, still without touching a flavor.

---

## 6. Pinning an Instance to a node, and mounting more than the workspace

An Instance normally lets the scheduler pick any node its pool covers. `spec.nodeName` narrows that to
one node, and `spec.additionalVolumes` mounts paths beside the workspace — here a shared dataset, one
key of a ConfigMap, and a directory on the node itself:

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

- **The pin is a `nodeSelector`, never a direct assignment.** The backing Pod gets exactly one selector
  entry, `kubernetes.io/hostname: <the node's own hostname label>` — read from the Node, because a
  provider may set that label to something other than the Node's name. `pod.spec.nodeName` is left to
  the scheduler, so the Pod still queues through Kueue and its `ClusterQueue` quota, and the
  `node-devices` AdmissionCheck still gates per-card feasibility. A pin that cannot be satisfied
  therefore surfaces as a Pending Pod with the scheduler's own reason, not as a Pod running somewhere
  else.
- **The node only has to exist.** It is checked when the Instance is *created*, and nothing more is
  required of it: it need not be managed by the operator, nor belong to the pinned type's pool. That is
  deliberate — a card-less Instance that only downloads a model must still be able to land on a
  specific accelerated node.
- **Pool membership is still the scheduler's business.** The pool a `type` covers and the node a pin
  names are decided independently, so pinning into a heterogeneous pool can be admitted by Kueue and
  then stay Pending because the chosen flavor's labels do not match that node.
- **Each additional volume needs an absolute, canonical `mountPath`** that duplicates neither another
  entry's path nor `spec.volumeMount`, and **exactly one** source: `persistent` (an
  `InstancePersistentVolume` in the same namespace), `configMap`, `secret`, or `hostPath`. `readOnly`
  and `subPath` behave as they do on any Pod volume mount.
- **They are mounted into the workload container only.** The SSH sidecar needs no change: it enters the
  workload container's mount namespace per session, so every additional mount is visible over SSH too.
- **Both new fields are immutable while the Instance is running**, and editable while it is stopped —
  the same rule the rest of `spec` follows.

Two of these cross the host boundary, so each is gated by its own administrator Setting, both
defaulting to `false`:

| Setting | Gates |
|---|---|
| `instance-privileged-allowed` | `spec.privileged` — escapes the container boundary, exposing the node's devices and kernel surface. |
| `instance-host-path-volume-allowed` | `spec.additionalVolumes[*].hostPath` — reaches the node's filesystem, but not its devices or kernel. |

They are kept separate so an administrator can allow node-path mounts without allowing a container
escape. Each gates the act of **taking** its escape — on creation, and on any later change that adds
one — while an Instance that already holds one keeps it: turning a gate off stops new grants without
stranding what was granted while it was on. See
[Settings](./settings.md#online-adjustable-settings) for how to change one.

The two gates govern the **node** boundary, not the namespace one. A `persistent`, `configMap` or
`secret` source names an object in the Instance's own namespace, and any of them may be mounted — the
same reach a Pod created directly in that namespace has. Namespaces remain the tenancy boundary; put
Instances whose authors should not read each other's Secrets in namespaces of their own.

---

**See also** — [Accelerator Requests](./accelerator-requests.md) (the request contract behind step 3) ·
[NVIDIA MIG Operations](./operation/nvidia-mig.md#walkthrough-three-mig-configurations-on-one-node)
(the same treatment for hardware partitioning) · [Settings](./settings.md#online-adjustable-settings)

**Next** → [NVIDIA MIG Operations](./operation/nvidia-mig.md) — the same treatment for hardware
partitioning, from enabling the mode to reclaiming the instance.
