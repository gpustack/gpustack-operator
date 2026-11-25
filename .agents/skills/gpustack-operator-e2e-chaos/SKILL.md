---
name: gpustack-operator-e2e-chaos
description: "Run multi-node scheduling storms, fit-label fragmentation races, dynamic Mock Device Manager fault injection, and control-loop resilience chaos suites on local or remote Kind/k3s clusters."
disable-model-invocation: true
---

# GPUStack Operator — Multi-Node Mock & Scheduling Chaos Suite

Use [the shared module map](../../../docs/README.md#module-map) to locate the relevant guides,
owning specs, code entry points and related modules. [AGENTS.md](../../../AGENTS.md#finding-sources)
owns the retrieval and evidence rules; this skill owns multi-node CPU-mock cluster generation and
scheduling/resilience chaos execution.

Exercise the real `device-manager` → `worker` → Kueue → `AdmissionCheck` → kubelet `DevicePlugin`
scheduling chain under concurrent scale bursts, checkerboard fragmentation, dynamic hardware fault
injection, and mid-flight control-loop disruptions:

- Architecture and contracts → [architecture.md](../../../docs/getting-started/architecture.md),
  [devices-discovery.md](../../../docs/reference/devices-discovery.md) (unhealthy retention and
  `lastDetected` preservation), [devices-scheduling.md](../../../docs/reference/devices-scheduling.md),
  [devices-admission.md](../../../docs/reference/devices-admission.md) (five gates, fit labels, and
  four-view accounting), and [settings.md](../../../docs/reference/settings.md)
  (`GPUSTACK_MOCK_DEVICES_FILE`); owning spec →
  `specs/2026-11-24-cpu-mock-device-manager-and-e2e-consolidation.md`.
- Functional single-feature E2E cases (`case-1.sh .. case-37.sh`) live in
  [gpustack-operator-e2e](../gpustack-operator-e2e/SKILL.md). Use `gpustack-operator-e2e-chaos` when
  stress-testing multi-node concurrency, admission contention, fault transitions, or controller
  churn across local `2–4` worker Kind clusters or remote `8–32+` worker Kind/k3s clusters.

## Safety boundary

- **Never run destructive chaos suites against a shared or production cluster without explicit user
  confirmation.** Default to an isolated local Kind cluster provisioned specifically for the run.
- Pin `KUBECONFIG` (or confirm `kubectl config current-context` with the user) before running any
  generator with `--create` or executing `cases/chaos-1.sh .. cases/chaos-4.sh`.
- Every `cases/chaos-N.sh` script creates an isolated `chaos-c<N>-<ts>` namespace for its test
  workloads, snapshots the active mock `ConfigMap` (`mock_dm_snapshot_cm`) and any touched settings
  or `NodeFeature` labels before mutating them, and restores the baseline inside an idempotent
  `EXIT` trap on both pass and fail.

## Quick-start workflow

1. **Generate (and optionally create) a zoned multi-node Kind cluster** (`2–4` local workers or
   `8–32+` remote workers, with nested `topology.kubernetes.io/region`, `zone`, and `rack` labels
   applied via `kubeadmConfigPatches` for Kueue TAS):

   ```bash
   # Local Docker host:
   bash .agents/skills/gpustack-operator-e2e-chaos/scripts/gen-kind-mock.sh \
     --name gpustack-chaos --workers 3 --zones 2 \
     --out /tmp/kind-chaos.yaml --create

   # Or on a larger remote Docker host over SSH (any Kubernetes version):
   terraform -chdir=testing/infra/clusters/kind init
   terraform -chdir=testing/infra/clusters/kind apply \
     -var='host=<user>@<remote-host>' -var='workers=4' -var='release=v1.34.11'
   ```

2. **Generate and apply a multi-node mock hardware `ConfigMap`** backed by the built-in multi-vendor
   presets (`nvidia-h100`, `nvidia-h100-mig`, `amd-mi308x`, `ascend-910b2`, `hygon-k100-ai-mig`,
   `thead-ppu-mig`, `metax-c500`, `cambricon-mlu590`, `iluvatar-bi-v150`, `mthreads-s5000`):

   ```bash
   kubectl create namespace gpustack-system --dry-run=client -o yaml | kubectl apply -f -
   bash .agents/skills/gpustack-operator-e2e-chaos/scripts/gen-mock-configmap.sh \
     --namespace gpustack-system --name mock-devices --key devices.yaml \
     --cluster-name gpustack-chaos --profile heterogeneous \
     | kubectl apply -f -
   ```

3. **Build, load, and install/upgrade the operator chart with CPU Mock Device Manager enabled**
   (`omitRuntimeClass=true` skips GPU `RuntimeClass` injection on plain CPU kubelets):

   ```bash
   helm upgrade --install gpustack-operator deploy/gpustack-operator/chart \
     -n gpustack-system \
     --set image.tag="$TAG" --set image.pullPolicy=IfNotPresent \
     --set deviceManager.mock.enabled=true \
     --set deviceManager.mock.configMapName=mock-devices \
     --set deviceManager.mock.configMapKey=devices.yaml \
     --set deviceManager.mock.omitRuntimeClass=true
   ```

4. **Run the chaos suites** (scaling concurrency and burst volume automatically with worker count or
   via `CHAOS_CONCURRENCY`, `CHAOS_BURST_SIZE`, `CHAOS_TIMEOUT`, and `E2E_MOCK_IMAGE`):

   ```bash
   for s in .agents/skills/gpustack-operator-e2e-chaos/cases/chaos-{1..4}.sh; do
     bash "$s" gpustack-system
   done
   ```

## Generator reference

- `scripts/gen-kind-mock.sh`:
  - `--name <cluster>` (default `gpustack-mock`), `--workers <N>` (default `3`), `--zones <Z>`
    (default `2`), `--racks-per-zone <R>` (default `2`), `--region <name>` (default `chaos-region`),
    `--image <kindest/node:tag>`, `--kubeconfig <file>`, `--out <file>`, `--create`.
  - Renders `enable-aggregator-routing: "true"` on the control-plane API server and strictly nested
    `topology.kubernetes.io/region`, `topology.kubernetes.io/zone=zone-<z>`, and
    `topology.kubernetes.io/rack=rack-<r>` labels on every worker node via `kubeadmConfigPatches`
    (`nodeRegistration.kubeletExtraArgs.node-labels`) and Kind node labels.
- `scripts/gen-mock-configmap.sh`:
  - `--namespace <NS>` (default `gpustack-system`), `--name <cm>` (default `mock-devices`),
    `--key <key>` (default `devices.yaml`), `--nodes <csv>` or `--cluster-name <kind-name>`
    (with optional `--workers <N>`, or live `kubectl get nodes` discovery),
    `--profile <heterogeneous|scale-mig|fault-lab|multi-vendor>` (default `heterogeneous`),
    `--default-preset <preset>` (default `nvidia-h100`), `--cards-per-node <N>`, `--raw-spec`,
    `--out <file>`.

## Cases

Every script in `cases/chaos-1.sh .. cases/chaos-4.sh` is self-contained, executable, numbered
contiguously (`1..4`, zero gaps; new suites append at `N+1` and are enforced by
`hack/check/skills.sh`), reuses `../gpustack-operator-e2e/cases/_rows-lib.sh` and `_mock-dm-lib.sh`,
and ends with a `STATUS | CHECK | OBJECT` verdict table.

| Case | Title | Run when these change (`git diff --name-only origin/main...HEAD`) | Mutates | Needs |
|---|---|---|---|---|
| `cases/chaos-1.sh` | Scale heterogeneous scheduling & Kueue admission storm (`EX`/`SH`/`SL`/`PT` bursts + multi-role P/D & EPD `ModelDeployment`) | `pkg/devicemanager/{mockspec,detector/mock,allocator/mock}/**`, `pkg/deviceplugin/**`, `pkg/worker/controllers/worker/{node_devices_admission,node_devices_inflight,node_devices,node_capacity,instance_type,model_deployment*}.go` | yes (confirm) | CPU + Mock DM (`deviceManager.mock.enabled=true`, `2–32+` mock workers ideal; `CHAOS_CONCURRENCY`, `CHAOS_BURST_SIZE`, `CHAOS_TIMEOUT`, `E2E_MOCK_IMAGE` optional) |
| `cases/chaos-2.sh` | Dynamic card degradation, loss & recovery chaos (`unhealthy`, `unqueryable`, `totalFailure`, transient `initError`, recovery) | `pkg/devicemanager/{mockspec,detector,detector/mock,allocator,allocator/mock}/**`, `pkg/worker/controllers/worker/{node_devices,node_capacity,instance_type}.go` | yes (confirm) | CPU + Mock DM (`deviceManager.mock.enabled=true`; `CHAOS_CONCURRENCY`, `CHAOS_TIMEOUT`, `E2E_MOCK_IMAGE` optional) |
| `cases/chaos-3.sh` | Checkerboard fragmentation, fit-label gating & cross-mode contention (`EX`/`SH`/`SL`/`PT` races + `AdmissionCheck` `Retry` & release) | `pkg/worker/controllers/worker/{node_devices_admission,node_devices_inflight,node_fit_label,fit_demand}.go`, `pkg/worker/webhooks/worker/{pod,workload}.go`, `pkg/nodefeature/{fit,knowns}.go`, `pkg/deviceplugin/**` | yes (confirm) | CPU + Mock DM serving `nvidia` (`deviceManager.mock.enabled=true`, `2+` mock workers ideal; `CHAOS_CONCURRENCY`, `CHAOS_TIMEOUT`, `E2E_MOCK_IMAGE` optional) |
| `cases/chaos-4.sh` | Control-loop churn & out-of-band disruption (`device-manager` & `worker` Pod kills, `Devices`/`NodeFeature`/`LocalQueue`/`Service` deletes, `managed` toggle, hot shrink/expand) | `pkg/devicemanager/**`, `pkg/worker/controllers/worker/{instance,node_flavor,node_queue,node_queue_entrance,node_devices,instance_type}.go` | yes (confirm) | CPU + Mock DM (`deviceManager.mock.enabled=true`; `CHAOS_TIMEOUT`, `E2E_MOCK_IMAGE` optional) |

## Case header contract

Every `cases/chaos-N.sh` opens with a seven-field header (`# CHAOS N — <title> (<posture>)`,
`# Prerequisites:`, `# Goal:`, `# Environment:`, `# Inputs:`, `# Expected:`, `# Cleanup:`) with zero
spec task IDs or commit hashes, isolates created workloads inside its own `chaos-c<N>-<ts>`
namespace, and restores cluster state in `trap … EXIT`. Note on `Detector` edge-triggered
re-detection (`pkg/devicemanager/detector/detector.go` `_DeviceKey`): when patching `cardOverrides`
to change card slicing/partition capabilities without changing the preset or `unhealthy` bit, also
override the card `id` so `Detector` breaks out of its monitor loop on the next tick.
