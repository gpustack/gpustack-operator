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

![Topology discovery and placement flow](../../assets/diagrams/topology-flow.svg)

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

Choose an inventory path in [Topology Operations](operations.md#choose-the-inventory-path), then
configure [Node labels](operations.md#observe-existing-node-labels), a
[ConfigMap snapshot](operations.md#publish-a-configmap-snapshot), or a
[webhook](operations.md#publish-by-webhook). [Topology-Aware Scheduling](scheduling.md) defines the placement contract.

## Operations

1. **Step 1: Choose a source.** Select the inventory path that matches your cluster.
2. **Step 2: Configure the source.** Apply the `TopologySource` manifest and any backing snapshot or webhook.
3. **Step 3: Verify the objects.** Check the source, queue, assignment and `Pod` nodes, then [request a level](operations.md#request-a-level).

Use [Verification](operations.md#verification) for commands and expected status. Follow the
[hierarchical placement walkthrough](../../walkthroughs/topology/hierarchical-placement.md) for an example.

## Troubleshooting

For a Pending group, follow [diagnosis](operations.md#diagnose-pending-workloads). Compare the
requested domain with the queue, topology and available capacity.

---

**See also** — [RDMA Networking](../rdma/_index.md) (network endpoints) ·
[Model Deployment](../model-deployment/_index.md) (multi-`Pod` inference)

**Next** → [Topology-Aware Scheduling Operations](operations.md) — publish and verify a topology.
