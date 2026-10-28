---
name: gpustack-operator-priors
description: "Maintain pkg/worker/scaling/priors/priors.yaml: fetch and merge public benchmark curves, retire outdated profiles by model/engine/hardware, and record source checksums in references/records.md."
---

# Maintain the capacity prior catalog

`pkg/worker/scaling/priors/priors.yaml` is the offline capacity catalog shipped with
`ModelDeploymentScaler`. At runtime, the operator reads only the embedded file or a referenced
same-namespace `ConfigMap`; it never fetches, benchmarks or profiles live traffic. This skill
describes how to change the catalog and record provenance:

- `SKILL.md` (this file) — the workflows, the identity rules and the schema.
- `references/records.md` — the provenance ledger: upstream URLs, capture dates, SHA-256
  checksums, alignment rules and retirement/refusal records.

Include each catalog change and its records entry in the same change. Either one without the
other is incomplete.

## The catalog file

The committed catalog is a single `priors.yaml` with human-editable profiles. The parser
also accepts JSON. Profiles carry no `digest`, `sources[]`, `servingConfig` or `workload`
blocks — qualification and provenance live in the generator and in `references/records.md`,
which keeps user `ConfigMap` catalogs concise. Each profile is:

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScalerCapacityPriorCatalog
profiles:
  - role: Server                         # Server, Prefill or Decode
    model: deepseek-ai/DeepSeek-R1-0528  # verified Hugging Face org/repo ID
    quantization: fp8                    # canonical lowercase token
    engine: vllm                         # vllm or sglang
    hardware: amd-mi325x                 # canonical Family identifier
    topology:                            # measured replica shape
      gpusPerReplica: 8
      tensorParallel: 8
    points:                              # measured load points, fixed units
      - concurrency: 4
        totalTokensPerSecond: 2130       # per-replica tokens/s
        inputTokensPerSecond: 1840       # per-replica tokens/s
        outputTokensPerSecond: 290       # per-replica tokens/s
        ttftMilliseconds:                # any canonical p<N> quantile in (0, 100]
          p50: 320
          p90: 410
          p95: 455
          p99: 512
        itlMilliseconds:                 # per token
          p50: 18
          p90: 24
          p95: 27
          p99: 33
