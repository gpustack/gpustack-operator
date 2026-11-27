---
name: gpustack-operator-e2e
description: "Run the operator's end-to-end suite on a Kubernetes cluster — the user's own, or one provisioned for the run and destroyed afterwards: deploy via the Helm chart, then assert the NFD → Worker → Kueue scheduling chain materializes."
disable-model-invocation: true
allowed-tools: "Read, Agent, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/cluster-auth.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/kube-context.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/build-load.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/deploy.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/provision.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/teardown.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/destroy.sh*), Bash(bash .claude/skills/gpustack-operator-e2e/cases/*), Bash(kubectl get*), Bash(kubectl describe*), Bash(kubectl logs*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(kubectl config get-contexts*), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*)"
---

# GPUStack Operator — E2E verification

Use [the shared module map](../../../docs/README.md#module-map) to locate the relevant guides,
owning specs, code entry points and related modules. [AGENTS.md](../../../AGENTS.md#finding-sources)
owns the retrieval and evidence rules; this skill owns its execution procedure.

Deploy the operator to a Kubernetes cluster — the user's own by default, or one provisioned for the run and destroyed afterwards — and verify the scheduling chain end to end:

```
NFD labels nodes → DeviceManager detects accelerators → Worker profiles capacity
  → NodeFlavor/InstanceType reconcilers → ResourceFlavor
  → InstanceType (real CRD; .status = four-view EX/SH/SL/PT)
  → ClusterQueue (exactly one isolated CQ per pool — NO Cohort) → LocalQueue
  ⊢ node-devices AdmissionCheck gates per-card feasibility
```

- Chain detail → [architecture.md](../../../docs/getting-started/architecture.md) (overview), then [devices-discovery.md](../../../docs/reference/devices-discovery.md) for `Detector` / unhealthy-retention contracts, [devices-scheduling.md](../../../docs/reference/devices-scheduling.md) for the reconcilers, and [devices-admission.md](../../../docs/reference/devices-admission.md) for the five gates / four-view math these cases assert; owning specs → `specs/2026-06-29-instancetype-unified-pool-refactor.md` and `specs/2026-11-24-cpu-mock-device-manager-and-e2e-consolidation.md`.
- **CPU-only Mock Device Manager mode (`deviceManager.mock.enabled=true`)**: deploying the chart with `deviceManager.mock.enabled=true` (mounting a declarative YAML/JSON `ConfigMap` via `--mock-devices-file` / `GPUSTACK_MOCK_DEVICES_FILE`, with `omitRuntimeClass: true` on CPU kubelets) runs the real `Detector` → `NodeFeature` + `Devices` (`device.gpustack.ai/mock: "true"`) → `Worker` → Kueue + `AdmissionCheck` → kubelet `DevicePlugin` `Allocator` chain on CPU-only Kind/k3s clusters with zero `/dev/*` mounts or preload shims. Cases 3–7 drive per-node hardware presets, virtual MIG partitions, and fault injection via `cases/_mock-dm-lib.sh` `ConfigMap` hot-reloads; case 38 exercises the deterministic mock-Forge kernel pipeline; real-hardware runtime shims and live inference engines remain covered in cases 8–13 and 32–38.

## Orchestration

Run as a **test-orchestration lead** (main agent) coordinating read-only **domain specialists** (`Agent` tool). **Read [`orchestration.md`](../_e2e-lib/references/orchestration.md) before Phase 0** — it owns roles, phases, parallelism/rendezvous rules, report layout, and the fix-and-retest loop. Below are the operator-e2e specifics.

**Hard rules** (full list in `orchestration.md`; operator-e2e specifics):
- **Pin the run to a user-confirmed context** — confirm the active context is the intended cluster. Never switch on your own judgement. When the confirmed cluster is **not** the active context, do not switch: take its isolated kubeconfig with `bash ../_e2e-lib/scripts/kube-context.sh <ctx>` and prefix every command of the run with the `KUBECONFIG=<path>` it prints (details in `orchestration.md`). On the user's explicit say-so you may switch instead — then record the context to return to as an outstanding environment mutation and restore it in Phase 8. Adopting the context a cluster **this run provisioned** merged in itself needs no such record.
- **Prefer the user's own cluster; provisioning is a separate opt-in** and a cluster this run provisioned **must** be destroyed in Phase 8 — see [`cluster-provisioning.md`](../_e2e-lib/references/cluster-provisioning.md).
- **Offer a context checkpoint at the trigger points** (see Flow); compacting is the user's action, and the lead hands over a focus block that carries the teardown obligation.
- **Build locally when the nodes share this machine's image store; otherwise build remote and push** — `build-load.sh` keeps `PACKAGE_PUSH=false` locally, and switches to a builder host + registry push when `E2E_BUILDER_SSH` / `E2E_IMAGE_NAMESPACE` are set inline (never written into a file). Both are read by `build-load.sh` itself — no case reads them, and `E2E_BUILDER_SSH` alone is not enough: the script also requires `E2E_IMAGE_NAMESPACE`.
- **Touch only what this run creates** — the Helm release, injected labels/NodeFeatures, mock `ConfigMap` overrides, test workloads. Never touch the user's pre-existing resources.
- **Confirm every mutating step** (`build-load.sh`, `deploy.sh`, mutating cases, `teardown.sh`); read-only steps (`preflight.sh`, `assert-core.sh`, CASE 1) run unprompted.
- **Specialists are read-only** — the lead is the sole writer.

**Layout:**
- The `cases/_*.sh` helpers are not cases: never run one directly. The one exception is `cases/_env-audit.sh`, which is run directly — `bash cases/_env-audit.sh <NS>` — because it audits the cluster rather than a fixture. It is read-only and reports, per case (`1..38`), whether the cluster satisfies that case's `Prerequisites:` line.
- `cases/_env-audit.sh` — the read-only preflight for the whole 38-case corpus: it parses each case's `Prerequisites:` field, reads the cluster once (the node inventory, the operator's `Devices` ledger including `device.gpustack.ai/mock`, the Mock Device Manager `DaemonSet`, store objects, and the operator's namespace), and prints one `CASE <N> | RUN|SKIP|UNKNOWN | nodes <have>/<want> | gpu <have>/<want> | features <list>` row per case. A feature nobody has ever pulled is undecidable rather than missing, so it reads `?` and the row reads UNKNOWN.
- `../_e2e-lib/scripts/` — `preflight.sh`, `build-load.sh <TAG>`, `deploy.sh <NS> <TAG>` (**installs only** — it refuses an existing release; upgrade in place with `helm upgrade`, or `teardown.sh` first), `assert-core.sh <NS>`, `teardown.sh <NS>` (custom-resource drain, test artifacts, release uninstall and a CRD-drain verdict; the drain and the cleanup are both delegated to the chart's `files/drain.sh` and `files/cleanup.sh`), plus the cluster-lifecycle trio `cluster-auth.sh <modality>` (read-only), `provision.sh <modality>` and `destroy.sh <modality>`, and `kube-context.sh <ctx>` (read-only; targets a context that is not the current one).
- `cases/case-N.sh <NS>` (`case-1.sh` through `case-38.sh`, strictly contiguous with zero gaps) — one self-contained scenario or consolidated domain suite each; ends in a `STATUS | CHECK | OBJECT` table, exits non-zero on any FAIL.
- `cases/_rows-lib.sh` — the shared plumbing every case builds on: `print_rows`, `record` and `results` with `FAILS`/`SKIPS`/`ROWS` counters, the `kubectl-shim` `PATH` injection, `wait_for`, `cq_of_instancetype`, the `gpustack-settings` readers/writers (`setting_get`, `setting_set`, `setting_unset`, `settings_save`, `settings_restore`), and `kvi_release_binding_finalizer`.
- `cases/_mock-dm-lib.sh` — sourced by the CPU Mock Device Manager cases (3, 4, 5, 7): discovers the active mock `device-manager` `DaemonSet` (`gpustack.ai/manufacturer=mock`) and its mounted `ConfigMap` (`mock_dm_detect_env`), snapshots and restores `ConfigMap` state in `EXIT` traps (`mock_dm_snapshot_cm`, `mock_dm_restore_cm`), hot-reloads per-node hardware/fault YAML specs without restarting `DaemonSet` Pods (`mock_dm_apply_spec`, `mock_dm_touch_pods`), and polls `Devices`, `InstanceType`/`AcceleratorType`, and `Node.status.allocatable` convergence.
- `cases/_quota-lib.sh` — sourced by the joint-admission quota-shortage case (28): `workload_charge` reads what Kueue charged one replica's `Workload`, and `pool_replica_bound` turns it into an upper bound on the replicas the queue's quota holds across all flavors.
- `cases/_md-lib.sh` — sourced by the `ModelDeployment` and `ModelArtifact` cases (20, 26–31, 35–37) for `AcceleratorType`/`InstanceType` discovery and rendering (`md_usable_accelerator_type`, `md_usable_instance_type`, `md_usable_type_for`, `md_cq_of_it`, `md_role_block`, `md_apply`), `Workload` verdict readers (`md_workloads`, `md_workload_verdicts`), force-release (`md_force_release`), and server-side dry-run assertions (`md_manifest`, `md_dryrun_refuses`, `md_dryrun_accepts`).
- `cases/_epd-lib.sh` — sourced by the EPD admission, topology, joint-admission, and live serving cases (26–28, 34) for the CPU-vendor `NodeFeature` fixture, router/sidecar image overrides and restore, and live serving readers (`epd_wait_pods`, `epd_router_pods`, `epd_wait_endpoint`, `epd_ec_document`, `epd_role_pod_claim`).
- `cases/_kvcache-ha-lib.sh` — sourced by the HA KV cache case (17) for the Mooncake leader `Lease`, rollout image pair, member snapshot, and failover bound verdict (`kvha_ready_leader_pod`, `kvha_lease_holder`, `kvha_member_snapshot`, `kvha_observe_handoff_snapshot`).
- `cases/_kvcache-inject-lib.sh` — sourced by the KV cache Pod injection and rendered-connector cases (18, 27) for the shared multi-tenant Mooncake backend/pool/`Binding` fixture, webhook refusal envelope checks, and store round-trip probe.
- `cases/_pool-lib.sh` — sourced by the pool-correlated accelerator cases (6–13): `discover_node_pool` matches the node's identity labels against the pool's `InstanceType` schedule labels.
- `cases/_ssh-lib.sh` — sourced by the SSH `Instance` cases (2, 7) for ed25519 keypair/Secret creation, port-forwarding, and `ssh_exec` / `ssh_sftp` / `ssh_open_loopback_forward`.
- `cases/_partition-lib.sh` — sourced by the real NVIDIA MIG hardware-partition lifecycle case (9) for node correlation, the `MIG_NODE_SSH` gate, profile/key discovery, and Pod plumbing.
- `cases/run-partition-block.sh <RAW_DIR> [NS] [CASES...]` — drives the partition family serially, running CPU Mock MIG cases (`5 7`) when Mock DM is active without `MIG_NODE_SSH`, or real hardware partition cases (`9 7 13`) with SSH MIG mode snapshot/restore when `MIG_NODE_SSH=<user@host>` is supplied.
- `cases/_rdma-lib.sh` — sourced by the consolidated NIC/RDMA case (14) for host-shape capability facts, the read-only `/sys` probe Pod, and `rdma_results`.
- `cases/run-rdma-block.sh [--report-only] <RAW_DIR> [NS] [CASES...]` — `--report-only` prints the cluster's RDMA capability matrix without touching the cluster; a run prints the same matrix and executes case 14, reporting `NOTHING WAS VERIFIED` if zero checks ran.
- `cases/_topology-tas-lib.sh` — sourced by the TAS case (15) for zone-shape measurement, disposable `Topology`/`ResourceFlavor`/`ClusterQueue`/`LocalQueue` fixtures, and `Workload`→Pod→`ModelDeployment` joins.
- `cases/_model-hub-lib.sh` — sourced by the node-delivery cases (21–23, 25): deploys `cases/_model-hub.py` as an in-cluster test Hugging Face hub or forward proxy, watches `NodeModelStore` progress writes, and reads impersonated `progress` subresources.
- `cases/_serving-lib.sh` — sourced by the live serving cases (32, 35) and delegates to `../_e2e-lib/scripts/serving-lib.sh` (shared with `gpustack-operator-benchmark`) for vendor/engine/router Prometheus counters and transfer evidence.
- `references/` — `drain-recycle.md` (CPU drain/managed-toggle rationale + CPU Mock DM architecture for cases 2–5), `packaged-image-deploy.md` (image-ref ↔ chart-values contract), `rdma-host-shapes.md` (host-shape matrix for case 14), `manual-ssh-verification.md` (VS Code Remote-SSH + `sshfs` runbook complementing case 7), `kind-two-zones.yaml` (4-worker 2-zone Kind config for cases 15 and 23); shared `../_e2e-lib/references/{orchestration,troubleshooting}.md`.

## Cases (locked titles)

Each script in `cases/case-1.sh .. cases/case-38.sh` is self-contained and numbered contiguously (`1..38`, zero gaps; new cases append at `N+1`). Its header (see **Case header contract**) states prerequisites / goal / environment (incl. auto-skip) / inputs (MOCKED vs real) / expected / cleanup. `Needs` summarizes the cluster/hardware/tooling required; exact auto-skip conditions live in each script's `Environment:` header.

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Mutates | Needs |
|---|---|---|---|---|
| `cases/case-1.sh` | CPU-only scheduling chain materializes (zero Cohort, Active `InstanceType`) | always (mandatory) | no | any CPU cluster (1+ Ready node) |
| `cases/case-2.sh` | CPU `Instance` lifecycle, drain, `managed` toggle, portless Service omission, `InstanceTypeFlavor` & queue ownership, CPU awareness, node pinning & volumes | `pkg/worker/controllers/worker/{instance,node_flavor,node_queue,instance_type}.go`, `pkg/worker/extensionapis/worker/instance_type_flavor.go`, `pkg/worker/webhooks/worker/{instance,instance_type}.go`, `pkg/worker/settings/value.go`, `pkg/nodefeature/helper.go`, `deploy/gpustack-operator/chart/values.yaml` | yes (confirm) | any CPU cluster + default StorageClass (2 nodes for pin-choice check; `ssh-keygen` for SSH volume checks) |
| `cases/case-3.sh` | CPU Mock `Detector` + `Allocator` full chain: slice-% webhook, multi-vendor discovery, `AdmissionCheck` hold & release, cross-mode exclusion (`EX`/`SH`/`SL`), fit-label fragmentation skip, accelerated CPU awareness | `pkg/devicemanager/{mockspec,detector/mock,allocator/mock}/**`, `pkg/deviceplugin/**`, `pkg/worker/controllers/worker/{node_devices_admission,node_devices_inflight,node_devices,node_fit_label,fit_demand,node_flavor,instance_type}.go`, `pkg/worker/webhooks/worker/{pod,instance_type,workload}.go`, `pkg/nodefeature/{knowns,fit,helper}.go`, `deploy/gpustack-operator/chart/templates/device-manager/**` | yes (confirm) | any CPU cluster for Phase A; CPU + Mock DM (`deviceManager.mock.enabled=true`, 2 nodes for cross-node fit-label leg, `E2E_MOCK_IMAGE` optional) for Phase B |
| `cases/case-4.sh` | Dynamic Mock fault injection: single-card `unhealthy`, unqueryable/lost-card in-place unhealthy retention & sibling `Index` preservation, total-failure `lastDetected` retention, recovery, clean shrink pruning | `pkg/devicemanager/{mockspec,detector,detector/mock,allocator,allocator/mock}/**`, `pkg/deviceplugin/**`, `pkg/worker/controllers/worker/{node_devices,node_capacity,instance_type}.go` | yes (confirm) | CPU + Mock DM (`deviceManager.mock.enabled=true`, `E2E_MOCK_IMAGE` optional); auto-skips without Mock DM |
| `cases/case-5.sh` | CPU Mock logical-slice packing/co-residency/over-commit guard & virtual MIG/partition mixed-node, live-ledger capacity, exclusion, concurrent multi-profile allocation, visibility co-allocation & reclaim | `pkg/devicemanager/{mockspec,detector/mock,allocator/mock}/**`, `pkg/deviceplugin/{server,resource,budget,controller,reclaim}.go`, `pkg/device/{population,physical_placement}.go`, `pkg/worker/controllers/worker/{node_capacity,node_devices_admission,instance_type}.go`, `pkg/worker/extensionapis/worker/instance.metrics.go` | yes (confirm) | CPU + Mock DM serving `nvidia` (`deviceManager.mock.enabled=true`, `E2E_MOCK_IMAGE` optional); auto-skips without Mock DM |
| `cases/case-6.sh` | `Instance` metrics subresource, Prometheus `/metrics` exporter (single target + accelerated labels), and carved-card-only slice process pass | `pkg/worker/extensionapis/worker/**`, `pkg/kubemetrics/**`, `pkg/devicemanager/{detector/slice.go,detector/snapshot.go,snapshot.go,exporter/**}`, `pkg/utils/datax/snapshot.go`, `api/worker/v1/instance.metrics.go` | yes (confirm) | any CPU cluster (exporter leg needs a running `device-manager` Pod; Phase 2 needs `>=2` logically sliceable cards on Mock or Real DM) |
| `cases/case-7.sh` | Unified SSH `Instance` suite: non-interactive exec + SFTP + TCP forward, exclusive whole-card, logically sliced, and partition-backed sidecar confinement | `pkg/worker/controllers/worker/instance.go`, `pack/ssh-server/**`, `pkg/deviceplugin/server.go`, `pkg/devicemanager/allocator/**`, `pkg/worker/settings/value.go` | yes (confirm) | any CPU cluster + `ssh`, `ssh-keygen`, `sftp` on runner (accelerated legs run on Mock DM or Real GPU pools; `E2E_SSH_IMAGE` / `F11_IMAGE` optional) |
| `cases/case-8.sh` | Real NVIDIA logical slicing: runtime isolation (`libvgpu.so` memory/SM cap), sibling-pool consistency, `Instance` slice webhooks, multi-card packing, single-card co-residency & slice metrics | `pkg/deviceplugin/**`, `pkg/devicemanager/{detector,allocator}/nvidia/**`, `pkg/devicemanager/detector/slice.go`, `pkg/devicemanager/procattr/**`, `pkg/worker/webhooks/worker/{pod,instance}.go`, `pkg/worker/controllers/worker/{node_devices_admission,instance_type}.go`, `pkg/kubemetrics/**` | yes (confirm) | real NVIDIA GPU advertising `*.sliced` (`>=2` cards for packing leg, 1-card pool for co-residency leg) |
| `cases/case-9.sh` | Real NVIDIA MIG hardware lifecycle: enable, carve, mixed node, live capacity ledger, concurrent multi-profile, reclaim, and disable | `pkg/devicemanager/allocator/nvidia/**`, `pkg/devicemanager/detector/nvidia/device.go`, `binding/nvml/**`, `pkg/deviceplugin/{controller,server,resource,reclaim}.go`, `pkg/device/{population,physical_placement}.go`, `pkg/nodefeature/knowns.go`, `pkg/worker/controllers/worker/{node_capacity,node_devices_admission,instance_type}.go` | yes (confirm) | real MIG-capable NVIDIA GPU + node SSH (`MIG_NODE_SSH=<user@host>`) |
| `cases/case-10.sh` | Real Ascend NPU logical-slice placement: claims pack, spill only on a misfit, and never cross into an exclusive card | `pkg/deviceplugin/{server,resource,budget}.go`, `pkg/devicemanager/{allocator,detector}/ascend/**`, `pkg/worker/webhooks/worker/pod.go` | yes (confirm) | real Ascend NPU (`>=3` logically sliceable cards on 1 node, `>=1` free) + CANN image (`E2E_SLICE_IMAGE`) |
| `cases/case-11.sh` | Real AMD ROCm whole-card, logical slicing (`libvrocm.so` + CU mask), two-slice disjoint compute windows, and `Instance` metrics | `pkg/devicemanager/allocator/amd/**`, `pkg/devicemanager/detector/amd/device.go`, `binding/amdsmi/**`, `csrc/amd/rocm-slicing-shim/**`, `pkg/worker/extensionapis/worker/instance.metrics.go` | yes (confirm) | real AMD GPU with logical slicing + ROCm image (`E2E_AMD_IMAGE`) |
| `cases/case-12.sh` | Real T-Head PPU pinned claim (`preferred-accelerator-id`) and logical-slice (`libvppu.so`) runtime memory/compute enforcement | `pkg/devicemanager/allocator/thead/**`, `pkg/devicemanager/detector/thead/device.go`, `binding/hgml/**`, `pkg/deviceplugin/{controller,server}.go`, `csrc/thead/ppu-slicing-shim/**` | yes (confirm) | real T-Head PPU (`>=2` idle cards on 1 node) + `ppu-smi` image (`E2E_SLICE_IMAGE`) + host context or `PPU_NODE_SSH=<user@host>` |
| `cases/case-13.sh` | Real Hygon DCU Multi-Instance partitions: geometry confinement, multi-partition co-residency, multi-card refusal, and reclaim | `pkg/devicemanager/{allocator,detector}/hygon/**`, `pkg/deviceplugin/**`, `pkg/device/physical_placement.go` | yes (confirm) | real Hygon DCU node already in Multi-Instance mode + node SSH (`MIG_NODE_SSH=<user@host>`) |
| `cases/case-14.sh` | Unified NIC/RDMA inventory & `rdma.*` labels, RDMA extended resource sizing, container device-cgroup `open()`/verbs isolation, and host-fabric member `DaemonSet` | `pkg/devicemanager/detector/{network,network_linux,link,detector}.go`, `pkg/deviceplugin/rdma_{server,devices,endpoint,allocate}.go`, `pkg/nodefeature/rdma.go`, `pkg/device/topology_distance.go`, `pkg/worker/kvcache/mooncake/member_workload.go`, `api/worker/v1alpha1/devices.go` | yes (confirm) | any cluster for Leg 1; running `device-manager` (`Devices`) for Leg 2; RDMA endpoint(s) for Legs 3–4 (`E2E_RDMA_IMAGE` optional for `ibv_devinfo`) |
| `cases/case-15.sh` | Topology sources drive Kueue TAS placement, joint `ModelDeployment` admission, and per-card accelerator admission agreement | `pkg/worker/controllers/worker/topology_source*.go`, `pkg/worker/controllers/worker/node_{topology,flavor,queue,devices,devices_admission}.go`, `pkg/worker/controllers/worker/model_deployment{,_joint_admission,_pod_group}.go`, `testing/infra/clusters/eks/**`, `deploy/gpustack-operator/chart/values.yaml` | yes (confirm) | `>=3` zoned Linux worker nodes (`references/kind-two-zones.yaml` or real) + Kueue TAS enabled (Part 2 accelerator leg auto-skips without a free whole accelerator) |
| `cases/case-16.sh` | `KVCachePool` & `KVCachePoolBinding` multi-tenant proportional quota, eviction enforcement (`-1700`), and tenant ledger gate (`-1701`) | `pkg/worker/controllers/worker/kv_cache_pool.go`, `pkg/worker/webhooks/worker/kv_cache_pool{,_binding}.go`, `pkg/worker/kvcache/mooncake/**`, `api/worker/v1alpha1/kv_cache_pool{,_binding}.go` | yes (confirm) | any CPU cluster + pullable CPU Mooncake Lease-enabled store image (`E2E_MOONCAKE_IMAGE`) |
| `cases/case-17.sh` | Replicated HA `KVCacheBackend` Kubernetes `Lease` leader election, RBAC minimality, graceful/hard failover, snapshot refusals, `1 -> 3` leader preservation, and rollout/rollback | `pkg/worker/kvcache/mooncake/{leader_workload,ha_rbac,leader_flags,keys}.go`, `pkg/worker/controllers/worker/kv_cache_backend_{handover,rollout}.go`, `pkg/worker/webhooks/worker/kv_cache_backend.go` | yes (confirm) | any CPU cluster + pullable Mooncake store & rollout image pair (`E2E_MOONCAKE_IMAGE`, `E2E_MOONCAKE_ROLLOUT_IMAGE`) |
| `cases/case-18.sh` | Plain `Deployment`, bare `Pod`, & `LeaderWorkerSet` KV cache injection webhook (vLLM / SGLang / vLLM-Ascend, opt-out byte-identity, actionable refusals) | `pkg/worker/webhooks/worker/pod_kv_cache*.go`, `pkg/worker/kvcache/inject/**`, `pkg/worker/webhooks/worker/kv_cache_backend.go` | yes (confirm) | any CPU cluster + Mooncake store image (`E2E_VLLM_IMAGE`, `E2E_SGLANG_IMAGE`, `E2E_VLLM_ASCEND_IMAGE`, and `LeaderWorkerSet` CRD legs auto-skip individually when absent) |
| `cases/case-19.sh` | Mooncake member local disk eviction tier (`master_allocated_file_size_bytes` > 0 + host disk files + byte-identical evicted readback) | `pkg/worker/kvcache/mooncake/{member_workload,leader_flags}.go`, `api/worker/v1alpha1/kv_cache_backend.go` | yes (confirm) | CPU cluster with writable `E2E_LOCALDISK_HOST_PATH` (default `/mnt/kvcache-localdisk`) on every Ready node + `E2E_MOONCAKE_IMAGE` |
| `cases/case-20.sh` | `ModelArtifact` Hub resolution (public/private/gated/revoked/terminating/secret redaction) + consumer claim placement affinity, pinned `Engine` argv, and `Instance` PV placement | `pkg/modelartifact/**`, `pkg/worker/controllers/worker/{model_artifact,model_artifact_placement,model_deployment_artifact,instance,instance_persistent_volume}.go`, `pkg/worker/webhooks/worker/{model_artifact,model_deployment,instance}.go`, `pkg/worker/settings/value.go` | yes (confirm) | 2 schedulable CPU workers + Hub egress + `git`, `curl`, `python3` on runner (`HF_TOKEN_READONLY` optional for private/revocation rows) |
| `cases/case-21.sh` | Node model-cache CSI delivery: cold mount dedup, `restricted` PSA namespace, forged attributes, corrupt rejection, crash resume, status guards, HTTPS proxy, plugin rollout, GC eviction & restart reconciliation | `pkg/modelmanager/**`, `pkg/worker/controllers/worker/{model_deployment_artifact,model_artifact_placement,node_model_store,node_model_store_kubelet,instance}.go`, `pkg/worker/webhooks/worker/{node_model_store,model_artifact_status}.go`, `deploy/gpustack-operator/chart/templates/model-manager/**` | yes (confirm) | 2 Kind/CPU workers + `modelManager.enabled` + stock `python` image + `openssl` (`docker` access to Kind nodes for GC/Node rows; `E2E_C21_OFFLINE=1` skips public Hub row) |
| `cases/case-22.sh` | Download progress reporting (30s/5% thresholds, v1 `progress` subresource RBAC), `ModelPrefetch`, expected-digest anchor, and node-to-node peer sync (zero hub bytes, `NetworkPolicy` isolation) | `pkg/modelmanager/{report,materialize,download,peer}/**`, `pkg/modelmanager/{downloads,config}.go`, `pkg/worker/controllers/worker/{model_artifact,model_artifact_nodes,model_prefetch,model_store,model_store_binding}.go`, `pkg/worker/extensionapis/worker/{model_artifact,model_artifact.progress,node_model_store}.go`, `pkg/worker/webhooks/worker/{model_artifact,model_store_binding,model_prefetch}.go`, `deploy/gpustack-operator/chart/templates/model-manager/**` | yes (confirm) | 2 CPU workers + `modelManager.enabled` (non-zero `modelManager.port` + peer `NetworkPolicy`) + stock `python` & `busybox` images (`E2E_OFFLINE=1` skips peer legs) |
| `cases/case-23.sh` | Node-cache placement preference through Kueue TAS (`TASRespectNodeAffinityPreferred`, full-node fallback, zero rollout on cache growth) | `pkg/worker/controllers/worker/{model_placement_preference,model_artifact_placement,model_deployment,instance}.go`, `deploy/gpustack-operator/chart/values.yaml` | yes (confirm) | 2+ same-CPU-model Kind/CPU workers (`references/kind-two-zones.yaml` 4 workers ideal) + `modelManager.enabled` + stock `python` image |
| `cases/case-24.sh` | OCI image-volume `ModelArtifact` source (tag-only digest refusal, claim-shaped resolution, read-only Kubernetes image volume mount, `ModelPrefetch` refusal) | `pkg/worker/webhooks/worker/{model_artifact,model_prefetch}.go`, `pkg/worker/controllers/worker/{model_artifact_placement,model_deployment_artifact,instance,model_placement_preference}.go`, `pkg/kubediscovery/feature.go` | yes (confirm) | Kind/CPU worker with kubelet 1.35+ & containerd 2.1+ image volumes + `registry:2` & `python:3.12-slim` pullable + `python3` on runner |
| `cases/case-25.sh` | ModelScope hub source resolution (`git ls-remote` cross-check), cold node materialization, peer sync, and pinned SDK download | `pkg/modelartifact/modelscope.go`, `pkg/worker/controllers/worker/model_artifact.go`, `pkg/modelmanager/{driver/authorize.go,materialize/materialize.go,report/report.go}`, `pkg/worker/webhooks/worker/model_artifact.go` | yes (confirm) | 2 CPU workers + `modelManager.enabled` + stock `python` image + egress to `www.modelscope.cn` & `pypi.org` (`E2E_C25_OFFLINE=1` skips) |
| `cases/case-26.sh` | Unified `ModelDeployment` admission refusal matrix: single-role, P/D, `engine.image` schema, SGLang External DP `RouterRequired`, EPD encoder matrix, and `runtimeControl` 15-key validation & `scaler-binding` | `api/worker/v1alpha1/model_deployment.go`, `pkg/worker/runtimecontrol/translator.go`, `pkg/worker/webhooks/worker/model_deployment{,_epd,_elastic,_scaler}.go`, `pkg/worker/controllers/worker/model_deployment{,_binding,_pod_group,_connector_epd,_parallelism}.go` | yes (confirm) | any CPU cluster with `ModelDeployment` & `ModelDeploymentScaler` CRDs + `jq` |
| `cases/case-27.sh` | `ModelDeployment` rendered topology & ownership: KV reuse domain isolation, unreachable `Binding` degradation, routed P/D 6-object router lifecycle, `KVEventsPublishing`, EPD encoder `Service` & picker stage | `pkg/worker/controllers/worker/model_deployment_{connector,binding,cache_attached,router,kv_events,service,status,render}.go`, `pkg/worker/extensionapis/worker/model_deployment.go`, `pkg/worker/kvcache/{inject,router}/**` | yes (confirm) | any CPU cluster + pullable Mooncake store image + `jq` (`pyyaml` for EPD picker config rows; `E2E_C27_WITNESS_IMAGE` default `busybox:1.36`) |
| `cases/case-28.sh` | Multi-role & P/D/EPD Kueue joint admission: per-replica `Workload`s, `Service` watch recreation, short-pool whole-set starvation, authored `InstanceType` (`instance-type-derived-from-node=false`), two-type barrier & parking | `pkg/worker/controllers/worker/model_deployment{,_pod_group,_service,_status,_joint_admission,_rollout}.go`, `pkg/worker/controllers/worker/node_queue.go`, `pkg/worker/kuberess/apps_kueue_admission_check.go`, `pkg/worker/webhooks/worker/model_deployment{,_epd}.go` | yes (confirm) | any CPU cluster with Kueue pod integration + pool's entrance `LocalQueue` in `<NS>` |
| `cases/case-29.sh` | `ModelDeployment` role scaling, surplus retirement finalizer release, multi-Pod `size` PodSet admission, and ordinary role update whole-group replacement | `pkg/worker/controllers/worker/model_deployment{,_initial_queue,_replacement_slot,_pod_group,_rollout,_render,_service}.go`, `pkg/worker/webhooks/worker/model_deployment.go`, `api/worker/v1alpha1/model_deployment.go` | yes (confirm) | any CPU cluster with entrance `LocalQueue` in `<NS>` (room for 4 placeholder Pods); Phase 2 role-shape replacement uses Mock or Real NVIDIA `AcceleratorType` (`onceMaxRequest=1`) |
| `cases/case-30.sh` | Managed role host compilation cache mount, user `additionalVolumes` yield, `startupTimeoutSeconds` bounds, startup-window `Revoked` hold, and optional warm-restart cache reuse | `pkg/worker/controllers/worker/model_deployment_{compile_cache,render,health}.go`, `api/worker/v1alpha1/model_deployment.go` | yes (confirm) | any CPU cluster (rendered-Pod rows need usable `AcceleratorType` + `LocalQueue`; warm-restart leg needs real GPU + `E2E_C30_ENGINE_IMAGE` + `E2E_C30_MODEL`) |
| `cases/case-31.sh` | `ModelDeploymentScaler` admission, defaulting, single-writer fence, binding/suspend/term lifecycle, `capacityPrior` defaulting, `priors.json` fallback, Priority 1/2 whole-unit resolution, EPD coupling & `ConfigMap` freeze | `api/worker/v1alpha1/model_deployment_scaler.go`, `pkg/worker/webhooks/worker/{model_deployment_scaler,model_deployment}.go`, `pkg/worker/controllers/worker/model_deployment_scaler{,_priors,_prior_identity,_prior_lifecycle}.go` | yes (confirm) | any CPU cluster with `ModelDeploymentScaler` CRD + `jq` (target & `capacityPrior` rows use Mock or Real `ascend`/`nvidia`/`amd` `AcceleratorType` with non-empty `product`) |
| `cases/case-32.sh` | Live routed P/D serving over direct KV leg + continuous-traffic router/role scaling with zero dropped requests | `pkg/worker/kvcache/inject/{vllm,sglang,types}.go`, `pkg/worker/controllers/worker/model_deployment{,_render,_router,_pod_group}.go`, `pkg/worker/kvcache/router/**` | yes (confirm) | 2-node real GPU cluster with Ready routed P/D `ModelDeployment` + `E2E_PD_PROBE_IMAGE` + `E2E_PD_MODEL` (pass target `none` for read-only Phase 1) |
| `cases/case-33.sh` | Live `ModelArtifact` serving: PVC claim + gated commit download, node-cache 7B zero-byte warm restart, and `cache_prefix` KV weight isolation | `pkg/modelmanager/**`, `pkg/worker/controllers/worker/{model_deployment_artifact,model_deployment_render,model_artifact_placement,model_deployment}.go`, `pkg/worker/kvcache/inject/{vllm,types}.go` | yes (confirm) | real GPU cluster + Hub egress (`HF_TOKEN_READONLY` for gated sub-leg; `model-artifact-delivery-mode=Node` + ~30 GB cache space for 7B leg; `E2E_C33_MEMBER_NODE` for KV isolation leg) |
| `cases/case-34.sh` | Live multimodal EPD serving behind `llm-d-router` + vLLM: NIXL encoder-cache leg on NVIDIA/AMD, and shared-directory RWX leg on Ascend | `pkg/worker/kvcache/inject/vllm_ec.go`, `pkg/worker/controllers/worker/model_deployment_connector_epd.go`, `pkg/worker/webhooks/worker/model_deployment_epd.go` | yes (confirm) | real NVIDIA, AMD, or Ascend GPU pool (`>=3` free exclusive cards, `E2E_C34_ACCELERATOR_TYPE`) + vision-language model weights + vLLM/`llm-d-router` images (`E2E_C34_CLAIM` RWX PVC on Ascend) |
| `cases/case-35.sh` | Live `ModelDeploymentScaler` active scaling under real load: proportional running-requests single-role scaling + disaggregated P/D `TokenThroughput` `capacityPrior` (`1P:2D -> 2P:4D -> 1P:2D`) | `pkg/worker/controllers/worker/model_deployment_scaler{,_sampler,_planner,_priors}.go`, `pkg/worker/scaling/algorithm/{proportional,token_throughput}.go`, `pkg/worker/controllers/worker/model_deployment{,_health,_router_observation}.go` | yes (confirm) | real GPU cluster: Phase 1 uses caller `<MD_NAME>` or `E2E_C35_ENGINE_IMAGE` + `E2E_C35_WEIGHTS`; Phase 2 requires `>=6` free GPUs + pre-warmed `ModelArtifact` (`E2E_C35_ARTIFACT`, default `qwen3-8b`) |
| `cases/case-36.sh` | Live online `runtimeControl` on vLLM & SGLang: `gpustack-engine-sentinel`, probes & ACK latch, Envoy allowlist/observer, live tuning & reset, role `pause`/`RolePaused`/resume, `PreStop` drain, and P/D, PP=2 & speculative knobs | `engine-sentinel/python/**`, `pkg/worker/runtimecontrol/**`, `pkg/worker/controllers/worker/model_deployment{,_render,_drain,_router,_runtime_control,_service,_scaler_planner}.go` | yes (confirm) | real NVIDIA GPU cluster (`>=1` GPU for core legs, `>=2` GPUs for P/D & PP=2 legs) + pre-warmed `ModelArtifact` (`E2E_C36_ARTIFACT`, default `qwen3-8b`) + vLLM & SGLang runner images |
| `cases/case-37.sh` | Live folded Elastic EP (`runtimeControl.parameters["elastic.dataParallelWidth"]`): `2 -> 3` in-place expansion preserving surviving master Pod UID and post-resize parameter replay | `pkg/worker/runtimecontrol/translator.go`, `pkg/worker/controllers/worker/model_deployment_elastic_*.go`, `pkg/worker/controllers/worker/model_deployment_runtime_control.go` | yes (confirm) | real NVIDIA GPU cluster (`>=3` allocatable GPUs) + pre-warmed MoE `ModelArtifact` (`E2E_C37_ARTIFACT`, default `deepseek-v2-lite-chat`) + vLLM runner image |
| `cases/case-38.sh` | `ModelDeploymentForger` & `ModelKernelArtifact`: `ConfigMap`/cross-ns/cross-class refusals, 2-node mock-Forge closed loop (probe, `ForgeSandbox`, `/ingest/v1/trees`, pin GC immunity, Peer Sync, CSI mount, canary rollout, gen-2 unpin, rollback & quarantine), and live GPU leg | `api/worker/v1alpha1/model_{kernel_artifact,deployment_forger}.go`, `pkg/modelkernelkey/**`, `pkg/modelforger/**`, `pkg/modelmanager/{ingest,driver,gc}/**`, `pkg/worker/controllers/worker/{model_deployment_forger*,model_deployment_kernel*,model_kernel_artifact*,node_model_store_pins*}.go`, `pkg/worker/webhooks/worker/model_{kernel_artifact,deployment_forger,deployment}*.go`, `engine-sentinel/python/**` | yes (confirm) | any CPU cluster for admission refusals; 2+ workers + CSI + `LocalQueue` + Mock DM (or `>=3` cards) for Leg A2; real GPU cluster + pre-warmed `ModelArtifact` + `E2E_C38_RUNNER_IMAGE` + engine image for Leg B |

Each note below is something the **lead** must act on before or around a run. What a case *does* — its goal, environment, inputs, assertions and cleanup — lives in its own header, which the **Case header contract** below requires to be readable on its own; the index never restates it.

> **The `Needs` column is the one thing the index duplicates from a case header, so it is the one thing that can go stale.** Change what a case requires and you must change that column in the same commit.

- **CPU-only Mock Device Manager verification (`cases/case-3.sh` .. `cases/case-7.sh`).** Install or upgrade the Helm chart on a CPU-only Kind or k3s cluster with:
  ```bash
  helm upgrade --install gpustack-operator deploy/gpustack-operator/chart -n "$NS" \
    --set image.tag="$TAG" --set image.pullPolicy=IfNotPresent \
    --set deviceManager.mock.enabled=true \
    --set deviceManager.mock.configMapName=gpustack-mock-devices \
    --set deviceManager.mock.omitRuntimeClass=true
  ```
  Pre-create `ConfigMap` `gpustack-mock-devices` with a `devices.yaml` key (for example `preset: nvidia-h100`). Cases 3, 4, 5, and 7 snapshot that `ConfigMap` via `cases/_mock-dm-lib.sh`, hot-reload per-node multi-vendor presets, virtual MIG profiles (`nvidia-h100-mig`), and fault states (`unhealthy`, `queryState: unqueryable`, `totalFailure`) without restarting `device-manager` Pods, and restore the exact `ConfigMap` bytes in `trap … EXIT`. Mock mode also unlocks target-dependent `ModelDeployment` / `ModelDeploymentScaler` checks in cases 29 and 31 on CPU-only clusters.
- **Two disjoint accelerator families, two disjoint card populations.** *Logical slicing* (software; the vendor preload library on real hardware, or virtual slice accounting in Mock DM) is `<base>.sliced` plus its sizing keys, served **only** by a card that is not in a hardware partitioning mode. *Physical partitioning* (hardware; NVIDIA MIG / Hygon Multi-Instance, or virtual MIG in Mock DM) is `<base>.partitioned` plus one `<base>.partitioned.<kind>-<profile>` key per profile, served **only** by a card that is. A card serves exactly one family — which is why `InstanceType` carries four views (`Accelerator(EX/SH/SL/PT)`), and why a case deploying a logical slice must select a pool with a non-zero *logical* slice count, never merely a "sliceable" one. Normative reference: [`docs/modules/devices/requests.md`](../../../docs/modules/devices/requests.md).
- **Ask whether a pool BACKS the node; never match its rendered name.** The webhook stamps each `InstanceType` with its own schedule labels — `nodefeature.PoolScheduleLabels`, via `pkg/worker/webhooks/worker/instance_type.go` — expressly so it is selectable by the same discriminators its `Devices` and `ResourceFlavor`s carry (`cases/_pool-lib.sh`'s `discover_node_pool` implements this predicate):

  ```python
  d = {k: v for k, v in pool_labels.items() if not k.startswith('schedule.gpustack.ai/')}
  backs = bool(d) and all(node_labels.get(k) == v for k, v in d.items())
  ```

  Add `acceleratorGroup.endswith('-' + groupID)` as an anchored suffix only when disambiguating multiple accelerator groups on one node — never match substrings (`a10` matches `a100`, `h20` matches `h200`), never ignore `os`/`arch`, and never ignore `general.feature.gpustack.ai/*` when `instance-type-aware-cpu-manufacturer` is enabled.
- **CASES 9, 12, and 13 need a node SSH address supplied at run time.** Cases 9 and 13 read `MIG_NODE_SSH=<user@host>`; Case 12 reads `PPU_NODE_SSH=<user@host>` when not running directly on the accelerator host. Each **exits 2 (input required)** when its SSH address is unset — ask the user and pass it inline at run time, never hardcoded in a file. All **auto-skip (exit 0)** when the target vendor hardware itself is absent.
- **Real-vendor cases require vendor runtime tooling inside the workload image:** Case 8 needs CUDA/`nvidia-smi` (`E2E_SLICE_IMAGE`), Case 10 a CANN-family image (`E2E_SLICE_IMAGE`), Case 11 a ROCm-family image (`E2E_AMD_IMAGE`), Case 12 an image shipping `ppu-smi` (`E2E_SLICE_IMAGE`), and Case 13 a DTK/Hygon image (`E2E_PARTITION_IMAGE`). Pre-pull large vendor images on the node so claims do not spend their readiness window pulling.
- **Drive hardware-partition verification with `cases/run-partition-block.sh` and RDMA verification with `cases/run-rdma-block.sh`.** On CPU Mock DM clusters `run-partition-block.sh` runs `5 7`; with `MIG_NODE_SSH=<user@host>` it snapshots initial MIG modes, runs `9 7 13` (enabling MIG before Case 7's SSH partition leg when needed), and restores initial MIG modes on exit. `run-rdma-block.sh --report-only [NS]` prints the cluster's RDMA host-shape capability matrix before running Case 14 (`references/rdma-host-shapes.md`).
- **Run `ModelDeployment` cases (20, 26–31) in a namespace carrying the pool's entrance `LocalQueue`**, which routes pod groups into the pool's `ClusterQueue` (`kubectl -n <NS> get localqueue` before deciding; system namespaces such as `gpustack-system` intentionally omit user entrance `LocalQueue`s). `ModelDeployment` cleanup traps always call `md_force_release` to clear Kueue `Workload`s owning serving Pod groups so a test failure never strands subsequent cases.

## Case header contract

Every `cases/case-N.sh` opens with a header describing the case **on its own terms — no spec anchors** (no Story/Task numbers, F-codes or `specs/*.md` paths; no commit hashes from repository history — an input artifact's own revision identifier, such as a weight revision under test, is an input, not an anchor). A reader must understand what the case does, needs, and asserts from the header alone. New/edited cases follow this seven-field template:

```
# CASE N — <one-line behavior title>   (<mutation posture>)
#
#   case-N.sh <NS>
#
# Prerequisites: nodes=<N>; gpu=<vendor|none|any>; features=<mig,partition,rdma,efa,multi-host,kind,mooncake-store>; cluster=<real|kind|any>; inputs=<ENV_VAR_NAMES>
# Goal:        <the one contract/behavior this case proves>
# Environment: <what the cluster must provide + when it AUTO-SKIPS>
# Inputs:      <what the case creates / injects / toggles; mark MOCKED inputs vs the real thing verified>
# Expected:    <the PASS assertions — the observable final state>
# Cleanup:     <what the trap restores — INCLUDING any environment baseline the case changed
#               (editable settings, managed toggles, node/NodeFeature labels, mock ConfigMap);
#               idempotent, runs on pass AND fail, safe to re-run>
```

- **Mutation posture** rides the title line, and every case's title line carries one: `MUTATING, self-recovering` (writes, then a trap restores), `MUTATING, self-cleaning` (writes, then removes what it wrote), `NON-MUTATING` (creates nothing outside its own report tree) or `READ-ONLY` (no trap). Append `AUTO-SKIPS without <requirement>` when the case self-skips (real hardware, Mock DM, `ssh` client, ≥2 logically sliceable cards, …).
- **Prerequisites** is the first field, one line, and it is the machine-readable gate an agent scans before reading anything else: `nodes=` the count of Ready worker nodes the case needs, `gpu=` the accelerator vendor families it can run against (`none` when it needs none, `any` for whatever the cluster provides, several families separated by `|`), `features=` drawn from the closed set `mig, partition, rdma, efa, multi-host, kind, mooncake-store`, `cluster=` the cluster shape (`real`, `kind` or `any`), and `inputs=` the environment variables that gate the case's behavior. `cases/_env-audit.sh <NS>` reads this line out of every case (`1..38`) and reports, per case, whether the cluster in front of it satisfies it.
- **Environment** always names the auto-skip condition so the run/skip decision is readable without executing the case.
- **Inputs** distinguish **MOCKED** inputs (declarative Mock DM `ConfigMap` presets, `_model-hub.py` fixtures, CPU-vendor `NodeFeature` overrides) from the **real** objects under test — the mock is a fixture, never the verification.
- Describe behavior in **plain terms**, not spec reference; the case↔code mapping lives in the table's "Run when these change" column.
- **Restore the baseline, pass or fail.** A case mutating shared baseline (an editable Setting, a `gpustack.ai/managed` toggle, an injected node/`NodeFeature` label, or the Mock DM `ConfigMap`) MUST restore it in `trap … EXIT` (runs on pass AND fail) and wait for the chain to settle back. Between cases the lead re-confirms the baseline (awareness off, all nodes managed, mock `ConfigMap` restored, no leftover test objects) and restores it if a case died before its trap ran — a mutated baseline is a frequent cause of a *spurious* failure in the NEXT case.
- Runtime output (results banner, `record` messages, FAIL-footer) stays spec-anchor-free and self-explanatory.

## Flow

Generic phase semantics live in [`orchestration.md`](../_e2e-lib/references/orchestration.md); below are the operator-e2e specifics. Let `RPT=.claude/reports/$(date +%F)-operator-e2e`.

**Phase 0 — Environment (read-only; provisioning only on opt-in).** Preflight, offer the user's own contexts, confirm the target, record drift:

```bash
bash .claude/skills/_e2e-lib/scripts/preflight.sh
kubectl config get-contexts
git rev-parse HEAD; git diff --stat origin/main...HEAD
```

**A cluster the user already has is the default** — enumerate the contexts, offer them, and do not continue until the user confirms which one this run verifies against (`kind` / `k3s` / `docker-desktop` / any reachable context). Then `mkdir -p "$RPT"/raw` and write the report header (incl. cluster provenance) + node inventory, and run `bash .claude/skills/gpustack-operator-e2e/cases/_env-audit.sh gpustack-system` to inspect per-case cluster eligibility.

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

**Context checkpoints.** This suite runs for hours. At each trigger point — after Phase 3, after each block of ~3–4 cases (or at the end of a domain block), before a Phase 5 triage or a Phase 7 loop, and **always before Phase 8** — pause between cases, bring `run-log.md` up to date, tell the user plainly that compacting now would protect the rest of the run, and emit the ready-to-use **compact focus block** (contents + worked example in [`orchestration.md`](../_e2e-lib/references/orchestration.md)). Compacting is the user's action. The block must carry the cluster's provenance and the teardown obligation above everything else: the worst outcome of a compaction is a lead that forgets it provisioned a paid cluster.

**Phase 1 — Test-item list.** Match `git diff --name-only origin/main...HEAD` against the 38-case table; also cross-check the `### User Stories` of the `specs/*.md` files the diff touches, newest first, for items **beyond** the table. Confirm with the user (`AskUserQuestion`); CASE 1 always included. No drift → ask for new items, invoking `interview-me` if the ask is too coarse.

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

`E2E_BUILDER_SSH` and `E2E_IMAGE_NAMESPACE` are consumed by `build-load.sh`, never by a case: no `cases/case-*.sh` reads either name. Setting them changes how the image is built and pushed, not what any case asserts.

Redeploy over an existing release with a new image (avoids a full reinstall):

```bash
helm upgrade gpustack-operator deploy/gpustack-operator/chart -n "$NS" \
  --reset-then-reuse-values --set image.tag="$TAG" --set image.pullPolicy=IfNotPresent
kubectl -n "$NS" rollout restart deploy/gpustack-operator-worker
kubectl -n "$NS" rollout status  deploy/gpustack-operator-worker --timeout=300s
```

`assert-core.sh` requires the **running binary revision == HEAD** (stale-image guard).

**Phase 4 — Execute + analyze.** CASE 1 read-only (no prompt); CASE 2+ mutate and self-recover, so confirm each. Per the rendezvous rule: run a case → capture to `raw/NN-caseN.txt` → let it converge → **then** fan out that case's specialists (one message, multiple `Agent` calls) on the snapshot → collect verdicts → write the execution record → only then the next mutating case.

```bash
bash .claude/skills/gpustack-operator-e2e/cases/case-1.sh "$NS" 2>&1 | tee "$RPT"/raw/10-case1.txt   # mandatory, read-only
# then, per the picked cases (each confirmed), e.g.:
bash .claude/skills/gpustack-operator-e2e/cases/case-2.sh "$NS" 2>&1 | tee "$RPT"/raw/11-case2.txt
bash .claude/skills/gpustack-operator-e2e/cases/case-3.sh "$NS" 2>&1 | tee "$RPT"/raw/12-case3.txt
# … case-4 … case-38 (real-hardware cases 8–13 and 32–37 auto-skip without physical accelerators; case-38 Leg B auto-skips)
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

**Phase 7 — Fix & retest / Suite upgrade (optional).** Per `orchestration.md`, two user-gated branches: (a) real bugs → confirm, choose local vs remote (SSH/RSYNC) packaging (record connection info), fix → `make lint` → signoff commit → package → `helm upgrade` → retest → append; (b) sink the self-upgrade backlog into the suite (new/adjusted `cases` contiguously numbered `1..N`, tightened assertions, case-table/`references` updates) or this protocol — confirm each, signoff commit, retest any changed case.

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
- `references/drain-recycle.md` — why CASES 2–5 need a real cluster (the fake-client blind spots), the `gpustack.ai/managed` drain-and-recycle path in CASE 2, and how CPU-only Mock Device Manager (`_mock-dm-lib.sh`) drives CASES 3–5 end to end.
- `references/rdma-host-shapes.md` — which host shape answers each RDMA reading in CASE 14, why each gate is written as a positive discriminating reading rather than "the cluster has RDMA", and the environment variables and image packages CASE 14 reads.
- `references/manual-ssh-verification.md` — the manual pass CASE 7 cannot drive in CI: a real VS Code Remote-SSH session (workspace opens, integrated terminal in `main`) and an `sshfs` mount round-trip.
- `../_e2e-lib/references/troubleshooting.md` — shared image/rollout/teardown failure modes.
