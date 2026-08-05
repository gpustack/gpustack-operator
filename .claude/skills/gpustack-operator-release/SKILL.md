---
name: gpustack-operator-release
description: "Cut a GPUStack Operator release end to end: sync & verify the target branch (any branch, default `main`), pick and confirm the release commit, draft highlight-focused release notes from the Conventional-Commit history, cut & push the `vX.Y.Z` git tag, watch every tag-triggered pipeline (`ci.yml` image/Release + `chart.yml` Helm chart) go green, then promote the CI-published pre-release to the official/latest release. Releases are published as **pre-releases first** and promoted only after CI is green and the user confirms. Also supports **overwrite (re-publish)** — re-cutting an existing `vX.Y.Z` under the same version via delete+recreate, interactive-only and confirm-gated. Proactively offer this when the user wants to ship/tag/publish a new version. Examples: \"release v0.7.0\", \"cut the next release\", \"tag and publish v0.7.0\", \"do an rc pre-release\", \"ship v0.7.0 and watch CI\", \"overwrite v0.7.0\", \"re-release v0.7.0\", \"redo the release\", \"覆盖发布 v0.7.0\", \"重新发布 v0.7.0\"."
allowed-tools: "Read, AskUserQuestion, Bash(git fetch*), Bash(git log*), Bash(git tag -l*), Bash(git tag --list*), Bash(git rev-parse*), Bash(git status*), Bash(git show*), Bash(git diff*), Bash(git describe*), Bash(git merge-base*), Bash(make version), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*), Bash(gh auth status*), Bash(gh release list*), Bash(gh release view*), Bash(gh api repos/gpustack/gpustack-operator/releases/latest*), Bash(gh run list*), Bash(gh run view*), Bash(gh run watch*), Bash(gh pr list*), Bash(gh pr view*), Bash(curl -sL https://gpustack.github.io/gpustack-operator/*), Bash(tar tzf /tmp/*), Bash(tar xzf /tmp/*)"
model: sonnet
---

# GPUStack Operator — cut a release

Drive a full release from a version number to a published GitHub Release. Cutting a release is a single act — **pushing a `vX.Y.Z` git tag** — which fans out to two pipelines:

- `ci.yml` → multi-arch image + the **GitHub Release** object (published as a **pre-release** with a categorized baseline note).
- `chart.yml` → the Helm chart, repacked from the tag and published to `github-pages`.

The version propagates from the tag → binary (`pack/utils.mk` / `hack/lib/version.sh` → ldflags → `gpustack-operator --version`) and → chart, so **the git tag is the single source of truth** — there is no `VERSION` file to bump.

**Release-note contract.** `ci.yml`'s `release` job builds a categorized baseline note (`mikepenz/release-changelog-builder-action` in COMMIT mode, grouped by Conventional-Commit prefix) and publishes as `prerelease: true`. This skill's job: (a) produce a **better, highlight-focused** note and replace the baseline, and (b) **promote** the pre-release to the official release once CI is green. Nothing gets the GitHub **Latest** badge until you promote — that is the gate.

**Overwrite (re-publish).** Re-cutting an already-published `vX.Y.Z` under the **same version** — to fix a mis-tagged commit, a bad build, or notes that need a full redo — is a supported but **non-default, interactive-only** path. It works by **delete + recreate** (drop the release and tag, re-tag the new SHA, push again); it relies on CI being overwrite-idempotent (mutable Docker Hub tags, `softprops/action-gh-release@v2` update-in-place, `stefanprodan/helm-gh-pages` overwriting the chart `.tgz`) so the re-pushed tag re-runs both pipelines over the same version. It never force-pushes and always requires an explicit confirm — see Phase 1 (detection) and Phase 3 (teardown). It is **reversible but not free**: *Rolling back an overwrite* restores the original version and re-cuts the new work under a fresh number, at the cost of a rewritten publish timestamp and a window in which the tag served other content.

## Hard rules

