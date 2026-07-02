---
name: gpustack-operator-e2e
description: "Run a local end-to-end (E2E) verification of the GPUStack Operator on a reachable local cluster (k3s / docker-desktop): build & load the dev image, deploy via the Helm chart, then assert the NFD → Worker → Kueue scheduling chain materializes. Proactively offer this when a branch ahead of main changes controller reconcile, admission webhook, extension-apiserver, or in-cluster app-installation code. Examples: \"run the e2e test\", \"verify my reconcile change on a real cluster\", \"deploy the operator to my local k3s and check the Kueue objects\", \"does this drain change actually work end to end?\"."
allowed-tools: "Read, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/gpustack-operator-e2e/cases/case-*.sh*), Bash(kubectl get*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*)"
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

The work is split into shared phase scripts and numbered, self-contained cases:

- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG>`,
  `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`).
- `cases/case-N.sh <NS>` — one numbered scenario each, ending in a `STATUS | CHECK | OBJECT` table and
  exiting non-zero on any FAIL.
- `references/` — `drain-recycle.md` (per-case rationale + mock recipes) and shared
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
behave the same on a single-node local cluster and a multi-node real one.

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

Also warranting CASE 1 at minimum: changes under `pkg/worker/controllers/**`, `pkg/*/webhooks/**`,
`pkg/worker/extensionapis/**`, `api/**`, `pkg/extensionapi/**`, `pkg/worker/kuberess/**`.

## Flow

1. **Preflight (read-only).** Run it and confirm the context with the user:

   ```bash
   bash .claude/skills/_e2e-lib/scripts/preflight.sh
   ```

   Do not continue unless the user confirms a local `k3s` / `docker-desktop` context.

2. **Pick cases.** Match the changed surface against the table above with
   `git diff --name-only origin/main...HEAD`. If a high-impact surface changed, ask the user (with
   `AskUserQuestion`) which cases to run; CASE 1 is always included. If nothing matches, say so and run
   CASE 1 only on explicit request.

3. **Build & load (confirm).** Compute a per-commit tag so the kubelet runs the new image:

   ```bash
   TAG=dev-$(git rev-parse --short HEAD)
   bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG"
   ```

4. **Deploy (confirm).** A fresh install:

   ```bash
   NS=gpustack-system
   bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG"
   ```

   To redeploy over an existing release with a new image (avoids a full reinstall):

   ```bash
   helm upgrade gpustack-operator deploy/gpustack-operator/chart -n "$NS" \
     --reuse-values --set image.tag="$TAG" --set image.pullPolicy=IfNotPresent
   kubectl -n "$NS" rollout restart deploy/gpustack-operator-worker
   kubectl -n "$NS" rollout status  deploy/gpustack-operator-worker --timeout=300s
   ```

5. **Run the selected cases.** CASE 1 is read-only (no prompt); CASE 2–6 mutate and self-recover, so
   confirm each before running. Each prints a PASS/FAIL table and exits non-zero on failure — read the
   table, do not re-derive from raw output.

   ```bash
   NS=gpustack-system
   bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh "$NS"   # mandatory, read-only
   # then, per the picked cases (each confirmed):
   bash .claude/skills/gpustack-operator-e2e/cases/case-2.sh "$NS"
   bash .claude/skills/gpustack-operator-e2e/cases/case-3.sh "$NS"
   bash .claude/skills/gpustack-operator-e2e/cases/case-4.sh "$NS"
   bash .claude/skills/gpustack-operator-e2e/cases/case-5.sh "$NS"
   bash .claude/skills/gpustack-operator-e2e/cases/case-6.sh "$NS"
   bash .claude/skills/gpustack-operator-e2e/cases/case-7.sh "$NS"
   bash .claude/skills/gpustack-operator-e2e/cases/case-8.sh "$NS"   # auto-skips without real accelerators
   bash .claude/skills/gpustack-operator-e2e/cases/case-9.sh "$NS"
   ```

   On a FAIL, diagnose the named stage (`kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=200`,
   `kubectl -n "$NS" describe deploy/gpustack-operator-worker`) and see the references. A useful sweep
   for reconcile errors: `kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=400 | grep -iE 'error|reconcile.*fail'`.

6. **Teardown (ask first).** Ask the user (with `AskUserQuestion`) whether to clean up or keep the
   deployment. To clean up (confirm):

   ```bash
   bash .claude/skills/_e2e-lib/scripts/teardown.sh gpustack-system
   ```

   `teardown.sh` removes the test artifacts, the operator release, the worker-installed
   Kueue/NFD/CSI sub-releases, their CRDs/finalizers (including the operator's
   `gpustack.ai/controlled` on **Instances and InstanceTypes**), and the runtime APIServices/webhooks.
   The `gpustack-system` namespace is kept on purpose. Never delete the user's pre-existing resources.

## References

- `references/drain-recycle.md` — why CASE 2–6 need a real cluster (the fake-client blind spots), the
  managed-toggle code path, and the accelerated mock recipes (fake accelerator NodeFeature + the
  phantom-node `Devices` ledger, patched on the **v1alpha1** CRD).
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
