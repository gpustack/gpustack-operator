# Model Delivery

`ModelArtifact` gives model weights a stable identity. Delivery uses an engine, node cache,
`PersistentVolumeClaim` or image. A workload can reference the artifact instead of repeating its source.

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

[Model Artifact](artifact.md) defines the sources and delivery choices.
[Remote Weight Delivery](remote.md) describes loading from a prepared source node.

## Configuration

Use the annotated [artifact example](artifact.md#resource) to select a source and filters.
[Model Image Source](image-source.md) defines image-backed weights.
[Model Prefetch](prefetch.md) defines cache grants and warm-up requests.

For a complete example, follow the
[Model Prefetch Walkthrough](../../walkthroughs/model-delivery/prefetch.md).

## Operations

### Step 1: Configure cache

Follow [Model Store Operations](operations.md#configuration) to configure node delivery.
[Node Model Store](node-store.md) defines node cache state and the plugin.

### Step 2: Inspect delivery

Inspect the artifact and node stores:

```bash
kubectl -n <namespace> get modelartifact <artifact-name> -o yaml
kubectl get nodemodelstores
```

**Verification:** Check the artifact's `Resolved` condition and the node cache status.
See [Model Artifact Status](artifact.md#status) and [Node Model Store](node-store.md#resource).
The [Model Artifact API](artifact-api.md) describes progress views.
[Node-to-Node Sync](node-to-node-sync.md) describes peer status and metrics.

## Troubleshooting

If weights do not reach a deployment, inspect the artifact and deployment conditions:

```bash
kubectl -n <namespace> describe modelartifact <artifact-name>
kubectl -n <namespace> get modeldeployment <deployment-name> -o yaml
```

If a serving `Pod` exists, inspect its events with `kubectl -n <namespace> describe pod <pod-name>`.

| Condition | Reason | Resolution |
|---|---|---|
| `WeightsReady=False` | `NodeDeliveryUnavailable` | Check `modelManager.enabled` and the `model.csi.gpustack.ai` `CSIDriver`. |
| `WeightsReady=False` | `FilterNeedsNodeDelivery` | Check the delivery setting and artifact patterns. |

Use [Model Store Troubleshooting](operations.md#troubleshooting) for more symptoms and recovery checks.
For remote loading failures, see [Remote Weight Delivery](remote.md#troubleshooting).

---

**See also** — [Model Deployment](../model-deployment/_index.md) (serving a model) ·
[GPU Instances](../instances/_index.md) (using weights in an `Instance`)

**Next** → [Model Artifact](artifact.md) — choose a source and delivery.
