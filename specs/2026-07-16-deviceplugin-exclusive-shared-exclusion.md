# Spec: Device-plugin enforces exclusive/shared per-card mutual exclusion on all paths

Status: Shipped
Type: Bug fix

## Summary
On the non-Kueue scheduling path, an exclusive whole-card claim (`nvidia.com/gpu`) and a shared claim
(`nvidia.com/gpu.shared`) could be allocated the **same physical GPU**, breaking the per-card
exclusive/shared mutual-exclusion invariant. The device plugin advertises each mode as an independent
resource-name device-ID pool that aliases the same cards, and its authoritative `Allocate` stamped a mode
onto the card kubelet picked without rejecting a card already held in another mode.

This fix moves the invariant to the node — the single gate every Pod crosses regardless of Kueue.
`Allocate` now rejects a cross-mode allocation (gRPC `FailedPrecondition`) when the target card is already
held in another, non-`None` mode, read from the `Devices` ledger `Status` **or** a race-safe in-process
reservation map. A per-node mutex serializes *identify → cross-mode check → reserve* so the check and the
reservation are atomic, and pod identification skips already-reserved pods so a batch of identical GPU Pods
admitted together (e.g. by Kueue) maps one-to-one to **distinct** pods instead of all resolving to the
oldest. `ListAndWatch` deliberately reflects hardware health only — it never withholds tokens by allocation
state, which would desynchronize kubelet's device accounting — so enforcement rests solely on the `Allocate`
reject.

## Motivation
### Goals
- **Root cause (missing on-node gate).** `pkg/deviceplugin/server.go` `Allocate` stamps `Mode:
  s.AllocationMode` onto the card kubelet picked **without** consulting the card's current mode. The only
  mode-aware read (`GetPreferredAllocation`) is advisory and racy, and kubelet may ignore it; the post-hoc
  conflict check in `controller.go` only logs and `continue`s. On the raw path the Pod webhook
  (`pkg/worker/webhooks/worker/pod.go`, objectSelector requires the Kueue queue label) and the node-devices
  AdmissionCheck (`pkg/worker/controllers/worker/node_devices_admission.go`, Workload-scoped, check-only) are
  both bypassed, so nothing enforces the invariant.
- **Second root cause (concurrent-admission misattribution).** `getAllocatingPod` maps a kubelet `Allocate`
  (which omits the pod identity) to "the oldest Pending pod requesting this resource+quantity". When identical
  GPU Pods are pending together (a Kueue-admitted batch), concurrent `Allocate`s all resolve to the same pod →
  one card is double-attributed and another is lost from the ledger → a genuinely-held card can look free and
  defeat the reject above.
- **Fixed behavior.** An exclusive claim and a shared claim are **never** allocated the same physical card on
  **any** path, including under concurrent admission. When no non-conflicting card is available, the
  second-mode claim is not co-located (raw path → device-plugin refusal / `UnexpectedAdmissionError`; Kueue
  path → unchanged). Sliced-on-sliced (same mode) stays allowed.
- **Success criteria (testable).** e2e CASE 22 passes on both variants (Kueue + raw) on real hardware,
  including the concurrent-batch precondition; new device-plugin unit tests for the cross-mode reject,
  health-only `ListAndWatch`, the per-node concurrent-`Allocate` lock, and distinct-pod identification pass
  under `-race`.
### Non-Goals
- Redesigning the per-mode resource-name model into a single unified device-ID pool.
- Changing the Kueue AdmissionCheck / credits accounting (it stays as the queue-path gate).
- Any behavior change for sliced-on-sliced sharing or the Visibility (SSH) sidecar co-allocation.

## Proposal
Enforce the per-card exclusive/shared/sliced cross-mode mutual-exclusion invariant at the on-node device
plugin, so it holds identically whether or not a Pod flows through Kueue.

### User Stories
#### Story 1 (repro / raw path)
As a cluster operator, when I create a raw Pod requesting `nvidia.com/gpu` (no Kueue queue label) and another
raw Pod requesting `nvidia.com/gpu.shared` on a node whose cards are all exclusively held, the shared Pod must
be **held**, never placed onto a card already held exclusively — so an "exclusive" tenant truly owns its card.
#### Story 2 (free-card race)
As a cluster operator, when an exclusive and a shared claim are submitted near-simultaneously on a node with a
free card, the shared claim lands on the **free** card, never co-located onto the card the exclusive claim just
took.
#### Story 3 (Kueue path unchanged)
As a cluster operator, the Kueue-managed path keeps holding a shared claim when all cards are exclusive (no
regression).

