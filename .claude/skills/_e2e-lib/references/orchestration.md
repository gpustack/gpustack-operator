# E2E orchestration protocol (shared)

The multi-specialist workflow shared by `gpustack-operator-e2e` and
`gpustack-operator-chart-e2e`. Each skill's `SKILL.md` owns its case table, its domain focus, and
its report-directory suffix; **this file owns the flow** — roles, phases, the parallelism/rendezvous
rules, the report layout, and the optional fix-and-retest loop. Read it before Phase 0.

The run is driven by a **test-orchestration lead** (the main agent) who coordinates a set of
**domain specialists** (subagents). The lead is the single cluster writer; specialists are read-only
analysts who report back to the lead. Everything the run learns is captured to a durable report.

## Roles

### Test-orchestration lead — the main agent

- **The only cluster writer.** Drives every mutating step (`build-load.sh`, `deploy.sh`, the mutating
  cases, `teardown.sh`, and any fix-and-retest packaging) **serially**. No one else touches cluster state.
- Confirms the environment, assembles the test-item list, plans who does what, fans out specialists at
  each rendezvous, collects their findings, drives root-cause triage, and writes `run-log.md` incrementally.
- **Continuously observes and upgrades the suite itself.** While running, watches for gaps in the test
  suite and in this protocol — mock blind spots, missing assertions, test-design mismatches, uncovered spec
  Stories, workflow/report improvements — and accrues them to a **self-upgrade backlog** in the report.
- Relays specialist findings and step-by-step progress to the user in real time (a short `log`-style line
  per step); specialists report **to the lead**, not to the user.

### Domain specialists — subagents spawned via the `Agent` tool

The lead **decides the roster dynamically** from the changed surface (`git diff --name-only
origin/main...HEAD`) and the cases in play — spin up only the lenses a change actually needs, don't run
idle specialists. Map a lens to an `agentType`:

| Lens | `agentType` |
|---|---|
| Logic / functional / behavioral correctness | `agent-skills:test-engineer` |
| Security (RBAC, admission/over-admit, finalizer/leftover leakage) | `agent-skills:security-auditor` |
| Code quality / root-cause on a FAIL | `agent-skills:code-reviewer` |
| Performance, compatibility/regression, or anything above lacks | `general-purpose` + a role prompt |

Every specialist prompt **must** state, verbatim: *"You are READ-ONLY. Never mutate the cluster — no
`create`/`apply`/`delete`/`label`/`patch`/`edit`, no `helm install/upgrade/uninstall`, no build/deploy.
Read the provided `raw/NN-*.txt` snapshot, the code diff, and the spec story; use only read-only
`kubectl get/describe/logs` if you must touch the live cluster."* Any write a specialist attempts is
outside the skill's allow-list and will prompt — that is the backstop, not the primary guard.

Each specialist returns a compact structured verdict (see **Specialist return schema** below), which the
lead folds into the report. Specialists do not write the report themselves.

## Hard rules (carried from each SKILL.md, reinforced here)

- **Never switch kube context** — confirm the active context is the intended local cluster; never
  `kubectl config use-context`.
- **Build locally, never push** — `build-load.sh` keeps `PACKAGE_PUSH=false`. The only exception is an
  explicit remote fix-and-retest the user chose in Phase 7.
- **Touch only objects this run creates** — never modify or delete the user's pre-existing resources.
- **Every mutating step is confirmed** before running; read-only steps run without prompting.
- **Specialists are read-only.** The lead is the sole writer.

## Phases

The lead advances through these in order, writing to `run-log.md` as it goes.

1. **Phase 0 — Environment (lead, read-only).** Run `preflight.sh`; show host tools, active context, and
   `kubectl get nodes -o wide`. **Do not continue until the user confirms** it is the intended local
   `k3s`/`docker-desktop` cluster. Record `git rev-parse HEAD` and `git diff --stat origin/main...HEAD`
   to judge drift. Create the report dir and write the **header** + **node inventory**.

2. **Phase 1 — Test-item list (lead, read-only + interactive).**
   - **Drift from main** (diff is non-empty): match `git diff --name-only origin/main...HEAD` against the
     skill's case table, and separately cross-check the spec (`specs/<...>.md` → `### User Stories` /
     `#### Story N`; the operator suite tracks `specs/2026-06-29-instancetype-unified-pool-refactor.md`)
     against the code changes to surface **new or adjusted test items beyond the case table**. Search with
     an `Explore` subagent when a case's intent is unclear. Present *existing cases + new items* to the
     user with `AskUserQuestion` to confirm.
   - **No drift** (diff empty): ask the user whether there are new test items. If the ask is too coarse,
     invoke the global `agent-skills:interview-me` skill to converge on concrete cases one question at a time.
   - Output: the confirmed test-item list.

