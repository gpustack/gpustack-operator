# GPU Instances

An `Instance` runs an interactive container workspace in a Kubernetes `Pod` on whole, sliced, or
partitioned accelerators, or on CPU-only capacity. When you attach an `InstanceSSHPublicKey`, the
operator adds an SSH sidecar that enters the workload container.

## Contents

- [Overview](#overview)
- [Pool selection](#pool-selection)
- [Unit resource sizing](#unit-resource-sizing)
- [Quickstart](#quickstart)
- [Troubleshooting](#troubleshooting)

## Overview

`InstanceType` defines pool capacity and unit sizing, and `Instance` declares the workspace image,
ports, and volumes. The worker reconciles the backing `Pod` through Kueue admission and maintains
an optional NodePort `Service`.

![Instance creation and access flow](../../assets/diagrams/instances-flow.svg)

<!--
flowchart TD
    TYPE["InstanceType pool"] -> INSTANCE["Instance workspace specification"]
    KEY["Optional InstanceSSHPublicKey"] -> INSTANCE
    INSTANCE -> POD["Workspace Pod and volume reconciliation"]
    INSTANCE -> SERVICE["Configured NodePort and optional isolation"]
    POD -> ADMISSION["Kueue admission and device allocation"]
    ADMISSION -> WORKSPACE["Running accelerator workspace"]
    SERVICE -> WORKSPACE
-->

## Pool selection

List available `InstanceType`s (`instype`) and read-only `InstanceTypeFlavor`s (`instypeflavor`)
before creating a workspace:

```bash
kubectl get instancetypes -o wide
kubectl get instancetypeflavors
```

Each `InstanceType` fronts one canonical pool `ClusterQueue` and reports `onceMaxRequest/remaining`
across four accelerator views (`EX` whole cards, `SH` shared 10-slot cards, `SL` logical VRAM
percentages, and `PT` hardware partitions). With `-o wide`, `PARTITIONS` lists remaining counts per
MIG profile, and `CPU` reports capacity for CPU-only types.

With `instance-type-derived-from-node` enabled (default `true`), the operator creates one derived
`InstanceType` per hardware pool (`gpustack--<group>-<os>-<arch>`). By default, accelerated nodes
also contribute to the generic CPU pool (`instance-type-mixed-on-node: true`), and pools collapse
CPU vendors (`instance-type-aware-cpu-manufacturer: false`).

## Unit resource sizing

Derived `InstanceType`s stamp *`spec.unitResources`* (*`cpu`*, *`ram`*) per whole accelerator from
hardware presets, plus a fixed `100Gi` *`spec.localStorage`*. Unlisted accelerators fall back to
`4` CPU and `16Gi` RAM, and CPU-only derived types use `1` CPU and `2Gi` RAM.

- **Default and cap**: Omitted *`cpu`* or *`ram`* defaults from *`unitResources`* (scaled by card
  count or VRAM share) and caps explicit requests.
- **Storage default**: Omitted *`resources.localStorage`* defaults to `15Gi`, capped at the
  `InstanceType`'s *`spec.localStorage`*.
- **Overcommit**: With `instance-general-resources-overcommit` `true` (default), limits keep the
  sized value while requests scale per unit: `100m` CPU per rounded-up core on acceleratable types
  (`800m` per core on CPU-only types, capped at the limit — for example, a `4`-core CPU-only limit
  requests `3200m`), `128Mi` RAM per GiB, and one-eighth *`localStorage`*.
- **Disabling overcommit**: Setting `instance-general-resources-overcommit` to `false` sets
  container requests equal to limits.
- **Upgrade immutability**: *`spec.unitResources`* and *`spec.localStorage`* freeze at creation;
  derived `InstanceType`s are create-only and retain existing values across upgrades.
- **Pool resizing**: Author a [custom sibling `InstanceType`](configuration.md#custom-instancetype)
  to keep running `Instance`s alive. Deleting a derived type persists *`spec.stop: true`* on its
  `Instance`s so they remain `Stopped` even after the type is recreated; once the replacement type
  is `Active`, restart each workspace using [Start workspace](operations.md#step-3-start-workspace).

## Quickstart

### Step 1: Create workspace

Select an `Active` `InstanceType` with free capacity, save this manifest as `instance.yaml`, and
apply it in a non-reserved workload namespace:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance  # namespaced, short name inst
metadata:
  name: workspace-demo
  namespace: team-a
spec:
  type: gpustack--nvidia-a10g-linux-amd64  # required; Active InstanceType name
  image: ubuntu:24.04  # required
  command:
    - sleep
    - infinity
  resources:
    accelerator: "1"  # optional, default: "1" on acceleratable types
  ports:
    - name: http
      port: 8080  # allocates one NodePort on the Instance Service
  volume:  # required; permanently immutable
    ephemeral:
      capacity: 1Gi
```

```bash
kubectl apply -f instance.yaml
```

### Step 2: Verify workspace

Wait for the `Instance` phase to reach `Ready` and inspect its assigned access address and ports:

```bash
kubectl -n team-a wait --for=jsonpath='{.status.phase}'=Ready instance/workspace-demo --timeout=5m
kubectl -n team-a get instance workspace-demo
```

**Verification:** `PHASE` reports `Ready`, `ACCESS` shows the host address, and `PORT(S)` lists
`8080:<nodePort>/TCP`. Configure SSH login, persistent storage, or node pinning in
[GPU Instance Configuration](configuration.md).

## Troubleshooting

If an `Instance` stays in `Starting`, read its phase message:

```bash
kubectl -n team-a get instance workspace-demo -o jsonpath='{.status.phaseMessage}{"\n"}'
```

See [GPU Instance Operations](operations.md#troubleshooting) for admission refusals, volume waits,
and subresource checks, or [Heterogeneous Devices Walkthrough](../../walkthroughs/devices/scheduling.md#troubleshooting)
for Kueue `Workload` diagnosis.

---

**See also** — [GPU Instance Configuration](configuration.md) (SSH keys, volumes, node pinning, and custom types) · [GPU Instance Operations](operations.md) (stop/start lifecycle, isolation, logs, events, and metrics) · [Heterogeneous Devices](../devices/_index.md) (accelerator pools and slicing)

**Next** → [GPU Instance Configuration](configuration.md) — configure resources, SSH access, and workspace volumes.
