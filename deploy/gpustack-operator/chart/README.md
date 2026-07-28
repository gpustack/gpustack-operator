# gpustack-operator

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) and Kueue.

## Introduction

This chart deploys the GPUStack Operator control plane (`gpustack-operator-worker`) and
the per-manufacturer device managers (`gpustack-operator-device-manager-<manufacturer>`)
onto a Kubernetes cluster using the [Helm](https://helm.sh) package manager.

The operator turns raw node hardware into a [Kueue](https://kueue.sigs.k8s.io)-based
scheduling chain for accelerators (GPU/NPU/TPU), built on
[Node Feature Discovery](https://kubernetes-sigs.github.io/node-feature-discovery) and Kueue.

> Node Feature Discovery, Kueue and the CSI drivers are bundled into the operator image and
> installed by the worker at runtime, so they are not chart dependencies.

## Prerequisites

Kubernetes: `>=1.23.0-0`
- Helm 3.8.0+
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
| global | object | `{"imageNamespace":"","imagePullPolicy":"","imagePullSecrets":[],"imageRegistry":"","nodeSelector":{}}` | Global values shared across the chart and its subcharts. |
| global.imageRegistry | string | `""` | Image registry override applied to every image this chart renders, the subcharts' workloads, sidecars and hook Jobs included (e.g. "docker.io"). When empty, the registry each image reference already encodes is used as-is. |
| global.imageNamespace | string | `""` | Image namespace override applied to every image this chart renders, the subcharts' workloads, sidecars and hook Jobs included. When non-empty it replaces the namespace segment of each image reference (e.g. "gpustack"). |
| global.imagePullPolicy | string | `""` | Image pull policy applied to every workload, the subcharts' included. It outranks a component's own pull policy; when empty, each component's own is used. |
| global.imagePullSecrets | list | `[]` | Image pull secrets applied to all workloads. |
| global.nodeSelector | object | `{}` | Node selector applied to all workloads when a component does not set its own. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| fullnameOverride | string | `""` | Override the fully qualified release name used as the base of resource names. |
| namespaceOverride | string | `""` | Namespace to deploy the resources into; defaults to the release namespace. |
| commonLabels | object | `{}` | Extra labels added to every resource. |
| commonAnnotations | object | `{}` | Extra annotations added to every resource. |
| image | object | `{"pullPolicy":"IfNotPresent","repository":"gpustack/gpustack-operator","tag":""}` | Default operator image (shared by the worker and the device-manager). Each component may override any of these keys via its own `image` block. |
| image.repository | string | `"gpustack/gpustack-operator"` | Operator image repository (shared by the worker and the device-manager). |
| image.tag | string | `""` | Operator image tag; defaults to "v<.Chart.AppVersion>" when empty. |
| image.pullPolicy | string | `"IfNotPresent"` | Operator image pull policy. |
| manufacturers | object | `{"amd":"1002","ascend":"19e5","cambricon":"cabc","hygon":"1d94","iluvatar":"1e3e","metax":"9999","mthreads":"1ed5","nvidia":"10de","thead":"1ded"}` | Accelerator manufacturers to manage, mapped to their PCI vendor ID. This is the single source for the worker `--manufacturer` list, the per-manufacturer device-manager DaemonSets, and the GPUSTACK_<MANUFACTURER>_PCI_VENDOR_ID environment variable propagated to both the worker and the device-managers. The manufacturer list is derived from these keys (sorted for stable rendering); each device-manager DaemonSet only schedules on nodes labelled by NFD with the matching PCI vendor. |
| worker.enabled | bool | `true` | Deploy the worker (control plane). Disable to deploy only the device-managers. |
| worker.certmanager | object | `{"enabled":"auto","issuer":{"kind":"Issuer","name":""}}` | Configure cert-manager integration for the worker webhook serving certificate. |
| worker.certmanager.enabled | string | `"auto"` | Whether to issue the worker webhook serving certificate with cert-manager. Accepts a string (or the equivalent boolean): "auto" creates the cert-manager resources only when the cert-manager CRDs are detected in the cluster; "true" always creates them (cert-manager must be installed); "false" never creates them and the worker generates and manages its own certificate. |
| worker.certmanager.issuer.name | string | `""` | Existing issuer name to use; when empty a self-signed Issuer is created. |
| worker.certmanager.issuer.kind | string | `"Issuer"` | Issuer kind ("Issuer" or "ClusterIssuer"); only used when `issuer.name` is set. |
| worker.replicas | int | `1` | Number of operator (control plane) replicas. |
| worker.securePort | int | `31443` | Secure serving port of the worker container. |
| worker.image | object | `{}` | Per-worker image overrides (`repository`/`tag`/`pullPolicy`); unset keys fall back to the chart-level `image`. |
| worker.dataDir | string | `"/var/lib/gpustack"` | Host path mounted at /var/lib/gpustack for persistent operator data. |
| worker.resources | object | `{"limits":{"cpu":"4","memory":"8Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits for the worker container. |
| worker.nodeSelector | object | `{}` | Node selector for the worker; falls back to `global.nodeSelector` when empty. |
| worker.tolerations | list | `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/master","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/controlplane","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/control-plane","operator":"Exists"},{"effect":"NoSchedule","key":"CriticalAddonsOnly","operator":"Exists"}]` | Tolerations for the worker. Defaults let the control plane run on control-plane/master nodes. |
| worker.extraArgs | list | `[]` | Extra command-line arguments appended to the worker container args. |
| worker.env | object | `{}` | Extra environment variables for the worker, as a name/value map. |
| deviceManager.enabled | bool | `true` | Deploy the per-manufacturer device-manager DaemonSets. |
| deviceManager.securePort | int | `32443` | Secure serving port of the device-manager container. |
| deviceManager.image | object | `{}` | Per-device-manager image overrides (`repository`/`tag`/`pullPolicy`); unset keys fall back to the chart-level `image`. |
| deviceManager.createRuntimeClasses | bool | `true` | Create the nvidia/mthreads RuntimeClass objects. Disable when the cluster already provides them (e.g. via the NVIDIA GPU operator). |
| deviceManager.resources | object | `{"limits":{"cpu":"4","memory":"8Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits for each device-manager container. |
| deviceManager.tolerations | list | `[{"operator":"Exists"}]` | Tolerations for the device-manager DaemonSets. Defaults tolerate every taint so a manager schedules on any node carrying the matching accelerator. |
| deviceManager.extraArgs | list | `[]` | Extra command-line arguments appended to each device-manager container args. |
| deviceManager.env | object | `{}` | Extra environment variables for the device-manager, as a name/value map. |
| kueue | object | see the `kueue.*` keys below | [Kueue](https://kueue.sigs.k8s.io) subchart (chart 0.18.4). |
| kueue.enabled | bool | `true` | Deploy the bundled Kueue. |
| kueue.fullnameOverride | string | `"kueue"` | Name the Kueue resources after a fixed "kueue" prefix instead of the release, so that this subchart and the runtime install render identical resource names. |
| kueue.controllerManager.tolerations | list | `[{"operator":"Exists"}]` | Tolerations for the Kueue controller manager. Defaults tolerate every taint, so the manager keeps admitting workloads wherever the control plane runs. |
| kueue.controllerManager.manager.image.repository | string | `"docker.io/gpustack/mirrored-kueue"` | Kueue controller manager image repository. |
| kueue.controllerManager.manager.image.tag | string | `"v0.18.4"` | Kueue controller manager image tag. Both Kueue lines run this same image; only the CRD schema their charts ship differs. |
| kueue.controllerManager.manager.image.pullPolicy | string | `"IfNotPresent"` | Kueue controller manager image pull policy. |
| kueue.controllerManager.manager.podAnnotations | object | `{"gpustack.ai/managed":"true"}` | Annotations of the Kueue controller manager pods, marking them as managed by the operator. |
| kueue.controllerManager.manager.resources | object | `{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits for the Kueue controller manager container. |
| kueue.managerConfig.controllerManagerConfigYaml | string | controllerManagerConfigYaml | Kueue's controller_manager_config.yaml, delivered through the manager-config ConfigMap. The "resources.transformations" list is generated from `pkg/nodefeature` by `make generate chart` between its markers; everything else is maintained here. The managed-jobs namespace selector names "gpustack-system" literally because Helm merges subchart values instead of rendering them, so the release namespace cannot reach this string. |
| node-feature-discovery | object | `{"enabled":true}` | [Node Feature Discovery](https://kubernetes-sigs.github.io/node-feature-discovery) subchart (chart 0.19.0). |
| node-feature-discovery.enabled | bool | `true` | Deploy the bundled Node Feature Discovery. |
| csi-driver-nfs | object | `{"enabled":true}` | [csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) subchart (chart 4.13.2). |
| csi-driver-nfs.enabled | bool | `true` | Deploy the bundled NFS CSI driver. |
| csi-driver-s3 | object | `{"enabled":true}` | [csi-driver-s3](https://github.com/thxcode/k8s-csi-s3) subchart (chart 0.43.7). |
| csi-driver-s3.enabled | bool | `true` | Deploy the bundled S3 CSI driver. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs](https://github.com/norwoodj/helm-docs).
