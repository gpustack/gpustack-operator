# Topology

GPUStack models failure and fabric domains (regions, zones, racks, and network switches) so Kueue
*topology-aware scheduling (TAS)* places multi-`Pod` replica groups inside a single domain.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

### Placement and scheduling

A `TopologySource` selects a node inventory source and an ordered label hierarchy (without one,
managed queues use a hostname-only default profile). The worker publishes Kueue `Topology` resources
and binds them to `ResourceFlavor`s and `ClusterQueue`s.

![Topology discovery and placement flow](/gpustack-operator/v0.12.0/docs/assets/diagrams/topology-flow.svg)

<!--
flowchart TD
    LABELS["Existing Node labels"] -> PROFILE["Topology profile builder"]
    CONFIG["ConfigMap snapshot via NFD"] -> PROFILE
    WEBHOOK["HTTPS inventory via NFD"] -> PROFILE
    PROFILE -> FLAVOR["Kueue Topology and ResourceFlavor"]
    FLAVOR -> CQ["TAS-enabled ClusterQueue"]
    REQUEST["Role replica topology request"] -> CQ
    CQ -> PODS["Domain placement for each replica PodSet"]
-->

## Configuration

### Topology sources

Choose one inventory source in [Topology inventory sources](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#topology-inventory-sources), then configure
[Topograph](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#topograph-setup), [existing Node labels](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#existing-node-labels),
a [ConfigMap snapshot](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#configmap-snapshot), or a
[webhook snapshot](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#webhook-snapshot).

## Operations

### Step 1: Select source

Pick the single inventory source that owns hierarchy labels for your `Node`s.

### Step 2: Apply source

Create the `TopologySource` (and any backing `ConfigMap` or HTTPS credentials).

### Step 3: Verify placement

Confirm profile labels, Kueue `Topology` objects, and `ClusterQueue` readiness in
[Verification](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#verification), then constrain serving roles in
[Topology level requests](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#topology-level-requests) or follow the
[Hierarchical Placement Walkthrough](/gpustack-operator/v0.12.0/docs/walkthroughs/topology/hierarchical-placement/index.md).

## Troubleshooting

When replicas remain `Pending`, follow
[Diagnose Pending workloads](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md#diagnose-pending-workloads) to compare requested topology
levels, per-domain capacity, and profile alignment.

---

**See also** — [RDMA Networking](/gpustack-operator/v0.12.0/docs/modules/rdma/index.md) (NIC endpoints) · [Model Deployment](/gpustack-operator/v0.12.0/docs/modules/model-deployment/index.md) (multi-`Pod` serving roles)

**Next** → [Topology-Aware Scheduling Operations](/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md) — publish domain hierarchies and diagnose TAS placement.
