# RDMA Networking

The Device Manager publishes usable RDMA endpoints as Kubernetes resources. Workloads request a
count; kubelet decides which endpoints a container receives.

## Contents

- [RDMA requests](#rdma-requests)
- [Interface discovery](#interface-discovery)

## RDMA requests

[RDMA Operations](operations.md) covers the resource keys, endpoint counts and kubelet
TopologyManager policy. It also explains why Kueue admission alone does not reserve an interface.

Follow [RDMA Network Endpoints Walkthrough](../../walkthroughs/rdma/network-endpoints.md) to
request and verify endpoints and NUMA alignment.

## Interface discovery

[Network Topology](network-topology.md) describes the interface inventory, link
checks and labels the Device Manager reports.

---

**See also** — [Topology Management](../topology/_index.md) (placing a workload within a domain) ·
[Heterogeneous Devices](../devices/_index.md) (accelerator allocation)

**Next** → [RDMA Operations](operations.md) — request and verify an endpoint.
