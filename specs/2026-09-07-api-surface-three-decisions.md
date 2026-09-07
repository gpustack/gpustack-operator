# Spec: Three decisions on the accelerator CR API surface

Status: Shipped
Type: Design decision

Answers [gpustack/gpustack-operator#223](https://github.com/gpustack/gpustack-operator/issues/223).
Addresses [gpustack/gpustack-operator#203](https://github.com/gpustack/gpustack-operator/issues/203)
and [gpustack/gpustack-operator#205](https://github.com/gpustack/gpustack-operator/issues/205) — each
advances by one recorded decision and neither is finished, for reasons stated per issue below.

Measured against `8c6d9a80`, which is `origin/main` at the time of writing. Line citations are that
tree's.

## Summary

Three issues touch `api/worker/v1alpha1/`. They are in one spec because their change surface is the
same four files, and because the generated artifacts under `api/` are rewritten wholesale — two
independent passes of `make generate` would silently overwrite one another.

| issue | what its own closing conditions require | what this spec settles |
| --- | --- | --- |
| #223 | a decision on exposure and naming; it states that adding a field does not fill it | all of it: the flag is not exposed, and why |
| #203 | a decision on whether parallelism is an API field, then per-engine formulas, then a validator | step one only: it is a field, `roles[].parallelism`. The formulas and the validator are not here |
| #205 | a per-sentence rewrite of the comments in four files, after the program's fields stop moving | its timing blocker only: `roles[].acceleratorKey` is withdrawn, so the fields have stopped moving. The rewrite itself is not here |

REQUIRED reading before treating either of the last two as done: *What this spec does not deliver*,
at the end. Both remaining pieces are blocked on the same kind of missing input, and neither is
blocked on a decision.

## Decision: `-quota_bytes` is not exposed as a field

`-quota_bytes` is the store's whole-instance capacity. It stays unexposed, reachable through
`spec.leader.extraArgs`, and the reason is recorded in the API comments rather than only here.

### The criterion is already in the tree

This API already states when a store flag becomes a field, in `MultiTenancy`'s comment
(`api/worker/v1alpha1/kv_cache_backend.go:311-314`):

> It is a FIELD rather than an extraArgs entry because another API validates against it: a
> KVCachePool is refused when its backend has no ledger to write quota into.

And the same file already carries the converse as a shipped precedent. `AllocationStrategy`
(`:297-301`) enumerates only the two strategies any pooled store would have, and says the rest are
"reachable through ExtraArgs for anyone who needs them, and would otherwise fix this API to one
implementation's vocabulary".

`-quota_bytes` fails the field test. Nothing else in this API reads a backend's total capacity: the
per-namespace figure is `KVCachePoolBinding.spec.quotaCeiling`, and what constrains the sum of those
figures is the pool's own ceiling, not the store's. A field would be written, rendered, and read by
nobody.

### The path already exists, so exposure is not the question the issue makes it

The operator renders nine flags for the leader — `-rpc_port`, `-metrics_port`,
`-allocation_strategy`, `-enable_multi_tenants`, `-tenant_quota_connector_uri`, `-enable_offload`,
`-offload_on_evict`, `-pod_name`, `-pod_namespace` — and then appends `spec.leader.extraArgs`
verbatim as `-key=value` (`pkg/worker/kvcache/mooncake/leader_flags.go:61-108`). An administrator who
needs to cap a store's capacity can set it today, without this API growing a name for it.

Two properties of that path matter and are already documented on the field: a key colliding with a
rendered flag is refused at admission, and every value is world-readable on three paths.

### Why this is the only option that removes the ambiguity rather than winning an argument about it

The issue's constraint is that any name not saying "global" will be read as the per-tenant one, and
that the confusable neighbour is no longer hypothetical — `quotaCeiling` has landed
(`kv_cache_pool_binding.go:97`). Every option other than non-exposure has to defeat that
misreading with a name. Non-exposure is the only one where the misreading has nothing to attach to.

### The constraint the issue does not state, and it points the same way

The word is wrong before the qualifier is. On this store a tenant quota is not an admission barrier;
it is the trigger line for evicting that tenant's own older objects. Measured on a real master and
read in its source at v0.3.13: a failed charge calls `EvictTenantMemoryForQuota` and retries up to
three times, and `over_quota` on the write path is defined as `charged_bytes > effective_quota_bytes`
by a routine that refuses the overshooting increment rather than recording it — so it is false
whenever a write succeeds.

So a reader who sees any `quota` field expects "over the limit means refused, older data intact",
and both halves are wrong: writes succeed, and the older data is what was spent to make room. A
global-versus-per-tenant qualifier does not touch that. Adding a second `quota` name would double
the surface on which this misreading can happen.

### What this decision changes in the tree

One comment gains one sentence. `QuotaCeiling`'s comment
(`kv_cache_pool_binding.go:73-97`) is accurate on what it does say — it is a request rather than a
grant, and a tenant with no policy is refused outright — but it does not say what exceeding the
ceiling does. That sentence is owed to whoever reads `kubectl explain`, and it belongs with the
field rather than in this file.

REQUIRED before this is called done: the sentence states the mechanism, not just the outcome, since
stating the outcome alone is what leaves the next reader waiting for a refusal that never comes.

### What this decision does not do

- It does not remove `-quota_bytes` from reach. `extraArgs` renders it.
- It does not decide whether the store's capacity should be *derived* from the pool's ceiling. That
  is a separate question about who owns the total, and nothing in this issue asks it.
- It does not rename `quotaCeiling`. That name is shipped in an unreleased API and the misreading it
  invites is about the mechanism, which the comment can fix; a rename cannot.

## Decision: parallelism is a field, `roles[].parallelism`

The issue lists three steps in order and states it cannot be closed by adding a validator alone.
Step one — field or derived — is decided here: **field**, per role, `*int32`, minimum 1, unset
rendering nothing.

Both premises were re-checked on `8c6d9a80` before the field was added, and both held:

| premise | how it read |
| --- | --- |
| no parallelism field on any CR | `Parallelism`, `TensorParallel`, `parallelism` all matched zero files under `api/worker/v1alpha1/`, against a positive control (`Engine`) that matched three |
| nothing validates the port window | the only match for `Ports` under `pkg/worker/webhooks/` was `instance.go:181`, an immutability comparison, against a positive control showing `model_deployment.go` reads `Template` in eight places |

### Why not derived from the card count, restated because a derived version was proposed and withdrawn

`specs/2026-09-02-model-deployment-pd-atomic-admission.md:1661-1685` had already rejected deriving,
and the reason is the shape of the failure rather than the accuracy of the estimate: under pipeline
parallelism the card count is the product of two degrees, so a window computed from it is correct
exactly where it does not matter and too small where it does. Its own words are the ones to keep —
**a formula missing an input is worse than no formula, because it passes.**

The same entry supplies the argument that makes the field worth having independently of any
validator: the degree is **already being consumed**. SGLang divides the configured KV segment size by
it (`global_segment_size // tp_scale_factor`, v0.5.18 `mooncake_store.py:413-416`), and this
repository already records that downstream at `pkg/worker/kvcache/inject/sglang.go:79`. So the field
declares a quantity that changes behaviour today and had no name.

Per role rather than per deployment, because prefill and decode can run different degrees while
`spec.engineVersion` sits one level up.

### It takes the protobuf number the withdrawn field vacated

`roles[].acceleratorKey` was field 9 and the highest in `ModelDeploymentRole`; `parallelism` is field
9. Numbers stay contiguous 1..9 with no reserved gap, which is this repository's rule for a
pre-release API: `go-to-protobuf` regenerates `generated.proto` without any `reserved` declaration,
so a gap could not be enforced durably anyway, and no client is pinned to the old wire numbers.

## Decision: `roles[].acceleratorKey` is withdrawn, which unblocks #205's timing

The issue's "When" section is part of its closing conditions: the rewrite happens after the
program's fields stop moving, and it named two things still in motion. They were one causal chain,
not two conditions — per-role queues landing
([gpustack/gpustack-operator#199](https://github.com/gpustack/gpustack-operator/issues/199), open) is
what would have left `acceleratorKey` without a reader.

The field is removed instead, ahead of that. What went with it: the field and its 33-line comment,
the admission rule that resolved a key against its pool's flavors, the two helpers that rule needed,
the `nodeSelector` entry the renderer wrote, four tests, one e2e row, and the handler's `Client` and
`APIReader` — that rule was the only one that read the cluster, so the webhook now holds no client at
all. `specs/2026-09-02-model-deployment-pd-atomic-admission.md:499-508` is updated from open to
decided.

### Removing it was permitted, and the check is not the one the old comment claimed

The field's own comment asserted "absent from all 37" tags. That number is wrong as a denominator:
of the 37 local tags, **8 are local backup refs** (`pre-rebase`, `s1-backup`, `ship-safety-net` and
so on). The real figure is **29 release tags, none of which carries
`api/worker/v1alpha1/model_deployment.go`** — verified per tag with `git cat-file -e`, and with
`comm` confirming that no remote tag was outside the set checked. The conclusion held; the
denominator did not.

Readers were enumerated by the compiler rather than by grep, which matters because
`pkg/nodefeature/helper.go` and `pkg/kubemetrics/accelerator.go` carry symbols of the **same name and
a different type** that must not be touched.

### FORBIDDEN: leaving the refusal recommending the withdrawn field

The one-instanceType refusal used to end by telling the user to set `roles[0].acceleratorKey`
instead, and that was deliberate — its comment said so. With the field gone that sentence would
recommend something that does not exist, so the message now names the gap:

> Putting roles on different accelerator models within one pool is not possible today; it is tracked
> at https://github.com/gpustack/gpustack-operator/issues/199

This is a functional regression, stated plainly because "withdrawing it is free" in the entry above
covered **compatibility only**. No stored object was stranded, and per-role model selection within
one pool now has no replacement until issue 199 lands. The gap is registered where the user meets it
— in the admission error — rather than only in an issue.

### The per-sentence test the rewrite runs on

The issue requires that every removed sentence be either restated compactly or demonstrably already
enforced, and that the check be per sentence. The operative question is not "is this history":

> **If this sentence is gone, does the next reader undo the rule?**

Three sentences from `kv_cache_pool.go` were run through it, and no two came out the same way — which
is why the check cannot be applied per field:

| sentence | recorded elsewhere | verdict |
| --- | --- | --- |
| `Dtype` is spelled to match its JSON name, because the generator records every mismatch as a checked-in API rule violation | `api/worker/zz_generated.openapi.violation_exceptions`, a checked-in file the generator writes | **delete** — a reader who renames it to `DType` is stopped by the generator, so the rule is enforced without the sentence |
| `core.TypedLocalObjectReference` cannot be a list map key, because its `apiGroup` is optional and defaultless | `specs/2026-08-28-kv-cache-pool.md` and `specs/2026-08-28-kv-cache-backend.md` | **compress, do not delete** — the history is on record, but a reader who does not see it swaps these three fields for that type, and the spec carrying the reason is a different document they will not open |
| occupancy means committed bytes on one master shape and charged bytes on another, so do not read it as committed usage | `specs/2026-08-28-kv-cache-pool.md:1158,1176`, with the two shapes tabulated | **keep the warning, compress the mechanism** — the caution is addressed to whoever reads the field, and no schema or webhook can enforce "do not misread this" |

⇒ "It is recorded in a spec" settles whether the *history* is safe to drop. It does not settle
whether the *sentence* is, because the sentence's job is to reach a reader who is looking at the Go
type and nothing else.

### The modality markers are not being introduced — they are being unified

Measured on `8c6d9a80` with a matcher that takes only paragraph-opening runs of three or more
uppercase words, controlled against a sentence-internal emphasis (`a POINTER would...`) and an
acronym pair (`RDMA, HIP`), both of which it correctly declines:

| file | paragraph-opening markers | using one of the four canonical words |
| --- | --- | --- |
| `kv_cache_backend.go` | 8 | 0 |
| `model_deployment.go` | 9 | 0 |
| `kv_cache_pool_binding.go` | 4 | 0 |
| `kv_cache_pool.go` | 0 | 0 |

So the state the issue warns about — a convention in three files out of four — already exists, with
free-form uppercase sentences standing in for it. `kv_cache_pool.go` carries none of them while still
carrying constraints that want one: `Backends` admits exactly one entry and is immutable, and
`Quota.Total` is required.

FORBIDDEN, because it is the failure the issue names: introducing the four words in some of the four
files. A reader who does not find `LIMITED:` on a field concludes there is no limit.

### The issue's measurements are from an earlier tree, and the share has grown

Re-measured on `8c6d9a80`. Comment blocks are runs of `//` lines that contain at least one line
which is not a `+k8s` marker:

| file | lines | blocks | comment lines | share | longest |
| --- | --- | --- | --- | --- | --- |
| `kv_cache_backend.go` | 684 | 63 | 531 | 78% | 37 |
| `model_deployment.go` | 589 | 63 | 430 | 73% | 33 |
| `kv_cache_pool_binding.go` | 259 | 22 | 188 | 73% | 28 |
| `kv_cache_pool.go` | 260 | 25 | 168 | 65% | 25 |
| total | 1792 | | 1317 | 73% | |

Against the issue's table, taken at `1b71add1`: `kv_cache_backend.go` grew from 604 lines and 74% to
684 and 78%, and its longest block from 33 to 37. The longest block in the four files is no longer
`acceleratorKey`'s, which is what the issue names. The share did not drift down while the issue
waited; it drifted up, and it moved to a different file.

## What this spec does not deliver

Two pieces are outstanding, and both are blocked on a **missing fact** rather than on a decision.
Neither is a judgement call left open.

**The port window validator, and the owned-key half that goes with it.** Sizing the window needs the
per-engine formula, and the only one on record is Ascend's `[20000, 20000 + npu_per_node * 1000)` —
which is not even indexed by engine, since `spec.engine`'s enum is `vllm` and `sglang` and Ascend is
a transport. The entry that rejected deriving also predicted what shipping the writable half would
produce: **a validator that looks general and covers one vendor.** So nothing was written.

The same gap holds the owned-key step. `roles[].parallelism` is declared but the operator does not
render an engine argument from it, because that needs each engine's own argument spelling, and this
table is paired with the renderer by an invariant test — an entry added here without a matching
render is caught, correctly, as a lie. Until that lands the field and `roles[].extraArgs` can
disagree with nothing refusing it, which the field's comment states as a LIMITED rather than
implying a refusal that does not exist.

**The comment rewrite itself.** Its timing blocker is gone and its method is settled above — the
per-sentence test, and the fact that the four modality words are being unified rather than
introduced. What is not done is the 1317 lines.

## What none of the three is settled by

- Adding a `quotaBytes` field of any spelling. #223 states this, and the constraint it names comes
  from a neighbour already in the tree, so a field added before the naming question inherits the
  ambiguity.
- A validator for the port window with parallelism defaulted to the card count. Rejected on record,
  and the reason is that it passes where it matters.
- Shortening the longest comment blocks. The share is 65-78% across all four files; the long blocks
  are the symptom.
- Introducing the uppercase modality markers in some of the four files. A convention present in three
  files reads, in the fourth, as an absence of constraint.
- Any rewrite that drops a sentence without either restating it compactly or demonstrating that
  schema validation or a webhook already enforces it. That demonstration is per sentence.
- Declaring `roles[].parallelism` and calling the port window checked. The field is an input the
  check will need; it is not the check.
