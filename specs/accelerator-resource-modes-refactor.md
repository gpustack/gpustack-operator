# Spec: Accelerator Resource Modes Refactor (Exclusive / Shared / Sliced)

## Summary

Normalize the three accelerator (GPU/NPU/TPU) resource modes — **Exclusive / Shared / Sliced** — onto a
unified Kueue credits accounting layer. The focus is completing the **Sliced** path, which has never been
wired end to end: an administrator opts a card model into slicing via the node label
`acceleratable.<prefix><manufacturer>-<id>.sliced.partitions=<N>`; the operator then materializes the
`-Ns`-suffixed ResourceFlavor / ClusterQueue / InstanceType, and users may request slice instances at
power-of-two fractions (`1/N, 2/N, …`). This spec lands the **full report scheduling-and-accounting
architecture** (global denominator D=12800, a Webhook unit conversion, Kueue `multiplyBy` to fold in card
count, the dual key `.sliced.units` via Patch Node + `.sliced` via device-plugin, and sliced quota that
borrows from the exclusive resource while the exclusive side may reclaim it). **Runtime isolation
(hami-core soft-partition injection, real MIG instance allocation) is deferred to a follow-up spec.**

## Motivation

### Goals

1. **Admins enable slicing declaratively.** Node label `.sliced.partitions=<N>`, where N must be a power of
   two (and `<= MaxPartitions` for the card). Illegal values are rejected by a **Validating Webhook**.
   Enabling it materializes `-Ns`-suffixed flavor/queue/InstanceType for that model on that node.
2. **Users may request more than one slice.** Lift the "at most one slice per request" limit so users may
   request `U ∈ {1,2,4,…}` (power of two, **strictly less than `partitions`**). On an `8s` node users may
   enter 1/2/4 but **not 8** (whole-card use goes through Exclusive).
3. **Unified credits accounting (1 whole card = 1 credit).** Introduce the global denominator
   **D = 2⁹×5² = 12800**. The Webhook normalizes per-card units U into the per-card scalar `U×D/partitions`;
   Kueue folds in the card count C via `multiplyBy: .sliced` with the single global factor `1/D`, yielding
   `credits = C×U/partitions` — independent of D and identically equal to the card fraction.
4. **Sliced borrows from exclusive; exclusive can reclaim (Story 1 topology).** The sliced flavor (`-Ns`)
   appears in both the sliced ClusterQueue (credits=0) and the exclusive ClusterQueue (credits=4,
   `borrowingLimit` left empty); sliced workloads borrow quota from the exclusive queue through the cohort,
   and exclusive workloads can reclaim the lent quota via `ReclaimWithinCohort`.
5. **Dual-key node reporting.** `.sliced.units` (fine-grained count = `D × participating-card-count`) is
   reported by the **device-manager via a direct Patch Node** (level-based, resilient to the kubelet wiping
   extended-resource capacity on restart); `.sliced` (slice-instance count = `card-count × partitions`,
   the injection-token / unified injection hook) is advertised by the **device-plugin**, which also requires
   **registering a Sliced device-plugin server**.
6. **Correct external output (Story 2).** A sliced InstanceType reports `Accelerator.Capacity =
   card-count × partitions` (node-5: 4×8=32); `UnitResource` is folded by `partitions` with round-down
   (1d=12c/48g → per slice 1c/6g); `OnceMaxRequest = partitions/2`.

**Testable success criteria.** Using node-5 (A10G×4) from `docs/architecture.md` as the canonical case, after
enabling `partitions=8`: the RF/CQ/Cohort names, credits values (sliced CQ=0 / exclusive CQ=4), Capacity=32,
UnitResource=1c/6g, OnceMaxRequest=4, the Webhook rejecting `units=8`, and the Webhook rejecting
`partitions=6` — all asserted by table-driven tests.

### Non-Goals

- **Runtime isolation.** hami-core compilation / Docker packaging / `LD_PRELOAD` injection, real MIG instance
  allocation and `/dev` mounting — **out of scope here.** The `Allocate()` injection hook is registered but
  only does placement bookkeeping into `Devices.Status`; it applies no real VRAM/compute isolation.
- **MIG card-level AdmissionCheck** (using `MaxPartitions=7` to block the 8th slice) — deferred with MIG
  runtime.
- **TAS (cross-node topology-aware scheduling)** — the report itself defers it; we assume external
  submissions do not span nodes for now.
- **Node-internal mixing (Phase B):** the "M cards sliced, the rest exclusive on the same physical node"
  three-tuple flavor split — this spec stays on v1 single-card slicing (node-granularity distinction) and
  does not implement arbitrary in-node mixing.
- **A tuning entry point for Shared mode.** Ten ownerships per card stays a fixed value; no knob is added.

## Proposal

Move the "per-node denominator difference" out of the Kueue transformation factor (which cannot vary per
node) and into a single Webhook unit conversion; keep credits living only in the Kueue accounting layer,
never pushed down to nodes; split the sliced count (Patch Node) from the sliced injection hook
(device-plugin) as a dual key; and have sliced quota reside on top of the exclusive resource (borrow +
reclaim).

