# Spec: KV Cache Injection — Make the Pool Consumable by Any Workload

Status: Shipped
Type: Feature

## Summary

A mutating admission webhook on Pods. Any Pod that opts in with the label `kvcache.gpustack.ai/inject:
"true"` gets its KV cache connector configuration injected — environment variables, engine arguments,
and a projected client-config file where the engine needs one — resolved from a `KVCachePoolBinding`
in the Pod's own namespace.

**It does not inject a reuse identity, and that is a measured constraint rather than a choice.** The
`tenant_id` that would carry one is unreachable from outside the engine process at the versions this
project targets: every engine writes its own Mooncake config reader, none of them carries a
`tenant_id` key, and each calls the client's `setup()` through its **positional** overload, which
truncates before that parameter. The capability exists in the client — a second `setup()` overload
takes a dict and forwards every key — so this is a caller-side gap with a named upstream fix, not a
missing feature downstream. F4a **records that gap on the Pod** instead of refusing: every Binding
declares a domain, so a refusal keyed on one would reject every Pod this webhook was ever asked about,
and the refusal that does match the harm belongs on the Binding's own admission (D8, not landed).

**One cluster-level prerequisite follows from that gap, and it is documented rather than enforced.**
Because no engine sends a tenant, the client falls back to the store's own default — the literal
`default` — and a multi-tenant master, the only kind a `KVCachePool` accepts, refuses writes from a
name absent from its ledger. So the cluster needs a `KVCachePoolBinding` whose reuse domain is
`default`; without one an injected Pod starts, stays Ready, and fails every write. It is not checked at
admission because the ledger is a reconciler's product and a Pod may legitimately arrive first. The
Pod's own stamp carries the name its writes land on, so the fact is readable where it is needed.

This is the change that turns the three KV cache CRDs from a latent asset into a shipped capability.
Nothing about `KVCacheBackend`, `KVCachePool` or `KVCachePoolBinding` assumes this project's own
workload CR, and this one webhook is what makes that independence usable: **the pool becomes consumable
by a plain `Deployment`, a `LeaderWorkerSet`, a Dynamo graph, a kthena `ModelServing`, or anything else,
with zero workload lock-in.** It is roughly an order of magnitude less work than a workload CR and does
not depend on one, so it ships **before** the workload line and is not blocked by it — even if the
workload work slips entirely, the pool is already useful. The precedent is real: LMCache's operator
injects its cache configuration into engine Pods by webhook rather than requiring its own workload CR,
and its cache CR stays unaware of prefill/decode roles, selecting the right configuration from a per-Pod
role annotation. That factoring is what this spec adopts.

The webhook **refuses rather than guesses**, in every place where a guess would produce a silently wrong
result: an undeclared engine, an ambiguous target container, a configuration key the user already set,
an unresolvable Binding, and a Pod supplying the injection record itself. The one case it deliberately
does **not** refuse is the reuse domain an engine cannot carry: that condition is true of every Pod, so
a refusal would deliver nothing at all, and the stamp reports it instead.

**A Binding under deletion counts as unresolvable, and the reason is that nothing else catches it.** A
`Get` returns a terminating Binding exactly like a live one, so the read cannot tell them apart, while
the pool reconciler is already withdrawing that domain from the master's ledger — after which every
write from the injected Pod fails with `TENANT_NOT_REGISTERED`, permanently. The usual argument for
admitting and letting the system converge does not apply in either direction: the domain is not coming
back, and the Binding's finalizer cannot hold the deletion for this consumer because a plain Pod never
appears in `status.usedBy`. Admitting here produces a Pod that is injected, stamped as injected, and
broken — which is the failure shape this whole document is organised against.

## Motivation

### Dependencies

This spec consumes three CRDs and one renderer package it does not own. Declaring them here is what
moves "can this be built today?" out of the reader's memory and into the document: every prerequisite
below is on `main`, and each row carries the command that distinguishes a *shipped implementation*
from a *design that only describes one*. A zero on any of these would mean the work is not selectable.

