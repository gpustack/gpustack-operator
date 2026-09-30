# RDMA Networking

> **Purpose** — request network interfaces beside accelerators and understand their placement limits.
> **Audience** users, operators · **Prerequisites** [Architecture](../../getting-started/architecture.md) · **Read time** ~2 min

The Device Manager publishes usable RDMA endpoints as Kubernetes resources. Workloads request a
count; kubelet decides which endpoints a container receives.

## Contents

- [Request an interface](#request-an-interface)
- [Inspect the node](#inspect-the-node)

## Request an interface

[RDMA Operations](operations.md) covers the resource keys, endpoint counts and kubelet
TopologyManager policy. It also explains why Kueue admission alone does not reserve an interface.

## Inspect the node

[Network Topology](network-topology.md) describes the interface inventory, link
checks and labels the Device Manager reports.

---

**See also** — [Topology Management](../topology/_index.md) (placing a workload within a domain) ·
[Heterogeneous Devices](../devices/_index.md) (accelerator allocation)

**Next** → [RDMA Operations](operations.md) — request and verify an endpoint.
