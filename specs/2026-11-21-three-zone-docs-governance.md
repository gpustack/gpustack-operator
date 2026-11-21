# Spec: Three-Zone Documentation Governance (Public Site, LLM Reference, and Contributor Docs)

Status: Building
Blocked on: Implementation and verification of T1-T14 via my-build before PR merge.
Type: Feature

## Summary

Split GPUStack Operator's documentation into a concise human-facing Hugo site, a flat LLM reference area under `docs/reference/`, and repository-only contributor docs under `docs/contribute/`.
The Hugo site publishes only operator guides, walkthroughs, capability docs, and benchmarks, excluding `docs/reference/` and `docs/contribute/` from HTML, sidebar navigation, sitemap, and search.
Static Markdown exports serve `docs/reference/*.md` at `/reference/*.md` and index both public docs and flat references through `/llms.txt`.
Fourteen mechanical, API, telemetry, and controller-internal pages move into flat `docs/reference/`, while their displaced operator tasks keep explicit public owners.
Updated lint, site, and publication gates enforce zone isolation, a three-level heading ceiling (`H1`–`H3`), unified terminology and singular/plural rules, immutable release retries, and a three-page pilot review before rollout.

## Motivation

At clean `main` (`30f3c1a32`), `docs/` contains 82 Markdown files (30,140 lines), 22 SVGs, and 1 shell helper.
Four verified defects degrade both operator usability and LLM retrieval:

1. **No separation between human site and LLM reference**: [site/hugo.toml](../site/hugo.toml) excludes only `README.md` and `assets/**`. Hugo renders all 81 content pages into HTML and writes 87 entries into [site/layouts/index.json](../site/layouts/index.json) (81 pages plus 6 wrappers), exposing unlisted `docs/contribute/` pages, mechanical flag mappings (`parameterize.md`), integrator API matrices (`artifact-api.md`), and dense preset tables (`instance-type-unit-resources.md`).
2. **Oversized mixed-audience pages and H4 depth**: Six pages exceed 900 lines — `scaling.md` (999), `kv-cache/backend.md` (995), `configuration.md` (969), `model-delivery/remote.md` (966), `nvidia-mig.md` (946), and `walkthroughs/devices/scheduling.md` (938). Seven pages reach `H4` (`vendor-prerequisites.md`, `nvidia-mig.md`, `kv-cache/backend.md`, `model-delivery/remote.md`, `model-deployment/prefill-decode.md`, `model-deployment/status.md`, `operate/migration/bundled-subcharts.md`).
3. **Cross-page duplication and heavy walkthrough noise**: Ascend A5 UB/Host RoCE rendering rules repeat across five pages. Walkthroughs embed raw SHA-256 cache digests and long Pod YAML dumps (`devices/scheduling.md` has 545 fenced lines; `encode-prefill-decode.md` embeds three long Pod specs). `docs/walkthroughs/_index.md` omits `runtime-control.md`.
4. **Dense sweep sections without visual plots**: Four of the five benchmark introductions already state an opening trade-off (such as P/D's 25.6×–27.8× p99 ITL drop versus TTFT cost, and EPD's unequal GPU budgets). However, their body sections rely on long multi-decimal sweep tables without labeled charts or concise per-scenario deltas.

### Goals

- **Target users**: Cluster operators configuring and troubleshooting workloads on the public site; coding agents consuming `/llms.txt` and static Markdown; maintainers enforcing docs gates.
- **Self-sufficient human site**: Every common operator task (deploy, configure, verify, diagnose a pending Pod, refused KV injection, missing metrics, reverted setting, or persisted `unitResources`) succeeds using public site pages alone.
- **Clean three-zone split**: `docs/reference/` and `docs/contribute/` never generate HTML, sidebar links, sitemap entries, or search records; `docs/reference/*.md` publishes as flat static Markdown indexed via `/llms.txt`.
- **Consistent prose and structure**: Walkthroughs read naturally with reproducible commands; configuration guides use short sentences, uniform terminology, and checked singular/plural rules; all `docs/**` pages stay at `<= H3` (zero `H4`–`H6`).
- **Automated enforcement and calibrated rollout**: Updated lint, site, and publication fixtures verify all invariants, and a three-page pilot (one walkthrough, one configuration guide, one benchmark) calibrates editorial quality before bulk rewrites.

### Non-Goals

- Changing operator runtime behavior in Go, Python, or Helm chart templates (Python lint/site/benchmark-path scripts and Go doc comments referencing moved docs are in scope).
- Rewriting historical records under `specs/` (preserve newly moved implementation facts in an owning spec only when absent there).
- Enabling the disabled Chinese (`zh`) site build in [site/hugo.toml](../site/hugo.toml).
- Modifying already-published immutable release snapshots or `/charts/index.yaml` on `github-pages`.
- Discarding verified benchmark measurements or counterexamples.

## Proposal

Adopt a strict three-zone documentation model driven by one shared index in [docs/README.md](../docs/README.md).
Each zone has one visibility contract, one link policy, and one audience:

| Zone | Source paths | Site HTML & search | `/llms.txt` & Markdown | Outbound relative link policy |
| --- | --- | --- | --- | --- |
| Public Human Site | `getting-started/`, `walkthroughs/`, curated `modules/`, `operate/` | Included (61 pages + 4 section wrappers = 65 search entries) | Listed under topic sections via `/docs/.../index.md` | Public site, `docs/assets/`, and non-`docs/` repo files (rendered as source-revision GitHub URLs); **never** `docs/reference/` or `docs/contribute/` |
| LLM Reference | `docs/reference/*.md` (flat, zero subdirectories) | Excluded (zero HTML, sidebar, sitemap, or `index.json`) | Listed under `## Reference` via public static `/reference/*.md` URLs | Sibling `reference/*.md`, public site pages (rewritten to `.md`), `docs/assets/`, and repo/`contribute/` paths rewritten to source-revision GitHub URLs |
| Repo Contributor | `docs/contribute/*.md` | Excluded (zero HTML, sidebar, sitemap, or `index.json`) | Source-revision GitHub links under `## Optional` only | Any repository path |

> Note: "Excluded from site" means hidden from human HTML navigation, sitemap, and search. Static `/reference/*.md` files remain publicly fetchable over HTTP so LLM agents can read them from `/llms.txt`. "Optional only" governs `/llms.txt` listings.

### User Stories

#### Story 1
As a cluster operator, I want the public site to keep only actionable setup, configuration, and troubleshooting guides, so I can operate and diagnose workloads from any entry point without internal noise.

#### Story 2
As a developer using coding agents, I want exhaustive mechanical references in one flat `docs/reference/` folder (`<= 3` heading levels) indexed by `/llms.txt`, so agents retrieve exact parameters, APIs, and edge rules reliably.

#### Story 3
As a reader of walkthroughs and configuration guides, I want reproducible, human-voiced walkthroughs and crisp configuration prose with unified terms and checked singular/plural forms, so every page stays clear and consistent.

#### Story 4
As an engineer evaluating serving topologies or scalers, I want benchmarks to preserve honest opening trade-offs while pairing dense sweeps with labeled charts and scenario deltas, so workload fit is immediately legible.

#### Story 5
As a repository maintainer, I want automated lint, preview, and publication gates plus a three-page editorial pilot before bulk rewrites, so documentation quality and zone boundaries never silently drift.

### Core Features & Acceptance Criteria

#### 1. 81-Page Disposition Map and Public Operating Coverage

