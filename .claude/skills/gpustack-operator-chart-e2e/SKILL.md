---
name: gpustack-operator-chart-e2e
description: "Verify the GPUStack Operator **Helm chart** end to end on a reachable local cluster (k3s / docker-desktop): build & load the dev image, `helm install` the chart, assert the worker/device-manager roll out, the versions are consistent (image tag ↔ bundled chart tgz ↔ `gpustack-operator --version`), then `helm uninstall` and assert zero leftovers. SCOPE: this validates the **chart contract** — chart changes, install/startup, image build, and version consistency — and is **NOT a feature/behavioral e2e**; for deep scheduling-chain behavior use `gpustack-operator-e2e` instead. Proactively offer this when a branch ahead of main changes the chart, the in-cluster app-installation code, or the image build. Examples: \"verify the chart installs and uninstalls cleanly\", \"does helm install of the operator work\", \"check the chart version is right\", \"test my chart change end to end\", \"did my kuberess change break the runtime install\"."
allowed-tools: "Read, Agent, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/kube-context.sh*), Bash(bash .claude/skills/gpustack-operator-chart-e2e/cases/case-1.sh*), Bash(kubectl get*), Bash(kubectl describe*), Bash(kubectl logs*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(helm status*), Bash(helm list*), Bash(helm template*), Bash(helm lint*), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*)"
model: sonnet
---

# GPUStack Operator — Helm chart E2E verification

Install the operator **from its Helm chart** onto a **local** cluster and verify it installs, runs, and uninstalls cleanly — with the **right version** baked in.

**Scope — chart contract only, NOT feature e2e:**
- **Validates:** chart changes, install, startup/rollout, the image build, and the version contract (image tag ↔ bundled chart tgz ↔ `gpustack-operator --version`).
- **Does NOT** assert scheduling-chain behavior — deep NFD → Worker → Kueue behavior lives in `gpustack-operator-e2e`. Keep only install/uninstall + version-contract assertions here; add no behavioral assertions.

## Orchestration

Run as a **test-orchestration lead** (main agent) coordinating read-only **domain specialists** (`Agent` tool). **Read [`orchestration.md`](../_e2e-lib/references/orchestration.md) before Phase 0** — it owns roles, phases, parallelism/rendezvous rules, report layout, and the fix-and-retest loop. Below are the chart-e2e specifics.

**Hard rules** (full list in `orchestration.md`; chart-e2e specifics):
- **Pin the run to a user-confirmed context** — `preflight.sh` shows the active one; proceed only after the user confirms the intended local cluster. Never switch on your own judgement. When the confirmed cluster is **not** the active context, do not switch: take its isolated kubeconfig with `bash ../_e2e-lib/scripts/kube-context.sh <ctx>` and prefix every command of the run with the `KUBECONFIG=<path>` it prints (details in `../_e2e-lib/references/orchestration.md`). On the user's explicit say-so you may switch instead — then record the context to return to as an outstanding environment mutation and restore it in Phase 8.
- **Build locally, never push** — `build-load.sh` keeps `PACKAGE_PUSH=false`.
- **Touch only what this run creates** — the Helm release + its cleanup. Never modify or delete the user's pre-existing resources.
- **Confirm every mutating step** (`build-load.sh`, `deploy.sh`, CASE 2, CASE 3); read-only steps (`preflight.sh`, CASE 1) run unprompted.
- **Specialists are read-only** — the lead is the sole writer.