### User Stories

#### Story 1 — Admin enables slicing
As an **administrator**, I label `${NODE_NAME}-gpustack-worker` with
`acceleratable.<prefix><manufacturer>-<id>.sliced.partitions=<N>` to enable slicing of a card, so that a
whole card can be divided into 1/N units across multiple workloads.

- **Input constraint:** N must be a power of two (and `<= MaxPartitions` for the card); otherwise the
  Validating Webhook rejects it.
- **Output:** materialize an `-Ns` InstanceType pointing at the node; the smallest request is 1/N of a slice.
- **Scenario (node-5, A10G×4):**
  - **Before enabling:** node-5 is pinned by RF
    `gpustack--amd-epyc-7r13-processor-ln-x64-48c-192g-88g--nvidia-a10g-4d`, belongs to CQ
    `gpustack--amd-epyc-7r13-processor-ln-x64-12c-48g--nvidia-a10g-1d`, with `credits.gpustack.ai/nvidia: 4`.
  - **After enabling `partitions=8`:**
    - node-5 is pinned by RF `…--nvidia-a10g-4d-8s`, belongs to CQ `…--nvidia-a10g-1d-8s`, whose sliced
      flavor carries `credits.gpustack.ai/nvidia: 0`.
    - External stats: take 4 from `4d` and 8 from `8s`; `4×8 = 32` becomes the Capacity count.
    - Simultaneously, the exclusive CQ `…--nvidia-a10g-1d`'s resourceGroup **adds an entry** for flavor
      `…-4d-8s` with `credits.gpustack.ai/nvidia: 4` and `borrowingLimit` left empty — meaning the sliced
      resource borrows from the exclusive resource.
    - Improve `IndexingResourceFlavorsByQueueName`: for an `-Ns`-suffixed RF, **also** index it under the
      suffix-stripped queue name so it appears in the exclusive CQ's `rfList`.
    - The `ClusterQueueSpec` enables `ReclaimWithinCohort` so exclusive can reclaim the resources lent to the
      sliced side.

#### Story 2 — User requests slices
As a **user**, once an admin has configured slicing, I can pick different InstanceTypes to deploy workloads
and may request more than one slice.

- **Input constraints:**
  1. When using a sliced InstanceType (a. specified via `Instance.spec.type`; b. submitted via a Kueue Pod
     label), `units` may exceed 1: on an `8s` node users may request 1/2/4.
  2. The accelerator's `OnceMaxRequest/Remaining/Capacity` are counted at the multiplied (slice) rate.
  3. The user-supplied units must be a power of two and **strictly less than `partitions`** (on `8s`,
     1/2/4 are allowed, 8 is not).
- **Scenario (continuing Story 1):** the InstanceType for CQ `…--nvidia-a10g-1d-8s` reports Accelerator = 32;
  `UnitResource` is folded: 1d is allotted 12c/48g, so each slice (1/8) is 12/8=1c, 48/8=6g (**round down
  only**).

### Core Features & Acceptance Criteria

| # | Feature | Acceptance criteria (testable) |
|---|---|---|
| F1 | partitions Validating Webhook | Node label `.sliced.partitions` non-power-of-two / > `MaxPartitions` → rejected; power of two and legal → accepted. Table-driven unit test. |
| F2 | `-Ns` materialization | After enabling, node-5 gets RF/CQ/InstanceType with `-8s`; after disabling, the RF becomes a draining tombstone. |
| F3 | Global denominator D=12800 | `SlicedResourceMaxSize` 16→512; the size set and base derivation satisfy `D % partitions == 0` and `1/D` is nano-clean; `AcceleratorAllocation` is metered by Mode (whole card 12800 / Shared 1280 / Sliced base 25). |
| F4 | Webhook unit conversion | Pod `.sliced.units` rewritten to `U×D/partitions` (per-card, not multiplied by C); `.sliced`=C unchanged; request==limit; U first power-of-two aligned and validated `< partitions`. |
| F5 | Kueue transformations | Three global `Replace` rules; the sliced one uses `multiplyBy: .sliced` with factor `1/D`; `credits = C×U/partitions` verified by the table below. |
| F6 | Borrow + reclaim topology | The sliced flavor's credits=0 in the sliced CQ; credits=4, borrowingLimit=nil in the exclusive CQ; `IndexingResourceFlavorsByQueueName` strips `-Ns` so the sliced RF enters the exclusive rfList; `ReclaimWithinCohort` enabled. |
| F7 | Dual-key node reporting | `.sliced.units` via Patch Node = `D×card-count` (level-based repatch); `.sliced` via device-plugin = `card-count×partitions`; NVIDIA registers a Sliced server. |
| F8 | extensionapis output | Sliced InstanceType: Capacity=`card-count×partitions`, Remaining at slice rate, UnitResource round-down, OnceMaxRequest=`partitions/2`. |
| F9 | API field | `InstanceResources` gains `AcceleratorUnits` (U, default 1, next protobuf tag); `make generate` passes. |