Every one of the 81 baseline content pages (`main` @ `30f3c1a32`, excluding `docs/README.md`) maps to one zone.
Seven extracted appendices and one flat navigation index (`docs/reference/README.md`) form a final target of **61 public + 25 flat reference + 3 contributor = 89 authored files** (excluding root `docs/README.md`; gates derive exact counts dynamically from `docs/README.md`).

- **Existing `docs/reference/` retained and excluded from site (3)**: `commands.md`, `settings.md`, `instance-type-unit-resources.md`.
- **Whole-page moves from `docs/modules/` into flat `docs/reference/` (14)**:
  - `devices/discovery.md` → `reference/devices-discovery.md`
  - `devices/scheduling.md` → `reference/devices-scheduling.md`
  - `devices/admission.md` → `reference/devices-admission.md`
  - `rdma/network-topology.md` → `reference/rdma-network-topology.md`
  - `topology/scheduling.md` → `reference/topology-scheduling.md`
  - `kv-cache/injection.md` → `reference/kv-cache-injection.md`
  - `model-delivery/artifact-api.md` → `reference/model-delivery-artifact-api.md`
  - `model-delivery/node-store.md` → `reference/model-delivery-node-store.md`
  - `model-deployment/parameterize.md` → `reference/model-deployment-parameters.md`
  - `model-deployment/metrics.md` → `reference/model-deployment-metrics.md`
  - `model-deployment/status.md` → `reference/model-deployment-status.md`
  - `model-deployment/shutdown.md` → `reference/model-deployment-shutdown.md`
  - `model-deployment/scaling-simulation.md` → `reference/model-deployment-scaling-simulation.md`
  - `instances/metrics.md` → `reference/instances-metrics.md`
- **Flat navigation index and extracted appendices under `docs/reference/` (8)**:
  - `reference/README.md` — Flat LLM navigation hub with task routing, exact API/condition aliases, and evidence labels (contract, recorded capture, or synthetic simulation).
  - `reference/ascend-a5-matrix.md` — Single canonical Ascend A5 UB/UBG/UBoE/Host RoCE rendering and mount matrix deduplicated across five pages.
  - `reference/devices-mig-transcripts.md` — Three-configuration NVIDIA MIG transcript extracted from `modules/devices/nvidia-mig.md`.
  - `reference/devices-scheduling-capture.md` — Raw multi-object `kubectl` capture archive extracted from `walkthroughs/devices/scheduling.md`.
  - `reference/kv-cache-backend-internals.md` — Vendor rendering details and full condition tables extracted from `modules/kv-cache/backend.md`.
  - `reference/model-delivery-remote-internals.md` — Source-memory arithmetic examples, ModelExpress sidecar/env internals, and raw sweep details extracted from `modules/model-delivery/remote.md`.
  - `reference/model-deployment-internals.md` — Operator-owned key matrices, render-version upgrade rules, and exhaustive refusal catalog extracted from `modules/model-deployment/configuration.md`.
  - `reference/model-deployment-scaling-internals.md` — Capacity-prior qualification, normalization, and predictor internals extracted from `modules/model-deployment/scaling.md`.
- **Public Human Site retained and refined (61 pages across 10 sidebar groups)**:
  - `Getting Started` (2): `architecture.md`, `vendor-prerequisites.md`.
  - `Walkthroughs` (14): `_index.md` (add `runtime-control.md`), `devices/scheduling.md`, `rdma/network-endpoints.md`, `topology/hierarchical-placement.md`, `kv-cache/shared-cache.md`, `model-delivery/prefetch.md`, `model-deployment/_index.md`, `prefill-decode.md`, `encode-prefill-decode.md`, `scaling.md`, `runtime-control.md`, `elastic-ep.md`, `external-dp.md`, `multi-host.md`.
  - `Heterogeneous Devices` (6): `_index.md`, `preflight.md`, `requests.md`, `nvidia-mig.md`, `hygon-mig.md`, `thead-mig.md`.
  - `RDMA Networking` (2): `_index.md`, `operations.md`.
  - `Topology` (2): `_index.md`, `operations.md`.
  - `KV Cache` (6): `_index.md`, `backend.md`, `pool.md`, `leader.md`, `local-disk-tier.md`, `disk-heavy-nodes.md`.
  - `Model Delivery` (7): `_index.md`, `artifact.md`, `image-source.md`, `prefetch.md`, `node-to-node-sync.md`, `remote.md`, `operations.md`.
  - `Model Deployment` (15): `_index.md`, `configuration.md`, `engine-versions.md`, `prefill-decode.md`, `prefill-decode-benchmark.md`, `encode-prefill-decode.md`, `encode-prefill-decode-benchmark.md`, `elastic-ep.md`, `elastic-ep-benchmark.md`, `external-dp.md`, `external-dp-benchmark.md`, `scaling.md`, `scaling-benchmark.md`, `runtime-control.md`, `routing.md`.
  - `GPU Instances` (1): `_index.md`.
  - `Operations` (6): `installation-modes.md`, `high-availability.md`, `migration/from-v0.5.md`, `migration/bundled-subcharts.md`, `migration/enforced-binding-dtype.md`, `migration/troubleshooting.md`.
- **Repo-only `docs/contribute/` (3)**: `development.md`, `local-cluster-setup.md`, `internals.md`.

**Mandatory Public Operating Coverage Matrix** (covers all 14 moved module pages and all 3 existing references, preventing broken human journeys across the 52 retained pages that currently link into excluded docs):

| Displaced source(s) | Public owner page(s) | Minimum operator task retained on public site (symptom, command/config, expected signal, next action) |
| --- | --- | --- |
| `devices/discovery.md`, `scheduling.md`, `admission.md` | `modules/devices/preflight.md`, `requests.md` | Pick supported isolation mode; inspect pool defaults and queue quota; distinguish webhook refusal, queued demand, and per-device fragmentation. |
| `rdma/network-topology.md`, `topology/scheduling.md` | `modules/rdma/operations.md`, `modules/topology/operations.md` | Inspect `Devices` NIC record when `rdma.capable` is absent; diagnose pending replicas via requested level, PodSet fit, and `Workload` conditions. |
| `kv-cache/injection.md` | `modules/kv-cache/pool.md`, `walkthroughs/kv-cache/shared-cache.md` | Resolve refused engine launch; verify pool binding on admitted Pod; keep tenant compatibility rule inline. |
| `model-delivery/artifact-api.md`, `node-store.md` | `modules/model-delivery/artifact.md`, `operations.md` | State required RBAC for `progress`; show where to inspect failed mounts; distinguish namespace/UID/digest refusal from transient unavailability. |
| `model-deployment/parameterize.md`, `status.md`, `shutdown.md` | `modules/model-deployment/configuration.md`, `routing.md`, `scaling.md`, `runtime-control.md` | Keep owned/defaulted/take-over rules; diagnose readiness and endpoint eligibility; explain withdrawal, grace window, stalled retirement, and paused-vs-retiring lifecycle. |
| `model-deployment/metrics.md`, `scaling-simulation.md` | `modules/model-deployment/scaling.md`, `scaling-benchmark.md` | Inspect partial samples and missing sources; label synthetic simulation figures clearly whenever cited beside hardware runs. |
| `instances/metrics.md` | `modules/instances/_index.md` | Show one utilization query, required subresource RBAC, legitimate pre-start zero, unavailable-source response, and omitted accelerator sample. |
| `reference/settings.md`, `commands.md` | `operate/installation-modes.md`, `modules/devices/preflight.md`, `modules/model-delivery/operations.md` | Explain `Setting` vs env-var precedence and restart-revert inspection command; keep day-1 `preflight` and cache commands inline. |
| `reference/instance-type-unit-resources.md` | `modules/instances/_index.md`, `walkthroughs/devices/scheduling.md` | Explain default/cap CPU and RAM sizing, create-time immutability of `spec.unitResources`, overcommit effects, and choosing between a custom sibling vs recreating a derived type (keeping the 12-vendor table in reference). |

