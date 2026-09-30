# Topology Management

> **Purpose** — place multi-Pod work where the required topology has enough capacity.
> **Audience** users, operators · **Prerequisites** [Architecture](../../getting-started/architecture.md) · **Read time** ~2 min

GPUStack can describe domains such as zones and racks, then let Kueue account for capacity within
those domains.

## Contents

- [Understand placement](#understand-placement)
- [Configure a source](#configure-a-source)

## Understand placement

[Topology-Aware Scheduling](scheduling.md) explains topology
profiles, Kueue Topologies and how a replica asks to stay in one domain.

## Configure a source

[Topology-Aware Scheduling Operations](operations.md) covers inventory
sources, verification and diagnosis of a Pending group.

---

**See also** — [RDMA Networking](../rdma/_index.md) (network endpoints) ·
[Model Deployment](../model-deployment/_index.md) (multi-Pod inference)

**Next** → [Topology-Aware Scheduling Operations](operations.md) —
publish and verify a topology.
