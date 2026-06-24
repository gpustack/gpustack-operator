---
name: gpustack-operator-e2e
description: "Run a local end-to-end (E2E) verification of the GPUStack Operator on a reachable local cluster (k3s / docker-desktop): build & load the dev image, deploy via the Helm chart, then assert the NFD → Worker → Kueue scheduling chain materializes. Proactively offer this when a branch ahead of main changes controller reconcile, admission webhook, extension-apiserver, or in-cluster app-installation code. Examples: \"run the e2e test\", \"verify my reconcile change on a real cluster\", \"deploy the operator to my local k3s and check the Kueue objects\", \"does this drain change actually work end to end?\"."
allowed-tools: "Read, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh*), Bash(kubectl get*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*)"
model: sonnet
---

# GPUStack Operator — local E2E verification

Deploy the operator onto a **local** cluster and verify the four-stage scheduling chain end to end:
NFD labels nodes → Device Manager detects accelerators → Worker profiles capacity → four controllers
materialize Kueue `ResourceFlavor` → `ClusterQueue` → `Cohort` / `LocalQueue`. See
[architecture.md](../../../docs/architecture.md) for the chain. **CASE 5** additionally exercises the
**sliced accelerator** borrow-and-reclaim path (`partitions=8` → 1/8 admits, 0.125 credit) on a GPU-less
cluster by injecting feature labels and mocking the device-plugin capacity.

This skill **mutates a cluster**. Hard rules:

- **Never switch kube context.** Show the active context (via `preflight.sh`) and proceed only after
  the user confirms it is the intended local cluster. **Stop and ask** if a different context is
  needed — never run `kubectl config use-context`.
- Build the image **locally only** — never push (`build-load.sh` keeps `PACKAGE_PUSH=false`).
- Touch only objects this skill creates (the Helm release, injected labels, test workloads). **Never**
  modify or delete the user's pre-existing namespaces/resources.
- Every **mutating** step (`build-load.sh`, `deploy.sh`, the mutating cases, `teardown.sh`) is
  confirmed before running. Read-only steps (`preflight.sh`, `assert-core.sh`, CASE 1) run without
  prompting.

The work is split into shared phase scripts and numbered, self-contained cases:

- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG>`,
  `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`).
- `cases/case-N.sh <NS>` — one numbered scenario each, ending in a `STATUS | CHECK | OBJECT` table and
  exiting non-zero on any FAIL.
- `references/` — `drain-recycle.md` (CASE 2/3/4/5 rationale) and shared `../_e2e-lib/references/troubleshooting.md`.

## Cases (locked titles)

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Script | Mutates |
|---|---|---|---|---|
| 1 | CPU-only scheduling chain materializes | always (mandatory) | `cases/case-1.sh` | no |
| 2 | Drain stops a running Instance (not recreate) | `pkg/worker/controllers/worker/instance.go`, `pkg/worker/webhooks/worker/instance.go` | `cases/case-2.sh` | yes (confirm) |
| 3 | Managed-toggle is an independent drain trigger | `pkg/worker/controllers/worker/{resourceflavor,cohort}.go` | `cases/case-3.sh` | yes (confirm) |
| 4 | Accelerated chain & drain-recycle (approx.) | accelerated / drain paths (optional) | `cases/case-4.sh` | yes (confirm) |
| 5 | Sliced accelerator: partitions=8 → 1/8 admits, 0.125 credit | `pkg/worker/controllers/worker/{clusterqueue,node,nodefeature}.go`, `pkg/worker/webhooks/worker/{nodefeature,instance}.go`, `pkg/worker/kuberess/apps_kueue.go`, `pkg/worker/extensionapis/worker/instance_type.go`, `pkg/nodefeature/{knowns,helper}.go`, `pkg/devicemanager/**`, `pkg/deviceplugin/**` | `cases/case-5.sh` | yes (confirm) |

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

4. **Deploy (confirm).**

   ```bash
   NS=gpustack-system
   bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG"
   ```

5. **Run the selected cases.** CASE 1 is read-only (no prompt); CASE 2/3/4/5 mutate and self-recover, so
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
   ```

   On a FAIL, diagnose the named stage (`kubectl -n "$NS" logs deploy/gpustack-operator-worker --tail=200`,
   `kubectl -n "$NS" describe deploy/gpustack-operator-worker`) and see the references.

6. **Teardown (ask first).** Ask the user (with `AskUserQuestion`) whether to clean up or keep the
   deployment. To clean up (confirm):

   ```bash
   bash .claude/skills/_e2e-lib/scripts/teardown.sh gpustack-system
   ```

   `teardown.sh` removes the test artifacts, the operator release, the worker-installed
   Kueue/NFD/CSI sub-releases, their CRDs/finalizers, and the runtime APIServices/webhooks. The
   `gpustack-system` namespace is kept on purpose. Never delete the user's pre-existing resources.

## References

- `references/drain-recycle.md` — why CASE 2/3/4/5 need a real cluster, the unit-test blind spot, the
  managed-toggle code path, and the injection recipes (incl. the sliced CASE 5 mock recipe).
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
