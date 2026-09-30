# Model Delivery

`ModelArtifact` gives a model version a stable identity. Its delivery can be handled by the engine,
a node cache, a PVC or an image.

## Contents

- [Sources and delivery](#sources-and-delivery)
- [Cache operations](#cache-operations)
- [Observing and syncing](#observing-and-syncing)

## Sources and delivery

[Model Artifact](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md) covers resolution, validation and the delivery
choices. [Model Image Source](/gpustack-operator/main/docs/modules/model-delivery/image-source/index.md) covers image-backed weights.

## Cache operations

[Model Store Operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md) covers configuration, watermarks and
removal. [Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md) covers warming weights before a Pod needs
them. [Node Model Store](/gpustack-operator/main/docs/modules/model-delivery/node-store/index.md) describes the per-node record and plugin.

## Observing and syncing

The [Model Artifact API](/gpustack-operator/main/docs/modules/model-delivery/views/index.md) covers reading artifact progress and node
caches from the API. [Node-to-Node Sync](/gpustack-operator/main/docs/modules/model-delivery/peer-sync/index.md) covers how a node pulls
weights from another node's cache.

---

**See also** — [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) (serving a model) ·
[Accelerated Instances](/gpustack-operator/main/docs/modules/instances/index.md) (using weights in an Instance)

**Next** → [Model Artifact](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md) — choose a source and delivery.
