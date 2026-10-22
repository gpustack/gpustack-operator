# Multi-Host Serving Walkthrough

Deploy large language models across multiple physical GPU nodes in a single coordinated serving group.
When a model exceeds the memory of a single machine, an instance spans multiple `Pod`s with gang admission.
The operator coordinates leader discovery and environment variable injection across all group members.

## Contents

- [Summary](#summary)
- [Step 1: Define the deployment](#step-1-define-the-deployment)
- [Step 2: Verify gang admission](#step-2-verify-gang-admission)
- [Step 3: Verify distributed inference](#step-3-verify-distributed-inference)
- [Step 4: Change replicas and size](#step-4-change-replicas-and-size)
- [Step 5: Release the application](#step-5-release-the-application)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

Multi-host serving runs a model whose *tensor parallelism (TP)* and *pipeline parallelism (PP)*
require more GPUs than one node provides. One serving instance spans multiple hosts as one
collective unit, admitted as a gang and served through its leader `m0`.

A multi-host serving role configures *`spec.roles[].size`* greater than one.
Each serving replica forms a collective unit composed of *`size`* distinct member `Pod`s.
All member `Pod`s in a group live and die together under gang scheduling.

Member `m0` acts as the primary master replica and receives all incoming `Service` traffic.
Subordinate members (`m1`, `m2`, etc.) run auxiliary parallel workers and synchronize with `m0`.
The operator injects cluster discovery environment variables into each container automatically.

Prepare a cluster with at least two physical GPU nodes containing identical accelerator counts.
Complete [cluster preparation](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/index.md#prepare-the-cluster) and [model prefetch](/gpustack-operator/v0.9.2/docs/walkthroughs/model-delivery/prefetch/index.md).
Verify inter-node network connectivity and high-speed fabric (such as RoCE, InfiniBand, or AWS EFA).
Each node in the configuration provides eight GPUs.
*TP* spans the GPUs inside each host. *PP* spans the two hosts.

This sixteen-GPU layout is a configuration example without measured runtime results. Unlike the
recorded H100 walkthroughs in `gpustack-bench`, it uses the `gpustack-elastic` namespace, a
`deepseek-v2-lite-chat` `ModelArtifact` in that namespace, and the `elastic-cpu-gpu` `InstanceType`
from [Prepare the GPU InstanceType](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/index.md#prepare-the-gpu-instancetype).

When running [model prefetch](/gpustack-operator/v0.9.2/docs/walkthroughs/model-delivery/prefetch/index.md), warm the artifact onto **both**
physical GPU nodes (`minReady: 2` across the GPU node pool rather than pinning a single
`kubernetes.io/hostname`) so both `m0` and `m1` mount cached weights locally.

## Step 1: Define the deployment

Save the following multi-host definition as `multi-host.yaml`.
The specification declares `replicas: 1` and `size: 2`, allocating sixteen whole GPUs across two `Pod`s.
The launch script sets *TP* = 8 across local GPUs and *PP* = 2 across the two hosts.

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
    image: gpustack/runner:cuda13.0-vllm0.29.0
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
          if [ "${GPUSTACK_MEMBER_INDEX}" -eq 0 ]; then
            ray start --head --port=6379 --disable-usage-stats
            exec vllm serve /models --served-model-name deepseek-v2-lite-chat \
              --host 0.0.0.0 --port 8000 \
              --tensor-parallel-size 8 \
              --pipeline-parallel-size "${GPUSTACK_REPLICA_SIZE}" \
              --distributed-executor-backend ray
          else
            exec ray start --address="${GPUSTACK_REPLICA_LEADER_ADDRESS}:6379" --block
          fi
```

The operator does not compose multi-node arguments. A multi-host role takes over the command line with
*`command`*, and the script reads the three variables the operator injects into every member.
This snippet illustrates how the operator wires member coordinates to the entrypoint script.

Production multi-host engines typically use Ray (`--distributed-executor-backend ray`).
Member `m0` initializes the Ray head cluster and serves the API; subordinate members join the Ray cluster
with `ray start --address=... --block`. A role with *`command`* reports `status.roles[].unmanaged: true`,
because the operator renders no engine argument for it. See
[Three override tiers](/gpustack-operator/v0.9.2/docs/modules/model-deployment/deployment/index.md#three-override-tiers).

Apply the deployment manifest:

```bash
kubectl apply -f multi-host.yaml
kubectl -n gpustack-elastic get modeldeployment deepseek-multi-host
```

## Step 2: Verify gang admission

Gang admission places the two members of the group together, and the operator injects the leader
address, the member index and the replica size into each member container.

Check the member `Pod`s:

```bash
kubectl -n gpustack-elastic get pods -l app.kubernetes.io/instance=deepseek-multi-host
```

Verify that two member `Pod`s have the expected member index suffixes:
`deepseek-multi-host-server-r0-m0` and `deepseek-multi-host-server-r0-m1`.
Inspect the injected environment variables in the secondary worker `m1`:

```bash
kubectl -n gpustack-elastic exec deepseek-multi-host-server-r0-m1 -- env | grep GPUSTACK_
```

The operator automatically populates:
- `GPUSTACK_REPLICA_LEADER_ADDRESS`: the DNS name of member `m0`, `deepseek-multi-host-server-r0-m0.deepseek-multi-host-server-r0`.
- `GPUSTACK_REPLICA_SIZE`: total member count (`2`).
- `GPUSTACK_MEMBER_INDEX`: the zero-based index of this specific member (`1`).

Inspect the Kubernetes `Service` created for the role:

```bash
kubectl -n gpustack-elastic get svc deepseek-multi-host-server -o yaml
```

The `Service` selector targets only member `m0`. External traffic lands on the primary replica.

## Step 3: Verify distributed inference

Once both member `Pod`s reach the `Ready` condition, query the server `Service`:

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

## Step 4: Change replicas and size

To scale serving throughput, increase *`replicas`* to launch additional multi-host groups:

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

To change the member topology of each instance, you can also update *`size`*, *`resources`* or *`instanceType`*.
These edits do not resize a running collective engine in place.

The operator deletes each old instance and its `Workload` first.
It then creates the replacement in that slot and waits for fresh Kueue gang admission.
The next old instance is deleted only after the replacement is admitted.
A role with one replica therefore stops serving during the replacement.

The script reads `GPUSTACK_REPLICA_SIZE`, so the *PP* size follows *`size`*.
The *TP* size is fixed at 8, so it must still match the accelerators per member.
See [Rollout behavior](/gpustack-operator/v0.9.2/docs/modules/model-deployment/deployment/index.md#rollout-behavior).

## Step 5: Release the application

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

## Limitations

Gang scheduling is all-or-nothing: all *`size`* members of an instance need placement at once, or the
whole instance waits.

HTTP ingress is leader-only under *internal load balancing (Internal LB)*: the `Service` targets
member `m0`, and subordinate members expose no serving endpoint.

Changing *`size`* replaces whole replicas rather than resizing a running collective. With one replica,
the role stops serving while its replacement is rebuilt.

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| `ModelDeployment` stuck in `Starting`, Kueue composes no `Workload` | `QuotaReserved=False/PodGroupIncomplete` | Fewer instances exist than the role declares. Read the message for the counts, then check the `Pod`s that are missing or unschedulable |
| An instance was lost and is being rebuilt | `ReplicasUpToDate=False/ReplacementInProgress` | A replica disappeared for a reason outside the spec: a drained node, Kueue reclaiming quota, an eviction or a manual delete. The `Pod`s of an instance share one lifecycle, so the whole instance is rebuilt. Check the node and Kueue events |
| Replicas wait for quota | `QuotaReserved=False/Pending` | The `ClusterQueue` has insufficient capacity to admit the replica. Free capacity, raise the pool quota, or reduce per-member accelerator requests |
| Secondary worker cannot reach the leader | Engine logs on `m1` | Check that the headless `Service` resolves `GPUSTACK_REPLICA_LEADER_ADDRESS` and that no `NetworkPolicy` blocks the engine's distributed port |
| Engine errors on parallel size | Engine startup log | Check that `--tensor-parallel-size` matches the accelerators per member (*TP* size) and that `--pipeline-parallel-size` equals *`size`* (*PP* size) |

---

**See also** — [Model Deployment Configuration](/gpustack-operator/v0.9.2/docs/modules/model-deployment/deployment/index.md) (role fields and scaling) ·
[Prefill and Decode Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/prefill-decode/index.md) (disaggregated serving) ·
[Elastic EP Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/elastic-ep/index.md) (collective expansion) ·
[External DP Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/external-dp/index.md) (fixed per-rank serving)

**Next** → [Model Deployment Status](/gpustack-operator/v0.9.2/docs/modules/model-deployment/status/index.md) — observe status conditions and phases.
