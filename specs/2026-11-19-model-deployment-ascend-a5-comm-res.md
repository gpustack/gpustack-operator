# Spec: Ascend A5 Conditional Communication Resource Configuration and Read-Only Host Root Utility

Status: Building
Blocked on: implementation and verification of T1–T6 on this branch before opening a pull request
Type: Feature

- **Related Specs**: [2026-11-10-model-deployment-kvtransfer-nixl-hixl.md](2026-11-10-model-deployment-kvtransfer-nixl-hixl.md) · [2026-11-11-model-deployment-kvtransfer-peer-backends.md](2026-11-11-model-deployment-kvtransfer-peer-backends.md)

## Summary

Add Ascend A5 (`Family == "950"`) aware communication resource configuration to `ModelDeployment` and `device-manager` without adding new CRD fields or cluster `Setting`s.
For `vLLM` workloads on Ascend A5, derive Host RoCE versus on-die UB/UBG/UBoE intent from `roles[].resources.interface`: omit `/etc/hixlep` and default `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` when `interface == 0`, and mount `/etc/hixlep` read-only with outer `kv_connector_extra_config.ascend_local_comm_res_path` when `interface > 0`.
For `SGLang` disaggregated workloads on Ascend A5, default `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` while preserving `sdma` on Ascend A2/A3.
Mount the host root (`/`) read-only at `/host` on every `device-manager` DaemonSet (controlled by `deviceManager.hostRootMount.enabled`, default `true`) and provide a shared, strictly read-only host-root filesystem utility (`pkg/utils/hostfs`) so vendor plugins inspect host paths natively without privilege escalation or path escape.
Use that utility in the Ascend A5 `Allocate` path to mount `/etc/hccl_rootinfo.json` (only when it is a valid version `2.0` regular file) and `/lib/route.conf` (only when it is a regular file) read-only into allocated containers.

## Motivation

`gpustack-operator` already classifies Ascend A5 accelerators (`Ascend950PR`, `Ascend950DT`, `Ascend910_95*`) under `Family == "950"` (`pkg/devicemanager/product/ascend/product.go`) and propagates that value onto `AcceleratorType.Status.Detail.Family`.
However, `ModelDeployment` currently reads only `Status.Detail.Manufacturer`, and `device-manager` lacks a read-only host-root filesystem mount.
This leaves four operational gaps on Ascend A5:

1. **Missing A5 `LocalCommRes` and Host RoCE `/etc/hixlep` selection in `vLLM`**:
   - In upstream `vllm-ascend` (`1c9b0ab01`), `setup_ascend_local_comm_res` runs only on A5 (`HardwareCapability.LOCAL_KV_COMM_RESOURCE`). It reads `<ascend_local_comm_res_path>/ub_endpoint_npu_<physical_id>.json` from the outer `kv_transfer_config.kv_connector_extra_config` and overwrites `ASCEND_LOCAL_COMM_RES`.
   - If `ascend_local_comm_res_path` points to a missing or empty `/etc/hixlep` directory, worker startup crashes immediately with `FileNotFoundError` and no fallback.
   - Under A5 UB (`ub_ctp`), UBG (`ub_rtp:device`), and UBoE (`uboe:device`), `cann/hixl` (`cd4c5e86`) auto-generates endpoints in memory via DCMI, DSMI, and `/usr/local/Ascend/driver/topo/950/` when `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` is set. Mounting stale `/etc/hixlep` files on UB/UBoE hosts overwrites auto-generation and selects wrong endpoints (`MindIE-Motor` `01b30230` drops `/etc/hixlep` on UB/UBoE).
   - Under A5 Host RoCE (`roce:host`), released CANN 9.1/9.2 stacks do not yet support `roce:host` auto-generation. Workloads must request host RDMA interfaces (`device.gpustack.ai/rdma*`), mount `/etc/hixlep` read-only, and set outer `ascend_local_comm_res_path: "/etc/hixlep"` (including on the outer `MultiConnector` when P/D pairs with a shared `KVCache` pool).
   - `ModelDeploymentDirectInterfaceProtocol` (`pkg/worker/controllers/worker/model_deployment_fabric.go`) currently returns `""` for all Ascend roles, blocking `roles[].resources.interface > 0` for A5 Host RoCE across routed P/D pairs, multi-Pod `Server` roles, and single-Pod `Server` + `CANN` `KVCache` roles.
2. **Wrong `SGLang` MemFabric transfer protocol default on A5**:
   - `pkg/worker/kvcache/inject/sglang.go` defaults `ASCEND_MF_TRANSFER_PROTOCOL=sdma` for all Ascend pools.
   - In upstream `sglang` (`9304abb532`), `sdma` targets A3 HCCS, while Ascend A5 (`950PR`/`950DT`) requires `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` and never reads `/etc/hixlep`.
3. **Unsafe or missing `/etc/hccl_rootinfo.json` and `/lib/route.conf` handling across MindCluster versions**:
   - Upstream `vllm-ascend`, `Mooncake` (`ascend_direct`), and `cann/hixl` never read `/etc/hccl_rootinfo.json` or `/lib/route.conf` on CANN >= 9.2. Healthy A5 nodes often carry neither file (`pkg/devicemanager/allocator/ascend/rank_table.go`).
   - MindCluster 26.2.0 (`mind-cluster` commit `9ce494e17`) removed `ascend-docker-runtime`'s default mount of `/etc/hccl_rootinfo.json` (MindCluster 26.0.x mounted it on presence). Neither version mounts `/lib/route.conf`.
   - Workloads backed by `memfabric_hybrid` (`SGLang` A5 `device_urma`) or legacy CANN 9.1 multi-node UB HCCL still need `/etc/hccl_rootinfo.json` (version `2.0`) and `/lib/route.conf` when present on the host.
   - Placing `HostPathFile` in a `ModelDeployment` Pod spec fails kubelet volume setup on healthy A5 nodes where the file is absent. Using `HostPathFileOrCreate` creates an empty file that triggers HCCL `Config_Error_Ranktable (EI0014)`.
4. **No shared read-only host-root access layer in `device-manager`**:
   - Today `device-manager` DaemonSet (`deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml`) mounts only vendor-specific driver directories and never mounts host `/`.
   - Future vendor plugins that inspect host files (such as `/etc/*` or `/lib/*`) would otherwise duplicate ad-hoc host mounts or risk resolving paths against the container's own filesystem.

### Goals

