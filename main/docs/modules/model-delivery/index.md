# Model Delivery

`ModelArtifact` gives model weights a stable, content-addressed or volume-backed identity. Workloads
consume weights via engine download, verified node cache (`NodeModelStore`), `PersistentVolumeClaim`,
OCI image volume, or remote host-memory transfer without repeating URIs or tokens.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

![Model delivery and node cache flow](/gpustack-operator/main/docs/assets/diagrams/model-delivery-flow.svg)

<!--
flowchart TD
    SOURCE["Hub, image or PVC source"] -> ARTIFACT["Resolved ModelArtifact"]
    ARTIFACT -> PREFETCH["Optional prefetch and peer sync"]
    PREFETCH -> STORE["NodeModelStore and CSI"]
    STORE -> NODE["Read-only node delivery"]
    STORE -> REMOTE["Prepared source for remote host transfer"]
    ARTIFACT -> OTHER["Engine, PVC or image delivery"]
-->

[Model Artifact](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md) defines weight sources, resolution, and workload mounts.
[Remote Weight Delivery](/gpustack-operator/main/docs/modules/model-delivery/remote/index.md) covers streaming tensors from prepared source `Node`s.

## Configuration

Start with the [artifact manifest](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md#configuration) to declare hub, PVC, or
[OCI image](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md#image-source) weights. Configure [Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md) for namespace
cache budgets, warm-up placement, and [node-to-node peer sync](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md#node-to-node-sync).

For an end-to-end cache run, follow the
[Model Prefetch Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md).

## Operations

### Step 1: Configure cache

Set deploy-time and runtime cache policies in [Model Store Operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md#configuration),
and inspect per-node state in [Node cache inspection](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md#node-cache-inspection).

### Step 2: Inspect delivery

Check artifact resolution and node cache status:

```bash
kubectl -n <namespace> get modelartifact <artifact-name> -o yaml
kubectl get nodemodelstores
```

**Verification:** Confirm `Resolved=True` on the `ModelArtifact` ([Status](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md#status)) and
`Ready=True` on target `NodeModelStore` objects. See
[Progress subresource](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md#progress-subresource) for live download progress and RBAC.

## Troubleshooting

If weights do not reach a `ModelDeployment`, inspect the artifact, deployment conditions, and `Pod`
events:

```bash
kubectl -n <namespace> describe modelartifact <artifact-name>
kubectl -n <namespace> get modeldeployment <deployment-name> -o yaml
kubectl -n <namespace> describe pod <pod-name>
```

| Condition | Reason | Resolution |
|---|---|---|
| `WeightsReady=False` | `NodeDeliveryUnavailable` | Enable `modelManager.enabled` and verify the `model.csi.gpustack.ai` `CSIDriver`. |
| `WeightsReady=False` | `FilterNeedsNodeDelivery` | Switch `model-artifact-delivery-mode` to `Node` or remove file patterns. |

Use [Model Store Operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md#troubleshooting) for CSI mount diagnosis and
[Remote Weight Delivery](/gpustack-operator/main/docs/modules/model-delivery/remote/index.md#troubleshooting) for remote transfer failures.

---

**See also** — [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) (serving workloads) · [GPU Instances](/gpustack-operator/main/docs/modules/instances/index.md) (mounting weights in workspaces)

**Next** → [Model Artifact](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md) — configure weight sources, digests, and workload delivery.
