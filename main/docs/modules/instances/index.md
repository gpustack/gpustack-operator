# Accelerated Instances

An `Instance` is a container workspace backed by a Kubernetes Pod. It can use a whole, shared,
sliced or partitioned accelerator. With an SSH public key, the operator adds a sidecar that lets
the user enter the workload container.

## Contents

- [Instance configuration](#instance-configuration)
- [Device access](#device-access)
- [Service lifecycle](#service-lifecycle)

## Instance configuration

The [Usage](https://github.com/gpustack/gpustack-operator/blob/79c912a40ff94b99a0040f785dd427f087738b1a/README.md#usage) example creates a sliced Instance. [Accelerator
Requests](../devices/requests.md#requesting-through-the-instance-api) explains how
`spec.resources` maps to the accelerator resource families.

## Device access

The workload container owns the accelerator request. The optional SSH sidecar uses a separate
visibility resource to enter the same workspace without acquiring a second accelerator. See
[Device Discovery](/gpustack-operator/main/docs/modules/devices/discovery/index.md#ssh-enabled-instances-and-the-visibility-resource).
[Instance Metrics](/gpustack-operator/main/docs/reference/instance-metrics/index.md) explains the reported utilization.

## Service lifecycle

An Instance listing `spec.ports` is reached through a NodePort Service named after it, and
`status.ports` reports the node port allocated for each of them.

Stopping the Instance (`spec.stop: true`) deletes the Pod but retains the Service, so the node ports
a user already connected to stay allocated. Starting it again (`spec.stop: false`) builds a new Pod
and reports the same node ports. The Service belongs to the Instance rather than to the Pod, and
selects the Pod by the part-of label the two share, so it keeps routing across both the Pod
replacement and a change of the Instance's type.

`spec.ports` may be edited while the Instance is stopped, and the changes take effect on the Service
when the Instance starts again: ports left unchanged keep their allocated node port, a port added to
`spec.ports` is allocated a new one, a port removed from `spec.ports` is dropped from the Service,
and clearing `spec.ports` deletes the Service along with its allocations. Deleting the Instance
itself also deletes the Service.

---

**See also** — [Heterogeneous Devices](/gpustack-operator/main/docs/modules/devices/index.md) (resource families) ·
[Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) (mounting model weights)

**Next** → [Usage](https://github.com/gpustack/gpustack-operator/blob/79c912a40ff94b99a0040f785dd427f087738b1a/README.md#usage) — launch a sliced Instance.
