# Spec: Unified Integer Credit-Base Scoring (Exclusive / Shared / Sliced)

Status: Building

## Summary
The sliced-accelerator borrow model is silently broken: a 1/8 slice that should consume `0.125` credits is
charged a **full `1` credit** by Kueue. Root cause is in Kueue itself — `pkg/resources/requests.go`
`ResourceValue(name, q)` stores every non-CPU resource usage as `int64` via `q.Value()`, which **ceils** any
fractional quantity. Because our `credits.gpustack.ai/<mfr>` resource carries sub-1 values for shared (`0.1`)
and sliced (`≤0.5`) requests, Kueue rounds them all up to `1`, so the three allocation modes do not share one
honest accounting basis and slicing never borrows correctly. This spec removes fractions entirely by scoring
credits on an **integer base `B = D = 12800`** (reusing the existing `ResourceMaxUnits` denominator): one whole
card = `B` credits, so the smallest legal unit (`1/512` card) maps to the integer `25`. The InstanceType API
keeps displaying whole **card counts** (`credits ÷ B`), and the operator mirrors Kueue's `ResourceValue`
quantization when reading credits back so the displayed/accounted values stay truthful under misoperation.

## Motivation
### Goals
- **Root cause (confirmed).** Kueue `pkg/resources/requests.go`:
  ```go
  func ResourceValue(name corev1.ResourceName, q resource.Quantity) int64 {
      if name == corev1.ResourceCPU { return utilmath.SafeMilliValue(q) }
      return q.Value() // non-CPU → ceil to integer
  }
  ```
  Fractional `credits` (`0.1` shared, `0.125`…`0.5` sliced) are ceiled to `1`. We cannot change Kueue; we must
  keep all credit quantities **integer-valued**.
- **Fixed behavior — one integer basis for all three modes.** Score credits as `B × card-fraction` with
  `B = D = 12800`:
  - exclusive: `B × cards`
  - shared: `(B/10) × owners` = `1280 × owners`
  - sliced: `(B/D) × units × cards` = `units × cards` (factor becomes exactly `1`)

  Every legal request yields an integer; Kueue's `q.Value()` ceil becomes a no-op, so accounting is exact and
  the sliced borrow/reclaim path works.
- **InstanceType display unchanged for users.** `credits` is an internal accounting resource that
  *corresponds to the accelerator*; the API must still surface **`1 × card-count`** (`credits ÷ B`), not the
  raw base-scaled number.
- **Fault tolerance.** Introduce a `ResourceValue`-like quantization helper in `pkg/nodefeature` and use it
  wherever the operator converts credit quantities to integer card counts, so the operator's view matches
  Kueue's int64 accounting and a misconfigured fractional value degrades safely instead of mis-displaying.
- **Success criteria (testable):** a 1/8 slice consumes exactly `1600` credits and reads back as `0.125`
  card / `1` slice; exclusive shows whole cards; e2e case-5 reports the sliced workload **admitted** with
  credit usage matching `B × fraction`, not a ceiled `1`.

### Non-Goals
- Patching or forking Kueue's `ResourceValue` / int64 quantization.
- Changing device-plugin advertised resources (`.exclusive` / `.shared` / `.sliced` / `.sliced.units`), the
  Patch-Node flow, or the global denominator `D` / `SlicedResourceMaxSize`.
- Reworking the borrow/reclaim cohort topology (it is correct; only the per-credit *magnitude* was wrong).
- Changing the Instance/Pod request shape — credits never enter a real Pod spec.

## Proposal
Rescale the single credits accounting layer by the integer base `B = D = 12800` in the three places credits
are produced or consumed, and divide back to card counts only at the display boundary. Credits remain
Kueue-internal.

### User Stories
#### Story 1 — Sliced borrow now charges the true fraction (repro)
As the scheduler, when a user submits a `1/8` sliced Instance on a partitions=8 card, I record `1600` credits
of usage (= `B/8`) against the exclusive queue's `12800 × cards` nominal, so 8 such slices fill exactly one
card — instead of each slice ceiling to `1` credit and exhausting the card after 4 slices (or, with
nominal=cards, failing to admit at all).

#### Story 2 — Operator/admin still sees card counts
As an operator, `kubectl get instancetype` shows the exclusive type's Accelerator capacity/remaining as whole
**cards** (e.g. `4`), and the sliced type as **slices** (e.g. `32`), even though Kueue internally tracks
`51200` base-scaled credits.

