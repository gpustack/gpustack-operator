# Multi-Host Serving Walkthrough

Deploy large language models across multiple physical GPU nodes in a single coordinated serving group.
When a model exceeds the memory of a single machine, an instance spans multiple Pods with gang admission.
The operator coordinates leader discovery and environment variable injection across all group members.

## Contents

- [Deployment shape](#deployment-shape)
- [Prerequisites](#prerequisites)
- [Step 1: define the multi-host deployment](#step-1-define-the-multi-host-deployment)
- [Step 2: verify gang admission and leader injection](#step-2-verify-gang-admission-and-leader-injection)
- [Step 3: verify distributed inference](#step-3-verify-distributed-inference)
- [Step 4: scale replicas and update member size](#step-4-scale-replicas-and-update-member-size)
- [Step 5: release the application](#step-5-release-the-application)
- [Troubleshooting](#troubleshooting)

## Deployment shape

A multi-host serving role configures `spec.roles[].size` greater than one.
Each serving replica forms a collective unit composed of *size* distinct member Pods.
All member Pods in a group live and die together under gang scheduling.

Member `m0` acts as the primary master replica and receives all incoming Service traffic.
Subordinate members (`m1`, `m2`, etc.) run auxiliary parallel workers and synchronize with `m0`.
The operator injects cluster discovery environment variables into each container automatically.

## Prerequisites

Prepare a cluster with at least two physical GPU nodes containing identical accelerator counts.
Complete [cluster preparation](_index.md#prepare-the-cluster) and [model prefetch](../model-delivery/prefetch.md).
Verify inter-node network connectivity and high-speed fabric (such as RoCE, InfiniBand, or AWS EFA).
Each node in this walkthrough provides eight GPUs.
Tensor parallelism spans the GPUs inside each host. Pipeline parallelism spans the two hosts.

This layout is a configuration example. The walkthrough does not record a measured run on sixteen GPUs.

## Step 1: define the multi-host deployment

Save the following definition as `multi-host.yaml`.
The specification declares `replicas: 1` and `size: 2`, allocating sixteen whole GPUs across two Pods.
The launch script configures tensor parallelism across eight local GPUs and pipeline parallelism across the two hosts.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: deepseek-multi-host
  namespace: gpustack-elastic
spec:
  model:
    name: /models
    artifactRef:
      name: deepseek-v2-lite-chat
  engine:
    name: vLLM
    version: "0.29.0"
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 2
      instanceType: elastic-cpu-gpu
      image: gpustack/runner:cuda13.0-vllm0.29.0
      resources:
        accelerator: "8"
      command:
        - /bin/bash
        - -c
        - |
          set -euo pipefail
          set -- --tensor-parallel-size 8 \
            --pipeline-parallel-size "${GPUSTACK_REPLICA_SIZE}" \
            --distributed-executor-backend mp \
            --nnodes "${GPUSTACK_REPLICA_SIZE}" \
            --node-rank "${GPUSTACK_MEMBER_INDEX}" \
            --master-addr "${GPUSTACK_REPLICA_LEADER_ADDRESS}"
          if [ "${GPUSTACK_MEMBER_INDEX}" -gt 0 ]; then
            set -- "$@" --headless
          fi
          exec vllm serve /models --served-model-name deepseek-v2-lite-chat \
            --host 0.0.0.0 --port 8000 "$@"
```

The operator does not compose multi-node arguments. A multi-host role takes over the command line with
`command`, and the script reads the three variables the operator injects into every member. Member
`m0` serves the API. The other members start with `--headless`. A role with `command` reports
`status.roles[].unmanaged: true`, because the operator renders no engine argument for it. See
[the override tiers](../../modules/model-deployment/deployment.md#the-three-override-tiers).

Apply the deployment manifest:

```bash
kubectl apply -f multi-host.yaml
kubectl -n gpustack-elastic get modeldeployment deepseek-multi-host
```

## Step 2: verify gang admission and leader injection

Check the member Pods:

```bash
kubectl -n gpustack-elastic get pods -l app.kubernetes.io/instance=deepseek-multi-host
```

You will observe two member Pods named with their member index suffixes:
`deepseek-multi-host-server-r0-m0` and `deepseek-multi-host-server-r0-m1`.
Inspect the injected environment variables in the secondary worker `m1`:

```bash
kubectl -n gpustack-elastic exec deepseek-multi-host-server-r0-m1 -- env | grep GPUSTACK_
```

The operator automatically populates:
- `GPUSTACK_REPLICA_LEADER_ADDRESS`: the DNS name of member `m0`, `deepseek-multi-host-server-r0-m0.deepseek-multi-host-server-r0`.
- `GPUSTACK_REPLICA_SIZE`: total member count (`2`).
- `GPUSTACK_MEMBER_INDEX`: the zero-based index of this specific member (`1`).

Inspect the Kubernetes Service created for the role:

```bash
kubectl -n gpustack-elastic get svc deepseek-multi-host-server -o yaml
```

The Service selector targets only member `m0`. External traffic lands on the primary replica.

## Step 3: verify distributed inference

Once both member Pods reach the `Ready` condition, query the server Service:

```bash
kubectl -n gpustack-elastic run test-client --rm -i --restart=Never \
  --image=curlimages/curl -- \
  curl -s http://deepseek-multi-host-server:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v2-lite-chat",
    "messages": [{"role": "user", "content": "Hello distributed inference!"}],
    "max_tokens": 16
  }' | jq .
```

The master rank coordinates distributed collective operations with the secondary worker and returns tokens.

## Step 4: scale replicas and update member size

To scale serving throughput, increase `replicas` to launch additional multi-host groups:

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

Scaling to `replicas: 2` creates another complete two-host group (`m0` and `m1`), reserving sixteen additional GPUs.

To change the member topology of each instance, you can also update `size`, `resources` or `instanceType`.
These edits do not resize a running collective engine in place.

The operator deletes each old instance and its Workload first.
It then creates the replacement in that slot and waits for fresh Kueue gang admission.
The next old instance is deleted only after the replacement is admitted.
A role with one replica therefore stops serving during the replacement.

The script reads `GPUSTACK_REPLICA_SIZE`, so the pipeline degree follows `size`.
The tensor degree is fixed at 8, so it must still match the accelerators per member.
See [Rollout behavior](../../modules/model-deployment/deployment.md#rollout-behavior).

## Step 5: release the application

Remove the deployment and verify that all GPU allocations are freed:

```bash
kubectl -n gpustack-elastic delete modeldeployment deepseek-multi-host
kubectl -n gpustack-elastic get pods -l app.kubernetes.io/instance=deepseek-multi-host
```

Confirm that the accelerators on both host nodes are free again. Each card reports `remaining`:

```bash
kubectl get devices -o json | jq '[.items[].status.groups[].accelerators[] |
  {mode, remaining, allocatedSlices: (.allocatedSlices // 0)}]'
```

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| Deployment stuck in `Starting`, Kueue composes no Workload | `QuotaReserved=False/PodGroupIncomplete` | Fewer instances exist than the role declares. Read the message for the counts, then check the Pods that are missing or unschedulable |
| An instance was lost and is being rebuilt | `ReplicasUpToDate=False/ReplacementInProgress` | A replica disappeared for a reason outside the spec: a drained node, Kueue reclaiming quota, an eviction or a manual delete. The Pods of an instance share one lifecycle, so the whole instance is rebuilt. Check the node and Kueue events |
| Workload no longer asks for quota | `QuotaReserved=False/Parked` | The Workload was deactivated. Free capacity, then reactivate or recreate it. Waiting does not help |
| Secondary worker cannot reach the leader | Engine logs on `m1` | Check that the headless Service resolves `GPUSTACK_REPLICA_LEADER_ADDRESS` and that no NetworkPolicy blocks the engine's distributed port |
| Engine errors on parallel size | Engine startup log | Check that `--tensor-parallel-size` equals the accelerators per member and that the pipeline degree equals `size` |
| Pods stay `Pending` | Kueue Workload events | The ClusterQueue has too little quota. Raise the pool quota or lower the per-member accelerator request |

---

**See also** — [Model Deployment Configuration](../../modules/model-deployment/deployment.md) (role fields and scaling) ·
[Prefill and Decode Walkthrough](prefill-decode.md) (disaggregated serving) ·
[Elastic EP Walkthrough](elastic-ep.md) (collective expansion) ·
[External DP Walkthrough](external-dp.md) (fixed per-rank serving)

**Next** → [Model Deployment Status](../../modules/model-deployment/status.md) — observe status conditions and phases.