```

- A profile is identified by its normalized `(role, model, quantization, engine, hardware,
  topology)` tuple — never by a separate `id` field. `ParseCatalog` fails closed when two
  entries normalize to the same tuple; `--merge` instead upserts one canonical sweep
  deterministically and sorts profiles by role, model, quantization, engine, hardware and
  parallelism widths.
- `ttftMilliseconds` and `itlMilliseconds` accept any canonical `p<N>` percentile key in
  `(0, 100]` (for example `p25`, `p50`, `p70`, `p90`, `p95`, `p99`, `p99.9`). Latency quantiles
  a source did not publish stay absent.
- Throughput units are per-replica `tokens/s`; latencies are milliseconds, and
  `itlMilliseconds` is per token. Point values above are illustrative shapes, not measurements.

## Canonical identity rules

**Model** — `model` is always a verified Hugging Face `<org>/<repo>` repository ID, for example
`deepseek-ai/DeepSeek-R1-0528`, `Qwen/Qwen3.5-397B-A17B`, `meta-llama/Llama-3.3-70B-Instruct`,
`openai/gpt-oss-120b`, `moonshotai/Kimi-K2.5`, `moonshotai/Kimi-K3`, `MiniMaxAI/MiniMax-M2.5`,
`MiniMaxAI/MiniMax-M3`, `zai-org/GLM-5`, `zai-org/GLM-5.2`, `deepseek-ai/DeepSeek-V4-Pro` or
`Qwen/Qwen3.8-Flash-Next`. Adapt to it during import, never record the alias:

- Source-internal slugs (`dsr1`, `qwen3.5`, `llama70b`) resolve to their repository.
- ModelScope mirror orgs (`ZhipuAI/`, `MiniMax/`, `LLM-Research/`) map to the Hugging Face org.
- Vendor-repackaged checkpoints (`amd/DeepSeek-R1-0528-MXFP4`,
  `nvidia/Llama-3.3-70B-Instruct-FP8`) map to the upstream repository; the repackaging shows up
  in the quantization token, not the model name.

A source with no verified upstream repository is refused, not guessed.

**Hardware** — `hardware` is the canonical `Family` identifier from
[Instance Type Unit Resources](../../../docs/reference/instance-type-unit-resources.md)
(`pkg/nodefeature/unit_resources_preset.yaml`): `amd-mi325x`, `amd-mi300x`, `amd-mi350x`,
`amd-mi355x`, `nvidia-h100`, `nvidia-h200`, `nvidia-b200`, `nvidia-a100-80gb`, and so on.
Natural names (`mi325x`, `AMD Instinct MI325X`) normalize by the same Family rules.

**Quantization** — canonical lowercase tokens: `fp8`, `fp4`, `bf16`, `fp16`, `int8`, `int4`,
`awq`, `gptq`. Source spellings fold (`nvfp4`/`mxfp4` to `fp4`, `bfloat16` to `bf16`).

## Workflow 1 — Fetch and merge

1. Sync the live upstream availability and fetch the active models into a temporary directory
   (for example `$TMPDIR/inferencex-upstream/`), which is deleted after the merge. Never commit
   upstream snapshots — `gen/priors/snapshots/` stays out of git; the checkout ships only
   `priors.yaml`.

   ```bash
   TMP_UP="${TMPDIR:-/tmp}/inferencex-upstream"
   mkdir -p "$TMP_UP"
   curl -fsSL --compressed https://inferencex.semianalysis.com/api/v1/availability > "$TMP_UP/availability.json"
   # then, for each active model <Model> the availability endpoint reports:
   curl -fsSL --compressed "https://inferencex.semianalysis.com/api/v1/benchmarks?model=<Model>" > "$TMP_UP/<Model>.json"
   ```

2. Diff the fetched availability against the catalog before merging: any model, hardware family
   or data source that `priors.yaml` or `references/records.md` still carries but the live
   upstream has removed, deprecated or superseded is reported to the user proactively, with an
   offer to retire the affected profiles (Workflow 2). Merge only after that review.
3. Run `go run ./gen/priors --merge <path>` with the downloaded data. The merge upserts
   profiles by their normalized 6-tuple, selects one canonical sweep deterministically and
   refuses conflicting or unverifiable evidence.
4. Verify: `make generate priors` (deterministic re-format) and
   `go test ./pkg/worker/scaling/priors/...`.
5. Record the source in `references/records.md`: upstream URL, capture date, canonical model
   ID, row count and SHA-256 (`shasum -a 256 <file>`). One row per upstream dataset.

## Workflow 2 — Retire

Use the availability diff from Workflow 1 to find models, hardware families or datasets that
upstream has removed, deprecated or superseded but `priors.yaml` still carries. Report the affected
profiles and offer to retire them; never remove them silently.

Remove outdated or superseded profiles with `go run ./gen/priors --retire`, selecting by one or
more of `--role`, `--model`, `--quantization`, `--engine`, `--hardware`, and optionally `--tp`
or `--gpus` (for example `go run ./gen/priors --retire --model moonshotai/Kimi-K2.5 --hardware
nvidia-h100`). All selectors normalize by the same identity rules. Then:

1. Run `make generate priors` to re-format and re-validate the file.
2. Log the retirement in `references/records.md`: what was removed, why, and the date.

## Workflow 3 — Validate and format

`make generate priors` (or `go run ./gen/priors` with no flags) normalizes identities, validates
schema and points, and formats `priors.yaml` deterministically. Run it before every commit that
touches the catalog; two consecutive runs must produce identical bytes.

## Verification

```bash
make generate priors
go test ./pkg/worker/scaling/priors/...
make lint docs
```

The catalog contract (qualification, eligibility, resolution) is documented in
[Model Deployment Scaling](../../../docs/modules/model-deployment/scaling.md) under
[Capacity priors](../../../docs/modules/model-deployment/scaling.md#capacity-priors); the
local-evaluation spec is
[specs/2026-10-26-inferencex-model-deployment-scaling.md](../../../specs/2026-10-26-inferencex-model-deployment-scaling.md).
