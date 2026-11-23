# Remote Weight Delivery Internals

Evidence: contract (benchmark sweep details marked as recorded capture). Detailed source-memory
arithmetic walkthroughs, manifest and journal bounds, reader session negotiation, ModelExpress
injected environment variables, and raw benchmark host budgets for remote weight delivery.

## Contents

- [Source memory arithmetic](#source-memory-arithmetic)
- [Journal and manifest limits](#journal-and-manifest-limits)
- [Reader and session protocol](#reader-and-session-protocol)
- [ModelExpress internals](#modelexpress-internals)
- [Benchmark sweep details](#benchmark-sweep-details)

## Source memory arithmetic

Source admission enforces the smallest of three limits: broker node budget
(*`modelTransfer.source.maxMemoryBytes`*, default `MemTotal / 16`, floored at `minMemoryBytes`
`1 GiB`), per-generation resident window (`GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES`
and HIXL `GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES` defaulting to `16 GiB`, rounded down
to `2 MiB` with a `1 GiB` floor), and optional `ModelStoreBinding` *`spec.sourceMemory`*.

Every admission adds `10%` reservation overhead on top of native backing and checks live available
node RAM from `/proc/meminfo`. Each generation and session keeps its own charge even for the same
digest, and copies on additional nodes share the Binding grant.

### 128 GiB broker example

Illustrative setup using `Qwen/Qwen3-8B` (`15.27 GiB` checkpoint) and `Qwen/Qwen3.6-35B-A3B`
(`66.99 GiB` checkpoint) sharing `model-source-pool` and `qwen-source` (`sourceMemory.bytes: 64Gi`)
on a node with a `128 GiB` broker ceiling and `16 GiB` HIXL generation ceiling:

```yaml
modelTransfer:
  source:
    maxMemoryBytes: 137438953472  # 128 GiB shared by all this broker's generations
    minMemoryBytes: 1073741824
    maxSessions: 64
  env:
    GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES: "0"
    GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES: "17179869184"  # 16 GiB per generation
```

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelArtifact
metadata:
  name: qwen3-8b
  namespace: gpustack-default
spec:
  source:
    modelScope:
      repository: Qwen/Qwen3-8B
---
apiVersion: worker.gpustack.ai/v1
kind: ModelArtifact
metadata:
  name: qwen3-6-35b-a3b
  namespace: gpustack-default
spec:
  source:
    modelScope:
      repository: Qwen/Qwen3.6-35B-A3B
---
apiVersion: worker.gpustack.ai/v1
kind: ModelStore
metadata:
  name: model-source-pool
spec:
  nodeSelector:
    matchLabels:
      gpustack.ai/model-transfer-source: "true"
---
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
  sourceMemory:
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
  source: {}
---
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: qwen3-6-35b-a3b-source
  namespace: gpustack-default
spec:
  artifactRef:
    name: qwen3-6-35b-a3b
  bindingRef:
    name: qwen-source
  placement:
    nodeSelector:
      matchLabels:
        gpustack.ai/model-transfer-source: "true"
  retention:
    pinned: true
  source: {}
```

- **8B vs 35B under 16 GiB**: `Qwen3-8B` (`15.27 GiB`) fits in full if admission grants enough
  space for native backing; `Qwen3.6-35B-A3B` (`66.99 GiB`) pages successive resident batches
  bounded by the largest safetensors shard (shards are never split).
- **Binding grant ceiling**: The `64 GiB` grant cannot enlarge a `16 GiB` per-generation window.
  With `10%` overhead, `16 GiB` of native backing charges about `17.6 GiB`, so three such sources
  fit inside `64 GiB` and a fourth is refused even below the node's `128 GiB` ceiling.

### 16 GiB broker example

Lowering `modelTransfer.source.maxMemoryBytes` to `17179869184` (`16 GiB`) leaves at most about
`14.54 GiB` for native backing after `10%` overhead and `2 MiB` alignment when no other charges
exist.

The Binding's `64 GiB` grant cannot override this node ceiling: even `Qwen3-8B` must page, both
sources cannot claim the full ceiling concurrently, and every admitted window must still fit its
largest shard.

## Journal and manifest limits

- **Contract version**: Operator, source SDK and engine SDK must deploy together on layout-only
  contract version `2`; version `1` readers and manifests are refused.
- **Manifest bounds**: Inline manifests cap at `64 MiB` raw JSON. Paged manifests cap roots and
  pages at `4 MiB` each, `512 MiB` total pages, and `1,048,576` tensors overall.
- **Journal bounds**: Load and source journals cap operations at `1,048,576` (byte limits can
  refuse work sooner). Proven acknowledgements compact; unknown receipts stay charged.
- **Page swap and idle release**: A page swap drains and acknowledges active work, revokes old
  registrations, reuses shared files and frees evicted blocks; a queued page miss stops newer
  resident reads from extending drain. Idle sources release resident blocks once no native
  operations or unknown sessions remain.
- **Quarantine and quota authority**: Failed restore or unknown release quarantines the generation
  and retains its reservation; lease expiry, `Pod` deletion and process exit alone do not prove
  release. Missing or corrupt quota authority blocks new admissions while retaining existing charges.
- **Broker `ServiceAccount`**: Reviews audience-scoped tokens (`gpustack-model-transfer`), reads
  live workload/artifact/placement/grant state, pins each Binding's quota authority ConfigMap in the
  operator namespace, and acknowledges drain on source `Pod`s. `model-manager` remains the sole
  writer of `NodeModelStore` status.
- **Offline GitOps TLS**: Without cert-manager, Helm preserves the serving certificate via Secret
  lookup; offline renderers (`helm template | kubectl apply`) rotate the trust anchor on every
  render unless cert-manager is configured.

## Reader and session protocol

- **`all` mode window division**: Each rank computes
  `max(GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES // tp_size, 1 GiB)` aligned down to `2 MiB`. For
  large *TP* groups, the `1 GiB` per-rank floor lets the sum exceed the group budget.
- **`model_loader_extra_config`**: Engine JSON option structure (override per-reader window without
  *TP* division):

```yaml
window_bytes: 8589934592       # explicit per-reader window; overrides env-derived budget
max_inflight: 4                # omit to use available CPU threads, capped at 256
owned_output_bytes: 4294967296 # 4 GiB default ceiling for one owned output tensor
```

- **Session negotiation (`POST /v1/sessions`)**: Returns `sourceWindowBytes` when source RAM clears
  the `256 MiB` adaptive floor; client clamps to `min(clientBudget, sourceWindowBytes)` bounded by
  needed tensor span. Exhausted session capacity returns HTTP `409`, `Retry-After: 5`, and
  `{"retryAfter": 5}`; clients try alternate peers for the digest, then wait for the shortest
  delay until startup deadline.
- **Alignment rules**: `GPUSTACK_MODEL_TRANSFER_TARGET_MEMORY` is no longer read. HIXL owned host
  regions always use `2 MiB`-aligned addresses and lengths (reserving rounded extent plus `2 MiB`,
  zeroing tail, charging full backing); direct loader windows floor at `4 MiB` (`8 MiB` for double
  buffering) and `HixlReader` preserves caller-owned bounds. NVIDIA pinned host allocations `>= 2 MiB`
  retain alignment, rounding NIXL windows down and needed tensor spans up before clamping.
- **Isolation qualification**: Projected tokens bind sessions to `Pod` identity and target process
  instance. Source-initiated writes into target staging windows withhold source registration
  metadata on the control path, but raw READ/WRITE/ATOMIC and generation isolation still require
  hardware backend safety qualification.

## ModelExpress internals

### Injected role environment

When a managed `ModelDeployment` role sets `--load-format=modelexpress` (or `mx`) on a Hugging Face
artifact, the operator creates `<md-name>-modelexpress` (shortened for long names) and injects the
following defaults (user *`roles[].env`* overrides any entry):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: qwen-chat-server
  namespace: team-a
spec:
  containers:
    - name: server
      image: example/engine:tag
      env:
        - name: MX_SERVER_ADDRESS
          value: qwen-chat-modelexpress.team-a.svc:8001  # metadata Service
        - name: MX_WORKER_HOST
          valueFrom:
            fieldRef:
              fieldPath: status.podIP  # replica Pod IP
        - name: MX_ARTIFACT_TRANSFER
          value: "1"  # set "0" to disable compile-cache transfer
        - name: MX_ARTIFACT_COMPILE_CONFIG_DIGEST
          value: <role-digest>  # omitted when compile-cache identity is unavailable
        - name: MX_ARTIFACT_READY_URL
          value: http://127.0.0.1:8000/health  # omitted when engine grading or port is unknown
        - name: MX_LOAD_PLUGIN
          value: gpustack  # GPUStack loading strategy
        - name: MX_LOAD_PLUGIN_BEFORE
          value: server-cache  # strategy insertion point
```

### Strategy precedence

| Value | Meaning | Constraints |
|---|---|---|
| `RdmaStrategy` | Loads tensors from a ready peer over NIXL D2D | Tried first per *TP* rank; peer *TP*/*PP* layouts must match |
| `GpustackLoadStrategy` | Loads from a ready `ModelPrefetch` source over NIXL H2H + H2D | Tried after peer delivery (seed replica or post-restart fallback) |
| Other cache / `DefaultStrategy` | Loads from eligible local cache or engine volume | Available under `Prefer` only |
| `Require` | Permits peer and prefetch source strategies only | Failure of both is terminal; no local checkpoint fallback |
| `Prefer` | Permits peer, prefetch source and eligible local delivery | Local strategies follow remote strategies |

## Benchmark sweep details

Evidence: recorded capture. Host RAM budgets, shard ceilings and detailed ModelExpress timing
breakdowns backing the matched benchmark tables in [Remote Weight Delivery](https://docs.gpustack.ai/gpustack-operator/v0.12.1/docs/modules/model-delivery/remote/index.md#results).

### Ascend HIXL host budgets

- **Hardware and image**: Two Ascend 910B2 nodes, vLLM-Ascend `cann9.2-910b-vllm0.30.0`, eager
  mode, pre-warmed source weights (`8B` = `Qwen/Qwen3-8B`, `35B` = `Qwen/Qwen3.6-35B-A3B`, `235B`
  = `vllm-ascend/Qwen3-235B-A22B-W8A8`).
- **Source host RAM**: `2014 GiB` total, `~1514 GiB` available; default `maxMemoryBytes: 0` budget
  `2014 / 16 = 125.88 GiB` before 10% overhead.
- **Shard ceilings**: `3.72 GiB` on `8B` and `35B`, `4.00 GiB` on `235B`. Each reading rank caps
  its host window at `min(perRankBudget, shardBytes)`. Note: `235B` paged and full-resident runs
  used different SDK builds.

### NVIDIA NIXL and ModelExpress

- **Hardware and image**: Nebius H100 80 GB nodes, vLLM `0.30.0` (eager mode for NIXL source runs;
  graph mode for ModelExpress D2D runs; `80B` = `Qwen/Qwen3-Next-80B-A3B-Instruct-FP8` *TP* = 4,
  `235B` = `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` *TP* = 8 + *EP*).
- **Source host RAM and shards**: `1574.84 GiB` total, `~1554.06 GiB` available (default budget
  `98.43 GiB`). Safetensors shard ceilings: `10.00 GiB` on `80B`, `9.31 GiB` on `235B`.
- **ModelExpress compile-cache breakdown (`235B`, graph mode)**: Seed replica ran `232.10 s` weight
  load, `77.67 s` cold `torch.compile`, `112 s` CUDA Graph capture (`296.82 s` total engine init)
  and published `386.07 MiB` of compiled caches (`torch.compile`, Triton, DeepGEMM, FlashInfer).
  Peer replica ran `16.78 s` NIXL D2D weight load (`13.83×` vs seed), `0.28 s` `torch.compile`
  (`277.39×` vs seed), and `90.25 s` total engine init (`3.29×` vs seed). Post-restart fallback
  loaded from resident `220.19 GiB` source RAM in `109.08 s` (`10.28×` vs baseline).

---

**See also** — [Remote Weight Delivery](https://docs.gpustack.ai/gpustack-operator/v0.12.1/docs/modules/model-delivery/remote/index.md) (operator setup, matched benchmark tables, troubleshooting and rollback) · [Ascend A5 Matrix](ascend-a5-matrix.md#remote-delivery-matrix) (Ascend A5 remote transfer scenarios) · [Node Model Store](model-delivery-node-store.md) (node cache verification and CSI mounts)
