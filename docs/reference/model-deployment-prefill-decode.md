# Model Deployment Prefill and Decode Reference

> **Purpose** — what pairs a `prefill` role with a `decode` role: the connector each engine and
> router renders, the router block and its fields, the direct transfer and its transport, roles on
> different hardware, and a role's own address.
> **Audience** users, operators, contributors · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** ~4 min

A deployment declaring a `prefill` and a `decode` role is admitted as one set; the role fields and
that admission are under [Prefill and decode](model-deployment.md#prefill-and-decode). This page is
what the operator renders between the two halves once both run.

## Contents

- [How a pair is wired](#how-a-pair-is-wired)
- [Two ways to configure a pair](#two-ways-to-configure-a-pair)
- [The router block](#the-router-block)
- [The two router fields](#the-two-router-fields)
- [The direct transfer's transport](#the-direct-transfers-transport)
- [Different hardware per role](#different-hardware-per-role)
- [Addressing a role](#addressing-a-role)

## How a pair is wired

Without `spec.router`, `kind` adds the role discriminator to the engine's shared-store connector and
nothing pairs the two roles.

With the managed `llm-d-router`, what a native vLLM role runs depends on whether `spec.kvCache` is
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

On **Ascend** the same two shapes render with that platform's own connector names —
`MooncakeConnectorV1` for the leg and `AscendStoreConnector` for the pool — and the sidecar relays
the handshake in its `nixlv2` mode, the one whose per-request document matches what that connector
waits for.

That is the ONE router combination that renders the leg on Ascend. Behind `vllm-router` an Ascend
pair renders no leg — that router drives its pairs in a handshake vocabulary the Ascend connector
rejects — and behind no router nothing pairs the roles at all. The missing leg is quiet: the
deployment goes Ready, every request is answered normally, and the two roles never exchange a
block — a correctly answering deployment is exactly what makes it hard to see.

Both role containers then mount the host's `/usr/local/Ascend/driver` tree read-only — the leg's
transport builds Device RoCE endpoints and reads each NPU's NIC address through the `hccn_tool`
that ships with the driver, while the engine image carries the driver libraries but not the tool.

Without the mount the leg renders but the engine dies at startup, unable to resolve a device IP; a
node with no driver installation fails the Pod's volume setup instead, naming the path. Reading
`/etc/hccn.conf` would work too, but only on a host that keeps that file, while the tool answers
from the driver on every host that has one. The mount ships with the transfer leg alone — an
Ascend deployment without one carries no host path.

The leg renders both halves' parallel sizes off each role's own books: a managed role's
`extraArgs`, or a take-over role's whole `command` — never both. The one degree vLLM accepts as a
literal environment entry, `VLLM_DP_SIZE`, rides alongside either. A role declaring none renders
`1/1`, exactly as before.

Those books are the whole of what the leg can see. A degree arriving any other way — the image's
own entrypoint (never part of the rendered argv), a config file, a flag inside a `sh -c` string,
`--additional-config`'s JSON value, an environment value pulled from a ConfigMap or Secret — is
invisible to it, and an invisibly widened role keeps its `1/1` half.

The connector's startup assert compares the document against itself, not against the engine, so
an invisibly widened pair fails loudly only when the document's decode degree exceeds its
prefill one; a pair widened symmetrically starts, answers, and pulls a wrong layout — the missing
leg's quiet failure, one level down.

Admission holds the visible side of the contract: a degree the books cannot be read for is
refused, and so is a declared per-member width the role's card request cannot hold — see
[What admission refuses](model-deployment.md#what-admission-refuses).

SGLang renders its halves through the engine's own disaggregation arguments rather than this
connector path; the two engines' legs differ by [their handshake](#the-direct-transfers-transport)
alone.

## Two ways to configure a pair

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
  engine:                                # vllm | sglang; this example walks the vLLM pair
    name: vllm
    version: "0.29.0"
  router:
    name: llm-d-router                 # required to pair the roles; takes either engine
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

**Each replica of a role becomes its own Kueue pod group — one group per replica, declaring a total
of one.** Two roles naming the same `instanceType` are still separate groups, each replica composing
its own Workload. The example above therefore has **four** groups of one replica, admitted as a set.

⛔ **A lost replica is replaced once its slot reads empty, and its siblings keep serving.** A node
drained, a replica preempted for higher-priority work, a kubelet evicting under pressure — each costs
the one replica that left, not its siblings: every replica is its own group with its own Workload, so
a departure touches no other replica's admission.

> **Why the slot is what a replacement waits for** — the group is annotated as serving, so Kueue never
> releases the finalizer it holds on the departed Pod; only that replica's Workload being deleted
> releases it. The replacement is created once no Pod for that replica's ordinal reads on the API
> server — never beside a member still listed, because a second member in a group of one reads as
> excess, and Kueue's answer to the excess is to delete the newcomer.

⭐ **The replica is what bounds the blast radius, and no knob is needed to get that.** A loss or an
edit inside one replica's group does not reach another replica's group — one `prefill` replica turning
over leaves its siblings and all of `decode` serving. The groups are still admitted together — an
`AdmissionCheck` holds them until the whole set has reserved quota, so `prefill` still never starts
without `decode`.

Splitting `instanceType`s now buys different hardware, and nothing else: the isolation it used to
buy, every replica has by default.

**With a shared pool** — the same object plus one block:

```yaml
spec:
  # ... everything above, unchanged ...
  kvCache:
    poolRef:
      name: team-a-dram                  # a KVCachePoolBinding in THIS namespace
```

## The router block

**`spec.router` is what pairs the roles, and `spec.kvCache` is not a substitute for it.** A
deployment declaring `prefill` and `decode` with no router is admitted and renders two roles that
nothing routes between: each gets the shared-store connector with its role discriminator, and no
request is ever split across them. Attaching a pool does not change that.

Conversely a router with no pool is complete. What each shape renders is the table under [How a pair is wired](#how-a-pair-is-wired).

`spec.router.replicas` is optional, and absent means one. More than one trades cache consistency for
availability: a router scoring on a prefix cache holds that state per replica — upstream reports radix
trees that do not synchronize across replicas and a hit rate falling by ten to twenty percent as a
result. Where replicas do exchange events, the exchange improves load estimation without making two
replicas route alike.

`spec.router.extraArgs` takes additional flags for the router process. A flag the operator derives
itself is **refused rather than merged**, so one setting has one source. The owned catalog is keyed by
router, and the refusal names the flag and the router.

For `llm-d-router` it is `--endpoint-selector`, `--endpoint-target-ports`, `--config-file`,
`--secure-serving`, `--grpc-health-port` and `--metrics-endpoint-auth`.

`vllm-router` and `sglang-gateway` take their whole configuration on the command line, so their
catalogs are wider: the two bind addresses and their ports, the four service-discovery flags and
each project's own spelling of the disaggregation switch. Only `vllm-router` names a transfer
connector; the gateway has no such flag, because its transfer backend is the engine's.

A router is also **engine-matched**, and a pair outside this table is refused naming both sides:

| `spec.router.name` | Engines it fronts | Shape it renders | Routing policy |
| --- | --- | --- | --- |
| `llm-d-router` | `vllm`, `sglang` | An endpoint picker behind a proxy, configured by a mounted document | [A fixed scoring profile](model-deployment-routing.md#llm-d-router-takes-no-policy-flag) |
| `vllm-router` | `vllm` | One process, configured entirely by its command line | [`cache_aware` unless `extraArgs` names another](model-deployment-routing.md#switching-to-round-robin) |
| `sglang-gateway` | `sglang` | One process, configured entirely by its command line | [`cache_aware` unless `extraArgs` names another](model-deployment-routing.md#switching-to-round-robin) |

`llm-d-router` takes both engines because upstream carries a handshake connector and a metrics
configuration for each. The other two are each one project's router for that project's own engine,
and admitting a cross pairing would be a claim this repository has not measured.

## The two router fields

`spec.router.requestTimeoutSeconds` is how long the router waits for a reply. **Leaving it out does
not mean one thing across the three.** It renders nothing, so each router keeps its own upstream
default: **one day** under `llm-d-router`, whose proxy carries the timeout, against **half an hour**
under the two configured by their command line — a factor of forty-eight.

Setting it is what makes a declaration survive a change of router. Zero is refused: it would mean
"wait forever" under the proxy and nothing in particular under the other two.

`spec.router.disaggregationThresholdTokens` is how many prompt tokens **not already in a prefix
cache** make a request worth splitting between a prefiller and a decoder; below it the decode replica
serves the whole request itself. Unset renders the value the picker already used.

**Zero disables splitting entirely** rather than meaning "always split" — the decider returns "do not
disaggregate" on a zero threshold before reading anything else — and it is accepted because it is a
value upstream defines. It is **refused** on the other two routers rather than ignored: they have no
per-request decision to threshold, and a field that is legal to write and renders nothing is a shape
this API has rejected before.

The proxy under `llm-d-router` also writes **one access log**: a JSON object per request on the
container's standard output, carrying the response code, the response flags, the response code
details, the duration, the endpoint the picker selected, the method, the path, the request id and the
byte counts. It is not a field — the gap it fills is that nothing is logged at all, so there is no
value to choose. The other two routers log whatever their own flags say.

**Two of those are a boundary rather than a derived value.** The router runs with
`--secure-serving=false` and `--metrics-endpoint-auth=false`, and upstream defaults both to **true**.
The inversion is deliberate: a router manages **east-west** traffic, picking which replica of this
deployment serves a request already inside the cluster. TLS and caller authentication are
**north-south** concerns, owned by the gateway that admits traffic into the cluster.

Where those two flags sit, neither protects anything. `--secure-serving` puts TLS on the endpoint
picker's ext_proc gRPC server, whose only client is the Envoy container **in the same Pod** dialing
`127.0.0.1`. `--metrics-endpoint-auth` guards the picker's own `/metrics`, scraped in-cluster. So
there is no field for either, and neither is reachable through `extraArgs`.

## The direct transfer's transport

The point-to-point leg renders `tcp` unless the deployment says otherwise:

```yaml
spec:
  kvTransfer:
    protocol: rdma                     # unset renders "tcp"
```

The value is a property of **one link**, so it is deployment-wide: a per-role field could only
express two ends naming different protocols for one connection, which fails at transfer time rather
than at admission.

It is **declared, not discovered, and not gated**. The set an engine accepts belongs to the mooncake
build inside the engine's own image — a HIP-compiled build makes `hip` a working transport — so vLLM
gets the value verbatim, and a value the build rejects fails that container at startup. SGLang maps
it: `tcp` renders `--disaggregation-transfer-backend mooncake_tcp`, anything else `mooncake`.
Which value works on which engine is [the transport matrix](engine-versions.md#which-transport-each-engine-can-use).

**`tcp` is enforced, not only requested**: the transfer engine picks its transport from the host,
and with no RDMA device a build with multi-node NVLink installs NVLink between hosts with no NVLink
path. So native vLLM also gets the [defaulted](model-deployment.md#what-the-operator-owns) `MC_FORCE_TCP=1`. Both pins
are process-wide, so neither renders beside a store on another transport, and every client at
its engine's [supported minimum](engine-versions.md) honors them.

It is read on the direct-transfer leg, which every **admitted router-and-engine pair** renders on its
`prefill` and `decode` roles — a prefiller that cannot hand a decoder its blocks is not
disaggregated under any router. On every other shape the field is accepted and renders nothing.

What differs per pair is the handshake, not whether there is a leg: Mooncake's bootstrap server
under native vLLM, SGLang's own registry under SGLang, and on Ascend the decode sidecar's relay —
[the one router combination that renders a leg there](#how-a-pair-is-wired). There the field is
ignored: vllm-ascend hardcodes the protocol to `ascend` (upstream `mooncake_transfer_engine.py`,
verified at v0.23.0 and v0.26.0rc1).

It is also **not** the pool's transport. `KVCacheBackend.spec.transport` feeds the engine's store
client; this leg is engine to engine and never traverses the store, so the two declare separately —
a deployment with no `kvCache` block still has this leg to configure.

Editing it [turns over every role](model-deployment.md#rollout-is-a-rolling-replacement): the value renders into both
ends' arguments, so every role's replicas turn over one at a time. A prefiller and a decoder can
disagree on the protocol until both converge — the same window an `engine.version` edit opens.

## Different hardware per role

A Kueue Workload carries one `queueName`, and that name is the one the role's `instanceType`
publishes as its `status.entrance`. Every replica is its own group regardless, so two roles were never
going to share a Workload — on two `instanceType`s or on one. The set is admitted together by an
admission check rather than by Kueue's intra-group rule.

See [One group per replica](model-deployment.md#one-group-per-replica) for what that costs an edit, and the last row of
[What admission refuses](model-deployment.md#what-admission-refuses) for the one state in which the shape is refused
instead.

**Across manufacturers is the same change, not a second one.** A queue's accelerator quota is
`credits.gpustack.ai/<manufacturer>`, one resource name per manufacturer, and Kueue's own webhook
refuses a second resource group repeating a covered resource within one queue. With a queue per role
there is no second group to repeat anything.

**Two roles on different manufacturers cannot share KV through their pool.** The deployment is
admitted and both halves serve; what fails is the sharing, and it fails silently — reads from the
shared store miss, and nothing on the deployment reports it. Three independent reasons stand
between the halves:

- **This operator's own refusal is the one an administrator meets.** On a pool left at its default
  transport, the renderer [refuses the vLLM-Ascend
  half](kv-cache-injection.md#transport-compatibility-at-binding) — the rule and its
  remediation are stated there. The check is one-sided — it fires for a single-manufacturer Ascend
  deployment just the same — and it is the only one of the three that produces a message;
  following its remediation clears only this refusal; the next two apply regardless.
- **Two upstream walls then apply, read in the source of vLLM `v0.29.0` and vLLM-Ascend `v0.23.0`,
  the [minimum each is supported at](engine-versions.md#the-minimum-per-shape) — a description of
  that pair of releases, not a permanent property of either project.** The operator
  renders no key that lets the Ascend half load from the shared store, and the two engines address
  it with incompatible keys, so every lookup misses and no error is raised.

This limit governs the shared pool alone: [a pair needs no shared pool to hand blocks
over](#how-a-pair-is-wired).

**The direct transfer across manufacturers follows a different rule — not "two manufacturers",
but the router in front.** The leg renders per role, and on Ascend [only one router combination
carries it](#how-a-pair-is-wired).

A mixed pair never forms a transfer either way: the two sides' connectors speak different handshake
vocabularies, so a relayed document from one names nothing the other reads. Two non-Ascend roles
both render it — NVIDIA and AMD, say — and what the engines then do is upstream's answer,
unmeasured here.

[Kueue assigns a ResourceFlavor per
PodSet](../architecture/scheduling-chain.md#stage-4-the-kueue-chain), so a role still takes whatever
its own pool assigns; what selects the hardware is the `instanceType` the role names.

## Addressing a role

Each role gets a `ClusterIP` Service named `<deployment>-<role>`, beside the deployment-wide one, so a
decoder is reachable **as** a decoder. The managed router uses these stable role addresses for the
tokenizer and cache-event contracts; they also remain useful for addressing one half directly while
debugging.

With `spec.router`, the operator renders six objects named `<deployment>-router`: a Deployment,
ConfigMap, Service, ServiceAccount, Role and RoleBinding. Removing `spec.router` prunes all six.
What `status.endpoint` publishes in each shape is under [Status](model-deployment-status.md#status).

---

**See also** — [Model Deployment Reference](model-deployment.md) for the role fields, the owned-key
table and what admission refuses · [Model Deployment Routing
Reference](model-deployment-routing.md) for which replica each router picks · [Engine Versions
Reference](engine-versions.md) for which transport each engine can use on each leg · [Model
Deployment Status Reference](model-deployment-status.md) for what `status.endpoint` publishes.

**Next** → [Model Deployment Routing Reference](model-deployment-routing.md)
