# Case rationale: why a real cluster, and how the accelerated cases are mocked

Background for CASE 2–6 of `specs/2026-06-29-instancetype-unified-pool-refactor.md`. Read this
before running or debugging those cases.

Post-refactor shape the cases assume: unit specs are **Queue/InstanceType-managed** (not on the
node); all three modes fold into **one isolated ClusterQueue per pool** (**Cohort removed** — there
is no borrow topology and **no `schedule.gpustack.ai/drain` tombstone** anymore); `InstanceType` is a
**real CRD** whose `.status` three-view is materialized by `InstanceTypeReconciler`; teardown flows
through the InstanceType **finalizer** (`HoldAndDrain` the backing CQ, then delete it).

## Shared accelerated mock recipe (CASE 4 and CASE 6)

A GPU-less node produces no accelerator labels and no `Devices` ledger, so two inputs are mocked; the
derivation and the three-view / AdmissionCheck math are **not** mocked.

1. **A fake accelerator NodeFeature** — NFD merges its labels onto `Node.Labels`, and the Worker
   derives the accelerated `ResourceFlavor` → `InstanceType` → `ClusterQueue`. Minimal set (validated
   against `ConstructNodeCapacityLabels` in `pkg/nodefeature/helper.go`):

   ```
   acceleratable.feature.gpustack.ai/<manu>-<id>        = "true"   # <manu> a known manufacturer, e.g. nvidia
   acceleratable.feature.gpustack.ai/<manu>-<id>.count  = "8"      # > 0; GATES derivation (and sets card count)
   ```

   `.product`/`.memory`/`.cores` add realism but are not the gate. The NodeFeature must carry
   `metadata.labels."nfd.node.kubernetes.io/node-name": <node>` so NFD merges it onto that node.

2. **A phantom-node `Devices` CR** carrying the per-card ledger the `InstanceTypeReconciler` reads.
   It is named for a node the DeviceManager DaemonSet never runs on (so `NodeDevicesReconciler` leaves
   it untouched) and carries the pool's reverse-lookup labels — the feature key +
   `kubernetes.io/os|arch` + `gpustack.ai/managed=true`. The per-card occupancy lives in its
   **`.status`** (`status.groups[].accelerators[].{mode,remaining}`), so it must be written to the
   `/status` subresource.

   > **Patch the `v1alpha1` CRD, not the unversioned/`v1` resource.** `devices` (no version) resolves
   > to `worker.gpustack.ai/v1` — the aggregated proxy — whose `/status` subresource write currently
   > returns `ServiceUnavailable`. Use `kubectl patch devices.v1alpha1.worker.gpustack.ai <name>
   > --subresource=status …` to hit the real CRD. (The aggregated-proxy status write is a separate
   > tracked bug; the DeviceManager writes the ledger via the typed v1alpha1 client, so production is
   > unaffected.)

Card tokens the ledgers use: `mode` `0`=free, `1`=exclusive, `2`=shared, `3`=sliced; `remaining` in
credit units out of `D = 1,600,000` per whole card (`50%` sliced ⇒ `remaining = 50 × D/100`).

## CASE 2 — Instance admits on its queue, then drain stops it (not recreate)

`pkg/worker/controllers/worker/instance.go` + `pkg/worker/webhooks/worker/instance.go` +
`pkg/worker/kuberess/apps_kueue.go`, not covered by CASE 1. Three verified facts:

