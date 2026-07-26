# Spec: Split Logical and Physical Slicing into Distinct Resource Families

Status: Building
Type: Feature

## Summary

GPUStack advertises a single accelerator "sliced" resource family (`<vendor>/<device>.sliced`,
`.sliced.units`, `.sliced.cores-percentage`, `.sliced.memory-percentage`, `.sliced.memory-mib`,
`.sliced.mig-<profile>`) for two physically incompatible things: **logical slicing** — software
injection on an unpartitioned card — and **physical slicing** — hardware partitioning of a card put
into a partitioning mode (NVIDIA MIG). Because both share one family, the device-plugin advertises one
bare `.sliced` token pool spanning both card populations. On a node whose cards are mixed, the kubelet
can hand a partition request a token belonging to a card that is not partitionable, the partition
cannot be created, and the Pod dies with a terminal `UnexpectedAdmissionError`.

This spec splits the two apart. `.sliced*` becomes logical slicing only; a new `.partitioned*` family,
served by a new `Partitioned` allocation mode, expresses physical slicing. Each family's token pool is
advertised only by the cards that can serve it, which makes the kubelet's card pick correct by
construction rather than by an advisory hint. In the same pass the codebase adopts one vocabulary
(*logical slicing* replaces "soft slicing"; *physical slicing* / *partitioning* names the hardware
concept), the InstanceType table view grows from three accelerator columns to four (`EX/SH/SL/PT`), the
request rules become one normative set enforced at admission, and a partition becomes requestable
through the `Instance` API.

## Motivation

### Goals

1. **One vocabulary.** "soft slicing" / "soft sliced" / "soft slice" become "logical slicing" /
   "logically sliced" / "logical slice" across live code, comments, tests, docs, chart templates,
   Dockerfiles and e2e cases. Archived specs are historical records and are not rewritten.
2. **Correct card selection on a mixed node.** A partition request must never be placed on a card that is
   not partitionable, and a logical-slice request never on a partitioned card. For the logical family that
   is achieved on the kubelet's own selection path; for partitions the plugin chooses the card itself, so
   the guarantee holds by construction and a rejection can only mean the node is full.
3. **The right primitive per exclusion.** Exclusive, Shared and logical Sliced share one physical
   substrate: every unpartitioned card can serve all three, so nothing in the hardware prevents a
   cross-mode co-location and **health** is the correct mechanism. Physical partitioning is *physically*
   exclusive with those three, so the correct mechanism there is **advertisement scope**: only
   partitionable cards advertise `.partitioned`, only unpartitioned cards advertise `<base>`, `.shared`
   and `.sliced`. Health then covers nothing further for the partition family — its card is chosen by
   `Allocate` (F2), so its pool health is a node-level count of remaining room, published over a set of
   token IDs stable enough for the kubelet's own re-admission check.
4. **Separate accounting, one card counted once.** Kueue credits, node capacity, the admission check and
   the InstanceType views count logical slices and physical partitions on separate keys, and every one of
   those consumers scopes a card to exactly one family.
5. **Four accelerator views** — `EX` (exclusive), `SH` (shared), `SL` (logical), `PT` (physical).
6. **One normative set of request rules**, enforced in both webhooks and written into the docs, instead of
   partly enforced at admission, partly discovered at runtime by a vendor responder, partly not at all.
7. **A partition is requestable through the `Instance` API**, so the new `PT` view describes capacity a
   first-class GPUStack workload can consume.

**Measurable success criteria**

- On a node mixing partitioned and unpartitioned cards of one model, **once the node's slicing capability
  has converged**, a partition request lands on a partitionable card and a logical-slice request on an
  unpartitioned card in 100% of placements, with no `UnexpectedAdmissionError` from a family mismatch.
  Inside the pre-convergence window the previous family's tokens are still healthy, so the guarantee is
  scoped to the converged state.
- A partition request whose offered tokens all name a card that cannot host its profile still runs, on a
  card that can; `Allocate` rejects only when no card on the node has room.
- Two Pods pending on one node that differ only in their requested profile — or only in a percentage the
  vendor responder reads — each get **their own** request actuated; two candidates that are equal on every
  response-affecting dimension are interchangeable and the oldest is still chosen.
- A re-detected capability actually lands in the node's `Devices` object: an existing group's slicing
  capability is rewritten **in place**, with no need to delete the object (F16).
- A node advertises `allocated + remaining` healthy `.partitioned` tokens, so the scheduler's view of free
  slots equals `remaining` — a node with no room left has a free view of zero while allocatable still equals
  its live instance count. Partitions already running keep running and survive a kubelet restart, including
  one whose container is stopped at restart time, because every ID recorded against a live allocation stays
  Healthy for that allocation's life.
- A node's `.partitioned.<kind>-<profile>` value never exceeds what its own cards can still host: after
  one `3g.40gb` is carved on the node's only partitioned card, the `7g.80gb` key reads zero.
- `<base>.sliced`, `<base>.partitioned` and every per-profile key are exactly `1`; a request naming more is
  rejected at admission, the sliced one naming the deferral rather than a manufacturer limit.
- A Pod claiming an accelerator family in both container groups is rejected at admission, naming the group
  that must give it up — so no Pod consumes two cards against one unit of quota.
- An exclusive or shared request is never judged feasible against a partitioned card — neither by the
  admission check nor by the `EX`/`SH` views — so it is rejected or left queued instead of being admitted
  into a permanent `Pending`.
- A restartable init container requesting any accelerator family is rejected.
- A legacy `.sliced.mig-<profile>` request is rejected at admission with the replacement key named, rather
  than failing later with a message about a missing memory budget.
- `grep -riE 'soft[ _-]?slic' pkg/ api/ docs/ deploy/ pack/ testing/ README.md .claude/skills/` returns
  nothing, and `grep -rn '\.sliced\.mig-' pkg/ api/ docs/ .claude/skills/` returns only the
  removal-only recognition site and the legacy guard.
- `kubectl get instancetypes` prints an `Accelerator(EX/SH/SL/PT)` column with four groups.
- Every request rule is rejected-when-violated by a webhook unit test in both directions, and all are
  stated in the docs with an accepted and a rejected example.
- An `Instance` with `AcceleratorPartitionedProfile` set runs on a partition-offering pool.
- `make lint` and `make test` pass; the operator e2e suite passes, including a new mixed-node case.

### Non-Goals

- **Per-profile device-plugin resources.** Making `<base>.partitioned.<kind>-<profile>` an actual
  device-plugin resource would let the kubelet's own accounting close the node-level window of residual 3.
  Deferred (see Alternatives); per-profile counting stays at node level.
- **Multi-card partitions.** `<base>.partitioned` is capped at 1 (rule 3); a multi-partition workload asks
  for several Pods. This is a scope decision, not a limitation — placement authority (F2) would make `N > 1`
  implementable, since the plugin can pick N distinct cards itself.
- **Multi-card logical slicing.** `<base>.sliced` is capped at 1 for every manufacturer (rule 2), NVIDIA
  included. Unlike the partition cap this one is a *limitation*, not just scope: no node-level key expresses
  "N distinct cards", so a one-card node would accept a two-card request and fail it terminally at
  `Allocate`. Lifting it needs a node-level card-count dimension (Alternatives), which also decides whether
  the `.units` keys become total-valued.
- **Accelerator claims in more than one container group.** Rule 1 confines them to one group. The
  scheduler charges `max(Σ init, Σ app)` per key while two claims consume two cards, so allowing both groups
  to claim would over-advertise the node by exactly one slot per Pod.
- **Placement authority for the other three families.** Only `.partitioned` chooses its own card; see
  Alternatives for the symmetric change and why it is not taken here.
- **Renaming API types or JSON/protobuf fields.** `AcceleratorLogicalSliced`, `AcceleratorPhysicalSliced`,
  `logicalSliced`, `physicalSliced`, `AllocatedPhysicalProfile` and friends already read as
  logical/physical and stay as they are — no CRD churn, no protobuf renumbering, no applyconfiguration
  renames. Exactly two **additive** fields are added, each with a new protobuf number:
  `InstanceTypeStatus.AcceleratorPartitioned` and `InstanceResources.AcceleratorPartitionedProfile`
  (`api/worker/v1` defines `Instance` as `workercore.Instance`, so the field is added once).
- **Media-engine and graphics profile variants.** `+me`, `+me.all`, `+gfx` stay unexposed — they are
  already dropped when a card's profile inventory is built; this spec turns that into a stated limitation.
- **Relaxing the profile-shape rule.** A request still names exactly one profile shape.
- **New vendors' hardware partitioning.** The partition kind and key family are vendor-parameterized so
  Ascend vNPU or AMD compute partitioning can register without a redesign; only NVIDIA MIG is implemented.
- **Changing the unit basis, credit base, or the Kueue quota shape.** The denominator, the credit base and
  "the ClusterQueue covers only the credits resource" are unchanged.
- **Migrating in-flight workloads.** A pre-release key rename is a clean break; Pods requesting the old
  keys are not translated.

## Proposal

### The two families after the split

`<kind>` is the manufacturer's own name for hardware partitioning — `mig` for NVIDIA — so an NVIDIA
3g.40gb request reads `nvidia.com/gpu.partitioned.mig-3g.40gb`. A manufacturer with no partitioning has
no kind, hence no `.partitioned*` keys and no Partitioned server.

| Key | Advertised by | Population it serves | Request value | Node value |
| --- | --- | --- | --- | --- |
| `<base>` | device-plugin (Exclusive) | **unpartitioned cards only** | card count | Σ healthy tokens |
| `<base>.shared` | device-plugin (Shared) | **unpartitioned cards only** | ownership shares | Σ healthy tokens |
| `<base>.sliced` | device-plugin (Sliced) | **logically sliceable cards only** | always `1` | Σ healthy tokens |
| `<base>.sliced.units` | node capacity | logically sliceable cards | **per card** | Σ cards × D |
| `<base>.sliced.cores-percentage` | node capacity | logically sliceable cards | **per card** | Σ per-card budget |
| `<base>.sliced.memory-percentage` | node capacity | logically sliceable cards | **per card** | Σ per-card budget |
| `<base>.sliced.memory-mib` | node capacity | logically sliceable cards | **per card** | Σ per-card budget |
| `<base>.partitioned` | device-plugin (**Partitioned**, new) | **partitioned cards only** | always `1` | Σ healthy tokens |
| `<base>.partitioned.units` | node capacity | partitioned cards | one card's units | Σ cards × D |
| `<base>.partitioned.<kind>-<profile>` | node capacity | partitioned cards | always `1` | Σ (allocated + remaining) |
| `device.gpustack.ai/<manufacturer>.visibility` | device-plugin (Visibility) | every card | sidecar count | Σ tokens |

A card advertises tokens only for the family its current capability reports, so a capability flip
*removes* that family's tokens rather than marking them unhealthy. Continuity across a flip is therefore
an **operational** property — drain the card first, which the hardware already requires — not a mechanism.

```yaml
# Physical: one 3g.40gb instance on one partitioned card.
resources:
  limits:
    nvidia.com/gpu.partitioned: "1"                  # always 1 card
    nvidia.com/gpu.partitioned.mig-3g.40gb: "1"      # always 1
    nvidia.com/gpu.partitioned.units: "800000"       # the card's units, folded by the Pod webhook
---
# Logical: half of one card. Every manufacturer requests exactly 1 card.
resources:
  limits:
    nvidia.com/gpu.sliced: "1"                       # always 1 card
    nvidia.com/gpu.sliced.cores-percentage: "50"     # per card
    nvidia.com/gpu.sliced.memory-percentage: "50"    # per card
    nvidia.com/gpu.sliced.units: "800000"            # per card, folded by the Pod webhook
```

### User Stories

