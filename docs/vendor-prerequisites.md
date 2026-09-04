# Vendor Prerequisites

> **Purpose** — what to install before GPUStack per manufacturer, and which vendor GPU Operator
> components to keep or disable so exactly one device plugin, feature-discovery component and
> scheduler own each job.
> **Audience** operators · **Prerequisites** [Architecture](architecture.md) · **Read time** ~10 min

Vendor value keys and component names change between releases, so each manufacturer section names the
vendor artifact and product version its statements were read against.

Every section starts with what to **install before GPUStack**. Where the vendor also ships a GPU
Operator, it goes on to say which of that operator's components conflict with ours, how to **install
it** alongside GPUStack, and — if it is **already installed** — what to change before GPUStack can go
on the same node.

## Contents

- [Who injects the devices](#who-injects-the-devices)
- [Two device plugins, one resource name](#two-device-plugins-one-resource-name)
- [One Node Feature Discovery per cluster](#one-node-feature-discovery-per-cluster)
- [A removed plugin's resource lingers](#a-removed-plugins-resource-lingers)
- [Per manufacturer](#per-manufacturer)

## Who injects the devices

Whether a container toolkit is required at all follows directly from how our own allocator makes the
accelerator visible: a `/dev` node needs nothing from the toolkit, while an env var needs the vendor's
runtime hook to interpret it.

| Manufacturer | Allocator injects | Container toolkit required |
|---|---|---|
| AMD | `/dev` nodes (`/dev/kfd`, `/dev/dri/card*`, `/dev/dri/renderD*`) | No |
| Ascend | `ASCEND_VISIBLE_DEVICES` | Yes |
| Cambricon | `/dev` nodes (`/dev/cambricon_dev*`, the control nodes), plus `CAMBRICON_VISIBLE_DEVICES` | No |
| Hygon | `/dev` nodes (`/dev/kfd`, `/dev/mkfd`, `/dev/dri/card*`, `/dev/dri/renderD*`) | No |
| Iluvatar | `IX_VISIBLE_DEVICES` | Yes |
| MetaX | `/dev` nodes (`/dev/mxcd`, `/dev/mxnd`, `/dev/mxgd`, `/dev/dri/card*`, `/dev/dri/renderD*`) | No |
| Moore Threads | `MTHREADS_VISIBLE_DEVICES` | Yes |
| NVIDIA | `NVIDIA_VISIBLE_DEVICES` by default, or a CDI request — see [NVIDIA](#nvidia) | Yes |
| T-Head | `/dev` nodes (`/dev/alixpu`, `/dev/alixpu_ctl`, `/dev/alixpu_ppu<N>`) | No |

> **Why** — a device plugin that names `/dev` entries directly needs only what `runc` already grants;
> an env var needs the vendor's own runtime hook to translate it into a device grant, so removing that
> hook removes the accelerator from the container.

An env var reaches the container either way, so a missing toolkit does not fail the allocation: it
produces a running container with the variable set and no accelerator behind it.

## Two device plugins, one resource name

The kubelet does not reject a second device-plugin registration for a resource name it already knows:
it replaces the endpoint. Both plugins keep writing their own device sets, so node capacity oscillates
between the two views, and a Pod can fail admission during the flap.

An `Allocate` call can also arrive carrying the *other* plugin's device IDs — GPUStack's own responder
rejects those outright (`ParseResourceToken` in `pkg/deviceplugin/server.go`, called from `Allocate`).
The symptom of two plugins claiming one resource is flapping capacity and admission failures, not a
clean startup error.

## One Node Feature Discovery per cluster

Run exactly one NFD master. Two masters write and garbage-collect the same
`feature.node.kubernetes.io/*` labels, so each one's GC pass can erase the other's writes. Every
device-manager DaemonSet in this operator is placed onto a node by a `pci-<vendor>.present` label that
NFD produces, so a lost label stops that DaemonSet from scheduling at all.

A vendor GPU Operator reaches its own node labels in one of two ways, and only one of them is
unaffected by turning its bundled NFD off. Some ship a cluster-scoped `NodeFeatureRule` and let
whichever NFD is running evaluate it — AMD's operator does, and ours evaluates it: our master denies no
label namespace, honours NodeFeature labels, and is pinned to no namespace. Others read a label an NFD
*source* produces, which exists only where that source is enabled — see [NVIDIA](#nvidia).

**Bringing your own NFD.** Turning ours off hands the job to a cluster's own, through whichever lever
the install mode gives you: `--set node-feature-discovery.enabled=false` in chart mode, or
`--disable-applications=node-feature-discovery` on the worker in image mode — [Installation
Modes](architecture/installation-modes.md#chart-mode-and-image-mode) has the difference.

Either way the worker still applies the `gpustack-cpu-info` NodeFeatureRule itself, so that one is never
the replacement's to install — only to evaluate. Ours is configured for six things an NFD must do
here, and a replacement must be too:

1. A worker collecting `pci`, with `vendor` among its `deviceLabelFields` and device classes `02`, `03`,
   `0b` and `12` whitelisted. Those are what produce the `pci-<vendor>.present` labels, and the
   `gpustack-cpu-info` NodeFeatureRule matches the same classes.
2. A worker collecting `system`, which publishes `system-os_release.*`. This chain reads none of it;
   the NVIDIA GPU Operator does, and ours publishes it so that operator can run with its own NFD off
   — [NVIDIA](#nvidia).
3. A master that evaluates `NodeFeature` objects from **every** namespace. The worker's and the
   device-managers' own `<node>-gpustack-*` objects are how their detections reach the Node.
4. `restrictions.denyNodeFeatureLabels: false` and `disableExtendedResources: false`, since those
   detections arrive as labels *and* as extended resources.
5. A `denyLabelNs` that admits `acceleratable.feature.gpustack.ai` and `general.feature.gpustack.ai`.
6. One owner for the NFD CRDs. Vendor charts bundle an NFD version of their own, so decide which
   release installs them before installing two that both want to.

A vendor chart that bundles NFD as a subchart exposes all five, so its NFD can be the cluster's one.
The NVIDIA GPU Operator's already publishes `vendor` as its only `deviceLabelFields`, which is what
item 1 asks for, and collects every source, which is item 2; its device classes are the gap.

It whitelists `0300` and `0302` — the GPUs it was written for — and neither `0b` nor `12`, so an
accelerator in either class never gets the label its device manager schedules on, and that DaemonSet
stays unscheduled with nothing to say why. Both values live under that chart's
`node-feature-discovery.worker.config.sources.pci`.

Item 6 is not a version problem there: both charts vendor NFD 0.19.0, so the CRDs are the same objects
and all that has to be settled is which release owns them. And the class gap only bites once a second
manufacturer is present — the display-controller classes it does whitelist cover NVIDIA and AMD, so a
cluster holding only those is already served by its list.

Read the six items as a checklist rather than a recipe: every cluster this page was written against
ran the NFD this chart bundles, so a handover has not been observed end to end.

## A removed plugin's resource lingers

Removing a vendor's device plugin does not immediately remove its extended resource from the Node
object — the kubelet only drops it once it reconverges. A scheduler can keep seeing the old resource as
allocatable for a window after the plugin is gone, so "disabled" is not the same moment as "gone" from
a scheduling decision's point of view.

## Per manufacturer

### AMD

Read against **AMD GPU Operator v1.5.1** —
[Overview](https://instinct.docs.amd.com/projects/gpu-operator/en/latest/index.html) ·
[Helm Chart Install](https://instinct.docs.amd.com/projects/gpu-operator/en/latest/installation/kubernetes-helm.html) ·
[Toolkit Install](https://instinct.docs.amd.com/projects/container-toolkit/en/latest/index.html).

**Install before GPUStack** — the ROCm driver alone. Per [Who injects the
devices](#who-injects-the-devices), our allocator injects `/dev/kfd` and each granted accelerator's DRM
nodes itself, so neither the AMD Container Toolkit nor `amd-container-runtime` is needed.

A non-root workload still needs `video` and `render` supplementary-group membership on its container's
user — the device plugin API has no field to grant a supplementary group, so this is the workload's or
the cluster's own `securityContext` to set. Measured, the failure mode without it is **zero enumerated
ROCm agents, not an error**: `rocm-smi` reads sysfs and keeps reporting the accelerator regardless, so
it is not a valid check here — use `rocminfo` instead.

**Vendor GPU Operator** — the AMD GPU Operator, driven by a `DeviceConfig` CR the chart creates.

| Component | Keep / disable | Mechanism |
|---|---|---|
| Driver | Keep | `DeviceConfig.spec.driver.enable` (default `true`) |
| Metrics exporter (DCGM-class) | Keep | `DeviceConfig.spec.metricsExporter.enable` — **default `false`**, enable it |
| Device plugin | Disable | `DeviceConfig.spec.devicePlugin.enableDevicePlugin: false` |
| Node Labeller | Disable | `DeviceConfig.spec.devicePlugin.enableNodeLabeller: false` |
| Bundled Node Feature Discovery | Disable | `--set node-feature-discovery.enabled=false` |
| Bundled Kernel Module Management | Disable when the driver is pre-installed | `--set kmm.enabled=false --set kmm.watch=false` |
| DRA driver | Leave enabled to test whole-card allocation | `DeviceConfig.spec.draDriver.enable` — mutually exclusive with `devicePlugin.enableDevicePlugin` on one CR |

**Install it** — the chart's own switches cover the two bundled add-ons; the device plugin and the node
labeller are `DeviceConfig` fields, so they are turned off on the CR the chart just created.

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

**Already installed** — the operator's device plugin advertises `amd.com/gpu`, the same name ours
does, so it has to stop before GPUStack goes on that node:

1. **Device plugin and Node Labeller** — turn both off on the existing `DeviceConfig`, the same patch as
   above against the CR the running release created. Do not delete the DaemonSet: the controller
   recreates whatever the CR still asks for.
2. **Bundled Node Feature Discovery** — if that release brought its own, `helm upgrade` it with the
   values it was installed with plus `--set node-feature-discovery.enabled=false`.
3. **Verify** — wait for `amd.com/gpu` to leave the Node before installing GPUStack;
   `kubectl get node <node> -o jsonpath='{.status.allocatable}'` no longer lists it. The kubelet drops
   it a moment after the plugin goes, not at the same instant.


### Ascend

Read against the **Ascend NPU driver and firmware** package —
[Driver Install](https://www.hiascend.com/hardware/firmware-drivers).

**Install before GPUStack** — the Ascend NPU driver and firmware, and the Ascend Docker Runtime: per
[Who injects the devices](#who-injects-the-devices), our allocator emits `ASCEND_VISIBLE_DEVICES` and
no device node, so that runtime is what turns an allocation into `/dev` entries.

Register the runtime under the handler name `ascend`, which is the RuntimeClass this chart creates for
this manufacturer, or set `deviceManager.createRuntimeClasses=false` and attach your own.

**Atlas A5 (the 950 series) needs MindCluster 26.0.0 or newer.** A5 support landed in that release,
whose notes carry the line *"Ascend Docker Runtime supports the Atlas 350 PCIe card"* — this
generation's PCIe card. 910B and earlier are unaffected: their line ends at 7.3.x, and the vendor's
releases run 5.x, 6.x, 7.x then 26.x with nothing in between.

**An older runtime refuses the allocation, and blames the wrong thing.** It maps the `Ascend950` chip
name to no device type it knows, so it falls back to the manager-device list every earlier generation
used — and that list carries `/dev/devmm_svm`, which an A5 host does not have. Adding a device node
that is not there is fatal, so container creation fails, naming that device rather than the version
behind it.

The cards are not the gap: `/dev/davinci<N>` is built from the requested index with no chip name
involved, so an older runtime would have injected them. It never gets that far.

`ascend-docker-runtime --version` does not answer this. It is a `runc` wrapper that hands every
unrecognized argument to the host's own `runc`, so `--version` prints `runc`'s banner and says nothing
about the vendor release. Read the version the installer recorded instead:

```bash
grep '^version=' /usr/local/Ascend/Ascend-Docker-Runtime/ascend_docker_runtime_install.info
# version=v7.3.0    <- predates A5 support
# version=v26.1.0   <- serves A5
```

`gpustack-operator device-manager preflight` reads the same file and reports it as the
`ascend-docker-runtime` check on each A5 accelerator, so a node too old to serve them is named before
a workload lands on it.


### Cambricon

Read against **Cambricon driver 5.10.22** (Neuware SDK 1.15.0) —
[Driver Install](https://www.cambricon.com/docs/sdk_1.15.0/driver_5.10.22/user_guide/index.html).

**Install before GPUStack** — the Cambricon MLU driver alone. Per [Who injects the
devices](#who-injects-the-devices), our allocator injects the card's own node and the host's control
nodes on both the whole-card and the sliced path, so `cntoolkit` is not required. It still sets the
`CAMBRICON_VISIBLE_DEVICES` a deployment running `cambricon-container-runtime` keys on, naming the same
cards the injected nodes do.


### Hygon

Read against the **Hygon DCU Operator** — its documentation tree publishes only a `latest` revision,
with no pinned semantic version —
[Overview](https://developer.sourcefind.cn/document/7541efc4-54b1-11f1-8265-0242ac150003).

**Install before GPUStack** — the Hygon DCU driver and firmware, with the device files
(`/dev/kfd`, `/dev/mkfd`, `/dev/dri/card*`, `/dev/dri/renderD*`) visible on the node. Per [Who injects
the devices](#who-injects-the-devices), our allocator injects those nodes directly, and the DCU
Operator ships no container runtime of its own, so no container toolkit is required.

**Vendor GPU Operator** — the DCU Operator, driven by a single `DeviceConfig` CR (`kubectl explain
deviceconfig.spec`); every component below is a field under that one CR, not a separate CRD.

| Component | Keep / disable | Mechanism |
|---|---|---|
| DCU-Exporter (metrics) | Keep | `DeviceConfig.spec.metricsExporter.enable` — disabled by default, enable it |
| DCU-DCGM (diagnostics) | Keep | `DeviceConfig.spec.dcgm.enable` — disabled by default, enable it |
| DCU-Device-Plugin | Disable | `DeviceConfig.spec.devicePlugin.enableDevicePlugin: false` |
| DCU-Label-Node (NFD-equivalent) | Disable | `DeviceConfig.spec.devicePlugin.enableNodeLabeller: false` |

**Install it** — the vendor's install chapter sits behind its authenticated documentation portal, so no
install command is reproduced here. Whatever installs the operator, turn the two components off on the
`DeviceConfig` it creates.

```bash
kubectl -n kube-system patch deviceconfig <name> --type merge -p \
  '{"spec":{"devicePlugin":{"enableDevicePlugin":false,"enableNodeLabeller":false},"metricsExporter":{"enable":true},"dcgm":{"enable":true}}}'
```

**Already installed** — the operator's device plugin advertises `hygon.com/dcu`, the same name ours
does, so it has to stop before GPUStack goes on that node:

1. **Device plugin and Node Labeller** — turn `enableDevicePlugin` and `enableNodeLabeller` off on the
   existing `DeviceConfig`, with the patch above against the CR the running release created. Do not
   delete the DaemonSets: the controller recreates whatever the CR still asks for.
2. **Verify** — wait for `hygon.com/dcu` to leave the Node before installing GPUStack.


### Iluvatar

Read against **ix-GPU-Operator v4.4.0** and **ix-Container-Toolkit v1.1.0** —
[Helm Chart Install](https://developer.iluvatar.com/docs/generaldocs_ix_gpu_operator?category=%E4%BA%91%E5%8E%9F%E7%94%9F%E4%B8%8E%E9%9B%86%E7%BE%A4%E9%83%A8%E7%BD%B2&manual=generaldocs_k8stools_all) ·
[Toolkit Install](https://developer.iluvatar.com/docs/generaldocs_ix_container_toolkit?category=%E4%BA%91%E5%8E%9F%E7%94%9F%E4%B8%8E%E9%9B%86%E7%BE%A4%E9%83%A8%E7%BD%B2&manual=generaldocs_k8stools_all).

**Install before GPUStack** — the Iluvatar driver and the ix-Container-Toolkit: per [Who injects the
devices](#who-injects-the-devices), our allocator emits `IX_VISIBLE_DEVICES` and no device node. The
two are paired by version — a v4.4.0 driver needs the v1.1.0 toolkit, and the vendor states the two
must match on host and in-container.

Register the runtime under the handler name `iluvatar`, which is the RuntimeClass this chart creates
for this manufacturer, or set `deviceManager.createRuntimeClasses=false` and attach your own.

**Vendor GPU Operator** — the ix-GPU-Operator, a Helm chart with one switch per component.

| Component | Keep / disable | Mechanism |
|---|---|---|
| Driver (`ix-driver`) | Disable when the driver is pre-installed | `ixDriver.enabled` — default `false` already |
| ix-Exporter (metrics) | Keep | `ixExporter.enabled` (default `true`) |
| ix-Device-Plugin | Disable | `--set ixDevicePlugin.enabled=false` |
| ix-Feature-Discovery (NFD-equivalent) | Disable | `--set ixfd.enabled=false` — bundles upstream Node Feature Discovery under the release name |

**Install it** — the OCI chart, with the driver left to the node and the two competing components off.

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

**Already installed** — ix-Device-Plugin advertises `iluvatar.com/gpu`, the same name ours does, so it
has to stop before GPUStack goes on that node:

1. **ix-Device-Plugin and ix-Feature-Discovery** — `helm upgrade` the release with the values it was
   installed with plus `--set ixDevicePlugin.enabled=false --set ixfd.enabled=false`. The operator
   removes both workloads itself; deleting one by hand only makes it come back on the next reconcile.
2. **Verify** — wait for `iluvatar.com/gpu` to leave the Node before installing GPUStack.

Below software-stack v4.3.0 the operator is deployed from YAML manifests with no per-component toggle.
There the only lever is the whole manifest: delete it, and install the driver and ix-Container-Toolkit
on the node yourself.


### MetaX

Read against **MetaX Kubernetes GPU Operator v0.15.3** —
[Overview](https://developer.metax-tech.com/api/client/document/preview/1411/k8s/00_overview.html) ·
[Helm Chart Install](https://developer.metax-tech.com/api/client/document/preview/1411/k8s/02_start.html) ·
[Components](https://developer.metax-tech.com/api/client/document/preview/1411/k8s/03_component.html).

**Install before GPUStack** — the MetaX driver and the MXMACA SDK. Per [Who injects the
devices](#who-injects-the-devices), our allocator injects the three control nodes (`/dev/mxcd`,
`/dev/mxnd`, `/dev/mxgd`) and each granted accelerator's DRM nodes directly, so no container toolkit
is required.

**Vendor GPU Operator** — the MetaX GPU Operator. Its `minimalMode` is the install shape that fits
here: it deploys neither the kernel driver nor the MXMACA SDK, leaving both to the node.

| Component | Keep / disable | Mechanism |
|---|---|---|
| Kernel driver and MXMACA SDK | Not deployed | `--set minimalMode=true` leaves both to the node |
| `mx-exporter` (metrics) | Keep | optional under `minimalMode` |
| `controller-exporter` | Keep | optional under `minimalMode` |
| `gpu-device` (device plugin) | **Cannot be disabled** | the vendor marks it mandatory in every install mode, `minimalMode` included |
| `gpu-label` (NFD-equivalent) | **Cannot be disabled** | same marking |
| `gpu-scheduler` | Disable | optional — leave it undeployed |
| `topo-discovery` | Disable | optional — leave it undeployed |

**Install it** — substitute your own registry for `DOMAIN/PROJECT`. `minimalMode` is fixed at install
time and cannot be switched on a running release.

```bash
helm install oci://DOMAIN/PROJECT/metax-operator \
  --version VERSION \
  --create-namespace -n metax-operator \
  --generate-name \
  --wait \
  --set registry=DOMAIN/PROJECT \
  --set minimalMode=true
```

**Already installed** — `gpu-device` advertises `metax-tech.com/gpu`, the same name ours does, and the
vendor marks it mandatory in every install mode, so the two cannot share a node. The operator has to
go:

1. **Kernel driver and MXMACA SDK** — make sure both are on the node independently of the operator.
   Under `minimalMode` they already are; otherwise install them first, or uninstalling takes them with
   it.
2. **The whole release** — `helm uninstall` it. `gpu-device` and `gpu-label` have no switch, so there
   is nothing narrower to turn off.
3. **Verify** — wait for `metax-tech.com/gpu` to leave the Node before installing GPUStack.


### Moore Threads

Read against **MT GPU Operator v2.1.0** (KUAE Cloud Native) —
[Overview](https://docs.mthreads.com/cloud-native/cloud-native-doc-online/introduction/) ·
[Helm Chart Install](https://docs.mthreads.com/cloud-native/cloud-native-doc-online/install_guide#mt-gpu-operator).

**Install before GPUStack** — the Moore Threads driver and the MT Container Toolkit: per [Who injects
the devices](#who-injects-the-devices), our allocator emits `MTHREADS_VISIBLE_DEVICES` and no device
node. Both can come from the operator's own `full` mode below.

**Vendor GPU Operator** — the MT GPU Operator, installed in one of two modes rather than by a single
toggle. `full` is the `mt-gpu-operator` Helm chart pair; `core` is a `kubectl apply -f` of plain
manifests carrying only the device plugin and the exporter.

| Component | Keep / disable | Mechanism |
|---|---|---|
| Driver (`driverToolkit`) | Keep | `mtGpuClusterPolicyClusterpolicy.driverToolkit.enabled` (default `true`) |
| Container toolkit | Keep | `mtGpuClusterPolicyClusterpolicy.containerToolkit.enabled` (default `true`) |
| DCGM / DCGM exporter | Keep | `.dcgm.enabled` / `.dcgmExporter.enabled` |
| Bundled NFD | Disable | `.nfd.enabled=false` |
| sGPU scheduler + admission webhook | Disable | `.gpuScheduler.enabled` / `.gpuWebhook.enabled` — default `false` already |
| `kured`, node-problem-detector, `aiOps` | Disable | `.kured.enabled=false`, `.nodeProblemDetector.enabled=false`, `.aiOps.enabled=false` |
| Device plugin | No switch | it ships inside the operator's reconciliation loop under `full` mode |

**Install it** — from the chart directories the vendor's bundle unpacks, with the cluster-level add-ons
off. No key disables the device plugin under `full` mode, so a node that must run ours instead is
served by `core` mode with that manifest's device plugin removed.

```bash
helm install --wait mt-gpu-operator mt-gpu-operator
helm install --wait mt-gpu-operator-custom-resources mt-gpu-operator-custom-resources \
  --set mtGpuClusterPolicyClusterpolicy.nfd.enabled=false \
  --set mtGpuClusterPolicyClusterpolicy.kured.enabled=false \
  --set mtGpuClusterPolicyClusterpolicy.nodeProblemDetector.enabled=false \
  --set mtGpuClusterPolicyClusterpolicy.aiOps.enabled=false
```

**Already installed** — the vendor's device plugin advertises `mthreads.com/gpu`, the same name ours
does, and `full` mode exposes no key that removes it, so what to do depends on which mode installed it:

1. **Cluster-level add-ons** (bundled NFD, `kured`, node-problem-detector, `aiOps`) — under `full`,
   `helm upgrade` the `mt-gpu-operator-custom-resources` release with the switches above. `core` ships
   none of them.
2. **Device plugin** — no key removes it under `full`, so the mode that installed it decides:
   - **`full`** — either leave that node to the vendor stack, or move it to `core`: uninstall both
     charts, keep the driver and MT Container Toolkit on the node by installing them outside the
     operator, and go on below.
   - **`core`** — there is no controller, so edit the manifests directly: drop the device plugin from
     `deployments.yaml`, leave the exporter, and re-apply.
3. **Verify** — wait for `mthreads.com/gpu` to leave the Node before installing GPUStack.


### NVIDIA

Read against **NVIDIA GPU Operator v26.3.3** —
[Overview](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/overview.html) ·
[Helm Chart Install](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html).

**Install before GPUStack** — the NVIDIA driver and the NVIDIA Container Toolkit: per [Who injects the
devices](#who-injects-the-devices), our allocator emits `NVIDIA_VISIBLE_DEVICES` by default and no
device node. The toolkit is what the other channel needs too — it is what writes the node's CDI
specifications. Both can come from the operator itself.

**The toolkit is also what MIG needs**, not only workloads: the Device Manager Pod reads the management
library through it, and the chart declares `NVIDIA_MIG_CONFIG_DEVICES` / `NVIDIA_MIG_MONITOR_DEVICES` on
that Pod so the runtime stops hiding the driver's MIG capabilities from it — see
[MIG prerequisites](operation/nvidia-mig.md#prerequisites) for the two ways to break that.

**Vendor GPU Operator** — the NVIDIA GPU Operator, a Helm chart with one switch per component.

| Component | Keep / disable | Mechanism |
|---|---|---|
| Driver | Keep | `driver.enabled` (default `true`) |
| Container Toolkit | Keep | `toolkit.enabled` (default `true`) |
| DCGM exporter | Keep | `dcgmExporter.enabled` (default `true`) |
| Device plugin | Disable | `--set devicePlugin.enabled=false` |
| GPU Feature Discovery | Keep | `gfd.enabled` (default `true`) |
| Bundled Node Feature Discovery | Disable — or keep it and turn ours off instead | `--set nfd.enabled=false`, or [hand the job over](#one-node-feature-discovery-per-cluster) |
| MIG Manager | Disable | `--set migManager.enabled=false` — it reconfigures MIG geometry from node labels and fights this operator's Allocate-time MIG actuation. One profile does not: [NVIDIA MIG](operation/nvidia-mig.md#across-a-fleet-with-the-nvidia-gpu-operator) |
| CDI | Disable | `--set cdi.enabled=false` — default `true` from v25.10, see below |

**Check the node is not opted out.** Some GPU distributions ship their nodes already labelled
`nvidia.com/gpu.deploy.operands=false`, which tells this operator to place nothing on them at all. Its
`ClusterPolicy` still reports `ready`, so the only symptom is that no operand ever appears. Set it to
`true` on the nodes you want managed.

**Install it** — the driver, toolkit, exporter and feature discovery stay, the three competing
components go.

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

**Already installed** — the operator's device plugin advertises `nvidia.com/gpu`, the same name ours
does, and its MIG Manager drives the same hardware our allocator does, so both have to stop before
GPUStack goes on that node.

One `helm upgrade`, with the values the release was installed with plus the three switches below. The
operator removes those DaemonSets itself; deleting one by hand only makes it come back, because the
`ClusterPolicy` still asks for it.

1. **Device plugin** — `--set devicePlugin.enabled=false`. It advertises the resource name ours does.
2. **MIG Manager** — `--set migManager.enabled=false`. A node whose geometry it had been maintaining
   keeps whatever it was left in, and our allocator reconfigures that at `Allocate` time from then on.
3. **Bundled Node Feature Discovery** — `--set nfd.enabled=false`. Ours publishes the
   `system-os_release.*` labels that release had been publishing, so nothing has to be carried over by
   hand. Keeping this one and turning ours off is the other way round — [One NFD per
   cluster](#one-node-feature-discovery-per-cluster).
4. **Driver, container toolkit, DCGM exporter, GPU Feature Discovery** — leave running. GPUStack needs
   the first two.
5. **Verify** — wait for `nvidia.com/gpu` to leave the Node before installing GPUStack.

**CDI mode.** `--set cdi.enabled=false` is the narrower choice for a node this operator's allocator
must control end to end. Leaving it on is workable.

> **Why** — GPU Operator >= 25.10 defaults `cdi.enabled: true`, and the older `cdi.default` field is
> deprecated and has no effect once `cdi.enabled` exists. With CDI on, the toolkit resolves a bare
> accelerator UUID against the node's specifications, and a name it cannot resolve fails container
> creation loudly rather than silently — which is what makes leaving it on workable rather than wrong.

**A node serving MIG wants CDI off.** Our own side of it is safe whatever the toolkit does: a
partition allocation is always carried by the variable, never by a CDI request.

> **Why** — in `cdi` mode the runtime resolves whatever `NVIDIA_VISIBLE_DEVICES` holds against the
> node's specifications, and the MIG instance our allocator materializes at `Allocate` time is named
> in none of them, so a partition request has nothing to resolve against.

**Which channel carries our own grant.** Prefer `auto`, set through
[`GPUSTACK_NVIDIA_DEVICE_INJECTION_STRATEGY`](settings.md#per-manufacturer-overrides), over naming a
channel yourself. It answers per container, independently of the switch above.

> **Why** — two channels exist, and which one a node uses is what decides whether it needs the vendor
> runtime at all: the default names the accelerator in `NVIDIA_VISIBLE_DEVICES`, which only
> `nvidia-container-runtime` reads, while the other requests it as a CDI device the container engine
> resolves by itself under plain `runc`. Whether a node can use the second is a property of its
> container engine, which is what `auto` reads and a human would otherwise have to. It uses CDI only
> where the vendor's own specifications already name the exact accelerator being granted, and writes
> no specification of its own. [Device
> Discovery](architecture/device-discovery.md#exclusive-and-shared) has the mechanism, and
> [Settings](settings.md#per-manufacturer-overrides) what it checks, when it declines, and the one
> engine that has to be told.

`auto` leaves a Pod that names a `runtimeClassName` to that handler, whose configuration it cannot
read — correctly, since the handler is what injects. Whether that covers the Instances this operator
creates depends on the class existing: the worker attaches `runtimeClassName: nvidia` only where it
finds one, so with `deviceManager.createRuntimeClasses=false` and none supplied, an Instance runs
under the engine's default handler and `auto` answers for it like any other Pod.


### T-Head

Read against the **T-Head SAIL SDK** (also called the PPU SDK), HGGCRT v3 —
[Driver Install](https://developer.t-head.cn/docs_center/doc_detail/index.html?projectId=39&chapterId=194) ·
[Overview](https://developer.t-head.cn/docs_center/doc_detail/index.html?projectId=33&chapterId=118).

**Install before GPUStack** — the PPU SDK alone. It ships as a self-extracting Runfile from T-Head's
download centre, installs to `/usr/local/PPU_SDK` by default, and is verified with `ppu-smi` (which
prints the driver version) and `hgcc --version`. Take HGGCRT v3 rather than v2: it is backward
compatible with the v2 API.

Per [Who injects the devices](#who-injects-the-devices), our allocator injects T-Head accelerators as
`/dev` nodes directly, which is the mechanism the vendor's own container guide uses — it isolates a
container by passing those nodes to `docker run --device`, with no vendor runtime in the path. Which
nodes a container receives is in [Device Discovery](architecture/device-discovery.md).

---

**See also** — [Device Discovery](architecture/device-discovery.md) (how the allocator and
`runtimeInjects*` decide what a Pod's container receives) · [Settings & Environment
Variables](settings.md) (per-manufacturer overrides and toolkit paths)

**Next** → [Installation Modes](architecture/installation-modes.md) — what the chart owns, what the
worker applies.
