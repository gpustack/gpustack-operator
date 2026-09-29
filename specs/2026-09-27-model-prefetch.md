# Spec: Model prefetch, retention and tenant budgets (S6)

Status: Shipped
Type: Feature

## Summary

Node-side model caching (shipped in S1–S4) downloads weights when the first Pod mounts them. S6 adds
the ability to push weights to nodes *before* any Pod asks: a tenant declares a `ModelPrefetch`
against a `ModelArtifact`, and the operator lands the weights on the target nodes by running
per-node warm-up Pods through the existing node-delivery path. Two admin objects bound the blast
radius: `ModelStore` carries per-pool cache policy (watermarks, concurrency, bandwidth, sync
tuning), and `ModelStoreBinding` is the namespaced provisioning point that grants a tenant
namespace a byte budget on specific pools and the right to pin. Prefetched content that a tenant
pins survives LRU eviction; everything else is collected by the existing watermark GC once
unreferenced and past its grace.

## Motivation

### Goals

- A tenant can warm a model onto a node set before scheduling, so the first Pod on each node starts
  in seconds instead of after a full download (cold-start elimination).
- Prefetch consumes a per-namespace budget that an admin granted, measured in filesystem bytes, and
  cannot exceed it: an over-budget prefetch is refused at admission.
- An admin can tune the node cache per node pool without touching global Settings, and the two
  pools' selector scopes can never silently overlap.
- Pinning is an admin-granted capability, never a tenant default.
- Deletion semantics stay lazy and safe: deleting a `ModelPrefetch` revokes the residency intent;
  bytes are reclaimed by GC only after the content is unreferenced and past grace.

### Non-Goals