#### Story 1
As a platform operator running a node whose GPUs are deliberately mixed — some cards partitioned for
small tenants, the rest left whole for logical slicing — I want a partition request to always land on a
partitioned card and a logical-slice request always on an unpartitioned card, so that my tenants stop
hitting `UnexpectedAdmissionError` Pods that never recover.

#### Story 2
As a tenant requesting a hardware partition, I want the resource key to say what I am asking for
(`nvidia.com/gpu.partitioned.mig-3g.40gb`) rather than hiding a hardware partition inside the
software-slicing family, so that reading a Pod spec tells me which kind of GPU sharing I got.

#### Story 3
As a platform operator, I want a partitioned card whose geometry is fully consumed to stop attracting new
partition Pods immediately — not after the next reconcile — so that the kubelet spreads partitions across
cards instead of piling them onto one.

#### Story 4
As a platform operator reading `kubectl get instancetypes`, I want four accelerator columns with the
partition view separate from the logical view, so that a partition-only pool does not report a logical
percentage capacity it cannot serve.

#### Story 5
As a contributor, I want one vocabulary across code, comments, docs and e2e cases, so that I do not have
to learn that "soft", "logical" and "sliced" are the same thing while "sliced.mig" is not.

#### Story 6
As the maintainer adding a second vendor's hardware partitioning, I want the partition kind and the key
family to be vendor-parameterized, so that Ascend vNPU registers its own name instead of shipping under
NVIDIA's.

### Core Features & Acceptance Criteria

**F1 — a card serves only the families its capability reports, in every consumer.**

| Card state | `<base>` | `<base>.shared` | `<base>.sliced` | `<base>.partitioned` | visibility |
| --- | --- | --- | --- | --- | --- |
| not partitioned | ✅ 1 token | ✅ shares | ✅ logical slice count | — none | ✅ |
| partitioned | — none | — none | — none | ✅ partition ceiling | ✅ |

The Sliced pool is sized from the card's *logical* slice count alone, with no fallback to a partition
ceiling; the Partitioned pool from the card's partition ceiling (the largest instance count over its
profiles). Both are zero for a card in the other state, and a zero-sized pool advertises no IDs.

The two families' tokens differ in what they *mean*. `<base>`, `.shared` and `.sliced` tokens name the card
they belong to and therefore **are** the kubelet's card selection. `.partitioned` tokens — like Visibility's
— are a fungible count, and the plugin selects the card itself (F2). Both shapes are sized from the
population that can serve them, so the observable contract is the same either way: allocatable per family
equals that population's capacity. What the split buys is therefore **node**-level, not card-level: the
resource name is the only thing the scheduler filters on, and a node's inability to serve a kind is the one
placement error `Allocate` cannot repair — it can only reject.

The same population rule binds the two consumers that ignore it today, and they are part of this feature
rather than a follow-up: the admission check's scalar path currently filters partitioned cards for the
*Sliced* mode only, and the InstanceType `EX`/`SH` views count every card with `Remaining ≥ D` as
whole-card-free — and an empty partitioned card's `Remaining` starts at `D`. Left alone, the split would
convert an exclusive tenant's broken container into a Pod that is admitted, unschedulable, and `Pending`
forever, and F9's rejection of an exclusive request against an all-partitioned pool would have no data
source. For the three card-bound families, health remains the mechanism for the one thing it can express on
a card that *is* capable of the family: a cross-mode hold, where tokens stay advertised and are merely
Unhealthy. The Partitioned family does not need it — its `Allocate` chooses the card (F2), so its health is
a pure node-level count (F5). Visibility is healthy on every card: the SSH sidecar must co-allocate
whatever its workload holds.

*Accept:* on a node with N unpartitioned and M partitioned cards of one model, `<base>` allocatable is N,
`<base>.shared` is N × shares, `<base>.sliced` is Σ logical slice counts over the N, and
`<base>.partitioned` is Σ partition ceilings over the M; an exclusive, shared or logical-slice Pod is
never assigned a token from any of the M cards and a partition Pod never from the N; the admission check
and the `EX`/`SH`/`SL` views each exclude the M; with M = all cards the three logical/whole-card families
advertise nothing and the `.sliced.*` counting keys are absent; the docs state the drain-before-flip
requirement, and no acceptance criterion claims survival across an undrained flip.

**F2 — a new `Partitioned` allocation mode whose `Allocate` places the instance itself.**
`DeviceAllocationModePartitioned` is added and its resource name is `<base>.partitioned`. Unlike the three
card-bound families, its `Allocate` takes the kubelet's device IDs as a **quantity** and ignores which card
they name: it chooses the card itself — under the existing node allocate mutex, against the live partition
state — actuates the instance there, and records the card it actually used in the durable annotation. The
kubelet does not verify that an `AllocateResponse` corresponds to the IDs it offered, and the Visibility
family already relies on that, so this is an existing property of the plugin rather than a new liberty.

Placement authority is what makes the mixed-node guarantee *constructive* rather than retried: the
kubelet's pick can no longer send a partition to a card that cannot host its profile, and two Pods
scheduled for mutually exclusive profiles are placed on different cards whenever the node has them. A
rejection then means what it says — **the node** has no room — which is exactly what the node-level keys of
F3 are responsible for. The placement selector reads the live source (the ledger, plus NVML through the
existing actuator seam, since `pkg/deviceplugin` must not import vendor CGO), so an instance an
administrator created out of band is not double-booked *by placement* — though it remains invisible to the
node-level keys, which are annotation-derived (residual 2).

Two `Allocate` calls can otherwise choose the same card: the mutex today covers identify → cross-mode check
→ reserve, while actuation happens after it is released. The selection is therefore **published into the
reservation — card, profile and the memory-slice intervals it intends to occupy — before the mutex is
released**, so the next caller selects against post-decision state rather than against a card that merely
has not been carved yet.

The mode also gets an **explicit branch in the per-token ledger cost** charging the partition's own folded
units — never the whole-card amount the `switch`'s `default` branch applies to any newly added mode.
*Accept:* `<base>.partitioned` allocatable equals Σ partition ceilings over the partitioned cards; a node
with no partitioned card reads zero; the mode string round-trips as `Partitioned`; a request whose offered
tokens all name a card that cannot host the requested profile still runs, on a card that can; two
concurrent requests for mutually exclusive profiles land on different cards when the node has two;
`Allocate` rejects only when no card on the node can host the profile, and says so; the annotation records
the card actually used, not the one the kubelet offered; a single small partition consumes only its own
units in the per-card ledger — asserted by a ledger-cost fixture distinct from the token-set fixture,
because a token-set test cannot observe it.

**F3 — accounting moves off the logical keys: a geometry-aware per-profile key and its own credits input.**

*The `.units` keys stay per-card, unchanged.* A per-card value under-subtracts an N-card request by (N−1)
cards' worth of the node's budget, because the scheduler subtracts each extended-resource key exactly once
— but with `<base>.sliced` capped at 1 (rule 2) and `<base>.partitioned` always 1, N is always 1 and the
per-card value *is* the total. The Kueue transformation therefore keeps its `multiplyBy`, and nothing about
the existing logical accounting moves. Multi-card logical slicing is deferred precisely because closing that
gap needs more than a total-valued key: no additive scalar can distinguish "two cards at half each" from
"one card in full", so admitting `N > 1` safely needs a node-level *card-count* dimension, which is a
key-set change of its own (Non-Goals). `.partitioned.units` counts a partitioned card at whole-card value
(partitioned cards × the denominator), exactly as `.sliced.units` values a logically sliceable card.

*The per-profile key stops being a static ceiling.* Its request value is always `1`; its node value is,
summed over the node's partitioned cards, **`allocated + remaining` for that profile** as the
placement-aware ledger reports them. The `allocated` term is not a rounding detail: the scheduler's fit
test is *allocatable minus the requests of Pods already on the node*, so publishing bare `remaining` would
subtract every existing instance twice. On an 80 GB card holding one `3g.40gb` (4 of 8 memory slices):

| key | static ceiling (today) | bare remaining | `allocated + remaining` |
| --- | --- | --- | --- |
| `7g.80gb` | 1 − 0 = **1** — geometry says no | 0 − 0 = 0 ✅ | (0+0) − 0 = **0** ✅ |
| `3g.40gb` | 2 − 1 = 1 ✅ | 1 − 1 = **0** — one more does fit | (1+1) − 1 = **1** ✅ |
| `1g.10gb` | 7 − 0 = **7** — only 4 slices free | 4 − 0 = 4 ✅ | (0+4) − 0 = **4** ✅ |

Both terms already exist in the ledger the reconciler rebuilds each pass, so this is arithmetic with no
device access, and it degrades to the static ceiling on an empty node.

Two mechanics to carry. The reconciler must now watch the ledger's **Status** side, which it deliberately
ignored to avoid allocation churn, so partition allocate/release re-patches that node's capacity, bounded
by the existing coalescing window. And the presence gate stays on **capacity**, not allocatable: after F1
a family's capacity is already zero on a node whose cards cannot serve it (the kubelet's capacity is
healthy + unhealthy), while allocatable *also* falls to zero whenever the family is merely saturated or
held in another mode — gating on that would delete `.units` and the per-profile keys while instances are
live, then re-add them on release. Deliberately **not** done: a second writer for
`Node.Status.Allocatable`; the kubelet derives an extended resource's allocatable from its capacity on
every status sync, so such a writer would be overwritten and flap.

Credits are computed per family from the units key alone, one transformation each:

```yaml
transformations:
  - input: <base>                        # exclusive → B credits per card
  - input: <base>.shared                 # shared    → B/10 per ownership share
  - input: <base>.sliced.units           # logical   → B/D per unit, multiplyBy <base>.sliced
  - input: <base>.partitioned.units      # physical  → B/D per unit, always one card    (NEW)
```

The two coarse token keys still pass through the Workload unconsumed and are ignored by
`quotaCheckStrategy: IgnoreUndeclared`.
*Accept:* the rendered Kueue values carry four transformations per manufacturer; a logical Workload's
credits are byte-identical to today for the same demand; a partition Workload's credits equal its profile
units; on a partition-only pool `.sliced.units` is absent and `.partitioned.units` equals partitioned cards
× the denominator; on a mixed pool both are present and no card is counted by both; the three-row table
above reproduces exactly on a one-card fixture; the credits factor stays integer-valued so Kueue's int64
quantization never rounds a fraction up.

**F4 — vendor-parameterized per-profile keys, with a name-validity guard instead of a sanitizer.**
`<base>.partitioned.<kind>-<profile>` replaces `<base>.sliced.mig-<profile>`. `<kind>` comes from a
per-manufacturer map (NVIDIA → `mig`) overridable by `GPUSTACK_<MANUFACTURER>_PARTITION_KIND`. Profile
names are used verbatim: Kubernetes resource names forbid `+`, so a profile whose name is not already
key-safe is **excluded** from the inventory — as the `+me` / `+me.all` / `+gfx` variants already are —
never rewritten. A key therefore maps back to its profile by plain prefix strip, and no reverse
resolution exists anywhere.
*Accept:* `3g.40gb` yields `nvidia.com/gpu.partitioned.mig-3g.40gb` and parses back by prefix strip;
`.partitioned.units` is never parsed as a profile; a manufacturer with no kind produces no per-profile
key; a name outside the qualified-name grammar is dropped with a log line rather than emitted; every
generated key passes resource-name validation including the 63-character limit after `/`; the variant
exclusion is documented as a limitation in the MIG guide.

**F5 — partition token health is a node-level count over a stable set of IDs.**
Because the Partitioned family's tokens are fungible at *selection* time (F2), health only has to answer
*how many* more instances the node can host, never which card a token names. The healthy count is, summed
over the node's partitioned cards, `allocated + remaining`, where a card's `remaining` is the largest number
of further instances it can host over its profiles; the rest of the ceiling is advertised Unhealthy.

