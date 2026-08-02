# Two Install Modes

> **Purpose** — what deploys the chain (chart mode vs image mode), why the two can never run together,
> and which objects the worker must apply itself in either mode.
> **Audience** operators, contributors touching the chart or `pkg/worker/kuberess` ·
> **Prerequisites** [Architecture](../architecture.md) · **Read time** ~8 min

## Contents

- [Chart mode and image mode](#chart-mode-and-image-mode)
- [The two modes are exclusive](#the-two-modes-are-exclusive)
- [Two switches worth calling out](#two-switches-worth-calling-out)
- [The chart deploys workloads; the worker applies the custom resources](#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)

## Chart mode and image mode

Kueue, Node Feature Discovery and the two CSI drivers are **vendored subcharts** of the operator chart
(`deploy/gpustack-operator/chart/charts/`), each behind an `enabled` switch. There is exactly one
configuration surface for them — the chart's `values.yaml` — and two ways to reach it:

- **Chart mode (the default).** Helm renders everything: the worker, the device-manager DaemonSets and
  the four subcharts, all in **one release**. The worker is then started with
  `--disable-applications=*` (from `worker.disableApplications`, default `["*"]`) so it installs nothing
  at runtime and cannot collide with what the chart already deployed.
- **Image mode.** Nothing deploys the worker via Helm — it runs from a checkout or outside the cluster —
  so the worker installs the chart **packaged into its own image**
  (`${GPUSTACK_CONF_DIR:-/etc/gpustack}/charts/gpustack-operator-<version>.tgz`) with an overlay it
  computes from its own flags and settings: `worker.enabled=false`, `fullnameOverride:
  gpustack-operator`, one `enabled` per component, and the manufacturer map. The release is named
  `gpustack-operator-device-manager` — the name earlier versions gave the device-manager-only release,
  kept so that no existing cluster needs a release migration. Image mode has **no user-values channel**:
  the overlay is the whole surface, so a per-value override like `kueue.controllerManager.replicas`
  cannot be expressed there.

`--disable-applications` accepts `*` (all of them) plus `kueue`, `node-feature-discovery`,
`csi-driver-nfs`, `csi-driver-s3` and `device-manager`; the names are validated at flag-parse time
against `pkg/worker/kuberess`'s own map, which is also what renders the overlay's switches.

The `gpustack-cpu-info` NodeFeatureRule has **no name in that set and no `enabled` switch**: the
scheduling chain starts at that rule, so it is required in every mode — including when
`node-feature-discovery.enabled=false`, which is the supported way to run the chain against an NFD the
cluster already has. That is exactly why the worker applies it rather than the chart (see
[below](#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)).

## The two modes are exclusive

Both installs render the same chart under the same `fullnameOverride`, so any component enabled on both
sides produces identically named objects, and Helm refuses to import an object another release owns —
the worker's install fails on the first such object, and because it gates its startup on installing its
applications, it never starts. (Measured: `ServiceAccount "csi-nfs-controller-sa" ... invalid ownership
metadata`; Helm names whichever shared object it maps first.)

The two sides' switches are independent knobs, so handing a component over would mean disabling it in
this chart and in `worker.disableApplications` in step, through every upgrade, with nothing checking it.
Wherever this chart deploys the worker, `worker.disableApplications` keeps the `*`; image mode is for
clusters where no chart deploys the worker at all.

## Two switches worth calling out

Because they change what a mode installs:

- **`deviceManager.enabled=false`** — the chart renders no device-manager DaemonSets, and that is all it
  does. It does **not** hand that install back to the worker: with the wildcard in place the worker
  installs nothing, so the cluster simply has no device managers (useful when only the control plane is
  wanted). Before chart mode covered them, this switch was how the worker came to install them.
- **`worker.enabled=false`** — the chart deploys only the applications, which is exactly what image
  mode's overlay sets.

Migrating a cluster from the pre-subchart layout is an ownership transfer, documented in [Migrating to
the bundled subcharts](../migration/to-subcharts.md). For running more than one replica of each
control-plane component, see [High Availability](../operation/high-availability.md).

## The chart deploys workloads; the worker applies the custom resources

**A chart cannot own a custom resource whose CRD it does not ship.** Helm REST-maps the *entire*
manifest before it creates anything, so an unserved kind fails the whole install rather than degrading.
The two objects that sit on the wrong side of that line are therefore applied by the worker:

- the `gpustack-node-devices` **AdmissionCheck** — its CRD belongs to Kueue, and Kueue templates its
  CRDs, so nothing can order them ahead of a custom resource in the same render;
- the `gpustack-cpu-info` **NodeFeatureRule** — its CRD belongs to NFD, and the rule is required even
  when `node-feature-discovery.enabled=false`, which is the supported way to run against a cluster's own
  NFD. That install ships no NFD CRD at all, so a chart-owned rule fails it outright:
  `resource mapping not found ... no matches for kind "NodeFeatureRule"`.

So the division is: **the chart deploys workloads and configuration; the worker applies the custom
resources whose CRDs the chart cannot order.** It is the boundary the worker's own CRDs, aggregated
APIServices and webhook configurations already sit on, all of them installed in Go for the same reason.

The cost is that `helm template` shows neither object — the same as for those. Both are *applied*, not
created, so a repeat run only sets `spec` and never clobbers a controller-owned status.

---

**See also** — [Internals](internals.md#worker-startup-order-matters) (where in the boot these are
applied) · [Migrating to the bundled subcharts](../migration/to-subcharts.md) ·
[High Availability](../operation/high-availability.md) · [Development](../development.md#vendored-subcharts)

**Next** → [Internals](internals.md) — startup ordering and the invariants a contributor must keep.
