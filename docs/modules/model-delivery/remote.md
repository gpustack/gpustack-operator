# Remote Weight Delivery

Remote weight delivery streams model tensors from a prepared source `Node`'s host RAM directly into
serving replicas without writing a full checkpoint onto the target `Node`'s disk. Sources register
verified node-cache weights, targets mount a metadata-only CSI view, and a cluster-wide `Setting`
governs activation.

![Remote weight transfer between nodes](../../assets/diagrams/model-delivery-remote.svg)

<!--
flowchart TD
    TCP_SRC["Default TCP: node-01 source disk"] -> TCP_DISK["node-02: write to local disk"] -> TCP_DRAM["node-02: load from disk into Device DRAM"]
    HIXL_SRC["HIXL / NIXL: node-01 source Host RAM"] -> HIXL_RAM["node-02: stream directly into Host RAM"] -> HIXL_DRAM["node-02: transfer from Host RAM into Device DRAM"]
-->

*H2H* means Host-to-Host and *H2D* means Host-to-Device. Bounded host-memory windows are reused
across batches so successful remote loads write zero checkpoint shards to target storage.

## Contents

- [Configuration](#configuration)
- [Source preparation](#source-preparation)
- [Replica startup](#replica-startup)
- [Hardware requirements](#hardware-requirements)
- [NVIDIA ModelExpress](#nvidia-modelexpress)
- [Results](#results)
- [Verification](#verification)
- [Troubleshooting](#troubleshooting)

## Configuration

The `model-artifact-remote-delivery-policy` `Setting` controls cluster-wide activation:

| Value | Behavior | Requirements |
|---|---|---|
| **`Prefer`** (default) | Streams remotely when a prepared source is `SourceReady`; falls back to local delivery otherwise. | `model.csi.gpustack.ai` `CSIDriver` for metadata mounts. |
| `Require` | Streams remotely and waits within startup limits; never downloads locally. | `model.csi.gpustack.ai` `CSIDriver` and an allowlisted role/backend. |
| `Disabled` | Disables remote delivery, leaving `Pod` specs and status fields unchanged. | None (`modelTransfer.enabled: false` seeds `Disabled` on start). |

`modelTransfer.images` configures per-vendor source `DaemonSet`s (`nvidia`, `amd`, `ascend`, matched
by `pci-<vendor>.present` under `OnDelete`). Use cert-manager when rendering manifests offline
(`helm template | kubectl apply`) so repeated syncs do not rotate source TLS certificates.

Remote delivery applies to **hub `ModelArtifact`s on managed `vLLM` and `SGLang` roles** (`nixl` on
NVIDIA and `hixl` on Ascend). Take-over *`roles[].command`*, PVC, and OCI image artifacts keep their
local paths. Treat source `Pod`s and vendor images as trusted cluster infrastructure.

### Fallback policies

| Policy | Allowlisted, source ready | Allowlisted, source unavailable | Outside allowlist | Ineligible role |
|---|---|---|---|---|
| `Require` | Loads remote | Waits within startup timeout; never downloads | Blocked (`UnqualifiedTransferBackend`) | Blocked (`RoleIneligibleForModelTransfer`) |
| `Prefer` | Loads remote | Uses local delivery unless a remote replica is running | Falls back unless rolling a loaded replica | Falls back to local delivery |

Under `Prefer`, failed remote loads recreate the replica and increment *`fallbackAttempts`*. After
three attempts, *`reason`* turns `FallbackAttemptsSpent` and local `Node` or `Engine` delivery runs;
loaded remote replicas keep their spec if the source disappears.

### Control and payload networks

Control traffic uses authenticated broker HTTPS, while payload traffic uses UCX/InfiniBand or shared
memory (`nixl`) and CANN fabric (`hixl`). Pass NIC selection variables on sources via
`modelTransfer.env` or `modelTransfer.extraArgs`, and apply
[RDMA selection policies](../rdma/operations.md#selection-policy) on serving targets.

## Source preparation

Add `spec.source: {}` and explicit *`placement`* on a [Model Prefetch](prefetch.md) to pin verified
disk weights (*`status.readyNodes`*) and register a host-memory source (*`status.transfer`*).

Source admission enforces the minimum of three bounds (including `10%` reservation overhead): the
broker's node budget, the per-generation resident window, and any shared Binding
*`spec.sourceMemory`* grant. Checkpoints larger than the window page in batches; the window must fit
at least `1 GiB` and the largest safetensors shard (shards are never split):

```yaml
modelTransfer:
  source:
    maxMemoryBytes: 0  # 0 uses 1/16 of MemTotal, floored at minMemoryBytes
    minMemoryBytes: 1073741824  # 1 GiB floor
    maxSessions: 64  # max concurrent sessions per source Pod
  workerResources: {}  # explicit memory limits must include non-transfer overhead
  env:
    GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES: "0"
    GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES: "17179869184"  # 16 GiB HIXL default
```

### Source memory examples

Label source `Node`s `gpustack.ai/model-transfer-source: "true"`, optionally cap *`sourceMemory`* on
`ModelStoreBinding`, and declare `source: {}` on `ModelPrefetch`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelStoreBinding
metadata:
  name: qwen-source
  namespace: gpustack-default
spec:
  storeRefs:
    - name: model-source-pool
  quota:
    bytes: 128Gi  # disk cache budget (separate from source RAM)
  allowPinned: true
  sourceMemory:  # optional; omit to use node-derived limits
    bytes: 64Gi  # shared across prefetches, nodes, and vendors; monotonic upward
    registrations: 4096  # monotonic upward
    sessions: 64  # editable in both directions
---
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: qwen3-8b-source
  namespace: gpustack-default
spec:
  artifactRef:
    name: qwen3-8b
  bindingRef:
    name: qwen-source
  placement:
    nodeSelector:
      matchLabels:
        gpustack.ai/model-transfer-source: "true"
  retention:
    pinned: true
  source: {}  # registers host-memory source alongside disk cache
```

| Model | Checkpoint | 16 GiB budget | Paging behavior |
|---|---|---|---|
| `Qwen3-8B` | `15.27 GiB` | Pages (`~14.54 GiB` net after `10%` overhead) | Fits whole when broker budget exceeds `~17.6 GiB`. |
| `Qwen3.6-35B-A3B` | `66.99 GiB` | Pages resident batches | Window must fit largest shard; `64 GiB` Binding grant cannot widen a `16 GiB` resident cap. |

Keep source disk caches intact for page-in and restore. Drain and delete `OnDelete` source `Pod`s
after chart flag or env changes, and keep operator, source SDK, and engine SDK on contract version `2`.

## Replica startup

Remote-enabled replicas mount a metadata-only CSI view (`model-transfer.gpustack.ai/delivery: metadata`),
a `gpustack-model-transfer` token, and the host-staged wheel when absent. Tune *tensor parallelism
(TP)* mode and target staging RAM in *`spec.roles[].env`*:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen3-8b
   namespace: gpustack-default
 spec:
   roles:
     - kind: Server
       name: server
+      env:
+        - name: GPUSTACK_MODEL_TRANSFER_TP_MODE
+          value: broadcast  # broadcast (or tp0): rank 0 streams and broadcasts; all: every rank opens a session
+        - name: GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES
+          value: "8589934592"  # 8 GiB target staging cap (split across ranks in all mode; 1 GiB/rank floor)
```

The client caps pinned staging RAM at the minimum of its window, the source window, and the tensor
span. It closes the session after `load_weights`, retries peers on HTTP `409`, and leaves loaded
replicas untouched when a source drains.

## Hardware requirements

### Ascend

Ascend (`hixl`) supports `vLLM` and `SGLang` on CANN `9.2+` images (remove stale preinstalled
`gpustack_model_transfer` wheels so the host-staged wheel loads). On Ascend A5
(*`status.detail.family`* `"950"`):

- **UB / UB H2H / UBG / UBoE**: Keep `modelTransfer.ascend.hostRoCE.enabled: false` and target
  `interface: 0`. Set `ASCEND_GLOBAL_RESOURCE_CONFIG` (`"comm_resource_config.protocol_desc"`:
  `["ub_ctp:host"]`, `["ub_rtp:device"]`, or `["uboe:device"]`) in `modelTransfer.env` and
  *`roles[].env`* for non-default UB modes.
- **Host RoCE (`roce:host`)**: Prepare
  [Ascend host prerequisites](../../getting-started/vendor-prerequisites.md#ascend), enable
  `modelTransfer.ascend.hostRoCE.enabled: true` on the source `DaemonSet`, and set
  `resources.interface >= 1` on `Server` or symmetric *prefill/decode (PD)* roles (`Encode` and
  asymmetric *PD* are refused):

```yaml
modelTransfer:
  nodeSelector:
    ascend.gpustack.ai/generation: a5  # drain non-matching nodes before narrowing; applies to all vendors
  ascend:
    hostRoCE:
      enabled: true  # mounts RO /etc/hixlep, enables hostNetwork, and defaults roce:host
```

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen3-8b-a5-roce
  namespace: gpustack-default
spec:
  model:
    name: Qwen/Qwen3-8B
    artifactRef:
      name: qwen3-8b
  engine:
    name: vLLM  # vLLM or SGLang on CANN 9.2+
    image: example/vllm-ascend:cann9.2
  roles:
    - name: server
      replicas: 1
      acceleratorType: atlas-950  # status.detail.family: "950"
      resources:
        accelerator: 1
        interface: 1  # grants RDMA, RO /etc/hixlep, and roce:host default
```

### NVIDIA

NVIDIA (`nixl`) supports `vLLM` and `SGLang`, choosing `LIBFABRIC` when `aws-efa-enabled` is `true`
or an attached `KVCachePool` uses `EFA`, and `UCX` otherwise. Supply a custom EFA-built source image
in `modelTransfer.images.nvidia` on AWS EFA clusters.

## NVIDIA ModelExpress

ModelExpress streams GPU tensors peer-to-peer over NIXL device-to-device (D2D) transfers and shares
`torch.compile` and Triton caches across NVIDIA `vLLM` and `SGLang` replicas (HIXL D2D is not
supported).

![ModelExpress peer and source delivery](../../assets/diagrams/model-delivery-modelexpress.svg)

<!--
flowchart LR
    subgraph SOURCE["ModelPrefetch source"]
        HOST["Source host memory"]
    end
    subgraph DEPLOYMENT["ModelDeployment"]
        SERVER["In-memory gRPC metadata server"]
        subgraph SEED["Seed replica (Replica 0)"]
            PGPU["Device DRAM (GPU tensors)"]
            PCACHE["Compile cache (torch.compile / Triton)"]
        end
        subgraph PEER["Peer replica (Replica 1)"]
            TGPU["Device DRAM (GPU tensors)"]
            TCACHE["Compile cache (torch.compile / Triton)"]
        end
    end
    SEED ->|"Tensor descriptors and cache manifests"| SERVER
    SERVER ->|"Peer and cache discovery"| PEER
    PGPU ->|"RdmaStrategy: NIXL D2D tensor stream"| TGPU
    PCACHE ->|"NIXL compile cache transfer"| TCACHE
    HOST ..>|"Seed replica or restart fallback: GpustackLoadStrategy (NIXL H2H + H2D)"| SEED
    HOST ..>|"Seed replica or restart fallback: GpustackLoadStrategy (NIXL H2H + H2D)"| PEER
-->

### Role configuration

Pass `--load-format=modelexpress` (or `mx`) in *`roles[].extraArgs`* on a Hugging Face-backed role:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen3-8B  # required
    artifactRef:
      name: qwen-weights  # required Hugging Face ModelArtifact
  engine:
    name: vLLM  # required; enum: vLLM, SGLang
    image: example/engine:tag  # required
  roles:
    - name: server
      replicas: 2
      acceleratorType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: 1
      extraArgs:
        - --load-format=modelexpress  # mx is also accepted
```

Each *TP* rank tries `RdmaStrategy` (NIXL D2D from a matching *TP*/*PP* peer) before
`GpustackLoadStrategy` (prefetch source), falling back locally only under `Prefer`. User `MX_*`
entries in *`roles[].env`* override defaults (`MX_ARTIFACT_TRANSFER: "0"` disables compile-cache
sharing).

### Metadata server

The operator deploys an unauthenticated in-memory gRPC metadata server and `Service`
(`<md-name>-modelexpress`) in the deployment namespace; restrict network access to trusted `Pod`s.
Server restarts clear descriptors without interrupting running replicas; the next replica reloads via
`GpustackLoadStrategy` and re-registers. Pin or mirror the server image via `Setting`:

```yaml
apiVersion: gpustack.ai/v1
kind: Setting
metadata:
  name: model-deployment-modelexpress-server-image
  namespace: gpustack-system  # use the system namespace
spec:
  value: gpustack/mirrored-modelexpress-server:v0.8.0  # default image
```

## Results

Tables compare **`Weight Transfer Time`** (slowest rank load) and **`Target Storage`** (`0 GiB` on
remote rows) against a cold **`TCP peer sync + local load` phase-sum baseline**. Source preparation,
scheduling, and warm-up are excluded:

- **Choose `TP0 + broadcast` when source RAM is smaller than the checkpoint**: Rank 0 streams shards
  sequentially (`1.32×`–`4.26×` faster than the `TCP peer sync + local load` sum). **Exception**: do
  not use `TP all` under a `16 GiB` paged window: concurrent rank paging regresses to **`0.28×`**
  (`1380.52 s` vs `393.14 s` baseline) on 80B NVIDIA and trails `TP0 + broadcast` on 35B Ascend
  (`2.79×` vs `4.26×`).
- **Choose `TP all` when source RAM holds the full checkpoint**: Parallel per-rank sessions cut
  `Weight Transfer Time` by `4.84×`–`16.21×` on NVIDIA (eager mode) and `2.57×`–`7.69×` on Ascend
  (eager mode; **confounder**: Ascend 235B `16 GiB` paged and `256 GiB` full-resident runs used
  different SDK builds).
- **Choose ModelExpress D2D for multi-replica NVIDIA scale-out**: Peer D2D loads 235B weights in
  `16.78 s` (`66.82×` vs the `1121.30 s` baseline, `0 GiB` host transfer RAM) and cuts
  `torch.compile` from `77.67 s` to `0.28 s` (`277.39×`), **confounded** against eager-mode NIXL
  runs because ModelExpress runs in graph mode (`296.82 s` → `90.25 s` total engine init, `3.29×`).

### Ascend (HIXL)

Two Ascend 910B2 `Node`s, vLLM-Ascend `cann9.2-910b-vllm0.30.0`, eager mode (`8B` = `Qwen/Qwen3-8B`
at *TP* = 2; `35B` = `Qwen/Qwen3.6-35B-A3B` and `235B` = `vllm-ascend/Qwen3-235B-A22B-W8A8` at
*TP* = 8 plus *expert parallelism (EP)*; 235B `16 GiB` and `256 GiB` rows used different SDK builds):

| Model | Mode | Source RAM | Target RAM | Target Storage | Weight Transfer Time |
|---|---|---|---|---|---|
| 8B (*TP* = 2) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 15.27 GiB | 82.90 s (baseline) |
| 8B (*TP* = 2) | TP0 + broadcast (16 GiB source, 8 GiB target) | 15.26 GiB | 3.72 GiB | 0 GiB | 15.31 s (5.41×, −81.5% time) |
| 8B (*TP* = 2) | TP all (16 GiB source, 8 GiB target) | 15.26 GiB | 7.45 GiB | 0 GiB | 10.78 s (7.69×, −87.0% time) |
| 35B (*TP* = 8 + *EP*) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 66.99 GiB | 282.60 s (baseline) |
| 35B (*TP* = 8 + *EP*) | TP0 + broadcast (16 GiB source, 8 GiB target) | 15.66 GiB | 3.72 GiB | 0 GiB | 66.33 s (4.26×, −76.5% time) |
| 35B (*TP* = 8 + *EP*) | TP all (16 GiB source, 8 GiB target) | 15.66 GiB | 8.00 GiB | 0 GiB | 101.39 s (2.79×, −64.1% time) |
| 35B (*TP* = 8 + *EP*) | TP all (80 GiB source, 64 GiB target) | 67.05 GiB | 29.81 GiB | 0 GiB | 46.39 s (6.09×, −83.6% time) |
| 235B (*TP* = 8 + *EP*) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 220.54 GiB | 599.05 s (baseline) |
| 235B (*TP* = 8 + *EP*) | TP0 + broadcast (16 GiB source, 8 GiB target) | 15.99 GiB | 4.00 GiB | 0 GiB | 452.83 s (1.32×, −24.4% time) |
| 235B (*TP* = 8 + *EP*) | TP0 + broadcast (256 GiB source, 32 GiB target) | 220.67 GiB | 4.00 GiB | 0 GiB | 253.58 s (2.36×, −57.7% time) |
| 235B (*TP* = 8 + *EP*) | TP all (256 GiB source, 32 GiB target) | 220.67 GiB | 32.00 GiB | 0 GiB | 232.93 s (2.57×, −61.1% time) |

### NVIDIA (NIXL)

Nebius H100 80 GB `Node`s, vLLM `0.30.0`, eager mode (`80B` = `Qwen/Qwen3-Next-80B-A3B-Instruct-FP8`
at *TP* = 4; `235B` = `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` at *TP* = 8 + *EP*):

| Model | Mode | Source RAM | Target RAM | Target Storage | Weight Transfer Time |
|---|---|---|---|---|---|
| 80B (*TP* = 4) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 76.45 GiB | 393.14 s (baseline) |
| 80B (*TP* = 4) | TP0 + broadcast (16 GiB source, 8 GiB target) | 16.00 GiB | 8.00 GiB | 0 GiB | 160.59 s (2.45×, −59.2% time) |
| 80B (*TP* = 4) | TP all (16 GiB source, 8 GiB target) | 16.00 GiB | 8.00 GiB | 0 GiB | 1380.52 s (0.28×, +251.2% time) |
| 80B (*TP* = 4) | TP all (80 GiB source, 64 GiB target) | 76.42 GiB | 40.00 GiB | 0 GiB | 81.16 s (4.84×, −79.4% time) |
| 235B (*TP* = 8 + *EP*) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 220.21 GiB | 1121.30 s (baseline) |
| 235B (*TP* = 8 + *EP*) | TP0 + broadcast (16 GiB source, 8 GiB target) | 16.00 GiB | 8.00 GiB | 0 GiB | 287.97 s (3.89×, −74.3% time) |
| 235B (*TP* = 8 + *EP*) | TP0 + broadcast (256 GiB source, 64 GiB target) | 220.19 GiB | 9.31 GiB | 0 GiB | 108.89 s (10.30×, −90.3% time) |
| 235B (*TP* = 8 + *EP*) | TP all (256 GiB source, 32 GiB target) | 220.19 GiB | 32.00 GiB | 0 GiB | 69.16 s (16.21×, −93.8% time) |

### ModelExpress GPU transfers

Nebius H100 80 GB `Node`s, vLLM `0.30.0`, graph mode on `235B (TP8 + EP)`
(`Qwen/Qwen3-235B-A22B-Instruct-2507-FP8`):

| Model | Mode | Source RAM | Target RAM | Target Storage | Weight Transfer Time |
|---|---|---|---|---|---|
| 235B (TP8 + EP) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 220.21 GiB | 1121.30 s (baseline) |
| 235B (TP8 + EP) | Seed replica: `GpustackLoadStrategy` (256 GiB source, 8 GiB target) | 220.19 GiB | 8.00 GiB | 0 GiB | 232.10 s (4.83×, −79.3% time) |
| 235B (TP8 + EP) | Peer replica: `RdmaStrategy` (NIXL D2D + compile cache) | 0 GiB | 0 GiB | 0 GiB | 16.78 s (66.82×, −98.5% time) |
| 235B (TP8 + EP) | Restart fallback: `GpustackLoadStrategy` (256 GiB source, 8 GiB target) | 220.19 GiB | 8.00 GiB | 0 GiB | 109.08 s (10.28×, −90.3% time) |

## Verification

Inspect per-role and per-replica remote loading state on `ModelDeployment`:

```bash
kubectl -n <ns> get modeldeployment <name> -o jsonpath='{.status.model.remote}' | jq
```

*`status.model.remote`* echoes the active *`policy`* and each replica's *`delivery`* (`Remote`,
`Node`, or `Engine`), *`load`* (`Unknown`, `AwaitingMetadata`, `AwaitingSource`, `Loading`, or
`Loaded`), *`reason`*, and *`fallbackAttempts`*. Source readiness lives on `ModelPrefetch`
([Prefetch status](prefetch.md#prefetch-status)).

## Troubleshooting

| Observation | Cause | Resolution |
|---|---|---|
| `SourceWindowFallback` (`Progressing=True`) on prefetch | Source grant below `1 GiB` floor under `Prefer` | Raise `ModelStoreBinding` *`sourceMemory`* or broker RAM budget. |
| `SourceWindowBelowFloor` (`Progressing=False`) on prefetch | Source grant below `1 GiB` floor under `Require` | Raise `ModelStoreBinding` *`sourceMemory`* or broker `maxMemoryBytes`. |
| `SourceGrantMissing` (`Progressing=False`) on prefetch | Missing `ModelStoreBinding` | Create the `ModelStoreBinding` named in *`spec.bindingRef`*. |
| `load: AwaitingSource` | No `SourceReady` prefetch serves the digest | Inspect `ModelPrefetch` *`status.transfer`* and source `Pod` logs. |
| `load: AwaitingMetadata` | Metadata CSI mount pending | Inspect `Pod` `FailedMount` events and `model.csi.gpustack.ai`. |
| `RemoteLoadFailed` (`load: Unknown`) | Remote load exited; replica recreating | Inspect previous container logs and *`fallbackAttempts`*. |
| `FallbackAttemptsSpent` | Remote load failed more than 3 times under `Prefer` | Diagnose fallback `Node` or `Engine` delivery on the recreated `Pod`. |
| `RoleIneligibleForModelTransfer` or `UnqualifiedTransferBackend` | Custom command, unsupported engine, or unallowlisted backend under `Require` | Switch to a managed NVIDIA (`nixl`) or Ascend (`hixl`) role, or set policy to `Prefer`. |
| `WeightsReady=False` (`NodeDeliveryUnavailable`) | `Require` active without `model.csi.gpustack.ai` | Enable `modelManager.enabled: true` or roll back remote policy. |

### Rollback

Patch `model-artifact-remote-delivery-policy` to `Disabled` (or set `modelTransfer.enabled: false`
in Helm values):

```diff
 apiVersion: gpustack.ai/v1
 kind: Setting
 metadata:
   name: model-artifact-remote-delivery-policy
   namespace: gpustack-system
 spec:
-  value: Prefer
+  value: Disabled
```

New replicas revert to `model-artifact-delivery-mode` (`Node` or `Engine`) while already-loaded
replicas continue serving until their next rollout.

---

**See also** — [Model Prefetch](prefetch.md) (source preparation and `ModelStoreBinding` grants) · [Model Store Operations](operations.md) (delivery `Setting`s and CSI operations) · [Model Artifact](artifact.md) (digest identity) · [Model Deployment Configuration](../model-deployment/configuration.md) (serving roles) · [Engine Compatibility](../model-deployment/engine-versions.md) (supported runner versions)

**Next** → [Model Artifact](artifact.md) — review weight source contracts and resolution rules.