- **Release off any branch; default to `origin/main`.** Tag by SHA — never switch branches. Default target = `origin/main` HEAD; when the user names another branch/commit (e.g. a maintenance line like `origin/v0.5-dev`), tag that. A maintenance-line commit will **not** be an ancestor of `origin/main` — expected, not an error; confirm the (branch, commit) with the user and flag it as a maintenance release in the summary.
- **Tag shape is exactly `vX.Y.Z` (final) or `vX.Y.Z-rcN` (pre-release)** — nothing else; a hyphen-less form like `v0.6.1rc1` is rejected (Phase 1 regex). The `-rcN` **hyphen is required**: `chart.yml` sets the Helm chart version to the tag without the `v`, and Helm enforces strict SemVer2, which rejects a hyphen-less pre-release like `0.6.1rc1`; the build's version derivation requires this shape too. Any tag containing `rc` stays a pre-release and must not be promoted.
- **Every mutating step is confirmed** — creating/pushing the tag, `gh release edit`, and any tag/release deletion. Read-only inspection (git log/status, `gh run list/watch`, `gh release view`) runs without prompting.
- **Never force-push.** An existing tag is **rejected by default**; the only way past it is the explicit, confirm-gated **overwrite path** (delete + recreate — Phase 1 detects, Phase 3 tears down), which never force-pushes and never mutates a published tag/release without a separate explicit confirm.
- **Never delete a release without exporting its notes first.** `gh release delete` destroys the body irrecoverably — GitHub keeps no history of it. Phase 3 writes it to `$RPT/notes-original-$VER.md` **before** teardown; that file is the only thing that makes a rollback possible (see *Rolling back an overwrite*).
- **Overwrite is interactive-only.** Auto mode never overwrites — an existing tag stops it and hands back to a human. Overwriting a release that is already a **promoted GA** or currently holds the **Latest** badge requires an **extra** explicit confirm, shown after the consumer-impact warning.
- **Auto mode does not promote** — it stops at the published pre-release (see Modes).

## Modes

- **Interactive (default).** Confirm at each gate: the (version, commit) pair, the drafted notes, the tag push, the note replacement, and the final promotion.
- **Auto / bypass-permissions** (user says "auto", or runs with permissions skipped). Replace every confirmation with the sensible default — target = the branch the user named (its HEAD), else `origin/main` HEAD; notes generated without asking — and run **Phase 0 → 5 unattended, then stop at the published pre-release** (skip Phase 6). Report the release URL and tell the user to promote to the official release on GitHub when ready. **Auto never overwrites** — if the tag already exists, stop and hand back to a human (see Overwrite).
- **Overwrite (re-publish)** layers on top of interactive mode when Phase 1 finds the tag already exists and the user chooses to overwrite. It is never entered in auto mode. It adds a teardown-and-recreate step (Phase 3) plus a re-promote step for a formerly-promoted GA (Phase 6); every other phase is unchanged.

## Flow

Let `VER` = the requested version (e.g. `v0.7.0`) and `RPT=.claude/reports/$(date +%F)-release-$VER`.

### Phase 0 — Preflight (read-only)

Confirm the tooling and the release line before touching anything. Let `BR` = the target branch (the branch the user named, else `main`); fetch it explicitly if it is not `main` (`git fetch origin "$BR"`).

```bash
gh auth status
git fetch origin --tags --prune
git status --porcelain                              # must be empty — refuse on a dirty tree
git rev-parse "origin/$BR"                          # the default target SHA (BR defaults to main)
git tag -l 'v*' --sort=-creatordate | head -n 5     # recent release tags → the previous one
git log --oneline -1 "origin/$BR"
```

Nothing meaningful to print before the tag exists (version is derived from it at build time via ldflags — the git tag is the single source of truth). Report whether local `$BR` matches `origin/$BR` (`git rev-parse "$BR" "origin/$BR"`, if the local branch exists); if they differ and the user is on `$BR`, offer `git pull --ff-only`, otherwise just tag off `origin/$BR` by SHA — no branch switch. Then `mkdir -p "$RPT"` for the run artifacts.