- The `allocated` term is required: the kubelet publishes the healthy count as allocatable and the
  scheduler then subtracts the requests of Pods already on the node, so bare `remaining` would lose one
  slot per live instance. A node with no room left therefore advertises exactly its live instance count,
  which the scheduler reduces to a free view of zero — "full" is read from the free view, not from
  allocatable reaching zero.
- Token IDs are **never removed** — removal strands the kubelet's checkpointed allocations.
- **Fungible to the selector is not fungible to the kubelet.** The kubelet checkpoints the exact IDs it
  offered and, on any later allocation for that container, refuses to proceed unless every one of them is
  still in the healthy set — the check runs before the "no new devices needed" shortcut, and
  `sanitizeNodeAllocatable` does not rescue it. So the healthy **set** is part of the contract, not just its
  size: the IDs recorded against a live allocation (F15) stay Healthy for that allocation's life, and the
  free room is a stable prefix of the rest. Without this, a shrinking count could evict a checkpointed ID
  while the total looks fine, and the container would fail to restart. What this family still does *not*
  need is per-card health attribution — no ID names a card — which the three card-bound families keep,
  along with the cross-mode hold rule.
- Live partitions keep running: a container restart while the kubelet is up reuses its cached allocation
  response without consulting the plugin, and during kubelet re-initialisation an already-running container
  short-circuits before the health check. A container that is **stopped** when the kubelet restarts takes
  the checked path, which is exactly why the previous bullet is load-bearing.

*Accept:* an empty node advertises its full partition ceiling healthy; after one instance is carved the
healthy count is `1 + remaining`, so the scheduler's free-slot view equals `remaining`; a node with no room
for any profile has a free view of zero while its running partitions are unaffected and survive a kubelet
restart, including one whose container is stopped at restart time; an ID held by a live allocation is
Healthy in every cycle even as the count falls; two consecutive cycles with an unchanged ledger publish the
identical healthy set; freeing an instance raises the count with no restart.

**F6 — four explicit families in one classifier, and one normative set of request rules.**
`pkg/nodefeature` gains a single classifier mapping a resource name to
`exclusive | shared | sliced | partitioned | visibility | none`, and both webhooks enforce the rules
through it. The rules refer to a Pod's two **container groups**, defined by **lifetime, not by which field a
container sits in**: the *init group* is `spec.initContainers` without `restartPolicy: Always`; the
*running group* is `spec.containers`. A restartable init container (a native sidecar) belongs to neither —
rule 7 forbids it from requesting an accelerator at all, because a sidecar starts during the init phase and
keeps running, so it overlaps every later init container as well as every app container.

| # | Scope | Rule | Today |
| --- | --- | --- | --- |
| 1 | Pod-wide | All containers of a Pod request the **same** family, and accelerator claims appear in **exactly one** container group. | family part enforced; the one-group part is not |
| 2 | per key | `<base>.sliced` must be exactly **1**. Multi-card logical slicing is deferred (Non-Goals). | not enforced at admission; Ascend rejects `N > 1` inside `Allocate`, after scheduling |
| 3 | per key | `<base>.partitioned` must be exactly **1**. | flows through, undocumented |
| 4 | within the claiming group | A partition request names **exactly one** profile shape. | enforced Pod-wide |
| 5 | per key | Each per-profile key's value must be exactly **1**, matching `<base>.partitioned`. | enforced as exactly 1 |
| 6 | within the claiming group | **At most one container** may request a slicing family — logical or physical. | not enforced |
| 7 | Pod-wide | A **restartable init container** may not request **any** accelerator family. | not enforced |

The SSH sidecar is unaffected by rule 6: it requests the visibility resource, deliberately outside the
accelerator families.

Rule 1's one-group clause is the load-bearing addition. Two claims in different groups are **not** free of
each other, for two independent reasons the earlier "they do not overlap in time" reading missed. First,
nothing releases the earlier claim: the kubelet keeps a regular init container's devices in its Pod record
for the Pod's whole life, and GPUStack's own reclaimer destroys a partition on Pod deletion, not container
termination — so a second claim coexists with the first rather than succeeding it. Second, the scheduler
charges a Pod `max(Σ init, Σ app)` per key, so two same-family claims cost **one** unit of quota while
consuming two: the kubelet even hands the second container the first's device IDs from its per-Pod reuse
set, and a placement-authoritative `Allocate` would carve a second instance against that single charge. The
node then over-advertises by exactly one slot and the *next* tenant fails terminally. Confining claims to
one group makes the charge and the consumption agree by construction, and it collapses several
would-be mechanisms: no per-(group, family) demand vector, no cross-group identity resolution, and an
unambiguous reservation for the visibility sidecar to co-allocate.

The demand model still changes: one correlated `(cards, per-card demand, profile)` tuple **per family**
replaces the single `{mode, count, profile}` tuple per Pod, whose mode is overwritten by whichever key is
scanned last. The upstream effective-request helper is the cross-check for the scalar keys — not the source
of the correlated tuple, because it aggregates each key independently — and the correlation must be kept per
podset, never re-derived from a maximum of one key against a sum of another.
*Accept:* the classifier is table-driven and unit-tested for every family including the
`.partitioned.units` and `.partitioned.<kind>-<profile>` shapes; each rule has a webhook unit test in both
directions on the raw-Pod path and, where applicable, the `Instance` path; a Pod claiming an accelerator
family in both container groups is rejected naming the group that must give it up, whether the two claims
are the same family or different ones; a Pod carrying two families, two profile shapes, or the slicing
family in two containers is each rejected with its own message; `nvidia.com/gpu.sliced: 2` is rejected
naming the deferral; two `Allocate` calls for one Pod resolve to their own containers; the per-container
"physical and logical are mutually exclusive" branch is deleted, and the `.units` fold — today scoped to app
containers only — covers whichever group holds the claims.

**F7 — a fourth InstanceType accelerator view.**
`InstanceTypeStatus` gains `AcceleratorPartitioned` (additive, existing field numbers unchanged), computed
from partitioned cards only, while `EX`, `SH` and `SL` are computed from unpartitioned cards only. `PT`'s
`OnceMaxRequest` is a per-card maximum like `SL`'s, since a partition request is single-card. The table
column becomes `Accelerator(EX/SH/SL/PT)` with four `onceMaxRequest/remaining` groups.
*Accept:* on a logical-only pool `PT` reads `0/0` and `SL` is unchanged from today; on a partition-only
pool `SL`, `EX` and `SH` read `0/0` and `PT` reflects the partition instances; on a mixed pool each card
contributes to exactly one of the two; the partition view does not collapse to zero after the first small
allocation; `kubectl get instancetypes` prints the four-group column.

**F8 — the conflation's special cases become population predicates, not deletions.**
One set of per-card population predicates is added first, and the four places that each answer "which cards
count?" their own way are migrated onto it. `EffectiveSlicedCount` (the logical-or-physical fallback) is
removed together with its single caller in the device plugin. The admission check's "skip a partitioned card
for a sliced request" branch is **generalized** to all four families — deleting it outright would regress
the sliced filter. The node-capacity `slicedCards` recount and the InstanceType logical view's inclusion of
partitioned cards move onto the same predicates. This is a small, enumerable set of call sites, not a
graph-scale refactor; adding the predicates ahead of the migrations is what keeps four tasks from growing
four definitions, which is how the present divergence arose.
*Accept:* no symbol returns "logical count or else physical count"; per-card population helpers name what
they count (`logicallySliceable`, `partitioned`) with no third "sliceable" union; the admission check
scopes every family, not only Sliced; behavior is proven by the existing unit tests plus the new ones.

**F9 — the `Instance` API can request a partition, and its logical path is logical-only.**
`InstanceResources` gains `AcceleratorPartitionedProfile string` (additive, new protobuf number): a
non-empty value makes the request a partition request for that profile, mutually exclusive with the two
slice percentages, shaped into `<base>.partitioned: 1` plus `<base>.partitioned.<kind>-<profile>: 1`. The
controller's slicing predicate becomes logical-only, and the webhook rejects a logical-slice or exclusive
request against a pool that cannot serve it — now backed by the re-scoped views of F7. The `Instance` path
carries the same rules as the raw-Pod path; the "a slice request is exactly one card" restriction stays,
matching rule 2, so no unit re-sizing is needed. Narrowing `Status.Detail.IsSliceable()` to logical-only
changes the meaning of five existing call sites without changing their types, so each is audited rather
than assumed.
*Accept:* an Instance with `AcceleratorPartitionedProfile: 3g.40gb` produces the two partition keys plus
`.partitioned.units`, is admitted on a pool offering that profile, and runs; naming a profile the pool does
not offer is rejected with the offered set in the message; setting both a profile and a slice percentage is
rejected; a logical-slice or exclusive Instance against an all-partitioned pool is rejected at admission
rather than left Pending; every `IsSliceable` call site is confirmed correct under the narrowed meaning.

**F10 — keys we stop owning are removed, not stranded.**
The node-capacity reconciler keeps recognizing `<base>.sliced.mig-<profile>` as an owned key for **removal
only**, so a node that an earlier build wrote them onto is cleaned up instead of carrying a phantom resource
forever; it never writes one.
*Accept:* a node carrying `.sliced.mig-*` capacity has those keys nulled within one reconcile; the e2e
teardown reverse-patch covers both families.

**F11 — observe what the SSH sidecar actually gets for a partition, then decide.**
The visibility allocate reuses main's reserved *card* and asks the responder for that card's
visible-devices env, while main's own env names the **partition**. The reservation carries the profile name
and the occupied memory-slice intervals but not the partition's device identity, so the responder cannot
reconstruct it; and passing a whole-card identifier for a card in a partitioning mode is not obviously
safe either. This is measured on hardware rather than guessed.
*Accept:* an e2e observation on a partition-backed SSH workload records the sidecar's visible-devices env,
`nvidia-smi -L` inside the sidecar, and whether a trivial CUDA init succeeds. If the sidecar sees only its
own partition, the case becomes a regression guard and F11 is done; if it sees more, the fix is carrying
the actuator's chosen partition identity in the reservation — in this spec when confined to that one
addition, a follow-up spec if it requires changing the responder contract.

**F12 — vocabulary and documentation, including the request rules.**
The rename covers live code comments, error strings, test helper names, chart templates, Dockerfile stage
comments, `README.md`, `docs/architecture.md`, `docs/walkthrough.md` and `docs/operation/nvidia-mig.md`.
Beyond it, the docs gain a **normative request-rules section** — the seven rules, the container-group
scoping and why accelerator claims are confined to one group, the single-card cap and what deferred it, the
variant exclusion, and a worked example per family — plus the corrected capability-re-detect procedure and a
statement that carving a partition outside GPUStack on a managed node is unsupported (residual 2). The old
MIG key is documented as a pre-release break with no translation rather than as a migration. The MIG guide
moves to the new keys end to end and gains the `Instance`-side partition request. Archived specs are
untouched.
*Accept:* the two greps in the success criteria return nothing outside the archived specs and past
reports; every rule is stated with an accepted and a rejected example; a reader can determine from the
docs alone why an accelerator family may appear in only one container group, why `.sliced: 2` is rejected,
and what to do before flipping a card's partitioning mode.

**F13 — e2e coverage.**
Existing cases move to the new keys; new coverage proves what only real hardware shows: mixed-node
placement, concurrent distinct profiles, saturated-node health, exclusive/shared allocatable-zero on a
partitioned card, the legacy-key rejection, the F11 observation, and one case per demonstrable residual —
the reclaim window, an out-of-band instance, a terminated init container whose instance is still live, and
a kubelet restart with the container stopped.
*Accept:* the MIG lifecycle case passes against the new keys; the new mixed case passes on real hardware
and self-skips without it; request-rule rejections live in the webhook unit tests with one end-to-end
rejection to prove the webhook is wired; the teardown script cleans both families.

