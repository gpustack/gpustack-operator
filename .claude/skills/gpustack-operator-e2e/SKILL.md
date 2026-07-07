---
name: gpustack-operator-e2e
description: "Run a local end-to-end (E2E) verification of the GPUStack Operator on a reachable local cluster (k3s / docker-desktop): build & load the dev image, deploy via the Helm chart, then assert the NFD → Worker → Kueue scheduling chain materializes. Proactively offer this when a branch ahead of main changes controller reconcile, admission webhook, extension-apiserver, or in-cluster app-installation code. Examples: \"run the e2e test\", \"verify my reconcile change on a real cluster\", \"deploy the operator to my local k3s and check the Kueue objects\", \"does this drain change actually work end to end?\"."
allowed-tools: "Read, Agent, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh*), Bash(kubectl get*), Bash(kubectl describe*), Bash(kubectl logs*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*)"
model: sonnet
---

# GPUStack Operator — local E2E verification

Deploy the operator onto a **local** cluster and verify the scheduling chain end to end:
NFD labels nodes → Device Manager detects accelerators → Worker profiles capacity → the
`NodeFlavor`/`InstanceType` reconcilers materialize Kueue `ResourceFlavor` → **`InstanceType`**
(a real CRD whose `.status` carries the three-view) → its backing **`ClusterQueue`** (exactly
**one isolated CQ per pool — no Cohort**) → `LocalQueue`; a node-devices **`AdmissionCheck`**
gates per-card feasibility. See [architecture.md](../../../docs/architecture.md) for the chain and
`specs/2026-06-29-instancetype-unified-pool-refactor.md` for the unified-pool refactor this suite
tracks. The accelerated cases run on a **GPU-less** cluster **by approximation** (fake accelerator
NodeFeature + a mocked per-card `Devices` ledger).

## Orchestration

Run this as a **test-orchestration lead** (you, the main agent) coordinating dynamically-chosen
**domain specialists** (read-only subagents via the `Agent` tool). You are the sole cluster writer and
drive every mutating step serially; specialists analyze in parallel and report back to you; the whole run
is captured to a durable report under `.claude/reports/`. **Read
[`orchestration.md`](../_e2e-lib/references/orchestration.md) before Phase 0** — it owns the roles, the
phase-by-phase flow, the parallelism/rendezvous rules, the report layout, and the optional fix-and-retest
loop. The phases below are the operator-e2e specifics that plug into it.

This skill **mutates a cluster**. Hard rules:

- **Never switch kube context.** Show the active context (via `preflight.sh`) and proceed only after
  the user confirms it is the intended local cluster. **Stop and ask** if a different context is
  needed — never run `kubectl config use-context`.
- Build the image **locally only** — never push (`build-load.sh` keeps `PACKAGE_PUSH=false`).
- Touch only objects this skill creates (the Helm release, injected labels/NodeFeatures, mocked
  `Devices`, test workloads). **Never** modify or delete the user's pre-existing namespaces/resources.
- Every **mutating** step (`build-load.sh`, `deploy.sh`, the mutating cases, `teardown.sh`) is
  confirmed before running. Read-only steps (`preflight.sh`, `assert-core.sh`, CASE 1) run without
  prompting.
- **Specialists are read-only.** You are the only writer — fan them out for analysis, never for mutation.

The work is split into shared phase scripts and numbered, self-contained cases:

- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG>`,
  `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`).
- `cases/case-N.sh <NS>` — one numbered scenario each, ending in a `STATUS | CHECK | OBJECT` table and
  exiting non-zero on any FAIL.
- `references/` — `drain-recycle.md` (per-case rationale + mock recipes), shared
  `../_e2e-lib/references/orchestration.md` (the multi-specialist flow), and shared
  `../_e2e-lib/references/troubleshooting.md`.

## Cases (locked titles)

Each case maps to a User Story of the unified-pool refactor. On a GPU-less cluster the accelerated
cases mock two inputs the DeviceManager would produce (a fake accelerator NodeFeature and a
phantom-node `Devices` ledger); the derivation and the three-view/AdmissionCheck math are **not**
mocked — that is the verification. CASE 4's mock uses a fake product key (`nvidia-e2emock`) that
never collides with a real GPU's pool, so it is safe on a real-accelerator cluster too, not only
GPU-less. CASE 7 exercises the general (CPU) pool and runs on any cluster. CASE 8 needs **real**
accelerator hardware (the HAMI runtime cap cannot be mocked) and **auto-skips** with a message when
no `*.sliced` resource is advertised. CASE 2/3 drain **every** node feeding the general pool, so they
behave the same on a single-node local cluster and a multi-node real one. CASE 12 mocks the sliceable
accelerated pool like CASE 6 and asserts only the Instance admission result (persisted spec.resources
/ rejection), so it needs no real hardware. The `spec.os`/`spec.arch` materialization from the backing
ClusterQueue labels is asserted inline — in CASE 1 for the cpu-only pool and CASE 6 for the accelerated
one — not as a standalone case. CASE 13, like CASE 8, needs **real** accelerator hardware and
**auto-skips** when no `*.sliced` resource is advertised (also skips when the runner has no `ssh`
client); it renders a real SSH-enabled sliced Instance and asserts, through a real SSH login, that the
slice is visible in both `main` and the SSH shell and that the shell is capability-stripped (host
`mknod` denied). It uses a 60% slice (Story 1's canonical ~9830 MiB of a 16Gi T4); a > 50% single-card
slice also exercises the node-devices AdmissionCheck fix that stops it from counting a Workload's own
allocation against itself (which previously self-evicted such slices in a recreate loop). CASE 14
(Story 6) and CASE 15 (exclusive/whole-card SSH regression) likewise need **real** accelerator hardware
and **auto-skip** when none is advertised (CASE 15 also needs an `ssh` client); CASE 14 asserts two
in-budget slices coexist on one card while an over-budget third is held, and CASE 15 asserts the
non-sliced SSH path still sees the whole card through a capability-stripped shell. CASE 16 and CASE 17
cover the queue-ownership split (declarative InstanceType management) on the general (CPU) pool, so
they run on any cluster: CASE 16 asserts the aggregated InstanceTypeFlavor catalog, self-heal on an
accidental ClusterQueue delete, and delete-then-wait teardown of an admin type; CASE 17 asserts the
admission contract (required inputs; unit spec frozen on a REAL update — server dry-run does not
reliably exercise update admission for a merge patch; Default stamps the schedule + entrance labels).
Under this split CASE 3's observable changed: draining a pool no longer deletes the InstanceType — the
flavor is deleted but the type SURVIVES with its queue emptied, and it reactivates when nodes return.

| Case | Title (Story) | Run when these change (`git diff --name-only origin/main...HEAD`) | Script | Mutates |
|---|---|---|---|---|
| 1 | CPU-only scheduling chain materializes — zero Cohort, InstanceType Active (Story 1/2 baseline) | always (mandatory) | `cases/case-1.sh` | no |
| 2 | Running Instance admits, then drain stops it (not recreate) | `pkg/worker/controllers/worker/instance.go`, `pkg/worker/webhooks/worker/instance.go`, `pkg/worker/kuberess/apps_kueue.go` | `cases/case-2.sh` | yes (confirm) |
| 3 | Managed-toggle scopes node onboarding (Story 5) | `pkg/worker/controllers/worker/{nodeflavor,instancetype}.go`, `pkg/nodefeature/helper.go` | `cases/case-3.sh` | yes (confirm) |
| 4 | AdmissionCheck holds exclusive over-admit (Story 4) | `pkg/worker/controllers/worker/{nodedevicesadmission,nodedevices,instancetype}.go`, `pkg/worker/kuberess/apps_kueue.go` | `cases/case-4.sh` | yes (confirm) |
| 5 | Pod webhook folds slice-by-memory-% into units (Story 3) | `pkg/worker/webhooks/worker/pod.go`, `pkg/nodefeature/knowns.go` | `cases/case-5.sh` | yes (confirm) |
| 6 | Pooled three-view + watch freshness (Story 2/6) | `pkg/worker/controllers/worker/instancetype.go`, `pkg/worker/webhooks/worker/instancetype.go`, `api/worker/v1alpha1/{instance_type,devices}.go` | `cases/case-6.sh` | yes (confirm) |
| 7 | Portless Instance reaches Ready, creates no Service | `pkg/worker/controllers/worker/instance.go` | `cases/case-7.sh` | yes (confirm) |
| 8 | Real accelerator slicing runtime isolation (Story 1) | `pkg/deviceplugin/**`, `pkg/devicemanager/**`, `pkg/worker/webhooks/worker/pod.go` | `cases/case-8.sh` | yes (confirm) |
| 9 | Instance lifecycle survives an InstanceType unit-spec change | `pkg/worker/webhooks/worker/instance.go` | `cases/case-9.sh` | yes (confirm) |
| 10 | Start re-validates a resized-while-stopped Instance (no create-check bypass) | `pkg/worker/webhooks/worker/instance.go` | `cases/case-10.sh` | yes (confirm) |
| 11 | Sliced per-card accounting: three-view + per-card OnceMax (spread observed best-effort) | `pkg/deviceplugin/server.go`, `pkg/worker/controllers/worker/{nodedevicesadmission,instancetype}.go` | `cases/case-11.sh` | yes (confirm) |
| 12 | Sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1 | `pkg/worker/webhooks/worker/instance.go`, `pkg/utils/quantityx/quantity.go` | `cases/case-12.sh` | yes (confirm) |
| 13 | SSH-enabled sliced Instance: slice visible over SSH + confined shell (Stories 1/4) | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh`, `pkg/deviceplugin/**`, `pkg/devicemanager/allocator/**` | `cases/case-13.sh` | yes (confirm) |
| 14 | Multiple slices coexist on one physical card within budget (Story 6) | `pkg/worker/controllers/worker/{nodedevicesadmission,instancetype}.go`, `pkg/deviceplugin/**` | `cases/case-14.sh` | yes (confirm) |
| 15 | Exclusive whole-card SSH Instance still works (regression) | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh` | `cases/case-15.sh` | yes (confirm) |
| 16 | InstanceTypeFlavor catalog + declarative queue ownership (recreate-on-delete, delete-then-wait teardown) | `pkg/worker/controllers/worker/{instancetype,nodequeue,nodeflavor}.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/settings/value.go` | `cases/case-16.sh` | yes (confirm) |
| 17 | InstanceType declarative admission (require + freeze inputs; Default stamps schedule + entrance labels) | `pkg/worker/webhooks/worker/instancetype.go`, `api/worker/v1alpha1/instance_type.go` | `cases/case-17.sh` | yes (confirm) |

Also warranting CASE 1 at minimum: changes under `pkg/worker/controllers/**`, `pkg/*/webhooks/**`,
`pkg/worker/extensionapis/**`, `api/**`, `pkg/extensionapi/**`, `pkg/worker/kuberess/**`.

## Flow

The lead drives these phases; the shared protocol
([`orchestration.md`](../_e2e-lib/references/orchestration.md)) owns the roles, rendezvous rules, report
layout, and fix-and-retest loop. Below are the operator-e2e specifics. Let `RPT=.claude/reports/$(date +%F)-operator-e2e`.

**Phase 0 — Environment (read-only).** Run preflight, confirm the context with the user, record drift:

```bash
bash .claude/skills/_e2e-lib/scripts/preflight.sh
git rev-parse HEAD; git diff --stat origin/main...HEAD
```

Do not continue unless the user confirms a local `k3s` / `docker-desktop` context. Then
`mkdir -p "$RPT"/raw` and write the report header + node inventory.

**Phase 1 — Test-item list.** Match the changed surface against the case table with
`git diff --name-only origin/main...HEAD`, and cross-check
`specs/2026-06-29-instancetype-unified-pool-refactor.md` (its `### User Stories`) against the diff for
items **beyond** the table. Confirm the list with the user (`AskUserQuestion`); CASE 1 is always included.
No drift → ask for new items, invoking the global `interview-me` skill if the ask is too coarse.

**Phase 2 — Plan.** Split into the serial mutating sequence and the parallel read-only analyses; pick the
specialist roster — logic/functional via `agent-skills:test-engineer`; over-admit/AdmissionCheck security
via `agent-skills:security-auditor`; real-slice runtime isolation/perf via `general-purpose`; root-cause
via `agent-skills:code-reviewer`. Write the test-plan section (assignments + rendezvous points).

**Phase 3 — Build & deploy (confirm).** Per-commit tag so the kubelet runs the new image:

```bash
TAG=dev-$(git rev-parse --short HEAD); NS=gpustack-system
bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG"   2>&1 | tee "$RPT"/raw/01-build.txt
bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG" 2>&1 | tee "$RPT"/raw/02-deploy.txt
bash .claude/skills/_e2e-lib/scripts/assert-core.sh "$NS"   2>&1 | tee "$RPT"/raw/03-assert-core.txt
```

To redeploy over an existing release with a new image (avoids a full reinstall):

```bash
helm upgrade gpustack-operator deploy/gpustack-operator/chart -n "$NS" \
  --reuse-values --set image.tag="$TAG" --set image.pullPolicy=IfNotPresent
kubectl -n "$NS" rollout restart deploy/gpustack-operator-worker
kubectl -n "$NS" rollout status  deploy/gpustack-operator-worker --timeout=300s
```

`assert-core.sh` requires the **running binary revision == HEAD** (stale-image guard).

**Phase 4 — Execute + analyze.** CASE 1 is read-only (no prompt); CASE 2–9 mutate and self-recover, so
confirm each. Per the rendezvous rule: run a case, capture to `raw/NN-caseN.txt`, let it converge — **then**
fan out that case's specialists (one message, multiple `Agent` calls) on the captured snapshot, collect
verdicts, write the execution record, and only then run the next mutating case.

```bash
bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh "$NS" 2>&1 | tee "$RPT"/raw/10-case1.txt   # mandatory, read-only
# then, per the picked cases (each confirmed), e.g.:
bash .claude/skills/gpustack-operator-e2e/cases/case-2.sh "$NS" 2>&1 | tee "$RPT"/raw/11-case2.txt
# … case-3 … case-9 (case-8 auto-skips without real accelerators)
```

Each case prints a PASS/FAIL table and exits non-zero on failure — read the table, do not re-derive from
raw output.

**Phase 5 — Triage.** On a FAIL/suspicion, diagnose the named stage and record a `Finding #N` block:

```bash
kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=200
kubectl -n "$NS" describe deploy/gpustack-operator-worker
kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=400 | grep -iE 'error|reconcile.*fail'
```

**Phase 6 — Summary.** Write the summary section (confirmed problems + repro + conclusion; classify each as
real bug / test-design issue / awaiting-owner) plus the **suite gaps & upgrade backlog** accrued during the run.

**Phase 7 — Fix & retest / Suite upgrade (optional).** Per `orchestration.md`, two user-gated branches:
(a) fixable real bugs → confirm, choose local vs remote (SSH/RSYNC) packaging (record connection info), then
fix → `make lint` → signoff commit → package → `helm upgrade` → retest → append results; (b) sink the
self-upgrade backlog into the suite (new/adjusted `cases`, tightened assertions, case-table/`references`
updates) or this protocol — confirm each, signoff commit, retest any changed case.

**Phase 8 — Teardown (mandatory).** Run unconditionally as the final phase (confirm the single call); defer
only if a Phase 7 loop is still running:

```bash
bash .claude/skills/_e2e-lib/scripts/teardown.sh gpustack-system 2>&1 | tee "$RPT"/raw/90-teardown.txt
```

`teardown.sh` removes the test artifacts, the operator release, the worker-installed Kueue/NFD/CSI
sub-releases, their CRDs/finalizers (including the operator's `gpustack.ai/controlled` on **Instances and
InstanceTypes**), and the runtime APIServices/webhooks. The `gpustack-system` namespace is kept on purpose.
Never delete the user's pre-existing resources.

## References

- `../_e2e-lib/references/orchestration.md` — the shared multi-specialist flow: roles, phases, rendezvous
  rules, report layout, and the fix-and-retest loop.
- `references/drain-recycle.md` — why CASE 2–6 need a real cluster (the fake-client blind spots), the
  managed-toggle code path, and the accelerated mock recipes (fake accelerator NodeFeature + the
  phantom-node `Devices` ledger, patched on the **v1alpha1** CRD).
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
