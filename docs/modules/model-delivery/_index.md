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

![Model delivery and node cache flow](../../assets/diagrams/model-delivery-flow.svg)

<!--
flowchart TD
    SOURCE["Hub, image or PVC source"] -> ARTIFACT["Resolved ModelArtifact"]
    ARTIFACT -> PREFETCH["Optional prefetch and peer sync"]
    PREFETCH -> STORE["NodeModelStore and CSI"]
    STORE -> NODE["Read-only node delivery"]
    STORE -> REMOTE["Prepared source for remote host transfer"]
    ARTIFACT -> OTHER["Engine, PVC or image delivery"]
-->

[Model Artifact](artifact.md) defines weight sources, resolution, and workload mounts.
[Remote Weight Delivery](remote.md) covers streaming tensors from prepared source `Node`s.
`ModelKernelArtifact` reuses `NodeModelStore` (`Source: Forge`, `Kind: Kernel`) and peer sync to
deliver forged single-GPU kernels to [Model Deployment Forger](../model-deployment/forger.md).

## Configuration

Start with the [artifact manifest](artifact.md#configuration) to declare hub, PVC, or
[OCI image](artifact.md#image-source) weights. Configure [Model Prefetch](prefetch.md) for namespace
cache budgets, warm-up placement, and [node-to-node peer sync](prefetch.md#node-to-node-sync).

For an end-to-end cache run, follow the
[Model Prefetch Walkthrough](../../walkthroughs/model-delivery/prefetch.md).

## Operations

### Step 1: Configure cache

Set deploy-time and runtime cache policies in [Model Store Operations](operations.md#configuration),
and inspect per-node state in [Node cache inspection](operations.md#node-cache-inspection).

### Step 2: Inspect delivery

Check artifact resolution and node cache status:

```bash
kubectl -n <namespace> get modelartifact <artifact-name> -o yaml
kubectl get nodemodelstores
```

**Verification:** Confirm `Resolved=True` on the `ModelArtifact` ([Status](artifact.md#status)) and
`Ready=True` on target `NodeModelStore` objects. See
[Progress subresource](artifact.md#progress-subresource) for live download progress and RBAC.

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

Use [Model Store Operations](operations.md#troubleshooting) for CSI mount diagnosis and
[Remote Weight Delivery](remote.md#troubleshooting) for remote transfer failures.

---

**See also** — [Model Deployment](../model-deployment/_index.md) (serving workloads) · [GPU Instances](../instances/_index.md) (mounting weights in workspaces)

**Next** → [Model Artifact](artifact.md) — configure weight sources, digests, and workload delivery.
