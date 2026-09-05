# KV Cache Injection Reference

> **Purpose** — how any Pod joins a KV cache pool with one label and a handful of annotations: the
> contract, what gets injected per engine, every refusal and its fix, and the operational facts a
> cache changes about a workload.
> **Audience** users, operators · **Prerequisites** [KV Cache Pool](../kv-cache/pool.md) ·
> **Read time** ~14 min

A `KVCachePool` is usable by any Pod, not only by workloads this operator renders. A mutating
admission webhook watches for one label, reads the `KVCachePoolBinding` the Pod names, and writes the
client configuration its inference engine expects. Nothing else about the Pod changes.

A Pod that does not carry the label is left untouched **by this webhook** — not byte-identical after
admission, since the API server still defaults fields such as the service-account volume, and other
webhooks may write too.

## Contents

- [The contract](#the-contract)
- [What gets injected, per engine](#what-gets-injected-per-engine)
- [Refusals and their fixes](#refusals-and-their-fixes)
- [The cluster needs a Binding whose domain is `default`](#the-cluster-needs-a-binding-whose-domain-is-default)
- [The vLLM vehicle needs vLLM 0.21.1 or newer](#the-vllm-vehicle-needs-vllm-0211-or-newer)
- [Reading the injection record](#reading-the-injection-record)
- [Isolation is per engine, and so is the `default` Binding](#isolation-is-per-engine-and-so-is-the-default-binding)
- [What a cache changes about a workload](#what-a-cache-changes-about-a-workload)

## The contract

A Pod opts in with a **label** and configures the injection with **annotations**. The trigger is a
label because a webhook's `objectSelector` can only match labels; a label value is capped at 63
characters, so everything of unbounded length is an annotation.

| Kind | Key | Value | Required |
|---|---|---|---|
| label | `kvcache.gpustack.ai/inject` | `"true"` | yes — the trigger |
| annotation | `kvcache.gpustack.ai/binding` | a `KVCachePoolBinding` name, in this Pod's namespace | yes |
| annotation | `kvcache.gpustack.ai/engine` | `vllm` \| `vllm-ascend` \| `sglang` | yes |
| annotation | `kvcache.gpustack.ai/role` | `prefill` \| `decode` | no — **vLLM family only**; SGLang refuses any role |
| annotation | `kvcache.gpustack.ai/container` | a container name | only when the Pod has more than one container |

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: chat
spec:
  template:
    metadata:
      labels:
        kvcache.gpustack.ai/inject: "true"
      annotations:
        kvcache.gpustack.ai/binding: chat
        kvcache.gpustack.ai/engine: vllm
    spec:
      containers:
        - name: server
          image: vllm/vllm-openai:v0.25.1
          args: ["--model", "Qwen/Qwen3-8B"]   # see the refusal on an empty args
```

The engine is **declared, never guessed from the image**. Engines take entirely different flags, and a
renamed or vendored image sniffed wrongly produces a container that starts normally and caches
nothing.

There is deliberately **no domain annotation**. The reuse domain comes from the Binding, because the
Binding is the object that registers a domain and carries its ceiling — so every domain this operator
provisions has something accounting for it. `kvcache.gpustack.ai/domain` is refused rather than
ignored, so a manifest written against an escape hatch that does not exist fails where its author can
see it.

That is a provisioning contract, **not an isolation boundary** — what a Binding does and does not
bound is stated in [What a Binding does not do](../kv-cache/pool.md#what-a-binding-does-not-do), with
issue #168 for the gap. The consequence that belongs here is narrower: this webhook never overwrites
a variable the workload declared, so a container setting `MOONCAKE_TENANT_ID` itself keeps that
value, and refusing the annotation changes nothing about what that container can reach.

## What gets injected, per engine

The vehicle differs per engine, and the reason is *when* a value can be known rather than which keys
an engine accepts. `local_hostname` is an address, and a mutating webhook runs before a Pod has an IP.
vLLM computes its own at startup; SGLang reads it from configuration, and only its environment path
consults the process environment — where Kubernetes can supply the Pod's IP through a `fieldRef` the
kubelet resolves at container start.

| Engine | Vehicle | What lands on the container |
|---|---|---|
| `vllm` | a projected file | arg `--kv-transfer-config` selecting `MooncakeStoreConnector` and the role; env `MOONCAKE_CONFIG_PATH`; a read-only volume and mount at `/etc/gpustack/kvcache` |
| `vllm-ascend` | a projected file | the same, except the connector is `AscendStoreConnector` — the two engines share the vehicle and the file's keys, but not a connector registry |
| `sglang` | environment variables | arg `--hicache-storage-backend mooncake`; the `MOONCAKE_*` variables below; **no** volume and **no** mount |

The file is a `downwardAPI` projection of the Pod's own `kvcache.gpustack.ai/client-config`
annotation. No ConfigMap is created, so the webhook needs no RBAC for one and leaves nothing to
garbage-collect: the configuration's lifetime is exactly the Pod's.

The values are the same on every engine; only the spellings differ. Both vLLM-family engines read
the same file with the same key names — where they differ is the connector selected alongside it,
and which keys their readers know.

| Value | vLLM file key | SGLang variable |
|---|---|---|
| the pool's `status.clientEndpoint` | `master_server_address` | `MOONCAKE_MASTER` |
| the metadata plane, always the literal `P2PHANDSHAKE` | `metadata_server` | `MOONCAKE_TE_META_DATA_SERVER` |
| the backend's transport, always written | `protocol` | `MOONCAKE_PROTOCOL` |
| the RDMA device filter, always empty | `device_name` | `MOONCAKE_DEVICE` |
| the contributed storage segment, always `0` | `global_segment_size` | `MOONCAKE_GLOBAL_SEGMENT_SIZE` |
| the pure-client topology | `mode: standalone-store` | no key — SGLang has none |
| the client staging buffer | `local_buffer_size: 128 MiB` | no key — SGLang hardcodes 16 MiB |
| the Pod's own address | no key — vLLM computes it | `MOONCAKE_LOCAL_HOSTNAME`, a `fieldRef` to `status.podIP` |

> **Why** — every key is written explicitly rather than left to a default, because two of the
> defaults are GiB of host memory: 4 GiB per key on vLLM, 1 GiB on vLLM-Ascend. An absent
> `global_segment_size` makes the engine container a store member of that size; an absent
> `local_buffer_size` holds that much staging. Neither appears in the container's `resources`, and
> the symptom is an OOM pointing at no field anybody wrote.

`device_name` is empty on **every** path, RDMA included. Empty means "use every device found", which is
the only value correct for every host in one pool — a device is named per host, `mlx5_0` on one and
`erdma_0` on the next. The documented string `auto-discovery` is not special-cased anywhere in the
client: it is parsed as a filter naming a device no host has.

Two observability variables, `MC_TE_METRIC` and `MC_STORE_CLIENT_METRIC_BANDWIDTH`, are set to `1`
when the container has not spoken about them. A value you set yourself is left alone.

**A variable you declare yourself wins — except the one that selects the mechanism.** The injection
fills in what a container has not declared and never overrules an `env` entry the workload carries.
`MOONCAKE_CONFIG_PATH` is the exception: it names the file rather than carrying a value inside it, so
declaring it yourself is **refused at admission** rather than honoured. Two containers pointing at
two different configurations is an ambiguity nothing would report.

This applies only to `env`: a value supplied through `envFrom` is invisible to the check and **will
be overwritten with no symptom**, so declare Mooncake variables in `env`.

## Refusals and their fixes

The webhook fails closed and refuses rather than guessing, because every case below produces a
container that starts normally and does not use the cache — a result invisible from outside the Pod.

| The message names | Why it refuses | The fix |
|---|---|---|
| the `binding` annotation | it is required; it selects the pool and the declared reuse domain to resolve — not necessarily the Binding the writes are charged to | set it to a `KVCachePoolBinding` in this Pod's namespace |
| a `/` in the binding value | there is no cross-namespace form; a Binding is resolved in the Pod's own namespace | use a plain name |
| a Binding that does not exist, and the namespace | without it there is nothing to resolve the provisioned domain and endpoint from | create the Binding, or fix the name |
| a pool or backend that does not exist | the Binding points at something missing | fix the `poolRef`, or create the pool |
| the pool and `QuotaLedgerAvailable`, with the controller's own reason | `MultiTenancyDisabled` means the master holds no tenant ledger; `LedgerUnreachable` means a request to it failed, which is an outage rather than a setting | read the reason: turn multi-tenancy on for the first, restore the master for the second, or wait if the condition is not reported yet |
| the `engine` annotation | it is required and never guessed from an image | set `vllm`, `vllm-ascend` or `sglang` |
| the container count and their names | several containers and none named; the first is never chosen | set `kvcache.gpustack.ai/container` |
| a named container that is an init container | it finishes before the workload starts, so configuring it caches nothing | name an app container |
| a key **this Pod's own engine** would be given — `MOONCAKE_CONFIG_PATH` or `--kv-transfer-config` on the vLLM family, `--hicache-storage-backend` on SGLang | the container already has a KV cache configured, and two sources for one setting is undiagnosable | remove yours, or drop the inject label |
| a Binding that exists but is being deleted | its reuse domain is being withdrawn from the ledger, so the Pod would be injected and then fail every write with `TENANT_NOT_REGISTERED`, and waiting does not heal it | wait for the deletion to finish and create a new Binding, or point the Pod at one that is not terminating |
| a pool that has published no client endpoint yet | there is no address to point the engine at | wait for the pool to report `status.clientEndpoint` |
| a volume name or mount path the webhook owns | the same collision, in the Pod's storage | rename yours |
| a container declaring **neither** `command` nor `args` | appending would not append: Kubernetes then reads `args` as the whole command line and discards the image's `CMD` | copy the image's launch arguments into **`args`**, leaving `command` unset |
| a container launched through a shell's `-c` | an appended flag becomes the shell's `$0`, so it never reaches the engine and the Pod is stamped as injected anyway | launch the engine directly — its executable in `command`, its arguments in `args` — or add the connector flag to the script yourself |
| the `role` annotation on an SGLang Pod | that engine has no prefill/decode equivalent, and accepting the role while ignoring it would leave the container looking configured and behaving otherwise | drop the annotation, or use a vLLM-family engine |
| an unrecognised `kvcache.gpustack.ai/` key | a typo would otherwise be ignored, leaving the Pod configured differently from its manifest | fix the key |
| `kvcache.gpustack.ai/client-config` or `.../injected` | these record what the webhook decided; a submitted value would be a record of a decision nobody made | remove them |

> **Why** the `command` warning — putting the launch arguments in `command` overrides the image's
> `ENTRYPOINT` as well, and on this project's accelerator images that entrypoint initializes the vendor
> runtime. The resulting failure is further from its cause than the discarded `CMD` the refusal exists
> to prevent.

Two keys are **not** conflicts and are left alone: `MOONCAKE_TENANT_ID`, which the webhook does write
for SGLang but never over a value you set yourself — declare it and yours stands, and the injection
record then reports `"tenantInjected":false` — and `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, which means
you have configured SGLang from a file of your own. In the second case the injected variables silently stop mattering — see the note under
[Reading the injection record](#reading-the-injection-record).

To take future Pods back over, set `kvcache.gpustack.ai/inject: "false"` on the workload's **Pod
template**, or drop the label there. It does not undo an existing Pod: the injected args, env and
volume stay, and most of a running Pod's spec is immutable — the change takes effect when the
workload rolls.

## The cluster needs a Binding whose domain is `default`

**This applies to pools serving vLLM.** SGLang writes under its own reuse domain and needs no such
Binding — see [isolation is per engine](#isolation-is-per-engine-and-so-is-the-default-binding).

**Without one, injection succeeds, a vLLM Pod starts, stays Ready — and every write fails.** This is a
prerequisite, not a tuning knob, and it is the one failure in this document whose symptom is furthest
from its cause: what you see is a cache that never hits.

vLLM does not send a tenant (see [the section on per-engine isolation](#isolation-is-per-engine-and-so-is-the-default-binding)),
so for that engine the Mooncake client falls back to its own default — the literal string `default`. A `KVCachePool`
is only accepted over a multi-tenant backend, and a multi-tenant master refuses a write from a tenant
that is not in its ledger:

```console
$ kubectl logs chat-0 | grep TENANT
E client_service.cpp:1893] Failed to start put operation for key=...: TENANT_NOT_REGISTERED
```

Registering that name is what a Binding does, so declare one whose reuse domain is `default`:

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: shared
  namespace: team-a
spec:
  poolRef:
    name: chat-pool
  domain:
    name: default          # the name every engine that forwards no tenant writes under
    blockSize: 16
    dtype: bfloat16
  quotaCeiling: 8Gi        # the budget for every no-tenant Pod on this pool, not for one namespace
```

Two things follow from that `quotaCeiling`, and both are consequences of there being one tenant rather
than of this being a workaround:

- It is the budget for **every injected Pod that forwards no tenant** - `vllm` and `vllm-ascend` -
  across every namespace. SGLang Pods write under their own domain and are charged to their own
  Binding, so a mixed-engine pool splits its traffic between the two. A per-namespace
  Binding's ceiling does not apply to injected traffic, because injected traffic is not written under
  that namespace's domain. Verified end to end: after an injected Pod writes, this Binding's
  `status.usage` rises and the domain-carrying Binding's stays at `0`.
- Deleting this Binding does not stop the Pods. They keep running and keep failing every write.
- Deleting it is also **held while the domain still holds objects**, so the Binding does not
  disappear the moment you ask — see [Operating notes](../kv-cache/pool.md#operating-notes) for the
  two hold reasons and how to tell a working finalizer from a stuck one. What matters here: a new Pod
  naming a Binding in that state is refused, per the table above.

Whether a given Pod is affected is on the Pod **and its engine**, not on the stamp alone.
`"tenantInjected":false` says this webhook wrote no tenant. On `vllm` and `vllm-ascend` that is the
whole story, and the writes land on the shared name. On `sglang` it has a second cause — the workload
declared `MOONCAKE_TENANT_ID` itself and the injection stood aside — and then the writes go to the
workload's own tenant instead. Read the container's environment to tell those two apart.

**When this requirement goes away.** It is conditional, not permanent: it exists because no tenant
reaches the store on the paths this webhook renders for `vllm` and `vllm-ascend`. SGLang already
writes under its Binding's own reuse domain — a name that Binding registered — and needs nothing
extra. The vLLM change is a call-site one upstream, not a Mooncake release, and it would restore real
isolation there too.

So do not check this page to find out whether the rule still applies; check the Pod together with its
engine, as above. `tenantInjected` records an **action**; reading an outcome straight off it is wrong
in exactly the SGLang case.

## The vLLM vehicle needs vLLM 0.21.1 or newer

**On an older vLLM the file is projected, mounted, and read by nothing.** No error is logged, because
no code looks for it.

vLLM's Mooncake store connector — the module holding `MooncakeStoreConfig`, `from_file` and
`load_from_config` — was added on 2026-05-13 and first shipped in `v0.21.1rc0`. Earlier releases have
no reader for `MOONCAKE_CONFIG_PATH` at all.

This is not validated, and it cannot be: admission never inspects the container image, so the webhook
does not know which build will run. The `engineVersion` on the stamp is the release our facts table
was measured at, not a reading of your image. So the check is yours, and it is one command against the
image you are actually running:

```console
$ kubectl exec chat-0 -- python3 -c \
    "import vllm.distributed.kv_transfer.kv_connector.v1.mooncake.store.worker; print('reader present')"
```

An `ImportError` means the injection is inert on that image. Note the symptom is different from the
one in the previous section, and calls for a different fix: an unregistered tenant fails loudly with
`TENANT_NOT_REGISTERED` on every write, while a missing reader fails silently — the workload runs
correctly, just with no cache at all.

SGLang has no equivalent floor: its `mooncake_store.py` predates every release this project targets.

## Reading the injection record

An injected Pod carries `kvcache.gpustack.ai/injected`, a JSON object recording what was decided:

```console
$ kubectl get pod chat-0 -o jsonpath='{.metadata.annotations.kvcache\.gpustack\.ai/injected}'
{"binding":"chat","engine":"vllm","engineVersion":"v0.25.1","vehicle":"file",
 "domain":"team-a-chat","tenantInjected":false}
```

| Field | What it answers |
|---|---|
| `binding` | which Binding this Pod named and the webhook resolved — **not** necessarily the one its writes are charged to; see the `default` Binding section |
| `engine`, `engineVersion` | what was configured, and the version the isolation answer was measured at |
| `vehicle` | `file` or `environment` |
| `domain` | the reuse domain the Binding declared |
| `tenantInjected` | whether a tenant was written into the container — an **action**, not an outcome |

`vehicle` is on the record because it turns one otherwise-silent outcome into a one-line check: a Pod
stamped `"vehicle":"environment"` whose cache stays cold is a Pod whose own
`SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` or `--hicache-storage-backend-extra-config` has taken
precedence over the injection. That precedence is correct — your explicit configuration outranks a
defaulted one — so the webhook does not refuse it, and this annotation is where you find out.

## Isolation is per engine, and so is the `default` Binding

**Whether a Binding's reuse domain actually isolates depends on the engine**, and the injection says
what it did on every Pod rather than claiming a result.

| Engine | Version measured | Reads a tenant? | What is injected |
|---|---|---|---|
| `vllm` | `v0.25.1` | no — its configuration class has no tenant key | nothing; writes land on the store's `default` tenant |
| `vllm-ascend` | `v0.19.1rc1` | no — that release carries no tenant at all | nothing; writes land on the store's `default` tenant |
| `sglang` | `v0.5.18` | **yes** — `MOONCAKE_TENANT_ID`, forwarded when it differs from `default` | the Binding's reuse domain, as that variable |

**The vLLM-Ascend row is about our configuration, not that engine's capability** — and for this
release the two now agree. The webhook selects that engine's own store, `AscendStoreConnector`
(`vllm_ascend/distributed/kv_transfer/__init__.py:39-43`), and its config reader takes six keys with
no tenant among them. A `tenant_id` in the file would be read by nobody.

An earlier revision of this page said the answer was `no` *because* the webhook selected vLLM's
generic connector instead, and implied selecting the Ascend one would change it. The webhook now
selects it and the answer did not change: `v0.19.1rc1` has no tenant anywhere. Selecting the right
connector fixes a startup failure — the engine's factory resolves connector names against a
registry, and vLLM's name is absent from that release's — and forwards nothing.

**So the `default` Binding is required for `vllm` and `vllm-ascend`, and not for `sglang`.** A pool
serving only SGLang workloads needs no such Binding: each Pod writes under its own domain, which its
own Binding already registered.

> **Known limitation — one `default` Binding per cluster, not per backend.** A Binding's reuse domain
> is unique **cluster-wide**, while a tenant ledger belongs to **one master**. On a cluster running two
> independent `KVCacheBackend`s, only one of them can hold the `default` Binding this section requires.
> Injected `vllm` and `vllm-ascend` Pods on the others are **admitted** and then fail every write with
> `TENANT_NOT_REGISTERED` — the Pod starts, stays Ready, and the cache never works. Tracked as
> [#166](https://github.com/gpustack/gpustack-operator/issues/166).

### vLLM-Ascend and a non-`ascend` transport

**A `KVCacheBackend` whose transport is not `ascend` makes a vLLM-Ascend container fail to start**, and
the injection is what triggers it. That engine accepts one transport and raises on the rest:

```text
NotImplementedError: MooncakeBackend does not support protocol 'tcp'.
```

The engine's own file reader defaults `protocol` to `ascend`, so a file that said nothing would have
worked. This project writes the key explicitly on every path — because vLLM and SGLang disagree about
what an absent one means — and that explicit value overwrites a default that was already correct here.

It is documented rather than refused. The failure is loud, immediate, and names its own cause in the
container's logs, so refusing at admission would buy very little; and the constraint is one this
project has only read on upstream `main`, so a rule built on it could reject a combination an older
deployed build accepts. Pair vLLM-Ascend with an `ascend` backend.

**The record says `tenantInjected`, never "isolated", and the difference matters.** Admission never
inspects the image, so it cannot know the build — an SGLang build older than the one our table was
measured at is handed a variable it never reads, shares the `default` tenant, and nothing reports it. A
stamp claiming isolation would be wrong in the one direction that misleads. So it states the action,
which is certain, and leaves the outcome to be inferred from the engine you actually run.

**One side of that has a hard signal; the other does not.** If your Mooncake *client* library is too
old to accept a tenant argument, SGLang raises and the Pod does not start — loud, and impossible to
miss. If your *SGLang* is too old to read the variable, there is no signal at all: the workload runs,
the cache works, and two domains quietly share one tenant. Pin the engine version you tested.

**What the sharing costs**, on either engine, when two domains land in one tenant:

- **Sharing the key space is not the problem.** Two deployments of one model sharing a prefix is the
  point of a pool.
- **Eviction is.** What a full quota actually does — evict and retry rather than refuse — is in
  [What a full quota actually does](../kv-cache/pool.md#what-a-full-quota-actually-does), along with
  why no metric reveals it. The consequence specific to sharing one tenant: **the objects evicted
  belong to the other domains**, which are not the workload that hit the ceiling.

**What is being done about it.** vLLM needs one line: pass the tenant through to the client, which
already accepts it — the same change SGLang already made. Until then, the refusal that matches the
harm — creating a **second** reuse domain against a backend that cannot separate them — belongs on the
Binding's own admission and **has not landed yet**. So on a vLLM-only pool today: **one reuse domain
per backend is safe; a second one shares a cache with the first.**

## What a cache changes about a workload

Joining a pool changes three things about a Pod that are easy to file as bugs.

**Host memory the Pod never asked for — on the vLLM family only.** The injected `local_buffer_size`
is `128 MiB` of staging the client registers with the transfer engine. It is charged to the
container's memory and appears in no `resources` field, so **add 128 MiB to both the request and the
limit of any `vllm` or `vllm-ascend` container you inject into**.

Both, not just the limit. The limit alone keeps one container off a cgroup OOM, but the scheduler
places Pods by their *requests* — so raising only the limit lets a node be filled to its request
capacity while every injected Pod on it consumes 128 MiB more than that arithmetic accounted for.
The result is node memory pressure and kubelet eviction, on a node whose bookkeeping says it is
within budget.

SGLang is not given one: the injection writes no `local_buffer_size` in any spelling, and that engine
hardcodes 16 MiB (v0.5.18 `mooncake_store.py:28`, `DEFAULT_LOCAL_BUFFER_SIZE`, used at `:464` and
`:514`). Budget those 16 MiB the same
way — in the request as well as the limit.

> **Why** that number, and why it is written at all — it is the value the store's own reference uses,
> and it is a constant here rather than a field because it is transfer-layer staging, not a resource
> grant. What an absent key costs instead is under
> [What gets injected, per engine](#what-gets-injected-per-engine).

**Random ports.** The transfer engine binds ports nobody configured — one observed run took `15002`
and `15995`, a second client `16566` and `16655`. **Any NetworkPolicy or port reservation must be
written as a range, not a list.** The webhook cannot change this and does not try.

**A 30-second lease on cached blocks.** `kv_lease_duration` defaults to 30 seconds. It does not expire
from long queueing, but it does expire when a Pod's heartbeat is interrupted — preemption, eviction,
restart — and the default failure policy then fails the request outright. Anything that kills an
injected Pod destroys cache its peers may be waiting on.

One benign line appears in every client's startup log and is not an error to chase:

```
E transfer_metadata.cpp:991] Local segment descriptor not found
```

The projected client configuration is readable by anyone who can read the Pod. It carries addresses, a
protocol and a domain name, and no credential.

---

**See also** — [KV Cache Pool](../kv-cache/pool.md) (the grant and the reuse domain this page consumes) ·
[KV Cache Backend](../kv-cache/backend.md) (the store the pool draws from) ·
[Admission](../architecture/admission.md) (where this webhook sits relative to the five gates)

**Next** → [Accelerator Requests](../accelerator-requests.md) — the other thing a Pod asks this
operator for.
