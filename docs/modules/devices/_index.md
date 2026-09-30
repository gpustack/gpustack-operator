# Heterogeneous Devices

GPUStack uses one request model for GPUs, NPUs, MLUs, DCUs and PPUs. The resource name and the
runtime isolation method depend on the manufacturer.

## Contents

- [Start with a request](#start-with-a-request)
- [Operate the hardware](#operate-the-hardware)

## Start with a request

[Accelerator Requests](requests.md) explains whole, shared, logically sliced and
physically partitioned requests. [Device Discovery](discovery.md) explains
how the per-node `Devices` record connects discovery to allocation.

## Operate the hardware

Check a node before installation with [Preflight Operations](preflight.md). If you
use hardware partitioning, follow the runbook for [NVIDIA](nvidia-mig.md),
[T-Head](thead-mig.md) or [Hygon](hygon-mig.md).

---

**See also** — [Accelerated Instances](../instances/_index.md) (a container workspace using an
accelerator) · [Architecture](../../getting-started/architecture.md) (the scheduling chain)

**Next** → [Accelerator Requests](requests.md) — choose a request shape.
