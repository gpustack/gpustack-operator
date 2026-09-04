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
referencing the same Binding share KV, and name matching between workloads disappears along with a
whole class of typo. **The domain is not yet enforced at the storage layer, and this spec ships
saying so**: `tenant_id` is a parameter past the positional cut every supported engine stops at, so
every deployment reaches the cache as the tenant named `default` (F4). The API property that matters
survives — a workload still cannot name a domain, so it cannot mint tenants to escape a quota
ceiling — and the missing half is enforcement downstream of the API, which is upstream's to land.

The engine command line is the fastest-moving thing in the design, so it has a three-tier escape
hatch — append, overlay, take over — guarded by two webhook rules that refuse a silent merge on a
key the operator owns and keep the scheduling scalars out of the template. And because a flag being
accepted proves nothing about it being in effect, `CacheAttached` is judged on each replica's own
engine accounting for its store operations — never on the operator having rendered the flag, and
never on a figure a whole shared tenant contributes to.

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
  names — so the namespace quota ceiling cannot be escaped by minting tenants (F3). This is met at
  the **API** layer, which is where the escape it prevents would have happened. It is **not** met
  downstream: `tenant_id` reaches no supported engine, so the domain a deployment inherits does not
  yet reach the cache (F4). The spec reports that rather than implying otherwise, and closing it is
  upstream's to land.
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
- **Version feasibility checking.** The operator assembles an image name out of the engine, the
  engine version and the observed hardware (F11). It does not check that the combination was ever
  published, that the engine version supports the installed driver, or the reverse. **The user
  guarantees version alignment.** Any such gate would need the runner's release matrix compiled into
  the operator, which is precisely what F11 is defined as not doing; and the failure it would prevent
  is already legible without it, as an `ImagePullBackOff` on a tag that does not exist. This also
  means the enum on `engine` is not a claim that every listed engine can run on every pool.
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
  engine: vllm                             # vllm | sglang. The connector follows the role's
                                           # InstanceType backend, not this field (F4)
  engineVersion: 0.25.1                    # free-form, unvalidated. With each role's observed
                                           # backend it synthesizes that role's image (F11)
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
    resources:                             # the ACCELERATOR half only; CPU/memory are derived
      accelerator: 1                       # from the InstanceType's per-unit resources
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
- Two deployments on one Binding share KV — asserted end to end (Test Plan).
- Two on **two** Bindings not sharing is **not** asserted, because no supported engine passes
  `tenant_id` to the cache client, so every deployment lands in the tenant named `default` (F4). The
  semantics above are the API's and the design's; the enforcement is upstream's and is not there
  yet. The gap is stated here, in `status`, in the reference page and in case-47 rather than being
  left for a reader to infer from a passing test suite.

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

**Each engine reads a config reader of its own, and the operator renders what that reader reads.**
This is the single most important measured fact in F4, because the obvious design — drive Mooncake's
own configuration loader, which documents every key including the reuse domain — configures a loader
that **no supported engine calls**. Each of the three ships its *own* `MooncakeStoreConfig` class,
with its own key set and its own mandatory source:

| Engine package in the runner image (pinned) | Its config reader | Source it demands | Keys it reads |
|---|---|---|---|
| `vllm` `v0.25.1` | `vllm/distributed/kv_transfer/kv_connector/v1/mooncake/store/worker.py` | **`MOONCAKE_CONFIG_PATH`, or it raises** | `metadata_server`, `master_server_address`, `protocol`, `device_name`, `mode`, `global_segment_size`, `local_buffer_size`, `enable_offload` |
| `vllm-ascend` `v0.19.1rc1` | `vllm_ascend/distributed/kv_transfer/kv_pool/ascend_store/backend/mooncake_backend.py` | **`MOONCAKE_CONFIG_PATH`, or it raises** (its `load_from_env` only reads that one path) | `metadata_server`, `global_segment_size`, `local_buffer_size`, `protocol`, `device_name`, `master_server_address` |
| `sglang` `gateway-v0.3.1-1689` | `python/sglang/srt/mem_cache/storage/mooncake_store/mooncake_store.py` | in order: `--hicache-storage-backend-extra-config` → `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` → its own `MOONCAKE_*` environment | the six above plus `master_metrics_port`, `check_server`, `standalone_storage`, `client_server_address` |

So, under `auto`, the operator renders:

| `engine` | `backend` | Argument the operator adds | How the client is configured |
|---|---|---|---|
| `vllm` | `cann` | `--kv-transfer-config` selecting `AscendStoreConnector` | a mounted JSON + `MOONCAKE_CONFIG_PATH` |
| `vllm` | any other | `--kv-transfer-config` selecting `MooncakeStoreConnector` | a mounted JSON + `MOONCAKE_CONFIG_PATH` |
| `sglang` | any | `--hicache-storage-backend mooncake`, and **no** `--hicache-storage-backend-extra-config` | `MOONCAKE_*` environment variables, with `local_hostname` from `fieldRef: status.podIP` |

**The `sglang` row's omission is load-bearing, so it is written as an omission rather than left
blank.** `_load_config` tries three carriers in order — `extra_config`, then the file variable, then
the environment — and `load_from_extra_config()` is **key-for-key identical to `from_file()`**, all
ten keys falling back to the same literals (`mooncake_store.py:183-222`). So the extra-config
argument is not the safe alternative to the file; it is the *same* defect at *higher* precedence.
Passing it, with either address key in it, makes the environment carrier unreachable. An earlier
draft of this table named that argument, which would have shipped the bug at the one precedence level
where nothing downstream can override it.

**That table is keyed on two fields because the connector name was always a property of the backend,
never of the engine.** An earlier draft made `vllm-ascend` a third value of the `engine` enum and gave
it its own row, which read as a third engine and is not one: the runner matrix's own `service`
dimension takes `vllm`, `sglang`, `mindie` and `voxbox`, and every Ascend image is
`service=vllm, backend=cann`. The 128 `cann` records in that matrix spell the engine `vllm`. The
package that gets installed — `vllm_ascend` — is what the runner puts in the image when the backend
is `cann`, not something a user selects. So narrowing the enum to `{vllm, sglang}` is not a
compatibility concession; it removes a value that had been hung on the wrong dimension, and the row
that used to be `vllm-ascend`'s becomes a `(vllm, cann)` cell. The config-reader table above keeps
three rows for exactly the same reason it always had them: those are three Python packages with three
different readers, and which of them is present follows from `(engine, backend)`.

**`vllm-ascend` selects `AscendStoreConnector`, not `MultiConnector`**, and the difference is
not cosmetic. `vllm-ascend` re-registers `MultiConnector` to its own `AscendMultiConnector`, which
is a **composite** for running several connectors at once — a P2P connector beside a store
connector, say. The connector that reaches the Mooncake store is `AscendStoreConnector` (also
registered as `MooncakeConnectorStoreV1`), and its worker resolves the store backend from a
`backend` extra-config key that **already defaults to `mooncake`**, so the operator sets that key
too: not at all. Selecting the composite for a single-role deployment would add a layer with
nothing to compose.

`kv_role` is rendered as `kv_both`, and it is not optional: vLLM refuses a `kv_connector` with no
`kv_role`. `kv_both` is the value that is simultaneously a valid producer and a valid consumer,
which is what a store-backed cache shared between replicas needs — each replica both fills the
cache and reads from it.

For the file carrier, the JSON is rendered into a `ConfigMap` owned by the `ModelDeployment` and
mounted read-only into every replica of the role. It is **one ConfigMap per deployment rather than
one per replica**, and the claim that makes that safe is narrower than an earlier draft asserted:
every key `vllm` and `vllm-ascend` read from the file is deployment-wide, because neither reads
`local_hostname` from the file at all — each computes it per process. **That is a property of the two
engines that take a file, not of all three**; `sglang` does read it from the file, which is why it
does not get one (see below). The key set rendered is what the selected engine reads, and no more: a
key an engine's reader ignores is a key that documents a wiring that is not happening.

**The reuse domain does not reach the storage layer on any of the three engines, and this spec
ships saying so rather than implying otherwise.** `tenant_id` is the **11th** parameter of
`MooncakeDistributedStore.setup()`. All three engines call `setup()` **positionally**, stopping at
the 7th or 8th argument:

- `vllm` `.../store/worker.py:1040` — 7 positional arguments, through `master_server_address`.
- `vllm-ascend` `.../mooncake_backend.py:42` — 8, adding the transfer engine.
- `sglang` `.../mooncake_store.py:372` — 8, likewise.

And there is **no fallback that could rescue it**: in `mooncake-integration/store/store_py.cpp:2234`
`tenant_id` is a pybind argument defaulting to the literal `"default"`; the C++ client reads no
`MOONCAKE_TENANT_ID` anywhere; and the only reader of that variable is Mooncake's own
`mooncake_config.py` / `mooncake_store_service.py`, which no engine uses. **None of the three
engines' config classes carries a `tenant_id` key at all** — `tenant` does not appear anywhere under
`vllm/distributed/kv_transfer/` or `python/sglang/`.

The vendor's own `docs/source/deployment/multi-tenancy.md` has a *SGLang* section and a *vLLM*
section describing exactly this wiring. **The shipped engine integrations do not implement it.** This
is the same failure this spec's F8 was written for, one level up: the documentation is not the
artifact, and a documented key is worth no more than a rendered flag.

Consequences, stated plainly so that nobody reads this CR as delivering more than it does:

- Every deployment on these engine versions writes into the tenant named `default`, whatever domain
  its Binding declares.
- **F3's API property still holds and is the durable half**: a workload cannot name its own domain,
  so the escape-hatch-to-unlimited-tenants attack the design exists to prevent is still
  unrepresentable. What is missing is enforcement *downstream* of the API, not in it.
- **The isolation half of case-47 cannot pass yet** and is recorded as deferred with this reason,
  rather than being quietly dropped or asserted against a mechanism that is not there. The sharing
  half — and G1's headline measurement — are unaffected, because one shared `default` tenant shares
  KV perfectly well.
- `status.kvCache.domain` therefore reports **what the Binding declares**, which is what the operator
  needs in order to diagnose sharing, and the reference page states in one line that the domain is
  not yet enforced at the storage layer. A status field that claimed enforcement would be the
  design's own anti-pattern.

The route to enforcement is upstream: `tenant_id` reaching `setup()` in each engine. When it lands,
the operator adds one key to the rendered JSON and case-47's second half flips from deferred to
asserted — **nothing lands here that would have to be un-landed.**

**`local_hostname` splits the two engines apart, and the earlier claim here that no engine reads it
from the file was wrong.** It held for `vllm` and `vllm-ascend` — they compute it per process, from
`get_requester_local_hostname(get_ip())` (overridable by `MOONCAKE_REQUESTER_LOCAL_HOSTNAME`) and
`get_ip()` respectively. It does **not** hold for `sglang`, and the error has a shape worth naming:
that engine's config class has **two loader paths with different behaviour**, and the earlier
measurement was taken on the one this design does not use.

- `MooncakeStoreConfig.load_from_env()` reads `MOONCAKE_LOCAL_HOSTNAME`, falling back to
  `LOCAL_HOSTNAME`. Environment-driven, so a Pod can supply it.
- `MooncakeStoreConfig.from_file()` reads `local_hostname` **from the JSON**, falling back to
  `envs.MOONCAKE_LOCAL_HOSTNAME.default` (`mooncake_store.py:114-116`). And `EnvField` stores
  `default` as a plain attribute in `__init__`, with `os.getenv` reached only from `get()`
  (`environ.py:38-54`) — so **that fallback never consults the environment**. Its value is the
  literal `EnvStr("localhost")` (`environ.py:296`). All ten keys `from_file()` loads fall back the
  same way.

So a file-carried SGLang config either names a `local_hostname` the operator cannot know at admission
time (it is the Pod IP) or leaves every replica registering its transfer-engine identity as
`localhost`.

**Hence the carrier is per engine: `vllm` and `vllm-ascend` take a file, `sglang` takes environment
variables.** The two directions are forced independently and in opposite ways:

| Engine | Carrier | Why the other one cannot work |
|---|---|---|
| `vllm`, `vllm-ascend` | **file** | `load_from_config()` uses the environment only to locate the *path*; the configuration itself has to come from the file, with no per-key environment fallback |
| `sglang` | **environment** | the file path is what selects `from_file()`, whose fallbacks are literals; the environment path is the only one where `local_hostname` can arrive as `fieldRef: status.podIP` at container start |

The two carriers do not collide, because the two engines look for a file under **different variable
names** — `MOONCAKE_CONFIG_PATH` and `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`. Leaving the latter unset
is what selects the environment path: `_load_config` tries `extra_config`, then the file variable,
then the environment (`mooncake_store.py:242-260`).

**The deepest reason for the split is evaluation time, and it is worth stating separately because it
survives even if every literal fallback above were fixed upstream.** A file's contents and an
argument's contents are both fixed when the object is admitted. **No Pod IP exists at that moment**;
it is assigned when the Pod is scheduled and bound. The only carrier whose value is produced later is
an environment variable with a `fieldRef`, which kubelet evaluates as the container starts. So for
any key whose correct value is a per-replica runtime fact, a declarative carrier is not a worse
option — it is not an option. That is why this is a carrier decision rather than a defaulting one.

**And the file carrier fails asymmetrically for SGLang, which is worse than failing outright.**
`setup()`'s first argument is `client_hostname`, which is `config.local_hostname` **unless all four
of these hold**: the shared transfer engine was initialized at all (`_shared_mooncake_transfer_engine
is not None`), `metadata_server == "P2PHANDSHAKE"`, `protocol == "rdma"`, and the device matches. In
that case it is `get_session_id()` from the shared engine — self-derived, correct, and
`local_hostname` goes unread (`mooncake_store.py:352-372`). Four conjuncts is a *narrower* escape
than three, so `config.local_hostname` is reached more often than a first reading suggests. Since
`metadata_server` is unconditionally `P2PHANDSHAKE` here, the literal `localhost` is invisible under
RDMA-with-a-working-engine and certain otherwise. **The verification hardware is a local cluster with
no RDMA, so the e2e path exercises the failing branch** — which is the only reason this is checkable
at the level this spec verifies at.

