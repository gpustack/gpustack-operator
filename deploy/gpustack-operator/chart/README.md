# gpustack-operator

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) and Kueue.

## Introduction

This chart deploys the GPUStack Operator control plane (`gpustack-operator-worker`), the
per-manufacturer device managers (`gpustack-operator-device-manager-<manufacturer>`), and the
applications the scheduling chain is built on — [Kueue](https://kueue.sigs.k8s.io),
[Node Feature Discovery](https://kubernetes-sigs.github.io/node-feature-discovery) and the NFS/S3
CSI drivers — onto a Kubernetes cluster using the [Helm](https://helm.sh) package manager.

The operator turns raw node hardware into a Kueue-based scheduling chain for accelerators
(GPU/NPU/TPU).

> Those four applications are **vendored subcharts** of this chart, each behind an `enabled` switch,
> and they are deployed as part of **this** release. Two consequences worth knowing before you
> install:
>
> - `helm uninstall` deletes Kueue's CRDs, and therefore **every ClusterQueue, LocalQueue,
>   ResourceFlavor, AdmissionCheck and Workload in the cluster**. To keep a Kueue this release does
>   not own, install with `--set kueue.enabled=false` and bring your own. The same applies to
>   `node-feature-discovery.enabled=false`, which still gives you the `gpustack-cpu-info`
>   NodeFeatureRule the chain starts from.
> - Upgrading an install from v0.7.x or earlier, where these ran as Helm releases of their own, is a
>   **one-time ownership transfer**. See the
>   [migration guide](https://github.com/gpustack/gpustack-operator/blob/main/docs/migration/to-subcharts.md).

Running more than one replica of each control-plane component is described in the
[high-availability guide](https://github.com/gpustack/gpustack-operator/blob/main/docs/operation/high-availability.md).

## Prerequisites

Kubernetes: `>=1.23.0-0`
- Helm 3.8.0+ — or **3.21.0+** for the one-time upgrade of a v0.7.x-or-earlier install, which needs `--take-ownership`
- (Optional) [cert-manager](https://cert-manager.io) when `worker.certmanager.enabled` is `"true"` (or `"auto"` with cert-manager present)

## Installing the Chart

To install the chart with the release name `gpustack-operator`:

```bash
helm install gpustack-operator oci://docker.io/gpustack/charts/gpustack-operator \
  --namespace gpustack-system \
  --create-namespace
```

> Install with the release name `gpustack-operator` so that the rendered resource names
> match the conventional `gpustack-operator-worker` / `gpustack-operator-device-manager`.

## Uninstalling the Chart

To uninstall the `gpustack-operator` release:

```bash
helm uninstall gpustack-operator --namespace gpustack-system
```

`helm uninstall` removes only what the release owns. The finalizers the controllers leave on their
objects, and the CRDs, aggregated APIServices and webhook configurations the worker registers itself,
stay behind. Run the chart's `files/cleanup.sh` afterwards to clear them, or install with
`--set cleanupOnUninstall=true` to have a gated post-delete Job do it — which is safe only where this
release exclusively owns those CRDs.

## Requirements

Kubernetes: `>=1.23.0-0`

| Repository | Name | Version |
|------------|------|---------|
|  | csi-driver-nfs | 4.13.2 |
|  | csi-driver-s3 | 0.43.7 |
|  | kueue | 0.18.4 |
|  | node-feature-discovery | 0.19.0 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| cleanupOnUninstall | bool | `false` | Run a post-delete cleanup Job on `helm uninstall` that removes the leftovers the worker creates at runtime but Helm does not manage: the Kueue / Node Feature Discovery / CSI Helm releases, their CRDs, the finalizers pinning Kueue and Instance objects, and the aggregated APIServices / webhooks. Disabled by default because those CRDs may be shared with other workloads; enable only when this release exclusively owns them (e.g. CI / e2e). |
| global | object | `{"imageNamespace":"","imagePullPolicy":"","imagePullSecrets":[],"imageRegistry":"","manufacturers":{"amd":{"pciVendorID":"1002","resourceName":"amd.com/gpu","runtimeName":"amd"},"ascend":{"pciVendorID":"19e5","resourceName":"huawei.com/npu","runtimeName":"ascend"},"cambricon":{"pciVendorID":"cabc","resourceName":"cambricon.com/mlu","runtimeName":"cambricon"},"hygon":{"pciVendorID":"1d94","resourceName":"hygon.com/dcu"},"iluvatar":{"pciVendorID":"1e3e","resourceName":"iluvatar.com/gpu","runtimeName":"iluvatar"},"metax":{"pciVendorID":"9999","resourceName":"metax-tech.com/gpu"},"mthreads":{"pciVendorID":"1ed5","resourceName":"mthreads.com/gpu","runtimeInjectsDriver":true,"runtimeName":"mthreads"},"nvidia":{"partitionKind":"mig","pciVendorID":"10de","resourceName":"nvidia.com/gpu","runtimeInjectsDriver":true,"runtimeName":"nvidia"},"thead":{"pciVendorID":"1ded","resourceName":"alibabacloud.com/ppu"}},"nodeSelector":{}}` | Global values shared across the chart and its subcharts. |
| global.imageRegistry | string | `""` | Image registry override applied to every image this chart renders, the subcharts' workloads, sidecars and hook Jobs included (e.g. "docker.io"). When empty, the registry each image reference already encodes is used as-is. |
| global.imageNamespace | string | `""` | Image namespace override applied to every image this chart renders, the subcharts' workloads, sidecars and hook Jobs included. When non-empty it replaces the namespace segment of each image reference (e.g. "gpustack"). |
| global.imagePullPolicy | string | `""` | Image pull policy applied to every workload, the subcharts' included. It outranks a component's own pull policy; when empty, each component's own is used. |
| global.imagePullSecrets | list | `[]` | Image pull secrets applied to all workloads. |
| global.nodeSelector | object | `{}` | Node selector applied to all workloads when a component does not set its own. |
| global.manufacturers | object | `{"amd":{"pciVendorID":"1002","resourceName":"amd.com/gpu","runtimeName":"amd"},"ascend":{"pciVendorID":"19e5","resourceName":"huawei.com/npu","runtimeName":"ascend"},"cambricon":{"pciVendorID":"cabc","resourceName":"cambricon.com/mlu","runtimeName":"cambricon"},"hygon":{"pciVendorID":"1d94","resourceName":"hygon.com/dcu"},"iluvatar":{"pciVendorID":"1e3e","resourceName":"iluvatar.com/gpu","runtimeName":"iluvatar"},"metax":{"pciVendorID":"9999","resourceName":"metax-tech.com/gpu"},"mthreads":{"pciVendorID":"1ed5","resourceName":"mthreads.com/gpu","runtimeInjectsDriver":true,"runtimeName":"mthreads"},"nvidia":{"partitionKind":"mig","pciVendorID":"10de","resourceName":"nvidia.com/gpu","runtimeInjectsDriver":true,"runtimeName":"nvidia"},"thead":{"pciVendorID":"1ded","resourceName":"alibabacloud.com/ppu"}}` | Accelerator manufacturers to manage, one row per manufacturer holding everything the release needs to know about it. `pciVendorID` is what NFD labels its devices with, `resourceName` what its device-plugin advertises to the kubelet, `runtimeName` the RuntimeClass its workloads run under, and `partitionKind` its own name for hardware partitioning ("mig" is NVIDIA's name for the concept, not the concept's). Each of those four is also a `pkg/nodefeature` default overridable by a `GPUSTACK_<MANUFACTURER>_*` variable, and this chart fans every stated one out as that variable to the worker and to the device-managers — so a row here decides them rather than restating them, and a field left out keeps the operator's own default. `runtimeInjectsDriver` is the exception, a deployment fact no Go code holds: it marks a manufacturer whose user-space driver reaches a container only through its container runtime, so that manufacturer's device-manager runs under the RuntimeClass too. Every other vendor's device-manager reads its management library from a hostPath mount the DaemonSet already makes.  `runtimeName` is what decides which manufacturers this chart creates a RuntimeClass for (`deviceManager.createRuntimeClasses`), so it is stated only where a vendor's container runtime registers a handler by that name: the operator attaches a RuntimeClass to a workload whenever one exists, and a class no runtime backs would fail every Pod of that vendor. `pkg/nodefeature` knows two more names than are stated here, which is deliberate — a cluster whose vendor operator created that class still has it used, while this chart never conjures one.  The keys are the manufacturer list: they become the worker's `--manufacturer` flag (which is where the `gpustack-cpu-info` NodeFeatureRule's vendor IDs come from, since the worker renders that rule itself), one device-manager DaemonSet each, and Kueue's credits mapping. Each DaemonSet only schedules on nodes NFD labelled with the matching PCI vendor, so adding a row is all it takes for a new vendor's devices to be labelled, detected and given a device-manager, and a row no node in the cluster carries simply runs nothing.  It lives under `global` because the vendored Kueue subchart reads it too, and `global` is the only channel Helm gives a subchart to this chart's values. The defaults are every manufacturer `pkg/nodefeature` knows, which a Go test asserts and the generated schema requires. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| fullnameOverride | string | `""` | Override the fully qualified release name used as the base of resource names. |
| namespaceOverride | string | `""` | Namespace to deploy the resources into; defaults to the release namespace. |
| commonLabels | object | `{}` | Extra labels added to every resource. |
| commonAnnotations | object | `{}` | Extra annotations added to every resource. |
| image | object | `{"pullPolicy":"IfNotPresent","repository":"gpustack/gpustack-operator","tag":""}` | Default operator image (shared by the worker and the device-manager). Each component may override any of these keys via its own `image` block. |
| image.repository | string | `"gpustack/gpustack-operator"` | Operator image repository (shared by the worker and the device-manager). |
| image.tag | string | `""` | Operator image tag; defaults to "v<.Chart.AppVersion>" when empty. |
| image.pullPolicy | string | `"IfNotPresent"` | Operator image pull policy. |
| worker.enabled | bool | `true` | Deploy the worker (control plane). Disable to deploy only the device-managers. |
| worker.certmanager | object | `{"enabled":"auto","issuer":{"kind":"Issuer","name":""}}` | Configure cert-manager integration for the worker webhook serving certificate. |
| worker.certmanager.enabled | string | `"auto"` | Whether to issue the worker webhook serving certificate with cert-manager. Accepts a string (or the equivalent boolean): "auto" creates the cert-manager resources only when the cert-manager CRDs are detected in the cluster; "true" always creates them (cert-manager must be installed); "false" never creates them and the worker generates and manages its own certificate. |
| worker.certmanager.issuer.name | string | `""` | Existing issuer name to use; when empty a self-signed Issuer is created. |
| worker.certmanager.issuer.kind | string | `"Issuer"` | Issuer kind ("Issuer" or "ClusterIssuer"); only used when `issuer.name` is set. |
| worker.replicas | int | `1` | Number of operator (control plane) replicas. Leader election is always on, so extra replicas stand by; the reconcilers only ever run in one of them. |
| worker.disableApplications | list | `["*"]` | Applications the worker must not install at runtime, or `*` for all of them. This chart deploys every one of them itself, so the default takes the worker out of that business entirely. Keep the wildcard: whenever this chart deploys the worker, anything left off the list makes the worker install a SECOND release of this same chart, whose render overlaps this one's. Helm refuses an object another release owns, so that install fails and the worker's startup with it. Handing a component back would mean disabling it here and in this chart's own switch for it, in step, through every upgrade — nothing checks that, which is why the supported split is all or nothing. The worker installs applications only where no chart deploys it (see docs/architecture.md, "Two install modes"). The value is templated rather than appended through `extraArgs` because the flag accumulates across occurrences, so a second one could only ever disable more. Accepted names: `*`, `kueue`, `node-feature-discovery`, `csi-driver-nfs`, `csi-driver-s3`, `device-manager`. |
| worker.podDisruptionBudget.enabled | bool | `false` | Create a PodDisruptionBudget for the worker. Leave it off on a single-replica install, where it would only block node drains. |
| worker.podDisruptionBudget.minAvailable | int | `1` | Minimum number of worker pods that must stay available, as a count or a percentage. Keep it below `replicas`, or no node can ever be drained. |
| worker.securePort | int | `31443` | Secure serving port of the worker container. |
| worker.image | object | `{}` | Per-worker image overrides (`repository`/`tag`/`pullPolicy`); unset keys fall back to the chart-level `image`. |
| worker.dataDir | string | `"/var/lib/gpustack"` | Host path mounted at /var/lib/gpustack for persistent operator data. |
| worker.resources | object | `{"limits":{"cpu":"4","memory":"8Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits for the worker container. |
| worker.nodeSelector | object | `{}` | Node selector for the worker; falls back to `global.nodeSelector` when empty. |
| worker.tolerations | list | `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/master","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/controlplane","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/control-plane","operator":"Exists"},{"effect":"NoSchedule","key":"CriticalAddonsOnly","operator":"Exists"}]` | Tolerations for the worker. Defaults let the control plane run on control-plane/master nodes. |
| worker.topologySpreadConstraints | list | `[]` | Topology spread constraints for the worker. This is where node spread belongs: the default anti-affinity is only `preferred`, on purpose, so three replicas still schedule on a two-node cluster. An entry that omits `labelSelector` gets the worker's own. |
| worker.affinity | object | `{}` | Affinity for the worker, replacing the default `preferred` pod anti-affinity that keeps replicas off one node without ever making one unschedulable. |
| worker.extraArgs | list | `[]` | Extra command-line arguments appended to the worker container args. |
| worker.env | object | `{}` | Extra environment variables for the worker, as a name/value map. |
| deviceManager.enabled | bool | `true` | Deploy the per-manufacturer device-manager DaemonSets. |
| deviceManager.securePort | int | `32443` | Secure serving port of the device-manager container. |
| deviceManager.image | object | `{}` | Per-device-manager image overrides (`repository`/`tag`/`pullPolicy`); unset keys fall back to the chart-level `image`. |
| deviceManager.createRuntimeClasses | bool | `true` | Create a RuntimeClass for every manufacturer whose `global.manufacturers` row states a `runtimeName`, where the cluster does not already have one. Disable when something else provides them all (e.g. a vendor GPU operator). |
| deviceManager.resources | object | `{"limits":{"cpu":"4","memory":"8Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits for each device-manager container. |
| deviceManager.tolerations | list | `[{"operator":"Exists"}]` | Tolerations for the device-manager DaemonSets. Defaults tolerate every taint so a manager schedules on any node carrying the matching accelerator. |
| deviceManager.extraArgs | list | `[]` | Extra command-line arguments appended to each device-manager container args. |
| deviceManager.env | object | `{}` | Extra environment variables for the device-manager, as a name/value map. |
| migrate | object | `{"image":{}}` | Migration hook Jobs. A pre-install/pre-upgrade Job reaps a Kueue left stranded by an interrupted teardown, and on an upgrade applies the vendored subcharts' CRDs, which Helm itself applies on install only. A post-upgrade Job retires the release records of the per-application releases these subcharts replaced and prunes what they left behind. Both run the operator image, which bundles kubectl, helm and this chart; `helm upgrade --no-hooks` skips them. |
| migrate.image | object | `{}` | Per-hook image overrides (`repository`/`tag`/`pullPolicy`); unset keys fall back to the chart-level `image`. Overridable so an upgrade is never blocked by an operator tag that is not mirrored yet — at the cost that the CRDs the hook applies are then the ones vendored by the image it does run. |
| kueue | object | see the `kueue.*` keys below | [Kueue](https://kueue.sigs.k8s.io) subchart (chart 0.18.4). |
| kueue.enabled | bool | `true` | Deploy the bundled Kueue. |
| kueue.fullnameOverride | string | `"kueue"` | Name the Kueue resources after a fixed "kueue" prefix instead of the release, so that this subchart and the runtime install render identical resource names. |
| kueue.controllerManager.replicas | int | `1` | Number of Kueue controller manager replicas. `managerConfig` below elects a leader, so extra replicas stand by — but every one of them serves the admission webhook, and that is what a highly available install buys: with a single replica, losing its node blocks Pod creation in every namespace Kueue manages until it is rescheduled. |
| kueue.controllerManager.podDisruptionBudget.enabled | bool | `false` | Create a PodDisruptionBudget for the Kueue controller manager. Enable it together with `replicas` above; on a single replica it would only block node drains. |
| kueue.controllerManager.podDisruptionBudget.minAvailable | int | `1` | Minimum number of Kueue controller manager pods that must stay available, as a count or a percentage. Keep it below `replicas`, or no node can ever be drained. |
| kueue.controllerManager.topologySpreadConstraints | list | `[]` | Topology spread constraints for the Kueue controller manager, rendered as given. Unlike the worker's, these carry no selector of their own, so a `DoNotSchedule` spread across nodes needs `labelSelector: {matchLabels: {app.kubernetes.io/name: kueue, control-plane: controller-manager}}` spelled out. |
| kueue.controllerManager.tolerations | list | `[{"operator":"Exists"}]` | Tolerations for the Kueue controller manager. Defaults tolerate every taint, so the manager keeps admitting workloads wherever the control plane runs. |
| kueue.controllerManager.manager.image.repository | string | `"docker.io/gpustack/mirrored-kueue"` | Kueue controller manager image repository. |
| kueue.controllerManager.manager.image.tag | string | `"v0.18.4"` | Kueue controller manager image tag. Both Kueue lines run this same image; only the CRD schema their charts ship differs. |
| kueue.controllerManager.manager.image.pullPolicy | string | `"IfNotPresent"` | Kueue controller manager image pull policy. |
| kueue.controllerManager.manager.podAnnotations | object | `{"gpustack.ai/managed":"true"}` | Annotations of the Kueue controller manager pods, marking them as managed by the operator. |
| kueue.controllerManager.manager.resources | object | `{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits for the Kueue controller manager container. |
| kueue.managerConfig.controllerManagerConfigYaml | string | controllerManagerConfigYaml | Kueue's controller_manager_config.yaml, delivered through the manager-config ConfigMap. The "resources.transformations" list is generated from `pkg/nodefeature` by `make generate chart` between its markers; everything else is maintained here. The managed-jobs namespace selector names "gpustack-system" literally because Helm merges subchart values instead of rendering them, so the release namespace cannot reach this string. |
| node-feature-discovery | object | see the `node-feature-discovery.*` keys below | [Node Feature Discovery](https://kubernetes-sigs.github.io/node-feature-discovery) subchart (chart 0.19.0). |
| node-feature-discovery.enabled | bool | `true` | Deploy the bundled Node Feature Discovery. |
| node-feature-discovery.fullnameOverride | string | `"node-feature-discovery"` | Name the NFD resources after a fixed "node-feature-discovery" prefix instead of the release, so that this subchart and an NFD installed by anything else render identical resource names. Without it the two run side by side, each labelling the same nodes. |
| node-feature-discovery.image.repository | string | `"docker.io/gpustack/mirrored-node-feature-discovery"` | NFD image repository; the master, the worker and the gc all run it. |
| node-feature-discovery.master.replicaCount | int | `1` | Number of NFD master replicas. The master is what turns a device-manager's detections into node labels, so while it is down no node is ever (re)classified and the scheduling chain stalls for new or changed nodes. Above one, NFD's own chart adds `-enable-leader-election` for you; the standbys watch without writing. NFD's templates render no topology spread constraints, so spreading these replicas across nodes means setting `master.affinity` — which replaces NFD's own preference for control-plane nodes rather than adding to it. |
| node-feature-discovery.master.podDisruptionBudget.enable | bool | `false` | Create a PodDisruptionBudget for the NFD master. Enable it together with `replicaCount` above; on a single replica it would only block node drains. NFD spells this key `enable`, not `enabled`. |
| node-feature-discovery.master.podDisruptionBudget.minAvailable | int | `1` | Minimum number of NFD master pods that must stay available, as a count or a percentage. Keep it below `replicaCount`, or no node can ever be drained. |
| node-feature-discovery.master.annotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD master pods. |
| node-feature-discovery.master.deploymentAnnotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD master Deployment. |
| node-feature-discovery.master.serviceAccount.annotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD master ServiceAccount. |
| node-feature-discovery.master.tolerations | list | `[{"operator":"Exists"}]` | Tolerations for the NFD master. Defaults tolerate every taint, so labelling keeps working wherever the control plane runs. |
| node-feature-discovery.master.config.restrictions | object | `{"allowOverwrite":true,"denyNodeFeatureLabels":false,"disableAnnotations":false,"disableExtendedResources":false,"disableLabels":false,"disableTaints":false}` | What the NFD master is allowed to write. Every value here restates an NFD default, stated because the whole chain depends on it: labels, taints, annotations and extended resources all stay enabled, an existing label may be overwritten, and the labels a NodeFeature asks for are honoured — that last one is how a device-manager's detections reach its node.  `nodeFeatureNamespaceSelector` is deliberately absent. Pinned to one namespace it makes the master ignore every NodeFeature created anywhere else, which is exactly what taking over a cluster's existing NFD must not do: a vendor GPU operator keeps its NodeFeatures in its own namespace, and they are the features being adopted. Unset means every namespace, so — together with the two permissive keys above — a NodeFeature anywhere can influence node labels; `denyNodeFeatureLabels` is the lever that tightens it. |
| node-feature-discovery.worker.annotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD worker pods. |
| node-feature-discovery.worker.daemonsetAnnotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD worker DaemonSet. |
| node-feature-discovery.worker.serviceAccount.annotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD worker ServiceAccount. |
| node-feature-discovery.worker.tolerations | list | `[{"operator":"Exists"}]` | Tolerations for the NFD worker. Defaults tolerate every taint, so a node is discovered whatever it is tainted for. |
| node-feature-discovery.worker.config.core.labelSources | list | `["cpu","pci","custom"]` | Feature sources the worker collects. `custom` is what makes the `gpustack-cpu-info` NodeFeatureRule the operator applies evaluate. |
| node-feature-discovery.worker.config.core.labelWhiteList | string | `"^(pci-|cpu-model\\.|acceleratable)"` | Only the labels this chart consumes are published, so NFD does not cover the nodes in features nothing reads. |
| node-feature-discovery.worker.config.sources.pci.deviceClassWhitelist | list | `["02","03","0b","12"]` | PCI device classes to label, as class-code prefixes: display controllers, processing accelerators and the co-processors accelerators present as (see https://admin.pci-ids.ucw.cz/read/PD). The `gpustack-cpu-info` NodeFeatureRule matches the same classes, so it sees exactly the devices NFD was told to label; the operator renders that rule from `pkg/nodefeature`, and a Go test holds the two lists equal. Narrowing this list without narrowing that one labels fewer devices than the rule expects. |
| node-feature-discovery.worker.config.sources.pci.deviceLabelFields | list | `["vendor"]` | Device fields to label. `vendor` is what the NodeFeatureRule matches the managed `global.manufacturers`' PCI vendor IDs against. |
| node-feature-discovery.gc.annotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD gc pods. |
| node-feature-discovery.gc.deploymentAnnotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD gc Deployment. |
| node-feature-discovery.gc.serviceAccount.annotations | object | `{"gpustack.ai/managed":"true"}` | Annotations added to the NFD gc ServiceAccount. |
| node-feature-discovery.gc.tolerations | list | `[{"operator":"Exists"}]` | Tolerations for the NFD gc. Defaults tolerate every taint, so stale NodeFeature and NodeResourceTopology objects are collected wherever the control plane runs. |
| csi-driver-nfs | object | see the `csi-driver-nfs.*` keys below | [csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) subchart (chart 4.13.2). |
| csi-driver-nfs.enabled | bool | `true` | Deploy the bundled NFS CSI driver. |
| csi-driver-nfs.driver.name | string | `"nfs.csi.gpustack.ai"` | CSI driver name. Namespaced to gpustack so this driver coexists with a cluster's own csi-driver-nfs, and matched by `kuberess.CSIProvisionerNFS`, which the worker writes into the StorageClasses it provisions. |
| csi-driver-nfs.image | object | `{"baseRepo":"docker.io","csiProvisioner":{"repository":"/gpustack/mirrored-csi-provisioner","tag":"v6.1.0"},"csiResizer":{"repository":"/gpustack/mirrored-csi-resizer","tag":"v2.0.0"},"csiSnapshotter":{"repository":"/gpustack/mirrored-csi-snapshotter","tag":"v8.4.0"},"livenessProbe":{"repository":"/gpustack/mirrored-csi-livenessprobe","tag":"v2.17.0"},"nfs":{"repository":"/gpustack/mirrored-csi-nfs-driver","tag":"v4.13.0"},"nodeDriverRegistrar":{"repository":"/gpustack/mirrored-csi-node-driver-registrar","tag":"v2.15.0"}}` | Mirrored images. `baseRepo` is joined with each leading-slash `repository`. Every tag is stated rather than inherited from the chart, so what this release pulls is readable here and a subchart bump cannot move an image without the diff showing it. The driver tag stays at v4.13.0 — the image this operator has always run and mirrored — rather than following the chart's 4.13.2; the sidecar tags are the ones chart 4.13.2 ships. The `externalSnapshotter` image is absent because that component is off, and turning it on would need a mirror first. |
| csi-driver-nfs.controller.replicas | int | `1` | Number of NFS controller (provisioner) replicas. Every sidecar runs with `--leader-election`, so extra replicas stand by. Losing the controller delays volume provisioning, resizing and snapshotting; volumes already mounted keep working, because the mounting side is the node DaemonSet. This chart renders neither a PodDisruptionBudget nor topology spread constraints for the controller, and it honours `controller.affinity` only when that affinity carries `nodeSelectorTerms` — a pod anti-affinity is silently dropped — so replicas may share a node. |
| csi-driver-nfs.controller.strategyType | string | `"Recreate"` | Deployment strategy of the NFS controller. The chart's `Recreate` takes every replica down before starting the new one, which gives up the failover `replicas` above just bought; set `RollingUpdate` alongside it. |
| csi-driver-nfs.controller.name | string | `"csi-nfs-controller"` | Name of the NFS controller Deployment and of the pods it owns. |
| csi-driver-nfs.controller.dnsPolicy | string | `"ClusterFirstWithHostNet"` | DNS policy of the NFS controller pods. |
| csi-driver-nfs.controller.affinity | object | `{}` | Affinity of the NFS controller pods. Honoured only when it carries `nodeSelectorTerms` (see `replicas` above), which makes it a node-placement knob and nothing else. |
| csi-driver-nfs.controller.nodeSelector | object | `{}` | Node selector of the NFS controller pods. |
| csi-driver-nfs.controller.priorityClassName | string | `"system-cluster-critical"` | Priority class of the NFS controller pods. `system-cluster-critical` keeps provisioning working while a node is under pressure. |
| csi-driver-nfs.controller.tolerations | list | `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/master","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/controlplane","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/control-plane","operator":"Exists"},{"effect":"NoSchedule","key":"CriticalAddonsOnly","operator":"Exists"}]` | Tolerations of the NFS controller pods. The defaults let the provisioner run on control-plane nodes. |
| csi-driver-nfs.node.name | string | `"csi-nfs-node"` | Name of the NFS node DaemonSet and of the pods it owns. This is the mounting side: one pod per node, so it has no replica count or disruption budget to configure. |
| csi-driver-nfs.node.dnsPolicy | string | `"ClusterFirstWithHostNet"` | DNS policy of the NFS node pods. `ClusterFirstWithHostNet` is what lets an NFS server named in the cluster's DNS resolve from a host-networked mount. |
| csi-driver-nfs.node.affinity | object | `{}` | Affinity of the NFS node pods. Rendered as given, unlike the controller's. |
| csi-driver-nfs.node.nodeSelector | object | `{}` | Node selector of the NFS node pods, narrowing which nodes can mount NFS volumes. The chart adds `kubernetes.io/os: linux` on top of whatever is set here. |
| csi-driver-nfs.node.priorityClassName | string | `"system-cluster-critical"` | Priority class of the NFS node pods. |
| csi-driver-nfs.node.tolerations | list | `[{"operator":"Exists"}]` | Tolerations of the NFS node pods. The defaults tolerate every taint, so a volume can be mounted on any node the scheduler picks. |
| csi-driver-nfs.nodeDriverRegistrar.livenessProbe.enabled | bool | `false` | Leave the node-driver-registrar's own liveness probe off; the node plugin already runs one on its health port. |
| csi-driver-nfs.rbac.name | string | `"csi-nfs"` | Prefix of the driver's ClusterRole and ClusterRoleBinding names. |
| csi-driver-s3 | object | see the `csi-driver-s3.*` keys below | [csi-driver-s3](https://github.com/thxcode/k8s-csi-s3) subchart (chart 0.43.7). |
| csi-driver-s3.enabled | bool | `true` | Deploy the bundled S3 CSI driver. |
| csi-driver-s3.driver.name | string | `"s3.csi.gpustack.ai"` | CSI driver name. Namespaced to gpustack so this driver coexists with a cluster's own S3 CSI driver, and matched by `kuberess.CSIProvisionerS3`, which the worker writes into the StorageClasses it provisions. |
| csi-driver-s3.images | object | `{"csiProvisioner":"docker.io/gpustack/mirrored-csi-provisioner:v6.1.0","livenessProbe":"docker.io/gpustack/mirrored-csi-livenessprobe:v2.17.0","nodeDriverRegistrar":"docker.io/gpustack/mirrored-csi-node-driver-registrar:v2.15.0","s3":"docker.io/gpustack/mirrored-csi-s3-driver:v0.43.7"}` | Mirrored images. This chart stores whole `registry/namespace/name:tag` references with no separate tag key. |
| csi-driver-s3.controller.replicas | int | `1` | Number of S3 controller (provisioner) replicas. The provisioner sidecar runs with `--leader-election`, so extra replicas stand by. Losing the controller delays volume provisioning; volumes already mounted keep working, because the mounting side is the node DaemonSet. Like the NFS chart, this one renders neither a PodDisruptionBudget nor topology spread constraints for the controller, and it honours `controller.affinity` only when that affinity carries `nodeSelectorTerms` — a pod anti-affinity is silently dropped — so replicas may share a node. |
| csi-driver-s3.controller.strategyType | string | `"Recreate"` | Deployment strategy of the S3 controller. The chart's `Recreate` takes every replica down before starting the new one, which gives up the failover `replicas` above just bought; set `RollingUpdate` alongside it. |
| csi-driver-s3.controller.name | string | `"csi-s3-controller"` | Name of the S3 controller Deployment and of the pods it owns. |
| csi-driver-s3.controller.affinity | object | `{}` | Affinity of the S3 controller pods. Honoured only when it carries `nodeSelectorTerms` (see `replicas` above), which makes it a node-placement knob and nothing else. This chart renders no `dnsPolicy` for the controller, so there is no key for it. |
| csi-driver-s3.controller.nodeSelector | object | `{}` | Node selector of the S3 controller pods. |
| csi-driver-s3.controller.priorityClassName | string | `"system-cluster-critical"` | Priority class of the S3 controller pods. `system-cluster-critical` keeps provisioning working while a node is under pressure. |
| csi-driver-s3.controller.tolerations | list | `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/master","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/controlplane","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/control-plane","operator":"Exists"},{"effect":"NoSchedule","key":"CriticalAddonsOnly","operator":"Exists"}]` | Tolerations of the S3 controller pods. The defaults let the provisioner run on control-plane nodes. |
| csi-driver-s3.node.name | string | `"csi-s3-node"` | Name of the S3 node DaemonSet and of the pods it owns. This is the mounting side: one pod per node, so it has no replica count or disruption budget to configure. |
| csi-driver-s3.node.dnsPolicy | string | `"ClusterFirstWithHostNet"` | DNS policy of the S3 node pods. `ClusterFirstWithHostNet` is what lets an in-cluster object store endpoint resolve from a host-networked mount. |
| csi-driver-s3.node.affinity | object | `{}` | Affinity of the S3 node pods. Rendered as given, unlike the controller's. |
| csi-driver-s3.node.nodeSelector | object | `{}` | Node selector of the S3 node pods, narrowing which nodes can mount S3 volumes. The chart adds `kubernetes.io/os: linux` on top of whatever is set here. |
| csi-driver-s3.node.priorityClassName | string | `"system-cluster-critical"` | Priority class of the S3 node pods. |
| csi-driver-s3.node.tolerations | list | `[{"operator":"Exists"}]` | Tolerations of the S3 node pods. The defaults tolerate every taint, so a volume can be mounted on any node the scheduler picks. |
| csi-driver-s3.nodeDriverRegistrar.livenessProbe.enabled | bool | `false` | Leave the node-driver-registrar's own liveness probe off; the node plugin already runs one on its health port. |
| csi-driver-s3.rbac.name | string | `"csi-s3"` | Prefix of the driver's ClusterRole and ClusterRoleBinding names. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs](https://github.com/norwoodj/helm-docs).
