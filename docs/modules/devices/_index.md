# Heterogeneous Devices

GPUStack discovers GPUs, NPUs, MLUs, DCUs, and PPUs across nodes and exposes them through Kueue
pool queues. Each manufacturer governs its extended resource name and slicing or partitioning
mechanism.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

Device Manager detects node hardware and updates the `Devices` ledger. The worker builds
`AcceleratorType` pools (for serving and prefetch) and `InstanceType` pools (for interactive
workspaces) over shared canonical `ClusterQueue`s, while Kueue and the device plugin enforce quota
and container isolation.

![Device discovery and allocation flow](../../assets/diagrams/devices-flow.svg)

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

Choose whole-card, shared, logically sliced, or hardware-partitioned resource keys in
[Accelerator Requests](requests.md#resource-keys). Review the seven `Pod` validation rules in
[Request rules](requests.md#request-rules) and pool capabilities in
[Isolation mode selection](requests.md#isolation-mode-selection).

## Operations

### Step 1: Run host preflight

Run [Preflight Checks](preflight.md) on bare hosts before installing workloads.

### Step 2: Request devices

Submit queued workloads following the
[Heterogeneous Devices Walkthrough](../../walkthroughs/devices/scheduling.md).

### Step 3: Verify allocation

Inspect pool capacity and `Workload` reservations in
[Pool troubleshooting](requests.md#pool-troubleshooting). For hardware partitions, follow the
[NVIDIA](nvidia-mig.md), [Hygon](hygon-mig.md), or [T-Head](thead-mig.md) MIG runbooks.

## Troubleshooting

Resolve webhook rejections, queued demand, and per-card fragmentation in
[Diagnosing Pending workloads](requests.md#diagnosing-pending-workloads). Interpret host probe
verdicts in [Result format](preflight.md#result-format), and follow each vendor MIG runbook for
partition recovery.

---

**See also** — [GPU Instances](../instances/_index.md) (interactive accelerator workspaces) · [Architecture](../../getting-started/architecture.md) (end-to-end scheduling chain)

**Next** → [Accelerator Requests](requests.md) — select resource keys and verify admission rules.
