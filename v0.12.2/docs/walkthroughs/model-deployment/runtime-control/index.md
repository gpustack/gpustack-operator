# Online Runtime Control Walkthrough

Tune live vLLM and SGLang serving parameters, pause and resume roles safely beside an active scaler,
and verify restart replay and Elastic EP expansion without restarting worker `Pod`s.

## Contents

- [Summary](#summary)
- [vLLM runtime changes](#vllm-runtime-changes)
- [SGLang runtime changes](#sglang-runtime-changes)
- [Verification](#verification)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

`ModelDeployment` mounts `gpustack-engine-sentinel` read-only via `PYTHONPATH` on managed accelerated
non-*Encode* roles without API-key `Secret`s. Managed routers enforce an Envoy inference allowlist on
port `8000` and serve `/observer/endpoints` on pod-internal port `9091`:

| Capability | Engine and router | Verified behavior |
|---|---|---|
| Envoy allowlist and router hash decoupling | `vLLM 0.32.0` + `vllm-router`, `SGLang 0.5.22` + `sglang-gateway` | `200` on `/v1/chat/completions`, `403` on control routes; editing *`requestTimeoutSeconds`* rolls only the router |
| Live parameter tuning and auto-reset | `vLLM 0.32.0`, `SGLang 0.5.22` (including NGRAM speculative) | Applies live via `/gpustack/v1/runtime_config`; removing keys restores `boot_defaults` with zero `Pod` restarts |
| Version-gated unsupported parameters | `vLLM 0.32.0`, `SGLang 0.5.22` | Reports `UnsupportedEngineVersion` (`unsupportedFields`); removing keys recovers `Synced` |
| Role pause and scaler guard | `vLLM 0.32.0` + `Active` scaler, `SGLang 0.5.22`, `1P+1D` `llm-d-router` | `/live=200`, `/ready=503`, direct inference `503`, scaler `Blocked=True (RolePaused)`; resume restores `200` |
| Same-UID restart latch and Elastic EP resize | `vLLM 0.32.0` (`Qwen3-8B`, `DeepSeek-V2-Lite-Chat`) | Container restart rotates `boot_id` and replays parameters before readiness; Elastic EP grows `2 → 3` keeping master `Pod` UID |

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes `v1.35.7`; 1 CPU node (`cpu-d3`) and 3 GPU nodes (`gpu-h200-sxm`, 1 GPU per node) |
| Hardware | 3 × NVIDIA H200 SXM 141 GB (`gpustack--nvidia-h200-linux-amd64`) |
| Runners | `gpustack/runner:cuda13.0-vllm0.32.0`, `gpustack/runner:cuda13.0-sglang0.5.22` |
| Models | `Qwen/Qwen3-8B` (`ModelArtifact` `qwen3-8b`), `deepseek-ai/DeepSeek-V2-Lite-Chat` (`deepseek-v2-lite-chat`) |
| Routers | `vllm-router`, `sglang-gateway`, `llm-d-router` (`gpustack/llm-router:v0.5.0`) |

Enable [node model delivery](/gpustack-operator/v0.12.2/docs/modules/model-delivery/operations/index.md) and complete
[Model Prefetch](/gpustack-operator/v0.12.2/docs/walkthroughs/model-delivery/prefetch/index.md) for `qwen3-8b` and `deepseek-v2-lite-chat`, then check
cluster capacity:

```bash
kubectl get nodes
kubectl get acceleratortypes
kubectl get nodemodelstores
```

Captured output:

```text
NAME                                 STATUS   ROLES    AGE    VERSION
computeinstance-e01ah0pbdcbn4pngh5   Ready    <none>   143m   v1.35.7
computeinstance-e01dcjfh8cjnywrtf2   Ready    <none>   143m   v1.35.7
computeinstance-e01j5ns0wkmc376ygw   Ready    <none>   144m   v1.35.7
computeinstance-e01nd7j1ezqp5jhwh3   Ready    <none>   143m   v1.35.7

NAME                                ENTRANCE                          ACCELERATOR(EX/SH/SL/PT)   PHASE
gpustack--nvidia-h200-linux-amd64   gpustack-fnv64-1bdcbb48b860c5ed   1/3 1/30 100/300 0/0       Active
```

## vLLM runtime changes

### Step 1: Deploy routed vLLM

Save `vllm-runtime-control.yaml` and wait for `qwen-vllm-rc` to reach `Ready`:

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

Record the initial worker `Pod` UID, patch *`spec.router.requestTimeoutSeconds`* to `90`, and verify
router hash decoupling:

```bash
BEFORE_UID=$(kubectl -n default get pod -l app.kubernetes.io/instance=qwen-vllm-rc,app.kubernetes.io/component=server \
  -o jsonpath='{.items[0].metadata.uid}')
kubectl -n default patch modeldeployment qwen-vllm-rc --type=merge \
  -p '{"spec":{"router":{"requestTimeoutSeconds":90}}}'
AFTER_UID=$(kubectl -n default get pod -l app.kubernetes.io/instance=qwen-vllm-rc,app.kubernetes.io/component=server \
  -o jsonpath='{.items[0].metadata.uid}')
test "$BEFORE_UID" = "$AFTER_UID" && echo "worker UID unchanged"
```

Expected behavior:

- Zero runtime-control `Secret`s are created in `default`.
- Router port `:8000` returns `200` on `POST /v1/chat/completions` and `403` on `/gpustack/v1/ready`,
  `/gpustack/v1/runtime_config`, `/gpustack/v1/pause`, `/scale_elastic_ep`, and `/observer/endpoints`,
  while pod-internal `:9091/observer/endpoints` returns `200`.
- Updating *`requestTimeoutSeconds`* from `60` to `90` rolls only the router `Deployment`, keeping
  the worker `Pod` UID identical with `restartCount: 0`.

### Step 2: Tune and reset vLLM

Patch *`spec.roles[0].runtimeControl.parameters`* live and inspect `RuntimeControlSynced`:

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

Expected convergence, parameter reset, and version-gating behavior:

- `RuntimeControlSynced` converges to `True` (`Reason: Synced`) with a non-empty *`appliedConfigDigest`*
  and zero worker restarts.
- Removing `scheduling.longPrefillTokenThreshold` resets only that parameter to its `boot_defaults`
  value while keeping `maxActiveRequests=24` and `maxBatchedTokens=4096` active under a new config
  digest.
- Setting version-gated keys absent from `vllm0.32.0` (`scheduling.cacheAwareWindow` and
  `scheduling.cacheAwareThreshold`, or `scheduling.maxInflightKVLoads` and
  `scheduling.maxAsyncKVLoadsPerStep` on a `1P+1D` `decode` role) reports
  `RuntimeControlSynced=False` (`Reason: UnsupportedEngineVersion`) with *`unsupportedFields`*
  populated and zero `Pod` restarts. Removing the unsupported keys restores `Synced` on the same
  worker `Pod` UID.

### Step 3: Pause scaled role

With an `Active` `ModelDeploymentScaler` bound to `qwen-vllm-rc`, pause the `server` role:

```bash
kubectl -n default patch modeldeployment qwen-vllm-rc --type=json \
  -p '[{"op":"add","path":"/spec/roles/0/pause","value":true}]'
kubectl -n default get modeldeploymentscaler \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Blocked")]}{"\n"}{end}'
```

Expected pause, resume, and drain behavior:

- The operator removes `modeldeployment.gpustack.ai/endpoint-eligible`, drains active requests, and
  sets sentinel `paused: true` (`Reason: Synced`).
- Worker `/gpustack/v1/live` returns `200`, while `/gpustack/v1/ready` and direct
  `POST /v1/chat/completions` return `503`.
- The bound `Active` `ModelDeploymentScaler` sets `Blocked=True` (`Reason: RolePaused`) and suspends
  replica writes.
- On a `1P+1D` deployment behind `llm-d-router`, pausing `decode` turns only `decode` `/ready` to
  `503` (`prefill` stays `200`) and evicts only `decode` from `/observer/endpoints`
  (`registry_revision` `3 → 6`).
- Setting `pause: false` and removing *`runtimeControl`* restores `boot_defaults`, removes the role
  ledger `ConfigMap`, sets `RuntimeControlSynced=True` (`Reason: NotConfigured`), and restores router
  `200` responses.
- Deleting the worker `Pod` executes `python3 -m gpustack_engine_sentinel.drain` with zero
  `FailedPreStopHook` events.

## SGLang runtime changes

SGLang parameter tuning uses a separate `SGLang 0.5.22` deployment (`qwen-sglang-rc` behind
`sglang-gateway`), distinct from `qwen-vllm-rc` in Steps 1–3. Start from a `Ready` `ModelDeployment`
configured with `spec.engine.name: SGLang` and a compatible `spec.engine.image` (substituting your
deployment name if different), then patch all five scheduling parameters live:

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

Captured output (`/gpustack/v1/runtime_config` readback from `wt-sglang-server-7wwdr`):

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

On a separate `SGLang 0.5.22` deployment launched with `--speculative-algorithm=NGRAM`,
`speculative.acceptThresholdSingle: "0.85"` and `speculative.acceptThresholdAcc: "0.75"` applied live
with zero restarts, whereas `speculative.draftBudgetFraction` (DFlash/DSpark only) reported
`UnsupportedEngineVersion` and recovered cleanly when removed.

## Verification

Verify `ModelDeployment` sentinel replay across container restarts and Elastic EP width changes:

- **Same-UID container restart latch replay**: killing vLLM's `EngineCore` process triggered an
  in-place container restart (`restartCount: 0 → 1`) on the same `Pod` UID. The restarted sentinel
  generated a fresh `boot_id`, held `/gpustack/v1/ready` fail-closed until the controller replayed
  `maxActiveRequests=24` and `maxBatchedTokens=4096` and sent `/gpustack/v1/ack`, and only then
  returned `200` on `/gpustack/v1/ready`.
- **Folded Elastic EP expansion (`DeepSeek-V2-Lite-Chat`)**: widening
  `runtimeControl.parameters["elastic.dataParallelWidth"]` from `"2"` to `"3"` preserved the master
  `Pod` UID, converged `ElasticResize=True` (`Reason: NoRefusal`), advanced `/gpustack/v1/runtime_config`
  to `ranks=3` and `membership_epoch=2`, and replayed `scheduling.maxActiveRequests=16` across all
  three ranks.

## Limitations

- `scheduling.ppMaxMicroBatchSize` requires `--pp-size > 1` with `resources.accelerator >= 2` per
  member and was admission-tested only on this 1-GPU-per-node H200 cluster.
- Parameters absent from `vllm0.32.0` (`cacheAware*`, `maxInflightKVLoads`, `maxAsyncKVLoadsPerStep`)
  and `sglang0.5.22` NGRAM (`draftBudgetFraction`) were verified through the `UnsupportedEngineVersion`
  rejection and recovery path.
- Behavior on other engines or hardware follows
  [Online Runtime Control](/gpustack-operator/v0.12.2/docs/modules/model-deployment/runtime-control/index.md).

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `RuntimeControlSynced=False` | `Reason: UnsupportedEngineVersion` | Inspect *`status.roles[].runtimeControl.unsupportedFields`* on `ModelDeployment`, then remove those keys or upgrade the runner image. |
| Control routes return `403` on router `:8000` | Envoy inference allowlist active | Expected security boundary; query `/observer/endpoints` on the router pod's internal port (`:9091` or `:9090`) rather than `:8000`. |
| `ModelDeploymentScaler` stops scaling | `Blocked=True` (`Reason: RolePaused`) | Expected while any target role sets `pause: true`; set `pause: false` on `ModelDeployment` to resume autoscaling. |
| Restarted container stays unready briefly | `/gpustack/v1/ready` returns `503` after container restart | Sentinel holds readiness fail-closed on a new `boot_id` until the worker replays *`runtimeControl.parameters`* and `/gpustack/v1/ack`; check worker connectivity if it does not clear. |
| Router fails to render | `RouterReady=False/RenderFailed` | No Router `Pod` exists when rendering fails; inspect `RouterReady` on `ModelDeployment` and fix *`spec.router`*. |

---

**See also** — [Online Runtime Control](/gpustack-operator/v0.12.2/docs/modules/model-deployment/runtime-control/index.md) (parameter
catalog and status reasons) · [Elastic EP Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/elastic-ep/index.md) (eight-GPU H100 collective
expansion)

**Next** → [Multi-Host Serving Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/multi-host/index.md) — span a serving group across multiple GPU hosts.
