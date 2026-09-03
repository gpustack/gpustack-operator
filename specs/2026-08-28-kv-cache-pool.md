# Spec: KV Cache Pool — Quota Domain, Namespace Grant and Reuse-Domain Registry

Status: Shipped
Type: Feature

## Summary

A `KVCacheBackend` declares the physical KV cache — the nodes, the media, the host memory it claims.
Nothing yet says **which namespaces are granted a quota on it, how much of it, and under which reuse
identity**. This spec adds
that layer as two objects and one controller: `KVCachePool`, cluster-scoped, is the quota domain and
the reuse-domain registry over exactly one backend; `KVCachePoolBinding`, namespaced, is the
provisioning point that gives a namespace a quota on a pool, **registers exactly one reuse domain**, and
is the one place where "how much can *my namespace* still write" is legible.

The load-bearing property is a subtraction. Mapping the object model onto what a Mooncake master
already serves — a per-tenant quota ledger behind an admin API, its own approximate-LRU eviction, and
an in-process object→segment map — turns every control-plane responsibility into a CR field plus an
admin-API call. **No new long-running process, no block index of our own, no Valkey or Redis
dependency, no new subchart.** The pool reconciler is an admin-API translator; the Binding reconciler
is one metrics scrape and one status write.

Two CRDs in one spec is a deliberate trade-off, taken with its mitigation stated: see *A trade-off,
not an oversight* below.

## Motivation

### Goals

- **G1 (primary)** A namespace's quota on a KV cache pool is **an object an admin can RBAC on its
  own** — no `KVCachePoolBinding` in a namespace, no quota. The grant must not be a pool name a user
  types into their own workload. It is a provisioning boundary, not an access-control one — see F2
  for what this object does and does not enforce.
- **G2** "How much can my namespace still write" is answerable from **one object**, in the terms the
  storage layer actually keeps: `requestedQuota`, `effectiveQuota`, `usage`, `overQuota`.
- **G3** **No bespoke control plane.** Every responsibility this feature needs already exists in the
  Mooncake master; the operator translates CR fields into its admin API and reads its metrics back.
  Nothing new runs.
- **G4** **The reuse domain, not the namespace, is the isolation unit — and an admin names it.** The
  storage layer's tenant *is* the reuse domain, so naming a domain creates a quota ledger. That makes
  naming a privileged act, and it lands on `KVCachePoolBinding`: one Binding registers exactly one
  domain, so a Binding is 1:1 with a tenant and every quota figure is one series rather than a sum.
- **G5** **A configuration that would abort the master is refused at admission, never written.** The
  master's quota policy file is a hard failure surface: a malformed one terminates the process. The
  operator renders it whole and a webhook pre-validates it.
- **G6** **Referential integrity is a single-scope query in both directions.** A cluster-scoped pool
  lists its Bindings through one indexed query; a Binding lists workloads in its own namespace and
  nowhere else. Nothing walks namespaces one at a time, and no *namespaced* object ever reads across
  a namespace boundary. The one cluster-wide read this spec adds is not referential at all: the
  domain-name uniqueness check (F9), which is a uniqueness constraint over a master-global
  `tenant_id` space and belongs to the cluster-scoped admission path.
- **G7** The generated-artifact risk of introducing two CRDs at once is **isolated in one task** that
  does nothing else.

### Non-Goals

- **Cross-backend quota** — several backends behind one pool, per-medium ceilings, automatic
  spillover. In this spec **`spec.backends` is validated as length exactly 1**, because quota lands
  on a *single* Mooncake master's per-tenant ledger and one master cannot account for bytes held in
  another backend. Cross-backend quota is a later spec.
- **No per-medium quota field.** `spec.type` on a backend is the *implementation* (`mooncake`), while
  NFS/3FS are *media* on `members[].medium` (`DRAM` / `LocalDisk` / `NoF` / `CXL` / `DFS`), so a
  per-tier ceiling is per-**medium**, not per-backend-type — and Mooncake's own quota is a single
  per-tenant scalar, so per-medium ceilings cannot be enforced through it at all. A field whose
  enforcement does not exist is not shipped.
- **No `spec.metadataStore` field, and no block index of our own.** The Mooncake master already holds
  the object→segment map. A field presupposing that we maintain that index would induce a Valkey
  dependency and a new subchart for nothing. The claim is *"not needed at the scale this ships for"*,
  **not** *"never needed"* — F12 bounds it and OQ records what would reopen it.
- **No standalone control-plane process of any kind.**
- **Arbitration policy between namespaces sharing a pool.** Sharing a *pool* works. Sharing one
  *reuse domain* across namespaces does not exist here: because `tenant_id` **is** the domain, two
  Bindings on one domain would share cache — possibly intended — but collide on one quota ledger,
  which never is. The second claim is refused at admission rather than adjudicated (F9).
- **No workload-declared reuse domain.** A workload may not name its own domain. Every distinct
  domain name is a new tenant with its own quota ledger, so a workload free to invent names could
  mint unlimited tenants in its namespace and draw a fresh ceiling for each, which turns
  `spec.quotaCeiling` into decoration. Domain naming stays on the object an admin controls.
- **No HA master, and no leader elected among several.** A backend's leader is one process, and the
  flags that would change that — `-enable_ha`, `-ha_backend_type`, `-ha_backend_connstring` — are
  refused at admission rather than merely left unrendered. This was true from the first commit and
  was enforced in code, but it was **never written down here**, which is how an acceptance row asking
  for a measurement on HA failover came to survive review (see ④ in *Verification*). A scope boundary
  that lives only in code will be contradicted by prose that never read the code.
- **No eviction configuration — and no eviction figure in status either.** Eviction ratios are
  process-level startup flags on the one master serving a backend, and a backend may be referenced by
  several pools, so a per-pool eviction setting is unimplementable rather than merely awkward. The
  status side falls to the same argument: the master exports no ratio at all, and the eviction counter
  it does export is master-global, so a per-pool figure would charge a co-tenant pool's evictions to
  this one. There is no `spec.eviction` and no `status.watermark` (F1). The claim is *"not observable
  per pool at the shape this ships"*, **not** *"never observable"* — F1 names the per-tenant series
  that would reopen it.
- **Workloads, engines, routers, P/D.** Nothing here creates or mutates a workload. How a workload
  selects a Binding is the consuming spec's business; this spec defines the domain it lands in.

### A trade-off, not an oversight

**One spec introducing two CRDs doubles the failure surface of `make generate`, and that command's
failure is destructive.** That is a standing lesson in this repository, and it is knowingly accepted
here.

It is accepted because `KVCachePool` and `KVCachePoolBinding` are **one concept split by scope**,
modelled on Kueue's `ClusterQueue` : `LocalQueue` — the precedent this repository already builds on.
The requested→effective quota computation, the two-level `usedBy` back-fill and the finalizer **all
span both objects**. Splitting them across two specs would cut one reconcile loop at a spec boundary
and make each half untestable without the other.

**The mitigation is T1, and it is the first task.** T1 lands the two API types and gets `make
generate` green, and does *nothing else* — no controller, no webhook, no chart. Every generated
artifact both CRDs produce is created, reviewed and re-verified (`make generate && git diff
--exit-code`) inside that one task, before any behaviour depends on it. If the generator misbehaves,
it misbehaves in a task whose entire diff is API types and generated files.

## Proposal

Map the object model onto what a Mooncake master already has, and every control-plane responsibility
becomes a CR field plus an admin-API call:

| Responsibility | Where it lands | Evidence |
|---|---|---|
| Quota | The master's **per-tenant quota**: `--enable_multi_tenants=true` + a quota connector + `/api/v1/tenant_quotas` | Measured — *Notes*, *Connectors* and *Measured admin-API response body* |
| Watermark & eviction | **Nowhere — neither object sets it and neither reports it.** The ratios are startup gflags on the master (`-eviction_high_watermark_ratio`, `-eviction_ratio`), which the backend spec does not render from a field either: reaching them is what its `extraArgs` escape hatch is for. The master exports no ratio, and the eviction counter it does export is master-global | Measured flags, and the master's own exposition |
| Reuse domain | `domain` → engine-side `tenant_id` (isolation) + `cache_salt` (prefix identity) | Measured — *Notes*, *`domain` → `tenant_id`: measured isolation* |
| **Block index** | **The master already holds the object→segment map** — its `kNumShards = 1024` is its own in-process metadata sharding | Upstream source |
| Hit-rate observability | Master metrics + engine-side metrics | Measured |

Four consequences follow, and all four are subtractions: no `spec.metadataStore`, `spec.backends`
limited to one, nothing at all on the eviction axis — neither `spec.eviction` nor `status.watermark` —
and a **thin** `KVCachePool`: quota, `usedBy` and `status.domains[]`, whose reconciler is an admin-API
translator rather than a cache manager.

### User Stories

#### Story 1
As a cluster admin, I want to grant a namespace a quota on a KV cache pool by creating one object in
that namespace, so that the grant is something I can RBAC and audit rather than a string a tenant
types into their own workload.

#### Story 2
As a namespace owner, I want one object that tells me how much I asked for, how much I was actually
granted, how much I am using, and whether I am over, so that I do not have to reason about a
cluster-wide pool to answer a namespace-wide question.

#### Story 3
As a platform engineer running two teams on one pool, I want each team's effective quota to shrink
*in proportion to what they requested* when the pool is oversubscribed, so that oversubscription
degrades predictably instead of first-come-first-served.

#### Story 4
As an operator, I want a pool with nothing mounted yet to say so in a Condition, so that "every
tenant's effective quota is zero" reads as a startup-ordering problem rather than as a silent
inability to write.

#### Story 5
As a model-platform owner, I want every deployment pointing at the same `KVCachePoolBinding` to share
one KV cache, so that a shared base model is warmed once for the whole team rather than once per
deployment — and I want a second reuse boundary to be a second Binding an admin creates, not a string
a deployment invents.

#### Story 6
As a cluster admin, I want a configuration that would crash the cache master to be rejected by
admission with a message naming the field, so that a typo is an API error rather than a
CrashLoopBackOff.

### Core Features & Acceptance Criteria

#### F1 — `KVCachePool`: cluster-scoped, thin, one backend

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool                          # cluster-scoped
metadata: { name: team-a-pool }
spec:
  backends: [mooncake-dram]                # webhook: exactly 1
  quota:
    total: 100Ti
status:
  clientEndpoint: "mooncake-dram-master.gpustack-system.svc:50051"  # the backend's Client entry
  usage: { total: 61Ti }
  domains:
  - { name: qwen-72b-v2, binding: {namespace: team-a, name: shared-kv},
      blockSize: 64, dtype: fp8, blocks: 41200000, hitRate: "0.87" }
  usedBy: [ {kind: KVCachePoolBinding, namespace: team-a, name: shared-kv} ]
  conditions: [...]
```

**Why the pool is cluster-scoped**, and not namespaced like everything else:

1. **Data-plane isolation and control-plane ownership are orthogonal axes.** Mooncake's `tenant_id`
   solves the first; object scope solves the second. Solving one with the other conflates them.
2. **`KVCacheBackend` is a privileged physical resource** — it names nodes, claims host memory and
   host paths, and needs `hostNetwork` on the RDMA path. Only a cluster admin can declare one, so the
   object that references it is cluster-scoped too.
3. **Pools must be shareable across namespaces**, and a cross-namespace reference *from* a namespaced
   object is a Kubernetes anti-pattern.
4. **Kueue's `ClusterQueue` : `LocalQueue` split is the existing, accepted answer** to exactly this
   shape, and this repository already builds on Kueue.

- `spec.quota` and its `total` are both **required**, which is why the field is a value and not a
  pointer: a pool with no declared ceiling has nothing to write into any ledger. It is the pool's
  **declared** ceiling and it is our number, not the master's: the master's real capacity is
  `mooncake_tenant_quota_allocatable_capacity_bytes`, and the two can disagree. F10 is the case where
  that disagreement is total — a declared ceiling over a backend that has mounted nothing — and it is
  a Condition rather than a silence.
- **The pool publishes exactly one address, and it is the one an engine connects to.** The backend's
  `status.endpoints[]` is a `+listType=map` keyed on `name` and enum-constrained to `Client` and
  `Admin`; the pool echoes the `Client` entry as `status.clientEndpoint` and republishes the `Admin`
  entry **nowhere**. `Admin` is one port serving the Prometheus exposition and the HTTP admin API both
  — the backend API says so where the constant is declared — so it is the write face of the quota
  ledger, while a pool is cluster-scoped and readable by anyone holding a pool RBAC rule. The operator
  dials it; nobody reads it off the pool. An absent or empty `status.endpoints[]` publishes no
  endpoint at all, sets a Condition and requeues; it never falls back to a Service DNS name derived
  from the backend's name, because a guessed address that happens to resolve is how a pool would
  silently drive the wrong master.
- **The metadata plane is not published here, because the backend does not expose it.** A client needs
  a `metadata_server` value as well as an address, and every backend this generation ships takes the
  literal `P2PHANDSHAKE` unconditionally — but that literal is rendered onto members as an environment
  variable and appears in no `status.endpoints[]` entry, so there is no backend field for a pool to
  echo. A pool publishing it anyway would be emitting its own constant under the appearance of an
  observation, which is the one thing a status field must never do.
- **Transport protocol and RDMA devices are deliberately not published here.** They are properties of
  the node a client Pod lands on, not of the pool: two clients against one pool can legitimately
  resolve differently. Each client resolves its own, and the backend reports what each of *its* members
  resolved to.
- `status.usage.total` is the sum of `used_bytes` over the tenants this pool owns.
- **Neither the spec nor the status carries anything about eviction.** The master's eviction ratios
  are process-level startup gflags and the measured HTTP surface has no eviction endpoint, so they are
  not runtime-mutable the way quota is; and one master can serve several pools, so a per-pool ratio
  could not be honoured for either of them. The status side falls to the same argument twice over. The
  master **exports no ratio at all**, so a `highRatio` field could only ever have echoed a number
  nobody here sets. And the eviction counter it does export, `master_evicted_key_count`, is
  **master-global and unlabelled**, so a pool republishing it would charge a co-tenant pool's
  evictions to itself. A field with nothing honest to put in it is not shipped, exactly as a field
  with no enforcement is not.

  What would reopen it is a **per-tenant** series rather than a pool-wide one.
  `mooncake_tenant_evict_bytes_total{tenant_id}` exists in the build this spec measured and would land
  on a **Binding**, where the tenant is already 1:1 and no attribution has to be invented. It carried
  no sample in the capability experiment because a labelled counter emits none until its first
  observation — not because it is absent. That series is bytes evicted per domain, not a watermark,
  and adding it is a later spec's.
- **A `KVCacheBackend` may be referenced by more than one `KVCachePool`.** A pool has exactly one
  backend (F4); the reverse is not exclusive. Everything that lands on the master — the tenant ledger
  and the policy file — is therefore shared, which is why F7 converges per **master** rather than per
  pool, and why domain names are unique cluster-wide rather than per pool (F9).

#### F2 — `KVCachePoolBinding`: namespaced, the provisioning point

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding                   # namespaced — the analogue of Kueue's LocalQueue
metadata: { name: shared-kv, namespace: team-a }
spec:
  poolRef: { name: team-a-pool }           # immutable
  quotaCeiling: 20Ti                       # REQUIRED; webhook: positive and <= pool quota
  domain:                                  # REQUIRED, exactly one, every field immutable
    name: qwen-72b-v2                      # webhook: claimed by no other Binding, cluster-wide
    blockSize: 64
    dtype: fp8
status:
  requestedQuota: 20Ti                     # == requested_quota_bytes for this one tenant
  effectiveQuota: 18Ti                     # what the pool actually granted after recomputation
  usage: 11.2Ti
  overQuota: false                         # true => holds MORE than it is now granted (a recut). NOT "writes refused" — see F11
  blocks: 41200000                         # observed
  hitRate: "0.87"                          # observed
  usedBy: [ {kind: ModelDeployment, name: qwen-72b} ]
  conditions: [...]
```

Four reasons it is its own object rather than a field on the pool or a name in a workload:

- **It is the provisioning point.** An admin creating a Binding in a namespace is what gives that
  namespace a quota on the pool. That has to be RBAC-able on its own.

  ⚠️ **Corrected during review, and the original wording was wrong.** This said "No Binding, no
  access", which the shipped design does not deliver: the store accepts whatever tenant id a caller
  sends, over a Service any pod can dial, and nothing derives a credential from the Binding. A
  workload that knows another namespace's domain name can use that domain today — case 44 connects
  by passing the domain string alone. What a Binding governs is who is **granted** capacity and under
  which name. Enforcement needs an authenticated proxy or network isolation, is **out of scope here**,
  and is recorded as a follow-up rather than implied by this object's name.
- **It is where quota is legible.** `requestedQuota` / `effectiveQuota` / `overQuota` are only
  meaningful here: the pool is global, a workload is singular, and *"how much can my namespace still
  write"* is a namespace-level question.
- **It registers the reuse domain, and exactly one.** `spec.domain` is where the domain is named,
  sized and typed, so no workload repeats — or mistypes — any of it. Two workloads pointing at the
  **same** Binding share KV; two pointing at **different** Bindings do not. Name matching between
  workloads disappears, and with it a whole class of typo. A namespace needing two reuse boundaries
  creates **two Bindings** against the same pool, exactly as a namespace with two scheduling
  boundaries has two Kueue `LocalQueue`s.
- **It makes referential integrity a single-scope query.** Pool `status.usedBy` → Bindings, one per
  namespace; Binding `status.usedBy` → workloads in that same namespace.

**One Binding is one tenant.** Because a Binding registers exactly one domain and a domain *is* a
`tenant_id`, every figure in `status` is read straight off a single
`mooncake_tenant_quota_*{tenant_id="<spec.domain.name>"}` series — nothing is summed — and
`spec.quotaCeiling` is written straight into that tenant's `requested_quota_bytes`. **The cardinality
is structural, not a webhook rule**: `spec.domain` is a required struct rather than a list, so a
second domain has nowhere to go and no rule for dividing one ceiling among several has to be invented
(*Alternatives*). The webhook enforces what a schema cannot — that the name is unclaimed, and that
none of the three fields changes on update.

**`spec.quotaCeiling` is REQUIRED.** The storage layer has no default policy to fall back on: a
tenant it holds no policy for is refused outright, with the *same* error a reuse domain that was
never declared gets. The artifact's own error header spells it `TENANT_NOT_REGISTERED = -1701,
///< Tenant has no quota policy.`, `default_quota` appears nowhere in its source, and a measurement
on a real master closes it — a domain declared by a Binding with no ceiling and a domain never
declared at all return the same refusal to every write, and the master does not register the tenant
just because something tried to use it. An optional ceiling would therefore describe a configuration
that passes admission, reports Ready, and refuses every byte its workloads write. A state reachable
only by misconfiguration belongs among the schema's refusals, where admission answers it, rather than
in a Condition somebody has to go and read. Required is also the direction that can be taken back:
should a default quota ever appear upstream, relaxing this to optional leaves every object already
written valid, while the reverse would invalidate every object that omitted the field.

**No migration note is owed for this, and the reason is worth stating so nobody writes one.** The
obvious fear — an object created before the field was required is now schema-invalid, so removing its
finalizer is an update the api server refuses and the object is undeletable — was tested on a real
cluster along the actual upgrade path: an old build installed, a Binding without a ceiling created,
the release upgraded in place, the new schema confirmed to carry `quotaCeiling` in `required`, and
the object then deleted cleanly. Its status kept being written, too. The likely mechanism is CRD
validation ratcheting, which does not re-validate fields an update leaves untouched and is on by
default on the versions this operator targets — that attribution is inferred rather than measured,
but the outcome is not. A warning about stranded objects would have been a plausible, checked-in
falsehood. What a tightening genuinely does put at risk is an update that *touches* the field, never
a deletion.

**Every field of `spec.domain` is immutable, webhook-enforced.** `blockSize` or `dtype` changed under
a warm cache is the most damaging silent failure in this design: the writes succeed, the reads
succeed, and the tensors are wrong. `name` changed re-points the namespace at a different ledger and
strands the old one. This is a webhook rule with a test of its own (T6), not a doc note.

#### F3 — One controller, keyed by the pool

Both kinds are reconciled by **one** reconciler in `pkg/worker/controllers/worker/kv_cache_pool.go`,
and **every reconcile key is a pool name**:

- `For(&KVCachePool{})`; `Watches(&KVCachePoolBinding{}, …)` maps each Binding event to
  `spec.poolRef.name`.
- Bindings are indexed by `spec.poolRef.name`, so one pass lists every Binding of a pool in a single
  scoped query and never walks namespaces. Pools are indexed by `spec.backends`, so the pass can also
  reach the **other pools on the same master**, which is what makes the ledger converge per master
  rather than per pool (F7).
- One pass performs **one** metrics scrape of the master and writes the pool status and the status of
  every Binding that references it. Scraping per Binding would issue N reads of the same endpoint per
  resync to produce N slices of one document, and the global gauges F10 needs are only in that
  document.
- A Binding whose pool no longer exists still maps to the (now absent) pool name; the pass finds no
  pool, lists the orphans through the index, and releases their finalizers. Nothing is stranded on a
  key nobody enqueues.

Acceptance: a Binding created, updated or deleted causes exactly one pool-keyed reconcile; a pool
resync issues **one** request to the master's metrics endpoint regardless of how many Bindings
reference it, asserted by a fake that counts calls.

#### F4 — `spec.backends` is exactly one, and the message says why

- A `KVCachePool` with zero or more than one entry in `spec.backends` is **rejected by the validating
  webhook**, on create and on update, with a message that names the reason: *quota lands on a single
  master's per-tenant ledger, and one master cannot account for bytes held in another backend.*
- `spec.backends` is immutable after creation. Moving a pool to another backend would strand every
  tenant quota on the old master's ledger with nothing left to delete them with.
- The referenced `KVCacheBackend` must exist at admission. A pool that names nothing resolvable can
  produce no endpoint, and would sit Unready forever with no field to point at.

#### F5 — Multi-tenancy is a precondition, not an option

**`--enable_multi_tenants=true` is not optional — it is the precondition for `domain` semantics.**
With it off, every request falls into the default tenant and the shard index degrades to a plain key
hash, so **different domains collide with each other's cache**: two models silently read each other's
blocks. The API itself is unavailable too — the admin API answers `-1011` / **409**
`UNAVAILABLE_IN_CURRENT_MODE` when multi-tenancy is off.

