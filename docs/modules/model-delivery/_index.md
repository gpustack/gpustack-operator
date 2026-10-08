# Model Delivery

`ModelArtifact` gives a model version a stable identity. Its delivery can be handled by the engine,
a node cache, a PVC or an image.

Follow the [Model Prefetch Walkthrough](../../walkthroughs/model-delivery/prefetch.md) for a complete example with captured CR output.

## Contents

- [Sources and delivery](#sources-and-delivery)
- [Cache operations](#cache-operations)
- [Observing and syncing](#observing-and-syncing)

## Sources and delivery

[Model Artifact](artifact.md) covers resolution, validation and the delivery
choices. [Model Image Source](image-source.md) covers image-backed weights.

## Cache operations

[Model Store Operations](operations.md) covers configuration, watermarks and
removal. [Model Prefetch](prefetch.md) covers warming weights before a Pod needs
them. [Node Model Store](node-store.md) describes the per-node record and plugin.

## Observing and syncing

The [Model Artifact API](views.md) covers reading artifact progress and node
caches from the API. [Node-to-Node Sync](peer-sync.md) covers how a node pulls
weights from another node's cache.

---

**See also** — [Model Deployment](../model-deployment/_index.md) (serving a model) ·
[Accelerated Instances](../instances/_index.md) (using weights in an Instance)

**Next** → [Model Artifact](artifact.md) — choose a source and delivery.
