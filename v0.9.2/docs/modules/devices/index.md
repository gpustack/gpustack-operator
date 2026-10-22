# Heterogeneous Devices

GPUStack uses one request model for GPUs, NPUs, MLUs, DCUs and PPUs. The resource name and the
runtime isolation method depend on the manufacturer.

## Contents

- [Accelerator requests](#accelerator-requests)
- [Device operations](#device-operations)

## Accelerator requests

[Accelerator Requests](/gpustack-operator/v0.9.2/docs/modules/devices/requests/index.md) explains whole, shared, logically sliced and
physically partitioned requests. [Device Discovery](/gpustack-operator/v0.9.2/docs/modules/devices/discovery/index.md) explains
how the per-node `Devices` record connects discovery to allocation.

Follow [Heterogeneous Devices Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/devices/scheduling/index.md) for
end-to-end device allocation and Kueue queuing.

## Device operations

Check a node before installation with [Preflight Checks](/gpustack-operator/v0.9.2/docs/modules/devices/preflight/index.md). If you
use hardware partitioning, follow the runbook for [NVIDIA](/gpustack-operator/v0.9.2/docs/modules/devices/nvidia-mig/index.md),
[T-Head](/gpustack-operator/v0.9.2/docs/modules/devices/thead-mig/index.md) or [Hygon](/gpustack-operator/v0.9.2/docs/modules/devices/hygon-mig/index.md).

---

**See also** — [GPU Instances](/gpustack-operator/v0.9.2/docs/modules/instances/index.md) (a container workspace using an
accelerator) · [Architecture](/gpustack-operator/v0.9.2/docs/getting-started/architecture/index.md) (the scheduling chain)

**Next** → [Accelerator Requests](/gpustack-operator/v0.9.2/docs/modules/devices/requests/index.md) — choose a request shape.
