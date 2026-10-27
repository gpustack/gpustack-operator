# Spec: ModelDeploymentScaler Capacity Priors and Closed-Loop Evaluation

Status: Building
Blocked on: T7 hardware qualification requires a user-provided environment and explicit benchmark execution instruction.
Type: Feature

## Summary

Add an opt-in, source-neutral capacity catalog to `ModelDeploymentScaler`. Use measured throughput
under a declared workload and latency constraint to fill missing per-role capacity settings. Keep
explicit settings and the existing manual mode unchanged. InferenceX is one possible source;
configuration recipes, single-request speed, and synthetic fixtures are not measured capacity.

Acquire and qualify every prior offline. Publish reviewed catalog artifacts before deployment;
the operator reads only the embedded artifact or the configured ConfigMap. It never queries
InferenceX, MLPerf or another benchmark service, runs a benchmark, or creates priors from live
traffic. Runtime workload metrics remain inputs to the existing scaling algorithm, separately
from this offline capacity input.

Build a deterministic local simulation that exercises the real sampler, algorithms and planner.
Use it to measure response time, sensitivity, stability, resource use and failure handling before
requesting hardware. Then compare fixed manual settings with catalog-derived settings on a small
model that the available hardware can run with at least two independent replicas. Large-model,
multi-node and prefill/decode qualification are separate extensions.

The original proposal's online capacity formula and latency-floor clamp are not ready for Active
use. Keep online estimation as a local experiment until its inputs and held-out tests support it.
Do not add a public `Auto` policy or enable online learning by default in this implementation.
The user authorized `my-build team` on 2026-10-26 for the local implementation. Hardware
qualification remains a separate, explicit execution gate.

## Motivation and feasibility

The design was checked against checkout `ddd2578fb3275ed991fb5f32890f27e76e43ebbc` on 2026-10-26.
The local environment has Go 1.27.3; the module uses Kubernetes 0.36.5, controller-runtime 0.24.1
and Kueue 0.18.11. These facts establish the implementation seams:

| Current evidence | Consequence for this design |
|---|---|
| [TokenThroughput](../pkg/worker/scaling/algorithm/token_throughput.go) divides processed token rate by a configured per-replica target. [Predictive](../pkg/worker/scaling/algorithm/predictive.go) has separate forecast capacities and embeds a TokenThroughput fallback. | Resolve every missing capacity field independently. A supplied forecast capacity must not overwrite a supplied fallback target. |
| The [sampler](../pkg/worker/controllers/worker/model_deployment_scaler_sampler.go) reads cache-missed prompt tokens, output tokens and windowed latency histograms. It emits processed throughput, not offered traffic. | Idle or underloaded throughput does not identify maximum capacity. Cached input throughput from an external dashboard is not the same numerator. |
| [RoleObservation](../pkg/worker/scaling/algorithm/algorithm.go) has no request-length distribution or request sample count. Readiness starts from answering Pods. | Workload identity must be declared. Online length buckets and multi-rank capacity normalization need additional evidence. |
| The sampler has clock/scrape injection; the reconciler clock and `plan` method are private to `package worker`. | Put controller simulation tests in that package and use the real code. A separate simulator must not copy the planner. |
| [Public API](../api/worker/v1/model_deployment_scaler.go) defines its resource over the storage type; [storage API](../api/worker/v1alpha1/model_deployment_scaler.go) admits prediction horizons of 30–600 seconds. | Regenerate API artifacts and test both routes. Do not add a duplicate hand-maintained public struct or silently cap horizons at 120 seconds. |
| The [webhook](../pkg/worker/webhooks/worker/model_deployment_scaler.go) permits a missing target to support GitOps ordering. | Preserve deferred target validation and distinguish NotFound from API read failures. |

The committed [Scaling Benchmark](../docs/modules/model-deployment/scaling-benchmark.md) is the
historical measurement source. It records lower ramp median TTFT for Predictive PD than
Proportional PD (467.906 versus 2540.853 ms), with different capacity at ramp start. Its text
cooldown was 90 seconds while downward stabilization was 300 seconds. It covers one node and
limited runs. Those results motivate controlled comparisons; they do not establish a general
algorithm ranking, a full settling time, or a universal overshoot bound. The removed raw report
is not an input or prerequisite for this work.

**Assessment:** static priors and local closed-loop evaluation are feasible. Automatic discovery
of safe capacity from sparse passive samples is unproven. The first release therefore separates
catalog resolution from online estimation and separates local correctness from hardware results.

## Goals and boundaries

- G1: Fill missing capacity settings from attributable measurements with explicit units,
  statistics, workload, serving configuration and replica denominators.
- G2: Preserve manual settings, GitOps ordering, Shadow behavior, role bounds, pacing, ratios,
  pending-cohort handling and retirement safety.
- G3: Evaluate the real control path locally with deterministic traffic, independent plant
  behavior, injected faults and quantitative acceptance criteria.
- G4: Prepare a repeatable manual-versus-automated hardware comparison that can start with a
  small model. Keep unavailable hardware out of the local development dependency graph.
- G5: Expose why a prior was selected or refused, and distinguish assumed capacity from measured
  local performance.

This work changes role replica counts, not TP, PP or EP width inside a running replica. Elastic EP
and custom serving commands keep their existing scaler restrictions. Encode remains ratio-coupled.
It does not promise cluster autoscaling, universal hardware extrapolation, a production SLO
guarantee, or automatic optimization of the inference engine's launch flags.

## Design

### 1. Measurements and source eligibility

Each catalog profile has a versioned schema, stable ID and content digest. It records:

- Source URL, source revision or run ID, capture date, raw artifact digest, adapter version,
  applicable attribution/license and original measurement lineage. A republished run is not an
  independent measurement. Publicly readable data is not automatically licensed for redistribution.
- Exact model and weight variant/revision, tokenizer identity, quantization recipe, GPU model,
  memory and form factor, engine build/image, role, topology, GPU count per scaling replica and
  measured replica count.
- TP, PP, DP/local DP, context parallelism and EP settings where applicable; router, KV-cache
  dtype, offload, prefix caching, speculative decoding and relevant launch parameters. Unsupported
  fields remain explicit. An omitted performance-critical setting is not a wildcard.
- Workload ID and revision, input/output length distributions, modality, cache behavior, arrival
  model, requested and achieved concurrency, warm-up and measured duration. Agent concurrency,
  active requests, internal batch size and DP rank count are distinct fields.