- The `KVCachePool` webhook **rejects** a pool whose referenced backend has multi-tenancy disabled.
- Because a backend can be edited after the pool is admitted, admission is not sufficient on its own:
  the reconciler treats a `409 UNAVAILABLE_IN_CURRENT_MODE` from the admin API as authoritative,
  writes no policy, and surfaces a `MultiTenancyDisabled` Condition. Level-based, not edge-triggered.
- The flag is rendered onto the master by the backend spec, which must render it
  **unconditionally** — it is a precondition of this design, not a backend option anyone toggles.
  This spec owns the refusal and the Condition when it is nevertheless off. See *Cross-spec seams*
  for the required seam and criterion 11 for the check that fails loudly instead of assuming it.

#### F6 — The quota policy file: operator-rendered, pre-validated, on a writable volume

The master's tenant quota policy file has two measured properties that together decide its whole
handling.

**⛔ A `PUT` rewrites the policy file itself.** After a create/update through the admin API, a
hand-written `quota: 1GB` comes back as:

```yaml
version: 1
tenants:
  - name: "team-a"
    quota: 1073741824
```

The master treats the connector as a **writable source of truth** — consistent with upstream's own
statement that every admin-API change is written to the connector's config source first and only then
takes effect. **Therefore the policy file must NOT be a read-only ConfigMap mount.** A read-only
mount makes the first successful quota write fail at the filesystem, and the failure surfaces as a
quota that will not apply rather than as a permissions error anyone would look for.

**This spec requires the file connector on a writable volume, and renders both halves**: the pool
reconciler renders the policy into a ConfigMap (T8), and the leader renderer copies it into an
`emptyDir` with an initContainer and points `--tenant_quota_connector_uri` at the copy (T8.5). The
second half sits in the backend's workload renderer — which this spec otherwise never touches — for
the reason *Cross-spec seams* records: without it the master does not start at all. Criterion 11
still requires the mount to fail loudly rather than be assumed, because a renderer that is right does
not make a node that is right: a `PUT` the master cannot persist because its policy source is
read-only surfaces as a `QuotaPolicyNotWritable` Condition on the pool and on every Binding, and the
pool does not report Ready. Reasons for the file connector, in order:

- `--tenant_quota_connector_type` **defaults to `file`**; the etcd connector needs a managed etcd,
  which is a new stateful dependency and a new subchart — exactly the kind of addition the object
  model's whole derivation exists to avoid.
- The operator is the source of truth regardless of the connector. It re-renders the ConfigMap and
  re-`PUT`s the ledger every reconcile, so an `emptyDir` lost with the Pod is re-seeded and
  re-converged. Durability of the file buys nothing that the reconcile loop does not already
  guarantee.
- The ConfigMap belongs to the **backend**, not to the pool, because the file belongs to the one
  master serving that backend and several pools may sit on it. Its content is the union of every such
  pool's domains (F7); rendering it per pool would make each pass erase the other pool's tenants.
- A `PUT` rewriting the copy is harmless: the copy is not the desired state, the ConfigMap is.

**⛔ A malformed policy file kills the master instead of erroring cleanly.** Omit `version:` and:

```
terminate called after throwing an instance of 'std::runtime_error'
  what():  failed to load tenant quota policy: tenant quota policy version is required
Aborted (core dumped)
```

which is CrashLoopBackOff. **Therefore the file is rendered wholly by the operator, and user-supplied
file content is never passed through.** Acceptance:

- The renderer emits `version: 1` unconditionally and emits tenants only from validated inputs.
- Every constraint is enforced **before** anything can reach the master: name non-empty, unique,
  **must not start with `_`**, no NUL and no control characters; quota a **positive** integer number
  of bytes.
- A validating webhook runs the same renderer + validator over what the object *would* render and
  refuses the object with a typed field error. The renderer never emits a partial file: a refused
  input yields an error and no output at all.
- The renderer and the validator are the **same code path** in `pkg/worker/kvcache/mooncake`, called
  from both the webhook and the reconciler. Two implementations of "is this file safe" would agree
  today and diverge on the case neither exercises.
- **Every write of this document is whole and master-wide, so every writer owes it the same set.**
  The snapshot the reconciler builds deliberately omits two kinds of tenant — a *contested* domain,
  and one whose Binding is *releasing* but whose entry the master refused to drop — because neither
  is something to converge. Both are nonetheless **live on the master**, and the seed container copies
  this document over the master's own policy at every leader container start, so a document written
  without them withdraws their quota at the next restart. Both the serving pass and a pool's
  **teardown** put them back before writing.

  ⚠️ **Corrected during review; only the serving path did this.** A pool's teardown re-rendered from
  the snapshot alone. The entries it dropped belong to **sibling pools on the same backend**, whose
  reconcile is not running during that teardown, so deleting any pool on a shared backend published a
  document missing a live sibling's tenant with nothing to notice. The teardown now reads the ledger
  and keeps every entry carrying an explicit policy that this pool did not register; a listing it
  cannot read **holds** the teardown rather than writing a document built from half the state.

#### F7 — The tenant ledger: quota reconciliation through the admin API

The admin API is served on `--metrics_port` (default 9003):

| Operation | Method | Path | Body |
|---|---|---|---|
| List | GET | `/api/v1/tenant_quotas` | — |
| Query one | GET | `/api/v1/tenant_quotas?tenant_id=X` | — |
| Create/update | PUT | `/api/v1/tenant_quotas?tenant_id=X` | `{"requested_quota_bytes": N}` |
| Delete | DELETE | `/api/v1/tenant_quotas?tenant_id=X` | — |

- **The unit of convergence is the master, not the pool.** A backend may be referenced by several
  pools (F1), and the ledger and the policy file belong to the one master serving it, so the desired
  ledger is one entry per domain across **every pool bound to that backend**, resolved through an
  index on `spec.backends`. Converging per pool would make each pass delete the other pool's tenants.
- The reconciler converges that set: `PUT` where the observed `requested_quota_bytes` differs,
  `DELETE` for an entry **no Binding of any pool on that master claims any more**, nothing at all
  where they agree. Level-based and idempotent; a steady state issues no writes. Domain names are
  unique cluster-wide (F9), so the desired set never has to merge two claims on one entry.
- Every rejection path is **pre-validated** rather than discovered (the table is in *Notes*), so a
  refusal from the master means a bug or a race, not a user input. A refusal is logged with the
  master's own message and surfaced on the owning Binding's Condition; it never silently drops.
- The client maps the master's error codes to typed errors, and it keys on the **code in the body**
  rather than on the HTTP status, because two statuses are overloaded (*Notes*): 400 is invalid
  input, separated into a bad tenant name and a non-positive quota by message because the code cannot
  tell them apart; 404 is unknown tenant, modelled as **absent** rather than as failure because it is
  expected during convergence; 409 is F5's precondition failure **only when the body says
  `UNAVAILABLE_IN_CURRENT_MODE`**, since `TENANT_NOT_EMPTY` and `UNAVAILABLE_IN_CURRENT_STATUS` share
  that status and the first of them is answered on the finalizer's own DELETE.

#### F8 — The Binding's status is one scrape and one write

The per-tenant Prometheus surface **is** the Binding's status:

```
mooncake_tenant_quota_requested_bytes{tenant_id="team-a"} 1073741824   -> status.requestedQuota
mooncake_tenant_quota_effective_bytes{tenant_id="team-a"} 0            -> status.effectiveQuota
mooncake_tenant_quota_used_bytes{tenant_id="team-a"} 0                 -> status.usage
mooncake_tenant_quota_reserved_bytes{tenant_id="team-a"} 0             -> in-flight PutStart reservation
mooncake_tenant_quota_committed_count{tenant_id="team-a"} 0
mooncake_tenant_quota_metadata_object_count{tenant_id="team-a"} 0
mooncake_tenant_quota_over_quota{tenant_id="team-a"} 0                 -> status.overQuota
mooncake_tenant_quota_explicit_policy{tenant_id="team-a"} 1
mooncake_tenant_quota_allocatable_capacity_bytes 0                     (global)
mooncake_tenant_quota_requested_bytes_sum 5368709120                   (global)
mooncake_tenant_quota_effective_bytes_sum 0                            (global)
```

**Exposing both requested and effective is not our invention.** Mooncake already models it exactly
that way, because when the sum of requested quotas exceeds cluster capacity it recomputes each
tenant's effective quota **in proportion to what they requested**. Story 3's guarantee is the storage
layer's, reported rather than implemented.

- A tenant **is** a domain, and a Binding registers exactly one (F2), so the mapping is 1:1 and
  nothing is summed: `requestedQuota`, `effectiveQuota`, `usage` and `overQuota` are each read from
  the single series for `tenant_id="<spec.domain.name>"`. An aggregate that could hide which domain
  is the full one cannot exist, because there is only one.
- **The Binding carries no `status.domains[]`.** With one domain per Binding, a length-1 list would
  restate the four figures above in a second place that can disagree with the first — and it would
  not buy an API-compatible path to multi-domain either, since multi-domain turns those four scalars
  into sums whether or not a list sits beside them. The two genuinely per-domain observations,
  `blocks` and `hitRate`, are scalars on the Binding's status. The **pool's** `status.domains[]` is a
  genuine list: one entry per Binding of that pool.
- `status.usage` is `used_bytes`, not `used_bytes + reserved_bytes`. Reserved is an in-flight
  `PutStart` reservation and folding it in would make a burst of concurrent writes read as
  consumption that never happened.
- Fields the status **does not** carry: the upstream documentation also lists `charged_bytes` and
  `admission_closed`, and the measured build returns neither. Status is mapped from the fields the
  master actually returns — `used_bytes` and `reserved_bytes` are the equivalents.
- **The eleven series above are what the measured document contained, which is not the same as every
  series the build defines.** The master also defines two labelled per-tenant counters —
  `mooncake_tenant_quota_reject_total{tenant_id,reason}` and
  `mooncake_tenant_evict_bytes_total{tenant_id}` — that emit **no line at all** until their first
  observation, and nothing had rejected or evicted during the experiment. Neither is read here, and
  the distinction matters in one direction only: a series missing from a scrape may be a series that
  has never fired, so a reader is absent rather than zero (F9) whether the tenant is unknown or merely
  quiet.

Acceptance: given a recorded metrics document, each Binding's four figures equal the series for its
own `tenant_id` and nothing else, with no summation on any path; a Binding whose tenant is absent
from the document reports no figures and carries a Condition, rather than reporting zero.

#### F9 — Domains: the registry, and the refusal to share one

A **domain** is the reuse identity: it maps to the engine-side `tenant_id` (isolation) and
`cache_salt` (prefix identity). It is declared in exactly one place — `spec.domain` on a
`KVCachePoolBinding` — by whoever can create Bindings. `status.domains[]` on the pool is the registry
— name, owning Binding, blockSize, dtype, blocks, hitRate — assembled from that pool's Binding index.

- **The operator writes one requested quota per Binding.** `spec.quotaCeiling` lands verbatim on that
  Binding's one tenant as `requested_quota_bytes`. There is no division rule, because there is
  nothing to divide; an even split across several domains is recorded as rejected in *Alternatives*.
- **A domain name another Binding already claims is refused at admission.** The webhook rejects the
  create, with a message stating that cross-namespace sharing of one reuse domain is not supported in
  this spec and naming the Binding that holds it. The reason is that `tenant_id` **is** the domain:
  two Bindings on one domain would share cache — possibly intended — but collide on **one** quota
  ledger, which never is. Refusing at admission means the user learns at `kubectl apply` instead of
  discovering later that a Binding silently declines to manage itself.
- Uniqueness is **cluster-wide**, not per pool, because one master can serve several pools (F1) and
  `tenant_id` is master-global.
- Admission is the gate; the reconciler keeps a **race backstop only**. Two creates racing the same
  cache can both pass the webhook, so a domain the reconciler nevertheless finds claimed twice is
  managed for neither Binding, excluded from both, with a `DomainClaimedByMultipleBindings` Condition
  naming the other claimant on both. That path exists for the race, not as the enforcement point.
- A domain name violating the master's name constraints is refused by the **webhook**, running the
  renderer's own validator (F6), so it can never reach the file — a name the master rejects at load
  time is a crash rather than an error. The constraint is the master's own, measured:

  | input | master's response |
  |---|---|
  | `tenant_id=` (empty) | `-600` / 400 `Missing or invalid tenant_id` |
  | `tenant_id=_bad` (leading underscore) | `-600` / 400 `Missing or invalid tenant_id` |
  | unknown tenant on GET | `-704` / 404 `OBJECT_NOT_FOUND` |

  The accepted shape is a **DNS-1123 label** — non-empty, lowercase alphanumerics and `-`, not starting
  or ending with `-`. That is strictly inside what the master accepts and is what a Kubernetes object
  name already looks like, so nobody learns a second naming rule. This is the **only** place the shape
  is checked: the name is an admin's input here, it is immutable once admitted, and every consumer
  downstream copies it rather than re-judging it.
- `blocks` and `hitRate` are **observed, not declared**: hit rate comes from the master's own metrics
  plus the engine-side metrics, which is where the object model's derivation put hit-rate
  observability. A domain whose figures are not in the scrape reports them absent, never zero — a
  fabricated `hitRate: "0"` on a warm cache is worse than no number.
- **The registry is authoritative, not advisory.** A domain appears in `status.domains[]` because a
  Binding declares it, not because a workload announced it, so building the list is one pass over the
  Binding index and needs no watch on any workload kind.

#### F10 — Zero mounted members is a Condition, not a silence

`mooncake_tenant_quota_allocatable_capacity_bytes` is `0` for a master with no mounted segments, and
**a pool with zero mounted members gives every tenant an effective quota of 0**. That is a
startup-ordering trap: the objects all look configured, and writes fail for a reason nothing states.

Acceptance: when the master reports `allocatable_capacity_bytes = 0`, the pool carries a Condition
that names it — *the backend has no mounted members, so every effective quota is 0* — and every
Binding of that pool carries the same reason. The pool does not report Ready, and no Binding reports
a quota it cannot honour.

#### F11 — What an effective quota enforces, measured rather than assumed

**⛔ A quota is not an admission barrier. It is the line at which the master begins evicting the
domain's OWN objects to make room for the next write.** Measured on a real master and confirmed
against its source (Mooncake v0.3.13):

- `PutStart` does not return `TENANT_QUOTA_EXCEEDED` on the first refused charge. It calls
  `EvictTenantMemoryForQuota(tenant, deficit)` and retries, up to `kMaxTenantQuotaEvictionRetries = 2`
  — three attempts in all. Writing eight 4 MiB objects into a 16 MiB grant produces eight *successful*
  writes, four surviving objects, and a charge of exactly 16 MiB.
- That eviction is invisible to the store's general eviction counters: `master_evicted_key_count` and
  its three siblings all stay at `0` throughout, because it is not on their path. The counter that
  does move is `mooncake_tenant_quota_reject_total{tenant_id,reason="quota_exceeded"}`, and it counts
  only the refusals that survived all three attempts — it is a record of real refusals, not of
  overshoots.
- **A write IS refused, with `TENANT_QUOTA_EXCEEDED` (-1700), when nothing can be evicted.** The
  eviction skips any object whose lease is still live, and a read grants a lease of
  `default_kv_lease_ttl`, 10 s by default. Reading back every object in a filled grant and then
  writing once more within that window produces the refusal — measured at 0.0 s after the reads —
  and every object that caused it is still readable afterwards.

**Therefore the claim under test is precise, and every half of it is asserted (T12, CASE 44):**

- with every object in the grant under lease, **a new write is refused**, and the refusal is a quota
  verdict rather than an unregistered tenant;
- the objects that caused the refusal remain **readable, deletable and reclaimable**;
- with the leases lapsed, **the same write is admitted and older objects of the same domain are gone**
  — recorded as the behaviour it is, so that a master which stops doing it fails the case instead of
  quietly passing it.

That first pair is a causal chain rather than two hopes side by side: the objects being intact is
*why* the write is refused, because their leases are what leave the master nothing it is allowed to
evict.

**`status.overQuota` is not that signal, and never becomes true on this path.** The master computes it
as `charged_bytes > effective_quota_bytes` while refusing any charge that would overshoot, so a domain
writing past its grant leaves the flag false — waiting on it as the sign that writes are being refused
is waiting for something that never arrives. It reports one situation: the grant was **recut below
what the domain already holds**, which is what a proportional cut does when a pool's members shrink or
another Binding joins. The field ships, because that situation is this spec's own feature, but nothing
in this repository may describe it as a write barrier.

#### F12 — Finalizers, `usedBy`, and immutability

- **Deleting a `KVCachePoolBinding` with a non-empty `status.usedBy` is refused by the finalizer.**
  The Binding is the grant; releasing it while a workload still holds the pool through it
  would leave a workload writing into a pool nothing records it as using.
- The Binding's finalizer, once `usedBy` is empty, `DELETE`s the one tenant quota entry that Binding
  owned and drops it from the rendered policy, then releases. A ledger entry outliving its Binding is
  capacity nobody can reclaim and nobody can see.
- **The finalizer is taken before the pass writes anything outside the cluster, and on every live
  Binding on the master — not just the reconciled pool's.** The entry a Binding owns can only be
  deleted by name, and the name survives only while some object still carries it, so a Binding that
  became deletable while its entry was already written would strand that entry past every later pass.
  The master-wide reach follows from the writes themselves: converging one pool converges the whole
  master's ledger (F7), so a pass writes entries for Bindings it does not own and owes each of them a
  finalizer first.
- A `KVCachePool` finalizer refuses deletion while `status.usedBy` names a Binding, and on release
  deletes every tenant entry its own Bindings created — **and no other**, because a master shared
  with a second pool carries that pool's entries in the same ledger (F7).
- **`spec.poolRef` is rejected on update, and so is every field of `spec.domain` (immutable).**
  Re-pointing a Binding would move a namespace's grant silently and leave its bytes on the
  old master; changing `blockSize` or `dtype` under a warm cache is silent tensor corruption.
- `status.usedBy` is filled at two levels and each is a single-scope query: the pool's from the
  Binding index; the Binding's from workloads in its own namespace.
- **The two levels have different WRITERS, and the Binding's is a spec that has not landed yet.** The
  pool's list is written by the pool reconciler, which already lists its Bindings through the index.
  The Binding's is written by whoever consumes the pool through it — and the one kind that will,
  `ModelDeployment`, is defined by the **model-deployment spec in THIS repository**, whose
  `spec.kvCache.poolRef` names a `KVCachePoolBinding` in its own namespace. That spec has not been
  built, so the type does not exist yet and nothing here could enumerate it. This spec's reconciler
  therefore READS the list and refuses the release on it, exactly as
  `KVCacheBackend.status.usedBy` is read by the backend's own teardown and written by its consumers.
  The single-scope rule still holds and is now structural: the writer is already in the namespace, so
  there is no cross-namespace query to avoid.
- **Until that spec lands, the Binding's finalizer is an empty shell, and this has to be said out
  loud.** No controller writes `KVCachePoolBinding.status.usedBy`, so the finalizer sees an empty
  list on every pass and always releases. *"Deleting a Binding is refused while a workload holds
  it"* — criterion 5 — therefore describes a protection that **does not yet exist** when this spec
  ships on its own. It is a sequencing limit rather than a defect: the mechanism is complete and
  tested against a list a test writes, and the day a consumer writes one it engages with no change
  here. What must not happen is a reader taking the criterion as protection available on day one.
- **A pool CLAIMS its backend, in the same list shape.** The pool reconciler writes
  `{kind: KVCachePool, namespace: "", name: <pool>}` into `KVCacheBackend.status.usedBy` when it
  resolves the backend, and removes it on release — AFTER the ledger entries and BEFORE its own
  lock. After, because a backend released while an entry of this pool's is still on its master could
  be deleted with that entry on it; before, because a pool that dropped its own lock first would
  leave the claim behind with nothing in the cluster left to remove it. This is the write criterion
  13 reads back, and the only one in the family that puts an empty string into a list map key.
- **The backend reads that claim as a claim only while the claimant exists.** `usedBy` is written by
  the consumer and cleared by the consumer, so nothing else can clear an entry a consumer left
  behind — and an operator forcing a wedged pool's finalizer off leaves exactly that: a claim naming
  an object that is gone, holding the backend's teardown with no event able to end it. The backend's
  reconciler therefore resolves each `KVCachePool` entry before refusing on it, ignores the ones that
  are definitely gone, and **says so in the Condition** instead of reporting a bare *"no object claims
  this backend"* that contradicts the list beside it. It watches `KVCachePool` for the same reason:
  the pool disappearing is the event that has to re-examine the claim, and it is the one event the
  ordinary wake path — the consumer's own write onto this status — cannot deliver. An entry is ignored
  **only on a definite NOT FOUND**; a kind the reconciler cannot resolve, or a read that failed, is
  kept, because turning *"cannot verify"* into *"does not exist"* is the wrong direction for a claim
  whose whole purpose is to hold a deletion.
  ⚠️ **The write is unchanged** — consumers still write and clear their own entries. What changed is
  how the entry is READ, and the reason generalizes past this field: *a field written by A and read by
  B must leave B correct when A never cleans up*, because A going missing is precisely the case that
  needs it.
- **Every `usedBy` in this family carries ONE shape — `{kind, namespace, name}` — and names no API
  group.** The missing group is a constraint, not an omission: a `usedBy` entry may only name a kind
  in this API group, and within one group kind and name identify an object. Stating the constraint is
  what makes the group safe to leave out, and leaving it out is what makes the list keyable at all —
  `core.TypedLocalObjectReference`'s `apiGroup` is optional with no default, which a structural
  schema refuses as a list map key, so a list keyed on `kind` and `name` beside it would silently
  merge two objects that differ only by group. All three fields are required and all three are keys;
  `namespace` carries the **empty string** — a value, not an absence — when the referent is in the
  holder's own scope, and a real namespace only where a cluster-scoped object names a namespaced one.
  **`KVCacheBackend.status.usedBy` moves to the same shape (T2)** — it is unreleased, and one shape
  read once beats two that differ by accident.