**F14 — the old MIG key is a hard break with a legible rejection.**
No released version is marked stable, so the key change needs no migration, no rollout order and no
version-skew handling: `<base>.sliced.mig-<profile>` is simply gone as a request key. What it does need is a
failure an operator can read. Left alone, such a request now falls into the logical-slice path and complains
about a missing memory budget, which explains nothing — so the Pod webhook rejects a container carrying the
key outright, naming the replacement. Nothing on the node side re-checks it: the on-node identity heuristic
cannot reliably tell which container it is looking at, and with no compatibility guarantee to defend there is
no threat model that would justify a best-effort second guess. An in-place upgrade of a development cluster
is handled operationally — drain, upgrade, let the new keys converge — not by compatibility code.
The rejection reaches only Kueue-labelled Pods, like every other rule: a hand-written Pod carrying the old
key pends forever against a resource no node advertises (residual 7).
*Accept:* a legacy-key request is rejected at the webhook naming
`<base>.partitioned.<kind>-<profile>`; no code path maps a legacy MIG key onto the logical responder; one
end-to-end assertion proves the webhook is wired.

**F15 — unambiguous per-container allocation identity (prerequisite, repairs pre-existing breaks).**
The device-plugin API omits the Pod identity, so `Allocate` matches (resource name, quantity) against the
node's pending Pods and skips Pods that already hold a reservation. Three things are wrong with that today,
all fixed here because placement authority (F2) makes identity the last remaining way to get a wrong
outcome.

*The skip is Pod-wide.* The reservation map is keyed by **Pod UID alone**, so once any container of a Pod has
been served, no later `Allocate` can resolve to that Pod. Two containers of one group each holding a live
claim — two app containers each taking a whole card, which no rule forbids — break today. Fixed by keying
reservations per (Pod UID, container), making the skip predicate "skip an already reserved (Pod, container)",
and having the visibility lookup name the Pod's non-self accelerator reservation. Because rule 1 confines
accelerator claims to a single container group, that reservation is unambiguous, including for a native
sidecar that belongs to neither group.

*The durable annotation is a single slot.* A second live claim **erases** the first from the ledger the
reconciler rebuilds, so the cross-mode gate fails open for a card that is still running a container after a
device-manager restart. Fixed by re-shaping the annotation's value into a map **keyed by container name**,
each entry carrying that container's allocation and the device IDs the kubelet offered it. The container
key is what makes the record accumulate across containers *and* stay idempotent within one — a repeated
`Allocate` overwrites its own entry rather than charging its card twice — and it is the only place the
offered IDs can live, since neither `AcceleratorAllocation` may grow fields (Non-Goals) nor can the IDs be
re-derived once `Allocate` picks its own card. It is a value-format change on a device-plugin-owned
annotation, not an API change: the two readers outside the plugin (the `Instance` controller's status
mirror and the cross-mode e2e case) move to an exported aggregation helper. Also fixed by keeping a
**terminating** Pod's
allocation merged until the object is gone, matching the live set the reclaim loop already uses. The
accumulation is **not** filtered by container liveness: the NVIDIA reclaimer destroys an instance on Pod
deletion, not container termination, and the kubelet likewise keeps a regular init container's devices in
its Pod record for the Pod's whole life. A liveness filter would make the ledger report a card free while
an instance still physically occupies it — advertising room that placement then refuses. An entry therefore
charges its card until its Pod is gone, which is what the hardware does.

*The match is too coarse to tell two requests apart.* Two Pods requesting different profiles both carry
`<base>.partitioned: 1`, so one Pod can absorb the other's `Allocate` and have the **wrong profile**
actuated. With placement authority the symptom changes from a terminal failure that gets noticed into a
silently wrong instance, so it is narrowed rather than merely recorded.

It cannot be closed by matching harder. `ContainerAllocateRequest` carries a resource name and a list of
device IDs and nothing else — no profile, no percentage, no Pod identity — so no predicate over the
request can name the container the call is for; adding the response-affecting keys to the *match* would
only shrink the candidate set without saying which candidate is right. What the plugin can check is the
converse: **which candidates this call could actually serve**. A candidate whose demand does not fit the
cards the kubelet offered — a slice whose per-card units exceed their remaining, later a partition whose
profile no card on the node can host — is not the one being served, and dropping it is a real narrowing in
exactly the mixed case the defect describes. Candidates that survive the test are genuinely
interchangeable, so the existing oldest-pending tie-break is kept rather than replaced by a fail-closed
error, which would reject the concurrent-identical-Pods case this heuristic was last repaired to support.

The test **disambiguates; it does not gate**. It reads a ledger that lags reality, so when it rejects
every candidate the search falls back to the unfiltered oldest rather than failing a resolvable request —
admission belongs to the Pod webhook and the node-devices admission check, upstream. What remains when
several feasible candidates differ is the same guess as today, now confined to the cases the node can
actually serve either way.

*The one hole that stays open.* If the kubelet dies after receiving an `AllocateResponse` but before writing
its checkpoint, the Pod is re-admitted with no record while the plugin still holds its reservation; the
retry is then skipped as already-served and can resolve to a different, unreserved candidate. Containment:
when **every** remaining candidate is already reserved, `Allocate` replays the oldest reserved candidate's
allocation instead of erroring. The mixed case — one reserved and one unreserved candidate — is residual 5.

*Accept:* the concurrent-identical-Pods behavior the Pod-wide skip existed for is unchanged; a Pod with two
live claims has both durably recorded, so the rebuilt ledger holds both cards; a container that has
terminated still charges its card until its Pod is gone; a terminating Pod still charges its card until the
object disappears; two pending Pods differing only in per-card demand resolve to the one the offered card
can still hold, rather than to the older one it cannot; equally feasible candidates still fall back to
oldest-pending, and an all-infeasible candidate set still resolves rather than erroring; the device IDs the
kubelet offered are recorded with the allocation so F5 can keep them Healthy; no reservation or annotation
entry survives a prune sweep for a gone Pod.

**F16 — a re-detected capability actually lands in the `Devices` object (prerequisite).**
The detector's alignment path indexes the existing groups, writes an updated group into the index value,
and then rebuilds the list from the **original** slice — so an existing group's capability is computed,
marked dirty, and discarded; only added or removed groups take effect. Every mechanism in this spec derives
from that capability, and the spec's level-based convergence claim is false until this is fixed. The
re-detect trigger is separately narrow: the loop compares `{manufacturer, id, unhealthy}`, so a
partitioning-mode toggle fires no re-detect and the capability stays stale until the DaemonSet restarts.
*Accept:* a fixture where an existing group's slicing capability changes results in the new capability
being persisted; deleting the `Devices` object is not required to pick up a capability change; the docs
state the real mechanism (restart the DaemonSet after a mode toggle) instead of the deleted-object step.

### Known residuals

Each is accepted with its containment; none is a regression of this work.

1. **The ledger frees a partition before the hardware does.** Occupancy is rebuilt from Pod annotations, so
   a deleted Pod's slot reappears in the per-profile key and the healthy count immediately, while the
   reclaimer destroys the instance only after three consecutive absent sightings on its resync cadence. A
   same-profile replacement scheduled inside that window finds the old instance still present and its
   marker still bound, so it can neither adopt nor place, and fails. Partly pre-existing, but F3 and F5 are
   what newly advertise the slot as consumable. Containment: `Allocate` fails closed and Kueue retries;
   the window closes on its own. Measured by an e2e case rather than assumed away; the cheap fix, if it
   proves common, is to let an instance whose owning Pod no longer exists be adopted.
2. **An out-of-band instance is invisible to every node-level key.** Placement reads live NVML, so it will
   not double-book an instance an administrator carved by hand — but the per-profile key, the partition
   health count and the admission check are all annotation-derived, and no annotation ever appears for such
   an instance. Unlike residual 3 this never converges: the node keeps advertising room it does not have
   until the instance is removed. Documented as unsupported on a managed node.
3. **Node-level over-advertisement of mutually exclusive profiles.** The per-profile keys are independently
   subtracted scalars, so a scheduled small-profile Pod cannot reduce an incompatible larger profile's key
   and two Pods can be scheduled onto a node that can host only one of them. The *card* dimension of this
   defect is gone — `Allocate` places the instance itself (F2), so it fails only when no card on the node
   has room. Containment: `Allocate` fails closed, the admission check retries, and the node key converges
   as the ledger updates. Closing the window entirely needs per-profile device-plugin resources
   (Alternatives); nothing in the chosen key shape blocks adopting them.
4. **A non-default TopologyManager policy can mis-align a partition.** Reporting no NUMA topology stops the
   kubelet from aligning CPU and memory to a card the plugin may not use, but it does not stop the opposite:
   under `single-numa-node` the CPU and memory providers can settle on one socket while the only card with
   room is on the other, and the Partitioned resource contributes no constraint to say so. The default
   policy is `none`, where this cannot arise. Stated as a limitation of enabling the policy.
5. **The kubelet-crash identity window.** If the kubelet dies between receiving an `AllocateResponse` and
   writing its checkpoint, the retry after restart can resolve to a different candidate (F15). Contained
   only when *every* candidate is reserved; the mixed case has no fix that does not require the Pod identity
   the API omits.
6. **Asymmetric rollback on responder failure (pre-existing).** If the container-response step fails after
   a partition has been actuated and the annotation patched, `Allocate` returns without releasing the
   reservation or rolling the partition back. Out of scope.
7. **The rules bind only the Kueue-labelled path.** Both Pod webhooks select on the queue-name label, so a
   hand-written Pod without it bypasses every rule and fold while still being able to request the
   device-plugin resources. F14's legible legacy-key rejection is bypassed with them: such a Pod pends
   forever against a key no node advertises. The contract is complete for managed workloads, advisory
   otherwise.
8. **The device plugin never re-registers after a kubelet socket deletion (pre-existing).** Affects every
   family equally; worth an e2e observation rather than a fix here.
9. **One container request per `Allocate` RPC.** The server reads `ContainerRequests[0]`, which matches how
   every current kubelet calls it, but the API permits several and upstream carries a TODO to batch them.
   Stated as an assumption with a fixture that fails loudly if a second entry ever arrives.

### Notes / Constraints / Caveats

- **Go, controller-runtime, Kueue, NFD.** No new dependency. `pkg/deviceplugin` must stay free of vendor
  CGO imports (it is linked as a Go plugin), so the partition placement selector lives in the pure
  `pkg/device` layer and reaches hardware only through the existing actuator seam.
- **Two token semantics coexist.** `<base>`, `.shared` and `.sliced` tokens are card-bound — the token *is*
  the card selection, which is why the cross-mode health machinery applies to them. `.partitioned` and
  visibility tokens are fungible counts whose card `Allocate` chooses. This asymmetry must be stated
  wherever the token model is documented; a new reader will not guess it.
- **TopologyManager hints are meaningless for the Partitioned family.** The kubelet aligns CPU/memory to the
  NUMA node of the device it picked, and the plugin then picks another card. That is moot under the default
  policy `none`, which the existing cross-mode reasoning already relies on. The Partitioned server therefore
  reports **no** NUMA topology rather than a card's — which stops the kubelet from aligning to a card that
  may not be used, but does **not** make a non-default policy safe (residual 4): a resource with no topology
  simply contributes no constraint. The authority for container→device is the annotation ledger, not the
  kubelet's records.
- **`GetPreferredAllocation` is called under policy `none`, contrary to a comment in the plugin today.** It
  runs before the kubelet falls back to the unconstrained device set; it is merely advisory, so nothing in
  this design rests on it either way. The stale comment is corrected during the vocabulary sweep, because a
  false statement about when a hook runs is how the next reader builds on sand.
