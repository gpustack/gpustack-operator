# Preflight Checks

Run `device-manager preflight` on a bare host to verify accelerator detection, slicing isolation,
RDMA link health, and kubelet `TopologyManager` configuration before joining a cluster. It drives
each vendor's production allocator directly without needing CRDs, NFD labels, or a running cluster.

Unlike read-only `detect` and `monitor`, `preflight` stages preload libraries and briefly toggles
off-state sharing flags when required. It also launches non-privileged probe containers unless
`--dry-run` is passed.

## Contents

- [Prerequisites](#prerequisites)
- [Command line](#command-line)
- [Mounts](#mounts)
- [Flags](#flags)
- [Side effects](#side-effects)
- [Emitted steps](#emitted-steps)
- [Output by manufacturer](#output-by-manufacturer)
- [Result format](#result-format)

## Prerequisites

Run `preflight` **before** scheduling live workloads on the host (or pass `--dry-run` for a zero-write
plan):

- **Temporary occupancy**: Probe containers hold each sliceable accelerator briefly while measuring
  quota enforcement.
- **Staged libraries**: Copies `/var/lib/gpustack/operator/lib/<manufacturer>` onto the host; if the
  node already runs Device Manager, use the **exact same operator image tag** so staged preload
  libraries match.
- **Driver mode toggle**: On Ascend (`container-share`) and Cambricon (`smlu-mode`), if the sharing
  flag is off, `preflight` enables it to confirm driver support and immediately restores it (reporting
  loudly if restore fails).

Probe containers run **unprivileged**, receiving only the device nodes, mounts, and environment the
production allocator injects (`--privileged` on the outer `preflight` container covers driver reads
and `/dev` discovery).

## Command line

Run the command for your hardware vendor. On `containerd`, replace `docker run` with `nerdctl run`
(except on Moore Threads and NVIDIA, noted below). Verify each host driver path exists first, because
`docker run -v` creates missing host directories empty and causes detection to report `0` cards.

### AMD

```bash
docker run --rm --privileged --network=host \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  -v /opt/rocm:/opt/rocm:ro -v /opt/rocm/lib:/opt/rocm/lib:ro \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=amd
```

Mount both `/opt/rocm` and `/opt/rocm/lib` so the host resolves ROCm symlinks on multi-version
installations (or mount `-v /opt/rocm/core-<ver>:/opt/rocm:ro` to test one version).

### Ascend

```bash
docker run --rm --privileged --network=host \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  -v /usr/local/Ascend:/usr/local/Ascend:ro -v /usr/local/dcmi:/usr/local/dcmi:ro \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=ascend
```

The outer container needs no `--runtime ascend` because `/dev` is already mounted; inner Ascend
probe containers pass `--runtime ascend` automatically under `docker` (and emit commands under
`nerdctl`).

### Cambricon

```bash
docker run --rm --privileged --network=host \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  -v /usr/local/neuware:/usr/local/neuware:ro \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=cambricon
```

### Hygon

```bash
docker run --rm --privileged --network=host \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  -v /opt/hyhal:/opt/hyhal:ro -v /opt/dtk:/opt/dtk:ro \
  -v /etc/dmi_mig_config:/etc/dmi_mig_config \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=hygon --probe-image=<image>
```

`/etc/dmi_mig_config` is mounted writable for MIG registry operations, and `--probe-image` is
required to measure slice rows ([Container-probe tier](#container-probe-tier)).

### Iluvatar

```bash
docker run --rm --privileged --network=host \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  -v /usr/local/corex:/usr/local/corex:ro \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=iluvatar
```

### MetaX

```bash
docker run --rm --privileged --network=host \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  -v /opt/mxdriver:/opt/mxdriver:ro -v /opt/maca:/opt/maca:ro \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=metax
```

### Moore Threads

```bash
docker run --rm --privileged --network=host --runtime mthreads \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=mthreads
```

The `mthreads` container runtime injects user-space libraries (`nerdctl --runtime` expects an OCI
shim name and cannot invoke `mthreads` directly).

### NVIDIA

```bash
docker run --rm --privileged --network=host --runtime nvidia \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=nvidia
```

Pass `--runtime nvidia` on `docker` or `--gpus all` on `nerdctl` (running under `runc` hides NVML
and reports `accelerators: 0` with exit code `1`).

### T-Head

```bash
docker run --rm --privileged --network=host \
  -v /:/host -v /dev:/dev -v /sys:/sys \
  -v /usr/local/PPU_SDK:/usr/local/PPU_SDK:ro \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight --manufacturer=thead --probe-image=<image>
```

Supply `--probe-image` carrying the workload PPU SDK to measure slice rows.

### All manufacturers

```bash
docker run --rm --network=host -v /:/host \
  gpustack/gpustack-operator:<tag> \
  gpustack-operator device-manager preflight
```

Without vendor mounts, `preflight` cross-checks host CLIs on NVIDIA, Ascend, and AMD and prints the
required vendor mounts for detected hardware.

### Running Pod checks

Run `detect` or dry-run `preflight` inside an installed `gpustack-operator-device-manager-<manufacturer>`
`Pod`:

```bash
kubectl exec -n gpustack-system <device-manager-pod> -- gpustack-operator device-manager detect
kubectl exec -n gpustack-system <device-manager-pod> -- \
  gpustack-operator device-manager preflight --manufacturer=<manufacturer>
```

The `DaemonSet` `Pod` omits `/:/host`, so `preflight` enters dry-run mode automatically. It runs
detection, `device-health`, and read-only driver checks, emits container commands, and leaves the
node untouched.

## Mounts

| Mount | Purpose |
|---|---|
| `-v /:/host` | Enters host root via `chroot` to run the host container and vendor CLIs and stage preload libraries. |
| `--network=host` | Provides host DNS resolution when pulling probe images inside `chroot`. |
| `-v /dev:/dev`, `-v /sys:/sys` | Supplies device nodes and sysfs topology for in-process detection and driver reads. |
| Vendor mount or `--runtime` | Loads user-space vendor management libraries into the `preflight` process. |

## Flags

| Flag | Effect |
|---|---|
| `--manufacturer` | Comma-separated manufacturers to check (defaults to all). |
| `--no-pci-check` | Skips PCI presence filtering during detection. |
| `--dry-run` | Prints probe steps without writing host files, toggling driver modes, or starting containers. |
| `--probe-image` | Overrides the probe container image (required on Hygon and T-Head, or for air-gapped hosts). |
| `--host-root` | Host root mount path inside the container (default `/host`). |
| `--runtime` | Overrides CRI auto-detection (`docker`, `nerdctl`, or `ctr`; missing `/host/proc` requires explicit `--runtime`). |

By default, `preflight` reads the container runtime from kubelet CRI flags or
`/var/lib/kubelet/{kubeadm-flags.env,config.yaml}`. Otherwise it falls back to `docker`, `nerdctl`,
or `ctr`.

## Side effects

On measured tiers, `preflight` runs one 50%-slice probe per card, then two concurrent slices to
verify co-tenancy. Containers carry label `gpustack.ai/preflight=true` (containerd namespace
`gpustack-preflight`) and are swept before and after each run.

| Host write | Path | Lifecycle |
|---|---|---|
| Preload libraries | `/var/lib/gpustack/operator/lib/<manufacturer>` | Kept on disk (matches Device Manager init-container staging). |
| Per-probe configs | `/var/lib/gpustack/operator/preflight/` | Removed at pass completion (isolated from live `.../operator/pods` state). |
| Process lock | `/var/lib/gpustack/operator/preflight/lock` | File stays; kernel `flock` releases automatically on process exit. |
| Driver sharing flag | Ascend / Cambricon driver state | Restored immediately within the same pass. |

## Emitted steps

Container probes skip execution under `--dry-run`, missing `/host`, read-only library paths, missing
runtimes, `ctr`, or `nerdctl` on Ascend. In those cases, `preflight` prints the full command
(`emitted`) without failing the exit code.

## Output by manufacturer

Each accelerator reports only the modes it currently offers (for example, a MIG-enabled GPU reports
partition checks and omits logical-slice rows).

| Tier | Manufacturers | Driver check | Capability checks | Deepest answer |
|---|---|---|---|---|
| **Full** | NVIDIA, Ascend, AMD, T-Head | Yes | Real container (`sidecar-visibility` simulated) | `measured` |
| **Container probe** | Hygon | No (`note` explains why) | Real container (`sidecar-visibility` simulated) | `measured` |
| **Driver + injection** | Cambricon, MetaX | Yes | Allocator injection only | `simulated` |
| **Injection only** | Iluvatar, MThreads | No (`note` explains why) | Allocator injection only | `simulated` |

### Injection-only tier

Iluvatar and Moore Threads enforce slicing in the kernel module and container runtime rather than a
user-space flag. `preflight` verifies allocator injection (`simulated`) and records an explanatory
`note`.

### Container-probe tier

Hygon runs real probe containers (`measured`) with `--probe-image` (any glibc image carrying `sh`,
`cat`, `grep`, `awk`, `mkdir`, `sleep`, and `kill`). The probe runs `/opt/dtk` `BandwidthTest` and
reads `kfd` vgpu sysfs because `hy-smi` always prints full physical VRAM.

### Vendor capability matrix

| Manufacturer | Detection API | Host cross-check | Driver check | Target mode | Container probe |
|---|---|---|---|---|---|
| NVIDIA | NVML | `nvidia-smi -L` | `mig-partitioning` | `partitioned` | Yes |
| Ascend | DCMI | `npu-smi info -l` | `container-share` | `sliced` | Yes |
| AMD | RSMI, AMDSMI, HSA | `rocm-smi --showuniqueid` | `cu-mask-topology` | `sliced` | Yes |
| T-Head | HGML | — | `mig-partitioning` | `partitioned` | Yes |
| Cambricon | CNDev | — | `smlu-mode` | `sliced` | — |
| MetaX | MXSML, sGPU sysfs | — | `sgpu-mode` | `sliced` | — |
| Hygon | RSMI, AMDGPU, HSA | — | — | `sliced` / `partitioned` | Yes |
| Iluvatar | IXML | — | — | `sliced` | — |
| MThreads | MTML | — | — | `sliced` | — |

Compare vendors by the report's `mode` field rather than vendor-specific capability names.

### Driver checks

| Check | `ok` meaning | `not-declared` meaning | `unavailable` meaning |
|---|---|---|---|
| `mig-partitioning` | Physical partition profile declared | MIG off or unsupported on card | Driver query failed |
| `container-share` | Flag on, or toggled on and restored | Generation has no flag | DCMI missing or refused |
| `cu-mask-topology` | HSA topology and CU atom validated | — | HSA init failed or card missing |
| `smlu-mode` | Mode on, or toggled on and restored | Card advertises no sMLU capacity | CNDev missing sMLU API or refused |
| `sgpu-mode` | sGPU sysfs registry readable | — | Registry unreadable |

### Ascend A5 host checks

On Atlas A5 (`950`) NPUs, `preflight` adds two node-level `declared` checks on every card:

| Check | Reads | `ok` meaning | `unavailable` meaning |
|---|---|---|---|
| `ascend-docker-runtime` | `/usr/local/Ascend` version | MindCluster `26.0.0+` | Older or unreadable runtime |
| `hccl-ranktable` | `/host/etc/hccl_rootinfo.json` | Absent or JSON version `2.0` | Wrong version, symlink, invalid JSON, or missing `/host` |

See [Vendor Prerequisites](../../getting-started/vendor-prerequisites.md#ascend) for remediation.

### Slice probes

Each slice probe requests **50%** of one card (so a `24564 MiB` card reports `12282 MiB` inside the
probe):

| Manufacturer | Default probe image | Vendor runtime | Probe reader | Cap source |
|---|---|---|---|---|
| NVIDIA | `nvidia/cuda:<CUDA major>` | `--runtime nvidia` / `--gpus all` | `nvidia-smi --query-gpu=memory.total` | `CUDA_DEVICE_MEMORY_LIMIT_<i>` |
| Ascend | `quay.io/ascend/cann:<CANN major>-<family>` | `--runtime ascend` (`docker`) | `enpu-monitor` | `npu_info.config` (`memory-quota`) |
| AMD | `ubuntu:24.04` | None | `rocm-monitor` | `VROCM_DEVICE_MEMORY_LIMIT_<i>` |
| T-Head | Required via `--probe-image` | None | `ppu-monitor` | `HGGC_DEVICE_MEMORY_LIMIT_<i>` |
| Hygon | Required via `--probe-image` | None | `BandwidthTest` + `kfd` vgpu sysfs | `vdev0.conf` (`mem`) |

Capability rows report `sliced-runtime-loaded` (shim or vgpu active), `sliced-quota-in-force` (50%
VRAM cap), `sidecar-visibility` (`simulated` SSH grant), and `co-tenancy` (two slices on one card).

## Result format

`preflight` writes one YAML report to `stdout` with `accelerators`, `network`
([NIC record inspection](../rdma/operations.md#nic-record-inspection)), and `topology` (configured
kubelet `topologyManagerPolicy`, or `unknown` when unset):

```yaml
accelerators:
  - manufacturer: nvidia
    detection:
      state: ok
      depth: declared
      accelerators: 8
    checks: []
network:
  timestamp: "2026-10-29T00:00:00Z"
  checks: []
topology:
  timestamp: "2026-10-29T00:00:00Z"
  policy: single-numa-node
  depth: declared
```

### States and depths

| State | Meaning | Exit code |
|---|---|---|
| `ok` | Capability verified at reported depth (`declared`, `simulated`, or `measured`) | `0` |
| `not-declared` | Hardware does not offer this capability | `0` |
| `unavailable` | Offered capability, detection, lock, crash guard, or stale sweep failed | Non-zero |

Broken RDMA links and `topology: unknown` or `none` do **not** fail the exit code because accelerator
allocation still works. Concurrent runs fail closed on `/var/lib/gpustack/operator/preflight/lock`,
vendor panics report `preflight-panicked`, and failed sweeps report `stale-container-sweep` (clean
via `gpustack.ai/preflight=true` or `gpustack-preflight`).

### Unhealthy accelerators

Uncorrectable ECC or hardware faults (`status.unhealthy: true`) keep `detection` `ok` (`NVIDIA A100 x8 (1 unhealthy)`)
while healthy cards run full checks. Each faulty card emits `capability: device-health`
(`state: unavailable`, non-zero exit) because the device plugin withholds unhealthy cards.

---

**See also** — [Vendor Prerequisites](../../getting-started/vendor-prerequisites.md) (host drivers and runtimes per vendor) · [Accelerator Requests](requests.md) (resource keys and admission rules) · [RDMA Operations](../rdma/operations.md) (kubelet `TopologyManager` setup and NIC inspection)

**Next** → [NVIDIA MIG Operations](nvidia-mig.md) — enable and operate hardware MIG partitions.
