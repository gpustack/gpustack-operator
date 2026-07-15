---
name: gpustack-operator-e2e
description: "Run a local end-to-end (E2E) verification of the GPUStack Operator on a reachable local cluster (k3s / docker-desktop): build & load the dev image, deploy via the Helm chart, then assert the NFD → Worker → Kueue scheduling chain materializes. Proactively offer this when a branch ahead of main changes controller reconcile, admission webhook, extension-apiserver, or in-cluster app-installation code. Examples: \"run the e2e test\", \"verify my reconcile change on a real cluster\", \"deploy the operator to my local k3s and check the Kueue objects\", \"does this drain change actually work end to end?\"."
allowed-tools: "Read, Agent, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh*), Bash(kubectl get*), Bash(kubectl describe*), Bash(kubectl logs*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*)"
model: sonnet
---

# GPUStack Operator — local E2E verification

Deploy the operator to a **local** cluster and verify the scheduling chain end to end:

```
NFD labels nodes → DeviceManager detects accelerators → Worker profiles capacity
  → NodeFlavor/InstanceType reconcilers → ResourceFlavor
  → InstanceType (real CRD; .status = three-view)
  → ClusterQueue (exactly one isolated CQ per pool — NO Cohort) → LocalQueue
  ⊢ node-devices AdmissionCheck gates per-card feasibility
```

- Chain detail → [architecture.md](../../../docs/architecture.md); the unified-pool refactor this suite tracks → `specs/2026-06-29-instancetype-unified-pool-refactor.md`.
- Accelerated cases run **GPU-less by approximation**: mock a fake accelerator NodeFeature + a per-card `Devices` ledger. The derivation and the three-view/AdmissionCheck math are **NOT** mocked — that is the verification.

## Orchestration

Run as a **test-orchestration lead** (main agent) coordinating read-only **domain specialists** (`Agent` tool). **Read [`orchestration.md`](../_e2e-lib/references/orchestration.md) before Phase 0** — it owns roles, phases, parallelism/rendezvous rules, report layout, and the fix-and-retest loop. Below are the operator-e2e specifics.

**Hard rules** (full list in `orchestration.md`; operator-e2e specifics):
- **Never switch kube context** — confirm the active context is the intended local cluster; never `kubectl config use-context`.
- **Build locally, never push** — `build-load.sh` keeps `PACKAGE_PUSH=false`.
- **Touch only what this run creates** — the Helm release, injected labels/NodeFeatures, mocked `Devices`, test workloads. Never touch the user's pre-existing resources.
- **Confirm every mutating step** (`build-load.sh`, `deploy.sh`, mutating cases, `teardown.sh`); read-only steps (`preflight.sh`, `assert-core.sh`, CASE 1) run unprompted.
- **Specialists are read-only** — the lead is the sole writer.

