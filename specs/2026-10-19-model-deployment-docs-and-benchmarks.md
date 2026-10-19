# Spec: Model Deployment Documentation, Navigation, and Live Benchmarks

Status: Shipped
Type: Feature

## Summary

The Model Deployment documentation, walkthroughs, and site navigation have five gaps. First, the
Encode, Prefill and Decode (EPD) pages show `--enforce-eager` on the `encode` role without explaining
why an encoder-only instance defaults to eager mode while `prefill`, `decode`, and `server` roles
keep CUDA graphs enabled. Second, three walkthrough pages contain `Validation record` or
`Validation summary` sections with run-specific binary checksums, personal image namespaces,
and misplaced engine-specific notes, and walkthrough headings do not follow a uniform
`Summary` / `Step N:` / `Limitation` / `Troubleshooting` contract. Third, `site/data/navigation.yaml`
still labels the EPD benchmark `EPD benchmark`, mixes capitalization across secondary menus, and lists
`Model Deployment` pages in an flat, ungrouped order instead of nesting `Benchmark` under each serving
topology. Fourth, `docs/modules/model-deployment/` has no module guide for External Data Parallelism
(`External DP`). Fifth, `Prefill and decode`, `Elastic EP`, and `External DP` have no live benchmark
pages that prove when each capability helps and when a simpler deployment wins.

This spec resolves all five gaps. It documents the upstream vLLM `v0.30.0` `--enforce-eager` contract
for `RoleEncode` and removes unnecessary `--enforce-eager` flags from non-encoder roles. It removes
all `Validation record` and `Validation summary` sections, strips personal identifiers and run-pinning
digests, and standardizes all ten walkthroughs on `Summary`, `Step N:`, `Limitation`, and
`Troubleshooting`. It adds data-driven child nesting to the Hugo sidebar, standardizes secondary menu
capitalization, and reorders the `Model Deployment` menu. Finally, it adds the `External DP` module
guide and executes live Nebius GPU benchmarks for `Prefill and decode`, `Elastic EP`, and
`External DP` to publish evidence-backed benchmark pages.

## Motivation

### Goals

1. **Verify and document `--enforce-eager` on `RoleEncode`.** Confirm across GPUStack Operator,
   `vllm-project/vllm@v0.30.0`, `vllm-project/vllm-ascend@v0.19.1rc1`, and
   `sgl-project/sglang@cff88d6eac` why `pkg/worker/kvcache/inject/vllm_ec.go` defaults
   `--enforce-eager` on the vLLM encoder-cache producer alone, document why `Encode` runs no language
   model or decoder CUDA graphs, and ensure `Prefill`, `Decode`, and `Server` benchmark workloads keep
   default accelerator graph capture enabled.
2. **Eliminate personal identifiers and run-identity checksums from documentation.** Remove every
   personal registry namespace reference and run-pinning binary or container digest from `docs/`,
   delete `Validation record` and `Validation summary` sections from walkthroughs, and relocate the
   SGLang router front-door contract from the EPD walkthrough into the EPD module guide.
3. **Standardize every walkthrough's section layout.** Structure all ten walkthrough leaf pages under
   `docs/walkthroughs/` around `## Contents`, `## Summary`, `## Step N: ...`, `## Limitation`, and
   `## Troubleshooting` (plus the required footer), while staying within the 10-`##` section cap
   enforced by `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh`.
4. **Restructure the Hugo sidebar and standardize menu capitalization.** Support nested child pages
   (`+ Benchmark`) under leaf module guides in `site/layouts/partials/nav-section.html` and
   `site/data/navigation.yaml` without moving flat Markdown files in `docs/modules/model-deployment/`,
   rename `Deployment configuration` to `Configuration` and `EPD benchmark` to `Benchmark`, reorder
   `Model Deployment` menu entries, and enforce consistent capitalization across groups and secondary
   labels.
5. **Add the `External DP` module guide.** Publish `docs/modules/model-deployment/external-dp.md`
   covering how the operator derives External DP from role arguments, injects per-rank identity,
   selects all eligible rank endpoints in Services and Routers, and enforces admission boundaries.
6. **Publish three scenario-driven live benchmarks on Nebius.** Design and run live benchmarks on
   Nebius GPU clusters (`NVIDIA L40S` and `NVIDIA RTX PRO 6000`) for `Prefill and decode`,
   `Elastic EP`, and `External DP`. Each benchmark compares the feature against an equal-GPU baseline
   to show the exact workload regimes where the capability helps and where it loses.

### Non-Goals

1. **Removing `--enforce-eager` from `pkg/worker/kvcache/inject/vllm_ec.go` `DefaultedArgs`.** In
   `vllm-project/vllm@v0.30.0`, the upstream disaggregated encoder guide marks `--enforce-eager` as
   required on encoder instances, and the V1 `GPUModelRunner` (`ECExampleConnector`) otherwise runs
   empty language-model CUDA graph capture loops during startup. Because `DefaultedArgs` yields when a
   workload declares its own flag, keeping the producer-only default preserves compatibility.
2. **Modifying pinned upstream subchart image references under `deploy/`.** The `csi-s3-driver:0.43.7`
   image reference and maintainer notes under `deploy/gpustack-operator/chart/` belong to the vendored
   `csi-driver-s3` chart, not user-facing documentation.
3. **Converting leaf module guides into Hugo `_index.md` subdirectories.** Moving
   `docs/modules/model-deployment/*.md` into subdirectories would break relative links across `docs/`,
   `.agents/skills/`, and `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh` exemption paths, and would force an extra
   `Overview` item inside each collapsible section.
4. **Multi-node InfiniBand RDMA benchmarking on presets without GPU clustering.** Where the Nebius
   platform preset does not expose InfiniBand fabric attachment, benchmarks run on a single multi-GPU
   node over TCP/NVLink/PCIe and explicitly state what single-node measurement does and does not
   prove.
5. **Validating External DP inside a disaggregated `Prefill` or `Decode` role.** That combination is
   tracked separately under issue #754 and remains outside this spec's scope.

## Proposal

### User Stories

#### Story 1: Clear Encoder Flag Semantics for EPD Operators

As a platform engineer deploying a multimodal `ModelDeployment` with `encode`, `prefill`, and `decode`
roles, I want the EPD module guide and walkthrough to explain why `--enforce-eager` appears on the
`encode` Pod by default while `prefill` and `decode` run with accelerator graph capture enabled, so
that I do not mistakenly copy `--enforce-eager` onto `prefill` or `decode` roles and degrade token
generation throughput.

#### Story 2: Clean, Uniform Walkthroughs Without Build-Identity Noise

As a user following a GPUStack Operator walkthrough, I want every walkthrough to open with a concise
`Summary` of the problem and scenario, step through reproducible commands (`Step 1` ... `Step N`), and
close with `Limitation` and `Troubleshooting` sections free of personal image registries or one-off
binary SHA256 hashes, so that I can judge whether the feature fits my workload and run the steps on my
own cluster.

#### Story 3: Predictable Model Deployment Navigation and External DP Contract

As a reader browsing the documentation site, I want the `Model Deployment` sidebar to list topologies
in progressive order (`Overview`, `Configuration`, `Prefill and decode`, `Encode, prefill and decode`,
`Elastic EP`, `External DP`, followed by `Engine versions`, `Routing`, `Metrics`, `Status`, and
`Shutdown`) with each topology's `Benchmark` nested directly beneath it and consistent capitalization
across all menus, so that I can find both the architecture contract and the measured trade-offs in one
place.

#### Story 4: Scenario-Driven Benchmarks That Prove When a Capability Is Worth Enabling

As an inference architect choosing between a monolithic `Server` role and `Prefill and decode`,
`Elastic EP`, or `External DP`, I want live benchmark pages that compare equal GPU budgets across
short/long prompts, low/high concurrency, and scale-up windows, so that I know the exact crossover
point where enabling the capability beats a simpler deployment—and where it does not.

### Features