#### 2. Single-Source Indexing, Link Rewriting, Preview Parity, and Versioned Publication

- **Single authoritative index in `docs/README.md`**:
  - Maintain three distinct inventory tables in [docs/README.md](../docs/README.md): `## All pages` (61 public pages), `## Reference` (flat `docs/reference/*.md` including `reference/README.md`), and `## Contribute` (3 repo-only pages).
  - [site/layouts/home.llms.txt](../site/layouts/home.llms.txt) reads all three tables from the mounted `assets/doc-index/README.md`: resolves `All pages` via `site.GetPage` → `.OutputFormats.Get "Markdown"`, derives versioned `/reference/<file>.md` URLs directly from `## Reference` (zero duplicate summaries across two READMEs), and renders `## Contribute` as source-revision GitHub URLs under `## Optional`.
- **Hugo mounts, wrappers, and navigation cleanup**:
  - In [site/hugo.toml](../site/hugo.toml), exclude `"README.md"`, `"assets/**"`, `"reference/**"`, and `"contribute/**"` from `content/docs`, preserve existing `layouts`, `assets`, and `data` mounts, and mount the staged directory `site/resources/reference` → `static/reference` with `includeFiles = ["*.md"]` as the sole source of published reference files.
  - Delete `site/content/docs/reference/_index.md` and `site/content/docs/contribute/_index.md`; remove `Reference` from [site/content/docs/_index.md](../site/content/docs/_index.md) and remove obsolete `groups`, `order`, `children`, and `labels` entries (`parameterize`, `artifact-api`, `scaling-simulation`, `reference/*`, `contribute/*`) in [site/data/navigation.yaml](../site/data/navigation.yaml).
  - Tighten [site/layouts/partials/link-destination.html](../site/layouts/partials/link-destination.html) so links to excluded `docs/reference/` or `docs/contribute/` targets fail verification instead of falling back to dead site paths, while retaining source-revision GitHub URL rendering for valid non-`docs/` repository links (such as chart README and owning specs).
- **Deterministic Markdown link rewriting & local preview parity**:
  - In [hack/site.sh](../hack/site.sh), mirror and rewrite `docs/reference/*.md` into a fresh `site/resources/reference/` staging directory (clearing deleted files on each run) via `hack/site/rewrite_reference.py` before invoking Hugo so BOTH `make site` and `make site-serve` serve identical rewritten `/reference/*.md` bytes. Document that editing `docs/reference/*.md` during an active `make site-serve` session requires restarting `make site-serve` to refresh static bytes.
  - Rewrite rules outside code fences, inline code, and HTML/Mermaid comments:
    - Public regular page (`../modules/.../foo.md#a`) → `../docs/modules/.../foo/index.md#a`.
    - Public section index (`../modules/.../_index.md#a`) → `../docs/modules/.../index.md#a`.
    - Sibling reference (`settings.md#a`) → `settings.md#a`.
    - Shared diagram asset (`../assets/diagrams/x.svg`) → `../docs/assets/diagrams/x.svg`.
    - Repo source file or `docs/contribute/*.md` (for example `commands.md` / `settings.md` links to `contribute/`) → source-revision GitHub `blob/<sourceRef>/...` URL (or removed).
- **Legacy-default release, immutable retries, and Pages publication** ([hack/site/publish.py](../hack/site/publish.py), [hack/site/verify.py](../hack/site/verify.py)):
  - Derive root `llms.txt` and root `/reference/*.md` exclusively from the selected `latest` version, rewriting root `/reference/*.md` links against `<root_url><latest>/docs/...`.
  - If `latest` is a legacy stable release without `reference/`, remove any stale root `/reference/` directory so root `llms.txt` and root `/reference/` stay consistent.
  - When a new stable release promotes to `latest`, replace root `/reference/` atomically so renamed or deleted aliases disappear completely.
  - **Immutable tag retry contract**: Retain existing rejection when a published tag points to a different commit. When a published tag and commit match an existing snapshot, compare the built file inventory against the published snapshot: treat an identical inventory as an idempotent no-op retry; if any byte differs, reject before modifying the snapshot, root files, version manifest, or charts.
  - Extend `verify.py` to verify BOTH expected root/version bytes AND the absence of obsolete root `/reference/` aliases after a rename or promotion, keeping `/charts/` byte-identical.

#### 3. Writing Standards, Terminology, Singular/Plural Rules, and Visual/Heading Discipline

- **Walkthroughs (`docs/walkthroughs/**`)**:
  - Use a natural, imperative human voice following `.agents/skills/gpustack-operator-docs/`: action → expected observation → recovery.
  - Keep tested software versions, runnable manifests/scripts, and small labeled output excerpts. Preserve captured-vs-illustrative distinction.
  - Keep immutable functional inputs when a command depends on them (such as a pinned model revision matching an explicit file list), but strip unreferenced raw SHA-256 cache digests, repeated raw Pod UIDs (show equality concisely), and multi-hundred-line raw YAML dumps.
- **Configuration and capability guides (`docs/modules/**`, `docs/getting-started/**`, `docs/operate/**`)**:
  - Apply 80% ASD-STE100 sentence targets with concrete before/after examples: short sentences (`<= 20` words procedural, `<= 25` words descriptive), one idea per sentence, active voice, condition before instruction, plain verbs, zero AI filler.
  - Keep prerequisites, defaults, permissions, and failure branches at the point of action. State each rule once on its owning page; omit generic Kubernetes basics and self-evident fields.
  - Use complete two-space Pretty YAML (`apiVersion`, `kind`, `metadata`) with inline `#` comments for full examples and `diff` blocks for edits. Add micro-bullets or comparison tables only when they add real constraints or compare choices.
- **Unified glossary and singular/plural rules**:
  - Maintain one shared **Glossary & Plurality Table** in `.agents/skills/gpustack-operator-docs/references/conventions.md`.
  - Standardize Kubernetes kind plurals in authored prose (`Pod`s, `Node`s, `Instance`s, `Topology` resources, inherently plural `Devices`), sibling menu/heading plurality, and count-noun agreement. Exempt fenced code blocks, CLI flags, API paths, YAML keys, and captured output.
- **Benchmarks and data storytelling**:
  - Preserve honest opening trade-offs (baseline, hardware, concurrency, units, statistic, sample scope, counterexamples, and failed runs).
  - Match visual format to evidence: labeled SVG line/bar chart for multi-point sweeps (with accessible labels and textual takeaway), key matched comparison tables directly beneath the charts on the public site, SVG diagram + hidden Mermaid comment for mechanisms (never Mermaid flowcharts for numeric sweeps). Move only verbose raw log counts or uncondensed secondary dumps to `docs/reference/`. Use bold keyword leads only where they discriminate choices.