**Layout:**
- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG>`, `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`).
- `cases/case-N.sh <NS>` — one scenario each; ends in a `STATUS | CHECK | OBJECT` table, exits non-zero on any FAIL.
- `references/` — `drain-recycle.md` (per-case rationale + mock recipes), `packaged-image-deploy.md` (image-ref ↔ chart-values contract); shared `../_e2e-lib/references/{orchestration,troubleshooting}.md`.

## Cases (locked titles)

Each case is self-contained; its header (see **Case header contract**) states goal / environment (incl. auto-skip) / inputs (MOCKED vs real) / expected / cleanup. `Needs` = the hardware/tooling the case requires and its auto-skip condition.

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Script | Mutates | Needs |
|---|---|---|---|---|---|
| 1 | CPU-only scheduling chain materializes — zero Cohort, InstanceType Active | always (mandatory) | `cases/case-1.sh` | no | any |
| 2 | Running Instance admits, then drain stops it (not recreate) | `pkg/worker/controllers/worker/instance.go`, `pkg/worker/webhooks/worker/instance.go`, `pkg/worker/kuberess/apps_kueue.go` | `cases/case-2.sh` | yes (confirm) | any |
| 3 | Managed-toggle scopes node onboarding | `pkg/worker/controllers/worker/{node_flavor,instance_type}.go`, `pkg/nodefeature/helper.go` | `cases/case-3.sh` | yes (confirm) | any |
| 4 | AdmissionCheck holds exclusive over-admit | `pkg/worker/controllers/worker/{node_devices_admission,node_devices,instance_type}.go`, `pkg/worker/kuberess/apps_kueue.go` | `cases/case-4.sh` | yes (confirm) | any |
| 5 | Pod webhook folds slice-by-memory-% into units | `pkg/worker/webhooks/worker/pod.go`, `pkg/nodefeature/knowns.go` | `cases/case-5.sh` | yes (confirm) | any |
| 6 | Pooled three-view + watch freshness | `pkg/worker/controllers/worker/instance_type.go`, `pkg/worker/webhooks/worker/instance_type.go`, `api/worker/v1alpha1/{instance_type,devices}.go` | `cases/case-6.sh` | yes (confirm) | any |
| 7 | Portless Instance reaches Ready, creates no Service | `pkg/worker/controllers/worker/instance.go` | `cases/case-7.sh` | yes (confirm) | any |
| 8 | Real accelerator slicing runtime isolation | `pkg/deviceplugin/**`, `pkg/devicemanager/**`, `pkg/worker/webhooks/worker/pod.go` | `cases/case-8.sh` | yes (confirm) | real GPU · skips: no `*.sliced` |
| 9 | Instance lifecycle survives an InstanceType unit-spec change | `pkg/worker/webhooks/worker/instance.go` | `cases/case-9.sh` | yes (confirm) | any |
| 10 | Start re-validates a resized-while-stopped Instance (no create-check bypass) | `pkg/worker/webhooks/worker/instance.go` | `cases/case-10.sh` | yes (confirm) | any |
| 11 | Sliced per-card accounting: three-view + per-card OnceMax (spread observed best-effort) | `pkg/deviceplugin/server.go`, `pkg/worker/controllers/worker/{node_devices_admission,instance_type}.go` | `cases/case-11.sh` | yes (confirm) | any |
| 12 | Sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1 | `pkg/worker/webhooks/worker/instance.go`, `pkg/utils/quantityx/quantity.go` | `cases/case-12.sh` | yes (confirm) | any |
| 13 | SSH-enabled sliced Instance: slice visible over SSH + confined shell | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh`, `pkg/deviceplugin/**`, `pkg/devicemanager/allocator/**` | `cases/case-13.sh` | yes (confirm) | real GPU + ssh · skips: no `*.sliced` / no ssh |
| 14 | Multiple slices coexist on one physical card within budget | `pkg/worker/controllers/worker/{node_devices_admission,instance_type}.go`, `pkg/deviceplugin/**` | `cases/case-14.sh` | yes (confirm) | real GPU · skips: no `*.sliced` |
| 15 | Exclusive whole-card SSH Instance still works (regression) | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh` | `cases/case-15.sh` | yes (confirm) | real GPU + ssh · skips: no `*.sliced` / no ssh |
| 16 | InstanceTypeFlavor catalog + declarative queue ownership (recreate-on-delete, delete-then-wait teardown) | `pkg/worker/controllers/worker/{instance_type,node_queue,node_flavor}.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/settings/value.go` | `cases/case-16.sh` | yes (confirm) | any |
| 17 | InstanceType declarative admission (require + freeze inputs; Default stamps schedule + entrance labels) | `pkg/worker/webhooks/worker/instance_type.go`, `api/worker/v1alpha1/instance_type.go` | `cases/case-17.sh` | yes (confirm) | any |
| 18 | CPU-manufacturer awareness reshapes the catalog (finest RF + cpuDetail; collapse↔split by setting) | `pkg/nodefeature/helper.go`, `pkg/worker/settings/value.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/webhooks/worker/instance_type.go`, `pkg/worker/controllers/worker/node_flavor.go` | `cases/case-18.sh` | yes (confirm) | any |
| 19 | Awareness on: accelerated type carries real GPU + folded CPU descriptors; a real GPU Instance runs on it | `pkg/worker/controllers/worker/{node_flavor,instance_type}.go`, `pkg/worker/webhooks/worker/instance_type.go`, `pkg/nodefeature/helper.go` | `cases/case-19.sh` | yes (confirm) | real GPU · skips: GPU-less |
| 20 | Sibling InstanceTypes on one pool stay status-consistent (Devices-watch re-enqueues all) | `pkg/worker/controllers/worker/instance_type.go` | `cases/case-20.sh` | yes (confirm) | real GPU · skips: GPU-less |
| 21 | SSH Instance serves non-interactive SSH (exec channel) + interactive login unchanged | `pack/ssh-server/rootfs/chroot.sh`, `pack/ssh-server/Dockerfile`, `pkg/worker/settings/value.go` | `cases/case-21.sh` | yes (confirm) | ssh client · skips: no ssh/ssh-keygen/sftp |

- Also run **CASE 1 at minimum** for changes under `pkg/worker/controllers/**`, `pkg/*/webhooks/**`, `pkg/worker/extensionapis/**`, `api/**`, `pkg/extensionapi/**`, `pkg/worker/kuberess/**`.
- `spec.os`/`spec.arch` materialization is asserted **inline** — CASE 1 (cpu pool) + CASE 6 (accelerated) — not as a standalone case.
- CASE 3 under the queue-ownership split: draining a pool no longer deletes the InstanceType — the flavor is deleted but the **type survives with its queue emptied**, and reactivates when nodes return.
- CASE 4's mock uses a fake product key (`nvidia-e2emock`) that never collides with a real GPU pool → safe on a real-accelerator cluster too.

## Case header contract

Every `cases/case-N.sh` opens with a header describing the case **on its own terms — no spec anchors** (no Story/Task numbers, F-codes, `specs/*.md` paths, or commit hashes). A reader must understand what the case does, needs, and asserts from the header alone. New/edited cases follow this six-field template:

```
# CASE N — <one-line behavior title>   (<mutation posture>)
#
#   case-N.sh <NS>
#
# Goal:        <the one contract/behavior this case proves>
# Environment: <what the cluster must provide + when it AUTO-SKIPS>
# Inputs:      <what the case creates / injects / toggles; mark MOCKED inputs vs the real thing verified>
# Expected:    <the PASS assertions — the observable final state>
# Cleanup:     <what the trap restores — INCLUDING any environment baseline the case changed
#               (editable settings, managed toggles, node/NodeFeature labels); idempotent, runs on
#               pass AND fail, safe to re-run>
```

- **Mutation posture** rides the title line: `READ-ONLY` (no trap) or `MUTATING, self-recovering` (writes, then a trap restores). Append `AUTO-SKIPS without <requirement>` when the case self-skips (real hardware, `ssh` client, ≥2 sliced cards, …).
- **Environment** always names the auto-skip condition so the run/skip decision is readable without executing the case.
- **Inputs** distinguish **MOCKED** inputs (fake accelerator NodeFeature, phantom-node `Devices` ledger) from the **real** objects under test — the mock is a fixture, never the verification.
- Describe behavior in **plain terms**, not spec reference; the case↔code mapping lives in the table's "Run when these change" column.
- **Restore the baseline, pass or fail.** A case mutating shared baseline (an editable Setting, a `gpustack.ai/managed` toggle, an injected node/NodeFeature label, a patched unit spec) MUST restore it in `trap … EXIT` (runs on pass AND fail) and wait for the chain to settle back. Between cases the lead re-confirms the baseline (awareness off, all nodes managed, no leftover test objects) and restores it if a case died before its trap ran — a mutated baseline is a frequent cause of a *spurious* failure in the NEXT case.
- Runtime output (results banner, `record` messages, FAIL-footer) stays spec-anchor-free and self-explanatory.

## Flow

Generic phase semantics (roles, rendezvous, report, fix-retest) live in [`orchestration.md`](../_e2e-lib/references/orchestration.md); below are the operator-e2e specifics. Let `RPT=.claude/reports/$(date +%F)-operator-e2e`.

**Phase 0 — Environment (read-only).** Preflight, confirm context with the user, record drift:

```bash
bash .claude/skills/_e2e-lib/scripts/preflight.sh
git rev-parse HEAD; git diff --stat origin/main...HEAD
```

Do not continue unless the user confirms a local `k3s` / `docker-desktop` context. Then `mkdir -p "$RPT"/raw` and write the report header + node inventory.

**Phase 1 — Test-item list.** Match `git diff --name-only origin/main...HEAD` against the case table; also cross-check `specs/2026-06-29-instancetype-unified-pool-refactor.md` (its `### User Stories`) for items **beyond** the table. Confirm with the user (`AskUserQuestion`); CASE 1 always included. No drift → ask for new items, invoking `interview-me` if the ask is too coarse.

**Phase 2 — Plan.** Split into the serial mutating sequence + parallel read-only analyses; pick the specialist roster — logic → `agent-skills:test-engineer`; over-admit/AdmissionCheck security → `agent-skills:security-auditor`; real-slice runtime/perf → `general-purpose`; root-cause → `agent-skills:code-reviewer`. Write the test-plan section.

**Phase 3 — Build & deploy (confirm).** Per-commit tag so the kubelet runs the new image:

```bash
TAG=dev-$(git rev-parse --short HEAD); NS=gpustack-system
bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG"   2>&1 | tee "$RPT"/raw/01-build.txt
bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG" 2>&1 | tee "$RPT"/raw/02-deploy.txt
bash .claude/skills/_e2e-lib/scripts/assert-core.sh "$NS"   2>&1 | tee "$RPT"/raw/03-assert-core.txt
```

Redeploy over an existing release with a new image (avoids a full reinstall):

```bash
helm upgrade gpustack-operator deploy/gpustack-operator/chart -n "$NS" \
  --reuse-values --set image.tag="$TAG" --set image.pullPolicy=IfNotPresent
kubectl -n "$NS" rollout restart deploy/gpustack-operator-worker
kubectl -n "$NS" rollout status  deploy/gpustack-operator-worker --timeout=300s
```

`assert-core.sh` requires the **running binary revision == HEAD** (stale-image guard).

**Phase 4 — Execute + analyze.** CASE 1 read-only (no prompt); CASE 2+ mutate and self-recover, so confirm each. Per the rendezvous rule: run a case → capture to `raw/NN-caseN.txt` → let it converge → **then** fan out that case's specialists (one message, multiple `Agent` calls) on the snapshot → collect verdicts → write the execution record → only then the next mutating case.

```bash
bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh "$NS" 2>&1 | tee "$RPT"/raw/10-case1.txt   # mandatory, read-only
# then, per the picked cases (each confirmed), e.g.:
bash .claude/skills/gpustack-operator-e2e/cases/case-2.sh "$NS" 2>&1 | tee "$RPT"/raw/11-case2.txt
# … case-3 … case-9 (case-8 auto-skips without real accelerators)
```

Read the PASS/FAIL table; do not re-derive from raw output.

**Phase 5 — Triage.** On a FAIL/suspicion, diagnose the named stage and record a `Finding #N` block:

```bash
kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=200
kubectl -n "$NS" describe deploy/gpustack-operator-worker
kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=400 | grep -iE 'error|reconcile.*fail'
```

**Phase 6 — Summary.** Write the summary (confirmed problems + repro + conclusion; classify each as real bug / test-design issue / awaiting-owner) + the **suite gaps & upgrade backlog** accrued during the run.

**Phase 7 — Fix & retest / Suite upgrade (optional).** Per `orchestration.md`, two user-gated branches: (a) real bugs → confirm, choose local vs remote (SSH/RSYNC) packaging (record connection info), fix → `make lint` → signoff commit → package → `helm upgrade` → retest → append; (b) sink the self-upgrade backlog into the suite (new/adjusted `cases`, tightened assertions, case-table/`references` updates) or this protocol — confirm each, signoff commit, retest any changed case.

**Phase 8 — Teardown (mandatory).** Run unconditionally as the final phase (confirm the single call); defer only if a Phase 7 loop is still running:

```bash
bash .claude/skills/_e2e-lib/scripts/teardown.sh gpustack-system 2>&1 | tee "$RPT"/raw/90-teardown.txt
```

`teardown.sh` removes the test artifacts, the operator release, the worker-installed Kueue/NFD/CSI sub-releases, their CRDs/finalizers (including the operator's `gpustack.ai/controlled` on **Instances and InstanceTypes**), and the runtime APIServices/webhooks. The `gpustack-system` namespace is kept on purpose. Never delete the user's pre-existing resources.

## References

- `../_e2e-lib/references/orchestration.md` — the shared multi-specialist flow: roles, phases, rendezvous rules, report layout, and the fix-and-retest loop.
- `references/drain-recycle.md` — why CASE 2–6 need a real cluster (the fake-client blind spots), the managed-toggle code path, and the accelerated mock recipes (fake accelerator NodeFeature + the phantom-node `Devices` ledger, patched on the **v1alpha1** CRD).
- `references/manual-ssh-verification.md` — the manual pass CASE 21 cannot drive in CI: a real VS Code Remote-SSH session (workspace opens, integrated terminal in `main`) and an `sshfs` mount round-trip.
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
