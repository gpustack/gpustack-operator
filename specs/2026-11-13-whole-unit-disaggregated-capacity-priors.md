# Spec: Whole-Unit Disaggregated Capacity Priors and Scaling Documentation Alignment

Status: Built
Blocked on: end-of-build review and final ship gate before setting Status to Shipped
Type: Feature

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Core Features & Acceptance Criteria](#core-features--acceptance-criteria)
  - [Notes / Constraints / Caveats](#notes--constraints--caveats)
  - [Boundaries](#boundaries)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Commands](#commands)
  - [Project Structure](#project-structure)
  - [Code Style](#code-style)
  - [Implementation Plan](#implementation-plan)
  - [Test Plan](#test-plan)
- [Alternatives](#alternatives)
- [Open Questions](#open-questions)

## Summary

Extend `ModelDeploymentScaler` capacity priors to support disaggregated `Prefill`/`Decode` (PD) and
`Encode`/`Prefill`/`Decode` (EPD) deployments alongside single-role `Server` (S) deployments under
limited offline benchmark catalogs. When dedicated `Prefill` or `Decode` catalog profiles are absent,
treat a fixed-ratio PD or EPD deployment as one whole serving unit. Aggregate prompt throughput and
`TTFT` from `Prefill` with generation throughput and `ITL` from `Decode`, estimate an initial
whole-unit throughput prior from matching `Server` catalog profiles, guard stage bottlenecks through
closed-loop latency hysteresis, and step coupled roles atomically by integer unit multiples. In the
same change, document `capacityPrior` catalog defaulting and identity annotations in the scaling
guides, and replace container mount paths (`/models`) in reusable `ModelDeployment` walkthrough
manifests with logical model identifiers.

## Motivation

### Goals

- Enable `spec.algorithm.capacityPrior` on `ModelDeployment` targets that declare `Prefill`/`Decode`
  or `Encode`/`Prefill`/`Decode` roles with managed `spec.router` and `spec.kvTransfer` wiring.
- Support both direct per-role catalog resolution (Tier 1, when dedicated `Prefill` and `Decode`
  profiles both match in the active catalog) and whole-unit fallback resolution (Tier 2, when either
  dedicated profile has zero matches in [priors.yaml](../pkg/worker/scaling/priors/priors.yaml) or a
  configured `ConfigMap`).
- Preserve whole-unit semantics without fabricating per-role capacities from unmeasured GPU time
  splits: measure unit demand as `PromptTokensPerSecond(Prefill) + GenerationTokensPerSecond(Decode)`,
  enforce `TTFT(Prefill)` and `ITL(Decode)` latency hysteresis even under partial signal gaps, step
  coupled roles atomically by whole units, and keep serial multi-replica retirement and abort
  recovery atomic at the decision level.
- Document how `capacityPrior` selects the embedded catalog versus a `ConfigMap` (including
  `catalogRef.key` defaulting to `priors.yaml`) and clarify when `worker.gpustack.ai/precision`,
  `worker.gpustack.ai/model-id`, `worker.gpustack.ai/hardware-id`,
  `worker.gpustack.ai/prefill-hardware-id`, and `worker.gpustack.ai/decode-hardware-id` apply.
- Standardize `spec.model.name` across `ModelDeployment` walkthroughs so reusable manifests and
  client requests declare logical served model names while preserving historical benchmark records.

### Non-Goals

- Do not synthesize isolated per-role capacities from `Server` benchmarks by dividing throughput by
  user-configured replica ratios or assumed GPU service-time fractions.
- Do not claim Tier 2 `Server` fallback is a guaranteed mathematical lower bound for arbitrary P/D
  stage ratios; it is an initial whole-unit throughput prior paired with live latency guards.
- Do not query [InferenceX](https://inferencex.semianalysis.com/zh/inference) or any external
  benchmark service at runtime, and do not calibrate priors online from live cluster traffic.
- Do not enable `capacityPrior` on deployments that attach external KV cache pools
  (`spec.kvCache != nil`), elastic expert parallelism (`ElasticEP`), external data parallelism, or
  custom container commands unless a catalog profile explicitly records that configuration.
- Do not change the public `v1` or storage `v1alpha1` CRD wire schema of `ModelDeploymentScaler`.

## Proposal

In [InferenceX](https://inferencex.semianalysis.com/zh/inference) benchmark sweeps, disaggregated
deployments are reported at the whole-system level: total system token throughput equals prompt
tokens processed on prefill accelerators plus output tokens generated on decode accelerators. Public
disaggregated sweeps remain sparse compared with single-role `Server` sweeps, and all current
profiles in [priors.yaml](../pkg/worker/scaling/priors/priors.yaml) record `role: Server`.

Today, [model_deployment_scaler_prior_identity.go](../pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go)
refuses `capacityPrior` whenever `spec.router`, `spec.kvTransfer`, or an `Encode` role is present,
and renders operator environment checks without synthesizing the disaggregated connector. In
addition, [resolve.go](../pkg/worker/scaling/priors/resolve.go) matches profiles strictly by `role`,
[model_deployment_scaler_constraints.go](../pkg/worker/controllers/worker/model_deployment_scaler_constraints.go)
can suppress minority-role scale-ups under unweighted L1 projection (such as `1P:3D`) or deadlock
scale-downs under the per-role `current - 1` step floor, and
[model_deployment_scaler_planner.go](../pkg/worker/controllers/worker/model_deployment_scaler_planner.go)
tracks only one `(Role, Ordinal)` in `status.lastScaleDown` during multi-replica serial retirement.

This specification introduces a two-tier prior contract for disaggregated deployments, hardens
whole-unit pacing and multi-replica retirement recovery, and aligns scaling and deployment guides
with actual admission and rendering behavior.

### User Stories

#### Story 1

As a platform engineer running a fixed-ratio PD or EPD `ModelDeployment`, I want
`ModelDeploymentScaler` to estimate an initial whole-unit capacity prior from compatible `Server`
profiles when dedicated PD profiles are absent and protect stage bottlenecks via live `TTFT` and
`ITL` guards, so that I can run `TokenThroughput` or `Predictive` autoscaling without hand-tuning
token rates.

#### Story 2

As an ML engineer configuring `capacityPrior` in `ModelDeploymentScaler`, I want accurate
documentation on `catalogRef` defaulting and `ModelDeployment` identity/precision annotations, so
that admission and prior resolution succeed on the first apply.

#### Story 3

As a user following GPUStack Operator walkthroughs, I want reusable `ModelDeployment` manifests and
client `curl` commands to use consistent logical model identifiers instead of `/models`, so that I
do not confuse `spec.model.name` with the container model mount path.

### Core Features & Acceptance Criteria

1. **Disaggregated Serving Eligibility and Synthesized Connector Environment Gate**
   - Allow `capacityPrior` resolution on `ModelDeployment` targets that declare `spec.router`,
     `spec.kvTransfer`, and `Encode`, `Prefill`, and `Decode` roles. Keep refusing
     `spec.kvCache != nil` when the selected profile records `offload: none`.
   - Synthesize the actual disaggregated connector for each role through
     `SynthesizeModelDeploymentConnector` inside
     [model_deployment_scaler_prior_identity.go](../pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go)
     across `vLLM`, `vLLM-Ascend`, and `SGLang` so `modelDeploymentScalerOperatorEnvGate` inspects
     the real injected `Env` and `DefaultedEnv` entries rather than an empty connector struct.
   - Add verified operator disaggregation transport/side-channel and encoder-cache runner variables
     (`VLLM_NIXL_SIDE_CHANNEL_HOST`, `VLLM_MOONCAKE_BOOTSTRAP_PORT`, `VLLM_EC_SIDE_CHANNEL_HOST`,
     `VLLM_EC_SIDE_CHANNEL_PORT`, and `VLLM_USE_V2_MODEL_RUNNER`) to
     `modelDeploymentScalerCapacityNeutralEnvNames`. Keep refusing any unaccounted operator-injected
     variable and keep redacting user environment values from refusal messages.
   - Pass the target router name in Tier 1 `ServingConfig.Router` checks while permitting standalone
     `Server` profiles (`Router: ""` or `"none"`) during Tier 2 whole-unit fallback.
   - Treat `Encode` roles as ratio-coupled (`ReasonRatioCoupled`) with no token capacity field and no
     catalog profile lookup.
   - **Acceptance:** Real synthesized `vLLM`, `vLLM-Ascend`, and `SGLang` PD/EPD roles pass the
     connector environment gate; an unknown injected variable fails closed with redacted values;
     `spec.kvCache != nil` against `offload: none` still fails closed.

2. **Two-Tier Disaggregated Prior Resolution and Unambiguous Wire Status**
   - **Manual bypass:** When all required capacity fields across `Prefill` and `Decode` (including
     both forecast capacities and `TokenThroughput` targets under `Predictive`) are explicitly set in
     the spec, skip catalog, artifact, and hardware reads as today.
   - **Mode selection before resolution:** When any required capacity field is omitted, probe
     dedicated `Prefill` and `Decode` shapes in the active catalog first:
     - **Tier 1 (Direct Role Priors):** Selected when both `Prefill` and `Decode` match a single
       dedicated profile (`Select` succeeds on both). Each role resolves missing fields
       independently; partial explicit per-field overrides remain supported; any ambiguity,
       identity/config mismatch, `IncompatibleMetric`, `InsufficientLoadCoverage`, or infeasible SLO
       fails closed without falling back to Tier 2.
     - **Tier 2 (Whole-Unit `Server` Fallback):** Entered **only** when one or both of `Prefill` and
       `Decode` has zero matching profiles (`len(matched) == 0`) in the active catalog and neither
       role failed with profile ambiguity or declaration/identity refusal. A custom `catalogRef`
       never falls back to the embedded catalog.
   - **Tier 2 requirements and unit capacity estimate:**
     - Refuse partial explicit per-role capacity fields in Tier 2.
     - Solve `spec.ratios` using exact rational arithmetic (`math/big.Rat` via `Quantity.AsDec()`).
       Require every ratio edge to set `min == max`, connect all target roles into one consistent
       component, and reduce to a unique positive integer unit vector `u` (`gcd == 1`) within
       `int32` bounds with a non-empty feasible multiplier interval
       `k_min = max_r ceil(minReplicas_r / u_r) <= k_max = min_r floor(maxReplicas_r / u_r)`.
     - Require both `ttftTargetMilliseconds` and `itlTargetMilliseconds`. Resolve a matching `Server`
       profile for `Prefill` and for `Decode` under each role's own identity and topology against
       both latency bounds (`TTFT` and `ITL`). Compute `c_gpu = min(eff_P / g_P, eff_D / g_D)` and
       estimate initial whole-unit throughput capacity as `C_unit = (u_P * g_P + u_D * g_D) * c_gpu`
       (headroom applied once inside `eff_r`).
     - Document and surface in `CapacityPriorsReady=True` condition text that Tier 2 `C_unit` is an
       initial whole-unit throughput estimate assuming GPU-time-balanced stage sizing rather than a
       guaranteed lower bound under arbitrary P/D splits.
   - **Wire status and joint fingerprint:**
     - Emit one `ModelDeploymentScalerResolvedCapacityPriorStatus` entry per LLM role (`prefill` and
       `decode`) in `status.resolvedCapacityPriors`. Keep `SeedTokensPerSecond` equal to each role's
       own matched `Server` per-replica seed, and record `C_unit` under field names
       `tokenThroughput.unitTargetTokensPerSecond` (and `predictive.unitTokenCapacityPerReplica` for
       `Predictive`) so status consumers never confuse whole-unit capacity with per-replica role
       capacity.
     - Compute a joint `CompatibilityFingerprint` across Tier 2 mode, canonical unit vector `u`, all
       coupled canonical roles (with `Replicas = 0` so ordinary scaling preserves history), model,
       engine, router, `kvTransfer`, identity annotations, algorithm parameters, and both resolved
       `Server` evidence entries. Any change to mode, ratios, member hardware/config, quantile,
       headroom, or catalog digest invalidates in-memory and persisted predictor and stabilization
       state.
   - **Acceptance:** On `1P:3D` and `1E:1P:2D` deployments backed only by `Server` profiles,
     `CapacityPriorsReady` becomes `True` with unambiguous `unitTargetTokensPerSecond` entries in
     `status.resolvedCapacityPriors`; ambiguous profiles, loose ratios (`min < max`), infeasible
     integer bounds, or partial per-role overrides set `CapacityPriorsReady=False`
     (`CapacityPriorUnresolved`).

3. **Whole-Unit Observation, Partial-Signal Latency Guard, Atomic Unit Pacing, and Retirement Safety**
   - **Whole-unit observation and hysteresis:**
     - When `resolved.WholeUnit != nil` and the current replica vector equals `k_cur * u_r`, build
       one unit `RoleObservation` (`Kind: RoleKindServer`, `CurrentReplicas: k_cur`):
       - `ReadyReplicas = min_r floor(ready_r / u_r)` across all coupled roles (including `Encode`
         readiness in EPD).
       - `PromptTokensPerSecond` and `TTFTMilliseconds` from `Prefill`; `GenerationTokensPerSecond`
         and `ITLMilliseconds` from `Decode`.
       - `ThroughputState` is usable only when both `Prefill` and `Decode` throughput signals are
         usable (`SignalFresh` or `SignalIdle`); missing throughput never injects synthetic zeros
         into demand history or the Kalman filter.
       - `LatencyState` is `SignalFresh` whenever at least one of `Prefill` (`TTFT`) or `Decode`
         (`ITL`) is `SignalFresh`, keeping any unreadable counterpart marked `*Unmeasured = true`.
       - `CompleteCoverage` requires `CompleteCoverage` on `Prefill` and `Decode` plus full readiness
         (`ready_E >= current_E`) on `Encode` when present.
     - Preserve `RoleKindServer` latency hysteresis in
       [token_throughput.go](../pkg/worker/scaling/algorithm/token_throughput.go) and
       [predictive.go](../pkg/worker/scaling/algorithm/predictive.go): latency ratio `> 1.2`
       recommends `CurrentReplicas + 1` (`ReasonLatencyGuardScaleUp`, `HoldScaleDown = true`), while
       `1.0 < ratio <= 1.2` holds scale-down (`ReasonLatencyGuardHold`). Ensure `TokenThroughput` and
       `Predictive` still honor a fresh `> 1.2` latency breach when `ReadyReplicas > 0` even if
       `ThroughputState` is `Missing` or `Stale`, without recording an invalid demand sample.
     - Persist the whole-unit `PredictorState` in `status.predictors` under the canonical `Decode`
       role name and persist role-scaled `StabilizationHistory` (`k * u_r`) per role in
       `status.roles` so restarts preserve history without synthetic role-name collisions.
   - **Atomic whole-unit pacing:**
     - Pace integer unit count `k` within `[k_min, k_max]` in `SolveModelDeploymentScalerConstraints`
       when `WholeUnit` is active. Allow at least `±1` whole unit under default step caps while
       enforcing explicit `Pods`/`Percent` step caps (translated to whole-unit steps with a floor of
       hold when an explicit cap forbids a single unit), stabilization windows,
       `HasPendingUnadmittedWorkload`, `DecodePostScaleUpGraceSeconds`, and `InAbortCooldown`.
       Project `vector[r] = k * u_r`. Keep Tier 1 and manual constraint projection unchanged.
   - **Multi-replica serial retirement hold and abort recovery:**
     - Treat any live non-terminal surplus Pod (`ordinal >= current[role]`) observed in
       `executionState` as an active scale-down retirement (`HasActiveRetirement = true`) even in the
       reconcile gap between a cleared `Completed` reservation and the next `Admitted` reservation,
       preventing consecutive scale-downs from overwriting `status.lastScaleDown.preScaleDownVector`.
     - While `status.lastScaleDown` has `Outcome == Committed` for the active scale-down target,
       match `Aborted` reservations on **any** `(role, ordinal)` within the removed range
       `current[role] <= ordinal < PreScaleDownVector[role]` and restore the full
       `PreScaleDownVector` across restarts. Clear stale `Committed` `LastScaleDown` entries once all
       surplus Pods finish or a newer scale-up decision commits, while keeping `CooldownUntil` on
       `Aborted` outcomes.
   - **Acceptance:** Closed-loop causal simulations (using independent stage service times rather
     than setting plant capacity equal to `C_unit`) verify `1P:3D` and `1E:1P:2D` `1x -> 2x -> 1x`
     cycles under load and single-stage latency breaches; controller tests verify abort recovery on
     the first, middle, and last retired ordinal across restarts.

4. **Documentation for `capacityPrior`, Annotations, and Logical `spec.model.name`**
   - Update [scaling.md](../docs/modules/model-deployment/scaling.md),
     [scaling.md](../docs/walkthroughs/model-deployment/scaling.md), and
     [README.md](../docs/README.md) to document:
     - `catalogRef` defaulting and validation: omitting `catalogRef` (`capacityPrior: {}` or setting
       only `headroom`) selects the embedded catalog; `catalogRef: {name: my-priors}` defaults `key`
       to `priors.yaml` (and falls back to legacy `catalog.yaml` at load time if `priors.yaml` is
       absent); `catalogRef: {}` fails admission on missing `name`; an explicit whitespace
       `key` fails admission on `key`.
     - Identity and quantization matching: `ModelArtifact` source/revision + `AcceleratorType`
       hardware + `--quantization`; automatic quantization selection when a single profile matches
       `(role, model, engine, hardware, topology)` versus requiring `--quantization` or
       `worker.gpustack.ai/precision` when multiple quantizations match or to pin precision; optional
       consistency guards `worker.gpustack.ai/model-id`, `worker.gpustack.ai/hardware-id`,
       `worker.gpustack.ai/prefill-hardware-id`, and `worker.gpustack.ai/decode-hardware-id`.
     - Tier 1 versus Tier 2 PD/EPD behavior, Tier 2's GPU-time-balanced prior assumption, latency
       hysteresis (`> 1.2x` scale-up, `(1.0x, 1.2x]` hold), and EPD's `Prefill`-only `TTFT` scope.
   - Replace `spec.model.name: /models` and matching reusable `curl` / `bench_client.py --model`
     arguments across [scaling.md](../docs/walkthroughs/model-deployment/scaling.md),
     [prefill-decode.md](../docs/walkthroughs/model-deployment/prefill-decode.md),
     [encode-prefill-decode.md](../docs/walkthroughs/model-deployment/encode-prefill-decode.md),
     [elastic-ep.md](../docs/walkthroughs/model-deployment/elastic-ep.md),
     [external-dp.md](../docs/walkthroughs/model-deployment/external-dp.md), and
     [multi-host.md](../docs/walkthroughs/model-deployment/multi-host.md) (fixing the internal
     `deepseek-v2-lite-chat` mismatch in `multi-host.md`), while keeping container mount paths
     (`/var/lib/gpustack/model`) and historical captured `Pod` excerpts in
     `encode-prefill-decode.md` factually labeled as recorded benchmark artifacts.
   - **Acceptance:** `git grep -n "name: /models" docs/` returns zero matches, every reusable
     manifest's `spec.model.name` matches its `--served-model-name` and client requests, and
     `make lint docs` exits `0`.

### Notes / Constraints / Caveats

- Grounded against commit `7e33372fc3660f56c1d37293cb4d736d5ab8939d` (`main`) on `Go 1.27.3`
  (`Kubernetes v0.36.5`, `controller-runtime v0.24.1`, `Kueue v0.18.11`).
- Expanding `capacityPrior` eligibility in
  [model_deployment_scaler_prior_identity.go](../pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go)
  supersedes the initial single-role gate from
  [2026-10-26-inferencex-model-deployment-scaling.md](2026-10-26-inferencex-model-deployment-scaling.md)
  for managed `router`, `kvTransfer`, and `Encode` topologies while keeping external `kvCache`,
  `ElasticEP`, external DP, and custom commands gated.
- In EPD deployments, `Encode` exposes no token or latency Prometheus histograms, and all profiles in
  [priors.yaml](../pkg/worker/scaling/priors/priors.yaml) carry `Modality: text`. `TTFT` reflects the
  `Prefill` engine's histogram and does not measure upstream `Encode` compute time unless reflected
  in `Prefill` queueing; `Encode` scales strictly via fixed ratio coupling.

### Boundaries

- **Always:**
  - Keep explicit manual capacity fields authoritative when `capacityPrior` is omitted or all
    required fields are supplied explicitly.
  - Enter Tier 2 only on dedicated profile absence (`len(matched) == 0`), never on ambiguity,
    mismatch, infeasible SLOs, or custom `ConfigMap` read errors.
  - Require fixed connected ratios (`min == max`) and both `TTFT` and `ITL` targets in Tier 2.
  - Run `make lint` for Go changes and `make lint docs` for Markdown changes.
- **Ask first:**
  - Any change to public `api/worker/v1` or storage `api/worker/v1alpha1` CRD schemas.
  - Any modification to embedded [priors.yaml](../pkg/worker/scaling/priors/priors.yaml) data or
    live cluster/hardware benchmark execution (`gpustack-operator-e2e`,
    `gpustack-operator-benchmark`).
- **Never:**
  - Estimate isolated `Prefill` or `Decode` capacities by dividing `Server` throughput by replica
    ratios or assumed GPU service-time fractions.
  - Inject synthetic zero token or latency readings when role metrics are `Missing` or `Stale`.
  - Bypass Kueue `PendingCohort` admission rollback or single-replica retirement draining.
  - Put spec task IDs, decorative symbols, or emojis in Go comments.

### Risks and Mitigations

- **Unbalanced P/D GPU service-time share causes one stage to saturate well below `C_unit` even at
  the benchmark `ISL:OSL`** → Frame `C_unit` explicitly as an initial GPU-time-balanced throughput
  estimate rather than a lower bound, keep `DefaultHeadroom` (`0.75`), and trigger `+1` whole-unit
  scale-up whenever `TTFT(Prefill)` or `ITL(Decode)` exceeds `1.2x` target (holding scale-down above
  `1.0x`), including under missing counterpart signals.
- **Serial single-replica retirement across a multi-replica unit step (`2P:6D -> 1P:3D`) aborts on a
  non-first ordinal or races between reservations** → Hold further scale-downs while any non-terminal
  surplus Pod (`ordinal >= current[role]`) exists, and match `Aborted` reservations across the full
  removed range `current[role] <= ordinal < PreScaleDownVector[role]` to restore `PreScaleDownVector`.
- **Heterogeneous tensor parallelism across `Prefill` and `Decode` alters per-GPU efficiency** →
  Resolve a matching `Server` profile per LLM role's own topology and take
  `c_gpu = min(eff_P / g_P, eff_D / g_D)`; refuse if either topology has no compatible profile.
- **Tier 1 to Tier 2 transition or ratio edit reuses incompatible predictor/stabilization units** →
  Include resolution mode, canonical unit vector `u`, all member configurations, and both `Server`
  seeds in `CompatibilityFingerprint` so any structural change clears in-memory and status state.

## Design Details

### Commands

Local verification runs on `macOS darwin/arm64` with `Go 1.27.3` (`GODEBUG= CGO_ENABLED=1`) and
contacts no Kubernetes cluster:

```bash
# Run priors, algorithm, controller, simulation, and webhook unit/integration suites
go test ./pkg/worker/scaling/priors ./pkg/worker/scaling/algorithm
go test -run 'TestScalerConstraints|TestModelDeploymentScaler' ./pkg/worker/controllers/worker
go test -run 'TestModelDeploymentScalerDefault_CapacityPrior|TestModelDeploymentScalerValidate_CapacityPrior' ./pkg/worker/webhooks/worker

# Run Go formatter and linters (mutates formatting in place when needed)
make lint

# Run documentation, spec, cross-reference, skill, and site gates
make lint docs
```

### Project Structure

```text
pkg/worker/scaling/priors/                         # Offline catalog schema, profile selection, and resolution
pkg/worker/scaling/algorithm/                      # Proportional, TokenThroughput, and Predictive algorithms
pkg/worker/controllers/worker/                     # Scaler prior binding, sampler, planner, constraints, and retirement
pkg/worker/webhooks/worker/                        # ModelDeployment and ModelDeploymentScaler defaulting and validation
docs/modules/model-deployment/                     # Normative configuration and scaling reference guides
docs/walkthroughs/model-deployment/                # Step-by-step serving and scaling walkthroughs
docs/README.md                                     # Shared documentation and owning-spec module index
specs/                                             # Versioned design specifications
```

### Code Style

Follow repository Go, Kubernetes, and testing conventions in
[development.md](../docs/contribute/development.md): `snake_case` Go filenames, explicit error
handling without panics, short plain comments without emojis or spec task IDs, and table-driven
deterministic tests with fake clients. Reuse `RoleKindServer` total-token demand and latency-guard
hysteresis in [token_throughput.go](../pkg/worker/scaling/algorithm/token_throughput.go) while
keeping missing signals distinct from measured zero readings:

```go
func (t TokenThroughput) demand(obs RoleObservation) float64 {
	switch roleKind(obs) {
	case RoleKindPrefill:
		return nonNegative(obs.PromptTokensPerSecond)
	case RoleKindDecode:
		return nonNegative(obs.GenerationTokensPerSecond)
	case RoleKindServer:
		return nonNegative(obs.PromptTokensPerSecond) + nonNegative(obs.GenerationTokensPerSecond)
	default:
		return 0
	}
}
```

### Implementation Plan

`T5` is independent from start. Once `T1` lands, `T2` and `T3` own disjoint packages and can run in
parallel before `T4` finalizes the scaling guides and module index.

- [x] **T1 · Disaggregated identity, connector env gate, and two-tier capacity prior resolution**
  - **Blocked by:** None
  - **Owns:** `pkg/worker/scaling/priors/resolve.go`, `pkg/worker/scaling/priors/priors_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_priors.go`, `pkg/worker/controllers/worker/model_deployment_scaler_priors_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_identity_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_lifecycle.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_lifecycle_test.go`
  - **Gate:** review
  - **Deliverables:** Add exact zero-match profile detection (`IsProfileNotFound`) in `priors`; unlock `spec.router`, `spec.kvTransfer`, and `Encode` roles while keeping `spec.kvCache` gated; synthesize real `ModelDeploymentConnectorRender` in `modelDeploymentScalerRenderedOperatorEnv` across `vLLM`, `vLLM-Ascend`, and `SGLang` PD/EPD topologies and account for disaggregation/EC transport variables while keeping unknown injected vars fail-closed and values redacted; implement exact `math/big.Rat` fixed-ratio minimal unit-vector solver (`u`, `[k_min, k_max]`); implement Tier 1 direct per-role resolution and Tier 2 whole-unit `Server` fallback with `unitTargetTokensPerSecond` / `unitTokenCapacityPerReplica` status entries, joint `CompatibilityFingerprint`, and explicit Tier 2 fallback condition messaging.
  - **Verify:**
    - `go test ./pkg/worker/scaling/priors`
    - `go test -run 'TestModelDeploymentScalerPrior|TestModelDeploymentScalerOperatorEnvGate|TestModelDeploymentScalerCapacityPrior' ./pkg/worker/controllers/worker`
  - **Expected:** Exit code `0`; Tier 1 and Tier 2 resolution matrices, connector env synthesis, and fingerprint invalidation tests pass.

- [x] **T2 · Whole-unit observation, partial-signal latency guard, atomic unit pacing, and retirement recovery**
  - **Blocked by:** T1
  - **Owns:** `pkg/worker/scaling/algorithm/token_throughput.go`, `pkg/worker/scaling/algorithm/predictive.go`, `pkg/worker/scaling/algorithm/algorithm_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_planner.go`, `pkg/worker/controllers/worker/model_deployment_scaler_constraints.go`, `pkg/worker/controllers/worker/model_deployment_scaler_constraints_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_sim_ratio_test.go`
  - **Gate:** review
  - **Deliverables:** Preserve `> 1.2x` latency scale-up when `ReadyReplicas > 0` and `LatencyState == SignalFresh` even if `ThroughputState` is `Missing`/`Stale` without recording bogus zero demand samples; aggregate `Prefill` + `Decode` (+ `Encode` readiness) into a whole-unit `RoleKindServer` observation and persist predictor/stabilization histories cleanly across restarts; add `WholeUnit` atomic integer unit pacing in `SolveModelDeploymentScalerConstraints` without altering Tier 1/manual L1 behavior; hold scale-downs while live non-terminal surplus Pods (`ordinal >= current[role]`) remain and restore `PreScaleDownVector` when any removed `(role, ordinal)` aborts; add causal stage-bottleneck simulation suites (`1P:3D`, `1E:1P:2D`) and multi-ordinal retirement recovery controller tests.
  - **Verify:**
    - `go test ./pkg/worker/scaling/algorithm`
    - `go test -run 'TestScalerConstraints|TestModelDeploymentScaler' ./pkg/worker/controllers/worker`
  - **Expected:** Exit code `0`; `1P:3D` and `1E:1P:2D` scale `1x -> 2x -> 1x` cleanly under throughput and single-stage latency breaches, and aborting first/middle/last retired ordinals restores the pre-scale-down vector.

- [x] **T3 · Webhook defaulting and admission coverage for disaggregated `capacityPrior` and `catalogRef`**
  - **Blocked by:** T1
  - **Owns:** `pkg/worker/webhooks/worker/model_deployment_scaler.go`, `pkg/worker/webhooks/worker/model_deployment_scaler_prior_test.go`
  - **Gate:** review
  - **Deliverables:** Verify and tighten `Default`, `ValidateCreate`, and `ValidateUpdate` for PD and EPD `capacityPrior` scalers (explicit `ttftTargetMilliseconds` and `itlTargetMilliseconds`, pre-target warnings when `ModelDeployment` is absent, `CREATE` rejection versus `UPDATE` warning/repair entry when Tier 2 ratios or `Server` profiles are unresolved, and preservation of manual/single-role behavior); add full defaulting + validation coverage for `catalogRef` omitted, empty `{}`, `{name: x}` (`key` defaulted to `priors.yaml`), and whitespace `key`.
  - **Verify:**
    - `go test -run 'TestModelDeploymentScalerDefault_CapacityPrior|TestModelDeploymentScalerValidate_CapacityPrior' ./pkg/worker/webhooks/worker`
    - `make lint`
  - **Expected:** Exit code `0`; webhook tests and Go linters pass with zero warnings or formatting diffs.

- [x] **T4 · Scaling module reference, scaling walkthrough, and shared index updates**
  - **Blocked by:** T1, T2, T3
  - **Owns:** `docs/modules/model-deployment/scaling.md`, `docs/walkthroughs/model-deployment/scaling.md`, `docs/README.md`
  - **Deliverables:** Document Tier 1 versus Tier 2 `capacityPrior` behavior, GPU-time-balanced estimation limits, `TTFT`/`ITL` hysteresis, EPD `Encode` metric scope, `catalogRef` defaulting, and `ModelDeployment` identity/precision annotation rules in `docs/modules/model-deployment/scaling.md`; replace `spec.model.name: /models` and `--model /models` with `Qwen/Qwen3-8B` and add a fixed-ratio PD `capacityPrior` example in `docs/walkthroughs/model-deployment/scaling.md`; register this specification under `model-deployment` in `docs/README.md`.
  - **Verify:**
    - `make lint docs`
  - **Expected:** Exit code `0` across `check-docs.sh`, `check-specs.sh`, `check-crossrefs.sh`, `hack/check/skills.sh`, and `make site`.

- [x] **T5 · Logical `spec.model.name` alignment across remaining model-deployment walkthroughs**
  - **Blocked by:** None
  - **Owns:** `docs/walkthroughs/model-deployment/prefill-decode.md`, `docs/walkthroughs/model-deployment/encode-prefill-decode.md`, `docs/walkthroughs/model-deployment/elastic-ep.md`, `docs/walkthroughs/model-deployment/external-dp.md`, `docs/walkthroughs/model-deployment/multi-host.md`
  - **Deliverables:** Replace `spec.model.name: /models` in reusable `ModelDeployment` manifests and matching `curl` payloads with consistent logical model identifiers (`Qwen/Qwen3-32B`, `qwen2-5-vl-7b-instruct`, and `deepseek-v2-lite-chat`, resolving the internal mismatch in `multi-host.md`), preserve `/var/lib/gpustack/model` container mount paths, and keep captured historical `Pod` outputs in `encode-prefill-decode.md` explicitly labeled as recorded benchmark excerpts.
  - **Verify:**
    - `make lint docs`
  - **Expected:** Exit code `0`, and zero `name: /models` occurrences remain under `docs/`.

### Test Plan

- [x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

Extend the ratio simulation fixture in
[model_deployment_scaler_sim_ratio_test.go](../pkg/worker/controllers/worker/model_deployment_scaler_sim_ratio_test.go)
with a causal multi-stage serving plant (`Encode -> Prefill -> Decode`) carrying independent per-stage
GPU service times and request conservation rather than setting simulated plant capacity equal to
`C_unit`. Retain all existing single-role, Tier 1, manual, and rebalance regression tests.

#### Unit tests

- `pkg/worker/scaling/priors`: profile-not-found versus ambiguity/mismatch classification; `PerGPU`,
  `PerReplica`, and `WholeSystem` denominator reconstruction; single-application headroom.
- `pkg/worker/controllers/worker` (`model_deployment_scaler_prior*`):
  - Exact rational unit-vector solver: `D/P = 3 -> 1P:3D`, `0.5` versus `500m` equivalence, cycle
    consistency and contradiction, reordered/duplicate/disconnected edges, `min != max` rejection,
    `int32` LCM overflow, and `[k_min, k_max]` empty-interval refusal.
  - Tier 1 / Tier 2 decision matrix: both dedicated roles present, one dedicated role missing, both
    missing, profile ambiguity, identity/config conflict, infeasible SLOs, `InsufficientLoadCoverage`,
    `IncompatibleMetric`, partial versus complete explicit `TokenThroughput` and `Predictive` fields,
    and custom `ConfigMap` non-fallback to embedded `priors.yaml`.
  - Synthesized `vLLM`, `vLLM-Ascend`, and `SGLang` PD/EPD connector environment gates, unknown
    injected variable refusal, `kvCache` refusal, and secret value redaction.
- `pkg/worker/scaling/algorithm` and `model_deployment_scaler_constraints`:
  - Cross-role `Fresh`/`Idle`/`Missing`/`Stale` combinations, single-side readable latency with
    ratios `1.0`, `1.1`, `1.2`, and `> 1.2`, `ThroughputState` missing with `LatencyState` breaching
    `> 1.2`, warmup versus mature `Predictive` filter updates, and zero synthetic-zero pollution.
  - Partial unit readiness (`u=(1,3)` with ready `(2,5)` -> `1` unit; zero ready units; unready
    `Encode` in EPD), default `±1` whole-unit step, explicit `Pods`/`Percent` caps, role bounds,
    `Decode` post-scale-up grace, pending-cohort hold, and abort cooldown.

#### Integration tests

- `pkg/worker/controllers/worker`:
  - Closed-loop controller simulations for `TokenThroughput` and `Predictive` on `1P:3D` and
    `1E:1P:2D` (`1x -> 2x -> 1x`) under P-bottlenecked, D-bottlenecked, and combined load waves.
  - Multi-replica serial retirement recovery: inject `Aborted` on the first, middle, and final
    retired reservation of `2P:6D -> 1P:3D`; interleave scaler reconciles when `md.Status.Retirement`
    is `Completed` or `nil` while surplus Pods still exist; verify restart recovery and verify a
    subsequent scale-up clears stale `Committed` `LastScaleDown` state.
  - Partial Kueue admission deadline rollback (`PendingCohort`), pre-commit `recheckCapacityPriors`
    race rejection, Tier 1 <-> Tier 2 and ratio/hardware/quantile/headroom fingerprint invalidation,
    ordinary replica-change history retention, and steady-state status-write suppression.
- `pkg/worker/webhooks/worker`:
  - End-to-end `Default` + `ValidateCreate` + `ValidateUpdate` coverage for PD/EPD `capacityPrior`,
    missing-target warning, `CREATE` refusal versus `UPDATE` repair/Shadow entry, and the four-case
    `catalogRef` defaulting/validation matrix.

#### e2e tests

Local fake-client controller, simulation, webhook, Go lint (`make lint`), and documentation lint
(`make lint docs`) suites gate implementation completion without requiring cluster GPUs. Real-cluster
reconcile/webhook verification (`gpustack-operator-e2e`) and live multi-GPU serving/scaling
qualification (`gpustack-operator-benchmark`) remain explicit opt-in gates that can be invoked on
user-supplied cluster hardware.

## Alternatives

- **Synthesize per-role `Prefill` and `Decode` capacities from `Server` priors via GPU-share
  division (Option A):** Dividing `Server` input and output rates by the user's `P:D` GPU fraction
  assumes the user's ratio equals the unmeasured compute time split inside the collocated benchmark,
  and feeding independent role recommendations into unweighted L1 projection suppresses minority-role
  scale-ups on non-`1:1` fixed ratios. Rejected.
- **Import only `disagg: true` rows from InferenceX without `Server` fallback:** Upstream
  `disagg: true` sweeps cover far fewer models and accelerators than `Server` sweeps and mostly target
  multi-node wrapper frameworks. Direct `Prefill`/`Decode` profiles remain supported as Tier 1 when
  available, with Tier 2 covering `Server`-only catalogs.
- **Add a new `WholeUnit` algorithm enum to `ModelDeploymentScalerSpec`:** Duplicates
  `TokenThroughput` and `Predictive` configuration surfaces and alters CRD schemas unnecessarily.
  Rejected.

## Open Questions

None.