### Phase 1 — Version + commit (confirm)

- Validate `VER` against `^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$` (pre-releases need the SemVer2 hyphen, e.g. `v0.6.1-rc1`).
- **Existing-tag decision.** If `git tag -l "$VER"` is empty, this is a fresh release — sanity-check `VER` is greater than the previous tag and continue. If it is **not** empty, do not hard-refuse: probe the current state, then ask (`AskUserQuestion`) whether to **pick a new version**, **overwrite the existing one** (destructive re-publish), or **abort**.

  ```bash
  git tag -l "$VER"                                                            # non-empty → overwrite decision
  gh release view "$VER" --json isPrerelease,isDraft,tagName,url               # is there a release? already a GA?
  gh api repos/gpustack/gpustack-operator/releases/latest --jq '.tag_name'     # does $VER currently hold Latest?
  ```

  Both probes tolerate "not found" — treat it as state, not failure: `gh release view` exits non-zero when the tag exists without a release object (a bare tag → the tag-only teardown path in Phase 3), and `releases/latest` 404s when the repo has no GA yet (`WAS_LATEST` false). Proceed to the decision either way.

  On **overwrite**, set `OVERWRITE=1` and record `WAS_GA` (release exists with `isPrerelease=false`) and `WAS_LATEST` (the latest-release tag equals `$VER`) for Phase 3/6. In overwrite mode **skip** the "greater than the previous tag" check — reusing the same version is the point.
- Default target = `origin/$BR` HEAD (`SHA=$(git rev-parse "origin/$BR")`, `BR` defaulting to `main`).
- Different commit wanted → list candidates and let the user pick (`AskUserQuestion`):

  ```bash
  git log --date=short --pretty='%h  %ad  %s' "origin/$BR" | head -n 20
  ```

Confirm the final **(version, commit)** pair before proceeding.

### Phase 2 — Draft release notes (confirm)

Read the range since the previous release and group by Conventional-Commit type.

```bash
# previous *stable* release ON THIS LINE — restrict to tags reachable from $SHA so a maintenance-line
# release picks its own predecessor (e.g. v0.5.3 for v0.5.4, not the global-latest v0.6.x); skip -rcN.
# In overwrite mode `grep -Fvx "$VER"` drops the tag being re-published (fixed-string, whole-line match,
# so the dots in the version aren't treated as regex) so it can't become its own predecessor.
LAST=$(git tag -l 'v*' --merged "$SHA" --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -Fvx "$VER" | head -n1)
# no prior stable tag on this line (brand-new repo/branch): fall back to the full history up to $SHA
git log --no-merges --pretty='%h %s (%an)' ${LAST:+"$LAST.."}"$SHA"
```

Compose `$RPT/notes.md`:

- **Don't start the body with a title/heading that repeats the tag** (e.g. `## vX.Y.Z`) — GitHub renders the release name as the page title, so a leading title shows up as a duplicate. Start with an optional one-line intro or the first section.
- Sections **🚀 Features / 🐛 Fixes / ♻️ Refactor / 📚 Docs / Other**, concise bullets, imperative voice.
- Keep the **highlights**; fold or drop noisy `chore`/`ci`/`build`/`test`/`style` unless notable.
- Link PRs/issues: parse `(#NNN)` from subjects; for squash/merge commits without one, recover via `gh pr list --search "<subject>"` / `gh pr view`.
- End with **Full Changelog**: `https://github.com/gpustack/gpustack-operator/compare/$LAST...$VER`.
- **Unsure which items to surface → list them and ask** (`AskUserQuestion`). Auto mode: skip the question, include the sensible default set.

### Phase 3 — Cut & push the tag (confirm → prompts)

