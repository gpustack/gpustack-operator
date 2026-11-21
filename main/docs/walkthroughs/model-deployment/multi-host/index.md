# Multi-Host Serving Walkthrough

Span a single serving instance across multiple physical GPU hosts when a model's memory footprint
exceeds one node, using gang admission and operator-injected leader coordinates.

## Contents

- [Summary](#summary)
- [Step 1: Deploy distributed model](#step-1-deploy-distributed-model)
- [Verification](#verification)
- [Step 4: Scale replica groups](#step-4-scale-replica-groups)
- [Cleanup](#cleanup)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

Setting *`spec.roles[].size`* above `1` groups *`size`* fate-sharing member `Pod`s into each serving
replica under Kueue gang scheduling. Member `m0` runs the Ray head and receives all `Service` HTTP
traffic, while subordinate members (`m1`..`mN`) join `m0` using injected discovery variables:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | Resolves `deepseek-v2-lite-chat` weights warmed onto both GPU hosts |
| `AcceleratorType` | cluster | operator | Discovered whole-GPU H100 pool requested by every member |
| `ModelDeployment` | namespace | user | Two-node (`size: 2`, 16-GPU) Ray serving group |

This 16-GPU layout (*tensor parallelism (TP)* = 8 within each node, *pipeline parallelism (PP)* = 2 across two nodes) is an illustrative configuration example.
Complete [Cluster preparation](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md#cluster-preparation), [GPU pool selection](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md#gpu-pool-selection), and [Model Prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) in namespace `gpustack-elastic` with `minReady: 2` across both GPU hosts so both `m0` and `m1` mount warm local weights.

### Environment

| Item | Value |
|---|---|
| Nodes | Two physical GPU hosts (`8` GPUs each, `16` GPUs total) |
| Fabric | High-speed inter-node network (InfiniBand, RoCE, or AWS EFA) |
| `AcceleratorType` | `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` |
| Namespace and model | `gpustack-elastic` with `ModelArtifact` `deepseek-v2-lite-chat` |

## Step 1: Deploy distributed model

Because the operator does not synthesize multi-node Ray startup commands, multi-host roles use the
take-over tier (*`roles[].command`*, which sets `status.roles[].unmanaged: true`; see
[Engine overrides](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#engine-overrides)). Save
`multi-host.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: deepseek-multi-host
  namespace: gpustack-elastic
spec:
  model:
    name: deepseek-v2-lite-chat
    artifactRef:
      name: deepseek-v2-lite-chat
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.29.0
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 2
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.29.0
      resources:
        accelerator: "8"
      command:
        - /bin/bash
        - -c
        - |
          set -euo pipefail
          if [ "${GPUSTACK_MEMBER_INDEX}" -eq 0 ]; then
            ray start --head --port=6379 --disable-usage-stats
            exec vllm serve /var/lib/gpustack/model --served-model-name deepseek-v2-lite-chat \
              --host 0.0.0.0 --port 8000 \
              --tensor-parallel-size 8 \
              --pipeline-parallel-size "${GPUSTACK_REPLICA_SIZE}" \
              --distributed-executor-backend ray
          else
            exec ray start --address="${GPUSTACK_REPLICA_LEADER_ADDRESS}:6379" --block
          fi
```

Apply `multi-host.yaml`:

```bash
kubectl apply -f multi-host.yaml
kubectl -n gpustack-elastic get modeldeployment deepseek-multi-host
```

## Verification

### Step 2: Verify gang admission

Confirm that Kueue admitted both members (`deepseek-multi-host-server-r0-m0` and `r0-m1`) together and
inspect the injected `GPUSTACK_*` coordinates on `m1`:

```bash
kubectl -n gpustack-elastic get pods -l app.kubernetes.io/instance=deepseek-multi-host -o wide
kubectl -n gpustack-elastic exec deepseek-multi-host-server-r0-m1 -- env | grep '^GPUSTACK_' | sort
```

Example output (illustrative):

```text
GPUSTACK_MEMBER_INDEX=1
GPUSTACK_REPLICA_LEADER_ADDRESS=deepseek-multi-host-server-r0-m0.deepseek-multi-host-server-r0
GPUSTACK_REPLICA_SIZE=2
```

Verify that `Service/deepseek-multi-host-server` selects only leader `m0` (`member-index: "0"`):

```bash
kubectl -n gpustack-elastic get svc deepseek-multi-host-server -o jsonpath='{.spec.selector}{"\n"}'
```

If either member stays `Pending`, inspect `QuotaReserved` and `Workload` conditions in
[Troubleshooting](#troubleshooting).

### Step 3: Verify distributed inference

Once `m0` is serving on `:8000` (take-over roles omit operator health probes unless configured in your
entrypoint), send a completion request to `Service/deepseek-multi-host-server`:

```bash
kubectl -n gpustack-elastic run test-client --rm -i --restart=Never \
  --image=curlimages/curl -- \
  curl -fsS http://deepseek-multi-host-server:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v2-lite-chat",
    "messages": [{"role": "user", "content": "Hello distributed inference!"}],
    "max_tokens": 16
  }' | jq '{model, finish_reason: .choices[0].finish_reason, tokens: .usage.completion_tokens}'
```

Confirm `"model": "deepseek-v2-lite-chat"` and positive `tokens`. If `curl` fails to connect, inspect
`m0` and `m1` container logs to confirm `m1` joined the Ray head on `:6379`.

## Step 4: Scale replica groups

Increase *`replicas`* to add a second independent 16-GPU two-node group (requiring 32 GPUs total):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: deepseek-multi-host
   namespace: gpustack-elastic
 spec:
   roles:
     - name: server
-      replicas: 1
+      replicas: 2
       size: 2
```

Editing *`size`*, *`resources`*, or *`acceleratorType`* replaces whole replica groups one slot per pass
rather than resizing Ray in place: the operator deletes an old replica and its `Workload` before
admitting its replacement, so a `replicas: 1` deployment experiences downtime during replacement
([Rollout behavior](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#rollout-behavior)). Keep
`--tensor-parallel-size` aligned with per-member `resources.accelerator`.

## Cleanup

Delete `deepseek-multi-host` and verify all 16 GPUs return full `remaining` credits:

```bash
kubectl -n gpustack-elastic delete modeldeployment deepseek-multi-host
kubectl -n gpustack-elastic get pods -l app.kubernetes.io/instance=deepseek-multi-host
kubectl get devices -o json | jq '[.items[].status.groups[].accelerators[] |
  {mode, remaining, allocatedSlices: (.allocatedSlices // 0)}]'
```

## Limitations

- **All-or-nothing gang scheduling**: all *`size`* members must reserve pool capacity together, or the
  entire replica waits `Pending`.
- **Leader-only HTTP ingress**: under *internal load balancing (Internal LB)*, only `m0` serves HTTP
  traffic.
- **Full-replica replacement on *`size`* edits**: changing *`size`* tears down and recreates each
  replica group rather than expanding live.

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `ModelDeployment` stuck in `Starting`, missing `Workload` | `QuotaReserved=False/PodGroupIncomplete` | Read `QuotaReserved` on `ModelDeployment` and inspect missing or rejected member `Pod`s. |
| Entire replica tears down and rebuilds | `ReplicasUpToDate=False/ReplacementInProgress` | One member `Pod` was evicted, drained, or deleted; because members share fate, the operator rebuilds the whole `size`-member replica. Check node and Kueue events. |
| Replica stays `Pending` for quota | `QuotaReserved=False/Pending` | `ClusterQueue` lacks `size × accelerator` free GPUs across distinct hosts; free capacity or reduce *`replicas`* / *`size`*. |
| `m1` cannot join `m0` Ray head | Connection errors in `m1` logs | Verify headless `Service` DNS resolution for `GPUSTACK_REPLICA_LEADER_ADDRESS` and allow port `6379` plus Ray/NCCL ephemeral ports in `NetworkPolicy`. |
| vLLM exits on invalid parallel world size | Startup error in `m0` logs | Ensure `--tensor-parallel-size` equals per-member `resources.accelerator` (`8`) and `--pipeline-parallel-size` equals `GPUSTACK_REPLICA_SIZE` (`2`). |

---

**See also** — [Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) (multi-member roles and
override tiers) · [Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) (disaggregated serving) ·
[Elastic EP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) (in-place collective expansion) ·
[External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) (fixed per-rank serving)

**Next** → [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) — review serving contracts, routers, and benchmarks.
