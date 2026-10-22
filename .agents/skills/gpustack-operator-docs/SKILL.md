---
name: gpustack-operator-docs
description: "Write or update the GPUStack Operator documentation: route a new fact to the page that owns it, and keep each page's title, Contents, footer and the docs/README.md index in sync. Also adding a page or reviewing a docs diff."
---

# GPUStack Operator — documentation

Use [the shared module map](../../../docs/README.md#module-map) to locate the relevant guides,
owning specs, code entry points and related modules. [AGENTS.md](../../../AGENTS.md#finding-sources)
owns the retrieval and evidence rules; this skill owns its execution procedure.

The docs have capability guides, with technical pages linked from each guide. Everything in
`docs/` follows one shape and one index. Generated chart documentation is still pinned to code.
This skill routes a change to the page that owns it.

**Read the index first**: [`docs/README.md`](../../../docs/README.md) — it carries the reading paths and
the page table you must update when adding a page.

## Shared index and generated exports

`docs/README.md` owns both the complete page table and the module map. Each module entry uses
single-line `ID`, `Use for`, `Aliases`, `Guides`, `Code` and `Skills` fields; `Specs` and `Related`
link to applicable design records and neighboring module headings. Keep IDs stable when display
names change. A capability directory needs its own entry; API changes, installation and development
have shared task entries. Navigation points to evidence and does not establish implemented behavior.

Update routing in the same PR when adding or moving a page, changing a code entry point or module
boundary, replacing an owning spec, or changing a skill's document inputs. Update the owning guide
and spec for contract changes; edit routing only when the association changes. Pure wording edits
need no mechanical index update. Preserve existing spec status and evidence limits.

Hugo generates `/llms.txt` from the `All pages` table and Markdown exports from page sources. Do not
maintain a separate copy or summary. `make site` and `make lint docs` verify module coverage, skill
consumers, local source paths, the generated index, exported prose and code blocks, and site links.
These checks establish structure and synchronization; review must establish semantic completeness.
The local module map stays out of the site's article list and navigation.

## The rule that keeps the overview small

`docs/getting-started/architecture.md` is the **front door**: what the operator builds, the scheduling chain, one worked
request trace, the vocabulary. It is capped at ~200 lines and must stay readable in under 10 minutes.

Cover each major capability and its role in the workload flow, including model delivery. Keep the
diagram, workflow and vocabulary aligned. Put mechanisms, rationale and detailed configuration on
the owning deep page, then link to it. Add a deep page when no existing page owns the detail.

## Where a fact belongs

| Topic | Page |
|---|---|
| NFD labels, the `gpustack-cpu-info` rule, the manufacturer map | `docs/modules/devices/discovery.md` |
| Device Manager detection, the `Devices` ledger, allocator injection, cross-mode exclusion, placement | `docs/modules/devices/discovery.md` |
| The NIC/RDMA interface inventory, `pciRootId`/`pciSwitches`, the three link states, the `rdma.*` node labels, the RDMA resource keys | `docs/modules/rdma/network-topology.md` |
| Capacity labels, flavor/queue/InstanceType naming and grouping, the scheduling controllers | `docs/modules/devices/scheduling.md` |
| Topograph's boundary, `TopologySource`, topology profiles, Kueue Topologies and TAS capacity semantics, and the placement preference toward nodes holding a model | `docs/modules/topology/scheduling.md` |
| Any admission gate, the four-view status, InstanceType/Instance/Pod webhook rules, drain-stop | `docs/modules/devices/admission.md` |
| Chart mode vs image mode, `disableApplications`, what the worker applies itself | `docs/operate/installation-modes.md` |
| Startup ordering, the gateway mirror, the device-plugin registration loop, per-manufacturer packages, CGO bindings, the 63-char rule | `docs/contribute/internals.md` |
| A `KVCacheBackend`'s workloads, its admin surface, its phase or capacity | `docs/modules/kv-cache/backend.md` |
| The leader process itself: its Deployment, Service, probes, health document and Lease election | `docs/modules/kv-cache/leader.md` |
| The local disk tier itself: what it renders, its bucket, its eviction, its host directory | `docs/modules/kv-cache/local-disk-tier.md` |
| Configuring a node that is mostly disk: why a group is never disk alone, the thin-segment shape, the memory floor | `docs/modules/kv-cache/disk-heavy-nodes.md` |
| A `KVCachePool` or `KVCachePoolBinding`: the grant, the reuse domain, a quota ceiling or grant, what a full quota does | `docs/modules/kv-cache/pool.md` |
| Standing a cache up end to end, or which object comes first: the pasteable four-object sequence | `docs/walkthroughs/kv-cache/shared-cache.md` |
| How a **Pod** consumes a pool: the inject label and annotations, the injected keys per engine, a refusal, the isolation record | `docs/modules/kv-cache/injection.md` |
| A `ModelArtifact`: its sources, resolution and revalidation, the manifest digest, how a `ModelDeployment` or an `Instance` mounts or downloads it, claim placement, the weight identity in KV keys | `docs/modules/model-delivery/artifact.md` |
| The `image` source of a `ModelArtifact`: the digest contract, building weights into an image, image-volume delivery, the version floors, double storage, kubelet image GC, registry mirrors | `docs/modules/model-delivery/image-source.md` |
| A `ModelStore`, `ModelStoreBinding` or `ModelPrefetch`: the grant, the budget, pinning, TTL expiry, the warm-up pod and why it is label-free | `docs/modules/model-delivery/prefetch.md` |
| The public APIs for `ModelArtifact` and `NodeModelStore`, the `progress` subresource and who may read it, the GPUStack server capability map | `docs/modules/model-delivery/views.md` |
| A `NodeModelStore` or the `model-manager` plugin: a field and its writer, the status guard, mount authorization, materialization, a failure reason, collection, a metric | `docs/modules/model-delivery/node-store.md` |
| Running node delivery: the chart values, where the node's configuration comes from, reading a node, the watermark cap, switching delivery, where replicas land and turning the preference off, upgrading, removing the cache | `docs/modules/model-delivery/operations.md` |
| The `ModelDeployment` contract: the inherited reuse domain, the three override tiers, the owned-key table, the runner-image formula, prefill/decode pairing, the topology-placement field contract | `docs/modules/model-deployment/deployment.md` |
| The measured replica-scaling comparison across algorithms and engines, its latency and throughput, scale-out timing, retirement observations and failure limits | `docs/modules/model-deployment/scaling-benchmark.md` |
| Configuring and observing `ModelDeploymentScaler`, Shadow and Active modes, algorithm selection, EPD ratios and retirement | `docs/walkthroughs/model-deployment/scaling.md` |
| How an `elasticEp` profile resizes a serving instance's expert-parallel collective in place without replacing surviving pods | `docs/modules/model-deployment/elastic-ep.md` |
| The measured comparison of an expert-parallel collective before, during, and after a live `elasticEp.width` resize | `docs/modules/model-deployment/elastic-ep-benchmark.md` |
| A recorded Elastic EP expansion, its captured CR output and inference checks | `docs/walkthroughs/model-deployment/elastic-ep.md` |
| A recorded model prefetch sequence and captured artifact, prefetch and node-store output | `docs/walkthroughs/model-delivery/prefetch.md` |
| How External DP runs one fixed data-parallel collective with an HTTP endpoint on every rank behind `vllm-router` or `sglang-gateway` | `docs/modules/model-deployment/external-dp.md` |
| The measured comparison of External DP per-rank serving against a monolithic wide tensor-parallel group | `docs/modules/model-deployment/external-dp-benchmark.md` |
| A fixed External DP group, its minimum arguments, per-rank requests, or measured Router coverage | `docs/walkthroughs/model-deployment/external-dp.md` |
| A `ModelDeployment` metrics snapshot, which series each field reads per engine, role and router, cache-hit scope or Pod scrape annotation | `docs/modules/model-deployment/metrics.md` |
| What a `ModelDeployment` status condition or published field means, and how to read them when a deployment misbehaves | `docs/modules/model-deployment/status.md` |
| How a prefill role and a decode role are paired: the connector each engine and router renders, `spec.router` and its fields, `spec.kvTransfer`, roles on different hardware, a role's own Service | `docs/modules/model-deployment/prefill-decode.md` |
| The measured comparison of disaggregated prefill/decode serving against an equal-GPU aggregated group across uniform and mixed prompt lengths | `docs/modules/model-deployment/prefill-decode-benchmark.md` |
| How an `Encode` role pairs with `Prefill` and `Decode`: the admitted EPD matrix, encoder-cache connectors (`ECCPUConnector`, `ECExampleConnector`), shared RWX PVC rules, soft co-location affinity, and bounded encoder drain | `docs/modules/model-deployment/encode-prefill-decode.md` |
| The measured comparison of aggregated, prefill/decode and EPD serving: latency and throughput at each concurrency, the hardware and workload behind it, and the conditions under which EPD does not help | `docs/modules/model-deployment/encode-prefill-decode-benchmark.md` |
| A recorded three-role EPD deployment (`Encode` + `Prefill` + `Decode` + `llm-d-router`), its captured CR/Pod output and multimodal inference check | `docs/walkthroughs/model-deployment/encode-prefill-decode.md` |
| Which replica a managed router picks, its default routing policy, switching it through `spec.router.extraArgs`, the router's own per-replica series | `docs/modules/model-deployment/routing.md` |
| What a `ModelDeployment` replica does between its Pod's delete and its engine's exit: the drain hook, its timings, what it does not cover | `docs/modules/model-deployment/shutdown.md` |
| The lowest engine release a deployment configuration runs on, the Mooncake client its runner image carries, the store line it needs, which transport each engine can use on each path | `docs/modules/model-deployment/engine-versions.md` |
| A resource key, a request rule, a request example | `docs/modules/devices/requests.md` |
| How many RDMA endpoints a workload asks for, setting or reading the kubelet TopologyManager policy, what to do about RDMA keys no queue meters, the engine image an EFA path needs | `docs/modules/rdma/operations.md` |
| Enabling Topograph, publishing topology snapshots, webhook trust, requesting a level, TAS diagnosis or EKS validation | `docs/modules/topology/operations.md` |
| A `Setting` or a `GPUSTACK_*` variable | `docs/reference/settings.md` |
| A make target, a subchart patch, code generation, a vendored dependency | `docs/contribute/development.md` |
| An administrator procedure (MIG mode, replicas) | `docs/modules/devices/*-mig.md`, `docs/operate/*.md` |
| An upgrade path between versions | `docs/operate/migration/*.md` |
| Recovery from a wedged upgrade or a stuck namespace deletion | `docs/operate/migration/troubleshooting.md` |
| A per-product preset value | `docs/reference/instance-type-unit-resources.md` |
| Instance resource-use metrics | `docs/reference/instance-metrics.md` |
| A subcommand, one of its flags, its exit codes, its invocation | `docs/reference/commands.md` |
| A user-visible capability, a vendor's slicing support, the install flow | `README.md` |
| Vendor prerequisites, vendor GPU Operator coexistence | `docs/getting-started/vendor-prerequisites.md` |
| A recorded run with real output | `docs/walkthroughs/devices/scheduling.md`, or the walkthrough section of `docs/modules/devices/nvidia-mig.md` |

A decision *record* — why an approach was chosen over another — belongs in `specs/`, not in `docs/`.
The docs state the rule that resulted; a `> **Why**` note carries only as much rationale as a reader
needs to not undo it.

Full routing, including what does **not** belong on a page, is in
[references/page-map.md](references/page-map.md).

## Public API versions

User-facing manifests and API calls use `worker.gpustack.ai/v1`. The `v1alpha1` types are internal
controller storage; do not ask users to switch between versions or describe public resources as
"v1 views". Document internal types only when explaining implementation under `docs/contribute/`.
An inventory snapshot's format version is separate from a Kubernetes resource API version.

## YAML examples

REQUIRED: write Pretty YAML in `README.md`, `docs/` and site content. Expand nonempty mappings and sequences
into block style with two-space indentation, one field or list item per line. Do not embed
JSON-style objects or arrays in YAML examples, including commented alternatives. Empty collections
use `{}` or `[]`; removing their delimiters would change them to null.

When formatting is requested, preserve field order, values, scalar types, quoting and intentional
invalid examples. Preserve the facts in comments. Correct stale comments only after checking the
current API, defaults or rendering code. A string whose consumer requires JSON and a captured JSON
response keep that format.

Attach each comment to the field it explains. Put a long comment immediately above that field at
the same indentation. Keep inline comments at least two spaces after the value. Align adjacent
short comments when this needs little padding; do not align an entire block to its longest value.
Write short, factual comments for defaults, constraints and non-obvious behavior. Keep commented
alternatives indented like the active fields. Avoid arrows, all-capital emphasis and vague references
such as "the same". Review every changed comment against its field and source.

For comment or whitespace edits, compare parsed YAML before and after to verify that values and
scalar types are unchanged. Check commented alternatives separately; parsers ignore them.

## Reader documentation and technical ownership

Apply the reader boundary in `references/conventions.md` before adding implementation detail.
User guides retain public configuration, operational behavior and constraints; the owning `specs/`
document retains source analysis, internal routes and measured implementation evidence. Compare the
actual spec contents before moving a fact, supplement missing material, and update skills that need
that technical input. A guide must not become a source-file walkthrough.

## Site rendering

Keep diagrams in fenced `mermaid` blocks so GitHub and the Hugo site render the same source.
The site shows the diagram with an expandable source block. `docs/README.md` remains the agent
index and is excluded from the site; link site readers to the module landing pages.
`make lint docs` checks source links, page structure and the rendered site's internal links.
Published Markdown allows raw HTML only for attribute-free `<br>` or `<br/>` line breaks. Keep
HTML examples inside fenced or inline code. The existing site gate checks this allowlist;
link templates retain automatic URL escaping.
When adding a page, add its order and short menu label to `site/data/navigation.yaml`. Put setup
and configuration before operations and diagnosis; keep related vendor procedures together.
Name top-level groups for capabilities, such as `Heterogeneous Devices`, `RDMA Networking` and `Topology`.
Use component names such as `Device Manager` when describing those components.
Use short, Title-Cased nouns or noun phrases for menu labels at every depth, including child menus.
Name the article's purpose within
its hierarchy and omit the parent module's name. Use `children` for subordinate pages, such as
`Benchmark` under a serving mode or `API` under `Model Artifact`.
Walkthrough module links use the same names and order as the outer capability groups.

Choose titles from the page's task or subject. Check them against its introduction and main sections.
For example, use `Election and Failover` for the menu of a page about those tasks.
Article H1 titles may retain module names and useful `Operations`, `Reference` or `Walkthrough` suffixes.
Keep accurate existing titles. Change a title only when it is unclear, misstates the content or conflicts with its menu label.
The menu label and H1 must describe the same subject; they need not use identical words.
Breadcrumbs use the page title and its source parent. Sidebar child groups do not change that parent.
Review the rendered sidebar, breadcrumb and H1 together; each must identify the same article and capability.
Use `Overview` for a module introduction. Give a page about preparation or example selection a label that names that purpose.
The `All pages` labels in `docs/README.md` must match each page's H1 character for character.
Keep that table in navigation order, with child articles immediately after their parent.

## Page shape

Every page in `docs/` (the index excepted) opens with a short introduction, has a `## Contents` list
mirroring its `##` headings and a `**See also**` / `**Next**` footer. Templates and the writing rules
are in
[references/conventions.md](references/conventions.md).
Apply its [section names and steps](references/conventions.md#section-names-and-steps) rules:
use one name for each section purpose, concise plain headings and `Step N: <short imperative>`
for ordered operations. Keep every H2–H6 heading within four words and 32 visible characters,
excluding a numbered prefix such as `Step 2:`. In numbered headings, capitalize the first word after
the colon and use sentence case for the rest. Preserve the meaning of architecture stages, admission
gates and request rules.
Apply its [terminology and numbers](references/conventions.md#terminology-and-numbers) rules to
guides, operations, benchmarks and reference pages as well as walkthroughs. Expand unfamiliar
abbreviations, italicize the defined concept terms, roles and fields, and use consistent degree and unit notation.
Use inline code for Kubernetes resource kinds; add bold when a resource needs emphasis.
Keep identifiers and captured evidence intact. Search related pages for competing names before editing.
Apply the [narrative voice](references/conventions.md#narrative-voice) to README, docs and site content:
use imperative instructions and objective descriptions. Name technical subjects directly; avoid author
first person and page self-description.

## Sync invariants

These pages are not free text. Changing one side without the other breaks a build or a reader's trust.

| Doc | Pinned to | How it breaks |
|---|---|---|
| `docs/reference/instance-type-unit-resources.md` | `pkg/nodefeature/unit_resources_preset.yaml` | compare the product rows with the preset data when either changes; no Go documentation test pins this table |
| `deploy/gpustack-operator/chart/README.md`, `values.schema.json` | `values.yaml` + `README.md.gotmpl` via `make generate chart` | generated — never hand-edit; a doc path quoted in a `values.yaml` comment needs a regenerate, and `chart.yml` fails on drift |
| `README.md` accelerator matrix | `pkg/nodefeature/knowns.go` (resource names, `SharedResourceMaxSize`, `_ManufacturerPartitionKindMap`) and **whether** each `pkg/devicemanager/detector/<mfr>/device.go` sets `LogicalSliced` at all | nothing fails; the table silently lies about what a vendor can do. The matrix is deliberately Yes/— only — per-card slice counts and the per-vendor isolation mechanism live in `docs/modules/devices/discovery.md`, not on the front page |
| `README.md` Usage accelerator examples | `docs/modules/devices/requests.md` — *The resource keys* and *Worked example per family* | nothing fails; the front page and the normative contract drift apart. This copy is the one sanctioned exception to "state a fact once" (the README is the shop window) — change both together |
| `docs/modules/model-deployment/deployment.md` owned-key table | `modelDeploymentOwnedKeys` | compare the owned keys with each engine's table row when either changes; the Go documentation test was removed |
| `docs/reference/settings.md` tables | `pkg/worker/settings` and the `GPUSTACK_*` readers | nothing fails; an operator configures something that no longer exists |
| `docs/README.md` page table | the set of files under `docs/` | `check-docs.sh` fails |
| `docs/README.md` page **labels** | each page's `#` H1, character for character | `check-docs.sh` fails when an index label differs from its page title |

## Before you finish

```bash
make lint docs                                                             # the gate, as CI runs it
bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh --report   # while writing
```

It verifies relative links and `#anchor`s across `README.md`, `AGENTS.md`, `docs/**` and
`.agents/skills/**`; and — for `docs/**` only — each page's `## Contents` against its headings, a
`**See also**` footer at the end, registration in the `## All pages` table of `docs/README.md`, the
label there against the page's H1, and the size caps for paragraphs, page length, section count and
H2–H6 heading length (see `references/conventions.md`). The heading word and character limits and
numbered-heading capitalization apply to all authored Markdown under `docs/` and `site/content/`,
plus `README.md`, `ADOPTERS.md` and the two csrc READMEs. They remain fatal under `--report`.
`--report` demotes the paragraph, page-length, section-count and index-label checks to warnings and
prints the per-page metrics. The gate also resolves literal `docs/*.md` paths named across skills,
so a moved page cannot leave a skill reading a missing file.

`make lint docs` checks source links and page shape, including Copilot instruction links. It also
builds the Hugo site and checks HTML links, module routing, `llms.txt` and Markdown exports.
`.github/workflows/docs.yml` runs the same gate. The target also checks spec status and task
completion. Report the failing subcheck when unrelated spec work prevents a clean result; do not
mark unfinished tasks complete to make the gate pass.

It still does **not** read prose: everything below is on you.

- [ ] `wc -l docs/getting-started/architecture.md` is still ≤ ~200.
- [ ] The new fact is stated **once**; every other page links to it.
- [ ] Terminology matches related guides, operations, benchmarks, reference pages and walkthroughs.
      Name configurations, transfer paths, criteria and settings directly; avoid unexplained metaphors.
      Expand unfamiliar abbreviations; walkthroughs may use established terms from a linked owning guide.
      The concept terms listed in the conventions use italics in prose; component names remain plain.
      Resource kinds use code, emphasized resources use bold code, and roles and fields use italics.
      Check degree notation, units and measured precision against `references/conventions.md`.
- [ ] Changed a title or navigation label? Compare the page's subject, sidebar, breadcrumb and H1 in
      the rendered site. Check capitalization at every menu depth and the `All pages` index alignment.
- [ ] Instructions use imperatives, behavior uses objective statements, and authors and pages are not
      the subject. Preserve captured evidence and explicit measurement limits.
- [ ] Same-purpose sections use the shared names, including `Prerequisites` and `Limitations`.
      Headings are short, plain and free of unnecessary `The`; numbered headings use a colon.
      Step headings state reader actions. Update Contents and every inbound anchor after renaming.
      For a full sweep, account for every authored page, including pages that need no edit.
- [ ] YAML comments describe the correct fields and current contracts. Check indentation, local
      alignment and commented alternatives; compare parsed values when formatting examples.
- [ ] Read the diff once for the prose tells in `references/conventions.md` (*Prose tells*): not-X-but-Y
      contrasts, one-line closers, forced triads, dashes as connectors, stock AI words, bold labels.
- [ ] Touched `docs/reference/instance-type-unit-resources.md`? Compare its rows with
      `pkg/nodefeature/unit_resources_preset.yaml`.
- [ ] Touched `values.yaml`? Run `make generate chart` and commit the regenerated chart README/schema.
- [ ] Moved or renamed a page? Search tracked files for the old path and update each active reference.
      Historical specs may keep their original wording, but a path that points to a moved page must
      resolve to its new location.

## Keeping this skill honest

Found a doc that a test, a generator or a CI job pins, and it is not in the invariants table? Add the
row. Split or added a page? Add it to the routing table above and to `docs/README.md`. The tables are
the memory — they are only as good as their coverage.

## Publication and languages

Site publication is owned by `.github/workflows/site.yml` and `hack/site/publish.py`; chart and
Pages jobs share the `github-pages` queue. Preserve `/charts/index.yaml` and chart packages when
changing publication. Versioned builds use their exact source revision; do not publish current
prose as documentation for an older tag. Run the existing site link and export gates with a
production `SITE_BASE_URL` when changing templates or publication paths.

English is the active language. Translation files share their original stem with a language
suffix; Chinese remains disabled until its content, interface and checks are ready. Keep language
switches limited to real translations and search indexes scoped to the selected language.
