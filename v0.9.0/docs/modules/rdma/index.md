# RDMA Networking

The Device Manager publishes usable RDMA endpoints as Kubernetes resources. Workloads request a
count; kubelet decides which endpoints a container receives.

## Contents

- [RDMA requests](#rdma-requests)
- [Interface discovery](#interface-discovery)

## RDMA requests

[RDMA Operations](/gpustack-operator/v0.9.0/docs/modules/rdma/operations/index.md) covers the resource keys, endpoint counts and kubelet
TopologyManager policy. It also explains why Kueue admission alone does not reserve an interface.

Follow [RDMA Network Endpoints Walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/rdma/network-endpoints/index.md) to
request and verify endpoints and NUMA alignment.

## Interface discovery

[Network Topology](/gpustack-operator/v0.9.0/docs/modules/rdma/network-topology/index.md) describes the interface inventory, link
checks and labels the Device Manager reports.

---

**See also** — [Topology Management](/gpustack-operator/v0.9.0/docs/modules/topology/index.md) (placing a workload within a domain) ·
[Heterogeneous Devices](/gpustack-operator/v0.9.0/docs/modules/devices/index.md) (accelerator allocation)

**Next** → [RDMA Operations](/gpustack-operator/v0.9.0/docs/modules/rdma/operations/index.md) — request and verify an endpoint.
