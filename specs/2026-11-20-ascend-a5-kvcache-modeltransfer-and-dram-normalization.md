# Spec: Ascend A5 KVCacheBackend and ModelTransfer Communication Resource Support, HIXL Routing Admission, and Mooncake DRAM Normalization

Status: Building
Blocked on: implementation and verification of this specification via `/my-build`.
Type: Feature

- **Related Specs**: [2026-11-19-model-deployment-ascend-a5-comm-res.md](2026-11-19-model-deployment-ascend-a5-comm-res.md) · [2026-09-05-kv-cache-media-and-scaling.md](2026-09-05-kv-cache-media-and-scaling.md) · [2026-11-17-modular-loopwright-auto-sync.md](2026-11-17-modular-loopwright-auto-sync.md)

## Summary

Extend Ascend A5 (`Family == "950"`) communication resource handling beyond `ModelDeployment` KV transfer into `KVCacheBackend` and `ModelTransfer`, admit Ascend `hixl` remote weight delivery in the controller allowlist, and normalize official `mirrored-mooncake` builds to Host RAM (`DRAM`) with `-DUSE_VRAM_SEGMENT=OFF`.
For `mirrored-mooncake`, keep all four build targets (`default`, `cuda`, `cann`, `rocm`), set `-DUSE_VRAM_SEGMENT=OFF` explicitly in `builder-cuda`, align `.auto-sync` instructions and gates to reject `USE_VRAM_SEGMENT=ON`, keep `VRAM` in the `KVCacheBackend` CRD enum for accounting-only self-built images, and converge `docs/` (rendered by `site/`) onto `DRAM`.
For `KVCacheBackend`, treat `protocol: CANN` with an explicit `fabricInterfaceCount >= 1` as the Ascend A5 Host RoCE signal: grant Host RDMA resources, mount `/etc/hixlep` read-only with user-mount yielding, and inject overridable defaults `ASCEND_LOCAL_COMM_RES=""` and `ASCEND_GLOBAL_RESOURCE_CONFIG` using HIXL's canonical flat dotted key (`{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep/ub_endpoint_npu_0.json"}`) without adding CRD fields.
For `ModelTransfer`, forward `ASCEND_LOCAL_COMM_RES` and `ASCEND_GLOBAL_RESOURCE_CONFIG` into `Hixl.initialize()` on both source and target, normalize nested protocol config to HIXL's flat dotted key, fail-closed resolve directory `local_comm_res_path` entries per physical NPU ID, switch host buffers to owned 2 MiB-aligned anonymous `mmap` when `protocol_desc` selects `roce:host`, add an opt-in Helm toggle `modelTransfer.ascend.hostRoCE.enabled` (default `false`) with safe same-image drain rollout in `ModelTransferImageReconciler`, wire A5 `remoteDelivery && hixl` fabric/mount/env defaults on target `ModelDeployment` Pods while keeping KV and weight eligibility separated, and admit `(Ascend, vLLM/SGLang, hixl)` in `modelDeploymentModelTransferDefaultQualified`.
Add Ascend A5 scenario configuration matrices (mirroring `ModelDeployment`'s six-scenario guide) to both `docs/modules/kv-cache/backend.md` and `docs/modules/model-delivery/remote.md`.

## Motivation

This specification is grounded against branch `mola` at `bbcecc593200616573b6cbcaeceed056f5e21b8c` (including `6a2e7d33c19c29a3e935b0a3c3017c99d32e08e2` for `ModelDeployment` A5 communication resources and PR #858 for the Mooncake `0.3.14` CUDA VRAM header patch). Four operational gaps remain across `KVCacheBackend`, `ModelTransfer`, and `mirrored-mooncake`:

1. **Misaligned Mooncake `VRAM` build and documentation semantics**:
   - Upstream `Mooncake` (`v0.3.14`) restricts `USE_VRAM_SEGMENT` to CUDA (`mooncake-common/common.cmake:266-274`). Both `cann` (`ascend_allocator.cpp`) and `rocm` (`client_buffer_allocation.cpp`) allocate store segments in Host RAM, never device memory.
   - `pack/mirrored-mooncake/Dockerfile` enables `USE_VRAM_SEGMENT=ON` on `builder-cuda` (carrying the `0.3.14` header patch on `client_buffer_allocation.cpp`) and carries a misleading comment on `builder-cann` claiming `USE_ASCEND_DIRECT` allocates device memory.
   - `docs/modules/kv-cache/backend.md` shows Ascend `KVCacheBackend` members with `medium: VRAM`. Because `memberResources()` omits `capacityPerMember` from Pod `requests.memory` when `medium == "VRAM"`, Ascend Host RAM segments go unaccounted for in Kubernetes scheduling.
   - On NVIDIA, `USE_VRAM_SEGMENT=ON` calls `cudaMalloc` without `cudaSetDevice`, taking unscheduled memory from GPU 0 alongside inference engines. The default `-cpu` build already links `libibverbs` and AWS `libfabric`, so NVIDIA and AMD Host-RAM stores use RDMA and EFA without `USE_VRAM_SEGMENT`.
   - Without an explicit guard in `.auto-sync/instructions/kv-transfer.md` and `.auto-sync/checks/validate_kv_transfer.sh`, automated Mooncake recipe syncs can re-enable `USE_VRAM_SEGMENT=ON`.

2. **Missing Ascend A5 Host RoCE support in `KVCacheBackend`**:
   - On Ascend A5 UB/UBG/UBoE, a `protocol: CANN` member with unset `fabricInterfaceCount` already works because `Mooncake >= 0.3.13.post1` defaults `adxl.LocalCommRes` to `{"version":"1.3"}` when `ASCEND_LOCAL_COMM_RES` is unset.
   - On Ascend A5 Host RoCE (`roce:host`), `memberProtocolIsHostFabric("ascend")` in `pkg/worker/kvcache/mooncake/member_workload.go` returns `false`. Setting `fabricInterfaceCount >= 1` grants no `hostNetwork`, no `IPC_LOCK`/`SYS_RESOURCE`, and no `device.gpustack.ai/rdma*` device, while `warnKVCacheBackendIdleFabricInterfaceCount` warns that the field has no effect.
   - Even with RDMA granted, `Mooncake v0.3.14` (`transfer_executor_base.cpp:275-281`) injects `{"version":"1.3"}` whenever `ASCEND_LOCAL_COMM_RES` is unset, causing `cann/hixl` (`hixl_options.cc:494-498`) to ignore `GlobalResourceConfig.local_comm_res_path`. Meanwhile, `ascend_allocator.cpp:139-149` switches A5 host allocations from `aclrtMallocHost` to `mmap` only when `ASCEND_GLOBAL_RESOURCE_CONFIG` selects `"roce:host"`.

3. **Missing Ascend A5 H2H / UB / Host RoCE wiring in `ModelTransfer`**:
   - Both `HixlSourceBackend.start()` (`model-transfer/python/gpustack_model_transfer/source/backend.py:469-470`) and `TargetNative._initialize_endpoint()` (`model-transfer/python/gpustack_model_transfer/client_native.py:155-157`) hardcode `initialize(endpoint, {"BufferPool": "0:0"})`. Because `cann/hixl` C++ never reads `ASCEND_LOCAL_COMM_RES` or `ASCEND_GLOBAL_RESOURCE_CONFIG` from process environment variables, A5 pure Host-to-Host UB (`ub_ctp:host`), UBG/UBoE, and Host RoCE (`roce:host`) cannot be configured.
   - Pinned `hicann/hixl` (`60dbe9d1`, `src/hixl/engine/hixl_options.cc:203-208`) looks up the literal top-level dotted key `"comm_resource_config.protocol_desc"` (not nested `{"comm_resource_config":{"protocol_desc":...}}`) and requires `local_comm_res_path` to point to a single regular file (`S_ISREG`).
   - Both `AclHostOwner` (`source/memory.py`) and `BorrowedAclOwner` (`client_native.py`) allocate host buffers unconditionally with `aclrtMallocHost`, which fails on Ascend A5 Host RoCE NICs.
   - The Ascend source DaemonSet (`deploy/gpustack-operator/chart/templates/model-transfer/daemonset.yaml`) neither mounts `/etc/hixlep` nor enables `hostNetwork`, and its `OnDelete` rollout controller (`pkg/worker/controllers/worker/model_transfer.go:126-179`) replaces Pods only on image changes, leaving same-image config toggles unapplied.
   - On target `ModelDeployment` Pods, `ModelDeploymentDirectInterfaceProtocol` (`pkg/worker/controllers/worker/model_deployment_fabric.go:59-84`) omits `remoteDelivery && hixl` on Ascend A5, blocking single-Pod `Server` roles from requesting `resources.interface > 0` for Host RoCE weight loading.

4. **Ascend `hixl` blocked from remote weight delivery routing**:
   - `modelDeploymentModelTransferDefaultQualified` (`pkg/worker/controllers/worker/model_deployment_transfer.go:249-253`) admits only `(NVIDIA, vLLM/SGLang, nixl)`.
   - All Ascend `hixl` deployments are classified as `UnqualifiedTransferBackend`, silently falling back to local loading under `Prefer` and blocking Pod creation under `Require` with no operator-facing toggle.

### Goals

- Target users: cluster administrators operating Ascend A5 (`950PR`/`950DT`) or mixed Ascend/NVIDIA/AMD clusters, workload owners deploying `KVCacheBackend` and remote-delivered `ModelDeployment` workloads, and maintainers running `.auto-sync` pipelines.
- Normalize official `pack/mirrored-mooncake/Dockerfile` builds to Host RAM (`DRAM`) by keeping all four build targets (`default`, `cuda`, `cann`, `rocm`), setting `-DUSE_VRAM_SEGMENT=OFF` explicitly in `builder-cuda`, removing the `0.3.14` VRAM header patch, and fixing inaccurate `cann`/`cuda` comments.
- Align `.auto-sync/instructions/kv-transfer.md`, `.auto-sync/checks/validate_kv_transfer.sh`, and `.auto-sync/checks/selftest_kv_transfer.sh` to enforce `USE_VRAM_SEGMENT=OFF` while preserving the existing four-guard validation structure.
- Preserve `members[].medium: ["DRAM", "VRAM"]` and its existing memory-request accounting in `KVCacheBackend`, while converging `docs/` (and therefore `site/`) onto `DRAM` and documenting that `VRAM` is an accounting-only option for user-supplied `USE_VRAM_SEGMENT=ON` builds.
- Enable Ascend A5 Host RoCE on `KVCacheBackend` (`protocol: CANN` with explicit `fabricInterfaceCount >= 1`) without adding CRD fields: grant Host RDMA (`hostNetwork`, `IPC_LOCK`, `SYS_RESOURCE`, `device.gpustack.ai/rdma*`), mount `/etc/hixlep` read-only with user-mount yielding, inject overridable defaults `ASCEND_LOCAL_COMM_RES=""` and `ASCEND_GLOBAL_RESOURCE_CONFIG` using `"comm_resource_config.protocol_desc"`, and suppress the `fabricInterfaceCount` no-effect admission warning for `CANN`.
- Enable Ascend A5 UB, pure Host-to-Host UB (`ub_ctp:host`), UBG, UBoE, and Host RoCE (`roce:host`) in `ModelTransfer`:
  - Forward `ASCEND_LOCAL_COMM_RES` and `ASCEND_GLOBAL_RESOURCE_CONFIG` into `Hixl.initialize()` in Python SDK source and target paths, normalize nested `comm_resource_config.protocol_desc` to HIXL's flat dotted key, and fail-closed resolve directory `local_comm_res_path` entries per physical NPU ID.
  - Switch `AclHostOwner` and `BorrowedAclOwner` to owned 2 MiB-aligned anonymous `mmap` buffers when `protocol_desc` selects `roce:host`.
  - Add opt-in Helm setting `modelTransfer.ascend.hostRoCE.enabled` (default `false`) for Ascend source Pods and extend `ModelTransferImageReconciler` to safely drain and replace Pods on same-image template config changes.
  - Admit `(Ascend, vLLM/SGLang, hixl)` in `modelDeploymentModelTransferDefaultQualified` and wire Ascend A5 `remoteDelivery && hixl` fabric/mount/env defaults on target `ModelDeployment` Pods while keeping KV and weight fabric eligibility separate and preserving unaffected workload upgrade hashes.
- Add clear Ascend A5 scenario configuration matrices (user config to resulting behavior/effect) to both `docs/modules/kv-cache/backend.md` and `docs/modules/model-delivery/remote.md`.

### Non-Goals

- Removing `"VRAM"` from the `KVCacheBackend.spec.connection.managed.members[].medium` CRD enum, adding runtime verification of whether a custom image allocates VRAM, or providing a migration path for prior `medium: VRAM` objects.
- Removing the `cuda` or `rocm` build targets from `pack/mirrored-mooncake/Dockerfile` or changing the four-guard stage count expected by `.auto-sync`.
- Adding new CRD fields to `KVCacheBackend` or `ModelDeployment`, or adding new cluster `Setting`s.
- Provisioning `/etc/hixlep/ub_endpoint_npu_*.json` on cluster nodes or building/publishing new container image tags in this change (image publication remains a separate release step).
- Allowing `Encode` roles or asymmetric P/D pairs on Ascend A5 to request `resources.interface > 0` (preserving the [2026-11-19](2026-11-19-model-deployment-ascend-a5-comm-res.md) admission contract).
- Changing `device-manager` `/host` mounting, `/etc/hccl_rootinfo.json` and `/lib/route.conf` injection, or `PodKVCacheWebhook` standalone Pod injection.
- Enabling `HCCL_INTRA_ROCE_ENABLE=1`, `roce:device`, or `ASCEND_ENABLE_USE_FABRIC_MEM=1` on Ascend A5.

## Proposal

Unify Mooncake store images around Host RAM (`DRAM`) across all vendor targets while keeping `VRAM` as an accounting-only API option, and apply one canonical Ascend A5 communication resource contract (`"comm_resource_config.protocol_desc"` flat dotted JSON, read-only `HostPathDirectory` `/etc/hixlep` mounts, and `roce:host` `mmap` buffers) consistently across `KVCacheBackend` and `ModelTransfer`.
Reuse existing signals instead of adding CRD fields: `protocol: CANN` plus an explicit `fabricInterfaceCount >= 1` selects Host RoCE on `KVCacheBackend`; `modelTransfer.ascend.hostRoCE.enabled` opts Ascend source Pods into Host RoCE in Helm with drained same-image rollout; and `Family == "950"` plus `roles[].resources.interface > 0` selects Host RoCE on eligible remote-delivered `ModelDeployment` roles without conflating weight transfer with KV transfer rules.

### User Stories

#### Story 1
As a cluster administrator deploying `KVCacheBackend` across NVIDIA, AMD, and Ascend nodes, I want official `mirrored-mooncake` builds to disable `USE_VRAM_SEGMENT` (`OFF`) and documentation to standardize on `medium: DRAM`, so that Host RAM store capacity is accurately charged to Pod `requests.memory`, no store Pod claims unscheduled GPU 0 memory, and `VRAM` remains clearly documented as an accounting-only option for custom `USE_VRAM_SEGMENT=ON` builds.

#### Story 2
As a maintainer running automated `.auto-sync` updates for `kv-transfer`, I want `.auto-sync/instructions/kv-transfer.md` and `.auto-sync/checks/validate_kv_transfer.sh` to enforce `-DUSE_VRAM_SEGMENT=OFF` without changing the four-stage release-guard layout, so that future Mooncake version bumps never re-enable `USE_VRAM_SEGMENT=ON` or break parallel `.auto-sync` work.

#### Story 3
As a cluster administrator deploying a `CANN` `KVCacheBackend` on Ascend A5 Host RoCE nodes, I want setting `fabricInterfaceCount >= 1` on a `CANN` member group to grant Host RDMA (`hostNetwork`, capabilities, and `device.gpustack.ai/rdma*`), mount `/etc/hixlep` read-only, and provide overridable defaults for `ASCEND_LOCAL_COMM_RES=""` and `ASCEND_GLOBAL_RESOURCE_CONFIG` (`"comm_resource_config.protocol_desc":["roce:host"]` plus `/etc/hixlep/ub_endpoint_npu_0.json`), so that the member starts on Host RoCE cleanly while leaving `CANN` groups without `fabricInterfaceCount` (A2/A3 and A5 UB/UBG/UBoE) byte-for-byte unchanged.

#### Story 4
As a user or cluster administrator running `ModelTransfer` on Ascend A2/A3 or A5 (over UB, pure Host-to-Host UB `ub_ctp:host`, UBG, UBoE, or Host RoCE), I want Ascend `hixl` admitted by default, the Python SDK to forward `ASCEND_LOCAL_COMM_RES` and `ASCEND_GLOBAL_RESOURCE_CONFIG` (with flat-key normalization, fail-closed per-physical-NPU `/etc/hixlep` resolution, and owned `roce:host` `mmap` buffers), and Helm/controller wiring to support Host RoCE on both source (including same-image drained toggles) and target Pods, so that Ascend remote weight delivery works reliably across all A5 interconnect topologies.

#### Story 5
As an operator user configuring Ascend A5 workloads, I want `docs/modules/kv-cache/backend.md` and `docs/modules/model-delivery/remote.md` to each provide a scenario configuration matrix matching `ModelDeployment`, so that I can see at a glance which fields and environment variables activate UB, H2H UB, UBG, UBoE, or Host RoCE.

### Core Features & Acceptance Criteria

1. **Mooncake `USE_VRAM_SEGMENT=OFF` Normalization and `.auto-sync` Alignment**:
   - In `pack/mirrored-mooncake/Dockerfile`:
     - Keep all four targets (`default` final stage `mooncake-store`, `cuda`, `cann`, `rocm`), all four `MOONCAKE_VERSION` release-guard `case` blocks, and all existing runtime smoke tests intact.
     - In `builder-cuda`, remove the `USE_VRAM_SEGMENT=ON` branch, remove the `0.3.14*` `client_buffer_allocation.cpp` `<ylt/util/tl/expected.hpp>` patch arm, and when `option(USE_VRAM_SEGMENT ` exists in `mooncake-common/common.cmake` pass `-DUSE_VRAM_SEGMENT=OFF` explicitly to `cmake` (failing the build if configure log prints `"VRAM SEGMENT is ON"`). Releases predating `USE_VRAM_SEGMENT` omit the flag cleanly.
     - Update comments in `builder-cuda` and `builder-cann` to state accurately that official `mirrored-mooncake` builds allocate store segments in Host RAM (`USE_VRAM_SEGMENT=OFF`) and that `USE_ASCEND_DIRECT` allocates Host RAM rather than device memory.
   - In `.auto-sync/instructions/kv-transfer.md`:
     - Document explicitly that `pack/mirrored-mooncake/Dockerfile` MUST keep `USE_VRAM_SEGMENT` disabled (`-DUSE_VRAM_SEGMENT=OFF`, never `USE_VRAM_SEGMENT=ON`) across Mooncake version bumps.
   - In `.auto-sync/checks/validate_kv_transfer.sh` and `.auto-sync/checks/selftest_kv_transfer.sh`:
     - Preserve the existing release-guard and 5th `context_manager.cpp` checks.
     - Add a static check in `validate_kv_transfer.sh` that rejects non-comment `-DUSE_VRAM_SEGMENT=ON` anywhere in `pack/mirrored-mooncake/Dockerfile` and requires executable `-DUSE_VRAM_SEGMENT=OFF` wired into `builder-cuda`'s `cmake` invocation.
     - Add positive and negative test cases in `selftest_kv_transfer.sh` (valid `builder-cuda`, `OFF` only in a comment or unused assignment, `ON` alongside `OFF`).

2. **`KVCacheBackend` `VRAM` Accounting Contract and Ascend A5 Host RoCE Wiring**:
   - **CRD `medium` contract**:
     - Keep `members[].medium` enum as `["DRAM", "VRAM"]` in `api/worker/v1alpha1/kv_cache_backend.go` and preserve `memberResources()` and `renderMemberEnv()` `medium != "VRAM"` accounting and vendor visibility-hiding behavior (note Ascend has no visibility-hiding env in that list, so `medium: DRAM` + `protocol: CANN` keeps NPU access intact).
     - Update API doc comments in `api/worker/v1alpha1/kv_cache_backend.go` (including `medium`, `image`, and `fabricInterfaceCount` comments) and run `make generate` so `api/worker/v1alpha1/zz_generated.crds.go`, `api/worker/v1alpha1/generated.proto`, `api/worker/zz_generated.openapi.go`, and `pkg/kubeclients/applyconfiguration/worker/**` stay in sync.
   - **Ascend A5 Host RoCE detection on `KVCacheBackend`**:
     - Define `memberGroupUsesCANNHostRoCE(kvcb, member)` in `pkg/worker/kvcache/mooncake/member_workload.go` returning `true` only when `MemberProtocolForGroup(kvcb, member) == "ascend" && member.FabricInterfaceCount != nil && *member.FabricInterfaceCount > 0` (checked **before** `ptr.Deref(member.FabricInterfaceCount, 1)` so `nil` never activates Host RoCE).
     - Keep `MemberProtocols` returning `"ascend"` for `CANN` groups (never rewriting the wire protocol to `"rdma"`).
     - Preserve byte-for-byte identical DaemonSet rendering and `MemberPodSpecHash` for all `CANN` groups with `fabricInterfaceCount == nil` (A2/A3 and A5 UB/UBG/UBoE) and for all `TCP`, `RDMA`, `EFA`, `ROCM`, `MUSA`, and `MACA` groups.
   - **Host RDMA grant, `/etc/hixlep` mount, and overridable defaults for `CANN + fabricInterfaceCount >= 1`**:
     - Grant the exact Host RDMA base (`hostNetwork: true`, `DNSPolicy: ClusterFirstWithHostNet`, `IPC_LOCK` + `SYS_RESOURCE` unioned with user `securityContext`, `device.gpustack.ai/rdma-shared` for count `1` or `device.gpustack.ai/rdma-exclusive` for count `> 1`).
     - Update `warnKVCacheBackendIdleFabricInterfaceCount` (`pkg/worker/webhooks/worker/kv_cache_backend.go:129`) so `CANN` with `fabricInterfaceCount` emits no warning, while `TCP`, `Auto`, `ROCM`, `MUSA`, and `MACA` keep their warning (and update warning text to name `RDMA`, `EFA`, or `CANN`).
     - Reject a `CANN + fabricInterfaceCount` group whose `members[].localDisks[].path` overlaps `/etc/hixlep` (exact, ancestor, or descendant cleaned container path) with an actionable field error in admission, including on updates that switch into `CANN + fabricInterfaceCount`.
     - Mount `/etc/hixlep` read-only (`volumeName: ascend-hixlep`, `hostPath.path: /etc/hixlep`, `hostPath.type: Directory`, `readOnly: true`), yielding cleanly whenever any `members[].hostPaths` entry overlaps `/etc/hixlep` (exact, ancestor, or descendant cleaned `mountPath`).
     - Define `MemberDefaultedEnvs` in `pkg/worker/kvcache/mooncake/keys.go` strictly disjoint from `MemberDerivedEnvs` (containing `ASCEND_LOCAL_COMM_RES` and `ASCEND_GLOBAL_RESOURCE_CONFIG`), extend `TestMemberDerivedEnvs_CoversEveryNameTheRendererEmits` to verify both lists and their disjointness, and emit the two defaults in deterministic order only when absent from `members[].extraEnv`:
       1. `ASCEND_LOCAL_COMM_RES` = `""` (prevents Mooncake from filling `{"version":"1.3"}`).
       2. `ASCEND_GLOBAL_RESOURCE_CONFIG` = `{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep/ub_endpoint_npu_0.json"}`.

3. **`ModelTransfer` Python SDK Ascend A5 HIXL Options, Fail-Closed Physical ID Resolution, and Owned Host RoCE `mmap`**:
   - **Shared HIXL init options builder** in `model-transfer/python/gpustack_model_transfer/backends/hixl/runtime.py` (with env entries in `envs.py`), called by both `HixlSourceBackend.start()` (`source/backend.py`) and `TargetNative._initialize_endpoint()` (`client_native.py`) while keeping `RealHixlAPI.create_agent`'s explicit caller-options seam separate:
     - Always preserve `"BufferPool": "0:0"`.
     - Read `ASCEND_LOCAL_COMM_RES`: when non-empty, set `options["LocalCommRes"] = value`.
     - Read `ASCEND_GLOBAL_RESOURCE_CONFIG`: when non-empty, parse JSON when valid:
       - Normalize nested `{"comm_resource_config": {"protocol_desc": ...}}` into HIXL's literal top-level dotted key `"comm_resource_config.protocol_desc"` (with an existing top-level dotted key taking precedence, preserving all unrelated keys).
       - If `"LocalCommRes"` is unset/empty and `local_comm_res_path` points to an existing directory (such as `"/etc/hixlep"`), lazily resolve `aclrtGetDevice` and `aclrtGetPhyDevIdByUserDevId` on the active ACL thread (probing `libacl_rt.so` then `libascendcl.so`, with explicit injectable resolver for tests) to locate `<dir>/ub_endpoint_npu_<phy_id>.json`. If the symbol is unavailable, the call returns non-zero, or the resolved regular file does not exist, fail closed with a typed `SourceUnavailableError` directing the operator to set an explicit file path or `ASCEND_LOCAL_COMM_RES` (never silently guess physical device `0`).
       - If `local_comm_res_path` is already a regular file (or not an existing directory), pass it through verbatim for HIXL to validate.
     - Set `options["GlobalResourceConfig"]` to the serialized config when non-empty.
   - **Owned Host RoCE `mmap` memory allocator** in `source/memory.py` (`AclHostOwner`) and `client_native.py` (`BorrowedAclOwner`):
     - Implement `uses_roce_host_protocol(env=None) -> bool` inspecting the canonicalized protocol descriptor (supporting both `"comm_resource_config.protocol_desc"` and nested `comm_resource_config.protocol_desc`, string or list) for exact `"roce:host"`, aligned with the effective options passed to HIXL.
     - When `uses_roce_host_protocol()` is `true`, allocate a private anonymous writable `mmap.mmap(-1, allocation_bytes)` sized from `core/budgets.py:hixl_allocation_bytes(byte_size)`, align the base virtual address to 2 MiB, zero-fill the registration padding tail, and store on the block a strong reference to the `mmap` object, allocator kind (`"mmap"`), and aligned offset.
     - Expose payload views via `memoryview(mapping)[aligned_offset + offset : aligned_offset + offset + length]` (so `torch.frombuffer` holds a live buffer-protocol reference rather than a raw-address ctypes view) while registering `aligned_address` with length `align_up_2m(byte_size)` as `hixl.MemType.MEM_HOST`.
     - On `release(block)`, dispatch strictly by the block's recorded allocator kind (independent of later env mutations), make successful release idempotent, keep `mmap.close()` after synchronization/deregistration/finalization, and treat a refused `close()` (for example `BufferError` from a live exported view) as retained under quarantine rather than dropping ownership.
     - When `uses_roce_host_protocol()` is `false` (A2/A3 and A5 UB / pure H2H `ub_ctp:host` / UBG / UBoE), keep `aclrtMallocHost` unchanged.

4. **`ModelTransfer` Chart Opt-In Host RoCE and Same-Image Drained Rollout**:
   - In `deploy/gpustack-operator/chart/values.yaml`:
     - Add `modelTransfer.ascend.hostRoCE.enabled: false` (default `false`) with JSON schema annotations.
   - In `deploy/gpustack-operator/chart/templates/model-transfer/daemonset.yaml`:
     - Stamp a deterministic config-revision annotation (`model-transfer.gpustack.ai/config-hash`) on every source DaemonSet Pod template covering the worker image, `hostNetwork`, `dnsPolicy`, worker `env`, and `/etc/hixlep` volume/mount so same-image config toggles are observable.
     - Apply Host RoCE wiring strictly when `$manufacturer == "ascend"` and `$root.Values.modelTransfer.ascend.hostRoCE.enabled` is `true` (leaving `nvidia`, `amd`, and default `ascend` container specs unchanged):
       - Set `hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet` on the Pod spec.
       - Mount `/etc/hixlep` (`volumeName: ascend-hixlep`, `hostPath.path: /etc/hixlep`, `hostPath.type: Directory`, `readOnly: true`) on container `worker` only.
       - When `modelTransfer.env` does not define `ASCEND_GLOBAL_RESOURCE_CONFIG`, emit default `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep"}'` on container `worker` only (leaving `ASCEND_LOCAL_COMM_RES` unset unless declared in `modelTransfer.env`).
   - In `pkg/worker/controllers/worker/model_transfer.go` and `model_transfer_test.go`:
     - Extend `ModelTransferImageReconciler` to compare the target DaemonSet template's desired revision (worker image plus `model-transfer.gpustack.ai/config-hash` when present) against each owned Pod's revision, reusing the exact `transferDrainFor` / `transferDrainedFor` / `transferWorkerInstance` / `UID` + `ResourceVersion` precondition drain-and-replace contract so toggling `modelTransfer.ascend.hostRoCE.enabled` (`false -> true -> false`) at an identical worker image drains and replaces Ascend source Pods safely.
   - Regenerate `values.schema.json` and `chart/README.md` via `make generate chart`, and add enabled/disabled/user-override/rollback coverage in `deploy/gpustack-operator/chart/ci/test-model-transfer.sh`, `pkg/worker/kuberess/chart_test.go`, and `pkg/worker/controllers/worker/model_transfer_test.go`.

5. **Admit Ascend `hixl` Remote Weight Delivery and Wire Ascend A5 Target `ModelDeployment` Pods**:
   - **Admit Ascend `hixl` in default routing allowlist**:
     - Update `modelDeploymentModelTransferDefaultQualified` (`pkg/worker/controllers/worker/model_deployment_transfer.go:249-253`) to admit both `(ManufacturerNVIDIA, vLLM/SGLang, "nixl")` and `(ManufacturerAscend, vLLM/SGLang, "hixl")`.
     - Update tests in `pkg/worker/controllers/worker/model_deployment_transfer_test.go` that previously relied on Ascend being implicitly unqualified (inject explicit unqualified fixtures for unqualified-gate tests, while asserting default admission of both NVIDIA `nixl` and Ascend `hixl`).
   - **Separate KV fabric eligibility from remote weight eligibility on Ascend A5**:
     - Keep `modelDeploymentAscendKVDirectProtocol` (for KV transfer, KVCache, and multi-Pod/elasticEp `Server`) strictly separate from role-local remote weight Host RoCE eligibility (`modelDeploymentAscendRemoteDirectProtocol`).
     - Preserve [2026-11-19](2026-11-19-model-deployment-ascend-a5-comm-res.md) rules: reject `resources.interface > 0` on Ascend A5 `Encode` roles and asymmetric P/D pairs even when `Model.ArtifactRef != nil`.
     - Allow Ascend A5 (`Family == "950"`) remote weight Host RoCE (`"rdma"`, never `"efa"`) on managed (`len(role.Command) == 0`), non-conflicting `--load-format` `Server` roles (including single-Pod `Server` on `vLLM` and `SGLang`) and symmetric P/D pairs when `Model.ArtifactRef != nil` and the role is eligible for `hixl`.
     - Drive `modelDeploymentAscendHostRoCE` (which controls `vLLM` `--kv-transfer-config` `ascend_local_comm_res_path`) **exclusively** from the KV/collective leg so a weight-only leg never alters KV connector semantics.
     - Align admission (`pkg/worker/webhooks/worker/model_deployment.go`) and reconciliation (`pkg/worker/controllers/worker/model_deployment.go`): when an A5 `Server` (or symmetric P/D) role with `Model.ArtifactRef != nil` requests `resources.interface > 0` under `Prefer` before a source is ready or during per-ordinal fallback, keep the `"rdma"` device grant eligible consistently across admission, no-source wait, remote delivery, and fallback so reconciliation never fails with `"no effective RDMA or EFA transfer leg"`.
   - **Unified `/etc/hixlep` yield, defaulted A5 remote env, and zero-rollout Pod hash stability**:
     - Add `omitempty` `ModelTransferAscendA5` and `ModelTransferAscendHostRoCE` on `ModelDeploymentArtifactRender` (`pkg/worker/controllers/worker/model_deployment_artifact.go`) and zero both in `model_deployment_render.go:2123-2149` unless `!takeOver && Delivery == Remote && ModelTransferBackend == "hixl"` on Ascend A5. Non-A5, A2/A3 same-delivery, and non-Remote Pod hashes stay byte-for-byte identical (`modelDeploymentRenderVersion` stays `5`).
     - Perform a single unified `/etc/hixlep` mount projection in `model_deployment_render.go` across user `additionalVolumes`, KV connector mounts, and A5 remote Host RoCE mounts, checking path overlap (exact, ancestor, and descendant cleaned paths) so `/etc/hixlep` is mounted at most once (`gpustack-ascend-hixlep`, `HostPathDirectory`, `readOnly: true`) and always yields to user `additionalVolumes`.
     - Apply A5 `remoteDelivery && hixl` env defaults via append-if-absent after role and connector env merge:
       - Host RoCE (`interface > 0`): default `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep"}'` and omit defaulted `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'`.
       - UB / UBG / UBoE (`interface == 0`): default `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` (deduplicated with any connector default) and emit neither `/etc/hixlep` nor `ASCEND_GLOBAL_RESOURCE_CONFIG`.
     - Add `inject.AscendGlobalResourceConfigEnv` to `modelDeploymentDefaultedEnvNames` (`pkg/worker/controllers/worker/model_deployment_connector.go:660`; already present in `modelDeploymentScalerCapacityNeutralEnvNames`), keeping `TestModelDeploymentOwnedAndDefaultedCannotDisagree` and scaler prior-identity tests green.

6. **Documentation Normalization and Ascend A5 Scenario Matrices**:
   - Update `docs/modules/kv-cache/backend.md`, `docs/modules/kv-cache/leader.md`, `docs/modules/kv-cache/disk-heavy-nodes.md`, and `docs/modules/kv-cache/local-disk-tier.md`:
     - State clearly that all official `mirrored-mooncake` builds (`-cpu`, `cuda`, `cann`, `rocm`) disable `USE_VRAM_SEGMENT` (`Host RAM` only); explain that `-cpu` ships RDMA (`libibverbs`) and EFA (`libfabric`) for NVIDIA/AMD Host-RAM stores while `-cann` is required on Ascend because `vllm-ascend` speaks HIXL (`protocol: ascend`). Distinguish older published tags from future normalized builds.
     - Document that `members[].medium: VRAM` remains in the CRD schema for accounting alignment only (omitting `capacityPerMember` from Pod `requests.memory` and omitting `DRAM` device-visibility hiding) when users bring their own `USE_VRAM_SEGMENT=ON` image; the operator does not verify actual VRAM usage.
     - Convert the Ascend worked example to `medium: DRAM` + `protocol: CANN`.
     - Add a 6-row **Ascend A5 Scenario Matrix** table and worked examples (using flat dotted `"comm_resource_config.protocol_desc"`):
       1. A5 UB (`ub_ctp`, `protocol: CANN`, no `fabricInterfaceCount`)
       2. A5 UBG (`ub_rtp:device`, `protocol: CANN`, user `ASCEND_GLOBAL_RESOURCE_CONFIG`)
       3. A5 UBoE (`uboe:device`, `protocol: CANN`, user `ASCEND_GLOBAL_RESOURCE_CONFIG`)
       4. A5 Host RoCE (`roce:host`, `protocol: CANN`, `fabricInterfaceCount: 1` → auto RDMA grant, `/etc/hixlep` RO mount, defaulted `ASCEND_LOCAL_COMM_RES=""` + `ASCEND_GLOBAL_RESOURCE_CONFIG`)
       5. A5 Host RoCE custom file / topology override (`extraEnv` / `hostPaths`)
       6. Non-A5 Ascend A2/A3 (`910B`/`910C`, `protocol: CANN`, no `fabricInterfaceCount`)
   - Update `docs/modules/model-delivery/remote.md` (fixing stale HIXL non-allowlisted statements at lines 75–78 and 139) and qualify `docs/modules/model-deployment/parameterize.md:105` (noting SGLang omits `/etc/hixlep` on KV-only legs while remote Host RoCE weight transfer mounts it when `interface > 0`):
     - Document default Ascend `hixl` admission (`vLLM` and `SGLang`, noting CANN 9.2+ engine image and updated source/wheel image requirement).
     - Document `modelTransfer.ascend.hostRoCE.enabled` (including node loopback ports `9250`/`9251`/`9252`, mixed Ascend fleet `nodeSelector` scoping, and same-image drained rollout), Python SDK `ASCEND_*` forwarding, fail-closed physical NPU resolution, and `roce:host` `mmap` buffers.
     - Add a 6-row **Ascend A5 Scenario Matrix** table and worked examples (using flat dotted `"comm_resource_config.protocol_desc"`):
       1. A5 UB default (`ub_ctp:device`, `interface: 0`, `hostRoCE.enabled: false`)
       2. A5 UB pure Host-to-Host H2H (`"comm_resource_config.protocol_desc":["ub_ctp:host"]` on source and target)
       3. A5 UBG (`"comm_resource_config.protocol_desc":["ub_rtp:device"]`)
       4. A5 UBoE (`"comm_resource_config.protocol_desc":["uboe:device"]`)
       5. A5 Host RoCE (`modelTransfer.ascend.hostRoCE.enabled: true` on source + `roles[].resources.interface > 0` on target → auto RDMA grant, `/etc/hixlep` RO mount, fail-closed per-physical-NPU resolution, and owned 2 MiB-aligned `mmap`)
       6. Non-A5 Ascend A2/A3 (`910B`/`910C`, `hostRoCE.enabled: false`, `interface: 0`, `aclrtMallocHost`)
   - Confirm `site/hugo.toml` mounts `docs/` directly into `site/content/docs` and run `make lint docs`.

### Notes / Constraints / Caveats

- **Upstream source contracts verified directly**:
  - `hicann/hixl@60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a` (`include/hixl/hixl_types.h:29-35`, `src/hixl/engine/hixl_options.cc:102, 203-208, 312-318, 417-508`):
    - Direct `Hixl::Initialize` option keys are `"BufferPool"`, `"LocalCommRes"`, and `"GlobalResourceConfig"`.
    - `ParseCommResourceConfig()` checks the **literal top-level dotted key** `"comm_resource_config.protocol_desc"`; nested `{"comm_resource_config":{"protocol_desc":...}}` is ignored by pinned HIXL.
    - `ResolveLocalCommResFromFile()` ignores `local_comm_res_path` whenever `OPTION_LOCAL_COMM_RES` is non-empty, and requires `local_comm_res_path` to reference a single regular file (`S_ISREG`, up to 1 MiB).
  - `kvcache-ai/Mooncake@v0.3.14` (`mooncake-common/common.cmake:266-274`, `mooncake-store/src/common/client_buffer_allocation.cpp`, `ascend_allocator.cpp:139-149`, `transfer_executor_base.cpp:275-320`, `utils.cpp:101-110, 188-299`):
    - Only CUDA supports `USE_VRAM_SEGMENT`. Ascend allocates Host RAM via `ascend_allocate_memory()` (`aclrtMallocHost` by default, or private `mmap` on A5 when `ASCEND_GLOBAL_RESOURCE_CONFIG` selects `"roce:host"`—checking dotted `"comm_resource_config.protocol_desc"` first, then nested—or when `MC_CUSTOM_TOPO_JSON` is set).
    - `TransferExecutorBase::initEngines()` sets `options["adxl.LocalCommRes"] = R"({"version":"1.3"})"` when `ASCEND_LOCAL_COMM_RES` is unset (`nullptr`) and `client_server_mode` is true; setting `ASCEND_LOCAL_COMM_RES=""` sets an empty string instead, letting HIXL fall through to `GlobalResourceConfig.local_comm_res_path`.
  - `vllm-project/vllm-ascend@1c9b0ab01` (`vllm_ascend/utils.py:717-748`):
    - When KV transfer Host RoCE and `ModelTransfer` coexist on one A5 `vLLM` Pod, `NPUWorker.init_device()` reads `/etc/hixlep/ub_endpoint_npu_<phy_id>.json` and overwrites `os.environ["ASCEND_LOCAL_COMM_RES"]` before `load_model()` runs. `gpustack_model_transfer` then forwards that `ASCEND_LOCAL_COMM_RES` alongside `ASCEND_GLOBAL_RESOURCE_CONFIG` (`roce:host` → `mmap`). Standalone remote `Server` roles have no KV connector and rely on `gpustack_model_transfer`'s directory resolver.
- **Runtime image and wheel delivery dependency (outside this code PR)**:
  - Published `gpustack/model-transfer:v0.1.0-cann9.2-a5` and `gpustack/mirrored-mooncake:0.3.13.post1-*` tags predate the T1/T3 changes. Live A5 clusters require rebuilt `mirrored-mooncake` (`0.3.14+` with `USE_VRAM_SEGMENT=OFF`) and `model-transfer` images carrying the updated wheel; engine images with a preinstalled older `gpustack_model_transfer` wheel should upgrade to match.
- **Host network ports and mixed Ascend source fleets**:
  - Under `modelTransfer.ascend.hostRoCE.enabled: true`, the Ascend source Pod shares node networking, placing broker `:9251` (sessions TLS), broker `127.0.0.1:9250`, and worker `127.0.0.1:9252` on the host network namespace while headless Service SNI (`status.podIP:9251`) stays intact. Because the Ascend source DaemonSet matches all Ascend PCI nodes (`pci-19e5.present=true`), mixed A2/A3 + A5 Host RoCE clusters should scope placement via `modelTransfer.nodeSelector`.
- **Upgrade compatibility priority**:
  - Unaffected `ModelDeployment` workloads (non-A5, Ascend A2/A3 same-delivery, and non-Remote Pods) MUST keep byte-for-byte identical renders and `pod-spec-hash` digests across upgrades. When an Ascend cluster explicitly prepares a remote `ModelPrefetch` source, admitting `(Ascend, vLLM/SGLang, hixl)` intentionally transitions matching roles from local delivery to `Remote` delivery. Other legacy compatibility concerns (such as old `medium: VRAM` CRs or pre-normalized tags) are explicitly deferred per project priority.

### Boundaries

- **Always:**
  - Use HIXL's canonical flat dotted key `"comm_resource_config.protocol_desc"` in every operator, chart, SDK, and documentation output.
  - Keep all four targets (`default`, `cuda`, `cann`, `rocm`) and all four `MOONCAKE_VERSION` release guards in `pack/mirrored-mooncake/Dockerfile`, with `-DUSE_VRAM_SEGMENT=OFF` explicit in `builder-cuda` and guarded in `.auto-sync`.
  - Keep `members[].medium: ["DRAM", "VRAM"]` in the `KVCacheBackend` CRD schema and preserve its existing memory-request accounting logic.
  - Yield operator-generated `/etc/hixlep` mounts (checking exact, ancestor, and descendant cleaned paths) and defaulted `ASCEND_*` environment variables cleanly whenever user-declared mounts or env vars already specify them.
  - Keep `HostPathDirectory` (never `HostPathDirectoryOrCreate`) for every `/etc/hixlep` mount.
  - Keep non-A5, Ascend A2/A3 same-delivery, and `CANN` without `fabricInterfaceCount` renders and Pod hashes byte-for-byte unchanged (`modelDeploymentRenderVersion = 5`).
  - Run `make generate`, `make generate chart`, `make lint`, `make lint python`, `make lint chart`, `make lint docs`, Go tests, Python `pytest`, and `.auto-sync/checks/selftest_kv_transfer.sh` before completion.
- **Ask first:**
  - Removing `cuda` or `rocm` targets from `pack/mirrored-mooncake/Dockerfile`, or narrowing `members[].medium` enum to `["DRAM"]`.
  - Adding any new CRD field under `api/worker/v1alpha1/` or `api/worker/v1/` or adding any new cluster `Setting`.
  - Allowing `Encode` roles or asymmetric P/D pairs on Ascend A5 to request `resources.interface > 0`.
  - Changing `pack/model-transfer/Dockerfile` pinned `HIXL_COMMIT` or `001_channel_generation.patch` (owned by `.auto-sync` `gpustack/runner` parity).
- **Never:**
  - Emit nested `{"comm_resource_config":{"protocol_desc":...}}` as the canonical default passed to pinned HIXL.
  - Re-enable `USE_VRAM_SEGMENT=ON` in `pack/mirrored-mooncake/Dockerfile`.
  - Add `ASCEND_LOCAL_COMM_RES` or `ASCEND_GLOBAL_RESOURCE_CONFIG` to `mooncake.MemberDerivedEnvs`.
  - Silently fall back to physical NPU `0` in Python SDK directory resolution when ACL physical ID lookup fails.
  - Use `HostPathDirectoryOrCreate` for `/etc/hixlep` on `KVCacheBackend`, `ModelDeployment`, or `model-transfer` DaemonSet.
  - Enable `hostNetwork: true` or mount `/etc/hixlep` on `model-transfer` Ascend source Pods when `modelTransfer.ascend.hostRoCE.enabled` is `false`.
  - Emit duplicate environment variable names or overlapping duplicate `/etc/hixlep` mounts in any rendered Pod spec.

### Risks and Mitigations

- **Pinned HIXL ignores nested `comm_resource_config.protocol_desc` while succeeding on `local_comm_res_path`** → Emit flat dotted `"comm_resource_config.protocol_desc"` in all producers (T2/T4/T5/T6), normalize nested user input in T3 Python SDK with dotted-key precedence, and test exact output bytes against pinned HIXL parsing rules.
- **`OnDelete` `model-transfer` DaemonSet leaves existing source Pods on stale config when toggling `hostRoCE.enabled` at the same image** → Stamp `model-transfer.gpustack.ai/config-hash` on the DaemonSet Pod template and extend `ModelTransferImageReconciler` in T4 to drain and replace Pods on revision mismatch using the existing `worker-instance` + `UID`/`ResourceVersion` preconditions.
- **Conflating remote weight loading with KV fabric eligibility weakens P/D symmetry or breaks `Prefer` no-source reconciliation** → Separate KV/collective eligibility from role-local remote weight eligibility in T5, preserve `Encode` and asymmetric P/D rejection, and align admission and reconciler RDMA grant eligibility across no-source, ready, and fallback transitions.
- **Unmapped physical NPU IDs (for example visible `0` mapping to physical `6`) load the wrong Host RoCE endpoint file** → Fail closed in T3 when ACL physical ID lookup (`libacl_rt.so`/`libascendcl.so`) is unavailable or fails, rather than guessing device `0`, and allow explicit single-file `local_comm_res_path` or `ASCEND_LOCAL_COMM_RES`.
- **Premature unmapping or GC of anonymous `mmap` while `torch.frombuffer` holds a view** → Retain a strong reference to `mmap.mmap` on the host block in T3, expose `memoryview(mapping)[aligned_offset:...]` directly, release by recorded allocator kind after sync/deregister/finalize, and quarantine on `BufferError` or refused release.
- **Merge conflict with concurrent `.auto-sync` work** → Preserve the 4-stage Dockerfile structure and existing `validate_kv_transfer.sh` release-guard parser; add the `USE_VRAM_SEGMENT=OFF` gate as a standalone validation block and instruction bullet in T1.

## Design Details

### Commands

Verification environment: **local workstation** (`darwin/arm64` / Linux, verified with `go 1.27.3` and `uv 0.8.24`; no Ascend A5 hardware required for unit, golden, chart, `.auto-sync`, and docs gates). Run from the repository root:

```bash
CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/mooncake/... -run 'TestMemberWorkload_|TestMemberDerivedEnvs_|TestFabricDeviceResource_'
CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/... -run 'TestKVCacheBackendWebhook_|TestModelDeploymentWebhook_AscendA5'
CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... -run 'TestTransfer|TestModelDeploymentModelTransfer|TestModelTransfer|TestModelDeploymentDirectInterfaceProtocol|TestModelDeploymentGoldenYAML|TestModelDeploymentLegacyRender|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentScaler|TestRenderModelDeploymentPod_AscendA5'
CGO_ENABLED=1 go test -race ./pkg/worker/kuberess/... -run TestChart
(cd model-transfer/python && uv run pytest)
bash .auto-sync/checks/selftest_kv_transfer.sh
make generate
make generate chart
make lint
make lint python
make lint chart
make lint docs
```

### Project Structure

| Area | Responsibility |
|---|---|
| `pack/mirrored-mooncake/Dockerfile` | Keeps all 4 targets (`default`/`mooncake-store`, `cuda`, `cann`, `rocm`); sets `-DUSE_VRAM_SEGMENT=OFF` explicitly in `builder-cuda`, drops the `0.3.14` VRAM header patch, and fixes Host-RAM comments across `cuda` and `cann`. |
| `.auto-sync/instructions/kv-transfer.md`, `.auto-sync/checks/validate_kv_transfer.sh`, `.auto-sync/checks/selftest_kv_transfer.sh` | Documents and statically enforces `USE_VRAM_SEGMENT=OFF` on `pack/mirrored-mooncake/Dockerfile` while preserving the 4 release guards and 5th `context_manager.cpp` check. |
| `api/worker/v1alpha1/kv_cache_backend.go` & generated API files | Keeps `members[].medium: ["DRAM", "VRAM"]` and updates API doc comments to state `DRAM` normalization, `VRAM` accounting-only semantics, and `CANN + fabricInterfaceCount` Host RoCE behavior (regenerating CRDs, protobuf, OpenAPI, and apply configurations via `make generate`). |
| `pkg/worker/kvcache/mooncake/{keys,member_workload,member_workload_test}.go` & `pkg/worker/webhooks/worker/kv_cache_backend*.go` | Adds `CANN + fabricInterfaceCount >= 1` Host RoCE support: RDMA fabric grant, yielding read-only `/etc/hixlep` mount, `MemberDefaultedEnvs` disjoint from `MemberDerivedEnvs`, yielding defaulted `ASCEND_LOCAL_COMM_RES=""` and flat-key `ASCEND_GLOBAL_RESOURCE_CONFIG`, `localDisks` vs `/etc/hixlep` overlap refusal, and updated `warnKVCacheBackendIdleFabricInterfaceCount`. |
| `model-transfer/python/gpustack_model_transfer/` & `model-transfer/python/tests/` | Forwards `ASCEND_LOCAL_COMM_RES` and canonicalized flat-key `ASCEND_GLOBAL_RESOURCE_CONFIG` to `Hixl.initialize()` on both source (`source/backend.py`) and target (`client_native.py`), fail-closed resolves directory `local_comm_res_path` per physical NPU ID (`backends/hixl/runtime.py`), and switches `AclHostOwner` (`source/memory.py`) and `BorrowedAclOwner` (`client_native.py`) to owned 2 MiB-aligned anonymous `mmap` buffers when `protocol_desc` selects `roce:host`. |
| `deploy/gpustack-operator/chart/`, `pkg/worker/kuberess/chart_test.go`, `pkg/worker/controllers/worker/model_transfer*.go` | Adds `modelTransfer.ascend.hostRoCE.enabled` (default `false`), stamps `model-transfer.gpustack.ai/config-hash`, wires `hostNetwork`, read-only `/etc/hixlep` mount, and non-duplicating flat-key `ASCEND_GLOBAL_RESOURCE_CONFIG` on the Ascend source DaemonSet, extends `ModelTransferImageReconciler` for safe same-image drained rollout, and regenerates chart schema/README. |
| `pkg/worker/controllers/worker/model_deployment*.go`, `testdata/golden-render/**`, `pkg/worker/webhooks/worker/model_deployment*.go` | Admits `(Ascend, vLLM/SGLang, hixl)` in `modelDeploymentModelTransferDefaultQualified`; separates KV vs remote weight Host RoCE eligibility on A5 while keeping `Encode` and asymmetric P/D rejected; aligns webhook and reconciler across `Prefer` no-source/fallback; adds `omitempty` `ModelTransferAscendA5` / `ModelTransferAscendHostRoCE` on `ModelDeploymentArtifactRender`; unifies `/etc/hixlep` yield and appends flat-key `ASCEND_GLOBAL_RESOURCE_CONFIG` (Host RoCE) or `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` (UB). |
| `docs/modules/kv-cache/`, `docs/modules/model-delivery/remote.md`, `docs/modules/model-deployment/parameterize.md` | Converges KV cache docs onto `DRAM`, explains `VRAM` accounting-only semantics and `-cpu` RDMA/EFA vs `-cann` HIXL, documents default `hixl` admission and `modelTransfer.ascend.hostRoCE.enabled`, qualifies `parameterize.md`, and adds complete 6-row Ascend A5 scenario configuration matrices to both guides. |

### Code Style

Follow repository Go conventions (`snake_case` filenames, explicit error handling, short plain comments without decorative symbols, pure renderers) and Python conventions (`uv run ruff check` / `ruff format`, lazy vendor imports, typed `SourceUnavailableError` failures):

```python
_FLAT_PROTOCOL_DESC_KEY = "comm_resource_config.protocol_desc"


def build_hixl_init_options(
    env: Mapping[str, str] | None = None,
    *,
    phy_id_resolver: Callable[[], int] | None = None,
) -> dict[str, str]:
    """Build Hixl.initialize() options from Ascend communication env vars.

    Always preserves BufferPool=0:0, normalizes nested comm_resource_config
    protocol_desc to HIXL's literal dotted key, forwards LocalCommRes and
    GlobalResourceConfig when set, and fail-closed resolves a directory
    local_comm_res_path to <dir>/ub_endpoint_npu_<phy_id>.json when
    LocalCommRes is absent.
    """
    options = {"BufferPool": "0:0"}
    local_comm_res = getenv("ASCEND_LOCAL_COMM_RES", "", env)
    if local_comm_res:
        options["LocalCommRes"] = local_comm_res
    global_cfg = getenv("ASCEND_GLOBAL_RESOURCE_CONFIG", "", env)
    if global_cfg:
        options["GlobalResourceConfig"] = _resolve_global_resource_config(
            global_cfg,
            has_local_comm_res=bool(local_comm_res),
            phy_id_resolver=phy_id_resolver,
        )
    return options
```

### Implementation Plan

Parallel entry frontier: `T1`, `T2`, `T3`, `T4`, and `T5` own strictly disjoint paths and run concurrently (`{T1 ∥ T2 ∥ T3 ∥ T4 ∥ T5} → T6`).

- [x] **T1 · Normalize `mirrored-mooncake` to `USE_VRAM_SEGMENT=OFF` and align `.auto-sync` gates**
      Blocked by: None
      Owns: `pack/mirrored-mooncake/Dockerfile`, `.auto-sync/instructions/kv-transfer.md`, `.auto-sync/checks/validate_kv_transfer.sh`, `.auto-sync/checks/selftest_kv_transfer.sh`
      Gate: review
      Acceptance: `builder-cuda` removes `USE_VRAM_SEGMENT=ON` and the `0.3.14*` `client_buffer_allocation.cpp` header patch, passes `-DUSE_VRAM_SEGMENT=OFF` explicitly when `option(USE_VRAM_SEGMENT ` exists in `common.cmake`, and fails configure if `"VRAM SEGMENT is ON"` appears; all four targets (`default`/`mooncake-store`, `cuda`, `cann`, `rocm`), all four `MOONCAKE_VERSION` release guards, and all runtime smoke tests stay intact; `builder-cuda` and `builder-cann` comments accurately describe Host RAM allocation; `.auto-sync/instructions/kv-transfer.md` documents the `USE_VRAM_SEGMENT=OFF` invariant; `.auto-sync/checks/validate_kv_transfer.sh` rejects executable `-DUSE_VRAM_SEGMENT=ON` across stages and requires executable `-DUSE_VRAM_SEGMENT=OFF` wired into `builder-cuda`'s `cmake` command (ignoring comments); `.auto-sync/checks/selftest_kv_transfer.sh` covers valid, comment-only `OFF`, `ON`-alongside-`OFF`, and pre-option releases and passes cleanly.
      Verify: `bash .auto-sync/checks/selftest_kv_transfer.sh && make lint`

- [x] **T2 · Add `KVCacheBackend` Ascend A5 Host RoCE wiring and update `VRAM` accounting API comments**
      Blocked by: None
      Owns: `api/worker/v1alpha1/kv_cache_backend.go`, `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/zz_generated*.go`, `api/worker/v1/zz_generated*.go`, `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/applyconfiguration/worker/**`, `pkg/worker/kvcache/mooncake/keys.go`, `pkg/worker/kvcache/mooncake/member_workload.go`, `pkg/worker/kvcache/mooncake/member_workload_test.go`, `pkg/worker/webhooks/worker/kv_cache_backend.go`, `pkg/worker/webhooks/worker/kv_cache_backend_test.go`
      Gate: review
      Acceptance: `members[].medium` keeps `["DRAM", "VRAM"]` and existing `memberResources` / `DRAM` visibility-hiding logic with updated API doc comments regenerated cleanly via `make generate`; `memberGroupUsesCANNHostRoCE` gates on `protocol == "ascend"` with original `member.FabricInterfaceCount != nil && *member.FabricInterfaceCount > 0` (before `ptr.Deref`), keeping `MemberProtocols` returning `"ascend"` and leaving `CANN` without `fabricInterfaceCount` and all other protocols byte-for-byte identical in render and `MemberPodSpecHash`; `CANN + fabricInterfaceCount >= 1` grants Host RDMA (`hostNetwork: true`, `DNSClusterFirstWithHostNet`, `IPC_LOCK` + `SYS_RESOURCE` unioned with user `securityContext`, `rdma-shared` for 1 / `rdma-exclusive` for >1), mounts `/etc/hixlep` read-only (`HostPathDirectory`, yielding on exact/ancestor/descendant cleaned `mountPath` overlap with user `hostPaths`), and emits deterministic defaulted `ASCEND_LOCAL_COMM_RES=""` and `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep/ub_endpoint_npu_0.json"}'` yielding cleanly when declared in `members[].extraEnv` (including empty value or `valueFrom`); `MemberDefaultedEnvs` stays strictly disjoint from `MemberDerivedEnvs` and `TestMemberDerivedEnvs_CoversEveryNameTheRendererEmits` covers both; webhook drops the idle `fabricInterfaceCount` warning for `CANN` (preserving it for `TCP`/`Auto`/`ROCM`/`MUSA`/`MACA`) and rejects `localDisks[].path` overlapping `/etc/hixlep` on `CANN + fabricInterfaceCount`.
      Verify: `make generate && CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/mooncake/... ./pkg/worker/webhooks/worker/... -run 'TestMemberWorkload_|TestMemberDerivedEnvs_|TestFabricDeviceResource_|TestKVCacheBackendWebhook_' && make lint`

- [ ] **T3 · Forward HIXL A5 options, fail-closed resolve `/etc/hixlep` per physical NPU, and add owned `roce:host` `mmap` in Python `model-transfer` SDK**
      Blocked by: None
      Owns: `model-transfer/python/gpustack_model_transfer/**`, `model-transfer/python/tests/**`
      Gate: review
      Acceptance: `envs.py` and `backends/hixl/runtime.py` provide `build_hixl_init_options()` and `uses_roce_host_protocol()`, wired into both `HixlSourceBackend.start()` (`source/backend.py`) and `TargetNative._initialize_endpoint()` (`client_native.py`) without altering `RealHixlAPI.create_agent`'s explicit caller-options contract; options preserve `"BufferPool": "0:0"`, forward non-empty `ASCEND_LOCAL_COMM_RES` (`"LocalCommRes"`) and `ASCEND_GLOBAL_RESOURCE_CONFIG` (`"GlobalResourceConfig"`), normalize nested `comm_resource_config.protocol_desc` to HIXL's literal top-level dotted key `"comm_resource_config.protocol_desc"` (with dotted key taking precedence and unrelated keys preserved), and fail-closed resolve directory `local_comm_res_path` entries via lazy ACL `aclrtGetDevice` + `aclrtGetPhyDevIdByUserDevId` lookup (`libacl_rt.so` then `libascendcl.so`) to `<dir>/ub_endpoint_npu_<phy_id>.json` (raising `SourceUnavailableError` on unavailable/failed lookup or missing regular file, never guessing physical `0`); when `uses_roce_host_protocol()` is true, `AclHostOwner` (`source/memory.py`) and `BorrowedAclOwner` (`client_native.py`) allocate owned 2 MiB-aligned private anonymous `mmap` buffers sized from `hixl_allocation_bytes`, retain strong `mmap` references with allocator kind and aligned offset, zero-fill the registration padding tail, expose `memoryview(mapping)[aligned_offset:...]` for `torch.frombuffer`, register `align_up_2m(byte_size)` as `MEM_HOST`, release idempotently by recorded allocator kind after sync/deregister/finalize, and quarantine on `BufferError` or refused release; non-`roce:host` paths keep `aclrtMallocHost`; unit tests cover canonical flat and nested JSON, UB/H2H/UBG/UBoE, physical ID mapping (including visible 0 → physical 6 and multi-TP ranks), fail-closed errors, and real CPU anonymous `mmap` lifetime/offset/GC/quarantine cases.
      Verify: `(cd model-transfer/python && uv run pytest) && make lint python`

- [ ] **T4 · Add opt-in `modelTransfer.ascend.hostRoCE.enabled` to Helm chart and same-image drained rollout to `ModelTransferImageReconciler`**
      Blocked by: None
      Owns: `deploy/gpustack-operator/chart/values.yaml`, `deploy/gpustack-operator/chart/values.schema.json`, `deploy/gpustack-operator/chart/README.md`, `deploy/gpustack-operator/chart/templates/model-transfer/daemonset.yaml`, `deploy/gpustack-operator/chart/ci/test-model-transfer.sh`, `pkg/worker/kuberess/chart_test.go`, `pkg/worker/controllers/worker/model_transfer.go`, `pkg/worker/controllers/worker/model_transfer_test.go`
      Gate: review
      Acceptance: `values.yaml` adds `modelTransfer.ascend.hostRoCE.enabled: false` (regenerated into `values.schema.json` and `chart/README.md` via `make generate chart`); `daemonset.yaml` stamps deterministic `model-transfer.gpustack.ai/config-hash` on source Pod templates; when `false`, `amd`, `ascend`, and `nvidia` DaemonSets render without `hostNetwork`, `/etc/hixlep`, or default `ASCEND_GLOBAL_RESOURCE_CONFIG`; when `true`, only `gpustack-operator-model-transfer-ascend` renders `hostNetwork: true`, `dnsPolicy: ClusterFirstWithHostNet`, read-only `HostPathDirectory` `/etc/hixlep` (`volumeName: ascend-hixlep`) on container `worker`, and default `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep"}'` on container `worker` only when `modelTransfer.env` does not declare `ASCEND_GLOBAL_RESOURCE_CONFIG`; `ModelTransferImageReconciler` compares desired template revision (worker image + `config-hash`) against each owned Pod so same-image `false -> true -> false` toggles drain and replace Pods while preserving exact `worker-instance` + `UID`/`ResourceVersion` fences; `test-model-transfer.sh`, `chart_test.go`, and `model_transfer_test.go` verify enabled/disabled/env-override renders, no-hostNetwork default, and same-image toggle/rollback drain acknowledgements.
      Verify: `make generate chart && CGO_ENABLED=1 go test -race ./pkg/worker/kuberess/... ./pkg/worker/controllers/worker/... -run 'TestChart|TestTransfer' && make lint chart && make lint`

- [ ] **T5 · Admit Ascend `hixl` remote delivery and wire Ascend A5 target `ModelDeployment` Pods**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment.go`, `pkg/worker/controllers/worker/model_deployment_artifact.go`, `pkg/worker/controllers/worker/model_deployment_connector.go`, `pkg/worker/controllers/worker/model_deployment_fabric.go`, `pkg/worker/controllers/worker/model_deployment_render.go`, `pkg/worker/controllers/worker/model_deployment_transfer.go`, `pkg/worker/controllers/worker/model_deployment_*test.go`, `pkg/worker/controllers/worker/testdata/golden-render/**`, `pkg/worker/webhooks/worker/model_deployment.go`, `pkg/worker/webhooks/worker/model_deployment_test.go`
      Gate: review
      Acceptance: `modelDeploymentModelTransferDefaultQualified` admits `(ManufacturerAscend, vLLM/SGLang, "hixl")` alongside `(ManufacturerNVIDIA, vLLM/SGLang, "nixl")` (existing unqualified-gate tests inject explicit unqualified tuples); KV/collective Host RoCE eligibility stays strictly separated from role-local remote weight Host RoCE eligibility so `modelDeploymentAscendHostRoCE` (`vLLM` KV connector) is driven only by the KV leg, while `Encode` and asymmetric P/D pairs continue rejecting `resources.interface > 0`; Ascend A5 (`Family == "950"`) eligible `Server` (including single-Pod `Server` on `vLLM` and `SGLang`) and symmetric P/D roles with `Model.ArtifactRef != nil` resolve `"rdma"` (never `"efa"`) consistently across admission, no-source `Prefer` wait, remote delivery, and per-ordinal fallback (while non-remote SGLang A5 P/D rejection stays intact); `ModelDeploymentArtifactRender` adds `omitempty` `ModelTransferAscendA5` and `ModelTransferAscendHostRoCE` (zeroed on non-A5, A2/A3, and non-Remote renders so unaffected Pod hashes remain byte-for-byte identical and `modelDeploymentRenderVersion` stays `5`); `model_deployment_render.go` performs one unified `/etc/hixlep` overlap-aware yield across user `additionalVolumes`, KV connector mounts, and A5 remote Host RoCE mounts; A5 `remoteDelivery && hixl` appends flat-key `ASCEND_GLOBAL_RESOURCE_CONFIG` (`roce:host` + `/etc/hixlep`) on `interface > 0` or `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` on `interface == 0` via append-if-absent after role/connector env merge; `inject.AscendGlobalResourceConfigEnv` is registered in `modelDeploymentDefaultedEnvNames` with `TestModelDeploymentOwnedAndDefaultedCannotDisagree` and scaler prior-identity tests green; unit, webhook, and golden YAML tests cover UB, Host RoCE (weights-only and KV+weights), SGLang remote vs non-remote, asymmetric P/D, `Encode`, webhook→reconcile fallback, and legacy hash stability.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestModelDeploymentModelTransfer|TestModelTransfer|TestModelDeploymentDirectInterfaceProtocol|TestModelDeploymentGoldenYAML|TestModelDeploymentLegacyRender|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentScaler|TestRenderModelDeploymentPod_AscendA5|TestModelDeploymentWebhook_AscendA5' && make lint`

- [ ] **T6 · Converge KV Cache docs to `DRAM`, document `hixl` admission, and add Ascend A5 scenario matrices**
      Blocked by: T1, T2, T3, T4, T5
      Owns: `docs/modules/kv-cache/**`, `docs/modules/model-delivery/remote.md`, `docs/modules/model-deployment/parameterize.md`, `docs/README.md`
      Gate: None
      Acceptance: `docs/modules/kv-cache/{backend,leader,disk-heavy-nodes,local-disk-tier}.md` state that official `mirrored-mooncake` builds disable `USE_VRAM_SEGMENT` (`Host RAM` only), distinguish older tags from normalized builds, explain `-cpu` RDMA/EFA vs `-cann` HIXL, document `medium: VRAM` as accounting-only for user-built `USE_VRAM_SEGMENT=ON` images (describing both memory-request and device-visibility effects), convert the Ascend worked example to `medium: DRAM`, and add the 6-row Ascend A5 `KVCacheBackend` scenario configuration matrix using flat dotted `"comm_resource_config.protocol_desc"`; `docs/modules/model-delivery/remote.md` updates all HIXL allowlist statements (including line 139) to reflect default Ascend `hixl` admission, documents CANN 9.2+ and updated source/wheel image prerequisites, documents `modelTransfer.ascend.hostRoCE.enabled` (including loopback ports `9250`/`9251`/`9252`, mixed-fleet `nodeSelector`, and same-image drained rollout), documents Python SDK `ASCEND_*` forwarding, fail-closed physical NPU resolution, and `roce:host` `mmap`, and adds the 6-row Ascend A5 `ModelTransfer` scenario configuration matrix; `docs/modules/model-deployment/parameterize.md:105` qualifies that SGLang omits `/etc/hixlep` on KV-only legs; `make lint docs` passes with 0 broken site links and 0 spec/crossref errors.
      Verify: `make lint docs`

### Test Plan

- [ ] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Update `TestMemberDerivedEnvs_CoversEveryNameTheRendererEmits` (`pkg/worker/kvcache/mooncake/member_workload_test.go`) in `T2` to validate both `MemberDerivedEnvs` and `MemberDefaultedEnvs` plus their strict disjointness.
- Update `TestModelDeploymentDirectInterfaceProtocol_AscendA5` (`pkg/worker/controllers/worker/model_deployment_render_test.go:1692`) and implicit Ascend-as-unqualified test fixtures in `pkg/worker/controllers/worker/model_deployment_transfer_test.go` (`:1058`, `:1102`, `:1887`, `:2247`) in `T5` to reflect Ascend `hixl` default admission and A5 remote Host RoCE eligibility while preserving explicit unqualified-tuple gate tests.

#### Unit tests

- `pack/mirrored-mooncake` & `.auto-sync`: `2026-11-20` — `bash .auto-sync/checks/selftest_kv_transfer.sh` covering valid `builder-cuda` `USE_VRAM_SEGMENT=OFF`, comment-only `OFF`, `ON` alongside `OFF`, pre-option releases, and 5th `context_manager.cpp` guard.
- `pkg/worker/kvcache/mooncake`: `2026-11-20` — `CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/mooncake/...` covering `CANN` with `nil`/`1`/`2` `fabricInterfaceCount`, inherited vs group-override protocol, `/etc/hixlep` overlap yield (exact/ancestor/descendant/sibling), `extraEnv` override (including empty string and `valueFrom`), `MemberDefaultedEnvs` vs `MemberDerivedEnvs` disjointness, capability union, and deterministic `MemberPodSpecHash` stability for unaffected groups.
- `pkg/worker/webhooks/worker`: `2026-11-20` — `CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/...` covering `KVCacheBackend` `CANN` idle-count warning suppression, `localDisks` vs `/etc/hixlep` overlap refusal (create and protocol transition), and `ModelDeployment` Ascend A5 create/update admission for remote `Server`, symmetric/asymmetric P/D, `Encode`, SGLang remote vs non-remote, and `aws-efa-enabled=true`.
- `model-transfer/python`: `2026-11-20` — `(cd model-transfer/python && uv run pytest)` covering flat dotted and nested JSON normalization/precedence, `LocalCommRes` vs `local_comm_res_path` precedence, UB/H2H/UBG/UBoE option strings, visible-to-physical NPU ID mapping (visible 0 → physical 6, multi-TP ranks, missing symbol/error/missing file fail-closed), and CPU real-anonymous-`mmap` alignment, zero-tail, nonzero-offset `torch.frombuffer` copy, GC safety, idempotent release by recorded kind, and quarantine on `BufferError` / refused release.
- `pkg/worker/controllers/worker` & `pkg/worker/kuberess`: `2026-11-20` — `CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/kuberess/...` covering `ModelTransferImageReconciler` same-image `false -> true -> false` drained rollout and stale-ack fencing, default `(Ascend, vLLM/SGLang, hixl)` qualification, KV-only vs weights-only vs KV+weights A5 Pod renders, unified `/etc/hixlep` yield, webhook→reconcile `Prefer` no-source/fallback/recovery parity, scaler prior-identity env gate, and Helm chart `hostRoCE.enabled` toggles.

#### Integration tests

- `ModelDeployment` golden YAML suite (`TestModelDeploymentGoldenYAML` and `TestModelDeploymentLegacyRender`): verify zero diff across all existing legacy fixtures and golden scenarios, plus new golden coverage for Ascend A5 `remoteDelivery && hixl` (UB and Host RoCE).
- Helm offline render suite (`deploy/gpustack-operator/chart/ci/test-model-transfer.sh` via `make lint chart`): verify per-vendor DaemonSet isolation, default no-`hostNetwork` invariant, opt-in Ascend `hostRoCE.enabled` (`hostNetwork`, `dnsPolicy`, `/etc/hixlep` mount, non-duplicating `ASCEND_GLOBAL_RESOURCE_CONFIG`, `config-hash`), and `values.schema.json` validation.
- Docs and spec integrity suite (`make lint docs`): verify all 80+ docs pages, `check-specs` across all specs and `go test -run` patterns, `check-crossrefs`, and Hugo site build with 0 broken internal links.

#### e2e tests

- Local workstations and repository CI do not have Ascend A5 (`950PR`/`950DT`) hardware, Host RoCE NICs, or A5 UB fabrics. Control-plane rendering, admission, drain-rollout, `.auto-sync` gates, and Python SDK option/memory contracts are verified offline through the unit and integration suites above (with fake ACL/HIXL bindings and real CPU anonymous `mmap`).
- End-to-end data-plane validation (rebuilt `mirrored-mooncake` `0.3.14+` and `model-transfer` images transferring real KV cache blocks and safetensors checkpoints over A5 UB, H2H `ub_ctp:host`, and Host RoCE across multi-NPU nodes) is deferred to hardware acceptance on an Ascend A5 cluster after image publication.
