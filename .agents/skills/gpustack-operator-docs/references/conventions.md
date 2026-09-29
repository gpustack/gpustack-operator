# Conventions — page shape and writing rules

## The page template

Every page under `docs/` (the index excepted) looks like this:

```markdown
# Scheduling Chain

> **Purpose** — how the capacity labels become Kueue queues and a materialized InstanceType.
> **Audience** contributors · **Prerequisites** [Architecture](../architecture.md) · **Read time** ~15 min

One or two sentences of orientation, if the purpose line is not enough. Optional.

## Contents

- [Stage 3: capacity profiling](#stage-3-capacity-profiling)
- [Stage 4: the Kueue chain](#stage-4-the-kueue-chain)

## Stage 3: capacity profiling

...

---

**See also** — [Device Discovery](device-discovery.md) · [Walkthrough](../walkthrough.md)

**Next** → [Admission](admission.md) — the five gates a request passes.
```

**Header block** — a single blockquote, immediately under the H1, within the first six lines
(`check-docs.sh` looks there):

- `**Purpose**` — one sentence, what the page answers. Not a summary of the project.
- `**Audience**` — `everyone` / `users` / `operators` / `contributors`, or a combination.
- `**Prerequisites**` — the page to read first, as a link, or `none`.
- `**Read time**` — an honest estimate, rounded to the minute; `~18 min` beats a flattering `~5 min`.
  Use `reference — look up your product` for lookup tables.

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
  }' docs/architecture/scheduling-chain.md
```

**Footer** — a `---` rule, then `**See also**` (sideways links, ` · `-separated, each with a
parenthetical saying why) and `**Next** →` (the next page on this reader's path). `See also` is
required; `Next` is expected wherever a next step exists.

## Titles and file names

The file name, the `#` H1 and the `docs/README.md` index label say the same words — `check-docs.sh`
compares the last two character for character. The H1 is a Title-Case noun phrase of at most six words,
shaped by the directory it lives in:

| Directory | H1 form | Example |
|---|---|---|
| root, `architecture/` | `<Subject>` | `Installation Modes` |
| a domain directory — `kv-cache/`, `model-store/`, `model-deployment/` | `<Domain> <Topic>` where the domain reads naturally; a subject that names itself (`Engine Versions`, `Node-to-Node Sync`) stands without it; the domain's runbook keeps the `Operations` suffix | `KV Cache Backend`, `Model Artifact`, `Engine Versions`, `Node-to-Node Sync`, `Model Store Operations` |
| `operation/` | `<Subject> Operations` | `High Availability Operations` |
| `migration/` | `Migrating <from\|to> <what>`; a recovery page is `<Subject> Troubleshooting` | `Migrating from v0.5.x`, `Migration Troubleshooting` |
| `reference/` | `<Subject> Reference` | `Instance Metrics Reference` |

A domain directory collects every page orbiting one CR family — the contracts, the field references,
the views, the runbook — under short file names (`backend.md`, `artifact.md`, `deployment.md`) whose
H1s name the domain where it reads naturally. It exists so `reference/` stays true lookup tables
rather than becoming the dumping ground for whichever domain landed last.

`##` and `###` headings are sentence case. GitHub lowercases anchors, so re-casing a heading keeps every
inbound link; changing its *words* does not.

## Size caps

`check-docs.sh` enforces three caps. They are shape, not budget: a page over one of them is carrying
something that belongs in a list, a table, or another page.

| Cap | Limit | Exempt |
|---|---|---|
| prose paragraph | 5 lines / 500 rendered characters | fenced blocks, table rows, list *markers*, `> **Why**` notes, the footer |
| page length | 1000 lines | `docs/walkthrough.md`, `docs/operation/*` — recordings and runbooks |
| `##` sections | 10 per page, `## Contents` not counted | `docs/README.md`, `docs/reference/*` — both are lookup tables |

A paragraph nested under a list item is prose a reader still has to get through, so it is measured on
its dedented text — the marker line is skipped, its continuation is not. The one indentation that does
exempt is **four spaces or a tab**, which is Markdown's own indented code block. Continuations in this
corpus align with their marker, at two or three spaces, so the two never collide.