### Core Features & Acceptance Criteria
| # | Feature | Acceptance |
|---|---------|-----------|
| F1 | Credit base constant `B = D = 12800` | A single named base in `pkg/nodefeature` (reusing/derived from `ResourceMaxUnits`); `B % 10 == 0` and `B % SlicedResourceMaxSize == 0` so every mode yields an integer. |
| F2 | Kueue transformations rescaled | `apps_kueue.go`: exclusive→credits `"12800"`, shared→`"1280"`, `.sliced.units`→`"1"` with `multiplyBy: .sliced`; `.sliced` drain rule unchanged. `helm template` renders integer factors. |
| F3 | NominalQuota rescaled | `constructResourceGroups`: exclusive credits nominal = `B × cards`; sliced-lent nominal = `units` (= `B/D × units`); sliced-in-sliced-CQ stays `0`. node-5 (A10G×4, 8s): exclusive CQ credits=`51200`, sliced CQ credits=`0`. |
| F4 | Worked credit table (integer) | The table below holds end-to-end via Kueue; no value is ceiled. |
| F5 | InstanceType displays card counts | Exclusive type Accelerator cap/remaining = `credits ÷ B` (whole cards); ORM already in cards; sliced type unchanged (slice-rate). node-5: exclusive cap=`4`, sliced cap=`32`. |
| F6 | `ResourceValue`-like tolerance helper | A `pkg/nodefeature` helper mirroring Kueue's quantization (CPU→milli, others→ceil int) used at the credit→card display conversion; a misconfigured fractional credit degrades to a safe integer rather than a wrong fraction. |
| F7 | Regression guard | Unit tests assert the rescaled transformation factors, the `B × card` nominal, and the `credits ÷ B` display; e2e case-5 asserts admission + true fractional usage. |

**Credit worked examples (B = D = 12800):**

| Request | cards C | units U | partitions | pod `.sliced.units` | credits = `C·U·B/(D·partitions)` | card fraction |
|---|---|---|---|---|---|---|
| exclusive 1 card | 1 | — | — | — | `12800` | 1 |
| shared (1 owner) | — | — | — | — | `1280` | 0.1 |
| sliced 1/8 | 1 | 1 | 8 | 1600 | `1600` | 0.125 |
| sliced 2/8 | 1 | 2 | 8 | 3200 | `3200` | 0.25 |
| sliced 1/512 | 1 | 1 | 512 | 25 | `25` | ≈0.00195 |

### Notes / Constraints / Caveats
- Go + controller-runtime + Kueue v0.18.1 (vendored, `mirrored-kueue`); `ResourceValue` behavior verified
  against that tag via GitNexus.
- Credits never written into a real Pod's `spec.containers.resources`; only produced by Kueue
  transformations and consumed in accounting.
- `B = D` makes the `.sliced.units → credits` factor exactly `1`, so the `.sliced.units` allocatable value
  *is* the credit value — keep that identity to avoid a second magic number.
- Overcommit (`ScaleBack/ToOvercommit`) does not touch credits; the display divide-by-`B` is independent of
  overcommit.

### Boundaries
- **Always:** keep every credit quantity integer-valued; keep the InstanceType API showing card/slice counts;
  run `make lint` and `go test` on changed packages; re-run e2e case-5.
- **Ask first:** any change to `D` / `SlicedResourceMaxSize`, the device-plugin resource set, or the
  borrow/reclaim topology.
- **Never:** fork Kueue; push credits as a real node/Pod resource; introduce a fractional credit anywhere
  Kueue's `ResourceValue` can ceil it.

### Risks and Mitigations
- **Stale data after redeploy** (old fractional credits in existing CQ status) → reconcile rewrites
  NominalQuota; verify on a clean case-5 run.
- **Display divide-by-`B` drift if Kueue ceils a misconfigured value** → F6 tolerance helper quantizes
  consistently; assert in unit tests.
- **Hidden second consumer of the credit magnitude** (a reader assuming credits==cards) → grep all
  `GetAcceleratableCreditsResourceName` callers (`creditsQuota`, `convertInstanceTypeFromClusterQueue`,
  `mkSlicedClusterQueue`) and cover each.
- **int64 overflow** → max realistic `B × cards` (e.g. 8×12800) is tiny; no risk.

