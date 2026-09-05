# Model Deployment Reference

> **Purpose** — the `ModelDeployment` contract: what you declare, what the operator owns and will
> refuse to merge, how a role's runner image is assembled, and what each status condition means.
> **Audience** users, operators, contributors · **Prerequisites** [KV Cache Pool](../kv-cache/pool.md) ·
> **Read time** ~13 min

A `ModelDeployment` is N replicas of one inference-engine role attached to a KV cache pool, so that
the replicas hit each other's cached prefixes instead of each re-computing the same prefill.

It renders **Pods** directly, which is why it needs no new admission gate: a Pod is a first-class
citizen of the chain in [Admission](../architecture/admission.md), so every rule there applies to a
replica unchanged.

## Contents

- [A minimal deployment](#a-minimal-deployment)
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

`roles` is a list from the first version, and this version accepts exactly one entry. The
length-1 bound lives in the validating webhook rather than in the schema so that the refusal can name
the spec that lifts it.

`replicas` and `instanceType` are structured fields and stay so: they are inputs to Kueue PodSet
counts and flavor selection, so a template able to shadow them would make the feasibility check read
a ledger that does not match reality.

Each replica's accelerator request lives in `roles[].resources`, whose fields mirror
[Accelerator Requests](../accelerator-requests.md). CPU, memory and ephemeral storage are **derived**
from the InstanceType's per-unit resources scaled by the card count, so they are not expressible here.

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

## What the operator owns

Ownership is per **(engine, key)**: a key one engine owns is an ordinary user argument on another.
`SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` is meaningless to `vllm` and is a plain user variable there.

| Engine | Owned arguments | Owned environment |
|---|---|---|
| `vllm` | `--kv-transfer-config` | `MOONCAKE_CONFIG_PATH` |
| `sglang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config` | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME` |

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

Note that `MOONCAKE_CONFIG_PATH` is owned on `vllm` and **not** on `sglang`, while six other
`MOONCAKE_*` names are owned on `sglang` alone. The table is the authority; a name prefix is not.

On the vLLM family the operator mounts the rendered client JSON at
`/etc/gpustack/kvcache/mooncake.json`, read-only, from one ConfigMap shared by every replica. It sits
under `/etc` rather than in the image's workspace so that a template's own volumes are unlikely to
collide — but an overlay that mounts over that path replaces the configuration silently, and the
owned `MOONCAKE_CONFIG_PATH` cannot protect against it. SGLang gets no file and no ConfigMap.

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

`engineVersion` is required, free-form and **unvalidated** — the operator checks neither that the
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

## Status

`status.phase` is the field to read first: `Starting`, `Ready`, `Degraded` or `Deleting`. `Degraded`
means some replicas are ready and some are not — serving, at less than the capacity asked for.
`status.roles[]` carries `desired`, `ready` and `unmanaged` per role, and `status.endpoint` is the one
address every replica serves behind, in the form `http://<name>.<namespace>.svc:<port>`.

Three conditions carry the axes a single phase cannot. They are independent: "quota reserved but cache
not attached" is a real and actionable state.

**`DomainRegistered`** — whether the referenced Binding resolved and its domain was read.

| Value | Reason | Where it sends you |
|---|---|---|
| `True` | `Registered` | nowhere; the domain in `status.kvCache` is current |
| `False` | `BindingNotFound` | create the Binding — an admin doing so is what grants access |
| `False` | `BindingNotReady` | wait for it, or look at the pool it points at |
| `False` | `BindingDeleting` | find who deleted it; the replicas keep writing to the domain they attached to |

**`QuotaReserved`** — whether every replica holds Kueue quota. It reads each Workload's own conditions
rather than asking the admission gate, because the gate stops evaluating a Workload once it is
admitted: anything derived from the gate would answer for the moment of admission and never again.

| Value | Reason | Meaning |
|---|---|---|
| `True` | `Reserved` | every replica has quota reserved in the named cluster queue |
| `False` | `Pending` | some replicas are waiting for quota |
| `Unknown` | `AdmissionInFlight` | some replicas have no Workload yet — Kueue creates them asynchronously, so absence is admission in flight, not refusal |
| `Unknown` | `NoReplicas` | no replica has been created yet |

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

## What admission refuses

One validating webhook is the whole admission surface for this CR; defaults live in the CRD schema,
so there is no mutating webhook.

| Refused | Message names |
|---|---|
| more than one role | the spec that lifts the bound |
| an owned key in `extraArgs` | the key, the engine, and `template.command` as the way to own it |
| an owned name in `env` | the same three |
| `template.resources` | `roles[].resources` and `roles[].instanceType` as where the request is decided |
| a partition profile together with a slice percentage | both slice fields; one accelerator cannot serve both |
| a `poolRef` outside this namespace | nothing — it is unrepresentable in the type |
| a self-declared reuse domain | nothing — the field does not exist |

A manufacturer with no runner backend is refused **at render time and not at admission**, and the
difference matters: admission reads `InstanceType.status`, which has not converged on a freshly
created object, so a webhook would refuse a perfectly legal deployment for losing a race against the
InstanceType reconciler.

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
