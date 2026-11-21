---
name: gpustack-operator-docs
description: "Write or update the GPUStack Operator documentation: route a new fact to the page that owns it, and keep each page's title, Contents, footer and the docs/README.md index in sync. Also adding a page or reviewing a docs diff."
---

# GPUStack Operator — Documentation

Route every change through the three-zone contract and keep context lean by loading detail files
only when needed:

- **Routing & index (read first)**: [`docs/README.md`](../../../docs/README.md) (`All pages`,
  `Reference`, `Contribute`, and `#module-map`) and [references/page-map.md](references/page-map.md)
  (per-page ownership and exclusion boundaries).
- **Style, templates & diagrams**: [references/conventions.md](references/conventions.md) (read when
  drafting/rewriting prose, YAML, tables, or SVG/Mermaid diagrams).
- **Automated gate**: `make lint docs` (`scripts/check-docs.sh`, `check-specs.sh`, `check-crossrefs.sh`,
  `skills.sh`, Hugo build, `site-links.py`, `agent-index.py`).

## Three-Zone Contract

| Zone | Paths | Site HTML / Search / Sidebar | `/llms.txt` | Outbound link policy |
| --- | --- | --- | --- | --- |
| Public Human Site | `docs/{getting-started,walkthroughs,modules,operate}/` | Included (`navigation.yaml`) | `/docs/<page>/index.md` | Public pages, `docs/assets/`, non-`docs/` repo files; **never** `reference/` or `contribute/` |
| LLM Reference | `docs/reference/*.md` (flat, zero subdirs) | Excluded | `/reference/*.md` (rewritten) | Sibling `reference/*.md`, public pages, assets, repo/`contribute/` (GitHub URLs) |
| Contributor | `docs/contribute/*.md` | Excluded | `## Optional` (GitHub URLs) | Any repository path |

- Keep design rationale in `specs/`, not `docs/`; guides carry only brief `> **Why**` notes when
  needed to prevent operators from undoing a rule.
- Keep `docs/README.md` labels equal to each page's H1 character for character, in navigation order.
  Update `docs/README.md`, `docs/reference/README.md`, `site/data/navigation.yaml`, and
  `references/page-map.md` in the same PR when adding, moving, or renaming pages.

## Hard rules

- **Headings & outlines**: Hard `H1`–`H3` ceiling (`H4+` forbidden across `docs/**`). Public pages
  stay at `<= 8` content `##` sections—group related subtopics as `###` children rather than
  flattening 9+ peer `##`s. Every H2–H3 is `<= 4` words and `<= 32` visible chars (excluding
  `Step N:` / `Rule N:` / `Stage N:` / `Gate N:` / `Suite N:` prefixes; hyphenated parts count as
  separate words), sentence case below H1, zero interrogatives (`what/where/when/how/which/why/who`),
  zero layout-container nouns (`rows`, `table`, `columns`, `guide`, `entries`, `section`), zero
  leading articles (`a`, `an`, `the`) on non-`Step N:` headings, and self-explanatory at first
  glance (no cryptic negative/insider labels like `No policy flag` or `Zero grants`).
- **Flattening H4s**: Keep child sub-items under a peer category (e.g. Ascend A5 under `### Ascend`)
  as bold lead-ins or tables inside that `###`—never promote them to sibling `###` next to peer
  categories.
- **Page & module shape**: Every page (except `docs/README.md` and `docs/reference/README.md`) has
  an intro, `## Contents` mirroring its `##` headings, and `**See also**` footer (public adds
  `**Next**` where applicable; `docs/reference/*.md` omits `**Next**` and declares `contract`,
  `recorded capture`, or `synthetic simulation`). Capability modules cover Overview, Configuration,
  Operations, and Troubleshooting without `<150`-line orphan sub-pages. Converge multi-module flows
  into `docs/walkthroughs/`, matching outer capability sidebar order.
