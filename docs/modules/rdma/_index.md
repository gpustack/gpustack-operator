# RDMA Networking

Device Manager discovers healthy *remote direct memory access (RDMA)* endpoints and advertises them
as Kubernetes extended resources. Kubelet allocates endpoints to containers that request them.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

### Interface discovery

The node inventory records link health in `Devices`, the device plugin advertises eligible RDMA
keys, and kubelet grants endpoints to containers. Kueue queue admission does not meter RDMA keys.

![RDMA discovery and allocation flow](../../assets/diagrams/rdma-flow.svg)

<!--
flowchart TD
    NIC["RDMA NIC ports, GIDs, state and locality"] -> INVENTORY["Devices RDMA inventory"]
    INVENTORY -> POLICY["Optional interface selection policy"]
    POLICY -> PLUGIN["RDMA device plugin and allocatable"]
    PLUGIN -> KUBELET["Kubelet topology policy and admission"]
    KUBELET -> CONTAINER["Granted RDMA devices and HCA names"]
-->

## Configuration

### RDMA requests

Select an RDMA key and endpoint count in [Key selection](operations.md#key-selection), configure an
optional `DevicesRDMASelectionPolicy` in [Selection policy](operations.md#selection-policy), and
inspect link states in [NIC record inspection](operations.md#nic-record-inspection).

## Operations

### Step 1: Inspect capacity

Check allocatable `device.gpustack.ai/rdma*` keys across `Node`s.

### Step 2: Set alignment

Configure the kubelet `TopologyManager` policy when containers require *non-uniform memory access
(NUMA)* alignment ([Enabling NUMA alignment](operations.md#enabling-numa-alignment)).

### Step 3: Verify grant

Follow the [RDMA Networking Walkthrough](../../walkthroughs/rdma/network-endpoints.md) and confirm
the active kubelet policy in [Verification](operations.md#verification).

## Troubleshooting

Use the [RDMA troubleshooting table](operations.md#troubleshooting) to resolve unhealthy links,
missing `rdma.capable` labels, `TopologyAffinityError` rejections, or `Pod`s stuck `Pending` behind
exhausted endpoints.

---

**See also** — [Topology](../topology/_index.md) (domain placement) · [Heterogeneous Devices](../devices/_index.md) (accelerator allocation)

**Next** → [RDMA Operations](operations.md) — select RDMA keys, filter adapters, and align NUMA placement.