One more property of the environment carrier, recorded because it is a dependency and not a
guarantee: `load_from_env()` reads `MOONCAKE_LOCAL_HOSTNAME` and, when that is unset, falls back to
the legacy **`LOCAL_HOSTNAME`** (`mooncake_store.py:160-165`) — a generic name any base image or
entrypoint could already be exporting. The operator sets the specific one, so the legacy path is
never reached. **The correctness therefore rests on the operator setting it, not on nothing else
supplying it**, and a change that stops setting it would not fail loudly; it would inherit whatever
`LOCAL_HOSTNAME` happens to hold.

**Three surfaces spell the RDMA device list three different ways** — `setup()`'s positional
parameter is `rdma_devices`, the JSON key every engine uses is `device_name`, and Mooncake's own
environment variable is `MOONCAKE_DEVICE`. This spec renders the engines' JSON, so **`device_name`**
is the spelling that matters here; the other two are recorded because the same fact under three
names is how a reader concludes one of them is a typo.

**These two keys DECLARE A ROLE, and this spec had it backwards.** It said they merely size a
replica's contribution, that a wrong operator-chosen value would be a silent capacity error, and
that the operator therefore renders neither — nor `mode`, whose cross-field rule couples to them.
Measured in vLLM v0.25.1's own config class
(`.../mooncake/store/worker.py`: `DEFAULT_GLOBAL_SEGMENT_SIZE` and `DEFAULT_LOCAL_BUFFER_SIZE` are
both **4 GiB**, `mode` defaults to **`embedded`**, and `__post_init__` requires a positive global
segment in embedded mode):

| `global_segment_size` | role the engine rank takes |
|---|---|
| `> 0` (the default) | an **in-process store member** contributing that much memory |
| `0` (requires `mode: standalone-store` on `vllm`) | a **pure client**, which is what this design wants — S3 runs the members |

⇒ **Rendering nothing is not "leaving it to the client's defaults"; it is choosing the wrong role.**
Every engine Pod becomes a 4 GiB store member. And the two omissions fail in opposite directions,
which is why the mistake survives: `global_segment_size: 0` with no `mode` **raises** and is caught
immediately, while omitting both **does not raise** and is silent.

The old reasoning was right about the coupling and wrong about the conclusion: a cross-field pair
is dangerous **when split**, which argues for rendering the whole coherent group rather than none of
it. **The group is per engine, and no single key set is right for two of them:**

| Engine | `mode` | `global_segment_size` | `local_buffer_size` |
|---|---|---|---|
| `vllm` | **required** as `standalone-store`, or `0` raises | `0` | `> 0` — **128 MiB**, the documented client staging size |
| `vllm-ascend` | no such field | `0` | `> 0` |
| `sglang` | no such field | `0`, then divided by the TP factor (`mooncake_store.py:295`) | **no such key** — `DEFAULT_LOCAL_BUFFER_SIZE` (16 MiB) is passed to `setup()` as a literal, commented "Zero copy interface does not need local buffer" (`mooncake_store.py:22`, `:336`, `:376`) |

So the **128 MiB is a vLLM-family constant, not a shared one.** Writing it for `sglang` would render
a key no reader reads, which is the exact failure this section is otherwise arguing against.
`global_segment_size` is the one that must be written explicitly on **every** engine: `vllm`'s class
defaults it to 4 GiB, and `sglang`'s default is the string `"4gb"` parsed by
`_parse_global_segment_size` (`mooncake_store.py:63-76`, which accepts a bare `0` as well) — the same
4 GiB by a different route. Neither engine's default is a pure client.

**Both carriers render the group, and the reason neither was deferred is a scheduling fact rather
than a technical one.** The plan had been to leave this to the task that consolidates rendering into
the shared `pkg/worker/kvcache/inject` package, on the grounds that fixing it in the CR's own
renderer means verifying the same behaviour twice. That reasoning has a hole: **the remaining tasks
before ship do not depend on that package**, so this branch could reach `main` with the omission
intact, and every vLLM replica in it would contribute 4 GiB it was never asked for. A defect whose
fix depends on a cross-spec ordering that nothing enforces is not deferred — it is scheduled for
never. The double-verification cost is also smaller than it looked: the shared package's own
assertions for these keys already exist, so consolidation deletes a duplicate rather than moving an
unverified one.

⇒ The consolidation task therefore becomes a **pure deletion** of this renderer, which is a
stronger position to consolidate from than "delete this one and hope the new one covers it".

**That link is now closed, and it closed in the strongest available way.** The question was whether
Mooncake's C++ `setup()` accepts `global_segment_size == 0` at all — all three engines only pass the
value down, so acceptance is decided a layer below them. Measured in `real_client.cpp`'s
`setup_internal` (pinned at v0.3.13-rc1): a zero **skips mounting a segment**, with the comment
**"A size of 0 keeps the pure client/server setup semantics"**, and the validator reads
`if (value != 0 && value < MIN_SEGMENT_SIZE)` — rejecting a small non-zero value while admitting
zero. So zero is not merely tolerated; upstream names it with the same words this design uses for
the role it wants. `local_buffer_size == 0` is handled the same way, logging
"Local buffer size is 0, skip registering local memory".

⇒ Nothing technical blocks writing the group any more. What remains is ownership: the shared
rendering package takes this synthesis over wholesale, so fixing it in the CR's own renderer means
verifying the same behaviour twice, in the second place under a different structure.

**The per-vendor client is an image concern, not a pool concern.** Per-vendor client wheels exist and
are versioned in lockstep (all at 0.3.13 when measured): `mooncake-transfer-engine` (base, a CUDA 12
build), `-cuda13`, `-npu` (Ascend — the name is `-npu`, **not** `-ascend`), `-rocm` (x86_64 only) and
`-musa` (one file, cp310 only). **The per-vendor artifact is the client, and it belongs inside the
per-vendor engine image**; this repo's engine images are already split cuda / rocm / CANN. **The pool
itself is vendor-neutral, and this spec designs no per-hardware pool.** A deployment whose image
lacks the matching wheel is a `CacheAttached=False` case (F8), not an admission-time one — nothing
at admission can see inside an image.

Acceptance:

- For each `(engine, backend)` cell, the rendered Pod carries exactly the argument and the carrier in
  the tables above, and the carrier holds exactly the keys that engine's reader reads — asserted
  against a golden fixture per cell.
- On the file carrier, the ConfigMap is owned by the `ModelDeployment` and mounted read-only into
  every replica; one ConfigMap per deployment, mounted into all of them. On the environment carrier
  there is **no ConfigMap at all**, and a test asserts none is created — an unused ConfigMap would
  claim a wiring that is not happening.
- **`local_hostname` follows the carrier, and the rule inverts between them.** On the file carrier
  it is **absent**, and a test asserts the absence: `vllm` and `vllm-ascend` compute it per process,
  and a deployment-wide value would be wrong for every replica but one. On the environment carrier it
  is **present and required**, as `MOONCAKE_LOCAL_HOSTNAME` valued from `fieldRef: status.podIP` — a
  test asserts the `fieldRef` rather than any literal, because a literal is exactly the failure being
  avoided.
- **`--hicache-storage-backend-extra-config` is not passed**, and a test asserts its absence for
  `sglang`, with the precedence order as the recorded reason.
- **No `tenant_id` key is rendered either, and a test asserts that absence too** — rendering a key
  no reader reads would document a wiring that is not happening, which is the failure mode this
  spec's whole `CacheAttached` design exists to refuse. The test carries the reason, so that whoever
  deletes it when upstream lands `tenant_id` knows what they are changing.
- Changing the Binding's pool endpoint re-renders the ConfigMap; the replicas restart to pick it up
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
  undiagnosable. The catalogue is a data table keyed by (engine, key), and it holds exactly what the
  operator renders for that engine:

  | Engine | Owned arguments | Owned environment |
  |---|---|---|
  | `vllm` | `--kv-transfer-config` | `MOONCAKE_CONFIG_PATH` |
  | `sglang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config` | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME` |

  Adding an engine adds rows; the webhook reads the table rather than a hard-coded list, so the rule
  and the renderer can never disagree about what is owned.

  **One `vllm` row covers both backends, and an earlier draft of this table had two.** It listed
  `vllm-ascend` separately with a value IDENTICAL to `vllm`'s, which is the evidence the catalogue was
  hung on the wrong dimension: the owned keys follow the engine, while only the connector name
  follows the accelerator backend. T16 removed the engine value; this table follows it.

  **Two of SGLang's seven environment names are owned for what a user entry would DESTROY rather
  than duplicate**, and the operator sets neither of them. That engine picks its configuration source
  in the order extra-config argument, then config-path file, then environment; leaving the first two
  unset is what selects the environment loader, and each of the first two loads through a function
  whose per-key fallbacks are compile-time literals. So setting either does not override a value, it
  replaces the whole configuration with a 4 GiB segment and a `localhost` identity.

  The remaining five are the variables the engine actually reads, `MOONCAKE_LOCAL_HOSTNAME` among
  them — which is why this engine gets no config file: that key is the replica's own Pod IP, and only
  an environment variable can carry a `fieldRef` kubelet evaluates as the container starts.

  Note that `MOONCAKE_CONFIG_PATH` is owned on `vllm` and **not** on `sglang`, while six other
  `MOONCAKE_*` names are owned on `sglang` alone. The table is the authority; a name prefix is not.

  The config-path variable is the load-bearing one, and it is owned for a reason worth stating: it
  is the **only** pointer to the file the operator wrote. A user who re-points it silently swaps the
  whole client configuration — pool endpoint, protocol, transport — for whatever that other file
  says, and every symptom appears one layer away from the cause. The refusal names the tier that
  owns it.

  **Ownership is per engine and the table is what says so.** `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`
  is meaningless to `vllm` and is an ordinary user variable there. This is exactly the case the
  `extra_args_owned_key_wrong_engine` test pins.
- **Defaulted** — the operator supplies a value only where the user supplied none, because
  duplication is harmless and last-wins is well defined. `MC_TE_METRIC` is the case that matters
  (F10): the operator sets it, and a user may turn it off. It is read by the transfer engine itself
  rather than by any engine's config class, which is why it is reachable at all where a tenant is
  not. There is deliberately no variable named here for the tenant: the client reads none, so naming
  one would invent a knob that does not exist.

**Rule 2 — the scheduling scalars must not be inferrable from the template.** `replicas`,
`instanceType`, `resources` and (in the next spec) `parallelism` are inputs to admission and
scheduling — Kueue PodSet counts, flavor selection, topology domains. They stay **structured
fields**. The template may override container content and **never** the replica count or the
resource request; a template carrying `resources` is rejected, naming `roles[].resources` as where
the accelerator request lives. Otherwise **the admission feasibility check reads a ledger that does
not match reality**.

**`roles[].resources` exists because `instanceType` cannot supply the request, and the earlier
draft of this rule was wrong about that.** It said the resource decision "lives on
`roles[].instanceType`". Verified against the code, it does not:

- `getResourceRequirements` in `pkg/worker/controllers/worker/instance.go` reads every **amount** —
  card count, slice percentages, partition profile — off the workload's own `Resources`. The
  InstanceType supplies only *how to spell* the resource keys: whether it is acceleratable, the
  manufacturer, and whether it slices or partitions.
- `InstanceTypeSpec` carries `UnitResources`, which size **one card**, plus a fixed `LocalStorage`.
  How many cards a replica wants is a property of the model being served, not of the pool it is
  admitted against — two deployments on one InstanceType routinely want different counts.
- CPU and memory are **derived**, not declared: the `Instance` webhook computes them as
  `UnitResources × card count` and then caps them. `ModelDeployment` has no mutating webhook, so its
  **renderer** performs the same derivation.

So the role carries the accelerator half of a request and nothing else. That is a *stronger* form of
Rule 2 than refusing a full `resources` block would have been: CPU and memory are not merely
refused in the template, they are **inexpressible anywhere**, and a field that does not exist cannot
be shadowed. `ModelDeploymentRoleResources` mirrors `InstanceResources`'s accelerator field names
exactly rather than inventing a second vocabulary for one request.

The two rules that need the InstanceType itself — that it offers the mode being asked for, and that
the request fits its per-unit ceiling — are not in the validating webhook, which holds no client.
They arrive with the Binding-resolution rule that gives it one. **Recorded rather than left as a
silent hole**: until then an infeasible request is refused by the admission chain's own gates rather
than at the API, which is a worse message but not a wrong outcome.

One rule *is* enforced without a client, because it needs no cross-object read and its silent
failure is the dangerous kind: a request naming both a partition profile and a slice percentage is
refused, since one accelerator cannot serve both and the renderer resolves the pair by precedence —
which would grant the profile and discard the percentages with nothing said.

**Arguments fold into `Command`; there is no `Args`.** `InstanceTemplate` has `Command []string` and
no `Args`, and this spec does not add one.

**That has a consequence the earlier draft left implicit: the operator builds the WHOLE argv, not
just the arguments.** With no `Args` field there is nowhere to put arguments beside an image's own
entrypoint, so the append tier cannot mean "add to whatever the image runs". Either the operator
renders base command + synthesized connector arguments + `extraArgs` into `Command`, or the user
replaces all of it (take-over). There is no middle where the operator contributes arguments to a
command line it did not build. The base commands are the engines' own entry points, verified in
their sources rather than assumed:

| Engine | Base command | Note |
|---|---|---|
| `vllm` | `vllm serve <model>` | the console script is `vllm`, and `serve` takes the model **positionally** |
| `vllm-ascend` | `vllm serve <model>` | a vLLM plugin, sharing its entry point |
| `sglang` | `python3 -m sglang.launch_server --model-path <model>` | `--model-path` is required; `--model` is its alias |

It follows that `roles[].template.image` is effectively **required** for a role to render at all —
the engine argv is the operator's, but the image carrying that engine is the user's, and there is no
default image this spec could pick. The renderer refuses a role with no image rather than creating a
Pod the API server would reject; making that a webhook rule is a client-free check worth adding when
the webhook next changes. Adding `args` would create a **second append tier beside
`extraArgs` with no defined precedence** — precisely the "which one wins" state Rule 1 exists to
prevent — and would make the take-over tier ambiguous, since `args` alone (no `command`) would mean
neither take-over nor append. With one field the tier boundary is a predicate: `template.command`
non-empty means the user owns the whole argv. `instance.go` already renders `Command:
inst.Spec.Command` with no `Args`, so both CRDs keep one argv source and render identically.

**Which template fields the overlay honours, and the one it cannot.** `Image`, `ImagePullPolicy`,
`Command`, `Privileged`, `Ports`, `Env`, `ImagePullSecret` and `AdditionalVolumes` all reach the
rendered container; `Resources` is refused at admission (Rule 2). `VolumeMount` is the exception:
it names where an `Instance`'s workspace volume is mounted, and a `ModelDeployment` has no workspace
volume — weights arrive through `AdditionalVolumes` or the engine's own hub client. It is
**ignored rather than refused**, and the reason is that refusing it would refuse every template:
the field carries the schema default `/workspace`, so it is set on any template a user writes and
**its presence carries no user intent to read**. A role that needs a path mounted names it in
`AdditionalVolumes`, where the mount path is the user's own.

**A replica always exposes a port**, defaulting to `8000` named `http` when the template names
none, because a replica nothing can reach serves nothing and the Service fronting the deployment
(F9) needs a target. `8000` is the port every supported engine's OpenAI-compatible server listens
on by default.

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

**Two facts `QuotaReserved` rests on, both verified rather than assumed.**

**It must read each Workload's own conditions, and cannot be derived from the admission gate.**
Gate 3 stops evaluating a Workload once it is admitted (Notes), so a `QuotaReserved` derived from
the gate would answer for the moment of admission and never again — a Workload preempted since
would still read as reserved. The Workload's own `QuotaReserved` condition is the only reading that
stays true over time.

**The ClusterQueue a refusal names is read off the spec, with no lookup.** The ClusterQueue is
named after the InstanceType — `instance.go` renders the entrance label as
`nodefeature.FormatLocalQueueName(inst.Spec.Type)` and states that the LocalQueue is "named by the
hash of the ClusterQueue(InstanceType) name". So `roles[].instanceType` **is** the queue's name, and
the message can point at something the user wrote rather than at a hash they cannot map back.

**The Workload belonging to a replica is found through its controller reference, not by
recomputing its name.** Kueue's `jobframework` calls `SetControllerReference(pod, wl, …)`, so the
reference is exact. Deriving the name instead means calling
`kueue/pkg/controller/jobs/pod.GetWorkloadNameForPod` — measured, that import pulls
`kueue/pkg/controller/jobframework`, which needs `github.com/ray-project/kuberay` and
`sigs.k8s.io/jobset` in `go.sum`. Two new module dependencies to recompute a fact the object
already carries.

#### F8 — `CacheAttached` is an observation, never an assumption

**A flag being accepted proves nothing.** Measured on the shipped artifact:
`--enable_kv_events=true` is accepted, the master's own startup log echoes `enable_kv_events=1`, and
yet `GET /kv_events/status` returns `{"enabled":false,...}` and the configured socket is never
bound. In the same project, a different undeclared build switch fails *loudly* —
`TENT backend is not enabled. Please rebuild with -DUSE_TENT=ON`. **You cannot infer one switch's
failure mode from another's**, so `CacheAttached` is judged on what is observed downstream of the
engine, never on "we rendered the flag" and never on a log line echoing it back.

**The predicate this section originally named does not exist on any of the three engines, and
that was measured.** It was "the client having come up", answering from connector init before any
request. What the shipped artifacts actually publish:

| engine | what it publishes about its KV connector | readable without traffic? |
|---|---|---|
| `vllm` `v0.25.1` | five `vllm:mooncake_store_operation_*` families (`.../mooncake/store/metrics.py:116,138,143,148,153`), **all labelled**; `.labels()` is reached only from `_get_metrics`, which is reached only from `observe()`, which returns at `metrics.py:177` on empty data | **no** |
| `vllm-ascend` | **nothing** — the repository declares zero Prometheus metrics of its own | **no** |
| `sglang` | `StorageMetricsCollector`'s counters, labelled and `.labels()`-ed only on the traffic path; `sglang:hicache_host_*` do appear at startup but prove only that the **host tier** is on, not that a remote store attached | **no** |

A childless labelled counter *does* still emit `# HELP` and `# TYPE` on a direct registry
(`prometheus_client` 0.26.0, measured with a control: one `.labels().inc()` is what adds a series),
and the presence of that header would have been an init-time, per-replica, effect-not-echo signal.
But vLLM exposes through `MultiProcessCollector` (`vllm/v1/metrics/prometheus.py:21-26` sets
`PROMETHEUS_MULTIPROC_DIR` when unset, line 49 registers the collector), and that collector
reconstructs metrics from the per-process `.db` files' samples: a metric with no samples writes no
file, so the exposition is **empty** — measured, again with the control.