| # | Prerequisite | Where it lives | Discriminating command | Result |
|---|---|---|---|---|
| D1 | `KVCachePoolBinding` — the provisioning object and `spec.domain.name`, the reuse identity | `api/worker/v1alpha1/kv_cache_pool_binding.go` | `git grep -c KVCachePoolBinding origin/main -- api/worker/v1alpha1/kv_cache_pool_binding.go` | 19 |
| D2 | `KVCachePoolStatus.ClientEndpoint` — the address an engine connects to | `api/worker/v1alpha1/kv_cache_pool.go:114` | `git grep -c ClientEndpoint origin/main -- api/worker/v1alpha1/kv_cache_pool.go` | 2 |
| D3 | `QuotaLedgerAvailable` condition + `MultiTenancyDisabled` reason — F4's gate reads these | `pkg/worker/controllers/worker/kv_cache_pool.go:108,125` | `git grep -c KVCachePoolConditionQuotaLedgerAvailable origin/main -- pkg/worker/controllers/worker/kv_cache_pool.go` | 7 |
| D4 | `mooncake.MemberProtocol` — the API-spelling-to-artifact-spelling transport map this spec reuses rather than restates | `pkg/worker/kvcache/mooncake/member_workload.go:141` | `git grep -c 'func MemberProtocol' origin/main -- pkg/worker/kvcache/mooncake/member_workload.go` | 1 |
| D5 | The `P2PHANDSHAKE` metadata-plane literal and the `META_DATA` spelling trap | `pkg/worker/kvcache/mooncake/member_workload.go:66,69` | `git grep -c P2PHANDSHAKE origin/main -- pkg/worker/kvcache/mooncake/member_workload.go` | 1 |
| D6 | The existing `PodWebhook`, whose selector this spec must **not** widen and whose fields it must not touch | `pkg/worker/webhooks/worker/pod.go` | `git grep -c 'func (r \*PodWebhook) Default' origin/main -- pkg/worker/webhooks/worker/pod.go` | 1 |
| D7 | `pkg/worker/kvcache/inject/**` — this spec's own new package | — | `git grep -c . origin/main -- 'pkg/worker/kvcache/inject/*'` | **0 — created here** |
| D8 | **NOT LANDED.** A Binding-side refusal of a second *distinct* reuse domain against a backend whose engines cannot separate them — the gate F4a deliberately does not perform | `pkg/worker/webhooks/worker/kv_cache_pool_binding.go` (a follow-up, outside this spec's file surface) | `git grep -c KVCacheBackend origin/main -- pkg/worker/webhooks/worker/kv_cache_pool_binding.go` | **0 — the Binding's webhook does not resolve its backend at all today** |

D1–D6 are all present on `main` at `c052b8af`, this branch's base. D7 is zero by design: the synthesis
package is new, so nothing in this spec's own file surface collides with work in flight elsewhere.

**D8 is the one prerequisite this spec ships without, and states rather than assumes.** The existing
Binding webhook enforces the opposite shape — that a domain is claimed by no *other* Binding
(`validateKVCachePoolBindingDomainIsUnclaimed`) — and never resolves a Binding to its backend, which is
what the missing check needs. Until it lands, **a second reuse domain on a shared backend is accepted,
and the isolation gap is declared by this webhook's stamp rather than prevented by any gate.** The
scope must be the backend and not the pool: one backend may serve several pools
(`kv_cache_pool.go:59-60`), so two Bindings on different pools sharing a backend collide identically.

### Goals

- **G1 (primary)** A Pod owned by **anything** — a plain `Deployment`, a `LeaderWorkerSet`, a bare Pod,
  a third-party operator's object — can read and write the pool by carrying one label and a handful of
  annotations. No workload CR, no Kueue queue, no operator-owned owner reference.
- **G2** A reuse domain this project cannot actually deliver is **declared, never approximated**. The
  isolation the Binding's domain promises requires the engine to carry a `tenant_id`, which no targeted
  engine version does (F4). So the goal is not "the domain isolates" — it is that nobody can believe it
  does without being told otherwise: the Pod is injected and carries a stamp saying the domain is not
  enforced and why, instead of starting and quietly sharing one tenant with no record of it anywhere.
  The refusal that matches the causality is on creating a second domain the backend cannot separate, and
  it is a named prerequisite this spec ships without (D8) rather than a gate placed on the wrong actor.
- **G3** Every configuration the webhook cannot resolve **unambiguously** is a rejection with an
  actionable message, never a default and never a guess.
- **G4** The synthesis — the mapping from (engine, role, pool connection facts) to the env, args,
  volumes and mounts a container ends up with — is a pure function, unit-testable with no cluster and
  no engine.
- **G5** A Pod that does not opt in is **byte-identical** after admission. The webhook is invisible to
  every workload in the cluster that did not ask for it.
- **G6** Acceptance is measured as **"the Pod actually reads and writes the pool"**, never as "the
  environment variable is present".

### Non-Goals

- **No routing, no prefix affinity, no KV events.** Injection only. Which replica a request lands on is
  another spec's problem; a global pool's hit rate does not depend on affinity routing.
- **No workload CR, no replica management, and no part in this project's Kueue admission chain.** A Pod
  that opts in may be owned by anything at all, and this webhook neither reads nor writes any Kueue
  object, any `Instance`, or any container's `resources`. It is an admission webhook that injects
  configuration; it charges no quota and gates no capacity.
- **No engine-version detection heuristics.** If the engine is not declared, the Pod is refused. Sniffing
  an image name is a heuristic that mis-injects silently on a renamed or vendored image.
- **Not a substitute for the workload CR's own wiring.** A future workload CR may render the same
  configuration directly and skip this path; both must produce the same result, and this spec's Test
  Plan is where that equivalence gets pinned once the workload CR exists.
- **No `KVCacheBackend`, `KVCachePool` or `KVCachePoolBinding` type changes.** Those types are the
  companion spec's; this spec consumes them and states, in Open Questions, the two fields it needs from
  them that the sketched shapes do not yet carry.
- **No NetworkPolicy authorship.** The transfer engine's port behaviour is documented here (see
  Notes) so an administrator can write a correct one; the webhook does not write one.
- **No tenant isolation, at these engine versions — and no attempt to fake one.** This spec ships
  connectivity, not isolation. Every injected Pod reaches the pool under the client's `default` tenant,
  because no targeted engine forwards a `tenant_id` — **and only once a Binding has registered that
  name**, since a multi-tenant master refuses writes from a tenant absent from its ledger and a
  `KVCachePool` accepts no other kind of backend. That prerequisite is documented rather than enforced
  (F4a); without it an injected Pod starts, stays Ready, and fails every write. With it, a pool serving
  two domains has them sharing one tenant's cache, where one domain's write pressure evicts another's
  blocks with no metric moving (F4a). The webhook's response is to **inject and say so on the Pod** rather than to refuse the
  Pod, whose author caused none of this, or to inject a domain that would not take effect (F4). The
  refusal belongs on the Binding side and has not landed (D8); the upstream fix that would lift the gap
  entirely is named in Open Questions. Patching the engines is out of scope here.

## Proposal

A Pod opts in with a **label**, and configures the injection with **annotations**:

| kind | key | value | role |
|---|---|---|---|
| **label** | `kvcache.gpustack.ai/inject` | `"true"` | the `objectSelector` trigger; a fixed value, so no length problem |
| annotation | `kvcache.gpustack.ai/binding` | the `KVCachePoolBinding` name, in this Pod's namespace | the configuration input |
| annotation | `kvcache.gpustack.ai/engine` | `vllm` \| `sglang` \| `vllm-ascend` | selects the synthesis; required, never guessed |
| annotation | `kvcache.gpustack.ai/role` | `prefill` \| `decode` \| unset | prefill/decode role, when the caller has one |
| annotation | `kvcache.gpustack.ai/container` | container name (optional) | which container to inject into |

**There is deliberately no domain annotation.** The reuse domain comes from the Binding and only from
the Binding, because `tenant_id` *is* the reuse domain: every distinct domain is a storage tenant with
a quota ledger of its own, and the Binding is the object that registers one and carries its ceiling. A
namespace that needs two reuse boundaries gets two Bindings, which only someone with create rights on
Bindings can make. `kvcache.gpustack.ai/domain` is therefore a **refused** key, not an unrecognised
one, so a manifest written against the escape hatch fails loudly instead of being silently ignored.

That is a **provisioning and accounting** contract, not an isolation boundary, and the distinction is
stated because the shape invites the stronger reading. A container that sets `MOONCAKE_TENANT_ID`
itself keeps that value — the precedence rule below never overwrites a variable the workload declared
— so it can already name any domain some Binding has registered, anywhere in the cluster, and read
that cache. Two things it still cannot do, so the limit is not read as larger than it is: mint a new
tenant, since the multi-tenant master this webhook injects against refuses a name absent from its
ledger (`TENANT_NOT_REGISTERED`); or reach a pool its own namespace has no Binding for, since the
Binding is what supplies the master address. Tracked as #168.

The webhook resolves the Binding in the Pod's own namespace, follows it to the pool and the pool's
backend, verifies the pool is in a state where injection means what it says, renders the client
configuration, and writes it into exactly one container. Everything else about the Pod is left alone.

### User Stories

#### Story 1

As a platform user with a plain `Deployment` running vLLM, I want to add one label and two annotations
and have my Pods share the team's KV cache, so that I get cache reuse without adopting anybody's
workload CRD.

#### Story 2

As a platform user running a `LeaderWorkerSet`, I want the same three lines to work on my leader and
worker templates, so that "supported workload kinds" is not a list I have to be on.

#### Story 3

As a platform administrator, I want a Pod that names a Binding I have not created in its namespace to be
**rejected at admission** with a message naming the Binding and the namespace, so that a missing
grant is a create-time error rather than a silently cacheless deployment.

#### Story 4

As a platform administrator, I want a Pod pointing at a pool whose master is not running with
multi-tenancy on to be **rejected**, so that two teams' reuse domains can never quietly collide in one
another's cache.

#### Story 5

As a platform administrator who has given a team a Binding with its own reuse domain, I want a Pod
running an engine version that **cannot carry that domain** to be **rejected at admission**, naming the
engine version and the domain, so that I learn the isolation I granted is not deliverable at create
time — rather than discovering months later that every team has been sharing one tenant's cache while
each object in the cluster reported a domain of its own.

#### Story 6

As an engine operator who already passes `--kv-transfer-config` myself, I want the webhook to **refuse**
rather than merge, so that I never have to debug which of two conflicting connector configurations won.

#### Story 7

As a cluster operator, I want a Pod that does not carry the opt-in label to come back from the API
server byte-identical to what I submitted, so that adding this operator to a cluster changes nothing for
workloads that never asked for a cache.

### Core Features & Acceptance Criteria

#### F1 — The opt-in is a label; the configuration is annotations

`objectSelector` is a `LabelSelector`. **It cannot select on annotations.** So the trigger must be a
label, and the trigger label alone must carry no data — a label *value* is capped at 63 characters and
restricted to alphanumerics plus `-_.`, while an object name may be a DNS subdomain up to 253
characters. A Binding name in a label value would hit that cliff; a fixed-value label plus an annotation
for the name avoids it entirely.

- The selector is `kvcache.gpustack.ai/inject In ["true"]`, not `Exists`. The documented opt-out
  `kvcache.gpustack.ai/inject: "false"` then costs nothing: the API server never calls the webhook at
  all, so an opted-out Pod is not merely un-mutated but un-consulted.
- The annotation keys are exported constants in the webhook package, following the repo's existing
  pattern (`QueueEntranceLabelKey` in `pkg/worker/webhooks/worker/instance_type.go`, prefix
  `gpustack.ai/` from `pkg/systemname`).
- An annotation the webhook does not recognise under the `kvcache.gpustack.ai/` prefix is **refused**,
  not ignored: a typo in `kvcache.gpustack.ai/bindng` would otherwise leave the Pod with no Binding at
  all, and a typo in a key that does carry meaning would silently drop the setting it was meant to
  apply.
- The two annotations the webhook **writes** — `kvcache.gpustack.ai/client-config` (F5) and
  `kvcache.gpustack.ai/injected` (F7) — are reserved. A submitted Pod carrying either is refused: they
  are the record of what the webhook decided, and a user-supplied value would be a forged one.

#### F2 — A new webhook type, with its own selector and its own name prefix

The existing `PodWebhook` (`pkg/worker/webhooks/worker/pod.go`) already registers a validating and a
mutating webhook on Pods, with the generated markers:

```go
// +k8s:webhook-gen:mutating:group="",version="v1",resource="pods",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:objectSelector={"matchExpressions":[{"key":"kueue.x-k8s.io/queue-name","operator":"Exists"}]}
// +k8s:webhook-gen:mutating:namePrefix="gpustack-worker"
```

Its `objectSelector` requires the label `kueue.x-k8s.io/queue-name` to **exist**, so it fires only on
Pods entering this project's Kueue chain. This spec exists precisely to serve Pods that do **not** enter
that chain — a plain `Deployment` or an LWS-owned Pod carries no queue-name label. So the injection
cannot extend `PodWebhook`; it is a **new webhook type with its own selector**.

- New type `PodKVCacheWebhook` in `pkg/worker/webhooks/worker/pod_kv_cache.go`, registered in the
  hand-written `setups` list in `pkg/worker/webhooks/setup.go`, with markers generating a **mutating
  webhook only** — there is nothing to validate that the mutation does not already decide.
- **The `namePrefix` must differ from `"gpustack-worker"`.** The generator derives both the webhook name
  and its serving path from it: `wh.Name = "<mutate|validate>.<namePrefix>.<group>.<version>.<resource>"`
  (`gen/api/generator/cmd/webhook-gen/generators/helper.go`), which for a second Pod webhook under the
  same prefix would produce a second `mutate.gpustack-worker.core.v1.pod` — a duplicate name inside one
  `MutatingWebhookConfiguration`, which the API server rejects, and a second handler on an already-taken
  mux path. The prefix is `"gpustack-worker-kvcache"`, giving the name
  `mutate.gpustack-worker-kvcache.core.v1.pod` and the path
  `/mutate-gpustack-worker-kvcache-core-v1-pod`.
- **The configuration object name does not change.** `MergeConfigurations` folds every generated webhook
  into the single `gpustack-worker-mutation` object, whose name comes from `configurationPrefix` in
  `pkg/worker/webhooks/setup.go` — deliberately chosen to sort before
  `kueue-mutating-webhook-configuration`. The new webhook joins that object; it must not introduce a
  configuration object of its own, which would put an unordered second configuration into the chain.
- The two Pod mutating webhooks may both fire on one Pod (an `Instance`-rendered Pod that also opts into
  a cache). They mutate **disjoint** fields — the accelerator webhook only `resources`, this one only
  `env`, `args`, `volumes`, `volumeMounts` and the Pod's annotations — and neither reads a field the
  other writes: this one reads container names, `env`, `args` and `command`, none of which the
  accelerator webhook touches; that one reads `resources` and the queue label, neither of which this one
  touches. So their order is immaterial, and each keeping to its own fields is an invariant, asserted by
  a test that runs both over one Pod in both orders.
- **The order is nonetheless fixed and worth naming, so nobody has to guess it.** The generator emits
  the mutating entries sorted by Go type name, so `PodKVCacheWebhook` precedes `PodWebhook` and the API
  server calls this one first. That ordering is an output of the generator, not a requirement of either
  webhook, which is exactly why the both-orders test is the thing that guards it: a future rename would
  silently reverse the order, and only the test would notice that it does not matter.
- `reinvocationPolicy` is `Never` (the default, stated explicitly because it is load-bearing here). This
  webhook runs first, and `PodWebhook` mutates the object after it — the condition that triggers
  reinvocation. Under `IfNeeded`, `Default` would re-enter on a Pod this webhook had already injected,
  and the F6 conflict rule would reject it, because the webhook's own output looks exactly like a
  user-set key. `Never` plus the F7 stamp makes that impossible rather than merely unlikely.
- **`failurePolicy` is `Fail`.** It is an availability decision, not a copy-paste: with `Ignore`, an
  opted-in Pod starts **silently without a cache** while its owner believes the cache is active, and a
  cache that is silently absent is worse than a Pod that did not start. The blast radius of `Fail` is
  bounded by the selector — only Pods that explicitly opted in are affected while the worker's webhook
  endpoint is down; every other workload in the cluster is untouched, because the API server does not
  consult a webhook whose `objectSelector` does not match. The alternative is recorded in Alternatives.
- `sideEffects: None` is truthful and must stay so: the webhook only **reads** cluster state. It creates
  no ConfigMap, no Secret and no object of any kind — which is why F5's file is projected from the Pod's
  own annotation rather than from a ConfigMap the webhook would have to create and garbage-collect.

#### F3 — Resolution: the Binding, the pool, the domain

- `kvcache.gpustack.ai/binding` names a `KVCachePoolBinding` **in the Pod's own namespace**. A value
  containing `/` is refused: there is no cross-namespace form. The Binding is where a namespace's
  grant is provisioned, and the only object in this chain an administrator can RBAC — so resolving one
  from another namespace would let a Pod draw on a grant its own namespace was never given. It is not
  an enforcement point: nothing derives a credential from it, and a workload that knows another
  domain's name can still reach the store directly.
- A Binding that does not exist is refused with a message naming the Binding, the namespace and the
  annotation key. The read follows the repo's existing webhook pattern: the cached client first, then a
  direct `APIReader` read, so a cold cache is a retry rather than a rejection.
- The Binding resolves to its pool through `spec.poolRef`, and the pool to its backend. From those come
  the connection facts the client needs, and each has exactly one source:
  - `master_server_addr` is the pool's `status.clientEndpoint` — the address an engine connects to,
    echoed there from the backend's `Client` endpoint. The backend's `Admin` address is republished
    nowhere and is never read here: it is the write face of the quota ledger, which is the operator's
    business and not an engine's.
  - `metadata_server` is the constant `P2PHANDSHAKE`, not an address. The metadata plane this project
    ships is peer-to-peer, so there is no store to point at — the leader renders no metadata flag of
    any kind, and the member renders this same literal. The **key** it is written under depends on the
    vehicle: the JSON key `metadata_server` in a config file, or the environment variable
    `MOONCAKE_TE_META_DATA_SERVER`. Only the second carries a spelling trap — `META_DATA` has an
    underscore the readable `METADATA` does not, and the wrong one degrades the plane silently instead
    of erroring — so the environment vehicle asserts the key byte for byte, as the member renderer
    already does for the identical key on its own side.
  - `protocol` is the backend's `spec.transport.protocol`, resolved through the existing exported
    `mooncake.MemberProtocol` rather than a second mapping table of this spec's own. It is
    **backend-wide, not per-node**: one member group renders one DaemonSet, so a single Pod template
    covers every node the group selects and cannot carry a different transport per node.
  - the RDMA device filter — spelled **`device_name`** in the file the engines read, not
    `rdma_devices` — is written as the **empty string**, on any path including RDMA. Empty is what the
    client reads as "use every device found", which is the only correct value here: an RDMA device is
    named per host — `mlx5_0` on one, `erdma_0` on the next — so no single name is right for a whole
    pool's consumers. The documented value `auto-discovery` is not special-cased anywhere in the
    client; setting it produces a filter matching a device of that literal name, which no host has.
- The **reuse domain** is `spec.domain` on the resolved Binding — required there, immutable there, and
  singular there, so this webhook reads one field and has nothing to choose between and nothing to
  default. There is no fallback to the client library's own `tenant_id` default of `'default'`: that is
  the domain every unconfigured client in the cluster lands in, so falling back to it would produce
  exactly the cross-domain collision this spec exists to prevent. A Binding with no readable domain is
  a refusal, not a default.
- The domain name arrives **already validated**: the Binding's own webhook checks the name against the
  master's measured `tenant_id` rejection paths at Binding admission, which is the right place for it —
  the name is an admin's input there, and by the time a Pod references it the value is fixed and
  immutable. This webhook re-checks nothing and duplicates no rule. Its own failure mode is a Binding
  it cannot read, not a name it must judge.

#### F4 — Two ends of isolation, and only one of them is a gate here

Isolation needs **both ends**: a master holding a tenant ledger, and an engine that actually sends a
`tenant_id`. Either one missing produces the same outcome — every request lands in the default tenant.
The two ends are treated differently, and the difference is where the harm is introduced rather than
where it is suffered.

**A refusal belongs on the action that introduces the harm.** The engine end (F4a) is not something the
Pod's author did: they asked for a cache with an annotation, and the reason their isolation will not
take effect is an upstream engine's call site. Refusing that Pod would mean one namespace's Binding
makes another namespace's Pods fail to start — a cross-tenant denial of service whose victim did
nothing wrong and can fix nothing. So F4a **records** rather than refuses, and the refusal that matches
the causality belongs on creating a second reuse domain against a backend that cannot separate them.
That check is on the Binding's own webhook, which is outside this spec's file surface; it is carried in
Dependencies as a prerequisite that has not landed, and until it does, **isolation is declared by the
stamp rather than guaranteed by a gate**.

F4b stays a refusal, because its subject is the pool the Pod is asking to join and its answer is
already observed continuously by a controller.

##### F4a — The engine end: no targeted engine sends a tenant, and the Pod is told so

**This is a fact about the engine versions this project targets, not a property of the technology, and
it is written to be falsifiable.** The client is capable: `setup()` has a second overload taking a dict
that forwards every key, `tenant_id` included (`mooncake-integration/store/store_py.cpp:2237-2271`;
the key is read at `mooncake-store/src/real_client.cpp:1310` and named at
`mooncake-store/include/types.h:214`). What is missing is on the **caller** side — each engine writes
its own config reader with no `tenant_id` key, and each calls the **positional** overload, which stops
before that parameter:

| engine | version measured | reads a tenant? | evidence |
|---|---|---|---|
| vLLM | `v0.25.1` | **no** | `.../mooncake/store/worker.py` contains no `tenant` at all; `setup()` is called with 7 positional arguments (`:1040-1048`), stopping four short of the parameter |
| vLLM-Ascend | `v0.19.1rc1` | **no** | This webhook now selects that engine's own store, `AscendStoreConnector` (`vllm_ascend/distributed/kv_transfer/__init__.py:39-43`), and its reader takes six keys with no tenant among them (`.../ascend_store/backend/mooncake_backend.py:115-124`). At this version there is no tenant anywhere: `grep` over `vllm_ascend/`, tests excluded, returns zero hits. Two earlier revisions were wrong here and both read the answer off something adjacent: first **yes**, citing `mooncake_backend.py:166-167,344,379` on upstream `main` - real lines, at a version we do not pin; then **no** *because* we rendered vLLM's connector instead, which predicted this would flip once we selected the Ascend one. We now do, and it did not |
| SGLang | `v0.5.18` | **yes** | all three config paths take a `tenant_id` — `from_file:164`, `load_from_env:208` (from `MOONCAKE_TENANT_ID`), `load_from_extra_config:257`; the store call adds it to its keyword arguments at `:505` when it differs from the literal `"default"`, and passes them at `:510` |

**This table asks about OUR configuration, not about the engines' capabilities.** Every wrong entry
in its history has the same shape: a property attributed to the wrong subject. First "no engine
forwards a tenant", written after reading one of them; then a vLLM-Ascend **yes** read off upstream
`main` rather than off the version we pin; then a **no** whose stated reason was the connector we
selected rather than the absence of a tenant. The field name invites it — `ForwardsTenant` sends the
reader to the engine's source and never to the configuration we write.

The third correction is the instructive one, because it was a *reason* that was wrong while the
verdict stayed right, and a wrong reason survives review that a wrong verdict would not. It also
carried a prediction — that selecting `AscendStoreConnector` would make this row **yes** — which the
connector fix has since falsified.

**Each version above is the one this project deploys**, and that qualifier is the correction rather
than a detail. The SGLang row previously read `gateway-v0.3.1-1689` and reported "no". That tag is
from SGLang's own scheme, is not in the runner catalog, and is neither deployed nor tested here — yet
it *looked* like a precise qualifier, carrying a describe format and a build number, so nobody
questioned what it qualified. The entry was measured accurately against the wrong object.

⇒ **When citing an engine's behaviour, the ref must be the version we deploy, not the one at hand.**
The two happened to coincide for vLLM, which is why the same method produced one right answer and one
wrong one, and why the wrong one survived review.

⇒ **The rule is per engine: inject the reuse domain where the engine reads one, and stamp what was
done either way.** SGLang is given `MOONCAKE_TENANT_ID`; vLLM is given nothing, because a key it does
not read would be decoration that reads as a guarantee.

⇒ **The stamp records the ACTION and never the outcome** — `tenantInjected`, not "isolated". Whether
an injected tenant takes effect depends on the engine BUILD, which admission never sees: it does not
inspect the container image, and the `engineVersion` it stamps is the release the facts table was
measured at rather than a reading of what will run. An SGLang older than the version measured is
handed a variable it never reads, shares the default tenant, and reports nothing.
A stamp claiming isolation would then be wrong in the one direction that misleads. So it states what
is certain and leaves the rest to be inferred from the engine actually deployed.

⇒ **The field must derive from the emission, not from its precondition.** Computing it as "a domain
was supplied" made it a statement of intent: a mutation that stopped the emission left the flag true,
so the stamp would have claimed a tenant no container carried. It is now a consequence of the append,
and a test asserts the admitted Pod's own environment rather than the renderer's report of it. The stamp names the engine, the version the fact was measured at, and the domain that is not
being enforced. It does **not** say "the engine does not support tenants" — that phrasing would outlive
the fact and keep claiming a limitation after an engine started forwarding, with nobody prompted to
revisit it.

**Every Binding declares a domain, which is why this cannot be a refusal.** `spec.domain` is required
and held by value, `spec.domain.name` is required, and an empty name is refused at Binding admission
(`ValidateQuotaPolicyTenantName`, `pkg/worker/kvcache/mooncake/quota_policy.go:129-132`). A gate
triggered on "a domain is declared" therefore has a hit rate of 100% while every entry in the table
above reads false, and this webhook would refuse every Pod it was ever asked about.

**Measured on a live cluster, the immediate cost is not a weaker guarantee — it is no writes at all.**
The table above says no engine forwards a tenant. What follows from that is stronger than "the domain
does not separate anything": the client falls back to its own default, the literal string `default`,
and a multi-tenant master refuses a write from a tenant that is not in its ledger. A `KVCachePool` is
only accepted over a multi-tenant backend (a pool is refused when its backend has no ledger to write
quota into), so there is no single-tenant configuration to fall back to. An injected Pod starts, stays
Ready, and fails every `put` with `TENANT_NOT_REGISTERED` (`-1701`).

Two experiments establish this, each answering exactly one question:

1. **Which name does an omitted tenant land on?** Three `setup()` calls against one master, differing
   only in this argument: omitting `tenant_id` and passing `tenant_id="default"` both failed
   identically with `-1701`, while a registered domain returned `PUT rc=0`. Two of them agreeing is
   what identifies the name; the third is what shows the mechanism is otherwise sound. The master's
   `/metrics` per-tenant dimension and its log were both checked for an echo of the name and neither
   carried one — this conclusion rests on the behavioural agreement, not on the master naming it.
2. **Does registering that name suffice?** A second Binding whose `spec.domain.name` is literally
   `default` was added to the same pool. The operator wrote it into the policy, and the *same*
   omitted-tenant client then returned `PUT rc=0` / `GET match=True`. The only variable was one more
   entry in the policy.

⇒ **The prerequisite is a Binding whose reuse domain is `default`, and it applies to `vllm` and
`vllm-ascend`.** An SGLang Pod is given its own domain and writes under a name its Binding already
registered, so a pool serving only SGLang needs no such object. It is documented rather than enforced,
and it is not checked at admission: the policy is a reconciler's product, so a Pod arriving before that
reconcile has converged would be refused for a race rather than for a mistake. The Pod carries the
answer instead — `tenantInjected` on the stamp says whether this webhook wrote a tenant at all.

**One `default` Binding exists per cluster, not per master**, because a reuse domain is unique
cluster-wide while a tenant ledger belongs to one master. A second backend therefore cannot have one,
and its injected Pods fail every write with `TENANT_NOT_REGISTERED`. Filed as #166, which also records
why an opt-in tenant would remove the need for the fix.

**A gate here would test a condition that is nearly constant.** Two of the three engine values read
false in the table above, and the third differs only in whether the tenant reaches the store — not in
whether the pool's policy carries `default`, which is the half that actually varies and the half this
webhook must not read. Anyone adding a gate here should know how little it discriminates.

**What the missing isolation costs once that Binding exists, measured rather than assumed.** The blocks
of every domain land in one tenant, so they are one eviction pool. Mooncake's quota is not an admission barrier: on a
charge failure the master **evicts that tenant's own older objects and retries**
(`master_service.cpp:4492-4506`, three attempts). With one shared tenant, "that tenant's own objects"
are the other domains' blocks, so one domain's write pressure silently drops another's.
Sharing the key space is *not* the harm — two deployments of one model sharing a prefix is the point of
the pool. Cross-domain **eviction** is, and it is invisible: this path does not go through the global
LRU, so `master_evicted_key_count` and its three siblings stay at zero. The one counter that moves,
`mooncake_tenant_quota_reject_total{reason="quota_exceeded"}`, increments only when all three retries
fail — a zero there means the eviction succeeded, not that nothing happened. **There is no metric that
would reveal this**, which is the whole reason the stamp has to say it.

**The harm boundary is the `KVCacheBackend`, not the pool.** One backend may be referenced by several
pools, which is why the ledger converges per master rather than per pool
(`api/worker/v1alpha1/kv_cache_pool.go:59-60`). Two Bindings on two different pools that share a
backend collide exactly as two on one pool do, so any check scoped to a pool would miss them.

**The discriminating check, so this stays honest as versions move:** grep the engine's own config class
for a `tenant_id` key, and count the arguments at its `.setup(` call site — fewer than 11 positional,
or a `setup(` that is not the dict overload, means the tenant is truncated. A version that passes both
belongs in the table as forwarding, and the stamp stops claiming a gap for it. **The table is the fact;
the stamp is derived from it.**

##### F4b — The master gate: refuse when the master holds no ledger

**This gate looked redundant and is not, and the reason is worth stating before the mechanics.** A
`KVCachePool` is already refused at creation when its backend carries no tenant ledger, so a Pod would
seem unable to ever meet a pool in that state. It can, because `multiTenancy` is an ordinary field on a
live `KVCacheBackend`: flipping it to `false` is accepted by admission with nothing rejecting it
(verified by a server-side dry-run patch). The chain then runs:

1. an administrator turns `multiTenancy` off on a backend — legal, unremarked;
2. the pool's reconciler observes it and sets `QuotaLedgerAvailable` to `False`;
3. **F4b refuses new Pods**, which is the only thing left standing between them and a pool that cannot
   separate anything.

⇒ **A create-time invariant does not protect against a later edit.** F4b is the last link of that
degradation chain rather than a second copy of the pool's own check, and an e2e case reaches it by
walking exactly those three steps. Two cheaper routes do not work and are recorded so nobody retries
them: a pool naming a nonexistent backend cannot be created at all (the reference is validated at
admission), and wedging a pool with a finalizer risks leaving one stuck on a shared cluster.

**The route that was chosen has that same cost, and execution is how we learned it.** Turning
`multiTenancy` off is precisely what makes the pool and its backend undeletable: the pool's finalizer
teardown reads the tenant ledger the master no longer holds, so it never completes, and the backend
cannot go while the pool is still there. One run left both wedged for 3h51m on a shared cluster.
Patching the flag back does not recover them — by then the backend is `Deleting`, its controller has
stopped reconciling children, and the leader keeps its old arguments (`generation` 2,
`observedGeneration` 2, no pod restart, no movement in 120s); only removing the pool's finalizer clears
it. The e2e case now restores multi-tenancy before deleting anything. The product behaviour underneath
is filed as #164: it needs no test to reach it, and the reasoning a user would have to do is far longer
than the symptom they would see.

`tenant_id` carries the reuse domain, and the isolation it promises exists **only** when the master runs
with `--enable_multi_tenants=true`. Measured, with one key written by two clients under different
tenants:

```
setup(tenant_id=team-a) rc=0 ; put -> 0 ; get len=4096 first=b'AAAA'
setup(tenant_id=team-b) rc=0 ; put -> 0 ; get len=4096 first=b'BBBB'
re-get team-a          len=4096 first=b'AAAA'      <- not overwritten
```

With multi-tenancy off, every request falls into the default tenant, the shard index degrades to a plain
key hash, and **different domains collide with each other's cache** — silently, corrupting results. The
master's own admin API says so directly: with multi-tenancy off it answers `-1011` / 409
`UNAVAILABLE_IN_CURRENT_MODE` to every tenant-quota call.

- The webhook reads the pool's **observed** state — specifically the `QuotaLedgerAvailable` condition,
  which the pool's reconciler writes with reason `MultiTenancyDisabled` when the master holds no tenant
  ledger. It is deliberately the *condition* and not the backend's `spec.…leader.multiTenancy` field:
  the condition is that precondition observed at runtime rather than assumed from a spec value.
  The three states map exactly onto what this gate needs — `True` injects, `False` is a configuration
  error, and **absent or `Unknown` is a wait, not an "on"**. The message distinguishes the last two.
- **This gate is not redundant with the pool's own admission, and the external branch is why.** A
  *managed* backend without multi-tenancy already fails at `KVCachePool` admission, so a resolvable
  pool over one necessarily has a ledger. An *external* backend is not asked that question at all —
  this operator did not start that master and cannot know how it was launched — so for external
  backends the runtime condition is the **only** place the answer exists. A gate reading the spec field
  would pass every external pool blindly, which is precisely the case that silently collides.
- The refusal message names the pool and the flag, so the reader knows what to change and where.
- **Neither gate can be relaxed by an annotation.** There is no override: a Pod that opts in while its
  Binding declares a domain is asking for an isolated reuse domain, and there is no version of "inject
  anyway" that gives it one — from either end.
- **There is no ordering to arbitrate.** An earlier draft had both ends refusing and put F4a first;
  F4a now stamps, so F4b is the only refusal here and nothing competes with it for the message. What
  survives from that draft is the reason it mattered: a refusal must point at the end an operator can
  actually act on, which is why the one that remains is the multi-tenancy check on the pool.

#### F5 — What gets injected, and by which mechanism per engine

**The contract is the engine's own config file, not the client's `setup()` signature** — and confusing
the two is the trap this section exists to disarm. There are **three** naming spaces for the same
values, and the one this webhook writes into is the third:

```python
# 1. setup() positional overload — what the engines actually call, truncating at 7 args
setup(local_hostname, metadata_server, global_segment_size, local_buffer_size,
      protocol, rdma_devices, master_server_addr, engine=None, enable_ssd_offload=False,
      ssd_offload_path='', tenant_id='default', enable_client_http_server=False,
      client_http_port=9300) -> int

# 2. setup(config: dict) overload — same key names as the positional parameters, all forwarded
setup({'master_server_addr': ..., 'rdma_devices': ..., 'tenant_id': ...}) -> int
```

```jsonc
// 3. The engine's config FILE — different key names, and what this webhook renders
{ "master_server_address": "...",   // not master_server_addr
  "device_name": "",                // not rdma_devices
  "metadata_server": "P2PHANDSHAKE",
  "protocol": "tcp",                // absent defaults differ: vLLM "rdma", SGLang "tcp"
  "mode": "standalone-store",       // required whenever global_segment_size is 0
  "global_segment_size": 0,
  "local_buffer_size": ... }
```

Three consequences the renderer is built around, each measured rather than assumed:

- **The file's key names win, and they differ from the signature's.** `master_server_address` carries
  four letters the parameter does not, and the RDMA filter is `device_name` in a file but
  `rdma_devices` as a parameter. An earlier draft of this spec asserted the reverse — that the name is
  `rdma_devices` "not `device_name`" — which is true of `setup()` and **exactly wrong** for the file.
- **A wrong key in the file is silent.** The readers are `config.get(key, default)`
  (`worker.py:129-142`), so an unrecognised key is ignored and its value falls to the default. This is
  the opposite of the positional overload, where a wrong keyword raises `TypeError`. Silence is why
  every key the renderer emits is asserted against the engine's own config class in a test.
- **`global_segment_size` and `local_buffer_size` are a ROLE DECLARATION, not two size settings.**
  This is the single fact that explains every rendering rule below, and reading them as sizes is what
  makes omitting one look harmless. The store's own design page states the two switches directly
  (`docs/source/design/store/mooncake-store.md:36-38`):

  > If `global_segment_size` is set to zero, the instance functions as a **pure client**, issuing
  > requests but not contributing memory to the system.
  >
  > If `local_buffer_size` is set to zero, it acts as a **pure server**, providing memory for storage.
  > In this case, request operations such as `Get` or `Put` are **not permitted** from this instance.

  One `Client` class serves two roles at once, and the two numbers are what select between them:

  | `local_buffer_size` | `global_segment_size` | the instance is |
  |---|---|---|
  | `> 0` | `0` | **pure client** — issues Get/Put, contributes no memory. **What an engine Pod must be.** |
  | `0` | `> 0` | pure server — contributes memory, and **may not Get or Put at all** |
  | `> 0` | `> 0` | both roles: an embedded store member. **This is what writing neither field produces.** |

  So an omitted field does not fall back to a sensible size — it **declares a different role**. Writing
  neither leaves the engine container as an embedded member contributing its reader's default —
  4 GiB on vLLM (`DEFAULT_GLOBAL_SEGMENT_SIZE`, `worker.py:74`), 1 GiB on vLLM-Ascend
  (`mooncake_backend.py:18`) — that appears in no `resources` field, while the Pod
  was meant to be a pure client. The store's own deployment guidance describes the shape this spec
  targets: with a standalone store service, *"embedded clients can be configured with
  `global_segment_size = 0` so they contribute network/NIC resources only"*.
  **The two ways of getting it wrong fail in opposite directions, and the quiet one is worse — on
  vLLM. On vLLM-Ascend both are quiet**, because that reader has no `mode` key and so no consistency
  check to raise: an omitted size makes it a 1 GiB member, and a `0` with no `mode` is simply
  accepted. The loud row below exists on one engine only, which is a reason to render the pair
  rather than to rely on being told:

  | rendered | outcome on `vllm` | outcome on `vllm-ascend` |
  |---|---|---|
  | `global_segment_size: 0`, no `mode` | **raises `ValueError`** at startup — loud, immediate, diagnosable | accepted silently; the size alone decides, and `0` is the shape we want |
  | `global_segment_size` **omitted**, no `mode` | **no error**, wrong role. `embedded` is satisfied by the 4 GiB default, so the container silently becomes a store member | **no error**, wrong role, at 1 GiB |

  vLLM adds its own consistency check on top: `__post_init__` (`worker.py:118-123`) raises **both**
  ways — `embedded` with `global_segment_size == 0`, and `standalone-store` with anything else — and
  `mode` defaults to `embedded` (`worker.py:110`). So the engine-facing rendering is fixed at
  `global_segment_size: 0`, `mode: "standalone-store"`, and a `local_buffer_size` above zero. A test
  asserts the three move together, because any one of them alone changes what the instance *is*.
- **vLLM's file schema is closed at eight keys**, read one by one — `metadata_server`,
  `master_server_address`, `protocol`, `device_name`, `mode`, `global_segment_size`,
  `local_buffer_size`, `enable_offload` (`worker.py:129-142`). There is no `**config` splat and no
  passthrough, so a key outside this set is not merely ignored: **no code path ever sees it.** This is
  the fourth independent confirmation that `tenant_id` cannot reach the engine — writing it into the
  file changes nothing, because `from_file` never reads it. vLLM-Ascend reads the same file with a
  reader closed at **six** of those names, without `mode` or `enable_offload`
  (`mooncake_backend.py:115-124`) — it ignores the `mode` we render, which is safe only because vLLM
  does not pass `mode` to `setup()` either. SGLang's reader is closed at ten keys of
  its own (`mooncake_store.py:114-141`), which are not these eight; the per-engine table below is the
  one both renderers work from.

**The vehicle is per engine, and the reason is a value only the runtime knows.** vLLM must take a
file; SGLang must take environment variables. An earlier draft of this spec closed this the other way
— "a file for both" — on the strength of the claim that no engine reads the client's named
`MOONCAKE_*` variables. SGLang reads exactly those, and picking the file for it costs a value that
cannot be rendered at admission time.

| engine | vehicle | what the webhook writes |
|---|---|---|
| `vllm` | a projected file | arg `--kv-transfer-config` selecting `MooncakeStoreConnector` and the role; env `MOONCAKE_CONFIG_PATH` naming the file; a read-only volume and mount carrying it |
| `vllm-ascend` | a projected file | the same, with `AscendStoreConnector` as the connector: the two share a vehicle and the file's keys, not a connector registry |
| `sglang` | environment variables | arg `--hicache-storage-backend mooncake`; the `MOONCAKE_*` variables below, `MOONCAKE_LOCAL_HOSTNAME` among them as a `fieldRef` to `status.podIP`; **no** `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, no volume, no mount |

- **vLLM needs a file, measured rather than assumed** — this was T1's open lookup and it is now closed.
  `MooncakeStoreConfig.load_from_config()` (`worker.py:144-151`) reads `MOONCAKE_CONFIG_PATH` and
  **raises when it is unset**; there is no environment fallback, unlike Mooncake's own
  `MooncakeConfig.load_from_env()`. So the file, its volume and its mount stay.
- **The client accepts a zero on both size keys, and the two zeros do not mean the same thing.**
  `setup_internal` skips mounting a segment for a zero, in its own words "A size of 0 keeps the pure
  client/server setup semantics", and the validator refuses only a *small non-zero* value
  (`if (value != 0 && value < MIN_SEGMENT_SIZE)`). So there is no technical obstacle to writing zero on
  either key.
  That closes the question of whether zero is *legal*, not whether it should be *used*. A zero
  `global_segment_size` means "contributes no storage segment", which is what an engine container
  wants. A zero `local_buffer_size` declares pure-server semantics, which may not `Get` or `Put` - the
  opposite of a client. The `128 MiB` constant therefore stands, and the two keys are written together
  precisely because their zeros point in opposite directions.

- **SGLang needs the environment, and the reason is evaluation time rather than key coverage.**
  `local_hostname` is an **address**, not a label: vLLM computes it at runtime as
  `get_requester_local_hostname(local_ip)`, which returns the local IP
  (`vllm/distributed/kv_transfer/kv_connector/v1/mooncake/rdma_utils.py:21-25`) from `get_ip()`. A Pod's
  IP does not exist when a mutating webhook runs — the Pod has not been scheduled, let alone assigned
  one.
  `_load_config` offers three sources (`mooncake_store.py:242-260`), and the decisive difference between
  them is *when their contents are fixed*, not what they can express:

  | source | contents fixed | can carry the Pod's IP |
  |---|---|---|
  | `extra_config` (a CLI argument) | at admission, by this webhook | no |
  | the config file (a projected annotation) | at admission, by this webhook | no |
  | the environment | **at container start, by the kubelet** | yes, through `fieldRef: status.podIP` |

  The first two are the same failure for the same reason, and both were checked rather than assumed:
  `from_file` (`mooncake_store.py:114-141`) and `load_from_extra_config` (`mooncake_store.py:183-221`)
  are key-for-key isomorphic, each falling back to `envs.<NAME>.default`. That attribute is the literal
  default (`environ.py:41-42`); `.get()` is the accessor that reads the process environment
  (`environ.py:54-72`) and neither of those two paths calls it. So on either of them an unwritten
  `local_hostname` resolves to the literal `"localhost"` (`environ.py:296`) — the same wrong value on
  every Pod in the pool — and a written one would be a value this webhook had to invent.
  Only `load_from_env` reads through `.get()` (`mooncake_store.py:167-180`), which is what lets
  `MOONCAKE_LOCAL_HOSTNAME` be a `fieldRef` the kubelet resolves at container start — exactly when vLLM
  computes its own.
  That variable has a **two-level** fallback: unset, `load_from_env` reads the legacy, ungeneric name
  `LOCAL_HOSTNAME` before giving up to the literal (`mooncake_store.py:158-165`). This webhook always
  sets `MOONCAKE_LOCAL_HOSTNAME` explicitly, so the fallback is never reached — but the safety comes from
  setting it, **not** from the variable having no other source. A bare `LOCAL_HOSTNAME` is a name another
  component may well export, and a design that relied on the fallback would be relying on nobody else
  using a common word.
  **This is also why the answer is not "wait for upstream".** An upstream release that taught the file
  reader more keys would change nothing: the value is not missing from the schema, it does not exist yet
  at the moment the file is written. The env path is selected by **not setting**
  `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, which drops `_load_config` past the file branch.
- **`config.local_hostname` is not always what reaches `setup()`, and running that down makes the case
  narrower and stronger rather than weaker.** There is a branch (`mooncake_store.py:353-370`) that takes
  a self-derived session id instead, under **four** conditions at once: a shared transfer engine exists,
  `device_name` equals that engine's own IB device, `metadata_server` is `P2PHANDSHAKE`, and `protocol`
  is `rdma`. Only when all four hold does the file path accidentally produce a correct hostname.
  **Under this spec's rendering that branch is unreachable, on every transport.** F5 writes
  `device_name` empty on every path, and the comparison is against
  `MooncakeTransferEngine.get_ib_device()`, which returns `self.ib_device` from
  `get_ib_devices_for_gpu`, and that returns `None` for an empty or blank device string
  (`mooncake_transfer_engine.py:31-32,114,260-261`). So the clause is `"" == None` when the shared
  engine has no device and `"" == "<name>"` when it has one - false either way. `device_name` is
  reassigned before the branch only inside a JSON-parsing path that requires the value to start with
  `{`, so the empty string reaches the comparison intact.
  **This is a constraint on a future change, and worth stating as one.** The reasoning above depends on
  a rendering decision, not on the engine: the day `device_name` becomes configurable, the branch
  becomes reachable and this defect turns into one that appears only on RDMA hosts with a shared engine.
  A defect that is correct by luck on some hardware is worse than one that is always wrong, because the
  hardware that hides it is the hardware people test on. The verification hardware for this spec has no
  RDMA, so e2e exercises the always-wrong side - which is the right side to exercise, but it is a
  property of the lab rather than a guarantee, and it stops being true the moment someone tests on an
  RDMA host.
- **The two engines look for their file under different variable names, which is what makes the split
  clean.** vLLM reads `MOONCAKE_CONFIG_PATH`; SGLang reads `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`. So
  injecting the first into a vLLM container cannot accidentally push an SGLang container onto its file
  branch, and the per-engine vehicles never interfere.
- **A user's own configuration always wins, and that is deliberate rather than a gap.** `extra_config`
  and the config file both outrank the environment in `_load_config`, so an operator who sets either
  silently takes over from this injection. That is the correct precedence — an explicit configuration
  should outrank a defaulted one — so this webhook does not gate on it.
  The documentation names the two concrete entrances rather than describing them, because a name is
  what a reader can search their own manifest for: the argument
  **`--hicache-storage-backend-extra-config`** (`server_args.py:4753`), and the variable
  **`SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`**. Either one silently makes the injected variables stop
  mattering.
- **SGLang's `--hicache-storage-backend-extra-config`** is not the vehicle either, even though it
  outranks both others. Its reader is isomorphic to the file's, so it inherits the `localhost` problem
  in full, and it would additionally put the whole configuration on the command line — colliding with
  the one argument a user is most likely to have set for themselves.
- **The file is projected from the Pod's own annotation, via `downwardAPI`** — for the vLLM family, the
  webhook writes the rendered client JSON into `kvcache.gpustack.ai/client-config` on the Pod it is
  already mutating, and projects that annotation as a file into the container. No ConfigMap, so no
  webhook side effect, no RBAC, no garbage collection, and the file's lifetime is exactly the Pod's. The
  mount is read-only at a fixed path. SGLang carries no such annotation, volume or mount.
- **`tenant_id` is written nowhere**, in any vehicle. No engine's config class carries the key, so
  writing it would be decoration that reads as a guarantee — the worst of both. F4a reports the gap on
  the stamp instead, where it is addressed to an operator rather than to the store.
- **The two engines do not share one key set, and treating them as one is how a key gets dropped.**
  The writable keys per engine, measured from each engine's own reader:

  The two vLLM-family engines get their own columns because they do not read the same set: the
  Ascend reader is closed at six keys and has neither `mode` nor `enable_offload`.

  | key | `vllm` | `vllm-ascend` | `sglang` | what this webhook writes |
  |---|---|---|---|---|
  | `master_server_address` / `MOONCAKE_MASTER` | yes | yes | yes | the pool's `status.clientEndpoint` |
  | `metadata_server` / `MOONCAKE_TE_META_DATA_SERVER` | yes | yes | yes | the constant `P2PHANDSHAKE` |
  | `protocol` / `MOONCAKE_PROTOCOL` | yes | yes | yes | the backend's transport, always explicit |
  | `device_name` / `MOONCAKE_DEVICE` | yes | yes | yes | empty, on every path |
  | `global_segment_size` / `MOONCAKE_GLOBAL_SEGMENT_SIZE` | yes | yes | yes | `0` |
  | `mode` | yes | **no key** | **no key** | `standalone-store`; only vLLM reads it |
  | `local_buffer_size` | yes | yes | **no key** | the `128 MiB` constant, vLLM family only |
  | `local_hostname` / `MOONCAKE_LOCAL_HOSTNAME` | **no key** (computed at runtime) | **no key** (computed at runtime) | yes | a `fieldRef` to `status.podIP`, SGLang only |
  | `enable_offload` | yes | **no key** | no | never written |
  | `master_metrics_port`, `check_server`, `standalone_storage`, `client_server_address` | no | no | yes | never written; their defaults are the pure-client shape already |

  Every engine defaults `global_segment_size` to something non-zero when it is absent, which is why
  it is written explicitly on all three and for the same reason. The defaults themselves differ and
  the difference is not load-bearing, only the non-zero part is: vLLM 4 GiB (`worker.py:74`),
  vLLM-Ascend **1 GiB** (`mooncake_backend.py:18`), SGLang `"4gb"` (`environ.py:298`).
- **`global_segment_size` is `0`, and `mode` is `standalone-store` with it, on the vLLM family.** An
  engine Pod is a *client*; the pool's capacity comes from the backend's declared members. Contributing
  host memory from an engine Pod would change that Pod's real memory footprint without appearing
  anywhere in its `resources`, and this webhook never mutates `resources`. The two fields are the locked
  pair above. SGLang has no `mode` key at all, so the zero segment stands alone there.
- **SGLang divides the segment size across tensor-parallel ranks before passing it on.** The value that
  reaches `setup()` is `per_tp_global_segment_size`, computed from the configured value and
  `storage_config.tp_size` (`mooncake_store.py:293-295,374`). Writing `0` is unaffected — zero divides to
  zero — but anyone who later renders a non-zero segment here must account for it: the host memory the
  Pod actually contributes is the per-rank value times the TP degree, not the number written.
- **`local_buffer_size` is a vLLM-family key, written explicitly as a constant `128 MiB`, and reads from
  no API field.**
  What it is, in the store's own words
  (`docs/source/design/store/mooncake-store.md:500`):

  > Each Store client can also create a setup-time local buffer through `local_buffer_size`. This
  > memory is registered once with the Transfer Engine and managed by `ClientBufferAllocator` for
  > **short-lived client-side staging work**.

  So it is **transfer-layer staging, not a tenant resource allowance** — which is why it belongs on
  neither the pool nor the Binding: those carry the grant and its quota, and this is an implementation
  detail of how bytes cross the wire. The store's own examples use `128*1024*1024  # 128MB local
  buffer` (`docs/source/api-reference/python/mooncake-store.md:50,87`), and this spec renders that
  value.
  It is written rather than omitted because the layers disagree by orders of magnitude: the file
  default is **4 GiB** on vLLM (`worker.py:75`) and **1 GiB** on vLLM-Ascend
  (`mooncake_backend.py:19`), while the client's pybind default is **16 MiB**
  (`store_py.cpp:2205`), and `from_file` takes the engine's. Those are figures chosen for an
  *embedded* instance that also serves memory; for a pure client either is enormous, held inside a
  container limit its owner sized for a model, and the symptom is an OOM pointing at no field
  anybody wrote.
  It must also stay **above zero**: a zero here declares a pure server that may not `Get` or `Put`.
  Whether this should become configurable per Binding or per Pod is an Open Question, deliberately not
  answered by adding a field now — no consumer has asked for one, and it is a staging buffer rather
  than a resource grant.
  **SGLang is not given a value here and does not need one.** It has no such key, and hardcodes
  `DEFAULT_LOCAL_BUFFER_SIZE = 16 MiB` at both of its `setup()` call sites, with the reason in its own
  comment: "Zero copy interface does not need local buffer"
  (`mooncake_store.py:22,336,376`). Rendering the constant for SGLang would write something nothing
  reads.
- `device_name` — the RDMA filter — is written **empty on every path**, RDMA included. See F3: empty
  means "use every device found", which is the only value correct for every host in a pool, and the
  string `auto-discovery` is not special-cased anywhere in the client. It is written rather than omitted
  for the same reason `local_buffer_size` is: the engine's default for an absent key should not be
  something a reader has to know to predict the container's behaviour.
- The prefill/decode role, when the caller sets one, selects the connector role for the vLLM family
  (`kv_producer` / `kv_consumer`, and `kv_both` when unset). For `sglang` the role annotation is
  **refused** rather than accepted and ignored: an accepted-and-ignored role is exactly the silent wrong
  result this spec refuses everywhere else. What SGLang's equivalent knob is belongs to the
  prefill/decode spec, and is recorded in Open Questions.
- `vllm-ascend` uses the vLLM synthesis. It has its own enum value so the two can diverge later without
  an API change. It needs **no transport special-casing of its own**: `Ascend` is already one of the
  values the backend's `spec.transport.protocol` enum accepts, and `mooncake.MemberProtocol` already
  maps it to the artifact's `ascend`. So this engine resolves its protocol through the same D4 map as
  every other engine, and what Ascend additionally needs — the CANN runtime present in the container —
  is an image concern this webhook cannot see and does not judge.

#### F6 — Refuse rather than guess

Three places where guessing produces a silently wrong result. In all three the Pod is **rejected with an
actionable message** naming the annotation or container at fault and what to set.

1. **The engine is not declared.** Different engines take completely different flags. Sniffing the image
   name mis-injects silently on a renamed or vendored image, and the symptom — an engine that starts
   fine and caches nothing — is invisible from outside.
2. **The target container is ambiguous.** With exactly one container in `spec.containers`, use it. With
   several, require `kvcache.gpustack.ai/container`. **Never pick the first.** The grounding is this
   repo's own experience: an injection landed in a sidecar while the workload ran in the main container,
   and the symptom was "artifacts present, feature absent" — undiagnosable from outside the Pod. A named
   container that does not exist is refused, naming the containers that do. `spec.initContainers` are
   never candidates and naming one is refused.
3. **A key that selects the mechanism is already set.** If the target container's `args` or `command`
   already carries `--kv-transfer-config` or `--hicache-storage-backend`, or its `env` already carries
   `MOONCAKE_CONFIG_PATH`, the Pod is **refused** — never merged. A silent merge produces "two
   `--kv-transfer-config`, which one wins", which is undiagnosable. The same applies to a volume or
   mount already occupying the name or path this webhook owns. Taking over is an explicit opt-out: set
   `kvcache.gpustack.ai/inject: "false"`, or simply do not carry the label.
   - **The list is the keys whose presence makes the injection ambiguous, and it shrank with the
     design.** `MOONCAKE_TENANT_ID` and `--hicache-storage-backend-extra-config` are **not** on it:
     the webhook stopped writing them, and refusing a Pod over a key we do not set would reject a user
     doing something legitimate — including, in `MOONCAKE_TENANT_ID`'s case, the one workaround
     available to someone running a patched engine.
   - `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` came **off** the list with the per-engine vehicle. The
     webhook no longer writes it, and a user who sets it has configured SGLang from a file of their
     own — correct precedence, not a collision. See the residual in Risks: the injection yields
     silently there, and only the documentation says so.
   - **The per-engine value variables are not on this list either; they yield instead of refusing.**
     `MOONCAKE_MASTER` and its siblings are values inside the mechanism rather than the mechanism, and
     this repository already has one rule for that case: an injection leaves a variable the workload
     declared for itself alone (`deviceplugin.ContainerEnvDeclared`, `pkg/deviceplugin/budget.go:45-55`,
     used at `pkg/devicemanager/allocator/nvidia/deviceplugin.go:340`). This webhook reuses it rather
     than inventing a second precedence, so a user overriding one variable keeps the rest of the
     injection.
   - `command` is scanned as well as `args`, because a user may put the flag in either.
   - **`envFrom` sources are not read, and the consequence is a known limitation rather than a
     cosmetic one.** Kubernetes resolves an inline `env` entry over any `envFrom` source, and
     `ContainerEnvDeclared` inspects only inline entries (`pkg/deviceplugin/budget.go:45-47`). So a
     user who supplies `MOONCAKE_MASTER` from a ConfigMap gets `false` from that check, this webhook
     injects its own, and **the injection silently wins over the user's configuration** — the opposite
     of the precedence stated everywhere else here.
     An earlier draft of this section called the residual cosmetic. That was written about the
     *detection* direction and is wrong in this one: the existing callers of that helper inject a log
     level, where being overruled makes the logs noisier, while this one injects a storage address and
     a segment size, where being overruled means connecting to the wrong pool or contributing a
     segment nobody asked for.
     It is still not detected, and the reason is that detection cannot be made correct: this webhook
     cannot read a ConfigMap while declaring `sideEffects: None` on a hot admission path, and a value
     read at admission is stale by the time the kubelet resolves it. This repository already answers it
     the same way — `pkg/devicemanager/allocator/ascend/deviceplugin.go:316-317` documents the blind
     spot in a comment and tells the reader to use an explicit `env` entry — so the documentation
     states the rule: declare Mooncake variables in `env`, because one supplied through `envFrom` is
     overwritten with no symptom.
   - The two observability variables of F8 are deliberately **outside** this rule: a user-set value wins
     and is left alone. They change no result, and refusing a Pod over a metrics toggle would be a
     rejection with nothing behind it.
4. **The target container declares neither `command` nor `args`.** Both engines need a flag on the
   command line — `--kv-transfer-config` and `--hicache-storage-backend` have no environment equivalent
   (`vllm/envs.py` and `python/sglang/srt/environ.py` carry neither) — so the injection has to append to
   `args`. Kubernetes gives `args` a second meaning when `command` is also absent: the container runs
   the image's `ENTRYPOINT` with **only** the supplied args, and the image's `CMD` is discarded
   entirely. Appending to an empty `args` would therefore delete the engine's own launch arguments,
   which this webhook cannot see and cannot restore. The Pod is refused, and the message names the fix
   precisely: copy the image's launch arguments into **`args`**, leaving `command` unset. It must say
   which field, because `command` is the wrong one — setting it overrides the image's `ENTRYPOINT` too,
   and on the accelerator images this project ships that entrypoint is a wrapper that initializes the
   vendor runtime. Bypassing it produces a failure further from its cause than the discarded `CMD` this
   refusal exists to prevent.

#### F7 — A Pod that does not opt in is untouched, and an injected Pod says so

- A Pod without the label is byte-identical after admission. Asserted by diffing the admitted object
  against the submitted one, not by inspecting the webhook's own decision.
- An injected Pod carries `kvcache.gpustack.ai/injected`, a small JSON object recording the resolved
  Binding, the engine and the version F4a judged it at, **which vehicle was used**, and **whether the
  Binding's reuse domain is actually in effect** — one place to look when asking "did this Pod get a
  cache, and under what". It is JSON rather than prose so a `-o jsonpath` query can select on a field;
  annotations are not label-selectable, so being machine-readable is the most a stamp can be.
  The vehicle is on it because that is what turns the one silent outcome in Risks into a one-line check:
  a Pod stamped with the environment vehicle whose cache is cold is a Pod to check for its own
  `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`.
- **The isolation field is the load-bearing one, because it is the only place the gap is visible at
  all.** F4a injects rather than refusing, so a Pod whose domain will not take effect looks exactly like
  one whose domain would — and the cost of the difference, cross-domain eviction, moves no metric (see
  F4a). The field carries the domain that was declared, that it is not enforced, and the engine version
  that is the reason, so the fact is queryable per Pod rather than inferable from nothing.
  Stamping the domain here is not the false guarantee that writing a `tenant_id` key would be: the key
  would claim isolation to the store, while the stamp reports to an operator that there is none.
- **The `tenant` field answers a different question from the isolation one, and a harsher one.**
  Isolation says the declared domain separates nothing; `tenant` says which name the writes actually
  land on. An unregistered name is not a weaker guarantee — it is a Pod that starts, stays Ready, and
  fails every write (F4a). The value is derived from the same verdict the isolation field is, never
  read from the facts table directly: reading the table there would let the two fields disagree, which
  is the one combination neither may report, and would make the tenant the one value a substituted
  answer cannot move. It is the same string for every engine accepted today, and it is recorded anyway
  because the Pod that cannot write is the first object anyone opens.
- The mutation touches only `env`, `args`, `volumes`, `volumeMounts` on the one target container, plus
  the two annotations it writes. **No `resources` key, no label, no owner reference, no scheduling
  field.** Asserted by a test that diffs the whole Pod and enumerates the allowed paths.

#### F8 — Observability defaults, and the operational facts that must be documented

The client's observability knobs are off or auto by default:

```
MC_TE_METRIC=1                    enable transfer-engine metrics (OFF by default)
MC_STORE_CLIENT_METRIC_BANDWIDTH  client bandwidth summary
MC_STORE_MEMCPY                   unset => auto-detected ("TCP-only environment, memcpy enabled")
```

- The webhook sets `MC_TE_METRIC` and `MC_STORE_CLIENT_METRIC_BANDWIDTH` on by default. The pool's
  hit-rate story depends on them, they cost nothing when nothing scrapes them, and a knob that is off by
  default is one a user finds only after the incident that needed it.
- `MC_STORE_MEMCPY` is left **unset**, deliberately: unset means auto-detected, and the auto-detection
  already picks memcpy in a TCP-only environment. Writing a value would replace a correct runtime
  decision with a webhook's guess.
- Both defaults are overridable by simply setting the variable yourself (F6).

Two operational facts go in the reference page, or every user files them as bugs:

- **The transfer engine binds random ports.** One observed run took `15002` (P2P RPC) and `15995` (TCP
  transport); a second client took `16566` and `16655` — none of them configured. **Any NetworkPolicy or
  port reservation must be a range, not a list.** The webhook cannot fix this; it documents it, and any
  guidance it renders uses a range.
- A benign startup ERROR appears on every client:
  `E transfer_metadata.cpp:991] Local segment descriptor not found`.

### Verification

**Hardware: a local Kubernetes cluster. No GPU, no RDMA, no cloud.** The pool runs with the TCP
transport and a single master with multi-tenancy and a quota connector on, which is the configuration
the companion pool spec already has to stand up for its own acceptance.

| # | Case | Vehicle | What proves it |
|---|---|---|---|
| 1 | **The headline** — a plain `Deployment`, no queue-name label, nothing to do with this project's workload CR | `Deployment` + label + binding annotation, **and a Binding whose reuse domain is `default`** | its Pod **reads and writes the pool**: the master's used-bytes figure moves and a read returns what was written. The `default` Binding is a precondition of the read/write half, not of the injection half: without it the Pod is still injected and still starts, and every write fails with `TENANT_NOT_REGISTERED` (F4a) |
| 2 | **No workload lock-in** | a `LeaderWorkerSet`, and separately a bare Pod | identical injection, identical read/write result |
| 3 | **The stamp that replaces isolation** — a Binding declaring a domain, against an engine the F4a table marks as truncating | `Deployment` + a domain-carrying Binding | the Pod is **created and injected**, and its stamp says the declared domain is **not** enforced, naming the engine version as the reason. This is the case that keeps the spec honest: it asserts we do not ship a Pod that looks isolated and is not, and that the difference is readable from the Pod itself rather than only from this document |
| 4 | A Pod without the label is untouched | submit and diff | admitted object byte-identical to the submitted one |
| 5 | The refusals, on a live API server | one Pod each: no engine annotation; several containers and none named; an owned key already set; the Binding absent from the namespace; the pool reporting multi-tenancy off | `create` fails with a message naming the annotation, container or object at fault — the remaining refusals of F1/F3/F6 are unit-covered |
| 6 | The selector is the injection label | read the installed `MutatingWebhookConfiguration` | the new entry selects `kvcache.gpustack.ai/inject`, **not** `kueue.x-k8s.io/queue-name` |
| 7 | **Each rendered artifact is one its own engine accepts** — one case, both vehicles, because it is one question asked twice | a `vllm` Pod and an `sglang` Pod, each feeding its artifact to that engine's own reader | vLLM: `MooncakeStoreConfig.from_file` parses and `__post_init__` does **not** raise — the `mode`/`global_segment_size` pair is what is being proven, since getting it wrong aborts the engine at startup. SGLang: `_load_config` itself is called — never `load_from_env`, which is a staticmethod and so *is* the choice rather than a test of it — and **all three** of its branch log lines are asserted, the env one present and the other two absent. A positive control then populates `extra_config` and requires that branch to win, so the rule that we must not occupy `extra_config` is exercised rather than merely stated. Finally **`local_hostname` comes back as the Pod's IP rather than `"localhost"`** — the value that decided the vehicle in the first place |

**Case 1's read/write is proven with the client library, not with a running engine.** The Pod's
container consumes the injected `MOONCAKE_CONFIG_PATH` file exactly as the vLLM connector does at init —
parse the file, then a put and a get — so the injected configuration is proven complete and correct
against the real client.

**Case 7's SGLang half is the one that would pass for the wrong reason if written casually.** A fixture
that read the environment itself, rather than through SGLang's own `_load_config`, would prove only that
this webhook set the variables it meant to set. What has to be proven is which branch SGLang takes, so
the fixture calls `_load_config` and asserts on the config object it returns — a fixture that reads
`os.environ` directly would be green even if the file branch had swallowed the injection whole.

**The fixture must call `setup()` the way its engine does — positionally, and with that engine's own
argument count — and must not use the dict overload.** The dict overload forwards every key, `tenant_id`
included, so a fixture using it would be *more capable than the engine it stands in for*: it would carry
a tenant the real connector truncates, and case 3's stamp would read "enforced" for a deployment where
nothing enforces it. A test that quietly outperforms the thing it models proves the wrong
system. The vLLM fixture therefore mirrors `worker.py:1040-1048` argument for argument (seven), and the
SGLang one mirrors `mooncake_store.py:372-381` (eight, the extra one a transfer-engine handle). Both
stop short of parameter 11. What this does **not** prove is vLLM's own parsing of
`--kv-transfer-config`, which needs a GPU host; that is a bounded, named gap with a hardware follow-up,
not a claim quietly folded into a green local run. It is also why acceptance is stated as bytes moving
in the pool rather than as an environment variable being present: measured in this same programme,
`--enable_kv_events=true` is accepted, the master's log echoes `enable_kv_events=1`, and the status
endpoint still reports `{"enabled":false}` with no socket ever bound — while a *different* undeclared
switch in the same codebase fails loudly (`TENT backend is not enabled. Please rebuild with
-DUSE_TENT=ON`). **You cannot infer one switch's failure mode from another's**, so nothing here is
accepted on the strength of a flag being accepted.

### Notes / Constraints / Caveats

- **Known limitation, recorded rather than enforced: an SGLang older than `v0.5.18` silently loses its
  tenant.** The renderer writes `MOONCAKE_TENANT_ID` for every SGLang Pod; a build predating that
  variable never reads it, so the Pod writes under the store's default tenant, shares a cache with
  every other domain, and **nothing reports it**. There is no version gate, and admission has nothing
  to build one from — it never inspects the image — which is precisely why the stamp records
  `tenantInjected` (what was done) instead of an isolation verdict (what resulted).
  **The two failure directions are asymmetric, and that asymmetry is the operational fact.** A
  Mooncake *client* too old to accept the tenant argument makes SGLang raise, so the Pod does not
  start — loud, immediate, unmissable. An *SGLang* too old to read the variable produces no signal at
  all: the workload runs, the cache works, and the isolation quietly is not there. Pin the engine
  version you tested.
- **Known limitation, recorded rather than enforced: the file vehicle needs vLLM `0.21.1` or newer.**
  vLLM's Mooncake store connector — the module holding `MooncakeStoreConfig`, `from_file` and
  `load_from_config` — was added 2026-05-13 and first shipped in `v0.21.1rc0`. Below that the module
  does not exist, so the projected file is mounted and **read by nothing**: the workload runs
  correctly, with no cache and no error. Of the vLLM versions the runner catalog publishes, the
  majority are below this floor, and admission cannot tell which one a Pod will run: it never
  inspects the image, and there is no `engineVersion` annotation for a user to declare one.
  **No version validation is added** — nothing on this path has the input it would need. The mitigation is that the limitation is written down, in this list and in
  `docs/reference/kv-cache-injection.md`, together with the one command that distinguishes it: an
  `ImportError` from that module means the injection is inert on that image. Note the symptom differs
  from the unregistered-tenant one in F4a and calls for a different fix — that failure is loud on
  every write, this one is entirely silent.
- **No chart manifest and no chart RBAC.** Webhook configurations are generated into
  `pkg/worker/webhooks/worker/zz_generated.webhooks.go` from the `+k8s:webhook-gen:` markers and
  installed by `pkg/worker/webhooks/setup.go` at worker startup; the worker's ServiceAccount is bound to
  `cluster-admin`, so there is no per-resource RBAC rule to add. `deploy/gpustack-operator/chart/**` is
  **not** in any task's `Owns`.
- **`make generate` is what installs the webhook.** The registration lives in the generated file, so the
  task that adds the webhook type is also the task that runs `make generate`, and its verification is
  that the generated diff shows the new webhook entry **and nothing else**.
- **`tenant_id` would bind to the reuse domain, not to the namespace — when it becomes reachable.**
  The impedance mismatch is real and unchanged: the store has **one** `tenant_id` carrying isolation,
  while the object model has two levels (the namespace's grant above, reuse domain below). Binding it
  to the domain is the correct choice, because two Pods in different namespaces that name the same
  domain are *supposed* to share cache. This is recorded as the **decided shape of a capability this
  spec does not yet deliver** (F4a): stating it now is what keeps the eventual fix from re-litigating
  a settled question, and the consequence — a namespace-level ceiling being our aggregate rather than a
  master-enforced number — belongs to the pool spec either way.
- **The reach of the gap extends past this webhook.** Anything downstream that keys on a per-tenant
  figure is describing a partition the engines do not currently create — every injected client lands in
  the `default` tenant, so per-domain accounting sees one tenant's traffic under many names. This spec
  does not fix that and does not own it; it is noted because a reader who sees per-domain fields
  elsewhere in this API group would otherwise assume they are already populated by engine traffic.
- **The SLO coupling to state.** `kv_lease_duration` defaults to **30 seconds**. It does not expire from
  long queueing, but it **does** expire when a Pod's heartbeat is interrupted — preemption, eviction,
  restart — and the default failure policy then fails the request outright. Anything that kills an
  injected Pod destroys cache its peers may be waiting on. This webhook creates that coupling by
  connecting a Pod to a pool, so the reference page states it; it does not attempt to manage it.
- **The projected client config is world-readable to anyone who can read the Pod.** It carries addresses,
  a protocol and a domain name — no credential. If a future backend needs one, it must not travel in an
  annotation; that is a boundary, not a preference.
- **The injection is not a Kueue gate and must not be described as one.**
  `docs/architecture/admission.md` documents a five-gate model; this webhook is deliberately outside it,
  and the page says so, or the next reader will count six gates and look for the quota this one charges.
- Go files are snake_case (`pod_kv_cache.go`); the synthesis package holds no cluster types, so it tests
  without a client.
- The webhook reads three objects per admitted Pod (Binding, pool, backend) with a 10-second timeout, on
  a path that only opted-in Pods take. Reads go through the manager's cache first, exactly as
  `PodWebhook` already does for its `InstanceType` lookup.

### Boundaries

- **Always:** trigger on a label and configure with annotations; resolve the Binding in the Pod's own
  namespace; **stamp** a declared domain the resolved engine cannot carry, and **refuse** when the pool
  does not report multi-tenancy on; render `mode` and `global_segment_size` as a pair on the vLLM family;
  write every key in the **engine's own** spelling; refuse rather than guess an engine, a container or
  an already-set mechanism key; leave a value variable the workload declared for itself alone; keep the
  mutation inside `env`, `args`, `volumes` and `volumeMounts` on one container; keep the synthesis a
  pure function that applies nothing.
- **Ask first:** anything that changes `failurePolicy`, adds a validating webhook on Pods, widens the
  `objectSelector`, touches the existing `PodWebhook`, changes `configurationPrefix`, makes the
  webhook write any object, or **marks an engine version as forwarding a tenant** — that last one flips
  every stamp from "not enforced" to "enforced", and getting it wrong makes the operator's one source of
  truth about isolation state a lie, which is worse than the gap it would be claiming to close.
- **Never:** emit a `tenant_id` key that no engine reads; fall back to the client's `'default'` tenant
  as if it were a domain; write a key in the `setup()` spelling; write a key the target engine's own
  reader does not read; set `global_segment_size` without `mode` on the vLLM family; render a
  `local_hostname` this webhook cannot know; pick a container when several are plausible; append to
  `args` when the container declares neither `command` nor `args`; merge with a user-set connector key;
  mutate `resources`; create a ConfigMap or Secret from a webhook declaring `sideEffects: None`; put a
  Binding name in a label value.

### Risks and Mitigations

- **The webhook is down and opted-in Pods cannot be created** (`failurePolicy: Fail`) → the selector
  bounds it to Pods that asked for a cache, the worker runs with more than one replica in the supported
  HA mode, and the alternative and its cost are recorded rather than assumed away.
- **A second Pod webhook collides with the existing one on name and path** → the distinct `namePrefix` is
  the fix, and a test asserts both entries are present with distinct names in the generated
  configuration, so a copy-pasted marker fails in CI rather than at install time.
- **The selector silently reverts to the queue-name label** in a later refactor, quietly reducing this
  webhook to serving only our own chain — the exact failure this spec exists to avoid → asserted
  directly in a unit test against the generated configuration.
- **The pool's status does not yet carry the multi-tenancy fact** → the gate refuses on "not reported
  yet" with a message that distinguishes it from "reported off", so the failure is a wait with an
  explanation rather than a wrong injection; the field itself is a named dependency in Open Questions.
- **An engine's flag or environment-variable name changes between versions** → the synthesis is a pure
  function with per-engine tables and per-engine tests, and every key is asserted against the **engine's
  own reader**, not against the client signature — the two disagree, and trusting the signature is what
  an earlier draft of this spec got wrong.
- **A user's `envFrom`-supplied Mooncake variable is silently overwritten by the injection** → a known
  limitation, not a defect to be fixed later: detecting it would mean reading a ConfigMap from a
  webhook that declares `sideEffects: None`, on a hot path, for a value that is stale by the time the
  kubelet resolves it. This repository already answers the same question the same way in
  `ascend/deviceplugin.go:316-317`. The mitigation is documentation plus a test that pins the behaviour
  so it cannot change unnoticed. What makes it worth stating rather than inheriting: the existing
  callers of `ContainerEnvDeclared` inject a log level, and this one injects a storage address.
- **A Pod is stamped as injected while its injection is inert** → this is the one accepted silent
  outcome in the design, and it has exactly one entrance: an SGLang container whose own
  `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` or `--hicache-storage-backend-extra-config` outranks the
  injected environment (`_load_config`, `mooncake_store.py:242-260`). The precedence is correct — an
  explicit user configuration should beat a defaulted one — so gating on it would reject a legitimate
  manifest. What is left is diagnosis: both entrances are named by name in the documentation, and the
  F7 stamp records the vehicle, so "the annotation says injected but the cache is cold" resolves to a
  one-line check rather than a support case.
- **The F4a version table goes stale in the permissive direction** — an engine is marked as
  forwarding a tenant when its build does not → **every stamp on that engine then lies in the
  reassuring direction**, claiming an isolation that is not there and naming a tenant the writes do not
  use. Nothing refuses, because F4a refuses nothing; the table's only consumer is the record, so a
  wrong entry does not fail, it corrupts the one place the fact is written. Mitigated by the table
  being `Ask first` in Boundaries, each entry carrying the source line it was read from, and the
  discriminating check being written beside it so a reviewer can re-run it. The stale-in-the-
  *restrictive* direction is the safe one: it under-claims, and a Pod recorded as not isolated when it
  is costs nothing.
- **An operator ships a patched engine that forwards a tenant but reports an unchanged version string**
  → the stamp under-claims for that deployment: it reads `isolated:false` and names the `default`
  tenant, while the engine is in fact writing under its domain. Nothing breaks — the writes succeed
  either way, since both names are registered — but the record is wrong in the direction of claiming
  less. Named in Open Questions rather than solved here; the webhook does not touch a user-supplied
  `MOONCAKE_TENANT_ID`, so such a deployment can carry its own.
- **A user's own tooling injects the same keys after us** (a second mutating webhook later in the chain)
  → out of our control and stated: the F7 stamp records what we wrote, so a divergence is diagnosable
  from the admitted object alone.
- **The transfer engine's random ports break a restrictive NetworkPolicy** → documented as a range
  requirement with the measured evidence, in the reference page, before the first user meets it.
- **A benign startup ERROR is read as a failure** → documented verbatim.

## Design Details

### Commands

Build, lint and test run **locally on darwin**; nothing in this spec touches cgo or a vendor library.
**Confirmed by a read-only smoke check before planning**: `go build ./pkg/worker/webhooks/...
./api/worker/...` succeeds and `go test ./pkg/worker/webhooks/worker/ -run TestPodWebhook` passes on
this machine, so the environment is reachable and the existing suite is green to build against.

```bash
go build ./pkg/... ./api/...
go test ./pkg/worker/kvcache/... ./pkg/worker/webhooks/...
make test                     # go test -v -failfast -race -cover -timeout=30m ./...
make lint                     # golangci-lint over the whole module
make lint docs
```

`make lint` is an **edit pass** over the whole module, not a read-only check, and a cold run can
exceed a two-minute timeout. Budget for it rather than cancelling it midway.

**Code generation runs in the main checkout, not in a worktree.** `make generate` derives package paths
GOPATH-style and requires a working directory ending in `gpustack.ai/gpustack`. Apply the source edit in
the main checkout, run the generator there, and sync the delta back; when syncing with `rsync`, use
`--filter='P .git'` and **not** `--exclude '.git/'` — a worktree's `.git` is a *file*, which the latter
misses, and combined with `--delete` it destroys the receiver's repository.

```bash
make generate                 # regenerates pkg/worker/webhooks/worker/zz_generated.webhooks.go
git diff --stat pkg/worker/webhooks/worker/zz_generated.webhooks.go
```

End-to-end runs on a local cluster (k3s or docker-desktop) via the project's chart, with the pool spec's
own Mooncake deployment as the fixture. Two images are involved, and only the first is required:

```bash
E2E_MOONCAKE_IMAGE   # default docker.io/kvcacheai/mooncake:0.3.13 — the client fixture, as case-44 uses
E2E_VLLM_IMAGE       # OPTIONAL; unset makes case-59 SKIP loudly rather than pass
```

`E2E_VLLM_IMAGE` is optional because the engine's `MooncakeStoreConfig` lives in `worker.py`, whose
import chain pulls all of vLLM and torch — several GB that a local cluster may not want to hold. The
case is written so an absent image produces an explicit "schema unverified" skip, never a silent pass.

### Project Structure

```
pkg/worker/webhooks/worker/
  pod_kv_cache.go              # PodKVCacheWebhook: markers, key constants, Default, resolution, refusals
  pod_kv_cache_test.go         # table-driven admission cases against a fake client
  zz_generated.webhooks.go     # regenerated: the new mutating entry
pkg/worker/webhooks/setup.go   # one line: the new handler in `setups`
pkg/worker/kvcache/inject/
  types.go                     # Engine, Role, Input, Result, the typed refusal          (T1)
  engine.go                    # the F4a version table + SupportsTenant                  (T1)
  inject.go                    # Input -> Result (env, args, volumes, mounts) or refusal (T2)
  client_config.go             # the config file, keyed to the ENGINE's file schema      (T2)
  vllm.go                      # the vLLM / vllm-ascend synthesis                        (T2)
  sglang.go                    # the SGLang synthesis                                    (T2)
  *_test.go                    # pure, no cluster, no engine
docs/reference/kv-cache-injection.md   # the label/annotation contract, the injected keys, the refusals
docs/architecture/admission.md         # one paragraph: a Pod webhook outside the five gates
```

### Code Style

The synthesis is a pure function over a value: no client, no context, no cluster types, so every case in
the Test Plan is a table row.

```go
// Input is everything the synthesis needs, already resolved and already validated. It holds no
// Kubernetes client and no cluster types on purpose: resolving is the webhook's job, and rendering is
// this package's, so the rendering tests need neither a cluster nor an engine.
type Input struct {
	// Engine selects the synthesis. It is never inferred from an image: different engines take
	// completely different flags, and a renamed or vendored image would mis-inject silently.
	Engine Engine
	// The reuse domain is deliberately NOT here. F4a stamps rather than refuses, so the synthesis
	// has no decision to make about a domain: it is never written into the config (a tenant_id no
	// engine reads would look like a guarantee and be none), and the verdict that DOES depend on it
	// belongs to the webhook, which is where the stamp is written. Carrying it here would be a field
	// this package reads for nothing.
	//
	// Role is the prefill/decode role, empty when the caller has none. It selects the connector's
	// role for the vLLM family; for SGLang a non-empty Role is refused rather than ignored.
	Role Role
	// Connection is what resolution already established: the master address from the pool's
	// clientEndpoint, and the transport from the backend, already in the artifact's own spelling.
	// It carries no metadata-plane address and no device list, because neither is a resolved value:
	// the metadata plane is a constant this package writes, and the device filter is left empty so
	// the client discovers per host.
	Connection Connection
}

// Result is the mutation, expressed as what the container ends up with rather than as a sequence of
// edits, so a test asserts the final container instead of the calls that built it.
type Result struct {
	Env          []core.EnvVar
	Args         []string
	Volumes      []core.Volume
	VolumeMounts []core.VolumeMount
	// ClientConfig is the rendered client JSON for the engines whose vehicle is a file. It is empty
	// for the engines whose vehicle is args and env; the webhook projects it from the Pod's own
	// annotation, so nothing here creates an object.
	ClientConfig string
}
```

Conventions: a refusal is a typed error carrying the annotation or container it names, so the webhook
renders one actionable message and the tests assert the reason rather than the prose; every engine table
is a `var` in its own file, so adding an engine is a file, not a branch in a shared function; the config
file's keys are constants checked against the **engine's own config class** — never against the
`setup()` signature, which spells two of them differently and is the mistake this spec already made
once.

### Implementation Plan

Two foundations start in parallel — the engine facts and the webhook shell — and the DAG stays two
lanes wide for most of its length: the `inject` package and the `webhooks` package touch disjoint
paths, so the renderers and the resolution build side by side. Checkpoints: after T1+T3 (the facts are
pinned and the webhook is registered but injects nothing); after T5 (a Pod is injected or refused,
unit-tested); after T6 (the invariants that silently rot are pinned); after T8 (a plain `Deployment`
reads and writes the pool, and a domain-carrying one is refused).

- [x] **T1 · Engine facts: the types, the F4a version table, and `SupportsTenant`**
  Blocked by: None
  Owns: `pkg/worker/kvcache/inject/engine.go`, `pkg/worker/kvcache/inject/types.go`,
  `pkg/worker/kvcache/inject/engine_test.go`
  Gate: review
  Acceptance: `Engine`, `Role`, `Input`, `Result` and the typed refusal error; plus the F4a table as a
  `var` keyed by engine, each entry carrying the measured version, whether that version forwards a
  tenant, and — in a comment on the entry — the source file and line the fact was read from.
  `SupportsTenant(Engine) bool` reads only that table. **Every entry is `false` at this commit**, and a
  test asserts that the refusal path is reachable for each engine, so the gate cannot be silently
  disabled by a table that is empty or all-true. `Input.Domain` is carried but never rendered.
  Verify: `go test ./pkg/worker/kvcache/inject/ -run 'Engine|SupportsTenant'`

- [x] **T2 · The renderers: the vLLM file, the SGLang environment**
  Blocked by: T1
  Owns: `pkg/worker/kvcache/inject/inject.go`, `pkg/worker/kvcache/inject/client_config.go`,
  `pkg/worker/kvcache/inject/vllm.go`, `pkg/worker/kvcache/inject/sglang.go`,
  `pkg/worker/kvcache/inject/inject_test.go`, `pkg/worker/kvcache/inject/client_config_test.go`
  Gate: review
  Acceptance: one exported entry, `Render(Input) (*Result, error)`, a pure function that **applies
  nothing**. `Result` carries `Env`, `Args`, `Volumes`, `VolumeMounts` and `PodAnnotations`; the caller
  applies them to whatever object it owns — the webhook to a `Pod`, another caller to a workload's Pod
  template. `Render` is written to be called from outside this webhook, because it is this package that
  owns Mooncake client-config rendering for the whole repository.
  Cases cover all three engine values against all three role values, asserting the **final** `Result`
  rather than intermediate calls. Keys are **each engine's own**, never the `setup()` signature's:
  `master_server_address` not `master_server_addr`, `device_name` not `rdma_devices`.
  For the vLLM family the three role-declaring keys are asserted **together**, since any one alone
  changes what the instance is: `global_segment_size: 0`, `mode: "standalone-store"`, and
  `local_buffer_size` at the `128 MiB` constant — above zero, or the instance becomes a pure server that
  may not Get or Put. For `sglang` neither `mode` nor `local_buffer_size` is emitted in any spelling,
  and `MOONCAKE_LOCAL_HOSTNAME` is a `fieldRef` to `status.podIP` rather than a literal.
  `metadata_server` is `P2PHANDSHAKE` on both, and SGLang's variable name is asserted byte-for-byte as
  `MOONCAKE_TE_META_DATA_SERVER`. `protocol` is always written; `device_name` is written empty on every
  path. **No `tenant_id` key in any output**, on any input — `Input` carries no domain at all, so there
  is no shape of call that could produce one. A role on `sglang` returns a typed refusal.
  `Render`'s doc comment states that its `Env` entries are **desired** values: a caller that finds the
  workload already declaring a variable of the same name leaves the workload's value in place, per this
  repository's standing rule that an injection never overrides what a workload declared for itself. The
  package doc comment records the source lines the per-engine vehicle choice rests on
  (`worker.py:144-151`, `mooncake_store.py:114-141,242-260`, `environ.py:41-42,54-72`,
  `rdma_utils.py:21-25`). No cluster, no engine, no Kubernetes client.
  Verify: `go test ./pkg/worker/kvcache/inject/...`

- [x] **T3 · The webhook type, its markers, and the generated registration**
  Blocked by: None
  Owns: `pkg/worker/webhooks/worker/pod_kv_cache.go`, `pkg/worker/webhooks/setup.go`,
  `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
  Gate: review
  Acceptance: a `PodKVCacheWebhook` implementing `Defaulter` only, registered in `setups`, with
  `objectSelector` `kvcache.gpustack.ai/inject In ["true"]`, `failurePolicy: Fail`,
  `reinvocationPolicy: Never`, `sideEffects: None`, `operations: ["CREATE"]`, and
  `namePrefix: "gpustack-worker-kvcache"`. `Default` is a no-op stub at this task. The generated
  configuration carries **two** Pod mutating entries with distinct names and distinct paths, inside the
  single `gpustack-worker-mutation` object.
  Verify: `make generate` from the main checkout, then
  `git diff pkg/worker/webhooks/worker/zz_generated.webhooks.go` shows the new webhook entry and
  nothing else; `go build ./pkg/...`

- [x] **T4 · Resolution, the isolation verdict, and the master gate**
  Blocked by: T1, T3
  Owns: `pkg/worker/webhooks/worker/pod_kv_cache.go`,
  `pkg/worker/webhooks/worker/pod_kv_cache_resolve.go`,
  `pkg/worker/webhooks/worker/pod_kv_cache_resolve_test.go`
  Gate: review
  Acceptance: the Binding is read from the Pod's own namespace (cached client, then `APIReader`), the
  pool supplies the master address from `status.clientEndpoint`, and the backend supplies the transport
  through `mooncake.MemberProtocol`. The domain is `spec.domain.name` on the Binding and nothing else —
  never an annotation, which F1 refuses outright — and its **shape is not re-judged here**, since the
  Binding's own webhook is the one place it is checked.
  **F4a stamps, F4b refuses.** F4a produces an isolation verdict rather than an error: a declared domain
  against an engine `SupportsTenant` reports as non-forwarding yields "declared, not enforced" plus the
  engine version behind it, and the Pod proceeds. It is not a gate because every Binding declares a
  domain, so a gate here would refuse every Pod; the matching refusal belongs on the Binding side and is
  carried as D8. F4b refuses: the pool reporting multi-tenancy **off** (`QuotaLedgerAvailable` False /
  `MultiTenancyDisabled`), and the pool **not yet reporting** it (condition absent or `Unknown`), each
  with its own message.
  A test pins that the verdict is not a constant: the same Pod against a table entry marked as
  forwarding yields "enforced". Both paths inject, so a verdict hard-coded either way would leave every
  test passing and the distinction unobservable.
  Refusals, each with its own message: no binding annotation; a `/` in the value; the Binding absent;
  the pool absent; the domain annotation set on the Pod.
  Verify: `go test ./pkg/worker/webhooks/worker/ -run KVCache`

- [x] **T5 · The remaining refusals, and the injection itself**
  Blocked by: T2, T4
  Owns: `pkg/worker/webhooks/worker/pod_kv_cache.go`,
  `pkg/worker/webhooks/worker/pod_kv_cache_inject.go`,
  `pkg/worker/webhooks/worker/pod_kv_cache_inject_test.go`
  Gate: review
  Acceptance: several containers and none named → refused; a named container that does not exist, or
  that is an init container → refused, naming the candidates; a key that **selects the mechanism**
  already present in `env`, `args` or `command`, or an owned volume name or mount path already taken →
  refused, naming the key; a container declaring **neither** `command` nor `args` → refused, naming
  `args` as the field to fill. An unrecognised `kvcache.gpustack.ai/` annotation → refused, and one of
  another tool's prefixes left alone. A user-set `MOONCAKE_TENANT_ID` or
  `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` is **not** a conflict, and a user-set value variable yields
  without costing the rest of the injection.
  Otherwise the target container carries the synthesis, the Pod carries the client-config and
  injected-stamp annotations, and for the file vehicle the projection comes from the Pod's own
  annotation — no object is created. **The stamp is JSON carrying the Binding, the engine and version
  judged, the vehicle, the declared domain and whether it is enforced.** Its isolation field is
  asserted against a **substituted** table entry as well as a shipped one: both inject, so an assertion
  using only shipped engines passes equally against a hard-coded verdict.
  A refusal leaves the Pod byte-identical, and a success changes only env, args, volumes, mounts and
  the two annotations — asserted by diffing the whole object, never `resources`.
  Verify: `go test ./pkg/worker/webhooks/worker/ -run KVCache`

- [x] **T6 · The invariants that would otherwise rot silently**
  Blocked by: T5
  Owns: `pkg/worker/webhooks/worker/pod_kv_cache_invariant_test.go`
  Gate: review
  Acceptance: assertions against the generated configuration and the admitted object — the new entry's
  `objectSelector` is the `kvcache.gpustack.ai/inject` label and **not** `kueue.x-k8s.io/queue-name`;
  the two Pod mutating entries have distinct names and paths; both live in one configuration object; a
  Pod without the label, and a Pod with the label set to `"false"`, come out of `Default` deep-equal to
  the input. Plus the disjointness test: `PodWebhook.Default` and `PodKVCacheWebhook.Default` over one
  Pod, in both orders, produce the same object, and the injection touches no path outside `env`,
  `args`, `volumes`, `volumeMounts` and its two annotations.
  Verify: `go test ./pkg/worker/webhooks/worker/...`

- [x] **T7 · Documentation**
  Blocked by: T5
  Owns: `docs/reference/kv-cache-injection.md`, `docs/architecture/admission.md`, `docs/README.md`
  Gate: review
  Acceptance: a reference page carrying the label and annotation contract, the injected keys per engine
  with the vehicle and the reason for it, and every refusal with its message and its fix. **The tenant
  gap is documented as a first-class operational fact**, not a footnote: what the F4a stamp reports and
  how to query it, why the gap exists, the version table, the upstream fix that lifts it, and the
  Binding-side check (D8) that has not landed — so a reader learns that isolation is currently declared
  rather than enforced from the documentation, not from an incident. Plus the random-port behaviour stated as a
  **range** requirement for any NetworkPolicy, the benign `Local segment descriptor not found` startup
  ERROR, and the 30-second lease coupling. **The client buffer gets its own stated number, not a
  vague coupling**: the injected `local_buffer_size` is host memory held inside the container's own
  limit, the engine's default is GiB-scale (4 on vLLM, 1 on vLLM-Ascend), and a container whose limit
  was sized for a model alone will OOM — so the page says what to add to the limit and where the
  value comes from. One paragraph in
  `docs/architecture/admission.md` placing this webhook **outside** the five gates. The index links
  both.
  Verify: `make lint docs`

- [x] **T8a · The seven case scripts, written and NOT YET EXERCISED**
  Blocked by: T5, T7
  Owns: `.claude/skills/gpustack-operator-e2e/cases/case-53.sh` … `case-59.sh`
  Gate: a local cluster with the pool running, multi-tenancy on, TCP transport
  Acceptance: the seven Verification cases, in order, one file each — case numbers **53–59**, the next
  free block above the 44 files on `main`. Every verdict goes through the shared `lib.sh` helpers
  rather than a case deciding its own, which preflight refuses.
  Case 1 (`case-53.sh`) is the gate: a plain `Deployment`'s Pod moves the master's used-bytes figure
  and reads back what it wrote. Its fixture calls `setup()` **positionally with seven arguments**,
  mirroring `worker.py:1040-1048` — never the dict overload, which would forward a tenant the real
  engine truncates and make case 3's refusal look over-cautious.
  Case 3 (`case-55.sh`) is the honesty gate: a domain-carrying Binding against a non-forwarding engine
  is **injected and stamped as not isolated**. The assertion is on the stamp's isolation field, and it
  is paired with a control that the same field reads differently for a forwarding table entry -
  otherwise a stamp hard-coded to "not isolated" would pass. Case 5 asserts a message, not just a
  non-zero exit.
  Case 7 (`case-59.sh`) carries **both vehicles**, each **gated on its own engine image**
  (`E2E_VLLM_IMAGE`, `E2E_SGLANG_IMAGE`), and each half SKIPs independently: one image present must not
  let the other half report green by omission. With a vLLM image it feeds the rendered file to
  `MooncakeStoreConfig.from_file` and asserts `__post_init__` does not raise. With an SGLang image it
  calls that engine's own `_load_config` — never `os.environ` directly, which would be green even if the
  file branch had swallowed the injection — and asserts the returned config carries the Pod's IP as
  `local_hostname`, not `"localhost"`. A SKIP is **loud**, reporting the schema as unverified against
  that engine: the Go tests prove we render what we believe is right, and only this case proves an
  engine accepts it.
  Verify (T8a): `bash -n` on every file, plus the cluster-independent half executed against a stub
  `kubectl` — the results table and its exit status, all three branches of the refusal helper, the
  manifest generator's YAML, the stamp parser including an absent record, and the skip accounting.
  A follow-up sweep applies one grep-able rule to every `|| continue`, `|| true` and `2>/dev/null` in
  the family: if this swallows a failure, would the summary still claim the check happened? Of 53
  sites, 51 are safe (a swallowed read yields an empty value that the assertion then fails on, and a
  swallowed cleanup cannot reach the summary at all); two were not, and both are fixed.
  That harness found four real defects in total: a run of nothing but SKIPs printed "all checks passed", and
  case 55's control loop passed over an engine whose Pod never appeared - reporting PASS with the
  words "all three" having observed none of them; case 58 read an empty `reinvocationPolicy` as "unset,
  so the default applies" when it is equally what a MISSING ENTRY returns, which would have reported
  PASS about a webhook that is not installed, in the one case whose purpose is catching that; and case
  59 reported an empty probe log as "the config path is set", naming a defect in the injection when
  the actual fault was a probe that never ran. All four were found after the files were written and
  syntax-clean, and none is visible by reading: each line is unremarkable on its own.
  Every case header states the condition under which it SKIPS, including the five that never do,
  because "this one cannot skip" is itself a property worth writing down where a reader looks.

- [x] **T8b · Run the seven cases on a live cluster**
  Blocked by: T8a — and a deployable pool
  Gate: review
  **T8a does not close T8, and this spec may not ship until T8b does.** A case that has never been
  executed is a case whose defects are exactly the ones execution finds, and it reads as finished
  either way. The precedent is in this programme: a sibling spec's case was written, never run, and
  an external review then found that its restart check passed without a restart having happened.
  Acceptance: each of the seven run against a cluster with the operator installed and a Mooncake
  image reachable, with the output recorded. Case 59's halves may SKIP if no engine image is
  available, and a SKIP is reported as unverified rather than as a pass.
  Verify: the recorded run output — the metric before and after, the refusal messages, and per half
  either the parse result or an explicit SKIP

  **CLOSED 2026-09-05, on a single-node Kubernetes cluster with no accelerator, against the merged
  revision of every case.** 53 passes 5/5. 54 passes its bare-Pod half and SKIPs the LWS half for a
  missing CRD. 55 passes 7/7. 56 passes 4/4, its positive control included. 57 passes 16/16 — the
  fifteen refusals plus the terminating-Binding one, each now also asserting that the refusal came
  from this webhook and not merely from something. 58 passes 8/8. 59 SKIPs both halves, reported as
  `2 check(s) SKIPPED and 0 passed - the skipped ones verified NOTHING`, which is the clause above
  working: a SKIP lands in the SKIP count, never in the pass count.

  What that leaves open, deliberately and not by omission: the engine half — whether an engine
  actually runs with the configuration this webhook injects — is unanswerable without an
  accelerator, and case 59's two SKIPs are that gap reported rather than hidden. The gap, and the
  five things that would look like closing it without closing it, are recorded in the headers of
  cases 59 and 60 and in issue #172.

  Three annotations expired on the way and one did not. Case 57's multi-tenancy restore ran and
  converged, its first execution ever. The trap ordering that only matters when setup fails was
  exercised by forcing a setup failure through an environment variable, so nothing had to be edited
  to reach it. Case 53's readiness wait ran its success branch only; its failure branch still has
  not run, and a passing run is evidence for the branch it took and for no other.

  **REOPENED 2026-09-04.** The seven all ran and passed, and then a review showed one of them had
  passed over a wrong fact (see below). The cases were amended - SGLang now receives and asserts a
  tenant, vLLM's half matches its whole output line rather than a prefix - and the amended cases have
  not been run. A run of the previous revision is not evidence for this one.

  **First pass, all seven on a three-node k3s cluster.** 53 passes 5/5, 54 passes its bare-Pod
  half and SKIPs the LWS half for a missing CRD, 55 passes 7/7, 56 passes 4/4, 57 passes 12/12, 58
  passes 7/7, 59 passes both halves.
  **A second, harder justification arrived after this task was first marked done.** An external review
  found the F4a facts table wrong for the SGLang build this project deploys — and case 59 had passed
  over it. Two defects had to combine for that: the table was stale, AND the case printed five branch
  booleans that no pass condition ever read. Either alone would have been caught; together, the case
  went green across the exact fact it existed to check. Worse, the case's own header carried an
  honest caveat — "if this half fails, you cannot tell upstream drift from a wrong table entry" — and
  that caveat was never triggered, because the half never failed. **A boundary note only means
  something when the check it qualifies can actually fail.**

  **Four of the seven failed on their first real run, not one of them for the reason it printed, and
  three of those four were defects in the case rather than in the webhook.** That ratio is the
  retrospective justification for splitting T8: an unexecuted case does not merely lack evidence, it
  misreports — and three quarters of what these would have misreported were faults in the test, the
  class that survives review by looking like a finding. Case 53 reported a failed round-trip when the
  cluster was missing a prerequisite, then reported zero usage that was the correct answer to the
  wrong question; case 56 could not have passed whatever the webhook did, because it compared fields
  the API server defaults onto every Pod; case 59 reported a timeout that was a 19.1GB image pull.
  Case 58 was additionally run BEFORE the webhook was deployed and observed to fail, which is what
  makes its later pass mean something.
  **Case 59 is what moves D8 from read to observed.** vLLM printed
  `PARSED mode=standalone-store segment=0 buffer=134217728` — two of the three differ from that
  engine's own defaults, so the file was read rather than fallen back from. SGLang reported
  `BRANCH_ENV=True` with the file and extra_config branches both False, `local_hostname` as the Pod's
  IP rather than `localhost`, and a control confirming the extra_config branch does win when
  populated — which is what makes "we must not occupy `extra_config`" a decision with content rather
  than a stated caution. Both probes ran with no GPU and no CUDA runtime present.
### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/worker/webhooks/worker` already builds fake clients with
  `ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(...)`; the KV cache cases add
  fixture builders for a Binding, a pool reporting multi-tenancy on, and a pool reporting it off, in the
  same style as `instanceTypeWithEntrance`.
- A Pod fixture builder parameterised by container count, opt-in label value and annotation set, so the
  refusal table is data.
- No new fake is needed for the synthesis package: it takes a value and returns a value.
- **The F4a table needs a test-only entry marked as forwarding.** Every shipped entry is
  non-forwarding, so without one the "injects when the engine forwards" path has no fixture and the
  gate would be tested in one direction only — which is how a gate that always refuses passes its
  own suite. The fixture injects the table rather than mutating the package `var`, so the shipped
  values stay the shipped values.

#### Unit tests

Per-package targets. The baseline was measured on this branch before any of this spec's code existed,
so the second column is what the work has to hold rather than a number to discover afterwards:

- `pkg/worker/kvcache/inject`: `2026-09-03` - baseline `0%` (package does not exist) → target `≥90%`.
  It is a pure package with no client, no context and no cluster types, so anything below that is
  untested branches rather than untestable ones.
- `pkg/worker/webhooks/worker`: `2026-09-03` - baseline `83.3%` → target **no regression**. The new
  webhook adds a large refusal surface that is cheap to cover, so a drop here means the refusals are
  under-tested, not that the denominator grew.

**Synthesis** (`pkg/worker/kvcache/inject`) — one row per (engine, role), asserting the final container:

| Case | Input | Expected |
|---|---|---|
| `vllm_no_role` | engine `vllm`, no role | `--kv-transfer-config` with `kv_both`; `MOONCAKE_CONFIG_PATH`; one volume, one read-only mount |
| `vllm_prefill` / `vllm_decode` | roles | `kv_producer` / `kv_consumer`, everything else identical |
| `vllm_ascend` | engine `vllm-ascend` | the vLLM result, produced by its own table entry |
| `sglang_no_role` | engine `sglang` | `--hicache-storage-backend mooncake`; the `MOONCAKE_*` variables; **no** `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, no volume, no mount |
| `sglang_local_hostname_is_a_field_ref` | engine `sglang` | `MOONCAKE_LOCAL_HOSTNAME` is a `fieldRef` to `status.podIP`, never a literal — a literal is a value this webhook cannot know |
| `sglang_with_role` | engine `sglang`, role set | typed refusal, not a silently ignored role |
| `keys_are_the_engine_s_own` | each engine | every key is one that engine's own reader reads — `master_server_address`, **not** `master_server_addr`; `device_name`, **not** `rdma_devices`. Mutation test: rename a key to the `setup()` spelling and assert the case **fails**, since `config.get` would otherwise swallow it silently |
| `no_key_outside_the_schema` | each engine | the rendered key set is a subset of **that engine's** reader — vLLM's eight, SGLang's ten. An extra key is unreachable code, not a harmless addition |
| `vllm_role_declaring_keys_move_together` | the vLLM family | the **three** role keys asserted as one unit: `global_segment_size: 0`, `mode: "standalone-store"`, `local_buffer_size` at the constant. They declare *what the instance is*, not how big it is, so no case asserts one without the others |
| `sglang_has_no_mode_or_local_buffer` | engine `sglang` | neither key is emitted, in any spelling. SGLang's reader has no `mode` and no `local_buffer_size`, and hardcodes its own 16 MiB buffer, so emitting either would be a key nothing reads |
| `local_buffer_size_is_nonzero` | the vLLM family | strictly above zero — a zero declares a pure server that may not `Get` or `Put`, which is a silently useless client rather than an error |
| `no_tenant_id_emitted` | every engine | **no `tenant_id` key in any vehicle** — writing one would read as a guarantee the stack cannot keep (F4a). There is no domain dimension to vary: `Input` carries none, which is the structural version of the same guarantee |
| `device_name_empty_every_path` | TCP **and** RDMA protocol, each engine | present and empty in both — empty means "discover per host", and `auto-discovery` is a filter no host matches |
| `metadata_server_literal` | each engine | the value is `P2PHANDSHAKE`, never an address |
| `metadata_variable_spelling` | engine `sglang` | the variable is byte-for-byte `MOONCAKE_TE_META_DATA_SERVER` — see the trap below |
| `protocol_written_explicitly` | each `spec.transport.protocol` value, each engine | resolved through `mooncake.MemberProtocol`; **always written**, because an absent `protocol` defaults to `"rdma"` on vLLM and `"tcp"` on SGLang — omitting it would silently select the wrong transport on one engine or the other |
| `unknown_engine` | an unlisted value | typed refusal naming the accepted values |

**The `META_DATA` spelling trap is back in scope, and that reverses what this spec said before.**
`MOONCAKE_TE_META_DATA_SERVER` carries an underscore the readable `METADATA` does not, and the wrong
spelling degrades the metadata plane silently instead of erroring. An earlier draft ruled it out of
scope, on the reasoning that the trap lives in the environment vehicle while both engines read files —
which was true of the design at that moment and is not true of this one. SGLang now takes environment
variables, so this renderer emits that exact variable and the trap is reachable from it.

`metadata_variable_spelling` therefore asserts the name byte-for-byte rather than through a constant,
because a constant with the wrong spelling in it would satisfy any test written against the constant.
The vLLM family keeps writing the JSON key `metadata_server`, which carries no such trap and is covered
by `keys_are_the_engine_s_own`.

The assertion is modelled on the one already guarding the trap where it was previously the only
reachable place: `TestMemberWorkload_Environment`
(`pkg/worker/kvcache/mooncake/member_workload_test.go:124`, assertion at 137–139), which covers the
member renderer. Two renderers now emit that variable, and both are pinned the same way.

**Admission** (`pkg/worker/webhooks/worker`) — every refusal is the point; a wrong-but-plausible
injection is far worse than none:

| Case | Fixture | Expected |
|---|---|---|
| `no_label` | no opt-in label | object deep-equal to input |
| `label_false` | `inject: "false"` | object deep-equal to input |
| `binding_annotation_missing` | label only | refused, names the annotation |
| `binding_namespaced_value` | `other-ns/shared-kv` | refused; no cross-namespace form |
| `binding_absent` | names a Binding that does not exist | refused, names Binding and namespace |
| `pool_absent` | Binding resolves, pool does not | refused, names the pool |
| `multi_tenancy_off` | pool carries `QuotaLedgerAvailable` False / `MultiTenancyDisabled` | refused, names the pool and the flag |
| `multi_tenancy_unreported` | the condition is absent | refused, message distinct from `multi_tenancy_off` |
| `multi_tenancy_unknown` | the condition is `Unknown` | refused as unreported — Unknown is a wait, never an "on" |
| `external_backend_ledger_off` | an **external** backend, which pool admission never gates | refused here; this is the case the pool's own webhook lets through by design |
| `domain_against_truncating_engine` | a Binding declaring a domain; engine marked truncating in F4a's table | **injected, and stamped as not isolated** — the stamp names the domain, says it is not enforced, and gives the engine version as the reason. Refusing here would make one namespace's Binding stop another namespace's Pods |
| `domain_against_forwarding_engine` | the same Binding; a table entry marked as forwarding | injected, and stamped as **isolated** — proves the stamp is driven by the table and will change on its own when an engine is fixed, rather than being hard-coded |
| `stamp_isolation_is_not_a_constant` | the two rows above, compared | the isolation field **differs** between them. Both rows inject, so a stamp that read the same either way would make the whole distinction unobservable while every test still passed |
| `f4b_refuses_even_when_f4a_would_stamp` | truncating engine **and** a pool reporting multi-tenancy off | **refused**, with F4b's message: F4a no longer refuses anything, so there is no precedence left to arbitrate, and the pool the Pod asked to join is genuinely unusable |
| `domain_annotation_refused` | `kvcache.gpustack.ai/domain` set on the Pod | refused, naming the key; the value is never read |
| `domain_empty_is_not_a_refusal` | a Binding whose domain name is empty — only reachable for an object that never went through Binding admission | **injected**, stamped as declaring no domain. It was a refusal in an earlier draft, to keep the webhook from falling back to the client's `'default'` tenant; that concern is gone, because this webhook writes no tenant in any form. Refusing here would be a gate whose trigger no admitted object can satisfy |
| `engine_missing` / `engine_unknown` | no or bad engine | refused, names the accepted values |
| `single_container` | one container, no container annotation | injected into it |
| `two_containers_unnamed` | two containers | refused; never the first |
| `two_containers_named` | container annotation names the second | injected into the second only |
| `named_container_absent` | names a container that does not exist | refused, lists the candidates |
| `named_init_container` | names an init container | refused |
| `conflict_env_config_path` | engine `vllm`, `MOONCAKE_CONFIG_PATH` already set | refused, names the key |
| `sglang_config_path_not_refused` | engine `sglang`, `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` already set | **injected, left alone** — the webhook stopped writing this key, and a user who sets it has configured SGLang from a file of their own. The injection yields to it silently; only the documentation says so |
| `user_tenant_id_not_refused` | the user sets `MOONCAKE_TENANT_ID` themselves | **injected, left alone** — the webhook does not write this key, so refusing over it would block the one workaround a patched-engine operator has |
| `sglang_value_var_user_set` | engine `sglang`, the user sets `MOONCAKE_PROTOCOL` | **injected, that one variable left at the user's value** — the rest of the injection still lands, per `ContainerEnvDeclared` |
| `conflict_arg_kv_transfer` | `--kv-transfer-config` in `args` | refused |
| `conflict_command_kv_transfer` | the same flag in `command` | refused — `command` is scanned too |
| `conflict_arg_hicache_backend` | engine `sglang`, `--hicache-storage-backend` already in `args` | refused; the flag selects the mechanism |
| `no_command_no_args` | the target container declares neither | refused, and the message names `args` as the field to fill — appending would discard the image's `CMD` |
| `conflict_volume_name` / `conflict_mount_path` | engine `vllm`, ours already taken | refused |
| `sglang_no_volume_injected` | engine `sglang`, a successful injection | no volume, no mount, no client-config annotation — the environment is the whole vehicle |
| `conflict_envfrom_only` | the user supplies `MOONCAKE_MASTER` from an `envFrom` ConfigMap | **injected, and the injected value wins** — the case exists to pin the known limitation rather than to bless it, so the day someone teaches the webhook to read `envFrom` this test fails and makes them read why it did not |
| `observability_user_set` | `MC_TE_METRIC=0` set by the user | left alone, no refusal |
| `observability_default` | absent | both metric variables set on |
| `unknown_kvcache_annotation` | `kvcache.gpustack.ai/domian` | refused |
| `reserved_annotation_supplied` | the user sets `kvcache.gpustack.ai/client-config` or `.../injected` | refused; the record of what the webhook decided is never user-supplied |
| `stamp_present` | a successful injection | the stamp parses as JSON and carries the Binding, the engine and the version F4a judged it at, the vehicle, the declared domain, whether it is enforced, and the tenant the writes actually land on — so a `-o jsonpath` query can select any one |
| `only_allowed_paths_change` | a successful injection | the diff touches only env, args, volumes, mounts and the two annotations |
| `cold_cache_falls_back` | the cached client misses, the reader has it | injected, not refused |

**Generated configuration** (`pkg/worker/webhooks/worker`):

| Case | Expected |
|---|---|
| `selector_is_inject_label` | the new entry selects `kvcache.gpustack.ai/inject`, not `kueue.x-k8s.io/queue-name` |
| `two_pod_entries_distinct` | both Pod mutating entries present, distinct names, distinct paths |
| `single_configuration_object` | both live in `gpustack-worker-mutation`; no second configuration object |
| `failure_policy_fail` | the new entry is `Fail`, `Never`, `None`, `CREATE` |

#### Integration tests

- `PodWebhook.Default` and `PodKVCacheWebhook.Default` over one Pod carrying both a queue-name label and
  the injection label, run in both orders: the same final object, and neither webhook's fields touched
  by the other.
- The full resolution chain against a fake client holding Binding, pool and backend: one admitted Pod
  whose rendered client config matches, field for field, what the synthesis package produces for the
  same inputs — so the webhook cannot drift from the tested renderer.
- **The verdict reads the table, and the table alone.** With a test-only table entry marked as
  forwarding, the same domain-carrying Binding that stamps "not enforced" against a shipped engine
  stamps "enforced" — proving F4a is data-driven and will follow an engine that gets fixed, rather than
  being a hard-coded answer that outlives its reason. Both paths inject, so this pairing is the only
  thing that makes the verdict observable at all: without it, a constant would pass every test.

#### e2e tests

The seven Verification cases on a local cluster, as **T8**, in `case-53.sh` … `case-59.sh`.

**Two of them fail the spec if they fail, and they fail it in opposite directions:**

- **Case 1 (`case-53.sh`)** is the difference between "the configuration is present" and "the Pod is
  using the pool". Its fixture calls `setup()` positionally with seven arguments, mirroring
  `worker.py:1040-1048`; a fixture using the dict overload would be more capable than the engine it
  stands in for and would quietly invalidate case 3.
- **Case 3 (`case-55.sh`)** is the difference between shipping connectivity and *claiming* isolation: a
  Binding declaring a domain, against a non-forwarding engine, is injected and its stamp must say the
  domain is not enforced. If this case ever passes with a stamp claiming isolation, the spec has started
  lying. It is checked against a control that flips the table entry, because the assertion is on a field
  whose wrong value is a constant.

**Case 7 (`case-59.sh`) is gated and skips loudly.** With `E2E_VLLM_IMAGE` set it parses the rendered
file with the engine's own `MooncakeStoreConfig.from_file` and asserts `__post_init__` does not raise —
the `mode`/`global_segment_size` pair is what it really tests, since getting that wrong aborts the
engine at startup. Without the image it reports **SKIPPED / schema unverified against the real
engine**, never green. The Go tests cover the same keys, but they compare our renderer against our own
reading of the schema; only this case closes that loop, and pretending otherwise would make a
self-consistent copy look like a verified contract.

When the workload CR exists, one more case pins the equivalence Open Questions raises — the same
declaration through the workload CR and through this webhook produces the same container.

## Alternatives

- **Extend the existing `PodWebhook`.** Impossible, and this is the constraint the whole API shape
  follows from: its `objectSelector` requires `kueue.x-k8s.io/queue-name` to exist, so it fires only on
  Pods entering the Kueue chain, and this spec exists to serve Pods that do not. Relaxing that selector
  instead would send every Pod in the cluster through the accelerator webhook.
- **Refuse the Pod when its pool carries two or more distinct reuse domains.** Rejected on where the
  refusal lands, not on cost. It was cheap: the field index `IndexingKVCachePoolBindingByPool` already
  exists (`pkg/worker/controllers/worker/kv_cache_pool.go:427-430`), the Binding's own webhook already
  lists Bindings cluster-wide at admission with the cached-then-`APIReader` fallback
  (`kv_cache_pool_binding.go:187-192`), and the webhook and the controllers share the manager the
  index is registered on (`pkg/worker/worker.go:170,227`, with `controllers.Setup` running before
  `Manager.Start`). Two things are wrong with it anyway.
  **The scope is one level off:** the harm is bounded by the `KVCacheBackend`, not the pool, because one
  backend may serve several pools (`kv_cache_pool.go:59-60`) and two Bindings on different pools sharing
  a backend collide identically. A per-pool check cannot see them.
  **The refusal lands on the wrong actor:** the Pod's author did not create the second domain. Refusing
  them means one namespace's Binding stops another namespace's Pods from starting - a cross-tenant
  denial of service whose victim did nothing wrong and can fix nothing. The action that introduces the
  harm is creating that second domain, so that is where the refusal belongs, which puts it on the
  Binding's webhook. Carried as D8, where the same three facts above are what that follow-up will need.
- **Put the Binding name in the trigger label.** Rejected: a label value is capped at 63 characters and
  restricted to alphanumerics plus `-_.`, while an object name may be a DNS subdomain up to 253
  characters. A fixed-value label plus an annotation has no such cliff.
- **Use annotations for the trigger too, with `matchConditions` doing the selection.** A CEL
  `matchConditions` expression can read annotations, unlike `objectSelector`, and the generator already
  supports the marker. Rejected on the supported floor: this project supports Kubernetes `>= 1.23`
  (`deploy/gpustack-operator/chart/Chart.yaml`, `kubeVersion: ">=1.23.0-0"`), while `matchConditions`
  is alpha in 1.27 and reaches GA in 1.30 — an older API server drops the field, and a trigger that is
  silently dropped fires on every Pod in the cluster. A label selector works on every version this
  project supports, and an operator can read it in one line of
  `kubectl get mutatingwebhookconfiguration`.
- **`failurePolicy: Ignore`.** Rejected as the default: it converts webhook downtime into engine Pods
  that start silently without a cache their owner believes is active — a wrong result rather than a
  failed create. It remains the right choice for a cluster that would rather serve uncached than not
  serve, and changing it is a one-marker change, so the trade-off is recorded rather than designed away.
- **A companion validating webhook enforcing the same rules after mutation.** Rejected: with
  `failurePolicy: Fail` on the mutating webhook, every rule already fails closed, and a second webhook
  would duplicate the rule set in a second place — two places to keep in agreement, for no case the
  first does not already cover.
- **Create a ConfigMap holding the client config and mount it, or render it in an init container.**
  Rejected: the ConfigMap makes a webhook declaring `sideEffects: None` write objects, which is both
  untrue and a dry-run hazard, and adds a garbage collection problem for a file whose natural lifetime
  is the Pod's; the init container adds a container, a scheduling delay and an image dependency to every
  injected Pod. The `downwardAPI` projection of the Pod's own annotation has none of those costs.
- **One vehicle for both engines — the file everywhere, or the args everywhere; or both vehicles at
  once.** Rejected in every direction: the file everywhere would put a volume on an SGLang Pod that
  needs none; the args everywhere would put a JSON blob on a vLLM command line where the connector reads
  a path anyway; and writing `tenant_id` twice is the same failure class as two `--kv-transfer-config`
  flags, where the second spelling is only ever consulted when the first is wrong. One vehicle *per
  engine*, the cheapest that engine has, is what the measured wiring supports.
- **Merge with a user-set connector key instead of refusing.** Rejected: "two `--kv-transfer-config`,
  which one wins" is undiagnosable from outside the Pod. An explicit opt-out is how a user takes over.
- **Infer the engine from the image.** Rejected: a renamed or vendored image mis-injects silently, and
  the failure mode — an engine that starts and caches nothing — is exactly the one nobody notices.
- **Pick the first container when several exist.** Rejected on this repo's own experience: an injection
  landed in a sidecar while the workload ran in the main container, and the symptom was "artifacts
  present, feature absent".
- **Inject anyway when the tenant would not take effect, and warn.** Rejected at both ends — a master
  with no ledger, and an engine that truncates the parameter. A warning on a create the user does not
  read produces silent cross-domain collisions, which corrupt results. This is the alternative the
  whole F4 section exists to refuse.
- **Write the `tenant_id` key anyway, so the file is "ready" when engines catch up.** Rejected, and it
  is the most tempting wrong answer here: the key costs nothing to emit and would make the config look
  complete. But no engine reads it, so its only effect is on a human — someone inspecting the injected
  file, or the F7 stamp, would see a domain named and reasonably conclude it is in force. A value that
  is present, ignored, and reads as a guarantee is worse than an absent one, and it would also mask
  the day an engine starts forwarding, since nothing would change in the rendered output.
- **Patch the engines in this project — vendor a fork, or inject a sitecustomize shim.** Rejected as
  out of scope: it puts this operator in the business of shipping engine builds, and the fix belongs
  upstream where one call-site change serves everyone. Recorded in Open Questions as the follow-up.

## Open Questions

- **Should F4a refuse a Pod whose declared reuse domain will not take effect? Decided: no, it stamps.**
  It was written as a refusal, and as a refusal it fired on every Pod: `spec.domain` and
  `spec.domain.name` are both required and an empty name is refused at Binding admission
  (`pkg/worker/kvcache/mooncake/quota_policy.go:129-132`), while every engine in F4a's table is measured
  as non-forwarding. A gate at a 100% hit rate is not a gate, and it would have shipped a webhook that
  only ever refused.
  The refusal did not move to a narrower Pod-side condition; it moved to a different **actor**. The Pod's
  author did not create the second reuse domain, so refusing them turns one namespace's Binding into
  another namespace's outage. The matching refusal is on creating a second distinct domain against a
  backend that cannot separate them, which is the Binding's webhook and is carried as **D8**, not landed.
  Until D8 lands, the isolation gap is declared by the F7 stamp and prevented by nothing — stated here,
  in Dependencies and in Risks, rather than left for a reader to notice.
  The residual question is D8's own: what a Binding webhook should do about the domains that already
  exist when the check is added. Refusing only *new* ones leaves the existing collision in place;
  refusing on update would make an unrelated edit fail. That belongs to the follow-up.
- **`failurePolicy: Fail` or `Ignore`?** Decided `Fail`, for the reason in F2, with the availability
  cost stated and the selector bounding it. Recorded here because it is the one decision an operator may
  legitimately want to reverse per cluster.
- **Which observability variables are on by default?** Decided: `MC_TE_METRIC` and
  `MC_STORE_CLIENT_METRIC_BANDWIDTH` on, `MC_STORE_MEMCPY` left to auto-detection. The residual is
  whether the client bandwidth summary needs the client HTTP server (`enable_client_http_server`,
  `client_http_port` default `9300`) to be reachable to be useful, which would make it a port question
  rather than a variable question.
- **Environment variable versus projected file per engine — closed, and it is one of each.**
  Measured, not chosen. vLLM has no environment path at all: `load_from_config` raises when
  `MOONCAKE_CONFIG_PATH` is unset (`worker.py:144-151`), and that variable names a file rather than
  carrying configuration. SGLang has three paths, and the two that are fixed at admission time — the
  file and `extra_config` — are both ruled out by one value, `local_hostname`, which is an address the
  Pod does not have yet. See F5 for the evidence; the short form is that a webhook cannot write a value
  that does not exist when it runs, and only the environment defers evaluation to the kubelet.
  An earlier draft of this spec closed this the other way, on a claim — "neither reads the client's named
  `MOONCAKE_*` variables" — that is false for SGLang's `load_from_env`. The residual is engine-version
  pinning: the variable names, each reader's key set and the three-way `_load_config` precedence are
  pinned to the versions in F4a's table, and a version that moves them is caught by that table's
  discriminating check rather than by a user's failed deployment.
- **The upstream fix that would lift the tenant gap.** The client already accepts a `tenant_id` —
  `setup()`'s dict overload forwards every key (`store_py.cpp:2237-2271`,
  `real_client.cpp:1310`). What is missing is one call-site change per engine: switch from the
  positional overload to the dict one, and thread a `tenant_id` key through the engine's own config
  class. That is a small upstream PR against vLLM and SGLang, **not** a wait for a Mooncake release —
  which is why F4a is written as a defeasible fact with a version table rather than as a permanent
  limitation. **That change is worth more than isolation alone:** it would also retire the `default`
  Binding prerequisite, since an engine that forwards its domain writes under a name the Binding has
  already registered. Open: whether this project files those PRs, and what the **stamp** says about an
  engine build that carries the patch but reports the same version string — it would under-claim,
  recording `isolated:false` and the `default` tenant for a deployment that is in fact isolated.
- **The pool must publish the client's master address — closed, and it does.** The field is
  `status.clientEndpoint`, and its contract is exactly what this webhook needs: *the address an
  inference engine connects to*, echoed from the backend's `Client` endpoint, with the `Admin` address
  deliberately republished nowhere. Nothing has to be added to the pool API for this spec.
  The rest of the original question dissolved rather than being answered, and in the opposite direction
  from how it was posed — the resolved sources are now stated in F3:
  - the **metadata-plane value is not an address at all**, so no object needs to publish one: the plane
    is peer-to-peer and every participant writes the constant `P2PHANDSHAKE`;
  - the **transport is backend-wide, not per-node** — the reverse of what this question assumed. One
    member group is one DaemonSet and cannot carry a per-node transport, so the webhook reads the
    backend's field through the existing map rather than resolving anything per Pod;
  - the **RDMA device list is never written**, because empty is what means "discover per host" and is
    therefore the only value correct for every consumer of one pool.
- **Should `local_buffer_size` become configurable?** Not yet, and the reason it is not a field is
  worth recording because the opposite was briefly decided. It reads like a resource allowance — host
  memory, per namespace, sized against a limit — which argues for putting it on the Binding beside the
  quota ceiling. The store's own description settles it the other way: it is a **setup-time staging
  buffer registered with the Transfer Engine for short-lived client-side work** (F5), an implementation
  detail of the data path rather than a grant. The Binding carries the grant and its quota; this is
  neither.
  So the renderer writes the store's own example value, `128 MiB`, as a constant, and **no API surface
  changes**. What would reopen this: a workload whose staging genuinely needs a different figure, at
  which point the question is per-Binding versus per-Pod — and that is a requirement question, not the
  feasibility reasoning that produced the first answer.
- **What is SGLang's prefill/decode role knob?** The role annotation is refused on `sglang` until the
  prefill/decode spec names it, rather than accepted and ignored.
- **`vllm-ascend`'s transport — closed.** Ascend transport does **not** need a `USE_ASCEND` build: the
  published `-npu` wheel ships `ascend_transport.so` as a separate shared library linking
  `libascendcl.so`, so `ascend` is one of the transports the backend spec's `transport.protocol` enum
  accepts. What Ascend needs is the CANN runtime present in the container, which is an image concern.
  `vllm-ascend` therefore resolves to `ascend` where the backend offers it and `tcp` otherwise, using
  the same enum value the backend spec defines rather than a spelling of its own.
- **Does the workload CR use this webhook, or render its configuration directly?** Both must produce
  identical results; if they diverge, the same declaration gives users different behaviour. Whichever is
  chosen, the equivalence needs the test named in the Test Plan's e2e section.
- **Should the webhook inject NetworkPolicy requirements**, given the random-port behaviour, or is that
  left to the administrator? The current answer is administrator-owned and documented as a range; the
  question is whether a pool-scoped policy object belongs to the pool spec instead.

## External References

- Mooncake repository — <https://github.com/kvcache-ai/Mooncake>
- Mooncake Store design — <https://kvcache-ai.github.io/Mooncake/design/mooncake-store.html>
- Mooncake multi-tenancy deployment, including the vLLM and SGLang `tenant_id` wiring —
  <https://kvcache-ai.github.io/Mooncake/deployment/multi-tenancy.html>
- vLLM KV transfer configuration — <https://docs.vllm.ai/>
- SGLang HiCache storage backend — <https://docs.sglang.ai/>
- LMCache, the webhook-injection precedent — <https://github.com/LMCache/LMCache>
- Kubernetes admission webhook `objectSelector` semantics (a `LabelSelector`, so labels only) —
  <https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/>
- LeaderWorkerSet, for the second acceptance case — <https://github.com/kubernetes-sigs/lws>
