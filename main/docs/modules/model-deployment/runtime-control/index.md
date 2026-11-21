# Online Runtime Control

A `ModelDeployment` tunes engine parameters live, pauses roles and resizes *Elastic EP* collectives
without restarting worker `Pod`s or changing `--load-format`. See
[Online Runtime Control Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/runtime-control/index.md) for
end-to-end vLLM and SGLang examples.

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
- [Runtime safeguards](#runtime-safeguards)
- [Pause and retirement](#pause-and-retirement)
- [Operations](#operations)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Configuration

Declare static boot ceilings in *`roles[].extraArgs`*, live-tunable values in
*`roles[].runtimeControl.parameters`* and role traffic state in *`roles[].pause`*:

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
      extraArgs:  # static boot ceilings; edits roll worker Pods
        - --max-num-seqs=64
        - --max-num-batched-tokens=8192
      runtimeControl:  # excluded from pod-spec-hash; reconciled live over HTTP
        parameters:
          scheduling.maxActiveRequests: "32"
          scheduling.maxBatchedTokens: "4096"
          scheduling.longPrefillTokenThreshold: "2048"
          scheduling.cacheAwareWindow: "128"
          scheduling.cacheAwareThreshold: "0.5"
```

- **Owned environment**: Never set `GPUSTACK_ENGINE_SENTINEL_ENABLE`,
  `GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK` or `GPUSTACK_ENGINE_SENTINEL_ENGINE` in *`roles[].env`*.
- **Host staging**: Managed accelerated non-*Encode* roles mount read-only `gpustack-engine-sentinel`
  at `/var/lib/gpustack/operator/lib/engine-sentinel`.

## Supported parameters

Admission validates *`runtimeControl.parameters`* against engine support, manufacturer, topology
prerequisites and declared *`extraArgs`* boot ceilings. All values are strings; decimal keys accept
Kubernetes decimal quantities and reject binary suffixes.

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
| `elastic.dataParallelWidth` | Integer | `[2, 64]` | `vLLM` (NVIDIA, Ascend) | `<= --elastic-ep-max-dp-size` when declared; immutable presence, non-decreasing, `replicas: 1`, `size: 1` |

In vLLM, `scheduling.maxActiveRequests` updates `scheduler.max_num_active_reqs` (not
`max_num_running_reqs`); in SGLang, it updates a live admission gate. Removing keys or clearing
*`runtimeControl`* restores engine boot defaults before reporting `Synced` or `NotConfigured`.

### Role pause

*`spec.roles[].pause`* works on every role, including CPU-only, *Encode*, SGLang Ray and unmanaged
*`roles[].command`* roles. Setting `pause: true` revokes `endpoint-eligible`, waits for router
withdrawal, drains in-flight requests and (on sentinel roles) enables the ASGI `503` pause gate on
inference routes and `/gpustack/v1/ready`. Setting `pause: false` clears pause mode, replays
parameters, re-acknowledges `/gpustack/v1/ack` and restores eligibility.

### Elastic EP integration

`elastic.dataParallelWidth` enables *elastic expert parallelism (Elastic EP)* inside
*`runtimeControl.parameters`* ([Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md)). Presence is frozen in `pod-spec-hash`;
numeric increases (`2`–`64`) serialize against *`pause`* and replay non-elastic parameters onto the
master after expansion.

### Reserved LoRA adapters

*`spec.model.loraAdapters`* (`name`, `artifactRef`, `subPath`, `pinned`) reserves a deployment-level
LoRA contract; admission currently rejects non-empty values pending `ModelArtifact` LoRA support.

### Router rollout effects

Worker `pod-spec-hash` reads only *`spec.router`* presence and *`spec.router.name`*. Editing
`replicas`, `image`, `imagePullPolicy`, `imagePullSecrets`, `extraArgs`, `requestTimeoutSeconds` or
`disaggregationThresholdTokens` updates the router `Deployment` without rolling worker `Pod`s.
Pre-existing `render-version <= 5` `Pod`s roll at most once if a router field changes before
replacement.

## Runtime safeguards

### Engine sentinel injection

Managed accelerated non-*Encode* roles inject `gpustack-engine-sentinel` automatically (CPU-only,
*Encode*, unmanaged *`roles[].command`* and SGLang Ray roles are skipped):

- **Zero intrusion**: Loads via `PYTHONPATH` (`sitecustomize.py`) without altering `VLLM_PLUGINS`,
  container `argv` or developer modes.
- **ASGI coverage**: Hooks vLLM, SGLang and `uvicorn.Config` fallback (`vllm-omni`, `sglang-omni`).
- **PreStop drain**: Runs `python3 -m gpustack_engine_sentinel.drain` on `Pod` deletion.

### Router API allowlist

All three managed routers enforce a shared Envoy method/path allowlist on port `8000` (including
`vllm-omni` and `sglang-omni` multimodal endpoints) and return `403` on non-inference routes:

- **Envoy sidecar**: `vllm-router` and `sglang-gateway` front loopback `127.0.0.1:8001` with Envoy
  on `0.0.0.0:8000` and expose `/observer/endpoints` on `:9091`.
- **Forbidden flags**: Admission rejects `--enable-igw`, `--admin-port`, `--admin-host`,
  `--control-plane-port`, `--control-plane-host`, `--enable-admin-api` and `--enable-control-plane`
  in *`spec.router.extraArgs`*.
- **Router-first gate**: Reconcile holds `GPUSTACK_ENGINE_SENTINEL_ENABLE=1`
  (`WaitingForRouterIsolation`) until the router `Deployment` converges on the allowlist revision.

### Startup readiness and drain

Each sentinel-injected engine generates a per-boot `boot_id` UUID and returns `503` on
`GET /gpustack/v1/ready` until replay and `POST /gpustack/v1/ack` succeed:

- **Liveness and startup**: Poll `GET /gpustack/v1/live` (`200` while alive including paused; `503`
  on failure).
- **Incarnation-bound drain**: Binds pre-pause draining to `(podUID, bootID)` across restarts.
- **Sharded ledger**: Per-role `ConfigMap` (`runtime-control-<deploymentUID>-<roleHash>`) fences
  `5 s` `POST` timeouts (`AmbiguousOperation`) for readback recovery.

### Scaling during pause

`ModelDeploymentScaler` coexists with *`pause`* and *`runtimeControl`* because `scaler-binding`
fences only *`replicas`*. While desired or observed *`pause`* is `true`, the scaler holds writes
with `Blocked=True` (`RolePaused`).

## Pause and retirement

Role pause, replica retirement and `Pod` deletion drain follow separate lifecycles:

| Mode | Trigger | Capacity and quota | Signal and recovery |
|---|---|---|---|
| **Paused** | *`roles[].pause: true`* | Keeps `Pod`s, weights and Kueue `Workload` quota | `RuntimeControlSynced=True`, `paused: true`, `/ready=503`, `/live=200`, scaler `RolePaused`; set `pause: false` to resume |
| **Retiring** | *`replicas`* decrease, surplus shed or dropped role | Holds `Pod`s and `Workload` until drained, then deletes both | *`status.retirement.state`* steps to `Deleting`; re-declaring the ordinal before `Deleting` cancels (`RetirementAdopted`) |
| **`Pod` drain** | Rollout, eviction or post-retirement delete | Holds devices up to *`terminationGracePeriodSeconds`* (`G`, default `30 s`) | `preStop` drains in-flight work before `SIGTERM` |

### Retirement budgets

At most one retirement runs per deployment (*`status.retirement`*); unscheduled or unadmitted
replicas delete immediately without running the protocol:

| Retirement step | Budget | Behavior |
|---|---|---|
| `Withdrawing` | `30 s` | Revokes `endpoint-eligible` and awaits Router withdrawal (requires *`spec.router`*) |
| `Draining` | `240 s` | Waits `60 s` on disaggregated (*PD* / *External DP*) replicas, then requires two idle `pods/exec` `/metrics` reads |
| `Settling` | `30 s` | Deletes UID-preconditioned `Pod`s and `Workload` (`300 s` total operation cap) |

Exceeding any budget before `Deleting` sets `state: Aborted` and keeps the `Pod`s and quota.
Inspect active or stalled retirement:

```bash
kubectl -n <namespace> get modeldeployment <name> -o json \
  | jq '{retirement: .status.retirement, roles: [.status.roles[] | {name, ready, desired, runtimeControl}]}'
```

### Drain window and grace

On `Pod` deletion, `preStop` serves normally for `0`–`5 s` so late-routed traffic lands, polls
in-flight gauges (`vllm:num_requests_running` + `vllm:num_requests_waiting`, or SGLang running,
queue, bootstrap and transfer gauges) from `5 s` to `G - 5` (`G` in `15`–`3600 s`), and exits on
two idle reads so `SIGTERM` arrives by `G - 3`.

Set *`roles[].terminationGracePeriodSeconds: 45`* on SGLang *Prefill* and *Decode* roles because
SGLang's split-role scheduler waits ~15 s after `SIGTERM` even when idle. Take-over
*`roles[].command`* roles receive no `preStop` hook.

## Operations

### Step 1: Set startup limits

Apply a `ModelDeployment` with static ceilings in *`roles[].extraArgs`* and wait for any managed
router to converge:

```bash
kubectl apply -f model-deployment.yaml
```

### Step 2: Tune or pause roles

Patch *`runtimeControl.parameters`* or *`pause`* without recreating worker `Pod`s:

```bash
kubectl -n team-a patch modeldeployment qwen-tuned --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/scheduling.maxActiveRequests","value":"24"}]'

kubectl -n team-a patch modeldeployment qwen-tuned --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/pause","value":true}]'
```

### Step 3: Verify runtime settings

Inspect `RuntimeControlSynced`, *`status.roles[].runtimeControl`* and scaler `Blocked` conditions:

```bash
kubectl -n team-a get modeldeployment qwen-tuned -o jsonpath='{.status.conditions[?(@.type=="RuntimeControlSynced")]}{"\n"}{.status.roles[*].runtimeControl}{"\n"}'
kubectl -n team-a get modeldeploymentscaler -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Blocked")]}{"\n"}{end}'
```

| Condition | Reason | Meaning |
|---|---|---|
| `True` | `Synced` | All answering `Pod`s match desired `runtimeControl` and `pause` |
| `True` | `NotConfigured` | No role sets `runtimeControl` or `pause` and prior state is restored |
| `Unknown` | `WaitingForRouterIsolation` | Router `Deployment` has not converged on Envoy allowlist |
| `Unknown` | `Draining` | Eligible endpoints are draining before pause |
| `Unknown` | `Reconciling` | Replay, pause toggle or `/gpustack/v1/ack` in progress |
| `Unknown` | `EndpointUnreachable` | One or more answering `Pod`s unreachable |
| `False` | `UnsupportedEngineVersion` | Engine rejected fields in `unsupportedFields` |
| `False` | `DrainFailed` | Pre-pause incarnation drain failed |
| `False` | `AmbiguousOperation` | Mutating `POST` timed out; readback did not settle |
| `False` | `ReconcileFailed` | Engine returned a validation or apply error |

## Limitations

- **Deferred power states**: GPU sleep / weight offload and KV eviction states are not applied live.
- **Static parameters**: *TP*, *PP*, context parallelism, quantization, `--gpu-memory-utilization`,
  `--kv-cache-dtype`, `--load-format ipc_cache` and *Elastic EP* scale-down require restart or
  recreation.
- **Excluded shapes**: Unmanaged (`roles[].command`), *Encode*, CPU-only and SGLang Ray roles reject
  *`runtimeControl`*; *`pause`* works on all roles.
- **Gateway auth**: Put caller auth at the external gateway; engine `--api-key` blocks sentinel
  probes.

## Troubleshooting

| Symptom | Owner | Action |
|---|---|---|
| `WaitingForRouterIsolation` persists | `<deployment>-router` `Deployment` | Inspect router `Pod`s and events for Envoy sidecar or image failures. |
| `UnsupportedEngineVersion` reported | `ModelDeployment` / engine image | Read *`status.roles[*].runtimeControl.unsupportedFields`* and remove unsupported keys or upgrade the engine image. |
| `AmbiguousOperation` reported | Worker `Pod` | Inspect worker `Pod` logs and `/gpustack/v1/runtime_config` state before retrying. |
| `Pod` stays unready after restart | Worker `Pod` / operator worker | Check `/gpustack/v1/ready` response and operator worker logs for replay or `/ack` errors. |
| `RolePaused` blocks scaler | `ModelDeployment` | Set *`spec.roles[*].pause: false`* and wait for `RuntimeControlSynced=True`. |
| Retirement `Aborted` at `30 s` | `ModelDeployment` | Add *`spec.router`* for active scale-down (unrouted `Service` withdrawal cannot be proved), or scale down before admission. |
| Disaggregated retirement stays `Draining` | `ModelDeployment` / member `Pod`s | Wait out the normal `60 s` handoff grace, inspect *`status.retirement.reason`* and `/metrics`, or raise *`replicas`* before `Deleting` (`RetirementAdopted`). |
| Admission rejects `loraAdapters` | `ModelDeployment` | Omit *`spec.model.loraAdapters`* until `ModelArtifact` LoRA support ships. |

---

**See also** — [Online Runtime Control Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/runtime-control/index.md) (end-to-end vLLM and SGLang examples) ·
[Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) (role fields, readiness and rollout hash) ·
[Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (router policies and endpoint eligibility) ·
[Model Deployment Scaling](/gpustack-operator/main/docs/modules/model-deployment/scaling/index.md) (`ModelDeploymentScaler` behavior) ·
[Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) (collective resize serialization)

**Next** → [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) — inspect router policies and traffic targets.