1. **EPD `--enforce-eager` Documentation Alignment**
   - Document in `docs/modules/model-deployment/encode-prefill-decode.md` and
     `docs/walkthroughs/model-deployment/encode-prefill-decode.md` why the operator defaults
     `--enforce-eager` only on `RoleEncode` for vLLM engines: an encoder-only instance runs
     `execute_mm_encoder` with no language model, no KV cache, and no sampler; vLLM `v0.30.0`
     disables vision encoder CUDA graphs by default (`cudagraph_mm_encoder=False`), `MMEncoderModelRunner`
     (`ECCPUConnector` with `VLLM_USE_V2_MODEL_RUNNER=1`) captures zero graphs (`capture_model() -> 0`),
     and the V1 `GPUModelRunner` (`ECExampleConnector`) requires `--enforce-eager` to skip empty
     language-model graph profiling loops.
   - Clarify that `Prefill`, `Decode`, and `Server` roles do not receive `--enforce-eager` from the
     operator and should keep default accelerator compilation and CUDA graph capture enabled in
     production and in the new benchmarks.

2. **Walkthrough Hygiene and Structural Standardization**
   - Delete `## Validation record` from `docs/walkthroughs/model-deployment/elastic-ep.md` and
     `docs/walkthroughs/model-deployment/external-dp.md`, and delete `## Validation summary` from
     `docs/walkthroughs/model-deployment/encode-prefill-decode.md`. This removes the personal registry
     namespace occurrence in `docs/` (`external-dp.md:325`) and all run-identity binary and image SHA256
     checksums, while preserving legitimate `ModelArtifact` contract digests in
     `docs/modules/model-delivery/artifact.md` and `docs/walkthroughs/model-delivery/prefetch.md`.
   - Delete `## Check the software identity` from `docs/walkthroughs/model-deployment/_index.md` and
     update `docs/walkthroughs/_index.md` so walkthrough indexes no longer promise run-identity digest
     tables.
   - Relocate the SGLang router front-door requirement (`status.endpoint` vs plain Service HTTP 400)
     from `docs/walkthroughs/model-deployment/encode-prefill-decode.md` into
     `docs/modules/model-deployment/encode-prefill-decode.md` under `### SGLang`, and update the
     cross-reference in `docs/modules/model-deployment/encode-prefill-decode-benchmark.md`.
   - Standardize all ten walkthrough leaf pages under `docs/walkthroughs/` so their `##` headings
     consist exclusively of `## Contents`, `## Summary`, `## Step 1: ...` through `## Step N: ...`,
     `## Limitation`, and `## Troubleshooting`.

3. **Hugo Sidebar Nesting, Ordering, and Capitalization**
   - Extend `site/data/navigation.yaml` with a `children:` map that associates parent page keys with
     ordered child page slugs:
     - `/docs/modules/model-deployment/prefill-decode/` -> `prefill-decode-benchmark`
     - `/docs/modules/model-deployment/encode-prefill-decode/` -> `encode-prefill-decode-benchmark`
     - `/docs/modules/model-deployment/elastic-ep/` -> `elastic-ep-benchmark`
     - `/docs/modules/model-deployment/external-dp/` -> `external-dp-benchmark`
   - Update `site/layouts/partials/ordered-pages.html` and `site/layouts/partials/nav-section.html`
     so pages claimed in `children:` are omitted from the parent section's top-level loop and rendered
     inside an indented `<div class="nav-children">` block immediately below their parent leaf link.
   - Update `site/data/navigation.yaml` `order:` for `/docs/modules/model-deployment/` to:
     `deployment`, `prefill-decode`, `encode-prefill-decode`, `elastic-ep`, `external-dp`,
     `engine-versions`, `routing`, `metrics`, `status`, `shutdown` (with `Overview` rendered
     automatically by `nav-section.html` from `docs/modules/model-deployment/_index.md`).
   - Rename `/docs/modules/model-deployment/deployment/` label from `Deployment configuration` to
     `Configuration`, and set the label for all four benchmark pages to `Benchmark`.
   - Standardize capitalization in `site/data/navigation.yaml`: top-level `groups:` use Title Case
     (`Getting Started` instead of `Getting started`), and secondary `labels:` use Sentence case
     (first letter capitalized, proper nouns and acronyms preserved: `Model delivery` instead of
     `Model Delivery`).

