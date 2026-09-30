# RDMA Networking

The Device Manager publishes usable RDMA endpoints as Kubernetes resources. Workloads request a
count; kubelet decides which endpoints a container receives.

## Contents

- [RDMA requests](#rdma-requests)
- [Interface discovery](#interface-discovery)

## RDMA requests

[RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) covers the resource keys, endpoint counts and kubelet
TopologyManager policy. It also explains why Kueue admission alone does not reserve an interface.

## Interface discovery

[Network Topology](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md) describes the interface inventory, link
checks and labels the Device Manager reports.

---

**See also** — [Topology Management](/gpustack-operator/main/docs/modules/topology/index.md) (placing a workload within a domain) ·
[Heterogeneous Devices](/gpustack-operator/main/docs/modules/devices/index.md) (accelerator allocation)

**Next** → [RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) — request and verify an endpoint.