- **`Namespace` carries no `omitempty`, and that is load-bearing rather than an oversight.** With it,
  an empty namespace would be omitted from the serialized entry, the field's own `required` rule
  would then refuse the write, and the backend's `usedBy` — the one list that always writes an empty
  namespace — could never be written at all. The empty string has to reach the wire for the key to
  exist. **Criterion 13 is what proves it**, because nothing else does: criterion 1 exercises the
  pool's `usedBy`, whose namespaces are real. An empty-string list map key should be legal — a
  structural schema constrains a key field to being required-or-defaulted and non-nullable, not to
  any value domain, and `""` is a string rather than a null — but "should be legal" is exactly the
  distance this repository has been caught by before, so it is measured rather than reasoned.

#### F13 — The scale envelope this rests on

"No separate metadata service is needed" holds **at the scale this ships for**, and it is not a
general claim, because the Mooncake master is a **single process** (the evidence is in *Notes*).
**Four things must be measured before the control plane is allowed to rest on a single master, and
they are acceptance here rather than runtime discovery:**

1. Resident memory per object — RSS against object count.
2. Lookup throughput and p99 latency.
3. How eviction-scan and failure-recovery latency curve with object count.
4. How long the tenant ledger takes to answer again after the one master restarts.

The numbers land in *Verification*, filled by T13. A missing number is not a passing measurement.

### Verification

**Hardware: a local Kubernetes cluster. No GPU, no RDMA, no cloud.** Every acceptance criterion below
is reachable there; the quota ledger, the admin API, the policy file and the metrics surface need no
accelerator.

| # | Criterion | Where it is checked |
|---|---|---|
| 1 | Two `KVCachePoolBinding`s in two different namespaces referencing the **same** `KVCachePool` both reach Ready, and the pool's `status.usedBy` lists both | e2e — T11 |
| 2 | Both `requestedQuota` and `effectiveQuota` are visible on each Binding, and when the sum of requested quotas exceeds cluster capacity, `effectiveQuota` is reduced **in proportion to what was requested** | e2e — T11 |
| 3 | With every object in a filled grant held under lease, **a new write is refused while those objects remain readable, deletable and reclaimable** — and once the leases lapse, the same write is admitted by evicting the domain's own older objects (F11) | e2e — T12, its own case |
| 4 | A pool with zero mounted members surfaces a Condition explaining that `effectiveQuota` is 0 — it does not silently accept writes that will fail | e2e — T11; unit — T8 |
| 5 | Deleting a Binding with a non-empty `status.usedBy` is refused by the finalizer — the mechanism, on a list a test writes; **the protection itself waits on the model-deployment spec to supply a writer** (F12) | unit + e2e — T10, T11 |
| 6 | `spec.poolRef` is rejected on update, and so is every field of `spec.domain` — `name`, `blockSize`, `dtype` (immutable) | unit — T6 |
| 7 | `spec.backends` with more than one entry is rejected with a message naming the reason | unit — T5 |
| 8 | A quota policy file the operator would render is validated by the webhook **before** it can reach the master — the `version` field, positive quotas, and the name constraints | unit — T3, T5, T6 |
| 9 | The four scale measurements are recorded, with numbers, here | T13 |
| 10 | A second `KVCachePoolBinding` claiming a `spec.domain.name` another Binding already holds is **rejected at admission**, cluster-wide, with a message naming the holder | unit — T6; e2e — T11 |
| 11 | Both backend preconditions **fail loudly** rather than being assumed: a master without `--enable_multi_tenants=true` yields `MultiTenancyDisabled`, a policy source the master cannot rewrite yields `QuotaPolicyNotWritable`, and neither pool reports Ready | unit — T8; e2e — T11 |
| 12 | `spec.domain.name` is rejected at admission for every shape the master rejects as a `tenant_id`, so a bad name never reaches an engine | unit — T6 |
| 13 | A `usedBy` entry whose `namespace` is the **empty string** round-trips: it is written, it reads back, and writing it again produces one entry rather than two | e2e — T11 |

**One piece of evidence here is harder than the tests written to produce it, and the difference is
worth stating.** The undrained-domain hold — `pool_held_by_undrained_domain` in the unit tests, and
the Binding's own `Releasable=False (LedgerNotReleased)` — is covered by a fixture that makes the
master answer 409 `TENANT_NOT_EMPTY`, and by an e2e assertion over a `status.usedBy` that a test
wrote. Both establish the same thing: *given that state, the code takes the right branch.* Neither
establishes that the state occurs.

A CASE 44 run that failed to remove its objects produced it for real: the domain still held four
objects, the master refused to drop its quota, and the operator held the finalizer and published the
Condition naming the domain and the reason. That run was a broken test — it left a namespace
Terminating, and the case was fixed so it stops happening — but it is the only evidence in this spec
that the held state is **reachable through the mechanism rather than only through a fixture**. A
fixture proves the branch; only this proves the branch is ever taken.

**The four scale measurements (criterion 9).** Recorded on the same local cluster, against one master
with a synthetic object population. Each row is filled by T13 with the number and the population it
was measured at; an empty cell is an unmet acceptance, not an omission.

| # | Measurement | Method | Result |
|---|---|---|---|
| ① | Resident memory per object | RSS at 10^4 / 10^5 / 10^6 objects, differenced | **~697–715 B/object** marginal across two backend shapes, plus 0.8–1.0 MB fixed |
| ② | Lookup throughput and p99 | A fixed-rate get loop at each population; highest reading driven from three client nodes | **p50 1.6–1.7 ms, p99 2.8–3.1 ms**, flat from 10^4 to 10^5; **≥ 11005.8 qps** (24 concurrency over three nodes, two 30 s runs 0.5% apart) — ⛔ a floor for **one store handle shared by N threads**, NOT a system envelope: the leader drew **2.09 of 14 cores** at that reading, and a process-per-handle client read 2.7× higher at equal concurrency (see below) |
| ③a | Tenant-eviction cost vs object count | Drive one domain past a ceiling scaled to its population, timing the write that trips it | **1.288 µs per object discarded**, linear from 4×10^4 to 4×10^5 discarded |
| ③b | Global eviction-scan cost vs object count | A full metadata traversal at the store's high-water mark | *out of reach here — see below* |
| ③c | Snapshot cost, and what snapshotting on does to a restart — both ③c's own recovery and ④'s figure | `master_snapshot_duration_ms` for the snapshot; external timing for the restart; VmRSS at 200 ms through the fork | *a **product gap**, not a measurement that failed: the rendered surface does not offer snapshotting, and whether to offer it has not been decided — see below* |
| ④ | Ledger recovery time after the one master restarts | Seven leader restarts, over restart method, pre-restart object count and **where the replacement Pod lands** | ⛔ **two branches, not one number**: **2.8–4.4 s** when the leader keeps its address or returns to the member's node, and **32.0–32.1 s** when it changes address *and* lands elsewhere. **The second is the production-typical layout.** In every run the master answers `effective_quota_bytes: 0` before it answers correctly. Excludes what a snapshot fork would add (③c) |

**③c and ④ were originally one requirement and are now two rows.** The coupling — both measured with
snapshotting on, both taken from the same restart — was written because snapshots are taken by
**forking a child process** (`-snapshot_child_timeout_seconds`), which risks doubling resident memory
at high object counts, the failure ① would otherwise miss; and because one restart answers both halves
(③c times what the store gets back, ④ times what the ledger gets back).

⛔ **It was dropped as a requirement because it transmitted ③c's unreachability to ④, which does not
share it.** A single master's ledger is durable through the policy file, so ④ is measurable on an
ordinary restart and can be delivered; bound to ③c it could only be reported as unmet. Neither half
was relaxed: ④ still owes a number, and the snapshot-on case still owes an account of why it cannot be
reached. What was removed is only the demand that one restart produce both. ⚠️ The cost of removing it
is real and stated rather than absorbed: **④'s figure therefore excludes whatever a snapshot fork adds
to a restart**, and nothing here bounds that.

**How ① and ③a were measured, and what each number does not say.** Both were taken on one master
with `multiTenancy` on and a ceiling far above the population, so nothing was ever discarded to make
room while ① was being taken.

- **① is a marginal figure**, differenced between populations rather than against an empty process:
  the from-empty reading at 10^4 is 795.4 B/object and at 10^6 is 712.7, and the gap between them is
  a fixed cost being amortized, not a per-object cost that shrinks.
- **The linear reading is not a two-point fit agreeing with itself.** Two points cannot disagree with
  the line through them, so the split they imply — a fixed cost plus a marginal one — was used to
  **predict** a reading nobody had taken yet, and the prediction was written down before the run. On a
  different backend, different member size and different node, the two predicted figures came back
  within **0.3% and 1.8%**. A model that survives a stated prediction on hardware it was not fitted to
  is doing more than describing its own inputs.
- **The range is the answer, not an average of it.** Solved on its own points, the second backend gives
  696.8 B/object against the first's 711.8 (re-confirmed at 715.3) — a **2.6% spread across backend
  shapes**, and the reason the predictions came in low in a consistent direction. Reporting 711.8 would
  hide a real cross-configuration difference behind a decimal place. What caused the spread is not
  established; the two differ in member capacity, which is a guess and nothing more.
- ⚠️ **RSS never comes back down.** Discarding 448,000 objects moved resident memory from 832392 kB to
  832536 kB: the allocator keeps what it took. So ① bounds what a population **has cost**, and cannot
  be read backwards to infer how many objects are held now.
- ⛔ **② is a floor, and what it is a floor on is a client shape rather than the store.** The highest
  reading is **11056.7 and 11005.8 qps** — 24 concurrency over three client nodes, two 30 s runs
  **0.5%** apart, `bad=0`, 64 B values over a 10^5 key space in one reuse domain. At that reading the
  **leader process drew 2.09 of its node's 14 cores** and the member 0.96, so the store was nowhere
  near saturation and this number **is not a system envelope**. It was taken with one store instance
  per pod shared by N **threads**, and that shape is what ran out — not the store, and not the
  hardware. ⚠️ Reading it as the system's peak understates the system by an amount **this spec does
  not bound**.
- ⛔ **An earlier reading of the concurrency curve was withdrawn, and only its shape came back.** A
  first pass at 1, 2, 4, 8 and 16 concurrency gave 1123, 2756, 5236, 9152 and 7863 qps in a **10.3 s**
  window, and this row called the drop at 16 "a fall, not a plateau" because 16 probes outnumbered a
  14-core node. The noise floor at that window had never been characterised — measured afterwards on
  one pod, one key space, one store instance, a **3 s** window spreads **51%** and a **30 s** window
  **5.2%** — so the steps *between* those points could not be told from noise, and the mechanism was
  an attribution over a difference that might not exist. **The fall itself is now confirmed and is far
  steeper than that pass showed**: at 30 s, one pod at 8 threads gives 8451.9 while the same pod at 16
  gives **2880.3 / 2540.0** and at 24 gives 2062.6 / 2095.2.
- ⭐ **The 2.7× gap between the two passes is explained, and the explanation is that they measured
  different clients.** The old pass used `multiprocessing`: 16 **processes**, each constructing its
  own store handle. The new one uses `threading`: 16 **threads** sharing one. Recovered from the old
  script's own text, not inferred. So the readings were never comparable, and the difference is the
  client's concurrency model — no GIL and no shared handle on one side, both on the other.
  ⚠️ **Two candidate causes were not separated**: the interpreter lock and contention inside the
  store handle both differ between the two shapes, and nothing here tells them apart.
- ⛔ **A "progressive collapse" reading of this was proposed here and is withdrawn as refuted.** One
  30 s run split into six 5 s segments with no reconnection between them: the collapsed shape gives
  2576, 2653, 2603, 2603, 2588, 2680 qps — a **4.0%** spread, with client cores pinned at 13.68 from
  the first segment. The healthy control gives 9396…9368, spread 5.5%, so the segmenting method is
  sound. **The collapse is instantaneous steady state, not degradation within the window**, and a
  10.3 s window could never have read 7863 from this shape. The run-to-run 12% and 14% seen earlier
  are variance *between* runs and were wrongly offered as evidence of decay *within* one.
- ⭐ **The collapse is located, and it is not the store.** Client CPU was differenced against a
  same-method idle baseline (`/proc/stat` over 30 s: 0.15, 0.84 and 0.09 cores on the three nodes).
  Net client cost against delivered work: **4.97 cores → 8452 qps (1701 qps/core)** at 8 threads,
  **12.96 → 2880 (222)** at 16, **13.35 → 2063 (155)** at 24. Per-core productivity falls **7.6×**
  while the master's own CPU *falls too* (leader 0.91–0.94 → 0.34–0.36 cores). Two quantities moving
  in opposite directions puts the limit on the client and rules out the store, the network and RPC
  serialisation. ⚠️ Core exhaustion alone does not explain it — 12.96 of 14 cores is near-saturated,
  but throughput fell 3× while CPU rose 2.6×, so 13 of those cores were buying nothing.
- ⛔ **What this row therefore does not license.** It does not bound the store's throughput, because
  the store was at 15% of one node's CPU when the client gave out. It does not say a real engine would
  see this ceiling — an engine sets up its own store handle per process, which is the shape that read
  **2.7× higher** at equal concurrency. And the earlier claim here that pushing further "needs more
  nodes or a non-Python client" was **wrong**: the faster shape was also Python. What it needs is
  processes rather than threads.
- ⚠️ **`loadavg` nearly manufactured a background load that was not there.** One node still read
  4.73 (1 min) after a run; the same-method measurement 90 s later read **0.15 cores**. Load average
  is residual heat, and reading it would have charged this measurement's own consumption to someone
  else.

- ⭐ **④ has two branches and the discriminator is where the replacement Pod lands.** Timed from the
  restart command returning, with one continuous polling process at 50 ms so both ends sit on one
  clock (cross-machine offset measured separately at 0.161–0.313 s, inside the reported precision).
  ⭐ **The slow branch needs two conditions at once — it is an interaction, not a dimension.** With
  the member fixed on one node, and no exceptions:

  | New leader's placement | Objects held before the restart | Recovery |
  |---|---|---|
  | **off the member's node** | **10^5** | **32.00 / 32.09 / 32.32 s** |
  | off the member's node | none | 4.05 s |
  | back on the member's node | 10^5 | 4.20 / 4.27 / 4.35 s |
  | back on the member's node | none | 3.73 / 4.38 s |

  The three slow readings span **0.32 s**, which is what makes the slow case a branch rather than an
  outlier. A `kill 1` restart is fast in every combination, because the Pod address does not change.
- ⛔ **Two of this spec's own readings of ④ are withdrawn, and the second is the more instructive.**
  It first called the 32 s figure an *isolated anomaly*. It then called the discriminator *where the
  replacement Pod lands* — measured on a 2×2 over restart method and object count, which concluded
  object count had no effect. ⚠️ **All four of those cells had the Pod land back on the member's
  node.** Inside that slice the conclusion was true; taken as general it was wrong.
  ⭐ **Both marginal effects can be zero while the interaction term is large**, and a matrix that
  covers its own axes says nothing about a term it did not model.
- ⭐ **The fast branch is attributed to three named constants; the slow one is not attributed at
  all.** A member reaches the leader by a **Service DNS name**, and on the non-HA path its ping
  thread carries `max_ping_fail_count = 3`, `success_ping_interval_ms = 1000` and
  `fail_ping_interval_ms = 1000` — three `const` locals in `client_service.cpp`, with **no flag,
  environment variable or argument** that reaches any of them. Three failed pings plus one reconnect
  is 3–4 s, which is every fast reading above; the figures were measured before the constants were
  read.
- ⛔ **The 30 s is spent inside one blocked call, not across retries — and this spec said the
  opposite.** It argued the slow branch could not be that loop *because* the loop retries every
  second and never gives up. The member's own log refutes the premise: across the whole 32 s there
  are **three lines, all within 1.24 s of the restart** — `RPC call failed: End of file`,
  `Failed to ping master`, `RPC call failed: End of file` — and then **30.4 s of silence**, with no
  `Reconnect failed` and no `Reconnected to master`. Every path through that loop either logs or
  sleeps and returns to log, so silence means it **never iterated**. The leader agrees: `Clients: 0`
  until the transition, then `Clients: 1` with the segment back. ⚠️ The blocking point itself was
  **not** observed — no thread stack was taken — so "blocked in the call" is the only reading
  consistent with both logs, not a direct measurement.
- ⚠️ **The fast branch logs the same two lines in the same place**, so the two are indistinguishable
  in the member's log except by duration.
- ⛔ **Three candidates were proposed and all three are refuted.** `kViewChangeTimeout` is on the HA
  leader-monitor thread, which **this scope never starts** — right magnitude, right side, right name,
  ruled out only by checking whether the code holding it runs at all. A DNS cache TTL and a stale UDP
  conntrack entry are both refuted by the interaction: **neither mechanism can depend on how many
  objects the member held**, and that is half the condition. ⭐ That is a stronger exclusion than a
  wrong magnitude — the *independent variable* is wrong.
- ⚠️ **No knob was found either way.** `rpc_conn_timeout_seconds` exists but is a master-side flag
  defaulting to *no timeout*; the ping constants are `const` locals; the transfer engine's RPC
  communicator carries a `timeout_seconds = 30` struct default that nothing here traces to this call.
  **What the 30 s is remains unestablished**, and finding out needs a thread stack from a member
  inside the window — a new experiment this spec does not owe.
- ⛔⛔ **The slow branch is the production-typical layout.** A leader and a member on separate nodes is
  the ordinary deployment, and a deleted Pod does not come back with its old IP. So **~32 s is the
  figure that belongs in a capacity envelope**, not the 3–4 s one.
- ⛔ **The window where the master answers `success: true` with `effective_quota_bytes: 0` is present
  in every run — 7 of 7 — alongside a correct `requested_quota_bytes`.** Its readiness probe reads
  `/get_all_segments` rather than the ledger, so **the Pod goes Ready at the same instant it starts
  serving zeros**, and 29.6–29.8 s of the slow branch is spent that way. That is why the two
  conditions reporting it are not optional, and why neither names a cause: nothing in a single scrape
  distinguishes a 0.06 s window from a half-minute one.
- ⭐ **The capacity gauge and the tenant's effective quota flip together.** Sampled at 65 ms, one
  reading has `cap=0 seg=0 eff=0 req=536870912` and the next has `cap=2147483648 seg=1
  eff=536870912`. So segments genuinely had not remounted, rather than ledger convergence and segment
  mounting being independent timelines — the prediction that made this discriminating was written
  before the restart, and `master_service.cpp:1465` sums mounted segments' sizes, which agrees.
- ⭐ **`QuotaGranted` was then checked against the input the reconciler actually reads, which is not
  the one this window was first observed on.** The conditions read the per-tenant series in
  `/metrics`; the window was first seen on `/api/v1/tenant_quotas`. A stale non-zero gauge on the
  first would have left the Binding Ready and the fix inert, so both were sampled **at the same
  instants** inside a slow-branch window:

  ```
  T0+0.50 s  /metrics DOWN                                    admin API DOWN
  T0+2.57 s  effective_bytes{dom-c} 0, requested 536870912     eff 0, req 536870912
  T0+32.32 s effective_bytes{dom-c} 536870912, capacity 2Gi    eff 536870912
  ```

  The series **exists and reads zero** — so the branch taken is `ZeroGranted`, not the absent-tenant
  one and not a stale figure. The two paths agree throughout, which only sampling both can show.
  ⚠️ `/metrics` comes up about **0.5 s after** the admin API, so the reconciler's earliest read in a
  restart is a connection failure — `MasterNotScraped`, also not Ready — rather than a stale value.
- ⚠️ **The condition messages themselves are still only unit-tested.** No CR status was read on a
  cluster in any of these runs; what was verified here is the **input** they branch on. The hop that
  is unverified — decode → axis → phase — is the one a mutation check covers, and the hop that a
  mutation check **cannot** cover — what a real master writes into `/metrics` during that window —
  is the one with real evidence. That is why this is acceptable rather than merely acknowledged.

  ⇒ **Follow-up, and deliberately not part of this criterion**: an e2e case that manufactures the
  slow branch on a cluster (delete the leader Pod until it lands off its member's node, with objects
  held) and asserts on the **CR status** — the pool at `NothingToAllocate` and every Binding at
  `ZeroGranted`, neither Ready, both clearing on their own. It belongs with the other e2e cases and
  not in a scale envelope, and it is written down here so the gap is an entry point rather than a
  sentence that was read once.

- **③a is per object discarded, not per object held** — the scan stops once it has freed enough, so
  its cost tracks the reclaim, and the number is only linear in how much is being reclaimed. The
  10^4 point (3.53 µs/object) is **not in the fit**: its whole net signal is 14.1 ms, the same order
  as the measurement noise there — the transfer floor for an identical operation differed by 33%
  between two runs — so whether it departs from the line cannot be told from this data. Its cause is
  unknown and nothing here should be read as explaining it.
- **③a's write cost was subtracted, not ignored.** The object that trips the eviction is scaled to the
  population, so its transfer time scales too; a control domain with room to spare wrote the same
  three sizes to measure that floor. Without that subtraction the transfer would have manufactured a
  cost proportional to N and it would have looked like the answer.

⛔ **③a is not a proxy for ③b, and its number is not a lower bound for ③b's.** They are two different
mechanisms in the store, not two scales of one. Tenant eviction starts at a random shard, walks that
domain's own metadata, and **stops as soon as it has freed enough** — its cost tracks the bytes being
reclaimed, not the population. Global eviction traverses metadata in full to rank candidates — its
cost tracks the population, which is what the upstream figure below describes. Reading ③a as an
optimistic ③b would take a number about reclaiming *n* bytes and present it as a number about
scanning *N* objects.

**③b is out of reach on this hardware, and that is a measurement limit rather than a wrong question.**
Global eviction begins at the store's high-water mark: 8 Gi of segment at a 0.9 ratio is 7.2 Gi, which
is **120 million** 64-byte objects — around 45 hours of writes at the rate measured here. The
mechanism is real and will engage in a large enough deployment; this environment simply cannot reach
the population that triggers it. The upstream figure below is the only evidence available for it, and
**this spec has not independently verified it**.

⛔ **③c cannot be measured at all, and the reason is a gap rather than a limit.** Snapshotting needs
`MOONCAKE_SNAPSHOT_LOCAL_PATH` in the leader's environment — the store's local object store refuses to
construct without it, and no flag substitutes, because `SnapshotObjectStore::Create` throws before
`snapshot_backup_dir` is ever read. The leader's rendered surface passes **flags only**. So the store
cannot be started with snapshotting on by anything this operator produces.

