# Accelerated Instances

> **Purpose** — use an accelerator from a container workspace that can offer SSH access.
> **Audience** users, operators · **Prerequisites** [Architecture](../../getting-started/architecture.md) · **Read time** ~2 min

An `Instance` is a container workspace backed by a Kubernetes Pod. It can use a whole, shared,
sliced or partitioned accelerator. With an SSH public key, the operator adds a sidecar that lets
the user enter the workload container.

## Contents

- [Create an Instance](#create-an-instance)
- [Understand device access](#understand-device-access)

## Create an Instance

The [Usage](../../../README.md#usage) example creates a sliced Instance. [Accelerator
Requests](../devices/requests.md#requesting-through-the-instance-api) explains how
`spec.resources` maps to the accelerator resource families.

## Understand device access

The workload container owns the accelerator request. The optional SSH sidecar uses a separate
visibility resource to enter the same workspace without acquiring a second accelerator. See
[Device Discovery](../devices/discovery.md#ssh-enabled-instances-and-the-visibility-resource).
[Instance Metrics](../../reference/instance-metrics.md) explains the reported utilization.

---

**See also** — [Heterogeneous Devices](../devices/_index.md) (resource families) ·
[Model Delivery](../model-delivery/_index.md) (mounting model weights)

**Next** → [Usage](../../../README.md#usage) — launch a sliced Instance.
