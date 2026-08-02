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

**See also** — [Device Discovery](discovery.md) · [Walkthrough](../walkthrough.md)

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

## Diátaxis, mapped onto this repository

[Diátaxis](https://diataxis.fr/) splits documentation by what the reader is doing. Our pages map onto
it as follows — when a page starts serving two modes at once, that is the moment to split it:

| Mode | Reader is… | Our pages |
|---|---|---|
| Tutorial | learning by doing | `README.md` Quick Start, `docs/walkthrough.md`, the MIG walkthrough |
| How-to | achieving a goal | `docs/operation/*`, `docs/migration/*`, `docs/development.md` |
| Reference | looking something up | `docs/accelerator-requests.md`, `docs/settings.md`, `docs/reference/*` |
| Explanation | building understanding | `docs/architecture.md` and `docs/architecture/*` |

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

- **State a fact once.** The second place links to the first. If you are tempted to restate it "for
  convenience", the two copies will disagree within two releases.
- **Break a wall of text into named subsections.** If a paragraph carries more than one rule, it is
  more than one paragraph. A `###` heading is cheap and becomes an anchor others can link to.
- **Prefer a table for anything enumerable** — modes, keys, vendors, gates, knobs.
- **Name the code.** `pkg/nodefeature`, `node_queue.go`, `TestUnitResourcesPresetDocs` — a reader
  should be able to jump from the claim to the source. Do not paste code that will drift; name it.
- **No symbol-numbered cross-references** (`switch ①`, "gate-2 above"). Use the heading name and a
  link — the numbering breaks the moment a page is split.
- **Wrap at about 100 columns**, and keep tables on one line each (a wrapped table row is unreadable in
  a diff).
- **Links are relative** (`../settings.md`, `architecture/admission.md`), never absolute GitHub URLs —
  the exception is the chart README, which is rendered outside the repo on Artifact Hub.

## Adding a page

1. Put it under the directory of the reader it serves (`architecture/`, `operation/`, `migration/`,
   `reference/`).
2. Copy the template above; fill the header block honestly — an inflated read time is worse than none.
3. Add a row to the `docs/README.md` page table, and a step to any reading path it belongs on.
4. Add it to the routing table in the skill's `SKILL.md` and to `references/page-map.md`, saying what it
   owns and what it must not absorb.
5. Link to it from the page a reader arrives from — a page reachable only through the index is a page
   nobody reads.
6. Run `scripts/check-docs.sh`.
