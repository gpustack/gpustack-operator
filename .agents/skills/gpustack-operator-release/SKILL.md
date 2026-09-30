---
name: gpustack-operator-release
description: "Cut a GPUStack Operator release end to end: draft the notes, push the `vX.Y.Z` tag, watch the tag-triggered pipelines, and promote the pre-release once CI is green. Also retries publication of an existing version."
disable-model-invocation: true
allowed-tools: "Read, AskUserQuestion, Bash(git fetch*), Bash(git ls-tree*), Bash(git show*), Bash(git log*), Bash(git tag -l*), Bash(git tag --list*), Bash(git rev-parse*), Bash(git status*), Bash(git show*), Bash(git diff*), Bash(git describe*), Bash(git merge-base*), Bash(make version), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*), Bash(gh auth status*), Bash(gh release list*), Bash(gh release view*), Bash(gh api repos/gpustack/gpustack-operator/releases/latest*), Bash(gh run list*), Bash(gh run view*), Bash(gh run watch*), Bash(gh pr list*), Bash(gh pr view*), Bash(curl -sL https://gpustack.github.io/gpustack-operator/*), Bash(tar tzf /tmp/*), Bash(tar xzf /tmp/*)"
---

# GPUStack Operator — cut a release

Drive a full release from a version number to a published GitHub Release. Cutting a release is a single act — **pushing a `vX.Y.Z` git tag** — which fans out to two pipelines:

- `ci.yml` → multi-arch image + the **GitHub Release** object (published as a **pre-release** with a categorized baseline note).
- `site.yml` → the Helm chart and versioned documentation, published together to `github-pages` and deployed once.

The version propagates from the tag → binary (`pack/utils.mk` / `hack/lib/version.sh` → ldflags → `gpustack-operator --version`) and → chart, so **the git tag is the single source of truth** — there is no `VERSION` file to bump.

**Release-note contract.** `ci.yml`'s `release` job builds a categorized baseline note (`mikepenz/release-changelog-builder-action` in COMMIT mode, grouped by Conventional-Commit prefix) and publishes as `prerelease: true`. This skill's job: (a) produce a **better, highlight-focused** note and replace the baseline, and (b) **promote** the pre-release to the official release once CI is green. Nothing gets the GitHub **Latest** badge until you promote — that is the gate.

**Published versions are immutable.** A tag with published chart or documentation content cannot
be re-cut at a different commit. Retry its workflows at the same source revision, edit its release
notes, or choose a new version. Delete and recreate is limited to recovering a version with neither
a chart package nor a documentation record; it is interactive-only and requires explicit approval.
The Site workflow preserves package bytes on retries and rejects a changed source revision before
preparing chart changes.

## Hard rules

- **Release off any branch; default to `origin/main`.** Tag by SHA — never switch branches. Default target = `origin/main` HEAD; when the user names another branch/commit (e.g. a maintenance line like `origin/v0.5-dev`), tag that. A maintenance-line commit will **not** be an ancestor of `origin/main` — expected, not an error; confirm the (branch, commit) with the user and flag it as a maintenance release in the summary.
- **Tag shape is exactly `vX.Y.Z` (final) or `vX.Y.Z-rcN` (pre-release)** — nothing else; a hyphen-less form like `v0.6.1rc1` is rejected (Phase 1 regex). The `-rcN` **hyphen is required**: `site.yml` sets the Helm chart version to the tag without the `v`, and Helm enforces strict SemVer2, which rejects a hyphen-less pre-release like `0.6.1rc1`; the build's version derivation requires this shape too. Any tag containing `rc` stays a pre-release and must not be promoted.
- **Every mutating step is confirmed** — creating/pushing the tag, `gh release edit`, and any tag/release deletion. Read-only inspection (git log/status, `gh run list/watch`, `gh release view`) runs without prompting.
- **Never force-push or move a published tag.** Inspect both the Pages manifest and chart inventory before offering recovery. A lookup failure is not evidence that a version is unpublished.
- **Never delete a release without exporting its notes first.** `gh release delete` destroys the body irrecoverably — GitHub keeps no history of it. Phase 3 writes it to `$RPT/notes-original-$VER.md` **before** teardown; retain that file when recovering an unpublished version.
- **Recovery is interactive-only.** Auto mode never deletes an existing release or tag. Recreating an unpublished version that already holds the **Latest** badge requires a separate explicit confirmation.
- **Auto mode does not promote** — it stops at the published pre-release (see Modes).

## Modes

