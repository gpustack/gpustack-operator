# Heterogeneous Devices

GPUStack makes GPUs, NPUs, MLUs, DCUs and PPUs available to workloads. The manufacturer
controls the resource name and runtime isolation method.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

Device Manager detects hardware and publishes capacity. `AcceleratorType` selects accelerated pools
for serving and prefetch; `InstanceType` sizes interactive workspaces. Both share the canonical pool
`ClusterQueue`. Kueue admits queued requests, and the device plugin allocates devices to containers.

![Device discovery and allocation flow](/gpustack-operator/v0.11.0/docs/assets/diagrams/devices-flow.svg)

<!--
flowchart TD
    HARDWARE["Accelerators and node hardware"] -> NFD["NFD NodeFeatureRule"]
    HARDWARE -> DM["Device Manager inventory and ledger"]
    NFD -> PROFILER["Worker capacity profiler"]
    DM -> PROFILER
    PROFILER -> QUEUES["AcceleratorType, InstanceType, ResourceFlavor and pool queues"]
    QUEUES -> GATES["Webhook, quota, per-device fit, scheduler and plugin"]
    GATES -> CONTAINER["Device access and supported slicing or MIG"]
-->

## Configuration

### Accelerator requests

Choose a resource family in [Accelerator Requests](/gpustack-operator/v0.11.0/docs/modules/devices/requests/index.md#resource-keys). The guide includes
request examples and validation rules. [Device Discovery](/gpustack-operator/v0.11.0/docs/modules/devices/discovery/index.md) explains the inventory and
allocation boundary.

## Operations

### Device operations

1. **Step 1: Check the node.** Run [Preflight Checks](/gpustack-operator/v0.11.0/docs/modules/devices/preflight/index.md) before installation.
2. **Step 2: Request devices.** Follow the [device walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/devices/scheduling/index.md).
3. **Step 3: Verify allocation.** Check the `Workload`, `Pod` and device inventory in the walkthrough.

For physical partitions, use the [NVIDIA](/gpustack-operator/v0.11.0/docs/modules/devices/nvidia-mig/index.md), [T-Head](/gpustack-operator/v0.11.0/docs/modules/devices/thead-mig/index.md), or
[Hygon](/gpustack-operator/v0.11.0/docs/modules/devices/hygon-mig/index.md) runbook. The [scheduling chain](/gpustack-operator/v0.11.0/docs/modules/devices/scheduling/index.md) describes queue status.

## Troubleshooting

For a rejected request, check the [admission gates](/gpustack-operator/v0.11.0/docs/modules/devices/admission/index.md#gates) and request rules.
For node checks and their results, see [Preflight Checks](/gpustack-operator/v0.11.0/docs/modules/devices/preflight/index.md#reading-the-result).
For partition recovery, use the runbook for the manufacturer.

---

**See also** — [GPU Instances](/gpustack-operator/v0.11.0/docs/modules/instances/index.md) (accelerator workspaces) ·
[Architecture](/gpustack-operator/v0.11.0/docs/getting-started/architecture/index.md) (the scheduling chain)

**Next** → [Accelerator Requests](/gpustack-operator/v0.11.0/docs/modules/devices/requests/index.md) — choose a request shape.
