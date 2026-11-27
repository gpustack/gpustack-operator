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
    runner:
      image: gpustack/forge-runner:v0.1.0
      imagePullPolicy: IfNotPresent
      command:
        - loopwright-forger
      args:
        - --budget
        - "4"
      env:
        - name: LOOPWRIGHT_MODEL
          value: gpt-5
        - name: LOOPWRIGHT_PROVIDER_TOKEN
          valueFrom:
            secretKeyRef:
              name: llm-creds
              key: token
      envFrom:
        - configMapRef:
            name: forger-config
    priorityClassName: forge-batch
    activeDeadlineSeconds: 1800  # 60..14400 (default: 1800)
    outputSizeLimit: 2Gi         # 64Mi..16Gi (default: 2Gi)
    imagePullSecrets:
      - name: runner-regcred
    resources:
      cpu: "8"
      ram: 32Gi
      localStorage: 20Gi
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
*expert parallelism (EP)* sharding and normalizes the engine version to `major.minor` (keeping
pre-release tags separate), excluding dynamic batch or sequence lengths. Patch upgrades (`0.8.x`)
reuse verified artifacts, while minor upgrades (`0.8` → `0.9`) or *TP*/*EP* changes trigger
re-forging.

## Runner and verifier contract

In `sandbox.runnerMode: External`, the operator launches a `restricted` Kueue-admitted `ForgeSandbox`
`Pod` pinned to the probed role's GPU architecture, bounded by *`sandbox.activeDeadlineSeconds`*
(`60`–`14400`, default `1800`) and *`sandbox.outputSizeLimit`* (`64Mi`–`16Gi`, default `2Gi`). Four
stages run inside the `Pod` in `External` mode (three in `Mock` mode):

1. **`initContainer: forge-runner`**: Reads `forge-request.json`, `definitions/*.json`, and
   `workloads/*.jsonl` from `/var/run/gpustack/forge/input/` and writes `dispatch-manifest.json`,
   `solutions/*.json`, `kernels/*`, and `traces/*.json` into the `forge-output` `emptyDir`.
2. **`initContainer: forge-stage-sentinel`** (`External` mode only): Copies the operator's built-in
   `gpustack-engine-sentinel` package into a read-only `forge-sentinel` `emptyDir` for the verifier.
3. **`initContainer: forge-verifier`**: Mounts `forge-output` read-only, validates file integrity and
   checksums, re-executes candidate kernels on the sandbox GPU against held-out workloads, enforces
   *`minSpeedupRatio`* and numerical tolerances, and writes `verification-report.json`.
4. **`forge-hold` main container**: Runs `gpustack-operator model-forger hold` to keep the `Pod` in
   `Running` after all init containers exit `0` so the seed `Node`'s `model-manager` can ingest
   `forge-output` directly from kubelet's `emptyDir`.

### Runner container isolation

Configure synthesis container overrides under *`sandbox.runner`* (`image`, `imagePullPolicy`,
`command`, `args`, `env`, and `envFrom`) when *`sandbox.runnerMode`* is `External` (`Mock` mode
rejects *`sandbox.runner`*):

- **Strict container boundary**: *`sandbox.runner`* (`command`, `args`, `env`, `envFrom`, and
  `imagePullPolicy`) applies strictly to `initContainer: forge-runner` and is never exposed to
  `forge-stage-sentinel`, `forge-verifier`, or `forge-hold`.
- **Reserved operator prefix**: `GPUSTACK_FORGE_*` environment names and `envFrom` prefixes are
  reserved and rejected; operator-injected `GPUSTACK_FORGE_INPUT_DIR` and
  `GPUSTACK_FORGE_OUTPUT_DIR` always override `envFrom`.
- **Registry credentials and allowlists**: *`sandbox.imagePullSecrets`* merges after the target
  role's *`imagePullSecrets`* (deduplicated in order), and `forger-allowed-runner-image-prefixes`
  restricts allowed *`sandbox.runner.image`* prefixes.
- **Deterministic mock mode**: Set *`sandbox.runnerMode`* to `Mock` (with *`sandbox.runner`* omitted)
  to run the built-in Go `gpustack-operator model-forger mock` and `model-forger verify` helpers
  without Python.

### Sandbox accelerator allocation

Both `forge-runner` and `forge-verifier` run as sequential `initContainer`s, so the `ForgeSandbox`
`Pod` consumes one accelerator unit plus optional host *`cpu`*, *`ram`*, and *`localStorage`*:

| Allocation mode | *`sandbox.resources`* fields | Guidance |
|---|---|---|
| `Exclusive` (default) | Omit slicing and partitioning fields | Requests `1` whole card; recommended for deterministic verification timing. |
| `Partitioned` | Set *`acceleratorPartitionedProfile`* (e.g., `"3g.40gb"`) | Requests `1` hardware partition on a partition-offering pool; mutually exclusive with slicing. |
| `Sliced` | Set *`acceleratorSlicedMemoryPercentage`* (`1`–`100`) and optional *`acceleratorSlicedCoresPercentage`* (`0`–`100`) | Requests `1` software slice on a logically sliceable pool; subject to neighbor contention. |

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
| `ForgeSandbox` stuck pending | `Blocked=True` (`SandboxQuotaPending`) | Free 1 accelerator unit (whole card, partition, or slice) in the target `ClusterQueue` or raise *`sandbox.priorityClassName`*. |
| Multi-node rollout blocked | `KernelsReady=False` (`PeerSyncDisabled`) | Enable `model-store-peer-sync` and set `modelManager.port: 32445` on consumer `Node`s. |
| Pin ceiling reached | `Blocked=True` (`PinnedCapacityExceeded`) | Delete unused `ModelPrefetch` pins or lower *`rollout.maxRetainedGenerations`*. |
| Canary rolled back | `Ready=True` (`RollbackCompleted`), `mkart` `status.quarantined: true` | Inspect `ForgeSandbox` verification traces and `Pod` startup logs before re-forging. |

---

**See also** — [Model Deployment Configuration](configuration.md) (role fields, rollout behavior, and admission rules) · [Online Runtime Control](runtime-control.md) (live parameter tuning and role pause) · [Model Deployment Scaling](scaling.md) (`ModelDeploymentScaler` coexistence) · [Model Store Operations](../model-delivery/operations.md) (`NodeModelStore` cache and watermark collection)

**Next** → [Model Deployment Scaling](scaling.md) — configure automated replica scaling alongside kernel forging.
