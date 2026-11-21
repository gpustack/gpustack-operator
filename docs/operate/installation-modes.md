# Installation Modes

The operator installs its dependencies in chart or image mode. Chart mode renders them as subcharts in one Helm
release; image mode has the worker install them at runtime. Choose one mode per cluster; both read
the operator chart's `values.yaml`.

## Contents

- [Prerequisites](#prerequisites)
- [Chart and image modes](#chart-and-image-modes)
- [Modes are exclusive](#modes-are-exclusive)
- [Configuration](#configuration)
- [Settings and precedence](#settings-and-precedence)
- [Deployed and applied resources](#deployed-and-applied-resources)

## Prerequisites

The Helm chart accepts Kubernetes 1.23 and newer. Use Kubernetes 1.29 or newer for the Kueue
scheduling chain and node model delivery. Simple allocation on 1.23–1.28 is untested.
[Image-based model delivery](../modules/model-delivery/image-source.md#prerequisites) has a higher
version requirement for Kubernetes and the container runtime.

## Chart and image modes

![Chart mode and image mode](../assets/diagrams/installation-modes-flow.svg)

<!--
flowchart TD
    CHART["Chart mode: user Helm install -> single Helm release"]
    IMAGE["Image mode: external worker -> managed release"]
    COMMON["Worker NodeFeatureRule and exclusive ownership"]
    CHART -> COMMON
    IMAGE -> COMMON
-->


Kueue, NFD, Topograph and the two CSI drivers are vendored subcharts of the operator chart
(`deploy/gpustack-operator/chart/charts/`), each behind an `enabled` switch. Their one configuration
surface is the chart's `values.yaml`, reachable through:

| Mode | Installation |
| --- | --- |
| **Chart mode** | **Default.** Helm renders the worker, Device Manager `DaemonSet`s and four subcharts in one release. The worker starts with `--disable-applications=*` (`worker.disableApplications`, default `["*"]`) and installs nothing at runtime. |
| Image mode | No Helm release deploys the worker. It runs from a checkout or outside the cluster and installs the chart packaged in its own image (`${GPUSTACK_CONF_DIR:-/etc/gpustack}/charts/gpustack-operator-<version>.tgz`) as release `gpustack-operator-device-manager`. |

In image mode, the overlay is the whole values surface, so an override like
`kueue.controllerManager.replicas` cannot be expressed. The overlay comes from the worker's flags
and settings: `worker.enabled=false`, `fullnameOverride: gpustack-operator`, one `enabled` per
component, and the manufacturer map.

> **Why that release name** — earlier versions gave it to the release that deployed only the Device Manager; keeping it
> spares existing clusters a release migration.

`--disable-applications` accepts `*` plus `kueue`, `node-feature-discovery`, `csi-driver-nfs`,
`csi-driver-s3`, `device-manager`, `model-manager`, `model-transfer`, validated at flag-parse against `pkg/worker/kuberess`'s map, which
also renders the overlay's switches.

`gpustack-cpu-info` is in neither set: that `NodeFeatureRule` has **no `enabled` switch**, since the chain
starts at it. Every mode needs it, including `node-feature-discovery.enabled=false`, the supported way
to run against the cluster's own NFD. That is why the worker applies it rather than the chart (see
[below](#deployed-and-applied-resources)).

Topograph is also absent from the image-mode application map. It is an optional provider stack,
defaults off, and requires provider credentials and security choices that an image-mode overlay must
not invent. The default chart renders no Topograph object and pulls no Topograph image.

Enable Topograph through a chart-mode release. Generic `TopologySource` discovery and the hostname-only
fallback remain available without it. See [Topology-Aware Scheduling
Operations](../modules/topology/operations.md#choose-the-inventory-path).

## Modes are exclusive

Both installs render the same chart under the same `fullnameOverride`, so a component enabled on both
sides produces identically named objects, and Helm refuses to import an object another release owns. The
worker's install fails on the first one and it never starts: startup is gated on that install.

> **Why** — measured: `ServiceAccount "csi-nfs-controller-sa" ... invalid ownership metadata`; Helm
> names whichever shared object it maps first.

Splitting components across the sides does not work either. The switches are independent, so it means
disabling a component here and in `worker.disableApplications` in step, at every upgrade, with nothing
checking it. Wherever this chart deploys the worker, `worker.disableApplications` keeps the `*`; image
mode is for clusters where no chart deploys it.

## Configuration

These switches change what a mode installs:

- `deviceManager.enabled=false` — the chart renders no Device Manager `DaemonSet`s, nothing more. It
  does **not** hand that install to the worker: with the wildcard the worker installs nothing, so the
  cluster has no Device Managers (useful for control-plane-only). Before chart mode covered them, this
  switch was how the worker came to install them.
- `modelManager.enabled=false` — no Model Manager `DaemonSet` and no `CSIDriver`, and the worker
  then seeds no `Node` delivery; see [Model Store Operations](../modules/model-delivery/operations.md#prerequisites).
- `modelTransfer.enabled=false` — no remote weight delivery source `DaemonSet`s, `Service`s, RBAC or
  TLS `Secret`s, the Model Manager omits the source ledger mount and CSI token request, and the chart
  declares `GPUSTACK_MODEL_ARTIFACT_REMOTE_DELIVERY_POLICY=Disabled` on the worker; see
  [Remote Weight Delivery](../modules/model-delivery/remote.md#configuration).
- `worker.enabled=false` — the chart deploys only the applications, what image mode's overlay sets.

## Settings and precedence

Runtime `Setting` objects (`gpustack.ai/v1`, short name `set`, stored in the `gpustack-settings` `Secret` in `gpustack-system`) can be edited with `kubectl -n gpustack-system patch setting <name> --type merge -p '{"spec":{"value":"..."}}'`. On every worker start (restart, drain, or upgrade), the worker aligns each `Setting` in four ordered rules:

| Rule | Precedence | Behavior |
| --- | --- | --- |
| 1. `Backfill` | Missing key only | Seeds an absent key from `GPUSTACK_<UPPER_SNAKE_NAME>` when set, else the built-in default. |
| 2. `env-declared` | Wins on every start | Any non-blank `GPUSTACK_*` variable on the worker `Deployment` (from chart values such as `global.imageRegistry`, `global.imagePullPolicy`, `modelManager.enabled`, `modelTransfer.enabled`, or `worker.env`) overwrites the stored `Setting`. |
| 3. `default-tracked` | Wins when no env claims it | If `settings-default-tracking` names the `Setting` (built-in image `Setting`s are tracked by default), the operator's built-in default overwrites the stored value on every start. |
| 4. Administrator-owned | Preserved | Every other `Setting` keeps its stored value across worker restarts and upgrades. |

### Reverted setting after restart

If a runtime `Setting` edit reverts after a worker restart or upgrade, check which startup rule overwrote it:

```bash
# 1. Inspect worker startup override logs (names rule="env-declared" or rule="default-tracked" and envName):
kubectl -n gpustack-system logs deploy/gpustack-operator-worker | grep 'setting overridden on start'

# 2. Check non-blank GPUSTACK_* variables rendered on the worker Deployment:
kubectl -n gpustack-system get deploy gpustack-operator-worker -o yaml | grep -A1 'GPUSTACK_'

# 3. Check whether settings-default-tracking lists the Setting:
kubectl -n gpustack-system get setting settings-default-tracking -o yaml
```

- **`rule="env-declared"`**: Update or clear the owning Helm chart value (an empty string `""` claims nothing and hands ownership back) or `worker.env` entry, then run `helm upgrade`.
- **`rule="default-tracked"`**: Remove that `Setting` name from `settings-default-tracking` with `kubectl -n gpustack-system patch setting settings-default-tracking --type merge -p '{"spec":{"value":"<remaining-comma-separated-names>"}}'`, then apply your `Setting` pin.

## Deployed and applied resources

A chart cannot own a custom resource whose CRD it does not ship. Helm REST-maps the *entire*
manifest before creating anything, so an unserved kind fails the whole install rather than degrading.
The worker applies these resources after their CRDs become available:

- the `gpustack-node-devices` and `gpustack-model-deployment-joint` `AdmissionCheck`s. Their CRD belongs
  to Kueue, which templates its CRDs, so nothing can order it ahead of a custom resource in the same
  render;
- the `gpustack-cpu-info` `NodeFeatureRule`, whose CRD belongs to NFD. The rule is required even
  when `node-feature-discovery.enabled=false`; that install ships no NFD CRD, so a chart-owned rule
  fails outright: `resource mapping not found ... no matches for kind "NodeFeatureRule"`.

Both `AdmissionCheck`s are created in chart mode and image mode, including when applications are
disabled. You do not create them manually. The worker installs them at startup; their controllers
mark them `Active` before queues reference them.

| `AdmissionCheck` | Purpose | Referencing queues |
| --- | --- | --- |
| `gpustack-node-devices` | Checks whether individual accelerators can satisfy a request | Accelerated queues while `instance-type-derived-from-node` is enabled |
| `gpustack-model-deployment-joint` | Coordinates admission across a `ModelDeployment`'s roles | Every operator-managed queue, including CPU queues and queues for administrator-authored `InstanceType`s |

The joint check immediately passes workloads outside a multi-role `ModelDeployment`, including
single-role deployments. The table above describes when each queue references a check.

The chart deploys workloads and configuration; the worker applies the custom resources whose CRDs
the chart cannot order. The worker's own CRDs, aggregated `APIService`s and webhook configurations
are also installed by the worker. `helm template` therefore does not show them. Repeated startup
updates their desired configuration and preserves controller-owned status.

No release owns them either, so `helm uninstall` leaves them behind. Both `AdmissionCheck`s go with
Kueue's CRDs; `files/cleanup.sh` deletes the `NodeFeatureRule`, but only while it carries the
`app.kubernetes.io/part-of: gpustack-operator` label the worker puts on it.

---

**See also** — [Migrating to Bundled Subcharts](migration/bundled-subcharts.md) (the ownership
transfer out of the pre-subchart layout) · [High Availability Operations](high-availability.md) (more
than one replica per control-plane component) · [Topology-Aware Scheduling
Operations](../modules/topology/operations.md) (Topograph and generic inventory)

**Next** → [High Availability Operations](high-availability.md) — configure control-plane replicas and disruption budgets.