That is not the same as ③b being out of reach, and not the same as the question ④ **used to** ask
having been the wrong one. Three distinct reasons, and the third is the only one that is a gap:

| Row | What it is | What it means |
|---|---|---|
| ④ *as originally written* — superseded | a decision already made | HA is refused at admission, so that row asked for a number on a configuration this spec declines to produce. The ④ **now** in the measurement table asks a different question — the ledger's recovery across a single master's restart — and is measurable; what replaced it, and why the two are not substitutes, is below |
| ③b | a limit of this hardware | global eviction is real and engages at scale; this environment cannot reach the population |
| ③c | **a question never asked** | nobody decided whether to support snapshotting. Not refused — never considered |

**③c is therefore a product gap this measurement surfaced, not a measurement that failed.** Deciding
to support snapshotting means opening an environment passthrough on the leader, which is an API
surface and a decision in its own right — and it is deliberately not being made here, because an
acceptance row should not settle a product question by needing it.

**The volume, at least, is not the obstacle.** With it on, the leader mounts
the writable quota-policy volume — an `emptyDir` with no `Medium` and no `SizeLimit`, so it is node
disk rather than tmpfs — and the snapshot path goes to a **subdirectory** of it, clear of the policy
file the store maintains by renaming over it. Measured on a real cluster: 147 GB available, writable
by the store's uid.

⛔ **What that volume does NOT cover is a delivered conclusion, not a caveat.** `emptyDir` survives a
**container** restart and disappears with the **Pod** — and the leader's strategy is `Recreate`, so
every upgrade and every flag change is a Pod replacement. **Snapshotting therefore covers process
crashes and OOM kills, and covers none of the routine operational restarts.**

**Two independent paths lead there, and only one of them is the volume.** What a restored snapshot
brings back is the master's map of which region holds which key — the bytes themselves never moved,
because they live in the process that contributed the segment. Measured directly: a client that
closes its connection takes its segment with it and every object on it disappears, `key_count`
dropping to 0. So a restart that also replaces the **members** leaves the recovered map pointing at
nothing, and replacing the whole backend replaces both. ⚠️ Bounded: what a restore produces once the
segment provider is gone was **not** measured — in every experiment here the contributing process
outlived the master — so whether that yields orphaned entries or none is unverified. Either way no
data comes back, which is why the conclusion above stands on the volume path alone.

A reader planning around
snapshots needs that sentence, and a cell reading *"waits on a volume"* would not have given it to
them.

> **Why no PVC** — its only gain would be covering the Pod replacement above, and what a restored
> snapshot brings back is narrower than it sounds. Measured directly on the store, outside this
> operator: restore keeps exactly those objects whose lease is **still valid when recovery completes**,
> and a completed write leaves its object without one. A read grants a lease, so the survivors are the
> objects read shortly before the restart — and since a dead master serves no reads, nothing renews
> during the outage. The window is `lease TTL − restart duration`: at the 10 s default against a
> ~4 s restart, **only what was read in the last ~6 seconds survives**. A hot key read every 30
> seconds is lost with the cold ones.
>
> ⚠️ Two things follow that a flat "snapshots do not help" would have hidden. The window is a
> **configurable** one — a 300 s TTL against the same restart kept 100% of the read objects in the
> same experiment — so this is a tunable trade-off, not a dead end. And widening it is not free:
> a longer lease also blocks eviction, which is what a quota relies on to reclaim space (F11), and it
> enlarges every snapshot, whose cost is exactly what ③c cannot measure here. Three consequences, one
> of them unmeasured.
>
> The rule was isolated on a single variable rather than inferred: same 30 s TTL, same snapshot taken
> while the lease was live, two restarts. The 20 s one recovered with **2.9 s of lease remaining** and
> kept every read object; the 63.8 s one kept none. Had the snapshot's own instant been the test, both
> would have kept everything. ⚠️ Still bounded: no real access pattern has been replayed against it,
> so what fraction of a working cache survives a restart is unknown.

⚠️ Recorded because the earlier reasoning here was wrong in a specific, reusable way: the "no volume
at all" observation was taken on a `multiTenancy: false` backend and then carried across the change to
`true` without being retaken. **An observation that was true keeps looking like data after its
premises are replaced** — its only tell is that its timestamp precedes the change.

⛔ **④ used to ask for metadata takeover time on HA failover, and that was the wrong question rather
than a hard one.** There is no HA master in this scope — the flags that would elect a leader among
several are refused at admission — so the row was asking for a measurement on a configuration this
spec declines to produce, and no amount of effort could have filled it honestly. It is recorded here
rather than quietly rewritten, because the way it survived is the reusable part: the exclusion was
enforced in code and stated in a code comment, and **was absent from this document's Non-Goals**, so
a reviewer reading only the spec had nothing to contradict. The Non-Goal now exists.

⇒ Both of ③'s recovery half and ④ came from the same place: a general template for "what you measure
on a system with a leader", never checked against what THIS spec delivers. An acceptance row has to
be read against the Non-Goals, and the Non-Goals have to be complete enough to read against.

**The measurement ④ replaces it with is not a substitute for it** — it is the question this spec's
own shape actually raises. A single master's ledger is durable (a tenant's quota is written through
on every change) while its object index is not, so what ④ asks is how fast the durable half comes
back. ⚠️ Adjacent work on the backend has already timed the *other* half on this shape — key
readback and capacity recovery across a `Recreate` rollout — at **3.5–4.1 s typical, with one
unreproduced outlier near 28 s, so the upper bound is unknown**. That figure is not ④: it never
observed the ledger. Quote it as a neighbouring result and never as this row's answer, and do not
present whatever ④ measures as an upper bound either.

**Two upstream data points bound the expectation and are not substitutes for the measurements:**
single-threaded eviction scanning at roughly **1 second per 1,000,000 objects**, nearly all of it in
a full metadata traversal, which extrapolates linearly to ~100 seconds per scan at 10^8 objects; and
a **still-open** RFC discussing recovery at tens-of-millions to ~100M objects and *asking for* future
large-scale measurements — which is evidence that this scale is **unverified**, not evidence of
proven capacity.

### Notes / Constraints / Caveats

#### Source of the measured facts

A capability experiment run **2026-08-28** against the official artifact (PyPI
`mooncake_transfer_engine` **0.3.12.post1**, cp312/x86_64) and a second CUDA-13 image carrying the
same build family. **Both agreed.** Everything below marked *measured* comes from that run, not from
documentation; where the two disagree, the measurement is what this spec builds on.

Two claims here are read from **upstream source at the pinned `v0.3.13`** rather than from that run,
and they are labelled as such where they appear: which metric families the master serializes, and
that a labelled counter emits no sample line before its first observation. They are used only to
explain an *absence* the experiment saw — never to assert a value the experiment did not.

#### Tenant quota policy file schema

```yaml
version: 1          # REQUIRED — omitting it aborts the master (F6)
tenants:
  - name: team-a
    quota: 1GB      # positive integer, optional B/KB/MB/GB/TB suffix
  - name: team-b
    quota: 2GB
```

Name constraints: non-empty, unique, **must not start with `_`**, no NUL or control characters.

#### Connectors

```
--enable_multi_tenants=true
--tenant_quota_connector_type=file
--tenant_quota_connector_uri=/etc/mooncake/tenant_quotas.yaml
```

```
--enable_multi_tenants=true --cluster_id=mooncake_cluster
--tenant_quota_connector_type=etcd
--tenant_quota_connector_uri=127.0.0.1:2379
#  policy is stored at  mooncake-store/<cluster_id>/tenant_quota_policy
```

`--tenant_quota_connector_type` defaults to `file`.

#### Measured admin-API response body (0.3.12.post1)

**⛔ Every response is WRAPPED, and the experiment's log recorded only the payload.** The snapshot
below is what the experiment saw, and it is right — but it arrives inside an envelope, so a decoder
written to the shape as first recorded here fails against the very build that was measured. Read from
`master_admin_service.cpp` **at the `v0.3.12.post1` tag**, which is the experiment's own build:

```json
{"success": true,
 "data": {"tenant_id":"team-a","requested_quota_bytes":1073741824,"effective_quota_bytes":0,
          "used_bytes":0,"reserved_bytes":0,"committed_count":0,"metadata_object_count":0,
          "over_quota":false,"has_explicit_policy":true}}
```

- GET without `tenant_id` returns `data` as an **array** of snapshots; GET with one, and PUT, return
  a single snapshot; DELETE returns `data` **optional** — absent or null when there was nothing to
  remove. There is no unwrapped path on any handler.
- The nine fields above are the whole snapshot at this tag. The upstream documentation also lists
  `charged_bytes` and `admission_closed`; **0.3.12.post1 declares neither**, so neither may be
  required nor invented. Status is mapped from these; `used_bytes` and `reserved_bytes` are the
  equivalents.
- A **bare** snapshot — the shape this section first recorded — is refused as malformed rather than
  accepted, so the omission cannot quietly return.
- A **refusal** is enveloped too, in its own shape: `{"success": false, "error_code": …,
  "error_message": …}`, written by one helper for every failing route.

#### The tenant surface changed shape in 0.3.13

**⛔ `v0.3.13` REMOVES four of the figures 0.3.12.post1 reports, on both surfaces at once.** This is
not the "documentation lists fields the build does not send" case recorded above; it is the reverse,
and it is a breaking change to the exact fields this spec's status is built from.

| | 0.3.12.post1 (measured) | v0.3.13 |
| --- | --- | --- |
| admin snapshot | 9 fields | **7** |
| per-tenant series | 8 | **6** |
| occupancy | `used_bytes` + `reserved_bytes` | one `charged_bytes` |
| object counts | `committed_count`, `metadata_object_count` | *gone* |
| — | — | `admission_closed` added |

Read from `HttpTenantQuotaSnapshot` in `master_admin_service.cpp` and the exposition it renders, at
each tag. The removal of the first pair is deliberate rather than incidental: 0.3.13 carries a test
asserting a `used_bytes` or `reserved_bytes` series is **not** in the body.

Two consequences for this spec:

- **`usage` excluding reserved bytes is only possible on 0.3.12.post1.** 0.3.13 keeps ONE occupancy
  figure and charges it at `PutStart` rather than at `PutEnd`, so in-flight reservations are already
  inside it and there is nothing to subtract. The separation F-numbered here is a property of the
  measured build, not of the artifact.
- **Against 0.3.13 the decoder reads no occupancy at all**, because it looks for series names that
  build does not emit. It does not fail — every figure is a pointer and an absent series is an absent
  pointer — so a Binding would report `requestedQuota`, `effectiveQuota` and `overQuota` and simply
  no `usage`. Silent, and correct by this spec's own "absent, never zero" rule, which is exactly what
  makes it worth writing down.

#### The two error codes that are not what their status suggests

Both are read from the same tag, and both decide a Condition, so neither may be keyed on the HTTP
status alone.

- **409 carries three different meanings.** `ErrorCodeToHttpStatus` maps
  `UNAVAILABLE_IN_CURRENT_MODE`, `UNAVAILABLE_IN_CURRENT_STATUS` **and `TENANT_NOT_EMPTY`** to
  conflict. Only the first is F5's multi-tenancy precondition. The third is answered on **DELETE of a
  tenant that still holds objects** — which is the finalizer's own path (F12), so an unconditional
  409 → `MultiTenancyDisabled` would put a false Condition on the one call whose answer decides
  whether an object can be released. The client keys on the body's `error_code`, not the status.
  Reading the `error_message` instead would work today and stop working silently: the master renders
  the enum's own spelling as the message only when the handler passes none, which is what every
  ledger handler currently does.
- **…and the status still has to be asked first, because `-1011` also arrives as 503.** A leader
  whose service plane is not yet active answers `UNAVAILABLE_IN_CURRENT_MODE` under
  `service_unavailable`, from the same helper. Read by code alone, a master that is merely starting
  would be reported as one somebody has to go and reconfigure. So: status decides the class, and the
  code decides which refusal within 409.
- **The two `-600` refusals are indistinguishable by code.** A bad `tenant_id` and a non-positive
  quota both return `INVALID_PARAMS`: `ParseAdminTenantId` uses it for empty and for invalid alike,
  and the zero-quota refusal uses it too. Only the message differs — `Missing or invalid tenant_id`
  against `Tenant quota must be positive`. The client therefore separates them by message, and that
  is not a shortcut to be replaced later with a code check: the code cannot tell them apart.

#### Rejection paths — the webhook's pre-validation list

| Input | Master's response |
|---|---|
| `requested_quota_bytes: 0` | `-600` / 400 `Tenant quota must be positive` |
| `requested_quota_bytes: -1` | `-600` / 400 `Invalid JSON body: Failed to parse number` |
| `tenant_id=_bad` (leading underscore) | `-600` / 400 `Missing or invalid tenant_id` |
| `tenant_id=` (empty) | `-600` / 400 `Missing or invalid tenant_id` |
| Unknown tenant on GET | `-704` / 404 `OBJECT_NOT_FOUND` |
| The API at all, when multi-tenancy is off | `-1011` / **409** `UNAVAILABLE_IN_CURRENT_MODE` |

#### `domain` → `tenant_id`: measured isolation

The store client's setup signature — `tenant_id` is a first-class parameter, default `'default'`:

```python
setup(local_hostname: str, metadata_server: str, global_segment_size: int,
      local_buffer_size: int, protocol: str, rdma_devices: str, master_server_addr: str,
      engine: object = None, enable_ssd_offload: bool = False, ssd_offload_path: str = '',
      tenant_id: str = 'default', enable_client_http_server: bool = False,
      client_http_port: int = 9300) -> int
```

Measured with **one key** written by two clients under different `tenant_id`s:

```
setup(tenant_id=team-a) rc=0 ; put -> 0 ; get len=4096 first=b'AAAA'
setup(tenant_id=team-b) rc=0 ; put -> 0 ; get len=4096 first=b'BBBB'
re-get team-a          len=4096 first=b'AAAA'      <- not overwritten
```

#### `tenant_id` **is** the reuse domain — which is why an admin names it

Mooncake has **one** `tenant_id` carrying **both** isolation and quota, while this object model wants
two levels: the namespace grant above, reuse domain below. It gets both by making the namespaced
object that carries the grant also the object that registers the domain, exactly one per Binding.

Binding `tenant_id` to the **reuse domain** rather than to the namespace is the correct choice
because **correctness beats administrative convenience**: the domain is what the storage layer
isolates on, so keying quota on the namespace would leave the ledger describing a boundary the cache
does not have.

Two consequences follow, and both are load-bearing:

- **A domain is a quota ledger, so naming one is a privileged act.** A workload free to name its own
  domain could mint a new tenant — and a fresh ceiling — for every name it invents, and
  `spec.quotaCeiling` would bound nothing. Domain naming therefore sits on `KVCachePoolBinding`,
  which only an admin creates, and relaxing that is a Non-Goal rather than an oversight.
- **`spec.quotaCeiling` is that tenant's `requested_quota_bytes`, not an aggregate over anything.**
  One Binding, one domain, one tenant, one number — so the ceiling is the storage layer's own request
  figure rather than a total this operator maintains. What it is *not* is a granted amount: the
  master reduces every tenant's effective quota in proportion when requests exceed allocatable
  capacity, and `status.effectiveQuota` is what was actually granted. An aggregate divided across
  several domains is recorded as rejected in *Alternatives*.

#### The scale limit that bounds the derivation

- `MasterService` holds **1024** in-process `MetadataShard`s, each an
  `unordered_map<TenantId, TenantState>` with `TenantState.metadata` an
  `unordered_map<string, ObjectMetadata>`. **1024 shards is lock striping, not horizontal scale** —
  memory and lookup both stay in one address space.
- `ObjectMetadata` is not light: a UUID client_id, timestamps, an object checksum, two strings
  (`group_id`, `user_key`), a `TenantId`, a `shared_ptr<Lease>`, a soft-pin timeout, a
  `TenantQuotaLedger`, and a `vector<Replica>`. **No credible per-object byte constant can be derived
  from source — it must be measured.**
- The related flag set, relevant to measurement ③c — **and to ④ only in what ④'s figure therefore
  excludes**: `-enable_snapshot`, `-snapshot_interval_seconds` (600),
  `-snapshot_child_timeout_seconds` (300 — **so snapshots are taken by forking a child process**),
  `-snapshot_retention_count` (2), `-snapshot_object_store_type` (`local` | `s3`),
  `-snapshot_catalog_store_type` (`embedded` | `redis`), `-snapshot_backup_dir`,
  `-enable_snapshot_restore`. ⛔ **Flags alone cannot turn snapshotting on** — the local object store
  refuses to construct without `MOONCAKE_SNAPSHOT_LOCAL_PATH` in the environment, and
  `SnapshotObjectStore::Create` throws before `-snapshot_backup_dir` is ever read.

#### Repository conventions this feature lands under

