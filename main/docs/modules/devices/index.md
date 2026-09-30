# Heterogeneous Devices

GPUStack uses one request model for GPUs, NPUs, MLUs, DCUs and PPUs. The resource name and the
runtime isolation method depend on the manufacturer.

## Contents

- [Accelerator requests](#accelerator-requests)
- [Device operations](#device-operations)

## Accelerator requests

[Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) explains whole, shared, logically sliced and
physically partitioned requests. [Device Discovery](/gpustack-operator/main/docs/modules/devices/discovery/index.md) explains
how the per-node `Devices` record connects discovery to allocation.

## Device operations

Check a node before installation with [Preflight Operations](/gpustack-operator/main/docs/modules/devices/preflight/index.md). If you
use hardware partitioning, follow the runbook for [NVIDIA](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md),
[T-Head](/gpustack-operator/main/docs/modules/devices/thead-mig/index.md) or [Hygon](/gpustack-operator/main/docs/modules/devices/hygon-mig/index.md).

---

**See also** — [Accelerated Instances](/gpustack-operator/main/docs/modules/instances/index.md) (a container workspace using an
accelerator) · [Architecture](/gpustack-operator/main/docs/getting-started/architecture/index.md) (the scheduling chain)

**Next** → [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) — choose a request shape.
