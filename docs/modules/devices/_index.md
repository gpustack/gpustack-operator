# Heterogeneous Devices

GPUStack makes GPUs, NPUs, MLUs, DCUs and PPUs available to workloads. The manufacturer
controls the resource name and runtime isolation method.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

Device Manager detects hardware and publishes capacity. Kueue admits requests against the resulting
queues. The device plugin allocates devices to containers.

![Device discovery and allocation flow](../../assets/diagrams/devices-flow.svg)

<!--
flowchart TD
    HARDWARE["Accelerators and node hardware"] -> NFD["NFD NodeFeatureRule"]
    HARDWARE -> DM["Device Manager inventory and ledger"]
    NFD -> PROFILER["Worker capacity profiler"]
    DM -> PROFILER
    PROFILER -> QUEUES["InstanceType, ResourceFlavor and isolated queues"]
    QUEUES -> GATES["Webhook, quota, per-device fit, scheduler and plugin"]
    GATES -> CONTAINER["Device access and supported slicing or MIG"]
-->

## Configuration

### Accelerator requests

Choose a resource family in [Accelerator Requests](requests.md#resource-keys). The guide includes
request examples and validation rules. [Device Discovery](discovery.md) explains the inventory and
allocation boundary.

## Operations

### Device operations

1. **Step 1: Check the node.** Run [Preflight Checks](preflight.md) before installation.
2. **Step 2: Request devices.** Follow the [device walkthrough](../../walkthroughs/devices/scheduling.md).
3. **Step 3: Verify allocation.** Check the `Workload`, `Pod` and device inventory in the walkthrough.

For physical partitions, use the [NVIDIA](nvidia-mig.md), [T-Head](thead-mig.md), or
[Hygon](hygon-mig.md) runbook. The [scheduling chain](scheduling.md) describes queue status.

## Troubleshooting

For a rejected request, check the [admission gates](admission.md#gates) and request rules.
For node checks and their results, see [Preflight Checks](preflight.md#reading-the-result).
For partition recovery, use the runbook for the manufacturer.

---

**See also** — [GPU Instances](../instances/_index.md) (accelerator workspaces) ·
[Architecture](../../getting-started/architecture.md) (the scheduling chain)

**Next** → [Accelerator Requests](requests.md) — choose a request shape.
