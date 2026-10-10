# Topology Management

GPUStack can describe domains such as zones and racks, then let Kueue account for capacity within
those domains.

## Contents

- [Placement and scheduling](#placement-and-scheduling)
- [Topology sources](#topology-sources)

## Placement and scheduling

[Topology-Aware Scheduling](scheduling.md) explains topology
profiles, Kueue Topologies and how a replica asks to stay in one domain.

Follow [Hierarchical Placement Walkthrough](../../walkthroughs/topology/hierarchical-placement.md) to
place serving replicas across zones and physical racks.

## Topology sources

[Topology-Aware Scheduling Operations](operations.md) covers inventory
sources, verification and diagnosis of a Pending group.

---

**See also** — [RDMA Networking](../rdma/_index.md) (network endpoints) ·
[Model Deployment](../model-deployment/_index.md) (multi-Pod inference)

**Next** → [Topology-Aware Scheduling Operations](operations.md) —
publish and verify a topology.
