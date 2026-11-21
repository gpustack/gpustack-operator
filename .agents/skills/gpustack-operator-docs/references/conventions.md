# Conventions — page shape and writing rules

## Documentation zones

`docs/` holds three zones. Each zone has one audience, one visibility contract and one link policy.
Choose the zone before writing; the zone decides the template, the footer and the registration.

| Zone | Source paths | Visibility | Audience |
|---|---|---|---|
| Public Human Site | `docs/getting-started/`, `docs/walkthroughs/`, curated `docs/modules/`, `docs/operate/` | Hugo HTML, sidebar, sitemap and search; listed in `/llms.txt` under its topic section | Cluster operators |
| LLM Reference | `docs/reference/*.md`, flat, zero subdirectories | No HTML, sidebar, sitemap or search entry; published as static `/reference/*.md` and listed under `## Reference` in `/llms.txt` | Coding agents and integrators |
| Repo Contributor | `docs/contribute/*.md` | No HTML, sidebar, sitemap or search entry; source-revision GitHub links under `## Optional` in `/llms.txt` | Maintainers |

Static `/reference/*.md` files stay fetchable over HTTP so agents can read them from `/llms.txt`.
"Excluded from the site" means absent from human navigation, sitemap and search, not unreachable.

### Link isolation

Links run one way: from the reference and contributor zones toward the public site, never back.

| Source zone | May link to | Never links to |
|---|---|---|
| Public site (`docs/` public pages and `site/content/**`) | Public pages, `docs/assets/`, non-`docs/` repository files (rendered as source-revision GitHub URLs) | `docs/reference/**`, `docs/contribute/**`, root-relative or same-origin `/reference/*.md` URLs |
| LLM reference | Sibling `docs/reference/` pages, public pages (exported as `.md`), `docs/assets/`, repository and `docs/contribute/` paths (rewritten to source-revision GitHub URLs) | Nothing else |
| Contributor | Any repository path | Nothing else |

A public page that needs a fact from `docs/reference/` carries the operator task itself: the symptom,
the command or configuration, the expected signal and the next action. Keep the exhaustive table in
the reference page and do not point the reader at it. Strict zone checks activate once the hub
`reference/README.md` exists under `docs/reference/`; until then, remove any public link into an
excluded zone whenever you edit the page.

### Reference pages and hub

Name reference pages `<module>-<topic>.md` directly under `docs/reference/`, such as
`devices-discovery.md` or `model-deployment-status.md`. Never create a subdirectory.
Open each reference topic page by stating its evidence type, using one label from this table:

| Label | Meaning | Rule |
|---|---|---|
| contract | Behavior enforced by the API, admission or controller code | Verify against current code before changing it |
| recorded capture | Output from a live run | Keep versions, hardware and genericized node names |
| synthetic simulation | Output from a model or simulator, not hardware | Label it as simulation wherever it is cited beside hardware runs |

The hub `reference/README.md` is the flat navigation entry for agents. It holds three parts:

- **Task routing**: one row per task, naming the reference page and the public page that owns the operator task.
- **Exact aliases**: API kinds, field paths, condition types and reasons, each mapped to its reference page.
- **Evidence labels**: the label of every reference page, using the table above.

The hub and `docs/README.md` are exempt from a mandatory `## Contents`, from listing themselves and
from `**Next**`. `docs/README.md` registers every reference page, the hub included, in its
`## Reference` table.

## The page template

Every page under `docs/` looks like this, except `docs/README.md` and the reference hub. A public
page:

```markdown
# Accelerator Requests

A workload asks for accelerators through resource keys on its containers. [Preflight
Checks](preflight.md) covers the node checks that come first.

## Contents

- [Resource keys](#resource-keys)
- [Request rules](#request-rules)

## Resource keys

...

---

**See also** — [Preflight Checks](preflight.md) (node readiness before a request) · [Walkthrough](../../walkthroughs/devices/scheduling.md) (a captured end-to-end run)

**Next** → [NVIDIA MIG](nvidia-mig.md) — partition profiles on NVIDIA hardware.
```

A reference topic page states its evidence type in the introduction and ends without `**Next**`:

```markdown
# Model Deployment Status

Evidence: contract. Every condition type and reason that a `ModelDeployment` reports, with the field
that sets it.

## Contents

- [Conditions](#conditions)

## Conditions

...

---

**See also** — [Routing](../modules/model-deployment/routing.md) (endpoint eligibility for operators) · [Shutdown](model-deployment-shutdown.md) (retirement conditions)
```

**Introduction** — start with the subject or the task in a short introduction. If the page already
opens with useful prose, use it. Do not prepend a second summary or metadata fields for purpose,
audience or reading time. Put required software, permissions and earlier steps beside the operation
that needs them, or in a prerequisites section when several steps share them. Link to background
reading where it helps the reader follow a mechanism.

**`## Contents`** — one bullet per `##` heading, in document order, no `###`. Regenerate rather than
hand-edit (this short form does not handle a heading with an inline link; `scripts/check-docs.sh` is
the authority on the anchor):

```bash
awk '
  /^```/ { fence = !fence; next }
  fence  { next }
  /^## / {
    t = substr($0, 4); if (t == "Contents") next
    a = tolower(t); gsub(/`|\*/, "", a); gsub(/[^a-z0-9 _-]/, "", a); gsub(/ /, "-", a)
    gsub(/`/, "", t); printf "- [%s](#%s)\n", t, a
  }' docs/modules/devices/requests.md
```

**Footer** — every page ends with a `---` rule, then `**See also**` (sideways links, ` · `-separated,
each with a parenthetical saying why). The rest depends on the zone:

| Zone | Footer | Next |
|---|---|---|
| Public site | `---`, `**See also**`, `**Next** →` | Required wherever a next step exists on the reader's path |
| Reference topic page | `---`, `**See also**` | Omitted |
| Reference hub | `---`, `**See also**` | Omitted; no `## Contents` |
| Contributor | `---`, `**See also**` | Optional |

