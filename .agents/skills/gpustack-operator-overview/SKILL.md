---
name: gpustack-operator-overview
description: "A guided tour of the GPUStack Operator codebase — its architecture, where things live, and its naming conventions. Use when onboarding, or when asking where something lives."
---

# GPUStack Operator — Code Overview

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Architecture in brief

The `gpustack-operator` binary provides these subcommands: `worker` ([pkg/worker](../../../pkg/worker/),
the control plane), `worker-gateway` ([pkg/workergateway](../../../pkg/workergateway/), the fleet
view), `device-manager` ([pkg/devicemanager](../../../pkg/devicemanager/), one DaemonSet per
manufacturer) and `model-manager` ([pkg/modelmanager](../../../pkg/modelmanager/), a per-node CSI
plugin that delivers Hugging Face weights), and `model-transfer` ([pkg/modeltransfer](../../../pkg/modeltransfer/),
the remote weight source broker). The scheduling chain proceeds as follows: NFD labels the nodes; the
Device Manager detects accelerators and network interfaces into a per-node `Devices` ledger; the
worker derives per-card capacity labels ([pkg/nodefeature](../../../pkg/nodefeature/) is the label
algebra); worker controllers materialize them into Kueue `ResourceFlavor`, `ClusterQueue` (one
isolated queue per pool), `LocalQueue`, `AcceleratorType` and `InstanceType` CRDs, gated per card by an
AdmissionCheck. Vendor runtime bindings under [binding/](../../../binding/) are generated from
[gen/binding/](../../../gen/binding/). Hand-written slicing preload libraries live in
[csrc/](../../../csrc/). The serving chain lives beside the device chain under the same
`pkg/worker/controllers/worker/` directory, grouped by filename prefix: `model_deployment_*` (render,
rollout, retirement, drain, router, elastic resize, scaler, KV events, runtime control), `kv_cache_*`,
`model_*` (artifact, store, prefetch, transfer), `node_*`, `topology_source_*` and `instance_*`. Their
supporting packages are `pkg/worker/scaling` (capacity priors and the planner), `pkg/worker/kvcache`
(Mooncake rendering and injection), `pkg/worker/runtimecontrol` and `pkg/worker/elasticengine`
(online parameter tuning and Elastic EP), and the staged [engine-sentinel/python](../../../engine-sentinel/python/)
package. [architecture.md](../../../docs/getting-started/architecture.md) is the
one-page version: the scheduling chain, the life of a sliced-GPU request, and the vocabulary.

## Routing

Load [the module map](../../../docs/README.md#module-map) and read the entry that owns the task;
each entry lists its guides, owning specs, code entry points, related modules and skills.
Capability entries: [Heterogeneous Devices](../../../docs/README.md#heterogeneous-devices),
[RDMA Networking](../../../docs/README.md#rdma-networking),
[Topology](../../../docs/README.md#topology),
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
- **Kueue object names**: a `ResourceFlavor` is `gpustack--${gKey}--${aKey}-${os}-${arch}-${count}d`
  when accelerated and `gpustack--${gKey}-${os}-${arch}-${count}c` when CPU-only. A canonical pool
  `ClusterQueue` is `gpustack--${aKey}-${os}-${arch}`, or `gpustack--${gKey}--${aKey}-${os}-${arch}`
  when topology-aware with a non-generic CPU group; a CPU pool uses `gpustack--${gKey}-${os}-${arch}`
  when aware and `gpustack--generic-${os}-${arch}` otherwise. `CanonicalPoolQueueName` in
  `pkg/nodefeature/helper.go` is the single source.
  An `AcceleratorType` and all `InstanceType`s on a pool share that queue.
- **LocalQueue names**: `gpustack-fnv64-<fnv64a-hash>` — always 31 chars (the full ClusterQueue
  name goes in the `schedule.gpustack.ai/queue` annotation).
- **Label domains**: `feature.gpustack.ai/` (CPU/PCI facts), `acceleratable.feature.gpustack.ai/`
  (device models + `.sliced.*` / `.partitioned.*` capacities), `general.feature.gpustack.ai/`
  (CPU-only capacity), `credits.gpustack.ai/<mfr>` (Kueue quota resource), `schedule.gpustack.ai/`
  + `note.gpustack.ai/` (long names / unit-spec annotations), `topology.gpustack.ai/` (region,
  zone, rack, profile), `device.gpustack.ai/` (extended-resource prefix and allocation
  annotations), and per-controller families `modeldeployment.gpustack.ai/`,
  `worker.gpustack.ai/`, `kvcache.gpustack.ai/`, `model.csi.gpustack.ai/`.
  `gpustack.ai/managed=true` is the node onboarding mark. `gpustack.ai/controlled` is the finalizer
  `systemmeta.Lock` adds to owned resources (pool kinds, `Instance`s, `ModelDeployment`s, KV cache
  kinds) while the system holds them; it gates each one's teardown, not only queues.
- **63-char rule**: Kubernetes label *values* cap at 63 chars — names that exceed it live in
  annotations, not labels. Always check when generating a name that flows into a label value.
- **Build-constrained files**: `_linux.go` / `_other.go` split platform-specific code.
- **Generated files**: anything matching `zz_generated.*`, `generated.pb.go`, `generated.proto` is
  generated — never hand-edit; edit the source types and run the `gpustack-operator-generate` skill.
- Reconcilers under `controllers/worker/` are unit-tested with the controller-runtime fake client
  (`sigs.k8s.io/controller-runtime/pkg/client/fake`), registering the same field indexers the
  controller uses via `WithIndex` — see the `*_test.go` beside each reconciler.
