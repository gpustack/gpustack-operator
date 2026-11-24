# Spec: CPU-Only Mock Device Manager, E2E Consolidation, and Chaos Scheduling Skill

Status: Building
Type: Feature

## Summary

Add an opt-in, CPU-only Mock `Detector` and Mock `Allocator` mode to `gpustack-operator device-manager` driven by a hot-reloaded declarative YAML/JSON hardware specification with built-in multi-vendor hardware presets and per-node overrides, enabling full end-to-end verification of `NodeFeature`, `Devices`, Kueue pool queues, `AdmissionCheck`, kubelet `DevicePlugin` allocation across all five modes, virtual hardware partitioning, and dynamic card fault/unhealthy-retention lifecycles on local or remote CPU-only Kind clusters with zero vendor drivers or host device nodes.
In the same effort, audit and consolidate the 90 non-contiguous end-to-end test scripts in [`.agents/skills/gpustack-operator-e2e/SKILL.md`](../.agents/skills/gpustack-operator-e2e/SKILL.md) (`case-1.sh` through `case-137.sh`, carrying 47 numbering gaps) into 37 contiguous scripts (`case-1.sh` through `case-37.sh`) guarded by a mechanical contiguity and index-sync gate, and introduce a dedicated explicit-only `gpustack-operator-e2e-chaos` skill for multi-node scale scheduling, admission gating, fault injection, and control-loop resilience testing on any given Kubernetes cluster.

## Motivation

### Goals

1. **Run the real `device-manager` → `worker` → Kueue → kubelet `DevicePlugin` scheduling chain on CPU-only clusters**:
   - Today, accelerated E2E cases either hand-craft fake `NodeFeature` and `Devices` objects (bypassing `device-manager` and kubelet `DevicePlugin` entirely) or require scarce physical GPU/NPU clusters.
   - An explicit opt-in mock `Detector` and mock `Allocator` inside `gpustack-operator` exercise the exact production `Detector` loop ([`detector.go`](../pkg/devicemanager/detector/detector.go)), `Allocator` lifecycle ([`allocator.go`](../pkg/devicemanager/allocator/allocator.go)), `pkg/deviceplugin.ResourceServer` ([`server.go`](../pkg/deviceplugin/server.go)), `DevicesReconciler`, `Exporter`, and `worker` controllers without touching vendor CGO bindings or `/dev/*` nodes.