- Go files are snake_case: `kv_cache_pool.go`, `kv_cache_pool_binding.go`. Never flat-concatenated.
- CRDs are **not** chart files. They are generated into `api/worker/v1alpha1/zz_generated.crds.go` by
  `make generate` and installed by the worker itself through `pkg/worker/apis/setup.go`. Webhook
  configurations are likewise generated into `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
  from the `+k8s:webhook-gen:` markers and installed by `pkg/worker/webhooks/setup.go`. **This spec
  therefore touches no chart file**: the worker's ServiceAccount is already bound to `cluster-admin`,
  so no RBAC rule is needed either.
- Reconcilers are registered in `pkg/worker/controllers/setup.go`; webhooks in
  `pkg/worker/webhooks/setup.go`. Both lists are hand-maintained and a new entry that is not added
  there compiles and does nothing.
- Status shape follows `api/v1.Status`: `Phase`, `PhaseMessage`, `Conditions`. `pkg/kubeapistatus`
  reaches the conditions reflectively, so the controller is unaffected by whether the triple is
  reused from `api/v1` or declared in `v1alpha1` — T1 settles which the generator accepts, and
  nothing downstream depends on the answer.
- Quantities are `resource.Quantity` (`100Ti`, `20Ti`), matching `InstanceTypeResource`. Ratios are
  **strings** with a validation pattern (`"0.80"`, `"0.87"`), never floats — the same shape the
  measured surface uses.
- Tests use `sigs.k8s.io/controller-runtime/pkg/client/fake` and table-driven cases, per the packages
  beside them; there is no envtest in this tree.

#### Cross-spec seams

- `KVCacheBackend` comes from the companion backend spec. `spec.backends` holds **names**, so nothing
  in the API types depends on that type at compile time: T1 can land before it. The webhook and the
  reconciler resolve the backend and **do** depend on it — those tasks are gated on it.
- **`KVCachePoolBinding.status.usedBy` is a seam pointing FORWARD, to the model-deployment spec —
  and it is in this same repository.** That spec declares `ModelDeployment`, whose
  `spec.kvCache.poolRef` names a Binding in its own namespace, and its CRD is generated into this
  tree's own `api/worker/v1alpha1/zz_generated.crds.go`. So the writer of that list is a type this
  repository will define and does not define yet — not another repository's, and not a
  cross-team contract. Nothing here is gated on it: this spec's reconciler reads the list and
  enforces on it, which is complete on its own. **What is NOT available until that spec lands is
  the protection**, because an unwritten list is an empty one (F12). The direction of the dependency
  is worth stating plainly: this spec ships the enforcement, that spec supplies the fact it enforces
  on, and neither blocks the other from being built.
- **Two things are required of the master's Pod.** Neither is a field of this spec's own two types,
  because both live on `KVCacheBackend`; the first is a switch T2 added there, the second is the
  workload T8.5 renders behind it:
  1. `--enable_multi_tenants=true` whenever `spec.leader.multiTenancy` asks for it. With it off,
     every request falls into the default tenant, the shard index degrades to a plain key hash, and
     different domains collide with each other's cache — which destroys the domain semantics this
     spec exists to provide.
  2. The tenant quota policy file on a **writable** volume, because a `PUT` rewrites it (F6). A
     read-only mount breaks the first successful quota write, and breaks it at the filesystem where
     nobody is looking.

  Neither is assumed at runtime either. Criterion 11 requires each to fail loudly —
  `MultiTenancyDisabled` and `QuotaPolicyNotWritable` Conditions, and a pool held away from Ready —
  because the backend can be edited after admission and a mount can be right in the renderer and
  wrong on the node.

  **⛔ The second seam does not exist in the merged backend work, and its absence is fatal rather
  than degrading — which is why T8.5 builds it here instead of leaving it a seam.** The merged
  backend renders no ConfigMap mount, no initContainer and no connector flag at all. The upstream
  code says what that costs, and it is not a missing seed:

  - `--tenant_quota_connector_type` defaults to `file` and `--tenant_quota_connector_uri` defaults to
    **empty**. The file connector refuses an empty uri, and the master's constructor throws on that
    refusal rather than degrading. So `--enable_multi_tenants=true` without a uri is a master that
    does not start — CrashLoopBackOff, unconditionally, on every backend this spec's own T2 switch
    turns on.
  - The file is not a cold-start seed. Both admin-API write paths persist to the connector **before**
    applying, and answer `PERSISTENT_FAIL` when that write fails. So a read-only mount does not lose
    durability; it fails every quota write.
  - The file must already exist when the master starts: the loader treats an unopenable file as an
    error and the constructor rethrows it. An empty file does not satisfy it either — the parser
    requires a YAML map with `version: 1` and a `tenants` sequence.

  All three hold at **both** tags this spec touches — `v0.3.12.post1`, the build the capability
  experiment ran, and the pinned `v0.3.13`. That is worth stating because the two builds do NOT agree
  everywhere (see *The tenant surface changed shape in 0.3.13* below), so agreeing here is a fact to
  check rather than one to assume.

  Those three together leave exactly one shape, and it is the one *Object model* already describes:
  a writable `emptyDir`, seeded by an initContainer from the ConfigMap, with the uri pointing at the
  copy. Neither a direct ConfigMap mount (kubelet mounts it read-only, and the writer renames a
  sibling temp file into the same directory) nor a bare `emptyDir` (nothing to load) is viable.

  A fourth constraint comes from this spec's own division of labour: the ConfigMap is written by the
  **pool** reconciler, so a `multiTenancy` backend with no pool bound to it has nobody to write one.
  The ConfigMap volume is therefore `optional`, and the initContainer falls back to writing the empty
  policy — the exact document the renderer already emits for an empty tenant set — so the master
  starts on a backend that no pool has reached yet.
- **Eviction is not a seam at all: this spec asks the backend spec for nothing on that axis.**
  `-eviction_high_watermark_ratio` and `-eviction_ratio` are startup gflags on the master process and
  the measured HTTP surface has no eviction endpoint, so eviction is not runtime-mutable the way quota
  is. Decisively: one master can serve several pools, so a per-pool eviction ratio is unimplementable
  for both of them the moment two pools share a backend. The backend spec does not render either flag
  from a field either — its leader renderer emits no flag sitting at the artifact's own default, and
  its member rules record the tiering knobs as deliberately absent because reaching them is what its
  `extraArgs` escape hatch is for. So there is no `spec.eviction` here, and no `status.watermark`
  either (F1).

### Boundaries

- **Always:** render the quota policy file **whole**, from validated inputs, through one code path
  shared by the webhook and the reconciler; keep `spec.backends` at exactly one and `spec.domain` at
  exactly one; keep every referential query inside a single scope; converge the ledger per **master**
  so a shared backend is never half-erased; state a measured fact with the build it was measured on.
- **Ask first:** anything that changes the connector choice (file vs etcd), anything that adds a
  datastore or a subchart, anything that renders or mutates the master's Pod beyond the one thing
  T8.5 owns — the policy volume, its initContainer and the connector flag, all rendered only when
  `multiTenancy` is on — anything that hangs more than one domain off a Binding, and any change to
  `spec.quotaCeiling`'s meaning.
- **Never:** mount the quota policy file read-only; pass user-supplied file content to the master;
  let a workload name its own reuse domain; admit a Binding onto a domain another Binding already
  holds; delete a ledger entry another pool's Binding owns; arbitrate between two namespaces claiming
  one domain; describe `overQuota` as a write barrier, which F11 measured it is not; ship a field whose
  enforcement does not exist — that rule is why there is no `spec.eviction`.

### Risks and Mitigations

- **Two CRDs double the `make generate` failure surface, and that failure is destructive** → T1 does
  nothing but land the two types and get the generator green, from a module-suffixed physical path,
  verified by a second `make generate && git diff --exit-code`. No behaviour lands in that task, so a
  generator problem is contained to a diff of API types and generated files.
- **The policy file is a crash surface, not an error surface** → it is operator-rendered from
  validated inputs only, pre-validated by a webhook running the same renderer, and never carries
  user-supplied content. A refused input yields no file rather than a partial one.
- **A read-only ConfigMap mount would break the first successful quota write** → the file connector
  is placed on a writable `emptyDir` seeded by an initContainer copy, and the ConfigMap remains the
  desired state that the reconcile loop re-converges.
- **`overQuota` is not enforcement, and it is named as if it were** → T12 measured what it actually
  reports (F11): the master never lets the charge overshoot the grant, so the flag stays false on the
  path everyone expects to set it. It is not described as a write barrier anywhere; the barrier is
  described where it exists, and the eviction that stands in for it is asserted rather than left in
  prose.
- **A pool with nothing mounted grants everyone zero and looks healthy** → F10 makes it a Condition
  on the pool and on every Binding, and the pool does not report Ready.
- **`quotaCeiling` reads as a granted amount rather than a request** → the field's doc comment, the
  reference page and this spec all state that it is the tenant's `requested_quota_bytes` and that
  `status.effectiveQuota` is what was actually granted.
- **Two Bindings on one domain would collide on one ledger** → domain names are unique cluster-wide
  and the second claim is refused at admission, so the collision is an API error at `kubectl apply`
  rather than a Binding that silently declines to manage itself; the reconcile-time refusal remains
  only as the backstop for two creates racing the webhook's cache (F9).
- **`blockSize` or `dtype` changed under a warm cache is silent tensor corruption** → every field of
  `spec.domain` is webhook-immutable, with a test of its own rather than a doc note (T6).
- **A backend shared by two pools has one ledger and one policy file** → convergence is per master,
  over every pool bound to that backend, and no pass deletes an entry another pool's Binding owns
  (F6, F7, F12).
- **A precondition the backend spec owns could quietly go missing** → multi-tenancy off and a
  read-only policy source each produce a named Condition and a pool that does not report Ready, and
  criterion 11 tests both; neither is taken on trust.
- **The master is a single process, and the scale claim is bounded rather than proven** → F13's four
  measurements are acceptance, and the two upstream data points are recorded as bounds, not as
  clearance.
- **The backend can be edited after the pool is admitted, turning multi-tenancy off underneath it** →
  admission is not trusted as the only gate; the reconciler treats the 409 as authoritative and
  refuses to write, level-based.
- **The reconcile writes N Binding statuses from one pool key, so a slow master stalls every Binding
  of that pool** → the scrape has a deadline from the reconcile context, and a failed scrape leaves
  the previous figures in place with a Condition rather than zeroing them.
- **An entry left on the ledger after its Binding is gone is invisible capacity** → the finalizer
  deletes the entries it owned before releasing, and the pool reconcile deletes the orphaned entries
  it can prove it registered, so an interrupted finalizer is repaired by the next pass rather than
  leaking.

  ⚠️ **Corrected during review; the original wording claimed a stronger sweep than exists.** It said
  the reconcile "deletes any entry no Binding claims". It does not, and must not: an entry this
  operator cannot prove it wrote may belong to somebody else on a shared master, so the delete path
  is gated on `registered` — the union of the pools' published domains and the live Bindings' — and
  refuses everything outside it. That refusal is correct, and it is what would have made a leak
  permanent: an entry written for a Binding that then vanished before either record named its domain
  would be unreachable forever. What actually closes it is the ordering in F12, not a sweep.

## Design Details

### Commands

Build, lint and test run **locally on darwin**; nothing in this feature is cgo or accelerator-bound.

```bash
go build ./api/... ./pkg/...
go test ./pkg/worker/kvcache/... ./pkg/utils/quantityx/... ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...
make lint                     # golangci-lint over the whole module; a --fix pass, cold runs are slow
make test                     # go test -v -failfast -race -cover -timeout=30m ./...
make lint docs
```

**Code generation runs where the module path allows it.** `make generate` derives package paths
GOPATH-style and requires a working directory ending in `gpustack.ai/gpustack`; a worktree that does
not end there fails, and the failure is destructive. So T1 (and the two webhook tasks, which
regenerate `zz_generated.webhooks.go`) apply the source edit at a module-suffixed physical path, run
the generator there, and sync the delta back. When syncing with `rsync`, use `--filter='P .git'` and
**not** `--exclude '.git/'` — a worktree's `.git` is a *file*, which the latter misses, and combined
with `--delete` it destroys the receiver's repository.

```bash
make generate                 # T1, T5, T6 — from a module-suffixed physical path
make generate && git diff --exit-code    # the drift gate, run a second time
```

The e2e acceptance runs on a local Kubernetes cluster (k3s / docker-desktop / kind) with the dev
image rolled out. No GPU, no RDMA, no cloud.

### Project Structure

```
api/worker/v1alpha1/
  kv_cache_pool.go                     # KVCachePool, KVCachePoolList, spec/status types
  kv_cache_pool_binding.go             # KVCachePoolBinding, KVCachePoolBindingList, spec/status types
  zz_generated.crds.go                 # + two CRDs (generated; installed by pkg/worker/apis)
  zz_generated.deepcopy.go             # (generated)
  zz_generated.register.go             # (generated)
  zz_generated.model_name.go           # (generated)
  generated.pb.go / generated.proto    # (generated)
api/worker/v1alpha1/kv_cache_backend.go  # EXISTING, T2 only: + spec.leader.multiTenancy
api/worker/zz_generated.openapi.go     # (generated)

pkg/worker/kvcache/                    # EXISTING package — reduced to what ANY backend shares
  image.go                             # EXISTING; pull-policy resolution, backend-agnostic
  resource.go                          # ResourceType / ResourceNoteBackend, moved up out of the
                                       #   leader renderer: a watch filters by them before it knows
                                       #   which implementation rendered the object
pkg/worker/kvcache/mooncake/           # NEW package — everything specific to the Mooncake store
  quota_policy.go                      # render + validate the tenant quota policy file (one path,
                                       #   shared by the webhook and the reconciler)
  quota_policy_workload.go             # the policy ConfigMap: name, data key, owner reference
  tenant_quota.go                      # /api/v1/tenant_quotas as methods on the EXISTING AdminClient;
                                       #   typed errors per rejection
  tenant_metrics.go                    # parse the per-tenant + global gauges into one sample set
  admin.go, leader_*.go, member_*.go   # EXISTING, moved verbatim
  keys.go                              # EXISTING, T2 only: enable_multi_tenants joins Derived

pkg/utils/quantityx/                   # EXISTING package
  quantity.go                          # T3 only: quantityOverflowsInt64 promoted here from the
                                       #   backend webhook, so renderer and webhook share one rule

pkg/worker/controllers/worker/
  kv_cache_pool.go                     # ONE reconciler for both kinds, keyed by the pool
pkg/worker/controllers/setup.go        # + the reconciler

pkg/worker/webhooks/worker/
  kv_cache_pool.go                     # validating webhook: backends==1 + immutable, quota, backend
  kv_cache_pool_binding.go             # validating webhook: poolRef + domain immutable, ceiling,
                                       #   domain name unclaimed cluster-wide
  kv_cache_backend.go                  # EXISTING; T2: refuse enable_multi_tenants in extraArgs,
                                       #   T3: call the promoted overflow rule instead of its own
  zz_generated.webhooks.go             # (generated from the +k8s:webhook-gen: markers)
pkg/worker/webhooks/setup.go           # + the two webhooks

docs/                                  # the reference page for the two kinds
.claude/skills/gpustack-operator-e2e/cases/case-43.sh   # two namespaces, one pool
.claude/skills/gpustack-operator-e2e/cases/case-44.sh   # quota actually rejects a write
.claude/skills/gpustack-operator-e2e/SKILL.md           # + the two cases in its case table

.claude/skills/_e2e-lib/scripts/deploy.sh    # SHARED, not owned by any task below: refuse to
                                             #   install over an existing release
.claude/skills/_e2e-lib/scripts/teardown.sh  # SHARED: delegate the cleanup to the chart's own
                                             #   files/cleanup.sh, and judge the CRD drain
.claude/skills/gpustack-operator-chart-e2e/cases/case-2.sh  # SHARED: the comment the delegation
.claude/skills/gpustack-operator-chart-e2e/SKILL.md         #   above made wrong, in both places
```

**Three of those files are shared harness, and no task owns them.** They are here because running
the two new cases hit them, and each was a defect that made a broken run look like a clean one:

- `deploy.sh` treated an **already-installed** release as the residue of its own failed attempt. Its
  retry loop cannot tell an interrupted install from a healthy prior one — both return non-zero — so
  it tore the healthy release down, taking its CRDs and every custom object with them, and installed
  again. That reinstall **succeeds**, so the caller sees an ordinary deployment and never learns the
  state it was standing on is gone. It now queries the release and refuses, naming `teardown.sh`.
- `teardown.sh` carried its own copy of the chart's `files/cleanup.sh` logic, "so the skill does not
  depend on the `deploy/` tree". The two had drifted, and **every** difference favoured the copy
  being wrong: it read `.spec.versions[0].name` where the original reads the *storage* version; it
  deleted Kueue and NFD CRDs **by name with no ownership check**, so a teardown on a cluster running
  its own Kueue deleted that Kueue's CRDs; and it matched APIServices and webhooks by name where the
  original first confirms the Service they point at is in this namespace. It now delegates, and adds
  the one thing a post-delete hook must not do — **fail** when the gpustack CRDs did not drain.
- `case-2.sh`'s comment described the copy that no longer exists.

No chart file is touched: CRDs and webhook configurations are generated into Go and installed by the
worker, and the worker's ServiceAccount is already `cluster-admin`.

### Code Style

The Binding's status, following this repository's discipline — a doc comment states behaviour and the
reason for it, a field that is *not* a guarantee says so where it is declared, and a measured mapping
names what it was mapped from:

```go
// KVCachePoolBindingStatus is the namespace's own view of the pool: what it asked for, what the
// pool actually granted, what it is using, and whether it is over.
//
// Every figure below is read from ONE tenant's series, because a Binding registers exactly one reuse
// domain and the storage layer's tenant IS that domain. Nothing here is summed, and no figure can
// hide a second domain behind it.
//
// Every observed figure is a POINTER, and the reason is the same one for all of them: a
// resource.Quantity is a struct and omitempty does not omit a zero-valued struct, so a value-held
// figure serializes as "0" on exactly the passes whose contract says there must be no field at all.
// The companion backend paid for this already — its Status.Capacity carries the reason in its own doc
// comment.
type KVCachePoolBindingStatus struct {
	// RequestedQuota is what the operator asked the pool for on this namespace's behalf: the
	// requested_quota_bytes it wrote for this Binding's one tenant. It is absent, with a Condition
	// saying why, whenever the operator refused to write — a master that answered that multi-tenancy
	// is off, or a policy source it cannot rewrite.
	RequestedQuota *resource.Quantity `json:"requestedQuota,omitempty" protobuf:"bytes,1,opt,name=requestedQuota"`

	// EffectiveQuota is what the pool actually granted. It is LOWER than RequestedQuota whenever the
	// sum of every tenant's request exceeds the pool's allocatable capacity: the pool then recomputes
	// each tenant's effective quota in proportion to what that tenant requested. A pool with no
	// mounted members grants ZERO to everyone; that case carries its own Condition rather than
	// appearing as an ordinary shortfall — and it is the case that makes the pointer load-bearing,
	// because a granted zero and an unobserved quota must not serialize the same way.
	EffectiveQuota *resource.Quantity `json:"effectiveQuota,omitempty" protobuf:"bytes,2,opt,name=effectiveQuota"`

	// Usage is what the master reports this namespace's reuse domain as holding, and WHICH figure
	// that is depends on the master's version rather than on this API. A master exposing used bytes
	// apart from reservations is read as committed bytes; one exposing a single charged figure —
	// 0.3.13's shape — charges it when a write STARTS, so in-flight reservations are inside this
	// number and cannot be subtracted.
	Usage *resource.Quantity `json:"usage,omitempty" protobuf:"bytes,3,opt,name=usage"`

	// OverQuota is true when this Binding's domain is over its effective quota. Existing objects stay
	// readable, deletable and reclaimable; it is new writes that are refused.
	//
	// A POINTER for the same reason the quantities around it are, and it is the easiest one to get
	// wrong: held by value with omitempty, an OBSERVED false — the ordinary, healthy case — omits
	// itself and becomes indistinguishable from a tenant nobody could scrape.
	OverQuota *bool `json:"overQuota,omitempty" protobuf:"varint,4,opt,name=overQuota"`

	// Blocks and HitRate are OBSERVED from the master and the engine, never declared. They are absent
	// when the scrape does not carry this tenant, because a fabricated zero hit rate on a warm cache
	// is worse than no number at all. Blocks is a pointer for that reason one level down: zero blocks
	// and "not in the scrape" are different facts, and an int64 held by value cannot tell them apart.
	Blocks  *int64 `json:"blocks,omitempty" protobuf:"varint,5,opt,name=blocks"`
	HitRate string `json:"hitRate,omitempty" protobuf:"bytes,6,opt,name=hitRate"`

	// UsedBy names the workloads in THIS namespace that hold the pool through this Binding. It is
	// always a single-scope query — nothing here ever looks across namespaces — and a non-empty
	// UsedBy is what the finalizer refuses deletion on.
	//
	// Entries leave Namespace empty: everything that can appear is in this Binding's own namespace,
	// so naming it would restate the object's own metadata on every entry.
	//
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	UsedBy []KVCacheObjectReference `json:"usedBy,omitempty" protobuf:"bytes,7,rep,name=usedBy"`

	// Phase, PhaseMessage and Conditions follow api/v1.Status: Phase summarizes the conditions.
	Phase        string             `json:"phase,omitempty" protobuf:"bytes,8,opt,name=phase"`
	PhaseMessage string             `json:"phaseMessage,omitempty" protobuf:"bytes,9,opt,name=phaseMessage"`
	Conditions   []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,10,rep,name=conditions"` // nolint: lll
}
```

And the two spec fields the whole design hangs on — the domain that *is* the tenant, and the ceiling
that lands straight on it:

```go
	// Domain is the reuse identity this Binding registers, and it is REQUIRED. It maps to the storage
	// layer's tenant_id (isolation) and cache_salt (prefix identity), so registering a domain creates
	// a tenant with a quota ledger of its own — which is why it is declared here, on an object only
	// an admin creates, and never by a workload that could otherwise mint tenants at will.
	//
	// Workloads pointing at the SAME Binding share KV; workloads pointing at different Bindings do
	// not. A namespace needing two reuse boundaries creates two Bindings against the same pool.
	//
	// EVERY FIELD IS IMMUTABLE. Name re-points this namespace at a different ledger and strands the
	// old one. BlockSize or Dtype changed under a warm cache is silent corruption: the writes
	// succeed, the reads succeed, and the tensors are wrong.
	//
	// +required
	Domain KVCachePoolBindingDomain `json:"domain" protobuf:"bytes,2,opt,name=domain"`

	// QuotaCeiling is what this namespace may consume in its reuse domain. It is written verbatim
	// into that one tenant's requested_quota_bytes, so it is the storage layer's own request figure
	// rather than a total this operator maintains.
	//
	// IT IS A REQUEST, NOT A GRANT. The pool reduces every tenant's effective quota in proportion
	// when the sum of requests exceeds allocatable capacity, and Status.EffectiveQuota is what was
	// actually granted.
	//
	// REQUIRED, because the state it would otherwise allow does not work. The storage layer has no
	// default policy to fall back on: a tenant it holds no policy for is refused outright, with the
	// same error a reuse domain that was never declared gets — measured on a real master, and stated
	// in the artifact's own header, where the code is spelled `TENANT_NOT_REGISTERED = -1701,
	// ///< Tenant has no quota policy.` A Binding without this field would pass admission, report
	// Ready and refuse every byte its workloads wrote.
	//
	// Required is also the direction that can be taken back. Should the storage layer ever grow a
	// default quota, relaxing this to optional keeps every object already written valid; going the
	// other way — optional today, required later — invalidates every object that omitted it.
	//
	// Held BY VALUE, like the pool's own ceiling and for the same reason: the schema guarantees the
	// key is present, so there is no unset to distinguish and a pointer would only add a nil case
	// nothing can produce. The webhook still refuses a value that is not positive.
	//
	// +required
	QuotaCeiling resource.Quantity `json:"quotaCeiling" protobuf:"bytes,3,name=quotaCeiling"`
