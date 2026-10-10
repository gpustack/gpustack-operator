# Model Deployment Configuration

A `ModelDeployment` defines one or more inference-engine roles, each with its own replicas. The
roles can share cached prefixes through a KV cache pool, form a direct prefill/decode pair, or do
both.

The operator creates Pods for those replicas. Its `ModelDeployment` webhook checks the transport
of a new KV cache binding; the generated Pods bypass KV cache Pod injection. Ordinary Pod
admission still applies to every replica.

## Contents

- [A minimal deployment](#a-minimal-deployment)
- [Prefill and decode](#prefill-and-decode)
- [Topology placement](#topology-placement)
- [Inherited reuse domain](#inherited-reuse-domain)
- [The three override tiers](#the-three-override-tiers)
- [Operator-owned keys](#operator-owned-keys)
- [Runner image formula](#runner-image-formula)
- [Rollout behavior](#rollout-behavior)
- [Admission refusals](#admission-refusals)
- [Operating notes](#operating-notes)

## A minimal deployment

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment                    # namespaced, short name md
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct      # served, never provisioned
  engine:                                # vLLM | SGLang
    name: vLLM
    version: "0.29.0"                    # free-form; you guarantee alignment
  kvCache:                               # OPTIONAL; omit it and no shared pool is attached
    poolRef:
      name: team-a-dram                  # a KVCachePoolBinding IN THIS NAMESPACE
    connector: Mooncake                  # the only value; defaulted
  roles:
    - name: server
      replicas: 4
      instanceType: gpustack-nvidia-a10g-linux-amd64
      resources:
        accelerator: 2                   # cards per Pod
```

`model.name` is what the engine serves. The weights come from the engine's own hub client, a role's
volumes, or a `ModelArtifact` named by `model.artifactRef`; see the
[Model Artifact](/gpustack-operator/v0.9.0/docs/modules/model-delivery/artifact/index.md).

`poolRef` names a `KVCachePoolBinding` in the deployment's namespace. An administrator must create
that binding to grant the namespace access. A cluster-scoped pool name, another namespace or a bare
endpoint URL cannot be used in this field.

The only accepted `connector` value is `Mooncake`; it is defaulted and immutable. Current rendering
derives the connector configuration from the engine, role kind and pool backend. The field reserves
the API for future connector implementations; NIXL and ROCm NIXL have not been verified here.

A deployment supports 1 to 10 roles. Set `replicas` for the number of serving instances in each
role, and `size` for the number of Pods in each instance.

`replicas` counts **independent serving instances**: each one starts, serves and is replaced on its
own. Changing the number adds or removes instances, and the ones that survive are not restarted:
they keep serving without interruption and keep whatever cache they hold.

`size` is how many Pods form one instance, defaulting to 1. The Pods of one instance are
fate-sharing: they start together, they are admitted together, and they are replaced together. Use
it when one instance genuinely spans hosts (tensor, pipeline, expert or sequence parallelism).

**`size` is editable, and the edit replaces the role's instances.** An instance is never reshaped in
place: each old instance and its Workload are deleted first, and the replacement created in that
slot is admitted fresh before the next old instance may be deleted. See
[Rollout behavior](#rollout-behavior) for the cadence and its costs. Scaling without replacement is
what `replicas` is for, and it disturbs nothing already running.

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

**What the engine does with those facts is yours.** The operator composes no
`--tensor-parallel-size` or equivalent, because the degrees do not decompose from `size` alone and
a formula missing an input would pass silently. A degree you declare on the role is another matter:
it is read and validated, and [the transfer leg renders from
it](prefill-decode.md#how-a-pair-is-wired).

A whole instance is the unit of replacement at every size. That is Kueue's constraint rather than a
preference: a deleted member of an admitted group is held on the API server until the group's
Workload goes, and deleting that Workload stops the instance's surviving members anyway.

`replicas`, `size` and `instanceType` are structured fields and stay so: admission and scheduling
read them, so an override able to shadow them would make the feasibility check read a ledger that
does not match reality.

The accelerator request of **one Pod** lives in `roles[].resources`, whose fields mirror
[Accelerator Requests](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md); at `size: 1` the Pod and the instance are the
same request. CPU, memory and ephemeral storage are **derived** from the InstanceType's per-unit
resources scaled by the card count, so they are not expressible here.

An optional `resources.interface` requests a whole number of fabric interfaces per engine Pod.
One RDMA interface uses `device.gpustack.ai/rdma.shared`; more than one uses that count of
`device.gpustack.ai/rdma` devices. EFA uses
[the EFA device plugin's own key](/gpustack-operator/v0.9.0/docs/modules/rdma/network-topology/index.md#the-rdma-resource-keys).
An unset or zero count adds no device request.

The key follows the bound cache backend's effective group protocol and any direct prefill/decode
transport the engine actually renders. Different backend-group protocols or an RDMA and EFA mix
are refused for a positive count.

A count with no managed RDMA or EFA transfer leg is also refused, whenever the request, the
transfer settings or the pool the roles name change. See [RDMA Operations](/gpustack-operator/v0.9.0/docs/modules/rdma/operations/index.md)
for the allocation and topology limits.

## Prefill and decode

Several roles in one deployment are admitted **atomically**: a pool that cannot fit all of them leaves
all of them queued, instead of admitting the prefillers and stranding them waiting for decoders that
never arrive. Every replica of every role is its own pod group, so the set is held together by an
admission check this operator runs, which holds every group until the whole set has reserved quota.

```yaml
  roles:
    - name: prefill
      kind: Prefill                        # Server (default) | Prefill | Decode
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64
      resources:
        accelerator: 2
    - name: decode
      kind: Decode
      replicas: 2
      instanceType: gpustack-nvidia-h20-linux-amd64   # may differ; see the prefill and decode page
      resources:
        accelerator: 2
```

> **A role's parallelism degrees are not API fields.** They reach the engine through
> `roles[].extraArgs` (or through `roles[].command` alone when the role takes the line over),
> spelled the engine's own way. The operator composes no degree, but it reads the command line:
> admission refuses a degree it cannot parse, and the transfer leg renders from them. They do not
> determine the AscendDirect transfer-port window.

`name` identifies the role and becomes the Kueue PodSet name; `kind` selects behaviour and is closed.
They are separate because a semantic reachable by typing a free-form string is a semantic one typo away
from silently changing.

Two roles may share a `kind` and differ in `name` only where that `kind` is `Server`: a pair of
servers is a set of equals, whereas nothing consuming these roles expresses a second prefiller, so a
deployment declaring one would render a role nothing downstream can reach.

What pairs the two roles is on [Model Deployment Prefill and Decode](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md): the connector each engine and router renders, the
router block and its fields, the direct transfer and its transport, roles on different hardware, and
a role's own address.

### Pod group labels and annotations

| Key | Value | Purpose |
|---|---|---|
| label `kueue.x-k8s.io/pod-group-name` | `gpustack-fnv64-<hash>` over the namespace, deployment, role and ordinal — always the hashed form, on every shape | membership: it is what makes a replica its own group. The name is unique, not parseable; the ordinal travels in its own label |
| label `modeldeployment.gpustack.ai/pod-ordinal` | the replica's slot within its role, from 0 up | the per-replica identity: the group name derives from it, and a scale-down sheds the highest ordinals first |
| label `modeldeployment.gpustack.ai/member-index` | which Pod of its instance this is, from 0 up — rendered at **every** size, one Pod included | what tells two Pods of one instance apart, and what a `Service` selector matches to front only the leader above `size: 1`. A container reads its own index through this label rather than from a rendered value, which is what keeps one Pod template per instance |
| annotation `kueue.x-k8s.io/pod-group-total-count` | the role's `size` | how many Pods Kueue waits for before composing anything. A replica-count change moves no total any member carries — a resize is a trim, not a rebuild. A `size` edit never edits a running group's total either: it replaces whole groups, and each new group carries its own |
| annotation `kueue.x-k8s.io/role-hash` | the role's `name` | names the single PodSet the replica's group composes, which is what lets status attribute a Workload back to the role that asked for it. Every replica of a role names the same PodSet |
| annotation `kueue.x-k8s.io/pod-group-serving` | `"true"` | an inference deployment never finishes; without it Kueue reclaims the quota of a replica that exited |
| label `kueue.x-k8s.io/queue-name` | the `status.entrance` **published by** the role's InstanceType | unchanged; Kueue refuses a group whose Pods disagree on it. Read from the type so this operator and the LocalQueue it creates cannot disagree about the queue |
| label `app.kubernetes.io/component` | the role's `name` | unchanged; what a `Service` selects on and what `status.roles[]` is attributed by |
| label `modeldeployment.gpustack.ai/role-kind` | the role's effective kind mapped to the router's spelling, so `server` when the API field is unset | what something in front of the replicas selects on to tell a prefiller from a decoder. Rendered for every deployment, a lone `server` included, so "no prefiller is running" and "this deployment does not label its roles" are different answers |
| annotation `kueue.x-k8s.io/podset-required-topology` | `roles[].topology.requiredLevel`, when non-empty | asks Kueue to fit this replica's whole PodSet in one domain at the named hierarchy level |
| `spec.nodeSelector` | no topology value is added | Kueue selects the concrete domain through the flavor's Topology and writes its assignment; the deployment requests a level, not a region, zone, rack, or host value |

The `role-hash` annotation is load-bearing: status joins a Workload's PodSets to the roles by this
name. Without it Kueue names the PodSet after a digest of the Pod spec's *shape*, and that join
breaks while nothing errors.

> **`kueue.x-k8s.io/pod-group-fast-admission` must never be set.** It composes the group's Workload
> from the first runnable Pod alone; with a declared total of one it buys nothing, and it
> mis-composes the group the day an instance grows a second member. The operator never sets it.

## Topology placement

`roles[].topology.requiredLevel` is an optional Kubernetes label key. It requires the `size` Pods in
each replica group to fit within one domain at that level. See [per-replica request
semantics](../topology/scheduling.md#per-replica-topology-requests).

The value names a configured level, not a domain value. Region, zone, a GPUStack rack key, an
administrator-owned key, and selected Topograph labels all use the same field. The syntax is
validated at admission; availability in the chosen queue is resolved dynamically by Kueue.

Omit `topology` or leave `requiredLevel` empty for unconstrained topology-aware placement.
`kubernetes.io/hostname` is implicit and is rejected as an explicit required level. Changing or
removing the request changes the role render hash and rolls only that role's replicas.

The full discovery, profile, capacity, and diagnostic contract is in [Topology-Aware
Scheduling](../topology/scheduling.md); setup examples are in [Topology-Aware
Scheduling Operations](../topology/operations.md).

## Inherited reuse domain

The reuse domain (`name`, `blockSize`, `dtype`) is a required, immutable block on the
`KVCachePoolBinding`; its `name` alone may be left out, and is then `default`. `ModelDeploymentSpec` has **no domain field**, and that is a security property
rather than tidiness.

> **Why** — a workload free to name its own domain could mint tenants and escape its namespace's
> quota ceiling. The mechanism is stated once, under
> [One Binding, one reuse domain](/gpustack-operator/v0.9.0/docs/modules/kv-cache/pool/index.md#one-binding-one-reuse-domain).

The requested semantics:

- Two deployments referencing the **same** Binding share KV.
- Two referencing **different** Bindings use different tenant identifiers. They are isolated only
  when their engine images read and forward those identifiers.
- Name matching between workloads disappears, and with it a whole class of typo.
- A namespace needing two reuse boundaries creates **two Bindings** on the same pool, the same shape
  as a namespace having several Kueue `LocalQueue`s.

`status.kvCache` echoes the Binding's `binding`, `pool` and the whole domain block, so an operator
reads the attached domain off this object alone. A wrong `blockSize` or `dtype` is silent cache
pollution: writes succeed, reads succeed, and the tensors are wrong.

For an operator-managed role, the operator renders a non-empty Binding domain as the engine's tenant
identifier. It does not inspect the engine image version or decide whether that build supports
tenant isolation. The tenant variable is operator-owned, so supplying it in `env` or `extraArgs` is
refused: it is a second path to a value [the API already refuses](#inherited-reuse-domain).

A rendered tenant identifier does not by itself prove isolation. The operator records what it
rendered, never what the container did with it: whether the build inside the image reads the value
is decided by the image, not by the render.

Users who require tenant isolation must select a compatible engine image and verify it themselves; see
[Tenant compatibility](/gpustack-operator/v0.9.0/docs/modules/kv-cache/injection/index.md#tenant-compatibility) for what
the image must consume. The API states the requested boundary, while the engine enforces it
(the same caveat [KV Cache Pool](/gpustack-operator/v0.9.0/docs/modules/kv-cache/pool/index.md#limitations) states for
capacity).

## The three override tiers

The engine command line is the fastest-moving thing in this design, so it has three escape tiers.
Without one, users patch the rendered Pod and the reconcile loop silently overwrites them.

| Tier | Field | Semantics |
|---|---|---|
| append | `roles[].extraArgs`, `roles[].env` | appended **after** the operator-synthesized arguments; a key the operator owns is refused, never merged |
| overlay | the role's own Pod fields — `image`, `imagePullPolicy`, `imagePullSecrets`, `privileged`, `ports`, `additionalVolumes`, `shmSize`, `terminationGracePeriodSeconds` | the operator renders first, then merges this overlay on top |
| take over | `roles[].command` | the user owns the whole argv; the operator synthesizes **no** engine argument and **no** client environment |

A role's Pod fields sit on the role itself, unlike the `Instance` that keeps its pod shape inside an
`InstanceTemplate`, and they are **mutable** — which is what makes a rollout possible at all.

Arguments fold into `command`; there is no `args` field.

A take-over `command` is the whole argv (the image's own entrypoint never participates), and an
`extraArgs` written beside it is inert: read by nobody, refused nothing, and invisible to the check
that reads parallelism from the command line.

**Engine authentication is the append tier's known bad input.** `--api-key` and `VLLM_API_KEY`
are not operator-owned, so admission accepts them, and vLLM then guards its `/v1` routes,
including the `/v1/models` a direct decode role's gates read. The probes get 401, and a
disaggregated pair's decode replica never becomes Ready.

North-south authentication belongs at the gateway in front of the deployment. The routing
sidecar authenticates no inference traffic, so there is nothing to configure on it.

Taking over the command line has a visible cost: the role reports
`status.roles[].unmanaged: true` and `CacheAttached` moves to `Unknown`. The operator configured no
cache client for that role, so it does not report on one it did not render.

### Take-over roles and the reuse domain

`MOONCAKE_TENANT_ID` is refused in `roles[].env` on the engines that own it (the table under
[Operator-owned keys](#operator-owned-keys) is the authority), but `roles[].command` is a program
and its arguments: the same value can travel inside a shell assignment or inside the script the
argv names, and admission has nothing to read either way.

Where the operator builds the argv, that refusal is real enforcement: the user cannot interpose
a shell, so the environment is the only path left. Why the key is owned at all is stated under
[Operator-owned keys](#operator-owned-keys).

That is one instance of a wider exposure, not its boundary. The boundary is stated once, under
[Limitations](/gpustack-operator/v0.9.0/docs/modules/kv-cache/pool/index.md#limitations), and tracked at
[#168](https://github.com/gpustack/gpustack-operator/issues/168), whose own void conditions include a
webhook-level one, so nothing here should be read as a claim about how that issue can be closed.

### Shared memory

`spec.roles[].shmSize` sets the capacity limit of `/dev/shm` in each role Pod. It accepts a positive Kubernetes
quantity, such as `32Gi`. Omission renders a `16Gi` memory-backed `EmptyDir`.
This applies to both engines, take-over roles, and the Elastic Ray head and GPU members.
Each Pod has its own volume. Only the main container mounts it.

Set it in the role configuration:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-chat
spec:
  roles:
    - name: server
      shmSize: 32Gi
```

An explicit `/dev/shm` mount in `additionalVolumes` takes precedence, including when `shmSize` is set.
The operator preserves that mount's capacity and backing.
The default is a project capacity choice, not a universal engine requirement.

The capacity limit does not reserve RAM or increase the Pod's memory request.
Used shared memory counts toward the main container's memory limit.
Budget memory for both shared memory and the engine heap.
Ray's object store may need more than `16Gi`; size the volume for the configured workload.
Kubelet lowers the mount size when the Pod memory limit or node allocatable memory is smaller.

Changing the rendered capacity follows ordinary role rollout behavior.
Existing Elastic members keep their mounts; new or replacement Pods use the current value.
Upgrading the operator adds the default mount to ordinary roles and triggers their recreate rollout.

## Operator-owned keys

Ownership is per (engine, key): a key one engine owns is an ordinary user argument on another.
`SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` is meaningless to `vllm` and is a plain user variable there.

| Engine | Owned arguments | Owned environment |
|---|---|---|
| `vLLM` | `--kv-transfer-config`, `--kv-events-config` — each also in every spelling vLLM reads as it: a unique prefix (`--kv-transfer-conf`), underscores (`--kv_transfer_config`), or a dotted member (`--kv-transfer-config.kv_role`), which vLLM merges into a whole document that replaces the operator's | `MOONCAKE_CONFIG_PATH`, `VLLM_MOONCAKE_BOOTSTRAP_PORT` |
| `SGLang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config`, `--disaggregation-mode`, `--disaggregation-transfer-backend`, `--disaggregation-bootstrap-port` — each also as a unique prefix (`--disaggregation-mo`) | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME`, **`MOONCAKE_TENANT_ID`** |

One `vLLM` row covers **both backends**. The owned keys follow the engine while only the connector
name follows the accelerator backend, so an Ascend pool and an NVIDIA pool running `vLLM` own exactly
the same keys and differ only in the connector the operator names.

**Owned** means the operator refuses a user-supplied duplicate, because two values for one connector
argument cannot be told apart. The refusal names the key, the engine, and `roles[].command` as the
way to own it instead.

**`--kv-cache-dtype` is owned on both engines while `spec.kvCache` is set**, in every spelling the
engine reads as it, because the operator renders the Binding's `dtype` there; why is under
[Engine dtype](/gpustack-operator/v0.9.0/docs/modules/kv-cache/pool/index.md#engine-dtype). It is
not in the table because it is conditional:

- A deployment with no `spec.kvCache`, or a role with `roles[].command`, keeps the flag as its own.
- With the Setting `model-deployment-kv-cache-dtype-owned` off, nothing is rendered or refused.
- A deployment stored with the flag before the refusal keeps running on **its own value**, which
  comes later on the command line and wins. Its next update is refused until the flag is removed; the
  update that clears its finalizer on deletion is not.

**Defaulted** is the other case, and `MC_TE_METRIC` is the one that matters: the operator sets it to
`1`, and a user's own value wins with no refusal. It turns on the transfer engine's metrics, without
which the hit rate this design rests on cannot be measured at all.

So are `MC_FORCE_TCP` [on a `tcp` leg](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md#direct-transfer-transport) and
[SGLang's two cache switches](/gpustack-operator/v0.9.0/docs/modules/kv-cache/injection/index.md#sglang-host-memory-tier).

Two of SGLang's owned keys are owned for what a user entry would **destroy** rather than duplicate,
and the operator does not set either of them:

- SGLang picks its configuration source in the order extra-config argument, then config-path file,
  then environment. The operator leaves both of the first two unset, and that is what **selects** the
  environment loader.
- Each of the first two loaders falls back to built-in defaults per key. So setting either
  one does not override a value: it silently replaces the whole configuration with defaults (a 4 GiB
  segment and a `localhost` identity).

SGLang needs `local_hostname`, which is the replica's own Pod IP. A file and an argument are both
fixed when the object is admitted, when no Pod IP exists yet, so only an environment variable with a
`fieldRef` on `status.podIP` can carry it, which is why this engine gets no config file at all.

`MOONCAKE_CONFIG_PATH` is owned on `vllm` and not on `sglang`, while seven other
`MOONCAKE_*` names are owned on `sglang` alone. The table is the authority; a name prefix is not.

`MOONCAKE_TENANT_ID` is the one whose ownership is a security property: it carries the reuse domain,
and a workload able to set it could write into another Binding's domain. It is a second path to a
value [the API already refuses](#inherited-reuse-domain).

On the vLLM family the operator mounts the rendered client JSON at
`/etc/gpustack/kvcache/mooncake.json`, read-only. **There is no ConfigMap**: the file is a downwardAPI
projection of the Pod's own `kvcache.gpustack.ai/client-config` annotation.

When `spec.router` is present, a role that produces cache blocks also receives a
`--kv-events-config` document. It enables the ZMQ publisher on `tcp://*:5557`, enables replay on
`tcp://*:5558`, retains 10,000 batches, uses a high-water mark and queue depth of 100,000, and
publishes topic `kv@`.

**That applies to the vLLM engine only.** A routed SGLang deployment renders no publisher arguments
and reports `KVEventsPublishing=False/PublisherDisabled`. The condition reports configuration rather
than live traffic, and SGLang's rendering carries no publisher configuration to report.

The wildcard addresses are bind addresses only. The role's Service hostname with ports 5557 and
5558 is the dialable form published in status, and both ports are declared on the producing
container. A decode-only role does not need to publish. A role with `roles[].command` receives none
of this configuration because the operator does not own its command line.

Nothing is created beside the Pod, no RBAC for one is needed, and the configuration's lifetime is
exactly the replica's. It is also part of the Pod's spec hash, which is what moves the replicas when
the pool's published endpoint changes.

It sits under `/etc` rather than in the image's workspace so that a role's own volumes are
unlikely to collide, but an overlay that mounts over that path replaces the configuration silently,
and the owned `MOONCAKE_CONFIG_PATH` cannot protect against it. SGLang gets no file at all; its
configuration travels entirely in the environment.

## Runner image formula

A role with no `roles[].image` gets one assembled from the engine the deployment declares and the
hardware its InstanceType observed. A stated image always wins.

```text
gpustack/runner:<backend><runtimeVersion>[-<variant>]-<engine><version>
```

`gpustack/runner:cuda12.9-vllm0.29.0` on an NVIDIA pool; `gpustack/runner:cann9.0-910b-sglang0.5.18`
on an Ascend 910B one. The platform is not part of the tag: no published tag carries an
architecture, which indicates one multi-arch manifest per tag.

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

Two synthesis failures read alike and are not the same: a manufacturer with no backend, or a family
with no variant, will **never** resolve and the role has to name an image, while an unobserved
runtime version resolves on a later reconcile. Each message says which one it is.

**A pool mid driver rollout does not agree on a runtime version.** The image takes the **lowest**
version the pool reports, because a workload's image is fixed before admission chooses its node and
only the lowest runs everywhere. The deployment then carries a `RuntimeVersionSkew` warning event
naming the value taken and the ones skipped, so the node holding the pool back is legible instead of
appearing as an unattributable `ImagePullBackOff`.

## Rollout behavior

Changing `replicas` adds or removes instances and nothing more: the survivors are not restarted, do
not reload their weights and keep their cached blocks. What still replaces **every** instance of the
role is an edit that changes what a replica's Pod renders: `size`, `instanceType`, `resources`,
`command`, `image`, `extraArgs`, `env`, `ports`, `additionalVolumes`, `shmSize`,
`terminationGracePeriodSeconds`.

Such an edit **deletes and recreates** the role's replicas, one replica per role per pass, waited
out. The role set itself cannot be edited at all; admission refuses a role added, removed, renamed
or moved to another kind, so there is none of that to roll. There are no surge or unavailable knobs.

The one-at-a-time cadence is a constraint: each replica's group declares a total — the role's
`size` — and a replacement created beside its still-counted member reads as excess, which Kueue
answers by deleting the newer Pod, the replacement itself.

A replacement is a **fresh admission**, not a rider on the reservation the departed replica held:
freeing the slot deletes that replica's Workload, and the reservation goes with it.

The cadence guard therefore turns a healthy replica over only when every replica the role declares holds an
admitted Workload; on a full pool a rollout waits for capacity rather than shedding replicas it
cannot re-reserve. That gate holds back deleting the next healthy replica, not the configuration
already waiting in a replacement slot: a queued replacement is superseded by a newer edit and
admitted once, as the newer shape — the obsolete configuration need never be admitted.

The slot is reused, but its obsolete Pods and Workload are deleted and replaced with fresh objects.

A complete group still waiting for its first admission can also be replaced after a configuration edit.
It uses the same single replacement slot and fresh admission rules.
The operator selects this queued group before deleting an admitted sibling.
This exception requires every member to remain behind Kueue's admission gate.
An eviction, preemption, or earlier execution does not qualify as first admission.

### Replacing a role whose shape moved

`size`, `instanceType`, `resources` and `command` roll the same way as any Pod field, and each
carries consequences the Pod fields do not.

**One unresolved replacement per role.** A replacement's fresh admission gates deleting the next
old replica, so a role turns over one replica at a time no matter how many of its fields one edit
moved: a patch changing `size`, `resources` and `command` together is one rollout, not three. The
role remembers which replica it is replacing until that replacement is admitted, so an operator
restart resumes the same rollout instead of selecting a second replica.

**A broken replica does not jump the queue.** An old replica that loses a member while its role's
replacement is unresolved waits behind that replacement, then is rebuilt whole before another
healthy old replica is rolled. A replacement that itself loses a member is recovered in its own
slot. Recovery never mixes members: an interrupted replacement is completed with its own
configuration, and an old survivor is never filled with new-configuration members.

**The old replicas run the configuration they were created with until each is deleted for
replacement.** Nothing reshapes a running instance: an instance of two Pods keeps its two members,
its leader address and its rank layout until it leaves. Cache injection, the unmanaged marker and
retirement read the deployed Pods, so a role part-way through a `command` edit serves through
replicas in both modes, and what status claims is what is actually deployed.

A role moving between a managed command and a takeover keeps each running Pod's own mode. A managed
replica keeps its qualification rules. Once qualification is active, an Unknown observation keeps
existing eligibility without granting new eligibility; a definite fault still withdraws it. A role
whose qualification was never activated keeps its legacy readiness rule.

A takeover replica keeps its legacy Service membership and stays `unmanaged: true`. Routing
membership does not establish engine health or cache qualification.

**Mixed shapes can appear across instances, never within one.** A replacement is created only
after the instance it replaces has left — members and Workload — so no ordinal holds old and
new members at once. Replicas not yet selected keep serving their old shape beside replicas
already replaced onto the new one; above `size: 1` each shape's leader is picked by the
member-index label, so a leader of each shape can answer the same Service during that window.

Peer DNS is per instance — one headless Service per replica — so the old and the new members
resolve their own shape's peers and never each other's. Each instance keeps its peer Service until
its members have left, so a terminating replica's members can still resolve one another. Each
deployed shape also answers its own way while both serve: a leader-served replica through its
leader, an External DP replica through its rank-carrying members.

**A one-replica role has a gap.** Its only instance must leave before its replacement is admitted,
so serving stops until the replacement is ready. The replacement's Kueue admission gates the next
old replica's departure. Admission does not wait for engine readiness, so the next old replica may
leave while the previous replacement is still starting.

A multi-replica role can temporarily have no serving capacity. Replacement does not guarantee
uninterrupted inference.

**Siblings are untouched unless the edit reaches them.** An edit to one role's shape never rewrites
another role's groups: their Workloads, admission, member names and DNS stay as they are.

The exception is the shared transfer document: on the vLLM-Ascend leg the parallel degrees one role
declares render into the document both roles carry, so a degree edit — through `extraArgs` or
through a replaced `command` — rolls the pair, and until both halves converge the pair can fail to
complete a transfer. See [Prefill and Decode](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md#direct-transfer-transport) for what
that window means.

**Admission rejudges a moved dependency, not the world.** The interface and pool-transport checks
re-run for a role whose own interface request, type or command presence moved, for the roles whose
rendered direct-transfer leg a router or transfer edit actually changes — a router fronting no
prefill/decode pair renders none — and on a type move for the moved role. Everything else keeps its
admitted verdict, so a drifted store is repaired at the pool, not refused at the next edit.

A role carrying `elasticEp` is excluded from all of this: the profile pins that role's shape, and
width is the only thing that moves ([Elastic EP](/gpustack-operator/v0.9.0/docs/modules/model-deployment/elastic-ep/index.md#changing-the-width)).

The cost rides on the block lease described under
[Workload impact](/gpustack-operator/v0.9.0/docs/modules/kv-cache/injection/index.md#workload-impact): a lease survives a long queue and does **not**
survive an interrupted heartbeat, which is what a departing replica is. A departing replica
therefore costs its siblings the blocks it held.

The deployment records an event naming the replica and the lease window on each of three paths
(`ReplicaEvicted`, `ReplicaLeaving`, `ReplicaRestarted`), so an operator correlating a burst of
failed requests with a replica that went away has the correlation written down rather than inferred.

**An upgrade can trigger the same turnover without any spec edit.** The fingerprint covers a replica's
labels, annotations and spec, so a release that changes what every replica renders turns each one over
once: the `role-kind` label above did, and so did the [drain](/gpustack-operator/v0.9.0/docs/modules/model-deployment/shutdown/index.md). Nothing
is required of you, but on a busy deployment the restart is worth scheduling.

### Replica departures

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

A delete that lands on the Pod alone (a drain, a kubelet eviction, `kubectl delete pod`) leaves
that replica's Workload standing, and the Workload is what holds the slot: the Pod stays readable on
the API server, and the slot is freed only when that Workload is deleted — which the deployment's
controller does when it replaces the replica. The replacement is created once no Pod for that
ordinal reads on the API server, on the terms the callout above states.

The gate reads the ordinal and nothing else, which is why a scale-up is immediate: an ordinal nothing
ever occupied has no Pod to wait out, so its replica is created on the first pass that sees it.

On a cluster with preemption enabled, a replica going away is routine rather than an incident, worth
knowing before you chase one as a fault.

**A replica on a node that is NotReady but still registered is never replaced.** Its Pod keeps its
`nodeName` and a Running phase, so its ordinal still reads occupied and no replacement is created.
Force-deleting that Pod is how two processes end up holding one accelerator; deleting the Node object
resolves it, which is what a cluster that replaces nodes already does.

The replacement carries a fresh name the API server assigns, never the departed Pod's name — so
nothing can predict a replica's name, and anything that addresses replicas should select by label
instead:

```bash
kubectl get pods -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=<deployment>
```

Add `,app.kubernetes.io/component=<role>` for one role's replicas, or
`,modeldeployment.gpustack.ai/role-kind=prefill` for every prefiller regardless of what its role is
called. A runbook that spells `<deployment>-<role>-0` breaks here and has no fixed name to move to.
Changing `replicas` is none of these departures — see [Rollout behavior](#rollout-behavior) — and the
role set cannot be edited at all; admission refuses it.

### Deployment identity fields

Fields that identify the deployment are frozen. Fields that control how it runs are editable; the
table below lists both groups.

| Frozen | Editable |
|---|---|
| `model`, `engine.name`, `kvCache` | `engine.version`, `kvTransfer` |
| `router.name` in place — the router block itself may be added or removed | `router.replicas`, `router.extraArgs` |
| the set of roles, and each role's `name` and `kind` | `roles[].replicas`, `roles[].size` |
| on a role carrying `elasticEp`: its `size`, `instanceType`, `resources` and `command` | `roles[].instanceType`, `roles[].resources`, `roles[].command` |
| | `roles[].extraArgs`, `roles[].env` |
| | the role's own Pod fields — `image`, `imagePullPolicy`, `imagePullSecrets`, `privileged`, `ports`, `additionalVolumes`, `shmSize`, `terminationGracePeriodSeconds` |
| | labels and annotations |

The role's shape fields — `size`, `instanceType`, `resources`, `command` — are editable on the same
terms as the Pod fields: each edit replaces that role's replicas one at a time, deleting each old
instance before creating its replacement, whose fresh admission gates the next deletion.
`roles[].privileged` stays editable because it controls how the role runs.

The elastic-EP row is the exception: the profile admits one fixed member shape, and
[width](/gpustack-operator/v0.9.0/docs/modules/model-deployment/elastic-ep/index.md#changing-the-width) is the only shape knob it offers.

**To change a frozen field, create another deployment.** A frozen field is not a lock protecting a
concurrent writer, and the refusal says so: what you are describing is a different deployment, so it
is created rather than edited. Editing is what keeps the object's name, its `status` history and its
cache-pool registration; a frozen field is none of those things.

When adding a field, decide whether it identifies the deployment or controls how it runs before
choosing its update rule.

> **A merge patch replaces the `roles` list wholesale.** `kubectl patch --type=merge` follows
> RFC 7386, which replaces an array rather than merging into it: a role restated with only some of
> its fields loses the ones it did not carry. An omitted `size` lands on the default of 1 and an
> omitted `resources` lands on the accelerator default, exactly as a full-list rewrite says; a role
> restated without its `name` is refused for the missing name, and one restated without `instanceType`
> is refused for the empty value — none of these fields has a default that can stand in.
> `listType=map` does not soften this: it is what keeps role names unique and drives `kubectl apply`'s
> merge, not what a client-side merge patch does to an array.
>
> `kind` still binds too: a role restated without its `kind` picks up the default `Server`, so
> restating a prefiller that way is refused as a kind change rather than silently converting it.
>
> Change one field with a JSON patch, or restate the role in full:
>
> ```bash
> kubectl patch modeldeployment qwen-chat -n team-a --type=json \
>   -p '[{"op":"replace","path":"/spec/roles/0/size","value":2}]'
> ```
>
> ```yaml
> # --type=merge --patch-file=role.yaml: the whole role, not the field
> spec:
>   roles:
>     - name: server
>       kind: Server
>       replicas: 4
>       size: 2
>       instanceType: h20-8x
>       resources:
>         accelerator: "2"
> ```

### One group per replica

The grouping key is the **replica**: every replica of every role forms its own pod group, derived from
the role and the replica's ordinal within it, and Kueue composes one Workload per group with a
declared total equal to the role's `size`. Two roles naming the same `instanceType` share nothing:
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

## Admission refusals

Two webhooks make up the admission surface. Nearly every default lives in the CRD schema; the
mutating half exists for the one value a schema cannot reach: a role's accelerator count, which
depends on the `InstanceType` the role names.

| Refused | Message names |
|---|---|
| more than 10 roles | the bound as **this operator's own shape limit**, not an upstream number — every role renders its own replicas, Services and queue references |
| a role whose members could not be named | the longest name the declared `replicas` and `size` would produce, its length and why it is not a hostname, and the three ways out — shorten the role, shorten the deployment, or declare fewer replicas |
| two roles sharing a `name` | the duplicate — refused by the **schema**, since `roles` is a list keyed on `name`, so this one never reaches the webhook |
| an edit to an identity field — `model`, `engine.name`, `kvCache`, or the shape of the roles | the field path, and that a different value describes a different **deployment**, which is created rather than edited. See [Deployment identity fields](#deployment-identity-fields) |
| a resource mode the named `InstanceType` does not offer | the mode and the type — a slice on a type that offers no slicing, a partition profile on a type that cannot partition, or one outside its profile inventory, with the offered list |
| a whole-accelerator count over the type's whole-accelerator capacity | the capacity itself, not only that the request was too large, so the next attempt is not a guess. The bound is the pool's total, not what is free, so a deployment submitted while every accelerator is held is admitted and waits in its queue; one above the largest node but within the total is admitted and stays queued — see [Accelerator Requests](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md#limitations) |
| a negative or fractional `resources.interface`, or one with no effective RDMA/EFA leg | the role's interface field and the protocol that prevents allocation; mixed backend groups and mixed fabric legs are rejected |
| an explicit `accelerator: 0` on an acceleratable `InstanceType` shared by another role | the accelerator field, the shared type, and two recommended remedies: request at least one accelerator or move the CPU-only role to a non-acceleratable type |
| a `Prefill` and a `Decode` role both requesting a **logical slice** from types that draw on the same accelerator group | both roles and the slice field. Whole cards and partition profiles are accepted — including on one card, because partitions are isolated by the device |
| a role whose `<deployment>-<role>` is not a DNS-1035 label | the combined **Service** name, which is what the pair becomes; over 63 characters or carrying a dot from a subdomain-shaped deployment name. A role the object **already had** is exempt, so a rule added later cannot strand a stored object |
| two roles whose Services would be named the same | the shared name and both claimants — a role named `x-r0` collides with a role `x` of several members, whose instance 0 is published behind `<deployment>-x-r0`. Checked on every edit, since `replicas` decides how many instance Services a role derives |
| an invalid topology `requiredLevel` | the field path and the [topology placement](#topology-placement) field rule |
| a `replicas` over 1024, or a `size` over 64 | the bound — refused by the **schema**. It limits how many Pods one pass renders before it writes any of them, so it is this operator's own ceiling rather than a Kubernetes one |
| `kind: Server` beside any other kind | that a server serves whole requests by itself, so the combination describes no arrangement |
| a `kind` the engine has no rendering term for | the engine and the kind. No engine this API accepts is refused by this rule today: vLLM and SGLang both render `Server`, `Prefill` and `Decode` |
| an owned key in `extraArgs` | the key, the engine, and `roles[].command` as the way to own it |
| an owned name in `env` | the same three |
| `--kv-cache-dtype` in `extraArgs` while `spec.kvCache` is set | that it carries the Binding's `dtype`, and a Binding declaring another dtype or `roles[].command` as the ways out — see [Operator-owned keys](#operator-owned-keys) |
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
would let a deleted `InstanceType` block the very update that clears the deployment's finalizer, an
object its own teardown could never release.

The price is named rather than hidden: an edit made while a deployment is being deleted can move a
role onto a mode its type does not offer, and nothing renders the result. Why that trade is necessary
rather than merely tidy is in
[Update validation while an object is deleted](/gpustack-operator/v0.9.0/docs/modules/devices/admission/index.md#update-validation-while-an-object-is-deleted).

A manufacturer with no runner backend is still refused **at render time and not at admission**, and the
reason is not the missing client. The rule needs the InstanceType's OBSERVED detail, and
`InstanceType.status` has not converged on a freshly created object, so the rule would refuse a
perfectly legal deployment for losing a race against the InstanceType reconciler.

The render-time refusal reaches a reader as a `RenderFailed` warning event carrying the renderer's
own message, because a pass that cannot build a replica aborts before writing any status.

## Operating notes

**Two notes apply to every workload on a pool, replicas included, and are stated once under**
[Workload impact](/gpustack-operator/v0.9.0/docs/modules/kv-cache/injection/index.md#workload-impact): the transfer engine binds ports nobody
configured, so a NetworkPolicy or port reservation has to be a range rather than a list; and the
"Local segment descriptor not found" line the transfer engine logs at startup is an `ERROR` that is
benign on a client mounting no segment of its own, which is what every replica here is.

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

**See also** — [KV Cache Pool](/gpustack-operator/v0.9.0/docs/modules/kv-cache/pool/index.md) for the Binding that grants the quota and declares
the domain · [Elastic EP](/gpustack-operator/v0.9.0/docs/modules/model-deployment/elastic-ep/index.md) for the profile that resizes one serving instance's collective
instead of adding replicas · [Model Deployment Prefill and Decode](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md) for
what pairs a prefill role with a decode role · [Accelerator Requests](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md) for the request fields
`roles[].resources` mirrors · [Admission](/gpustack-operator/v0.9.0/docs/modules/devices/admission/index.md) for the gates a replica passes
as an ordinary Pod · [Model Deployment Status](/gpustack-operator/v0.9.0/docs/modules/model-deployment/status/index.md) for what each condition
and published field means · [Model Deployment Metrics](/gpustack-operator/v0.9.0/docs/modules/model-deployment/metrics/index.md) for the
structured snapshot and Pod scrape endpoints.

**Next** → [Accelerator Requests](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md)
