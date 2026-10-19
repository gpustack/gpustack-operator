# Spec: Host-Backed Compilation Cache and Role Startup Timeout for ModelDeployment

Status: Shipped
Type: Feature

## Summary

`ModelDeployment` workloads running `vLLM` (including `vllm-ascend`) or `SGLang` in graph mode (`torch.compile`, Inductor, Triton, FlashInfer, and `npugraph_ex`) recompile computation graphs and kernels from scratch on every Pod restart because the operator mounts no persistent compilation cache directory into inference containers. In addition, `ModelDeployment` hardcodes a 30-minute `startupProbe` budget (`failureThreshold: 180, periodSeconds: 10, timeoutSeconds: 3`) with no per-role timeout field, and `modelDeploymentMemberReadiness` misclassifies running containers whose `startupProbe` has not yet succeeded (`Started == false`) as `modelDeploymentLegFailed` (`EndpointsRevoked`) once a role has previously reached `Available`. This spec introduces an operator-managed host-backed compilation cache under `/var/lib/gpustack/cache` (aligned with `gpustack/gpustack`'s default `cache_dir`), a standalone cache-path builder function with explicit upstream-evolution guards, a configurable per-role `startupTimeoutSeconds` field on `ModelDeploymentRole`, a startup-aware health-qualification fix, and graph-mode E2E test coverage.

## Motivation

### Goals

1. **Persist engine compilation artifacts across Pod restarts on the same node.** Eliminate redundant `torch.compile` (Inductor FX graph cache, AOT compiled artifacts, Triton kernel cache, FlashInfer JIT workspace, and `vllm-ascend` `npugraph_ex` Python graph code) recompilation during cold starts and Pod rescheduling by mounting a deterministic, collision-free host subdirectory into `/var/lib/gpustack/cache` inside each inference container.
2. **Align the default host cache root with `gpustack/gpustack`.** Use `/var/lib/gpustack/cache` as the default host compilation cache root (matching `gpustack/gpustack` at `gpustack/config/config.py:410-412`, where `self.cache_dir` defaults to `os.path.join(self.data_dir, "cache")` under `/var/lib/gpustack`) to reduce configuration friction when users migrate or co-locate with `gpustack`. Legacy compiled graphs already on disk are not migrated.
3. **Prevent cross-model and cross-topology cache collisions across both `vLLM` and `SGLang`.** Isolate host cache directories by hardware manufacturer (derived from `InstanceType`, with `"cpu"` fallback for non-acceleratable instance types), engine name, model identity (`ManifestDigest`, OCI image digest, PVC UID/claim identity, or `spec.model.name` hash), declared parallelism (`TP/PP/DP/EP/CP`), and an 8-character digest of compilation-affecting role arguments, environment variables, and container image.
4. **Encapsulate cache path generation in a single standalone function.** Define the cache directory calculation in a dedicated helper function (`BuildModelDeploymentCompileCacheSubPath`) with explicit code comments warning maintainers that future upstream changes to `vLLM` (including `vllm-ascend`) or `SGLang` compilation cache paths or environment variables require updating the mapping in that function.
5. **Expose a per-role configurable startup timeout (`startupTimeoutSeconds`).** Allow users to configure `spec.roles.<role>.startupTimeoutSeconds` on `ModelDeploymentRole` (default `1800` seconds) to scale `startupProbe.failureThreshold` and raise main-container HTTP probe `timeoutSeconds` from `3` seconds to `5` seconds so large models compiling graphs or loading weights under GIL/CPU contention are not prematurely killed by kubelet.
6. **Fix false `EndpointsRevoked` / `Failed` qualification during container startup.** Update `modelDeploymentMemberReadiness` in [model_deployment_health.go](../pkg/worker/controllers/worker/model_deployment_health.go) so a running container with `ContainerStatus.Started != nil && !*ContainerStatus.Started` returns `modelDeploymentLegUnknown` (Hold) rather than `modelDeploymentLegFailed`.
7. **Verify real graph-mode startup and warm-cache reuse in E2E tests.** Add E2E test cases that exercise graph-mode `ModelDeployment` workloads (without `--enforce-eager` / `--disable-cuda-graph`), verifying cold compilation cache population on `/var/lib/gpustack/cache`, warm restart cache reuse, and `startupTimeoutSeconds` probe rendering.

### Non-Goals

1. **Serializing in-memory CUDA Graph or CANN ACLGraph capture objects to disk.** In both `vLLM` (`vllm/compilation/cuda_graph.py:146-167,278-361` at commit `df6c7ad031`) and `SGLang` (`python/sglang/srt/compilation/cuda_piecewise_backend.py:175-230` at commit `40f5d9411f`), `torch.cuda.CUDAGraph` (and `torch.npu.NPUGraph` in `vllm-ascend` `vllm_ascend/compilation/acl_graph.py:55-88` at commit `f8a759a0`) binds to process-local virtual memory addresses and live tensor pools. Host disk caching persists `torch.compile` / Inductor / Triton / FlashInfer / `npugraph_ex` compilation artifacts, while in-memory graph capture replays per process.
2. **Migrating pre-existing cache directories created by standalone `gpustack/gpustack`.** Sharing `/var/lib/gpustack/cache` as the host root unifies host disk provisioning only; existing unmanaged subdirectories under `/var/lib/gpustack/cache` are ignored.
3. **Cross-node distributed compilation cache sharing via shared network filesystems (NFS/S3) or automatic disk garbage-collection daemons.** Cache directories are node-local `HostPath` (`DirectoryOrCreate`) mounts under `/var/lib/gpustack/cache` by default (though users may explicitly mount a custom PVC/volume at `/var/lib/gpustack/cache` via `additionalVolumes`, in which case the operator yields its default `HostPath` volume).
4. **Exposing raw Kubernetes `corev1.Probe` structs on `ModelDeploymentRole`.** Probe paths, ports, and headers remain operator-owned; only `startupTimeoutSeconds` is exposed on the role specification.

## Proposal

### User Stories

#### Story 1: Fast Warm Restarts in Graph Mode

As a platform engineer running production `vLLM` (`nvidia` and `ascend`) and `SGLang` models in graph mode, I want `ModelDeployment` to persist compiled graph and kernel artifacts onto a collision-free subdirectory under `/var/lib/gpustack/cache` on the host node, so that when a Pod restarts or reschedules onto the same node it reuses compiled artifacts instead of paying the multi-minute compilation penalty again.

#### Story 2: Configurable Startup Timeout for Large Graph-Mode Workloads

As a cluster user deploying large models (such as 70B+ dense or MoE models with TP/PP/EP and graph compilation enabled), I want to configure `startupTimeoutSeconds` per role on `ModelDeployment` and have the operator hold readiness during the `startupProbe` window, so that kubelet does not kill the Pod mid-compilation and the controller does not falsely mark the deployment `EndpointsRevoked` (`Failed`) while a replacement Pod is still starting.

#### Story 3: End-to-End Verification of Graph Mode and Warm Cache Reuse

As a GPUStack Operator developer, I want E2E test cases that run `ModelDeployment` in graph mode across cold start and Pod restart, so that regressions in host cache mounting, environment variable injection, cache-path isolation, or startup timeout handling are caught automatically.

### Core Features & Acceptance Criteria

#### Feature 1: Standalone Host Compilation Cache Path Builder & Volume Injection

- **Standalone Path Builder Function:**
  - Implement `BuildModelDeploymentCompileCacheSubPath` in a dedicated file ([model_deployment_compile_cache.go](../pkg/worker/controllers/worker/model_deployment_compile_cache.go)) as the single source of truth for computing the relative host subdirectory path.
  - The function doc comment must explicitly state that `vLLM` (`VLLM_CACHE_ROOT`), `vllm-ascend` (`VLLM_CACHE_ROOT` / `npugraph_ex`), and `SGLang` (`SGLANG_CACHE_DIR`) compilation cache layouts depend on upstream engine implementations, and any future upstream change to graph compilation cache paths or environment variables must be mapped in this function.
- **Host Root Alignment & User Volume Yielding:**
  - Default host cache root: `/var/lib/gpustack/cache` (`DefaultModelDeploymentCompileCacheHostDir = "/var/lib/gpustack/cache"`), matching `gpustack/gpustack`'s `cache_dir`.
  - Container mount path: `/var/lib/gpustack/cache` (`ModelDeploymentCompileCacheMountPath = "/var/lib/gpustack/cache"`), declared in [model_deployment_connector.go](../pkg/worker/controllers/worker/model_deployment_connector.go). Unlike `/var/lib/gpustack/model` and `/var/lib/gpustack/model-cache` (which are reserved when `spec.model.artifactRef != nil`), users may mount a custom volume at, inside, or around `/var/lib/gpustack/cache` via `additionalVolumes`, in which case the renderer yields its default `HostPath` volume (matching `/dev/shm` yielding behavior) and lets the user's volume back `/var/lib/gpustack/cache`.
- **Subdirectory Layout (`v1`):**
  ```text
  <host-cache-root>/<manufacturer>-<engine>/<model-identity>/tp<TP>-pp<PP>-dp<DP>-ep<EP>-cp<PCP>_<DCP>-<compile-hash[:8]>
  ```
  1. `<manufacturer>-<engine>`:
     - `<manufacturer>` is resolved from the role's `InstanceType` ([instance_type.go](../api/worker/v1alpha1/instance_type.go)): when `!instType.Spec.Acceleratable`, `<manufacturer>` is `"cpu"`; when `instType.Spec.Acceleratable` is `true`, `<manufacturer>` is `instType.Status.Detail.Manufacturer` (one of the accelerator vendors in [knowns.go](../pkg/nodefeature/knowns.go)).
     - `<engine>` is `ModelDeploymentEngineUpstreamName(md.Spec.Engine.Name)` (`"vllm"` or `"sglang"` from [model_deployment_connector.go](../pkg/worker/controllers/worker/model_deployment_connector.go)).
  2. `<model-identity>`:
     - When `md.Spec.Model.ArtifactRef != nil`:
       - `HuggingFace` or `ModelScope` source: `"m-" + HexOf(ma.Status.Resolved.ManifestDigest)[:32]` (matching `modelArtifactKVIdentity` in [model_artifact_placement.go](../pkg/worker/controllers/worker/model_artifact_placement.go)).
       - `Image` source: `"m-" + ociDigestHex[:32]` extracted from the digest-pinned `ma.Spec.Source.Image.Reference` (`@sha256:<64-hex>`).
       - `PersistentVolumeClaim` source: `"m-" + sha256(ma.Namespace + "/" + ma.Name + "/" + string(ma.UID) + "/" + ma.Spec.Source.PersistentVolumeClaim.ClaimName + "/" + ma.Spec.Source.PersistentVolumeClaim.Path)[:32]`.
     - When `md.Spec.Model.ArtifactRef == nil` (engine self-pull mode):
       - `"m-" + sha256(md.Spec.Model.Name)[:32]`.
  3. `tp<TP>-pp<PP>-dp<DP>-ep<EP>-cp<PCP>_<DCP>`:
     - Extracted by reusing `ParseModelDeploymentDeclaredParallelism(md.Spec.Engine.Name, ModelDeploymentRoleArgs(role), role.Env)` in [model_deployment_parallelism.go](../pkg/worker/controllers/worker/model_deployment_parallelism.go), defaulting unset degrees to `1` (`tp1-pp1-dp1-ep1-cp1_1`).
  4. `<compile-hash[:8]>`:
     - First 8 lowercase hex characters of the SHA-256 digest over a canonicalized compilation-factor record comprising:
       - `role.Image`
       - Normalized parallelism degrees and modes (`TP`, `PP`, `DP`, `DPL`, `PCP`, `DCP`, `EP`, `AttnCP`, `MoEDP`, `DWDP`, `NodeCount`, `Modes`)
       - Normalized compilation-affecting CLI flags across `vLLM` and `SGLang`:
         - Data type & quantization: `--dtype`, `--quantization` (`-q`), `--quantization-param-path`, `--kv-cache-dtype`, `--moe-runner-backend`
         - Context length: `--max-model-len`, `--context-length`
         - Compilation & graph capture: `--compilation-config`, `-O`, `--enforce-eager`, `--cudagraph-capture-sizes`, `--max-cudagraph-capture-size`, `--additional-config`, `--enable-torch-compile`, `--torch-compile-max-bs`, `--cuda-graph-max-bs`, `--cuda-graph-bs`, `--disable-cuda-graph`, `--disable-cuda-graph-padding`, `--piecewise-cuda-graph`, `--piecewise-cuda-graph-max-tokens`, `--piecewise-cuda-graph-tokens`, `--piecewise-cuda-graph-compiler`
         - Attention & speculative decoding: `--attention-backend`, `--attention-config`, `--prefill-attention-backend`, `--decode-attention-backend`, `--triton-attention-reduce-in-fp32`, `--all2all-backend`, `--speculative-config`, `--speculative-algorithm`, `--speculative-num-steps`, `--speculative-eagle-topk`, `--speculative-num-draft-tokens`
         - Model structure overrides: `--trust-remote-code`, `--hf-overrides`, `--json-model-override-args`, `--rope-scaling`, `--rope-theta`, `--config`
       - Compilation-affecting environment variables from `role.Env`: `VLLM_USE_V1`, `VLLM_ATTENTION_BACKEND`, `VLLM_USE_STANDALONE_COMPILE`, `VLLM_USE_MEGA_AOT_ARTIFACT`, `VLLM_FLASH_ATTN_VERSION`, `SGLANG_ENABLE_TORCH_COMPILE`, `TRITON_ATTN_NUM_STAGES`, `TORCH_CUDA_ARCH_LIST`
- **Environment Variable & Volume Injection (`!takeOver`):**
  - Managed roles (`len(role.Command) == 0`) mount the `HostPath` (`DirectoryOrCreate`) volume `gpustack-compile-cache` at `/var/lib/gpustack/cache` on the `main` container (`modelDeploymentMainContainerName = "main"`) unless the role's `additionalVolumes` already mount a path overlapping `/var/lib/gpustack/cache`. Because every replica member (`Member: 0 .. ReplicaSize-1`) and every Elastic-EP Ray member is rendered as an independent `Pod` from `renderModelDeploymentPodTemplate`, this single template mount covers every rank's engine container. Command take-over roles (`len(role.Command) > 0`) receive no operator volumes or operator env vars, consistent with the existing take-over contract (`model_deployment_render.go:428,526`). The managed router (`model_deployment_router.go`) does not compile model graphs and receives neither the volume nor the env var.
  - For `vLLM` (`nvidia`, `ascend`, `amd`, `cpu`, and other manufacturers): inject `VLLM_CACHE_ROOT=/var/lib/gpustack/cache` into the defaulted env tier (`mergeModelDeploymentEnv`) if `VLLM_CACHE_ROOT` is not already present in `role.Env`, and register `VLLM_CACHE_ROOT` in `modelDeploymentDefaultedEnvNames`.
  - For `SGLang`: inject `SGLANG_CACHE_DIR=/var/lib/gpustack/cache` into the defaulted env tier (`mergeModelDeploymentEnv`) if `SGLANG_CACHE_DIR` is not already present in `role.Env`, and register `SGLANG_CACHE_DIR` in `modelDeploymentDefaultedEnvNames`.
- **Acceptance Criteria:**
  - Every rendered managed (`len(role.Command) == 0`) `ModelDeployment` inference Pod mounts a `HostPath` (`DirectoryOrCreate`) volume named `gpustack-compile-cache` from `/var/lib/gpustack/cache/<subpath>` to `/var/lib/gpustack/cache` (read-write) on the `main` container when no user `additionalVolumes` entry overlaps `/var/lib/gpustack/cache`.
  - Non-compilation flag changes (such as `--max-running-requests`, `--gpu-memory-utilization`, `--mem-fraction-static`, or `--served-model-name`) produce the exact same `<compile-hash[:8]>` and reuse the existing cache directory.
  - Compilation-affecting flag changes (such as `--dtype`, `--quantization`, `--max-model-len`, `--context-length`, `--compilation-config`, `--enable-torch-compile`, or `TP/PP/DP/EP/CP`) produce a distinct path segment and isolate the cache directory.
  - When a user-specified `additionalVolumes` entry mounts at, inside, or around `/var/lib/gpustack/cache`, admission accepts it and `renderModelDeploymentPodTemplate` yields the default `gpustack-compile-cache` `HostPath` volume while keeping `VLLM_CACHE_ROOT=/var/lib/gpustack/cache` or `SGLANG_CACHE_DIR=/var/lib/gpustack/cache` in the defaulted env tier.

#### Feature 2: Per-Role `startupTimeoutSeconds` and Probe Tuning

- **API Field Addition:**
  - Add `StartupTimeoutSeconds *int32` (`json:"startupTimeoutSeconds,omitempty" protobuf:"varint,20,opt,name=startupTimeoutSeconds"`) to `ModelDeploymentRole` in [model_deployment.go](../api/worker/v1alpha1/model_deployment.go) with markers `// +optional`, `// +k8s:validation:default=1800`, `// +k8s:validation:minimum=10`, `// +k8s:validation:maximum=86400`.
- **Probe Calculation in [model_deployment_render.go](../pkg/worker/controllers/worker/model_deployment_render.go):**
  - `startupProbe` on the `main` container keeps `periodSeconds: 10` and computes `failureThreshold = max(1, int32((effectiveStartupTimeoutSeconds + 9) / 10))`. When `role.StartupTimeoutSeconds == nil`, `effectiveStartupTimeoutSeconds` defaults to `1800` (`failureThreshold = 180`, preserving the existing 30-minute default).
  - Raise `modelDeploymentProbeTimeoutSeconds` from `3` to `5` (`startupProbe`, `readinessProbe`, and `livenessProbe` on `main`, matching `modelDeploymentEncoderProbeTimeoutSeconds = 5`) to prevent false probe timeouts when Python's event loop is briefly blocked during weight or graph initialization.
- **Acceptance Criteria:**
  - Omitting `startupTimeoutSeconds` renders `startupProbe.failureThreshold = 180`, `periodSeconds = 10`, `timeoutSeconds = 5`.
  - Setting `startupTimeoutSeconds: 3600` on a role renders `startupProbe.failureThreshold = 360`, `periodSeconds = 10`, `timeoutSeconds = 5`.
  - Setting `startupTimeoutSeconds` outside `[10, 86400]` is rejected by CRD schema validation in [zz_generated.crds.go](../api/worker/v1alpha1/zz_generated.crds.go).

#### Feature 3: Startup-Aware Health Qualification (`modelDeploymentMemberReadiness`)

- **Bug Fix in [model_deployment_health.go](../pkg/worker/controllers/worker/model_deployment_health.go):**
  - Reuse `isTerminalWaitReason` from [kv_cache_backend.go](../pkg/worker/controllers/worker/kv_cache_backend.go) (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `InvalidImageName`, `CreateContainerConfigError`, `CreateContainerError`, `RunContainerError`).
  - In `modelDeploymentMemberReadiness(pod *corev1.Pod)`:
    1. If `PodReady == True`, return `modelDeploymentLegVerified`.
    2. Inspect `pod.Status.InitContainerStatuses` and `pod.Status.ContainerStatuses`: if any container has `cs.State.Waiting != nil && isTerminalWaitReason(cs.State.Waiting.Reason)`, return `modelDeploymentLegFailed`.
    3. If `pod.Status.Phase == corev1.PodRunning` and any container has `cs.State.Running != nil && (cs.Started == nil || !*cs.Started)` (meaning kubelet's `startupProbe` has not yet succeeded), return `modelDeploymentLegUnknown` (Hold) instead of falling through to `PodReady == False` (`modelDeploymentLegFailed`).
    4. In `qualifyModelDeploymentInstance` (`model_deployment_health.go:329-343`), also hold (`membersReady.Verdict = modelDeploymentLegUnknown`) when `unready == 0 && unobserved > 0` so a multi-member replica with one ready member and one starting member is held rather than falsely reported verified or failed.
- **Acceptance Criteria:**
  - A `ModelDeployment` whose Pods are running and executing `startupProbe` (`Started == false`) stays in `Phase: Starting` and reports `EndpointEligibility` as `Unknown` (`Reason=NotObserved` or `Reason=HeldUnverified`), never `False` (`Reason=Revoked`).
  - Once `Started == true`, if `PodReady == False`, `modelDeploymentMemberReadiness` returns `modelDeploymentLegFailed` and `EndpointEligibility` reports `False` (`Reason=Revoked`) as before.
  - If any container enters `CrashLoopBackOff` or another terminal waiting state matched by `isTerminalWaitReason` during startup (`Started == false`), `modelDeploymentMemberReadiness` returns `modelDeploymentLegFailed` immediately.

#### Feature 4: Graph-Mode E2E Verification

- **E2E Test Addition in `.agents/skills/gpustack-operator-e2e/`:**
  - Add `case-127.sh` under [.agents/skills/gpustack-operator-e2e/cases/case-127.sh](../.agents/skills/gpustack-operator-e2e/cases/case-127.sh) and register row `127` in [.agents/skills/gpustack-operator-e2e/SKILL.md](../.agents/skills/gpustack-operator-e2e/SKILL.md).
  - Structure `case-127.sh` with two tiers following existing E2E conventions (`case-124.sh` / `case-125.sh` / `case-126.sh`):
    1. **Always-runnable verification rows (no accelerator required):** verify that a `ModelDeployment` with `startupTimeoutSeconds: 3600` renders the `gpustack-compile-cache` `HostPath` volume under `/var/lib/gpustack/cache/<manufacturer>-<engine>/<model-identity>/tp...-<hash8>`, mounts it at `/var/lib/gpustack/cache`, yields `gpustack-compile-cache` when a user mounts `/var/lib/gpustack/cache` via `additionalVolumes`, injects `VLLM_CACHE_ROOT` / `SGLANG_CACHE_DIR`, sets `startupProbe.failureThreshold = 360` and `timeoutSeconds = 5`, and keeps `EndpointEligibility` out of `Revoked` while `Started == false`.
    2. **Accelerator graph-mode warm-restart rows (`AUTO-SKIP` when no real accelerator/engine image is available):** deploy a real graph-mode `ModelDeployment`, verify that `/var/lib/gpustack/cache/<subpath>` is populated on the host node after reaching `Ready`, delete the Pod, and verify that the warm restart reuses the existing cache directory and reaches `Ready`.

### Notes / Constraints / Caveats

- **Upstream Engine Cache Behavior (Measured from Source):**
  - `vLLM` (`github.com/vllm-project/vllm` @ `df6c7ad031`, `vllm/compilation/backends.py:1038-1086` and `vllm/compilation/compiler_interface.py:456-475`) uses `VLLM_CACHE_ROOT` and creates `$VLLM_CACHE_ROOT/torch_compile_cache/<H>/rank_<rank>_<dp_rank>/<prefix>`, redirecting `TORCHINDUCTOR_CACHE_DIR` and `TRITON_CACHE_DIR` beneath that directory at runtime. Setting `TORCHINDUCTOR_CACHE_DIR` directly on the container environment is overwritten by `vLLM`; `VLLM_CACHE_ROOT` is the authoritative variable.
  - `vllm-ascend` (`github.com/vllm-project/vllm-ascend` @ `f8a759a0`, `vllm_ascend/compilation/compiler_interface.py:164-205,241-259,309-351`) plugs `AscendCompiler` into `vLLM`'s `CompilerManager`, inheriting `VLLM_CACHE_ROOT` for its `cache_dir` and persisting `npugraph_ex` generated Python code (while intentionally skipping subgraphs that contain `triton_kernel_wrapper` calls because `kernel_side_table` indices are process-local).
  - `SGLang` (`github.com/sgl-project/sglang` @ `40f5d9411f`, `python/sglang/srt/environ.py:1241-1248,2050-2078` and `python/sglang/srt/compilation/backend.py:91-125,407-424`) uses `SGLANG_CACHE_DIR` and redirects `TRITON_CACHE_DIR`, `TORCHINDUCTOR_CACHE_DIR`, `CUDA_CACHE_PATH`, `TILELANG_CACHE_DIR`, and `FLASHINFER_WORKSPACE_BASE` beneath `SGLANG_CACHE_DIR`. Because `SGLang`'s `CompilerManager.compute_hash()` currently hashes only `CacheBase.get_system()` and `torch_key()` (omitting model config, parallelism degrees, dtype, quantization, and `max_model_len`, and hardcoding `rank_0_0`), operator-level path isolation via `BuildModelDeploymentCompileCacheSubPath` is mandatory to prevent cross-model and cross-topology collisions.
- **Forward Compatibility with Upstream Engine Changes:**
  - Because the operator mounts the isolated host subdirectory at `/var/lib/gpustack/cache` and sets only the top-level root variable (`VLLM_CACHE_ROOT` or `SGLANG_CACHE_DIR`) without overriding `compilation_config.cache_dir`, any relative directory tree an engine creates inside `$VLLM_CACHE_ROOT` or `$SGLANG_CACHE_DIR` composes cleanly (`OperatorHostSubPath / EngineRelativePath`).
- **Cache Invalidation and Eviction Rules:**
  - When a cache miss occurs (empty directory or new `<compile-hash[:8]>`), both `vLLM` and `SGLang` fall back to compiling from scratch and writing the new artifacts.
  - Because `vLLM`'s `InductorStandaloneAdaptor.load()` (`vllm/compilation/compiler_interface.py:421-424`) loads files referenced by `vllm_compile_cache.py` without catching `FileNotFoundError`, operators cleaning host caches must delete entire `<compile-hash[:8]>` (or `<model-identity>`) directories rather than deleting individual binary files inside a leaf directory while leaving `vllm_compile_cache.py` intact.

### Boundaries

- **Always:**
  - Keep the compilation cache directory calculation in the standalone function `BuildModelDeploymentCompileCacheSubPath` with a clear doc comment explaining how to update the mapping if `vLLM` or `SGLang` changes its compiled graph cache path or environment variables.
  - Default the host compilation cache root to `/var/lib/gpustack/cache` (matching `gpustack/gpustack`).
  - Run `make generate`, `make lint`, and `make lint docs` after modifying API types, controller/webhook Go sources, E2E shell scripts, or documentation indexes.
  - Preserve user-supplied `VLLM_CACHE_ROOT` or `SGLANG_CACHE_DIR` environment variables in `role.Env` if the user explicitly overrides them.
- **Ask first:**
  - Adding any new CRD fields beyond `startupTimeoutSeconds` on `ModelDeploymentRole`.
  - Modifying Helm chart values in `deploy/gpustack-operator/chart/values.yaml`.
- **Never:**
  - Set `vLLM`'s explicit `compilation_config.cache_dir` flag from the operator, because doing so bypasses `vLLM`'s internal `VllmConfig` (`config.json`) hash and `rank_<rank>_<dp_rank>` subdirectory partitioning.
  - Migrate or delete pre-existing directories under `/var/lib/gpustack/cache` created outside the operator's `<manufacturer>-<engine>/` hierarchy.
  - Expose raw `corev1.Probe` fields on `ModelDeploymentRole`.

### Risks and Mitigations

- **Risk: Concurrent cold-start writes from multiple replicas on the same node.** Neither `vLLM`'s `CompilerManager.save_to_file()` (`vllm/compilation/backends.py:218-224`) nor `SGLang`'s `CompilerManager.save_to_file()` (`python/sglang/srt/compilation/backend.py:109-115`) uses `flock` or atomic rename when writing `vllm_compile_cache.py` / `sglang_compile_cache.py`, and `SGLang` hardcodes `rank_0_0` across TP ranks (`backend.py:415-418`). → **Mitigation:** Document that multi-replica deployments on a cold node should either warm up with 1 replica before scaling out or rely on `vLLM` (which separates TP/PP/DP ranks into distinct `rank_<rank>_<dp_rank>` directories).
- **Risk: In-place mutation of weights inside a `PersistentVolumeClaim` without recreating `ModelArtifact`.** PVC sources have no `ManifestDigest`. → **Mitigation:** Key PVC model identity by `ma.Namespace + "/" + ma.Name + "/" + string(ma.UID) + "/" + claimName + "/" + subPath` so recreating the `ModelArtifact` or changing `subPath` isolates the cache; for `vLLM`, its internal `ModelConfig` hash (`config.json`) provides a second layer of isolation even if PVC weights are replaced in place with a different architecture.
- **Risk: Unbounded host disk growth under `/var/lib/gpustack/cache`.** Repeated changes to compilation flags create new `<compile-hash[:8]>` directories. → **Mitigation:** Structure the directory hierarchy as `<manufacturer>-<engine>/<model-identity>/tp<TP>-pp<PP>-dp<DP>-ep<EP>-cp<PCP>_<DCP>-<hash8>` so administrators can inspect and prune stale model or topology caches with standard filesystem tools (`rm -rf` at the `<model-identity>` or `<hash8>` directory level).
- **Risk: Operator-managed `HostPath` volume interaction with tenancy invariants.** User `HostPath` volumes are gated by `InstanceHostPathVolumeAllowed` in `validateModelDeploymentHostAccess` (`pkg/worker/webhooks/worker/model_deployment.go:129-159`), and `TestModelDeploymentRolesSharedMemory` (`model_deployment_elastic_workload_test.go:480`) previously asserted that no volume on a rendered Pod had `HostPath != nil`. → **Mitigation:** The operator-injected `gpustack-compile-cache` volume is strictly scoped to `/var/lib/gpustack/cache/<subpath>` where every path segment is operator-derived (`[a-z0-9_-]`), never user-controlled; update `TestModelDeploymentRolesSharedMemory` to allow only the operator-owned `gpustack-compile-cache` volume while continuing to forbid any other `HostPath` volume.
- **Risk: Slow container startup masking a genuinely wedged process.** Raising `startupTimeoutSeconds` allows a container to stay in `Started == false` longer. → **Mitigation:** Keep default `startupTimeoutSeconds` at `1800` seconds (30 minutes), cap maximum at `86400` seconds, and continue failing immediately on terminal waiting reasons (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError`, `CreateContainerError`, `InvalidImageName`).

## Design Details

### Commands

```bash
# Regenerate deepcopy, protobuf, and CRD schema Go files after editing api/worker/v1alpha1
make generate

# Run unit tests for API, worker controllers, and webhooks
go test -v ./api/worker/v1alpha1/... ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...

# Run full linter and docs/shell gates
make lint
make lint agents-shell
make lint docs

# Build the operator binary
make build
```

### Project Structure

```text
api/worker/v1alpha1/
  model_deployment.go                        # Add StartupTimeoutSeconds *int32 to ModelDeploymentRole (protobuf field 20)
  model_deployment_test.go                   # Pin startupTimeoutSeconds CRD schema default (1800), min (10), max (86400)
  zz_generated.deepcopy.go                   # Regenerated DeepCopyInto for StartupTimeoutSeconds
  generated.proto                            # Regenerated protobuf schema for ModelDeploymentRole
  generated.pb.go                            # Regenerated protobuf marshaler
  generated.protomessage.pb.go               # Regenerated protobuf message helpers
  zz_generated.crds.go                       # Regenerated OpenAPI v3 CRD schema for ModelDeployment
pkg/worker/controllers/worker/
  model_deployment_compile_cache.go          # Standalone BuildModelDeploymentCompileCacheSubPath, compile flag/env canonicalizer, and volume/env/mount-override helpers
  model_deployment_compile_cache_test.go     # Table-driven unit tests for cache path generation, flag whitelist invariance, and env helpers
  model_deployment_connector.go              # Add DefaultModelDeploymentCompileCacheHostDir, ModelDeploymentCompileCacheMountPath, ModelDeploymentCompileCacheVolumeName, and register VLLM_CACHE_ROOT / SGLANG_CACHE_DIR in modelDeploymentDefaultedEnvNames
  model_deployment_health.go                 # Reuse isTerminalWaitReason; hold (modelDeploymentLegUnknown) when Running with Started=false; hold multi-member replica when unready==0 && unobserved>0
  model_deployment_health_test.go            # Table-driven unit tests for Started=false Hold vs Started=true Failed vs CrashLoopBackOff Failed
  model_deployment_render.go                 # Mount gpustack-compile-cache on !takeOver unless overridden by additionalVolumes; inject VLLM_CACHE_ROOT / SGLANG_CACHE_DIR in defaulted env tier; scale startupProbe.failureThreshold from StartupTimeoutSeconds; raise modelDeploymentProbeTimeoutSeconds to 5
  model_deployment_render_test.go            # Unit tests for compile cache volume/env rendering, user additionalVolumes yielding, custom startupTimeoutSeconds probe scaling, and updated volume count in TestRenderModelDeploymentPod_ClientConfigMount
  model_deployment_elastic_workload_test.go  # Update HostPath assertion in TestModelDeploymentRolesSharedMemory to allow operator-owned gpustack-compile-cache volume
pkg/worker/webhooks/worker/
  model_deployment_artifact_test.go          # Extend TestValidateModelDeploymentReservedMountPaths to verify /var/lib/gpustack/cache is open to user additionalVolumes override
.agents/skills/gpustack-operator-e2e/
  SKILL.md                                   # Register Case 127 in the E2E case dispatch table
  cases/case-127.sh                          # E2E test verifying graph-mode compilation cache rendering, user volume yielding, persistence across Pod restart, and startupTimeoutSeconds
docs/
  README.md                                  # Register model_deployment_compile_cache.go entry point and spec link under model-deployment module map
```

### Code Style

Follow the repository's Go conventions in `AGENTS.md`: snake_case filenames, plain and concise comments without decorative symbols or spec task IDs, table-driven unit tests, and explicit standalone helper functions.

```go
const (
	// DefaultModelDeploymentCompileCacheHostDir is the default host directory for
	// engine compilation caches, aligned with gpustack/gpustack's default cache_dir.
	DefaultModelDeploymentCompileCacheHostDir = "/var/lib/gpustack/cache"

	// ModelDeploymentCompileCacheMountPath is the container path where the
	// per-role host compilation cache subdirectory is mounted.
	ModelDeploymentCompileCacheMountPath = "/var/lib/gpustack/cache"

	// ModelDeploymentCompileCacheVolumeName is the volume name used for the
	// host-backed compilation cache on managed ModelDeployment Pods.
	ModelDeploymentCompileCacheVolumeName = "gpustack-compile-cache"

	// DefaultModelDeploymentStartupTimeoutSeconds is the default per-role startup
	// probe timeout budget (30 minutes).
	DefaultModelDeploymentStartupTimeoutSeconds int32 = 1800
)

// BuildModelDeploymentCompileCacheSubPath returns the relative subdirectory path
// under DefaultModelDeploymentCompileCacheHostDir for a ModelDeployment role:
//
//	<manufacturer>-<engine>/<model-identity>/tp<TP>-pp<PP>-dp<DP>-ep<EP>-cp<PCP>_<DCP>-<hash8>
//
// NOTE: This mapping is coupled to how vLLM (VLLM_CACHE_ROOT, including vllm-ascend)
// and SGLang (SGLANG_CACHE_DIR) organize their compiled graph and kernel caches.
// If future versions of vLLM or SGLang change their compilation cache directory
// layout, environment variable names, or compilation-affecting CLI flags, update
// the mapping and flag table in this function accordingly.
func BuildModelDeploymentCompileCacheSubPath(in ModelDeploymentCompileCacheInput) (string, error) {
	// ...
}
```

### Implementation Plan

#### Prerequisite Testing Updates

Before or alongside the rendering changes in `T3` and webhook verification in `T4`, two existing unit tests that assert exact volume counts or forbid `HostPath` volumes on `ModelDeployment` Pods must be updated so their invariants remain accurate:

1. `TestModelDeploymentRolesSharedMemory` in [model_deployment_elastic_workload_test.go](../pkg/worker/controllers/worker/model_deployment_elastic_workload_test.go):
   - Line 480 currently asserts `require.Nil(t, pod.Spec.Volumes[j].HostPath, "%s reaches the host filesystem", pod.Name)` across every volume on the Pod. Because managed (`!takeOver`) Pods now mount the operator-owned `gpustack-compile-cache` `HostPath` volume, update this check to assert that only `ModelDeploymentCompileCacheVolumeName` may carry a `HostPath` (and only when `!tc.takeOver`), while `/dev/shm` and all other volumes still have `HostPath == nil`.
2. `TestRenderModelDeploymentPod_ClientConfigMount` in [model_deployment_render_test.go](../pkg/worker/controllers/worker/model_deployment_render_test.go):
   - Lines 1515 and 1526 assert `require.Len(t, pod.Spec.Volumes, 2)` and `require.Len(t, mounts, 2)`. With `gpustack-compile-cache` mounted on managed Pods, update the expected counts to `3` and assert the presence of `gpustack-compile-cache`.
3. `TestValidateModelDeploymentReservedMountPaths` in [model_deployment_artifact_test.go](../pkg/worker/webhooks/worker/model_deployment_artifact_test.go):
   - Keep `/var/lib/gpustack/model` and `/var/lib/gpustack/model-cache` reserved when `spec.model.artifactRef != nil`, while adding cases confirming `/var/lib/gpustack/cache` is allowed in `additionalVolumes` with or without `artifactRef`.

#### Task DAG

```text
Wave 0 (parallel):
  T1 (API + CRD codegen)
  T2 (Standalone compile-cache path builder + connector constants)
  T5 (Startup-aware health qualification in model_deployment_health.go)

Wave 1 (parallel, after Wave 0 gates):
  T3 (Pod rendering: cache volume/env + user volume yield + startupProbe scaling)                    [Blocked by: T1, T2]
  T4 (Webhook reserved-path verification for /var/lib/gpustack/cache user override)                  [Blocked by: T2]

Wave 2 (integration & docs):
  T6 (E2E case-127.sh + SKILL.md table row)                                                          [Blocked by: T1, T3, T4, T5]
  T7 (Shared index update in docs/README.md + final full verification)                               [Blocked by: T2, T6]
```

---

#### [x] T1: Add `StartupTimeoutSeconds` to `ModelDeploymentRole` and Regenerate API Artifacts

- **Blocked by:** none
- **Gate:** `review` (new public CRD field and protobuf wire tag `20`)
- **Owns:**
  - [model_deployment.go](../api/worker/v1alpha1/model_deployment.go)
  - [model_deployment_test.go](../api/worker/v1alpha1/model_deployment_test.go)
  - [zz_generated.deepcopy.go](../api/worker/v1alpha1/zz_generated.deepcopy.go)
  - [generated.proto](../api/worker/v1alpha1/generated.proto)
  - [generated.pb.go](../api/worker/v1alpha1/generated.pb.go)
  - [generated.protomessage.pb.go](../api/worker/v1alpha1/generated.protomessage.pb.go)
  - [zz_generated.crds.go](../api/worker/v1alpha1/zz_generated.crds.go)
- **Description:**
  - Add `StartupTimeoutSeconds *int32` (`json:"startupTimeoutSeconds,omitempty" protobuf:"varint,20,opt,name=startupTimeoutSeconds"`) to `ModelDeploymentRole` in [model_deployment.go](../api/worker/v1alpha1/model_deployment.go) with markers `// +optional`, `// +k8s:validation:default=1800`, `// +k8s:validation:minimum=10`, `// +k8s:validation:maximum=86400`.
  - Run `make generate` to update deepcopy, protobuf, and CRD OpenAPI schema files.
  - Add a schema test in [model_deployment_test.go](../api/worker/v1alpha1/model_deployment_test.go) (following `TestModelDeploymentRoleSizeBoundsSurviveMutability`) verifying that `spec.roles[].startupTimeoutSeconds` in `GetCustomResourceDefinitions()` carries default `1800`, minimum `10`, and maximum `86400`.
- **Acceptance:**
  - `make generate` succeeds with only the five generated files under `api/worker/v1alpha1/` modified.
  - `startupTimeoutSeconds` is present in the generated CRD schema with `Default == 1800`, `Minimum == 10`, and `Maximum == 86400`.
- **Verify:**
  ```bash
  make generate
  go test -v ./api/worker/v1alpha1/... -run TestModelDeploymentRole
  make lint
  ```

---

#### [x] T2: Implement Standalone `BuildModelDeploymentCompileCacheSubPath` and Connector Constants

- **Blocked by:** none
- **Gate:** `review` (defines exported constants and `BuildModelDeploymentCompileCacheSubPath` consumed by `T3` and `T4`)
- **Owns:**
  - [model_deployment_compile_cache.go](../pkg/worker/controllers/worker/model_deployment_compile_cache.go)
  - [model_deployment_compile_cache_test.go](../pkg/worker/controllers/worker/model_deployment_compile_cache_test.go)
  - [model_deployment_connector.go](../pkg/worker/controllers/worker/model_deployment_connector.go)
- **Description:**
  - In [model_deployment_connector.go](../pkg/worker/controllers/worker/model_deployment_connector.go), declare `DefaultModelDeploymentCompileCacheHostDir = "/var/lib/gpustack/cache"`, `ModelDeploymentCompileCacheMountPath = "/var/lib/gpustack/cache"`, and `ModelDeploymentCompileCacheVolumeName = "gpustack-compile-cache"` alongside `ModelDeploymentModelMountPath`, and add `"VLLM_CACHE_ROOT"` and `"SGLANG_CACHE_DIR"` to `modelDeploymentDefaultedEnvNames` so `ModelDeploymentDefaultsEnv` reports them as defaulted (user-overridable) rather than owned.
  - Create [model_deployment_compile_cache.go](../pkg/worker/controllers/worker/model_deployment_compile_cache.go) implementing:
    - `BuildModelDeploymentCompileCacheSubPath(in ModelDeploymentCompileCacheInput) (string, error)` with the required doc comment explaining the `vLLM` (`VLLM_CACHE_ROOT`), `vllm-ascend`, and `SGLang` (`SGLANG_CACHE_DIR`) cache layout coupling and maintenance instructions.
    - Manufacturer derivation (`"cpu"` when `!in.InstanceType.Spec.Acceleratable`, else `strings.ToLower(string(in.InstanceType.Status.Detail.Manufacturer))`, returning a loud error if an acceleratable `InstanceType` has an empty manufacturer).
    - Engine upstream name derivation via `ModelDeploymentEngineUpstreamName(in.Deployment.Spec.Engine.Name)`.
    - Model identity derivation across Hub/ModelScope (`ManifestDigest` or UID fallback matching `modelArtifactKVIdentity`), OCI Image (`@sha256:` digest or `stringx.SumBySHA256(ImageReference)` fallback for tag-only references), PVC (`stringx.SumBySHA256(ns/name/uid/claimName/subPath)[:32]`), and engine self-pull (`stringx.SumBySHA256(md.Spec.Model.Name)[:32]`).
    - Parallelism and compilation-affecting CLI/env whitelist canonicalizer (`<compile-hash[:8]>`), normalizing underscore flags (`--max_model_len` → `--max-model-len`) and ignoring operational serving flags (`--max-running-requests`, `--gpu-memory-utilization`, `--mem-fraction-static`, `--served-model-name`).
    - Helper functions `modelDeploymentCompileCacheVolumes(subPath string)`, `modelDeploymentCompileCacheMountOverridden(mounts []core.VolumeMount)`, and `modelDeploymentCompileCacheDefaultedEnv(engine string)` for `renderModelDeploymentPodTemplate`.
  - Create [model_deployment_compile_cache_test.go](../pkg/worker/controllers/worker/model_deployment_compile_cache_test.go) with table-driven unit tests covering every path component, whitelist invariance, and sensitivity.
- **Acceptance:**
  - Operational flag edits leave `<compile-hash[:8]>` unchanged; compilation flag, parallelism degree, or image edits produce a distinct `<compile-hash[:8]>`.
  - Non-acceleratable `InstanceType` produces `cpu-<engine>`; acceleratable `InstanceType` produces `<manufacturer>-<engine>`.
  - `ModelDeploymentDefaultsEnv` returns `true` and `ModelDeploymentOwnsEnv` returns `false` for `VLLM_CACHE_ROOT` and `SGLANG_CACHE_DIR`.
- **Verify:**
  ```bash
  go test -v ./pkg/worker/controllers/worker -run 'Test.*CompileCache|TestModelDeployment.*Env'
  make lint
  ```

---

#### [x] T5: Fix Startup-Aware Health Qualification in `model_deployment_health.go`

- **Blocked by:** none
- **Owns:**
  - [model_deployment_health.go](../pkg/worker/controllers/worker/model_deployment_health.go)
  - [model_deployment_health_test.go](../pkg/worker/controllers/worker/model_deployment_health_test.go)
- **Description:**
  - In [model_deployment_health.go](../pkg/worker/controllers/worker/model_deployment_health.go):
    - Reuse `isTerminalWaitReason` from [kv_cache_backend.go](../pkg/worker/controllers/worker/kv_cache_backend.go) (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `InvalidImageName`, `CreateContainerConfigError`, `CreateContainerError`, `RunContainerError`).
    - Update `modelDeploymentMemberReadiness(pod *core.Pod)` so that:
      1. `PodReady == True` returns `modelDeploymentLegVerified`.
      2. Any init or app container with `cs.State.Waiting != nil && isTerminalWaitReason(cs.State.Waiting.Reason)` returns `modelDeploymentLegFailed`.
      3. When `pod.Status.Phase == core.PodRunning` and any container has `cs.State.Running != nil && (cs.Started == nil || !*cs.Started)`, return `modelDeploymentLegUnknown` (Hold).
      4. Otherwise, `PodReady == False` returns `modelDeploymentLegFailed`, and `PodReady == Unknown` or absent returns `modelDeploymentLegUnknown`.
    - In `qualifyModelDeploymentInstance` (`model_deployment_health.go:329-343`), add a branch `case unready == 0 && unobserved > 0:` that sets `membersReady.Verdict = modelDeploymentLegUnknown` so a multi-member replica with one ready member and one starting member holds rather than reporting ready or failed.
  - In [model_deployment_health_test.go](../pkg/worker/controllers/worker/model_deployment_health_test.go), add table-driven unit tests verifying that:
    - A running container with `Started == false` and `PodReady == False` returns `modelDeploymentLegUnknown` and keeps `EndpointEligibility` out of `Revoked`.
    - A running container with `Started == true` and `PodReady == False` returns `modelDeploymentLegFailed` and sets `EndpointEligibility` to `False` (`Reason=Revoked`).
    - A container in `CrashLoopBackOff` / `ImagePullBackOff` returns `modelDeploymentLegFailed` immediately.
    - A multi-member replica with one ready member and one starting (`Started == false`) member is held (`Unknown`).
- **Acceptance:**
  - All existing tests in `model_deployment_health_test.go` (`TestAnUnverifiedLegIsAHoldAndNotARevocation`, `TestEndpointEligibilityStatusMapping`, `TestACondemnedReplicaIsNeverProbed`) pass alongside the new startup-hold table cases.
- **Verify:**
  ```bash
  go test -v ./pkg/worker/controllers/worker -run 'Test.*Health|Test.*MemberReadiness|TestEndpointEligibility|TestAnUnverifiedLeg|TestACondemnedReplica|TestQualifiedCount'
  make lint
  ```

---

#### [x] T3: Render Compilation Cache Volume/Env and Scale `startupProbe`

- **Blocked by:** `T1`, `T2`
- **Owns:**
  - [model_deployment_render.go](../pkg/worker/controllers/worker/model_deployment_render.go)
  - [model_deployment_render_test.go](../pkg/worker/controllers/worker/model_deployment_render_test.go)
  - [model_deployment_elastic_workload_test.go](../pkg/worker/controllers/worker/model_deployment_elastic_workload_test.go)
- **Description:**
  - In [model_deployment_render.go](../pkg/worker/controllers/worker/model_deployment_render.go):
    - Raise `modelDeploymentProbeTimeoutSeconds` from `3` to `5` (`:205`).
    - Update `modelDeploymentProbes` (`:1371`) to accept `startupTimeoutSeconds *int32` (defaulting to `DefaultModelDeploymentStartupTimeoutSeconds = 1800` when `nil`) and compute `startupProbe.FailureThreshold = max(1, (effectiveStartupTimeoutSeconds + modelDeploymentProbePeriodSeconds - 1) / modelDeploymentProbePeriodSeconds)`.
    - In `renderModelDeploymentPodTemplate` (`:518-560`), when `!takeOver`:
      - When `!modelDeploymentCompileCacheMountOverridden(mounts)`, compute the cache subpath via `BuildModelDeploymentCompileCacheSubPath` and append the `gpustack-compile-cache` `HostPath` (`DirectoryOrCreate`) volume and `/var/lib/gpustack/cache` volume mount to `vols` and `mounts`. When the user's `additionalVolumes` already mount at, inside, or around `/var/lib/gpustack/cache`, yield the default `HostPath` volume.
      - Pass `modelDeploymentCompileCacheDefaultedEnv(md.Spec.Engine.Name)` into the defaulted tier of `mergeModelDeploymentEnv` (`:1434`) so `VLLM_CACHE_ROOT=/var/lib/gpustack/cache` or `SGLANG_CACHE_DIR=/var/lib/gpustack/cache` is set unless the user already supplied it in `role.Env`.
  - Update the two prerequisite tests (`TestModelDeploymentRolesSharedMemory` in [model_deployment_elastic_workload_test.go](../pkg/worker/controllers/worker/model_deployment_elastic_workload_test.go) and `TestRenderModelDeploymentPod_ClientConfigMount` in [model_deployment_render_test.go](../pkg/worker/controllers/worker/model_deployment_render_test.go)) and regenerate legacy render fixtures.
  - Extend [model_deployment_render_test.go](../pkg/worker/controllers/worker/model_deployment_render_test.go) with table-driven tests covering:
    - Default `startupTimeoutSeconds == nil` (`failureThreshold = 180`, `periodSeconds = 10`, `timeoutSeconds = 5`) and custom `startupTimeoutSeconds` (`10` → `1`, `3600` → `360`, `86400` → `8640`).
    - `gpustack-compile-cache` volume and mount on `vLLM` and `SGLang` Pods across delivery modes (`Node`, `PVC`, `Image`, `Engine`, and `ArtifactRef == nil`), yielding when `additionalVolumes` overlaps `/var/lib/gpustack/cache`, and absence on take-over roles (`len(role.Command) > 0`).
    - Defaulted `VLLM_CACHE_ROOT` / `SGLANG_CACHE_DIR` injection and preservation of user-supplied overrides in `role.Env`.
- **Acceptance:**
  - `go test ./pkg/worker/controllers/worker/...` passes 100%.
- **Verify:**
  ```bash
  go test -v ./pkg/worker/controllers/worker -run 'TestRenderModelDeploymentPod|TestModelDeploymentRolesSharedMemory|TestModelDeploymentElasticLiveness'
  go test ./pkg/worker/controllers/worker/...
  make lint
  ```

---

#### [x] T4: Verify `/var/lib/gpustack/cache` User Override in `ModelDeployment` Validating Webhook

- **Blocked by:** `T2`
- **Owns:**
  - [model_deployment_artifact_test.go](../pkg/worker/webhooks/worker/model_deployment_artifact_test.go)
- **Description:**
  - In [model_deployment_artifact_test.go](../pkg/worker/webhooks/worker/model_deployment_artifact_test.go), extend `TestValidateModelDeploymentReservedMountPaths` with table cases confirming `/var/lib/gpustack/cache` is accepted in `additionalVolumes` both when `spec.model.artifactRef != nil` (`noRef: false`) and when `spec.model.artifactRef == nil` (`noRef: true`), while `/var/lib/gpustack/model` and `/var/lib/gpustack/model-cache` remain reserved when `spec.model.artifactRef != nil`.
- **Acceptance:**
  - User mounts at `/var/lib/gpustack/cache` are accepted both with and without `spec.model.artifactRef`.
  - User mounts overlapping `/var/lib/gpustack/model` or `/var/lib/gpustack/model-cache` when `spec.model.artifactRef != nil` remain rejected.
- **Verify:**
  ```bash
  go test -v ./pkg/worker/webhooks/worker -run TestValidateModelDeploymentReservedMountPaths
  go test ./pkg/worker/webhooks/worker/...
  make lint
  ```

---

#### [x] T6: Add Graph-Mode Compilation Cache & Startup Timeout E2E Case (`case-127.sh`)

- **Blocked by:** `T1`, `T3`, `T4`, `T5`
- **Owns:**
  - [.agents/skills/gpustack-operator-e2e/cases/case-127.sh](../.agents/skills/gpustack-operator-e2e/cases/case-127.sh)
  - [.agents/skills/gpustack-operator-e2e/SKILL.md](../.agents/skills/gpustack-operator-e2e/SKILL.md)
- **Description:**
  - Create [.agents/skills/gpustack-operator-e2e/cases/case-127.sh](../.agents/skills/gpustack-operator-e2e/cases/case-127.sh) following the six-field case header contract (`SKILL.md:326`, no spec anchors or story numbers) and `_rows-lib.sh` structure:
    - Row 1 (always runnable): a `ModelDeployment` with `additionalVolumes` mounting `/var/lib/gpustack/cache` is accepted at admission and yields the operator's default `gpustack-compile-cache` `HostPath` volume in favor of the user's volume while preserving `VLLM_CACHE_ROOT=/var/lib/gpustack/cache`.
    - Row 2 (always runnable): a `ModelDeployment` with `startupTimeoutSeconds: 3600` renders `gpustack-compile-cache` (`HostPath` under `/var/lib/gpustack/cache/<manufacturer>-<engine>/<model-identity>/tp...-<hash8>`), mounts `/var/lib/gpustack/cache`, injects `VLLM_CACHE_ROOT` or `SGLANG_CACHE_DIR`, sets `startupProbe.failureThreshold = 360` and `timeoutSeconds = 5`, and keeps `EndpointEligibility` out of `Revoked` while `Started == false`.
    - Row 3 (`AUTO-SKIP` when real accelerator/engine image is absent): runs a graph-mode `ModelDeployment`, verifies `/var/lib/gpustack/cache/<subpath>` is populated on the node once `Ready`, deletes the Pod, and verifies warm-restart reuse of the populated cache directory.
  - Add row `127` to the case table in [.agents/skills/gpustack-operator-e2e/SKILL.md](../.agents/skills/gpustack-operator-e2e/SKILL.md).
- **Acceptance:**
  - `make lint agents-shell` and `make lint docs` pass with 0 warnings/errors.
- **Verify:**
  ```bash
  make lint agents-shell
  make lint docs
  ```

---

#### [x] T7: Update Shared Module Index in `docs/README.md` and Run Full Verification Gate

- **Blocked by:** `T2`, `T6`
- **Owns:**
  - [docs/README.md](../docs/README.md)
- **Description:**
  - Update the `model-deployment` module entry in [docs/README.md](../docs/README.md) (`:107-112`) to list `pkg/worker/controllers/worker/model_deployment_compile_cache.go` and link this spec per `AGENTS.md`.
  - Run the complete repository verification suite (`make generate`, `make test`, `make lint`, `make lint agents-shell`, `make lint docs`).
- **Acceptance:**
  - `git status` shows no uncommitted codegen diffs after `make generate`.
  - `make lint`, `make lint agents-shell`, `make lint docs`, and unit test suites pass cleanly.
- **Verify:**
  ```bash
  make generate
  go test -race ./api/worker/v1alpha1/... ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...
  make lint
  make lint agents-shell
  make lint docs
  ```

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior tomerge, and I/we will adhere to the project testing conventions in `AGENTS.md`.

#### Baseline Coverage (Measured on `main` @ `1c149b1c`)

- `pkg/worker/controllers/worker`: **84.5%** statement coverage (97 test files, 1254 top-level `Test*` functions, 740 `t.Run` subtests).
- `pkg/worker/webhooks/worker`: **90.5%** statement coverage (29 test files, 249 top-level `Test*` functions, 235 `t.Run` subtests).

#### Unit Tests

1. **CRD Schema Validation ([model_deployment_test.go](../api/worker/v1alpha1/model_deployment_test.go)):**
   - Verify `spec.roles[].startupTimeoutSeconds` in `GetCustomResourceDefinitions()["ModelDeployment"]` carries `Default == 1800`, `Minimum == 10`, and `Maximum == 86400`.
2. **Compilation Cache Path Builder ([model_deployment_compile_cache_test.go](../pkg/worker/controllers/worker/model_deployment_compile_cache_test.go)):**
   - **Manufacturer & Engine:** acceleratable NVIDIA + `vLLM` → `nvidia-vllm/...`; acceleratable Ascend + `vLLM` → `ascend-vllm/...`; non-acceleratable (`Acceleratable: false`) + `vLLM` → `cpu-vllm/...`; acceleratable with empty `Status.Detail.Manufacturer` → returns error; unknown engine → returns error.
   - **Model Identity:** HuggingFace/ModelScope with `ManifestDigest` → `m-<digest[:32]>`; unresolved artifact fallback → `m-<sha256(uid)[:32]>`; OCI Image with `@sha256:` → `m-<ociDigest[:32]>`; OCI Image with floating tag → `m-<sha256(imageRef)[:32]>`; PVC claim → `m-<sha256(ns/name/uid/claimName/subPath)[:32]>`; `ArtifactRef == nil` → `m-<sha256(spec.model.name)[:32]>`.
   - **Parallelism Segment:** all unset → `tp1-pp1-dp1-ep1-cp1_1`; `--tensor-parallel-size 8 --ep-size 4` → `tp8-pp1-dp1-ep4-cp1_1`; `--prefill-context-parallel-size 2 --decode-context-parallel-size 4` → `tp1-pp1-dp1-ep1-cp2_4`.
   - **Whitelist Invariance:** changing `--max-running-requests`, `--gpu-memory-utilization`, `--mem-fraction-static`, or `--served-model-name` produces an identical `<compile-hash[:8]>` and full subpath.
   - **Whitelist Sensitivity & Flag Normalization:** changing `--dtype`, `-q`/`--quantization`, `--max-model-len` (and underscore form `--max_model_len`), `--context-length`, `--compilation-config`, `-O`, `--enforce-eager`, `--enable-torch-compile`, `--cuda-graph-max-bs`, `--speculative-config`, `VLLM_ATTENTION_BACKEND`, `role.Image`, or any parallelism degree produces a different `<compile-hash[:8]>`.
3. **Pod Rendering ([model_deployment_render_test.go](../pkg/worker/controllers/worker/model_deployment_render_test.go) & [model_deployment_elastic_workload_test.go](../pkg/worker/controllers/worker/model_deployment_elastic_workload_test.go)):**
   - Managed roles (`vLLM` and `SGLang`) across all delivery modes (`Node`, `PVC`, `Image`, `Engine`, and `ArtifactRef == nil`) mount `gpustack-compile-cache` (`HostPath` `DirectoryOrCreate`) at `/var/lib/gpustack/cache` on the `main` container and receive `VLLM_CACHE_ROOT=/var/lib/gpustack/cache` or `SGLANG_CACHE_DIR=/var/lib/gpustack/cache` in the defaulted env tier.
   - User-supplied `additionalVolumes` mounting at, inside, or around `/var/lib/gpustack/cache` cause the renderer to yield the default `gpustack-compile-cache` `HostPath` volume while preserving the user's mount and the defaulted `VLLM_CACHE_ROOT` / `SGLANG_CACHE_DIR` env var.
   - User-supplied `VLLM_CACHE_ROOT` or `SGLANG_CACHE_DIR` in `role.Env` overrides the operator default.
   - Command take-over roles (`len(role.Command) > 0`) and the router workload (`model_deployment_router_test.go`) receive neither the volume nor the env var.
   - `startupProbe` on `main` renders literal `FailureThreshold = 180`, `PeriodSeconds = 10`, `TimeoutSeconds = 5` when `StartupTimeoutSeconds == nil`, scales `FailureThreshold` to `360` when `StartupTimeoutSeconds == 3600` and `1` when `StartupTimeoutSeconds == 10`, and keeps `ReadinessProbe` (`3`) and `LivenessProbe` (`6`) failure thresholds unchanged with `TimeoutSeconds = 5`.
4. **Startup-Aware Health Qualification ([model_deployment_health_test.go](../pkg/worker/controllers/worker/model_deployment_health_test.go)):**
   - Running container with `Started == false` and `PodReady == False` → `modelDeploymentLegUnknown` (Hold; `EndpointEligibility` is `NotObserved` or `HeldUnverified`, never `Revoked`).
   - Running container with `Started == true` and `PodReady == False` → `modelDeploymentLegFailed` (`EndpointEligibility` is `False` / `Revoked`).
   - Waiting container matched by `isTerminalWaitReason` (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `InvalidImageName`, `CreateContainerConfigError`, `CreateContainerError`, `RunContainerError`) → `modelDeploymentLegFailed`.
   - Multi-member replica with one `Verified` member and one `Unknown` (`Started == false`) member → `membersReady.Verdict == modelDeploymentLegUnknown` (Hold).
5. **Webhook Reserved Mount Paths ([model_deployment_artifact_test.go](../pkg/worker/webhooks/worker/model_deployment_artifact_test.go)):**
   - `/var/lib/gpustack/cache` is allowed in `additionalVolumes` both when `spec.model.artifactRef != nil` (`noRef: false`) and when `spec.model.artifactRef == nil` (`noRef: true`).
   - `/var/lib/gpustack/model` and `/var/lib/gpustack/model-cache` (and `/var/lib/gpustack` around them) remain rejected when `spec.model.artifactRef != nil` and allowed when `noRef: true`.

#### Integration Tests

- Controller reconciliation convergence tests in `pkg/worker/controllers/worker/` (`TestModelDeploymentRolesSharedMemory` and `convergeModelDeployment` tests) verify full end-to-end reconciliation of rendered `Pod` objects with `gpustack-compile-cache` volumes and `EndpointEligibility` status projection.

#### End-to-End Tests

- [.agents/skills/gpustack-operator-e2e/cases/case-127.sh](../.agents/skills/gpustack-operator-e2e/cases/case-127.sh):
  - **Admission & Render Verification (always runnable):** verifies user `additionalVolumes` override yielding at `/var/lib/gpustack/cache`, rendered `gpustack-compile-cache` `HostPath` subpath structure, `VLLM_CACHE_ROOT` / `SGLANG_CACHE_DIR` env injection, `startupTimeoutSeconds: 3600` → `startupProbe.failureThreshold = 360` and `timeoutSeconds = 5`, and `EndpointEligibility` remaining held (not `Revoked`) while `Started == false`.
  - **Graph-Mode Cache Persistence & Warm Restart (`AUTO-SKIP` without accelerator/engine image):** deploys a graph-mode `ModelDeployment`, verifies `/var/lib/gpustack/cache/<subpath>` is populated on the host node after reaching `Ready`, deletes the Pod, and verifies warm-restart reuse of the populated host cache directory.

## Alternatives

1. **Mount a single flat `/var/lib/gpustack/cache` directory shared by all models and roles without operator subdirectory isolation.**
   - *Rejected:* `SGLang`'s `CompilerManager.compute_hash()` (`python/sglang/srt/compilation/backend.py:91-92`) omits model identity, parallelism degrees (`TP/PP/DP/EP/CP`), `dtype`, `quantization`, and `max_model_len`, and hardcodes `rank_0_0`. Sharing a single root across different models or TP configurations causes `SGLang` workloads to overwrite `sglang_compile_cache.py` and collide on disk.
2. **Hash the entire raw `role.ExtraArgs` and `role.Env` slice for `<compile-hash[:8]>`.**
   - *Rejected:* Hashing operational flags unrelated to graph compilation (such as `--max-running-requests`, `--gpu-memory-utilization`, `--mem-fraction-static`, `--served-model-name`, or logging flags) changes the directory path on routine serving tuning and forces unnecessary cold recompilations. Extracting a canonicalized whitelist of compilation-affecting parameters across `vLLM` and `SGLang` preserves warm cache hits across operational edits while isolating graph-changing edits.
3. **Pass `--compilation-config '{"cache_dir": "/var/lib/gpustack/cache"}'` directly to `vLLM`.**
   - *Rejected:* In `vLLM` (`vllm/compilation/backends.py:1065-1080`), an explicit `compilation_config.cache_dir` bypasses `vLLM`'s internal `<H>` hash (`VllmConfig.compute_hash()`, which hashes the model's `config.json` and traced source files). Setting `VLLM_CACHE_ROOT=/var/lib/gpustack/cache` preserves `vLLM`'s internal `<H>` as a second line of defense.
4. **Expose full `corev1.Probe` overrides (`startupProbe`, `readinessProbe`, `livenessProbe`) on `ModelDeploymentRole`.**
   - *Rejected:* Probe endpoints (`/health`), ports, and headers are internal engine contracts owned by the operator. Exposing a single `startupTimeoutSeconds` field on `ModelDeploymentRole` (mirroring `terminationGracePeriodSeconds`) solves slow cold-start timeouts without leaking low-level probe mechanics.

## Open Questions

None. All design decisions have been resolved.