### Reproduction & Evidence
Observed on a real 2×RTX-4090 node.

**Before the fix — raw-path co-location (the reported bug).** Fill both cards with raw exclusive Pods
(`nvidia.com/gpu:1`, no queue label), then submit a raw `nvidia.com/gpu.shared:1` Pod → the shared Pod is
allocated a card already held exclusively. The device-manager reconciler detects it but only logs:
```
conflicting allocation mode for resource geforce-rtx-4090:GPU-1068d7f6…: Exclusive vs. Shared
```
The same steps *with* the pool's LocalQueue label hold the shared Pod (Pending): the Kueue path's quota
covers it indirectly; the raw path has no equivalent gate.

**Before the fix — free-card race.** Submit a raw exclusive and a raw shared claim near-simultaneously with
**both** cards free → both land on the **same** card, the other left idle:
```
poc-excl   -> GPU-bbc521eb (mode=Exclusive)
poc-shared -> GPU-bbc521eb (mode=Shared)   # same card; the other card sits idle
```
A microsecond-level trace at the shared `Allocate`'s decision point shows the information to reject was
**already present** — this is not a stale-ledger problem:
```
Allocate diag poc-shared card=GPU-bbc521eb statusMode=Exclusive reservationMode=Exclusive
             conflictByStatus=TRUE conflictByReservation=TRUE
```
Both the ledger `Status` and the cross-pod reservation flagged the conflict at decision time.
`GetPreferredAllocation` was called but is advisory, and the shared pool's `available` still listed the
conflicting card's token because `ListAndWatch` never consulted the card's current mode. `Allocate` had every
signal needed to refuse and simply lacked the logic. (A residual TOCTOU remains if the shared `Allocate`
reads *before* the exclusive one reserves — closed by the per-node serialization below.)

**After the fix (verified 2026-07-16, real 2-card node).** CASE 22 passes on both variants — variant A
(Kueue) holds the shared claim via the admission gate; variant B (raw) holds it via the device-plugin
`FailedPrecondition` → `UnexpectedAdmissionError`. A concurrent Kueue batch of two `nvidia.com/gpu:1` Pods
maps each to a **distinct** card, each pod's allocation annotation matches its own `NVIDIA_VISIBLE_DEVICES`,
and the ledger reads `remaining=0`. The device-manager log shows the reject firing on the raw path
(`heldMode=Exclusive requestedMode=Shared`).

### Core Features & Acceptance Criteria
1. **Cross-mode reject in `Allocate`** — before stamping the mode, if the target physical card is held in a
   different, non-`None` mode (per `Devices.Status` OR the in-process reservation), return gRPC
   `FailedPrecondition`. Acceptance: a raw shared Pod onto an all-exclusive node reaches
   `UnexpectedAdmissionError`, never a co-located allocation annotation.
2. **`ListAndWatch` reflects hardware health only (no cross-mode withhold)** — a card is advertised for a mode
   based solely on its health, never on whether another mode holds it. Withholding tokens by allocation state
   would make the advertised allocatable count fluctuate as cards are reserved/freed (the external Detector
   drives these updates), desynchronizing kubelet's device accounting; enforcement lives in `Allocate`.
   Acceptance: with one card held exclusive (via Status or reservation), the shared server still advertises
   that card's tokens.
3. **Race-safe reservation source** — a cross-pod `reservedModeForResource(group, device)` query over the
   reservation map (written synchronously by every workload `Allocate`) is authoritative when `Devices.Status`
   lags. Acceptance: a shared `Allocate` observes the sibling exclusive reservation even before the async
   ledger reconcile.
4. **Per-node serialization (TOCTOU closure)** — the pod-identification → cross-mode check → reserve section in
   `Allocate` is serialized by a single per-node `allocateMutex` on the shared `DevicesReconciler`, so
   concurrent exclusive+shared `Allocate` for the same card cannot both pass, and (with Feature 7) a concurrent
   batch resolves to distinct pods. The mutex is released before the annotation patch (I/O). Acceptance: a
   concurrency unit test issuing simultaneous exclusive+shared `Allocate` for one card yields exactly one
   success and one `FailedPrecondition`, with no deadlock.
