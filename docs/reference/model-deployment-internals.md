# Model Deployment Internals

Evidence: contract. Exhaustive `ModelDeployment` `Pod` metadata, operator-owned key matrix, runtime
cache defaults, render-version upgrade mechanics, router RBAC and sidecar listeners, full admission
refusal catalog, and runtime-selected transfer port formulas.

## Contents

- [Pod labels and annotations](#pod-labels-and-annotations)
- [Operator-owned keys](#operator-owned-keys)
- [Render version and upgrades](#render-version-and-upgrades)
- [Router RBAC and listeners](#router-rbac-and-listeners)
- [Admission refusal catalog](#admission-refusal-catalog)
- [Runtime transfer ports](#runtime-transfer-ports)

## Pod labels and annotations

Kueue and the `ModelDeployment` controller join `Pod`s to replica groups and `Workload` PodSets
through operator-written metadata (values in angle brackets are illustrative):

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    # Hash of namespace, deployment, role and ordinal; unique, not parseable
    kueue.x-k8s.io/pod-group-name: gpustack-fnv64-<hash>
    # AcceleratorType status.entrance; all Pods in one group agree
    kueue.x-k8s.io/queue-name: <accelerator-type-entrance>
    app.kubernetes.io/component: server  # Role name; Service selection and status attribution
    modeldeployment.gpustack.ai/role-kind: server  # Effective kind in router spelling
    modeldeployment.gpustack.ai/pod-ordinal: "0"  # Replica slot; scale-down sheds highest first
    modeldeployment.gpustack.ai/member-index: "0"  # Member index, present even at size 1
  annotations:
    kueue.x-k8s.io/pod-group-total-count: "1"  # Role size; Kueue waits for this many Pods
    kueue.x-k8s.io/role-hash: server  # Role name; joins Workload PodSet to role
    kueue.x-k8s.io/pod-group-serving: "true"  # Retain quota for serving groups that exit
    kueue.x-k8s.io/podset-required-topology: <required-level>  # Present when requiredLevel set
    modeldeployment.gpustack.ai/pod-spec-hash: <digest>  # SHA-256 of explicit fields & bindings
    modeldeployment.gpustack.ai/render-version: "7"  # Positive integer rendering baseline
```

- **`role-hash` invariant**: Status joins a `Workload`'s PodSets to roles by `kueue.x-k8s.io/role-hash`.
  Without it, Kueue names PodSets by `Pod` spec digest and status attribution breaks silently.
- **`pod-group-fast-admission` prohibition**: `kueue.x-k8s.io/pod-group-fast-admission` is never
  set because it composes the `Workload` from the first runnable `Pod` alone and mis-composes
  multi-member instances.
- **No topology `nodeSelector`**: The operator never adds topology values to `spec.nodeSelector`;
  Kueue assigns the domain from the flavor's `Topology`.

## Operator-owned keys

### Owned key matrix

Ownership is scoped per `(engine, key)` (`modelDeploymentOwnedKeys`):

| Engine | Owned arguments | Owned environment |
|---|---|---|
| `vLLM` | `--kv-transfer-config`, `--kv-events-config`, `--ec-transfer-config` (including unique prefixes like `--kv-transfer-conf`, underscores `--kv_transfer_config`, and dotted members `--kv-transfer-config.kv_role`) | `MOONCAKE_CONFIG_PATH`, `VLLM_MOONCAKE_BOOTSTRAP_PORT`, `VLLM_NIXL_SIDE_CHANNEL_HOST`, `VLLM_EC_SIDE_CHANNEL_HOST` |
| `SGLang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config`, `--disaggregation-mode`, `--disaggregation-transfer-backend`, `--disaggregation-bootstrap-port`, `--kv-events-config`, `--encoder-only`, `--language-only`, `--encoder-urls`, `--encoder-bootstrap-port` (including unique prefixes like `--disaggregation-mo`) | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME`, **`MOONCAKE_TENANT_ID`** |

- **Conditional `--kv-cache-dtype`**: Owned on both engines while *`spec.kvCache`* is set and
  `model-deployment-kv-cache-dtype-owned` is enabled (default `true`). Stored deployments with the
  flag keep running on their own value (later on argv) until updated; deletion finalizer removal is
  exempt.
- **Conditional `vllm-router` `kv_aware` keys**: When `vllm-router` activates `kv_aware` on a managed
  `vLLM` *Server* or *Prefill* role (`>= 0.31.0`; *Decode* never subscribes to KV events),
  `--prefix-caching-hash-algo` and `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES` are owned on that role
  (refused at admission and stripped during user env merge).
  `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES` is listed in `modelDeploymentScalerCapacityNeutralEnvNames`.
- **Demoted EC env vars**: `VLLM_USE_V2_MODEL_RUNNER` (emitted as `1` only for `< 0.34.0` or legacy
  empty version) and `VLLM_EC_SIDE_CHANNEL_PORT` (defaulted to `5601`) are Defaulted, not owned.
- **Replica membership variables**: `GPUSTACK_REPLICA_LEADER_ADDRESS`, `GPUSTACK_REPLICA_SIZE`, and
  `GPUSTACK_MEMBER_INDEX` are reserved on all engines at every *`size`* (rendered only when
  `size > 1`).

### SGLang and vLLM config

- **SGLang loader precedence**: SGLang resolves Mooncake config in order:
  `--hicache-storage-backend-extra-config` → `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` → environment.
  The operator owns and leaves both first sources unset to force environment loading (which carries
  `MOONCAKE_LOCAL_HOSTNAME` from `status.podIP` via `fieldRef`); setting either file/flag source
  replaces all other keys with built-in defaults (`4 GiB` segment, `localhost`).
- **vLLM client JSON**: Mounted read-only at `/etc/gpustack/kvcache/mooncake.json` via downwardAPI
  projection of `Pod` annotation `kvcache.gpustack.ai/client-config` (no `ConfigMap` created).
  An overlay volume mounting over `/etc/gpustack/kvcache` silently replaces it.
- **KV events publisher**: Publishing roles open ZMQ publisher `tcp://*:5557` and replay
  `tcp://*:5558` (`tcp://*:<5557+localDP>` when per-`Pod` `localDP > 1`, reserving
  `[5557 .. 5557 + 2*localDP - 1]` across local DP ranks; `topic: "kv@pod@<spec.model.name>"` on
  `vLLM`/`vLLM-Ascend` behind `llm-d-router` or `vllm-router` `kv_aware`; `topic: "kv-events"` on
  `SGLang` *Server*/*Prefill* behind `sgl-router`).
- **`KVEventsPublishing` condition**: Reports `True/Publishing` when all expected roles publish,
  `True/NotApplicable` for unrouted *Server* roles, `SGLang + llm-d-router`, `vllm-router` without
  `kv_aware`, and frozen `sglang-gateway`, `Unknown/RoleUnmanaged` when an expected role sets
  *`roles[].command`*, and `False` for unrouted *PD* (`NoRouter`) or missing config
  (`PublisherDisabled`).

### Defaulted cache and env

For managed roles (*`roles[].command`* empty), the operator mounts a `DirectoryOrCreate` `HostPath`
volume (`gpustack-compile-cache`) at `/var/lib/gpustack/cache` backed by
`/var/lib/gpustack/cache/<manufacturer>-<engine>/<model-identity>/tp<TP>-pp<PP>-dp<DP>-ep<EP>-cp<PCP>_<DCP>-<hash8>`
and defaults `VLLM_CACHE_ROOT` / `SGLANG_CACHE_DIR` plus seven startup cache variables (each
overridable in *`roles[].env`*; `HF_HOME` stays on container storage to protect credentials):

| Name | Points at | Holds |
|---|---|---|
| `HF_HUB_CACHE` | `/var/lib/gpustack/cache/hf-hub` | Model weights and tokenizer files from `huggingface_hub` |
| `HF_XET_CACHE` | `/var/lib/gpustack/cache/xet` | Xet chunks read by `hf_xet` |
| `PIP_CACHE_DIR` | `/var/lib/gpustack/cache/pip` | `pip` download and wheel cache |
| `TORCH_HOME` | `/var/lib/gpustack/cache/torch` | PyTorch hub checkpoints and cached state |
| `FLASHINFER_WORKSPACE_BASE` | `/var/lib/gpustack/cache/flashinfer` | FlashInfer JIT sources and kernels (appends `.cache/flashinfer`) |
| `TORCHINDUCTOR_CACHE_DIR` | `/var/lib/gpustack/cache/inductor` | Torch Inductor compiled kernels |
| `TRITON_CACHE_DIR` | `/var/lib/gpustack/cache/triton` | Triton compiled kernels |

- **Compile-cache env hash**: The leaf `<hash8>` tracks `VLLM_USE_STANDALONE_COMPILE`,
  `VLLM_USE_MEGA_AOT_ARTIFACT`, `SGLANG_ENABLE_TORCH_COMPILE`, `TRITON_ATTN_NUM_STAGES`, and
  `TORCH_CUDA_ARCH_LIST`. Retired vLLM `< 0.30` keys (`VLLM_USE_V1`, `VLLM_ATTENTION_BACKEND`,
  `VLLM_FLASH_ATTN_VERSION`) were removed; legacy `< 0.30` `Pod`s restarting after upgrade incur one
  cold compile-cache miss.
- **Other defaulted env**: `MC_TE_METRIC=1`, `MC_FORCE_TCP=1`, Mooncake TCP tuning
  (`MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER`,
  `MC_TCP_LANES_PER_PEER`, `MC_TCP_ADMISSION_TIMEOUT_MS`), `VLLM_NIXL_SIDE_CHANNEL_PORT=8998`,
  `VLLM_EC_SIDE_CHANNEL_PORT=5601`, and `VLLM_USE_V2_MODEL_RUNNER=1` (`< 0.34.0` / legacy only).
- **Vendor and SGLang defaults**: `SGLANG_DISAGGREGATION_NIXL_BACKEND`, `ASCEND_AUTO_CONNECT=1`,
  `ASCEND_LOCAL_COMM_RES`, `ASCEND_GLOBAL_RESOURCE_CONFIG`, `ASCEND_MF_TRANSFER_PROTOCOL`, and
  SGLang flags `--enable-hierarchical-cache` (*Server*/*Prefill* with pool),
  `--disaggregation-decode-retraction-backup cpu_tensor` (*Decode*), and `--page-size 64` (TCP).

## Render version and upgrades

Two currency annotations separate user spec edits from operator-internal rendering changes:

| Annotation | Scope | Trigger | Rollout behavior |
|---|---|---|---|
| `modeldeployment.gpustack.ai/pod-spec-hash` (`Pod`) | Explicit `ModelDeployment` fields (`model`, *`engine`* including resolved effective version, *`router`* projection, *`kvCache.poolRef`*, *`kvTransfer`*, role config) plus `bindingHash` (resolved `AcceleratorType`, `KVCachePoolBinding`, `ModelArtifact`, fabric key, `RuntimeClassName`, `kv_aware` role activation, `ordinal`/`member`) | Spec edit or bound external change | Rolls affected replicas one per role per pass |
| `modeldeployment.gpustack.ai/render-version` (`Pod`) | Operator-synthesized flags, defaulted env, probes, `preStop` drain hook, sidecars (`routing-proxy`, `wait-for-worker`), implicit metadata, and worker `Setting` values (current baseline: `7`) | Operator upgrade with newer render baseline | Keeps compatible running replicas in place; applies to new/replaced `Pod`s |
| `modeldeployment.gpustack.ai/min-render-version` (`ModelDeployment`) | Per-deployment render-version floor clamped to `[minSupportedRenderVersion, 7]` | Setting or raising annotation | Rolls replicas below effective floor one per role per pass |

- **`renderVersion=7` currency protection**: `modelDeploymentPodMatchesCurrencyWithLegacy` accepts
  pre-upgrade `bindingHash` values for standing pre-`renderVersion=7` `Pod`s (including
  `vLLM-Ascend + llm-d-router` `Pod`s hashed with `PublishKVEvents: false`), preventing automatic
  rollout on operator upgrade while new or edited `Pod`s adopt `renderVersion=7`.
- **Engine version and `kv_aware` hashing**: Folding each role's resolved effective `EngineVersion`
  into `pod-spec-hash` rolls affected roles when *`spec.engine.version`* or *`roles[].engineVersion`*
  changes on custom images. Folding per-role `kv_aware` activation into `bindingHash` rolls only
  affected roles when toggling `kv_aware`, while unrelated *`spec.router.extraArgs`* edits leave
  worker `Pod`s untouched.

### Rollout and departure rules

- **Cadence guard**: A healthy old replica is deleted only when every declared replica holds an
  admitted `Workload` (avoiding shedding capacity on a full pool). Queued replacements are
  superseded directly without waiting for obsolete-config admission; complete groups still waiting
  on first admission (never previously admitted/evicted/preempted) are replaced ahead of admitted
  siblings.
- **Broken replica ordering**: An old replica that loses a member while a replacement is unresolved
  waits behind the active replacement, then rebuilds whole before another healthy replica rolls.
  Recovery never mixes old and new configuration members in one group.
- **Slot release by cause**: Kueue preemption deletes the `Workload` and `Pod`, freeing the slot
  automatically. A `Pod`-only delete (drain, kubelet eviction, `kubectl delete pod`) leaves the
  serving `Workload` holding finalizers until the controller deletes that `Workload` and recreates
  the ordinal once zero `Pod`s remain on the API server.
- **`NotReady` nodes**: A replica on a `NotReady` but registered `Node` retains its `Running` phase
  and ordinal; do not force-delete the `Pod` (risking dual accelerator holders) — delete the `Node`
  object instead.
- **Engine authentication**: `--api-key` and `VLLM_API_KEY` are not owned, so admission accepts
  them, causing `/v1/models` health gates on managed direct decoders to fail with HTTP `401`.

## Router RBAC and listeners

- **`sgl-router` `EndpointSlice` discovery**: Per-role `Service`s (`<md>-<role>`) carry
  `modeldeployment.gpustack.ai/role-kind=<kind>` (never stamped on `<md>` or headless replica
  `Service`s `<md>-<role>-r<n>`), which Kubernetes copies onto role `EndpointSlice` objects. The
  `<md>-router` `Role` grants `discovery.k8s.io/endpointslices` (`get`, `list`, `watch`). Custom
  least-privilege operator worker bindings must include `discovery.k8s.io/endpointslices`
  (`get`, `list`, `watch`) so the controller can create `sgl-router` `Role`s.
- **Envoy allowlist sidecar listeners**: On `vllm-router`, `sglang-gateway`, and `sgl-router`, Envoy
  listens on `:8000` (inference API allowlist forwarding to loopback `127.0.0.1:8001`) and `:9091`
  (forwarding only `GET /observer/endpoints`). Because `sgl-router` serves Prometheus `/metrics` on
  its main HTTP server rather than a dedicated metrics port, `sgl-router`'s Envoy sidecar also opens
  `:9090` forwarding `GET /metrics` so Prometheus scraping stays off `:8000`.

## Admission refusal catalog

Two webhooks (mutating for `AcceleratorType`-dependent accelerator default and `gpustack/runner`
engine version backfill; validating for the rules below) plus CRD schema checks enforce admission:

| Refused | Message names |
|---|---|
| Unparseable managed image without explicit version | Image, role, and *`spec.engine.version`* or *`roles[].engineVersion`* remediation |
| Explicit version disagreeing with parseable `gpustack/runner` tag | Explicit version, image, parsed tag version, and update-or-clear remediation |
| Invalid engine version syntax | Field path and `major.minor[.patch][rcN|.rcN|.postN]` grammar |
| Mismatched engine and router pairing | Router name and allowed engines (`sgl-router`/`sglang-gateway` → `SGLang`, `vllm-router` → `vLLM`, `llm-d-router` → both) |
| `vllm-router` `kv_aware` with `vLLM < 0.31.0` or unresolved version | Role, `0.31.0` floor (`GET /kv_event_sources`), and version field path |
| Mixed Ascend *PD* connector generation (`< 0.30.0` paired with `>= 0.30.0`) | Both roles, resolved versions, and `MooncakeConnectorV1`/`V2` alignment rule |
| Invalid `VLLM_EC_SIDE_CHANNEL_PORT` or HTTP port collision | Role, port (`0` or `1`–`65535`), and `ECCPUConnector` side-channel conflict |
| More than 10 roles | Operator's own role-count limit (each role renders replicas, `Service`s, and queues) |
| Unnameable role members | Longest name produced by *`replicas`* and *`size`*, length, and remediation |
| Duplicate role `name` | Schema refusal (`listType=map` keyed on `name`) |
| Identity field edit (`model`, *`engine.name`*, *`kvCache`*, role set, role *`kind`*) | Field path; create a new deployment instead |
| Unsupported resource mode on `AcceleratorType` | Mode, type, and offered partition profile inventory |
| Whole-card count exceeding pool capacity | Pool total capacity (not free capacity) |
| Invalid or unsupported *`resources.interface`* | Role interface field and blocking protocol (asymmetric A5 *PD*, `SGLang` Ascend, A2/A3, unconverged `family`, mixed backend/fabric) |
| Accelerator request without `acceleratorType` | Omit accelerator resources for CPU-only role or select an `AcceleratorType` |
| Incompatible *`kvTransfer.backend`* or mixed Ascend pair | Field path and incompatible manufacturer (`HIXL` on non-Ascend/CPU, `NIXL` on Ascend, mixed Ascend/non-Ascend *PD*) |
| *Prefill* + *Decode* both logically slicing same accelerator group | Both roles and slice field (whole cards and hardware partitions accepted) |
| `<deployment>-<role>` not a DNS-1035 label | Combined `Service` name (`> 63` chars or dot); existing stored roles exempt |
| Colliding derived `Service` names (e.g., role `x-r0` vs multi-member `x`) | Shared name and both claimants (checked on every edit) |
| Invalid *`topology.requiredLevel`* (including `kubernetes.io/hostname`) | Field path and topology field rule |
| *`replicas`* `> 1024` or *`size`* `> 64` | Schema ceiling |
| `kind: Server` beside another kind | Conflict between standalone server and disaggregated kinds |
| Unsupported role *`kind`* for engine | Engine and kind |
| Owned key in *`extraArgs`* or *`env`* (including `kv_aware` conditional keys and router owned flags) | Key, engine/router, and *`roles[].command`* alternative |
| `--kv-cache-dtype` in *`extraArgs`* with *`spec.kvCache`* | Binding `dtype` ownership and ways out |
| `--port` mismatching first *`ports`* entry | Both values and source fields (managed direct decoder exempt; stored roles checked on edit) |
| Reserved listener port collision (ZMQ `[5557 .. 5557 + 2*localDP - 1]` on all publishing *Server*/*Prefill* roles, NIXL `8998`, bootstrap `8998`, SGLang encoder bootstrap `8997`) | Port, source field, reserved set (take-over *`command`* exempt) |
| Unparseable parallel degree flag or malformed `VLLM_DP_SIZE` | Role, flag, and *`roles[].command`* alternative |
| Declared per-member parallel width exceeding card request | Card count and derived degrees (*`size`* does not cover per-member width) |
| Removed `template` field on role | Strict schema decoding refusal |
| Partition profile combined with slice percentage | Both slice fields |
| Cross-namespace *`poolRef`* or self-declared reuse domain | Unrepresentable in schema |
| Empty *`poolRef.name`* | Binding authorization requirement |
| New binding to mixed-protocol backend for unconstrained engine | Effective protocols (existing unchanged binding stays editable) |

### Webhook and render boundary

- **Deletion bypass**: Engine version, router matrix, and `AcceleratorType`-reading rules short-circuit
  when a `ModelDeployment` has a deletion timestamp so finalizer removal never deadlocks.
- **Fresh `AcceleratorType` race**: Unconverged `AcceleratorType.status` is not rejected at
  admission; rendering emits a `RenderFailed` warning Event until `AcceleratorType.status`
  converges.

## Runtime transfer ports

*`roles[].ports`* configures container and `Service` listeners only; it does not reserve or select
transfer-engine ports. On Ascend, `AscendDirect` calculates its container-namespace transfer port
window at runtime after physical device assignment:

| Input | Rule |
|---|---|
| Base port | `ASCEND_BASE_PORT`, or `20000` when unset |
| Window | `base_port + physical_device_id * 100` through `base_port + (physical_device_id + 1) * 100` inclusive |
| Eight-card example | Physical device ID `7` selects within `20700`–`20800` |
| Port choice | Random selection with up to `500` attempts (other transfer families also pick random ports) |

Parallelism degrees and card counts are not inputs to this window calculation.

---

**See also** — [Configuration](../modules/model-deployment/configuration.md) (operator override tiers, core manifests and status diagnosis) · [Parameters](model-deployment-parameters.md) (complete parameter injection tables) · [Model Deployment Status](model-deployment-status.md) (conditions and retirement fields) · [Model Deployment Shutdown](model-deployment-shutdown.md) (PreStop drain and retirement budgets)