- Target users: cluster administrators operating Ascend A5 (`950PR`/`950DT`) or mixed Ascend A2/A3/A5 clusters, workload owners deploying `ModelDeployment` on Ascend A5, and operator contributors building vendor `device-manager` plugins.
- Keep `ModelDeployment` CRD schemas and cluster `Setting` catalogs unchanged (`api/worker/v1`, `api/worker/v1alpha1`, and `pkg/worker/settings` add no new fields or settings; `instance-host-path-volume-allowed` stays `false` by default).
- Scope all A5 `ModelDeployment` behaviors strictly to `AcceleratorType.Status.Detail.Family == "950"`, preserving byte-for-byte identical rendering and `pod-spec-hash` digests for all Ascend A2 (`910B`), A3 (`910C`), and non-Ascend pools (`modelDeploymentRenderVersion` stays `5`).
- Enable `ModelDeployment` on Ascend A5 to:
  - Auto-configure `vLLM` UB/UBG/UBoE (`interface == 0`) on P/D and `KVCache` roles (excluding `RoleEncode`) with defaulted `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` and no `/etc/hixlep` mount or `ascend_local_comm_res_path`.
  - Auto-configure `vLLM` Host RoCE (`interface > 0`) by granting `device.gpustack.ai/rdma*` (never AWS EFA), mounting `/etc/hixlep` read-only (`HostPathDirectory`, yielding cleanly if a user `additionalVolume` already mounts `/etc/hixlep`), and setting `"ascend_local_comm_res_path": "/etc/hixlep"` on the outer `kv_connector_extra_config` (including outer `MultiConnector`).
  - Default `SGLang` A5 P/D (`HIXL`) to `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` without mounting `/etc/hixlep`.
- Mount host `/` read-only at `/host` on the `main` container of every `device-manager` DaemonSet when `deviceManager.hostRootMount.enabled` is `true` (default), and provide a strictly read-only host-root utility (`pkg/utils/hostfs`) that accepts native host paths (for example `"/etc/hccl_rootinfo.json"` and `"/lib/route.conf"`) with strict containment, merged-usr intermediate directory symlink support, and fail-closed validation.
- Conditionally mount `/etc/hccl_rootinfo.json` (valid JSON `version == "2.0"` regular file) and `/lib/route.conf` (regular file) read-only at kubelet `Allocate` time on Ascend A5 (`Family == "950"`).
- Document all Ascend A5 interconnect topologies (UB, UBG, UBoE, Host RoCE, Aggregated `Server`, and constrained static-UB fallback), `deviceManager.hostRootMount.enabled`, and conditional A5 mounts in `docs/`.

### Non-Goals