- **Headings, subsections, and footnotes**:
  - Enforce a hard **3-level heading ceiling (`H1` → `H2` → `H3`)** across all `docs/**/*.md` (flatten all 7 existing `H4` pages; convert 10 H4 conditions in `status.md` into a concise table or linkable H3s while updating inbound anchors).
  - Keep headings to `<= 4` words and `<= 32` visible characters after numbered prefixes, sentence case below `H1`, zero interrogatives. Merge thin subsections rather than padding outlines.
  - Keep decision-changing caveats next to the value; use short GFM footnotes (`[^1]`, one short sentence) only for secondary precision/provenance notes.
  - Reading footer: public pages end with `---`, `**See also**`, and `**Next**` (where a next step exists); `docs/reference/` topic pages end with `---` and `**See also**`. Exempt `docs/reference/README.md` (alongside `docs/README.md`) from mandatory `## Contents`, self-listing, and `**Next**`.

#### 4. Automated Gates, Consumer Audits, and Editorial Calibration

- **Updated `check-docs.sh` and `check-docs-selftest.sh`**:
  - Enforce `MAX_HEADING_DEPTH=3` across `docs/**` (with fenced/indented negative fixtures).
  - Enforce flat `docs/reference/` layout (zero subdirectories).
  - Enforce one-way link isolation across relative, root-relative, and same-origin URLs (including `site/content/**`), verifying intended source and rendered destination.
  - Enforce distinct index coverage for `All pages` (public), `Reference` (`docs/reference/*.md`, with explicit `docs/reference/README.md` policy), and `Contribute`.
  - Add an authored-prose singular/plural checker that specifically catches malformed code-annotated kind plurals (such as `` `Pods` ``, `` `Nodes` ``, `` `Instances` ``, `` `Topology`s ``) while exempting literal tokens, CLI flags, API paths, fences, and captured output, backed by positive and negative selftests.
  - Keep the six 900+ line pages as review triggers for reference extraction rather than imposing an artificial line cap.
- **Updated `agent-index.py` and `site-links.py`**:
  - Assert zero `site/public/docs/reference/`, `site/public/docs/contribute/`, or retired moved-module HTML outputs (all 22 retired URLs) and zero excluded entries in `index.json` or `sitemap.xml`.
  - Validate `/llms.txt` bi-directionally against actual outputs (origin, multiplicity, H1/description, `Reference`, and GitHub `Optional`), plus verify every `/reference/*.md` export's body/code integrity and `#anchor` resolution (including duplicate slugs, multiline links, footnotes, and code-block lookalikes).
- **Repo-wide consumer audit**:
  - Update all references to moved/excluded paths across Markdown, Python, Go comments, and JSON review rules (`README.md`, `.agents/skills/gpustack-operator-benchmark/scripts/{autoscaling_qualification.py,test_autoscaling_qualification.py}`, `.opencodereview/rule.json`, `pkg/device/doc.go`, and `pkg/devicemanager/detector/hygon/process.go`).
  - Keep `docs/reference/settings.md` and `docs/modules/model-delivery/remote.md` at their existing paths so `.auto-sync/*` and `pkg/nodefeature/unit_resources_preset.yaml` keep working without disruption.
- **Pilot calibration and editorial-taste review (`gpt-6.2-astra`)**:
  - Before bulk-rewriting all 61 public pages, apply the editorial standards to one Walkthrough, one Configuration guide, and one Benchmark page, verify operator-task pass/fail, and run an independent Codex (`--model gpt-6.2-astra --effort high`) review to approve the style baseline.

#### Consolidated Acceptance Table

| Area | Verification criterion |
| --- | --- |
| Public operator journeys | Starting from public entry points, an operator resolves pending Pod, refused KV injection, missing metrics, reverted setting, and persisted `spec.unitResources` using public pages and site search alone. |
| Zone isolation | `site/public` has zero HTML, sidebar, sitemap, or `index.json` entries across all 22 retired paths (`reference/`, `contribute/`, and 14 moved module paths); 65 docs search entries (61 public + 4 wrappers); zero public links into excluded zones. |
| Reference & `/llms.txt` | All 25 flat `docs/reference/*.md` files export via `site/resources/reference/` to `/reference/*.md` with valid rewritten links and `#anchors`, match `/llms.txt` bi-directionally, and serve identically under `make site` and `make site-serve`. |
| Publication fixtures | Local `publish.py` + `verify.py` fixtures pass for legacy-stable default + new `main`, new-stable promotion (asserting obsolete root alias removal), idempotent same-tag/commit retry, and rejection of same-tag/commit retry with changed bytes, with `/charts/` unchanged. |
| Style & depth gates | `make lint docs` passes with zero `H4+` headings in `docs/**`, zero nested dirs in `docs/reference/`, singular/plural selftests passing, and zero stale consumers in Markdown, Python, Go, or JSON rules. |
| Editorial taste | Three-page pilot and final rewrite wave pass independent `gpt-6.2-astra` (`effort: high`) review. |

### Notes / Constraints / Caveats

- **Toolchain & environment**: Local macOS/Linux workspace (`Python 3.11.9`, macOS `onetrueawk 20200816`, repository-pinned `.sbin/hugo v0.152.2+extended`), Bash/awk (`check-docs.sh`), and Python (`agent-index.py`, `site-links.py`, `publish.py`, `verify.py`). No Kubernetes cluster, Node.js runtime, or live GitHub Pages push is required during implementation.
- **Spec status gate**: [check-specs.sh](../.agents/skills/gpustack-operator-docs/scripts/check-specs.sh) checks every file under `specs/` without a branch check: line 3 must be `Status: <Shipped|Building|Planned|Built>`, and non-`Shipped` states require a non-empty `Blocked on:` on line 4. Set `Status: Shipped` before opening the PR to `main`.
- **Retired HTML URLs (resolved)**: Removing 14 moved `modules/` pages, 3 `reference/` pages, 3 `contribute/` pages, and 2 section landings retires **22 HTML URLs** cleanly on new builds with no redirect stubs. Document the URL retirement in release notes; older published tags remain immutable on `github-pages`.
- **Benchmark sweep tables (resolved)**: Public benchmark pages retain their key matched per-concurrency comparison tables directly beneath the scenario decision summary and labeled SVG charts; only verbose raw access-log counts or uncondensed secondary dumps move to `docs/reference/`.

### Boundaries

- **Always:**
  - Run `make lint docs`, `make site`, and `SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site` after template, checker, or doc changes.
  - Keep `docs/README.md`, `.agents/skills/gpustack-operator-docs/`, and repo-wide consumers in sync in the same commit as any page move.
  - Keep `docs/getting-started/architecture.md` at `<= 200` lines and preserve verified measurement limits.
  - Keep `docs/reference/settings.md`, `docs/reference/instance-type-unit-resources.md`, and `docs/modules/model-delivery/remote.md` at their existing paths to preserve `.auto-sync/*` and code-pinned invariants.
- **Ask first:**
  - Deleting verified benchmark evidence that has no owning record in `specs/` or `docs/reference/`.
  - Changing slugs of pages that remain on the public human site.
- **Never:**
  - Expose `docs/reference/**` or `docs/contribute/**` in Hugo HTML, sidebar, sitemap, or `index.json`.
  - Link any public site page (`docs/` or `site/content/`) to `docs/reference/**` or `docs/contribute/**`.
  - Allow `H4`–`H6` headings in `docs/**` or subdirectories in `docs/reference/`.
  - Hand-edit generated chart docs (`deploy/gpustack-operator/chart/README.md`, `values.schema.json`).

### Risks and Mitigations

