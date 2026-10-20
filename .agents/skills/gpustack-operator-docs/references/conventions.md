# Conventions — page shape and writing rules

## The page template

Every page under `docs/` (the index excepted) looks like this:

```markdown
# Scheduling Chain

The worker turns Node and `Devices` signals into capacity labels, Kueue queues and `InstanceType`
resources. [Architecture](../../getting-started/architecture.md) introduces the earlier discovery stages.

## Contents

- [Stage 3: capacity profiling](#stage-3-capacity-profiling)
- [Stage 4: Kueue chain](#stage-4-kueue-chain)

## Stage 3: capacity profiling

...

---

**See also** — [Device Discovery](discovery.md) · [Walkthrough](../../walkthroughs/devices/scheduling.md)

**Next** → [Admission](admission.md) — the admission gates a request passes.
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
  }' docs/modules/devices/scheduling.md
```

**Footer** — a `---` rule, then `**See also**` (sideways links, ` · `-separated, each with a
parenthetical saying why) and `**Next** →` (the next page on this reader's path). `See also` is
required; `Next` is expected wherever a next step exists.

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
| Model Artifact child article | `API` | `Model Artifact API` |
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

Each `docs/README.md` `All pages` label must match its page's H1 character for character;
`check-docs.sh` checks this rule. Keep the table in navigation order, with child articles immediately
after their parent. Update links that name a changed title and any links to its old H1 anchor.

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

Keep every H2–H6 heading to at most four words and 32 visible characters. Exclude a numbered prefix
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

Name the object or behavior directly. Avoid body-part and mechanical metaphors such as "arm",
"leg", "knob" and "wiring" when describing software. Use `configuration` for benchmark alternatives,
`transfer path` for data movement, `criterion` for eligibility checks, `source type` for inventory
choices and `setting` or `parameter` for configurable values. Name CPU and memory constraints
instead of calling them "axes". Use a more specific term when the context requires it; do not
collapse different concepts into one replacement word. Keep literal chart axes, hardware names,
identifiers and quoted diagnostics unchanged. Apply the same terms to headings, prose, table
headers and link descriptions.

Expand unfamiliar abbreviations at first use in each article's prose, even when its title uses the abbreviation.
Introduce terms before tables that depend on them. A link label or heading is not a prose definition.
Walkthroughs may use established abbreviations such as *TP* and *EP* directly when they link to the
owning guide for background. Keep operation steps focused; do not repeat definitions from that guide.
Italicize the serving terms listed below and introduced domain concepts such as *RDMA* and *NUMA*
in prose, lists and explanatory table cells. Do not italicize every technical noun.
Keep titles, headings, navigation and link labels plain. Keep product and component names, such as
Kubernetes, vLLM, kubelet, TopologyManager and Device Manager, in their official form.
Use Device Manager for the component and `device-manager` for its command or workload name.
Use Model Manager for the component and `model-manager` for its command or workload name.

Format prose by meaning, including lists and explanatory table cells:

| Meaning | Format | Example |
|---|---|---|
| Kubernetes resource kind, including upstream and operator resources | Inline code | `Pod`, `ModelDeployment`, `ClusterQueue` |
| Resource that needs emphasis | Bold inline code | **`ModelDeployment`** |
| Stage, role or submodule | Italics | *encode*, *prefill*, *decode*, *Prefill* role |
| Custom-resource field or field path | Italics; retain code for an exact identifier | *replicas*, *`roles[].kind`*, *`spec.router`* |
| Literal value, flag, environment variable, resource key or command | Inline code | `kind: Prefill`, `--tensor-parallel-size`, `GPUSTACK_LOG_LEVEL` |

Use the singular kind inside code when adding a plural suffix: `Pod`s.
For irregular plurals, write `Topology` resources. Classify names by context: an Ascend super pod
and an NVIDIA GPU instance are hardware concepts, not Kubernetes resources.
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

Use `PD` for the serving configuration in prose; keep `P/D`, `pd_*` or other spellings in literal engine output and identifiers.
Use `*TP* = 2` for a degree, `*DP* width 2 → 4` for a width change and `*EP* world size` for the collective size.
Avoid competing forms such as `TP2`, `TP 2` and `TP=2`. Distinguish a fixed degree from a changing width.
Apply the same first-use rule to domain concepts such as remote direct memory access (RDMA) and
non-uniform memory access (NUMA). Do not infer an acronym's expansion from its spelling.

Use `×` for multiplication and `→` for transitions in prose. Space multiplication operands: `2 × 8 GPUs`.
Put a space between a number and its unit: `22 ms`, `1 GiB`, `128.24 tok/s`.
Keep percentages and ratio multipliers compact: `13.3%`, `2.22×`.
Keep decimal and binary units distinct, and put units in table headers when a whole column shares them.
Use the same number style for comparable quantities. Keep literal configuration values and output in code formatting.
Preserve measured precision, values, comparison highlights, sample counts and evidence limits.
Do not round results or change a captured command, manifest, log or output block to satisfy prose style.

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
| How-to | achieving a goal | `docs/operate/*`, `docs/modules/devices/*-mig.md`, `docs/operate/migration/*`, `docs/contribute/development.md`, a domain runbook (`docs/modules/model-delivery/operations.md`) |
| Reference | looking something up | `docs/modules/devices/requests.md`, `docs/reference/settings.md`, `docs/reference/*` |
| Explanation | building understanding | `docs/getting-started/architecture.md` and the deep pages under `docs/modules/` (contract pages read as reference, mechanism pages as explanation) |

Two consequences worth stating:

- **Do not teach in a reference page**, and do not put a lookup table in an explanation page — link.
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

Implementation rationale, source investigations, measured traces and rejected alternatives belong
in the existing owning `specs/` document. Before removing such material from a guide, compare the
actual spec section and preserve any missing fact there, including its evidence limits. Link to the
spec for readers who need the engineering detail. Do not duplicate evidence across guides or erase
uncertainty while shortening prose.

A documentation review checks both sides of a move: the guide still gives the reader the correct
operation and limits, and the spec retains the implementation information. Check referenced skills
as well; a skill that needs source or measurement detail should read the owning spec explicitly.
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
  convenience", the two copies will disagree within two releases.
- **Break a wall of text into named subsections.** If a paragraph carries more than one rule, it is
  more than one paragraph — and past 5 lines it fails the paragraph cap. A `###` heading is cheap and
  becomes an anchor others can link to.
- **Prefer a table for anything enumerable** — modes, keys, vendors, gates, settings.
- **Keep table headers short.** Use a precise noun or short phrase, such as `Resource`, `Scope` or
  `Default`. Put conditions and explanations in the cells or adjacent prose. Preserve technical names.
- **Name code in contributor guides.** Give a source path when the reader is editing that code.
  User guides state the behavior and link to the owning spec for its implementation.
- **No symbol-numbered cross-references** (`switch ①`, "gate-2 above"). Use the heading name and a
  link — the numbering breaks the moment a page is split.
- **Wrap at about 100 columns**, and keep tables on one line each (a wrapped table row is unreadable in
  a diff).
- **Write Pretty YAML.** Follow [YAML examples](../SKILL.md#yaml-examples), including its rules for
  empty collections, comments and preserving values when formatting.
- **Links are relative** (`../settings.md`, `architecture/admission.md`), never absolute GitHub URLs —
  the exception is the chart README, which is rendered outside the repo on Artifact Hub.

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

1. Choose the owning capability under `docs/modules/`, or the task directory under `docs/`:
   `getting-started/`, `walkthroughs/`, `operate/`, `reference/` or `contribute/`.
   Check the module map before choosing a location.
2. Copy the template above and write a short introduction. Add prerequisites only when the task needs them.
3. Add a row to the `docs/README.md` page table, and a step to any reading path it belongs on.
4. Add it to the routing table in the skill's `SKILL.md` and to `references/page-map.md`, saying what it
   owns and what it must not absorb.
5. Link to it from the page a reader arrives from — a page reachable only through the index is a page
   nobody reads.
6. Run `scripts/check-docs.sh`.