- Adding new CRD fields to `ModelDeployment.spec.kvTransfer`, adding new cluster `Setting`s or `GPUSTACK_*` environment knobs for A5 communication resources, or changing `instance-host-path-volume-allowed` from its `false` default.
- Extending `PodKVCacheWebhook` (`pkg/worker/webhooks/worker/pod_kv_cache_inject.go`) for standalone annotated Pods (standalone Pod injection has no `AcceleratorType` lookup and remains out of scope).
- Managing or generating `/etc/hixlep/ub_endpoint_npu_*.json`, `/etc/hccl_rootinfo.json`, or `/lib/route.conf` on host nodes (node provisioning remains the administrator's responsibility via `mindcluster-tools` or HIXL tooling).
- Supporting `Memcache` (`AscendStore` memcache backend) as a managed `KVCacheBackend` in this spec (`KVCache` remains Mooncake-backed).
- Injecting `ASCEND_LOCAL_COMM_RES_PATH` (unused by `vllm-ascend`, `sglang`, `Mooncake`, and `cann/hixl`), `HCCL_INTRA_ROCE_ENABLE=1`, `protocol_desc: ["roce:device"]`, or `ASCEND_ENABLE_USE_FABRIC_MEM=1` on Ascend A5.
- Supporting automatic `/etc/hixlep` injection for `SGLang` (SGLang has no `/etc/hixlep` loader), `RoleEncode`, or Aggregated `Server` roles with no KV connector.

## Proposal

Use `AcceleratorType.Status.Detail.Family == "950"` (`nodefeature.AcceleratorFamilyAscend950`) to separate Ascend A5 from earlier Ascend generations at Pod render and admission time, and use `roles[].resources.interface > 0` to distinguish A5 Host RoCE from on-die UB/UBG/UBoE.
Separate pool-level engine configuration (rendered in `ModelDeploymentReconciler` before scheduling) from node-level host file presence (verified in `device-manager` at kubelet `Allocate` time through a universal, strictly read-only `/host` root utility).

### User Stories

#### Story 1
As a user deploying `ModelDeployment` on Ascend A5 (`Family == "950"`) with UB, UBG, or UBoE interconnect, I want the operator to omit `/etc/hixlep` and `/etc/hccl_rootinfo.json` from the Pod spec and default `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` for `vLLM` KV transfer (and `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` for `SGLang`), so that Pods start cleanly on standard A5 nodes without static host files or `FileNotFoundError` / `EI0014` failures.

#### Story 2
As a user deploying disaggregated `vLLM` or a `KVCache`-backed `vLLM` role on Ascend A5 over Host RoCE (`roce:host`), I want setting `roles[].resources.interface > 0` to grant `device.gpustack.ai/rdma*`, mount `/etc/hixlep` read-only, and inject `"ascend_local_comm_res_path": "/etc/hixlep"` on the outer `kv_connector_extra_config` (including outer `MultiConnector`), so that each worker loads its matching `ub_endpoint_npu_<physical_id>.json` without breaking A2/A3 or A5 UB pools.

#### Story 3
As a user or cluster administrator running Ascend A5 workloads that need `/etc/hccl_rootinfo.json` or `/lib/route.conf` (such as `SGLang` MemFabric `device_urma` or legacy CANN 9.1 multi-node UB HCCL on MindCluster >= 26.2), I want `device-manager` to mount those files read-only at `Allocate` time only when they safely exist on the node (requiring valid version `2.0` JSON for `/etc/hccl_rootinfo.json`), so that compatible workloads succeed without triggering `EI0014` or kubelet mount failures on nodes where those files are absent.

#### Story 4
As a cluster administrator operating mixed Ascend A2 (`910B`), A3 (`910C`), and A5 (`950`) pools, I want all A5-specific `/etc/hixlep`, `ASCEND_LOCAL_COMM_RES`, `device_urma`, `/etc/hccl_rootinfo.json`, and `/lib/route.conf` rules to stay strictly scoped to `Family == "950"`, so that existing A2/A3 and non-Ascend renders and `pod-spec-hash` digests remain byte-for-byte unchanged across upgrades.

#### Story 5
As an operator contributor or cluster administrator running `device-manager`, I want a shared, strictly read-only host-root utility over `/host` mounted across `device-manager` DaemonSets (with a Helm toggle `deviceManager.hostRootMount.enabled` for policy-restricted clusters), so that any manufacturer plugin can inspect native host paths without manually prefixing `"/host"`, escaping `/host`, or writing to the host.

### Core Features & Acceptance Criteria

1. **Read-Only Host-Root Mount on `device-manager` DaemonSet (`H5-A`)**:
   - `deploy/gpustack-operator/chart/values.yaml` adds `deviceManager.hostRootMount.enabled: true` (default `true`), and `make generate chart` updates `deploy/gpustack-operator/chart/values.schema.json` and `deploy/gpustack-operator/chart/README.md`.
   - When `deviceManager.hostRootMount.enabled` is `true`, `deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml` mounts host `/` (`hostPath.path: /`, `hostPath.type: Directory`) at `/host` with `readOnly: true` on the `main` container only (never on `initContainers[stage-libs]`) for every manufacturer DaemonSet (volume name `host-root-dir`).
   - When `deviceManager.hostRootMount.enabled` is `false`, neither the `host-root-dir` volume nor its `/host` mount is rendered, giving policy-hardened clusters (OPA Gatekeeper / Kyverno forbidding `hostPath: /`) and MindCluster `26.0.x` troubleshooting a clean escape switch.

2. **Shared Read-Only Host-Root Filesystem Utility (`pkg/utils/hostfs`)**:
   - Provide `pkg/utils/hostfs` (`DefaultHostRoot = "/host"`, `ReadOnlyHostFS`) backed by Go's `os.Root` (`os.OpenRoot`) with an explicit `Close() error` lifecycle.
   - Callers pass native host paths (for example `"/etc/hccl_rootinfo.json"` or `"/lib/route.conf"`). `normalizeHostPath` maps `"/" -> "."` and explicitly rejects `""` and any `".."` path segment (never silently folding `".."` with `path.Clean`).
   - Root validation enforces that `etc`, `proc`, and `usr` exist and are directories (`info.IsDir()`); an unmounted, empty, or invalid root returns an explicit error instead of silently reporting files as absent.
   - Merged-usr intermediate symlink resolution: resolves intermediate directory symlinks (both relative `lib -> usr/lib` and absolute `lib -> /usr/lib` on Ubuntu/openEuler) strictly inside the host root with a 40-hop loop cap, while still rejecting leaf symlinks on `Lstat` and `ReadRegularFile`.
   - Strict read-only security boundary:
     - Exposes only read-only operations (`Close`, `Open`, `ReadFile`, `ReadRegularFile(hostPath, maxBytes)`, `Stat`, `Lstat`, `ReadDir`, `Readlink`, `Exists`).
     - Exposes zero write, create, append, truncate, chmod, chown, remove, rename, symlink, or command-execution methods.
     - `ReadRegularFile(hostPath, maxBytes)` performs `Lstat` (must be regular non-symlink), opens `O_RDONLY`, runs `f.Stat()` + `os.SameFile` TOCTOU verification, enforces `maxBytes`, and reads via `io.LimitReader`.

3. **Ascend A5 `Allocate`-Time Conditional Injection of `/etc/hccl_rootinfo.json` and `/lib/route.conf`**:
   - Thread `HostRoot` (`--host-root`, default `hostfs.DefaultHostRoot`) through `device.AllocatorOptions`, `pkg/devicemanager/{option,config}.go`, `pkg/devicemanager/allocator/{option,config,allocator}.go`, and `PreflightResponder`, closing any opened `ReadOnlyHostFS` in `aggregated.Stop()`.
   - Extract pure `validateRankTableVersion950(content []byte) error` in `pkg/devicemanager/allocator/ascend/rank_table.go` so `checkRankTable` and `deviceplugin.go` share one exact version-`2.0` validator while `NewPreflighter` remains lazy (keeping `darwin` unit tests without `/host` green).
   - In `pkg/devicemanager/allocator/ascend/deviceplugin.go`, when any allocated accelerator has `Group.Family == nodefeature.AcceleratorFamilyAscend950`:
     - Inspect `"/etc/hccl_rootinfo.json"` via `ReadRegularFile` (`1 MiB` cap) and `validateRankTableVersion950`. Append `&deviceplugin.Mount{HostPath: "/etc/hccl_rootinfo.json", ContainerPath: "/etc/hccl_rootinfo.json", ReadOnly: true}` (never using `deviceplugin.NewROMount`) at most once per container **only** when valid.
     - Inspect `"/lib/route.conf"` via `Lstat` (`Mode().IsRegular()`). Append `&deviceplugin.Mount{HostPath: "/lib/route.conf", ContainerPath: "/lib/route.conf", ReadOnly: true}` at most once per container **only** when it is a regular non-symlink file.
     - Skip emitting either mount when `HostRoot` is unavailable/disabled, the file is absent/symlink/non-regular/invalid, `s.hostFS` is nil (zero-value `&server{}` in unit tests), or `ctr != nil` already declares a `VolumeMount` with the same cleaned `MountPath`. Log diagnostics without failing container allocation.
   - For non-A5 Ascend generations (`910`, `910B`, `910C`, `310P`, `310B`), emit neither mount.

4. **Ascend A5 Signal-Driven `/etc/hixlep` and `ASCEND_LOCAL_COMM_RES` in `ModelDeployment` (`vLLM`)**:
   - Define `nodefeature.AcceleratorFamilyAscend950 = "950"` in `pkg/nodefeature/knowns.go` and reuse it in `pkg/devicemanager/product/ascend/product.go`.
   - Thread `AcceleratorType.Status.Detail.Family` from `ModelDeploymentReconciler`, `ModelDeploymentWebhook`, and scaler prior-identity gates into fabric resolution and `inject.Input`.
   - Scope all A5 `vLLM` rules strictly to `EngineVLLMAscend` + `Family == "950"` on managed (`len(role.Command) == 0`), non-`Encode` roles:
     - **A5 UB / UBG / UBoE (`interface == 0`)**:
       - On disaggregated P/D (`HIXL` or `Mooncake`) or non-`Encode` `KVCache`-backed `vLLM` roles: inject defaulted `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` (`DefaultedEnv`, user-overridable via `roles[].env`).
       - Emit neither `/etc/hixlep` mount nor `ascend_local_comm_res_path`.
       - Users who want `UBG` (`ub_rtp:device`) or `UBoE` (`uboe:device`) set `ASCEND_GLOBAL_RESOURCE_CONFIG` in `roles[].env` without being blocked by owned-env validation.
     - **A5 Host RoCE (`interface > 0`)**:
       - Add a dedicated Ascend A5 `vLLM` branch in `ModelDeploymentDirectInterfaceProtocol` (`manufacturer == ManufacturerAscend && family == "950" && len(role.Command) == 0`) that always returns `"rdma"` (never `"efa"`, even when `awsEFAEnabled == true`) for:
         1. Routed `vLLM` prefill/decode pairs when **every** `Prefill` and `Decode` role requests `resources.interface > 0` (`ModelDeploymentDirectPairRequestsInterfaces`), evaluated ahead of the `KVTransferBackendHIXL` early return. Asymmetric P/D interface requests are rejected even when `md.Spec.KVCache != nil`.
         2. Managed multi-Pod or `elasticEp` `vLLM` `Server` roles (`size > 1` or `elasticEp != nil`) requesting `resources.interface > 0` (RDMA grant only; no `/etc/hixlep` unless `md.Spec.KVCache != nil`).
         3. Managed non-P/D, non-`Encode` `KVCache`-backed `vLLM` roles (`md.Spec.KVCache != nil`) requesting `resources.interface > 0` (enabling single-Pod `Server` + `CANN` `KVCache` over Host RoCE).
       - Update `modelDeploymentRoleNeedsInterfaceRejudge` (`pkg/worker/webhooks/worker/model_deployment.go`) to probe both `(ManufacturerNVIDIA, "")` and `(ManufacturerAscend, "950")` so `ValidateUpdate` rejudges A5 `HIXL` roles when a sibling drops `interface` or `spec.router` is removed.
       - Whenever an A5 `vLLM` role renders a KV transfer or `KVCache` connector (never `RoleEncode`) with Host RoCE active (`AscendHostRoCE == true`):
         - Mount `/etc/hixlep` read-only (`volumeName: gpustack-ascend-hixlep`, `hostPath.path: /etc/hixlep`, `hostPath.type: Directory`, `readOnly: true`), yielding `gpustack-ascend-hixlep` cleanly when a user `additionalVolume` already mounts `path.Clean(av.MountPath) == "/etc/hixlep"`.
         - Set `"ascend_local_comm_res_path": "/etc/hixlep"` on the **outer** `kv_connector_extra_config` of `--kv-transfer-config` — whether the outer connector is standalone `MooncakeConnectorV1`, standalone `AscendStoreConnector`, or `MultiConnector` (never nested inside a child connector of `MultiConnector`).
         - Omit defaulted `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'`.
   - Register `ASCEND_LOCAL_COMM_RES` in `modelDeploymentDefaultedEnvNames` (`pkg/worker/controllers/worker/model_deployment_connector.go`) and `modelDeploymentScalerCapacityNeutralEnvNames` (`pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go`), and pass `family` through `modelDeploymentScalerOperatorEnvGateWithManufacturer` and `TestModelDeploymentOwnedAndDefaultedCannotDisagree`.
   - On Ascend A2/A3 (`Family != "950"`), keep `ModelDeploymentDirectInterfaceProtocol`, `ASCEND_LOCAL_COMM_RES`, and `/etc/hixlep` completely unchanged (byte-for-byte legacy parity).

5. **Ascend A5 `SGLang` P/D Transfer Protocol Default**:
   - In `pkg/worker/kvcache/inject/sglang.go`, when `KVTransferBackendHIXL` renders for Ascend A5 (`Family == "950"`), default `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` (`DefaultedEnv`, user-overridable via `roles[].env`).
   - Keep `ASCEND_MF_TRANSFER_PROTOCOL=sdma` for Ascend A2/A3 (`Family != "950"`).
   - Never mount `/etc/hixlep` or inject `ascend_local_comm_res_path` or `ASCEND_LOCAL_COMM_RES` on `SGLang` roles.
   - Keep `SGLang` A5 P/D `resources.interface > 0` rejected unless a bound `KVCache` pool itself uses RDMA.

6. **Zero-Rollout Pod Binding Hash and Golden YAML Coverage**:
   - Do **not** add raw `Family` to `modelDeploymentAcceleratorTypeHashInput` (live NVIDIA, AMD, A2, and A3 `AcceleratorType` objects carry non-empty `Family`, so hashing raw `Family` there would roll every existing accelerated Pod on operator upgrade).
   - Instead, record A5-effective connector state (`AscendLocalCommRes` / `AscendTransferProtocol` with `omitempty`, non-empty only when `Manufacturer == "ascend" && Family == "950"` actually alters connector output) inside `modelDeploymentConnectorBinding` (`pkg/worker/controllers/worker/model_deployment_connector.go`), keeping `modelDeploymentRenderVersion = 5`.
   - Add an `atlas-950` fixture (`Family: "950"`) and 4 A5 golden YAML scenarios (`len == 37`: A5 UB `vLLM` P/D, A5 Host RoCE `vLLM` P/D + `MultiConnector`, A5 Host RoCE `vLLM` single-Pod `Server` + `KVCache`, and A5 `SGLang` P/D `device_urma`), verifying zero diff on all 33 existing golden scenarios and all 7 legacy fixtures.

7. **Documentation of All Ascend A5 Interconnect Topologies and `hostRootMount`**:
   - Update `docs/modules/model-deployment/parameterize.md`, `docs/modules/model-deployment/prefill-decode.md`, `docs/modules/model-deployment/configuration.md`, and `docs/getting-started/vendor-prerequisites.md` (and `docs/README.md` only if navigation/index routing changes) to document:
     - Ascend A5 on-die UB/URMA (`fabric.kind=ub`, `resources.interface: 0`) versus Linux Host RDMA/RoCE (`device.gpustack.ai/rdma*`, `resources.interface > 0`).
     - Complete configuration guidance for all six A5 scenarios:
       1. UB (`ub_ctp` intra-SuperPod / single-node)
       2. UBG (`ub_rtp:device` cross-SuperPod via user `ASCEND_GLOBAL_RESOURCE_CONFIG`)
       3. UBoE (`uboe:device` via user `ASCEND_GLOBAL_RESOURCE_CONFIG`)
       4. Host RoCE (`roce:host` via `roles[].resources.interface > 0` + host `/etc/hixlep/ub_endpoint_npu_<phy_id>.json`)
       5. Aggregated `Server` (single-node / multi-node without KV connector)
       6. Constrained static-UB fallback (`roles[].command` take-over + administrator `instance-host-path-volume-allowed` when an unprivileged UB environment requires static `/etc/hixlep` files without Host RoCE NICs)
     - `deviceManager.hostRootMount.enabled` (default `true`, layered read-only boundary, and when to set `false` on OPA/Kyverno-restricted or custom MindCluster `26.0.x` nodes) plus conditional A5 `/etc/hccl_rootinfo.json` (version `2.0`) and `/lib/route.conf` injection across MindCluster `26.0.x` and `26.2+`.

### Notes / Constraints / Caveats

- **Upstream evidence distilled (local verification without A5 hardware)**:
  - `vllm-ascend` (`1c9b0ab01`): `NPUWorker.init_device()` calls `setup_ascend_local_comm_res()` only on A5 (`AscendDeviceType.A5` / `SOC_VERSION` `ascend950` / `260`). It reads outer `kv_transfer_config.kv_connector_extra_config["ascend_local_comm_res_path"]`, maps logical-to-physical NPU ID via `aclrtGetPhyDevIdByUserDevId`, reads `ub_endpoint_npu_<physical_id>.json`, and overwrites `os.environ["ASCEND_LOCAL_COMM_RES"]`. Missing files raise `FileNotFoundError` with no fallback. Putting `ascend_local_comm_res_path` inside a child connector of `MultiConnector` is silently ignored because `MultiConnector` constructs child configs after `init_device()`.
  - `cann/hixl` (`cd4c5e86`): `OPTION_LOCAL_COMM_RES` takes precedence over `GlobalResourceConfig.local_comm_res_path`. Passing `{"version":"1.3"}` triggers `EndpointGeneratorV2::AutoGen` on A5, reading DCMI, DSMI, and `/usr/local/Ascend/driver/topo/950/`. Neither `/etc/hixlep`, `/etc/hccl_rootinfo.json`, nor `/lib/route.conf` is read by HIXL C++ auto-gen. Setting `HCCL_INTRA_ROCE_ENABLE=1` or `protocol_desc: ["roce:device"]` on A5 produces an empty `endpoint_list` (`PARAM_INVALID`).
  - `mind-cluster` (`9ce494e17`, MindCluster 26.2.0): Removed default mounting of `/etc/hccl_rootinfo.json` from `ascend-docker-runtime` (`addUBMount` and `legacy_config.go`). Identical duplicate `(src, dst)` mounts between `ascend-docker-runtime` (`26.0.x`) and kubelet device-plugin `AllocateResponse.Mounts` deduplicate in `mind-cluster`; if a customized `26.0.x` runtime ever rejects duplicate mounts in the field, setting `deviceManager.hostRootMount.enabled=false` cleanly disables A5 `Allocate` host file injection.
  - `sglang` (`9304abb532`): Uses `memfabric_hybrid` for `--disaggregation-transfer-backend ascend` with `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` on A5 (`950PR`/`950DT`), has zero `/etc/hixlep` or `ASCEND_LOCAL_COMM_RES` support, and transitively relies on `/etc/hccl_rootinfo.json` and `/lib/route.conf` in non-privileged A5 containers.
- **Unconverged `AcceleratorType.Status.Detail.Family` at admission**: Consistent with `validateModelDeploymentInterfaceRequests` (`pkg/worker/webhooks/worker/model_deployment.go`), requesting `resources.interface > 0` for A5 Host RoCE requires a converged `AcceleratorType` reporting `Family == "950"`; `Manufacturer == "ascend" && Family == ""` continues to evaluate as non-A5 so legacy A2/A3 behavior and unit fixtures stay intact.
- **Homogeneous `CANN` `KVCacheBackend` requirement**: On Ascend A5 Host RoCE, `KVCache`-backed `vLLM` roles continue to attach to a `KVCache` pool whose `KVCacheBackend` uses homogeneous `CANN` (`"ascend"`) transport across member groups (`validateModelDeploymentKVCacheFabric`).
- **Layered read-only boundary on `/host`**: In Kubernetes, `volumeMounts[].readOnly: true` without `recursiveReadOnly` marks the top-level `/host` bind mount read-only in the mount table while submounts remain governed by process access. Strict read-only enforcement is therefore dual-layered: `readOnly: true` on `main` container's `/host` mount plus `pkg/utils/hostfs.ReadOnlyHostFS` (`os.OpenRoot`, `O_RDONLY`, containment of relative/absolute intermediate directory symlinks, leaf-symlink rejection via `Lstat`, and zero write/exec API surface).
- **Host `/etc/hixlep` permission boundary**: Operator-rendered `gpustack-ascend-hixlep` (`HostPathDirectory`, `readOnly: true`) is fixed to `/etc/hixlep` by the controller (matching operator-owned compilation cache mounts), so Host RoCE users never need cluster-wide `instance-host-path-volume-allowed=true`.

### Boundaries

- **Always:**
  - Keep the `/host` mount on `device-manager` (`main` container only) `readOnly: true`, gated by `deviceManager.hostRootMount.enabled` (default `true`), and expose only read-only methods on `pkg/utils/hostfs`.
  - Keep all A5 rules gated on `Family == "950"` and fingerprint A5 state only in `modelDeploymentConnectorBinding` (`omitempty`) so non-A5 and A2/A3 renders and `pod-spec-hash` digests remain byte-for-byte unchanged.
  - Place `ascend_local_comm_res_path` on the **outer** `kv_connector_extra_config` (including outer `MultiConnector`, never inner child connectors) and never touch `RoleEncode`.
  - Keep `TestModelDeploymentOwnedAndDefaultedCannotDisagree`, scaler prior-identity tests, legacy render fixtures, and golden YAML fixtures green.
  - Run `make lint`, `make lint chart`, and `make lint docs` before completion.
- **Ask first:**
  - Adding any new CRD field under `api/worker/v1alpha1/` or `api/worker/v1/` or adding any new cluster `Setting`.
  - Allowing write operations or command execution through `pkg/utils/hostfs` (preflight's existing `hostExec` stays isolated inside `pkg/devicemanager/preflight`).
  - Modifying `pack/model-transfer/Dockerfile` or `PodKVCacheWebhook`.
- **Never:**
  - Use `HostPathFileOrCreate` or `HostPathDirectoryOrCreate` for `/etc/hccl_rootinfo.json`, `/lib/route.conf`, or `/etc/hixlep`.
  - Mount `/etc/hccl_rootinfo.json` without verifying regular non-symlink file status, `1 MiB` size cap, and JSON `version == "2.0"`.
  - Use `deviceplugin.NewROMount` for `/etc/hccl_rootinfo.json` or `/lib/route.conf` (which rewrites container paths when `HostPath` differs).
  - Add raw `Family` to `modelDeploymentAcceleratorTypeHashInput`.
  - Inject `ASCEND_LOCAL_COMM_RES_PATH`, `HCCL_INTRA_ROCE_ENABLE=1`, `protocol_desc: ["roce:device"]`, or `ASCEND_ENABLE_USE_FABRIC_MEM=1` on Ascend A5.
  - Mount `/etc/hixlep` on A5 UB/UBG/UBoE (`interface == 0`), on `SGLang`, on `RoleEncode`, or on Aggregated `Server` roles without a KV connector.
  - Reference uncommitted or off-repo local paths in committed specs, code comments, or docs.

### Risks and Mitigations

- **Fleet-wide Pod rollout on upgrade if raw `Family` enters `modelDeploymentAcceleratorTypeHashInput`** → Leave `modelDeploymentAcceleratorTypeHashInput` untouched; record A5-effective connector fields (`omitempty`) inside `modelDeploymentConnectorBinding` only when `Manufacturer == "ascend" && Family == "950"`, verified by an explicit non-A5/A2/A3 hash-stability unit test.
- **Policy-hardened clusters (OPA Gatekeeper / Kyverno) reject `hostPath: /` on `device-manager`, or a customized MindCluster `26.0.x` runtime rejects duplicate `/etc/hccl_rootinfo.json` mounts** → Provide `deviceManager.hostRootMount.enabled` (default `true`, `main` container only); setting `false` omits the `/host` volume and mount cleanly while `device-manager` skips A5 `Allocate` file mounts without failing allocations.
- **Merged-usr Linux distributions (`/lib -> /usr/lib` or `/lib -> usr/lib`) cause Go `os.Root` to reject `/lib/route.conf`** → Resolve intermediate directory symlinks (both relative and absolute) safely inside the host root with a 40-hop loop cap in `pkg/utils/hostfs`, while still rejecting leaf symlinks via `Lstat`.
- **Asymmetric P/D `resources.interface` with `KVCache` attached causes one half to run Host RoCE and the other UB** → Gate disaggregated A5 `vLLM` P/D Host RoCE strictly on `ModelDeploymentDirectPairRequestsInterfaces(md)` even when `md.Spec.KVCache != nil`, and probe `(ManufacturerAscend, "950")` in `modelDeploymentRoleNeedsInterfaceRejudge` on updates.
- **Stale `/etc/hixlep` on a UB/UBoE host overrides HIXL auto-generation, or missing `/etc/hixlep` on a Host RoCE node goes unnoticed** → Mount `/etc/hixlep` (`HostPathDirectory`, never `DirectoryOrCreate`) only when Host RoCE (`interface > 0`) is active, turning a missing Host RoCE directory into an immediate kubelet volume error while keeping UB/UBG/UBoE (`interface == 0`) on `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'`.
- **Invalid v1.0 `/etc/hccl_rootinfo.json` breaks multi-card HCCL (`EI0014`)** → Share `validateRankTableVersion950` between `rank_table.go` and `deviceplugin.go`; skip invalid files at `Allocate` time with a diagnostic log while preflight surfaces the node defect.

## Design Details

### Commands

Verification environment: **local workstation** (`darwin/arm64` / Linux, no Ascend A5 hardware required; all A5 runtime contracts are verified through unit tests, reconciler golden fixtures, and chart/docs linters). Run from the repository root:

```bash
CGO_ENABLED=1 go test -race ./pkg/utils/... ./pkg/devicemanager/allocator/ascend/... ./pkg/deviceplugin/...
CGO_ENABLED=1 go test -race ./pkg/worker/kuberess/... -run TestChart
CGO_ENABLED=1 go test -race ./pkg/nodefeature/... ./pkg/devicemanager/product/ascend/... ./pkg/worker/kvcache/inject/... -run 'TestRender|TestVLLMConfig|TestSGLang'
CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestModelDeploymentGoldenYAML|TestModelDeploymentLegacyRender|TestSynthesizeModelDeploymentConnector|TestModelDeploymentConnector|TestRenderModelDeployment|TestModelDeploymentDirectInterfaceProtocol|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentWebhook|TestValidateModelDeployment|TestModelDeploymentScaler'
make generate chart
make lint
make lint chart
make lint docs
```

### Project Structure

| Area | Responsibility |
|---|---|
| `pkg/utils/hostfs/` | Provides `ReadOnlyHostFS` (`DefaultHostRoot = "/host"`) backed by `os.OpenRoot`, validating `etc`/`proc`/`usr` directory markers, resolving merged-usr intermediate directory symlinks inside the root, rejecting leaf symlinks and `".."` traversal, and enforcing `os.SameFile` + byte caps with zero write/exec methods. |
| `deploy/gpustack-operator/chart/` & `pkg/worker/kuberess/chart_test.go` | Adds `deviceManager.hostRootMount.enabled` (default `true`), mounts host `/` read-only at `/host` on container `main` only across manufacturer DaemonSets, updates `values.schema.json` and `chart/README.md`, and tests enabled/disabled renders. |
| `pkg/device/`, `pkg/devicemanager/`, `pkg/devicemanager/allocator/ascend/` | Wires `HostRoot` (`--host-root`) through allocator options and `PreflightResponder`; shares pure `validateRankTableVersion950` in `rank_table.go`; conditionally emits native read-only `/etc/hccl_rootinfo.json` and `/lib/route.conf` `Allocate` mounts on A5 (`Family == "950"`). |
| `pkg/nodefeature/knowns.go` & `pkg/devicemanager/product/ascend/product.go` | Defines `nodefeature.AcceleratorFamilyAscend950 = "950"` once and reuses it in Ascend product classification. |
| `pkg/worker/kvcache/inject/` | Adds `AcceleratorFamily` and `AscendHostRoCE` to `inject.Input`; renders `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` vs outer `ascend_local_comm_res_path: "/etc/hixlep"` + `/etc/hixlep` RO mount for `vLLM` A5, and `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` for `SGLang` A5. |
| `pkg/worker/controllers/worker/` | Threads `AcceleratorType.Status.Detail.Family` into `ModelDeploymentDirectInterfaceProtocol`, connector synthesis, `/etc/hixlep` user-mount yielding, `modelDeploymentConnectorBinding` hash (`omitempty`), scaler capacity-neutral env catalog, and 4 new A5 golden YAML scenarios. |
| `pkg/worker/webhooks/worker/` | Threads `AcceleratorType.Status.Detail.Family` into `validateModelDeploymentInterfaceRequests` and updates `modelDeploymentRoleNeedsInterfaceRejudge` to probe `(ManufacturerAscend, "950")` on `ValidateUpdate`. |
| `docs/modules/model-deployment/` & `docs/getting-started/` | Documents all 6 Ascend A5 interconnect topologies, parameter injection rules, `deviceManager.hostRootMount.enabled`, and conditional A5 `/etc/hccl_rootinfo.json` + `/lib/route.conf` behavior. |

### Code Style

Follow repository Go conventions (`snake_case` filenames, explicit error handling, short plain comments without decorative symbols, and pure testable helpers):

```go
// ReadOnlyHostFS provides strictly read-only access to the node root filesystem
// mounted at a host-root prefix (default "/host"). Callers pass native host paths
// such as "/etc/hccl_rootinfo.json" without prepending the mount prefix.
type ReadOnlyHostFS struct {
	root *os.Root
}

// Close releases the underlying root directory descriptor.
func (h *ReadOnlyHostFS) Close() error {
	if h == nil || h.root == nil {
		return nil
	}
	return h.root.Close()
}
```

### Implementation Plan

Parallel entry frontier: `T1`, `T2`, and `T4` own disjoint paths and run concurrently (`{T1 ∥ T2 ∥ T4} → {T3 ∥ T5} → T6`).

- [x] **T1 · Build strictly read-only host-root filesystem utility (`pkg/utils/hostfs`)**
      Blocked by: None
      Owns: `pkg/utils/hostfs/**`
      Gate: review
      Acceptance: `pkg/utils/hostfs` exports `DefaultHostRoot = "/host"` and `ReadOnlyHostFS` (`OpenHostRoot(root)`, `Close()`, `Open(hostPath)`, `ReadFile`, `ReadRegularFile(hostPath, maxBytes)`, `Stat`, `Lstat`, `ReadDir`, `Readlink`, `Exists`) backed by Go `os.OpenRoot`; validates `etc`, `proc`, and `usr` exist as directories (`info.IsDir()`), failing closed otherwise; accepts native host paths without `/host` prefix (`"/" -> "."`, explicitly rejecting `""` and `".."` path elements without folding them); resolves intermediate directory symlinks (both relative `lib -> usr/lib` and absolute `lib -> /usr/lib`) strictly inside the host root with a 40-hop cap while rejecting leaf symlinks on `Lstat` and `ReadRegularFile`; implements `ReadDir` via `Open` + `f.ReadDir(-1)`; enforces `O_RDONLY`, `os.SameFile` TOCTOU verification, regular-file checks, and `maxBytes` cap; exposes zero write, create, chmod, remove, rename, or exec methods; unit tests cover valid reads, marker failures, traversal rejection, leaf vs intermediate symlinks, special/oversized files, and `Close()` lifecycle.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/utils/hostfs/... && make lint`

- [ ] **T2 · Mount host `/` read-only at `/host` on `device-manager` DaemonSet (`H5-A`) and add chart tests**
      Blocked by: None
      Owns: `deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml`, `deploy/gpustack-operator/chart/values.yaml`, `deploy/gpustack-operator/chart/values.schema.json`, `deploy/gpustack-operator/chart/README.md`, `pkg/worker/kuberess/chart_test.go`
      Gate: review
      Acceptance: `values.yaml` adds `deviceManager.hostRootMount.enabled: true` with clear comments; `device-manager` DaemonSet mounts `host-root-dir` (`hostPath.path: /`, `hostPath.type: Directory`) at `/host` with `readOnly: true` on container `main` only (never `initContainers[stage-libs]`) across all manufacturers when `enabled` is `true`, and omits both volume and mount when `false`; `make generate chart` leaves `values.schema.json` and `chart/README.md` in sync; `TestChartDeviceManagerMountsHostRootReadOnly` asserts both enabled and disabled renders across manufacturers; `make lint chart` exits 0.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/worker/kuberess/... -run TestChart && make lint chart`

- [ ] **T3 · Wire `HostRoot` and Ascend A5 conditional `/etc/hccl_rootinfo.json` and `/lib/route.conf` mounts in `device-manager` allocator and preflight**
      Blocked by: T1
      Owns: `pkg/device/types.go`, `pkg/devicemanager/option.go`, `pkg/devicemanager/config.go`, `pkg/devicemanager/allocator/option.go`, `pkg/devicemanager/allocator/config.go`, `pkg/devicemanager/allocator/allocator.go`, `pkg/devicemanager/allocator/ascend/**`
      Gate: review
      Acceptance: `device.AllocatorOptions` and `device-manager` options carry `HostRoot` (`--host-root`, default `hostfs.DefaultHostRoot`); `allocator/ascend/rank_table.go` extracts pure `validateRankTableVersion950(content []byte) error` shared by `checkRankTable` and `deviceplugin.go` while keeping `NewPreflighter` lazy so `darwin` unit tests without `/host` stay green; in `GetContainerAllocateResponse`, when any allocated accelerator has `Group.Family == "950"`, `deviceplugin.go` inspects `"/etc/hccl_rootinfo.json"` (`ReadRegularFile` with `1 MiB` cap + `validateRankTableVersion950`) and `"/lib/route.conf"` (`Lstat` regular non-symlink file) via `hostfs.ReadOnlyHostFS` and appends native read-only `&deviceplugin.Mount{HostPath: p, ContainerPath: p, ReadOnly: true}` at most once per container (never using `deviceplugin.NewROMount`), skipping without failing allocation when host root is unavailable, file is absent/invalid/symlink, `s.hostFS` is nil (`&server{}`), or `ctr != nil` already mounts that `ContainerPath`; non-A5 families emit neither mount; `PreflightResponder` wires the same `HostRoot` and `TestPreflightResponder_MatchesTheProductionResponder` verifies A5 parity; `aggregated.Stop()` closes any held `ReadOnlyHostFS`.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/devicemanager/allocator/ascend/... ./pkg/deviceplugin/... && make lint`

- [ ] **T4 · Add shared Ascend A5 family constant and A5 `vLLM`/`SGLang` connector synthesis in `pkg/worker/kvcache/inject`**
      Blocked by: None
      Owns: `pkg/nodefeature/knowns.go`, `pkg/devicemanager/product/ascend/product.go`, `pkg/worker/kvcache/inject/**`
      Gate: review
      Acceptance: `nodefeature.AcceleratorFamilyAscend950 = "950"` is defined once and reused by `productascend.Family`; `inject.Input` adds `AcceleratorFamily string` and `AscendHostRoCE bool`; `vllmConnectorExtraConfig` adds `AscendLocalCommResPath string \`json:"ascend_local_comm_res_path,omitempty"\``; on `EngineVLLMAscend` + `AcceleratorFamily == "950"`: (a) when `AscendHostRoCE == false`, injects defaulted `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` for P/D and `KVCache` roles (never `RoleEncode`) with no `/etc/hixlep` mount or `ascend_local_comm_res_path`; (b) when `AscendHostRoCE == true`, mounts `gpustack-ascend-hixlep` (`HostPathDirectory`, `/etc/hixlep`, `readOnly: true`), sets `ascend_local_comm_res_path: "/etc/hixlep"` on the **outer** `kv_connector_extra_config` only (standalone `MooncakeConnectorV1`, standalone `AscendStoreConnector`, and outer `MultiConnector` — never inner connectors), and omits `ASCEND_LOCAL_COMM_RES`; on `EngineSGLang` + `KVTransferBackendHIXL` + `AcceleratorFamily == "950"`, defaults `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` (keeping `sdma` for `Family != "950"` and never mounting `/etc/hixlep`); all A2/A3 and non-Ascend renders remain byte-identical.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/nodefeature/... ./pkg/devicemanager/product/ascend/... ./pkg/worker/kvcache/inject/... && make lint`

- [ ] **T5 · Wire A5 `Family` across `ModelDeployment` fabric resolution, connector synthesis, render hash, scaler gate, webhook admission, and golden YAML fixtures**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/**`, `pkg/worker/webhooks/worker/**`
      Gate: review
      Acceptance: `ModelDeploymentDirectInterfaceProtocol` takes `family string` and allows `"rdma"` (never `"efa"`) on Ascend A5 `vLLM` (`Family == "950"`, `len(role.Command) == 0`) for routed P/D pairs with both halves requesting interfaces (bypassing `HIXL` early return), managed multi-Pod/`elasticEp` `Server`, and non-P/D non-`Encode` `KVCache` roles — while rejecting asymmetric P/D (even with `KVCache`), `RoleEncode`, `SGLang`, and `Family != "950"`; `modelDeploymentRoleNeedsInterfaceRejudge` probes both `(ManufacturerNVIDIA, "")` and `(ManufacturerAscend, "950")` so `ValidateUpdate` rejudges dropped sibling interfaces or router removal under `HIXL`; `validateModelDeploymentInterfaceRequests` requires converged `Family == "950"` for A5 Host RoCE; `renderModelDeploymentPodTemplate` yields `gpustack-ascend-hixlep` if a user `additionalVolume` overlaps `/etc/hixlep` (via `path.Clean`) while preserving `ascend_local_comm_res_path`; `modelDeploymentConnectorBinding` fingerprints A5-effective connector state (`omitempty`) without adding raw `Family` to `modelDeploymentAcceleratorTypeHashInput` (unit test proves identical `pod-spec-hash` for live NVIDIA/A2/A3 with non-empty `Family`); `ASCEND_LOCAL_COMM_RES` is registered in `modelDeploymentDefaultedEnvNames` and `modelDeploymentScalerCapacityNeutralEnvNames`, and `family` is threaded into `modelDeploymentScalerOperatorEnvGateWithManufacturer` and `TestModelDeploymentOwnedAndDefaultedCannotDisagree`; 4 new A5 golden YAML scenarios pass (`len == 37`: A5 UB `vLLM` P/D, A5 Host RoCE `vLLM` P/D + `MultiConnector`, A5 Host RoCE `vLLM` single-Pod `Server` + `KVCache`, A5 `SGLang` P/D `device_urma`) with zero diff on all 33 existing golden scenarios, all 7 legacy fixtures, and `modelDeploymentRenderVersion = 5`.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestModelDeploymentGoldenYAML|TestModelDeploymentLegacyRender|TestSynthesizeModelDeploymentConnector|TestModelDeploymentConnector|TestRenderModelDeployment|TestModelDeploymentDirectInterfaceProtocol|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentWebhook|TestValidateModelDeployment|TestModelDeploymentScaler' && make lint`

- [ ] **T6 · Document Ascend A5 interconnect topologies, parameter injection, and read-only `/host` mounts**
      Blocked by: T2, T3, T5
      Owns: `docs/modules/model-deployment/parameterize.md`, `docs/modules/model-deployment/prefill-decode.md`, `docs/modules/model-deployment/configuration.md`, `docs/getting-started/vendor-prerequisites.md`, `docs/README.md`
      Acceptance: Documents all 6 Ascend A5 scenarios (UB `ub_ctp`, UBG `ub_rtp:device`, UBoE `uboe:device`, Host RoCE `roce:host` via `resources.interface > 0`, Aggregated `Server`, and constrained static-UB `command` take-over + administrator `instance-host-path-volume-allowed`); clarifies UB/URMA (`fabric.kind=ub`, `interface: 0`) vs Linux Host RDMA (`device.gpustack.ai/rdma*`, `interface > 0`); updates `parameterize.md` for `ASCEND_LOCAL_COMM_RES`, outer `ascend_local_comm_res_path`, `/etc/hixlep` user-mount yielding, and SGLang `device_urma`; documents `deviceManager.hostRootMount.enabled` (default `true`, layered read-only boundary, policy/runtime disable path) and A5 conditional `/etc/hccl_rootinfo.json` + `/lib/route.conf` behavior across MindCluster `26.0.x` and `26.2+`; `make lint docs` exits 0.
      Verify: `make lint docs`

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
None. Existing 33 golden YAML fixtures (`pkg/worker/controllers/worker/testdata/golden-render/`) and 7 legacy render fixtures (`pkg/worker/controllers/worker/testdata/legacy-render/`) already pin current A2/A3 and non-Ascend rendering; T5 adds an `atlas-950` (`Family: "950"`) base fixture and extends `TestModelDeploymentGoldenYAML` from 33 to 37 scenarios.

#### Unit tests
- `pkg/utils/hostfs`: `2026-11-19` - `>= 90%` (valid root reads with native host paths; fail-closed missing/non-dir `etc`/`proc`/`usr` markers; `""` / `"/"` / `".."` traversal rejection; leaf absolute and relative symlink rejection; intermediate relative `lib -> usr/lib` and absolute `lib -> /usr/lib` directory symlink resolution within root + 40-hop loop guard; directory/FIFO/oversized file rejection; `os.SameFile` regular-file check; `Close()` lifecycle)
- `pkg/worker/kuberess`: `2026-11-19` - chart rendering assertions (`TestChartDeviceManagerMountsHostRootReadOnly` across all manufacturers for `main` container only, never `stage-libs`, covering `deviceManager.hostRootMount.enabled=true` and `false`)
- `pkg/devicemanager/allocator/ascend`: `2026-11-19` - `>= 85%` (A5 conditional `/etc/hccl_rootinfo.json` v2.0 + `/lib/route.conf` mounts across all 4 allocation modes, multi-card single-mount dedup, absent/invalid/v1.0/symlink/directory/unmounted-root skip without allocation failure, non-A5 omission, `ctr.VolumeMounts` collision dedup and `ctr == nil` safety, zero-value `&server{}` safety, `checkRankTable` vs `Allocate` validation parity, and `TestPreflightResponder_MatchesTheProductionResponder` A5 parity)
- `pkg/worker/kvcache/inject`: `2026-11-19` - `>= 90%` (A5 `vLLM` UB `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` vs Host RoCE outer `ascend_local_comm_res_path: "/etc/hixlep"` + `/etc/hixlep` mount on `MooncakeConnectorV1`, `AscendStoreConnector`, and outer `MultiConnector` with inner connectors clean; `RoleEncode` untouched; A5 `SGLang` `ASCEND_MF_TRANSFER_PROTOCOL=device_urma` vs A2/A3 `sdma`; user env overrides)
- `pkg/worker/controllers/worker`: `2026-11-19` - `ModelDeploymentDirectInterfaceProtocol` A5 matrix (routed P/D `HIXL`/`Mooncake` even with `awsEFAEnabled=true`, asymmetric P/D rejected with and without `KVCache`, single-Pod `Server`+`KVCache`, multi-Pod/`elasticEp` `Server`, `RoleEncode`, take-over, `SGLang`, A2/A3 unchanged); upgrade `pod-spec-hash` stability for live NVIDIA/A2/A3 with non-empty `Family`; user `additionalVolumes` `/etc/hixlep` yielding; `TestModelDeploymentOwnedAndDefaultedCannotDisagree` and scaler prior-identity A5 rows; 4 new A5 golden YAML scenarios with zero diff on existing 33 golden + 7 legacy fixtures
- `pkg/worker/webhooks/worker`: `2026-11-19` - `ValidateCreate` A5 Host RoCE acceptance and asymmetric/SGLang/A2/A3/unconverged-`Family` rejection; `ValidateUpdate` HIXL rejudge regression when a sibling drops `interface` or `spec.router` is removed

#### Integration tests
- `pkg/worker/controllers/worker/model_deployment_golden_yaml_test.go`: Full reconciler render pipeline (controller + fake client + `AcceleratorType` + `KVCache` + `ModelDeployment`) exercised end-to-end across all 37 scenarios (`pd-vllm-ascend-a5-ub`, `pd-vllm-ascend-a5-host-roce-multiconnector`, `server-vllm-ascend-a5-kvcache-host-roce`, `pd-sglang-ascend-a5-urma`, plus 33 existing scenarios).

#### e2e tests
- None in CI (no Ascend A5 hardware nodes exist in the test infrastructure). Local unit tests, 37-scenario reconciler golden fixtures, `TestPreflightResponder_MatchesTheProductionResponder`, `make lint chart`, and `make lint docs` verify all manifest, webhook, and allocation contracts deterministically without live A5 hardware.

## Alternatives

- **Unconditional `HostPathFile` for `/etc/hccl_rootinfo.json` in `ModelDeployment` Pod specs**: Rejected because healthy A5 nodes frequently omit `/etc/hccl_rootinfo.json`. A Pod-level `HostPathFile` fails kubelet volume setup on those nodes, while `HostPathFileOrCreate` creates an empty file that breaks HCCL with `EI0014`.
- **Node-presence auto-detection of `/etc/hixlep` to drive `vLLM` config**: Rejected because `ModelDeployment` renders `--kv-transfer-config` pool-wide before scheduling, and leftover `/etc/hixlep` files on UB/UBoE hosts would erroneously overwrite `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` and break UB/UBoE workloads.
- **Adding new A5-specific `Setting` or `GPUSTACK_*` environment switches (`model-deployment-ascend-a5-local-comm-res` / `GPUSTACK_DEVICE_PLUGIN_ASCEND_A5_HOST_MOUNTS`)**: Rejected because vendor-generation-specific toggles add permanent configuration complexity before live hardware shows any discrepancy with the code-deduced rules, whereas `deviceManager.hostRootMount.enabled`, `DefaultedEnv` overrides (`roles[].env`), and Scenario 6 (`roles[].command` take-over) already provide clean escape hatches.
- **Changing `instance-host-path-volume-allowed` default from `"false"` to `"true"`**: Rejected because `instance-host-path-volume-allowed` governs arbitrary tenant `additionalVolumes[*].hostPath` mounts across all namespaces and manufacturers; enabling it by default breaks multi-tenant host isolation and still cannot inject `"ascend_local_comm_res_path"` inside operator-owned `--kv-transfer-config`.
- **Mounting only `/etc` or mounting `/` unconditionally without a chart toggle on `device-manager`**: Rejected in favor of a universal read-only `/` -> `/host` mount gated by `deviceManager.hostRootMount.enabled` (`true` by default), so `/lib/route.conf` and future non-Ascend vendor host inspections work through one audited boundary while policy-hardened clusters retain an explicit opt-out.

## Open Questions

None.