5. **Prompt release on Pod termination (no stuck/leaked card)** — when the Pod holding a card terminates, its
   reservation is pruned and the ledger mode returns to `None` in the **same** reconcile pass, so the card is
   re-advertised and an opposite-mode claim can reuse it. Acceptance: after the holding Pod is deleted,
   `cardHeldInOtherMode` returns false for that card, `ListAndWatch` re-advertises it, and a subsequent
   opposite-mode `Allocate` succeeds on it; no card is left withheld once its Pod is gone.
6. **Regression coverage** — new table-driven unit tests in `pkg/deviceplugin/server_test.go`; the committed
   e2e `cases/case-22.sh` passes both variants.
7. **Distinct-pod identification under concurrent admission** — `getAllocatingPod`, on the workload path, skips
   a pod that already holds an in-process reservation, so concurrent `Allocate`s serialized by `allocateMutex`
   each map to a distinct pending pod instead of all resolving to the oldest one. The reservation is the
   synchronous, cache-lag-free claim marker. The visibility path does **not** skip (it re-finds its own
   reserved pod). Acceptance: two identical exclusive Pods pending together, with one `Allocate` per distinct
   card, end with each pod reserved for a distinct card (both cards accounted), not one pod double-attributed
   and the other lost.

### Notes / Constraints / Caveats
- Go + controller-runtime + the kubelet device-plugin gRPC API; klog logger; `google.golang.org/grpc` status
  codes. No API-type changes (`make generate` not required).
- The reservation map and the mutex live on `DevicesReconciler`; all per-mode `ResourceServer`s share one
  reconciler singleton, and every node's `Allocate` calls are served by that single device-manager DaemonSet
  process — so a cross-pod query and a node-wide `allocateMutex` are in-process and race-free. No external lock
  (`coordination.k8s.io` Lease / Node lock) is needed: physical-card allocation is single-writer per node, and
  locking the Node object would churn every Node watcher.
- **One workload allocation per Pod.** `getAllocatingPod`'s skip-reserved attributes at most one workload
  `Allocate` to a Pod. The supported shape is one workload container requesting the whole-card resource
  (`nvidia.com/gpu[.shared|.sliced]`) plus the optional `sshd` visibility sidecar. A second workload container
  requesting the same resource in the same Pod is skipped by identification and its `Allocate` either finds no
  pod and fails, or — if an unrelated single-card Pod is pending at the same instant — misattributes to that
  pod; this shape is unsupported. Recorded in `docs/architecture.md`.
- **Reservation lifetime.** The reservation gates cross-mode allocation for the Pod's whole lifetime, not just
  the sidecar's admission window, and is pruned in lockstep with the ledger rebuild: `pruneReservations`
  piggybacks the same `Reconcile` live-pod-UID sweep that recomputes `Devices.Status` (terminating pods stay in
  the live set during their grace period), so a card frees for the opposite mode exactly when its Pod
  disappears — no earlier, no later.
- A single `allocateMutex` (not a per-card lock map) serializes the whole critical section. `Allocate` is an
  admission-time, in-memory operation (the annotation patch runs outside the lock), so node-wide serialization
  has negligible cost and cannot deadlock (one lock, no ordering).
- `Allocate` is the sole authoritative gate; `ListAndWatch` is health-only and never enforces the invariant.

### Known Limitations (tracked follow-ups, out of scope here)
Both stem from the same "device-plugin `Allocate` omits pod identity, so identification cannot distinguish
concurrent identical requests beyond the reservation-skip heuristic" class; both are fail-closed or contingent
on unsupported/concurrent shapes, and neither is a regression against this fix's goal.
- **Visibility concurrent leak.** The `sshd` visibility path must re-find its own already-reserved Pod (so it
  cannot skip). When two SSH-enabled Instances are admitted at the same instant, both sidecars' visibility
  `Allocate` can resolve to the oldest Pod, granting a sidecar the *other* Instance's card in
  `NVIDIA_VISIBLE_DEVICES`. Visibility tokens are fungible (they do not encode the physical card), so the
  workload path's match-by-device-id does not apply; hardening it needs a distinct pod-correlation mechanism.
- **Multi-container misattribution.** The unsupported two-whole-card-container Pod shape above can misattribute
  the second container to a concurrently-pending unrelated Pod. Suggested guard: have the Pod admission webhook
  reject a Pod with more than one container requesting a whole-card accelerator resource.

### Boundaries
- **Always:** enforce the invariant at the device plugin (`Allocate`), independent of Kueue; keep the
  reservation map the race-safe source of truth.