1. **Admission (finding #5).** A general Instance's Workload also requests `memory`/`ephemeral-storage`
   the CPU-only ClusterQueue does not cover (it covers only `cpu`, F3b). Kueue would refuse to assign a
   flavor for an uncovered resource (`couldn't assign flavors … resource memory`) and never admit —
   unless the deployed Configuration sets `quotaCheckStrategy: IgnoreUndeclared` (F3d), which makes each
   queue check only its covered dimension and ignore the rest. The case creates a running Instance and
   asserts its Workload reaches `Admitted=True`.
2. **Drain removes the pool.** Toggling `gpustack.ai/managed=false` on the `<node>-gpustack-worker`
   NodeFeature drops the node from the flavor index, so the pool's general `ResourceFlavor` is deleted
   (F3a — node-index-driven, immediate, independent of any running Instance/Workload). The old "bump the
   general `ram` capacity label" injection no longer works: a general pool's capacity is the Node's CPU
   count, not a bumpable label. NFD owns `gpustack.ai/managed` (it is in the node's
   `nfd.node.kubernetes.io/feature-labels`), so the NodeFeature edit propagates to the node label.
3. **The running Instance is stopped, not recreated.** As the pool drains, the derived InstanceType
   tears down (`HoldAndDrain` → Kueue evicts the Pod → CQ deleted → IT removed). `instance.go` evaluates
   the gone/`Inactive` type on every reconcile — before (re)creating the Pod — and sets `spec.stop=true`
   (log `stop instance as inactive instance type`), which deletes the Pod and marks the Instance
   `Stopped`. An `InstanceType` watch enqueues the Instance so the stop is prompt even when no Pod event
   fires. This closed a pre-existing gap (`1afc5b5` on main) where the stop check sat inside the
   `pod == nil` branch with no InstanceType watch: an evicted Pod was recreated (the type still looked
   `Active` at that instant) and the running Instance was left with a stuck Pending Pod, never stopped.

Why a real cluster: the Instance's Pod carries the `kueue.x-k8s.io/queue-name` label, so it is
admission-managed; the admission decision, the eviction, and the managed-toggle drain propagation cannot
be observed with the fake client.

## CASE 3 — managed-toggle scopes node onboarding (Story 5)

Excluding a node (`gpustack.ai/managed=false`, toggled via the NodeFeature — NFD reverts a direct node
label) must remove its pool contribution. **What changed post-refactor:** there is **no drain
tombstone** — `NodeFlavorReconciler` *deletes* the flavor when no node contributes (F3a), and the
derived `InstanceType`'s finalizer then drives the CQ through `HoldAndDrain` and deletes it (F5d). So
the case asserts the flavor is **deleted** and the derived InstanceType **tears down** (CQ
`HoldAndDrain` or gone), *not* a `schedule.gpustack.ai/drain=true` annotation.

Non-obvious: a managed toggle changes only `gpustack.ai/managed` — no feature label — so it converges
**only if** `NodeFlavorReconciler`'s Node-watch predicate includes `systemname.ManagedLabelKey`.
**A restart masks a missing predicate** (the `For`-watch start-up resync re-lists everything), so
verify against a **continuously running** operator.

## CASE 4 — AdmissionCheck holds exclusive over-admit (Story 4)

Uses the shared accelerated mock (count=8) plus a ledger where **all 8 cards are 50%-sliced — no clean
whole card**. A request for 5 exclusive cards passes coarse `credits` (gate 1: `5×M ≤ 8×M`) but the
node-devices **AdmissionCheck** (gate 3) must hold it (`Retry`, not `Rejected` — transient) because no
card can host a whole exclusive card.

Wiring that must hold: `installKueue` applies the `gpustack-node-devices` AdmissionCheck object right
after the Kueue install; `NodeDevicesAdmissionCheckReconciler` sets its `Active=True`; and
`InstanceTypeReconciler.ensureClusterQueue` references it in `spec.admissionChecksStrategy` **only when
`acceleratable && derived && the AC is Active`**. The Instance's Pod → Kueue `Workload` gets a quota
reservation, then the AC reads the phantom ledger (uncached, via `APIReader`) and writes
`admissionChecks[gpustack-node-devices].state = Retry`; the Workload never reaches `Admitted`.

## CASE 5 — Pod webhook folds slice-by-memory-% into units (Story 3)

Pure GPU-less check of the `pods` CREATE webhook (objectSelector on `kueue.x-k8s.io/queue-name`,
`failurePolicy: Fail`; webhook set is `{Instance, Pod}`). A `.sliced` Pod requesting
`.sliced.memory-percentage: 20` must be **mutated** to `.sliced.units = 20 × M/100 = 320000` with
`.sliced.cores-percentage` defaulted to `100`; a `.sliced` Pod with **no** memory (neither percentage
nor mib) must be **rejected** by the validating webhook. The memory-**percentage** fold is a pure
`×16000` computation, so it needs no card VRAM and no real accelerator — only the installed webhook.
The Pods are never expected to schedule (the node has no `nvidia.com/gpu.sliced`); the webhook fires
at CREATE, so the mutated/validated request is observable on the persisted Pod.

Ordering invariant (pinned by a comment in `webhooks/setup.go`): our mutating configuration name
`gpustack-worker-mutation` must sort before `kueue-mutating-webhook-configuration` (`g` < `k`) so our
fold runs before Kueue hashes the container resources.

## CASE 6 — Pooled three-view + watch freshness (Story 2/6)

Uses the shared accelerated mock. Walks the five-step pooling sequence and asserts the InstanceType
three-view (`.status.accelerator/.acceleratorShared/.acceleratorSliced`) matches the oracle exactly:
`8/80/800 → 6/60/600 → 4/58/400 → 2/38/360 → 2/38/356 → 1/28/256`. Also asserts **watch freshness** (a
native `kubectl get instancetype -w` observes the `.status` move as the ledger allocs/frees — the whole
point of promoting InstanceType to a real CRD; the old aggregated projection could not emit
`Devices`-driven changes), **unit-spec edit** through the InstanceType API persisting on the
InstanceType spec (`spec.unitResources.cpu`) — never a CQ note or a `Node`/NodeFeature — and **zero
Cohort** objects.

The three-view is a per-card bin-packing projection the reconciler computes over the mocked ledger;
because it reads `Devices.status`, the mock must be written to the **v1alpha1** `/status` subresource
(see the shared recipe warning). A three-view stuck at `0/0/0` almost always means the status patch
did not land (wrong API version) or the ledger's reverse-lookup labels do not match the pool's.

## Skill-specific troubleshooting

- **CASE 2 pool ResourceFlavor never drains** — the `gpustack.ai/managed=false` patch must land on the
  `<node>-gpustack-worker` NodeFeature (Worker-authored), not the Node directly (NFD owns and overwrites
  the label). Confirm NFD merged it to the node
  (`kubectl get node <n> -o jsonpath='{.metadata.labels.gpustack\.ai/managed}'`).
- **CASE 2 instance not stopped after drain** — the running Instance must reach `spec.stop=true` /
  `Stopped` when its InstanceType drains. Confirm the InstanceType went `Inactive`/gone (drain
  propagated) and that the operator image includes the drain-stop fix — a stale image predating it
  leaves the evicted Pod recreated and stuck `Pending`. Ground truth:
  `kubectl -n <ns> logs deploy/gpustack-operator-worker | grep 'stop instance as inactive'`.
- **CASE 3 nothing tears down** — confirm the operator was not restarted between the toggle and the
  assertion (a restart's resync converges regardless of the predicate and masks a bug).
- **CASE 4 workload never gets a check state** — confirm the AC is `Active` and the accelerated CQ
  references it (`kubectl get cq <name> -o jsonpath='{.spec.admissionChecksStrategy}'`); confirm the
  phantom Devices carries the pool's feature-key + `kubernetes.io/os|arch` + `gpustack.ai/managed=true`.
- **CASE 6 three-view stuck at 0/0/0** — the ledger status patch did not land: target
  `devices.v1alpha1.worker.gpustack.ai --subresource=status` (the `v1` proxy returns
  `ServiceUnavailable`), and verify the phantom Devices' reverse-lookup labels match the InstanceType's.
- **Accelerated InstanceType never materializes** — `instance-type-derived-from-node` must be on
  (default true); confirm the fake accelerator NodeFeature merged onto `Node.Labels`.