**This section had already stated the rule that condemns its own signal.** It rejects the
corroborating `blocks: 0` because "an observed `blocks: 0` is byte-identical to a detached one's" —
and then chose a primary signal with exactly that shape. A discriminating requirement a spec sets
for one of its signals applies to **all** of them, and the rule was in this same document before the
signal was chosen. The constructive half matters as much: the answer is not to give up reporting,
but to go and find a signal that *does* discriminate.

So the reading is what the artifacts afford: **three values, and the third is not a negative.**
Level-based, evaluated every reconcile:

| State | Reason | When |
|---|---|---|
| `Unknown` | `Unmanaged` | the role took over the command line (F5) |
| `Unknown` | `NoReplicaReady` | no replica has become Ready yet |
| `Unknown` | `NoObservationAvailable` | no replica gave an account, and the domain's figures are absent or an observed zero |
| `True` | `CacheActive` | a replica reports **succeeding** store operations — or, weaker, the domain holds data |
| `False` | `CacheOperationsFailing` | a replica reports store operations of which **none** succeeded |

**`NoObservationAvailable` is `Unknown` because the state it would report has a nearer observer,
not merely because the signal is missing.** A connector that cannot come up takes its replica with
it — `vllm` raises without `MOONCAKE_CONFIG_PATH` — so that failure already appears as a replica
that never becomes Ready, which `status.roles[].ready` reports. What is left under this reason is a
deployment that is attached and idle, and reporting that as detached is the false alarm the whole
section exists to avoid. Written the other way round — "we cannot measure it, so we say nothing" —
it would be a defect; written this way it is a division of labour.

**`CacheOperationsFailing` is the one state with no other observer at all**, which is why it
survives as a `False` while the old `NoCacheActivity` does not. The engine is Ready, it is serving,
and every store operation it attempts fails: `roles[].ready` says Ready, `QuotaReserved` says
reserved, `DomainRegistered` says registered — the Binding is fine, the store is not. It is
observable because the metric families carry a `status` label whose values are hardcoded as `"ok"`,
`"error"` and `"partial_failure"` (six sites in `.../mooncake/store/worker.py`: 599, 688, 728, 906,
928, 1507), so a replica that tried and failed publishes series nothing else publishes.

There is therefore **no observation window**: the old `False` row needed one to tell a slow start
from a permanent failure, and neither of the two rows that replaced it does. An absence is `Unknown`
however long it lasts, and an observed failure is a fact on the pass that observes it.

Two signals, in order:

1. **Primary — the engine's own metrics endpoint, on a replica of this deployment, accounts for its
   store operations.** It is scraped per replica at the Pod's own address, and it has two of the
   three properties the condition wants:
   - **Attributable.** It is *this* replica answering about its own connector, so a sibling
     deployment sharing the same reuse domain cannot answer for it.
   - **An effect, not an echo.** A connector that failed to import its per-vendor wheel, or failed
     to reach the master, publishes no succeeding operation — so the report is downstream of the
     thing being judged, unlike a rendered flag or a log line repeating one back.
   - **NOT available without traffic.** No engine publishes anything before the first store
     operation (measured above), so this signal can say "it worked" and "it failed", and cannot say
     "it is up and nothing has happened yet". That third state is `Unknown`.

   It is the engine's own account of itself, which is a real limitation, but it is the account of
   the component that either did or did not attach.

   **Why not the cache client's own health server, which would have been better.** The Mooncake
   store client can serve `GET /health`, `/metrics` and `/metrics/summary` on port 9300 —
   per-client, bound at init, one layer closer to the thing being judged than the engine's own
   report. **It is unreachable on all three supported engines.** `enable_client_http_server` is the
   **12th** parameter of `setup()`, past the 7-or-8 positional arguments every engine passes (F4);
   there is no `MOONCAKE_ENABLE_CLIENT_HTTP_SERVER` in the C++ client; and the one escape hatch,
   `FLAGS_enable_http_server` at `mooncake-store/src/real_client.cpp:1147`, is a gflags flag whose
   `ParseCommandLineFlags` is called only by standalone binaries — never inside the Python
   extension, so loaded in an engine process it stays at its compiled `false`. Recorded because it
   is the signal to switch to the moment any engine passes that argument.
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

`usage` and `blocks` are **pointers with `omitempty` on the Binding**, and absent is not zero: an
absent figure means the scrape did not carry this domain, which is `NoObservationAvailable`, while an
observed zero is a domain holding nothing. Treating absent as zero would turn every unscraped pool
into a positive detachment report.

Acceptance:

- Breaking the config on purpose drives `CacheAttached` away from `True` — asserted by a test that
  breaks it, not by asserting the happy path only. Two vehicles:
  - **Unit, deterministic:** a fake scraper reporting, on every replica, store operations of which
    none succeeded, over a deployment whose Binding reports no held data, yields `False` /
    `CacheOperationsFailing`. And the same deployment with **no** account anywhere yields `Unknown`
    / `NoObservationAvailable`, which is the pair that keeps absence and failure apart.
  - **End to end:** an engine image without the matching per-vendor `mooncake-transfer-engine`
    wheel, and separately a Binding pointing at an unreachable pool. The engine's failure policy on
    a broken connector is not something this spec can predict — it may abort at init or serve on
    without the cache — so the assertion is the falsifiable one: **`CacheAttached` is never `True`**,
    and the case records which shape the engine took.
- **The fake scraper must not be able to express a reading no engine publishes.** A stand-in that
  could report "initialized, no traffic yet" would make the `Unknown` path unreachable in tests
  while it is the commonest state in production, and every case over it would be a case about a
  signal that does not exist.
- The happy path yields `True` / `CacheActive` from a replica's own account of a **succeeding**
  operation, not from a rendered flag and not from the shared domain's figures.
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

#### F11 — The runner image is a formula over five fields, never a lookup in a release matrix

F5 left `roles[].template.image` effectively required: a role that named no image rendered no
container. This feature makes the common case work without it, by synthesizing the image from fields
the API already has or gains here. It synthesizes by **string formula**, and deliberately does not
consult the runner's own release matrix — a decision whose consequences are stated below rather than
discovered later.

The published tag shape, checked against `gpustack_runner` 0.1.28's `runner.py.json` — all **338**
records, **zero** mismatches:

```
gpustack/runner:<backend><backend_version>[-<backend_variant>]-<engine><engineVersion>
```

That check needs a control, because a formula that reproduced nothing would also score zero
mismatches against an empty comparison: the same 338 records scored against a deliberately wrong
field order mismatch **all 338**.

Five fields, and the reason each comes from where it does:

| Field | Source | Scope | Why there |
|---|---|---|---|
| `engine` | `spec.engine` | deployment | the user's choice of serving stack |
| `engineVersion` | `spec.engineVersion` — new here | deployment | the user's choice of version, free-form |
| `backend` | the role's `InstanceType`'s `status.detail.manufacturer` | **role** | a property of the hardware, fixed by the admin when the pool was created |
| `backend_variant` | the same object's `status.detail.family`, mapped | **role** | likewise, and non-empty for exactly one vendor |
| `backend_version` | the same object's `status.detail.runtimeVersion` — new, aggregated | **role** | an observed fact about the nodes, not a choice anyone makes |

**The last three are on the `InstanceType`, not on the `ModelDeployment`, because they are not the
user's to state.** A user who could type a driver version could type one no node has. Putting them
on the observed side of an object the admin owns means the only way they can be wrong is that the
cluster changed, which is the case the reconciler already re-reads on every pass.

**The split of scopes in that table is what makes heterogeneous roles work without a new field.**
Two fields are per deployment and three are per role, so one `engine` plus one `engineVersion`
synthesizes a *different* image for each role, following each role's own hardware. A prefill role on
NVIDIA and a decode role on Ascend, both `vllm` `0.20.2`, render
`gpustack/runner:cuda12.9-vllm0.20.2` and `gpustack/runner:cann9.0-910b-vllm0.20.2`. The later spec
that lifts the length-1 bound on `roles` gets cross-vendor images for free.