- **Stripping day-2 tasks when moving 14 module pages and hiding 3 reference pages (205 current inbound link occurrences across 52 retained pages)** → Enforce the Public Operating Coverage Matrix in Wave 2 before cutting over in Wave 3, and run reader-journey checks before closing.
- **Broken root `/reference/` when `latest` is an older stable release or on mutated same-tag retries** → Gate root `/reference/` sync on `latest`, reject mutated same-tag/commit bytes, and assert stale alias absence in local `publish.py` + `verify.py` fixtures.
- **Silent drift between `make site` and `make site-serve`** → Fresh-mirror rewritten reference Markdown into `site/resources/reference/` via `hack/site/rewrite_reference.py` before Hugo builds or serves.
- **Hidden non-Markdown consumers breaking after moves** → Audit and update Python scripts/tests (`autoscaling_qualification.py`), Go doc comments (`pkg/device/doc.go`, `pkg/devicemanager/detector/hygon/process.go`), and `.opencodereview/rule.json` in the same cutover task (T8), and verify `.auto-sync` via `autosync-selftest.sh`.
- **Mechanical rewriting degrading readability** → Calibrate editorial rules on a three-page pilot (T10) and pass `gpt-6.2-astra` review before sweeping all 61 public pages in parallel.

## Design Details

### Commands

Execution environment: **local workspace** (verified `Python 3.11.9`, macOS `onetrueawk 20200816`, repository-pinned `.sbin/hugo v0.152.2+extended`; no cluster or network deployment required).

```bash
# Full docs lint gate (self-tests, structure/heading/plural checks, skills, Hugo build, links, index)
make lint docs

# Build site locally and verify HTML + Markdown exports + /llms.txt + /reference/*.md
make site

# Verify production-style versioned base URL rewriting
SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site

# Preview locally with identical rewritten /reference/*.md output
make site-serve

# Advisory metrics report during refactoring
bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh --report

# Run checker and site Python/Bash self-tests directly
bash .agents/skills/gpustack-operator-docs/scripts/check-docs-selftest.sh .
bash .agents/skills/gpustack-operator-docs/scripts/check-crossrefs-selftest.sh .
bash .agents/skills/gpustack-operator-docs/scripts/check-specs-selftest.sh .
bash hack/check/autosync-selftest.sh .
python3 -m unittest -v hack/site/test_rewrite_reference.py hack/site/test_publish_verify.py hack/check/test_agent_index.py
python3 -m unittest -v .agents/skills/gpustack-operator-benchmark/scripts/test_autoscaling_qualification.py
```

### Project Structure

```text
docs/
├── README.md                    # Single source of truth: Reading paths, Module map, All pages (61), Reference (25), Contribute (3)
├── assets/diagrams/             # SVG diagrams and benchmark charts (paired with hidden Mermaid where applicable)
├── getting-started/             # Public Site (2)
├── walkthroughs/                # Public Site (14)
├── modules/                     # Public Site (39 curated pages across 7 modules)
├── operate/                     # Public Site (6)
├── reference/                   # Flat LLM Reference (25 .md files, zero subdirs, <= H3, exported via /llms.txt)
│   ├── README.md                # Flat LLM navigation hub with aliases and evidence types
│   ├── commands.md
│   ├── settings.md
│   ├── instance-type-unit-resources.md
│   ├── ascend-a5-matrix.md
│   └── <module>-<topic>.md
└── contribute/                  # Repo-only (3): development, local-cluster-setup, internals

site/
├── hugo.toml                    # Excludes reference/** & contribute/**; mounts staged resources/reference/*.md -> static/reference
├── resources/reference/         # Gitignored fresh-mirrored rewritten reference/*.md staged by hack/site/rewrite_reference.py
├── data/navigation.yaml         # 10 human sidebar groups (no Reference, no Contribute, obsolete entries removed)
├── layouts/home.llms.txt        # Reads All pages + Reference + Contribute from assets/doc-index/README.md
└── layouts/index.json           # Indexes 65 public docs entries (61 pages + 4 wrappers)

hack/
├── site.sh                      # Runs rewrite_reference.py into site/resources/reference, Hugo build/serve, site-links.py, agent-index.py
├── site/
│   ├── rewrite_reference.py     # Deterministic reference/*.md link rewriter & fresh-mirror stager
│   ├── test_rewrite_reference.py
│   ├── publish.py               # Composes versioned Pages + atomic root llms.txt & root reference/ + immutable retry check
│   ├── verify.py                # Verifies root + versioned bytes and absence of obsolete root aliases
│   └── test_publish_verify.py   # Local composition fixtures (legacy-default, promotion, idempotent & mutated retry)
└── check/
    ├── agent-index.py           # Verifies module map, zone isolation, bi-directional llms.txt, and reference #anchors
    ├── test_agent_index.py      # Fixture tests for zone leaks, anchor validation, multiline links, and footnotes
    └── site-links.py            # Verifies rendered HTML links, anchors, and absence of retired URLs
```

### Code Style

Target Hugo mount configuration in [site/hugo.toml](../site/hugo.toml) (retaining existing `layouts`, `assets`, and `data` mounts while mounting only `.md` files from the fresh-mirrored `site/resources/reference` staging directory):

```toml
[module]
  [[module.mounts]]
    source = "content"
    target = "content"
  [[module.mounts]]
    source = "../docs"
    target = "content/docs"
    excludeFiles = ["README.md", "assets/**", "reference/**", "contribute/**"]
  [[module.mounts]]
    source = "../docs/assets"
    target = "static/docs/assets"
  [[module.mounts]]
    source = "resources/reference"
    target = "static/reference"
    includeFiles = ["*.md"]
  [[module.mounts]]
    source = "layouts"
    target = "layouts"
  [[module.mounts]]
    source = "assets"
    target = "assets"
  [[module.mounts]]
    source = "data"
    target = "data"
  [[module.mounts]]
    source = "../docs"
    target = "assets/doc-index"
    includeFiles = ["README.md"]
```

Key conventions:
- **Short, direct prose**: Active voice, condition before instruction, plain verbs, and one idea per sentence.
- **Checked terminology & plurality**: Follow the shared glossary and kind-plural rules in `conventions.md`; keep literal identifiers, API paths, and captured outputs verbatim.
- **Complete runnable YAML**: Two-space Pretty YAML with full Kubernetes headers (`apiVersion`, `kind`, `metadata`); use `diff` blocks for edits.
- **Three-level headings**: `H1` → `H2` → `H3` max; `<= 4` words and `<= 32` chars after numbered prefixes; zero interrogatives.
- **Portability & self-tests**: Keep `check-docs.sh` compatible with macOS `onetrueawk` (no GNU `awk` extensions); pair every new Bash/Python lint or rewrite rule with positive and negative fixtures.

### Implementation Plan

Expand–contract DAG: Wave 1 builds additive tooling and rules with zero breakage to current `main`; Wave 2 lands Public Operating Coverage and removes excluded outbound links across three parallel module batches; Wave 3 performs the atomic three-zone cutover and appendix extraction; Wave 4 calibrates prose and chart quality on a three-page pilot gated by `gpt-6.2-astra`; Wave 5 executes parallel editorial rewrites; Wave 6 verifies end-to-end parity and signs off.

#### Wave 1 — Tooling, Publication Hardening, and Style Contract (Expand)

