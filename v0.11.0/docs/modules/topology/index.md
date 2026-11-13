# Topology

GPUStack describes domains such as zones and racks. Kueue accounts for capacity within those domains.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

### Placement and scheduling

A `TopologySource` can select an inventory path and hierarchy. Without a source, GPUStack uses the
default profile. GPUStack publishes profiles and Kueue `Topology` resources for placement.

![Topology discovery and placement flow](/gpustack-operator/v0.11.0/docs/assets/diagrams/topology-flow.svg)

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

Choose an inventory path in [Topology Operations](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md#choose-the-inventory-path), then
configure [Node labels](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md#observe-existing-node-labels), a
[ConfigMap snapshot](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md#publish-a-configmap-snapshot), or a
[webhook](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md#publish-by-webhook). [Topology-Aware Scheduling](/gpustack-operator/v0.11.0/docs/modules/topology/scheduling/index.md) defines the placement contract.

## Operations

1. **Step 1: Choose a source.** Select the inventory path that matches your cluster.
2. **Step 2: Configure the source.** Apply the `TopologySource` manifest and any backing snapshot or webhook.
3. **Step 3: Verify the objects.** Check the source, queue, assignment and `Pod` nodes, then [request a level](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md#request-a-level).

Use [Verification](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md#verification) for commands and expected status. Follow the
[hierarchical placement walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/topology/hierarchical-placement/index.md) for an example.

## Troubleshooting

For a Pending group, follow [diagnosis](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md#diagnose-pending-workloads). Compare the
requested domain with the queue, topology and available capacity.

---

**See also** — [RDMA Networking](/gpustack-operator/v0.11.0/docs/modules/rdma/index.md) (network endpoints) ·
[Model Deployment](/gpustack-operator/v0.11.0/docs/modules/model-deployment/index.md) (multi-`Pod` inference)

**Next** → [Topology-Aware Scheduling Operations](/gpustack-operator/v0.11.0/docs/modules/topology/operations/index.md) — publish and verify a topology.
