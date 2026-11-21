# Vendor Prerequisites

Install the manufacturer driver and any required container toolkit before installing GPUStack. If a
vendor GPU Operator runs on the same nodes, disable competing device plugins, node labellers, and
duplicate NFD instances first. Vendor chart keys vary by release, so check the version cited in each
section.

## Contents

- [Third-party dependencies](#third-party-dependencies)
- [Vendor setup requirements](#vendor-setup-requirements)
- [Shared cluster requirements](#shared-cluster-requirements)

## Third-party dependencies

Review these cluster dependencies before installing GPUStack. Ownership rules for chart mode and
image mode are in [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md).

| Dependency | Role | Configuration |
|---|---|---|
| Node Feature Discovery (NFD) | Labels `Node`s and publishes hardware features. | Run one NFD master per cluster. See [Single NFD per cluster](#single-nfd-per-cluster). |
| Kueue | Manages workload quota and gang admission. | Bundled in the chart; see [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md). |
| Topograph | Publishes topology domains from a provider. | Optional and disabled by default. See [Topology-Aware Scheduling Operations](/gpustack-operator/main/docs/modules/topology/operations/index.md). |
| Mooncake | Provides KV cache storage and transfer transport. | Optional. Match engine, client, store, and transport in [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md). |
| ModelExpress | Coordinates NVIDIA peer weight transfer and cache sharing. | Optional; the operator creates a per-deployment metadata service. See [ModelExpress](/gpustack-operator/main/docs/modules/model-delivery/remote/index.md#nvidia-modelexpress). |
| CSI drivers | Mount NFS or S3 volumes, or mount cached weights. | NFS and S3 ship as bundled subcharts; node model delivery uses Model Manager CSI ([Model Store Operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md)). |
| Serving engines | Run inference containers. | Select a compatible runner image from [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md). |
| Vendor drivers and toolkits | Expose accelerators to node workloads. | Install host prerequisites from [Vendor setup requirements](#vendor-setup-requirements). |

## Vendor setup requirements

### AMD

Checked against **AMD GPU Operator v1.5.1** ([Overview](https://instinct.docs.amd.com/projects/gpu-operator/en/latest/index.html) ·
[Helm Chart Install](https://instinct.docs.amd.com/projects/gpu-operator/en/latest/installation/kubernetes-helm.html) ·
[Toolkit Install](https://instinct.docs.amd.com/projects/container-toolkit/en/latest/index.html)).

**Install before GPUStack** — install only the ROCm kernel driver. Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack injects `/dev/kfd` and each granted card's DRM
nodes directly, so neither AMD Container Toolkit nor `amd-container-runtime` is needed.

If a workload runs as non-root, add `video` and `render` to its container *`securityContext`*
supplementary groups. Kubernetes device plugins cannot grant supplementary groups. Without those
groups, ROCm enumerates zero agents without raising an error; verify inside the container with
`rocminfo` rather than sysfs-based `rocm-smi`.

**Vendor GPU Operator** — AMD GPU Operator reconciles a `DeviceConfig` custom resource created by its
Helm chart. Disable its device plugin and node labeller on `DeviceConfig`, and disable bundled NFD
and Kernel Module Management (when drivers are pre-installed) in Helm values:

```yaml
apiVersion: amd.com/v1alpha1
kind: DeviceConfig
metadata:
  name: default
  namespace: kube-amd-gpu
spec:
  driver:
    enable: true  # default: true
  metricsExporter:
    enable: true  # default: false
  devicePlugin:
    enableDevicePlugin: false  # disable: conflicts with GPUStack amd.com/gpu plugin
    enableNodeLabeller: false  # disable: GPUStack publishes capacity labels
  draDriver:
    enable: true  # optional: mutually exclusive with devicePlugin on one DeviceConfig
```

```yaml
node-feature-discovery:
  enabled: false  # disable: run one NFD per cluster
kmm:
  enabled: false  # disable when ROCm driver is pre-installed on the host
  watch: false
```

**Install it** — install the chart with NFD and KMM disabled, then patch the chart-created
`DeviceConfig`:

```bash
helm repo add rocm https://rocm.github.io/gpu-operator
helm repo update
helm install amd-gpu-operator rocm/gpu-operator-charts \
  --namespace kube-amd-gpu --create-namespace \
  --version=v1.5.1 \
  --set node-feature-discovery.enabled=false \
  --set kmm.enabled=false \
  --set kmm.watch=false

kubectl -n kube-amd-gpu patch deviceconfig <name> --type merge -p \
  '{"spec":{"devicePlugin":{"enableDevicePlugin":false,"enableNodeLabeller":false},"metricsExporter":{"enable":true}}}'
```

The patch applies this change to the existing `DeviceConfig`:

```diff
 apiVersion: amd.com/v1alpha1
 kind: DeviceConfig
 metadata:
   name: <name>
   namespace: kube-amd-gpu
 spec:
   devicePlugin:
-    enableDevicePlugin: true
-    enableNodeLabeller: true
+    enableDevicePlugin: false
+    enableNodeLabeller: false
   metricsExporter:
-    enable: false
+    enable: true
```

**Already installed** — both plugins advertise `amd.com/gpu`. Stop the vendor plugin before
installing GPUStack:

1. **Device plugin and Node Labeller**: patch the existing `DeviceConfig` as shown above. Do not
   delete the `DaemonSet` directly, because the operator recreates it from `DeviceConfig`.
2. **Bundled NFD**: if the release enabled NFD, run `helm upgrade` with its existing values plus
   `--set node-feature-discovery.enabled=false`.
3. **Verify**: wait until `kubectl get node <node> -o jsonpath='{.status.allocatable}'` no longer
   lists `amd.com/gpu` before installing GPUStack.

### Ascend

Checked against the **Ascend NPU driver and firmware** ([Driver Install](https://www.hiascend.com/hardware/firmware-drivers))
and [`ascend-docker-runtime`](https://www.hiascend.com/eng/developer/software/mindcluster) **26.1.0**
on Atlas A5 (`950` series).

**Install before GPUStack** — install the Ascend NPU driver, firmware, and Ascend Docker Runtime. Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack sets `ASCEND_VISIBLE_DEVICES` without `/dev`
nodes, so the runtime hook must mount the devices. Register the runtime under handler `ascend` (matching
the chart's `RuntimeClass`), or set `deviceManager.createRuntimeClasses=false` and supply your own.

On Atlas A5 (`950`), install `ascend-docker-runtime` newer than `26.0.x`[^1]. Releases `7.3.x` and
earlier support 910B and older generations only. On A5, an older runtime does not recognize
`Ascend950`, falls back to legacy manager devices, and fails container creation on missing
`/dev/devmm_svm`.

[^1]: Basic Atlas A5 support first shipped in `ascend-docker-runtime` `26.0.0` (noted as Atlas 350 PCIe support), after version numbers jumped directly from `7.3.x` to `26.x`.

Do not rely on `ascend-docker-runtime --version`; the wrapper forwards `--version` to host `runc`.
Read the installer metadata file instead:

```bash
grep '^version=' /usr/local/Ascend/Ascend-Docker-Runtime/ascend_docker_runtime_install.info
# version=v7.3.0    <- predates A5 support
# version=v26.1.0   <- serves A5
```

`gpustack-operator device-manager preflight` checks the same file and reports it as
[`ascend-docker-runtime`](/gpustack-operator/main/docs/modules/devices/preflight/index.md#ascend-a5-host-checks) on each A5 NPU.

**A5 host ranktable** — on Atlas A5 (`950`), `/etc/hccl_rootinfo.json` must be absent (default) or at
version `"2.0"`. Prefer `ascend-docker-runtime` `26.2+` so the runtime does not mount it
unconditionally. If an A2 (`"1.0"`) or A3 (`"1.2"`) ranktable remains on the host, `libhccl_v2` fails
multi-card jobs with `Config_Error_Ranktable` (`EI0014`):

```bash
# absent is healthy; HCCL negotiates through rootInfo
test -e /etc/hccl_rootinfo.json && grep -o '"version"[^,]*' /etc/hccl_rootinfo.json
# "version": "1.0"   <- breaks multi-card HCCL on A5; remove or regenerate as 2.0
```

Preflight reports this state as [`hccl-ranktable`](/gpustack-operator/main/docs/modules/devices/preflight/index.md#ascend-a5-host-checks).

**A5 conditional host mounts** — on Ascend A5 (`status.detail.family` `"950"`), `device-manager` inspects `/etc/hccl_rootinfo.json` (regular non-symlink file `<= 1 MiB` at version `"2.0"`) and `/lib/route.conf` (regular non-symlink file) through `/host` during `Allocate` and mounts valid files read-only.
Absent, symlinked, invalid, or colliding paths are skipped cleanly, non-A5 hosts never receive either mount, and [`deviceManager.hostRootMount.enabled=false`](#device-manager-host-mount) disables both.

**A5 Host RoCE endpoints** — for Ascend A5 *Host RoCE* (`roce:host`) `vLLM` workloads, create per-NPU
endpoint files at `/etc/hixlep/ub_endpoint_npu_<physical_id>.json` on every Host RoCE node. Managed
Host RoCE `Pod`s mount `/etc/hixlep` read-only (`HostPathDirectory`); a missing directory blocks
kubelet volume setup, and a missing file fails `vllm-ascend` with `FileNotFoundError`. See
[Ascend A5 topologies](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#ascend-a5-topologies).

### Cambricon

Checked against **Cambricon driver 5.10.22** (Neuware SDK 1.15.0,
[Driver Install](https://www.cambricon.com/docs/sdk_1.15.0/driver_5.10.22/user_guide/index.html)).

**Install before GPUStack** — install only the Cambricon MLU driver. Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack injects card and host control `/dev` nodes on
both whole-card and sliced allocations and sets `CAMBRICON_VISIBLE_DEVICES` for compatibility with
`cambricon-container-runtime`, so `cntoolkit` is not required.

### Hygon

Checked against **Hygon DCU Operator** (`latest` unversioned vendor docs,
[Overview](https://developer.sourcefind.cn/document/7541efc4-54b1-11f1-8265-0242ac150003)).

**Install before GPUStack** — install the Hygon DCU driver and firmware so `/dev/kfd`, `/dev/mkfd`,
`/dev/dri/card*`, and `/dev/dri/renderD*` exist on the host. Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack injects those device nodes directly without a
container toolkit.

**Vendor GPU Operator** — Hygon DCU Operator configures all components on one `DeviceConfig` custom
resource (`kubectl explain deviceconfig.spec`). Substitute the cluster's installed `apiVersion` and
object name:

```yaml
apiVersion: <existing-api-version>
kind: DeviceConfig
metadata:
  name: device-config
  namespace: kube-system
spec:
  metricsExporter:
    enable: true  # default: false
  dcgm:
    enable: true  # default: false
  devicePlugin:
    enableDevicePlugin: false  # disable: conflicts with GPUStack hygon.com/dcu plugin
    enableNodeLabeller: false  # disable: DCU-Label-Node conflicts with GPUStack labels
```

**Install it** — after installing the operator from Hygon's portal, patch its `DeviceConfig`:

```bash
kubectl -n kube-system patch deviceconfig <name> --type merge -p \
  '{"spec":{"devicePlugin":{"enableDevicePlugin":false,"enableNodeLabeller":false},"metricsExporter":{"enable":true},"dcgm":{"enable":true}}}'
```

Read `apiVersion` and `metadata.name` from `kubectl get deviceconfig -n kube-system -o yaml`:

```diff
 apiVersion: <existing-api-version>
 kind: DeviceConfig
 metadata:
   name: <name>
   namespace: kube-system
 spec:
   devicePlugin:
-    enableDevicePlugin: <current-value>
+    enableDevicePlugin: false
-    enableNodeLabeller: <current-value>
+    enableNodeLabeller: false
   metricsExporter:
-    enable: <current-value>
+    enable: true
   dcgm:
-    enable: <current-value>
+    enable: true
```

**Already installed** — both plugins advertise `hygon.com/dcu`. Stop the vendor plugin first:

1. **Device plugin and DCU-Label-Node**: apply the `DeviceConfig` patch above. Do not delete the
   `DaemonSet`s directly.
2. **Verify**: wait until `hygon.com/dcu` disappears from `Node` allocatable capacity before
   installing GPUStack.

### Iluvatar

Checked against **ix-GPU-Operator v4.4.0** and **ix-Container-Toolkit v1.1.0**
([Helm Chart Install](https://developer.iluvatar.com/docs/generaldocs_ix_gpu_operator?category=%E4%BA%91%E5%8E%9F%E7%94%9F%E4%B8%8E%E9%9B%86%E7%BE%A4%E9%83%A8%E7%BD%B2&manual=generaldocs_k8stools_all) ·
[Toolkit Install](https://developer.iluvatar.com/docs/generaldocs_ix_container_toolkit?category=%E4%BA%91%E5%8E%9F%E7%94%9F%E4%B8%8E%E9%9B%86%E7%BE%A4%E9%83%A8%E7%BD%B2&manual=generaldocs_k8stools_all)).

**Install before GPUStack** — install matching versions of the Iluvatar driver and
ix-Container-Toolkit (for example, driver v4.4.0 with toolkit v1.1.0). Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack sets `IX_VISIBLE_DEVICES` without `/dev` nodes.
Register the runtime as `iluvatar`, or set `deviceManager.createRuntimeClasses=false` and supply your
own `RuntimeClass`.

**Vendor GPU Operator** — ix-GPU-Operator exposes one Helm value per component:

```yaml
ixDriver:
  enabled: false  # default: false; keep off when driver is pre-installed
ixExporter:
  enabled: true  # default: true
ixDevicePlugin:
  enabled: false  # disable: conflicts with GPUStack iluvatar.com/gpu plugin
ixfd:
  enabled: false  # disable: bundles upstream NFD
```

**Install it** — install the OCI chart with the competing components disabled:

```bash
helm install --wait ix-gpu-operator \
  oci://registry.iluvatar.com.cn:10443/k8s/ix-gpu-operator --version 1.0.0 \
  --create-namespace --namespace ix-gpu-operator \
  --insecure-skip-tls-verify \
  --set global.repository="registry.iluvatar.com.cn/k8s" \
  --set global.version=4.4.0 \
  --set ixDriver.enabled=false \
  --set ixDevicePlugin.enabled=false \
  --set ixfd.enabled=false
```

**Already installed** — both plugins advertise `iluvatar.com/gpu`. Stop ix-Device-Plugin first:

1. **ix-Device-Plugin and ix-Feature-Discovery**: run `helm upgrade` with existing values plus
   `--set ixDevicePlugin.enabled=false --set ixfd.enabled=false`.
2. **Verify**: wait until `iluvatar.com/gpu` leaves `Node` allocatable capacity before installing
   GPUStack.

On software stacks older than v4.3.0, ix-GPU-Operator ships as static YAML without component switches.
Delete that manifest and install the host driver and ix-Container-Toolkit directly.

### MetaX

Checked against **MetaX Kubernetes GPU Operator v0.15.3**
([Overview](https://developer.metax-tech.com/api/client/document/preview/1411/k8s/00_overview.html) ·
[Helm Chart Install](https://developer.metax-tech.com/api/client/document/preview/1411/k8s/02_start.html) ·
[Components](https://developer.metax-tech.com/api/client/document/preview/1411/k8s/03_component.html)).

**Install before GPUStack** — install the MetaX driver and MXMACA SDK on the host. Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack injects `/dev/mxcd`, `/dev/mxnd`, `/dev/mxgd`,
and card DRM nodes directly, so no container toolkit is required.

**Vendor GPU Operator** — MetaX GPU Operator marks `gpu-device` and `gpu-label` mandatory in every
mode, including `minimalMode`:

| Component | Action | Mechanism |
|---|---|---|
| Kernel driver and MXMACA SDK | Keep on host | `--set minimalMode=true` leaves both to the host |
| `mx-exporter` | Optional | Optional under `minimalMode` |
| `controller-exporter` | Optional | Optional under `minimalMode` |
| `gpu-device` (device plugin) | **Cannot disable** | Mandatory in every vendor install mode |
| `gpu-label` (NFD-equivalent) | **Cannot disable** | Mandatory in every vendor install mode |
| `gpu-scheduler` | Disable | Leave undeployed |
| `topo-discovery` | Disable | Leave undeployed |

**Install it** — `minimalMode` is fixed at install time and only fits nodes that GPUStack does **not**
manage:

```bash
helm install oci://DOMAIN/PROJECT/metax-operator \
  --version VERSION \
  --create-namespace -n metax-operator \
  --generate-name \
  --wait \
  --set registry=DOMAIN/PROJECT \
  --set minimalMode=true
```

**Already installed** — because `gpu-device` cannot be disabled and advertises `metax-tech.com/gpu`,
the vendor operator and GPUStack cannot share a node:

1. **Host driver and MXMACA SDK**: confirm both are installed on the host outside the operator so
   uninstalling the chart does not remove them.
2. **Uninstall release**: run `helm uninstall` on the MetaX operator release.
3. **Verify**: wait until `metax-tech.com/gpu` leaves `Node` allocatable capacity before installing
   GPUStack.

### Moore Threads

Checked against **MT GPU Operator v2.1.0** (KUAE Cloud Native,
[Overview](https://docs.mthreads.com/cloud-native/cloud-native-doc-online/introduction/) ·
[Helm Chart Install](https://docs.mthreads.com/cloud-native/cloud-native-doc-online/install_guide#mt-gpu-operator)).

**Install before GPUStack** — install the Moore Threads driver and MT Container Toolkit. Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack sets `MTHREADS_VISIBLE_DEVICES` without `/dev`
nodes.

**Vendor GPU Operator** — MT GPU Operator ships two modes: `full` (two Helm charts whose controller
always reconciles the vendor device plugin with no disable switch) and `core` (plain YAML manifests
containing only the device plugin and exporter):

```yaml
mtGpuClusterPolicyClusterpolicy:
  driverToolkit:
    enabled: true  # default: true
  containerToolkit:
    enabled: true  # default: true
  dcgm:
    enabled: true
  dcgmExporter:
    enabled: true
  nfd:
    enabled: false  # disable: run one NFD per cluster
  gpuScheduler:
    enabled: false  # default: false
  gpuWebhook:
    enabled: false  # default: false
  kured:
    enabled: false
  nodeProblemDetector:
    enabled: false
  aiOps:
    enabled: false
```

**Install it** — `full` mode below suits nodes left to the vendor plugin; for nodes managed by
GPUStack, install host drivers and MT Container Toolkit directly and apply `core` manifests with the
device plugin removed:

```bash
helm install --wait mt-gpu-operator mt-gpu-operator
helm install --wait mt-gpu-operator-custom-resources mt-gpu-operator-custom-resources \
  --set mtGpuClusterPolicyClusterpolicy.nfd.enabled=false \
  --set mtGpuClusterPolicyClusterpolicy.kured.enabled=false \
  --set mtGpuClusterPolicyClusterpolicy.nodeProblemDetector.enabled=false \
  --set mtGpuClusterPolicyClusterpolicy.aiOps.enabled=false
```

**Already installed** — both plugins advertise `mthreads.com/gpu`. Stop the vendor plugin before
installing GPUStack:

1. **Cluster add-ons**: in `full` mode, upgrade `mt-gpu-operator-custom-resources` with the `false`
   flags above (`core` mode deploys none of them).
2. **Device plugin**:
   - **`full` mode**: uninstall both Helm charts, ensure the driver and MT Container Toolkit remain on
     the host, and switch to `core` mode if you still want the exporter.
   - **`core` mode**: remove the device plugin from `deployments.yaml`, keep the exporter, and
     re-apply.
3. **Verify**: wait until `mthreads.com/gpu` leaves `Node` allocatable capacity before installing
   GPUStack.

### NVIDIA

Checked against **NVIDIA GPU Operator v26.3.3**
([Overview](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/overview.html) ·
[Helm Chart Install](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html)).

**Install before GPUStack** — install the NVIDIA driver and NVIDIA Container Toolkit (either on the
host or via NVIDIA GPU Operator). Per [Container toolkit requirements](#container-toolkit-requirements), GPUStack sets
`NVIDIA_VISIBLE_DEVICES` by default or emits CDI annotations; both paths require the toolkit.

Device Manager also relies on the toolkit for MIG management (`NVIDIA_MIG_CONFIG_DEVICES` and
`NVIDIA_MIG_MONITOR_DEVICES`); see [MIG prerequisites](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#prerequisites).

**Vendor GPU Operator** — keep the driver, toolkit, DCGM exporter, and GPU Feature Discovery, and
disable components that conflict with GPUStack:

```yaml
driver:
  enabled: true  # default: true
toolkit:
  enabled: true  # default: true
dcgmExporter:
  enabled: true  # default: true
devicePlugin:
  enabled: false  # disable: conflicts with GPUStack nvidia.com/gpu plugin
gfd:
  enabled: true  # default: true
nfd:
  enabled: false  # disable, or keep and disable GPUStack bundled NFD
migManager:
  enabled: false  # disable: conflicts with Allocate-time MIG actuation
cdi:
  enabled: false  # default: true from v25.10; disable on MIG nodes
```

For the single-profile exception where fleet-wide MIG Manager can stay enabled, see
[Fleet-wide MIG Manager](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#fleet-wide-mig-manager). Also verify that
managed nodes do not carry `nvidia.com/gpu.deploy.operands=false`; that label suppresses all operands
while `ClusterPolicy` still reports `ready`.

**Install it** — install the chart with the competing components disabled:

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update
helm install --wait --generate-name \
  -n gpu-operator --create-namespace \
  nvidia/gpu-operator --version=v26.3.3 \
  --set devicePlugin.enabled=false \
  --set nfd.enabled=false \
  --set migManager.enabled=false \
  --set cdi.enabled=false
```

**Already installed** — run one `helm upgrade` preserving existing values plus the switches below (do
not delete `DaemonSet`s manually, or `ClusterPolicy` recreates them):

1. **Device plugin**: pass `--set devicePlugin.enabled=false`.
2. **MIG Manager**: pass `--set migManager.enabled=false`. Existing geometry stays intact until
   GPUStack reconfigures partitions at `Allocate` time.
3. **Bundled NFD**: pass `--set nfd.enabled=false` (GPUStack's bundled NFD already publishes the
   `system-os_release.*` labels NVIDIA's operator expects), or see
   [Single NFD per cluster](#single-nfd-per-cluster) to keep NVIDIA's NFD instead.
4. **CDI on MIG nodes**: pass `--set cdi.enabled=false` on nodes serving MIG.
5. **Verify**: wait until `nvidia.com/gpu` leaves `Node` allocatable capacity before installing
   GPUStack.

**CDI mode** — `--set cdi.enabled=false` gives GPUStack end-to-end control; leaving CDI on works only
for non-MIG whole-GPU nodes.

> **Why** — GPU Operator v25.10+ defaults `cdi.enabled: true` (ignoring deprecated `cdi.default`). In
> CDI mode, the runtime resolves `NVIDIA_VISIBLE_DEVICES` against static `/etc/cdi` or `/var/run/cdi`
> specs, which never list MIG instances created dynamically at `Allocate` time.

**Device injection channel** — leave `GPUSTACK_NVIDIA_DEVICE_INJECTION_STRATEGY` at `auto`.

> **Why** — `auto` selects `cdi-annotations` only when the `Pod` sets no *`runtimeClassName`*, the
> runtime config enables CDI on a non-vendor default handler, and CDI specs list every granted GPU;
> otherwise it falls back to `envvar` (`NVIDIA_VISIBLE_DEVICES`). When a `Pod` or `Instance` sets
> `runtimeClassName: nvidia`, that runtime handler performs injection directly.

### T-Head

Checked against **T-Head SAIL SDK** (PPU SDK, HGGCRT v3,
[Driver Install](https://developer.t-head.cn/docs_center/doc_detail/index.html?projectId=39&chapterId=194) ·
[Overview](https://developer.t-head.cn/docs_center/doc_detail/index.html?projectId=33&chapterId=118)).

**Install before GPUStack** — install the PPU SDK Runfile (`/usr/local/PPU_SDK` by default) using
HGGCRT v3 (backward compatible with v2), and verify with `ppu-smi` and `hgcc --version`. Per
[Container toolkit requirements](#container-toolkit-requirements), GPUStack injects `/dev/alixpu`, `/dev/alixpu_ctl`, and
`/dev/alixpu_ppu<N>` (plus MIG partition nodes) directly without a vendor container runtime.

## Shared cluster requirements

### Container toolkit requirements

Whether a host needs a vendor container toolkit depends on what GPUStack's allocator returns to
kubelet:

| Manufacturer | Allocator injection | Toolkit required |
|---|---|---|
| AMD | `/dev` nodes (`/dev/kfd`, `/dev/dri/card*`, `/dev/dri/renderD*`) | No |
| Ascend | `ASCEND_VISIBLE_DEVICES` | Yes |
| Cambricon | `/dev` nodes (`/dev/cambricon_dev*` and control nodes) plus `CAMBRICON_VISIBLE_DEVICES` | No |
| Hygon | `/dev` nodes (`/dev/kfd`, `/dev/mkfd`, `/dev/dri/card*`, `/dev/dri/renderD*`) | No |
| Iluvatar | `IX_VISIBLE_DEVICES` | Yes |
| MetaX | `/dev` nodes (`/dev/mxcd`, `/dev/mxnd`, `/dev/mxgd`, `/dev/dri/card*`, `/dev/dri/renderD*`) | No |
| Moore Threads | `MTHREADS_VISIBLE_DEVICES` | Yes |
| NVIDIA | `NVIDIA_VISIBLE_DEVICES` by default, or CDI annotations ([NVIDIA](#nvidia)) | Yes |
| T-Head | `/dev` nodes (`/dev/alixpu`, `/dev/alixpu_ctl`, `/dev/alixpu_ppu<N>`) | No |

> **Why** — `runc` mounts returned `/dev` nodes directly, whereas an environment variable requires a
> vendor runtime hook to mount devices. Without that toolkit, the container starts with the variable
> set and zero accelerators visible.

### Device plugin name collisions

Kubelet allows a second device plugin to overwrite an existing registration for the same resource key.
Node capacity then oscillates between both plugins, and `Allocate` calls fail when kubelet sends one
plugin's device IDs to the other. Stop competing vendor plugins before deploying GPUStack.

### Single NFD per cluster

Run exactly one NFD master per cluster. Two masters garbage-collect each other's
`feature.node.kubernetes.io/*` labels, which can remove `pci-<vendor>.present` labels and unschedule
Device Manager `DaemonSet`s.

GPUStack's bundled NFD evaluates cluster-scoped `NodeFeatureRule`s (such as AMD's) and publishes
`system-os_release.*` labels (required by NVIDIA GPU Operator).

**Bringing your own NFD** — disable GPUStack's bundled NFD with
`--set node-feature-discovery.enabled=false` in chart mode or
`--disable-applications=node-feature-discovery` in image mode
([Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md#chart-and-image-modes)). The worker still
applies `gpustack-cpu-info`, so the existing NFD installation must satisfy six rules:

1. Enable `pci` worker discovery with `vendor` in `deviceLabelFields` and PCI classes `02`, `03`,
   `0b`, and `12` whitelisted[^2].
2. Enable `system` worker discovery (`system-os_release.*`) if NVIDIA GPU Operator runs on the
   cluster.
3. Evaluate `NodeFeature` objects from **all** namespaces so `<node>-gpustack-*` objects reach `Node`s.
4. Set `restrictions.denyNodeFeatureLabels: false` and `disableExtendedResources: false`.
5. Allow `acceleratable.feature.gpustack.ai` and `general.feature.gpustack.ai` in `denyLabelNs`.
6. Keep a single Helm release owning the NFD CRDs (both GPUStack and NVIDIA vendor NFD 0.19.0).

[^2]: NVIDIA GPU Operator's bundled NFD whitelists only `0300` and `0302` under `node-feature-discovery.worker.config.sources.pci`, omitting `0b` and `12` NPUs unless extended.

Hardware validation used GPUStack's bundled NFD; external NFD handover is a configuration checklist
and has not been tested end to end.

### Resource removal lag

Disabling a vendor device plugin does not clear its extended resource from `Node.status.allocatable`
instantaneously. Wait until kubelet reconverges and removes the vendor key before installing GPUStack.

### Device Manager host mount

`deviceManager.hostRootMount.enabled` defaults to `true` and mounts host `/` read-only
(`HostPathDirectory`) at `/host` on the `device-manager` `main` container only (never on `stage-libs`).
The host reader enforces three boundaries:

- **Root confinement**: `os.OpenRoot` confines lookups under `etc`, `proc`, and `usr`.
- **Read-only access**: files open with `O_RDONLY` and `os.SameFile` checks, with zero write or exec paths.
- **Symlink safety**: intermediate symlinks resolve inside `/host`, while A5 regular-file checks reject leaf symlinks.

Set `deviceManager.hostRootMount.enabled=false` when cluster policy forbids `hostPath: /` mounts or
when custom [`ascend-docker-runtime`](https://www.hiascend.com/eng/developer/software/mindcluster)
setups should skip A5 conditional host mounts under [Ascend](#ascend).

---

**See also** — [Preflight Checks](/gpustack-operator/main/docs/modules/devices/preflight/index.md) (verify driver paths and runtime
isolation on a node) · [Heterogeneous Devices](/gpustack-operator/main/docs/modules/devices/index.md) (device families and
requests) · [NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md) (MIG prerequisites and mode
lifecycle)

**Next** → [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md) — choose chart or image ownership and runtime settings precedence.
