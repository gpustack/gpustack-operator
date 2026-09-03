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

- Chain detail → [architecture.md](../../../docs/architecture.md) (overview), then [scheduling-chain.md](../../../docs/architecture/scheduling-chain.md) for the reconcilers and [admission.md](../../../docs/architecture/admission.md) for the five gates / four-view math these cases assert; the unified-pool refactor this suite tracks → `specs/2026-06-29-instancetype-unified-pool-refactor.md`.
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
- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG>` (**installs only** — it refuses an existing release; upgrade in place with `helm upgrade`, or `teardown.sh` first), `assert-core.sh <NS>`, `teardown.sh <NS>` (test artifacts, release uninstall and a CRD-drain verdict; the cleanup itself is delegated to the chart's `files/cleanup.sh`), plus the cluster-lifecycle trio `cluster-auth.sh <modality>` (read-only), `provision.sh <modality>` and `destroy.sh <modality>`, and `kube-context.sh <ctx>` (read-only; targets a context that is not the current one).
- `cases/case-N.sh <NS>` — one scenario each; ends in a `STATUS | CHECK | OBJECT` table, exits non-zero on any FAIL.
- `cases/_partition-lib.sh` — sourced by the hardware-partition cases (24–32, 34) for node correlation, the `MIG_NODE_SSH` gate, profile/key discovery and pod plumbing. Not a case; never run it directly.
- `cases/run-partition-block.sh <RAW_DIR> [NS] [CASES...]` — runs that family in its required order, writing each case's raw log and exit code. **Drive 24–32/34 with it rather than by hand**: the ordering is a real constraint and the block outlives a context, so a step held only in conversation is one a compaction drops.
- `references/` — `drain-recycle.md` (per-case rationale + mock recipes), `packaged-image-deploy.md` (image-ref ↔ chart-values contract); shared `../_e2e-lib/references/{orchestration,troubleshooting}.md`.

## Cases (locked titles)

Each case is self-contained; its header (see **Case header contract**) states goal / environment (incl. auto-skip) / inputs (MOCKED vs real) / expected / cleanup. `Needs` = the hardware/tooling the case requires; the exact auto-skip conditions live in the case's own `Environment:` field, which is what the script actually prints.

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Mutates | Needs |
|---|---|---|---|---|
| 1 | CPU-only scheduling chain materializes — zero Cohort, InstanceType Active | always (mandatory) | no | any |
| 2 | Running Instance admits, then drain stops it (not recreate) | `pkg/worker/controllers/worker/instance.go`, `pkg/worker/webhooks/worker/instance.go`, `pkg/worker/kuberess/apps_kueue.go` | yes (confirm) | any |
| 3 | Managed-toggle scopes node onboarding | `pkg/worker/controllers/worker/{node_flavor,instance_type}.go`, `pkg/nodefeature/helper.go` | yes (confirm) | any |
| 4 | AdmissionCheck holds exclusive over-admit | `pkg/worker/controllers/worker/{node_devices_admission,node_devices,instance_type}.go`, `pkg/worker/kuberess/apps_kueue.go` | yes (confirm) | any |
| 5 | Pod webhook folds slice-by-memory-% into units | `pkg/worker/webhooks/worker/pod.go`, `pkg/nodefeature/knowns.go` | yes (confirm) | any |
| 6 | Pooled four-view + watch freshness | `pkg/worker/controllers/worker/instance_type.go`, `pkg/worker/webhooks/worker/instance_type.go`, `api/worker/v1alpha1/{instance_type,devices}.go` | yes (confirm) | any |
| 7 | Portless Instance reaches Ready, creates no Service | `pkg/worker/controllers/worker/instance.go` | yes (confirm) | any |
| 8 | Real accelerator slicing runtime isolation | `pkg/deviceplugin/**`, `pkg/devicemanager/**`, `pkg/worker/webhooks/worker/pod.go` | yes (confirm) | real GPU, logical slicing |
| 9 | Instance lifecycle survives an InstanceType unit-spec change | `pkg/worker/webhooks/worker/instance.go` | yes (confirm) | any |
| 10 | Start re-validates a resized-while-stopped Instance (no create-check bypass) | `pkg/worker/webhooks/worker/instance.go` | yes (confirm) | any |
| 11 | Per-card logical-slice accounting: slices pack, and no card is over-committed (SL view + per-card OnceMax) | `pkg/deviceplugin/{server,helper}.go`, `pkg/worker/controllers/worker/{node_devices_admission,instance_type}.go` | yes (confirm) | real GPU, >=2 logically sliceable cards |
| 12 | Logically sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1 | `pkg/worker/webhooks/worker/instance.go`, `pkg/utils/quantityx/quantity.go` | yes (confirm) | real GPU, logically sliceable pool |
| 13 | SSH-enabled sliced Instance: slice visible over SSH + confined shell | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh`, `pkg/deviceplugin/**`, `pkg/devicemanager/allocator/**` | yes (confirm) | real GPU, logical slicing + ssh |
| 14 | Multiple slices coexist on one physical card within budget, each reporting its own share | `pkg/worker/controllers/worker/{node_devices_admission,instance_type}.go`, `pkg/deviceplugin/**`, `pkg/devicemanager/detector/slice.go`, `pkg/devicemanager/procattr/**`, `pkg/kubemetrics/**` | yes (confirm) | real GPU, logical slicing |
| 15 | Exclusive whole-card SSH Instance still works (regression) | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/rootfs/chroot.sh` | yes (confirm) | real GPU, logical slicing + ssh |
| 16 | InstanceTypeFlavor catalog + declarative queue ownership (recreate-on-delete, delete-then-wait teardown) | `pkg/worker/controllers/worker/{instance_type,node_queue,node_flavor}.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/settings/value.go` | yes (confirm) | any |
| 17 | InstanceType declarative admission (require + freeze inputs; Default stamps schedule + entrance labels) | `pkg/worker/webhooks/worker/instance_type.go`, `api/worker/v1alpha1/instance_type.go` | yes (confirm) | any |
| 18 | CPU-manufacturer awareness reshapes the catalog (finest RF + cpuDetail; collapse↔split by setting) | `pkg/nodefeature/helper.go`, `pkg/worker/settings/value.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/webhooks/worker/instance_type.go`, `pkg/worker/controllers/worker/node_flavor.go` | yes (confirm) | any |
| 19 | Awareness on: accelerated type carries real GPU + folded CPU descriptors; a real GPU Instance runs on it | `pkg/worker/controllers/worker/{node_flavor,instance_type}.go`, `pkg/worker/webhooks/worker/instance_type.go`, `pkg/nodefeature/helper.go` | yes (confirm) | real GPU |
| 20 | Sibling InstanceTypes on one pool stay status-consistent (Devices-watch re-enqueues all) | `pkg/worker/controllers/worker/instance_type.go` | yes (confirm) | real GPU, logically sliceable pool |
| 21 | SSH Instance serves non-interactive SSH (exec channel) + interactive login unchanged | `pack/ssh-server/rootfs/chroot.sh`, `pack/ssh-server/Dockerfile`, `pkg/worker/settings/value.go` | yes (confirm) | ssh client (ssh-keygen, sftp) |
| 22 | Cross-mode claims never co-locate on one physical card (exclusive/shared/sliced; free-card placement + held-when-full) | `pkg/deviceplugin/{server,controller,helper}.go`, `pkg/devicemanager/allocator/**`, `pkg/worker/webhooks/worker/pod.go`, `pkg/worker/controllers/worker/node_devices_admission.go` | yes (confirm) | real accelerator, exclusive + shared (C/D also sliced) |
| 23 | NVIDIA MIG dynamic-allocation lifecycle (logical→enable→carve→exclusion→reuse→reclaim→disable) | `pkg/devicemanager/allocator/nvidia/**`, `pkg/devicemanager/detector/nvidia/device.go`, `binding/nvml/**`, `pkg/deviceplugin/**`, `pkg/device/population.go`, `pkg/nodefeature/knowns.go`, `pkg/worker/controllers/worker/node_capacity.go`, `pkg/worker/webhooks/worker/pod.go` | yes (confirm) | MIG-capable NVIDIA card + node SSH |
| 24 | Mixed node: a partition lands on a partitioned card, a logical slice on a whole one (zero `UnexpectedAdmissionError`) | `pkg/deviceplugin/{server,helper,controller}.go`, `pkg/device/{population,physical_placement}.go`, `pkg/devicemanager/allocator/nvidia/**`, `pkg/nodefeature/knowns.go` | yes (confirm) | partition-capable NVIDIA node, >=2 cards + node SSH |
| 25 | Per-profile capacity is derived from the live ledger, not from a static ceiling (+ node-status write volume) | `pkg/worker/controllers/worker/node_capacity.go`, `pkg/device/physical_placement.go`, `pkg/nodefeature/knowns.go` | yes (confirm) | partition-capable NVIDIA card + node SSH |
| 26 | Partition token health is a node-level count: allocated + remaining | `pkg/deviceplugin/{server,controller}.go`, `pkg/device/physical_placement.go` | yes (confirm) | partition-capable NVIDIA card + node SSH |
| 27 | A partitioned card is never judged feasible for an exclusive or shared claim | `pkg/deviceplugin/helper.go`, `pkg/device/population.go`, `pkg/worker/controllers/worker/{instance_type,node_devices_admission}.go` | yes (confirm) | partition-capable NVIDIA node, >=2 cards none partitioned at start + node SSH |
| 28 | The SSH sidecar of a partition-backed workload is confined to that same partition | `pkg/deviceplugin/server.go`, `pkg/devicemanager/allocator/nvidia/**`, `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/**` | yes (confirm) | partition-capable NVIDIA card + node SSH |
| 29 | Two concurrent requests for different profiles each get their own instance | `pkg/deviceplugin/{controller,server}.go`, `pkg/device/physical_placement.go` | yes (confirm) | partition-capable NVIDIA card + node SSH |
| 30 | A terminated init container still charges the card its instance occupies | `pkg/deviceplugin/controller.go`, `pkg/worker/controllers/worker/node_capacity.go`, `pkg/worker/webhooks/worker/pod.go` | yes (confirm) | partition-capable NVIDIA card + node SSH |
| 31 | A same-profile replacement scheduled inside the reclaim window (observation) | `pkg/deviceplugin/reclaim.go`, `pkg/devicemanager/allocator/nvidia/mig.go`, `pkg/worker/controllers/worker/node_capacity.go` | yes (confirm) | partition-capable NVIDIA card + node SSH |
| 32 | An instance carved outside GPUStack: placement sees it, the node keys never do (observation) | `pkg/device/physical_placement.go`, `pkg/devicemanager/allocator/nvidia/mig.go`, `pkg/worker/controllers/worker/node_capacity.go` | yes (confirm) | partition-capable NVIDIA card, exactly one partitioned + node SSH (MIG instance mgmt) |
| 34 | `single-numa-node` topology with partition capacity only on the far socket (observation) | `pkg/deviceplugin/server.go` (the NUMA topology reported per family) | yes (confirm) | dual-socket node, `single-numa-node` policy, partition-capable card + node SSH |
| 35 | Ascend logical-slice placement: claims pack, spill only on a misfit, and never cross into an exclusive card | `pkg/deviceplugin/{server,helper}.go`, `pkg/devicemanager/allocator/ascend/**`, `pkg/worker/webhooks/worker/pod.go` | yes (confirm) | real Ascend, >=3 logically sliceable cards (>=1 free) + CANN-family image |
| 36 | Node-pinned Instance with additional volumes, and the host-access gates | `pkg/worker/controllers/worker/instance.go`, `pkg/worker/webhooks/worker/instance.go`, `pkg/worker/settings/value.go`, `api/worker/v1alpha1/instance.go` | yes (confirm) | any + default StorageClass |
| 37 | Instance metrics subresource serves the current instance-scoped utilization, and answers a stopped Instance | `pkg/worker/extensionapis/worker/**`, `pkg/kubemetrics/**`, `pkg/devicemanager/**`, `pkg/utils/datax/snapshot.go`, `api/worker/v1/instance.metrics.go` | yes (confirm) | any (chain materialized) |
| 38 | AMD accelerator claims over both carriers: exclusive whole cards, logical slices, and the Instance metrics array | `pkg/devicemanager/allocator/amd/**`, `pkg/devicemanager/detector/amd/device.go`, `binding/amdsmi/**`, `csrc/amd/rocm-slicing-shim/**`, `pkg/worker/extensionapis/worker/instance.metrics.go` | yes (confirm) | real AMD, logical slicing |
| 39 | T-Head PPU: a pinned claim lands exactly where it was told, and a logical slice is capped inside the container | `pkg/devicemanager/allocator/thead/**`, `pkg/devicemanager/detector/thead/device.go`, `binding/hgml/**`, `pkg/deviceplugin/{controller,server}.go`, `csrc/thead/ppu-slicing-shim/**` | yes (confirm) | real T-Head, >=2 accelerators idle + node host context (else `PPU_NODE_SSH`) |
| 40 | The device manager exports this node's Instances as Prometheus gauges, from exactly one target | `pkg/devicemanager/exporter/**`, `pkg/devicemanager/detector/snapshot.go`, `pkg/kubemetrics/**`, `pkg/manager/metrics.go` | yes (confirm) | any node running a device manager |
| 41 | The slice pass reads only the carved cards: a whole card on the same node is never queried | `pkg/devicemanager/detector/slice.go`, `pkg/devicemanager/detector/snapshot.go`, `pkg/devicemanager/snapshot.go` | yes (confirm) | real GPU, >=2 logically sliceable cards on one node |
| 43 | Two namespaces share one KV cache pool: proportional quota, and both backend preconditions fail loudly | `pkg/worker/controllers/worker/kv_cache_pool.go`, `pkg/worker/webhooks/worker/kv_cache_pool{,_binding}.go`, `pkg/worker/kvcache/**`, `api/worker/v1alpha1/kv_cache_pool{,_binding}.go` | yes (confirm) | any (no GPU, no RDMA) + a registry the cluster can pull the Mooncake image from |
| 44 | What an effective quota enforces: a refusal when nothing can be evicted, an eviction of the domain's own objects when something can | `pkg/worker/kvcache/mooncake/tenant_metrics.go`, `pkg/worker/controllers/worker/kv_cache_pool.go` (the Binding pass) | yes (confirm) | CASE 43 passing + a registry the cluster can pull the Mooncake image from |

Each note below is something the **lead** must act on before or around a run. What a case *does* — its goal, environment, inputs, assertions and cleanup — lives in its own header, which the **Case header contract** below requires to be readable on its own; the index never restates it.

- **Two disjoint accelerator families, two disjoint card populations.** *Logical slicing* (software; the vendor preload library) is `<base>.sliced` plus its sizing keys, served **only** by a card that is not in a hardware partitioning mode. *Physical partitioning* (hardware; NVIDIA MIG) is `<base>.partitioned` plus one `<base>.partitioned.<kind>-<profile>` key per profile, served **only** by a card that is. A card serves exactly one family — which is why `InstanceType` carries four views (`Accelerator(EX/SH/SL/PT)`), and why a case deploying a logical slice must select a pool with a non-zero *logical* slice count, never merely a "sliceable" one. Normative reference: [`docs/accelerator-requests.md`](../../../docs/accelerator-requests.md).
- **Ask whether a pool BACKS the node; never match its rendered name.** The webhook stamps each
  `InstanceType` with its own schedule labels — `nodefeature.PoolScheduleLabels`, via
  `pkg/worker/webhooks/worker/instance_type.go` — expressly "so it is selectable by the same
  discriminators its `Devices` and `ResourceFlavor`s carry". So the pool backing a node is the one whose
  every label the node also carries, and the recipe is one predicate:

  ```python
  # every discriminator the pool carries is a label the node carries too
  d = {k: v for k, v in pool_labels.items() if not k.startswith('schedule.gpustack.ai/')}
  backs = bool(d) and all(node_labels.get(k) == v for k, v in d.items())
  ```

  That covers the whole identity at once — `kubernetes.io/os` / `arch`,
  `feature.gpustack.ai/acceleratable`, `acceleratable.feature.gpustack.ai/<accelerator group>` and,
  **only under `instance-type-aware-cpu-manufacturer`**, `general.feature.gpustack.ai/<general group>`.
  Enumerating `spec` fields instead means getting that last one conditionally right: with CPU awareness
  off every pool's `spec.generalGroup` is `generic`, and with it on the pools split by CPU key, so a
  hand-written tuple is wrong in one mode or the other. `schedule.gpustack.ai/*` is the pool's own
  bookkeeping and never a node label, so it is excluded.
  Add the accelerator group the case targets — as an **anchored suffix**,
  `acceleratorGroup.endswith('-' + groupID)` — only to choose among the pools of a node carrying more
  than one accelerator group. The per-node group id from `Devices.spec.groups[].id` cannot carry the
  match on its own: it has no manufacturer, no os/arch and no CPU group.

  Three ways a looser match picks the wrong pool, all silent:
  - **A substring of the InstanceType name.** Real product tokens nest — `a10` inside `a100`, `l40`
    inside `l40s`, `h20` inside `h200` — so an A10 node's group id also matches the A100 pool's name.
    Which one wins then depends on API list order, not on anything the case controls. The anchored
    suffix above is what closes this.
  - **Ignoring os/arch.** The name carries `-<os>-<arch>` and the group id does not, so on a mixed-arch
    cluster with one accelerator model a node's group id matches **both** arch pools, and `linux-amd64`
    sorts ahead of `linux-arm64`.
  - **Ignoring the general group.** Under CPU awareness two nodes with the same accelerator and os/arch
    but different CPUs are different pools, and a tuple that stops at the accelerator matches both.

  The damage is not a clean error: the claim is submitted through the wrong pool's entrance LocalQueue,
  whose ResourceFlavor selects other nodes, so a node-pinned Pod is simply never admitted and the case
  fails on a timeout that reads like a placement defect. Worse where a ceiling comes from the pool —
  CASE 11 reads its over-commit limit from `Status.Detail.Memory` — because a mismatched pool with larger
  cards makes a real over-commit pass vacuously.
- **`spec.os`/`spec.arch` has no case of its own** — CASE 1 (cpu pool) and CASE 6 (accelerated) assert it inline.
- **CASE 4 is safe on a real-accelerator cluster**: its mock uses a fake product key (`nvidia-e2emock`) that never collides with a real GPU pool.
- **CASES 23–32, 34 and 39 need a node address you must ask the user for.** 23–32 and 34 read `MIG_NODE_SSH=<user@host>`; CASE 39 reads `PPU_NODE_SSH=<user@host>`, and only when it is not already running on the node. Each **exits 2 (input required)**, going no further, when its address is unset — ask for it and pass it inline at run time, never hardcoded. All **auto-skip (exit 0)** when the hardware itself is missing.
- **Vendor cases carry a vendor image**, because every in-container reading is that vendor's own tool: CASE 35 a CANN-family image (a bare one exits 127 on an Ascend slice), CASE 38 a ROCm-family image, CASE 39 any image shipping `ppu-smi`. Each takes an `E2E_*_IMAGE=<ref>` override, and all are large — pre-pull on the node, or the first claim spends its whole wait pulling.
- **CASE 23 owns the whole MIG mode transition** — enabling MIG moves a card out of the logical family and into the partition one entirely — and is self-recovering: its trap restores the card's original mode on pass AND fail.
- **CASES 24–32 and 34 are the hardware-partition family**, sharing `cases/_partition-lib.sh` (node correlation, the SSH gate, profile/key discovery, pod plumbing, the `record` idiom). Its leading underscore marks it **not a case**; reuse it when adding another partition case rather than copying the discovery block.
  - Each partitions a card only if none is partitioned yet and restores exactly the card it toggled, so one up-front `nvidia-smi -i 0 -mig 1` plus a Device Manager rollout restart lets most of them run back to back — but **CASE 27 requires the opposite** and skips if any card is already partitioned. That ordering — **27 first, then 24, then the rest** — is what `run-partition-block.sh` encodes; drive the family with it.
  - Optional environment, all with defaults: `MIG_NODE_NAME`, `MIG_NODE_SSH_OPTS`, `MIG_GPU_INDEX` (0), `MIG_SSH_TIMEOUT` (90), `IMAGE`, `MIG_MIXED_INDEXES` / `MIG_MIXED_ROUNDS` (CASE 24), `MIG_WRITE_IDLE_WINDOW` (CASE 25), `MIG_MAX_FILL` (CASE 27), `F11_IMAGE` / `F11_EXPECT` (CASE 28), `MIG_RECLAIM_BOUND` (CASE 31), `MIG_OOB_WINDOW` (CASE 32).
- **CASE 24 is the headline regression guard** for the failure the two-family split exists to remove: a single token pool used to let the kubelet hand a partition request a token from a card that cannot be partitioned, and the Pod died with a terminal `UnexpectedAdmissionError`.
- **CASE 14 and CASES 11/22/41 want opposite POOL shapes.** CASE 14's over-budget assertion needs a pool
  of a **single** logically sliceable card — with a free sibling the third slice simply lands there
  instead of being held — so it auto-skips on a multi-card pool. CASES 11, 22 and 41 need **two or
  more**. The incompatibility is between pools, not nodes: one node carrying a single-card pool of one
  accelerator model beside a multi-card pool of another satisfies both. But CASE 14 takes the **first**
  sliceable pool it finds rather than seeking a single-card one, so even that node can leave it
  skipping — check which pool it picked before reading the skip as a hardware verdict. What genuinely
  cannot be arranged is hiding a card: the `*_VISIBLE_DEVICES` variables are what the allocators inject
  into a workload container, not a filter on what the device manager detects.
- **CASE 40's accelerator sub-check cannot execute on its own Instance.** The case creates a **CPU**
  Instance on the general pool, which holds no accelerator, so the accelerator-family label set —
  including `mode` — always skips, on any hardware. Reading it needs an accelerated Instance, and no
  case does it on this surface: **CASE 14** asserts the `mode` spelling on the Instance metrics
  subresource, but the Prometheus accelerator-label path is **uncovered** — CASE 41 and CASE 37 read
  neither `mode` nor those labels.
- **CASE 40's "exactly one target" only bites on a multi-vendor node.** A node running one device
  manager satisfies it trivially; the rule it guards — that a node's Instances are published by one
  device manager, never by each — is only exercised where two manufacturers are present. Read a PASS
  there as "not double-counted on this node", not as "the election was tested".
- **CASES 25, 31, 32 and 34 record observations, not thresholds** — accepted consequences with stated containment, measured and printed as a copyable block for the design record; each still carries one hard assertion where a real regression would hide. **CASE 28 prints the same block but is a guard**, and only an explicit `F11_EXPECT=observe` demotes its verdict to a recording.


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