4. **Model Deployment `External DP` Module Guide**
   - Create `docs/modules/model-deployment/external-dp.md` documenting the operator's External DP
     architecture:
     - When to use External DP + Expert Parallelism (`--enable-expert-parallel`) on MoE models versus
       monolithic Tensor Parallelism (`TP`) or `Elastic EP`.
     - How the controller derives External DP balance mode (`modelDeploymentLoadBalance`) from role
       arguments (`--data-parallel-external-lb`, `--data-parallel-rank`, `--data-parallel-size`,
       `--data-parallel-local-size`, `--data-parallel-start-rank`) with no dedicated CRD field.
     - How a single `Server` replica group with `size: N` materializes `N` per-rank member Pods using
       operator-injected `$(GPUSTACK_MEMBER_INDEX)` and `$(GPUSTACK_REPLICA_LEADER_ADDRESS)`.
     - How Service and Router endpoint selectors drop the leader-only member-index term once the
       endpoint-eligibility gate activates (`modelDeploymentAnsweringAll`), registering every rank's
       HTTP endpoint in `gpustack/llm-router`.
     - Admission boundaries: `MultiPort` (`--data-parallel-multi-port-external-lb`) is refused on
       managed roles, `Encode` + External/Hybrid DP is refused, `Prefill`/`Decode` + External DP is
       unvalidated (#754), and no engine version gate is enforced at admission.
   - Register `docs/modules/model-deployment/external-dp.md` in `docs/README.md`,
     `docs/modules/model-deployment/_index.md`, and `.agents/skills/gpustack-operator-docs/SKILL.md`
     if applicable.

5. **Three Live Nebius Benchmark Pages**
   - Provision Nebius GPU nodes in two warm hardware batches, run reproducible benchmarks via `mcode`
     with `qwen-code` review, tear down the clusters immediately after collection, and publish:
     - `docs/modules/model-deployment/prefill-decode-benchmark.md`
     - `docs/modules/model-deployment/elastic-ep-benchmark.md`
     - `docs/modules/model-deployment/external-dp-benchmark.md`

### Notes/Constraints/Caveats

- **Documentation gate compliance (`make lint docs`).** Every added or edited Markdown file under
  `docs/` must pass `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh`:
  - Rule 1: Every internal Markdown link and `#anchor` resolves.
  - Rule 2: Every page with `##` headings has a `## Contents` list whose entries and anchors match its
    `##` headings in exact order.
  - Rule 3: Every non-index page ends with a `---` rule followed by `**See also**` and/or `**Next**`.
  - Rule 4 & 8: Every page under `docs/` appears in `docs/README.md` under `## All pages` with a link
    label identical to its `# H1` title.
  - Rule 5, 6, 7: Line count `<= 550`, code blocks `<= 12`, and `##` headings (excluding
    `## Contents`) `<= 10` unless explicitly exempted in `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh`.
- **Walkthrough `H2_CAP = 10` ceiling.** `docs/walkthroughs/kv-cache/shared-cache.md` has 7 steps;
  with `## Summary`, `## Step 1`..`## Step 7`, `## Limitation`, and `## Troubleshooting`, it has
  exactly 10 `##` sections (excluding `## Contents`). No additional `##` heading may be added to that
  file.
- **No off-repo or personal identifiers in committed docs.** Neither personal registry namespaces, nor
  `dev-<hash>` image tags, nor `@sha256:...` image/binary digests from test runs may appear in any
  committed file under `docs/` or `specs/`.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Removing `## Validation record` and `## Validation summary` breaks 4 in-page/cross-page `#validation-record` and `#sglang-ep--d-behind-both-routers` anchors. | Update all 4 prose links in `external-dp.md`, `encode-prefill-decode.md`, and `encode-prefill-decode-benchmark.md` in the same task and verify with `make lint docs`. |
| Nebius `project-e07nm50ama00jwzpb60mhy` (`eu-south1`) uses platform name `gpu-rtx6000-a` (`8gpu-192vcpu-1744gb`), while `testing/infra/clusters/nebius/variables.tf` only maps `gpu-rtx6000` in `us-central1`. | Validate the platform/preset mapping in `testing/infra/clusters/nebius/` before applying Terraform; if `gpu-rtx6000-a` in `eu-south1` or `gpu-l40s-d` (`4gpu-128vcpu-768gb`) in `eu-north1` needs a `tfvars` override or local variable entry, test with `terraform plan` first. |
| Cold-start provisioning, container image pulls, and Hugging Face weight downloads on paid Nebius GPU nodes waste cluster budget if clusters are recreated per test case. | Group all live tests into two warm batches (Batch 1 on 1 × 4-GPU L40S node; Batch 2 on 1 × 8-GPU RTX PRO 6000 node), reuse `NodeModelStore` cached weights across arms, and run `terraform destroy` immediately once each batch's verification gates pass. |
| `codex` or other Orca worker sessions may disconnect during long research, benchmark, or review turns. | Enforce 5/10/15-minute worker heartbeats in every dispatch brief and pair every coordinator `orca orchestration check --wait` with a 5-minute watchdog timer that checks `orca orchestration worker-read` and sends `continue` if a worker disconnects. |

## Design Details

### 1. Encode Role `--enforce-eager` Semantics and Documentation

1. **Code reality in GPUStack Operator (`pkg/worker/kvcache/inject/vllm_ec.go:75-93, 284-289`).**
   - `renderVLLMEncoderCache` appends four producer-only `DefaultedArgs` when `in.Role == RoleEncode`:
     `--enforce-eager`, `--no-enable-prefix-caching`, `--max-num-batched-tokens 114688`, and
     `--mm-encoder-only`.
   - `modelDeploymentDefaultedArgs` (`pkg/worker/controllers/worker/model_deployment_render.go:1570-1582`)
     drops any default group whose flag name matches a user-declared flag in `role. extraArgs`.
   - `RolePrefill`, `RoleDecode`, and `RoleServer` never receive `--enforce-eager` from the operator.
2. **Upstream engine reality (`vllm-project/vllm@v0.30.0`, `sgl-project/sglang@cff88d6eac`).**
   - In vLLM `v0.30.0`, an encoder-only instance (`--mm-encoder-only` or `ec_role: "ec_producer"`)
     executes only the vision/multimodal tower (`execute_mm_encoder`) and finishes each request as
     soon as the prompt's multimodal items are encoded (`vllm/v1/core/sched/scheduler.py:2046-2051`).
     It runs no language model forward pass, allocates no KV cache (`get_kv_cache_spec() -> {}`), and
     runs no token sampler.
   - Standard vLLM `torch.compile` and CUDA graph capture (`CUDAGraphMode.FULL` / `PIECEWISE`) compile
     and capture the **language model decoder**, while multimodal vision encoder compilation and CUDA
     graph capture (`compile_mm_encoder` and `cudagraph_mm_encoder` in
     `vllm/config/compilation.py:542-553`) default to `False`.
   - On the V2 model runner (`ECCPUConnector`, where the operator sets `VLLM_USE_V2_MODEL_RUNNER=1`),
     `vllm/v1/worker/gpu_worker.py:470-473` constructs `MMEncoderModelRunner`
     (`vllm/v1/worker/mm_encoder_model_runner.py:3-8, 67-74`), whose `capture_model()` returns `0` and
     `needs_cudagraph_capture()` returns `False` regardless of `--enforce-eager`.
   - On the V1 model runner (`ECExampleConnector` on vLLM and vLLM-Ascend, where
     `VLLM_USE_V2_MODEL_RUNNER` is unset), `GPUModelRunner._dummy_run()`
     (`vllm/v1/worker/gpu_model_runner.py:5894-5897`) short-circuits to empty tensors when
     `mm_encoder_only` is set; without `--enforce-eager`, `gpu_worker.py:581-585, 814-816` still enters
     `profile_cudagraph_memory()` and `capture_model()` across all decoder batch descriptors, and
     upstream `examples/disaggregated/disaggregated_encoder/README.md:40` explicitly marks
     `--enforce-eager` as required for encoder instances.
   - SGLang's `--encoder-only` mode natively skips language model weight loading and CUDA graph
     capture without a separate eager flag.
3. **Documentation changes.**
   - In `docs/modules/model-deployment/encode-prefill-decode.md` (around lines 170-175), explain why
     `--enforce-eager` is defaulted on the vLLM `encode` role (vision-only execution with no language
     model decode or KV cache, avoiding useless V1 decoder graph profiling and satisfying upstream
     vLLM `v0.30.0` encoder-only mode) and state that `prefill` and `decode` roles keep default CUDA
     graph capture enabled.
   - In `docs/walkthroughs/model-deployment/encode-prefill-decode.md` (`Step 2: inspect what the
     operator injected`), note that `--enforce-eager` and `--no-enable-prefix-caching` are injected as
     encoder-only defaults by the operator even though `case120-epd.yaml` did not declare them in
     `extraArgs`.

### 2. Walkthrough Section Contract and Checksum/Identifier Cleanup

1. **Sections and files to remove or relocate.**
   - `docs/walkthroughs/model-deployment/external-dp.md`:
     - Delete `## Validation record` (lines 320-345), removing the personal registry image reference
       and all binary/image SHA256 hashes.
     - Rewrite line 267 (which linked to `[validation record](#validation-record)`) to reference the
       new [Model Deployment External DP Benchmark](../../modules/model-deployment/external-dp-benchmark.md)
       page, and rewrite line 352 in `## Troubleshooting` so it states the symptom and fix directly
       without referencing the validation record.
   - `docs/walkthroughs/model-deployment/elastic-ep.md`:
     - Delete `## Validation record` (lines 341-391) and its `## Contents` entry.
     - Update the `See also` / `Next` footer to link to
       [Model Deployment Elastic EP Benchmark](../../modules/model-deployment/elastic-ep-benchmark.md).
   - `docs/walkthroughs/model-deployment/encode-prefill-decode.md`:
     - Delete `## Validation summary` (lines 314-350) and its `## Contents` entry.
     - Move the durable SGLang front-door rule (*"Send SGLang requests to `status.endpoint`. The
       deployment's plain Service does not front a disaggregated SGLang server and answers HTTP 400."*)
       into `docs/modules/model-deployment/encode-prefill-decode.md` under `### SGLang` (line 113).
     - Move the two-GPU node scheduling note (*"On a cluster of two-GPU nodes, the encoder and
       prefiller share one node through the weight-50 affinity rule, while the decoder runs on a second
       node."*) into `## Limitation` of `docs/walkthroughs/model-deployment/encode-prefill-decode.md`.
     - Update `docs/modules/model-deployment/encode-prefill-decode-benchmark.md:327` so its link points
       to `encode-prefill-decode.md#sglang` instead of the deleted walkthrough anchor
       `#sglang-ep--d-behind-both-routers`.
   - `docs/walkthroughs/model-deployment/_index.md` and `docs/walkthroughs/_index.md`:
     - Delete `## Check the software identity` (`docs/walkthroughs/model-deployment/_index.md:77-92`)
       and remove its entry from `## Contents`.
     - Rewrite `docs/walkthroughs/_index.md:31` so it describes reading live CR status and pod output
       without promising a `Validation record` section.

2. **Uniform walkthrough heading contract (`Summary` / `Step N:` / `Limitation` / `Troubleshooting`).**
   Every leaf walkthrough under `docs/walkthroughs/` must use only the following `##` headings (after
   `## Contents`):
   - `## Summary` — what problem the capability solves, which workload scenario it fits, and the
     deployment topology used in the walkthrough.
   - `## Step 1: <verb phrase>` ... `## Step N: <verb phrase>` — sequential reproducible steps.
   - `## Limitation` — prerequisites, hardware/capacity constraints, and operational boundaries of the
     walkthrough topology.
   - `## Troubleshooting` — symptom-to-cause diagnosis and recovery steps.

   Per-file mapping across all 10 leaf walkthroughs:

   | Walkthrough file | Current `##` (excl. Contents) | Transformation | Target `##` count (cap 10) |
   | --- | --- | --- | --- |
   | `devices/scheduling.md` | 8 (`The cluster`, `1.`..`6.`, `Troubleshooting`) | Rename `The cluster` -> `Summary`; rename `1.`..`6.` -> `Step 1:`..`Step 6:`; add `Limitation` before `Troubleshooting`. | 9 |
   | `rdma/network-endpoints.md` | 7 (`Prerequisites`, `Step 1`..`5`, `Troubleshooting`) | Add `Summary`; rename `Prerequisites` -> `Limitation` and move after `Step 5`. | 8 |
   | `topology/hierarchical-placement.md` | 7 (`Prerequisites`, `Step 1`..`5`, `Troubleshooting`) | Add `Summary`; rename `Prerequisites` -> `Limitation` and move after `Step 5`. | 8 |
   | `kv-cache/shared-cache.md` | 9 (`Creation order`, `Step 1`..`7`, `Troubleshooting`) | Rename `Creation order` -> `Summary`; add `Limitation` after `Step 7`. | 10 |
   | `model-delivery/prefetch.md` | 7 (`Prerequisites`, `Step 1`..`5`, `Troubleshooting`) | Add `Summary`; rename `Prerequisites` -> `Limitation` and move after `Step 5`. | 8 |
   | `model-deployment/prefill-decode.md` | 8 (`Deployment shape`, `Prerequisites`, `Step 1`..`5`, `Troubleshooting`) | Rename `Deployment shape` -> `Summary`; rename `Prerequisites` -> `Limitation` and move after `Step 5`. | 8 |
   | `model-deployment/encode-prefill-decode.md` | 9 (`Deployment shape`, `Prerequisites`, `Step 1`..`5`, `Validation summary`, `Troubleshooting`) | Rename `Deployment shape` -> `Summary`; fold `Prerequisites` + two-GPU note into `Limitation` after `Step 5`; delete `Validation summary`. | 8 |
   | `model-deployment/elastic-ep.md` | 8 (`Prerequisites`, `Step 1`..`4`, `Diagnose a stalled resize`, `Validation record`, `Troubleshooting`) | Add `Summary`; rename `Prerequisites` -> `Limitation` after `Step 4`; fold `Diagnose a stalled resize` (`###` subsection) into `Troubleshooting`; delete `Validation record`. | 7 |
   | `model-deployment/external-dp.md` | 10 (`Deployment shape`, `Prerequisites`, `Step 1`..`4`, `Understand the scaling limits`, `Step 5`, `Validation record`, `Troubleshooting`) | Rename `Deployment shape` -> `Summary`; fold `Prerequisites` and `Understand the scaling limits` into `Limitation` after `Step 5`; delete `Validation record`. | 8 |
   | `model-deployment/multi-host.md` | 8 (`Deployment shape`, `Prerequisites`, `Step 1`..`5`, `Troubleshooting`) | Rename `Deployment shape` -> `Summary`; rename `Prerequisites` -> `Limitation` and move after `Step 5`. | 8 |

### 3. Hugo Site Navigation Hierarchy and Capitalization

1. **Target `Model Deployment` sidebar hierarchy.**
   ```text
   Model Deployment
     - Overview
     - Configuration
     - Prefill and decode
       + Benchmark
     - Encode, prefill and decode
       + Benchmark
     - Elastic EP
       + Benchmark
     - External DP
       + Benchmark
     - Engine versions
     - Routing
     - Metrics
     - Status
     - Shutdown
   ```

2. **Data-driven child page support in Hugo templates.**
   - In `site/data/navigation.yaml`, add a top-level `children:` map keyed by parent `page-key`:
     ```yaml
     children:
       /docs/modules/model-deployment/prefill-decode/:
         - prefill-decode-benchmark
       /docs/modules/model-deployment/encode-prefill-decode/:
         - encode-prefill-decode-benchmark
       /docs/modules/model-deployment/elastic-ep/:
         - elastic-ep-benchmark
       /docs/modules/model-deployment/external-dp/:
         - external-dp-benchmark
     ```
   - In `site/layouts/partials/ordered-pages.html`, collect all child slugs declared under
     `site.Data.navigation.children` for pages within the current section and exclude those child
     pages from the top-level section slice so they do not appear twice.
   - In `site/layouts/partials/nav-section.html`, after rendering a leaf page `<a ...>{{ $name }}</a>`,
     look up `index site.Data.navigation.children (partial "page-key.html" .)`. When non-empty,
     resolve matching sibling pages from `$page.Pages` in the declared order and render them inside
     `<div class="nav-children">` beneath the parent link.

3. **Capitalization rules across `site/data/navigation.yaml`.**
   - **`groups:` (top-level sidebar section headers)**: Title Case. Change `Getting started` to
     `Getting Started` so all 11 groups use Title Case consistently.
   - **`labels:` (secondary and tertiary menu items)**: Sentence case (capitalize the first word and
     preserve proper nouns and acronyms: `RDMA`, `NVIDIA MIG`, `Hygon MIG`, `T-Head MIG`, `KV`, `EP`,
     `DP`, `API`, `v0.5.x`).
   - Change `/docs/walkthroughs/model-delivery/` from `Model Delivery` to `Model delivery` and
     `/docs/walkthroughs/kv-cache/` stays `KV Cache` (or `KV cache` to match `Model deployment` and
     `Model delivery`).
   - Rename `/docs/modules/model-deployment/deployment/` to `Configuration`.
   - Set `/docs/modules/model-deployment/prefill-decode-benchmark/`,
     `/docs/modules/model-deployment/encode-prefill-decode-benchmark/`,
     `/docs/modules/model-deployment/elastic-ep-benchmark/`, and
     `/docs/modules/model-deployment/external-dp-benchmark/` labels to `Benchmark`.
   - Add `/docs/modules/model-deployment/external-dp/: External DP`.

### 4. Model Deployment `External DP` Module Guide

Create `docs/modules/model-deployment/external-dp.md` with `# Model Deployment External DP` and the
following structure (compliant with `check-docs.sh` caps):

1. `## Contents`
2. `## When to use External DP` — explains why MoE models with Multi-head Latent Attention (MLA) or
   small attention heads benefit from data-parallel attention (`DP`) combined with expert parallelism
   (`EP = TP × DP`) across ranks, and compares External DP (fixed rank group, per-rank HTTP endpoint,
   router-balanced) against monolithic Tensor Parallelism (`TP`) and `Elastic EP` (Ray collective,
   single master HTTP endpoint, online scale-up).
3. `## How the operator derives External DP` — documents how `scanModelDeploymentParallelism`
   (`pkg/worker/controllers/worker/model_deployment_parallelism.go:477-548`) derives the balance mode
   from `role.extraArgs` and `role.env` (`--data-parallel-external-lb`, `--data-parallel-rank`,
   `--data-parallel-size`, `--data-parallel-local-size`, `--data-parallel-start-rank`) and reports
   `dataParallel`, `dataParallelLocal`, and `expertParallel` in status.
4. `## Per-rank Pods and runtime variables` — shows a complete `ModelDeployment` manifest using a
   `server` role with `replicas: 1`, `size: N`, `--tensor-parallel-size`, `--data-parallel-size`,
   `--enable-expert-parallel`, `--data-parallel-rank $(GPUSTACK_MEMBER_INDEX)`, and
   `--data-parallel-address $(GPUSTACK_REPLICA_LEADER_ADDRESS)`. Explains which flags can be omitted
   (`--data-parallel-external-lb` when `--data-parallel-rank` is set; `--data-parallel-rpc-port`).
5. `## Service and router endpoint selection` — explains how `modelDeploymentSelectEligibleEndpoints`
   (`pkg/worker/controllers/worker/model_deployment_service.go:415-428`) and
   `modelDeploymentRouterRoleSelector` (`model_deployment_router.go:139-149`) drop the leader
   member-index selector term once the endpoint-eligibility gate activates, so both the Kubernetes
   Service and `gpustack/llm-router` route across all qualified ranks.
6. `## Admission and operational boundaries` — documents the admission rules:
   - `MultiPort` (`--data-parallel-multi-port-external-lb`) is refused on managed roles.
   - `Encode` role + External/Hybrid DP is refused (`validateModelDeploymentEPDEncodeShape`).
   - `Prefill` / `Decode` + External DP is unvalidated (#754).
   - `size` is immutable in place on an External DP gang; changing DP world size requires recreating
     the deployment or using `Elastic EP`.
7. Footer (`---`, `**See also**`, `**Next**`).

### 5. Nebius Live Benchmark Plan and Scenario Justification

Every benchmark follows a clear pedagogical contract: **demonstrate both the regime where a simpler
baseline is sufficient and the concrete crossover regime where turning on the capability wins**, while
recording hardware/interconnect boundaries (`## Limitation`) when the capability is run on unsuitable
fabric (such as plain PCIe or TCP without RDMA/NVSwitch).

#### 5.1 Root-Cause Retrospective of Phase 1 (Plain-PCIe / TCP Run) and Hardware Redesign

In the initial run on `gpu-rtx6000-a` (`8 × NVIDIA RTX PRO 6000 Blackwell Server Edition 96 GB` on
plain PCIe without NVLink and without InfiniBand RDMA):

1. **`Prefill and Decode` (`Qwen/Qwen3-8B` on 1-GPU PCIe/TCP)**:
   - Model was too small (`16 GB` weights on a `96 GB` GPU, leaving `64 GiB` KV cache per card so one GPU
     was never compute- or KV-capacity-bound), and KV transfer ran over `MooncakeConnector` TCP loopback
     without RDMA (`1.2×–6.5×` TTFT penalty and `103/104` empty-body drops under `ret=-1` burst saturation).
2. **`Elastic EP` (`DeepSeek-V2-Lite-Chat` on PCIe with `--max-num-seqs=64`)**:
   - With `--max-num-seqs=64` per DP rank, `Width 2` already had `128` concurrent slots (`4×` the maximum
     tested concurrency `c=32`), so `Width 2` never queued; meanwhile, expanding `EP=4 -> EP=8` doubled
     `allgather_reducescatter` payload across plain PCIe without NVSwitch, increasing single-token TPOT by
     `82%` and reducing throughput by `27%–40%`.
3. **`External DP` (`DeepSeek-V2-Lite-Chat` on PCIe with `--max-num-seqs=64`)**:
   - Even `Wide TP (TP8)` had `64` slots (`2×` maximum concurrency `c=32`), so its single scheduler and
     8× replicated MLA KV cache never saturated; meanwhile, 8-card PCIe `allgather_reducescatter` cost
     `4.3×` the TPOT of `TP8`.

**Unified Phase 2 Hardware Base (Cost-Controlled SXM5 + NVSwitch + InfiniBand RDMA)**:
- **Cluster shape**: **1 × 8-GPU SXM5 node** (`gpu-h100-sxm` in `eu-north1` `project-e00xbnvkpr009f9sk2gcsz`,
  or `gpu-h200-sxm` in `eu-west1` `project-e01yvmeppr008t3kdsqkkw` / `eu-north1`, preset
  `8gpu-128vcpu-1600gb`, **`preemptible = true`** for ~50%–70% cost reduction, `boot_disk_size_gb = 400`,
  with **900 GB/s intra-node NVSwitch NVLink** and **InfiniBand RDMA `infiniband_fabric`**) + **1 × CPU
  node** (`cpu-d3` or `cpu-e2` `4vcpu-16gb` within the 9 vCPU non-GPU quota in `eu-north1`).
- **Execution discipline**: Run all 4 scenarios sequentially on the same warm 8-GPU SXM5 node, prefetching
  each model (`Qwen/Qwen2.5-VL-7B-Instruct`, `Qwen/Qwen3-32B`, `deepseek-ai/DeepSeek-V2-Lite-Chat`) once
  into `NodeModelStore`, capturing both the benchmark suites and the corresponding walkthrough outputs, and
  running `terraform destroy` immediately after verification.
- **Preserving Phase 1 lessons**: Retain the measured plain-PCIe / TCP (`RTX PRO 6000`) negative results
  as concrete quantitative evidence in each benchmark and walkthrough page's **`## Limitation`** section.

#### 5.2 Scenario 1: `Encode, Prefill and Decode` (`encode-prefill-decode-benchmark.md` + `walkthroughs/model-deployment/encode-prefill-decode.md`)

- **Model**: `Qwen/Qwen2.5-VL-7B-Instruct` (revision `cc594898137f460bfe9f0759e9844b3ce807cfb5`, BF16).
- **Arms on Unified SXM5 Hardware**:
  - **Arm 1 — Aggregated (`1 × Server`, 1 GPU)**: Vision encoder + prefill + decode in one engine.
  - **Arm 2 — `PD` (`1 × Prefill + 1 × Decode`, 2 GPUs)**: Vision encoder still inside `Prefill`; KV cache
    transferred to `Decode`.
  - **Arm 3 — `EPD` (`1 × Encode + 1 × Prefill + 1 × Decode`, 3 GPUs)**: Dedicated vision encoder
    (`--mm-encoder-only`, defaulted `--enforce-eager`) transferring visual embeddings via `ECCPUConnector`
    (NIXL + shared RWX volume) to `Prefill`, and KV cache to `Decode`.
- **Workload suites**:
  - **Suite 1: Image resolution & concurrency sweep** — `1k`, `2k`, `4k`, and `8k` total tokens (visual +
    text prompt, `128` output tokens, unique non-cached images) at concurrencies `1`, `4`, and `8`. Shows
    `Aggregated` winning at `1k`/`2k` low concurrency (lowest GPU cost, no transfer hop) and `EPD` crossing
    over to win at `4k`/`8k` under concurrency (`4, 8`).
  - **Suite 2: Mixed small-image steady stream + heavy-vision burst** — Concurrent `1k` steady decode stream
    and `4k` heavy-vision bursts (`c=4` + `c=4`), demonstrating how `EPD` isolates both `Prefill` and
    `Decode` from ViT forward stalls.
  - **Suite 3: Repeated-image multi-turn queries (encoder cache reuse)** — `4k` shared PNG with unique text
    questions at concurrencies `4` and `8`, showing `EPD` encoder cache reuse cutting TTFT and boosting
    total throughput across all metrics.
- **Walkthrough update (`docs/walkthroughs/model-deployment/encode-prefill-decode.md`)**:
  - Verify and align the walkthrough manifest, CR status, and curl verification against the unified SXM5
    environment, while keeping the 2-GPU node co-location rule documented in `## Limitation`.

#### 5.3 Scenario 2: `Prefill and Decode` (`prefill-decode-benchmark.md` + `walkthroughs/model-deployment/prefill-decode.md`)

- **Model**: **`Qwen/Qwen3-32B`** (BF16, ~64 GB weights — sized appropriately for 80 GB SXM5 GPUs with
  `TP2`, replacing the undersized `Qwen3-8B` so prefill compute and KV transfer volume are realistic).
- **Equal-GPU arms (6 GPUs total per arm, matching `docs/walkthroughs/model-deployment/prefill-decode.md`)**:
  - **Arm 1 — Aggregated (`3 × Server`, `TP2`, 6 GPUs total)**: `replicas: 3`, `size: 1`, 2 GPUs per
    replica (`--tensor-parallel-size=2`), fronted by `gpustack/llm-router:v0.3.1` (`round_robin`).
  - **Arm 2 — Disaggregated `PD` (`1 × Prefill TP2 + 2 × Decode TP2`, 6 GPUs total)**: `prefill`
    (`replicas: 1`, 2 GPUs, `TP2`) + `decode` (`replicas: 2`, 2 GPUs each, `TP2`) behind `llm-d-router`,
    using high-speed RDMA / NVLink KV transfer (**target: 100% request success, 0 empty-body drops across
    all suites**).
- **Workload suites**:
  - **Suite 1: Context-length & concurrency sweep** — `512 / 256` (short prompt), `4096 / 512` (balanced
    long context), and `8192 / 256` (prefill-heavy) across concurrencies `1`, `4`, `8`, and `16`. At
    `512/256` low concurrency, `Aggregated` has lower TTFT (no KV transfer hop); at `4096/512` and
    `8192/256` under concurrency `8–16`, `PD` wins on ITL p99 and sustained decode throughput.
  - **Suite 2: Mixed long-prefill bursts and steady decode stream (`100%` completion)** — 4 steady decode
    workers (`512` prompt / `512` output) + 4 burst prefill workers (`8192` prompt / `64` output). Shows
    `Aggregated` suffering severe ITL p99 spikes when 8k prefills interrupt active decode batches, while
    `PD` keeps steady-stream ITL p99 flat (`3×–5×` lower) and achieves higher combined throughput.
  - **Suite 3: Stage execution & KV transfer verification** — Confirms 100% request completion through
    `prefill` -> KV connector -> `decode`.
- **`## Limitation`**: Retain the measured plain-PCIe / TCP (`RTX PRO 6000` without RDMA) numbers showing
  that over TCP without RDMA, KV transfer latency (`1.2×–6.5×` TTFT penalty) and Mooncake TCP `ret=-1`
  burst drops negate disaggregation benefits.
- **Walkthrough update (`docs/walkthroughs/model-deployment/prefill-decode.md`)**:
  - Replace `This page records no captured run.` with live captured CR JSON, router status, inference curl
    response, and Kueue workload admission on the 6-GPU `1 × Prefill (TP2) + 2 × Decode (TP2)` deployment.

#### 5.4 Scenario 3: `Elastic EP` (`elastic-ep-benchmark.md` + `walkthroughs/model-deployment/elastic-ep.md`)

- **Model**: `deepseek-ai/DeepSeek-V2-Lite-Chat` (revision `85864749cd611b4353ce1decdb286193298f64c7`, BF16)
  on 8 × SXM5 GPUs (900 GB/s NVSwitch NVLink).
- **Deployment shape & bounded per-rank capacity**:
  - Single `Server` role with `--tensor-parallel-size=2`, `--enable-expert-parallel`,
    `--all2all-backend=allgather_reducescatter`, `--elastic-ep-max-dp-size=4`, and bounded per-rank batch
    slots **`--max-num-seqs=16`** (so `Width 2` provides `2 × 16 = 32` active sequence slots across 4 GPUs,
    and `Width 4` provides `4 × 16 = 64` active sequence slots across 8 GPUs).
- **States & workload suites**:
  - **State 1 — Baseline `Width 2` (`DP2, TP2, EP4`, 4 GPUs)** across concurrencies `1, 8, 16, 32, 64`
    (`1024` prompt / `256` output tokens).
  - **State 2 — Live scale-up transition (`Width 2 -> Width 4`, `4 GPUs -> 8 GPUs`) under load**: Patch
    `elasticEp.width: 4`, recording exact `Scaling` duration, readiness withdrawal window, in-flight drain,
    and zero weight reload on surviving members `0` and `1`.
  - **State 3 — Expanded `Width 4` (`DP4, TP2, EP8`, 8 GPUs)** across concurrencies `1, 8, 16, 32, 64`.
    Shows that at `c=1, 8`, `Width 2` is sufficient and saves 4 GPUs; at `c=32, 64`, `Width 2` saturates
    its 32 slots and queues heavily, whereas `Width 4` over NVSwitch eliminates queueing, **slashes TTFT,
    and nearly doubles output throughput (`tok/s` and `req/s`)**.
- **`## Limitation`**: Retain the measured plain-PCIe (`RTX PRO 6000`) finding that without NVSwitch/RDMA,
  expanding `allgather_reducescatter` from 4 to 8 GPUs over PCIe increases single-token TPOT by `82%` and
  reduces throughput by `27%–40%`.
- **Walkthrough update (`docs/walkthroughs/model-deployment/elastic-ep.md`)**:
  - Align image tag (`vllm0.30.0`), `--elastic-ep-max-dp-size=4`, `--max-num-seqs=16`, and captured scale-up
    logs/status with the live SXM5 run.

#### 5.5 Scenario 4: `External DP` (`external-dp-benchmark.md` + `walkthroughs/model-deployment/external-dp.md`)

- **Model**: `deepseek-ai/DeepSeek-V2-Lite-Chat` (revision `85864749cd611b4353ce1decdb286193298f64c7`, BF16,
  reusing the warm `NodeModelStore` artifact from Scenario 3).
- **Equal-GPU arms (8 × SXM5 GPUs per arm, 900 GB/s NVSwitch NVLink, `--max-num-seqs=16` per scheduler)**:
  - **Arm 1 — Monolithic `Wide TP` (`1 × Server TP8`, 8 GPUs)**: `replicas: 1, size: 1`, 8 GPUs
    (`--tensor-parallel-size=8`, `--max-num-seqs=16`), 1 HTTP API server & scheduler (`16` active slots
    total), 8× duplicated MLA KV cache across all 8 GPUs.
  - **Arm 2 — `External DP` (`1 × Server, size: 4, TP2, DP4, EP8`, 8 GPUs)**: `replicas: 1, size: 4`, 2
    GPUs per member (`--tensor-parallel-size=2`, `--data-parallel-size=4`, `--enable-expert-parallel`,
    `--data-parallel-rank $(GPUSTACK_MEMBER_INDEX)`, `--max-num-seqs=16`), 4 independent HTTP API servers &
    schedulers (`4 × 16 = 64` active slots total, `4×` effective MLA KV cache capacity) fronted by
    `gpustack/llm-router:v0.3.1` (`round_robin`).
- **Workload suites**:
  - **Prompt shape and concurrency sweep** — `1024 / 256` and `4096 / 256` across concurrencies `1, 4, 8,
    16, 32, 64`. Shows `Wide TP` winning at `c=1, 4` (all 8 GPUs cooperate on one request's attention for
    lowest single-stream TTFT/TPOT), and **`External DP` crossing over to win decisively at `c=16, 32, 64`**
    (where `Wide TP`'s single 16-slot scheduler queues heavily while `External DP`'s 4 independent
    schedulers and NVSwitch-backed EP deliver much lower TTFT and higher total throughput).
  - **Per-rank registration and load balance** — Verifies all 4 rank endpoints in the router ConfigMap and
    balanced `vllm:request_success_total` counters across ranks `0..3`.
- **`## Limitation`**: Retain the measured plain-PCIe (`RTX PRO 6000`) finding where 8-card PCIe
  `allgather_reducescatter` cost `4.3×` the TPOT of `TP8`.
- **Walkthrough update (`docs/walkthroughs/model-deployment/external-dp.md`)**:
  - Align image tags (`vllm0.30.0`, `llm-router:v0.3.1`), manifest flags, and captured status/metrics with
    the live SXM5 run.

### Commands

```bash
# Documentation and site index lint gate (runs check-docs.sh + Hugo site build + verify.py)
make lint docs

# Full Go/shell lint gate (if any Go or shell file is touched)
make lint

# Operator image build and push (only if an operator code fix is required during live testing)
PACKAGE_PUSH=true PACKAGE_NAMESPACE=$REGISTRY_NAMESPACE PACKAGE_TAG=dev-$(git rev-parse --short HEAD) make package
```

### Project Structure

- `docs/README.md` — master documentation index (`## Module map` and `## All pages`).
- `docs/modules/model-deployment/` — module guides (`_index.md`, `deployment.md`, `prefill-decode.md`,
  `prefill-decode-benchmark.md`, `encode-prefill-decode.md`, `encode-prefill-decode-benchmark.md`,
  `elastic-ep.md`, `elastic-ep-benchmark.md`, `external-dp.md`, `external-dp-benchmark.md`,
  `engine-versions.md`, `routing.md`, `metrics.md`, `status.md`, `shutdown.md`).
- `docs/walkthroughs/` — walkthrough index (`_index.md`), model-deployment index
  (`model-deployment/_index.md`), and the 10 walkthrough leaf pages.
- `site/data/navigation.yaml` — Hugo sidebar groups, page ordering, child nesting (`children:`), and
  display labels.
- `site/layouts/partials/nav-section.html` and `site/layouts/partials/ordered-pages.html` — Hugo
  sidebar rendering partials.
- `testing/infra/clusters/nebius/` — Terraform root module for Nebius MKS GPU test clusters.

### Code Style

- Follow `~/.agents/skills/my-workflow/references/writing-style.md` and
  `.agents/skills/gpustack-operator-docs/SKILL.md`: short declarative sentences, active voice, concrete
  measurements, and zero marketing prose.
- Every page under `docs/` must include `## Contents` matching its `##` headings and a `---` footer
  with `**See also**` / `**Next**`.

### Boundaries

- **Always:**
  - Run `make lint docs` after any documentation, index, or Hugo template edit.
  - Keep walkthroughs within the 10-`##` section cap and 550-line cap enforced by
    `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh`.
  - Destroy Nebius GPU clusters via `terraform destroy` as soon as live benchmark collection finishes.
- **Ask first:**
  - Any change to `pkg/` Go source code (only permitted if live benchmark testing uncovers a genuine
    operator bug blocking a benchmark scenario).
- **Never:**
  - Commit personal registry namespaces, `dev-<hash>` tags, or run-identity binary/image SHA256
    digests into `docs/` or `specs/`.
  - Leave a Nebius GPU cluster running after benchmark tasks complete.
  - Pass `--enforce-eager` on `Prefill`, `Decode`, or `Server` roles in the new `Prefill and decode`
    benchmark.

### Implementation Plan

- [x] **T1 · Walkthrough Hygiene, Checksum/Identifier Removal, and Section Standardization**
      Blocked by: None
      Owns: `docs/walkthroughs/**`, `docs/reference/instance-type-unit-resources.md`
      Acceptance: Every `## Validation record`, `## Validation summary`, and `## Check the software identity` section is removed from `docs/walkthroughs/`; zero personal registry namespaces or run-identity binary/image SHA256 digests remain under `docs/walkthroughs/`; all 10 walkthrough leaf pages use only `## Contents`, `## Summary`, `## Step 1: ...`..`## Step N: ...`, `## Limitation`, and `## Troubleshooting` (with `##` count <= 10 and line count <= 550); the two-GPU co-location note in `encode-prefill-decode.md` is folded into `## Limitation` and `Step 2` explains why `--enforce-eager` is rendered as a producer default on `encode`.
      Verify: `! grep -riE '## Validation (record|summary)' docs/walkthroughs/ && bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh --report`

- [x] **T2 · EPD `--enforce-eager` Clarification, SGLang Front-Door Relocation, and `External DP` Module Guide**
      Blocked by: None
      Owns: `docs/modules/model-deployment/_index.md`, `docs/modules/model-deployment/encode-prefill-decode.md`, `docs/modules/model-deployment/encode-prefill-decode-benchmark.md`, `docs/modules/model-deployment/external-dp.md`
      Gate: review
      Acceptance: `docs/modules/model-deployment/encode-prefill-decode.md` documents why `--enforce-eager` is defaulted on `RoleEncode` alone (`MMEncoderModelRunner` / `--mm-encoder-only` runs no language model, KV cache, or decoder CUDA graphs, and V1 `GPUModelRunner` / upstream vLLM `v0.30.0` requires `--enforce-eager` on encoder instances while `Prefill` and `Decode` keep CUDA graphs enabled) and includes the SGLang `status.endpoint` front-door contract under `### SGLang`; `encode-prefill-decode-benchmark.md` updates its SGLang anchor to `encode-prefill-decode.md#sglang`; `docs/modules/model-deployment/external-dp.md` documents External DP derivation, per-rank Pods, Service/Router endpoint eligibility, and admission rules, and is linked from `docs/modules/model-deployment/_index.md`.
      Verify: `test -s docs/modules/model-deployment/external-dp.md && bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh --report`

- [x] **T3 · Live Nebius Benchmark Batch 1: `Prefill and decode` Benchmark**
      Blocked by: None
      Owns: `docs/modules/model-deployment/prefill-decode-benchmark.md`, `docs/modules/model-deployment/prefill-decode.md`
      Gate: review
      Acceptance: Live Nebius benchmark executed by `mcode` (cross-reviewed by `qwen-code`) on 4 × NVIDIA RTX PRO 6000 (`project-e07nm50ama00jwzpb60mhy`, `eu-south1` `gpu-rtx6000-a` `8gpu-192vcpu-1744gb`, or 4 × NVIDIA L40S in `project-e00xbnvkpr009f9sk2gcsz`) comparing equal-GPU Aggregated (`4 × 1-GPU server`) vs Disaggregated (`2 × 1-GPU prefill + 2 × 1-GPU decode` over `MooncakeConnector` TCP) on `Qwen/Qwen3-8B` BF16 (vLLM `0.30.0`, no `--enforce-eager`) across Suite 1 (`512/256`, `4096/128`, `256/1024` at concurrencies `1, 4, 8, 16`), Suite 2 (mixed long-prefill bursts + steady decode stream), and Suite 3 (stage execution verification). `docs/modules/model-deployment/prefill-decode-benchmark.md` is written and reviewed by `codex`, linked from `prefill-decode.md`, and contains zero personal registry namespaces or SHA256 digests.
      Verify: `test -s docs/modules/model-deployment/prefill-decode-benchmark.md && ! grep -riE '@sha256:' docs/modules/model-deployment/prefill-decode-benchmark.md`

- [x] **T4 · Live Nebius Benchmark Batch 2: `Elastic EP` and `External DP` Benchmarks & Cluster Teardown**
      Blocked by: T2, T3
      Owns: `docs/modules/model-deployment/elastic-ep-benchmark.md`, `docs/modules/model-deployment/elastic-ep.md`, `docs/modules/model-deployment/external-dp-benchmark.md`
      Gate: review
      Acceptance: Live Nebius benchmarks executed by `mcode` (cross-reviewed by `qwen-code`) on `deepseek-ai/DeepSeek-V2-Lite-Chat` (`85864749cd611b4353ce1decdb286193298f64c7`) using 1 × 8-GPU `gpu-rtx6000-a` (`project-e07nm50ama00jwzpb60mhy`) or the warm 4-GPU `gpu-l40s-d` node (`project-e00xbnvkpr009f9sk2gcsz`): (1) `Elastic EP` measures baseline `DP=2`, live scale-up `DP=2 -> DP=4` under concurrent load (resize duration, readiness withdrawal window, HTTP 503/error count, recovery time), and expanded `DP=4` across concurrencies `1, 4, 8, 16, 32`; (2) `External DP` compares equal-GPU Monolithic Wide TP (`replicas: 1, size: 1`) vs `External DP + EP` (`replicas: 1, size: 4`, `--enable-expert-parallel`, per-rank HTTP endpoints behind `gpustack/llm-router` `round-robin`) across `1024/256` and `4096/128` at concurrencies `1, 4, 8, 16, 32` plus per-rank counter verification. All Nebius clusters are destroyed via `terraform destroy` with zero remaining GPU nodes. Both benchmark pages are reviewed by `codex`.
      Verify: `test -s docs/modules/model-deployment/elastic-ep-benchmark.md && test -s docs/modules/model-deployment/external-dp-benchmark.md && ! grep -riE '@sha256:' docs/modules/model-deployment/elastic-ep-benchmark.md docs/modules/model-deployment/external-dp-benchmark.md`

- [x] **T5 · Hugo Sidebar Nesting, Menu Ordering, Capitalization, and Shared Index Sync**
      Blocked by: T1, T2, T3, T4
      Owns: `site/data/navigation.yaml`, `site/layouts/partials/nav-section.html`, `site/layouts/partials/ordered-pages.html`, `docs/README.md`, `.agents/skills/gpustack-operator-docs/SKILL.md`
      Gate: review
      Acceptance: `site/data/navigation.yaml`, `ordered-pages.html`, and `nav-section.html` render the reordered `Model Deployment` menu (`Overview`, `Configuration`, `Prefill and decode` + `Benchmark`, `Encode, prefill and decode` + `Benchmark`, `Elastic EP` + `Benchmark`, `External DP` + `Benchmark`, `Engine versions`, `Routing`, `Metrics`, `Status`, `Shutdown`) with Title Case `groups:` (`Getting Started`) and Sentence case `labels:` (`Model delivery`); `docs/README.md` (`## Module map` and `## All pages`) and `.agents/skills/gpustack-operator-docs/SKILL.md` index all 4 new pages; `make lint docs` passes with zero warnings or errors.
      Verify: `make lint docs`

- [x] **T6 · Live Nebius SXM5 Re-Run: 4-Scenario Benchmarks (`EPD`, `PD`, `Elastic EP`, `External DP`) and Walkthrough Captures**
      Blocked by: T5
      Owns: `testing/infra/clusters/nebius/terraform.tfvars` (ephemeral, uncommitted)
      Gate: review
      Acceptance: Provision 1 × 8-GPU SXM5 node (`gpu-h100-sxm` in `eu-north1` `project-e00xbnvkpr009f9sk2gcsz`, or `gpu-h200-sxm`, `8gpu-128vcpu-1600gb`, `preemptible = true`, 900 GB/s NVSwitch NVLink + InfiniBand RDMA) + 1 × CPU node (`cpu-d3` / `cpu-e2` `4vcpu-16gb`), executed by `mcode` and cross-reviewed by `qwen`: (1) `EPD` (`Qwen/Qwen2.5-VL-7B-Instruct`: `Aggregated 1 GPU` vs `PD 2 GPUs` vs `EPD 3 GPUs` across Suites 1–3 + walkthrough verification); (2) `PD` (`Qwen/Qwen3-32B`: `Aggregated 3 × TP2 = 6 GPUs` vs `PD 1 × Prefill TP2 + 2 × Decode TP2 = 6 GPUs` with 100% completion across Suites 1–3 + walkthrough capture); (3) `Elastic EP` (`deepseek-ai/DeepSeek-V2-Lite-Chat`, `--max-num-seqs=16`: `Width 2 (4 GPUs) -> Width 4 (8 GPUs)` live resize + concurrency sweep `1..64` showing high-concurrency crossover + walkthrough capture); (4) `External DP` (`deepseek-ai/DeepSeek-V2-Lite-Chat`, `--max-num-seqs=16`: `Wide TP (TP8, 8 GPUs)` vs `External DP (size: 4, TP2, DP4, EP8, 8 GPUs)` across concurrencies `1..64` showing high-concurrency crossover + per-rank balance + walkthrough capture). Immediately run `terraform destroy` upon completion.
      Verify: `cd testing/infra/clusters/nebius && terraform state list | wc -l | grep -qx '0'`

- [x] **T7 · Update 4 Benchmark Pages and 4 Walkthrough Pages with SXM5 Crossover Results and PCIe/TCP Limitations**
      Blocked by: T6
      Owns: `docs/modules/model-deployment/encode-prefill-decode-benchmark.md`, `docs/modules/model-deployment/prefill-decode-benchmark.md`, `docs/modules/model-deployment/elastic-ep-benchmark.md`, `docs/modules/model-deployment/external-dp-benchmark.md`, `docs/walkthroughs/model-deployment/encode-prefill-decode.md`, `docs/walkthroughs/model-deployment/prefill-decode.md`, `docs/walkthroughs/model-deployment/elastic-ep.md`, `docs/walkthroughs/model-deployment/external-dp.md`
      Gate: review
      Acceptance: Update all 4 benchmark pages and 4 walkthrough pages with the SXM5/NVSwitch/RDMA measurements and walkthrough outputs, bold winning cell numbers, highlight notable crossover rows, preserve the Phase 1 plain-PCIe / TCP (`RTX PRO 6000`) negative results as quantitative evidence in `## Limitation`, pass `codex` review, and pass `make lint docs` with zero errors.
      Verify: `make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh` and `hack/docs.sh` (`make lint docs`) already validate link/anchor
resolution, `## Contents` parity, footer structure, `docs/README.md` index completeness, H1 label
matching, and line/code-block/heading caps.

#### Unit tests

- `pkg/worker/kvcache/inject`: `2026-10-20` - `vllm_ec_test.go` verifies `DefaultedArgs` on `RoleEncode`, and `inject_test.go` / `sglang_test.go` verify Mooncake TCP defaulted environment variables (`MC_TCP_*`) and coarse KV block/page sizes (`--block-size 128` on vLLM, `--page-size 64` on SGLang).
- `pkg/worker/controllers/worker` and `pkg/worker/webhooks/worker`: `2026-10-20` - `model_deployment_connector_test.go`, `model_deployment_render_test.go`, `model_deployment_test.go`, and `pod_kv_cache_inject_test.go` verify TCP defaulted env/args override semantics and `resources.interface` admission on multi-Pod `Server` roles (`size > 1` or `elasticEp != nil`).

#### Integration tests

- Documentation and Hugo site build verification via `make lint docs`:
  - `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh`: verifies all 9 documentation rules across all existing and new Markdown pages.
  - Hugo site build + `hack/site/verify.py`: verifies the generated HTML sidebar hierarchy, `children:` nesting under `Model Deployment`, and exported Markdown/LLM indexes.

#### e2e tests

- **Unified SXM5 Live Nebius Benchmark & Walkthrough Suite (`gpu-h100-sxm` / `gpu-h200-sxm` `8gpu-128vcpu-1600gb`, `preemptible = true`)**:
  - **Scenario 1 (`Encode, Prefill and Decode`)**: `Qwen/Qwen2.5-VL-7B-Instruct` on `Aggregated (1 GPU)` vs `PD (2 GPUs)` vs `EPD (3 GPUs)` across Suites 1–3 (`1k, 2k, 4k, 8k` at `c=1, 4, 8`, mixed burst, repeated-image cache reuse) + walkthrough verification.
  - **Scenario 2 (`Prefill and Decode`)**: `Qwen/Qwen3-32B` on `Aggregated (3 × TP2 = 6 GPUs)` vs `PD (1 × Prefill TP2 + 2 × Decode TP2 = 6 GPUs)` over RDMA/NVLink across Suites 1–3 (`512/256`, `4096/512`, `8192/256` at `c=1, 4, 8, 16`, mixed steady + 8k prefill bursts with 100% completion, stage verification) + walkthrough capture.
  - **Scenario 3 (`Elastic EP`)**: `deepseek-ai/DeepSeek-V2-Lite-Chat` (`--max-num-seqs=16`) on `TP2, Width 2 (4 GPUs) -> Width 4 (8 GPUs)` across concurrencies `1, 8, 16, 32, 64` + live scale-up transition + walkthrough capture.
  - **Scenario 4 (`External DP`)**: `deepseek-ai/DeepSeek-V2-Lite-Chat` (`--max-num-seqs=16`) on `Wide TP (TP8, 8 GPUs)` vs `External DP (size: 4, TP2, DP4, EP8, 8 GPUs)` across `1024/256` and `4096/256` at concurrencies `1, 4, 8, 16, 32, 64` + per-rank load balance + walkthrough capture.
- **Cluster Teardown Verification**:
  - Run `terraform destroy` and verify via `nebius compute instance list` / `nebius mk8s cluster list` that zero test instances or clusters remain active.

### Graduation Criteria

1. `docs/modules/model-deployment/encode-prefill-decode.md` and
   `docs/walkthroughs/model-deployment/encode-prefill-decode.md` clearly explain why `--enforce-eager`
   is defaulted on `RoleEncode` alone.
2. Zero occurrences of personal registry namespaces, `## Validation record`, or `## Validation summary`
   remain under `docs/`.
3. All 10 walkthrough leaf pages under `docs/walkthroughs/` use only `## Contents`, `## Summary`,
   `## Step N: ...`, `## Limitation`, and `## Troubleshooting`.
4. The Hugo sidebar renders the reordered `Model Deployment` hierarchy with `Benchmark` nested under
   `Prefill and Decode`, `Encode, Prefill and Decode`, `Elastic EP`, and `External DP`, and consistent
   concise noun-phrase labels across all groups and pages.
5. All four benchmark pages (`encode-prefill-decode-benchmark.md`, `prefill-decode-benchmark.md`,
   `elastic-ep-benchmark.md`, `external-dp-benchmark.md`) and four walkthrough pages under
   `docs/walkthroughs/model-deployment/` are updated with live SXM5/NVSwitch/RDMA measurements showing
   each capability's true crossover regime, while preserving plain-PCIe / TCP boundaries in `## Limitation`.
6. `make lint docs` passes with zero errors.

## Drawbacks

1. Adding four benchmark pages and one module guide increases the total page count under
   `docs/modules/model-deployment/` from 10 to 14, requiring `children:` nesting in
   `site/data/navigation.yaml` so the sidebar stays compact.
2. Single-node 8-GPU SXM5 benchmarks measure intra-node NVSwitch (900 GB/s) and RDMA/host-local transfers
   rather than multi-rack cross-switch fabric latency; each benchmark page states this boundary
   explicitly in its `## Limitation` section alongside the measured plain-PCIe / TCP penalties.

## Alternatives

1. **Remove `--enforce-eager` from `vllm_ec.go` `DefaultedArgs`.** Rejected because upstream vLLM
   `v0.30.0` documents `--enforce-eager` as required on disaggregated encoder instances, `MMEncoderModelRunner`
   already skips all CUDA graph capture (`capture_model() -> 0`), and V1 `GPUModelRunner`
   (`ECExampleConnector`) wastes startup time running empty LM graph capture loops when eager mode is
   off.
2. **Convert `prefill-decode.md`, `encode-prefill-decode.md`, `elastic-ep.md`, and `external-dp.md`
   into Hugo `_index.md` subdirectories.** Rejected (`jev` confidence 0.98 for data-driven `children:`)
   because moving those files breaks relative links across `docs/`, `.agents/skills/`, and
   `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh`, and forces a redundant `Overview` link under each collapsible item.
3. **Provision a separate Nebius cluster per benchmark.** Rejected (`jev` confidence 0.72 for warm
   unified batch) because cold node provisioning, image pulls, and model weight downloads dominate wall-clock
   time and cluster cost.

## Infrastructure Needed

1. Nebius project `project-e00xbnvkpr009f9sk2gcsz` (`eu-north1`, `gpu-h100-sxm` `8gpu-128vcpu-1600gb`,
   `preemptible = true`, `infiniband_fabric`, plus `cpu-d3` / `cpu-e2` `4vcpu-16gb`) or
   `project-e01yvmeppr008t3kdsqkkw` (`eu-west1`, `gpu-h200-sxm` `8gpu-128vcpu-1600gb`, `preemptible = true`)
   managed via `testing/infra/clusters/nebius/`.
2. Container registry push access (only if an operator code fix is required during live testing).
