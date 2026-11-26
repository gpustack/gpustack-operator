# Prefill and Decode

A *prefill/decode (PD)* deployment runs *Prefill* and *Decode* roles in separate `Pod`s. The
operator admits all replicas as one set and links them through a managed router and a direct KV
transfer path. See [Prefill and decode](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#prefill-and-decode) for base role fields.

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

- [Configuration](#configuration)
- [Router configuration](#router-configuration)
- [Direct KV transports](#direct-kv-transports)
- [Hardware requirements](#hardware-requirements)
- [Role placement and services](#role-placement-and-services)

## Configuration

A direct *PD* pair needs *`spec.router`* and no *`spec.kvCache`*:

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
    name: vLLM  # enum: vLLM, SGLang
    image: <a runner image your cluster can pull>  # required
  router:
    name: llm-d-router  # required to pair Prefill and Decode
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

- **One Kueue group per replica**: Each replica forms its own `Workload` of size *`roles[].size`*
  (four single-`Pod` groups above), held together by an `AdmissionCheck` until all groups reserve
  quota.
- **Replica blast radius**: Preemption, eviction or rollout of one replica leaves sibling replicas
  and the peer role serving. Replacement starts only after the departed `Pod` disappears from the
  API server.

> **Why replacement waits for an empty slot** — Kueue keeps its finalizer on a departed serving
> `Pod` until that replica's `Workload` is deleted. Creating a replacement while the old `Pod` is
> still listed makes the group exceed its declared size, and Kueue deletes the newcomer.

To share prefixes across deployments as well, add *`spec.kvCache`* on first apply (the block is
frozen after creation):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-pd
   namespace: team-a
 spec:
+  kvCache:
+    poolRef:
+      name: team-a-dram  # KVCachePoolBinding in this namespace
```

## Router configuration

***`spec.router`* pairs the roles; *`spec.kvCache`* does not.** A *PD* deployment without
*`spec.router`* is admitted, but each role receives only its shared-store discriminator and no
request splits across roles.

Admission matches *`spec.router.name`* to *`spec.engine.name`* and rejects cross pairings:

| Router | Engines | Rendered configuration | Routing policy |
|---|---|---|---|
| `llm-d-router` | `vLLM`, `SGLang` | Endpoint picker behind a proxy with a mounted config file | [Fixed scoring profile](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#fixed-scoring-profile) |
| `vllm-router` | `vLLM` | Single process configured by CLI flags | [`cache_aware`, or `--policy`](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#policy-overrides) (supports `kv_aware` at `>= 0.31.0`) |
| `sgl-router` | `SGLang` | Single process configured by CLI flags with `EndpointSlice` discovery | [SGLang native policies and KV events](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#policy-overrides) |
| `sglang-gateway` (deprecated, frozen at `gateway-v0.3.1`) | `SGLang` | Single process configured by CLI flags | [`cache_aware`, or `--policy`](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#policy-overrides) |

- **Router replicas**: *`spec.router.replicas`* defaults to `1`. Running `> 1` trades prefix-cache
  hit rate for availability (upstream reports unsynchronized radix trees and a 10%–20% hit-rate
  drop).
- **Owned router flags**: Admission refuses operator-derived flags in *`spec.router.extraArgs`*. On
  `llm-d-router`: `--endpoint-selector`, `--endpoint-target-ports`, `--config-file`,
  `--secure-serving`, `--grpc-health-port`, and `--metrics-endpoint-auth`. On `vllm-router`,
  `sgl-router`, and `sglang-gateway`: bind addresses/ports, service-discovery flags, disaggregation
  flags, `vllm-router`'s `--kv-connector`, and `sgl-router`'s `--model-id`, `--chat-routing`,
  `--no-tokenizer`, and `--dp-aware`.
- **Fixed east-west flags**: `llm-d-router` fixes `--secure-serving=false` (loopback gRPC inside
  the `Pod`) and `--metrics-endpoint-auth=false` (in-cluster scrape). Enforce caller TLS and auth at
  the north-south cluster gateway.

### Router fields

- *`spec.router.requestTimeoutSeconds`*: Sets router reply timeout (`> 0`; `0` is refused). If
  omitted, upstream defaults diverge: 1 day on `llm-d-router` versus 30 minutes on `vllm-router`,
  `sgl-router`, and `sglang-gateway`.
- *`spec.router.disaggregationThresholdTokens`*: Supported on `llm-d-router` only (refused on other
  routers). Sets uncached prompt tokens needed to split a request; `0` disables splitting.
- **Access logs**: The `llm-d-router` proxy writes one JSON access record per request to stdout
  (status, flags, duration, chosen endpoint, method, path, request ID, bytes). Other routers follow
  their own logging flags.

### Router token counting

**Non-*Encode* vLLM deployments behind `llm-d-router` require `--enable-scale-out` on every role.**
The router counts prompt tokens via `POST /v1/chat/completions/render`, which vLLM exposes only
under `--enable-scale-out` (floor: vLLM `0.30.0`, vLLM-Ascend `0.30.0rc1`). Admission does not
check for the flag, and the operator does not inject it.

| Declared roles | Token counting | Flag needed | Rendered flag |
|---|---|---|---|
| *Encode*, *Prefill*, *Decode* | Router built-in estimator; no HTTP call | No | None |
| *Prefill*, *Decode* | Engine `/v1/chat/completions/render` call | Yes | None |
| Single *Server* role | Engine `/v1/chat/completions/render` call | Yes | None |

**Without `--enable-scale-out`, disaggregation fails silently.** Render calls return `404`, the
router sends every request to *Decode* alone, clients receive `200`, and *Prefill* receives zero
completions ([Verification](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md#verification)).

Add the flag to both roles of a vLLM *PD* deployment:

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

Do not add `--enable-scale-out` to images below the documented floor; older vLLM builds exit on the
unknown flag.

### Router compatibility

Pick an engine image meeting [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md#minimum-versions). Other
router paths:

| Router path | `--enable-scale-out` status | Configuration path |
|---|---|---|
| `llm-d-router` + `vLLM` / `vLLM-Ascend` | Required on `>= 0.30.0` (non-*Encode*) | Publishes KV events (`kv@pod@<model>`) on *Server* and *Prefill* |
| `vllm-router` + `vLLM` / `vLLM-Ascend` | Not established | Uses router *PD* mode and selectors; `kv_aware` (`>= 0.31.0`) enables KV events; *encode/prefill/decode (EPD)* renders [`--epd-config`](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#encode-stage-routing) |
| `sgl-router` + `SGLang` | Not applicable | Uses `EndpointSlice` selectors, SGLang native disaggregation flags, and native KV events (`kv-events`) on *Server* and *Prefill*; *EPD* uses `--encoder-urls` |
| `sglang-gateway` + `SGLang` (deprecated) | Not applicable | Frozen at `gateway-v0.3.1`; uses `--pd-disaggregation` and SGLang native disaggregation flags |
| `llm-d-router` + `SGLang` | Not applicable; token path unmeasured | Uses `approx-prefix-cache-producer` (no KV events) and SGLang native disaggregation flags |

## Direct KV transports

*`spec.kvTransfer.backend`* selects `Mooncake`, `NIXL` or `HIXL`. If omitted, it defaults to
`Mooncake` on non-Ascend pools and `HIXL` on Ascend pools.

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-pd
   namespace: team-a
 spec:
+  kvTransfer:
+    backend: NIXL
```

- **Backend refusals**: Admission rejects `HIXL` off Ascend (or CPU-only), `NIXL` on Ascend, and
  mixed Ascend/non-Ascend *PD* pairs (see [Admission refusals](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#admission-refusals)).
- **Fabric derivation**: Mooncake selects `ascend` on Ascend, `efa` when both roles set
  *`resources.interface > 0`* under EFA, `hip` on AMD ROCm, `rdma` when both roles set
  *`resources.interface > 0`*, and `tcp` otherwise. NIXL selects `LIBFABRIC` when `aws-efa-enabled`
  is `true` or a bound pool uses EFA, and `UCX` otherwise.
- **Rollout**: Changing transfer settings rolls both roles; transfers can fail until both halves
  converge. Verify image support in [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md).

### Role links and connectors

- **Leader routing**: Multi-`Pod` *PD* replicas route HTTP to member `0` (`member-index=0`).
- **Combined pool + direct KV**: On vLLM pairs with *`spec.kvCache`*, *Prefill* wraps the direct
  connector first in `MultiConnector` beside the store connector; *Decode* runs the direct connector
  alone while keeping pool dtype and block-size alignment.
- **Ascend connector generation**: On `vLLM-Ascend`, effective engine version `>= 0.30.0` renders
  `MooncakeConnectorV2` (Pull mode) and relaxes *TP* validation to
  `max(prefill_tp, decode_tp) % min(prefill_tp, decode_tp) == 0`. Versions `< 0.30.0` and legacy
  versionless `ModelDeployment`s keep `MooncakeConnectorV1` (`prefill_tp >= decode_tp`). Admission
  refuses mixed V1/V2 pairs where one role is `< 0.30.0` and the other is `>= 0.30.0`.
- **Sidecar and handshakes**: Under `llm-d-router`, *Decode* runs a `routing-proxy` sidecar using
  `--kv-connector=nixlv2` (vLLM NIXL or Ascend), `mooncake` (vLLM Mooncake), or `sglang` (SGLang).
  Ascend vLLM pairs also render behind `vllm-router` with `--kv-connector=nixl`. SGLang uses its
  native disaggregation flags.

### Parallelism declarations

The operator reads each role's parallel degrees from *`roles[].extraArgs`* (managed role) or
*`roles[].command`* (take-over role), plus literal `VLLM_DP_SIZE` in *`env`*. Undeclared degrees
default to `1/1`. On SGLang, prefer `--attn-dp-size` over legacy `--enable-dp-attention`; both are
parsed.

*`resources.accelerator`* requests GPUs for scheduling; it never generates `--tensor-parallel-size`,
`--pipeline-parallel-size` or `--data-parallel-size`. Pass engine parallelism flags explicitly in
*`extraArgs`*.

Flags hidden in an image entrypoint, config file, `sh -c` wrapper, `--additional-config` JSON or
`ConfigMap`/`Secret` `envFrom` are invisible to the operator. A symmetrically hidden pair silently
reads a mismatched KV layout; an asymmetric pair fails startup when *Decode* exceeds *Prefill*.

## Hardware requirements

### Ascend driver mount

On Ascend *PD* pairs, both roles mount host `/usr/local/Ascend/driver` read-only so `hccn_tool` can
resolve NPU Device RoCE addresses. Non-transfer Ascend deployments omit this mount. If a node lacks
the host driver path, `Pod` volume setup fails naming `/usr/local/Ascend/driver`; install the host
Ascend driver on that node first.

### Ascend A5 topologies

On Ascend A5 (`AcceleratorType` *`status.detail.family`* `"950"`, `Ascend950PR` and `Ascend950DT`),
the operator selects on-die UnifiedBus (*UB*) / Unified Remote Memory Access (*URMA*) versus Linux
Host RDMA (*Host RoCE*) from the engine and *`roles[].resources.interface`*. Ascend A2 and A3 keep
legacy rendering.

- **UB, UBG and UBoE (`interface: 0`)**: Omit *`resources.interface`* on `fabric.kind=ub` nodes
  ([RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md)). Pass `ASCEND_GLOBAL_RESOURCE_CONFIG` in *`roles[].env`*
  for UBG (`ub_rtp:device`) or UBoE (`uboe:device`).
- **Host RoCE (`interface > 0`)**: Set *`resources.interface > 0`* on both *Prefill* and *Decode*
  (or `KVCache` *Server*) for vLLM roles with converged `"950"`. The operator requests
  `device.gpustack.ai/rdma.shared` (`1`) or `device.gpustack.ai/rdma` (`> 1`), mounts `/etc/hixlep`
  read-only and sets `ascend_local_comm_res_path`. Provision host endpoint files first
  ([Ascend prerequisites](/gpustack-operator/main/docs/getting-started/vendor-prerequisites/index.md#ascend)).
- **Constraints**: Asymmetric *PD* interfaces fail admission. *Encode* and SGLang never receive
  `/etc/hixlep` on KV paths (SGLang A5 *PD* rejects *`interface > 0`* unless a bound `KVCache` pool
  uses `RDMA`). Static-UB take-over via *`roles[].command`* requires
  `instance-host-path-volume-allowed: "true"`.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-a5-roce
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct
  engine:
    name: vLLM  # Host RoCE /etc/hixlep applies to vLLM only
    image: <vllm-ascend runner image>
  kvCache:
    poolRef:
      name: ascend-shared-kv  # homogeneous CANN KVCachePoolBinding
  kvTransfer:
    backend: HIXL
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      acceleratorType: atlas-950  # converged status.detail.family: "950"
      resources:
        accelerator: 1
        interface: 1  # required on both Prefill and Decode for Host RoCE
    - name: decode
      kind: Decode
      replicas: 1
      acceleratorType: atlas-950
      resources:
        accelerator: 1
        interface: 1  # grants device.gpustack.ai/rdma.shared
```

An explicit `/etc/hixlep` entry in *`additionalVolumes`* replaces the operator mount while keeping
the connector path. Operator-managed `/etc/hixlep` works with `instance-host-path-volume-allowed`
set to `false`.

### AWS EFA

For AWS EFA, set an EFA-enabled runner in *`spec.engine.image`* or *`roles[].image`*, and an
EFA-enabled `model-transfer` image in Helm `modelTransfer.images.nvidia` for remote weight loading.
Default images exclude the EFA provider.

## Role placement and services

### Different hardware per role

Each role's `Workload` submits to the queue named by its *`acceleratorType`* *`status.entrance`*, so
*Prefill* and *Decode* can target different pools or queues while the `AdmissionCheck` still admits
the deployment atomically (see [Replica admission groups](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#replica-admission-groups) and
[Worker-managed resources](/gpustack-operator/main/docs/operate/installation-modes/index.md#worker-managed-resources)).

Different manufacturers use separate pool queues (`credits.gpustack.ai/<manufacturer>`). Mixed
Ascend/non-Ascend *PD* pairs fail admission; mixed non-Ascend pairs are admitted but unmeasured.

### Role endpoints

Each role publishes a `ClusterIP` `Service` named `<deployment>-<role>` beside the deployment-wide
`Service`, used by the router and direct role debugging.

When *`spec.router`* is set, the operator manages `<deployment>-router` (`Deployment`, `ConfigMap`,
`Service`, `ServiceAccount`, `Role`, `RoleBinding`) and prunes them if *`spec.router`* is removed.
*`status.endpoint`* publishes `http://<deployment>-router.<namespace>.svc:<port>` once a router
replica is ready.

---

**See also** — [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) (role fields, owned keys and admission refusals) ·
[Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (replica selection and endpoint eligibility) ·
[Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md) (supported versions and transports) ·
[Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) (equal-GPU comparison against aggregated serving)

**Next** → [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md)