## Design Details
### Commands
```bash
make lint
go test ./pkg/nodefeature/... ./pkg/worker/...
helm template deploy/gpustack-operator/chart | grep -A4 transformations   # integer factors
bash .claude/skills/gpustack-operator-e2e/cases/case-5.sh gpustack-system  # sliced admission + true credit
```
### Project Structure (files in scope)
```
pkg/nodefeature/knowns.go                         # credit base B (=D), ResourceValue-like helper
pkg/nodefeature/helper.go                         # GetAcceleratableCreditsResourceName (unchanged) + credit↔card converters
pkg/worker/kuberess/apps_kueue.go                 # transformation factors 1→12800, 0.1→1280, 1/D→1
pkg/worker/controllers/worker/clusterqueue.go     # constructResourceGroups: NominalQuota = B × cards
pkg/worker/extensionapis/worker/instance_type.go  # exclusive cap/rem = credits ÷ B (display)
```
### Code Style
```go
// One whole card is worth B = D credits; the smallest legal unit (1/512 card)
// is the integer 25, so Kueue's ResourceValue int64-ceil never corrupts accounting.
const CreditsPerCard = ResourceMaxUnits // B = D = 12800

// Producer side: card count → integer credits (NominalQuota, and conceptually
// the transformation factors which are derived from B, not hardcoded).
func CardsToCredits(cards resource.Quantity) resource.Quantity // = cards × B

// Consumer/display side: integer credits → card units, fraction-preserving so
// the sliced path (× partitions) keeps working; the exclusive whole-card display
// applies a ResourceValue-like guard (mirror Kueue's quantization) for tolerance.
func CreditsToCards(credits resource.Quantity) resource.Quantity // = credits ÷ B
```

**Display normalization happens at the read points, not downstream.** In
`convertInstanceTypeFromClusterQueue`, `credits ÷ B` is applied where credit quantities are first read —
the `NominalQuota` sum and the `FlavorsReservation.Total` subtraction for the credits resource — so both the
exclusive whole-card branch and the sliced `cardAcc + remAcc` → `× partitions` branch operate in card units
exactly as they do today. `ScaleBackOvercommit` is a no-op for credits and runs before the `÷ B`, so order is
safe.
### Implementation Plan
Dependency order: **Task 1 (base + converters)** → **Task 2 + Task 3 (producer side: transformations &
NominalQuota — land together so admission stays correct)** → **Task 4 (display side: divide-at-read)** →
checkpoint → **Task 5 (e2e + comments)**. Tasks 2–4 are tightly coupled (the credit *magnitude* changes); the
system is only fully consistent again after Task 4, so the working-state checkpoint sits after it. The call
surface is closed to six files (`GetAcceleratableCreditsResourceName` def + three producer/consumer sites +
their tests); `node_test.go` only uses `ResourceMaxUnits` for `.sliced.units` allocatable and is unaffected.

- [x] **Task 1 — Credit base + converters (`pkg/nodefeature`).** Add `CreditsPerCard = ResourceMaxUnits`
  (`B = D = 12800`) in `knowns.go`, plus `CardsToCredits` / `CreditsToCards` converters and a
  `ResourceValue`-like quantization guard (CPU→milli, others→ceil int) for the exclusive whole-card display.
  Pure addition — unused until later tasks, so the build/behavior is unchanged.
  **Acceptance:** `B % 10 == 0` and `B % SlicedResourceMaxSize == 0` (both hold: 12800); `CreditsToCards`
  round-trips whole cards (`CardsToCredits(4)=51200`, `CreditsToCards(51200)=4`) and preserves the sliced
  fraction (`CreditsToCards(1600)=0.125`). **Verify:** `go test ./pkg/nodefeature/...`.

- [x] **Task 2 — Rescale Kueue transformations (`apps_kueue.go`).** Change the per-manufacturer factors:
  exclusive→credits `"1"`→`"12800"`, shared→`"0.1"`→`"1280"`, `.sliced.units`→credits `"0.000078125"`→`"1"`
  (= `B/D`); keep `multiplyBy: <.sliced>` and the `.sliced` empty-`outputs` drain rule. Prefer deriving the
  factors from `B` via a template func over hardcoded literals to avoid drift. Update the `0.000078125`-era
  comment to the integer model.
  **Acceptance:** `helm template` renders integer factors `12800` / `1280` / `1`; the sliced rule still carries
  `multiplyBy: nvidia.com/gpu.sliced`; the `.sliced` drop rule remains. **Verify:**
  `go test ./pkg/worker/kuberess/...` + `helm template deploy/gpustack-operator/chart | grep -A4 transformations`.

- [x] **Task 3 — Rescale NominalQuota (`clusterqueue.go` `constructResourceGroups`).** Wrap the exclusive
  `accQ` (node exclusive allocatable, cards) and the sliced-lent `accQ` (`unitsQ/D`, cards) with
  `CardsToCredits` so the credits `NominalQuota` becomes `B × cards`; the sliced-in-its-own-queue credits stay
  `0`. With `B = D`, the sliced-lent quota collapses to `unitsQ.Value()` — note this identity in a comment.
  **Acceptance:** node-5 (A10G×4, 8s): exclusive CQ credits `NominalQuota = 51200`, sliced CQ credits `= 0`,
  `BorrowingLimit` still nil. **Verify:** `go test ./pkg/worker/controllers/worker/...` (rescale
  `clusterqueue_test` `wantCredits`: 4→51200, etc.).

