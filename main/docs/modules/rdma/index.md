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

![RDMA discovery and allocation flow](/gpustack-operator/main/docs/assets/diagrams/rdma-flow.svg)

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

Select an RDMA key and endpoint count in [RDMA resource requests](/gpustack-operator/main/docs/modules/rdma/operations/index.md#rdma-resource-requests), configure an
optional `DevicesRDMASelectionPolicy` in [Selection policy](/gpustack-operator/main/docs/modules/rdma/operations/index.md#selection-policy), and
inspect link states in [NIC record inspection](/gpustack-operator/main/docs/modules/rdma/operations/index.md#nic-record-inspection).

## Operations

### Step 1: Inspect capacity

Check allocatable `device.gpustack.ai/rdma*` keys across `Node`s.

### Step 2: Configure NUMA alignment

Configure the kubelet `TopologyManager` policy when containers require *non-uniform memory access
(NUMA)* alignment ([Enabling NUMA alignment](/gpustack-operator/main/docs/modules/rdma/operations/index.md#enabling-numa-alignment)).

### Step 3: Verify RDMA allocation

Follow the [RDMA Networking Walkthrough](/gpustack-operator/main/docs/walkthroughs/rdma/network-endpoints/index.md) and confirm
the active kubelet policy in [Verification](/gpustack-operator/main/docs/modules/rdma/operations/index.md#verification).

## Troubleshooting

Follow [Troubleshooting](/gpustack-operator/main/docs/modules/rdma/operations/index.md#troubleshooting) to resolve unhealthy links,
missing `rdma.capable` labels, `TopologyAffinityError` rejections, or `Pod`s stuck `Pending` behind
exhausted endpoints.

---

**See also** — [Topology](/gpustack-operator/main/docs/modules/topology/index.md) (domain placement) · [Heterogeneous Devices](/gpustack-operator/main/docs/modules/devices/index.md) (accelerator allocation)

**Next** → [RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) — select RDMA keys, filter adapters, and align NUMA placement.
