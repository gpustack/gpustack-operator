---
name: gpustack-operator-release
description: "Cut a GPUStack Operator release end to end: sync & verify `main`, pick and confirm the release commit, draft highlight-focused release notes from the Conventional-Commit history, cut & push the `vX.Y.Z` git tag, watch every tag-triggered pipeline (`ci.yml` image/Release + `ci-chart.yml` Helm chart) go green, then promote the CI-published pre-release to the official/latest release. Releases are published as **pre-releases first** and promoted only after CI is green and the user confirms. Proactively offer this when the user wants to ship/tag/publish a new version. Examples: \"release v0.7.0\", \"cut the next release\", \"tag and publish v0.7.0\", \"do an rc pre-release\", \"ship v0.7.0 and watch CI\"."
allowed-tools: "Read, AskUserQuestion, Bash(git fetch*), Bash(git log*), Bash(git tag -l*), Bash(git tag --list*), Bash(git rev-parse*), Bash(git status*), Bash(git show*), Bash(git diff*), Bash(git describe*), Bash(git merge-base*), Bash(make version), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*), Bash(gh auth status*), Bash(gh release list*), Bash(gh release view*), Bash(gh run list*), Bash(gh run view*), Bash(gh run watch*), Bash(gh pr list*), Bash(gh pr view*)"
model: sonnet
---

# GPUStack Operator — cut a release

Drive a full release from a version number to a published GitHub Release. Cutting a release is a single
act — **pushing a `vX.Y.Z` git tag** — which fans out to two pipelines:

- `ci.yml` → multi-arch image + the **GitHub Release** object (published as a **pre-release** with a
  categorized baseline note; see the note-generation contract below).
- `ci-chart.yml` → the Helm chart, repacked from the tag and published to `github-pages`.

The version propagates from the tag into the binary (`pack/utils.mk` / `hack/lib/version.sh` → ldflags →
`gpustack-operator --version`) and into the chart, so the git tag is the single source of truth. There is
no `VERSION` file to bump.

**Release-note contract.** The `release` job in `ci.yml` builds a categorized baseline note
(`mikepenz/release-changelog-builder-action` in COMMIT mode, grouped by Conventional-Commit prefix) and
publishes the release as `prerelease: true`. This skill's job is to (a) produce a **better,
highlight-focused** note and replace the baseline, and (b) **promote** the pre-release to the official
release once CI is green. Nothing gets the GitHub **Latest** badge until you promote it — that is the gate.

## Hard rules

- **Release only off the `origin/main` line.** Tag a commit that is an ancestor of `origin/main`; default
  to `origin/main` HEAD. Do not switch branches to do this — tag by SHA.
- **Tag shape must match `v*.*.*`** — final `vX.Y.Z`, pre-release `vX.Y.ZrcN` (any tag containing `rc` stays
  a pre-release and must not be promoted). The build's version derivation requires this shape.
- **Every mutating step is confirmed** — creating/pushing the tag, `gh release edit`, and any tag/release
  deletion prompt for approval. Read-only inspection (git log/status, `gh run list/watch`, `gh release
  view`) runs without prompting.
- **Never force-push; never delete a published tag or release** without an explicit, separate confirm.
- **Auto mode does not promote** — it stops at the published pre-release (see Modes).

## Modes

- **Interactive (default).** Confirm at each gate: the (version, commit) pair, the drafted notes, the tag
  push, the note replacement, and the final promotion.
- **Auto / bypass-permissions** (the user says "auto", or runs with permissions skipped). Replace every
  confirmation with the sensible default — target = `origin/main` HEAD, notes generated without asking which
  items to surface — and run **Phase 0 → 5 unattended, then stop at the published pre-release** (skip Phase
  6). Report the release URL and tell the user to promote to the official release on GitHub when ready.

## Flow

Let `VER` = the requested version (e.g. `v0.7.0`) and `RPT=.claude/reports/$(date +%F)-release-$VER`.

### Phase 0 — Preflight (read-only)

Confirm the tooling and the release line before touching anything.

