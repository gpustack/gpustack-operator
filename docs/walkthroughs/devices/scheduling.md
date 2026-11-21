# Heterogeneous Devices Walkthrough

Inspect accelerator scheduling on a four-node cluster through captured `kubectl` commands and output.
Read pool objects, remove and re-admit a node, request a logical slice, author a custom pool, enable
CPU awareness, and pin a workload to one node.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Read the initial pools](#step-1-read-the-initial-pools)
- [Step 2: Remove a managed node](#step-2-remove-a-managed-node)
- [Step 3: Request a logical slice](#step-3-request-a-logical-slice)
- [Step 4: Manage a custom InstanceType](#step-4-manage-a-custom-instancetype)
- [Step 5: Enable CPU awareness](#step-5-enable-cpu-awareness)
- [Step 6: Instance pinning and volumes](#step-6-instance-pinning-and-volumes)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

NFD labels the nodes, and the Device Manager updates each `Devices` ledger. The worker derives Kueue
`ResourceFlavor`s. The operator creates the aggregated `InstanceType` and `ClusterQueue` pools that
admit `Instance` `Pod`s.

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `InstanceType` | cluster | administrator | the schedulable pool; Step 4 authors a custom one beside the operator's derived pools |
| `Instance` | namespace | user | the workload admitted against a pool |
| `NodeFeature` | namespace | the worker | the per-node `managed` label whose flip removes a node from its pool (Step 2) |

### Reading the captured output

Object YAML retains *`metadata.labels`*, *`spec`* and *`status`*. Each operation includes
before-and-after `kubectl get instancetypes` output. `Node` names are genericized as `node-cpu`,
`node-a10g`, `node-t4-a` and `node-t4-b`.

Watch three columns: **UNIT(CPU/RAM)/STORAGE**, the per-unit request the `InstanceType` charges; **CPU**, the collapsed
CPU pool's `remaining/capacity` cores; **ACCELERATOR(EX/SH/SL/PT)**, the `onceMaxRequest/remaining` of each
projection (exclusive whole cards, shared slots, logical slices, and physical partitions). This run predates the shared card-count
reading, so its `SH` once-max figures are shares, not accelerators ([Pre-release breaks](../../modules/devices/requests.md#pre-release-breaks)).

Each accelerator counts in exactly one of `EX`/`SH`/`SL` (unpartitioned) or `PT` (partitioned), so `0/0`
under `PT` throughout means none is in a partitioning mode. For the all-partitioned and **mixed** configurations see the
[three-configuration walkthrough](../../modules/devices/nvidia-mig.md#three-mig-configurations).

## Environment

The captured run used this cluster, settings and hardware:

| Item | Value |
|---|---|
| Cluster | four `linux/amd64` nodes, all operator-managed |
| Settings | the defaults: `instance-type-derived-from-node=true` auto-derives the pool objects, and `instance-type-aware-cpu-manufacturer=false` keeps the CPU manufacturer out of the aggregation layer until Step 5 flips it on |

| `Node` | CPU (`gKey`) | Cores | Accelerator (`aKey`) |
|---|---|---|---|
| `node-cpu` | `amd-epyc-7r13` | 16 | — |
| `node-a10g` | `amd-epyc-7r32` | 4 | 1 × NVIDIA A10G (`nvidia-a10g`) |
| `node-t4-a` | `intel-xeon-platinum-8259cl` | 4 | 1 × NVIDIA Tesla T4 (`nvidia-tesla-t4`) |
| `node-t4-b` | `intel-xeon-platinum-8259cl` | 48 | 4 × NVIDIA Tesla T4 (`nvidia-tesla-t4`) |

## Step 1: Read the initial pools

With `instance-type-derived-from-node` on, the operator materializes the finest-grain `ResourceFlavor`s
and one collapsed pool per accelerator plus a generic CPU pool:

```bash
kubectl get resourceflavor
```

Captured output:

```text
NAME                                                                   AGE
gpustack--amd-epyc-7r13-linux-amd64-16c                                5h19m
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d                    9m26s
gpustack--amd-epyc-7r32-linux-amd64-4c                                 9m26s
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4-linux-amd64-1d   5h19m
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4-linux-amd64-4d   5h19m
gpustack--intel-xeon-platinum-8259cl-linux-amd64-48c                   5h19m
gpustack--intel-xeon-platinum-8259cl-linux-amd64-4c                    5h19m
```

```bash
kubectl get clusterqueue
```

Captured output:

```text
NAME                                    COHORT   PENDING WORKLOADS
gpustack--generic-linux-amd64                    0
gpustack--nvidia-a10g-linux-amd64                0
gpustack--nvidia-tesla-t4-linux-amd64            0
```

```bash
kubectl get instancetype
```

Captured output:

```text
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

```bash
kubectl get instancetypeflavor
```

Captured output:

```text
NAME                        GENERALGROUP   ACCELERATORGROUP   ACCELERATABLE   MANUFACTURER   PRODUCT       MEMORY   CORES   SLICEABLE
gpustack--generic           generic                          false           generic
gpustack--nvidia-a10g                      nvidia-a10g        true            nvidia         NVIDIA-A10G   24Gi     10240   true
gpustack--nvidia-tesla-t4                  nvidia-tesla-t4    true            nvidia         Tesla-T4      16Gi     2560    true
```

```bash
kubectl get devices
```

Captured output:

```text
NAME
node-t4-a
node-a10g
node-t4-b
```

- **7 `ResourceFlavor`s**, one per `(gKey, [aKey,] os, arch, count)` — `node-t4-b` (48 cores, 4×T4)
  yields both `…-48c` and `…--nvidia-tesla-t4-…-4d`.
- **3 `ClusterQueue`s / `InstanceType`s**, collapsed — **A10G** `1/1 10/10 100/100 0/0` (1 accelerator),
  **T4** `4/5 40/50 100/500 0/0` (5 = `node-t4-a`'s 1 + `node-t4-b`'s 4, none partitioned).
- **No `Devices` for `node-cpu`** — it carries no accelerator.

Inspect each generated layer on `node-a10g` with `kubectl get <kind> <name> -o yaml`:

| Kind | Object on `node-a10g` | Key fields |
|---|---|---|
| `Node` | `node-a10g` | `gpustack.ai/managed: "true"`, `general.feature.gpustack.ai/amd-epyc-7r32*`, `acceleratable.feature.gpustack.ai/nvidia-a10g*` |
| `Devices` | `node-a10g` | `status.groups[].accelerators[]` with `remaining: 1600000` credits per card |
| `ResourceFlavor` | `gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d` | `feature.gpustack.ai/acceleratable: "true"`, `spec.nodeLabels` pinning managed nodes |
| `ClusterQueue` | `gpustack--nvidia-a10g-linux-amd64` | `admissionChecks: [gpustack-node-devices]`, `credits.gpustack.ai/nvidia: 1600k` |
| `InstanceType` | `gpustack--nvidia-a10g-linux-amd64` | `spec.unitResources` (`8` CPU / `64Gi` RAM preset), `status` four-view (`EX`/`SH`/`SL`/`PT`) |
| `InstanceTypeFlavor` | `gpustack--nvidia-a10g` | OS/arch-agnostic catalog entry carrying `product`, `memory`, `cores`, `sliceable` |

## Step 2: Remove a managed node

A node stays in a pool only while its worker `NodeFeature` (`gpustack-system/<node>-gpustack-worker`)
publishes `gpustack.ai/managed=true`. Flip that label to `"false"` on `node-a10g`:

```bash
kubectl -n gpustack-system patch nodefeature node-a10g-gpustack-worker \
    --type=merge -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'
kubectl get instancetypes
```

Captured output:

```text
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/68   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The A10G pool drops to `0/0 0/0 0/0 0/0` (its `ResourceFlavor` is deleted), and generic **CPU**
moves from `48/72` to `48/68` as `node-a10g`'s 4 cores leave.

Re-admit `node-a10g` by patching `gpustack.ai/managed` back to `"true"`; one reconcile rebuilds the
flavor and restores A10G to `1/1 10/10 100/100 0/0` and generic CPU to `48/72`:

```bash
kubectl -n gpustack-system patch nodefeature node-a10g-gpustack-worker \
    --type=merge -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}'
kubectl get instancetypes
```

## Step 3: Request a logical slice

A sliceable `InstanceType` — the A10G reports logical slicing in its status detail — admits
fractional-accelerator workloads. Request 20% of an accelerator's VRAM with
*`acceleratorSlicedMemoryPercentage`*:

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
    acceleratorSlicedMemoryPercentage: 20  # 20% of the accelerator's VRAM
    acceleratorSlicedCoresPercentage: 100
  volume:
    ephemeral:
      capacity: 1Gi
```

**Before**, the A10G shows `1/1 10/10 100/100`. Apply it and wait for `Ready`:

```bash
kubectl apply -f sliced-demo.yaml
kubectl get instancetypes
```

Captured output:

```text
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 80/80 0/0          0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The A10G row moves `1/1 10/10 100/100 0/0` → `0/0 0/0 80/80 0/0`: **SL** gives up the 20% slice;
**EX** and **SH** fall to `0/0` because a partly-sliced accelerator is neither whole nor a shared
unit; **PT** stays `0/0`, since no partitioning mode means no hardware partition.

Inside the `Instance`, the logical-slicing runtime caps visible VRAM to the slice, ≈ 20% of 24 GiB:

```bash
kubectl exec sliced-demo -- nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
```

Captured output:

```text
NVIDIA A10G, 4912 MiB
```

Deleting the `Instance` releases the slice: the row returns to `1/1 10/10 100/100 0/0`.

> **Physical partitioning (MIG).** The A10G slices logically: a runtime caps a shared accelerator, and
> the `SL` view above tracks the per-accelerator credit budget. A MIG-capable accelerator (A100 / H100)
> instead hard-partitions into fixed hardware instances the operator materializes on demand:
>
> - a different resource family (`.partitioned*`, reported under `PT`), with a different request shape —
>   keys and rules in [Accelerator Requests](../../modules/devices/requests.md);
> - MIG *mode* is the administrator's, driven with `nvidia-smi`, so it has its own runbook and a recorded
>   enable → request → reclaim → disable walkthrough in
>   [NVIDIA MIG Operations](../../modules/devices/nvidia-mig.md).

## Step 4: Manage a custom InstanceType

The custom workspace type shares its canonical pool queue with the derived `AcceleratorType` and
other `InstanceType`s for the same pool. The output below records an earlier run with separate
queue entrances; current siblings publish the same canonical pool entrance.

A derived pool is sized from a **per-product preset**: 8 CPU / 64 GiB for this A10G, 4 CPU / 16 GiB for
an unrecognised accelerator ([Unit resource sizing](../../modules/instances/_index.md#unit-resource-sizing)).
`spec.unitResources` defaults omitted `cpu`/`ram` and caps explicit requests.

With `instance-general-resources-overcommit` enabled (default), scheduler requests use `100m` CPU per core
and `128Mi` RAM per GiB (disabled: request equals limit). Because `spec.unitResources` is immutable at
creation, existing derived types keep their sizing across operator upgrades.

To resize without stopping active `Instance`s or losing the pool, author a custom sibling `InstanceType`
referencing the catalog flavor by its *`acceleratorGroup`* rather than deleting and letting the
operator recreate the derived type:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: a10g-12c128g
spec:
  acceleratable: true
  acceleratorGroup: nvidia-a10g  # references the gpustack--nvidia-a10g catalog flavor
  os: linux
  arch: amd64
  unitResources:
    cpu: "12"
    ram: 128Gi
  localStorage: 200Gi
```

Apply it: the defaulting webhook enriches its descriptors from the matching flavor, and it lands as a
**sibling** of the derived A10G pool, on the one physical accelerator:

```bash
kubectl apply -f a10g-12c128g.yaml
kubectl get instancetypes
```

Captured output:

```text
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
a10g-12c128g                            gpustack-fnv64-8cf5b3114035c84a   12/128Gi/200Gi          1/1 10/10 100/100 0/0      0/0     Active
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

The new row shows `12/128Gi/200Gi` against the derived `8/64Gi/100Gi`, and both siblings `1/1`: one
accelerator, two views of it.

Deploy an `Instance` onto the custom type, whole accelerator:

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

```bash
kubectl apply -f custom-demo.yaml
kubectl get instancetypes
```

Captured output:

```text
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
a10g-12c128g                            gpustack-fnv64-8cf5b3114035c84a   12/128Gi/200Gi          0/0 0/0 0/0 0/0            0/0     Active
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

Deleting the type stops its `Instance`. The shared `ClusterQueue` remains while another type fronts
the pool:

```bash
kubectl delete instancetype a10g-12c128g
```

Captured output:

```text
instancetype.worker.gpustack.ai "a10g-12c128g" deleted
```

```bash
kubectl -n default get instance custom-demo -o jsonpath='{.status.phase}'
```

Captured output:

```text
Stopped
```

```bash
kubectl get instancetypes
```

Captured output:

```text
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

`a10g-12c128g` is gone, `custom-demo` is `Stopped` (kept for Step 5), and the released accelerator returns the derived pool to `1/1 10/10 100/100`.

## Step 5: Enable CPU awareness

Flipping `instance-type-aware-cpu-manufacturer` on makes the aggregation layer split every pool by
the CPU key. `ResourceFlavor`s are never rewritten; only queues, types and catalog re-group.

```bash
kubectl -n gpustack-system patch secret gpustack-settings --type=merge \
    -p '{"data":{"instance-type-aware-cpu-manufacturer":"'"$(printf true | base64)"'"}}'
kubectl -n gpustack-system rollout restart deploy/gpustack-operator-worker
```

**Before**, 3 `InstanceType`s; **after** the re-derive, CPU-aware types appear (create-only)
**alongside** the old collapsed ones:

```bash
kubectl get instancetypes
```

Captured output:

```text
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

> **Cleanup hint.** An admin may delete the stale ones: `kubectl delete instancetype gpustack--generic-linux-amd64 gpustack--nvidia-a10g-linux-amd64 gpustack--nvidia-tesla-t4-linux-amd64`.

Re-purpose the `custom-demo` `Instance` stopped in Step 4: a drained `Instance` carries operator-set
`spec.stop: true`, so repoint its `type` at a CPU-aware pool and clear the stop:

```bash
kubectl -n default patch instance custom-demo --type=merge \
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

```bash
kubectl get instancetypes
```

Captured output:

```text
NAME                                               ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64   gpustack-fnv64-029fd9550e0c70bd   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
gpustack--nvidia-a10g-linux-amd64                  gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
```

`custom-demo` goes `Ready` on the aware pool, which drops to `0/0 0/0 0/0`, and the collapsed
`gpustack--nvidia-a10g-linux-amd64` also drops: one accelerator, two consistent views.

The aware `ClusterQueue` (`gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64`) carries
`general.feature.gpustack.ai/amd-epyc-7r32: "true"`, and `kubectl get instancetypeflavor` now lists
five per-CPU and per-`(gKey,aKey)` rows in place of the collapsed three. The `ResourceFlavor` set
stays byte-for-byte unchanged (`7` objects):

```bash
kubectl get resourceflavor --no-headers | wc -l
```

Captured output:

```text
7
```

Turning CPU awareness back off collapses the aggregation layer again without touching a flavor.

## Step 6: Instance pinning and volumes

An `Instance` normally lets the scheduler pick any node its pool covers. *`spec.nodeName`* narrows that to
one, and *`spec.additionalVolumes`* mounts paths beside the workspace — a shared dataset, one `ConfigMap`
key, a directory on the node itself:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: pinned-demo
  namespace: default
spec:
  type: gpustack--nvidia-tesla-t4-linux-amd64
  nodeName: node-t4-b  # pin to this node
  image: ubuntu:24.04
  command:
    - sleep
    - "86400"
  resources:
    accelerator: "1"
  volume:
    ephemeral:
      capacity: 1Gi
  volumeMount: /workspace  # container workspace
  additionalVolumes:
    - mountPath: /mnt/datasets  # a shared dataset, read-write
      persistent:
        name: datasets
    - mountPath: /etc/model/config.json  # one ConfigMap key, as a file
      subPath: config.json
      readOnly: true
      configMap:
        name: model-config
    - mountPath: /mnt/host-cache  # a directory on node-t4-b itself
      readOnly: true
      hostPath:
        path: /var/lib/gpustack-cache
        type: DirectoryOrCreate
```

- **The pin is a *`nodeSelector`*, never a direct assignment.** The `Pod` gets one selector entry,
  `kubernetes.io/hostname: <the node's own hostname label>`, read from the `Node` because a provider may
  set it to something other than the `Node`'s name. *`pod.spec.nodeName`* stays with the scheduler, so the
  `Pod` still queues through Kueue's `ClusterQueue` quota and the `node-devices` `AdmissionCheck`'s
  per-accelerator feasibility gate; an unsatisfiable pin goes Pending with the scheduler's own reason
  instead of running elsewhere.
- **The node only has to exist**, checked at *creation*. It need not be managed by the operator nor
  belong to the pinned type's pool: an accelerator-less `Instance` that only downloads a model must
  still land on a specific accelerated node.
- **Pool membership stays the scheduler's business.** A `type`'s pool and a pin's node are decided
  independently, so a pin into a heterogeneous pool can be admitted by Kueue and then stay Pending
  because the chosen flavor's labels do not match that node.
- **Each additional volume needs an absolute, canonical *`mountPath`*** duplicating neither another
  entry's path nor *`spec.volumeMount`*, and **exactly one** source: `persistent` (an
  `InstancePersistentVolume` in the same namespace), *`configMap`*, `secret` or *`hostPath`*. *`readOnly`*
  and *`subPath`* behave as on any `Pod` volume mount.
- **They mount into the workload container only.** The SSH sidecar needs no change: it enters that
  container's mount namespace per session, so every mount is visible over SSH too.
- **A persistent claim places the `Instance` where it can attach.** The workspace claim and every
  `persistent` entry follow the [claim placement rules](../../modules/model-delivery/artifact.md#claim-delivery-and-placement):
  a node-local volume pins the `Instance` to its node and that node's capacity; network or `ReadWriteMany`
  storage avoids the pin. A claim nothing binds creates no `Pod` and says why in *`status.phaseMessage`*;
  `instance-persistent-volume-placement` turns this off.
- **Both new fields are immutable while the `Instance` runs**, editable while stopped — the rule the rest
  of *`spec`* follows.

Two cross the host boundary, so each has its own administrator `Setting`, both `false` by default and
kept separate so node-path mounts can be allowed without a container escape:

| `Setting` | Gates |
|---|---|
| `instance-privileged-allowed` | *`spec.privileged`* — escapes the container boundary, exposing the node's devices and kernel surface. |
| `instance-host-path-volume-allowed` | *`spec.additionalVolumes[*].hostPath`* — reaches the node's filesystem, but not its devices or kernel. |

Each gates taking its escape, so turning one off stops new grants without stranding an `Instance`
that already holds one.

Both govern the **node** boundary, not the namespace one: a `persistent`, *`configMap`* or `secret`
source names an object in the `Instance`'s own namespace and may always be mounted, the same reach a `Pod`
there has. `Namespace`s stay the tenancy boundary: put `Instance`s whose authors should not read each
other's `Secret`s in their own.

## Limitations

This recorded session uses four unpartitioned nodes (`PT` reads `0/0` throughout); see the
[three-configuration walkthrough](../../modules/devices/nvidia-mig.md#three-mig-configurations)
for hardware partitioning.

Pool isolation (`ClusterQueue` without cross-pool borrowing), queue inspection, and Pending diagnosis
are covered in [Pools and diagnosis](../../modules/devices/requests.md#pools-and-diagnosis).

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| An `Instance` does not become Ready and its `Workload` stays pending | Quota, flavor labels or a node pin the scheduler cannot satisfy | Read `kubectl -n <namespace> get instance <name> -o jsonpath='{.status.phaseMessage}'`, then `kubectl -n <namespace> get workloads`. Compare the type's remaining counts in `kubectl get instancetypes`. See [Step 6](#step-6-instance-pinning-and-volumes) for pins |
| `Pod` rejected: `a Pod may request only one accelerator family, found [...]` | Two accelerator families in one `Pod` | Request one family. See [rule 1](../../modules/devices/requests.md#rule-1-family-and-container-group) |
| `Pod` rejected: `at most one container may request a slicing family, found N` | More than one container requests a slice or a partition | Keep one claiming container. See [rule 6](../../modules/devices/requests.md#rule-6-slicing-container-count) |
| `Pod` rejected: `a restartable init container (a native sidecar) may not request an accelerator` | A native sidecar carries the accelerator request | Move the request to an app container. See [rule 7](../../modules/devices/requests.md#rule-7-native-sidecar-requests) |
| A node's accelerators are missing from the `InstanceType` pool | The node's `NodeFeature` carries `gpustack.ai/managed: "false"`, or `node-management-manual` is `true` and no one labeled the node | Run `kubectl -n gpustack-system get nodefeature <node>-gpustack-worker -o yaml` and read *`spec.labels`*. See [Step 2](#step-2-remove-a-managed-node) |
| `Instance` rejected: `privileged mode is not allowed: enable the "instance-privileged-allowed" setting to allow it` | The `instance-privileged-allowed` setting is `false` | Allow it only if the node boundary is acceptable; update the `Setting` as shown below |

If the privileged-mode gate is disabled, the mitigation above changes it as follows:

```diff
 apiVersion: gpustack.ai/v1
 kind: Setting
 metadata:
   name: instance-privileged-allowed
   namespace: gpustack-system
 spec:
-  value: "false"
+  value: "true"
```

---

**See also** — [Accelerator Requests](../../modules/devices/requests.md) (the contract behind Step 3) · [NVIDIA MIG Operations](../../modules/devices/nvidia-mig.md#three-mig-configurations) (hardware partitioning) · [GPU Instances](../../modules/instances/_index.md) (unit resource sizing and metrics)

**Next** → [RDMA Networking Walkthrough](../rdma/network-endpoints.md) — request and verify RDMA endpoints with NUMA alignment.