```

Conventions: doc comments state behaviour and its reason, not the field name; a field that cannot
guarantee what its name suggests says so in its own comment; markers spell scope explicitly
(`+k8s:crd-gen:resource:scope="Cluster"` for the pool, `scope="Namespaced"` for the Binding, both
with `subResources=["status"]`); list fields carry `+listType=map` with their keys so a server-side
apply does not clobber a peer's entry.

**Turning a `Quantity` into bytes has exactly one correct spelling, and this repository already
holds it.** T3 renders a ceiling into the policy file as an integer byte count, which is the
conversion `Quantity.Value()` gets wrong *without failing*: measured on the companion backend,
`9223372036854775808` comes back as the **minimum** int64 and `1e30` comes back as **0**, and both are
then read as "not positive". `CmpInt64` saturates the same way and answers "equal" for a quantity
above the maximum. The one form that works compares against a `Quantity` built from the maximum —
`resource.NewQuantity(math.MaxInt64, resource.DecimalSI)`, then `Cmp` — because that path compares the
two as decimals. A binary-suffixed `8Ei` never reaches it: `ParseQuantity` saturates at the maximum,
so the field already holds a value `Value()` reports faithfully.

That rule is implemented once already, as `quantityOverflowsInt64` in
`pkg/worker/webhooks/worker/kv_cache_backend.go`, with the measurement in its own doc comment — but it
is package-private there, and T3's renderer lives in `pkg/worker/kvcache/mooncake`. **It is promoted to
`pkg/utils/quantityx` and called from both sides rather than copied** (T3). A second implementation of
"does this number survive an int64" is how a webhook and a renderer come to disagree about one
ceiling, and the disagreement would be silent in the direction that matters: the webhook admits, and
the renderer writes a quota nobody asked for.

### Implementation Plan

T1 and T2 are barriers by design: **nothing else starts until the generator is green**, and both of
them touch generated artifacts, so they are also the two tasks that may never run concurrently. T1
lands the two new types; T2 promotes the leader's multi-tenancy switch on the already-shipped
`KVCacheBackend`, which every quota write depends on. After T2 the plan opens into two lanes that
converge: the quota library (T3, T4), which the webhooks (T5, T6) then validate through; and the
controller, whose skeleton (T7) needs only T1 but whose behaviour (T8–T10) needs the library. The two
e2e tasks join them, and T13–T14 close.

Checkpoints: after T1 (two CRDs exist, nothing behaves); after T2 (a backend can be asked for a
tenant ledger, and the generator is green a second time); after T4 (the vendor protocol is covered
without a cluster); after T6 (nothing invalid can be admitted); after T10 (the loop converges on a
fake); after T12 (both e2e criteria hold on a real master); after T14.

- [x] **T1 · The two API types, and `make generate` green**
      Blocked by: None
      Owns: `api/worker/v1alpha1/kv_cache_pool.go`, `api/worker/v1alpha1/kv_cache_pool_binding.go`,
      and the generated artifacts they produce (`api/worker/v1alpha1/zz_generated.*`,
      `api/worker/v1alpha1/generated.*`, `api/worker/zz_generated.openapi.go`, and — recorded here
      because the first pass at this list missed them — the typed clients, listers, informers and
      apply-configurations `make generate` emits for any new kind, under
      `pkg/kubeclients/**/worker/v1alpha1/`. They are mechanical output of registering the two types,
      not a second piece of work, but a closed `Owns:` list that omits them is wrong rather than
      silent)
      Gate: review. **This task lands NO controller, NO webhook and NO chart change** — that is the
      whole point of it. It is the mitigation for introducing two CRDs in one spec, so the generated
      artifact risk lives in a diff of API types and generated files and nowhere else.
      Acceptance: `KVCachePool` is registered `scope="Cluster"` with `subResources=["status"]` and
      `KVCachePoolBinding` `scope="Namespaced"` with the same, both with their `List` types and
      `runtime.Object` assertions; the field set is exactly F1's and F2's — no `metadataStore`, no
      per-medium quota, **no `spec.eviction` and no `status.watermark`**, and `spec.domain` is a
      required singular struct rather than a list; `spec.backends` is `[]string`, which keeps this
      package free of a compile-time dependency on the backend type even though that type is now
      merged; ratios are validated strings — a `pattern`, which is safe on a ratio this operator
      COMPUTES in a way it would not be on an echoed vendor value, and which obliges T9 to format to
      that shape because a value failing it fails the whole status write; the pool publishes
      `status.clientEndpoint` and no admin address at all. `KVCacheObjectReference` lands here, as the
      one shape every `usedBy` in this family carries.

      **Every status quantity is a POINTER**, and this is the one shape decision the task cannot get
      wrong, because it is unfixable once published: `resource.Quantity` is a struct, and `omitempty`
      does not omit a zero-valued struct. Held by value, `status.requestedQuota` serializes as `"0"`
      on every pass where the operator refused to write — the exact reading the field's own contract
      says must be *absent*, and one a client cannot tell from a quota that really is zero. The
      companion backend paid for this already: `api/worker/v1alpha1/kv_cache_backend.go` carries
      `Capacity *KVCacheBackendCapacity` with the reason in its doc comment. `Blocks` is a pointer for
      the same reason one level down — `0` blocks and "not in the scrape" are different facts — and so
      is **`status.overQuota`**, which is the easiest one to miss because it is a `bool` rather than a
      quantity: held by value with `omitempty`, the ordinary healthy reading (observed, and false)
      omits itself and becomes the same bytes as a tenant nobody could scrape.
      **`spec.quotaCeiling` is NOT among them**, and neither is `spec.quota.total`: both are
      required, so there is no unset to distinguish and a pointer would add a nil case nothing can
      produce. F2 states why a ceiling may not be omitted.
      The status triple resolves (whether reused from `api/v1` or declared locally is settled here,
      and recorded in the task's notes).
      Verify: from a module-suffixed physical path, `make generate`, sync back, then `make generate &&
      git diff --exit-code` a second time; `go build ./api/...`; `make lint`. A rendered CRD asserted
      to carry no `required` entry for any of the four quota figures.

- [x] **T2 · `spec.leader.multiTenancy` on `KVCacheBackend`, and the flag it renders**
      Blocked by: T1
      Owns: `api/worker/v1alpha1/kv_cache_backend.go`, `pkg/worker/kvcache/mooncake/leader_flags.go`,
      `pkg/worker/kvcache/mooncake/keys.go`, `pkg/worker/webhooks/worker/kv_cache_backend.go`,
      `pkg/worker/controllers/worker/kv_cache_backend.go`, their tests, and
      the generated artifacts (the backend's CRD is regenerated into
      `api/worker/v1alpha1/zz_generated.crds.go`, not into the chart — see *Project Structure*)
      Gate: review. **This task changes a MERGED API** — the multi-tenancy field is additive and
      therefore compatible, but the `usedBy` retype below is not, and it is the only task in this spec
      that touches the companion feature. Both are isolated here for exactly that reason.
      Acceptance: the leader's per-tenant quota ledger is reachable through a field rather than
      through the escape hatch. `spec.leader.multiTenancy` is an optional bool defaulting to false;
      the renderer emits `--enable_multi_tenants=true` when set and emits nothing when unset, so an
      existing backend's rendered command line is byte-identical to what it renders today (asserted
      against the current golden output, not against a re-derived expectation); `enable_multi_tenants`
      joins `LeaderExtraArgsRules.Derived`, so admission refuses it in `extraArgs` with the
      two-sources-for-one-flag message every other `Derived` key already produces; a backend that
      flips the field is restarted by the existing leader-workload path with no special casing, because
      the flag is part of the rendered command and the workload already converges on that.
      Why a field and not the escape hatch: `extraArgs` is documented as the way to reach *what this
      API does not enumerate*, and a value another CRD's webhook validates against is enumerated by
      definition. A `KVCachePool` webhook reading `extraArgs["enable_multi_tenants"]` would be judging
      an unschema'd string (`"true"` / `"1"` / `"True"`) whose value domain belongs to the operator who
      typed it.
      **This task also retypes `KVCacheBackend.status.usedBy` from `core.TypedLocalObjectReference` to
      T1's `KVCacheObjectReference`**, so every `usedBy` in this family reads the same. It is a
      breaking status change and it is taken deliberately: the backend is on `main` but in **no
      release tag**, so nothing has consumed the old shape, and the alternative is two reference
      shapes that differ only by which spec happened to write them. The only reader is
      `formatKVCacheBackendConsumers`, which uses `Kind` and `Name` and never the `apiGroup` being
      dropped, so the change is a retype plus its test constructors. Asserted by the existing backend
      tests passing unchanged apart from that constructor.
      A cluster already running the older shape needs no migration step and gets none: its stored
      entries carry `apiGroup`/`kind`/`name` and no `namespace`, reads do not validate, so they list
      fine, and the first reconcile rebuilds `status.usedBy` wholesale into the new shape. Deleting
      the CRD and its objects before upgrading is simpler if anything is odd, and costs nothing —
      this API is in no release tag, so no cluster is running it on purpose.
      Verify: `go test ./pkg/worker/kvcache/... ./pkg/worker/webhooks/worker/...
      ./pkg/worker/controllers/worker/...`; from a module-suffixed physical path, `make generate &&
      git diff --exit-code`; `make lint`.

- [x] **T3 · The quota policy renderer and its validator**
      Blocked by: T2
      Owns: `pkg/worker/kvcache/mooncake/quota_policy.go`, `pkg/worker/kvcache/mooncake/quota_policy_test.go`,
      `pkg/utils/quantityx/quantity.go` (+ test) and the one call site in
      `pkg/worker/webhooks/worker/kv_cache_backend.go` that the promotion below moves
      Gate: T2 has landed, so the generator is green and the field set the renderer reads from is
      settled. T3 also touches a file T2 owns, so the two may not run concurrently — the edge above is
      what serializes them.
      Acceptance: one function renders the file — `version: 1` unconditionally, then one tenant entry
      per validated input with the quota in bytes — and one function validates an input set,
      returning typed field errors; the two share their rules, so the webhook and the reconciler
      cannot disagree about what is safe. Every name constraint is enforced: non-empty, unique, no
      leading `_`, no NUL, no control characters; quota strictly positive. **A refused input yields
      an error and no output at all** — never a file with the bad entry dropped, because a silently
      shortened tenant list is a quota nobody set.
      Rendering a ceiling into bytes is where `Quantity.Value()` answers wrongly without failing, so
      this task also **promotes** the existing `quantityOverflowsInt64` out of
      `pkg/worker/webhooks/worker/kv_cache_backend.go` into `pkg/utils/quantityx`, carrying its
      measurement doc comment, and calls it from both sides. It is a move, not a copy, and the existing
      backend webhook test must keep passing unchanged — see *Code Style* for the measurement and for
      why a second copy fails silently in the admitting direction.
      Verify: `go test ./pkg/worker/kvcache/... ./pkg/utils/quantityx/... ./pkg/worker/webhooks/worker/...`

- [x] **T4 · The admin-API client and the metrics reader**
      Blocked by: T2
      Owns: `pkg/worker/kvcache/mooncake/tenant_quota.go`, `pkg/worker/kvcache/mooncake/tenant_metrics.go`, and their
      tests
      Gate: the recorded response bodies and metrics document from the capability experiment are
      checked in as fixtures — they are the only record of what the measured build returns.
      **These land beside the EXISTING `AdminClient`, as methods on it** — not in a
      package of their own. The quota endpoints and the metrics exposition are served by the *same*
      port this package already reads (`KVCacheBackendEndpointNameAdmin`, whose doc comment states
      that one port serves the Prometheus exposition and the HTTP admin API both), so a second client
      would be a second set of rules for one connection: its own body limit, its own error excerpting,
      its own redirect policy. The last of those is not cosmetic — a zero-valued `http.Client` follows
      ten redirects, and the address being dialled comes from a CR a user wrote.
      Acceptance: list / query-one / create-update / delete against an `httptest` server; the decoder
      reads the `{"success", "data"}` **envelope** every handler writes and refuses a bare snapshot,
      and it accepts exactly the measured nine-field set without requiring `charged_bytes` or
      `admission_closed`, which that build does not declare; each rejection path maps to a distinct
      typed error keyed on the body's own code — 404 modelled as *absent* rather than as failure, and
      409 split so that only `UNAVAILABLE_IN_CURRENT_MODE` is the multi-tenancy precondition while
      `TENANT_NOT_EMPTY` gets a sentinel of its own, because that one is answered on the finalizer's
      DELETE; the metrics reader parses the eight per-tenant
      series and the three global gauges from one document, and reports a tenant absent from the
      document as absent rather than zero. The write paths reuse `adminReadLimit` and the existing
      excerpting, asserted by a test that an oversized error body is truncated the same way the read
      paths truncate it. The package's own doc comment is widened to say it now also carries the
      tenant-quota face of that port.
      Verify: `go test ./pkg/worker/kvcache/...`

- [x] **T5 · The `KVCachePool` validating webhook**
      Blocked by: T1, T3
      Owns: `pkg/worker/webhooks/worker/kv_cache_pool.go` (+ test), the regenerated
      `zz_generated.webhooks.go`, the entry in `pkg/worker/webhooks/setup.go`
      Gate: T2 has landed `spec.leader.multiTenancy`, so the multi-tenancy check below reads a typed
      field rather than an `extraArgs` string; and review.
      Acceptance: `spec.backends` of length ≠ 1 is refused with a message naming the reason (quota
      lands on one master's ledger); `spec.backends` is immutable on update; the referenced backend
      must exist, and one whose `spec.leader.multiTenancy` is false is refused with the consequence in
      the message (without the ledger, every quota write comes back
      `UNAVAILABLE_IN_CURRENT_MODE` and the pool can enforce nothing).
      **Both backend questions are asked on CREATE only**, and an EXTERNAL backend is asked neither:
      `spec.backends` is immutable, so no update can move which backend is named, while the backend
      itself may since have been deleted — and removing this pool's finalizer is an update, so a rule
      that re-read it would strand a pool undeletable for as long as its backend stayed gone, which
      is the ordinary order a stack is torn down in. An external backend runs a command line this
      operator never saw. Neither is a hole in F5: the reconciler's 409 is the level-based
      enforcement and admission is only the early refusal.
      `spec.quota.total` must be positive and must survive the conversion into a byte count, through
      **the rule T3 validates policy quotas with** — a pool total bounds every ceiling written under
      it, so the two may not hold separate opinions about which numbers are usable. T5 therefore
      exports that rule out of T3's file; what is shared is the RULE and not a rendered document,
      because the pool total never becomes a policy entry itself. **There is no eviction field to
      validate** — eviction
      is the backend spec's (*Cross-spec seams*), so a pool carrying one does not compile.
      Verify: `go test ./pkg/worker/webhooks/worker/...`; `make generate && git diff --exit-code`

- [x] **T6 · The `KVCachePoolBinding` validating webhook**
      Blocked by: T1, T3, T5
      Owns: `pkg/worker/webhooks/worker/kv_cache_pool_binding.go` (+ test), the regenerated
      `zz_generated.webhooks.go`, the entry in `pkg/worker/webhooks/setup.go`
      Gate: review
      Acceptance: `spec.poolRef` is refused on update (immutable), because re-pointing moves a
      namespace's grant silently and strands its bytes on the old master; the referenced pool
      must exist on create; `spec.quotaCeiling` must be positive and ≤ the pool's
      `spec.quota.total` — required, so there is no unset case to admit and none to test for; **`spec.domain` is required, and `name`, `blockSize` and `dtype` are each
      refused on update** — a `blockSize` or `dtype` changed under a warm cache is silent tensor
      corruption, so this is a webhook rule with a test of its own and not a doc note;
      `domain.blockSize` must be positive and `dtype` must be a non-empty lowercase token (the
      exhaustive dtype set belongs to the spec that owns workloads; this webhook enforces the
      syntactic form only); **`domain.name` must be claimed by no other Binding cluster-wide**, and
      the refusal states that cross-namespace sharing of one reuse domain is not supported here and
      names the holder (criterion 10) — the check is a single cluster-scoped `List` of
      `KVCachePoolBinding` through the manager's cache, so it walks no namespaces, and F9's
      reconcile-time refusal remains as the backstop for two creates that race that cache; the tenant
      name this Binding would contribute is run through T3's validator, which T6 exports out of that
      file the way T5 exported the quota rule. The name is therefore held to TWO rules — a DNS-1123
      label, which is this API's shape, and the leader's own lower bound — and the second is redundant
      today because the first is strictly stricter. It is asked anyway: a rule added to the leader's
      set then reaches this path without anyone remembering to copy it.
      **The pool is read on create, and on an update only when that update MOVED the ceiling.**
      `spec.poolRef` is immutable, so no update can change which pool is named, and asking about it
      unconditionally would put a different object in the path of removing this Binding's finalizer —
      a pool deleted before its Bindings is the ordinary teardown order. The consequence, taken
      deliberately: a ceiling left where it was is not re-refused when the pool total shrinks under
      it. That is a state to report, not an object to refuse — the reconciler already observes what
      the master actually granted, and reports the shortfall there.
      Verify: `go test ./pkg/worker/webhooks/worker/...`; `make generate && git diff --exit-code`

- [x] **T7 · The controller skeleton: one reconciler, pool-keyed, with the `poolRef` index**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/kv_cache_pool.go`,
      `pkg/worker/controllers/worker/kv_cache_pool_test.go`, the entry in
      `pkg/worker/controllers/setup.go` and a `setup_test.go` beside it — that list had no test at
      all, and "compiles and does nothing" is not a failure any other test can see
      Gate: review
      Acceptance: one reconciler `For` the pool and `Watches` the Binding, mapping every Binding event
      to `spec.poolRef.name`, with a field index on that path so a pool's Bindings are one scoped
      query, and a second index on the pool's `spec.backends` so the other pools on one master are
      also one query (F7); predicates ignore the operator's own status writes so the loop does not
      self-trigger; a
      Binding whose pool is absent still reaches the pass through the index; the reconciler is
      registered in `setup.go` (a reconciler missing from that list compiles and does nothing).
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T8 · Pool reconcile: the policy ConfigMap, the tenant ledger, and the two endpoints**
      Blocked by: T3, T4, T7
      Owns: `pkg/worker/controllers/worker/kv_cache_pool.go`, plus
      `pkg/worker/kvcache/mooncake/quota_policy_workload.go` — the ConfigMap's NAME and its data key are read
      by whoever mounts it too, which is the backend spec, so they are exported constants next to the
      other rendered-object names rather than literals in a reconciler
      Gate: T2 has landed, so a backend can be asked for the tenant ledger this task writes to; and
      review.
      Acceptance: one pass resolves the backend and reads its `status.endpoints[]` **by name**, which
      is the shipped shape: a `+listType=map` keyed on `name`, enum-constrained to `Client` and
      `Admin`. The reconciler dials the `Admin` entry and republishes only the `Client` one as the
      pool's `status.clientEndpoint`.
      Two consequences are asserted rather than assumed. **`Admin` is one port serving the Prometheus
      exposition and the HTTP admin API both** — the backend API says so where the constant is
      declared — so there is no third address to resolve and no assertion to make that a metrics
      address differs from an admin one. And **the `Admin` entry is republished nowhere**: a pool is
      cluster-scoped and readable by anyone with a pool RBAC rule, while the admin port is the write
      face of the quota ledger. A missing or empty `status.endpoints[]` is a backend that has not been
      observed yet: the pass writes no endpoint, sets a Condition naming it, and requeues — it never
      falls back to a derived Service DNS name, because a guessed address that happens to resolve is
      how a pool would silently drive the wrong master.
      The pass then
      renders the policy for that **master** — every pool bound to the backend, not this pool alone —
      into the backend's ConfigMap, and converges the ledger: `PUT` only where the observed
      `requested_quota_bytes` differs, `DELETE` for an entry no Binding of any pool on that master
      claims, no write at all in a steady state, asserted by a call-counting fake. **An entry another
      pool's Binding owns is never deleted.** The ConfigMap is the desired state and is re-rendered
      every pass, so the master rewriting its own copy of the file (which it does on every `PUT`)
      converges rather than fights. A `409 UNAVAILABLE_IN_CURRENT_MODE` writes nothing and sets
      `MultiTenancyDisabled`; a `PUT` the master cannot persist because its policy source is not
      writable sets `QuotaPolicyNotWritable`; both hold the pool away from Ready (criterion 11).
      `allocatable_capacity_bytes = 0` sets the zero-members Condition on the pool and holds it away
      from Ready.
      **The delete rule is narrower than "no Binding claims it", and deliberately.** The ledger
      carries no label saying whose an entry is, and an EXTERNAL backend may well be serving tenants
      nobody in this cluster created — so a pass that deleted everything it was not asked for would
      destroy somebody else's quota with no trace of who did it. An entry is removed only when this
      operator itself REGISTERED it, which it reads from the `status.domains` the pools on that master
      published. T8 therefore writes the declared half of that registry (name, binding, blockSize,
      dtype); T9 adds the observed half. A first pass against an unknown ledger deletes nothing, which
      is correct: it has registered nothing yet.
      **The two faults are REASONS, not condition types.** Every condition in this repository is
      spelled positively and False carries the fault, so `MultiTenancyDisabled` and
      `QuotaPolicyNotWritable` are the reasons on `QuotaLedgerAvailable` and `QuotaPolicyWritable`. A
      type that is True when something is wrong reads backwards from every other condition on the
      cluster. Criterion 11 and the e2e case assert the reason.
      `QuotaPolicyNotWritable` needs a distinguishable answer to key on, so this task also adds
      `ErrQuotaPolicyNotWritable` to T4's client: the master answers `-1503` / 500 `PERSISTENT_FAIL`
      when it accepted a change and could not write it down, and 500 is also what
      `ErrorCodeToHttpStatus` gives everything it has no case for — so the code in the body is what
      separates them.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T8.5 · The writable policy volume the master needs to start at all**
      Blocked by: T2, T8
      Owns: `pkg/worker/kvcache/mooncake/leader_flags.go`, `pkg/worker/kvcache/mooncake/leader_workload.go`
      Gate: review
      Acceptance: rendering a backend with `multiTenancy` on produces a leader Deployment that carries
      `--tenant_quota_connector_uri` pointing at a path inside a writable `emptyDir`, an initContainer
      seeding that path, and the ConfigMap T8 renders mounted `optional` for the initContainer to read
      — and rendering one with `multiTenancy` off produces none of them, asserted field by field so
      the switch cannot half-apply.
      **This task exists because T2 shipped a switch that, alone, is a crash.** The master's
      constructor builds the quota policy store when multi-tenancy is on, the file connector refuses
      the empty uri that is the flag's default, and the constructor rethrows rather than degrading:
      `multiTenancy: true` without this task is CrashLoopBackOff on every such backend, deterministic
      rather than probabilistic. Nothing has caught it because no e2e has yet turned the switch on.
      The shape is forced, not chosen; each of the three obvious shortcuts is refused by a different
      line of upstream:
      - **Not a direct ConfigMap mount.** kubelet mounts it read-only, and the saver writes a sibling
        temp file in the same directory and renames it over the target. Every admin write would answer
        `PERSISTENT_FAIL`, which is the very fault T8's `QuotaPolicyWritable` reports — so the whole
        ledger would converge to permanently broken while every unit test stayed green.
      - **Not a bare `emptyDir`.** The loader treats an unopenable file as an error and the
        constructor rethrows it, so an unseeded volume is the same crash by another route.
      - **Not an empty file either.** The parser requires a YAML map with `version: 1` and a `tenants`
        sequence; nothing less parses.
      `--tenant_quota_connector_type` is NOT rendered: `file` is already the artifact's default, and
      this renderer's standing rule is to emit no flag sitting at its own default so an upstream
      default that moves surfaces as a behaviour change rather than as a value silently re-asserted.
      The ConfigMap is `optional` and the initContainer falls back to writing the empty policy,
      because the ConfigMap is written by the POOL reconciler and a `multiTenancy` backend that no
      pool has bound yet would otherwise wait forever for a volume nobody is going to create. The
      fallback document is the renderer's own empty-tenant-set output, not a second literal.
      The initContainer runs the leader's own image rather than a copy utility, so an air-gapped
      install pulls nothing extra; that it can run a shell is asserted by T11 on a real image, not
      here.
      Verify: `go test ./pkg/worker/kvcache/...`