That works only because the version sets overlap, so it is measured rather than hoped for: for
`vllm`, `cuda` publishes 19 versions and `cann` 15, and their intersection is **9**
(`0.10.0`, `0.10.1.1`, `0.10.2`, `0.11.0`, `0.12.0`, `0.13.0`, `0.14.1`, `0.16.0`, `0.20.2`); for
`sglang` the same pair intersects in **8**. **But the intersection across all backends of either
engine is empty**, so a single deployment-level `engineVersion` is sufficient for the two-vendor case
this programme targets and would not be for an arbitrary mix. A per-role override is therefore a
thing the P/D spec may need and this one does not: with `roles` bounded at length 1, deployment-level
and role-level are the same field, and adding the override now would be configurability nobody has
asked for.

**`runtimeVersion` is the minimum across the pool, and the disagreement it hides is reported.**
One `InstanceType` covers every node carrying its accelerator group, and a driver rollout makes
those nodes disagree for as long as it runs. The formula consumes one value, so the aggregate has to
choose one.

The minimum is the only value whose image *every* node can run — a container built against an older
toolkit runs on a newer driver, not the reverse — and that matters because **which node a replica
lands on is decided by admission, after the image is already in the Pod spec.** The alternative,
publishing nothing while the nodes disagree, takes the pool out of service for the whole rollout
window; a rolling driver upgrade is a routine operation, and a routine operation must not do that.

**The minimum's one real cost is that its failure mode is far from its cause.** A single
un-upgraded node drags the pool onto a version the matrix may not publish for the requested engine
version, and the user sees an `ImagePullBackOff` with nothing pointing at the node responsible. So
the disagreement is reported rather than absorbed: a **Warning Event on the `ModelDeployment`**,
naming the value taken and the other values present in the pool. It goes on the deployment and not
on the `InstanceType` because the deployment is the object whose failure sent the user looking.

⇒ **That event is reachable, and the contrast with the `deprecated` warning is the point.** This one
needs only "the nodes disagree", which is in this project's own `Devices` ledger; it reads nothing
outside the cluster. The `deprecated` flag lives in the runner's release matrix, which F11 reads
nothing from — so one warning can be built and the other cannot, and the difference is not effort.

An `InstanceType` with **no** `runtimeVersion` is a distinct state from one whose nodes all agree:
no synced flavor, or a generic collapsed pool, observes nothing at all. The field has to
distinguish those, so it does not default to a version it never saw.

**`manufacturer` maps to `backend`, and one vendor has no image at all:**

| Manufacturer | `backend` | | Manufacturer | `backend` |
|---|---|---|---|---|
| `nvidia` | `cuda` | | `metax` | `maca` |
| `ascend` | `cann` | | `mthreads` | `musa` |
| `amd` | `rocm` | | `iluvatar` | `corex` |
| `hygon` | `dtk` | | `thead` | `hggc` |
| `cambricon` | **none** | | | |

The runner data itself only ever spells the backend, never the vendor — its records carry no vendor
field at all — so the pairing is not derived here. It comes from `gpustack_runtime`'s
`_MANUFACTURER_BACKEND_MAPPING` (module `gpustack_runtime/detector/__types__.py`), whose own doc
comment states its names are meant to be the runner's backend names. Two rows do not follow from the
vendor name and would be got wrong by guessing: `hygon` is `dtk` and `thead` is `hggc` — the latter
independently carried by this repository's own `csrc/thead/ppu-slicing-shim/hggc/`. That map grew
`thead` between releases, absent at `0.1.39.post2` and present at `0.2.4.post3`, so it has to be read
at a current version.

`cambricon` is in `pkg/nodefeature/knowns.go`'s manufacturer list, and it is where this table stops
short of that upstream map: upstream pairs it with `neuware`, but no runner record carries a
`neuware` image, so a role on a cambricon pool cannot have an image synthesized and must name one.
That is a validation rule, not a silent empty string.

**`backend_variant` is non-empty for `cann` alone.** Across all 338 records the variant is populated
for `cann` (`310p` 26, `910b` 54, `a3` 46, `950` 2) and empty for the other seven backends. So the
mapping is needed for exactly one vendor — and it is not a lowercasing:

| `status.detail.family` (from `getFamilyFromSocName`) | `backend_variant` |
|---|---|
| `310P` | `310p` |
| `910B` | `910b` |
| `910C` | **`a3`** |
| `950` | `950` |
| `910`, `310B` | **none** |

`910C` becoming `a3` is the whole reason this table exists instead of a `strings.ToLower` call. The
detector's own comments name the pairs (`"910C" // 910C/A3`), and two families the detector can emit
have no published variant at all, which is the same case as `cambricon`: no synthesized image.

**`platform` is not a field in the formula, and that is measured rather than assumed.** No tag
contains `amd64` or `arm64` (zero of 338). The 338 records collapse to **208** distinct image names
and **338** distinct `(image, platform)` pairs, which is the signature of one multi-arch manifest per
name. So the operator never selects an architecture; the node's runtime does, at pull time.

**The runner matrix's ambiguity is not the operator's ambiguity.** Keyed by
`(engine, engineVersion, backend, backend_variant)` the matrix has 160 keys, of which **38 carry more
than one `backend_version`** — `('vllm', '0.25.1', 'cuda', '')` offers `12.9` and `13.0`, and so on.
That reads like an ambiguity the operator has to resolve, and it is not: every one of the 38 is a
`backend_variant == ''` key on `cuda` (29) or `rocm` (9), and each set is a **candidate set across
driver versions**. The cluster supplies the coordinate that picks one. A multi-valued key is
therefore evidence the formula can hit, not evidence it cannot.

The version shapes line up on their own, which is why no parsing is needed: the runner data carries
both `original_backend_version` (`9.1.0`) and `backend_version` (`9.1`), so its own published field is
already `major.minor`; and `device.NormalizeVersion` reduces every detector's runtime version to
`major.minor` by construction. The same shape on both sides is a fact about the two sources, not a
convention this spec introduces.

**The failure mode this buys, stated plainly.** A node reporting CUDA `12.4` under a role asking for
`vllm` `0.25.1` synthesizes `gpustack/runner:cuda12.4-vllm0.25.1`, and that image **does not exist**
— on `cuda` the matrix publishes `0.25.1` for `12.9` and `13.0` only. The replica then sits in
`ImagePullBackOff`. This is accepted, not overlooked: the decision on version alignment is that the
user guarantees it and the operator only assembles a name from the information it has. A gate that
rejected the combination would need the release matrix compiled into the operator, which is the
lookup this whole feature is defined as not doing.

Two consequences follow from that, and both are named here so neither is mistaken for an oversight:

- **A synthesized image carries no `deprecated` warning, and that is a decision taken here rather
  than an omission.** 78 of the 338 records are flagged deprecated, per image rather than per
  engine. Reading that flag requires the matrix, and the formula reads no matrix — so there is no
  signal for a warning to fire on. An earlier intent was to accept a deprecated image and record a
  Warning Event; **that intent required this feature's central premise to be false**, and it took
  measuring the flag's location to notice. The three ways to get the signal all cost in the same
  place: compile a snapshot of the matrix in (the lookup this feature is defined as not doing, and
  it goes stale against every runner release), query it at admission (a new outbound dependency on
  the request path), or leave deprecation to whatever surface publishes the matrix to users. **This
  spec takes the third**, because deprecation is a fact about the runner repository and belongs
  where that repository is presented. It is stated rather than left silent, since silence and
  oversight read identically to whoever comes next.
- **`roles[].template.image` stays as the override,** and remains the only way to run an image the
  formula cannot name — a cambricon pool, an unmapped Ascend family, a private build. A role that
  names an image gets it verbatim; synthesis never overwrites a stated value.

Acceptance:

- A role with `engine: vllm`, `engineVersion: 0.25.1` and no `template.image`, on an `InstanceType`
  whose detail says `nvidia` / `12.9`, renders `gpustack/runner:cuda12.9-vllm0.25.1`.
- The same role on an `ascend` / `910C` / `9.1` InstanceType renders
  `gpustack/runner:cann9.1-a3-vllm0.25.1`, exercising the one mapping that is not a lowercasing.
- A role naming `template.image` explicitly renders that image, whatever the detail says.
- A role on a `cambricon` pool, or an Ascend `910`/`310B` family, with no `template.image` is
  **refused at render time**, with a message naming the manufacturer or the family, rather than
  rendering a Pod with an empty image.

  **An earlier draft of this line said the validating webhook refuses it, and that would have been
  a defect.** Admission reads the `InstanceType`'s *observed* detail, which is exactly the field
  that has not converged when the type was just created — so a webhook refusing on it would reject
  a perfectly valid deployment for losing a race with the InstanceType reconciler. The render path
  has no such problem: it runs again on the next pass, so an unconverged detail is a wait rather
  than a verdict. **The information a gate needs has to exist at the moment the gate runs**, and
  here it does not.
  ⇒ An admission-time check remains possible for the half that never converges (a manufacturer with
  no backend), gated on the detail being populated at all. It is not built, because it duplicates a
  refusal that already happens and its value is only in the error arriving sooner.
- An `InstanceType` whose detail carries no `runtimeVersion` yet — a flavor not synced, a generic
  collapsed pool — renders no image and the role reports the reason; it does not render a tag with a
  hole in it. The message distinguishes this from the manufacturer being unmapped, because **only
  one of the two resolves on its own**.
- **The pool's version disagreement produces a Warning Event on the `ModelDeployment`**, naming the
  version used and the ones skipped, for a role whose image was synthesized and not for one that
  stated its own.
- **A change to the observed detail re-renders the replicas.** The controller watches
  `InstanceType`, without a generation predicate, because what moves is the status: a driver rollout
  changes the image every replica should be running, and the spec-hash comparison would otherwise go
  on matching Pods built from a version the pool no longer reports.

### Verification

**Hardware: a local two-node Kubernetes cluster with two consumer GPUs on one node is sufficient. No
RDMA, no cloud.** Only functional correctness is verified here; throughput belongs to the P/D specs
and needs a separate RDMA window.

The verification ladder, cheapest first:

| Level | Vehicle | What it settles |
|---|---|---|
| unit | table-driven tests over the render, merge, validate and status functions | every rule in F1–F10 that does not need a live engine |
| integration | a fake client, the reconciler driven directly against a fake Binding and a fake pool status | convergence, ownership, condition transitions, the `CacheAttached=False` path |
| e2e | the dev image on the two-node cluster, `.claude/skills/gpustack-operator-e2e/cases/` | the four cases in the Test Plan, including the headline measurement |

The headline measurement (G1) is the one that cannot be faked at a lower level: it needs two real
engine replicas, one real pool, and one request stream replayed against both a single replica and
the four-replica deployment. **The numbers are recorded in the Test Plan when the case runs.**

**One failure mode recurs across the levels of that ladder and is named here because it produced a
green result twice while building this spec: a test that agrees with the code by construction.** It
does not look like a weak test. It looks like a passing one.

- **Asserting the current behaviour on a point nobody has ruled on.** A case-45 row observed that a
  deployment naming an absent Binding still gets its replicas. Asserting either direction would
  have written an unreviewed rule into the suite, and asserting the *observed* direction is the
  worse of the two, because it guarantees only that the behaviour will not change — and what does
  not change may be the defect. The row recorded rather than judged until the rule was found (it
  existed; see the note on cross-references in Risks) and only then became an assertion.
- **Resting an acceptance on a signal that cannot fire.** T14's acceptance said a re-rendered
  ConfigMap is caught by the Pod spec hash moving. It cannot move, for a reason that is invisible
  from the sentence: a ConfigMap enters a Pod by name only. A test written against that wording
  passes because nothing was recreated, and reads as if it had proved nothing needs to be.

⇒ The discipline both cases point at: **a check whose expected output equals the output it would
produce if the mechanism were absent proves nothing.** Before writing an assertion, name what would
make it fail; if nothing would, the row belongs in the verdict table as a SKIP that says why, not as
a PASS. This is also why every refusal row in case-45 quotes a fragment of the operator's own
wording instead of the bare fact of a rejection, and why that case opens with a baseline that must
be **accepted**.

### Notes / Constraints / Caveats

- **This spec must write `KVCachePoolBinding.status.usedBy`, and the KV-cache-pool spec's
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

  The parameter is **`rdma_devices`**; the JSON key for the same thing is `device_name` and
  Mooncake's own environment variable is `MOONCAKE_DEVICE` — one fact under three spellings, which is
  worth writing down once so a reader does not conclude two of them are typos.

  **Read this signature by argument POSITION, because that is what decides what is reachable.**
  `tenant_id` is 11th and `enable_client_http_server` is 12th, and every supported engine calls
  `setup()` positionally with 7 or 8 arguments (F4). So the two parameters this design would most
  like to use — the one that carries the reuse domain, and the one that binds the client's own
  `/health` and `/metrics` on port 9300 — are both **past the cut and unreachable**, with no
  environment fallback in the C++ client and no in-process gflags path. A signature is not an
  interface until a caller passes the argument.
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
  - Mooncake multi-tenancy deployment —
    <https://kvcache-ai.github.io/Mooncake/deployment/multi-tenancy.html>
    Its *vLLM* and *SGLang* sections describe a `tenant_id` wiring that the shipped engine
    integrations of those two projects **do not implement** (F4). Cited for the master-side quota
    API, which is accurate, and **not** as authority for how an engine reaches a tenant.
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
- **The Kueue pod-group annotations are inert in this spec, and the operator therefore renders
  neither.** Both are group-only in the pinned `v0.17.1`:
  `isServing()` is `p.isGroup && annotation == value`, and `isReclaimable()` is
  `p.isGroup && !p.isServing()` — so on a Pod that is not part of a group both read false whatever
  the annotation says. `pod-group-fast-admission` is read only inside `constructGroupPodSets`.
  Since this spec creates **no pod group** (N replicas are N independent Workloads), rendering
  either would be emitting a key nothing reads, which is the same mistake as rendering a tenant no
  engine passes on.

  **They stop being inert the moment the P/D spec introduces the group, and that spec inherits
  two rules:** `pod-group-serving` becomes **mandatory** — without it a group's Pods are
  `isReclaimable()`, so Kueue applies batch semantics to a resident inference workload and shrinks
  the Workload as Pods terminate — and `pod-group-fast-admission` must stay **unset**, because it
  switches PodSet construction to `constructGroupPodSetsFast(items, totalCount)`, which builds the
  PodSets from the declared total instead of the observed Pods and so collapses the per-role
  accounting the whole P/D design rests on. Its cost is not free either: without it Kueue refuses to
  compose the Workload until the observed Pods reach the group total, which is the partial-creation
  hang this spec's Alternatives already cites.