- **The `Partitioned` enum value is inserted before `Visibility`**, keeping the enum ordered by concept and
  shifting `Visibility` from 4 to 5. `DeviceAllocationMode` is a protobuf varint persisted in the `Devices`
  status and in the per-Pod allocation annotation, so confirm by call-site inspection that `Visibility` is
  internal-only and written to neither before the renumber lands — cheap insurance against a development
  cluster's existing records decoding as the new mode.
- **A partitioning-mode change needs a device-manager restart.** The detect loop re-detects only when
  `{manufacturer, id, unhealthy}` changes, which a mode toggle does not. Restarting the DaemonSet is
  sufficient once F16 lands; deleting the node's `Devices` object is not required.
- **Drain a card before changing its partitioning mode.** A family's tokens exist only while the card's
  capability reports that family, so a flip removes them; the hardware already refuses the toggle while
  compute is running.
- **One more device-plugin server per manufacturer.** The Partitioned server starts only when the
  manufacturer has a partition kind, adding one socket and one list-and-watch stream on such nodes.
- **The operator writes `capacity`, never `allocatable`**, and the presence gate reads capacity (F3).
- **Two mechanisms share the `allocated + remaining` rule** — the per-profile node key (F3) and the
  partition token health (F5). They are separate code paths; changing one without the other reintroduces
  double subtraction.
- **The ClusterQueue still covers only the credits resource.** Both `.units` keys are node-capacity plus
  admission-check inputs feeding credits through the transformation; neither becomes a quota dimension.
- **Clean break on keys.** The old `.sliced.mig-<profile>` request key is not accepted, aliased or
  translated; only the node-capacity *removal* path keeps recognizing it.
- **Clean break on the allocation annotation too, so drain before upgrading the device manager.**
  Its value becomes a per-container map (F15) and the old flat shape is not read. A Pod carrying the
  old shape on a node whose device manager has restarted drops out of the ledger and its cards read
  *free* while it still holds them — a fail-open the rebuild cannot recover, since the occupancy is
  exactly what became unreadable. No translation is written (Non-Goals); instead the reconciler logs
  what is at stake, naming the Pod, and the docs state the drain requirement.
- **`make generate` runs from the main checkout** — the protobuf generator requires a working-directory
  path ending in the module name, so it fails inside a worktree.
- **Verification reach.** The whole module, including the vendor CGO detectors, builds and unit-tests on
  the development host; the mixed-node and saturated-card behaviors need a real partition-capable
  accelerator and are covered by e2e.

### Boundaries

- **Always:** keep the two families' populations disjoint on every code path (device-plugin, node
  capacity, admission check, views); enforce every request rule at admission, so no rule is first
  discovered by a vendor responder at `Allocate` time; keep vendor-specific knowledge (the partition kind)
  in the per-manufacturer map; keep reconciliation level-based and idempotent;
  keep `Allocate` failing closed rather than starting a container without what it asked for, and rather
  than guessing which container it is serving; record the card a partition actually landed on;
  reverse-patch keys the reconciler stops owning; run `make lint` and `make test` after every task; sign
  off every commit.
- **Ask first:** before renaming any API type, JSON field or protobuf field; before changing the credit
  base, the unit denominator, or the ClusterQueue quota shape; before promoting per-profile keys to
  device-plugin resources; before touching an archived spec.
- **Never:** hardcode a vendor's partitioning name outside the kind map; import vendor CGO bindings into
  `pkg/deviceplugin`; emit a resource name that is not a valid Kubernetes qualified name; rewrite a
  profile name to make it key-safe (exclude it instead); advertise a token for a card that cannot serve
  that family; remove an advertised token ID to express a health condition; leave a stale node-capacity
  key unpatched; translate an old-format request key silently.

### Risks and Mitigations

- **An exclusive or shared tenant loses advertised capacity when a card is partitioned.** → That capacity
  was never usable (an exclusive tenant on a partitioning-mode card gets a GPU CUDA cannot use); the views
  now report what is real, and the docs explain why `EX` drops after enabling partitioning.
- **The ledger-derived key increases node status writes.** → Bounded by the existing coalescing window and
  proportional to partition events on that node; measured during the e2e run.
- **Placement authority makes the kubelet's device record for a partition fictional.** → Deliberate and
  already true of the Visibility family; the annotation ledger is the authority, the Partitioned server
  reports no NUMA topology so no alignment is computed from a card it may not use, and the divergence is
  documented rather than left for someone to discover in `pod-resources` output.
- **All partition correctness now rests on resolving the right container.** → That is why F15 widens the
  match to every response-affecting dimension instead of recording the gap, with a red test for the
  cross-Pod case; a wrong resolution can no longer be caught downstream by a placement that simply fails.
  What remains is a kubelet-crash window with no available fix (residual 5), scoped and measured rather
  than papered over.
- **The F8 migration or the T13 sweep changes behavior while claiming not to.** → Both proceed per package
  with that package's tests run after each step (`make test` is the gate for drifting assertion strings),
  and F8's call sites are enumerated in the tasks rather than trusted to a grep; new behavior lands only in
  the T5–T11 tasks.
- **Capping the claiming group at one slicing container breaks an existing workload.** → Pre-release, and no
  shipped workload shape uses a slicing family in more than one container (the SSH sidecar uses the
  visibility resource). Noted against it: the accounting would in fact work, since per-container units are
  additive — the rule is a conservatism, not an arithmetic necessity, and is the cheapest of these rules to
  relax later. The message names the rule and the offending container.
- **The additive InstanceType status field breaks a client assuming three views.** → Additive only, with a
  new protobuf number; the column header change is a display contract, not an API one.

## Design Details

### Commands

Go build, unit tests and lint run on the development host: the whole module — including the vendor CGO
detector packages — compiles and tests there. Three exceptions: `make generate` must run from the **main
checkout**; container images are built on a **remote amd64 builder over SSH** and pushed to a private
registry namespace (only the operator image moves namespace); the e2e suite needs a **Kubernetes cluster
with a MIG-capable NVIDIA node carrying at least two cards**, one partitioned and one not, for the
mixed-placement, concurrent-profile and residual cases — plus, for the one `single-numa-node` observation, a
dual-socket node, which is optional and self-skips. Everything else, including every request-rule
rejection, is covered without hardware.

```bash
make deps        # only when staging/vendored modules change
make generate    # after the additive API fields; from the main checkout, not a worktree
make test        # whole module, including the vendor CGO detectors
make lint        # whole-module golangci-lint; the first cold run needs several minutes
make build
make package     # remote builder; never pushed from a development host by default

go test ./pkg/nodefeature/... ./pkg/deviceplugin/... ./pkg/device/... \
        ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...
```

### Project Structure

```
api/worker/v1alpha1/
  instance_type.go              # + AcceleratorPartitioned status view (additive)
  instance.go                   # + InstanceResources.AcceleratorPartitionedProfile (additive)
  devices.go                    # + DeviceAllocationModePartitioned before Visibility;
                                # remove EffectiveSlicedCount (no field/type renames)

pkg/nodefeature/
  knowns.go                     # .partitioned family; per-manufacturer partition kind (env-overridable);
                                # key-name validity guard; family classifier; .sliced.mig- kept for
                                # removal only

pkg/device/
  population.go                 # NEW — the per-card population predicates all four consumers share
  physical_placement.go         # existing geometry + the partition placement selector (no vendor CGO;
                                # hardware only via the actuator seam)

pkg/deviceplugin/
  helper.go                     # token pool per family (logical count vs partition ceiling)
  server.go                     # Partitioned server, placement-authoritative Allocate (offered IDs are a
                                # quantity, selection published into the reservation before the mutex is
                                # released, no NUMA topology reported); node-level partition health over a
                                # stable healthy set, never ID removal; profile read from
                                # .partitioned.<kind>-<profile>
  controller.go                 # reservations keyed per (Pod, container); candidate resolution
                                # narrowed by feasibility, oldest-pending among the survivors; the
                                # allocation annotation's value becomes a per-container map carrying
                                # each container's allocation and the device IDs kubelet offered it;
                                # terminating Pods stay merged
  types.go                      # Partitioned mode plumbing

pkg/devicemanager/detector/
  detector.go                   # alignment path writes the updated group back (F16)
  nvidia/{device.go,mig_profile.go}   # comment wording; the +me/+gfx exclusion becomes documented

pkg/devicemanager/allocator/nvidia/
  deviceplugin.go, mig.go       # register the Partitioned server; actuator wiring
pkg/devicemanager/allocator/{ascend,cambricon,hygon,metax,mthreads,amd,iluvatar,thead}/
                                # logical-slicing comment/identifier wording only

pkg/worker/controllers/worker/
  node_capacity.go              # own .partitioned.units + per-profile keys valued Σ(allocated+remaining);
                                # watch the ledger Status side; presence gate stays on capacity;
                                # .sliced.* logical-only; legacy .sliced.mig- removal-only
  node_devices_admission.go     # one correlated (cards, per-card demand, profile) tuple per family;
                                # upstream effective-request helper as the scalar cross-check; the shared
                                # population predicates for all four families
  instance_type.go              # four views; EX/SH/SL over unpartitioned cards only
  instance.go                   # logical-only slicing predicate

pkg/worker/webhooks/worker/
  pod.go                        # family classifier; the seven rules, claims confined to one container
                                # group; .units fold follows the claiming group; reject a legacy
                                # .sliced.mig-* request
  instance.go                   # validate AcceleratorPartitionedProfile; reject a request a pool cannot serve

pkg/worker/extensionapis/worker/instance_type.go   # Accelerator(E/S/P) → Accelerator(EX/SH/SL/PT) column
pkg/worker/kuberess/apps_kueue.go                  # fourth transformation; key helpers

deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml   # comment wording
pack/gpustack-operator/{Dockerfile,external/*/build-*.sh}               # comment wording
docs/{architecture.md,walkthrough.md,operation/nvidia-mig.md}, README.md # keys + vocabulary
.claude/skills/gpustack-operator-e2e/cases/*.sh, .claude/skills/_e2e-lib/scripts/teardown.sh
```

### Code Style

The per-manufacturer partition kind follows the existing manufacturer-map-plus-environment-override
pattern in `pkg/nodefeature/knowns.go`, so a new vendor is one map entry:

```go
// _ManufacturerPartitionKindMap maps a manufacturer to its own name for hardware
// partitioning, which becomes the per-profile key's segment prefix — "mig" is NVIDIA's
// name, not the concept's. A manufacturer absent from the map has no hardware
// partitioning, so it advertises no ".partitioned" family at all. Each entry is
// overridable by GPUSTACK_<MANUFACTURER>_PARTITION_KIND.
var _ManufacturerPartitionKindMap = map[string]string{
	ManufacturerNVIDIA: "mig",
}

// GetAcceleratablePartitionedProfileResourceName returns the per-profile physical-partition
// key for a manufacturer and profile — profile "3g.40gb" for nvidia yields
// "nvidia.com/gpu.partitioned.mig-3g.40gb". The profile name is used verbatim: a name that
// is not a valid resource-name segment is excluded upstream when the card's inventory is
// built, so the key always maps back to its profile by plain prefix strip. It returns ""
// when the manufacturer has no partition kind, or when the name would not yield a valid
// resource name.
func GetAcceleratablePartitionedProfileResourceName(manufacturer, profile string) core.ResourceName
```

Conventions to keep: snake_case multi-word file names; exported symbols documented with behavior and
constraints; comments state the logic and the reason, never a spec/task identifier; generic helpers live
under `pkg/utils/*x` or `pkg/device`, not inline; no test written purely to raise a count.

### Implementation Plan