- For each measured load point: successful and failed request counts, token totals/rates,
  denominator, token-count convention, latency metric, unit, quantile, sample count and censoring
  rules. Missing measurements remain missing.

Keep model variants distinct. Do not strip `Instruct`, quantization or revision suffixes. Alias
mapping requires evidence of identical identity; common architecture is insufficient. Do not map
an unspecified H100 to SXM by default, merge all four-bit formats, interpolate across TP values,
or estimate cross-card capacity from a bandwidth ratio.

The reviewed public sources have different uses:

| Source | Useful information | Import boundary |
|---|---|---|
| [InferenceX](https://inferencex.semianalysis.com/zh/inference) and its [benchmark implementation](https://github.com/SemiAnalysisAI/InferenceX/tree/65a6dd1978720f65099fea8c4d5ac5eddfdfd8d7) | Throughput/interactivity curves, load points, serving configuration and run artifacts. | Read underlying points and artifacts, including non-frontier points. A frontier can combine different serving configurations; it is not one deployment's capacity curve. Coverage of large models does not imply coverage of the local small model. |
| [AA-AgentPerf methodology](https://artificialanalysis.ai/methodology/agentperf) and [configurations](https://artificialanalysis.ai/hardware-inference-stack/configs) | Agentic throughput versus per-request output speed; native tokenizer counts and disclosed serving settings. Current charts default to P25 output speed. | Full test trajectories are private. Total input throughput includes KV-cache hits. Published curves are useful references; raw-data access, reuse rights and matching latency statistics must be checked before an adapter is accepted. |
| [NVIDIA inference performance](https://developer.nvidia.com/deep-learning-performance-training-inference/ai-inference) and [public JSON](https://developer.nvidia.com/search-data/nv_inference_benchmark.json) | Downloadable concurrency, topology, token throughput, TTFT, TPOT and interactivity fields. | The snapshot inspected on 2026-10-26 (SHA256 `af85101e389e5b7b479c10ed2d1b3660d268dc31d031895b2bceaf5e89219351`) has 519 rows and no P95/P99 TTFT or ITL, image, revision, source URL or cache-missed count fields. Units, percentile semantics and run lineage need adapter validation; do not convert these summaries into fabricated P95 observations. This is evidence about that checked snapshot, not a universal judgment about the source. |
| [MLPerf Inference results v6.1](https://github.com/mlcommons/inference_results_v6.1) and [rules](https://github.com/mlcommons/inference_policies/blob/master/inference_rules.adoc) | Public result files, system descriptions and throughput under versioned latency constraints; includes smaller-model benchmarks. | Use the matching release's Server/Interactive rules and logs. Offline throughput has no equivalent interactive SLO. TPOT constraints do not establish an ITL quantile. An inspected NVIDIA B300x8 DeepSeek-R1 Server run publishes TTFT and TPOT P95 but no direct ITL and no same-config multi-load boundary. |
| [MLPerf Endpoints methodology](https://docs.mlcommons.org/endpoints-submission-docs/understand/what-is-mlperf-endpoints/) | Multi-concurrency throughput versus per-user output speed, with disclosed systems and datasets. | Current v1.0 reports 90th-percentile TTFT and interactivity derived from 90th-percentile TPOT. This is closer to the desired curve shape, but it does not establish P95 ITL or our cache-missed numerator. Validate raw fields and exact recipes before import. |
| [ROCm ATOM dashboard](https://rocm.github.io/ATOM/benchmark-dashboard/) | Nightly performance, throughput/latency views and public dashboard data. | Validate per-run fields, workload and versions before import. AMD results cannot seed H100 capacity. Vendor pages that cite InferenceX retain that lineage. An inspected official run (Qwen3.5-397B FP8, 4×MI355X, TP4) shares image/model/recipe provenance and disables prefix caching, but reports precision `null`, `measurement_valid: false`, an inter-token metric measured over collector chunk gaps, and an engine this operator does not support — so its points cannot become production profiles today, and its artifacts expire in 15 days (pin originals before any qualification). Evidence about checked runs, not a blanket exclusion of the source. |
| [vLLM recipes](https://github.com/vllm-project/recipes/tree/1259f9ff08ccc56938c012266c9eb1e1036cde0f) | Supported launch parameters, hardware requirements, precision and tuning instructions. | Some benchmark output is placeholder text. A supported or verified recipe is configuration evidence, not an SLO capacity measurement. |
| [SGLang cookbook](https://github.com/sgl-project/sglang/tree/79701a2549c3cf9935d41d753637e7443d016bfd/docs/cookbook) and its [snippets](https://github.com/sgl-project/sglang/tree/79701a2549c3cf9935d41d753637e7443d016bfd/docs/src/snippets) | Launch configurations and selected benchmark records with workload, concurrency and speed fields. | Follow imported snippets. Some entries provide accuracy only; some AgentX results originate from InferenceX. Neither is a new independent capacity observation. |

InferenceX is the primary source targeted for offline catalog construction. An inspected
DeepSeek-R1-0528 sweep on eight MI325X accelerators was first refused: its collector records gaps
between streamed messages, and the available evidence did not establish that each message contains
exactly one token, so the reported gap percentile could not be asserted to be per-token ITL. The
refused data and the ineligible MLPerf sample were removed from the repository at that time.

The public-source alignment contract resolves that defect deterministically: chunk-gap latency
quantiles normalize to per-token milliseconds by the measured chunk-to-token ratio (falling back to
TPOT quantiles when ITL is absent), and bracketing P90/P99 quantiles interpolate to the requested
quantile. Under that contract the sweep qualifies, and the imported verified InferenceX sweeps —
including the `deepseek-ai/DeepSeek-R1-0528` `amd-mi325x` sweep (`fp8`, `tp=8`) — form the
embedded catalog. Canonical accelerator identity has a single source of truth:
`pkg/nodefeature`'s `unit_resources_preset.yaml` `Family` matcher
(`docs/reference/instance-type-unit-resources.md`) resolves every accelerator to its canonical
`<vendor>-<chip>[-<variant>]` family identifier (`amd-mi325x`, `nvidia-h100`, `nvidia-a100-80gb`,
and so on) for both public-source import and runtime target matching. The original refusal record
is kept for provenance. This finding applied to the inspected sweep, not every InferenceX result.

Benchmarks are offline data sources, not decision components in this operator. Any future accepted
measurement must pass offline qualification and review, including its metric semantics, workload,
runtime configuration, topology and source evidence. The operator does not fetch priors online.

These sources were inspected on 2026-10-26. Moving web pages are discovery references; any future
import must pin the actual data snapshot and applicable methodology. Implement only adapters with
eligible data. A list of candidate sources is not a commitment to import every provider.

### 2. Concurrency, SLOs and capacity selection

`tok/s/user` is a per-request decode-speed metric. It does not mean the benchmark has one
concurrent user. For a single request with consistent first-token and token-count conventions,
50 output tokens/s corresponds to 20 ms mean time per output token. This reciprocal does not
convert a median speed, a provider's interactivity label, or mean TPOT into P95 ITL. TTFT is a
separate constraint. Under speculative decoding, token delivery bursts make the distinction
especially important.

The selected InferenceX URL's `i_optimal=1` controls the optimal-points filter, not concurrency.
On 2026-10-26, the [Qwen3.8-Flash-Next data endpoint](https://inferencex.semianalysis.com/api/v1/benchmarks?model=Qwen3.8-Flash-Next&view=inference&sequence=agentic-traces)
returned H100/SGLang/FP8 AgentX points with `conc` values 1, 4, 8, 12 and 16, dated 2026-08-27.
The five points share a recipe fingerprint and a [source run](https://github.com/SemiAnalysisAI/InferenceX/actions/runs/33039903416/attempts/1).
This establishes multi-concurrency coverage, not automatic eligibility for our resolver. The API
also exposes an unresolved denominator discrepancy: these aggregated rows report 64 in both
role GPU-count fields, while total throughput divided by per-GPU throughput is approximately 8.
An adapter must resolve the actual GPU/replica topology from the run artifacts and quarantine
conflicting metadata; it must not silently choose a multiplier. Conflicting run artifacts also
require refusal unless a pinned, reproducible correction resolves them. Refused source evidence
may remain in the offline source report, but must not become an eligible profile. Check metric
semantics as well:
Agent concurrency need not equal active decode requests when trajectories include tool delays
and subagents.

A concurrency-one point characterizes low-load latency and speed. It cannot by itself establish
saturated throughput or capacity under multiple concurrent requests. Do not multiply its speed by
the expected user count: batching, queueing, KV-cache pressure and the input/output mix are
nonlinear. A low-load point may remain as a diagnostic, but cannot become a production capacity
prior merely by applying a safety factor.

For one fixed serving configuration and workload, let `Q(c)` be measured aggregate throughput at
load point `c`, and `L(c)` its measured latency statistics. Capacity selection is:

```text
eligible = points with compatible workload, units, denominator and sufficient load coverage
feasible = eligible points satisfying every required latency constraint
C_seed = headroom * max(Q(point) / measured_scaling_replicas for point in feasible)
```

The initial headroom is 0.75, configurable in `(0, 1]`. It is an engineering margin, not a proof
that queue length or replica count stays below a fixed multiple. Require at least two distinct
measured load levels and evidence of the capacity boundary: an adjacent higher-load SLO failure
or an explicit saturation measurement. Without that boundary, label the result a measured lower
bound and keep it out of automatic capacity resolution. Sparse data may therefore produce no
eligible prior. Do not synthesize a missing point to make lookup succeed.

The runtime does not need to have exactly the same instantaneous concurrency as one benchmark
point. The prior represents a measured load envelope and its feasible boundary. The configured
profile must cover the intended workload and operating range. Declared changes outside that
envelope invalidate it; passive aggregate metrics cannot certify that every live request remains
inside it. Test bursts and out-of-envelope demand explicitly and report this limitation.

Use the actual selected topology, not the best point from a different TP/DP configuration at each
load. Fixed-concurrency clients are closed-loop: slower service reduces the offered request rate.
They do not establish how an open-loop arrival burst behaves. Preserve this distinction in the
catalog and evaluate both load models in simulation and hardware tests.

The resolver requires TTFT and ITL for Server, TTFT for Prefill and ITL for Decode, at the
configured `latencyQuantile` (P95 by default). The same quantile applies to online histogram
observations and prior resolution, including the manual and Predictive paths. A directly measured
higher quantile of the same metric that meets the target may be used as a conservative bound,
labeled as such, and counts toward measured load coverage. A higher quantile that misses the
target does not prove the requested lower quantile misses it. A lower quantile, a different metric or a fitted
distribution cannot fill a missing statistic. Diagnostic points without the required measured
statistics do not count toward load coverage or boundary evidence. They do not invalidate an
otherwise qualified curve with sufficient independent points. Parse units from a known source schema, never from
the numeric magnitude. Preserve raw values and conversion metadata.

The public-source alignment contract sits on top of these rules. Every import pins its upstream
revision and artifact digests; identity normalizes on one yardstick on both sides — canonical model
names with variant suffixes preserved and quantization labels folded to canonical tokens, canonical
hardware slugs carrying standard memory and form factor. Chunk-gap latency quantiles normalize to
per-token milliseconds by the measured chunk-to-token ratio when that ratio exceeds 1.05, falling
back to TPOT quantiles when ITL is absent; bracketing quantiles interpolate the requested quantile
linearly; a single higher quantile remains a conservative bound. PerGPU denominators reconstruct
one replica through `gpusPerReplica`, and aggregated rows whose total-to-per-GPU ratio contradicts
the declared GPU count are refused. A source without a recorded container environment imports as
incomplete; deployments may add volumes, environment variables and non-conflicting launch flags,
while explicit conflicts with recorded core flags are refused. The canonical model identity is
the verified Hugging Face `<org>/<repo>` repository: source-internal slugs (`dsr1`, `qwen3.5`,
`llama70b`), ModelScope mirror orgs (`ZhipuAI/`, `MiniMax/`, `LLM-Research/`) and
vendor-repackaged checkpoints (`amd/DeepSeek-R1-0528-MXFP4`, `nvidia/Llama-3.3-70B-Instruct-FP8`)
adapt to it (`deepseek-ai/DeepSeek-R1-0528`, `meta-llama/Llama-3.3-70B-Instruct`,
`Qwen/Qwen3.5-397B-A17B`), and a source with no verified upstream repository is refused.

The numerator must match the existing sampler: Prefill uses cache-missed input tokens/s, Decode
uses output tokens/s, and Server uses their sum. A source that only publishes input including
cache hits is ineligible unless its measured misses can be recovered. Per-chip throughput must
be reconstructed for the complete scaling replica using the source's actual denominator.
Disaggregated whole-system totals cannot be assigned to Prefill and Decode independently.

### 3. Minimal opt-in API and exact resolution

Add `spec.algorithm.capacityPrior` for TokenThroughput and Predictive only. This is a new API;
the earlier draft's `Auto`/`BuiltinCatalog` policy names were never shipped, and the field
carries no policy switch: an absent `capacityPrior` is manual, and a present one opts the
deployment into offline catalog resolution. The contract is:

| Field | Contract |
|---|---|
| `capacityPrior` | Absent: manual mode with the existing required fields and defaults. Present (an object): the offline catalog may resolve missing capacity fields; explicit fields always win. Failures fail closed with no online fallback. `Proportional` rejects a present `capacityPrior`. |
| `catalogRef` | Optional same-namespace ConfigMap name and key; key defaults to `priors.yaml` (the parser accepts YAML and JSON). When present, this is the sole catalog source. Otherwise use the embedded catalog. No silent fallback or merge. |
| `headroom` | Positive quantity no greater than 1; default 0.75. Applies only to catalog-derived values. The default is an initial configurable engineering margin — not measured or optimized. Local mock rounds and later user-provided hardware experiments should compare margins and performance; no warranty. |

The committed catalog is one `pkg/worker/scaling/priors/priors.yaml` of lean profiles — `role`,
`model`, `quantization`, `engine`, `hardware`, `topology` and `points[]` with fixed units. A
profile is identified by its normalized `(role, model, quantization, engine, hardware, topology)`
tuple; any `Server`, `Prefill` or `Decode` role that omits a capacity field under a present
`capacityPrior` resolves automatically by that tuple, and `Encode` never resolves from the
catalog. `ParseCatalog` fails closed on duplicate tuples, naming both entries, while
`gen/priors --merge` upserts one canonical sweep deterministically and sorts profiles by role,
model, quantization, engine, hardware and parallelism widths. Upstream URLs, capture dates,
artifact digests and refusal records live in
`.agents/skills/gpustack-operator-priors/references/records.md`, not in the catalog. The
runtime still resolves from the one configured catalog (embedded or referenced) and performs no
runtime fetch or merge.

Keep explicit TTFT/ITL target fields. Require the applicable latency targets when a role relies on
catalog capacity; do not introduce arbitrary Strict/Standard/Relaxed presets. When missing capacity
needs Catalog resolution, do not fill an omitted required latency target from legacy defaults.
Catalog lookup does
not mutate the user's SLO or write derived values back into spec. A configured model profile is
also an operator declaration of the expected workload; the current sampler cannot infer it.

Match profile identity against the rendered deployment, including artifact revision when present,
engine image and shared precision/parallelism parsing. Preserve exact quantization schemes and
flag precedence, aliases, `--flag=value`, repeated flags and `--` termination. Unknown parser
results cannot become permissive defaults. Reuse the controller's interpretation in admission.

Where identity is absent from the typed deployment, use explicit declarations on the target
ModelDeployment: `worker.gpustack.ai/model-id`, `worker.gpustack.ai/hardware-id` and
`worker.gpustack.ai/precision`; Prefill and Decode may override the hardware declaration with
`worker.gpustack.ai/prefill-hardware-id` and `worker.gpustack.ai/decode-hardware-id`. IDs resolve to
exact catalog entries, including model revision and hardware memory/form factor. Reject conflicts
with information that can be established from the deployment. A declaration is operator-provided
identity, not proof that the operator inspected the weights or physically benchmarked the node.

Compute model IDs as `model:` plus the canonical digest of the complete catalog model identity,
and hardware IDs as `hardware:` plus the digest of the complete hardware identity. This binds
revision/tokenizer/quantization and memory/form factor without a separate alias registry. For
initial Catalog support, require an effective engine image pinned by `@sha256` that matches the
catalog image digest. Record the source build label as provenance rather than treating it as the
image digest. A source build label alone cannot establish target identity. Preserve source
revisions separately and require complete evidence for the effective engine environment.

Initial Catalog support is limited to managed, single-serving-Pod replicas with a proven metric
denominator. A Pod may use TP across several local GPUs. Multi-Pod/External-DP groups require
tests that count complete logical replicas and deduplicate rank metrics before becoming eligible.
Unsupported configurations retain the manual path; missing profiles do not weaken compatibility.

Resolution is per required capacity field:

| Configuration | Result |
|---|---|
| Prior absent | Existing required fields and defaults, unchanged. |
| Every required capacity field explicit | Use those fields unchanged; no catalog, annotations or prior-specific SLO requirement. |
| Predictive has only its forecast capacity explicit | Preserve it; resolve the missing fallback target from the profile. |
| Predictive has only its fallback target explicit | Preserve it; resolve the missing forecast capacity from the profile. |
| Both Predictive values absent | Fill both from the same eligible seed; report both field sources. |
| A required value remains unresolved | Refuse the new binding/plan as described below. Never substitute zero or borrow another role's capacity. |

An explicitly supplied invalid value remains an admission error. It is not treated as absent.
Positive but different manual fallback/forecast settings are permitted as today and remain visible.
Build effective parameters separately for each role; do not mutate the shared algorithm spec
while resolving a role. Existing explicit fields remain scoped by role kind as today. Distinct
roles of the same kind may use distinct profiles only through their independent effective values.

### 4. Admission, status and invalidation

Structural validation applies on CREATE and UPDATE: legal policies and algorithms, valid positive
quantities, unique role mappings, reference syntax, role bounds, ratios and existing restrictions.
External resolution has a separate policy:

| Situation | Admission and reconciliation |
|---|---|
| Target does not exist | Admit with a warning; retain the existing TargetNotFound behavior and validate after the target appears. |
| Target exists on CREATE; a required prior cannot resolve | Reject with an actionable field error naming the role/profile and missing evidence. |
| Required prior becomes unresolved on UPDATE | Admit structurally valid updates with a warning so suspension and repairs remain possible; stop new replica commits until resolution succeeds. |
| An API read fails | Return the read error. Do not reinterpret it as an absent target or a catalog miss. |
| A referenced ConfigMap changes/disappears at runtime | Re-resolve and stop new commits on failure; preserve existing retirement and pending-cohort recovery. Never use a stale prior to issue a new growth/shrink decision. |

Use a relevant ConfigMap watch or indexed mapping plus periodic re-resolution. Existing scaler
predicates ignore arbitrary annotation-only updates; target identity changes and catalog updates
must have explicit tests. Parse only the configured key. Limit its decoded content to 512 KiB,
256 profiles and 128 measured points per profile; reject an oversized catalog without truncation.
Reject duplicate IDs, unsupported schema versions and non-finite/invalid measurements. Do not fetch
URLs from the controller or webhook. Recheck resolution before an Active commit.

Add per-role `status.resolvedCapacityPriors`, recording profile/source IDs and digest, compatibility
fingerprint, metric/quantile basis, workload envelope, matched denominator, headroom, seed, effective
value and source of each capacity field. Use bounded structured failure reasons, including
`CapacityPriorUnresolved`, `InsufficientLoadCoverage` and `IncompatibleMetric`. Publish a condition
for prior readiness; do not claim MetricsReady solely because the catalog resolved. Explain a block
through the existing planning conditions. Avoid unbounded events or model IDs in metric labels.

The compatibility fingerprint includes target UID, role configuration, model, engine, hardware,
quantization, topology, workload, catalog digest and relevant SLO/settings. It excludes ordinary
replica-count changes. On an incompatible change, discard prior-dependent predictor state before
using the new capacity, including status-restored state. Reset sampling windows on membership or
counter changes without treating every replica change as a new serving configuration. Deletion,
target recreation, suspend/resume, controller restart and source replacement need lifecycle tests.

### 5. Online calibration and control changes: experiment first

The proposed `processedTPS / readyReplicas * sqrt(SLO / latency)` estimator is not adopted. Under
low load, processed TPS mainly follows arrivals. Queueing, batch formation and prompt/output mix
can change latency without identifying service capacity. Concurrency at least four, or latency
within a fixed fraction of SLO, does not establish saturation. A global drift ratio must not fill
an unseen workload bucket.

The local experiment may compare candidate estimators, but must first specify observable inputs:
aligned window boundaries, latency sample counts, complete logical-replica coverage, request-length
signals, backlog and membership/counter continuity. Reject stale, partial, duplicate, idle and
transition samples. Bound storage per scaler UID/role and define TTL, minimum evidence, outlier
rules and reset semantics. Preserve the catalog seed when observations cannot identify capacity.
Synthetic observations cannot silently add inputs the production sampler does not have.

Hold out plant shapes, traffic seeds and capacity errors from tuning. Compare the candidate with
the static seed and fixed manual controls. Any estimator proposed for Active use needs a separate
recorded gate: explicit API/defaults, observability changes, reset/persistence contract, sensitivity
results and hardware qualification. This gate may conclude that online calibration is deferred.

Do not assert that a single-request P95 is at least 1.15 times P50. A low-load measurement is an
empirical diagnostic, not a universal physical floor. Keep the requested SLO unchanged and report
infeasibility evidence with its measured conditions. Existing bounds/pacing still limit commits.

Keep the current horizon range and default. Evaluate forecast overshoot and scale-down hysteresis
locally before selecting new opt-in controls. A bound on forecast-driven growth must not suppress
the separate latency/panic path, and a latency change must cover both TokenThroughput and
Predictive. Raw recommendations and committed vectors are separate measurements. Do not claim
that a configured maxStep proves the raw forecast is well behaved.

### 6. Local closed-loop evaluation

Use a discrete-event fake clock, reproducible traffic and independent fake engine/scheduler
behavior. Add pure traffic and plant helpers under proposed `pkg/worker/scaling/sim`; keep tests
that invoke the private sampler and planner in `pkg/worker/controllers/worker`, `package worker`.
The helpers must not import that controller package or duplicate its algorithm/planning rules.

Feed vLLM/SGLang-style counter and histogram exposition through the real scrape hook. Drive the
real recommendation, stabilization, pacing, ratio, pending-cohort and status paths with fake clients.
Simulate admission/boot delay, quota and retirement observations through those clients. Mark these
as fake lifecycle observations: they do not validate a real Kueue scheduler, router or drain hook.

The plant owns arrival queues, service, batching, cache effects, capacity degradation and token
completion. It must not use the learner's current estimate to decide its own service capacity.
Maintain independent truth for offered requests, completed requests, latency and SLO-compliant
goodput. Test fixtures are labeled synthetic and stay under testdata; production catalogs never
load them. Profiles fitted from one plant are evaluated against other plants.

| Scenario | Required assertion or measurement |
|---|---|
| Idle and steady load with noise | No invalid writes or sustained replica chatter; compare settled replica-seconds and latency against fixed manual controls. |
| Step up/down, ramp and repeated bursts | Time to recommendation, commit and ready capacity; peak/area of excess replicas; queue recovery and full cooldown. |
| Length/cache shifts and mixed workloads | Detect sensitivity to changed effective work; show when the selected profile is out of scope. |
| Capacity prior errors | Sweep 0.5x, 0.75x, 1x, 1.5x and 2x of plant capacity; measure stability and SLO/resource trade-offs, without claiming these are real hardware errors. |
| Boot delay and resource exhaustion | Sweep zero, short and multi-minute delays; bounds, pending timeout, rollback, opposite-role rebalance and no impossible readiness guarantee. |
| Missing/partial metrics and resets | No calibration from invalid samples; preserve missing versus observed zero; no unsafe downscale from incomplete coverage. |
| Configuration/catalog changes and restarts | Correct invalidation, no stale status restoration, no state leaking between target UIDs. |
| Retirement and renewed load | Exercise planner response to in-flight/aborted retirement and renewed demand; real draining remains a hardware gate. |
| Open-loop versus fixed concurrency | Expose coordinated-omission effects; a slower closed-loop client must not make an overloaded system look SLO-compliant. |

Define deterministic tuning seeds and held-out seeds in fixtures before choosing candidate
parameters. Evaluate offered load, failed/timed-out requests, request latency distributions,
goodput, replica-seconds, raw/committed/ready counts, changes per unit time and retirement outcomes.
Do not average quantiles across requests or independent runs. Record both simulated seconds and
test runtime; wall-clock speed is a measured optimization, not a promised two-second race-test gate.

All safety invariants are hard gates: finite valid outputs, role bounds and pacing, ratio rules,
Shadow with no desired-state mutation, correct missing-data handling and bounded state. Performance
gates compare identical traffic and available resources. Freeze baseline tolerances before tuning:
for example, a latency improvement must report its replica cost and a cost improvement its SLO
effect. Use an oracle only when its feasibility and optimality under the same startup/quota
constraints are established. Otherwise name the static comparison arm and report paired excess;
do not describe it as optimal-oracle excess.
A fixed four-replica limit or a 15-second recovery requirement is invalid across arbitrary plants.

#### Local experiment decision (2026-10-26)

Defer production online capacity estimation. The frozen test candidate uses observed processed
TPS per ready replica and the smaller square-root SLO/latency ratio, with fresh-sample,
membership, pending/retirement, outlier and expiry guards. It remains test-only. Its seed is
fitted offline on the baseline plant before opening held-out cases: measured raw capacity
399.444 tokens/s, effective capacity 299.583 tokens/s after 0.75 headroom. The manual control is
300 tokens/s. The original baseline and held-out seeds were not tuned after results were read.

The final experiment contains 48 synthetic rows. Representative paired results compare the
candidate with the static resolved arm at the same seed factor. Cost below means requested-object
replica-seconds, including pending and retiring objects, not billed GPU time.

| Synthetic scenario / seed factor | Static SLO fraction | Candidate SLO fraction | Static / candidate replica-seconds | Candidate cost change |
|---|---|---|---|---|
| Steady noise / 1 | 99.86% | 99.86% | 990 / 1800 | +81.82% |
| Held-out bursts / 1 | 67.30% | 95.86% | 2330 / 2250 | -3.43% |
| Held-out length/cache shift / 1 | 57.33% | 74.83% | 2200 / 2400 | +9.09% |
| Held-out length/cache shift / 1.5 | 57.33% | 74.83% | 2130 / 2400 | +12.68% |
| Held-out length/cache shift / 2 | 57.33% | 74.83% | 2120 / 2400 | +13.21% |

The last two rows exceed the frozen 10% paired cost tolerance; the steady-load regression also
fails it. The burst improvement includes two ready replicas before the load event versus one in
the static arm. An event-boundary recommendation at zero elapsed seconds reads a preceding
window and does not establish zero causal reaction time. At factor 1, commit/readiness delays
are 10/40 seconds in both burst arms. The excess-area comparison uses the named manual control;
no optimal oracle was qualified.

A low-load counterexample gives identical complete sampler observations for independent plants
with capacities 400 and 800 tokens/s. This demonstrates non-identifiability for these inputs,
not impossibility of every learner. With two fixed replicas, an open-loop trace offers 2400
requests and fails 1924, while a closed-loop trace offers only 1200 and fails none; comparing
successful-request latency alone would hide that demand difference. Failed and unfinished
requests are included in the SLO denominator.

Synthetic histogram counts were corrected to whole token gaps with consistent completion
tolerance, and the candidate's timestamp watermark was made monotone. Independent narrow
probes close both defects; rerunning the frozen experiment after those review fixes leaves all
48 rows unchanged. The executable evidence is the controller experiment test and the frozen
`pkg/worker/scaling/sim/testdata/{baseline,passive_experiment}.json` fixtures. Cross-plant and
workload-shift transfers here test sensitivity; they do not claim production Catalog compatibility.
No hardware result or production learning benefit is established by these tests.

### 7. Hardware evaluation after local gates

Start with a supported small text model, a Server role and two or more independently schedulable
replicas. Select the exact model, engine image, precision, context envelope and resource shape when
the user provides hardware. Verify memory, stable serving and simultaneous replica readiness before
measuring scaling. Local mock tests do not require an InferenceX-covered model.

Qwen3.8-Flash-Next is an optional larger-model follow-up. Its [model documentation](https://github.com/QwenLM/Qwen3.8-Flash-Next/blob/69885871a64393807d988b27b1b5e380e8f28526/README.md)
describes a 125B main model plus a 51B embedding component. Its [vLLM recipe](https://github.com/vllm-project/recipes/blob/4b1c04e76fe9ac3ca8b7e541dd423e7d5f38dc60/recipes/models/Qwen/Qwen3.8-Flash-Next.yaml)
includes an eight-H100 FP8 configuration with expert parallelism and specific offload/kernel
settings. This is configuration guidance, not a qualified GPUStack scaling profile.

One eight-H100 node can validate replica scaling only if at least two complete replicas fit and
run together. An eight-GPU-per-replica configuration leaves no room for 1-to-2 scaling on that
node; two such nodes supply two replicas. A four-GPU shape on one node must be qualified, including
host-memory/offload pressure and interconnect contention. Two nodes are needed for cross-node
failure/placement tests; they are not required for the first small-model single-node experiment.
A Prefill/Decode pair also needs spare capacity beyond its minimum pair to test growth.

Preemptible instances can host exploratory runs. Record interruption and restart/cache state;
an interrupted run is invalid for the primary steady comparison and is analyzed separately as a
fault-injection run. The owner supplies the environment later; this spec authorizes no rental,
deployment or benchmark execution.

Use three primary comparison arms: frozen existing manual settings, separately tuned manual
settings, and Catalog settings. Add a calibrated arm only after the online gate passes. Compare
within each algorithm first so algorithm choice and automatic configuration are not confounded.
Keep workload, bounds, horizon, pacing, start replicas, image, topology and cache conditions equal.
Use the same load trace and at least three valid repeats with alternating/randomized arm order.
Separate cold-start and warm-start results and report run-level spread.

Qualify the local model's load curve with independent concurrency/rate sweeps before selecting its
prior. Reserve different traces/runs for evaluation; do not fit capacity to the same observations
used to claim improvement. If public data does not match, measured local data supplies the profile.
The initial sweep is part of hardware qualification, not a prerequisite for local implementation.

Improve the existing [case-5 harness](../.agents/skills/gpustack-operator-benchmark/cases/case-5-autoscaling.sh)
before reuse. Its summary is currently assembled inline; the generic table renderer has no
`--self-test` interface. Add a dedicated report parser with fixture tests. Capture raw/effective
recommendations, desired/ready counts, metrics and freshness, source/fingerprint, forecast, panic,
quota/pending state, retirement, request failures, engine image and workload seed with timestamps.
Missing metrics must serialize as null with a reason, not as zero.

Create a fresh target and scaler UID per arm or prove complete reset. Deletion/reset timeout must
fail the round; wait for children and quota to be released, retirement to clear, and exact expected
role readiness. Start sampling before traffic. Run cooldown for at least the configured downward
stabilization plus retirement/startup allowances and measure the actual return to baseline.

## Implementation plan

The local build for T1–T6 is complete. Checkboxes record committed work. The user delegated routine
implementation and review decisions to the coordinator; normal review gates use that authority.
T7 still needs the explicit hardware gate. Shared ownership paths serialize overlapping writers.
The current execution boundary is local fake-client, simulation and executable-mock verification.
Stop before any real-cluster validation, including preflight probes; that stage requires a new
explicit user instruction. Primary coding and mutual review use the existing mcode and qwen
contexts; agy handles complex coding questions, and Codex performs the final review.

- [x] **T1 · Establish the real-controller local simulation and baseline**
  - Blocked by: None.
  - Owns: `pkg/worker/scaling/sim/**`, `pkg/worker/controllers/worker/model_deployment_scaler_sim*_test.go`, controller simulation testdata.
  - Gate: review.
  - Acceptance: Deterministic traffic/plant fixtures exercise actual sampler and planner; manual baselines, coverage faults and startup/quota delays have quantitative results. Synthetic truth is independent of capacity estimates. Freeze comparison tolerances and held-out cases before candidate tuning.
  - Verify: `CGO_ENABLED=1 go test -race ./pkg/worker/scaling/sim/... ./pkg/worker/controllers/worker/...` after the new helper package exists.

- [x] **T2 · Deliver one offline catalog adapter and resolver**
  - Blocked by: None.
  - Owns: `gen/priors/**`, `pkg/worker/scaling/priors/**`, `hack/generate.sh`.
  - Gate: review.
  - Acceptance: Versioned profiles, exact matching, load-coverage/quantile checks, source attribution and deterministic generation. At least one licensed pinned real source fixture proves import; synthetic fixtures remain test-only. Missing eligible production data produces an empty catalog or explicit refusal, never fabricated entries. Ordinary generation uses local snapshots and performs no network fetch.
  - Verify: `go test ./gen/priors/... ./pkg/worker/scaling/priors/...`; `make generate priors` twice with identical output; `make lint`. These package/target commands apply after T2 creates them.

- [x] **T3 · Integrate Catalog policy through API, admission and reconciliation**
  - Blocked by: T1, T2.
  - Owns: `api/worker/**/model_deployment_scaler*`, related generated API/CRD/protobuf artifacts, `pkg/worker/controllers/worker/model_deployment_scaler*.go`, shared deployment identity/parser helpers, `pkg/worker/webhooks/worker/model_deployment_scaler*.go`, scaler proxy tests.
  - Gate: review.
  - Preparation: Additive API declarations and serialization tests may run independently against this approved contract. Runtime integration and its completion gate still depend on accepted T1/T2 results. The API writer owns the type declarations; the integration writer owns controller/admission code.
  - Acceptance: The API/resolution matrix, typed failures, ConfigMap updates, fingerprint invalidation, status and logical-replica eligibility work through the public route and controller. Manual, partial override, absent target, suspension and Shadow contracts remain intact. Local comparison uses production resolution without exposing test data.
  - Verify: `make generate`; `CGO_ENABLED=1 go test -race ./api/... ./pkg/worker/scaling/algorithm/... ./pkg/worker/scaling/priors/... ./pkg/worker/extensionapis/... ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/...`; `make lint`.

- [x] **T4 · Evaluate online estimation and control-policy candidates locally**
  - Blocked by: T1, T2.
  - Owns: simulation experiments/testdata under `pkg/worker/scaling/sim/**`, controller simulation tests, this spec's experiment decision record.
  - Gate: review.
  - Acceptance: Compare static and candidate policies on held-out plants and traffic; identify missing real inputs. Record sensitivity, stability, reaction time, SLO and resource trade-offs. Decide whether to defer online calibration. No public Auto field or production behavior change is authorized by an experiment result alone; an accepted extension must first amend its API and acceptance contract here.
  - Verify: `CGO_ENABLED=1 go test -race ./pkg/worker/scaling/sim/... ./pkg/worker/controllers/worker/...`; `make lint docs` for the recorded decision.

- [x] **T5 · Prepare reproducible hardware rounds and reports locally**
  - Blocked by: T3.
  - Owns: `.agents/skills/gpustack-operator-benchmark/cases/case-5-autoscaling.sh`, its autoscaling manifests, a dedicated comparison helper, report parser and fixtures under that skill's `scripts/`.
  - Gate: review.
  - Preparation: The offline parser and fixture-tested harness construction may run against frozen API/artifact contracts while T3 review proceeds. Final integration and completion still depend on accepted T3 behavior and status fields.
  - Acceptance: Configurable small model/resource shape, controlled comparison arms, complete resets, fail-closed prerequisites and null-aware trace/report handling. For Predictive comparisons, both forecast and fallback capacities vary between the manual arms and are omitted in the full-Catalog arm; noncapacity settings remain equal. This benchmark restriction does not remove public API support for partial overrides. Fixture tests cover interrupted runs, malformed requests and timestamps, absent fields, missing or mismatched per-sample UIDs and incomplete cooldown. Preparing scripts does not run a cluster experiment.
  - Verify: `bash -n .agents/skills/gpustack-operator-benchmark/cases/case-5-autoscaling.sh`; `PYTHONDONTWRITEBYTECODE=1 python3 -B -m unittest discover -s .agents/skills/gpustack-operator-benchmark/scripts -p 'test_autoscaling_*.py'` after its tests exist; `make lint agents-shell`; `make lint docs` if manifests/documentation change.

- [x] **T6 · Document the implemented catalog contract and local evidence**
  - Blocked by: T3, T4, T5.
  - Owns: `docs/modules/model-deployment/scaling.md`, `docs/walkthroughs/model-deployment/scaling.md`, `docs/README.md`, this spec's status/evidence.
  - Preparation: Draft against frozen API contracts and accepted synthetic evidence; verify examples against final T3/T5 source before completion.
  - Acceptance: Manual/Catalog/partial override examples, exact matching, concurrency coverage, source provenance and unresolved behavior match implementation. Label local results synthetic and hardware qualification pending. Do not rewrite historical benchmark conclusions as new measurements.
  - Verify: `make lint docs`.

- [ ] **T7 · Qualify on user-provided hardware and publish the bounded comparison**
  - Blocked by: T5, T6; user-provided environment and explicit benchmark execution instruction.
  - Owns: measured profile additions and `docs/modules/model-deployment/scaling-benchmark.md` with its shared index entry.
  - Gate: review.
  - Acceptance: Local model load sweep plus held-out manual/Catalog comparison, at least three valid repeats, complete growth and retirement, metric provenance and limitations. Record zero/negative improvements honestly. Multi-node, large-model, P/D and preemption results remain separate qualifications.
  - Verify: Explicit `gpustack-operator-benchmark` execution on the supplied environment, followed by `make lint docs` and the offline catalog checks for any accepted profile.

T1 and T2 can proceed independently. T4 is a bounded experiment and does not block T3's static
integration. T7 does not block local development, but must pass before claiming measured hardware
benefits. Automatic weekly source synchronization is deferred until one adapter and its offline
review process are qualified; a future fetch job must pin inputs and open a reviewable change.

## Test plan

- [x] Component owners review the API, algorithm and controller tests before implementation is complete.

Prerequisites: add the fake plant and deterministic simulation seams; retain current algorithm and
webhook regression tests. Existing Go conventions apply: table-driven data, shared execution flow,
semantic assertions and fake clients. No GPU is required for these tests.

Unit tests cover exact profile identity, explicit units, quantiles, missing/P50-only/TPOT-only data,
concurrency-one and unbounded-sweep refusal, cache-token denominators, per-GPU versus per-replica
normalization, aliases, parser conflicts, duplicate profile IDs and deterministic generation.
Include conflicting aggregate/per-GPU totals and topology fields as a rejected-source fixture.
No coverage percentage is claimed before the packages and tests exist.

Integration tests cover public/storage API parity; CREATE/UPDATE and absent-target behavior; each
manual-override combination; ConfigMap modification/deletion; fingerprint and state lifecycle;
partial metrics; and actual sampler/planner feedback under the scenario matrix. Test logical
replica accounting before widening Catalog support beyond a single serving Pod per replica.

End-to-end tests use the staged hardware plan after explicit invocation. Mocking scheduler and
retirement state cannot replace real admission, router visibility or graceful draining checks.

## Risks and decisions

| Risk | Decision and mitigation |
|---|---|
| Public benchmark does not cover the actual deployment | Exact profiles and explicit refusal. A measured local profile can be added after the small-model sweep. |
| Single-user speed mistaken for multi-user capacity | Require a load curve and boundary evidence; preserve concurrency and arrival semantics. |
| Prior appears precise but omits cache/workload/quantile differences | Retain metric provenance and reject incompatible or missing values. Do not manufacture P95. |
| Low-load online feedback drives capacity downward | No production online calibration in the first release; evaluate identifiability and held-out behavior first. |
| Simulation favors its own model | Independent plant truth, multiple plant shapes and held-out seeds; hardware gate remains explicit. |
| Silent behavior change for existing users | Nil capacityPrior preserves current validation and controls. No horizon clamp, SLO rewrite or automatic Auto default. |
| Catalog replacement restores incompatible predictor state | Fingerprint invalidation covers both memory and status restoration. Controller rechecks before committing. |
| Hardware test confounds algorithm with capacity or warm-up | Equal start/resource conditions, within-algorithm arms, repeated runs and separate cold/warm/preemption reports. |

The exact small model, resource shape and hardware access are intentionally selected at T7.
They are not open prerequisites for T1–T6. Whether online calibration merits a later Active API
is deferred by the T4 decision: the passive candidate failed the frozen cost gates. Local synthetic
results do not qualify hardware capacity or establish production benefits.

## Verification and handoff

Local implementation checks use macOS/arm64 with Go 1.27.3 and `GODEBUG= CGO_ENABLED=1`.
The removed `GODEBUG=gotypesalias=0` option is not used. Tests exercise real controller code with
fake Kubernetes clients and independent simulated traffic and plants; executable mocks cover
benchmark orchestration. None of these checks contacts a real cluster.

T1–T4 evidence includes the API, conversion/storage, admission, algorithm, catalog, controller
and simulation suites. The broader race run passed before the final bounded fixes; focused
race and ordinary controller suites then passed on the final T3 source, including prior lifecycle,
semantic Quantity equality, steady-state status suppression and the real-controller simulations.
The final independent review reran focused public/storage API, webhook, domain and controller
tests. This is local contract evidence; it does not prove live admission, quota release or draining.

`make generate` completed in an isolated checkout with the generator's required repository path.
Its 16 generated outputs were copied into this worktree and verified byte-for-byte against the
generation manifest. The offline catalog generation was deterministic across two runs at that
earlier checkpoint. The MLPerf fixture and the examined upstream snapshot directories have since
been removed from the tree: the checkout ships only `pkg/worker/scaling/priors/priors.yaml`, and
upstream evidence pins (URLs, capture dates, SHA-256 digests) plus refusal records live in
`.agents/skills/gpustack-operator-priors/references/records.md`. The InferenceX importer passed
its evidence checks under the public-source alignment contract, and the embedded catalog carries
the imported verified InferenceX sweeps.

The required local gates are `make lint`, `make lint agents-shell`, `make lint docs`, the dedicated
`test_autoscaling_*.py` fixture suite and `git diff --check`. Keep Python bytecode disabled with
`PYTHONDONTWRITEBYTECODE=1` or `python3 -B`. Preserve the pinned upstream summary's original
trailing whitespace and SHA; the branch-wide whitespace check excludes only that raw fixture.
The final combined benchmark suite passed all 18 test methods in 204.397 seconds, including
executable-mock lifecycle, cleanup, interruption, UID replacement, Predictive comparisons and
malformed evidence. The three lint targets passed. Independent Codex review reported no remaining
high-confidence Standards or Spec findings after the four final benchmark fixes were verified.
These results cover the local build; T7 remains unexecuted, including real-cluster preflight.

Keep this spec in `specs/` and register its current state in the Model Deployment module map.
The user invoked `my-build team` on 2026-10-26 after the design review and relocation. Finish
local implementation and review before handoff; keep T7 pending until its explicit hardware gate.
