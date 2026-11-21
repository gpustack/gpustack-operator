# Configuration

A `ModelDeployment` defines one or more inference-engine roles, each with its own replicas. Roles
can share cached prefixes through a KV cache pool, form a direct *prefill/decode* pair, or do both.

The operator creates `Pod`s for those replicas. Its `ModelDeployment` webhook validates KV cache
transport compatibility; generated `Pod`s bypass KV cache `Pod` injection while ordinary `Pod`
admission still applies.

## Contents

- [Minimal deployment](#minimal-deployment)
- [Prefill and decode](#prefill-and-decode)
- [Topology placement](#topology-placement)
- [Inherited reuse domain](#inherited-reuse-domain)
- [Three override tiers](#three-override-tiers)
- [Operator-owned keys](#operator-owned-keys)
- [Role images](#role-images)
- [Rollout behavior](#rollout-behavior)
- [Admission refusals](#admission-refusals)
- [Operating notes](#operating-notes)

## Minimal deployment

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment  # required; namespaced, short name md
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct  # required; served, never provisioned
  engine:
    name: vLLM  # required; enum: vLLM or SGLang
    image: <a runner image your cluster can pull>  # required runner image
  kvCache:  # optional; omit to run without a shared pool
    poolRef:
      name: team-a-dram  # KVCachePoolBinding in this namespace
    connector: Mooncake  # the only value; defaulted
  roles:
    - name: server
      replicas: 4
      acceleratorType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: 2  # cards per Pod
        cpu: "8"  # optional CPU per Pod
        ram: 32Gi  # optional memory per Pod
        localStorage: 100Gi  # optional ephemeral storage per Pod
```

- **Model**: *`model.name`* identifies the served model; supply weights via hub, role volumes, or
  *`model.artifactRef`* ([Model Artifact](../model-delivery/artifact.md)).
- **Pool**: *`poolRef.name`* names a `KVCachePoolBinding` in the deployment's namespace; `connector`
  defaults to `Mooncake` (immutable).
- **Roles and replicas**: Declare `1` to `10` roles. *`replicas`* counts independent serving
  instances (changing it adds or removes instances without restarting survivors); *`size`* (default
  `1`) counts fate-sharing `Pod`s per instance (editing *`size`* replaces instances one at a time).
- **Multi-member instances (`size > 1`)**: `Pod`s are named `<deployment>-<role>-r<replica>-m<member>`
  behind a per-instance headless `Service`, with `m0` as the HTTP leader. The operator injects and
  reserves `GPUSTACK_REPLICA_LEADER_ADDRESS`, `GPUSTACK_REPLICA_SIZE`, and `GPUSTACK_MEMBER_INDEX`
  (reserved at all sizes, injected when `size > 1`). Declare engine parallelism flags yourself.
- **Resources and CPU-only roles**: *`roles[].resources`* mirrors
  [Accelerator Requests](../devices/requests.md) per `Pod`; optional `cpu`, `ram`, and
  `localStorage` inject only specified values (no unit preset). Omitting *`acceleratorType`*
  schedules CPU-only `Pod`s directly on kube-scheduler without Kueue (accelerator requests rejected).
- **Fabric interfaces**: Optional *`resources.interface`* requests `device.gpustack.ai/rdma.shared`
  (`1`), `device.gpustack.ai/rdma` (`> 1`), or `vpc.amazonaws.com/efa` per `Pod`
  ([RDMA Operations](../rdma/operations.md)) when an effective RDMA/EFA transfer path exists. On
  Ascend A5 (`family` `"950"`), see [Ascend A5 topologies](prefill-decode.md#ascend-a5-topologies).

## Prefill and decode

Multiple roles in one deployment are admitted **atomically**: each replica forms its own Kueue `Pod`
group and an admission check holds all groups until the entire set reserves quota.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: example-model
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: example/engine:tag  # Editable.
  roles:
    - name: prefill
      kind: Prefill  # Server (default), Prefill, Decode or Encode
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

- **Role identity**: `name` becomes the Kueue PodSet name; *`kind`* selects serving behavior. Only
  `Server` roles may repeat a kind with different names.
- **Parallelism**: Pass parallelism degrees via *`roles[].extraArgs`* (or *`roles[].command`* on
  take-over); admission parses and validates them against card counts.
- **Pairing**: See [Prefill and Decode](prefill-decode.md) for connectors, `spec.router`, direct
  transfer transports, and per-role `Service`s.

## Topology placement

*`roles[].topology.requiredLevel`* accepts a configured topology label key (such as region, zone,
rack, or Topograph level) and requires all *`size`* `Pod`s of each replica group to fit within one
domain at that level. Omit it for unconstrained placement; `kubernetes.io/hostname` is implicit and
rejected. Changing it rolls only that role's replicas; see
[Topology-Aware Scheduling Operations](../topology/operations.md).

## Inherited reuse domain

The reuse domain (`name`, *`blockSize`*, `dtype`) is an immutable block on `KVCachePoolBinding`.
`ModelDeploymentSpec` carries no domain field so workloads cannot mint tenants outside their
Binding's quota ([One Binding, one domain](../kv-cache/pool.md#one-binding-one-domain)).

- Deployments sharing a Binding share KV; separate Bindings receive separate tenant identifiers.
- *`status.kvCache`* echoes the attached `binding`, `pool`, and domain block.
- The operator renders the tenant identifier on managed roles; select a compatible engine image to
  enforce isolation ([KV Cache Pools and Bindings](../kv-cache/pool.md#limitations)).

## Three override tiers

Three parameter tiers let you customize engines without patching rendered `Pod`s:

| Tier | Trigger | Behavior |
|---|---|---|
| **Owned** | Managed role and operator-owned key | Admission rejects duplicate *`roles[].extraArgs`* or *`roles[].env`* (including prefixes, underscores and dotted members); use *`roles[].command`* to own the line. |
| **Defaulted** | Managed role and absent user value | Operator injects `--host 0.0.0.0`, `--port`, `--served-model-name`, compile caches, transfer defaults and tuning variables; user *`extraArgs`* or *`env`* wins without refusal. |
| **Take-over** | Non-empty *`roles[].command`* | User owns the full argv; operator skips CLI synthesis, KV/transfer client env, default cache mounts and health probes, and sets *`status.roles[].unmanaged: true`*. |

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: example-model
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: example/engine:tag  # Editable.
  roles:
    - name: server
      acceleratorType: gpustack--nvidia-h20-linux-amd64
      extraArgs:
        - --enable-prefix-caching
      env:
        - name: EXAMPLE
          value: "true"
      image: example/engine:tag
      imagePullPolicy: IfNotPresent
      imagePullSecrets: []
      privileged: false
      ports: []
      additionalVolumes: []
      shmSize: 32Gi
      terminationGracePeriodSeconds: 30
      startupTimeoutSeconds: 1800
      # Take-over alternative (skips operator CLI/env/probes; extraArgs ignored):
      # command:
      #   - /usr/local/bin/server
```

Do not pass engine authentication (`--api-key` or `VLLM_API_KEY`) in *`extraArgs`*: managed direct
decoders probe `/v1/models` and receive `401`, blocking readiness. Place north-south auth at the
cluster gateway.

### Take-over roles

A non-empty *`roles[].command`* replaces the entire argv (there is no `args` field, and *`extraArgs`*
beside it is ignored). The role reports `status.roles[].unmanaged: true` and `CacheAttached=Unknown`,
and receives no operator-rendered cache client environment or reuse domain.

### Shared memory

*`spec.roles[].shmSize`* sets the memory-backed `EmptyDir` limit at `/dev/shm` on the `main`
container (default `16Gi`, applied to both engines, take-over roles, and Elastic Ray members). An
explicit `/dev/shm` mount in *`additionalVolumes`* takes precedence. Shared memory counts toward
container memory limits without increasing `requests.memory`.

### Engram host memory

For non-privileged roles enabling Engram host-memory offload (`vLLM` `--engram-config` with
`cpu_offload` enabled, or `SGLang` `SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE=1`), the operator grants
`IPC_LOCK` and `SYS_RESOURCE` on the `main` container.

### Compilation cache and timeout

Managed roles mount a `DirectoryOrCreate` `HostPath` volume (`gpustack-compile-cache`) at
`/var/lib/gpustack/cache` and default `VLLM_CACHE_ROOT` / `SGLANG_CACHE_DIR` plus seven startup
cache paths (`HF_HUB_CACHE`, `HF_XET_CACHE`, `PIP_CACHE_DIR`, `TORCH_HOME`,
`FLASHINFER_WORKSPACE_BASE`, `TORCHINDUCTOR_CACHE_DIR`, `TRITON_CACHE_DIR`).

`HF_HOME` credentials stay on container storage. Override the mount via *`additionalVolumes`* or
any variable via *`roles[].env`*.

*`spec.roles[].startupTimeoutSeconds`* (default `1800`, range `10`..`86400`) scales
*`startupProbe.failureThreshold`* (`ceil(startupTimeoutSeconds / 10)` at `periodSeconds: 10`) so
cold graph compilation is not killed prematurely.

## Operator-owned keys

Ownership is per `(engine, key)` (`modelDeploymentOwnedKeys`):

| Engine | Owned arguments | Owned environment |
|---|---|---|
| `vLLM` | `--kv-transfer-config`, `--kv-events-config` (including unique prefixes, underscores, and dotted members) | `MOONCAKE_CONFIG_PATH`, `VLLM_MOONCAKE_BOOTSTRAP_PORT`, `VLLM_NIXL_SIDE_CHANNEL_HOST` |
| `SGLang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config`, `--disaggregation-mode`, `--disaggregation-transfer-backend`, `--disaggregation-bootstrap-port` (including unique prefixes) | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME`, **`MOONCAKE_TENANT_ID`** |

- **Conditional `--kv-cache-dtype`**: Owned on both engines while *`spec.kvCache`* is set and
  `model-deployment-kv-cache-dtype-owned` is `true` ([Engine dtype](../kv-cache/pool.md#engine-dtype)).
- **Defaulted keys**: `MC_TE_METRIC=1`, `MC_FORCE_TCP=1` (TCP paths), SGLang hierarchical-cache
  flags, compilation/runtime cache paths, and Ascend transfer defaults (`ASCEND_LOCAL_COMM_RES`,
  `ASCEND_MF_TRANSFER_PROTOCOL`) are defaulted; user entries win without refusal.
  `ASCEND_GLOBAL_RESOURCE_CONFIG` is accepted in *`roles[].env`*.
- **Client configuration**: `vLLM` mounts `/etc/gpustack/kvcache/mooncake.json` read-only from a
  downwardAPI projection (avoid mounting an overlay over `/etc/gpustack/kvcache`); `SGLang` reads
  its entire cache config from environment variables. Routed cache-producing `vLLM` roles also
  render `--kv-events-config` on ZMQ ports `5557`/`5558`; routed `SGLang` reports
  `KVEventsPublishing=False/PublisherDisabled`.

## Role images

Each replica resolves *`roles[].image`* first, then *`spec.engine.image`* (required). The operator
never infers an image tag from node driver versions; specify a pullable tag compatible with your
configuration ([Engine Compatibility](engine-versions.md)).

## Rollout behavior

Changing *`replicas`* adds or removes instances without restarting survivors. Editing any field
that changes a rendered `Pod` (*`size`*, *`acceleratorType`*, *`resources`*, *`command`*, *`image`*,
*`extraArgs`*, *`env`*, *`ports`*, *`additionalVolumes`*, *`shmSize`*,
*`terminationGracePeriodSeconds`*, *`startupTimeoutSeconds`*) rolls that role's replicas.

Each rollout deletes and recreates replicas **one per role per pass**, waiting for fresh Kueue
admission before deleting the next healthy replica.

User spec edits (`pod-spec-hash`) roll immediately; operator-internal rendering upgrades
(`render-version`) keep compatible running replicas in place and apply to new `Pod`s. Set
`modeldeployment.gpustack.ai/min-render-version` on `ModelDeployment` annotations to roll running
replicas onto a newer rendering baseline on your own schedule.

Live fields in *`roles[].runtimeControl.parameters`*, *`roles[].pause`*, and mutable
*`spec.router`* subfields do not roll worker `Pod`s
([Online Runtime Control](runtime-control.md#router-hash-decoupling)).

Select replicas by label rather than name (replacements receive fresh names):

```bash
kubectl get pods -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=<deployment>
```

### Deployment identity fields

Identity fields are frozen (create a new deployment to change them); runtime fields are editable:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: example-model
  labels: {}       # Editable
  annotations: {}  # Editable
spec:
  model:
    name: Qwen/Qwen3-8B  # Frozen
    loraAdapters: []  # Reserved; non-empty values currently rejected
  engine:
    name: vLLM  # Frozen
    image: example/engine:tag  # Editable
  kvCache:  # Frozen
    poolRef:
      name: cache-binding
  kvTransfer: {}  # Editable
  router:  # Block may be added or removed
    name: llm-d-router  # Frozen in place
    replicas: 1    # Editable without rolling worker Pods
    extraArgs: []  # Editable without rolling worker Pods
  roles:  # Role set is frozen
    - name: server  # Frozen
      kind: Server  # Frozen
      replicas: 1  # Editable
      pause: false  # Editable without rolling worker Pods
      size: 1  # Editable, except on Elastic EP
      acceleratorType: gpu-pool  # Editable, except on Elastic EP
      resources: {}  # Editable, except on Elastic EP
      command: []  # Editable, except on Elastic EP
      runtimeControl:  # Editable without rolling Pods; elastic.dataParallelWidth presence frozen
        parameters: {}
      extraArgs: []  # Editable
      env: []  # Editable
      image: example/engine:tag  # Editable
      shmSize: 32Gi  # Editable
      terminationGracePeriodSeconds: 30  # Editable
      startupTimeoutSeconds: 1800  # Editable
```

> **Use JSON patch for single role fields.** `kubectl patch --type=merge` replaces the entire
> *`roles`* array, dropping omitted fields or defaulting *`kind`* back to `Server`. Patch a single
> field with JSON patch or restate the full role:
>
> ```bash
> kubectl patch modeldeployment qwen-chat -n team-a --type=json \
>   -p '[{"op":"replace","path":"/spec/roles/0/size","value":2}]'
> ```

### One group per replica

For accelerated roles, every replica is its own Kueue `Pod` group with one `Workload` of total size
*`size`* (CPU-only roles bypass Kueue). Scale-down sheds the highest ordinals first, deleting each
departing replica's `Pod` and `Workload` together to release quota and finalizers.

## Admission refusals

Schema and validating webhook checks reject invalid configurations at apply time:

| Category | Rejected cases |
|---|---|
| Role structure | `> 10` roles, duplicate role `name`, `replicas > 1024`, `size > 64`, `kind: Server` mixed with other kinds, frozen identity edits, DNS-1035 / colliding `<deployment>-<role>` `Service` names, unnameable members |
| Resources and placement | Unsupported accelerator mode or whole-card count over pool capacity, accelerator request without `acceleratorType`, slice percentage paired with partition profile, *PD* both logically slicing the same group, invalid `topology.requiredLevel` |
| Transfer and interfaces | Negative/fractional or unsupported `resources.interface`, incompatible `kvTransfer.backend` (`HIXL` off Ascend, `NIXL` on Ascend, mixed Ascend/non-Ascend *PD*), new binding to mixed-protocol backend |
| Owned keys and ports | Owned arguments/env in `extraArgs`/`env`, `--kv-cache-dtype` with `spec.kvCache`, `--port` mismatching first `ports` entry, reserved listener ports (`5557`/`5558`, `8998`), unparseable parallel flags or per-member width exceeding cards |

Rules reading `AcceleratorType` are skipped during deletion so missing types cannot block finalizer
removal. An unconverged `AcceleratorType.status` passes admission and emits a `RenderFailed` warning
Event until status converges.

## Operating notes

- **Ports**: Replicas serve on port `8000` unless *`roles[].ports`* or `--port` specifies another
  container port (passing `--port` alone updates the container and `Service` *`targetPort`* while
  keeping the external `Service` port on `8000`). Managed native-vLLM decoders place `routing-proxy`
  on the external port and vLLM on an internal port.
- **Transfer ports and startup log**: Allow ephemeral port ranges in `NetworkPolicy` (transfer
  engines pick random ports, and AscendDirect selects within `ASCEND_BASE_PORT + physical_id * 100`).
  The `"Local segment descriptor not found"` startup `ERROR` line is benign on clients.

### Readiness and status

Inspect phase, `ROLES` summary (`1R2S`, `1R1E1P1D`), conditions, and per-role readiness:

```bash
kubectl -n <namespace> get modeldeployment <name> -o yaml
```

Managed roles probe `GET /health` (or sentinel `/gpustack/v1/live` and `/gpustack/v1/ready`), so
`ready` counts replicas whose engine answered across all *`size`* members. Probes are omitted on
take-over *`roles[].command`*, custom `--host`, conflicting `--port`, client-cert
`--ssl-cert-reqs` outside `0`/`1`, or `UDP`/`SCTP` ports.

| Condition | Key reasons | Action |
|---|---|---|
| `QuotaReserved` | `Pending`, `PodGroupIncomplete`, `PreemptedInPart`, `Parked`, `NoQueueInReservedNamespace` | Check pool `ClusterQueue` capacity and `Workload` status; recreate/reactivate `Parked` sets or free capacity on `PreemptedInPart`. |
| `ReplicasUpToDate` | `UpToDate`, `RolloutInProgress`, `ReplacementInProgress`, `RolloutHeldByCache`, `RolloutHeldByWeights` | Wait for one-per-pass turnover, or repair the `KVCachePoolBinding` / `ModelArtifact` (`WeightsReady`) holding a rollout. |
| `RoleKindsReady` | `AllKindsReady`, `KindsNotReady` | Inspect `Pod` logs and events for any role kind (`Encode`, `Prefill`, `Decode`, `Server`) with zero ready replicas. |
| `CacheAttached` | `CacheActive`, `CacheOperationsFailing`, `NoObservationAvailable`, `Unmanaged` | On `CacheOperationsFailing`, inspect engine and `KVCacheBackend` logs; `NoObservationAvailable` is normal while idle. |

For `RouterReady`, `EndpointEligibility`, and per-group router qualification, see
[Model Deployment Routing](routing.md#troubleshooting).

---

**See also** — [KV Cache Pools and Bindings](../kv-cache/pool.md) (namespace Binding grants and reuse domains) · [Elastic EP](elastic-ep.md) (in-place collective expansion) · [Prefill and Decode](prefill-decode.md) (pairing *prefill* and *decode* roles) · [Accelerator Requests](../devices/requests.md) (request fields mirrored in *`roles[].resources`*) · [Model Deployment Routing](routing.md) (endpoint eligibility and router policies) · [Online Runtime Control](runtime-control.md) (live parameter tuning and role pause) · [Model Deployment Scaling](scaling.md) (`ModelDeploymentScaler` and graceful retirement).

**Next** → [Accelerator Requests](../devices/requests.md)
