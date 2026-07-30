---
name: gpustack-operator-e2e
description: "Run an end-to-end (E2E) verification of the GPUStack Operator on a Kubernetes cluster — one the user already has (k3s / docker-desktop / any reachable context), or one provisioned for the run from testing/infra/clusters/ and destroyed afterwards: build & load the dev image, deploy via the Helm chart, then assert the NFD → Worker → Kueue scheduling chain materializes. Proactively offer this when a branch ahead of main changes controller reconcile, admission webhook, extension-apiserver, or in-cluster app-installation code. Examples: \"run the e2e test\", \"verify my reconcile change on a real cluster\", \"deploy the operator to my local k3s and check the Kueue objects\", \"does this drain change actually work end to end?\"."
allowed-tools: "Read, Agent, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/cluster-auth.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/kube-context.sh*), Bash(bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh*), Bash(kubectl get*), Bash(kubectl describe*), Bash(kubectl logs*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(kubectl config get-contexts*), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*)"
model: sonnet
---

# GPUStack Operator — local E2E verification

Deploy the operator to a Kubernetes cluster — the user's own by default, or one provisioned for the run and destroyed afterwards — and verify the scheduling chain end to end:

```
NFD labels nodes → DeviceManager detects accelerators → Worker profiles capacity
  → NodeFlavor/InstanceType reconcilers → ResourceFlavor
  → InstanceType (real CRD; .status = four-view EX/SH/SL/PT)
  → ClusterQueue (exactly one isolated CQ per pool — NO Cohort) → LocalQueue
  ⊢ node-devices AdmissionCheck gates per-card feasibility
```

- Chain detail → [architecture.md](../../../docs/architecture.md); the unified-pool refactor this suite tracks → `specs/2026-06-29-instancetype-unified-pool-refactor.md`.
- Accelerated cases run **GPU-less by approximation**: mock a fake accelerator NodeFeature + a per-card `Devices` ledger. The derivation and the four-view/AdmissionCheck math are **NOT** mocked — that is the verification.

## Orchestration

Run as a **test-orchestration lead** (main agent) coordinating read-only **domain specialists** (`Agent` tool). **Read [`orchestration.md`](../_e2e-lib/references/orchestration.md) before Phase 0** — it owns roles, phases, parallelism/rendezvous rules, report layout, and the fix-and-retest loop. Below are the operator-e2e specifics.

**Hard rules** (full list in `orchestration.md`; operator-e2e specifics):
- **Pin the run to a user-confirmed context** — confirm the active context is the intended cluster. Never switch on your own judgement. When the confirmed cluster is **not** the active context, do not switch: take its isolated kubeconfig with `bash ../_e2e-lib/scripts/kube-context.sh <ctx>` and prefix every command of the run with the `KUBECONFIG=<path>` it prints (details in `orchestration.md`). On the user's explicit say-so you may switch instead — then record the context to return to as an outstanding environment mutation and restore it in Phase 8. Adopting the context a cluster **this run provisioned** merged in itself needs no such record.
- **Prefer the user's own cluster; provisioning is a separate opt-in** and a cluster this run provisioned **must** be destroyed in Phase 8 — see [`cluster-provisioning.md`](../_e2e-lib/references/cluster-provisioning.md).
- **Offer a context checkpoint at the trigger points** (see Flow); compacting is the user's action, and the lead hands over a focus block that carries the teardown obligation.
- **Build locally when the nodes share this machine's image store; otherwise build remote and push** — `build-load.sh` keeps `PACKAGE_PUSH=false` locally, and switches to a builder host + registry push when `E2E_BUILDER_SSH` / `E2E_IMAGE_NAMESPACE` are set inline (never written into a file).
- **Touch only what this run creates** — the Helm release, injected labels/NodeFeatures, mocked `Devices`, test workloads. Never touch the user's pre-existing resources.
- **Confirm every mutating step** (`build-load.sh`, `deploy.sh`, mutating cases, `teardown.sh`); read-only steps (`preflight.sh`, `assert-core.sh`, CASE 1) run unprompted.
- **Specialists are read-only** — the lead is the sole writer.

