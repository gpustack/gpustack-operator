# Configuration

A `ModelDeployment` runs one or more inference-engine roles, and each role has its own replicas.
Roles can share cached prefixes through a KV cache pool, form a direct *prefill/decode (PD)* pair,
or do both.

The operator creates the `Pod`s for each replica. The `ModelDeployment` webhook checks KV cache
transport compatibility. Generated `Pod`s skip KV cache `Pod` injection, but ordinary `Pod`
admission still applies.

## Contents

- [Deployment configuration](#deployment-configuration)
- [Engine overrides](#engine-overrides)
- [Rollout behavior](#rollout-behavior)
- [Admission refusals](#admission-refusals)
- [Operations](#operations)

## Deployment configuration

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment  # namespaced, short name md
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct  # required; served, never provisioned
  engine:
    name: vLLM  # required; enum: vLLM, SGLang
    image: <a runner image your cluster can pull>  # required
  kvCache:  # optional; omit to run without a shared pool
    poolRef:
      name: team-a-dram  # KVCachePoolBinding in this namespace
    connector: Mooncake  # optional, default: Mooncake (the only value); immutable
  roles:  # 1 to 10 roles
    - name: server
      replicas: 4  # independent serving instances
      size: 1  # optional, default: 1; fate-sharing Pods per instance
      acceleratorType: gpustack--nvidia-a10g-linux-amd64  # omit for a CPU-only role
      resources:  # per Pod
        accelerator: 2  # cards
        cpu: "8"  # optional
        ram: 32Gi  # optional
        localStorage: 100Gi  # optional ephemeral storage
```

| Field | Constraints |
|---|---|
| *`model.name`* | Supply the weights from the hub, role volumes or *`model.artifactRef`* ([Model Artifact](/gpustack-operator/v0.12.0/docs/modules/model-delivery/artifact/index.md)). |
| *`resources`* | Mirrors [Accelerator Requests](/gpustack-operator/v0.12.0/docs/modules/devices/requests/index.md). The operator injects only the `cpu`, `ram` and `localStorage` values you set, with no unit preset. |
| *`acceleratorType`* | If omitted, kube-scheduler places the `Pod`s directly without Kueue, and admission rejects accelerator requests. |
| *`resources.interface`* | Optional. Requests `device.gpustack.ai/rdma.shared` for `1`, `device.gpustack.ai/rdma` for `> 1`, or `vpc.amazonaws.com/efa`, per `Pod`. |

See [Rollout behavior](#rollout-behavior) for how edits to *`replicas`* and *`size`* apply. Request
*`resources.interface`* only when an effective RDMA or EFA transfer path exists; see
[RDMA Operations](/gpustack-operator/v0.12.0/docs/modules/rdma/operations/index.md). On Ascend A5 (`family` `"950"`), follow
[Ascend A5 topologies](/gpustack-operator/v0.12.0/docs/modules/model-deployment/prefill-decode/index.md#ascend-a5-topologies).

### Multi-member instances

If *`size`* is greater than `1`, `Pod`s are named `<deployment>-<role>-r<replica>-m<member>`. Each
instance has its own headless `Service`, and member `m0` is the HTTP leader.

The operator reserves `GPUSTACK_REPLICA_LEADER_ADDRESS`, `GPUSTACK_REPLICA_SIZE` and
`GPUSTACK_MEMBER_INDEX` at every size. It injects them only when `size > 1`. Declare the engine
parallelism flags yourself.

### Prefill and decode

Admission handles all roles in one deployment **atomically**. Each replica forms its own Kueue `Pod`
group. An admission check holds every group until the whole set reserves quota.

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
    image: example/engine:tag
  roles:
    - name: prefill  # becomes the Kueue PodSet name
      kind: Prefill  # enum: Server (default), Prefill, Decode, Encode
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

- Only `Server` roles can repeat a *`kind`* under different names.
- Pass parallelism degrees in *`roles[].extraArgs`*, or in *`roles[].command`* on a take-over role.
- Admission parses those degrees and checks them against the card counts.

[Prefill and Decode](/gpustack-operator/v0.12.0/docs/modules/model-deployment/prefill-decode/index.md) covers connectors, *`spec.router`*, direct transfer
transports and per-role `Service`s.

### Topology placement

*`roles[].topology.requiredLevel`* takes a configured topology label key, such as a region, zone,
rack or Topograph level. All *`size`* `Pod`s of each replica group must then fit in one domain at
that level.

Omit the field for unconstrained placement. `kubernetes.io/hostname` is implicit, and admission
rejects it. A change rolls only that role's replicas. See
[Topology-Aware Scheduling Operations](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md).

### KV cache reuse domain

The reuse domain (`name`, *`blockSize`*, `dtype`) is an immutable block on the `KVCachePoolBinding`.
`ModelDeploymentSpec` has no domain field. A workload therefore cannot mint tenants outside its
Binding's quota ([Cache reuse domains](/gpustack-operator/v0.12.0/docs/modules/kv-cache/pool/index.md#cache-reuse-domains)).

- `ModelDeployment`s that share a Binding share KV. Separate Bindings get separate tenant identifiers.
- *`status.kvCache`* echoes the attached `binding`, `pool` and domain block.
- The operator renders the tenant identifier on managed roles. Select a compatible engine image to
  enforce isolation ([KV Cache Pools and Bindings](/gpustack-operator/v0.12.0/docs/modules/kv-cache/pool/index.md#limitations)).

### Role images

Each replica uses *`roles[].image`* if set, then the required *`spec.engine.image`*. The operator
never infers an image tag from node driver versions. Set a pullable tag that fits the configuration;
see [Engine Compatibility](/gpustack-operator/v0.12.0/docs/modules/model-deployment/engine-versions/index.md).

## Engine overrides

Customize the engine through three tiers. Do not patch rendered `Pod`s.

| Tier | Trigger | Behavior |
|---|---|---|
| **Owned** | Managed role and operator-owned key | Admission rejects owned arguments in *`roles[].extraArgs`* (including unique prefixes; on vLLM also underscores and dotted members) and owned variables in *`roles[].env`*. Use *`roles[].command`* to own the line. |
| **Defaulted** | Managed role and no user value | The operator injects `--host 0.0.0.0`, `--port`, `--served-model-name`, compile caches, transfer defaults and tuning variables. A user *`extraArgs`* or *`env`* entry wins without refusal. |
| **Take-over** | Non-empty *`roles[].command`* | You own the full argv (there is no `args` field, and *`extraArgs`* is ignored). The operator skips CLI synthesis, KV and transfer client environment, default cache mounts and health probes. It sets *`status.roles[].unmanaged`* to `true`, reports `CacheAttached=Unknown` and assigns no reuse domain. |

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
    image: example/engine:tag
  roles:
    - name: server
      acceleratorType: gpustack--nvidia-h20-linux-amd64
      extraArgs:  # Defaulted tier: user values win over operator defaults
        - --enable-prefix-caching
      env:
        - name: EXAMPLE
          value: "true"
      image: example/engine:tag  # optional; overrides spec.engine.image
      imagePullPolicy: IfNotPresent
      imagePullSecrets: []
      privileged: false
      ports: []
      additionalVolumes: []
      shmSize: 32Gi  # optional; see Shared memory
      terminationGracePeriodSeconds: 30
      startupTimeoutSeconds: 1800  # optional, default: 1800; range 10..86400
      # command:  # optional; triggers Take-over tier
      #   - /usr/local/bin/server
```

Do not pass engine authentication (`--api-key` or `VLLM_API_KEY`) in *`extraArgs`*. Managed direct
decoders probe `/v1/models`, receive `401` and never become ready. Put north-south authentication at
the cluster gateway.

### Shared memory

*`spec.roles[].shmSize`* sets the size of the memory-backed `EmptyDir` at `/dev/shm` in the `main`
container. The default is `16Gi`. It applies to both engines, take-over roles and Elastic Ray
members.

An explicit `/dev/shm` mount in *`additionalVolumes`* takes precedence. Shared memory counts toward
the container memory limit but does not raise `requests.memory`.

### Engram host memory

If a non-privileged role enables Engram host-memory offload, the operator grants `IPC_LOCK` and
`SYS_RESOURCE` on the `main` container. vLLM enables offload through `--engram-config` with
`cpu_offload`. SGLang enables it through `SGLANG_ENABLE_DSV41_ENGRAM_HOST_TABLE=1`.

### Compilation cache and timeout

Managed roles mount the `DirectoryOrCreate` `HostPath` volume `gpustack-compile-cache` at
`/var/lib/gpustack/cache`. The operator also defaults `VLLM_CACHE_ROOT` or `SGLANG_CACHE_DIR` and
seven startup cache paths: `HF_HUB_CACHE`, `HF_XET_CACHE`, `PIP_CACHE_DIR`, `TORCH_HOME`,
`FLASHINFER_WORKSPACE_BASE`, `TORCHINDUCTOR_CACHE_DIR` and `TRITON_CACHE_DIR`.

`HF_HOME` credentials stay on container storage. To override the mount, use *`additionalVolumes`*.
To override a variable, set it in *`roles[].env`*.

*`spec.roles[].startupTimeoutSeconds`* sets *`startupProbe.failureThreshold`* to
`ceil(startupTimeoutSeconds / 10)` at `periodSeconds: 10`. If cold graph compilation needs more time
than the default timeout, raise it.

### Operator-owned keys

The operator owns each key per engine. Admission refuses these keys in *`extraArgs`* and *`env`* on a
managed role:

| Engine | Owned arguments | Owned environment |
|---|---|---|
| `vLLM` | `--kv-transfer-config`, `--kv-events-config` (including unique prefixes, underscores, and dotted members) | `MOONCAKE_CONFIG_PATH`, `VLLM_MOONCAKE_BOOTSTRAP_PORT`, `VLLM_NIXL_SIDE_CHANNEL_HOST` |
| `SGLang` | `--hicache-storage-backend`, `--hicache-storage-backend-extra-config`, `--disaggregation-mode`, `--disaggregation-transfer-backend`, `--disaggregation-bootstrap-port` (including unique prefixes) | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH`, `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME`, **`MOONCAKE_TENANT_ID`** |

- Encoder-stage keys (including vLLM `--ec-transfer-config` and SGLang encoder flags) are also owned
  per engine; see [Keys the operator owns](/gpustack-operator/v0.12.0/docs/modules/model-deployment/encode-prefill-decode/index.md#operator-owned-keys).
- `--kv-cache-dtype` is owned on both engines while *`spec.kvCache`* is set and
  `model-deployment-kv-cache-dtype-owned` is `true` ([Engine cache precision](/gpustack-operator/v0.12.0/docs/modules/kv-cache/pool/index.md#engine-cache-precision)).
- These keys are defaulted, so a user entry wins without refusal: `MC_TE_METRIC=1`, `MC_FORCE_TCP=1`
  on TCP paths, SGLang hierarchical-cache flags, and compilation and runtime cache paths.
- Ascend transfer defaults `ASCEND_LOCAL_COMM_RES` and `ASCEND_MF_TRANSFER_PROTOCOL` are also
  defaulted. *`roles[].env`* accepts `ASCEND_GLOBAL_RESOURCE_CONFIG`.

vLLM reads its client configuration from `/etc/gpustack/kvcache/mooncake.json`. The file is a
read-only downwardAPI projection, so do not mount an overlay over `/etc/gpustack/kvcache`. SGLang
reads its whole cache configuration from environment variables.

Routed cache-producing vLLM roles also render `--kv-events-config` on ZMQ ports `5557` and `5558`.
Routed SGLang roles report `KVEventsPublishing=False/PublisherDisabled`.

## Rollout behavior

A change to *`replicas`* adds or removes instances and leaves survivors running. An edit to any field
that changes the rendered `Pod` rolls that role's replicas. These fields are *`size`*,
*`acceleratorType`*, *`resources`*, *`command`*, *`image`*, *`extraArgs`*, *`env`*, *`ports`*,
*`additionalVolumes`*, *`shmSize`*, *`terminationGracePeriodSeconds`* and *`startupTimeoutSeconds`*.

Each rollout deletes and recreates **one replica per role per pass**. It waits for fresh Kueue
admission before it deletes the next healthy replica.

A user spec edit changes `pod-spec-hash` and rolls at once. An operator rendering upgrade changes
`render-version`. Compatible running replicas stay in place, and new `Pod`s use the new rendering.

To move running replicas to a newer rendering baseline on your own schedule, set the minimum render
version. When replicas run an older rendering, the `ReplicasUpToDate` message reports the current
render version:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-chat
   namespace: team-a
+  annotations:
+    modeldeployment.gpustack.ai/min-render-version: "<render version>"  # integer
```

Live fields in *`roles[].runtimeControl.parameters`*, *`roles[].pause`* and mutable *`spec.router`*
subfields do not roll worker `Pod`s
([Online Runtime Control](/gpustack-operator/v0.12.0/docs/modules/model-deployment/runtime-control/index.md#router-rollout-effects)).

Replacement `Pod`s get fresh names, so select replicas by label:

```bash
kubectl get pods -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=<deployment>
```

### Deployment identity fields

Identity fields are frozen. To change one, create a new `ModelDeployment`. Runtime fields are
editable:

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

> **Use JSON patch for one role field.** `kubectl patch --type=merge` replaces the whole *`roles`*
> array. It drops omitted fields and can reset *`kind`* to `Server`. Patch one field with JSON patch,
> or restate the full role:
>
> ```bash
> kubectl patch modeldeployment qwen-chat -n team-a --type=json \
>   -p '[{"op":"replace","path":"/spec/roles/0/size","value":2}]'
> ```

### Replica admission groups

For an accelerated role, each replica is its own Kueue `Pod` group with one `Workload` of total size
*`size`*. CPU-only roles bypass Kueue.

Scale-down removes the highest ordinals first. It deletes each departing replica's `Pod`s and
`Workload` together, which releases quota and finalizers.

## Admission refusals

Schema and validating webhook checks reject these configurations at apply time:

| Category | Rejected cases |
|---|---|
| Role structure | `> 10` roles, duplicate role `name`, `replicas > 1024`, `size > 64`, `kind: Server` mixed with other kinds, frozen identity edits, DNS-1035 / colliding `<deployment>-<role>` `Service` names, unnameable members |
| Resources and placement | Unsupported accelerator mode or whole-card count over pool capacity, accelerator request without `acceleratorType`, slice percentage paired with partition profile, *PD* both logically slicing the same group, invalid `topology.requiredLevel` |
| Transfer and interfaces | Negative/fractional or unsupported `resources.interface`, incompatible `kvTransfer.backend` (`HIXL` off Ascend, `NIXL` on Ascend, mixed Ascend/non-Ascend *PD*), new binding to mixed-protocol backend |
| Owned keys and ports | Owned arguments/env on managed roles (including conditional `--kv-cache-dtype`; see [Operator-owned keys](#operator-owned-keys)), `--port` mismatching first `ports` entry, reserved listener ports (`5557`/`5558`, `8998`), unparseable parallel flags or per-member width exceeding cards |

Rules that read `AcceleratorType` are skipped during deletion, so a missing type cannot block
finalizer removal. If `AcceleratorType.status` has not converged, admission passes. The deployment
then gets a `RenderFailed` warning Event until the status converges.

## Operations

- Replicas serve on port `8000` unless *`roles[].ports`* or `--port` sets another container port.
- If you pass only `--port`, the operator updates the container port and the `Service`
  *`targetPort`*. The external `Service` port stays `8000`.
- Managed native-vLLM decoders put `routing-proxy` on the external port and vLLM on an internal port.
- Transfer engines pick random ports, so allow ephemeral port ranges in `NetworkPolicy`. AscendDirect
  picks ports within `ASCEND_BASE_PORT + physical_id * 100`.
- The startup `ERROR` line `"Local segment descriptor not found"` is benign on clients.

### Readiness and status

Check the phase and the `ROLES` summary, such as `1R2S` or `1R1E1P1D`. Then list the conditions:

```bash
kubectl -n <namespace> get modeldeployment <name>
kubectl -n <namespace> get modeldeployment <name> \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

For per-role readiness, read *`status.roles`* with `-o yaml`. Managed roles probe `GET /health`, or
the sentinel endpoints `/gpustack/v1/live` and `/gpustack/v1/ready`. So `ready` counts replicas whose
engine answered on all *`size`* members.

The operator omits probes for a take-over *`roles[].command`*, a custom `--host`, a conflicting
`--port`, a client-cert `--ssl-cert-reqs` outside `0`/`1`, or `UDP` or `SCTP` ports.

| Condition | Reason | Resolution |
|---|---|---|
| `QuotaReserved` | `Pending`, `PreemptedInPart`, `Parked` | Check pool `ClusterQueue` capacity and `Workload` status. Free capacity on `PreemptedInPart`, or recreate or reactivate `Parked` sets. |
| `QuotaReserved` | `PodGroupIncomplete` | Inspect missing replica `Pod`s and events. Kueue creates no `Workload` for missing replicas. |
| `QuotaReserved` | `NoQueueInReservedNamespace` | Recreate the `ModelDeployment` in a workload namespace. Reserved namespaces have no `LocalQueue` and never schedule workloads. |
| `ReplicasUpToDate` | `UpToDate`, `RolloutInProgress`, `ReplacementInProgress`, `RolloutHeldByCache`, `RolloutHeldByWeights` | Wait for one-per-pass turnover. If a rollout is held, repair the `KVCachePoolBinding` or the `ModelArtifact` (`WeightsReady`). |
| `RoleKindsReady` | `AllKindsReady`, `KindsNotReady` | Inspect `Pod` logs and events for any role kind (`Encode`, `Prefill`, `Decode`, `Server`) with zero ready replicas. |
| `CacheAttached` | `CacheActive`, `CacheOperationsFailing`, `NoObservationAvailable`, `Unmanaged` | On `CacheOperationsFailing`, inspect engine and `KVCacheBackend` logs. `NoObservationAvailable` is normal while idle. |

For `RouterReady`, `EndpointEligibility` and per-group router qualification, see
[Model Deployment Routing](/gpustack-operator/v0.12.0/docs/modules/model-deployment/routing/index.md#troubleshooting).

---

**See also** — [KV Cache Pools and Bindings](/gpustack-operator/v0.12.0/docs/modules/kv-cache/pool/index.md) (namespace Binding grants and reuse domains) · [Elastic EP](/gpustack-operator/v0.12.0/docs/modules/model-deployment/elastic-ep/index.md) (in-place collective expansion) · [Prefill and Decode](/gpustack-operator/v0.12.0/docs/modules/model-deployment/prefill-decode/index.md) (pairing *prefill* and *decode* roles) · [Accelerator Requests](/gpustack-operator/v0.12.0/docs/modules/devices/requests/index.md) (request fields mirrored in *`roles[].resources`*) · [Model Deployment Routing](/gpustack-operator/v0.12.0/docs/modules/model-deployment/routing/index.md) (endpoint eligibility and router policies) · [Online Runtime Control](/gpustack-operator/v0.12.0/docs/modules/model-deployment/runtime-control/index.md) (live parameter tuning and role pause) · [Model Deployment Scaling](/gpustack-operator/v0.12.0/docs/modules/model-deployment/scaling/index.md) (`ModelDeploymentScaler` and graceful retirement).

**Next** → [Accelerator Requests](/gpustack-operator/v0.12.0/docs/modules/devices/requests/index.md)