- [x] **T9 · Binding reconcile: requested, effective, usage, overQuota — one tenant each**
      Blocked by: T8
      Owns: `pkg/worker/controllers/worker/kv_cache_pool.go`
      Gate: review
      Acceptance: one scrape per pass feeds every Binding of the pool (a fake asserts the scrape count
      does not grow with the Binding count); each Binding's four figures are read from the single
      series for its own `spec.domain.name`, with **no summation on any path**; `spec.quotaCeiling` is
      written verbatim as that tenant's `requested_quota_bytes`, with no division anywhere; `usage`
      excludes reserved bytes; `blocks` and `hitRate` are written when observed and left absent when
      not; a Binding whose tenant is missing from the document reports no figures and says so rather
      than reporting zero;
      the F9 race backstop holds — a domain the pass nevertheless finds claimed twice is managed for
      neither Binding, with `DomainClaimedByMultipleBindings` naming the other claimant on both; a
      failed scrape leaves the previous figures in place with a Condition instead of zeroing them.
      **Four decisions this task settled, none of them free choices.**
      **`usage` excluding reserved bytes is 0.3.12.post1's property, not the artifact's.** 0.3.13
      keeps ONE occupancy figure charged from `PutStart` (*The tenant surface changed shape in
      0.3.13*), so on that build there is nothing to subtract. Both land in `usage` — a Binding on the
      newer master must not silently report none — and the newer one carries the caveat in its
      Condition rather than in a doc note nobody reads next to the number. The two generations are
      resolved in ONE place, `TenantSample.Occupancy`, so nothing above it knows which build answered.
      **`blocks` and `hitRate` have no source on either build.** No per-tenant count series exists,
      and hit rate is per-master. Both stay absent, which is what F-observed-not-declared already
      requires; a test asserts the absence so a later reader does not take it for a bug.
      **A missing tenant is a write that has not landed, and says so.** Every Binding carries a
      ceiling, so a domain the exposition does not mention is always an entry this operator meant to
      write and the master has not published — never a tenant deliberately running without one.
      **The clearing rule is the mirror of the keeping rule, and the pair only works if both hold.**
      A failed scrape KEEPS the previous figures, because nothing was learned. A successful scrape
      that no longer mentions the tenant CLEARS them, because something was — a quota the master has
      stopped holding may not stay on display. Both directions are asserted.
      The conditions are `DomainExclusive`, `QuotaObserved` and `QuotaGranted`, positive as
      everywhere else, with `DomainClaimedByMultipleBindings` as a reason on the first.
      ⛔ **`QuotaGranted` is separate from `QuotaObserved` because a grant of zero is a successful
      observation.** Summarizing the phase from observation alone made a Binding Ready whenever the
      master answered, whatever it answered — so a domain granted zero bytes, and a domain with no
      ledger entry at all, both reported Ready while no write could succeed. That was found by ④'s
      measurement rather than by a test, and it is asserted now by two cases that fail without the
      third axis.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T10 · `usedBy` at both levels, and the two finalizers**
      Blocked by: T9
      Owns: `pkg/worker/controllers/worker/kv_cache_pool.go`
      Gate: review
      Acceptance: the pool's `status.usedBy` lists its Bindings from the index and the Binding's lists
      the workloads in its own namespace — **no query in either direction crosses a namespace**;
      deleting a Binding whose `usedBy` is non-empty is held by the finalizer with a Condition naming
      the holder; once empty, the finalizer `DELETE`s the one tenant entry that Binding owned, drops
      it from the rendered policy, and releases; the pool's finalizer refuses while a Binding remains
      and otherwise deletes every entry its own Bindings created **and no other**; an interrupted
      finalizer leaves no orphan, because the next pass over that master deletes any ledger entry no
      Binding of any pool on it claims.
      **Three things this task settled, and the first changes who writes what.**
      **The Binding's `usedBy` is an INPUT here, not an output.** The kind that will appear in it,
      `ModelDeployment`, is defined by the model-deployment spec **in this repository** and has not
      been built yet, so this reconciler reads the list, refuses the release on it, and never writes
      it (*F12*). The pool's own list is written, from the index. Both are still single-scope; the
      second is structurally so, because its writer is already in the namespace. **The consequence is
      in F12 and is not a detail: until that spec lands, nothing writes the list, so the Binding's
      finalizer always releases and criterion 5's protection is not yet in effect.**
      **The pool claims its backend on the same pass** — the write criterion 13 reads back, and the
      only entry in this family carrying an empty-string namespace. The claim goes on as soon as the
      backend RESOLVES, before its address is asked for, because a backend that has published nothing
      yet is exactly the one somebody might delete while a pool waits for it; and it comes off AFTER
      the ledger entries and BEFORE this pool's own lock — after, because a backend released while an
      entry of this pool's is still on its master could be deleted with that entry on it; before,
      because a pool that released its own lock first would leave the claim behind with nothing left
      to run the removal.
      **Two absences are not a hold, and everything else is.** A backend that no longer exists took
      its ledger with it, and a pool that waited would be undeletable for as long as it stayed gone —
      which is the ordinary order a stack is torn down in. A pool that registered nothing has nothing
      to remove. An address not published, a master that refuses, a domain that still holds objects
      (the third meaning of 409, F-error-codes) all HOLD, because in each the entry may still be
      there. A Binding's release is gated on the same pass's ledger having converged, so it never
      comes off over an entry that is still on the master.
      **The pool's teardown RELEASES before it refuses, and that ordering is a deadlock fix rather
      than tidiness.** A Binding's lock comes off in the serving pass, and a pool marked for deletion
      never reaches one — so `kubectl delete` over a pool and its Bindings together, which is the
      ordinary way a stack goes, would leave the pool waiting on Bindings whose only release path it
      had itself stopped taking, forever. The teardown therefore lets go of every Binding nothing
      holds before it decides whether anything is left, and the accepted cost is that a pool held by
      a Binding stops refreshing that Binding's figures: it has been asked to go, and the Condition
      names every Binding standing in the way.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T11 · e2e: two namespaces, one pool, proportional effective quota, and the zero-members Condition**
      Blocked by: T5, T6, T8.5, T10
      Owns: `.claude/skills/gpustack-operator-e2e/cases/case-43.sh`
      Gate: a real master runs on the local cluster with an allocatable capacity small enough to
      oversubscribe. The companion backend spec left NO re-runnable case behind — its own e2e was a
      manual run recorded in its own text — so this case stands the backend up ITSELF rather than
      inheriting one from a suite that has none, and the cluster shape it asks for is the one that
      spec recorded: no GPU, no RDMA, no etcd.
      Acceptance: on a local cluster with no GPU and no RDMA — two `KVCachePoolBinding`s in two
      different namespaces, with **different `spec.domain.name`s**, referencing the **same**
      `KVCachePool` both reach Ready and the pool's `status.usedBy` lists both (criterion 1); both
      `requestedQuota` and `effectiveQuota` are visible on each, and with the sum of requests
      deliberately raised above the master's allocatable capacity, each `effectiveQuota` falls **in
      proportion to what that Binding requested** (criterion 2); a pool whose backend has no mounted
      members carries the Condition naming why every effective quota is 0, and is not Ready
      (criterion 4); deleting a Binding that still has a `usedBy` entry is refused (criterion 5); a
      third Binding reusing the first's `domain.name` is **rejected by `kubectl apply`**, with the
      holder named in the message (criterion 10); the backend's own `status.usedBy` — the one list
      that writes an **empty-string** namespace into a list map key — is read back after the pool
      claims it, carries the entry, and carries exactly one after a second identical write
      (criterion 13); and both backend preconditions are broken UNDER a bound pool, so the pool
      carries `MultiTenancyDisabled` and `QuotaPolicyNotWritable` respectively and reaches Ready in
      neither case (criterion 11).
      **Neither fault can be staged by starting the backend broken.** Admission refuses a pool whose
      backend runs without multi-tenancy, so a backend started that way never acquires the pool that
      would report the Condition — which is why F5 calls this a runtime observation rather than an
      admission one. The case therefore admits a healthy pool first and then turns multi-tenancy OFF
      on the backend, which nothing freezes. The read-only half is staged the same way and from
      inside: the policy file is replaced with a DIRECTORY of the same name in the leader, because
      the master writes through a temp file and a rename — a `chmod` does not survive that, and the
      master runs as a non-root user that cannot `chmod` the directory the file sits in. The teardown
      `rmdir`s explicitly and the leader's container must not restart while it stands, or the master
      fails to parse a directory as a policy document and crash-loops with no way back, the volume
      outliving the container.
      **Two of T8.5's assumptions are only checkable here, and the case asserts them rather than
      inheriting them from a green unit test.** That the leader's image can run the initContainer's
      shell at all: a rendered Deployment proves the field, only a real image proves the command, and
      a failure looks like `CrashLoopBackOff` on an initContainer nobody was watching. And that the
      master reaches Ready on a `multiTenancy` backend **before any pool binds to it** — the path
      where the ConfigMap does not exist and the fallback document is what the master loads. Assert
      the master Ready first, then create the pool; the reverse order passes without ever exercising
      the fallback.
      **Criterion 5's `usedBy` entry is written by the CASE**, with a `kubectl patch` onto the
      Binding's status subresource. The consumer that will write it in production is the
      model-deployment spec's `ModelDeployment`, which has not been built (*F12*), so no controller
      fills that list on this cluster — a case that waited for one would wait forever, and one that
      skipped the write would assert a release nothing was holding. When that spec lands, the patch
      is what this case keeps asserting the mechanism with: it exercises the finalizer without
      depending on another spec's controller being present.
      Verify: the case script's own verdict, through the suite's shared verdict path.

- [x] **T12 · e2e: quota actually rejects a write**
      Blocked by: T11
      Owns: `.claude/skills/gpustack-operator-e2e/cases/case-44.sh`
      Gate: CASE 43 passes, so a pool and two Bindings are known to converge before this case tries to
      push one of them over.
      **This is a case of its own because the behaviour was NOT demonstrated by the capability
      experiment** — `effective_quota_bytes = 0` did not block writes there, and nothing in this
      repository may assume it does.
      Acceptance (criterion 3, and F11 is what each half means): the grant is filled exactly; every
      object in it is read back, which puts it under a lease and so out of the master's reach; the
      **next** write is then refused with `TENANT_QUOTA_EXCEEDED`, and refused for the quota rather
      than for an unregistered tenant; every object that caused the refusal still reads back; the
      master's own `mooncake_tenant_quota_reject_total` records **exactly one** refusal, so the
      client's return code and the master's ledger are two independent witnesses to the same event;
      the charge is capped **at** the grant rather than tracking what was offered; and once the leases
      lapse, a further write is admitted while older objects of the same domain are gone. That last
      one is recorded, not endorsed — a master that stops behaving this way must fail the case rather
      than pass it quietly. `status.overQuota` is **not** asserted: F11 shows it cannot become true on
      this path, and an assertion that cannot fail is worse than none.
      **The writes come from the leader's own image**, which ships the store client: its `setup()`
      takes a `tenant_id` directly, so a reuse domain can be written to without an inference engine.
      The connection arguments are the ones a member uses — a `P2PHANDSHAKE` metadata server, `tcp`,
      and the leader Service on its RPC port — with a **zero segment size**, since this client
      contributes no storage, and a **non-zero local buffer**, which is a different argument and not
      optional: a zero there is not "use the default", and a client with no buffer cannot move a
      multi-megabyte object.
      ⚠️ **Removal must be retried past `OBJECT_HAS_LEASE` (-706).** The same read that makes the
      refusal above possible also makes the object undeletable until its lease expires, and
      `OBJECT_NOT_FOUND` (-704) counts as removed — several keys are *expected* to be gone by then.
      A case that takes one -706 as final leaves the domain holding objects, which makes the master
      refuse to drop its quota, which makes the Binding's finalizer hold correctly and forever.
      ⚠️ **Two refusals must not be confused, and they are easy to.** A write to a tenant the master
      holds no policy for is refused `TENANT_NOT_REGISTERED`, which is what a domain that was never
      declared also gets — *not* a quota verdict. This case's Binding therefore has to be one whose
      ceiling actually reached the ledger before any of it means anything, or a case that never wrote
      a policy would read its own misconfiguration as the enforcement it set out to prove.
      Verify: the case script's own verdict, through the suite's shared verdict path.