- [ ] **Task 4 — Divide-at-read on the display side (`instance_type.go`
  `convertInstanceTypeFromClusterQueue`).** Apply `CreditsToCards` at the two credit read points — the
  `capAcc.Add(res.NominalQuota)` sum and the `remAcc.Sub(total)` / `remAccRf.Sub(total)` reservation
  subtraction for the credits resource — so all downstream card math is unchanged. The exclusive whole-card
  cap/remaining go through the `ResourceValue`-like guard; the sliced branch keeps `cardAcc + remAcc` →
  `× partitions`.
  **Acceptance:** exclusive InstanceType Accelerator cap=`4` cards (from 51200), remaining tracks reserved
  cards; sliced InstanceType cap=`32`, and the reservation→remaining table holds with base-scaled inputs
  (1/8 slice reserved = `1600` credits → remaining `31`; 29 slices = `46400` → `3`; full card = `51200` → `0`).
  **Verify:** `go test ./pkg/worker/extensionapis/worker/...` (rescale `instance_type_test` reservation inputs
  `125m`→`1600`, `3625m`→`46400`, `3750m`→`48000`, `3875m`→`49600`, `4`→`51200`; expected slice outputs
  unchanged).

  *Checkpoint: full `go test ./pkg/nodefeature/... ./pkg/worker/...` green and `make lint` clean — scheduling,
  admission, and display are all consistent on the integer basis.*

- [ ] **Task 5 — e2e case-5 + comment refresh.** Update any case-5 assertion that reads the credit magnitude
  to expect the true base-scaled value (a 1/8 slice consumes `1600` credits, not a ceiled `1`); confirm the
  sliced workload is **admitted**. Sweep remaining `0.000078125` / "= 1 credit" comments across the touched
  files to the integer model (doc-only).
  **Acceptance:** `case-5.sh` reports PASS with the sliced workload admitted and the exclusive CQ
  `flavorsUsage` showing `1600` credits consumed for the 1/8 slice. **Verify:** redeploy the dev image and run
  `bash .claude/skills/gpustack-operator-e2e/cases/case-5.sh gpustack-system`.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
None — the existing fake-client table-driven suites in the four touched packages already cover the credit
producer/consumer paths; this change rescales their fixtures and expectations rather than adding new harness.

#### Unit tests
- `pkg/nodefeature`: 2026-06-24 - add `CreditsPerCard` invariants (`B%10==0`, `B%SlicedResourceMaxSize==0`)
  and `CardsToCredits`/`CreditsToCards` round-trip + fraction cases in `knowns_test.go`; target ≥ existing
  coverage, no regression.
- `pkg/worker/kuberess`: 2026-06-24 - `apps_kueue_test.go` transformation expectations updated to integer
  factors (`12800`/`1280`/`1`), multiplyBy + `.sliced` drain unchanged.
- `pkg/worker/controllers/worker`: 2026-06-24 - `clusterqueue_test.go` `wantCredits` rescaled to `B × cards`
  (exclusive `51200`, sliced `0`); borrow/reclaim topology assertions unchanged.
- `pkg/worker/extensionapis/worker`: 2026-06-24 - `instance_type_test.go` reservation inputs base-scaled
  (`125m`→`1600`, …, `4`→`51200`); exclusive cap asserts whole cards (`4`); sliced cap/remaining table
  outputs unchanged.

#### Integration tests
None — no new cross-component integration harness; the fake-client controller tests above exercise the
RF→CQ→InstanceType chain in-process.

#### e2e tests
Reuse `cases/case-5.sh` (sliced accelerator, partitions=8) as the end-to-end regression: it injects feature
labels, mocks the `.sliced` device-plugin capacity via Patch Node, submits a 1/8 Instance, and now asserts the
workload is **admitted** with the exclusive ClusterQueue consuming `1600` credits (the true `B/8`), proving
Kueue's int64 quantization no longer ceils the charge to `1`.

## Alternatives
- **Fork Kueue's `ResourceValue`** to keep fractions — rejected: maintains a vendor patch forever, fights
  upstream.
- **base = 128000 (10×D)** — rejected (this round): unused sub-unit headroom, adds a new magic number;
  `B = D` reuses the existing denominator and zeroes the sliced factor.
- **Drop shared's `0.1` and model shared as integer owners directly** — rejected: changes the shared
  semantics and the user-facing 0.1-card meaning; out of scope.

## Open Questions
- F6 precise rounding direction (ceil vs round) at the credit→card display conversion — lean **ceil** to
  mirror Kueue and stay conservative on "remaining"; confirm in `/my-plan`.
- Whether to keep `0.000078125`-era comments or rewrite them to the integer model (doc-only).