3. **Phase 2 — Plan (lead).** Split the list into ① a **serial mutating sequence**
   (`build → deploy → case-N → … → teardown`, lead-only) and ② **parallel read-only analysis tasks** for
   specialists. Pick the roster (per the table above). For each specialist state its **input** (the case's
   `raw/NN.txt` snapshot / code diff / spec story), its **output** (structured verdict), and its
   **rendezvous** (which mutating step must converge before it starts — see below). Write this into the
   report's **test-plan** section.

4. **Phase 3 — Build & deploy (lead, confirm).** `TAG=dev-$(git rev-parse --short HEAD)`, then
   `build-load.sh "$TAG"` and `deploy.sh "$NS" "$TAG"` (or `helm upgrade --reuse-values --set
   image.tag="$TAG"` + `rollout restart` to redeploy over an existing release). Capture output to `raw/`.
   Then `assert-core.sh "$NS"` — the **running binary revision must equal HEAD** (stale-image guard).

5. **Phase 4 — Execute + analyze (lead serial, specialists parallel).** For each selected case, in order:
   run the case (confirm if mutating), capture to `raw/NN-caseN.txt`, let it converge. Then, as one
   **rendezvous barrier**, fan out that case's specialists (a single message with multiple `Agent` calls so
   they run concurrently), each reading the case's snapshot. Collect their verdicts, `log` progress to the
   user, write the **execution-record** section — **then** proceed to the next mutating case. Never start
   the next mutating step while a case's analysis window is open.

6. **Phase 5 — Triage (lead + specialists).** For every FAIL/suspicion, dig in (read code, read logs, run
   read-only experiments). Record each as a `Finding #N (severity)` block — **symptom / root cause
   (`file:line`) / experiment that pins it / fix direction** — and a triage verdict: **excluded /
   confirmed / source / repro steps**. When a finding exposes a suite/protocol gap (a mock that hid the
   bug, a missing assertion, a test-design mismatch), also append it to the self-upgrade backlog.

7. **Phase 6 — Summary (lead).** Write the **summary** section: the finally-confirmed problems (table),
   repro steps per item, and the current conclusion. Classify each as *real bug / test-design issue /
   awaiting-owner*. Also write the **suite gaps & upgrade backlog** section from what was accrued during the run.

8. **Phase 7 — Fix & retest / Suite upgrade (optional).** Two parallel, user-gated branches (see below):
   fix product bugs and retest, and/or sink the self-upgrade backlog into the suite/protocol. Skip straight
   to Phase 8 if the user declines both.

9. **Phase 8 — Teardown (mandatory standalone phase).** Per the `e2e-teardown-standalone-phase`
   convention, teardown is **not** a per-case prompt and is **not** skippable by asking — run
   `teardown.sh gpustack-system` unconditionally as the final phase (confirm the single mutating call),
   keeping the `gpustack-system` namespace on purpose. **Only exception:** if a Phase 7 fix-and-retest loop
   is in progress, defer teardown until that loop ends. (`chart-e2e` runs its CASE 2 "zero-leftovers"
   assertion right after this teardown.)

## Parallelism & rendezvous

The case scripts all mutate the same cluster / same namespace / same cluster-scoped objects (CRDs,
APIServices, webhooks) and self-recover via `trap EXIT`. **They cannot run in parallel on one cluster.** So:

1. **All cluster mutations are serial** — the lead is the sole writer.
2. **Specialists are strictly read-only** (see Roles). Parallelism is analysis, never mutation.
3. **The rendezvous is one barrier per case:** mutating case completes → converges (assert) → fan out
   read-only analysis → collect verdicts → only then the next mutating case. No new mutation starts while a
   case's analysis window is open, so nothing pollutes an in-flight analysis.
4. **Prefer the captured snapshot.** A specialist's primary input is the case's `raw/NN.txt`; it touches
   the live cluster only when necessary, and only within the barrier when the lead guarantees no concurrent
   mutation.
5. **Concurrency lives inside a case** — several specialists analyzing one case's output at once — **not**
   across cases.

## Report

Reuse the existing `.claude/reports/` convention (do **not** create `.claude/test-reports/`). One
directory per run:

