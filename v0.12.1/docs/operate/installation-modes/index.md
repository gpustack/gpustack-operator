# Installation Modes

GPUStack Operator installs its dependencies in chart mode or image mode. Chart mode renders bundled
subcharts inside one Helm release; image mode lets an external worker install its packaged chart at
runtime. Choose one mode per cluster.

## Contents

- [Prerequisites](#prerequisites)
- [Chart and image modes](#chart-and-image-modes)
- [Installation ownership conflicts](#installation-ownership-conflicts)
- [Configuration](#configuration)
- [Settings and precedence](#settings-and-precedence)
- [Worker-managed resources](#worker-managed-resources)

## Prerequisites

The Helm chart accepts Kubernetes 1.23 and newer. Use Kubernetes 1.29 or newer for the Kueue
scheduling chain and node model delivery; simple allocation on 1.23–1.28 is untested.
[Model Image Source](/gpustack-operator/v0.12.1/docs/modules/model-delivery/artifact/index.md#image-prerequisites) requires newer
Kubernetes and container-runtime versions.

## Chart and image modes

![Chart mode and image mode](/gpustack-operator/v0.12.1/docs/assets/diagrams/installation-modes-flow.svg)

<!--
flowchart TD
    CHART["Chart mode: user Helm install -> single Helm release"]
    IMAGE["Image mode: external worker -> managed release"]
    COMMON["Worker NodeFeatureRule and exclusive ownership"]
    CHART -> COMMON
    IMAGE -> COMMON
-->

Kueue, NFD, Topograph, and the NFS and S3 CSI drivers ship under `deploy/gpustack-operator/chart/charts/`
behind per-subchart `enabled` switches:

| Mode | Installation |
|---|---|
| **Chart mode** | **Default.** Helm renders the worker, Device Manager `DaemonSet`s, and enabled subcharts in one release. The worker runs with `--disable-applications=*` (`worker.disableApplications`, default `["*"]`) and installs no Helm release at runtime. |
| Image mode | No Helm release deploys the worker. An external worker installs `${GPUSTACK_CONF_DIR:-/etc/gpustack}/charts/gpustack-operator-<version>.tgz` as release `gpustack-operator-device-manager`[^1]. |

[^1]: Image mode retains the historical `gpustack-operator-device-manager` release name so existing image-mode clusters upgrade in place.

In image mode, the worker generates its values overlay (`worker.enabled=false`,
`fullnameOverride: gpustack-operator`, component switches, and manufacturer map) from flags and
settings, so subchart fields such as `kueue.controllerManager.replicas` cannot be customized there.

`--disable-applications` accepts `*`, `kueue`, `node-feature-discovery`, `csi-driver-nfs`,
`csi-driver-s3`, `device-manager`, `model-manager`, and `model-transfer`.

Two components sit outside `--disable-applications`:

- **`gpustack-cpu-info`**: the worker always applies this `NodeFeatureRule`, including when
  `node-feature-discovery.enabled=false` hands feature discovery to an existing NFD installation.
- **Topograph**: disabled by default and configurable only in chart mode because it requires provider
  credentials and trust settings. `TopologySource` snapshots and hostname fallback work without it
  ([Topology-Aware Scheduling Operations](/gpustack-operator/v0.12.1/docs/modules/topology/operations/index.md#topology-inventory-sources)).

## Installation ownership conflicts

Both modes render the same chart under `fullnameOverride: gpustack-operator`. Enabling a component in
both modes creates identically named objects, and Helm rejects adoption with
`invalid ownership metadata`, blocking worker startup.

Do not split components across chart mode and image mode either; keeping `enabled` flags and
`worker.disableApplications` synchronized across upgrades has no safeguard. Keep
`worker.disableApplications: ["*"]` whenever Helm deploys the worker.

## Configuration

Use these chart switches to scope what chart mode deploys:

- **`deviceManager.enabled=false`**: omits Device Manager `DaemonSet`s (for control-plane-only
  installs). With `worker.disableApplications: ["*"]`, the worker does not install them at runtime.
- **`modelManager.enabled=false`**: omits the Model Manager `DaemonSet`, `CSIDriver`, and `Node`
  delivery defaults ([Model Store Operations](/gpustack-operator/v0.12.1/docs/modules/model-delivery/operations/index.md#prerequisites)).
- **`modelTransfer.enabled=false`**: omits remote-delivery `DaemonSet`s, `Service`s, RBAC, TLS
  `Secret`s, and source-ledger mounts, and sets
  `GPUSTACK_MODEL_ARTIFACT_REMOTE_DELIVERY_POLICY=Disabled` on the worker
  ([Remote Weight Delivery](/gpustack-operator/v0.12.1/docs/modules/model-delivery/remote/index.md#configuration)).
- **`worker.enabled=false`**: deploys only node and subchart components (used by image mode's runtime
  overlay).

## Settings and precedence

Runtime `Setting` objects (`gpustack.ai/v1`, short name `set`, backed by `Secret/gpustack-settings` in
`gpustack-system`) can be patched live with
`kubectl -n gpustack-system patch setting <name> --type merge -p '{"spec":{"value":"..."}}'`. On every
worker start, restart, or upgrade, the worker reconciles each `Setting` in four ordered rules:

| Rule | Precedence | Behavior |
|---|---|---|
| 1. `Backfill` | Missing key only | Seeds an absent `Setting` from `GPUSTACK_<UPPER_SNAKE_NAME>` if set, else the built-in default. |
| 2. `env-declared` | Wins on every start | Any non-blank `GPUSTACK_*` variable on the worker `Deployment` (from `global.imageRegistry`, `global.imagePullPolicy`, `modelManager.enabled`, `modelTransfer.enabled`, or `worker.env`) overwrites the stored `Setting`. |
| 3. `default-tracked` | Wins if unclaimed by env | If `settings-default-tracking` lists the `Setting` (built-in image `Setting`s are tracked by default), the built-in default overwrites the stored value on start. |
| 4. Administrator-owned | Preserved | Every other `Setting` keeps its stored value across restarts and upgrades. |

### Reverted setting after restart

If a patched `Setting` reverts after a worker restart or upgrade, inspect which startup rule overwrote
it:

```bash
# 1. Read worker startup override logs (prints rule="env-declared" or rule="default-tracked" and envName):
kubectl -n gpustack-system logs deploy/gpustack-operator-worker | grep 'setting overridden on start'

# 2. Inspect non-blank GPUSTACK_* variables on the worker Deployment:
kubectl -n gpustack-system get deploy gpustack-operator-worker -o yaml | grep -A1 'GPUSTACK_'

# 3. Inspect settings-default-tracking:
kubectl -n gpustack-system get setting settings-default-tracking -o yaml
```

- **`rule="env-declared"`**: update or clear the owning Helm value (an empty string `""` releases env
  ownership) or `worker.env` entry, then run `helm upgrade`.
- **`rule="default-tracked"`**: remove that `Setting` name from `settings-default-tracking` with
  `kubectl -n gpustack-system patch setting settings-default-tracking --type merge -p '{"spec":{"value":"<remaining-comma-separated-names>"}}'`,
  then re-apply your `Setting` value.

## Worker-managed resources

Helm validates REST mappings for the entire render before creating objects, so a chart cannot render a
custom resource whose CRD is templated in the same install or supplied externally. Instead, the worker
applies these objects after their CRDs become established:

- **`gpustack-node-devices` and `gpustack-model-deployment-joint` `AdmissionCheck`s**: created in
  both modes once Kueue's templated CRDs exist, and marked `Active` before queues reference them.
- **`gpustack-cpu-info` `NodeFeatureRule`**: applied once NFD's CRD exists, including when
  `node-feature-discovery.enabled=false` omits bundled NFD CRDs.

| `AdmissionCheck` | Purpose | Referencing queues |
|---|---|---|
| `gpustack-node-devices` | Verifies per-accelerator fit before placement | Accelerated queues while `instance-type-derived-from-node` is `true` |
| `gpustack-model-deployment-joint` | Holds multi-role `ModelDeployment` replicas for atomic gang admission | Every operator-managed queue, including CPU and custom `InstanceType` queues |

`gpustack-model-deployment-joint` passes single-role deployments and standalone workloads immediately.

The worker also installs and updates its own CRDs, aggregated `APIService`s, and webhook
configurations at startup while preserving `status`, so `helm template` omits them and
`helm uninstall` leaves them behind. Kueue's `AdmissionCheck`s are removed with Kueue's CRDs, and
`deploy/gpustack-operator/chart/files/cleanup.sh` deletes `gpustack-cpu-info` while it retains
`app.kubernetes.io/part-of: gpustack-operator`.

---

**See also** — [Migrating to Bundled Subcharts](/gpustack-operator/v0.12.1/docs/operate/migration/bundled-subcharts/index.md) (transferring ownership
from legacy runtime releases) · [High Availability Operations](/gpustack-operator/v0.12.1/docs/operate/high-availability/index.md) (multi-replica
control-plane configuration) · [Topology-Aware Scheduling Operations](/gpustack-operator/v0.12.1/docs/modules/topology/operations/index.md)
(Topograph and generic topology sources)

**Next** → [High Availability Operations](/gpustack-operator/v0.12.1/docs/operate/high-availability/index.md) — configure control-plane replicas and disruption budgets.