- [x] **T13 · The scale envelope: four measurements, with numbers**
      Blocked by: T11
      Owns: `specs/2026-08-28-kv-cache-pool.md` (the *Verification* table only)
      Gate: a master is running on the local cluster and can be driven to the object populations
      below without a GPU, an RDMA fabric or a cloud.
      Acceptance: the four rows are filled with measured numbers and the object populations they were
      measured at — resident memory per object, lookup throughput and p99, eviction-scan and recovery
      latency against object count, and ledger recovery time after the one master restarts — with ③
      and ④ split so each row carries one verdict; ④ measured on an ordinary leader restart and its
      figure stated as excluding what a snapshot fork would add; ③b recorded as out of reach with its
      reason, and ③c — snapshot cost, and what snapshotting on does to both halves of a restart —
      recorded as a **product gap** with the mechanism that makes it one. A row that could not be
      measured says so with the reason; an empty cell is an unmet acceptance, not an omission
      (criterion 9).
      ⛔ **③c and ④ are NOT required to come from one restart, and the earlier demand that they do
      was never satisfiable.** Snapshotting needs an environment variable the leader's rendered
      surface does not pass, which has nothing to do with the setting below — so the shared restart
      was unreachable from the moment it was written. It was protected by a true counterfactual (the
      next paragraph's "off, the run would lose the shared restart"), and a counterfactual **rules out
      one option without establishing any**: to establish that leaving multi-tenancy on suffices, one
      would have to name the line of code that turns snapshotting on, and there is none.
      ⛔ **All four are measured with `multiTenancy` ON and a ceiling far above the population's
      bytes** — the shape that actually ships. Turning multi-tenancy off would be the wrong fix for a
      real trap: a domain at its ceiling does not stop accepting writes, the master evicts that
      domain's own objects to make room (F11), so the object count pins at the grant and 10^5 is
      unreachable while the curve still looks ordinary. What avoids that is the CEILING, not the
      switch — 10^6 objects of 64 B is 64 MB, so a 4 Gi ceiling never charges near it. Off, the run
      would also lose ④ entirely (no ledger exists) and under-report ① by omitting the per-object
      accounting multi-tenancy adds. ⚠️ This argument establishes only that turning it **off** is
      wrong; it is **not** evidence that leaving it on makes anything else reachable, which is exactly
      how it came to shelter the shared restart above.
      Verify: the table in this spec carries numbers, and `make lint docs` is green.

- [x] **T14 · Documentation**
      Blocked by: T10, T12
      Owns: `docs/**`
      Gate: T12 has reported what the master actually does about `overQuota`, so the page states a
      measurement rather than an expectation.
      Acceptance: a reference page states what the two kinds are and why they are split by scope
      (naming the `ClusterQueue` : `LocalQueue` precedent); that a Binding is the provisioning point
      and there is no quota without one; that a Binding registers exactly one reuse domain, so
      workloads share KV by pointing at the same Binding and a second reuse boundary is a second
      Binding; that `spec.quotaCeiling` is that tenant's request and `status.effectiveQuota` is the
      grant; that every field of `spec.domain` is immutable and why; that a workload may not name its
      own domain, with the reason; that eviction is neither configured nor reported by either kind,
      and where it is reached instead; the quota policy file's
      handling, including why it is not a read-only mount; the zero-mounted-members Condition; and
      whatever T12 established about `overQuota`, stated as what was measured. The page is linked
      from the docs index and its Contents block is in sync.
      Verify: `make lint docs`

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/worker/kvcache/mooncake` is an **existing** package that already has an `httptest`-based fake master and
  recorded fixtures under `testdata/`, so this feature extends both rather than starting a second set:
  the fake grows the `/api/v1/tenant_quotas` routes and the measured rejection responses, and a
  recorded metrics document is added carrying the eight per-tenant series and the three global gauges.
  They are checked in because they are the only record of what the measured build actually returns.
- `pkg/worker/controllers/worker` needs a fake admin client that **counts calls**, so "one scrape per
  pass regardless of Binding count" and "no write in a steady state" are asserted rather than assumed.
  Its `TestMain` already configures the loopback client the reconcilers' settings reads need.
- `pkg/worker/webhooks/worker` needs pool and Binding fixtures in its existing builder style, plus a
  fake reader that can be told a backend exists with multi-tenancy on or off, and that can be
  pre-loaded with Bindings in other namespaces so the cluster-wide domain-name check has something to
  collide with.
- The e2e suite needs a master with a deliberately small allocatable capacity, so oversubscription is
  reachable without filling a real cache. Both new cases must route their verdict through the suite's
  shared verdict path; a case that decides its own verdict fails the suite's preflight.

#### Unit tests

Table-driven, one behaviour per case, comparing semantic equivalence rather than byte-identical
output where the shape allows it.

**Policy render and validate** (`pkg/worker/kvcache/mooncake`) — every "refuse" case is the point: an
accepted-but-wrong file is a master that will not start.

| Case | Input | Expected |
|---|---|---|
| `renders_version_unconditionally` | one valid tenant | output begins `version: 1`; omitting it is not reachable from any input |
| `renders_quota_in_bytes` | `1GB`-equivalent quantity | `quota: 1073741824`, the form a `PUT` itself rewrites the file into |
| `quota_overflows_int64` | `9223372036854775808` and `1e30` | refused — the measured values `Quantity.Value()` reports as MinInt64 and as `0`, both of which would otherwise read as "not positive" |
| `name_empty` | `""` | refused, field error names the tenant index |
| `name_leading_underscore` | `_bad` | refused |
| `name_duplicate` | two tenants, one name | refused; never last-wins |
| `name_control_character` | name with `\x01` | refused |
| `name_nul` | name with `\x00` | refused |
| `quota_zero` | `0` | refused — the master answers `Tenant quota must be positive` |
| `quota_negative` | `-1` | refused |
| `one_bad_entry_yields_no_file` | one valid tenant, one invalid | error and **no output**; never the valid entry alone |
| `empty_tenant_set` | no tenants | a valid file with `version: 1` and an empty list, not an error |
| `webhook_and_reconciler_same_verdict` | the same input through both entry points | identical verdict, asserted side by side |

**Admin client and metrics** (`pkg/worker/kvcache/mooncake`).

| Case | Condition | Expected |
|---|---|---|
| `measured_response_body` | the recorded 0.3.12.post1 body, in its `{"success","data"}` envelope | decodes; `charged_bytes` / `admission_closed` are neither required nor invented |
| `bare_snapshot_refused` | a snapshot with no envelope | malformed, never accepted — the shape the Notes first recorded must not come back |
| `delete_returns_no_data` | `{"success":true}` with `data` absent | accepted; a delete that removed nothing is not an error |
| `unknown_tenant_get` | `-704` / 404 | modelled as absent, not as failure |
| `multi_tenancy_off` | 409 carrying `UNAVAILABLE_IN_CURRENT_MODE` | a distinct typed error the reconciler keys F5 off |
| `tenant_not_empty_is_not_multi_tenancy` | 409 carrying `TENANT_NOT_EMPTY` | its own sentinel — the finalizer's DELETE answers this, and reading it as F5 would put a false Condition on the release path |
| `quota_not_positive` | `-600` / 400 | typed input error carrying the master's own message |
| `invalid_tenant_id` | `-600` / 400 on `_bad` and on empty | typed input error, distinguishable from the quota one |
| `put_is_idempotent` | `PUT` of the value already observed | the reconciler issues none; asserted at the caller |
| `metrics_absent_tenant` | tenant not in the document | absent, **never zero** |
| `metrics_global_gauges` | the three global series | parsed alongside the per-tenant ones from one document |
| `metrics_malformed_line` | a truncated exposition | error, not a partial sample set |

**Webhooks** (`pkg/worker/webhooks/worker`).

| Case | Object | Expected |
|---|---|---|
| `backends_zero` | `spec.backends: []` | refused |
| `backends_two` | two entries | refused, message names the single-ledger reason |
| `backends_mutated` | one name changed on update | refused (immutable) |
| `backend_absent` | names no existing backend | refused on create |
| `backend_multi_tenancy_off` | backend with the flag off | refused, message names the domain-collision consequence |
| `pool_quota_zero` | `0` | refused |
| `poolref_mutated` | `spec.poolRef.name` changed on update | refused (criterion 6) |
| `ceiling_above_pool` | ceiling > pool `quota.total` | refused |
| `ceiling_absent` | unset | accepted; the Binding manages no quota and says so |
| `domain_absent` | `spec.domain` unset | refused — the domain is required, not defaulted |
| `domain_name_mutated` | `domain.name` changed on update | refused (immutable, criterion 6) |
| `domain_blocksize_mutated` | `blockSize` changed on update | refused — a warm cache would be silently corrupted |
| `domain_dtype_mutated` | `dtype` changed on update | refused — same reason, asserted separately |
| `domain_claimed_by_another_binding` | a second Binding, any namespace, same `domain.name` | refused at admission; the message names the holder and says cross-namespace domain sharing is unsupported (criterion 10) |
| `domain_claim_released` | the holder is deleted, then the same name is applied | accepted — the refusal is a live claim, not a tombstone |
| `domain_blocksize_zero` | `blockSize: 0` | refused |
| `domain_dtype_malformed` | `FP8!` | refused on form |
| `policy_prevalidated_at_admission` | a domain name the master would reject | refused **by the webhook**, so it can never reach the file (criterion 8) |

**Reconcile** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `steady_state_no_writes` | observed ledger equals desired | zero `PUT`, zero `DELETE` |
| `one_scrape_many_bindings` | five Bindings on one pool | exactly one metrics request per pass |
| `binding_event_enqueues_pool` | a Binding is edited | one pool-keyed reconcile |
| `orphan_binding_pool_absent` | pool deleted, Binding remains | the pass still reaches it and releases the finalizer |
| `binding_reads_its_own_tenant` | a Binding whose domain is in the document | the four figures are that one series; no code path sums |
| `binding_tenant_absent` | the tenant is not in the document | figures absent with a Condition, **never zero** |
| `usage_excludes_reserved` | non-zero `reserved_bytes` | `usage` is `used_bytes` alone |
| `ceiling_written_verbatim` | `quotaCeiling: 20Ti` | one `PUT` of that value in bytes; no division anywhere |
| `ceiling_unset_no_policy` | no `quotaCeiling` | no `PUT`; a Condition says the tenant runs without an explicit policy |
| `shared_backend_union` | two pools on one backend | the rendered policy and the desired ledger cover both pools' domains |
| `foreign_ledger_entry_untouched` | an entry owned by a Binding of another pool on the same master | never `DELETE`d |
| `domain_claimed_twice_race` | two Bindings admitted concurrently onto one domain | managed for neither, both carry the Condition naming the other — the admission-race backstop, not the gate |
| `zero_allocatable` | `allocatable_capacity_bytes = 0` | Condition on the pool and every Binding; pool not Ready (criterion 4) |
| `multi_tenancy_off_at_runtime` | the master answers 409 | no policy written, `MultiTenancyDisabled`, previous status untouched, pool not Ready (criterion 11) |
| `policy_source_read_only` | a `PUT` the master cannot persist | `QuotaPolicyNotWritable`, pool not Ready — it does not read as a quota that will not converge (criterion 11) |
| `scrape_failure` | the endpoint is unreachable | previous figures retained with a Condition; **never zeroed** |
| `effective_below_requested` | requests exceed capacity | each Binding's effective is proportional to its own request |
| `finalizer_holds_on_usedby` | Binding with a workload in `usedBy` | deletion held, Condition names the holder (criterion 5) |
| `finalizer_releases_and_deletes` | `usedBy` emptied | the Binding's one tenant entry is `DELETE`d, then release |
| `unconverged_ledger_holds_release` | the master refuses the ledger on the pass a Binding is being released | the lock stays on — released over a live entry, the capacity becomes unreclaimable — and the next converging pass releases it |
| `orphan_ledger_entry` | an entry no Binding of any pool on that master claims | deleted by the next pass |
| `domain_registered_before_status` | a Binding released while the pool's `status.domains` has never named its domain | still registered, so the delete path can reach the entry a failed status write left invisible |
| `pool_finalizer_holds_on_bindings` | a Binding still references the pool | deletion held |
| `pool_and_bindings_deleted_together` | pool and every Binding deleted in one go | all released in ONE pass — the deadlock guard: a Binding's lock only comes off in a serving pass, which a deleting pool never reaches |
| `held_binding_does_not_strand_its_sibling` | same, with one Binding held by a workload | the unheld sibling is released anyway; the pool refuses and names only the held one |
| `pool_claims_its_backend` | a pool resolves its backend | one `usedBy` entry with an **empty-string** namespace; a settled pass rewrites neither it nor the pool's own list |
| `pool_releases_only_its_own` | two pools on one master, one deleted after its Binding was force-deleted | its own entry goes, the sibling's stays, and only its own claim leaves the backend |
| `pool_held_by_undrained_domain` | `DELETE` answers 409 `TENANT_NOT_EMPTY` | deletion held with that reason, never read as `MultiTenancyDisabled` |
| `pool_not_stranded_by_absent_backend` | the backend is deleted before the pool | released, because the ledger went with the master |

#### Integration tests

- The full loop against the `httptest` master and a fake client: create a pool and two Bindings in two
  namespaces with distinct domains, assert the rendered ConfigMap, the ledger writes, and both
  statuses; then delete one Binding and assert only its own entry is removed.
- The same loop with **two pools on one backend**: assert the rendered file and the desired ledger
  cover both, and that deleting one pool leaves the other's entries intact. This is the test that
  stands between a shared master and a half-erased ledger.
- Round-trip the rendered policy: render → parse with the same schema the master loads → assert
  `version` is present and every name satisfies the constraints. This is the test that stands between
  a bad render and a CrashLoopBackOff.
- Convergence after a simulated `PUT`-rewrite: the fake master rewrites its stored file into the
  normalized form, and the next pass must issue **no** write, proving the operator's desired state and
  the master's rewrite agree rather than oscillate.
- Concrete test names are added after the implementation PR merges.

#### e2e tests

Two new cases on a local cluster — **no GPU, no RDMA, no cloud** — routed through the suite's shared
verdict path.

**What a run against a real master established, and what each fact costs a case that ignores it.**
Every one of these was measured, and every one of them would otherwise have produced a case that
passes or fails for the wrong reason:

- **Judge recovery on the master's own capacity coming back, never on `status.phase` returning to
  Ready.** The status is refreshed on the resync, so a published `Error` LATCHES until the next one:
  an outage measured at 3–4s on the master presented as 15.2s on the object. That figure is the
  latch's natural width and therefore a LOWER bound — a reconcile delayed for any other reason makes
  it longer, so it may not be used as a timeout ceiling either. Asserting that an outage *happened*
  may use `phase=Error`, which the latch makes easier to catch, not harder.
- **A backend reporting Ready is not evidence that its configuration took.** The fault this feature's
  own reconcile fix removed presented as a healthy object: Ready, every condition True, while the
  master ran without the flag the edit had turned on. So a case may not treat `Ready` as its
  precondition being met — it asserts the product instead, and the products are legible: the flags
  the master process actually carries, the init container actually present on the pod, and the
  absence of the api server's refusal in the operator's log. Ready became true in both the broken and
  the fixed build; only those three told them apart.
- **A failing backend is four assertions, not one.** `phase=Error`; `status.capacity` **absent
  entirely** — not zero and not the previous value, which is the API's own promise that an unreadable
  capacity cannot impersonate an empty pool; the leader/capacity/members conditions False; and
  `phaseMessage` carrying the operator's own sentence, which is where the paradox actually shows
  (`the leader pod is ready but its health could not be read`). A case asserting only the capacity
  misses what happened.
- **Any assertion of the form "did not change" / "never appeared" needs a sampler that can prove it
  sampled.** Two failures during this spec's own verification looked exactly like the result being
  sought: a probe image without `curl` produced 90s of empty answers that read as a dead master, and
  a watch that died after one line read as a status that never moved. The second is the dangerous
  shape — the tool's failure looked like the hypothesis. Carry a sample count or a resourceVersion,
  and downgrade an unprovable negative to "not observed".
- **A sampling-rate argument assumes the observed value is instantaneous.** Where it is latched, the
  resync period is not the denominator but the numerator, and the window is at least one period wide.
- **`deploy.sh` installs; it does not upgrade.** Against an existing release its first attempt fails
  on the name, its retry clears the residue — uninstalling the release and its CRDs, taking every
  custom object with them — and its second attempt succeeds. The caller sees an ordinary successful
  deployment. A case that needs an old version and then a new one must upgrade the release in place.
- **The probe image ships no `curl`.** In-cluster HTTP checks use `python3`'s `urllib`.
- **Oversubscription has exactly one legal shape.** Admission refuses a Binding whose ceiling exceeds
  the pool's own, so the sum of ceilings — each within the pool — is what must exceed the master's
  allocatable capacity. Sizing one Binding above the pool total fails at apply, for a reason
  unrelated to what criterion 2 tests.
- **A backend with a pool bound to it never shows a quiescent ledger**, because this operator
  converges it every pass. Reading the ledger's unmanaged contents needs a backend no pool binds to.
- **`master_remount_segment_requests_total` does not survive a restart** and also counts a member's
  first mount, so it is neither an increment nor a signal that a leader restarted.
- **Making the policy source unwritable** — criterion 11's second half — is done by replacing the
  policy FILE with a directory of the same name inside the leader. A `chmod` does not hold, because
  the master writes through a temp file and a rename, and the master runs as a non-root user that
  cannot `chmod` the directory it sits in. The cleanup must `rmdir` explicitly, and the leader's
  container must not restart while the directory is in place: the master fails to parse a directory
  as a policy document and enters an unrecoverable crash loop, since the volume outlives the
  container.

- **CASE 43** — two namespaces, one pool: criteria 1, 2, 4, 5, 10 and 11, as T11 states them. The
  oversubscription half needs a master whose allocatable capacity is small enough to exceed with two
  ordinary ceilings; the case sizes the backend for that rather than writing terabytes. The two
  Bindings carry **different** domain names — a third one reusing a name is the criterion-10
  assertion — and the criterion-11 half deliberately misconfigures the backend twice, once with
  multi-tenancy off and once with the policy source read-only, to prove each precondition is checked
  rather than assumed.
- **CASE 44** — quota actually rejects a write: criterion 3, as T12 states it, with the three
  assertions kept separate (a new write is refused; an existing object still reads; an existing object
  can still be deleted and its bytes reclaimed). This case exists precisely because the capability
  experiment could **not** demonstrate the behaviour, so a green run is evidence and a red run is a
  finding — neither is a formality.

## Alternatives

- **One namespaced CRD carrying both concerns.** Rejected: pools must be shareable across namespaces,
  and a cross-namespace reference from a namespaced object is a Kubernetes anti-pattern. It would also
  make the pool's privileged backend reference something a namespace owner could write.
- **One cluster-scoped CRD with an in-spec list of authorized namespaces.** Rejected: authorization
  then lives inside an object only a cluster admin can edit, so delegating "grant this team access" is
  impossible without granting edit on the pool itself; and the quota figures a namespace needs would
  be buried in a list element nobody can RBAC or watch on its own.
- **Two specs, one CRD each.** Rejected, and this is the trade-off stated above: the requested→
  effective computation, the two-level `usedBy` back-fill and the finalizer span both objects, so the
  split would cut one reconcile loop at a spec boundary. The generator risk that motivated the split
  is handled by making T1 a task that lands nothing else.
- **A `spec.metadataStore` field with a Valkey or Redis block index of our own.** Rejected: the master
  already holds the object→segment map, so the field would induce a datastore dependency and a new
  subchart for a responsibility nothing has. It returns only if a decision later goes toward a genuine
  cross-backend control plane, which F13's measurements are the input to.
- **A per-medium quota block on the pool.** Rejected: media live on the backend's `members[].medium`,
  and the storage layer's quota is a single per-tenant scalar, so a per-medium ceiling could not be
  enforced through it at all. Shipping the field would ship a promise with no mechanism.
- **The etcd quota connector instead of a writable file.** A real option, and the one to revisit when
  the master becomes multi-replica: it removes the writable-mount requirement entirely and stores the
  policy at `mooncake-store/<cluster_id>/tenant_quota_policy`. Not taken for the first release because
  it adds a managed etcd — a stateful dependency and a subchart — to buy durability the reconcile loop
  already provides. Recorded as an Open Question rather than closed.
- **A read-only ConfigMap mount for the policy file.** Rejected on measurement: a `PUT` rewrites the
  file, so a read-only mount breaks the first successful quota write, and it breaks it at the
  filesystem where nobody is looking.
- **Binding `tenant_id` to the namespace instead of the reuse domain.** Rejected: it is
  administratively tidier and it makes the ledger and the cache disagree about what a tenant is. The
  domain is what the storage layer isolates on, so keying quota on the namespace leaves the ledger
  describing a boundary the cache does not have. It would also foreclose cross-namespace sharing
  permanently, where refusing a doubly-claimed domain merely defers it.
- **Several reuse domains per `KVCachePoolBinding`, with `spec.quotaCeiling` divided evenly across
  them** — the integer remainder to the lexicographically first, floored at 1 byte. Rejected: an even
  split is arbitrary and predictably wrong. A 72B domain and a 7B domain do not want equal shares,
  and the operator has nothing to weight them by, because nothing in this spec supplies a per-domain
  *request*. Removing the multiplicity removes the split: one Binding, one domain, one tenant, and
  the ceiling lands verbatim on `requested_quota_bytes`. Multi-domain reopens with the spec that owns
  workloads, which is what would supply the per-domain request to divide by (Open Question 4).
- **Letting a workload declare its own reuse domain.** Rejected on a security ground, not an
  aesthetic one: `tenant_id` **is** the domain, so a workload free to invent domain names mints an
  unlimited number of tenants, each with a quota ledger of its own, and escapes the namespace ceiling
  entirely. Recorded as a Non-Goal so nobody relaxes it later without seeing that cost.
- **A per-pool `spec.eviction` block** (`highWatermarkRatio`, `policy`), **and the `status.watermark`
  that was drafted beside it as the surviving observability half.** Both rejected, and the second one
  is the more instructive. The ratios are startup gflags on the master process and the measured HTTP
  surface has no eviction endpoint, so eviction is not runtime-mutable the way quota is; and one
  master can serve several pools, so a per-pool ratio is unimplementable for both of them the moment
  two pools share a backend. `status.watermark` looked like the half that escapes that argument, and
  it does not: the master exports **no** ratio, so `highRatio` had no read path at any version, and
  the counter it does export is master-global, so `evictedObjects` on a per-pool status charges a
  co-tenant pool's evictions to this one. The same unimplementability, one field further on. F1
  records the per-tenant series that would reopen the observability half on the **Binding**.
- **A length-1 `status.domains[]` on the Binding.** Considered as the API-compatible path to
  multi-domain, and not taken: it restates the four top-level figures in a second place that can
  disagree with the first, and it does not avoid the break it was meant to avoid, since multi-domain
  turns those four scalars into sums whether or not a list sits beside them. The pool's
  `status.domains[]` stays a genuine list — one entry per Binding.
- **Arbitrating a domain claimed by two Bindings — highest ceiling wins, or first writer wins.**
  Rejected: arbitration between namespaces is an explicit Non-Goal, and every rule available is
  arbitrary. The enforcement sits earlier still — the second claim is refused at admission (F9), so
  the user learns at `kubectl apply` rather than finding a Binding that silently declines to manage
  itself.
- **Per-Binding scrape instead of one scrape per pool.** Rejected: it issues N reads of one document
  per resync, and the global gauges F10 needs are in that document exactly once anyway.
- **Dropping `spec.quotaCeiling`.** Considered seriously, and not taken: without it the operator has
  nothing to write into the ledger, every tenant runs without an explicit policy, and criterion 2 has
  no request to be proportional to. With one domain per Binding it is not an aggregate at all — it
  *is* that tenant's `requested_quota_bytes` — so the objection that motivated dropping it does not
  apply; what remains is that a request is not a grant, and the doc comment says so.

## Open Questions

1. **File connector on a writable volume, or the etcd connector?** This spec requires the **file
   connector on a writable `emptyDir` seeded from an operator-rendered ConfigMap**, for the reasons in
   F6, and the backend spec provides the mount. What would reopen it: a multi-replica or HA master,
   where the policy must be shared rather than per-Pod, and measurement ④ is what tells us whether we
   are heading there.
2. **Does `overQuota` mean reject-writes or evict?** The observed behaviour and the master's
   eviction watermark could contradict each other: one refuses new bytes, the other makes room for
   them. T12 answers what the master does. Which of the two the *object model* should promise is
   still open, and the answer changes what F11's doc text may claim — but the knob is now the backend
   spec's, so whatever is decided here is a statement about behaviour we observe, not one we set.
3. **What reconciles `status.domains[]`?** — **closed.** The domain is declared on `spec.domain` of
   the Binding, so the registry is one pass over the pool's Binding index and is authoritative rather
   than advisory: no watch on any workload kind, and nothing is learned from a workload announcing
   itself. F9 states the resulting rules.
4. **Should a `KVCachePoolBinding` be able to register more than one reuse domain?** Not in this
   spec: one Binding, one domain, one tenant, and `spec.quotaCeiling` lands verbatim on that tenant's
   `requested_quota_bytes` with no division rule to invent. What would reopen it is the spec that
   owns workloads, because that is what could supply a **per-domain request** — the thing an even
   split had to fabricate. Until such a request exists, a namespace needing two reuse boundaries
   creates two Bindings, which costs one object and invents no policy.
5. **When does cross-namespace sharing of one reuse domain come back?** This spec refuses it at
   admission, because two Bindings on one domain would share cache but collide on one quota ledger.
   The refusal is about the *ledger*, not about the sharing, so what would reopen it is a way to
   express one tenant with two granted namespaces — a co-owner list on the domain, or a
   quota-owner distinct from the grant holder. Neither is designed here, and G4's promise
   that the domain is the isolation unit is what keeps the door open.

## External references

- Mooncake multi-tenancy deployment (policy schema, connectors, admin API) —
  <https://kvcache-ai.github.io/Mooncake/deployment/multi-tenancy.html>
- Mooncake Store design, including the tenant-quota section —
  <https://kvcache-ai.github.io/Mooncake/design/mooncake-store.html#tenant-quota>
- Mooncake repository — <https://github.com/kvcache-ai/Mooncake>
- Eviction-scan measurement — <https://github.com/kvcache-ai/Mooncake/issues/2560>
- Large-scale metadata recovery RFC (still open) — <https://github.com/kvcache-ai/Mooncake/issues/3321>
- Kueue `ClusterQueue` / `LocalQueue` concepts, the scoping precedent —
  <https://kueue.sigs.k8s.io/docs/concepts/>

## Appendix: calibration log

This spec was drafted on **2026-08-28**, before the companion backend feature was implemented. That
feature merged on **2026-08-31**, and the draft's assumptions about it were then checked against the
shipped code rather than against the design that preceded it. The body above states only the
conclusions; this appendix records what changed and why, so a reader who saw the draft is not left
reconciling two versions from memory.

**The reuse-domain and quota model was not touched.** Everything below is a correction to what the
draft assumed about the *backend*, plus three shape decisions the backend's own implementation had
already paid for.

- **`status.watermark` is deleted.** The draft carried
  `watermark: { highRatio, evictedObjects }` marked *"OBSERVED from the master"*, and neither half
  survived being looked up. The master exports **no** eviction ratio — it is a startup gflag — so
  `highRatio` never had a read path. The counter it does export, `master_evicted_key_count`, is
  master-global and unlabelled, so on a per-pool status it would charge a co-tenant pool's evictions
  to this one. That is the same unimplementability the draft had already used to reject
  `spec.eviction`; it simply had not been carried one field further. F1 now records the per-tenant
  series that would reopen the observability half on the Binding.
- **The claim that "the backend spec sets them" is deleted.** The draft's *Proposal* table asserted
  that the eviction gflags are rendered by the backend. They are not: the backend's leader renderer
  emits no flag sitting at the artifact's own default, and its member rules record the tiering knobs
  as deliberately absent because reaching them is what its `extraArgs` escape hatch is for. Eviction
  is therefore not a cross-spec seam at all, and *Cross-spec seams* says so.
- **The pool publishes one address, not two.** The draft predated the backend's
  `status.endpoints[]`, which ships as a `+listType=map` keyed on `name` and enum-constrained to
  `Client` and `Admin`. The pool now echoes the `Client` entry as `status.clientEndpoint` and
  republishes the `Admin` entry nowhere; the draft's separate "metrics address" assertion is dropped,
  because `Admin` is one port serving the Prometheus exposition and the admin API both.
- **`metadataPlane` is not published.** The draft placed it on the pool's status. The backend does not
  expose it: the `P2PHANDSHAKE` literal is rendered onto members as an environment variable and is in
  no endpoint entry, so a pool publishing it would be emitting its own constant dressed as an
  observation.
- **Every observed quantity, and `spec.quotaCeiling`, became a pointer.** The status side is the
  backend's own lesson — `resource.Quantity` is a struct and `omitempty` does not omit a zero-valued
  one. `spec.quotaCeiling` is the harder case and was found separately: as an optional field the
  webhook requires to be positive when set, held by value it would make "unset" indistinguishable
  from "set to an illegal 0" and refuse the object outright.
- **The quota library lands beside the client that already reads that port**, not in a separate
  `pkg/worker/kvcachequota`. The admin API and the Prometheus exposition are the same port, so a
  second client would be a second body limit, a second error-excerpting rule and a second redirect
  policy for one connection. That code now sits in `pkg/worker/kvcache/mooncake` — see the split
  below, which moved the client too and kept the two together.
- **`pkg/worker/kvcache` was split, with the Mooncake-specific whole moved to a sub-package.** The
  parent had grown into the Mooncake implementation under a name that reads as a generic layer: the
  entrypoint it runs, the gflags it renders, the admin routes it dials, the metric series it parses
  and the policy schema it writes are each a fact about one store. Only two things survived in the
  parent — pull-policy resolution and the resource-note constants — because only those would be
  answered identically by a second implementation.
  **No interface, and no dispatch.** `spec.type` admits one value and nothing branches on it; an
  abstraction drawn against a single case gets drawn in the wrong place, and the second
  implementation then has to break it. The package boundary marks where a second one would be added.
  It is not a seam already built for it, and the package comments say so in as many words so nobody
  reads the empty parent as a plugin point.
- **`spec.leader.multiTenancy` was added to the merged `KVCacheBackend` (T2).** The shipped backend
  has no multi-tenancy switch, and this spec needs one it can validate against. Reading
  `extraArgs["enable_multi_tenants"]` from another CRD's webhook was rejected: it would judge an
  unschema'd string whose value domain belongs to whoever typed it.
- **T3, T4 and T7 now block on T2 rather than on T1**, which is what the plan's own prose already
  said — the two lanes open after the second barrier, not the first. T3 additionally shares a file
  with T2, so the edge is load-bearing rather than cosmetic.

### A measured fact that was recorded incomplete (found in T4)

The *Measured admin-API response body* section recorded the snapshot the experiment printed, but not
the `{"success", "data"}` envelope every handler wraps it in. The omission was invisible until
someone wrote a decoder to the section's literal text — which would then have failed against the very
build the section claims to describe.

Settled by reading `mooncake-store/src/master_admin_service.cpp` **at the `v0.3.12.post1` tag**, the
experiment's own build, rather than by re-running the experiment. Three things came out of that read,
and all three are now in the body above:

- the envelope, on all four handlers, with `data` optional on DELETE, and a second envelope shape on
  every refusal;
- that 409 is shared by `TENANT_NOT_EMPTY` — which is answered on the finalizer's own DELETE — so the
  status alone cannot mean F5's precondition;
- that `UNAVAILABLE_IN_CURRENT_MODE` is *also* what a not-yet-active service plane answers under 503,
  which is why the code alone cannot decide it either;
- that the two 400 refusals return one code and differ only by message, so separating them by message
  is the only thing available rather than a shortcut.

The nine-field snapshot itself, and the absence of `charged_bytes` / `admission_closed`, were
recorded correctly and are unchanged.

### Shape decisions taken while building T1

The API shape is the one part of this feature that cannot be revised after publication, so the
decisions taken while landing the types are recorded with the reason rather than left in a commit
message.

- **`status.overQuota` is `*bool`, not `bool`.** With `omitempty`, a value-held false omits itself —
  so the ordinary healthy reading, *observed and not over quota*, became the same bytes as *nobody
  could scrape this tenant*. Every quantity beside it was already a pointer for that reason; the bool
  had simply not been covered by a rule written about quantities.
- **`status.hitRate` carries a `pattern`.** A ratio is a string here, and the pattern is what makes
  that a guarantee rather than a convention. It is safe on this field in a way it would not be on an
  echoed vendor value — the ratio is computed by this operator — but it does oblige the writer,
  because a value failing it fails the whole status write and freezes every other figure.
- **`status.domains[].blockSize` and `dtype` are required.** They are copied from a Binding that
  already requires them, so an entry without them could only be a writer bug, and leaving them
  optional would let the registry answer "this domain's blocks are of unknown shape" — a state that
  does not exist.
- **One reference shape for every `usedBy`, and it names no API group.**
  `core.TypedLocalObjectReference` cannot carry the group in its list map key: `apiGroup` is optional
  with no default, and a structural schema requires every key to be required or defaulted. Keeping it
  would have left the list keyed on kind and name with a group field beside them that silently merges
  two objects differing only by group. `KVCacheObjectReference` drops the group and states the
  constraint that replaces it — a `usedBy` entry may only name a kind in this API group — and keys on
  all three of its fields. **`KVCacheBackend.status.usedBy` moves to it too (T2)**: that API is on
  `main` but in no release tag, its only reader uses `Kind` and `Name`, and two reference shapes
  differing by authorship is worse than one breaking change nothing has consumed.
