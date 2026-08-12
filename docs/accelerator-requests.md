# Accelerator Requests

> **Purpose** — the normative request contract: every resource key a workload may set, the seven rules
> admission enforces, and a worked example per family.
> **Audience** users writing workloads, contributors touching the webhooks · **Prerequisites**
> [Architecture](./architecture.md) · **Read time** ~12 min

Every rule below is checked at admission: a violating Pod is rejected at `CREATE`, never discovered at
container start. They bind the **Kueue-managed path** — both Pod webhooks select on the
`kueue.x-k8s.io/queue-name` label, so a Pod without it bypasses every rule and every fold while still
able to request the device-plugin resources.

## Contents

- [Two families, two accelerator populations](#two-families-two-accelerator-populations)
- [The resource keys](#the-resource-keys)
- [Worked example per family](#worked-example-per-family)
- [The request rules](#the-request-rules)
- [Requesting through the Instance API](#requesting-through-the-instance-api)
- [Pre-release breaks](#pre-release-breaks)
- [Limitations](#limitations)

## Two families, two accelerator populations

An accelerator is shared in one of two physically incompatible ways, and GPUStack names them apart:

| Term | What it is | How isolation is enforced | Example |
|---|---|---|---|
| **Logical slicing** (`.sliced*`) | software slicing of a whole accelerator | the manufacturer's own sharing facility caps compute and VRAM per container — [which facility, per manufacturer](architecture/device-discovery.md#sliced-logical-slicing) | 50 % of an A10G |
| **Physical partitioning** (`.partitioned*`) | hardware partitioning of an accelerator put into a partitioning mode | the hardware itself; the operator materializes the instance | an NVIDIA MIG `3g.40gb` or a T-Head PPU partition |

The two never apply to the same accelerator: one in a partitioning mode advertises **only** the partition
family, an unpartitioned one **only** the whole-accelerator, shared and logical-slice families. Hence the
four separate `InstanceType` views (`EX` / `SH` / `SL` / `PT`) — each accelerator feeds exactly one.

`<kind>` is the manufacturer's own word for hardware partitioning, and NVIDIA and T-Head both call it
`mig`: `nvidia.com/gpu.partitioned.mig-3g.40gb`, `alibabacloud.com/ppu.partitioned.mig-<profile>`. A
manufacturer with no hardware partitioning has no kind, and no `.partitioned*` keys at all.

## The resource keys

`<base>` is the manufacturer's device resource (`nvidia.com/gpu`, `huawei.com/npu`, … — see
[Accelerator support](../README.md#accelerator-support)).

| Key | Served by | Accelerators that serve it | Request value | Node value |
|---|---|---|---|---|
| `<base>` | device plugin (Exclusive) | unpartitioned only | accelerator count | Σ healthy tokens |
| `<base>.shared` | device plugin (Shared) | unpartitioned only | ownership shares (10 per accelerator) | Σ healthy tokens |
| `<base>.sliced` | device plugin (Sliced) | logically sliceable only | always `1` | Σ healthy tokens |
| `<base>.sliced.units` | node capacity | logically sliceable | **webhook-derived**, per accelerator | Σ accelerators × 1,600,000 |
| `<base>.sliced.cores-percentage` | node capacity | logically sliceable | per accelerator, `(0,100]` | Σ per-accelerator budget |
| `<base>.sliced.memory-percentage` | node capacity | logically sliceable | per accelerator, `(0,100]` | Σ per-accelerator budget |
| `<base>.sliced.memory-mib` | node capacity | logically sliceable | per accelerator, ≤ accelerator VRAM | Σ per-accelerator budget |
| `<base>.partitioned` | device plugin (Partitioned) | partitioned only | always `1` | Σ healthy tokens |
| `<base>.partitioned.units` | node capacity | partitioned | **webhook-derived** | Σ accelerators × 1,600,000 |
| `<base>.partitioned.<kind>-<profile>` | node capacity | partitioned | always `1` | Σ (allocated + remaining) |
| `device.gpustack.ai/<manufacturer>.visibility` | device plugin (Visibility) | every accelerator | sidecar's accelerator count | Σ tokens |

Two things the table does not say:

- **Never write a `.units` key by hand.** The Pod webhook recomputes it from the memory budget (logical)
  or the profile's VRAM (partition), overwriting any client value. It feeds Kueue's `credits`
  transformation, so a partition and a logical slice of the same VRAM cost the same credits.
- **The two token shapes differ.** `<base>`, `.shared` and `.sliced` tokens are *accelerator-bound*: the
  token the kubelet picks **is** the accelerator. `.partitioned` and visibility tokens are a *fungible
  count* — the plugin picks the accelerator itself against the live partition geometry and records the
  one it used. So a partition request never lands where its profile does not fit, and a rejection from
  `Allocate` means the whole node has no room.

## Worked example per family

All four are Pods submitted on a pool's entrance `LocalQueue` (`kueue.x-k8s.io/queue-name`).

**Exclusive** — two whole accelerators; **Shared** — 3 of an accelerator's 10 ownership shares:

```yaml
resources: { limits: { nvidia.com/gpu: "2" } }
resources: { limits: { nvidia.com/gpu.shared: "3" } }
```

**Logical slice** — half of one accelerator's VRAM, capped at 40 % of its compute:

```yaml
resources:
  limits:
    nvidia.com/gpu.sliced: "1"                       # always exactly 1 accelerator
    nvidia.com/gpu.sliced.memory-percentage: "50"    # per accelerator; or .sliced.memory-mib, never both
    nvidia.com/gpu.sliced.cores-percentage: "40"     # per accelerator; defaults to 100 when omitted
    # nvidia.com/gpu.sliced.units is folded by the webhook — do not set it
```

**Physical partition** — one MIG `3g.40gb` instance:

```yaml
resources:
  limits:
    nvidia.com/gpu.partitioned: "1"                  # always exactly 1 accelerator
    nvidia.com/gpu.partitioned.mig-3g.40gb: "1"      # always exactly 1 instance
    # nvidia.com/gpu.partitioned.units is folded by the webhook — do not set it
```

**The SSH sidecar** sits outside the four families, so no family rule counts it. It requests the
internal visibility resource for its workload container's accelerator count:

```yaml
resources: { limits: { device.gpustack.ai/nvidia.visibility: "1" } }
```

## The request rules

The rules are scoped by a container's **lifetime group**, not by the field it sits in:

- the **init group** is `spec.initContainers` *without* `restartPolicy: Always`;
- the **running group** is `spec.containers`;
- a **native sidecar** — `spec.initContainers` *with* `restartPolicy: Always` — belongs to neither. It
  starts during the init phase and keeps running, so it overlaps every later init container as well as
  every app container.

### Rule 1 — one family, in exactly one container group

*All containers of a Pod request the same accelerator family, and the accelerator claims sit in exactly
one container group.*

Accepted — both app containers claim the same family:

```yaml
spec:
  containers:
    - name: trainer
      resources: { limits: { nvidia.com/gpu: "1" } }
    - name: sidecar-metrics
      resources: { limits: { nvidia.com/gpu: "1" } }
```

Rejected — two families in one Pod:

```yaml
spec:
  containers:
    - { name: a, resources: { limits: { nvidia.com/gpu: "1" } } }
    - { name: b, resources: { limits: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.memory-percentage: "50" } } }
```

> `spec: Forbidden: a Pod may request only one accelerator family, found [exclusive sliced]`

Rejected — the same family claimed in both container groups:

```yaml
spec:
  initContainers: [{ name: warmup, resources: { limits: { nvidia.com/gpu: "1" } } }]
  containers:     [{ name: main,   resources: { limits: { nvidia.com/gpu: "1" } } }]
```

> `spec.initContainers: Forbidden: a Pod's accelerator requests must all sit in one container group;
> spec.initContainers must give up its request, because its devices are held for the Pod's whole life while
> the scheduler charges the Pod only once`

Needs a device in two phases? Keep the claim on the app container — its device is the same hardware the
init container would have held.

> **Why** — two independent reasons, and one group makes charge and consumption agree by construction.
>
> - *Nothing releases the earlier claim.* The kubelet holds a finished init container's devices for the
>   Pod's whole life, and GPUStack's reclaimer destroys a partition on **Pod** deletion, not container
>   termination. The second claim coexists with the first.
> - *The scheduler charges once.* A Pod's demand for a key is `max(Σ init, Σ app)`, so two same-family
>   claims cost **one** unit of quota while consuming **two** accelerators. The node over-advertises by
>   one slot per such Pod, and the *next* tenant fails terminally.

### Rule 2 — `<base>.sliced` is exactly 1

Accepted: `nvidia.com/gpu.sliced: "1"`. Rejected:

```yaml
resources: { limits: { nvidia.com/gpu.sliced: "2", nvidia.com/gpu.sliced.memory-percentage: "50" } }
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.sliced]: Invalid value: "2": a logical slice
> request is always a single accelerator; multi-accelerator logical slicing is not supported yet`

A multi-accelerator workload asks for several Pods, or for whole accelerators.

> **Why** — a **deferral, not a manufacturer limit**: NVIDIA can isolate more than one accelerator. No
> node-level key expresses "N *distinct* accelerators" — an additive scalar cannot tell "two at half
> each" from "one in full", so a one-accelerator node would accept the request and fail it at
> `Allocate`. Lifting the cap needs a node-level accelerator-count dimension.

A logical slice must also name exactly one memory budget:

- neither `.sliced.memory-percentage` nor `.sliced.memory-mib` → `Required value: a nvidia.com/gpu.sliced
  request must set nvidia.com/gpu.sliced.memory-percentage or nvidia.com/gpu.sliced.memory-mib`;
- both → `Forbidden: cannot set both …memory-percentage and …memory-mib`;
- a percentage outside `(0,100]`, a non-positive MiB value, or a MiB value above the accelerator's
  VRAM → rejected.

### Rule 3 — `<base>.partitioned` is exactly 1

Accepted: `nvidia.com/gpu.partitioned: "1"`. Rejected:

```yaml
resources: { limits: { nvidia.com/gpu.partitioned: "2", nvidia.com/gpu.partitioned.mig-1g.10gb: "1" } }
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.partitioned]: Invalid value: "2": a partition
> request is always a single accelerator; request one Pod per instance`

Unlike rule 2 this is a **scope decision** — the plugin picks the accelerator itself, so `N > 1` would
be implementable. No workload needs it yet, so a multi-partition workload asks for several Pods.

A bare accelerator key with no profile is also rejected; there is no hardware shape to actuate.

> `Required value: a nvidia.com/gpu.partitioned request must name a profile, e.g.
> nvidia.com/gpu.partitioned.<kind>-<profile>`

### Rule 4 — a partition request names exactly one profile shape

Accepted: one per-profile key. Rejected:

```yaml
resources:
  limits:
    nvidia.com/gpu.partitioned: "1"
    nvidia.com/gpu.partitioned.mig-1g.10gb: "1"
    nvidia.com/gpu.partitioned.mig-3g.40gb: "1"
```

> `spec: Forbidden: a Pod may request only one partition profile, found [1g.10gb 3g.40gb]`

The rule is Pod-wide as well as per-container: two containers naming different profiles are rejected the
same way.

### Rule 5 — each per-profile key is exactly 1

Accepted: `nvidia.com/gpu.partitioned.mig-3g.40gb: "1"`. Rejected:

```yaml
resources: { limits: { nvidia.com/gpu.partitioned: "1", nvidia.com/gpu.partitioned.mig-3g.40gb: "2" } }
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.partitioned.mig-3g.40gb]: Invalid value: "2": a
> partition profile request must be exactly 1 instance`

### Rule 6 — at most one container may request a slicing family

Logical or physical, one claiming container per Pod. The SSH sidecar is unaffected: the visibility
resource sits deliberately outside the accelerator families.

Accepted:

```yaml
spec:
  containers:
    - name: main
      resources: { limits: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.memory-percentage: "50" } }
    - name: sshd
      resources: { limits: { device.gpustack.ai/nvidia.visibility: "1" } }   # visibility, not a slicing family
```

Rejected — two containers each holding a slice:

```yaml
spec:
  containers:
    - { name: a, resources: { limits: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.memory-percentage: "50" } } }
    - { name: b, resources: { limits: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.memory-percentage: "50" } } }
```

> `spec: Forbidden: at most one container may request a slicing family, found 2`

### Rule 7 — a restartable init container may not request an accelerator

Accepted — the claim sits on an app container while the native sidecar carries none; rejected — the
native sidecar carries the claim:

```yaml
spec:                                                       # accepted
  initContainers: [{ name: log-shipper, restartPolicy: Always, resources: { limits: { cpu: "100m" } } }]
  containers:     [{ name: main, resources: { limits: { nvidia.com/gpu: "1" } } }]
---
spec:                                                       # rejected
  initContainers: [{ name: log-shipper, restartPolicy: Always, resources: { limits: { nvidia.com/gpu: "1" } } }]
```

> `spec.initContainers[0].resources.limits: Forbidden: a restartable init container (a native sidecar) may
> not request an accelerator; move the request to an app container`

A native sidecar belongs to neither container group, so its claim would overlap every later init
container *and* every app container — the exact double-consumption rule 1 exists to prevent, with no
group to move it out of.

## Requesting through the `Instance` API

An `Instance` expresses the same four families through `spec.resources`, and the controller shapes them
into the keys above:

| Field | Effect |
|---|---|
| `accelerator: "N"` | whole accelerators (exclusive) — may span accelerators |
| `accelerator: "1"` + `acceleratorSlicedMemoryPercentage` (+ `acceleratorSlicedCoresPercentage`) | one logical slice |
| `accelerator: "1"` + `acceleratorPartitionedProfile: "3g.40gb"` | one hardware partition of that profile |

```yaml
kind: Instance
spec:
  type: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
  resources:
    accelerator: "1"
    acceleratorPartitionedProfile: 3g.40gb
```

The webhook rejects these at admission rather than leaving the Instance Pending:

- a profile the pool does not offer — the message lists the offered set;
- a profile on a manufacturer with no hardware partitioning;
- a profile **and** a slice percentage together (`a hardware partition and a logical slice percentage are
  mutually exclusive`);
- an `accelerator` count other than `1` for a slice or a partition request;
- **a slice percentage against a pool that offers no logical slicing** — a tightening; such a request
  used to be silently reshaped into a whole-accelerator one and served. On an all-partitioned pool the
  message points at `spec.resources.acceleratorPartitionedProfile` instead.

A partition's host CPU and RAM are sized by the profile's share of the accelerator's VRAM, so a `1g`
instance does not ask for a whole accelerator's worth.

## Pre-release breaks

No released version is marked stable, so both of the following are clean breaks with **no** translation
layer.

**The old MIG key is gone.** A MIG profile used to be requested through the *logical* family: a
per-profile key built from `<base>.sliced` with a `mig-<profile>` segment appended, alongside
`<base>.sliced: 1`. It is replaced by `<base>.partitioned.<kind>-<profile>` alongside
`<base>.partitioned: 1`.

Nothing recognizes the old one — not the builders, not the parsers, not the node-capacity reconciler,
and deliberately **not even a rejection by name**. Rewrite such a manifest; untouched, it meets two
ordinary failures, neither naming the replacement:

- its per-profile key is an extended resource no node advertises, so it never schedules;
- the `<base>.sliced: 1` beside it is a logical slice with no memory budget, which rule 2 rejects.

> **Why** — legibility would cost a legacy branch in every key path, to serve a request no
> documentation, no `InstanceType` and no example produces.

A development node an earlier build wrote those legacy capacities onto keeps them, because nothing owns
them. List what is left and patch it off — one JSON-patch removal per stale key:

```console
$ kubectl get node <node> -o json | jq -r '.status.capacity | keys[] | select(contains("mig-"))'
$ kubectl patch node <node> --subresource=status --type=json \
    -p '[{"op":"remove","path":"/status/capacity/<escaped-key>"}]'   # escape "/" in the key as "~1"
```

**The allocation annotation's value changed too, so drain a node before upgrading its device manager.**
The Pod annotation `device.gpustack.ai/accelerator.allocated` is now a per-container map; the old flat
shape is not read. Drain the node (or delete its accelerator Pods) before rolling the device-manager
DaemonSet, then let the workloads reschedule.

> **Why** — a Pod carrying the old shape on a restarted device manager **drops out of the ledger**: its
> accelerators read *free* while its containers hold them, and the next opposite-mode Pod can land on an
> occupied one. The rebuild cannot recover — the occupancy is exactly what became unreadable — so it
> logs loudly, naming the Pod.

## Limitations

- **Media-engine and graphics profile variants are not exposed.** A profile whose name is not a valid
  Kubernetes resource-name segment — the `+me`, `+me.all` and `+gfx` MIG variants — is **excluded** from
  an accelerator's inventory rather than rewritten to something key-safe, so a key always maps back to
  its profile by a plain prefix strip. Those variants cannot be requested.
- **One accelerator per slice or partition request** (rules 2 and 3), for the different reasons given
  above.
- **Hand-carving a partition outside GPUStack is unsupported on a managed node.** Every node-level key —
  per-profile capacity, partition token health, the admission check — comes from the Pod annotations the
  device plugin writes, and an instance made with `nvidia-smi mig -cgi` produces none. The node keeps
  advertising room it does not have, and unlike a transient over-advertisement this **never converges**:
  it persists until the instance is removed. Placement reads the live hardware and will not double-book
  it, but the accounting above stays wrong. Let GPUStack materialize the instances; it reuses any
  already on an accelerator it manages.
- **Flipping an accelerator's partitioning mode is an operational procedure, not a live switch.** An
  accelerator advertises a family's tokens only while its reported capability backs that family, so a
  flip *removes* the old tokens rather than marking them unhealthy — there is no continuity across it.
  Drain the accelerator (the hardware refuses the toggle under load anyway), flip the mode with the
  manufacturer's tool, then **restart that node's Device Manager DaemonSet**: the re-detect trigger
  watches the device set and health, not the partitioning mode. Deleting the node's `Devices` object is
  **not** required — an existing group's capability is rewritten in place. Full procedure: [NVIDIA MIG
  Operations](./operation/nvidia-mig.md#enabling-mig-on-a-node) · [T-Head MIG
  Operations](./operation/thead-ppu-partitioning.md#enabling-partitioning-on-a-node).
- **A non-default `TopologyManager` policy can mis-align a partition.** The Partitioned resource reports
  no NUMA topology (the plugin may not use the accelerator the kubelet aligned to), so under
  `single-numa-node` the CPU and memory providers can settle on one socket while the only accelerator
  with room is on the other. The default `none` is unaffected.

---

**See also** — [NVIDIA MIG Operations](./operation/nvidia-mig.md) (the administrator runbook for an
accelerator's partitioning mode, plus a recorded enable → request → reclaim → disable walkthrough) ·
[T-Head MIG Operations](./operation/thead-ppu-partitioning.md) (the same runbook for T-Head's own
partitioning) · [Admission](./architecture/admission.md) (where these keys are checked) ·
[Device Discovery](./architecture/device-discovery.md#the-device-plugin-allocator) (where they are served)

**Next** → [Walkthrough](./walkthrough.md) — the same requests on a live cluster.
