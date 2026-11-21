# Model Prefetch Walkthrough

Cache DeepSeek-V2-Lite-Chat on one GPU node before you start inference. The Elastic EP and External
DP walkthroughs start from the namespace and artifact you create here.

## Contents

- [Summary](#summary)
- [Cache preparation](#cache-preparation)
- [Step 3: Pin cached weights](#step-3-pin-cached-weights)
- [Cleanup](#cleanup)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

Prefetching verifies Hugging Face weights on disk and records the result in `NodeModelStore` so
serving `Pod`s mount the warm cache without re-downloading:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | the weight files to cache and their revision |
| `ModelStore` | cluster | administrator | the nodes that hold cached weights |
| `ModelStoreBinding` | namespace | administrator | how much the namespace may cache, and the right to pin |
| `ModelPrefetch` | namespace | user | warming the artifact onto the store's nodes |
| `ModelDeployment` | namespace | user | the serving workload that finds the weights warm |

### Prerequisites

- [Node model delivery](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md) is enabled on the GPU node.
- You have `kubectl`, `jq` and access to cluster-scoped model stores.
- The node has enough free disk for the weights: 31,418,807,311 bytes for this model.

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes with GPUStack, node model delivery enabled |
| Node | one GPU node, named `gpu-node` in the manifests, with disk for the weights |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat` at revision `85864749cd611b4353ce1decdb286193298f64c7` |

## Cache preparation

### Step 1: Create artifact and prefetch

Save the manifest below as `model-delivery.yaml`. Replace both `gpu-node` values with your GPU
node's `kubernetes.io/hostname` label. The store and the prefetch placement must select the same
node.

Keep the pinned `revision`: the explicit *`allowPatterns`* list matches that commit and resolves to
12 files and 31,418,807,311 bytes. For another model, remove the DeepSeek-specific patterns or
replace them with a complete file list. Then size the binding quota and the node disk for the
resolved weights.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: gpustack-elastic
---
apiVersion: worker.gpustack.ai/v1
kind: ModelArtifact
metadata:
  name: deepseek-v2-lite-chat
  namespace: gpustack-elastic
spec:
  source:
    huggingFace:
      repository: deepseek-ai/DeepSeek-V2-Lite-Chat
      revision: 85864749cd611b4353ce1decdb286193298f64c7
  allowPatterns:
    - model-00001-of-000004.safetensors
    - model-00002-of-000004.safetensors
    - model-00003-of-000004.safetensors
    - model-00004-of-000004.safetensors
    - config.json
    - configuration_deepseek.py
    - generation_config.json
    - model.safetensors.index.json
    - modeling_deepseek.py
    - tokenization_deepseek_fast.py
    - tokenizer.json
    - tokenizer_config.json
---
apiVersion: worker.gpustack.ai/v1
kind: ModelStore
metadata:
  name: rtx6000-models
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: gpu-node
---
apiVersion: worker.gpustack.ai/v1
kind: ModelStoreBinding
metadata:
  name: rtx6000-models
  namespace: gpustack-elastic
spec:
  storeRefs:
    - name: rtx6000-models
  quota:
    bytes: 64Gi
---
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: deepseek-v2-lite-chat
  namespace: gpustack-elastic
spec:
  artifactRef:
    name: deepseek-v2-lite-chat
  bindingRef:
    name: rtx6000-models
  placement:
    nodeSelector:
      matchLabels:
        kubernetes.io/hostname: gpu-node
  minReady: 1
```

Apply it:

```bash
kubectl apply -f model-delivery.yaml
```

If admission rejects the prefetch, check the binding quota and the store name against
[Troubleshooting](#troubleshooting).

### Step 2: Verify the cached model

Wait for the artifact to resolve and the prefetch to become available, then confirm their status:

```bash
kubectl -n gpustack-elastic wait --for=condition=Resolved modelartifact/deepseek-v2-lite-chat --timeout=5m
kubectl -n gpustack-elastic wait --for=condition=Available modelprefetch/deepseek-v2-lite-chat --timeout=20m
kubectl -n gpustack-elastic get modelartifacts,modelprefetches
```

Captured output:

```text
NAME                                                     SOURCE                              REVISION       SIZE          READY   DOWNLOADING   RESOLVED
modelartifact.worker.gpustack.ai/deepseek-v2-lite-chat   deepseek-ai/DeepSeek-V2-Lite-Chat   85864749cd61   31418807311   1       0             True

NAME                                                     ARTIFACT                BINDING          READY   DESIRED   AVAILABLE
modelprefetch.worker.gpustack.ai/deepseek-v2-lite-chat   deepseek-v2-lite-chat   rtx6000-models   1       1         True
```

A healthy run shows `RESOLVED` as `True` and `AVAILABLE` as `True`, with one ready node out of one
desired. If either wait times out or reports `False`, inspect the conditions and match the reason in
[Troubleshooting](#troubleshooting):

```bash
kubectl -n gpustack-elastic get modelprefetch deepseek-v2-lite-chat \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

Then confirm that the selected node holds this artifact's resolved digest:

```bash
HOST=$(kubectl -n gpustack-elastic get modelprefetch deepseek-v2-lite-chat -o json |
  jq -r '.spec.placement.nodeSelector["kubernetes.io/hostname"]')
NODE=$(kubectl get nodes -l "kubernetes.io/hostname=$HOST" -o jsonpath='{.items[0].metadata.name}')
DIGEST=$(kubectl -n gpustack-elastic get modelartifact deepseek-v2-lite-chat \
  -o jsonpath='{.status.resolved.manifestDigest}')
kubectl get nodemodelstore "$NODE" -o json |
  jq --arg d "$DIGEST" '.status.models[]? | select(.digest == $d) | {sizeBytes, state}'
```

Captured output:

```json
{
  "sizeBytes": 31418807311,
  "state": "Ready"
}
```

The matched entry is `Ready`, and its size equals the artifact's `SIZE`. If the output is empty or
`state` is not `Ready`, inspect `kubectl get nodemodelstore "$NODE" -o yaml` for the digest's
*`reason`*, *`message`* and *`retryTime`*.

Warm-up `Pod`s verify the SHA-256 digest of all 12 files against the resolved manifest and terminate
on completion; collect their logs while running if you need per-file output. Wait for `Ready` before
you create a serving workload.

Continue with [Elastic EP](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) to grow one collective, or with
[External DP](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) to route requests across fixed *DP* ranks. Use the
namespace and artifact from Step 1 in either manifest.

## Step 3: Pin cached weights

When usage passes the high watermark, the node cache removes unreferenced weights, oldest first. A
pin keeps the digest out of that collection. If the `ModelStoreBinding` does not set *`allowPinned`*,
admission refuses *`retention.pinned`*, so the administrator grants it first.

In `model-delivery.yaml`, add the grant to the binding:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelStoreBinding
 metadata:
   name: rtx6000-models
   namespace: gpustack-elastic
 spec:
   storeRefs:
     - name: rtx6000-models
   quota:
     bytes: 64Gi
+  allowPinned: true
```

Then request the pin on the prefetch:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelPrefetch
 metadata:
   name: deepseek-v2-lite-chat
   namespace: gpustack-elastic
 spec:
   artifactRef:
     name: deepseek-v2-lite-chat
   bindingRef:
     name: rtx6000-models
   placement:
     nodeSelector:
       matchLabels:
         kubernetes.io/hostname: gpu-node
   minReady: 1
+  retention:
+    pinned: true
```

Apply the file again. The binding comes before the prefetch in the file, so the grant exists when
admission checks the pin:

```bash
kubectl apply -f model-delivery.yaml
kubectl -n gpustack-elastic get modelprefetch deepseek-v2-lite-chat -o jsonpath='{.spec.retention.pinned}'
```

Example output (illustrative):

```text
true
```

If admission still rejects the prefetch, confirm that the binding shows `allowPinned: true`. A
pinned digest is never a collection candidate on the target nodes, but it still counts toward the
store's usage. See [Pinning and expiry](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md#pinning-and-expiry).

## Cleanup

Keep the prefetch while a serving application needs the cached weights. After you delete those
consumers, remove the dedicated resources:

```bash
kubectl -n gpustack-elastic delete modelprefetch deepseek-v2-lite-chat
kubectl -n gpustack-elastic delete modelstorebinding rtx6000-models
kubectl -n gpustack-elastic delete modelartifact deepseek-v2-lite-chat
kubectl delete modelstore rtx6000-models
```

Deleting these objects does not delete the Kubernetes cluster or its disks. Cached weights then
follow the [retention rules](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md).

## Limitations

The hostname selector in *`placement.nodeSelector`* targets one node, and `minReady: 1` marks the
prefetch `Available` once that node holds the weights. A serving `Pod` scheduled on another node
finds no warm cache.

High-watermark eviction order, pin behavior and *`retention.ttlAfterLastUse`* expiry are covered in
[Pinning and expiry](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md#pinning-and-expiry) and
[Cache collection rules](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md#cache-collection-rules).

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `ModelArtifact` resolution failed | Condition: `Resolved=False/AccessDenied` | Private or gated repository. Check the namespace `Secret` token and inspect the `InvalidToken` event. |
| `ModelArtifact` resolution failed | Condition: `Resolved=False/RevisionNotFound` | Branch, tag or commit hash misspelled. ModelScope does not silently default to master. |
| `ModelArtifact` resolution failed | Condition: `Resolved=False/SourceUnavailable` | Hub endpoint unreachable, large directory enumeration timeout, or 5xx network error. |
| `ModelArtifact` resolution failed | Condition: `Resolved=False/EmptyManifest` | Allow or ignore patterns excluded every file. Review the glob patterns. |
| `ModelDeployment` blocked at `Starting` | Condition: `WeightsReady=False/NodeDeliveryUnavailable` | CSI driver `model.csi.gpustack.ai` is missing, or Helm values set `modelManager.enabled: false`. |
| `ModelDeployment` blocked with pattern filters | Condition: `WeightsReady=False/FilterNeedsNodeDelivery` | Pattern filtering needs `Node` delivery mode. Set `model-artifact-delivery-mode: Node`. |
| `Pod` stuck in `ContainerCreating` | Condition: `WeightsReady=False/Materializing` | The node plugin is downloading. The `Pod` `FailedMount` event shows the downloaded byte count. |
| Weight download failed on node | Condition: `WeightsReady=False/MaterializationFailed` | Run `kubectl get nodemodelstore <node> -o yaml` and read the digest's *`reason`*, *`message`* and *`retryTime`*. |
| `ModelPrefetch` does not reach `Available` | Condition: `Degraded=True/NodeFailed` | One or more target nodes failed the download. Check node disk capacity and logs. |
| `ModelPrefetch` shows fewer ready nodes than *`minReady`* | Condition: `Lapsed=True/RetentionTTLExpired` | `Node` copies went unused past *`retention.ttlAfterLastUse`*. They left *`readyNodes`*, released any pin and are not re-warmed. Raise or remove the TTL. |
| New `ModelPrefetch` rejected at admission | Over quota or unauthorized | The prefetch exceeds `ModelStoreBinding` *`quota.bytes`*, sets *`retention.pinned`* without `allowPinned: true`, or targets an undeclared store. |
| Peer sync downloads continuously from Hub | `modelManager.port: 0` or peer sync disabled | Restore a positive `modelManager.port` in Helm values if peer sync is intended, enable the `model-store-peer-sync` `Setting`, and verify that `NodeModelStore` *`spec.peerSyncEnabled`* reconciles to `true`. |

---

**See also** — [Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) (capability overview) ·
[Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md) (configuration and retention)

**Next** → [Model Deployment Walkthroughs](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md)