**Layout:**
- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG>`, `assert-core.sh <NS>`, `teardown.sh <NS>` (self-contained cleanup; mirrors the chart's `cleanup.sh`), plus the cluster-lifecycle trio `cluster-auth.sh <modality>` (read-only), `provision.sh <modality>` and `destroy.sh <modality>`, and `kube-context.sh <ctx>` (read-only; targets a context that is not the current one).
- `cases/case-N.sh <NS>` — one scenario each; ends in a `STATUS | CHECK | OBJECT` table, exits non-zero on any FAIL.
- `cases/_partition-lib.sh` — sourced by the hardware-partition cases (24–32, 34) for node correlation, the `MIG_NODE_SSH` gate, profile/key discovery and pod plumbing. Not a case; never run it directly.
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
| 6 | Pooled four-view + watch freshness | `pkg/worker/controllers/worker/instance_type.go`, `pkg/worker/webhooks/worker/instance_type.go`, `api/worker/v1alpha1/{instance_type,devices}.go` | `cases/case-6.sh` | yes (confirm) | any |
| 7 | Portless Instance reaches Ready, creates no Service | `pkg/worker/controllers/worker/instance.go` | `cases/case-7.sh` | yes (confirm) | any |
| 8 | Real accelerator slicing runtime isolation | `pkg/deviceplugin/**`, `pkg/devicemanager/**`, `pkg/worker/webhooks/worker/pod.go` | `cases/case-8.sh` | yes (confirm) | real GPU · skips: no `*.sliced` |
| 9 | Instance lifecycle survives an InstanceType unit-spec change | `pkg/worker/webhooks/worker/instance.go` | `cases/case-9.sh` | yes (confirm) | any |
| 10 | Start re-validates a resized-while-stopped Instance (no create-check bypass) | `pkg/worker/webhooks/worker/instance.go` | `cases/case-10.sh` | yes (confirm) | any |
| 11 | Per-card logical-slice accounting: slices pack, and no card is over-committed (SL view + per-card OnceMax) | `pkg/deviceplugin/{server,helper}.go`, `pkg/worker/controllers/worker/{node_devices_admission,instance_type}.go` | `cases/case-11.sh` | yes (confirm) | real GPU · skips: <2 logically sliceable cards |
| 12 | Logically sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1 | `pkg/worker/webhooks/worker/instance.go`, `pkg/utils/quantityx/quantity.go` | `cases/case-12.sh` | yes (confirm) | real GPU · skips: no logically sliceable pool |
| 13 | SSH-enabled sliced Instance: slice visible over SSH + confined shell | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh`, `pkg/deviceplugin/**`, `pkg/devicemanager/allocator/**` | `cases/case-13.sh` | yes (confirm) | real GPU + ssh · skips: no `*.sliced` / no ssh |
| 14 | Multiple slices coexist on one physical card within budget | `pkg/worker/controllers/worker/{node_devices_admission,instance_type}.go`, `pkg/deviceplugin/**` | `cases/case-14.sh` | yes (confirm) | real GPU · skips: no `*.sliced` |
| 15 | Exclusive whole-card SSH Instance still works (regression) | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh` | `cases/case-15.sh` | yes (confirm) | real GPU + ssh · skips: no `*.sliced` / no ssh |
| 16 | InstanceTypeFlavor catalog + declarative queue ownership (recreate-on-delete, delete-then-wait teardown) | `pkg/worker/controllers/worker/{instance_type,node_queue,node_flavor}.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/settings/value.go` | `cases/case-16.sh` | yes (confirm) | any |
| 17 | InstanceType declarative admission (require + freeze inputs; Default stamps schedule + entrance labels) | `pkg/worker/webhooks/worker/instance_type.go`, `api/worker/v1alpha1/instance_type.go` | `cases/case-17.sh` | yes (confirm) | any |
| 18 | CPU-manufacturer awareness reshapes the catalog (finest RF + cpuDetail; collapse↔split by setting) | `pkg/nodefeature/helper.go`, `pkg/worker/settings/value.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/webhooks/worker/instance_type.go`, `pkg/worker/controllers/worker/node_flavor.go` | `cases/case-18.sh` | yes (confirm) | any |
| 19 | Awareness on: accelerated type carries real GPU + folded CPU descriptors; a real GPU Instance runs on it | `pkg/worker/controllers/worker/{node_flavor,instance_type}.go`, `pkg/worker/webhooks/worker/instance_type.go`, `pkg/nodefeature/helper.go` | `cases/case-19.sh` | yes (confirm) | real GPU · skips: GPU-less |
| 20 | Sibling InstanceTypes on one pool stay status-consistent (Devices-watch re-enqueues all) | `pkg/worker/controllers/worker/instance_type.go` | `cases/case-20.sh` | yes (confirm) | real GPU · skips: no logically sliceable pool |
| 21 | SSH Instance serves non-interactive SSH (exec channel) + interactive login unchanged | `pack/ssh-server/rootfs/chroot.sh`, `pack/ssh-server/Dockerfile`, `pkg/worker/settings/value.go` | `cases/case-21.sh` | yes (confirm) | ssh client · skips: no ssh/ssh-keygen/sftp |
| 22 | Cross-mode claims never co-locate on one physical card (exclusive/shared/sliced; free-card placement + held-when-full) | `pkg/deviceplugin/{server,controller,helper}.go`, `pkg/devicemanager/allocator/**`, `pkg/worker/webhooks/worker/pod.go`, `pkg/worker/controllers/worker/node_devices_admission.go` | `cases/case-22.sh` | yes (confirm) | real accelerator (exclusive + shared; sliced for the C/D variants) · skips: no `<base>` + `<base>.shared` |
| 23 | NVIDIA MIG dynamic-allocation lifecycle (logical→enable→carve→exclusion→reuse→reclaim→disable) | `pkg/devicemanager/allocator/nvidia/**`, `pkg/devicemanager/detector/nvidia/device.go`, `binding/nvml/**`, `pkg/deviceplugin/**`, `pkg/device/population.go`, `pkg/nodefeature/knowns.go`, `pkg/worker/controllers/worker/node_capacity.go`, `pkg/worker/webhooks/worker/pod.go` | `cases/case-23.sh` | yes (confirm) | MIG-capable NVIDIA card **+ node SSH** · skips: not MIG-capable / no `MIG_NODE_SSH` |
| 24 | Mixed node: a partition lands on a partitioned card, a logical slice on a whole one (zero `UnexpectedAdmissionError`) | `pkg/deviceplugin/{server,helper,controller}.go`, `pkg/device/{population,physical_placement}.go`, `pkg/devicemanager/allocator/nvidia/**`, `pkg/nodefeature/knowns.go` | `cases/case-24.sh` | yes (confirm) | partition-capable NVIDIA node with **≥2 cards** **+ node SSH** · skips: not partition-capable / <2 cards / mixed state not reachable / no `MIG_NODE_SSH` (exit 2) |
| 25 | Per-profile capacity is derived from the live ledger, not from a static ceiling (+ node-status write volume) | `pkg/worker/controllers/worker/node_capacity.go`, `pkg/device/physical_placement.go`, `pkg/nodefeature/knowns.go` | `cases/case-25.sh` | yes (confirm) | partition-capable NVIDIA card **+ node SSH** · skips: not partition-capable / no 4-slice+whole-card profile pair / no `MIG_NODE_SSH` (exit 2) |
| 26 | Partition token health is a node-level count: allocated + remaining | `pkg/deviceplugin/{server,controller}.go`, `pkg/device/physical_placement.go` | `cases/case-26.sh` | yes (confirm) | partition-capable NVIDIA card **+ node SSH** · skips: not partition-capable / no whole-card profile / no `MIG_NODE_SSH` (exit 2) |
| 27 | A partitioned card is never judged feasible for an exclusive or shared claim | `pkg/deviceplugin/helper.go`, `pkg/device/population.go`, `pkg/worker/controllers/worker/{instance_type,node_devices_admission}.go` | `cases/case-27.sh` | yes (confirm) | partition-capable NVIDIA node with **≥2 cards, none partitioned at start** **+ node SSH** · skips: not partition-capable / <2 cards / a card is already partitioned / no `MIG_NODE_SSH` (exit 2); the fill sub-check skips above `MIG_MAX_FILL` |
| 28 | The SSH sidecar of a partition-backed workload is confined to that same partition | `pkg/deviceplugin/server.go`, `pkg/devicemanager/allocator/nvidia/**`, `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/**` | `cases/case-28.sh` | yes (confirm) | partition-capable NVIDIA card **+ node SSH** · skips: not partition-capable / no partition profile / no `MIG_NODE_SSH` (exit 2) |
| 29 | Two concurrent requests for different profiles each get their own instance | `pkg/deviceplugin/{controller,server}.go`, `pkg/device/physical_placement.go` | `cases/case-29.sh` | yes (confirm) | partition-capable NVIDIA card **+ node SSH** · skips: not partition-capable / fewer than two coexisting profiles / no `MIG_NODE_SSH` (exit 2) |
| 30 | A terminated init container still charges the card its instance occupies | `pkg/deviceplugin/controller.go`, `pkg/worker/controllers/worker/node_capacity.go`, `pkg/worker/webhooks/worker/pod.go` | `cases/case-30.sh` | yes (confirm) | partition-capable NVIDIA card **+ node SSH** · skips: not partition-capable / no 4-slice+whole-card profile pair / no `MIG_NODE_SSH` (exit 2) |
| 31 | A same-profile replacement scheduled inside the reclaim window (observation) | `pkg/deviceplugin/reclaim.go`, `pkg/devicemanager/allocator/nvidia/mig.go`, `pkg/worker/controllers/worker/node_capacity.go` | `cases/case-31.sh` | yes (confirm) | partition-capable NVIDIA card **+ node SSH** · skips: not partition-capable / no 4-slice profile / no `MIG_NODE_SSH` (exit 2) |
| 32 | An instance carved outside GPUStack: placement sees it, the node keys never do (observation) | `pkg/device/physical_placement.go`, `pkg/devicemanager/allocator/nvidia/mig.go`, `pkg/worker/controllers/worker/node_capacity.go` | `cases/case-32.sh` | yes (confirm) | partition-capable NVIDIA card, **exactly one partitioned**, **+ node SSH with MIG instance management** · skips: not partition-capable / ≠1 partitioned card / no vendor profile id / carve produced nothing / no `MIG_NODE_SSH` (exit 2) |
| 34 | `single-numa-node` topology with partition capacity only on the far socket (observation) | `pkg/deviceplugin/server.go` (the NUMA topology reported per family) | `cases/case-34.sh` | yes (confirm) | **dual-socket** node running `topologyManagerPolicy: single-numa-node`, partition-capable card **+ node SSH** · skips: <2 NUMA nodes / policy is not `single-numa-node` / policy unreadable / card has no NUMA affinity / no `MIG_NODE_SSH` (exit 2) |
| 35 | Ascend logical-slice placement: claims pack, spill only on a misfit, and never cross into an exclusive card | `pkg/deviceplugin/{server,helper}.go`, `pkg/devicemanager/allocator/ascend/**`, `pkg/worker/webhooks/worker/pod.go` | `cases/case-35.sh` | yes (confirm) | real **Ascend** hardware, >=3 logically sliceable cards on one node with >=1 free at start, **+ a CANN-family image** · skips: <3 logically sliceable ascend cards; fails setup: no free card |

- Also run **CASE 1 at minimum** for changes under `pkg/worker/controllers/**`, `pkg/*/webhooks/**`, `pkg/worker/extensionapis/**`, `api/**`, `pkg/extensionapi/**`, `pkg/worker/kuberess/**`.
- `spec.os`/`spec.arch` materialization is asserted **inline** — CASE 1 (cpu pool) + CASE 6 (accelerated) — not as a standalone case.
- CASE 3 under the queue-ownership split: draining a pool no longer deletes the InstanceType — the flavor is deleted but the **type survives with its queue emptied**, and reactivates when nodes return.
- CASE 4's mock uses a fake product key (`nvidia-e2emock`) that never collides with a real GPU pool → safe on a real-accelerator cluster too.
- **Two disjoint accelerator families, two disjoint card populations.** *Logical slicing* (software; the vendor preload library) is `<base>.sliced` + `.sliced.units` / `.cores-percentage` / `.memory-percentage` / `.memory-mib`, served **only** by a card that is not in a hardware partitioning mode. *Physical partitioning* (hardware; NVIDIA MIG) is `<base>.partitioned` + `.partitioned.units` + one `<base>.partitioned.<kind>-<profile>` key per profile (`<kind>` is `mig` for NVIDIA), served **only** by a card that is. A card serves exactly one family, which is why the `InstanceType` column is four views (`Accelerator(EX/SH/SL/PT)`) and why a case that deploys a logical slice must select a pool with a non-zero *logical* slice count, never merely a "sliceable" one. Normative reference: [`docs/accelerator-requests.md`](../../../docs/accelerator-requests.md).
- CASE 22 needs a **real** accelerator (the co-location can only be observed on cards the device plugin actually allocates). It proves the cross-mode mutual-exclusion invariant on both enforcement levels: ListAndWatch reporting an opposite-mode-held card's tokens Unhealthy — a claim lands exactly on the FREE card (variant C's sliced-after-exclusive is the production UnexpectedAdmissionError regression guard) or is Unschedulable when none is free — and the Allocate `FailedPrecondition` backstop. Variants A/B cover exclusive→shared (Kueue path + raw path); variants C/D cover exclusive↔sliced on the raw path (the card pick they prove is path-independent) and skip independently when no `<base>.sliced` companion is advertised. The three families it exercises all live on the **unpartitioned** population — a partitioned card advertises none of them, so it never enters this case's card set.
- CASE 23 is the **only** case that toggles node hardware state (`nvidia-smi -mig`), so it needs the node's SSH address and will **not** guess it: it reads `MIG_NODE_SSH=<user@host>` and **exits 2 (input required)** — proceeding no further — when it is unset. **Ask the user for the node address and pass it inline at run time** (`MIG_NODE_SSH=<user@host> bash …/case-23.sh <NS>`); never hardcode it. It auto-skips (exit 0) when the card is not MIG-capable. It is self-recovering: the trap restores the card's original MIG mode (and re-detects) on pass AND fail. Because enabling MIG moves the card from the logical family to the partition family **entirely** — the `.sliced.*` keys disappear and the `.partitioned.*` keys appear — this case owns the whole mode transition, driving the Device Manager re-detect between every mode change by **rollout-restarting the DaemonSet only**: the detect loop watches `{manufacturer, id, unhealthy}`, which a mode toggle does not change, but an existing group's capability is now rewritten in place, so deleting the `Devices` object is *not* required (and the case deliberately does not, so a regression of that stays visible). It asserts the family swap only when **every** card of the group is partitioned; with an unpartitioned sibling the logical keys legitimately survive and that sub-check records SKIP.

- CASE 35 is the **Ascend** logical-slice placement case, and the only one whose expectations are **computed from the ledger immediately before each claim** rather than hardcoded — so it is valid on a pool that already carries unrelated workloads, which is the normal state of an accelerator cluster someone is actually using. It splits the contract in two per claim: *did it join an in-use card that had room* (the defect — opening a fresh card strands the node) and *did it take the fullest card that fits* (the policy). When no in-use card had room the first records **SKIP**, never a vacuous PASS. Ascend-only because the runtime-confinement cross-check reads `ASCEND_VISIBLE_DEVICES`; the placement assertions themselves read the allocation the plugin recorded on the Pod and are vendor-neutral, so a sibling case needs only that variable's name. Its claim carrier must be a **CANN-family image**: allocating an Ascend slice installs an `/etc/ld.so.preload` pulling in the Ascend userspace runtime, and in a bare base image every process — `sleep` included — dies with exit 127 (`libc_sec.so`), which the vendor runtimeClass does **not** fix. Override with `E2E_SLICE_IMAGE=<ref>`. CASE 11 covers the same packing property on NVIDIA and needs a pristine pool.
- **CASES 24–32 and 34 are the hardware-partition family** and share one sourced helper,
  `cases/_partition-lib.sh` — node correlation, the `MIG_NODE_SSH` gate, profile/key discovery, the pod
  plumbing and the `record` idiom. The leading underscore marks it as **not a case**: it has no header, no
  trap and no results table, all of which stay with the case that sources it. Reuse it when adding another
  partition case rather than copying the discovery block again.
  - Every one of them **reads the node's SSH address from `MIG_NODE_SSH=<user@host>` and exits 2 (input
    required) when it is unset**, exactly like CASE 23 — ask the user and pass it inline at run time; never
    hardcode it. They **auto-skip (exit 0)** when the hardware itself is missing.
  - Each one **partitions a card only if none is partitioned yet**, and restores exactly the card it
    toggled. So partitioning one card once up front (`nvidia-smi -i 0 -mig 1` + a Device Manager rollout
    restart) lets 25, 26, 28–32 run back to back without paying a mode switch each — but **CASE 27 requires
    the opposite**: it measures the before/after delta of the whole-card and shared keys, so it skips if any
    card is already partitioned. Run **27 first**, then 24, then the rest.
  - Per-profile keys are always **discovered from the node** (`<base>.partitioned.*` minus `.units`) and
    profiles from each card's own capability, never composed — the `<kind>` segment is a per-manufacturer,
    environment-overridable name. A device-plugin pool key only **zeroes out**; it never disappears, so the
    cases assert absent-or-zero for those.
  - Optional environment, all with defaults: `MIG_NODE_NAME`, `MIG_NODE_SSH_OPTS`, `MIG_GPU_INDEX` (0),
    `MIG_SSH_TIMEOUT` (90), `IMAGE`, `MIG_MIXED_INDEXES` / `MIG_MIXED_ROUNDS` (CASE 24),
    `MIG_WRITE_IDLE_WINDOW` (CASE 25), `MIG_MAX_FILL` (CASE 27), `F11_IMAGE` / `F11_EXPECT` (CASE 28),
    `MIG_RECLAIM_BOUND` (CASE 31), `MIG_OOB_WINDOW` (CASE 32).
- **CASE 24 is the headline regression guard** for the failure the two-family split exists to remove: on a
  mixed node a single token pool used to let the kubelet hand a partition request a token from a card that
  cannot be partitioned, and the Pod died with a terminal `UnexpectedAdmissionError`. It runs several rounds
  because one correct placement could be luck.
- **CASES 25, 31, 32 and 34 record observations rather than assert thresholds** — the ledger-vs-hardware
  reclaim gap, an administrator's hand-carved instance, node-level over-advertisement of mutually exclusive
  profiles, and cross-socket alignment are accepted consequences with stated containment, so the cases
  measure them and print a copyable block for the design record. Each still carries **one** hard assertion
  where a real regression would hide: the replacement converges on its own (31), placement never
  double-books a card (32). **CASE 28 prints the same copyable block but is a guard**, not an observation: it
  asserts the sidecar's visible-devices env names exactly `main`'s partition and carries no whole-card
  identity, and only an explicit `F11_EXPECT=observe` demotes that verdict back to a recording.

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

- **Mutation posture** rides the title line: `READ-ONLY` (no trap) or `MUTATING, self-recovering` (writes, then a trap restores). Append `AUTO-SKIPS without <requirement>` when the case self-skips (real hardware, `ssh` client, ≥2 logically sliceable cards, …).
- **Environment** always names the auto-skip condition so the run/skip decision is readable without executing the case.
- **Inputs** distinguish **MOCKED** inputs (fake accelerator NodeFeature, phantom-node `Devices` ledger) from the **real** objects under test — the mock is a fixture, never the verification.
- Describe behavior in **plain terms**, not spec reference; the case↔code mapping lives in the table's "Run when these change" column.
- **Restore the baseline, pass or fail.** A case mutating shared baseline (an editable Setting, a `gpustack.ai/managed` toggle, an injected node/NodeFeature label, a patched unit spec) MUST restore it in `trap … EXIT` (runs on pass AND fail) and wait for the chain to settle back. Between cases the lead re-confirms the baseline (awareness off, all nodes managed, no leftover test objects) and restores it if a case died before its trap ran — a mutated baseline is a frequent cause of a *spurious* failure in the NEXT case.
- Runtime output (results banner, `record` messages, FAIL-footer) stays spec-anchor-free and self-explanatory.

## Flow

Generic phase semantics (roles, rendezvous, report, fix-retest) live in [`orchestration.md`](../_e2e-lib/references/orchestration.md); below are the operator-e2e specifics. Let `RPT=.claude/reports/$(date +%F)-operator-e2e`.

**Phase 0 — Environment (read-only; provisioning only on opt-in).** Preflight, offer the user's own contexts, confirm the target, record drift:

```bash
bash .claude/skills/_e2e-lib/scripts/preflight.sh
kubectl config get-contexts
git rev-parse HEAD; git diff --stat origin/main...HEAD
```

**A cluster the user already has is the default** — enumerate the contexts, offer them, and do not continue until the user confirms which one this run verifies against (`k3s` / `docker-desktop` / any reachable context). Then `mkdir -p "$RPT"/raw` and write the report header (incl. cluster provenance) + node inventory.

Only when the user has no usable cluster, **and separately opts in**, provision one — full protocol in [`cluster-provisioning.md`](../_e2e-lib/references/cluster-provisioning.md), which is normative for everything below:

```bash
bash .claude/skills/_e2e-lib/scripts/cluster-auth.sh <eks|k3s|nebius>          # read-only, before provisioning
bash .claude/skills/_e2e-lib/scripts/provision.sh  <eks|k3s|nebius> -var='<k>=<v>' ...   # MUTATING + BILLABLE
```

- **Two confirmations, never one:** (a) provision at all, (b) which modality — asked after stating the cost. `k3s` installs onto servers the user already owns (**cheap**); `eks` and `nebius` **bill real money per hour** on cloud (and accelerator) hardware.
- **Check the cloud CLI's auth before provisioning and again before teardown.** A module may resolve its GPU driver/OS preset from a **live cloud API call at plan, apply AND destroy time**, so a credential that expires mid-run fails `terraform destroy` and strands paid hardware.
- **Node preparation is a named step before Phase 3.** A managed GPU node may ship a vendor device plugin as a **static Pod** under `/etc/kubernetes/manifests/` (it competes for the accelerator resource and cannot be `kubectl delete`d) plus a **DCGM service** holding driver handles (it blocks a MIG mode switch). Disable both, record them as outstanding environment mutations, and restore them in Phase 8 on a cluster the user brought.
- The Kubernetes minor version may be constrained by **GPU driver-preset availability**, not by the operator — record why the version was chosen.
- Record the modality and the shaping `terraform` inputs in the report so the run is reproducible; host addresses, SSH targets and cloud project ids live in the live command only, **never in a file**.

**Context checkpoints.** This suite runs for hours. At each trigger point — after Phase 3, after each block of ~3–4 cases (or at the end of the partition family), before a Phase 5 triage or a Phase 7 loop, and **always before Phase 8** — pause between cases, bring `run-log.md` up to date, tell the user plainly that compacting now would protect the rest of the run, and emit the ready-to-use **compact focus block** (contents + worked example in [`orchestration.md`](../_e2e-lib/references/orchestration.md)). Compacting is the user's action. The block must carry the cluster's provenance and the teardown obligation above everything else: the worst outcome of a compaction is a lead that forgets it provisioned a paid cluster.

**Phase 1 — Test-item list.** Match `git diff --name-only origin/main...HEAD` against the case table; also cross-check `specs/2026-06-29-instancetype-unified-pool-refactor.md` (its `### User Stories`) for items **beyond** the table. Confirm with the user (`AskUserQuestion`); CASE 1 always included. No drift → ask for new items, invoking `interview-me` if the ask is too coarse.

**Phase 2 — Plan.** Split into the serial mutating sequence + parallel read-only analyses; pick the specialist roster — logic → `agent-skills:test-engineer`; over-admit/AdmissionCheck security → `agent-skills:security-auditor`; real-slice runtime/perf → `general-purpose`; root-cause → `agent-skills:code-reviewer`. Write the test-plan section.

**Phase 3 — Build & deploy (confirm).** Per-commit tag so the kubelet runs the new image:

```bash
TAG=dev-$(git rev-parse --short HEAD); NS=gpustack-system
bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG"   2>&1 | tee "$RPT"/raw/01-build.txt
bash .claude/skills/_e2e-lib/scripts/deploy.sh "$NS" "$TAG" 2>&1 | tee "$RPT"/raw/02-deploy.txt
bash .claude/skills/_e2e-lib/scripts/assert-core.sh "$NS"   2>&1 | tee "$RPT"/raw/03-assert-core.txt
```

**When the cluster's nodes are not this machine** (any provisioned cluster, and most remote ones) the local build is useless to them, and a node of a different architecture cannot run it at all. Switch `build-load.sh` to remote mode by supplying the builder and a pushable registry namespace **inline** — both stay out of every file:

```bash
E2E_BUILDER_SSH=<user@host> E2E_IMAGE_NAMESPACE=<ns> \
  bash .claude/skills/_e2e-lib/scripts/build-load.sh "$TAG" 2>&1 | tee "$RPT"/raw/01-build.txt
```

It syncs the exact commit to the builder as a git bundle before building. That step is not optional: a builder is not a clone that follows this branch, so a bare `make package` there builds whatever it last had checked out and produces an image of the wrong revision — which surfaces only later, as `assert-core.sh`'s stale-image guard. **Honor that guard rather than waiving it**, even when the intervening diff looks non-compiling: `case-1.sh` embeds the same assertion and aborts on it.

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

A log that says nothing is not evidence the code did not run: device-plugin `Allocate` /
`GetPreferredAllocation` decisions log above the deployed `-v`. Raise verbosity on the pod **before**
re-triggering the case — see `../_e2e-lib/references/troubleshooting.md` (*Component log verbosity*).

**Phase 6 — Summary.** Write the summary (confirmed problems + repro + conclusion; classify each as real bug / test-design issue / awaiting-owner) + the **suite gaps & upgrade backlog** accrued during the run.

**Phase 7 — Fix & retest / Suite upgrade (optional).** Per `orchestration.md`, two user-gated branches: (a) real bugs → confirm, choose local vs remote (SSH/RSYNC) packaging (record connection info), fix → `make lint` → signoff commit → package → `helm upgrade` → retest → append; (b) sink the self-upgrade backlog into the suite (new/adjusted `cases`, tightened assertions, case-table/`references` updates) or this protocol — confirm each, signoff commit, retest any changed case.

**Phase 8 — Teardown (mandatory, two levels).** Run unconditionally as the final phase (confirm each call); defer only if a Phase 7 loop is still running. Offer a context checkpoint **before** starting it.

*Level 1 — in-cluster (always):*

```bash
bash .claude/skills/_e2e-lib/scripts/teardown.sh gpustack-system 2>&1 | tee "$RPT"/raw/90-teardown.txt
```

`teardown.sh` removes the test artifacts, the operator release — which now takes Kueue / NFD / the CSI drivers with it, since they are subcharts of it — the releases it does not own (the worker's own image-mode release, and the pre-subchart per-application ones), their CRDs/finalizers (including the operator's `gpustack.ai/controlled` on **Instances and InstanceTypes**), and the runtime APIServices/webhooks. The `gpustack-system` namespace is kept on purpose. Never delete the user's pre-existing resources. Then walk the report's **environment mutations** list and restore anything still outstanding (node preparation, disabled static Pod / DCGM service, toggled card mode).

*Level 2 — infrastructure (ONLY when this run provisioned the cluster):*

```bash
bash .claude/skills/_e2e-lib/scripts/cluster-auth.sh <modality> 2>&1 | tee "$RPT"/raw/91-destroy-auth.txt
bash .claude/skills/_e2e-lib/scripts/destroy.sh     <modality> 2>&1 | tee "$RPT"/raw/92-destroy.txt
```

Re-check the credential first — a run this long outlives a short-lived cloud token, and an expired one fails the destroy and leaves paid hardware billing. `destroy.sh` verifies the Terraform state is empty afterwards and exits non-zero if it is not: **the run is not over until that check passes.** A cluster the user brought stops at level 1 and is **never** destroyed.

## References

- `../_e2e-lib/references/orchestration.md` — the shared multi-specialist flow: roles, phases, rendezvous rules, report layout, the context-checkpoint discipline (with a worked focus block), and the fix-and-retest loop.
- `../_e2e-lib/references/cluster-provisioning.md` — where the cluster comes from: bring-your-own first, the three modalities and their cost asymmetry, the credential-is-a-teardown-dependency rule, node preparation, and the destroy obligation.
- `references/drain-recycle.md` — why CASE 2–6 need a real cluster (the fake-client blind spots), the managed-toggle code path, and the accelerated mock recipes (fake accelerator NodeFeature + the phantom-node `Devices` ledger, patched on the **v1alpha1** CRD).
- `references/manual-ssh-verification.md` — the manual pass CASE 21 cannot drive in CI: a real VS Code Remote-SSH session (workspace opens, integrated terminal in `main`) and an `sshfs` mount round-trip.
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
