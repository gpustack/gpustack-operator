# Model Deployment Reference

> **Purpose** — the `ModelDeployment` contract: what you declare, what the operator owns and will
> refuse to merge, and how a role's runner image is assembled.
> **Audience** users, operators, contributors · **Prerequisites** [KV Cache Pool](../kv-cache/pool.md) ·
> **Read time** ~12 min

A `ModelDeployment` is N replicas of one or more inference-engine roles attached to a KV cache pool, so
that the replicas hit each other's cached prefixes instead of each re-computing the same prefill.

It renders **Pods** directly, which is why it needs no new admission gate: a Pod is a first-class
citizen of the chain in [Admission](../architecture/admission.md), so every rule there applies to a
replica unchanged.

## Contents

- [A minimal deployment](#a-minimal-deployment)
- [Prefill and decode](#prefill-and-decode)
- [The reuse domain is inherited](#the-reuse-domain-is-inherited)
- [The three override tiers](#the-three-override-tiers)
- [What the operator owns](#what-the-operator-owns)
- [The runner image is a formula](#the-runner-image-is-a-formula)
- [Rollout is recreate](#rollout-is-recreate)
- [What admission refuses](#what-admission-refuses)
- [Operating notes](#operating-notes)

## A minimal deployment

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment                    # namespaced, short name md
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct      # served, never provisioned
  engine: vllm                           # vllm | sglang
  engineVersion: "0.27.1"                # free-form; you guarantee alignment
  kvCache:                               # OPTIONAL; omit it and no shared pool is attached
    poolRef:
      name: team-a-dram                  # a KVCachePoolBinding IN THIS NAMESPACE
    connector: auto                      # the only value; defaulted
  roles:
    - name: server
      replicas: 4
      instanceType: gpustack-nvidia-a10g-linux-amd64
      resources:
        accelerator: 2                   # cards per replica
```

`poolRef` is a `LocalObjectReference` on purpose: naming another namespace, the cluster-scoped
`KVCachePool`, or a bare endpoint URL is unrepresentable rather than merely rejected. The Binding is
the authorization point — an admin creating one in a namespace is what grants that namespace access.

**`connector` has one value, and that is the transport convergence rather than a placeholder.** The KV
transfer converges on Mooncake because it is the implementation that supports heterogeneous prefill
and decode. NIXL and ROCm NIXL stay reachable; nothing in this repository has run them, and the
field's existence is not a claim that they would work.

What the enum reserves is the **discriminator**, so naming a second connector later is a widening
rather than a new field. Widening it is four things and not one: a sub-package, an entry in the enum,
a renderer, and the wiring that threads this value to a dispatch point that does not exist yet.

Today the value reaching the renderer is synthesized from the engine, the role's `kind` and the
pool's backend, and this field is read by nothing. It is also frozen after creation, so a widened
enum reaches new deployments only.

`roles` takes **1 to 10** entries. The upper bound is Kueue's rather than this operator's — every role
becomes one PodSet of the Workload its group composes, and `Workload.spec.podSets` is capped at ten —
and it lives in the validating webhook rather than in the schema so the refusal can say whose limit it
is, and so tracking an upstream number is not a schema change every stored object must survive.

`replicas` and `instanceType` are structured fields and stay so: they are inputs to Kueue PodSet
counts and flavor selection, so a template able to shadow them would make the feasibility check read
a ledger that does not match reality.

Each replica's accelerator request lives in `roles[].resources`, whose fields mirror
[Accelerator Requests](../accelerator-requests.md). CPU, memory and ephemeral storage are **derived**
from the InstanceType's per-unit resources scaled by the card count, so they are not expressible here.

## Prefill and decode

Several roles in one deployment are admitted **atomically**: a pool that cannot fit all of them leaves
all of them queued, instead of admitting the prefillers and stranding them waiting for decoders that
never arrive. Roles sharing one `instanceType` get that from Kueue's own pod-group rule; roles spread
over several get it from an admission check this operator runs, which holds every group until the
whole set has reserved quota.

```yaml
  roles:
    - name: prefill
      kind: prefill                        # server (default) | prefill | decode
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64
      resources:
        accelerator: 2
    - name: decode
      kind: decode
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64   # may differ; see Different hardware per role
      resources:
        accelerator: 2
```

> **A role's parallelism degrees are not API fields.** They reach the engine through
> `roles[].extraArgs`, spelled the engine's own way, and the operator neither reads nor validates
> them. They do not determine the AscendDirect transfer-port window.

`name` identifies the role and becomes the Kueue PodSet name; `kind` selects behaviour and is closed.
They are separate because a semantic reachable by typing a free-form string is a semantic one typo away
from silently changing.

Two roles may share a `kind` and differ in `name` only where that `kind` is `server`: a pair of
servers is a set of equals, whereas nothing consuming these roles expresses a second prefiller, so a
deployment declaring one would render a role nothing downstream can reach.

Without `spec.router`, `kind` adds the role discriminator to the engine's shared-store connector and
nothing pairs the two roles.

With the managed `llm-d` router, what a native vLLM role runs depends on whether `spec.kvCache` is
set, and the two shapes are different documents rather than one with a field toggled:

| `spec.kvCache` | Connector rendered | What carries the blocks |
| --- | --- | --- |
| omitted | `MooncakeConnector` alone | the direct prefill-to-decode transfer, and nothing else |
| set | `MultiConnector` wrapping `MooncakeConnector` and `MooncakeStoreConnector` | the direct transfer, with the shared pool attached alongside it |

A pair needs no shared pool to hand blocks over, which is why omitting `spec.kvCache` is a supported
shape rather than a degraded one. Attaching a pool adds reuse ACROSS deployments; it is not what
makes the pair work.

In both shapes the decode Pod runs llm-d's routing proxy as a restartable sidecar; it executes the
prefill leg named by the router and then forwards the request to the decoder.

SGLang has no P/D role rendering, and the Ascend connector has a different runtime-selected
transport contract, so neither is presented as this native vLLM path.

### Two ways to configure a pair

Both are complete objects. The only difference is the `kvCache` block, and it is the difference
between "these two roles hand blocks to each other" and "these two roles hand blocks to each other
AND share a pool with every other deployment bound to it".

**Without a shared pool** — the ordinary shape for a pair serving one model:

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: qwen-pd
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct
  engine: vllm                           # the native P/D path is vLLM only
  engineVersion: "0.27.1"
  router:
    name: llm-d                          # required to pair the roles; the only value today
  roles:
    - name: prefill
      kind: prefill
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64
      resources:
        accelerator: 2
    - name: decode
      kind: decode
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64
      resources:
        accelerator: 2
```

**With a shared pool** — the same object plus one block:

```yaml
spec:
  # ... everything above, unchanged ...
  kvCache:
    poolRef:
      name: team-a-dram                  # a KVCachePoolBinding in THIS namespace
```

**`spec.router` is what pairs the roles, and `spec.kvCache` is not a substitute for it.** A
deployment declaring `prefill` and `decode` with no router is admitted and renders two roles that
nothing routes between: each gets the shared-store connector with its role discriminator, and no
request is ever split across them. Attaching a pool does not change that.

Conversely a router with no pool is complete. What each shape renders is the table above.

`spec.router.replicas` is optional, and absent means one. More than one trades cache consistency for
availability: a router scoring on a prefix cache holds that state per replica — upstream reports radix
trees that do not synchronize across replicas and a hit rate falling by ten to twenty percent as a
result. Where replicas do exchange events, the exchange improves load estimation without making two
replicas route alike.

`spec.router.extraArgs` takes additional flags for the router process. A flag the operator derives
itself is **refused rather than merged**, so one setting has one source. The owned catalog is keyed by
router; for `llm-d` it is `--endpoint-selector`, `--endpoint-target-ports`, `--config-file`,
`--secure-serving`, `--grpc-health-port` and `--metrics-endpoint-auth`, and the refusal names the flag
and the router.

### The direct transfer's transport

The point-to-point leg renders `tcp` unless the deployment says otherwise:

```yaml
spec:
  directTransfer:
    protocol: rdma                     # unset renders "tcp"
```

The value is a property of **one link**, so it is deployment-wide: a per-role field could only
express two ends naming different protocols for one connection, which fails at transfer time rather
than at admission.

It is **declared, not discovered, and not gated**. The set an engine accepts belongs to the mooncake
build inside the engine's own image — a HIP-compiled build makes `hip` a working transport — so the
operator passes the value through verbatim, and a value the build rejects fails that container at
startup. It is read only on the direct-transfer leg (the managed `llm-d` router in front of native
vLLM prefill/decode roles); on every other shape it is accepted and renders nothing.

On Ascend the field has no consumer even beyond that gate: vllm-ascend's point-to-point connectors
(its own family — `MooncakeConnectorV1`, not the native name) initialize their transfer engine with
the protocol **hardcoded** to `ascend`, read from nothing (upstream `mooncake_transfer_engine.py`,
verified at v0.23.0 and v0.26.0rc1 — upstream state, not a contract, and it may change). A declared
value could only become meaningful there if upstream makes the protocol configurable.

It is also **not** the pool's transport. `KVCacheBackend.spec.transport` defines the data plane the
store members run and feeds the engine's store client; this leg is engine to engine and never
traverses the store, so the two declare separately — a deployment with no `kvCache` block still has
this leg to configure.

Editing it [restarts every role](#rollout-is-recreate): the value renders into both ends' arguments,
so every pod group rebuilds. With roles split across `instanceType`s the groups rebuild
independently, and a prefiller and a decoder can disagree on the protocol until both converge — the
same window an `engineVersion` edit opens.

### What every Pod of the group carries

| Key | Value | What it is for |
|---|---|---|
| label `kueue.x-k8s.io/pod-group-name` | the deployment's name when it forms ONE group, or `gpustack-fnv64-<hash>` when that name is too long for a label value or the deployment forms several | membership: it is what makes a group's replicas one group. Several groups hash the `instanceType` in, because a readable composite would share a namespace with deployment names and could equal one |
| annotation `kueue.x-k8s.io/pod-group-total-count` | the sum of the replicas of THIS group's roles, and of no others | how many Pods Kueue waits for before composing anything. A group claiming the deployment-wide total waits for Pods that are never coming |
| annotation `kueue.x-k8s.io/role-hash` | the role's `name` | the PodSet's identity, so two identically-shaped roles stay two PodSets |
| annotation `kueue.x-k8s.io/pod-group-serving` | `"true"` | an inference deployment never finishes; without it Kueue reclaims the quota of a replica that exited |
| annotation `modeldeployment.gpustack.ai/role-replicas` | the role's own `replicas` | ours, not Kueue's, and the only entry here Kueue does not read. It is what makes the rebuild predicate see a **reshape**: moving prefill 2 / decode 2 to prefill 1 / decode 3 leaves the total at four, so a check reading the total alone would trim one replica and add another in the same pass |
| label `kueue.x-k8s.io/queue-name` | the `status.entrance` **published by** the role's InstanceType | unchanged; Kueue refuses a group whose Pods disagree on it. Read from the type rather than re-derived from its name, so this operator and the reconcile that creates the LocalQueue cannot disagree about the queue |
| label `app.kubernetes.io/component` | the role's `name` | unchanged; what a `Service` selects on and what `status.roles[]` is attributed by |
| label `modeldeployment.gpustack.ai/role-kind` | the role's **effective** `kind`, so `server` when the field is unset | what something in front of the replicas selects on to tell a prefiller from a decoder. It is the resolved value rather than the field, because a selector matching the empty string would miss every replica of the default shape. Rendered for every deployment, a lone `server` included, so "no prefiller is running" and "this deployment does not label its roles" are different answers |
| `spec.nodeSelector` | nothing is added | a role takes whatever flavor its pool assigns. Kueue evaluates a candidate flavor per PodSet, and with no selector to match against there is nothing to narrow the choice within one pool |

The `role-hash` annotation is load-bearing rather than cosmetic. Kueue takes it verbatim when present
and otherwise derives a digest of the Pod spec's *shape*, so two roles that render identically would
collapse into one PodSet holding both their replicas — and per-role counting, per-role flavor
assignment and per-role status would all disappear with nothing erroring.

> **`kueue.x-k8s.io/pod-group-fast-admission` must never be set.** That path composes the Workload from
> the first runnable Pod alone and gives that single PodSet the whole group's total, so every role
> collapses into one and per-role flavor assignment goes with it. The Workload still looks well formed.
> The operator never sets it, and a test asserts its absence.

### Different hardware per role

A Kueue Workload carries one `queueName`, and that name is the one the role's `instanceType`
publishes as its `status.entrance`. So roles on two `instanceType`s cannot be one Workload — and the
answer is not to forbid the shape but to stop making it one Workload. Each `instanceType` is its own
pod group with its own Workload, and the set is admitted together by an admission check rather than
by Kueue's intra-group rule.

See [One group, or one per `instanceType`](#one-group-or-one-per-instancetype) for what that costs an
edit, and the last row of [What admission refuses](#what-admission-refuses) for the one state in
which the shape is refused instead.

**Across manufacturers is the same change, not a second one.** A queue's accelerator quota is
`credits.gpustack.ai/<manufacturer>`, one resource name per manufacturer, and Kueue's own webhook
refuses a second resource group repeating a covered resource within one queue. With a queue per role
there is no second group to repeat anything.

[Kueue assigns a ResourceFlavor per
PodSet](../architecture/scheduling-chain.md#stage-4-the-kueue-chain), so a role still takes whatever
its own pool assigns; what selects the hardware is the `instanceType` the role names.

### Addressing a role

Each role gets a `ClusterIP` Service named `<deployment>-<role>`, beside the deployment-wide one, so a
decoder is reachable **as** a decoder. The managed router uses these stable role addresses for the
tokenizer and cache-event contracts; they also remain useful for addressing one half directly while
debugging.

With `spec.router`, the operator renders six objects named `<deployment>-router`: a Deployment,
ConfigMap, Service, ServiceAccount, Role and RoleBinding. Removing `spec.router` prunes all six.
What `status.endpoint` publishes in each shape is under [Status](model-deployment-status.md#status).

## The reuse domain is inherited

The reuse domain — `name`, `blockSize`, `dtype` — is a required, immutable block on the
`KVCachePoolBinding`. `ModelDeploymentSpec` has **no domain field**, and that is a security property
rather than tidiness.

> **Why** — a workload free to name its own domain could mint tenants and escape its namespace's
> quota ceiling. The mechanism is stated once, under
> [One Binding, one reuse domain](../kv-cache/pool.md#one-binding-one-reuse-domain).

The requested semantics:

- Two deployments referencing the **same** Binding share KV.
- Two referencing **different** Bindings use different tenant identifiers. They are isolated only
  when their engine images read and forward those identifiers.
- Name matching between workloads disappears, and with it a whole class of typo.
- A namespace needing two reuse boundaries creates **two Bindings** on the same pool — the same shape
  as a namespace having several Kueue `LocalQueue`s.

`status.kvCache` echoes the Binding's `binding`, `pool` and the whole domain block, so an operator
reads the attached domain off this object alone. A wrong `blockSize` or `dtype` is silent cache
pollution: writes succeed, reads succeed, and the tensors are wrong.

For an operator-managed role, the operator renders a non-empty Binding domain as the engine's tenant
identifier. It does not inspect the engine image version or decide whether that build supports
tenant isolation. The tenant variable is operator-owned, so supplying it in `env` or `extraArgs` is
refused — it is a second path to a value [the API already refuses](#the-reuse-domain-is-inherited).

**"A tenant was injected" is not "the workload is isolated."** The operator records what it
rendered, never what the container did with it: whether the build inside the image reads the value
is not knowable at render time.

Users who require tenant isolation must select a compatible engine image and verify it themselves;
see
[Tenant compatibility is the image owner's responsibility](kv-cache-injection.md#tenant-compatibility-is-the-image-owners-responsibility)
for what the image must consume. The API states the requested boundary, while the engine enforces it
— the same caveat [KV Cache Pool](../kv-cache/pool.md#what-a-binding-does-not-do) states for capacity.

## The three override tiers

The engine command line is the fastest-moving thing in this design, so it has three escape tiers.
Without one, users patch the rendered Pod and the reconcile loop silently overwrites them.

| Tier | Field | Semantics |
|---|---|---|
| append | `roles[].extraArgs`, `roles[].env` | appended **after** the operator-synthesized arguments; a key the operator owns is refused, never merged |
| overlay | `roles[].template` | the operator renders first, then merges this overlay on top |
| take over | `roles[].template.command` | the user owns the whole argv; the operator synthesizes **no** engine argument and **no** client environment |

Unlike the `Instance` that shares the `InstanceTemplate` type, this template is **mutable** — that
immutability is a rule the Instance webhook enforces, not a property of the type, and dropping it is
what makes a rollout possible at all.

Arguments fold into `command`; there is deliberately no `args`. A second append tier beside
`extraArgs` would have no defined precedence, and would make the take-over tier ambiguous, since
`args` alone would be neither take-over nor append.

Taking over the command line has a visible cost: the role reports
`status.roles[].unmanaged: true` and `CacheAttached` moves to `Unknown`. The operator configured no
cache client for that role, so it does not report on one it did not render.

### A take-over role is outside the reuse-domain guarantee

⚠️ **A role that owns its whole argv can name any reuse domain, and this operator does not stop it.**
`MOONCAKE_TENANT_ID` is refused in `roles[].env` on the engines that own it — the table under
[What the operator owns](#what-the-operator-owns) is the authority — but `template.command` is a
program and its arguments, so the same value travels inside a shell assignment or inside the script
the argv names, and admission has nothing to read either way.

**This is a consequence of what the field is, not a protection waiting to be implemented.** Any check
would have to recover intent from an argv the tier exists to let the user write however they like, so
there is no version of the take-over tier that also bounds the domain.

Where the operator *does* build the argv, that refusal is real enforcement — the user cannot interpose
a shell, so the environment is the only path left. Why the key is owned at all is stated under
[What the operator owns](#what-the-operator-owns).

⛔ Do not read the above as the boundary of the exposure: a take-over role is one instance of the
mechanism, not the mechanism. The boundary is stated once, under
[What a Binding does not do](../kv-cache/pool.md#what-a-binding-does-not-do), and tracked at
[#168](https://github.com/gpustack/gpustack-operator/issues/168) — whose own void conditions include a
webhook-level one, so nothing here should be read as a claim about how that issue can be closed.

## What the operator owns

Ownership is per **(engine, key)**: a key one engine owns is an ordinary user argument on another.
`SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` is meaningless to `vllm` and is a plain user variable there.

| Engine | Owned arguments | Owned environment |
|---|---|---|
| `vllm` | `--kv-transfer-config`, `--kv-events-config` | `MOONCAKE_CONFIG_PATH`, `VLLM_MOONCAKE_BOOTSTRAP_PORT` |
| `sglang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config` | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME`, **`MOONCAKE_TENANT_ID`** |

One `vllm` row covers **both backends**. The owned keys follow the engine while only the connector
name follows the accelerator backend, so an Ascend pool and an NVIDIA pool running `vllm` own exactly
the same keys and differ only in the connector the operator names.

**Owned** means the operator refuses a user-supplied duplicate, because two values for one connector
argument cannot be told apart. The refusal names the key, the engine, and `template.command` as the
way to own it instead.

**Defaulted** is the other case, and `MC_TE_METRIC` is the one that matters: the operator sets it to
`1`, and a user's own value wins with no refusal. It turns on the transfer engine's metrics, without
which the hit rate this design rests on cannot be measured at all. It is read by the transfer engine
rather than by an engine's config class, so it does not depend on which keys that class accepts.

Two of SGLang's owned keys are owned for what a user entry would **destroy** rather than duplicate,
and the operator does not set either of them:

- SGLang picks its configuration source in the order extra-config argument, then config-path file,
  then environment. The operator leaves both of the first two unset, and that is what **selects** the
  environment loader.
- Each of the first two loaders falls back to a **compile-time literal** per key. So setting either
  one does not override a value: it silently replaces the whole configuration with defaults — a 4 GiB
  segment and a `localhost` identity.

SGLang needs `local_hostname`, which is the replica's own Pod IP. A file and an argument are both
fixed when the object is admitted, when no Pod IP exists yet, so only an environment variable with a
`fieldRef` on `status.podIP` can carry it — which is why this engine gets no config file at all.

Note that `MOONCAKE_CONFIG_PATH` is owned on `vllm` and **not** on `sglang`, while seven other
`MOONCAKE_*` names are owned on `sglang` alone. The table is the authority; a name prefix is not.

`MOONCAKE_TENANT_ID` is the one in that list whose ownership is a **security** property rather than a
consistency one: it carries the reuse domain, and a workload able to set it could write into another
Binding's domain. It is a second path to a value [the API already refuses](#the-reuse-domain-is-inherited).

On the vLLM family the operator mounts the rendered client JSON at
`/etc/gpustack/kvcache/mooncake.json`, read-only. **There is no ConfigMap**: the file is a downwardAPI
projection of the Pod's own `kvcache.gpustack.ai/client-config` annotation.

When `spec.router` is present, a role that produces cache blocks also receives a
`--kv-events-config` document. It enables the ZMQ publisher on `tcp://*:5557`, enables replay on
`tcp://*:5558`, retains 10,000 batches, uses a high-water mark and queue depth of 100,000, and
publishes topic `kv@`.

**That applies to the vLLM engine only.** A routed SGLang deployment renders no publisher arguments
and reports `KVEventsPublishing=False/PublisherDisabled`. That is the accurate reading rather than a
fault: nothing in that engine's rendering emits the stream.

The wildcard addresses are bind addresses only. The role's Service hostname with ports 5557 and
5558 is the dialable form published in status, and both ports are declared on the producing
container. A decode-only role does not need to publish. A role with `template.command` receives none
of this configuration because the operator does not own its command line.

Nothing is created beside the Pod, no RBAC for one is needed, and the configuration's lifetime is
exactly the replica's. It is also part of the Pod's spec hash, which is what moves the replicas when
the pool's published endpoint changes.

It sits under `/etc` rather than in the image's workspace so that a template's own volumes are
unlikely to collide — but an overlay that mounts over that path replaces the configuration silently,
and the owned `MOONCAKE_CONFIG_PATH` cannot protect against it. SGLang gets no file at all; its
configuration travels entirely in the environment.

## The runner image is a formula

A role with no `template.image` gets one assembled from the engine the deployment declares and the
hardware its InstanceType observed. A stated image always wins.

```text
gpustack/runner:<backend><runtimeVersion>[-<variant>]-<engine><engineVersion>
```

`gpustack/runner:cuda12.9-vllm0.27.1` on an NVIDIA pool; `gpustack/runner:cann9.0-910b-sglang0.5.18`
on an Ascend 910B one. The shape is verified against the runner project's 338 published records with
zero mismatches. The platform is **not** part of the tag: no published tag carries an architecture,
and the 338 records collapse to 208 distinct names, the signature of one multi-arch manifest each.

| This project's manufacturer | Runner backend |
|---|---|
| `nvidia` | `cuda` |
| `ascend` | `cann` |
| `amd` | `rocm` |
| `metax` | `maca` |
| `mthreads` | `musa` |
| `iluvatar` | `corex` |
| `hygon` | `dtk` |
| `thead` | `hggc` |
| `cambricon` | none — the role must name an image |

The variant applies to **Ascend only**: `310P` to `310p`, `910B` to `910b`, `910C` to `a3`, `950` to
`950`. Across the whole matrix the variant is populated for `cann` alone. Ascend `910` and `310B`
publish none, so a role on one of those must name an image.

`engineVersion` is required and non-empty — a schema `minLength`, not a webhook rule — and otherwise
**free-form**: the operator checks neither that the combination was ever published nor that the
version supports the installed driver. You guarantee
version alignment; a bad combination surfaces as an `ImagePullBackOff` on a tag that does not exist.

It is per deployment rather than per role, which is what lets one engine and one version assemble a
**different** image per role: the backend half of the tag comes from the role's own InstanceType.

Two synthesis failures read alike and are not: a manufacturer with no backend, or a family with no
variant, will **never** resolve and the role has to name an image, while an unobserved runtime version
resolves on a later reconcile. Each message says which one it is.

**A pool mid driver rollout does not agree on a runtime version.** The image takes the **lowest**
version the pool reports, because a workload's image is fixed before admission chooses its node and
only the lowest runs everywhere. The deployment then carries a `RuntimeVersionSkew` warning event
naming the value taken and the ones skipped, so the node holding the pool back is legible instead of
appearing as an unattributable `ImagePullBackOff`.

## Rollout is recreate

A spec change that changes a replica's rendered Pod **deletes and recreates** it. There are no surge
or unavailable knobs, and that is a decision rather than an omission: a rollout policy trades
availability against **cache** as well as against capacity, and choosing that trade needs the hit-rate
instrument this CR exists to build.

The cost is real and worth stating, and it rides on the block lease described under
[What a cache changes about a workload](kv-cache-injection.md#what-a-cache-changes-about-a-workload): a lease survives a long queue and does **not**
survive an interrupted heartbeat, which is what a departing replica is.

So a departing replica costs its siblings the blocks it held. The deployment records an event naming
the replica and the lease window on each of three paths — `ReplicaEvicted`, `ReplicaLeaving`,
`ReplicaRestarted` — so an operator correlating a burst of failed requests with a replica that went
away has the correlation written down rather than inferred.

**An upgrade can trigger the same rebuild without any spec edit.** The fingerprint covers a replica's
labels and annotations as well as its spec, so a release that adds a key every replica carries leaves
every existing replica stale and recreates it once. The `role-kind` label listed above did exactly
that. Nothing is required of you, but on a busy deployment the restart is worth scheduling.

### Which fields are the deployment's identity

Some fields cannot be edited at all, and the rule that sorts them is a question rather than a list:
**a field is frozen when it answers *which deployment is this*, and editable when it answers *how is
this deployment being run right now*.**

| Frozen | Editable |
|---|---|
| `model`, `engine`, `kvCache` | `engineVersion`, `directTransfer` |
| the set of roles, and each role's `name` and `kind` | `roles[].replicas` |
| `roles[].instanceType` | `roles[].extraArgs`, `roles[].env` |
| `roles[].resources` | the whole `roles[].template` except `command` |
| `roles[].template.command` | labels and annotations |

`roles[].resources` is frozen against the criterion rather than by it, and that is marked here so it
does not read as an oversight: it does not say which deployment this is, but changing it renegotiates
the scheduling, which is not materially different from deleting and recreating. Its mirror image is
`template.privileged`, which the criterion leaves editable even though a different argument could
move it.

**What to do instead of editing one is create another deployment.** A frozen field is not a lock
protecting a concurrent writer, and the refusal says so: what you are describing is a different
deployment, so it is created rather than edited. The name, the `status` history and the cache-pool
registration are what you keep by editing, and none of them is what a frozen field carries.

Judging a **new** field means asking that question, not appending to the table — a list alone grows
by precedent and stops meaning anything.

> **A merge patch that omits a frozen field is an edit to that frozen field.** `roles` is a list, and
> `kubectl patch --type=merge` replaces a list wholesale rather than merging into it — so a role
> restated without its `template` sets `template.command` to null, and the edit is refused naming
> that field rather than the one you meant to change.
>
> Change one field with a JSON patch (`--type=json`, `/spec/roles/0/replicas`), or send the whole
> object with `kubectl apply` or `kubectl edit`. This is not a quirk of the freeze: omitting a value
> in a merge patch IS setting it to null, and the rule is reading what you actually sent.

### One group, or one per `instanceType`

When every role names one `instanceType` the deployment is **one** pod group. When roles name
different ones it is **one group per type**, because a queue name is derived from the `instanceType`,
one Kueue Workload carries one queue name, and two of them therefore cannot be one Workload. The
grouping key is the `instanceType` and not the role: two roles on one type are one group, and a third
on another is a second.

**How expensive an edit is depends on which shape you are in**, and that is worth knowing where it is
not where anyone would look for it:

| Shape | What a `replicas` or `template` edit rebuilds |
|---|---|
| every role on one `instanceType` | every role of the deployment |
| roles split across types | only the group whose shape moved; the others keep serving |

So a user who wants cheap scaling has a reason to split `instanceType`s that has nothing to do with
hardware. The groups are still admitted as a **set** — see the last row of
[What admission refuses](#what-admission-refuses) for what happens when that gate cannot be installed.

**Any replica leaving rebuilds its group.** This is stronger than the recreate policy above, and it is
a contract rather than a symptom. Kueue holds a finalizer on every Pod of the group and releases it
only when the group's Workload is deleted — a *serving* group is never finished, so nothing else
releases it — and deleting that Workload makes Kueue stop the group.

A departing replica therefore takes the siblings **in its own group** with it, and that group is
rebuilt whole on the next pass.

**Most departures are not a spec change.** These all restart every role:

| Cause | Who initiates it |
|---|---|
| a `replicas` change, or adding or removing a role | you |
| a `template` edit, or any change to a replica's rendered Pod | you |
| **Kueue preempting** the deployment for a higher-priority workload | the scheduler |
| **a node being drained**, cordoned or replaced | the cluster |
| **the kubelet evicting** a replica under node pressure | the node |
| `kubectl delete pod` on one replica | you |

On a cluster with preemption enabled, a whole-deployment restart is therefore **routine rather than an
incident** — worth knowing before you chase one as a fault. It is also the only recovery available: an
evicted replica is held by Kueue's finalizer and cannot leave until the Workload does.

A shape change additionally takes **two passes**. The group's declared total is carried by every Pod
and Kueue requires them all to agree on it, so nothing is created while any Pod still declares the old
one.

## What admission refuses

Two webhooks make up the admission surface. Nearly every default lives in the CRD schema; the
mutating half exists for the one value a schema cannot reach — a role's accelerator count, which
depends on the `InstanceType` the role names.

| Refused | Message names |
|---|---|
| more than 10 roles | Kueue's 10-PodSet cap on `Workload.spec.podSets` as the cause, not merely the number |
| two roles sharing a `name` | the duplicate — refused by the **schema**, since `roles` is a list keyed on `name`, so this one never reaches the webhook |
| an edit to an identity field — `model`, `engine`, `kvCache`, or the shape of the roles | the field path, and that a different value describes a different **deployment**, which is created rather than edited. See [Which fields are the deployment's identity](#which-fields-are-the-deployments-identity) |
| a resource mode the named `InstanceType` does not offer | the mode and the type — a slice on a type that offers no slicing, a partition profile on a type that cannot partition, or one outside its profile inventory, with the offered list |
| a request over the type's per-unit ceiling | the ceiling itself, not only that the request was too large, so the next attempt is not a guess |
| an explicit `accelerator: 0` on an acceleratable `InstanceType` shared by another role | the accelerator field, the shared type, and two recommended remedies: request at least one accelerator or move the CPU-only role to a non-acceleratable type |
| a `prefill` and a `decode` role both requesting a **logical slice** from types that draw on the same accelerator group | both roles and the slice field. Whole cards and partition profiles are accepted — including on one card, because partitions are isolated by the device |
| roles on several `instanceType`s **when `instance-type-derived-from-node` is off** | that setting. The groups are gated as a set by an admission check this operator references from the queues it derives, and with the setting off no queue carries it |
| a role whose `<deployment>-<role>` is not a DNS-1035 label | the combined **Service** name, which is what the pair becomes; over 63 characters or carrying a dot from a subdomain-shaped deployment name. A role the object **already had** is exempt, so a rule added later cannot strand a stored object |
| `kind: server` beside any other kind | that a server serves whole requests by itself, so the combination describes no arrangement |
| a `kind` the engine has no term for | the engine and the kind — today, `prefill` or `decode` on SGLang |
| an owned key in `extraArgs` | the key, the engine, and `template.command` as the way to own it |
| an owned name in `env` | the same three |
| `template.resources` | `roles[].resources` and `roles[].instanceType` as where the request is decided |
| a partition profile together with a slice percentage | both slice fields; one accelerator cannot serve both |
| a `poolRef` outside this namespace | nothing — it is unrepresentable in the type |
| a self-declared reuse domain | nothing — the field does not exist |
| an EMPTY `poolRef.name` | the Binding as the authorization point, and that an empty reference names none |

**Most rules above are answered from the submitted object, and three are not.** Whether the named
`InstanceType` offers the mode a role asks for, whether the role's card count fits what that type
hands out at once, and whether a shared type is acceleratable are facts about another object;
everything else is decided without leaving the request. All three read the type from the API server
rather than from a cache, because a cache that is behind decides the outcome in both directions.

**Any rule that reads an `InstanceType` declines for a deployment being deleted**, in the mutating
half and the validating half alike. Such a rule refuses when the type is absent, so leaving it on
would let a deleted `InstanceType` block the very update that clears the deployment's finalizer — an
object its own teardown could never release.

The price is named rather than hidden: an edit made while a deployment is being deleted can move a
role onto a mode its type does not offer, and nothing renders the result. Why that trade is necessary
rather than merely tidy is in
[Update validation while an object is deleted](../architecture/admission.md#update-validation-while-an-object-is-deleted).

A manufacturer with no runner backend is still refused **at render time and not at admission**, and the
reason is not the missing client. The rule needs the InstanceType's OBSERVED detail, and
`InstanceType.status` has not converged on a freshly created object — so the rule would refuse a
perfectly legal deployment for losing a race against the InstanceType reconciler.

The render-time refusal reaches a reader as a `RenderFailed` warning event carrying the renderer's
own message, because a pass that cannot build a replica aborts before writing any status.

## Operating notes

**Two notes apply to every workload on a pool, replicas included, and are stated once under**
[What a cache changes about a workload](kv-cache-injection.md#what-a-cache-changes-about-a-workload): the transfer engine binds ports nobody
configured, so a NetworkPolicy or port reservation has to be a range rather than a list; and the
`transfer_metadata.cpp` "Local segment descriptor not found" line at startup is an `ERROR` that is
benign on a client mounting no segment of its own — which is what every replica here is.

**A replica serves on port 8000** unless the role's template names its own container port. The
Service and `status.endpoint` keep that external port. On a managed native-vLLM decoder the routing
proxy owns it and vLLM listens behind the proxy on an internal port; every other role tells the engine
itself to open the external port. The startup, readiness and liveness probes follow the external
listener, so a decoder becomes Ready only when the proxy can reach the engine.

### Transfer ports are runtime-selected

`roles[].template.ports` exposes container ports for the engine and Service. It neither reserves nor
selects transfer-engine ports. AscendDirect binds its transfer ports inside the container's own
network namespace, so a declaration here cannot prevent a collision with another process in that
same namespace.

AscendDirect calculates the transfer-port window only after scheduling, when it resolves a logical
device to its physical device ID. The device plugin decides that assignment, so admission cannot
know the window's position. Images can use different rules.

| Input | Rule |
|---|---|
| base port | `ASCEND_BASE_PORT`, or `20000` when unset |
| window | `base_port + physical_device_id * 100` through `base_port + (physical_device_id + 1) * 100`, inclusive |
| example on eight cards | physical device ID `7` can use `20700` through `20800` |
| selection | a port is chosen at random, with up to 500 attempts; the other transfer-port families are also random |

Parallelism and card count are not inputs to this calculation. Neither identifies the physical device
and therefore neither locates its window.

---

**See also** — [KV Cache Pool](../kv-cache/pool.md) for the Binding that grants the quota and declares
the domain · [Accelerator Requests](../accelerator-requests.md) for the request fields
`roles[].resources` mirrors · [Admission](../architecture/admission.md) for the gates a replica passes
as an ordinary Pod · [Model Deployment Status](model-deployment-status.md) for what each condition
and published field means.

**Next** → [Accelerator Requests](../accelerator-requests.md)