Footer links obey the [link isolation](#link-isolation) table: a public footer never names a
reference or contributor page.

## Titles and file names

Name top-level groups for capabilities, such as `Heterogeneous Devices`, `RDMA Networking` and `Topology`.
Use component names such as `Device Manager` when describing those components.
Use short, Title-Cased nouns or noun phrases for menu labels at every depth, including child menus.
Capitalize the first word and main words; keep connecting words such as `and` lowercase.
Omit the parent module's name from the menu.
Check each article title against its introduction and main sections. Keep accurate existing titles.
Article H1 titles may retain module names and useful `Operations`, `Reference` or `Walkthrough` suffixes.
Change titles that are unclear, misstate the content or conflict with their menu labels.

| Context | Menu label | H1 |
|---|---|---|
| Model Deployment | `Configuration` | `Model Deployment Configuration` |
| External DP child article | `Benchmark` | `External DP Benchmark` |
| Model Delivery | `Remote Delivery` | `Remote Weight Delivery` |
| KV Cache | `Election and Failover` | `KV Cache Election and Failover` |
| KV Cache | `Pools and Bindings` | `KV Cache Pools and Bindings` |
| Walkthroughs / Model Deployment | `Elastic EP` | `Elastic EP Walkthrough` |

Menu labels and H1 titles must describe the same subject; they need not use identical words.
The breadcrumb's current-page label uses the full page title, as does the browser title.
Its parent comes from the source directory. A sidebar child group does not create a breadcrumb parent.
Check all three in the rendered site when changing titles or navigation. A short sidebar label must
still lead to the article the reader expects from its breadcrumb and H1.
Use `Overview` for module introductions. Name preparation and example-selection pages by their purpose.
Use `children` in `site/data/navigation.yaml` for subordinate articles.
Walkthrough module links use the same names and order as the outer capability groups.
Keep existing file names when changing display titles to preserve page URLs.
Reference and contributor pages have no menu label and never appear in `site/data/navigation.yaml`.

Each `docs/README.md` index label must match its page's H1 character for character; `check-docs.sh`
checks this rule for `All pages`. Keep `## Reference` and `## Contribute` labels equal to their H1s
as well. Keep `All pages` in navigation order, with child articles immediately after their parent.
Update links that name a changed title and any links to its old H1 anchor.

Headings below H1 use sentence case. Preserve official names, resource-kind casing and abbreviations.
For a numbered heading, capitalize the first word after the colon: `Step 1: Create the deployment`,
`Stage 1: Node Feature Discovery`, `Gate 3: Per-accelerator AdmissionCheck` and
`Rule 1: Family and container group`. Keep the rest in sentence case. Preserve official spellings
that begin with a lowercase letter. Apply the same capitalization to Contents labels and other
heading labels.
Titles, headings, navigation and link labels stay plain. GitHub lowercases anchors, so re-casing keeps every
inbound link; changing its *words* does not.

## Section names and steps

Use the same heading for the same section purpose across README, docs and site content.
Choose by the section's content, not its current name. Keep technical topic headings when these
categories do not fit. Do not add empty sections or force every article into the same outline.

| Section purpose | Heading | Avoid competing names |
|---|---|---|
| Brief task context, outcome or synthesis | Summary | TL;DR, Recap, Conclusion |
| Software, permissions, hardware or prior setup required to proceed | Prerequisites | Prerequisite, Requirements, Before you begin |
| Fields, options and configuration choices | Configuration | Setup options, Config |
| Ordered procedure grouped beneath one heading | Steps | Procedure, Instructions |
| Checks that confirm the expected state | Verification | Validation, Checks |
| Removing resources created by an example | Cleanup | Teardown, Release resources |
| Measurements and observed outcomes | Results | Findings, Outcomes |
| Reasons to choose a capability | Benefits | Advantages, Strengths |
| Costs and workloads where the benefit decreases | Trade-offs | Disadvantages, Good at / Not good at |
| Constraints, unsupported cases and measurement boundaries | Limitations | Limitation, Weaknesses, Weak points |
| Symptoms, diagnosis and recovery actions | Troubleshooting | Problems, Common issues |

An introduction can supply the summary without a `Summary` heading. A closing synthesis may use
`Summary` when the introduction has no such section. Do not repeat it as `Conclusion` or `TL;DR`.
Keep technical requirements in their owning topic; not every occurrence of "requirements" means
prerequisites. A verification procedure is distinct from a description of webhook validation.
For a section that combines prerequisites and constraints, use `Prerequisites and limitations`
or split it by purpose. Do not classify the whole section by whichever kind of bullet is more common.
Use `Verification` for checks of a configured capability; name the object in its first paragraph.
Benchmark summaries use `Benefits` and `Trade-offs` for observed gains and costs. Keep the
measurement boundaries in `Limitations`; do not create competing `Limitations` headings on one page.

In README and docs, exclude `what`, `where`, `when`, `how`, `which`, `why`, `who`, `whose` and
`whom` from all headings, regardless of case. Use concise topic names instead.

### Heading depth

Every page under `docs/`, in all three zones, uses at most three heading levels: `H1` → `H2` → `H3`.
`H4`, `H5` and `H6` are forbidden; headings inside fenced or indented code blocks do not count.
When content seems to need a fourth level, change its shape instead:

- **A set of peer entries**: put them in a table, or make each entry a linkable `###` under one `##`.
- **A long subsection**: promote it to `##`, or move its lookup detail to a `docs/reference/` page.
- **A thin subsection**: merge it into its parent. One short paragraph does not earn a heading.

Do not pad an outline to look complete. Update Contents, self links and every inbound anchor when a
flattened heading changes its words.

Keep every H2–H3 heading to at most four words and 32 visible characters. Exclude a numbered prefix
such as `Step 2:` or `Rule 3:` from both counts. Count hyphenated parts as separate words. This is a
hard limit, including in `--report` mode. Move conditions, quantities and explanations into the
first paragraph. Omit an initial `The` unless it belongs to an official name. Do not bold,
italicize or add code formatting to headings; exact identifiers in body text keep their format.

- Number dependent operations as `Step N: <short imperative>`, such as `Step 1: Create the deployment`.
  Restart at 1 for each independent procedure. When steps are direct `##` headings, omit a redundant
  `Steps` wrapper. A standalone `Verification` or `Cleanup` section need not carry a step number.
- Keep `Stage N: <topic>` for numbered architecture stages, `Gate N: <topic>` for admission checkpoints
  and `Rule N: <topic>` for normative rules. Use the same capitalization after the colon for these
  and other numbered headings: capitalize the first word, then use sentence case. These describe system behavior, not reader operations.
  Use a colon consistently; do not replace their technical meaning with `Step`.
  Name the topic in the heading, such as `Rule 2: Logical slice count`. State the complete rule in
  the first paragraph: "A logical slice request must set `<base>.sliced` to `1`."
  Keep quantities, scope words such as "exactly" and "at most", and conditions in that paragraph.
  Review the rendered table of contents; it should list topics without repeating full rule sentences.
- Use topic headings for independent component responsibilities, such as `PCI vendor labels`.
  Avoid arbitrary `Job N` numbering. Keep numbers when they identify an actual Kubernetes `Job`.
- Configuration scenarios and behavior descriptions use topic headings, such as `Mixed configuration`
  or `Failover behavior`. An imperative verb alone does not make an explanation an operation.
  Number only the actions the reader performs; keep observations and accounting explanations as topics.
- Update Contents, self links, prose references and every inbound anchor when heading words change.
  Preserve rule and stage relationships when shortening their headings.

For a style sweep, inventory all authored Markdown in scope, excluding fenced content from heading
classification. Read each affected section before renaming it. Record files reviewed without changes
as well as edits. Review like sections across page types, then run link and site checks. Keep examples,
captured output, measured values and comparison highlights intact. Generated documentation follows
its source template; do not hand-edit generated files to satisfy this vocabulary.

## Terminology and numbers

Apply these rules across guides, operations, benchmarks, reference pages and walkthroughs.
Read the owning guide and search related pages before choosing a term. Use one name for one concept.
Keep distinct concepts separate: a scheduling request does not declare engine parallelism, and a
component such as `Device Manager` does not name the whole device-management capability.
The [glossary and plurality table](#glossary-and-plurality-table) is the single list of standard
terms and plural forms.

Name the object or behavior directly. Avoid body-part and mechanical metaphors such as "arm",
"leg", "knob" and "wiring" when describing software. Use a more specific term when the context
requires it; do not collapse different concepts into one replacement word. Keep literal chart axes,
hardware names, identifiers and quoted diagnostics unchanged. Apply the same terms to headings,
prose, table headers and link descriptions.

Expand unfamiliar abbreviations at first use in each article's prose, even when its title uses the abbreviation.
Introduce terms before tables that depend on them. A link label or heading is not a prose definition.
Walkthroughs may use established abbreviations such as *TP* and *EP* directly when they link to the
owning guide for background. Keep operation steps focused; do not repeat definitions from that guide.
Italicize the serving terms listed below and introduced domain concepts such as *RDMA* and *NUMA*
in prose, lists and explanatory table cells. Do not italicize every technical noun.
Keep titles, headings, navigation and link labels plain. Keep product and component names, such as
Kubernetes, vLLM, kubelet, TopologyManager and Device Manager, in their official form.

Format prose by meaning, including lists and explanatory table cells:

| Meaning | Format | Example |
|---|---|---|
| Kubernetes resource kind, including upstream and operator resources | Inline code | `Pod`, `ModelDeployment`, `ClusterQueue` |
| Resource that needs emphasis | Bold inline code | **`ModelDeployment`** |
| Stage, role or submodule | Italics | *encode*, *prefill*, *decode*, *Prefill* role |
| Custom-resource field or field path | Italics; retain code for an exact identifier | *replicas*, *`roles[].kind`*, *`spec.router`* |
| Literal value, flag, environment variable, resource key or command | Inline code | `kind: Prefill`, `--tensor-parallel-size`, `GPUSTACK_LOG_LEVEL` |

A role name in prose uses italics; an exact enum assignment keeps code formatting.
Do not use bold or code alone for stage names in prose. Bold may highlight an actionable rule,
warning, comparison winner or a short lead-in. It can wrap an italic term or field within that text.
Avoid decorating every list item with bold. Keep manifests, commands and captured output unchanged.

Use these serving terms consistently:

| Concept | First prose use | Later use |
|---|---|---|
| Tensor parallelism | `*tensor parallelism (TP)*` | `*TP*` |
| Data parallelism | `*data parallelism (DP)*` | `*DP*` |
| Expert parallelism | `*expert parallelism (EP)*` | `*EP*` |
| Pipeline parallelism | `*pipeline parallelism (PP)*` | `*PP*` |
| Internal load balancing | `*internal load balancing (Internal LB)*` | `*Internal LB*` |
| External load balancing | `*external load balancing (External LB)*` | `*External LB*` |
| Hybrid load balancing | `*hybrid load balancing (Hybrid LB)*` | `*Hybrid LB*` |
| Mixture of experts | `*mixture of experts (MoE)*` | `*MoE*` |
| Prefill/decode | `*prefill/decode (PD)*` | `*PD*` |
| Encode/prefill/decode | `*encode/prefill/decode (EPD)*` | `*EPD*` |
| Time to first token | `*time to first token (TTFT)*` | `*TTFT*` |
| Time per output token | `*time per output token (TPOT)*` | `*TPOT*` |
| Inter-token latency | `*inter-token latency (ITL)*` | `*ITL*` |

Apply the same first-use rule to domain concepts such as remote direct memory access (RDMA) and
non-uniform memory access (NUMA). Do not infer an acronym's expansion from its spelling.

Use `×` for multiplication and `→` for transitions in prose. Space multiplication operands: `2 × 8 GPUs`.
Put a space between a number and its unit: `22 ms`, `1 GiB`, `128.24 tok/s`.
Keep percentages and ratio multipliers compact: `13.3%`, `2.22×`.
Keep decimal and binary units distinct, and put units in table headers when a whole column shares them.
Use the same number style for comparable quantities. Keep literal configuration values and output in code formatting.
Preserve measured precision, values, comparison highlights, sample counts and evidence limits.
Do not round results or change a captured command, manifest, log or output block to satisfy prose style.

## Glossary and plurality table

This section is the one shared list of standard terms and singular and plural forms. Search it before
choosing a word; add a row here rather than settling a term on one page.

### Glossary

| Concept | Use | Avoid |
|---|---|---|
| Device-detection component | Device Manager; `device-manager` for its command or workload | Device Manager for the whole device-management capability |
| Model-mounting component | Model Manager; `model-manager` for its command or workload | Model Manager for the whole model-delivery capability |
| Benchmark alternative | configuration | arm, leg |
| Data movement route | transfer path | wiring |
| Eligibility check | criterion | — |
| Inventory choice | source type | — |
| Configurable value | setting or parameter | knob |
| CPU and memory constraints | CPU constraint, memory constraint | axes |
| Prefill/decode serving | *PD* in prose | `P/D`, `pd_*` outside literal output and identifiers |
| Fixed parallel degree | `*TP* = 2` | `TP2`, `TP 2`, `TP=2` |
| Changing parallel width | `*DP* width 2 → 4` | A degree form for a width change |
| Collective size | `*EP* world size` | — |
| Hardware grouping | Ascend super pod, NVIDIA GPU instance, in plain prose | `Pod` or `Instance` code formatting |
| Public documentation zone | public site, public page | public reference |
| Agent documentation zone | LLM reference, reference page | site reference section |
| Maintainer documentation zone | contributor docs | — |
| Evidence type | contract, recorded capture, synthetic simulation | measured, for a simulation |

### Kind plurals

In authored prose, keep the singular kind inside backticks and put a plural `s` outside: `` `Pod`s ``.
Three forms exist:

- **Regular kinds** add `s` outside the code span.
- **Irregular kinds** take a plural noun after the kind: `` `Topology` resources ``.
- **Inherently plural kinds** never add a suffix: "a `Devices` object", "two `Devices` objects".

| Kind | Plural prose | Never write | Lint |
|---|---|---|---|
| `Pod` | `` `Pod`s `` | `` `Pods` `` | Checked |
| `Node` | `` `Node`s `` | `` `Nodes` `` | Checked |
| `Instance` | `` `Instance`s `` | `` `Instances` `` | Checked |
| `InstanceType` | `` `InstanceType`s `` | `` `InstanceTypes` `` | Review |
| `ModelDeployment` | `` `ModelDeployment`s `` | `` `ModelDeployments` `` | Checked |
| `ModelArtifact` | `` `ModelArtifact`s `` | `` `ModelArtifacts` `` | Checked |
| `ClusterQueue` | `` `ClusterQueue`s `` | `` `ClusterQueues` `` | Review |
| `LocalQueue` | `` `LocalQueue`s `` | `` `LocalQueues` `` | Review |
| `ResourceFlavor` | `` `ResourceFlavor`s `` | `` `ResourceFlavors` `` | Review |
| `Workload` | `` `Workload`s `` | `` `Workloads` `` | Checked |
| `Setting` | `` `Setting`s `` | `` `Settings` `` | Review |
| `Topology` | `` `Topology` resources `` | `` `Topology`s ``, `` `Topologies` `` | Checked for `` `Topology`s `` |
| `Devices` | `` `Devices` objects `` | `` `Devices`s `` | Checked |

`Checked` forms fail the authored-prose plural lint. `Review` forms follow the same rule and are
caught in review. Apply the regular pattern to any other kind not listed here.

### Lint exemptions

The plural rules govern authored prose only. These keep their literal spelling:

- **Fenced code blocks**: manifests, commands, logs and any other fenced content.
- **CLI flags and commands**: `--nodes`, `kubectl get pods`.
- **API paths and endpoints**: `/pods`, `/nodes`, `/api/v1/namespaces/{namespace}/pods`.
- **YAML and JSON**: keys, values and field names, such as `pods: 10` or `"nodes": []`.
- **Captured output**: verbatim tool output, quoted diagnostics and log lines.

### Menu and heading plurality

Pick grammatical number by what the label covers, then keep siblings of the same type consistent:

- **Collections** use a plural noun: `Walkthroughs`, `Operations`, `Heterogeneous Devices`,
  `GPU Instances`, `Pools and Bindings`, `Resource keys`.
- **One capability or workflow** uses a singular noun: `Topology`, `Model Delivery`,
  `Configuration`, `Prefetch`, `Benchmark`.
- **Siblings of one type** share the same number. Every serving-mode child is `Benchmark`, never
  `Benchmark` beside `Benchmarks`.

### Count nouns

Match the noun to the number, in prose and in table cells:

- **Count nouns**: `1 GPU`, `2 GPUs`, `1 replica`, `2 replicas`, `0 replicas`.
- **Verb agreement**: "1 replica is ready", "2 replicas are ready".
- **Units** stay singular: `1 GiB`, `16 GiB`, `22 ms`, `128.24 tok/s`.
- **Abbreviations** take a plain `s`: `GPUs`, `NPUs`, `NICs`. Never `GPU's` for a plural.

## Footnotes

Keep a decision-changing caveat inline, next to the value it changes. A limit, an unsupported case,
a failed run or a data-loss risk is decision-changing. Use a GitHub Flavored Markdown footnote only
for secondary precision or provenance:

- Write `[^1]` after the value and one short sentence as its definition.
- Put the definition directly below the paragraph or table that cites it.
- Number footnotes in order of first use on the page.
- Never put a footnote in a heading, a table header or a code block.

```markdown
| Item | Captured value |
|---|---|
| Runner | `gpustack/runner:cuda13.0-vllm0.30.0`[^1] |

[^1]: Internal package metadata in this build logs `vLLM 0.19.0` at startup.
```

When a page collects many footnotes, the detail belongs in a table column or a `docs/reference/` page.

## Size caps

`check-docs.sh` enforces three caps. They are shape, not budget: a page over one of them is carrying
something that belongs in a list, a table, or another page.

| Cap | Limit | Exempt |
|---|---|---|
| prose paragraph | 5 lines / 500 rendered characters | fenced blocks, table rows, list *markers*, blockquotes, the footer |
| page length | 1000 lines | `docs/walkthroughs/devices/scheduling.md`, `docs/operate/*`, `docs/modules/devices/*-mig.md` — recordings and runbooks |
| `##` sections | 10 per page, `## Contents` not counted | `docs/README.md`, `docs/reference/*` — both are lookup tables |

A paragraph nested under a list item is prose a reader still has to get through, so it is measured on
its dedented text — the marker line is skipped, its continuation is not. The one indentation that does
exempt is **four spaces or a tab**, which is Markdown's own indented code block. Continuations in this
corpus align with their marker, at two or three spaces, so the two never collide.

**The paragraph cap is the one that matters.** Length is not the defect; verbosity is. A long page that
reads in short paragraphs and clear lists is a good page, and a short one that reads as a wall of text is
not. The line cap is a backstop against a page nobody split, not a budget to spend down.

A page that passes 900 lines is a review trigger, not a new cap. Review it for lookup tables,
captures and internals that belong in `docs/reference/`, and keep its operator tasks public. Six pages
started this review: `docs/modules/model-deployment/scaling.md`, `docs/modules/kv-cache/backend.md`,
`docs/modules/model-deployment/configuration.md`, `docs/modules/model-delivery/remote.md`,
`docs/modules/devices/nvidia-mig.md` and `docs/walkthroughs/devices/scheduling.md`.

`docs/getting-started/architecture.md` is capped tighter still, at 200 lines: it is the front door, and a front door that
grows a mechanism has stopped being one.

Run `scripts/check-docs.sh --report` while writing — it demotes the caps to warnings and prints the
per-page metrics, so one page can be measured before the rest is done.

## Diátaxis, mapped onto this repository

[Diátaxis](https://diataxis.fr/) splits documentation by what the reader is doing. The repository maps
it as follows — when a page starts serving two modes at once, that is the moment to split it:

| Mode | Reader is… | Repository pages |
|---|---|---|
| Tutorial | learning by doing | `README.md` Usage examples, `docs/walkthroughs/devices/scheduling.md`, the MIG walkthrough, `docs/walkthroughs/kv-cache/shared-cache.md` |
| How-to | achieving a goal | `docs/operate/*`, `docs/modules/devices/*-mig.md`, `docs/operate/migration/*`, a domain runbook (`docs/modules/model-delivery/operations.md`); repo-only `docs/contribute/development.md` |
| Reference | looking something up | Public contracts a reader acts on (`docs/modules/devices/requests.md`); exhaustive lookups, API matrices and telemetry catalogs in the flat LLM reference (`docs/reference/settings.md`, `docs/reference/*`), off the site |
| Explanation | building understanding | `docs/getting-started/architecture.md` and the deep pages under `docs/modules/` (contract pages read as reference, mechanism pages as explanation) |

Three consequences worth stating:

- **Do not teach in a reference page**, and do not put a lookup table in an explanation page — link.
- **Keep the operator task public.** When a lookup table moves to `docs/reference/`, its public owner
  keeps the symptom, command, expected signal and next action.
- **Keep examples and recordings distinct.** Label an illustrative configuration as an example.
  Captured output must come from a live run, with node names genericized. Never invent runtime output
  or present an unexecuted configuration as measured behavior.

## Reader and implementation detail

User documentation explains how to configure, use and operate a capability. Keep public field
contracts, commands, defaults, permissions, compatibility limits and observable failure behavior.
A filename, method name or internal HTTP route is not evidence a reader needs to finish a task.
Use a source path only in contributor documentation or when the task is editing that source.
Keep public API routes when the reader calls them; explain a missing capability through its effect
on the workload rather than a missing internal endpoint.

Route each kind of detail to one owner:

| Detail | Owner |
|---|---|
| Operator task, default, permission or failure branch | Public page |
| Exhaustive mechanical contract, parameter mapping, API matrix, telemetry catalog or raw capture | `docs/reference/` page |
| Rationale, source investigation, measured trace or rejected alternative | Owning `specs/` document |
| Build, test, release or internal-architecture workflow | `docs/contribute/` page |

Before removing implementation material from a guide, compare the actual spec section and preserve
any missing fact there, including its evidence limits. Link to the spec for readers who need the
engineering detail. Do not duplicate evidence across guides or erase uncertainty while shortening prose.

A documentation review checks both sides of a move: the guide still gives the reader the correct
operation and limits, and the spec or reference page retains the moved information. Check referenced
skills as well; a skill that needs source or measurement detail should read the owning spec explicitly.
Existing docs lint checks structure and links; it does not prove that a moved contract is complete.

## Narrative voice

Apply one reader-focused voice to `README.md`, `docs/` and site content:

- Use imperative sentences for operations: "Create a `ModelArtifact`."
- State system behavior objectively: "The worker creates a `ClusterQueue`."
- Use second person when identifying the reader's responsibility prevents ambiguity: "If you use an existing NFD installation, disable the bundled NFD."
- Avoid author first person (`I`, `we`, `our`) and page self-description (`This page records`, `This walkthrough shows`, `This section covers`). Name the task, configuration or observed behavior directly.
- State measurement scope directly: "The captured run uses two ranks." Keep the hardware, versions,
  untested cases and distinction between examples and captured results.

Keep quotations, commands, logs and captured output verbatim. Links may direct readers to another
article. Contributor instructions may describe document structure when editing documentation is the task.
Replace ownership pronouns with specific component names, such as "the bundled NFD" or "GPUStack's allocator".
Do not replace first-person wording inside a literal identifier or external title.

## Scene-specific prose

The voice above holds everywhere. Each page type adds its own standard.

### Walkthroughs

Pages under `docs/walkthroughs/` read as a person guiding a run. Write each step as
action → expected observation → recovery: tell the reader what to run, what a healthy result looks
like, and what to do when it differs.

Keep:

- **Tested versions**: an environment table with the software and hardware the capture used.
- **Runnable inputs**: complete manifests and scripts that apply as written.
- **Small labeled excerpts**: a few lines of output that prove the step, labeled `Captured output`
  or `Example output (illustrative)`.
- **Immutable functional inputs**: a value a command depends on, such as a pinned model revision that
  matches an explicit file list.

Strip:

- **Unreferenced digests**: a raw SHA-256 cache digest that no later step uses.
- **Repeated Pod UIDs**: show equality in one sentence instead of printing each UID.
- **Raw dumps**: multi-hundred-line YAML. Project the fields that prove the step with `jq` or
  `-o jsonpath`. A full capture with lasting value moves to a `docs/reference/` capture page that the
  walkthrough does not link.

Before:

````markdown
We now check that the cache has been populated. Let's look at the node store:

```bash
kubectl get nodemodelstores -o yaml
```

```yaml
apiVersion: v1
items:
- apiVersion: worker.gpustack.ai/v1
  kind: NodeModelStore
  metadata:
    uid: 3f0c9a6e-...
  # ...240 more lines...
    models:
    - digest: sha256:1369aae0b5cc2d0e260557bcea04b0d6aad40f8bb6accdac60f1ee543887d9f7
      state: Ready
```
````

After:

````markdown
## Step 2: Wait for the cache

Wait until the prefetch reports the artifact on the bound node:

```bash
kubectl get modelprefetch -n gpustack-elastic
```

Captured output:

```text
NAME                    ARTIFACT                BINDING          READY   DESIRED   AVAILABLE
deepseek-v2-lite-chat   deepseek-v2-lite-chat   rtx6000-models   1       1         True
```

If `AVAILABLE` stays `False`, describe the prefetch and read its conditions before you start a
serving workload.
````

The pinned revision `85864749cd611b4353ce1decdb286193298f64c7` stays in the `ModelArtifact` manifest
because the explicit file list depends on it. The digest goes because no later step reads it.

### Configuration and capability guides

Pages under `docs/modules/`, `docs/getting-started/` and `docs/operate/` follow 80% ASD-STE100
controlled English. At least four sentences in five meet these limits:

- **Length**: at most 20 words for a procedural sentence and 25 words for a descriptive sentence.
- **One idea per sentence**, in active voice.
- **Condition before instruction**: "If the binding omits *`allowPinned`*, admission refuses the pin."
- **Plain verbs**: use, set, run, check, start, stop. Write "use" for "utilize", "to" for
  "in order to", "can" for "is able to" and "before" for "prior to".
- **Zero filler**: no "it is worth noting", "additionally" or "plays a key role".

Keep prerequisites, defaults, permissions and failure branches at the point of action, not in a
distant section. State each rule once, on its owning page, and link to it from elsewhere. Omit generic
Kubernetes basics, such as what a `Namespace` is or how `kubectl apply` works, and self-evident fields.

Show a full resource as complete two-space Pretty YAML with `apiVersion`, `kind` and `metadata`, and
inline `#` comments. Show an edit as a `diff` block with the same complete header. Add micro-bullets or
a comparison table only when they add a real constraint or compare choices; do not restate the YAML.

Before:

```markdown
It should be noted that pinning plays a crucial role in keeping weights around, and it can be
enabled by setting retention.pinned, but this will only work if the administrator has previously
configured the binding to allow it, otherwise the request will be rejected by admission.
```

After:

````markdown
Pinning keeps a digest out of cache eviction. If the `ModelStoreBinding` does not set
*`allowPinned`*, admission refuses *`retention.pinned`*. Ask the administrator to grant it first:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelStoreBinding
 metadata:
   name: rtx6000-models
   namespace: gpustack-elastic
 spec:
   storeRefs:
     - name: rtx6000-models
+  allowPinned: true
```

A pinned digest still counts toward the store's usage.
````

Plural forms follow the [kind plurals](#kind-plurals) table:

```markdown
Before: The controller recreates both `Pods` and updates the `ModelDeployments`.
After:  The controller recreates both `Pod`s and updates the `ModelDeployment`s.
```

### Benchmarks and data storytelling

Benchmark pages open with the honest trade-off: the baseline, hardware, concurrency, units,
statistic and sample scope, plus every counterexample and failed run. Never let a chart replace a
caveat.

Match the visual to the evidence:

| Evidence | Visual | Rule |
|---|---|---|
| Multi-point numeric sweep | Labeled SVG line or bar chart | Axis titles with units, a legend, the statistic, accessible `<title>` and `<desc>`, and a one-sentence delta takeaway |
| Matched per-concurrency comparison | Table directly beneath the chart | Stays on the public page with captured precision |
| Mechanism with three or more parts | SVG diagram plus hidden Mermaid comment | Never a Mermaid flowchart for a numeric sweep |
| Raw log counts or uncondensed secondary dumps | `docs/reference/` page | The public page keeps its conclusions and key tables |

Use bold keyword leads only where they discriminate choices, such as `**Choose PD**` beside
`**Choose aggregated**`.

Before:

```markdown
## Suite 1: Fixed shapes

| Concurrency | Configuration | TTFT p50 (ms) | ITL p99 (ms) | Output (tok/s) |
|---:|---|---:|---:|---:|
| 1 | Aggregated | 264.263 | 16.852 | 63.580 |
| 1 | PD | 661.241 | 17.225 | 52.958 |
| ...six more rows per shape, four shapes... |
```

After:

````markdown
On one H100 SXM5 node with six serving GPUs per configuration, *PD* cut long-prompt *ITL* p99 at
concurrency 16 and raised *TTFT*. All 16 cells completed with zero errors; aggregated serving kept
higher throughput in 15 of 16 cells.

![ITL p99 in ms by concurrency for aggregated and PD serving, 4096/128 shape](../../assets/diagrams/prefill-decode-benchmark-itl.svg)

At concurrency 16 on 4096/128, *ITL* p99 fell from 458.058 ms to 17.895 ms (25.6×). *TTFT* p50 rose
from 1,231.691 ms to 8,065.377 ms (6.5×), and output fell from 490.425 to 194.074 tok/s.

| Concurrency | Configuration | *TTFT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---:|---|---:|---:|---:|
| 16 | Aggregated | **1,231.691** | 458.058 | **490.425** |
| 16 | *PD* | 8,065.377 | **17.895** | 194.074 |
````

### Reference pages

Pages under `docs/reference/` serve agents and integrators who need exact values. Use exhaustive
tables, exact identifiers and linkable `###` entries for aliases such as condition types. State the
evidence label in the introduction. Do not teach; the public owner carries the task.

## Writing rules

Avoid counting extensible lists in prose when the adjacent list or table already states their
members. Name the subject directly. Keep quantities that affect configuration, compatibility,
capacity, timing or a safety limit.

- **State the rule first, in the first sentence of the paragraph or section.** A reader who stops
  there must still be correct.
- **Keep decision-relevant rationale short.** Put source investigations, measured traces and rejected
  alternatives in the owning spec; a brief note may explain a constraint the reader must act on:

  ```markdown
  The gate reads capacity rather than allocatable.

  > **Why** — allocatable also falls to zero when a family is merely saturated, which would delete the
  > keys while instances are live.
  ```

- **Delete some rationale rather than demoting it.** Before writing a `> **Why**` that names the error a
  step avoids, ask **who takes that step** — not which page you are on. One the operator performs
  unconditionally leaves the reader nothing to decide: preserve the technical rationale in its owning
  spec before removing the block, then
  re-read around it, since a removal orphans the pronouns and cross-references that pointed into it. One
  the reader takes keeps its error, which is what tells them what they are choosing. Either way keep the
  **boundary of a measurement** — which hardware, which driver generation, what is still unmeasured.
- **A runtime claim holds itself to measured behavior, the vendored header, or the code** — not to
  another project's account of it. Say what GPUStack observes from where it runs, as device discovery
  does for the `cntoolkit` userspace version being unreadable from where the detector runs. Two things
  are not that claim and stay: a link to an upstream table a reader wants, and `vendor-prerequisites.md`
  quoting a **vendor's own** docs for an install prerequisite with the version it was read against.
- **State a fact once.** The second place links to the first. If you are tempted to restate it "for
  convenience", the two copies will disagree within two releases. Across zones, the second place
  must also be allowed to link; a public page restates the operator task instead of linking a reference page.
- **Break a wall of text into named subsections.** If a paragraph carries more than one rule, it is
  more than one paragraph — and past 5 lines it fails the paragraph cap. A `###` heading becomes an
  anchor others can link to, and it is the deepest level allowed. Merge a subsection back into its
  parent when it holds one short paragraph.
- **Prefer a table for enumerable topics** such as modes, vendors and gates. For CR fields, use
  annotated Pretty YAML as required by [YAML examples](../SKILL.md#yaml-examples).
- **Keep table headers short.** Every column header must contain 1 to 3 words and must not use `a`,
  `an`, `the`, `this`, `that`, `these`, `those`, `what`, `where`, `when`, `how`, `which`, `why`,
  `who`, `whose`, or `whom`, regardless of case. Use precise names such as `Resource`, `Scope` or
  `Default`. Put conditions and explanations in cells or adjacent prose. Preserve technical names.
- **Name code in contributor guides.** Give a source path when the reader is editing that code.
  User guides state the behavior and link to the owning spec for its implementation.
- **No symbol-numbered cross-references** (`switch ①`, "gate-2 above"). Use the heading name and a
  link — the numbering breaks the moment a page is split.
- **Wrap at about 100 columns**, and keep tables on one line each (a wrapped table row is unreadable in
  a diff).
- **Write Pretty YAML.** Follow [YAML examples](../SKILL.md#yaml-examples), including its rules for
  empty collections, comments and preserving values when formatting.
- **Links are relative** (`../settings.md`, `architecture/admission.md`), never absolute GitHub URLs —
  the exception is the chart README, which is rendered outside the repo on Artifact Hub. Every link
  also obeys the [link isolation](#link-isolation) table.

## Short sentences and visual expression

Follow 80% ASD-STE100 controlled-English discipline across `README.md`, `docs/` and site content.
The [configuration and capability guides](#configuration-and-capability-guides) standard lists the
limits; this section adds the shape rules:

- **Short sentences**: Keep procedural sentences to at most 20 words and descriptive sentences to at
  most 25 words. Express one thought per sentence using active voice and concrete verbs.
- **No redundant code commentary**: State only facts backed by recorded runs or repository source
  (`api/`, `pkg/`). When Go struct comments or code logic already explain internal implementation
  details that do not affect user configuration or operations, omit them from `docs/`.
- **Single-dimension logic (Bullets)**: Express single-dimension lists as bullets, each explanation at
  most 20 words. Add a bold short lead-in only when it names the choice or condition a reader picks
  between: `- **<Short lead-in>**: <Explanation in <=20 words>.`
- **Two-dimension logic (Uniform tables)**: Express two-dimension comparisons or mappings as Markdown
  tables. Tables of the same category across modules must share identical column headers (for example,
  `| Value | Meaning | Constraints |`, `| Condition | Reason | Resolution |`, or
  `| Component | Role | Source | Required |`) and consistent cell phrasing. To emphasize a meaningful
  comparison row, bold its first cell and a key verdict or value in another cell. The site highlights
  that row, including its sticky first cell; bold text also conveys emphasis in Markdown exports.
- **Numeric sweeps (SVG charts)**: Plot a multi-point measurement as a labeled SVG line or bar chart
  under `docs/assets/diagrams/`, following the [benchmarks](#benchmarks-and-data-storytelling) rules.
  Do not add a hidden Mermaid comment to a chart, and never draw a sweep as a Mermaid flowchart.
- **Multi-step or multi-component logic (SVG flowcharts)**: Express three or more interacting
  components, controllers or lifecycle transitions as a clean directional SVG under
  `docs/assets/diagrams/` or `site/assets/images/`, with labeled edges and clear group boundaries.
  Place the matching Mermaid `flowchart LR` or `flowchart TD` in an HTML comment immediately
  below the image, separated by a blank line, for LLM readers and Markdown exports. In the comment,
  write `->` in place of `-->` and `..>` in place of `-.->` so HTML does not close the comment
  early; restore Mermaid arrows before rendering the graph. Keep all nodes, edges and labels
  consistent between the SVG and the comment. Use generous node padding
  and never trap text in tight pill borders. Use a bordered canvas, tinted domain containers with
  bold colored header bands, and high-contrast node cards with colored accent bars. Use cobalt
  for discovery, violet for scheduling, teal for delivery and cache, and amber for workloads.
  Support light and dark themes, reduced motion and crisp connector lines. Use architecture,
  swimlane, feedback, transfer and hierarchy layouts when they clarify verified relationships.
  Pair complex diagrams with a numbered step list or lookup table below the diagram.
- **Macro and micro CR configuration**: Pair every CR configuration block with:
  1. A **macro** complete Pretty YAML resource (`apiVersion`, `kind`, `metadata`) with inline `#`
     comments (`# required`, `# optional, default: ...`, `# enum: ...`).
  2. A **micro** breakdown immediately below, limited to constraints the comments cannot carry:
     `<=20`-word bullets for fields required together, immutability or refusals, and uniform tables
     for enum values, admission gates or webhook validations. Do not restate what the YAML shows.

## Prose tells

These are manual review heuristics; lint checks structure, links, synchronization and size limits.
A matching phrase needs context before it becomes a finding.

Docs here are reference and technical text, so the voice is neutral and plain. Text written on autopilot
carries patterns a careful writer rarely chooses on purpose. Read a draft once for them before you run
the checker; the checker does not read prose. The patterns are adapted from the
[humanizer](https://github.com/blader/humanizer) skill (MIT), strongest first.
Keep wording that states a necessary technical distinction.

| Pattern | Watch for | Write instead |
|---|---|---|
| Not X but Y | "not just X, but Y", "it is not X, it is Y", "X rather than Y", "This does not mean X. It means Y.", a clipped tail such as ", no guessing" | Say Y. Keep the contrast only when the reader really holds the belief X, or when both halves carry a fact |
| One-line closer | a sentence that restates the paragraph above it, "That is the real win.", "That distinction matters.", a row of fragments ("No cache. No retry. No queue.") | Delete it, or merge the fragments into one sentence with a concrete claim. Keep a closer only if it adds a consequence the text above does not show |
| Sayings that sound deep | "the real question is", "at its core", "fundamentally", "X is the language of Y", "becomes a trap" | The specific claim |
| Staged run-up | "Let's dive in", "here is what you need to know", "Honestly?", "the thing is", "note that" before a routine claim | Start with the point |
| Arguing with no one | "To be clear", "this is not about", "a tempting approach would be", a rejected option no page proposes | Delete it. Keep a rejection only when a reader would really weigh that option, and then state the reason once |
| Forced triads | three parallel items where the meaning has two, "fast, reliable, and scalable" | One item per real idea; keep three only when there are three |
| Dashes as connector | ` — `, ` – `, ` -- ` between clauses | A period, comma, colon or parentheses. The footer and table cells keep their template separators, and code, paths and URLs are untouched |
| Stacked qualifiers | "could potentially", "may arguably", "in some cases it might" | One hedge, and only when the code or a measurement supports the doubt |
| Inflated significance | "pivotal", "crucial", "plays a key role", "marks a shift", a closing paragraph about the future | The fact. End a page on its last concrete fact |
| Stock AI words | additionally, delve, robust (figurative), seamless, leverage, showcase, highlight (verb), landscape, tapestry, testament, underscore, valuable, key (adjective) | A plain word, or nothing. "Gate" and "robust" used in their technical sense are fine |
| Avoiding is, are, has | "serves as", "acts as", "features", "boasts", "offers" | "is", "are", "has" |
| Shallow -ing riders | ", ensuring X", ", enabling Y", ", reflecting Z" tacked onto a fact | Drop the rider, or make it its own sentence with the mechanism named |
| Bold as decoration | isolated bold ordinary words, a bold label and colon on every bullet | Use bold for an actionable rule, warning, short lead-in or comparison highlight. Keep labels plain when they add no structure |
| Decorative headings | Title Case on ordinary words below the H1, emoji, arrows, a rule between sections, a heading that restates its first sentence | Sentence case with official names and abbreviations preserved; keep the required footer separator |
| Writing about the page | "This page records", "This walkthrough shows", "the table below compares", "this section is organized by" | Name the task, technical object or observed result. Preserve measurement scope and useful navigation links |
| Chat residue | "Certainly!", "I hope this helps", "let me know", an offer to expand | Delete it |

Three limits keep the cleanup from doing harm:

- **Keep every fact.** A rewrite must not add or drop a name, number, version, key, condition or
  ranking. A sentence that needs a detail you do not have gets a simpler wording, not a guess.
- **Change prose only unless the task requests example formatting.** Code blocks, inline code,
  commands, paths, link targets and table rows compared with code (see the invariants in `SKILL.md`)
  stay as they are. When the task includes headings, use concise technical names and update Contents,
  self links and every inbound anchor. Re-casing alone preserves anchors.
- **A pattern is a default, not a crime.** A `> **Why**` note may legitimately correct a belief the
  reader holds ("allocatable also falls to zero when a family is merely saturated"), and a quotation, a
  title or a proper name keeps its wording. One weak tell alone (a single dash, a single hedge) is not
  worth an edit; several together are.

After a rewrite, search the result again for the survivors: not-X-but-Y contrasts, closers, triads,
dashes and bold labels.

## Adding a page

1. Choose the [zone](#documentation-zones) first, then the location. Check the module map before choosing.
   - **Public site**: the owning capability under `docs/modules/`, or `docs/getting-started/`,
     `docs/walkthroughs/` or `docs/operate/`.
   - **LLM reference**: a flat `docs/reference/<module>-<topic>.md`, never a subdirectory.
   - **Contributor**: `docs/contribute/`.
2. Copy the zone's template above and write a short introduction. Add prerequisites only when the
   task needs them. A reference page states its evidence label.
3. Register the page in its zone:
   - **Public site**: a row in the `docs/README.md` `## All pages` table, a step on any reading path
     it belongs to, and its order and menu label in `site/data/navigation.yaml`.
   - **LLM reference**: a row in the `docs/README.md` `## Reference` table and a routing row in the
     hub `reference/README.md`. Never add it to `site/data/navigation.yaml`.
   - **Contributor**: a row in the `docs/README.md` `## Contribute` table. Never add it to navigation.
4. Add it to the routing table in the skill's `SKILL.md` and to `references/page-map.md`, saying what it
   owns and what it must not absorb.
5. Link to it from the page a reader arrives from — a page reachable only through the index is a page
   nobody reads. A public page arrives from public pages; a reference page arrives from the hub and
   sibling reference pages, never from a public page.
6. Run `scripts/check-docs.sh`.
