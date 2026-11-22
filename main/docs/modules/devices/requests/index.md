# Accelerator Requests

`Pod` admission webhooks validate accelerator requests and compute quota units at creation for every
`Pod` carrying `kueue.x-k8s.io/queue-name`. Unlabeled `Pod`s bypass these checks and credit folds.
See [Pool troubleshooting](#pool-troubleshooting) to inspect pool capacity and `Pending` workloads.

## Contents

- [Accelerator resource families](#accelerator-resource-families)
- [Request rules](#request-rules)
- [RDMA resource requests](#rdma-resource-requests)
- [Legacy request migration](#legacy-request-migration)
- [Limitations](#limitations)
- [Pool troubleshooting](#pool-troubleshooting)

## Accelerator resource families

GPUStack separates software slicing and hardware partitioning into disjoint resource families:

| Family | Mechanism | Isolation | Example |
|---|---|---|---|
| **Logical slicing** (`.sliced*`) | Software sharing of an unpartitioned card | Vendor runtime or preload shim caps VRAM and compute ([Isolation mode selection](#isolation-mode-selection)) | `50%` of an NVIDIA A10G |
| **Physical partitioning** (`.partitioned*`) | Hardware instance on a MIG-enabled card | Hardware partition materialized by Device Manager | NVIDIA `mig-3g.40gb`, T-Head `mig-4g.48gb`, Hygon `mig-2g.15gb` |

A partitioned card advertises **only** `.partitioned*` keys; an unpartitioned card advertises
**only** `<base>`, `.shared`, and `.sliced*` keys. Those populations map to the four `InstanceType`
views (`EX`, `SH`, `SL`, and `PT`).

### Resource keys

`<base>` is the vendor extended resource (`nvidia.com/gpu`, `huawei.com/npu`, `amd.com/gpu`, and
others in [Accelerator Support](https://github.com/gpustack/gpustack-operator/blob/8ed4a0acaea93847d38a3ab3434849d91603a342/README.md#accelerator-support)):

| Key | Provider | Eligible cards | Request value | `Node` capacity |
|---|---|---|---|---|
| `<base>` | Device plugin (`Exclusive`) | Unpartitioned | Whole card count | Healthy tokens |
| `<base>.shared` | Device plugin (`Shared`) | Unpartitioned | Distinct cards on one `Node` (`1` of `10` shares per card) | Healthy tokens |
| `<base>.sliced` | Device plugin (`Sliced`) | Logically sliceable | Always `1` | Healthy tokens |
| `<base>.sliced.units` | Node capacity | Logically sliceable | **Webhook-derived** (never set manually) | Cards × `1,600,000` |
| `<base>.sliced.cores-percentage` | Node capacity | Logically sliceable | `(0, 100]` (defaults to `100`) | Sum of card budgets |
| `<base>.sliced.memory-percentage` | Node capacity | Logically sliceable | `(0, 100]` | Sum of card budgets |
| `<base>.sliced.memory-mib` | Node capacity | Logically sliceable | Positive MiB `<=` card VRAM | Sum of card budgets |
| `<base>.partitioned` | Device plugin (`Partitioned`) | Partitioned | Always `1` | Healthy tokens |
| `<base>.partitioned.units` | Node capacity | Partitioned | **Webhook-derived** (never set manually) | Cards × `1,600,000` |
| `<base>.partitioned.<kind>-<profile>` | Node capacity | Partitioned | Always `1` | `allocated + remaining` |
| `device.gpustack.ai/<manufacturer>.visibility` | Device plugin (`Visibility`) | Any card | Workload container's card count | Healthy tokens |

- **Webhook-owned `.units` keys**: The `Pod` webhook overwrites `.units` from slice memory or
  partition VRAM to charge Kueue `credits.gpustack.ai/<manufacturer>`.
- **Token binding**: `<base>`, `.shared`, and `.sliced` tokens bind specific cards in kubelet, while
  `.partitioned` and `.visibility` tokens are fungible counts resolved by Device Manager at
  `Allocate`.

### Pod request examples

Submit `Pod`s on a pool's namespace `LocalQueue` (`ENTRANCE` on `kubectl get instancetypes`).

**Exclusive** (two whole cards):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: exclusive-pod
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      command:
        - sleep
        - "3600"
      resources:
        limits:
          nvidia.com/gpu: "2"
```

**Shared** (one unisolated `1/10` share on each of two distinct cards on one `Node`; for `>= 2`
cards, the webhook injects `acceleratable.feature.gpustack.ai/<group>.count Gt N-1`):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: shared-pod
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      command:
        - sleep
        - "3600"
      resources:
        limits:
          nvidia.com/gpu.shared: "2"
```

**Logical slice** (`50%` VRAM and `40%` compute on one card):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: sliced-pod
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      command:
        - sleep
        - "3600"
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"  # always "1"
          nvidia.com/gpu.sliced.memory-percentage: "50"  # or .sliced.memory-mib, never both
          nvidia.com/gpu.sliced.cores-percentage: "40"  # optional, default: "100"
```

**Physical partition** (one `3g.40gb` MIG instance):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: partitioned-pod
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      command:
        - sleep
        - "3600"
      resources:
        limits:
          nvidia.com/gpu.partitioned: "1"  # always "1"
          nvidia.com/gpu.partitioned.mig-3g.40gb: "1"  # always "1"
```

**SSH visibility sidecar** (sits outside the four families to share `main`'s device grant):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ssh-sidecar
spec:
  containers:
    - name: sshd
      image: gpustack/ssh-server:v1.4.0
      resources:
        limits:
          device.gpustack.ai/nvidia.visibility: "1"
```

### Instance API requests

An `Instance` declares whole cards, one logical slice, or one hardware partition in
*`spec.resources`* ([GPU Instance Configuration](/gpustack-operator/main/docs/modules/instances/configuration/index.md#resource-requests)):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: partitioned-instance
  namespace: team-a
spec:
  type: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
  image: ubuntu:24.04
  command:
    - sleep
    - infinity
  resources:
    accelerator: "1"  # integer whole cards, or "1" for sliced/partitioned requests
    # acceleratorSlicedMemoryPercentage: 50
    # acceleratorSlicedCoresPercentage: 50
    acceleratorPartitionedProfile: 3g.40gb  # mutually exclusive with slice percentages
  volume:
    ephemeral:
      capacity: 10Gi
```

Admission rejects unknown partition profiles (listing offered profiles), partitioning on unsupported
vendors or unpartitioned pools, slice percentages on pools without logical slicing, non-`1` card
counts on slice/partition requests, and whole-card counts exceeding *`status.accelerator.capacity`*.
Host CPU and RAM scale by card count or slice/partition VRAM share.

### Preferred accelerator placement

Steer kubelet's intra-node device selection to specific cards by setting a `Pod` annotation for
whole, shared, or logical-slice requests:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: preferred-card-pod
  labels:
    kueue.x-k8s.io/queue-name: <local-queue-name>  # required; selects the pool LocalQueue and Pod webhook
  annotations:
    device.gpustack.ai/accelerator.preferred-index: "3,1"  # comma-separated Devices index order
    # device.gpustack.ai/accelerator.preferred-id: "GPU-...,GPU-..."  # takes precedence over preferred-index
spec:
  nodeSelector:  # optional; annotations select cards inside one node, so constrain the node separately
    kubernetes.io/hostname: gpu-node-0
  containers:
    - name: main
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu: "2"
```

- **Annotation precedence**: Presence of `device.gpustack.ai/accelerator.preferred-id` (even when
  empty) overrides `device.gpustack.ai/accelerator.preferred-index`.
- **Value matching**: Match comma-separated IDs or indices against the node's
  `Devices.spec.groups[].accelerators[]` (`id` or `index`); whitespace and duplicates are ignored.
- **Advisory intra-node hint**: Applies only inside the scheduled `Node` via
  `GetPreferredAllocation`; unavailable, cross-node, or insufficient cards fall back to other
  node cards. For logical slices, if every card lacks the requested units, a fallback hint can still
  be returned and `Allocate` rejects overcommitment.
- **Selection order**: Over-specified lists take the highest-priority available cards up to the
  request size, returned in ascending hardware order.
- **Unsupported modes**: Hardware partitioning (`Partitioned` / MIG) and SSH visibility sidecars
  ignore preferred-card annotations.
- **Pod scope**: Both annotations apply at `Pod` scope across all containers requesting accelerators.

## Request rules

Admission groups containers by lifetime: standard *`spec.initContainers`* (without
`restartPolicy: Always`), *`spec.containers`*, and **native sidecars** (*`spec.initContainers`* with
`restartPolicy: Always`). Include full container `image` and `queue-name` labels when applying.

### Rule 1: Family and container group

All accelerator requests in a `Pod` must use **one family** and sit in **one container group**
(either init containers or app containers, never both, because kubelet holds init devices for the
`Pod`'s life while the scheduler charges `max(init, app)`):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: mixed-family-pod
spec:
  containers:
    - name: a
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu: "1"
    - name: b
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"
          nvidia.com/gpu.sliced.memory-percentage: "50"
```

> `spec: Forbidden: a Pod may request only one accelerator family, found [exclusive sliced]`

### Rule 2: Logical slice count

A logical slice request must set `<base>.sliced` to `"1"` and specify **exactly one** of
`.sliced.memory-percentage` (`(0, 100]`) or `.sliced.memory-mib` (`(0, card VRAM]`). Use multiple
`Pod`s or whole cards for multi-GPU workloads:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: invalid-slice-count
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.sliced: "2"
          nvidia.com/gpu.sliced.memory-percentage: "50"
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.sliced]: Invalid value: "2": a logical slice request is always a single accelerator; multi-accelerator logical slicing is not supported yet`

### Rule 3: Partition count

A partition request must set `<base>.partitioned` to `"1"` and name a `.partitioned.<kind>-<profile>`
key:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: invalid-partition-count
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.partitioned: "2"
          nvidia.com/gpu.partitioned.mig-1g.10gb: "1"
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.partitioned]: Invalid value: "2": a partition request is always a single accelerator; request one Pod per instance`

### Rule 4: Partition profile

A `Pod` may request only **one** partition profile across all containers:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: invalid-partition-profiles
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.partitioned: "1"
          nvidia.com/gpu.partitioned.mig-1g.10gb: "1"
          nvidia.com/gpu.partitioned.mig-3g.40gb: "1"
```

> `spec: Forbidden: a Pod may request only one partition profile, found [1g.10gb 3g.40gb]`

### Rule 5: Profile instance count

Each `.partitioned.<kind>-<profile>` key must equal `"1"`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: invalid-profile-count
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.partitioned: "1"
          nvidia.com/gpu.partitioned.mig-3g.40gb: "2"
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.partitioned.mig-3g.40gb]: Invalid value: "2": a partition profile request must be exactly 1 instance`

### Rule 6: Slicing container count

At most **one** container per `Pod` may request `.sliced*` or `.partitioned*` keys
(`.visibility` sidecars are exempt):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: multi-sliced-pod
spec:
  containers:
    - name: a
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"
          nvidia.com/gpu.sliced.memory-percentage: "50"
    - name: b
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"
          nvidia.com/gpu.sliced.memory-percentage: "50"
```

> `spec: Forbidden: at most one container may request a slicing family, found 2`

### Rule 7: Native sidecar requests

Restartable init containers (`restartPolicy: Always`) must not request accelerators because they
overlap both later init containers and app containers:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: rejected-sidecar-pod
spec:
  initContainers:
    - name: log-shipper
      image: ubuntu:24.04
      restartPolicy: Always
      resources:
        limits:
          nvidia.com/gpu: "1"
  containers:
    - name: main
      image: ubuntu:24.04
```

> `spec.initContainers[0].resources.limits: Forbidden: a restartable init container (a native sidecar) may not request an accelerator; move the request to an app container`

## RDMA resource requests

Node-level RDMA keys (`device.gpustack.ai/rdma`, `rdma.shared` at `64` tokens/endpoint, and
`rdma.partitioned` for *SR-IOV* VFs) sit outside accelerator families, carry no `.units` keys, and
are unmetered by Kueue ([RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md#rdma-resource-requests) and
[NIC record inspection](/gpustack-operator/main/docs/modules/rdma/operations/index.md#nic-record-inspection)):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: shared-rdma-pod
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      resources:
        limits:
          nvidia.com/gpu.shared: "1"
          device.gpustack.ai/rdma.shared: "1"
```

### NUMA alignment conditions

Kubelet aligns an accelerator and an RDMA interface onto the same *NUMA* node only when all four
conditions hold:

- **Same container**: Both resource requests sit in the same container (`topologyManagerScope: container`).
- **NUMA hints present**: Whole, shared, and logical-slice accelerator tokens publish *NUMA* hints;
  fungible `.partitioned` tokens publish none.
- **Enforcing kubelet policy**: The `Node` runs `restricted` or `single-numa-node`
  ([Enabling NUMA alignment](/gpustack-operator/main/docs/modules/rdma/operations/index.md#enabling-numa-alignment) and
  [Result format](/gpustack-operator/main/docs/modules/devices/preflight/index.md#result-format)).
- **Node-level capacity fit**: Kube-scheduler sees only node totals; if a `Node`'s free GPU and HCA
  sit on different sockets, kubelet rejects the `Pod` without rescheduling.

## Legacy request migration

- **Legacy `.sliced.mig-*` keys removed**: Replace legacy `<base>.sliced.mig-<profile>` requests
  with `<base>.partitioned: "1"` and `<base>.partitioned.mig-<profile>: "1"`. Remove stale capacity
  entries on development `Node`s (`/` escaped as `~1`):

  ```bash
  kubectl get node <node> -o json | jq -r '.status.capacity | keys[] | select(contains("mig-"))'
  kubectl patch node <node> --subresource=status --type=json \
    -p '[{"op":"remove","path":"/status/capacity/<escaped-key>"}]'
  ```

- **Per-container allocation annotation**: `device.gpustack.ai/accelerator.allocated` uses a
  per-container map; drain accelerator `Pod`s before upgrading Device Manager from legacy flat
  builds.
- **`.shared: N` counts distinct cards**: `InstanceType` `SH` `onceMaxRequest` now counts cards with
  a free share (`1/10` on one free card instead of legacy `10/10`).

## Limitations

- **Excluded MIG variants**: Profiles containing `+` (`+me`, `+me.all`, `+gfx`) are omitted because
  Kubernetes resource names forbid `+`.
- **Pool-wide whole-card cap**: Whole-card requests above a single `Node`'s card count but within
  pool *`status.accelerator.capacity`* are admitted and remain queued.
- **No manual `nvidia-smi mig -cgi` carving**: Hand-carved instances lack `Pod` annotations, skewing
  node capacity until reclaimed when idle.
- **Manual MIG mode toggle**: Switching hardware partitioning mode requires draining workloads,
  running the vendor tool, and restarting the Device Manager `DaemonSet` ([NVIDIA](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#enabling-partitioning),
  [T-Head](/gpustack-operator/main/docs/modules/devices/thead-mig/index.md#enabling-partitioning), [Hygon](/gpustack-operator/main/docs/modules/devices/hygon-mig/index.md#enabling-partitioning)).
- **Shared card-count pin gaps**: Unqueued `.shared` `Pod`s on too few cards fail with
  `UnexpectedAdmissionError`, and template-derived `Job` / `JobSet` / `RayJob` `Workload`s or mixed
  partitioned `Node`s are not narrowed by the `Pod` webhook's `.count` affinity.
- **Partition NUMA alignment**: Under `single-numa-node`, CPU/memory may lock to one socket while
  the only free MIG slot sits on another socket.

## Pool troubleshooting

### Isolation mode selection

```bash
kubectl get instancetypes -o wide
kubectl get devices <node> -o jsonpath='{.spec.groups[*].accelerators[*].status}'
```

| Mode | Request key | Expected signal | Next action |
|---|---|---|---|
| Exclusive | `<base>` | Non-zero `EX` on `InstanceType` | Request integer cards on unpartitioned `Node`s. |
| Shared | `<base>.shared` | Non-zero `SH` (`10` shares/card) | Request `N` distinct cards on one `Node` (no VRAM cap). |
| Logical slice | `<base>.sliced` | Non-zero `SL`; `logicalSliced` in `Devices` | Set `.sliced: "1"` and one memory budget. |
| Physical partition | `<base>.partitioned.<kind>-<profile>` | Non-zero `PT`; `PARTITIONS` in `-o wide` | Set `.partitioned: "1"` and `.partitioned.<kind>-<profile>: "1"`. |

Logical slicing mechanism and slice limits per manufacturer:

| Manufacturer | Enforcer | Compute overcommit | Max slices |
|---|---|---|---|
| NVIDIA, Iluvatar | HAMi-core `libvgpu.so` | Yes | `128` (`100` Iluvatar) |
| AMD | `libvrocm.so` + `HSA_CU_MASK` | Yes | `128` |
| T-Head | `hggc_quota.so` + `hgml_dlsym_hook.so` | Yes | `128` |
| Ascend | vcann-rt `libvruntime.so` | Yes | `63` |
| Cambricon | cnDev sMLU profile | No | `16` |
| MetaX | sysfs `sgpu` subdevice | No | `16` |
| MThreads | Host sGPU kmod | No | `16` |
| Hygon | Host DTK `vdev.conf` | No | `4` |

### Inspecting pools and quota

```bash
kubectl get instancetypes
kubectl -n <namespace> get localqueues
kubectl get clusterqueue <cluster-queue> -o yaml
kubectl get resourceflavors -l resource.gpustack.ai/type=nodes
```

Confirm `InstanceType` `PHASE` is `Active`, `ENTRANCE` exists in your namespace, and `ClusterQueue`
has `spec.stopPolicy: None`, `gpustack-node-devices` in `admissionChecksStrategy`, and
`credits.gpustack.ai/<manufacturer>` (`1,600,000` per card) in `resourceGroups`. If `ClusterQueue`
is on `Hold` (`empty-plan-hold` or `spec.inactive: true`), verify `ResourceFlavor`s and
`gpustack.ai/managed=true` `Node` labels.

### Diagnosing Pending workloads

| Stage | Command | Expected signal | Next action |
|---|---|---|---|
| Webhook refusal | `kubectl apply` or `kubectl -n <ns> get events` | `Forbidden`, `Invalid value`, or `Required value` (no `Pod` created) | Fix manifest against [Request rules](#request-rules) or offered pool profiles. |
| Queued demand | `kubectl -n <ns> describe workload <name>` | `Pod` gated by `kueue.x-k8s.io/admission`; `QuotaReserved=False` | Wait for quota, add `Node`s, or reduce requested resources. |
| Card fragmentation (affinity) | `kubectl -n <ns> describe workload <name>` | `excluded: affinity` despite free aggregate pool credits | Wait for a contiguous card or request a smaller slice. |
| Card fragmentation (check / kubelet) | `kubectl -n <ns> get workload <name> -o jsonpath='{.status.admissionChecks}'` | `gpustack-node-devices` in `Retry`, or `Pod` `Failed` (`UnexpectedAdmissionError`) | Inspect `Devices` `remaining` / `allocatedSlices` and `fit.gpustack.ai` labels below; delete failed unqueued `Pod`s. |

```bash
kubectl get devices <node> -o yaml
kubectl get node <node> -o json | jq '.metadata.labels | with_entries(select(.key | contains("fit.gpustack.ai")))'
```

---

**See also** — [NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md) (per-GPU MIG setup and capacity accounting) · [Hygon MIG Operations](/gpustack-operator/main/docs/modules/devices/hygon-mig/index.md) (node-wide MIG on Hygon DCUs) · [T-Head MIG Operations](/gpustack-operator/main/docs/modules/devices/thead-mig/index.md) (MIG partitioning on T-Head PPUs) · [Preflight Checks](/gpustack-operator/main/docs/modules/devices/preflight/index.md) (bare-host detection and slicing verification) · [RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) (RDMA endpoint pairing and *NUMA* alignment)

**Next** → [Heterogeneous Devices Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md) — trace queued accelerator requests on a live cluster.