- **Judging a Binding usable means reading a `Ready` that includes `QuotaGranted`, not the
  Binding merely existing and not `Phase: Ready` alone.** The KV-cache-pool spec found and fixed a
  state-machine defect whose only consumer is this spec: the Binding's earlier readiness read two
  axes, and one of them reported `True` both when the master granted an **effective quota of zero**
  and when the master carried **no ledger entry for the domain at all**. A Binding could therefore
  report `Ready` while unable to take a single byte. It was harmless only because nothing in the
  repository read `effectiveQuota` — and **this spec is what makes it a consumer.** The fix adds a
  third axis, `QuotaGranted`, whose `False` reasons distinguish `ZeroGranted` from `NoLedgerEntry`
  from `GrantNotExported`: **nil and zero are separate answers**, because "not exported" is not
  "granted nothing".
- **A store leader restart makes every Binding briefly not-Ready, and that is an ordinary
  operation rather than a fault.** The window was measured at 3.5–32 seconds, and it exists because
  the leader's readiness probe reads its segment list rather than its quota ledger, so the Pod is
  Ready while the ledger still answers zero. **The deployment must not do anything irreversible in
  that window** — not drop its `usedBy` entry, not report a permanent error, not tear down
  replicas. `DomainRegistered` goes `False` with the Binding's reason, the Pods keep serving, and
  the next reconcile converges. Waiting is the correct behaviour; punishing a Binding for a routine
  upgrade is not.

  **Convergence is therefore never gated on `DomainRegistered`, and that covers a Binding that was
  never created at all — not only one that is briefly unready.** The two reach the renderer by the
  same path on purpose: a controller that told them apart would have to decide how long "not yet"
  lasts, and it cannot, because a Binding created a second after the deployment is indistinguishable
  from one that is never coming. What bounds the cost is T14's rule — a deployment whose Binding
  cannot be resolved renders **no** connector rather than a partial one — so an unregistered domain
  costs the replicas their cache and nothing else.
- **The connector needs the members' transport, and no namespaced object republishes it.** Three of
  the four client keys have a source a `ModelDeployment` can read: `master_server_address` is the
  pool's `status.clientEndpoint`, `metadata_server` is the literal `P2PHANDSHAKE` the metadata plane
  takes unconditionally in this scope, and `device_name` has **no source at all** — the pool and the
  backend APIs carry no RDMA device list, so it is empty and the client auto-discovers, which is the
  right default on every transport that does not use one. `protocol` is the exception: it lives on
  the cluster-scoped `KVCacheBackend` as `spec.transport.protocol`, and the pool publishes its client
  endpoint and **nothing about the data plane**. The two ways out are for the deployment to read the
  backend the pool names, or for the pool to republish the transport; the second keeps every read a
  workload does inside one namespaced object, the first needs no cross-spec API change. Guessing is
  not one of them: a client configured for a fabric the members are not on fails as a transfer error
  one layer away from its cause. Resolving this is what the connector-wiring task waits on.
- **Gate 3's "no flavor assignment" hold cannot reach a `ModelDeployment`, and the reason is a
  timing guarantee rather than a property of this CR.** The node-devices feasibility check is scoped
  per podset's assigned flavor, and it holds a demand whose podset carries no assignment. A reader
  reasonably asks whether a single-role deployment can land in that hold. It cannot:

  - The controller's watch admits only a Workload holding a quota reservation
    (`pkg/worker/controllers/worker/node_devices_admission.go`, the `For` predicate).
  - `Reconcile` re-checks that **after** its own `Get`, and also skips evicted, finished and
    deactivated Workloads — a re-check that exists because Kueue drops the reservation and resets
    the checks in two separate writes.
  - Only past both of those is the demand parsed at all.
  - And Kueue's `SetQuotaReservation` (`pkg/workload/workload.go`, pinned at `v0.17.1`) assigns
    `Status.Admission` and sets the `QuotaReserved` condition **in one function, before a single
    status write**.

  So a Workload that reaches the parse always carries an admission, and the empty-flavor branch is
  unreachable *for that reason*. It stays reachable for three others — an assignment naming no
  entry for the podset, one carrying no accelerator flavor, and two manufacturers' credits resources
  covered at once — but all three need a ClusterQueue covering more than one resource, which is an
  admin-written queue rather than the one this CR's entrance label points at. **Recorded because the
  answer is "the path is dead", and a reader who cannot see that concludes the opposite from the
  commit message alone.**

  One consequence does land on this spec: Gate 3 deliberately stops evaluating once a Workload is
  admitted, so `QuotaReserved` (F7) must be read from the Workload's own conditions and never from a
  fresh feasibility verdict, which after admission is not recomputed.
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
- **A reader takes the CR's domain plumbing for isolation it does not yet have** → this is the
  largest honesty risk in the spec, because every layer *above* the engine does the right thing: the
  admin declares the domain, the workload inherits it, `status` reports it, the pool registers it.
  Only the last hop is missing, and a missing last hop is invisible from every layer above it. The
  mitigation is that the gap is written in four places a reader actually reaches — the Summary, G2,
  the reference page and case-47 — and that case-47 asserts the gap's own signature, so the claim
  cannot silently drift back to being true-by-omission.
