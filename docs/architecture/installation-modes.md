# Installation Modes

> **Purpose** — what deploys the chain (chart mode vs image mode), why the two can never run together,
> and which objects the worker must apply itself in either mode.
> **Audience** operators, contributors touching the chart or `pkg/worker/kuberess` ·
> **Prerequisites** [Architecture](../architecture.md) · **Read time** ~3 min

## Contents

- [Chart mode and image mode](#chart-mode-and-image-mode)
- [The two modes are exclusive](#the-two-modes-are-exclusive)
- [Two switches worth calling out](#two-switches-worth-calling-out)
- [The chart deploys workloads; the worker applies the custom resources](#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)

## Chart mode and image mode

Kueue, NFD and the two CSI drivers are **vendored subcharts** of the operator chart
(`deploy/gpustack-operator/chart/charts/`), each behind an `enabled` switch. Their one configuration
surface is the chart's `values.yaml`, reachable two ways:

- **Chart mode (the default).** Helm renders the worker, the device-manager DaemonSets and the four
  subcharts in **one release**; the worker starts with `--disable-applications=*`
  (`worker.disableApplications`, default `["*"]`) and installs nothing at runtime.
- **Image mode.** No Helm release deploys the worker; it runs from a checkout or outside the cluster and
  installs the chart **packaged into its own image**
  (`${GPUSTACK_CONF_DIR:-/etc/gpustack}/charts/gpustack-operator-<version>.tgz`) as release
  `gpustack-operator-device-manager`. Its overlay — from the worker's own flags and settings:
  `worker.enabled=false`, `fullnameOverride: gpustack-operator`, one `enabled` per component, the
  manufacturer map — is the **whole** values surface, so an override like
  `kueue.controllerManager.replicas` cannot be expressed.

> **Why that release name** — earlier versions gave it to the device-manager-only release; keeping it
> spares existing clusters a release migration.

`--disable-applications` accepts `*` plus `kueue`, `node-feature-discovery`, `csi-driver-nfs`,
`csi-driver-s3`, `device-manager`, validated at flag-parse against `pkg/worker/kuberess`'s map, which
also renders the overlay's switches.

`gpustack-cpu-info` is in neither set: that NodeFeatureRule has **no `enabled` switch**, since the chain
starts at it. Every mode needs it, including `node-feature-discovery.enabled=false` — the supported way
to run against the cluster's own NFD. Hence the worker applies it, not the chart (see
[below](#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)).

## The two modes are exclusive

Both installs render the same chart under the same `fullnameOverride`, so a component enabled on both
sides produces identically named objects, and Helm refuses to import an object another release owns. The
worker's install fails on the first one and it never starts: startup is gated on that install.

> **Why** — measured: `ServiceAccount "csi-nfs-controller-sa" ... invalid ownership metadata`; Helm
> names whichever shared object it maps first.

Splitting components across the sides is no way out: the switches are independent, so it means
disabling a component here and in `worker.disableApplications` in step, every upgrade, with nothing
checking it. Wherever this chart deploys the worker, `worker.disableApplications` keeps the `*`; image
mode is for clusters where no chart deploys it.

## Two switches worth calling out

Because they change what a mode installs:

- **`deviceManager.enabled=false`** — the chart renders no device-manager DaemonSets, nothing more. It
  does **not** hand that install to the worker: with the wildcard the worker installs nothing, so the
  cluster has no device managers (useful for control-plane-only). Before chart mode covered them, this
  switch was how the worker came to install them.
- **`worker.enabled=false`** — the chart deploys only the applications, what image mode's overlay sets.

## The chart deploys workloads; the worker applies the custom resources

**A chart cannot own a custom resource whose CRD it does not ship.** Helm REST-maps the *entire*
manifest before creating anything, so an unserved kind fails the whole install rather than degrading.
Two objects sit on the wrong side of that line, so the worker applies them:

- the `gpustack-node-devices` **AdmissionCheck** — its CRD belongs to Kueue, which templates its CRDs,
  so nothing can order them ahead of a custom resource in the same render;
- the `gpustack-cpu-info` **NodeFeatureRule** — its CRD belongs to NFD, and the rule is required even
  when `node-feature-discovery.enabled=false`; that install ships no NFD CRD, so a chart-owned rule
  fails outright: `resource mapping not found ... no matches for kind "NodeFeatureRule"`.

The division: **the chart deploys workloads and configuration; the worker applies the custom resources
whose CRDs the chart cannot order** — the boundary the worker's own CRDs, aggregated APIServices and
webhook configurations already sit on, in Go for the same reason. The cost: `helm template` shows none
of them. All are *applied*, not created, so a repeat run only sets `spec`, never clobbering a
controller-owned status.

---

**See also** — [Migrating to Bundled Subcharts](../migration/to-subcharts.md) (the ownership
transfer out of the pre-subchart layout) · [High Availability Operations](../operation/high-availability.md) (more
than one replica per control-plane component) · [Development](../development.md#vendored-subcharts)

**Next** → [Internals](internals.md) — startup ordering and the invariants a contributor must keep.
