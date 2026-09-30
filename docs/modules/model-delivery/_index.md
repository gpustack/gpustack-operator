# Model Delivery

> **Purpose** — choose how model weights reach a workload and when to keep them on nodes.
> **Audience** users, operators · **Prerequisites** [Architecture](../../getting-started/architecture.md) · **Read time** ~2 min

`ModelArtifact` gives a model version a stable identity. Its delivery can be handled by the engine,
a node cache, a PVC or an image.

## Contents

- [Choose a source and delivery](#choose-a-source-and-delivery)
- [Run the node cache](#run-the-node-cache)

## Choose a source and delivery

[Model Artifact](artifact.md) covers resolution, validation and the delivery
choices. [Model Image Source](image-source.md) covers image-backed weights.

## Run the node cache

[Model Store Operations](operations.md) covers configuration, watermarks and
removal. [Model Prefetch](prefetch.md) covers warming weights before a Pod needs
them. [Node Model Store](node-store.md) describes the per-node record and plugin.

---

**See also** — [Model Deployment](../model-deployment/_index.md) (serving a model) ·
[Accelerated Instances](../instances/_index.md) (using weights in an Instance)

**Next** → [Model Artifact](artifact.md) — choose a source and delivery.
