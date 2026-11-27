# Model Deployment Forger

A `ModelDeploymentForger` (`mdf`) automates closed-loop single-GPU kernel specialization for a
running `ModelDeployment`. It runs 3–5 step sentinel probes, builds and verifies candidate kernels
in a Kueue-admitted `ForgeSandbox` `Pod`, stores verified bundles in `NodeModelStore` as
`ModelKernelArtifact` (`mkart`) objects, and rolls them out with readiness-gated canary checks.

## Contents

- [Workflow and modes](#workflow-and-modes)
- [Scope and exclusions](#scope-and-exclusions)
- [Runner and verifier contract](#runner-and-verifier-contract)
- [Runtime coexistence](#runtime-coexistence)
- [Canary rollout and rollback](#canary-rollout-and-rollback)
- [Upgrade and downgrade](#upgrade-and-downgrade)
- [Troubleshooting](#troubleshooting)

## Workflow and modes

Each `ModelDeploymentForger` targets one `ModelDeployment` in the same namespace and drives a
six-phase loop per role: `Idle` → `Probing` → `Forging` → `Verifying` → `Publishing` → `Promoting`.
Select the autonomy level with *`spec.mode`*:

| Mode | Probe | `ForgeSandbox` and `mkart` | Target `ModelDeployment` |
|---|---|---|---|
| `ObserveOnly` | Runs 3–5 step probe | Skipped | Unchanged |
| `Shadow` (default) | Runs 3–5 step probe | Builds, verifies, and pins | Unchanged |
| `Active` | Runs 3–5 step probe | Builds, verifies, and pins | Promotes *`kernelArtifactRef`* |

Start in `ObserveOnly` or `Shadow` to confirm bottleneck detection and sandbox verification before
switching to `Active`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentForger
metadata:
  name: qwen-decode-forger
  namespace: team-a
spec:
  targetRef:
    name: qwen-pd  # immutable; ModelDeployment in this namespace
  mode: Active   # ObserveOnly | Shadow | Active (default: Shadow)
  suspend: false
  roles:
    - name: decode  # target role: Server, Prefill, or Decode
      maxKernelsPerGeneration: 4  # 1..16; top non-GEMM bottlenecks
      minSpeedupRatio: "1.05"     # >= 1.01 required by forge-verifier
  probe:
    steps: 4              # 3..5 steps (default: 4)
    cooldownSeconds: 600  # minimum 60 s between passes
    minActiveRequests: 1  # wait for live traffic before probing
  sandbox:
    runnerMode: External  # External | Mock (default: External)
    runnerImage: gpustack/forge-runner:v0.1.0
    timeoutSeconds: 1800  # 60..14400
    outputSizeLimit: 2Gi  # 64Mi..16Gi (default: 2Gi)
    resources:
      cpu: "8"
      ram: 32Gi
  rollout:
    maxRetainedGenerations: 3   # 1..16; prunes older unreferenced mkart
    failureTimeoutSeconds: 600  # auto-rollback deadline on canary failure
```

Set *`spec.suspend`* to `true` to pause probing, sandbox creation, and promotion while releasing
the target binding without deleting the forger.

### Immutable kernel artifacts

In `Shadow` and `Active` modes, the worker creates an immutable `ModelKernelArtifact`
(`<forger>-<role>-gen-<N>`) backed by *`spec.source.nodeStore`*. Only the operator worker identity
can create `nodeStore` artifacts, and binary kernel bundles never use `ConfigMap`s or `Secret`s.

Tenants can also import pre-built kernel bundles into `ModelKernelArtifact` using a 40-character
hex commit SHA under *`spec.source.huggingFace`* or a digest-pinned *`spec.source.image`*
(`registry/repo@sha256:...`), then bind them via *`spec.roles[*].kernelArtifactRef`* when no
`ModelDeploymentForger` owns the target.

## Scope and exclusions

`ModelDeploymentForger` targets memory-bound and irregular single-GPU operators on `vLLM` and
`SGLang` roles running `Server`, `Prefill`, or `Decode` kinds.

| Category | Status | Details |
|---|---|---|
| Allowed single-GPU operators | Supported | `rmsnorm`, `layernorm`, `rope_kv_append`, `activation_quant`, `moe_router_topk`, `moe_permute_unpermute`, `paged_attention`, `ragged_prefill_attention`, `mla_local`, `speculative_verify`, `sampling_logits`, `custom_elementwise_fusion` |
| Dense and block-scaled GEMM | Excluded | `cublas*`/`hipblas*`/`rocblas*`, `aten::mm`, `fp8_blockwise_gemm`, `mxfp8_gemm`, `nvfp4_gemm`, `deep_gemm*` |
| Multi-GPU collectives | Excluded | `nccl*`/`rccl*`/`hccl*`, `all_reduce`, `all_gather`, `reduce_scatter`, `all_to_all`, `fused_all_reduce*` |
| Unsupported role shapes | Rejected | *Encode* roles, CPU-only roles, unmanaged *`roles[].command`* roles, SGLang-Ray roles, and *Elastic EP* roles |

Each target key encodes static per-rank shard dimensions after *tensor parallelism (TP)* and
*expert parallelism (EP)* sharding, excluding dynamic batch or sequence lengths. Changing a role's
*TP* or *EP* degree produces new keys and triggers a fresh forge pass.

## Runner and verifier contract

In `sandbox.runnerMode: External`, the operator launches a `restricted` Kueue-admitted `ForgeSandbox`
`Pod` requesting `1` whole accelerator card matching the probed role's GPU architecture. Three
stages run inside the `Pod`:

1. **`initContainer: forge-runner`**: Reads `forge-request.json`, `definitions/*.json`, and
   `workloads/*.jsonl` from `/var/run/gpustack/forge/input/` and writes `dispatch-manifest.json`,
   `solutions/*.json`, `kernels/*`, and `traces/*.json` into the `forge-output` `emptyDir`.
2. **`initContainer: forge-verifier`**: Mounts `forge-output` read-only, validates file integrity and
   checksums, re-executes candidate kernels on the sandbox GPU against held-out workloads, enforces
   *`minSpeedupRatio`* and numerical tolerances, and writes `verification-report.json`.
3. **`forge-hold` main container**: Holds the `Pod` in `Running` after both init containers exit `0`
   so the seed `Node`'s `model-manager` can ingest `forge-output` directly from kubelet's `emptyDir`.

Clusters may restrict allowed `runnerImage` prefixes with the `forger-allowed-runner-image-prefixes`
cluster `Setting`. For pipeline testing without Python or a synthesis image, set
*`sandbox.runnerMode`* to `Mock` to run the built-in Go `gpustack-operator forge-mock` and
`forge-verify` helpers.

## Runtime coexistence

`ModelDeploymentForger` and `ModelDeploymentScaler` can bind the same `ModelDeployment`
simultaneously without conflict:

- **Orthogonal bindings**: `forger-binding` fences *`spec.roles[*].kernelArtifactRef`*, while
  `scaler-binding` fences *`spec.roles[*].replicas`*.
- **Runtime control alignment**: The forger holds probing and promotion whenever a target role has
  *`pause: true`*, unsynced *`runtimeControl`*, or an active replica retirement (`status.retirement`).
- **Node cache pinning**: Active and rollback-protected `ModelKernelArtifact` digests stay pinned in
  `NodeModelStore.spec.pinned` so watermark collection never evicts them before promotion converges.

## Canary rollout and rollback

When `Active` mode promotes a new `ModelKernelArtifact` onto *`spec.roles[*].kernelArtifactRef`*,
`ModelDeployment` uses recreate-style slot replacement with a readiness gate:

- **Canary gate**: The first replacement slot stays held until every member `Pod` passes
  `/gpustack/v1/ready` and confirms the target `dispatch_manifest_digest` on
  `/gpustack/v1/runtime_config`. Sibling replicas stay on the previous kernel until the canary
  converges.
- **Recreate capacity trade-off**: Because each slot deletes the old replica before creating the
  replacement to avoid double GPU quota, one replica of serving capacity is unavailable during each
  swap. A 1-replica role experiences brief downtime during promotion and rollback.
- **Automatic rollback and quarantine**: If the canary fails readiness past
  *`rollout.failureTimeoutSeconds`* or reports `unsupported_kernel_artifact`, the forger reverts
  *`kernelArtifactRef`* to the previous artifact (or `nil` for stock kernels) and sets
  `status.quarantined: true` on the failed `ModelKernelArtifact`.

Quarantine persists across `ModelDeploymentForger` recreation, and quarantined artifacts are never
promoted or newly mounted.

## Upgrade and downgrade

Upgrading to an operator release with `ModelDeploymentForger` causes zero rollout churn for existing
`ModelDeployment`s that omit *`spec.roles[*].kernelArtifactRef`*.

Before downgrading to an operator version that predates `ModelDeploymentForger` and
`ModelKernelArtifact`, complete these steps in order:

1. Delete all `ModelDeploymentForger` resources across all namespaces.
2. Clear *`spec.roles[*].kernelArtifactRef`* on any `ModelDeployment` using forged kernels and wait
   for replicas to roll back to stock engine kernels.
3. Delete all `ModelKernelArtifact` resources so `NodeModelStore.spec.pinned` releases forged
   digests for normal watermark collection.

## Troubleshooting

Inspect the forger, its generated `ModelKernelArtifact`s, and the target `ModelDeployment`:

```bash
kubectl -n <ns> get mdf,mkart
kubectl -n <ns> describe mdf <forger-name>
kubectl -n <ns> get md <deployment> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

| Symptom | Signal | Resolution |
|---|---|---|
| Probing or promotion paused | `Blocked=True` (`RolePaused`, `RuntimeControlUnsynced`, `RetirementInProgress`) | Unpause the role, wait for `runtimeControl` sync, or wait for retirement to finish. |
| `ForgeSandbox` stuck pending | `Blocked=True` (`SandboxQuotaPending`) | Free 1 whole accelerator card in the target `ClusterQueue` or raise *`sandbox.priorityClassName`*. |
| Multi-node rollout blocked | `KernelsReady=False` (`PeerSyncDisabled`) | Enable `model-store-peer-sync` and set `modelManager.port: 32445` on consumer `Node`s. |
| Pin ceiling reached | `Blocked=True` (`PinnedCapacityExceeded`) | Delete unused `ModelPrefetch` pins or lower *`rollout.maxRetainedGenerations`*. |
| Canary rolled back | `Ready=True` (`RollbackCompleted`), `mkart` `Quarantined=True` | Inspect `ForgeSandbox` verification traces and `Pod` startup logs before re-forging. |

---

**See also** — [Model Deployment Configuration](configuration.md) (role fields, rollout behavior, and admission rules) · [Online Runtime Control](runtime-control.md) (live parameter tuning and role pause) · [Model Deployment Scaling](scaling.md) (`ModelDeploymentScaler` coexistence) · [Model Store Operations](../model-delivery/operations.md) (`NodeModelStore` cache and watermark collection)

**Next** → [Model Deployment Scaling](scaling.md) — configure automated replica scaling alongside kernel forging.