- **Interactive (default).** Confirm at each gate: the (version, commit) pair, the drafted notes, the tag push, the note replacement, and the final promotion.
- **Auto / bypass-permissions** (user says "auto", or runs with permissions skipped). Replace every confirmation with the sensible default — target = the branch the user named (its HEAD), else `origin/main` HEAD; notes generated without asking — and run **Phase 0 → 5 unattended, then stop at the published pre-release** (skip Phase 6). Report the release URL and tell the user to promote to the official release on GitHub when ready. **Auto never overwrites** — if the tag already exists, stop and hand back to a human (see Phase 1).
- **Unpublished-version recovery** layers on top of interactive mode when Phase 1 finds the tag already exists and the user chooses recovery. It is never entered in auto mode. It adds a teardown-and-recreate step (Phase 3) plus a re-promote step for a recovered GA (Phase 6); every other phase is unchanged.

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
- Default target = `origin/$BR` HEAD (`SHA=$(git rev-parse "origin/$BR")`, `BR` defaulting to `main`).
- **Existing-tag decision.** Fetch the current Pages branch and inspect its manifest and chart
  package before changing an existing tag:

  ```bash
  git fetch origin github-pages
  git show FETCH_HEAD:versions.json > "$RPT/versions.json"
  git ls-tree -r --name-only FETCH_HEAD -- "charts/gpustack-operator-${VER#v}.tgz"
  gh release view "$VER" --json isPrerelease,isDraft,tagName,url
  gh api repos/gpustack/gpustack-operator/releases/latest --jq '.tag_name'
  ```

  Read the manifest's `published` records. If either a record for `$VER` or its chart package
  exists, do not offer delete and recreate. Compare the tag's resolved commit with any recorded
  source revision. Offer a retry at the original source, a new version, or abort. A chart with no
  documentation record does not prove its source revision; verify provenance before adding a site.
  A failed fetch or manifest read blocks the decision rather than establishing absence.

  If neither chart nor documentation has been published, interactive recovery may use the existing
  teardown procedure after confirmation. Set `OVERWRITE=1`, record `ORIG_SHA`, `WAS_GA` and
  `WAS_LATEST`, and skip the greater-than-previous-tag check. A missing release object is a bare tag,
  not a failed probe; `releases/latest` returning 404 means no stable release exists.

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
# In recovery mode `grep -Fvx "$VER"` drops the tag being re-published (fixed-string, whole-line match,
# so the dots in the version aren't treated as regex) so it can't become its own predecessor.
LAST=$(git tag -l 'v*' --merged "$SHA" --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -Fvx "$VER" | head -n1)
# no prior stable tag on this line (brand-new repo/branch): fall back to the full history up to $SHA
git log --no-merges --pretty='%h %s (%an)' ${LAST:+"$LAST.."}"$SHA"
```

Compose `$RPT/notes.md`:

- **Don't start the body with a title/heading that repeats the tag** (e.g. `## vX.Y.Z`) — GitHub renders the release name as the page title, so a leading title shows up as a duplicate. Start with an optional one-line intro or the first section.
- Use applicable sections in this order: **🚀 Features / 🐛 Fixes / ♻️ Refactor / 📚 Docs / ⚠️ Limitations / Other**; omit empty sections. Put limitations in their own **⚠️ Limitations** section before Other. Put release materials and artifact inventories in Appendix or Other. Keep bullets concise and in imperative voice.
- Keep the **highlights**; fold or drop noisy `chore`/`ci`/`build`/`test`/`style` unless notable.
- For **Refactor**, compare against `$LAST`, the previous stable release; RC tags do not reset that baseline. Omit internal renames or restructures of modules and APIs first introduced after `$LAST`: they are part of developing the new feature, not a change to previously released behavior. Include a refactor only when it changes something that already existed in `$LAST` and matters to release users.
- Include an **Appendix** section in every release note with `Images:` and `Charts:` lists. Compare with `$LAST` to identify added or updated items, then list only the resulting references: one `repository:tag` or `name:version` per bullet. For a synthesized image without a fixed tag, list its reference template. Do not show old versions, `old -> new`, or status labels in the bullets. Include the operator image tag and root chart version for each new release; write `- None` if a list has no changes. A short note after the lists may distinguish images built by this tag's CI from referenced dependencies and identify optional components.
- Build those lists from the tag's image/chart workflows, `deploy/gpustack-operator/chart/` metadata, values and templates (including disabled-by-default components), and image defaults and workload renderers in the controllers. Inspect the actual changes in each source; a default Helm render or a changed-file summary alone misses conditional workloads and vendored chart versions. Do not invent versions for configurable image overrides.
- Link PRs/issues: parse `(#NNN)` from subjects; for squash/merge commits without one, recover via `gh pr list --search "<subject>"` / `gh pr view`.
- End with **Full Changelog**: `https://github.com/gpustack/gpustack-operator/compare/$LAST...$VER`.
- **Unsure which items to surface → list them and ask** (`AskUserQuestion`). Auto mode: skip the question, include the sensible default set.