- [x] **T1 · Reference link rewriter and fresh-mirror stager**
      Blocked by: None
      Owns: `hack/site/rewrite_reference.py`, `hack/site/test_rewrite_reference.py`
      Gate: review
      Acceptance: Rewrites relative links in `docs/reference/*.md` outside fences, inline code, and HTML/Mermaid comments across all target classes (public regular pages, `_index.md`, sibling references, `../assets/diagrams/`, repo/`contribute/` → GitHub `sourceRef` URLs) while preserving queries, `#anchors`, multiline links, and footnotes; mirrors into a target staging directory and removes deleted files; supports both version-local (`../docs`) and Pages-root (`<latest>/docs`) prefixes.
      Verify: `python3 -m unittest -v hack/site/test_rewrite_reference.py`

- [x] **T2 · Pages publication root `/reference/` sync and immutable retry guard**
      Blocked by: T1
      Owns: `hack/site/publish.py`, `hack/site/verify.py`, `hack/site/test_publish_verify.py`, `.github/workflows/site.yml`
      Gate: review
      Acceptance: `publish.py` mirrors root `/reference/*.md` exclusively from the selected `latest` version (rewriting root links under `<root_url><latest>/docs/...`), deletes stale root `/reference/` when `latest` is a legacy stable release, replaces root `/reference/` atomically on promotion, treats same-tag/commit with identical inventory as an idempotent retry, and rejects same-tag/commit with changed bytes before mutating snapshot, root, manifest, or charts. `verify.py` validates both expected root/version bytes and absence of obsolete root aliases. Local fixture tests cover all four scenarios with `/charts/` byte-identical.
      Verify: `python3 -m unittest -v hack/site/test_publish_verify.py`

- [x] **T3 · Style conventions, glossary, and singular/plural specification in docs skill**
      Blocked by: None
      Owns: `.agents/skills/gpustack-operator-docs/references/conventions.md`
      Acceptance: Updates `conventions.md` with the three-zone boundary rules, Walkthrough vs Configuration vs Benchmark prose standards (including concrete before/after examples), the shared Glossary & Singular/Plurality Table, `H1`–`H3` heading ceiling, GFM footnote rules, and `docs/reference/README.md` structural policy without breaking existing `docs/` path checks.
      Verify: `make lint docs`

- [x] **T4 · Three-zone lint, singular/plural checker, and Markdown anchor verification gates**
      Blocked by: T1
      Owns: `.agents/skills/gpustack-operator-docs/scripts/check-docs.sh`, `.agents/skills/gpustack-operator-docs/scripts/check-docs-selftest.sh`, `hack/check/agent-index.py`, `hack/check/test_agent_index.py`, `hack/check/site-links.py`, `site/layouts/partials/markdown-content.html`
      Gate: review
      Acceptance: Adds onetrueawk-compatible checks in `check-docs.sh` (with positive/negative cases in `check-docs-selftest.sh`) for `MAX_HEADING_DEPTH=3`, flat `docs/reference/`, one-way link isolation across `docs/` and `site/content/`, three-table `docs/README.md` (`All pages`, `Reference`, `Contribute`) + `docs/reference/README.md` policy, and authored-prose kind-plural lint (catching `` `Pods` `` / `` `Nodes` `` / `` `Instances` `` / `` `Topology`s `` while exempting literal tokens, CLI flags, API paths, fences, and captured output). Updates `agent-index.py` + `test_agent_index.py` and `site-links.py` for zero excluded HTML/search/sitemap leaks, bi-directional `/llms.txt` parity, and `/reference/*.md` `#anchor` validation (duplicate slugs, multiline links, footnotes, code-block lookalikes). Strict three-zone mode activates automatically when `docs/reference/README.md` is present so current `main` stays green until T8.
      Verify: `bash .agents/skills/gpustack-operator-docs/scripts/check-docs-selftest.sh . && python3 -m unittest -v hack/check/test_agent_index.py && make lint docs`

#### Wave 2 — Public Operating Coverage and H4 Flattening (Parallel Disjoint Batches)

- [x] **T5 · Devices, RDMA, Topology, Instances & Vendor Prerequisites: public task retention and H4 flattening**
      Blocked by: T3
      Owns: `docs/modules/devices/{_index.md,preflight.md,requests.md,nvidia-mig.md,hygon-mig.md,thead-mig.md}`, `docs/modules/rdma/{_index.md,operations.md}`, `docs/modules/topology/{_index.md,operations.md}`, `docs/modules/instances/_index.md`, `docs/getting-started/vendor-prerequisites.md`, `docs/walkthroughs/devices/scheduling.md`, `docs/walkthroughs/rdma/network-endpoints.md`, `docs/walkthroughs/topology/hierarchical-placement.md`
      Acceptance: Retains every assigned minimum operator task from the Public Operating Coverage Matrix (`discovery`, `scheduling`, `admission`, `network-topology`, `topology/scheduling`, `instances/metrics`, `commands`, `instance-type-unit-resources`) inside the named public owners; flattens all `H4` headings in `vendor-prerequisites.md` and `nvidia-mig.md` to `<= H3`; removes all outbound relative links to `docs/reference/**`, `docs/contribute/**`, and the 14 moving module pages.
      Verify: `make lint docs`

- [x] **T6 · KV Cache & Model Delivery: public task retention and H4 flattening**
      Blocked by: T3
      Owns: `docs/modules/kv-cache/{_index.md,backend.md,pool.md,leader.md,local-disk-tier.md,disk-heavy-nodes.md}`, `docs/modules/model-delivery/{_index.md,artifact.md,image-source.md,prefetch.md,node-to-node-sync.md,remote.md,operations.md}`, `docs/walkthroughs/kv-cache/shared-cache.md`, `docs/walkthroughs/model-delivery/prefetch.md`
      Acceptance: Retains every assigned minimum operator task from the Public Operating Coverage Matrix (`kv-cache/injection.md`, `artifact-api.md`, `node-store.md`, `settings.md`, `commands.md`) inside the named public owners; flattens all `H4` headings in `kv-cache/backend.md` and `model-delivery/remote.md` to `<= H3`; separates ModelExpress configuration cleanly from benchmark results in `remote.md`; removes all outbound relative links to `docs/reference/**`, `docs/contribute/**`, and the 14 moving module pages.
      Verify: `make lint docs`

- [x] **T7 · Model Deployment, Architecture & Operations: public task retention and H4 flattening**
      Blocked by: T3
      Owns: `docs/modules/model-deployment/{_index.md,configuration.md,engine-versions.md,prefill-decode.md,prefill-decode-benchmark.md,encode-prefill-decode.md,encode-prefill-decode-benchmark.md,elastic-ep.md,elastic-ep-benchmark.md,external-dp.md,external-dp-benchmark.md,scaling.md,scaling-benchmark.md,runtime-control.md,routing.md}`, `docs/walkthroughs/_index.md`, `docs/walkthroughs/model-deployment/**`, `docs/getting-started/architecture.md`, `docs/operate/**`
      Acceptance: Retains every assigned minimum operator task from the Public Operating Coverage Matrix (`parameterize.md`, `status.md`, `shutdown.md`, `metrics.md`, `scaling-simulation.md`, `settings.md`) inside the named public owners; adds missing `runtime-control.md` link to `docs/walkthroughs/_index.md`; flattens all `H4` headings in `prefill-decode.md` and `operate/migration/bundled-subcharts.md` to `<= H3`; keeps `architecture.md <= 200` lines; removes all outbound relative links to `docs/reference/**`, `docs/contribute/**`, and the 14 moving module pages.
      Verify: `make lint docs`

#### Wave 3 — Atomic Three-Zone Cutover and Appendix Extraction (Contract)

