# Heterogeneous Devices Walkthrough

Inspect accelerator pool derivation on a four-node cluster through recorded `kubectl` commands and
output. Read derived pools, remove and re-admit a node, allocate a logical VRAM slice, create a
custom sibling `InstanceType`, enable CPU-manufacturer splitting, and pin an `Instance` with extra
volumes.

## Contents

- [Summary](#summary)
- [Pool discovery](#pool-discovery)
- [Instance configuration](#instance-configuration)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

NFD publishes hardware feature labels, Device Manager maintains each node's `Devices` ledger, and the
worker materializes Kueue `ResourceFlavor`s, `ClusterQueue`s, `AcceleratorType`s, and `InstanceType`s:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `InstanceType` | cluster | operator or administrator | Schedulable workspace pool; Step 4 adds a custom sibling beside derived pools |
| `Instance` | namespace | user | Interactive workload admitted against a target `InstanceType` |
| `NodeFeature` | namespace | worker | Per-node `<node>-gpustack-worker` record carrying `gpustack.ai/managed` |

### Captured pool fields

Captured `kubectl get instancetypes` excerpts genericize node names as `node-cpu`, `node-a10g`,
`node-t4-a`, and `node-t4-b`. Watch three columns across each step:

- **`UNIT(CPU/RAM)/STORAGE`**: per-unit CPU, RAM, and storage defaults/caps for the `InstanceType`.
- **`CPU`**: `remaining/capacity` cores in CPU-only pools.
- **`ACCELERATOR(EX/SH/SL/PT)`**: `onceMaxRequest/remaining` for exclusive whole cards (`EX`), shared
  slots (`SH`)[^1], logical slices (`SL`), and hardware partitions (`PT`).

[^1]: This capture predates shared card-count reporting, so `SH` shows share slots rather than whole-card counts ([Legacy request migration](/gpustack-operator/main/docs/modules/devices/requests/index.md#legacy-request-migration)).

Each accelerator counts in either `EX`/`SH`/`SL` (unpartitioned) or `PT` (partitioned). Because none
of these four nodes enables MIG, `PT` stays `0/0` throughout; see
[MIG capacity scenarios](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#mig-capacity-scenarios) for
partitioned and mixed runs.

### Environment

| Item | Value |
|---|---|
| Cluster | Four operator-managed `linux/amd64` nodes |
| Settings | Defaults: `instance-type-derived-from-node=true` and `instance-type-aware-cpu-manufacturer=false` (toggled in Step 5) |

| `Node` | CPU (`gKey`) | Cores | Accelerator (`aKey`) |
|---|---|---|---|
| `node-cpu` | `amd-epyc-7r13` | 16 | — |
| `node-a10g` | `amd-epyc-7r32` | 4 | 1 × NVIDIA A10G (`nvidia-a10g`) |
| `node-t4-a` | `intel-xeon-platinum-8259cl` | 4 | 1 × NVIDIA Tesla T4 (`nvidia-tesla-t4`) |
| `node-t4-b` | `intel-xeon-platinum-8259cl` | 48 | 4 × NVIDIA Tesla T4 (`nvidia-tesla-t4`) |

## Pool discovery

### Step 1: Inspect derived pools

With `instance-type-derived-from-node` enabled, list the generated Kueue flavors, queues, types, and
`Devices` ledgers:

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

Verify three relationships in the output:

- **Seven `ResourceFlavor`s**: one per `(gKey, [aKey,] os, arch, count)` tuple, so `node-t4-b` emits
  both `-48c` and `--nvidia-tesla-t4-linux-amd64-4d`.
- **Three collapsed pools**: `nvidia-a10g` reports `1/1` whole card, and `nvidia-tesla-t4` aggregates
  5 total cards (`4` once-max on `node-t4-b` plus `1` on `node-t4-a`).
- **Three `Devices` ledgers**: CPU-only `node-cpu` has no `Devices` object.

If a GPU node is missing from `kubectl get devices`, inspect Device Manager `DaemonSet` pods and run
[Preflight Checks](/gpustack-operator/main/docs/modules/devices/preflight/index.md) on that host.

| Kind | Object on `node-a10g` | Key fields |
|---|---|---|
| `Node` | `node-a10g` | `gpustack.ai/managed: "true"`, `general.feature.gpustack.ai/amd-epyc-7r32*`, `acceleratable.feature.gpustack.ai/nvidia-a10g*` |
| `Devices` | `node-a10g` | `status.groups[].accelerators[]` with `remaining: 1600000` credits per card |
| `ResourceFlavor` | `gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d` | `feature.gpustack.ai/acceleratable: "true"`, `spec.nodeLabels` selecting managed nodes |
| `ClusterQueue` | `gpustack--nvidia-a10g-linux-amd64` | `admissionChecks: [gpustack-node-devices]`, `credits.gpustack.ai/nvidia: 1600k` |
| `InstanceType` | `gpustack--nvidia-a10g-linux-amd64` | `spec.unitResources` (`8` CPU / `64Gi` RAM preset), four-view `status` |
| `InstanceTypeFlavor` | `gpustack--nvidia-a10g` | Catalog entry carrying `product`, `memory`, `cores`, and `sliceable` |

### Step 2: Exclude one node

A node contributes capacity to pools only while `gpustack.ai/managed=true` is present. Patch
`NodeFeature/node-a10g-gpustack-worker` to set that label to `"false"` temporarily:

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

The A10G pool drops to `0/0 0/0 0/0 0/0` (its `ResourceFlavor` is deleted), and generic `CPU` drops
from `48/72` to `48/68`.

> **Persistent node opt-out**: while `Setting/node-management-manual` is `false` (default), the worker
> rewrites `<node>-gpustack-worker` back to `"true"` on its next node reconcile. To keep a node out of
> pools permanently, set `node-management-manual` to `"true"` and manage `gpustack.ai/managed` on each
> `Node` directly.

Restore `gpustack.ai/managed: "true"` and confirm A10G returns to `1/1 10/10 100/100 0/0`:

```bash
kubectl -n gpustack-system patch nodefeature node-a10g-gpustack-worker \
  --type=merge -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}'
kubectl get instancetypes
```

## Instance configuration

### Step 3: Request a logical slice

Because `gpustack--nvidia-a10g` reports `SLICEABLE=true`, save `sliced-demo.yaml` to request 20% of
one A10G's VRAM:

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
    acceleratorSlicedMemoryPercentage: 20  # 20% of 24 GiB VRAM
    acceleratorSlicedCoresPercentage: 100
  volume:
    ephemeral:
      capacity: 1Gi
```

Apply `sliced-demo.yaml`, wait for `sliced-demo` to reach `Ready`, and inspect pool capacity:

```bash
kubectl apply -f sliced-demo.yaml
kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready instance/sliced-demo --timeout=180s
kubectl get instancetypes
```

Captured output:

```text
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 80/80 0/0          0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

`SL` drops from `100/100` to `80/80`, while `EX` and `SH` drop to `0/0` because a partially sliced
card cannot satisfy whole-card or shared-card allocations. Verify that the slicing shim caps visible
VRAM inside the container:

```bash
kubectl -n default exec sliced-demo -- nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
```

Captured output:

```text
NVIDIA A10G, 4912 MiB
```

If `sliced-demo` stays `Pending`, inspect `kubectl -n default get instance sliced-demo -o yaml` and
match the message in [Troubleshooting](#troubleshooting). Delete `sliced-demo` before Step 4 so the
A10G pool returns to `1/1 10/10 100/100 0/0`. For hardware MIG partitions (`PT`) on A100 or H100, see
[NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md).

### Step 4: Manage a custom InstanceType

Derived `InstanceType`s initialize *`spec.unitResources`* from product presets (`8` CPU / `64Gi` RAM
for A10G; `4` CPU / `16Gi` RAM fallback for unrecognized models; see
[Instance resource defaults](/gpustack-operator/main/docs/modules/instances/index.md#instance-resource-defaults)). *`spec.unitResources`*
sets default CPU and RAM when an `Instance` omits them and caps explicit requests.

While `instance-general-resources-overcommit` is `true` (default), `Pod` scheduler requests use `100m`
CPU per core and `128Mi` RAM per GiB (when `false`, requests equal limits). Because
*`spec.unitResources`* is immutable after creation, derived `InstanceType`s keep their original sizing
across operator upgrades.

To offer larger per-GPU CPU and RAM caps without deleting the derived pool or disrupting running
`Instance`s, create a custom sibling `InstanceType` referencing the same *`acceleratorGroup`*[^2]:

[^2]: Current releases share one canonical pool `ClusterQueue` across sibling `InstanceType`s and `AcceleratorType`; the captured `ENTRANCE` column below reflects an earlier per-type `LocalQueue` naming pass.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: a10g-12c128g
spec:
  acceleratable: true
  acceleratorGroup: nvidia-a10g  # matches InstanceTypeFlavor gpustack--nvidia-a10g
  os: linux
  arch: amd64
  unitResources:
    cpu: "12"
    ram: 128Gi
  localStorage: 200Gi
```

Apply `a10g-12c128g.yaml` and confirm both A10G types report `Active` over the same physical card:

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

Launch a whole-GPU `Instance` on `a10g-12c128g`:

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

Both `a10g-12c128g` and `gpustack--nvidia-a10g-linux-amd64` report `0/0` remaining because they share
the same underlying card. Now delete `a10g-12c128g` and verify that `custom-demo` drains to `Stopped`
while releasing the A10G card:

```bash
kubectl delete instancetype a10g-12c128g
kubectl -n default get instance custom-demo -o jsonpath='{.status.phase}{"\n"}'
kubectl get instancetypes
```

Captured output:

```text
instancetype.worker.gpustack.ai "a10g-12c128g" deleted
Stopped
NAME                                    ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64           gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64       gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
gpustack--nvidia-tesla-t4-linux-amd64   gpustack-fnv64-6b371caa2da0b799   8/32Gi/100Gi            4/5 40/50 100/500 0/0      0/0     Active
```

### Step 5: Split pools by CPU

Enable `instance-type-aware-cpu-manufacturer` so derived pools split by CPU model (`gKey`) while
keeping the seven `ResourceFlavor`s unchanged:

```bash
kubectl -n gpustack-system patch setting instance-type-aware-cpu-manufacturer \
  --type=merge -p '{"spec":{"value":"true"}}'
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

Five CPU-aware pools appear (`amd-epyc-7r13`, `amd-epyc-7r32`, `intel-xeon-platinum-8259cl`, plus two
`(gKey, aKey)` GPU pools). Because derived `InstanceType`s are create-only, the three older collapsed
types stay until an administrator deletes them manually.

Restart `custom-demo` from Step 4 onto `gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64`:

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

Verify that both the CPU-aware and collapsed A10G pools report `0/0` while the `ResourceFlavor` count
stays at `7`:

```bash
kubectl get instancetypes gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64 gpustack--nvidia-a10g-linux-amd64
kubectl get resourceflavor --no-headers | wc -l
```

Captured output:

```text
NAME                                               ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64   gpustack-fnv64-029fd9550e0c70bd   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
gpustack--nvidia-a10g-linux-amd64                  gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            0/0 0/0 0/0 0/0            0/0   Active
7
```

### Step 6: Configure pinning and volumes

Use *`spec.nodeName`* to pin an `Instance` to one `Node`'s `kubernetes.io/hostname` selector (while
still queueing through Kueue and `gpustack-node-devices`), and *`spec.additionalVolumes`* to mount
datasets, `ConfigMap` keys, `Secret`s, or host directories into the workload container (and SSH
sessions):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: pinned-demo
  namespace: default
spec:
  type: gpustack--nvidia-tesla-t4-linux-amd64
  nodeName: node-t4-b  # renders nodeSelector kubernetes.io/hostname; editable only while stopped
  image: ubuntu:24.04
  command:
    - sleep
    - "86400"
  resources:
    accelerator: "1"
  volume:
    ephemeral:
      capacity: 1Gi
  volumeMount: /workspace
  additionalVolumes:  # unique canonical mountPaths; editable only while stopped
    - mountPath: /mnt/datasets
      persistent:  # same-namespace InstancePersistentVolume
        name: datasets
    - mountPath: /etc/model/config.json
      subPath: config.json
      readOnly: true
      configMap:
        name: model-config
    - mountPath: /mnt/host-cache  # requires Setting instance-host-path-volume-allowed=true
      readOnly: true
      hostPath:
        path: /var/lib/gpustack-cache
        type: DirectoryOrCreate
```

Constraints that admission and scheduling enforce:

- **Pin vs pool mismatch**: *`nodeName`* only checks that the `Node` exists at creation; if the pinned
  `Node` lacks the pool's labels or capacity, Kueue can admit the `Workload` while the `Pod` waits
  `Pending`.
- **Volume placement**: node-local persistent claims pin the `Instance` to their bound `Node` unless
  `instance-persistent-volume-placement` is disabled; unbound claims hold `Pod` creation and report why
  in *`status.phaseMessage`* ([PVC binding and placement](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md#pvc-binding-and-placement)).
- **Host-boundary gates**: both `instance-privileged-allowed` (*`spec.privileged`*) and
  `instance-host-path-volume-allowed` (*`additionalVolumes[].hostPath`*) default to `"false"` and gate
  new grants without stopping running `Instance`s.

After applying `pinned-demo.yaml` (with prerequisite `datasets`, `model-config`, and host-path
`Setting` enabled), verify that the `Pod` runs on `node-t4-b`:

```bash
kubectl -n default get pod -l app.kubernetes.io/instance=pinned-demo \
  -o jsonpath='{.items[0].spec.nodeName}{"\n"}'
```

Example output (illustrative):

```text
node-t4-b
```

## Limitations

All four nodes in this environment run unpartitioned (`PT` stays `0/0`). See
[MIG capacity scenarios](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#mig-capacity-scenarios) for hardware
MIG partitioning and [Pool troubleshooting](/gpustack-operator/main/docs/modules/devices/requests/index.md#pool-troubleshooting) for
queue quota diagnosis.

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `Instance` stays unready and `Workload` waits | Quota, flavor labels, or *`nodeName`* pin unsatisfied | Inspect `kubectl -n <ns> get instance <name> -o jsonpath='{.status.phaseMessage}'`, `kubectl -n <ns> get workloads`, and `kubectl get instancetypes`. Clear or change a mismatched *`spec.nodeName`* while stopped. |
| `Pod` rejected: `a Pod may request only one accelerator family` | Multiple accelerator families on one `Pod` | Request one family on the `Pod` ([Rule 1](/gpustack-operator/main/docs/modules/devices/requests/index.md#rule-1-family-and-container-group)). |
| `Pod` rejected: `at most one container may request a slicing family` | Multiple containers request `.sliced` or `.partitioned*` | Keep slicing requests on a single application container ([Rule 6](/gpustack-operator/main/docs/modules/devices/requests/index.md#rule-6-slicing-container-count)). |
| `Pod` rejected: `a restartable init container (a native sidecar) may not request an accelerator` | Native sidecar requests an accelerator | Move the accelerator request to `spec.containers[]` ([Rule 7](/gpustack-operator/main/docs/modules/devices/requests/index.md#rule-7-native-sidecar-requests)). |
| Node accelerators absent from pool | `gpustack.ai/managed` is `"false"` or absent under manual mode | Check `Setting/node-management-manual` and `kubectl get node <node> --show-labels`; label the `Node` `gpustack.ai/managed=true` when manual mode is enabled. |
| `Instance` rejected on `privileged` or `hostPath` | `instance-privileged-allowed` or `instance-host-path-volume-allowed` is `"false"` | Remove the field from the `Instance`, or enable the owning `Setting` in `gpustack-system` if cluster policy permits. |

To enable `instance-privileged-allowed` (or `instance-host-path-volume-allowed`) when required:

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

**See also** — [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) (resource keys and request
rules) · [NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#mig-capacity-scenarios)
(hardware MIG partitioning) · [GPU Instances](/gpustack-operator/main/docs/modules/instances/index.md) (instance resource
defaults and utilization metrics)

**Next** → [RDMA Networking Walkthrough](/gpustack-operator/main/docs/walkthroughs/rdma/network-endpoints/index.md) — request and verify RDMA endpoints with NUMA alignment.
