# Ascend A5 Matrix

Evidence: contract. Canonical host, device-plugin, serving, cache and remote-transfer matrix for
Ascend Atlas A5 (`AcceleratorType` *`status.detail.family`* `"950"`, `Ascend950PR` and
`Ascend950DT`). Ascend A2 and A3 (`family` other than `"950"`) retain legacy rendering unchanged.

## Contents

- [Host prerequisites](#host-prerequisites)
- [Conditional host mounts](#conditional-host-mounts)
- [ModelDeployment matrix](#modeldeployment-matrix)
- [KVCacheBackend matrix](#kvcachebackend-matrix)
- [Remote delivery matrix](#remote-delivery-matrix)

## Host prerequisites

Atlas A5 distinguishes on-die UnifiedBus (*UB*) / Unified Remote Memory Access (*URMA*) from Linux
Host RDMA (*Host RoCE*). Host preparation and `preflight` checks verify the runtime, ranktable and
endpoint files before workloads land on the node.

### Runtime and ranktable

- **Container runtime**: Install `ascend-docker-runtime` newer than `26.0.x` (prefer `26.2+`).
  Basic A5 support landed in `26.0.0`. Older releases fall back to the legacy manager-device list
  carrying `/dev/devmm_svm`, which A5 hosts lack, causing container creation to fail naming that
  device. Read the installed version from
  `/usr/local/Ascend/Ascend-Docker-Runtime/ascend_docker_runtime_install.info`.
- **Ranktable version**: `/etc/hccl_rootinfo.json` must be absent (healthy default) or version
  `2.0`. Format versions map by generation: `1.0` for A2 (`910`/`910B`/`310P`), `1.2` for A3
  (`910C`), and `2.0` for A5 (`950`). A5 loads `libhccl_v2`, which rejects non-`2.0` files on
  multi-card jobs with `Config_Error_Ranktable` error `EI0014`.
- **Runtime mount change**: `ascend-docker-runtime` through `26.1.x` mounts `/etc/hccl_rootinfo.json`
  unconditionally when present; `26.2+` removed that default mount. Neither release mounts
  `/lib/route.conf`.

### Preflight host checks

On Atlas A5 (`950`) accelerators, `device-manager preflight` adds two node-level checks at depth
`declared` on every accelerator and under every mode:

| Check | Source | Healthy (`ok`) | Failing (`unavailable`) |
|---|---|---|---|
| `ascend-docker-runtime` | Installer file under `/usr/local/Ascend` | MindCluster `26.0.0` or newer | Older release or unreadable version |
| `hccl-ranktable` | `/etc/hccl_rootinfo.json` via `/host` | File absent or JSON `"version": "2.0"` | Other version, unparseable file, symlink, or unreadable host root |

An absent ranktable reports `ok`, whereas an unmounted or invalid `/host` root reports `unavailable`
so a dry run cannot confuse an uninspected host with a healthy node. Symlinks report `unavailable`
and name their target so the container never resolves an absolute host symlink inside its own root.

### Host RoCE endpoints

For Ascend A5 *Host RoCE* (`roce:host`) workloads, provision per-NPU endpoint JSON files at
`/etc/hixlep/ub_endpoint_npu_<physical_id>.json` on each Host RoCE node. Operator-managed Host RoCE
`Pod`s mount `/etc/hixlep` read-only (`HostPathDirectory`). A missing directory fails kubelet volume
setup, and a missing endpoint file fails engine or SDK startup closed (`FileNotFoundError` or
`SourceUnavailableError`).

## Conditional host mounts

On Ascend A5 (`status.detail.family` equal to `"950"`), `device-manager` inspects host files through
`/host` at `Allocate` time and conditionally mounts them read-only for `SGLang` MemFabric
(`device_urma`) and legacy multi-node UB HCCL across `ascend-docker-runtime` releases:

| Host file | Mount condition | Skip behavior |
|---|---|---|
| `/etc/hccl_rootinfo.json` | Regular non-symlink file (`<= 1 MiB`) containing JSON `"version": "2.0"` | Absent, symlinked, oversized, or non-`2.0` files are skipped with a diagnostic log; allocation succeeds |
| `/lib/route.conf` | Regular non-symlink file; resolves merged-usr `/lib` → `/usr/lib` inside `/host` | Absent, symlinked, or non-regular files are skipped with a diagnostic log; allocation succeeds |

- **Non-A5 exclusion**: Ascend `910`, `910B`, `910C`, `310P` and `310B` never receive either mount.
- **Collision handling**: Containers already declaring a mount at the same path receive no duplicate.
- **Host-root confinement**: `deviceManager.hostRootMount.enabled` (default `true`) mounts `/`
  read-only at `/host` on the `main` container only (never `stage-libs`). `os.OpenRoot` confines
  `etc`, `proc` and `usr` reads to `O_RDONLY` with `os.SameFile` checks and rejects leaf symlinks.
  Setting `deviceManager.hostRootMount.enabled=false` skips both mounts without failing allocations.

## ModelDeployment matrix

On Ascend A5 (`AcceleratorType` *`status.detail.family`* `"950"`), `ModelDeployment` selects UB/URMA
versus Linux Host RDMA from the engine and *`roles[].resources.interface`*. Nodes publishing
`fabric.kind=ub` serve UB/URMA with `interface: 0`; Host RoCE grants `device.gpustack.ai/rdma.shared`
(`1`) or `device.gpustack.ai/rdma` (`> 1`), never AWS EFA.

| Scenario | Interface | Operator selection | User requirement |
|---|---|---|---|
| **UB (`ub_ctp`)**, intra-SuperPod | `0` (omitted) | Selects on-die UB/URMA defaults (`vLLM` `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'`, `SGLang` `ASCEND_MF_TRANSFER_PROTOCOL=device_urma`) with no `/etc/hixlep` mount | Managed role; omit *`resources.interface`* |
| **UBG (`ub_rtp:device`)**, cross-SuperPod | `0` (omitted) | Same on-die UB path with no `/etc/hixlep` mount | Pass `ASCEND_GLOBAL_RESOURCE_CONFIG` in *`roles[].env`* |
| **UBoE (`uboe:device`)** | `0` (omitted) | Same on-die UB path with no `/etc/hixlep` mount | Pass `ASCEND_GLOBAL_RESOURCE_CONFIG` in *`roles[].env`* |
| **Host RoCE (`roce:host`)** | `> 0` on both `Prefill` and `Decode`, or `KVCache` `Server` | Grants `device.gpustack.ai/rdma*`, sets outer `kv_connector_extra_config.ascend_local_comm_res_path: "/etc/hixlep"`, and mounts `/etc/hixlep` read-only (`gpustack-ascend-hixlep`) | `vLLM` only with converged `"950"`; provision `/etc/hixlep` endpoint files |
| **Aggregated `Server`** (no KV connector) | `0`, or `> 0` on multi-`Pod`/Elastic EP `vLLM` | Renders no KV connector or A5 transfer resources | Set `interface > 0` only on multi-`Pod` or Elastic EP `vLLM` `Server` roles |
| **Constrained static-UB fallback** | `0` (omitted) | Skips operator CLI and KV/transfer env synthesis; sets `status.roles[].unmanaged: true` | Use *`roles[].command`* take-over and *`additionalVolumes`* with `instance-host-path-volume-allowed: "true"` |

- **Symmetric pair interfaces**: Set *`resources.interface > 0`* on both `Prefill` and `Decode`;
  asymmetric requests fail admission.
- **Outer connector path**: `ascend_local_comm_res_path` is set only on outer
  `kv_connector_extra_config` (including `MultiConnector`), never on inner child connectors.
- **Mount yielding**: User *`additionalVolumes`* at `/etc/hixlep` replaces `gpustack-ascend-hixlep`
  while keeping `ascend_local_comm_res_path`.
- **Role and engine exclusions**: `RoleEncode` (`kind: Encode`) and `SGLang` never receive
  `/etc/hixlep` or `ASCEND_LOCAL_COMM_RES` on KV legs. `SGLang` A5 *PD* rejects
  *`resources.interface > 0`* unless a bound `KVCache` pool uses `RDMA` (or remote Host RoCE weight
  delivery applies).
- **Host-path gate**: Operator-managed `/etc/hixlep` works with `instance-host-path-volume-allowed`
  set to `false`; static-UB *`command`* take-over requires `true`.

## KVCacheBackend matrix

On `KVCacheBackend` member groups using `protocol: CANN` and `medium: DRAM`, omitting
*`fabricInterfaceCount`* keeps the group off Host RDMA (A2/A3 and A5 UB/UBG/UBoE). Setting
*`fabricInterfaceCount >= 1`* enables Ascend A5 Host RoCE (`roce:host`):

| Scenario | Configuration | Resulting behavior |
|---|---|---|
| **A5 UB (`ub_ctp`)** | `protocol: CANN`, `medium: DRAM`, omit *`fabricInterfaceCount`* | On-die UB path; no Host RDMA grant, no `/etc/hixlep` mount, Mooncake defaults `ASCEND_LOCAL_COMM_RES` |
| **A5 UBG (`ub_rtp:device`)** | `protocol: CANN`, omit *`fabricInterfaceCount`*, set *`extraEnv`* `ASCEND_GLOBAL_RESOURCE_CONFIG` | Same non-RDMA `Pod` template with `'{"comm_resource_config.protocol_desc":["ub_rtp:device"]}'` |
| **A5 UBoE (`uboe:device`)** | `protocol: CANN`, omit *`fabricInterfaceCount`*, set *`extraEnv`* `ASCEND_GLOBAL_RESOURCE_CONFIG` | Same non-RDMA `Pod` template with `'{"comm_resource_config.protocol_desc":["uboe:device"]}'` |
| **A5 Host RoCE (`roce:host`)** | `protocol: CANN`, `medium: DRAM`, *`fabricInterfaceCount: 1`* | Grants `hostNetwork`, `dnsPolicy: ClusterFirstWithHostNet`, `IPC_LOCK` + `SYS_RESOURCE`, `rdma.shared`/`rdma`, `/etc/hixlep` RO mount (`ascend-hixlep`), and defaulted `ASCEND_LOCAL_COMM_RES=""` + `ASCEND_GLOBAL_RESOURCE_CONFIG` |
| **A5 Host RoCE override** | `protocol: CANN`, *`fabricInterfaceCount >= 1`*, user *`extraEnv`* or *`hostPaths`* at `/etc/hixlep` | Keeps Host RDMA grant; yields `/etc/hixlep` mount on path overlap and yields `ASCEND_*` defaults when declared |
| **Non-A5 Ascend A2/A3** | `910B`/`910C`, `protocol: CANN`, `medium: DRAM`, omit *`fabricInterfaceCount`* | Legacy `ascend` rendering (`aclrtMallocHost`); no Host RDMA grant or `/etc/hixlep` mount |

Defaulted Host RoCE environment on `CANN` members (`fabricInterfaceCount >= 1`):

- `ASCEND_LOCAL_COMM_RES=""`
- `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep/ub_endpoint_npu_0.json"}'`
- Admission suppresses the idle *`fabricInterfaceCount`* warning on `CANN` and rejects
  *`localDisks[].path`* overlapping `/etc/hixlep`.

Illustrative Ascend A5 UBG `KVCacheBackend` manifest (use `uboe:device` for UBoE, or set
`fabricInterfaceCount: 1` for Host RoCE):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: ascend-a5-ubg-cache
spec:
  transport:
    protocol: CANN
  connection:
    managed:
      leader: {}  # replicas, allocationStrategy and multiTenancy default
      members:
        - nodeSelector:
            kvcache-ascend-a5: "true"
          medium: DRAM
          image: gpustack/mirrored-mooncake:0.3.14-cann9.2-a5  # A5 (950PR/950DT) CANN build
          capacityPerMember: 32Gi
          extraEnv:
            - name: ASCEND_GLOBAL_RESOURCE_CONFIG
              value: '{"comm_resource_config.protocol_desc":["ub_rtp:device"]}'
          hostPaths:
            - path: /usr/local/Ascend/driver
              mountPath: /usr/local/Ascend/driver
              type: Directory
              readOnly: true
            - path: /usr/local/dcmi
              mountPath: /usr/local/dcmi
              type: Directory
              readOnly: true
            - path: /usr/local/bin/npu-smi
              mountPath: /usr/local/bin/npu-smi
              type: File
              readOnly: true
```

## Remote delivery matrix

On Ascend A5 remote weight delivery (`hixl`, CANN 9.2+ on `vLLM` and `SGLang`), source Helm value
`modelTransfer.ascend.hostRoCE.enabled` (default `false`) opts `gpustack-operator-model-transfer-ascend`
into Host RoCE, while `roles[].resources.interface > 0` enables Host RoCE on eligible target roles
(`Server` and symmetric *PD* pairs; `Encode` and asymmetric *PD* pairs are rejected).

| Scenario | Configuration | Resulting behavior |
|---|---|---|
| **A5 UB default** | `hostRoCE.enabled: false`, target `interface: 0` | Target defaults `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'`; uses `ub_ctp:device` and `aclrtMallocHost` with no `/etc/hixlep` |
| **A5 UB H2H** | Set `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["ub_ctp:host"]}'` on source and target | Pure Host-to-Host UB over `ub_ctp:host` using `aclrtMallocHost`; no `/etc/hixlep` or `hostNetwork` |
| **A5 UBG** | Set `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["ub_rtp:device"]}'` on source and target | Cross-SuperPod UBG over `ub_rtp:device` using `aclrtMallocHost`; no `/etc/hixlep` or `hostNetwork` |
| **A5 UBoE** | Set `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["uboe:device"]}'` on source and target | UBoE over `uboe:device` using `aclrtMallocHost`; no `/etc/hixlep` or `hostNetwork` |
| **A5 Host RoCE** | Source `hostRoCE.enabled: true` + target `interface > 0` | Grants target `device.gpustack.ai/rdma*`, mounts `/etc/hixlep` RO, fails closed on physical NPU resolution, and uses `2 MiB`-aligned `mmap` |
| **Non-A5 A2/A3** | `910B`/`910C`, `hostRoCE.enabled: false`, `interface: 0` | Legacy `hixl` path using `aclrtMallocHost`; no `/etc/hixlep` or `hostNetwork` |

### Source and SDK rules

- **Host RoCE source `DaemonSet`**: `modelTransfer.ascend.hostRoCE.enabled: true` applies
  `hostNetwork: true`, `ClusterFirstWithHostNet`, read-only `/etc/hixlep` (`HostPathDirectory`),
  and default `ASCEND_GLOBAL_RESOURCE_CONFIG` (`roce:host` + `/etc/hixlep`) on
  `gpustack-operator-model-transfer-ascend` only.
- **Ports and UDS control**: Under `hostNetwork: true`, the broker exposes only `:9251` on the host
  namespace (CSI clients dial `<sourcePodIP>:9251` with TLS SNI `<daemonSetName>.<namespace>.svc`);
  broker-worker private HTTP control uses `/run/model-transfer-sockets/{control,worker}.sock` UDS
  (keeping `/run/model-transfer/bootstrap` read-only on `worker`) instead of loopback `9250`/`9252`,
  while HIXL rendezvous binds an ephemeral port on `status.podIP`.
- **Same-image drained rollout**: Source `Pod` templates stamp `model-transfer.gpustack.ai/config-hash`
  covering image, `hostNetwork`, `dnsPolicy`, `env` and `/etc/hixlep` so `ModelTransferImageReconciler`
  drains and replaces `Pod`s when toggling `hostRoCE.enabled` at the same image tag.
- **Mixed-fleet `nodeSelector`**: `modelTransfer.nodeSelector` applies to all vendor source
  `DaemonSet`s (`nvidia`, `amd`, `ascend`). Narrowing it on a running cluster deletes `Pod`s on
  excluded nodes immediately, bypassing `OnDelete` drain.
- **Python SDK `Hixl.initialize()`**: Forwards `ASCEND_LOCAL_COMM_RES` (`LocalCommRes`) and
  `ASCEND_GLOBAL_RESOURCE_CONFIG` (`GlobalResourceConfig`), preserves `"BufferPool": "0:0"`, and
  normalizes nested `comm_resource_config.protocol_desc` to HIXL's flat dotted key
  `"comm_resource_config.protocol_desc"`.
- **Physical NPU lookup**: When `ASCEND_LOCAL_COMM_RES` is empty and `local_comm_res_path` is a
  directory, the SDK resolves `aclrtGetDevice` and `aclrtGetPhyDevIdByUserDevId` to
  `<dir>/ub_endpoint_npu_<phy_id>.json` and fails closed with `SourceUnavailableError` on missing
  symbols, lookup errors, or absent files (never defaulting to NPU `0`).
- **Host buffer allocation**: When `protocol_desc` selects `"roce:host"`, both source (`AclHostOwner`)
  and target (`BorrowedAclOwner`) allocate `2 MiB`-aligned private anonymous `mmap` buffers
  registered as `MEM_HOST`, release idempotently by allocator kind, and quarantine mappings if
  `close()` is refused. Non-`roce:host` paths use `aclrtMallocHost`.
- **Bundled wheel precedence**: Target engine bootstrap skips installing the host-staged wheel when
  `gpustack_model_transfer` is already preinstalled; upgrade or omit stale preinstalled wheels in
  CANN 9.2+ engine images to pick up A5 options and `roce:host` `mmap`.

Source Helm values for UB H2H, UBG, or UBoE (for Host RoCE, set `ascend.hostRoCE.enabled: true` and
omit `env.ASCEND_GLOBAL_RESOURCE_CONFIG`):

```yaml
modelTransfer:
  nodeSelector:
    ascend.gpustack.ai/generation: a5  # admin-supplied node label
  ascend:
    hostRoCE:
      enabled: false  # keeps hostNetwork and /etc/hixlep off for UB-family paths
  env:
    # ["ub_ctp:host"] for UB H2H, ["ub_rtp:device"] for UBG, ["uboe:device"] for UBoE
    ASCEND_GLOBAL_RESOURCE_CONFIG: '{"comm_resource_config.protocol_desc":["ub_ctp:host"]}'
```

---

**See also** — [Vendor Prerequisites](https://docs.gpustack.ai/gpustack-operator/v0.12.1/docs/getting-started/vendor-prerequisites/index.md#ascend) (Ascend host setup) · [Preflight Checks](https://docs.gpustack.ai/gpustack-operator/v0.12.1/docs/modules/devices/preflight/index.md#ascend-a5-host-checks) (A5 preflight checks) · [Prefill and Decode](https://docs.gpustack.ai/gpustack-operator/v0.12.1/docs/modules/model-deployment/prefill-decode/index.md#ascend-a5-topologies) (`ModelDeployment` A5 topologies) · [KV Cache Backend](https://docs.gpustack.ai/gpustack-operator/v0.12.1/docs/modules/kv-cache/backend/index.md#ascend) (`CANN` cache members) · [Remote Weight Delivery](https://docs.gpustack.ai/gpustack-operator/v0.12.1/docs/modules/model-delivery/remote/index.md#ascend) (Ascend remote transfer) · [Parameters](model-deployment-parameters.md#transfer-parameters) (synthesized transfer keys)
