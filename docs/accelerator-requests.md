# Accelerator Requests

> **Purpose** — the normative request contract: every resource key a workload may set, the seven rules
> admission enforces, and a worked example per family.
> **Audience** users writing workloads, contributors touching the webhooks · **Prerequisites**
> [Architecture](./architecture.md) · **Read time** ~15 min

The normative contract for asking GPUStack for an accelerator: the resource keys a workload may set, the
seven rules the admission webhooks enforce, and a worked example per family. Every rule below is checked at
admission — a violating Pod is rejected at `CREATE`, never discovered at container start.

The rules bind the **Kueue-managed path**. Both Pod webhooks select on the `kueue.x-k8s.io/queue-name`
label, so a hand-written Pod without it bypasses every rule and every fold while still being able to request
the device-plugin resources. The contract is complete for managed workloads and advisory otherwise.

## Contents

- [Two families, two card populations](#two-families-two-card-populations)
- [The resource keys](#the-resource-keys)
- [Worked example per family](#worked-example-per-family)
- [The request rules](#the-request-rules)
- [Requesting through the Instance API](#requesting-through-the-instance-api)
- [Pre-release breaks](#pre-release-breaks)
- [Limitations](#limitations)

## Two families, two card populations

A card is shared in one of two physically incompatible ways, and GPUStack names them apart:

| Term | What it is | How isolation is enforced | Example |
|---|---|---|---|
| **Logical slicing** (`.sliced*`) | software slicing of a whole card | a vendor preload library caps compute and VRAM per container (NVIDIA HAMi-core `libvgpu.so`, Ascend vcann-rt `libvruntime.so`) | 50 % of an A10G |
| **Physical partitioning** (`.partitioned*`) | hardware partitioning of a card put into a partitioning mode | the hardware itself; the operator materializes the instance | an NVIDIA MIG `3g.40gb` |

The two never apply to the same card. A card in a partitioning mode advertises **only** the partition
family; an unpartitioned card advertises **only** the whole-card, shared and logical-slice families. That is
why a pool's `InstanceType` reports four separate views (`EX` / `SH` / `SL` / `PT`) instead of folding them:
each card feeds exactly one of them.

`<kind>` in the partition keys is the manufacturer's own name for hardware partitioning — `mig` for NVIDIA —
so an NVIDIA `3g.40gb` request reads `nvidia.com/gpu.partitioned.mig-3g.40gb`. A manufacturer with no
hardware partitioning has no kind, hence no `.partitioned*` keys at all.

## The resource keys

`<base>` is the manufacturer's device resource (`nvidia.com/gpu`, `huawei.com/npu`, … — see
[Accelerator support](../README.md#accelerator-support)).

| Key | Served by | Cards that serve it | Request value | Node value |
|---|---|---|---|---|
| `<base>` | device plugin (Exclusive) | unpartitioned only | card count | Σ healthy tokens |
| `<base>.shared` | device plugin (Shared) | unpartitioned only | ownership shares (10 per card) | Σ healthy tokens |
| `<base>.sliced` | device plugin (Sliced) | logically sliceable only | always `1` | Σ healthy tokens |
| `<base>.sliced.units` | node capacity | logically sliceable | **webhook-derived**, per card | Σ cards × 1,600,000 |
| `<base>.sliced.cores-percentage` | node capacity | logically sliceable | per card, `(0,100]` | Σ per-card budget |
| `<base>.sliced.memory-percentage` | node capacity | logically sliceable | per card, `(0,100]` | Σ per-card budget |
| `<base>.sliced.memory-mib` | node capacity | logically sliceable | per card, ≤ card VRAM | Σ per-card budget |
| `<base>.partitioned` | device plugin (Partitioned) | partitioned only | always `1` | Σ healthy tokens |
| `<base>.partitioned.units` | node capacity | partitioned | **webhook-derived** | Σ cards × 1,600,000 |
| `<base>.partitioned.<kind>-<profile>` | node capacity | partitioned | always `1` | Σ (allocated + remaining) |
| `device.gpustack.ai/<manufacturer>.visibility` | device plugin (Visibility) | every card | sidecar's card count | Σ tokens |

Two notes a reader will not guess:

- **The `.units` keys are never written by hand.** The Pod webhook recomputes them from the memory budget
  (logical) or from the profile's VRAM (partition) and overwrites any client-supplied value. They are the
  credit-counting input Kueue's `credits` transformation reads; a partition and a logical slice of the same
  VRAM therefore cost the same credits.
- **The two token shapes differ.** `<base>`, `.shared` and `.sliced` tokens are *card-bound* — the token the
  kubelet picks **is** the card selection. `.partitioned` and visibility tokens are a *fungible count*: the
  device plugin chooses the card itself, against the live partition geometry, and records the card it
  actually used. So a partition request never lands on a card that cannot host its profile, and a rejection
  from `Allocate` means the whole node has no room.

## Worked example per family

All four are Pods submitted on a pool's entrance `LocalQueue` (`kueue.x-k8s.io/queue-name`).

**Exclusive** — two whole cards:

```yaml
resources:
  limits:
    nvidia.com/gpu: "2"
```

**Shared** — 3 of a card's 10 ownership shares:

```yaml
resources:
  limits:
    nvidia.com/gpu.shared: "3"
```

**Logical slice** — half of one card's VRAM, capped at 40 % of its compute:

```yaml
resources:
  limits:
    nvidia.com/gpu.sliced: "1"                       # always exactly 1 card
    nvidia.com/gpu.sliced.memory-percentage: "50"    # per card; or .sliced.memory-mib, never both
    nvidia.com/gpu.sliced.cores-percentage: "40"     # per card; defaults to 100 when omitted
    # nvidia.com/gpu.sliced.units is folded by the webhook — do not set it
```

**Physical partition** — one MIG `3g.40gb` instance:

```yaml
resources:
  limits:
    nvidia.com/gpu.partitioned: "1"                  # always exactly 1 card
    nvidia.com/gpu.partitioned.mig-3g.40gb: "1"      # always exactly 1 instance
    # nvidia.com/gpu.partitioned.units is folded by the webhook — do not set it
```

**The SSH sidecar** is deliberately outside the four accelerator families, so none of the family rules
count it. It requests the internal visibility resource for the card count its workload container holds:

```yaml
resources:
  limits:
    device.gpustack.ai/nvidia.visibility: "1"
```

## The request rules

The rules are scoped by a container's **lifetime group**, not by the field it sits in:

- the **init group** is `spec.initContainers` *without* `restartPolicy: Always`;
- the **running group** is `spec.containers`;
- a **native sidecar** — a `spec.initContainers` entry *with* `restartPolicy: Always` — belongs to neither.
  It starts during the init phase and keeps running, so it overlaps every later init container as well as
  every app container.

### Rule 1 — one family, in exactly one container group

*All containers of a Pod request the same accelerator family, and the accelerator claims sit in exactly one
container group.*

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
    - name: a
      resources: { limits: { nvidia.com/gpu: "1" } }                  # exclusive
    - name: b
      resources: { limits: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.memory-percentage: "50" } }
```

> `spec: Forbidden: a Pod may request only one accelerator family, found [exclusive sliced]`

Rejected — the same family claimed in both container groups:

```yaml
spec:
  initContainers:
    - name: warmup
      resources: { limits: { nvidia.com/gpu: "1" } }
  containers:
    - name: main
      resources: { limits: { nvidia.com/gpu: "1" } }
```

> `spec.initContainers: Forbidden: a Pod's accelerator requests must all sit in one container group;
> spec.initContainers must give up its request, because its devices are held for the Pod's whole life while
> the scheduler charges the Pod only once`

**Why an accelerator family may appear in only one container group.** The two claims are not free of each
other, for two independent reasons:

1. *Nothing releases the earlier claim.* The kubelet keeps a finished init container's devices in its Pod
   record for the Pod's whole life, and GPUStack's own reclaimer destroys a partition on **Pod** deletion,
   not on container termination. The second claim therefore coexists with the first rather than succeeding
   it.
2. *The scheduler charges the Pod only once.* A Pod's demand for a key is `max(Σ init, Σ app)`, so two
   same-family claims cost **one** unit of quota while consuming **two** cards. The node then
   over-advertises by exactly one slot per such Pod, and the *next* tenant fails terminally.

Confining the claims to one group makes the charge and the consumption agree by construction. If a Pod
genuinely needs a device in two phases, keep the claim on the app container and let the init container run
without one — the app container's device is the same hardware the init container would have held.

### Rule 2 — `<base>.sliced` is exactly 1

Accepted: `nvidia.com/gpu.sliced: "1"`. Rejected:

```yaml
resources:
  limits:
    nvidia.com/gpu.sliced: "2"
    nvidia.com/gpu.sliced.memory-percentage: "50"
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.sliced]: Invalid value: "2": a logical slice request is
> always a single card; multi-card logical slicing is not supported yet`

This is a **deferral, not a manufacturer limit** — NVIDIA's logical slicing can isolate more than one card.
It is capped at 1 because no node-level key expresses "N *distinct* cards": an additive scalar cannot
separate "two cards at half each" from "one card in full", so a one-card node would accept a two-card
request and then fail it terminally at `Allocate`. Lifting the cap needs a node-level card-count dimension.
A multi-card workload asks for several Pods, or for whole cards.

A logical slice must also name exactly one memory budget:

- neither `.sliced.memory-percentage` nor `.sliced.memory-mib` → `Required value: a nvidia.com/gpu.sliced
  request must set nvidia.com/gpu.sliced.memory-percentage or nvidia.com/gpu.sliced.memory-mib`;
- both → `Forbidden: cannot set both …memory-percentage and …memory-mib`;
- a percentage outside `(0,100]`, a non-positive MiB value, or a MiB value above the card's VRAM → rejected.

### Rule 3 — `<base>.partitioned` is exactly 1

Accepted: `nvidia.com/gpu.partitioned: "1"`. Rejected:

```yaml
resources:
  limits:
    nvidia.com/gpu.partitioned: "2"
    nvidia.com/gpu.partitioned.mig-1g.10gb: "1"
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.partitioned]: Invalid value: "2": a partition request
> is always a single card; request one Pod per instance`

Unlike rule 2 this one is a **scope decision**: the plugin picks the card itself, so `N > 1` would be
implementable. No workload needs it yet, so a multi-partition workload asks for several Pods.

A bare card key with no profile is also rejected — there is no hardware shape to actuate:

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
resources:
  limits:
    nvidia.com/gpu.partitioned: "1"
    nvidia.com/gpu.partitioned.mig-3g.40gb: "2"
```

> `spec.containers[0].resources.limits[nvidia.com/gpu.partitioned.mig-3g.40gb]: Invalid value: "2": a
> partition profile request must be exactly 1 instance`

### Rule 6 — at most one container may request a slicing family

Logical or physical, one claiming container per Pod. Accepted:

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
    - name: a
      resources: { limits: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.memory-percentage: "50" } }
    - name: b
      resources: { limits: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.memory-percentage: "50" } }
```

> `spec: Forbidden: at most one container may request a slicing family, found 2`

The SSH sidecar is unaffected: the visibility resource sits deliberately outside the accelerator families.

### Rule 7 — a restartable init container may not request an accelerator

Accepted — the claim sits on an app container while the native sidecar carries none:

```yaml
spec:
  initContainers:
    - name: log-shipper
      restartPolicy: Always
      resources: { limits: { cpu: "100m" } }
  containers:
    - name: main
      resources: { limits: { nvidia.com/gpu: "1" } }
```

Rejected:

```yaml
spec:
  initContainers:
    - name: log-shipper
      restartPolicy: Always
      resources: { limits: { nvidia.com/gpu: "1" } }
```

> `spec.initContainers[0].resources.limits: Forbidden: a restartable init container (a native sidecar) may
> not request an accelerator; move the request to an app container`

A native sidecar belongs to neither container group, so its claim would overlap every later init container
*and* every app container — the exact double-consumption rule 1 exists to prevent, with no group to move it
out of.

## Requesting through the `Instance` API

An `Instance` expresses the same four families through `spec.resources`, and the controller shapes them into
the keys above:

| Field | Effect |
|---|---|
| `accelerator: "N"` | whole cards (exclusive) — may span cards |
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

The webhook rejects, at admission rather than leaving the Instance Pending:

- a profile the pool does not offer — the message lists the offered set;
- a profile on a manufacturer with no hardware partitioning;
- a profile **and** a slice percentage together (`a hardware partition and a logical slice percentage are
  mutually exclusive`);
- an `accelerator` count other than `1` for a slice or a partition request;
- **a slice percentage against a pool that offers no logical slicing.** This is a tightening: such a request
  used to be silently reshaped into a whole-card request and served as one. On an all-partitioned pool the
  message points at `spec.resources.acceleratorPartitionedProfile` instead.

A partition request's host CPU and RAM are sized by the profile's share of the card's VRAM, so a `1g`
instance does not ask for a whole card's worth of CPU and RAM.

## Pre-release breaks

No released version is marked stable, so both of the following are clean breaks with **no** translation
layer.

**The old MIG key is gone.** Before the split, a MIG profile was requested through the *logical* slicing
family: a per-profile key built from `<base>.sliced` with a `mig-<profile>` segment appended, alongside
`<base>.sliced: 1`. That key has been replaced by `<base>.partitioned.<kind>-<profile>` alongside
`<base>.partitioned: 1`, and nothing in the project recognizes the old one any more — not the builders, not
the parsers, not the node-capacity reconciler, and deliberately **not even a rejection by name**. Paying for
that legibility would mean every key path carrying a legacy branch to serve a request no documentation, no
`InstanceType` and no example produces.

A Pod still carrying the old shape meets two ordinary failures instead: its per-profile key is an extended
resource no node advertises, so it never schedules; and the `<base>.sliced: 1` beside it is a logical slice
request with no memory budget, which rule 2's budget check rejects at admission. Neither failure names the
replacement key. Rewrite such a manifest to the partition keys above.

A development node an earlier build wrote those legacy per-profile capacities onto keeps them, because
nothing owns them any longer. List what is left and patch it off — one JSON-patch removal per stale key:

```console
$ kubectl get node <node> -o json | jq -r '.status.capacity | keys[] | select(contains("mig-"))'
$ kubectl patch node <node> --subresource=status --type=json \
    -p '[{"op":"remove","path":"/status/capacity/<escaped-key>"}]'   # escape "/" in the key as "~1"
```

**The allocation annotation's value changed too, so drain a node before upgrading its device manager.**
The device plugin records each allocation in the Pod annotation
`device.gpustack.ai/accelerator.allocated`, whose value is now a per-container map. The old flat shape is
not read. A Pod carrying the old shape on a node whose device manager has restarted **drops out of the
ledger**: its cards read *free* while its containers still hold them, and the next opposite-mode Pod can
land on an occupied card. The rebuild cannot recover from this — the occupancy is exactly what became
unreadable — so it logs loudly, naming the Pod. Drain the node (or delete its accelerator Pods) before
rolling the device-manager DaemonSet, then let the workloads reschedule.

## Limitations

- **Media-engine and graphics profile variants are not exposed.** A profile whose name is not a valid
  Kubernetes resource-name segment — the `+me`, `+me.all` and `+gfx` MIG variants — is **excluded** from a
  card's inventory rather than rewritten to something key-safe, so a key always maps back to its profile by
  a plain prefix strip. Those variants cannot be requested.
- **One card per slice or partition request** (rules 2 and 3), for the different reasons given above.
- **Hand-carving a partition outside GPUStack is unsupported on a managed node.** The operator's ledger owns
  a managed card's geometry, and every node-level key — the per-profile capacity, the partition token health
  and the admission check — is derived from the Pod annotations the device plugin writes. An instance an
  administrator created with `nvidia-smi mig -cgi` produces no annotation, so it is invisible to all of
  them: the node keeps advertising room it does not have, and unlike a transient over-advertisement this
  **never converges** — it persists until the instance is removed. Placement itself reads the live hardware
  and so will not double-book such an instance, but the accounting above it stays wrong. Let GPUStack
  materialize the instances; it reuses any that already exist on a card it manages.
- **Flipping a card's partitioning mode is an operational procedure, not a live switch.** A card advertises
  a family's tokens only while its reported capability backs that family, so a flip *removes* the old
  family's tokens rather than marking them unhealthy — there is no continuity across it. **Drain the card
  first** (the hardware refuses the toggle while compute is running anyway), flip the mode with the vendor
  tool, then **restart that node's Device Manager DaemonSet**: the detect loop's re-detect trigger watches
  the device set and health, not the partitioning mode, so nothing else picks the change up. Deleting the
  node's `Devices` object is **not** required — an existing group's capability is rewritten in place. The
  NVIDIA procedure end to end is in
  [NVIDIA MIG Operations](./operation/nvidia-mig.md#enabling-mig-on-a-node).
- **A non-default `TopologyManager` policy can mis-align a partition.** The Partitioned resource reports no
  NUMA topology (the plugin may not use the card the kubelet aligned to), so under `single-numa-node` the
  CPU and memory providers can settle on one socket while the only card with room is on the other. The
  default policy `none` is unaffected.

---

**See also** — [NVIDIA MIG Operations](./operation/nvidia-mig.md) (the administrator runbook for a
card's partitioning mode, plus a recorded enable → request → reclaim → disable walkthrough) ·
[Admission](./architecture/admission.md) (where these keys are checked) ·
[Device Discovery](./architecture/discovery.md#the-device-plugin-allocator) (where they are served)

**Next** → [Walkthrough](./walkthrough.md) — the same requests on a live cluster.
