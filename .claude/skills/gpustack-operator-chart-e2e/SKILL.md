---
name: gpustack-operator-chart-e2e
description: "Verify the GPUStack Operator **Helm chart** end to end on a reachable local cluster (k3s / docker-desktop): build & load the dev image, `helm install` the chart, assert the worker/device-manager roll out, the versions are consistent (image tag ↔ bundled chart tgz ↔ `gpustack-operator --version`), then `helm uninstall` and assert zero leftovers. Proactively offer this when a branch ahead of main changes the chart, the in-cluster app-installation code, or the image build. Examples: \"verify the chart installs and uninstalls cleanly\", \"does helm install of the operator work\", \"check the chart version is right\", \"test my chart change end to end\", \"did my kuberess change break the runtime install\"."
allowed-tools: "Read, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/gpustack-operator-chart-e2e/cases/case-1.sh*), Bash(kubectl get*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(helm status*), Bash(helm list*), Bash(helm template*), Bash(helm lint*), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*)"
model: sonnet
---

# GPUStack Operator — Helm chart E2E verification

Install the operator **from its Helm chart** onto a **local** cluster and verify it installs, runs, and
uninstalls cleanly — with the **right version** baked in. This is the chart/version counterpart to the
`gpustack-operator-e2e` skill (which exercises the deep NFD → Worker → Kueue scheduling-chain behavior);
keep the deep behavioral assertions there and the install/uninstall + version contract here.

This skill **mutates a cluster**. Hard rules:

- **Never switch kube context.** Show the active context (via `preflight.sh`) and proceed only after the
  user confirms it is the intended local cluster. Never run `kubectl config use-context`.
- Build the image **locally only** — never push (`build-load.sh` keeps `PACKAGE_PUSH=false`).
- Touch only objects this skill creates (the Helm release, its cleanup). **Never** modify or delete the
  user's pre-existing namespaces/resources.
- Every **mutating** step (`build-load.sh`, `deploy.sh`, CASE 2, CASE 3) is confirmed before running.
  Read-only steps (`preflight.sh`, CASE 1) run without prompting.

The work is split into shared phase scripts and numbered, self-contained cases:

- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG> [--set …]`,
  `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`).
- `cases/case-N.sh` — one numbered scenario each, ending in a `STATUS | CHECK | OBJECT` table and
  exiting non-zero on any FAIL.
- `references/version-contract.md` and shared `../_e2e-lib/references/troubleshooting.md`.

## Cases (locked titles)

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Script | Mutates |
|---|---|---|---|---|
| 1 | Install + version consistency | always (mandatory) | `cases/case-1.sh` | no |
| 2 | Uninstall leaves zero leftovers | `deploy/gpustack-operator/chart/**`, `pkg/worker/kuberess/**` | `cases/case-2.sh` | yes (confirm) |
| 3 | Release version survives a warm build cache | `pack/gpustack-operator/Dockerfile`, `hack/package.sh` (optional) | `cases/case-3.sh` | yes (confirm) |

## Flow

1. **Preflight (read-only).** Run it and confirm the context; optionally `helm lint deploy/gpustack-operator/chart`.

   ```bash
   bash .claude/skills/_e2e-lib/scripts/preflight.sh
   ```

   Do not continue unless the user confirms a local `k3s` / `docker-desktop` context.

2. **Pick cases.** Match the changed surface against the table with `git diff --name-only origin/main...HEAD`.
   If a high-impact surface changed, ask the user (with `AskUserQuestion`) which cases to run; CASE 1 is
   always included. If nothing matches, say so and run CASE 1 only on explicit request.

3. **Build & load (confirm).**

   ```bash
   TAG=dev-$(git rev-parse --short HEAD)
   bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG"
   ```

4. **Deploy (confirm).** To also exercise the **device-manager runtime install** (the version-critical
   path — see `references/version-contract.md`), add `--set deviceManager.enabled=false`. To exercise the
   gated post-delete hook in-cluster, add `--set cleanupOnUninstall=true`.

   ```bash
   NS=gpustack-system
   bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG"
   # version-critical variant:
   # bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG" --set deviceManager.enabled=false
   ```

5. **Run CASE 1 (read-only, mandatory).** Pass the built `$TAG` so it also checks the deployed image tag.
   Reads the PASS/FAIL table; do not re-derive from raw output.

   ```bash
   bash .claude/skills/gpustack-operator-chart-e2e/cases/case-1.sh gpustack-system "$TAG"
   ```

6. **Optional CASE 3 (confirm).** Reproduce the version/cache path against a forced release version.
   Run after the §3 build so the cache is warm.

   ```bash
   bash .claude/skills/gpustack-operator-chart-e2e/cases/case-3.sh
   ```

7. **CASE 2 — uninstall & assert zero leftovers (confirm, final step).** This deletes the deployment.

   ```bash
   bash .claude/skills/gpustack-operator-chart-e2e/cases/case-2.sh gpustack-system
   ```

   CASE 2 runs the shared `teardown.sh` (operator release + worker-installed Kueue/NFD/CSI sub-releases,
   their CRDs/finalizers, runtime APIServices/webhooks) then asserts the cluster is clean. The
   `gpustack-system` namespace is kept on purpose. Never delete the user's pre-existing resources.

## References

- `references/version-contract.md` — the three-view version contract, the device-manager runtime-install
  path, and the warm-cache risk CASE 3 reproduces.
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