**Overwrite teardown (only when `OVERWRITE=1`).** Before re-tagging, spell out the consumer-impact of re-publishing the same version, then take an **extra** confirm — stronger wording when `WAS_GA`/`WAS_LATEST`:

- The `vX.Y.Z` image tag and the chart `.tgz` are overwritten **in place** — anyone who already pulled them gets different content on the next pull. When verifying afterwards, pin the new `@sha256:` digest rather than trusting the tag alone.
- If `WAS_LATEST`, the Latest badge drops to the next-newest GA during the teardown→re-run window and only returns when you re-promote in Phase 6.
- The release's **publish timestamp resets to now** — a release object cannot be recreated with its historical date, so an overwrite permanently rewrites when that version appears to have shipped. Irreversible even by a rollback.
- **Reusing a version number is not free even when the build is fine.** If the new content changes behavior — anything carrying an `action required` release-note, moved values, or a new manual upgrade step — downstream gets that change under a version number it has already pinned, with no signal to re-read the migration notes. Say so, and offer cutting a **new** version instead; overwrite is for a version whose *artifacts* are wrong, not for one whose *content* should have been a new release.

Then tear down state-aware and recreate back-to-back to minimize the no-release window:

```bash
# Export the body FIRST — `gh release delete` destroys it irrecoverably and it is the only
# source a rollback (or a redo of the new note) can restore from.
# Then delete the release together with the remote tag if a release exists;
# otherwise (bare tag, no release object) delete just the remote tag.
if gh release view "$VER" >/dev/null 2>&1; then
  gh release view "$VER" --json body --jq '.body' > "$RPT/notes-original-$VER.md"
  gh release delete "$VER" --yes --cleanup-tag
else
  git push origin --delete "$VER"                   # bare tag — no release object, nothing to export
fi
git tag -d "$VER"                                   # --cleanup-tag only removes the remote tag; drop the local one too
```

Record `ORIG_SHA=$(git rev-list -n1 "$VER")` **before** the teardown too — a rollback needs the commit the tag used to point at, and nothing else preserves it.

Then cut & push (fresh release, or the recreated one after teardown):

```bash
git tag -a "$VER" "$SHA" -m "Release $VER"
git push origin "$VER"
```

This triggers `ci.yml` and `chart.yml`. Per the note-generation contract, `ci.yml` creates the GitHub Release as a **pre-release** carrying the categorized baseline body.

### Phase 4 — Monitor CI (read-only)

Watch **every** run for the tag (both workflows) to completion. Tag pushes show up with the tag name as the head branch.

```bash
gh run list --branch "$VER" --limit 20              # enumerate ci.yml + chart.yml runs
gh run watch <run-id> --exit-status --compact       # per run, blocks until it finishes
```

Report per-workflow status. On any failure: `gh run view <run-id> --log-failed`, triage, and **stop — do not promote**. Offer (each gated by a confirm) to fix forward, or to delete and retry:

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

Confirm with the user (`AskUserQuestion`) that the pre-release is good, then promote it to the official release:

```bash
gh release edit "$VER" --latest --prerelease=false
```

**Do not take the `--latest` badge on a maintenance-line release when a newer stable tag exists** (e.g. releasing `v0.5.4` while `v0.6.3` is out) — `--latest` would steal the badge from the newer GA. In that case drop `--prerelease=false` alone and keep `--latest=false`:

```bash
gh release edit "$VER" --prerelease=false --latest=false
```

**After an overwrite**, CI always rebuilds the release as a **pre-release** (`ci.yml` sets `prerelease: true`). Re-promote here **only when `WAS_GA`** — i.e. the release you overwrote was itself a promoted GA; if it was a pre-release/rc, leave it as a pre-release. When re-promoting, restore the badge with `--latest` if `WAS_LATEST`, otherwise follow the maintenance-line rule above.

Skip this phase entirely for `rc` tags and in auto mode — leave those as pre-releases.

### Rolling back an overwrite (interactive only; confirm → prompts)