- DeliveryClass and external providers; OCI `image` sources; peer sync between nodes;
  LoRA / draft-model weights; cross-namespace prefetch (a prefetch warms only into its own
  namespace's budget).
- Any new node-side transfer mechanism: warm-up Pods ride the existing CSI node-delivery path
  (`model.csi.gpustack.ai` inline ephemeral volumes) unchanged.
- Enforcing the byte budget inside the plugin at mount time. The budget gates *prefetch admission*
  and *accounting*; a workload that mounts an artifact through the normal path consumes node cache
  under the pool watermarks as it does today, not under the tenant budget.

## Proposal

Three new APIs and one extension of an existing one:

- `ModelStore` (cluster-scoped, admin): a node pool's cache policy. Selected by `nodeSelector`;
  at most one store may select any node, and overlapping scopes are a status condition, not a
  silent tie-break. Fields mirror the effective `NodeModelStoreSpec` shape: `watermarks`,
  `download` (concurrency, bytesPerSecond). Cache root path is deliberately absent (deployment-time
  hostPath, not a per-pool knob).
- `ModelStoreBinding` (namespaced, admin-created, tenant-readable): grants the namespace (a) quota
  `bytes` counted as filesystem usage, and (b) `allowPinned`. Creation is a privileged act in the
  same sense `KVCachePoolBinding` is: it is RBAC-gated and auditable, the webhook enforces
  immutability of the grant, and every figure a tenant sees about its own consumption lives on this
  object's status (`usedBytes`, `OverQuota` condition). `storeRefs` is strictly required — there is
  no optional default-pool grant: authorization must be explicit, the chart's default pool is
  deployment configuration rather than an authorization basis, and a fallback would create a second
  source of truth for who may warm where.
- `ModelPrefetch` (namespaced, tenant): names an `artifactRef` and a `bindingRef`, a placement
  (`instanceTypes` XOR `nodeSelector`, default = the InstanceTypes of the namespace's own
  ModelDeployment roles that reference the artifact), `minReady` (0 = all target nodes), and
  retention (`pinned: false` default, `ttlAfterLastUse`). Status aggregates per-node facts into
  `desiredNodes` / `readyNodes` / `downloadingNodes` and conditions (`Progressing`, `Available`,
  `Degraded`). ModelPrefetch gets a v1 aggregated view for the GPUStack server read surface;
  ModelStore and ModelStoreBinding are CRD-only (admin objects, precedent KVCachePool /
  KVCachePoolBinding). The v1 view is status-only by design: per-node facts already live in
  `NodeModelStore.status`, so a progress-style subresource adds nothing the aggregates need; if one
  is ever wanted it follows the existing artifact `progress` pattern (stored aggregation plus a
  bounded direct-to-plugin live path, `docs/reference/model-artifact-views.md`) — an additive
  change, not a v1 break.
- `NodeModelStore.spec` gains two fields the worker writes and the plugin consumes: `store` (the
  name of the effective ModelStore, empty = cluster defaults) and `pinned` (the digests this node
  must retain). The plugin excludes pinned digests from GC candidates while they still count toward
  usage; everything else about LRU and watermarks is unchanged.

The prefetch controller implements delivery as **warm-up Pods**: one Pod per target node, same
namespace, inline CSI volume naming the artifact, no accelerator resources, pinned by `nodeName`,
completing (exit 0) once the materialized tree is readable. Measured facts this design relies on
(PoC-H, kind 1.29.14, chart-built operator image):

- A restricted-PSA namespace admits the warm-up shape; `nodeName` and required nodeAffinity both
  work; the Pod reads every file with the source's SHA-256 and exits Succeeded.
- A warm-up Pod **without** the `kueue.x-k8s.io/queue-name` label is invisible to Kueue: no
  Workload, no schedulingGates, no quota. The same Pod *with* the label is held by Kueue's
  `admission` + `topology` gates and its Workload never reserves quota ("no TAS flavor assigned"),
  so the label must never be rendered.
- The operator's own Pod webhook (Gate 1) selects on `queue-name Exists`, so it never touches
  warm-up Pods; no webhook rule is added for them.
- After the Pod exits and is deleted, the node's reference for the digest drops to zero while the
  published tree stays; retention is purely the GC/LRU layer's decision.

### User Stories

#### Story 1
As a tenant ML engineer, I want to declare a ModelPrefetch for my model so that its weights are on
every node of my target pool before I scale up, and my first Pod per node starts in seconds.

#### Story 2
As a tenant ML engineer, I want to see how far my prefetch is (desired/ready/downloading nodes) and
get Available only when minReady nodes hold the weights.

#### Story 3
As an admin, I want to grant team-a 2 TiB of model cache across the h100 pool — and nothing on the
mi300 pool — with one namespaced object I can audit, and take the grant away by deleting it.

#### Story 4
As an admin, I want the h100 pool to keep more free disk than the default (higher collection
threshold) without touching the cluster-wide Settings, so the pool's cache rides its dedicated NVMe.

#### Story 5
As an admin, I want a tenant's pinned model to survive other tenants' churn on shared nodes, but
only for namespaces where I opted pinning in.

### Core Features & Acceptance Criteria

**F1 ModelStore API.** Cluster-scoped CRD `modelstores.worker.gpustack.ai`. Spec:
`nodeSelector` (required, LabelSelector), `watermarks.highPercent` / `lowPercent` (optional,
validated 2..95 / 1..94, low < high), `download.concurrency` (optional, 1..64),
`download.bytesPerSecond` (optional, >= 0). Status: `nodes` (count of matched nodes),
`capacity` (summed totalBytes/storedBytes of matched nodes' NodeModelStores), conditions
including `Ready` and `SelectorOverlap`.
- AC1: a ModelStore matching a node overrides only the fields it sets; unselected nodes keep
  cluster defaults (Setting-derived), verified in the node's `NodeModelStore.spec`.
- AC2: two ModelStores whose selectors both match one node set `SelectorOverlap=True` on both;
  the worker picks the alphabetically first name deterministically and records it in the node's
  `spec.store`.

**F2 ModelStoreBinding API.** Namespaced CRD. Spec: `storeRefs` (required, >= 1 names of
ModelStores this namespace may prefetch into; immutable), `quota.bytes` (required, positive
resource.Quantity; immutable except growing), `allowPinned` (optional, default false; may only go
false→true). Status: `usedBytes`, conditions `Ready` / `OverQuota`.
- AC3: creating a Binding is refused when a `storeRef` names no ModelStore or a quota is
  non-positive; every spec field is immutable (webhook-enforced) except `allowPinned` false→true
  and `quota.bytes` growing — an increase passes, a decrease or any other spec change is refused.
- AC4: `usedBytes` equals the full size of every distinct digest any of the namespace's prefetches
  targets on any node holding it, summed per digest per node — shared trees count fully against
  every namespace that references them (conservative, cannot be gamed).

**F3 ModelPrefetch API + warm-up delivery.** Namespaced CRD with v1 view. Spec: `artifactRef.name`
(required, same namespace), `bindingRef.name` (required, same namespace), `placement` (exactly one
of `instanceTypes` / `nodeSelector`; both absent = derived from referencing roles; both present =
refused), `minReady` (default 0 = all), `retention.pinned` (default false),
`retention.ttlAfterLastUse` (optional duration). TTL expiry is enforced at the hour granularity the
plugin already reports in `NodeModelStore.status.models[].lastUsedTime`: retention semantics do not
need minute precision, and requiring finer recording would multiply plugin status writes for a
decision that cannot tell the difference. Status: `desiredNodes`, `readyNodes`,
`downloadingNodes`, conditions `Progressing` / `Available` / `Degraded`.
- AC5: reconciling a prefetch creates at most one warm-up Pod per target node, pinned by
  `nodeName`, rendering the artifact's resolved digest and the CSI volume exactly as the consumer
  path does, never rendering `kueue.x-k8s.io/queue-name`, and requesting no accelerator resources.
- AC6: a node counts ready when its `NodeModelStore.status.models` lists the digest `Ready`;
  `Available` becomes True when `readyNodes >= max(minReady, 0)` interpreted as: minReady=0 means
  all desired nodes.
- AC7: prefetch admission refuses: unknown artifact or binding, binding not granted one of the
  target stores, projected bytes over quota, `pinned=true` while `allowPinned=false`.
- AC8: deleting the prefetch deletes its warm-up Pods, drops its digests from the nodes' `pinned`
  lists, and stops counting toward `usedBytes`; bytes are reclaimed by the plugin's watermark GC
  only when unreferenced and past the existing grace.
- AC9: with `ttlAfterLastUse` set, a node whose digest has not been mounted within the TTL stops
  counting as ready and is unpinned on that node (the model stays if someone mounts it again).

**F4 L3 config merge.** `NodeModelStoreReconciler.effectiveSpec` gains a second layer: after the
cluster Settings layer, the worker appends the layer derived from the (deterministically chosen)
ModelStore matching the node, then merges field-by-field (existing `modelstore.Layer` semantics)
and writes the chosen store's name into `spec.store`.
- AC10: a node's spec reflects ModelStore overrides within one reconcile of either the Setting
  change or the ModelStore change; `spec.store` names the winner or is empty.

**F5 Plugin pinned consumption.** The plugin reads `spec.pinned` on its NodeModelStore; pinned
digests are excluded from GC candidates (never evicted by watermark collection) while remaining
counted in usage and capacity reporting.
- AC11: with usage above the high watermark and only pinned + referenced content present, the
  collector removes nothing and reports the saturated state it reports today (no new failure mode).

### Notes / Constraints / Caveats

- Version floor: unchanged from the project's — functional floor Kubernetes 1.29 (the bundled
  Kueue v0.18.9's requirement; the chart's install floor stays 1.23). New API surface uses only
  CRD v1 + strategic-merge patch (>= 1.22); no CEL rules, no schedulingGates. PoC-H ran on kind
  node image v1.29.14; behavior under PSA Restricted is a 1.25+ stable feature, well below the
  floor.
- Names must avoid KV-cache vocabulary.
- Budget accounting is filesystem-usage-based and per-namespace-full-amount; it is admission +
  accounting policy, not a mount-time enforcement boundary — documented as such, in the same spirit
  as the KVCachePoolBinding non-enforcement note.
- Warm-up Pods are ordinary namespaced Pods restricted-compliant by construction
  (runAsNonRoot 65534, RuntimeDefault, drop ALL, no privilege escalation, no hostPath) so they pass
  any restricted PSA namespace; PoC-H verified the shape end to end.
- The prefetch controller's target-node expansion: `nodeSelector` selects nodes directly;
  `instanceTypes` resolves via the flavor the InstanceType is derived from to the nodes carrying
  it; the derived default (no placement given) unions the InstanceTypes of ModelDeployment roles in
  the namespace whose artifact matches. Target sets are recomputed on relevant watches (artifact
  resolution, node set, InstanceType, ModelDeployment changes).
- Immutability rules follow the KVCachePoolBinding precedent (schema where possible, webhook where
  relational): re-pointing a grant or shrinking a quota would silently strand bytes or break
  accounting.

### Boundaries

- **Always:** keep warm-up Pods label-free of `kueue.x-k8s.io/queue-name`; write pinned lists only
  through the worker (plugin never writes spec); keep per-node facts on NodeModelStore status and
  per-namespace aggregates on the prefetch/Binding status.
- **Ask first:** any need to enforce the budget at mount time in the plugin (product behavior
  change); any widening of who may create ModelStoreBinding beyond RBAC.
- **Never:** let two ModelStore selectors overlap silently; let a tenant object override admin
  config (endpoints, proxies, watermarks stay admin-only); reference the task-tracking report from
  repo documents.

### Risks and Mitigations

- Warm-up Pod storm on a large pool (e.g. 200 nodes, one prefetch) → controller creates Pods in
  bounded batches and the API surfaces Progressing; node fan-out is inherent to the ask and matches
  one-Pod-per-node delivery.
- Accounting drift between projected (admission) and actual (status) bytes → admission projects
  from the artifact's resolved manifest size; the reconciler recomputes `usedBytes` from
  NodeModelStore status and moves the Binding's `OverQuota` condition; a digest that fails to
  materialize never counts.
- ModelStore selector overlap misconfiguration → explicit `SelectorOverlap` condition +
  deterministic alphabetical winner, never a silent union.
- Pinned content wedges a node's disk (tenant pins more than the disk) → pinned admission is
  bounded by the same quota check at prefetch admission; the plugin still refuses to publish when
  the reservation cannot fit, and the prefetch reports `Degraded`.
- Two writers on one `NodeModelStore.spec` (the L1–L3 configuration reconciler vs the per-node
  pinned writer) → strict field ownership: the prefetch controller writes only `spec.pinned`, the
  configuration reconciler preserves the stored `pinned` byte-for-byte when composing the rest of
  the spec, and both rely on the existing conflict-requeue path; a test pins this split (pinned
  survives a Settings-only reconcile, and watermarks survive a pinned-only write).

## Design Details

### Commands

- `make generate` after API edits (deepcopy, register, CRDs, applyconfigurations, openapi,
  protobuf for the aggregated group, webhooks).
- `make lint` (+ `make lint docs` for the docs page).
- Package tests: `go test ./pkg/worker/controllers/worker/... ./pkg/modelmanager/... ./pkg/modelstore/... ./api/worker/...`.
- e2e on local kind: `bash .agents/skills/_e2e-lib/scripts/build-load.sh dev-$(git rev-parse --short HEAD)`
  then `bash .agents/skills/_e2e-lib/scripts/deploy.sh gpustack-system dev-<hash>`, case runner
  under `.agents/skills/gpustack-operator-e2e/cases/`.

### Project Structure

- `api/worker/v1alpha1/model_store.go` — ModelStore + list (+ status types).
- `api/worker/v1alpha1/model_store_binding.go` — ModelStoreBinding + list.
- `api/worker/v1alpha1/model_prefetch.go` — ModelPrefetch + list.
- `api/worker/v1alpha1/node_model_store.go` — add `store` and `pinned` to
  `NodeModelStoreSpec` (worker-written, plugin-consumed).
- `pkg/worker/extensionapis/worker/model_prefetch.go` — v1 view + TableConvertor + printer columns
  (precedent: model_artifact view).
- `pkg/worker/controllers/worker/model_prefetch.go` — target expansion, warm-up Pod lifecycle,
  status aggregation, pinned-list computation, TTL expiry.
- `pkg/worker/controllers/worker/model_store.go` — overlap detection, node/capacity aggregation.
- `pkg/worker/controllers/worker/model_store_binding.go` — usedBytes accounting, OverQuota.
- `pkg/worker/controllers/worker/node_model_store.go` — L3 merge and `spec.store`.
- `pkg/worker/webhooks/worker/model_store_binding.go`, `model_prefetch.go` — admission.
- `pkg/modelstore/config.go` — nothing structural; Layer already supports field-level override.
- `pkg/modelmanager/gc/gc.go` + `pkg/modelmanager/manager.go` — consume `spec.pinned`
  (excluded from candidates, counted in usage).
- `docs/` new page + `docs/README.md` index entry (routing per the docs skill).
- `.agents/skills/gpustack-operator-e2e/cases/case-<NNN>.sh` — number taken at my-ship time as
  current main's max case number + 1.

### Code Style

Follow repo conventions; illustrative pieces only. The layer merge is already in place:

```go
// pkg/modelstore/config.go (existing)
type Layer struct {
	HighWatermarkPercent   *int32
	LowWatermarkPercent    *int32
	DownloadConcurrency    *int32
	DownloadBytesPerSecond *int64
	// Hub fields...
}

func Merge(layers ...Layer) workercore.NodeModelStoreSpec {
	var spec workercore.NodeModelStoreSpec
	for _, l := range layers {
		overlay(&spec.Watermarks.HighPercent, l.HighWatermarkPercent)
		// ...
	}
	return spec
}
```

The warm-up Pod shape (fields that matter; verified working under restricted PSA):

```yaml
spec:
  nodeName: <target>
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: warm
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: model, mountPath: /model, readOnly: true}]
  volumes:
    - name: model
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes: {artifact: ..., artifactUID: ..., manifestDigest: ...}
```

### Implementation Plan

Tasks run sequentially in one seat (repo working agreement): every task starts with
`git fetch origin && git rebase origin/main`, image builds and e2e runs rebase immediately before
they run, and each task lands its own `--signoff` commit (folded per module at ship). No per-task
review gates; the stage gates go to the coordinator (draft confirmed, plan gate, built gate).

- [x] **T1 · API surface: three CRDs + NodeModelStore fields + codegen**
      Blocked by: None
      Owns: `api/worker/v1alpha1/model_store.go`, `api/worker/v1alpha1/model_store_binding.go`,
      `api/worker/v1alpha1/model_prefetch.go`, `api/worker/v1alpha1/node_model_store.go`,
      `api/worker/v1/**` (prefetch view type if separate), generated trees
      (`zz_generated.*`, applyconfigurations, openapi, webhook manifests), chart RBAC entries for
      the new resources if the role templates enumerate them
      Acceptance: types follow the KVCachePoolBinding / NodeModelStore precedents (markers,
      protobuf tags, printcolumns); `store` and `pinned` on `NodeModelStoreSpec` documented as
      worker-written; `make generate` leaves a clean tree; schema-level defaults/bounds carry
      API-level tests.
      Verify: `make generate && go build ./... && go test ./api/worker/...`

- [x] **T2 · L3 merge + SelectorOverlap (worker)**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/node_model_store.go`,
      `pkg/worker/controllers/worker/model_store.go` (+ tests)
      Acceptance: AC1, AC2, AC10 — a node's spec reflects only the overriding fields; two
      overlapping stores set `SelectorOverlap` on both and resolve to the alphabetically first
      name; `spec.store` records the winner or is empty; the reconciler preserves the
      `spec.pinned` field another controller owns.
      Verify: `go test ./pkg/worker/controllers/worker/... ./pkg/modelstore/...`

- [x] **T3 · plugin pinned consumption**
      Blocked by: T1
      Owns: `pkg/modelmanager/gc/gc.go`, `pkg/modelmanager/manager.go` (+ tests)
      Acceptance: AC11 — pinned digests are excluded from GC candidates while still counted in
      usage and capacity reporting; the saturated-state report is unchanged.
      Verify: `go test ./pkg/modelmanager/...`

- [x] **T4 · prefetch controller (delivery + aggregation)**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/model_prefetch.go` (+ tests),
      `pkg/worker/controllers/setup.go`
      Acceptance: AC5, AC6, AC9 — at most one warm-up Pod per target node with exactly the
      restricted-compliant, label-free shape; readiness read through
      `modelstore.AggregateEntries` / `NodeEntries`; TTL expiry at the plugin's hour granularity;
      `Progressing` / `Available` / `Degraded` conditions; prefetch deletion removes its Pods.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T5 · admission webhooks**
      Blocked by: T1
      Owns: `pkg/worker/webhooks/worker/model_store_binding.go`,
      `pkg/worker/webhooks/worker/model_prefetch.go`, `pkg/worker/webhooks/setup.go` (+ tests)
      Acceptance: AC3 — binding refusals (unknown storeRef, non-positive quota, every-field
      immutability except `allowPinned` false→true and `quota.bytes` growing; the refusal table
      carries a quota-increase-passes case and a quota-decrease-refused case); AC7 — prefetch
      refusals (unknown artifact or binding, target store not granted, projected bytes over quota,
      `pinned` without permission).
      Verify: `go test ./pkg/worker/webhooks/worker/...`

- [x] **T6 · accounting + per-node pinned writer**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/model_store_binding.go`,
      `pkg/worker/controllers/worker/model_prefetch.go` (pinned subset) (+ tests),
      `pkg/worker/controllers/setup.go`
      Acceptance: AC4, AC8 — `usedBytes` counts each distinct digest at full size per holding node
      (absent, not zero, when unmeasurable); prefetch deletion unpins and stops counting; the
      per-node union of pinned digests is written to `NodeModelStore.spec.pinned` under the field
      ownership split (prefetch controller writes `pinned`; the L1–L3 reconciler preserves it).
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T7 · v1 view for ModelPrefetch**
      Blocked by: T4
      Owns: `pkg/worker/extensionapis/worker/model_prefetch.go` (+ tests), extension-API
      registration tables
      Acceptance: status-only v1 view with TableConvertor and printer columns per the artifact-view
      precedent; the view serves delete (regression guard pattern).
      Verify: `go test ./pkg/worker/extensionapis/...`

- [x] **T8 · docs**
      Blocked by: T6, T7
      Owns: `docs/**` (one new page + `docs/README.md` index entry + a `docs/settings.md` entry
      for the `model-prefetch-warmup-image` Setting)
      Acceptance: page routing, header/Contents/footer per the docs skill; naming avoids KV-cache
      vocabulary.
      Verify: `make lint docs`

- [x] **T9 · e2e case on kind**
      Blocked by: T2, T3, T5, T6, T7
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-<NNN>.sh` (+ the SKILL.md case-table
      row; `<NNN>` = main's max case number + 1, taken at my-ship after the final rebase)
      Acceptance: the four acceptance scenarios — prefetch reaches `minReady`; over-budget
      prefetch refused; `pinned=true` refused while `allowPinned=false`; deleting the prefetch
      reclaims bytes only after grace and only when unreferenced.
      Verify: `bash .agents/skills/gpustack-operator-e2e/cases/case-<NNN>.sh <ns>` against a kind
      cluster running the image built from the rebased head (`build-load.sh dev-<hash>` first).

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
None beyond the existing suites: `api/worker/...` schema tests, `pkg/modelstore` merge/validate
tests, and the controller/webhook table-driven suites already exist and stay green throughout.

#### Unit tests
- `api/worker/v1alpha1`: new types' defaults, bounds, and immutability-relevant schema facts
  (target: parity with the KVCachePoolBinding tests' shape).
- `pkg/modelstore`: L3 override merge — only named fields change, others keep L2 values; winner
  selection given two layers.
- `pkg/worker/controllers/worker`: SelectorOverlap detection + alphabetical winner; target
  expansion (instanceTypes / nodeSelector / derived-from-roles); warm-up Pod shape (no
  `kueue.x-k8s.io/queue-name`, restricted-compliant, no accelerator request); readiness
  aggregation; TTL expiry at hour granularity; per-node pinned union; `usedBytes` full-amount
  accounting and absent-vs-zero; webhook refusal table (every AC7/AC3 branch).
- `pkg/modelmanager/gc`: pinned exclusion from candidates; usage still counts pinned; saturation
  report unchanged.
- `pkg/worker/extensionapis/worker`: printer columns and TableConvertor of the v1 view; every CRD
  view serves delete.
Every load-bearing new test is proven able to fail by a compile-safe mutation of the mechanism it
guards (mutation → red → revert → green), with the result recorded in the task's handback notes.

#### Integration tests
None as a separate tier: cluster-level behavior is covered by the e2e case, and unit suites use
fake clients per repo convention.

#### e2e tests
`case-<NNN>` (number taken at my-ship) on local kind, four scenarios: prefetch reaches `minReady`
with status aggregation correct; over-budget prefetch refused at admission; `pinned=true` refused
while `allowPinned=false`; prefetch deletion unpins and bytes are reclaimed only after grace and
only when unreferenced. Regression guards: case-101 / 108 / 109 stay green (the plugin's delivery
path is touched only by pinned consumption). Chart-matrix coverage follows the standing Go-change
requirement at my-ship.

## Alternatives

- **A daemon/agent-side prefetch RPC instead of warm-up Pods.** Rejected: the plugin would need a
  tenant-authenticated API and a new job/state machine; PoC-A/PoC-H showed the CSI + Pod path
  already delivers retry semantics, budget-safe publishing and reference counting.
- **Kueue-managed warm-up Pods (queue-name label, zero-consumption queue).** Rejected on measured
  evidence: a labeled Pod is held by Kueue's admission+topology gates and its Workload fails flavor
  assignment ("no TAS flavor assigned") on a plain cpu/mem podset; unlabeled Pods bypass Kueue and
  the operator's own Pod webhook entirely.
- **Budget enforced by the plugin at mount time.** Rejected for this spec: mounts are the workload
  path, not the prefetch path; enforcing there changes product behavior for every consumer (needs
  coordinator/user sign-off) and gains nothing for warm-up, which is already admission-gated.
- **One ModelStore per node (no selectors).** Rejected: per-node objects are already the *effective*
  view (`NodeModelStore`); the pool layer exists so admins declare policy once per pool.

## Open Questions

None at build time. The three draft questions were adjudicated on 2026-09-27:

1. `storeRefs` stays required; no optional default-pool grant (explicit authorization only; the
   chart's default pool is deployment configuration, not an authorization basis) — see F2.
2. `ttlAfterLastUse` is enforced at hour granularity; no finer plugin recording (avoids write
   amplification) — see F3.
3. The v1 view is status-only; a progress-style subresource, if ever needed, follows the artifact
   `progress` pattern as an additive change — see the Proposal.
