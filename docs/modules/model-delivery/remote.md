# Remote Weight Delivery

Remote weight delivery lets an engine load tensors from a prepared source node without first
copying the complete checkpoint to the serving node. The source registers weights verified by its
node cache, and the target mounts a metadata-only view. A cluster-wide policy enables delivery; the
deployment's model block does not.

![Remote weight transfer between nodes](../../assets/diagrams/model-delivery-remote.svg)

<!--
flowchart TD
    TCP_SRC["Default TCP: node-01 source disk"] -> TCP_DISK["node-02: write to local disk"] -> TCP_DRAM["node-02: load from disk into Device DRAM"]
    HIXL_SRC["HIXL / NIXL: node-01 source Host RAM"] -> HIXL_RAM["node-02: stream directly into Host RAM"] -> HIXL_DRAM["node-02: transfer from Host RAM into Device DRAM"]
-->

*H2H* means Host-to-Host; *H2D* means Host-to-Device. Host-memory windows are bounded and reused,
so the successful remote path does not store a complete checkpoint on the target filesystem.

## Contents

- [Configuration](#configuration)
- [Source preparation](#source-preparation)
- [Replica startup](#replica-startup)
- [Require and Prefer](#require-and-prefer)
- [Networks](#networks)
- [Vendor-specific behavior](#vendor-specific-behavior)
- [NVIDIA ModelExpress](#nvidia-modelexpress)
- [Benchmarks](#benchmarks)
- [Observing status](#observing-status)
- [Troubleshooting](#troubleshooting)

## Configuration

One `Setting` chooses the cluster's policy, and the chart ships it:

| `Setting` | Default | Values |
|---|---|---|
| **`model-artifact-remote-delivery-policy`** | **`Prefer`** | `Disabled`, `Require`, `Prefer` |

- **`Prefer`**: Default policy; uses remote delivery when a prepared source is ready and local
  delivery otherwise.
- **`Disabled`**: Turns remote delivery off; leaves `Pod` specs, conditions, and status unchanged.
- **`Require` and `Prefer`**: Require the `model.csi.gpustack.ai` `CSIDriver` for metadata mounts.

```bash
kubectl -n gpustack-system patch setting model-artifact-remote-delivery-policy \
  --type merge -p '{"spec":{"value":"Disabled"}}'
```

`modelTransfer.images` configures full source-worker image references per manufacturer (`nvidia`,
`amd`, `ascend`), rendering one `OnDelete` `DaemonSet` per configured vendor (`pci-<vendor>.present`
`nodeSelector`). Configure cert-manager for offline GitOps rendering (`helm template | kubectl apply`)
so repeated syncs do not rotate the source serving certificate.

Remote delivery targets **hub artifacts on managed roles** (default routing admits `nixl` on NVIDIA
and `hixl` on Ascend for `vLLM` and `SGLang`; production acceptance remains separate). Take-over
*`roles[].command`*, claim, and image artifacts keep their existing paths. Treat source `Pod`s and
vendor images as trusted cluster infrastructure.

## Source preparation

A [Model Prefetch](prefetch.md) with `spec.source: {}` pins a verified disk cache and registers a
host-memory source on nodes matched by `placement` and covered by a `ModelStoreBinding`. Disk
readiness appears in *`status.readyNodes`*; transfer readiness appears in *`status.transfer`*.

Source admission enforces the smallest of three limits: the broker's node budget, the
per-generation resident window, and an optional shared Binding grant (`spec.sourceMemory`),
including `10%` reservation overhead. Both NIXL and HIXL page checkpoints larger than the admitted
window, which must fit at least `1 GiB` and the largest safetensors shard (shards are never split).

```yaml
modelTransfer:
  source:
    maxMemoryBytes: 0           # 0 uses 1/16 of MemTotal, floored at minMemoryBytes
    minMemoryBytes: 1073741824  # 1 GiB floor
    maxSessions: 64             # concurrent sessions across this source Pod
  workerResources: {}           # explicit limits must cover non-transfer allocations too
  env:
    GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES: "0"
    GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES: "17179869184"  # 16 GiB HIXL default
```

### Source memory examples

Label source nodes `gpustack.ai/model-transfer-source: "true"`, grant optional *`sourceMemory`* on
the `ModelStoreBinding`, and enable `source: {}` on `ModelPrefetch`:

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
    bytes: 128Gi  # disk-cache quota; separate from source RAM
  allowPinned: true
  sourceMemory:  # optional; omit to use node-derived limits
    bytes: 64Gi  # shared across models, source nodes and manufacturers
    registrations: 4096
    sessions: 64
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
  source: {}  # register host-memory source alongside disk cache
```

| Model | Checkpoint | 16 GiB ceiling | Notes |
|---|---|---|---|
| `Qwen3-8B` | 15.27 GiB | Fits in full when broker budget exceeds ~17.6 GiB | With a 16 GiB broker budget (~14.54 GiB net after 10% overhead), even 8B pages |
| `Qwen3.6-35B-A3B` | 66.99 GiB | Pages successive resident batches | Admitted window must fit the largest shard; `64 GiB` grant cannot enlarge a `16 GiB` window |

Keep source disk available for page-in and restore. Drain and replace source `Pod`s after chart
argument or environment changes; Binding *`sourceMemory`* grants can grow online, and removing
*`sourceMemory`* restores node-derived limits. Deploy the operator, source SDK, and engine SDK on
contract version `2`.

## Replica startup

The operator renders three additions on a remote replica `Pod`: a metadata-only CSI volume
(`model-transfer.gpustack.ai/delivery: metadata`, serving config/tokenizer/safetensors headers), a
projected `ServiceAccount` token (`gpustack-model-transfer` audience), and the host-staged
`gpustack_model_transfer` wheel installed offline when not already importable.

Configure *TP* loading mode and target staging RAM in *`spec.roles[].env`*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen3-8b
  namespace: gpustack-default
spec:
  roles:
    - kind: Server
      name: server
      env:
        # broadcast (or tp0): rank 0 reads remotely and broadcasts tensors
        # all: each TP rank opens its own remote session
        - name: GPUSTACK_MODEL_TRANSFER_TP_MODE
          value: "broadcast"
        # 8 GiB target staging ceiling; divided across ranks in all mode (1 GiB/rank floor)
        - name: GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES
          value: "8589934592"
```

The loader clamps its pinned-RAM staging window to the smaller of its budget, the source's
advertised window, and the needed tensor span, then closes the session after `load_weights`. On
HTTP `409` session exhaustion, the client tries alternate peers and retries after `Retry-After`.
A drained or quarantined generation stops admitting new sessions while loaded replicas keep serving.

## Require and Prefer

| Policy | Allowlisted, source ready | Allowlisted, source unavailable | Outside allowlist | Ineligible role |
|---|---|---|---|---|
| `Require` | Loads remote | Holds before load starts; never downloads | Blocked (`UnqualifiedTransferBackend`) | Blocked (`RoleIneligibleForModelTransfer`) |
| `Prefer` | Loads remote | Uses declared local delivery unless a remote replica is running | Falls back unless fallback would roll a loaded remote replica | Falls back to declared local delivery |

- **`Require` never downloads**: Replicas wait within startup limits rather than materializing a
  local checkpoint.
- **`Prefer` fallback tracking**: Failed remote loads recreate the replica and increment
  *`fallbackAttempts`*; past three attempts the reason becomes `FallbackAttemptsSpent` and fallback
  proceeds. Verified artifacts keep verified Node delivery on fallback.
- **Loaded replica protection**: The controller holds remote delivery for an already-loaded replica
  even if the source later disappears, avoiding unnecessary rollouts.

## Networks

Control (authenticated broker HTTP/TLS) and payload (UCX/InfiniBand or shared memory for `nixl`;
CANN fabric for `hixl`) endpoints are identified separately. No `Setting` selects a NIC today; pass
backend device environment variables via `modelTransfer.env` / `modelTransfer.extraArgs` on sources,
and use RDMA selection policies on serving targets.

## Vendor-specific behavior

### Ascend

Ascend (`hixl`) admits `vLLM` and `SGLang` and requires CANN `9.2+` engine images (upgrade or omit
stale preinstalled `gpustack_model_transfer` wheels so the host-staged wheel loads). On Ascend A5
(`status.detail.family` `"950"`):

- **UB / UB H2H / UBG / UBoE**: Keep `modelTransfer.ascend.hostRoCE.enabled: false` and target
  `interface: 0`. Pass `ASCEND_GLOBAL_RESOURCE_CONFIG` with `"comm_resource_config.protocol_desc"`
  (`["ub_ctp:host"]`, `["ub_rtp:device"]`, or `["uboe:device"]`) in `modelTransfer.env` and
  *`roles[].env`* when selecting a non-default UB mode.
- **Host RoCE (`roce:host`)**: Set `modelTransfer.ascend.hostRoCE.enabled: true` on the Ascend
  source `DaemonSet` (drains and rolls via `config-hash`) and *`roles[].resources.interface > 0`*
  on `Server` or symmetric *PD* target roles (`Encode` and asymmetric *PD* are rejected). Provision
  [host endpoint files](../../getting-started/vendor-prerequisites.md#a5-host-roce-endpoints) first.
- **Mixed-fleet `nodeSelector` warning**: `modelTransfer.nodeSelector` applies to all vendor source
  `DaemonSet`s (`nvidia`, `amd`, `ascend`). Drain excluded nodes before narrowing it, because the
  Kubernetes `DaemonSet` controller deletes non-matching `Pod`s immediately.

Example source Helm values and target `ModelDeployment` for Ascend A5 Host RoCE:

```yaml
modelTransfer:
  nodeSelector:
    ascend.gpustack.ai/generation: a5  # admin-supplied label matching A5 nodes
  ascend:
    hostRoCE:
      enabled: true  # enables hostNetwork, RO /etc/hixlep, and roce:host default
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
      acceleratorType: atlas-950  # converged status.detail.family: "950"
      resources:
        accelerator: 1
        interface: 1  # grants RDMA, RO /etc/hixlep, and roce:host default
```

### NVIDIA

NVIDIA admits `nixl` for `vLLM` and `SGLang`, selecting `LIBFABRIC` when `aws-efa-enabled` is
`true` or a bound `KVCache` pool uses EFA, and `UCX` otherwise. For AWS EFA, supply an EFA-enabled
source image via `modelTransfer.images.nvidia`; default builds omit EFA.

## NVIDIA ModelExpress

ModelExpress lets NVIDIA `vLLM` and `SGLang` replicas share GPU tensors over NIXL device-to-device
(D2D) transfers and reuse `torch.compile` and Triton caches across replicas (HIXL D2D is outside
this design; hardware qualification remains separate).

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

Add `--load-format=modelexpress` (or `mx`) on a role backed by a Hugging Face `ModelArtifact`:

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
      name: qwen-weights  # required for the prepared source
  engine:
    name: vLLM  # required; vLLM or SGLang
    image: example/engine:tag  # required; use a compatible runner image
  roles:
    - name: server
      replicas: 2
      acceleratorType: gpustack--nvidia-a10g-linux-amd64
      resources:
        accelerator: 1
      extraArgs:
        - --load-format=modelexpress  # required; mx is also accepted
```

- **Peer precedence**: Each *TP* rank tries `RdmaStrategy` (NIXL D2D from a ready peer with
  matching *TP*/*PP* layout) before `GpustackLoadStrategy` (prefetch source), then local delivery
  under `Prefer` only (`Require` permits no local fallback).
- **Overrides**: Any `MX_*` entry in *`roles[].env`* overrides operator-injected defaults (set
  `MX_ARTIFACT_TRANSFER: "0"` to disable compile-cache transfer).

### Metadata server

The operator deploys an unauthenticated in-memory gRPC metadata server and `Service`
(`<md-name>-modelexpress`) in the deployment namespace; restrict access to trusted `Pod`s. A server
restart clears in-memory registrations while existing replicas keep serving, and the next replica
falls back to `GpustackLoadStrategy` and republishes descriptors. Pin or mirror the image via:

```yaml
apiVersion: gpustack.ai/v1
kind: Setting
metadata:
  name: model-deployment-modelexpress-server-image
  namespace: gpustack-system  # use the system namespace
spec:
  value: gpustack/mirrored-modelexpress-server:v0.8.0  # default image
```

## Benchmarks

Benchmarks compare a cold target node running `TCP peer sync + local load` (phase sum, excluding
source preparation, scheduling, and warm-up) against cross-node remote loading and ModelExpress D2D
scale-out. **Weight Transfer Time** is the slowest rank's load duration; **Target Storage** reports
target checkpoint disk usage (`0 GiB` for remote rows). `TP2`, `TP4`, and `TP8` use two, four, and
eight devices.

**Decision summary:**

- **Use `TP0 + broadcast`** when source RAM cannot hold the full checkpoint: it streams sequential
  shards (`2.45×`–`4.26×` faster than baseline), whereas `TP all` thrashes a 16 GiB source window
  (`0.28×` on 80B NVIDIA, `2.79×` vs `4.26×` on 35B Ascend).
- **Use `TP all` with full source residency** when source RAM fits the checkpoint: `4.84×`–`16.21×`
  faster on NVIDIA and `2.57×`–`7.69×` faster on Ascend with zero target disk usage.
- **Use ModelExpress D2D on NVIDIA scale-out** for multi-replica serving: `16.78 s` weight load
  (`66.82×` vs baseline, `0 GiB` host transfer RAM) plus `277.39×` faster `torch.compile`.

### Ascend (HIXL)

Two Ascend 910B2 nodes, vLLM-Ascend `cann9.2-910b-vllm0.30.0`, eager mode (`8B` = `Qwen/Qwen3-8B`,
`35B` = `Qwen/Qwen3.6-35B-A3B`, `235B` = `vllm-ascend/Qwen3-235B-A22B-W8A8`; 235B paged and
full-resident runs used different SDK builds):

| Model | Mode | Source RAM | Target RAM | Target Storage | Weight Transfer Time |
|---|---|---|---|---|---|
| 8B (TP2) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 15.27 GiB | 82.90 s (baseline) |
| 8B (TP2) | TP0 + broadcast (16 GiB source, 8 GiB target) | 15.26 GiB | 3.72 GiB | 0 GiB | 15.31 s (5.41×, −81.5% time) |
| 8B (TP2) | TP all (16 GiB source, 8 GiB target) | 15.26 GiB | 7.45 GiB | 0 GiB | 10.78 s (7.69×, −87.0% time) |
| 35B (TP8 + EP) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 66.99 GiB | 282.60 s (baseline) |
| 35B (TP8 + EP) | TP0 + broadcast (16 GiB source, 8 GiB target) | 15.66 GiB | 3.72 GiB | 0 GiB | 66.33 s (4.26×, −76.5% time) |
| 35B (TP8 + EP) | TP all (16 GiB source, 8 GiB target) | 15.66 GiB | 8.00 GiB | 0 GiB | 101.39 s (2.79×, −64.1% time) |
| 35B (TP8 + EP) | TP all (80 GiB source, 64 GiB target) | 67.05 GiB | 29.81 GiB | 0 GiB | 46.39 s (6.09×, −83.6% time) |
| 235B (TP8 + EP) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 220.54 GiB | 599.05 s (baseline) |
| 235B (TP8 + EP) | TP0 + broadcast (16 GiB source, 8 GiB target) | 15.99 GiB | 4.00 GiB | 0 GiB | 452.83 s (1.32×, −24.4% time) |
| 235B (TP8 + EP) | TP0 + broadcast (256 GiB source, 32 GiB target) | 220.67 GiB | 4.00 GiB | 0 GiB | 253.58 s (2.36×, −57.7% time) |
| 235B (TP8 + EP) | TP all (256 GiB source, 32 GiB target) | 220.67 GiB | 32.00 GiB | 0 GiB | 232.93 s (2.57×, −61.1% time) |

### NVIDIA (NIXL)

Nebius H100 80 GB nodes, vLLM `0.30.0`, eager mode (`80B` = `Qwen/Qwen3-Next-80B-A3B-Instruct-FP8`,
`235B` = `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8`):

| Model | Mode | Source RAM | Target RAM | Target Storage | Weight Transfer Time |
|---|---|---|---|---|---|
| 80B (TP4) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 76.45 GiB | 393.14 s (baseline) |
| 80B (TP4) | TP0 + broadcast (16 GiB source, 8 GiB target) | 16.00 GiB | 8.00 GiB | 0 GiB | 160.59 s (2.45×, −59.2% time) |
| 80B (TP4) | TP all (16 GiB source, 8 GiB target) | 16.00 GiB | 8.00 GiB | 0 GiB | 1380.52 s (0.28×, +251.2% time) |
| 80B (TP4) | TP all (80 GiB source, 64 GiB target) | 76.42 GiB | 40.00 GiB | 0 GiB | 81.16 s (4.84×, −79.4% time) |
| 235B (TP8 + EP) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 220.21 GiB | 1121.30 s (baseline) |
| 235B (TP8 + EP) | TP0 + broadcast (16 GiB source, 8 GiB target) | 16.00 GiB | 8.00 GiB | 0 GiB | 287.97 s (3.89×, −74.3% time) |
| 235B (TP8 + EP) | TP0 + broadcast (256 GiB source, 64 GiB target) | 220.19 GiB | 9.31 GiB | 0 GiB | 108.89 s (10.30×, −90.3% time) |
| 235B (TP8 + EP) | TP all (256 GiB source, 32 GiB target) | 220.19 GiB | 32.00 GiB | 0 GiB | 69.16 s (16.21×, −93.8% time) |

### ModelExpress D2D

Nebius H100 80 GB nodes, vLLM `0.30.0`, graph mode on `235B (TP8 + EP)`
(`Qwen/Qwen3-235B-A22B-Instruct-2507-FP8`):

| Model | Mode | Source RAM | Target RAM | Target Storage | Weight Transfer Time |
|---|---|---|---|---|---|
| 235B (TP8 + EP) | TCP peer sync + local load (sum) | 0 GiB | 0 GiB | 220.21 GiB | 1121.30 s (baseline) |
| 235B (TP8 + EP) | Seed replica: `GpustackLoadStrategy` (256 GiB source, 8 GiB target) | 220.19 GiB | 8.00 GiB | 0 GiB | 232.10 s (4.83×, −79.3% time) |
| 235B (TP8 + EP) | Peer replica: `RdmaStrategy` (NIXL D2D + compile cache) | 0 GiB | 0 GiB | 0 GiB | 16.78 s (66.82×, −98.5% time) |
| 235B (TP8 + EP) | Restart fallback: `GpustackLoadStrategy` (256 GiB source, 8 GiB target) | 220.19 GiB | 8.00 GiB | 0 GiB | 109.08 s (10.28×, −90.3% time) |

Restoring shared compile caches over NIXL cut `torch.compile` from `77.67 s` to `0.28 s` (`277.39×`)
and reduced total engine initialization from `296.82 s` to `90.25 s` (`3.29×`).

## Observing status

Read per-role and per-replica delivery state from *`status.model.remote`*:

```bash
kubectl get modeldeployment <name> -o jsonpath='{.status.model.remote}' | jq
```

- *`policy`* echoes the active policy; *`roles[].replicas[]`* reports *`delivery`* (`Remote`,
  `Node`, `Engine`), *`load`* (`Unknown`, `AwaitingMetadata`, `AwaitingSource`, `Loading`,
  `Loaded`), *`reason`*, and *`fallbackAttempts`*.
- `WeightsReady` confirms loaded weights; a loaded replica stays `Loaded` if its source later drains.
- Source eligibility follows the owning prefetch's [serving observation](prefetch.md#status-and-views).

## Troubleshooting

Start from *`status.model.remote`*, then `WeightsReady` and the replica `Pod`:

| Observation | Meaning | Check |
|---|---|---|
| `Progressing=True`, `SourceWindowFallback` on prefetch | Grant cannot back `1 GiB` minimum under `Prefer` | Increase Binding grant; check local delivery |
| `Progressing=False`, `SourceWindowBelowFloor` on prefetch | Grant cannot back `1 GiB` minimum under `Require` | Increase Binding grant; check source capacity |
| `Progressing=False`, `SourceGrantMissing` on prefetch | Referenced Binding is missing | Restore `ModelStoreBinding` named by *`spec.bindingRef`* |
| `load: AwaitingSource` | No prepared source serves the digest yet | Check prefetch *`status.transfer`* and `NodeModelStore` generations |
| `load: AwaitingMetadata` | Metadata view not mounted yet | Check `Pod` `FailedMount` events and `CSIDriver` |
| `load: Unknown`, `RemoteLoadFailed` | Load attempt ended; replica recreating | Check terminated container exit and *`fallbackAttempts`* |
| `FallbackAttemptsSpent` | Fallback attempts exceeded three | Check fallback `Node` or `Engine` delivery failure |
| `RoleIneligibleForModelTransfer` (no `Pod`) | Role has custom command, unsupported engine, or no backend | Check [Configuration](#configuration) or use `Prefer` |
| `UnqualifiedTransferBackend` (no `Pod`) | Candidate outside routing allowlist | Check engine/backend eligibility under [Configuration](#configuration) |
| `WeightsReady=False`, `NodeDeliveryUnavailable` | `Require` active without `model.csi.gpustack.ai` `CSIDriver` | Install/enable `model-manager` CSI plugin |

### Rollback

Patch `model-artifact-remote-delivery-policy` to `Disabled` (or set `modelTransfer.enabled: false`
in chart values). New replicas use `model-artifact-delivery-mode` (`Node` or `Engine`); already
loaded replicas keep serving until their next roll.

---

**See also** — [Model Prefetch](prefetch.md) (intent and binding grant) ·
[Model Store Operations](operations.md) (delivery `Setting`s in operation) ·
[Model Artifact](artifact.md) (identity a source serves) ·
[Model Deployment Configuration](../model-deployment/configuration.md) (deployment roles and conditions) ·
[Engine Compatibility](../model-deployment/engine-versions.md) (per-engine adapter notes)

**Next** → [Model Artifact](artifact.md)
