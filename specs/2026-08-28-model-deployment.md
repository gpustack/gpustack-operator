# Spec: ModelDeployment — One Role, Many Replicas, One Shared KV Cache

Status: Building
Type: Feature

## Summary

A `ModelDeployment` is N replicas of one inference-engine role, attached to a KV cache pool through a
namespaced `KVCachePoolBinding`, all sharing the one reuse domain that Binding declares — so the
replicas hit each other's cached prefixes instead of each re-computing the same prefill. It is a new
namespaced CRD in `worker.gpustack.ai/v1alpha1`, and it **renders Pods directly**: the existing
admission chain keys on Pods, so rendering Pods reuses all five gates with no new integration point,
while the `Instance` CRD — one Pod, hard, and immutable after creation — could express neither many
replicas nor the one-replica-many-Pods shape the next spec needs.

The workload does **not** declare its reuse domain; it inherits the Binding's. Two `ModelDeployment`s
referencing the same Binding share KV, two referencing different Bindings do not, and name matching
between workloads disappears along with a whole class of typo. The engine command line is the
fastest-moving thing in the design, so it has a three-tier escape hatch — append, overlay, take over
— guarded by two webhook rules that refuse a silent merge on a key the operator owns and keep the
scheduling scalars out of the template. And because a flag being accepted proves nothing about it
being in effect, `CacheAttached` is judged on the cache client's own health server answering on each
replica — never on the operator having rendered the flag, and never on a figure the whole reuse
domain shares.

This spec delivers the **single-role** form. `roles` exists as a list and is validated as length 1;
multiple roles, P/D disaggregation and cross-role atomic admission are the next spec's.

## Motivation

Today the only way to run an inference engine through this project's scheduling chain is a Pod the
user writes by hand, or a GPUStack `Instance`. Both give exactly one replica. Neither knows anything
about a KV cache pool, so every replica of one model pays full prefill for a prefix its sibling
computed a second ago — the pool exists (an earlier spec), and nothing routes a workload into it.

`Instance` cannot be stretched to cover this, and the reasons are properties of the type rather than
matters of taste:

- `pkg/worker/controllers/worker/instance.go` constructs exactly **one** `core.Pod` per `Instance`
  (`convertPodFromInstance`), so "one replica = leader + workers across nodes" is inexpressible
  through it — and that shape is what the next spec requires.
- `InstanceSpec.Type` is *"the name of InstanceType that provisions corresponding resources"* and is
  **`Immutable after creation`**; the whole inline `InstanceTemplate` is `Immutable after creation`
  too (`api/worker/v1alpha1/instance.go`). A rolling update through `Instance`s degenerates into
  recreate-everything.
- **There is no `Replicas` field anywhere in `api/worker/v1alpha1/`.**

### Goals

- **G1 (primary)** Several replicas of one model **share one KV cache**, and the measured hit rate
  exceeds what a single replica achieves on the same request stream. This is the claim the whole
  spec exists to make demonstrable; the numbers are recorded (F10, Test Plan).
- **G2** The reuse domain is an **admin-controlled** property a workload inherits, never one it
  names — so the namespace quota ceiling cannot be escaped by minting tenants (F3).
- **G3** A `ModelDeployment` enters the existing five-gate admission chain **as Pods**, with no new
  gate, no new controller in the chain, and no change to the four-view status (F1).
- **G4** The engine command line has an **escape hatch that the reconcile loop respects**: a user's
  override is not silently overwritten on the next pass, and an override that would produce an
  undiagnosable duplicate is refused at admission rather than merged (F5).
- **G5** `CacheAttached` reports **observed behaviour**. A rendered flag, and a log line echoing a
  flag back, are both worth nothing (F8).
- **G6** An operator can see **which reuse domain** a deployment attached to, and its `blockSize` and
  `dtype`, by reading the `ModelDeployment` alone — without also reading the Binding (F2, F7).
- **G7** The CR stays **thin enough to be replaceable**. Its whole justification is one property
  upstream cannot give; everything else is upstream's and stays upstream's (Non-Goals).

### Non-Goals

- **`ModelDeployment` is not a general-purpose serving CR.** Its entire justification is one thing
  upstream cannot do: **cross-role atomic admission through this project's Kueue chain.** Role
  modelling, multi-Pod replicas, rolling update and routing are all done better upstream, and this
  programme reuses upstream for them.
  - **LWS / `DisaggregatedSet`** has the best role and multi-Pod replica modelling, but each LWS
    replica group becomes its own Kueue Workload, so it **gives up cross-role atomic admission** —
    the one property P/D cannot afford to lose.
  - **Dynamo** has the most complete router and cost function, and this programme copies its
    conclusions; but it is a whole platform with its own operator and **no Kueue integration at
    all**.
  - **llm-d** has the right routing architecture — a single EPP covering all roles — and this
    programme **reuses its router** rather than replacing it.

  There is an open upstream effort to give `DisaggregatedSet` Kueue integration. **If it lands, this
  CR's reason to exist goes away**, so every feature below is scoped to keep that retirement cheap:
  no state lives here that the Binding, the pool or upstream could not hold.
- **A workload may not declare its own reuse domain.** Because `tenant_id` *is* the reuse domain,
  every distinct domain is a tenant with its own quota ledger — so a workload free to name arbitrary
  domains could **mint unlimited tenants in its namespace and escape the namespace ceiling
  entirely**, turning `quotaCeiling` into decoration. Domain naming therefore sits on the admin's
  object, the `KVCachePoolBinding`.
- **Multiple roles.** No `prefill`/`decode` split. The `roles` list exists but is validated as
  length 1 (F6).
- **P/D disaggregation and cross-role atomic admission.** A later spec. In particular this spec
  creates **no Kueue pod group**: N replicas are N independent Workloads, which is correct for one
  role whose replicas are independently useful, and is exactly what the next spec replaces.
- **Router orchestration, prefix-affinity routing, KV-event consumption.** A later spec. The
  `Service` this spec renders is a plain round-robin front door (F9).
- **Anything that manages the backend or the pool.** Those are earlier specs; this one only
  *references* a Binding.
- **Model weight provisioning.** `spec.model` names the model; it does not download, cache or place
  weights. They arrive through the role template's volumes or through the engine's own hub client.
  Adding a weight-provisioning block here is the first step towards the general-purpose serving CR
  this is not.
- **An intermediate `InstanceGroup` layer.** A `ModelDeployment → InstanceGroup → Instance → Pod`
  chain does not solve the two things that matter and adds a correctness risk; see Alternatives. An
  `InstanceGroup` may be worth having for its own sake — there is no `Replicas` anywhere in the API
  group today — but it must not be a substrate for this CR.
- **Rewriting the existing `Instance` CRD.** `ModelDeployment` is additive. `Instance` keeps its
  one-Pod, immutable-spec contract.
- **Throughput and latency claims.** Only functional correctness is in scope. Throughput belongs to
  the P/D specs and needs an RDMA window this spec does not have.

## Proposal

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment                      # namespaced
metadata: { name: qwen-72b, namespace: team-a }
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct        # the identifier the engine serves
  engine: vllm                             # vllm | sglang | vllm-ascend
  kvCache:
    poolRef: { name: shared-kv }           # a KVCachePoolBinding in THIS namespace — not a bare URL,
                                           # and never a cross-namespace reference. The reuse DOMAIN
                                           # comes from the Binding; this workload declares none.
    connector: auto                        # the operator synthesizes the transfer config from
                                           # backend type x engine
  roles:                                   # webhook: exactly 1 entry in this spec
  - name: server
    replicas: 4
    instanceType: h20-8x                   # reuses the existing pool -> ClusterQueue mapping
    extraArgs: []                          # tier 1, see F5
    env: []                                # tier 1
    template: { ... }                      # an InstanceTemplate overlay, tier 2/3
status:
  phase: Ready
  phaseMessage: ""
  endpoint: "http://qwen-72b.team-a.svc:8000"
  roles:
  - name: server
    desired: 4
    ready: 4
    unmanaged: false
  kvCache:
    binding: shared-kv
    pool: shared-kv-pool
    domain: { name: team-a-shared, blockSize: 256, dtype: bfloat16 }
  conditions:
  - type: DomainRegistered
  - type: QuotaReserved
  - type: CacheAttached                    # does the injection ACTUALLY work
