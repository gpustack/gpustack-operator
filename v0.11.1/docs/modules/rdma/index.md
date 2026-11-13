# RDMA Networking

The Device Manager publishes usable *remote direct memory access (RDMA)* endpoints as Kubernetes
resources. Kubelet allocates endpoints to containers that request them.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

### Interface discovery

The interface inventory records link state. The device plugin advertises eligible endpoints, and
kubelet grants them to containers. Queue admission does not reserve RDMA endpoints.

![RDMA discovery and allocation flow](/gpustack-operator/v0.11.1/docs/assets/diagrams/rdma-flow.svg)

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

Choose a key and endpoint count in [RDMA Operations](/gpustack-operator/v0.11.1/docs/modules/rdma/operations/index.md#choosing-a-key). Configure an
optional [selection policy](/gpustack-operator/v0.11.1/docs/modules/rdma/operations/index.md#selection-policy) to filter adapters. See
[Network Topology](/gpustack-operator/v0.11.1/docs/modules/rdma/network-topology/index.md) for inventory and link states.

## Operations

1. **Step 1: Inspect capacity.** Read the node's allocatable RDMA keys.
2. **Step 2: Set alignment.** Configure the kubelet policy if the workload needs NUMA alignment.
3. **Step 3: Verify the grant.** Follow the [RDMA walkthrough](/gpustack-operator/v0.11.1/docs/walkthroughs/rdma/network-endpoints/index.md).

Read the effective policy in [Verification](/gpustack-operator/v0.11.1/docs/modules/rdma/operations/index.md#verification). Monitor Pending `Pod`s
for endpoint exhaustion in [RDMA Operations](/gpustack-operator/v0.11.1/docs/modules/rdma/operations/index.md#troubleshooting).

## Troubleshooting

For unhealthy endpoints or Pending `Pod`s, use the [RDMA troubleshooting table](/gpustack-operator/v0.11.1/docs/modules/rdma/operations/index.md#troubleshooting).
It links each symptom to a check and recovery action.

---

**See also** — [Topology](/gpustack-operator/v0.11.1/docs/modules/topology/index.md) (domain placement) ·
[Heterogeneous Devices](/gpustack-operator/v0.11.1/docs/modules/devices/index.md) (accelerator allocation)

**Next** → [RDMA Operations](/gpustack-operator/v0.11.1/docs/modules/rdma/operations/index.md) — request and verify an endpoint.
