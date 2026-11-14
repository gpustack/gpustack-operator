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
`Encode`/`Prefill`/`Decode` (EPD) deployments alongside single-role `Server` deployments, and align
the offline prior catalog schema with measured benchmark physics by replacing `role: Server |
Prefill | Decode` with `mode: aggregated | disaggregated`. Under `mode: disaggregated`, each
profile records coupled `prefill` and `decode` stage blocks (`replicas`, `hardware`, and `topology`)
with load points normalized per minimal coupled unit (`PerUnit`). Under `capacityPrior`, a
fixed-ratio PD or EPD deployment resolves directly from a matching `mode: disaggregated` profile
when present, and falls back to estimating initial whole-unit capacity `C_unit` from `mode:
aggregated` stage profiles when no `mode: disaggregated` profile matches—guarding stage bottlenecks
through closed-loop `TTFT`/`ITL` hysteresis and stepping coupled roles atomically by integer units.
In the same change, upgrade the InferenceX importer (`gen/priors`) and sync live `aggregated` and
`disaggregated` sweeps into [priors.yaml](../pkg/worker/scaling/priors/priors.yaml), document
`capacityPrior` catalog defaulting and identity annotations, and standardize logical
`spec.model.name` identifiers across `ModelDeployment` walkthroughs.

## Motivation

### Goals

- Enable `spec.algorithm.capacityPrior` on `ModelDeployment` targets that declare `Prefill`/`Decode`
  or `Encode`/`Prefill`/`Decode` roles with managed `spec.router` and `spec.kvTransfer` wiring.
- Replace the artificial `role: Server | Prefill | Decode` catalog field on
  `ModelDeploymentScalerCapacityPriorProfile` with `mode: aggregated | disaggregated` so every
  profile represents a physically measurable end-to-end serving system: one scaling replica in
  `aggregated` mode (`PerReplica`) or one minimal coupled `(Prefill, Decode)` unit in
  `disaggregated` mode (`PerUnit`).
- Resolve fixed-ratio PD and EPD deployments in two whole-unit priorities:
  - **Priority 1 (`mode: disaggregated`):** match `(model, quantization, engine)` plus coupled
    `prefill` and `decode` hardware/topology and reduced `P:D` unit ratio, scaling minimal-unit
    throughput by `pdScale = gcd(u_P, u_D)`.
  - **Priority 2 (`mode: aggregated` fallback):** entered only when zero `mode: disaggregated`
    profiles match (`IsProfileNotFound`), estimating initial whole-unit throughput capacity `C_unit`
    from `mode: aggregated` profiles for each LLM stage's hardware and topology.
- Preserve whole-unit runtime control without fabricating isolated per-role capacities: observe unit
  demand as `PromptTokensPerSecond(Prefill) + GenerationTokensPerSecond(Decode)`, enforce
  `TTFT(Prefill)` and `ITL(Decode)` latency hysteresis even under partial signal gaps, pace coupled
  roles atomically by integer units, and recover multi-replica serial retirements atomically.
- Upgrade `gen/priors/adapter/inferencex.go` to import multi-node (`> 8` GPUs) and disaggregated
  (`disagg: true`) InferenceX sweeps under throughput conservation and worker-divisibility gates,
  sync live upstream sweeps into [priors.yaml](../pkg/worker/scaling/priors/priors.yaml), and record
  upstream artifact checksums in
  [records.md](../.agents/skills/gpustack-operator-priors/references/records.md).
- Document `catalogRef` defaulting (`priors.yaml` with `priors.json` legacy fallback), identity and
  precision annotations, and logical `spec.model.name` usage across scaling and deployment guides.

### Non-Goals

- Do not retain isolated `role: Prefill` or `role: Decode` profiles in the catalog schema, and do
  not synthesize isolated per-role capacities from `aggregated` or `disaggregated` benchmarks by
  dividing throughput by replica ratios or unmeasured GPU service-time fractions.
- Do not claim Priority 2 `mode: aggregated` fallback is a guaranteed mathematical lower bound for
  arbitrary P/D stage ratios; it is an initial whole-unit throughput prior paired with live latency
  guards.
