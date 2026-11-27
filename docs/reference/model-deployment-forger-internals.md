# Model Deployment Forger Internals

Evidence: contract. Exact contracts for `LocalKernelKey`, FlashInfer Trace schemas,
`gpustack-engine-sentinel` `/gpustack/v1/probe` and kernel loading, `/ingest/v1/trees` security,
store-local `Kind: Kernel` isolation, and the 3-voice `NodeModelStore.spec.pinned` union.

## Contents

- [LocalKernelKey contract](#localkernelkey-contract)
- [FlashInfer Trace and manifests](#flashinfer-trace-and-manifests)
- [Sentinel probe and loader](#sentinel-probe-and-loader)
- [Local emptyDir ingestion](#local-emptydir-ingestion)
- [Content-class CSI isolation](#content-class-csi-isolation)
- [Single-writer pin union](#single-writer-pin-union)
- [Bindings and metrics](#bindings-and-metrics)

## LocalKernelKey contract

`pkg/worker/kernelkey` identifies single-GPU operator specializations in canonical 6-segment form:

```text
<engine>/<engineVersion>/<manufacturer>/<arch>/<opFamily>/<signatureHash16>
```

### Canonical signature hash

- **`signatureHash16`**: First 16 lowercase hex characters (64 bits) of SHA-256 over compact,
  sorted-key JSON of `{constAxes, constants, inputDtypes, outputDtypes}` with HTML escaping
  disabled.
- **`constAxes` vs dynamic `var` axes**: `FilterConstAxes` hashes only static per-rank
  compile-time dimensions (after *TP*/*EP* sharding) and strips dynamic runtime axes (`batch`,
  `batch_size`, `bs`, `context_len`, `kv_len`, `max_num_tokens`, `max_seq_len`,
  `num_decode_tokens`, `num_prefill_tokens`, `num_requests`, `num_seqs`, `num_tokens`, `q_len`,
  `seq_len`, `seq_length`, `sequence_length`, `token_count`, `total_tokens`).
- **Segment normalization**: `engine` is lowercase (`vllm`, `sglang`); `engineVersion` strips build
  metadata suffixes (`+cu128`, `+rocm6.3`, git suffixes) while retaining `major.minor.patch` (or
  pre-release tags); `manufacturer` and `arch` match `pkg/nodefeature` labels (e.g.,
  `nvidia/sm_90a`, `amd/gfx942`, `ascend/910b`).

### Allowed and excluded operators

| Category | Members |
|---|---|
| Allowed `OpFamily` | `rmsnorm`, `layernorm`, `rope_kv_append`, `activation_quant`, `moe_router_topk`, `moe_permute_unpermute`, `paged_attention`, `ragged_prefill_attention`, `mla_local`, `speculative_verify`, `sampling_logits`, `custom_elementwise_fusion` |
| Excluded collectives | `nccl*`, `rccl*`, `hccl*`, `nvshmem*`, `rocshmem*`, `msccl*`, `all_reduce`, `all_gather`, `reduce_scatter`, `all_to_all`, `fused_all_reduce*` |
| Excluded block-scaled GEMM | `deep_gemm*`, `fp8_blockwise_gemm`, `mxfp8_gemm`, `nvfp4_gemm`, `fp4_gemm`, `cutlass_block_scaled_gemm`, `block_scaled_gemm` |
| Excluded dense GEMM | `cublas*`, `hipblas*`, `rocblas*`, `aten::mm`, `aten::addmm`, `aten::bmm`, `aten::matmul`, `gemm`, `matmul`, `bmm`, `addmm`, standalone `mm` |

Collectives and block-scaled GEMMs are checked before dense GEMMs and allowed names so hybrid
symbols such as `fused_all_reduce_rmsnorm` never bypass exclusion.

## FlashInfer Trace and manifests

`ForgeSandbox` mounts input files read-only at `/var/run/gpustack/forge/input/` (from an
owner-referenced `ConfigMap` capped at `512 KiB`) and stages outputs in `forge-output` at
`/var/run/gpustack/forge/output/`.

### Input and Trace schemas

| File | Schema / format | Contents |
|---|---|---|
| `forge-request.json` | `gpustack-forge-request/v1` | `engine`, `engineVersion`, `manufacturer`, `architecture`, `targetKernelKeys`, `baselineLatenciesUs`, `perKernelTimeoutSeconds`, `minSpeedupRatio` (default `1.05`, min `1.01`), `atol` (`1e-2`), `rtol` (`1e-2`). |
| `definitions/<op>.json` | `flashinfer-trace/v1` `Definition` | `name`, `op_type`, `local_kernel_key`, `axes` (`const`/`var`), `inputs`, `outputs`, `reference` PyTorch specification. |
| `workloads/<op>.jsonl` | `flashinfer-trace/v1` `Workload` | `uuid`, `definition`, `local_kernel_key`, `axes` (concrete `var` values), `inputs` (synthetic descriptors; never raw prompt tokens), `observed_duration_us`, `call_count`. |

### Output manifest and report

`initContainer: forge-runner` writes `solutions/<id>.json` (`Solution`), `kernels/<id>/*`,
`traces/<id>.json` (`Trace`), and `dispatch-manifest.json` (`gpustack-forge-dispatch/v1`):

```json
{
  "schemaVersion": "gpustack-forge-dispatch/v1",
  "compatibility": {
    "engine": "vLLM",
    "engineVersion": "0.19.0",
    "manufacturer": "nvidia",
    "architecture": "sm_90a"
  },
  "entries": [
    {
      "localKernelKey": "vllm/0.19.0/nvidia/sm_90a/rmsnorm/0123456789abcdef",
      "definition": "rmsnorm_d4096",
      "solutionPath": "solutions/rmsnorm_v1.json",
      "solutionSha256": "sha256:...",
      "libraryPath": "kernels/rmsnorm_v1/kernel.py",
      "librarySha256": "sha256:...",
      "entrypoint": "run",
      "tracePath": "traces/rmsnorm_v1.json",
      "traceSha256": "sha256:...",
      "baselineLatencyUs": 100.0,
      "forgedLatencyUs": 80.0,
      "speedupRatio": 1.25
    }
  ]
}
```

After `forge-runner` (and `forge-stage-sentinel` in `External` mode) exits `0`,
`initContainer: forge-verifier` mounts `forge-output` read-only and writes
`forge-verified/verification-report.json` (`gpustack-forge-verification/v1`, `16 MiB` `emptyDir`).

The verifier asserts `overallStatus: "PASSED"` (requiring `failedCount == 0` and `passedCount > 0`,
so all entries must pass), `evaluation.status == "PASSED"`, element-wise `allclose` within `atol`
and `rtol` (`allclose_passed: true`), and `speedup_factor >= minSpeedupRatio`.

### Sandbox pod contract

`pkg/worker/forger.BuildSandboxPod` and `ModelDeploymentForgerWebhook` enforce the PodSpec-style
`spec.sandbox` contract:

| Surface | Fields and bounds | Rendering and isolation |
|---|---|---|
| Pod envelope | `priorityClassName`, `activeDeadlineSeconds` (`60..14400`, default `1800`), `outputSizeLimit` (`64Mi..16Gi`, default `2Gi`), `imagePullSecrets` (`<= 32` valid DNS-1123 subdomain names, no duplicates) | `imagePullSecrets` merges target `role.imagePullSecrets` followed by `sandbox.imagePullSecrets` (deduplicated in order) so `forge-verifier` still pulls private engine images. |
| `runnerMode` & `runner` | `External` (default; requires `runner != nil` and `runner.image` `<= 512` chars matching `forger-allowed-runner-image-prefixes`) or `Mock` (rejects non-nil `runner`); `imagePullPolicy` (`Always`, `Never`, `IfNotPresent`); `command` & `args` (`<= 64` entries, `<= 4096` bytes each, no NUL `\x00`) | `runner.{imagePullPolicy,command,args,env,envFrom}` renders strictly on `initContainer: forge-runner` and is never passed to `forge-stage-sentinel`, `forge-verifier`, or `forge-hold`. |
| `runner.env` & `runner.envFrom` | `env` (`<= 64` unique valid env names `<= 256` chars; mutually exclusive `value` vs `valueFrom.{configMapKeyRef,secretKeyRef}`, no downward API); `envFrom` (`<= 16` entries, exactly one of `configMapRef` or `secretRef`, optional valid `prefix`) | Case-insensitive `GPUSTACK_FORGE_*` names and `envFrom[].prefix` values are rejected at admission and stripped defensively in `BuildSandboxPod`; operator `GPUSTACK_FORGE_INPUT_DIR` and `GPUSTACK_FORGE_OUTPUT_DIR` are always injected in `Env` so Kubernetes prioritizes them over `EnvFrom`. |
| Host & accelerator `resources` | Positive `cpu`, `ram`, `localStorage` (`ephemeral-storage`); `acceleratorSlicedMemoryPercentage` (`0..100`), `acceleratorSlicedCoresPercentage` (`0..100`, requires `memory > 0`), `acceleratorPartitionedProfile` (`<= 64` chars, mutually exclusive with slicing) | Applied to sequential `initContainer`s `forge-runner` and `forge-verifier` (effective Pod request of `1` unit): `Exclusive` whole-card default (`<vendor>/<dev>: 1`, recommended for deterministic verification timing), `Partitioned` (`.partitioned: 1` + `.partitioned.<kind>-<profile>: 1` on a partition-offering pool), or `Sliced` (`.sliced: 1` + `.sliced.memory-percentage` + `.sliced.cores-percentage` on a sliceable pool). |

## Sentinel probe and loader

### Probe HTTP wire contract

`gpustack-engine-sentinel` serves `/gpustack/v1/probe` (`5s` worker client timeout per call):

| Method | Purpose | Guardrails and codes |
|---|---|---|
| `POST /gpustack/v1/probe` | Arm a probe (`expected_boot_id`; `steps` must be in `[3, 5]` (default `4`); out-of-range values return `400 invalid_argument`) | `400 invalid_argument`, `409 boot_id_mismatch`, `409 probe_in_progress`, `409 role_paused`, `429 probe_cooldown` (`GPUSTACK_PROBE_MIN_COOLDOWN_SECONDS`, default `60`), `422 unsupported_engine_version` |
| `GET /gpustack/v1/probe` | Read state and distilled `Definition` + `Workload` JSON (`<= 512 KiB`) | Strips dense/block-scaled GEMM and collectives; zero prompt/tensor values |
| `DELETE /gpustack/v1/probe` | Abort active probe and purge staging directory | Restores rank-0 probe hooks and immediately unlinks `/dev/shm/gpustack-probe/<probe_id>/` |

### Staging and engine hooks

- **`/dev/shm` budget**: Temporary traces stage under `/dev/shm/gpustack-probe/<probe_id>/` with a
  wall-clock cap of `15s` and byte cap `min(64 MiB, 5% of shmSize)`. All ranks' raw trace files
  unlink in a `finally` block; only the API-server latch sweeps orphaned directories older than
  `60s` on startup.
- **Engine dispatch**: vLLM dispatches step-counted `torch.profiler.profile` to the rank-0 GPU
  worker via `collective_rpc(..., unique_reply_rank=0)` (returning `422 unsupported_engine_version`
  if the RPC hook is absent). SGLang invokes `TokenizerManager.start_profile` in-process with
  `record_shapes=True` and cleans up every TP rank's `*-TP-<rank>*.trace.json.gz`.
- **Startup kernel loader**: When `GPUSTACK_KERNEL_ARTIFACT_DIR=/var/lib/gpustack/kernels` is set,
  sentinel lazily loads `dispatch-manifest.json` before CUDA Graph capture without mutating
  `VLLM_PLUGINS` or `argv`. Each rank reports `loaded_kernel_keys`, `dispatch_manifest_digest`,
  and optional `kernel_load_error` (`unsupported_kernel_artifact`) on `GET /gpustack/v1/runtime_config`
  and `GET /gpustack/v1/ready`. `ModelDeployment` folds `KernelManifestDigest` into
  `ModelDeploymentCompileCacheInput` to isolate Inductor/Triton compile caches per generation.

## Local emptyDir ingestion

`model-manager` serves `POST /ingest/v1/trees` on its HTTPS secure port (`32444`):

1. **Projected token authentication**: The worker sends a projected `ServiceAccount` token with
   audience `gpustack-model-ingest` (`3600s` TTL, never its API-server token). `model-manager`
   validates it via `TokenReview` against `gpustack-model-ingest` and
   `system:serviceaccount:<ns>:<--ingest-service-account>`.
2. **Live ownership and init barrier**: Using `APIReader`, `model-manager` verifies that
   `ModelDeploymentForger.status.roles[*].activeSandbox.podUID == req.PodUID`, that the `Pod` runs
   on `d.NodeID` with controller `ownerReference` matching the forger UID, and that both
   `forge-runner` and `forge-verifier` `initContainerStatuses` are `Terminated` with `ExitCode == 0`.
   `ForgeSandbox` `Pod`s run with explicit non-zero `runAsUser`/`runAsGroup`/`fsGroup` (`65532`)
   and random nonces to prevent pre-creation adoption.
3. **Tree validation**: `model-manager` reads
   `<kubeletDir>/pods/<podUID>/volumes/kubernetes.io~empty-dir/` (`forge-verified/verification-report.json`
   and `forge-output`), walks files via `os.OpenRoot` with `O_NOFOLLOW|O_NONBLOCK` and `Nlink == 1`,
   and enforces caps (`<= 1024` files, `<= 2 GiB` default (up to `spec.sandbox.outputSizeLimit`,
   max `16 GiB`), depth `<= 16`, path `<= 256`).
4. **Single-pass copy and publish**: `model-manager` reserves capacity with
   `gc.Collector.Reserve`, hashes every byte in a single copy pass into `<root>/partial/`, builds
   `gpustack-manifest v1`, writes `manifest.json` (`0600`, `Kind: "Kernel"`), publishes
   `store.Marker{Source: "Forge", Kind: "Kernel"}`, and flushes `NodeModelStore.status`.

## Content-class CSI isolation

| Surface | Mechanism | Enforcement |
|---|---|---|
| Store marker (`store.Marker`) | `Kind: "Kernel"` vs `""` / `"Model"` | Preserved in `<root>/published/<hex>` and `peer.Listing.Kind` across `/peer/v1/trees/{hex}` |
| CSI `volumeAttributes` | `artifactKind: "ModelKernelArtifact"` | Selects `ModelKernelArtifact` lookup instead of `ModelArtifact` |
| `authorizeKernelMount` | Informer lookup in `csi.storage.k8s.io/pod.namespace` | Requires matching `UID`, `Resolved=True`, `status.quarantined == false`, and `manifestDigest` |
| `authorizeMarkerKind` | Reads `Store.ReadMarker(hex)` before bind mount | Refuses `Kernel` mounts of `Model` trees and `Model` mounts (including `expectedDigest`) of `Kernel` trees with `PermissionDenied` |

## Single-writer pin union

`ModelPrefetchReconciler` (`pkg/worker/controllers/worker/node_model_store_pins.go`) is the sole
writer of `NodeModelStore.spec.pinned`, watching `ModelPrefetch`, `ModelKernelArtifact`, and
`ModelDeployment`. For each node, it computes the sorted, deduplicated union across three voices
under the `256`-entry schema cap:

1. **Priority 1 — `ModelTransfer`**: Admitted `ModelTransfer` source-generation digests reported on
   the node.
2. **Priority 2 — `ModelKernelArtifact`**: Non-deleting `nodeStore` kernel digests on `seedNode`
   (while unquarantined and either awaiting initial resolution with `status.resolved == nil` or
   reported `Ready`; released when `status.quarantined: true`), nodes hosting live `Pod`s
   referencing `spec.roles[*].kernelArtifactRef`, and nodes protecting `previousKernelArtifact`
   during an in-flight `ModelDeploymentForger` rollout.
3. **Priority 3 — `ModelPrefetch`**: Live `spec.retention.pinned` entries under `allowPinned: true`.

Each node is capped independently at `256` entries (`1 > 2 > 3`, lexicographic tie-break); cap
saturation sets `Pinned=False` (`Reason=PinnedCapacityExceeded`) on excess `ModelKernelArtifact`s
without aborting reconciliation for other nodes.

## Bindings and metrics

- **Annotations on `ModelDeployment`**: `modeldeployment.gpustack.ai/forger-binding`
  (`{forgerName, forgerUID, term}`) and `modeldeployment.gpustack.ai/forger-decision`
  (`{decisionID, term, kernelArtifacts}`) are fenced at admission to the operator worker identity
  and co-exist orthogonally with `scaler-binding`.
- **Worker Prometheus metrics**: `gpustack_forger_probes_total{engine,result}`,
  `gpustack_forger_probe_duration_seconds{engine}`, `gpustack_forger_sandbox_runs_total{mode,result}`,
  `gpustack_forger_promotions_total{outcome}`, and `gpustack_forger_rollbacks_total{reason}`.

---

**See also** — [Model Deployment Forger](../modules/model-deployment/forger.md) (operator guide and troubleshooting) · [Node Model Store](model-delivery-node-store.md) (`NodeModelStore` contract, CSI mounts, and collection) · [Model Deployment Status](model-deployment-status.md) (`KernelsReady` condition and `forger-binding` annotations) · [Model Deployment Internals](model-deployment-internals.md) (compile-cache keys and admission refusals)
