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
- [Tenant compatibility is the image owner's responsibility](#tenant-compatibility-is-the-image-owners-responsibility)
- [Verify the vLLM file vehicle](#verify-the-vllm-file-vehicle)
- [Reading the injection record](#reading-the-injection-record)
- [Transport compatibility at binding](#transport-compatibility-at-binding)
- [What a cache changes about a workload](#what-a-cache-changes-about-a-workload)
- [What it leaves alone, and one flag that replaces it](#what-it-leaves-alone-and-one-flag-that-replaces-it)

## The contract

A Pod opts in with a **label** and configures the injection with **annotations**. The trigger is a
label because a webhook's `objectSelector` can only match labels; a label value is capped at 63
characters, so everything of unbounded length is an annotation.

| Kind | Key | Value | Required |
|---|---|---|---|
| label | `kvcache.gpustack.ai/inject` | `"true"` | yes — the trigger |
| annotation | `kvcache.gpustack.ai/binding` | a `KVCachePoolBinding` name, in this Pod's namespace | yes |
| annotation | `kvcache.gpustack.ai/engine` | `vllm` \| `sglang` | yes |
| annotation | `kvcache.gpustack.ai/manufacturer` | `ascend` | no — only with `engine: vllm`; selects the vLLM-Ascend runtime |
| annotation | `kvcache.gpustack.ai/role` | `prefill` \| `decode`; omitted for a plain server | no — **vLLM family only**; SGLang refuses any role |
| annotation | `kvcache.gpustack.ai/container` | a container name | only when the Pod has more than one container |
| annotation | `kvcache.gpustack.ai/launch-args-forwarded` | `"true"` | no — only when an unrecognised launcher, script, or image ENTRYPOINT forwards appended arguments to the declared engine |

For a plain server, one that is not half of a prefill/decode split, LEAVE THE ROLE ANNOTATION OFF.
`server` is not in its value domain and a Pod carrying it is refused, while the same arrangement is
spelled `server` on `ModelDeployment.spec.roles[].kind`, which even defaults to it. The value that
is correct there turns a Pod away here. An absent annotation renders the read-and-write
configuration a shared cache wants.

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
          image: vllm/vllm-openai:v0.29.0
          command: ["vllm"]
          args: ["serve", "--model", "Qwen/Qwen3-8B"]
```

A bare Pod follows the same contract — the label opts it in, and the optional annotations select the
role and the container:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: bench
  namespace: team-a
  labels:
    kvcache.gpustack.ai/inject: "true"          # the opt-in; a LABEL, not an annotation
  annotations:
    kvcache.gpustack.ai/binding: team-a         # a KVCachePoolBinding in this namespace
    kvcache.gpustack.ai/engine: vllm
    kvcache.gpustack.ai/role: decode            # optional: prefill or decode
    kvcache.gpustack.ai/container: server       # required with more than one container
spec:
  containers:
    - name: server
      image: vllm/vllm-openai:v0.29.0
      command: ["vllm"]
      args: ["serve", "--model", "Qwen/Qwen2.5-72B-Instruct"]
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
issue #168 for the gap. The tenant value is Binding-owned too: on SGLang the webhook replaces every
container declaration of `MOONCAKE_TENANT_ID` with the Binding's domain.

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
| `sglang` | environment variables | arg `--hicache-storage-backend mooncake` and [`--enable-hierarchical-cache`](#sglangs-host-memory-tier); the `MOONCAKE_*` variables below; **no** volume and **no** mount |

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
| the transport of the pool group the engine matched, always written | `protocol` | `MOONCAKE_PROTOCOL` |
| the RDMA device filter, always empty | `device_name` | `MOONCAKE_DEVICE` |
| the contributed storage segment, always `0` | `global_segment_size` | `MOONCAKE_GLOBAL_SEGMENT_SIZE` |
| the pure-client topology | `mode: standalone-store` | no key — SGLang has none |
| the client staging buffer | `local_buffer_size: 128 MiB` | no key — SGLang hardcodes 16 MiB |
| the Pod's own address | no key — vLLM computes it | `MOONCAKE_LOCAL_HOSTNAME`, a `fieldRef` to `status.podIP` |
| the Binding's reuse domain | `tenant_id` | `MOONCAKE_TENANT_ID` |

> **Why** — every key is written explicitly rather than left to a default, because two of the
> defaults are GiB of host memory: 4 GiB per key on vLLM, 1 GiB on vLLM-Ascend. An absent
> `global_segment_size` makes the engine container a store member of that size; an absent
> `local_buffer_size` holds that much staging. Neither appears in the container's `resources`, and
> the symptom is an OOM pointing at no field anybody wrote.

`device_name` is empty on **every** path, RDMA and EFA included. Empty means "use every device found",
which is the only value correct for every host in one pool — a device is named per host, `mlx5_0` on
one and `erdma_0` on the next. The documented string `auto-discovery` is not special-cased anywhere in
the client: it is parsed as a filter naming a device no host has.

Nothing injected here grants the Pod a fabric device. These values **name** a transport; a Pod
using this opt-in webhook must request the appropriate device-plugin resource itself. A managed
`ModelDeployment` can instead request a positive `spec.roles[].resources.interface` count, which
renders an RDMA or EFA resource limit on its engine Pod according to the effective transport.
The backend's member Pods configure their own host network and device access separately.

The one exception is the Ascend prefill/decode leg, whose engine Pods mount a host driver
tree read-only — see [Prefill and decode](model-deployment.md#prefill-and-decode).

Two observability variables, `MC_TE_METRIC` and `MC_STORE_CLIENT_METRIC_BANDWIDTH`, are set to `1`
when the container has not spoken about them. A value you set yourself is left alone.

### SGLang's host-memory tier

An SGLang container also gets `--enable-hierarchical-cache`, after the injected arguments, unless its
own arguments already name that flag. The storage backend hangs off the hierarchical cache's host
tier: naming the backend alone enables storage prefetch over a plain radix cache, and the first
request fails with an `AttributeError` (measured at SGLang v0.5.18).

That tier is a pinned host pool of `hicache_ratio` (default 2.0) times the device KV pool. SGLang
refuses to build it unless the node's available memory — read host-wide, not from the container's
limit — exceeds a fixed 10 GiB reserve plus the pool, so size the node for it. The container's
memory limit still has to hold the engine and the pool, or the result is an OOM kill.

A `ModelDeployment` renders the same switch on every SGLang role with a store except a decode half,
where SGLang forces its radix cache off and refuses the two together. A decode half gets
`--disaggregation-decode-retraction-backup cpu_tensor` instead: left unset, SGLang infers a host
pool for retraction and meets the same check with no store at all. A role's own `extraArgs` naming
either flag drops the operator's.

**An injected variable overrules one you declared yourself.** Injection is opt-in and its opt-out
is explicit, so a Pod that asked for it and then declares a Mooncake variable has given two answers
to a question the Binding already answered; the injected value is written **in place**, leaving one
entry per name rather than two.

The two observability toggles above are the exception: they change no result, so a value you set is
kept.

Two kinds of key are **refused at admission** instead of overwritten, because overwriting them
would not help.

`MOONCAKE_CONFIG_PATH` and `--kv-transfer-config` select the **mechanism** — a second one is an
ambiguity nothing reports. `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` and
`--hicache-storage-backend-extra-config` select the configuration **source**, which leaves the
injected variables present and unread. Each key, the engine it applies to and why it is refused are
under [Refusals and their fixes](#refusals-and-their-fixes).

A flag is refused in every spelling the engine's own parser reads as it — a unique prefix, and on
the vLLM family an underscored or dotted form — by the rule under
[What the operator owns](model-deployment.md#what-the-operator-owns).

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
| the pool and `QuotaLedgerAvailable`, with the controller's own reason | the pool has not reported the condition, reports it `Unknown`, or reports it `False` for a reason other than `MultiTenancyDisabled` — `LedgerUnreachable`, for one, means a request to the master failed, which is an outage rather than a setting. `False` with `MultiTenancyDisabled` is **not** refused: it is a declared single-tenant store, and the Pod is injected with no tenant id — see [What a Binding does not do](../kv-cache/pool.md#what-a-binding-does-not-do) | restore the master, or wait if the condition is not reported yet or is `Unknown` |
| the `engine` or `manufacturer` annotation | the engine is required and never guessed from an image; `manufacturer` selects the Ascend vLLM runtime | set `vllm` or `sglang`; for vLLM-Ascend, set `engine: vllm` and `manufacturer: ascend` |
| the container count and their names | several containers and none named; the first is never chosen | set `kvcache.gpustack.ai/container` |
| a named container that is an init container | it finishes before the workload starts, so configuring it caches nothing | name an app container |
| a key **this Pod's own engine** would be given — `MOONCAKE_CONFIG_PATH` or `--kv-transfer-config` on the vLLM family, `--hicache-storage-backend` on SGLang | the container already has a KV cache configured, and two sources for one setting is undiagnosable | remove yours, or drop the inject label |
| a Binding that exists but is being deleted | its reuse domain is being withdrawn from the ledger, so the Pod would be injected and then fail every write with `TENANT_NOT_REGISTERED`, and waiting does not heal it | wait for the deletion to finish and create a new Binding, or point the Pod at one that is not terminating |
| a pool that has published no client endpoint yet | there is no address to point the engine at | wait for the pool to report `status.clientEndpoint` |
| a volume name or mount path the webhook owns | the same collision, in the Pod's storage | rename yours |
| a container declaring **neither** `command` nor `args` | appending would not append: Kubernetes then reads `args` as the whole command line and discards the image's `CMD` | put the engine executable in `command` and the image's launch arguments in `args` |
| a container declaring `args` but no `command` | admission cannot inspect the image `ENTRYPOINT`, so it cannot show that appended arguments reach the engine | put the engine executable in `command`, or declare `kvcache.gpustack.ai/launch-args-forwarded: "true"` only when the image ENTRYPOINT forwards them |
| the launcher, when nothing follows it — `command: ["tini", "--"]` with `args` empty | the container names no program at all, so the appended connector flag becomes the command that launcher executes | put the engine executable and its arguments after the launcher, or in `command` and `args` directly |
| an unrecognised launch program or a program for another engine | the webhook would otherwise inject one engine's configuration into another program, or silently trust an unknown launcher | launch the engine named by the `engine` annotation directly, or declare forwarding only for an unrecognised launcher that passes appended arguments through |
| a container launched through a shell's `-c` | an appended flag becomes the shell's `$0`, so it never reaches the engine and the Pod is stamped as injected anyway | launch the engine directly — its executable in `command`, its arguments in `args` — or add the connector flag to the script yourself |
| a command line hidden inside one argument — `env -S "…"` and its `--split-string` spellings | there is nothing on the command line to test: the launcher splits that string itself, so admission cannot tell whether a shell is inside it | launch the engine directly, add the connector flag inside that argument, or declare that it forwards appended arguments |
| a program whose name ends in `.sh` — `./run.sh`, and `sh /app/run.sh` alike | whether an appended argument reaches the engine depends on whether the script forwards `"$@"`, which is a file inside the image rather than a token on the command line | launch the engine directly, add the connector flag inside the script, or declare that it forwards appended arguments |
| the `role` annotation on an SGLang Pod | that engine has no prefill/decode equivalent, and accepting the role while ignoring it would leave the container looking configured and behaving otherwise | drop the annotation, or use a vLLM-family engine |
| an unrecognised `kvcache.gpustack.ai/` key | a typo would otherwise be ignored, leaving the Pod configured differently from its manifest | fix the key |
| `kvcache.gpustack.ai/client-config` or `.../injected` | these record what the webhook decided; a submitted value would be a record of a decision nobody made | remove them |

> **Why** the `command` warning — putting the launch arguments in `command` overrides the image's
> `ENTRYPOINT` as well, and on this project's accelerator images that entrypoint initializes the vendor
> runtime. The resulting failure is further from its cause than the discarded `CMD` the refusal exists
> to prevent.

The script-specific refusal keys off the **`.sh` suffix**, which is a convention rather than a
guarantee. The general launch check also refuses a suffix-less wrapper named `entrypoint` or `run`
unless its author declares that it forwards appended arguments. Admission cannot open the file, so
the declaration is the only way to admit that uncertainty.

⛔ **A key that selects where the engine reads its store configuration from is refused**, per engine:
`SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` and `--hicache-storage-backend-extra-config` on SGLang,
`MOONCAKE_CONFIG_PATH` on vLLM. None of them collides with anything this operator writes, and that
is what makes them worth refusing: the engine selects one source out of three, so either key leaves
every injected variable present on the Pod and read by nothing.

The same key on an engine that does not read it is **not** refused — SGLang's variable means nothing
to vLLM, and a refusal there would have nothing behind it.

To take future Pods back over, set `kvcache.gpustack.ai/inject: "false"` on the workload's **Pod
template**, or drop the label there. It does not undo an existing Pod: the injected args, env and
volume stay, and most of a running Pod's spec is immutable — the change takes effect when the
workload rolls.

> **The label and the two written annotations are frozen once the Pod exists**, which is why the
> paragraph above says *template*. Editing `kvcache.gpustack.ai/inject`, `.../injected` or
> `.../client-config` on a live Pod is refused, and so is removing one. Injection runs at CREATE and
> only there, so a label added afterwards would leave a Pod carrying the label that says it uses a
> cache and none of the configuration; an edited `injected` would be a record of a decision nobody
> made; and an edited `client-config` would swap the master address, transport and segment sizes under
> a running container, since the file at `/etc/gpustack/kvcache/mooncake.json` is a `downwardAPI`
> projection of that annotation. Every other metadata edit is left alone, finalizers included.
>
> **The freeze covers a terminating Pod too**, which is where it matters most: a Pod keeps serving
> through its termination grace period, and the kubelet keeps reprojecting that file, so a master
> address swapped after the delete call would still reach the running container.
>
> That guard is a validating webhook on UPDATE with `failurePolicy: Ignore`, so it does **not** hold
> while the webhook is unreachable. The direction is deliberate: it keeps a record honest, and it must
> never be the reason a live Pod cannot be updated or finished — under `Fail` an unreachable webhook
> would block finalizer removal on every opted-in Pod, and a pool's own teardown waits behind exactly
> that. Clearing a finalizer touches none of the three keys, so it is admitted whether the webhook is
> reachable or not.

## Tenant compatibility is the image owner's responsibility

**Use an engine image that reads and forwards the injected tenant when the Binding's domain must
isolate cache reuse.** The webhook always writes a non-empty domain and never inspects or rejects the
image version. Verify the image you deploy: the vLLM family must consume `tenant_id` from the
projected file, while SGLang must consume `MOONCAKE_TENANT_ID` from its environment.

Keep the Mooncake library bundled by the engine unless that engine documents another compatible
combination. A mismatched library may reject the tenant even when the engine reads it.

An older engine is allowed. It may ignore the injected value and use Mooncake's literal `default`
tenant instead, without any admission error. Such a pool needs a `KVCachePoolBinding` whose domain is
`default`; its quota is then shared by every client that falls back to that tenant.

`tenantInjected` records only that the webhook wrote the value. It does not prove the image read it
or that isolation took effect.

## Verify the vLLM file vehicle

**On an older vLLM the file is projected, mounted, and read by nothing.** No error is logged, because
no code looks for it.

Some vLLM images have no reader for `MOONCAKE_CONFIG_PATH`. On those images the projected file is
inert.

This is not validated, and it cannot be: admission never inspects the container image, so the webhook
does not know which build will run. The check is yours, against the image you are actually running:

```console
$ kubectl exec chat-0 -- python3 -c \
    "import vllm.distributed.kv_transfer.kv_connector.v1.mooncake.store.worker; print('reader present')"
```

An `ImportError` means the injection is inert on that image. Note the symptom is different from the
one in the previous section, and calls for a different fix: an unregistered tenant fails loudly with
`TENANT_NOT_REGISTERED` on every write, while a missing reader fails silently — the workload runs
correctly, just with no cache at all.

## Reading the injection record

An injected Pod carries `kvcache.gpustack.ai/injected`, a JSON object recording what was decided:

```console
$ kubectl get pod chat-0 -o jsonpath='{.metadata.annotations.kvcache\.gpustack\.ai/injected}'
{"binding":"chat","engine":"vllm","vehicle":"file","domain":"team-a-chat",
 "tenantInjected":true,"launchProgram":"vllm","launchArgsForwarded":false}
```

| Field | What it answers |
|---|---|
| `binding` | which Binding this Pod named and the webhook resolved — **not** necessarily the one its writes are charged to; see [tenant compatibility](#tenant-compatibility-is-the-image-owners-responsibility) |
| `engine` | what was configured |
| `vehicle` | `file` or `environment` |
| `domain` | the reuse domain the Binding declared |
| `tenantInjected` | whether a tenant was written into the container — an **action**, not an outcome |
| `launchProgram` | the executable left after transparent launcher prefixes were removed; empty only where there was no executable to read and forwarding was declared anyway — an image `ENTRYPOINT`, or a command line hidden in one argument |
| `launchArgsForwarded` | whether the author's `kvcache.gpustack.ai/launch-args-forwarded: "true"` declaration admitted a launch the webhook could not identify as an engine entry point |

Pods admitted by an older operator may also carry `engineVersion`. New records omit it because it
described the upstream source used when the injector was written, not the image in the Pod. Readers
must ignore that legacy field and must not infer image compatibility from it.

`vehicle` is on the record so a reader can tell which shape was written without decoding the
container: `"file"` means a projected configuration document plus its volume, `"environment"` means
variables alone.

It used to carry a second job — telling you that your own `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` had
taken precedence and left the injection inert. That outcome no longer occurs: those keys are refused
at admission, so a Pod that was injected is a Pod whose injection is read.

## Transport compatibility at binding

An engine configured with one transport cannot safely bind to a pool whose member groups offer
different protocols. The client reads each target segment's protocol; a block on a group using a
transport the engine did not install fails with `NotSupportedTransport`. Pod injection refuses that
binding and names every effective offer. Make the groups use one protocol or choose another pool;
the mixed pool itself remains valid for consumers that can use it.

**A pool whose groups offer no `ascend` transport makes a vLLM-Ascend container fail to start**, and
the injection is what triggers it. That engine accepts one transport and raises on the rest:

```text
NotImplementedError: MooncakeBackend does not support protocol 'tcp'.
```

The engine's own file reader defaults `protocol` to `ascend`, so a file that said nothing would have
worked. This project writes the resolved pool transport explicitly on every path; that value
overwrites the engine default.

It is **refused, not left to the container**. The shared transport check rejects an injected Pod at
admission. A `ModelDeployment` checks a new binding in its own validating webhook, since its
controller path does not use Pod injection.

**The transport has two spellings and the message uses both.** What the pool offers and what the
engine accepts are reported as the artifact spells them, because that is the value the container was
handed: `tcp` against `ascend`. The value to set is the API's, **`CANN`**, because
`spec.transport.protocol` is a case-sensitive enum.

**The failing backend is not one somebody misconfigured.** `spec.transport.protocol` defaults to
`Auto`, which resolves to the store's `tcp` — so a backend left entirely at its defaults is precisely the
one this engine cannot use. Pair vLLM-Ascend with a pool that offers `CANN`: declared on the backend's
`spec.transport.protocol`, or on one member group's `transport.protocol` when only one group serves
the fabric.

**A backend may also mix vendors across member groups** — one group offering `rdma` to NVIDIA nodes,
another `ascend` to Ascend nodes — through the same per-group `transport.protocol` override.
Admission imposes no rule on that combination; the per-engine check above is untouched. Whether
cross-vendor *sharing* then works is a property of the engine's cache key, not of the transport:

| Engine | Cross-vendor sharing | Why |
|---|---|---|
| `sglang` | possible in principle | its Mooncake key is vendor-neutral — no vendor, dtype, device or engine id in the key, and no platform branch in the `mooncake` backend |
| `vllm` | not applicable | the key embeds `model`, `tp_rank`, `pcp`, `dcp` and `pp_rank`, so the key itself is heterogeneous across two vendors |

"Cross-vendor sharing is meaningless" is a statement about vLLM only. For SGLang the one remaining
precondition is that both sides lay a block's payload bytes out identically, and that has NEVER been
measured: the experiment is one model, one `page_size` and one TP/PP shape, SGLang on NVIDIA against
SGLang on Ascend pointed at one pool, checking whether the second side hits the first's entries and
reads back the correct bytes.

The transport is not the limit: the HIXL wiki documents Mooncake's `rdma` transport moving buffers
directly between an NVIDIA GPU and an Ascend NPU — [Mooncake NPU guide, appendix 2](https://gitcode.com/cann/hixl/wiki/Mooncake%EF%BC%88NPU%20%E7%89%88%EF%BC%89%E5%AE%8C%E6%95%B4%E6%8C%87%E5%8D%97.md).

The operator's `applyMemberFabric` grants host network and device access to backend member groups
only for `rdma` and `efa`. An `ascend` group receives none of those grants. This rendering alone
does not establish whether that member starts or can transfer bytes on a particular Ascend node;
verify both before relying on cross-vendor sharing.

> **Why a page instead of an admission rule** — a rule becomes API semantics, and an upstream
> improvement on either side would then force an incompatible removal; a documented note only gets
> edited. Treat this section as temporary: it records where the engines and this operator stand, and
> upstream work on either side can obsolete it.

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
uses a 16 MiB default. Budget those 16 MiB the same way — in the request as well as the limit.

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

## What it leaves alone, and one flag that replaces it

**Injection does not touch the engine's own prefix caching, and switching that off is not a
workaround for anything.** vLLM's `enable_prefix_caching` defaults on and reuses blocks in GPU HBM;
SGLang's radix cache does the same.

What the transfer engine registers is that SAME memory, so the network card can map it. Nothing is
copied and nothing is allocated twice, so the two are not competing — disabling prefix caching buys
no memory back and loses the engine's own reuse.

Host DRAM is where an overlap could happen, and neither engine puts KV there unless asked. vLLM's
`--swap-space` is deprecated and ignored, and `kv_offloading_size` defaults off, so on vLLM the only
host DRAM this injection adds is the staging buffer above. SGLang's host tier needs
`--enable-hierarchical-cache`, which this injection renders because the store hangs off that tier —
its [host pool](#sglangs-host-memory-tier) is the one host allocation it adds there.

> **NEVER set `kv_offloading_size` on a container this injects into.** Setting it makes vLLM
> overwrite `kv_connector` with `OffloadingConnector` — unconditionally, with no conflict check,
> because `kv_connector` holds one value. The connector this operator rendered is simply gone.
>
> The container then starts, serves, and uses no shared cache, with nothing in it saying so. The two
> are alternative second-tier caches — one on the node's own DRAM, one on the cluster's pool — so
> choose between them rather than configuring both.

---


**See also** — [KV Cache Pool](../kv-cache/pool.md) (the grant and the reuse domain this page consumes) ·
[KV Cache Backend](../kv-cache/backend.md) (the store the pool draws from) ·
[Admission](../architecture/admission.md) (where this webhook sits relative to the five gates)

**Next** → [Accelerator Requests](../accelerator-requests.md) — the other thing a Pod asks this
operator for.
