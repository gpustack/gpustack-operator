# KV Cache Pool

> **Purpose** — how a `KVCachePool` and a `KVCachePoolBinding` grant a namespace a quota on a KV cache,
> what that ceiling actually buys, and the two things operators get wrong: a full quota does not refuse
> writes but discards that namespace's own objects, and a Binding is not an isolation boundary.
> **Audience** operators, contributors · **Prerequisites** [KV Cache Backend](backend.md) ·
> **Read time** ~12 min

A `KVCacheBackend` runs a store. A `KVCachePool` publishes one, and a `KVCachePoolBinding` gives a
namespace a quota on it under one reuse domain.

⚠️ **A Binding provisions capacity; it does not enforce access.** See
[What a Binding does not do](#what-a-binding-does-not-do) before treating it as an isolation boundary.

Two vocabularies meet here, as on the backend page. This API says **reuse domain**; the store says
**tenant**, and every flag, metric and error keeps the vendor's spelling.

## Contents

- [Two kinds, split by scope](#two-kinds-split-by-scope)
- [The Binding is where capacity is granted](#the-binding-is-where-capacity-is-granted)
- [One Binding, one reuse domain](#one-binding-one-reuse-domain)
- [The domain is immutable](#the-domain-is-immutable)
- [The ceiling is a request, the grant is the answer](#the-ceiling-is-a-request-the-grant-is-the-answer)
- [What a full quota actually does](#what-a-full-quota-actually-does)
- [The quota policy file](#the-quota-policy-file)
- [When a pool grants zero](#when-a-pool-grants-zero)
- [Operating notes](#operating-notes)

## Two kinds, split by scope

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool                      # cluster-scoped, short name kvcp
metadata:
  name: shared-dram
spec:
  backends: [mooncake-dram]            # exactly one
  quota:
    total: 900Gi
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding               # namespaced, short name kvcpb
metadata:
  name: team-a
  namespace: team-a
spec:
  poolRef: {name: shared-dram}
  quotaCeiling: 600Gi                  # required
  domain:                              # required, exactly one, every field immutable
    name: qwen-72b-v2
    blockSize: 64
    dtype: fp8
```

The split follows the one this operator already uses for scheduling: **`ClusterQueue` : `LocalQueue`**.
A cluster-scoped object owns the capacity and an administrator manages it; a namespaced object is how
a namespace draws on that capacity, and it is the object RBAC can be written against.

`spec.backends` is validated as **length exactly 1**. Quota lands on a single store's per-tenant
ledger, and one store cannot account for bytes held in another — so a pool spanning two backends
could not answer the question the pool exists to answer.

## The Binding is where capacity is granted

**No Binding in a namespace, no quota on the pool for that namespace.** An administrator creating the
Binding is the act that provisions the two things a namespace needs — a ceiling and a registered reuse
domain — which is why it is a separate object rather than a field on the pool or a name a workload
types.

A workload reaches the cache by sending the **reuse domain name** its Binding registered — that string
is the store's tenant id, and it is what the store keys the cache on. Two workloads sending the same
domain share one cache and one ledger entry; two sending different domains do not. Nothing at runtime
takes a Binding *name*; the Binding is what put that domain on the store and what accounts for it.

> **Why** — a pool name a workload could type would make its quota a spelling question. Here it is an
> object an admin has to create in that namespace, so it is RBAC-able on its own.

### What a Binding does not do

⚠️ **It is not an isolation boundary, and nothing here enforces one.** The store accepts whatever
tenant id a caller sends, over a Service any pod in the cluster can dial, and no credential is derived
from this object. A workload that knows another namespace's domain name **can read and write that
domain's cache today** — Binding or no Binding.

What a Binding governs is who is *granted* capacity and under which name: provisioning and accounting.
Real enforcement needs an authenticated proxy or network isolation between workloads and the store,
and neither exists yet. Do not place two mutually distrusting tenants on one backend and treat their
separate Bindings as the thing keeping them apart.

## One Binding, one reuse domain

`spec.domain` declares exactly one reuse domain, and the domain **is** the store's tenant id. That
identity is what makes the rest follow:

- **A domain name is claimed cluster-wide.** A second Binding naming a domain another Binding already
  holds is **rejected at admission**, with a message naming the holder — anywhere in the cluster, not
  just in that namespace.
- **A workload may not *register* its own domain.** It necessarily sends a domain name at runtime —
  that is how the store is addressed — but the name has to be one an admin already registered through
  a Binding. Every distinct registered name is a new tenant with its own ceiling, so a workload free
  to mint them would draw a fresh ceiling for each. Registration stays on the object an admin
  controls; sending an unregistered name gets `TENANT_NOT_REGISTERED`, not a new quota.
- **Sharing a pool works; sharing a domain does not.** Two namespaces on one pool is the ordinary
  case. Two Bindings on one domain would share cache — sometimes the intent — but collide on one
  ledger, which never is.

`status.domains` on the pool lists the domains its Bindings registered, and
`DomainExclusive` on the Binding reports whether this Binding still holds its own name, and
`QuotaGranted` whether the grant it observed can serve a write at all.

## The domain is immutable

Every field of `spec.domain` is rejected on update, and so is `spec.poolRef`:

| Field | Why a change is refused |
|---|---|
| `domain.name` | the tenant id; changing it abandons the ledger entry and its bytes under the old name |
| `domain.blockSize` | a warm cache is read back at the block size it was written at |
| `domain.dtype` | same, one level down — reading fp8 blocks as bf16 is silent tensor corruption |
| `poolRef` | re-pointing moves a namespace's grant without moving its bytes, which stay on the old store |

To change any of them, delete the Binding and create a new one. That is the honest cost: the cache
under the old domain is not carried over, and pretending otherwise is what the refusal prevents.

## The ceiling is a request, the grant is the answer

`spec.quotaCeiling` is what this namespace **asks for**. `status.effectiveQuota` is what the pool
**granted**, and the two differ whenever the pool is oversubscribed.

- When every ceiling fits inside the pool's allocatable capacity, the grant equals the ceiling.
- When the ceilings sum past it, the store recomputes each grant **in proportion to what was
  requested** — a domain asking for twice as much gets twice the share of the shortfall's remainder.
- The reduction is computed by the store, not by this operator. The operator writes ceilings into the
  policy file and reads the resulting grants back.

`status.usage` is what the master reports the domain as holding, republished as read — the operator
caps nothing. What is bounded is the **store's charge**: it refuses a charge that would overshoot the
grant, discarding the domain's own objects instead, so usage normally settles *at* the grant rather
than above it. It genuinely exceeds the grant in the one case the next section describes — a grant
recut below what the domain already holds — and is reported that way.

> **Why a ceiling is required** — the store has no default quota. A tenant with no policy is refused
> `TENANT_NOT_REGISTERED` on every write, so a Binding without a ceiling would report Ready and be
> unusable. The field is required rather than defaulted because a guessed ceiling is a number nobody
> chose.

## What a full quota actually does

**A quota is not an admission barrier. It is the point at which the store starts discarding this
domain's own objects to make room for the next write.** Measured on a real store, not inferred:

- Writing eight 4 MiB objects into a 16 MiB grant produces **eight successful writes**, four
  surviving objects, and a charge of exactly 16 MiB. Nothing reports an error.
- The store's general eviction counters stay at **zero** throughout — this path is not on them — so a
  dashboard watching evictions sees nothing happen.
- A write **is** refused, with `TENANT_QUOTA_EXCEEDED`, when nothing can be discarded: reading an
  object puts it under a lease — about 10 s by default, set by a store flag — and a write while every
  object in a filled grant holds one fails.

⚠️ **`status.overQuota` does not report this, and cannot.** The store computes it as *charge exceeds
grant* while refusing any charge that would overshoot, so writing past a grant leaves it `false`
forever. It reports one situation: **the grant was recut below what the domain already holds**, which
is what a proportional recomputation does when a pool's members shrink or another Binding joins.

**So: do not wait on `overQuota` to learn that writes are being refused.** Watch `usage` against
`effectiveQuota` instead, and treat a domain sitting at its grant as one already discarding objects to
admit new ones — **and not in any predictable order**. The store scans from an arbitrary shard and
stops as soon as it has freed enough, so a recently written object can go before an older one, and a
hit rate cannot be reasoned about from age.

### Eviction is not configured or reported here

Neither kind has an eviction field, and neither reports an eviction figure.

Ratios are **process-level startup flags on the store**, and one backend may serve several pools — so
a per-pool setting is unimplementable rather than merely awkward. The counter the store does export
covers the **global** high-water eviction — not the per-domain discarding above, which is on no counter
at all — and it is process-global, so a per-pool figure would charge a co-tenant's evictions here.

Eviction is reached where it lives: the store's own process-level startup flags, which reach it
through the backend's `spec.connection.managed.leader.extraArgs` — see
[The leader](backend.md#the-leader) for how that container is assembled. The flag names are the
store's to document, and are deliberately not restated here.

## The quota policy file

The store reads tenant ceilings from a file, and this operator renders that file **whole** from the
Bindings of every pool on that backend. A partial write is never emitted: a refused render leaves the
previous file in place.

The file lives on a **writable** volume, and that is deliberate rather than an oversight:

- the store **rewrites it itself** on every admin-API change, renaming a new file over the old one;
- a read-only mount would make the store fail that rename, and it does not degrade — it reports the
  failure and the ledger stops accepting policy updates.

A `ConfigMap` is mounted read-only alongside it as a **seed**, copied into place by an init container
before the store starts. The pool reconciler renders the ConfigMap; a backend with multi-tenancy on
that no pool has bound yet has nobody to write one, so the mount is optional and the init container
then writes an **empty policy document** in its place.

⚠️ **The file itself is never optional** — the store is started with a flag naming it and fails
without it. What varies is only whether its contents came from a ConfigMap or from that empty
fallback.

`QuotaPolicyWritable` on the pool reports whether the operator can still write that file. False means
ceilings have stopped propagating, whatever the rest of the status says.

## When a pool grants zero

A pool whose backend has **nothing mounted** has nothing to allocate. Every domain's effective quota
is then zero and no write can succeed, so this is reported rather than left to look healthy:

```
CapacityAllocatable   False   NothingToAllocate
  the master reports nothing to allocate, so every reuse domain's effective quota is zero and no
  write can succeed. Its members have either not mounted their segments yet, or not finished
  remounting them after the master restarted
```

The pool does **not** report `Ready` in this state. A zero grant that looked healthy would send a
workload to a cache that refuses every byte it writes, with nothing in the status saying so.

**The condition names the reading, not a cause, because it cannot tell the two apart.** A first start
and a restart both present as a zero capacity gauge, and the gauge is all this check reads.

The restart case is the one that surprises. A restarted master answers its admin API in about two
seconds and passes its readiness probe there — the probe reads the segment list, not the ledger —
then reports a **zero** effective quota until its segments have remounted.

**How long depends on where the replacement Pod lands, and the slow case is the ordinary one.**
Measured over seven restarts with no exceptions: **2.8–4.4 s** when the master keeps its address or
returns to the node its member is on, and **about 32 seconds** when it changes address *and* lands
elsewhere — which is what a deleted Pod does in any deployment whose leader and member are on
separate nodes.

⚠️ **So expect the pool and every Binding on it to report `Error` for around half a minute after a
master restarts.** That is the conditions working, not a fault to chase: the phase clears itself and
nothing needs to be done to it. What it does mean is that a workload admitted in that window would
have every byte refused, which is why the Bindings stop reporting Ready rather than only the pool.

**Every Binding on that pool reports it too, on its own `QuotaGranted`.** It is a separate
condition from `QuotaObserved` because the master answers perfectly throughout — a grant of zero is a
successful observation, and a Binding that reported Ready on observation alone would send a workload
to a cache that refuses every byte, which is the reading this whole status exists to prevent.

The same shape covers the two backend preconditions. A store started without multi-tenancy has no
per-tenant ledger at all, and a policy file it cannot rewrite cannot receive ceilings; both surface as
a False condition and a non-Ready pool rather than as a pool that quietly grants nothing.

## Operating notes

**A Binding's deletion is held for two different reasons, and the condition says which.** Read the
reason on `Releasable=False` before acting — the two want opposite things:

- `HeldByWorkloads` — a workload in the namespace still references the Binding (it is in
  `status.usedBy`). The message names the **workloads**. Remove them; nothing needs draining.
- `LedgerNotReleased` — the store refuses to drop the tenant while its domain is non-empty. The
  message names the **domain**. Drain it — remove its objects — and the release completes in seconds.

They are separate because the action is: an operator who assumes the second goes looking for objects
to drain in a domain that may already be empty, while the thing actually holding the deletion is a
workload that was never removed.

**A pool is held while a Binding still references it**, and a backend while a pool still claims it.
Each layer names what to remove in its own condition message.

**Read the grant, not the ceiling, when diagnosing.** `kubectl get kvcpb` prints both:

```
NAME     POOL          DOMAIN        EFFECTIVE   USAGE    PHASE   AGE
team-a   shared-dram   qwen-72b-v2   450Gi       280Gi    Ready   6d
```

A grant well below the ceiling is oversubscription, which is legitimate and reported. A grant of zero
is the section above.

---

**See also** — [KV Cache Backend](backend.md) (the store this pool publishes, and where eviction is
configured) · [Admission](../architecture/admission.md) (the gates and the four-view status pattern) ·
[Settings & Environment Variables](../settings.md)

**Next** → [Accelerator Requests](../accelerator-requests.md) — how a workload asks for the devices it
runs on.