- **No dev-journal noise**: Never write `recorded on <old-version>` / `verified on <old-version>`
  stamps or blank `not recorded` rows in public configuration guides. State hard version floors
  (`>= X.Y`) directly, route compatibility matrices to
  `docs/modules/model-deployment/engine-versions.md` (sidebar label: `Compatibility`), and state
  benchmark/walkthrough hardware once in `## Environment` rather than repeating `In the recorded
  run...` in every step.
- **Tested facts & combinations**: Document only verified behavior and complete configurable
  combinations (never partial sets that imply false exclusivity). Use `worker.gpustack.ai/v1` for
  public APIs (`v1alpha1` only in `docs/contribute/`).
- **YAML, tables & prose**: Pretty block YAML only (no inline JSON objects/arrays); full resource
  headers (`apiVersion`, `kind`, `metadata`) on every Kubernetes resource and `diff` block. Show one
  annotated macro YAML + micro constraint bullets (`<= 20` words) or uniform tables (1–3 word
  headers, no articles/demonstratives)—never duplicate full CR schemas in field tables. Prose stays
  `<= 5` lines / `500` chars per paragraph, imperative for instructions, objective for behavior,
  backtick kinds with outside-backtick plurals (`` `Pod`s ``, `` `Topology` resources ``,
  `` `Devices` objects ``).
- **Diagrams & site**: Use light/dark WCAG-AA SVGs under `docs/assets/diagrams/` paired with hidden
  Mermaid source comments (`->` / `..>` instead of `-->` / `-.->`). Never hand-edit generated chart
  docs. Keep `docs/getting-started/architecture.md` `<= 200` lines.

## Sync Invariants (Code ↔ Docs)

| Doc | Pinned to | Breakage / Gate |
| --- | --- | --- |
| `docs/getting-started/architecture.md` `## Components` | `cmd/gpustack-operator/main.go` subcommands, `*/python/pyproject.toml` (`engine-sentinel`, `model-transfer`), `csrc/` | `check-docs.sh` fails on missing runtime subcommand/package or `> 200` lines |
| `docs/reference/instance-type-unit-resources.md` | `pkg/nodefeature/unit_resources_preset.yaml` | Compare preset rows manually when either changes |
| `deploy/gpustack-operator/chart/README.md`, `values.schema.json` | `values.yaml` + `README.md.gotmpl` via `make generate chart` | Generated—never hand-edit; `chart.yml` fails on drift |
| `README.md` accelerator matrix & Usage examples | `pkg/nodefeature/knowns.go`, `pkg/devicemanager/detector/*/device.go`, `docs/modules/devices/requests.md` (*Resource keys*, *Pod request examples*) | Silent drift; update both together |
| `configuration.md`, `model-deployment-internals.md`, `model-deployment-parameters.md` | `modelDeploymentOwnedKeys` | Compare owned keys per engine when rendering changes |
| `docs/reference/settings.md` | `pkg/worker/settings` & `GPUSTACK_*` readers (also `.auto-sync/*`) | Keep exact path; sync defaults and effect windows |
| `docs/modules/model-delivery/remote.md` | `.auto-sync/*` | Keep exact path; verify via `hack/check/autosync-selftest.sh` |
| `docs/README.md` zone tables & `docs/reference/README.md` | All files under `docs/` and H1 titles | `check-docs.sh` and `agent-index.py` fail |

## Before You Finish

```bash
make lint docs                                                             # full CI gate
bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh --report  # per-page metrics
SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site   # when changing site/templates
```

Automated gates verify links, anchors, zone isolation, H1–H3 depth, `<= 8` public H2s, heading
length/nouns/articles, kind plurals, noise stamps, `architecture.md` components/cap, `/llms.txt`,
and HTML/Markdown exports. Review manually for what gates cannot check:

- [ ] Every changed fact matches current Go/Python source, webhooks, and async reconcile behavior.
- [ ] Facts are stated once and linked elsewhere; terminology/italics match `references/conventions.md`.
- [ ] Sidebar label, breadcrumb, and H1 align in rendered HTML; diagrams are legible in light & dark themes.
- [ ] Zero prose tells (`references/conventions.md`); YAML comments match actual defaults and types.
- [ ] `values.yaml` changes regenerated via `make generate chart`; moved paths updated across tracked files.