**The paragraph cap is the one that matters.** Length is not the defect; verbosity is. A long page that
reads in short paragraphs and clear lists is a good page, and a short one that reads as a wall of text is
not. The line cap is a backstop against a page nobody split, not a budget to spend down.

`docs/architecture.md` is capped tighter still, at 200 lines: it is the front door, and a front door that
grows a mechanism has stopped being one.

Run `scripts/check-docs.sh --report` while writing — it demotes the caps to warnings and prints the
per-page metrics, so one page can be measured before the rest is done.

## Diátaxis, mapped onto this repository

[Diátaxis](https://diataxis.fr/) splits documentation by what the reader is doing. Our pages map onto
it as follows — when a page starts serving two modes at once, that is the moment to split it:

| Mode | Reader is… | Our pages |
|---|---|---|
| Tutorial | learning by doing | `README.md` Quick Start, `docs/walkthrough.md`, the MIG walkthrough, `docs/kv-cache/walkthrough.md` |
| How-to | achieving a goal | `docs/operation/*`, `docs/migration/*`, `docs/development.md`, a domain runbook (`docs/model-store/operations.md`) |
| Reference | looking something up | `docs/accelerator-requests.md`, `docs/settings.md`, `docs/reference/*` |
| Explanation | building understanding | `docs/architecture.md`, `docs/architecture/*`, and the domain pages under `docs/kv-cache/`, `docs/model-store/` and `docs/model-deployment/` (a domain page serves the mode its reader arrives in — contract pages read as reference, mechanism pages as explanation) |

Two consequences worth stating:

- **Do not teach in a reference page**, and do not put a lookup table in an explanation page — link.
- **A tutorial shows real output.** Every command and result in a walkthrough is captured from a live
  cluster, with node names genericized. Never hand-write plausible output.

## Writing rules

- **State the rule first, in the first sentence of the paragraph or section.** A reader who stops
  there must still be correct.
- **Demote rationale into a `> **Why**` note.** The measured failure, the alternative rejected, the
  race that forced the design — valuable, but not on the critical reading path:

  ```markdown
  The gate reads capacity rather than allocatable.

  > **Why** — allocatable also falls to zero when a family is merely saturated, which would delete the
  > keys while instances are live.
  ```

- **Delete some rationale rather than demoting it.** Before writing a `> **Why**` that names the error a
  step avoids, ask **who takes that step** — not which page you are on. One the operator performs
  unconditionally leaves the reader nothing to decide, so its error is noise: delete the block, then
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
- **Prefer a table for anything enumerable** — modes, keys, vendors, gates, knobs.
- **Name the code.** `pkg/nodefeature`, `node_queue.go`, `TestUnitResourcesPresetDocs` — a reader
  should be able to jump from the claim to the source. Do not paste code that will drift; name it.
- **No symbol-numbered cross-references** (`switch ①`, "gate-2 above"). Use the heading name and a
  link — the numbering breaks the moment a page is split.
- **Wrap at about 100 columns**, and keep tables on one line each (a wrapped table row is unreadable in
  a diff).
- **Links are relative** (`../settings.md`, `architecture/admission.md`), never absolute GitHub URLs —
  the exception is the chart README, which is rendered outside the repo on Artifact Hub.

## Prose tells

Docs here are reference and technical text, so the voice is neutral and plain. Text written on autopilot
carries patterns a careful writer rarely chooses on purpose. Read a draft once for them before you run
the checker; the checker does not read prose. The patterns are adapted from the
[humanizer](https://github.com/blader/humanizer) skill (MIT), strongest first. The first five justify an
edit on one sighting.

| Pattern | Watch for | Write instead |
|---|---|---|
| Not X but Y | "not just X, but Y", "it is not X, it is Y", "X rather than Y", "This does not mean X. It means Y.", a clipped tail such as ", no guessing" | Say Y. Keep the contrast only when the reader really holds the belief X, or when both halves carry a fact |
| One-line closer | a sentence that restates the paragraph above it, "That is the real win.", "That distinction matters.", a row of fragments ("No cache. No retry. No queue.") | Delete it, or merge the fragments into one sentence with a concrete claim. Keep a closer only if it adds a consequence the text above does not show |
| Sayings that sound deep | "the real question is", "at its core", "fundamentally", "X is the language of Y", "becomes a trap" | The specific claim |
| Staged run-up | "Let's dive in", "here is what you need to know", "Honestly?", "the thing is", "note that" before a routine claim | Start with the point |
| Arguing with no one | "To be clear", "this is not about", "a tempting approach would be", a rejected option no page proposes | Delete it. Keep a rejection only when a reader would really weigh that option, and then state the reason once |
| Forced triads | three parallel items where the meaning has two, "fast, reliable, and scalable" | One item per real idea; keep three only when there are three |
| Dashes as connector | ` — `, ` – `, ` -- ` between clauses | A period, comma, colon or parentheses. The header block, footer and table cells keep their template separators, and code, paths and URLs are untouched |
| Stacked qualifiers | "could potentially", "may arguably", "in some cases it might" | One hedge, and only when the code or a measurement supports the doubt |
| Inflated significance | "pivotal", "crucial", "plays a key role", "marks a shift", a closing paragraph about the future | The fact. End a page on its last concrete fact |
| Stock AI words | additionally, delve, robust (figurative), seamless, leverage, showcase, highlight (verb), landscape, tapestry, testament, underscore, valuable, key (adjective) | A plain word, or nothing. "Gate" and "robust" used in their technical sense are fine |
| Avoiding is, are, has | "serves as", "acts as", "features", "boasts", "offers" | "is", "are", "has" |
| Shallow -ing riders | ", ensuring X", ", enabling Y", ", reflecting Z" tacked onto a fact | Drop the rider, or make it its own sentence with the mechanism named |
| Bold as decoration | bold on ordinary terms, a bold label and colon on every bullet | Plain text. Turn a list whose labels carry nothing into a sentence or a table |
| Decorative headings | Title Case below the H1, emoji, arrows, a rule between sections, a heading that restates its first sentence | Sentence case, none of the decoration |
| Writing about the page | "the table below compares", "this section is organized by", "was added to replace" | State the subject. Mention a previous version only in `docs/migration/*` and release notes |
| Chat residue | "Certainly!", "I hope this helps", "let me know", an offer to expand | Delete it |

Three limits keep the cleanup from doing harm:

- **Keep every fact.** A rewrite must not add or drop a name, number, version, key, condition or
  ranking. A sentence that needs a detail you do not have gets a simpler wording, not a guess.
- **Change prose only.** Code blocks, inline code, commands, paths, link targets and table rows pinned by
  a test (see the invariants in `SKILL.md`) stay as they are. A heading keeps its words, because
  changing them breaks every inbound anchor; re-casing is safe.
- **A pattern is a default, not a crime.** A `> **Why**` note may legitimately correct a belief the
  reader holds ("allocatable also falls to zero when a family is merely saturated"), and a quotation, a
  title or a proper name keeps its wording. One weak tell alone (a single dash, a single hedge) is not
  worth an edit; several together are.

After a rewrite, search the result again for the survivors: not-X-but-Y contrasts, closers, triads,
dashes and bold labels.

## Adding a page

1. Put it under the directory that fits: the reader it serves (`architecture/`, `operation/`,
   `migration/`, `reference/`), or the domain directory of the CR family it orbits
   (`kv-cache/`, `model-store/`, `model-deployment/`) when the page joins a family that already has one.
2. Copy the template above; fill the header block honestly — an inflated read time is worse than none.
3. Add a row to the `docs/README.md` page table, and a step to any reading path it belongs on.
4. Add it to the routing table in the skill's `SKILL.md` and to `references/page-map.md`, saying what it
   owns and what it must not absorb.
5. Link to it from the page a reader arrives from — a page reachable only through the index is a page
   nobody reads.
6. Run `scripts/check-docs.sh`.
