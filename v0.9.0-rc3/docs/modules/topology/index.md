# Topology Management

GPUStack can describe domains such as zones and racks, then let Kueue account for capacity within
those domains.

## Contents

- [Placement and scheduling](#placement-and-scheduling)
- [Topology sources](#topology-sources)

## Placement and scheduling

[Topology-Aware Scheduling](/gpustack-operator/v0.9.0-rc3/docs/modules/topology/scheduling/index.md) explains topology
profiles, Kueue Topologies and how a replica asks to stay in one domain.

## Topology sources

[Topology-Aware Scheduling Operations](/gpustack-operator/v0.9.0-rc3/docs/modules/topology/operations/index.md) covers inventory
sources, verification and diagnosis of a Pending group.

---

**See also** — [RDMA Networking](/gpustack-operator/v0.9.0-rc3/docs/modules/rdma/index.md) (network endpoints) ·
[Model Deployment](/gpustack-operator/v0.9.0-rc3/docs/modules/model-deployment/index.md) (multi-Pod inference)

**Next** → [Topology-Aware Scheduling Operations](/gpustack-operator/v0.9.0-rc3/docs/modules/topology/operations/index.md) —
publish and verify a topology.
