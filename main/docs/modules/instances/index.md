# Accelerated Instances

An `Instance` is a container workspace backed by a Kubernetes Pod. It can use a whole, shared,
sliced or partitioned accelerator. With an SSH public key, the operator adds a sidecar that lets
the user enter the workload container.

## Contents

- [Instance configuration](#instance-configuration)
- [Device access](#device-access)

## Instance configuration

The [Usage](https://github.com/gpustack/gpustack-operator/blob/e32f9f0480b320e8d7653c458aa6166cb4602c2d/README.md#usage) example creates a sliced Instance. [Accelerator
Requests](../devices/requests.md#requesting-through-the-instance-api) explains how
`spec.resources` maps to the accelerator resource families.

## Device access

The workload container owns the accelerator request. The optional SSH sidecar uses a separate
visibility resource to enter the same workspace without acquiring a second accelerator. See
[Device Discovery](/gpustack-operator/main/docs/modules/devices/discovery/index.md#ssh-enabled-instances-and-the-visibility-resource).
[Instance Metrics](/gpustack-operator/main/docs/reference/instance-metrics/index.md) explains the reported utilization.

---

**See also** — [Heterogeneous Devices](/gpustack-operator/main/docs/modules/devices/index.md) (resource families) ·
[Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) (mounting model weights)

**Next** → [Usage](https://github.com/gpustack/gpustack-operator/blob/e32f9f0480b320e8d7653c458aa6166cb4602c2d/README.md#usage) — launch a sliced Instance.