```
T0 detector write-back      ─┐
T1 allocation identity      ─┤  wave 1 — no shared paths, no shared symbols, all four
T2 enum + API fields        ─┤  additive or repairs; nothing downstream is observable yet
T3 population predicates    ─┘
                             ↓ (T2)
T4 .partitioned key family + classifier
                             ↓
   ┌─────────── atomic contract flip — judged together at Checkpoint A ───────────┐
   T5  per-family pool sizing, EffectiveSlicedCount removed          (T3,T4)
   T6  Partitioned server + placement authority + ledger cost        (T1,T4,T5)
   T7  node capacity: .partitioned.units, ledger-derived per-profile (T3,T4)
   T8  Pod webhook: the request rules + per-family admission demand  (T3,T4)
   T9  Kueue credits input for the partition family                  (T4)
   T10 partition token health as a node-level count                  (T6)
   T11 four InstanceType views, EX/SH/SL re-scoped                   (T2,T3)
   └──────────────────────────── Checkpoint A ───────────────────────────────────┘
                             ↓
T12 Instance API partition request                (T2,T4,T8,T11)   ↓  Checkpoint B
                             ↓
T13 vocabulary sweep ──┬── T14 documentation
                       └── T15 e2e port ── T16 new e2e cases   ↓  Checkpoint C (hardware)
```

T5–T11 are one **atomic contract flip**: the moment `.sliced` stops serving partitioned cards, a partition
is requestable only as `.partitioned.*`, so producers and consumers move together. Credits and geometry
health are *inside* the flip — a cut between them would schedule partitions that charge no quota and carry
no geometry gate, neither of which a build-and-test gate can see. The four views are inside it too: an
`EX`/`SH` view re-scoped ahead of the pools would report zero on a card that `.sliced` is still serving.
Each is a separate commit that compiles with its tests updated, but correctness is judged at Checkpoint A.

Three tasks share a file with an ancestor rather than a sibling, which the blocking edge already serializes:
`controller.go` (T1 → T6), `server.go` (T5 → T6 → T10), and `api/worker/v1alpha1/devices.go` (T2 → T5,
because `EffectiveSlicedCount` must be deleted in the same commit as its only caller). T7, T8 and T11 own
disjoint files of **one Go package**, so a package-level `go test` can observe a sibling's in-flight edit;
they still run in parallel, and a failing package-level verify is re-run rather than treated as a defect.

F8 is not a graph-scale refactor: `EffectiveSlicedCount` has exactly one caller
(`pkg/deviceplugin/server.go:171`) and the population logic lives at three further enumerable sites
(`slicedCards` in node capacity, the Sliced-only filter in the admission check, `getAcceleratorResources`
in the InstanceType view). T3 adds the shared predicates; T5, T7, T8 and T11 each migrate their own site.

#### Wave 1 — repairs and additive groundwork (no hardware, fully parallel)

- [x] **T0 · The detector's alignment path writes an updated group back** (F16)
      Blocked by: None
      Owns: `pkg/devicemanager/detector/detector.go`, `pkg/devicemanager/detector/detector_test.go`
      *Do:* add a failing fixture where an existing group's slicing capability changes and is not
      persisted, then rebuild the group list from the index value rather than the original slice. Record in
      the same pass that the re-detect trigger excludes the slicing capability, so a mode toggle needs a
      DaemonSet restart.
      Acceptance: the fixture fails before the fix and passes after; a capability change on an existing
      group is persisted without deleting the `Devices` object; the trigger's blindness is stated in the
      code comment.
      Verify: `go test ./pkg/devicemanager/detector/...`

- [x] **T1 · Unambiguous per-container allocation identity** (F15)
      Blocked by: None
      Owns: `pkg/deviceplugin/controller.go`, `controller_test.go`, `pkg/deviceplugin/gc.go`, `gc_test.go`,
      `pkg/deviceplugin/reclaim.go`, `reclaim_test.go`, `pkg/deviceplugin/server.go` (the reservation,
      identity and patch call sites only), `server_test.go`,
      `pkg/worker/controllers/worker/instance.go` (the annotation read),
      `.claude/skills/gpustack-operator-e2e/cases/case-22.sh` (the annotation parse)
      Gate: review
      *Do:* two failing tests first — two containers of one group each holding a live claim, where the
      second `Allocate` is refused by the Pod-wide skip, and where the second's annotation erases the
      first. Then key reservations by (Pod UID, container), change the skip predicate accordingly, have the
      visibility lookup select the Pod's non-self accelerator reservation, re-shape the annotation's value
      into a per-container map (carrying each container's allocation and the device IDs kubelet offered it,
      which T10 pins Healthy), keep a terminating Pod's allocation merged until the object is gone, and
      narrow the candidate resolution by **feasibility** — drop a candidate whose per-card demand the
      offered cards cannot hold. Keep the oldest-pending tie-break among the survivors; the feasibility
      test must fall back rather than gate. Do **not** add a fail-closed tie-break, and do **not** filter an
      entry by container liveness — both are corrected upstream in F15. The two annotation readers outside
      the plugin move to an exported aggregation helper; the three extra `Owns:` entries are all downstream
      of this task's siblings, so nothing in wave 1 contends for them.
      Acceptance: F15's list; the concurrent-identical-Pods behaviour is unchanged; a terminated container's
      card stays charged until its Pod is gone; no reservation or annotation entry survives a prune sweep
      for a gone Pod.
      Verify: `go test ./pkg/deviceplugin/...`

- [ ] **T2 · `Partitioned` enum + the two additive API fields + regeneration** (F2, F7, F9)
      Blocked by: None
      Owns: `api/worker/**`, `pkg/kubeclients/**`, `deploy/gpustack-operator/chart/crds/**`
      Gate: review
      *Do:* insert `DeviceAllocationModePartitioned` before `Visibility`, add the two additive fields with
      new protobuf numbers, regenerate. Re-confirm by call-site inspection that `Visibility` is never
      persisted and record the result; treat any persisted `Visibility` as a blocker to raise. Runs in the
      **main checkout** — `make generate` fails inside a worktree — so this task is not worktree-isolatable.
      Acceptance: the confirmation is in the commit message; `Partitioned.String()` round-trips; generated
      protobuf, CRD, deepcopy, openapi and applyconfiguration output is consistent; no field number moved.
      Verify: `make generate` from the main checkout, then `make lint` && `go test ./api/... ./pkg/...`

- [x] **T3 · Per-card population predicates** (F8, prefactor)
      Blocked by: None
      Owns: `pkg/device/population.go`, `pkg/device/population_test.go`
      *Do:* add the per-card population predicates (`logically sliceable`, `partitioned`, whole-card
      capable) beside the existing geometry helpers, expressed on the card's reported capability, and
      change **no** consumer. `EffectiveSlicedCount` stays until T5 deletes it with its only caller. This
      exists so the four consumers migrate onto one definition instead of each growing its own — which is
      how today's `slicedCards` / `IsSliceable` / `EffectiveSlicedCount` divergence arose.
      Acceptance: predicates unit-tested for a not-partitioned card, a partitioned card and a card with
      neither capability; every existing test still passes unchanged.
      Verify: `go test ./pkg/device/...` && `make test`

#### Phase 1 — the family split (T5–T11 are one atomic flip)

- [ ] **T4 · The `.partitioned` key family and one classifier** (F4, F6)
      Blocked by: T2
      Owns: `pkg/nodefeature/**`
      *Do:* add the two suffixes, the per-manufacturer partition-kind map (env-overridable), the
      per-profile key builder and parser, a resource-name validity guard, and the family classifier. Keep
      the `.sliced.mig-` helpers annotated as removal-only. The edge on T2 is real:
      `pkg/nodefeature/knowns.go` already switches on `DeviceAllocationMode`, so the classifier needs the
      new enum value.
      Acceptance: nothing consumes the new family yet; the parser never reads `.partitioned.units` as a
      profile; a manufacturer with no kind yields no per-profile key; an invalid name yields an empty key;
      keys stay within the 63-character limit after `/`.
      Verify: `go test ./pkg/nodefeature/...` && `make test`

- [ ] **T5 · Per-family pool sizing, `EffectiveSlicedCount` removed** (F1, F8)
      Blocked by: T3, T4
      Owns: `pkg/deviceplugin/helper.go`, `helper_test.go`, `pkg/deviceplugin/server.go` (pool sizing only),
      `server_test.go`, `api/worker/v1alpha1/devices.go` (the method deletion)
      *Do:* pin today's per-(card state, mode) token set in a table-driven test first. Then size the Sliced
      pool from the card's logical count alone and the Partitioned pool from its partition ceiling, both
      through T3's predicates; keep Exclusive and Shared off a partitioned card; delete
      `EffectiveSlicedCount` together with its only caller (`server.go:171`). No new server yet — this task
      is a pure narrowing, so its diff reads as a scope change rather than a redefinition.
      Acceptance: per the F1 table, allocatable per family matches the card population; a zero-sized pool
      advertises no IDs; with every card partitioned the three logical/whole-card families advertise
      nothing; no symbol returns "logical count or else physical count".
      Verify: `go test ./pkg/deviceplugin/... ./api/...`

- [ ] **T6 · The Partitioned server, placement authority, and the new mode's ledger cost** (F2)
      Blocked by: T1, T4, T5
      Owns: `pkg/deviceplugin/server.go`, `server_test.go`, `pkg/deviceplugin/types.go`,
      `pkg/deviceplugin/controller.go` (the ledger-cost branch), `pkg/device/physical_placement.go`,
      `physical_placement_test.go`, `pkg/devicemanager/allocator/nvidia/**`,
      `pkg/devicemanager/allocator/{allocator.go,config.go,option.go}`
      Gate: review
      *Do:* add the Partitioned `ResourceServer` and register it for NVIDIA; make its `Allocate`
      **placement-authoritative** — treat the offered device IDs as a quantity, choose the card in a pure
      `pkg/device` selector against the live ledger under the existing allocate mutex, and **publish the
      selection (card, profile, intended placement intervals) into the reservation before releasing the
      mutex**, so a concurrent `Allocate` cannot pick the same card against pre-actuation state. Record the
      card actually used in the durable annotation; report no NUMA topology for this family; give the new
      mode an explicit per-token ledger-cost branch instead of the `switch`'s whole-card `default`; move the
      MIG driver and the reclaim-loop gate from Sliced to Partitioned; read the requested profile from the
      per-profile key; add `--no-partitioned` beside `--no-shared` / `--no-sliced` in
      `pkg/devicemanager/allocator/option.go`.
      Acceptance: F2's list, including a request whose offered tokens all name a card that cannot host the
      profile; two concurrent requests for mutually exclusive profiles land on different cards when the node
      has two, with the second observing the first's published selection; a manufacturer without a partition
      kind starts no Partitioned server; one small partition consumes only its own units — asserted by a
      ledger-cost fixture distinct from the token-set fixture.
      Verify: `go test ./pkg/deviceplugin/... ./pkg/device/... ./pkg/devicemanager/...`