An overwrite can be reversed — restore the original version, and re-cut the new work under a fresh number. Most often decided at the Phase 6 gate, once the overwritten release is there to look at. Two things make this more than editing the Release object back:

- **The artifacts were already overwritten.** The `vX.Y.Z` image tag and chart `.tgz` now hold the new content, so restoring the Release object alone leaves the tag pointing at one commit while the registry and Helm repo serve another. The tag has to be re-cut at `ORIG_SHA` and **pushed, so CI re-runs** and rebuilds the original content back over the new.
- **The body is gone unless Phase 3 exported it** — restore from `$RPT/notes-original-$VER.md`.

Push both tags back to back so all four pipelines run in parallel:

```bash
gh release delete "$VER" --yes --cleanup-tag         # drop the overwritten release + tag
git tag -d "$VER"
git tag -a "$VER" "$ORIG_SHA" -m "Release $VER"      # ORIG_SHA — recorded before the Phase 3 teardown
git push origin "$VER"                               # CI rebuilds the ORIGINAL content over the new
git tag -a "$NEW_VER" "$SHA" -m "Release $NEW_VER"   # the work that was briefly published as $VER
git push origin "$NEW_VER"
```

Once all four runs are green:

- `$VER` — attach `$RPT/notes-original-$VER.md`, then re-promote per Phase 6 if it was `WAS_GA`, but with **`--latest=false`**: `$NEW_VER` is now the newer stable tag, so the maintenance-line rule applies.
- `$NEW_VER` — Phases 5–6 as normal, with the notes **rewritten for its narrower range** (`$VER..$SHA`, not the overwritten draft's `$LAST..$SHA`). Re-run the Phase 2 `LAST` computation rather than editing the old draft by hand: commits that belong to `$VER` must drop out, and the `Full Changelog` link and any `blob/<tag>/` doc links have to follow the new tag.

**Verify the artifacts actually diverged**, not just the Release objects — the chart is the cheapest probe (note the `/charts/` path segment):

```bash
curl -sL -o /tmp/c.tgz "https://gpustack.github.io/gpustack-operator/charts/gpustack-operator-<X.Y.Z>.tgz"
tar tzf /tmp/c.tgz | grep -cE '^gpustack-operator/charts/[^/]+/Chart.yaml'   # e.g. vendored subchart count
tar xzf /tmp/c.tgz -O gpustack-operator/Chart.yaml | grep -E '^(version|appVersion):'
```

Pick something that genuinely differs between the two commits — a file only one side has, or a line one side changed. Matching `version`/`appVersion` alone proves only that the repack ran.

**Residue a rollback cannot undo:** `$VER`'s publish timestamp is now the rollback date, and anyone who pulled the image or chart during the overwrite window holds the other content under the same tag. Both belong in the summary.

### Phase 7 — Summary

Report the tag, the release URL, the CI run URLs, and the final state; persist a short summary next to `$RPT/notes.md`.

```bash
gh release view "$VER" --json tagName,isPrerelease,isDraft,url
```

**Verifying the Latest badge.** `isLatest` is **not** a field on `gh release view --json` — it rejects unknown fields client-side (`Unknown JSON field: "isLatest"`), because "Latest" is a repo-level pointer, not a per-release property. `isPrerelease=false` proves it is a GA, not that it holds the badge. When Phase 6 promoted with `--latest`, confirm the badge actually landed by comparing the repo's latest-release tag to `$VER`:

```bash
gh api repos/gpustack/gpustack-operator/releases/latest --jq '.tag_name'   # == $VER for a promoted GA
```

For a maintenance-line release that kept `--latest=false`, this correctly still points at the newer GA — not `$VER` — so a mismatch there is expected, not a failure.

**After an overwrite**, note in the summary that the `vX.Y.Z` image tag was overwritten in place; anyone verifying that the new content actually runs should pin the fresh `@sha256:` digest rather than trusting the mutable tag.