2. **Ship realistic multi-vendor hardware presets and per-node dynamic fault injection**:
   - Built-in presets modeled after real multi-vendor hardware captures from [gpustack/runtime](https://github.com/gpustack/runtime) (NVIDIA H100/H100-MIG/H200/RTX 4090 48G/RTX 5090D/GB10, AMD MI300X/MI308X/RX 7800 XT, Ascend 910B2/310P3, Hygon K100-AI, MetaX C500, MThreads S5000, T-Head PPU, plus Cambricon and Iluvatar profiles) let developers reproduce realistic heterogeneous pools, PCI/NUMA/Fabric topologies, logical slicing, and MIG geometries without hand-writing verbose specs.
   - Supporting per-node overrides (`nodes.<nodeName>` with `default` fallback) inside a single hot-reloaded spec file or `ConfigMap` lets multi-node Kind clusters assign diverse hardware per node and inject targeted single-card `unhealthy`, `unqueryable` (lost card with in-place unhealthy retention and sibling `Index` preservation), `totalFailure`, `initError`, recovery, and hot-shrink events via one `ConfigMap` update.
3. **Eliminate E2E suite sprawl, overlap, and numbering drift**:
   - `.agents/skills/gpustack-operator-e2e/cases/` currently holds 90 scripts spanning `case-1.sh` to `case-137.sh` with 47 missing numbers and repeated overlap across admission, slicing, MIG, SSH, KV cache, ModelArtifact, and live serving families.
   - Removing 13 obsolete/redundant scripts, merging overlapping families, adding 3 CPU-mock E2E cases, and renumbering strictly `case-1.sh` through `case-37.sh` with a mechanical contiguity gate keeps the suite lean, discoverable, and maintainable.
4. **Add a dedicated `gpustack-operator-e2e-chaos` skill for scale scheduling, gating, and control-loop chaos**:
   - Local Kind clusters (2–4 mock workers) and large remote Kind/k3s hosts (8–32+ mock workers) can exercise high-concurrency heterogeneous scheduling storms, checkerboard fragmentation and fit-label gating, mid-flight card loss/recovery, and controller/DeviceManager restarts to expose subtle race conditions and reconcile-loop regressions before they reach physical GPU clusters.

### Non-Goals

- Replacing real-hardware runtime qualification for vendor preload libraries (`libvgpu.so`, `libvrocm.so`, `libvppu.so`), kernel device cgroups, real NVML/DCMI/ROCm/HGML/DMI/CNDEV drivers, or real LLM inference engines (`vLLM`, `SGLang`, `vLLM-Ascend`). Real-hardware cases remain in Domains 2 and 7 of the consolidated suite.
- Simulating CUDA/ROCm/CANN compute kernels or GPU memory allocation inside workload containers.
- Exposing an unauthenticated HTTP mutation API on `device-manager` for fault injection; file/ConfigMap hot-reload is the sole runtime control plane.
- Rewriting or renumbering `.agents/skills/gpustack-operator-chart-e2e/`, `.agents/skills/gpustack-operator-benchmark/`, or `.agents/skills/gpustack-operator-xbuild-and-verify/`.

## Proposal

Introduce a declarative CPU-only mock provider behind `device.Detector` and `device.Allocator` with built-in multi-vendor presets, per-node selectors, and optional Helm chart support; consolidate and contiguously renumber `.agents/skills/gpustack-operator-e2e/` (`90 → 37` cases with a lint gate); and ship a new explicit-only `.agents/skills/gpustack-operator-e2e-chaos/` skill for multi-node scheduling, gating, and failure-injection verification.

### User Stories

#### Story 1
As a GPUStack Operator developer, I want to run real `device-manager` (`detect`, `monitor`, `serve`) and `worker` scheduling chains on a local CPU-only Kind cluster, so that I can verify `Devices`, `NodeFeature`, Kueue queues, `AdmissionCheck`, and kubelet `DevicePlugin` allocation across `exclusive`, `shared`, `sliced`, `partitioned`, and `visibility` modes without physical GPUs.

#### Story 2
As a test engineer, I want to pick from built-in multi-vendor hardware presets or declare custom cards, PCI/NUMA/Fabric topology, logical/MIG profiles, and per-node mappings in one YAML/JSON file or `ConfigMap`, so that I can spin up heterogeneous multi-node Kind clusters quickly and reproducibly.

#### Story 3
As a reliability engineer, I want to inject single-card unhealthy flags, unqueryable/lost cards, total query failures, transient driver init errors, recovery, and clean card removal by patching a mounted `ConfigMap`, so that I can verify health retention, index preservation, and scheduler isolation end to end without breaking real hardware.

#### Story 4
As an operator maintainer, I want a pruned, non-overlapping, contiguously numbered E2E suite (`case-1.sh` through `case-37.sh`) guarded by an automated index-contiguity check, so that obsolete cases do not rot in the tree and new cases follow a clear auto-incrementing convention.

#### Story 5
As a scheduling and stability engineer, I want a dedicated `gpustack-operator-e2e-chaos` skill that drives high-concurrency scheduling storms, fragmentation/gating races, card failure injection, and controller/DeviceManager restarts against any local or remote Kind/k3s cluster, so that I can verify reconcile-loop convergence and admission invariants at scale.

### Core Features & Acceptance Criteria

#### Feature 1 — Declarative Mock Hardware Presets, Per-Node Spec & Fault Injection

1. **Activation**: Opt-in via `--mock-devices-file <path>` on `device-manager` (`serve`, `detect`, `monitor`) or `GPUSTACK_MOCK_DEVICES_FILE=<path>` environment variable (CLI flag overrides env var). Default is empty (disabled).
2. **Schema & built-in presets**: Supports YAML and JSON declaring:
   - Either a flat node spec or a multi-node map (`default` + `nodes.<nodeName>` matched against `KUBERNETES_NODE_NAME`), so one shared `ConfigMap` configures heterogeneous nodes and targeted per-node faults.
   - Named hardware `preset` references (with optional field/count/fault overlays) backed by committed built-in presets in `pkg/devicemanager/mockspec/presets/` covering all nine manufacturers (modeled on real hardware captures: NVIDIA H100, H100-MIG, H200, RTX 4090 48G, RTX 5090D, GB10; AMD MI300X, MI308X, RX 7800 XT; Ascend 910B2, 310P3; Hygon K100-AI; MetaX C500; MThreads S5000; T-Head PPU; plus Cambricon MLU and Iluvatar BI profiles).
   - Per-manufacturer fault controls: `initError` (optional transient count or persistent failure), `countError`, and `totalFailure`.
   - Per-group metadata (`id`, `name`, `memory`, `cores`, `driverVersion`, `runtimeVersion`, `computeCapability`, `family`) and per-slot accelerators (`id`, `index`, `physicalIndexes`, `topology`, `logicalSliced`, `physicalSliced`, `unhealthy`, `queryState` in `ok` | `unqueryable` | `fatalError`, and monitor metrics).
   - Optional per-process (`AcceleratorProcesses`), per-partition (`AcceleratorPartitions`), and node-level `interfaces` / `rdmaDevices` overrides.
3. **Hot-reload**: Re-reads the file on `detect` and `monitor` passes when content/mtime changes (supporting atomic `ConfigMap` symlink swaps). A malformed reload logs an error and retains the last valid mock spec without crashing or dropping inventory.
4. **Acceptance criteria**:
   - `device-manager detect --mock-devices-file <spec>` and `monitor --mock-devices-file <spec>` output valid `DevicesGroupList` and `MetricsGroupList` YAML on macOS and Linux without PCI hardware or vendor libraries.
   - Every built-in preset validates cleanly and produces non-empty groups matching its manufacturer's schema and topology conventions.
   - Updating `queryState: unqueryable` on one card while a sibling stays `ok` retains that card in place with `unhealthy: true` and unchanged sibling `index`; restoring `queryState: ok` clears `unhealthy` automatically.
   - Setting all cards to `queryState: unqueryable` returns a non-nil error from both `DetectAccelerator` and `MonitorAccelerator` so `Detector` preserves `lastDetected`; cleanly removing a slot prunes the cache.

#### Feature 2 — CPU-Only Mock `Detector`

1. Implements `device.Detector`, `device.AcceleratorProcessDetector`, and `device.AcceleratorPartitionDetector` ([`types.go`](../pkg/device/types.go)).
2. When mock mode is active, [`detector.New`](../pkg/devicemanager/detector/detector.go) instantiates mock detectors for the configured manufacturers instead of real vendor detectors, and skips real PCI/driver probes and real host `sysfs` NIC/RDMA scans whenever mock interfaces/RDMA are configured.
3. Obeys the exact `Detector` error and per-slot unhealthy-retention contracts documented in [`devices-discovery.md`](../docs/reference/devices-discovery.md).
4. **Acceptance criteria**:
   - Zero vendor shared libraries are `dlopen`'d when mock mode is enabled.
   - `Detector.Start` publishes `NodeFeature` (`<node>-gpustack-device-manager`), creates/updates `Devices` (`<node>`), notifies `Allocator` via `detectedManufacturersCh`, and refreshes `MonitorSnapshot` (including slice metrics) cleanly on a CPU-only node.

#### Feature 3 — CPU-Only Mock `Allocator`

1. Implements `device.Allocator` ([`types.go`](../pkg/device/types.go)) and registers real `pkg/deviceplugin.ResourceServer` instances against `--kube-socket` for every enabled allocation mode (`exclusive`, `shared`, `sliced`, `partitioned`, `visibility`) and optional mock `rdma`.
2. `GetContainerAllocateResponse` validates allocated accelerators through the exact production `deviceplugin.AllocatedAccelerators` path, injects safe diagnostic environment variables (`GPUSTACK_MOCK_ALLOCATED_DEVICES`, `GPUSTACK_MOCK_ALLOCATION_MODE`, `GPUSTACK_MOCK_MANUFACTURER`, plus the vendor's standard `*_VISIBLE_DEVICES` variable set to the mock accelerator/partition IDs), and mounts **zero** host `/dev/*` nodes and **zero** `ld.so.preload` shims.
3. For `partitioned` and `visibility` modes, maintains an in-memory virtual partition store (keyed by parent card, profile, placement, and synthetic partition UUID) with a reclaim loop driven by `deviceplugin.RunReclaimLoop`, so concurrent multi-profile partition creation, exclusion, visibility co-allocation, and dead-Pod reclamation work end to end on CPU containers.
4. **Acceptance criteria**:
   - Plain CPU containers (such as `busybox` or `pause`) requesting mock `.exclusive`, `.shared`, `.sliced`, `.partitioned.<kind>-<profile>`, or `.visibility` resources schedule through Kueue + `AdmissionCheck`, receive kubelet `Allocate` grants, reach `Running`, and observe the expected mock env vars inside the container.
   - Deleting a partitioned Pod reclaims its virtual partition slot and updates `Devices.status` cleanly.

#### Feature 4 — Production Safety Boundary & Optional Helm/Kind Support

1. Mock mode is strictly disabled by default. When enabled, `device-manager` logs a clear warning at startup and stamps `device.gpustack.ai/mock: "true"` on the node's `Devices` object.
2. Helm chart ([`values.yaml`](../deploy/gpustack-operator/chart/values.yaml), [`daemonset.yaml`](../deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml)) gains an opt-in `deviceManager.mock` section (`enabled: false`, `configMapName`, `manufacturers`, `omitRuntimeClass`, `nodeSelector`) so CPU-only Kind/k3s clusters can deploy mock `device-manager` DaemonSets without real NFD PCI labels or GPU container runtimes, while `deviceManager.mock.enabled=false` produces byte-identical manifests to today.
3. **Acceptance criteria**:
   - `helm template` with default values shows zero diff on existing manifests.
   - `make generate chart && make lint chart` passes cleanly.

#### Feature 5 — `gpustack-operator-e2e` Full Audit, Consolidation, and Contiguous Renumbering (`90 → 37`)

1. **Remove 13 obsolete, non-operator, recording-only, or rarely verified cases**:
   - `case-9` (half permanently skipped; unit-spec migration covered by unit/webhook tests)
   - `case-32` (observation-only out-of-band MIG carving)
   - `case-34` (observation-only dual-socket NUMA + static CPU + MIG)
   - `case-63` (self-constructed Mooncake failover timing benchmark; never reads rendered manifests)
   - `case-66` (EOL Kubernetes 1.23 historical CRD upgrade)
   - `case-67` (zero operator objects; raw Mooncake pods testing upstream drain)
   - `case-73` (legacy known-failure-detector KV reuse; superseded by live P/D and artifact KV cases)
   - `case-82` (unattainable three-property hardware shape; NUMA hint covered by unit tests)
   - `case-83` (bare-host preflight TopologyManager check; covered by preflight unit tests)
   - `case-84` (pure InfiniBand `ib_write_bw` transport check; rarely verifiable)
   - `case-86` (Ascend-specific TP transfer-doc check; covered by unit tests + live EPD/P/D cases)
   - `case-89` (non-self-contained read-only fixture inspector)
   - `case-90` (non-self-contained read-only serving metrics scraper)
2. **Consolidate overlapping families, add 3 CPU-mock cases, and renumber contiguously `case-1.sh .. case-37.sh` (zero gaps)**:

| New Case | Domain | Legacy Source(s) | Scope Summary | Cluster / Hardware |
| --- | --- | --- | --- | --- |
| `case-1` | Core CPU & Mock | `case-1` | CPU-only scheduling chain materializes (zero Cohort, Active `InstanceType`) | Any CPU |
| `case-2` | Core CPU & Mock | `case-2`, `case-16`, `case-36` | CPU `Instance` lifecycle, drain, `managed` toggle, portless omission, `InstanceTypeFlavor` & queue ownership, CPU awareness, node pinning & volumes | Any CPU (2 nodes for pin) |
| `case-3` | Core CPU & Mock | **New** + `case-4`, `case-5`, mock `case-19`, `case-22`, `case-96` | CPU Mock `Detector` + `Allocator` full chain: multi-vendor discovery, slice-% webhook, `AdmissionCheck` hold, cross-mode exclusion (`EX`/`SH`/`SL`), fit-label fragmentation skip, accelerated CPU awareness | CPU + Mock DM |
| `case-4` | Core CPU & Mock | **New** | Dynamic Mock fault injection: single-card `unhealthy`, unqueryable/lost card in-place unhealthy retention & sibling `Index` preservation, total-failure `lastDetected` retention, recovery, clean shrink pruning | CPU + Mock DM |
| `case-5` | Core CPU & Mock | **New** + mock `case-11`, `case-14`, `case-24`, `case-25`, `case-27`, `case-29` | CPU Mock logical-slice packing/co-residency/over-commit guard & virtual MIG/partition mixed-node, live-ledger capacity, exclusion, concurrent multi-profile allocation & reclaim | CPU + Mock DM |
| `case-6` | Core CPU & Mock | `case-37`, `case-41` | `Instance` metrics subresource, Prometheus `/metrics` exporter (single target + accelerated labels), and carved-card-only slice process pass | CPU + Mock/Real DM |
| `case-7` | Core CPU & Mock | `case-21`, `case-13`, `case-15`, `case-28` | SSH `Instance` suite: non-interactive exec + SFTP, exclusive, sliced, and partition-backed sidecar confinement | CPU + Mock/Real DM + SSH |
| `case-8` | Real Hardware | `case-8`, real `case-11`, real `case-14` | Real NVIDIA logical slicing runtime isolation (`libvgpu.so` memory/SM cap, co-resident workloads, real utilization metrics) | Real NVIDIA |
| `case-9` | Real Hardware | `case-23`, real `case-24`, `case-25`, `case-27`, `case-29` | Real NVIDIA MIG hardware lifecycle (enable, carve, mixed node, live capacity, concurrent multi-profile, reclaim, disable) | Real MIG NVIDIA + SSH |
| `case-10` | Real Hardware | `case-35` | Real Ascend NPU logical-slice placement & CANN container isolation | Real Ascend |
| `case-11` | Real Hardware | `case-38` | Real AMD ROCm whole-card, logical slicing (`libvrocm.so` + CU mask), and metrics | Real AMD |
| `case-12` | Real Hardware | `case-39` | Real T-Head PPU pinned claim and logical-slice (`libvppu.so`) runtime enforcement | Real T-Head + SSH |
| `case-13` | Real Hardware | `case-42` | Real Hygon DCU Multi-Instance hardware grant and refusal | Real Hygon + SSH |
| `case-14` | Network & TAS | `case-52`, `case-80`, `case-81`, `case-85` | NIC/RDMA inventory & `rdma.*` labels, RDMA resource sizing, container device-cgroup open/verbs isolation, host-fabric member DaemonSet | Any (RDMA legs auto-skip) |
| `case-15` | Network & TAS | `case-87`, `case-88` | Topology sources driving Kueue TAS placement, joint `ModelDeployment` admission, and per-card accelerator admission agreement | 3+ zoned nodes (Kind/Real) |
| `case-16` | KV Cache | `case-43`, `case-44` | `KVCachePool` / `KVCachePoolBinding` multi-tenant proportional quota, eviction enforcement, and tenant ledger gate | Any CPU + Mooncake |
| `case-17` | KV Cache | `case-62`, `case-75` | Replicated HA `KVCacheBackend` Kubernetes Lease leader election, graceful/hard failover, snapshot refusals, `1 -> 3` leader preservation, and rollout | Any CPU + Mooncake |
| `case-18` | KV Cache | `case-53` | Plain Deployment & LWS Pod KV cache injection webhook (vLLM / SGLang / vLLM-Ascend, opt-out byte-identity, refusals) | Any CPU + Mooncake |
| `case-19` | KV Cache | `case-65` | Mooncake member local disk eviction tier (`master_allocated_file_size_bytes` + host disk files) | Node with local disk path |
| `case-20` | Model Delivery | `case-97`, `case-98` | `ModelArtifact` Hub resolution (public/private/gated/revoked/terminating/secret redaction) + consumer claim placement affinity, pinned argv, and `Instance` PV placement | 2 CPU nodes + Hub |
| `case-21` | Model Delivery | `case-101`, `case-103` | Node model-cache CSI delivery: cold mount dedup, restricted namespace, forged-attribute & corrupt rejection, crash resume, plugin rollout, GC eviction, and restart reconciliation | 2 Kind/CPU nodes |
| `case-22` | Model Delivery | `case-108`, `case-111` | Download progress reporting (30s/5% thresholds, `progress` subresource RBAC), `ModelPrefetch`, and node-to-node peer sync (zero hub bytes, checkpoint resume, NetworkPolicy) | 2 CPU nodes |
| `case-23` | Model Delivery | `case-107` | Node-cache placement preference through Kueue TAS (`TASRespectNodeAffinityPreferred`) | 2+ Kind/CPU nodes |
| `case-24` | Model Delivery | `case-112` | OCI image-volume `ModelArtifact` source (digest contract, claim-shaped resolution, read-only image volume mount, `ModelPrefetch` refusal) | Kubelet 1.35+ / Kind |
| `case-25` | Model Delivery | `case-113` | ModelScope hub source resolution, cold materialization, and peer sync | 2 CPU nodes + ModelScope |
| `case-26` | MD & Scaler | `case-45`, `case-116`, `case-126`, `case-133` | Unified `ModelDeployment` admission refusal matrix (single-role, P/D, EPD encoder matrix, `engine.image`, External DP `RouterRequired`, `runtimeControl` 15-key validation & `scaler-binding`) | Any CPU |
| `case-27` | MD & Scaler | `case-47`, `case-70`, `case-117` | `ModelDeployment` rendered topology & ownership: KV reuse domain isolation, unreachable Binding degradation, routed P/D 6-object router lifecycle, `KVEventsPublishing`, EPD encoder Service & router stage | Any CPU + Mooncake |
| `case-28` | MD & Scaler | `case-49`, `case-50`, `case-68`, `case-118` | Multi-role & P/D/EPD Kueue joint admission: per-replica Workloads, Service watch recreation, short-pool whole-set starvation, two-InstanceType joint admission, `instance-type-derived-from-node=false` | Any CPU |
| `case-29` | MD & Scaler | `case-78`, `case-115` | `ModelDeployment` role scaling, multi-Pod `size` PodSet admission, surplus retirement finalizer release, and ordinary role update whole-group replacement | CPU + Mock/Real NVIDIA |
| `case-30` | MD & Scaler | `case-127` | Managed role host compilation cache mount, user `additionalVolumes` yield, `startupTimeoutSeconds` bounds, and startup-window `Revoked` hold | Any CPU (+ real warm-restart leg) |
| `case-31` | MD & Scaler | `case-128`, `case-131` | `ModelDeploymentScaler` admission, single-writer fence, binding/suspend/term lifecycle, `capacityPrior` defaulting, `priors.json` fallback, Priority 1/2 whole-unit resolution, EPD coupling, and ConfigMap freeze | Any CPU / Mock Accelerator |
| `case-32` | Live Serving | `case-91`, `case-94` | Live routed P/D serving over direct KV leg + continuous-traffic router/role scaling with zero dropped requests | Real GPU (2 nodes) |
| `case-33` | Live Serving | `case-99`, `case-100`, `case-104` | Live `ModelArtifact` serving (claim + gated download + node-cache 7B zero-byte warm restart + `cache_prefix` KV weight isolation) | Real GPU + Hub |
| `case-34` | Live Serving | `case-120`, `case-121` | Live multimodal EPD serving (`llm-d-router` + vLLM NIXL on NVIDIA/AMD, shared-directory RWX on Ascend) | Real NVIDIA/AMD or Ascend |
| `case-35` | Live Serving | `case-129`, `case-132` | Live `ModelDeploymentScaler` active scaling under real load (proportional running-requests + disaggregated `TokenThroughput` `1P:2D -> 2P:4D -> 1P:2D`) | Real GPU |
| `case-36` | Live Serving | `case-134`, `case-135`, `case-137` | Live online `runtimeControl` on vLLM & SGLang (`gpustack-engine-sentinel`, probes/ACK latch, Envoy allowlist/observer, live tuning & reset, role `pause`/resume, `PreStop` drain) | Real NVIDIA |
| `case-37` | Live Serving | `case-136` | Live folded Elastic EP (`runtimeControl.parameters["elastic.dataParallelWidth"]` `2 -> 3` in-place expansion preserving master Pod UID and post-resize parameter replay) | Real NVIDIA (3+ GPUs) |

3. **Contiguous auto-incrementing numbering & sync invariant**:
   - Every script in `.agents/skills/gpustack-operator-e2e/cases/` matches `case-<N>.sh` for exact `1..N` with zero gaps.
   - Update [`SKILL.md`](../.agents/skills/gpustack-operator-e2e/SKILL.md) (removing legacy absorbed-number columns), [`_env-audit.sh`](../.agents/skills/gpustack-operator-e2e/cases/_env-audit.sh), [`run-partition-block.sh`](../.agents/skills/gpustack-operator-e2e/cases/run-partition-block.sh), [`run-rdma-block.sh`](../.agents/skills/gpustack-operator-e2e/cases/run-rdma-block.sh), all `_*-lib.sh` helpers, and `references/*.md` to reference only `1..37`.
   - Enforce contiguity and `SKILL.md` row alignment mechanically in [`hack/check/skills.sh`](../hack/check/skills.sh) (covered by [`hack/check/skills-selftest.sh`](../hack/check/skills-selftest.sh)).

#### Feature 6 — Explicit-Only `gpustack-operator-e2e-chaos` Skill

1. Create `.agents/skills/gpustack-operator-e2e-chaos/` as an explicit-only skill (`disable-model-invocation: true` in `SKILL.md` with description `<= 247` chars, matching `agents/openai.yaml`, trigger row in [`AGENTS.md`](../AGENTS.md), and module-map entry in [`docs/README.md`](../docs/README.md)).
2. Provide a parameterized multi-node Kind mock cluster generator (`scripts/gen-kind-mock.sh` + `scripts/gen-mock-configmap.sh`) supporting both small local Kind topologies (2–4 mock workers) and large remote Kind/k3s topologies (8–32+ mock workers with heterogeneous built-in presets across zones/NUMA domains).
3. Provide four self-contained chaos suites (`cases/chaos-1.sh` through `cases/chaos-4.sh`) reusing `_rows-lib.sh` and `_mock-dm-lib.sh`:
   - `chaos-1.sh` — **Scale Heterogeneous Scheduling & Kueue Admission Storm**: concurrent mixed `exclusive` / `shared` / `sliced` / `partitioned` / P/D / EPD workload bursts across many mock nodes; asserts zero double-booking on `Devices.status`, zero negative `Remaining` credits, zero `UnexpectedAdmissionError`, and truthful four-view `InstanceType` accounting.
   - `chaos-2.sh` — **Dynamic Card Degradation, Loss & Recovery Chaos**: per-node `ConfigMap` fault injection (`unhealthy`, `unqueryable`, `totalFailure`, `initError`, recovery) under active scheduling load; asserts in-place unhealthy retention, sibling `Index` preservation, `lastDetected` preservation, unhealthy-card avoidance, and capacity restoration.
   - `chaos-3.sh` — **Fragmentation, Fit-Label Gating & Cross-Mode Contention**: checkerboard fragmentation and concurrent cross-mode contention races; asserts `AdmissionCheck` and fit labels prevent livelock and cross-mode card collision.
   - `chaos-4.sh` — **Control-Loop Churn & Out-of-Band Disruption**: mid-flight `device-manager` and `worker` Pod restarts, out-of-band deletions of `Devices`/`NodeFeature`/`LocalQueue`/`Service`, `gpustack.ai/managed` toggles, and mock card hot-shrink/expand; asserts full convergence with zero stuck finalizers or leaked virtual partitions.
4. **Acceptance criteria**:
   - `bash hack/check/skills-selftest.sh && bash hack/check/skills.sh`, `make lint agents-shell`, and `make lint docs` all pass cleanly.
   - `chaos-1.sh` through `chaos-4.sh` run cleanly on a local CPU-only Kind cluster with mock `device-manager` enabled.

### Notes / Constraints / Caveats

- Mock `Detector` and Mock `Allocator` must compile on both Linux and macOS (`darwin`) without CGO vendor headers, enabling fast local `go test` and `device-manager detect/monitor --mock-devices-file` runs on macOS as well as full `serve` runs in local or remote Linux Kind/k3s clusters.
- Existing vendor detector and allocator packages remain completely independent; mock mode is isolated in dedicated `pkg/devicemanager/mockspec/`, `pkg/devicemanager/detector/mock/`, and `pkg/devicemanager/allocator/mock/` packages.
- On CPU-only clusters where `worker` injects a manufacturer `RuntimeClassName`, tests or Helm mock overrides must either set `global.manufacturers.<mfr>.runtimeName: ""` or provision a `RuntimeClass` backed by `runc` so Pods start cleanly on CPU kubelets.
- Every edited or added shell script under `.agents/skills/` must pass `make lint agents-shell` (`shellcheck` + `symbols.sh`) and keep the `case-N.sh` / `chaos-N.sh` header contract (`Goal:`, `Prerequisites:`, `Environment:`, `Inputs:`, `Expected:`, `Cleanup:`).

### Boundaries

- **Always:**
  - Keep mock mode disabled by default with zero runtime overhead or behavioral change on real hardware nodes.
  - Validate mock spec files strictly on load and retain the last valid state on malformed hot-reloads.
  - Preserve full coverage of every non-obsolete operator invariant when merging overlapping E2E cases.
  - Keep `case-1.sh .. case-N.sh` strictly contiguous (`1..N`) and synchronized with `SKILL.md` and helper scripts.
  - Register `gpustack-operator-e2e-chaos` consistently across `SKILL.md`, `agents/openai.yaml`, `AGENTS.md`, and `docs/README.md`.
- **Ask first:**
  - Adding or changing any public CRD field under `api/worker/v1/` or `api/worker/v1alpha1/` (none expected; mock indicator uses an annotation).
  - Running destructive chaos or real-GPU E2E suites against a user's shared cluster (default to an isolated local Kind cluster).
- **Never:**
  - Allow mock mode to open `/dev/*`, load vendor `.so` libraries via `dlopen`, or write host `ld.so.preload` files.
  - Silently drop a real-hardware runtime assertion (such as `libvgpu.so` / `libvrocm.so` / `libvppu.so` enforcement or live engine inference) without keeping its real-hardware case.
  - Leave broken case-number references or numbering gaps in `.agents/skills/gpustack-operator-e2e/`.

### Risks and Mitigations

- **Risk:** Mock mode accidentally enabled in production via stray environment variable → **Mitigation:** Require an explicit file path (`--mock-devices-file` / `GPUSTACK_MOCK_DEVICES_FILE`) pointing to a valid schema file, emit a high-visibility startup warning log, and stamp `device.gpustack.ai/mock: "true"` on `Devices`.
- **Risk:** Renumbering collisions when parallel tasks author new `case-N.sh` scripts that share names with legacy scripts (for example, legacy `case-16.sh` vs new `case-16.sh`) → **Mitigation:** Use an expand-contract staging directory (`cases/next/case-1..37.sh`) for parallel authoring tasks (T6–T9) followed by one atomic cutover task (T10).
- **Risk:** Merged E2E cases become overly long or fail early on one sub-leg, hiding later assertions → **Mitigation:** Follow `_rows-lib.sh` `record` + `results` so independent sub-legs report per-check `PASS`/`FAIL`/`SKIP` rows and clean up reliably in traps.
- **Risk:** Chaos tests leave orphaned resources or mutated `ConfigMap`s on failure → **Mitigation:** Every `chaos-N.sh` script creates an isolated test namespace and restores the mock `ConfigMap` and cluster settings inside an `EXIT` trap.
- **Risk:** Helm `values.yaml` drift against `values.schema.json` or chart `README.md`, or `hook-dispatch.sh` mismatch if `agents_shell_lint()` steps change → **Mitigation:** Run `make generate chart && make lint chart`, place the E2E contiguity check inside `hack/check/skills.sh` (already covered by `hook-dispatch.sh`), and run `make lint` + `make lint docs`.

## Design Details

### Commands

**Build and test environment (confirmed):**
- **Local macOS host**: Go 1.27 unit tests, `make lint`, `make lint docs`, `make generate chart && make lint chart`, `make lint agents-shell`, and `bash hack/check/skills-selftest.sh`.
- **Local Kind cluster (`kind`)**: CPU-only Mock `device-manager` + `worker` + Kueue end-to-end verification (`case-1.sh`..`case-5.sh` and `chaos-1.sh`..`chaos-4.sh`).
- **Remote native Linux host (`frank@192.168.50.17`, `x86_64`, via incremental `git bundle`)**: Native Linux `CGO_ENABLED=1` builds/tests and optional large multi-node Kind chaos runs (8–32+ mock workers).

```bash
# Run unit and integration tests for mockspec, detector, allocator, and deviceplugin
go test -count=1 ./pkg/device/... ./pkg/devicemanager/... ./pkg/deviceplugin/...

# Run full Go, shell, commit, and skill-selftest linters
make lint

# Regenerate and validate Helm chart schema and README
make generate chart
make lint chart

# Validate docs, specs, module map, and skill registrations
make lint docs

# Validate .agents shell scripts and E2E case contiguity gate
bash hack/check/skills-selftest.sh
bash hack/check/skills.sh
make lint agents-shell

# Audit consolidated E2E case prerequisites against a cluster
bash .agents/skills/gpustack-operator-e2e/cases/_env-audit.sh gpustack-system
```

### Project Structure

Existing touchpoints and proposed additions:

- [`pkg/device/types.go`](../pkg/device/types.go) — `Detector`, `AcceleratorProcessDetector`, `AcceleratorPartitionDetector`, and `Allocator` interfaces (unchanged contracts).
- [`pkg/devicemanager/option.go`](../pkg/devicemanager/option.go), [`pkg/devicemanager/config.go`](../pkg/devicemanager/config.go), [`pkg/devicemanager/cmd.go`](../pkg/devicemanager/cmd.go) — plumb `--mock-devices-file` / `GPUSTACK_MOCK_DEVICES_FILE` into `detector.Config` and `allocator.Config`.
- `pkg/devicemanager/mockspec/` *(proposed)* — declarative YAML/JSON mock hardware & fault-injection schema, per-node selector (`default` + `nodes.<nodeName>`), built-in multi-vendor presets (`presets/*.yaml` embedded via `embed.FS`), validator, and thread-safe hot-reloading `Store`.
- `pkg/devicemanager/detector/mock/` *(proposed)* — CPU-only mock `device.Detector` (plus process/partition/RDMA providers and per-slot unhealthy-retention cache).
- `pkg/devicemanager/allocator/mock/` *(proposed)* — CPU-only mock `device.Allocator` wrapping `pkg/deviceplugin.ResourceServer` across all five allocation modes plus in-memory virtual partition ledger and reclaim.
- [`pkg/devicemanager/detector/detector.go`](../pkg/devicemanager/detector/detector.go), [`pkg/devicemanager/allocator/allocator.go`](../pkg/devicemanager/allocator/allocator.go) — select mock creators when mock spec is configured; stamp `device.gpustack.ai/mock: "true"` on `Devices`.
- [`deploy/gpustack-operator/chart/values.yaml`](../deploy/gpustack-operator/chart/values.yaml), [`deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml`](../deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml) — optional `deviceManager.mock` values (disabled by default).
- [`.agents/skills/gpustack-operator-e2e/`](../.agents/skills/gpustack-operator-e2e/SKILL.md) — consolidated `cases/case-1.sh .. case-37.sh`, `_mock-dm-lib.sh`, updated `SKILL.md`, `_env-audit.sh`, `run-partition-block.sh`, `run-rdma-block.sh`, `_*-lib.sh`, and `references/`.
- `.agents/skills/gpustack-operator-e2e-chaos/` *(proposed)* — explicit-only chaos & scale-scheduling skill (`SKILL.md`, `agents/openai.yaml`, `scripts/gen-kind-mock.sh`, `scripts/gen-mock-configmap.sh`, `cases/chaos-1.sh .. chaos-4.sh`).
- [`hack/check/skills.sh`](../hack/check/skills.sh), [`hack/check/skills-selftest.sh`](../hack/check/skills-selftest.sh) — automated E2E case contiguity and `SKILL.md` table sync gate.
- [`AGENTS.md`](../AGENTS.md), [`docs/README.md`](../docs/README.md), [`docs/reference/settings.md`](../docs/reference/settings.md) — skill trigger registration, module map sync, and `GPUSTACK_MOCK_DEVICES_FILE` reference entry.

### Code Style

All Go packages follow `AGENTS.md` conventions (`snake_case` filenames, explicit error handling, table-driven tests, plain comments with zero decorative symbols). Mock implementations conform directly to the existing `pkg/device/types.go` interfaces:

```go
// From pkg/device/types.go
type Detector interface {
	Name() string
	DetectAccelerator(noPciCheck bool) (DevicesGroupList, error)
	MonitorAccelerator(noPciCheck bool) (MetricsGroupList, error)
}

type Allocator interface {
	Name() string
	Start(ctx context.Context) error
	Stop()
}
```

### Implementation Plan

- [x] **T1 · Declarative mock hardware spec, built-in presets, and hot-reload store**
      Blocked by: None
      Owns: `pkg/devicemanager/mockspec/**`
      Gate: review
      Acceptance: YAML/JSON schema parses flat and per-node (`default` + `nodes.<nodeName>`) specs, resolves embedded built-in presets across all 9 manufacturers (`nvidia`, `amd`, `ascend`, `hygon`, `metax`, `mthreads`, `thead`, `cambricon`, `iluvatar`), validates manufacturers/indexes/fault modes, hot-reloads on file change (including atomic ConfigMap symlink swap), and retains the last valid spec on malformed reload.
      Verify: `go test -count=1 ./pkg/devicemanager/mockspec/...`

- [x] **T2 · CPU-only Mock `Detector` with unhealthy retention and process/partition/RDMA providers**
      Blocked by: T1
      Owns: `pkg/devicemanager/detector/mock/**`
      Acceptance: Implements `device.Detector`, `device.AcceleratorProcessDetector`, `device.AcceleratorPartitionDetector`, and mock node interface/RDMA readers with zero CGO; enforces init/count/fatal/all-cards-failed error propagation, per-slot in-place unhealthy retention (`Status.Unhealthy: true` and `Unhealthy: true` in monitor), sibling `Index` preservation, recovery, and clean shrink cache pruning on both macOS and Linux.
      Verify: `go test -count=1 ./pkg/devicemanager/detector/mock/...`

- [x] **T3 · CPU-only Mock `Allocator` with virtual partition ledger and reclaim**
      Blocked by: T1
      Owns: `pkg/devicemanager/allocator/mock/**`
      Acceptance: Implements `device.Allocator` wrapping real `pkg/deviceplugin.ResourceServer` across `exclusive`, `shared`, `sliced`, `partitioned`, and `visibility` (plus optional mock `rdma`); injects safe diagnostic env vars and vendor `*_VISIBLE_DEVICES` with zero `/dev/*` mounts and zero `ld.so.preload` shims; maintains an in-memory virtual partition store with `deviceplugin.RunReclaimLoop` for concurrent multi-profile creation, visibility co-allocation, and dead-Pod reclamation.
      Verify: `go test -count=1 ./pkg/devicemanager/allocator/mock/...`

- [x] **T4 · Plumb `--mock-devices-file` / `GPUSTACK_MOCK_DEVICES_FILE` into `device-manager` and document setting**
      Blocked by: T2, T3
      Owns: `pkg/devicemanager/option.go`, `pkg/devicemanager/config.go`, `pkg/devicemanager/cmd.go`, `pkg/devicemanager/detector/option.go`, `pkg/devicemanager/detector/config.go`, `pkg/devicemanager/detector/detector.go`, `pkg/devicemanager/detector/detector_test.go`, `pkg/devicemanager/allocator/option.go`, `pkg/devicemanager/allocator/config.go`, `pkg/devicemanager/allocator/allocator.go`, `pkg/devicemanager/allocator/allocator_test.go`, `docs/reference/settings.md`
      Gate: review
      Acceptance: Mock mode is disabled by default (zero behavior change when unset); when configured, `detect`/`monitor`/`serve` bypass real vendor detectors/allocators and PCI/sysfs scans, log a startup warning, stamp `device.gpustack.ai/mock: "true"` on `Devices`, work on macOS (`detect`/`monitor`) and Linux (`serve`), and document `GPUSTACK_MOCK_DEVICES_FILE` in `docs/reference/settings.md`.
      Verify: `go test -count=1 ./pkg/devicemanager/... && make lint docs`

- [ ] **T5 · Add opt-in Helm chart `deviceManager.mock` values and DaemonSet rendering**
      Blocked by: T4
      Owns: `deploy/gpustack-operator/chart/**`
      Gate: review
      Acceptance: `deviceManager.mock.enabled` defaults to `false` with byte-identical `helm template` output vs `main`; when `enabled: true`, renders mock DaemonSets mounting `configMapName`, passing `--mock-devices-file`, and honoring `omitRuntimeClass` and custom `nodeSelector`; regenerated `values.schema.json` and chart `README.md` pass lint.
      Verify: `make generate chart && make lint chart`

- [ ] **T6 · Author consolidated Domain 1 CPU & Mock E2E cases (`next/case-1.sh` .. `next/case-7.sh`)**
      Blocked by: T4, T5
      Owns: `.agents/skills/gpustack-operator-e2e/cases/next/case-[1-7].sh`, `.agents/skills/gpustack-operator-e2e/cases/_mock-dm-lib.sh`
      Acceptance: Adds `_mock-dm-lib.sh` (ConfigMap fixture + hot-reload helper + trap restore) and authors `next/case-1.sh` through `next/case-7.sh` covering CPU baseline, merged CPU Instance/catalog/pinning (`2+16+36`), Mock full scheduling & cross-mode/fit-label chain (`4+5+19+22+96`), Mock dynamic fault/unhealthy-retention lifecycle, Mock logical slicing & virtual MIG (`11+14+24+25+27+29`), metrics/exporter (`37+41`), and unified SSH suite (`21+13+15+28`), all passing `shellcheck` and header contract.
      Verify: `bash -n .agents/skills/gpustack-operator-e2e/cases/_mock-dm-lib.sh .agents/skills/gpustack-operator-e2e/cases/next/case-[1-7].sh && make lint agents-shell`

- [x] **T7 · Author consolidated Domain 2 & 3 Real-Hardware, RDMA & TAS E2E cases (`next/case-8.sh` .. `next/case-15.sh`)**
      Blocked by: None
      Owns: `.agents/skills/gpustack-operator-e2e/cases/next/case-8.sh`, `.agents/skills/gpustack-operator-e2e/cases/next/case-9.sh`, `.agents/skills/gpustack-operator-e2e/cases/next/case-1[0-5].sh`
      Acceptance: Authors `next/case-8.sh` through `next/case-15.sh` consolidating real NVIDIA slicing (`8+11+14`), real NVIDIA MIG lifecycle (`23+24+25+27+29`), Ascend (`35`), AMD (`38`), T-Head (`39`), Hygon (`42`), unified NIC/RDMA (`52+80+81+85`), and Topology/TAS (`87+88`), preserving all real-hardware guards and auto-skip contracts.
      Verify: `bash -n .agents/skills/gpustack-operator-e2e/cases/next/case-8.sh .agents/skills/gpustack-operator-e2e/cases/next/case-9.sh .agents/skills/gpustack-operator-e2e/cases/next/case-1[0-5].sh && make lint agents-shell`

- [x] **T8 · Author consolidated Domain 4 & 5 KV Cache & Model Delivery E2E cases (`next/case-16.sh` .. `next/case-25.sh`)**
      Blocked by: None
      Owns: `.agents/skills/gpustack-operator-e2e/cases/next/case-1[6-9].sh`, `.agents/skills/gpustack-operator-e2e/cases/next/case-2[0-5].sh`
      Acceptance: Authors `next/case-16.sh` through `next/case-25.sh` consolidating KV Cache pool/quota (`43+44`), HA leader/failover/rollout (`62+75`), Pod injection (`53`), local disk (`65`), ModelArtifact Hub + consumers (`97+98`), CSI node cache + lifecycle (`101+103`), progress/prefetch/peer sync (`108+111`), TAS preference (`107`), OCI image volume (`112`), and ModelScope (`113`).
      Verify: `bash -n .agents/skills/gpustack-operator-e2e/cases/next/case-1[6-9].sh .agents/skills/gpustack-operator-e2e/cases/next/case-2[0-5].sh && make lint agents-shell`

- [x] **T9 · Author consolidated Domain 6 & 7 MD, Scaler & Live Serving E2E cases (`next/case-26.sh` .. `next/case-37.sh`)**
      Blocked by: None
      Owns: `.agents/skills/gpustack-operator-e2e/cases/next/case-2[6-9].sh`, `.agents/skills/gpustack-operator-e2e/cases/next/case-3[0-7].sh`
      Acceptance: Authors `next/case-26.sh` through `next/case-37.sh` consolidating MD admission matrix (`45+116+126+133`), rendered topology/router/EPD (`47+70+117`), joint admission (`49+50+68+118`), scaling & replacement (`78+115`), compile cache (`127`), Scaler + capacityPrior (`128+131`), live P/D serving & scaling (`91+94`), live ModelArtifact serving (`99+100+104`), live EPD serving (`120+121`), live Scaler (`129+132`), live vLLM+SGLang runtimeControl (`134+135+137`), and live folded Elastic EP (`136`).
      Verify: `bash -n .agents/skills/gpustack-operator-e2e/cases/next/case-2[6-9].sh .agents/skills/gpustack-operator-e2e/cases/next/case-3[0-7].sh && make lint agents-shell`

- [ ] **T10 · Cut over `case-1.sh .. case-37.sh`, remove legacy 90 scripts, and sync `SKILL.md` and helpers**
      Blocked by: T6, T7, T8, T9
      Owns: `.agents/skills/gpustack-operator-e2e/**`, `.agents/skills/_e2e-lib/**`, `.agents/skills/gpustack-operator-benchmark/**`
      Gate: review
      Acceptance: Replaces all 90 legacy `case-*.sh` files with `case-1.sh .. case-37.sh` from `cases/next/`, removes `cases/next/`, updates `SKILL.md` (exact `1..37` table with accurate `Needs` column and zero retired absorbed-number lists), `_env-audit.sh`, `run-partition-block.sh`, `run-rdma-block.sh`, `_*-lib.sh`, `references/*.md`, and cross-references in `_e2e-lib/` and `gpustack-operator-benchmark/`.
      Verify: `bash .agents/skills/gpustack-operator-e2e/cases/_env-audit.sh gpustack-system && make lint agents-shell && make lint docs`

- [ ] **T11 · Add automated E2E case contiguity and `SKILL.md` index sync gate**
      Blocked by: T10
      Owns: `hack/check/skills.sh`, `hack/check/skills-selftest.sh`
      Gate: review
      Acceptance: `hack/check/skills.sh` fails whenever `.agents/skills/gpustack-operator-e2e/cases/case-*.sh` has a numbering gap, non-sequential start, or mismatch against `SKILL.md`'s case table; `hack/check/skills-selftest.sh` proves each failure shape fails and the clean tree passes.
      Verify: `bash hack/check/skills-selftest.sh && bash hack/check/skills.sh && make lint`

- [ ] **T12 · Create explicit-only `gpustack-operator-e2e-chaos` skill and multi-node Kind mock harness**
      Blocked by: T6, T10, T11
      Owns: `.agents/skills/gpustack-operator-e2e-chaos/**`, `AGENTS.md`, `docs/README.md`
      Gate: review
      Acceptance: Adds `.agents/skills/gpustack-operator-e2e-chaos/` (`SKILL.md` with `disable-model-invocation: true` and description `<= 247` chars, `agents/openai.yaml`, `scripts/gen-kind-mock.sh`, `scripts/gen-mock-configmap.sh`, `cases/chaos-1.sh .. chaos-4.sh`) and registers it in `AGENTS.md` and `docs/README.md`; all lint gates pass and `chaos-1.sh .. chaos-4.sh` are verified on a local CPU-only Kind cluster.
      Verify: `bash hack/check/skills-selftest.sh && bash hack/check/skills.sh && make lint agents-shell && make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- Extend `hack/check/skills-selftest.sh` with failing fixtures for E2E case numbering gaps, extra unindexed scripts, and stale `SKILL.md` table rows.
- Ensure existing `pkg/devicemanager/detector/detector_test.go` and `pkg/devicemanager/allocator/` tests continue passing unchanged when `--mock-devices-file` is unset.

#### Unit tests
Every new Go package targets `>= 85%` statement coverage with table-driven deterministic tests (zero CGO, runnable on macOS and Linux):
- `pkg/devicemanager/mockspec`: target `>= 85%` — YAML/JSON parsing, flat vs per-node (`default` + `nodes.<nodeName>`) resolution, all 9 built-in vendor presets validation, invalid schema rejections, atomic symlink hot-reload, and last-valid-state preservation on malformed reload.
- `pkg/devicemanager/detector/mock`: target `>= 85%` — multi-vendor `DetectAccelerator` and `MonitorAccelerator`, `initError` / `countError` / `fatalError` / all-cards-unqueryable total failure, per-slot in-place unhealthy retention and sibling `Index` preservation, recovery back to healthy, clean shrink cache pruning, `MonitorAcceleratorProcesses`, `MonitorAcceleratorPartitions`, and mock NIC/RDMA inventory reads.
- `pkg/devicemanager/allocator/mock`: target `>= 85%` — `exclusive`/`shared`/`sliced`/`partitioned`/`visibility` + mock `rdma` server construction, `GetContainerAllocateResponse` env injection with zero `/dev/*` devices and zero `ld.so.preload` mounts, concurrent virtual partition allocation/exclusion, visibility co-allocation, and `RunReclaimLoop` reclamation.
- `pkg/devicemanager/detector` & `pkg/devicemanager/allocator`: target `>= 80%` on modified wiring — default-disabled production path, mock creator dispatch, startup warning, and `device.gpustack.ai/mock: "true"` annotation stamping.

#### Integration tests
- In-process fake Kubernetes client loop wiring `Detector` (mock) → `NodeFeature` + `Devices` (`device.gpustack.ai/mock: "true"`) → `detectedManufacturersCh` → `Allocator` (mock), including live hot-reload transitions (healthy → single card unqueryable/unhealthy → total failure → recovery → shrink).
- Helm chart template verification: `helm template` default values produce zero diff vs `main`; `deviceManager.mock.enabled=true` mounts the mock `ConfigMap`, passes `--mock-devices-file`, and honors `omitRuntimeClass` and `nodeSelector`.
- Shell & docs gate integration: `bash hack/check/skills-selftest.sh`, `make lint agents-shell`, `make lint chart`, `make lint docs`, and `make lint`.

#### e2e tests
- Local CPU-only Kind cluster running the operator with `deviceManager.mock.enabled=true`:
  - Run consolidated `case-1.sh` through `case-5.sh` (CPU baseline, CPU Instance/catalog, Mock full scheduling/cross-mode/fit-label chain, Mock dynamic fault/unhealthy-retention lifecycle, Mock logical slicing & virtual MIG).
  - Run `gpustack-operator-e2e-chaos` `chaos-1.sh` through `chaos-4.sh` (scale scheduling storm, card degradation/recovery, fragmentation/gating contention, control-loop Pod/object churn).
  - Run `bash .agents/skills/gpustack-operator-e2e/cases/_env-audit.sh gpustack-system` to verify all 37 consolidated cases parse cleanly with strict `1..37` contiguity.

## Alternatives

1. **Fake `libnvidia-ml.so` / vendor C shims injected via `LD_LIBRARY_PATH` inside Kind nodes**:
   - Rejected: requires building and maintaining 9 separate C shared-library stubs across architectures (`amd64`/`arm64`), cannot cleanly bypass vendor `DevicePlugin` `/dev/*` mounts or `ld.so.preload` shims without extra hacks, and breaks on macOS developer machines.
2. **Continue hand-crafting fake `NodeFeature` and `Devices` CRs directly from bash E2E scripts**:
   - Rejected: bypasses `device-manager` detection, monitoring, unhealthy retention, exporter metrics, and kubelet `DevicePlugin` allocation completely; real integration bugs in those layers stay invisible until physical GPU qualification.
3. **Separate Go build tag (`-tags mock`) for a test-only binary**:
   - Rejected: doubles CI image build and distribution overhead and prevents running mock verification with the exact release image. An explicit opt-in file flag on the single binary provides equivalent safety with one build artifact.
4. **Embed chaos stress tests inside `gpustack-operator-e2e` instead of a dedicated `gpustack-operator-e2e-chaos` skill**:
   - Rejected: keeps functional regression cases (`case-1..37`) fast, deterministic, and clearly separated from high-concurrency scale/chaos runs (`chaos-1..4`) that intentionally induce failures and Pod restarts on large multi-node mock clusters.

## Open Questions

- None. All scope, built-in preset, local Kind testing, Helm, E2E consolidation, and `gpustack-operator-e2e-chaos` decisions are resolved.