**credits worked examples (D=12800, for acceptance):**

| Request | C | U | partitions | pod `.sliced.units` | credits |
|---|---|---|---|---|---|
| single card 1/8 | 1 | 1 | 8 | 1600 | 0.125 |
| 2 cards ×1/8 | 2 | 1 | 8 | 1600 | 0.25 |
| single card 1/4 | 1 | 2 | 8 | 3200 | 0.25 |
| single card 1/512 | 1 | 1 | 512 | 25 | 0.001953125 |

### Notes / Constraints / Caveats

- Go (controller-runtime + aggregated extension API) + Kueue + NFD. Follow the Go / Kubernetes / testing
  conventions in `CLAUDE.md`.
- **63-character constraint:** re-check length when the `-Ns` suffix flows into a label value.
- Credits are **never** written into a real Pod's `spec.containers.resources` — they are computed only in the
  Kueue accounting layer via transformations.
- Patch Node must be **level-based and idempotent repatch** (resilient to the kubelet wiping capacity on
  restart).
- After API changes, run `gpustack-operator-generate`.

### Boundaries

- **Always:** run `make generate` after editing API types/webhooks; run `make lint` after Go changes; add
  table-driven tests for new logic; preserve the worker startup order (start the controller manager only
  after the extension API services report ready).
- **Ask first:** deviating from the Story 1 borrow + reclaim topology; changing the value of D; touching
  the Shared fixed value; introducing TAS / MIG / hami-core runtime.
- **Never:** push credits down as a real node resource; do a non-idempotent one-shot Patch Node; remove the
  existing draining-tombstone mechanism; keep `borrowingLimit:0` for sliced queues (this design switches to
  borrowing).

### Risks and Mitigations

1. **Dual-key drift causing accounting/injection mismatch** → the Webhook pins `.sliced`/`.sliced.units` as a
   pair, enforces `D % partitions == 0`, and aligns U to a power of two; API validation as a backstop.
2. **Borrow + reclaim interacting with other queues in the cohort** → enable `ReclaimWithinCohort` only for
   sliced↔exclusive in the same cohort; use a fake client to test that the preemption path does not harm a
   CPU-only queue in the same cohort.
3. **Node-granularity counting fragmentation** → this spec accepts fragmentation + reschedule-on-failure,
   explicitly documented; introduce TAS only at scale.
