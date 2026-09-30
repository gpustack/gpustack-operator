---
name: gpustack-operator-overview
description: "A guided tour of the GPUStack Operator codebase — its architecture, where things live, and its naming conventions. Use when onboarding, or when asking where something lives."
---

# GPUStack Operator — Code Overview

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Architecture in brief

One `gpustack-operator` binary, four subcommands: `worker` ([pkg/worker](../../../pkg/worker/),
the control plane), `worker-gateway` ([pkg/workergateway](../../../pkg/workergateway/), the fleet
view), `device-manager` ([pkg/devicemanager](../../../pkg/devicemanager/), one DaemonSet per
manufacturer) and `model-manager` ([pkg/modelmanager](../../../pkg/modelmanager/), a per-node CSI
plugin that delivers Hugging Face weights). The chain has four stages: NFD labels the nodes; the
Device Manager detects accelerators and network interfaces into a per-node `Devices` ledger; the
worker derives per-card capacity labels ([pkg/nodefeature](../../../pkg/nodefeature/) is the label
algebra); worker controllers materialize them into Kueue `ResourceFlavor`, `ClusterQueue` (one
isolated queue per pool), `LocalQueue` and an `InstanceType` CRD, gated per card by an
AdmissionCheck. Vendor runtime bindings under [binding/](../../../binding/) are generated from
[gen/binding/](../../../gen/binding/). Hand-written slicing preload libraries live in
[csrc/](../../../csrc/). [architecture.md](../../../docs/getting-started/architecture.md) is the
one-page version: the four stages, the life of a sliced-GPU request, and the vocabulary.

## Routing

Load [the module map](../../../docs/README.md#module-map) and read the entry that owns the task;
each entry lists its guides, owning specs, code entry points, related modules and skills.
Capability entries: [Devices Manager](../../../docs/README.md#devices-manager),
[RDMA Manager](../../../docs/README.md#rdma-manager),
[Topology Aware](../../../docs/README.md#topology-aware),
[KV Cache](../../../docs/README.md#kv-cache),
[Model Delivery](../../../docs/README.md#model-delivery),
[Model Deployment](../../../docs/README.md#model-deployment) and
[GPU Instances](../../../docs/README.md#gpu-instances). Shared task entries:
[API Changes](../../../docs/README.md#api-changes),
[Installation](../../../docs/README.md#installation) and
[Development](../../../docs/README.md#development).

Then load only the deep page the task needs, from the entry's Guides field. For code changes, the
build, lint, test and codegen commands are in
[development.md](../../../docs/contribute/development.md); writing or updating any documentation
page is the `gpustack-operator-docs` skill. Every page is listed in
[docs/README.md](../../../docs/README.md).

## Contributor invariants

- Manufacturers: nvidia, amd, ascend, cambricon, hygon, iluvatar, metax, mthreads, thead.
- **Kueue object names**: `gpustack-${key}-${os}-${arch}-${count}{c|d}` for a `ResourceFlavor`
  (`c` = CPU cores, `d` = devices); `gpustack-${key}-${os}-${arch}` for the `ClusterQueue` /
  `InstanceType` — single dash, CPU and device pools split (not composite), os/arch in full.
- **LocalQueue names**: `gpustack-fnv64-<fnv64a-hash>` — always 31 chars (the full ClusterQueue
  name goes in the `schedule.gpustack.ai/queue` annotation).
- **Label domains**: `feature.gpustack.ai/` (CPU/PCI facts), `acceleratable.feature.gpustack.ai/`
  (device models + `.sliced.*` / `.partitioned.*` capacities), `general.feature.gpustack.ai/`
  (CPU-only capacity), `credits.gpustack.ai/<mfr>` (Kueue quota resource), `schedule.gpustack.ai/`
  + `note.gpustack.ai/` (long names / unit-spec annotations); `gpustack.ai/managed` and
  `gpustack.ai/controlled` mark node onboarding and queue teardown.
- **63-char rule**: Kubernetes label *values* cap at 63 chars — names that exceed it live in
  annotations, not labels. Always check when generating a name that flows into a label value.
- **Build-constrained files**: `_linux.go` / `_other.go` split platform-specific code.
- **Generated files**: anything matching `zz_generated.*`, `generated.pb.go`, `generated.proto` is
  generated — never hand-edit; edit the source types and run the `gpustack-operator-generate` skill.
- Reconcilers under `controllers/worker/` are unit-tested with the controller-runtime fake client
  (`sigs.k8s.io/controller-runtime/pkg/client/fake`), registering the same field indexers the
  controller uses via `WithIndex` — see the `*_test.go` beside each reconciler.
