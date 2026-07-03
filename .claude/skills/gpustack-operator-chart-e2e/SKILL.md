---
name: gpustack-operator-chart-e2e
description: "Verify the GPUStack Operator **Helm chart** end to end on a reachable local cluster (k3s / docker-desktop): build & load the dev image, `helm install` the chart, assert the worker/device-manager roll out, the versions are consistent (image tag ↔ bundled chart tgz ↔ `gpustack-operator --version`), then `helm uninstall` and assert zero leftovers. SCOPE: this validates the **chart contract** — chart changes, install/startup, image build, and version consistency — and is **NOT a feature/behavioral e2e**; for deep scheduling-chain behavior use `gpustack-operator-e2e` instead. Proactively offer this when a branch ahead of main changes the chart, the in-cluster app-installation code, or the image build. Examples: \"verify the chart installs and uninstalls cleanly\", \"does helm install of the operator work\", \"check the chart version is right\", \"test my chart change end to end\", \"did my kuberess change break the runtime install\"."
allowed-tools: "Read, Agent, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/gpustack-operator-chart-e2e/cases/case-1.sh*), Bash(kubectl get*), Bash(kubectl describe*), Bash(kubectl logs*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(helm status*), Bash(helm list*), Bash(helm template*), Bash(helm lint*), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*)"
model: sonnet
---

# GPUStack Operator — Helm chart E2E verification

Install the operator **from its Helm chart** onto a **local** cluster and verify it installs, runs, and
uninstalls cleanly — with the **right version** baked in.

**Scope — chart contract only, not feature e2e.** This skill is for validating **chart changes, install,
startup/rollout, the image build, and the version contract** (image tag ↔ bundled chart tgz ↔
`gpustack-operator --version`). It is the chart/version counterpart to the `gpustack-operator-e2e` skill,
which exercises the deep NFD → Worker → Kueue scheduling-chain behavior. **Do not** add feature/behavioral
assertions here — keep the deep behavioral e2e (scheduling chain) in `gpustack-operator-e2e`, and keep only
install/uninstall + version-contract assertions here.

## Orchestration

Run this as a **test-orchestration lead** (you, the main agent) coordinating dynamically-chosen
**domain specialists** (read-only subagents via the `Agent` tool). You are the sole cluster writer and
drive every mutating step serially; specialists analyze in parallel and report back to you; the whole run
is captured to a durable report under `.claude/reports/`. **Read
[`orchestration.md`](../_e2e-lib/references/orchestration.md) before Phase 0** — it owns the roles, the
phase-by-phase flow, the parallelism/rendezvous rules, the report layout, and the optional fix-and-retest
loop. The phases below are the chart-e2e specifics that plug into it.

This skill **mutates a cluster**. Hard rules:

- **Never switch kube context.** Show the active context (via `preflight.sh`) and proceed only after the
  user confirms it is the intended local cluster. Never run `kubectl config use-context`.
- Build the image **locally only** — never push (`build-load.sh` keeps `PACKAGE_PUSH=false`).
- Touch only objects this skill creates (the Helm release, its cleanup). **Never** modify or delete the
  user's pre-existing namespaces/resources.
- Every **mutating** step (`build-load.sh`, `deploy.sh`, CASE 2, CASE 3) is confirmed before running.
  Read-only steps (`preflight.sh`, CASE 1) run without prompting.
- **Specialists are read-only.** You are the only writer — fan them out for analysis, never for mutation.

The work is split into shared phase scripts and numbered, self-contained cases:

- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG> [--set …]`,
  `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`).
- `cases/case-N.sh` — one numbered scenario each, ending in a `STATUS | CHECK | OBJECT` table and
  exiting non-zero on any FAIL.
- `references/version-contract.md`, shared `../_e2e-lib/references/orchestration.md` (the
  multi-specialist flow), and shared `../_e2e-lib/references/troubleshooting.md`.

## Cases (locked titles)

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Script | Mutates |
|---|---|---|---|---|
| 1 | Install + version consistency | always (mandatory) | `cases/case-1.sh` | no |
| 2 | Uninstall leaves zero leftovers | `deploy/gpustack-operator/chart/**`, `pkg/worker/kuberess/**` | `cases/case-2.sh` | yes (confirm) |
| 3 | Release version survives a warm build cache | `pack/gpustack-operator/Dockerfile`, `hack/package.sh` (optional) | `cases/case-3.sh` | yes (confirm) |

## Flow

The lead drives these phases; the shared protocol
([`orchestration.md`](../_e2e-lib/references/orchestration.md)) owns the roles, rendezvous rules, report
layout, and fix-and-retest loop. Below are the chart-e2e specifics. Let `RPT=.claude/reports/$(date +%F)-chart-e2e`.

**Phase 0 — Environment (read-only).** Run preflight, confirm the context, record drift; optionally
`helm lint deploy/gpustack-operator/chart`:

```bash
bash .claude/skills/_e2e-lib/scripts/preflight.sh
git rev-parse HEAD; git diff --stat origin/main...HEAD
```

Do not continue unless the user confirms a local `k3s` / `docker-desktop` context. Then
`mkdir -p "$RPT"/raw` and write the report header + node inventory.

**Phase 1 — Test-item list.** Match the changed surface against the case table with
`git diff --name-only origin/main...HEAD`; CASE 1 is always included. Confirm with the user
(`AskUserQuestion`) when a high-impact surface (chart, `kuberess` app-install, image build) changed. No
drift → ask for new items, invoking the global `interview-me` skill if the ask is too coarse.

**Phase 2 — Plan.** Pick the specialist roster — version-contract consistency via
`agent-skills:test-engineer`; uninstall cleanliness / leftover-leak via `agent-skills:security-auditor`;
image build via `general-purpose`; root-cause via `agent-skills:code-reviewer`. Keep to the chart contract —
**no behavioral assertions**. Write the test-plan section.

**Phase 3 — Build & deploy (confirm).**

```bash
TAG=dev-$(git rev-parse --short HEAD); NS=gpustack-system
bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG"   2>&1 | tee "$RPT"/raw/01-build.txt
bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG" 2>&1 | tee "$RPT"/raw/02-deploy.txt
```

To also exercise the **device-manager runtime install** (the version-critical path — see
`references/version-contract.md`), add `--set deviceManager.enabled=false`. To exercise the gated
post-delete hook in-cluster, add `--set cleanupOnUninstall=true`.

**Phase 4 — Execute + analyze.** CASE 1 is read-only (no prompt); pass the built `$TAG` so it also checks
the deployed image tag. Optional CASE 3 (confirm) reproduces the version/warm-cache path — run it after the
Phase 3 build so the cache is warm. Per the rendezvous rule, fan out specialists on each case's snapshot
before the next mutating step.

```bash
bash .claude/skills/gpustack-operator-chart-e2e/cases/case-1.sh "$NS" "$TAG" 2>&1 | tee "$RPT"/raw/10-case1.txt   # mandatory, read-only
# optional, warm-cache (confirm):
bash .claude/skills/gpustack-operator-chart-e2e/cases/case-3.sh              2>&1 | tee "$RPT"/raw/11-case3.txt
```

Read the PASS/FAIL table; do not re-derive from raw output.

**Phase 5 — Triage.** On a FAIL/suspicion, diagnose (`kubectl -n "$NS" describe/logs
deploy/gpustack-operator-worker`, `helm status gpustack-operator -n "$NS"`) and record a `Finding #N` block.

**Phase 6 — Summary.** Write the summary section (confirmed problems + repro + conclusion) plus the
**suite gaps & upgrade backlog** accrued during the run.

**Phase 7 — Fix & retest / Suite upgrade (optional).** Per `orchestration.md`, two user-gated branches:
(a) fixable real bugs → confirm, choose local vs remote (SSH/RSYNC) packaging (record connection info), then
fix → `make lint` → signoff commit → package → redeploy → retest → append results; (b) sink the self-upgrade
backlog into the suite (new/adjusted `cases`, tightened assertions, case-table/`references` updates) or this
protocol — confirm each, signoff commit, retest any changed case.

**Phase 8 — Teardown (mandatory) — CASE 2, uninstall & assert zero leftovers.** Run unconditionally as the
final phase (confirm the single call); defer only if a Phase 7 loop is still running. CASE 2 runs the shared
`teardown.sh` then asserts the cluster is clean:

```bash
bash .claude/skills/gpustack-operator-chart-e2e/cases/case-2.sh gpustack-system 2>&1 | tee "$RPT"/raw/90-case2-teardown.txt
```

`teardown.sh` removes the operator release + worker-installed Kueue/NFD/CSI sub-releases, their
CRDs/finalizers, and the runtime APIServices/webhooks. The `gpustack-system` namespace is kept on purpose.
Never delete the user's pre-existing resources.

## References

- `../_e2e-lib/references/orchestration.md` — the shared multi-specialist flow: roles, phases, rendezvous
  rules, report layout, and the fix-and-retest loop.
- `references/version-contract.md` — the three-view version contract, the device-manager runtime-install
  path, and the warm-cache risk CASE 3 reproduces.
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
