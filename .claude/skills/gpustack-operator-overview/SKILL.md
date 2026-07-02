---
name: gpustack-operator-overview
description: "Use when you need a guided tour of the GPUStack Operator codebase — its architecture, where things live (key directories), and naming conventions. Examples: \"Give me an overview of this repo\", \"How is this project structured?\", \"Where does X live?\", onboarding to the scheduling chain."
model: haiku
---

# GPUStack Operator — Code Overview

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Architecture in brief

A single `gpustack-operator` binary with three subcommands. The scheduling chain is built in four
stages:

1. **Bootstrap** — the `worker` installs NFD and per-manufacturer Device Manager DaemonSets.
2. **Device discovery** — the Device Manager detects accelerators and writes `acceleratable.feature.gpustack.ai/*` labels.
3. **Capacity profiling** — the worker's `NodeFeatureReconciler` derives `general.`/`acceleratable.` capacity & profile labels, keyed by the node's CPU identity.
4. **Queue construction** — four worker controllers materialize the labels into Kueue `ResourceFlavor` → `ClusterQueue` → `Cohort` / `LocalQueue` objects.

`pkg/nodefeature` is the heart of the label algebra (construct/extract of node keys, flavors,
queues, cohorts). Full stage-by-stage detail and a worked example live in
[architecture.md](../../../docs/architecture.md).

## Key directories

```
cmd/gpustack-operator/            single binary entrypoint (3 cobra subcommands)
pkg/
  worker/                         control-plane process (worker subcommand)
    worker.go                     Prepare → Start lifecycle (startup ordering)
    controllers/worker/           the 4 scheduling-chain reconcilers
    extensionapis/                aggregated extension-API handlers (Instance, Devices, …)
    webhooks/worker/              admission webhooks (generated + hand-written)
    kuberess/                     installs NFD / Kueue / Device-Manager / CSI apps
  workergateway/                  worker-gateway subcommand (upstream aggregation)
  devicemanager/                  device-manager subcommand (per-node DaemonSet)
    detector/<mfr>/               per-manufacturer accelerator detection
    allocator/<mfr>/              per-manufacturer device-plugin allocation
  nodefeature/                    label algebra (node keys, flavors, queues, cohorts)
  extensionapi/                   generic aggregated-apiserver storage plumbing
api/
  v1/                             gpustack.ai/v1 extension API (settings, status)
  worker/v1/                      worker.gpustack.ai/v1 extension API
  worker/v1alpha1/                worker.gpustack.ai/v1alpha1 CRDs (Instance, Devices)
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

- **Kueue object names**: `gpustack--${gKey}-…[--${aKey}-…]` — general(CPU) segment first, then the device segment, joined by `--`.
- **LocalQueue names**: `gpustack-fnv64-<fnv64a-hash>` — always 31 chars (the full ClusterQueue name goes in the `schedule.gpustack.ai/cluster-queue` annotation).
- **Label domains**: `feature.gpustack.ai/` (CPU/PCI facts), `acceleratable.feature.gpustack.ai/` (device models), `general.feature.gpustack.ai/` (CPU-only capacity), `schedule.gpustack.ai/` (long names as annotations).
- **63-char rule**: Kubernetes label *values* cap at 63 chars — names that exceed it live in annotations, not labels. Always check when generating a name that flows into a label value.
- **Build-constrained files**: `_linux.go` / `_other.go` split platform-specific code.
- **Generated files**: anything matching `zz_generated.*`, `generated.pb.go`, `generated.proto` is generated — never hand-edit; edit the source types and run the `gpustack-operator-generate` skill.

## Going deeper

- Build / lint / test / codegen / vendored deps → [development.md](../../../docs/development.md)
- Scheduling chain, label tables, worked example → [architecture.md](../../../docs/architecture.md)
- Settings & `GPUSTACK_*` configuration knobs → [settings.md](../../../docs/settings.md)
