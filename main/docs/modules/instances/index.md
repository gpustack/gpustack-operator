# GPU Instances

An `Instance` is a container workspace backed by a Kubernetes `Pod`. It can use a whole, shared,
sliced or partitioned accelerator. With an SSH public key, the operator adds a sidecar that lets
the user enter the workload container.

## Contents

- [Instance configuration](#instance-configuration)
- [Device access](#device-access)
- [Service lifecycle](#service-lifecycle)
- [Network isolation](#network-isolation)

## Instance configuration

The [Usage](https://github.com/gpustack/gpustack-operator/blob/a4b463899a64d9e79c3b3c599f470f79f6701a09/README.md#usage) example creates a sliced `Instance`. [Accelerator
Requests](../devices/requests.md#instance-api-requests) explains how
*`spec.resources`* maps to the accelerator resource families.

## Device access

The workload container owns the accelerator request. The optional SSH sidecar uses a separate
visibility resource to enter the same workspace without acquiring a second accelerator. See
[Device Discovery](/gpustack-operator/main/docs/modules/devices/discovery/index.md#ssh-instances-and-visibility).
[Instance Metrics](/gpustack-operator/main/docs/reference/instance-metrics/index.md) explains the reported utilization.

## Service lifecycle

An `Instance` listing *`spec.ports`* is reached through a NodePort `Service` named after it, and
*`status.ports`* reports the node port allocated for each of them.

Stopping the `Instance` (`spec.stop: true`) deletes the `Pod` but retains the `Service`, so the node ports
a user already connected to stay allocated. Starting it again (`spec.stop: false`) builds a new `Pod`
and reports the same node ports. The `Service` belongs to the `Instance` rather than to the `Pod`, and
selects the `Pod` by the part-of label the two share, so it keeps routing across both the `Pod`
replacement and a change of the `Instance`'s type.

*`spec.ports`* may be edited while the `Instance` is stopped, and the changes take effect on the `Service`
when the `Instance` starts again: ports left unchanged keep their allocated node port, a port added to
*`spec.ports`* is allocated a new one, a port removed from *`spec.ports`* is dropped from the `Service`,
and clearing *`spec.ports`* deletes the `Service` along with its allocations. Deleting the `Instance`
itself also deletes the `Service`.

## Network isolation

The operator isolates `Instance`s of different tenants at the namespace boundary. The behavior is
opt-in: with the `instance-isolation-pod-cidrs` Setting blank — the default — no policy is
rendered and `Instance` networking stays untouched.

When the Setting names the cluster's pod CIDRs, the operator renders one `NetworkPolicy` named
`gpustack-instance-isolation` in every namespace that holds an `Instance`, selecting the
instance `Pod`s by their `resource.gpustack.ai/type: instances` label.

The policy allows traffic to and from `Pod`s of the same namespace, and to and from anywhere
outside the pod CIDRs the Setting names, which is how external clients arrive through the NodePort
`Service`. It blocks traffic to and from `Pod`s of every other namespace in both directions, so a
shell inside one `Instance` cannot scan or reach another tenant's `Instance`s. DNS (port 53) stays
open.

The pod CIDRs cannot be learned reliably for every CNI — a node's pod CIDR and what the CNI's own
IPAM allocates routinely disagree — so the Setting is the one source. Find the values with
`kubectl get ippools.crd.projectcalico.org` on a Calico-IPAM cluster, or
`kubectl get nodes -o jsonpath='{.items[*].spec.podCIDRs}'` where the nodes carve the pod
addresses.

The chart carries the same value as `worker.instanceIsolationPodCIDRs`; the Setting can also
be edited at runtime through the Settings API.

The object belongs to the namespace rather than to one `Instance`: deleting the last `Instance` of
a namespace does not remove it, and the reconcile rewrites any manual edit back to the rendered
shape. Emptying the Setting collects the policy again — but only the object the operator rendered
itself, never a same-named one written by hand.

Enforcing the policy takes a CNI that implements `NetworkPolicy`, such as Calico or Cilium; with a
non-enforcing CNI the object renders but filters nothing. The Kubernetes policy contract always
allows traffic from the node hosting a `Pod`, so a client inside one `Instance` can still reach
another tenant's `Service` through the hosting node's address and node port.

---

**See also** — [Heterogeneous Devices](/gpustack-operator/main/docs/modules/devices/index.md) (resource families) ·
[Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) (mounting model weights)

**Next** → [Usage](https://github.com/gpustack/gpustack-operator/blob/a4b463899a64d9e79c3b3c599f470f79f6701a09/README.md#usage) — launch a sliced `Instance`.
