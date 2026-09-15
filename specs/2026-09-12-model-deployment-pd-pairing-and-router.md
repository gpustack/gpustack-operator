# Spec: ModelDeployment PD Pairing and Router Orchestration

Status: Shipped
Blocked on: nothing. Both of the things that blocked this have since resolved, and what they
unblocked is recorded here rather than deleted, because a spec that never says what it was waiting
for reads as one that was never contested. The prerequisite was the ModelDeployment stabilisation
work — it makes a deployment's identity immutable, splits roles on different instanceTypes into
separate scheduling groups, and converges the transport, and this spec's premises about one
instanceType and one queue are written against the shape that precedes it. It has shipped. The three
owner decisions were OQ1 (which router), OQ2 (one router or several, and whether the API carries a
discriminator) and OQ3 (does `status.endpoint` move); all three are answered in Open Questions. Which
tasks each one blocked is stated per task in the Implementation Plan.

The code tasks through T9 have landed. T10 remains permanently blocked on RDMA-capable hardware and
a serving engine image.

This moved to Shipped when every task below except T10 had landed. T10 is separate and permanent: it
needs a cluster with RDMA-capable nodes and a serving engine image, and no code change can lift it.
The cluster it would run against is not confirmed available, so T10 records what it needs rather than
when it happens, and the Shipped criterion excludes it by name rather than by silence.
Type: Feature

Supersedes `#### F4 — roles[].acceleratorKey: per-role model selection inside one pool` of
[2026-09-02-model-deployment-pd-atomic-admission.md](2026-09-02-model-deployment-pd-atomic-admission.md),
together with the `roles[].acceleratorKey` row of that spec's field table, the two YAML examples that
set the field, and the clause in its rollout-hash section reading "same (or no) `acceleratorKey`".
Those sections are left unchanged, as a shipped specification is a historical design record. What
changed is stated in "The withdrawn field, and where its capability went" below.

## Summary

A `ModelDeployment` can already run a prefill role and a decode role and get them admitted
atomically. Nothing routes a request to them.

The address a client resolves — `status.endpoint` — is the deployment-wide Service, and that Service
selects **one role, the first one declared**. So a disaggregated deployment publishes an address that
reaches either every prefiller or every decoder, never a pair, and which of the two it is depends on
the order the roles happen to appear in the manifest. The per-role Services exist and are correct;
what does not exist is anything that decides which prefiller feeds which decoder.

This spec adds `spec.router` and nothing else to the CRD surface. The operator does not implement a
router: it deploys a selected upstream one and wires it to the three contracts it needs — the
serving metrics it scores on, the KV event stream it learns cache placement from, and the pool
endpoint it reads blocks through. Those three contracts are also published on the deployment, so a
reader can see what the router was configured from.

Three properties of this design are load-bearing. Two come from upstream; the third is a requirement
placed on it, and it is the one that decides which upstream is eligible at all.

- **There is exactly one prefix decision-maker.** Layering a gateway above a router is supported;
  layering two things that both decide prefix affinity is not. The design admits an L7 gateway above
  the router it renders, and refuses to render a second scorer beneath one.
- **KV-event publishing is the standard silent degradation.** A router in KV-aware mode with no
  publisher behind it does not fail; it scores on an empty cache view and keeps serving. The
  deployment therefore reports whether publishing is on as its own status axis rather than inferring
  it from the router being up.
- **The router manages east-west traffic and its lifetime is decoupled from the replicas it routes
  to.** Scaling a prefill, decode or server role up or down MUST NOT restart the router, ever.
  The set of live replicas is the router's **data**, discovered at runtime through the selectors this
  spec publishes; it is never part of the router's **configuration**. That line is what makes the
  rolling rule in F2 safe, it is what makes the role label in F3 load-bearing rather than convenient,
  and it is a **selection criterion in OQ1** rather than something adapted to afterwards: an
  implementation that takes its backends as start-up arguments, or snapshots them once at boot,
  cannot satisfy it and is not eligible.

## Motivation

### What already exists, and what is missing

Measured on `origin/main` at `38e4bbb5`.

| Surface | State |
| --- | --- |
| `spec.roles[]` | Present: `name`, `replicas`, `instanceType`, `resources`, `extraArgs`, `env`, `template`, `kind` |
| `ModelDeploymentRoleKind` | Present: `server`, `prefill`, `decode` (`api/worker/v1alpha1/model_deployment.go`) |
| Atomic admission across roles | Shipped in S7: one Kueue Workload, one PodSet per role |
| `status.conditions` | Three axes: `DomainRegistered`, `QuotaReserved`, `CacheAttached` |
| `status.endpoint` | The deployment-wide Service, which selects **`roles[0]` only** (`model_deployment_service.go`) |
| Per-role Services | Present: `<deployment>-<role>`, rendered by S7 for whatever would later pair the roles |
| `spec.router` | **Absent. Zero fields, zero references in the API package.** |
| Any P/D pairing mechanism | **Absent.** Nothing tells a request which prefiller feeds which decoder |
| KV event configuration | **Absent.** No role is told to publish, and nothing consumes |
| Serving metrics contract | **Absent as a contract.** The engines expose them; nothing asserts they are reachable |

**The table above is a dated measurement and is left as one.** Three rows of it have since been
answered by this spec's own first tasks, and re-measuring the table in place would delete the gap
that justifies the spec. What follows instead is what landed after that measurement, so a reader
cannot take an "Absent" above for a statement about the tree in front of them:

| Row | Landed as | Commit |
| --- | --- | --- |
| `spec.router` | the four-field struct and `status.router`, plus one `make generate` | `86e2e35b` |
| `spec.router` description | one generated description corrected after the fact | `75cf38f2` |
| Admission: two roles of one non-`server` kind | the router-independent subset of F8 | `86e2e35b` |
| Role labelling | `modeldeployment.gpustack.ai/role-kind`, rendered for every deployment | `25a17e24` |

`spec.router` therefore exists and is accepted, and nothing reads it: there is no rendering, no
router-dependent admission rule, and no projection. The API shape landed ahead of its behaviour on
purpose, and the field comments say so where a reader of the API would look.

So the gap is not "the roles do not exist" — they do, they are admitted correctly, and each is
individually addressable. The gap is that **a correctly admitted P/D pair is unreachable as a pair**.

A deployment-wide Service selecting *every* role was considered and rejected when the per-role
Services shipped, and the reason is recorded beside the renderer: it would round-robin one request
onto a process configured as a producer and one configured as a consumer. This spec does not revisit
that. It adds the thing whose absence made the question unanswerable.

### The withdrawn field, and where its capability went

