# Prefill and Decode Walkthrough

Deploy a disaggregated LLM serving topology with dedicated prefill and decode roles fronted by a router.
The operator coordinates joint admission across both roles and routes requests through a unified endpoint.
Configuration diffs show how the roles cooperate. This page records no captured run.

## Contents

- [Deployment shape](#deployment-shape)
- [Prerequisites](#prerequisites)
- [Step 1: start disaggregated serving](#step-1-start-disaggregated-serving)
- [Step 2: pin the KV cache transfer protocol](#step-2-pin-the-kv-cache-transfer-protocol)
- [Step 3: verify router endpoints and inference](#step-3-verify-router-endpoints-and-inference)
- [Step 4: inspect joint admission and scaling](#step-4-inspect-joint-admission-and-scaling)
- [Step 5: release the application](#step-5-release-the-application)
- [Troubleshooting](#troubleshooting)

## Deployment shape

Prefill and decode disaggregation separates prompt processing from token generation.
A compute-heavy Prefill role processes initial prompt context with high parallel throughput.
A memory-bandwidth-bound Decode role generates subsequent tokens sequentially.

A managed Router fronts both roles and exposes a single external HTTP endpoint.
The Router parses prompt requests, forwards prefill work, and coordinates KV cache transfer to decoders.
Kueue schedules both roles as a joint unit, preventing half-admitted workloads.

## Prerequisites

Complete [cluster and GPU preparation](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/index.md#prepare-the-cluster) and
[model prefetch](/gpustack-operator/v0.9.0/docs/walkthroughs/model-delivery/prefetch/index.md).
Ensure sufficient GPUs exist for both roles simultaneously (at least six GPUs for this example).
Use the shared namespace, cached artifact, and GPU InstanceType.

## Step 1: start disaggregated serving

Save the following manifest as `pd-deployment.yaml`.
The definition specifies one Prefill replica with two GPUs and two Decode replicas with two GPUs each.
The `spec.router` stanza deploys `llm-d-router` as the unified front door.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: deepseek-pd
  namespace: gpustack-elastic
spec:
  model:
    name: /models
    artifactRef:
      name: deepseek-v2-lite-chat
  engine:
    name: vLLM
    version: "0.29.0"
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      instanceType: elastic-cpu-gpu
      image: gpustack/runner:cuda13.0-vllm0.29.0
      resources:
        accelerator: "2"
    - name: decode
      kind: Decode
      replicas: 2
      instanceType: elastic-cpu-gpu
      image: gpustack/runner:cuda13.0-vllm0.29.0
      resources:
        accelerator: "2"
```

Apply the manifest to the cluster:

```bash
kubectl apply -f pd-deployment.yaml
kubectl -n gpustack-elastic get modeldeployment deepseek-pd
```

## Step 2: pin the KV cache transfer protocol

Disaggregated serving requires prompt KV tensors to flow from prefill workers to decode workers.
The pair in Step 1 already hands blocks over directly. Unset `spec.kvTransfer.protocol` renders `tcp`.
Pin the protocol explicitly. Add the lines marked `+` to `pd-deployment.yaml`:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: deepseek-pd
   namespace: gpustack-elastic
 spec:
+  kvTransfer:
+    protocol: TCP
   router:
     name: llm-d-router
   roles:
     - name: prefill
       kind: Prefill
```

Apply the update:

```bash
kubectl apply -f pd-deployment.yaml
```

The protocol is deployment-wide, because it describes one link between the two roles.
It accepts `Auto`, `TCP`, `RDMA`, `EFA`, `CANN` and `ROCM`. `Auto` renders `tcp` too.

An edit restarts every role, because the value renders into both ends' arguments.
The roles can run different protocols until both converge.
`RDMA` and `EFA` also need a matching device request on each role.
See the [RDMA walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/rdma/network-endpoints/index.md#step-3-prefill-and-decode-interface-requests).

A shared store through `spec.kvCache.poolRef` is part of the deployment identity.
Declare it in the first apply. Adding it later is refused, because a different cache describes a different deployment.
See [deployment identity fields](/gpustack-operator/v0.9.0/docs/modules/model-deployment/deployment/index.md#deployment-identity-fields).

## Step 3: verify router endpoints and inference

Monitor status conditions until all roles and the router reach ready states:

```bash
kubectl -n gpustack-elastic get modeldeployment deepseek-pd -o yaml
```

Read the reported endpoint. It carries the scheme and a cluster-internal `.svc` host name:

```bash
ROUTER_ENDPOINT=$(kubectl -n gpustack-elastic get modeldeployment deepseek-pd \
  -o jsonpath='{.status.endpoint}')
echo "Router endpoint: $ROUTER_ENDPOINT"
```

Send a test completion request through the Router from inside the cluster:

```bash
kubectl -n gpustack-elastic run test-client --rm -i --restart=Never \
  --image=curlimages/curl -- \
  curl -s "${ROUTER_ENDPOINT}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v2-lite-chat",
    "messages": [{"role": "user", "content": "Explain prefill and decode disaggregation."}],
    "max_tokens": 16
  }'
```

The Router receives the request, delegates the prompt phase, transfers KV tensors, and streams tokens.
`status.endpoint` remains empty until the router Pod passes its readiness check.

## Step 4: inspect joint admission and scaling

The operator manages Kueue workloads across all declared roles simultaneously.
Inspect the Kueue Workloads that the deployment's Pods own:

```bash
kubectl -n gpustack-elastic get workloads
```

Joint admission guarantees that neither role starts execution without capacity for the other.
If the cluster lacks capacity for decode workers, prefill workers remain queued rather than idling.
You can adjust `replicas` independently on either role to balance prompt versus token throughput.

Role kinds (`kind: Prefill`, `kind: Decode`), role names and the declared role set are frozen.
The `size`, `resources`, `instanceType` and `command` of an existing role are editable.
An edit replaces whole replicas, one at a time, with fresh Kueue admission.
See [rollout behavior](/gpustack-operator/v0.9.0/docs/modules/model-deployment/deployment/index.md#rollout-behavior).

## Step 5: release the application

After testing disaggregated serving, delete the deployment to release GPUs and router services:

```bash
kubectl -n gpustack-elastic delete modeldeployment deepseek-pd
kubectl -n gpustack-elastic get pods,services -l app.kubernetes.io/instance=deepseek-pd
```

Verify that the accelerators are free again. Each card reports `remaining`:

```bash
kubectl get devices -o json | jq '[.items[].status.groups[].accelerators[] |
  {mode, remaining, allocatedSlices: (.allocatedSlices // 0)}]'
```

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| Deployment stuck in `Starting`, Kueue composes no Workload | `QuotaReserved=False/PodGroupIncomplete` | Fewer replicas exist than a role declares. Read the message for the counts, then check the missing or unschedulable Pods |
| No Workload asks for quota any more | `QuotaReserved=False/Parked` | The groups could not all be placed, so their Workloads were deactivated. An identical re-apply does not clear this. Free the capacity and reactivate the Workloads, or recreate the deployment |
| Part of the deployment was taken by another workload | `QuotaReserved=False/PreemptedInPart` | A higher-priority workload reclaimed some replicas. The replicas that kept their quota hold accelerators until the reclaimed ones are admitted again or the deployment is deleted. Check what took the quota |
| `status.endpoint` is empty | `RouterReady=False/NoReadyReplicas` | The router has no ready replica. It may still pull its image or fail readiness. Inspect the router Pod |
| Router is not created | `RouterReady=False/RenderFailed` | The router render was refused as a spec problem, so no router Pod exists. Read the condition message and check `spec.router` |
| Disaggregated roles fail KV transfer | Engine logs on both roles | The protocol is deployment-wide, so the two ends cannot name different values. Check that the image carries the matching Mooncake build, that the device request fits the protocol, and that the nodes can reach each other |
| Router sees no cache events | `KVEventsPublishing=False/PublisherDisabled` | A role that produces cache blocks renders no publisher. A routed SGLang deployment always reports this, because only vLLM renders one. The condition reports configuration, not traffic. See [KV events](/gpustack-operator/v0.9.0/docs/modules/model-deployment/deployment/index.md#operator-owned-keys) |
| Missing role readiness | `RoleKindsReady=False` | One of the role kinds (Prefill or Decode) has zero ready replicas; check failed engine pods |

---

**See also** — [Model Deployment Prefill and Decode](/gpustack-operator/v0.9.0/docs/modules/model-deployment/prefill-decode/index.md) (configuration and transfer) ·
[Routing](/gpustack-operator/v0.9.0/docs/modules/model-deployment/routing/index.md) (router engine combinations) ·
[Elastic EP Walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/elastic-ep/index.md) (collective expansion) ·
[External DP Walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/external-dp/index.md) (fixed per-rank serving)

**Next** → [Multi-Host Serving Walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/multi-host/index.md) — scale models across multiple GPU nodes.
