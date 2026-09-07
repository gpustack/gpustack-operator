# Model Deployment Reference

> **Purpose** — the `ModelDeployment` contract: what you declare, what the operator owns and will
> refuse to merge, how a role's runner image is assembled, and what each status condition means.
> **Audience** users, operators, contributors · **Prerequisites** [KV Cache Pool](../kv-cache/pool.md) ·
> **Read time** ~17 min

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
- [Status](#status)
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
  engineVersion: "0.25.1"                # free-form; you guarantee alignment
  kvCache:
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

`roles` takes **1 to 10** entries. The upper bound is Kueue's rather than this operator's — every role
becomes one PodSet of the deployment's single Workload, and `Workload.spec.podSets` is capped at ten —
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
never arrive.

```yaml
  roles:
    - name: prefill
      kind: prefill                        # server (default) | prefill | decode
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64
      parallelism: 2                       # tensor parallel degree, per role
      resources:
        accelerator: 2
    - name: decode
      kind: decode
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64   # identical, and it must be
      parallelism: 2
      resources:
        accelerator: 2
```

`name` identifies the role and becomes the Kueue PodSet name; `kind` selects behaviour and is closed.
They are separate because a semantic reachable by typing a free-form string is a semantic one typo away
from silently changing. Two roles may share a `kind` and differ in `name`.

`kind` adds **one term** to the synthesized transfer configuration — the role discriminator the engine's
own KV-transfer configuration takes, vLLM's `kv_role` and its per-engine equivalents. Nothing else
changes. Pairing a prefiller with a decoder, negotiating a transport between them, and routing a
request to either are **not** here.

### What every Pod of the group carries

| Key | Value | What it is for |
|---|---|---|
| label `kueue.x-k8s.io/pod-group-name` | the deployment's name, or `gpustack-fnv64-<hash>` when that is too long for a label value | membership: it is what makes the replicas one group |
| annotation `kueue.x-k8s.io/pod-group-total-count` | the sum of every role's `replicas` | how many Pods Kueue waits for before composing anything |
| annotation `kueue.x-k8s.io/role-hash` | the role's `name` | the PodSet's identity, so two identically-shaped roles stay two PodSets |
| annotation `kueue.x-k8s.io/pod-group-serving` | `"true"` | an inference deployment never finishes; without it Kueue reclaims the quota of a replica that exited |
| annotation `modeldeployment.gpustack.ai/role-replicas` | the role's own `replicas` | ours, not Kueue's, and the only entry here Kueue does not read. It is what makes the rebuild predicate see a **reshape**: moving prefill 2 / decode 2 to prefill 1 / decode 3 leaves the total at four, so a check reading the total alone would trim one replica and add another in the same pass |
| label `kueue.x-k8s.io/queue-name` | the `status.entrance` **published by** the role's InstanceType | unchanged; Kueue refuses a group whose Pods disagree on it. Read from the type rather than re-derived from its name, so this operator and the reconcile that creates the LocalQueue cannot disagree about the queue |
| label `app.kubernetes.io/component` | the role's `name` | unchanged; what a `Service` selects on and what `status.roles[]` is attributed by |
| `spec.nodeSelector` | nothing is added | a role takes whatever flavor its pool assigns. Kueue evaluates a candidate flavor per PodSet, and with no selector to match against there is nothing to narrow the choice within one pool |

The `role-hash` annotation is load-bearing rather than cosmetic. Kueue takes it verbatim when present
and otherwise derives a digest of the Pod spec's *shape*, so two roles that render identically would
collapse into one PodSet holding both their replicas — and per-role counting, per-role flavor
assignment and per-role status would all disappear with nothing erroring.

> **`kueue.x-k8s.io/pod-group-fast-admission` must never be set.** That path composes the Workload from
> the first runnable Pod alone and gives that single PodSet the whole group's total, so every role
> collapses into one and per-role flavor assignment goes with it. The Workload still looks well formed.
> The operator never sets it, and a test asserts its absence.

### One `instanceType` for every role

A Kueue Workload carries one `queueName`, and that name is the one the role's `instanceType`
publishes as its `status.entrance`. So roles
on two `instanceType`s cannot be one group and cannot be admitted together at all — Kueue enforces the
same rule on the Pods, unretryably, so letting it through would trade a refusal for a group that never
assembles.

Different **hardware** per role is **not** expressible today, and the refusal above says so rather
than only saying "pick one type".

[Kueue does assign a ResourceFlavor per
PodSet](../architecture/scheduling-chain.md#stage-4-the-kueue-chain), so the mechanism exists; what
is missing is a way for a role to ask for one model rather than another within its pool. Per-role
queues are the route tracked at
[issue 199](https://github.com/gpustack/gpustack-operator/issues/199).

Across manufacturers stays impossible for a different and permanent reason: a queue's accelerator
quota is `credits.gpustack.ai/<manufacturer>`, one resource name per manufacturer.

### Addressing a role

Each role gets a `ClusterIP` Service named `<deployment>-<role>`, beside the deployment-wide one, so a
decoder is reachable **as** a decoder. Nothing in the operator dials these names — no router exists yet
— they are there for whatever pairs the roles.

`status.endpoint` stays the deployment-wide Service, which fronts the **first** role. It is not a
router and must not be read as one: a Service selecting every role would round-robin a request onto a
process configured as a producer and one configured as a consumer.

## The reuse domain is inherited

The reuse domain — `name`, `blockSize`, `dtype` — is a required, immutable block on the
`KVCachePoolBinding`. `ModelDeploymentSpec` has **no domain field**, and that is a security property
rather than tidiness.

> **Why** — a workload free to name its own domain could mint tenants and escape its namespace's
> quota ceiling. The mechanism is stated once, under
> [One Binding, one reuse domain](../kv-cache/pool.md#one-binding-one-reuse-domain).

The resulting semantics:

- Two deployments referencing the **same** Binding share KV.
- Two referencing **different** Bindings do not.
- Name matching between workloads disappears, and with it a whole class of typo.
- A namespace needing two reuse boundaries creates **two Bindings** on the same pool — the same shape
  as a namespace having several Kueue `LocalQueue`s.

`status.kvCache` echoes the Binding's `binding`, `pool` and the whole domain block, so an operator
reads the attached domain off this object alone. A wrong `blockSize` or `dtype` is silent cache
pollution: writes succeed, reads succeed, and the tensors are wrong.

**Whether the domain reaches the storage layer depends on the engine, and the answer is measured per
engine version rather than stated here.** `SupportsTenant` and `TenantSupportSource` in
`pkg/worker/kvcache/inject` carry it beside the version and source line it was read at.

Read that table before relying on the domain reaching the store; this page states no answer of its
own, because an answer written here goes stale silently while the table carries the version it was
measured at. What IS this page's own: the variable an engine reads the domain from is
operator-owned wherever one exists, so supplying it in `env` or `extraArgs` is refused — it is a
second path to a value [the API already refuses](#the-reuse-domain-is-inherited).

**"A tenant was injected" is not "the workload is isolated."** The operator records what it
rendered, never what the container did with it: whether the build inside the image reads the value
is not knowable at render time.

So on an engine that forwards, treat a second Binding as a boundary the operator asked for, not one
it verified. On one that does not, two deployments on **two** Bindings share one cache today. Either
way the semantics are this API's and the enforcement is the engine's — the same caveat
[KV Cache Pool](../kv-cache/pool.md#what-a-binding-does-not-do) states for capacity.

> **Why this page names no engine's answer** — an answer copied to a second place is a second
> implementation of it: the copy and the table agree today and diverge on whichever release lands
> next, with nothing failing in between. This page previously said no supported engine could receive
> a tenant, reasoning that `tenant_id` is the 11th positional parameter of the store client's
> `setup()` while every engine calls it positionally with seven or eight arguments. **That reasoning
> is a counterfactual now.** It measured the C++ client and the positional overload, and SGLang
> reaches the same parameter from a different direction — its Python layer reads
> `MOONCAKE_TENANT_ID` and passes the value as a keyword argument. "The client reads no environment
> variable" stayed true while "no tenant reaches the client" became false, because the measurement
> point sat downstream of the path that actually carries it.

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
| `vllm` | `--kv-transfer-config` | `MOONCAKE_CONFIG_PATH` |
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

`gpustack/runner:cuda12.9-vllm0.25.1` on an NVIDIA pool; `gpustack/runner:cann9.0-910b-sglang0.5.18`
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

## Status

`status.phase` is the field to read first: `Starting`, `Ready`, `Degraded` or `Deleting`. `Degraded`
means some replicas are ready and some are not — serving, at less than the capacity asked for.
`status.roles[]` carries `name`, `kind`, `desired`, `ready`, `unmanaged` and `assignedFlavor` per role,
and `status.endpoint` is the address the deployment-wide Service serves on, in the form
`http://<name>.<namespace>.svc:<port>`.

`assignedFlavor` answers *which accelerator model did this role actually get*, read from the group
Workload's per-PodSet assignment. It is **absent** rather than empty while no assignment exists,
because "not assigned yet" and "assigned to a flavor with no name" are different facts and one of them
reads as an answer.

**Absent does not only mean "not admitted yet".** The field reports the flavor of a role's
*accelerator* credits. A role admitted onto a pool carrying no accelerator at all reports nothing here
while being perfectly healthy — its Workload holds an assignment, naming a flavor for `cpu`.

That is the field's contract rather than a gap in it. The answer is read through the same lens the
per-accelerator admission gate uses, and a flavor reported here that the gate would not fit against
would be worse than none.

Three conditions carry the axes a single phase cannot. They are independent: "quota reserved but cache
not attached" is a real and actionable state.

**`DomainRegistered`** — whether the referenced Binding resolved and its domain was read.

| Value | Reason | Where it sends you |
|---|---|---|
| `True` | `Registered` | nowhere; the domain in `status.kvCache` is current |
| `False` | `BindingNotFound` | create the Binding — an admin doing so is what grants access |
| `False` | `BindingNotReady` | wait for it, or look at the pool it points at |
| `False` | `BindingDeleting` | find who deleted it; the replicas keep writing to the domain they attached to |

**`QuotaReserved`** — whether the deployment's **one** Workload holds Kueue quota. `True` therefore
covers every role by construction and cannot be true for one and not another. It reads that Workload's
own conditions rather than asking the admission gate, because the gate stops evaluating a Workload once
it is admitted: anything derived from the gate would answer for the moment of admission and never again.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `Reserved` | the group has quota reserved in the named cluster queue |
| `False` | `Pending` | the group is waiting for quota |
| `False` | `PodGroupIncomplete` | fewer Pods exist than the group declares, so Kueue composes **no Workload at all**; the message carries `<have>/<want>` |
| `Unknown` | `AdmissionInFlight` | the group is complete and has no Workload yet — Kueue composes it asynchronously, so absence is admission in flight, not refusal |
| `Unknown` | `NoReplicas` | no replica has been created yet |
| `Unknown` | `AllReplicasTerminating` | every replica is on its way out, so none holds quota to report on |

`PodGroupIncomplete` and `AdmissionInFlight` are the same observation — no Workload — and they mean
opposite things. One clears in a moment; the other is the deployment sitting with gated Pods and an
empty `kubectl get workloads` until something creates the missing replica. Telling them apart is why
the reason exists.

**`CacheAttached`** — whether the cache is observed to be in effect, which is a different question
from whether it was configured. It is judged downstream of the engine and **never** on a rendered flag
or a log line.

> **Why** — measured on the shipped store, `--enable_kv_events=true` is accepted, the startup log
> echoes `enable_kv_events=1`, and `GET /kv_events/status` still answers `{"enabled":false}` with the
> socket never bound. In the same project another undeclared switch fails loudly instead. One
> switch's failure mode cannot be inferred from another's.

> **Two rows are not reachable yet.** The per-replica reader this condition takes is an interface the
> reconciler is built with, and no concrete implementation is wired: the operator therefore reads
> every replica as giving no account. So the first `CacheActive` row and the `CacheOperationsFailing`
> row below describe the contract rather than current behaviour — today `True` is reached only
> through the domain-holds-data row, and a serving deployment whose every store operation fails reads
> as `Unknown`/`NoObservationAvailable`. The rows are documented rather than removed because the
> condition's semantics are what a consumer codes against, and they are marked rather than left
> implicit because a table a reader trusts must not describe a state the operator cannot produce.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `CacheActive` | *(not reachable yet)* a ready replica reports succeeding store operations |
| `True` | `CacheActive` | no replica gave an account, and the reuse domain holds data — this attributes to the domain, which is shared by every deployment on its Binding |
| `False` | `CacheOperationsFailing` | *(not reachable yet)* a ready replica reports store operations of which **none** succeeded; the engine is serving without the cache |
| `Unknown` | `Unmanaged` | a role took over its command line, so the operator rendered no cache client |
| `Unknown` | `NoReplicaReady` | no replica is ready, so no engine has an account to give |
| `Unknown` | `NoObservationAvailable` | ready replicas gave no account and the domain reports nothing held |

`NoObservationAvailable` is `Unknown` rather than `False` because an attached deployment that is
simply idle looks exactly like an unread one. No supported engine publishes anything that says "the
connector initialized" before any traffic, so reading silence as a detachment would be a false alarm
on the most common steady state there is. A connector that cannot come up takes its replica with it,
and that is already reported as a replica that never becomes Ready.

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

**Any replica leaving rebuilds the whole group.** This is stronger than the recreate policy above, and
it is a contract rather than a symptom. Kueue holds a finalizer on every Pod of the group and releases
it only when the group's Workload is deleted — a *serving* group is never finished, so nothing else
releases it — and deleting that Workload makes Kueue stop the group. A departing replica therefore
takes its siblings with it, and the deployment is rebuilt whole on the next pass.

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

One validating webhook is the whole admission surface for this CR; defaults live in the CRD schema,
so there is no mutating webhook.

| Refused | Message names |
|---|---|
| more than 10 roles | Kueue's 10-PodSet cap on `Workload.spec.podSets` as the cause, not merely the number |
| two roles sharing a `name` | the duplicate — refused by the **schema**, since `roles` is a list keyed on `name`, so this one never reaches the webhook |
| roles on different `instanceType`s | every type named, on `spec.roles` rather than on any one role — disagreement is a property of the set — plus the one-queue-name reason, and that differentiating hardware within one pool is not possible today |
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

**Every rule above is answered from the submitted object.** The handler holds no client and reads
nothing from the cluster, so admission cannot be delayed or made to fail by a cache that has not
caught up.

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
Service in front of the replicas takes that port, and `status.endpoint` reports it.

---

**See also** — [KV Cache Pool](../kv-cache/pool.md) for the Binding that grants the quota and declares
the domain · [Accelerator Requests](../accelerator-requests.md) for the request fields
`roles[].resources` mirrors · [Admission](../architecture/admission.md) for the gates a replica passes
as an ordinary Pod.

**Next** → [Accelerator Requests](../accelerator-requests.md)
