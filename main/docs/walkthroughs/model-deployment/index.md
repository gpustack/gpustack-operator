# Model Deployment Walkthroughs

Choose a serving configuration, then use its cluster profile and deployment manifest.
The H100 walkthroughs distinguish recorded deployments from configuration examples.
The illustrative *Multi-Host* configuration uses the separate `InstanceType` example below.

## Contents

- [Choose an example](#choose-an-example)
- [Prepare the cluster](#prepare-the-cluster)
- [Prepare the GPU InstanceType](#prepare-the-gpu-instancetype)

## Choose an example

*Tensor parallelism (TP)*, *data parallelism (DP)* and *expert parallelism (EP)* describe the parallel groups below.
The *DP* width counts data-parallel ranks; the *EP* world size counts GPU workers in the expert-parallel collective.
With *internal load balancing (Internal LB)*, the master distributes requests to its *DP* ranks.

| Walkthrough | Serving configuration | What it covers |
|---|---|---|
| [Prefill and Decode](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) | Disaggregated roles fronted by Router | Recorded H100 deployment with one *Prefill* and two *Decode* replicas, *TP* = 2, TCP KV transfer, and a linked [benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md). |
| [Encode, Prefill and Decode](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) | Disaggregated multimodal roles fronted by Router | Recorded H100 deployment with one *Encode*, one *Prefill* and one *Decode* replica, NIXL encoder transfer, and a linked [benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md). |
| [Elastic EP](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) | One *Internal LB* instance on Ray | Recorded H100 expansion: *TP* = 2, *DP* width 2 → 4, *EP* world size 4 → 8; 439 client requests with zero errors. |
| [External DP](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) | Four fixed *DP* ranks using multiprocessing | Recorded H100 deployment: *TP* = 2, *DP* = 4, *EP* world size 8; captured per-rank inference and Router coverage. |
| [Multi-Host](/gpustack-operator/main/docs/walkthroughs/model-deployment/multi-host/index.md) | Coordinated instance spanning multiple hosts | Not measured here. `size: 2`, sixteen GPUs across two physical nodes, leader `m0` routing, and gang admission. |

A Router can front the *Internal LB* and *External LB* configurations.
The engine's *load-balancing* mode determines which `Pod`s accept inference requests.
Increasing *`roles[].replicas`* adds complete groups; see [role scaling](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#rollout-behavior).
That operation was not measured in these walkthroughs.

## Prepare the cluster

Install the operator and enable node model delivery using the
[installation guide](/gpustack-operator/main/docs/operate/installation-modes/index.md) and
[model-store operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md).
Use `kubectl`, `jq`, `curl`, and a kubeconfig for the intended cluster.

The H100 recordings use one CPU node and one GPU node with eight NVIDIA H100 80 GB HBM3 SXM5 GPUs.
Read each walkthrough's device and cluster section for its allocation and evidence limits.
The *Multi-Host* example instead needs two GPU nodes.

```bash
kubectl get nodes
kubectl get devices -o json
kubectl get instancetypes
kubectl get nodemodelstores -o json
```

Confirm Model Manager readiness and free GPUs before creating a serving workload.
*External DP* needs eight free cards. *Elastic EP* needs four initially and eight after expansion.
Complete [model prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) with the artifact, namespace and node selection
specified by the serving walkthrough.

## Prepare the GPU InstanceType

The H100 manifests use the discovered whole-GPU type
`gpustack--nvidia-h100-80gb-hbm3-linux-amd64`. The recordings used the custom type `bench-gpu`.
Each walkthrough explains this substitution; captured queue output retains the recorded name.
Confirm the selected type's capacity before applying a manifest.

For the illustrative *Multi-Host* configuration, save this custom type as `gpu-instance-type.yaml`.
Apply it only on nodes with the matching RTX PRO accelerator group.
Select another whole-GPU type when using different hardware, and verify model compatibility.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: elastic-cpu-gpu
spec:
  generalGroup: generic
  acceleratable: true
  acceleratorGroup: nvidia-rtx-pro-6000-blackwell-server-edition
  os: linux
  arch: amd64
  localStorage: 10Gi
  unitResources:
    cpu: "4"
    ram: 16Gi
```

```bash
kubectl apply -f gpu-instance-type.yaml
kubectl get instancetype elastic-cpu-gpu
```

[Multi-Host](/gpustack-operator/main/docs/walkthroughs/model-deployment/multi-host/index.md) uses `elastic-cpu-gpu` as an illustrative custom `InstanceType`.
It is separate from the H100 type used by the other reusable manifests.

---

**See also** — [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) (configuration guides) ·
[Model Prefetch Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md)

**Next** → [Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) — disaggregated serving with router.