- [x] **T8 · Atomic three-zone switchover: move 14 pages, enable Hugo/llms.txt/nav split, update all repo consumers**
      Blocked by: T1, T4, T5, T6, T7
      Owns: `docs/reference/{README.md,commands.md,settings.md,instance-type-unit-resources.md,devices-discovery.md,devices-scheduling.md,devices-admission.md,rdma-network-topology.md,topology-scheduling.md,kv-cache-injection.md,model-delivery-artifact-api.md,model-delivery-node-store.md,model-deployment-parameters.md,model-deployment-metrics.md,model-deployment-status.md,model-deployment-shutdown.md,model-deployment-scaling-simulation.md,instances-metrics.md}`, `docs/README.md`, `docs/contribute/{internals.md,development.md}`, `docs/modules/devices/{discovery.md,scheduling.md,admission.md}`, `docs/modules/rdma/network-topology.md`, `docs/modules/topology/scheduling.md`, `docs/modules/kv-cache/injection.md`, `docs/modules/model-delivery/{artifact-api.md,node-store.md}`, `docs/modules/model-deployment/{parameterize.md,metrics.md,status.md,shutdown.md,scaling-simulation.md}`, `docs/modules/instances/metrics.md`, `site/hugo.toml`, `site/data/navigation.yaml`, `site/layouts/home.llms.txt`, `site/layouts/partials/link-destination.html`, `site/content/docs/_index.md`, `site/content/docs/reference/**`, `site/content/docs/contribute/**`, `hack/site.sh`, `README.md`, `.opencodereview/rule.json`, `pkg/device/doc.go`, `pkg/devicemanager/detector/hygon/process.go`, `.agents/skills/gpustack-operator-benchmark/scripts/{autoscaling_qualification.py,test_autoscaling_qualification.py}`, `.agents/skills/gpustack-operator-docs/{SKILL.md,references/page-map.md,references/conventions.md}`, `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Gate: review
      Acceptance: Moves all 14 module pages into flat `docs/reference/<module>-<topic>.md` (flattening 10 H4 conditions in `model-deployment-status.md` to `<= H3`); creates `docs/reference/README.md` (with task routing, API/condition aliases, and evidence labels); rewrites or removes `contribute/` links in `commands.md`/`settings.md`; updates `docs/README.md` to the three-table index (`All pages` 61, `Reference` 18, `Contribute` 3); updates `site/hugo.toml`, `home.llms.txt`, `link-destination.html`, `navigation.yaml` (10 groups, obsolete entries removed), `site/content/docs/` (deletes `reference/` and `contribute/` wrappers), and `hack/site.sh` (runs `rewrite_reference.py` before Hugo build and serve); updates all non-`docs/` consumers (`README.md`, `.opencodereview/rule.json`, Go doc comments, benchmark Python scripts/tests, and skill routing tables). Strict three-zone gates pass with 65 docs search entries and zero files at all 22 retired HTML paths.
      Verify: `make lint docs && SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site && python3 -m unittest -v .agents/skills/gpustack-operator-benchmark/scripts/test_autoscaling_qualification.py`

- [x] **T9 · Extract 7 oversized appendices and deduplicate Ascend A5 matrix into flat `docs/reference/`**
      Blocked by: T8
      Owns: `docs/reference/{README.md,ascend-a5-matrix.md,devices-mig-transcripts.md,devices-scheduling-capture.md,kv-cache-backend-internals.md,model-delivery-remote-internals.md,model-deployment-internals.md,model-deployment-scaling-internals.md}`, `docs/README.md`, `docs/getting-started/vendor-prerequisites.md`, `docs/modules/devices/{preflight.md,nvidia-mig.md}`, `docs/walkthroughs/devices/scheduling.md`, `docs/modules/kv-cache/backend.md`, `docs/modules/model-delivery/remote.md`, `docs/modules/model-deployment/{configuration.md,prefill-decode.md,scaling.md}`, `.agents/skills/gpustack-operator-docs/{SKILL.md,references/page-map.md}`
      Acceptance: Extracts all 7 dense appendices (`ascend-a5-matrix.md`, `devices-mig-transcripts.md`, `devices-scheduling-capture.md`, `kv-cache-backend-internals.md`, `model-delivery-remote-internals.md`, `model-deployment-internals.md`, `model-deployment-scaling-internals.md`) at `<= H3` with explicit evidence labels, deduplicates Ascend A5 UB/Host RoCE rendering matrices across the 5 source pages, registers all 25 flat reference files in `docs/README.md` (`## Reference`) and `docs/reference/README.md`, and updates `SKILL.md`/`page-map.md`. All six formerly 900+ line public pages are substantially slimmed while keeping their required operator tasks intact.
      Verify: `make lint docs && SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site`

#### Wave 4 — Three-Page Editorial Pilot Calibration

- [x] **T10 · Three-page editorial pilot (Walkthrough + Configuration + Benchmark) and `gpt-6.2-astra` taste approval**
      Blocked by: T9
      Owns: `docs/walkthroughs/model-delivery/prefetch.md`, `docs/modules/model-deployment/configuration.md`, `docs/modules/model-deployment/prefill-decode-benchmark.md`, `docs/assets/diagrams/prefill-decode-benchmark-*.svg`, `.agents/skills/gpustack-operator-docs/references/conventions.md`
      Gate: review
      Acceptance: Rewrites one Walkthrough (`prefetch.md`: natural voice, pinned revision preserved, raw SHA-256 noise removed), one Configuration guide (`configuration.md`: 80% ASD-STE100, uniform glossary/plurality, zero YAML-prose duplication), and one Benchmark (`prefill-decode-benchmark.md`: opening trade-off preserved, labeled SVG sweep chart added, matched comparison tables retained, concise footnotes). Passes `make lint docs`, operator-task checks, and an independent Codex (`--model gpt-6.2-astra --effort high`) editorial-taste approval before unlocking Wave 5.
      Verify: `make lint docs` (plus recorded `gpt-6.2-astra` pilot approval)

#### Wave 5 — Full-Corpus Editorial Rewrite and Visual Charts (Parallel Disjoint Batches)

- [x] **T11 · Editorial rewrite: remaining 13 Walkthroughs, Getting Started, and Operations**
      Blocked by: T10
      Owns: `docs/getting-started/**`, `docs/operate/**`, `docs/walkthroughs/_index.md`, `docs/walkthroughs/devices/**`, `docs/walkthroughs/rdma/**`, `docs/walkthroughs/topology/**`, `docs/walkthroughs/kv-cache/**`, `docs/walkthroughs/model-deployment/**`
      Acceptance: Applies the pilot-approved editorial standard across all remaining 13 walkthroughs (natural human voice, immutable inputs preserved, unreferenced hashes/Pod UIDs/long raw dumps stripped), `getting-started/` (`architecture.md <= 200` lines), and `operate/` (80% ASD-STE100, unified terminology and singular/plural forms, concise headings and footnotes).
      Verify: `make lint docs`

- [x] **T12 · Editorial rewrite: Devices, RDMA, Topology, KV Cache, Model Delivery, and GPU Instances**
      Blocked by: T10
      Owns: `docs/modules/devices/**`, `docs/modules/rdma/**`, `docs/modules/topology/**`, `docs/modules/kv-cache/**`, `docs/modules/model-delivery/**`, `docs/modules/instances/**`
      Acceptance: Applies the pilot-approved 80% ASD-STE100 configuration standard across all 24 retained pages in these six modules, removes residual duplication, enforces unified terminology and singular/plural rules, and verifies self-sufficient operator tasks (pending Pod, refused KV injection, missing metrics, persisted `spec.unitResources`).
      Verify: `make lint docs`