### Phase 3 — Cut & push the tag (confirm → prompts)

**Recovery teardown (only when `OVERWRITE=1`).** Repeat the Pages manifest and chart inventory
checks immediately before teardown. Refuse if content has been published since preflight. Confirm
that the existing image tag may change, the release publish date resets, and the Latest badge may
move temporarily. Export the release notes and record the original tag commit before deletion.

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

This triggers `ci.yml` and `site.yml`. Per the note-generation contract, `ci.yml` creates the GitHub Release as a **pre-release** carrying the categorized baseline body.

### Phase 4 — Monitor CI (read-only)

Watch **every** run for the tag (both workflows) to completion. Tag pushes show up with the tag name as the head branch. `site.yml` publishes the chart and exact-tag documentation together, deploys their complete content snapshot, and verifies public bytes. `ci.yml` builds the multi-arch image and vendor SDK stages.

Enumerate the runs, then chain both watches into **one backgrounded command** and wait for its completion notification:

```bash
gh run list --branch "$VER" --limit 20              # enumerate ci.yml + site.yml runs
# one command, both runs, run_in_background: true — each watch blocks until its run finishes
gh run watch <site-run-id> --exit-status --compact > /tmp/w-site.log 2>&1; echo "SITE-EXIT=$?" >> /tmp/w-site.log
gh run watch <ci-run-id>    --exit-status --compact > /tmp/w-ci.log    2>&1; echo "CI-EXIT=$?"    >> /tmp/w-ci.log
```

**Do not also poll in the foreground.** A `sleep 300; gh run list` blocks the whole turn doing nothing and duplicates the watcher already tracking the same runs. Wait for the background task's notification, then read the `*-EXIT=` lines — the chained command's own exit code is the trailing `echo`, not the watch, so judge each run by its marker. If a status check is genuinely needed mid-flight, one bare `gh run list` is enough; never a multi-minute `sleep`.

Report per-workflow status. On any failure, read `gh run view <run-id> --log-failed`, diagnose it,
and stop before promotion. Retry a failed run at the same source commit. If a source change is
required, cut a new version once any chart or documentation has been published; do not delete and
recreate that tag. Notes can be edited without replacing release artifacts.

### Phase 5 — Refine & attach notes (confirm → prompts)

Once all runs are green, verify the Appendix image and chart lists against the image tags produced by CI and the published chart package (root and bundled `Chart.yaml` files), correcting the draft before replacing the CI baseline body with the curated notes. Do not describe a referenced image as published by this release unless the workflow produced it.

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

**After unpublished-version recovery**, CI always rebuilds the release as a **pre-release** (`ci.yml` sets `prerelease: true`). Re-promote here **only when `WAS_GA`** — i.e. the release being recovered was itself a promoted GA; if it was a pre-release/rc, leave it as a pre-release. When re-promoting, restore the badge with `--latest` if `WAS_LATEST`, otherwise follow the maintenance-line rule above.

Skip this phase entirely for `rc` tags and in auto mode — leave those as pre-releases.

### Recovering a bad publication

For an unchanged source commit, rerun the failed workflow. After a Site fix reaches `main`, the
Site workflow can be dispatched with `ref` set to the existing tag: it uses current publishing tools
and the tag's exact sources. Confirm the public manifest, chart index and package bytes before
calling the retry successful. It must preserve the original package.

For a source change, publish a new version with its own notes and artifacts. Leave the previous
tag in place and explain the correction in the new release notes. An image or release-note problem
alone does not justify moving the tag or replacing a published chart.

### Phase 7 — Summary

Report the tag, the release URL, the CI run URLs, the Appendix image and chart lists, and the final state; persist a short summary next to `$RPT/notes.md`.

```bash
gh release view "$VER" --json tagName,isPrerelease,isDraft,url
```

**Verifying the Latest badge.** `isLatest` is **not** a field on `gh release view --json` — it rejects unknown fields client-side (`Unknown JSON field: "isLatest"`), because "Latest" is a repo-level pointer, not a per-release property. `isPrerelease=false` proves it is a GA, not that it holds the badge. When Phase 6 promoted with `--latest`, confirm the badge actually landed by comparing the repo's latest-release tag to `$VER`:

```bash
gh api repos/gpustack/gpustack-operator/releases/latest --jq '.tag_name'   # == $VER for a promoted GA
```

For a maintenance-line release that kept `--latest=false`, this correctly still points at the newer GA — not `$VER` — so a mismatch there is expected, not a failure.

**After unpublished-version recovery**, note in the summary that the `vX.Y.Z` image tag was overwritten in place; anyone verifying that the new content actually runs should pin the fresh `@sha256:` digest rather than trusting the mutable tag.
