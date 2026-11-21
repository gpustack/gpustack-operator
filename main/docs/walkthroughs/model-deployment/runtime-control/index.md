# Online Runtime Control Walkthrough

Tune live vLLM and SGLang serving parameters, pause individual roles, and resize Elastic EP on NVIDIA H200 GPUs without restarting worker `Pod`s.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Deploy routed vLLM](#step-1-deploy-routed-vllm)
- [Step 2: Tune and reset vLLM](#step-2-tune-and-reset-vllm)
- [Step 3: Pause with a scaler](#step-3-pause-with-a-scaler)
- [Step 4: Tune SGLang parameters](#step-4-tune-sglang-parameters)
- [Step 5: Verify restart and resize](#step-5-verify-restart-and-resize)
- [Limitations](#limitations)

## Summary

`ModelDeployment` injects `gpustack-engine-sentinel` into managed accelerated non-*Encode* roles through a read-only `PYTHONPATH` mount.
There are no operator-managed API keys or per-deployment auth `Secret`s; managed routers enforce a shared Envoy inference allowlist on port `8000` and expose `/observer/endpoints` internally.

| Capability | Engine and router | H200 outcome |
|---|---|---|
| Envoy allowlist and router hash decoupling | `vLLM 0.32.0` + `vllm-router`, `SGLang 0.5.22` + `sglang-gateway` | `200` on `/v1/chat/completions`, `403` on control routes; `requestTimeoutSeconds` rolled router only (worker UID unchanged) |
| Live parameter tuning and auto-reset | `vLLM 0.32.0`, `SGLang 0.5.22` (including NGRAM speculative) | Applied live via `/gpustack/v1/runtime_config`; key removal restored `boot_defaults` with zero `Pod` restart |
| Version-gated unsupported parameters | `vLLM 0.32.0`, `SGLang 0.5.22` | Reported `UnsupportedEngineVersion` with `unsupportedFields`; removal recovered `Synced` without restart |
| Role pause and `ModelDeploymentScaler` guard | `vLLM 0.32.0` + Active scaler, `SGLang 0.5.22`, `1P+1D` `llm-d-router` | `/live=200`, `/ready=503`, direct inference `503`, scaler `Blocked=True (RolePaused)`; resume restored `200` |
| Same-UID restart latch and Elastic EP resize | `vLLM 0.32.0` (`Qwen3-8B`, `DeepSeek-V2-Lite-Chat`) | Container restart rotated `boot_id` and replayed digest; Elastic EP grew `2 -> 3` while keeping master UID |

## Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes `v1.35.7`; 1 CPU node (`cpu-d3`) and 3 GPU nodes (`gpu-h200-sxm`, 1 GPU per node) |
| Hardware | 3 × NVIDIA H200 SXM 141 GB (`gpustack--nvidia-h200-linux-amd64`) |
| Runners | `gpustack/runner:cuda13.0-vllm0.32.0`, `gpustack/runner:cuda13.0-sglang0.5.22` |
| Models | `Qwen/Qwen3-8B` (`ModelArtifact` `qwen3-8b`), `deepseek-ai/DeepSeek-V2-Lite-Chat` (`deepseek-v2-lite-chat`) |
| Routers | `vllm-router`, `sglang-gateway`, `llm-d-router` (`gpustack/llm-router:v0.5.0`) |

Enable [node model delivery](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md) and complete [model prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) for `qwen3-8b` and `deepseek-v2-lite-chat` before running these steps.
Confirm the cluster profile before deploying:

```bash
kubectl get nodes
kubectl get acceleratortypes
kubectl get nodemodelstores
```

Captured cluster snapshot:

```text
NAME                                 STATUS   ROLES    AGE    VERSION
computeinstance-e01ah0pbdcbn4pngh5   Ready    <none>   143m   v1.35.7
computeinstance-e01dcjfh8cjnywrtf2   Ready    <none>   143m   v1.35.7
computeinstance-e01j5ns0wkmc376ygw   Ready    <none>   144m   v1.35.7
computeinstance-e01nd7j1ezqp5jhwh3   Ready    <none>   143m   v1.35.7

NAME                                ENTRANCE                          ACCELERATOR(EX/SH/SL/PT)   PHASE
gpustack--nvidia-h200-linux-amd64   gpustack-fnv64-1bdcbb48b860c5ed   1/3 1/30 100/300 0/0       Active
```

## Step 1: Deploy routed vLLM

Save the manifest as `vllm-runtime-control.yaml` and apply it:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-vllm-rc
  namespace: default
spec:
  model:
    name: Qwen/Qwen3-8B
    artifactRef:
      name: qwen3-8b
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.32.0
  router:
    name: vllm-router
    requestTimeoutSeconds: 60
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: gpustack--nvidia-h200-linux-amd64
      resources:
        accelerator: "1"
      extraArgs:
        - --max-model-len=4096
        - --max-num-seqs=64
        - --max-num-batched-tokens=8192
        - --gpu-memory-utilization=0.80
```

```bash
kubectl apply -f vllm-runtime-control.yaml
kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready modeldeployment/qwen-vllm-rc --timeout=600s
```

Verify the default sentinel injection, zero runtime-control `Secret`s, and Envoy allowlist:

```bash
kubectl -n default get secrets | grep runtime-control || echo "zero runtime-control secrets"
kubectl -n default get pod -l app.kubernetes.io/instance=qwen-vllm-rc,app.kubernetes.io/component=server \
  -o jsonpath='{.items[0].metadata.uid}{"\n"}'
```

On the live H200 run (`c134-13614-vllm`, worker UID `88e10b33-0b25-4541-8f37-38066677f982`):

- `POST http://<router-service>:8000/v1/chat/completions` returned `200`.
- `GET`/`POST` to `/gpustack/v1/ready`, `/gpustack/v1/runtime_config`, `/gpustack/v1/pause`, `/scale_elastic_ep`, and `/observer/endpoints` on port `8000` all returned `403`.
- Pod-internal `GET http://<router-pod-ip>:9091/observer/endpoints` returned `200`.
- Patching *`spec.router.requestTimeoutSeconds`* from `60` to `90` rolled only the router `Deployment`, leaving worker UID `88e10b33-0b25-4541-8f37-38066677f982` unchanged with `restartCount: 0`.

## Step 2: Tune and reset vLLM

Patch *`spec.roles[0].runtimeControl.parameters`* to lower the active request ceiling, batched tokens, and long-prefill threshold:

```bash
kubectl -n default patch modeldeployment qwen-vllm-rc --type=json -p '[
  {"op":"add","path":"/spec/roles/0/runtimeControl","value":{"parameters":{
    "scheduling.maxActiveRequests":"24",
    "scheduling.maxBatchedTokens":"4096",
    "scheduling.longPrefillTokenThreshold":"1024"
  }}}
]'
kubectl -n default get modeldeployment qwen-vllm-rc \
  -o jsonpath='{.status.conditions[?(@.type=="RuntimeControlSynced")]}{"\n"}{.status.roles[0].runtimeControl}{"\n"}'
```

Observed behavior on `vLLM 0.32.0`:

- `RuntimeControlSynced` converged to `True` (`Reason: Synced`) with `appliedConfigDigest: sha256:9f5cad1b...` and zero worker restart.
- Removing `scheduling.longPrefillTokenThreshold` restored only that field to its `boot_defaults` value while preserving `maxActiveRequests=24` and `maxBatchedTokens=4096` (`appliedConfigDigest: sha256:e5b20f38...`).
- Adding `scheduling.cacheAwareWindow` and `scheduling.cacheAwareThreshold` (absent from `vllm0.32.0`) reported `RuntimeControlSynced=False` (`Reason: UnsupportedEngineVersion`, `unsupportedFields: [scheduling.cacheAwareThreshold, scheduling.cacheAwareWindow]`) without restarting the `Pod`. Removing those keys recovered `True` (`Reason: Synced`) with the same worker UID.
- On a `1P+1D` `vLLM 0.32.0` deployment (`llm-d-router`), setting `scheduling.maxInflightKVLoads` and `scheduling.maxAsyncKVLoadsPerStep` on `decode` similarly reported `UnsupportedEngineVersion` and recovered cleanly on removal.

## Step 3: Pause with a scaler

Attach an Active `ModelDeploymentScaler` targeting `qwen-vllm-rc` and set *`spec.roles[0].pause: true`*:

```bash
kubectl -n default patch modeldeployment qwen-vllm-rc --type=json \
  -p '[{"op":"add","path":"/spec/roles/0/pause","value":true}]'
kubectl -n default get modeldeploymentscaler \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Blocked")]}{"\n"}{end}'
```

Observed pause and resume lifecycle:

- The operator withdrew `modeldeployment.gpustack.ai/endpoint-eligible`, drained in-flight traffic, and toggled sentinel pause mode (`paused: true`, `Reason: Synced`).
- Worker `/gpustack/v1/live` remained `200`, while `/gpustack/v1/ready` and direct `POST /v1/chat/completions` returned `503`.
- The Active `ModelDeploymentScaler` reported `Blocked=True` (`Reason: RolePaused`) and withheld replica writes.
- On a `1P+1D` `vLLM` deployment behind `llm-d-router`, pausing `decode` returned `decode /ready=503` while `prefill /ready` stayed `200`, and `:9090/observer/endpoints` removed only `decode` (`registry_revision` advanced `3 -> 6`).
- Clearing `pause: false` and removing `runtimeControl` restored all parameters to `boot_defaults`, deleted the role ledger `ConfigMap`, returned `RuntimeControlSynced=True` (`Reason: NotConfigured`), and restored router inference `200`.
- Deleting the worker `Pod` ran `python3 -m gpustack_engine_sentinel.drain` cleanly with zero `FailedPreStopHook` events.

## Step 4: Tune SGLang parameters

Deploy `SGLang 0.5.22` behind `sglang-gateway` and tune its five core scheduling knobs:

```bash
kubectl -n default patch modeldeployment qwen-sglang-rc --type=json -p '[
  {"op":"add","path":"/spec/roles/0/runtimeControl","value":{"parameters":{
    "scheduling.maxActiveRequests":"24",
    "scheduling.maxBatchedTokens":"2048",
    "scheduling.prefillMaxRequests":"12",
    "scheduling.scheduleConservativeness":"1.2",
    "scheduling.prefillDecodeInterval":"2"
  }}}
]'
```

Captured `/gpustack/v1/runtime_config` readback from `wt-sglang-server-7wwdr` (`gpustack/runner:cuda13.0-sglang0.5.22`):

```json
{
  "engine": "sglang",
  "boot_id": "ccf7515e-d667-49c5-ae79-ff88367160b3",
  "observed_generation": 2,
  "applied_config_digest": "sha256:2cb017f63c6377dc1c09d7bc8a9634b170067cde11fae3898a281e9dda41b25c",
  "ready": true,
  "paused": false,
  "boot_defaults": {
    "scheduling.maxActiveRequests": 64,
    "scheduling.maxBatchedTokens": 4096,
    "scheduling.prefillMaxRequests": null,
    "scheduling.scheduleConservativeness": 1.0,
    "scheduling.prefillDecodeInterval": 0
  },
  "values": {
    "scheduling.maxActiveRequests": 24,
    "scheduling.maxBatchedTokens": 2048,
    "scheduling.prefillMaxRequests": 12,
    "scheduling.scheduleConservativeness": 1.2,
    "scheduling.prefillDecodeInterval": 2
  }
}
```

On a separate `SGLang 0.5.22` deployment with `--speculative-algorithm=NGRAM`:

- `speculative.acceptThresholdSingle: "0.85"` and `speculative.acceptThresholdAcc: "0.75"` applied live and read back from `/gpustack/v1/runtime_config` without restarting the `Pod`.
- `speculative.draftBudgetFraction` (requiring DFlash or DSpark) cleanly reported `UnsupportedEngineVersion` and recovered on removal.

## Step 5: Verify restart and resize

Two lifecycle edge cases were verified on the same H200 cluster:

- **Same-UID container restart latch replay (`wt-latch-replay`)**: Killing vLLM's `EngineCore` process (`pid=160`) triggered an in-place restart (`restartCount: 0 -> 1`) on the same `Pod` UID (`426fc922-49ab-403c-8234-f3eb7f3c161e`). The restarted sentinel minted a fresh `boot_id` (`1f4bed32-...` → `bb55ae70-...`), stayed fail-closed until the controller replayed `appliedConfigDigest` (`maxActiveRequests=24`, `maxBatchedTokens=4096`) and sent `/gpustack/v1/ack`, then returned `/gpustack/v1/ready=200`.
- **Folded Elastic EP expansion (`DeepSeek-V2-Lite-Chat`)**: Expanding `runtimeControl.parameters["elastic.dataParallelWidth"]` from `"2"` to `"3"` preserved master `Pod` UID `df3459d5-9a45-41d7-95e4-be12cd5ed7ed`, converged `ElasticResize=True` (`Reason: NoRefusal`), advanced `/gpustack/v1/runtime_config` to `ranks=3`, `membership_epoch=2`, and replayed `scheduling.maxActiveRequests=16` across all three ranks.

## Limitations

- `scheduling.ppMaxMicroBatchSize` requires `--pp-size > 1` with at least two GPUs per member on a single node (`resources.accelerator >= 2`); it was admission-checked only on the 1-GPU-per-node H200 cluster.
- Version-gated parameters absent from `vllm0.32.0` (`cacheAware*`, `maxInflightKVLoads`, `maxAsyncKVLoadsPerStep`) and `sglang0.5.22` NGRAM (`draftBudgetFraction`) were verified through the `UnsupportedEngineVersion` contract rather than live mutation.
- This recording covers NVIDIA H200 GPUs with `vLLM 0.32.0` and `SGLang 0.5.22`; behavior on other accelerator vendors or engine builds follows the parameter contract in [Online Runtime Control](/gpustack-operator/main/docs/modules/model-deployment/runtime-control/index.md).

---

**See also** — [Online Runtime Control](/gpustack-operator/main/docs/modules/model-deployment/runtime-control/index.md) (parameter contract and status reasons) ·
[Elastic EP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) (recorded eight-GPU H100 expansion)

**Next** → [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md)
