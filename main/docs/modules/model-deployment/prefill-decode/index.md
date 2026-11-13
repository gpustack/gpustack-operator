# Prefill and Decode

A *prefill/decode (PD)* deployment with *Prefill* and *Decode* roles is admitted as one set. Once both roles run, the
operator connects them through a router and a transfer path. [Prefill and
decode](configuration.md#prefill-and-decode) describes the role fields and admission rules.

The recorded runs apply to vLLM `v0.30.0` and SGLang `0.5.22`. Other versions must be re-verified
before they inherit a claim on this page.

![Prefill and decode request and KV transfer flow](/gpustack-operator/main/docs/assets/diagrams/model-deployment-pd.svg)

<!--
flowchart LR
    CLIENT["Client"] -> ROUTER["llm-d-router"] -> SIDECAR["Decode routing sidecar"]
    SIDECAR -> PREFILL["Prefill role"]
    PREFILL -> DECODE["Decode role: direct KV blocks"] -> CLIENT
    SIDECAR -> DECODE
    PREFILL ..> POOL["Optional shared KV pool"]
    DECODE ..> POOL
-->

## Contents

- [Pair connections](#pair-connections)
- [Configuration](#configuration)
- [Router configuration](#router-configuration)
- [Router fields](#router-fields)
- [Counting engine tokens](#counting-engine-tokens)
- [Direct transfer transport](#direct-transfer-transport)
- [Different hardware per role](#different-hardware-per-role)
- [Addressing a role](#addressing-a-role)

## Pair connections

Without *`spec.router`*, *`kind`* adds the role discriminator to the engine's shared-store connector and
nothing pairs the two roles.

For a multi-`Pod` *PD* replica, managed HTTP routing selects its leader (`member-index=0`).
A router policy such as `round_robin` chooses among those leaders; it does not add non-HTTP members.
*External data parallelism (External DP)* inside a *Prefill* or *Decode* role has not been validated;
the shape's own contract, including its Router requirement, is on [External DP](/gpustack-operator/main/docs/modules/model-deployment/external-dp/index.md).
[Issue #754](https://github.com/gpustack/gpustack-operator/issues/754) tracks that deferred combination.

With a managed router, the direct connector depends on the engine, platform and `spec.kvTransfer.backend`. On
vLLM pairs with a shared `spec.kvCache` pool, *Prefill* wraps the direct connector first in `MultiConnector`
alongside the store connector, while *Decode* runs the direct connector alone and keeps pool dtype and
block-size alignment. See [Transfer parameters](/gpustack-operator/main/docs/modules/model-deployment/parameterize/index.md#transfer-parameters) for connector and
environment mappings.

A pair needs no shared pool to hand blocks over. With `llm-d-router`, the decode `Pod` runs a
routing-proxy sidecar. vLLM NIXL and Ascend use `--kv-connector=nixlv2`; vLLM Mooncake uses
`mooncake` and SGLang uses `sglang`. Ascend vLLM pairs also render behind `vllm-router`, which
uses `--kv-connector=nixl` for their relay handshake.

Both role containers then mount the host's `/usr/local/Ascend/driver` tree read-only: the connector's
transport builds Device RoCE endpoints and reads each NPU's NIC address through the `hccn_tool`
that ships with the driver, while the engine image carries the driver libraries but not the tool.

Without the mount the operator renders the connector, but the engine dies at startup, unable to resolve a device IP; a
node with no driver installation fails the `Pod`'s volume setup instead, naming the path. The mount
ships with the transfer path alone; an Ascend deployment without one carries no host path.

The operator reads each role's parallel size from that role's own command line: a managed role's
*`extraArgs`*, or a take-over role's whole *`command`*, never both. The one degree vLLM accepts as a
literal environment entry, `VLLM_DP_SIZE`, rides alongside either. A role declaring none renders
`1/1`, exactly as before.

*`resources.accelerator`* is a scheduling request. It does not become `--tensor-parallel-size`,
`--pipeline-parallel-size` or `--data-parallel-size`. If one engine process must use two cards,
declare the engine's parallel arguments on that role. The operator reads those declarations for the
transfer document and validates them against the role's card request; it does not choose a topology
from the card count.

The command line is the only source the operator reads. A degree arriving any other way is invisible
to it: the image's own entrypoint (never part of the rendered argv), a config file, a flag inside a
`sh -c` string, `--additional-config`'s JSON value, an environment value pulled from a `ConfigMap` or
`Secret`. An invisibly widened role keeps its `1/1` half.

The connector's startup assert compares the document against itself, not against the engine. An
invisibly widened pair therefore fails loudly only when the document's *decode* degree exceeds its
*prefill* one. A pair widened symmetrically starts, answers, and pulls a wrong layout.

Admission holds the visible side of the contract: a degree the command line does not state legibly
is refused, and so is a declared per-member width the role's card request cannot hold; see
[Admission refusals](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#admission-refusals).

SGLang renders its halves through the engine's own disaggregation arguments rather than this
connector path; the two engines' paths differ by [their handshake](#direct-transfer-transport)
alone.

## Configuration

Both are complete objects. The only difference is the *`kvCache`* block, and it is the difference
between "these two roles hand blocks to each other" and "these two roles hand blocks to each other
and share a pool with every other deployment bound to it".

**Without a shared pool**, the ordinary configuration for a pair serving one model:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-pd
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct
  engine:
    name: vLLM  # vLLM or SGLang; vLLM is used here
    image: <a runner image your cluster can pull>  # required runner image
  router:
    name: llm-d-router  # required for pairing; supports vLLM and SGLang
  roles:
    - name: prefill
      kind: Prefill
      replicas: 2
      acceleratorType: gpustack--nvidia-h20-linux-amd64
      resources:
        accelerator: 2
    - name: decode
      kind: Decode
      replicas: 2
      acceleratorType: gpustack--nvidia-h20-linux-amd64
      resources:
        accelerator: 2
```

**Each replica of a role becomes its own Kueue `Pod` group: one group per replica, declaring a total
equal to the role's *`size`*.** Two roles naming the same *`acceleratorType`* are still separate groups,
each replica composing its own `Workload`. The example above therefore has **four** single-`Pod`
groups, admitted as a set.

**A lost replica is replaced once its slot reads empty, and its siblings keep serving.** A node
drained, a replica preempted for higher-priority work, a kubelet evicting under pressure: each costs
the one replica that left, not its siblings. Every replica is its own group with its own `Workload`, so
a departure touches no other replica's admission.

> **Why the slot is what a replacement waits for** — the group is annotated as serving, so Kueue never
> releases the finalizer it holds on the departed `Pod`; only that replica's `Workload` being deleted
> releases it. The replacement is created once no `Pod` for that replica's ordinal reads on the API
> server, never beside a member still listed, because a group over its declared total reads as
> excess, and Kueue's answer to the excess is to delete the newcomer.

**The replica bounds the blast radius.** A loss or an edit inside one replica's group does not reach
another replica's group: one *Prefill* replica turning over leaves its siblings and all of *Decode*
serving. The groups are still admitted together; an `AdmissionCheck` holds them until the whole set
has reserved quota, so *Prefill* still never starts without *Decode*.

Splitting *`acceleratorType`*s buys different hardware; the isolation it used to buy, every replica has
by default.

**With a shared pool**, the same object plus one block. *`spec.kvCache`* is frozen after creation, so declare it in the first apply:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-pd
   namespace: team-a
 spec:
+  kvCache:
+    poolRef:
+      name: team-a-dram                  # a KVCachePoolBinding in THIS namespace
```

## Router configuration

***`spec.router`* is what pairs the roles, and *`spec.kvCache`* is not a substitute for it.** A
deployment declaring *Prefill* and *Decode* with no router is admitted and renders two roles that
nothing routes between: each gets the shared-store connector with its role discriminator, and no
request is ever split across them. Attaching a pool does not change that.

Conversely a router with no pool is complete. What each configuration renders is the table under [Pair connections](#pair-connections).

*`spec.router.replicas`* is optional, and absent means one. More than one trades cache consistency for
availability: a router scoring on a prefix cache holds that state per replica; upstream reports radix
trees that do not synchronize across replicas and a hit rate falling by ten to twenty percent as a
result. Where replicas do exchange events, the exchange improves load estimation without making two
replicas route alike.

*`spec.router.extraArgs`* takes additional flags for the router process. A flag the operator derives
itself is **refused rather than merged**, so one setting has one source. The owned catalog is keyed by
router, and the refusal names the flag and the router.

For `llm-d-router` it is `--endpoint-selector`, `--endpoint-target-ports`, `--config-file`,
`--secure-serving`, `--grpc-health-port` and `--metrics-endpoint-auth`.

`vllm-router` and `sglang-gateway` take their whole configuration on the command line, so their
catalogs are wider: the two bind addresses and their ports, the four service-discovery flags and
each project's own spelling of the disaggregation switch. Only `vllm-router` names a transfer
connector; the gateway has no such flag, because its transfer backend is the engine's.

A router is also **engine-matched**, and a pair outside this table is refused naming both sides:

| *`spec.router.name`* | Engines | Rendered configuration | Routing policy |
| --- | --- | --- | --- |
| `llm-d-router` | `vLLM`, `SGLang` | An endpoint picker behind a proxy, configured by a mounted document | [A fixed scoring profile](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#no-policy-flag) |
| `vllm-router` | `vLLM` | One process, configured entirely by its command line | [`cache_aware` unless `extraArgs` names another](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#switching-to-round-robin) |
| `sglang-gateway` | `SGLang` | One process, configured entirely by its command line | [`cache_aware` unless `extraArgs` names another](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#switching-to-round-robin) |

`llm-d-router` takes both engines because upstream carries a handshake connector and a metrics
configuration for each. The other two are each one project's router for that project's own engine,
and admitting a cross pairing would be a claim this repository has not measured.

## Router fields

*`spec.router.requestTimeoutSeconds`* is how long the router waits for a reply. **Leaving it out does
not mean one thing across the three.** It renders nothing, so each router keeps its own upstream
default: **one day** under `llm-d-router`, whose proxy carries the timeout, against **half an hour**
under the two configured by their command line, a factor of forty-eight.

Setting it is what makes a declaration survive a change of router. Zero is refused: it would mean
"wait forever" under the proxy and nothing in particular under the other two.

*`spec.router.disaggregationThresholdTokens`* is how many prompt tokens **not already in a prefix
cache** make a request worth splitting between a prefiller and a decoder; below it the *decode* replica
serves the whole request itself. Unset renders the value the picker already used.

**Zero disables splitting entirely** rather than meaning "always split" (the decider returns "do not
disaggregate" on a zero threshold before reading anything else), and it is accepted because it is a
value upstream defines. It is **refused** on the other routers rather than ignored: they have no
per-request decision to threshold, and a field that is legal to write and renders nothing is a configuration
this API has rejected before.

The proxy under `llm-d-router` also writes **one access log**: a JSON object per request on the
container's standard output, carrying the response code, the response flags, the response code
details, the duration, the endpoint the picker selected, the method, the path, the request id and the
byte counts. It is not a field: the gap it fills is that nothing is logged at all, so there is no
value to choose. The other routers log whatever their own flags say.

**Two flags are fixed.** The router runs with
`--secure-serving=false` and `--metrics-endpoint-auth=false`, where upstream defaults both to
**true**. The inversion is deliberate: a router manages **east-west** traffic, picking which replica
of this deployment serves a request already inside the cluster, while TLS and caller authentication
are **north-south** concerns owned by the gateway that admits traffic into the cluster.

Where those two flags sit, neither protects anything. `--secure-serving` puts TLS on the endpoint
picker's gRPC server, whose only client is the proxy container **in the same `Pod`** dialing
`127.0.0.1`; `--metrics-endpoint-auth` guards the picker's own `/metrics`, scraped in-cluster. So
there is no field for either, and neither is reachable through *`extraArgs`*.

## Counting engine tokens

**A deployment that declares no *Encode* role needs `--enable-scale-out` on its vLLM roles,
behind `llm-d-router`.** The router counts prompt tokens by calling the engine: it POSTs
`/v1/chat/completions/render`. vLLM serves that endpoint only under `--enable-scale-out`. The
operator does not render the flag, and admission does not check for it: set the flag in each
role's *`extraArgs`*.

What the declared configuration decides:

| Declared roles | Token counting | Flag needed | Rendered flag |
| --- | --- | --- | --- |
| *Encode*, *Prefill*, *Decode* | the router's built-in estimator; no HTTP call | no | no flag, which is correct |
| *Prefill*, *Decode* | a call to the engine's render endpoint | yes | nothing |
| a single *Server* role | a call to the engine's render endpoint | yes | nothing |

**Without the flag the failure is silent.** Measured on vLLM-Ascend `0.30.0rc1`: the render call
answers `404`, no token count comes back, and the prefix decider routes every request to the
decoder alone. Requests return `200`, the *prefill* stage receives nothing, no KV moves, and no
error is reported. The render calls do appear in the router's access log, so the stage counts
name the gap ([what the counts showed](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md#stage-execution-verification)).

**The floor is vLLM `0.30.0` and vllm-ascend `0.30.0rc1`.** Earlier releases lack
`--enable-scale-out`, and at vLLM-Ascend `0.23.0` the render route itself is absent.

Setting the flag registers the render, derender and generate endpoints; it changes nothing else
about the engine. The flag and the endpoint are vLLM's. Other router paths are summarized under
[Version and configuration limits](#version-and-configuration-limits).

For a vLLM *Prefill* and *Decode* deployment behind `llm-d-router`, add the flag to both roles.
This diff shows the fields to add to the [complete deployment](#configuration):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-pd
   namespace: team-a
 spec:
   roles:
     - name: prefill
       kind: Prefill
+      extraArgs:
+        - --enable-scale-out
     - name: decode
       kind: Decode
+      extraArgs:
+        - --enable-scale-out
```

Older vLLM images below [the documented floor](#counting-engine-tokens) predate the
flag, so adding it to them can make the engine reject its command line.
The operator does not choose between these cases from the image tag.

### Version and configuration limits

For each configuration, name an image that meets the documented floor and add the engine-specific arguments
it needs.

The `llm-d-router` plus vLLM requirement and supported floors are documented under
[Counting engine tokens](#counting-engine-tokens). The other router paths are:

| Router and engine | `--enable-scale-out` status | Configuration path |
|---|---|---|
| `vllm-router` with vLLM | No equivalent requirement is established here | The router uses its own *PD* mode and endpoint selectors; *encode/prefill/decode (EPD)* uses operator-rendered [`--epd-config`](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#encode-stage-routing) |
| `sglang-gateway` with SGLang | Not applicable | SGLang uses its own disaggregation arguments; `--enable-scale-out` is a vLLM flag |
| `llm-d-router` with SGLang | No vLLM flag applies; token path is unmeasured | SGLang uses its own disaggregation arguments; do not infer this path from the vLLM row |

[Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md#minimum-versions) carries the same floors as a
requirement on the vLLM and vLLM-Ascend rows.

## Direct transfer transport

The deployment-wide `spec.kvTransfer.backend` selects `Mooncake`, `NIXL`, or `HIXL`.
When omitted, it defaults to `Mooncake` on non-Ascend pools and `HIXL` on Ascend pools.
For example, select NIXL on an existing deployment with:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-pd
 spec:
+  kvTransfer:
+    backend: NIXL
```

Admission refuses `HIXL` on non-Ascend or CPU-only roles, `NIXL` on Ascend `AcceleratorType`s, and mixed
Ascend/non-Ascend (including CPU-only) prefill/decode pairs. If the manufacturer is not yet observed, the renderer
can refuse an incompatible combination after it resolves.

Mooncake derives `ascend` on Ascend, `efa` when both roles request `resources.interface > 0`
under EFA, `hip` on AMD ROCm, `rdma` when both roles request `resources.interface > 0`, and
`tcp` otherwise. NIXL selects `LIBFABRIC` when `aws-efa-enabled` is true or a bound KV cache
pool uses EFA, and `UCX` otherwise. HIXL uses the Ascend driver mount.
See [Transfer parameters](/gpustack-operator/main/docs/modules/model-deployment/parameterize/index.md#transfer-parameters) for the injected settings.

For AWS EFA, supply an EFA-enabled runner through `spec.engine.image` or `roles[].image`, and
an EFA-enabled `model-transfer` image through Helm `modelTransfer.images.nvidia` for remote
weight delivery. The default image builds do not include the EFA provider.

The engine image must support its chosen transport. [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md)
lists existing compatibility evidence; these rendered combinations require their own validation.
[Parameters](/gpustack-operator/main/docs/modules/model-deployment/parameterize/index.md) lists synthesized parameters and defaults.
Changing transfer settings rolls the roles; transfers can fail until both ends converge.

## Different hardware per role

A Kueue `Workload` carries one `queueName`, and that name is the one the role's *`acceleratorType`*
publishes as its *`status.entrance`*. Every replica is its own group regardless, so two roles were
never going to share a `Workload`, on two *`acceleratorType`*s or on one. The set is admitted together by an
admission check rather than by Kueue's intra-group rule.

See [One group per replica](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#one-group-per-replica) for what that costs an edit, and
[Authoring the InstanceType](/gpustack-operator/main/docs/reference/settings/index.md#authoring-the-instancetype) for which
queues carry the check.

**Adding a second manufacturer uses the same mechanism.** A queue's accelerator quota is
`credits.gpustack.ai/<manufacturer>`, one resource name per manufacturer, and Kueue's own webhook
refuses a second resource group repeating a covered resource within one queue. Roles on the same pool share its queue; different manufacturers use different pool queues.

A mixed Ascend/non-Ascend pair is refused once both manufacturers are observed. Two non-Ascend
roles can name different manufacturers, but their direct transfer interoperability is unmeasured
here. A pair needs no shared pool for [direct transfer](#pair-connections); a vLLM-Ascend role
using a pool has separate [store transport requirements](/gpustack-operator/main/docs/modules/kv-cache/injection/index.md#binding-transport-support).

[Kueue assigns a ResourceFlavor per
PodSet](../devices/scheduling.md#stage-4-kueue-chain), so a role still takes whatever
its own pool assigns; what selects the hardware is the *`acceleratorType`* the role names.

## Addressing a role

Each role gets a `ClusterIP` `Service` named `<deployment>-<role>`, beside the deployment-wide one, so a
decoder is reachable **as** a decoder. The managed router uses these stable role addresses for the
tokenizer and cache-event contracts; they also remain useful for addressing one half directly while
debugging.

With *`spec.router`*, the operator renders resources named `<deployment>-router`: a `Deployment`,
`ConfigMap`, `Service`, `ServiceAccount`, `Role` and `RoleBinding`. Removing *`spec.router`* prunes these resources.
What *`status.endpoint`* publishes in each configuration is under [Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md#status).

---

**See also** — [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) for the role fields, the owned-key
table and what admission refuses · [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) for which replica each
router picks · [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md) for which transport each engine can use on
each path · [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) for what *`status.endpoint`* publishes ·
[Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) for the measured
comparison of a disaggregated pair with an aggregated group on equal GPUs.

**Next** → [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md)
