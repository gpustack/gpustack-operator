# Model Delivery

`ModelArtifact` gives a model version a stable identity. Its delivery can be handled by the engine,
a node cache, a PVC or an image.

## Contents

- [Sources and delivery](#sources-and-delivery)
- [Cache operations](#cache-operations)

## Sources and delivery

[Model Artifact](artifact.md) covers resolution, validation and the delivery
choices. [Model Image Source](image-source.md) covers image-backed weights.

## Cache operations

[Model Store Operations](operations.md) covers configuration, watermarks and
removal. [Model Prefetch](prefetch.md) covers warming weights before a Pod needs
them. [Node Model Store](node-store.md) describes the per-node record and plugin.

---

**See also** — [Model Deployment](../model-deployment/_index.md) (serving a model) ·
[Accelerated Instances](../instances/_index.md) (using weights in an Instance)

**Next** → [Model Artifact](artifact.md) — choose a source and delivery.
