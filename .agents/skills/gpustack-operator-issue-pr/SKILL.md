---
name: gpustack-operator-issue-pr
description: "Conventions for filing a GPUStack Operator issue or opening a pull request: the six issue title prefixes with their label and type, the 80-character cap, the Conventional-Commit grammar for PR titles, and the three linking verbs. Invoke before `gh issue create` or `gh pr create`, which apply none of a template's frontmatter. Examples: \"file an issue\", \"open a PR\", \"link a PR without closing the issue\"."
allowed-tools: "Read, Write, Bash(gh issue create*), Bash(gh pr create*), Bash(gh issue view*), Bash(gh pr view*), Bash(gh label list*)"
model: sonnet
---

# GPUStack Operator — file an issue, open a PR

Two title conventions live in this repository and they must not bleed into each other: **issues** take
a kind prefix (`bug: `), **pull requests** take Conventional Commits (`fix(chart): `). A squash merge
makes the PR title `main`'s commit subject, and `ci.yml`'s changelog parses exactly that grammar.

**Nothing enforces this.** No hook refuses a non-compliant filing and no workflow rejects a title, so
this page is the whole of the convention — read it before you file, not after.

## The mechanism you must know

The prefix, label and type in an issue template's frontmatter are applied by GitHub's **web**
new-issue flow **only**. Over the API — which is what `gh` uses — none of the three is applied:
`gh issue create --title … --body-file …` produces no prefix, no label and no type. `gh`'s
`--template` flag is documented as supplying *starting body text* and nothing more.

So an agent complies only by passing `--label` and `--type` explicitly.

## Issues — prefix, cap, label, type

| Prefix | Label | Type | Template |
| --- | --- | --- | --- |
| `bug: ` | `kind/bug` | `Bug` | [BUG_REPORT.md](../../../.github/ISSUE_TEMPLATE/BUG_REPORT.md) |
| `enhancement: ` | `kind/enhancement` | `Enhancement` | [ENHANCEMENT.md](../../../.github/ISSUE_TEMPLATE/ENHANCEMENT.md) |
| `support: ` | `kind/support` | `Question` | [SUPPORT.md](../../../.github/ISSUE_TEMPLATE/SUPPORT.md) |
| `docs: ` | `kind/documentation` | `Documentation` | [DOCUMENTATION.md](../../../.github/ISSUE_TEMPLATE/DOCUMENTATION.md) |
| `cleanup: ` | `kind/cleanup` | `Task` | [CLEANUP.md](../../../.github/ISSUE_TEMPLATE/CLEANUP.md) |
| `todo: ` | `todo` | `Task` | [TODO.md](../../../.github/ISSUE_TEMPLATE/TODO.md) |

Those six prefixes are the whole set — there is no seventh, and no prefix-less issue.

**The title is at or under 80 characters, prefix included.** One clause, naming the symptom or the
outcome — not the diagnosis. Detail belongs in the body; a title that needs a comma is usually two
issues. `todo: ` is for work a pull request knowingly left undone.

The recipe:

```bash
# The frontmatter prefix/label/type is applied by GitHub's web flow only — over the API, pass them.
gh issue create --title 'bug: <one clause, ≤80 chars including the prefix>' \
  --label kind/bug --type Bug --body-file <file>
```

Swap the three values per the table row. `--type` takes the org-level type name character for
character. Add an `area/*` or `priority/*` label with a second `--label` when you know it.

Do **not** file through `mcp__github__create_issue`: its input schema carries `labels` but no `type`
field, so an issue filed that way can never have an issue type.

## Pull requests

The title is a Conventional Commit: `<type>: subject`, or `<type>(<scope>): subject` with the scope in
**literal parentheses** — `fix(chart): rebuild NFD with HOSTMOUNT_PREFIX`. `<type>` is one of `build`
`chore` `ci` `docs` `feat` `fix` `perf` `refactor` `revert` `style` `test`, and a `!` before the colon
marks a breaking change. `ci.yml`'s changelog parses exactly this grammar —
`^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([\w\-./]+\))?(!)?: .+` — so a title
it cannot parse still merges, but its release note lands under "Other". An issue prefix (`bug: `)
never appears on a PR title.

Fill the body from [PULL_REQUEST_TEMPLATE.md](../../../.github/PULL_REQUEST_TEMPLATE.md), and keep its
`/kind` and `/area` lines. Those are Prow commands: their target labels all exist, but **Prow is not
installed yet, so they apply nothing today** — write them anyway so the PR records what it is, and
expect a maintainer to label it by hand until Prow lands.

### The three linking verbs

GitHub auto-closes an issue on merge only for `Fix(es|ed)`, `Close(s|d)` and `Resolve(s|d)`. That is
why the other two verbs are safe to use on unfinished work:

- `Fixes #<n>` — the issue is fully resolved by this PR; **GitHub closes it on merge**.
- `Addresses #<n>` — this PR advances the issue but does not finish it; **no auto-close**.
- `Relates #<n>` — context only, no resolution claimed; **no auto-close**.

Pick the weakest verb that is true. A PR that only fixes a failing test or a flake does not use
`Fixes` on the issue that reported the underlying defect.
