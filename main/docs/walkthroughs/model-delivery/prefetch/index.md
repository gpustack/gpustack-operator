# Model Prefetch Walkthrough

Cache DeepSeek-V2-Lite-Chat on one GPU node before starting inference.
This shared preparation serves both the Elastic EP and External DP walkthroughs.
The output below was captured with node model delivery enabled.

## Contents

- [Prerequisites](#prerequisites)
- [Step 1: create the artifact and prefetch](#step-1-create-the-artifact-and-prefetch)
- [Step 2: verify the cached model](#step-2-verify-the-cached-model)
- [Step 3: start a serving workload](#step-3-start-a-serving-workload)
- [Step 4: release the cache](#step-4-release-the-cache)

## Prerequisites

Enable [node model delivery](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md) and select the GPU node.
You need `kubectl`, `jq`, access to cluster-scoped model stores, and enough disk for the weights.
The example uses a dedicated namespace and store grant.

## Step 1: create the artifact and prefetch

Save the following as `model-delivery.yaml`. Replace both `gpu-node` values with your GPU node's
`kubernetes.io/hostname` label. The store grant and the prefetch placement must select that same node.
The fixed file list resolves to 12 files and 31,418,807,311 bytes.

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
  allowPinned: true
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
  retention:
    pinned: true
```

## Step 2: verify the cached model

```bash
kubectl apply -f model-delivery.yaml
kubectl -n gpustack-elastic get modelartifacts,modelprefetches
kubectl -n gpustack-elastic get modelprefetch deepseek-v2-lite-chat -o yaml
kubectl get nodemodelstores -o json
```

Captured `kubectl get modelartifacts,modelprefetches` output:

```text
NAME                                                     SOURCE                              REVISION       SIZE          READY   DOWNLOADING   RESOLVED
modelartifact.worker.gpustack.ai/deepseek-v2-lite-chat   deepseek-ai/DeepSeek-V2-Lite-Chat   85864749cd61   31418807311   1       0             True

NAME                                                     ARTIFACT                BINDING          READY   DESIRED   AVAILABLE
modelprefetch.worker.gpustack.ai/deepseek-v2-lite-chat   deepseek-v2-lite-chat   rtx6000-models   1       1         True
```

Check the matching node's model entry as well:

```bash
kubectl get nodemodelstores -o json |
  jq '.items[] | .status.models[]? | {digest, sizeBytes, state}'
```

Captured projection:

```json
{
  "digest": "sha256:1369aae0b5cc2d0e260557bcea04b0d6aad40f8bb6accdac60f1ee543887d9f7",
  "sizeBytes": 31418807311,
  "state": "Ready"
}
```

The validation also retained the successful warm-up Pod and compared its 12 SHA-256 results with
the resolved manifest. Warm-up Pods are transient; collect their logs before completion if you need
that evidence. See [Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md).
Wait for the required content before creating the serving workload.

## Step 3: start a serving workload

Continue with [Elastic EP](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) to grow one collective,
or [External DP](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) to route requests across fixed DP ranks.
Use the namespace and artifact created here in either manifest.

## Step 4: release the cache

Keep the prefetch while a serving application needs the cached weights.
After deleting those consumers, remove the dedicated resources:

```bash
kubectl -n gpustack-elastic delete modelprefetch deepseek-v2-lite-chat
kubectl -n gpustack-elastic delete modelstorebinding rtx6000-models
kubectl -n gpustack-elastic delete modelartifact deepseek-v2-lite-chat
kubectl delete modelstore rtx6000-models
```

Cache collection follows the [retention rules](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md).
Deleting these CRs does not delete the Kubernetes cluster or its disks.

---

**See also** — [Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) ·
[Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md) (configuration and retention)

**Next** → [Model Deployment Walkthroughs](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md)