S7 shipped `roles[].acceleratorKey`, which let a role select one accelerator model inside its pool by
rendering a single nodeSelector entry. It was withdrawn in
[#258](https://github.com/gpustack/gpustack-operator/pull/258), a breaking change permitted because
no release tag carried the field.

The mechanism it drove — Kueue assigning a ResourceFlavor per PodSet, so two roles of one Workload
can land on two accelerator models — **still exists and still works**. What no longer exists is a way
for a role to ask for one model rather than another within its pool. That gap is tracked at
[#199](https://github.com/gpustack/gpustack-operator/issues/199), and the one-instanceType refusal
message names it rather than recommending a field that is gone.

**This spec does not restore the field and does not depend on it.** Today every role in a deployment
shares one `instanceType` and therefore one ClusterQueue, and admission refuses anything else.

**That constraint is being lifted by the stabilisation work this spec now waits on**, which splits
roles on different instanceTypes into separate scheduling groups with a workload set each. Nothing
below depends on the roles sharing a queue: the contracts are per role, the label is per Pod, and the
projection maps role names to endpoints. So this spec survives the lift — but a sentence here that
asserted the old shape as permanent would not, which is why the assertion is dated rather than
stated flat.

### Goals

1. A request addressed to a disaggregated `ModelDeployment` reaches one prefill replica and one
   decode replica, in that order, and the KV blocks move between them rather than being recomputed.
   Whether they travel point to point or through a shared pool depends on whether one is attached;
   see F10.
2. The operator deploys and configures a selected upstream router when asked to, and publishes the
   contracts a user's own router needs when not.
3. Whether the engines are publishing KV events is visible on the deployment, because a router that
   is up while publishing is off is the failure this class of system produces most often.
4. A single-role (`kind: server`) deployment is unaffected. `spec.router` is optional and its absence
   renders nothing.

### Non-Goals

| Not doing | Why |
| --- | --- |
| Writing a router | Six or more upstream implementations exist. The Gateway API Inference Extension states that production deployments should run llm-d-router or write their own endpoint picker, and that an endpoint picker MUST implement Envoy ext_proc streaming. Writing one means maintaining an Envoy extension |
| Building a KV index | The upstream indexer API (Mooncake issue #1403) was closed as not planned with no reference implementation. The Mooncake master flags that point at it are deprecated pointers to something never built |
| A second prefix scorer | Upstream is explicit that layers compose but prefix decision-makers do not. Rendering a scorer under a gateway that already has one produces two answers to one question |
| Block-level metadata in the API server | A 64K context at `block_size=64` is 1K block records for a transfer that takes under a second; a hundred-terabyte pool implies block counts in the hundreds of millions |
| A router CRD | The router is configuration, not an API object. The shape adopted here is a Deployment configured by a ConfigMap |
| Cross-manufacturer role placement | Out of scope **here**, and no longer out of scope everywhere: the stabilisation work owns it, together with #199. This spec routes to whatever it produces |
| **Teaching SGLang to declare a role** | See "Only vLLM can declare a role today" below. It is an engine-rendering gap, not a routing one, and fixing it here would widen this spec into S7's territory |

### Only vLLM can declare a role today

`spec.engine: sglang` cannot carry `kind: prefill` or `kind: decode` at all. The role-support table
maps SGLang to the absence of a role, admission reads that same table, and a role the table has no
term for is refused with the engine named.

**The table is accurate and the refusal is deliberate.** Every consumer of that table — the
renderer's own predicate, the controller helper admission calls, and the refusal message a user
reads — speaks of whether the engine's *rendering* has a term for the role, never of what the engine
can do. Admission reads the renderer's table rather than a copy, and the reason is recorded beside
it: a second table would agree today and diverge on whichever engine release lands next, with
nothing failing in between. So this is a **missing renderer feature**, not a limit worded wider than
its reason: this repository has no code that emits SGLang's role arguments, and the table says
exactly that.

It is also the state an earlier spec asked for in as many words — refuse the role on this engine
rather than accept it and ignore it, until a spec names the knob. What is still open is only the
second half of that sentence: **which spec names it.** That is registered separately; it is not this
one, because the subject is engine-role rendering rather than routing.

The consequence here is therefore a scope fact rather than a task: **every contract below is
reachable only on the vLLM family until that rendering gap is closed.** Nothing here waits on it, and
nothing here is made wrong by it — a routing contract published for a deployment shape that cannot
exist yet costs nothing, and the refusal keeps that shape from existing half-configured.

### Dependencies

| Depends on | State | Why |
| --- | --- | --- |
| S7 — cross-role atomic admission | Shipped | Roles and per-role PodSets are the thing being routed to |
| **ModelDeployment stabilisation** | **Shipped** | **The direct prerequisite.** It makes a deployment's identity immutable — the presence of a router is editable, because what fronts a deployment does not change which deployment it is — splits roles on different instanceTypes into separate scheduling groups, and converges the transport on Mooncake while keeping NIXL and ROCm NIXL reachable. Its conclusions are premises here, not inputs to be designed here |
| S3 — `KVCachePool.status.clientEndpoint` | Shipped | The endpoint is published in `status.router`; llm-d has no input for it, so its runtime configuration does not carry it |
| S6 — single-role `ModelDeployment` | Building, blocked on hardware | Only its shipped code surface is used. Its two open tasks are measurements, and nothing here waits on them |
| SGLang role rendering | Missing feature, tracked separately | Not a blocker. It widens who can use this, not whether it works |
| Upstream router selection | **Open — see Open Questions** | The rendering differs per router. Most of the contracts do not |

## Proposal

### User Stories

**A platform operator turns on disaggregated serving.** They already have a `ModelDeployment` with a
prefill role and a decode role that Kueue admits atomically. They add `spec.router`.
The operator renders a router Deployment, a ConfigMap holding its configuration, and a Service; sets
the deployment's `status.endpoint` to the router's address; and labels each role's Pods so the router
can tell a prefiller from a decoder. Nothing else in their manifest changes.

**An SRE finds the cache is not helping.** Hit rate is flat. They read
`status.conditions[type=KVEventsPublishing]` and find it `False` with a reason naming the role whose
engine was not configured to publish. Before this spec that state is indistinguishable from a cold
cache.

**A user does nothing.** Their deployment has one `kind: server` role and no `spec.router`. Behaviour
is byte-identical to today, except that every Pod gains one label whose value is `server`.

### Core Features and Acceptance Criteria

#### F1 — `spec.router`: the field, and what it accepts

A new optional struct on `ModelDeploymentSpec`, numbered 6 (the next free protobuf tag; 1 through 5
are `model`, `engine`, `kvCache`, `roles`, `engineVersion`). `status.router` takes status tag 7, the
next free one there.

```yaml
spec:
  router:
    name: llm-d            # which router implementation. Required when the struct is present.
    replicas: 1            # a POINTER with no schema default; the renderer defaults it to 1.
    image: ""              # empty means synthesize from the selected router.
    extraArgs: []          # refused for any flag the operator derives.
```

**`replicas` is a pointer and carries no schema default, and that is forced rather than chosen.**
Structural-schema defaulting runs before any webhook, so a plain `int32` defaulted to 1 arrives at
admission as 1 whether or not the user typed it, and the distinction cannot be recovered afterwards.
Keeping "did anyone ask for this" answerable at admission is what the pointer buys. `image` and
`extraArgs` need no such treatment: an explicitly empty value means the same thing there as an absent
one. The same distinction is already made on `status.roles[].assignedFlavor`, for the same reason.

Acceptance:

- A deployment with a `prefill` role and a `decode` role and no `spec.router` is **accepted** and
  renders no router. It is the S7 behaviour, unchanged: the pair is admitted atomically and reachable
  only through the per-role Services. The condition added by F6 reports the gap rather than a webhook
  refusing it, because the shape was legal before this spec and refusing it now would break it.
- An `extraArgs` entry whose flag the operator derives is refused. **This is a new rule, not a reuse
  of the role one.** `validateModelDeploymentRoleExtraArgs` takes a `*ModelDeploymentRole`, reads
  `role.ExtraArgs`, and consults a catalog keyed by *engine*; a router argument is none of those. What
  is reused is the shape — a catalog, a name-extracting helper, a refusal naming the key — behind a
  second catalog keyed by *router*. **Scope note:**
  [#353](https://github.com/gpustack/gpustack-operator/issues/353) records that this class of rule
  keys on a flag's name while what it protects is a setting, and that one setting may have more than
  one name. This spec inherits that weakness and does not fix it.

#### F2 — What gets rendered

One Deployment, one ConfigMap, one Service, one ServiceAccount, one Role and one RoleBinding, all
owned by the `ModelDeployment` and garbage-collected with it. The ConfigMap carries the router's
configuration; the Deployment mounts it. A router that
does not hot-reload is the common case, so the ConfigMap's content is hashed into a pod template
annotation and a change rolls the router.

**What may go in that ConfigMap is therefore bounded, and the bound is a requirement rather than a
preference.** The rendered configuration holds only things that are stable across scaling: the label
selectors, the metric names and port, the KV event topic and settings, and the router's own knobs.
The pool's client endpoint is published in status but is not placed in llm-d's strict configuration,
because llm-d has no field that consumes it. The configuration holds **no list of replicas, no replica count, and no per-Pod
address** — because the hash rolls the router, and anything scale-derived inside it would restart the
router every time a role scaled, which the third load-bearing property forbids outright. The live
set is discovered by the router at runtime through the selectors F3 renders. A candidate router that
cannot discover its backends is ineligible; that is an OQ1 criterion, not a rendering detail for
T4a to settle.

**The router is not a workload of the deployment's queue.** Its Pods carry no
`kueue.x-k8s.io/queue-name` label and request no accelerator, which keeps them outside the admission
chain entirely — the Pod webhooks select on that label's presence, so a router Pod is never seen by
them. This is stated as an acceptance criterion rather than left implicit: adding the label would put
the router into the same ClusterQueue as the engines it routes to, where it would compete for the
quota it exists to make useful.

Acceptance:

- Deleting the `ModelDeployment` removes all six. Verified by **asserting the owner reference is
  set** in unit tests and by observing the actual deletion in the cluster case — the fake client runs
  no garbage collector, so a unit test that "observes" collection is asserting nothing.
- Editing anything that changes the rendered configuration rolls the router Deployment exactly once.
  A re-render that differs only in map iteration order produces the same hash and no roll.
- **Scaling any role — prefill, decode or server — up or down does not roll the router**, asserted
  directly by re-rendering across a `replicas` change and requiring the hash to be unmoved and the
  router's pod template untouched. This is the acceptance criterion the third load-bearing property
  reduces to, and it is asserted on the rendering rather than only observed in a cluster, so a
  regression fails at L0.
- The router's Service becomes `status.endpoint` (subject to OQ3). The deployment-wide Service and
  the per-role Services stay and keep their own names, because the roles remain individually
  addressable for debugging.
- The router Pod carries no queue-name label and no accelerator request, asserted directly.
- A router that never becomes ready leaves the deployment `Degraded` with the router named in
  `phaseMessage`, and does **not** leave `status.endpoint` pointing at an address nothing answers on.
  **This widens what `Degraded` means**, which today is derived from role replica counts alone and
  cannot reach `Degraded` while every replica is ready. The widening is deliberate and its cost is in
  Risks.

#### F3 — Role labels: how the router tells a prefiller from a decoder

Each role's Pods carry a label **in this project's own domain** whose value is the role's `kind`.
Prefill and decode Pods of one deployment belong to one logical pool and are distinguished within it,
which is the shape llm-d uses and the shape both the vLLM router and SGLang Model Gateway consume
through their prefill and decode selectors.

**This label is the mechanism the no-restart property rests on.** A router that is given its backends
as a list must be reconfigured, and therefore restarted, every time a role scales. A router given a
*selector* is not: the set behind the selector changes underneath it and its configuration does not
move. So the label is not a convenience for external users — it is what lets the router's
configuration stay free of anything scale-derived.

Three constraints are not negotiable and each has a cause in code:

- **It is a new key, never `app.kubernetes.io/component`.** That label carries the role's *name*, it
  is what the Services select on, and it is what status reads to attribute a Pod to a role. Two roles
  of one kind would collapse into one Service and one status entry. The pod-group file records that
  no label of ours carries the role and that adding one in our own domain later is additive; this is
  that addition, and that comment is updated to say so.
- **It goes in the Pod's labels, not in the Service selector.** A selector must hold nothing a spec
  update can move, or moving it orphans the replicas it used to front — and `kind` is mutable.
- **It enters the Pod spec hash, and therefore rolls every existing group once.** The hash covers the
  label map. This is the accepted cost of any label addition here and is stated in Risks rather than
  discovered at upgrade time.

Acceptance:

- A `kind: prefill` role's Pods carry the role label with value `prefill`, a `kind: decode` role's
  carry `decode`, and a `kind: server` role's carry `server`.
- The label is rendered for every deployment, not only routed ones — including a single `server` role
  and including an unrouted one. It costs nothing, and a user reading `status.router` needs it to
  make sense of the selectors published there.
- The Service selector is unchanged, asserted by comparing the rendered selector against the render
  with the label absent.
- The selectors the operator would use are published in `status.router` verbatim, so an external
  router is configured from observed strings rather than from this document.

#### F4 — The KV event contract

vLLM takes a `KVEventsConfig`: publishing off by default, `publisher` one of `null` or `zmq`, an
endpoint defaulting to `tcp://*:5557`, plus replay endpoint, buffer depth, high-water mark and topic.
The event types are block-stored, block-removed and all-blocks-cleared, and the stored event carries
the extra keys an external consumer needs to reconstruct a block hash.

The operator renders this configuration onto the roles that produce blocks, points the router at the
resulting endpoints, and publishes those endpoints on the deployment.

**A bind address is not a dialable address.** `tcp://*:5557` is what the publisher binds; it is not
something a router can connect to. The rendering therefore also decides how a consumer reaches it,
and that decision is part of this feature rather than an implementation detail: the port is rendered
as a container port, and `status.router` publishes per-role addresses a consumer can actually dial
rather than the bind string. Which carrier those addresses use — the role's existing Service extended
with a second port, or Pod addresses — is settled by the task, since it depends on whether the
selected router discovers endpoints or is given a list.

Acceptance:

- A routed deployment's prefill role is configured to publish; its decode role is not required to.
- The rendered endpoint, topic and replay endpoint appear in `status.router` exactly as rendered, in
  a form a consumer can dial.
- The publisher's port appears as a container port on the roles that publish.
- A user who sets the same setting through `roles[].extraArgs` or `env` is refused by the existing
  owned-key rule rather than silently overridden, which means the new keys join the owned-key catalog
  — and that catalog is pinned to the reference page by a unit test, so the page is updated in the
  same task or the test goes red.
- A role that replaced its whole command line is configured with nothing, because the operator
  synthesizes nothing for it. What that role reports is F6's problem, not this one's.
- **Not asserted here:** that the events are correct, or that the router's hit rate improves. Those
  need a serving engine and belong to F9.

#### F5 — The serving metrics contract

The Model Server Protocol requires three Prometheus metrics from a model server: queued requests,
running requests and KV cache utilisation, with block size and block count optional and needed by a
prefix scorer. The engines expose these; what is missing is anything asserting they are reachable
from where the router runs.

**The table's reach is the engine family and no further, and the limit is stated rather than left to
be found.** `engineVersion` is deliberately unvalidated and a role may name any image, so a table
keyed by engine cannot establish what a particular build exposes. It answers "does the engine this
operator knows how to configure expose what the selected router requires", which is a real question
with a real wrong answer to prevent; it does not answer "does this image".

Acceptance:

- The rendered router configuration names the three metrics and the port they are served on.
- `status.router` publishes the same three names and that port.
- A metric the selected router requires and the configured engine does not expose is **refused at
  admission**, naming both. This is a static table keyed by engine, not a live scrape, and the field
  comment states that a custom image can still fail this check's premise.
- Every value of the engine enum has an entry in the table, asserted by a test over the enum rather
  than over the table — a table missing an engine would otherwise accept every deployment on it.

#### F6 — `KVEventsPublishing`: the condition

A fourth condition on `ModelDeploymentStatus`, independent of the existing three.

| State | Meaning |
| --- | --- |
| `True` | Every role that should publish is configured to publish |
| `True`, reason `NotApplicable` | The deployment declares only `server` roles AND sets no `spec.router`. Nothing would consume the events |
| `False`, reason `PublisherDisabled` | A producing role is not configured to publish. The router will score on an empty cache view |
| `False`, reason `NoRouter` | The deployment has a prefill and a decode role and no `spec.router`. The pair is admitted and unroutable |
| `Unknown`, reason `RoleUnmanaged` | A producing role replaced its whole command line. The operator synthesized nothing and cannot say what that role does |

**There is no state the operator never writes.** An earlier draft carried a bare `Unknown` for "not
yet evaluated", which no branch could reach: the condition is computed from rendered configuration,
and every pass that writes status has it. `Unknown` now means one specific thing the operator really
cannot answer, which is the take-over tier — the same tier that already moves `CacheAttached` to
`Unknown`, for the same reason, so the two agree instead of disagreeing.

Acceptance:

- The condition is computed from the rendered configuration, not from a probe. It answers "did we
  configure publishing", which is what the operator can know.
- It is set on single-role deployments too, as `True` with the reason saying publishing is not
  applicable — an absent condition and a false one are different states and a reader should not have
  to tell them apart by absence.
- Every case asserts the **reason**, not only the status. Two of the states share `False` and two
  share `True`, so a status-only assertion passes with the reasons swapped.
- It survives the wholesale status rebuild: a second reconcile over an unchanged spec leaves the
  condition present and unchanged, because a condition that is not recomputed disappears rather than
  going stale.
- **The naming is deliberate and the limit is stated in the field comment:** this condition does not
  observe events flowing. A publisher configured and crashed reads `True` here. Observing the stream
  is F9's job and needs a live engine.

#### F7 — `status.router`: the published contract

`status.router` carries the pool's client endpoint alongside everything a router is configured from:
the per-role label selectors, the KV event endpoints and topic, the metrics names and port, and the
role-to-endpoint mapping.

**It is published even though this operator is the only thing that reads it**, and that is the point
worth stating. A contract the operator both writes and consumes could have stayed internal; keeping
it observable is what lets an operator answer "what did the router actually get" without reading a
ConfigMap, and what makes a rendering mistake visible as a wrong string rather than as a router that
merely behaves oddly.

Acceptance:

- Every value in `status.router` is a string the operator actually rendered, never a default this
  document describes. Asserted by perturbing a rendered value and requiring the projection to follow,
  not by comparing against the literals written here.
- The role-to-endpoint map's key set **equals** the role set. A non-empty-map assertion passes with a
  role missing.
- Adding `spec.router` to a deployment that had none renders the six objects, and **refuses to
  adopt** an object of the same name the deployment does not own, exactly as the Service path already
  does.
- Removing `spec.router` from a live deployment removes the six objects and reverts
  `status.endpoint` to the deployment-wide Service. **Removal is an explicit prune, not garbage
  collection** — the deployment is not deleted, so its owner references collect nothing. This is the
  same asymmetry the backend's HA access grant already has, and the same shape of fix.

**Both transitions are representable, and the reason is worth stating because it is not the obvious
one.** The stabilisation work makes a deployment's identity immutable, and the presence of a router
is editable under that rule — not because switching without an outage is desirable, but because
**what fronts a deployment does not change which deployment it is**. The distinction matters for the
next field that asks for the same treatment: avoiding disruption is a benefit of this decision, never
its reason, and a rule built on the benefit would exempt every field whose edit is inconvenient.

#### F8 — Admission rules, in one place

| Rule | Refused because |
| --- | --- |
| `spec.router.extraArgs` colliding with a derived flag | One setting, one source |
| An engine that does not expose a metric the router requires | The failure would otherwise surface as a router scoring on nothing |
| Two roles with the same `kind` other than `server` | Two prefill roles is a shape no consumed router expresses |
| `spec.router.name` changing on an update | It is declared immutable |

**The `kind` rule changes a shipped answer**, and does so unconditionally rather than only when
`spec.router` is present. The `kind` field's own comment states that two roles may share a kind; that
sentence is narrowed in the same change, because leaving the API describing a shape admission now
refuses is worse than the refusal itself. The API is unreleased, which is what makes the tightening
available.

**The rules are named rather than numbered in the prose above and below.** An earlier revision of
this section said "the last rule", which stopped pointing at the rule it meant the first time a row
was appended to the table.

##### The `name` freeze, and what it is not

The refusal carries one reason and that reason is the declaration itself: the field is immutable.
**No mechanism is offered, and the omission is deliberate.** This project already freezes a set of
fields, and it freezes them against a criterion — a field is frozen when it answers "which deployment
is this". The router does **not** meet that criterion, and F7 says so in as many words: what fronts a
deployment does not change which deployment it is. So a refusal borrowing the identity wording would
assert a reason this spec elsewhere denies, and send an operator looking for a relationship that is
not there. The freeze on `name` is its own rule, with its own message, sitting beside the identity
rules rather than inside them.

**The block may still be added and removed; only the value may not move.** `nil` to a name is
allowed and a name to `nil` is allowed, both as F7 describes, and it is the name-to-a-different-name
transition that is refused. The consequence is worth stating here rather than leaving it to be found:
removing the router and adding it back under another name reaches, in two steps, the state one step
cannot. **That is not a hole in the rule.** The intermediate step really does prune the three
objects, so the two-step path is a delete and a create with an interval in between, which is what a
router change is; what the rule refuses is the pretence that it is an edit.

**No reachable cluster input violates this rule today, and the acceptance criterion says so.** The
schema closes `name` to a single value, so the API server refuses a second one before admission runs.
The rule is still written, and it is tested by calling the validator directly with a pair of values
the schema would not accept — which is how every other rule in this webhook is tested, the validators
being pure functions over two objects. Stated plainly because a reviewer who does not know this reads
the test as exercising an impossible state, and a rule whose only test looks vacuous gets deleted by
the next person who tidies.

**The validating half answers every rule from the submitted object.** It holds no client and reads
nothing from the cluster, and this spec reintroduces no rule that would. The `name` freeze compares
against the stored object, which arrives in the admission request rather than being fetched, so it
does not breach that. The *mutating* half of this same webhook does read the cluster — it resolves an
InstanceType to default an accelerator count — so the claim is about the validating path
specifically and is worded that way.

#### F9 — The measurement, and the two things it needs

A request reaches a prefiller, its blocks reach a decoder, and the decoder produces tokens without
recomputing the prompt. Recorded: time to first token against a single-role deployment of the same
model, and the block count that moved.

The measurement is written against the handover, not against a particular route for it. A pair with
no shared pool attached hands blocks over point to point and is measurable on the same terms; naming
the pool here would have made the cheaper shape look unmeasured rather than merely unmeasured yet.

This needs an RDMA cluster and a serving engine image. It is the one acceptance criterion in this
spec that no code change can satisfy.

#### F10 — `spec.kvCache` is optional, and what each shape renders

`spec.kvCache` attaches a deployment to a shared pool. It was REQUIRED when this spec was written
and is optional now, because a prefill/decode pair does not need a shared pool to hand blocks over:
the engines can move them point to point. Requiring the pool made the cheaper shape unrepresentable,
and that shape is the ordinary one for a pair that serves a single model on two nodes.

The renderer selects on presence alone. There is no third state and no flag:

| `spec.kvCache` | Rendered into `--kv-transfer-config` | `CacheAttached` |
| --- | --- | --- |
| absent | the point-to-point connector alone | `True/NotApplicable` |
| present | `MultiConnector` wrapping the point-to-point connector and the shared-store connector | resolved from the Binding |

Acceptance:

- A managed vLLM prefill/decode deployment carrying no `spec.kvCache` is admitted, and the argument
  it renders names the point-to-point connector with no shared-store connector beside it.
- The same deployment with `spec.kvCache` renders `MultiConnector` carrying both. The outer `kv_role`
  follows the role, while the shared-store child takes `kv_both` on a prefiller and `kv_consumer` on
  a decoder — the asymmetry is the upstream contract's, not this operator's invention.
- Without `spec.kvCache` the controller resolves no Binding and fetches no shared cache. Nothing
  else changes: whether a role publishes KV events is decided by F4's own predicate — a router is
  present, the engine is vLLM, the role's command was not replaced wholesale, and the role is not a
  decoder — and **that predicate does not read `spec.kvCache` at all**. The two were reported
  together during the cluster run and they are separate rules; stating them as one would make a
  reader expect an unrouted pair to stop publishing.
- `CacheAttached` reads `True/NotApplicable` rather than staying `Unknown`. "This deployment has no
  shared cache" and "this deployment's cache could not be resolved" must not be the same reading:
  the first is a choice and the second is a fault.

**The Go field changed from a value to a pointer, and that is source-incompatible** for a caller
constructing `ModelDeploymentSpec` directly. The Kubernetes API stays compatible, because every
manifest that was valid before names `spec.kvCache` and still does.

**This feature does not choose WHICH point-to-point connector runs.** See OQ4.

### Verification

**L0, no cluster.** F1's schema and every rule in F8, as table-driven webhook tests. F2's rendering,
F3's labels, F4's and F5's configuration and F7's status projection, as renderer tests comparing
whole objects. Reconcile behaviour — the prune, the adoption refusal, the roll-exactly-once — against
the controller-runtime fake client, which is what this repository has; **there is no envtest here**,
so anything needing a real API server's semantics, garbage collection first among them, is not an L0
claim.

**L1, single node, no accelerator.** A deployment with a prefill and a decode role stays `Starting`
for lack of cards — not `Degraded`, which needs at least one ready replica — and the router, its
ConfigMap and its Service are all rendered and owner-referenced anyway. Deleting the deployment
removes them, which is where owner-reference collection is actually observed. Adding and removing
`spec.router` on a live deployment both converge. The `KVEventsPublishing` condition takes each of its states.

Whether the router itself becomes ready is asserted **separately** from whether its objects are
rendered, because the first needs a pullable router image and the second does not. A case that
bundles them loses the half that works whenever the image is unavailable.

**L2, RDMA cluster with accelerators.** F9 only.

The split matters because S6 is sitting in `Building` on exactly this boundary: fifteen of its
seventeen tasks are merged and the two that are not are measurements no code change can lift. **This
spec is written so that the same thing cannot happen to it** — F1 through F8 complete without
hardware, and F9 is separated rather than woven through them.

### Notes, Constraints and Caveats

**One prefix decision-maker.** The composable shape upstream describes is: an L7 gateway doing host,
path, model name and tenant; below it exactly one endpoint picker covering every role; below that the
engine Pods distinguished by role label. The operator renders the middle layer and does not render a
second one, so a cluster that already runs its own endpoint picker should not also declare
`spec.router`.

**Router replicas trade consistency for availability, and upstream says so.** SGLang states plainly
that its radix trees do not synchronise across replicas and that hit rate drops by ten to twenty
percent as a result. Dynamo records that even with replica synchronisation enabled, the synchronised
events improve load estimation and do not synchronise prefix-cache state or guarantee that two
replicas route alike. So `replicas > 1` is permitted and its cost is stated in the field comment.

**Prefill scores on overlap; decode does not.** Dynamo's implementation turns overlap scoring off for
the decode stage deliberately, because a decoder should not chase prefix reuse. Where the selected
router exposes this, the rendering follows it.

**Which port a role's transport uses is not knowable at admission.** The Ascend direct transport
picks a port at random inside a hundred-port window whose position comes from a runtime call, so the
operator can bound a window but cannot place one. [#203](https://github.com/gpustack/gpustack-operator/issues/203)
was closed on this: a `containerPort` is informational, reserves nothing, and validating it cannot
prevent the runtime failure. **This spec therefore renders no port validation and derives nothing
from parallel degrees**, which is also why the per-engine parallelism survey that once blocked this
spec is not an input to it. The KV-event container port F4 renders is subject to the same rule: it is
declared so a reader and a NetworkPolicy can see it, and it reserves nothing.

**The router gets namespaced, least-privilege RBAC.** The worker's `cluster-admin` binding authorizes
the controller to create objects but does not authorize the router Pod, and a Pod cannot use the
worker's ServiceAccount from another namespace. Each router therefore gets a dedicated
ServiceAccount, a Role limited to `get`, `list` and `watch` on Pods, and a RoleBinding.
`deploy/gpustack-operator/chart/**` is in no task's `Owns`.

### Boundaries

Owns `api/worker/v1alpha1/model_deployment.go` (the `spec.router` and `status.router` types),
`pkg/worker/controllers/worker/model_deployment*.go` (router rendering, the condition, the
projection), `pkg/worker/webhooks/worker/model_deployment.go` (F8), a new
`pkg/worker/kvcache/router/`, and `docs/reference/model-deployment.md`.

Does not touch `pkg/devicemanager/**`, the `KVCacheBackend` or `KVCachePool` controllers,
`node_devices_admission.go`, or the chart. S7 left the admission check correct for multiple roles and
this spec adds no role-shaped demand.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| The selected router changes its configuration format | The configuration is rendered in one package behind one function per router. The contracts in `status.router` are ours and do not move |
| A user's gateway and the rendered router both score prefixes | The two are documented as mutually exclusive; a cluster that runs its own endpoint picker declares no `spec.router` |
| `KVEventsPublishing` reads `True` while nothing publishes | Stated in the field comment as the condition's known limit rather than left for a reader to discover. F9 is where flow is observed |
| **F3's label enters the Pod spec hash, so upgrading rolls every existing deployment once** | Stated here and in the release note rather than discovered during an upgrade. The rollout is the one the recreate policy already describes, and a replica that leaves loses its cached blocks to its siblings |
| **Widening `Degraded` to cover the router changes an already-documented meaning** | The reference page's definition is updated in the same change, and `phaseMessage` names the router, so a reader who sees the new state can tell it from the old one. An alert keyed on `Degraded` will fire for a router fault it did not fire for before |
| **F5's engine-keyed table cannot see the image a role actually runs** | The limit is stated in the field comment. The check prevents the misconfiguration it can see; it does not claim to prevent the one it cannot |
| F9 never runs because no RDMA cluster appears | F1 through F8 ship and the deployment is routable and observable without it. What is unproven is the benefit, not the mechanism |
| **OQ1 stays unanswered** — *retired; it was answered, and the row stays so the mitigation is not read as untried* | T1, T3 and the prefactor land regardless, and T2 lands as its router-independent subset. What stalls is the rendering, not the whole spec |
| **This document opens as one pull request with its implementation, and three of its tasks merged before it** | Each landed task names its commit, so what the pull request contains and what preceded it are separable by reading the plan rather than the diff. The reviewer's question — is anything here described but not built — is answerable per task instead of per document |
| **`spec.router` is accepted by the API today and nothing reads it** | The field comments say so where an API reader looks, and F6's condition reports an unrouted pair rather than the API refusing one. The window closes at T4b; until then a user who sets the field gets no router and no error, which is the state the condition exists to make visible |

## Design Details

### Project Structure

```
api/worker/v1alpha1/model_deployment.go        spec.router, status.router, the new condition
pkg/worker/kvcache/router/                     NEW: one file per supported router, plus the metric table
pkg/worker/controllers/worker/
  model_deployment.go                          the owned-child sync/prune path, the new watches
  model_deployment_render.go                   the role-kind label
  model_deployment_router.go                   NEW: router Deployment, ConfigMap, Service and namespaced RBAC
  model_deployment_connector.go                the KV event configuration and its owned keys
  model_deployment_kv_events.go                NEW: the KVEventsPublishing condition
  model_deployment_status.go                   the condition fold-in, status.router projection
  model_deployment_service.go                  which Service status.endpoint names
pkg/worker/webhooks/worker/model_deployment.go F8
docs/reference/model-deployment.md             the field, the label, the owned keys, the condition
```

### Commands

**Everything runs locally.** T1 through T8 are Go tests on this machine; T9 needs a local
single-node cluster with the operator deployed and no accelerator. There is no remote host, no cloud
account and no SSH target in this spec's loop.

| Purpose | Command |
| --- | --- |
| One package's tests | `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/...` |
| The whole tree | `make test` |
| Build one package | `go build ./api/...` |
| Lint Go and shell | `make lint` |
| Lint markdown and specs | `make lint docs < /dev/null` |
| Regenerate the API | `make generate`, **only from a checkout whose path ends in the module path** — see below |

Four properties of this loop decide how the tasks below are written:

- **`make lint` edits files.** It runs an import reviser and a linter with `--fix`, both writing in
  place. It is never run concurrently with a build, and whether it changed anything is decided by
  comparing file contents rather than by reading a diffstat.
- **`make generate` is decided by the checkout's path, and failing is destructive.** The protobuf
  generator derives its output base by trimming the module path off the physical working directory,
  so a path that does not end in the module path makes it wipe and partially rewrite every generated
  file before it fails. **The condition is the path, not the kind of checkout** — the primary
  checkout of this repository is normally module-suffixed and the command runs there directly, while
  a worktree parked somewhere else is not and must not run it. Checking which one is in hand costs
  one look at the working directory, and the earlier wording of this bullet said "cannot run from a
  worktree", which sends someone to build a second checkout they may not need.
- **`make test` takes exclusion patterns, not inclusion patterns.** A trailing argument names
  packages to *skip*. Selecting one package is a direct `go test` invocation, which is why the table
  above lists both.
- **A `go test -run` pattern that selects nothing still exits 0, and the docs gate knows it.** The
  spec linter checks every `go test … -run` in this file against the tests that exist. Not every test
  named below exists yet, so **no `Verify:` line below carries `-run`**; each names its package
  instead. Tightening them to a `-run` pattern is what the last task does, once the tests they would
  select are all real.

  The condition is written as "the tests do not all exist" rather than as a status word on purpose.
  An earlier revision said "while this spec is `Planned`", which went false the moment the status
  moved while the `Verify:` lines were still correct -- a restated status word goes stale on an edit
  made three lines from the top of the file.

There is no coverage threshold in this repository and no coverage gate in CI. Per-package numbers in
the Test Plan are obtained with `go test -cover` on that package.

### Code Style

Follows the file it lands in. The role label key is a constant beside the existing label constants,
not a literal at its use site. Each supported router gets one function returning its rendered
configuration, and the dispatch is a map keyed by the router's name — the shape the engine-keyed
tables in this package already use. Rendering stays a pure function over values, with the Kubernetes
client confined to the reconciler, which is how the KV cache renderers are already arranged.

### Implementation Plan

Three decision gates stood above the tasks. **All three are answered**, and they are kept here with
their answers rather than deleted, because a gate that vanishes once it opens leaves the tasks below
looking as though nothing ever had to be decided. Each names what it blocked; a task not named was
not blocked by it.

- **G1 — ANSWERED: the field is carried, and it is `name`.** Blocks **T1**. Whether `spec.router` carries
  a field naming *which* router is an API-shape decision, and it is separable from *which* router is
  chosen: reserving a discriminator now makes adding a second router an enum widening, while not
  reserving one makes it a reinterpretation of a field that was absent. The `kvCache.connector`
  field already took the first path for the same reason. **This is a recommendation, not a decision
  taken here.**
- **G2 — ANSWERED: `llm-d`.** Blocked **T4a**, **T4b**, **T6**, the router-dependent half of **T5**, and **T9**'s golden
  objects. Blocked **part of T2**: F8's router-dependent rules, and no others. Which those are is the table's
  to say, not this line's.
- **G3 — ANSWERED: yes, it moves.** Blocked **T8**. Fixes **T9**'s endpoint assertions to the router's
  address while one is rendered.
- **A prerequisite, not a gate: the stabilisation work. It has landed.** It moved premises rather
  than mechanisms here, so the tasks were written against either shape.

**What remains is therefore sequencing, not deciding.** Three tasks are unblocked simultaneously and
own disjoint paths — **P1**, **T5** and **T11** — so they are the entry points, and no ordering
between them is implied by the list below. T4a is the join of the rendering lane; T8 is the join of
everything.

Checkpoints: after T4b (a router exists, is owned, and rolls once per change); after T7 (the
deployment says whether publishing is on); after T8 (both transitions are correct); after T9 (all of
it holds on a cluster). The two earlier checkpoints this list carried — the CRD accepting the new
fields, and a prefiller being distinguishable from a decoder by selector — have both been passed.

- [x] **P1 · Prefactor: one path for syncing and pruning an owned child**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment.go` + its test
      Gate: review
      Acceptance: the Service sync and the Service prune are expressed through one helper taking a
      rendered object, an align function and the resource note that identifies it — the same helper
      T4b then uses for the router's six object kinds. **Behaviour is byte-identical**, asserted by the
      existing Service cases passing unchanged rather than by inspection. No new object kind is added
      here, and the refusal to adopt an object the deployment does not own keeps its current message.
      This exists because T4b and T8 would otherwise each write a second copy of it, and because
      removing `spec.router` needs a prune that owner references cannot provide.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

- [x] **T1 · The API shape and one `make generate`, nothing else**
      Landed: `86e2e35b`. **The struct shipped with four fields, not five.** A `mode` field naming who
      runs the router was designed here and removed before merge: only one of its values would have
      rendered anything, so every stored object would have carried the same value, and the field would
      have carried no information. Adding it later is an optional field with a default and is
      backward compatible; shipping it and then changing what its values mean would not be. The
      decision and its reasoning live on the `ModelDeploymentRouter` type itself. One generated
      description was corrected afterwards in `75cf38f2`.
      Blocked by: G1
      Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/zz_generated.*`,
      `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/generated.pb.go`,
      `api/worker/zz_generated.openapi.go`,
      `pkg/kubeclients/applyconfiguration/worker/v1alpha1/**`
      Gate: review
      Acceptance: `Router *ModelDeploymentRouter` at spec protobuf tag 6; `Router
      *ModelDeploymentRouterStatus` at status tag 7; `Name` carrying its enum and required when the
      struct is present; **`Replicas *int32` with no schema default**, so a set field stays
      distinguishable from a defaulted one; the `KVEventsPublishing` condition type and its reasons as
      constants. The `Kind` field's comment sentence permitting two roles to share a kind is narrowed
      to match the rule T2 adds — the comment lives in this file, so it is edited here even though
      the rule is enforced there. The `replicas > 1` field comment states the hit-rate cost upstream
      reports. No controller change, no webhook change.
      Verify: run `make generate` from the module-suffixed checkout, sync back, then `make generate
      && git diff --exit-code` there; `go build ./api/...`; `git diff --stat api/` shows only the
      type and its regenerated files.

- [x] **T3 · The role-kind label, rendered for every deployment**
      Landed: `25a17e24`. The key is `modeldeployment.gpustack.ai/role-kind`.
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go` + its test,
      `pkg/worker/controllers/worker/model_deployment_pod_group.go` (its header comment) + its test,
      `docs/reference/model-deployment.md`
      Gate: review
      Acceptance: a new constant in this project's label domain, added in `modelDeploymentPodLabels`
      and **not** in `modelDeploymentSelectorLabels`, carrying the role's effective kind. Rendered for
      every deployment including a lone `server` role and including an unrouted pair. A test asserts
      the Service selector is unchanged, in the shape the entrance-label guard already uses. The
      pod-group file's "no label of ours carries the role" comment is updated to record that one now
      does and why it is not the role's *name*. The reference page's label table gains the key, and
      its `## Contents` list stays in sync. The rollout consequence — the label is inside the Pod spec
      hash, so every existing group rebuilds once — is stated in the constant's comment.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`
      and `make lint docs < /dev/null`

- [x] **T2 · F8's router-independent rules**
      Landed: `86e2e35b`, in the same commit as T1. It carries the `kind` rule and nothing else; the
      `name` freeze is router-independent too but was decided later, so it is T11 rather than a
      reopening of this task.
      Blocked by: T1
      Owns: `pkg/worker/webhooks/worker/model_deployment.go` + its test,
      `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
      Gate: review
      Acceptance: two roles of one non-server kind refused. Each rule has a refusal case and a
      positive baseline, and each baseline is also run with its own rule violated, so a baseline that
      would pass against a validator refusing nothing is excluded. The `[server, server]` shape stays
      accepted — the rule says "other than `server`", and a table holding only non-server pairs passes
      against an implementation that refuses server pairs too. The two OQ1-dependent rules are **not**
      here; they arrive with T6.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/`

- [x] **T11 · The `name` freeze**
      Blocked by: None
      Owns: `pkg/worker/webhooks/worker/model_deployment.go` + its test
      Gate: review
      Acceptance: an update changing `spec.router.name` from one value to another is refused, and the
      message says the field is immutable and says nothing else. It does **not** reuse the identity
      message, and a test asserts the two messages differ — the identity message asserts a reason,
      that the value is part of what makes this deployment the deployment it is, and F7 denies exactly
      that for the router. Adding the block to a deployment that had none is accepted and removing it
      is accepted, both asserted, because a rule that refuses every write to `spec.router` passes a
      test that only checks the change case. The refusal names `spec.router.name` as the field path,
      not `spec.router`.
      **The violating input is unreachable through the API server, and the case is named so that a
      reader knows it.** The schema closes the enum to one value, so the API server refuses a second
      one before admission runs. The case is therefore built by calling the validator directly with
      two objects, which is how every rule in this file is already tested, and it carries a comment
      saying it becomes reachable the day the enum widens — which is also the day to check that this
      rule still fires. Without that note the case reads as a test of an impossible state and is
      deleted by the next person tidying.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/`

- [x] **T4a · Rendering the router objects, as pure functions**
      Blocked by: None
      Owns: `pkg/worker/kvcache/router/**` (new),
      `pkg/worker/controllers/worker/model_deployment_router.go` (new) + its test
      Gate: review
      Acceptance: a Deployment, a ConfigMap, a Service, a ServiceAccount, a Role and a RoleBinding
      rendered as pure functions over the
      deployment, each carrying the resource note the controller's watch predicate filters on —
      without the note the watch is dead and out-of-band edits are never corrected. Owner references
      set with the project's non-blocking helper, asserted as set rather than as collected. The
      ConfigMap's content hashed into a pod template annotation; a re-render differing only in map
      order yields the same hash. The router Pod carries **no** `kueue.x-k8s.io/queue-name` label and
      **no** accelerator request, each asserted directly. **The rendered configuration contains
      nothing scale-derived** — no replica list, no count, no per-Pod address — asserted by
      re-rendering across a `replicas` change and requiring an unmoved hash and an untouched pod
      template. That assertion is the whole of the no-restart property at L0, so it is written as its
      own case rather than folded into the determinism one: the determinism case passes on a
      configuration that embeds a replica count, as long as it embeds it deterministically.
      Nothing here touches a client; the reconciler is T4b's.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/router/ ./pkg/worker/controllers/worker/`

- [x] **T4b · Wiring the rendered objects into the reconciler**
      Blocked by: T4a, P1
      Owns: `pkg/worker/controllers/worker/model_deployment.go` (the call site and the five new
      watches) + its test, `pkg/worker/controllers/worker/model_deployment_router.go` (the sync half)
      Gate: review
      Acceptance: the six objects converge through P1's helper rather than through a second copy of
      it, which is asserted by the helper having exactly one definition and seven callers. The
      controller gains `Owns` watches for Deployment, ConfigMap, ServiceAccount, Role and RoleBinding
      using the same predicate the Pod and Service watches use, and a test drives an out-of-band edit to one of them and requires the
      reconciler to correct it — a watch registered but filtered to nothing passes every test that
      only checks registration. Adoption of a same-named object the deployment does not own is
      refused with the message the Service path already uses. Removing `spec.router` prunes all six;
      that transition is asserted here rather than deferred to T8, because T8 asserts what
      `status.endpoint` does about it and the two are separate facts.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

      **This split is deliberate and the reason is the context window, not the layering.** The single
      task these two replace carried six rendered objects, five watches, a content hash, an adoption
      refusal, a prune and the no-restart assertion. Sized for one worker starting cold, that is over
      budget, and a task that does not fit is finished by guessing at the end of it.

- [x] **T5 · The KV event configuration, rendered per role**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_connector.go` + its test,
      `pkg/worker/kvcache/inject/**`, `docs/reference/model-deployment.md`
      Gate: review
      Acceptance: a producing role is configured to publish and a consuming role is not required to;
      the publisher's port is rendered as a container port; the new keys join the owned-key catalog,
      which makes the reference page's owned-key table part of this task because a unit test pins one
      to the other. A role in the take-over tier is configured with nothing, asserted rather than
      assumed. The addresses a consumer would dial are produced here as values, so T8 can project
      them without re-deriving them — a second derivation is how two answers to one question start.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/ ./pkg/worker/kvcache/inject/`
      and `make lint docs < /dev/null`

- [x] **T6 · The metrics contract and the two OQ1-dependent admission rules**
      Blocked by: T4b, T11 — T11 only because both edit the same webhook file, which is a
      serialisation and not a dependency; nothing T6 asserts needs T11's rule to exist
      Owns: `pkg/worker/kvcache/router/metrics.go` (new) + its test,
      `pkg/worker/webhooks/worker/model_deployment.go` + its test
      Gate: review
      Acceptance: a table keyed by engine naming the metrics the selected router requires and the
      port they are served on, with a test asserting **every value of the engine enum** has an entry
      rather than testing the entries the table happens to have. The refusal names both the metric
      and the engine. A second catalog, keyed by router, backs the `spec.router.extraArgs` collision
      rule; the helper that consults it is new rather than the role one reused, and the refusal names
      the field path the user actually wrote. The field comment states that the table speaks for an
      engine family and not for a particular image.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/router/ ./pkg/worker/webhooks/worker/`

- [x] **T7 · The `KVEventsPublishing` condition**
      Blocked by: T5
      Owns: `pkg/worker/controllers/worker/model_deployment_kv_events.go` (new) + its test, and the
      one call site in `model_deployment_status.go` that folds it in
      Gate: review
      Acceptance: all five states reachable, each by a fixture that forces it, and each case asserts
      the **reason** and not only the status. `True`/`NotApplicable` for an unrouted `server`-only
      deployment, paired with a ROUTED `server`-only one that must NOT read `NotApplicable`, because
      the condition turns on the router rather than on the kind;
      `False`/`NoRouter` for an unrouted pair; `False`/`PublisherDisabled` for a routed pair whose
      producer is not configured; `Unknown`/`RoleUnmanaged` for a routed pair whose producer took over
      its command line; `True` otherwise. A second reconcile over an unchanged spec leaves the
      condition present and its transition time unmoved. It reads the rendered configuration, never a
      probe, and the field comment says what it therefore cannot see.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`

- [x] **T8 · `status.router`, the endpoint, and both transitions**
      Blocked by: T4b, T5, T6, T7
      Owns: `pkg/worker/controllers/worker/model_deployment_status.go` + its test,
      `pkg/worker/controllers/worker/model_deployment_service.go` + its test,
      `docs/reference/model-deployment.md`
      Gate: review
      Acceptance: `status.router` projects values T4a, T5 and T6 rendered, asserted by perturbing a
      rendered value and requiring the projection to follow; the role-to-endpoint map's key set equals
      the role set. Removing `spec.router` prunes all six objects through P1's path and reverts
      `status.endpoint` to the deployment-wide Service; adding it renders them and
      refuses to adopt a same-named object it does not own. Under G3's answer, `status.endpoint` names the router only while the router has a
      ready replica, and a router with none leaves the deployment `Degraded` with the router named in
      `phaseMessage` — which requires phase derivation to take a second input beside the role counts,
      and the reference page's definition of `Degraded` to be updated in this task. **The endpoint's
      scheme comes from the router when the endpoint is the router's**, not from `roles[0]`, which is
      where the endpoint helper reads it today; publishing the first role's scheme in front of a
      router that does not speak it is the same defect that helper was just fixed for. An unrouted
      deployment's endpoint is unchanged, asserted, because a gate that blanks every endpoint passes a
      ready-only assertion.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/`
      and `make lint docs < /dev/null`

- [x] **T9 · The cluster case: rendering, ownership, deletion, both transitions, every condition state**
      Landed: `75084afc`, with the router runtime contract corrected in `15b75f3a` and the readiness
      case hardened against transient readiness in `13b1dda8`.
      Blocked by: T8
      Owns: two new e2e cases (`case-70`, `case-71`)
      Gate: review
      Acceptance: on a single-node cluster with no accelerator, a routed P/D deployment renders the
      router's six objects with owner references, stays `Starting` for lack of cards, and loses all
      six when deleted — the one place owner-reference collection is actually observed. Both
      transitions converge.
      The `KVEventsPublishing` condition is observed in each state a cluster can produce. **Whether
      the router becomes ready is a separate case** that SKIPs with a stated reason when no router
      image can be pulled, so the rendering and ownership assertions survive its absence.
      Verify: `bash .claude/skills/_e2e-lib/scripts/preflight.sh` then the two case scripts against a
      deployed namespace

- [x] **T12 · F10: make the shared cache optional**
      Blocked by: T5
      Owns: `api/worker/v1alpha1/model_deployment.go` and its generated artifacts,
      `pkg/worker/kvcache/inject/vllm.go`,
      `pkg/worker/controllers/worker/model_deployment_connector.go`,
      `pkg/worker/controllers/worker/model_deployment_cache_attached.go`, and their tests
      Gate: review
      Acceptance: F10's four criteria hold. A deployment with no `spec.kvCache` renders the
      point-to-point connector alone, resolves no Binding, fetches no shared cache, reports no KV
      event publisher on a take-over role, and reads `CacheAttached=True/NotApplicable`; the same
      deployment with `spec.kvCache` renders `MultiConnector` carrying both children with the outer
      `kv_role` following the role and the shared-store child taking `kv_both` on a prefiller and
      `kv_consumer` on a decoder.
      **The two shapes are asserted against each other rather than one at a time.** A test that only
      pins the absent case passes against a renderer that ignores the field entirely, which is the
      defect this task exists to prevent rather than to introduce.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/kvcache/inject/ ./pkg/worker/controllers/worker/`

- [ ] **T10 · F9: the measurement**
      Blocked by: a cluster with RDMA-capable nodes plus a serving engine image.
      **No such cluster is confirmed available.** A managed one is the intended venue and whether its
      nodes can serve the role is unknown until the attempt; that uncertainty is recorded here rather
      than turned into a date, because a task whose blocker is an unavailable machine does not become
      unblocked by being scheduled.
      Owns: nothing in the tree until it runs; its result is recorded in this spec's Test Plan
      Gate: review
      Acceptance: one request reaches a prefiller, its blocks reach a decoder, and the decoder
      produces tokens without recomputing the prompt. Recorded: time to first token against a
      single-role deployment of the same model, and the block count that moved. Which route the
      blocks take is F10's business, not this task's.
      **Nothing above depends on this task**, and no acceptance criterion above is written in terms of
      it. That separation is deliberate and is the one structural lesson taken from S6.
      Verify: recorded by hand from the run; there is no command that produces this on this hardware

### Deferred follow-up: user-visible KV cache usage

The live-engine measurement found three distinct cache-observability surfaces that a future spec
can expose without treating them as interchangeable:

- vLLM exposes local engine utilisation as `vllm:kv_cache_usage_perc`, prefix-cache demand as
  `vllm:prefix_cache_queries_total`, prefix-cache reuse as `vllm:prefix_cache_hits_total`, and the
  configured cache capacity through `vllm:cache_config_info`.
- The selected router exposes routing and disaggregation decision counters. Those counters answer
  whether requests were assigned to prefill and decode roles; they do not measure memory occupied by
  KV blocks.
- `KVCachePoolBinding.status.usage` and `status.blocks` describe shared-pool allocation. They do not
  describe either engine's local GPU cache.

This is deliberately recorded as a follow-up rather than added to this feature's API. Rapidly
changing gauges and counters belong on a Prometheus metrics surface; continuously copying them into
Kubernetes status would create API-server and storage write churn. A later specification may add a
bounded, low-frequency status summary for human inspection, but it must define freshness, aggregation
across replicas, and the distinction between local engine cache, router decisions, and shared-pool
usage. It must also verify the telemetry with request-before/request-after deltas: zero usage after a
failed request is not evidence of a successful KV transfer.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- **Done, with T3.** The pod-group test that pins **the exact label and annotation key set** a replica
  carries went red the moment T3 added a key. It was updated in T3, and the update states in its own
  message that the new key is in this project's domain and is not a second carrier of the role's
  *name*.
- A **take-over-tier fixture for a producing role** — a `prefill` role with a non-empty
  `template.command` — does not exist. It is what makes F6's `Unknown` state reachable, and without it
  that state is vocabulary no test can exercise.
- A **router-object fixture** (Deployment, ConfigMap, Service already present and owned, or present
  and *not* owned) is needed for the adoption-refusal and prune cases. The Service path has the
  equivalent already and its shape is reused.
- Existing render golden fixtures are reused **as regression baselines**: a `kind: server` single-role
  render must differ from today's only by the one new label.
- The e2e suite needs a way to run without a pullable router image, so the readiness case can SKIP
  with a reason rather than fail. The case picks one and states which.

#### Unit tests

New and extended packages carry table-driven coverage of every case below and must not regress the
package they live beside. Per-package coverage is measured with `go test -cover` when the tasks land;
there is no threshold in this repository and none is invented here.

- `pkg/worker/webhooks/worker`: `<date>` - `<coverage %>`
- `pkg/worker/controllers/worker`: `<date>` - `<coverage %>`
- `pkg/worker/kvcache/router`: `<date>` - `<coverage %>`
- `pkg/worker/kvcache/inject`: `<date>` - `<coverage %>`

**Admission cases** (`pkg/worker/webhooks/worker`) — every rule gets a refusal, a baseline, and a
baseline re-run with the rule violated.

| Case | Fixture | Expected |
|---|---|---|
| `router_absent_on_pair` | prefill + decode, no `spec.router` | **accept**; F6 reports the gap |
| `router_with_all_fields` | `spec.router` with `replicas`, `image` and `extraArgs` all set | accept |
| `two_prefill_roles` | prefill + prefill, distinct names | reject |
| `two_server_roles` | server + server | **accept** — the rule says "other than server" |
| `prefill_plus_decode` | one of each | accept |
| `router_extra_arg_derived` | a flag the router rendering derives | reject; names the flag and the field path |
| `router_extra_arg_free` | an unrelated flag | accept |
| `engine_missing_metric` | engine lacking a required metric | reject; names metric and engine |
| `engine_table_covers_enum` | every value of the engine enum | each has a table entry |
| `router_name_changed` | stored `llm-d`, incoming a different name — built by calling the validator directly, since the schema refuses the second value first | reject; names `spec.router.name`, message says immutable and nothing more |
| `router_name_unchanged` | the same name on both sides | accept |
| `router_added` | stored without `spec.router`, incoming with it | accept |
| `router_removed` | stored with `spec.router`, incoming without it | accept |
| `router_freeze_message_is_not_identity` | the freeze refusal beside an identity refusal | the two messages differ |
| `sglang_cannot_take_a_kind` | `sglang` + `kind: prefill` | reject, by the existing rule; asserted so the scope fact has a gate |

**Render cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `kind_label_prefill` | `kind: prefill` | the label carries `prefill` |
| `kind_label_decode` | `kind: decode` | the label carries `decode` |
| `kind_label_server` | one `server` role, no router | the label carries `server` — it renders for every deployment |
| `kind_label_not_in_selector` | any | the Service selector equals the render with the label absent |
| `kind_label_changes_the_hash` | `prefill` → `decode` | the Pod spec hash moves; the rollout is the documented one |
| `server_render_differs_by_one_label` | single `server` role | byte-identical to today's fixture but for the new label |

**Router-object cases** (`pkg/worker/kvcache/router`, `pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `router_objects_owned` | `spec.router` set | all six objects carry the owner reference and the resource note |
| `router_pod_has_no_queue_label` | `spec.router` set | no `kueue.x-k8s.io/queue-name`, no accelerator request |
| `config_hash_is_deterministic` | two renders of one spec | identical hash; no roll |
| `config_hash_ignores_map_order` | same content, different iteration order | identical hash |
| `scale_does_not_roll` | a role's `replicas` changed | hash unmoved, pod template untouched — the no-restart property at L0 |
| `config_holds_nothing_scale_derived` | the rendered configuration | contains no replica list, count or per-Pod address, asserted over the whole rendered content rather than by looking for known keys |
| `config_change_rolls_once` | one field edited | the annotation moves once; the next pass writes nothing |
| `out_of_band_delete_is_repaired` | ConfigMap deleted | recreated on the next pass |
| `adoption_refused` | a same-named Service the deployment does not own | refused, with the existing message |

**KV event cases** (`pkg/worker/controllers/worker`, `pkg/worker/kvcache/inject`).

| Case | Condition | Expected |
|---|---|---|
| `prefill_publishes` | routed pair | the producing role carries the publisher configuration |
| `decode_not_required` | routed pair | the consuming role is not required to publish |
| `publisher_port_is_a_container_port` | routed pair | the port appears on the producing role's container |
| `published_address_is_dialable` | routed pair | `status.router` carries an address, never the bind string |
| `take_over_role_gets_nothing` | producer with `template.command` | no publisher configuration synthesized |
| `owned_key_refused_in_extra_args` | the key set through `roles[].extraArgs` | refused by the existing rule |
| `owned_key_documented` | the owned-key catalog | every new key appears in the reference page's row |

**Condition cases** (`pkg/worker/controllers/worker`) — each asserts the reason, not only the status.

| Case | Condition | Expected |
|---|---|---|
| `single_server_not_applicable` | one `server` role, no `spec.router` | `True` / `NotApplicable` |
| `routed_servers_do_publish` | two `server` roles + `spec.router` | NOT `NotApplicable` — a routed server pool has a consumer |
| `pair_without_router` | prefill + decode, no `spec.router` | `False` / `NoRouter` |
| `publisher_disabled` | routed pair, producer not configured | `False` / `PublisherDisabled`, naming the role |
| `producer_unmanaged` | routed pair, producer took over the command | `Unknown` / `RoleUnmanaged` |
| `all_configured` | routed pair, producer configured | `True` |
| `survives_second_reconcile` | unchanged spec, second pass | present, unchanged, transition time unmoved |

**Status and transition cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `router_status_follows_the_render` | a rendered value perturbed | the projection changes with it |
| `role_endpoint_map_is_exact` | two roles | the map's key set equals the role set |
| `router_removed_prunes` | `spec.router` removed | all six objects deleted; `status.router` cleared |
| `router_added_renders` | `spec.router` added back | all six created |
| `endpoint_absent_while_router_unready` | router with zero ready replicas | endpoint does not name the router |
| `endpoint_present_when_router_ready` | router ready | endpoint names the router |
| `unrouted_endpoint_unchanged` | no `spec.router` | endpoint is today's value — the guard against a gate that blanks every endpoint |
| `degraded_names_the_router` | roles ready, router down | `Degraded`, `phaseMessage` names the router |
| `degraded_names_the_replicas` | router ready, some roles down | `Degraded`, today's message |

#### Integration tests

This repository has **no envtest**; these run against the controller-runtime fake client in the same
packages, driving whole reconcile passes rather than single functions. Concrete test names are added
after the implementation pull request merges.

- **The whole-group pass with a router**: one reconcile issues the creates for every role's every
  replica *and* the router's six objects, and a second pass over an unchanged spec writes nothing.
- **The transition, both directions**, driven through the reconciler rather than through the
  renderer, so the prune path is the one exercised.
- **The watch predicate**: an object rendered without the resource note is not enqueued — asserted so
  that a missing note fails here rather than as an un-repaired out-of-band edit in production.
- **Owner references are asserted as set, never as collected.** The fake client runs no garbage
  collector, so a case that deletes the deployment and finds the children gone would be asserting
  nothing. Collection is T9's.

#### e2e tests

Run against a local single-node Kubernetes cluster with the operator deployed. No RDMA, no cloud, no
accelerator.

- **The rendering and lifecycle case.** A routed P/D deployment; the router's six objects exist,
  are owner-referenced and carry the resource note; the deployment reports `Starting` for lack of
  cards; deleting the deployment removes all six. Both transitions converge. Each `KVEventsPublishing` state a cluster can
  produce is observed with its reason.
- **The readiness case, separate.** The router Deployment becomes Ready and `status.endpoint` names
  it. **SKIPs with a stated reason when the router image cannot be pulled** — bundling it with the
  case above would lose that case whenever the image is unavailable, which is the state OQ1 leaves
  the repository in until it is answered.
- **Not covered, and stated so:** that a request completes end to end. That is F9, it needs RDMA
  hardware, and no cluster case substitutes for it.

## Alternatives

**Refusing a router in front of `server`-only roles.** Previously chosen and then **overturned**. It
was justified by "there is no pair to route between", which presumes a router's job is pairing a
prefiller with a decoder. A router here is east-west traffic management: several plain servers is a
supported shape, and so is x prefillers with y decoders. A router that scores on a cache view picks
between equals in a way a Service cannot, so the absence of a pair is not a reason to refuse one.

The refusal is recorded here rather than left absent because a rule nobody proposed and a rule that
was proposed and overturned look identical once deleted, and the next reader would otherwise pay to
re-derive it. Its removal also reaches `KVEventsPublishing`: the `NotApplicable` reason keyed on the
deployment having one `server` role, which made a ROUTED server pool read as having nothing to
publish for — the shape that most needs the events. That reason now keys on the absence of
`spec.router`.

**A router CRD.** Rejected: it makes the router an API object the operator must then keep compatible.
A router configured by a ConfigMap and deployed as an ordinary Deployment keeps the compatibility
surface ours.

**Deploy no router, publish contracts only — as a selectable mode.** Previously chosen and then
**overturned**, in the same decision that removed `spec.router.mode`. The owner's reason is that under
such a mode `name`, `replicas`, `image` and `extraArgs` are all filled in for a router this operator
never runs, which makes them meaningless fields a user still has to reason about. A cluster that runs
its own endpoint picker declares no `spec.router` at all, which needs no mode to express.

A second reason belongs beside it because it decides the ORDER rather than the outcome: a two-valued
`mode` with only one value implemented carries no information — every object holds the same value —
while adding the choice later is an optional field with a default, which is backward compatible.
Shipping the choice first and then changing what its values mean would not be. So deferring costs
nothing and shipping early cannot be undone cheaply.

**A deployment-wide Service selecting every role.** Rejected before this spec existed, and the reason
is recorded beside the renderer: it round-robins a request onto a process configured as a producer
and one configured as a consumer. It is named here because the shape is the obvious first idea and
its rejection is what makes a router necessary rather than optional.

**Restore `roles[].acceleratorKey` to place prefill and decode on different accelerators.** Out of
scope and separately tracked at #199. It is a placement question; this spec is a routing one, and
coupling them would make the routing work wait on an API decision it does not need.

**Consume Mooncake's KV event flags directly.** Those flags are deprecated in favour of an indexer
API that was closed as not planned. Building against them means building against a pointer to
something that does not exist.

## Open Questions

**OQ1 — which router.** The rendering differs per router; most of the contracts do not. Four
candidates, and this is an owner decision because the choice is visible to users and hard to reverse.
**It blocked T4a, T4b, T6, the router-dependent half of T5, F8's router-dependent rules, and T9's
golden objects.**

**ANSWERED: `llm-d`, chosen for now.** The qualifier is the owner's and it is load-bearing rather
than hedging: it is why `spec.router.name` exists as a discriminator ahead of a second
implementation. A router here manages east-west traffic, so the shapes behind it include several
plain servers and x prefillers with y decoders, and the selected router must serve both.

**One criterion is a filter rather than a trade-off, and it is applied first.** A candidate must
discover its backends at runtime and keep serving across a scale event without restarting — the third
load-bearing property in the Summary. A candidate that takes its backend list as start-up arguments,
or snapshots it once at boot, is **ineligible**, and no amount of favourable behaviour elsewhere
buys it back. The column below is the one to fill first; every entry in it is marked unverified
because it decides eligibility and must be read off the candidate's current release rather than off
this document.

| Candidate | Survives a backend scale without restarting? | For | Against |
| --- | --- | --- | --- |
| vLLM router | **UNVERIFIED.** It registers prefill and decode endpoints; whether registration is dynamic or a start-up list is the thing to check | Native P/D pairing with prefill and decode registered separately; Mooncake is among its supported KV transports | Its cache-aware mode maintains its own approximate per-worker tree and does not consume the engine's KV events, so it is prefix-aware rather than KV-aware — which would leave F4's managed half without a consumer |
| SGLang Model Gateway | **UNVERIFIED.** Its P/D mode is described as discovering roles through selectors and annotations, which is the right shape — confirm it re-discovers rather than snapshotting | Cache-aware by default; P/D mode discovers roles through selectors and annotations, which matches F3 | Replica radix trees do not synchronise, with a stated hit-rate cost. **And this repository cannot render an SGLang P/D role at all today**, so choosing it either routes vLLM roles with SGLang's gateway or pulls the engine-rendering gap onto this spec's critical path |
| kthena router | **UNVERIFIED.** ConfigMap-driven, so the question is whether the backend set lives in that ConfigMap (which would roll it) or is discovered | Configured by a ConfigMap rather than a CRD, and explicitly deployable behind a standard gateway | KV-awareness goes through a sidecar and Redis, which is a second datastore to run. **The "not a CRD" characterisation is from the sources this spec was written against and may be out of date** — verify against the current release before the answer is locked |
| llm-d-router | **UNVERIFIED.** An endpoint picker is by construction dynamic, which is the strongest prior of the four — confirm against the release | Where the reference endpoint picker now lives; one picker covering all roles, which is the shape this spec is written against | Heaviest to operate: it is an Envoy plus endpoint-picker architecture rather than a single Deployment, so F2's "one Deployment, one ConfigMap, one Service" may not describe it |

**OQ2 — does the rendering support more than one router, or exactly one at first?** Supporting one
makes the rendering tasks concrete. Supporting several makes the dispatch real from the start. The
structure in "Code Style" allows either; what was not decided is whether the first delivery ships one
entry or two.

**ANSWERED: exactly one at first, and it is `llm-d`.** This question is answered rather than
cancelled, and the distinction matters because `spec.router.mode` was removed in the same round: that
removal took away a field, while this question was about how many router *renderings* the first
delivery carries. Nothing about it depended on the mode existing, so its premise is intact and its
answer stands on its own.

**Inside OQ2, and separable from it: does `spec.router` carry a field naming which router?** This is
the half that blocks T1, and it is answerable without answering OQ1. Reserving a discriminator now
means a second router is an enum widening; not reserving one means a second router has to
re-interpret a field that was absent, and every stored object's meaning shifts under it. The
`kvCache.connector` field took the first path for exactly this reason and says so in its comment. The
API is unreleased, so the second path is survivable — but it is survivable by breaking, not by
extending.

**OQ3 — should `status.endpoint` change when a router is rendered?** This spec says yes: the router
becomes the address. **It blocks T8 and flips T9's endpoint assertions.**

**ANSWERED: yes.** The router is the entrance, so `status.endpoint` names it while one is rendered
and reverts to the deployment-wide Service when none is.

The alternative — leave `status.endpoint` where it is and add a separate router address — is weaker
than it first appears. The field does not today point at a fan-out; it points at whichever role was
declared first, and now publishes that role's scheme as well as its port. So for a disaggregated
deployment the "compatible" choice preserves an address that was already the wrong door, and
preserves it for a client that could not have been using it successfully.

**Settled while this spec was being written: whether a router may be added to or removed from a live
deployment.** It was carried here as a fourth open question — whether a frozen `spec` leaves F7's
transitions representable at all — and the stabilisation work answered it by changing the rule rather
than the list: what is immutable is a deployment's **identity**, and the router in front of a
deployment is not part of which deployment it is. So both transitions stand, and F7 states the reason
in those terms rather than in terms of the outage a delete-and-recreate would cause. That framing is deliberate: the outage is why
anyone cares, but it is not why the field is editable, and a later field argued in on the outage
alone would be argued in on a criterion this project does not use.


**OQ4 — which point-to-point connector, and whether the choice belongs to the user.**

F10 renders vLLM's Mooncake point-to-point connector for the prefill/decode handover. Two readings
are in front of us, and they are not equivalent:

- **It stays one value.** The operator picks it, and `spec.kvCache.connector` — an enum with one
  member, read by no code — never widens.
- **It becomes a choice.** That enum widens, or a field is added outside `spec.kvCache` so that the
  choice stays reachable when no shared pool is attached.

**The second reading has a shape problem that must be solved before the enum is touched.**
`spec.kvCache.connector` lives inside `spec.kvCache`, and F10 just made that optional. So the
deployment that most needs to name a point-to-point connector — the one with no shared pool, where
that connector is the ONLY thing carrying blocks — is exactly the one that cannot reach the field.
Widening the enum in place would leave the choice unreachable in the shape where it matters most.

**An enum member is a promise.** Each value published there tells a user "this works here", and the
evidence behind each one is different.

The Mooncake point-to-point path selects its transport from what the LOCAL host can see rather than
from what the peer can reach: it constructs its engine with auto-discovery ON, and the discovery
branch installs a cross-node NVLink transport whenever the host reports no RDMA device. A pair of
single-accelerator nodes with no fabric between them therefore installs a transport that cannot
reach the peer, and fails at transfer time rather than at start-up — the quiet direction. The
build shipped in the engine image today compiles that branch in and carries no environment
variable to force TCP.

**The shared-store path does NOT share that defect, and the difference is structural rather than
incidental.** Its client constructs the engine with auto-discovery OFF and installs the transport
this operator renders. That is why the two halves can be reasoned about separately at all, and it
is what makes replacing only the point-to-point half coherent.

**But it is conditional, and the condition is ours to hold.** Auto-discovery turns back ON for the
shared-store path when the rendered protocol is `rdma` or `efa` AND no device name is given. A pool
rendered that way puts the same defect back into the half this spec just called safe. Whatever
answers this question must state that as a constraint on what the pool may render, not leave it as
a property the store path is assumed to have.

**Not answered, and it blocks nothing currently planned**: F10 renders one connector, and the field
it would widen is read by no code today. It has to be answered before that enum gains a second
member. The evidence needed is not "does the alternative work" in isolation — it is whether the
COMBINATION this operator would render has ever been exercised, and upstream has no test covering
one. So widening the enum has to be tied to this project's own cluster verification rather than to
an upstream document recommending the shape.
