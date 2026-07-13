---
name: gpustack-operator-overview
description: "Use when you need a guided tour of the GPUStack Operator codebase — its architecture, where things live (key directories), and naming conventions. Examples: \"Give me an overview of this repo\", \"How is this project structured?\", \"Where does X live?\", onboarding to the scheduling chain."
model: haiku
---

# GPUStack Operator — Code Overview

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Architecture in brief

One `gpustack-operator` binary, three subcommands; the scheduling chain builds in four stages:

1. **Bootstrap** — the `worker` installs NFD and per-manufacturer Device Manager DaemonSets.
2. **Device discovery** — the Device Manager detects accelerators and writes `acceleratable.feature.gpustack.ai/*` labels.
3. **Capacity profiling** — the worker's `NodeFeatureReconciler` stamps the `gpustack.ai/managed` marker and `NodeCapacityReconciler` derives `general.`/`acceleratable.` per-card capacity labels (CPU cores + the four `.sliced.*` accelerator capacities). The general(CPU) key defaults to `generic` and does not encode os/arch.
4. **Queue construction & admission** — worker controllers materialize the labels into Kueue `ResourceFlavor` → `ClusterQueue` (one isolated queue per pool, no Cohort) plus a materialized `InstanceType` CRD, fronted by a per-namespace `LocalQueue` and gated per-card by a `gpustack-node-devices` AdmissionCheck.

`pkg/nodefeature` is the heart of the label algebra (construct/extract of node keys, flavors,
queues, credits). Full stage-by-stage detail and a worked example live in
[architecture.md](../../../docs/architecture.md).

## Key directories

```
cmd/gpustack-operator/            single binary entrypoint (3 cobra subcommands)
pkg/
  worker/                         control-plane process (worker subcommand)
    worker.go                     Prepare → Start lifecycle (startup ordering)
    controllers/worker/           the scheduling-chain reconcilers
    extensionapis/                aggregated extension-API handlers (Instance, Devices, InstanceType, …)
    webhooks/worker/              admission webhooks (generated + hand-written)
    kuberess/                     installs NFD / Kueue / Device-Manager / CSI apps
  workergateway/                  worker-gateway subcommand (upstream aggregation)
  devicemanager/                  device-manager subcommand (per-node DaemonSet)
    detector/<mfr>/               per-manufacturer accelerator detection
    allocator/<mfr>/              per-manufacturer device-plugin allocation
  nodefeature/                    label algebra (node keys, flavors, queues, credits)
  extensionapi/                   generic aggregated-apiserver storage plumbing
api/
  v1/                             gpustack.ai/v1 extension API (settings, status)
  worker/v1/                      worker.gpustack.ai/v1 extension API
  worker/v1alpha1/                worker.gpustack.ai/v1alpha1 CRDs (Instance, Devices, InstanceType)
binding/<runtime>/                generated CGO bindings (nvml, rsmi, cndev, dcmi, …)
gen/
  api/generator/                  custom code generators (apireg-gen, crd-gen, webhook-gen)
  binding/<runtime>/config.yaml   c-for-go config per GPU runtime
hack/                             build/lint/test/deps/generate scripts behind the Makefile
staging/                          patched k8s modules (managed by make deps)
```

Manufacturers: nvidia, amd, ascend, cambricon, hygon, iluvatar, metax, mthreads, thead.

Reconcilers under `controllers/worker/` are unit-tested with the controller-runtime fake client
(`sigs.k8s.io/controller-runtime/pkg/client/fake`), registering the same field indexers the
controller uses via `WithIndex` — see the `*_test.go` beside each reconciler.

## Naming conventions

- **Kueue object names**: `gpustack-${key}-${os}-${arch}-${count}{c|d}` for a `ResourceFlavor` (`c` = CPU cores, `d` = devices); `gpustack-${key}-${os}-${arch}` for the `ClusterQueue` / `InstanceType` — single dash, CPU and device pools split (not composite), os/arch in full.
- **LocalQueue names**: `gpustack-fnv64-<fnv64a-hash>` — always 31 chars (the full ClusterQueue name goes in the `schedule.gpustack.ai/cluster-queue` annotation).
- **Label domains**: `feature.gpustack.ai/` (CPU/PCI facts), `acceleratable.feature.gpustack.ai/` (device models + `.sliced.*` capacities), `general.feature.gpustack.ai/` (CPU-only capacity), `credits.gpustack.ai/<mfr>` (Kueue quota resource), `schedule.gpustack.ai/` + `note.gpustack.ai/` (long names / unit-spec annotations); `gpustack.ai/managed` and `gpustack.ai/controlled` mark node onboarding and queue teardown.
- **63-char rule**: Kubernetes label *values* cap at 63 chars — names that exceed it live in annotations, not labels. Always check when generating a name that flows into a label value.
- **Build-constrained files**: `_linux.go` / `_other.go` split platform-specific code.
- **Generated files**: anything matching `zz_generated.*`, `generated.pb.go`, `generated.proto` is generated — never hand-edit; edit the source types and run the `gpustack-operator-generate` skill.

## Going deeper

- Build / lint / test / codegen / vendored deps → [development.md](../../../docs/development.md)
- Scheduling chain, label tables, worked example → [architecture.md](../../../docs/architecture.md)
- Settings & `GPUSTACK_*` configuration knobs → [settings.md](../../../docs/settings.md)
