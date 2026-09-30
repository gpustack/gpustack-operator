# Installation Modes

The operator installs its dependencies two ways. Chart mode renders them as subcharts in one Helm
release; image mode has the worker install them at runtime. Choose one mode per cluster; both read
the operator chart's `values.yaml`.

## Contents

- [Chart mode and image mode](#chart-mode-and-image-mode)
- [The two modes are exclusive](#the-two-modes-are-exclusive)
- [Notable switches](#notable-switches)
- [Chart-deployed and worker-applied resources](#chart-deployed-and-worker-applied-resources)

## Chart mode and image mode

Kueue, NFD, Topograph and the two CSI drivers are vendored subcharts of the operator chart
(`deploy/gpustack-operator/chart/charts/`), each behind an `enabled` switch. Their one configuration
surface is the chart's `values.yaml`, reachable two ways:

- Chart mode (the default): Helm renders the worker, the device-manager DaemonSets and the four
  subcharts in one release; the worker starts with `--disable-applications=*`
  (`worker.disableApplications`, default `["*"]`) and installs nothing at runtime.
- Image mode: no Helm release deploys the worker. It runs from a checkout or outside the cluster and
  installs the chart packaged into its own image
  (`${GPUSTACK_CONF_DIR:-/etc/gpustack}/charts/gpustack-operator-<version>.tgz`) as release
  `gpustack-operator-device-manager`.

  Its overlay is the whole values surface, so an override like `kueue.controllerManager.replicas`
  cannot be expressed. The overlay comes from the worker's own flags and settings: `worker.enabled=false`,
  `fullnameOverride: gpustack-operator`, one `enabled` per component, and the manufacturer map.

> **Why that release name** — earlier versions gave it to the device-manager-only release; keeping it
> spares existing clusters a release migration.

`--disable-applications` accepts `*` plus `kueue`, `node-feature-discovery`, `csi-driver-nfs`,
`csi-driver-s3`, `device-manager`, `model-manager`, validated at flag-parse against `pkg/worker/kuberess`'s map, which
also renders the overlay's switches.

`gpustack-cpu-info` is in neither set: that NodeFeatureRule has **no `enabled` switch**, since the chain
starts at it. Every mode needs it, including `node-feature-discovery.enabled=false`, the supported way
to run against the cluster's own NFD. That is why the worker applies it rather than the chart (see
[below](#chart-deployed-and-worker-applied-resources)).

Topograph is also absent from the image-mode application map. It is an optional provider stack,
defaults off, and requires provider credentials and security choices that an image-mode overlay must
not invent. The default chart renders no Topograph object and pulls no Topograph image.

Enable Topograph through a chart-mode release. Generic `TopologySource` discovery and the hostname-only
fallback remain available without it. See [Topology-Aware Scheduling
Operations](../modules/topology/operations.md#choose-the-inventory-path).

## The two modes are exclusive

Both installs render the same chart under the same `fullnameOverride`, so a component enabled on both
sides produces identically named objects, and Helm refuses to import an object another release owns. The
worker's install fails on the first one and it never starts: startup is gated on that install.

> **Why** — measured: `ServiceAccount "csi-nfs-controller-sa" ... invalid ownership metadata`; Helm
> names whichever shared object it maps first.

Splitting components across the sides does not work either. The switches are independent, so it means
disabling a component here and in `worker.disableApplications` in step, at every upgrade, with nothing
checking it. Wherever this chart deploys the worker, `worker.disableApplications` keeps the `*`; image
mode is for clusters where no chart deploys it.

## Notable switches

Because they change what a mode installs:

- `deviceManager.enabled=false` — the chart renders no device-manager DaemonSets, nothing more. It
  does **not** hand that install to the worker: with the wildcard the worker installs nothing, so the
  cluster has no device managers (useful for control-plane-only). Before chart mode covered them, this
  switch was how the worker came to install them.
- `modelManager.enabled=false` — no model-manager DaemonSet and no CSIDriver, and the worker
  then seeds no `Node` delivery; see [Model Store Operations](../modules/model-delivery/operations.md#enable-it).
- `worker.enabled=false` — the chart deploys only the applications, what image mode's overlay sets.

## Chart-deployed and worker-applied resources

A chart cannot own a custom resource whose CRD it does not ship. Helm REST-maps the *entire*
manifest before creating anything, so an unserved kind fails the whole install rather than degrading.
The worker applies three resources after their CRDs become available:

- the `gpustack-node-devices` and `gpustack-model-deployment-joint` AdmissionChecks. Their CRD belongs
  to Kueue, which templates its CRDs, so nothing can order it ahead of a custom resource in the same
  render;
- the `gpustack-cpu-info` NodeFeatureRule, whose CRD belongs to NFD. The rule is required even
  when `node-feature-discovery.enabled=false`; that install ships no NFD CRD, so a chart-owned rule
  fails outright: `resource mapping not found ... no matches for kind "NodeFeatureRule"`.

Both AdmissionChecks are created in chart mode and image mode, including when applications are
disabled. You do not create them manually. The worker installs them at startup; their controllers
mark them `Active` before queues reference them.

| AdmissionCheck | Purpose | Referencing queues |
| --- | --- | --- |
| `gpustack-node-devices` | Checks whether individual accelerators can satisfy a request | Accelerated queues while `instance-type-derived-from-node` is enabled |
| `gpustack-model-deployment-joint` | Coordinates admission across a ModelDeployment's roles | Every operator-managed queue, including CPU queues and queues for administrator-authored InstanceTypes |

The joint check immediately passes workloads outside a multi-role ModelDeployment, including
single-role deployments. The table above describes when each queue references a check.

The chart deploys workloads and configuration; the worker applies the custom resources whose CRDs
the chart cannot order. The worker's own CRDs, aggregated APIServices and webhook configurations
are also installed by the worker. `helm template` therefore does not show them. Repeated startup
updates their desired configuration and preserves controller-owned status.

No release owns them either, so `helm uninstall` leaves them behind. Both AdmissionChecks go with
Kueue's CRDs; `files/cleanup.sh` deletes the NodeFeatureRule, but only while it carries the
`app.kubernetes.io/part-of: gpustack-operator` label the worker puts on it.

---

**See also** — [Migrating to Bundled Subcharts](migration/to-subcharts.md) (the ownership
transfer out of the pre-subchart layout) · [High Availability Operations](high-availability.md) (more
than one replica per control-plane component) · [Topology-Aware Scheduling
Operations](../modules/topology/operations.md) (Topograph and generic inventory) ·
[Development](../contribute/development.md#vendored--patched-dependencies)

**Next** → [Internals](../contribute/internals.md) — startup ordering and the invariants a contributor must keep.
