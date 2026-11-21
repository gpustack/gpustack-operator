# Online Runtime Control

A `ModelDeployment` can tune engine parameters live, pause individual roles, and resize Elastic EP collectives without restarting worker `Pod`s or altering `--load-format`.

See [Online Runtime Control Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/runtime-control/index.md) for a recorded NVIDIA H200 deployment covering vLLM and SGLang live tuning, role pause, and Elastic EP resizing.

![Online runtime control architecture and isolation flow](/gpustack-operator/main/docs/assets/diagrams/model-deployment-runtime-control.svg)

<!--
flowchart LR
    SPEC["ModelDeployment Spec: runtimeControl.parameters, pause, extraArgs"] -> RECON["Operator Reconciler: router gate, ledger, drain, Elastic EP, RolePaused"]
    RECON -> ENVOY["Envoy Inference Allowlist: :8000 allowlist, :9091 observer"]
    RECON -> SENTINEL["gpustack-engine-sentinel: /ready, /ack, /runtime_config, /pause, drain"]
    SENTINEL -> SPEC
-->

## Contents

- [Configuration](#configuration)
- [Supported parameters](#supported-parameters)
- [Isolation and safety](#isolation-and-safety)
- [Step 1: Configure boot ceilings](#step-1-configure-boot-ceilings)
- [Step 2: Tune or pause roles](#step-2-tune-or-pause-roles)
- [Step 3: Verify convergence status](#step-3-verify-convergence-status)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Configuration

Declare static boot ceilings in *`spec.roles[].extraArgs`*, live-tunable parameters in *`spec.roles[].runtimeControl.parameters`*, and role traffic state in *`spec.roles[].pause`*.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-tuned
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-72B-Instruct
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.32.0
  router:
    name: vllm-router
  roles:
    - name: main
      kind: Server
      replicas: 2
      pause: false  # optional; excluded from pod-spec-hash
      acceleratorType: gpustack--nvidia-h20-linux-amd64
      resources:
        accelerator: 1
      extraArgs:  # optional; boot ceilings stay in extraArgs and roll Pods on edit
        - --max-num-seqs=64
        - --max-num-batched-tokens=8192
      runtimeControl:  # optional; excluded from pod-spec-hash, reconciled live over HTTP
        parameters:
          scheduling.maxActiveRequests: "32"
          scheduling.maxBatchedTokens: "4096"
          scheduling.longPrefillTokenThreshold: "2048"
          scheduling.cacheAwareWindow: "128"
          scheduling.cacheAwareThreshold: "0.5"
```

- **Boot ceilings**: Set static startup limits in *`roles[].extraArgs`*; changing *`extraArgs`* rolls affected worker `Pod`s.
- **Live parameters**: Set canonical string pairs in *`roles[].runtimeControl.parameters`* without changing `pod-spec-hash`.
- **Role pause**: Set *`roles[].pause: true`* on any role to withdraw eligibility, drain requests, and gate arrivals.
- **Reserved LoRA adapters**: Leave *`spec.model.loraAdapters`* unset; non-empty entries fail admission pending `ModelArtifact` LoRA support.
- **Host sentinel package**: Managed accelerated non-*Encode* roles mount read-only `gpustack-engine-sentinel` at `/var/lib/gpustack/operator/lib/engine-sentinel`.
- **Owned environment**: Never set `GPUSTACK_ENGINE_SENTINEL_ENABLE`, `GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK`, or `GPUSTACK_ENGINE_SENTINEL_ENGINE` in *`roles[].env`*.

## Supported parameters

Admission validates *`runtimeControl.parameters`* against the translator registry, accelerator manufacturer, topology prerequisites, and declared *`extraArgs`* boot ceilings.

| Parameter | Type | Valid range | Engine support | Boot ceiling |
|---|---|---|---|---|
| `scheduling.maxActiveRequests` | Integer | `>= 1` | `vLLM` (NVIDIA, Ascend), `SGLang` | `<= --max-num-seqs` (vLLM) or `--max-running-requests` (SGLang) when declared |
| `scheduling.maxBatchedTokens` | Integer | `>= 1` | `vLLM` (NVIDIA, Ascend), `SGLang` | `<= --max-num-batched-tokens` (vLLM) or `--chunked-prefill-size` (SGLang) when declared |
| `scheduling.longPrefillTokenThreshold` | Integer | `>= 0` | `vLLM` (NVIDIA, Ascend) | `<=` effective `maxBatchedTokens` when declared |
| `scheduling.prefillMaxRequests` | Integer | `>= 1` | `SGLang` | `<=` effective `maxActiveRequests` when declared |
| `scheduling.scheduleConservativeness` | Decimal | `[0.1, 2.0]` | `SGLang` | None |
| `scheduling.prefillDecodeInterval` | Integer | `>= 0` (`0` disables deferral) | `SGLang` | None |
| `scheduling.cacheAwareWindow` | Integer | `>= 0` | `vLLM` (NVIDIA, Ascend) | None |
| `scheduling.cacheAwareThreshold` | Decimal | `[0.0, 1.0]` | `vLLM` (NVIDIA, Ascend) | None |
| `scheduling.ppMaxMicroBatchSize` | Integer | `>= 1` (`<= effectiveMaxActiveRequests / ppSize`) | `SGLang` | Requires `--pp-size` or `--pipeline-parallel-size > 1` |
| `scheduling.maxInflightKVLoads` | Integer | `>= 1` | `vLLM` (NVIDIA, Ascend) | Requires active KV connector (`kvCache`, `kvTransfer`, routed pair, or `--kv-transfer-config`) |
| `scheduling.maxAsyncKVLoadsPerStep` | Integer | `>= 1` | `vLLM` (NVIDIA, Ascend) | Requires active KV connector (`kvCache`, `kvTransfer`, routed pair, or `--kv-transfer-config`) |
| `speculative.acceptThresholdSingle` | Decimal | `[0.0, 1.0]` | `SGLang` | None |
| `speculative.acceptThresholdAcc` | Decimal | `[0.0, 1.0]` | `SGLang` | None |
| `speculative.draftBudgetFraction` | Decimal | `(0.0, 1.0]` (strictly `> 0.0`, up to `1.0`) | `SGLang` | None |
| `elastic.dataParallelWidth` | Integer | `[2, 64]` | `vLLM` (NVIDIA, Ascend) | `<= --elastic-ep-max-dp-size` when declared; immutable presence, monotonic non-decreasing, `replicas: 1`, `size: 1` |

All parameter values are strings. Decimal parameters accept standard Kubernetes decimal quantities and reject binary suffixes.

In `vLLM`, lowering `scheduling.maxActiveRequests` updates `scheduler.max_num_active_reqs` instead of `scheduler.max_num_running_reqs`. In `SGLang`, it updates a live admission gate without resizing worker pools.

Clearing *`runtimeControl`* or removing keys restores modified sentinel parameters to engine boot defaults before reporting `Synced` or `NotConfigured`.

### Role pause

*`spec.roles[].pause`* works on any role, including CPU-only, *Encode*, SGLang Ray, and unmanaged *`roles[].command`* roles.

Setting `pause: true` withdraws `modeldeployment.gpustack.ai/endpoint-eligible`, observes router withdrawal, and drains in-flight requests for the current container incarnation. On sentinel-injected roles, it also enables the ASGI `503` pause gate on direct `Pod` inference arrivals and `/gpustack/v1/ready`.

Returning `pause: false` clears sentinel pause mode, replays configured parameters, re-acknowledges `/gpustack/v1/ack`, and restores endpoint eligibility.

### Elastic EP integration

`elastic.dataParallelWidth` configures *elastic expert parallelism (Elastic EP)* inside *`runtimeControl.parameters`*.

Its presence is immutable after creation and hashed into `pod-spec-hash`, while numeric updates (`2` to `64`) remain monotonic non-decreasing without replacing surviving `Pod`s. Elastic resizes serialize against role `pause` and replay all non-elastic parameters onto the master after rank expansion.

### Reserved LoRA adapters

*`spec.model.loraAdapters`* (`name`, `artifactRef`, `subPath`, `pinned`) reserves a deployment-level LoRA contract beside *`spec.model.artifactRef`*. Non-empty slices fail admission pending `ModelArtifact` LoRA weight delivery.

### Router hash decoupling

Worker `pod-spec-hash` reads only whether *`spec.router`* is set and *`spec.router.name`*. Editing `replicas`, `image`, `imagePullPolicy`, `imagePullSecrets`, `extraArgs`, `requestTimeoutSeconds`, or `disaggregationThresholdTokens` reconciles the router `Deployment` without rolling worker `Pod`s.

An existing `render-version <= 5` `Pod` keeps dual-hash currency and rolls at most once if a router field changes before replacement.

## Isolation and safety

Default engine sentinel injection, Envoy router allowlists, and boot-local latches isolate control routes without operator-managed API keys or auth `Secret`s.

### Engine sentinel injection

Managed accelerated non-*Encode* roles inject `gpustack-engine-sentinel` by default, leaving CPU-only, *Encode*, unmanaged *`roles[].command`*, and SGLang Ray roles untouched.

- **Flat host staging**: The package is version-locked to `gpustack-operator` and mounted read-only from `/var/lib/gpustack/operator/lib/engine-sentinel`.
- **Zero intrusion**: `sitecustomize.py` post-import hooks load via `PYTHONPATH` without mutating `VLLM_PLUGINS`, container `argv`, or developer modes.
- **Universal ASGI fallback**: Hooks cover `vLLM`, `SGLang`, and `uvicorn.Config` fallback for `vllm-omni` and `sglang-omni`.
- **Unified PreStop drain**: Injected roles run `python3 -m gpustack_engine_sentinel.drain` for graceful `PreStop` draining.

### Router allowlist

All three managed routers enforce a shared Envoy method/path inference allowlist on port `8000` and reject non-inference routes with `403`.

- **Multimodal routes**: The shared allowlist covers standard LLM endpoints plus `vllm-omni` and `sglang-omni` audio, image, and video endpoints.
- **Sidecar proxy**: `vllm-router` and `sglang-gateway` front loopback `127.0.0.1:8001` with Envoy on `0.0.0.0:8000` and expose `/observer/endpoints` on `:9091`.
- **Forbidden router flags**: Admission rejects `--enable-igw`, `--admin-port`, `--admin-host`, `--control-plane-port`, `--control-plane-host`, `--enable-admin-api`, and `--enable-control-plane` in *`spec.router.extraArgs`*.
- **Router-first gate**: Reconcile holds `GPUSTACK_ENGINE_SENTINEL_ENABLE=1` (`WaitingForRouterIsolation`) until the router `Deployment` converges onto the allowlist revision.

### Readiness latch and drain

Every sentinel-injected engine mints a fresh `boot_id` UUID at startup and fails `GET /gpustack/v1/ready` (`503`) until replay and `POST /gpustack/v1/ack` succeed.

- **Liveness and startup**: `startupProbe` and `livenessProbe` poll `GET /gpustack/v1/live` (`200` while alive including paused; `503` on engine failure).
- **Incarnation-relative drain**: Before pausing, drain reading binds to `(podUID, bootID)` across container restarts after withdrawing `endpoint-eligible`.
- **Sharded ledger**: Per-role `ConfigMap` ledgers (`runtime-control-<deploymentUID>-<roleHash>`) fence `5s` mutating `POST` timeouts (`AmbiguousOperation`) for live readback recovery.

### Scaler pause guard

A `ModelDeploymentScaler` coexists with *`pause`* and *`runtimeControl`* because `scaler-binding` restricts only *`spec.roles[].replicas`*. The scaler holds all replica writes (`Blocked=True`, reason `RolePaused`) while desired or current-generation observed pause state is `true`.

## Step 1: Configure boot ceilings

Apply a `ModelDeployment` declaring any static ceilings in *`roles[].extraArgs`*. If a managed router is present, wait for the router `Deployment` to converge onto its Envoy allowlist.

```bash
kubectl apply -f model-deployment.yaml
```

## Step 2: Tune or pause roles

Patch *`spec.roles[].runtimeControl.parameters`* or *`spec.roles[].pause`* without recreating worker `Pod`s.

```bash
kubectl -n team-a patch modeldeployment qwen-tuned --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/scheduling.maxActiveRequests","value":"24"}]'

kubectl -n team-a patch modeldeployment qwen-tuned --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/pause","value":true}]'
```

## Step 3: Verify convergence status

Inspect the `RuntimeControlSynced` condition and *`status.roles[].runtimeControl`* on the `ModelDeployment`, and check `ModelDeploymentScaler` for `RolePaused` holds during pause.

```bash
kubectl -n team-a get modeldeployment qwen-tuned -o jsonpath='{.status.conditions[?(@.type=="RuntimeControlSynced")]}{"\n"}{.status.roles[*].runtimeControl}{"\n"}'
kubectl -n team-a get modeldeploymentscaler -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Blocked")]}{"\n"}{end}'
```

| Condition | Reason | Meaning |
|---|---|---|
| `True` | `Synced` | All answering `Pod`s converged on desired `runtimeControl` and `pause` |
| `True` | `NotConfigured` | No role configures `runtimeControl` or `pause` and prior state is restored |
| `Unknown` | `WaitingForRouterIsolation` | Router `Deployment` has not converged onto the inference allowlist |
| `Unknown` | `Draining` | Eligible endpoints are draining before role pause |
| `Unknown` | `Reconciling` | Replay, pause toggle, or `/gpustack/v1/ack` is in progress |
| `Unknown` | `EndpointUnreachable` | One or more answering `Pod`s cannot be reached |
| `False` | `UnsupportedEngineVersion` | Engine rejected fields listed in `unsupportedFields` |
| `False` | `DrainFailed` | Incarnation-relative drain failed before role pause |
| `False` | `AmbiguousOperation` | Mutating `POST` timed out; live readback did not settle |
| `False` | `ReconcileFailed` | Engine returned a validation or apply error |

## Limitations

Operations that require static restart orchestration or cluster-level GPU reclaim are not applied live:

- **GPU sleep states**: Weight offload and KV eviction power states are deferred until cluster GPU reclaim or sharing is settled.
- **Static topology changes**: `TP`, `PP`, context parallelism, quantization, `--gpu-memory-utilization`, `--kv-cache-dtype`, `--load-format ipc_cache`, and Elastic EP scale-down are not applied live.
- **Excluded `runtimeControl` shapes**: Unmanaged (`roles[].command`), *Encode*, CPU-only, and SGLang Ray roles reject *`runtimeControl`*; *`pause`* remains supported on all roles.
- **Reserved LoRA adapters**: Non-empty *`spec.model.loraAdapters`* is rejected until `ModelArtifact` supports LoRA weights.
- **Gateway authentication**: North-south caller auth belongs to the external Gateway; if user `--api-key` flags block sentinel probes, the controller reports the failure without managing `--api-key`.

## Troubleshooting

| Symptom | Cause | Action |
|---|---|---|
| `WaitingForRouterIsolation` persists | Router `Deployment` lacks Envoy sidecar or ready replicas | Inspect `<deployment>-router` `Deployment` pods and events |
| `UnsupportedEngineVersion` reported | Engine build lacks required scheduler attributes | Read `status.roles[*].runtimeControl.unsupportedFields` and remove unsupported keys |
| `AmbiguousOperation` reported | Control-plane `POST` timed out mid-transition | Check worker `Pod` logs and `/gpustack/v1/runtime_config` state |
| `Pod` stays unready after restart | Engine failed startup replay or `/gpustack/v1/ack` | Inspect operator worker logs and `Pod` `/gpustack/v1/ready` response |
| `RolePaused` blocks scaler | Desired or observed `pause` state is `true` | Set `spec.roles[*].pause: false` and wait for `RuntimeControlSynced` |
| Admission rejects `loraAdapters` | Dynamic LoRA execution is not yet supported | Omit `spec.model.loraAdapters` until `ModelArtifact` LoRA support lands |

---

**See also** — [Online Runtime Control Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/runtime-control/index.md) (recorded NVIDIA H200 run) · [Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) (role fields and rollout hash) · [Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (router policies and allowlist isolation) · [Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) (deployment conditions) · [Scaling](/gpustack-operator/main/docs/modules/model-deployment/scaling/index.md) (`ModelDeploymentScaler` behavior) · [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) (collective resize serialization)

**Next** → [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) — inspect router policies and traffic targets.