```
.claude/reports/<date +%F>-<operator-e2e|chart-e2e>/
  run-log.md          # Chinese narrative, matching .claude/reports/2026-07-02-eks-e2e/run-log.md
  raw/NN-<step>.txt    # captured command output, zero-padded, in execution order
```

`run-log.md` is written **in Chinese** (matching the existing report convention) with these sections:

- **Header** — date; cluster; code sha vs `origin/main`; first-run image; (later) fix-retest image.
- **Node inventory** — a table (node / instance type / accelerators / role).
- **Test plan** — order + parallel groups + specialist assignments + rendezvous points.
- **Execution record** — per phase/case: action taken, each specialist's verdict, initial symptoms.
- **Triage** — `Finding #N (severity)` blocks (symptom / root cause `file:line` / experiment / fix
  direction) + triage verdict (excluded / confirmed / source / repro).
- **Summary** — confirmed-problem table + repro steps + current conclusion.
- **Suite gaps & upgrade backlog** — mock blind spots, missing assertions, test-design mismatches,
  uncovered spec Stories, and workflow/protocol improvements observed during the run.
- **Fix & retest / Suite upgrade** (if run) — fixable-item confirmation, local/remote connection info,
  fix commits / sunk cases / protocol edits, retest results.

Capture mechanism: `<script command> 2>&1 | tee .claude/reports/<dir>/raw/NN-<label>.txt`. Read-only script
prefixes and `tee .claude/reports/*` are on the allow-list, so read-only steps capture without a prompt;
mutating script prefixes are **not** on the allow-list, so they still prompt. Write `run-log.md`
incrementally with `Write`/`Edit`.

## Fix & retest (optional)

Mirrors the EKS run (`.claude/reports/2026-07-02-eks-e2e/run-log.md`, phases 6–9):

1. From the summary, the lead picks the **fixable items** (real bugs — exclude test-design issues and
   awaiting-owner observations) and confirms with the user (`AskUserQuestion`) whether to fix now.
2. On confirmation, ask again: **local packaging** vs **remote (SSH/RSYNC) packaging**. For remote,
   **record the user's connection info** in the report (host, repo path, `PACKAGE_NAMESPACE`, tag — e.g.
   `<user>@<builder-host>:<repo-path>`, `PACKAGE_NAMESPACE=<ns>`). **Run the remote build in a login
   shell**: a non-interactive `ssh host '<cmd>'` does not source the profile, so `go`/the toolchain are
   off PATH (`go: command not found`, `make package` exits 127/2) — wrap it as
   `ssh host 'bash -lc "cd <repo> && … make package"'`.
3. Fix loop, each step confirmed: edit code → `make lint` → commit with **`--signoff`** (conventional
   commit; fold fixes into their originating commit rather than a follow-up; add a test only when it guards
   a real regression, per the project's testing conventions) → package (local `build-load.sh`, or remote
   SSH login shell `bash -lc "… make package PACKAGE_PUSH=true"`) → deploy (`helm upgrade --reuse-values --set image.tag=…` +
   `rollout restart`) → retest the affected cases → append results to the report's fix-and-retest section.
4. SSH / RSYNC / `git commit` / image push are outward-facing; none are on the allow-list, so each prompts.
   Only run teardown (Phase 8) after the loop ends.

## Suite upgrade (optional)

The counterpart to fix-and-retest: instead of fixing product code, sink the **self-upgrade backlog** back
into the suite and this protocol so the next run is better. This mirrors the EKS run's phase 9 ("下沉 e2e
cases"), where real-cluster findings became new cases and a corrected case table.

1. From the backlog, the lead proposes concrete upgrades and confirms each with the user
   (`AskUserQuestion`) — **never edit the skill's own files silently.**
2. On confirmation, apply the upgrade:
   - **Suite** — add/adjust a `cases/case-*.sh`, tighten an assertion, fix a test-design mismatch, or
     update the case table / `references/` in the skill.
   - **Protocol** — edit this `orchestration.md` (flow, roster strategy, report layout) or a `SKILL.md`.
3. Commit with **`--signoff`** (conventional commit; fold into the originating commit where it applies). If
   a case changed, retest it before teardown. Record the edits in the report's suite-upgrade section.

## Specialist return schema

Ask each specialist to return a compact block the lead can paste into the report:

```
lens:      <logic|security|perf|...>
case:      <case id / analysis target>
verdict:   PASS | FAIL | SUSPECT
symptom:   <what was observed, if not PASS>
evidence:  <raw/NN.txt line refs, code file:line, or kubectl read>
direction: <initial root-cause / fix direction, if not PASS>
```
