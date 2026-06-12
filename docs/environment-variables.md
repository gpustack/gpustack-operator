# Environment Variables

This is the reference of every environment variable read by GPUStack Operator binaries. All of them are read once at process startup, so changing a value requires restarting the affected component.

A deployment-friendly property worth knowing up front: when the Worker (WK) creates the Device Manager (DM) DaemonSets, it copies every `GPUSTACK_`-prefixed environment variable from its own Pod spec onto the DM containers. Setting a `GPUSTACK_*` variable on the Worker Deployment is therefore enough — it propagates to the DMs automatically.

## Configuration Knobs

| Variable                                       | Default             | Component | Effect                                                                                                                                                                                                                                                                                                                                                       |
|------------------------------------------------|---------------------|-----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `GPUSTACK_DATA_DIR`                            | `/var/lib/gpustack` | all       | Root directory for data storage.                                                                                                                                                                                                                                                                                                                             |
| `GPUSTACK_CONF_DIR`                            | `/etc/gpustack`     | all       | Root directory for configuration and metadata, e.g. bundled Helm charts.                                                                                                                                                                                                                                                                                     |
| `GPUSTACK_PCI_CLASS_PREFIXES`                  | `02,03,0b,12`       | WK + DM   | Comma-separated PCI class prefixes treated as display/accelerator devices (see the [PCI class registry](https://admin.pci-ids.ucw.cz/read/PD)). Read in two places with identical parsing: the WK injects it into the NFD chart's `deviceClassWhitelist` and the acceleratable-detection NodeFeatureRule, and the DM applies it to its local sysfs PCI scan. |
| `GPUSTACK_DEVICES_DETECT_GROUP_ID_WITH_MEMORY` | `false`             | DM        | When `true`, the device group ID gains a memory-size suffix (e.g. `nvidia-tesla-t4-16g` instead of `nvidia-tesla-t4`), so same-model devices with different VRAM sizes form distinct groups.                                                                                                                                                                 |

## Per-Manufacturer Overrides

Three override patterns are expanded for every known manufacturer (`amd`, `ascend`, `cambricon`, `hygon`, `iluvatar`, `metax`, `mthreads`, `nvidia`, `thead`). They are read by both the WK and the DM, so the WK-to-DM propagation described above keeps the two sides consistent.

- `GPUSTACK_${MANUFACTURER}_PCI_VENDOR_ID` — overrides the PCI vendor ID used for NFD node selection and device scanning. Accepts either `${vendor}` or `${class}_${vendor}`.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RESOURCE_NAME` — overrides the extended resource name the scheduling chain allocates against.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RUNTIME_NAME` — overrides the container runtime class name used for accelerated workloads.

Defaults:

| Manufacturer | PCI vendor ID | Resource name          | Runtime name |
|--------------|---------------|------------------------|--------------|
| `amd`        | `1002`        | `amd.com/gpu`          | `amd`        |
| `ascend`     | `19e5`        | `huawei.com/npu`       | `ascend`     |
| `cambricon`  | `cabc`        | `cambricon.com/mlu`    | `cambricon`  |
| `hygon`      | `1d94`        | `hygon.com/dcu`        | `hygon`      |
| `iluvatar`   | `1e3e`        | `iluvatar.com/gpu`     | `iluvatar`   |
| `metax`      | `9999`        | `metax-tech.com/gpu`   | `metax`      |
| `mthreads`   | `1ed5`        | `mthreads.com/gpu`     | `mthreads`   |
| `nvidia`     | `10de`        | `nvidia.com/gpu`       | `nvidia`     |
| `thead`      | `1ded`        | `alibabacloud.com/ppu` | — (none)     |

> T-Head has no default runtime name, but `GPUSTACK_THEAD_ACCELERATABLE_RUNTIME_NAME` is still honored and can supply one.

## Vendor Toolkit Paths

The DM device bindings locate vendor libraries through conventional toolkit-home variables. Each falls back to the listed default directory when unset.

| Variable                      | Default                                                                                     | Manufacturer | Effect                                                                                            |
|-------------------------------|---------------------------------------------------------------------------------------------|--------------|---------------------------------------------------------------------------------------------------|
| `ROCM_HOME`, then `ROCM_PATH` | `/opt/rocm`                                                                                 | AMD          | ROCm root, searched for `librocm_smi64.so` / `libamd_smi.so` / `libhsa-runtime64.so`.             |
| `ROCM_SMI_LIB_PATH`           | —                                                                                           | AMD          | Extra directory searched for `librocm_smi64.so` before the ROCm root.                             |
| `AMD_SMI_LIB_PATH`            | —                                                                                           | AMD          | Extra directory searched for `libamd_smi.so` before the ROCm root.                                |
| `CANN_HOME`                   | `/usr/local/Ascend`                                                                         | Ascend       | Driver root, searched for `libdcmi.so`.                                                           |
| `ASCEND_TOOLKIT_HOME`         | `/usr/local/Ascend/cann`, falling back to `/usr/local/Ascend/ascend-toolkit/latest/runtime` | Ascend       | CANN toolkit root used by the Ascend detector.                                                    |
| `NEUWARE_HOME`                | `/usr/local/neuware`                                                                        | Cambricon    | Neuware root, searched for `libcndev.so`.                                                         |
| `PPU_HOME`                    | `/usr/local/PPU_SDK`                                                                        | Hygon        | PPU SDK root, searched for `libhgml.so`.                                                          |
| `COREX_HOME`                  | `/usr/local/corex`                                                                          | Iluvatar     | CoreX root, searched for `libixml.so`.                                                            |
| `MACA_HOME`                   | `/opt/maca`                                                                                 | MetaX        | MACA root, searched for `libmxsml.so`.                                                            |
| `LD_LIBRARY_PATH`             | —                                                                                           | all          | Standard library search path, consulted as an additional source of candidate library directories. |

## Kubernetes-Injected Variables

These are populated by the Pod specs that GPUStack Operator itself renders (Downward API or Service environment). They are not user-facing knobs — listed here only for completeness.

| Variable                   | Default                    | Effect                                                                                                                              |
|----------------------------|----------------------------|-------------------------------------------------------------------------------------------------------------------------------------|
| `KUBERNETES_NODE_NAME`     | — (required)               | Name of the node the Pod runs on; the DM uses it to name its NodeFeature/Devices objects.                                           |
| `KUBERNETES_POD_NAME`      | —                          | The WK's own Pod name, used to read back its container spec (image, pull policy, `GPUSTACK_*` env) for rendering the DM DaemonSets. |
| `KUBERNETES_POD_NAMESPACE` | `gpustack-system`          | System namespace where managed resources live.                                                                                      |
| `KUBERNETES_POD_IP`        | —                          | Overrides the auto-detected primary host IP in topology discovery.                                                                  |
| `KUBERNETES_SERVICE_NAME`  | `gpustack-operator-worker` | Service name used for system routing.                                                                                               |
| `KUBERNETES_SERVICE_HOST`  | —                          | Standard in-cluster marker; its presence tells the embedded runner it is inside a cluster.                                          |

## Proxy and Internal Flags

| Variable                                                | Default | Effect                                                                                                                                              |
|---------------------------------------------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `ALL_PROXY` / `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | —       | Standard proxy settings, passed through to the embedded Kubernetes installer.                                                                       |
| `NO_PROXY` / `no_proxy`                                 | —       | Also parsed (hosts, IPs, CIDRs) to bypass the proxy on direct HTTP calls.                                                                           |
| `_RUNNING_INSIDE_CONTAINER_`                            | `false` | Internal marker baked into the container image; switches data/conf paths to their absolute in-container locations. Not intended to be set by users. |