- **Ask first:** any change to the per-mode resource-name / device-ID advertising contract kubelet depends on.
- **Never:** reject sliced-on-sliced (intended same-mode sharing); break the Visibility sidecar co-allocation;
  hold a lock across the gRPC response build / external calls.

### Risks and Mitigations
- No `ListAndWatch` withhold → a claim scheduled onto a node with no free same-mode card fails admission
  (`UnexpectedAdmissionError`) rather than being steered away pre-schedule → **Mitigation:** accepted by
  design. Withholding by allocation state desyncs kubelet's device accounting (the Detector drives
  `ListAndWatch`), a worse failure mode; the `Allocate` reject still guarantees no co-location, and the Kueue
  quota/AdmissionCheck gates the managed path before scheduling.
- Node-wide `allocateMutex` serializes all workload `Allocate` on the node → **Mitigation:** the section is
  in-memory only (patch is outside the lock) and `Allocate` is admission-time, so contention is negligible; one
  mutex cannot deadlock.
- `Devices.Status` lag → stale mode read → **Mitigation:** the synchronous reservation map is authoritative;
  Status is a corroborating fallback.
- Concurrent identical-Pod admission → `getAllocatingPod` misattributes → corrupt per-card accounting defeats
  the reject → **Mitigation:** skip already-reserved pods during identification, under `allocateMutex`, so each
  concurrent `Allocate` maps to a distinct pod (Feature 7).
- **Release leak — a card stays stuck after its Pod is gone.** The reservation is load-bearing for cross-mode
  gating across the Pod's whole lifetime. If it lingered after the Pod terminated, `cardHeldInOtherMode` (Status
  OR reservation) would keep reporting the card held → it stays withheld and the opposite mode can never reuse a
  genuinely free card. **Mitigation:** `pruneReservations(livePodUIDs)` runs in the **same** `Reconcile` sweep
  that rebuilds `Devices.Status` from the live-pod set, so the reservation and the ledger free the card together.
  A release unit test + CASE 22's between-variant "wait for `accelerator.remaining == capacity`" guard this.
- **Reserve-before-patch strands a card.** If `patchAllocatingPod` fails after the in-memory reservation is
  written, the pod holds a reservation but no annotation, and the Pod-delete prune is gated on that annotation,
  so the card would stay stranded for the opposite mode until the next full resync → **Mitigation:** `Allocate`
  calls `releaseReservation(pod.UID)` on the patch-failure path (rolling the reservation back) and returns an
  error so kubelet does not start the container. Covered by
  `TestResourceServer_Allocate_RollsBackReservationOnPatchFailure`.

## Design Details
### Commands
Environment is split across three targets:
- **Unit-test iteration — local.** `pkg/deviceplugin` compiles and its tests run without a GPU:
  ```
  go build ./pkg/deviceplugin/
  go test  ./pkg/deviceplugin/... -race -count=1        # regression + concurrency
  go test  ./pkg/deviceplugin/... -cover                # coverage delta
  golangci-lint run ./pkg/deviceplugin/...              # focused lint of the changed package
  ```
- **Full lint / image build — an amd64 build host with the CGO GPU toolchain** (the bindings do not build on
  arm64 macOS). Sync the changed files, then in a login shell:
  ```
  make lint                                             # golangci-lint over ./...
  make build                                            # cross build
  PACKAGE_NAMESPACE=<ns> PACKAGE_TAG=dev PACKAGE_PUSH=true make package   # → registry
  ```
- **e2e regression — a Kubernetes cluster with ≥2 nvidia cards** advertising `/gpu` + `/gpu.shared`:
  ```
  bash .claude/skills/gpustack-operator-e2e/cases/case-22.sh gpustack-system
  ```