- [x] **T13 · Editorial rewrite & SVG charts: Model Deployment guides and remaining 4 Benchmarks**
      Blocked by: T10
      Owns: `docs/modules/model-deployment/{_index.md,engine-versions.md,prefill-decode.md,encode-prefill-decode.md,encode-prefill-decode-benchmark.md,elastic-ep.md,elastic-ep-benchmark.md,external-dp.md,external-dp-benchmark.md,scaling.md,scaling-benchmark.md,runtime-control.md,routing.md}`, `docs/assets/diagrams/{encode-prefill-decode,elastic-ep,external-dp,scaling}-benchmark-*.svg`
      Acceptance: Applies the pilot-approved 80% ASD-STE100 standard across all remaining Model Deployment guides and adds labeled SVG sweep charts + concise per-scenario delta takeaways beside matched comparison tables on `encode-prefill-decode-benchmark.md`, `elastic-ep-benchmark.md`, `external-dp-benchmark.md`, and `scaling-benchmark.md`, preserving all counterexamples, failed runs, and sample scope.
      Verify: `make lint docs`

#### Wave 6 — End-to-End Verification and Final Sign-Off

- [ ] **T14 · End-to-end parity verification, operator-journey audit, and final `gpt-6.2-astra` sign-off**
      Blocked by: T2, T11, T12, T13
      Owns: `specs/2026-11-21-three-zone-docs-governance.md`
      Gate: review
      Acceptance: All lint, unit, publication fixture, and auto-sync self-tests pass; `make site-serve` HTTP response for `/reference/README.md` matches `make site` output byte-for-byte; all five operator-journey exercises pass on the rendered site and search index alone; final Codex (`--model gpt-6.2-astra --effort high`) editorial-taste review approves the full diff; spec tasks are checked off and `Status:` is updated to `Shipped`.
      Verify: `make lint docs && SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site && bash hack/check/autosync-selftest.sh . && python3 -m unittest -v hack/site/test_rewrite_reference.py hack/site/test_publish_verify.py hack/check/test_agent_index.py .agents/skills/gpustack-operator-benchmark/scripts/test_autoscaling_qualification.py`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Extend `.agents/skills/gpustack-operator-docs/scripts/check-docs-selftest.sh` with fixtures covering `H4+` rejection, fenced/indented heading exemptions, nested `docs/reference/` directory rejection, public-to-reference/contribute link rejection, three-table `docs/README.md` + `docs/reference/README.md` rules, and authored-prose singular/plural positive/negative cases.
- Update `.agents/skills/gpustack-operator-benchmark/scripts/test_autoscaling_qualification.py` (`scaling-simulation.md` path assertion) in lockstep with T8 so benchmark unit tests remain green.

#### Unit tests

- `hack/site/test_rewrite_reference.py`: `2026-11-21` - target 100% branch coverage across all 6 link rewrite classes (public regular page, `_index.md`, sibling reference, `docs/assets/` diagram, repo/`contribute/` → GitHub `sourceRef`, external/mailto passthrough), query/fragment preservation, code-fence/inline-code/HTML-comment exemptions, multiline links, footnotes, version-local vs Pages-root prefixes, and fresh-mirror deletion of removed files.
- `hack/site/test_publish_verify.py`: `2026-11-21` - target 100% branch coverage across legacy-stable default + new `main` (root `/reference/` absent/cleaned), new-stable promotion (atomic replacement + stale root alias removed), idempotent same-tag/commit retry (identical bytes pass), mutated same-tag/commit retry (rejected before any write), different-commit tag rejection, `/charts/` byte-identity, and `verify.py` negative stale-alias check.
- `hack/check/test_agent_index.py`: `2026-11-21` - target 100% branch coverage across zero excluded HTML/search/sitemap leaks, 65 docs search entries, bi-directional `/llms.txt` parity (origin, multiplicity, H1/description, `Reference`, GitHub `Optional`), `/reference/*.md` body/code integrity, and Markdown `#anchor` resolution (including duplicate slugs, multiline links, footnotes, and code-block lookalikes).
- `.agents/skills/gpustack-operator-benchmark/scripts/test_autoscaling_qualification.py`: `2026-11-21` - regression pass confirming updated `docs/reference/model-deployment-scaling-simulation.md` path.

#### Integration tests

- **Local Hugo production-prefix build**: `SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site` verifying `site-links.py` + `agent-index.py` across all 61 public pages, 4 wrappers, 25 static `/reference/*.md` exports, and `/llms.txt` under a non-root repository prefix.
- **Local preview parity check**: Start `make site-serve` against an ephemeral port in a smoke script and assert HTTP GET `/reference/README.md` and `/llms.txt` match `site/public/` bytes identically.
- **Full repository lint & auto-sync integration**: `make lint docs` (running `check-docs`, `check-specs`, `check-crossrefs`, `skills.sh`, and `make site` under macOS `onetrueawk`) plus `bash hack/check/autosync-selftest.sh .` confirming `.auto-sync/*` invariants for `docs/reference/settings.md` and `docs/modules/model-delivery/remote.md`.
- **Five public operator-journey checks**: Manual/scripted walkthrough starting from public entry points and `index.json` search alone to resolve (1) pending Pod, (2) refused KV cache injection, (3) missing `ModelDeployment`/`Instance` metrics, (4) reverted `Setting` on restart, and (5) persisted `spec.unitResources` after upgrade.

#### e2e tests

- None required for Kubernetes cluster E2E (`gpustack-operator-e2e` / `gpustack-operator-chart-e2e`): this enhancement modifies no Go operator controllers, admission webhooks, CRDs, or Helm chart templates. End-to-end site publication is verified locally via isolated `publish.py` + `verify.py` fixtures without mutating the live `github-pages` branch.

## Alternatives

1. **Split Reference into public `docs/reference/` and hidden `docs/references/`**: Rejected because two nearly identical directory names cause routing ambiguity and contradict keeping `Reference` hidden from the site and indexed via `/llms.txt`.
2. **Hide pages via YAML frontmatter (`site_hidden: true`)**: Rejected because files remain scattered across `docs/modules/**` without a flat LLM navigation hub and require manual filtering across every Hugo template.
3. **Conservative slimming (~8 pure lookup pages moved)**: Rejected because leaving 9 dense internal/telemetry guides on the public site (~65 public pages total) keeps controller internals mixed into human navigation.
4. **Allow public HTML pages to link directly to `/reference/*.md`**: Rejected because humans browsing the styled site would drop into unstyled raw Markdown and public pages would couple to hidden reference paths.
5. **Infrastructure + single-module pilot spec only**: Rejected because maintaining dual writing and layout standards across a half-migrated corpus adds temporary lint exemptions and ongoing drift.
6. **Add HTML `<meta refresh>` redirect stubs for the 22 retired HTML URLs**: Rejected because 22 permanent stub files add ongoing maintenance and exemption debt in `site-links.py`/`agent-index.py` and cannot cleanly redirect split-owner pages, while older published tags already remain immutable on `github-pages`.
7. **Move all benchmark per-concurrency tables into `docs/reference/*-benchmark-data.md`**: Rejected because separating supporting numbers from benchmark trade-off conclusions creates dual-file drift on reruns and forces human evaluators off the site to verify concurrency inflection points.

## Open Questions

None — all architectural, URL-retirement, benchmark-table, and environment decisions are resolved.