- [ ] **T7 · Node capacity: partition keys, ledger-derived per-profile value, legacy removal** (F3, F10)
      Blocked by: T3, T4
      Owns: `pkg/worker/controllers/worker/node_capacity.go`, `node_capacity_test.go`
      Gate: review
      *Do:* split `slicedCards` into the two disjoint populations through T3's predicates; emit
      `.partitioned.units` at whole-card value over partitioned cards; emit each
      `.partitioned.<kind>-<profile>` as `Σ (allocated + remaining)`. That last one is the bulk of the work:
      this reconciler reads only the **Spec** side today, so it must join the Spec capability with the
      Status per-card ledger — copy the access pattern from `node_devices_admission.go:180` — and its
      Devices watch predicate, which today deliberately ignores Status churn, must fire on it inside the
      existing 3 s dedup window. Keep the presence gate on **capacity**; keep recognizing `.sliced.mig-` for
      removal only.
      Acceptance: F3's three-row table reproduces on a one-card fixture holding one mid-size partition; a
      terminating Pod's instance still counts; no card contributes to both `.units` keys; a partition-only
      model emits no logical keys and vice versa; legacy per-profile keys are nulled within one reconcile;
      a second reconcile with an unchanged ledger emits no patch.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [ ] **T8 · The request rules and a per-family admission demand** (F6, F8, F14)
      Blocked by: T3, T4
      Owns: `pkg/worker/webhooks/worker/pod.go`, `pod_test.go`,
      `pkg/worker/controllers/worker/node_devices_admission.go`, `node_devices_admission_test.go`
      Gate: review
      *Do:* implement the seven rules through T4's classifier, with the container groups defined by
      lifetime and accelerator claims confined to one group; delete the per-container
      physical/logical mutual-exclusion branch and extend the `.units` fold to whichever group holds the
      claims; reject a legacy `<base>.sliced.mig-<profile>` key outright, naming the replacement. Replace
      the admission check's single `{mode, count, profile}` tuple with one correlated
      `(cards, per-card demand, profile)` tuple **per family**, using the upstream effective-request helper
      as the scalar cross-check; never re-aggregate as max-units / sum-cards across podsets. Generalize the
      Sliced-only population filter to all four families.
      Acceptance: every rule has an accept and a reject test in both entry paths; a Pod claiming an
      accelerator family in both container groups is rejected naming the group that must give it up; a
      native-sidecar init container requesting any family is rejected; an exclusive request is not judged
      feasible against a partitioned card; `.sliced: 2`, `.partitioned: 2` and a per-profile value of 2 are
      each rejected with their own message; a legacy-key request is rejected naming
      `<base>.partitioned.<kind>-<profile>`.
      Verify: `go test ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/...`

- [ ] **T9 · Kueue credits for the partition family** (F3)
      Blocked by: T4
      Owns: `pkg/worker/kuberess/apps_kueue.go`, `apps_kueue_test.go`
      *Do:* add the `.partitioned.units` transformation and its template helper, and rename the per-unit
      factor helper to a family-neutral name. The logical transformation — including its `multiplyBy` — is
      unchanged.
      Acceptance: four transformations per manufacturer; a logical Workload's credits are byte-identical to
      today for the same demand; a partition Workload's credits equal its profile units; the factor stays
      integer-valued so Kueue's int64 quantization never rounds a fraction up.
      Verify: `go test ./pkg/worker/kuberess/...`

- [ ] **T10 · Partition token health as a node-level count** (F5)
      Blocked by: T6
      Owns: `pkg/deviceplugin/server.go`, `server_test.go`
      Gate: review
      *Do:* compute each partitioned card's `remaining` as the maximum over its profiles and publish
      `Σ (allocated + remaining)` tokens Healthy. The healthy **set**, not just the count, is the contract:
      every device ID recorded against a live allocation stays Healthy for that allocation's life, and the
      free room is a stable prefix of the remaining IDs. IDs are never removed.
      Acceptance: F5's list; an ID held by a live allocation is Healthy in every cycle even as the count
      falls; two consecutive ListAndWatch cycles with an unchanged ledger publish the identical healthy set;
      a saturated node's running partitions are unaffected.
      Verify: `go test ./pkg/deviceplugin/...`

- [ ] **T11 · Four InstanceType views and the table column** (F7, F8)
      Blocked by: T2, T3
      Owns: `pkg/worker/controllers/worker/instance_type.go`, `instance_type_test.go`,
      `pkg/worker/extensionapis/worker/instance_type.go`
      *Do:* compute `AcceleratorPartitioned` from partitioned cards and `EX`/`SH`/`SL` from unpartitioned
      cards only, through T3's predicates; `PT`'s `OnceMaxRequest` is a per-card maximum. The column is
      `Accelerator(E/S/P)` with three groups today — it becomes `Accelerator(EX/SH/SL/PT)` with four, so the
      abbreviations change as well as the count.
      Acceptance: F7's list, including that an all-partitioned pool reports `EX`, `SH` and `SL` as `0/0`.
      Verify: `go test ./pkg/worker/controllers/worker/... ./pkg/worker/extensionapis/worker/...`

- [ ] **Checkpoint A.** `make lint` && `make test` green; and — because a build-and-test gate cannot see
  the failure modes that made this flip atomic — the rendered-values test asserts four transformations, the
  health fixtures assert the saturated-card case and a stable healthy set across two cycles, the ledger-cost
  fixture asserts a small partition costs its own units, and the legacy-key rejection passes.
  `grep -rn '\.sliced\.mig-' pkg/` shows only the removal-only site and the webhook rejection.

#### Phase 1b — the `Instance` surface

- [ ] **T12 · The `Instance`-side partition request** (F9)
      Blocked by: T2, T4, T8, T11
      Owns: `pkg/worker/webhooks/worker/instance.go`, `instance_test.go`,
      `pkg/worker/controllers/worker/instance.go`, `instance_test.go`,
      `api/worker/v1alpha1/instance_type.go` (the `IsSliceable` narrowing)
      *Do:* shape `AcceleratorPartitionedProfile` into the two partition keys; make the slicing predicate
      logical-only; validate the profile against the pool's inventory; reject profile-plus-percentage and a
      request a pool cannot serve, backed by T11's re-scoped views. Narrowing `Status.Detail.IsSliceable()`
      to logical-only silently changes **five** call sites — four in the `Instance` webhook, one in the
      `Instance` controller — so audit each rather than relying on the type checker.
      Acceptance: F9's list; every `IsSliceable` call site is either confirmed correct under the narrowed
      meaning or updated, with the audit recorded in the commit message.
      Verify: `go test ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/...`

- [ ] **Checkpoint B.** `make lint` && `make test` && `make test chart` green; the four-view column
  renders; no dead symbol left from the F8 migration.

#### Phase 2 — vocabulary and documentation

- [ ] **T13 · The soft → logical vocabulary sweep** (F12)
      Blocked by: T12
      Owns: `pkg/**`, `deploy/gpustack-operator/chart/templates/**`, `pack/gpustack-operator/**`,
      `README.md`, `.claude/skills/gpustack-operator-e2e/**`
      Gate: review
      *Do:* rename package by package — comments, error strings, test helper identifiers, including
      identifiers carrying "Soft" without "SoftSlic" — running that package's tests after each. Correct the
      two comments this work proved wrong while passing through them: `server.go`'s claim that
      `GetPreferredAllocation` "never even runs" under policy `none` (it does run; it is merely advisory),
      and any comment describing `.sliced` as covering MIG. Archived specs and past reports are untouched.
      Acceptance: the success-criteria greps return nothing outside them; no assertion string drifts
      silently.
      Verify: `make test`, then the greps

- [ ] **T14 · Documentation** (F12, F14)
      Blocked by: T13
      Owns: `docs/**`, `README.md`
      *Do:* add the normative request-rules section; state that the old MIG key is a pre-release break with
      no translation, and that the allocation annotation's value is one too — so a node must be drained
      before its device manager is upgraded, or its running Pods' cards read free; correct the
      partitioning-mode-change procedure (DaemonSet restart, not object deletion); update the MIG guide to the new keys and add the `Instance`-side partition request; update
      the `Accelerator(E/S/P)` column header — it appears in `docs/walkthrough.md` and
      `docs/operation/nvidia-mig.md`; state that hand-carving a partition outside GPUStack is unsupported on
      a managed node, and why.
      Acceptance: a reader can determine from the docs alone why an accelerator family may appear in only
      one container group and what to do before flipping a card's partitioning mode; no stale key or stale
      column header in `docs/`.
      Verify: read-through against the F6 table

#### Phase 3 — e2e

Hardware is not provisioned during Wave 1 through Phase 2. When T15 starts, confirm whether to bring up a
cluster with a MIG-capable node.

- [ ] **T15 · Port the existing cases** (F13)
      Blocked by: T13
      Owns: `.claude/skills/gpustack-operator-e2e/**`, `.claude/skills/_e2e-lib/scripts/teardown.sh`
      *Do:* update the accelerator-view, sliced and cross-mode cases, the teardown reverse-patch, and the
      case table in the suite's skill document. The MIG lifecycle case moves to the new keys.
      Acceptance: each ported case passes unchanged in intent; teardown removes both key families.
      Verify: run the ported cases; teardown leaves no `.sliced.*` or `.partitioned.*` key behind

- [ ] **T16 · New cases for what only hardware shows** (F5, F11, F13, F14)
      Blocked by: T15
      Owns: `.claude/skills/gpustack-operator-e2e/**`
      *Do:* add cases for mixed-node placement, ledger-derived per-profile capacity, saturated-card health,
      exclusive/shared allocatable-zero on a partitioned card, the legacy-key rejection, and the F11
      observation — plus the five the cross-check surfaced: a terminated init container whose MIG instance
      is still live, a same-profile replacement scheduled inside the reclaim debounce window, an
      administrator-created out-of-band instance, a kubelet restart with the partition's container stopped
      rather than running, and — if a dual-socket node is available — `single-numa-node` with free capacity
      only on the far socket. Measure the node-status write volume the ledger-derived key produces.
      Acceptance: zero `UnexpectedAdmissionError` in the mixed case; each case self-skips without its
      hardware; residuals 1, 2 and 3 are demonstrated rather than asserted; the F11 observation and the
      write-volume measurement are written into the case output and folded back into the spec.
      Verify: full suite run; record the F11 outcome and either close F11 or open its follow-up

- [ ] **Checkpoint C.** The suite passes on a MIG-capable node; the F11 decision is recorded; the
  mixed-node requirement and the residuals are demonstrated rather than assumed.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

Several existing tests encode today's conflated behavior implicitly, so changing the code would silently
change what they assert. Before the behavior changes, make them state today's behavior explicitly:

- `pkg/deviceplugin/server_test.go` — pin the per-(card state, mode) token set **and** the per-token
  ledger cost per mode that exist today, so T5's, T6's and T10's changes read as diffs rather than
  redefinitions.
- `pkg/deviceplugin/controller_test.go` — T1's two red tests land here first and fail, proving the
  Pod-wide reserved-skip and the single-slot annotation breaks before they are fixed.
- `pkg/devicemanager/detector/detector_test.go` — T0's red fixture for a changed existing group.
- `pkg/worker/controllers/worker/node_capacity_test.go` — the `.sliced.mig-` fixtures become the legacy
  removal fixtures; add a mixed logical/partitioned group fixture first.
- `pkg/worker/controllers/worker/node_devices_admission_test.go` — pin the current summed demand and the
  current exclusive-vs-partitioned-card verdict before they change.
- `pkg/worker/webhooks/worker/pod_test.go` — the MIG-key fixtures state which container group they
  exercise before the group rule lands.

#### Unit tests

Baselines measured on 2026-07-25 on the development host; targets are the floor this work must not fall
below.

| package | baseline | target | why |
| --- | --- | --- | --- |
| `pkg/nodefeature` | 73.0% | ≥ 80% | key family, classifier, validity guard, per-manufacturer maps |
| `pkg/deviceplugin` | 51.5% | ≥ 60% | advertisement scope, partition health, per-container reservations |
| `pkg/device` | 77.9% | ≥ 80% | the shared population predicates and the partition placement selector |
| `pkg/worker/controllers/worker` | 61.2% | ≥ 65% | partition capacity keys, the fourth view, the demand vector |
| `pkg/worker/webhooks/worker` | 75.9% | ≥ 80% | the seven rules, both directions, both entry paths |
| `pkg/worker/kuberess` | 37.0% | ≥ 37.0% | extend the existing rendered-values test |
| `pkg/worker/extensionapis/worker` | 11.2% | ≥ 11.2% | the column change is asserted by e2e |
| `pkg/devicemanager/allocator/nvidia` | 76.2% | ≥ 76.2% | mode move, not new logic |
| `pkg/devicemanager/detector/nvidia` | 15.7% | ≥ 15.7% | the variant exclusion already has focused tests |