- **A wrong `blockSize`/`dtype` pollutes the cache silently** → not fixable here (the fields are the
  Binding's, validated and immutable there), so this spec makes the attached domain visible on the
  workload object. The mitigation is visibility, and the spec says so rather than implying it
  prevents the failure.
- **An idle deployment is reported as detached** → **the mitigation this bullet originally named
  turned out not to exist**, and the replacement is weaker but honest. No engine publishes anything
  before its first store operation (F8), so an idle attached deployment and an unread one are
  indistinguishable. The mitigation is therefore that such a deployment reports `Unknown` rather than
  `False`: `CacheAttached` never carries a `False` an absence could produce, only one an observed
  failure produces.
- **A broken deployment reads as attached because a sibling sharing its tenant is healthy** → the
  predicate is scraped per replica at the Pod's own address, so a sibling cannot answer for it. The
  domain-level figures, which cannot attribute, are corroboration only and never the sole basis for
  `True` while the engine's report is readable. With `tenant_id` unreachable (F4) the shared
  tenant is `default` and therefore **cluster-wide**, so the corroborating signal cannot even
  distinguish namespaces — which makes the per-replica predicate load-bearing rather than merely
  preferable.
- **The engine's metrics report the connector only once traffic has been scheduled** → then the
  init-time attributable signal does not exist on that engine version, and an idle deployment sits
  at `Unknown` / `NoObservationAvailable`. That is the designed behaviour rather than a gap: the rule
  is never `True` by assumption, and `Unknown` is what "nobody can say" looks like. T9 records, per
  engine, which of the two shapes was observed.
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
bash cases/case-47.sh    # same Binding shares; the isolation half is deferred, and says why
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
    model_deployment_config.go         # the client ConfigMap and the connector wiring (T14)
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
	// Resources is what one replica asks of an accelerator, and it is a STRUCTURED FIELD for the
	// same reason Replicas and InstanceType are. It carries only the ACCELERATOR half: CPU, memory
	// and ephemeral storage are DERIVED from the InstanceType's per-unit resources scaled by the
	// card count, so they are not expressible here — a stronger guarantee than refusing them, since
	// a field that does not exist cannot be shadowed by a template either.
	//
	// InstanceType alone cannot supply this: its UnitResources size ONE card, and how many cards a
	// replica wants is a property of the model served rather than of the pool it is admitted against.
	Resources *ModelDeploymentRoleResources `json:"resources,omitempty" protobuf:"bytes,4,opt,name=resources"`

	ExtraArgs []string `json:"extraArgs,omitempty" protobuf:"bytes,5,rep,name=extraArgs"`

	// Env is appended the same way and refused on the same terms. Keys the operator merely defaults
	// — MC_TE_METRIC — are not owned: a user's value wins and no rejection follows.
	//
	// +listType=map
	// +listMapKey=name
	Env []InstanceEnvVar `json:"env,omitempty" protobuf:"bytes,6,rep,name=env"`

	// Template overlays the rendered container. The operator renders first and merges this on top.
	// A non-empty Command is the TAKE-OVER tier: the user owns the whole argv, the operator
	// synthesizes no engine arguments, the role is marked Unmanaged and CacheAttached goes Unknown.
	// Arguments fold into Command; there is deliberately no Args, because a second append tier
	// beside ExtraArgs would have no defined precedence.
	//
	// Unlike the Instance that shares this type, the template is MUTABLE — that immutability is a
	// rule the Instance webhook enforces on InstanceSpec, not a property of InstanceTemplate.
	Template *InstanceTemplate `json:"template,omitempty" protobuf:"bytes,7,opt,name=template"`
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

- [x] **T1 · Land the API type and get `make generate` green, nothing else**
  Blocked by: none
  Delivered: `api/worker/v1alpha1/model_deployment.go` plus the generated set under
  `api/worker/**` and `pkg/kubeclients/**`. No commit hash is recorded here on purpose: the ship
  flow squashes this branch by module, so any hash written into this file would stop resolving.
  Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/zz_generated.*`,
  `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/generated.pb.go`
  Gate: review
  Acceptance: `ModelDeployment`, `ModelDeploymentList`, `ModelDeploymentSpec`,
  `ModelDeploymentKVCache`, `ModelDeploymentRole`, `ModelDeploymentStatus`,
  `ModelDeploymentRoleResources`, `ModelDeploymentRoleStatus` and `ModelDeploymentKVCacheStatus`
  exist with the markers in Code
  Style; the status reuses `api/v1.Condition` and declares **no** new condition type; `roles` carries
  `minItems=1` and **no** `maxItems`; there is no domain field anywhere in the spec; there is no
  `Args` field. Nothing else changes — no controller, no webhook, no `setup.go` entry. The generated
  diff is additive and its only surprise is the group's first `api/v1` protobuf reference, which is
  read and confirmed rather than skimmed.
  Verify: from the main checkout `make generate`, sync back, then `make generate && git diff
  --exit-code`; `go build ./api/...`; `git diff --stat api/` shows the new file plus regenerated
  files and nothing under `pkg/`.

- [x] **T2 · The validating webhook: every shape rule**
  Blocked by: T1
  Delivered: `pkg/worker/webhooks/worker/model_deployment.go` and its test, registered in
  `pkg/worker/webhooks/setup.go`. **Partially:** the `poolRef` existence rule and the two
  InstanceType-dependent resource rules are not written, because both need a client and a type this
  branch does not have yet; they land with T3.
  Owns: `pkg/worker/webhooks/worker/model_deployment.go`,
  `pkg/worker/webhooks/worker/model_deployment_test.go`,
  `pkg/worker/webhooks/worker/zz_generated.webhooks.go`, `pkg/worker/webhooks/setup.go`
  — the last one was missing from this list and is not optional: the generated configuration
  registers the webhook with the API server, and this hand-written list is what registers the
  handler with the manager. A webhook present in one and absent from the other is a configuration
  the API server honours by calling a path nothing serves.
  Gate: review
  Acceptance: one **validating** webhook and no mutating one (defaults are schema markers).
  It enforces: `roles` length exactly 1, rejected with a message naming
  `specs/*-pd-atomic-admission.md` as the spec that lifts it; `extraArgs`/`env` carrying an owned key
  rejected, naming the key, reading the owned-key table rather than a hard-coded list;
  `template.resources` rejected, naming `roles[].resources`; a role asking for both a partition
  profile and a slice percentage rejected; a `poolRef` naming no Binding in this
  namespace rejected with an actionable message. The length-1 rule is a separable predicate, so the
  next spec lifts it by deleting one call — asserted by a unit test that calls the remaining
  validation with two roles and gets no error.
  Verify: `go test ./pkg/worker/webhooks/worker/...`; from the main checkout `make generate`, then
  `git diff pkg/worker/webhooks/worker/zz_generated.webhooks.go` shows the ModelDeployment
  registration **and nothing else**.

- [x] **T3 · Resolve `poolRef`, read the domain, own `DomainRegistered`**
  Delivered: `pkg/worker/controllers/worker/model_deployment_binding.go` and its test; the reading
  folds into `model_deployment_status.go`'s one compute function and the reconciler gained a watch on
  `KVCachePoolBinding`.
  **Beyond the acceptance below, and not optional:** the deployment writes itself into the Binding's
  `status.usedBy`. That list's API declares `ModelDeployment` its **only** writer and the Binding's
  finalizer refuses to release a Binding the list is not empty on — so until something writes it, the
  refusal enforces over a list that is always empty, which is the harm it was built to prevent. The
  acceptance below only presupposed the entry (it forbids *removing* it on a `False`); the writing was
  never spelled out. Claimed on every pass that can read the Binding **including an unusable one**,
  released only once the last replica has left.
  Three reasons rather than two: `BindingDeleting` is its own, because it sends a reader somewhere
  else entirely — find who deleted the authorization, rather than wait for it or look at the pool.
  `status.kvCache` is **retained** when the Binding cannot be read at all, and a teardown pass passes
  no reading rather than an empty one: the record of which cache the replicas are still writing into
  is what the field exists for, and the condition is what says the reading is stale.
  Usability reads **two** independent facts — the Binding's own phase and the `QuotaGranted` axis
  directly — because each covers the other's failure mode: a copied axis list here would miss an axis
  added later while still reporting usable, and the phase alone would miss a regression on the one
  axis this spec was burned by.
  Blocked by: T1, **and the pool spec landing `KVCachePoolBinding` as a Go type**
  Owns: `pkg/worker/controllers/worker/model_deployment_binding.go` + its test
  Gate: the `KVCachePoolBinding` type compiles
  Acceptance: a function resolves the named Binding in the deployment's own namespace, reads its
  immutable domain block, and returns the `status.kvCache` projection (`binding`, `pool`, `domain`
  with `name`/`blockSize`/`dtype`). Missing → `DomainRegistered=False`, reason `BindingNotFound`;
  present but not Ready → `False`, reason `BindingNotReady`. **Readiness is the Binding's `Ready`
  INCLUDING its `QuotaGranted` axis** — a Binding reporting a granted quota of zero, or no ledger
  entry, is not usable however its phase reads (see Notes). And because a store leader restart makes
  every Binding not-Ready for a measured 3.5–32 seconds, this task does **nothing irreversible** on
  a `False`: no `usedBy` removal, no permanent error, no teardown. A test drives Ready → not-Ready
  → Ready and asserts the deployment converges without having dropped anything; deleted under a running deployment →
  `False` with the Pods **left running**. No cross-namespace read is attempted, asserted by a fake
  client that fails the test if asked for another namespace.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentBinding`

- [x] **T4 · The reconciler: one Pod per replica, converge, own them**
  Delivered: `pkg/worker/controllers/worker/model_deployment.go` and its test, registered in
  `pkg/worker/controllers/setup.go` and asserted there by `pkg/worker/controllers/setup_test.go`.
  **Partially:** the recreate path logs the replica it rebuilds but records no Event yet — the
  recorder and the lease-window wording arrive with T10, which owns every replica-leaving event.
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
  Verify: `go test ./pkg/worker/controllers/ ./pkg/worker/controllers/worker/ -run ModelDeployment`.

- [x] **T5 · The owned-key table and the three-tier merge**
  Delivered: `pkg/worker/controllers/worker/model_deployment_render.go` and its test.
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

- [x] **T6 · Connector synthesis for the three engines**
  Delivered: `pkg/worker/controllers/worker/model_deployment_connector.go` and its test — the
  owned-key table, the per-engine client config, and each engine's own launch command.
  Blocked by: T1 — **not T3**: the function takes the domain and the pool endpoints as plain values,
  so it is written and tested against a local input struct that T3 later fills from the Binding.
  Owns: `pkg/worker/controllers/worker/model_deployment_connector.go` + its test
  Gate: review
  Acceptance: a pure function over (engine, backend type, domain, pool endpoints) returns the
  argument, the config-path environment variable and the client JSON, matching F4's tables for
  `vllm`, `vllm-ascend` and `sglang`, asserted against one golden fixture per engine. **The JSON
  carries exactly the keys the selected engine's own reader reads and no others** — the three key
  sets differ, and rendering a key an engine ignores documents a wiring that is not happening. The
  device list is the JSON's `device_name`, not `setup()`'s `rdma_devices` nor Mooncake's
  `MOONCAKE_DEVICE`. It sets **no `local_hostname`** and **no `tenant_id`** (unreachable on all
  three, F4), each absence asserted by a test that carries its reason. `MC_TE_METRIC=1` is set as a
  **defaulted** key. The JSON is rendered into one deployment-owned ConfigMap mounted read-only into
  every replica.
  **Two defects were found after this task was ticked, and both are now fixed in place rather than
  deferred.** They are recorded here because the acceptance above states the reasoning that produced
  them, and that reasoning is what a reader would otherwise reuse.

  1. **It rendered a file carrier for every engine, and `sglang` must not have one.** The
     `local_hostname` claim above holds for the two vLLM-family readers only. `sglang` reads that
     key from the file — and from the extra-config argument, whose loader is key-for-key identical —
     falling back to the literal `localhost`, so a file-carried SGLang deployment registered every
     replica under one identity. F4 now carries the carrier split and the evaluation-time reason
     behind it.
  2. **It set neither `global_segment_size` nor `local_buffer_size` nor `mode`, "because the
     operator inventing them is a silent capacity error". That reason is backwards** — the group
     declares a ROLE, and omitting it makes every engine Pod a 4 GiB in-process store member instead
     of a pure client (F4 carries the measurement).

  **Both were originally deferred to the connector-wiring task, and that deferral was wrong for the
  same reason in both cases:** the remaining tasks before ship do not depend on the shared package,
  so deferring meant shipping the defects. The "two fixes need two verifications" objection also
  overstated the cost — the shared package's assertions for these keys already exist, so
  consolidation now deletes a duplicate instead of moving an unverified implementation.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentConnector`

- [x] **T7 · One Service, one endpoint**
  Delivered: `pkg/worker/controllers/worker/model_deployment_service.go` and its test.
  It fronts `roles[0]`, which is the only reading with one role; the P/D spec decides which of
  several roles is the front door, knowing what a router before them means.
  Blocked by: T4
  Owns: `pkg/worker/controllers/worker/model_deployment_service.go` + its test
  Gate: review
  Acceptance: one `ClusterIP` Service per deployment, owned by it, selecting exactly the role's Pods;
  `status.endpoint` = `http://<name>.<namespace>.svc:<port>` where the port is the template's first
  `Ports` entry or 8000. Scaling changes the Service's endpoints without recreating the Service. A
  test states why this is not `instance.go`'s per-Pod NodePort shape: N interchangeable replicas
  behind one address, not one addressable box.
  Verify: `go test ./pkg/worker/controllers/worker/ -run 'ModelDeploymentService|ModelDeploymentEndpoint|AlignModelDeploymentService'`

- [x] **T8 · Status: phase, per-role readiness, `QuotaReserved`**
  Delivered: `pkg/worker/controllers/worker/model_deployment_status.go` and its test.
  `DomainRegistered` and `CacheAttached` are deliberately **not written**, not even as `Unknown`:
  there is no Binding to resolve and no connector to scrape yet, and a condition written before
  anything can observe it reports a state nothing measured. They arrive with T3 and T9, folding into
  the same compute function — a second status writer would be free to leave its own field behind.
  Blocked by: T4
  Owns: `pkg/worker/controllers/worker/model_deployment_status.go` + its test
  Gate: review
  Acceptance: status is **rebuilt from observed state every reconcile**, so a stale field cannot
  survive a disagreement with the Pods. `phase` takes `Starting`/`Ready`/`Degraded`/`Deleting` — the
  `Instance` vocabulary rather than a new one — with `Ready` meaning every role's `ready == desired`
  and `Degraded` meaning some ready and some not. `roles[]` carries `name`, `desired`, `ready`,
  `unmanaged`. `QuotaReserved` is `True` only when every replica's Workload has quota reserved, and
  its `False` reason names the ClusterQueue.
  Verify: `go test ./pkg/worker/controllers/worker/ -run 'ComputeModelDeployment|ObserveModelDeployment|SyncModelDeployment'`

- [x] **T9 · `CacheAttached`, judged on an observation**
  Delivered: `pkg/worker/controllers/worker/model_deployment_cache_attached.go` and its test; the
  reading folds into `model_deployment_status.go`'s one compute function.
  **The per-engine measurement this task was asked for came back negative, and F8 was rewritten
  from it:** no supported engine publishes anything about its KV connector before the first store
  operation, so the predicate this acceptance named ("reports its KV connector initialized",
  traffic-free) does not exist. The measurement, the one link that could have rescued it
  (`# HELP`/`# TYPE` for a childless labelled counter) and why `MultiProcessCollector` breaks that
  link are all recorded in F8. The consequence is that `NoCacheActivity` splits into
  `NoObservationAvailable` (`Unknown`, because the state it would report has a nearer observer) and
  `CacheOperationsFailing` (`False`, the one state with no other observer), and that there is **no
  observation window** — neither replacement row needs one.
  The reading type therefore cannot express a traffic-free "initialized" value at all, which is what
  keeps the fake scraper from being more capable than what it stands in for.
  Blocked by: T3, T5, T8
  Owns: `pkg/worker/controllers/worker/model_deployment_cache_attached.go` + its test
  Gate: review
  Acceptance: the condition follows F8's table exactly. The signal is **the engine's own metrics
  endpoint, scraped per replica at the Pod's address, accounting for its store operations**. Where no
  replica gives an account, the Binding's `usage`/`blocks` corroborate; where neither can be read the
  condition is `Unknown` / `NoObservationAvailable`. **An absent (nil) `usage`/`blocks` is `Unknown`,
  never `False`** — and so is an observed zero, because an attached idle domain holds nothing either.
  It is **never** `True` because a flag was rendered or a log line echoed it. A role marked
  `unmanaged` yields `Unknown` / `Unmanaged` whatever is observed. The scraper is an **interface the
  reconciler takes**, not a dial it makes, because every case that matters here is a failure and a
  real dial cannot be made to fail on demand.
  Two deterministic negative tests live here: a fake scraper reporting, on every replica, store
  operations of which none succeeded, over a deployment whose Binding holds nothing, yields `False` /
  `CacheOperationsFailing`; and the same deployment sharing its tenant with a healthy sibling that
  *is* filling the domain still yields `False`, which is the assertion that pins the reading to a
  per-replica signal.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentCacheAttached`

- [x] **T10 · Losing a replica: the event and the lease window**
  Delivered: `pkg/worker/controllers/worker/model_deployment_events.go` and its test, which also
  completes the half T4 left as a log line.
  Blocked by: T8
  Owns: `pkg/worker/controllers/worker/model_deployment_events.go` + its test
  Gate: review
  Acceptance: a replica leaving — preempted, evicted, restarted or deleted — records an event on the
  `ModelDeployment` naming the replica and the 30 s `kv_lease_duration` window, so an operator
  correlating failed requests with a preemption has the correlation written down. The event is
  emitted from observed Pod state rather than from a delete hook, so it survives a missed watch
  event.
  Verify: `go test ./pkg/worker/controllers/worker/ -run ModelDeploymentEvents`

  **Two consequences of reading departures from observed state, both found by implementing it.**
  A departing replica has to still be there to be read, so the reconciler deletes replicas with
  their own grace period and **never** with `ctrlclix.Terminated` — which `instance.go` uses, and
  which is the obvious thing to copy. And a name still held by a terminating replica must not end
  the pass early: it did, which meant that during exactly the window a departure event is for,
  neither the event nor the status was written. The pass now carries the requeue to the end.

  The `30s` in the message is stated as `kv_lease_duration`'s **default**, not read from the
  pool. The value lives on the backend, which T3 resolves; naming a number this operator did not
  read would be worse than naming the default and saying so.

- [x] **T11 · Documentation**
  Blocked by: T2, T5, T7, T9
  Owns: `docs/reference/model-deployment.md`, `docs/README.md`, `docs/architecture.md`
  Gate: review
  Acceptance: a reference page states the CR's contract — the Binding-inherited domain and *why* a
  workload cannot name one, **plus one plain sentence saying the domain is not yet enforced at the
  storage layer on any supported engine and what that means for two deployments on two Bindings**
  (F4), because a user who reads only the docs must not conclude they have isolation they do not
  have; the three override tiers with the owned-vs-defaulted key distinction and
  the full owned-key table; `CacheAttached`'s predicate and every reason string; the recreate rollout
  policy and its cache cost; the **port-range, not port-list** NetworkPolicy requirement; and the
  benign startup error `E transfer_metadata.cpp:991] Local segment descriptor not found` with a line
  saying it is expected, so users stop filing it. `docs/README.md` gains the index entry and
  `docs/architecture.md`'s life-of-a-request table gains the row for a Pod a `ModelDeployment`
  renders. Routed through the `gpustack-operator-docs` skill.
  Verify: `make lint docs`

  Landed as `docs/reference/model-deployment.md`, 338 lines, with the index row and one clause plus
  one link on `docs/architecture.md`'s Submit row — the overview is the front door and stays capped,
  so the mechanism lives on the deep page.

  **Writing it found three stale rows in F5's own table above**, which T16 should have carried and
  did not: a `vllm-ascend` row for an engine value T16 deleted, an SGLang environment column listing
  one name where the code owns seven, and a `MOONCAKE_TENANT_ID` that no client reads. All three are
  corrected there. A normative table that contradicts the code is worse than no table, because it is
  the thing a reader trusts.

  The page's two example image tags are checked against the runner project's 338 published records
  rather than composed. One of them was wrong: `cann8.2-910b-sglang0.5.18` is not published and
  `cann9.0-910b-sglang0.5.18` is.

- [ ] **T12 · e2e: the functional cases**
  Blocked by: T2, T5, T7, T9, T10, **T14** — case-45's inference path and case-48's whole premise
  need a connector that is actually rendered
  Owns: `.claude/skills/gpustack-operator-e2e/cases/case-45.sh`,
  `.claude/skills/gpustack-operator-e2e/cases/case-47.sh`,
  `.claude/skills/gpustack-operator-e2e/cases/case-48.sh`,
  `.claude/skills/gpustack-operator-e2e/SKILL.md`
  Gate: a two-node cluster with two consumer GPUs on one node
  Acceptance: **case-45** — `replicas: 4`, one role, reaches `status.roles[0].ready == 4` and
  `status.endpoint` serves inference; the rejections all fire (two roles with the message naming the
  spec, an owned key in `extraArgs`, `template.resources`, a missing Binding, a cross-namespace
  `poolRef`, a self-declared domain). **case-47** — two deployments on the **same** Binding share
  blocks; the isolation half is deferred with `tenant_id`'s unreachability as the stated reason, and
  asserts the gap's own signature so that the case fails when the gap closes. **case-48** — the
  deliberate break:
  an image without the matching per-vendor wheel, and separately a Binding pointing at an unreachable
  pool; the assertion is `CacheAttached != True` in both, and the case **records which shape the
  engine took**. Each case reports its verdict the way the suite already does, which is a convention
  rather than a library: a `record` helper appending `STATUS|CHECK|OBJECT` rows to a local array, a
  `STATUS | CHECK | OBJECT` table at the end split on that delimiter rather than on whitespace, and
  `exit 1` when any row failed. There is no `lib.sh` to import; the cases carry those few lines
  themselves. Every refusal row must also assert a fragment of the operator's own
  message, because the schema refuses an incomplete sample before the webhook ever sees it, and a
  row that only checks for rejection would keep passing with the webhook deleted. The chart guard
  rides here: after `helm install`, the `modeldeployments` CRD is present —
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

- [ ] **T14 · Wire the synthesized connector into the replicas**
  **A task this list did not have, and its absence was load-bearing.** T6 delivers connector
  synthesis as a pure function; T3 delivers the Binding resolution. **Nothing owned the hop between
  them**, so the renderer is called with no connector at all: no engine argument, no config-path
  variable, no ConfigMap. Every replica therefore runs with the cache unattached, which makes
  `CacheAttached` unable to be `True` on a cluster however correct T9's predicate is, and makes
  case-45, case-46 and case-48 unable to pass. It is numbered last rather than inserted at T7 so that
  no existing task is renumbered; a task list is a DAG and this one's edges say where it belongs.
  Blocked by: T3, T5, T6, **and `pkg/worker/kvcache/inject` existing** — see the ownership rule
  below.
  Owns: `pkg/worker/controllers/worker/model_deployment_config.go` + its test; the render input's
  `Connector`/`ClientConfigName` fill in `model_deployment.go`
  Gate: the shared rendering package exists
  Acceptance: one ConfigMap per deployment, owned by it, carrying the selected engine's client JSON,
  mounted read-only into every replica of every role, and the synthesized argument and config-path
  variable reach the container through T5's merge. A role that took over the command line gets
  **none** of it. Changing the pool endpoint or the domain re-renders the ConfigMap and the replicas
  are recreated to pick it up under F10's recreate policy — **but the Pod spec hash does not move on
  its own, so an acceptance resting on it cannot be tested.** A ConfigMap reaches a Pod as a *name*
  (a `ConfigMapVolumeSource` over a `LocalObjectReference`), so re-rendering its contents leaves
  `core.PodSpec` byte-identical, and the hash's subject is `{Labels, Annotations, PodSpec}`. This is
  written out rather than deleted because the wording is the kind that gets reinvented: an e2e
  asserting it would go **green** while the replicas kept the stale config, and would read as if it
  had established that no recreate is needed.
  ⇒ So this task also writes a digest of the rendered client JSON into a **Pod annotation**, which is
  in the hash's subject and therefore does move it. Putting the digest in the ConfigMap's *name* was
  rejected: it turns every content change into a new object and hands this controller a
  garbage-collection duty it does not otherwise have. Should that route ever be taken, the only safe
  predicate for deleting an old ConfigMap is **"no Pod still references it"** — never "older than the
  current version", which deletes an object a terminating replica is still mounting.
  A deployment whose Binding cannot be resolved renders **no** connector rather than a partial one:
  a client pointed at an address that does not answer is a worse failure than one that was never
  configured, because the first looks like a cache miss.

  **The client JSON is rendered by `pkg/worker/kvcache/inject`, which is its sole owner across
  specs, and this task DELETES the copy in `model_deployment_connector.go`.** Two specs were about to
  render the same configuration — an injection webhook and this CR — and the alternative on the table
  was an equivalence test between them. A test *distinguishes* divergence; one implementation
  *eliminates* it, and the risk here is two client behaviours on one pool, which is not something to
  guard with a test. The gap has the same shape as the one this task exists for: nothing owned the
  hop between synthesis and the renderer here, and nothing owned *the rendering itself* across specs.
  ⇒ If the package is not there when this task starts, coordinate rather than writing a second copy
  "to merge later".

  Four things the shared renderer has to get right, each measured rather than assumed:
  - **The carrier is per engine**, as F4's carrier table records it: a file for `vllm` and
    `vllm-ascend`, `MOONCAKE_*` environment variables for `sglang` with `MOONCAKE_LOCAL_HOSTNAME`
    from `fieldRef: status.podIP`, and **no** `--hicache-storage-backend-extra-config`.
    **This is already implemented in the CR's own renderer, so the risk here runs the other way:**
    a shared package modelled on the file carrier alone would reintroduce the defect while looking
    like a consolidation. The shared package's interface therefore has to be able to return
    environment variables — including one with a `ValueFrom` rather than a value — and not just a
    config document. A signature returning only a document cannot express this, and would force the
    caller back to a file.
  - **The pure-client group**, per engine, as F4 records it — **also already implemented here, so
    this too is a property to preserve rather than to add.** `global_segment_size: 0` on all three.
    A `local_buffer_size` of **128 MiB** is the documented client staging size (the store's own
    `setup` example spells `128*1024*1024 # local_buffer_size (128MB)`, described as short-lived
    client-side staging), not a number picked for looking small — **and it is a vLLM-family key
    only.** `sglang` has no such key; it passes a hardcoded 16 MiB to `setup()`, so rendering the
    128 MiB for it writes a key nothing reads. `mode: standalone-store` is vLLM's alone and cannot
    be split from the segment size, in either direction.
  - **The size keys render as JSON numbers, not strings.** All three engines run them through a
    parser that accepts `"0"` as well, so a string works today and the difference is invisible in
    testing — which is exactly why it is pinned: if this renderer emits strings and the shared one
    emits numbers, consolidation changes the rendered document inside what reads as a deletion.
    Both sides emit numbers, so it stays a deletion.
  - **`protocol` comes from `mooncake.MemberProtocol(kvcb)`**, never from reading
    `spec.transport.protocol` here. That function already falls back to `Auto` for an empty value
    (`member_workload.go:142-152`, because the artifact looks the protocol up in a transport map and
    an empty string finds nothing) — so a second parser would not merely risk drifting, it would
    start out missing that fallback.
  - **`metadata_server` has exactly one definition.** It is the literal `P2PHANDSHAKE`, which this
    scope's metadata plane takes unconditionally; after the merge there must not be two constants of
    the same value in two packages.
  Verify: `go test ./pkg/worker/controllers/worker/ -run 'ModelDeploymentConfig|ModelDeploymentRender'`

- [x] **T15 · Aggregate the observed runtime version, then synthesize the image from it**
  Delivered in three commits: the `InstanceType` field and its aggregation; the full version list
  that a Warning Event needs; and the synthesis itself with the event and the watch.
  `pkg/worker/controllers/worker/model_deployment_image.go` holds both translation tables and the
  formula; `instance_type.go` holds the aggregation; the event and the `InstanceType` watch are in
  `model_deployment_events.go` and `model_deployment.go`.
  **Two things this task's plan had wrong, both found by doing it:**
  1. **Its stated dependency was on T5 alone, and it is also on T16** — the formula consumes
     `spec.engineVersion`, which T16 adds. The execution order was T15's first commit, then T16,
     then the rest of T15. **The edge is corrected here rather than only in what was executed**: a
     DAG with a wrong edge sends the next person who schedules from it into the same wall, and they
     will not know it has already been hit.
  2. **Publishing only the minimum could not support the Warning Event it was paired with.** The
     event has to name what was skipped, and a single value cannot. The full list was added, and
     with it the invariant that the single value is the list's first element — held by construction
     (one sorted list, one assignment) rather than by a rule someone has to remember.
  Blocked by: T5 — the three-tier merge is where a synthesized image has to lose to a stated one —
  **and T16**, for `spec.engineVersion`
  Owns: `api/worker/v1alpha1/instance_type.go` (one new observed field),
  `pkg/worker/controllers/worker/instance_type.go` (the aggregation),
  `pkg/worker/controllers/worker/model_deployment_image.go` + its test,
  one event reason in `pkg/worker/controllers/worker/model_deployment_events.go`, and the
  `InstanceType` watch plus its mapper in `pkg/worker/controllers/worker/model_deployment.go`
  Gate: none — both sources already exist (`Devices.Groups[].RuntimeVersion`, and the manufacturer
  and family already on `InstanceTypeDetail`)
  **Two commits, and the split is a requirement rather than tidiness:** the `InstanceType` field is a
  change to another spec's object and drags generated artifacts, so it lands alone with
  `make generate` run in the same commit; the synthesis lands second on top of it.
  Acceptance: the formula and both mapping tables of F11, with the cases that are not
  round-trippable spelled out. The aggregation walks the same structure `poolAcceleratorSlicedDetail`
  already walks — every node's `Devices` ledger, groups matched on the full
  `${manufacturer}-${group ID}` key — and takes `RuntimeVersion` where that function takes
  `Accelerators`, so a group that matches nothing yields **no** version rather than an empty string
  that reads like a version of `""`. A role whose `InstanceType` has no `runtimeVersion` renders no
  image and says why; a role naming `template.image` gets it verbatim and synthesis does not run; a
  role on a manufacturer with no runner backend (`cambricon`) or an Ascend family with no published
  variant (`910`, `310B`) is refused **at render time** naming the manufacturer or the family — not
  by the webhook, which cannot read a status that has not converged yet (F11 states why).
  The mapping tables are data, not a `strings.ToLower` call — `910C` maps to `a3`, and **a test that
  passes with a lowercasing implementation is not testing the table**, so each row is pinned as a
  literal. `cambricon` and the unmapped Ascend families are **refused**, not rendered with an empty
  image: an empty image fails as an `ImagePullBackOff`, whose symptom is far from its cause, while a
  refusal carries the reason.
  The aggregate is the **minimum across the pool**, and a pool whose nodes disagree also produces a
  **Warning Event on the `ModelDeployment`** naming the value taken and the others present — the
  event exists because the minimum's failure mode is otherwise untraceable to the node that caused
  it. Its input is entirely local (the `Devices` ledger), which is what makes it buildable where the
  `deprecated` warning is not.
  Verify: `go test ./pkg/worker/controllers/worker/ -run 'ModelDeploymentImage|InstanceTypeDetail'`,
  plus a case asserting the event fires on a mixed pool and **does not** fire on a uniform one

- [x] **T16 · Narrow the engine enum onto the dimension the connector actually varies with**
  Delivered: `spec.engine` is `{vllm, sglang}`, `spec.engineVersion` is added, and connector
  selection takes `(engine, manufacturer)`.
  **The ownership table became the evidence for the whole change.** One entry now covers the vLLM
  family on every backend — and when it had two, their values were *identical*, because owned keys
  follow the engine while only the connector name follows the backend. The invariant test that
  reads the renderer's real output runs over all three `(engine, backend)` pairs, so a key the
  Ascend backend renders that is missing from vLLM's entry fails there. **The table no longer has to
  be remembered; it reddens on its own.**
  Blocked by: T15's first commit only, to keep two API-plus-generate changes off each other
  Owns: `api/worker/v1alpha1/model_deployment.go` (the enum and the new `engineVersion`),
  `pkg/worker/webhooks/worker/model_deployment.go`,
  `pkg/worker/controllers/worker/model_deployment_connector.go` + its test
  Acceptance: `spec.engine` becomes `{vllm, sglang}` and `ModelDeploymentEngineVLLMAscend` is
  deleted; connector selection takes `(engine, backend)` instead of `engine`, with the backend read
  from the role's `InstanceType` detail; the owned-key table is keyed the same way. A `(vllm, cann)`
  case asserts `AscendStoreConnector` and a `(vllm, cuda)` case asserts `MooncakeStoreConnector`, so
  the table is exercised on both sides of the branch that used to be a third enum value.
  `spec.engineVersion` is added as required and free-form: **no format validation, no cross-check
  against the engine, and no list of known versions**, per the Non-Goal.
  **Deleting an enum value is allowed here for a reason worth stating:** `ModelDeployment` is created
  by this spec and has never been in a tagged release or on `main`, so the enum has no stored objects
  and no downstream consumer. That is the whole of the licence — it does not extend to
  `InstanceType`, whose new field in T15 is therefore purely additive.
  Verify: `go test ./api/... ./pkg/worker/... -run 'ModelDeployment'`, and `make generate` leaves the
  tree clean

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/worker/controllers/worker` needs a fake `KVCachePoolBinding` and a fake per-replica connector
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
| `env_owned_key` | `MOONCAKE_CONFIG_PATH` in `env` | reject |
| `env_owned_key_in_template` | the same key in `template.env` | reject; the message names **`roles[].template.env`**, not `roles[].env` |
| `env_owned_key_in_template_take_over` | the same, with `template.command` set | reject — the renderer drops owned keys unconditionally, so admission refuses unconditionally |
| `env_unowned_key_in_template` | `HF_HOME` in `template.env` | accept |
| `env_defaulted_key` | `MC_TE_METRIC=0` | accept; the user's value wins |
| `template_resources` | `template.resources` set | reject; names `roles[].resources` |
| `resources_accelerator_only` | a card count alone | accept |
| `resources_sliced_percentages` | a card count plus both slice percentages | accept |
| `resources_partition_profile_only` | a card count plus a profile | accept |
| `resources_partition_and_slice_together` | a profile plus a slice percentage | reject; one accelerator cannot serve both |
| `resources_absent` | no resources block | accept — a CPU-only replica is legitimate |
| `template_command` | `template.command` non-empty | accept; take-over |
| `pool_ref_missing` | names no existing Binding | reject; the message names namespace + Binding |
| `pool_ref_name_empty` | `poolRef: {name: ""}` | reject — **from the webhook**, because the field's type is upstream's `core.LocalObjectReference` and a `minLength` marker cannot be attached to a struct this API does not own. Every other required string gets its lower bound from the schema |
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
| `vllm_golden` | engine `vllm` | `--kv-transfer-config` selecting `MooncakeStoreConnector` with `kv_role: kv_both`, plus `MOONCAKE_CONFIG_PATH` |
| `vllm_ascend_golden` | `(vllm, cann)` | `--kv-transfer-config` selecting `AscendStoreConnector` — **not** `MultiConnector`, which is that project's composite. Keyed on the backend, so it also pins that `cann` is what selects this row |
| `sglang_golden` | engine `sglang` | `--hicache-storage-backend mooncake`, the `MOONCAKE_*` environment set, and **no** config-path variable and **no** `-extra-config` |
| `keys_are_exactly_this_engines` | each engine | the carrier holds exactly the key set that engine's reader reads — no key it ignores, no key it needs missing |
| `device_json_key` | any engine | the device list is the JSON's `device_name`, never `rdma_devices` or `MOONCAKE_DEVICE` — the two spellings the other surfaces use |
| `no_local_hostname_on_file` | `vllm`, `vllm-ascend` | `local_hostname` is **absent**; those two compute it per process and one file cannot hold a per-replica value |
| `local_hostname_is_a_fieldref` | `sglang` | `MOONCAKE_LOCAL_HOSTNAME` is **present** and valued from `fieldRef: status.podIP`. **A literal fails the case** — including a correct-looking one, because the defect being avoided is a literal that happens to parse |
| `no_configmap_for_sglang` | `sglang` | **no** ConfigMap is created for the deployment at all; an unused one would claim a wiring that is not happening |
| `no_extra_config_argument` | `sglang` | `--hicache-storage-backend-extra-config` is **absent**. Its loader is key-for-key identical to the file loader and sits at *higher* precedence, so passing it would make the environment carrier unreachable |
| `no_tenant_id` | any engine | `tenant_id` is **absent**, because no supported engine passes it to `setup()`; the test states the reason so its deletion is deliberate |
| `pure_client_group` | each engine | the coherent group, **asserted positively per key** because the failure mode is a MISSING key and an absence assertion cannot catch one going missing. The three differ: `vllm` gets `mode: standalone-store` + `global_segment_size: 0` + `local_buffer_size` 128 MiB; `vllm-ascend` the same without `mode`; `sglang` gets `MOONCAKE_GLOBAL_SEGMENT_SIZE=0` on its own carrier **and no `local_buffer_size` at all** — it passes a hardcoded 16 MiB to `setup()`, so that key would be one no reader reads |
| `sizes_are_json_numbers` | `vllm`, `vllm-ascend` | the two size keys render as numbers, not strings. Both work today, so this pins the choice rather than a behaviour: the shared package models them as `int64`, and a mismatch would let consolidation change the document inside what reads as a deletion |
| `config_path_env_per_engine` | each engine | `vllm`/`vllm-ascend` get `MOONCAKE_CONFIG_PATH`, `sglang` gets `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` |
| `mc_te_metric_default` | user set nothing | `MC_TE_METRIC=1` present |
| `mc_te_metric_user_off` | user set `MC_TE_METRIC=0` | the user's value, no duplicate entry |
| `one_configmap_per_deployment` | 4 replicas | one ConfigMap, mounted read-only into all four |

**`CacheAttached` cases** (`pkg/worker/controllers/worker`) — the negative cases are why this
condition exists.

| Case | Condition | Expected |
|---|---|---|
| `replica_reports_success` | a replica accounts for succeeding store operations | `True` / `CacheActive` |
| `one_succeeds_one_fails` | one replica succeeding, one failing | `True` / `CacheActive` — the cache IS in effect; the failing replica is a per-replica fault |
| `every_replica_failing` | every ready replica accounts for operations of which **none** succeeded | `False` / `CacheOperationsFailing` — the one state with no other observer |
| `no_account_anywhere` | no replica gives an account, Binding reports nothing held | `Unknown` / `NoObservationAvailable` — an attached idle deployment looks exactly like this |
| `endpoints_do_not_answer` | every scrape errors | `Unknown` / `NoObservationAvailable` |
| `no_scraper_wired` | the reconciler has no scraper at all — today's shipped state | `Unknown` / `NoObservationAvailable`, **not** `False` |
| `no_replica_ready` | no replica Ready | `Unknown` / `NoReplicaReady` |
| `only_ready_replicas_asked` | one Ready, one not Ready, one Ready-and-terminating | only the first is scraped |
| `engine_unscrapeable_binding_holds_data` | no account; Binding's `usage`/`blocks` observed above zero | `True` / `CacheActive` via the corroborating signal |
| `binding_figures_absent_is_not_zero` | `usage`/`blocks` are nil pointers | `Unknown`, **not** `False` — absent means the scrape did not carry this domain |
| `binding_figures_observed_zero` | `blocks` observed as `0` | `Unknown`, **not** `False` — an attached idle domain holds zero too |
| `sibling_holds_the_shared_tenant` | two deployments sharing a tenant; this one's replicas all failing, the sibling's writes fill the domain | `False` — the domain-level figures must not attribute the sibling's data to this deployment |
| `flag_rendered_only` | everything configured, Binding registered, replicas Ready, nothing observed | `Unknown`, **never** `True` — the case the condition exists for |
| `unmanaged_role` | take-over, and the domain does hold data | `Unknown` / `Unmanaged` — the operator claims nothing it did not render |

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

**These are fake-client tests, not envtest.** An earlier draft of this spec said envtest; the
repository has no envtest harness anywhere — every controller test in `pkg/worker/controllers/`
drives the reconciler directly against `sigs.k8s.io/controller-runtime/pkg/client/fake`, with
`interceptor.Funcs` where a call has to be counted rather than inferred. Introducing a second
harness for one CRD would be a project-wide decision this spec has no reason to make.

- The reconciler against a fake Binding: create → four Pods → delete one → converged; scale 4 → 2 →
  4; change the image → recreate; delete the deployment → the finalizer is held until the Pods are
  gone. Garbage collection through the owner reference is the API server's and is not asserted here,
  because a fake client does not run it.
- The webhook called directly: every rejection in the validation table arrives as an `Invalid` status
  error whose message is the one the table names.
- The `CacheAttached=False` path driven by the fake pool status — the deterministic half of the
  deliberate break, so the e2e case is confirming rather than discovering.

#### e2e tests

Run against a local two-node cluster with two consumer GPUs on one node. No RDMA, no cloud.

- **case-45 — replicas reach ready, and every rejection fires.** `replicas: 4`, one role,
  `status.roles[0].ready == 4`, `status.endpoint` serves inference. Then, in one pass: two roles
  rejected with a message naming a spec; an owned key in `extraArgs` rejected; an owned variable
  rejected in **both** tiers that carry one — `env` and `template.env`, the second naming its own
  tier; `template.resources` rejected; a missing Binding rejected; a cross-namespace `poolRef`
  refused; a self-declared domain refused. Plus the chart guard: after `helm install` the
  `modeldeployments` CRD is present, having been installed by the worker rather than by a chart
  manifest.
- **case-46 — the headline.** One request stream with a fixed prefix distribution, replayed against
  a `replicas: 1` deployment and a `replicas: 4` deployment on the same Binding. Asserted: the pool
  shows one domain with blocks contributed by more than one replica, and the four-replica hit rate
  exceeds the single-replica hit rate. **Recorded** in the table below.
- **case-47 — the isolation claim, one half asserted and one half deferred.** Two deployments on the
  same Binding share blocks: asserted. Two on **different** Bindings not seeing each other's:
  **deferred, and the case says why** — `tenant_id` reaches no supported engine (F4), so both land
  in the tenant named `default` and there is no isolation mechanism to test. The case asserts the
  sharing half, and for the isolation half asserts the *observable consequence of the gap* — that
  both deployments' blocks appear under one domain — so that the day an engine starts passing
  `tenant_id`, this case **fails** and tells whoever is looking that the deferral is over. A deferral
  that cannot detect its own end is a deletion. The isolation half remains the one that matters —
  a shared cache that leaks across reuse boundaries is worse than no shared cache — which is exactly
  why the gap is written into the case, the status and the reference page instead of being left for
  a reader to discover.
- **case-48 — the deliberate break.** Two vehicles, run separately: an engine image without the
  matching per-vendor `mooncake-transfer-engine` wheel, and a Binding pointing at an unreachable
  pool. The assertion in both is `CacheAttached != True`; the case records which shape the engine
  took — aborting at init, or serving on without the cache — because that is not predictable from
  the sources and is worth knowing once.

**Not covered by any of the above, and it needs a case of its own.** The controller reconciles the
Service and now watches it, but **nothing asserts the watch**. It cannot be asserted at the unit
level: `SetupController` has no observable output, so a test that builds the controller and checks
nothing would pass with the watch removed — which is worse than no test, by the predicate in
Verification. The honest vehicle is an e2e that **deletes the Service by hand and waits for it to
come back**, and nothing else in this plan exercises that path.

⇒ It is recorded here rather than left to a commit message on purpose: this branch squash-merges, so
a commit message is not a durable channel for a gap that outlives the change. The same applies to
the case-45 row for the overlay refusal, which was added without a cluster to run it on — its rule
is covered by mutation-checked unit cases, but the row's own message-matching has not been observed.

**This paragraph is the basis of record for both gaps.** A pull request body may restate them for
reviewers, and that copy is a summary of this one: where the two disagree, this is the one that is
right, because it is the copy that travels with the code. Naming the basis is what makes the
duplication safe — two descriptions of one fact drift, and without a designated winner the reader
has to guess which is newer.

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
  engine's own report of its connector is per replica and appears at startup, so it is the predicate.
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

- ~~**How does a replica supply `local_hostname` to the client?**~~ **Settled, but not the way this
  entry first recorded, and the correction is the more instructive half.** The worry was real for
  Mooncake's own `MooncakeConfig` — `local_hostname` sits in its `_REQUIRED_NON_EMPTY_FIELDS`,
  `from_file()` raises when the key is absent, and the environment default is the literal
  `"localhost"`. This entry then answered "no supported engine uses that loader; each of the three
  computes it per process", and **that generalized one measurement onto three engines**. It holds for
  `vllm` and `vllm-ascend`. It is false for `sglang`, whose `from_file()` and
  `load_from_extra_config()` both read the key and both fall back to the literal `"localhost"`,
  because `EnvField.default` is a plain attribute that never consults the environment. The answer is
  therefore **per carrier**, not per engine: absent on the file carrier, and required as
  `fieldRef: status.podIP` on the environment carrier (F4).

  This entry's own stated lesson was **"a client's documented configuration surface is not the surface
  its callers use, and this could only be answered by reading the callers."** That lesson was right,
  and the error above is its next layer: the callers were read, but on `sglang` only one of its
  **three** loader paths was, and not the one this design selects. ⇒ **Reading the caller is not
  enough when the caller has more than one path in; the path has to be the one your configuration
  actually arrives on.** Which of `load_from_extra_config` / `from_file` / `load_from_env` runs is
  decided by what the operator renders, so the operator's own choice selects which measurement
  applies — and measuring before making that choice reads whichever path the measurer happened to
  look at.
- **When does `tenant_id` become reachable, and through which engine first?** Enforcement of the
  reuse domain waits on an engine passing `setup()`'s 11th argument (F4). The two vehicles that would
  not wait — a `sitecustomize` shim that wraps `MooncakeDistributedStore.setup`, and a patch carried
  in this project's own per-vendor engine images — were both weighed and left unused here: each
  reaches inside an engine internal that moves faster than this operator releases, which is the kind
  of coupling this CR's whole thinness argument exists to avoid. The open part is which engine lands
  it first, because that decides whether the operator's first enforcement path is a JSON key
  (`sglang`, whose reader already merges three sources) or a `--kv-transfer-config` field. Until
  then, case-47's isolation half stays deferred and asserts the gap's signature so the deferral
  cannot outlive the gap.
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

  The replacement is **the engine's own metrics endpoint accounting for its store operations,
  scraped per replica** (F8) — attributable and downstream of the thing being judged. **It is
  not available without traffic**, which the sentence here originally claimed: that property was
  measured and is absent on all three engines, so this signal shares the second of the two
  limitations named just below rather than escaping both. The domain-level `Usage`/`Blocks` still
  cannot replace it, because they cannot **attribute**: they are shared by every deployment on one
  domain.

  **The better replacement was tried and is unreachable.** The Mooncake store client can serve its
  own `/health` on port 9300 — per client, bound at init, one layer closer to the truth than the
  engine's own report. But `enable_client_http_server` is `setup()`'s 12th parameter and every
  supported engine passes 7 or 8 positionally, with no environment fallback in the C++ and no
  in-process gflags path (F4, F8). It is recorded as the signal to switch to the moment an engine
  passes that argument.
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
- ~~**Should a deprecated runner image be flagged, and with what?**~~ **Settled: no, and the
  question was only askable because two decisions contradicted each other.** F11's premise is that
  the operator reads no release matrix; the `deprecated` flag lives in that matrix. So the intent to
  "accept the image and record a Warning Event" required the premise to be false, and nothing in
  either statement made that visible — each reads fine alone. It surfaced only from asking *where
  the flag physically is*. Deprecation now belongs to whatever surface publishes the matrix; F11
  states the non-goal rather than leaving it silent.
  ⇒ The general form is worth keeping: **two decisions can each be reasonable and jointly
  impossible, and reviewing them one at a time will not show it.** What showed it was locating the
  data each one needs.
- ~~**How should `runtimeVersion` be aggregated when a pool's nodes disagree?**~~ **Settled: the
  minimum across the pool, plus a Warning Event naming the disagreement** (F11). The deciding
  argument was not correctness — all three candidates can be made correct — but that **a rolling
  driver upgrade is a routine operation**, and refusing to publish a value during one takes the pool
  out of service every time it happens. The minimum's own weakness, a symptom (`ImagePullBackOff`)
  far from its cause (one un-upgraded node), is what the event exists to close.