```

### User Stories

#### Story 1

As a platform user, I want to declare four replicas of one model and have them all attach to the
same KV cache, so that a prefix one replica has already computed is served from the pool for the
other three instead of being re-prefilled.

#### Story 2

As a platform user, I want to see from `kubectl get modeldeployment -o yaml` alone which reuse
domain my deployment attached to and what its `blockSize` and `dtype` are, so that I can tell a
cache-sharing misconfiguration from a cache that is simply cold, without reading a second object.

#### Story 3

As a platform user chasing a vLLM release whose `--kv-transfer-config` syntax has moved, I want to
append or replace engine arguments and have the reconcile loop leave them alone, so that I am not
driven to `kubectl patch` the rendered Pods only to watch the operator overwrite me on the next
pass.

#### Story 4

As a cluster admin, I want a namespace's reuse boundaries to be something I create — a Binding —
rather than something a workload can name, so that a user cannot mint a fresh tenant per deployment
and walk past the namespace's quota ceiling.

#### Story 5

As an operator, I want `CacheAttached` to be `False` when the connector is not actually in effect,
so that a deployment quietly running with no cache at all is visible rather than indistinguishable
from a healthy one.

#### Story 6

As an operator, I want a deployment that runs two roles to be **refused with a message naming the
spec that will allow it**, so that I do not spend an afternoon deciding whether the field is broken
or the feature is unbuilt.

### Core Features & Acceptance Criteria

#### F1 — The CR renders Pods directly, and enters the existing chain as Pods

`docs/architecture.md`'s life-of-a-request table states the submitted object is *"a Pod (or a
GPUStack `Instance`, **which renders one**)"*, carrying the pool's entrance label
`kueue.x-k8s.io/queue-name: gpustack-fnv64-…` plus its accelerator requests. **The admission chain
keys on Pods. A plain Pod is a first-class citizen of it; `Instance` is sugar that renders one.**

`ModelDeployment` therefore renders Pods. Four reasons, all verified in this repository:

1. **The five controllers and the admission gates already act on Pods**, so rendering Pods reuses
   the entire existing chain with no new integration point — no new gate, no new AdmissionCheck, no
   change to the `Devices` ledger or the four-view status.
2. **`Instance` renders exactly one Pod**, so "one replica = several Pods" would be *inexpressible*
   through it. That shape is required by the next spec (tensor parallelism across nodes), so
   choosing `Instance` now creates a rewrite later.
3. **`Instance.Spec` is immutable after creation**, so a rolling update through `Instance`s
   degenerates into recreate-everything.
4. **Kueue pod-group membership is expressed as labels on the Pods.** Routing them through
   `Instance` would require a passthrough field that exists only to be passed through.

Acceptance:

- A `ModelDeployment` with `replicas: 4` and one role produces four Pods, named
  `<deployment>-<role>-<ordinal>`, each controlled by the `ModelDeployment` through an owner
  reference, each carrying `kueue.x-k8s.io/queue-name` =
  `nodefeature.FormatLocalQueueName(roles[0].instanceType)` and the role's resource requests.
- `status.roles[0].ready` reaches `4` and `status.endpoint` serves inference.
- Reconciliation is level-based and idempotent: deleting one Pod by hand converges back to four;
  a second reconcile over an unchanged spec issues no writes.
- **No `Instance` object is created**, and no field is added to `Instance` or `InstanceSpec`.

**The cost, and its mitigation.** Rendering Pods directly means this spec does not inherit what
`instance.go` already does — volume wiring, port and service exposure, SSH key handling, status
aggregation, allocation reporting. The mitigation is to **reuse `InstanceTemplate` as the per-role
template type** rather than invent a parallel one: it already carries `Image`, `ImagePullPolicy`,
`Command`, `Privileged`, `Ports`, `Env`, `Resources` (including `Accelerator`,
`AcceleratorSlicedMemoryPercentage`, `AcceleratorSlicedCoresPercentage`,
`AcceleratorPartitionedProfile`), `VolumeMount`, `ImagePullSecret` and `AdditionalVolumes`. One
shape for users, one validation surface for us.

**The template type is shared; its immutability is not.** `InstanceSpec` marks the inline
`InstanceTemplate` `Immutable after creation`, but that is a rule the `Instance` webhook enforces on
`InstanceSpec` — not a property of the template type. `ModelDeployment` reuses the type and does
**not** inherit the rule: its template is mutable, which is what makes a rollout possible at all.

#### F2 — `poolRef` is a namespaced Binding: never a pool, never a URL, never cross-namespace

`spec.kvCache.poolRef` is a `core.LocalObjectReference` naming a `KVCachePoolBinding` **in this
namespace**. The Binding is the authorization point: an admin creating one in a namespace is what
grants that namespace access to the pool. A workload naming the cluster-scoped pool directly, or a
bare endpoint URL, would bypass authorization entirely — so neither is expressible. The field is a
`LocalObjectReference` precisely because that type carries no namespace, which makes the
cross-namespace case unrepresentable rather than merely rejected.

Acceptance:

- A `poolRef` naming a `KVCachePoolBinding` that does not exist in this namespace is **rejected at
  admission** with an actionable message naming the namespace and the missing Binding.
- A `poolRef` that tries to reach another namespace is rejected. Because `LocalObjectReference` has
  no namespace field, the attempt arrives as an unknown field and the CRD's structural schema
  prunes or rejects it; the test asserts the observed behaviour either way rather than assuming
  which.
- A Binding that exists but is not yet Ready leaves the deployment in `Starting` with
  `DomainRegistered=False`, reason `BindingNotReady` — not rejected, because the Binding becoming
  Ready is a matter of time rather than a matter of the spec being wrong.
- Deleting the Binding out from under a running deployment sets `DomainRegistered=False` and leaves
  the Pods running. The operator does not tear down a serving deployment because an admin object
  vanished; the condition is the signal.

#### F3 — The reuse domain is inherited, never declared

The domain — `name`, `blockSize`, `dtype` — is a required, immutable block on the
`KVCachePoolBinding`, the object an **admin** controls. `ModelDeploymentSpec` has **no domain
field**, and this is a security property rather than tidiness: because `tenant_id` *is* the reuse
domain, every distinct domain is a tenant with its own quota ledger, so a workload that could name
an arbitrary domain could **mint unlimited tenants in its namespace and escape the namespace quota
ceiling** — each new domain drawing its own quota entry instead of drawing down a shared one.

Resulting semantics, stated plainly:

- Two `ModelDeployment`s referencing the **same** Binding share KV.
- Two referencing **different** Bindings do not.
- **Name matching between workloads disappears**, and with it a whole class of typo.
- A namespace needing two reuse boundaries creates **two Bindings** pointing at the same pool — the
  same shape as a namespace having several Kueue `LocalQueue`s.

The domain is deliberately **not** its own CRD either: its identity is immutable and bound to the
Binding, so a separate CRD would add a referential-integrity problem and buy nothing. The pool's
status provides the observability.

Acceptance:

- A `ModelDeployment` that tries to declare its own domain is rejected. The field does not exist, so
  a user supplying one supplies an unknown field; the test asserts the observed schema behaviour
  rather than assuming pruning or rejection.
- `status.kvCache.domain` echoes the Binding's `name`, `blockSize` and `dtype`, so an operator reads
  the attached domain off this object alone (G6).
- Two deployments on one Binding, and two on two Bindings, behave as stated above — asserted end to
  end (Test Plan).

#### F4 — `connector: auto` — the operator synthesizes the transfer config

`spec.kvCache.connector` is an enum whose only value today is `auto`, defaulted. An enum of one is
deliberate: it reserves the discriminator, so naming a specific connector later is an enum widening
rather than a new field and a new shape. There is no `none`, because "the operator synthesizes
nothing" is already reachable — and reachable *with* its consequences attached — through the
take-over tier (F5).

**The connector choice lives on the workload, not on the pool.** Three reasons: a cache object being
unaware of engine roles is a clean factoring already proven upstream (LMCache's cache CR serves
prefiller and decoder from one instance, selecting config by a per-Pod role annotation); the object
replica count is a per-`Put` caller argument anyway; and upstream projects give three different
answers (routing plane / workload / cache CR), of which **workload is the most defensible, because
the connector is tightly coupled to the *engine version*, and the engine version belongs to the
workload.**

Under `auto`, and per engine, the operator renders:

| Engine | Argument the operator adds | Where `tenant_id` goes |
|---|---|---|
| `vllm` | `--kv-transfer-config` enabling `MooncakeStoreConnector` | `MOONCAKE_TENANT_ID` |
| `vllm-ascend` | `--kv-transfer-config` selecting `MultiConnector` | `MOONCAKE_TENANT_ID` |
| `sglang` | `--hicache-storage-backend` + `--hicache-storage-backend-extra-config` | `MOONCAKE_TENANT_ID` |

**The client is configured entirely through environment, and there is no ConfigMap.** The three
engines differ in the argument that selects the connector; they do not differ in how the client
underneath is configured, so the environment block below is the same for all three:

| Variable | Source | Note |
|---|---|---|
| `MOONCAKE_MASTER` | the pool's `status.clientEndpoint` | its presence is what selects the environment branch at all |
| `MOONCAKE_TE_META_DATA_SERVER` | the pool | |
| `MOONCAKE_PROTOCOL` | the pool | |
| `MOONCAKE_DEVICE` | the pool | the client's *environment* spelling of the RDMA device list |
| `MOONCAKE_TENANT_ID` | the Binding's domain `name` | the reuse domain |
| `MOONCAKE_LOCAL_HOSTNAME` | **`fieldRef: status.podIP`**, per replica | see below |
| `MOONCAKE_ENABLE_CLIENT_HTTP_SERVER` | `true`, set by the operator | the observation surface F8 judges on |
| `MOONCAKE_CLIENT_HTTP_PORT` | `9300`, the client's own default | |
| `MC_TE_METRIC` | `1`, **defaulted** not owned (F5, F10) | |

**Why environment rather than a mounted JSON, stated from the shipped client.** In
`mooncake-wheel/mooncake/mooncake_config.py` (measured at `v0.3.13-rc1`):

- `local_hostname` is listed in `_REQUIRED_NON_EMPTY_FIELDS`, and `MooncakeConfig.from_file()` raises
  `Missing required config field: local_hostname` when the key is absent. **It is not defaulted from
  the host's own address.** A deployment-wide JSON therefore cannot omit the key — and cannot supply
  it either, because one file is one string for every replica, while the value is the address peers
  reach *this* replica's segment on.
- `MooncakeConfig.load_from_env()` reads `MOONCAKE_LOCAL_HOSTNAME`, and its own default is the
  literal `"localhost"` — also not the host address, so the environment branch does not rescue an
  unset value either. The operator therefore always sets it, from the downward API.
- The two branches are **exclusive, not layered**: `load_from_env()` returns
  `MooncakeConfig.from_file(config_file_path)` as soon as `MOONCAKE_CONFIG_PATH` is set, and reads no
  environment variable at all on that path. There is no hybrid where a JSON carries the
  deployment-wide values and an environment variable carries the per-replica one.

The downward API supplies the one per-replica value with no ConfigMap, no per-replica object, and no
entrypoint prelude — the last of which would have collided with the take-over tier, since a user who
owns the whole argv owns the entrypoint too.

⚠️ **Three surfaces spell the RDMA device list three different ways** — `setup()`'s positional
parameter is `rdma_devices`, the JSON key is `device_name`, and the environment variable is
`MOONCAKE_DEVICE`. This spec renders the environment, so `MOONCAKE_DEVICE` is the one that matters
here; the other two are recorded because the same fact under three names is how a reader concludes
one of them is a typo.

⚠️ **The environment branch is Mooncake's own config loader.** Whether each engine's connector
reaches the client through that loader — rather than through a config reader of its own that insists
on `MOONCAKE_CONFIG_PATH` — is per-engine, and T6 confirms it per engine before the golden fixtures
freeze. An engine that turns out to need the JSON gets the JSON *and* a per-replica object, which is
why the fixtures are frozen after the check rather than before.

**What the operator does not choose.** `global_segment_size` and `local_buffer_size` size a
replica's own contribution to the pool. The operator leaves them at the client's defaults and does
not invent values for them: a wrong operator-chosen value is a silent capacity error, and the append
tier exists exactly so a user can set a fast-moving client knob without a spec change. They are
therefore **not** owned keys (F5).

**The per-vendor client is an image concern, not a pool concern.** Per-vendor client wheels exist and
are versioned in lockstep (all at 0.3.13 when measured): `mooncake-transfer-engine` (base, a CUDA 12
build), `-cuda13`, `-npu` (Ascend — the name is `-npu`, **not** `-ascend`), `-rocm` (x86_64 only) and
`-musa` (one file, cp310 only). **The per-vendor artifact is the client, and it belongs inside the
per-vendor engine image**; this repo's engine images are already split cuda / rocm / CANN. **The pool
itself is vendor-neutral, and this spec designs no per-hardware pool.** A deployment whose image
lacks the matching wheel is a `CacheAttached=False` case (F8), not an admission-time one — nothing
at admission can see inside an image.

Acceptance:

- For each of the three engines, the rendered Pod carries exactly the arguments and environment in
  the tables above, asserted against a golden fixture per engine.
- `MOONCAKE_TENANT_ID` equals the Binding's domain name, and `MOONCAKE_LOCAL_HOSTNAME` is a
  `fieldRef` to `status.podIP` rather than a literal — asserted on the rendered `EnvVar`, because a
  literal that happens to be correct for one replica is wrong for the other three.
- **No ConfigMap is created**, and no volume is mounted for client configuration.
- Changing the Binding's pool endpoint re-renders the environment; the replicas restart to pick it up
  under F10's recreate policy rather than silently.

#### F5 — Three-tier override of the engine command line, and two webhook rules

The engine command line is the fastest-moving thing in this design — vLLM's `--kv-transfer-config`
syntax, SGLang's `--hicache-storage-backend-extra-config`, Ascend's `MultiConnector`. We cannot
enumerate it. **Without an escape hatch users will `kubectl patch` the workload we render, and the
reconcile loop will silently overwrite them** — a failure mode observed on a real upstream inference
operator whose enums were too narrow. The repo already has the precedent: `InstanceTemplate.Command
[]string` exists today.

| Tier | Field | Semantics | Status |
|---|---|---|---|
| append | `roles[].extraArgs`, `roles[].env` | appended **after** the operator-synthesized arguments; keys the operator owns (`--kv-transfer-config`, the pool endpoint, role identity) stay the operator's | normal |
| overlay | `roles[].template` (an `InstanceTemplate` overlay) | the operator renders first, then merges the user's overlay on top | normal |
| take over | `roles[].template.command` as a full replacement | the user owns the command line; the operator synthesizes **no** engine arguments at all | the role is marked `Unmanaged` and `CacheAttached` goes to `Unknown` |

**Rule 1 — an owned key in the append tier is rejected, never merged.** A silent merge produces "two
`--kv-transfer-config`, which one wins" — an undiagnosable state. The rejection names the key and the
tier that owns it.

The rule needs a precise notion of "owned", because the operator sets more keys than it owns:

- **Owned** — the operator will refuse a user-supplied duplicate, because duplication is
  undiagnosable. The catalogue is a data table keyed by (engine, key): the `--kv-transfer-config`
  family, `--hicache-storage-backend` and `--hicache-storage-backend-extra-config`; and, for every
  engine, the client environment the operator renders — `MOONCAKE_MASTER`,
  `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_TENANT_ID`,
  `MOONCAKE_LOCAL_HOSTNAME`, `MOONCAKE_ENABLE_CLIENT_HTTP_SERVER`, `MOONCAKE_CLIENT_HTTP_PORT` and
  `MOONCAKE_CONFIG_PATH`. Adding an engine adds rows; the webhook reads the table rather than a
  hard-coded list, so the rule and the renderer can never disagree about what is owned.

  Two of those are owned for a reason worth stating, because neither is a duplicate of anything the
  operator renders:

  - **`MOONCAKE_CONFIG_PATH` is owned although the operator never sets it.** Setting it flips the
    client from the environment branch to `from_file` and discards *every* variable above in
    silence — the deployment then runs against whatever that file says, including another tenant's
    domain. It is refused rather than rendered: the one key whose ownership is about what it
    destroys rather than what it duplicates.
  - **`MOONCAKE_ENABLE_CLIENT_HTTP_SERVER` and `MOONCAKE_CLIENT_HTTP_PORT` are owned** because F8's
    primary observation is read from that server. A user free to turn it off would move
    `CacheAttached` to `Unknown` permanently with nothing saying why; refusing the key says why at
    the moment of the attempt.
- **Defaulted** — the operator supplies a value only where the user supplied none, because
  duplication is harmless and last-wins is well defined. `MC_TE_METRIC` is the case that matters
  (F10): the operator sets it, and a user may turn it off. `MC_STORE_CLIENT_METRIC` is defaulted too
  — the operator leaves it alone, and a user setting it to `0` makes `/metrics` answer `503` while
  `/health` keeps working, which is why F8 reads `/health`.

**Rule 2 — the scheduling scalars must not be inferrable from the template.** `replicas`,
`instanceType` and (in the next spec) `parallelism` are inputs to admission and scheduling — Kueue
PodSet counts, flavor selection, topology domains. They stay **structured fields**. The template may
override container content and **never** the replica count or the resource requests; a template
carrying `resources` is rejected, naming `roles[].instanceType` as where that decision lives.
Otherwise **the admission feasibility check reads a ledger that does not match reality**.

**Arguments fold into `Command`; there is no `Args`.** `InstanceTemplate` has `Command []string` and
no `Args`, and this spec does not add one. Adding `args` would create a **second append tier beside
`extraArgs` with no defined precedence** — precisely the "which one wins" state Rule 1 exists to
prevent — and would make the take-over tier ambiguous, since `args` alone (no `command`) would mean
neither take-over nor append. With one field the tier boundary is a predicate: `template.command`
non-empty means the user owns the whole argv. `instance.go` already renders `Command:
inst.Spec.Command` with no `Args`, so both CRDs keep one argv source and render identically.

Acceptance:

- An `extraArgs` entry containing an owned key is **rejected by the webhook**, with a message naming
  the key.
- An `env` entry naming an owned environment key is rejected the same way; one naming
  `MC_TE_METRIC` is **accepted** and wins over the operator's default.
- A `template` carrying `resources`, or anything from which `replicas` or `instanceType` could be
  inferred, is rejected.
- A full `command` replacement marks `status.roles[i].unmanaged = true` and sets `CacheAttached` to
  `Unknown` with reason `Unmanaged`; the rendered Pod carries **no** operator-synthesized engine
  argument and **no** operator-set connector environment.
- The overlay tier merges on top of the operator's render, not under it, asserted by a case where
  both set the same non-owned key.

#### F6 — `roles` is a list, validated as length 1

`roles` is a list from the first version, because the next spec adds entries to it rather than
replacing the field. In this spec exactly one entry is allowed.

The bound is split deliberately between the two surfaces:

- The **schema** carries `+k8s:validation:minItems=1`. A deployment with no role is nonsense in this
  spec and in every later one.
- The **webhook** carries the length-1 rule, not the schema — so the rejection can carry an
  actionable message, and so lifting the restriction is a webhook edit rather than a CRD schema
  change that every stored object would have to survive.

Acceptance:

- `roles` with more than one entry is rejected with a message of the form: *"multiple roles are not
  supported by this version; P/D roles are introduced by the P/D atomic admission spec
  (`specs/*-pd-atomic-admission.md`)"*. The test asserts the message names a spec, not just a
  refusal.
- `roles` with zero entries is rejected by the schema.
- Removing the webhook rule is sufficient to accept two roles at admission — asserted by a unit test
  over the validation function, so the next spec inherits a seam rather than a rewrite.

#### F7 — Status: a flat phase, per-role readiness, an endpoint, a domain, three conditions

`worker/v1alpha1` has no `Conditions` anywhere: `InstanceStatus` is `Phase string` +
`PhaseMessage string` plus its observed fields. `ModelDeploymentStatus` keeps that pair as the
primary summary and adds conditions using the **existing `api/v1.Condition` type** rather than a new
per-CRD one.

Conditions are worth the addition here, and the reason is specific: the three facts below are
independent, and "quota reserved but cache not attached" is a real, actionable state that a single
phase string cannot carry.

| Condition | `True` | `False` | `Unknown` |
|---|---|---|---|
| `DomainRegistered` | the Binding resolved and its domain was read into `status.kvCache` | Binding missing, not Ready, or deleted under a running deployment | not yet resolved |
| `QuotaReserved` | every replica's Kueue Workload has quota reserved | a Workload is inadmissible; the reason names the ClusterQueue | admission still in flight |
| `CacheAttached` | see F8 | see F8 | see F8 |

`status.phase` takes the `Instance` vocabulary rather than inventing one: `Starting`, `Ready`,
`Degraded`, `Deleting`. `Ready` means every role's `ready == desired`; `Degraded` means at least one
replica is ready and at least one is not.

Acceptance:

- `status.roles[]` carries `name`, `desired`, `ready` and `unmanaged` per role.
- `status.kvCache` carries `binding`, `pool` and the `domain` block (`name`, `blockSize`, `dtype`).
- No new condition type is declared; `api/v1.Condition` is imported and used as-is.
- Status is rebuilt from observed state on every reconcile, so a stale field cannot survive a
  disagreement with the Pods.

#### F8 — `CacheAttached` is an observation, never an assumption

**A flag being accepted proves nothing.** Measured on the shipped artifact:
`--enable_kv_events=true` is accepted, the master's own startup log echoes `enable_kv_events=1`, and
yet `GET /kv_events/status` returns `{"enabled":false,...}` and the configured socket is never
bound. In the same project, a different undeclared build switch fails *loudly* —
`TENT backend is not enabled. Please rebuild with -DUSE_TENT=ON`. **You cannot infer one switch's
failure mode from another's**, so `CacheAttached` is judged on what is observed downstream of the
engine, never on "we rendered the flag" and never on a log line echoing it back.

The predicate is **the client having come up**, not cache traffic. A client that initialized answers
for itself from connector init, before any request; cache traffic appears only under load. Judging on
traffic would report a correctly attached but **idle** deployment as detached, which is a false alarm
on the most common steady state there is.

Level-based, evaluated every reconcile:

| State | Reason | When |
|---|---|---|
| `Unknown` | `Unmanaged` | the role took over the command line (F5) |
| `Unknown` | `NoReplicaReady` | no replica has become Ready yet |
| `Unknown` | `NoObservationAvailable` | neither signal below can be read at all |
| `True` | `CacheActive` | a signal was observed |
| `False` | `NoCacheActivity` | every replica has been Ready longer than the observation window and neither signal appeared |

Two signals, in order:

1. **Primary — the Mooncake store client's own HTTP server answers `GET /health` on a replica of
   this deployment.** The operator enables it (`MOONCAKE_ENABLE_CLIENT_HTTP_SERVER`, F4) and knows
   its port, and it is scraped per replica at the Pod's own address. It has the three properties the
   condition needs and nothing else has all three:
   - **Attributable.** It is answered by *this* replica's client process, so a sibling deployment
     sharing the same reuse domain cannot answer for it.
   - **Available at init, without traffic.** The server binds when the client comes up, so an
     attached but idle deployment is not reported detached.
   - **An effect, not an echo.** A client that failed to import its wheel, or failed to reach the
     master, never binds the port at all — so a bound port is downstream of the thing being judged,
     unlike a rendered flag or a log line repeating one back.
2. **Corroborating — the Binding's `status.usage` / `status.blocks` show the reuse domain holding
   data.** This is stronger evidence in one respect: it is the master's own account of bytes that
   actually landed. It is used where signal 1 cannot be read at all.

**The two are not ranked on one axis, and the second is not a weaker copy of the first.** They
answer different questions, and each is unusable for the other's:

- `usage` and `blocks` are **per reuse domain**, and F3's whole design is that one domain is shared
  by every deployment on one Binding — which case-47 asserts deliberately. So they **cannot
  attribute**: with a healthy deployment and a broken one on the same Binding, the healthy one's
  writes would report the broken one as attached.
- They also **only move under load**, which is the predicate this spec's own Alternatives rejects:
  an attached but idle deployment holds zero blocks, and an observed `blocks: 0` is byte-identical
  to a detached one's.

So the corroborating signal is used only when the primary cannot be read, and when it is, the
`True` it produces is recorded as the weaker attribution it is. If neither can be read, the
condition stays `Unknown` with `NoObservationAvailable`. **It is never `True` by assumption.**

⚠️ `usage` and `blocks` are **pointers with `omitempty` on the Binding**, and absent is not zero: an
absent figure means the scrape did not carry this domain, which is `NoObservationAvailable`, while an
observed zero is a domain holding nothing. Treating absent as zero would turn every unscraped pool
into a positive detachment report.

Acceptance:

- Breaking the config on purpose drives `CacheAttached` away from `True` — asserted by a test that
  breaks it, not by asserting the happy path only. Two vehicles:
  - **Unit, deterministic:** a fake scraper whose `/health` probe fails on every replica, over a
    deployment whose replicas have been Ready past the window and whose Binding reports no held
    data, yields `False` / `NoCacheActivity`.
  - **End to end:** an engine image without the matching per-vendor `mooncake-transfer-engine`
    wheel, and separately a Binding pointing at an unreachable pool. The engine's failure policy on
    a broken connector is not something this spec can predict — it may abort at init or serve on
    without the cache — so the assertion is the falsifiable one: **`CacheAttached` is never `True`**,
    and the case records which shape the engine took.
- The happy path yields `True` / `CacheActive` with no traffic having been sent, proving the
  client-came-up predicate rather than a traffic one.
- **Two deployments on one Binding, one healthy and one broken, report differently** — the case the
  per-replica predicate exists for. Judging on the shared domain's `usage`/`blocks` would report
  both as attached, so this is the assertion that would catch a regression back to the domain-level
  signal.
- A role that took over the command line yields `Unknown` / `Unmanaged` regardless of what the pool
  reports — the operator did not render the connector, so it does not claim the observation is about
  its own doing.

#### F9 — One Service, one endpoint

The deployment renders **one `ClusterIP` Service** selecting the role's Pods, and sets
`status.endpoint` to `http://<name>.<namespace>.svc:<port>`.

This is deliberately **not** what `instance.go` does. `convertServiceFromPod` renders one
`NodePort` Service **per Pod**, which is right for an `Instance` — a single addressable development
box a user SSHes into. N interchangeable replicas behind one address is a different shape: per-Pod
NodePort would publish N addresses with no load balancing and burn one node port per replica.

The port is the role template's first `Ports` entry, or 8000 where the template declares none — the
port the three supported engines serve on by default.

Acceptance:

- One Service per `ModelDeployment`, owned by it, selecting exactly the role's Pods.
- `status.endpoint` is populated once the Service has an address, and inference served through it
  reaches more than one replica across successive requests.
- Scaling the role up or down changes the Service's endpoints without recreating the Service.

#### F10 — Losing a replica costs cache, and that cost is visible

`kv_lease_duration` defaults to **30 seconds**. It does **not** expire because a request queued for
a long time, but it **does** expire when the engine Pod's heartbeat is interrupted — preemption,
eviction, restart — and under the default failure policy the request then simply fails. **So
preemption cost must account for invalidating KV blocks that a peer is still waiting on.** Even in
this single-role form it matters: a preempted replica's cached blocks are lost to its siblings.

- **Rollout is recreate, not surge.** A spec change that changes a replica's rendered Pod deletes
  and recreates it. There are no surge/unavailable knobs in this spec, and that is a decision rather
  than an omission: a rollout policy trades availability against **cache** as well as against
  capacity, and choosing the trade needs the hit-rate instrument this spec is building. The knobs
  are a later spec's, informed by F10's own numbers.
- **A replica leaving records an event** on the `ModelDeployment` naming the replica and the 30 s
  lease window, so an operator correlating a burst of failed requests with a preemption has the
  correlation written down rather than inferred.
- **`MC_TE_METRIC=1` is set by default** (a *defaulted*, not an owned, key — F5). Without
  transfer-engine metrics the hit rate that G1 rests on cannot be measured at all, and a knob that
  is off by default makes the headline claim unfalsifiable. `MC_STORE_CLIENT_METRIC_BANDWIDTH` is
  left unset because its cost is unmeasured, and `MC_STORE_MEMCPY` is left unset because it is
  auto-detected when unset.

Acceptance:

- Deleting one replica's Pod produces an event naming it and the lease window, and the deployment
  converges back to `desired`.
- Changing `roles[0].template.image` recreates every replica and the deployment returns to `Ready`.
- `MC_TE_METRIC` is present in every rendered replica unless the user set it, in which case the
  user's value stands.

### Verification

**Hardware: a local two-node Kubernetes cluster with two consumer GPUs on one node is sufficient. No
RDMA, no cloud.** Only functional correctness is verified here; throughput belongs to the P/D specs
and needs a separate RDMA window.

The verification ladder, cheapest first:

| Level | Vehicle | What it settles |
|---|---|---|
| unit | table-driven tests over the render, merge, validate and status functions | every rule in F1–F10 that does not need a live engine |
| integration | envtest + a fake client, the reconciler against a fake Binding and a fake pool status | convergence, ownership, condition transitions, the `CacheAttached=False` path |
| e2e | the dev image on the two-node cluster, `.claude/skills/gpustack-operator-e2e/cases/` | the four cases in the Test Plan, including the headline measurement |

The headline measurement (G1) is the one that cannot be faked at a lower level: it needs two real
engine replicas, one real pool, and one request stream replayed against both a single replica and
the four-replica deployment. **The numbers are recorded in the Test Plan when the case runs.**

### Notes / Constraints / Caveats

- ⛔ **This spec must write `KVCachePoolBinding.status.usedBy`, and the KV-cache-pool spec's
  finalizer is an empty shell until it does.** The pool side only ever READS that list — this
  operator has no way to enumerate workloads, so **the workload controller is the only writer there
  will ever be**. This spec's controller must append `{kind: ModelDeployment, namespace: "",
  name: <deployment>}` when a deployment attaches and remove it when it detaches; `namespace`
  carries the **empty string**, because a Binding and its workloads share a namespace and naming it
  would restate the object's own metadata. **Not writing it fails silently rather than loudly**: the
  Binding stays deletable while this deployment keeps writing into the pool that Binding authorizes.
  Make it an acceptance item of whichever task owns the attach path — a constraint recorded only in
  a Notes list is one that gets read once and then forgotten.
- **A Binding always carries a quota ceiling, so this spec never handles an "attached to a Binding
  with no quota" case.** `KVCachePoolBinding.spec.quotaCeiling` is **required**. The KV-cache-pool
  spec made it so after a real cluster showed there is no such thing as a master default: a tenant
  with no explicit quota policy is refused outright — `TENANT_NOT_REGISTERED = -1701`, whose own
  definition reads *"Tenant has no quota policy"* — so an unset ceiling produced a Binding that
  passed admission, reported `Ready`, and could not take a single byte. **Do not add a code path
  for that state here**: one the schema refuses is one this spec may assume away.
- **No chart files, and no chart RBAC.** CRDs are generated into
  `api/worker/v1alpha1/zz_generated.crds.go` and installed **by the worker itself** at startup
  through `pkg/worker/apis/setup.go`; there is no `chart/crds/` directory. Webhook configurations are
  generated into `pkg/worker/webhooks/worker/zz_generated.webhooks.go` from `+k8s:webhook-gen:`
  markers and installed by `pkg/worker/webhooks/setup.go`. The worker's ServiceAccount is bound to
  `cluster-admin` (`deploy/gpustack-operator/chart/templates/worker/serviceaccount.yaml`), so there
  is **no per-resource RBAC rule to add**. `deploy/gpustack-operator/chart/**` is in no task's
  `Owns`; the guard is an e2e assertion that a `helm install` still ends with the CRD present (T12).
- **Defaults go in the CRD schema.** `+k8s:validation:default=` markers cover `connector: auto`,
  `replicas: 1` and the port default, so **one validating webhook is the whole admission surface**
  for this CRD — there is no mutating webhook.
- **`make generate` also regenerates webhook registration.** The registration lives in
  `pkg/worker/webhooks/worker/zz_generated.webhooks.go`, so the webhook task runs `make generate`
  too — not only the API-type task.
- **`api/v1.Condition` in this group already has a precedent, and it is the shape to copy.**
  `api/worker/v1alpha1/kv_cache_backend.go` imports `gpustack "gpustack.ai/gpustack/api/v1"` and
  declares `Conditions []gpustack.Condition` with `+patchMergeKey=type`, `+patchStrategy=merge`,
  `+listType=map`, `+listMapKey=type` and a `// nolint: lll` on the tag line. Its status is
  `Phase` + `PhaseMessage` + `Conditions` in fields 1–3, which is exactly the pair-plus-conditions
  shape F7 describes. `ModelDeploymentStatus` follows it field for field rather than inventing a
  second spelling of the same idea, and T1 stays isolated for readability of the generated diff
  rather than for risk.
- **The engine command line is not enumerated, on purpose.** Everything the operator synthesizes is
  a small, named set (F4's table); everything else is the user's through the three tiers. A field
  added here to accommodate a new engine flag is a field that will be wrong by the next engine
  release.
- **`blockSize` and `dtype` being wrong is silent cache pollution** — writes succeed, reads succeed,
  and the tensors are wrong. It is the single most damaging silent failure in the whole design. They
  are validated and immutable **on the Binding**; this spec's job is to surface in `status` which
  domain the deployment actually attached to, so an operator sees it without reading two objects.
- **The transfer engine picks random ports.** One observed run bound 15002 (P2P RPC) and 15995 (TCP
  transport); a second client took 16566 and 16655 — none of them configured. **Any NetworkPolicy or
  port reservation must be a range, not a list.**
- **A benign-looking startup ERROR to document**, or every user will file it as a bug:
  `E transfer_metadata.cpp:991] Local segment descriptor not found`.
- **The measured client contract.** The Mooncake store client's setup signature, with `tenant_id` a
  first-class parameter defaulting to `'default'`:

  ```python
  setup(local_hostname: str, metadata_server: str, global_segment_size: int,
        local_buffer_size: int, protocol: str, rdma_devices: str, master_server_addr: str,
        engine: object = None, enable_ssd_offload: bool = False, ssd_offload_path: str = '',
        tenant_id: str = 'default', enable_client_http_server: bool = False,
        client_http_port: int = 9300) -> int
  #  or setup(config: dict) -> int
  ```

  The parameter is **`rdma_devices`**; the JSON key for the same thing is `device_name` and the
  environment variable is `MOONCAKE_DEVICE` — one fact under three spellings, which is worth writing
  down once so a reader does not conclude two of them are typos. `tenant_id` is what carries the
  reuse domain.

  The last two parameters are F8's observation surface: `enable_client_http_server` binds a
  client-local HTTP server on `client_http_port` (default 9300) serving `GET /health`,
  `GET /metrics` and `GET /metrics/summary`. F8 reads `/health` and not `/metrics`, because
  `MC_STORE_CLIENT_METRIC=0` makes the metrics routes answer `503 metrics not available` while the
  client is attached and healthy — a knob a user may legitimately set, and one that would otherwise
  read as a detached deployment.
- **Go names stay snake_case per file** (`model_deployment.go`, `model_deployment_render.go`), never
  flat-concatenated. The API group is `worker.gpustack.ai/v1alpha1`; types live in
  `api/worker/v1alpha1/`, controllers in `pkg/worker/controllers/worker/`, webhooks in
  `pkg/worker/webhooks/worker/`.
- **Neither `make generate` nor `make lint` is run while drafting this spec.** Both are
  single-writer across this repository; the commands are written into the plan and executed by the
  build.
- **External references:**
  - Mooncake repository — <https://github.com/kvcache-ai/Mooncake>
  - Mooncake Store design — <https://kvcache-ai.github.io/Mooncake/design/mooncake-store.html>
  - Mooncake multi-tenancy deployment (`tenant_id` wiring for vLLM and SGLang) —
    <https://kvcache-ai.github.io/Mooncake/deployment/multi-tenancy.html>
  - vLLM KV transfer / connector configuration — <https://docs.vllm.ai/>
  - SGLang HiCache storage backend — <https://docs.sglang.ai/>
  - Kueue concepts (queue-name entrance label, PodSets) —
    <https://kueue.sigs.k8s.io/docs/concepts/>
  - Per-vendor client wheels — <https://pypi.org/project/mooncake-transfer-engine/>

### Boundaries

- **Always:** render Pods, so the existing five gates apply unchanged; read the reuse domain from the
  Binding; surface the attached domain in `status`; judge `CacheAttached` on an observation; keep
  `replicas` and `instanceType` structured fields that no template can shadow; keep this CR thin
  enough that upstream landing `DisaggregatedSet` + Kueue makes it deletable rather than entangled.
- **Ask first:** anything that adds a field to `Instance` or `InstanceSpec`; anything that adds a
  chart manifest or a chart RBAC rule; adding a mutating webhook; adding a second append tier;
  changing the `InstanceTemplate` type itself (both CRDs would move).
- **Never:** let a workload name its own reuse domain; accept a `poolRef` to a cluster-scoped pool or
  a bare URL; silently merge a key the operator owns; infer `replicas` or `instanceType` from the
  template; set `CacheAttached=True` on the strength of a rendered flag or a log line; write a
  NetworkPolicy or port reservation as a list of ports rather than a range; create an `Instance` from
  a `ModelDeployment`.

### Risks and Mitigations

- **This spec alone does not exercise the CR's justification** — cross-role atomic admission is the
  next spec's, so what lands here is the substrate → stated plainly rather than papered over. The
  mitigation is scope discipline: every field this spec adds is one the next spec needs (`roles` a
  list, `instanceType` structured, the template shared), so nothing lands that would have to be
  un-landed.
- **Upstream `DisaggregatedSet` gains Kueue integration and this CR becomes redundant** → the CR is
  kept thin, holds no state the Binding or the pool could not, and adds no new gate to the admission
  chain. Retiring it is deleting a controller and a type, not unpicking the chain.
- **`MC_TE_METRIC` is silently unavailable on a Transfer Engine TENT build** — the vendor's own
  reference records it as *"Not supported when using Transfer Engine TENT"*. On such a build G1's
  hit rate is unmeasurable, and it fails by producing no metric rather than by refusing to start, so
  case-46 must fail loudly on a missing figure rather than record a zero. That is already the Test
  Plan's rule — *a run that cannot record a number is not a pass* — and this is the concrete way it
  gets exercised.
- **`KVCachePoolBinding` does not exist yet** (the pool spec) → the dependency is narrower than it
  looks. `poolRef` is a `core.LocalObjectReference`, which compiles against no Binding type at all,
  so T1, T2, T4, T5, T6, T7 and T8 are unblocked. Only T3 (resolution) and T9 (the corroborating
  signal) need the Go type, and they gate on it explicitly.
- **The e2e harness's own two known defects** (`deploy.sh` never upgrades and silently reinstalls
  over a torn-down release; `teardown.sh` strips finalizers from a hard-coded kind list that cannot
  contain a CRD this spec adds, then exits 0 with the drain hung) → T12 and T13 gate on the fixed
  scripts landing on the default branch. `ModelDeployment` carries a finalizer — it must remove its
  own `usedBy` entry — so the second defect is certain to bite rather than merely possible. The
  build does **not** fork a private copy of either script: two divergent copies of a harness are
  harder to diagnose than the one defect.
- **A user's take-over command line silently drops the cache** → that is the point of marking the
  role `Unmanaged` and moving `CacheAttached` to `Unknown`: the deployment reports that nobody is
  claiming the cache works, instead of claiming it does.
- **A wrong `blockSize`/`dtype` pollutes the cache silently** → not fixable here (the fields are the
  Binding's, validated and immutable there), so this spec makes the attached domain visible on the
  workload object. The mitigation is visibility, and the spec says so rather than implying it
  prevents the failure.
- **An idle deployment is reported as detached** → the `CacheAttached` predicate is the client's own
  `/health`, which answers from init, not cache traffic, which appears only under load.
- **A broken deployment reads as attached because a sibling on the same Binding is healthy** → the
  predicate is scraped per replica at the Pod's own address, so a sibling cannot answer for it. The
  domain-level figures, which cannot attribute, are corroboration only and never the sole basis for
  `True` while `/health` is readable.
- **The engine aborts rather than degrades on a broken connector** → the e2e assertion is
  `CacheAttached != True`, which holds in both shapes, and the case records which one the engine
  took rather than assuming.
- **A rollout costs more cache than it is worth** → recreate is the only policy in this spec, the
  cost is stated, and the surge/unavailable knobs wait for the hit-rate instrument this spec builds
  to inform them.
- **The transfer engine's random ports break a locked-down cluster** → documented as a range
  requirement, and the e2e cluster runs with no NetworkPolicy so the failure mode is not
  accidentally hidden.
- **`MC_TE_METRIC` defaulting on costs something unmeasured** → it is a *defaulted* key, so a user
  turns it off with one `env` entry and no spec change; and without it G1 is unmeasurable, which is
  the worse cost.

## Design Details

### Commands

Build and test run locally on darwin; nothing in this spec is CGO or linux-only.

```bash
go build ./api/... ./pkg/worker/...
go test ./api/worker/v1alpha1/... \
        ./pkg/worker/controllers/worker/... \
        ./pkg/worker/webhooks/worker/...
make test                     # go test -v -failfast -race -cover -timeout=30m ./...
make lint                     # golangci-lint over the whole module
make lint docs                # the documentation contract
```

**Code generation runs from a module-suffixed physical path.** `make generate` derives package paths
GOPATH-style and requires a working directory ending in `gpustack.ai/gpustack`; a worktree path does
not satisfy that. So the generator runs in the main checkout and the resulting delta is synced back.
When syncing with `rsync`, use `--filter='P .git'` and **not** `--exclude '.git/'` — a worktree's
`.git` is a *file*, which the latter misses, and combined with `--delete` it destroys the receiver's
repository.

```bash
make generate                 # T1 and T2 — from the main checkout
git diff --stat api/ pkg/worker/webhooks/   # the reviewable artifact of each generate
```

`make generate` regenerates deepcopy, register, apiservice, CRDs, conversion, protobuf **and webhook
registration**, so both T1 and T2 run it and each asserts its own diff.

End-to-end runs against a local two-node cluster with two consumer GPUs on one node, through the
existing e2e skill:

```bash
# from .claude/skills/gpustack-operator-e2e/ — build & load the dev image, helm install, run cases
bash cases/case-45.sh    # replicas reach ready, endpoint serves
bash cases/case-46.sh    # the headline: shared cache, hit rate beats a single replica
bash cases/case-47.sh    # same Binding shares, different Bindings do not
bash cases/case-48.sh    # the deliberate break: CacheAttached is never True
```

### Project Structure

```
api/worker/v1alpha1/
  model_deployment.go                  # the ModelDeployment type (T1)
  zz_generated.crds.go                 # regenerated: + the ModelDeployment CRD
  zz_generated.deepcopy.go             # regenerated
  zz_generated.register.go             # regenerated
  generated.proto, generated.pb.go     # regenerated; first api/v1 cross-group reference

pkg/worker/controllers/
  setup.go                             # + new(worker.ModelDeploymentReconciler)
  worker/
    model_deployment.go                # the reconciler: convergence, ownership, watches (T4)
    model_deployment_binding.go        # poolRef resolution, the domain read (T3)
    model_deployment_render.go         # base Pod render + the three-tier merge (T5)
    model_deployment_connector.go      # per-engine synthesis + the owned-key table (T6)
    model_deployment_service.go        # the Service and status.endpoint (T7)
    model_deployment_status.go         # phase, role readiness, the three conditions (T8, T9, T10)

pkg/worker/webhooks/worker/
  model_deployment.go                  # the validating webhook (T2)
  zz_generated.webhooks.go             # regenerated: + the ModelDeployment registration

docs/
  reference/model-deployment.md        # the CR's own reference page (T11)
  README.md                            # the index entry
  architecture.md                      # the life-of-a-request table gains the ModelDeployment row

.claude/skills/gpustack-operator-e2e/cases/
  case-45.sh .. case-48.sh             # T12, T13
```

The brief this spec is built from names three files as the minimum
(`api/worker/v1alpha1/model_deployment.go`, `pkg/worker/controllers/worker/model_deployment.go`,
`pkg/worker/webhooks/worker/model_deployment.go`). The controller is split further above so that
tasks own disjoint paths and can land in parallel; the split is by responsibility, and each file is
smaller than the 1118-line `instance.go` it sits beside.

### Code Style

The API type, following the file's discipline — a doc comment states behaviour and the reason for
it rather than restating the field name, and a rule that exists for a security reason says so:

```go
// ModelDeployment is N replicas of one inference-engine role attached to a KV cache pool, so that
// the replicas hit each other's cached prefixes instead of each re-computing the same prefill.
//
// It RENDERS PODS DIRECTLY. The admission chain keys on Pods — a plain Pod is a first-class citizen
// of it and an Instance is sugar that renders one — so rendering Pods reuses all five gates with no
// new integration point. Instance could not serve as the substrate: it renders exactly one Pod, and
// its Spec is immutable after creation.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",subResources=["status"]
type ModelDeployment struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelDeploymentSpec   `json:"spec" protobuf:"bytes,2,opt,name=spec"`
	Status ModelDeploymentStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// ModelDeploymentKVCache attaches the deployment to a KV cache pool.
//
// THE REUSE DOMAIN IS NOT DECLARED HERE, AND THAT IS A SECURITY PROPERTY. tenant_id IS the reuse
// domain, so every distinct domain is a tenant with its own quota ledger: a workload free to name
// arbitrary domains could mint unlimited tenants in its namespace and escape the namespace quota
// ceiling entirely. Domain naming therefore lives on the KVCachePoolBinding, which an admin owns.
type ModelDeploymentKVCache struct {
	// PoolRef names a KVCachePoolBinding IN THIS NAMESPACE. The Binding is the authorization point:
	// an admin creating one in a namespace is what grants that namespace access to the pool. The
	// type is a LocalObjectReference rather than a namespaced one so that reaching another
	// namespace — or naming the cluster-scoped pool, or a bare endpoint URL — is unrepresentable
	// rather than merely rejected.
	//
	// +required
	PoolRef core.LocalObjectReference `json:"poolRef" protobuf:"bytes,1,name=poolRef"`

	// Connector selects how the engine's transfer configuration is produced. "auto" synthesizes it
	// from the pool's backend type and the engine. The enum has one value on purpose: it reserves
	// the discriminator, so naming a specific connector later is an enum widening rather than a new
	// field. There is no "none" — synthesizing nothing is reachable through a full command
	// replacement, which also marks the role Unmanaged and moves CacheAttached to Unknown.
	//
	// +k8s:validation:default="auto"
	// +k8s:validation:enum=["auto"]
	Connector string `json:"connector,omitempty" protobuf:"bytes,2,opt,name=connector"`
}

// ModelDeploymentRole is one engine role and its replicas.
//
// Replicas and InstanceType are STRUCTURED FIELDS AND MUST STAY SO. They are inputs to admission and
// scheduling — Kueue PodSet counts and flavor selection — so a template that could shadow them would
// make the admission feasibility check read a ledger that does not match reality. The template may
// override container content and nothing else.
type ModelDeploymentRole struct {
	// Name identifies the role. In this version there is exactly one role and its name is free-form;
	// the P/D spec gives the name meaning.
	//
	// +required
	// +k8s:validation:maxLength=63
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Replicas is how many Pods this role runs. Each is an independent Kueue Workload: this version
	// creates no pod group, which is correct for one role whose replicas are independently useful
	// and is what the P/D spec replaces with cross-role atomic admission.
	//
	// +k8s:validation:default=1
	// +k8s:validation:minimum=1
	Replicas int32 `json:"replicas,omitempty" protobuf:"varint,2,opt,name=replicas"`

	// InstanceType is the name of the InstanceType whose pool this role's Pods are admitted
	// against. It is what the queue-name label is derived from, through
	// nodefeature.FormatLocalQueueName.
	//
	// +required
	InstanceType string `json:"instanceType" protobuf:"bytes,3,name=instanceType"`

	// ExtraArgs is appended AFTER the operator-synthesized arguments. An entry naming a key the
	// operator owns is REJECTED rather than merged: a silent merge produces two
	// --kv-transfer-config values and no way to tell which one won.
	//
	// +listType=atomic
	ExtraArgs []string `json:"extraArgs,omitempty" protobuf:"bytes,4,rep,name=extraArgs"`

	// Env is appended the same way and refused on the same terms. Keys the operator merely defaults
	// — MC_TE_METRIC — are not owned: a user's value wins and no rejection follows.
	//
	// +listType=map
	// +listMapKey=name
	Env []InstanceEnvVar `json:"env,omitempty" protobuf:"bytes,5,rep,name=env"`

	// Template overlays the rendered container. The operator renders first and merges this on top.
	// A non-empty Command is the TAKE-OVER tier: the user owns the whole argv, the operator
	// synthesizes no engine arguments, the role is marked Unmanaged and CacheAttached goes Unknown.
	// Arguments fold into Command; there is deliberately no Args, because a second append tier
	// beside ExtraArgs would have no defined precedence.
	//
	// Unlike the Instance that shares this type, the template is MUTABLE — that immutability is a
	// rule the Instance webhook enforces on InstanceSpec, not a property of InstanceTemplate.
	Template *InstanceTemplate `json:"template,omitempty" protobuf:"bytes,6,opt,name=template"`
}
```

Conventions: exported types state behaviour, expectations and constraints; a rule with a security
reason states the reason at the field, not only in the spec; a field deliberately absent (`Args`,
`none`) says why at the field it would have sat beside.

### Implementation Plan

T1 lands the type alone so the first cross-group generated diff is reviewable by itself. T2, T3 and
T4 then fan out over disjoint files. T5 is the join — the merge sits on top of both the base render
and the synthesized arguments — and everything user-visible follows it.

Checkpoints: after T1 (nothing user-visible; the CRD installs); after T4 (a deployment produces
Pods that reach Ready); after T6 (the connector is rendered); after T9 (the whole status is
truthful); after T13 (the headline claim is measured and recorded).

- [ ] **T1 · Land the API type and get `make generate` green, nothing else**
  Blocked by: none
  Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/zz_generated.*`,
  `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/generated.pb.go`
  Gate: review
  Acceptance: `ModelDeployment`, `ModelDeploymentList`, `ModelDeploymentSpec`,
  `ModelDeploymentKVCache`, `ModelDeploymentRole`, `ModelDeploymentStatus`,
  `ModelDeploymentRoleStatus` and `ModelDeploymentKVCacheStatus` exist with the markers in Code
  Style; the status reuses `api/v1.Condition` and declares **no** new condition type; `roles` carries
  `minItems=1` and **no** `maxItems`; there is no domain field anywhere in the spec; there is no
  `Args` field. Nothing else changes — no controller, no webhook, no `setup.go` entry. The generated
  diff is additive and its only surprise is the group's first `api/v1` protobuf reference, which is
  read and confirmed rather than skimmed.
  Verify: from the main checkout `make generate`, sync back, then `make generate && git diff
  --exit-code`; `go build ./api/...`; `git diff --stat api/` shows the new file plus regenerated
  files and nothing under `pkg/`.

- [ ] **T2 · The validating webhook: every shape rule**
  Blocked by: T1
  Owns: `pkg/worker/webhooks/worker/model_deployment.go`,
  `pkg/worker/webhooks/worker/model_deployment_test.go`,
  `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
  Gate: review
  Acceptance: one **validating** webhook and no mutating one (defaults are schema markers).
  It enforces: `roles` length exactly 1, rejected with a message naming
  `specs/*-pd-atomic-admission.md` as the spec that lifts it; `extraArgs`/`env` carrying an owned key
  rejected, naming the key, reading the owned-key table rather than a hard-coded list;
  `template.resources` rejected, naming `roles[].instanceType`; a `poolRef` naming no Binding in this
  namespace rejected with an actionable message. The length-1 rule is a separable predicate, so the
  next spec lifts it by deleting one call — asserted by a unit test that calls the remaining
  validation with two roles and gets no error.
  Verify: `go test ./pkg/worker/webhooks/worker/...`; from the main checkout `make generate`, then
  `git diff pkg/worker/webhooks/worker/zz_generated.webhooks.go` shows the ModelDeployment
  registration **and nothing else**.

- [ ] **T3 · Resolve `poolRef`, read the domain, own `DomainRegistered`**
  Blocked by: T1, **and the pool spec landing `KVCachePoolBinding` as a Go type**
  Owns: `pkg/worker/controllers/worker/model_deployment_binding.go` + its test
  Gate: the `KVCachePoolBinding` type compiles
  Acceptance: a function resolves the named Binding in the deployment's own namespace, reads its
  immutable domain block, and returns the `status.kvCache` projection (`binding`, `pool`, `domain`
  with `name`/`blockSize`/`dtype`). Missing → `DomainRegistered=False`, reason `BindingNotFound`;
  present but not Ready → `False`, reason `BindingNotReady`; deleted under a running deployment →
  `False` with the Pods **left running**. No cross-namespace read is attempted, asserted by a fake
  client that fails the test if asked for another namespace.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentBinding`

- [ ] **T4 · The reconciler: one Pod per replica, converge, own them**
  Blocked by: T1
  Owns: `pkg/worker/controllers/worker/model_deployment.go` + its test,
  `pkg/worker/controllers/setup.go`
  Gate: review
  Acceptance: `ModelDeploymentReconciler` is registered in `setup.go` and watches
  `ModelDeployment` plus the Pods it owns. It renders `replicas` Pods named
  `<deployment>-<role>-<ordinal>`, each controlled by the deployment, each carrying
  `kueue.x-k8s.io/queue-name` = `nodefeature.FormatLocalQueueName(roles[0].instanceType)` and the
  role's requests. **Level-based and idempotent**: a hand-deleted Pod is recreated, a second pass
  over an unchanged spec issues no writes, and a scale down deletes the highest ordinals. A spec
  change that changes a rendered Pod recreates it (F10's recreate policy) and records an event naming
  the replica and the 30 s lease window. **No `Instance` is created.**
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeployment_` and the envtest
  convergence case.

- [ ] **T5 · The owned-key table and the three-tier merge**
  Blocked by: T4, T6
  Owns: `pkg/worker/controllers/worker/model_deployment_render.go` + its test
  Gate: review
  Acceptance: one merge function takes the base render, the synthesized arguments and environment,
  and the role's three tiers, and produces the final container. Append lands **after** the
  synthesized arguments; the overlay merges **on top of** the operator's render; a non-empty
  `template.command` is take-over — the operator contributes **no** engine argument and **no**
  connector environment, and the result is flagged `unmanaged` for the status to carry. The owned-key
  table is exported so the webhook (T2) reads the same data the renderer does, and a test asserts
  they cannot disagree: every key the renderer emits is either owned or defaulted, and no key is
  both.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentRender`

- [ ] **T6 · Connector synthesis for the three engines**
  Blocked by: T1 — **not T3**: the function takes the domain and the pool endpoints as plain values,
  so it is written and tested against a local input struct that T3 later fills from the Binding.
  Owns: `pkg/worker/controllers/worker/model_deployment_connector.go` + its test
  Gate: review
  Acceptance: a pure function over (engine, backend type, domain, pool endpoints) returns the
  arguments and the environment, matching F4's two tables for `vllm`, `vllm-ascend` and `sglang`,
  asserted against one golden fixture per engine. `MOONCAKE_TENANT_ID` = the domain name;
  `MOONCAKE_LOCAL_HOSTNAME` is a `fieldRef` to `status.podIP` and never a literal; the RDMA device
  list is `MOONCAKE_DEVICE`, not the JSON's `device_name` nor `setup()`'s `rdma_devices`. It sets
  **neither** `MOONCAKE_GLOBAL_SEGMENT_SIZE` **nor** `MOONCAKE_LOCAL_BUFFER_SIZE` — a test asserts
  their absence, because the operator inventing them is a silent capacity error. It sets
  `MOONCAKE_ENABLE_CLIENT_HTTP_SERVER` and `MOONCAKE_CLIENT_HTTP_PORT`, which F8 has nothing to
  scrape without, and `MC_TE_METRIC=1` as a **defaulted** key. **No ConfigMap and no config volume.**
  Before the fixtures freeze, confirm per engine that its connector reaches the client through
  Mooncake's own loader rather than one that insists on `MOONCAKE_CONFIG_PATH`; an engine that needs
  the JSON gets a per-replica object and this task says so rather than rendering an environment the
  engine ignores.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentConnector`

- [ ] **T7 · One Service, one endpoint**
  Blocked by: T4
  Owns: `pkg/worker/controllers/worker/model_deployment_service.go` + its test
  Gate: review
  Acceptance: one `ClusterIP` Service per deployment, owned by it, selecting exactly the role's Pods;
  `status.endpoint` = `http://<name>.<namespace>.svc:<port>` where the port is the template's first
  `Ports` entry or 8000. Scaling changes the Service's endpoints without recreating the Service. A
  test states why this is not `instance.go`'s per-Pod NodePort shape: N interchangeable replicas
  behind one address, not one addressable box.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentService`

- [ ] **T8 · Status: phase, per-role readiness, `QuotaReserved`**
  Blocked by: T4
  Owns: `pkg/worker/controllers/worker/model_deployment_status.go` + its test
  Gate: review
  Acceptance: status is **rebuilt from observed state every reconcile**, so a stale field cannot
  survive a disagreement with the Pods. `phase` takes `Starting`/`Ready`/`Degraded`/`Deleting` — the
  `Instance` vocabulary rather than a new one — with `Ready` meaning every role's `ready == desired`
  and `Degraded` meaning some ready and some not. `roles[]` carries `name`, `desired`, `ready`,
  `unmanaged`. `QuotaReserved` is `True` only when every replica's Workload has quota reserved, and
  its `False` reason names the ClusterQueue.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentStatus`

- [ ] **T9 · `CacheAttached`, judged on an observation**
  Blocked by: T3, T5, T8
  Owns: `pkg/worker/controllers/worker/model_deployment_cache_attached.go` + its test
  Gate: review
  Acceptance: the condition follows F8's table exactly. The predicate is the **Mooncake store
  client's own `GET /health`, scraped per replica at the Pod's address** — not cache traffic. A test
  with `/health` answering and **zero** traffic yields `True`, which is the case that proves an idle
  deployment is not reported detached. `/health` rather than `/metrics`, because a user setting
  `MC_STORE_CLIENT_METRIC=0` makes `/metrics` answer `503` while the client is perfectly attached.
  Where `/health` cannot be scraped, the Binding's `usage`/`blocks` corroborate; where neither can be
  read the condition is `Unknown` / `NoObservationAvailable`. **An absent (nil) `usage`/`blocks` is
  `Unknown`, never `False`** — absent is not an observed zero. It is **never** `True` because a flag
  was rendered or a log line echoed it. A role marked `unmanaged` yields `Unknown` / `Unmanaged`
  whatever is observed.
  Two deterministic negative tests live here: a fake scraper failing `/health` on every replica of a
  deployment Ready past the window, whose Binding holds nothing, yields `False` / `NoCacheActivity`;
  and the same deployment sharing a Binding with a healthy sibling that *is* filling the domain still
  yields `False`, which is the assertion that pins the predicate to a per-replica signal.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentCacheAttached`

- [ ] **T10 · Losing a replica: the event and the lease window**
  Blocked by: T8
  Owns: `pkg/worker/controllers/worker/model_deployment_events.go` + its test
  Gate: review
  Acceptance: a replica leaving — preempted, evicted, restarted or deleted — records an event on the
  `ModelDeployment` naming the replica and the 30 s `kv_lease_duration` window, so an operator
  correlating failed requests with a preemption has the correlation written down. The event is
  emitted from observed Pod state rather than from a delete hook, so it survives a missed watch
  event.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentEvents`

- [ ] **T11 · Documentation**
  Blocked by: T2, T5, T7, T9
  Owns: `docs/reference/model-deployment.md`, `docs/README.md`, `docs/architecture.md`
  Gate: review
  Acceptance: a reference page states the CR's contract — the Binding-inherited domain and *why* a
  workload cannot name one; the three override tiers with the owned-vs-defaulted key distinction and
  the full owned-key table; `CacheAttached`'s predicate and every reason string; the recreate rollout
  policy and its cache cost; the **port-range, not port-list** NetworkPolicy requirement; and the
  benign startup error `E transfer_metadata.cpp:991] Local segment descriptor not found` with a line
  saying it is expected, so users stop filing it. `docs/README.md` gains the index entry and
  `docs/architecture.md`'s life-of-a-request table gains the row for a Pod a `ModelDeployment`
  renders. Routed through the `gpustack-operator-docs` skill.
  Verify: `make lint docs`

- [ ] **T12 · e2e: the functional cases**
  Blocked by: T2, T5, T7, T9, T10
  Owns: `.claude/skills/gpustack-operator-e2e/cases/case-45.sh`,
  `.claude/skills/gpustack-operator-e2e/cases/case-47.sh`,
  `.claude/skills/gpustack-operator-e2e/cases/case-48.sh`,
  `.claude/skills/gpustack-operator-e2e/SKILL.md`
  Gate: a two-node cluster with two consumer GPUs on one node
  Acceptance: **case-45** — `replicas: 4`, one role, reaches `status.roles[0].ready == 4` and
  `status.endpoint` serves inference; the rejections all fire (two roles with the message naming the
  spec, an owned key in `extraArgs`, `template.resources`, a missing Binding, a cross-namespace
  `poolRef`, a self-declared domain). **case-47** — two deployments on the **same** Binding share
  blocks; two on **different** Bindings do not see each other's. **case-48** — the deliberate break:
  an image without the matching per-vendor wheel, and separately a Binding pointing at an unreachable
  pool; the assertion is `CacheAttached != True` in both, and the case **records which shape the
  engine took**. Each case reports its verdict through the suite's `lib.sh` rather than deciding for
  itself. The chart guard rides here: after `helm install`, the `modeldeployments` CRD is present —
  no chart manifest was added, the worker installs it.
  Verify: `bash cases/case-45.sh; bash cases/case-47.sh; bash cases/case-48.sh`, each PASS

- [ ] **T13 · The headline measurement: several replicas beat one, and the numbers are recorded**
  Blocked by: T12
  Owns: `.claude/skills/gpustack-operator-e2e/cases/case-46.sh`, this spec's Test Plan
  Gate: case-45 green on the two-GPU node
  Acceptance: one request stream, replayed twice — once against a `replicas: 1` deployment and once
  against a `replicas: 4` deployment on the same Binding, same model, same stream, same order. The
  case asserts, and **records**: the pool shows **one** domain with blocks contributed by **more than
  one** replica; the four-replica hit rate **exceeds** the single-replica hit rate on that same
  stream. The recorded figures — hit rate each way, block count per contributing replica, domain name
  — are written into this spec's Test Plan, not left in a run log. A run that cannot record a number
  is not a pass.
  Verify: `bash cases/case-46.sh`, PASS, and the figures land in the Test Plan's measurement table

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/worker/controllers/worker` needs a fake `KVCachePoolBinding` and a fake per-replica `/health`
  scraper, so T3 and T9 test without a live pool or a live engine. Neither exists today. The scraper
  is a seam the reconciler takes as an interface rather than dialling directly, because the negative
  cases are the ones that matter and a real dial cannot be made to fail on demand.
- A fake client that **fails the test if asked to read another namespace**, so F2's
  no-cross-namespace claim is asserted rather than assumed.
- Golden fixtures for the rendered Pod, one per engine, so F4's table is a diff rather than a
  paragraph.
- The e2e suite needs a request-stream replayer with a fixed prefix distribution, so case-46's two
  runs are the same stream in the same order — otherwise the hit-rate comparison measures the stream,
  not the cache.

#### Unit tests

New packages carry table-driven coverage of every case below and must not regress the package they
live beside.

**Validation cases** (`pkg/worker/webhooks/worker`) — every "reject" case is the point; a silently
accepted bad spec is worse than a refusal.

| Case | Fixture | Expected |
|---|---|---|
| `roles_one` | one role | accept |
| `roles_two` | two roles | reject; the message names `specs/*-pd-atomic-admission.md` |
| `roles_zero` | empty list | rejected by the schema (`minItems=1`) |
| `roles_two_without_the_length_rule` | two roles, the length predicate not called | **no error** — the seam the next spec inherits |
| `extra_args_owned_key` | `--kv-transfer-config=...` in `extraArgs` | reject; the message names the key |
| `extra_args_owned_key_sglang` | `--hicache-storage-backend-extra-config` on `sglang` | reject |
| `extra_args_owned_key_wrong_engine` | a vLLM-owned key on a `sglang` deployment | accept — ownership is per (engine, key) |
| `extra_args_unowned_key` | `--max-model-len=32768` | accept |
| `env_owned_key` | `MOONCAKE_CONFIG_PATH` | reject |
| `env_defaulted_key` | `MC_TE_METRIC=0` | accept; the user's value wins |
| `template_resources` | `template.resources` set | reject; names `roles[].instanceType` |
| `template_command` | `template.command` non-empty | accept; take-over |
| `pool_ref_missing` | names no existing Binding | reject; the message names namespace + Binding |
| `pool_ref_cross_namespace` | a namespace supplied as an unknown field | rejected or pruned; assert the observed behaviour |
| `domain_declared` | a `domain` block supplied as an unknown field | rejected or pruned; assert the observed behaviour |
| `replicas_zero` | `replicas: 0` | rejected by the schema (`minimum=1`) |

**Render and merge cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `base_render_labels` | one role, one replica | the Pod carries `kueue.x-k8s.io/queue-name` from `FormatLocalQueueName(instanceType)` |
| `base_render_no_instance` | any spec | no `Instance` object is created, ever |
| `append_after_synth` | `extraArgs` set | the appended arguments follow the synthesized ones in order |
| `overlay_on_top` | template and operator both set a non-owned key | the template's value wins |
| `takeover_no_synth` | `template.command` non-empty | no synthesized argument, no connector env, `unmanaged` true |
| `takeover_no_client_env` | take-over | not one variable of the client environment block is rendered; the operator claims nothing |
| `owned_and_defaulted_disjoint` | the key table | no key is both owned and defaulted; every emitted key is one or the other |
| `render_idempotent` | two passes, unchanged spec | the second pass issues no write |
| `scale_down_highest_ordinal` | 4 → 2 | ordinals 3 and 2 are deleted, 0 and 1 survive |
| `pod_deleted_by_hand` | one Pod removed | recreated at the same ordinal |
| `template_image_change` | image changed | every replica recreated; an event per replica |

**Connector cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `vllm_golden` | engine `vllm` | `--kv-transfer-config` enabling `MooncakeStoreConnector`, plus the client environment block |
| `vllm_ascend_golden` | engine `vllm-ascend` | `--kv-transfer-config` selecting `MultiConnector` |
| `sglang_golden` | engine `sglang` | `--hicache-storage-backend` + `-extra-config`, plus the client environment block |
| `tenant_id_is_domain_name` | any engine | `MOONCAKE_TENANT_ID` equals the Binding's domain name |
| `device_env_key` | any engine | the RDMA device list is rendered as `MOONCAKE_DEVICE`, never `device_name` or `rdma_devices` — the two spellings the other two surfaces use |
| `local_hostname_is_fieldref` | 4 replicas | `MOONCAKE_LOCAL_HOSTNAME` is a `fieldRef` to `status.podIP`, never a literal — a literal is correct for at most one replica |
| `no_segment_sizes` | any engine | `MOONCAKE_GLOBAL_SEGMENT_SIZE` and `MOONCAKE_LOCAL_BUFFER_SIZE` are **absent** |
| `client_http_server_enabled` | any engine | `MOONCAKE_ENABLE_CLIENT_HTTP_SERVER` and `MOONCAKE_CLIENT_HTTP_PORT` are set — F8 has nothing to scrape otherwise |
| `mc_te_metric_default` | user set nothing | `MC_TE_METRIC=1` present |
| `mc_te_metric_user_off` | user set `MC_TE_METRIC=0` | the user's value, no duplicate entry |
| `no_configmap` | 4 replicas | **no ConfigMap is created and no config volume is mounted** |

**`CacheAttached` cases** (`pkg/worker/controllers/worker`) — the negative cases are why this
condition exists.

| Case | Condition | Expected |
|---|---|---|
| `health_ok_no_traffic` | `/health` answers, zero cache traffic | `True` / `CacheActive` — an idle deployment is attached |
| `ready_health_fails_past_window` | Ready past the window, `/health` fails on every replica, Binding reports nothing held | `False` / `NoCacheActivity` |
| `ready_health_fails_within_window` | Ready, still inside the window | `Unknown` / `NoReplicaReady` → not yet `False` |
| `no_replica_ready` | no replica Ready | `Unknown` / `NoReplicaReady` |
| `health_unreachable_binding_holds_data` | `/health` unscrapeable; Binding's `usage`/`blocks` observed above zero | `True` / `CacheActive` via the corroborating signal |
| `neither_signal_readable` | `/health` unreachable and the Binding's figures **absent** | `Unknown` / `NoObservationAvailable` — never `True` |
| `binding_figures_absent_is_not_zero` | `usage`/`blocks` are nil pointers | `Unknown`, **not** `False` — absent is not an observed zero |
| `sibling_deployment_holds_the_domain` | two deployments on one Binding; this one's `/health` fails, the other's writes fill the shared domain | `False` — the domain-level figures must not attribute the sibling's data to this deployment |
| `flag_rendered_only` | the argument is present, nothing else observed | **never** `True` — the case the condition exists for |
| `log_line_echo_only` | the engine logged the flag back | **never** `True` |
| `unmanaged_role_health_ok` | take-over, and `/health` does answer | `Unknown` / `Unmanaged` — the operator claims nothing it did not render |

**Status and binding cases** (`pkg/worker/controllers/worker`).

| Case | Condition | Expected |
|---|---|---|
| `binding_missing` | no Binding | `DomainRegistered=False` / `BindingNotFound` |
| `binding_not_ready` | Binding exists, not Ready | `False` / `BindingNotReady`; phase `Starting`, not rejected |
| `binding_deleted_while_running` | Binding removed under Ready Pods | `False`; **Pods still running** |
| `domain_echoed` | Binding Ready | `status.kvCache.domain` carries `name`, `blockSize`, `dtype` |
| `cross_namespace_read_attempted` | any reconcile | the fake client fails the test if asked for another namespace |
| `phase_ready` | 4/4 ready | `Ready` |
| `phase_degraded` | 3/4 ready | `Degraded` |
| `phase_starting` | 0/4 ready | `Starting` |
| `quota_reserved_partial` | 3 of 4 Workloads admitted | `QuotaReserved=False`; the reason names the ClusterQueue |
| `status_rebuilt_wholesale` | a stale field in the stored status | overwritten from observed state |

#### Integration tests

- The reconciler under envtest against a fake Binding: create → four Pods → delete one → converged;
  scale 4 → 2 → 4; change the image → recreate; delete the deployment → Pods and Service garbage
  collected through the owner reference.
- The webhook under envtest: every rejection in the validation table arrives as a `Invalid` status
  error whose message is the one the table names.
- The `CacheAttached=False` path end to end within envtest, driven by the fake pool status — the
  deterministic half of the deliberate break, so the e2e case is confirming rather than discovering.

#### e2e tests

Run against a local two-node cluster with two consumer GPUs on one node. No RDMA, no cloud.

- **case-45 — replicas reach ready, and every rejection fires.** `replicas: 4`, one role,
  `status.roles[0].ready == 4`, `status.endpoint` serves inference. Then, in one pass: two roles
  rejected with a message naming a spec; an owned key in `extraArgs` rejected; `template.resources`
  rejected; a missing Binding rejected; a cross-namespace `poolRef` refused; a self-declared domain
  refused. Plus the chart guard: after `helm install` the `modeldeployments` CRD is present, having
  been installed by the worker rather than by a chart manifest.
- **case-46 — the headline.** One request stream with a fixed prefix distribution, replayed against
  a `replicas: 1` deployment and a `replicas: 4` deployment on the same Binding. Asserted: the pool
  shows one domain with blocks contributed by more than one replica, and the four-replica hit rate
  exceeds the single-replica hit rate. **Recorded** in the table below.
- **case-47 — the isolation claim.** Two deployments on the same Binding share blocks; two on
  different Bindings do not see each other's. The second half is the one that matters: a shared cache
  that leaks across reuse boundaries is worse than no shared cache.
- **case-48 — the deliberate break.** Two vehicles, run separately: an engine image without the
  matching per-vendor `mooncake-transfer-engine` wheel, and a Binding pointing at an unreachable
  pool. The assertion in both is `CacheAttached != True`; the case records which shape the engine
  took — aborting at init, or serving on without the cache — because that is not predictable from
  the sources and is worth knowing once.

**Measurement record (filled by T13).**

| Run | Replicas | Requests | Hit rate | Blocks by replica | Domain |
|---|---|---|---|---|---|
| baseline | 1 | *tbd* | *tbd* | *tbd* | *tbd* |
| shared | 4 | *tbd* | *tbd* | *tbd* | *tbd* |

A run that cannot record a number is not a pass: the headline claim is a comparison, and a
comparison with one side missing is an assertion about nothing.

## Alternatives

- **Render `Instance` objects instead of Pods.** Rejected on four grounds, all properties of the
  type rather than preferences: the admission chain already keys on Pods so rendering Pods needs no
  new integration point; `Instance` renders exactly one Pod, making "one replica = several Pods"
  inexpressible and guaranteeing a rewrite when the next spec needs tensor parallelism across nodes;
  `Instance.Spec` is immutable after creation so a rolling update degenerates into
  recreate-everything; and Kueue pod-group membership is expressed as labels on Pods, which through
  `Instance` would need a passthrough field existing only to be passed through. The cost — not
  inheriting `instance.go`'s volume, port, SSH and status work — is mitigated by sharing
  `InstanceTemplate` as the per-role template type.
- **A `ModelDeployment → InstanceGroup → Instance → Pod` chain.** Rejected, and not for complexity:
  the layering does not solve the two things that matter and adds a correctness risk.
  - `Instance` is **exactly one Pod, hard**, so an `InstanceGroup` of `Instance`s can never express
    "one replica = leader + workers across nodes". Adding a layer above `Instance` does not remove
    the constraint that ruled `Instance` out.
  - `pod-group-total-count` is a property of the **whole cross-role group** — a number no single
    `InstanceGroup` knows. Threading it down means `InstanceGroup` grows a field that exists only to
    be passed through.
  - **Kueue only creates the Workload after observing *all* Pods the group declares.** Across three
    independent reconcile loops, "create every Pod in one shot" is hard to guarantee: any layer's
    requeue can partially create, and **a partially created group means Kueue never builds a Workload
    at all — a silent hang, not an error.**
  - `Instance.Spec` is immutable, so `InstanceGroup` would have to implement revisions and
    retire-and-recreate — reimplementing ReplicaSet.

  An `InstanceGroup` may be worth having for its own sake; it must not be a substrate for this CR.
- **Put the reuse domain on the workload.** Rejected on a security ground, not a stylistic one:
  `tenant_id` *is* the reuse domain, so a workload free to name domains could mint unlimited tenants
  and escape the namespace quota ceiling, each new domain drawing its own quota entry rather than
  drawing down a shared one. Domain naming lives on the admin's object.
- **Make the reuse domain its own CRD.** Rejected: its identity is immutable and bound to the
  Binding, so a separate CRD would only add a referential-integrity problem. The pool's status
  provides the observability a separate object would have been created for.
- **Put the connector choice on the pool.** Rejected: the connector is tightly coupled to the
  *engine version*, and the engine version belongs to the workload. A cache object unaware of engine
  roles is a clean factoring already proven upstream — LMCache's cache CR serves prefiller and
  decoder from one instance, selecting config by a per-Pod role annotation.
- **Design a per-hardware pool** so the vendor client wheel is a pool property. Rejected: the
  per-vendor artifact is the **client**, and it belongs inside the per-vendor engine image, which
  this repo already splits cuda / rocm / CANN. The pool stays vendor-neutral.
- **Add `Args` to the per-role template.** Rejected: it would create a second append tier beside
  `extraArgs` with no defined precedence — the exact "which one wins" state Rule 1 exists to prevent
  — and would make the take-over predicate ambiguous, since `args` alone would be neither take-over
  nor append. Arguments fold into `Command`, which is also what `instance.go` renders today, so both
  CRDs keep one argv source.
- **Merge an owned key silently instead of rejecting it.** Rejected: two `--kv-transfer-config`
  values with no stated precedence is an undiagnosable state, and the user who wrote the second one
  has no way to learn that the first exists.
- **Let the template carry `resources` and infer `instanceType` from it.** Rejected: `replicas`,
  `instanceType` and (next spec) `parallelism` are admission and scheduling inputs. Inferring them
  from container content makes the admission feasibility check read a ledger that does not match
  reality.
- **Judge `CacheAttached` on the rendered flag, or on the engine's log echoing it.** Rejected on
  measured evidence: `--enable_kv_events=true` is accepted, the master's own startup log echoes
  `enable_kv_events=1`, and `GET /kv_events/status` still returns `{"enabled":false,...}` with the
  socket never bound. A different undeclared switch in the same project fails loudly instead, so one
  switch's failure mode says nothing about another's.
- **Judge `CacheAttached` on cache traffic — the Binding's `usage`/`blocks` moving — as the primary
  predicate.** Rejected twice over. Traffic appears only under load, so an attached but idle
  deployment would report `False`, a false alarm on the most common steady state there is; and those
  figures are **per reuse domain**, which F3 makes shared across every deployment on one Binding, so
  they cannot attribute and a healthy sibling would report a broken deployment as attached. The
  client's own `/health` answers from connector init and is per replica, so it is the predicate.
  `usage`/`blocks` moving is strictly stronger evidence that data landed, which is why it corroborates
  and is what the headline test measures — but it is not what the condition tests.
- **Per-Pod NodePort Services, as `instance.go` renders.** Rejected: right for one addressable
  development box, wrong for N interchangeable replicas — it would publish N addresses with no load
  balancing and burn a node port per replica.
- **Surge/unavailable rollout knobs in this spec.** Deferred rather than rejected: a rollout policy
  trades availability against **cache** as well as against capacity, and choosing that trade needs
  the hit-rate instrument this spec is building. Recreate is the policy here, and its cache cost is
  stated rather than hidden.
- **A mutating webhook for defaults.** Rejected: `+k8s:validation:default=` markers put the defaults
  in the CRD schema, so one validating webhook is the whole admission surface.

## Open Questions

- ~~**How does a replica supply `local_hostname` to the client?**~~ **Settled against the shipped
  client, and the working assumption was wrong.** `local_hostname` is in
  `_REQUIRED_NON_EMPTY_FIELDS`; `from_file()` raises `Missing required config field: local_hostname`
  when it is absent, and `load_from_env()`'s own default is the literal `"localhost"`. The client
  never derives it from the host's address on either branch. The spec's two listed options — an
  entrypoint prelude, a per-replica ConfigMap — were both avoidable: the environment branch carries
  the **whole** config including `tenant_id`, so F4 renders environment instead of a mounted JSON and
  supplies `MOONCAKE_LOCAL_HOSTNAME` per replica from the downward API (`fieldRef: status.podIP`).
  The residual is per-engine and stays with T6: whether each engine's connector reaches the client
  through Mooncake's own loader rather than one that insists on `MOONCAKE_CONFIG_PATH`.
- **Rolling update semantics beyond recreate.** Surge and unavailable knobs are deferred to a later
  spec, informed by T13's numbers. The open part is what the right default trade is: a replica's
  cached blocks are lost when it goes away, so a fast rollout has a measurable cache cost that this
  spec will be the first to be able to measure.
- **Which client observability knobs the operator should set beyond `MC_TE_METRIC`.**
  `MC_STORE_CLIENT_METRIC_BANDWIDTH` controls the bandwidth summary and `MC_STORE_MEMCPY` is
  auto-detected when unset; both are left alone here because their cost is unmeasured. If T13's
  measurement turns out to need the bandwidth summary, the default moves — and the reason for moving
  it will be a number rather than a preference.
- ~~**Does the pool expose a per-tenant registered-client view at all?**~~ **Settled: it does not,
  and the replacement is better rather than weaker.** `KVCachePoolBindingStatus` carries `Phase`,
  `PhaseMessage`, `Conditions`, `RequestedQuota`, `EffectiveQuota`, `Usage`, `OverQuota`, `Blocks`,
  `HitRate` and `UsedBy` — no registered-client count anywhere, and `KVCachePool.status.domains[]`
  adds only per-domain `Blocks` and `HitRate`. A registered-client count would in any case have read
  an **action** ("a client connected"), and this design judges **effects**.

  The replacement is the Mooncake store client's own `GET /health`, scraped per replica (F8). It is
  the one signal that is attributable, available without traffic, and downstream of the thing being
  judged. The domain-level `Usage`/`Blocks` corroborate it and cannot replace it: they are shared by
  every deployment on one Binding, so they cannot attribute, and they only move under load, so an
  idle attached deployment is indistinguishable from a detached one.
- **What the engine actually does when the connector cannot initialize** — abort, or serve on
  without the cache. Not predictable from the sources, and it differs per engine and per version.
  case-48 records the observed shape rather than asserting one; if it turns out to be "abort", the
  `CacheAttached=False` state is far rarer in practice than this spec assumes, and the `Degraded`
  phase carries most of the diagnostic weight instead.
- **Should the deployment stop rendering replicas when `DomainRegistered` goes `False`?** This spec
  leaves running Pods running when an admin deletes the Binding, on the grounds that tearing down a
  serving deployment because an admin object vanished is worse than serving without a cache. That is
  a policy choice an admin might reasonably want inverted, and inverting it is a Binding-side
  decision (a `deletionPolicy`) rather than a workload-side one.
