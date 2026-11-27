---
name: gpustack-operator-overview
description: "A guided tour of the GPUStack Operator codebase — its architecture, where things live, and its naming conventions. Use when onboarding, or when asking where something lives."
---

# GPUStack Operator — Code Overview

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Architecture in brief

The `gpustack-operator` binary has these subcommands: `worker` (control plane, [pkg/worker](../../../pkg/worker/)),
`device-manager` (one DaemonSet per manufacturer, [pkg/devicemanager](../../../pkg/devicemanager/)),
`model-manager` (per-node CSI plugin for model weights and kernels, [pkg/modelmanager](../../../pkg/modelmanager/)),
`model-transfer` (remote weight source broker, [pkg/modeltransfer](../../../pkg/modeltransfer/)),
`model-forger` (hidden `ForgeSandbox` helper subcommands, [pkg/modelforger](../../../pkg/modelforger/)) and
`worker-gateway` (fleet view across clusters, [pkg/workergateway](../../../pkg/workergateway/)).

**Device chain.** NFD labels nodes. The Device Manager records accelerators and NICs in a per-node
`Devices` ledger. The worker derives per-card capacity labels from it
([pkg/nodefeature](../../../pkg/nodefeature/) is the label algebra) and materializes Kueue
`ResourceFlavor`, `ClusterQueue` (one isolated queue per pool) and `LocalQueue`, plus the
`AcceleratorType` and `InstanceType` CRDs, gated per card by an `AdmissionCheck`. Vendor runtime
bindings in [binding/](../../../binding/) are generated from [gen/binding/](../../../gen/binding/);
slicing preload libraries in [csrc/](../../../csrc/) are hand-written.

**Serving chain.** The same controller directory, `pkg/worker/controllers/worker/`, groups by
filename prefix. `node_*` and `topology_source_*` place workloads; `instance_*` runs `Instance`s;
`model_*` delivers weights; `kv_cache_*` shares cache. `model_deployment_*` serves models: render,
rollout, router, scaler, forger, elastic resize, runtime control. Supporting packages: `pkg/modelforger`,
`pkg/modelkernelkey`, `pkg/worker/kvcache`,
`pkg/worker/scaling`, `pkg/worker/runtimecontrol`, `pkg/worker/elasticengine`, and
[engine-sentinel/python](../../../engine-sentinel/python/), staged into engine `Pod`s.

[architecture.md](../../../docs/getting-started/architecture.md) is the one-page version: the
scheduling chain, the life of a sliced-GPU request, and the vocabulary.

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
- **Kueue object names**: `ResourceFlavor` is `gpustack--${gKey}[--${aKey}]-${os}-${arch}-${count}`
  with suffix `c` (CPU) or `d` (accelerated); the `--` separator is deliberate because the keys
  contain single dashes. Never hand-derive a pool queue name: `formatNodeFlavorName` and
  `CanonicalPoolQueueName` in `pkg/nodefeature/helper.go` are the single source. An
  `AcceleratorType` and its `InstanceType`s share one pool `ClusterQueue`.
- **LocalQueue names**: `gpustack-fnv64-<fnv64a-hash>` — always 31 chars (the full ClusterQueue
  name goes in the `schedule.gpustack.ai/queue` annotation).
- **Label domains**: `feature.gpustack.ai/` (CPU/PCI facts), `acceleratable.feature.gpustack.ai/`
  (device models + `.sliced.*` / `.partitioned.*` capacities), `general.feature.gpustack.ai/`
  (CPU-only capacity), `credits.gpustack.ai/<mfr>` (Kueue quota resource), `topology.gpustack.ai/`
  (region/zone/rack), `device.gpustack.ai/` (extended resources). `schedule.gpustack.ai/` and
  `note.gpustack.ai/` carry long names and unit specs as annotations; controllers add their own
  `<kind>.gpustack.ai/` families. `gpustack.ai/managed=true` marks an onboarded node;
  `gpustack.ai/controlled` is the `systemmeta.Lock` finalizer on owned resources.
- **63-char rule**: Kubernetes label *values* cap at 63 chars — names that exceed it live in
  annotations, not labels. Always check when generating a name that flows into a label value.
- **Build-constrained files**: `_linux.go` / `_other.go` split platform-specific code.
- **Generated files**: anything matching `zz_generated.*`, `generated.pb.go`, `generated.proto` is
  generated — never hand-edit; edit the source types and run the `gpustack-operator-generate` skill.
- Reconcilers under `controllers/worker/` are unit-tested with the controller-runtime fake client
  (`sigs.k8s.io/controller-runtime/pkg/client/fake`), registering the same field indexers the
  controller uses via `WithIndex` — see the `*_test.go` beside each reconciler.