**The named-test list is the F1–F16 accept criteria** — each bullet there must be a test, not a
coverage side effect, and the percentages above are a floor rather than the contract. Five fixtures are
called out because a plausible-looking test would miss them:

- the per-token ledger cost per mode needs its **own** fixture — a token-set test cannot observe it;
- the healthy **set**, for every family including Partitioned, must be asserted stable across two
  ListAndWatch cycles with an unchanged ledger, and an ID held by a live allocation must stay Healthy as
  the count falls (a count-only assertion passes while kubelet's re-admission check fails);
- node-capacity reconciles must be asserted idempotent — a second pass emits no patch;
- two placement `Allocate` calls interleaved between selection and actuation must land on different cards,
  which requires a fixture that can pause between the two;
- a card whose init container has terminated while its Pod lives must still be charged — the ledger
  fixture alone passes either way, so the assertion must be on the resulting per-profile capacity.

#### Integration tests

The project has no envtest harness. That role is served by the fake-client reconciler and webhook tests
above, plus `make test chart`. Two seams therefore have no automated coverage below e2e and are called out
rather than papered over: the device-plugin's registration handshake with a real kubelet, and the vendor
actuator's NVML calls.

#### e2e tests

Ported (T15): the accelerator-view cases and the MIG lifecycle case move to the new keys; teardown
reverse-patches both families.

New (T16), each self-skipping without the hardware it needs:

- **Mixed-node placement** — one node with a partitioned and an unpartitioned card of the same model; a
  partition Pod and a logical-slice Pod both reach Running on the correct card, with zero
  `UnexpectedAdmissionError`, repeated enough times that the kubelet's token choice cannot have been lucky.
  The regression guard for the defect this spec exists to fix.
- **Concurrent distinct profiles** — two Pods requesting different profiles admitted together on one node
  each get their own profile actuated on a card that can host it, and neither Pod's durable record is
  cross-patched.
- **Ledger-derived per-profile capacity** — carve a mid-size partition, then assert the whole-card profile
  key reads zero while the same-size profile key still reads one, and that a whole-card-profile request is
  not scheduled to that node. Also record the node-status write volume.
- **Saturated-node health** — carve until no partitioned card has room for any profile; assert
  `.partitioned` allocatable reaches zero, that already-running partitions are unaffected and survive a
  kubelet restart, and that freeing one restores the count without a restart.
- **Exclusive/shared allocatable-zero** — a partitioned card reports its whole-card and shared tokens
  Unhealthy (capacity intact, allocatable zero); an exclusive Pod on an all-partitioned pool is rejected
  at admission rather than admitted and left Pending.
- **Legacy-key rejection** — a Pod carrying `<base>.sliced.mig-<profile>` is rejected with the new key
  named, proving the webhook is wired.
- **SSH sidecar observation (F11)** — record the sidecar's visible-devices env, `nvidia-smi -L` inside the
  sidecar, and whether a trivial CUDA init succeeds.
- **Terminated init container, live instance** — a Pod whose init container took a partition and has
  since terminated, with the Pod still running: assert the per-profile key still charges that instance and
  that a Pod admitted against the reported free count does not fail at `Allocate`. The ledger-only unit
  fixture cannot see this; only the hardware can.
- **Reclaim-window replacement** — fill a card, delete one Pod, and immediately schedule a same-profile
  replacement. Records residual 1: the key frees on Pod deletion while the instance survives up to three
  reclaim misses.
- **Out-of-band instance** — an administrator carves a partition directly. Records residual 2: placement
  sees it, the node keys never do, and the failure does not converge.
- **Saturated restart with the container stopped** — the running-container path is protected by the
  kubelet's already-running shortcut; force the other path by restarting with the container stopped, which
  reaches the previously-allocated-devices health check.
- **`single-numa-node` cross-socket** (dual-socket node only) — CPU/memory constrained to one socket with
  partition capacity only on the other. Records residual 4 rather than asserting alignment.

## Alternatives

- **A device-plugin resource per profile** (one server per profile, tokens per card, health per profile).
  Once `Allocate` places the instance itself (F2), this no longer buys card-level correctness — that is
  already had. What remains is the *node*-level window of residual 3: a per-profile resource would let the
  kubelet's own accounting refuse a node that cannot host the profile, instead of a scalar key that
  converges. Rejected on cost and blast radius: it multiplies the registered resources per node (roughly
  seven on an H100), requires starting and stopping servers as profiles appear, and couples plugin
  registration to a capability that changes rarely. The chosen key shape needs no change to adopt it later.
- **Make all four families placement-authoritative.** The same trick applied to `<base>`, `.shared` and
  `.sliced` would collapse the cross-mode health machinery into pure counting — the plugin simply never
  picks a held card, which is stronger than marking tokens Unhealthy and needs no reservation to close the
  TOCTOU window — and it is a prerequisite for multi-card logical slicing, since only the plugin can
  guarantee that N tokens become N distinct cards. The responder interface would not change; only the
  source of the allocated card set would. Not adopted here: it rewrites a mechanism that shipped days ago
  together with the e2e case guarding it, it would give up the TopologyManager hints for every family
  rather than one, and it inherits the healthy-set stability obligation of F5 four times over. Recorded as
  the symmetric follow-up, most likely alongside the card-count key.
- **Keep one `.sliced` family for both kinds and let `Allocate` sort it out.** Placement authority does
  remove the card-level failure this bug started as, so a merged coarse key would no longer strand a
  partition on a non-partitionable card. Still rejected: the resource name is the only thing the scheduler
  filters nodes on, and a node that cannot serve a kind is the one placement error `Allocate` cannot repair.
  Merging would push that discriminator into the sibling counting keys, leave the coarse key meaning "some
  kind of sharing", and reproduce `.sliced.mig-<profile>` — where a tenant must read three keys to learn
  whether they got hardware isolation. The two kinds also differ in request grammar, accounting and rules.
- **Steer selection with `GetPreferredAllocation`.** It *is* called under the default topology manager
  policy, but it is advisory — the kubelet is free to ignore the returned set. Rejected as a correctness
  mechanism; still useful as a spreading hint.
- **Solve it with health alone: one pool, marked Unhealthy for the "wrong" kind.** Health is a property of
  the advertised device, not of the requester, so one pool cannot be healthy for a logical request and
  unhealthy for a partition request at once. Rejected as insufficient.
- **Make the `.units` keys total-valued (`per-card × cards`) so an N-card request subtracts N cards' worth.**
  Adopted for one revision of this spec and then withdrawn, because it does not achieve what it was for. It
  corrects the units key alone, while the sibling percentage keys stay per-card by necessity — the vendor
  responder reads them as the per-card injection budget — so a two-card, half-each request still fits a
  one-card node on every key: units total to exactly one card's denominator, and 50 % is under 100 %. No
  additive scalar can separate "two cards at half" from "one card in full"; only a card-count dimension can.
  With `<base>.sliced` capped at 1 the whole question is moot, so the keys stay per-card and the credits
  transformation keeps its `multiplyBy`. Revisit together with multi-card logical slicing.
- **Allow `<base>.sliced: N > 1` in this spec.** NVIDIA's logical slicing can isolate more than one card, so
  the runtime supports it. Deferred anyway: there is no node-level key that expresses "N distinct cards", so
  a one-card node accepts a two-card request and the rejection lands in `Allocate` as a terminal Pod — the
  exact failure class this spec exists to remove. Adding a node-level card-count key, its four consumers and
  its e2e coverage is a spec of its own, and nothing here blocks it.
- **Allow `<base>.partitioned: N > 1`.** Placement authority (F2) makes it implementable — the plugin can
  pick N distinct cards, which the kubelet's own advisory preference hook never could. Rejected on scope,
  not on mechanism: no workload needs it yet, and it would pull the same node-level card-count dimension
  that multi-card logical slicing needs into this spec. Capped at 1; a multi-partition workload asks for
  several Pods.
- **Rename the API to `Partitioned` for symmetry.** Rejected: the existing names already read as
  physical/logical, and the churn (CRD schema, protobuf, generated apply configs, a `Devices` rebuild)
  buys naming symmetry only. One sentence in the architecture doc states the equivalence.
- **Accept a `mig` infix as the concept's name.** Rejected: hardware partitioning exists on more than one
  vendor, and baking a vendor's marketing name into `pkg/nodefeature` forces the next vendor to misuse it
  or fork the family.

## Open Questions

None outstanding. The one decision deliberately left to a measurement is F11 — what the SSH sidecar
actually receives for a partition — with both outcomes and the escalation boundary written down in advance.

**Resolved during specification** (recorded so the rationale is not re-litigated):

- *Selection mechanism* — split the coarse token pools per family; per-profile keys stay node-level. The
  split's purpose is **node**-level: the resource name is what the scheduler filters on, and a node that
  cannot serve a kind is the one placement error `Allocate` cannot repair.
- *Placement authority* — `.partitioned` only: its `Allocate` ignores which card the kubelet's tokens name
  and chooses one against the live state, publishing the choice into the reservation before releasing the
  mutex. This removes the card dimension of residual 3 and reduces partition health to a node-level count.
  The other three families stay card-bound.
- *Allocation identity* — fixed here rather than deferred, because placement authority turns a misresolved
  container from a failure that gets noticed into a silently wrong instance. The Allocate RPC carries no
  dimension that could name the right container, so the resolution is narrowed by feasibility — drop the
  candidates this call could not serve on the cards offered — rather than by a richer match. The
  oldest-pending tie-break **stays** among the survivors, and the test never gates: an all-infeasible set
  falls back to it, because a fail-closed outcome would reject the concurrent-identical-Pods case the
  heuristic was last repaired to support.
- *The allocation annotation's value* — re-shaped into a per-container map carrying each container's
  allocation and the device IDs kubelet offered it. A device-plugin-owned value format, not an API change.
- *Container-liveness filtering* — rejected. The reclaimer and the kubelet both scope a device to the Pod's
  life, so a liveness filter would report a card free while its instance still occupies slices.
- *Per-profile key shape* — `<base>.partitioned.<kind>-<profile>` with a per-manufacturer, overridable
  kind; `mig` is NVIDIA's name for it, not the concept's.
- *Card counts* — `.sliced`, `.partitioned` and every per-profile key are exactly 1. Multi-card logical
  slicing is deferred: no additive node-level key can distinguish N cards at 1/N each from one whole card,
  so admitting `N > 1` would make a one-card node accept a two-card request and fail it terminally.
- *`.units` valuation* — stays per-card, and the credits transformation keeps its `multiplyBy`. Total-valued
  units were adopted for one revision and withdrawn: with every request capped at one card they are
  identical, and they would not have closed the N-card node-fit gap anyway.
- *Saturated-card health* — the healthy count is `allocated + remaining`, so a full node's free view is zero
  while allocatable still equals its live instances. IDs are never removed, and the IDs held by a live
  allocation stay Healthy for its life, because the kubelet refuses to proceed if a checkpointed ID has
  left the healthy set.
- *Presence gate* — stays on `capacity`; allocatable would delete keys while instances are live.
- *API changes* — no renames; two additive fields.
- *`Partitioned` enum placement* — inserted before `Visibility`.
- *Media/graphics profile variants* — not supported; excluded from the inventory, not sanitized.
- *SSH sidecar for a partition* — measured on hardware first (F11), with the escalation written down.
- *Rule scope* — accelerator claims are confined to one container group, with groups defined by lifetime and
  restartable init containers barred from holding a device. Two groups claiming would consume two cards
  against the single unit of quota the scheduler charges.
