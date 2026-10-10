# Model Deployment Walkthroughs

Choose a serving shape, prepare its model, then follow the matching walkthrough.
The Elastic EP and External DP examples use the official GPUStack runner and retain captured CR output and inference results.
The Prefill and Decode and the Multi-Host pages are configuration examples. They record no captured run.

## Contents

- [Choose an example](#choose-an-example)
- [Prepare the cluster](#prepare-the-cluster)
- [Prepare the GPU InstanceType](#prepare-the-gpu-instancetype)
- [Check the software identity](#check-the-software-identity)

## Choose an example

| Walkthrough | Serving shape | What it covers |
|---|---|---|
| [Prefill and Decode](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/prefill-decode/index.md) | Disaggregated roles fronted by router | Not measured here. Separate Prefill and Decode roles, `llm-d-router` routing, joint admission, and KV cache transfer. |
| [Elastic EP](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/elastic-ep/index.md) | One Internal LB instance on Ray | TP2, DP width 2→4, EP world 4→8; ordinary and streaming requests before and after expansion. |
| [External DP](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/external-dp/index.md) | Two fixed DP ranks using multiprocessing | TP2, DP2, EP world4; direct requests to each rank and Router distribution across both ranks. |
| [Multi-Host](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/multi-host/index.md) | Coordinated instance spanning multiple hosts | Not measured here. `size: 2`, sixteen GPUs across two physical nodes, leader `m0` routing, and gang admission. |

A Router can front either shape. The engine's LB mode determines which Pods accept inference requests.
Increasing `roles[].replicas` adds complete groups; see [role scaling](/gpustack-operator/v0.9.0/docs/modules/model-deployment/deployment/index.md#rollout-behavior).
That operation was not measured in these walkthroughs.

## Prepare the cluster

Install the operator and enable node model delivery using the
[installation guide](/gpustack-operator/v0.9.0/docs/operate/installation-modes/index.md) and
[model-store operations](/gpustack-operator/v0.9.0/docs/modules/model-delivery/operations/index.md).
Use `kubectl`, `jq`, `curl`, and a kubeconfig for the intended cluster.
The recorded cluster had one CPU node and one node with eight NVIDIA RTX PRO 6000 Blackwell GPUs.

```bash
kubectl get nodes
kubectl get devices -o json
kubectl get instancetypes
kubectl get nodemodelstores -o json
```

Confirm model-manager readiness and free GPUs before creating a serving workload.
External DP needs four free cards. Elastic EP needs four initially and eight after expansion.
Complete the [model prefetch walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/model-delivery/prefetch/index.md) before starting either example.

## Prepare the GPU InstanceType

Save the following as `gpu-instance-type.yaml` and apply it once.
For another GPU family, select an appropriate whole-GPU InstanceType and verify model compatibility.
Each serving member requests two GPUs; CPU and memory budgets scale with that count.

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

All four examples refer to `elastic-cpu-gpu`. Replace that name if you use an existing InstanceType.

## Check the software identity

Read the running images and binary versions. Repeat for the worker, device-manager, and model-manager Pods.
Set the namespace, Pod, and container names from your installation:

```bash
kubectl get pods -A
kubectl -n "$OPERATOR_NAMESPACE" get pod "$OPERATOR_POD" -o json |
  jq '.status.containerStatuses[] | {name, image, imageID}'
kubectl -n "$OPERATOR_NAMESPACE" exec "$OPERATOR_POD" -c "$OPERATOR_CONTAINER" -- \
  gpustack-operator --version
```

The `dev` tag can move. Each walkthrough's validation record identifies the binary and image used.
The serving manifests name the runner by tag. Validation records list the observed image tags or digests.
No engine packages were replaced during validation.

---

**See also** — [Model Deployment](/gpustack-operator/v0.9.0/docs/modules/model-deployment/index.md) (configuration guides) ·
[Model Prefetch Walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/model-delivery/prefetch/index.md)

**Next** → [Prefill and Decode Walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/model-deployment/prefill-decode/index.md) — disaggregated serving with router.