```bash
gh auth status
git fetch origin --tags --prune
git status --porcelain                              # must be empty — refuse on a dirty tree
git rev-parse origin/main                           # the default target SHA
git tag -l 'v*' --sort=-creatordate | head -n 5     # recent release tags → the previous one
git log --oneline -1 origin/main
make version
```

Report whether local `main` matches `origin/main` (`git rev-parse main origin/main`, if `main` exists
locally). If they differ and the user is on `main`, offer `git pull --ff-only`; otherwise just tag off
`origin/main` — no branch switch is needed. Then `mkdir -p "$RPT"` for the run artifacts.

### Phase 1 — Version + commit (confirm)

- Validate `VER` against `^v[0-9]+\.[0-9]+\.[0-9]+(rc[0-9]+)?$`; reject if the tag already exists
  (`git tag -l "$VER"` must be empty); sanity-check it is greater than the previous tag.
- Default target = `origin/main` HEAD (`SHA=$(git rev-parse origin/main)`).
- If the user wants a different commit, list candidates and let them pick (`AskUserQuestion`):

  ```bash
  git log --date=short --pretty='%h  %ad  %s' origin/main | head -n 20
  ```

Confirm the final **(version, commit)** pair before proceeding.

### Phase 2 — Draft release notes (confirm)

Read the range since the previous release and group by Conventional-Commit type.

```bash
LAST=$(git tag -l 'v*' --sort=-creatordate | head -n1)
git log --no-merges --pretty='%h %s (%an)' "$LAST..$SHA"
```

Compose `$RPT/notes.md`:

- Sections **🚀 Features / 🐛 Fixes / ♻️ Refactor / 📚 Docs / Other**, concise bullets, imperative voice.
- Keep the **highlights**; fold or drop noisy `chore`/`ci`/`build`/`test`/`style` unless notable.
- Link PRs/issues: parse `(#NNN)` from subjects; for squash/merge commits without one, recover via
  `gh pr list --search "<subject>"` / `gh pr view`.
- End with **Full Changelog**: `https://github.com/gpustack/gpustack-operator/compare/$LAST...$VER`.
- **When unsure which items to surface, list them and ask the user** which to include (`AskUserQuestion`).
  In auto mode, skip the question and include the sensible default set.

### Phase 3 — Cut & push the tag (confirm → prompts)

```bash
git tag -a "$VER" "$SHA" -m "Release $VER"
git push origin "$VER"
```

This triggers `ci.yml` and `ci-chart.yml`. With the note-generation contract, `ci.yml` creates the GitHub
Release as a **pre-release** carrying the categorized baseline body.

### Phase 4 — Monitor CI (read-only)

Watch **every** run for the tag (both workflows) to completion. Tag pushes show up with the tag name as the
head branch.

```bash
gh run list --branch "$VER" --limit 20              # enumerate ci.yml + ci-chart.yml runs
gh run watch <run-id> --exit-status --compact       # per run, blocks until it finishes
```

Report per-workflow status. On any failure: `gh run view <run-id> --log-failed`, triage, and **stop — do
not promote**. Offer (each gated by a confirm) to fix forward, or to delete and retry:

```bash
# retry after a fix (mutating — confirm each):
gh release delete "$VER" --yes --cleanup-tag
# or, if the release object isn't there yet:
git push origin --delete "$VER" && git tag -d "$VER"
```

### Phase 5 — Refine & attach notes (confirm → prompts)

Once all runs are green, replace the CI baseline body with the curated notes.

```bash
gh release view "$VER"                               # inspect the CI-generated baseline first
gh release edit "$VER" --notes-file "$RPT/notes.md"
```

The release is already `prerelease` (from the CI contract) — nothing else to flip here.

### Phase 6 — Promote (interactive only; confirm → prompts)

Confirm with the user (`AskUserQuestion`) that the pre-release is good, then promote it to the official,
latest release:

```bash
gh release edit "$VER" --latest --prerelease=false
```

Skip this phase entirely for `rc` tags and in auto mode — leave those as pre-releases.

### Phase 7 — Summary

Report the tag, the release URL, the CI run URLs, and the final state; persist a short summary next to
`$RPT/notes.md`.

```bash
gh release view "$VER" --json tagName,isPrerelease,isDraft,url
```
