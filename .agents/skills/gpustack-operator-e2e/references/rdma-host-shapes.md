# RDMA verification (`case-14.sh`): which host shape answers which reading

> **Purpose** — `cases/case-14.sh` consolidates the active NIC/RDMA end-to-end checks into four
> self-recovering legs gated on the SHAPE of the machine they run on, not on its manufacturer. This
> page records what each host shape can answer, what it structurally cannot, and what environment
> variables and userspace packages `case-14.sh` reads.

Run `cases/run-rdma-block.sh --report-only [NS]` against a cluster to print the capability matrix
computed from that cluster's live `Devices` ledger without touching the cluster.

## How `case-14.sh` maps to host capabilities

A reading's host requirement is **whatever that reading reads**:

- **Leg 1 — Host-fabric member `DaemonSet` rendering (`any` cluster, zero RDMA hardware needed):**
  Applies a render-only `KVCacheBackend` with `transport.protocol: RDMA` (selecting a node label no
  node carries so no member Pod is scheduled or image pulled) and asserts the rendered `DaemonSet`
  requests `device.gpustack.ai/rdma.shared: "1"`, mounts zero `/dev/infiniband` `hostPath`, sets
  `hostNetwork: true` + `ClusterFirstWithHostNet`, and requests `IPC_LOCK` / `SYS_RESOURCE` without
  `privileged: true`.
- **Leg 2 — NIC/RDMA inventory, `rdma.*` labels, stability & restart (any node with a `Devices` object):**
  Verifies `Devices.spec.interfaces` is non-empty, sorted by name, marks virtual interfaces, sets
  `pciRootId` to the outermost `pciSwitches` bridge, agrees with node `feature.gpustack.ai/rdma.*`
  labels in both directions, stays generation-stable across quiet detect passes, and preserves
  hardware-bearing rows across a `device-manager` Pod restart.
- **Leg 3 — RDMA extended resource sizing (node carrying `>= 1` RDMA endpoint):**
  Uses an independent read-only `/sys` probe Pod (`rdma_probe_try`) to verify `/sys/class/infiniband`
  inventory correspondence, per-mode allocatable counts (`device.gpustack.ai/rdma`,
  `device.gpustack.ai/rdma.shared` = `N × poolSize`, `device.gpustack.ai/rdma.partitioned` = usable
  VFs), retired `device.gpustack.ai/rdma.sliced` absent or `0`, `Failed`-link capacity vs allocatable
  exclusion, and SR-IOV `sriov_numvfs` vs VF/PF separation.
- **Leg 4 — Container device-cgroup `open()` & verbs isolation (node carrying `>= 1` whole-function RDMA endpoint):**
  Verifies an unprivileged Pod requesting `device.gpustack.ai/rdma.shared: "1"` receives
  `NCCL_IB_HCA` matching the ledger, sees only granted `/dev/infiniband/uverbs*` device nodes,
  succeeds on `open(O_RDWR)`, and (when `E2E_RDMA_IMAGE` ships `ibv_devinfo`) resolves the granted
  device through `libibverbs`, while an identical unprivileged control Pod without the resource
  request is refused `open()` at the device cgroup or mount namespace.

Each leg auto-skips individually with explicit `SKIP` rows when the cluster lacks its host shape,
preserving earlier `PASS` verdicts.

### SR-IOV `sriov_numvfs` states in Leg 3

Leg 3's SR-IOV sub-checks read `/sys/class/infiniband/<dev>/device/sriov_numvfs` from the kernel
rather than the ledger so a host with zero VFs does not pass vacuously as `0 == 0`:

| reading of `/sys/class/infiniband/<dev>/device/sriov_numvfs` | what it means | what to do |
|---|---|---|
| a number above zero | virtual functions are configured | this host answers the SR-IOV reading |
| `0` | capability is present, unconfigured | configure virtual functions on **this** machine |
| file does not exist | capability is **not present in this guest** (e.g. hypervisor pass-through) | test SR-IOV on bare metal or VF-capable host |

## What a machine must carry, as readings

| property | how to read it | the answer that qualifies |
|---|---|---|
| P1 an RDMA endpoint exists | `ls /sys/class/infiniband/` | at least one entry (unlocks `case-14.sh` Leg 3; whole-function unlocks Leg 4) |
| P2 SR-IOV is present and configured | `cat /sys/class/infiniband/<dev>/device/sriov_numvfs` | integer `> 0` (unlocks `case-14.sh` Leg 3 SR-IOV VF/PF checks) |
| P3 failed-link exclusion | `Devices.spec.interfaces[].link.state` / `.virtualFunctions[].link.state` | an endpoint already reporting `Failed` (observed when present; never induced) |

> **Retired standalone host-shape probes.** Earlier revisions carried separate observation scripts for
> kubelet `configz` `TopologyManager` policy preflight, asymmetric cross-NUMA accelerator+RDMA
> refusal, and classic InfiniBand `ib_write_bw` perftest transport. Those unit-covered or
> hardware-lab-only probes were retired during the 37-case suite consolidation (`specs/2026-11-24-cpu-mock-device-manager-and-e2e-consolidation.md`);
> `run-rdma-block.sh --report-only` still prints the full node topology and policy facts from
> `_rdma-lib.sh` for diagnostic inspection.

## Environment `case-14.sh` reads

| variable | what it is for | without it |
|---|---|---|
| `RDMA_NODE_NAME` | pin the node instead of taking the first qualifying node | first qualifying node is used |
| `RDMA_TEST_NS` | namespace for test and `/sys` probe Pods (default `default`) | `default`; a `restricted` PodSecurity namespace skips the `/sys` host probe |
| `E2E_RDMA_PROBE_IMAGE` | `/sys` probe Pod image (needs `sh`, `ls`, `cat`, `exec <>`) | `busybox:1.36` |
| `E2E_RDMA_WORKLOAD_IMAGE` | image for unprivileged grant/control `open()` Pods | `E2E_RDMA_PROBE_IMAGE` |
| `E2E_RDMA_IMAGE` | image shipping `ibv_devinfo` (`ibverbs-utils`, `rdma-core`, `ibverbs-providers`) | `ibv_devinfo` check records `SKIP`; `open()` checks still run |
| `E2E_RDMA_SHARED_POOL_SIZE` | expected shared-family token count per endpoint | `64` (the shipped ceiling) |
| `E2E_RDMA_POD_TIMEOUT` | seconds a probe or workload Pod gets to reach `Running` | `180` |
| `E2E_MOONCAKE_IMAGE` | image string written into Leg 1's render-only `KVCacheBackend` | default Mooncake tag (never pulled in Leg 1) |