No API-type change → `make generate` not required.
### Project Structure
```
pkg/deviceplugin/
  server.go        # ResourceServer: ListAndWatch, GetPreferredAllocation, Allocate (fix lands here)
  controller.go    # DevicesReconciler: reservation map (reserve/reserved/prune) + node allocateMutex
                   #                    + cross-mode query + getAllocatingPod skip-reserved
  helper.go        # Resource, GetDeviceIds (per-mode token pools)
  server_test.go   # table-driven tests (fake client) — new regression cases land here
api/worker/v1alpha1/devices.go   # DeviceAllocationMode enum, AcceleratorAllocation (read-only here)
```
### Code Style
```go
// cardHeldInOtherMode reports whether the physical card is currently held in a mode different from this
// server's, per the ledger Status OR the in-process reservations (the reservation is race-safe: written
// synchronously by every workload Allocate). Free (None) or same-mode is not a conflict.
func (s *ResourceServer) cardHeldInOtherMode(devs *workercore.Devices, res Resource) (bool, workercore.DeviceAllocationMode) {
	if m := statusModeOf(devs, res); m != workercore.DeviceAllocationModeNone && m != s.AllocationMode {
		return true, m
	}
	if m, _ := s.Reconciler.reservedModeForResource(res.Group, res.Device); m != workercore.DeviceAllocationModeNone && m != s.AllocationMode {
		return true, m
	}
	return false, workercore.DeviceAllocationModeNone
}
```
Conventions: return typed gRPC errors early (`grpcstatus.Errorf(grpccodes.FailedPrecondition, …)`); klog
structured logging; a single node-wide `allocateMutex sync.Mutex` on `DevicesReconciler` serializing the
critical section (released before I/O); table-driven tests with a fake client.
### Implementation Plan (shipped)
The fix landed as the change surface below, in dependency order, each step leaving the package building and
testable. Two approaches explored early were dropped during implementation — a `ListAndWatch` withhold and a
per-card lock map — see **Alternatives**.

- [x] **Race-safe cross-mode query + `Allocate` reject (raw-path core).** Added
  `DevicesReconciler.reservedModeForResource(group, device)` (scans the reservation map under an RLock: "what
  mode holds this physical card") and `ResourceServer.cardHeldInOtherMode` (= the card's `Devices.Status` mode
  OR `reservedModeForResource`; conflict when `mode != None && mode != s.AllocationMode`). `Allocate`, before
  stamping the mode, returns `FailedPrecondition` on any conflicting requested card.
- [x] **Reserve-before-patch with rollback.** `reserveDevices` runs inside the critical section (the card is
  taken the instant the check passes, so a concurrent other-mode `Allocate` sees it); `patchAllocatingPod` runs
  outside; a failed patch calls `releaseReservation` to roll back (else the card strands — the Pod-delete prune
  is gated on the annotation that never landed).
- [x] **Per-node serialization + distinct-pod identification.** A single `allocateMutex sync.Mutex` on
  `DevicesReconciler` serializes *identify → cross-mode check → reserve* (released before the annotation
  patch). `getAllocatingPod`/`getAllocatingPodWithRetry` gained a `skipReserved bool`: the workload `Allocate`
  passes `true` (skip pods already holding a reservation), while `GetPreferredAllocation` (advisory) and
  `allocateVisibility` (must re-find its own reserved pod) pass `false`. Serialize + skip together (neither
  alone) map a concurrent batch one-to-one to distinct pods, and close the check→reserve TOCTOU.
- [x] **`ListAndWatch` health-only.** `getListAndWatchResponse` advertises every healthy card for its mode
  regardless of cross-mode holds; it never withholds by allocation state (which would desync kubelet's device
  accounting). Enforcement rests on the `Allocate` reject alone.
- [x] **Prompt release on Pod termination.** `pruneReservations(livePodUIDs)` runs in the same `Reconcile` sweep
  that rebuilds `Devices.Status`, so the reservation and the ledger free a card together when its Pod
  disappears.
- [x] **Dual-layer regression.** Table-driven `-race` unit tests in `pkg/deviceplugin/server_test.go`
  (`pkg/deviceplugin` coverage 35.2% → 39.8%) plus the committed e2e `cases/case-22.sh`, hardened to fill the
  exclusive cards sequentially and to require a concrete held-reason before scoring a held PASS.

**Verified on real hardware (2-card nvidia node, 2026-07-16):** focused lint 0 issues; the fixed image was
built, pushed, and deployed by digest (worker + device-manager). CASE 22 passes on both variants (A held via
the Kueue admission gate; B held via the device-plugin `FailedPrecondition` → `UnexpectedAdmissionError`). A
concurrent Kueue batch of two `nvidia.com/gpu:1` Pods mapped each to a distinct card, each pod's allocation
annotation matched its own `NVIDIA_VISIBLE_DEVICES`, and the ledger read `remaining=0`.