- Do not query [InferenceX](https://inferencex.semianalysis.com/zh/inference) or any external
  benchmark service at runtime, and do not calibrate priors online from live cluster traffic.
- Do not enable `capacityPrior` on deployments that attach external KV cache pools
  (`spec.kvCache != nil`), elastic expert parallelism (`ElasticEP`), external data parallelism, or
  custom container commands unless a catalog profile explicitly records that configuration.
- Do not change the public `v1` or storage `v1alpha1` Kubernetes CRD wire schema of
  `ModelDeploymentScaler` (`ModelDeploymentScalerCapacityPriorCatalog` is `+protobuf=false` offline
  catalog YAML/JSON only).

## Proposal

In [InferenceX](https://inferencex.semianalysis.com/zh/inference) and self-hosted benchmarks,
neither `Prefill` nor `Decode` can be benchmarked end-to-end in isolation: every measurement runs
either an `aggregated` single-role replica (`disagg: false`) or a `disaggregated` coupled
`(Prefill, Decode)` cohort (`disagg: true`) whose total token throughput satisfies conservation
`input_tput_per_gpu * num_prefill_gpu + output_tput_per_gpu * num_decode_gpu == tput_per_gpu *
(num_prefill_gpu + num_decode_gpu)`. Splitting coupled runs into separate `role: Prefill` and
`role: Decode` catalog entries would discard the measured `P:D` cohort ratio and cross-stage
parallelism pairing.

This specification realigns the catalog schema to `mode: aggregated | disaggregated`, upgrades the
InferenceX importer to ingest both `aggregated` (including `16`/`32`/`64`-GPU shapes) and
`disaggregated` sweeps, resolves PD/EPD scalers via measured `disaggregated` whole-unit curves first
with `aggregated` whole-unit fallback when absent, hardens whole-unit pacing and serial retirement
recovery, and updates the priors skill and user documentation.

### User Stories

#### Story 1

As a platform engineer running a fixed-ratio PD or EPD `ModelDeployment`, I want
`ModelDeploymentScaler` to use a measured `mode: disaggregated` curve when my `P:D` shape is in the
catalog and fall back to a `mode: aggregated` whole-unit estimate plus live `TTFT`/`ITL` guards
when it is not, so that `TokenThroughput` and `Predictive` autoscaling work out of the box without
hand-tuning token rates.

#### Story 2

As an ML engineer configuring `capacityPrior` or maintaining custom `ConfigMap` priors, I want a
catalog schema that records `mode: aggregated | disaggregated` directly from end-to-end benchmarks
and clear documentation on `catalogRef` defaulting and `ModelDeployment` annotations, so that custom
and embedded catalogs match real benchmark outputs.

#### Story 3

As a user following GPUStack Operator walkthroughs, I want reusable `ModelDeployment` manifests and
client `curl` commands to use consistent logical model identifiers instead of `/models`, so that I
do not confuse `spec.model.name` with the container model mount path.

### Core Features & Acceptance Criteria

1. **Mode-Keyed Offline Catalog Schema (`aggregated` / `disaggregated`) and InferenceX Sync**
   - Replace `Role string` on `ModelDeploymentScalerCapacityPriorProfile` in
     [model_deployment_scaler.go](../api/worker/v1alpha1/model_deployment_scaler.go) with
     `Mode string` (`"aggregated"` or `"disaggregated"`):
     - `mode: aggregated`: requires top-level `hardware` and `topology`
       (`*ModelDeploymentScalerCapacityPriorTopology`, `omitempty`), forbids `prefill`/`decode`,
       and normalizes load points per replica (`DenominatorPerReplica`, `MeasuredReplicas: 1`).
     - `mode: disaggregated`: forbids top-level `hardware`/`topology`, requires `prefill` and
       `decode` (`*ModelDeploymentScalerCapacityPriorStage` with `replicas`, `hardware`, and
       `topology`) satisfying `replicas > 0` and `gcd(prefill.replicas, decode.replicas) == 1`, and
       normalizes load points per minimal coupled unit (`DenominatorPerUnit`, `MeasuredReplicas: 1`).
     - Both modes require `CacheMissedTotalTokensPerSecond` (`CacheMissedInputPlusOutput`) and both
       `TTFT` and `ITL` latency quantiles.
   - Raise `MaxProfiles` to `2048` and `MaxCatalogBytes` to `4 MiB` in
     [catalog.go](../pkg/worker/scaling/priors/catalog.go), and add
     `"minimaxm2.5": "MiniMaxAI/MiniMax-M2.5"` to `canonicalModelNames` in
     [identity.go](../pkg/worker/scaling/priors/identity.go).
   - Upgrade [inferencex.go](../gen/priors/adapter/inferencex.go) and
     [main.go](../gen/priors/main.go):
     - Remove the `<= 8` GPUs-per-replica cap and `is_multinode` / `disagg` exclusions.
     - Map disaggregated frameworks (`dynamo-vllm`, `llmd-vllm`, `vllm-disagg` -> `vllm`;
       `dynamo-sglang`, `mori-sglang` -> `sglang`) via `priors.EngineFamily`.
     - Enforce `<= 2%` relative throughput conservation (`input + output` vs `total`) on every row;
       for `disagg: true`, require positive `prefill_num_workers` and `decode_num_workers`, exact
       integer GPU divisibility per worker (`num_prefill_gpu % prefill_num_workers == 0` and
       `num_decode_gpu % decode_num_workers == 0`), fixed worker cohort inside `sweepKey()`, and
       normalization of `(m_P, m_D)` and system rates by `gcd(m_P, m_D)`.
     - Replace `--role` with `--mode` (plus stage filter support) on `gen/priors --retire`, sync all
       12 live InferenceX datasets into [priors.yaml](../pkg/worker/scaling/priors/priors.yaml), and
       run `make generate` and `make generate priors`.
   - **Acceptance:** `LoadEmbedded()` validates cleanly with both `aggregated` (including `> 8` GPU
     shapes) and `disaggregated` profiles; conservation or divisibility violations in upstream rows
     are dropped deterministically with unit test coverage.

2. **Disaggregated Serving Eligibility and Synthesized Connector Environment Gate**
   - Allow `capacityPrior` resolution on `ModelDeployment` targets that declare `spec.router`,
     `spec.kvTransfer`, and `Encode`, `Prefill`, and `Decode` roles. Keep refusing
     `spec.kvCache != nil` when the selected profile records `offload: none` or omits offload.
   - Synthesize the actual disaggregated connector for each role through
     `SynthesizeModelDeploymentConnector` inside
     [model_deployment_scaler_prior_identity.go](../pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go)
     across `vLLM`, `vLLM-Ascend`, and `SGLang` so `modelDeploymentScalerOperatorEnvGate` inspects
     the real injected `Env` and `DefaultedEnv` entries.
   - Account for operator disaggregation transport/side-channel and encoder-cache runner variables
     (`VLLM_NIXL_SIDE_CHANNEL_HOST`, `VLLM_MOONCAKE_BOOTSTRAP_PORT`, `VLLM_EC_SIDE_CHANNEL_HOST`,
     `VLLM_EC_SIDE_CHANNEL_PORT`, and `VLLM_USE_V2_MODEL_RUNNER`) in
     `modelDeploymentScalerCapacityNeutralEnvNames`. Keep refusing unknown injected variables and
     redacting user environment values.
   - Treat `Encode` roles as ratio-coupled (`ReasonRatioCoupled`) with no token capacity field and no
     catalog profile lookup.
   - **Acceptance:** Real synthesized `vLLM`, `vLLM-Ascend`, and `SGLang` PD/EPD roles pass the
     connector environment gate; an unknown injected variable fails closed with redacted values.

3. **Whole-Unit Disaggregated Prior Resolution (`disaggregated` Primary + `aggregated` Fallback)**
   - **Deployment mode mapping:**
     - Targets without `Prefill`+`Decode`: `Server` roles resolve per replica against
       `mode: aggregated`. Malformed single-half `Prefill`-only or `Decode`-only targets fail closed
       when catalog resolution is needed, while explicit manual capacities remain valid for any role.
     - Targets declaring both `Prefill` and `Decode` (PD or EPD): bypass catalog/artifact/hardware
       reads when all required capacity fields on `Prefill` and `Decode` are explicit; otherwise
       resolve in whole-unit mode.
   - **Whole-unit prerequisites (shared by Priority 1 and Priority 2):**
     - Refuse partial explicit per-role capacity fields on `Prefill` or `Decode`.
     - Require positive finite `ttftTargetMilliseconds` and `itlTargetMilliseconds`.
     - Solve `spec.ratios` via exact rational arithmetic (`math/big.Rat` over `Quantity.AsDec()`),
       requiring `min == max` across one connected component, a unique minimal integer unit vector
       `u` (`gcd == 1`) within `int32` bounds, and `k_min <= k_max`.
   - **Priority 1 (`ModelDeploymentScalerPriorModeDisaggregated` — measured `mode: disaggregated`):**
     - Compute `pdScale = gcd(u_P, u_D)` and reduced PD unit `(r_P, r_D) = (u_P / pdScale,
       u_D / pdScale)`.
     - Probe `mode: disaggregated` matching `(model, quantization, engine)`, `prefill: {r_P,
       hardware_P, topology_P}`, and `decode: {r_D, hardware_D, topology_D}` (when both stages state
       explicit conflicting quantizations, zero `disaggregated` profiles match).
     - When a single `mode: disaggregated` profile matches, validate both stages' identities and
       `ServingConfig` (including router), resolve against `(TTFT, ITL, LatencyQuantile, Headroom)`,
       and set `C_unit = pdScale * EffectiveTokensPerSecond`. Any ambiguity, identity/config
       mismatch, `CheckEligibility` failure, or infeasible SLO on `mode: disaggregated` **fails
       closed immediately** without falling back to `mode: aggregated`.
   - **Priority 2 (`ModelDeploymentScalerPriorModeAggregatedFallback` — `mode: aggregated` fallback):**
     - Entered **only** when `mode: disaggregated` has zero matching profiles (`IsProfileNotFound`)
       in the active catalog (custom `catalogRef` never falls back to embedded `priors.yaml`).
     - Resolve `Prefill` and `Decode` shapes independently against `mode: aggregated` (permitting
       standalone `Router: ""` or `"none"`), compute `c_gpu = min(eff_P / g_P, eff_D / g_D)` and
       `C_unit = max(1, floor((u_P * g_P + u_D * g_D) * c_gpu))`, and surface the GPU-time-balanced
       initial-estimate note in `CapacityPriorsReady=True`.
   - **Wire status and joint fingerprint:**
     - Emit `prefill` and `decode` entries in `status.resolvedCapacityPriors` recording `C_unit`
       under `tokenThroughput.unitTargetTokensPerSecond` (plus
       `predictive.unitTokenCapacityPerReplica` for `Predictive`) and a joint
       `CompatibilityFingerprint` covering resolution mode, unit vector `u`, canonical roles
       (`Replicas = 0`), model, engine, router, `kvTransfer`, annotations, algorithm spec, and stage
       evidence.
   - **Acceptance:** Matched `mode: disaggregated` PD/EPD shapes resolve exact measured unit curves;
     unmatched ratios/shapes fall back cleanly to `mode: aggregated` `C_unit`; ambiguity or SLO
     infeasibility on a matched `mode: disaggregated` profile fails closed without fallback.

4. **Whole-Unit Observation, Partial-Signal Latency Guard, Atomic Unit Pacing, and Retirement Safety**
   - Aggregate `Prefill` (`PromptTokensPerSecond`, `TTFT`) and `Decode` (`GenerationTokensPerSecond`,
     `ITL`) plus `Encode` readiness into one whole-unit `RoleObservation` (`RoleKindServer`,
     `CurrentReplicas: k_cur`, `ReadyReplicas: min_r floor(ready_r / u_r)`), propagating the sampled
     observation timestamp and never injecting synthetic zeros on missing/stale signals.
   - Preserve `> 1.2x` latency scale-up (`ReasonLatencyGuardScaleUp`) and `(1.0x, 1.2x]` hold
     (`ReasonLatencyGuardHold`) in `TokenThroughput` and `Predictive`, including when
     `ThroughputState` is missing/stale while `LatencyState` is fresh and `ReadyReplicas > 0`.
   - Pace integer unit count `k` atomically within `[k_min, k_max]` in
     `SolveModelDeploymentScalerConstraints` when `WholeUnit != nil`, hold scale-downs while live
     non-terminal surplus Pods (`ordinal >= current[role]`) exist, and restore `PreScaleDownVector`
     when any retired `(role, ordinal)` aborts.
   - **Acceptance:** Closed-loop causal simulations verify `1P:3D` and `1E:1P:2D` `1x -> 2x -> 1x`
     cycles under load and single-stage latency breaches; controller tests verify multi-ordinal
     retirement abort recovery across restarts.

5. **Priors Skill Provenance, Scaling Documentation, and Logical `spec.model.name` Alignment**
   - Update [SKILL.md](../.agents/skills/gpustack-operator-priors/SKILL.md) and
     [records.md](../.agents/skills/gpustack-operator-priors/references/records.md) with the
     `mode: aggregated | disaggregated` rules, conservation/divisibility gates, `--mode` retirement
     flags, and the 12 verified InferenceX dataset URLs, row counts, and SHA-256 digests.
   - Update [scaling.md](../docs/modules/model-deployment/scaling.md) (staying `<= 1000` lines),
     [scaling.md](../docs/walkthroughs/model-deployment/scaling.md), and
     [README.md](../docs/README.md) to document `mode: aggregated | disaggregated`, Priority 1
     measured `disaggregated` resolution versus Priority 2 `aggregated` whole-unit fallback,
     `catalogRef` defaulting (`priors.yaml` with `priors.json` legacy fallback), and
     `ModelDeployment` identity/precision annotations.
   - Keep logical `spec.model.name` and rendered `--served-model-name` identifiers standardized
     across all `ModelDeployment` walkthroughs (`git grep -n "name: /models" docs/` returns zero
     matches, and Step 2 `Pod` excerpts in `encode-prefill-decode.md` align `--served-model-name`
     to `qwen2-5-vl-7b-instruct` without internal `/models` historical notes).
   - **Acceptance:** `make lint` and `make lint docs` exit `0`, and `scaling.md` stays within the
     1000-line cap.

### Notes / Constraints / Caveats

- Grounded against commit `7e33372fc3660f56c1d37293cb4d736d5ab8939d` (`main`) on `Go 1.27.3`
  (`Kubernetes v0.36.5`, `controller-runtime v0.24.1`, `Kueue v0.18.11`).
- `ModelDeploymentScalerCapacityPriorCatalog` carries `+protobuf=false` and is serialized only in
  embedded `priors.yaml` and user `ConfigMap` payloads; changing `role` to `mode` does not alter
  the `ModelDeploymentScaler` Kubernetes CRD wire schema.
- In EPD deployments, `Encode` exposes no token or latency histograms; `TTFT` reflects the `Prefill`
  engine's histogram, and `Encode` scales strictly via fixed ratio coupling.

### Boundaries

- **Always:**
  - Keep explicit manual capacity fields authoritative when `capacityPrior` is omitted or all
    required fields are supplied explicitly.
  - Fall back to `mode: aggregated` only when `mode: disaggregated` returns zero matches
    (`IsProfileNotFound`), never on ambiguity, identity/config mismatch, infeasible SLOs, or custom
    `ConfigMap` read errors.
  - Require fixed connected ratios (`min == max`) and both `TTFT` and `ITL` targets for catalog-backed
    PD/EPD resolution.
  - Run `make lint` (followed by `make generate` and `make generate priors` when API/catalog files
    change) and `make lint docs`.
- **Ask first:**
  - Any change to public `api/worker/v1` or storage `api/worker/v1alpha1` Kubernetes CRD schemas.
  - Live cluster/hardware benchmark execution (`gpustack-operator-e2e`,
    `gpustack-operator-benchmark`).
- **Never:**
  - Reintroduce isolated `role: Prefill` or `role: Decode` catalog entries or divide throughput by
    assumed GPU service-time fractions.
  - Inject synthetic zero token or latency readings when role metrics are `Missing` or `Stale`.
  - Bypass Kueue `PendingCohort` admission rollback or single-replica retirement draining.
  - Put spec task IDs, decorative symbols, or emojis in Go comments.

### Risks and Mitigations

- **Unbalanced P/D GPU service-time share under Priority 2 `mode: aggregated` fallback saturates one
  stage below `C_unit`** → Prefer measured `mode: disaggregated` curves (Priority 1) whenever
  present, frame Priority 2 `C_unit` explicitly as an initial GPU-time-balanced estimate with
  `DefaultHeadroom` (`0.75`), and trigger `+1` whole-unit scale-up whenever `TTFT(Prefill)` or
  `ITL(Decode)` exceeds `1.2x` target.
- **Upstream InferenceX `disagg: true` rows vary worker counts within a benchmark sweep or report
  inconsistent stage rates** → Enforce `<= 2%` throughput conservation and integer worker GPU
  divisibility per row, include `(prefill_num_workers, decode_num_workers, framework)` in
  `sweepKey()`, and normalize each sweep by `gcd(m_P, m_D)`.
- **Serial single-replica retirement across a multi-replica unit step (`2P:6D -> 1P:3D`) aborts on a
  non-first ordinal or races between reservations** → Hold further scale-downs while any non-terminal
  surplus Pod (`ordinal >= current[role]`) exists, and match `Aborted` reservations across the full
  removed range `current[role] <= ordinal < PreScaleDownVector[role]`.
- **Priority 1 <-> Priority 2 transition or ratio edit reuses incompatible predictor/stabilization
  units** → Include resolution `Mode`, canonical unit vector `u`, member configurations, and stage
  evidence in `CompatibilityFingerprint` so structural changes reset predictor and stabilization
  state.

## Design Details

### Commands

Local verification runs on `macOS darwin/arm64` with `Go 1.27.3` (`GODEBUG= CGO_ENABLED=1`) and
contacts no Kubernetes cluster:

```bash
# Run priors, generator, algorithm, controller, simulation, and webhook suites
go test ./pkg/worker/scaling/priors ./pkg/worker/scaling/algorithm ./gen/priors/...
go test -run 'TestScalerConstraints|TestModelDeploymentScaler' ./pkg/worker/controllers/worker
go test -run 'TestModelDeploymentScalerDefault_CapacityPrior|TestModelDeploymentScalerValidate_CapacityPrior' ./pkg/worker/webhooks/worker

# Run Go formatter/linters, then regenerate API deepcopy/openapi and formatted priors.yaml
make lint
make generate
make generate priors

# Run documentation, spec, cross-reference, skill, and site gates
make lint docs
```

### Project Structure

```text
api/worker/v1alpha1/model_deployment_scaler.go     # Offline catalog document structs (+protobuf=false)
pkg/worker/scaling/priors/                         # Catalog schema, validation, resolution, and priors.yaml
gen/priors/                                        # InferenceX adapter, merger, retirement CLI
pkg/worker/scaling/algorithm/                      # Proportional, TokenThroughput, and Predictive algorithms
pkg/worker/controllers/worker/                     # Scaler prior binding, sampler, planner, constraints, retirement
pkg/worker/webhooks/worker/                        # ModelDeploymentScaler defaulting and admission validation
.agents/skills/gpustack-operator-priors/           # Priors maintenance skill and upstream provenance records
docs/modules/model-deployment/scaling.md           # Normative scaling and capacityPrior reference (<= 1000 lines)
docs/walkthroughs/model-deployment/                # Step-by-step serving and scaling walkthroughs
docs/README.md                                     # Shared documentation and owning-spec module index
```

### Code Style

Follow repository Go, Kubernetes, and testing conventions in
[development.md](../docs/contribute/development.md): `snake_case` Go filenames, explicit error
handling without panics, short plain comments without emojis or spec task IDs, and table-driven
deterministic tests with fake clients. Both `aggregated` and `disaggregated` catalog profiles measure
whole-system cache-missed total throughput (`input + output`) bounded by `[TTFT, ITL]`, matching
`RoleKindServer` unit demand in [token_throughput.go](../pkg/worker/scaling/algorithm/token_throughput.go).

### Implementation Plan

`T3` and `T5` are already implemented on branch. Once `T1` lands (`api/`, `priors/`, `gen/priors/`,
and synced `priors.yaml`), `T2` (controller + webhook) and `T4` (priors skill + scaling docs) own
disjoint paths and can run in parallel.

- [x] **T1 · Mode-keyed catalog schema (`aggregated` / `disaggregated`), `PerUnit` denominator, InferenceX adapter, and live `priors.yaml` sync**
  - **Blocked by:** None
  - **Owns:** `api/worker/v1alpha1/model_deployment_scaler.go`, `api/worker/v1alpha1/zz_generated.deepcopy.go`, `api/worker/v1alpha1/zz_generated.model_name.go`, `api/worker/zz_generated.openapi.go`, `pkg/worker/scaling/priors/catalog.go`, `pkg/worker/scaling/priors/catalog_yaml.go`, `pkg/worker/scaling/priors/shape.go`, `pkg/worker/scaling/priors/validate.go`, `pkg/worker/scaling/priors/resolve.go`, `pkg/worker/scaling/priors/refusal.go`, `pkg/worker/scaling/priors/identity.go`, `pkg/worker/scaling/priors/priors.yaml`, `pkg/worker/scaling/priors/catalog_test.go`, `pkg/worker/scaling/priors/catalog_yaml_test.go`, `pkg/worker/scaling/priors/launch_parameters_test.go`, `pkg/worker/scaling/priors/priors_test.go`, `pkg/worker/scaling/priors/resolve_test.go`, `pkg/worker/scaling/priors/identity_test.go`, `gen/priors/adapter/inferencex.go`, `gen/priors/adapter/inferencex_test.go`, `gen/priors/main.go`, `gen/priors/main_test.go`
  - **Gate:** review
  - **Deliverables:** Replace `Role` with `Mode` (`aggregated` | `disaggregated`) plus optional `Prefill`/`Decode` stage blocks (`ModelDeploymentScalerCapacityPriorStage`) on `ModelDeploymentScalerCapacityPriorProfile` (`Topology` pointer with `omitempty`); update `priors` (`ModeAggregated`/`ModeDisaggregated`, `DenominatorPerUnit`, `MaxProfiles = 2048`, `MaxCatalogBytes = 4 MiB`, `minimaxm2.5` canonical model name, shape matching/sorting, validation, and resolution); upgrade `gen/priors` (`inferencex.go` removing `<= 8` GPU cap and `is_multinode`/`disagg` filters, enforcing `<= 2%` throughput conservation and worker GPU divisibility, normalizing minimal units via `gcd`, and updating `main.go --retire --mode`); rebuild `pkg/worker/scaling/priors/priors.yaml` from the 12 verified InferenceX upstream datasets and regenerate deepcopy/openapi/priors artifacts.
  - **Verify:**
    - `go test ./pkg/worker/scaling/priors ./gen/priors/... ./api/worker/...`
  - **Expected:** Exit code `0`; `LoadEmbedded()` loads all synced `aggregated` and `disaggregated` profiles cleanly, and all `priors`, `gen/priors`, and `api/worker` unit tests pass.

- [x] **T2 · Whole-unit `disaggregated` prior resolution with `aggregated` fallback across controller and webhooks**
  - **Blocked by:** T1
  - **Owns:** `pkg/worker/controllers/worker/model_deployment_scaler_priors.go`, `pkg/worker/controllers/worker/model_deployment_scaler_priors_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_identity_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_lifecycle.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_lifecycle_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_sim_ratio_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_sim_experiment_test.go`, `pkg/worker/webhooks/worker/model_deployment_scaler.go`, `pkg/worker/webhooks/worker/model_deployment_scaler_prior_test.go`
  - **Gate:** review
  - **Deliverables:** Update controller prior resolution to map `Server` targets to `mode: aggregated`, refuse single-half `Prefill`-only/`Decode`-only catalog lookups while preserving explicit manual capacities, and resolve PD/EPD targets as whole units via Priority 1 (`mode: disaggregated` with `pdScale = gcd(u_P, u_D)`, both stages' identity/router/config checks, and fail-closed on ambiguity/mismatch/SLO infeasibility) followed by Priority 2 (`mode: aggregated` whole-unit `C_unit` fallback when `IsProfileNotFound`); update controller, lifecycle, causal simulation, and webhook prior test suites to the `mode: aggregated | disaggregated` contract.
  - **Verify:**
    - `go test -run 'TestModelDeploymentScalerPrior|TestModelDeploymentScalerOperatorEnvGate|TestModelDeploymentScalerCapacityPrior' ./pkg/worker/controllers/worker`
    - `go test -run 'TestScalerConstraints|TestModelDeploymentScaler' ./pkg/worker/controllers/worker`
    - `go test -run 'TestModelDeploymentScalerDefault_CapacityPrior|TestModelDeploymentScalerValidate_CapacityPrior' ./pkg/worker/webhooks/worker`
  - **Expected:** Exit code `0` across controller, simulation, and webhook test suites.

- [x] **T3 · Whole-unit runtime observation, partial-signal latency guard, atomic unit pacing, and retirement recovery**
  - **Blocked by:** None
  - **Owns:** `pkg/worker/scaling/algorithm/token_throughput.go`, `pkg/worker/scaling/algorithm/predictive.go`, `pkg/worker/scaling/algorithm/algorithm_test.go`, `pkg/worker/controllers/worker/model_deployment_scaler_sampler.go`, `pkg/worker/controllers/worker/model_deployment_scaler_planner.go`, `pkg/worker/controllers/worker/model_deployment_scaler_constraints.go`, `pkg/worker/controllers/worker/model_deployment_scaler_constraints_test.go`
  - **Gate:** review
  - **Deliverables:** Preserve `> 1.2x` latency scale-up when `ReadyReplicas > 0` and `LatencyState == SignalFresh` under missing/stale throughput without recording bogus zero demand samples; aggregate `Prefill` + `Decode` (+ `Encode` readiness) into a whole-unit `RoleKindServer` observation with propagated observation timestamp; add `WholeUnit` atomic integer unit pacing in `SolveModelDeploymentScalerConstraints`; hold scale-downs while live non-terminal surplus Pods remain and restore `PreScaleDownVector` when any retired `(role, ordinal)` aborts.
  - **Verify:**
    - `go test ./pkg/worker/scaling/algorithm`
    - `go test -run 'TestScalerConstraints' ./pkg/worker/controllers/worker`
  - **Expected:** Exit code `0`.

- [x] **T4 · Priors skill provenance records, scaling module reference, scaling/EPD walkthroughs, and shared index updates**
  - **Blocked by:** T1
  - **Owns:** `.agents/skills/gpustack-operator-priors/SKILL.md`, `.agents/skills/gpustack-operator-priors/references/records.md`, `docs/modules/model-deployment/scaling.md`, `docs/walkthroughs/model-deployment/scaling.md`, `docs/walkthroughs/model-deployment/encode-prefill-decode.md`, `docs/README.md`
  - **Deliverables:** Update `gpustack-operator-priors` `SKILL.md` and `references/records.md` for `mode: aggregated | disaggregated`, multi-node (`> 8` GPU) and `disagg: true` conservation/divisibility rules, `--mode` retirement flags, and the 12 synced InferenceX dataset URLs, row counts, and SHA-256 digests; update `docs/modules/model-deployment/scaling.md` (`<= 1000` lines), `docs/walkthroughs/model-deployment/scaling.md`, and `docs/README.md` to document `mode: aggregated | disaggregated`, Priority 1 measured `disaggregated` resolution versus Priority 2 `aggregated` whole-unit fallback, `catalogRef` defaulting (`priors.yaml` with `priors.json` legacy fallback), and `ModelDeployment` identity/precision annotations; remove the internal `/models` historical note in `docs/walkthroughs/model-deployment/encode-prefill-decode.md` and align Step 2 `--served-model-name` values to `qwen2-5-vl-7b-instruct`.
  - **Verify:**
    - `make lint docs`
  - **Expected:** Exit code `0` across `check-docs.sh`, `check-specs.sh`, `check-crossrefs.sh`, `hack/check/skills.sh`, and `make site`.

- [x] **T5 · Logical `spec.model.name` alignment across remaining model-deployment walkthroughs**
  - **Blocked by:** None
  - **Owns:** `docs/walkthroughs/model-deployment/prefill-decode.md`, `docs/walkthroughs/model-deployment/elastic-ep.md`, `docs/walkthroughs/model-deployment/external-dp.md`, `docs/walkthroughs/model-deployment/multi-host.md`
  - **Deliverables:** Replace `spec.model.name: /models` in reusable `ModelDeployment` manifests and matching `curl` payloads with consistent logical model identifiers and preserve `/var/lib/gpustack/model` container mount paths.
  - **Verify:**
    - `make lint docs`
  - **Expected:** Exit code `0`, and zero `name: /models` occurrences remain under `docs/`.

### Test Plan

- [x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

Update synthetic catalog fixtures across `pkg/worker/scaling/priors`, `pkg/worker/controllers/worker`,
and `pkg/worker/webhooks/worker` from `role: Server | Prefill | Decode` to
`mode: aggregated | disaggregated`, replacing legacy single-half `Prefill`-only catalog lookups with
coupled `Prefill`+`Decode` fixtures (`mode: disaggregated` and `mode: aggregated` fallback) plus
explicit fail-closed tests for malformed single-half catalog lookups.

#### Unit tests

- `pkg/worker/scaling/priors` and `gen/priors`:
  - `mode: aggregated` and `mode: disaggregated` YAML/JSON strict parsing, canonical marshaling
    (omitting empty `topology` on `disaggregated` and omitting `prefill`/`decode` on `aggregated`),
    `gcd(prefill.replicas, decode.replicas) == 1` enforcement, and `PerGPU`/`PerReplica`/`PerUnit`/`WholeSystem`
    reconstruction.
  - InferenceX adapter: `> 8` GPU (`16`/`32`/`64`) aggregated sweeps, `disagg: true` framework
    mapping (`dynamo-vllm`, `llmd-vllm`, `vllm-disagg`, `dynamo-sglang`, `mori-sglang`), `gcd`
    minimal-unit reduction (such as `2P:4D -> 1P:2D` and irreducible `2P:3D`), conservation and
    worker-divisibility rejection, `MiniMax-M2.5` canonicalization, and `--retire --mode`.
- `pkg/worker/controllers/worker` (`model_deployment_scaler_prior*`):
  - Exact rational unit-vector solver (`u`, `[k_min, k_max]`) and reduced `pdScale = gcd(u_P, u_D)`
    scaling (including `1E:2P:4D` matching `1P:2D` with `pdScale = 2`).
  - Priority 1 (`mode: disaggregated`) versus Priority 2 (`mode: aggregated` fallback) decision
    matrix: exact `disaggregated` match, `IsProfileNotFound` fallback to `aggregated`, heterogeneous
    P/D quantization fallback, fail-closed on `disaggregated` ambiguity / config mismatch / SLO
    infeasibility, malformed single-half refusal, manual bypass, and custom `ConfigMap` non-fallback.
  - Synthesized `vLLM`, `vLLM-Ascend`, and `SGLang` PD/EPD connector environment gates and secret
    value redaction.
- `pkg/worker/scaling/algorithm` and `model_deployment_scaler_constraints`:
  - Partial-signal `TTFT`/`ITL` latency hysteresis, zero synthetic-zero pollution, whole-unit
    readiness, atomic `±1` unit pacing, explicit step caps, grace windows, and abort cooldown.

#### Integration tests

- `pkg/worker/controllers/worker`:
  - Closed-loop causal controller simulations (`TokenThroughput` and `Predictive`) on `1P:3D` and
    `1E:1P:2D` (`1x -> 2x -> 1x`) under both Priority 1 (`disaggregated`) and Priority 2
    (`aggregated` fallback) priors.
  - Multi-replica serial retirement hold and abort recovery on first, middle, and final retired
    ordinals across restarts, plus Priority 1 <-> Priority 2 fingerprint invalidation.
- `pkg/worker/webhooks/worker`:
  - End-to-end `Default` + `ValidateCreate` + `ValidateUpdate` coverage for single-role `Server` and
    coupled PD/EPD `capacityPrior` (both `mode: disaggregated` and `mode: aggregated` fallback),
    malformed single-half rejection, missing-target warning, `CREATE` refusal versus `UPDATE`
    repair/Shadow entry, and the four-case `catalogRef` defaulting/validation matrix (`priors.yaml` /
    `priors.json`).

#### e2e tests

Local fake-client controller, simulation, webhook, Go lint (`make lint`), codegen (`make generate`
and `make generate priors`), and documentation lint (`make lint docs`) suites gate implementation
completion without requiring cluster GPUs. Real-cluster reconcile/webhook verification
(`gpustack-operator-e2e`) and live multi-GPU serving/scaling qualification
(`gpustack-operator-benchmark`) remain explicit opt-in gates on user-supplied cluster hardware.

## Alternatives

- **Keep `role: Server | Prefill | Decode` in `priors.yaml`:** Isolated `Prefill` and `Decode`
  latency/throughput curves cannot be measured independently of their counterpart stage, and
  splitting coupled runs discards the measured `P:D` replica ratio and cross-stage topology pairing.
  Replaced with `mode: aggregated | disaggregated`.
- **Synthesize per-role `Prefill` and `Decode` capacities from `aggregated` priors via GPU-share
  division:** Assumes the user's `P:D` GPU split equals the unmeasured compute time split inside a
  collocated benchmark and breaks minority-role L1 projection on asymmetric ratios. Rejected.
- **Require `mode: disaggregated` without `mode: aggregated` fallback:** Even after importing all
  180 upstream InferenceX `disaggregated` sweeps, arbitrary user `P:D` ratios or heterogeneous
  accelerator pairings may have no exact `disaggregated` match; Priority 2 `mode: aggregated`
  whole-unit fallback + live `TTFT`/`ITL` guards preserves broad out-of-the-box coverage.

## Open Questions

None.