**Layout:**
- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG> [--set …]`, `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`), `kube-context.sh <ctx>` (read-only; targets a context that is not the current one).
- `cases/case-N.sh` — one scenario each; ends in a `STATUS | CHECK | OBJECT` table, exits non-zero on any FAIL.
- `references/version-contract.md`; shared `../_e2e-lib/references/{orchestration,troubleshooting}.md`.

## Cases (locked titles)

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Script | Mutates |
|---|---|---|---|---|
| 1 | Install + version consistency | always (mandatory) | `cases/case-1.sh` | no |
| 2 | Uninstall leaves zero leftovers | `deploy/gpustack-operator/chart/**`, `pkg/worker/kuberess/**` | `cases/case-2.sh` | yes (confirm) |
| 3 | Release version survives a warm build cache | `pack/gpustack-operator/Dockerfile`, `hack/package.sh` (optional) | `cases/case-3.sh` | yes (confirm) |

## Case header contract

Every `cases/case-N.sh` opens with a header describing the case **on its own terms — no spec anchors** (no Story/Task numbers, F-codes, `specs/*.md` paths, or commit hashes). A reader must understand what the case does, needs, and asserts from the header alone. New/edited cases follow this six-field template:

```
# CASE N — <one-line behavior title>   (<mutation posture>)
#
#   case-N.sh <args>
#
# Goal:        <the one contract/behavior this case proves>
# Environment: <what the cluster/builder must provide + when it AUTO-SKIPS or is OPTIONAL>
# Inputs:      <what the case runs / installs / builds; mark any MOCKED inputs vs the real thing verified>
# Expected:    <the PASS assertions — the observable final state>
# Cleanup:     <what the trap / teardown restores; idempotent + safe to re-run>
```

- **Mutation posture** rides the title line: `READ-ONLY` (asserts existing state, no trap) or `MUTATING` (writes). Append the running constraint when there is one — `run LAST` (teardown), `OPTIONAL`, or `local build only` (no cluster).
- **Environment** always names the constraint (cluster with the chart installed, a local Docker builder, a warm-cache prerequisite) so the run/skip decision is readable without executing the case.
- **Inputs** distinguish any **MOCKED** inputs from the **real** objects under test — these chart-contract cases mock nothing, so they state "nothing mocked".
- Describe behavior in **plain terms** (e.g. "the running binary version equals the bundled tgz"), not by spec reference; the case↔code mapping lives in the table's "Run when these change" column.
- Runtime output (results banner, `record` messages, FAIL-footer) stays spec-anchor-free and self-explanatory.

## Flow

Generic phase semantics (roles, rendezvous, report, fix-retest) live in [`orchestration.md`](../_e2e-lib/references/orchestration.md); below are the chart-e2e specifics. Let `RPT=.claude/reports/$(date +%F)-chart-e2e`.

**Phase 0 — Environment (read-only).** Preflight, confirm the context, record drift; optionally `helm lint deploy/gpustack-operator/chart`:

```bash
bash .claude/skills/_e2e-lib/scripts/preflight.sh
git rev-parse HEAD; git diff --stat origin/main...HEAD
```

Do not continue unless the user confirms a local `k3s` / `docker-desktop` context. Then `mkdir -p "$RPT"/raw` and write the report header + node inventory.

**Phase 1 — Test-item list.** Match `git diff --name-only origin/main...HEAD` against the case table; CASE 1 always included. Confirm with the user (`AskUserQuestion`) when a high-impact surface (chart, `kuberess` app-install, image build) changed. No drift → ask for new items, invoking `interview-me` if the ask is too coarse.

**Phase 2 — Plan.** Pick the specialist roster — version-contract consistency → `agent-skills:test-engineer`; uninstall cleanliness / leftover-leak → `agent-skills:security-auditor`; image build → `general-purpose`; root-cause → `agent-skills:code-reviewer`. Keep to the chart contract — **no behavioral assertions**. Write the test-plan section.

**Phase 3 — Build & deploy (confirm).**

```bash
TAG=dev-$(git rev-parse --short HEAD); NS=gpustack-system
bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG"   2>&1 | tee "$RPT"/raw/01-build.txt
bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG" 2>&1 | tee "$RPT"/raw/02-deploy.txt
```

- To exercise the **device-manager runtime install** (the version-critical path — see `references/version-contract.md`), add `--set deviceManager.enabled=false`.
- To exercise the gated post-delete hook in-cluster, add `--set cleanupOnUninstall=true`.

**Phase 4 — Execute + analyze.** CASE 1 read-only (no prompt); pass the built `$TAG` so it also checks the deployed image tag. Optional CASE 3 (confirm) reproduces the version/warm-cache path — run it after the Phase 3 build so the cache is warm. Per the rendezvous rule, fan out specialists on each case's snapshot before the next mutating step.

```bash
bash .claude/skills/gpustack-operator-chart-e2e/cases/case-1.sh "$NS" "$TAG" 2>&1 | tee "$RPT"/raw/10-case1.txt   # mandatory, read-only
# optional, warm-cache (confirm):
bash .claude/skills/gpustack-operator-chart-e2e/cases/case-3.sh              2>&1 | tee "$RPT"/raw/11-case3.txt
```

Read the PASS/FAIL table; do not re-derive from raw output.

**Phase 5 — Triage.** On a FAIL/suspicion, diagnose (`kubectl -n "$NS" describe/logs deploy/gpustack-operator-worker`, `helm status gpustack-operator -n "$NS"`) and record a `Finding #N` block.

**Phase 6 — Summary.** Write the summary (confirmed problems + repro + conclusion) + the **suite gaps & upgrade backlog** accrued during the run.

**Phase 7 — Fix & retest / Suite upgrade (optional).** Per `orchestration.md`, two user-gated branches: (a) real bugs → confirm, choose local vs remote (SSH/RSYNC) packaging (record connection info), fix → `make lint` → signoff commit → package → redeploy → retest → append; (b) sink the self-upgrade backlog into the suite (new/adjusted `cases`, tightened assertions, case-table/`references` updates) or this protocol — confirm each, signoff commit, retest any changed case.

**Phase 8 — Teardown (mandatory) — CASE 2, uninstall & assert zero leftovers.** Run unconditionally as the final phase (confirm the single call); defer only if a Phase 7 loop is still running. CASE 2 runs the shared `teardown.sh` then asserts the cluster is clean:

```bash
bash .claude/skills/gpustack-operator-chart-e2e/cases/case-2.sh gpustack-system 2>&1 | tee "$RPT"/raw/90-case2-teardown.txt
```

`teardown.sh` removes the operator release + worker-installed Kueue/NFD/CSI sub-releases, their CRDs/finalizers, and the runtime APIServices/webhooks. The `gpustack-system` namespace is kept on purpose. Never delete the user's pre-existing resources.

## References

- `../_e2e-lib/references/orchestration.md` — the shared multi-specialist flow: roles, phases, rendezvous rules, report layout, and the fix-and-retest loop.
- `references/version-contract.md` — the three-view version contract, the device-manager runtime-install path, and the warm-cache risk CASE 3 reproduces.
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