### Test Plan
[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
None — the existing `pkg/deviceplugin/server_test.go` harness (controller-runtime fake client + `stubResponder`,
table-driven `t.Run`) already covers `Allocate` / reservation / `ListAndWatch`; new cases extend it in place.

#### Unit tests
- `pkg/deviceplugin`: `2026-07-16` — baseline `35.2%` (pre-change); `39.8%` after. In `server_test.go`
  (table-driven, fake client, run with `-race`):
  - `Allocate` cross-mode reject: shared onto an Exclusive-held card (via Status, and via reservation only, i.e.
    ledger not yet reconciled) → `FailedPrecondition`; free card and sliced-on-sliced → success.
  - `getListAndWatchResponse` health-only: a card held in another mode (via Status or reservation) is STILL
    advertised by the shared server — no cross-mode withhold.
  - Concurrent `Allocate` (node `allocateMutex`): simultaneous exclusive+shared for one/two cards → exactly one
    success + one `FailedPrecondition`, no deadlock.
  - Distinct-pod identification: two identical exclusive Pods pending together, one `Allocate` per distinct card
    → each pod reserved for a distinct card (both accounted); `getAllocatingPod(skipReserved=true)` skips an
    already-reserved pod while `skipReserved=false` returns the oldest.
  - Release counting on Pod termination: a card held by pod A (reserved); after A leaves the live-pod set, a
    prune drops the reservation → `cardHeldInOtherMode` returns false and an opposite-mode `Allocate` succeeds on
    it. Also assert the card is NOT freed while A is still live, and a failed patch rolls the reservation back.

#### Integration tests
None — the device-plugin gRPC surface is exercised by the fake-client unit tests; the full NFD→Worker→Kueue
chain is covered by e2e.

#### e2e tests
`cases/case-22.sh` — on a real ≥2-card nvidia node, both variants: variant A (Kueue LocalQueue label) and
variant B (raw, no label). Asserts an exclusive and a shared claim never occupy the same physical card, and a
held shared claim is held for a concrete reason (device-plugin refusal / Kueue gate / Unschedulable). Fills the
exclusive cards **sequentially** (create one, wait until Running with a correct allocation annotation and the
ledger decremented, then the next) so the "all cards exclusive" precondition is deterministic without depending
on the concurrent-admission path, which the distinct-pod unit test covers directly. Both variants PASS on the
fixed image.

## Alternatives
- **Enforce only in the Kueue node-devices AdmissionCheck** (reject cross-mode) — rejected: does not cover the
  raw non-Kueue path, which is a first-class supported path (any Pod may request the extended resources).
- **Single unified cross-mode device-ID pool** (one resource, kubelet sees one card once) — rejected: a much
  larger redesign of the resource-name model and the InstanceType three-view; high blast radius.
- **`ListAndWatch` withhold** (steer kubelet away from held cards by dropping their device-IDs) — **tried in the
  PoC and dropped.** It made the free-card race disappear by never offering the conflicting token, but
  withholding by allocation state makes the advertised allocatable count fluctuate with allocation (the external
  Detector drives the stream), desynchronizing kubelet's device accounting. Enforcement rests on the `Allocate`
  reject alone; the withhold is not shipped.
- **Per-card lock map** (a `sync.Mutex` per physical card, taken in a stable order) — **tried and superseded** by
  the single node-wide `allocateMutex`: `Allocate` is an infrequent, in-memory admission operation, so one lock
  has negligible cost, cannot deadlock (no ordering), and additionally serializes pod identification.
- **External lock for pod-serialization** — a `coordination.k8s.io` Lease named per node (HAMi-style NodeLocker),
  or locking the Node object — rejected: physical-card allocation is single-writer per node (one device-manager
  DaemonSet process), so an in-process `allocateMutex` suffices; an external lock adds API round-trips, renewal,
  and split-brain handling for a non-distributed operation, and locking the Node object churns every Node watcher.

## Resolved Decisions
- **Multi-card `Allocate` lock ordering/atomicity** → one node-wide `allocateMutex` (no per-card lock map, no
  ordering, cannot deadlock) serializes the whole critical section.
- **Withhold via `Unhealthy` vs omit in `ListAndWatch`** → moot: `ListAndWatch` withholds nothing; it reflects
  health, and enforcement lives in `Allocate`.
- **`getAllocatingPod` misattribution under concurrent identical-Pod admission** → the node `allocateMutex`
  serializes identification and `getAllocatingPod` skips already-reserved pods, so each concurrent `Allocate`
  maps to a distinct pod. `UnsafeDisableDeepCopy` in `getAllocatingPod` was investigated and ruled out as the
  cause (the patch deep-copies before mutating; nothing mutates the shared cache object).