4. **kubelet restart wipes capacity** → level-based repatch reconcile.
5. **D=12800 amplifying fake-device pressure** → `.sliced.units` goes through Patch Node (not the
   device-plugin's thousands of fake devices); `.sliced` is only `card-count × partitions`, on the order of
   tens.
6. **Kueue borrow + reclaim semantics unverified** → de-risk with an early envtest/fake-client spike (Task 0)
   before locking the T6/T7 topology.
7. **Removing `borrowingLimit:0` changes existing sliced-queue behavior** → low real impact (sliced was never
   functional: no device-plugin server was registered); assert with tests that Exclusive/Shared queues are
   unchanged.
8. **D 10000→12800 and maxSize 16→512 change conversion math and fake-device counts** → the sliced path was
   never live, so there is no in-field migration; assert Exclusive/Shared are unaffected.

## Design Details

### Commands

```bash
make generate      # regenerate deepcopy/CRD/conversion/protobuf/webhook after editing api/ or webhooks
make lint          # required after Go changes
make test          # unit tests (table-driven)
make build         # build the single gpustack-operator binary
```
(Use the `gpustack-operator-generate` skill for generation; the `gpustack-operator-lint` hook runs lint; use
`gpustack-operator-e2e` / `gpustack-operator-chart-e2e` for end-to-end.)

### Project Structure (files in scope)

```
api/worker/v1alpha1/instance.go        # add InstanceResources.AcceleratorUnits (U)
api/worker/v1alpha1/devices.go         # AcceleratorAllocation metered by Mode (fields exist, adjust semantics)
api/worker/v1/instance_type.go         # Sliced/UnitResource/OnceMaxRequest semantics
pkg/nodefeature/knowns.go              # D=12800, SlicedResourceMaxSize 16→512, size/base, bare .sliced, conversion funcs
pkg/nodefeature/helper.go              # .sliced.partitions → -Ns (lift the power-of-two upper bound)
pkg/worker/webhooks/                   # new node-label partitions validating webhook; Instance mutating unit conversion
pkg/worker/controllers/worker/clusterqueue.go   # IndexingResourceFlavorsByQueueName strips -Ns; borrow + reclaim; credits calc
pkg/worker/kuberess/apps_kueue.go      # transformations: add multiplyBy, factor 1/D
pkg/worker/extensionapis/worker/instance_type.go # Capacity=card-count×partitions, UnitResource, OnceMaxRequest=partitions/2
pkg/devicemanager/allocator/nvidia/deviceplugin.go # register Sliced server
pkg/devicemanager/...                  # Patch Node reporting .sliced.units; device-plugin advertising .sliced
pkg/deviceplugin/helper.go             # MaxUnits/_Step* adjusted to D
```

### Code Style

```go
// GetAcceleratableResourceName maps an allocation mode to its node resource name.
// Sliced advertises a fine-grained counting key (.sliced.units, via Patch Node) and
// a coarse injection-token key (.sliced, via device-plugin); credits never leave Kueue.
func GetAcceleratableResourceName(manufacturer string, mode workercore.DeviceAllocationMode) core.ResourceName {
	resName := _ManufacturerAcceleratableResourceNameMap[manufacturer]
	switch mode {
	default:
		return resName
	case workercore.DeviceAllocationModeShared:
		return resName + SharedResourceNameSuffix
	case workercore.DeviceAllocationModeSliced:
		return resName + SlicedResourceNameSuffix // ".sliced.units"
	}
}
```
Conventions: return errors explicitly, never panic for control flow; reconcile idempotently and level-based;
table-driven tests; document exported APIs with behavior/constraints.

### Worked Example — Story 1 borrow + reclaim (node-5, A10G×4, partitions=8)

After the admin enables `partitions=8`, node-5 is pinned by the sliced flavor `…-4d-8s` and reports
`nvidia.com/gpu.sliced.units = 51200` (`D × 4 cards`, `D=12800`). `constructResourceGroups` (Task 6) derives
the card count as `51200 / D = 4` and the index (Task 5) lists the **one** sliced flavor under **both** queues
of the shared cohort:

```yaml
# Cohort — never carries a sliced suffix; aggregates its member queues' quota.
apiVersion: kueue.x-k8s.io/v1beta2
kind: Cohort
metadata:
  name: gpustack--...-12c-48g--nvidia-a10g-1d
---
# Sliced CQ — sliced workloads land here; the sliced flavor holds NO credits and borrows.
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: gpustack--...-12c-48g--nvidia-a10g-1d-8s
spec:
  cohortName: gpustack--...-12c-48g--nvidia-a10g-1d
  resourceGroups:
    - flavors:
        - name: gpustack--...-48c-192g-88g--nvidia-a10g-4d-8s
          resources:
            - name: credits.gpustack.ai/nvidia
              nominalQuota: 0       # borrows from the cohort (borrowingLimit omitted = null = unlimited)
            # ... cpu / memory / ephemeral-storage: nominalQuota=<node>, borrowingLimit omitted
---
# Exclusive CQ — the same flavor is LENT here with the real card count; exclusive workloads land here.
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: gpustack--...-12c-48g--nvidia-a10g-1d
spec:
  cohortName: gpustack--...-12c-48g--nvidia-a10g-1d
  preemption:
    reclaimWithinCohort: Any        # Task 7: lets exclusive reclaim what it lent
  resourceGroups:
    - flavors:
        - name: gpustack--...-48c-192g-88g--nvidia-a10g-4d-8s   # lent sliced flavor
          resources:
            - name: credits.gpustack.ai/nvidia
              nominalQuota: 4       # lends 4 to the cohort (borrowingLimit omitted = null)
            # ... cpu / memory / ephemeral-storage: nominalQuota=<node>, borrowingLimit omitted
        - name: gpustack--...-48c-192g-88g--nvidia-a10g-4d      # exclusive tombstone, draining
          resources:
            - name: credits.gpustack.ai/nvidia
              nominalQuota: 0       # no Node uses this profile anymore
```

Flow, on the shared flavor `…-4d-8s`:
- **Cohort total credits** = `0` (sliced CQ) + `4` (exclusive CQ) = `4` — counted once, no double-count.
- **Borrow.** A sliced workload requesting `1/8` of a card (`0.125` credits, see the credits table above)
  fits nowhere in the sliced CQ's own nominal (`0`), so Kueue admits it by **borrowing** the unused 4 credits
  the exclusive CQ contributes on that same flavor (`borrowingLimit: null` = unlimited). Up to `4 × 8 = 32`
  such `1/8` slices can be admitted concurrently.
- **Reclaim.** When an exclusive workload becomes pending and fits within the exclusive CQ's **own** nominal
  (`4`), `reclaimWithinCohort: Any` lets it preempt the borrowing sliced workloads to take its lent credits
  back. Sliced workloads never fit their own nominal (`0`), so they never trigger reclaim themselves.
- **Residual placement risk** (deferred to e2e): credits accounting is exact, but cpu/memory nominal sits on
  the same physical node in both queues, so simultaneous exclusive+sliced admission can over-subscribe the
  node and leave pods Pending — acceptable per Risk #2 / Open Question #2.

### Worked Example — Story 2 sliced InstanceType output (Task 11, node-5, A10G×4, partitions=8)

`convertInstanceTypeFromClusterQueue` turns the sliced ClusterQueue above into the user-facing InstanceType.
Because the sliced queue's credits nominal is 0 (it borrows), the accelerator counts are derived from the
flavor name's card count (`4d` → 4) × partitions (`8s` → 8), and the unit resources are folded per slice:

The two ClusterQueues of the cohort surface as two InstanceTypes — the sliced one (slice-rate counts) and the
exclusive one (whole-card counts, backed by the credits it lends):

```yaml
# Sliced InstanceType — slice-rate accelerator counts, folded unit resources.
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: gpustack--...-12c-48g--nvidia-a10g-1d-8s   # the sliced queue name
spec:
  acceleratable: true
  manufacturer: nvidia
  sliced: 8                       # partition count N
  unitResources:
    cpu: "1"                      # 12c / 8, round down
    ram: 6Gi                      # 48g / 8, round down
  # ... product / family / os / arch / accelerator detail
status:
  accelerator:
    capacity: 32                  # card-count(4) × partitions(8)
    remaining: 32                 # (4 − reserved credits) × 8; 31 after one 1/8 slice is taken
    onceMaxRequest: 4             # partitions/2 = max power-of-two U < partitions
  # ... cpu / ram / localStorage: capacity/remaining/onceMaxRequest at node scale (unchanged)
---
# Exclusive InstanceType — whole-card counts; capacity is the credits the sliced
# flavor lends here (4), reclaimable from the borrowers. Unit resources are NOT
# folded. The drained "-4d" tombstone contributes 0.
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: gpustack--...-12c-48g--nvidia-a10g-1d      # the exclusive queue name
spec:
  acceleratable: true
  manufacturer: nvidia
  # sliced omitted (0) — not a sliced type
  unitResources:
    cpu: "12"                     # one whole card's unit
    ram: 48Gi
  # ...
status:
  accelerator:
    capacity: 4                   # whole cards (sum of lent credits)
    remaining: 4
    onceMaxRequest: 4
  # ... cpu / ram / localStorage
```

A user then requests the sliced InstanceType with `acceleratorUnits` ∈ {1, 2, 4} (Story 2 / Task 12); `8`
(== N) and non-powers-of-two are rejected.

### Implementation Plan

Sliced along the data flow, vertically; each phase ends with the system compiling and tests passing; the
highest-uncertainty item (Kueue borrowing/reclaim semantics) goes first to fail fast.

**Phase 0 — De-risk (spike)**
- [x] **Task 0:** Validate Kueue borrow + reclaim semantics **analytically** (the repo's controller tests use a
  fake client that does not run the Kueue scheduler/admission, and the sliced path is not yet built, so a live
  test is not possible at this stage — runtime validation is deferred to the Final Checkpoint e2e). Use the
  DeepWiki MCP on the Kueue docs plus the `sigs.k8s.io/kueue@v0.17.1` v1beta2 API source. **Acceptance:**
  confirm, with citations, (1) whether a CQ with `nominalQuota=0` on a flavor can borrow from a same-cohort CQ
  with `nominalQuota>0` on the same flavor; (2) the exact semantics of `ReclaimWithinCohort` (whether a lender
  can reclaim lent-out quota); (3) the precise v1beta2 field names/values for `borrowingLimit`,
  `preemption.reclaimWithinCohort`, `preemption.borrowWithinCohort` used by T6/T7. **Output:** record the
  finding under Open Question #2. **Dependencies:** None. **Files:** spec only (no code). **Scope:** S.

**Phase 1 — Accounting foundation (pure constants/funcs, no behavior change)**
- [x] **Task 1:** Set the global denominator `D=12800` as the single per-card unit basis for every mode by
  changing the existing `ResourceMaxUnits` 10000→12800 (one source of truth — also seeds the device-plugin
  grid and the AcceleratorAllocation ruler), raise `SlicedResourceMaxSize` 16→512, regenerate the size set
  (powers of two up to 512) / per-slice units (D/size, e.g. D/512=25), and rewrite `QuantityToSliceCount`
  (floor(q·sliced), D-independent) / `QuantityToAlignedValue` (q·D/sliced) / `QuantityToOriginalValue`.
  **Acceptance:** every per-mode max size divides D evenly (so `12800/10=1280` Shared and `12800/512=25`
  Sliced are exact) and `1/D` is nano-clean; the worked-example table (1/8→1600, 1/512→25) passes.
  **Verify:** `make test ./pkg/nodefeature/...`, `make lint`. **Dependencies:** None. **Files:**
  `pkg/nodefeature/knowns.go(+_test)`. **Scope:** M.
- [x] **Task 2:** Add the bare `.sliced` resource-name helper (`GetAcceleratableSlicedCardResourceName` →
  `nvidia.com/gpu.sliced`) plus the `SlicedCardResourceNameSuffix` constant, and teach
  `IsKnownAcceleratableResourceName` to recognize the bare key (so device-plugin-advertised `.sliced`
  allocatable changes trigger reconcile). **Acceptance:** returns the bare key; `.sliced.units` unchanged;
  the suffix-match does not collide. Unit tests. **Dependencies:** None. **Files:**
  `pkg/nodefeature/knowns.go(+_test)`. **Scope:** S.

*Checkpoint 1: `./pkg/nodefeature/...` all green, lint clean, no behavior regression.*

**Phase 2 — Admin enables slicing (Story 1 input + `-Ns` materialization + validation)**
- [x] **Task 3:** Tighten the partitions parsing in `helper.go` to accept only valid power-of-two counts via
  a reusable `IsValidSlicedPartitions` helper (power of two in [2, 512]; the per-card MaxPartitions bound is
  enforced by the Task 4 webhook); `-Ns` supports up to 512; z-cohort still carries no suffix. **Acceptance:**
  `partitions=512`→`-512s`; non-power-of-two (3) / below-min (1) / out-of-range silently ignored; table tests
  in `helper_test.go` + `IsValidSlicedPartitions` unit test. **Dependencies:** T1. **Files:**
  `pkg/nodefeature/knowns.go`, `pkg/nodefeature/helper.go(+_test)`. **Scope:** S.
- [x] **Task 4:** Add a Validating Webhook (`NodeFeatureWebhook`) on `nfd.NodeFeature` (CREATE/UPDATE,
  failurePolicy=Fail) that validates only the `${node}-gpustack-worker` object (identified by the
  `nfd.node.kubernetes.io/node-name` label + name convention), rejecting `.sliced.partitions` Spec.Labels that
  are not a power of two in [2, 512], and — best-effort, when the node's `Devices` CR is available — counts
  exceeding the card's `MaxPartitions` (a lookup miss degrades to power-of-two only, never a false rejection).
  **Acceptance:** non-power-of-two / non-integer / over-512 / over-MaxPartitions rejected; legal accepted;
  non-worker NodeFeatures pass through; webhook-gen markers + setup registration + 12-case table test.
  **Verified:** code review APPROVE. **Dependencies:** T3. **Files:**
  `pkg/worker/webhooks/worker/{nodefeature.go(+_test),setup.go,zz_generated.webhooks.go}`. **Scope:** M.

*Checkpoint 2: an admin labels node-5 `partitions=8` and `-8s` flavor/queue materialize; an illegal N is
rejected.*

**Phase 3 — Kueue scheduling topology (Story 1 output: borrow + reclaim)**
- [x] **Task 5:** `indexResourceFlavorByQueueName` emits the suffix-stripped queue name in addition for an
  `-Ns` RF. **Acceptance:** the sliced RF appears in both the sliced CQ's and the exclusive CQ's rfList; unit
  test. **Dependencies:** T3. **Files:** `pkg/worker/controllers/worker/clusterqueue.go(+_test)`. **Scope:**
  S.
- [x] **Task 6:** Rework credits in `constructResourceGroups` — derive card count from the node's
  `.sliced`/`.sliced.units` allocatable; set the sliced flavor's credits=0 in the sliced CQ and
  credits=card-count + `borrowingLimit=nil` in the exclusive CQ; **remove the sliced `borLimit=0`** (allow
  borrowing). **Acceptance:** node-5 8s → sliced CQ credits=0, exclusive CQ credits=4 borrowingLimit nil;
  table test. **Dependencies:** T2, T5. **Files:** `pkg/worker/controllers/worker/clusterqueue.go(+_test)`.
  **Scope:** M.
- [x] **Task 7:** Enable `ReclaimWithinCohort` in `ClusterQueueSpec` (sliced/exclusive queues), leaving
  CPU-only queues unchanged. **Acceptance:** preemption fields set correctly by queue type; table test +
  cross-check against the Task 0 finding. **Dependencies:** T0, T6. **Files:**
  `pkg/worker/controllers/worker/clusterqueue.go(+_test)`. **Scope:** S.

*Checkpoint 3: a fake-client test asserts node-5's Story 1 scheduling chain (RF/CQ/Cohort/credits/borrowing).*

**Phase 4 — credits end to end (Webhook conversion + transformations)**
- [x] **Task 8:** Add `AcceleratorUnits` (U, default 1, next protobuf tag) to `InstanceResources` + `make
  generate`. **Acceptance:** field generated, deepcopy/protobuf clean. **Dependencies:** None. **Files:**
  `api/worker/v1alpha1/instance.go`, generated artifacts. **Scope:** S (generation).
- [x] **Task 9:** `apps_kueue.go` transformations — the sliced rule's factor `1/12800` plus `multiplyBy:
  <.sliced>`; add the template func `getSlicedCardResourceName`; `.sliced` is not an input. **Acceptance:**
  the rendered Kueue config has the three rules correct; template render test. **Dependencies:** T2. **Files:**
  `pkg/worker/kuberess/apps_kueue.go(+_test)`. **Scope:** S.
- [x] **Task 10:** Sliced unit conversion across the Instance webhook + controller (pod resource keys live in
  the controller's `getResourceRequirements`, not the webhook — confirmed during build). **(a)** U defaults to
  1 via the CRD schema default (`+default=1` from Task 8, applied by kube-apiserver before webhooks); the
  controller keeps a `u<=0→1` guard, so no redundant webhook defaulting is added. **(b)** Controller
  `getResourceRequirements`: for a sliced type write the paired pod keys `.sliced.units`=`U×D/partitions`
  (per-card, not multiplied by C, via `QuantityToAlignedValue(U, partitions)`) and `.sliced`=C (card count),
  request==limit. **(c)** Webhook `ValidateCreate`: reject `U >= partitions`. The power-of-two and `<=
  OnceMaxRequest` checks are deferred to Task 12 (keeping them here would make Task 12's "3 rejected"
  unreachable; the power-of-two check there also rejects U=0/negatives). **Acceptance:** worked-example table
  (1/8→1600, U=2→3200, units=8 rejected on 8s). **Dependencies:** T1, T8. **Files:**
  `pkg/worker/webhooks/worker/instance.go(+_test)`, `pkg/worker/controllers/worker/instance.go(+_test)`.
  **Scope:** M.

*Checkpoint 4: `credits = C×U/partitions` worked-example table passes end to end.*

**Phase 5 — User-facing output (Story 2)**
- [x] **Task 11:** extensionapis output — Capacity=`card-count×partitions`, Remaining at slice rate,
  UnitResource round-down, OnceMaxRequest=`partitions/2`. **Acceptance:** node-5 8s InstanceType Accelerator
  Capacity=32, UnitResource 1c/6g, OnceMaxRequest=4; table test. **Dependencies:** T1, T6. **Files:**
  `pkg/worker/extensionapis/worker/instance_type.go(+_test)`. **Scope:** M.
- [ ] **Task 12:** Instance validating webhook — accept U that is a power of two `<= OnceMaxRequest` and `<
  partitions`, reject otherwise. **Acceptance:** on 8s, 1/2/4 accepted, 8 rejected, 3 rejected; table test.
  **Dependencies:** T10, T11. **Files:** `pkg/worker/webhooks/worker/instance.go(+_test)`. **Scope:** S.

*Checkpoint 5: a user picks an 8s InstanceType and can request 1/2/4; 8 is rejected.*

**Phase 6 — Node dual-key reporting**
- [ ] **Task 13:** Register the Sliced device-plugin server in the NVIDIA allocator `New()`; advertise
  `.sliced`=`card-count×partitions`; `Allocate()` does placement bookkeeping only + writes back
  `Devices.Status.AcceleratorAllocation` (no real isolation). **Acceptance:** an NVIDIA node advertises
  `nvidia.com/gpu.sliced`; Allocate writes AcceleratorAllocation; test. **Dependencies:** T2. **Files:**
  `pkg/devicemanager/allocator/nvidia/deviceplugin.go`, `pkg/deviceplugin/server.go(+_test)`. **Scope:** M.
- [ ] **Task 14:** device-manager direct Patch Node `.sliced.units`=`D×participating-card-count`, level-based
  idempotent repatch. **Acceptance:** node `status.capacity` carries `.sliced.units`; self-heals after a
  simulated re-reconcile; test. **Dependencies:** T1. **Files:** `pkg/devicemanager/detector/detector.go` or a
  new patcher (+_test). **Scope:** M.
- [ ] **Task 15:** Remove the sliced thousands-of-fake-devices path in `pkg/deviceplugin/helper.go` (counting
  has moved to Patch Node) and confirm the `_Step*` derivations now built on `MaxUnits=D=12800` (Task 1).
  **Acceptance:** no per-card fake-device pool for sliced; `_MinUnitsInPartitioned = 12800/512 = 25` divides
  evenly; Exclusive/Shared unaffected; test. **Dependencies:** T13. **Files:**
  `pkg/deviceplugin/helper.go(+_test)`. **Scope:** S.

*Final Checkpoint: local-cluster e2e (`gpustack-operator-e2e`) — label a node `partitions=8` → sliced
InstanceType Capacity=32 → a 1/8 request admits and consumes 0.125 credit.*

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- Task 0 Kueue borrow + reclaim semantics spike (envtest or fake client); the finding is written back into
  Open Question #2 and is a precondition before locking T6/T7.

#### Unit tests
Table-driven; target date 2026-06-22:
- `pkg/nodefeature`: `2026-06-22` - target `>=85%` (D / size / base / conversion funcs + `.sliced.partitions`
  → `-Ns`)
- `pkg/worker/controllers/worker`: `2026-06-22` - target `>=75%` (index stripping, credits 0 / card-count,
  borrowing, ReclaimWithinCohort)
- `pkg/worker/webhooks/worker`: `2026-06-22` - target `>=80%` (partitions validation, unit conversion, U <
  partitions validation)
- `pkg/worker/kuberess`: `2026-06-22` - target `>=70%` (transformations rendering: multiplyBy + 1/D)
- `pkg/worker/extensionapis/worker`: `2026-06-22` - target `>=75%` (Capacity / UnitResource / OnceMaxRequest)
- `pkg/devicemanager/allocator/nvidia`, `pkg/deviceplugin`: `2026-06-22` - target `>=70%` (Sliced server,
  Patch Node, Allocate bookkeeping)

#### Integration tests
- Controller envtest: node-5 `partitions=8` → assert RF/CQ/Cohort names + credits (sliced=0 / exclusive=4) +
  ReclaimWithinCohort (add concrete test names after the implementation PR merges).
- Webhook envtest: illegal partitions / `units=8` are rejected at admission.

#### e2e tests
- `gpustack-operator-e2e`: label a node `partitions=8` → sliced InstanceType Capacity=32 → submit a 1/8
  request, it admits and consumes 0.125 credit; remove the label → the RF becomes draining.

## Alternatives

- **Keep the report's `borrowingLimit:0` (sliced holds its own quota, never borrows):** simpler, but loses
  the "exclusive reclaims from sliced" capability — rejected by the user (Story 1 borrow + reclaim chosen).
- **Drop transformations and compute credits in the Webhook, writing them into the Pod:** would force credits
  to become a real node resource and move fragmentation into the credits layer — rejected per report §6.2.
- **Set D=2560 (the minimal value satisfying divisibility):** per-card resolution degrades below the current
  10000 — rejected in favor of 12800.

## Open Questions

1. **How common is U>1 (multiple slices on the same card)?** `AcceleratorUnits` defaults to 1; should U>1 be
   explicitly guided at the docs/default layer, or do most scenarios stay covered by partitions granularity
   + U=1?
2. **The fate of the exclusive flavor under the borrow topology.** After slicing is enabled, node-5 no longer
   advertises the exclusive `-4d` flavor; is the exclusive flavor in the exclusive CQ merely a draining
   tombstone? The reclaim target is the lent quota on the sliced flavor.

   **Task 0 finding (Kueue v0.17.1, v1beta2 — analytically confirmed via the API source + DeepWiki on
   kubernetes-sigs/kueue).** The Story 1 topology is mechanically supported at the quota/accounting layer:
   - **Borrowing works.** A CQ with `nominalQuota=0` on (flavor F, resource R) and `borrowingLimit=null`
     borrows the full unused cohort quota for that (flavor, resource) — so the sliced CQ (sliced flavor,
     credits=0, borrowingLimit nil) borrows the 4 credits the exclusive CQ contributes on that **same sliced
     flavor**. This is exactly why T5 must make the sliced RF appear in the exclusive CQ's resourceGroups (the
     lender and borrower must share the flavor). `borrowingLimit` is `*resource.Quantity`; **null = unlimited
     borrowing** (T6 changes the current `Quantity(0)` to nil).
   - **Reclaim works, with a precondition.** `Preemption.ReclaimWithinCohort=Any` lets the exclusive CQ
     preempt cohort workloads using more than their nominal (the borrowing sliced workloads, whose nominal is
     0). **But reclaim only triggers when the exclusive CQ's pending workload fits within the exclusive CQ's
     own `nominalQuota` on its assigned (flavor, resource).** T7 sets `ReclaimWithinCohort=Any` on the
     exclusive queue; the XValidation rule forbids `reclaimWithinCohort=Never` together with
     `borrowWithinCohort.policy!=Never`, so keep `BorrowWithinCohort.Policy=Never` (current). Sliced-CQ
     workloads never fit within their own nominal=0, so they are admitted only by borrowing and never trigger
     reclaim themselves — harmless to leave their preemption at default.
   - **Residual risk (placement, not accounting) — defer to Final Checkpoint e2e.** Reclaim is a *quota*
     operation; physical placement still needs a node advertising the resource the exclusive Pod requests.
     A slicing-enabled node-5 advertises `.sliced`/`.sliced.units`, not `nvidia.com/gpu`, so a whole-card
     exclusive request cannot land on node-5 while slicing is on. Exclusive reclaim is therefore only
     physically meaningful when the cohort also spans exclusive nodes of the same per-unit shape. The exclusive
     flavor `-4d` with no backing node becomes a zero-quota draining tombstone (existing mechanism); the
     exclusive CQ's live nominal sits on whichever flavor has backing nodes. This placement behavior is
     validated by the e2e, not unit tests.
3. **The boundary between MIG AdmissionCheck and deferred runtime.** This spec does no MIG card-level
   blocking, so when a qos node enables `partitions=8`, who backstops the 8th slice's strand (for now, the
   allocator's placement failure + reschedule)?
4. **The partitions validating-webhook target object — RESOLVED.** The webhook intercepts the
   `*-gpustack-worker` `nfd.NodeFeature` objects (the place users are advised to set `.sliced.partitions`).
   Identification: `metadata.labels` contains `nfd.NodeFeatureObjNodeNameLabel` **and** the object name equals
   `${labels[nfd.NodeFeatureObjNodeNameLabel]}-gpustack-worker`. The `.sliced.partitions` labels live in the
   NodeFeature's `Spec.Labels`. Other authoring paths (direct Node labels, other NodeFeatures) are out of
   scope for now.

   **Follow-up dependency (deferred — "other places not now").** `ConstructNodeCapacityLabels` reads
   `.sliced.partitions` from `node.Labels` and the `NodeFeatureReconciler` fully replaces the worker
   NodeFeature's `Spec.Labels` each reconcile, so a user-set `.sliced.partitions` on the worker NodeFeature is
   currently not durable. For the worker NodeFeature to actually drive slicing end to end, the reconciler must
   preserve/echo the user-supplied `.sliced.partitions`. Tracked as a follow-up task (not Task 4, which only
   adds the validation webhook).
