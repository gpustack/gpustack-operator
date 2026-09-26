# Model Deployment Reference

> **Purpose** — the `ModelDeployment` contract: what you declare, what the operator owns and will
> refuse to merge, and how a role's runner image is assembled.
> **Audience** users, operators, contributors · **Prerequisites** [KV Cache Pool](../kv-cache/pool.md) ·
> **Read time** ~9 min

A `ModelDeployment` is N replicas of one or more inference-engine roles. It can attach to a KV
cache pool so replicas reuse cached prefixes, run a direct prefill/decode pair, or use both paths.

It renders **Pods** directly. The deployment's own validating webhook checks a new KV cache
binding's transport, because these generated Pods do not pass through the KV cache Pod injection
path; ordinary Pod admission still applies to each replica.

## Contents

- [A minimal deployment](#a-minimal-deployment)
- [Prefill and decode](#prefill-and-decode)
- [Topology placement](#topology-placement)
- [The reuse domain is inherited](#the-reuse-domain-is-inherited)
- [The three override tiers](#the-three-override-tiers)
- [What the operator owns](#what-the-operator-owns)
- [The runner image is a formula](#the-runner-image-is-a-formula)
- [Rollout is a rolling replacement](#rollout-is-a-rolling-replacement)
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
  engine:                                # vllm | sglang
    name: vllm
    version: "0.29.0"                    # free-form; you guarantee alignment
  kvCache:                               # OPTIONAL; omit it and no shared pool is attached
    poolRef:
      name: team-a-dram                  # a KVCachePoolBinding IN THIS NAMESPACE
    connector: mooncake                  # the only value; defaulted
  roles:
    - name: server
      replicas: 4
      instanceType: gpustack-nvidia-a10g-linux-amd64
      resources:
        accelerator: 2                   # cards per Pod
```

`model.name` is what the engine serves. The weights come from the engine's own hub client, a role's
volumes, or a `ModelArtifact` named by `model.artifactRef` — see the
[Model Artifact Reference](model-artifact.md).

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

`roles` takes **1 to 10** entries, and the upper bound is **this operator's own shape limit** rather
than an upstream one. Each replica composes a Workload of its own carrying a single PodSet, so no
Kueue bound constrains how many roles a deployment declares; ten is what a prefill/decode deployment
needs with room to spare, and raising it is a product decision.

It lives in the validating webhook rather than in the schema so that the refusal can explain itself,
and so that changing it is not a schema change every stored object must survive.

`replicas` counts **independent serving instances**: each one starts, serves and is replaced on its
own. Changing the number adds or removes instances, and the ones that survive are not restarted —
they keep serving without interruption and keep whatever cache they hold.

`size` is how many Pods form one instance, defaulting to 1. The Pods of one instance are
fate-sharing: they start together, they are admitted together, and they are replaced together. Use
it when one instance genuinely spans hosts — tensor, pipeline, expert or sequence parallelism.

**`size` cannot be changed after creation.** The Pods a running instance is made of are not the Pods
a different size asks for, so no edit exists that does not replace every instance of the role at
once. To serve at a different size, create a deployment that declares it. Scaling is what `replicas`
is for, and it disturbs nothing already running.

Above 1, the operator names each Pod of an instance `<deployment>-<role>-r<replica>-m<member>` and
publishes them behind a headless Service per instance, so every Pod can address the others by a name
that is derivable before any of them exists.

Member `m0` is the **leader**: it is the one the role's Service fronts, because it is the one
serving the OpenAI API. Each container is told the leader's address, the instance's size and its own
index, through three variables the operator owns on every engine:

| Variable | Value | Read from |
|---|---|---|
| `GPUSTACK_REPLICA_LEADER_ADDRESS` | `<deployment>-<role>-r<replica>-m0.<deployment>-<role>-r<replica>` | a literal |
| `GPUSTACK_REPLICA_SIZE` | the role's `size` | a literal |
| `GPUSTACK_MEMBER_INDEX` | `0` for the leader, then `1`, `2`, … | the downward API, off a label |

They appear **only above `size: 1`**. The three names are nonetheless reserved at **every** size: a
role that sets one of them in `env` is refused rather than silently overridden, at `size: 1` as well,
so that widening an instance later cannot turn a deployment that was accepted into one that is
refused. The index is read from a label so that every member of an instance carries the same
container spec and only the label value differs.

⛔ **What the engine does with those facts is yours.** The operator composes no
`--tensor-parallel-size` or equivalent: the degrees do not decompose from `size` alone, and a
formula missing an input is worse than no formula. Composing none is not seeing none — a degree
the author declares on the role is read and validated, and [the transfer leg renders from
it](model-deployment-prefill-decode.md#how-a-pair-is-wired).

A whole instance is the unit of replacement at every size. That is Kueue's constraint rather than a
preference: a deleted member of an admitted group is held on the API server until the group's
Workload goes, and deleting that Workload stops the instance's surviving members anyway.

`replicas`, `size` and `instanceType` are structured fields and stay so: admission and scheduling
read them, so an override able to shadow them would make the feasibility check read a ledger that
does not match reality.

The accelerator request of **one Pod** lives in `roles[].resources`, whose fields mirror
[Accelerator Requests](../accelerator-requests.md) — at `size: 1` the Pod and the instance are the
same request. CPU, memory and ephemeral storage are **derived** from the InstanceType's per-unit
resources scaled by the card count, so they are not expressible here.

An optional `resources.interface` requests a whole number of fabric interfaces per engine Pod.
One RDMA interface uses `device.gpustack.ai/rdma.shared`; more than one uses that count of
`device.gpustack.ai/rdma` devices. EFA uses
[the EFA device plugin's own key](../architecture/network-topology.md#the-rdma-resource-keys-and-what-each-endpoint-serves).
An unset or zero count adds no device request.

The key follows the bound cache backend's effective group protocol and any direct prefill/decode
transport the engine actually renders. Different backend-group protocols or an RDMA and EFA mix
are refused for a positive count.

A count with no managed RDMA or EFA transfer leg is also refused. The count is frozen with the other
role resources. See [RDMA Operations](../operation/rdma.md) for the allocation and topology limits.

## Prefill and decode

Several roles in one deployment are admitted **atomically**: a pool that cannot fit all of them leaves
all of them queued, instead of admitting the prefillers and stranding them waiting for decoders that
never arrive. Every replica of every role is its own pod group, so the set is held together by an
admission check this operator runs, which holds every group until the whole set has reserved quota.

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
      instanceType: gpustack-nvidia-h20-linux-amd64   # may differ; see the prefill and decode page
      resources:
        accelerator: 2
```

> **A role's parallelism degrees are not API fields.** They reach the engine through
> `roles[].extraArgs` — or through `roles[].command` alone when the role takes the line over —
> spelled the engine's own way. The operator composes no degree, but it reads those books:
> admission refuses a degree it cannot parse, and the transfer leg renders from them. They do not
> determine the AscendDirect transfer-port window.

`name` identifies the role and becomes the Kueue PodSet name; `kind` selects behaviour and is closed.
They are separate because a semantic reachable by typing a free-form string is a semantic one typo away
from silently changing.

Two roles may share a `kind` and differ in `name` only where that `kind` is `server`: a pair of
servers is a set of equals, whereas nothing consuming these roles expresses a second prefiller, so a
deployment declaring one would render a role nothing downstream can reach.

What pairs the two roles is on [Model Deployment Prefill and Decode
Reference](model-deployment-prefill-decode.md): the connector each engine and router renders, the
router block and its fields, the direct transfer and its transport, roles on different hardware, and
a role's own address.

### What every Pod of the group carries

| Key | Value | What it is for |
|---|---|---|
| label `kueue.x-k8s.io/pod-group-name` | `gpustack-fnv64-<hash>` over the namespace, deployment, role and ordinal — always the hashed form, on every shape | membership: it is what makes a replica its own group. The name is unique, not parseable: the ordinal travels in its own label and the hash is never read back, so a sole role's replica names its group exactly as one role of several does |
| label `modeldeployment.gpustack.ai/pod-ordinal` | the replica's slot within its role, from 0 up | the one per-replica identity the converger reads back: the group name is derived from it, the spec hash covers it, and a scale-down sheds the highest ordinals first |
| label `modeldeployment.gpustack.ai/member-index` | which Pod of its instance this is, from 0 up — **present only above `size: 1`** | what tells two Pods of one instance apart, and what a `Service` selector matches to front only the leader. A container reads its own index through this label rather than from a rendered value, which is what keeps one Pod template per instance. Absent means member 0, which is what it would have said |
| annotation `kueue.x-k8s.io/pod-group-total-count` | the role's `size` | how many Pods Kueue waits for before composing anything. The group is this replica and nobody else's, so a replica-count change moves no total any member carries — a resize is a trim, not a rebuild. It is safe for this to be `size` rather than a constant only because `size` is frozen at creation: a number that moved would be back under two writers |
| annotation `kueue.x-k8s.io/role-hash` | the role's `name` | names the single PodSet the replica's group composes, which is what lets status attribute a Workload back to the role that asked for it. It carries the role and NOT the ordinal, so every replica of a role names the same PodSet |
| annotation `kueue.x-k8s.io/pod-group-serving` | `"true"` | an inference deployment never finishes; without it Kueue reclaims the quota of a replica that exited |
| label `kueue.x-k8s.io/queue-name` | the `status.entrance` **published by** the role's InstanceType | unchanged; Kueue refuses a group whose Pods disagree on it. Read from the type rather than re-derived from its name, so this operator and the reconcile that creates the LocalQueue cannot disagree about the queue |
| label `app.kubernetes.io/component` | the role's `name` | unchanged; what a `Service` selects on and what `status.roles[]` is attributed by |
| label `modeldeployment.gpustack.ai/role-kind` | the role's **effective** `kind`, so `server` when the field is unset | what something in front of the replicas selects on to tell a prefiller from a decoder. It is the resolved value rather than the field, because a selector matching the empty string would miss every replica of the default shape. Rendered for every deployment, a lone `server` included, so "no prefiller is running" and "this deployment does not label its roles" are different answers |
| annotation `kueue.x-k8s.io/podset-required-topology` | `roles[].topology.requiredLevel`, when non-empty | asks Kueue to fit this replica's whole PodSet in one domain at the named hierarchy level |
| `spec.nodeSelector` | no topology value is added | Kueue selects the concrete domain through the flavor's Topology and writes its assignment; the deployment requests a level, not a region, zone, rack, or host value |

The `role-hash` annotation is load-bearing rather than cosmetic. Kueue takes it verbatim when present
and otherwise derives a digest of the Pod spec's *shape*, which names the PodSet after nothing an
operator wrote — status joins a Workload's PodSets to the roles by this name, so a digest breaks the
join while nothing errors.

> **`kueue.x-k8s.io/pod-group-fast-admission` must never be set.** That path composes the Workload
> from the first runnable Pod alone and gives that single PodSet the whole group's total; with a total
> of one it buys nothing, and it stays a trap for the day a group ever grows a second member. The
> operator never sets it, and a test asserts its absence.

## Topology placement

`roles[].topology.requiredLevel` is an optional Kubernetes label key. It requires the `size` Pods in
each replica group to fit within one domain at that level. See [per-replica request
semantics](../architecture/topology-aware-scheduling.md#a-modeldeployment-request-is-per-replica).

The value names a configured level, not a domain value. Region, zone, a GPUStack rack key, an
administrator-owned key, and selected Topograph labels all use the same field. The syntax is
validated at admission; availability in the chosen queue is resolved dynamically by Kueue.

Omit `topology` or leave `requiredLevel` empty for unconstrained topology-aware placement.
`kubernetes.io/hostname` is implicit and is rejected as an explicit required level. Changing or
removing the request changes the role render hash and rolls only that role's replicas.

The full discovery, profile, capacity, and diagnostic contract is in [Topology-Aware
Scheduling](../architecture/topology-aware-scheduling.md); setup examples are in [Topology-Aware
Scheduling Operations](../operation/topology-aware-scheduling.md).

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
| overlay | the role's own Pod fields — `image`, `imagePullPolicy`, `imagePullSecrets`, `privileged`, `ports`, `additionalVolumes`, `terminationGracePeriodSeconds` | the operator renders first, then merges this overlay on top |
| take over | `roles[].command` | the user owns the whole argv; the operator synthesizes **no** engine argument and **no** client environment |

Unlike the `Instance` that keeps its pod shape inside an `InstanceTemplate`, a role's Pod fields sit
on the role itself and are **mutable** — the Instance's immutability is a rule its webhook enforces,
not a property of the type, and dropping it here is what makes a rollout possible at all.

Arguments fold into `command`; there is deliberately no `args`. A second append tier beside
`extraArgs` would have no defined precedence, and would make the take-over tier ambiguous, since
`args` alone would be neither take-over nor append. A take-over `command` is then the whole
argv — the image's own entrypoint never participates — and an `extraArgs` written beside it is
inert: read by nobody, refused nothing, and no part of the books the transfer leg renders from.

**Engine authentication is the append tier's known bad input.** `--api-key` and `VLLM_API_KEY`
are not operator-owned, so admission accepts them — and vLLM then guards its `/v1` routes,
including the `/v1/models` a direct decode role's gates read. The probes get 401, and a
disaggregated pair's decode replica never becomes Ready.

North-south authentication belongs at the gateway in front of the deployment. The routing
sidecar authenticates no inference traffic, so there is nothing to configure on it.

Taking over the command line has a visible cost: the role reports
`status.roles[].unmanaged: true` and `CacheAttached` moves to `Unknown`. The operator configured no
cache client for that role, so it does not report on one it did not render.

### A take-over role is outside the reuse-domain guarantee

⚠️ **A role that owns its whole argv can name any reuse domain, and this operator does not stop it.**
`MOONCAKE_TENANT_ID` is refused in `roles[].env` on the engines that own it — the table under
[What the operator owns](#what-the-operator-owns) is the authority — but `roles[].command` is a
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
| `vllm` | `--kv-transfer-config`, `--kv-events-config` — each also in every spelling vLLM reads as it: a unique prefix (`--kv-transfer-conf`), underscores (`--kv_transfer_config`), or a dotted member (`--kv-transfer-config.kv_role`), which vLLM merges into a whole document that replaces the operator's | `MOONCAKE_CONFIG_PATH`, `VLLM_MOONCAKE_BOOTSTRAP_PORT` |
| `sglang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config`, `--disaggregation-mode`, `--disaggregation-transfer-backend`, `--disaggregation-bootstrap-port` — each also as a unique prefix (`--disaggregation-mo`) | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME`, **`MOONCAKE_TENANT_ID`** |

One `vllm` row covers **both backends**. The owned keys follow the engine while only the connector
name follows the accelerator backend, so an Ascend pool and an NVIDIA pool running `vllm` own exactly
the same keys and differ only in the connector the operator names.

**Owned** means the operator refuses a user-supplied duplicate, because two values for one connector
argument cannot be told apart. The refusal names the key, the engine, and `roles[].command` as the
way to own it instead.

**`--kv-cache-dtype` is owned on both engines while `spec.kvCache` is set**, in every spelling the
engine reads as it, because the operator renders the Binding's `dtype` there — why is under
[The dtype is handed to the engine](../kv-cache/pool.md#the-dtype-is-handed-to-the-engine). It is
not in the table because it is conditional:

- A deployment with no `spec.kvCache`, or a role with `roles[].command`, keeps the flag as its own.
- With the Setting `model-deployment-kv-cache-dtype-owned` off, nothing is rendered or refused.
- A deployment stored with the flag before the refusal keeps running on **its own value**, which
  comes later on the command line and wins. Its next update is refused until the flag is removed; the
  update that clears its finalizer on deletion is not.

**Defaulted** is the other case, and `MC_TE_METRIC` is the one that matters: the operator sets it to
`1`, and a user's own value wins with no refusal. It turns on the transfer engine's metrics, without
which the hit rate this design rests on cannot be measured at all. It is read by the transfer engine
rather than by an engine's config class, so it does not depend on which keys that class accepts.

So are `MC_FORCE_TCP` [on a `tcp` leg](model-deployment-prefill-decode.md#the-direct-transfers-transport) and
[SGLang's two cache switches](kv-cache-injection.md#sglangs-host-memory-tier).

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
container. A decode-only role does not need to publish. A role with `roles[].command` receives none
of this configuration because the operator does not own its command line.

Nothing is created beside the Pod, no RBAC for one is needed, and the configuration's lifetime is
exactly the replica's. It is also part of the Pod's spec hash, which is what moves the replicas when
the pool's published endpoint changes.

It sits under `/etc` rather than in the image's workspace so that a role's own volumes are
unlikely to collide — but an overlay that mounts over that path replaces the configuration silently,
and the owned `MOONCAKE_CONFIG_PATH` cannot protect against it. SGLang gets no file at all; its
configuration travels entirely in the environment.

## The runner image is a formula

A role with no `roles[].image` gets one assembled from the engine the deployment declares and the
hardware its InstanceType observed. A stated image always wins.

```text
gpustack/runner:<backend><runtimeVersion>[-<variant>]-<engine><version>
```

`gpustack/runner:cuda12.9-vllm0.29.0` on an NVIDIA pool; `gpustack/runner:cann9.0-910b-sglang0.5.18`
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

`engine.version` is optional in the schema, and the obligation sits with the roles: a role that
names no image of its own has one synthesized from this version, so admission refuses an empty
version beside such a role. It is otherwise **free-form**: the operator checks neither that the
combination was ever published nor that the version supports the installed driver. You guarantee
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

## Rollout is a rolling replacement

Changing `replicas` adds or removes instances and nothing more: the survivors are not restarted, do
not reload their weights and keep their cached blocks. What still replaces **every** instance of the
role is an edit that changes what a replica's Pod renders — `image`, `extraArgs`, `env`, `ports`,
`additionalVolumes`, `terminationGracePeriodSeconds`. A change to `size` is not on that list because
it cannot be made: see `roles[].size` above.

Such an edit **deletes and recreates** the role's replicas — one replica per role per pass, waited
out. The role set itself cannot be edited at all; admission refuses it, so there is no role rename or
addition to roll. There are no surge or unavailable knobs.

The one-at-a-time cadence is a constraint rather than a choice: Kueue counts a group against its
declared total, so a replacement created beside a member Kueue still counts active reads as one over,
and Kueue's answer to the excess is to delete the newest un-finalized Pod — the replacement itself.

A replacement is a **fresh admission**, not a rider on the reservation the departed replica held:
freeing the slot deletes that replica's Workload, and the reservation goes with it. The cadence guard
therefore turns a replica over only when every replica the role declares holds an admitted Workload —
on a full pool a rollout waits for capacity rather than shedding replicas it cannot re-reserve.

The cost is real and worth stating, and it rides on the block lease described under
[What a cache changes about a workload](kv-cache-injection.md#what-a-cache-changes-about-a-workload): a lease survives a long queue and does **not**
survive an interrupted heartbeat, which is what a departing replica is.

So a departing replica costs its siblings the blocks it held. The deployment records an event naming
the replica and the lease window on each of three paths — `ReplicaEvicted`, `ReplicaLeaving`,
`ReplicaRestarted` — so an operator correlating a burst of failed requests with a replica that went
away has the correlation written down rather than inferred.

**An upgrade can trigger the same turnover without any spec edit.** The fingerprint covers a replica's
labels, annotations and spec, so a release that changes what every replica renders turns each one over
once: the `role-kind` label above did, and so did the [drain](model-deployment-shutdown.md). Nothing
is required of you, but on a busy deployment the restart is worth scheduling.

### A replica that leaves is replaced

Most departures are not a spec change, and none of them touches a sibling:

| Cause | Who initiates it |
|---|---|
| **Kueue preempting** the deployment for a higher-priority workload | the scheduler |
| **a node being drained**, cordoned or replaced | the cluster |
| **the kubelet evicting** a replica under node pressure | the node |
| `kubectl delete pod` on one replica | you |

Who frees the slot depends on the cause. Kueue's own preemption evicts the departed replica's
Workload, and that eviction removes the Pod and releases the slot; the replacement then follows on
its own.

A delete that lands on the Pod alone — a drain, a kubelet eviction, `kubectl delete pod` — leaves
that replica's Workload standing, and the Workload is what holds the slot: the Pod stays readable on
the API server and no replacement is created beside it. Deleting the departed replica's Workload
releases the Pod and the quota, and the replacement follows on the terms the callout above states.

The gate reads the ordinal and nothing else, which is why a scale-up is immediate: an ordinal nothing
ever occupied has no Pod to wait out, so its replica is created on the first pass that sees it.

The replacement carries a fresh name the API server assigns, never the departed Pod's name. What that
means for anything that addresses replicas, and the selector to use instead, is below.

On a cluster with preemption enabled, a replica going away is therefore **routine rather than an
incident** — worth knowing before you chase one as a fault.

**A replica on a node that is NotReady but still registered is never replaced.** Its Pod keeps its
`nodeName` and a Running phase, so its ordinal still reads occupied and no replacement is created.
Force-deleting that Pod is how two processes end up holding one accelerator; deleting the Node object
resolves it, which is what a cluster that replaces nodes already does.

**A replica's name is assigned by the API server, so nothing can predict it.** A replacement is a new
Pod under a new name rather than the departed one's name reused. Address replicas by label instead of
by name:

```bash
kubectl get pods -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=<deployment>
```

Add `,app.kubernetes.io/component=<role>` for one role's replicas, or
`,modeldeployment.gpustack.ai/role-kind=prefill` for every prefiller regardless of what its role is
called. A runbook that spells `<deployment>-<role>-0` breaks here and has no fixed name to move to.

Changing `replicas` is not one of these departures either: it adds or sheds instances and leaves the
survivors running. See [Rollout is a rolling replacement](#rollout-is-a-rolling-replacement) for
which edits replace every instance instead. The role set cannot be edited at all — admission refuses
it.

### Which fields are the deployment's identity

Some fields cannot be edited at all, and the rule that sorts them is a question rather than a list:
**a field is frozen when it answers *which deployment is this*, and editable when it answers *how is
this deployment being run right now*.**

| Frozen | Editable |
|---|---|
| `model`, `engine.name`, `kvCache` | `engine.version`, `kvTransfer` |
| `router.name` in place — the router block itself may be added or removed | `router.replicas`, `router.extraArgs` |
| the set of roles, and each role's `name` and `kind` | `roles[].replicas` |
| `roles[].size` | |
| `roles[].instanceType` | `roles[].extraArgs`, `roles[].env` |
| `roles[].resources` | the role's own Pod fields — `image`, `imagePullPolicy`, `imagePullSecrets`, `privileged`, `ports`, `additionalVolumes`, `terminationGracePeriodSeconds` |
| `roles[].command` | labels and annotations |

`roles[].resources` is frozen against the criterion rather than by it, and that is marked here so it
does not read as an oversight: it does not say which deployment this is, but changing it renegotiates
the scheduling, which is not materially different from deleting and recreating. Its mirror image is
`roles[].privileged`, which the criterion leaves editable even though a different argument could
move it.

**What to do instead of editing one is create another deployment.** A frozen field is not a lock
protecting a concurrent writer, and the refusal says so: what you are describing is a different
deployment, so it is created rather than edited. The name, the `status` history and the cache-pool
registration are what you keep by editing, and none of them is what a frozen field carries.

Judging a **new** field means asking that question, not appending to the table — a list alone grows
by precedent and stops meaning anything.

> **A merge patch that omits a frozen field is an edit to that frozen field.** `roles` is a list, and
> `kubectl patch --type=merge` replaces a list wholesale rather than merging into it — so a role
> restated without its `command` sets `command` to null, and the edit is refused naming
> that field rather than the one you meant to change.
>
> Change one field with a JSON patch (`--type=json`, `/spec/roles/0/replicas`), or send the whole
> object with `kubectl apply` or `kubectl edit`. This is not a quirk of the freeze: omitting a value
> in a merge patch IS setting it to null, and the rule is reading what you actually sent.

### One group per replica

The grouping key is the **replica**: every replica of every role forms its own pod group, derived from
the role and the replica's ordinal within it, and Kueue composes one Workload per group with a
declared total equal to the role's `size`. Two roles naming the same `instanceType` share nothing —
a queue name is derived from the `instanceType` and one Workload carries one queue name, so rather
than forbid the shape the roles are simply not made to share.

**What an edit costs is therefore the replica, not the group it used to share.** Growing or shrinking
`replicas` adds or removes whole groups and leaves every surviving replica's group, Workload and
admission untouched; a role-field edit rolls that role's replicas one at a time.

The groups are still admitted as a **set**, by the admission check described in
[Prefill and decode](#prefill-and-decode), whichever `instanceType`s they name.

A scale-down sheds the **highest ordinals first**, deleting each departing replica's Pod together with
its own Workload: the Workload delete is what releases Kueue's finalizer on the Pod and the quota the
replica held, because a serving group is never finished and nothing else releases either.

## What admission refuses

Two webhooks make up the admission surface. Nearly every default lives in the CRD schema; the
mutating half exists for the one value a schema cannot reach — a role's accelerator count, which
depends on the `InstanceType` the role names.

| Refused | Message names |
|---|---|
| more than 10 roles | the bound as **this operator's own shape limit**, not an upstream number — every role renders its own replicas, Services and queue references |
| a role whose members could not be named | the longest name the declared `replicas` and `size` would produce, its length and why it is not a hostname, and the three ways out — shorten the role, shorten the deployment, or declare fewer replicas |
| two roles sharing a `name` | the duplicate — refused by the **schema**, since `roles` is a list keyed on `name`, so this one never reaches the webhook |
| an edit to an identity field — `model`, `engine.name`, `kvCache`, or the shape of the roles | the field path, and that a different value describes a different **deployment**, which is created rather than edited. See [Which fields are the deployment's identity](#which-fields-are-the-deployments-identity) |
| a resource mode the named `InstanceType` does not offer | the mode and the type — a slice on a type that offers no slicing, a partition profile on a type that cannot partition, or one outside its profile inventory, with the offered list |
| a whole-accelerator count over the type's whole-accelerator capacity | the capacity itself, not only that the request was too large, so the next attempt is not a guess. The bound is the pool's total, not what is free, so a deployment submitted while every accelerator is held is admitted and waits in its queue; one above the largest node but within the total is admitted and stays queued — see [Accelerator Requests](../accelerator-requests.md#limitations) |
| a negative or fractional `resources.interface`, or one with no effective RDMA/EFA leg | the role's interface field and the protocol that prevents allocation; mixed backend groups and mixed fabric legs are rejected |
| an explicit `accelerator: 0` on an acceleratable `InstanceType` shared by another role | the accelerator field, the shared type, and two recommended remedies: request at least one accelerator or move the CPU-only role to a non-acceleratable type |
| a `prefill` and a `decode` role both requesting a **logical slice** from types that draw on the same accelerator group | both roles and the slice field. Whole cards and partition profiles are accepted — including on one card, because partitions are isolated by the device |
| a role whose `<deployment>-<role>` is not a DNS-1035 label | the combined **Service** name, which is what the pair becomes; over 63 characters or carrying a dot from a subdomain-shaped deployment name. A role the object **already had** is exempt, so a rule added later cannot strand a stored object |
| two roles whose Services would be named the same | the shared name and both claimants — a role named `x-r0` collides with a role `x` of several members, whose instance 0 is published behind `<deployment>-x-r0`. Checked on every edit, since `replicas` decides how many instance Services a role derives |
| an invalid topology `requiredLevel` | the field path and the [topology placement](#topology-placement) field rule |
| a `replicas` over 1024, or a `size` over 64 | the bound — refused by the **schema**. It limits how many Pods one pass renders before it writes any of them, so it is this operator's own ceiling rather than a Kubernetes one |
| `kind: server` beside any other kind | that a server serves whole requests by itself, so the combination describes no arrangement |
| a `kind` the engine has no term for | the engine and the kind — today, `prefill` or `decode` on SGLang |
| an owned key in `extraArgs` | the key, the engine, and `roles[].command` as the way to own it |
| an owned name in `env` | the same three |
| `--kv-cache-dtype` in `extraArgs` while `spec.kvCache` is set | that it carries the Binding's `dtype`, and a Binding declaring another dtype or `roles[].command` as the ways out — see [What the operator owns](#what-the-operator-owns) |
| a `--port` in `extraArgs` or `command` naming another port than the role's first `ports` entry | both values and the field each came from. A managed direct decoder is exempt, since its proxy owns the declared port. A stored role is judged only when an edit changes its `ports` or arguments |
| a port the operator reserves for a listener it synthesizes onto the role — vLLM's KV event ports under `llm-d-router`, or the bootstrap port of a prefiller in a declared pair that names a router or a cache — declared in `ports`, or passed as `--port` in `extraArgs` by a role declaring no `ports` | the port, the field it came from and the reserved set, on `spec.router` when a router is named. A replaced `command` is exempt, since nothing is synthesized onto it. A stored `--port` collision is left alone until an edit changes it; adding a router that creates one is refused |
| a parallel degree the role's books cannot be read for — a known flag's value missing, non-integer, out of range or below its bound, or a malformed `VLLM_DP_SIZE` | the role, the flag, and `roles[].command` as the way to own the whole line |
| a declared parallel width over the role's card request | the card count, the degrees behind the width, and that the width is per member — `size` does not rescue it |
| a `template` field on a role | the unknown field itself — the block is gone, so strict decoding refuses it rather than a webhook rule |
| a partition profile together with a slice percentage | both slice fields; one accelerator cannot serve both |
| a `poolRef` outside this namespace | nothing — it is unrepresentable in the type |
| a self-declared reuse domain | nothing — the field does not exist |
| an EMPTY `poolRef.name` | the Binding as the authorization point, and that an empty reference names none |
| a new binding to a pool with mixed member protocols for an unconstrained engine | each effective protocol and why one installed transport cannot read blocks on the others; use one protocol across the groups or another pool |

**Most rules above are answered from the submitted object.** Resource mode, card count and a shared
type's acceleratability depend on `InstanceType`; new cache binding compatibility depends on its
Binding, pool and backend. The webhook reads these objects from the API server so a stale cache
cannot decide admission. An unchanged binding stays editable if its backend later becomes mixed.

An older binding still renders its first offered transport. It can remain healthy while reads of
blocks on another transport fail, so repair the pool even though an unrelated update is accepted.

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

**A replica serves on port 8000** unless the role names its own container port. The
Service and `status.endpoint` keep that external port. On a managed native-vLLM decoder the routing
proxy owns it and vLLM listens behind the proxy on an internal port; every other role tells the engine
itself to open the external port. The startup, readiness and liveness probes follow the external
listener, so a decoder becomes Ready only when the proxy can reach the engine.

**A role that passes its own `--port` and declares no `ports` moves only the container side.** The
container port, the Service's `targetPort`, the probes and a router's target port follow the port the
engine reads, in any spelling it accepts, from `extraArgs` or a replaced `command`. The Service's own
port and `status.endpoint` stay on 8000, so callers keep their address.

### Transfer ports are runtime-selected

`roles[].ports` exposes container ports for the engine and Service. It neither reserves nor
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
the domain · [Model Deployment Prefill and Decode Reference](model-deployment-prefill-decode.md) for
what pairs a prefill role with a decode role · [Accelerator Requests](../accelerator-requests.md) for the request fields
`roles[].resources` mirrors · [Admission](../architecture/admission.md) for the gates a replica passes
as an ordinary Pod · [Model Deployment Status](model-deployment-status.md) for what each condition
and published field means · [Model Deployment Metrics](model-deployment-metrics.md) for the
structured snapshot and Pod scrape endpoints.

**Next** → [Accelerator Requests](../accelerator-requests.md)
